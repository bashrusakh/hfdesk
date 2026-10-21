// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/url"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/bashrusakh/hfdesk/internal/logging"
	"github.com/bashrusakh/hfdesk/internal/opener"
)

// Bounds of the log page size.
const (
	// runtimeLogsDefaultLimit is the number of records a log page carries
	// when the client does not ask for a specific size.
	runtimeLogsDefaultLimit = 200
	// runtimeLogsMaxLimit is the largest number of records a log page may
	// carry. A client asking for more is clamped to this bound, so a single
	// request can never produce an unbounded response.
	runtimeLogsMaxLimit = 1000
)

// runtimeLogStreamKeepAlive is the interval between the keep-alive comments
// of the log stream. It keeps an idle connection from being reaped.
const runtimeLogStreamKeepAlive = 15 * time.Second

// runtimeLogStreamActive counts the log streams that are currently open.
//
// The counter lets the server observe that a stream releases its resources
// when the client goes away; the tests use it to prove that the stream
// terminates and unsubscribes.
var runtimeLogStreamActive atomic.Int64

// RuntimeStatusResponse is the body of GET /api/runtime.
//
// It describes the running server: the application version, the runtime
// status, the browser-safe URL, the effective bound address, the active job
// counts and the cursor of the log.
type RuntimeStatusResponse struct {
	// Version is the application version.
	Version string `json:"version"`
	// Status is the runtime status of the server.
	Status string `json:"status"`
	// URL is the browser-safe URL of the running server.
	URL string `json:"url"`
	// Address is the effective address the server is bound to. It is empty
	// while the server is not bound yet.
	Address string `json:"address"`
	// ActiveJobs counts the running and the queued jobs.
	ActiveJobs RuntimeJobCounts `json:"activeJobs"`
	// LogCursor is the sequence number of the most recent log record. It
	// is the cursor a client sends to read the records emitted after it.
	LogCursor uint64 `json:"logCursor"`
	// LogFile is the path of the rotating log file. It is empty when the
	// application does not write a log file.
	LogFile string `json:"logFile"`
}

// RuntimeOpenLogsResponse is the body of POST /api/runtime/open-logs.
type RuntimeOpenLogsResponse struct {
	// OK reports that the log folder was handed to the file manager.
	OK bool `json:"ok"`
	// Path is the folder that was opened.
	Path string `json:"path"`
}

// RuntimeOpenUIResponse is the body of POST /api/runtime/open-ui.
type RuntimeOpenUIResponse struct {
	// OK reports that the web UI was handed to the default browser.
	OK bool `json:"ok"`
}

// RuntimeJobCounts counts the active jobs of the server.
type RuntimeJobCounts struct {
	// Running is the number of jobs that occupy an active slot.
	Running int `json:"running"`
	// Queued is the number of jobs that wait for a free slot.
	Queued int `json:"queued"`
}

// RuntimeLogRecord is a log record on the wire.
//
// The pipeline record is not serialized directly: its level is a slog.Level
// and would be marshaled as a number, and its attribute map may carry values
// the wire format does not define.
type RuntimeLogRecord struct {
	// Seq is the sequence number of the record. It is the cursor of the
	// record: a client reads the records emitted after it.
	Seq uint64 `json:"seq"`
	// Time is the RFC3339 timestamp of the record, in UTC.
	Time string `json:"time"`
	// Level is the severity of the record: DEBUG, INFO, WARN or ERROR.
	Level string `json:"level"`
	// Component names the subsystem that produced the record.
	Component string `json:"component"`
	// Message is the log message.
	Message string `json:"message"`
	// Attrs holds the structured attributes of the record. It is always an
	// object, never null.
	Attrs map[string]any `json:"attrs"`
}

// RuntimeLogsResponse is the body of GET /api/runtime/logs.
type RuntimeLogsResponse struct {
	// Records holds the page of records, oldest first.
	Records []RuntimeLogRecord `json:"records"`
	// Cursor is the cursor to send for the next page.
	//
	// It is the sequence number of the last record the page carried, so a
	// client never skips a record it did not receive. An empty page keeps
	// the cursor of the client: the page taught the client nothing, so
	// the cursor of the client does not move.
	Cursor uint64 `json:"cursor"`
	// Truncated reports that the caller cannot be served a complete answer
	// for the requested cursor and has to start over.
	//
	// It is true when the requested window is no longer fully covered by
	// the bounded ring, so the client must not assume that the page
	// continues the history it has. It is also true when the cursor is
	// ahead of every record the server holds: the sequence numbers of the
	// log belong to the run of the process that emits them and start over
	// with every run, so such a cursor comes from an earlier run of the
	// server.
	Truncated bool `json:"truncated"`
}

