// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package hfdownloader

import (
	"errors"
	"fmt"
)

// Common errors returned by the library.
var (
	// ErrInvalidRepo is returned when the repository ID is not in "owner/name" format.
	ErrInvalidRepo = errors.New("invalid repository ID: expected owner/name format")

	// ErrMissingRepo is returned when no repository is specified.
	ErrMissingRepo = errors.New("missing repository ID")

	// ErrUnauthorized is returned when authentication is required but not provided.
	ErrUnauthorized = errors.New("unauthorized: this repository requires authentication")

	// ErrNotFound is returned when the repository or revision does not exist.
	ErrNotFound = errors.New("repository or revision not found")

	// ErrRateLimited is returned when the API rate limit is exceeded.
	ErrRateLimited = errors.New("rate limited: too many requests")
)

// DownloadError wraps an error with file context.
type DownloadError struct {
	Path string
	Err  error
}

func (e *DownloadError) Error() string {
	return fmt.Sprintf("download %s: %v", e.Path, e.Err)
}

func (e *DownloadError) Unwrap() error {
	return e.Err
}

// VerificationError is returned when file verification fails.
type VerificationError struct {
	Path     string
	Expected string
	Actual   string
	Method   string // "sha256", "size", "etag"
}

func (e *VerificationError) Error() string {
	return fmt.Sprintf("verification failed for %s: %s mismatch (expected %s, got %s)",
		e.Path, e.Method, e.Expected, e.Actual)
}

// APIError represents an error from the HuggingFace API.
type APIError struct {
	StatusCode int
	Status     string
	Message    string
	URL        string
}

func (e *APIError) Error() string {
	if e.Message != "" {
		return fmt.Sprintf("API error %d (%s): %s", e.StatusCode, e.Status, e.Message)
	}
	return fmt.Sprintf("API error %d: %s", e.StatusCode, e.Status)
}

// IsRetryable returns true if the error might succeed on retry.
func (e *APIError) IsRetryable() bool {
	return RetryableStatus(e.StatusCode)
}

// IsFailFast returns true when the error is permanent for the whole job:
// retrying the same request cannot succeed and only multiplies requests
// against a gated, private, or missing resource. 401/403 (auth/gated) and
// 404 (not found) are permanent; every other status falls through to the
// retryable/unknown classification.
func (e *APIError) IsFailFast() bool {
	return FailFastStatus(e.StatusCode)
}

// Is implements errors.Is for common error comparisons.
func (e *APIError) Is(target error) bool {
	switch e.StatusCode {
	case 401, 403:
		return errors.Is(target, ErrUnauthorized)
	case 404:
		return errors.Is(target, ErrNotFound)
	case 429:
		return errors.Is(target, ErrRateLimited)
	default:
		return false
	}
}
