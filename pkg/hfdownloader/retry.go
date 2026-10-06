// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// DefaultMaxRetryAfter caps how long a server-requested wait (Retry-After or
// the Hub RateLimit header) may delay a retry. A hostile or misconfigured
// server must not be able to park a download for an unbounded time.
const DefaultMaxRetryAfter = 5 * time.Minute

// RetryableStatus reports whether an HTTP status is transient and worth
// retrying. It mirrors APIError.IsRetryable for callers that only hold the
// status code, keeping status classification consistent across the downloader
// and the repo tree/API layers.
func RetryableStatus(code int) bool {
	switch code {
	case 429, 500, 502, 503, 504:
		return true
	default:
		return false
	}
}

// FailFastStatus reports whether an HTTP status is permanent for the whole
// job: retrying it cannot succeed and only multiplies requests against a
// gated, private, or missing resource. It mirrors APIError.IsFailFast.
func FailFastStatus(code int) bool {
	switch code {
	case 401, 403, 404:
		return true
	default:
		return false
	}
}

// RetryAfterFromResponse returns the wait a throttled response asks for,
// honoring both the standard Retry-After header (delta-seconds or an HTTP
// date) and the Hugging Face Hub RateLimit header (for example
// `RateLimit: "api";r=0;t=30`). It returns 0 when the response is nil, has no
// relevant status, or carries no parseable wait. When both headers are present
// the larger wait wins.
func RetryAfterFromResponse(resp *http.Response, now time.Time) time.Duration {
	if resp == nil {
		return 0
	}
	switch resp.StatusCode {
	case 429, 503:
		// Only 429/503 are throttling responses; other statuses may carry a
		// stray Retry-After that must not change scheduling.
	default:
		return 0
	}
	wait := parseRetryAfterValue(resp.Header.Get("Retry-After"), now)
	if rl := parseRateLimitHeader(resp.Header.Get("RateLimit")); rl > wait {
		wait = rl
	}
	return wait
}

// parseRetryAfterValue parses the Retry-After header, accepting either a
// non-negative delta-seconds value or an HTTP-date. Unparseable or negative
// values return 0. A date already in the past returns 0.
func parseRetryAfterValue(v string, now time.Time) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	if secs, err := strconv.Atoi(v); err == nil {
		if secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	if t, err := http.ParseTime(v); err == nil {
		if d := t.Sub(now); d > 0 {
			return d
		}
	}
	return 0
}

// parseRateLimitHeader parses the Hugging Face Hub RateLimit header, a
// semicolon-separated directive list such as `"api";r=0;t=30`. The `t` field
// is the wait in seconds; `t="0"` (or a missing/unparseable `t`) means no wait.
func parseRateLimitHeader(v string) time.Duration {
	v = strings.TrimSpace(v)
	if v == "" {
		return 0
	}
	for _, part := range strings.Split(v, ";") {
		part = strings.TrimSpace(part)
		key, val, found := strings.Cut(part, "=")
		if !found || strings.TrimSpace(key) != "t" {
			continue
		}
		val = strings.Trim(strings.TrimSpace(val), `"`)
		secs, err := strconv.Atoi(val)
		if err != nil || secs <= 0 {
			return 0
		}
		return time.Duration(secs) * time.Second
	}
	return 0
}

// NextRetryWait returns how long to wait before the next attempt: the larger
// of the caller-computed local backoff and any server-requested wait
// (Retry-After / RateLimit), capped at maxWait when maxWait is positive. resp
// may be nil. Cancellation remains the caller's responsibility via sleepCtx.
func NextRetryWait(resp *http.Response, localBackoff, maxWait time.Duration, now time.Time) time.Duration {
	wait := localBackoff
	if server := RetryAfterFromResponse(resp, now); server > wait {
		wait = server
	}
	if maxWait > 0 && wait > maxWait {
		wait = maxWait
	}
	return wait
}

// NewFileResponseError builds the error for a non-2xx file-download response,
// with actionable wording for gated/private (401/403) and missing (404)
// resources. The returned *APIError retains the status code so Is,
// IsRetryable, and IsFailFast classify it consistently with the API layer.
func NewFileResponseError(resp *http.Response, url string) *APIError {
	e := &APIError{StatusCode: resp.StatusCode, Status: resp.Status, URL: url}
	switch resp.StatusCode {
	case http.StatusUnauthorized:
		e.Message = "repository is private or requires authentication; use a Hugging Face token with access"
	case http.StatusForbidden:
		e.Message = "access to this file is gated; accept the repository terms or use a token with access"
	case http.StatusNotFound:
		e.Message = "file or revision not found"
	}
	return e
}

// classifyFileResponse returns a non-nil *APIError when a file response is not
// a success. It returns nil for every 2xx response, including 206 Partial
// Content, so callers can distinguish a real failure from a usable body.
func classifyFileResponse(resp *http.Response, url string) *APIError {
	if resp == nil || (resp.StatusCode >= 200 && resp.StatusCode < 300) {
		return nil
	}
	return NewFileResponseError(resp, url)
}
