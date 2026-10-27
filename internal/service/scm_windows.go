// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"golang.org/x/sys/windows/svc"
	"golang.org/x/sys/windows/svc/eventlog"
)

// The Windows service mode of HFDesk is a registration in the service
// control manager.
//
// The service control manager owns the lifecycle of the instance: it starts
// the registered program, it asks it for the states and it requests the
// stop. HFDesk stays a normal foreground process: it is never forked, never
// daemonized itself, never writes a PID file and never runs through a
// shell.
//
// The instance runs as the built-in account LocalService, the least
// privileged of the service accounts of the operating system. It has no
// password, it is not a member of the administrators and it holds only the
// rights a service needs. The account has a profile of its own, so the
// instance never sees the desktop, the profile or the files of the user
// who installed it.
//
// The diagnostics of the instance live in the Windows Event Log: the
// lifecycle messages and a fatal startup error are written to the own
// source of the service, so a machine without a terminal can be inspected
// with the usual tools of the platform.
const (
	// scmEventSource is the event log source the lifecycle messages of a
	// HFDesk service are written to.
	scmEventSource = "HFDesk"

	// scmFallbackEventSource is the source that is used when the own
	// source of the service is not registered. The Application log
	// accepts the message of every source, so a diagnostic is never
	// lost completely.
	scmFallbackEventSource = "Application"

	// scmEventIDInfo, scmEventIDWarning and scmEventIDError are the event
	// ids of the three kinds of messages. The ids stay inside the range
	// of EventCreate.exe (1 to 1000), which is the message file the
	// event source is registered with.
	scmEventIDInfo    = 1
	scmEventIDWarning = 2
	scmEventIDError   = 3
)

const (
	// scmReadinessTimeout bounds the wait for the HTTP server of the
	// instance to become usable.
	scmReadinessTimeout = 30 * time.Second

	// scmReadinessInterval is the pause between two readiness probes.
	scmReadinessInterval = 100 * time.Millisecond
)

const (
	// scmExitCodeOK is the exit code of an instance that ended the way it
	// was asked to end.
	//
	// The code is reported with the stopped state as well: a stop the user
	// requested is a clean end, whatever the orderly shutdown observed on
	// the way, and the platform must never turn it into a recovery.
	scmExitCodeOK uint32 = 0

	// scmExitCodeFailure is the exit code of an instance that failed.
	//
	// The platform performs the recovery actions of a registration for an
	// instance that ends without reporting its stop, and, with the non-crash
	// switch of the registration set, for an instance that reports a
	// non-zero exit code. The failed paths of the handler therefore end
	// with this code and report it with the stopped state, so a failure of
	// the instance reaches the service control manager even when it reads
	// nothing but the reported status.
	scmExitCodeFailure uint32 = 1
)

// runAsWindowsService runs HFDesk as the process of a registered service.
//
// The function connects the process to the service dispatcher of the
// service control manager and blocks until the instance stops. The
// dispatcher owns the lifecycle: it receives the states of the instance
// and it requests the stop.
func runAsWindowsService(opts RunOptions) error {
	handler := newSCMHandler(opts)
	if err := svc.Run(ServiceName, handler); err != nil {
		logEvent(eventLogError, fmt.Sprintf("hfdesk: the service dispatcher failed: %v", err))
		return fmt.Errorf("service: run as a windows service: %w", err)
	}
	return nil
}

// scmHandler is the service handler of a HFDesk instance.
//
// The handler owns the lifecycle of the process the service control manager
// started: it reports the states of the instance to the manager, it starts
// the headless host, it waits until the HTTP server really answers and it
// drives the graceful shutdown of the server.
//
// The handler never opens a browser and never creates a tray icon: the
// instance is supervised and there is no desktop session to interact with.
type scmHandler struct {
	// opts is the run options of the instance. They are the fallback of
	// the argument resolution: a process that carries no parsable
	// argument vector of its own still runs with them.
	opts RunOptions

	// run starts the host of the instance.
	//
	// The field is the seam of the test: the handler runs the host of the
	// service layer, and a test can drive the lifecycle of the handler
	// with a host of its own, so the wait for the shutdown can be
	// observed without a server of its own.
	run func(context.Context, RunOptions) error

	// readinessTimeout bounds the wait for the HTTP server.
	readinessTimeout time.Duration

	// readinessInterval is the pause between two readiness probes.
	readinessInterval time.Duration

	// shutdownTimeout bounds the wait for the graceful shutdown.
	//
	// The wait is the shared budget of the service layer: the server
	// publishes the budget of its own orderly shutdown and the supervisor
	// waits for that total plus a margin. A number of its own here would
	// be a second copy of the same decision, and the platforms would drift
	// apart.
	shutdownTimeout time.Duration
}

