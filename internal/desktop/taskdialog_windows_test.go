// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"encoding/binary"
	"runtime"
	"strings"
	"testing"
	"unsafe"

	"github.com/bashrusakh/hfdesk/internal/service"
)

// taskDialogField is one field of the packed TASKDIALOGCONFIG record.
type taskDialogField struct {
	name   string
	offset int
	width  int
}

// taskDialogSDKLayout returns the sizes and TASKDIALOGCONFIG field offsets that
// CommCtrl.h declares inside <pshpack1.h> for the current architecture.
func taskDialogSDKLayout(t *testing.T) (configSize, buttonSize, pointerSize int, fields []taskDialogField) {
	t.Helper()
	switch runtime.GOARCH {
	case "amd64", "arm64":
		configSize, buttonSize, pointerSize = 160, 12, 8
		fields = []taskDialogField{
			{"cbSize", 0, 4}, {"hwndParent", 4, 8}, {"hInstance", 12, 8},
			{"dwFlags", 20, 4}, {"dwCommonButtons", 24, 4}, {"pszWindowTitle", 28, 8},
			{"mainIcon", 36, 8}, {"pszMainInstruction", 44, 8}, {"pszContent", 52, 8},
			{"cButtons", 60, 4}, {"pButtons", 64, 8}, {"nDefaultButton", 72, 4},
			{"cRadioButtons", 76, 4}, {"pRadioButtons", 80, 8}, {"nDefaultRadioButton", 88, 4},
			{"pszVerificationText", 92, 8}, {"pszExpandedInformation", 100, 8},
			{"pszExpandedControlText", 108, 8}, {"pszCollapsedControlText", 116, 8},
			{"footerIcon", 124, 8}, {"pszFooter", 132, 8}, {"pfCallback", 140, 8},
			{"lpCallbackData", 148, 8}, {"cxWidth", 156, 4},
		}
	case "386":
		configSize, buttonSize, pointerSize = 96, 8, 4
		fields = []taskDialogField{
			{"cbSize", 0, 4}, {"hwndParent", 4, 4}, {"hInstance", 8, 4},
			{"dwFlags", 12, 4}, {"dwCommonButtons", 16, 4}, {"pszWindowTitle", 20, 4},
			{"mainIcon", 24, 4}, {"pszMainInstruction", 28, 4}, {"pszContent", 32, 4},
			{"cButtons", 36, 4}, {"pButtons", 40, 4}, {"nDefaultButton", 44, 4},
			{"cRadioButtons", 48, 4}, {"pRadioButtons", 52, 4}, {"nDefaultRadioButton", 56, 4},
			{"pszVerificationText", 60, 4}, {"pszExpandedInformation", 64, 4},
			{"pszExpandedControlText", 68, 4}, {"pszCollapsedControlText", 72, 4},
			{"footerIcon", 76, 4}, {"pszFooter", 80, 4}, {"pfCallback", 84, 4},
			{"lpCallbackData", 88, 4}, {"cxWidth", 92, 4},
		}
	default:
		t.Skipf("the task dialog layout is not asserted for windows/%s", runtime.GOARCH)
	}
	if got := int(unsafe.Sizeof(uintptr(0))); got != pointerSize {
		t.Fatalf("the pointer size is %d, the SDK layout for windows/%s is %d", got, runtime.GOARCH, pointerSize)
	}
	return configSize, buttonSize, pointerSize, fields
}

