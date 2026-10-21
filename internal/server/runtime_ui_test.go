// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"encoding/json"
	"os"
	"os/exec"
	"testing"
)

// runtimeUIFixture is the data the runtime view harness works with.
//
// The harness drives the shipped runtime.js, so the fixture carries the
// cursors of the scenarios and the records the server holds after a restart.
type runtimeUIFixture struct {
	// Records are the log records the server holds after the restart.
	Records []map[string]any `json:"records"`
	// ClientCursor is the cursor the client holds when the server restarts.
	ClientCursor uint64 `json:"clientCursor"`
	// RestartHead is the head of the server after the restart.
	RestartHead uint64 `json:"restartHead"`
	// ContinuedHead is the head of a server that kept running.
	ContinuedHead uint64 `json:"continuedHead"`
}

// runtimeUIFixtureForTest returns the fixture of the runtime view harness.
func runtimeUIFixtureForTest() runtimeUIFixture {
	return runtimeUIFixture{
		Records: []map[string]any{
			{
				"seq":       1,
				"time":      "2026-10-21T10:00:01Z",
				"level":     "INFO",
				"component": "server",
				"message":   "server started",
				"attrs":     map[string]any{"addr": "127.0.0.1:8080"},
			},
			{
				"seq":       2,
				"time":      "2026-10-21T10:00:02Z",
				"level":     "WARN",
				"component": "jobs",
				"message":   "the queue is slow",
				"attrs":     map[string]any{"queued": 2},
			},
			{
				"seq":       3,
				"time":      "2026-10-21T10:00:03Z",
				"level":     "ERROR",
				"component": "cache",
				"message":   "the cache is not readable",
				"attrs":     map[string]any{},
			},
		},
		// The client holds the cursor of the earlier run of the server.
		ClientCursor: 100,
		// The server restarted and emitted three records.
		RestartHead: 3,
		// The server kept running and emitted more than the client saw.
		ContinuedHead: 150,
	}
}

// runRuntimeUIHarness runs the runtime view harness with the given
// scenario group.
//
// The harness needs Node: without it the test is skipped, because the
// view is browser code and only the browser can run it.
func runRuntimeUIHarness(t *testing.T, group string) {
	t.Helper()

	node, err := exec.LookPath("node")
	if err != nil {
		t.Skip("node unavailable: run testdata/runtime-ui.cjs with Node to verify the runtime view")
	}

	payload, err := json.Marshal(runtimeUIFixtureForTest())
	if err != nil {
		t.Fatalf("encode runtime UI fixture: %v", err)
	}

	cmd := exec.Command(node, "testdata/runtime-ui.cjs")
	cmd.Env = append(os.Environ(),
		"HFDESK_RUNTIME_FIXTURE="+string(payload),
		"HFDESK_RUNTIME_SCENARIO="+group)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("runtime UI regression: %v\n%s", err, out)
	}
	t.Log(string(out))
}

// TestRuntimeUIRestartResync proves that the Runtime / Logs view notices
// that the server it is talking to is a different run.
//
// The sequence numbers of the log belong to the run of the process that
// emits them and start over with every run, so a view that keeps its cursor
// across a restart of the server waits for records that will never arrive.
// The view has to notice the restart, start over and tell the user about it.
//
// Node's built-in VM drives the shipped runtime.js with DOM and fetch
// stubs, so the harness exercises the view itself and not a copy of its
// rules.
func TestRuntimeUIRestartResync(t *testing.T) {
	runRuntimeUIHarness(t, "restart")
}

// TestRuntimeUIFlushesEveryRecord proves that the Runtime / Logs view
// shows every record that is in its buffer.
//
// A flush renders a bounded number of rows, so a burst of records is
// rendered over several flushes. A flush that leaves records unrendered
// has to schedule the next batch: otherwise the records that did not fit
// into the batch stay invisible until another record arrives, and the
// count in the header reports more records than the user sees.
//
// The harness drives the shipped runtime.js, so the regression is caught
// in the view itself and not in a copy of its rules.
func TestRuntimeUIFlushesEveryRecord(t *testing.T) {
	runRuntimeUIHarness(t, "flush")
}