// newSCMHandler returns the handler of an instance with the default
// timeouts.
func newSCMHandler(opts RunOptions) *scmHandler {
	return &scmHandler{
		opts:              opts,
		run:               Run,
		readinessTimeout:  scmReadinessTimeout,
		readinessInterval: scmReadinessInterval,
		shutdownTimeout:   ShutdownBudget,
	}
}

// Execute runs the lifecycle of the service.
//
// The method is called by the dispatcher of the service control manager. It
// reports the states of the instance on the changes channel, it reacts to
// the control requests of the manager on the request channel and it returns
// the exit code of the instance.
//
// The registration of the instance carries the failure recovery of the
// platform, and the recovery for a non-crash failure keys on the exit code the
// instance reports. The two ends of the instance therefore never share a
// code:
//
//   - a stop the manager requested is a clean end. The instance reports the
//     stopped state with the exit code 0 and returns 0, even when the orderly
//     shutdown observed an error: the error is logged to the event log, and a
//     service the user stopped is never turned into a recovery, because the
//     platform would restart what the user asked to go away;
//   - a start that failed, and a host that failed, are failures. The instance
//     reports the stopped state with the exit code 1 and returns 1, so the
//     failure reaches the service control manager even when it reads nothing
//     but the reported status, and the recovery of the platform restarts the
//     instance. An orderly shutdown that does not finish in time is not one of
//     them: the instance was asked to stop, and the logged error is the
//     evidence of how cleanly it did;
//   - an instance the platform kills reports no status at all, and the
//     platform applies the recovery actions to it.
func (h *scmHandler) Execute(args []string, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, uint32) {
	// The instance does not run yet: the manager waits for the report of
	// the running state before it hands the instance over to the machine.
	changes <- svc.Status{State: svc.StartPending}

	opts, err := h.resolveOptions(args)
	if err != nil {
		logEvent(eventLogError, fmt.Sprintf("hfdesk: the service could not be started: %v", err))
		changes <- svc.Status{State: svc.Stopped, Win32ExitCode: scmExitCodeFailure}
		return false, scmExitCodeFailure
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	host := h.startHost(ctx, opts)

	// The instance is only reported as running when the HTTP server
	// really answers: a process that is alive but does not serve is not a
	// running service.
	stopRequested, err := h.waitForReady(ctx, opts.Listen, host, r, changes)
	if err != nil {
		logEvent(eventLogError, fmt.Sprintf("hfdesk: the service could not be started: %v", err))
		// The host is given the time to finish its own shutdown before
		// the instance is reported as stopped: the drain of the HTTP
		// server and the write back of the job manager belong to the
		// lifecycle of the instance, and the service control manager
		// must never be told that the instance is stopped while it is
		// still busy.
		//
		// The wait is the one of the normal stop path and it is bounded
		// by the shared budget of the service layer.
		ended := host.ended()
		if shutdownErr := h.waitForShutdown(cancel, host); shutdownErr != nil && !ended {
			// The host was still running when the readiness failed, so
			// the error is about the shutdown and not about the start
			// that failed already.
			logEvent(eventLogError, fmt.Sprintf("hfdesk: the instance did not stop cleanly: %v", shutdownErr))
		}
		// The start failed, so the instance ends as a failure: the
		// recovery of the platform restarts it instead of leaving a
		// service that does not serve.
		changes <- svc.Status{State: svc.Stopped, Win32ExitCode: scmExitCodeFailure}
		return false, scmExitCodeFailure
	}
	if stopRequested {
		// The manager asked the instance to stop while it was starting.
		h.stopInstance(cancel, host, changes)
		return false, scmExitCodeOK
	}

	logEvent(eventLogInfo, fmt.Sprintf("hfdesk: the service is running on %s", opts.Listen))
	changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}

	for {
		select {
		case request, ok := <-r:
			if !ok {
				// The dispatcher closed the channel: the instance is
				// asked to stop.
				h.stopInstance(cancel, host, changes)
				return false, scmExitCodeOK
			}
			switch request.Cmd {
			case svc.Interrogate:
				// The manager asks for the current status. The state
				// is reported again and the instance keeps running.
				changes <- svc.Status{State: svc.Running, Accepts: svc.AcceptStop | svc.AcceptShutdown}
			case svc.Stop, svc.Shutdown:
				h.stopInstance(cancel, host, changes)
				return false, scmExitCodeOK
			}
		case <-host.done:
			// The host ended on its own: the instance does not run any
			// more.
			if err := host.err; err != nil {
				logEvent(eventLogError, fmt.Sprintf("hfdesk: the service failed: %v", err))
				changes <- svc.Status{State: svc.Stopped, Win32ExitCode: scmExitCodeFailure}
				return false, scmExitCodeFailure
			}
			logEvent(eventLogInfo, "hfdesk: the service stopped on its own")
			changes <- svc.Status{State: svc.Stopped, Win32ExitCode: scmExitCodeOK}
			return false, scmExitCodeOK
		}
	}
}

