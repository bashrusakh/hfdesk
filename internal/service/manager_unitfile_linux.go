// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package service

import (
	"bytes"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"os/user"
	"path/filepath"
	"strconv"
	"strings"
	"unicode/utf8"

	"github.com/bashrusakh/hfdesk/internal/runtimepaths"
)

// execStartPath returns the program path of the ExecStart property, which the
// supervisor reports as a structure:
//
//	ExecStart={ path=/usr/local/bin/hfdesk ; argv[]=/usr/local/bin/hfdesk ... }
//
// The "path=" field is the program that really executes; an absent or empty
// property yields an empty string.
func execStartPath(output string) string {
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.TrimSpace(key) != "ExecStart" {
			continue
		}
		_, rest, ok := strings.Cut(strings.TrimSpace(value), "path=")
		if !ok {
			return ""
		}
		for _, end := range []string{" ; argv[]=", "; argv[]=", " ;", " }"} {
			if i := strings.Index(rest, end); i >= 0 {
				return trimExecStartPath(rest[:i])
			}
		}
		return trimExecStartPath(strings.TrimSuffix(rest, "}"))
	}
	return ""
}

// trimExecStartPath returns the program path of a property value without the
// quoting a path with spaces carries.
func trimExecStartPath(value string) string {
	value = strings.TrimSpace(value)
	if len(value) >= 2 && value[0] == '"' && value[len(value)-1] == '"' {
		return value[1 : len(value)-1]
	}
	return value
}

// execStartProgram returns the program of the registered command the
// supervisor reports and whether the command's shape made a program slot
// readable.
//
// The rule is the slot model of the update: the words come from the argv[] of
// the property, and the program is the token behind a recognized "env"
// prefix, the first word of a plain command. A shape the slot model does not
// recognize has no readable program: the executed binary (path=) is returned
// as the only program fact the supervisor gives, flagged unreadable, so a
// wrapper never passes as the program it launches while a failure can still
// name the wrapper as the executable it is. Without an argument vector the
// executed binary alone is the observable command.
func execStartProgram(output string) (program string, readable bool) {
	path := execStartPath(output)
	tokens := execStartArgvWords(output)
	if len(tokens) == 0 {
		tokens = []systemdWord{{value: path}}
	}
	slot, ok := systemdExecStartSlot(tokens)
	if !ok {
		return path, false
	}
	return tokens[slot].value, true
}

// execStartArgvWords returns the words of the argv[] field of the ExecStart
// property, or nil when the property carries no vector. The display joins the
// vector with spaces: the words are that joined form split back on
// whitespace, which is exact for the words a registration writes; a boundary
// the display itself cannot prove leaves the command unreadable instead of
// guessed.
func execStartArgvWords(output string) []systemdWord {
	for _, line := range strings.Split(output, "\n") {
		key, value, ok := strings.Cut(strings.TrimSpace(line), "=")
		if !ok || strings.TrimSpace(key) != "ExecStart" {
			continue
		}
		_, rest, ok := strings.Cut(strings.TrimSpace(value), "argv[]=")
		if !ok {
			return nil
		}
		for _, end := range []string{" ;", " }"} {
			if i := strings.Index(rest, end); i >= 0 {
				rest = rest[:i]
				break
			}
		}
		rest = strings.TrimSuffix(rest, "}")
		var words []systemdWord
		for _, word := range strings.Fields(rest) {
			words = append(words, systemdWord{value: word})
		}
		return words
	}
	return nil
}

func systemdUnicode32EscapeRune(value uint64) (rune, bool) {
	if value == 0 || value > utf8.MaxRune {
		return 0, false
	}
	r := rune(value)
	if !utf8.ValidRune(r) || (r >= 0xfdd0 && r <= 0xfdef) || r&0xfffe == 0xfffe {
		return 0, false
	}
	return r, true
}

// readUnitFile reads the current content of a unit file. The second result
// reports whether the file existed at all.
func readUnitFile(path string) ([]byte, bool, error) {
	content, err := os.ReadFile(path)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			return nil, false, nil
		}
		return nil, false, err
	}
	return content, true, nil
}

// writeUnitFile writes a unit file atomically: the content goes into a
// temporary file of the same directory and is renamed over the target. A
// reader of the unit file therefore never sees a half written unit.
func writeUnitFile(path, content string) error {
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, unitDirMode); err != nil {
		return fmt.Errorf("service: create the unit directory %q: %w", dir, err)
	}
	tmp, err := os.CreateTemp(dir, ".hfdesk-unit-*")
	if err != nil {
		return fmt.Errorf("service: create a temporary unit file in %q: %w", dir, err)
	}
	tmpName := tmp.Name()
	defer func() {
		// The temporary file only survives when it was not renamed.
		_ = os.Remove(tmpName)
	}()
	if _, err := tmp.WriteString(content); err != nil {
		_ = tmp.Close()
		return fmt.Errorf("service: write the unit file %q: %w", tmpName, err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("service: close the unit file %q: %w", tmpName, err)
	}
	if err := os.Chmod(tmpName, unitFileMode); err != nil {
		return fmt.Errorf("service: set the mode of the unit file %q: %w", tmpName, err)
	}
	if err := os.Rename(tmpName, path); err != nil {
		return fmt.Errorf("service: install the unit file %q: %w", path, err)
	}
	return nil
}

// unitTemplate is the shape of a HFDesk unit.
//
// The unit keeps the supervisor in charge: HFDesk stays a normal foreground
// process, the unit is a simple service and the process is never forked,
// detached or put into the background by HFDesk itself.
//
// The hardening set is deliberately small. HFDesk reads and writes the
// models, the cache and the state of the instance in directories that depend
// on the installation, so a sandbox that hides the file system or the home
// directory of the user would break the instance instead of protecting it.
// Every further restriction would need the explicit list of the paths the
// instance may write, and that list depends on the installation.
//
// The unit does not redirect the output: HFDesk writes its own bounded
// rotating log file into the log directory of the instance and the supervisor
// keeps the standard output and the standard error of the process in its
// journal.
//
// The identity of a machine-wide unit is the dedicated service account of
// the platform. A user scope unit names no account: it runs as the user of
// the session.
const unitTemplate = `# HFDesk service unit.
%s#
# Generated by the hfdesk service install command; a later installation
# updates its command while preserving other unit settings.
#
# The unit holds no access data: an instance reads its own settings from its
# config file, which only the owner of the instance can read. The unit
# therefore carries the executable and the canonical arguments of the
# process only.

[Unit]
Description=HFDesk
Documentation=https://github.com/bashrusakh/hfdesk
After=network-online.target
Wants=network-online.target

[Service]
# The supervisor owns the lifecycle of the instance: HFDesk stays a normal
# foreground process and the supervisor restarts it when it fails.
Type=simple
%sExecStart=%s
WorkingDirectory=%s
Restart=on-failure
RestartSec=5s
# The supervisor waits for the graceful shutdown of the instance before it
# kills it. The wait covers the whole orderly shutdown of the server, so a
# stop never interrupts an instance that is still saving its state.
TimeoutStopSec=%s
NoNewPrivileges=true
UMask=0077
Environment=%s

[Install]
WantedBy=%s
`

