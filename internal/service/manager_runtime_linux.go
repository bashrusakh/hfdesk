// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package service

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"math"
	"os"
	"os/exec"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/bashrusakh/hfdesk/pkg/hfdownloader"
	"golang.org/x/sys/unix"
)

// restartAndVerify restarts the unit and verifies the supervisor state, the
// actual HFDesk MainPID/argv, and the readiness response of that same stable
// process. A dynamic Listen value comes from the runtime argv, never the source.
func (m *systemdManager) restartAndVerify(ctx context.Context, executable, listen, expectedVersion string, dynamicListen bool) (bool, error) {
	if _, err := m.runSystemctl(ctx, "restart", unitName); err != nil {
		return false, fmt.Errorf("service: restart %s: %w", unitName, err)
	}
	return m.verifyRunningExecutableWithListenSource(ctx, executable, listen, expectedVersion, dynamicListen)
}

func (m *systemdManager) runtimeRunOptions(ctx context.Context) (RunOptions, uint32, error) {
	ctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	properties, err := m.loadedServiceProperties(ctx, unitName)
	if err != nil {
		return RunOptions{}, 0, fmt.Errorf("read the active service process from systemd: %w", err)
	}
	mainPID, err := systemdMainPIDFromProperties(properties)
	if err != nil {
		return RunOptions{}, 0, err
	}
	commandLine, err := os.ReadFile(filepath.Join("/proc", strconv.FormatUint(uint64(mainPID), 10), "cmdline"))
	if err != nil {
		return RunOptions{}, 0, fmt.Errorf("read the actual service process argv: %w", err)
	}
	if len(commandLine) == 0 {
		return RunOptions{}, 0, errors.New("the running service process exposed an empty argv")
	}
	argv := strings.Split(strings.TrimSuffix(string(commandLine), "\x00"), "\x00")
	if len(argv) < 2 || argv[0] == "" {
		return RunOptions{}, 0, errors.New("the running service process did not expose usable argv")
	}
	if options, err := ParseRunArgs(argv[1:]); err == nil {
		return options, mainPID, nil
	}
	// A shebang executable may put its script path before the service command.
	// Reuse the host parser rather than interpreting an environment expression.
	for i := 1; i+1 < len(argv); i++ {
		if argv[i] == "service" && argv[i+1] == "run" {
			options, err := ParseRunArgs(argv[i:])
			return options, mainPID, err
		}
	}
	return RunOptions{}, 0, errors.New("the running process argv did not expose HFDesk service run options")
}

// restartPollInterval is the interval between two checks whether a restarted
// instance runs the new executable.
const restartPollInterval = 100 * time.Millisecond

// restartVerifyWait returns the bounded wait for a restarted instance: the
// shutdown budget, because the new program cannot start before the old instance
// finished its orderly shutdown.
func (m *systemdManager) restartVerifyWait() time.Duration {
	if m.restartWait > 0 {
		return m.restartWait
	}
	return ShutdownBudget
}

// verifyRunningExecutable waits until the supervisor reports the expected
// program, a stable MainPID exposes HFDesk service-run arguments, and that
// process answers on its effective Listen address.
//
// systemctl show reports the configuration of the loaded unit, not the image of
// the process that runs it, so a registration that points at the new executable
// proves nothing while the old process still runs. The unit is of type simple,
// so the supervisor reports it active as soon as the process was forked, and a
// process that dies right after is active for a moment: the readiness probe
// therefore has to answer as well. A probe that does not answer within the wait
// fails the verification like a state that never becomes running, so the caller
// rolls the installation back instead of reporting an update that left no
// server behind.
//
// An empty executable skips only the path comparison. It does not waive the
// requirement for a live HFDesk process. The wait is bounded by the shutdown
// budget and the context of the installation.
//
// The registered command is compared program to program: the program is the
// token at the slot of the command's shape, so a recognized "env" wrapper is
// judged by the program it launches and never passes by its own wrapper path.
// The supervisor's path= stays part of the comparison as the raw first word of
// the command, which is the program of a plain command and the value a caller
// that verifies a command by its first token (the restore of a wrapped
// registration) passes. A command whose shape has no readable program slot
// fails with the wrapper path named as the executable it is.
func (m *systemdManager) verifyRunningExecutable(ctx context.Context, executable, listen, expectedVersion string) (bool, error) {
	return m.verifyRunningExecutableWithListenSource(ctx, executable, listen, expectedVersion, false)
}

