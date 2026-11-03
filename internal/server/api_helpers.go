// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"

	"github.com/SnailStudioX/model-fetch/pkg/hfdownloader"
)

// maxRequestBodyBytes caps what the server decodes from one request. The
// largest legitimate body here is a settings POST — hub sources, type routes and
// a few long paths — which is kilobytes. Without a cap any client can make the
// server allocate an arbitrarily large JSON document.
const maxRequestBodyBytes = 4 << 20

// bodyLimitMiddleware bounds the request body before any handler reads it, so
// an oversized POST is a 400 from the decode rather than an unbounded read.
func (s *Server) bodyLimitMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Body != nil {
			r.Body = http.MaxBytesReader(w, r.Body, maxRequestBodyBytes)
		}
		next.ServeHTTP(w, r)
	})
}

// sameProxy reports whether two proxy configurations would build the same
// transport. ProxyConfig holds only strings and booleans, so comparing values is
// exact and keeps a proxy password out of any formatted key.
func sameProxy(a, b *hfdownloader.ProxyConfig) bool {
	if a == nil || b == nil {
		return a == b
	}
	return *a == *b
}

// hubClient returns a client for one API-side hub request while reusing the
// transport built for the current proxy configuration. Building a client per
// request — which is what hfdownloader.BuildHTTPClient does, and what every
// handler used to do — gives each search, analyze, README and avatar lookup its
// own connection pool: a fresh TCP+TLS handshake every time, and an idle pool
// left behind. The returned value is a copy so each caller sets its own Timeout
// without affecting the others; the copy shares the cached Transport.
func (s *Server) hubClient(cfg Config) (*http.Client, error) {
	s.hubClientMu.Lock()
	defer s.hubClientMu.Unlock()

	if s.hubClientCache == nil || !sameProxy(s.hubProxy, cfg.Proxy) {
		if s.hubClientCache != nil {
			if transport, ok := s.hubClientCache.Transport.(interface{ CloseIdleConnections() }); ok {
				transport.CloseIdleConnections()
			}
		}
		client, err := hfdownloader.BuildHTTPClient(cfg.Proxy)
		if err != nil {
			return nil, err
		}
		// Hold a copy of the settings, not the caller's pointer: a Config is
		// copied by value but shares its ProxyConfig, so an in-place change would
		// otherwise compare equal to the cached pointer while meaning something
		// else.
		if cfg.Proxy != nil {
			proxyCopy := *cfg.Proxy
			s.hubProxy = &proxyCopy
		} else {
			s.hubProxy = nil
		}
		s.hubClientCache = client
	}
	clientCopy := *s.hubClientCache
	return &clientCopy, nil
}

func writeJSON(w http.ResponseWriter, status int, data any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(data)
}

func readJSON(r *http.Request, dst any) error {
	return json.NewDecoder(r.Body).Decode(dst)
}

func writeError(w http.ResponseWriter, status int, message, details string) {
	writeAPIError(w, status, errorCodeFromStatus(status), message, details)
}

func writeAPIError(w http.ResponseWriter, status int, code, message, details string) {
	writeJSON(w, status, ErrorResponse{
		OK:      false,
		Error:   message,
		Details: details,
		ErrorDetail: &APIErrorDetail{
			Code:    code,
			Message: message,
			Details: details,
		},
	})
}

func errorCodeFromStatus(status int) string {
	switch status {
	case http.StatusBadRequest:
		return "bad_request"
	case http.StatusUnauthorized:
		return "unauthorized"
	case http.StatusForbidden:
		return "forbidden"
	case http.StatusNotFound:
		return "not_found"
	case http.StatusConflict:
		return "conflict"
	case http.StatusBadGateway:
		return "bad_gateway"
	default:
		if status >= 500 {
			return "server_error"
		}
		return "api_error"
	}
}