// renderUnit renders the systemd unit of an installation.
//
// The function renders the text of the unit and never changes anything on
// the machine. The command of the unit is the canonical argument vector of
// the shared contract, so every platform registers the same command.
//
// The account is the service account the installation resolved for a
// machine-wide instance. It is nil for a user scope instance, which runs as
// the user of the session.
func renderUnit(opts InstallOptions, account *user.User) (string, error) {
	home, err := unitHomeDir(opts)
	if err != nil {
		return "", err
	}
	workingDirectory, err := systemdWorkingDirectoryValue(opts.Paths.StateDir)
	if err != nil {
		return "", err
	}
	identity, err := unitIdentity(opts, account)
	if err != nil {
		return "", err
	}

	target := "default.target"
	if opts.Scope == ScopeSystem {
		target = "multi-user.target"
	}

	return fmt.Sprintf(unitTemplate,
		"",
		identity,
		systemdExecStart(opts),
		workingDirectory,
		systemdStopTimeout(),
		systemdQuote("HOME="+home),
		target,
	), nil
}

func systemdExecStart(opts InstallOptions) string {
	words := make([]string, 0, len(RunArgs(opts))+1)
	words = append(words, systemdQuoteCommandAlways(opts.Executable))
	for _, arg := range RunArgs(opts) {
		words = append(words, systemdQuoteCommand(arg))
	}
	return strings.Join(words, " ")
}

const (
	// canonicalUnitFormat is the version of the canonical rendering. The
	// marker line of the header pins it, so a file another version wrote
	// is classified instead of being reinterpreted.
	canonicalUnitFormat = 1
	// canonicalUnitFormatPrefix introduces the format marker comment.
	canonicalUnitFormatPrefix = "# format:"
)

// canonicalUnit is the fact model of the canonical unit: every value the
// canonical rendering writes, already resolved, so rendering needs no
// environment lookup and is byte deterministic for the same facts.
type canonicalUnit struct {
	scope      Scope
	executable string
	paths      runtimepaths.Paths
	listen     string
	cacheDir   string
	hubDir     string
	localDir   string
	user       string
	group      string
	home       string
}

// installOptions returns the options whose canonical command the unit holds.
// The RunArgs vector reads only these fields, so the shared quoting and flag
// order of a first installation renders the command of the facts unchanged.
func (u canonicalUnit) installOptions() InstallOptions {
	return InstallOptions{
		Scope:      u.scope,
		Executable: u.executable,
		Listen:     u.listen,
		Paths:      u.paths,
		CacheDir:   u.cacheDir,
		HubDir:     u.hubDir,
		LocalDir:   u.localDir,
	}
}

// renderCanonicalUnit renders the canonical form of a unit byte
// deterministically: the same facts always produce the same bytes, and the
// header carries the format marker of the current renderer.
//
// The rendering never writes StateDirectory= or another *Directory=
// directive: the instance directories are created and granted explicitly,
// and a systemd directory directive could recursively hand their ownership
// to the supervisor.
func renderCanonicalUnit(u canonicalUnit) (string, error) {
	identity, err := canonicalUnitIdentity(u)
	if err != nil {
		return "", err
	}
	workingDirectory, err := systemdWorkingDirectoryValue(u.paths.StateDir)
	if err != nil {
		return "", err
	}
	target := "default.target"
	if u.scope == ScopeSystem {
		target = "multi-user.target"
	}
	return fmt.Sprintf(unitTemplate,
		fmt.Sprintf("%s %d\n", canonicalUnitFormatPrefix, canonicalUnitFormat),
		identity,
		systemdExecStart(u.installOptions()),
		workingDirectory,
		systemdStopTimeout(),
		systemdQuote("HOME="+u.home),
		target,
	), nil
}

// canonicalUnitIdentity renders the identity directives from the facts. The
// account lookup of a first installation already ran when the facts were
// built, so the rendering resolves nothing.
func canonicalUnitIdentity(u canonicalUnit) (string, error) {
	if u.scope != ScopeSystem {
		return "", nil
	}
	if u.user == "" {
		return "", errors.New("service: the machine-wide instance needs a service account")
	}
	identity := "User=" + systemdQuote(u.user) + "\n"
	if u.group != "" {
		identity += "Group=" + systemdQuote(u.group) + "\n"
	}
	return identity, nil
}