// TestTaskDialogConfigRecordMatchesTheWindowsSDK proves that the TASKDIALOGCONFIG
// record has the packed SDK layout: cbSize is followed by hwndParent at byte 4 and
// the offsets are checked against the bytes production writes.
func TestTaskDialogConfigRecordMatchesTheWindowsSDK(t *testing.T) {
	configSize, _, pointerSize, fields := taskDialogSDKLayout(t)

	var labels [8]uint16
	config := taskDialogConfig{
		hwndParent:              uintptr(0x10000001),
		hInstance:               uintptr(0x10000002),
		dwFlags:                 0x20000001,
		dwCommonButtons:         0x20000002,
		pszWindowTitle:          &labels[0],
		mainIcon:                uintptr(0x10000003),
		pszMainInstruction:      &labels[1],
		pszContent:              &labels[2],
		nDefaultButton:          0x20000003,
		cRadioButtons:           0x20000004,
		pRadioButtons:           uintptr(0x10000004),
		nDefaultRadioButton:     0x20000005,
		pszVerificationText:     &labels[3],
		pszExpandedInformation:  &labels[4],
		pszExpandedControlText:  &labels[5],
		pszCollapsedControlText: &labels[6],
		footerIcon:              uintptr(0x10000005),
		pszFooter:               &labels[7],
		pfCallback:              uintptr(0x10000006),
		lpCallbackData:          ^uintptr(0),
		cxWidth:                 0x20000006,
	}
	buttons := []taskDialogButton{
		{id: serviceGateOpenButtonID, text: &labels[0]},
		{id: serviceGateUpdateButtonID, text: &labels[1]},
	}
	configRecord, buttonRecord := marshalTaskDialogConfig(config, buttons)

	if got := len(configRecord); got != configSize {
		t.Fatalf("the TASKDIALOGCONFIG record is %d bytes, the SDK size is %d", got, configSize)
	}

	ptr := func(value unsafe.Pointer) uint64 { return uint64(uintptr(value)) }
	want := []uint64{
		uint64(configSize), uint64(config.hwndParent), uint64(config.hInstance),
		uint64(config.dwFlags), uint64(config.dwCommonButtons), ptr(unsafe.Pointer(config.pszWindowTitle)),
		uint64(config.mainIcon), ptr(unsafe.Pointer(config.pszMainInstruction)), ptr(unsafe.Pointer(config.pszContent)),
		uint64(len(buttons)), ptr(unsafe.Pointer(&buttonRecord[0])), uint64(uint32(config.nDefaultButton)),
		uint64(config.cRadioButtons), uint64(config.pRadioButtons), uint64(uint32(config.nDefaultRadioButton)),
		ptr(unsafe.Pointer(config.pszVerificationText)), ptr(unsafe.Pointer(config.pszExpandedInformation)),
		ptr(unsafe.Pointer(config.pszExpandedControlText)), ptr(unsafe.Pointer(config.pszCollapsedControlText)),
		uint64(config.footerIcon), ptr(unsafe.Pointer(config.pszFooter)), uint64(config.pfCallback),
		uint64(config.lpCallbackData), uint64(config.cxWidth),
	}
	if len(want) != len(fields) {
		t.Fatalf("the test states %d expected values for %d SDK fields", len(want), len(fields))
	}
	for i, field := range fields {
		if field.width != 4 && field.width != pointerSize {
			t.Fatalf("the field %s has width %d, the pointer size is %d", field.name, field.width, pointerSize)
		}
		if field.offset < 0 || field.offset+field.width > len(configRecord) {
			t.Fatalf("the field %s at offset %d with width %d is outside the %d-byte record",
				field.name, field.offset, field.width, len(configRecord))
		}
		var got uint64
		if field.width == 4 {
			got = uint64(binary.LittleEndian.Uint32(configRecord[field.offset:]))
		} else {
			got = binary.LittleEndian.Uint64(configRecord[field.offset:])
		}
		if got != want[i] {
			t.Errorf("the %s field at offset %d is 0x%x, the SDK layout expects 0x%x",
				field.name, field.offset, got, want[i])
		}
	}
}

