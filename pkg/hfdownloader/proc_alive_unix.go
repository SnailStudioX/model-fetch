// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package hfdownloader

import (
	"os"
	"syscall"
)

// isProcessAlive checks if a process with the given PID is still running.
func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return false
	}
	// On Unix, FindProcess always succeeds. We need to send signal 0 to check.
	err = process.Signal(syscall.Signal(0))
	return err == nil
}
