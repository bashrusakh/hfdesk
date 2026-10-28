// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"
)

// TestProbeServiceAddressMapsTheWildcardBindOntoTheLoopback checks the address
// the readiness probe dials.
//
// A wildcard bind has no address a client can connect to: 0.0.0.0 and :: are
// the addresses every interface answers on, and a connect() to them fails with
// WSAEADDRNOTAVAIL on Windows and EADDRNOTAVAIL elsewhere. The probe therefore
// dials the loopback address of the same family, and every other host is used
// unchanged.
func TestProbeServiceAddressMapsTheWildcardBindOntoTheLoopback(t *testing.T) {
	cases := []struct {
		name   string
		listen string
		want   string
	}{
		{"the IPv4 wildcard", "0.0.0.0:8080", "127.0.0.1:8080"},
		{"the IPv6 wildcard", "[::]:8080", "[::1]:8080"},
		{"an empty host", ":8080", "127.0.0.1:8080"},
		{"an IPv4 loopback", "127.0.0.1:8080", "127.0.0.1:8080"},
		{"an IPv6 loopback", "[::1]:8080", "[::1]:8080"},
		{"an address of the network", "192.168.1.5:8080", "192.168.1.5:8080"},
		{"a host name", "localhost:8080", "localhost:8080"},
		{"no address at all", "", DefaultListen},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := probeServiceAddress(tc.listen); got != tc.want {
				t.Errorf("probeServiceAddress(%q) = %q, want %q", tc.listen, got, tc.want)
			}
		})
	}
}

// TestProbeServiceReadyAcceptsTheHealthDocumentOfTheInstance checks the
// readiness probe against the health answer of HFDesk.
//
// The endpoint answers with the healthy status and the version of the instance,
// and the probe accepts exactly that answer: the address really serves the
// instance that was started. The probe asks the health endpoint with the
// request of a client that carries no access data, so it never sends the
// credential of the config file of the instance.
func TestProbeServiceReadyAcceptsTheHealthDocumentOfTheInstance(t *testing.T) {
	path := make(chan string, 1)
	authorization := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path <- r.URL.Path
		authorization <- r.Header.Get("Authorization")
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"status":"ok","version":"1.2.3","time":"2025-01-01T00:00:00Z","endpoint":"https://huggingface.co"}`)
	}))
	defer server.Close()

	if err := probeServiceReady(server.Listener.Addr().String()); err != nil {
		t.Fatalf("probeServiceReady(%q) = %v, want nil for the health answer of the instance", server.Listener.Addr(), err)
	}
	if got := <-path; got != "/api/health" {
		t.Errorf("the probe asked %q, want the health endpoint of the instance", got)
	}
	if got := <-authorization; got != "" {
		t.Errorf("the probe sent the credential %q, want no access data at all", got)
	}
}

// TestProbeServiceReadyAcceptsTheChallengeOfTheInstance checks that a protected
// instance is accepted without a credential.
//
// The basic authentication of the instance refuses a request without a
// credential with the challenge of HFDesk. That challenge is the evidence that
// the server on the address is the instance, and the probe needs no credential
// to observe it.
func TestProbeServiceReadyAcceptsTheChallengeOfTheInstance(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("WWW-Authenticate", `Basic realm="HFDesk"`)
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	if err := probeServiceReady(server.Listener.Addr().String()); err != nil {
		t.Fatalf("probeServiceReady(%q) = %v, want nil for the challenge of the instance", server.Listener.Addr(), err)
	}
}

// TestProbeServiceReadyRejectsAServerThatIsNotTheInstance checks that the probe
// proves the instance and not merely an HTTP server.
//
// A program that listens on the port of the instance is not the instance: only
// the health document of HFDesk and the challenge of its basic authentication
// are accepted, and every other answer is a failure that names the address and
// the reason. A server that answers with a status and a body of its own is the
// state the probe exists to refuse.
func TestProbeServiceReadyRejectsAServerThatIsNotTheInstance(t *testing.T) {
	cases := []struct {
		name string
		// handler answers the request of the probe the way the foreign
		// program does.
		handler http.HandlerFunc
		// want is a fragment of the failure that names what the probe
		// refused.
		want string
	}{
		{
			name: "a 200 with a JSON document that is not the health document",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"message":"hello"}`)
			},
			want: `the status ""`,
		},
		{
			name: "a 200 without a version of the instance",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"ok","version":""}`)
			},
			want: "reports no version",
		},
		{
			name: "a 200 with a body that is no JSON at all",
			handler: func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.WriteString(w, "<!doctype html><title>another server</title>")
			},
			want: "does not answer the health document",
		},
		{
			name: "a 200 of an unhealthy instance",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"degraded","version":"1.2.3"}`)
			},
			want: `the status "degraded"`,
		},
		{
			name: "a 404 of another program",
			handler: func(w http.ResponseWriter, r *http.Request) {
				http.NotFound(w, r)
			},
			want: "404",
		},
		{
			name: "a 500 of another program",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusInternalServerError)
			},
			want: "500",
		},
		{
			name: "a 401 without the challenge of the instance",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.WriteHeader(http.StatusUnauthorized)
			},
			want: "challenge",
		},
		{
			name: "a 401 with another realm",
			handler: func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("WWW-Authenticate", `Basic realm="Admin"`)
				w.WriteHeader(http.StatusUnauthorized)
			},
			want: `realm=\"Admin\"`,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			server := httptest.NewServer(tc.handler)
			defer server.Close()

			err := probeServiceReady(server.Listener.Addr().String())
			if err == nil {
				t.Fatalf("probeServiceReady(%q) = nil, want the failure of an answer that is not the instance", server.Listener.Addr())
			}
			for _, want := range []string{"does not answer as the HFDesk server", tc.want} {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("probeServiceReady() = %q, want an error that mentions %q", err, want)
				}
			}
		})
	}
}