// readCanonicalUnit is the strict inverse of renderCanonicalUnit: it reads
// the registration facts of a canonical unit, and refuses every shape it
// cannot prove the renderer produced — line continuations, escapes outside
// the quoting subset, an unprovable specifier or environment reference, a
// command that is not the canonical vector, or facts that contradict the
// scope. A refused shape is classified as not canonical and never guessed;
// the byte comparison of detector A proves the remaining form.
func readCanonicalUnit(content []byte, scope Scope) (canonicalUnit, error) {
	if !utf8.Valid(content) || len(content) == 0 || content[len(content)-1] != '\n' {
		return canonicalUnit{}, errors.New("the unit is not the UTF-8 text with a final newline the renderer writes")
	}
	lines := strings.Split(strings.TrimSuffix(string(content), "\n"), "\n")
	formatSeen := false
	for _, line := range lines {
		if hasSystemdLineContinuation(line) {
			return canonicalUnit{}, errors.New("a line ends with a continuation the renderer never writes")
		}
		if rest, ok := strings.CutPrefix(line, canonicalUnitFormatPrefix); ok {
			if strings.TrimSpace(rest) != strconv.Itoa(canonicalUnitFormat) {
				return canonicalUnit{}, fmt.Errorf("the unit carries format %q, not the supported format %d", strings.TrimSpace(rest), canonicalUnitFormat)
			}
			formatSeen = true
		}
	}
	if !formatSeen {
		return canonicalUnit{}, fmt.Errorf("the %s marker is missing", canonicalUnitFormatPrefix)
	}

	u := canonicalUnit{scope: scope}
	var command []string
	var workingDirectory string
	workingDirectorySeen := false
	userSeen, groupSeen := false, false
	environmentSeen := false
	section := ""
	for _, line := range lines {
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section = strings.ToLower(strings.TrimSpace(trimmed[1 : len(trimmed)-1]))
			continue
		}
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok || section != "service" {
			continue
		}
		switch key {
		case "ExecStart":
			if command != nil {
				return canonicalUnit{}, errors.New("the unit carries more than one ExecStart=")
			}
			words, err := decodeCanonicalCommand(value)
			if err != nil {
				return canonicalUnit{}, fmt.Errorf("ExecStart=: %w", err)
			}
			command = words
		case "WorkingDirectory":
			if workingDirectorySeen {
				return canonicalUnit{}, errors.New("the unit carries more than one WorkingDirectory=")
			}
			workingDirectorySeen = true
			decoded, err := decodeCanonicalWorkingDirectory(value)
			if err != nil {
				return canonicalUnit{}, fmt.Errorf("WorkingDirectory=: %w", err)
			}
			workingDirectory = decoded
		case "User":
			if userSeen {
				return canonicalUnit{}, errors.New("the unit carries more than one User=")
			}
			userSeen = true
			decoded, err := decodeCanonicalDirective(value)
			if err != nil {
				return canonicalUnit{}, fmt.Errorf("User=: %w", err)
			}
			u.user = decoded
		case "Group":
			if groupSeen {
				return canonicalUnit{}, errors.New("the unit carries more than one Group=")
			}
			groupSeen = true
			decoded, err := decodeCanonicalDirective(value)
			if err != nil {
				return canonicalUnit{}, fmt.Errorf("Group=: %w", err)
			}
			u.group = decoded
		case "Environment":
			if environmentSeen {
				return canonicalUnit{}, errors.New("the unit carries more than one Environment=")
			}
			environmentSeen = true
			decoded, err := decodeCanonicalDirective(value)
			if err != nil {
				return canonicalUnit{}, fmt.Errorf("Environment=: %w", err)
			}
			name, home, ok := strings.Cut(decoded, "=")
			if !ok || name != "HOME" {
				return canonicalUnit{}, errors.New("the Environment= of the unit is not the HOME the renderer writes")
			}
			u.home = home
		default:
			// A directive the renderer does not write is no fact of
			// the model: the byte comparison proves the form.
		}
	}
	if command == nil {
		return canonicalUnit{}, errors.New("the unit has no ExecStart= command")
	}
	if err := readCanonicalRunArgs(&u, command); err != nil {
		return canonicalUnit{}, err
	}
	if scope == ScopeSystem {
		if !userSeen || u.user == "" {
			return canonicalUnit{}, errors.New("a machine-wide unit names no User=")
		}
	} else if userSeen || groupSeen {
		return canonicalUnit{}, errors.New("a user scope unit names an account")
	}
	if !workingDirectorySeen {
		return canonicalUnit{}, errors.New("the unit has no WorkingDirectory=")
	}
	if workingDirectory != u.paths.StateDir {
		return canonicalUnit{}, fmt.Errorf("WorkingDirectory= holds %q, the command %q", workingDirectory, u.paths.StateDir)
	}
	if !environmentSeen {
		return canonicalUnit{}, errors.New("the unit has no Environment=")
	}
	home := systemServiceHome
	if scope != ScopeSystem {
		var err error
		if home, err = os.UserHomeDir(); err != nil || home == "" {
			return canonicalUnit{}, errors.New("the home directory of the user is unknown, the HOME of the unit cannot be proven")
		}
	}
	if u.home != home {
		return canonicalUnit{}, fmt.Errorf("Environment=HOME holds %q, the instance runs with %q", u.home, home)
	}
	return u, nil
}

// readCanonicalRunArgs reads the command of the canonical vector: the
// program, "service run", the four required flags in the order RunArgs
// writes them and the optional directories only where RunArgs writes them.
// A command that deviates in order, membership or arity is refused instead
// of read by a guess.
func readCanonicalRunArgs(u *canonicalUnit, words []string) error {
	if len(words) < 3 || words[1] != "service" || words[2] != "run" {
		return errors.New("the command is not a program with its \"service run\" vector")
	}
	u.executable = words[0]
	rest := words[3:]
	requiredNames := []string{"--config", "--state-dir", "--log-dir", "--listen"}
	requiredTargets := []*string{&u.paths.ConfigFile, &u.paths.StateDir, &u.paths.LogDir, &u.listen}
	for i, name := range requiredNames {
		if len(rest) < 2 || rest[0] != name {
			return fmt.Errorf("the command does not carry %s where the renderer writes it", name)
		}
		*requiredTargets[i] = rest[1]
		rest = rest[2:]
	}
	optionalNames := []string{"--cache-dir", "--hub-dir", "--local-dir"}
	optionalTargets := []*string{&u.cacheDir, &u.hubDir, &u.localDir}
	for i, name := range optionalNames {
		if len(rest) == 0 {
			break
		}
		if len(rest) < 2 || rest[0] != name {
			return errors.New("the command carries an argument the renderer does not write")
		}
		*optionalTargets[i] = rest[1]
		rest = rest[2:]
	}
	if len(rest) > 0 {
		return errors.New("the command carries trailing arguments the renderer does not write")
	}
	return nil
}

// decodeCanonicalDirective decodes a directive value the way systemdQuote
// writes it: the value is bare, or a double quoted run whose only escapes
// are the ones the writer emits (" for a quote, \\ for a backslash, %% for
// a literal percent sign). A dollar sign of a directive is literal and is
// kept as it is; any other escape, a single quote or a bare percent sign
// is a shape the renderer never writes and is refused.
func decodeCanonicalDirective(value string) (string, error) {
	value = strings.TrimRight(value, " \t")
	if value == "" {
		return "", nil
	}
	if value[0] != '"' {
		return decodeCanonicalDirectiveBare(value)
	}
	if len(value) < 2 || value[len(value)-1] != '"' {
		return "", errors.New("the quoted value is not closed")
	}
	inner := value[1 : len(value)-1]
	var b strings.Builder
	for i := 0; i < len(inner); {
		switch inner[i] {
		case '"':
			return "", errors.New("a quote inside the quoted value is not closed by the writer")
		case '\\':
			if i+1 >= len(inner) || (inner[i+1] != '"' && inner[i+1] != '\\') {
				return "", errors.New("the value carries an escape the writer never emits")
			}
			b.WriteByte(inner[i+1])
			i += 2
		case '%':
			if i+1 >= len(inner) || inner[i+1] != '%' {
				return "", errors.New("the value carries a specifier the writer never emits")
			}
			b.WriteByte('%')
			i += 2
		default:
			b.WriteByte(inner[i])
			i++
		}
	}
	return b.String(), nil
}

