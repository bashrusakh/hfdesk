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
// An empty group runs every group in one process.
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
// The run id of the server is what tells it: the head of the log cannot,
// because it is a snapshot that is behind the records the client already
// has.
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

// TestRuntimeUIHiddenPage proves that the Runtime / Logs view still draws
// the records that arrive after the page was hidden and shown again.
//
// A page that is hidden cancels its timers, and a browser that keeps the
// page in its back/forward cache shows it again later. The handles of the
// cancelled timers have to be released with them: a handle that outlives
// its timer makes the view skip its next batch, so the records that arrive
// after the page was shown again are never drawn.
//
// The harness drives the shipped runtime.js, so the regression is caught in
// the view itself and not in a copy of its rules.
func TestRuntimeUIHiddenPage(t *testing.T) {
	runRuntimeUIHarness(t, "hidden")
}

// TestRuntimeUIHiddenPageResumesTheLiveFeed proves that the Runtime / Logs
// view resumes its live feed after the browser showed the page again.
//
// A browser that keeps the page in its back/forward cache shows it again
// when the user comes back. The view closed its live source when the page
// was hidden, so it has to start it again: the event stream when the view
// followed the stream, the fallback poll when the view fell back to
// polling. The status has to be refreshed as well, because the page was
// hidden for an unknown time.
//
// The harness drives the shipped runtime.js, so the regression is caught in
// the view itself and not in a copy of its rules.
func TestRuntimeUIHiddenPageResumesTheLiveFeed(t *testing.T) {
	runRuntimeUIHarness(t, "restore")
}

// TestRuntimeUIRunID proves that the Runtime / Logs view tells a restart of
// the server from a continued run by the run id of the server.
//
// The head of the log cannot answer that question. The status is a snapshot
// that was taken before the request itself was written to the log, so the
// head it reports is behind the records the client already has, and
// comparing the two reports a restart on every refresh. The run id of the
// server answers it: it is stable for the whole life of the process and
// different in another process. A server that does not report a run id
// leaves the run of the client unknown, which is never read as a restart.
//
// Node's built-in VM drives the shipped runtime.js with DOM and fetch
// stubs, so the harness exercises the view itself and not a copy of its
// rules.
func TestRuntimeUIRunID(t *testing.T) {
	runRuntimeUIHarness(t, "run")
}

// TestRuntimeUIAllScenarioGroups proves that the Runtime / Logs view works
// when every scenario of the harness runs in one process.
//
// The other tests run one group per process, so a state that a group leaves
// behind cannot break the group that runs after it. The harness resets the
// view between the scenarios, so running all of them in one process proves
// that the reset really restores the view: a reset that keeps the handle of
// a dropped timer makes the view skip the next batch, and the records that
// wait in the buffer are never rendered.
//
// Node's built-in VM drives the shipped runtime.js, so the harness
// exercises the view itself and not a copy of its rules.
func TestRuntimeUIAllScenarioGroups(t *testing.T) {
	runRuntimeUIHarness(t, "")
}