func (m *systemdManager) verifyRunningExecutableWithListenSource(ctx context.Context, executable, listen, expectedVersion string, dynamicListen bool) (bool, error) {
	budget := m.restartVerifyWait()
	deadline := time.Now().Add(budget)
	expectedProgram := systemdLoadedExecutablePath(executable)
	address := ""
	if !dynamicListen {
		address = probeServiceAddress(listen)
	}
	detail := "the supervisor did not report a state"
	for {
		probeFailed := false
		var runtimeProcessErr error
		out, err := m.runSystemctl(ctx, "show", unitName,
			"--property=LoadState", "--property=ActiveState", "--property=SubState", "--property=ExecStart")
		if err != nil {
			detail = err.Error()
		} else {
			show := parseSystemdShow(out)
			path := execStartPath(out)
			program, programReadable := execStartProgram(out)
			programMatches := show.State == StateRunning && (executable == "" ||
				program == expectedProgram || path == expectedProgram)
			switch {
			case !programMatches && executable == "":
				detail = fmt.Sprintf("the supervisor reports the state %q", show.State)
			case !programMatches && !programReadable:
				detail = fmt.Sprintf("the supervisor reports the state %q with the executable %q of a command whose program slot cannot be read", show.State, path)
			case !programMatches:
				detail = fmt.Sprintf("the supervisor reports the state %q with the program %q", show.State, program)
			default:
				// Active unit metadata is not a running process: RemainAfterExit
				// can report active with MainPID=0. Use systemd's main process and
				// HFDesk's parser for static and expanded arguments alike.
				runOptions, mainPID, processErr := m.runtimeRunOptions(ctx)
				if processErr != nil {
					runtimeProcessErr = processErr
					probeFailed = true
					detail = fmt.Sprintf("the active unit's HFDesk process is not observable: %v", processErr)
					break
				}
				effectiveListen := runOptions.Listen
				address = probeServiceAddress(effectiveListen)
				if !dynamicListen && listenArg(effectiveListen) != listenArg(listen) {
					runtimeProcessErr = fmt.Errorf("the running process Listen %q does not match the static unit Listen %q", effectiveListen, listen)
					probeFailed = true
					detail = runtimeProcessErr.Error()
					break
				}
				verified, probeErr := serviceInstallProbe(effectiveListen, expectedVersion)
				if probeErr != nil {
					if errors.Is(probeErr, errServiceVersionMismatch) {
						return false, fmt.Errorf("the restarted %s does not run the expected version: %w", unitName, probeErr)
					}
					probeFailed = true
					if executable == "" {
						detail = fmt.Sprintf("the supervisor reports the state %q, but the instance does not answer on %s: %v", show.State, address, probeErr)
					} else {
						detail = fmt.Sprintf("the supervisor reports the state %q with the program %q, but the instance does not answer on %s: %v", show.State, program, address, probeErr)
					}
				} else {
					confirmedOptions, confirmedPID, confirmErr := m.runtimeRunOptions(ctx)
					switch {
					case confirmErr != nil:
						runtimeProcessErr = fmt.Errorf("confirm the service process after readiness: %w", confirmErr)
						probeFailed = true
						detail = runtimeProcessErr.Error()
					case confirmedPID != mainPID || confirmedOptions != runOptions:
						runtimeProcessErr = errors.New("the service process no longer matched the captured MainPID and runtime argv during readiness verification")
						probeFailed = true
						detail = runtimeProcessErr.Error()
					default:
						return verified, nil
					}
				}
			}
		}
		if !time.Now().Before(deadline) {
			if runtimeProcessErr != nil {
				if dynamicListen {
					return false, fmt.Errorf("the restarted %s runtime process/Listen was not observable and stable within %s: %w", unitName, budget, runtimeProcessErr)
				}
				return false, fmt.Errorf("the restarted %s service process could not be verified within %s: %w", unitName, budget, runtimeProcessErr)
			}
			switch {
			case probeFailed:
				return false, fmt.Errorf("the restarted %s does not answer on %s within %s: %s", unitName, address, budget, detail)
			case executable == "":
				return false, fmt.Errorf("the %s is not active within %s: %s", unitName, budget, detail)
			default:
				return false, fmt.Errorf("the restarted %s does not run %q within %s: %s", unitName, executable, budget, detail)
			}
		}
		select {
		case <-ctx.Done():
			return false, ctx.Err()
		case <-time.After(restartPollInterval):
		}
	}
}

