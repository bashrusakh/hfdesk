// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"strings"
	"sync"
	"sync/atomic"
)

// ComponentKey is the attribute key that carries the component name of a
// record.
const ComponentKey = "component"

// Defaults for the pipeline configuration.
const (
	// DefaultFileMaxSize is the size at which the log file rotates.
	DefaultFileMaxSize int64 = 5 << 20
	// DefaultFileMaxBackups is the number of rotated log files kept next to
	// the active file.
	DefaultFileMaxBackups = 3
	// DefaultRingCapacity is the number of records kept in the in-memory
	// ring.
	DefaultRingCapacity = 500
	// DefaultSubscriptionBuffer is the channel capacity of a subscription.
	DefaultSubscriptionBuffer = 256
)

// Options configures a Pipeline.
type Options struct {
	// Level is the minimum level that is emitted. The zero value is
	// slog.LevelInfo.
	Level slog.Level

	// Console enables the human readable console sink.
	Console bool

	// ConsoleWriter overrides the destination of the console sink. It
	// defaults to standard error, never standard output, so that redirecting
	// stdout keeps working for scripts.
	ConsoleWriter io.Writer

	// File enables the rotating file sink.
	File bool

	// FilePath is the path of the log file. It is required when File is
	// enabled.
	FilePath string

	// FileMaxSize is the size in bytes at which the log file rotates. It
	// defaults to DefaultFileMaxSize.
	FileMaxSize int64

	// FileMaxBackups is the number of rotated log files retained next to the
	// active file. Zero retains no rotated file, a negative value selects
	// DefaultFileMaxBackups.
	FileMaxBackups int

	// RingCapacity is the number of records retained in the in-memory ring.
	// Zero or less selects DefaultRingCapacity. The ring is always enabled
	// because it backs the cursor based reads.
	RingCapacity int

	// SubscriptionBuffer is the channel capacity of every subscription. It
	// defaults to DefaultSubscriptionBuffer.
	SubscriptionBuffer int
}

// Pipeline is the application owned logging pipeline.
//
// It fans one stream of records out to the console sink, the rotating file
// sink and the in-memory ring, and it feeds live subscriptions. A pipeline
// is safe for concurrent use.
type Pipeline struct {
	level   atomic.Int64
	console *ConsoleSink
	file    *FileSink
	ring    *RingSink

	// emitMu serializes the emission of records. It keeps the sequence
	// numbers, the ring order and the order of the console and file output
	// identical, which is what makes the cursor based reads meaningful.
	emitMu sync.Mutex
	seq    atomic.Uint64

	subMu     sync.Mutex
	subs      map[*Subscription]struct{}
	subBuffer int

	errMu sync.Mutex
	err   error
}

// defaultPipeline holds the pipeline installed as the process wide default.
var defaultPipeline atomic.Pointer[Pipeline]

// New builds a pipeline from opts without touching the process wide default
// logger.
func New(opts Options) (*Pipeline, error) {
	p := &Pipeline{
		subs:      make(map[*Subscription]struct{}),
		subBuffer: opts.SubscriptionBuffer,
	}
	p.level.Store(int64(opts.Level))
	if p.subBuffer <= 0 {
		p.subBuffer = DefaultSubscriptionBuffer
	}

	capacity := opts.RingCapacity
	if capacity <= 0 {
		capacity = DefaultRingCapacity
	}
	p.ring = NewRingSink(capacity)

	if opts.Console {
		writer := opts.ConsoleWriter
		if writer == nil {
			writer = DefaultConsoleWriter()
		}
		p.console = NewConsoleSink(writer)
	}

	if opts.File {
		if opts.FilePath == "" {
			return nil, fmt.Errorf("logging: file sink requires a path")
		}
		maxSize := opts.FileMaxSize
		if maxSize <= 0 {
			maxSize = DefaultFileMaxSize
		}
		maxBackups := opts.FileMaxBackups
		if maxBackups < 0 {
			maxBackups = DefaultFileMaxBackups
		}
		file, err := NewFileSink(opts.FilePath, maxSize, maxBackups)
		if err != nil {
			return nil, err
		}
		p.file = file
	}

	return p, nil
}

// Install builds a pipeline from opts and installs it as the process wide
// slog default, so that any package can simply call slog.Info, slog.Warn or
// slog.Error and have the record reach every sink.
//
// Installing also redirects the standard library log package to the pipeline,
// because slog.SetDefault does so for the standard logger.
//
// The pipeline is returned so the caller can read the ring, subscribe to
// records and close the file sink at shutdown.
func Install(opts Options) (*Pipeline, error) {
	p, err := New(opts)
	if err != nil {
		return nil, err
	}
	slog.SetDefault(slog.New(p.handler()))
	defaultPipeline.Store(p)
	return p, nil
}

// Default returns the currently installed pipeline, or nil when no pipeline
// was installed.
func Default() *Pipeline {
	return defaultPipeline.Load()
}

