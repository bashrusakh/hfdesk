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

// The credentials of the proxy fixtures. They are distinctive, so a leak
// cannot hide behind a word that the messages of the proxy use anyway.
const (
	proxyFixtureUser = "proxyuser"
	proxyFixturePass = "proxypassSECRET"
	proxyFixtureHost = "proxy.example"
)

// TestProxyReasonAndDescriptionCarryNoCredentials proves that neither the
// reason of a proxy configuration failure nor the human readable
// description of the proxy configuration carries the proxy credentials.
//
// The reason travels to the log records and to the API responses of the
// callers, and the description is shown to the user, so neither of them may
// carry the user or the password of the proxy.
//
// The table covers every shape of a proxy URL:
//
//   - a credential that was not terminated by an "@": a parser reads the
//     user as the host and the password as the port,
//   - the same shape with the other supported schemes,
//   - a credential with an explicit host,
//   - a host with a port that is not a number,
//   - a host with a numeric port,
//   - a host without a port.
//
// The last two shapes carry no credential at all and are the ones that must
// keep their host and their port: a user has to see what was misconfigured.
func TestProxyReasonAndDescriptionCarryNoCredentials(t *testing.T) {
	cases := []struct {
		name string
		url  string
		// forbidden are the parts of the URL that must not reach the
		// reason and not the description.
		forbidden []string
		// wantDescription is the description the URL must produce.
		wantDescription string
		// wantFailure reports that the URL does not parse, so the
		// callers see a failure and a reason.
		wantFailure bool
	}{
		{
			name:            "credential without a host",
			url:             "http://" + proxyFixtureUser + ":" + proxyFixturePass,
			forbidden:       []string{proxyFixtureUser, proxyFixturePass},
			wantDescription: "http://<redacted>",
			wantFailure:     true,
		},
		{
			name:            "credential without a host over https",
			url:             "https://" + proxyFixtureUser + ":" + proxyFixturePass,
			forbidden:       []string{proxyFixtureUser, proxyFixturePass},
			wantDescription: "https://<redacted>",
			wantFailure:     true,
		},
		{
			name:            "credential without a host over socks5",
			url:             "socks5://" + proxyFixtureUser + ":" + proxyFixturePass,
			forbidden:       []string{proxyFixtureUser, proxyFixturePass},
			wantDescription: "socks5://<redacted>",
			wantFailure:     true,
		},
		{
			name:            "credential without a host over socks5h",
			url:             "socks5h://" + proxyFixtureUser + ":" + proxyFixturePass,
			forbidden:       []string{proxyFixtureUser, proxyFixturePass},
			wantDescription: "socks5h://<redacted>",
			wantFailure:     true,
		},
		{
			name:            "credential with an explicit host",
			url:             "http://" + proxyFixtureUser + ":" + proxyFixturePass + "@" + proxyFixtureHost,
			forbidden:       []string{proxyFixtureUser, proxyFixturePass},
			wantDescription: "http://" + proxyFixtureHost,
		},
		{
			// The credential of this shape is terminated by an "@", so
			// the host of the URL is known, but its port is not a
			// number. The port of that shape can be a password, so
			// nothing of the authority is shown.
			name:            "credential with an explicit host and an invalid port",
			url:             "http://" + proxyFixtureUser + ":" + proxyFixturePass + "@" + proxyFixtureHost + ":notaport",
			forbidden:       []string{proxyFixtureUser, proxyFixturePass, proxyFixtureHost, "notaport"},
			wantDescription: "http://<redacted>",
			wantFailure:     true,
		},
		{
			// The authority of this shape is a host with a port that is
			// not a number, which is what a credential without an "@"
			// looks like to a parser. The host of the shape can be the
			// user and the port can be the password, so neither of them
			// may be shown.
			name:            "host with a port that is not a number",
			url:             "http://" + proxyFixtureHost + ":notaport",
			forbidden:       []string{proxyFixtureHost, "notaport"},
			wantDescription: "http://<redacted>",
			wantFailure:     true,
		},
		{
			name:            "host with a numeric port",
			url:             "http://" + proxyFixtureHost + ":8080",
			forbidden:       []string{proxyFixtureUser, proxyFixturePass},
			wantDescription: "http://" + proxyFixtureHost + ":8080",
		},
		{
			name:            "host without a port",
			url:             "http://" + proxyFixtureHost,
			forbidden:       []string{proxyFixtureUser, proxyFixturePass},
			wantDescription: "http://" + proxyFixtureHost,
		},
		{
			// A host literal is a host like every other host: the rule
			// keeps it, so a user sees which address was configured.
			name:            "host that is an IPv6 literal with a port",
			url:             "http://[::1]:8080",
			forbidden:       []string{proxyFixtureUser, proxyFixturePass},
			wantDescription: "http://[::1]:8080",
		},
		{
			// The path of the URL is not a credential, so it is kept.
			name:            "host with a port and a path",
			url:             "http://" + proxyFixtureHost + ":8080/proxy",
			forbidden:       []string{proxyFixtureUser, proxyFixturePass},
			wantDescription: "http://" + proxyFixtureHost + ":8080/proxy",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// The reason of a failure names the cause of the failure
			// and never a part of the URL that could be a credential.
			reason := proxyParseFailureReason(tc.url)
			for _, secret := range tc.forbidden {
				if strings.Contains(reason, secret) {
					t.Errorf("the proxy error reason %q still carries %q", reason, secret)
				}
			}

			// The description of the configuration is shown to the
			// user, so it carries no credential either.
			description := GetProxyInfoString(&ProxyConfig{URL: tc.url})
			for _, secret := range tc.forbidden {
				if strings.Contains(description, secret) {
					t.Errorf("the proxy description %q still carries %q", description, secret)
				}
			}
			if description != tc.wantDescription {
				t.Errorf("the proxy description = %q, want %q", description, tc.wantDescription)
			}

			// The error of the callers carries the reason, so it
			// carries no credential either.
			_, err := BuildHTTPClient(&ProxyConfig{URL: tc.url})
			if tc.wantFailure {
				if err == nil {
					t.Fatal("expected the proxy URL to fail parsing")
				}
				for _, secret := range tc.forbidden {
					if strings.Contains(err.Error(), secret) {
						t.Errorf("the proxy error %q still carries %q", err.Error(), secret)
					}
				}
				if !strings.Contains(err.Error(), "invalid proxy URL") {
					t.Errorf("the proxy error %q lost its context", err.Error())
				}
				if reason == "" {
					t.Error("the proxy error reason of a failing URL is empty")
				}
				return
			}
			if err != nil {
				t.Fatalf("the proxy URL was rejected: %v", err)
			}
			if reason != "" {
				t.Errorf("the proxy error reason of a valid URL = %q, want the empty string", reason)
			}
		})
	}
}

