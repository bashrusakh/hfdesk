// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

// Instance tokens distinguish PID reuse and shared PID 1, but exclusion comes
// from the lifetime domain lock. Unfenced takeover requires same-instance
// identity or matching-host PID-death evidence, never record age.

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"strings"
	"time"
)

var processInstanceToken = newProcessInstanceToken()

// processHostname enables same-host PID liveness checks; empty disables them.
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

// newRandomToken uses 128 random bits as hex, falling back to PID/time text.
func newRandomToken() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err == nil {
		return hex.EncodeToString(b[:])
	}
	// Fall back to PID and time if the random source fails.
	return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
}

// stagingOwnerIsSelf compares instance tokens; PID reuse does not imply self.
func stagingOwnerIsSelf(meta *IncompleteMeta) bool {
	return meta != nil && meta.Instance != "" && meta.Instance == processInstanceToken
}

// stagingOwnerProvablyDead requires a matching host and a non-live PID.
func stagingOwnerProvablyDead(meta *IncompleteMeta) bool {
	if meta == nil || meta.PID <= 0 {
		return false
	}
	if processHostname == "" || meta.Host == "" || !strings.EqualFold(meta.Host, processHostname) {
		return false
	}
	return !isProcessAlive(meta.PID)
}
