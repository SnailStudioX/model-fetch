// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"sync"
	"testing"
	"time"

	"github.com/SnailStudioX/model-fetch/pkg/hfdownloader"
)

// Every API-side hub request used to call hfdownloader.BuildHTTPClient, which
// builds a new transport each time: a separate connection pool per search,
// analyze, README and avatar lookup, so each one paid its own TCP+TLS handshake
// and left an idle pool behind. The server keeps one client per proxy
// configuration and hands out copies, because callers set their own Timeout.

func TestHubClientReusesTransportWhileProxyIsUnchanged(t *testing.T) {
	s := New(DefaultConfig())
	cfg := Config{Proxy: &hfdownloader.ProxyConfig{NoEnvProxy: true}}

	first, err := s.hubClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.hubClient(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if first.Transport != second.Transport {
		t.Fatal("two clients with unchanged proxy settings did not share a transport")
	}

	// The copy is what keeps one handler's timeout from becoming another's.
	first.Timeout = time.Millisecond
	if second.Timeout == time.Millisecond {
		t.Fatal("the client copy shares a Timeout with the caller's client")
	}

	// An equal but distinct value is still the same configuration: a settings
	// POST decodes a fresh struct every save.
	other := Config{Proxy: &hfdownloader.ProxyConfig{NoEnvProxy: true, URL: "http://127.0.0.1:9"}}
	third, err := s.hubClient(other)
	if err != nil {
		t.Fatal(err)
	}
	if third.Transport == second.Transport {
		t.Fatal("a changed proxy configuration reused the old transport")
	}
	fourth, err := s.hubClient(other)
	if err != nil {
		t.Fatal(err)
	}
	if fourth.Transport != third.Transport {
		t.Fatal("an equal proxy value rebuilt the transport")
	}
}

// The server holds a copy of the proxy settings rather than the caller's pointer:
// Config is copied by value but shares its *ProxyConfig, so an in-place change
// would otherwise compare equal to the cached pointer while meaning something
// else.
func TestHubClientNoticesInPlaceProxyChange(t *testing.T) {
	s := New(DefaultConfig())
	proxy := &hfdownloader.ProxyConfig{NoEnvProxy: true}
	before, err := s.hubClient(Config{Proxy: proxy})
	if err != nil {
		t.Fatal(err)
	}

	*proxy = hfdownloader.ProxyConfig{NoEnvProxy: true, URL: "http://127.0.0.1:9"}
	after, err := s.hubClient(Config{Proxy: proxy})
	if err != nil {
		t.Fatal(err)
	}
	if after.Transport == before.Transport {
		t.Fatal("an in-place proxy change kept the transport built for the old settings")
	}
}

func TestHubClientIsSafeForConcurrentRequests(t *testing.T) {
	s := New(DefaultConfig())
	cfg := Config{Proxy: &hfdownloader.ProxyConfig{NoEnvProxy: true}}

	const callers = 32
	var wg sync.WaitGroup
	transports := make(chan http.RoundTripper, callers)
	for range callers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			client, err := s.hubClient(cfg)
			if err != nil {
				t.Error(err)
				return
			}
			transports <- client.Transport
		}()
	}
	wg.Wait()
	close(transports)

	var seen http.RoundTripper
	for got := range transports {
		if seen == nil {
			seen = got
			continue
		}
		if got != seen {
			t.Fatal("concurrent callers got different transports for unchanged settings")
		}
	}
}