// preflight rejects an installation the supervisor demonstrably cannot run:
// a missing supervisor, an unusable bind address, a definite access denial, or
// a format failure that the host's visible binary-format handlers cannot
// resolve. Unknown executable formats are left to systemd rather than rejected
// by this partial inspection.
func (m *systemdManager) preflight(ctx context.Context, opts InstallOptions, account *user.User) error {
	var (
		accountName string
		identity    *accessAccount
	)
	if m.scope == ScopeUser {
		if current, err := user.Current(); err == nil {
			accountName = current.Username
		}
		current := currentExecutableAccessIdentity()
		identity = &current
	} else if account != nil {
		accountName = account.Username
		if current, err := linuxExecutableAccessIdentity(account); err == nil {
			identity = &current
		}
	}
	return m.preflightWithContext(ctx, opts, accountName, identity, opts.Paths.StateDir)
}

func systemdWorkingDirectoryForPreflight(opts InstallOptions, existing bool, loaded systemdLoadedConfig, loadedKnown bool) string {
	if !existing {
		return opts.Paths.StateDir
	}
	// An update that changes WorkingDirectory can be shadowed by native
	// drop-ins. Until the resulting manager value is loaded, its effective
	// cwd is not established for this preflight.
	if opts.Explicit.StateDir || !loadedKnown || !loaded.loaded {
		return ""
	}
	if filepath.IsAbs(loaded.workingDirectory) {
		return loaded.workingDirectory
	}
	return ""
}

func (m *systemdManager) preflightWithContext(ctx context.Context, opts InstallOptions, accountName string, identity *accessAccount, workingDirectory string) error {
	if _, err := exec.LookPath("systemctl"); err != nil {
		return fmt.Errorf("service: systemctl is not available: %w", err)
	}
	if _, _, err := ParseListen(opts.Listen); err != nil {
		return err
	}
	info, err := os.Stat(opts.Executable)
	if err != nil {
		return fmt.Errorf("service: executable %q is not usable: %w", opts.Executable, err)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("service: executable %q is not a regular file", opts.Executable)
	}
	inspection, err := inspectExecutable(opts.Executable, workingDirectory, linuxBinfmtMiscDirectory)
	if err != nil {
		return err
	}
	if inspection.format == executableFormatUnknown {
		log.Printf("service: executable %q has an unclassified format; the preflight treats the format as neither verified nor refused", opts.Executable)
	}

	accessMode := uint32(unix.X_OK)
	if inspection.format == executableFormatScript {
		// Linux's script handler starts the interpreter, which must be able
		// to read the script itself. Native executables do not need this bit.
		accessMode |= unix.R_OK
	}
	access := executableAccessUnknown
	if identity != nil {
		access, err = checkExecutableAccess(ctx, opts.Executable, *identity, accessMode, workingDirectory)
		if err != nil {
			return err
		}
	}
	if accountName == "" {
		accountName = "the native service identity"
	}
	// The rerun names the scope of this manager: the command line defaults
	// to the user scope on Linux, so the machine-wide refusal must name the
	// flag that reproduces it — the other scope would loop the retry.
	rerun := "hfdesk service install --user"
	if m.scope == ScopeSystem {
		rerun = "hfdesk service install --system"
	}
	switch access {
	case executableAccessDenied:
		reason := errExecutableNotUsable
		if inspection.format == executableFormatScript && accessMode&unix.R_OK != 0 {
			reason = errExecutableNotReadable
		}
		return fmt.Errorf("service: executable %q is not usable by the account %q: %w. Grant the account access to the program and its folders, then run %q again", opts.Executable, accountName, reason, rerun)
	case executableAccessUnknown:
		log.Printf("service: execute access for %q by %s is inconclusive; it is treated as neither a denial nor as verified", opts.Executable, accountName)
	}

	if inspection.interpreter != "" {
		interpreterWorkingDirectory := ""
		if !filepath.IsAbs(inspection.interpreter) {
			interpreterWorkingDirectory = workingDirectory
		}
		interpreterAccess := executableAccessUnknown
		if identity != nil && (filepath.IsAbs(inspection.interpreter) || interpreterWorkingDirectory != "") {
			interpreterAccess, err = checkExecutableAccess(ctx, inspection.interpreter, *identity, uint32(unix.X_OK), interpreterWorkingDirectory)
			if err != nil {
				return err
			}
		}
		switch interpreterAccess {
		case executableAccessDenied:
			return fmt.Errorf("service: executable %q names an interpreter the account %q cannot use: %q. Grant the account execute access to the interpreter, then run %q again", opts.Executable, accountName, inspection.interpreter, rerun)
		case executableAccessUnknown:
			log.Printf("service: interpreter access for %q by %s is inconclusive; it is treated as neither a denial nor as verified", inspection.interpreter, accountName)
		}
	}
	return nil
}

