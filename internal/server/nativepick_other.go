// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build !windows && !darwin

package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
)

// pickTool finds the desktop chooser this session has. zenity is the GTK default
// on most desktops, kdialog the KDE one; a server with neither has no dialog to
// offer and the UI keeps using the folder listing.
func pickTool() (string, error) {
	for _, name := range []string{"zenity", "kdialog"} {
		if path, err := exec.LookPath(name); err == nil {
			return path, nil
		}
	}
	return "", errors.New("no folder dialog found (install zenity or kdialog)")
}

// nativePickSupported needs both a chooser binary and a graphical session: without
// a display the tool would fail after the request was already waiting.
func nativePickSupported() bool {
	if strings.TrimSpace(os.Getenv("DISPLAY")) == "" && strings.TrimSpace(os.Getenv("WAYLAND_DISPLAY")) == "" {
		return false
	}
	_, err := pickTool()
	return err == nil
}

// pickFolderNative blocks until a folder is chosen or the dialog is dismissed.
// Both tools exit 1 with no output on Cancel, which is a normal answer here; a
// non-empty stderr is a real failure and is reported.
func pickFolderNative(ctx context.Context, start string) (string, error) {
	tool, err := pickTool()
	if err != nil {
		return "", err
	}

	args := []string{"--file-selection", "--directory"}
	if tool == "kdialog" {
		args = []string{"--getexistingdirectory", start, "ModelFetch"}
	} else if start != "" {
		args = append(args, "--filename="+strings.TrimRight(start, "/")+"/")
	}

	cmd := exec.CommandContext(ctx, tool, args...)
	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err = cmd.Run()
	if ctx.Err() != nil {
		return "", fmt.Errorf("folder dialog did not finish in time: %w", ctx.Err())
	}

	picked := strings.TrimSpace(stdout.String())
	if picked == "" {
		detail := strings.TrimSpace(stderr.String())
		if detail == "" {
			return "", errNativePickCancelled
		}
		return "", errors.New(detail)
	}
	if err != nil {
		// Output arrived anyway, so the folder was picked; report it.
		return picked, nil
	}
	return picked, nil
}
