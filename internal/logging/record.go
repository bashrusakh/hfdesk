// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package logging

import (
	"log/slog"
	"time"
)

// Record is a single structured log record that passed through the pipeline.
//
// A record always carries a timestamp, a level, a component and a message.
// Structured attributes are optional.
type Record struct {
	// Seq is a monotonically increasing sequence number assigned by the
	// pipeline when the record is emitted. It is the cursor used by
	// Pipeline.Records: a caller asks for "records after cursor N".
	Seq uint64

	// Time is the moment the record was created.
	Time time.Time

	// Level is the severity of the record.
	Level slog.Level

	// Component names the subsystem that produced the record, for example
	// "server", "jobs", "download" or "cache". It is empty when the caller
	// did not attach one.
	Component string

	// Message is the human-readable message of the record.
	Message string

	// Attrs holds the structured attributes of the record. The component
	// attribute is lifted into Record.Component and therefore not repeated
	// here. Values are plain Go values suitable for JSON encoding; a
	// grouped attribute becomes a nested map.
	Attrs map[string]any
}

// Order selects the order in which records are returned by a read.
type Order int

const (
	// OldestFirst returns records in emission order, oldest first.
	OldestFirst Order = iota
	// RecentFirst returns records in reverse emission order, newest first.
	RecentFirst
)

// String returns the name of the order.
func (o Order) String() string {
	switch o {
	case OldestFirst:
		return "oldest-first"
	case RecentFirst:
		return "recent-first"
	default:
		return "unknown"
	}
}

// attrValue converts a resolved slog value into a plain Go value that can be
// stored in Record.Attrs.
func attrValue(v slog.Value) any {
	switch v.Kind() {
	case slog.KindTime:
		return v.Time()
	case slog.KindDuration:
		return v.Duration()
	case slog.KindBool:
		return v.Bool()
	case slog.KindInt64:
		return v.Int64()
	case slog.KindUint64:
		return v.Uint64()
	case slog.KindFloat64:
		return v.Float64()
	case slog.KindString:
		return v.String()
	case slog.KindAny:
		any := v.Any()
		if err, ok := any.(error); ok {
			return err.Error()
		}
		return any
	default:
		return v.Any()
	}
}

// groupToMap converts the attributes of a slog group into a nested map.
func groupToMap(attrs []slog.Attr) map[string]any {
	out := make(map[string]any, len(attrs))
	for _, a := range attrs {
		addAttr(out, a)
	}
	return out
}

// addAttr stores a single attribute in out. Later attributes with the same
// key replace earlier ones.
func addAttr(out map[string]any, a slog.Attr) {
	if a.Key == "" && a.Value.Kind() == slog.KindAny && a.Value.Any() == nil {
		return
	}
	value := a.Value.Resolve()
	if value.Kind() == slog.KindGroup {
		group := groupToMap(value.Group())
		if len(group) == 0 {
			return
		}
		out[a.Key] = group
		return
	}
	out[a.Key] = attrValue(value)
}

// groupWrap applies the given group stack to an attribute, producing the
// nested group structure slog expects from WithGroup.
func groupWrap(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 {
		return a
	}
	wrapped := a
	for i := len(groups) - 1; i >= 0; i-- {
		wrapped = slog.Group(groups[i], wrapped)
	}
	return wrapped
}
