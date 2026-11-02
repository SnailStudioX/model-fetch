// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
)

// DefaultMaxStreamsPerHost is the per-host concurrent-stream budget used when
// SetMaxStreamsPerHost has not been called. It is deliberately small: a hub
// mirror serves one client IP, and opening one range stream per file per job
// multiplies past what a single client can actually use, which is what makes
// mirrors throttle or silently stall the connections.
const DefaultMaxStreamsPerHost = 4

// maxStreamsPerHost holds the configured budget. Zero or negative means "use
// DefaultMaxStreamsPerHost". It is process-wide because the throttle is imposed
// on the client IP, not per job: eight jobs with four connections each still
// look like one very noisy client to the server.
var maxStreamsPerHost atomic.Int64

// SetMaxStreamsPerHost sets the per-host budget for response bodies read at the
// same time by this process. Values below one are clamped to one; zero resets
// to the default. Lowering the limit never interrupts an in-flight stream, it
// only holds back the next one.
func SetMaxStreamsPerHost(n int) {
	if n < 0 {
		n = 0
	}
	maxStreamsPerHost.Store(int64(n))
	// Wake every gate so a raised budget admits queued streams immediately
	// instead of waiting for an unrelated body to close. Lowering the budget
	// runs the same pass; grantLocked stops as soon as the host is back over
	// the new limit, so queued streams simply stay parked.
	streamGatesMu.Lock()
	for _, g := range streamGates {
		g.mu.Lock()
		g.grantLocked()
		g.mu.Unlock()
	}
	streamGatesMu.Unlock()
}

// MaxStreamsPerHost returns the effective per-host budget.
func MaxStreamsPerHost() int {
	n := int(maxStreamsPerHost.Load())
	if n < 1 {
		return DefaultMaxStreamsPerHost
	}
	return n
}

// streamOwnerKey tags requests issued while one file downloads so the gate can
// hand slots out file by file. Without a tag every stream competes as one
// anonymous crowd and the goroutine that happens to wake first wins the next
// slot, which makes each file's live connection count differ from run to run.
type streamOwnerKey struct{}

// withStreamOwner tags ctx with the fairness bucket its requests compete for.
// Callers set it once per file; every retry and every multipart part request
// inherits it through the derived contexts.
func withStreamOwner(ctx context.Context, owner string) context.Context {
	if owner == "" {
		return ctx
	}
	return context.WithValue(ctx, streamOwnerKey{}, owner)
}

// streamOwnerFrom returns the bucket name for ctx, or "" for untagged traffic.
// Untagged streams share one anonymous bucket, so they still fair-share against
// each other and against tagged files.
func streamOwnerFrom(ctx context.Context) string {
	if ctx == nil {
		return ""
	}
	owner, _ := ctx.Value(streamOwnerKey{}).(string)
	return owner
}

// gateWaiter is one parked acquire call. grantLocked closes slot when it
// assigns the next stream to this waiter; granted is the under-lock flag the
// cancellation path uses to tell "never granted" from "granted in the race
// against ctx.Done".
type gateWaiter struct {
	owner    string
	seq      uint64 // arrival order, for stable tie-breaking
	slot     chan struct{}
	canceled bool
	granted  bool
}

// hostStreamGate counts the response bodies currently being read from one host
// and hands free slots out fairly between owners: the owner holding the fewest
// streams wins the next slot, ties going to whoever asked first. With one
// shared budget and several active files this converges to every file holding
// budget/files streams, instead of one file's part goroutines winning every
// wake-up race. A raised budget is re-read by grantLocked, so a settings
// change takes effect for queued streams without restarting the gate.
type hostStreamGate struct {
	mu       sync.Mutex
	inFlight int
	inUse    map[string]int // streams currently held per owner
	waiters  []*gateWaiter
	nextSeq  uint64
}

func newHostStreamGate() *hostStreamGate {
	return &hostStreamGate{inUse: make(map[string]int)}
}

// grantLocked assigns every free slot to the most-fair parked waiter, in order.
// Callers hold mu. It runs after each release and after each budget change, so
// a waiter is only ever parked while the host is at its budget.
func (g *hostStreamGate) grantLocked() {
	// Drop parked-but-canceled waiters first so a cancellation cannot consume a
	// grant that a live request should have.
	for i := 0; i < len(g.waiters); i++ {
		if g.waiters[i].canceled {
			g.waiters = append(g.waiters[:i], g.waiters[i+1:]...)
			i--
		}
	}
	for g.inFlight < MaxStreamsPerHost() && len(g.waiters) > 0 {
		best := 0
		for i := 1; i < len(g.waiters); i++ {
			w, cur := g.waiters[i], g.waiters[best]
			if g.inUse[w.owner] < g.inUse[cur.owner] ||
				(g.inUse[w.owner] == g.inUse[cur.owner] && w.seq < cur.seq) {
				best = i
			}
		}
		w := g.waiters[best]
		g.waiters = append(g.waiters[:best], g.waiters[best+1:]...)
		g.inFlight++
		g.inUse[w.owner]++
		w.granted = true
		close(w.slot)
	}
}

