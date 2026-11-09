// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
)

// TestSearchErrorResponseCarriesNoProxyCredentials proves that a proxy
// configuration failure cannot put the proxy credentials into the error
// response of the search endpoint.
//
// The search endpoint is not local only and the server binds every
// interface, so a peer that can reach the port could read the credentials
// out of the response body. The proxy URL of this case is a credential that
// was not terminated by an "@": a parser reads the user as the host and the
// password as the port, and the parse failure of that URL quotes the port.
func TestSearchErrorResponseCarriesNoProxyCredentials(t *testing.T) {
	captured := installCapturingPipeline(t)

	const (
		proxyUser = "proxyuser"
		proxyPass = "proxypassSECRET"
	)

	s := newTestServerWithConfig(t, Config{
		CacheDir: t.TempDir(),
		Proxy: &hfdownloader.ProxyConfig{
			URL:        "http://" + proxyUser + ":" + proxyPass,
			NoEnvProxy: true,
		},
	})

	w := httptest.NewRecorder()
	s.handleSearch(w, httptest.NewRequest("GET", "/api/search?q=test", nil))

	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d, want %d; body=%s", w.Code, http.StatusInternalServerError, w.Body.String())
	}

	// The response body carries the reason of the failure, and the reason
	// carries no credential.
	body := w.Body.String()
	for _, secret := range []string{proxyUser, proxyPass} {
		if strings.Contains(body, secret) {
			t.Errorf("the search error response carries %q: %s", secret, body)
		}
	}
	if !strings.Contains(body, "Invalid proxy configuration") {
		t.Errorf("the search error response lost its context: %s", body)
	}

	// The log record carries the same reason, and it carries no credential
	// either.
	out := captured.String()
	for _, secret := range []string{proxyUser, proxyPass} {
		if strings.Contains(out, secret) {
			t.Errorf("the log record carries %q: %s", secret, out)
		}
	}
	if !strings.Contains(out, "invalid proxy config") {
		t.Errorf("the log record does not report the invalid proxy: %s", out)
	}
}
