// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"context"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bashrusakh/hfdesk/internal/service"
)

// reachabilityTimeout bounds the probe of the published URL.
//
// The probe runs while a second launch waits for the user, so it must be
// short: a URL that does not answer right away is a URL of an instance that
// is not there.
const reachabilityTimeout = 2 * time.Second

// publishedURL returns the URL of the running instance, or an empty string
// when no instance published one.
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
