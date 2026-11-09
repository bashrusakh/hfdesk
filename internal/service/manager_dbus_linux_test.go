// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build linux

package service

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/godbus/dbus/v5"
)

func TestSystemdParseShow(t *testing.T) {
	cases := []struct {
		name   string
		output string
		want   State
	}{
		{
			name:   "not found",
			output: "LoadState=not-found\nActiveState=inactive\nSubState=dead",
			want:   StateNotInstalled,
		},
		{
			name:   "active",
			output: "LoadState=loaded\nActiveState=active\nSubState=running",
			want:   StateRunning,
		},
		{
			name:   "inactive",
			output: "LoadState=loaded\nActiveState=inactive\nSubState=dead",
			want:   StateStopped,
		},
		{
			name:   "activating",
			output: "LoadState=loaded\nActiveState=activating\nSubState=start",
			want:   StateStarting,
		},
		{
			name:   "deactivating",
			output: "LoadState=loaded\nActiveState=deactivating\nSubState=stop",
			want:   StateStopping,
		},
		{
			name:   "failed",
			output: "LoadState=loaded\nActiveState=failed\nSubState=failed",
			want:   StateFailed,
		},
		{
			name:   "unknown active state",
			output: "LoadState=loaded\nActiveState=reloading\nSubState=reload",
			want:   StateUnknown,
		},
		{
			name:   "empty output",
			output: "",
			want:   StateUnknown,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := parseSystemdShow(tc.output)
			if got.State != tc.want {
				t.Errorf("parseSystemdShow(%q).State = %q, want %q", tc.output, got.State, tc.want)
			}
		})
	}
}

func TestSystemdParseShowReadsTheProperties(t *testing.T) {
	output := "LoadState=loaded\nActiveState=active\nSubState=running\n"
	show := parseSystemdShow(output)
	if show.LoadState != "loaded" {
		t.Errorf("LoadState = %q, want %q", show.LoadState, "loaded")
	}
	if show.ActiveState != "active" {
		t.Errorf("ActiveState = %q, want %q", show.ActiveState, "active")
	}
	if show.SubState != "running" {
		t.Errorf("SubState = %q, want %q", show.SubState, "running")
	}
}

func TestLoadedConfigDecodesTypedExecStartArguments(t *testing.T) {
	wantArgs := []string{
		"/srv/HF Desk/bin/hfdesk", "service", "run", "--config",
		"/srv/HF Desk/config with spaces.json", "--empty", "", "--literal", "quote\" and \\ slash $HOME",
	}
	properties := map[string]dbus.Variant{
		"ExecStartEx": dbus.MakeVariant([]systemdExecStartExEntry{{
			Path:  wantArgs[0],
			Argv:  wantArgs,
			Flags: []string{"full-privileges"},
		}}),
		"WorkingDirectory": dbus.MakeVariant("/srv/HF Desk/state"),
		"User":             dbus.MakeVariant("hfdesk"),
		"Group":            dbus.MakeVariant("hfdesk"),
		"Environment": dbus.MakeVariant([]string{
			`HF_HOME=/srv/HF Desk/cache`, `HF_HUB_CACHE=/srv/HF Desk/hub`, `HF_LABEL=quoted "value"`,
		}),
		"EnvironmentFiles": dbus.MakeVariant([]systemdEnvironmentFile{{Path: "/etc/hfdesk/cache file.env", IgnoreErrors: true}}),
	}
	got, err := loadedConfigFromProperties(properties)
	if err != nil {
		t.Fatalf("loadedConfigFromProperties() = %v", err)
	}
	if !got.loaded || got.execPath != wantArgs[0] || !slices.Equal(got.execArgs, wantArgs) || !slices.Equal(got.execFlags, []string{"full-privileges"}) {
		t.Errorf("loaded ExecStart = %+v, want the typed argv and flags", got)
	}
	if got.workingDirectory != "/srv/HF Desk/state" || got.user != "hfdesk" || got.group != "hfdesk" {
		t.Errorf("loaded launch identity = %+v, want the typed values", got)
	}
	if !slices.Equal(got.environment, []string{`HF_HOME=/srv/HF Desk/cache`, `HF_HUB_CACHE=/srv/HF Desk/hub`, `HF_LABEL=quoted "value"`}) {
		t.Errorf("loaded Environment = %q, want each typed variable intact", got.environment)
	}
	if !slices.Equal(got.environmentFiles, []systemdEnvironmentFile{{Path: "/etc/hfdesk/cache file.env", IgnoreErrors: true}}) {
		t.Errorf("loaded EnvironmentFiles = %+v, want the typed path and ignore flag", got.environmentFiles)
	}
}

