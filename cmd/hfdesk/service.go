// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"regexp"
	"runtime"
	"strconv"
	"strings"
	"syscall"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
	"github.com/bashrusakh/hfdesk/internal/service"
)

// The service command family is a command of its own: "hfdesk" without a
// "service" token keeps the interactive behaviour of the CLI untouched.
//
// The family is built with the standard library flag package. Every
// subcommand owns its own flag set, so a flag of one subcommand is never
// accepted by another one, and no CLI framework is introduced.
//
// Everything platform specific stays in internal/service: the command
// family only describes what the user asked for and reports what the
// supervisor answered. The only platform check of the command line is the
// default scope, because the Windows service control manager registers
// machine-wide services only.

const (
	// serviceUsageExitCode is the exit code of a command line that could
	// not be understood: a missing subcommand, an unknown subcommand or an
	// invalid flag.
	serviceUsageExitCode = 2

	// serviceFailureExitCode is the exit code of an operation that failed.
	serviceFailureExitCode = 1
)

// serviceSubcommands lists the subcommands of the family in the order the
// usage block prints them.
var serviceSubcommands = []string{
	"install",
	"uninstall",
	"start",
	"stop",
	"restart",
	"status",
	"run",
}

// errHelpRequested reports that the user asked for the help of a command. It
// is not a failure: the command exits with 0.
var errHelpRequested = errors.New("help requested")

// versionLikeSegment matches a "digit.digit" segment of a file name.
//
// The heuristic is deliberately simple: a release artifact carries its
// version in its name ("hfdesk-1.2.3-linux-amd64", "hfdesk_0.9.1") and the
// next release replaces the file. A plain "hfdesk" does not match.
var versionLikeSegment = regexp.MustCompile(`\d+\.\d+`)

// isServiceCommand reports whether the arguments select the service command
// family.
//
// The check is exact: only the bare token "service" selects the family, so
// every other invocation keeps the interactive behaviour of the CLI.
func isServiceCommand(args []string) bool {
	return len(args) > 0 && args[0] == "service"
}

// runServiceCommand runs the "hfdesk service ..." command family and
// returns the exit code of the process.
//
// The arguments are the ones after the "service" token. The output streams
// are arguments as well, so the whole family can be exercised without
// starting a process.
func runServiceCommand(args []string, stdout, stderr io.Writer) int {
	if len(args) == 0 {
		fmt.Fprintln(stderr, "hfdesk service: a subcommand is required")
		writeServiceUsage(stderr)
		return serviceUsageExitCode
	}

	sub, rest := args[0], args[1:]

	switch sub {
	case "install":
		return serviceCommandResult(stderr, serviceInstall(rest, stdout))
	case "uninstall":
		return serviceCommandResult(stderr, serviceUninstall(rest, stdout))
	case "start":
		return serviceCommandResult(stderr, serviceStart(rest, stdout))
	case "stop":
		return serviceCommandResult(stderr, serviceStop(rest, stdout))
	case "restart":
		return serviceCommandResult(stderr, serviceRestart(rest, stdout))
	case "status":
		return serviceStatus(rest, stdout, stderr)
	case "run":
		return serviceCommandResult(stderr, serviceRun(rest, stdout))
	case "help", "-h", "--help":
		writeServiceUsage(stdout)
		return 0
	default:
		fmt.Fprintf(stderr, "hfdesk service: unknown subcommand %q\n", sub)
		fmt.Fprintf(stderr, "Valid subcommands: %s\n", strings.Join(serviceSubcommands, ", "))
		writeServiceUsage(stderr)
		return serviceUsageExitCode
	}
}

// serviceCommandResult prints the error of a failed command and returns the
// exit code of the process.
func serviceCommandResult(stderr io.Writer, err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errHelpRequested):
		return 0
	default:
		fmt.Fprintf(stderr, "hfdesk service: %s\n", serviceErrorText(err))
		return serviceFailureExitCode
	}
}

// serviceErrorText renders an error of the service layer for the command
// line. The package prefixes its errors with "service: ", which the command
// line already carries in its own prefix, so it is removed once.
func serviceErrorText(err error) string {
	return strings.TrimPrefix(err.Error(), "service: ")
}

