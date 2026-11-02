// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import "testing"

// TestNormalizeVerifyMode pins the accepted spellings of the verification mode.
// hfdownloader compares Settings.Verify by exact match, so a value it does not
// implement silently turns every post-download check off; both the config-file
// merge and POST /api/settings reject it instead. "etag" is listed here because
// it appears in the package documentation but has no implementation behind it.
func TestNormalizeVerifyMode(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want string
		ok   bool
	}{
		{"", "", true},
		{"none", "none", true},
		{"size", "size", true},
		{"sha256", "sha256", true},
		{" SHA256 ", "sha256", true},
		{"Size", "size", true},
		{"SHA-256", "", false},
		{"false", "", false},
		{"etag", "", false},
	} {
		got, ok := normalizeVerifyMode(tc.in)
		if got != tc.want || ok != tc.ok {
			t.Errorf("normalizeVerifyMode(%q) = %q,%v; want %q,%v", tc.in, got, ok, tc.want, tc.ok)
		}
	}
}
