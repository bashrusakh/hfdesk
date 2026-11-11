// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strconv"
	"strings"
	"time"
)

// IsValidModelName checks if the model name is in "owner/name" format.
//
// Beyond the structural check it rejects path-traversal and separator
// characters: a real HF repo ID never contains a "." or ".." path segment,
// a backslash, or a NUL. Allowing them here would let a crafted "owner/name"
// traverse the filesystem or the HF Hub URL path in the handlers and cache
// code that forward the value downstream, so this is the single chokepoint
// that all repo-accepting callers rely on.
func IsValidModelName(modelName string) bool {
	parts := strings.Split(modelName, "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" {
		return false
	}
	for _, seg := range parts {
		if seg == "." || seg == ".." || strings.ContainsAny(seg, "\\\x00") {
			return false
		}
	}
	return true
}

// validate checks that the job and settings are valid.
func validate(job Job, cfg Settings) error {
	if job.Repo == "" {
		return errors.New("missing repo")
	}
	if !IsValidModelName(job.Repo) {
		return fmt.Errorf("invalid repo id %q (expected owner/name)", job.Repo)
	}
	if job.StrictFiles {
		// Strict manual selection narrows only plan composition; reject the
		// combinations whose semantics contradict "exactly these paths",
		// before any network or plan work. Checks and their order mirror the
		// request boundary (admitStrictSelection in internal/server) so both
		// layers name the same offending entry or field with
		// ErrInvalidFileSelection. The block runs before the generic
		// branch-variant and AppendFilterSubdir rules so a strict request
		// always surfaces its own named error, matching the HTTP layer.
		if _, err := ValidateFileSelection(job.Filters); err != nil {
			return err
		}
		if len(job.Excludes) != 0 {
			return fmt.Errorf("%w: strict file selection cannot be combined with excludes", ErrInvalidFileSelection)
		}
		if job.BranchVariant {
			return fmt.Errorf("%w: strict file selection cannot be combined with a branch variant", ErrInvalidFileSelection)
		}
		if job.AppendFilterSubdir {
			return fmt.Errorf("%w: strict file selection cannot be combined with appendFilterSubdir", ErrInvalidFileSelection)
		}
	}
	if job.BranchVariant {
		// The branch-variant shape is enforced at the server's request
		// boundary too; repeating the incompatible-field check here keeps
		// direct library callers from producing a manifest that claims
		// completeness for only a subset of the branch.
		if job.IsDataset {
			return errors.New("branch variant requires a model repository")
		}
		if job.Revision == "" {
			return errors.New("branch variant requires a revision")
		}
		if len(job.Filters) != 0 || len(job.Excludes) != 0 || job.AppendFilterSubdir || job.LocalRepo != "" {
			return errors.New("branch variant does not accept filters")
		}
	}
	if job.AppendFilterSubdir {
		if err := validateAppendFilterSubdirFilters(job.Filters); err != nil {
			return err
		}
	}
	if job.ShardRanges != "" {
		// Ranges only narrow a filtered selection; a malformed expression or
		// ranges without filters fails here, before any network or plan work,
		// so PlanRepo and Download both fail closed the same way.
		if err := ValidateShardRanges(job.ShardRanges, job.Filters); err != nil {
			return err
		}
	}
	return nil
}

// shardRange is one parsed shard-range segment: the inclusive span of shard
// indices it selects and the spelling the caller wrote it with, which
// warnings reproduce verbatim.
type shardRange struct {
	start, end int
	text       string
}

// parseShardRanges parses a shard-range expression such as "1-100,120-185"
// into inclusive spans. Segments are comma-separated, whitespace around a
// segment is trimmed, and each segment is either a single index "N" or a
// span "N-M" of positive integers with N <= M; the order of segments does
// not matter. Anything else fails closed: an empty expression, an empty
// segment, a zero or negative index, a reversed span, a non-numeric token,
// and an index that does not fit an int are all ErrInvalidShardRanges.
func parseShardRanges(expr string) ([]shardRange, error) {
	expr = strings.TrimSpace(expr)
	if expr == "" {
		return nil, fmt.Errorf("%w: expression is empty", ErrInvalidShardRanges)
	}
	segments := strings.Split(expr, ",")
	ranges := make([]shardRange, 0, len(segments))
	for _, raw := range segments {
		text := strings.TrimSpace(raw)
		if text == "" {
			return nil, fmt.Errorf("%w: empty segment in %q", ErrInvalidShardRanges, expr)
		}
		startText, endText, hasEnd := strings.Cut(text, "-")
		start, err := parseShardIndex(startText)
		if err != nil {
			return nil, err
		}
		end := start
		if hasEnd {
			end, err = parseShardIndex(endText)
			if err != nil {
				return nil, err
			}
			if end < start {
				return nil, fmt.Errorf("%w: span %q ends before it starts", ErrInvalidShardRanges, text)
			}
		}
		ranges = append(ranges, shardRange{start: start, end: end, text: text})
	}
	return ranges, nil
}

// parseShardIndex parses one 1-based shard index, failing closed on empty,
// non-numeric, zero, negative and overflowing input.
func parseShardIndex(s string) (int, error) {
	s = strings.TrimSpace(s)
	n, err := strconv.Atoi(s)
	if err != nil || n < 1 {
		return 0, fmt.Errorf("%w: %q is not a positive shard index", ErrInvalidShardRanges, s)
	}
	return n, nil
}

// ValidateShardRanges reports whether expr is a well-formed shard-range
// expression for a non-empty filter selection. The server calls it at the
// request boundary so a bad range fails as a named client error before any
// Hub contact; validate() re-runs the same check for direct library callers.
func ValidateShardRanges(expr string, filters []string) error {
	if _, err := parseShardRanges(expr); err != nil {
		return err
	}
	if len(filters) == 0 {
		return fmt.Errorf("%w: ranges only narrow a selection, so filters must be non-empty", ErrInvalidShardRanges)
	}
	return nil
}

// validateAppendFilterSubdirFilters rejects path-shaped filters when the
// downloader would use each filter as a destination directory name. A filter
// that names a repository path (or the "."/".." components) is not a
// single-component group/token, so appending it as a directory would nest the
// matched file under a misleading path. Path filters remain valid when
// AppendFilterSubdir is disabled.
func validateAppendFilterSubdirFilters(filters []string) error {
	var pathFilters []string
	for _, filter := range filters {
		normalized := strings.ReplaceAll(filter, `\`, "/")
		if normalized == "." || normalized == ".." || strings.Contains(normalized, "/") {
			pathFilters = append(pathFilters, filter)
		}
	}
	if len(pathFilters) == 0 {
		return nil
	}
	return fmt.Errorf("%w: %q", ErrAppendFilterSubdirPathFilter, pathFilters)
}

// backoff implements exponential backoff with jitter.
type backoff struct {
	next   time.Duration
	init   time.Duration
	max    time.Duration
	mult   float64
	jitter time.Duration
}

// maxBackoffCeiling is a defensive ceiling on the configured backoff durations.
// It bounds the arithmetic in Next even when a library caller passes an absurd
// value (the settings boundary also rejects negatives, but not huge positives).
// One hour is chosen because it is far beyond any useful retry pacing: retries
// waiting longer than an hour are indistinguishable from a dead transfer, and
// the stall watchdog/absolute attempt ceiling already bound termination. A
// finite ceiling keeps Next's multiply and jitter additions from overflowing
// int64 nanoseconds.
const maxBackoffCeiling = time.Hour

// saturatingMulDuration multiplies d by mult (mult > 0) and saturates at max
// instead of overflowing int64 nanoseconds. It never returns a negative value.
func saturatingMulDuration(d time.Duration, mult float64, max time.Duration) time.Duration {
	if d <= 0 {
		return 0
	}
	// Saturate before the float multiply can overflow int64 nanoseconds:
	// a product exceeding math.MaxInt64 would otherwise wrap negative.
	if float64(d) > float64(math.MaxInt64)/mult {
		return max
	}
	p := time.Duration(float64(d) * mult)
	if p < 0 {
		return max
	}
	return p
}

// newRetry creates a new backoff instance from settings.
func newRetry(cfg Settings) *backoff {
	init := 400 * time.Millisecond
	max := 10 * time.Second
	if d, err := time.ParseDuration(defaultString(cfg.BackoffInitial, "400ms")); err == nil && d >= 0 {
		init = d
	}
	if d, err := time.ParseDuration(defaultString(cfg.BackoffMax, "10s")); err == nil && d >= 0 {
		max = d
	}
	// Defensive clamp: a library caller passing an absurd duration still gets
	// bounded, sane pacing.
	if init > maxBackoffCeiling {
		init = maxBackoffCeiling
	}
	if max > maxBackoffCeiling {
		max = maxBackoffCeiling
	}
	if max < init {
		max = init
	}
	return &backoff{next: init, init: init, max: max, mult: 1.6, jitter: 120 * time.Millisecond}
}

// Next returns the next backoff duration. Every value it returns or stores is
// guaranteed to be >= 0 and <= b.max, even for extreme configured durations: the
// exponential multiply and the jitter addition saturate rather than overflow
// int64 nanoseconds (an overflowed negative delay would make retries burst with
// no pacing).
func (b *backoff) Next() time.Duration {
	// Jitter addition must not overflow: clamp b.next to max first (it is
	// already <= max after every prior step, but an init == max is fine too),
	// then add the (non-negative) jitter with saturation.
	base := b.next
	if base > b.max {
		base = b.max
	}
	j := time.Duration(int64(b.jitter) * int64(time.Now().UnixNano()%3) / 2)
	d := base + j
	if d < base { // jitter addition overflowed
		d = b.max
	} else if d > b.max {
		d = b.max
	}
	if d < 0 {
		d = 0
	}

	b.next = saturatingMulDuration(base, b.mult, b.max)
	if b.next > b.max {
		b.next = b.max
	}
	if b.next < 0 {
		b.next = 0
	}
	return d
}

// reset returns the backoff to its initial delay. It is used when an attempt
// made real forward progress (advanced the resume offset), so the schedule
// does not stay inflated after a healthy retry.
func (b *backoff) reset() {
	b.next = b.init
}

// sleepCtx waits for d or returns false if ctx is canceled first.
func sleepCtx(ctx context.Context, d time.Duration) bool {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// parseSizeString parses a human-readable size string (e.g., "32MiB") to bytes.
func parseSizeString(s string, def int64) (int64, error) {
	if s == "" {
		return def, nil
	}
	var n float64
	var unit string
	_, err := fmt.Sscanf(strings.ToUpper(strings.TrimSpace(s)), "%f%s", &n, &unit)
	if err != nil {
		var nn int64
		if _, e2 := fmt.Sscanf(s, "%d", &nn); e2 == nil {
			return nn, nil
		}
		return 0, err
	}
	switch unit {
	case "B", "":
		return int64(n), nil
	case "KB":
		return int64(n * 1000), nil
	case "MB":
		return int64(n * 1000 * 1000), nil
	case "GB":
		return int64(n * 1000 * 1000 * 1000), nil
	case "KIB":
		return int64(n * 1024), nil
	case "MIB":
		return int64(n * 1024 * 1024), nil
	case "GIB":
		return int64(n * 1024 * 1024 * 1024), nil
	default:
		return 0, fmt.Errorf("unknown unit %q", unit)
	}
}

// defaultString returns s if non-empty, otherwise def.
func defaultString(s string, def string) string {
	if s == "" {
		return def
	}
	return s
}
