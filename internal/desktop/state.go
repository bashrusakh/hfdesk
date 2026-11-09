// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bashrusakh/hfdesk/internal/service"
)

// publishState writes the published state of the desktop host. It is the
// seam of the test: the fail-closed rule needs a publish that fails on any
// machine.
var publishState = service.PublishState

// publishServingState publishes the effective URL of a desktop host whose
// server already runs — it completes the claim the launch published before
// it opened its start window — and it fails closed in both directions.
//
// The mutation guard of this session decides whether a desktop serves from
// the published state next to the singleton probe (service.LiveServingDesktop),
// so a host that serves without its state would let `hfdesk service
// install/start/restart` start the service beside it. A publish that succeeds
// therefore lets the host serve exactly as before, and a publish that fails
// stops the server through the graceful path (stop) and returns the failure,
// so nothing is served and no state is ambiguous.
func publishServingState(path, url string, stop func()) error {
	if err := publishState(path, url); err != nil {
		stop()
		return fmt.Errorf("desktop: publish the effective URL to %s: %w", path, err)
	}
	return nil
}

// reachabilityTimeout bounds the probe of the published URL.
//
// The probe runs while a second launch waits for the user, so it must be
// short: a URL that does not answer right away is a URL of an instance that
// is not there.
const reachabilityTimeout = 2 * time.Second

// publishedURL returns the URL of the running instance, or an empty string
// when no instance published one — and also when the claim it reads belongs
// to a launch that passed the service gate but has not bound its address
// yet: such a claim carries no URL until the server knows it.
//
// The published state itself is owned by the service layer beside the
// singleton hold and probe that classify a service mutation: the desktop
// host consumes it here for the second-launch display.
func publishedURL() string {
	state, ok := service.ReadPublishedState(service.PublishedStatePath())
	if !ok {
		return ""
	}
	return state.URL
}

// serverReachable reports whether the server behind the URL answers a health
// request.
//
// The second launch probes the published URL before it opens it, so a stale
// state file never sends the browser to a dead URL.
func serverReachable(url string) bool {
	target := strings.TrimRight(url, "/") + "/api/health"

	ctx, cancel := context.WithTimeout(context.Background(), reachabilityTimeout)
	defer cancel()

	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return false
	}
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		return false
	}
	defer response.Body.Close()
	_, _ = io.Copy(io.Discard, response.Body)
	return response.StatusCode == http.StatusOK
}