func decodeCanonicalDirectiveBare(value string) (string, error) {
	for i := 0; i < len(value); i++ {
		switch c := value[i]; {
		case c == '"' || c == '\'' || c == '\\':
			return "", errors.New("a bare value carries a character the writer always quotes")
		case c == '%':
			return "", errors.New("the value carries a specifier the writer never emits")
		case c < 0x20:
			return "", errors.New("the value carries a control character")
		}
	}
	return value, nil
}

// decodeCanonicalWorkingDirectory decodes a WorkingDirectory= value the way
// systemdWorkingDirectoryValue writes it: bytes stay intact — the writer
// escapes nothing there — only a literal percent sign is doubled, and
// trailing whitespace was stripped by the supervisor before the value became
// a fact. A bare percent sign the writer would have doubled or a control
// character is refused; the cross-check against --state-dir proves the value
// is the one the command registers.
func decodeCanonicalWorkingDirectory(value string) (string, error) {
	value = strings.TrimRight(value, " \t")
	var b strings.Builder
	for i := 0; i < len(value); {
		switch value[i] {
		case '%':
			if i+1 >= len(value) || value[i+1] != '%' {
				return "", errors.New("the value carries a specifier the writer never emits")
			}
			b.WriteByte('%')
			i += 2
		default:
			if value[i] < 0x20 {
				return "", errors.New("the value carries a control character")
			}
			b.WriteByte(value[i])
			i++
		}
	}
	return b.String(), nil
}

// decodeCanonicalCommand decodes the words of an ExecStart= command the way
// systemdQuoteCommand and systemdQuoteCommandAlways write them: words are
// separated by single spaces, a word the writer quoted is one double quoted
// run whose only escapes are \", \\, %% and $$, and the closing quote ends
// the word. Everything else — a single quote, an unpaired percent sign or
// dollar sign, a C-style escape, a control character, a quote inside a bare
// word — is a shape the renderer never writes and is refused, never guessed.
func decodeCanonicalCommand(value string) ([]string, error) {
	if !utf8.ValidString(value) {
		return nil, errors.New("the command is not UTF-8 text")
	}
	var words []string
	var word strings.Builder
	inWord := false
	flush := func() {
		if inWord {
			words = append(words, word.String())
			word.Reset()
			inWord = false
		}
	}
	for i := 0; i < len(value); {
		c := value[i]
		switch {
		case c == ' ':
			flush()
			i++
		case c == '"':
			if inWord {
				return nil, errors.New("a quote opens inside a bare word the writer always quotes whole")
			}
			i++
			closed := false
			for i < len(value) && !closed {
				switch value[i] {
				case '"':
					closed = true
					i++
				case '\\':
					if i+1 >= len(value) || (value[i+1] != '"' && value[i+1] != '\\') {
						return nil, errors.New("the command carries an escape the writer never emits")
					}
					word.WriteByte(value[i+1])
					i += 2
				case '%':
					if i+1 >= len(value) || value[i+1] != '%' {
						return nil, errors.New("the command carries a specifier the writer never emits")
					}
					word.WriteByte('%')
					i += 2
				case '$':
					if i+1 >= len(value) || value[i+1] != '$' {
						return nil, errors.New("the command carries an environment reference the writer never emits")
					}
					word.WriteByte('$')
					i += 2
				default:
					if value[i] < 0x20 {
						return nil, errors.New("the command carries a control character")
					}
					word.WriteByte(value[i])
					i++
				}
			}
			if !closed {
				return nil, errors.New("the quoted word of the command is not closed")
			}
			inWord = true
			if i < len(value) && value[i] != ' ' {
				return nil, errors.New("a quoted word is not closed at the end of the word")
			}
		case c == '\'' || c == '\\' || c == '%' || c == '$':
			return nil, errors.New("the command carries a character the writer always quotes")
		default:
			if c < 0x20 {
				return nil, errors.New("the command carries a control character")
			}
			inWord = true
			word.WriteByte(c)
			i++
		}
	}
	flush()
	if len(words) == 0 {
		return nil, errors.New("the command holds no word")
	}
	return words, nil
}

// errUnitNotRendered classifies a file whose facts parse but whose bytes are
// not the rendering of those facts: a line, spacing or directive outside the
// canonical form.
var errUnitNotRendered = errors.New("the file bytes are not the rendering of the settings they hold")

// unitDropInPath returns the drop-in the ownership contract gives the
// administrator beside the unit HFDesk writes; HFDesk never writes it.
func unitDropInPath(unitPath string) string {
	return filepath.Join(filepath.Dir(unitPath), unitName+".d", "local.conf")
}

// detectHandEditedUnit refuses a base file HFDesk did not render: the bytes
// must be the canonical rendering of their own facts, so a hand edit, a
// wrapper, a foreign version or a dynamic operand classifies as not ours
// before anything is changed. The facts come back only for that exact form.
func detectHandEditedUnit(unitPath string, content []byte, scope Scope) (canonicalUnit, error) {
	unit, err := readCanonicalUnit(content, scope)
	if err != nil {
		return canonicalUnit{}, handEditedUnitRefusal(unitPath, err, scope)
	}
	rendered, err := renderCanonicalUnit(unit)
	if err != nil {
		return canonicalUnit{}, handEditedUnitRefusal(unitPath, err, scope)
	}
	if rendered != string(content) {
		return canonicalUnit{}, handEditedUnitRefusal(unitPath, errUnitNotRendered, scope)
	}
	return unit, nil
}

// handEditedUnitRefusal states the result, the cause with the round-trip
// reason, and the one next action of the ownership contract.
//
// The action must make the next attempt succeed in every state of this
// refusal: the move preserves the customizations, the removal of the unit
// file makes the next installation the first one that writes the canonical
// rendering again (an edit the move alone cannot undo would otherwise refuse
// forever), and the reload clears the supervisor's in-memory copy of the
// removed file.
func handEditedUnitRefusal(unitPath string, reason error, scope Scope) error {
	return fmt.Errorf("service: the update was refused and the registration was not changed: the unit file %q is not the unit HFDesk generates: %v. Move your customizations into %q, remove the unit file %q, reload the unit files with %q, and run %q again",
		unitPath, reason, unitDropInPath(unitPath), unitPath, scopeSupervisorHint(scope)+" daemon-reload", "hfdesk service install")
}

