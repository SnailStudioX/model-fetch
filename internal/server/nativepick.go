// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

// A folder dialog is modal: it waits for a person to click through the tree, so
// the request that opened it stays open for as long as they take. The timeout is
// only a backstop so a browser tab that vanished cannot hold the single dialog
// slot forever.
const nativePickTimeout = 15 * time.Minute

// nativePickLock keeps one dialog on the desktop at a time. Two dialogs stacked
// on one screen are worse than a clear "already open" answer, and the platform
// helpers are not required to be safe for concurrent use.
var nativePickLock sync.Mutex

// errNativePickCancelled is the sentinel every platform helper returns when the
// person dismissed the dialog. Cancelling is a normal answer, not a failure.
var errNativePickCancelled = errors.New("folder dialog cancelled")

// nativePickDisabled lets a headless deployment (Docker, CI, a test run) turn the
// dialog off without a build tag, so the UI keeps using the folder listing.
func nativePickDisabled() bool {
	switch strings.ToLower(strings.TrimSpace(os.Getenv("MODELFETCH_NO_NATIVE_PICK"))) {
	case "1", "true", "yes", "on":
		return true
	default:
		return false
	}
}

// nativePickAvailable reports whether a folder dialog can be shown right now,
// which is what the UI needs before it offers the button.
func nativePickAvailable() bool {
	if nativePickDisabled() {
		return false
	}
	return nativePickSupported()
}

type nativePickResponse struct {
	Path      string `json:"path,omitempty"`
	Cancelled bool   `json:"cancelled,omitempty"`
	Available bool   `json:"available"`
}

// isLoopbackRequest reports whether the request came from a browser on the same
// machine as the server. The dialog opens on the server desktop, so a browser on
// another host would be steering a screen it cannot see.
func isLoopbackRequest(r *http.Request) bool {
	host, _, err := net.SplitHostPort(strings.TrimSpace(r.RemoteAddr))
	if err != nil {
		host = strings.TrimSpace(r.RemoteAddr)
	}
	if host == "" {
		return false
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

// handleNativePick opens the operating system folder chooser on the machine that
// runs the server and returns the folder that was picked. A browser cannot read a
// local absolute path from a file dialog of its own, so the only way to get one is
// to ask the process that already owns the filesystem. The picker stays read-only:
// nothing is created, written, or deleted here either.
func (s *Server) handleNativePick(w http.ResponseWriter, r *http.Request) {
	if !isLoopbackRequest(r) {
		writeError(w, http.StatusForbidden, "Folder dialog needs a local browser", "the dialog opens on this machine desktop, so it is only offered to a browser on 127.0.0.1 or ::1")
		return
	}
	if !nativePickAvailable() {
		writeError(w, http.StatusNotImplemented, "No folder dialog on this server", "this server has no desktop session to show a dialog on; pick from the folder listing instead")
		return
	}

	// The dialog starts where the field points, or at the first configured folder
	// when the field is still empty, which is the same rule the listing uses.
	requested := strings.TrimSpace(r.URL.Query().Get("path"))
	if requested == "" {
		cfg := s.snapshotConfig().captureCacheEnvironment()
		requested = firstConfiguredDir(cfg)
	}
	if requested != "" {
		normalized, err := normalizeBrowsePath(requested)
		if err != nil {
			writeError(w, http.StatusBadRequest, "Invalid path", err.Error())
			return
		}
		requested = normalized
	}

	if !nativePickLock.TryLock() {
		writeError(w, http.StatusConflict, "A folder dialog is already open", "close the dialog that is already on this machine desktop, then try again")
		return
	}
	defer nativePickLock.Unlock()

	// The server-wide write deadline is far shorter than a person needs to browse
	// a tree, and the request context is cancelled when that deadline arms, so this
	// handler clears the deadline and drives the dialog from its own context.
	if controller := http.NewResponseController(w); controller != nil {
		_ = controller.SetWriteDeadline(time.Time{})
		_ = controller.SetReadDeadline(time.Time{})
	}
	ctx, cancel := context.WithTimeout(context.Background(), nativePickTimeout)
	defer cancel()

	picked, err := pickFolderNative(ctx, requested)
	switch {
	case errors.Is(err, errNativePickCancelled):
		writeJSON(w, http.StatusOK, nativePickResponse{Available: true, Cancelled: true})
	case err != nil:
		writeError(w, http.StatusBadGateway, "Folder dialog failed", err.Error())
	default:
		picked, normalizeErr := normalizeBrowsePath(picked)
		if normalizeErr != nil || picked == "" {
			writeError(w, http.StatusBadGateway, "Folder dialog returned no folder", "")
			return
		}
		writeJSON(w, http.StatusOK, nativePickResponse{Path: picked, Available: true})
	}
}
