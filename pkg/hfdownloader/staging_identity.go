// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Staging ownership identity (issue #140).
//
// A staging record's PID alone is not an ownership identity: PIDs are reused,
// and two containers can both report PID 1 while sharing a mounted cache
// directory. The record therefore carries a random per-process instance token
// written once at process start. The token distinguishes "an earlier run of
// this same process instance" (resumable, no live writer) from "a different
// process that happens to report the same PID" (foreign; only heartbeat
// freshness and a same-host liveness check may decide).
//
// The token is the record's identity, not the exclusion fence. The lifetime OS
// lock excludes current-version writers; a free lock is sufficient only for a
// LockFenced record. An unfenced record is protected unless same-host
// process evidence proves its owner dead. Publish also verifies the token under
// the local metadata gate as a claim check.
//
// PID, hostname and StartedAt remain diagnostic hints. They are never treated
// as proof of identity: a record with our PID but a different instance token is
// foreign, and a record without a hostname cannot use local PID liveness.

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"
)

// processInstanceToken uniquely identifies this process instance for the
// lifetime of the process. It is generated once, before any download can
// claim a staging file.
var processInstanceToken = newProcessInstanceToken()

// processHostname is the local hostname used to decide whether a PID recorded
// by another process instance names a process on this host. An empty value
// (hostname lookup failed) disables PID-based liveness and leaves heartbeat
// freshness as the only ownership signal.
var processHostname = func() string {
	h, err := os.Hostname()
	if err != nil {
		return ""
	}
	return h
}()

func newProcessInstanceToken() string {
	return newRandomToken()
}

// newRandomToken returns 128 random bits as hex. It seeds the process instance
// identity and other per-operation identifiers.
func newRandomToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	// crypto/rand is unavailable only in a broken environment. The token still
	// must not collide between processes, so mix in the PID and wall clock.
	return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
}

// stagingOwnerIsSelf reports whether the record belongs to this exact process
// instance. A matching token means an earlier run of this process wrote the
// record; the in-process staging registry, not the PID, distinguishes that
// case from a currently running goroutine.
func stagingOwnerIsSelf(meta *IncompleteMeta) bool {
	return meta != nil && meta.Instance != "" && meta.Instance == processInstanceToken
}

// stagingOwnerProvablyDead reports whether the record's owner is known to be
// on this host and is definitely no longer running. A record without a
// hostname cannot prove same-host identity, so PID liveness is not used for it.
func stagingOwnerProvablyDead(meta *IncompleteMeta) bool {
	if meta == nil || meta.PID <= 0 {
		return false
	}
	if processHostname == "" || meta.Host == "" || !strings.EqualFold(meta.Host, processHostname) {
		return false
	}
	return !isProcessAlive(meta.PID)
}
