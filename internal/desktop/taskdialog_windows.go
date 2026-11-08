// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"encoding/binary"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The identifiers of the buttons of the service gate dialog.
//
// The task dialog reports the id of the pressed button back, so the answer of
// the user is a value of the dialog and not a position in a list. The ids start
// above the ids of the standard buttons, so an answer of the message box
// fallback can never be read as a button of the dialog.
const (
	serviceGateOpenButtonID   = 101
	serviceGateUpdateButtonID = 102
	serviceGateCancelButtonID = 103
	// serviceGateDesktopButtonID is the button that continues the launch
	// into desktop mode. Only the dialog of a positively confirmed stopped
	// service has it, and it is that dialog's default button, so Enter
	// selects it.
	serviceGateDesktopButtonID = 104
)

// serviceGateButton is one button of the choice dialog: the command id the
// dialog reports back and the label the user reads.
type serviceGateButton struct {
	id    int32
	label string
}

// serviceGateButtonLayout returns the buttons of the choice dialog in display
// order and the id of the default button, for the state of the prompt.
//
// The positively confirmed stopped service is the only dialog with the desktop
// mode. It comes first and is the default, so Enter continues this launch into
// desktop mode — the answer that changes nothing on the machine. Every other
// dialog keeps the open/start button first and as its default: the action the
// user asked for by starting HFDesk again.
//
// The layout is a pure function, so a test can prove the buttons, their order
// and the default of every state without a dialog: the real dialog blocks the
// process until the user answers it.
func serviceGateButtonLayout(prompt serviceChoicePrompt) (buttons []serviceGateButton, defaultButton int32) {
	first := serviceGateButton{id: serviceGateOpenButtonID, label: serviceGateOpenLabel(prompt)}
	if servicePromptStopped(prompt) {
		return []serviceGateButton{
			{id: serviceGateDesktopButtonID, label: serviceGateDesktopLabel},
			first,
			{id: serviceGateUpdateButtonID, label: serviceGateUpdateLabel},
			{id: serviceGateCancelButtonID, label: serviceGateCancelLabel},
		}, serviceGateDesktopButtonID
	}
	return []serviceGateButton{
		first,
		{id: serviceGateUpdateButtonID, label: serviceGateUpdateLabel},
		{id: serviceGateCancelButtonID, label: serviceGateCancelLabel},
	}, first.id
}

// The task dialog flags of the Windows API.
//
// Only the cancellation flag is used: it closes the dialog on Esc and on the
// close button, so the user can always leave without an action. No flag of the
// dialog changes the rendering or the size of the dialog.
const (
	tdfAllowDialogCancellation = 0x0008
)

// showServiceChoice shows the choice dialog of the service gate.
//
// It is a package level variable so the tests can answer the dialog: the real
// dialog blocks the process until the user answers it, which an agent without
// an interactive window station can never do.
var showServiceChoice = showTaskDialogChoice

// showTaskDialogChoice shows the service gate with the buttons that name the
// actions, and falls back to a message box when the task dialog is not
// available.
//
// The task dialog needs the version 6 of the common controls, which the binary
// declares in its manifest. A binary that was built without the resource of the
// package loads the older library, which has no entry point of the dialog, and
// a call can fail for a reason of its own. The user is never left without a
// dialog: the message box of the fallback asks the same question and maps Yes,
// No and Cancel onto the actions in its text.
func showTaskDialogChoice(prompt serviceChoicePrompt) serviceChoice {
	choice, err := taskDialogChoice(prompt)
	if err == nil {
		return choice
	}
	desktopLog.Warn("the service choice is asked in a message box", "error", err)
	return messageBoxChoice(prompt)
}

// taskDialogIndirect is the entry point of the task dialog of the common
// controls of Windows.
var taskDialogIndirect = windows.NewLazySystemDLL("comctl32.dll").NewProc("TaskDialogIndirect")