// TestTaskDialogButtonRecordMatchesTheWindowsSDK proves the packed TASKDIALOG_BUTTON
// layout: a 12-byte record with id at byte 0 on 64-bit Windows, 8 bytes on 386.
func TestTaskDialogButtonRecordMatchesTheWindowsSDK(t *testing.T) {
	_, buttonSize, pointerSize, _ := taskDialogSDKLayout(t)

	var labels [2]uint16
	buttons := []taskDialogButton{
		{id: serviceGateOpenButtonID, text: &labels[0]},
		{id: serviceGateUpdateButtonID, text: &labels[1]},
	}
	record := marshalTaskDialogButtons(buttons)

	if got, want := len(record), len(buttons)*buttonSize; got != want {
		t.Fatalf("the TASKDIALOG_BUTTON array is %d bytes, the SDK size is %d", got, want)
	}
	for i, button := range buttons {
		offset := i * buttonSize
		if got, want := int32(binary.LittleEndian.Uint32(record[offset:])), button.id; got != want {
			t.Errorf("the button %d has the id %d, want %d", i, got, want)
		}
		var got uintptr
		if pointerSize == 4 {
			got = uintptr(binary.LittleEndian.Uint32(record[offset+4:]))
		} else {
			got = uintptr(binary.LittleEndian.Uint64(record[offset+4:]))
		}
		if want := uintptr(unsafe.Pointer(button.text)); got != want {
			t.Errorf("the label of button %d is at 0x%x, want 0x%x", i, got, want)
		}
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
		name    string
		state   service.State
		running bool
		label   string
		want    serviceChoice
	}{
		{name: "running", state: service.StateRunning, label: "Open HFDesk", want: serviceChoiceOpen},
		{name: "starting", state: service.StateStarting, label: "Open HFDesk", want: serviceChoiceOpen},
		{name: "stopped", state: service.StateStopped, label: "Start service", want: serviceChoiceStart},
		{name: "failed", state: service.StateFailed, label: "Start service", want: serviceChoiceStart},
		{name: "broken", state: service.StateBroken, label: "Start service", want: serviceChoiceStart},
		// The registration is broken but the instance it started still
		// runs: the state alone would offer the start of a service that
		// already serves.
		{name: "broken but running", state: service.StateBroken, running: true, label: "Open HFDesk", want: serviceChoiceOpen},
	} {
		t.Run(tc.name, func(t *testing.T) {
			prompt := serviceChoicePrompt{State: tc.state, Running: tc.running}
			if got := serviceGateOpenLabel(prompt); got != tc.label {
				t.Errorf("the first button of the %s service is labelled %q, want %q", tc.name, got, tc.label)
			}

			choice, err := serviceGateChoiceForButton(prompt, serviceGateOpenButtonID)
			if err != nil {
				t.Fatalf("the first button of the %s service returned an error: %v", tc.name, err)
			}
			if choice != tc.want {
				t.Errorf("the first button of the %s service performs %v, want %v", tc.name, choice, tc.want)
			}
		})
	}
}

// TestServiceGateChoiceForButtonMapsTheOtherIds proves that the update button
// maps onto the update and that every way of leaving the dialog changes
// nothing.
func TestServiceGateChoiceForButtonMapsTheOtherIds(t *testing.T) {
	if choice, err := serviceGateChoiceForButton(serviceChoicePrompt{State: service.StateRunning}, serviceGateUpdateButtonID); err != nil || choice != serviceChoiceUpdate {
		t.Errorf("the update button returned (%v, %v), want the update", choice, err)
	}

	// Both statuses matter for the cancel ids as well: the answer may never
	// depend on the state when the user did not ask for an action.
	for _, prompt := range []serviceChoicePrompt{
		{State: service.StateRunning},
		{State: service.StateStopped},
		{State: service.StateBroken, Running: true},
	} {
		for _, pressed := range []int32{serviceGateCancelButtonID, messageBoxCancel, 0} {
			choice, err := serviceGateChoiceForButton(prompt, pressed)
			if err != nil {
				t.Errorf("the cancel id %d of the %s service returned an error: %v", pressed, prompt.State, err)
			}
			if choice != serviceChoiceCancel {
				t.Errorf("the cancel id %d of the %s service performs %v, want no action", pressed, prompt.State, choice)
			}
		}
	}

	// An id the dialog never sends is reported instead of guessed: a button
	// the host does not know must not start or open anything.
	choice, err := serviceGateChoiceForButton(serviceChoicePrompt{State: service.StateStopped}, 999)
	if err == nil {
		t.Error("the unknown button returned no error")
	} else if !strings.Contains(err.Error(), "999") {
		t.Errorf("the error of the unknown button is %q, want it to name the id", err)
	}
	if choice != serviceChoiceCancel {
		t.Errorf("the unknown button performs %v, want no action", choice)
	}
}