// writeServiceUsage prints the usage block of the service command family.
func writeServiceUsage(w io.Writer) {
	fmt.Fprint(w, "Usage: hfdesk service <subcommand> [options]\n\n")
	fmt.Fprint(w, "Subcommands:\n")
	for _, line := range []struct{ name, summary string }{
		{"install", "register the service with the platform supervisor"},
		{"uninstall", "remove the registration (the user data is kept)"},
		{"start", "start the registered service"},
		{"stop", "stop the registered service"},
		{"restart", "stop and start the registered service"},
		{"status", "report the scope, the state and the paths of the instance"},
		{"run", "run the headless host (internal: the supervisor invokes it)"},
	} {
		fmt.Fprintf(w, "  %-10s %s\n", line.name, line.summary)
	}
	fmt.Fprintf(w, `
Scope:
  --user        per-user installation (the default on Linux and macOS)
  --system      machine-wide installation (the default on Windows)

Options:
  --listen HOST:PORT
                bind address of the instance (default %s)

Exit codes:
  0  the operation succeeded
  1  the operation failed
  2  the command line could not be understood

"hfdesk service status" exits with 0 when the service is running, stopped,
starting or stopping, and with 1 when it is not installed, broken, failed or
in an unknown state, so a script can branch on it.

Run "hfdesk service <subcommand> --help" for the flags of a subcommand.
`, service.DefaultListen)
}

// writeSubcommandUsage prints the usage of a subcommand: its summary and
// the flags it registers.
func writeSubcommandUsage(w io.Writer, fs *flag.FlagSet, summary string) {
	fmt.Fprintf(w, "Usage: %s [options]\n\n%s\n\nOptions:\n", fs.Name(), summary)
	fs.SetOutput(w)
	fs.PrintDefaults()
	fmt.Fprint(w, "\nExit codes: 0 success, 1 failed operation, 2 invalid command line.\n")
}

// writeField writes one "Key: value" line of a summary block. The keys are
// padded to a common width, so the values of a block line up.
func writeField(w io.Writer, key, value string) {
	if value == "" {
		value = "unknown"
	}
	fmt.Fprintf(w, "%-14s %s\n", key+":", value)
}

// isWindows reports whether the process runs on Windows.
func isWindows() bool {
	switch runtime.GOOS {
	case "windows":
		return true
	default:
		return false
	}
}

// defaultScope returns the scope of an installation that does not ask for
// one.
//
// The user scope is the default on Linux and macOS. The Windows service
// control manager registers machine-wide services only, so the system scope
// is the default there.
func defaultScope() service.Scope {
	if isWindows() {
		return service.ScopeSystem
	}
	return service.ScopeUser
}

// resolveScope turns the scope selector of the command line into the scope
// of the installation.
//
// The selectors are mutually exclusive: two scopes are two installations,
// so an ambiguous command line is rejected instead of picking one of them.
func resolveScope(user, system bool) (service.Scope, error) {
	switch {
	case user && system:
		return "", errors.New("--user and --system are mutually exclusive, choose one scope")
	case user:
		if isWindows() {
			return "", errors.New("--user is not available on windows: the service control manager registers machine-wide services only, use --system")
		}
		return service.ScopeUser, nil
	case system:
		return service.ScopeSystem, nil
	default:
		return defaultScope(), nil
	}
}

// scopeFlags holds the scope selector of a command.
type scopeFlags struct {
	user   bool
	system bool
}

// registerScopeFlags registers the scope selector on a flag set.
func registerScopeFlags(fs *flag.FlagSet, flags *scopeFlags) {
	fs.BoolVar(&flags.user, "user", false, "per-user installation (the default on Linux and macOS)")
	fs.BoolVar(&flags.system, "system", false, "machine-wide installation (the default on Windows)")
}

// pathFlag is one path flag of the command line.
type pathFlag struct {
	name  string
	value string
}

// validateAbsolutePaths rejects a relative path.
//
// A supervised process must not resolve a path against the working
// directory of the supervisor, so a relative path is a mistake and not a
// default.
func validateAbsolutePaths(paths []pathFlag) error {
	for _, p := range paths {
		if p.value == "" {
			continue
		}
		if !filepath.IsAbs(p.value) {
			return fmt.Errorf("%s %q must be an absolute path", p.name, p.value)
		}
	}
	return nil
}

