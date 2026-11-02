// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
)

// diskArbiterMinSize is the payload size above which a file's local work is
// worth arbitrating. Re-reading a whole multi-GB shard for SHA-256 verification
// saturates a spinning disk, and a small file's re-read finishes before it can
// measurably hurt a sibling transfer, so small files stay unarbitrated (and can
// never delay a sibling's start).
const diskArbiterMinSize = 256 << 20

// diskArbiter keeps the two kinds of disk work a download job performs from
// running on top of each other:
//
//   - transfer: network bytes are being written to a file (several parts at
//     far-apart offsets when the file is multipart), and
//   - local pass: one whole-file sequential re-read (SHA-256 verification,
//     cache publish that has to hash or copy).
//
// On a hard disk a single sequential multi-GB read competes with the several
// far-apart write streams of sibling transfers, and the seek cost can starve
// the transfers almost completely. The arbiter therefore lets transfers run
// freely together, but grants a local pass only while no transfer is writing,
// and holds back the START of new transfers while a local pass is queued so a
// long job cannot starve the queued pass forever.
//
// Transfers never wait on other transfers, so the wait graph is acyclic: a
// queued local pass is always served once the transfers that were already
// running finish.
type diskArbiter struct {
	mu       sync.Mutex
	transfer int
	local    int
	queued   int
	changed  chan struct{}
}

func newDiskArbiter() *diskArbiter {
	return &diskArbiter{changed: make(chan struct{})}
}

// broadcast wakes every waiter so it re-reads the counters. Callers hold mu.
func (a *diskArbiter) broadcast() {
	close(a.changed)
	a.changed = make(chan struct{})
}

// wait re-checks allowed() until it is true or ctx is done. Callers hold mu.
func (a *diskArbiter) wait(ctx context.Context, allowed func() bool) error {
	for !allowed() {
		ch := a.changed
		a.mu.Unlock()
		select {
		case <-ch:
		case <-ctx.Done():
			a.mu.Lock()
			return ctx.Err()
		}
		a.mu.Lock()
	}
	return nil
}

// beginTransfer admits a file that is about to write downloaded bytes.
func (a *diskArbiter) beginTransfer(ctx context.Context) error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.wait(ctx, func() bool { return a.local == 0 && a.queued == 0 }); err != nil {
		return err
	}
	a.transfer++
	return nil
}

// endTransfer releases a transfer slot.
func (a *diskArbiter) endTransfer() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.transfer--
	a.broadcast()
	a.mu.Unlock()
}

// beginLocalPass admits a whole-file local re-read. It returns with no transfer
// writing and no other local pass running.
func (a *diskArbiter) beginLocalPass(ctx context.Context) error {
	if a == nil {
		return nil
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	a.queued++
	err := a.wait(ctx, func() bool { return a.transfer == 0 && a.local == 0 })
	if err != nil {
		a.queued--
		a.broadcast()
		return err
	}
	a.queued--
	a.local++
	return nil
}

// endLocalPass releases a local-pass slot.
func (a *diskArbiter) endLocalPass() {
	if a == nil {
		return
	}
	a.mu.Lock()
	a.local--
	a.broadcast()
	a.mu.Unlock()
}

// diskArbiterFor returns the arbiter that guards writes under root. Jobs that
// share a destination root (the usual case: one cache directory) share one
// arbiter; different roots do not wait on each other.
func diskArbiterFor(root string) *diskArbiter {
	key := arbiterKey(root)
	diskArbitersMu.Lock()
	defer diskArbitersMu.Unlock()
	if a, ok := diskArbiters[key]; ok {
		return a
	}
	a := newDiskArbiter()
	diskArbiters[key] = a
	return a
}

// arbiterKey normalizes a destination root into a map key. Windows paths are
// case-insensitive, so only there is the key folded.
func arbiterKey(root string) string {
	root = filepath.Clean(root)
	if filepath.VolumeName(root) != "" {
		return strings.ToUpper(root)
	}
	return root
}

var (
	diskArbitersMu sync.Mutex
	diskArbiters   = make(map[string]*diskArbiter)
)