// scmHost is the goroutine of the host of an instance.
//
// The result of the host is kept next to the signal of its end: the error
// of a host that ended already is read from the value, and not from a
// channel that carries it once. A wait for the shutdown would otherwise
// wait for a value that was consumed somewhere else, and it would block
// until the budget of the wait is over.
type scmHost struct {
	// done is closed when the host ended.
	done chan struct{}

	// err holds the error the host ended with. It is written before done
	// is closed, so it is safe to read it once the host ended.
	err error
}

// startHost runs the host of the instance in a goroutine of its own.
//
// The goroutine ends with the host: it keeps the error of the host and it
// closes the signal of the end of the host when the host returned.
func (h *scmHandler) startHost(ctx context.Context, opts RunOptions) *scmHost {
	host := &scmHost{done: make(chan struct{})}
	run := h.run
	if run == nil {
		run = Run
	}
	go func() {
		host.err = run(ctx, opts)
		close(host.done)
	}()
	return host
}

// ended reports whether the host ended already.
func (h *scmHost) ended() bool {
	select {
	case <-h.done:
		return true
	default:
		return false
	}
}

// stopInstance drives the graceful shutdown of the instance.
//
// The application context is cancelled first: the server quiesces, drains
// its connections and closes the job manager. Only when that is done the
// instance is reported as stopped, so the service control manager never
// kills a process that is still busy.
//
// The stop is the end the manager asked for, so it is reported with the exit
// code 0 whatever the orderly shutdown observed. A shutdown that does not
// finish in time, or that reports the error of the server, is written to the
// event log and never reported as a failure of the instance: the registration
// of the instance carries the recovery of the platform for a non-crash
// failure, and a non-zero code here would restart a service the user asked to
// stop.
func (h *scmHandler) stopInstance(cancel context.CancelFunc, host *scmHost, changes chan<- svc.Status) {
	changes <- svc.Status{State: svc.StopPending}
	logEvent(eventLogInfo, "hfdesk: the service is stopping")
	if err := h.waitForShutdown(cancel, host); err != nil {
		logEvent(eventLogError, fmt.Sprintf("hfdesk: the service did not stop cleanly: %v", err))
	}
	logEvent(eventLogInfo, "hfdesk: the service is stopped")
	changes <- svc.Status{State: svc.Stopped, Win32ExitCode: scmExitCodeOK}
}

// waitForShutdown cancels the application context and waits for the
// graceful shutdown of the instance.
//
// The wait is bounded: a server that does not finish its drain must not
// keep the instance in the stopping state forever.
//
// The bound is the shared budget of the service layer, so the handler
// never gives up before the server had the time to finish its own
// shutdown.
func (h *scmHandler) waitForShutdown(cancel context.CancelFunc, host *scmHost) error {
	timeout := h.shutdownTimeout
	if timeout <= 0 {
		timeout = ShutdownBudget
	}
	cancel()
	select {
	case <-host.done:
		return host.err
	case <-time.After(timeout):
		return fmt.Errorf("the instance did not stop within %s", timeout)
	}
}

// resolveOptions determines the run options of the instance.
//
// The service control manager hands the handler the name of the service and
// the start parameters of the start request, not the argument vector of the
// registration: the registered command line is the command line of the
// process itself. Every vector that can carry the options is accepted, so
// the instance runs with the options of its registration no matter how the
// process was started.
func (h *scmHandler) resolveOptions(args []string) (RunOptions, error) {
	var firstErr error
	for _, candidate := range scmArgumentCandidates(args, os.Args) {
		opts, err := ParseRunArgs(candidate)
		if err == nil {
			return opts, nil
		}
		if firstErr == nil {
			firstErr = err
		}
	}
	// The caller handed the options over already: they are the fallback
	// for a process that carries no parsable vector of its own.
	if h.opts.Paths.ConfigFile != "" {
		return h.opts, nil
	}
	if firstErr != nil {
		return RunOptions{}, firstErr
	}
	return RunOptions{}, errors.New("the service carries no usable argument vector")
}

