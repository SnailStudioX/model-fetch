// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

// browseEntryLimit bounds one listing so a large directory cannot turn into a
// large response. The client is told when the list was cut short.
const browseEntryLimit = 500

type browseEntry struct {
	Name      string `json:"name"`
	Path      string `json:"path"`
	IsSymlink bool   `json:"is_symlink,omitempty"`
}

type browseResponse struct {
	Path   string `json:"path"`
	Parent string `json:"parent,omitempty"`
	// Roots lists the places a listing can start from, so a Windows picker can
	// leave the drive it opened on: a volume root has no parent, and one drive is
	// not visible from inside another.
	Roots      []browseEntry `json:"roots,omitempty"`
	Entries    []browseEntry `json:"entries"`
	Truncated  bool          `json:"truncated,omitempty"`
	Limit      int           `json:"limit"`
	FromConfig bool          `json:"from_config,omitempty"`
	// NativePick tells the picker whether this server can show the operating
	// system own folder dialog, so the UI only offers a button that can work.
	NativePick bool `json:"native_pick,omitempty"`
}

// handleBrowse lists the directories under a path so the Settings page can pick a
// folder instead of typing one. It is read-only: nothing is created, written, or
// deleted, and a symlinked entry is reported as a symlink rather than resolved
// into the listing. Unlike delete, mirror, and rebuild, which stay inside the
// configured roots, this listing is not confined to them, because picking a
// folder that is not configured yet is the point of the endpoint.
func (s *Server) handleBrowse(w http.ResponseWriter, r *http.Request) {
	cfg := s.snapshotConfig().captureCacheEnvironment()

	requested := strings.TrimSpace(r.URL.Query().Get("path"))
	fromConfig := false
	if requested == "" {
		requested = firstConfiguredDir(cfg)
		fromConfig = requested != ""
	}
	if requested == "" {
		writeError(w, http.StatusBadRequest, "Invalid path", "path is required when no folder is configured")
		return
	}
	requested, err := normalizeBrowsePath(requested)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid path", err.Error())
		return
	}
	if !filepath.IsAbs(requested) {
		writeError(w, http.StatusBadRequest, "Invalid path", "path must be absolute")
		return
	}

	entries, truncated, err := listBrowseDirectories(requested)
	if err != nil {
		writeError(w, http.StatusNotFound, "Cannot list folder", err.Error())
		return
	}

	writeJSON(w, http.StatusOK, browseResponse{
		Path:       requested,
		Parent:     browseParent(requested),
		Roots:      volumeRoots(),
		Entries:    entries,
		Truncated:  truncated,
		Limit:      browseEntryLimit,
		FromConfig: fromConfig,
		NativePick: nativePickAvailable(),
	})
}

// firstConfiguredDir is where the picker opens when the field is still empty. A
// configured folder that no longer exists is skipped, so the picker lands on a
// listing that can actually be read.
func firstConfiguredDir(cfg Config) string {
	candidates := []string{cfg.LocalDir, cfg.cache().HubDir(), cfg.cacheRoot()}
	candidates = append(candidates, routeDirs(cfg.DownloadRoutes)...)
	candidates = append(candidates, RunDir())

	for _, candidate := range candidates {
		if candidate == "" {
			continue
		}
		if info, err := os.Stat(candidate); err == nil && info.IsDir() {
			return candidate
		}
	}
	for _, candidate := range candidates {
		if candidate != "" {
			return candidate
		}
	}
	return ""
}

// normalizeBrowsePath accepts what a person actually types into the picker
// field: "~" and "~/some/folder" expand to the home folder on every platform,
// and a Windows drive written without its root ("D:", "D:Models") is completed
// to that drive root, which filepath.IsAbs would otherwise reject.
func normalizeBrowsePath(raw string) (string, error) {
	path := strings.TrimSpace(raw)
	if path == "" {
		return "", nil
	}
	if path == "~" || strings.HasPrefix(path, "~/") || strings.HasPrefix(path, "~\\") {
		home, err := os.UserHomeDir()
		if err != nil {
			return "", fmt.Errorf("cannot expand ~: %w", err)
		}
		rest := strings.TrimLeft(strings.TrimPrefix(path, "~"), "/\\")
		path = filepath.Join(home, rest)
	}
	if len(path) == 2 && path[1] == ':' && ((path[0] >= 'A' && path[0] <= 'Z') || (path[0] >= 'a' && path[0] <= 'z')) {
		path += string(filepath.Separator)
	}
	if volume := filepath.VolumeName(path); volume != "" {
		rest := path[len(volume):]
		if rest == "" || rest == "/" || rest == string(filepath.Separator) {
			rest = ""
		} else {
			rest = strings.TrimLeft(rest, "/\\")
		}
		path = volume + string(filepath.Separator) + rest
	}
	return filepath.Clean(path), nil
}

// browseParent stops at the volume root, where Dir() would return the same path.
func browseParent(path string) string {
	parent := filepath.Dir(path)
	if parent == path {
		return ""
	}
	return parent
}

func listBrowseDirectories(dir string) ([]browseEntry, bool, error) {
	handle, err := os.Open(dir)
	if err != nil {
		return nil, false, err
	}
	defer handle.Close()

	found, err := handle.ReadDir(-1)
	if err != nil {
		return nil, false, err
	}

	entries := []browseEntry{}
	truncated := false
	for _, entry := range found {
		isDir := entry.IsDir()
		isSymlink := entry.Type()&os.ModeSymlink != 0
		if !isDir && !isSymlink {
			continue
		}
		if len(entries) >= browseEntryLimit {
			truncated = true
			break
		}
		entries = append(entries, browseEntry{
			Name:      entry.Name(),
			Path:      filepath.Join(dir, entry.Name()),
			IsSymlink: isSymlink,
		})
	}

	sort.Slice(entries, func(i, j int) bool {
		return strings.ToLower(entries[i].Name) < strings.ToLower(entries[j].Name)
	})
	return entries, truncated, nil
}
