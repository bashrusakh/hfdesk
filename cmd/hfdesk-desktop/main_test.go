// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package main

import "testing"

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
