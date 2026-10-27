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

func computeSHA256(path string) (string, error) {
	return computeSHA256Ctx(context.Background(), path)
}

// computeSHA256Ctx checks cancellation between reads and returns the context
// error if hashing is interrupted. computeSHA256 uses a background context.
// Counters record each opened-file hash attempt and its initial logical size,
// not necessarily bytes actually read when an attempt fails or is cancelled.
func computeSHA256Ctx(ctx context.Context, path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	size := int64(0)
	if fi, serr := f.Stat(); serr == nil {
		size = fi.Size()
	}
	hashPassCount.Add(1)
	hashPassBytes.Add(size)

	h := sha256.New()
	if _, err := io.Copy(h, contextReader{ctx: ctx, r: f}); err != nil {
		return "", err
	}

	return hex.EncodeToString(h.Sum(nil)), nil
}

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

func shouldSkipLocal(it PlanItem, dst string) (bool, string, error) {
	return shouldSkipLocalCtx(context.Background(), it, dst)
}

// shouldSkipLocalCtx makes the pre-download decision context-aware, so a long
// existing-file hash stops after cancellation. Unlike normal size-mode transfer,
// this skip check may hash an existing LFS destination.
func shouldSkipLocalCtx(ctx context.Context, it PlanItem, dst string) (bool, string, error) {
	fi, err := os.Stat(dst)
	if err != nil {
		return false, "", nil
	}

	if it.Size > 0 && fi.Size() != it.Size {
		return false, "", nil
	}

	if it.LFS && it.SHA256 != "" {
		if err := verifySHA256Ctx(ctx, dst, it.SHA256); err == nil {
			return true, "sha256 match", nil
		} else if verr := ctx.Err(); verr != nil {
			// Cancelled mid-verify: surface the cancellation, do not skip.
			return false, "", verr
		}
		return false, "", nil
	}

	if it.Size > 0 && fi.Size() == it.Size {
		return true, "size match", nil
	}

	return false, "", nil
}
