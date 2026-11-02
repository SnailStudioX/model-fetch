// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package server

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
)

// folderPickScript shows the folder chooser Windows already ships, so no new
// dependency is needed. The start path travels through an environment variable
// rather than the command line, so a path containing quotes or PowerShell syntax
// cannot turn into script text.
const folderPickScript = `
[Console]::OutputEncoding = [System.Text.UTF8Encoding]::new($false)
Add-Type -AssemblyName System.Windows.Forms
$dialog = New-Object System.Windows.Forms.FolderBrowserDialog
$dialog.ShowNewFolderButton = $true
$start = [Environment]::GetEnvironmentVariable('MODELFETCH_PICK_START')
if ($start -and (Test-Path -LiteralPath $start -PathType Container)) { $dialog.SelectedPath = $start }
if ($dialog.ShowDialog() -ne [System.Windows.Forms.DialogResult]::OK) { exit 2 }
[Console]::Out.WriteLine($dialog.SelectedPath)
exit 0
`

// nativePickSupported reports the chooser is reachable: Windows PowerShell is part
// of every install this app runs on, but a stripped image could lack it.
func nativePickSupported() bool {
	_, err := exec.LookPath("powershell.exe")
	return err == nil
}

// pickFolderNative blocks until the person picks a folder or dismisses the dialog.
// Cancelling maps to errNativePickCancelled; anything else is reported, because a
// dialog that failed to open must not look like an empty success.
func pickFolderNative(ctx context.Context, start string) (string, error) {
	cmd := exec.CommandContext(ctx, "powershell.exe",
		"-NoProfile", "-NonInteractive", "-STA", "-Command", folderPickScript)
	cmd.Env = append(os.Environ(), "MODELFETCH_PICK_START="+start)
	// The server usually runs in a terminal or a hidden window; a second console
	// flashing behind the dialog is noise nobody asked for.
	cmd.SysProcAttr = &syscall.SysProcAttr{HideWindow: true}

	var stdout, stderr strings.Builder
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	if ctx.Err() != nil {
		return "", fmt.Errorf("folder dialog did not finish in time: %w", ctx.Err())
	}
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 2 {
			return "", errNativePickCancelled
		}
		detail := strings.TrimSpace(stderr.String())
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