type executableAccess int

const (
	executableAccessUnknown executableAccess = iota
	executableAccessAllowed
	executableAccessDenied
)

var (
	errExecutableNotUsable   = errors.New("the account cannot execute it")
	errExecutableNotReadable = errors.New("the account cannot read the script")
)

func executableAccessFromError(err error) executableAccess {
	switch {
	case err == nil:
		return executableAccessAllowed
	case errors.Is(err, errAccessWorkingDirectoryNotReady):
		return executableAccessUnknown
	case errors.Is(err, unix.EACCES), errors.Is(err, unix.ENOENT), errors.Is(err, unix.ENOTDIR), errors.Is(err, unix.ELOOP):
		return executableAccessDenied
	default:
		return executableAccessUnknown
	}
}

type executableFormat int

const (
	executableFormatUnknown executableFormat = iota
	executableFormatELF
	executableFormatScript
)

type executableInspection struct {
	format      executableFormat
	interpreter string
}

type binfmtMiscAvailability int

const (
	binfmtMiscUnknown binfmtMiscAvailability = iota
	binfmtMiscEmpty
	binfmtMiscActive
)

const (
	linuxBinfmtMiscDirectory = "/proc/sys/fs/binfmt_misc"
	maxExecutablePrefix      = 4096
	maxShebangLine           = 127
)

// inspectExecutable rejects only formats the available host evidence shows the
// ordinary Linux loaders cannot handle. It deliberately does not compare ELF
// machine identities or reject other files merely because this inspection does
// not recognize them.
func inspectExecutable(path, workingDirectory, binfmtDirectory string) (executableInspection, error) {
	handlers := binfmtMiscHandlers(binfmtDirectory)
	file, err := os.Open(path)
	if err != nil {
		// In particular, an execute-only ELF is a valid input even though the
		// installer cannot read its header.
		return executableInspection{format: executableFormatUnknown}, nil
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		return executableInspection{format: executableFormatUnknown}, nil
	}
	readSize := info.Size()
	if readSize > maxExecutablePrefix {
		readSize = maxExecutablePrefix
	}
	prefix := make([]byte, int(readSize))
	n, readErr := io.ReadFull(file, prefix)
	if readErr != nil && !errors.Is(readErr, io.EOF) && !errors.Is(readErr, io.ErrUnexpectedEOF) {
		return executableInspection{format: executableFormatUnknown}, nil
	}
	prefix = prefix[:n]

	if bytes.HasPrefix(prefix, []byte{0x7f, 'E', 'L', 'F'}) {
		if handlers != binfmtMiscEmpty {
			return executableInspection{format: executableFormatUnknown}, nil
		}
		if err := checkTruncatedELFHeader(prefix, info.Size()); err != nil {
			return executableInspection{}, fmt.Errorf("service: executable %q: %w", path, err)
		}
		interpreter, known, err := elfProgramInterpreter(file, prefix, info.Size())
		if err != nil {
			return executableInspection{}, fmt.Errorf("service: executable %q: %w", path, err)
		}
		if !known {
			return executableInspection{format: executableFormatUnknown}, nil
		}
		if interpreter != "" && !filepath.IsAbs(interpreter) && workingDirectory == "" {
			return executableInspection{format: executableFormatUnknown}, nil
		}
		return executableInspection{format: executableFormatELF, interpreter: interpreter}, nil
	}

	if bytes.HasPrefix(prefix, []byte("#!")) {
		if handlers != binfmtMiscEmpty {
			return executableInspection{format: executableFormatUnknown}, nil
		}
		interpreter, ok := linuxShebangInterpreter(prefix, info.Size())
		if !ok {
			return executableInspection{format: executableFormatUnknown}, nil
		}
		if interpreter == "" {
			return executableInspection{}, fmt.Errorf("service: executable %q has a shebang with no interpreter", path)
		}
		if !filepath.IsAbs(interpreter) && workingDirectory == "" {
			return executableInspection{format: executableFormatUnknown}, nil
		}
		return executableInspection{format: executableFormatScript, interpreter: interpreter}, nil
	}

	if handlers == binfmtMiscEmpty && plainTextFile(prefix, info.Size()) {
		return executableInspection{}, fmt.Errorf("service: executable %q is plain text without a shebang and no active binfmt_misc handler is visible", path)
	}
	return executableInspection{format: executableFormatUnknown}, nil
}

