//go:build windows

// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"syscall"
	"time"
	"unsafe"
)

var moveFileEx = syscall.NewLazyDLL("kernel32.dll").NewProc("MoveFileExW")

const moveFileReplaceExisting = 0x1

// errorSharingViolation and errorUserMappedFile are the variants Windows reports
// when the destination is still held open — the first by a plain reader, the
// second when it is held as a memory map. syscall does not name them.
const (
	errorSharingViolation syscall.Errno = 32
	errorUserMappedFile   syscall.Errno = 1224
)

// A state file is replaced by renaming a finished temp file over it, and Go
// opens files with FILE_SHARE_READ|FILE_SHARE_WRITE only - never
// FILE_SHARE_DELETE - so while any handle on the destination is open,
// MoveFileEx can only fail: an antivirus scan, a reader in this process, or
// another process that just loaded the file. The payload is already committed
// beside the target at that point, so the write is worth waiting a moment for:
// giving up loses the state change, and a dismissed job that reappears after a
// restart is what the user sees.
const (
	replaceAttempts = 10
	replaceDelay    = 25 * time.Millisecond
)

func renameStateFile(oldpath, newpath string) error {
	oldPtr, err := syscall.UTF16FromString(oldpath)
	if err != nil {
		return err
	}
	newPtr, err := syscall.UTF16FromString(newpath)
	if err != nil {
		return err
	}
	var lastErr error
	for attempt := range replaceAttempts {
		if attempt > 0 {
			time.Sleep(replaceDelay)
		}
		// The temp file is created beside the target, so replacement stays on one
		// volume. WRITE_THROUGH is omitted: this commit path makes no power-loss
		// durability claim, and that flag is not needed for replacement semantics.
		result, _, callErr := moveFileEx.Call(
			uintptr(unsafe.Pointer(&oldPtr[0])),
			uintptr(unsafe.Pointer(&newPtr[0])),
			moveFileReplaceExisting,
		)
		if result != 0 {
			return nil
		}
		if callErr == nil {
			// MoveFileEx reported failure without setting a code. Not a busy
			// target, so there is nothing to wait for.
			return errors.New("replace " + newpath + ": MoveFileEx failed without an error code")
		}
		lastErr = callErr
		if !isBusyRenameError(callErr) {
			return callErr
		}
	}
	return lastErr
}

func isBusyRenameError(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	switch errno {
	case syscall.ERROR_ACCESS_DENIED, errorSharingViolation, errorUserMappedFile:
		return true
	}
	return false
}
