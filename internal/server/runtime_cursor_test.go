// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"fmt"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/logging"
)

// inFlightWriter is a console sink writer that holds every write until it
// is released.
//
// The pipeline assigns the sequence number of a record before it hands the
// record to the console sink, and it appends the record to the ring
// afterwards. A writer that blocks in between holds a record in exactly
// that state: the sequence number is assigned, the record is not in the
// ring yet.
//
// A reader that reads the ring and the head of the pipeline in two
// independent reads sees such a record in one of them and not in the
// other. The writer turns that race into a fixed order, so a test can
// drive it on purpose.
type inFlightWriter struct {
	// entered is closed when the first write arrives.
	entered chan struct{}
	// release is closed when the writes may go on.
	release chan struct{}
	// enterOnce guards the notification of the first write.
	enterOnce sync.Once
	// releaseOnce guards the release of the writes.
	releaseOnce sync.Once
}

// newInFlightWriter returns a writer that blocks every write.
func newInFlightWriter() *inFlightWriter {
	return &inFlightWriter{
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
}

// Write holds the write until the writer is released.
func (w *inFlightWriter) Write(p []byte) (int, error) {
	w.enterOnce.Do(func() { close(w.entered) })
	<-w.release
	return len(p), nil
}

// releaseWrites lets the held writes go on.
func (w *inFlightWriter) releaseWrites() {
	w.releaseOnce.Do(func() { close(w.release) })
}

// slowWriter is a console sink writer that holds every write for a moment.
//
// It keeps a record in flight long enough for a reader to meet it, so the
// race between the ring and the head of the pipeline is met on every run
// instead of only by chance.
type slowWriter struct {
	// delay is the time every write is held.
	delay time.Duration
}

// Write holds the write for the delay of the writer.
func (w *slowWriter) Write(p []byte) (int, error) {
	time.Sleep(w.delay)
	return len(p), nil
}

// TestRuntimeLogsCursorNeverSkipsARecord proves that the cursor of a log
// page never names a record the page did not carry.
//
// The page of the endpoint and the cursor of the response have to agree:
// a client reads the cursor as the acknowledgement that everything up to
// it was delivered. A cursor that runs ahead of the page makes the client
// skip a record it never received, and the record is lost for good.
//
// The test drives the race on purpose: a record is held between the
// assignment of its sequence number and its place in the ring, so the
// reader sees the record in the head of the pipeline and not in the ring.
func TestRuntimeLogsCursorNeverSkipsARecord(t *testing.T) {
	writer := newInFlightWriter()
	t.Cleanup(writer.releaseWrites)

	pipeline, err := logging.New(logging.Options{
		Level:         slog.LevelDebug,
		Console:       true,
		ConsoleWriter: writer,
		RingCapacity:  16,
	})
	if err != nil {
		t.Fatalf("build logging pipeline: %v", err)
	}
	logger := pipeline.Logger().With(logging.ComponentKey, "test")

	handler := newRuntimeTestServer(pipeline).handler()

	// A record is emitted and stops between the assignment of its
	// sequence number and its place in the ring.
	emitted := make(chan struct{})
	go func() {
		logger.Info("record in flight")
		close(emitted)
	}()
	<-writer.entered

	// The record is in flight: the pipeline knows its sequence number,
	// the ring does not have it yet.
	page := runtimeLogsPage(t, handler, "127.0.0.1:5555", "after=0")
	if len(page.Records) != 0 {
		t.Fatalf("records = %d, want 0 while the record is in flight", len(page.Records))
	}
	if page.Cursor > 0 {
		t.Errorf("the cursor of an empty page is %d, want the cursor the client sent (0)", page.Cursor)
	}
	if page.Cursor != 0 {
		t.Errorf("the cursor of an empty page is %d, want the cursor the client sent (0)", page.Cursor)
	}

	// The record lands in the ring and is delivered: the cursor of the
	// empty page did not skip it.
	writer.releaseWrites()
	<-emitted

	page = runtimeLogsPage(t, handler, "127.0.0.1:5555", "after=0")
	if len(page.Records) != 1 {
		t.Fatalf("records = %d, want the record that was in flight", len(page.Records))
	}
	if page.Records[0].Message != "record in flight" {
		t.Errorf("record = %q, want %q", page.Records[0].Message, "record in flight")
	}
	if page.Cursor != page.Records[0].Seq {
		t.Errorf("cursor = %d, want the sequence number of the last delivered record (%d)",
			page.Cursor, page.Records[0].Seq)
	}
}

// TestRuntimeLogsCursorFollowsTheDeliveredRecords proves the cursor of a
// page that carries records: it is the sequence number of the last record
// the page delivered, and it never runs ahead of the records the client
// received.
func TestRuntimeLogsCursorFollowsTheDeliveredRecords(t *testing.T) {
	pipeline, logger := newRuntimeTestPipeline(t, 16)
	logger.Info("first")
	logger.Info("second")
	logger.Info("third")

	handler := newRuntimeTestServer(pipeline).handler()

	page := runtimeLogsPage(t, handler, "127.0.0.1:5555", "after=0")
	if len(page.Records) != 3 {
		t.Fatalf("records = %d, want 3", len(page.Records))
	}
	last := page.Records[len(page.Records)-1].Seq
	if page.Cursor != last {
		t.Errorf("cursor = %d, want the sequence number of the last delivered record (%d)", page.Cursor, last)
	}

	// A page that stops at the limit reports the last record it carried.
	page = runtimeLogsPage(t, handler, "127.0.0.1:5555", "after=0&limit=2")
	if len(page.Records) != 2 {
		t.Fatalf("records = %d, want 2", len(page.Records))
	}
	last = page.Records[len(page.Records)-1].Seq
	if page.Cursor != last {
		t.Errorf("cursor = %d, want the sequence number of the last delivered record (%d)", page.Cursor, last)
	}

	// A page without new records keeps the cursor of the client.
	page = runtimeLogsPage(t, handler, "127.0.0.1:5555", "after=3")
	if len(page.Records) != 0 {
		t.Fatalf("records = %d, want 0", len(page.Records))
	}
	if page.Cursor != 3 {
		t.Errorf("cursor = %d, want the cursor the client sent (3)", page.Cursor)
	}
}

// TestRuntimeLogsNeverSkipsARecordWhileTheServerEmits proves that a
// client that polls the endpoint while the server emits records receives
// every one of them.
//
// The union of the sequence numbers the endpoint delivered has to be
// exactly the set of the sequence numbers the server emitted: a record
// that the endpoint made the client skip is lost for good, because the
// client reads the cursor as the acknowledgement that everything up to it
// was delivered.
//
// The records are emitted slowly enough for the reader to meet a record
// that is in flight, so the race between the page and the cursor of the
// response is met on every run.
func TestRuntimeLogsNeverSkipsARecordWhileTheServerEmits(t *testing.T) {
	const (
		emitted = 120
		// idleWait is how long the client waits for a new record
		// before it gives up when the server stopped emitting.
		//
		// The wait is a duration and not a number of polls: a record
		// that is in flight leaves the ring empty, and how many
		// polls a client fits into that window depends on how long a
		// poll takes. A count of polls is not a measure of time, and a
		// client that counts them gives up before the record lands.
		idleWait = 50 * time.Millisecond
	)

	pipeline, err := logging.New(logging.Options{
		Level:         slog.LevelDebug,
		Console:       true,
		ConsoleWriter: &slowWriter{delay: time.Millisecond},
		RingCapacity:  emitted + 16,
	})
	if err != nil {
		t.Fatalf("build logging pipeline: %v", err)
	}
	logger := pipeline.Logger().With(logging.ComponentKey, "test")

	handler := newRuntimeTestServer(pipeline).handler()

	// The server emits its records while the client polls.
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < emitted; i++ {
			logger.Info("record", "index", i)
		}
	}()

	delivered := make(map[uint64]bool, emitted)
	var violations []string
	cursor := uint64(0)
	// idleSince is the moment the client last saw a new record. The zero
	// value means the client has not seen one yet.
	var idleSince time.Time
	deadline := time.Now().Add(30 * time.Second)

poll:
	for len(delivered) < emitted {
		page := runtimeLogsPage(t, handler, "127.0.0.1:5555",
			fmt.Sprintf("after=%d&limit=%d", cursor, runtimeLogsMaxLimit))

		if len(page.Records) == 0 {
			// An empty page teaches the client nothing, so the cursor
			// of the client must not move.
			if page.Cursor != cursor {
				violations = append(violations, fmt.Sprintf(
					"an empty page changed the cursor from %d to %d", cursor, page.Cursor))
			}
			if idleSince.IsZero() {
				idleSince = time.Now()
			}
		} else {
			idleSince = time.Time{}
			last := page.Records[len(page.Records)-1].Seq
			if page.Cursor != last {
				violations = append(violations, fmt.Sprintf(
					"the cursor of a page with records is %d, want the sequence number of the last delivered record (%d)",
					page.Cursor, last))
			}
			for _, record := range page.Records {
				if delivered[record.Seq] {
					violations = append(violations, fmt.Sprintf(
						"the record %d was delivered twice", record.Seq))
				}
				delivered[record.Seq] = true
			}
			cursor = page.Cursor
		}

		// The test ends when every record was delivered, or when the
		// server stopped emitting and the client sees nothing new.
		select {
		case <-done:
			if !idleSince.IsZero() && time.Since(idleSince) > idleWait {
				break poll
			}
		default:
		}
		if time.Now().After(deadline) {
			t.Fatalf("the client did not receive every record within the budget: %d of %d",
				len(delivered), emitted)
		}
	}

	// The union of the sequence numbers the endpoint delivered is exactly
	// the set of the sequence numbers the server emitted.
	for seq := uint64(1); seq <= emitted; seq++ {
		if !delivered[seq] {
			t.Errorf("the record %d was skipped by the endpoint", seq)
		}
	}
	for _, violation := range violations {
		t.Error(violation)
	}
}