// validateListen validates a bind address of the command line and returns
// its canonical form.
//
// The rules are the rules of the service layer: the address must name a
// host and a port, and an empty host is rejected because it binds every
// interface.
func validateListen(listen string) (string, error) {
	host, port, err := service.ParseListen(listen)
	if err != nil {
		return "", err
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

// resolveExecutable returns the absolute path of the running binary.
//
// The registration points at that exact path, so it must be absolute: a
// relative path would be resolved against the working directory of the
// supervisor.
func resolveExecutable() (string, error) {
	exe, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("the path of the running binary is unknown: %w", err)
	}
	abs, err := filepath.Abs(exe)
	if err != nil {
		return "", fmt.Errorf("the path of the running binary %q is not absolute: %w", exe, err)
	}
	return abs, nil
}

// installFlags holds the flags of the "service install" command.
type installFlags struct {
	scopeFlags
	listen   string
	config   string
	stateDir string
	logDir   string
	cacheDir string
	hubDir   string
	localDir string
}

// buildInstallOptions assembles the options of an installation from the
// scope, the flags of the command line and the path of the binary.
//
// The default runtime paths of the scope are the base and an explicit path
// flag overrides the matching default. The config file keeps its own
// directory: the parent of the config file is the directory the
// installation prepares.
//
// The cache directories are derived from the runtime paths, so they are
// resolved after the overrides: a machine-wide instance keeps its cache
// inside its own state root even when the state root was overridden.
func buildInstallOptions(scope service.Scope, flags installFlags, executable string) (service.InstallOptions, error) {
	paths, err := service.DefaultPaths(scope)
	if err != nil {
		return service.InstallOptions{}, err
	}
	if flags.config != "" {
		paths.ConfigFile = flags.config
	}
	if flags.stateDir != "" {
		paths.StateDir = flags.stateDir
	}
	if flags.logDir != "" {
		paths.LogDir = flags.logDir
	}

	cacheRoot, hubDir := service.DefaultCacheDirs(scope, paths)
	if flags.cacheDir != "" {
		cacheRoot = flags.cacheDir
	}
	if flags.hubDir != "" {
		hubDir = flags.hubDir
	}

	listen := flags.listen
	if listen == "" {
		listen = service.DefaultListen
	}

	return service.InstallOptions{
		Scope:      scope,
		Executable: executable,
		Listen:     listen,
		Paths:      paths,
		CacheDir:   cacheRoot,
		HubDir:     hubDir,
		LocalDir:   flags.localDir,
	}, nil
}

// serviceInstall registers the service with the platform supervisor of the
// scope.
//
// The command validates its own arguments before it touches the system. It
// never starts the instance: starting is a separate, explicit command.
func serviceInstall(args []string, stdout io.Writer) error {
	flags := installFlags{}
	fs := flag.NewFlagSet("hfdesk service install", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	registerScopeFlags(fs, &flags.scopeFlags)
	fs.StringVar(&flags.listen, "listen", service.DefaultListen, "host:port the instance binds")
	fs.StringVar(&flags.config, "config", "", "absolute path of the config file the instance owns")
	fs.StringVar(&flags.stateDir, "state-dir", "", "absolute path of the state directory of the instance")
	fs.StringVar(&flags.logDir, "log-dir", "", "absolute path of the log directory of the instance")
	fs.StringVar(&flags.cacheDir, "cache-dir", "", "absolute path of the Hugging Face cache root")
	fs.StringVar(&flags.hubDir, "hub-dir", "", "absolute path of the Hugging Face hub cache directory")
	fs.StringVar(&flags.localDir, "local-dir", "", "absolute path of the flat output directory")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			writeSubcommandUsage(stdout, fs, "Register HFDesk with the platform supervisor of the scope.\n\nThe registration is written, the instance is not started.")
			return errHelpRequested
		}
		return fmt.Errorf("invalid command line: %w", err)
	}
	if rest := fs.Args(); len(rest) > 0 {
		return fmt.Errorf("unexpected argument %q, the command takes flags only", rest[0])
	}

	scope, err := resolveScope(flags.user, flags.system)
	if err != nil {
		return err
	}
	listen, err := validateListen(flags.listen)
	if err != nil {
		return err
	}
	if err := validateAbsolutePaths([]pathFlag{
		{"--config", flags.config},
		{"--state-dir", flags.stateDir},
		{"--log-dir", flags.logDir},
		{"--cache-dir", flags.cacheDir},
		{"--hub-dir", flags.hubDir},
		{"--local-dir", flags.localDir},
	}); err != nil {
		return err
	}
	flags.listen = listen

	executable, err := resolveExecutable()
	if err != nil {
		return err
	}

	opts, err := buildInstallOptions(scope, flags, executable)
	if err != nil {
		return err
	}

	mgr, err := service.New(scope)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := mgr.Install(ctx, opts); err != nil {
		return err
	}

	writeInstallSummary(stdout, ctx, mgr, scope, opts)
	return nil
}