// unitFileUnchanged is the TOCTOU guard of the atomic write: the file
// re-read immediately before writeUnitFile must still be the snapshot the
// update read — same existence, same bytes — or the write is refused.
func unitFileUnchanged(snapshot []byte, snapshotExists bool, current []byte, currentExists bool) bool {
	return snapshotExists == currentExists && bytes.Equal(snapshot, current)
}

// canonicalUnitForInstall builds the canonical fact model of the unit an
// installation writes: the merged options plus the HOME and the account
// identity the installation already resolved, so the rendering resolves
// nothing and stays byte deterministic.
func canonicalUnitForInstall(opts InstallOptions, account *user.User) (canonicalUnit, error) {
	home, err := unitHomeDir(opts)
	if err != nil {
		return canonicalUnit{}, err
	}
	u := canonicalUnit{
		scope:      opts.Scope,
		executable: opts.Executable,
		paths:      opts.Paths,
		listen:     opts.Listen,
		cacheDir:   opts.CacheDir,
		hubDir:     opts.HubDir,
		localDir:   opts.LocalDir,
		home:       home,
	}
	if opts.Scope != ScopeSystem {
		return u, nil
	}
	name := serviceAccountName(opts)
	if name == "" {
		return canonicalUnit{}, errors.New("service: the machine-wide instance needs a service account")
	}
	u.user = name
	if account != nil {
		// The group of the unit is the primary group of the account,
		// exactly as unitIdentity renders it; a group the machine does
		// not know is left out instead of guessed.
		if group, err := user.LookupGroupId(account.Gid); err == nil && group.Name != "" {
			u.group = group.Name
		}
	}
	return u, nil
}

// registrationOptions returns the installation options the canonical facts
// register, so an update over an existing registration merges with what the
// file really holds: every unmarked request field keeps the registered value.
func (u canonicalUnit) registrationOptions() InstallOptions {
	opts := u.installOptions()
	// A user scope unit names no account: it runs as the user of the
	// session, and an account of the request never applies to it.
	if u.scope == ScopeSystem {
		opts.ServiceUser = u.user
	}
	return opts
}

// unitChangedDuringUpdateRefusal states the result, the cause and the one
// next action of the guard that protects the atomic write: the file moved
// out from under the snapshot this update read, so the write is refused
// instead of overwriting a change the update never saw.
func unitChangedDuringUpdateRefusal(unitPath string) error {
	return fmt.Errorf("service: the update was refused and the registration was not changed: the unit file %q changed while the update was running. Run %q again",
		unitPath, "hfdesk service install")
}

// systemdExecStartSlot returns the index of the program token of a command
// and whether the command has a recognized shape: the program alone, or the
// program followed by the canonical "service run" vector, optionally behind
// an "env" prefix of the supported subset that stays intact. A prefix word
// the subset does not list leaves the shape unrecognized instead of being
// read by a guessed option arity.
//
// The registered process is the HFDesk program that receives the canonical
// "service run" argument vector as its first arguments. Every other shape
// cannot be read reliably: a shell script string hides the program inside a
// quoted word, an unknown wrapper may consume the words after it
// differently, and a command without the run vector is not the HFDesk
// service command. The argument parser uses the same rule as the update, so
// the run flags of a recognized command are read after the program and
// never from the words of the wrapper prefix.
func systemdExecStartSlot(tokens []systemdWord) (int, bool) {
	if len(tokens) <= 1 {
		// The program alone carries no argument the rewrite could
		// mismatch.
		return 0, true
	}
	slot := 0
	if !tokens[0].dynamic && filepath.Base(tokens[0].value) == "env" {
		var ok bool
		slot, ok = systemdEnvPrefixProgramSlot(tokens)
		if !ok {
			return 0, false
		}
	}
	if slot+2 >= len(tokens) || tokens[slot+1].value != "service" || tokens[slot+2].value != "run" {
		return 0, false
	}
	return slot, true
}

// systemdEnvPrefixProgramSlot walks the words between an "env" wrapper and
// its program and returns the index of the program, or reports that the
// prefix is not in the supported subset.
//
// The subset is fixed: a value-less option word (-i, --ignore-environment),
// a value-taking option word with its separated value (-u NAME, --unset
// NAME), an attached "--option=value" word such as --unset=NAME, which
// carries its own value, and NAME=VALUE assignments. The word after a
// value-taking option is that option's value, never the program; the first
// bare word the subset does not consume is the program. Any other option
// word would need its arity guessed, so such a shape is not recognized.
func systemdEnvPrefixProgramSlot(tokens []systemdWord) (int, bool) {
	for slot := 1; slot < len(tokens); {
		value := tokens[slot].value
		if strings.HasPrefix(value, "-") {
			switch {
			case value == "-i" || value == "--ignore-environment":
				slot++
			case value == "-u" || value == "--unset":
				// The separated value is the next word when
				// it is not itself an option word; without
				// one no program remains behind the option.
				if slot+1 >= len(tokens) || strings.HasPrefix(tokens[slot+1].value, "-") {
					return 0, false
				}
				slot += 2
			case systemdEnvAttachedOptionWord(value):
				slot++
			default:
				// Any other option word: its arity would be a
				// guess, so the command shape is not
				// recognized and the line is refused.
				return 0, false
			}
			continue
		}
		if systemdEnvAssignmentWord(value) {
			slot++
			continue
		}
		return slot, true
	}
	// The prefix ends without a program.
	return 0, false
}

// systemdEnvAttachedOptionWord reports whether word is a long option in the
// attached form "--option=value": the value sits inside the word itself, so
// the word consumes no following word.
func systemdEnvAttachedOptionWord(word string) bool {
	if !strings.HasPrefix(word, "--") {
		return false
	}
	name, _, ok := strings.Cut(word[2:], "=")
	return ok && name != "" && !strings.HasPrefix(name, "-")
}

// systemdEnvAssignmentWord reports whether word is a NAME=VALUE assignment
// of the prefix: a name of letters, digits (not first) and underscores,
// separated by "=" from its value.
func systemdEnvAssignmentWord(word string) bool {
	name, _, ok := strings.Cut(word, "=")
	if !ok || name == "" {
		return false
	}
	for i := 0; i < len(name); i++ {
		c := name[i]
		switch {
		case c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z':
		case c >= '0' && c <= '9' && i > 0:
		default:
			return false
		}
	}
	return true
}

