// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package app

import (
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/bashrusakh/hfdesk/internal/logging"
	"github.com/bashrusakh/hfdesk/internal/server"
)

// LogOptions configures the logging pipeline for a host.
type LogOptions struct {
	Level    string // "debug"|"info"|"warn"|"error"; empty means info
	Console  bool
	FilePath string // empty means no file sink

	// RotatedFiles is the number of rotated log files kept next to the
	// active file.
	//
	// Zero selects logging.DefaultFileMaxBackups, so a host that says
	// nothing about the rotated files keeps the history of the log. A
	// negative value keeps no rotated file at all.
	//
	// The field is not named FileMaxBackups on purpose: the logging
	// package uses that name for the opposite convention, where zero
	// retains no rotated file and a negative value selects the default.
	// The translation to the logging package happens in SetupLogging, so
	// the two layers cannot be confused about the meaning of a count.
	RotatedFiles int
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

	// The rotated files are the history of the log: the desktop host
	// enables the file sink because the log file is its support material,
	// so a host that says nothing about the rotated files keeps them.
	//
	// This is the one place where the count of this layer is translated
	// into the count of the logging package. The two layers spell the
	// zero and the negative value the other way round, so the logging
	// package only ever sees a count it reads the same way: the default
	// history of three rotated files, or no rotated file at all.
	maxBackups := o.RotatedFiles
	if maxBackups == 0 {
		maxBackups = logging.DefaultFileMaxBackups
	} else if maxBackups < 0 {
		maxBackups = 0
	}

	pipeline, err := logging.Install(logging.Options{
		Level:          level,
		Console:        o.Console,
		File:           o.FilePath != "",
		FilePath:       o.FilePath,
		FileMaxBackups: maxBackups,
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

// LogFailureText returns the text that reports the failure of the persistent
// log to the user.
//
// The text names the problem and the log file, so the user can tell that the
// log stopped working and can find the file that stopped working. Both hosts
// use it, so the console and the desktop host tell the user the same thing
// about the same failure.
func LogFailureText(err error, logPath string) string {
	if err == nil {
		return ""
	}
	text := "HFDesk could not keep writing its log.\n\n" + err.Error()
	if logPath != "" {
		text += "\n\nThe log file is at " + logPath + "."
	}
	return text
}

// LogFailureReporter reports the failure of the persistent log to the user.
//
// The reporter is the single place where the "reported once" rule lives: the
// subscription and the check at the shutdown share one guard, so the user is
// never told twice about the same failure.
type LogFailureReporter struct {
	// once guards the report, so the failure is reported once.
	once sync.Once
	// pipeline holds the failure of the persistent log.
	pipeline *logging.Pipeline
	// logPath is the path of the log file. It is named in the report.
	logPath string
	// report shows the failure to the user.
	report func(text string)
}

// NewLogFailureReporter returns a reporter for the failures of the given
// pipeline.
//
// The reporter does nothing on its own: the host subscribes it to the
// pipeline and reports the pending failure before it closes the pipeline.
func NewLogFailureReporter(pipeline *logging.Pipeline, report func(text string)) *LogFailureReporter {
	reporter := &LogFailureReporter{report: report}
	if pipeline != nil {
		reporter.pipeline = pipeline
		reporter.logPath = pipeline.FilePath()
	}
	return reporter
}

// Subscribe reports the failure of the persistent log as soon as the
// pipeline records it.
//
// The pipeline records the failure of a sink when it happens, so the
// subscription is event driven: the reporter never polls the pipeline, and a
// user whose log file died does not have to quit the application to learn
// about it.
//
// The report happens on a goroutine of the pipeline, so the reporting never
// blocks the pipeline.
func (r *LogFailureReporter) Subscribe() {
	if r == nil || r.pipeline == nil {
		return
	}
	r.pipeline.OnError(func(err error) {
		r.reportFailure(err)
	})
}

// Report reports the failure the pipeline holds, when nothing reported it
// yet.
//
// The host calls it before it closes the pipeline: the error of the pipeline
// is still available then, and a closed pipeline cannot report it anymore.
func (r *LogFailureReporter) Report() {
	if r == nil || r.pipeline == nil {
		return
	}
	r.reportFailure(r.pipeline.Err())
}

// reportFailure shows the failure to the user, once.
func (r *LogFailureReporter) reportFailure(err error) {
	if err == nil {
		return
	}
	r.once.Do(func() {
		if r.report == nil {
			return
		}
		r.report(LogFailureText(err, r.logPath))
	})
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