func checkTruncatedELFHeader(prefix []byte, fileSize int64) error {
	if len(prefix) < 16 || fileSize < 16 {
		return errors.New("truncated ELF identification")
	}
	var headerSize int64
	switch prefix[4] {
	case 1: // ELFCLASS32
		headerSize = 52
	case 2: // ELFCLASS64
		headerSize = 64
	default:
		return errors.New("invalid ELF class")
	}
	if prefix[5] != 1 && prefix[5] != 2 {
		return errors.New("invalid ELF byte order")
	}
	if prefix[6] != 1 {
		return errors.New("invalid ELF identification version")
	}
	if fileSize < headerSize || int64(len(prefix)) < headerSize {
		return errors.New("truncated ELF header")
	}
	return nil
}

// elfProgramInterpreter reads only the ELF program-header table that the Linux
// executable loader consumes. It does not classify the machine or emulate ELF
// load behavior; a table it cannot interpret stays unclassified.
func elfProgramInterpreter(file *os.File, header []byte, fileSize int64) (string, bool, error) {
	var order binary.ByteOrder
	var phoff uint64
	var phentsize, phnum uint16
	var programHeaderSize uint64
	switch header[4] {
	case 1: // ELFCLASS32
		order = binary.LittleEndian
		if header[5] == 2 {
			order = binary.BigEndian
		}
		phoff = uint64(order.Uint32(header[28:32]))
		phentsize = order.Uint16(header[42:44])
		phnum = order.Uint16(header[44:46])
		programHeaderSize = 32
	case 2: // ELFCLASS64
		order = binary.LittleEndian
		if header[5] == 2 {
			order = binary.BigEndian
		}
		phoff = order.Uint64(header[32:40])
		phentsize = order.Uint16(header[54:56])
		phnum = order.Uint16(header[56:58])
		programHeaderSize = 56
	default:
		return "", false, nil
	}
	if phnum == 0xffff || phnum == 0 || uint64(phentsize) != programHeaderSize {
		return "", false, nil
	}
	tableSize := uint64(phnum) * programHeaderSize
	if fileSize < 0 || phoff > uint64(fileSize) || tableSize > uint64(fileSize)-phoff {
		return "", true, errors.New("truncated ELF program header table")
	}
	table := make([]byte, int(tableSize))
	n, err := file.ReadAt(table, int64(phoff))
	if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
		return "", false, nil
	}
	if n != len(table) {
		return "", false, nil
	}

	var interpreter string
	for i := 0; i < int(phnum); i++ {
		entry := table[i*int(programHeaderSize) : (i+1)*int(programHeaderSize)]
		if order.Uint32(entry[:4]) != 3 { // PT_INTERP
			continue
		}
		if interpreter != "" {
			return "", true, errors.New("ELF has multiple interpreter segments")
		}
		var offset, size uint64
		if header[4] == 1 {
			offset = uint64(order.Uint32(entry[4:8]))
			size = uint64(order.Uint32(entry[16:20]))
		} else {
			offset = order.Uint64(entry[8:16])
			size = order.Uint64(entry[32:40])
		}
		if size == 0 || offset > uint64(fileSize) || size > uint64(fileSize)-offset {
			return "", true, errors.New("truncated ELF interpreter segment")
		}
		if size > maxExecutablePrefix {
			return "", false, nil
		}
		segment := make([]byte, int(size))
		n, err := file.ReadAt(segment, int64(offset))
		if err != nil && !errors.Is(err, io.EOF) && !errors.Is(err, io.ErrUnexpectedEOF) {
			return "", false, nil
		}
		if n != len(segment) {
			return "", false, nil
		}
		terminator := bytes.IndexByte(segment, 0)
		if terminator <= 0 {
			return "", false, nil
		}
		interpreter = string(segment[:terminator])
	}
	return interpreter, true, nil
}

