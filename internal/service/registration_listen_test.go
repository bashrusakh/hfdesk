// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"testing"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
)

// TestRegistrationListenReadsTheAddressOfTheRegistrationVector proves the
// coordination of the two sides of the readiness probe's address: the vector
// RunArgs writes names its --listen explicitly — the default included, so the
// registration never falls back to a parser default behind the probe's back —
// and registrationListen returns exactly that value, never the request value
// a caller passed in. A request without a listen is the discriminating case:
// its input is empty while the vector names the default.
func TestRegistrationListenReadsTheAddressOfTheRegistrationVector(t *testing.T) {
	base := InstallOptions{
		Paths: runtimepaths.Paths{
			ConfigFile: "/etc/hfdesk.json",
			StateDir:   "/var/lib/hfdesk",
			LogDir:     "/var/log/hfdesk",
		},
	}

	explicit := base
	explicit.Listen = "127.0.0.1:18081"
	if got := registrationListen(explicit); got != "127.0.0.1:18081" {
		t.Errorf("registrationListen of an explicit request = %q, want the requested address", got)
	}

	empty := base
	empty.Listen = ""
	if got := registrationListen(empty); got != DefaultListen {
		t.Errorf("registrationListen of a request without a listen = %q, want the default %q the vector names explicitly",
			got, DefaultListen)
	}
	for _, opts := range []InstallOptions{explicit, empty} {
		if got := registrationListen(opts); got != runArgValue(RunArgs(opts), "--listen") {
			t.Errorf("registrationListen(%q) = %q, want the --listen of the registration vector %q",
				opts.Listen, got, runArgValue(RunArgs(opts), "--listen"))
		}
	}
}
