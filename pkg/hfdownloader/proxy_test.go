// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"fmt"
	"net/url"
	"strings"
	"testing"
)

// TestProxyErrorReasonDropsCredentialBearingURL proves that the reason of a
// proxy configuration failure never carries the proxy URL and never carries
// the proxy credentials.
//
// The reason travels to the log records of the callers and to the API
// responses, so a credential bearing URL must not be part of it.
func TestProxyErrorReasonDropsCredentialBearingURL(t *testing.T) {
	_, err := url.Parse("http://proxyuser:proxysecret@proxy.example:notaport")
	if err == nil {
		t.Fatal("expected the proxy URL to fail parsing")
	}

	reason := ProxyErrorReason(fmt.Errorf("invalid proxy URL: %w", err))
	if reason == "" {
		t.Fatal("the proxy error reason is empty")
	}
	for _, secret := range []string{"proxysecret", "proxyuser", "proxy.example"} {
		if strings.Contains(reason, secret) {
			t.Errorf("the proxy error reason %q still carries %q", reason, secret)
		}
	}
}

// TestProxyErrorReasonReportsTheCauseOfTheFailure proves that the reason is
// the cause of the failure and not the URL the parse failed on.
func TestProxyErrorReasonReportsTheCauseOfTheFailure(t *testing.T) {
	_, err := url.Parse("http://proxyuser:proxysecret@proxy.example:notaport")
	if err == nil {
		t.Fatal("expected the proxy URL to fail parsing")
	}

	reason := ProxyErrorReason(err)
	if !strings.Contains(reason, "port") {
		t.Errorf("the proxy error reason %q does not name the cause of the failure", reason)
	}
}

// TestProxyErrorReasonHandlesANilError proves that a failure that is not
// there has no reason: the helper reports the empty string instead of
// panicking.
func TestProxyErrorReasonHandlesANilError(t *testing.T) {
	if reason := ProxyErrorReason(nil); reason != "" {
		t.Errorf("the proxy error reason of a nil error = %q, want the empty string", reason)
	}
}
