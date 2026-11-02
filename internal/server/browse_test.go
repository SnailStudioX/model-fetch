// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
)

func decodeBrowse(t *testing.T, w *httptest.ResponseRecorder) browseResponse {
	t.Helper()
	var resp browseResponse
	if err := json.NewDecoder(w.Body).Decode(&resp); err != nil {
		t.Fatalf("decode: %v (%s)", err, w.Body.String())
	}
	return resp
}

func TestBrowseListsDirectoriesOnly(t *testing.T) {
	root := t.TempDir()
	for _, name := range []string{"beta", "Alpha", "nested"} {
		if err := os.Mkdir(filepath.Join(root, name), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(filepath.Join(root, "not-a-folder.txt"), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}

	srv := newTestServerWithConfig(t, Config{CacheDir: root})
	w := httptest.NewRecorder()
	srv.handleBrowse(w, httptest.NewRequest("GET", "/api/browse?path="+url.QueryEscape(root), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}

	resp := decodeBrowse(t, w)
	if resp.Path != root {
		t.Errorf("path=%q want %q", resp.Path, root)
	}
	if resp.Parent == "" || resp.Parent == root {
		t.Errorf("parent=%q want a parent that is not the listed folder", resp.Parent)
	}
	if resp.FromConfig {
		t.Errorf("from_config should be false for an explicit path")
	}
	if resp.Limit != browseEntryLimit {
		t.Errorf("limit=%d want %d", resp.Limit, browseEntryLimit)
	}

	names := make([]string, 0, len(resp.Entries))
	for _, entry := range resp.Entries {
		names = append(names, entry.Name)
		if entry.Path != filepath.Join(root, entry.Name) {
			t.Errorf("entry path %q want %q", entry.Path, filepath.Join(root, entry.Name))
		}
	}
	if len(names) != 3 || names[0] != "Alpha" || names[1] != "beta" || names[2] != "nested" {
		t.Errorf("entries=%v want three directories sorted case-insensitively", names)
	}
}

func TestBrowseTruncatesLargeListing(t *testing.T) {
	root := t.TempDir()
	for i := 0; i < browseEntryLimit+2; i++ {
		if err := os.Mkdir(filepath.Join(root, strconv.FormatInt(int64(i), 10)), 0o755); err != nil {
			t.Fatal(err)
		}
	}

	srv := newTestServerWithConfig(t, Config{CacheDir: root})
	w := httptest.NewRecorder()
	srv.handleBrowse(w, httptest.NewRequest("GET", "/api/browse?path="+url.QueryEscape(root), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}

	resp := decodeBrowse(t, w)
	if len(resp.Entries) != browseEntryLimit || !resp.Truncated {
		t.Errorf("entries=%d truncated=%v want %d and truncated", len(resp.Entries), resp.Truncated, browseEntryLimit)
	}
}

func TestBrowseRejectsRelativePath(t *testing.T) {
	srv := newTestServerWithConfig(t, Config{CacheDir: t.TempDir()})
	w := httptest.NewRecorder()
	srv.handleBrowse(w, httptest.NewRequest("GET", "/api/browse?path=relative", nil))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
}

func TestBrowseDefaultsToConfiguredFolder(t *testing.T) {
	root := t.TempDir()
	srv := newTestServerWithConfig(t, Config{CacheDir: root})
	w := httptest.NewRecorder()
	srv.handleBrowse(w, httptest.NewRequest("GET", "/api/browse", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}

	resp := decodeBrowse(t, w)
	if resp.Path != root || !resp.FromConfig {
		t.Errorf("path=%q from_config=%v want configured %q", resp.Path, resp.FromConfig, root)
	}
}

func TestBrowseReportsUnreadableFolder(t *testing.T) {
	root := t.TempDir()
	srv := newTestServerWithConfig(t, Config{CacheDir: root})
	w := httptest.NewRecorder()
	srv.handleBrowse(w, httptest.NewRequest("GET", "/api/browse?path="+url.QueryEscape(filepath.Join(root, "missing")), nil))
	if w.Code != http.StatusNotFound {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}
}

// The picker needs a way back out of a volume root: on Windows a drive has no
// parent and another drive is not visible from inside it, so the response carries
// the drive roots it can offer.
func TestBrowseOffersVolumeRoots(t *testing.T) {
	root := t.TempDir()
	srv := newTestServerWithConfig(t, Config{CacheDir: root})
	w := httptest.NewRecorder()
	srv.handleBrowse(w, httptest.NewRequest("GET", "/api/browse?path="+url.QueryEscape(root), nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}

	resp := decodeBrowse(t, w)
	if len(resp.Roots) == 0 {
		t.Fatalf("roots empty, want at least the root of %q", root)
	}
	want := filepath.VolumeName(root)
	if want == "" {
		want = "/"
	}
	found := false
	for _, entry := range resp.Roots {
		if filepath.VolumeName(entry.Path) == want || entry.Path == want {
			found = true
		}
	}
	if !found {
		t.Fatalf("roots %v do not include the volume of %q", resp.Roots, root)
	}
}

// A pasted "~" path is how a macOS or Linux user names their home folder, and a
// bare drive letter is how a Windows user names another drive; both are rejected
// by filepath.IsAbs as written, so the handler completes them first.
func TestBrowseExpandsHomePath(t *testing.T) {
	home, err := os.UserHomeDir()
	if err != nil {
		t.Skipf("no home directory: %v", err)
	}
	if info, statErr := os.Stat(home); statErr != nil || !info.IsDir() {
		t.Skipf("home is not a folder: %v", statErr)
	}

	srv := newTestServerWithConfig(t, Config{CacheDir: t.TempDir()})
	w := httptest.NewRecorder()
	srv.handleBrowse(w, httptest.NewRequest("GET", "/api/browse?path=~", nil))
	if w.Code != http.StatusOK {
		t.Fatalf("status %d: %s", w.Code, w.Body.String())
	}

	resp := decodeBrowse(t, w)
	if resp.Path != filepath.Clean(home) {
		t.Errorf("path=%q want home %q", resp.Path, filepath.Clean(home))
	}
}

func TestBrowseCompletesDriveRelativePath(t *testing.T) {
	if runtime.GOOS != "windows" {
		t.Skip("drive-relative paths only exist on Windows")
	}

	root := t.TempDir()
	volume := filepath.VolumeName(root)
	if volume == "" {
		t.Fatalf("temp dir %q has no volume", root)
	}
	// The same folders written without the separator after the drive letter: "C:"
	// for the drive root and "C:Users\\..." for a folder inside it. Both are drive-
	// relative to the process working directory, which is not what the picker means.
	separator := string(filepath.Separator)
	for _, testCase := range []struct{ requested, want string }{
		{volume, volume + separator},
		{volume + strings.TrimPrefix(strings.TrimPrefix(root, volume), separator), filepath.Clean(root)},
	} {
		srv := newTestServerWithConfig(t, Config{CacheDir: root})
		w := httptest.NewRecorder()
		srv.handleBrowse(w, httptest.NewRequest("GET", "/api/browse?path="+url.QueryEscape(testCase.requested), nil))
		if w.Code != http.StatusOK {
			t.Errorf("path=%q status %d: %s", testCase.requested, w.Code, w.Body.String())
			continue
		}
		resp := decodeBrowse(t, w)
		if resp.Path != testCase.want {
			t.Errorf("path=%q resolved to %q want %q", testCase.requested, resp.Path, testCase.want)
		}
	}
}
