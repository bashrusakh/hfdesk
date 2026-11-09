// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"time"
)

// runtimeRunIDBytes is the number of random bytes a run id carries.
//
// Sixteen bytes are 128 bits, so two runs collide with a probability that
// no deployment ever reaches.
const runtimeRunIDBytes = 16

// newRuntimeRunID returns a new run id.
//
// The run id identifies the run of a server: it is generated once and it
// never changes afterwards, so it stays the same for the whole life of the
// process and it differs in another process.
//
// The value is random, so it says nothing about the machine, the user or
// the files of the host, and it is not a secret: every local client of the
// runtime endpoints reads it.
func newRuntimeRunID() string {
	var buf [runtimeRunIDBytes]byte
	if _, err := rand.Read(buf[:]); err != nil {
		// The source of randomness of the system is not available. The
		// fallback names the process and the moment the run started: it
		// says no more about the host than the process id does.
		return fmt.Sprintf("%d-%d", os.Getpid(), time.Now().UnixNano())
	}
	return hex.EncodeToString(buf[:])
}

// RunID returns the run id of the server.
//
// The run id identifies the run of the server: it is generated once, when
// the server is asked for it the first time, and it never changes
// afterwards. It is therefore stable for the whole life of the process and
// different in another process, which is what a client needs to tell a
// restart of the server from a continued run.
//
// The value is random: it carries no information about the machine, the
// user or the files of the host, and it is not a secret.
func (s *Server) RunID() string {
	s.runIDMu.Lock()
	defer s.runIDMu.Unlock()
	if s.runID == "" {
		s.runID = newRuntimeRunID()
	}
	return s.runID
}