// acquire takes a stream slot, waiting while the host is at its budget, and
// returns with the slot held; the caller must release it once the response body
// is closed. Waiting is bounded by ctx so pause, cancel, and shutdown unblock a
// queued request instead of leaving it parked behind a stalled sibling. owner
// names the fairness bucket (see streamOwnerFrom).
func (g *hostStreamGate) acquire(ctx context.Context, owner string) error {
	if ctx == nil {
		ctx = context.Background()
	}
	g.mu.Lock()
	// A free slot implies no parked waiter: grantLocked drains the queue on
	// every release and budget raise, so taking directly never cuts in front.
	if g.inFlight < MaxStreamsPerHost() {
		g.inFlight++
		g.inUse[owner]++
		g.mu.Unlock()
		return nil
	}
	w := &gateWaiter{owner: owner, seq: g.nextSeq, slot: make(chan struct{})}
	g.nextSeq++
	g.waiters = append(g.waiters, w)
	g.mu.Unlock()

	select {
	case <-w.slot:
		return nil
	case <-ctx.Done():
		g.mu.Lock()
		defer g.mu.Unlock()
		if w.granted {
			// The grant landed in the race between the two select arms. The
			// slot is already counted against this waiter; hand it back and
			// re-run the fairness pass so it is not lost.
			g.inFlight--
			if g.inUse[w.owner]--; g.inUse[w.owner] <= 0 {
				delete(g.inUse, w.owner)
			}
			g.grantLocked()
			return ctx.Err()
		}
		w.canceled = true
		for i, x := range g.waiters {
			if x == w {
				g.waiters = append(g.waiters[:i], g.waiters[i+1:]...)
				break
			}
		}
		return ctx.Err()
	}
}

// release returns a stream slot held by owner and runs the fairness pass so
// the slot goes to the least-served owner rather than to whichever goroutine
// wakes first.
func (g *hostStreamGate) release(owner string) {
	g.mu.Lock()
	g.inFlight--
	if g.inUse[owner]--; g.inUse[owner] <= 0 {
		delete(g.inUse, owner)
	}
	g.grantLocked()
	g.mu.Unlock()
}

// streamGateFor returns the gate that bounds streams to host. Hosts are
// independent, so a slow mirror never holds back a download from a different hub.
func streamGateFor(host string) *hostStreamGate {
	streamGatesMu.Lock()
	defer streamGatesMu.Unlock()
	if g, ok := streamGates[host]; ok {
		return g
	}
	g := newHostStreamGate()
	streamGates[host] = g
	return g
}

var (
	streamGatesMu sync.Mutex
	streamGates   = make(map[string]*hostStreamGate)
)

// streamGatedTransport serializes response-body reads per host on top of an
// inner transport. It is installed on the transfer client only, so job scans,
// search, README rendering, and avatars keep their own latency.
//
// The slot is held for the whole life of the body, not just until the response
// headers arrive, because that is the window in which the server is actually
// serving us. Every download path closes the body on every exit, and the
// release is guarded so a double close cannot free a slot twice.
type streamGatedTransport struct {
	next http.RoundTripper
}

func (t *streamGatedTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	if t.next == nil {
		return nil, errors.New("modelfetch: gated transport has no inner round tripper")
	}
	host := ""
	if req.URL != nil {
		host = req.URL.Host
	}
	// Probes carry no body stream. Gating them would let a saturated download
	// queue block the size lookups and tree scans that start a job.
	if host == "" || req.Method == http.MethodHead {
		return t.next.RoundTrip(req)
	}

	owner := streamOwnerFrom(req.Context())
	g := streamGateFor(host)
	if err := g.acquire(req.Context(), owner); err != nil {
		return nil, err
	}
	resp, err := t.next.RoundTrip(req)
	if err != nil || resp.Body == nil {
		g.release(owner)
		return resp, err
	}
	resp.Body = &gateReleaseBody{ReadCloser: resp.Body, close: sync.OnceFunc(func() { g.release(owner) })}
	return resp, err
}

// gateReleaseBody releases the host stream slot exactly once, on Close.
type gateReleaseBody struct {
	io.ReadCloser
	close func()
}

func (b *gateReleaseBody) Close() error {
	err := b.ReadCloser.Close()
	b.close()
	return err
}
