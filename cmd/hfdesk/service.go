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
	"os/user"
	"path/filepath"
	"regexp"
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
// supervisor answered. The command line never compares the operating
// system itself: the scope and the account of an instance are facts of the
// platform, and the service layer owns them.

const (
	// serviceUsageExitCode is the exit code of a command line that could
	// not be understood: a missing subcommand, an unknown subcommand or an
	// invalid flag.
	serviceUsageExitCode = 2

	// serviceFailureExitCode is the exit code of an operation that failed.
	serviceFailureExitCode = 1

	// cacheRootNotRecorded is the wording of a cache root that no source
	// names.
	//
	// The default of the scope and the personal cache of the invoking user
	// are never reported instead: an instance that does not name a cache
	// root does not use them, so naming them would send the reader to the
	// wrong directory.
	cacheRootNotRecorded = "not recorded (unknown)"

	// windowsServiceAccount is the identity of a machine-wide instance on
	// Windows.
	//
	// The service control manager registers the built-in LocalService
	// account, which has no name of its own: the value is the name the
	// registration carries, so the summary names the account the instance
	// really runs as and not the one a command line asked for.
	windowsServiceAccount = `NT AUTHORITY\LocalService`

	// userScopeFlag is the name of the flag that selects the per-user
	// scope.
	//
	// The flag is written in one place, so the hint of the summary names
	// the same scope selector the parser of the command line accepts.
	userScopeFlag = "user"

	// systemScopeFlag is the name of the flag that selects the
	// machine-wide scope.
	systemScopeFlag = "system"
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

// serviceUsageError reports a command line that could not be understood.
//
// The type is the marker of the exit code 2: the family answers a command
// line it cannot understand with the exit code 2 and an operation that was
// understood but failed with the exit code 1. The kind of the failure
// travels with the error, so the classification is the same for every
// subcommand and never depends on the call site that reports the error.
type serviceUsageError struct {
	err error
}

// newServiceUsageError marks an error as a command line that could not be
// understood.
func newServiceUsageError(err error) error {
	if err == nil {
		return nil
	}
	return &serviceUsageError{err: err}
}

// Error renders the message of the wrapped error.
func (e *serviceUsageError) Error() string { return e.err.Error() }

// Unwrap gives access to the wrapped error.
func (e *serviceUsageError) Unwrap() error { return e.err }

// isServiceUsageError reports whether the error reports a command line that
// could not be understood.
func isServiceUsageError(err error) bool {
	var usage *serviceUsageError
	return errors.As(err, &usage)
}

// newServiceManager opens the manager of the platform supervisor of a scope.
//
// The variable is the seam of the tests: the command family is exercised
// without touching the system of the test run.
var newServiceManager = service.New

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
		code := serviceCommandResult(stderr, newServiceUsageError(errors.New("a subcommand is required")))
		writeServiceUsage(stderr)
		return code
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
		code := serviceCommandResult(stderr, newServiceUsageError(fmt.Errorf("unknown subcommand %q", sub)))
		fmt.Fprintf(stderr, "Valid subcommands: %s\n", strings.Join(serviceSubcommands, ", "))
		writeServiceUsage(stderr)
		return code
	}
}

// serviceCommandResult prints the error of a failed command and returns the
// exit code of the process.
//
// The exit code is the kind of the failure and nothing else: a command line
// that could not be understood exits with 2, an operation that was
// understood but failed exits with 1. The kind travels with the error, so
// every subcommand classifies a failure the same way.
func serviceCommandResult(stderr io.Writer, err error) int {
	switch {
	case err == nil:
		return 0
	case errors.Is(err, errHelpRequested):
		return 0
	}

	fmt.Fprintf(stderr, "hfdesk service: %s\n", serviceErrorText(err))
	if isServiceUsageError(err) {
		return serviceUsageExitCode
	}
	return serviceFailureExitCode
}

