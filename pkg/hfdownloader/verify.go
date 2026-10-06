// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"
	"strings"
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

// computeSHA256Ctx is computeSHA256 bounded by ctx: it aborts promptly with the
// context error if ctx is cancelled while hashing. Existing ctx-free callers
// keep using computeSHA256 (context.Background()).
func computeSHA256Ctx(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()

	h := sha256.New()
	if _, err := io.Copy(h, contextReader{ctx: ctx, r: f}); err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

// verifySHA256 computes the SHA256 of a file and compares it to expected.
func verifySHA256(path string, expected string) error {
	return verifySHA256Ctx(context.Background(), path, expected)
}

// verifySHA256Ctx is verifySHA256 bounded by ctx (see computeSHA256Ctx). It is
// used by the downloader so a cancelled job does not wait for a multi-GB
// re-hash before surfacing a sibling's permanent error.
func verifySHA256Ctx(ctx context.Context, path string, expected string) error {
	sum, err := computeSHA256Ctx(ctx, path)
	if err != nil {
		return err
	}
	if !strings.EqualFold(sum, expected) {
		return fmt.Errorf("sha256 mismatch: expected %s got %s", expected, sum)
	}
	return nil
}

// shouldSkipLocal checks if a file already exists and matches expected hash/size.
// Returns (skip, reason, error).
func shouldSkipLocal(it PlanItem, dst string) (bool, string, error) {
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
		if err := verifySHA256(dst, it.SHA256); err == nil {
			return true, "sha256 match", nil
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
