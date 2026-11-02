// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package server

import (
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// folderPickScript asks Finder for a folder through Standard Additions. The prompt
// and the start path arrive as script arguments, never as script text, so a path
// with quotes in it cannot break out of the script.
const folderPickScript = `on run argv
	set thePrompt to item 1 of argv
	set theStart to item 2 of argv
	if theStart is not "" then
		try
			set theFolder to choose folder with prompt thePrompt default location (POSIX file theStart)
		on error errMsg number errNum
			if errNum is -128 then error errMsg number -128
			set theFolder to choose folder with prompt thePrompt
		end try
	else
		set theFolder to choose folder with prompt thePrompt
	end if
	return POSIX path of theFolder
end run`

// nativePickSupported reports osascript is present, which it is on every macOS
// install; a missing one means this is not a Mac desktop and there is nothing to
// show the dialog on.
func nativePickSupported() bool {
	_, err := exec.LookPath("osascript")
	return err == nil
}

// pickFolderNative blocks until a folder is chosen or the person presses Cancel.
// AppleScript reports Cancel as error -128, which is a normal answer here.
func pickFolderNative(ctx context.Context, start string) (string, error) {
	cmd := exec.CommandContext(ctx, "osascript", "-e", folderPickScript, "ModelFetch", start)

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if ctx.Err() != nil {
		return "", fmt.Errorf("folder dialog did not finish in time: %w", ctx.Err())
	}
	if err != nil {
		detail := strings.TrimSpace(stderr.String())
		if strings.Contains(detail, "-128") || strings.Contains(strings.ToLower(detail), "user canceled") {
			return "", errNativePickCancelled
		}
		if detail == "" {
			detail = err.Error()
		}
		return "", errors.New(detail)
	}

	picked := strings.TrimSpace(stdout.String())
	if picked == "" {
		return "", errNativePickCancelled
	}
	return picked, nil
}
