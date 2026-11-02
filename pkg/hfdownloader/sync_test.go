// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

// TestCleanEmptyDirsRemovesChildrenFirst pins the traversal order. cleanEmptyDirs
// used to remove directories in place during a pre-order filepath.Walk: removing
// a parent before its children always fails, and the walk never revisits it, so
// after --clean dropped the last orphan symlink, models/<owner>/<name>/ stayed
// behind as an empty directory forever. The collected order is now replayed
// bottom-up, while the walk root and any directory that still holds content are
// left alone.
func TestCleanEmptyDirsRemovesChildrenFirst(t *testing.T) {
	root := t.TempDir()
	deepEmpty := filepath.Join(root, "models", "owner", "name")
	kept := filepath.Join(root, "models", "other")
	if err := os.MkdirAll(deepEmpty, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(kept, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(kept, "hfd.yaml"), []byte("files: []\n"), 0o644); err != nil {
		t.Fatal(err)
	}

	if err := cleanEmptyDirs(root); err != nil {
		t.Fatalf("cleanEmptyDirs: %v", err)
	}

	for _, gone := range []string{deepEmpty, filepath.Join(root, "models", "owner")} {
		if _, err := os.Stat(gone); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s survived an empty-directory clean (stat: %v)", gone, err)
		}
	}
	for _, stay := range []string{root, filepath.Join(root, "models"), kept, filepath.Join(kept, "hfd.yaml")} {
		if _, err := os.Stat(stay); err != nil {
			t.Errorf("%s was removed but should stay: %v", stay, err)
		}
	}
}