// TASKDIALOG_BUTTON and TASKDIALOGCONFIG are declared between pshpack1.h and
// poppack.h in CommCtrl.h, so no member is padded and a Go struct cannot mirror
// them; both records are marshalled into packed byte buffers instead. The sizes
// follow from eight 4-byte and sixteen pointer-size members; a test pins the
// offsets.
const (
	taskDialogPointerSize = int(unsafe.Sizeof(uintptr(0)))
	taskDialogConfigSize  = 8*4 + 16*taskDialogPointerSize
	taskDialogButtonSize  = 4 + taskDialogPointerSize
)

// packedRecord appends a value in the little-endian order and the width of the
// SDK member.
type packedRecord []byte

func (r *packedRecord) u32(v uint32) {
	*r = binary.LittleEndian.AppendUint32(*r, v)
}

func (r *packedRecord) i32(v int32) {
	r.u32(uint32(v))
}

func (r *packedRecord) uptr(v uintptr) {
	if taskDialogPointerSize == 4 {
		r.u32(uint32(v))
		return
	}
	*r = binary.LittleEndian.AppendUint64(*r, uint64(v))
}

type taskDialogButton struct {
	// id is the command id the dialog reports for the button.
	id int32
	// text is the label of the button.
	text *uint16
}

// taskDialogConfig holds the values of a TASKDIALOGCONFIG record; cbSize,
// cButtons and pButtons are derived by marshalTaskDialogConfig.
type taskDialogConfig struct {
	hwndParent              uintptr
	hInstance               uintptr
	dwFlags                 uint32
	dwCommonButtons         uint32
	pszWindowTitle          *uint16
	mainIcon                uintptr
	pszMainInstruction      *uint16
	pszContent              *uint16
	nDefaultButton          int32
	cRadioButtons           uint32
	pRadioButtons           uintptr
	nDefaultRadioButton     int32
	pszVerificationText     *uint16
	pszExpandedInformation  *uint16
	pszExpandedControlText  *uint16
	pszCollapsedControlText *uint16
	footerIcon              uintptr
	pszFooter               *uint16
	pfCallback              uintptr
	lpCallbackData          uintptr
	cxWidth                 uint32
}

func marshalTaskDialogButtons(buttons []taskDialogButton) []byte {
	record := make(packedRecord, 0, len(buttons)*taskDialogButtonSize)
	for _, button := range buttons {
		record.i32(button.id)
		record.uptr(uintptr(unsafe.Pointer(button.text)))
	}
	return record
}

// marshalTaskDialogConfig packs a TASKDIALOGCONFIG and the button array its
// pButtons field points at.
func marshalTaskDialogConfig(cfg taskDialogConfig, buttons []taskDialogButton) (configRecord, buttonRecord []byte) {
	buttonRecord = marshalTaskDialogButtons(buttons)
	record := make(packedRecord, 0, taskDialogConfigSize)
	record.u32(uint32(taskDialogConfigSize))
	record.uptr(cfg.hwndParent)
	record.uptr(cfg.hInstance)
	record.u32(cfg.dwFlags)
	record.u32(cfg.dwCommonButtons)
	record.uptr(uintptr(unsafe.Pointer(cfg.pszWindowTitle)))
	record.uptr(cfg.mainIcon)
	record.uptr(uintptr(unsafe.Pointer(cfg.pszMainInstruction)))
	record.uptr(uintptr(unsafe.Pointer(cfg.pszContent)))
	record.u32(uint32(len(buttons)))
	if len(buttonRecord) == 0 {
		record.uptr(0)
	} else {
		record.uptr(uintptr(unsafe.Pointer(&buttonRecord[0])))
	}
	record.i32(cfg.nDefaultButton)
	record.u32(cfg.cRadioButtons)
	record.uptr(cfg.pRadioButtons)
	record.i32(cfg.nDefaultRadioButton)
	record.uptr(uintptr(unsafe.Pointer(cfg.pszVerificationText)))
	record.uptr(uintptr(unsafe.Pointer(cfg.pszExpandedInformation)))
	record.uptr(uintptr(unsafe.Pointer(cfg.pszExpandedControlText)))
	record.uptr(uintptr(unsafe.Pointer(cfg.pszCollapsedControlText)))
	record.uptr(cfg.footerIcon)
	record.uptr(uintptr(unsafe.Pointer(cfg.pszFooter)))
	record.uptr(cfg.pfCallback)
	record.uptr(cfg.lpCallbackData)
	record.u32(cfg.cxWidth)
	return record, buttonRecord
}

