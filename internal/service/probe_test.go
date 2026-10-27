// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"net"
	"net/http"
	"net/http/httptest"
	"strconv"
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

// TestProbeServiceReadyReachesAServingInstance checks the readiness probe
// against a server of the test.
//
// The server answers with the status of the basic authentication of the
// instance: the probe accepts every answer, because the point of the probe is
// that an HTTP server listens on the address and not that it answers with a
// particular code. The health endpoint of the instance is the one the probe
// asks.
func TestProbeServiceReadyReachesAServingInstance(t *testing.T) {
	path := make(chan string, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		path <- r.URL.Path
		w.WriteHeader(http.StatusUnauthorized)
	}))
	defer server.Close()

	if err := probeServiceReady(server.Listener.Addr().String()); err != nil {
		t.Fatalf("probeServiceReady(%q) = %v, want nil for a server that answers", server.Listener.Addr(), err)
	}
	if got := <-path; got != "/api/health" {
		t.Errorf("the probe asked %q, want the health endpoint of the instance", got)
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
				w.WriteHeader(http.StatusUnauthorized)
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
