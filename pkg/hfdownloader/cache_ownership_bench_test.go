// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Ownership acquisition benchmark (cache mode, steady-state existing nodes):
// go test -run '^$' -bench BenchmarkCacheOwnershipAcquisition ./pkg/hfdownloader/ -benchtime 200x
//
// Reports wall time per item acquisition and ownership lock operations
// (flock open attempts, including refusals) per item at several concurrency
// levels. Every worker acquires from its own disjoint path pool so the only
// cross-worker contention is the shared ancestor/bootstrap key traffic real
// downloads contend on.

import (
	"context"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

const cacheOwnershipBenchPathsPerWorker = 32

func BenchmarkCacheOwnershipAcquisition(b *testing.B) {
	if !ownershipOSLockAvailable {
		b.Skip("this platform has no OS ownership lock primitive")
	}
	for _, concurrency := range []int{1, 4, 8} {
		b.Run(fmt.Sprintf("concurrency-%d", concurrency), func(b *testing.B) {
			benchCacheOwnershipAcquisition(b, concurrency)
		})
	}
}

func benchCacheOwnershipAcquisition(b *testing.B, concurrency int) {
	b.Helper()
	cacheDir := b.TempDir()
	cache := NewHFCache(cacheDir)
	repo, err := cache.Repo("owner/model", RepoTypeModel)
	if err != nil {
		b.Fatal(err)
	}
	if err := repo.EnsureDirs(); err != nil {
		b.Fatal(err)
	}
	realm, err := cache.ownershipRealmForData()
	if err != nil {
		b.Fatal(err)
	}

	// Distinct content-addressed destinations per pool slot.
	type benchItem struct {
		paths []string
	}
	pools := make([][]benchItem, concurrency)
	for w := 0; w < concurrency; w++ {
		for p := 0; p < cacheOwnershipBenchPathsPerWorker; p++ {
			idx := w*cacheOwnershipBenchPathsPerWorker + p
			sha := fmt.Sprintf("%064x", idx+1)
			dst, err := repo.blobDomainDst(sha)
			if err != nil {
				b.Fatal(err)
			}
			if _, ok := cacheDstDomainKey(dst); !ok {
				b.Fatalf("destination %q is not a cache staging name", dst)
			}
			paths := appendPartialOwnershipPaths(nil, dst)
			paths = appendUniqueOwnershipPaths(paths, repo.BlobPath(sha), repo.IncompletePath(sha), repo.IncompleteMetaPath(sha))
			pools[w] = append(pools[w], benchItem{paths: paths})
		}
	}

	// Warm-up: create every node once so the benchmark measures steady-state
	// resolution of existing nodes, not first-creation cost.
	for w := 0; w < concurrency; w++ {
		for _, item := range pools[w] {
			lease, err := realm.acquireWriterWithWait(context.Background(), item.paths, blobOwnershipWaitBudget, nil)
			if err != nil {
				b.Fatalf("warm-up acquisition: %v", err)
			}
			lease.Release()
		}
	}

	var lockOps atomic.Int64
	oldOpen := ownershipLockOpen
	ownershipLockOpen = func(path string, mode ownershipLockMode) (ownershipLockHandle, error) {
		lockOps.Add(1)
		return oldOpen(path, mode)
	}
	defer func() { ownershipLockOpen = oldOpen }()

	b.ResetTimer()
	var produced atomic.Int64
	var failed atomic.Int64
	var wg sync.WaitGroup
	for w := 0; w < concurrency; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for j := 0; ; j++ {
				if produced.Add(1) > int64(b.N) {
					return
				}
				item := pools[w][j%len(pools[w])]
				lease, err := realm.acquireWriterWithWait(context.Background(), item.paths, blobOwnershipWaitBudget, nil)
				if err != nil {
					failed.Add(1)
					return
				}
				lease.Release()
			}
		}(w)
	}
	wg.Wait()
	b.StopTimer()
	if n := failed.Load(); n > 0 {
		b.Fatalf("%d ownership acquisitions failed during the benchmark", n)
	}
	if b.N > 0 {
		b.ReportMetric(float64(lockOps.Load())/float64(b.N), "lockops/op")
	}
}