// scmArgumentCandidates returns the argument vectors the options of the
// instance can be parsed from, in the order they are tried.
//
// The first candidate holds the start parameters of the start request: the
// service control manager hands them over next to the name of the service.
// The second candidate holds the whole vector of the handler. The third one
// holds the command line of the process, which is the registered command
// line of the instance.
//
// The candidates are tried in that order, so an explicit instruction of the
// caller always wins over the registration.
func scmArgumentCandidates(args []string, processArgs []string) [][]string {
	var candidates [][]string
	if len(args) > 1 {
		candidates = append(candidates, args[1:])
	}
	if len(args) > 0 {
		candidates = append(candidates, args)
	}
	if len(processArgs) > 1 {
		candidates = append(candidates, processArgs[1:])
	}
	return candidates
}

// waitForReady waits until the HTTP server of the instance answers.
//
// The wait is bounded and it watches three things:
//
//   - the host: an instance that fails while it starts is a failed start
//     and not a start that timed out;
//   - the control requests: a stop request that arrives while the instance
//     starts must not wait for the startup to finish;
//   - the context of the instance.
//
// The first result reports whether the manager asked the instance to stop
// while it was starting.
func (h *scmHandler) waitForReady(ctx context.Context, listen string, host *scmHost, r <-chan svc.ChangeRequest, changes chan<- svc.Status) (bool, error) {
	timeout := h.readinessTimeout
	if timeout <= 0 {
		timeout = scmReadinessTimeout
	}
	interval := h.readinessInterval
	if interval <= 0 {
		interval = scmReadinessInterval
	}
	deadline := time.Now().Add(timeout)
	for {
		if err := serviceReadinessProbe(listen); err == nil {
			return false, nil
		}
		select {
		case <-host.done:
			// The host ended before the HTTP server answered: the
			// instance failed to start.
			err := host.err
			if err == nil {
				return false, errors.New("the instance ended before the HTTP server answered")
			}
			return false, err
		case request, ok := <-r:
			if !ok {
				// The dispatcher closed the channel: the instance is
				// asked to stop.
				return true, nil
			}
			switch request.Cmd {
			case svc.Stop, svc.Shutdown:
				return true, nil
			case svc.Interrogate:
				// The manager asks for the current status. The
				// instance is still starting, so the state is
				// reported again.
				changes <- svc.Status{State: svc.StartPending}
			}
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(interval):
		}
		if time.Now().After(deadline) {
			return false, fmt.Errorf("the HTTP server did not answer on %s within %s", listen, timeout)
		}
	}
}

// eventLogLevel is the kind of a message of the Windows Event Log.
type eventLogLevel int

const (
	// eventLogInfo is an informational message.
	eventLogInfo eventLogLevel = iota
	// eventLogWarning is a warning.
	eventLogWarning
	// eventLogError is an error.
	eventLogError
)

// logEvent writes one message to the Windows Event Log.
//
// The log is opened, written and closed again for every message: a service
// process lives for hours and an open handle would keep the source
// registered for the whole lifetime of the process.
//
// A log that cannot be opened is not a lost message: the own source of the
// service is tried first, the Application log is the second choice and the
// standard error of the process is the last one.
//
// The message never carries a credential: the arguments of the instance
// carry paths and a bind address only.
func logEvent(level eventLogLevel, message string) {
	if err := writeEventLog(scmEventSource, level, message); err == nil {
		return
	}
	if err := writeEventLog(scmFallbackEventSource, level, message); err == nil {
		return
	}
	fmt.Fprintln(os.Stderr, message)
}

// writeEventLog writes one message to the event log of a source.
//
// The log is opened and closed again for the message. An error is returned
// when the source cannot be opened or when the message is refused.
func writeEventLog(source string, level eventLogLevel, message string) error {
	log, err := eventlog.Open(source)
	if err != nil {
		return err
	}
	defer log.Close()

	switch level {
	case eventLogWarning:
		return log.Warning(scmEventIDWarning, message)
	case eventLogError:
		return log.Error(scmEventIDError, message)
	default:
		return log.Info(scmEventIDInfo, message)
	}
}
