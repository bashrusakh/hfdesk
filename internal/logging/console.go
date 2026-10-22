// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package logging

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

// timeLayout is the timestamp format used by the console and file sinks.
const timeLayout = "2006-01-02T15:04:05.000Z07:00"

// ConsoleSink writes human readable records to a writer, normally stderr.
//
// The console sink is meant for CLI and headless runs. It never writes to
// stdout, so redirecting stdout keeps working for scripts.
type ConsoleSink struct {
	mu sync.Mutex
	w  io.Writer
}

// NewConsoleSink returns a console sink writing to w.
func NewConsoleSink(w io.Writer) *ConsoleSink {
	return &ConsoleSink{w: w}
}

// Write renders the record and writes it to the sink's writer.
func (c *ConsoleSink) Write(rec Record) error {
	line := formatConsoleLine(rec)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.w == nil {
		return nil
	}
	_, err := io.WriteString(c.w, line)
	return err
}

// formatConsoleLine renders a record as a single human readable line:
//
//	2006-01-02T15:04:05.000Z INFO  [download] started repo=org/model bytes=42
func formatConsoleLine(rec Record) string {
	var b strings.Builder
	b.WriteString(rec.Time.Format(timeLayout))
	b.WriteByte(' ')
	b.WriteString(fmt.Sprintf("%-5s", rec.Level.String()))
	b.WriteByte(' ')
	if rec.Component != "" {
		b.WriteString("[" + rec.Component + "] ")
	}
	b.WriteString(rec.Message)

	keys := make([]string, 0, len(rec.Attrs))
	for key := range rec.Attrs {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		b.WriteByte(' ')
		b.WriteString(key)
		b.WriteByte('=')
		b.WriteString(formatAttrValue(rec.Attrs[key]))
	}
	b.WriteByte('\n')
	return b.String()
}

// formatAttrValue renders a single attribute value for the console sink.
func formatAttrValue(value any) string {
	switch typed := value.(type) {
	case nil:
		return "<nil>"
	case string:
		if typed == "" || strings.ContainsAny(typed, " \t\"=") {
			return strconv.Quote(typed)
		}
		return typed
	case time.Time:
		return typed.Format(timeLayout)
	case time.Duration:
		return typed.String()
	case error:
		return strconv.Quote(typed.Error())
	case map[string]any, []any:
		if encoded, err := json.Marshal(typed); err == nil {
			return string(encoded)
		}
		return fmt.Sprintf("%v", typed)
	default:
		return fmt.Sprintf("%v", typed)
	}
}

// DefaultConsoleWriter returns the writer the console sink uses when no
// writer is configured: standard error.
func DefaultConsoleWriter() io.Writer {
	return os.Stderr
}