func TestLoadedConfigDecodesLegacyTypedExecStart(t *testing.T) {
	want := []string{"/opt/HF Desk/hfdesk", "--config", "/etc/HF Desk/hfdesk.json"}
	properties := map[string]dbus.Variant{
		"ExecStart":        dbus.MakeVariant([]systemdExecStartEntry{{Path: want[0], Argv: want}}),
		"WorkingDirectory": dbus.MakeVariant(""),
		"User":             dbus.MakeVariant(""),
		"Group":            dbus.MakeVariant(""),
		"Environment":      dbus.MakeVariant([]string{}),
		"EnvironmentFiles": dbus.MakeVariant([]systemdEnvironmentFile{}),
	}
	got, err := loadedConfigFromProperties(properties)
	if err != nil {
		t.Fatalf("loadedConfigFromProperties() = %v", err)
	}
	if got.execPath != want[0] || !slices.Equal(got.execArgs, want) || got.ignoreErrors {
		t.Errorf("legacy loaded ExecStart = %+v, want the lossless typed argv", got)
	}
}

func TestLoadedConfigRejectsUnavailableTypedProperties(t *testing.T) {
	properties := map[string]dbus.Variant{
		"WorkingDirectory": dbus.MakeVariant(""),
		"User":             dbus.MakeVariant(""),
		"Group":            dbus.MakeVariant(""),
		"Environment":      dbus.MakeVariant([]string{}),
		"EnvironmentFiles": dbus.MakeVariant([]systemdEnvironmentFile{}),
	}
	if _, err := loadedConfigFromProperties(properties); err == nil || !strings.Contains(err.Error(), "ExecStart") {
		t.Fatalf("loadedConfigFromProperties() error = %v, want missing typed ExecStart error", err)
	}
}

func TestLoadedConfigRejectsWrongExecStartSignatureForEmptyArray(t *testing.T) {
	properties := systemdTypedProperties("", []string{}, []string{}, "", "", "", []systemdEnvironmentFile{})
	properties["ExecStartEx"] = dbus.MakeVariant([]systemdExecStartEntry{})
	if _, err := loadedConfigFromProperties(properties); err == nil || !strings.Contains(err.Error(), `ExecStartEx with signature "a(sasbttttuii)"`) {
		t.Fatalf("loadedConfigFromProperties() error = %v, want wrong ExecStartEx signature", err)
	}
}

func TestDBusPropertiesFromCallPreservesTypedValues(t *testing.T) {
	raw := map[string]dbus.Variant{
		"ExecStartEx": dbus.MakeVariant([]systemdExecStartExEntry{{
			Path: "/bin/true", Argv: []string{"/bin/true", ""}, Flags: []string{},
		}}),
		"EnvironmentFiles": dbus.MakeVariant([]systemdEnvironmentFile{{Path: "/etc/hfdesk.env", IgnoreErrors: true}}),
		"WorkingDirectory": dbus.MakeVariant("/srv/hfdesk"),
		"User":             dbus.MakeVariant(""),
		"Group":            dbus.MakeVariant(""),
		"Environment":      dbus.MakeVariant([]string{}),
	}
	call := &dbus.Call{Body: []interface{}{raw}}
	if raw["ExecStartEx"].Signature().String() != "a(sasasttttuii)" || raw["EnvironmentFiles"].Signature().String() != "a(sb)" {
		t.Fatalf("fixture signatures changed: ExecStartEx=%s EnvironmentFiles=%s", raw["ExecStartEx"].Signature(), raw["EnvironmentFiles"].Signature())
	}
	properties, err := dbusPropertiesFromCall(call)
	if err != nil {
		t.Fatalf("dbusPropertiesFromCall() = %v", err)
	}
	if properties["ExecStartEx"].Signature().String() != "a(sasasttttuii)" || properties["EnvironmentFiles"].Signature().String() != "a(sb)" {
		t.Errorf("production extraction changed signatures: ExecStartEx=%s EnvironmentFiles=%s", properties["ExecStartEx"].Signature(), properties["EnvironmentFiles"].Signature())
	}
	if _, err := loadedConfigFromProperties(properties); err != nil {
		t.Errorf("loadedConfigFromProperties(raw) = %v, want a valid typed projection", err)
	}
}