// taskDialogChoice shows the service gate as a task dialog.
//
// The dialog names the actions on its buttons, so the user reads what each
// answer does. The default button — the one Enter selects — is the first
// button of the layout: the action the user asked for by starting HFDesk
// again, and the desktop mode for a positively confirmed stopped service.
func taskDialogChoice(prompt serviceChoicePrompt) (serviceChoice, error) {
	if err := taskDialogIndirect.Find(); err != nil {
		return serviceChoiceCancel, fmt.Errorf("the task dialog of the common controls is not available: %w", err)
	}

	title, err := windows.UTF16PtrFromString(serviceGateCaption)
	if err != nil {
		return serviceChoiceCancel, err
	}
	instruction, err := windows.UTF16PtrFromString(serviceGateInstruction(prompt))
	if err != nil {
		return serviceChoiceCancel, err
	}
	content, err := windows.UTF16PtrFromString(serviceGateContent(prompt))
	if err != nil {
		return serviceChoiceCancel, err
	}

	layout, defaultButton := serviceGateButtonLayout(prompt)
	labels := make([]*uint16, len(layout))
	buttons := make([]taskDialogButton, len(layout))
	for i, item := range layout {
		label, err := windows.UTF16PtrFromString(item.label)
		if err != nil {
			return serviceChoiceCancel, err
		}
		labels[i] = label
		buttons[i] = taskDialogButton{id: item.id, text: label}
	}

	configRecord, buttonRecord := marshalTaskDialogConfig(taskDialogConfig{
		dwFlags:            tdfAllowDialogCancellation,
		pszWindowTitle:     title,
		pszMainInstruction: instruction,
		pszContent:         content,
		nDefaultButton:     defaultButton,
	}, buttons)

	var pressed int32
	result, _, _ := taskDialogIndirect.Call(
		uintptr(unsafe.Pointer(&configRecord[0])),
		uintptr(unsafe.Pointer(&pressed)),
		0,
		0,
	)
	// The dialog reads the records and the UTF-16 strings during the call.
	runtime.KeepAlive(buttonRecord)
	runtime.KeepAlive(configRecord)
	runtime.KeepAlive(buttons)
	runtime.KeepAlive(labels)
	runtime.KeepAlive(content)
	runtime.KeepAlive(instruction)
	runtime.KeepAlive(title)

	// The call reports an HRESULT: a negative 32-bit value is a failure.
	if hresult := int32(uint32(result)); hresult < 0 {
		return serviceChoiceCancel, fmt.Errorf("the service choice dialog could not be shown (HRESULT 0x%08x)", uint32(result))
	}

	return serviceGateChoiceForButton(prompt, pressed)
}

// serviceGateFirstChoice returns the action of the open/start button of the
// choice dialog.
//
// The button is the action the user asked for by starting HFDesk again: a
// running service is opened, and a service that does not run is started. The
// label of the button is derived from the same predicate, so the label and the
// action are two renderings of one decision and cannot drift apart. The
// positively confirmed stopped service has the desktop mode in front of it as
// a button of its own, so this button is its second one and names the start.
func serviceGateFirstChoice(prompt serviceChoicePrompt) serviceChoice {
	if servicePromptRunning(prompt) {
		return serviceChoiceOpen
	}
	return serviceChoiceStart
}

// serviceGateChoiceForButton maps the id of a pressed button onto the action it
// performs.
//
// The task dialog reports ids and not positions, so the answer is read from the
// id alone, together with the status the dialog was opened for. The open/start
// id is the only id whose action depends on the status: it opens a running
// service and starts one that is not running. The desktop id is a fixed answer
// that only the stopped dialog can produce, and the gate re-checks the state
// before it honors it. Everything else is a fixed answer as well.
//
// The mapping is a pure function, so a test can prove both states of the
// open/start button and the other ids without a dialog: the real dialog blocks
// the process until the user answers it.
func serviceGateChoiceForButton(prompt serviceChoicePrompt, pressed int32) (serviceChoice, error) {
	switch pressed {
	case serviceGateOpenButtonID:
		return serviceGateFirstChoice(prompt), nil
	case serviceGateDesktopButtonID:
		// Only the dialog of a positively confirmed stopped service has
		// this button; the gate re-checks that state before it continues.
		return serviceChoiceDesktop, nil
	case serviceGateUpdateButtonID:
		return serviceChoiceUpdate, nil
	case serviceGateCancelButtonID, messageBoxCancel, 0:
		// The dialog closes with the id of the cancel button, with the id
		// of the standard Cancel button of a dialog that was closed with
		// Esc or with the close button, and with zero when the button of
		// the dialog is not reported at all.
		return serviceChoiceCancel, nil
	default:
		return serviceChoiceCancel, fmt.Errorf("the service choice dialog returned the unknown button %d", pressed)
	}
}

