// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	"github.com/bashrusakh/hfdesk/internal/logging"
	"github.com/bashrusakh/hfdesk/internal/server"
)

// LogOptions configures the logging pipeline for a host.
type LogOptions struct {
	Level    string // "debug"|"info"|"warn"|"error"; empty means info
	Console  bool
	FilePath string // empty means no file sink
}

// SetupLogging installs the application logging pipeline and returns it.
//
// The pipeline becomes the process wide default, so every package logs
// through it. The console sink writes to standard error, never to standard
// output, so redirecting stdout keeps working for scripts.
//
// The choice of the sinks stays with the host: the CLI host enables the
// console sink and the file sink only when a log file was requested, while
// the desktop host passes its own default log file.
func SetupLogging(o LogOptions) (*logging.Pipeline, error) {
	level, err := parseLogLevel(o.Level)
	if err != nil {
		return nil, err
	}

	pipeline, err := logging.Install(logging.Options{
		Level:    level,
		Console:  o.Console,
		File:     o.FilePath != "",
		FilePath: o.FilePath,
	})
	if err != nil {
		return nil, err
	}
	return pipeline, nil
}

// parseLogLevel converts the configured level into a log level.
//
// An empty level means info. An unknown level is an error, never a silent
// fallback, because a silently ignored level hides a misconfiguration.
func parseLogLevel(value string) (slog.Level, error) {
	if strings.TrimSpace(value) == "" {
		return slog.LevelInfo, nil
	}
	level, err := logging.ParseLevel(value)
	if err != nil {
		return slog.LevelInfo, fmt.Errorf("invalid log level %q: expected one of debug, info, warn or error", value)
	}
	return level, nil
}

// DefaultLogPath returns a per-user path for the rotating log file, e.g.
// <user cache dir>/HFDesk/logs/hfdesk.log. It must not create anything.
func DefaultLogPath() string {
	base, err := os.UserCacheDir()
	if err != nil || base == "" {
		if home, homeErr := os.UserHomeDir(); homeErr == nil && home != "" {
			base = filepath.Join(home, ".cache")
		} else {
			base = server.AppConfigDir()
		}
	}
	return filepath.Join(base, "HFDesk", "logs", "hfdesk.log")
}
