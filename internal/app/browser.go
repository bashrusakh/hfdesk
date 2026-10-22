// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"github.com/bashrusakh/hfdesk/internal/opener"
)

// openURL hands a URL to the default browser of the operating system.
//
// The mechanism lives in the package that owns the platform behavior, so
// every caller of the browser uses the very same one.
//
// It is a package level variable so the tests can replace it: the browser of
// the machine is not the browser of a test, and the path that reports a
// failed open needs a browser that fails.
var openURL = opener.OpenURL

// OpenBrowser opens the application URL in the user's default browser.
//
// It is the URL of the running server, so it always points at the port the
// server is really listening on.
func (a *App) OpenBrowser() error {
	return OpenBrowser(a.URL())
}

// OpenBrowser opens url in the user's default browser.
//
// It is exported so a host can open an arbitrary URL if it ever needs to.
//
// The browser is started, not waited for, so opening it never blocks the
// server. The failure is reported the same way for every caller: the error
// of the opener names the URL that could not be opened.
func OpenBrowser(url string) error {
	return openURL(url)
}
