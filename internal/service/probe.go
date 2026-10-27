// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"io"
	"net"
	"net/http"
	"strings"
	"time"
)

// The readiness probe is the part of a verified start that no supervisor can
// answer: a supervisor reports that the process of the unit or of the
// registration was started, and a process is not a serving instance. The probe
// therefore asks the instance itself, and a start that was reported by the
// supervisor counts only when the HTTP server of the instance answers.
//
// The probe is platform neutral: it dials the bind address of the instance and
// it sends a request to the health endpoint of the instance, so every platform
// verifies a restart with the same check.

const (
	// serviceProbeTimeout bounds a single readiness probe.
	//
	// The probe talks to an instance of the same machine, so the bound is
	// short: a server that neither accepts the connection nor answers the
	// request within it is not ready. The caller probes again within its own
	// budget, so the bound of one probe never becomes the bound of a start.
	serviceProbeTimeout = 2 * time.Second
)

// serviceReadinessProbe is the readiness probe a manager uses to verify that a
// started instance really serves.
//
// The handler of a Windows instance probes the address of its own start, and the
// systemd manager probes the address of a restarted unit, so both platforms
// verify a start with the same check.
//
// The variable is the seam of the test: a test that drives the lifecycle of a
// manager with a fake supervisor forks no instance, so no server of the
// instance answers on the registered address. Such a test replaces the probe
// for its own run and restores it afterwards, and the verification of the
// manager is observed without a server of its own.
//
// The production value is the real probe, so a manager always asks the
// instance itself.
var serviceReadinessProbe = probeServiceReady

// probeServiceReady reports whether the HTTP server of the instance answers.
//
// Any HTTP status is accepted: the point of the probe is that an HTTP server
// listens on the address and answers, not that it answers with a particular
// code. A server that answers with the status of the basic authentication of
// the instance is a server that answers.
func probeServiceReady(listen string) error {
	addr := probeServiceAddress(listen)
	conn, err := net.DialTimeout("tcp", addr, serviceProbeTimeout)
	if err != nil {
		return err
	}
	_ = conn.Close()

	client := &http.Client{Timeout: serviceProbeTimeout}
	resp, err := client.Get("http://" + addr + "/api/health")
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return nil
}

// probeServiceAddress maps the bind address of the instance onto the address
// the readiness probe dials.
//
// A wildcard bind has no address a client can connect to: 0.0.0.0 and :: are
// the addresses every interface of the machine answers on, and a connect() to
// them fails with WSAEADDRNOTAVAIL on Windows and with EADDRNOTAVAIL
// elsewhere. The probe therefore dials the loopback address of the same
// family, which a wildcard listener accepts like any other client. Every other
// host is used unchanged.
//
// The port of the bind address is always kept: the probe has to reach the
// server of the instance and not another one.
func probeServiceAddress(listen string) string {
	addr := strings.TrimSpace(listen)
	if addr == "" {
		addr = DefaultListen
	}
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		// An address that carries no port cannot be mapped: it is used
		// unchanged, so the probe reports the error of the dial itself.
		return addr
	}
	switch host {
	case "0.0.0.0":
		host = "127.0.0.1"
	case "::":
		host = "::1"
	case "":
		// An empty host binds every interface of every family. The
		// probe uses the loopback address of the IPv4 family, which a
		// dual-stack listener accepts.
		host = "127.0.0.1"
	}
	return net.JoinHostPort(host, port)
}