// writeInstallSummary prints what was registered.
//
// The path of the registration is known only after the installation, so it
// is read back from the supervisor.
func writeInstallSummary(w io.Writer, ctx context.Context, mgr service.Manager, scope service.Scope, opts service.InstallOptions) {
	writeField(w, "Service", service.ServiceName)
	writeField(w, "Scope", string(scope))
	writeField(w, "Registration", registrationPath(ctx, mgr))
	writeField(w, "Executable", opts.Executable)
	writeField(w, "Listen", opts.Listen)
	writeField(w, "Config file", opts.Paths.ConfigFile)
	writeField(w, "State dir", opts.Paths.StateDir)
	writeField(w, "Log dir", opts.Paths.LogDir)
	writeField(w, "Cache root", opts.CacheDir)

	if warning := nonLoopbackWarning(opts.Listen); warning != "" {
		fmt.Fprintln(w)
		fmt.Fprint(w, warning)
	}
	if warning := upgradeWarning(opts.Executable); warning != "" {
		fmt.Fprintln(w)
		fmt.Fprint(w, warning)
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "The service was registered but not started.")
	fmt.Fprintln(w, "Start it with: hfdesk service start")
}

// registrationPath returns the path of the supervisor registration.
func registrationPath(ctx context.Context, mgr service.Manager) string {
	status, _ := mgr.Status(ctx)
	if status.UnitPath == "" {
		return "unknown"
	}
	return status.UnitPath
}

// serviceUninstall removes the registration of the service.
//
// The registration is removed and nothing else: the models, the cache, the
// config file, the job history and the logs of the instance stay where
// they are.
func serviceUninstall(args []string, stdout io.Writer) error {
	mgr, scope, err := newLifecycleManager("hfdesk service uninstall", args, stdout,
		"Remove the registration of the service.\n\nThe user data is kept: models, cache, config, jobs/history and logs are not touched.")
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// The paths of the registration are read before it is removed, so the
	// report can name the directories the data stays in.
	paths, cacheRoot := instanceDataPaths(ctx, mgr, scope)

	if err := mgr.Uninstall(ctx); err != nil {
		return err
	}

	fmt.Fprintf(stdout, "The registration of %s (%s scope) was removed.\n", service.ServiceName, scope)
	fmt.Fprintln(stdout, "The user data was kept: models, cache, config, jobs/history and logs are still there.")
	writeField(stdout, "Config file", paths.ConfigFile)
	writeField(stdout, "State dir", paths.StateDir)
	writeField(stdout, "Log dir", paths.LogDir)
	writeField(stdout, "Cache root", cacheRoot)
	return nil
}

// instanceDataPaths returns the runtime paths and the cache root of the
// instance of the scope.
//
// The registration is read first, so the report names the directories the
// data of the registration stays in. A registration that cannot be read
// falls back to the default paths of the scope.
func instanceDataPaths(ctx context.Context, mgr service.Manager, scope service.Scope) (runtimepaths.Paths, string) {
	paths, err := service.DefaultPaths(scope)
	if err != nil {
		paths = runtimepaths.Paths{}
	}
	if status, statusErr := mgr.Status(ctx); statusErr == nil {
		if status.Paths.ConfigFile != "" {
			paths.ConfigFile = status.Paths.ConfigFile
		}
		if status.Paths.StateDir != "" {
			paths.StateDir = status.Paths.StateDir
		}
		if status.Paths.LogDir != "" {
			paths.LogDir = status.Paths.LogDir
		}
	}
	cacheRoot, _ := service.DefaultCacheDirs(scope, paths)
	return paths, cacheRoot
}

// newLifecycleManager prepares a lifecycle command: it parses the scope
// selector and opens the manager of the scope.
func newLifecycleManager(name string, args []string, stdout io.Writer, summary string) (service.Manager, service.Scope, error) {
	flags := scopeFlags{}
	fs := flag.NewFlagSet(name, flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	registerScopeFlags(fs, &flags)

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			writeSubcommandUsage(stdout, fs, summary)
			return nil, "", errHelpRequested
		}
		return nil, "", fmt.Errorf("invalid command line: %w", err)
	}
	if rest := fs.Args(); len(rest) > 0 {
		return nil, "", fmt.Errorf("unexpected argument %q, the command takes flags only", rest[0])
	}

	scope, err := resolveScope(flags.user, flags.system)
	if err != nil {
		return nil, "", err
	}
	mgr, err := service.New(scope)
	if err != nil {
		return nil, "", err
	}
	return mgr, scope, nil
}