func linuxShebangInterpreter(prefix []byte, fileSize int64) (string, bool) {
	line := prefix[2:]
	if newline := bytes.IndexByte(line, '\n'); newline >= 0 {
		if newline > maxShebangLine {
			return "", false
		}
		line = line[:newline]
	} else if fileSize-int64(2) > maxShebangLine {
		// The kernel truncates long shebang lines. Without knowing which
		// kernel rule applies, do not guess at the interpreter path.
		return "", false
	}
	line = bytes.TrimLeft(line, " \t")
	if nul := bytes.IndexByte(line, 0); nul >= 0 {
		return "", false
	}
	if end := bytes.IndexAny(line, " \t"); end >= 0 {
		line = line[:end]
	}
	return string(line), true
}

func plainTextFile(prefix []byte, fileSize int64) bool {
	if fileSize < 0 || fileSize > maxExecutablePrefix || int64(len(prefix)) != fileSize || !utf8.Valid(prefix) {
		return false
	}
	for _, r := range string(prefix) {
		if r != '\n' && r != '\r' && r != '\t' && !unicode.IsPrint(r) {
			return false
		}
	}
	return true
}

func binfmtMiscHandlers(directory string) binfmtMiscAvailability {
	status, err := os.ReadFile(filepath.Join(directory, "status"))
	if err != nil {
		return binfmtMiscUnknown
	}
	switch strings.TrimSpace(string(status)) {
	case "disabled":
		return binfmtMiscEmpty
	case "enabled":
	default:
		return binfmtMiscUnknown
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return binfmtMiscUnknown
	}
	for _, entry := range entries {
		if entry.Name() == "register" || entry.Name() == "status" {
			continue
		}
		contents, err := os.ReadFile(filepath.Join(directory, entry.Name()))
		if err != nil {
			return binfmtMiscUnknown
		}
		firstLine, _, _ := bytes.Cut(contents, []byte{'\n'})
		if string(firstLine) == "enabled" {
			return binfmtMiscActive
		}
		if string(firstLine) != "disabled" {
			return binfmtMiscUnknown
		}
	}
	return binfmtMiscEmpty
}

func linuxExecutableAccessIdentity(account *user.User) (accessAccount, error) {
	uid, err := strconv.Atoi(account.Uid)
	if err != nil {
		return accessAccount{}, err
	}
	gid, err := strconv.Atoi(account.Gid)
	if err != nil {
		return accessAccount{}, err
	}
	groupIDs, err := account.GroupIds()
	if err != nil {
		return accessAccount{}, err
	}
	identity := accessAccount{UID: uid, GID: gid, Groups: []int{gid}}
	seen := map[int]struct{}{gid: {}}
	for _, groupID := range groupIDs {
		group, err := strconv.Atoi(groupID)
		if err != nil {
			return accessAccount{}, err
		}
		if _, ok := seen[group]; ok {
			continue
		}
		seen[group] = struct{}{}
		identity.Groups = append(identity.Groups, group)
	}
	return identity, nil
}

func currentExecutableAccessIdentity() accessAccount {
	groups, _ := unix.Getgroups()
	identity := accessAccount{UID: unix.Geteuid(), GID: unix.Getegid(), Groups: []int{unix.Getegid()}}
	seen := map[int]struct{}{identity.GID: {}}
	for _, group := range groups {
		if _, ok := seen[group]; ok {
			continue
		}
		seen[group] = struct{}{}
		identity.Groups = append(identity.Groups, group)
	}
	return identity
}

func sameAccessIdentity(left, right accessAccount) bool {
	if left.UID != right.UID || left.GID != right.GID || len(left.Groups) != len(right.Groups) {
		return false
	}
	groups := make(map[int]struct{}, len(left.Groups))
	for _, group := range left.Groups {
		groups[group] = struct{}{}
	}
	for _, group := range right.Groups {
		if _, ok := groups[group]; !ok {
			return false
		}
	}
	return true
}

