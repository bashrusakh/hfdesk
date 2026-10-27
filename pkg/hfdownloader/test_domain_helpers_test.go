// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Test-only helpers for the one-destination-domain ownership model
// (issue #140 / PR #151).
//
// Production always acquires the destination's ownership domain at item level
// and passes it to downloadMultipart; the claim never acquires a lock of its
// own. Direct callers in tests must therefore supply a domain of the same
// identity. These helpers acquire one explicitly and release it when the test
// ends. They are NOT production fallbacks: no production path can call them.

import (
	"context"
	"strings"
	"sync"
	"testing"
)

// testDomainIdentity derives the ownership-domain identity of dst exactly like
// the item path does: a cache destination (blobs/tmp-<name>) uses its
// name-based key and destination, any other path uses the flat/output-mode
// identity.
func testDomainIdentity(dst string) (key, lockSrc string) {
	if k, ok := cacheDstDomainKey(dst); ok {
		return k, dst
	}
	flat := newFlatDestinationDomain(dst)
	return flat.key, flat.lockSrc
}

// Test-domain reuse: one test may call downloadMultipart several times for the
// same destination (a failed publish followed by a resume, for example). The
// item path acquires the domain once per item, so the helper hands out one
// domain per (test, destination) and keeps it for the test's lifetime.
//
// A test that races several writers of ONE destination concurrently must not
// share that reused domain: it acquires a fresh domain per writer with
// acquireTestDomainFresh.
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

// acquireTestDomainFresh acquires a NEW destination ownership domain for dst
// and releases it when the test ends. Use it for each concurrent writer of one
// destination.
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

// acquireTestStagingDomain acquires the flat/output-mode destination domain
// whose multipart staging name is exactly staging. Direct claim tests name
// their staging path independently of a destination, so the domain is derived
// from the staging name.
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
