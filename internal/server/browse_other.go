// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build !windows

package server

// volumeRoots has one root on POSIX systems, so the picker needs no drive row
// there: the listing reaches every folder by walking down from "/".
func volumeRoots() []browseEntry {
	return []browseEntry{{Name: "/", Path: "/"}}
}