func accessCredential(identity accessAccount) (*syscall.Credential, error) {
	uid, uidErr := linuxIDToUint32(int64(identity.UID))
	gid, gidErr := linuxIDToUint32(int64(identity.GID))
	if uidErr != nil || gidErr != nil {
		return nil, errors.New("the service identity is outside the Linux UID/GID range")
	}
	groups := make([]uint32, 0, len(identity.Groups))
	seen := make(map[uint32]struct{}, len(identity.Groups))
	for _, group := range append([]int{identity.GID}, identity.Groups...) {
		id, err := linuxIDToUint32(int64(group))
		if err != nil {
			return nil, errors.New("the service identity has a group outside the Linux GID range")
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		groups = append(groups, id)
	}
	return &syscall.Credential{Uid: uid, Gid: gid, Groups: groups}, nil
}

func linuxIDToUint32(id int64) (uint32, error) {
	if id < 0 || id > math.MaxUint32 {
		return 0, errors.New("the Linux UID/GID is outside the uint32 range")
	}
	return uint32(id), nil
}

func (m *systemdManager) executableAccess(ctx context.Context, path string, mode uint32, account *user.User, workingDirectory string) (executableAccess, error) {
	if m.scope == ScopeUser {
		return checkExecutableAccess(ctx, path, currentExecutableAccessIdentity(), mode, workingDirectory)
	}
	if account == nil {
		return executableAccessUnknown, nil
	}
	target, err := linuxExecutableAccessIdentity(account)
	if err != nil {
		return executableAccessUnknown, nil
	}
	return checkExecutableAccess(ctx, path, target, mode, workingDirectory)
}

func checkExecutableAccess(ctx context.Context, path string, identity accessAccount, mode uint32, workingDirectory string) (executableAccess, error) {
	if sameAccessIdentity(identity, currentExecutableAccessIdentity()) {
		if identity.UID != 0 && accessProbeHasCapabilities() {
			return executableAccessUnknown, nil
		}
		if unix.Getuid() != unix.Geteuid() || unix.Getgid() != unix.Getegid() {
			return executableAccessUnknown, nil
		}
		return executableAccessFromError(faccessatExecutablePath(path, mode, workingDirectory)), nil
	}
	credential, err := accessCredential(identity)
	if err != nil {
		return executableAccessUnknown, nil
	}
	running, err := os.Executable()
	if err != nil {
		return executableAccessUnknown, nil
	}
	if !safeAccessProbeExecutable(running) {
		return executableAccessUnknown, nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	cmd := exec.CommandContext(probeCtx, running, internalAccessProbeCommand, path, strconv.FormatUint(uint64(mode), 10), workingDirectory)
	cmd.Env = []string{}
	cmd.SysProcAttr = &syscall.SysProcAttr{Credential: credential}
	err = cmd.Run()
	if ctx.Err() != nil {
		return executableAccessUnknown, ctx.Err()
	}
	if err == nil {
		return executableAccessAllowed, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		switch exitErr.ExitCode() {
		case accessProbeDeniedCode:
			return executableAccessDenied, nil
		case accessProbeUnknownCode:
			return executableAccessUnknown, nil
		}
	}
	return executableAccessUnknown, nil
}

func safeAccessProbeExecutable(path string) bool {
	info, err := os.Stat(path)
	if err != nil || info.Mode()&(os.ModeSetuid|os.ModeSetgid) != 0 {
		return false
	}
	_, err = unix.Getxattr(path, "security.capability", nil)
	return errors.Is(err, unix.ENODATA) || errors.Is(err, unix.ENOTSUP) || errors.Is(err, unix.EOPNOTSUPP)
}

// checkSystemPaths rejects a machine-wide installation that would adopt the
// state of the user who runs the install command.
//
// A system scope unit runs as root. A path inside the home directory of the
// invoking user would either expose the state of the user to the machine or
// take the state of the user away from it, so the installation is refused
// instead of silently taking the state over. The model and the cache trees
// are never touched by an installation: only the three directories of the
// instance (the config directory, the state directory and the log directory)
// are created, and nothing is ever chowned.
//
// A user scope instance is supposed to live inside the directories of its own
// user, so the guard does not touch it.
//
// The comparison is lexical: the paths are compared as they are written and
// the symbolic links of the file system are not resolved. A path that only
// reaches the home directory of the user through a symbolic link is
// therefore compared by its own path and not caught. The lexical test is the
// conservative choice: it never resolves a link of an unprivileged user into
// a machine-wide instance, and it reports the paths of the command line as
// they were typed.
func (m *systemdManager) checkSystemPaths(opts InstallOptions) error {
	if m.scope != ScopeSystem {
		return nil
	}
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		// The home directory of the invoking user is unknown: there is
		// nothing to compare the paths of the installation with, and an
		// unknown home is not a failed installation.
		return nil
	}
	for _, p := range []struct {
		name string
		path string
	}{
		{"config file", opts.Paths.ConfigFile},
		{"state dir", opts.Paths.StateDir},
		{"log dir", opts.Paths.LogDir},
		{"cache dir", opts.CacheDir},
		{"hub dir", opts.HubDir},
		{"local dir", opts.LocalDir},
	} {
		if p.path == "" {
			continue
		}
		// The containment helper of the downloader package is a lexical
		// test: it counts the home directory itself as inside it and it
		// keeps a sibling that shares the prefix of the home ("/home/user2"
		// against "/home/user") outside it.
		if hfdownloader.PathInside(home, p.path) {
			return fmt.Errorf("service: the %s %q of a machine-wide instance must not live inside the home directory %q of the invoking user", p.name, p.path, home)
		}
	}
	return nil
}

// Status reports the state of the registered service.
//
// The registration is described by the unit file: the executable, the bind
// address and the runtime paths come from the registered unit, not from the
// current intent of the caller. A registered executable that is gone is
// reported as broken, so a moved or deleted binary is never reported as a
// healthy registration. The state stays the diagnosis of the registration: the
// instance the supervisor started can still run, and Status.Running carries that
// fact, so a caller that has to know whether anything serves does not read the
// broken state as "nothing runs".
//
// The name of the status is the label of the service ("hfdesk"), not the
// name of the unit file ("hfdesk.service"): the label is the identity the
// user knows the service by.
func (m *systemdManager) Status(ctx context.Context) (Status, error) {
	path, err := m.unitPath()
	if err != nil {
		return Status{}, err
	}
	status := Status{
		Scope:    m.scope,
		Name:     ServiceName,
		UnitPath: path,
	}

	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			status.State = StateNotInstalled
			status.Detail = fmt.Sprintf("the unit file does not exist: %s", path)
			return status, nil
		}
		return Status{}, err
	}

	unit := parseUnit(string(content))
	// The status names the program the registration points at: behind a
	// recognized wrapper the program is judged, not the wrapper token.
	status.Executable = unit.Program
	status.Listen = unit.listen()
	status.Paths = unit.paths()
	// The cache root of the instance is the cache dir of the config file
	// of the instance when that file defines one, and the registered cache
	// dir otherwise: the status describes the instance that runs, not the
	// registration alone. An instance that defines no cache root at all
	// reports none.
	status.CacheDir = EffectiveCacheDir(unit.flagValue("--config"), unit.flagValue("--cache-dir"))

	// The state of the supervisor is read before the registration is judged:
	// the running fact answers a question of its own, and a registration
	// whose executable is gone can still have a live process. A supervisor
	// that cannot be reached is not a failure for a broken registration: the
	// registration is described by the unit file alone then.
	out, showErr := m.runSystemctl(ctx, "show", unitName,
		"--property=LoadState", "--property=ActiveState", "--property=SubState")
	show := systemdShow{State: StateUnknown}
	if showErr == nil {
		show = parseSystemdShow(out)
		status.Running = runningState(show.State)
	}

	if broken, detail := brokenExecutable(status.Executable); broken {
		status.State = StateBroken
		if status.Running {
			detail += brokenRunningDetail
		}
		status.Detail = detail
		return status, nil
	}

	if showErr != nil {
		status.State = StateUnknown
		status.Detail = showErr.Error()
		return status, showErr
	}
	status.State = show.State
	switch status.State {
	case StateNotInstalled:
		status.Detail = "the supervisor does not know the unit, reload the supervisor to pick the unit file up"
	case StateFailed:
		status.Detail = fmt.Sprintf("the unit failed, see `%s status %s`", m.supervisorHint(), unitName)
	}
	return status, nil
}

// brokenExecutable reports whether a registration that points at the given
// executable is broken, and the detail that names the missing program.
//
// A registration without an executable is not broken: there is no program
// that could be missing. A registered program that cannot be reached is
// broken: the instance can never run again until the program is installed
// again.
//
// The decision is pure and separate from the supervisor: a broken
// registration is described by the unit file alone, so the status of a
// broken registration never depends on a running supervisor.
func brokenExecutable(executable string) (bool, string) {
	if executable == "" {
		return false, ""
	}
	if _, err := os.Stat(executable); err != nil {
		return true, fmt.Sprintf("registered executable not found: %s", executable)
	}
	return false, ""
}

func systemdLoadedExecutablePath(executable string) string {
	words := splitSystemdWords(systemdQuoteCommandAlways(executable))
	if len(words) == 0 {
		return executable
	}
	return words[0]
}