// systemdRunOption is one recognized run flag occurrence of a command: the
// flag name and whether its source carries a specifier or environment
// reference the supervisor expands.
type systemdRunOption struct {
	name    string
	dynamic bool
}

// scanSystemdRunArgs follows the option/value consumption used by Go's
// flag.FlagSet: a separate option consumes its next word even when it is
// "--", and a standalone "--" ends the flags. The words behind the program
// and the "service run" vector are scanned, so the run flags are found
// exactly where the canonical vector writes them; a word the flag grammar
// does not recognize ends the scan instead of being guessed.
func scanSystemdRunArgs(words []systemdWord) []systemdRunOption {
	start := 0
	if len(words) >= start+2 && words[start].value == "service" && words[start+1].value == "run" {
		start += 2
	} else if len(words) > start && words[start].value == "run" {
		start++
	}

	var options []systemdRunOption
	for i := start; i < len(words); {
		if words[i].value == "--" {
			return options
		}
		name, hasInlineValue, ok := parseSystemdRunOption(words[i].value)
		if !ok {
			return options
		}
		end := i + 1
		dynamic := words[i].dynamic
		if !hasInlineValue {
			if end == len(words) {
				options = append(options, systemdRunOption{name: name, dynamic: dynamic})
				return options
			}
			dynamic = words[end].dynamic
			end++
		}
		options = append(options, systemdRunOption{name: name, dynamic: dynamic})
		i = end
	}
	return options
}

func parseSystemdRunOption(word string) (name string, hasInlineValue, ok bool) {
	if len(word) < 2 || word[0] != '-' {
		return "", false, false
	}
	nameStart := 1
	if word[1] == '-' {
		if len(word) == 2 {
			return "", false, false
		}
		nameStart = 2
	}
	if word[nameStart] == '-' || word[nameStart] == '=' {
		return "", false, false
	}
	name, _, hasInlineValue = strings.Cut(word[nameStart:], "=")
	if !systemdRunFlag(name) {
		return "", false, false
	}
	return name, hasInlineValue, true
}

// systemdStopTimeout returns the value of the TimeoutStopSec directive of a
// unit.
//
// The directive is the deadline of the supervisor: systemd asks the instance
// to stop and kills it when the deadline is over. The deadline is the shared
// budget of the service layer, so it covers the whole orderly shutdown of
// the server and can never be shorter than it.
//
// The value is written as a duration, the form systemd reads in every unit.
func systemdStopTimeout() string {
	return ShutdownBudget.String()
}

// unitIdentity renders the identity directives of a unit.
//
// A machine-wide instance runs as the dedicated service account of the
// platform: the unit names the account and its group, so the supervisor
// starts the instance with the least privilege it needs and not with the
// identity of the administrator who installed it.
//
// The group of the unit is the primary group of the account, never the name
// of the account: an account whose primary group is named after it only by
// accident (a wheel group, a shared group, a group of a directory server)
// would otherwise name a group that does not exist, and the unit would fail
// to start with a group error the installation never sees.
//
// A group that cannot be resolved is left out of the unit: the supervisor
// then uses the default group of the account, which is the primary group of
// the account itself. A name is never guessed.
//
// A user scope instance runs as the user of the session, so it names no
// account at all.
func unitIdentity(opts InstallOptions, account *user.User) (string, error) {
	if opts.Scope != ScopeSystem {
		return "", nil
	}
	name := serviceAccountName(opts)
	if name == "" {
		return "", errors.New("service: the machine-wide instance needs a service account")
	}
	identity := "User=" + systemdQuote(name) + "\n"
	if account == nil {
		// The account of the instance is unknown: the unit names the
		// account of the command line and no group at all.
		return identity, nil
	}
	group, err := user.LookupGroupId(account.Gid)
	if err != nil || group.Name == "" {
		// The primary group of the account cannot be resolved: the
		// unit names no group, and the supervisor uses the default
		// group of the account.
		return identity, nil
	}
	return identity + "Group=" + systemdQuote(group.Name) + "\n", nil
}

// unitHomeDir returns the HOME of the registered process.
//
// A user scope instance runs as the user of the session, so it keeps the
// home of that user. A machine wide instance never inherits the home of the
// user who runs the install command: it uses the machine level state
// directory as its home.
func unitHomeDir(opts InstallOptions) (string, error) {
	if opts.Scope == ScopeSystem {
		return systemServiceHome, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("service: the home directory of the user is unknown: %w", err)
	}
	if home == "" {
		return "", errors.New("service: the home directory of the user is unknown")
	}
	return home, nil
}

// systemdQuote renders one value of a unit file. The value is quoted only
// when it needs it, so a plain path stays readable.
//
// The value is a setting of the unit and not a part of its command line:
// the supervisor expands the environment of the unit in a command line only,
// so a dollar sign of such a value is a literal one.
func systemdQuote(value string) string {
	if value == "" {
		return `""`
	}
	if !strings.ContainsAny(value, " \t\"'\\%") {
		return value
	}
	return systemdQuoteAlways(value)
}

// systemdWorkingDirectoryValue leaves path bytes intact; unlike ExecStart,
// WorkingDirectory does not perform C-style unescaping. Only literal percent
// signs need escaping from native systemd specifier expansion. A final odd
// backslash gets a separator space so it cannot continue the unit-file line.
func systemdWorkingDirectoryValue(value string) (string, error) {
	if strings.HasSuffix(value, " ") {
		return "", fmt.Errorf("service: the state dir %q cannot be represented by systemd WorkingDirectory because trailing spaces are stripped", value)
	}
	value = strings.ReplaceAll(value, "%", "%%")
	backslashes := 0
	for i := len(value) - 1; i >= 0 && value[i] == '\\'; i-- {
		backslashes++
	}
	if backslashes%2 != 0 {
		// Keep an odd final backslash from continuing the unit-file line.
		value += " "
	}
	return value, nil
}

func parseSystemdWorkingDirectory(value string) (string, bool) {
	var b strings.Builder
	dynamic := false
	for i := 0; i < len(value); {
		if value[i] == '%' && i+1 < len(value) {
			if value[i+1] == '%' {
				b.WriteByte('%')
				i += 2
				continue
			}
			dynamic = true
		}
		b.WriteByte(value[i])
		i++
	}
	return b.String(), dynamic
}

