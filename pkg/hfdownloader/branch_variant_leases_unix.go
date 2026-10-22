// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build linux || darwin || freebsd || openbsd || netbsd || dragonfly || illumos

package hfdownloader

import (
	"context"
	"errors"
	"os"
	"syscall"
	"time"
)

// branchVariantLeaseContentionHook is an unexported deterministic test seam
// for process-level coordination tests; production leaves it nil.
var branchVariantLeaseContentionHook func(string)

func acquireBranchVariantOSDirectoryLock(ctx context.Context, path string, exclusive bool) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	mode := syscall.LOCK_SH | syscall.LOCK_NB
	if exclusive {
		mode = syscall.LOCK_EX | syscall.LOCK_NB
	}
	contentionReported := false
	for {
		err = syscall.Flock(int(file.Fd()), mode)
		if err == nil {
			break
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EINTR) {
			_ = file.Close()
			return nil, err
		}
		if !contentionReported && branchVariantLeaseContentionHook != nil {
			contentionReported = true
			branchVariantLeaseContentionHook(path)
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			_ = file.Close()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return func() {
		_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
		_ = file.Close()
	}, nil
}

func acquireBranchVariantOSFileLock(ctx context.Context, file *os.File, exclusive bool, onWait func()) (func(), error) {
	mode := syscall.LOCK_SH | syscall.LOCK_NB
	if exclusive {
		mode = syscall.LOCK_EX | syscall.LOCK_NB
	}
	contentionReported := false
	for {
		err := syscall.Flock(int(file.Fd()), mode)
		if err == nil {
			return func() {
				_ = syscall.Flock(int(file.Fd()), syscall.LOCK_UN)
				_ = file.Close()
			}, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) && !errors.Is(err, syscall.EAGAIN) && !errors.Is(err, syscall.EINTR) {
			return nil, err
		}
		if !contentionReported && onWait != nil {
			contentionReported = true
			onWait()
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			if !timer.Stop() {
				<-timer.C
			}
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
}
