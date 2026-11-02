// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"
)

// blockingBodyServer serves GET requests that hold the connection open until
// release is closed, and answers HEAD immediately. It records how many response
// bodies are being written at the same time.
type blockingBodyServer struct {
	srv       *httptest.Server
	release   chan struct{}
	perPath   map[string]chan struct{} // release one path's body without freeing the rest
	mu        sync.Mutex
	active    int
	maxActive int
	entered   chan string
	ranges    []string
}

func newBlockingBodyServer(t *testing.T) *blockingBodyServer {
	t.Helper()
	b := &blockingBodyServer{
		release: make(chan struct{}),
		perPath: make(map[string]chan struct{}),
		entered: make(chan string, 16),
	}
	b.srv = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method == http.MethodHead {
			w.Header().Set("Content-Length", "10")
			w.WriteHeader(http.StatusOK)
			return
		}
		b.mu.Lock()
		b.active++
		if b.active > b.maxActive {
			b.maxActive = b.active
		}
		if rng := r.Header.Get("Range"); rng != "" {
			b.ranges = append(b.ranges, rng)
		}
		b.mu.Unlock()
		ch := b.enter(r.URL.Path)
		select {
		case <-ch:
		case <-b.release:
		}
		b.mu.Lock()
		b.active--
		b.mu.Unlock()
		w.Write([]byte("payload"))
	}))
	t.Cleanup(func() {
		b.srv.Close()
	})
	return b
}

// enter announces that the body for path started serving and returns the
// channel that lets it finish, so a test can wait for the exact stream it
// expects to be admitted next and free one stream without freeing the others.
func (b *blockingBodyServer) enter(path string) chan struct{} {
	b.mu.Lock()
	ch, ok := b.perPath[path]
	if !ok {
		ch = make(chan struct{})
		b.perPath[path] = ch
	}
	b.mu.Unlock()
	b.entered <- path
	return ch
}

// releasePath finishes only the body for path. That is how the fairness test
// frees a single slot and then observes which file the gate hands it to.
func (b *blockingBodyServer) releasePath(path string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if ch, ok := b.perPath[path]; ok {
		close(ch)
		delete(b.perPath, path)
	}
}

func (b *blockingBodyServer) max() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.maxActive
}

func (b *blockingBodyServer) activeCount() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.active
}

// gatedClient builds a client whose transport carries the per-host gate, reusing
// the test server's own transport so plain-HTTP test URLs work.
func gatedClient(srv *httptest.Server) *http.Client {
	return &http.Client{Transport: &streamGatedTransport{next: srv.Client().Transport}}
}

// TestStreamGate_CapsConcurrentStreamsPerHost is the regression test for the
// self-inflicted throttle: with N jobs each opening several range streams, the
// process must still read at most the configured number of bodies from one host
// at a time.
func TestStreamGate_CapsConcurrentStreamsPerHost(t *testing.T) {
	defer maxStreamsPerHost.Store(0)
	SetMaxStreamsPerHost(2)

	b := newBlockingBodyServer(t)
	httpc := gatedClient(b.srv)

	var wg sync.WaitGroup
	errCh := make(chan error, 4)
	for i := 0; i < 4; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			resp, err := httpc.Get(b.srv.URL + "/file" + strconv.Itoa(i))
			if err != nil {
				errCh <- err
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}(i)
	}

	// Exactly the budget may enter; the rest must stay queued.
	for i := 0; i < 2; i++ {
		select {
		case <-b.entered:
		case <-time.After(5 * time.Second):
			t.Fatalf("only %d of 4 requests started", i)
		}
	}
	time.Sleep(250 * time.Millisecond)
	if got := b.activeCount(); got != 2 {
		t.Errorf("active bodies = %d, want 2 (gate limit)", got)
	}

	close(b.release)
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("request failed: %v", err)
	}
	if got := b.max(); got > 2 {
		t.Errorf("max concurrent bodies = %d, want <= 2", got)
	}
}