// TestProbeServiceReadyReportsAConnectionThatIsRefused checks the readiness
// probe of an instance that does not listen on its address.
//
// The address of the test was bound and released again, so nothing listens on
// it: the probe has to report the error instead of a ready instance.
func TestProbeServiceReadyReportsAConnectionThatIsRefused(t *testing.T) {
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("Listen() = %v, want nil", err)
	}
	addr := listener.Addr().String()
	if err := listener.Close(); err != nil {
		t.Fatalf("Close() = %v, want nil", err)
	}

	if err := probeServiceReady(addr); err == nil {
		t.Errorf("probeServiceReady(%q) = nil, want the error of the refused connection", addr)
	}
}

// TestProbeServiceReadyReachesAWildcardListener checks that the readiness probe
// reaches a server that binds every interface of the machine.
func TestProbeServiceReadyReachesAWildcardListener(t *testing.T) {
	cases := []struct {
		name string
		bind string
		host string
	}{
		{"the IPv4 wildcard", "0.0.0.0:0", "0.0.0.0"},
		{"the IPv6 wildcard", "[::]:0", "::"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			listener, err := net.Listen("tcp", tc.bind)
			if err != nil {
				t.Skipf("the address %q cannot be bound: %v", tc.bind, err)
			}
			server := &http.Server{Handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"status":"ok","version":"dev"}`)
			})}
			go func() {
				_ = server.Serve(listener)
			}()
			t.Cleanup(func() {
				_ = server.Close()
			})

			port := listener.Addr().(*net.TCPAddr).Port
			listen := net.JoinHostPort(tc.host, strconv.Itoa(port))
			// The loopback address of the family has to be reachable for
			// the check: a host without the family would report the probe
			// of a server it cannot reach.
			conn, err := net.DialTimeout("tcp", probeServiceAddress(listen), time.Second)
			if err != nil {
				t.Skipf("the loopback address of %q cannot be reached: %v", tc.bind, err)
			}
			_ = conn.Close()
			if err := probeServiceReady(listen); err != nil {
				t.Errorf("probeServiceReady(%q) = %v, want nil for a server that binds %q", listen, err, tc.bind)
			}
		})
	}
}