// RuntimeLogResync tells a streaming client that it missed records.
//
// The client re-syncs by reading GET /api/runtime/logs with the reported
// cursor.
type RuntimeLogResync struct {
	// Dropped is the number of records the client missed. It is zero when
	// the number is unknown, which is the case for a window that had
	// already fallen out of the bounded ring.
	Dropped uint64 `json:"dropped"`
	// Cursor is the sequence number of the last record the client
	// received.
	Cursor uint64 `json:"cursor"`
}

// runtimeLogQuery holds the parsed query parameters of the log endpoints.
type runtimeLogQuery struct {
	// after is the cursor the client last saw.
	after uint64
	// level is the minimum severity of the returned records.
	level slog.Level
	// filtered reports whether the level filter is active.
	filtered bool
	// limit is the maximum number of returned records.
	limit int
}

// handleRuntimeStatus returns the runtime status of the server.
func (s *Server) handleRuntimeStatus(w http.ResponseWriter, r *http.Request) {
	address := ""
	if addr := s.BoundAddr(); addr != nil {
		address = addr.String()
	}
	running, queued := s.ActiveJobCounts()

	var cursor uint64
	var logFile string
	if pipeline := s.logPipeline(); pipeline != nil {
		cursor = pipeline.Cursor()
		logFile = pipeline.FilePath()
	}

	writeJSON(w, http.StatusOK, RuntimeStatusResponse{
		Version:    Version,
		Status:     "ok",
		URL:        s.BrowserURL(),
		Address:    address,
		ActiveJobs: RuntimeJobCounts{Running: running, Queued: queued},
		LogCursor:  cursor,
		LogFile:    logFile,
	})
}

// openLogDirectory opens a directory with the default file manager of the
// operating system.
//
// It is a package level variable so the tests can replace it: the real
// opener starts a file manager, which a test must not do.
var openLogDirectory = opener.Open

// openBrowserURL hands a URL to the default browser of the operating
// system.
//
// It is a package level variable so the tests can replace it: the real
// opener starts a browser, which a test must not do.
var openBrowserURL = opener.Open

// handleRuntimeOpenUI opens the web UI of the running server in the default
// browser.
//
// The endpoint opens the URL of the running server and nothing else: it
// takes no URL and no path from the caller, so it can never be used to open
// another location.
func (s *Server) handleRuntimeOpenUI(w http.ResponseWriter, r *http.Request) {
	if s.BoundAddr() == nil {
		writeError(w, http.StatusNotFound, "No web UI",
			"the server is not bound yet")
		return
	}

	url := s.BrowserURL()
	if err := openBrowserURL(url); err != nil {
		serverLog.Error("could not open the web UI", "url", url, "error", err)
		writeError(w, http.StatusInternalServerError, "Could not open the web UI",
			"the operating system did not open the browser")
		return
	}

	writeJSON(w, http.StatusOK, RuntimeOpenUIResponse{OK: true})
}

// handleRuntimeOpenLogs opens the folder of the active log file with the
// default file manager of the operating system.
//
// The endpoint opens the log location of the running application and nothing
// else: it takes no path from the caller, so it can never be used to open
// another location.
func (s *Server) handleRuntimeOpenLogs(w http.ResponseWriter, r *http.Request) {
	logFile := ""
	if pipeline := s.logPipeline(); pipeline != nil {
		logFile = pipeline.FilePath()
	}
	if logFile == "" {
		writeError(w, http.StatusNotFound, "No log file",
			"the application does not write a log file")
		return
	}

	dir := filepath.Dir(logFile)
	if err := openLogDirectory(dir); err != nil {
		serverLog.Error("could not open the log folder", "dir", dir, "error", err)
		writeError(w, http.StatusInternalServerError, "Could not open the log folder",
			"the operating system did not open the log folder")
		return
	}

	writeJSON(w, http.StatusOK, RuntimeOpenLogsResponse{OK: true, Path: dir})
}

