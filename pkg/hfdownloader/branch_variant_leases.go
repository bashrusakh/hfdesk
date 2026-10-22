// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type branchVariantLeaseMode uint8

const (
	branchVariantLeaseShared branchVariantLeaseMode = iota
	branchVariantLeaseExclusive
)

type branchVariantDirectoryScope struct {
	path string
	mode branchVariantLeaseMode
}

// AcquireBranchVariantDirectoryLease leases a physical directory and its
// existing ancestors until the returned release function is called. Shared
// leases may coexist; an exclusive lease fences recursive effects rooted at
// that directory. Parent directories are resolved before locking, so symlink
// aliases share the same OS lock objects.
func AcquireBranchVariantDirectoryLease(path string, exclusive bool) (func(), error) {
	return AcquireBranchVariantDirectoryLeaseContext(context.Background(), path, exclusive)
}

// AcquireBranchVariantDirectoryLeaseContext is the cancellable form of
// AcquireBranchVariantDirectoryLease. The context bounds the wait, so a
// caller that competes with a long-running artifact mutation can give up
// instead of staying blocked until that mutation finishes.
func AcquireBranchVariantDirectoryLeaseContext(ctx context.Context, path string, exclusive bool) (func(), error) {
	mode := branchVariantLeaseShared
	if exclusive {
		mode = branchVariantLeaseExclusive
	}
	return acquireBranchVariantDirectoryScopes(ctx, branchVariantDirectoryScope{path: path, mode: mode})
}

// AcquireBranchVariantDirectoryLeases acquires a set of directory scopes in a
// stable order. It is used for multi-root operations such as orphan cleanup.
func AcquireBranchVariantDirectoryLeases(paths []string, exclusive bool) (func(), error) {
	return AcquireBranchVariantDirectoryLeasesContext(context.Background(), paths, exclusive)
}

// AcquireBranchVariantDirectoryLeasesContext is the cancellable form of
// AcquireBranchVariantDirectoryLeases.
func AcquireBranchVariantDirectoryLeasesContext(ctx context.Context, paths []string, exclusive bool) (func(), error) {
	mode := branchVariantLeaseShared
	if exclusive {
		mode = branchVariantLeaseExclusive
	}
	scopes := make([]branchVariantDirectoryScope, 0, len(paths))
	for _, path := range paths {
		if path != "" {
			scopes = append(scopes, branchVariantDirectoryScope{path: path, mode: mode})
		}
	}
	return acquireBranchVariantDirectoryScopes(ctx, scopes...)
}

// AcquireBranchVariantArtifactLease fences one variant's control and payload
// lifecycle while allowing distinct artifacts under the same selected root to
// proceed concurrently. The service root is shared; the artifact control
// directory is the exclusive/shared scope.
func AcquireBranchVariantArtifactLease(artifact *BranchVariantArtifact, exclusive bool) (func(), error) {
	return AcquireBranchVariantArtifactLeaseContext(context.Background(), artifact, exclusive)
}

// AcquireBranchVariantArtifactLeaseContext is the cancellable form of
// AcquireBranchVariantArtifactLease.
func AcquireBranchVariantArtifactLeaseContext(ctx context.Context, artifact *BranchVariantArtifact, exclusive bool) (func(), error) {
	if artifact == nil {
		return nil, fmt.Errorf("missing branch variant artifact")
	}
	mode := branchVariantLeaseShared
	if exclusive {
		mode = branchVariantLeaseExclusive
	}
	return acquireBranchVariantDirectoryScopes(ctx,
		branchVariantDirectoryScope{path: artifact.ServiceRoot, mode: branchVariantLeaseShared},
		branchVariantDirectoryScope{path: artifact.ControlRoot, mode: mode},
	)
}

func acquireExactBranchVariantDirectoryLease(ctx context.Context, path string, mode branchVariantLeaseMode) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	physical, err := resolvedPathForCreate(path)
	if err != nil {
		return nil, err
	}
	info, err := os.Stat(physical)
	if err != nil {
		return nil, err
	}
	if !info.IsDir() {
		return nil, fmt.Errorf("lease target %q is not a directory", path)
	}
	return acquireBranchVariantOSDirectoryLock(ctx, physical, mode == branchVariantLeaseExclusive)
}