func TestSessionBusAddressUsesCurrentContextWithoutMutatingEnvironment(t *testing.T) {
	runtimeDir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", runtimeDir)
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", "")
	busPath := filepath.Join(runtimeDir, "bus")
	if err := os.WriteFile(busPath, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	want := "unix:path=" + dbus.EscapeBusAddressValue(busPath)
	if got := sessionBusAddress(); got != want {
		t.Errorf("sessionBusAddress() = %q, want discovered runtime bus %q", got, want)
	}
	if got := os.Getenv("DBUS_SESSION_BUS_ADDRESS"); got != "" {
		t.Errorf("session discovery mutated DBUS_SESSION_BUS_ADDRESS to %q", got)
	}
	if err := os.Remove(busPath); err != nil {
		t.Fatal(err)
	}
	const discovered = "unix:abstract=/tmp/hfdesk-test-session"
	if err := os.WriteFile(filepath.Join(runtimeDir, "dbus-session"), []byte("DBUS_SESSION_BUS_ADDRESS="+discovered+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if got := sessionBusAddress(); got != discovered {
		t.Errorf("sessionBusAddress() = %q, want dbus-session-file address %q", got, discovered)
	}
	const explicit = "unix:abstract=/tmp/hfdesk-explicit-session"
	t.Setenv("DBUS_SESSION_BUS_ADDRESS", explicit)
	if got := sessionBusAddress(); got != explicit {
		t.Errorf("sessionBusAddress() = %q, want explicit address %q", got, explicit)
	}
}

func setSystemdTestPropertyReader(t *testing.T, manager *systemdManager) {
	t.Helper()
	loadedPath := os.Getenv("HFDESK_TEST_SYSTEMCTL_LOADED_UNIT")
	manager.readServiceProperties = func(_ context.Context, name string) (map[string]dbus.Variant, error) {
		if name != unitName {
			return nil, fmt.Errorf("unexpected unit %q", name)
		}
		content, err := os.ReadFile(loadedPath)
		if err != nil {
			return nil, err
		}
		return systemdTestPropertiesFromUnit(string(content)), nil
	}
}

func systemdTestPropertiesFromUnit(content string) map[string]dbus.Variant {
	var commands []systemdExecStartExEntry
	var environment []string
	var environmentFiles []systemdEnvironmentFile
	var supplementaryGroups []string
	section := ""
	for _, logical := range systemdLogicalLines(content) {
		trimmed := strings.TrimSpace(logical.text)
		if strings.HasPrefix(trimmed, "[") && strings.HasSuffix(trimmed, "]") {
			section = strings.ToLower(strings.TrimSpace(trimmed[1 : len(trimmed)-1]))
			continue
		}
		if section != "service" {
			continue
		}
		key, value, ok := strings.Cut(trimmed, "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "ExecStart":
			// The typed projection the supervisor stores is the word
			// vector of the directive the way the file carries it.
			argv := splitSystemdWords(value)
			if len(argv) > 0 {
				commands = []systemdExecStartExEntry{{Path: argv[0], Argv: argv}}
			}
		case "Environment":
			environment = append(environment, splitSystemdWords(value)...)
		case "SupplementaryGroups":
			if strings.TrimSpace(value) == "" {
				supplementaryGroups = nil
			} else {
				supplementaryGroups = append(supplementaryGroups, splitSystemdWords(value)...)
			}
		case "EnvironmentFile":
			for _, entry := range splitSystemdWords(value) {
				file := systemdEnvironmentFile{}
				if strings.HasPrefix(entry, "-") {
					file.IgnoreErrors = true
					entry = strings.TrimPrefix(entry, "-")
				}
				file.Path = entry
				environmentFiles = append(environmentFiles, file)
			}
		}
	}
	unit := parseUnit(content)
	return map[string]dbus.Variant{
		"ExecStartEx":         dbus.MakeVariant(commands),
		"WorkingDirectory":    dbus.MakeVariant(unit.WorkingDirectory),
		"User":                dbus.MakeVariant(unit.User),
		"Group":               dbus.MakeVariant(unit.Group),
		"SupplementaryGroups": dbus.MakeVariant(supplementaryGroups),
		"MainPID":             dbus.MakeVariant(uint32(0)),
		"Environment":         dbus.MakeVariant(environment),
		"EnvironmentFiles":    dbus.MakeVariant(environmentFiles),
		"FragmentPath":        dbus.MakeVariant("/tmp/hfdesk-test-fragment.service"),
		"DropInPaths":         dbus.MakeVariant([]string{}),
	}
}

func systemdTypedProperties(execPath string, argv, environment []string, workingDirectory, userName, groupName string, environmentFiles []systemdEnvironmentFile) map[string]dbus.Variant {
	return map[string]dbus.Variant{
		"ExecStartEx": dbus.MakeVariant([]systemdExecStartExEntry{{
			Path:  execPath,
			Argv:  slices.Clone(argv),
			Flags: []string{},
		}}),
		"WorkingDirectory":    dbus.MakeVariant(workingDirectory),
		"User":                dbus.MakeVariant(userName),
		"Group":               dbus.MakeVariant(groupName),
		"SupplementaryGroups": dbus.MakeVariant([]string{}),
		"MainPID":             dbus.MakeVariant(uint32(0)),
		"Environment":         dbus.MakeVariant(slices.Clone(environment)),
		"EnvironmentFiles":    dbus.MakeVariant(slices.Clone(environmentFiles)),
		"FragmentPath":        dbus.MakeVariant("/tmp/hfdesk-test-fragment.service"),
		"DropInPaths":         dbus.MakeVariant([]string{}),
	}
}

func setSystemdTypedPropertySequence(t *testing.T, manager *systemdManager, properties ...map[string]dbus.Variant) {
	t.Helper()
	if len(properties) == 0 {
		t.Fatal("a typed manager sequence must include at least one state")
	}
	reloadCountPath := os.Getenv("HFDESK_TEST_SYSTEMCTL_RELOAD_COUNT")
	manager.readServiceProperties = func(_ context.Context, name string) (map[string]dbus.Variant, error) {
		if name != unitName {
			return nil, fmt.Errorf("unexpected unit %q", name)
		}
		index := 0
		content, err := os.ReadFile(reloadCountPath)
		if err == nil {
			count, parseErr := strconv.Atoi(strings.TrimSpace(string(content)))
			if parseErr != nil {
				return nil, parseErr
			}
			index = count
		} else if !errors.Is(err, fs.ErrNotExist) {
			return nil, err
		}
		if index >= len(properties) {
			index = len(properties) - 1
		}
		return properties[index], nil
	}
}

func TestSystemdDetectExecStartOverride(t *testing.T) {
	content := []byte(canonicalGoldenSystemUnitText)
	t.Setenv("HOME", "/home/golden")
	facts, err := readCanonicalUnit(content, ScopeSystem)
	if err != nil {
		t.Fatalf("readCanonicalUnit() = %v, want the facts of the golden machine-wide unit", err)
	}

	matching, err := loadedConfigFromProperties(systemdTestPropertiesFromUnit(string(content)))
	if err != nil {
		t.Fatalf("decode the matching typed configuration: %v", err)
	}
	if err := detectExecStartOverride(matching, facts, ScopeSystem); err != nil {
		t.Errorf("detectExecStartOverride() over the matching command = %v, want nil", err)
	}
	if err := detectExecStartOverride(systemdLoadedConfig{}, facts, ScopeSystem); err != nil {
		t.Errorf("detectExecStartOverride() over an unloaded unit = %v, want nil", err)
	}

	argv := splitSystemdWords(systemdExecStart(facts.installOptions()))
	override := slices.Clone(argv)
	for i, arg := range override {
		if arg == "--config" {
			override[i+1] = "/etc/admin/hfdesk.json"
			break
		}
	}
	if slices.Equal(override, argv) {
		t.Fatal("the override fixture did not change the config operand")
	}
	driftedProperties := func(dropIns []string) map[string]dbus.Variant {
		properties := systemdTestPropertiesFromUnit(string(content))
		properties["ExecStartEx"] = dbus.MakeVariant([]systemdExecStartExEntry{{Path: argv[0], Argv: override}})
		properties["DropInPaths"] = dbus.MakeVariant(dropIns)
		return properties
	}

	// writeDropIn writes a drop-in file and returns its path: the update
	// reads the drop-ins the supervisor reports to find the file that
	// carries the override.
	writeDropIn := func(t *testing.T, name, body string) string {
		t.Helper()
		path := filepath.Join(t.TempDir(), name)
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatalf("write the drop-in: %v", err)
		}
		return path
	}

	t.Run("drop-in owns the effective command", func(t *testing.T) {
		dropIn := writeDropIn(t, "local.conf", "[Service]\nExecStart="+strings.Join(override, " ")+"\n")
		loaded, err := loadedConfigFromProperties(driftedProperties([]string{dropIn}))
		if err != nil {
			t.Fatalf("decode the typed override: %v", err)
		}
		err = detectExecStartOverride(loaded, facts, ScopeSystem)
		if err == nil {
			t.Fatal("detectExecStartOverride() = nil, want the refusal of an overridden command")
		}
		if !strings.Contains(err.Error(), `drop-ins: "`+dropIn+`"`) {
			t.Errorf("the refusal %q does not name the DropInPaths the supervisor reported", err)
		}
		// The override already lives in local.conf, the very file the
		// install summary recommends: the action must remove it there
		// instead of moving customizations into the same file again.
		assertSingleRemoveOverrideAction(t, err, dropIn)
	})

	t.Run("two drop-ins, only the second carries the override", func(t *testing.T) {
		decoy := writeDropIn(t, "00-other.conf", "[Service]\nEnvironment=HF_HUB_CACHE=/operator/hub\n")
		owner := writeDropIn(t, "local.conf", "[Service]\nExecStart="+strings.Join(override, " ")+"\n")
		loaded, err := loadedConfigFromProperties(driftedProperties([]string{decoy, owner}))
		if err != nil {
			t.Fatalf("decode the typed override: %v", err)
		}
		err = detectExecStartOverride(loaded, facts, ScopeSystem)
		if err == nil {
			t.Fatal("detectExecStartOverride() = nil, want the refusal of an overridden command")
		}
		// The action names the exact file that carries the override, not
		// the first drop-in the supervisor reports.
		assertSingleRemoveOverrideAction(t, err, owner)
		if strings.Contains(err.Error(), `Remove the "ExecStart=" override from "`+decoy+`"`) {
			t.Errorf("the refusal %q names the removal from a drop-in without the override", err)
		}
	})

	t.Run("every readable carrier of several is named", func(t *testing.T) {
		first := writeDropIn(t, "00-first.conf", "[Service]\nExecStart=/usr/bin/first --config /one.json\n")
		last := writeDropIn(t, "local.conf", "[Service]\nExecStart="+strings.Join(override, " ")+"\n")
		loaded, err := loadedConfigFromProperties(driftedProperties([]string{first, last}))
		if err != nil {
			t.Fatalf("decode the typed override: %v", err)
		}
		err = detectExecStartOverride(loaded, facts, ScopeSystem)
		if err == nil {
			t.Fatal("detectExecStartOverride() = nil, want the refusal of an overridden command")
		}
		// Review finding 2 (b): with several readable carriers one
		// removal does not clear the cause — the one instruction names
		// EVERY carrier in the plural form (the merge order of the
		// drop-ins no longer decides what the action covers), offers
		// no alternative and stays a single action.
		message := err.Error()
		for _, want := range []string{
			"the update was refused and the registration was not changed",
			"the drop-ins carrying the override: \"" + first + "\", \"" + last + "\"",
			`Remove the "ExecStart=" override from each of them`,
			"reload the unit files with \"",
			`run "hfdesk service install" again`,
		} {
			if !strings.Contains(message, want) {
				t.Errorf("the refusal %q does not contain %q", message, want)
			}
		}
		if strings.Contains(message, " or ") {
			t.Errorf("the refusal %q offers an alternative instead of one action", message)
		}
		if got := strings.Count(message, `Remove the "ExecStart=" override`); got != 1 {
			t.Errorf("the refusal %q states %d removal actions, want exactly one", message, got)
		}
		if strings.Contains(message, "Move your customizations into") {
			t.Errorf("the refusal %q names the customization move, which never clears an override", message)
		}
	})

	t.Run("a drop-in without an override names the reload", func(t *testing.T) {
		custom := writeDropIn(t, "local.conf", "[Service]\nEnvironment=HF_HUB_CACHE=/operator/hub\n")
		loaded, err := loadedConfigFromProperties(driftedProperties([]string{custom}))
		if err != nil {
			t.Fatalf("decode the typed override: %v", err)
		}
		err = detectExecStartOverride(loaded, facts, ScopeSystem)
		if err == nil {
			t.Fatal("detectExecStartOverride() = nil, want the refusal of a drifted command")
		}
		// No reported drop-in carries an ExecStart=: the loaded command
		// predates the file the supervisor holds, and only a reload makes
		// the supervisor read it again.
		assertSingleReloadAction(t, err, "systemctl")
	})

	t.Run("drift without a reported drop-in", func(t *testing.T) {
		loaded, err := loadedConfigFromProperties(driftedProperties([]string{}))
		if err != nil {
			t.Fatalf("decode the typed override: %v", err)
		}
		err = detectExecStartOverride(loaded, facts, ScopeSystem)
		if err == nil {
			t.Fatal("detectExecStartOverride() = nil, want the fail-closed refusal of a drifted command")
		}
		if strings.Contains(err.Error(), "(drop-ins:") {
			t.Errorf("the refusal %q names drop-ins the supervisor did not report", err)
		}
		assertSingleReloadAction(t, err, "systemctl")
	})

	t.Run("an unreadable drop-in names no clearing command", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads every file; the unreadable state needs an unprivileged reader")
		}
		dropIn := writeDropIn(t, "local.conf", "[Service]\nExecStart="+strings.Join(override, " ")+"\n")
		if err := os.Chmod(dropIn, 0); err != nil {
			t.Fatalf("make the drop-in unreadable: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(dropIn, 0o644) })

		loaded, err := loadedConfigFromProperties(driftedProperties([]string{dropIn}))
		if err != nil {
			t.Fatalf("decode the typed override: %v", err)
		}
		err = detectExecStartOverride(loaded, facts, ScopeSystem)
		if err == nil {
			t.Fatal("detectExecStartOverride() = nil, want the refusal over an unreadable drop-in")
		}
		// Honest terminal: this unreadable drop-in may carry the
		// override, so no command is named — a reload or a retry could
		// not clear that state — and the refusal says it repeats until
		// the override is removed.
		for _, want := range []string{
			"cannot read every drop-in the supervisor reports",
			"names no command",
			"the refusal repeats until the ExecStart= override is removed",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal %q does not mention %q", err, want)
			}
		}
		for _, unwanted := range []string{
			`Reload the unit files with`,
			`run "hfdesk service install"`,
			`Remove the "ExecStart=" override`,
			" or ",
		} {
			if strings.Contains(err.Error(), unwanted) {
				t.Errorf("the refusal %q names %q, want no command that cannot clear this state", err, unwanted)
			}
		}
	})

	t.Run("an unreadable drop-in beside a readable holder names no command", func(t *testing.T) {
		if os.Geteuid() == 0 {
			t.Skip("root reads every file; the unreadable state needs an unprivileged reader")
		}
		decoy := writeDropIn(t, "00-unreadable.conf", "[Service]\nEnvironment=HF_HUB_CACHE=/decoy\n")
		if err := os.Chmod(decoy, 0); err != nil {
			t.Fatalf("make the decoy unreadable: %v", err)
		}
		t.Cleanup(func() { _ = os.Chmod(decoy, 0o644) })
		owner := writeDropIn(t, "local.conf", "[Service]\nExecStart="+strings.Join(override, " ")+"\n")

		loaded, err := loadedConfigFromProperties(driftedProperties([]string{decoy, owner}))
		if err != nil {
			t.Fatalf("decode the typed override: %v", err)
		}
		err = detectExecStartOverride(loaded, facts, ScopeSystem)
		if err == nil {
			t.Fatal("detectExecStartOverride() = nil, want the refusal over a partially unreadable drop-in set")
		}
		// Review finding 2 (a): the unreadable decoy cannot be proven
		// free of an ExecStart= override of its own, so removing the
		// override from the readable holder would NOT clear the state —
		// the refusal is the honest terminal exactly as its comment
		// promises ("names no command" for that state). This
		// expectation FLIPS the earlier rule that named the readable
		// holder beside an unreadable drop-in: there the named action
		// did not clear the cause.
		for _, want := range []string{
			"cannot read every drop-in the supervisor reports",
			"names no command",
			"the refusal repeats until the ExecStart= override is removed",
		} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal %q does not mention %q", err, want)
			}
		}
		for _, unwanted := range []string{
			`Reload the unit files with`,
			`run "hfdesk service install"`,
			`Remove the "ExecStart=" override`,
			" or ",
		} {
			if strings.Contains(err.Error(), unwanted) {
				t.Errorf("the refusal %q names %q, want the honest terminal of this state", err, unwanted)
			}
		}
	})

	t.Run("a vanished drop-in beside a readable holder names the holder", func(t *testing.T) {
		// The realistic retry of the reviewer: the operator follows
		// an earlier "Remove the override from X", deletes the FILE
		// and runs install again without a reload. The vanished path
		// is provably free of directives — there is no content that
		// could carry an override — so the readable holder's removal
		// still clears the cause. Before the fix ENOENT joined the
		// unreadable class and the terminal named no command at all
		// (the recorded reason for this expectation).
		vanished := filepath.Join(t.TempDir(), "00-vanished.conf")
		owner := writeDropIn(t, "local.conf", "[Service]\nExecStart="+strings.Join(override, " ")+"\n")

		loaded, err := loadedConfigFromProperties(driftedProperties([]string{vanished, owner}))
		if err != nil {
			t.Fatalf("decode the typed override: %v", err)
		}
		err = detectExecStartOverride(loaded, facts, ScopeSystem)
		if err == nil {
			t.Fatal("detectExecStartOverride() = nil, want the refusal over a vanished drop-in")
		}
		assertSingleRemoveOverrideAction(t, err, owner)
	})

	t.Run("a vanished drop-in with no carrier names the reload", func(t *testing.T) {
		// Nothing of the reported set can hold the override any more:
		// the loaded command only a reload reads again decides, and
		// the reload+retry is the action that clears the state — the
		// honest terminal would name an impossible removal. Before
		// the fix ENOENT reached the terminal and named no command.
		vanished := filepath.Join(t.TempDir(), "00-vanished.conf")

		loaded, err := loadedConfigFromProperties(driftedProperties([]string{vanished}))
		if err != nil {
			t.Fatalf("decode the typed override: %v", err)
		}
		err = detectExecStartOverride(loaded, facts, ScopeSystem)
		if err == nil {
			t.Fatal("detectExecStartOverride() = nil, want the refusal over a vanished drop-in")
		}
		assertSingleReloadAction(t, err, "systemctl")
	})

	t.Run("a directory in place of a drop-in is not a carrier", func(t *testing.T) {
		// A directory where a file was reported has no directive
		// content either (the read fails with EISDIR): the same
		// exclusion as a vanished file — the readable holder's
		// removal clears the cause, the terminal would not.
		dirEntry := filepath.Join(t.TempDir(), "00-directory.conf")
		if err := os.MkdirAll(dirEntry, 0o755); err != nil {
			t.Fatalf("create the directory in place of the drop-in: %v", err)
		}
		owner := writeDropIn(t, "local.conf", "[Service]\nExecStart="+strings.Join(override, " ")+"\n")

		loaded, err := loadedConfigFromProperties(driftedProperties([]string{dirEntry, owner}))
		if err != nil {
			t.Fatalf("decode the typed override: %v", err)
		}
		err = detectExecStartOverride(loaded, facts, ScopeSystem)
		if err == nil {
			t.Fatal("detectExecStartOverride() = nil, want the refusal over a directory entry")
		}
		assertSingleRemoveOverrideAction(t, err, owner)
	})

	t.Run("command with loaded flags", func(t *testing.T) {
		properties := systemdTestPropertiesFromUnit(string(content))
		properties["ExecStartEx"] = dbus.MakeVariant([]systemdExecStartExEntry{{Path: argv[0], Argv: argv, Flags: []string{"full-privileges"}}})
		loaded, err := loadedConfigFromProperties(properties)
		if err != nil {
			t.Fatalf("decode the typed command: %v", err)
		}
		err = detectExecStartOverride(loaded, facts, ScopeSystem)
		if err == nil {
			t.Fatal("detectExecStartOverride() = nil, want the refusal of a command the file does not describe")
		}
		assertSingleReloadAction(t, err, "systemctl")
	})
}