// handleRuntimeLogs returns a bounded page of the retained log records.
//
// The page is served from the bounded in-memory ring of the logging
// pipeline. The log file is never read, so a reconnect does not re-read it
// and a client can never ask for an unbounded amount of data.
//
// The cursor of the response is the sequence number of the last record the
// page carried, so a client never skips a record it did not receive. An
// empty page keeps the cursor of the client, and a cursor that belongs to
// an earlier run of the server is reported by the truncated flag, so the
// client starts over.
func (s *Server) handleRuntimeLogs(w http.ResponseWriter, r *http.Request) {
	query, err := parseRuntimeLogQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid query", err.Error())
		return
	}

	pipeline := s.logPipeline()
	if pipeline == nil {
		// No pipeline is installed yet: the endpoint still answers, with an
		// empty page and a zero cursor.
		writeJSON(w, http.StatusOK, RuntimeLogsResponse{Records: []RuntimeLogRecord{}})
		return
	}

	records, truncated := pipeline.Records(query.after, logging.OldestFirst)

	page := make([]RuntimeLogRecord, 0, len(records))
	// The cursor of the response follows the records the page consumed. An
	// empty page leaves the cursor of the client where it is: the page
	// taught the client nothing, so the cursor does not move.
	cursor := query.after
	for _, record := range records {
		// The cursor follows the records the server consumed, so a page
		// that filtered records out does not make the client scan them
		// again on the next poll.
		cursor = record.Seq
		if query.filtered && record.Level < query.level {
			continue
		}
		page = append(page, newRuntimeLogRecord(record))
		if len(page) == query.limit {
			break
		}
	}

	writeJSON(w, http.StatusOK, RuntimeLogsResponse{
		Records:   page,
		Cursor:    cursor,
		Truncated: truncated,
	})
}

// handleRuntimeLogStream streams the log records as Server-Sent Events.
//
// The stream replays the retained records after the cursor of the client
// first and then continues with the live records. It ends when the client
// goes away and releases its subscription on the way out.
func (s *Server) handleRuntimeLogStream(w http.ResponseWriter, r *http.Request) {
	query, err := parseRuntimeLogQuery(r)
	if err != nil {
		writeError(w, http.StatusBadRequest, "Invalid query", err.Error())
		return
	}
	if _, ok := w.(http.Flusher); !ok {
		writeError(w, http.StatusInternalServerError, "Streaming unsupported",
			"the response writer does not support streaming")
		return
	}

	stream := &runtimeLogStream{
		writer:     w,
		controller: http.NewResponseController(w),
		query:      query,
		pipeline:   s.logPipeline,
		position:   query.after,
	}

	// The subscription is created before the response headers are written,
	// so a client that connects and receives the headers already has a
	// live feed: no record can slip through between the replay and the
	// live records.
	pipeline := stream.attach()

	// The server arms a write deadline for every request. A long lived
	// stream must not be cut off by it.
	_ = stream.controller.SetWriteDeadline(time.Time{})

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache")
	w.Header().Set("Connection", "keep-alive")
	w.WriteHeader(http.StatusOK)
	_ = stream.controller.Flush()

	runtimeLogStreamActive.Add(1)
	// The deferred close of the subscription runs before the counter is
	// decremented, so a counter of zero means that no subscription is left
	// behind.
	defer runtimeLogStreamActive.Add(-1)
	defer stream.close()

	// The retained records are replayed first.
	if err := stream.replay(pipeline); err != nil {
		return
	}

	stream.run(r.Context())
}

// runtimeLogStream writes log records to a Server-Sent Events response.
type runtimeLogStream struct {
	// writer is the response the events are written to.
	writer http.ResponseWriter
	// controller drives the flushing and the write deadline of the
	// response.
	controller *http.ResponseController
	// query holds the parsed query parameters of the request.
	query runtimeLogQuery
	// pipeline resolves the logging pipeline to read the records from. It
	// returns nil while no pipeline is installed.
	pipeline func() *logging.Pipeline
	// source is the pipeline the subscription belongs to.
	source *logging.Pipeline
	// sub is the live subscription of the logging pipeline, nil while no
	// pipeline is installed.
	sub *logging.Subscription
	// position is the sequence number of the last record the stream
	// consumed. It is the cursor a client re-syncs from.
	position uint64
	// dropped is the number of records the subscription reported lost the
	// last time the stream checked.
	dropped uint64
}

// attach subscribes to the logging pipeline.
//
// It returns the pipeline the subscription belongs to, or nil when no
// pipeline is installed. The subscription is created before the ring is
// read, so a record emitted in between is delivered by the replay and
// skipped by the live feed, never lost and never delivered twice.
func (st *runtimeLogStream) attach() *logging.Pipeline {
	if st.sub != nil {
		return st.source
	}
	pipeline := st.pipeline()
	if pipeline == nil {
		return nil
	}
	st.source = pipeline
	st.sub = pipeline.Subscribe()
	return pipeline
}

// close releases the subscription of the stream.
func (st *runtimeLogStream) close() {
	if st.sub != nil {
		st.sub.Close()
		st.sub = nil
	}
}

