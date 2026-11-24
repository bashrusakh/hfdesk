// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"context"
	"errors"
	"time"

	"github.com/bashrusakh/hfdesk/internal/server"
)

// runtimeServiceStateTimeout bounds one read of the service registration
// behind the runtime status endpoint of the Logs page.
//
// The read answers an HTTP request, so the page must not wait for a
// supervisor that does not answer: the registration of this machine is a
// local read, and a read that outlives this budget turns into the unknown
// state instead of holding the page.
const runtimeServiceStateTimeout = 5 * time.Second

// errNoHostServiceSurface reports that the host has no registration read to
// offer the runtime surface.
//
// It is a defensive answer: a host without a manager does not register the
// surface at all, so its page keeps the unknown state that the unregistered
// surface produces.
var errNoHostServiceSurface = errors.New("service: this host has no service registration to read")

// readServiceRegistrationState returns the state word of the service
// registration the host serves from, or the failure of that read.
//
// The word is the state vocabulary of the service layer. An empty word is
// passed through as it is: the runtime status turns it into its own unknown
// state, so this read never guesses a state the supervisor did not report.
func readServiceRegistrationState() (string, error) {
	open := openHostServiceManager
	if open == nil {
		return "", errNoHostServiceSurface
	}
	manager, err := open()
	if err != nil {
		return "", err
	}

	ctx, cancel := context.WithTimeout(context.Background(), runtimeServiceStateTimeout)
	defer cancel()
	status, err := manager.Status(ctx)
	if err != nil {
		return "", err
	}
	return string(status.State), nil
}

// installRuntimeServiceSurface registers the registration read of the host
// with the runtime surface of the server.
//
// It runs before the server can answer, so the Logs page of the instance
// reports the registration it was started from on its first request. The
// install action stays nil: a host that IS the service cannot install the
// service, and the page disables its install button on the 404 that answers
// the missing action.
//
// A platform whose service host has no settled registration scope leaves the
// manager factory nil, so the surface stays unregistered there and the page
// keeps the unknown state instead of a scope that was never read.
func installRuntimeServiceSurface() {
	if openHostServiceManager == nil {
		return
	}
	server.SetRuntimeService(readServiceRegistrationState, nil)
}