// TestProxyDescriptionOfTheEnvironmentCarriesNoCredentials proves that the
// description of a proxy that comes from the environment does not carry the
// credentials of that proxy either.
//
// The environment variables of the proxy hold the same credential as the
// configured URL does.
func TestProxyDescriptionOfTheEnvironmentCarriesNoCredentials(t *testing.T) {
	t.Setenv("HTTP_PROXY", "http://"+proxyFixtureUser+":"+proxyFixturePass+"@"+proxyFixtureHost)
	t.Setenv("http_proxy", "")
	t.Setenv("HTTPS_PROXY", "")
	t.Setenv("https_proxy", "")

	description := GetProxyInfoString(&ProxyConfig{})
	for _, secret := range []string{proxyFixtureUser, proxyFixturePass} {
		if strings.Contains(description, secret) {
			t.Errorf("the proxy description %q still carries %q", description, secret)
		}
	}
	if !strings.Contains(description, proxyFixtureHost) {
		t.Errorf("the proxy description %q lost the host of the proxy", description)
	}
}

// TestProxyDescriptionOfSeparateCredentialsCarriesNoCredentials proves that
// the description of a proxy whose credentials are configured in their own
// fields carries neither the user nor the password.
//
// The credentials of those fields are the credential of the proxy just like
// the userinfo of the URL is, and the description is shown to the user.
func TestProxyDescriptionOfSeparateCredentialsCarriesNoCredentials(t *testing.T) {
	description := GetProxyInfoString(&ProxyConfig{
		URL:      "http://" + proxyFixtureHost + ":8080",
		Username: proxyFixtureUser,
		Password: proxyFixturePass,
	})
	for _, secret := range []string{proxyFixtureUser, proxyFixturePass} {
		if strings.Contains(description, secret) {
			t.Errorf("the proxy description %q still carries %q", description, secret)
		}
	}
	if !strings.Contains(description, proxyFixtureHost+":8080") {
		t.Errorf("the proxy description %q lost the host and the port of the proxy", description)
	}
}