// replay writes the retained records the client asked for.
//
// A nil pipeline means that no pipeline is installed, so there is nothing
// to replay.
//
// The position of the stream only follows the records the replay consumed,
// so it never names a record the client did not receive. A replay that
// delivered no record keeps the cursor of the client, and the gap event
// carries it, so the client starts over from the place it knows.
func (st *runtimeLogStream) replay(pipeline *logging.Pipeline) error {
	if pipeline == nil {
		return nil
	}
	records, truncated := pipeline.Records(st.query.after, logging.OldestFirst)
	for _, record := range records {
		st.position = record.Seq
		if st.query.filtered && record.Level < st.query.level {
			continue
		}
		if err := st.writeEvent("log", newRuntimeLogRecord(record)); err != nil {
			return err
		}
	}
	if truncated {
		// The window the client asked for is no longer fully covered by
		// the ring, or the cursor is ahead of every record the server
		// holds and belongs to an earlier run of it. The client has to
		// re-sync instead of assuming that the replay continues its
		// history.
		return st.writeEvent("gap", RuntimeLogResync{Cursor: st.position})
	}
	return nil
}

// run writes the live records until the client goes away.
func (st *runtimeLogStream) run(ctx context.Context) {
	keepAlive := time.NewTicker(runtimeLogStreamKeepAlive)
	defer keepAlive.Stop()

	for {
		var records <-chan logging.Record
		if st.sub != nil {
			records = st.sub.C()
		}

		select {
		case <-ctx.Done():
			// The client is gone: the caller closes the subscription.
			return

		case record, ok := <-records:
			if !ok {
				// The subscription was closed, the pipeline is gone.
				return
			}
			if record.Seq <= st.position {
				// The record was already delivered by the replay.
				continue
			}
			st.position = record.Seq
			if st.query.filtered && record.Level < st.query.level {
				continue
			}
			if err := st.writeEvent("log", newRuntimeLogRecord(record)); err != nil {
				return
			}
			if err := st.reportDropped(); err != nil {
				return
			}

		case <-keepAlive.C:
			if st.sub == nil {
				// The pipeline was not installed when the client
				// connected. Try again, so the stream starts as soon
				// as one is available, and replay the records the
				// client missed in the meantime.
				if pipeline := st.attach(); pipeline != nil {
					if err := st.replay(pipeline); err != nil {
						return
					}
				}
			}
			if err := st.reportDropped(); err != nil {
				return
			}
			if _, err := st.writer.Write([]byte(": keep-alive\n\n")); err != nil {
				return
			}
			_ = st.controller.Flush()
		}
	}
}

// reportDropped tells the client about the records the subscription lost.
//
// A consumer that does not keep up loses records instead of blocking the
// pipeline. The client has to know about the loss, otherwise it silently
// shows a history with a hole in it.
func (st *runtimeLogStream) reportDropped() error {
	if st.sub == nil {
		return nil
	}
	dropped := st.sub.Dropped()
	if dropped == st.dropped {
		return nil
	}
	missed := dropped - st.dropped
	st.dropped = dropped
	return st.writeEvent("dropped", RuntimeLogResync{Dropped: missed, Cursor: st.position})
}

// writeEvent writes one Server-Sent Event and flushes it.
func (st *runtimeLogStream) writeEvent(event string, payload any) error {
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(st.writer, "event: %s\ndata: %s\n\n", event, data); err != nil {
		return err
	}
	_ = st.controller.Flush()
	return nil
}

// logPipeline returns the logging pipeline the server reads its records
// from, or nil when no pipeline is installed.
//
// The pipeline is installed by the shared startup layer, which does not
// exist yet, so the server has to work without one. The provider is a field
// so the tests can install a pipeline without touching the process wide
// default.
func (s *Server) logPipeline() *logging.Pipeline {
	if s.pipelineProvider != nil {
		return s.pipelineProvider()
	}
	return logging.Default()
}

// parseRuntimeLogQuery parses the query parameters of the log endpoints.
//
//   - after: the cursor the client last saw. A value that is not a number
//     is rejected.
//   - level: the minimum severity of the returned records. An empty value
//     or "all" disables the filter, every other value must name a level.
//   - limit: the maximum number of returned records. A value above the
//     maximum is clamped to the maximum.
func parseRuntimeLogQuery(r *http.Request) (runtimeLogQuery, error) {
	query := r.URL.Query()
	parsed := runtimeLogQuery{limit: runtimeLogsDefaultLimit}

	if raw := strings.TrimSpace(query.Get("after")); raw != "" {
		cursor, err := strconv.ParseUint(raw, 10, 64)
		if err != nil {
			return parsed, fmt.Errorf("invalid cursor %q: the cursor must be a sequence number", raw)
		}
		parsed.after = cursor
	}

	if raw := strings.TrimSpace(query.Get("level")); raw != "" && !strings.EqualFold(raw, "all") {
		level, err := logging.ParseLevel(raw)
		if err != nil {
			return parsed, fmt.Errorf("invalid level %q: the level must be one of debug, info, warn, error or all", raw)
		}
		parsed.level = level
		parsed.filtered = true
	}

	if raw := strings.TrimSpace(query.Get("limit")); raw != "" {
		limit, err := strconv.Atoi(raw)
		if err != nil {
			return parsed, fmt.Errorf("invalid limit %q: the limit must be a number", raw)
		}
		if limit <= 0 {
			return parsed, fmt.Errorf("invalid limit %d: the limit must be positive", limit)
		}
		if limit > runtimeLogsMaxLimit {
			limit = runtimeLogsMaxLimit
		}
		parsed.limit = limit
	}

	return parsed, nil
}

