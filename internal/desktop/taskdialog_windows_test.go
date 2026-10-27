// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"strings"
	"testing"
	"unsafe"

	"github.com/bashrusakh/hfdesk/internal/service"
)

// TestTaskDialogConfigLayoutMatchesTheWindowsAPI proves that the configuration
// the host hands to the task dialog has the layout of the 64-bit Windows API.
//
// The dialog reads a pointer out of every field of the struct: a field at the
// wrong offset makes it read a label pointer out of a flag and the process
// crashes instead of showing the choice. The layout is not something the
// compiler checks, because the struct only exists in Go, so it is checked here:
// a wrong 64-bit layout fails in CI instead of failing on the machine of a user.
//
// The expected offsets are the offsets of the C declaration, where only the
// eight-byte members are aligned. The struct declares no padding of its own:
// Go's own alignment already inserts the same padding, and a hand-written
// padding field would shift every following member by four bytes.
func TestTaskDialogConfigLayoutMatchesTheWindowsAPI(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("the task dialog layout is the layout of the 64-bit Windows API")
	}

	config := taskDialogConfig{}
	fields := []struct {
		name   string
		offset uintptr
		want   uintptr
	}{
		{"cbSize", unsafe.Offsetof(config.cbSize), 0},
		{"hwndParent", unsafe.Offsetof(config.hwndParent), 8},
		{"hInstance", unsafe.Offsetof(config.hInstance), 16},
		{"dwFlags", unsafe.Offsetof(config.dwFlags), 24},
		{"dwCommonButtons", unsafe.Offsetof(config.dwCommonButtons), 28},
		{"pszWindowTitle", unsafe.Offsetof(config.pszWindowTitle), 32},
		{"mainIcon", unsafe.Offsetof(config.mainIcon), 40},
		{"pszMainInstruction", unsafe.Offsetof(config.pszMainInstruction), 48},
		{"pszContent", unsafe.Offsetof(config.pszContent), 56},
		{"cButtons", unsafe.Offsetof(config.cButtons), 64},
		{"pButtons", unsafe.Offsetof(config.pButtons), 72},
		{"nDefaultButton", unsafe.Offsetof(config.nDefaultButton), 80},
		{"cRadioButtons", unsafe.Offsetof(config.cRadioButtons), 84},
		{"pRadioButtons", unsafe.Offsetof(config.pRadioButtons), 88},
		{"nDefaultRadioButton", unsafe.Offsetof(config.nDefaultRadioButton), 96},
		{"pszVerificationText", unsafe.Offsetof(config.pszVerificationText), 104},
		{"pszExpandedInformation", unsafe.Offsetof(config.pszExpandedInformation), 112},
		{"pszExpandedControlText", unsafe.Offsetof(config.pszExpandedControlText), 120},
		{"pszCollapsedControlText", unsafe.Offsetof(config.pszCollapsedControlText), 128},
		{"footerIcon", unsafe.Offsetof(config.footerIcon), 136},
		{"pszFooter", unsafe.Offsetof(config.pszFooter), 144},
		{"pfCallback", unsafe.Offsetof(config.pfCallback), 152},
		{"lpCallbackData", unsafe.Offsetof(config.lpCallbackData), 160},
		{"cxWidth", unsafe.Offsetof(config.cxWidth), 168},
	}
	for _, field := range fields {
		if field.offset != field.want {
			t.Errorf("the offset of %s is %d, want %d", field.name, field.offset, field.want)
		}
	}
	if size, want := unsafe.Sizeof(taskDialogConfig{}), uintptr(176); size != want {
		t.Errorf("the size of the task dialog configuration is %d, want %d", size, want)
	}

	button := taskDialogButton{}
	if offset, want := unsafe.Offsetof(button.text), uintptr(8); offset != want {
		t.Errorf("the offset of the button label is %d, want %d", offset, want)
	}
	if size, want := unsafe.Sizeof(taskDialogButton{}), uintptr(16); size != want {
		t.Errorf("the size of a task dialog button is %d, want %d", size, want)
	}
}