// systemdQuoteAlways renders one value of a unit file in the C style
// quoting of systemd, no matter what the value contains.
func systemdQuoteAlways(value string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '%':
			// A percent sign starts a specifier of the supervisor, a
			// doubled one is a literal percent sign.
			b.WriteString("%%")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// systemdQuoteCommand renders one word of the command line of a unit file.
//
// The command line is the one place where the supervisor expands the
// environment of the unit: a dollar sign starts a reference to a variable of
// the unit, and a doubled dollar sign is the documented escape of a literal
// dollar sign. Every dollar sign of the word is therefore doubled, so the
// process sees the path as it was written.
//
// The word is quoted only when it needs it, so a plain path stays readable.
func systemdQuoteCommand(value string) string {
	if value == "" {
		return `""`
	}
	if !strings.ContainsAny(value, " \t\"'\\%$") {
		return value
	}
	return systemdQuoteCommandAlways(value)
}

// systemdQuoteCommandAlways renders one word of the command line of a unit
// file in the C style quoting of systemd, no matter what the word contains.
//
// The dollar sign of the word is doubled: the supervisor expands the
// environment of the unit in the command line, and a doubled dollar sign is
// the documented escape of a literal dollar sign.
func systemdQuoteCommandAlways(value string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range value {
		switch r {
		case '"':
			b.WriteString(`\"`)
		case '\\':
			b.WriteString(`\\`)
		case '%':
			b.WriteString("%%")
		case '$':
			// A dollar sign starts a variable of the unit, a doubled
			// one is a literal dollar sign.
			b.WriteString("$$")
		default:
			b.WriteRune(r)
		}
	}
	b.WriteByte('"')
	return b.String()
}

// parsedUnit holds the registration facts of a unit file.
type parsedUnit struct {
	// DynamicRunArgs marks the run flags whose source systemd expands as
	// a specifier or an environment reference. The source expressions are
	// kept separate from the manager's typed loaded values, so a rollback
	// can stay conservative about paths such a prior registration may
	// reference.
	DynamicRunArgs map[string]bool
	// ExecArgs are the arguments of the executable.
	ExecArgs []string
	// Program is the program of the registered command: the token at the
	// program slot of systemdExecStartSlot — behind a recognized "env"
	// wrapper the program the wrapper launches, for every other shape the
	// first token, which is the binary the supervisor executes. An
	// unreadable command therefore names its executed binary, never a
	// guessed program.
	Program string
	// RunOffset is the index of the canonical run vector ("service run")
	// in ExecArgs. It skips the words of a recognized wrapper prefix (env
	// options and NAME=VALUE assignments before the program), so the run
	// flags are read after the program exactly as for a direct command,
	// and never from the words of the wrapper itself. An unrecognized
	// command keeps 0, the position the vector starts at in a direct
	// command.
	RunOffset int
	// WorkingDirectory is the working directory of the unit.
	WorkingDirectory        string
	WorkingDirectoryDynamic bool
	// User is the account a machine-wide unit runs as. A user scope unit
	// names no account, so the value is empty there.
	User  string
	Group string
	// Environment holds the environment variables of the unit.
	Environment map[string]string
}

// listen returns the bind address of the registered instance.
func (u parsedUnit) listen() string {
	return u.flagValue("--listen")
}

// paths returns the runtime paths of the registered instance.
//
// The values come from the registered unit file, so the status describes
// the registration and not the current intent of the caller.
func (u parsedUnit) paths() runtimepaths.Paths {
	paths := runtimepaths.Paths{
		ConfigFile: u.flagValue("--config"),
		StateDir:   u.flagValue("--state-dir"),
		LogDir:     u.flagValue("--log-dir"),
	}
	if paths.StateDir == "" {
		paths.StateDir = u.WorkingDirectory
	}
	return paths
}

// flagValue returns the value of a flag of the argument vector of the unit.
//
// The vector is read from the run vector, after a recognized wrapper
// prefix, so the words of the wrapper are never mistaken for run flags.
func (u parsedUnit) flagValue(flag string) string {
	return runArgValue(u.ExecArgs[u.RunOffset:], flag)
}

// parseUnit reads the registration facts of a unit file.
//
// The parser is tolerant: a unit file that was edited by hand is described
// as far as it can be read, so the status of the registration stays
// meaningful.
func parseUnit(content string) parsedUnit {
	unit := parsedUnit{Environment: map[string]string{}}
	section := ""
	for _, logical := range systemdLogicalLines(content) {
		line := logical.text
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";") {
			continue
		}
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section = strings.ToLower(strings.TrimSpace(trimmed[1 : len(trimmed)-1]))
			continue
		}
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		if section != "service" {
			continue
		}
		switch strings.TrimSpace(key) {
		case "ExecStart":
			tokens := splitSystemdWordTokens(value)
			if len(tokens) == 0 {
				continue
			}
			values := make([]string, len(tokens))
			for i, token := range tokens {
				values[i] = token.value
			}
			words := resolveSystemdDollars(values)
			// The run flags follow the program token, behind a
			// recognized wrapper prefix; an unrecognized command
			// keeps reading from the first argument, as before.
			runOffset, _ := systemdExecStartSlot(tokens)
			unit.RunOffset = runOffset
			unit.DynamicRunArgs = dynamicRunArgValues(tokens[1+runOffset:])
			unit.ExecArgs = words[1:]
			// The program of the command sits at the same slot; an
			// unrecognized shape keeps the first token.
			unit.Program = words[runOffset]
		case "WorkingDirectory":
			unit.WorkingDirectory, unit.WorkingDirectoryDynamic = parseSystemdWorkingDirectory(value)
		case "User":
			if words := splitSystemdWordTokens(value); len(words) > 0 {
				unit.User = words[0].value
			}
		case "Group":
			if words := splitSystemdWordTokens(value); len(words) > 0 {
				unit.Group = words[0].value
			}
		case "Environment":
			for _, word := range splitSystemdWords(value) {
				name, envValue, ok := strings.Cut(word, "=")
				if !ok {
					continue
				}
				unit.Environment[name] = envValue
			}
		}
	}
	if unit.WorkingDirectoryDynamic {
		if _, hasStateArg := unit.DynamicRunArgs["state-dir"]; !hasStateArg {
			if unit.DynamicRunArgs == nil {
				unit.DynamicRunArgs = make(map[string]bool)
			}
			unit.DynamicRunArgs["state-dir"] = true
		}
	}
	return unit
}

type systemdLogicalLine struct {
	text       string
	start, end int
}

