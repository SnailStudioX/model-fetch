// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestComputeSHA256(t *testing.T) {
	tmpDir := t.TempDir()

	t.Run("known content", func(t *testing.T) {
		// SHA256 of "hello world\n" is well-known
		content := []byte("hello world\n")

		filePath := filepath.Join(tmpDir, "test.txt")
		if err := os.WriteFile(filePath, content, 0644); err != nil {
			t.Fatal(err)
		}

		hash, err := computeSHA256(filePath)
		if err != nil {
			t.Fatalf("computeSHA256 error: %v", err)
		}

		// Note: The actual hash depends on exact bytes
		// We just verify it produces a 64-character hex string
		if len(hash) != 64 {
			t.Errorf("hash length = %d, want 64", len(hash))
		}
	})

	t.Run("empty file", func(t *testing.T) {
		filePath := filepath.Join(tmpDir, "empty.txt")
		if err := os.WriteFile(filePath, []byte{}, 0644); err != nil {
			t.Fatal(err)
		}

		// SHA256 of empty file
		expectedHash := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

		hash, err := computeSHA256(filePath)
		if err != nil {
			t.Fatalf("computeSHA256 error: %v", err)
		}

		if hash != expectedHash {
			t.Errorf("hash = %s, want %s", hash, expectedHash)
		}
	})

	t.Run("file not found", func(t *testing.T) {
		_, err := computeSHA256(filepath.Join(tmpDir, "nonexistent.txt"))
		if err == nil {
			t.Error("expected error for nonexistent file")
		}
	})

	t.Run("binary content", func(t *testing.T) {
		content := []byte{0x00, 0x01, 0x02, 0xFF, 0xFE, 0xFD}
		filePath := filepath.Join(tmpDir, "binary.bin")
		if err := os.WriteFile(filePath, content, 0644); err != nil {
			t.Fatal(err)
		}

		hash, err := computeSHA256(filePath)
		if err != nil {
			t.Fatalf("computeSHA256 error: %v", err)
		}

		if len(hash) != 64 {
			t.Errorf("hash length = %d, want 64", len(hash))
		}
	})
}

func TestVerifySHA256(t *testing.T) {
	tmpDir := t.TempDir()

	t.Run("matching hash", func(t *testing.T) {
		content := []byte{}
		expectedHash := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"

		filePath := filepath.Join(tmpDir, "match.txt")
		if err := os.WriteFile(filePath, content, 0644); err != nil {
			t.Fatal(err)
		}

		err := verifySHA256(filePath, expectedHash)
		if err != nil {
			t.Errorf("verifySHA256 unexpected error: %v", err)
		}
	})

	t.Run("case insensitive", func(t *testing.T) {
		content := []byte{}
		// Same hash but uppercase
		upperHash := "E3B0C44298FC1C149AFBF4C8996FB92427AE41E4649B934CA495991B7852B855"

		filePath := filepath.Join(tmpDir, "case.txt")
		if err := os.WriteFile(filePath, content, 0644); err != nil {
			t.Fatal(err)
		}

		err := verifySHA256(filePath, upperHash)
		if err != nil {
			t.Errorf("verifySHA256 should be case insensitive: %v", err)
		}
	})

	t.Run("mismatching hash", func(t *testing.T) {
		content := []byte("some content")
		wrongHash := "0000000000000000000000000000000000000000000000000000000000000000"

		filePath := filepath.Join(tmpDir, "mismatch.txt")
		if err := os.WriteFile(filePath, content, 0644); err != nil {
			t.Fatal(err)
		}

		err := verifySHA256(filePath, wrongHash)
		if err == nil {
			t.Error("expected error for mismatching hash")
		}
	})

	t.Run("file not found", func(t *testing.T) {
		err := verifySHA256(filepath.Join(tmpDir, "nonexistent.txt"), "abc")
		if err == nil {
			t.Error("expected error for nonexistent file")
		}
	})
}