// systemdLoadedFromDiskSeam answers the typed read of the loaded
// configuration from the unit file on disk: after the reload of the fake
// supervisor that file is what systemd holds, so the loaded-command
// verification of a first installation compares the written unit against
// the command on disk — the clean state the test asserts. A test that
// models a drift overrides the seam with its own answer.
func systemdLoadedFromDiskSeam(t *testing.T, manager *systemdManager) {
	t.Helper()
	path, err := manager.unitPath()
	if err != nil {
		t.Fatalf("unitPath(): %v", err)
	}
	manager.readServiceProperties = func(_ context.Context, name string) (map[string]dbus.Variant, error) {
		if name != unitName {
			return nil, fmt.Errorf("unexpected unit %q", name)
		}
		content, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		return systemdTestPropertiesFromUnit(string(content)), nil
	}
}

func TestSystemdDetectForeignRegistration(t *testing.T) {
	unitPath := "/etc/systemd/system/hfdesk.service"

	if err := detectForeignRegistration(unitPath, systemdLoadedConfig{}, ScopeSystem); err != nil {
		t.Errorf("detectForeignRegistration() over an unloaded unit = %v, want nil", err)
	}
	ours := systemdLoadedConfig{loaded: true, fragmentPathKnown: true, fragmentPath: unitPath}
	if err := detectForeignRegistration(unitPath, ours, ScopeSystem); err != nil {
		t.Errorf("detectForeignRegistration() over our own fragment = %v, want nil", err)
	}

	t.Run("an existing foreign fragment under our name", func(t *testing.T) {
		fragment := filepath.Join(t.TempDir(), unitName)
		if err := os.WriteFile(fragment, []byte("[Unit]\nDescription=foreign\n"), 0o644); err != nil {
			t.Fatalf("write the foreign fragment: %v", err)
		}
		loaded := systemdLoadedConfig{
			loaded: true, fragmentPathKnown: true,
			fragmentPath: fragment,
		}
		err := detectForeignRegistration(unitPath, loaded, ScopeSystem)
		if err == nil {
			t.Fatal("detectForeignRegistration() = nil, want the refusal of a foreign fragment")
		}
		for _, want := range []string{fragment, unitPath} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal %q does not name %q", err, want)
			}
		}
		// The fragment file exists: the action removes it, reloads the
		// unit files (the supervisor holds the removed fragment until
		// that reload) and only then retries the installation.
		assertSingleForeignFragmentAction(t, err, unitPath, fragment)
	})

	t.Run("a foreign fragment whose file is already gone names only the reload", func(t *testing.T) {
		fragment := filepath.Join(t.TempDir(), unitName)
		loaded := systemdLoadedConfig{
			loaded: true, fragmentPathKnown: true,
			fragmentPath: fragment,
		}
		err := detectForeignRegistration(unitPath, loaded, ScopeSystem)
		if err == nil {
			t.Fatal("detectForeignRegistration() = nil, want the refusal of a stale foreign load")
		}
		// The removal the existence branch names would find no file: this
		// state needs only the reload that drops the stale load, and the
		// message must not ask for a removal that cannot be performed.
		assertSingleReloadAction(t, err, "systemctl")
		if strings.Contains(err.Error(), "remove the unit fragment") {
			t.Errorf("the refusal %q names a removal of a file that is already gone", err)
		}
	})

	t.Run("unreadable fragment names no command", func(t *testing.T) {
		loaded := systemdLoadedConfig{loaded: true}
		err := detectForeignRegistration(unitPath, loaded, ScopeSystem)
		if err == nil {
			t.Fatal("detectForeignRegistration() = nil, want the fail-closed refusal of an unproved source")
		}
		if !strings.Contains(err.Error(), "no readable fragment") {
			t.Errorf("the refusal %q does not state the unreadable fragment", err)
		}
		assertNamesNoClearingCommand(t, err)
	})

	t.Run("relative fragment names no command", func(t *testing.T) {
		loaded := systemdLoadedConfig{loaded: true, fragmentPathKnown: true, fragmentPath: "hfdesk.service"}
		err := detectForeignRegistration(unitPath, loaded, ScopeSystem)
		if err == nil {
			t.Fatal("detectForeignRegistration() = nil, want the refusal of a fragment outside our unit path")
		}
		if !strings.Contains(err.Error(), "no readable fragment") {
			t.Errorf("the refusal %q does not state the unproved fragment", err)
		}
		assertNamesNoClearingCommand(t, err)
	})
}