// serviceStart starts the registered service.
func serviceStart(args []string, stdout io.Writer) error {
	mgr, _, err := newLifecycleManager("hfdesk service start", args, stdout,
		"Start the registered service.")
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := mgr.Start(ctx); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "The %s service was started.\n", service.ServiceName)
	writeLifecycleState(ctx, mgr, stdout)
	return nil
}

// serviceStop stops the registered service.
func serviceStop(args []string, stdout io.Writer) error {
	mgr, _, err := newLifecycleManager("hfdesk service stop", args, stdout,
		"Stop the registered service.")
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := mgr.Stop(ctx); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "The %s service was stopped.\n", service.ServiceName)
	writeLifecycleState(ctx, mgr, stdout)
	return nil
}

// serviceRestart stops and starts the registered service.
func serviceRestart(args []string, stdout io.Writer) error {
	mgr, _, err := newLifecycleManager("hfdesk service restart", args, stdout,
		"Stop and start the registered service.")
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if err := mgr.Restart(ctx); err != nil {
		return err
	}
	fmt.Fprintf(stdout, "The %s service was restarted.\n", service.ServiceName)
	writeLifecycleState(ctx, mgr, stdout)
	return nil
}

// writeLifecycleState prints the state of the instance after a lifecycle
// command, when the supervisor can report it.
func writeLifecycleState(ctx context.Context, mgr service.Manager, w io.Writer) {
	status, err := mgr.Status(ctx)
	if err != nil || status.State == "" {
		return
	}
	writeField(w, "State", string(status.State))
}

