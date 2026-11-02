// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"sync/atomic"
)

// multipartModeDirect marks a resume layout whose parts are written straight
// into the final temp file with WriteAt instead of into per-part .part-NN files.
// Layouts without the field (older runs) keep the legacy assemble-after-transfer
// layout, whose part files hold real bytes.
const multipartModeDirect = "direct"

// multipartTargetPath is the temp file every direct-mode part writes into. It is
// the same name the single-stream path and the legacy assembly already use, so
// every existing cleanup helper knows how to remove it.
func multipartTargetPath(dst string) string {
	return dst + ".part"
}

// errPartRangeOverflow is returned when the server sends more bytes than the
// requested range covers. Writing them would shift the bytes belonging to a
// later part, so the write is refused instead.
var errPartRangeOverflow = errors.New("server sent more bytes than the requested range")

// rangeWriter writes one HTTP range body at a fixed offset inside the shared
// target file. It stops at the end of its own range, refuses to cross it, and
// checks the part context before every write so pause/cancel/abort stops
// promptly instead of buffering more bytes into the middle of the file.
//
// counter tracks the bytes present in the range (not just the bytes this call
// transferred), which is what the progress ticker reports.
type rangeWriter struct {
	f       *os.File
	off     int64
	limit   int64
	ctx     context.Context
	counter *atomic.Int64
}

func (w *rangeWriter) Write(p []byte) (int, error) {
	if w.ctx != nil {
		if err := w.ctx.Err(); err != nil {
			return 0, err
		}
	}
	if w.off >= w.limit {
		return 0, errPartRangeOverflow
	}
	full := len(p)
	if int64(full) > w.limit-w.off {
		p = p[:w.limit-w.off]
	}
	n, err := w.f.WriteAt(p, w.off)
	w.off += int64(n)
	if w.counter != nil {
		w.counter.Add(int64(n))
	}
	if err != nil {
		return n, err
	}
	if len(p) < full {
		return n, errPartRangeOverflow
	}
	return n, nil
}

// clampPartWritten bounds a persisted part offset by that part's own range, so a
// stale or hand-edited layout can never ask for a write outside the file.
func clampPartWritten(v int64, p multipartPartRange) int64 {
	if v < 0 {
		return 0
	}
	if span := p.End - p.Start + 1; v > span {
		return span
	}
	return v
}

// prepareMultipartMode chooses the multipart layout for dst and prepares the
// resumable artifacts for it, returning the effective layout and whether the
// direct (no-assembly) mode should be used.
//
// Existing .part-NN files keep the legacy layout: their bytes are real and
// resumable, and switching modes underneath them would throw away finished
// work. With no part files on disk the run uses the direct layout, where one
// preallocated temp file receives every part at its final offset and no second
// pass rewrites the payload.
func prepareMultipartMode(dst string, layout multipartResumeLayout) (multipartResumeLayout, bool, error) {
	files, err := multipartPartFiles(dst)
	if err != nil {
		return layout, false, err
	}
	if len(files) > 0 {
		if err := prepareMultipartResume(dst, layout); err != nil {
			return layout, false, err
		}
		return layout, false, nil
	}

	direct, err := prepareMultipartDirect(dst, layout)
	if err != nil {
		return layout, false, err
	}
	return direct, true, nil
}

// prepareMultipartDirect preallocates the shared temp file and works out which
// bytes in it are already real.
//
// A preallocated file is the same size as the payload whether or not any part
// has been written, so its size proves nothing: only the layout written by a
// previous direct run for the exact same part plan carries the per-part offsets.
// Anything else on disk is treated as untrustworthy and the file restarts empty.
func prepareMultipartDirect(dst string, layout multipartResumeLayout) (multipartResumeLayout, error) {
	layout.Mode = multipartModeDirect
	for i := range layout.Parts {
		layout.Parts[i].Written = 0
	}

	stored, err := os.ReadFile(multipartLayoutMetaPath(dst))
	haveStored := false
	if err == nil {
		var existing multipartResumeLayout
		if json.Unmarshal(stored, &existing) == nil &&
			existing.Mode == multipartModeDirect &&
			multipartLayoutsEqual(existing, layout) {
			haveStored = true
			for i := range layout.Parts {
				layout.Parts[i].Written = clampPartWritten(existing.Parts[i].Written, layout.Parts[i])
			}
		}
	} else if !os.IsNotExist(err) {
		return layout, err
	}

	target, err := os.OpenFile(multipartTargetPath(dst), os.O_RDWR|os.O_CREATE, 0o644)
	if err != nil {
		return layout, err
	}
	defer target.Close()

	fi, err := target.Stat()
	if err != nil {
		return layout, err
	}
	size := fi.Size()

	// Preallocate the payload once. Extending the file is a metadata change on
	// NTFS (the added region reads back as zeros without being written), and it
	// fails immediately when the volume cannot hold the file at all instead of
	// after most of the transfer is done.
	if size != layout.Size {
		if err := target.Truncate(layout.Size); err != nil {
			return layout, err
		}
	}

	switch {
	case haveStored && size == layout.Size:
		// The stored per-part offsets describe exactly which bytes are real.
	case !haveStored && size > 0 && size < layout.Size:
		// A shorter, non-preallocated target can only come from the
		// single-stream path writing this same file, and those bytes are a
		// genuine prefix of the payload.
		for i := range layout.Parts {
			layout.Parts[i].Written = clampPartWritten(size-layout.Parts[i].Start, layout.Parts[i])
		}
	}
	// Every other case (preallocated with holes, oversize, or a layout that no
	// longer matches) leaves Written at zero: nothing on disk can be trusted.

	return layout, writeMultipartResumeLayout(dst, layout)
}