// Logger returns a logger that writes to this pipeline without installing it
// as the process wide default.
func (p *Pipeline) Logger() *slog.Logger {
	return slog.New(p.handler())
}

// Cursor returns the sequence number of the most recently emitted record.
//
// It is the cursor a reader has to send to get the records emitted after it.
// The cursor advances even when the ring already dropped the record it
// refers to, in which case a read reports the loss. The sequence numbers
// belong to the run of the process that emits the records and start over
// with every run, so a cursor of an earlier run is ahead of the cursor of a
// fresh pipeline and a read reports the loss as well.
func (p *Pipeline) Cursor() uint64 {
	return p.seq.Load()
}

// Records returns the retained records emitted after the given cursor.
//
// The boolean result reports whether the caller can be served a complete
// answer for that cursor.
//
// It is false when the cursor is at the newest retained record, because
// there is simply nothing new after it.
//
// It is true when the requested window is not fully covered by the ring
// anymore: either the records after the cursor were already dropped, or the
// cursor is ahead of every record the ring ever held. The second case means
// that the cursor belongs to an earlier run of the application, because the
// sequence numbers are assigned by the pipeline of the running process and
// start over with every run. The caller cannot be served a complete answer
// for such a cursor and has to start over.
func (p *Pipeline) Records(after uint64, order Order) ([]Record, bool) {
	return p.ring.Records(after, order)
}

// Subscribe returns a live feed of the records emitted from now on.
//
// The subscription is bounded: a consumer that does not keep up loses records
// instead of blocking the pipeline. Dropped reports how many were lost.
func (p *Pipeline) Subscribe() *Subscription {
	sub := newSubscription(p.subBuffer)
	sub.onClose = func() { p.removeSubscription(sub) }
	p.subMu.Lock()
	p.subs[sub] = struct{}{}
	p.subMu.Unlock()
	return sub
}

// removeSubscription drops a subscription from the pipeline.
func (p *Pipeline) removeSubscription(sub *Subscription) {
	p.subMu.Lock()
	delete(p.subs, sub)
	p.subMu.Unlock()
}

// Level returns the minimum level that is emitted.
func (p *Pipeline) Level() slog.Level {
	return slog.Level(p.level.Load())
}

// SetLevel changes the minimum level that is emitted.
func (p *Pipeline) SetLevel(level slog.Level) {
	p.level.Store(int64(level))
}

// Err returns the first error a sink reported, or nil.
func (p *Pipeline) Err() error {
	p.errMu.Lock()
	defer p.errMu.Unlock()
	return p.err
}

// recordErr stores the first error a sink reported.
func (p *Pipeline) recordErr(err error) {
	p.errMu.Lock()
	defer p.errMu.Unlock()
	if p.err == nil {
		p.err = err
	}
}

// FilePath returns the path of the rotating log file, or an empty string when
// the pipeline has no file sink.
//
// The file sink is configured once, when the pipeline is built, so the path
// never changes while the pipeline is in use.
func (p *Pipeline) FilePath() string {
	if p == nil || p.file == nil {
		return ""
	}
	return p.file.Path()
}

// Close releases the resources of the pipeline. Records emitted afterwards
// are not written to the file sink.
func (p *Pipeline) Close() error {
	if p.file != nil {
		return p.file.Close()
	}
	return nil
}

// emit fans a record out to every sink.
//
// The emission is serialized so that the sequence numbers, the ring order and
// the order of the console and file output stay identical.
func (p *Pipeline) emit(rec Record) {
	p.emitMu.Lock()
	defer p.emitMu.Unlock()

	rec.Seq = p.seq.Add(1)
	if p.console != nil {
		if err := p.console.Write(rec); err != nil {
			p.recordErr(err)
		}
	}
	if p.file != nil {
		if err := p.file.Write(rec); err != nil {
			p.recordErr(err)
		}
	}
	p.ring.Append(rec)
	p.broadcast(rec)
}

// broadcast offers a record to every subscriber without blocking the
// pipeline.
func (p *Pipeline) broadcast(rec Record) {
	p.subMu.Lock()
	subs := make([]*Subscription, 0, len(p.subs))
	for sub := range p.subs {
		subs = append(subs, sub)
	}
	p.subMu.Unlock()

	for _, sub := range subs {
		sub.send(rec)
	}
}

// buildRecord turns a slog record into a pipeline record, lifting the
// component attribute out of the attribute set and redacting the message and
// the attribute values.
func (p *Pipeline) buildRecord(r slog.Record, handlerAttrs []slog.Attr, groups []string) Record {
	attrs := make(map[string]any, r.NumAttrs()+len(handlerAttrs))
	for _, attr := range handlerAttrs {
		addAttr(attrs, attr)
	}
	r.Attrs(func(attr slog.Attr) bool {
		addAttr(attrs, groupWrap(groups, attr))
		return true
	})

	rec := Record{
		Time:    r.Time,
		Level:   r.Level,
		Message: r.Message,
		Attrs:   attrs,
	}
	if value, ok := attrs[ComponentKey]; ok {
		rec.Component = Redact(componentString(value))
		delete(attrs, ComponentKey)
	}
	rec.Message = Redact(rec.Message)
	rec.Attrs = redactAttrs(rec.Attrs)
	return rec
}