// serviceErrorText renders an error of the service layer for the command
// line. The package prefixes its errors with "service: ", which the command
// line already carries in its own prefix, so it is removed once.
func serviceErrorText(err error) string {
	return strings.TrimPrefix(err.Error(), "service: ")
}

// writeServiceUsage prints the usage block of the service command family.
//
// An option of a single subcommand is listed with the subcommand that owns
// it: a family level option list would offer a flag that the other
// subcommands reject.
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

Options of the "install" subcommand:
  --listen HOST:PORT
                bind address of the instance (default %s)
  --service-user ACCOUNT
                account a machine-wide instance runs as
                (the machine-wide scope of Linux and macOS; the
                account cannot be chosen on Windows; default: the
                service account of the platform)
  --executable PATH
                absolute path of the binary to register
                (default: the invoked binary)

Exit codes:
  0  the operation succeeded
  1  the operation failed: the service is not installed, the elevation is
     missing, the supervisor refused the operation
  2  the command line could not be understood: an unknown subcommand or
     flag, a relative path, an unusable --executable, an unparsable --listen

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
	fmt.Fprint(w, "\nExit codes: 0 the operation succeeded, 1 the operation failed, 2 the command line could not be understood.\n")
}

// writeField writes one "Key: value" line of a summary block. The keys are
// padded to a common width, so the values of a block line up.
func writeField(w io.Writer, key, value string) {
	if value == "" {
		value = "unknown"
	}
	fmt.Fprintf(w, "%-14s %s\n", key+":", value)
}

// platformFacts are the facts of the platform that the command line decides
// with.
//
// The facts come from the service layer: it owns everything that depends on
// the operating system, so the command line never compares one itself.
type platformFacts struct {
	// defaultScope is the scope of an installation that does not ask for
	// one.
	defaultScope service.Scope
	// supportsUserScope reports whether a per-user installation exists.
	supportsUserScope bool
	// supportsNamedServiceAccount reports whether the account of a
	// machine-wide instance can be named.
	supportsNamedServiceAccount bool
}

// hostPlatform holds the facts of the platform the command line runs on.
//
// The variable is the seam of the tests: the decisions of the command line
// that depend on the platform (the default scope, the scope of an
// installation, the account of an instance) are checked on every platform,
// so the behaviour of a Windows instance is covered by a test that runs on
// Linux as well.
var hostPlatform = platformFactsOfTheHost()

// platformFactsOfTheHost returns the facts of the platform of the process.
func platformFactsOfTheHost() platformFacts {
	return platformFacts{
		defaultScope:                service.DefaultScope(),
		supportsUserScope:           service.SupportsUserScope(),
		supportsNamedServiceAccount: service.SupportsNamedServiceAccount(),
	}
}

// defaultScope returns the scope of an installation that does not ask for
// one.
//
// The default scope is a fact of the platform: the service layer owns it.
func defaultScope() service.Scope {
	return hostPlatform.defaultScope
}

