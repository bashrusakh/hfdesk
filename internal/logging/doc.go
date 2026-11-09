// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

// Package logging provides the single application owned logging pipeline of
// HFDesk.
//
// # Pipeline
//
// Install builds one pipeline and installs it as the process wide slog
// default:
//
//	pipeline, err := logging.Install(logging.Options{
//		Level:   slog.LevelInfo,
//		Console: true,
//		File:    true,
//		FilePath: filepath.Join(configDir, "hfdesk.log"),
//	})
//
// Every package can then log with the standard library:
//
//	slog.Info("server started", "addr", addr)
//
// The pipeline fans every record out to three sinks:
//
//   - The console sink renders a human readable line and writes it to
//     standard error, never to standard output, so redirecting stdout keeps
//     working for scripts. It is enabled for CLI and headless runs.
//   - The file sink writes JSON lines to a rotating file. It is bounded by a
//     maximum file size and a maximum number of retained backups, and is
//     enabled for desktop runs where the logs are used for support.
//   - The ring sink keeps the most recent records in memory. It is hard
//     bounded and drops the oldest records, so the process never retains an
//     unbounded slice of log entries.
//
// # Record shape
//
// Every record carries a timestamp, a level, a component and a message.
// Structured attributes are optional. The component names the subsystem that
// produced the record, for example "server", "jobs", "download" or "cache".
//
// # Components
//
// Component returns a logger bound to a component. It resolves the current
// default logger on every call, so it must be used at the log site:
//
//	logging.Component("download").Info("started", "repo", repo)
//
// A package level variable would freeze the default logger that was installed
// when the variable was initialized, which is the standard library logger
// before Install ran. Use NewComponentLogger for a value that has to survive
// package initialization:
//
//	var downloadLog = logging.NewComponentLogger("download")
//
// # Reading records back
//
// The ring is read with a monotonic cursor. Records returns the retained
// records emitted after a cursor, oldest first or recent first:
//
//	records, truncated := pipeline.Records(cursor, logging.OldestFirst)
//
// The boolean result reports whether the requested window is no longer fully
// covered by the ring. It is true when the records after the cursor were
// already dropped, and also when the cursor is ahead of every record the
// ring ever held: the sequence numbers belong to the run of the process that
// emitted them and start over with every run, so such a cursor comes from an
// earlier run of the application. The caller cannot be served a complete
// answer for such a cursor and has to fall back to a fresh snapshot.
//
// A cursor that is exactly at the newest record is not truncated: there is
// simply nothing new after it. Cursor returns the sequence number of the
// most recently emitted record and is the cursor a reader has to send to
// continue where it stopped.
//
// # Live subscriptions
//
// Subscribe returns a live feed of the records emitted from now on, for
// example to stream them to a client:
//
//	sub := pipeline.Subscribe()
//	defer sub.Close()
//	for record := range sub.C() {
//		// send record
//	}
//
// The feed is bounded. A consumer that does not keep up loses records instead
// of blocking the pipeline; Dropped reports how many were lost.
//
// # Redaction
//
// The pipeline redacts credentials before a record reaches any sink, so no
// sink can leak one.
//
// Attribute keys are normalized (lowercased, non alphanumeric characters
// removed) and matched against a closed list of credential fragments:
// token, password, passwd, secret, apikey, authorization, credential,
// cookie, privatekey, accesskey, sessionid and bearer. The rule is
// deliberately conservative and may redact a harmless value.
//
// Free text is matched against a closed list of shapes:
//
//  1. credentials embedded in a URL: "scheme://user:password@host"
//  2. an Authorization or Proxy-Authorization value
//  3. a bare Basic or Bearer credential of at least eight characters
//  4. "<sensitive key><: or =><value>" pairs for credential names
//  5. a Hugging Face style token, "hf_" plus at least ten alphanumeric
//     characters
//
// Everything that matches is replaced with RedactedMarker.
package logging
