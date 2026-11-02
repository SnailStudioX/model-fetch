// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package hfdownloader

import "syscall"

// processQueryLimitedInformation matches PROCESS_QUERY_LIMITED_INFORMATION.
const processQueryLimitedInformation = 0x1000

// isProcessAlive checks if a process with the given PID is still running.
// Windows has no signal 0; querying process access rights is the supported
// probe, and PROCESS_QUERY_LIMITED_INFORMATION also works across elevation
// levels where full QUERY_INFORMATION would be denied.
func isProcessAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	handle, err := syscall.OpenProcess(processQueryLimitedInformation, false, uint32(pid))
	if err != nil {
		return false
	}
	_ = syscall.CloseHandle(handle)
	return true
}