// acquireBranchVariantCreationLease serializes a claim at its nearest existing
// parent only until the service directory appears. Waiters re-resolve that
// target while contended, then lock the new stable directory itself; otherwise
// a waiter that observed a missing root could retain an exclusive parent lock
// and deadlock the creator's shared destination lease.
func acquireBranchVariantCreationLease(path string) (func(), error) {
	for {
		full, nearest, _, err := branchVariantDirectoryChain(path)
		if err != nil {
			return nil, err
		}
		if nearest == full {
			return acquireBranchVariantDirectoryScopes(nil, branchVariantDirectoryScope{
				path: full, mode: branchVariantLeaseExclusive,
			})
		}

		ctx, cancel := context.WithTimeout(context.Background(), 40*time.Millisecond)
		release, leaseErr := acquireBranchVariantDirectoryScopes(ctx, branchVariantDirectoryScope{
			path: full, mode: branchVariantLeaseExclusive,
		})
		cancel()
		if leaseErr == nil {
			if _, statErr := os.Stat(full); statErr == nil {
				release()
				continue
			} else if !errors.Is(statErr, os.ErrNotExist) {
				release()
				return nil, statErr
			}
			return release, nil
		}
		if !errors.Is(leaseErr, context.DeadlineExceeded) && !errors.Is(leaseErr, os.ErrNotExist) {
			return nil, leaseErr
		}
		if _, statErr := os.Stat(full); statErr == nil {
			continue
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return nil, statErr
		}
		timer := time.NewTimer(20 * time.Millisecond)
		<-timer.C
	}
}

func acquireBranchVariantDirectoryScopes(ctx context.Context, scopes ...branchVariantDirectoryScope) (func(), error) {
	if ctx == nil {
		ctx = context.Background()
	}
	requested := make(map[string]branchVariantLeaseMode)
	for _, scope := range scopes {
		if scope.path == "" {
			continue
		}
		full, nearest, chain, err := branchVariantDirectoryChain(scope.path)
		if err != nil {
			return nil, fmt.Errorf("resolve branch variant lease path %q: %w", scope.path, err)
		}
		target := full
		if nearest != full {
			target = nearest
		}
		for _, dir := range chain {
			mode := branchVariantLeaseShared
			if dir == target {
				mode = scope.mode
			}
			key := branchVariantDirectoryKey(dir)
			if current, ok := requested[key]; !ok || mode > current {
				requested[key] = mode
			}
		}
	}

	paths := make([]string, 0, len(requested))
	for path := range requested {
		paths = append(paths, path)
	}
	sort.Strings(paths)
	var releases []func()
	for _, path := range paths {
		release, err := acquireBranchVariantOSDirectoryLock(ctx, path, requested[path] == branchVariantLeaseExclusive)
		if err != nil {
			for i := len(releases) - 1; i >= 0; i-- {
				releases[i]()
			}
			return nil, fmt.Errorf("acquire branch variant directory lease for %q: %w", path, err)
		}
		releases = append(releases, release)
	}
	return func() {
		for i := len(releases) - 1; i >= 0; i-- {
			releases[i]()
		}
	}, nil
}

func branchVariantDirectoryChain(path string) (full, nearest string, chain []string, err error) {
	full, err = resolvedPathForCreate(path)
	if err != nil {
		return "", "", nil, err
	}
	full = filepath.Clean(full)
	nearest = full
	for {
		info, statErr := os.Stat(nearest)
		if statErr == nil {
			if !info.IsDir() {
				return "", "", nil, fmt.Errorf("%q is not a directory", nearest)
			}
			break
		}
		if !os.IsNotExist(statErr) {
			return "", "", nil, statErr
		}
		parent := filepath.Dir(nearest)
		if parent == nearest {
			return "", "", nil, fmt.Errorf("no existing directory ancestor for %q", path)
		}
		nearest = parent
	}
	for current := nearest; ; current = filepath.Dir(current) {
		chain = append(chain, current)
		if parent := filepath.Dir(current); parent == current {
			break
		}
	}
	for left, right := 0, len(chain)-1; left < right; left, right = left+1, right-1 {
		chain[left], chain[right] = chain[right], chain[left]
	}
	return full, nearest, chain, nil
}

func branchVariantDirectoryKey(path string) string {
	key := filepath.Clean(path)
	if runtimeGOOSWindows() {
		key = strings.ToLower(key)
	}
	return key
}
