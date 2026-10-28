// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"github.com/bashrusakh/hfdesk/internal/server"
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

	// serviceProbePath is the endpoint a readiness probe asks. Every
	// instance answers it: with the health document when no basic
	// authentication is configured, and with the challenge of the instance
	// when one is.
	//
	// The path is the one the server registers, so a probe and the route it
	// asks cannot drift apart.
	serviceProbePath = server.HealthPath

	// serviceProbeBodyLimit bounds the body a readiness probe reads.
	//
	// The health document of the instance is a few hundred bytes. The bound
	// keeps a foreign program on the address from answering with a stream the
	// probe would read for the whole timeout of the probe.
	serviceProbeBodyLimit = 64 * 1024

	// serviceProbeHealthStatus is the status a healthy instance reports.
	serviceProbeHealthStatus = "ok"

	// serviceProbeAuthChallenge is the challenge the basic authentication of
	// the instance sends. The probe never sends a credential, so a protected
	// instance answers the request of the probe with this challenge, and only
	// that answer proves that the server on the address is the instance.
	//
	// The value is the challenge the server writes, and the probe compares the
	// whole header with it: a challenge of another realm describes another
	// server.
	serviceProbeAuthChallenge = server.BasicAuthChallenge
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

// probeServiceReady reports whether the address answers as the HFDesk server.
//
// The probe proves the instance and not merely an HTTP server: a program that
// happens to listen on the port of the instance is not the instance that was
// asked for. The answer of the health endpoint therefore has to show which
// server answered: a 200 has to carry the health document of HFDesk, with the
// healthy status and the version of the instance, and a 401 has to carry the
// exact challenge of the basic authentication of the instance, which is the
// answer a protected instance sends to a request without a credential. The
// probe never sends one: the readiness of an instance is no place for the access
// data of its config file.
//
// Every other answer is a failure. A 200 with a body of another shape, a 401
// without the challenge of HFDesk and every other status describe a server that
// does not answer as HFDesk, and a start that was reported by the supervisor
// counts only when this probe accepted the instance itself.
func probeServiceReady(listen string) error {
	addr := probeServiceAddress(listen)
	conn, err := net.DialTimeout("tcp", addr, serviceProbeTimeout)
	if err != nil {
		return err
	}
	_ = conn.Close()

	client := &http.Client{Timeout: serviceProbeTimeout}
	resp, err := client.Get("http://" + addr + serviceProbePath)
	if err != nil {
		return err
	}
	defer resp.Body.Close()

	switch resp.StatusCode {
	case http.StatusOK:
		health, err := readServiceHealth(resp.Body)
		if err != nil {
			return serviceNotAnswering(addr, err)
		}
		if health.Status != serviceProbeHealthStatus {
			return serviceNotAnswering(addr, fmt.Errorf("the health endpoint reports the status %q", health.Status))
		}
		if health.Version == "" {
			return serviceNotAnswering(addr, fmt.Errorf("the health endpoint reports no version of the instance"))
		}
		return nil
	case http.StatusUnauthorized:
		// The instance protects its API with the basic authentication of its
		// config file. A request without a credential is refused with the
		// challenge of the instance, and that challenge is the evidence that
		// the server on the address is HFDesk.
		if challenge := resp.Header.Get("WWW-Authenticate"); challenge != serviceProbeAuthChallenge {
			return serviceNotAnswering(addr, fmt.Errorf("the health endpoint answered %s with the challenge %q instead of the challenge of the instance", resp.Status, challenge))
		}
		// The refusal carries no document of interest: the body is drained
		// within the bound, so the connection is not left half read.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, serviceProbeBodyLimit))
		return nil
	default:
		return serviceNotAnswering(addr, fmt.Errorf("the health endpoint answered with the status %s", resp.Status))
	}
}

// serviceHealth is the part of the health document of the instance a readiness
// probe checks: the healthy status and the version the instance reports.
//
// The endpoint answers more than that (the time and the endpoint of the
// instance), and a document that carries those fields as well is the document
// of HFDesk. Only the two fields every instance answers are required, so the
// probe never refuses an instance over a field it does not depend on.
type serviceHealth struct {
	// Status is the state of the instance: "ok" for an instance that serves.
	Status string `json:"status"`
	// Version is the version of the program of the instance.
	Version string `json:"version"`
}

// readServiceHealth reads the health document of a 200 answer.
//
// The body is read within serviceProbeBodyLimit, so a foreign program that
// answers with a stream ends the read instead of holding the probe. A body that
// does not decode as the document of the endpoint is a failure: the caller turns
// it into the failure of the probe, and the instance is not accepted over a body
// it never wrote.
func readServiceHealth(body io.Reader) (serviceHealth, error) {
	var health serviceHealth
	if err := json.NewDecoder(io.LimitReader(body, serviceProbeBodyLimit)).Decode(&health); err != nil {
		return serviceHealth{}, fmt.Errorf("the health endpoint does not answer the health document of HFDesk: %w", err)
	}
	return health, nil
}

// serviceNotAnswering reports that the address answered, but not as the HFDesk
// server.
//
// The address is part of the failure: the probe of a start has to say which
// address was asked, because the caller reports it next to the start that was
// not proven.
func serviceNotAnswering(addr string, reason error) error {
	return fmt.Errorf("%s does not answer as the HFDesk server: %w", addr, reason)
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