func TestShouldSkipLocal(t *testing.T) {
	tmpDir := t.TempDir()

	t.Run("file does not exist", func(t *testing.T) {
		item := PlanItem{RelativePath: "test.txt", Size: 100}
		dst := filepath.Join(tmpDir, "nonexistent.txt")

		skip, reason, err := shouldSkipLocal(item, dst)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if skip {
			t.Error("should not skip nonexistent file")
		}
		if reason != "" {
			t.Errorf("reason = %q, want empty", reason)
		}
	})

	t.Run("size mismatch", func(t *testing.T) {
		filePath := filepath.Join(tmpDir, "sizeMismatch.txt")
		if err := os.WriteFile(filePath, []byte("hello"), 0644); err != nil {
			t.Fatal(err)
		}

		item := PlanItem{RelativePath: "test.txt", Size: 1000} // Different size
		skip, _, err := shouldSkipLocal(item, filePath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if skip {
			t.Error("should not skip when size differs")
		}
	})

	t.Run("size match non-LFS", func(t *testing.T) {
		content := []byte("hello")
		filePath := filepath.Join(tmpDir, "sizeMatch.txt")
		if err := os.WriteFile(filePath, content, 0644); err != nil {
			t.Fatal(err)
		}

		item := PlanItem{RelativePath: "test.txt", Size: int64(len(content)), LFS: false}
		skip, reason, err := shouldSkipLocal(item, filePath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !skip {
			t.Error("should skip when size matches for non-LFS")
		}
		if reason != "size match" {
			t.Errorf("reason = %q, want 'size match'", reason)
		}
	})

	t.Run("LFS with SHA256 match", func(t *testing.T) {
		content := []byte{}
		filePath := filepath.Join(tmpDir, "lfsMatch.txt")
		if err := os.WriteFile(filePath, content, 0644); err != nil {
			t.Fatal(err)
		}

		// SHA256 of empty file
		hash := "e3b0c44298fc1c149afbf4c8996fb92427ae41e4649b934ca495991b7852b855"
		item := PlanItem{RelativePath: "test.txt", Size: 0, LFS: true, SHA256: hash}

		skip, reason, err := shouldSkipLocal(item, filePath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !skip {
			t.Error("should skip when LFS SHA256 matches")
		}
		if reason != "sha256 match" {
			t.Errorf("reason = %q, want 'sha256 match'", reason)
		}
	})

	t.Run("LFS with SHA256 mismatch", func(t *testing.T) {
		content := []byte("actual content")
		filePath := filepath.Join(tmpDir, "lfsMismatch.txt")
		if err := os.WriteFile(filePath, content, 0644); err != nil {
			t.Fatal(err)
		}

		// Wrong hash but same size: the existing file provably differs from
		// the hub copy, so it is a content conflict, not a re-download.
		wrongHash := "0000000000000000000000000000000000000000000000000000000000000000"
		item := PlanItem{RelativePath: "test.txt", Size: int64(len(content)), LFS: true, SHA256: wrongHash}

		skip, _, err := shouldSkipLocal(item, filePath)
		if !errors.Is(err, errFileConflict) {
			t.Fatalf("err = %v, want errFileConflict", err)
		}
		if skip {
			t.Error("should not skip when LFS SHA256 mismatches")
		}
	})

	t.Run("LFS without SHA256 uses size", func(t *testing.T) {
		content := []byte("content here")
		filePath := filepath.Join(tmpDir, "lfsNoHash.txt")
		if err := os.WriteFile(filePath, content, 0644); err != nil {
			t.Fatal(err)
		}

		item := PlanItem{RelativePath: "test.txt", Size: int64(len(content)), LFS: true, SHA256: ""}
		skip, reason, err := shouldSkipLocal(item, filePath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if !skip {
			t.Error("should skip when size matches and no SHA256")
		}
		if reason != "size match" {
			t.Errorf("reason = %q, want 'size match'", reason)
		}
	})

	t.Run("unknown size does not skip", func(t *testing.T) {
		content := []byte("data")
		filePath := filepath.Join(tmpDir, "unknownSize.txt")
		if err := os.WriteFile(filePath, content, 0644); err != nil {
			t.Fatal(err)
		}

		item := PlanItem{RelativePath: "test.txt", Size: 0, LFS: false} // Size 0 = unknown
		skip, _, err := shouldSkipLocal(item, filePath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		// When Size is 0 (unknown), existing file exists - won't skip because size check fails
		// Actually, size 0 != 4, so it won't match
		if skip {
			t.Error("should not skip when expected size is 0")
		}
	})
}

// TestShouldSkipLocalFileConflict covers the flat-mode conflict guard: when
// the source supplies a SHA-256 and the file already at the destination
// provably differs from the hub copy, the downloader must refuse the file
// (errFileConflict) instead of silently overwriting a file the user may have
// placed there under the same name.
func TestShouldSkipLocalFileConflict(t *testing.T) {
	tmpDir := t.TempDir()

	t.Run("same size wrong sha256", func(t *testing.T) {
		content := []byte("user content")
		filePath := filepath.Join(tmpDir, "conflictSameSize.txt")
		if err := os.WriteFile(filePath, content, 0644); err != nil {
			t.Fatal(err)
		}

		wrongHash := "1111111111111111111111111111111111111111111111111111111111111111"
		item := PlanItem{RelativePath: "model.gguf", Size: int64(len(content)), SHA256: wrongHash}

		skip, reason, err := shouldSkipLocal(item, filePath)
		if !errors.Is(err, errFileConflict) {
			t.Fatalf("err = %v, want errFileConflict", err)
		}
		if skip || reason != "" {
			t.Errorf("skip=%v reason=%q, want no skip on conflict", skip, reason)
		}
	})

	t.Run("known sha256 with different size", func(t *testing.T) {
		filePath := filepath.Join(tmpDir, "conflictSize.txt")
		if err := os.WriteFile(filePath, []byte("short"), 0644); err != nil {
			t.Fatal(err)
		}

		// A known digest makes the size difference proof enough: the file
		// cannot be the hub copy and must not be overwritten.
		item := PlanItem{RelativePath: "model.gguf", Size: 1000, SHA256: "2222222222222222222222222222222222222222222222222222222222222222"}

		skip, _, err := shouldSkipLocal(item, filePath)
		if !errors.Is(err, errFileConflict) {
			t.Fatalf("err = %v, want errFileConflict", err)
		}
		if skip {
			t.Error("should not skip a conflicting file")
		}
	})

	t.Run("unknown sha256 size mismatch stays a re-download", func(t *testing.T) {
		filePath := filepath.Join(tmpDir, "noHashSize.txt")
		if err := os.WriteFile(filePath, []byte("short"), 0644); err != nil {
			t.Fatal(err)
		}

		// Without a digest there is no proof of difference: the old
		// re-download path (no skip, no error) must stay intact.
		item := PlanItem{RelativePath: "model.gguf", Size: 1000}

		skip, _, err := shouldSkipLocal(item, filePath)
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if skip {
			t.Error("should not skip when size differs")
		}
	})
}
