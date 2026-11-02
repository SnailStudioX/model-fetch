// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hubsource

import "testing"

// TestRevisionFallback pins which stored revisions may be re-mapped onto this
// hub's default branch. A job created on one hub carries that hub's default
// branch name, which the newly selected hub has never had; a revision the user
// chose must never be replaced by a guess.
func TestRevisionFallback(t *testing.T) {
	hf := NewHF("")
	ms := NewModelScope("")
	cases := []struct {
		name     string
		src      Source
		revision string
		want     string
		ok       bool
	}{
		{"modelscope default retried on hf", hf, "master", "main", true},
		{"hf default retried on modelscope", ms, "main", "master", true},
		{"already this hub default", hf, "main", "", false},
		{"already this hub default (modelscope)", ms, "master", "", false},
		{"user branch", hf, "dev", "", false},
		{"user tag", ms, "v1.2.3", "", false},
		{"commit sha", hf, "8f3c1e2d4a5b6c7d8e9f0a1b2c3d4e5f6a7b8c9d", "", false},
		{"pull ref", hf, "refs/pr/3", "", false},
		{"empty", hf, "", "", false},
		{"case-insensitive default", hf, "MASTER", "main", true},
		{"nil source", nil, "master", "", false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := RevisionFallback(tc.src, tc.revision)
			if ok != tc.ok || got != tc.want {
				t.Errorf("RevisionFallback(%v, %q) = (%q, %v), want (%q, %v)",
					tc.src, tc.revision, got, ok, tc.want, tc.ok)
			}
		})
	}
}

func TestHubDefaultRevisions(t *testing.T) {
	revs := HubDefaultRevisions()
	if len(revs) != 2 {
		t.Fatalf("HubDefaultRevisions() = %v, want the two registered defaults", revs)
	}
	for _, want := range []string{"main", "master"} {
		found := false
		for _, rev := range revs {
			if rev == want {
				found = true
			}
		}
		if !found {
			t.Errorf("HubDefaultRevisions() = %v, missing %q", revs, want)
		}
	}
	for _, rev := range []string{"main", "master", "Main"} {
		if !IsHubDefaultRevision(rev) {
			t.Errorf("IsHubDefaultRevision(%q) = false, want true", rev)
		}
	}
	for _, rev := range []string{"", "  ", "dev", "v1.0", "refs/pr/1"} {
		if IsHubDefaultRevision(rev) {
			t.Errorf("IsHubDefaultRevision(%q) = true, want false", rev)
		}
	}
}