// TestServiceGateButtonLayoutPerState proves which buttons each state gets, in
// which order, which one Enter selects and what each id performs: the label,
// the position and the action of a button are three renderings of one
// decision, so a test reads them side by side.
//
// The positively confirmed stopped service is the only dialog with the desktop
// mode, and it is its first button and its default — every other state keeps
// the open/start button first and as its default.
func TestServiceGateButtonLayoutPerState(t *testing.T) {
	// The state reads stopped but the supervisor reports a serving
	// instance: the running fact wins, so the dialog is the running one.
	stoppedButRunning := serviceChoicePrompt{State: service.StateStopped, Running: true}

	for _, tc := range []struct {
		name        string
		prompt      serviceChoicePrompt
		wantLabels  []string
		wantIDs     []int32
		wantDefault int32
		wantChoices []serviceChoice
	}{
		{
			name:        "stopped",
			prompt:      serviceChoicePrompt{State: service.StateStopped},
			wantLabels:  []string{"Work in desktop mode", "Start service", "Update service", "Cancel"},
			wantIDs:     []int32{serviceGateDesktopButtonID, serviceGateOpenButtonID, serviceGateUpdateButtonID, serviceGateCancelButtonID},
			wantDefault: serviceGateDesktopButtonID,
			wantChoices: []serviceChoice{serviceChoiceDesktop, serviceChoiceStart, serviceChoiceUpdate, serviceChoiceCancel},
		},
		{
			name:        "running",
			prompt:      serviceChoicePrompt{State: service.StateRunning},
			wantLabels:  []string{"Open HFDesk", "Update service", "Cancel"},
			wantIDs:     []int32{serviceGateOpenButtonID, serviceGateUpdateButtonID, serviceGateCancelButtonID},
			wantDefault: serviceGateOpenButtonID,
			wantChoices: []serviceChoice{serviceChoiceOpen, serviceChoiceUpdate, serviceChoiceCancel},
		},
		{
			name:        "failed",
			prompt:      serviceChoicePrompt{State: service.StateFailed},
			wantLabels:  []string{"Start service", "Update service", "Cancel"},
			wantIDs:     []int32{serviceGateOpenButtonID, serviceGateUpdateButtonID, serviceGateCancelButtonID},
			wantDefault: serviceGateOpenButtonID,
			wantChoices: []serviceChoice{serviceChoiceStart, serviceChoiceUpdate, serviceChoiceCancel},
		},
		{
			name:        "broken without an instance",
			prompt:      serviceChoicePrompt{State: service.StateBroken},
			wantLabels:  []string{"Start service", "Update service", "Cancel"},
			wantIDs:     []int32{serviceGateOpenButtonID, serviceGateUpdateButtonID, serviceGateCancelButtonID},
			wantDefault: serviceGateOpenButtonID,
			wantChoices: []serviceChoice{serviceChoiceStart, serviceChoiceUpdate, serviceChoiceCancel},
		},
		{
			name:        "broken but running",
			prompt:      serviceChoicePrompt{State: service.StateBroken, Running: true},
			wantLabels:  []string{"Open HFDesk", "Update service", "Cancel"},
			wantIDs:     []int32{serviceGateOpenButtonID, serviceGateUpdateButtonID, serviceGateCancelButtonID},
			wantDefault: serviceGateOpenButtonID,
			wantChoices: []serviceChoice{serviceChoiceOpen, serviceChoiceUpdate, serviceChoiceCancel},
		},
		{
			name:        "stopped with the running fact",
			prompt:      stoppedButRunning,
			wantLabels:  []string{"Open HFDesk", "Update service", "Cancel"},
			wantIDs:     []int32{serviceGateOpenButtonID, serviceGateUpdateButtonID, serviceGateCancelButtonID},
			wantDefault: serviceGateOpenButtonID,
			wantChoices: []serviceChoice{serviceChoiceOpen, serviceChoiceUpdate, serviceChoiceCancel},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			buttons, defaultButton := serviceGateButtonLayout(tc.prompt)
			if defaultButton != tc.wantDefault {
				t.Errorf("the default button is %d, want %d: Enter must select %q",
					defaultButton, tc.wantDefault, tc.wantLabels[0])
			}
			if len(buttons) != len(tc.wantLabels) {
				t.Fatalf("the dialog has %d buttons %v, want %d %v",
					len(buttons), buttonLabels(buttons), len(tc.wantLabels), tc.wantLabels)
			}
			for i, button := range buttons {
				if button.label != tc.wantLabels[i] {
					t.Errorf("the button %d is labelled %q, want %q", i, button.label, tc.wantLabels[i])
				}
				if button.id != tc.wantIDs[i] {
					t.Errorf("the button %d has the id %d, want %d", i, button.id, tc.wantIDs[i])
				}
				choice, err := serviceGateChoiceForButton(tc.prompt, button.id)
				if err != nil {
					t.Fatalf("the button %d returned an error: %v", i, err)
				}
				if choice != tc.wantChoices[i] {
					t.Errorf("the button %d performs %v, want %v", i, choice, tc.wantChoices[i])
				}
			}
		})
	}
}