// handler returns the slog handler that feeds the pipeline.
func (p *Pipeline) handler() slog.Handler {
	return &handler{p: p}
}

// componentString renders a component attribute value as a string.
func componentString(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case fmt.Stringer:
		return typed.String()
	default:
		return fmt.Sprintf("%v", value)
	}
}

// handler is the slog handler of a pipeline. It is immutable once built:
// WithAttrs and WithGroup return a copy.
type handler struct {
	p      *Pipeline
	attrs  []slog.Attr
	groups []string
}

// Enabled reports whether the level is at or above the configured minimum.
func (h *handler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= h.p.Level()
}

// Handle emits the record through the pipeline.
func (h *handler) Handle(_ context.Context, r slog.Record) error {
	h.p.emit(h.p.buildRecord(r, h.attrs, h.groups))
	return nil
}

// WithAttrs returns a handler that adds the given attributes to every
// record.
func (h *handler) WithAttrs(attrs []slog.Attr) slog.Handler {
	if len(attrs) == 0 {
		return h
	}
	next := &handler{
		p:      h.p,
		attrs:  make([]slog.Attr, 0, len(h.attrs)+len(attrs)),
		groups: h.groups,
	}
	next.attrs = append(next.attrs, h.attrs...)
	for _, attr := range attrs {
		next.attrs = append(next.attrs, groupWrap(h.groups, attr))
	}
	return next
}

// WithGroup returns a handler that nests the following attributes in the
// named group.
func (h *handler) WithGroup(name string) slog.Handler {
	if name == "" {
		return h
	}
	groups := make([]string, 0, len(h.groups)+1)
	groups = append(groups, h.groups...)
	groups = append(groups, name)
	return &handler{
		p:      h.p,
		attrs:  h.attrs,
		groups: groups,
	}
}

// Component returns a logger that tags every record with the given component
// name.
//
// The current default logger is resolved on every call, so a record emitted
// after the pipeline was installed still reaches the pipeline.
//
// Do not store the returned logger in a package level variable: such a
// variable freezes the default logger that was installed when the variable
// was initialized, which is the standard library logger before Install ran.
// Use NewComponentLogger for a value that has to survive package
// initialization.
func Component(name string) *slog.Logger {
	return slog.Default().With(ComponentKey, name)
}

// ComponentLogger logs with a component attribute.
//
// Unlike the logger returned by Component, a ComponentLogger resolves the
// current default logger on every call, so it is safe to store one in a
// package level variable.
type ComponentLogger struct {
	component string
	args      []any
}

// NewComponentLogger returns a lazily resolved logger for the component.
func NewComponentLogger(component string) *ComponentLogger {
	return &ComponentLogger{component: component}
}

// Component returns the component name of the logger.
func (l *ComponentLogger) Component() string {
	return l.component
}

// With returns a logger that adds the given attributes to every record.
func (l *ComponentLogger) With(args ...any) *ComponentLogger {
	if len(args) == 0 {
		return l
	}
	combined := make([]any, 0, len(l.args)+len(args))
	combined = append(combined, l.args...)
	combined = append(combined, args...)
	return &ComponentLogger{component: l.component, args: combined}
}

// Log emits a record at the given level.
func (l *ComponentLogger) Log(ctx context.Context, level slog.Level, msg string, args ...any) {
	l.logger().Log(ctx, level, msg, args...)
}

// Debug emits a debug record.
func (l *ComponentLogger) Debug(msg string, args ...any) {
	l.logger().Debug(msg, args...)
}

// Info emits an info record.
func (l *ComponentLogger) Info(msg string, args ...any) {
	l.logger().Info(msg, args...)
}

// Warn emits a warning record.
func (l *ComponentLogger) Warn(msg string, args ...any) {
	l.logger().Warn(msg, args...)
}

// Error emits an error record.
func (l *ComponentLogger) Error(msg string, args ...any) {
	l.logger().Error(msg, args...)
}

// logger resolves the current default logger and binds the component.
func (l *ComponentLogger) logger() *slog.Logger {
	logger := slog.Default().With(ComponentKey, l.component)
	if len(l.args) > 0 {
		logger = logger.With(l.args...)
	}
	return logger
}

// ParseLevel converts a configuration value into a log level.
func ParseLevel(value string) (slog.Level, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn", "warning":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	default:
		return slog.LevelInfo, fmt.Errorf("logging: unknown level %q", value)
	}
}

// ensure the pipeline satisfies the slog handler contract at compile time.
var _ slog.Handler = (*handler)(nil)