// assertNamesNoClearingCommand proves the honesty standard for the state of
// a refusal that no command HFDesk can name clears: the message promises no
// retry, offers no alternative and states that whatever loaded the
// registration must remove it before an installation can pass.
func assertNamesNoClearingCommand(t *testing.T, err error) {
	t.Helper()
	message := err.Error()
	for _, want := range []string{
		"the update was refused and the registration was not changed",
		"No file names where this registration comes from",
		"whatever loaded it must remove it",
		"refuses again until the supervisor no longer loads it",
	} {
		if !strings.Contains(message, want) {
			t.Errorf("the refusal %q does not contain %q", message, want)
		}
	}
	for _, unwanted := range []string{
		"hfdesk service install",
		"Move your customizations into",
		"daemon-reload",
		" or ",
	} {
		if strings.Contains(message, unwanted) {
			t.Errorf("the refusal %q names %q, want no command for a state no command clears", message, unwanted)
		}
	}
}

// A fresh manager connection occasionally gets no reply at all, and that
// connection never answers again: the read has to re-roll on a fresh
// connection with a fresh deadline instead of burning its whole budget on
// the lost one.
func TestLoadedServicePropertiesRetriesALostReply(t *testing.T) {
	t.Run("a lost reply is retried until the manager answers", func(t *testing.T) {
		m := &systemdManager{scope: ScopeUser}
		calls := 0
		var deadlines []time.Time
		m.readServiceProperties = func(ctx context.Context, name string) (map[string]dbus.Variant, error) {
			calls++
			deadline, ok := ctx.Deadline()
			if !ok {
				t.Errorf("attempt %d ran without a deadline", calls)
			} else {
				deadlines = append(deadlines, deadline)
			}
			if calls < loadedPropertiesReadAttempts {
				return nil, context.DeadlineExceeded
			}
			return map[string]dbus.Variant{"Marker": dbus.MakeVariant("read")}, nil
		}

		properties, err := m.loadedServiceProperties(context.Background(), unitName)
		if err != nil {
			t.Fatalf("loadedServiceProperties() = %v, want the recovered read", err)
		}
		if calls != loadedPropertiesReadAttempts {
			t.Errorf("the read was attempted %d times, want the bounded %d", calls, loadedPropertiesReadAttempts)
		}
		for i := 1; i < len(deadlines); i++ {
			if !deadlines[i].After(deadlines[i-1]) {
				t.Errorf("attempt %d reused the deadline of attempt %d, want a fresh deadline per attempt", i+1, i)
			}
		}
		if got := properties["Marker"].Value(); got != "read" {
			t.Errorf("the recovered properties = %v, want the manager's answer", got)
		}
	})

	t.Run("a healthy read costs one attempt", func(t *testing.T) {
		m := &systemdManager{scope: ScopeUser}
		calls := 0
		m.readServiceProperties = func(context.Context, string) (map[string]dbus.Variant, error) {
			calls++
			return map[string]dbus.Variant{"Marker": dbus.MakeVariant("read")}, nil
		}

		if _, err := m.loadedServiceProperties(context.Background(), unitName); err != nil {
			t.Fatalf("loadedServiceProperties() = %v, want nil", err)
		}
		if calls != 1 {
			t.Errorf("the healthy read was attempted %d times, want exactly one", calls)
		}
	})
}

