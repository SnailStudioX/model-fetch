// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"time"
)

// contextReader wraps an io.Reader and returns ctx.Err() as soon as ctx is
// done. Passing it into io.Copy makes long local I/O (hashing, copying,
// part assembly) stop promptly on cancellation instead of running to
// completion after a sibling's permanent error cancelled the job.
type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (c contextReader) Read(p []byte) (int, error) {
	if err := c.ctx.Err(); err != nil {
		return 0, err
	}
	return c.r.Read(p)
}

// computeSHA256 computes and returns the SHA256 hash of a file.
func computeSHA256(path string) (string, error) {
	return computeSHA256Ctx(context.Background(), path)
}

// hashBufferSize is the read buffer used for whole-file hashes. The verify
// path re-reads multi-GB payloads, and io.Copy's 32 KiB default makes the
// syscall count visible on slow disks, so the hash loop reads in MiB chunks.
const hashBufferSize = 1 << 20

// stageProgress reports byte progress for the local post-download work of a
// single file (part assembly, whole-file hash) at most about once a second,
// so a multi-GB re-read shows as a moving percentage instead of flooding the
// websocket with one event per buffer.
type stageProgress struct {
	path    string
	stage   string
	message string
	total   int64
	emit    func(ProgressEvent)

	last time.Time
}

// report emits the running byte count. A nil receiver is accepted so callers
// can pass a stage unconditionally when no progress is wanted.
func (s *stageProgress) report(done int64) {
	if s == nil || s.emit == nil {
		return
	}
	now := time.Now()
	if !s.last.IsZero() && now.Sub(s.last) < time.Second {
		return
	}
	s.last = now
	s.emit(ProgressEvent{
		Event:      "file_finalizing",
		Path:       s.path,
		Stage:      s.stage,
		StageBytes: done,
		StageTotal: s.total,
		Message:    s.message,
	})
}

// hashFileCtx hashes path and reports the running byte count through
// onProgress (nil when the caller has no use for it). It aborts promptly with
// the context error when ctx is cancelled mid-hash.
func hashFileCtx(ctx context.Context, path string, onProgress func(int64)) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	buf := make([]byte, hashBufferSize)
	var done int64
	for {
		if err := ctx.Err(); err != nil {
			return "", err
		}
		n, rerr := f.Read(buf)
		if n > 0 {
			h.Write(buf[:n])
			done += int64(n)
			if onProgress != nil {
				onProgress(done)
			}
		}
		if rerr != nil {
			if errors.Is(rerr, io.EOF) {
				break
			}
			return "", rerr
		}
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// computeSHA256Ctx is computeSHA256 bounded by ctx: it aborts promptly with the
// context error if ctx is cancelled while hashing. Existing ctx-free callers
// keep using computeSHA256 (context.Background()).
func computeSHA256Ctx(ctx context.Context, path string) (string, error) {
	return hashFileCtx(ctx, path, nil)
}

// HashFileCtx reports the SHA-256 of a file's contents, bounded by ctx. It is
// exported for callers outside the download path that must hash the same bytes the
// downloader verified when it wrote them - the cache integrity check re-reads a
// stored blob and compares the digest with the name it is stored under.
func HashFileCtx(ctx context.Context, path string) (string, error) {
	return hashFileCtx(ctx, path, nil)
}

// verifySHA256 computes the SHA256 of a file and compares it to expected.
func verifySHA256(path string, expected string) error {
	return verifySHA256Ctx(context.Background(), path, expected)
}

// verifySHA256Ctx is verifySHA256 bounded by ctx (see computeSHA256Ctx). It is
// used by the downloader so a cancelled job does not wait for a multi-GB
// re-hash before surfacing a sibling's permanent error.
func verifySHA256Ctx(ctx context.Context, path string, expected string) error {
	return verifySHA256Progress(ctx, path, expected, nil)
}

// verifySHA256Progress is verifySHA256Ctx with the re-read made visible through
// sp (nil when no progress is wanted).
func verifySHA256Progress(ctx context.Context, path string, expected string, sp *stageProgress) error {
	sum, err := hashFileCtx(ctx, path, func(done int64) { sp.report(done) })
	if err != nil {
		return err
	}
	if !strings.EqualFold(sum, expected) {
		return fmt.Errorf("sha256 mismatch: expected %s got %s", expected, sum)
	}
	return nil
}

// verifyDigest checks path against expected, reusing a digest that was already
// computed over the same bytes (multipart part assembly hashes while it
// copies) when the caller has one. precomputed is a hex digest or "" when no
// digest is available, in which case the file is read and hashed as before.
// Skipping that second full read is minutes of disk I/O on a multi-GB shard.
func verifyDigest(ctx context.Context, path string, expected string, precomputed string) error {
	return verifyDigestProgress(ctx, path, expected, precomputed, nil)
}

// verifyDigestProgress is verifyDigest with the fallback re-read made visible:
// without a precomputed digest the whole file has to be read again, which costs
// minutes on a multi-GB shard, so the running byte count is reported as stage
// progress (sp may be nil) and the job card shows the hash as a percentage
// instead of a silent freeze at 100%. A precomputed digest costs nothing here:
// no file is opened and no stage event is emitted.
func verifyDigestProgress(ctx context.Context, path string, expected string, precomputed string, sp *stageProgress) error {
	if precomputed != "" {
		if !strings.EqualFold(precomputed, expected) {
			return fmt.Errorf("sha256 mismatch: expected %s got %s", expected, precomputed)
		}
		return nil
	}
	if sp != nil && sp.total <= 0 {
		// Unknown planned size (unknown-length download): the bytes on disk are
		// exactly what the hash is about to read.
		if fi, err := os.Stat(path); err == nil {
			sp.total = fi.Size()
		}
	}
	return verifySHA256Progress(ctx, path, expected, sp)
}

// shouldSkipLocal checks if a file already exists and matches expected hash/size.
// Returns (skip, reason, error).
func shouldSkipLocal(it PlanItem, dst string) (bool, string, error) {
	return shouldSkipLocalCtx(context.Background(), it, dst)
}

// shouldSkipLocalCtx is shouldSkipLocal bounded by ctx. A multi-GB LFS hash can
// otherwise run to completion after a sibling's permanent error cancelled the
// job; with ctx it aborts promptly. Successful skip behavior is unchanged: a
// cancelled ctx returns ctx.Err() so the caller surfaces the cancellation
// instead of skipping.
func shouldSkipLocalCtx(ctx context.Context, it PlanItem, dst string) (bool, string, error) {
	fi, err := os.Stat(dst)
	if err != nil {
		// no file
		return false, "", nil
	}

	// Quick size check first: if known and different, don't skip
	if it.Size > 0 && fi.Size() != it.Size {
		return false, "", nil
	}

	// LFS with known sha: compute and compare
	if it.LFS && it.SHA256 != "" {
		if err := verifySHA256Ctx(ctx, dst, it.SHA256); err == nil {
			return true, "sha256 match", nil
		} else if verr := ctx.Err(); verr != nil {
			// Cancelled mid-verify: surface the cancellation, do not skip.
			return false, "", verr
		}
		// size matched but sha mismatched -> re-download
		return false, "", nil
	}

	// Non-LFS (or unknown sha): size match is sufficient
	if it.Size > 0 && fi.Size() == it.Size {
		return true, "size match", nil
	}

	return false, "", nil
}
