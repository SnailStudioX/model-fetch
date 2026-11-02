// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// errorPrivilegeNotHeld is what Windows CreateSymbolicLink reports when the
// process has no SeCreateSymbolicLinkPrivilege - no elevation and no Developer
// Mode. Go does not map that code to EPERM or EACCES, so os.IsPermission is false
// for it: a fixture guard that wants to recognise "this account cannot make
// symlinks" has to name the code itself.
const errorPrivilegeNotHeld syscall.Errno = 1314

// mustSymlink builds a fixture symlink, skipping the test where this account is
// not allowed to create one. A symlink failure anywhere else - another platform,
// or Windows with the privilege - is a real problem and stays fatal.
func mustSymlink(t *testing.T, oldname, newname string) {
	t.Helper()
	if err := os.Symlink(oldname, newname); err != nil {
		if errors.Is(err, errorPrivilegeNotHeld) {
			t.Skipf("this account cannot create symlinks: %v", err)
		}
		t.Fatal(err)
	}
}

func TestMain(m *testing.M) {
	root, err := os.MkdirTemp("", "modelfetch-server-tests-")
	if err != nil {
		panic(err)
	}
	for key, value := range map[string]string{
		"HOME":            root,
		"XDG_CONFIG_HOME": filepath.Join(root, "config"),
		"APPDATA":         filepath.Join(root, "appdata"),
		"LOCALAPPDATA":    filepath.Join(root, "localappdata"),
		"USERPROFILE":     root,
	} {
		if err := os.Setenv(key, value); err != nil {
			_ = os.RemoveAll(root)
			panic(err)
		}
	}
	for _, key := range []string{"HF_HUB_CACHE", "HF_HOME", "HF_TOKEN"} {
		if err := os.Unsetenv(key); err != nil {
			_ = os.RemoveAll(root)
			panic(err)
		}
	}

	code := m.Run()
	if err := os.RemoveAll(root); err != nil {
		_, _ = os.Stderr.WriteString("remove test sandbox: " + err.Error() + "\n")
		if code == 0 {
			code = 1
		}
	}
	os.Exit(code)
}

func newTestJobManager(t *testing.T, cfg Config, hub *WSHub) *JobManager {
	t.Helper()
	statePath := filepath.Join(t.TempDir(), "jobs_state.json")
	m := newJobManagerWithStatePath(cfg, hub, statePath)
	registerTestJobManagerCleanup(t, m)
	return m
}

func registerTestJobManagerCleanup(t *testing.T, m *JobManager) {
	t.Helper()
	t.Cleanup(func() {
		select {
		case <-m.closeDone:
			return
		default:
		}
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		if err := m.Close(ctx); err != nil {
			t.Errorf("close test job manager: %v", err)
			<-m.closeDone
		}
	})
}
