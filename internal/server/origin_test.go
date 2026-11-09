// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"net/http/httptest"
	"testing"
)

func TestIsAllowedWSOrigin(t *testing.T) {
	check := func(origin, host string, allowed []string) bool {
		s := &Server{config: Config{AllowedOrigins: allowed}}
		r := httptest.NewRequest("GET", "http://"+host+"/api/ws", nil)
		r.Host = host
		if origin != "" {
			r.Header.Set("Origin", origin)
		}
		return s.isAllowedWSOrigin(r)
	}

	cases := []struct {
		name    string
		origin  string
		host    string
		allowed []string
		want    bool
	}{
		// Native clients (curl/websocat) send no Origin: always allowed.
		{"no origin", "", "localhost:8080", nil, true},
		// Same-origin web UI must keep working.
		{"same origin", "http://localhost:8080", "localhost:8080", nil, true},
		// Cross-origin with no allowlist is default-deny (the CSWSH fix).
		{"cross origin default deny", "http://evil.example", "localhost:8080", nil, false},
		// Cross-origin explicitly allowlisted is permitted.
		{"cross origin allowlisted", "http://app.example", "localhost:8080", []string{"http://app.example"}, true},
		// Wildcard opt-in is honored.
		{"wildcard", "http://anything.example", "localhost:8080", []string{"*"}, true},
		// A different cross-origin while another is allowlisted stays denied.
		{"cross origin not in list", "http://evil.example", "localhost:8080", []string{"http://app.example"}, false},
	}
	for _, c := range cases {
		if got := check(c.origin, c.host, c.allowed); got != c.want {
			t.Errorf("%s: isAllowedWSOrigin(origin=%q, host=%q, allowed=%v) = %v, want %v",
				c.name, c.origin, c.host, c.allowed, got, c.want)
		}
	}
}