// The retries are bounded: a read the manager never answers keeps failing
// closed with its original error after the attempts, so a persistent
// failure can never turn into silent success.
func TestLoadedServicePropertiesRefusesWhenTheReadKeepsFailing(t *testing.T) {
	m := &systemdManager{scope: ScopeUser}
	calls := 0
	m.readServiceProperties = func(context.Context, string) (map[string]dbus.Variant, error) {
		calls++
		return nil, context.DeadlineExceeded
	}

	_, err := m.loadedServiceProperties(context.Background(), unitName)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("loadedServiceProperties() = %v, want the original deadline error of the read", err)
	}
	if calls != loadedPropertiesReadAttempts {
		t.Errorf("the failing read was attempted %d times, want exactly the bounded %d", calls, loadedPropertiesReadAttempts)
	}
}

// A caller that gave up cancels its context; the read stops with that
// cancellation instead of spending further attempts.
func TestLoadedServicePropertiesDoesNotRetryAfterTheCallerGaveUp(t *testing.T) {
	m := &systemdManager{scope: ScopeUser}
	calls := 0
	m.readServiceProperties = func(ctx context.Context, _ string) (map[string]dbus.Variant, error) {
		calls++
		return nil, ctx.Err()
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	_, err := m.loadedServiceProperties(ctx, unitName)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("loadedServiceProperties() = %v, want the cancellation of the caller", err)
	}
	if calls != 1 {
		t.Errorf("the canceled read was attempted %d times, want it to stop after one", calls)
	}
}

// A semantic answer of the manager is a refusal, not a lost reply: it is
// reported after the first attempt instead of being asked again.
func TestLoadedServicePropertiesDoesNotRetryASemanticAnswer(t *testing.T) {
	m := &systemdManager{scope: ScopeUser}
	semantic := errors.New("systemd returned an invalid object path for hfdesk.service")
	calls := 0
	m.readServiceProperties = func(context.Context, string) (map[string]dbus.Variant, error) {
		calls++
		return nil, semantic
	}

	_, err := m.loadedServiceProperties(context.Background(), unitName)
	if !errors.Is(err, semantic) {
		t.Fatalf("loadedServiceProperties() = %v, want the semantic answer of the manager", err)
	}
	if calls != 1 {
		t.Errorf("the semantic answer was asked %d times, want exactly one", calls)
	}
}
