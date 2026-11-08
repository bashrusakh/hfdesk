// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"strings"
	"testing"
)

func TestParseServiceHelperCommandSeparatesHostAndHelperArguments(t *testing.T) {
	helper, handled, err := parseServiceHelperCommand([]string{
		"--service-action", "install", "--wait-pid", "4321",
		"--handover-ready-event", `Local\ready`,
		"--handover-success-event", `Local\success`,
		"--instance-mutex", `Local\HFDesk.Desktop.sid.1`,
	})
	if err != nil || !handled {
		t.Fatalf("parse helper arguments returned handled=%t, err=%v", handled, err)
	}
	if helper.Action != "install" || helper.WaitPID != 4321 || helper.ReadyEvent != `Local\ready` ||
		helper.SuccessEvent != `Local\success` || helper.InstanceMutex != `Local\HFDesk.Desktop.sid.1` {
		t.Errorf("parsed helper options = %+v", helper)
	}

	if _, handled, err := parseServiceHelperCommand([]string{"--port", "8765"}); handled || err != nil {
		t.Errorf("ordinary desktop arguments were handled as a service helper: handled=%t, err=%v", handled, err)
	}
}

func TestParseServiceHelperCommandRejectsPartialHelperArguments(t *testing.T) {
	for _, args := range [][]string{
		{"--wait-pid", "4321"},
		{"--handover-ready-event", `Local\ready`},
		{"--service-action"},
		{"--service-action", "install", "--wait-pid", "not-a-pid"},
	} {
		if _, handled, err := parseServiceHelperCommand(args); !handled || err == nil {
			t.Errorf("parseServiceHelperCommand(%q) returned handled=%t, err=%v; want a rejected helper invocation", args, handled, err)
		}
	}
}

// TestParseServiceHelperCommandRejectsUnconsumedArguments proves the helper is
// fail-closed on stray positional values: flag parsing stops at the first one,
// so a value before --wait-pid would otherwise silently drop the wait and the
// helper could run its action without the evidence the flags name. Both the
// stray value before a flag and the values after "--" are rejected by name.
func TestParseServiceHelperCommandRejectsUnconsumedArguments(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{
			args: []string{"--service-action", "install", "stray", "--wait-pid", "4321"},
			want: "stray",
		},
		{
			args: []string{
				"--service-action", "install", "--wait-pid", "4321",
				"--handover-ready-event", `Local\ready`,
				"--handover-success-event", `Local\success`,
				"--instance-mutex", `Local\m`, "stray",
			},
			want: "stray",
		},
		{
			args: []string{"--service-action", "update", "--", "--instance-mutex", `Local\m`},
			want: "--instance-mutex",
		},
	} {
		_, handled, err := parseServiceHelperCommand(tc.args)
		if !handled || err == nil {
			t.Errorf("parseServiceHelperCommand(%q) returned handled=%t, err=%v; want a rejected helper invocation",
				tc.args, handled, err)
			continue
		}
		if !strings.Contains(err.Error(), tc.want) || !strings.Contains(err.Error(), "was not consumed") {
			t.Errorf("the error %q does not name the unconsumed arguments %q", err, tc.want)
		}
	}
}

// TestParseServiceHelperCommandAcceptsTheLegitimateInvocations keeps the
// supported helper command lines green: the full install handover, the
// ordinary flagless actions and the update that carries only the singleton.
func TestParseServiceHelperCommandAcceptsTheLegitimateInvocations(t *testing.T) {
	for _, args := range [][]string{
		{"--service-action", "install", "--wait-pid", "4321",
			"--handover-ready-event", `Local\ready`,
			"--handover-success-event", `Local\success`,
			"--instance-mutex", `Local\HFDesk.Desktop.sid.1`},
		{"--service-action", "update"},
		{"--service-action", "start"},
		{"--service-action", "update", "--instance-mutex", `Local\HFDesk.Desktop.sid.1`},
	} {
		helper, handled, err := parseServiceHelperCommand(args)
		if !handled || err != nil {
			t.Errorf("parseServiceHelperCommand(%q) returned handled=%t, err=%v; want the accepted invocation",
				args, handled, err)
			continue
		}
		if helper.Action == "" {
			t.Errorf("parseServiceHelperCommand(%q) named no action", args)
		}
	}
}

// TestParseHostOptionsRejectsUnconsumedArguments proves the host path applies
// the same fail-closed rule as the helper: flag parsing stops at the first
// value that is not a flag, so every flag behind it would be silently dropped
// and the host would start with options the command line never applied. Both
// a stray value before a flag and the values after "--" are rejected by name.
func TestParseHostOptionsRejectsUnconsumedArguments(t *testing.T) {
	for _, tc := range []struct {
		args []string
		want string
	}{
		{args: []string{"--port", "8765", "stray"}, want: "stray"},
		{args: []string{"stray"}, want: "stray"},
		{args: []string{"--port", "8765", "--", "--no-open"}, want: "--no-open"},
	} {
		_, usage, err := parseHostOptions(tc.args)
		if err == nil {
			t.Errorf("parseHostOptions(%q) accepted a command line with unconsumed arguments", tc.args)
			continue
		}
		if !strings.Contains(err.Error(), "was not consumed") || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("the error %q does not name the unconsumed arguments %q", err, tc.want)
		}
		if !strings.Contains(usage, "Usage: hfdesk-desktop") {
			t.Errorf("the usage %q does not carry the usage text", usage)
		}
	}
}

// TestParseHostOptionsAcceptsTheLegitimateInvocations keeps the supported host
// command lines green: no arguments, ordinary flags, and the flag-error path
// that reports its own usage.
func TestParseHostOptionsAcceptsTheLegitimateInvocations(t *testing.T) {
	opts, _, err := parseHostOptions(nil)
	if err != nil {
		t.Fatalf("an empty command line was rejected: %v", err)
	}
	if opts.Port != 8080 {
		t.Errorf("the default port is %d, want 8080", opts.Port)
	}
	opts, _, err = parseHostOptions([]string{"--port", "9090", "--no-open"})
	if err != nil {
		t.Fatalf("an ordinary command line was rejected: %v", err)
	}
	if opts.Port != 9090 || !opts.NoOpen {
		t.Errorf("parsed options = %+v, want port 9090 and no-open", opts)
	}
	_, usage, err := parseHostOptions([]string{"--port", "not-a-number"})
	if err == nil {
		t.Fatal("a malformed flag value was accepted")
	}
	if !strings.Contains(usage, "Usage: hfdesk-desktop") {
		t.Errorf("the usage %q does not carry the usage text of the flag error", usage)
	}
}
