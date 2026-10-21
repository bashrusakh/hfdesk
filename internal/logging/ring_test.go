// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package logging

import (
	"testing"
)

// ringRecord builds a record with the given sequence number.
func ringRecord(seq uint64, message string) Record {
	return Record{Seq: seq, Message: message}
}

func TestRingKeepsOnlyTheMostRecentRecords(t *testing.T) {
	ring := NewRingSink(3)
	for seq := uint64(1); seq <= 5; seq++ {
		ring.Append(ringRecord(seq, "record"))
	}

	if got := ring.Len(); got != 3 {
		t.Errorf("len = %d, want 3", got)
	}
	if got := ring.Capacity(); got != 3 {
		t.Errorf("capacity = %d, want 3", got)
	}
	if ring.OldestSeq() != 3 || ring.NewestSeq() != 5 {
		t.Errorf("oldest = %d, newest = %d, want 3 and 5", ring.OldestSeq(), ring.NewestSeq())
	}

	records, truncated := ring.Records(0, OldestFirst)
	if !truncated {
		t.Errorf("expected truncated = true for a cursor that fell out of the ring")
	}
	if len(records) != 3 {
		t.Fatalf("got %d records, want 3", len(records))
	}
	if records[0].Seq != 3 || records[2].Seq != 5 {
		t.Errorf("records span %d..%d, want 3..5", records[0].Seq, records[2].Seq)
	}
}

func TestRingNeverGrowsPastItsBound(t *testing.T) {
	const capacity = 8
	ring := NewRingSink(capacity)
	for seq := uint64(1); seq <= 1000; seq++ {
		ring.Append(ringRecord(seq, "record"))
		if ring.Len() > capacity {
			t.Fatalf("ring grew to %d records, bound is %d", ring.Len(), capacity)
		}
	}
	if ring.Len() != capacity {
		t.Errorf("len = %d, want %d", ring.Len(), capacity)
	}
}

func TestRingCursorReturnsOnlyRecordsAfterTheCursor(t *testing.T) {
	ring := NewRingSink(10)
	for seq := uint64(1); seq <= 5; seq++ {
		ring.Append(ringRecord(seq, "record"))
	}

	records, truncated := ring.Records(2, OldestFirst)
	if truncated {
		t.Errorf("truncated = true, want false")
	}
	if len(records) != 3 {
		t.Fatalf("got %d records, want 3", len(records))
	}
	for i, rec := range records {
		want := uint64(3 + i)
		if rec.Seq != want {
			t.Errorf("records[%d].Seq = %d, want %d", i, rec.Seq, want)
		}
	}
}

func TestRingCursorAtTheNewestRecordYieldsNothing(t *testing.T) {
	ring := NewRingSink(10)
	for seq := uint64(1); seq <= 3; seq++ {
		ring.Append(ringRecord(seq, "record"))
	}

	records, truncated := ring.Records(3, OldestFirst)
	if len(records) != 0 {
		t.Errorf("got %d records, want 0", len(records))
	}
	if truncated {
		t.Errorf("truncated = true, want false")
	}
}

func TestRingCursorDetectsRecordsDroppedAfterTheCursor(t *testing.T) {
	ring := NewRingSink(3)
	for seq := uint64(1); seq <= 3; seq++ {
		ring.Append(ringRecord(seq, "record"))
	}

	// Records 4 and 5 push the cursor of a reader that asked for record 1
	// out of the ring.
	ring.Append(ringRecord(4, "record"))
	ring.Append(ringRecord(5, "record"))

	records, truncated := ring.Records(1, OldestFirst)
	if !truncated {
		t.Errorf("truncated = false, want true")
	}
	if len(records) != 3 {
		t.Fatalf("got %d records, want 3", len(records))
	}
	if records[0].Seq != 3 {
		t.Errorf("records[0].Seq = %d, want 3", records[0].Seq)
	}
}

func TestRingCursorInsideTheRingIsNotTruncated(t *testing.T) {
	ring := NewRingSink(3)
	for seq := uint64(1); seq <= 5; seq++ {
		ring.Append(ringRecord(seq, "record"))
	}

	records, truncated := ring.Records(4, OldestFirst)
	if truncated {
		t.Errorf("truncated = true, want false")
	}
	if len(records) != 1 || records[0].Seq != 5 {
		t.Fatalf("got %d records, want 1 record with seq 5", len(records))
	}
}

func TestRingRecentFirstOrder(t *testing.T) {
	ring := NewRingSink(10)
	for seq := uint64(1); seq <= 3; seq++ {
		ring.Append(ringRecord(seq, "record"))
	}

	records, _ := ring.Records(0, RecentFirst)
	if len(records) != 3 {
		t.Fatalf("got %d records, want 3", len(records))
	}
	if records[0].Seq != 3 || records[2].Seq != 1 {
		t.Errorf("records are not recent first: %d..%d", records[0].Seq, records[2].Seq)
	}
}

func TestRingRecordsDoNotShareAttributeMaps(t *testing.T) {
	ring := NewRingSink(2)
	rec := ringRecord(1, "record")
	rec.Attrs = map[string]any{"repo": "org/model"}
	ring.Append(rec)

	records, _ := ring.Records(0, OldestFirst)
	if len(records) != 1 {
		t.Fatalf("got %d records, want 1", len(records))
	}
	records[0].Attrs["repo"] = "mutated"

	again, _ := ring.Records(0, OldestFirst)
	if again[0].Attrs["repo"] != "org/model" {
		t.Errorf("the ring state was mutated through a returned record")
	}
}

func TestPipelineRecordsUsesTheRingCursor(t *testing.T) {
	p, _ := newTestPipeline(t, Options{RingCapacity: 2})
	logger := p.Logger()
	logger.Info("first")
	logger.Info("second")
	logger.Info("third")

	records, truncated := p.Records(0, OldestFirst)
	if !truncated {
		t.Errorf("truncated = false, want true")
	}
	if len(records) != 2 {
		t.Fatalf("got %d records, want 2", len(records))
	}
	if records[0].Message != "second" {
		t.Errorf("records[0].Message = %q, want %q", records[0].Message, "second")
	}
}
