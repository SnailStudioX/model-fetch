// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"bytes"
	"context"
	"io"
	"log"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/SnailStudioX/model-fetch/internal/hubsource"
)

// Search rows show a logo per author, not per repository: neither source's search
// API reports one, so one lookup per unique author covers every row that author
// owns. The cache is server-wide because the same authors recur across searches;
// only successes are stored, so a transient failure is retried on the next search
// instead of being remembered as absent. Rows with no logo keep the UI's initials
// placeholder.

// maxAvatarAuthors bounds the upstream requests one search makes. Cached authors
// cost nothing, so only new ones fetch; the cap keeps a broad query from turning
// one search into a fan-out.
const maxAvatarAuthors = 25

// avatarConcurrency is how many uncached authors fetch at once.
const avatarConcurrency = 4

// avatarTimeout bounds one author's lookup. A slow mirror must not hold the whole
// search response open.
const avatarTimeout = 10 * time.Second

// avatarCacheKey pins one author's resolved logo to the source and endpoint it was
// resolved against, so switching the endpoint does not serve a URL rewritten for
// the old host.
func avatarCacheKey(src hubsource.Source, author string) string {
	return src.ID() + "|" + src.Base() + "|" + author
}

// rowAuthor is the author a row belongs to. The search APIs do not always report
// one — Hugging Face omits it unless full=true — so the repository id prefix is
// used, which is the same value the UI shows for the row.
func rowAuthor(h hubsource.Hit) string {
	if h.Author != "" {
		return h.Author
	}
	if i := strings.Index(h.ID, "/"); i > 0 {
		return h.ID[:i]
	}
	return ""
}

// resolveAvatars returns the logo URL for each unique author in hits.
func (s *Server) resolveAvatars(ctx context.Context, cfg Config, src hubsource.Source, hits []hubsource.Hit) map[string]string {
	samples := make(map[string]string) // author -> one repository, for detail-based sources
	for _, h := range hits {
		author := rowAuthor(h)
		if author == "" {
			continue
		}
		if _, ok := samples[author]; !ok {
			samples[author] = h.ID
		}
	}
	if len(samples) == 0 {
		return nil
	}

	s.avatarMu.Lock()
	if s.avatarCache == nil {
		s.avatarCache = make(map[string]string)
	}
	out := make(map[string]string, len(samples))
	var missing []string
	for author := range samples {
		if got, ok := s.avatarCache[avatarCacheKey(src, author)]; ok {
			out[author] = got
			continue
		}
		missing = append(missing, author)
	}
	s.avatarMu.Unlock()

	if len(missing) == 0 {
		return out
	}

	client, err := s.hubClient(cfg)
	if err != nil {
		// A broken proxy must not fail the search; the rows fall back to initials.
		log.Printf("avatars: invalid proxy config: %v", err)
		return out
	}
	client.Timeout = avatarTimeout

	// Sorted so a capped run is deterministic rather than map-order dependent.
	sort.Strings(missing)
	if len(missing) > maxAvatarAuthors {
		missing = missing[:maxAvatarAuthors]
	}

	var wg sync.WaitGroup
	sem := make(chan struct{}, avatarConcurrency)
	for _, author := range missing {
		wg.Add(1)
		go func(author string) {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()

			got := fetchAvatar(ctx, cfg, src, client, author, samples[author])
			if got == "" {
				return
			}
			s.avatarMu.Lock()
			defer s.avatarMu.Unlock()
			s.avatarCache[avatarCacheKey(src, author)] = got
			out[author] = got
		}(author)
	}
	wg.Wait()
	return out
}

// fetchAvatar tries each candidate URL in the source's attempt order and returns
// the first logo the source can parse. Every failure is silent at this level: the
// caller treats an empty result as "no logo", not as an error, because a missing
// avatar is normal for accounts that never set one.
func fetchAvatar(ctx context.Context, cfg Config, src hubsource.Source, client *http.Client, author, sampleRepo string) string {
	for _, u := range src.AvatarURLs(author, sampleRepo) {
		if ctx.Err() != nil {
			return ""
		}
		req, err := http.NewRequestWithContext(ctx, http.MethodGet, u, nil)
		if err != nil {
			continue
		}
		if cfg.Token != "" {
			req.Header.Set("Authorization", "Bearer "+cfg.Token)
		}
		req.Header.Set("User-Agent", "modelfetch/1")
		resp, err := client.Do(req)
		if err != nil {
			continue
		}
		body, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
		resp.Body.Close()
		if err != nil || resp.StatusCode != http.StatusOK {
			continue
		}
		got, err := src.ParseAvatar(bytes.NewReader(body))
		if err != nil {
			continue
		}
		if got != "" {
			return got
		}
	}
	return ""
}