// buttonLabels returns the labels of the buttons, for a failure message.
func buttonLabels(buttons []serviceGateButton) []string {
	labels := make([]string, 0, len(buttons))
	for _, button := range buttons {
		labels = append(labels, button.label)
	}
	return labels
}

// TestServiceGateMessageBoxFallbackMapsYesNoCancel proves the mapping of the
// message box fallback of the service gate: which box each answer opens and
// which action every answer performs.
//
// A positively confirmed stopped service is asked in two boxes — the first
// one picks the desktop mode, a service action or no action, and only a No
// opens the second one that names the service actions — while every other
// state keeps its single box and the mapping it had. The desktop answer is
// the one that continues the launch and never the start action, so the box
// that offers it can never select the start of the service.
//
// The real message box blocks a process without an interactive window station,
// so the seam is replaced with the scripted answers and records the text of
// every box it is asked to show.
func TestServiceGateMessageBoxFallbackMapsYesNoCancel(t *testing.T) {
	stopped := serviceChoicePrompt{State: service.StateStopped}
	running := serviceChoicePrompt{State: service.StateRunning}
	failed := serviceChoicePrompt{State: service.StateFailed}

	for _, tc := range []struct {
		name    string
		prompt  serviceChoicePrompt
		answers []int32
		want    serviceChoice
	}{
		{name: "stopped yes continues in desktop mode", prompt: stopped,
			answers: []int32{messageBoxYes}, want: serviceChoiceDesktop},
		{name: "stopped no asks for the service action", prompt: stopped,
			answers: []int32{messageBoxNo, messageBoxYes}, want: serviceChoiceStart},
		{name: "stopped no then no updates", prompt: stopped,
			answers: []int32{messageBoxNo, messageBoxNo}, want: serviceChoiceUpdate},
		{name: "stopped cancel changes nothing", prompt: stopped,
			answers: []int32{messageBoxCancel}, want: serviceChoiceCancel},
		{name: "running yes opens", prompt: running,
			answers: []int32{messageBoxYes}, want: serviceChoiceOpen},
		{name: "running no updates", prompt: running,
			answers: []int32{messageBoxNo}, want: serviceChoiceUpdate},
		{name: "failed yes starts", prompt: failed,
			answers: []int32{messageBoxYes}, want: serviceChoiceStart},
		{name: "cancel changes nothing", prompt: running,
			answers: []int32{messageBoxCancel}, want: serviceChoiceCancel},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var asked []string
			replaceSeam(t, &showGateMessageBox, func(text string, _ uint32) int32 {
				if len(asked) >= len(tc.answers) {
					t.Errorf("the fallback asked box %d, only %d answers scripted", len(asked)+1, len(tc.answers))
					return messageBoxCancel
				}
				asked = append(asked, text)
				return tc.answers[len(asked)-1]
			})

			if choice := messageBoxChoice(tc.prompt); choice != tc.want {
				t.Errorf("the fallback answer %v performs %v, want %v", tc.answers, choice, tc.want)
			}

			if len(asked) != len(tc.answers) {
				t.Fatalf("the fallback asked %d boxes, want %d: texts %q", len(asked), len(tc.answers), asked)
			}
			for i, text := range asked {
				// Only the stopped service gets a second box, and it is
				// the box that names the service actions.
				want := serviceGateChooseText(tc.prompt)
				if servicePromptStopped(tc.prompt) && i > 0 {
					want = serviceGateServiceActionChooseText(tc.prompt)
				}
				if text != want {
					t.Errorf("box %d text = %q, want %q", i+1, text, want)
				}
			}
		})
	}
}