// TestStreamGate_LimitChangeAdmitsQueuedRequests verifies a raised budget wakes
// waiters without restarting the gate, so a settings change affects future
// streams rather than dropping in-flight ones.
func TestStreamGate_LimitChangeAdmitsQueuedRequests(t *testing.T) {
	defer maxStreamsPerHost.Store(0)
	SetMaxStreamsPerHost(1)

	b := newBlockingBodyServer(t)
	httpc := gatedClient(b.srv)

	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		resp, err := httpc.Get(b.srv.URL + "/a")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	go func() {
		defer wg.Done()
		resp, err := httpc.Get(b.srv.URL + "/b")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()

	select {
	case <-b.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first request never started")
	}
	time.Sleep(150 * time.Millisecond)
	if got := b.activeCount(); got != 1 {
		t.Fatalf("active bodies = %d, want 1", got)
	}

	SetMaxStreamsPerHost(2)
	select {
	case <-b.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("raising the limit did not admit the queued request")
	}
	close(b.release)
	wg.Wait()
}

// TestStreamGate_HeadProbesBypassTheGate keeps size lookups and other probes off
// the transfer budget: a saturated download queue must not stall the requests
// that start a job.
func TestStreamGate_HeadProbesBypassTheGate(t *testing.T) {
	defer maxStreamsPerHost.Store(0)
	SetMaxStreamsPerHost(1)

	b := newBlockingBodyServer(t)
	httpc := gatedClient(b.srv)

	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := httpc.Get(b.srv.URL + "/slow")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	select {
	case <-b.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("GET never started")
	}

	headDone := make(chan error, 1)
	go func() {
		resp, err := httpc.Head(b.srv.URL + "/probe")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
		headDone <- err
	}()
	select {
	case err := <-headDone:
		if err != nil {
			t.Fatalf("HEAD failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("HEAD was blocked behind the transfer gate")
	}
	close(b.release)
	<-done
}

// TestStreamGate_QueuedRequestUnblocksOnCancel keeps pause, cancel, and shutdown
// responsive while a request waits for a stream slot.
func TestStreamGate_QueuedRequestUnblocksOnCancel(t *testing.T) {
	defer maxStreamsPerHost.Store(0)
	SetMaxStreamsPerHost(1)

	b := newBlockingBodyServer(t)
	httpc := gatedClient(b.srv)

	go func() {
		resp, err := httpc.Get(b.srv.URL + "/slow")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	select {
	case <-b.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("first GET never started")
	}

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, "GET", b.srv.URL+"/queued", nil)
	if err != nil {
		t.Fatal(err)
	}
	errDone := make(chan error, 1)
	go func() {
		_, err := httpc.Do(req)
		errDone <- err
	}()
	time.Sleep(200 * time.Millisecond)
	cancel()

	select {
	case err := <-errDone:
		if !errors.Is(err, context.Canceled) {
			t.Errorf("queued request error = %v, want context.Canceled", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("queued request stayed parked after cancellation")
	}
	close(b.release)
}

// TestStreamGate_DifferentHostsAreIndependent keeps one slow hub from starving a
// download from another, which is what makes multiple hub sources useful.
func TestStreamGate_DifferentHostsAreIndependent(t *testing.T) {
	defer maxStreamsPerHost.Store(0)
	SetMaxStreamsPerHost(1)

	slow := newBlockingBodyServer(t)
	fast := newBlockingBodyServer(t)

	go func() {
		resp, err := gatedClient(slow.srv).Get(slow.srv.URL + "/slow")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	select {
	case <-slow.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("slow-host GET never started")
	}

	go func() {
		resp, err := gatedClient(fast.srv).Get(fast.srv.URL + "/fast")
		if err == nil {
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()

	select {
	case <-fast.entered:
	case <-time.After(2 * time.Second):
		t.Fatal("a busy host blocked a different host")
	}
	close(slow.release)
	close(fast.release)
}

// gateWaitersFor counts the acquire calls currently parked for host. Tests use
// it to wait until the requests they expect to be queued really are queued, so
// the fairness assertions are deterministic instead of timing-based.
func gateWaitersFor(host string) int {
	g := streamGateFor(host)
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.waiters)
}

func waitForGateWaiters(t *testing.T, host string, want int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if got := gateWaitersFor(host); got == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("parked streams for %s = %d, want %d", host, gateWaitersFor(host), want)
}

// TestStreamGate_FairnessGivesEveryFileItsShare is the regression test for the
// run-to-run variance the user asked about: one shared per-host budget plus a
// broadcast wake-up means each file's live connection count depends on which
// goroutine wins the lock race. Grants must instead go to the owner holding the
// fewest streams, so with two files the stream count alternates until both hold
// the same number.
func TestStreamGate_FairnessGivesEveryFileItsShare(t *testing.T) {
	defer maxStreamsPerHost.Store(0)
	SetMaxStreamsPerHost(2)

	b := newBlockingBodyServer(t)
	defer close(b.release) // always unblock parked handlers, even on a failure path
	httpc := gatedClient(b.srv)
	u, err := url.Parse(b.srv.URL)
	if err != nil {
		t.Fatal(err)
	}

	var wg sync.WaitGroup
	errCh := make(chan error, 5)
	start := func(owner, path string) {
		req, err := http.NewRequestWithContext(withStreamOwner(context.Background(), owner), http.MethodGet, b.srv.URL+path, nil)
		if err != nil {
			t.Fatal(err)
		}
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, err := httpc.Do(req)
			if err != nil {
				errCh <- fmt.Errorf("%s: %w", path, err)
				return
			}
			io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}()
	}
	next := func(want string) {
		t.Helper()
		select {
		case got := <-b.entered:
			if got != want {
				t.Fatalf("gate admitted %s next, want %s", got, want)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("gate never admitted %s", want)
		}
	}

	start("A", "/a1")
	next("/a1")
	start("A", "/a2")
	next("/a2")
	// File A now holds both slots; its third part and both of B's parts queue.
	// Park them one at a time: B's two waiters tie on owner fairness, and the
	// first grant then falls to arrival seq — starting them concurrently would
	// make the expected winner scheduling-dependent.
	start("A", "/a3")
	waitForGateWaiters(t, u.Host, 1)
	start("B", "/b1")
	waitForGateWaiters(t, u.Host, 2)
	start("B", "/b2")
	waitForGateWaiters(t, u.Host, 3)
	if got := b.activeCount(); got != 2 {
		t.Fatalf("active bodies = %d, want 2 (budget)", got)
	}

	// A owns both streams and asked first, yet the freed slot must go to B.
	b.releasePath("/a1")
	next("/b1")
	// With A behind on streams again, its parked part is next, then B's.
	b.releasePath("/a2")
	next("/a3")
	b.releasePath("/b1")
	next("/b2")

	b.releasePath("/a3")
	b.releasePath("/b2")
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Errorf("request failed: %v", err)
	}
	if got := b.max(); got > 2 {
		t.Errorf("max concurrent bodies = %d, want <= 2", got)
	}
}

// TestDownloadSingle_StalledDribbleFailsFast is the regression test for a job
// that stayed "running" for hours at ~0 B/s: a throttled mirror that accepts the
// request, delivers a few bytes, then goes silent must exhaust the retry budget
// instead of earning an unlimited refund from tiny advances.
func TestDownloadSingle_StalledDribbleFailsFast(t *testing.T) {
	tmpDir := t.TempDir()
	dst := filepath.Join(tmpDir, "blobs", "tmp-dribble")
	if err := os.MkdirAll(filepath.Dir(dst), 0o755); err != nil {
		t.Fatal(err)
	}
	const total = int64(1) << 30

	var (
		mu       sync.Mutex
		requests int
	)
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		requests++
		mu.Unlock()
		w.Header().Set("Content-Range", fmt.Sprintf("bytes 0-%d/%d", total-1, total))
		w.Header().Set("Content-Length", strconv.FormatInt(total, 10))
		w.WriteHeader(http.StatusPartialContent)
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		// A few hundred bytes, then silence until the client aborts the attempt.
		// Deliberately below the floor the 1s watchdog window justifies.
		w.Write(make([]byte, 512))
		if f, ok := w.(http.Flusher); ok {
			f.Flush()
		}
		<-r.Context().Done()
	}))
	defer srv.Close()

	cfg := Settings{
		Retries:        2,
		BackoffInitial: "1ms",
		BackoffMax:     "1ms",
		StallTimeout:   "1s",
	}
	it := PlanItem{RelativePath: "dribble.bin", URL: srv.URL + "/dribble.bin", Size: total}

	done := make(chan error, 1)
	go func() {
		done <- downloadSingle(context.Background(), srv.Client(), "", Job{Repo: "o/r"}, cfg, it, dst, func(ProgressEvent) {})
	}()

	var err error
	select {
	case err = <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("a dribbling server kept the download alive")
	}
	if err == nil {
		t.Fatal("expected a stall failure")
	}
	var se *StallError
	if !errors.As(err, &se) {
		t.Errorf("failure = %T %v, want *StallError", err, err)
	}
	mu.Lock()
	n := requests
	mu.Unlock()
	if want := cfg.Retries + 1; n != want {
		t.Errorf("made %d requests; want %d (a dead stall must not refund the budget)", n, want)
	}
}

// TestDeadStallAttemptScalesWithWindow pins the rate-based floor: a short
// watchdog window that still delivered data is progress, a long one that
// delivered almost nothing is a dead stream.
func TestDeadStallAttemptScalesWithWindow(t *testing.T) {
	stall := &StallError{Timeout: 60 * time.Second}
	netErr := errors.New("connection reset by peer")
	cases := []struct {
		name   string
		err    error
		gained int64
		window time.Duration
		want   bool
	}{
		{"short window real progress", &StallError{Timeout: 80 * time.Second / 1000}, 10_000, 80 * time.Millisecond, false},
		{"short window nothing", stall, 0, 80 * time.Millisecond, true},
		{"long window dribble", stall, 19_365, 60 * time.Second, true},
		{"long window burst", stall, 40 << 20, 60 * time.Second, false},
		{"not a stall", netErr, 0, 60 * time.Second, false},
	}
	for _, tc := range cases {
		if got := deadStallAttempt(tc.err, tc.gained, tc.window); got != tc.want {
			t.Errorf("%s: deadStallAttempt = %v, want %v", tc.name, got, tc.want)
		}
	}
}