// serviceStatus prints the state of the registered service.
//
// The exit code tells a script whether the instance is in a usable state:
// 0 for a running, stopped, starting or stopping instance, 1 for an
// instance that is not installed, broken, failed or unknown.
func serviceStatus(args []string, stdout, stderr io.Writer) int {
	mgr, scope, err := newLifecycleManager("hfdesk service status", args, stdout,
		"Report the scope, the state and the paths of the instance.")
	if err != nil {
		return serviceCommandResult(stderr, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	status, statusErr := mgr.Status(ctx)
	renderStatus(stdout, scope, status)
	if statusErr != nil {
		fmt.Fprintf(stderr, "hfdesk service: %s\n", serviceErrorText(statusErr))
		return serviceFailureExitCode
	}
	return statusExitCode(status.State)
}

// statusExitCode maps the state of an instance onto the exit code of the
// status command.
func statusExitCode(state service.State) int {
	switch state {
	case service.StateRunning, service.StateStopped, service.StateStarting, service.StateStopping:
		return 0
	default:
		return serviceFailureExitCode
	}
}

// renderStatus prints the state of an instance as a "Key: value" block.
func renderStatus(w io.Writer, scope service.Scope, status service.Status) {
	name := status.Name
	if name == "" {
		name = service.ServiceName
	}
	reportedScope := status.Scope
	if reportedScope == "" {
		reportedScope = scope
	}

	// A registration that does not exist carries no paths; the defaults of
	// the scope are the paths such an instance would use.
	paths := status.Paths
	if paths.ConfigFile == "" && paths.StateDir == "" && paths.LogDir == "" {
		if defaults, err := service.DefaultPaths(scope); err == nil {
			paths = defaults
		}
	}
	cacheRoot, _ := service.DefaultCacheDirs(scope, paths)

	writeField(w, "Service", name)
	writeField(w, "Scope", string(reportedScope))
	writeField(w, "State", string(status.State))
	writeField(w, "Registration", status.UnitPath)
	writeField(w, "Executable", status.Executable)
	writeField(w, "Listen", status.Listen)
	writeField(w, "Config file", paths.ConfigFile)
	writeField(w, "State dir", paths.StateDir)
	writeField(w, "Log dir", paths.LogDir)
	writeField(w, "Cache root", cacheRoot)
	if status.Detail != "" {
		writeField(w, "Detail", status.Detail)
	}

	if warning := nonLoopbackWarning(status.Listen); warning != "" {
		fmt.Fprintln(w)
		fmt.Fprint(w, warning)
	}
}

// nonLoopbackWarning returns the warning for a bind address that is not a
// loopback address, or an empty string for a loopback address.
//
// A supervised instance binds loopback by default. An instance that binds
// another address is reachable from the network, so the exposure must be
// visible instead of silent.
func nonLoopbackWarning(listen string) string {
	listen = strings.TrimSpace(listen)
	if listen == "" {
		return ""
	}
	if _, _, err := service.ParseListen(listen); err != nil {
		return ""
	}
	if service.IsLoopbackListen(listen) {
		return ""
	}
	return fmt.Sprintf("WARNING: %s is not a loopback address.\n"+
		"         The HFDesk dashboard is reachable from the network: everyone\n"+
		"         who can reach this machine can reach the dashboard.\n", listen)
}

// upgradeWarning returns the warning for an executable whose path does not
// survive an upgrade, or an empty string for a stable path.
//
// The registration points at one exact path. Two shapes of path break that
// promise:
//
//   - a temporary directory: an unpacked archive or a downloaded artifact
//     is removed by the operating system or by the next download.
//   - a version-stamped name: a release artifact carries its version in its
//     name ("hfdesk-1.2.3-linux-amd64") and the next release replaces the
//     file.
//
// The heuristic is deliberately simple: a name with a "digit.digit" segment
// counts as version-stamped. It only produces a warning, so a false
// positive costs a hint and never a broken installation.
func upgradeWarning(executable string) string {
	if executable == "" {
		return ""
	}
	reason := ""
	switch {
	case isTemporaryPath(executable):
		reason = "it lives in a temporary directory"
	case isVersionStampedName(filepath.Base(executable)),
		isVersionStampedName(filepath.Base(filepath.Dir(executable))):
		reason = "its name carries a version"
	default:
		return ""
	}
	return fmt.Sprintf("WARNING: the registration points at %s, and %s.\n"+
		"         The registration points at that exact path: a version-stamped\n"+
		"         release binary is replaced by the next upgrade and a temporary\n"+
		"         path is removed by the operating system, so the service breaks.\n"+
		"         Install the binary at a stable path (for example\n"+
		"         /usr/local/bin/hfdesk) and run \"hfdesk service install\" again.\n",
		executable, reason)
}

// isTemporaryPath reports whether the path lives in a temporary directory.
//
// The markers are the ones the runtime path model of the repository already
// uses to skip an unpacked archive: the temporary directory of the operating
// system and the markers of a WinRAR extraction.
func isTemporaryPath(path string) bool {
	clean := filepath.Clean(path)
	temp := filepath.Clean(os.TempDir())
	if strings.HasPrefix(strings.ToLower(clean), strings.ToLower(temp)) {
		return true
	}
	lower := strings.ToLower(clean)
	return strings.Contains(lower, "rar$") || strings.Contains(lower, ".rartemp")
}

// isVersionStampedName reports whether the name of a file carries a
// version-like segment.
func isVersionStampedName(name string) bool {
	return versionLikeSegment.MatchString(name)
}

// serviceRun runs the headless host of an instance.
//
// The mode is internal: the supervisor invokes it with the canonical
// argument vector of the registration. It never opens a browser and never
// creates a tray icon.
func serviceRun(args []string, stdout io.Writer) error {
	fs := flag.NewFlagSet("hfdesk service run", flag.ContinueOnError)
	fs.SetOutput(io.Discard)
	// The flags of the subcommand are registered so its help lists them.
	// The parsing itself is done by the service layer, which owns the
	// canonical argument vector of a registered instance.
	registerRunFlags(fs)

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			writeSubcommandUsage(stdout, fs, "Run the headless host of an instance.\n\nThis mode is internal: the supervisor invokes it. It never opens a browser\nand never creates a tray icon.")
			return errHelpRequested
		}
		return fmt.Errorf("invalid command line: %w", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	return runServiceHost(ctx, args)
}

// registerRunFlags registers the flags of the "service run" subcommand.
//
// The list comes from the service layer, so the help of the command and the
// parser of the layer never drift apart.
func registerRunFlags(fs *flag.FlagSet) {
	for _, f := range service.RunArgsHelp() {
		fs.String(f.Name, "", f.Usage)
	}
}

// runServiceHost parses the arguments of the host mode and runs the
// instance.
//
// The host never opens a browser and never creates a tray icon: the
// instance is supervised and there is no desktop session to interact with.
func runServiceHost(ctx context.Context, args []string) error {
	opts, err := service.ParseRunArgs(args)
	if err != nil {
		return err
	}
	return service.RunProcess(ctx, opts)
}
