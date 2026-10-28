// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"github.com/bashrusakh/hfdesk/internal/server"
)

// The readiness probe answers what no supervisor can: a supervisor reports a
// started process, not a serving instance, so a start counts only when the HTTP
// server of the instance answers.
//
// The probe is platform neutral: it asks the bind address of the instance with
// the request of a bounded HTTP client, so every platform verifies a restart
// with the same check.

const (
	// serviceProbeTimeout bounds a single readiness probe.
	//
	// The probe talks to an instance of the same machine, so the bound is
	// short; the caller probes again within its own budget.
	serviceProbeTimeout = 2 * time.Second

	// serviceProbePath is the endpoint a readiness probe asks. Every instance
	// answers it, with the health document or with the auth challenge; the
	// value is the path the server registers, so the two cannot drift apart.
	serviceProbePath = server.HealthPath

	// serviceProbeBodyLimit bounds the body a readiness probe reads: the
	// health document is a few hundred bytes, and the bound keeps a foreign
	// program from streaming until the timeout of the probe.
	serviceProbeBodyLimit = 64 * 1024

	// serviceProbeHealthStatus is the status a healthy instance reports.
	serviceProbeHealthStatus = "ok"

	// serviceProbeAuthChallenge is the challenge the basic authentication of
	// the instance sends. The probe never sends a credential, so a protected
	// instance answers with this challenge, and only that answer proves that
	// the server on the address is the instance; the whole header is compared,
	// because a challenge of another realm describes another server.
	serviceProbeAuthChallenge = server.BasicAuthChallenge
)

// serviceReadinessProbe is the readiness probe a manager uses to verify that a
// started instance really serves: the Windows handler probes the address of its
// own start and the systemd manager the address of a restarted unit.
//
// The variable is the seam of the test: a test that drives a lifecycle with a
// fake supervisor replaces the probe, because no server of the instance answers
// then.
var serviceReadinessProbe = probeServiceReady

// probeServiceReady reports whether the address answers as the HFDesk server.
//
// The probe proves the instance and not merely an HTTP server: a 200 has to
// carry the health document with the healthy status and the version of the
// instance, and a 401 the exact challenge of its basic authentication, which is
// the answer a protected instance sends to a request without a credential. The
// probe never sends one: readiness is no place for the access data of the
// config file.
//
// Every other answer is a failure, and a start counts only when this probe
// accepted the instance itself.
func probeServiceReady(listen string) error {
	addr := probeServiceAddress(listen)

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
		// The instance protects its API with basic authentication. A request
		// without a credential is refused with the challenge of the instance,
		// which is the evidence that the server on the address is HFDesk.
		if challenge := resp.Header.Get("WWW-Authenticate"); challenge != serviceProbeAuthChallenge {
			return serviceNotAnswering(addr, fmt.Errorf("the health endpoint answered %s with the challenge %q instead of the challenge of the instance", resp.Status, challenge))
		}
		// The refusal carries no document of interest: drain the body.
		_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, serviceProbeBodyLimit))
		return nil
	default:
		return serviceNotAnswering(addr, fmt.Errorf("the health endpoint answered with the status %s", resp.Status))
	}
}

// serviceHealth is the part of the health document a readiness probe checks:
// the healthy status and the version. Only those two fields are required, so
// the probe never refuses an instance over a field it does not depend on.
type serviceHealth struct {
	// Status is the state of the instance: "ok" for an instance that serves.
	Status string `json:"status"`
	// Version is the version of the program of the instance.
	Version string `json:"version"`
}

// readServiceHealth reads the health document of a 200 answer within
// serviceProbeBodyLimit, so a foreign program that streams ends the read. A
// body that does not decode is a failure, not a document the instance wrote.
func readServiceHealth(body io.Reader) (serviceHealth, error) {
	var health serviceHealth
	if err := json.NewDecoder(io.LimitReader(body, serviceProbeBodyLimit)).Decode(&health); err != nil {
		return serviceHealth{}, fmt.Errorf("the health endpoint does not answer the health document of HFDesk: %w", err)
	}
	return health, nil
}

// serviceNotAnswering reports that the address answered, but not as the HFDesk
// server. The address is part of the failure, because a caller reports it next
// to the start that was not proven.
func serviceNotAnswering(addr string, reason error) error {
	return fmt.Errorf("%s does not answer as the HFDesk server: %w", addr, reason)
}

// probeServiceAddress maps the bind address onto the address the probe asks,
// using LocalAddress so the probe and the desktop browser share one rule. An
// address that cannot be mapped is used unchanged, and the request reports its
// own error.
func probeServiceAddress(listen string) string {
	addr, err := LocalAddress(listen)
	if err != nil {
		return strings.TrimSpace(listen)
	}
	return addr
}