// resolveScope turns the scope selector of the command line into the scope
// of the installation.
//
// The selectors are mutually exclusive: two scopes are two installations,
// so an ambiguous command line is rejected instead of picking one of them.
//
// A rejected selector is a command line that could not be understood, so the
// error carries that kind.
func resolveScope(user, system bool) (service.Scope, error) {
	switch {
	case user && system:
		return "", newServiceUsageError(errors.New("--user and --system are mutually exclusive, choose one scope"))
	case user:
		if !hostPlatform.supportsUserScope {
			return "", newServiceUsageError(errors.New("--user is not available on windows: the service control manager registers machine-wide services only, use --system"))
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
	fs.BoolVar(&flags.user, userScopeFlag, false, "per-user installation (the default on Linux and macOS)")
	fs.BoolVar(&flags.system, systemScopeFlag, false, "machine-wide installation (the default on Windows)")
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
//
// A rejected path is a command line that could not be understood, so the
// error carries that kind.
func validateAbsolutePaths(paths []pathFlag) error {
	for _, p := range paths {
		if p.value == "" {
			continue
		}
		if !filepath.IsAbs(p.value) {
			return newServiceUsageError(fmt.Errorf("%s %q must be an absolute path", p.name, p.value))
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
//
// A rejected address is a command line that could not be understood, so the
// error carries that kind.
func validateListen(listen string) (string, error) {
	host, port, err := service.ParseListen(listen)
	if err != nil {
		return "", newServiceUsageError(err)
	}
	return net.JoinHostPort(host, strconv.Itoa(port)), nil
}

// resolveExecutable returns the absolute path of the binary the
// registration points at.
//
// An explicit path wins: the user names the stable path of the
// installation, which may differ from the binary they happen to run.
//
// Without an explicit path the invocation path is preferred: the path the
// user typed is the name the installation is known by, and a symbolic link
// on that path keeps its name in the registration. The invocation path is
// only usable when it names a file: a bare name ("hfdesk") comes from the
// search path of the shell and must not be resolved against the working
// directory of the install command.
//
// The resolved path of the running binary (os.Executable) is the fallback.
// It is always a real file, but it is the target of a symbolic link, so a
// registration that uses it points at a version-stamped release artifact
// instead of at the stable name of the installation.
//
// A named path that cannot be registered is a command line that could not be
// understood, so its error carries that kind. A binary of the process that
// cannot be located is not a mistake of the command line: the operation
// failed, so its error keeps the kind of a failed operation.
func resolveExecutable(explicit, invocation string) (string, error) {
	if explicit != "" {
		return validateExecutablePath(explicit)
	}
	if path, ok := invocationExecutable(invocation); ok {
		return path, nil
	}
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

// invocationExecutable returns the absolute path of the invoked binary when
// the invocation path is usable.
//
// The invocation path is usable when it names a path (it contains a
// separator), so a bare name from the search path of the shell is never
// resolved against the working directory, and when it is an existing
// regular file.
func invocationExecutable(invocation string) (string, bool) {
	if invocation == "" || !hasPathSeparator(invocation) {
		return "", false
	}
	abs, err := filepath.Abs(invocation)
	if err != nil {
		return "", false
	}
	info, err := os.Stat(abs)
	if err != nil || !info.Mode().IsRegular() {
		return "", false
	}
	return abs, true
}

// hasPathSeparator reports whether the path names a file instead of a
// command of the search path.
//
// Both separators are accepted, so a path written with the separator of the
// other platform is not mistaken for a bare name.
func hasPathSeparator(path string) bool {
	return strings.ContainsRune(path, '/') || strings.ContainsRune(path, filepath.Separator)
}

// validateExecutablePath checks that the path can be registered as the
// program of the instance and returns it.
//
// The path must be absolute (a supervised process must not resolve a
// relative path against the working directory of the supervisor), it must
// exist and it must be a regular file.
//
// The execute bit is not a check of the command line: it is a fact of the
// platform, and the platform manager owns it. Windows reports a regular
// file without any execute bit, so a check of the permission bits would
// refuse every binary of a Windows installation. The managers of the
// platforms that require the bit enforce it themselves.
//
// A rejected path is a command line that could not be understood, so the
// error carries that kind.
func validateExecutablePath(path string) (string, error) {
	if !filepath.IsAbs(path) {
		return "", newServiceUsageError(fmt.Errorf("the executable %q must be an absolute path", path))
	}
	info, err := os.Stat(path)
	if err != nil {
		return "", newServiceUsageError(fmt.Errorf("the executable %q is not usable: %w", path, err))
	}
	if !info.Mode().IsRegular() {
		return "", newServiceUsageError(fmt.Errorf("the executable %q is not a regular file", path))
	}
	return path, nil
}

// installFlags holds the flags of the "service install" command.
type installFlags struct {
	scopeFlags
	listen      string
	config      string
	stateDir    string
	logDir      string
	cacheDir    string
	hubDir      string
	localDir    string
	serviceUser string
	executable  string
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
//
// The cache root and the hub directory are one layout, so an explicit cache
// root moves the hub directory with it: a registration that names a cache
// root and the hub of another tree would write the blobs of the instance
// into a directory its own cache root does not contain. A hub directory that
// was named explicitly is honoured as given: it separates the two locations
// on purpose, the same independence HF_HOME and HF_HUB_CACHE have.
//
// The service account belongs to the machine-wide scope only: a user scope
// instance runs as the user itself, so naming an account there is a
// mistake of the command line and not a default.
//
// A named account of a user scope instance is a command line that could not
// be understood, so the error carries that kind.
//
// The account of a machine-wide instance cannot be chosen on Windows: the
// service control manager registers the instance as the built-in
// LocalService account, and a service under a user account would need a
// credential, which must never reach the command line of an installation.
// A named account there is a command line that could not be understood as
// well, so its error carries that kind too.
func buildInstallOptions(scope service.Scope, flags installFlags, executable string) (service.InstallOptions, error) {
	if flags.serviceUser != "" {
		if scope == service.ScopeUser {
			return service.InstallOptions{}, newServiceUsageError(errors.New("--service-user applies to a machine-wide installation only: a user scope instance runs as the user itself, drop the option or use --system"))
		}
		if !hostPlatform.supportsNamedServiceAccount {
			return service.InstallOptions{}, newServiceUsageError(fmt.Errorf("--service-user is not available on windows: the machine-wide instance runs as the built-in %s account and the account cannot be chosen on this platform (a service under a user account needs a credential, and a credential must never reach a command line), drop the option", windowsServiceAccount))
		}
	}

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
		if flags.hubDir == "" {
			hubDir = filepath.Join(cacheRoot, "hub")
		}
	}
	if flags.hubDir != "" {
		hubDir = flags.hubDir
	}

	listen := flags.listen
	if listen == "" {
		listen = service.DefaultListen
	}

	return service.InstallOptions{
		Scope:       scope,
		Executable:  executable,
		Listen:      listen,
		Paths:       paths,
		CacheDir:    cacheRoot,
		HubDir:      hubDir,
		LocalDir:    flags.localDir,
		ServiceUser: flags.serviceUser,
	}, nil
}

// serviceInstall registers the service with the platform supervisor of the
// scope.
//
// The command validates its own arguments before it touches the system. It
// never starts the instance itself: the supervisor may start a registration
// that asks for it (a LaunchAgent with RunAtLoad starts it), so the summary
// reports the state that was reached instead of a fixed claim.
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
	fs.StringVar(&flags.serviceUser, "service-user", "", "account the machine-wide instance runs as (the machine-wide scope of Linux and macOS; the account cannot be chosen on Windows)")
	fs.StringVar(&flags.executable, "executable", "", "absolute path of the binary to register (default: the invoked binary)")

	if err := fs.Parse(args); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			writeSubcommandUsage(stdout, fs, "Register HFDesk with the platform supervisor of the scope.\n\nThe registration is written. The summary reports the state the supervisor\nreached: a registration that starts the instance starts it.")
			return errHelpRequested
		}
		return newServiceUsageError(fmt.Errorf("invalid command line: %w", err))
	}
	if rest := fs.Args(); len(rest) > 0 {
		return newServiceUsageError(fmt.Errorf("unexpected argument %q, the command takes flags only", rest[0]))
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

	// The registration points at the path the user knows the installation
	// by: the invoked binary, or the path the user named explicitly.
	executable, err := resolveExecutable(flags.executable, os.Args[0])
	if err != nil {
		return err
	}

	opts, err := buildInstallOptions(scope, flags, executable)
	if err != nil {
		return err
	}

	mgr, err := newServiceManager(scope)
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
// The path of the registration and the state of the instance are known only
// after the installation, so both are read back from the supervisor.
//
// The scope of the instance is carried into the hint that starts it: the
// hint has to start the instance that was registered and not the one of
// the default scope of the platform.
//
// The cache root and the hub directory are the effective ones: the config
// file of the instance wins over the registered values, because the cache
// directory is changed in the settings of the instance and the change has to
// survive a restart.
func writeInstallSummary(w io.Writer, ctx context.Context, mgr service.Manager, scope service.Scope, opts service.InstallOptions) {
	status, statusErr := mgr.Status(ctx)

	cacheRoot, hubDir := effectiveCacheDirs(opts)

	writeField(w, "Service", service.ServiceName)
	writeField(w, "Scope", string(scope))
	writeField(w, "Account", installAccount(scope, opts.ServiceUser))
	writeField(w, "Registration", registrationPath(status))
	writeField(w, "Executable", opts.Executable)
	writeField(w, "Listen", opts.Listen)
	writeField(w, "Config file", opts.Paths.ConfigFile)
	writeField(w, "State dir", opts.Paths.StateDir)
	writeField(w, "Log dir", opts.Paths.LogDir)
	writeField(w, "Cache root", cacheRoot)
	writeField(w, "Hub dir", hubDir)

	if warning := nonLoopbackWarning(opts.Listen); warning != "" {
		fmt.Fprintln(w)
		fmt.Fprint(w, warning)
	}
	if warning := upgradeWarning(opts.Executable); warning != "" {
		fmt.Fprintln(w)
		fmt.Fprint(w, warning)
	}

	fmt.Fprintln(w)
	fmt.Fprintln(w, "The service was registered.")
	writeInstallState(w, scope, status, statusErr)
}

// installAccount returns the account the registered instance runs as.
//
// A user scope instance runs as the user itself: it keeps the identity of
// the session and the access to the own cache of the user.
//
// A machine-wide instance runs as the dedicated service account of the
// platform: the account the user named, or the default of the platform.
// Windows registers the built-in LocalService account, which has no name
// of its own: there the account of the registration is the one that is
// reported, and an account of the command line is never reported, because
// the registration does not use it.
func installAccount(scope service.Scope, serviceUser string) string {
	switch {
	case scope == service.ScopeUser:
		return invokingUser()
	case !hostPlatform.supportsNamedServiceAccount:
		return windowsServiceAccount
	case serviceUser != "":
		return serviceUser
	default:
		return service.DefaultServiceUser()
	}
}

// invokingUser returns the name of the user that runs the command.
//
// The name is empty when it cannot be determined: a summary that names no
// account says "unknown" instead of guessing one.
func invokingUser() string {
	if u, err := user.Current(); err == nil && u.Username != "" {
		return u.Username
	}
	if name := os.Getenv("USER"); name != "" {
		return name
	}
	return ""
}

// effectiveCacheDirs returns the cache root and the hub directory the
// instance uses.
//
// The rule is the one the headless host applies to the running instance: a
// config file that defines a cache root owns the whole cache layout of the
// instance, so the hub directory is the "hub" child of that root. Without a
// cache root from the config file the registration keeps its hub directory,
// and a registration without one falls back to the "hub" child of the
// effective cache root.
//
// The two directories are reported as one pair: a summary that named a cache
// root without its hub directory would hide a layout that sends the blobs of
// the instance somewhere else.
func effectiveCacheDirs(opts service.InstallOptions) (string, string) {
	cacheRoot := service.EffectiveCacheDir(opts.Paths.ConfigFile, opts.CacheDir)

	// The registered value is empty when the config file of the instance
	// defines a cache root of its own: the config file then owns the whole
	// layout of the instance.
	if configured := service.EffectiveCacheDir(opts.Paths.ConfigFile, ""); configured != "" {
		return configured, filepath.Join(configured, "hub")
	}

	hubDir := opts.HubDir
	if hubDir == "" && cacheRoot != "" {
		hubDir = filepath.Join(cacheRoot, "hub")
	}
	return cacheRoot, hubDir
}

// writeInstallState prints the state the instance reached after the
// registration and the hint that starts it.
//
// The state is read back from the supervisor: a registration of some
// platforms starts the instance, so the summary cannot claim that the
// instance was not started. A state the supervisor did not report is
// reported as unknown, and the successful registration is reported
// nevertheless.
//
// The hint names the scope of the instance that was registered: the
// default scope of the platform is not the scope of every installation,
// so a hint without the scope would start another instance than the one
// that was just registered.
func writeInstallState(w io.Writer, scope service.Scope, status service.Status, statusErr error) {
	switch {
	case statusErr != nil:
		writeField(w, "State", fmt.Sprintf("unknown (the state could not be determined: %s)", serviceErrorText(statusErr)))
	case status.State == "":
		writeField(w, "State", "unknown (the supervisor did not report a state)")
	default:
		writeField(w, "State", string(status.State))
	}

	// The hint is printed for every state that is not an active one. The
	// instance then has to be started by the user, and a state that could
	// not be read back at all does not say that the instance runs.
	if status.State != service.StateRunning && status.State != service.StateStarting {
		fmt.Fprintf(w, "Start it with: %s\n", startCommand(scope))
	}
}

// startCommand returns the command that starts the instance of the scope.
//
// The scope selector is part of the command: the default scope of the
// platform is the user scope on Linux and macOS and the machine-wide scope
// on Windows, so a command without the selector would start another
// instance than the one that was registered.
//
// A scope the family does not know has no selector: the command then
// starts the default scope of the platform.
func startCommand(scope service.Scope) string {
	command := "hfdesk service start"
	switch scope {
	case service.ScopeUser:
		return command + " --" + userScopeFlag
	case service.ScopeSystem:
		return command + " --" + systemScopeFlag
	default:
		return command
	}
}

// registrationPath returns the path of the supervisor registration, or
// "unknown" when the supervisor did not report one.
func registrationPath(status service.Status) string {
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
	// report can name the directories the data stays in. The state of the
	// scope is read as well, so the report can state what the removal did.
	paths, cacheRoot, registered := instanceDataPaths(ctx, mgr, scope)

	if err := mgr.Uninstall(ctx); err != nil {
		return err
	}

	writeUninstallSummary(stdout, scope, paths, cacheRoot, registered)
	return nil
}

// writeUninstallSummary prints what was removed and what was kept.
//
// The summary states what the uninstallation did: a registration that was
// there is removed, and a scope without a registration has nothing to
// remove. A summary that always claims a removal hides the difference
// between the two.
//
// The directories of the kept data are named, so the reader knows where the
// models, the cache, the config file, the job history and the logs of the
// instance stay.
func writeUninstallSummary(w io.Writer, scope service.Scope, paths runtimepaths.Paths, cacheRoot string, registered bool) {
	if registered {
		fmt.Fprintf(w, "The registration of %s (%s scope) was removed.\n", service.ServiceName, scope)
	} else {
		fmt.Fprintf(w, "There was no registration of %s (%s scope) to remove.\n", service.ServiceName, scope)
	}
	fmt.Fprintln(w, "The user data was kept: models, cache, config, jobs/history and logs are still there.")
	writeField(w, "Config file", paths.ConfigFile)
	writeField(w, "State dir", paths.StateDir)
	writeField(w, "Log dir", paths.LogDir)
	writeField(w, "Cache root", cacheRoot)
}

// instanceDataPaths returns the runtime paths and the cache root of the
// instance of the scope, and whether the scope had a registration.
//
// The registration is read first, so the report names the directories the
// data of the registration stays in. A registration that cannot be read
// falls back to the default paths of the scope.
//
// The cache root is the effective one of the instance: the default of the
// scope and the personal cache of the invoking user are never reported
// instead, because the instance does not use them.
//
// A scope without a registration is reported as such: the supervisor answers
// it with the state "not-installed", and the removal of such a scope has
// nothing to remove. A scope whose state the supervisor did not report
// counts as a registration: the removal succeeded, so whatever was
// registered is gone.
func instanceDataPaths(ctx context.Context, mgr service.Manager, scope service.Scope) (runtimepaths.Paths, string, bool) {
	paths, err := service.DefaultPaths(scope)
	if err != nil {
		paths = runtimepaths.Paths{}
	}
	cacheRoot := cacheRootNotRecorded
	registered := true
	if status, statusErr := mgr.Status(ctx); statusErr == nil {
		registered = status.State != service.StateNotInstalled
		if status.Paths.ConfigFile != "" {
			paths.ConfigFile = status.Paths.ConfigFile
		}
		if status.Paths.StateDir != "" {
			paths.StateDir = status.Paths.StateDir
		}
		if status.Paths.LogDir != "" {
			paths.LogDir = status.Paths.LogDir
		}
		cacheRoot = effectiveCacheRoot(status)
	}
	return paths, cacheRoot, registered
}

// effectiveCacheRoot returns the Hugging Face cache root the instance uses.
//
// The supervisor reports the cache root of the registration, and the config
// file of the instance wins over it: the cache directory is changed in the
// settings of the instance, and the changed value has to survive a restart
// of the instance.
//
// A cache root that neither source names is not recorded. The default of
// the scope and the personal cache of the invoking user are never reported
// instead: the instance does not use them, so naming them would send the
// reader to the wrong directory.
func effectiveCacheRoot(status service.Status) string {
	if dir := service.EffectiveCacheDir(status.Paths.ConfigFile, status.CacheDir); dir != "" {
		return dir
	}
	return cacheRootNotRecorded
}

// newLifecycleManager prepares a lifecycle command: it parses the scope
// selector and opens the manager of the scope.
//
// A command line the parser rejects is a command line that could not be
// understood, so its error carries that kind. An error of the platform
// manager is a failed operation and keeps the kind of one.
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
		return nil, "", newServiceUsageError(fmt.Errorf("invalid command line: %w", err))
	}
	if rest := fs.Args(); len(rest) > 0 {
		return nil, "", newServiceUsageError(fmt.Errorf("unexpected argument %q, the command takes flags only", rest[0]))
	}

	scope, err := resolveScope(flags.user, flags.system)
	if err != nil {
		return nil, "", err
	}
	mgr, err := newServiceManager(scope)
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
		"Report the scope, the state and the paths of the instance.\n\n"+
			"The exit code tells a script whether the instance is in a usable state:\n"+
			"0 for a running, stopped, starting or stopping instance, 1 for an\n"+
			"instance that is not installed, broken, failed or in an unknown state.")
	if err != nil {
		return serviceCommandResult(stderr, err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	status, statusErr := mgr.Status(ctx)
	renderStatus(stdout, scope, status)
	if statusErr != nil {
		return serviceCommandResult(stderr, statusErr)
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
	// The cache root is not derived from the paths above: the defaults of
	// the scope describe the paths such an instance would use, not the
	// cache root it uses. The effective cache root of the instance is
	// reported instead.
	cacheRoot := effectiveCacheRoot(status)

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
		return newServiceUsageError(fmt.Errorf("invalid command line: %w", err))
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
//
// An argument vector the parser rejects is a command line that could not be
// understood, so its error carries that kind. A host that cannot run is a
// failed operation and keeps the kind of one.
func runServiceHost(ctx context.Context, args []string) error {
	opts, err := service.ParseRunArgs(args)
	if err != nil {
		return newServiceUsageError(err)
	}
	return service.RunProcess(ctx, opts)
}