// newRuntimeLogRecord converts a pipeline record into its wire shape.
func newRuntimeLogRecord(record logging.Record) RuntimeLogRecord {
	attrs := record.Attrs
	if attrs == nil {
		attrs = map[string]any{}
	}
	return RuntimeLogRecord{
		Seq:       record.Seq,
		Time:      record.Time.UTC().Format(time.RFC3339),
		Level:     runtimeLogLevelName(record.Level),
		Component: record.Component,
		Message:   record.Message,
		Attrs:     attrs,
	}
}

// runtimeLogLevelName maps a level onto the name used on the wire.
func runtimeLogLevelName(level slog.Level) string {
	switch {
	case level < slog.LevelInfo:
		return "DEBUG"
	case level < slog.LevelWarn:
		return "INFO"
	case level < slog.LevelError:
		return "WARN"
	default:
		return "ERROR"
	}
}

// localOnly wraps a handler with the local-only guard.
//
// The guard keeps the runtime data of the local machine away from the LAN.
// The server binds 0.0.0.0 by default, so every client on the network can
// reach the API, and the runtime data of a local host must not be readable
// from there.
//
// Two independent rules are applied and both must pass:
//
//  1. The peer address must be a loopback address (127.0.0.0/8 or ::1). The
//     peer is taken from r.RemoteAddr only. A forwarded header such as
//     X-Forwarded-For is controlled by the client and is ignored, so a
//     remote client cannot claim a loopback peer with it.
//  2. When the request carries an Origin header, its host must be a loopback
//     address (127.0.0.0/8, ::1 or the name localhost). A page on another
//     site can make the browser issue a request to 127.0.0.1, and that
//     request really does come from a loopback peer, so the peer check
//     alone does not stop it. A request without an Origin header comes from
//     a native client and is allowed.
//
// A rejected request never reaches the handler, so no runtime or log data
// is produced for it.
func (s *Server) localOnly(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !isLocalRuntimeRequest(r) {
			writeError(w, http.StatusForbidden, "Forbidden",
				"runtime data is only available to local clients")
			return
		}
		next(w, r)
	}
}

// isLocalRuntimeRequest reports whether a request may read the local
// runtime data.
//
// See localOnly for the rules.
func isLocalRuntimeRequest(r *http.Request) bool {
	if !isLoopbackHost(peerHost(r.RemoteAddr)) {
		return false
	}
	origin := r.Header.Get("Origin")
	if origin == "" {
		// A native client sends no Origin: there is no browser that
		// could have been tricked into sending the request.
		return true
	}
	return isLoopbackHost(originHost(origin))
}

// peerHost returns the host part of a peer address.
//
// An address without a port is used as it is.
func peerHost(remoteAddr string) string {
	remoteAddr = strings.TrimSpace(remoteAddr)
	if remoteAddr == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(remoteAddr); err == nil {
		return host
	}
	return remoteAddr
}

// originHost returns the host part of an Origin header.
//
// An origin that is not a URL with a host yields an empty host, which is
// never loopback.
func originHost(origin string) string {
	parsed, err := url.Parse(strings.TrimSpace(origin))
	if err != nil || parsed.Host == "" {
		return ""
	}
	if host, _, err := net.SplitHostPort(parsed.Host); err == nil {
		return host
	}
	return parsed.Host
}

// isLoopbackHost reports whether a host names the local machine.
//
// A loopback IP address (127.0.0.0/8 or ::1) and the name "localhost" are
// local. Everything else, including a host that cannot be parsed at all, is
// not.
func isLoopbackHost(host string) bool {
	host = strings.TrimSpace(host)
	if host == "" {
		return false
	}
	if ip := net.ParseIP(host); ip != nil {
		return ip.IsLoopback()
	}
	return strings.EqualFold(host, "localhost")
}
