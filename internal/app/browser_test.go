// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"errors"
	"reflect"
	"testing"

	"github.com/bashrusakh/hfdesk/internal/opener"
)

// TestOpenBrowserUsesTheSharedOpener proves that the browser is opened
// through the shared implementation of the package that owns the platform
// behavior.
//
// The application layer has no platform behavior of its own: the mechanism
// that opens a URL lives in the opener package, so the console host, the
// desktop host and the server reach the browser the same way.
func TestOpenBrowserUsesTheSharedOpener(t *testing.T) {
	const url = "http://127.0.0.1:34567"

	// The seam of the tests is the shared implementation itself.
	if reflect.ValueOf(openURL).Pointer() != reflect.ValueOf(opener.OpenURL).Pointer() {
		t.Error("the browser of the application layer is not the shared implementation of the opener")
	}

	var opened []string
	previous := openURL
	openURL = func(target string) error {
		opened = append(opened, target)
		return nil
	}
	defer func() { openURL = previous }()

	if err := OpenBrowser(url); err != nil {
		t.Fatalf("OpenBrowser: %v", err)
	}

	if len(opened) != 1 || opened[0] != url {
		t.Errorf("opened = %v, want the url %q", opened, url)
	}
}

// TestOpenBrowserReportsTheFailureOfTheOpener proves that the failure of
// the browser reaches the caller unchanged, so every caller reports the same
// failure of the same launch.
func TestOpenBrowserReportsTheFailureOfTheOpener(t *testing.T) {
	failure := errors.New("the browser is missing")

	previous := openURL
	openURL = func(string) error { return failure }
	defer func() { openURL = previous }()

	err := OpenBrowser("http://127.0.0.1:34567")
	if err == nil {
		t.Fatal("OpenBrowser returned no error for a failed launch")
	}
	if !errors.Is(err, failure) {
		t.Errorf("error = %v, want the failure of the opener", err)
	}
}