// messageBoxChoice asks the service gate with a message box.
//
// A message box has three answers but no named buttons, so the text maps Yes,
// No and Cancel onto the actions. Cancel is the default button, so a stray
// Enter changes nothing: the actions of the other two buttons change the
// machine.
//
// A positively confirmed stopped service has four actions, so it is asked in
// two boxes instead: the first one picks between the desktop mode, a service
// action and no action, and the second one names the service action when one
// is wanted. Its first box defaults to Yes, because the desktop mode changes
// nothing on the machine — the same rule that makes the desktop button the
// default of the task dialog.
func messageBoxChoice(prompt serviceChoicePrompt) serviceChoice {
	if servicePromptStopped(prompt) {
		return messageBoxStoppedChoice(prompt)
	}
	result := showGateMessageBox(serviceGateChooseText(prompt), windows.MB_DEFBUTTON3)
	switch result {
	case messageBoxYes:
		if servicePromptRunning(prompt) {
			return serviceChoiceOpen
		}
		return serviceChoiceStart
	case messageBoxNo:
		return serviceChoiceUpdate
	default:
		return serviceChoiceCancel
	}
}

// messageBoxStoppedChoice asks a stopped registration with its first message
// box: the desktop mode, a service action or no action.
func messageBoxStoppedChoice(prompt serviceChoicePrompt) serviceChoice {
	switch showGateMessageBox(serviceGateChooseText(prompt), windows.MB_DEFBUTTON1) {
	case messageBoxYes:
		return serviceChoiceDesktop
	case messageBoxNo:
		return messageBoxServiceActionChoice(prompt)
	default:
		return serviceChoiceCancel
	}
}

// messageBoxServiceActionChoice asks a stopped registration which service
// action it should get: the start or the update.
//
// Cancel stays the default button: both other answers change the machine, so
// a stray Enter changes nothing — the rule the ordinary fallback follows. The
// two answers are the actions of the ordinary fallback of a service that does
// not run, so they behave exactly like its Yes and No.
func messageBoxServiceActionChoice(prompt serviceChoicePrompt) serviceChoice {
	switch showGateMessageBox(serviceGateServiceActionChooseText(prompt), windows.MB_DEFBUTTON3) {
	case messageBoxYes:
		return serviceChoiceStart
	case messageBoxNo:
		return serviceChoiceUpdate
	default:
		return serviceChoiceCancel
	}
}

// showGateMessageBox shows one box of the fallback of the service gate and
// reports the id of the answer. A text or a caption that cannot be converted
// reports the Cancel id: without a readable question the box asks for no
// action.
//
// It is a package level variable so the tests can answer the fallback: the
// real message box blocks a process without an interactive window station.
var showGateMessageBox = showNativeGateMessageBox

// showNativeGateMessageBox shows one box of the fallback of the service gate
// in a native message box.
func showNativeGateMessageBox(text string, defaultButton uint32) int32 {
	textPtr, err := windows.UTF16PtrFromString(text)
	if err != nil {
		return messageBoxCancel
	}
	caption, err := windows.UTF16PtrFromString(serviceGateCaption)
	if err != nil {
		return messageBoxCancel
	}
	result, _ := windows.MessageBox(0, textPtr, caption,
		windows.MB_YESNOCANCEL|windows.MB_ICONQUESTION|defaultButton|
			windows.MB_SETFOREGROUND|windows.MB_TOPMOST)
	return result
}
