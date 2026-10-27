// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"errors"
	"fmt"
	"strings"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
	"github.com/bashrusakh/hfdesk/pkg/smartdl"
)

// errInvalidBranchVariant is returned when a request asks for a whole EXL
// branch but carries fields that describe a narrower or incompatible
// download. Handlers map it to HTTP 400.
var errInvalidBranchVariant = errors.New("invalid branch variant request")

// validateBranchVariantRequest is the single request-shape boundary for
// branch-variant downloads. It is used by both the download and plan
// endpoints so a dry run cannot accept a request the real download rejects.
//
// A branch variant is one whole EXL2/EXL3 branch of a model: the analyzer
// supplies a branch-shaped revision and no file-level selection. Datasets,
// filters/excludes, filter subdirectories, and LocalRepo redirection are
// rejected instead of being silently reinterpreted.
func validateBranchVariantRequest(req DownloadRequest) error {
	if !req.BranchVariant {
		return nil
	}
	if req.Dataset {
		return fmt.Errorf("%w: branch variants are model-only", errInvalidBranchVariant)
	}
	if strings.TrimSpace(req.Revision) == "" || !smartdl.BranchVariantRevisionShape(req.Revision) {
		return fmt.Errorf("%w: revision must name an EXL bitrate branch", errInvalidBranchVariant)
	}
	// Admission must match the completion-manifest layout. A revision that
	// passes the analyzer shape but cannot become a safe path component
	// sequence (for example "exl//4_25" or a component over 255 bytes) would
	// otherwise be admitted and then fail after the download.
	if !hfdownloader.ValidBranchVariantRevision(req.Revision) {
		return fmt.Errorf("%w: revision must be a valid manifest path component sequence", errInvalidBranchVariant)
	}
	if len(req.Filters) != 0 || len(req.Excludes) != 0 || req.AppendFilterSubdir || req.LocalRepo != "" {
		return fmt.Errorf("%w: filters, excludes, appendFilterSubdir, and localRepo are not allowed", errInvalidBranchVariant)
	}
	return nil
}
