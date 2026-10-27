// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Test helpers explicitly acquire the same destination domain passed by Download.

import (
	"context"
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
	dom, err := acquireBlobDomain(context.Background(), key, lockSrc)
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
	dom, err := acquireBlobDomain(context.Background(), flatDomainKey(dst), staging)
	if err != nil {
		t.Fatalf("acquire the test staging domain of %s: %v", staging, err)
	}
	t.Cleanup(dom.release)
	return dom
}
