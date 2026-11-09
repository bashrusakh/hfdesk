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
	unitPath := "/etc/systemd/system/hfdesk.service"
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
	if err := detectExecStartOverride(unitPath, matching, facts); err != nil {
		t.Errorf("detectExecStartOverride() over the matching command = %v, want nil", err)
	}
	if err := detectExecStartOverride(unitPath, systemdLoadedConfig{}, facts); err != nil {
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

	t.Run("drop-in owns the effective command", func(t *testing.T) {
		loaded, err := loadedConfigFromProperties(driftedProperties([]string{"/etc/systemd/system/hfdesk.service.d/local.conf"}))
		if err != nil {
			t.Fatalf("decode the typed override: %v", err)
		}
		err = detectExecStartOverride(unitPath, loaded, facts)
		if err == nil {
			t.Fatal("detectExecStartOverride() = nil, want the refusal of an overridden command")
		}
		if !strings.Contains(err.Error(), `drop-ins: "/etc/systemd/system/hfdesk.service.d/local.conf"`) {
			t.Errorf("the refusal %q does not name the DropInPaths the supervisor reported", err)
		}
		assertSingleDropInAction(t, err, unitPath)
	})

	t.Run("drift without a reported drop-in", func(t *testing.T) {
		loaded, err := loadedConfigFromProperties(driftedProperties([]string{}))
		if err != nil {
			t.Fatalf("decode the typed override: %v", err)
		}
		err = detectExecStartOverride(unitPath, loaded, facts)
		if err == nil {
			t.Fatal("detectExecStartOverride() = nil, want the fail-closed refusal of a drifted command")
		}
		if strings.Contains(err.Error(), "(drop-ins:") {
			t.Errorf("the refusal %q names drop-ins the supervisor did not report", err)
		}
		assertSingleDropInAction(t, err, unitPath)
	})

	t.Run("command with loaded flags", func(t *testing.T) {
		properties := systemdTestPropertiesFromUnit(string(content))
		properties["ExecStartEx"] = dbus.MakeVariant([]systemdExecStartExEntry{{Path: argv[0], Argv: argv, Flags: []string{"full-privileges"}}})
		loaded, err := loadedConfigFromProperties(properties)
		if err != nil {
			t.Fatalf("decode the typed command: %v", err)
		}
		err = detectExecStartOverride(unitPath, loaded, facts)
		if err == nil {
			t.Fatal("detectExecStartOverride() = nil, want the refusal of a command the file does not describe")
		}
		assertSingleDropInAction(t, err, unitPath)
	})
}

func TestSystemdDetectForeignRegistration(t *testing.T) {
	unitPath := "/etc/systemd/system/hfdesk.service"

	if err := detectForeignRegistration(unitPath, systemdLoadedConfig{}); err != nil {
		t.Errorf("detectForeignRegistration() over an unloaded unit = %v, want nil", err)
	}
	ours := systemdLoadedConfig{loaded: true, fragmentPathKnown: true, fragmentPath: unitPath}
	if err := detectForeignRegistration(unitPath, ours); err != nil {
		t.Errorf("detectForeignRegistration() over our own fragment = %v, want nil", err)
	}

	t.Run("vendor fragment under our name", func(t *testing.T) {
		loaded := systemdLoadedConfig{
			loaded: true, fragmentPathKnown: true,
			fragmentPath: "/usr/lib/systemd/system/hfdesk.service",
		}
		err := detectForeignRegistration(unitPath, loaded)
		if err == nil {
			t.Fatal("detectForeignRegistration() = nil, want the refusal of a foreign fragment")
		}
		for _, want := range []string{"/usr/lib/systemd/system/hfdesk.service", unitPath} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the refusal %q does not name %q", err, want)
			}
		}
		assertSingleDropInAction(t, err, unitPath)
	})

	t.Run("unreadable fragment", func(t *testing.T) {
		loaded := systemdLoadedConfig{loaded: true}
		err := detectForeignRegistration(unitPath, loaded)
		if err == nil {
			t.Fatal("detectForeignRegistration() = nil, want the fail-closed refusal of an unproved source")
		}
		if !strings.Contains(err.Error(), "no readable fragment") {
			t.Errorf("the refusal %q does not state the unreadable fragment", err)
		}
		assertSingleDropInAction(t, err, unitPath)
	})

	t.Run("relative fragment", func(t *testing.T) {
		loaded := systemdLoadedConfig{loaded: true, fragmentPathKnown: true, fragmentPath: "hfdesk.service"}
		err := detectForeignRegistration(unitPath, loaded)
		if err == nil {
			t.Fatal("detectForeignRegistration() = nil, want the refusal of a fragment outside our unit path")
		}
		if !strings.Contains(err.Error(), "no readable fragment") {
			t.Errorf("the refusal %q does not state the unproved fragment", err)
		}
		assertSingleDropInAction(t, err, unitPath)
	})
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
