// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hubtree

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// A hub that wraps each listing page in an envelope and paginates by query
// parameter needs both hooks. This is the shape ModelScope serves.
func TestWalkQueryPageWithEnvelopeDecoder(t *testing.T) {
	var requested []string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		page := r.URL.Query().Get("page")
		requested = append(requested, page)
		if page == "2" {
			io.WriteString(w, `{"Data":{"Files":[{"Path":"b.bin","Size":2,"Type":"blob"}]}}`)
			return
		}
		io.WriteString(w, `{"Data":{"Files":[{"Path":"a.bin","Size":1,"Type":"blob"}]}}`)
	}))
	defer srv.Close()

	w := &Walker{
		TreeURL: func(prefix string) string { return srv.URL + "/api/v1/models/owner/repo/repo/files" },
		Client:  srv.Client(),
		DecodePage: func(body io.Reader) ([]Node, error) {
			var env struct {
				Data struct {
					Files []Node `json:"Files"`
				} `json:"Data"`
			}
			if err := json.NewDecoder(body).Decode(&env); err != nil {
				return nil, err
			}
			return env.Data.Files, nil
		},
		Pagination: PaginationQueryPage,
		NextPage: func(cur *url.URL, prefix string, lastPageLen int) (string, bool) {
			if lastPageLen == 0 || cur.Query().Get("page") == "2" {
				return "", false
			}
			return "?page=2", true
		},
	}

	var got []Node
	if err := w.Walk(context.Background(), "", func(n Node) error {
		got = append(got, n)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].Path != "a.bin" || got[1].Path != "b.bin" {
		t.Errorf("nodes: %+v", got)
	}
	if len(requested) != 2 || requested[0] == "2" || requested[1] != "2" {
		t.Errorf("pages requested: %v", requested)
	}
}

// A cross-origin pagination target must still be refused, and a page the walk
// already visited must not be re-entered.
func TestQueryPagePaginationRefusesCrossOriginAndCycle(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		io.WriteString(w, `{"Data":{"Files":[{"Path":"a.bin","Size":1,"Type":"blob"}]}}`)
	}))
	defer srv.Close()

	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	defer other.Close()

	w := &Walker{
		TreeURL: func(prefix string) string { return srv.URL + "/api/v1/models/owner/repo/repo/files" },
		Client:  srv.Client(),
		DecodePage: func(body io.Reader) ([]Node, error) {
			var env struct {
				Data struct {
					Files []Node `json:"Files"`
				} `json:"Data"`
			}
			if err := json.NewDecoder(body).Decode(&env); err != nil {
				return nil, err
			}
			return env.Data.Files, nil
		},
		Pagination: PaginationQueryPage,
		NextPage:   func(cur *url.URL, prefix string, lastPageLen int) (string, bool) { return other.URL, true },
	}
	err := w.Walk(context.Background(), "", func(Node) error { return nil })
	if err == nil {
		t.Fatal("cross-origin pagination target accepted")
	}
	if !contains(err.Error(), "cross-origin") {
		t.Errorf("error text changed: %v", err)
	}

	w.NextPage = func(cur *url.URL, prefix string, lastPageLen int) (string, bool) {
		return "?page=1", true // the page the walk already fetched
	}
	err = w.Walk(context.Background(), "", func(Node) error { return nil })
	if err == nil || !contains(err.Error(), "cycle") {
		t.Errorf("cycle not refused: %v", err)
	}
}

func contains(s, sub string) bool {
	return strings.Contains(s, sub)
}
