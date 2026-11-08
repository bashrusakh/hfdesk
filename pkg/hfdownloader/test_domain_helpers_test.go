// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Test helpers explicitly acquire the same destination domain passed by Download.

import (
	"context"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func testDomainIdentity(dst string) (key, lockSrc string) {
	if k, ok := cacheDstDomainKey(dst); ok {
		return k, dst
	}
	flat := newFlatDestinationDomain(dst)
	return flat.key, flat.lockSrc
}

// Cache one domain per test/destination. Concurrent-writer tests must acquire
// distinct domains with acquireTestDomainFresh instead of reusing this cache.
var testDomains = struct {
	sync.Mutex
	m map[testing.TB]map[string]*blobDomain
}{m: map[testing.TB]map[string]*blobDomain{}}

// acquireTestDomain acquires (or reuses) dst's destination ownership domain
// for a test and releases it when the test ends.
func acquireTestDomain(t testing.TB, dst string) *blobDomain {
	t.Helper()
	testDomains.Lock()
	byDst := testDomains.m[t]
	if byDst == nil {
		byDst = map[string]*blobDomain{}
		testDomains.m[t] = byDst
	}
	if dom, ok := byDst[dst]; ok {
		testDomains.Unlock()
		return dom
	}
	testDomains.Unlock()

	dom := acquireTestDomainFresh(t, dst)
	testDomains.Lock()
	byDst = testDomains.m[t]
	if byDst == nil {
		byDst = map[string]*blobDomain{}
		testDomains.m[t] = byDst
	}
	byDst[dst] = dom
	testDomains.Unlock()
	t.Cleanup(func() {
		testDomains.Lock()
		delete(byDst, dst)
		testDomains.Unlock()
	})
	return dom
}

// acquireTestDomainFresh acquires an independent domain and releases it when
// the test ends; use it instead of cached reuse for competing writers.
func acquireTestDomainFresh(t testing.TB, dst string) *blobDomain {
	t.Helper()
	key, lockSrc := testDomainIdentity(dst)
	proofPath := dst
	paths := []string{dst, lockSrc}
	var realm *ownershipRealm
	var err error
	if cacheKey, ok := cacheDstDomainKey(dst); ok {
		// Cache-domain fixtures must share the same H-derived control root as
		// Download, not the legacy per-user flat-mode realm.
		hubDir := filepath.Dir(filepath.Dir(filepath.Dir(cacheKey)))
		cache := NewHFCacheResolved(hubDir, hubDir)
		realm, err = cache.ownershipRealmForData()
		if err != nil {
			t.Fatalf("resolve the test cache-owned Hub realm: %v", err)
		}
		if canonicalSHA256(filepath.Base(cacheKey)) {
			proofPath = cacheKey
		}
		paths = append(paths, cacheKey, dst+".incomplete", cacheKey+".incomplete")
	} else {
		ownerDir, resolveErr := resolveOwnershipDir("")
		if resolveErr != nil {
			t.Fatalf("resolve the test ownership realm: %v", resolveErr)
		}
		realm, err = newOwnershipRealm(ownerDir)
		if err != nil {
			t.Fatalf("create the test ownership realm: %v", err)
		}
		proofPath = strings.TrimPrefix(key, flatDomainKeyPrefix)
		paths = append(paths, proofPath, proofPath+".incomplete")
	}
	dom, err := acquireBlobDomainScoped(context.Background(), key, lockSrc, realm, paths, proofPath)
	if err != nil {
		t.Fatalf("acquire the test destination ownership domain of %s: %v", dst, err)
	}
	t.Cleanup(dom.release)
	return dom
}

// withTestDomain fills opts.Domain with an explicitly acquired domain when the
// caller did not supply one.
func withTestDomain(t testing.TB, dst string, opts multipartOpts) multipartOpts {
	t.Helper()
	if opts.Domain == nil {
		opts.Domain = acquireTestDomain(t, dst)
	}
	return opts
}

// acquireTestStagingDomain derives the flat destination domain from an explicitly
// named staging path, for direct claim tests without a separate dst.
func acquireTestStagingDomain(t testing.TB, staging string) *blobDomain {
	t.Helper()
	dst := strings.TrimSuffix(staging, ".incomplete")
	ownerDir, err := resolveOwnershipDir("")
	if err != nil {
		t.Fatalf("resolve the test ownership realm: %v", err)
	}
	realm, err := newOwnershipRealm(ownerDir)
	if err != nil {
		t.Fatalf("create the test ownership realm: %v", err)
	}
	dom, err := acquireBlobDomainScoped(context.Background(), flatDomainKey(dst), staging, realm,
		[]string{dst, staging, staging + ".meta", staging + ".meta" + stagingRecordTempSuffix}, dst)
	if err != nil {
		t.Fatalf("acquire the test staging domain of %s: %v", staging, err)
	}
	t.Cleanup(dom.release)
	return dom
}