// systemdLogicalLines joins the physical lines systemd continues before it
// parses assignments. start/end retain the original span so an updated
// directive can replace its whole continued value without leaving orphaned
// physical lines behind.
func systemdLogicalLines(content string) []systemdLogicalLine {
	physical := strings.Split(content, "\n")
	logical := make([]systemdLogicalLine, 0, len(physical))
	for start := 0; start < len(physical); {
		line := strings.TrimSuffix(physical[start], "\r")
		end := start
		for hasSystemdLineContinuation(line) {
			line = line[:len(line)-1] + " "
			next := end + 1
			for next < len(physical) && isSystemdCommentLine(physical[next]) {
				next++
			}
			if next >= len(physical) {
				end = len(physical) - 1
				break
			}
			line += strings.TrimSuffix(physical[next], "\r")
			end = next
		}
		logical = append(logical, systemdLogicalLine{text: line, start: start, end: end})
		start = end + 1
	}
	return logical
}

func isSystemdCommentLine(line string) bool {
	trimmed := strings.TrimLeft(line, " \t\r\n")
	return strings.HasPrefix(trimmed, "#") || strings.HasPrefix(trimmed, ";")
}

func hasSystemdLineContinuation(line string) bool {
	backslashes := 0
	for i := len(line) - 1; i >= 0 && line[i] == '\\'; i-- {
		backslashes++
	}
	return backslashes%2 == 1
}

type systemdWord struct {
	value   string
	dynamic bool
}

// splitSystemdWords implements systemd's ExecStart word boundary: whitespace
// separates unquoted words, quotes group a word, and C escapes are decoded
// without re-splitting the escaped result. Unknown escapes remain literal, as
// systemd's relaxed syntax path does after warning.
func splitSystemdWords(line string) []string {
	tokens := splitSystemdWordTokens(line)
	words := make([]string, len(tokens))
	for i, token := range tokens {
		words[i] = token.value
	}
	return words
}

func splitSystemdWordTokens(line string) []systemdWord {
	var (
		words     []systemdWord
		word      strings.Builder
		expansion strings.Builder
		quote     byte
		inWord    bool
	)
	flush := func() {
		if !inWord {
			return
		}
		words = append(words, systemdWord{
			value:   word.String(),
			dynamic: hasSystemdExpansion(expansion.String()),
		})
		word.Reset()
		expansion.Reset()
		inWord = false
	}
	for i := 0; i < len(line); {
		c := line[i]
		if c == '\\' {
			inWord = true
			value, next := unescapeSystemdSequence(line, i)
			word.WriteString(value)
			expansion.WriteString(value)
			i = next
			continue
		}
		if c == '%' && i+1 < len(line) && line[i+1] == '%' {
			inWord = true
			word.WriteByte('%')
			expansion.WriteString("%%")
			i += 2
			continue
		}
		if quote != 0 {
			if c == quote {
				quote = 0
			} else {
				word.WriteByte(c)
				expansion.WriteByte(c)
			}
			i++
			continue
		}
		switch c {
		case '\'', '"':
			inWord = true
			quote = c
		case ' ', '\t', '\n', '\r':
			flush()
		default:
			inWord = true
			word.WriteByte(c)
			expansion.WriteByte(c)
		}
		i++
	}
	flush()
	return words
}

func hasSystemdExpansion(value string) bool {
	for i := 0; i < len(value); i++ {
		switch value[i] {
		case '$':
			if i+1 < len(value) && value[i+1] == '$' {
				i++
				continue
			}
			if i+1 < len(value) && (value[i+1] == '{' || isSystemdVariableNameStart(value[i+1])) {
				return true
			}
		case '%':
			if i+1 < len(value) && value[i+1] == '%' {
				i++
				continue
			}
			if i+1 < len(value) {
				return true
			}
		}
	}
	return false
}

func isSystemdVariableNameStart(c byte) bool {
	return c == '_' || c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z'
}

func dynamicRunArgValues(args []systemdWord) map[string]bool {
	values := make(map[string]bool)
	for _, option := range scanSystemdRunArgs(args) {
		values[option.name] = option.dynamic
	}
	return values
}

func systemdRunFlag(name string) bool {
	switch name {
	case "config", "state-dir", "log-dir", "listen", "cache-dir", "hub-dir", "local-dir":
		return true
	default:
		return false
	}
}

func unescapeSystemdSequence(line string, slash int) (string, int) {
	if slash+1 >= len(line) {
		return "\\", slash + 1
	}
	code := line[slash+1]
	switch code {
	case 'a':
		return "\a", slash + 2
	case 'b':
		return "\b", slash + 2
	case 'f':
		return "\f", slash + 2
	case 'n':
		return "\n", slash + 2
	case 'r':
		return "\r", slash + 2
	case 't':
		return "\t", slash + 2
	case 'v':
		return "\v", slash + 2
	case '\\':
		return "\\", slash + 2
	case '"':
		return `"`, slash + 2
	case '\'':
		return "'", slash + 2
	case 's':
		return " ", slash + 2
	case 'x':
		if slash+4 <= len(line) {
			value, err := strconv.ParseUint(line[slash+2:slash+4], 16, 8)
			if err == nil && value != 0 {
				return string([]byte{byte(value)}), slash + 4
			}
		}
	case 'u':
		if slash+6 <= len(line) {
			value, err := strconv.ParseUint(line[slash+2:slash+6], 16, 16)
			if err == nil && value != 0 && value <= utf8.MaxRune && utf8.ValidRune(rune(value)) {
				return string(rune(value)), slash + 6
			}
		}
	case 'U':
		if slash+10 <= len(line) {
			value, err := strconv.ParseUint(line[slash+2:slash+10], 16, 32)
			if err == nil {
				if r, valid := systemdUnicode32EscapeRune(value); valid {
					return string(r), slash + 10
				}
			}
		}
	default:
		if code >= '0' && code <= '7' && slash+4 <= len(line) {
			value, err := strconv.ParseUint(line[slash+1:slash+4], 8, 8)
			if err == nil && value != 0 {
				return string([]byte{byte(value)}), slash + 4
			}
		}
	}
	return line[slash : slash+2], slash + 2
}

// resolveSystemdDollars resolves the escaped dollar signs of the words of a
// command line.
//
// The supervisor expands the environment of the unit in a command line, and a
// doubled dollar sign is the documented escape of a literal dollar sign. The
// parser resolves it the way the supervisor does, so the registration of a
// path that holds a dollar sign describes the path the process really sees.
//
// The words of a setting of the unit are not touched: the supervisor does not
// expand the environment of the unit there.
func resolveSystemdDollars(words []string) []string {
	for i, word := range words {
		if !strings.Contains(word, "$$") {
			continue
		}
		words[i] = strings.ReplaceAll(word, "$$", "$")
	}
	return words
}
