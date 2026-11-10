// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package service

import (
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"strings"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
)

// RunArgsHelp returns the flags of the "service run" argument vector with a
// one-line description each, in the order the usage text of the command
// lists them.
//
// Name is the flag name without the leading dashes ("config" for
// "--config"), so the caller can render it the way it likes. Usage is a
// single line that never ends with a period, so it can be printed as one
// help entry.
func RunArgsHelp() []struct{ Name, Usage string } {
	return []struct{ Name, Usage string }{
		{"config", "absolute path of the config file of the instance"},
		{"state-dir", "absolute path of the state directory of the instance"},
		{"log-dir", "absolute path of the log directory of the instance"},
		{"listen", "host:port the instance binds (default " + DefaultListen + ")"},
		{"cache-dir", "absolute path of the Hugging Face cache root"},
		{"hub-dir", "absolute path of the Hugging Face hub cache directory"},
		{"local-dir", "absolute path of the flat output directory"},
	}
}

// requiredRunFlags names the flags of the run vector an instance start must
// carry: a missing flag means a broken registration, not a default. The
// parser refuses a vector that misses one, and an update that has to verify a
// running start refuses a registered command that cannot show the same set —
// one list, so the update domain and the verification domain cannot drift
// apart.
var requiredRunFlags = []string{"config", "state-dir", "log-dir"}

// runOptionValue returns the parsed value of a run flag by its name without
// the leading dashes ("config" for "--config"); an unknown name has no value.
func runOptionValue(opts RunOptions, name string) string {
	switch name {
	case "config":
		return opts.Paths.ConfigFile
	case "state-dir":
		return opts.Paths.StateDir
	case "log-dir":
		return opts.Paths.LogDir
	case "listen":
		return opts.Listen
	case "cache-dir":
		return opts.CacheDir
	case "hub-dir":
		return opts.HubDir
	case "local-dir":
		return opts.LocalDir
	default:
		return ""
	}
}

// ParseRunArgs parses the argument vector of the service host process into
// the options of the headless host.
//
// The vector is the one RunArgs builds. The leading command tokens are
// optional, so the canonical vector ("service run --config ...") and the
// flags of the subcommand alone are both understood: the CLI hands over the
// flags of "service run", a platform service handler hands over the
// registered argument string.
//
// The parser never reads a credential. The flags it knows carry paths and a
// bind address only, so an argument that carries a token is not a flag of
// this parser: it is rejected as an unknown flag and its value never
// reaches an error message.
func ParseRunArgs(args []string) (RunOptions, error) {
	opts, err := parseRunArgsValues(args)
	if err != nil {
		return RunOptions{}, err
	}

	// An instance is only started with a complete path model. A missing
	// flag means a broken registration, not a default.
	for _, name := range requiredRunFlags {
		if runOptionValue(opts, name) == "" {
			return RunOptions{}, fmt.Errorf("service: missing required flag --%s", name)
		}
	}

	// A supervised process must not resolve a path against the working
	// directory of the supervisor, so every path of the vector is
	// absolute. The optional directories may stay unset, but a set one is
	// absolute as well.
	for _, p := range []struct {
		flag  string
		value string
	}{
		{"--config", opts.Paths.ConfigFile},
		{"--state-dir", opts.Paths.StateDir},
		{"--log-dir", opts.Paths.LogDir},
		{"--cache-dir", opts.CacheDir},
		{"--hub-dir", opts.HubDir},
		{"--local-dir", opts.LocalDir},
	} {
		if p.value == "" {
			continue
		}
		if !filepath.IsAbs(p.value) {
			return RunOptions{}, fmt.Errorf("service: %s %q must be an absolute path", p.flag, p.value)
		}
	}

	// The bind address is validated exactly as at install time: an empty
	// host and an out-of-range port are rejected the same way.
	if _, _, err := ParseListen(opts.Listen); err != nil {
		return RunOptions{}, err
	}
	opts.Listen = listenArg(opts.Listen)
	return opts, nil
}

// parseRunArgsValues is the one parser for the flag syntax accepted by the
// headless host. Registration readers use its values too, so status and a
// reinstall do not reinterpret an argument vector differently from Run.
// Values parsed before a malformed or unknown flag remain available to the
// tolerant registration readers.
func parseRunArgsValues(args []string) (RunOptions, error) {
	fs := flag.NewFlagSet("service run", flag.ContinueOnError)
	// The parser reports its own errors. A usage dump of the flag set is
	// noise for a CLI and useless for a service handler that has no
	// terminal at all.
	fs.SetOutput(io.Discard)

	var (
		configFile string
		stateDir   string
		logDir     string
		listen     string
		cacheDir   string
		hubDir     string
		localDir   string
	)

	fs.StringVar(&configFile, "config", "", "absolute path of the config file of the instance")
	fs.StringVar(&stateDir, "state-dir", "", "absolute path of the state directory of the instance")
	fs.StringVar(&logDir, "log-dir", "", "absolute path of the log directory of the instance")
	fs.StringVar(&listen, "listen", DefaultListen, "host:port the instance binds")
	fs.StringVar(&cacheDir, "cache-dir", "", "absolute path of the Hugging Face cache root")
	fs.StringVar(&hubDir, "hub-dir", "", "absolute path of the Hugging Face hub cache directory")
	fs.StringVar(&localDir, "local-dir", "", "absolute path of the flat output directory")

	err := fs.Parse(stripRunCommand(args))
	// Everything the parser does not know is a mistake: a positional
	// argument would otherwise be dropped silently. Its value is not
	// repeated in the error: a bare word is the shape a credential takes
	// when it is handed over without a flag.
	opts := RunOptions{
		Paths: runtimepaths.Paths{
			ConfigFile: configFile,
			StateDir:   stateDir,
			LogDir:     logDir,
		},
		// The address keeps the canonical shape the argument vector
		// carries, so a parsed instance and a registered one agree.
		Listen:   listen,
		CacheDir: cacheDir,
		HubDir:   hubDir,
		LocalDir: localDir,
	}
	if err != nil {
		return opts, fmt.Errorf("service: parse run arguments: %w", err)
	}
	if rest := fs.Args(); len(rest) > 0 {
		return opts, fmt.Errorf("service: parse run arguments: unexpected argument, the vector carries flags only")
	}
	return opts, nil
}

// runArgValue returns one registered flag from an argument vector using the
// same parser as ParseRunArgs. It is intentionally tolerant of other missing
// required values because status must still describe a damaged registration.
func runArgValue(args []string, flag string) string {
	return runOptionValue(parseRunArgValues(args), strings.TrimLeft(flag, "-"))
}

func parseRunArgValues(args []string) RunOptions {
	opts, _ := parseRunArgsValues(args)
	return opts
}

// stripRunCommand removes the leading command tokens of the invocation, so
// the canonical vector ("service run --config ...") and the flags of the
// subcommand alone are both accepted.
func stripRunCommand(args []string) []string {
	if len(args) >= 2 && args[0] == "service" && args[1] == "run" {
		return args[2:]
	}
	if len(args) >= 1 && args[0] == "run" {
		return args[1:]
	}
	return args
}