// TestShellExecuteInfoLayoutMatchesTheWindowsAPI proves that the struct the host
// hands to the shell has the layout of the 64-bit Windows API.
//
// The shell reads the verb, the program and the arguments out of the struct, so
// a field at the wrong offset makes it start a program the user did not ask for.
func TestShellExecuteInfoLayoutMatchesTheWindowsAPI(t *testing.T) {
	if unsafe.Sizeof(uintptr(0)) != 8 {
		t.Skip("the shell layout is the layout of the 64-bit Windows API")
	}

	info := shellExecuteInfo{}
	fields := []struct {
		name   string
		offset uintptr
		want   uintptr
	}{
		{"cbSize", unsafe.Offsetof(info.cbSize), 0},
		{"fMask", unsafe.Offsetof(info.fMask), 4},
		{"hwnd", unsafe.Offsetof(info.hwnd), 8},
		{"lpVerb", unsafe.Offsetof(info.lpVerb), 16},
		{"lpFile", unsafe.Offsetof(info.lpFile), 24},
		{"lpParameters", unsafe.Offsetof(info.lpParameters), 32},
		{"lpDirectory", unsafe.Offsetof(info.lpDirectory), 40},
		{"nShow", unsafe.Offsetof(info.nShow), 48},
		{"hInstApp", unsafe.Offsetof(info.hInstApp), 56},
		{"lpIDList", unsafe.Offsetof(info.lpIDList), 64},
		{"lpClass", unsafe.Offsetof(info.lpClass), 72},
		{"hkeyClass", unsafe.Offsetof(info.hkeyClass), 80},
		{"dwHotKey", unsafe.Offsetof(info.dwHotKey), 88},
		{"hIcon", unsafe.Offsetof(info.hIcon), 96},
		{"hProcess", unsafe.Offsetof(info.hProcess), 104},
	}
	for _, field := range fields {
		if field.offset != field.want {
			t.Errorf("the offset of %s is %d, want %d", field.name, field.offset, field.want)
		}
	}
	if size, want := unsafe.Sizeof(shellExecuteInfo{}), uintptr(112); size != want {
		t.Errorf("the size of the shell execute info is %d, want %d", size, want)
	}
}

// TestServiceGateChoiceForButtonMapsTheFirstButtonOntoItsLabel proves that the
// first button of the task dialog performs the action its label names.
//
// The button id is the same for both states, so a dialog that mapped the id
// unconditionally would open the browser under a "Start service" label. The
// mapping and the label are checked side by side, so a change of one without
// the other fails here.
func TestServiceGateChoiceForButtonMapsTheFirstButtonOntoItsLabel(t *testing.T) {
	for _, tc := range []struct {
		name  string
		state service.State
		label string
		want  serviceChoice
	}{
		{name: "running", state: service.StateRunning, label: "Open HFDesk", want: serviceChoiceOpen},
		{name: "starting", state: service.StateStarting, label: "Open HFDesk", want: serviceChoiceOpen},
		{name: "stopped", state: service.StateStopped, label: "Start service", want: serviceChoiceStart},
		{name: "failed", state: service.StateFailed, label: "Start service", want: serviceChoiceStart},
		{name: "broken", state: service.StateBroken, label: "Start service", want: serviceChoiceStart},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prompt := serviceChoicePrompt{State: tc.state}
			if got := serviceGateOpenLabel(prompt); got != tc.label {
				t.Errorf("the first button of the %s service is labelled %q, want %q", tc.state, got, tc.label)
			}

			choice, err := serviceGateChoiceForButton(tc.state, serviceGateOpenButtonID)
			if err != nil {
				t.Fatalf("the first button of the %s service returned an error: %v", tc.state, err)
			}
			if choice != tc.want {
				t.Errorf("the first button of the %s service performs %v, want %v", tc.state, choice, tc.want)
			}
		})
	}
}

// TestServiceGateChoiceForButtonMapsTheOtherIds proves that the update button
// maps onto the update and that every way of leaving the dialog changes
// nothing.
func TestServiceGateChoiceForButtonMapsTheOtherIds(t *testing.T) {
	if choice, err := serviceGateChoiceForButton(service.StateRunning, serviceGateUpdateButtonID); err != nil || choice != serviceChoiceUpdate {
		t.Errorf("the update button returned (%v, %v), want the update", choice, err)
	}

	// Both states matter for the cancel ids as well: the answer may never
	// depend on the state when the user did not ask for an action.
	for _, state := range []service.State{service.StateRunning, service.StateStopped} {
		for _, pressed := range []int32{serviceGateCancelButtonID, messageBoxCancel, 0} {
			choice, err := serviceGateChoiceForButton(state, pressed)
			if err != nil {
				t.Errorf("the cancel id %d of the %s service returned an error: %v", pressed, state, err)
			}
			if choice != serviceChoiceCancel {
				t.Errorf("the cancel id %d of the %s service performs %v, want no action", pressed, state, choice)
			}
		}
	}

	// An id the dialog never sends is reported instead of guessed: a button
	// the host does not know must not start or open anything.
	choice, err := serviceGateChoiceForButton(service.StateStopped, 999)
	if err == nil {
		t.Error("the unknown button returned no error")
	} else if !strings.Contains(err.Error(), "999") {
		t.Errorf("the error of the unknown button is %q, want it to name the id", err)
	}
	if choice != serviceChoiceCancel {
		t.Errorf("the unknown button performs %v, want no action", choice)
	}
}
