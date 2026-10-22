// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

// Package runtimecoordination supplies the private runtime-directory
// dependency shared by HFDesk's app and downloader. Its override is an
// in-process dependency-injection seam; production callers use their OS-derived
// default and never consult environment variables for lease identity.
package runtimecoordination

import "sync"

type BaseDirProvider func() (string, error)

var (
	mu       sync.RWMutex
	override BaseDirProvider
)

// BaseDir resolves through an explicitly injected runtime context when one is
// installed, otherwise through the caller's OS-derived default provider.
func BaseDir(defaultProvider BaseDirProvider) (string, error) {
	mu.RLock()
	provider := override
	mu.RUnlock()
	if provider == nil {
		provider = defaultProvider
	}
	return provider()
}

// SetBaseDirProvider installs a process-local runtime context and returns a
// restore function. HFDesk uses this to isolate package tests under their
// fixture root without making coordinator identity depend on HOME, XDG, or
// scratch-directory environment variables.
func SetBaseDirProvider(provider BaseDirProvider) func() {
	mu.Lock()
	previous := override
	override = provider
	mu.Unlock()
	return func() {
		mu.Lock()
		override = previous
		mu.Unlock()
	}
}
