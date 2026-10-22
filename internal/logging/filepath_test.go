// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package logging

import (
	"path/filepath"
	"testing"
)

// TestPipelineFilePathReportsTheConfiguredPath proves that the pipeline
// reports the path of its rotating log file, so a caller can show where the
// log is written.
func TestPipelineFilePathReportsTheConfiguredPath(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hfdesk.log")
	pipeline, err := New(Options{File: true, FilePath: path})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer pipeline.Close()

	if got := pipeline.FilePath(); got != path {
		t.Errorf("FilePath() = %q, want %q", got, path)
	}
}

// TestPipelineFilePathIsEmptyWithoutAFileSink proves that a pipeline without
// a file sink reports no path at all: there is no log file to show.
func TestPipelineFilePathIsEmptyWithoutAFileSink(t *testing.T) {
	pipeline, err := New(Options{Console: true})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	defer pipeline.Close()

	if got := pipeline.FilePath(); got != "" {
		t.Errorf("FilePath() = %q, want an empty path", got)
	}
}
