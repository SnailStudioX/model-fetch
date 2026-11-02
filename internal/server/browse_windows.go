// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"syscall"
)

// kernel32 GetLogicalDrives returns a bit per registered drive. It is reached
// through the standard library loader because syscall has no wrapper for it and
// the project adds no dependencies: the mask reports which drive roots exist
// without touching their media, so a disconnected network share or an empty card
// reader cannot stall the listing the way os.Stat on every letter would.
var procGetLogicalDrives = syscall.NewLazyDLL("kernel32.dll").NewProc("GetLogicalDrives")

// volumeRoots lists the drive roots that exist right now so the folder picker can
// offer them as starting points; a volume root has no parent, and one drive is
// not visible from inside another.
func volumeRoots() []browseEntry {
	mask, _, _ := procGetLogicalDrives.Call()
	if mask == 0 {
		// Zero is what GetLogicalDrives returns on failure; a real system always
		// has at least the boot drive, so there is nothing to offer.
		return nil
	}
	roots := []browseEntry{}
	for index := 0; index < 26; index++ {
		if uint32(mask)&(1<<index) == 0 {
			continue
		}
		letter := string(rune('A' + index))
		roots = append(roots, browseEntry{
			Name: letter + ":",
			Path: letter + ":" + "\\",
		})
	}
	return roots
}
