// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import "testing"

// TestClaimStagingPathSeparatesOverlappingWriters pins the staging claim. A
// staging file is named after the blob SHA, so a retry whose previous run has
// not finished unwinding, or a second job for the same repository, used to open
// the same file and each append at the offset its own cursor reported — the
// payload ends up shifted, which only a hash check can see. The first claimer
// must keep the natural name (that is how a paused run's partial file is
// resumed), and a released name must become available again.
func TestClaimStagingPathSeparatesOverlappingWriters(t *testing.T) {
	const path = "/cache/hub/models--o--r/blobs/tmp-6f2c1a"

	first, releaseFirst := claimStagingPath(path)
	if first != path {
		t.Fatalf("first claimer got %q, want the natural staging name %q", first, path)
	}

	second, releaseSecond := claimStagingPath(path)
	if second == first {
		t.Fatalf("second overlapping claimer got the same file: %q", second)
	}
	if second == "" {
		t.Fatal("second claimer got an empty path")
	}

	third, releaseThird := claimStagingPath(path)
	if third == first || third == second {
		t.Fatalf("third overlapping claimer collided: %q", third)
	}

	releaseSecond()
	if again, releaseAgain := claimStagingPath(path); again == first || again == third {
		t.Fatalf("a released name was not offered again: %q", again)
	} else {
		releaseThird()
		releaseAgain()
	}

	releaseFirst()
	if next, releaseNext := claimStagingPath(path); next != path {
		t.Fatalf("after every claim was released the natural name should be reusable, got %q", next)
	} else {
		releaseNext()
	}
}
