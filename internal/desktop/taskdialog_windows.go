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

// The identifiers of the three buttons of the service gate dialog.
//
// The task dialog reports the id of the pressed button back, so the answer of
// the user is a value of the dialog and not a position in a list. The ids start
// above the ids of the standard buttons, so an answer of the message box
// fallback can never be read as a button of the dialog.
const (
	serviceGateOpenButtonID   = 101
	serviceGateUpdateButtonID = 102
	serviceGateCancelButtonID = 103
)

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
// The dialog names the three actions on its buttons, so the user reads what
// each answer does. The first button is the default: it is the action the user
// asked for by starting HFDesk again.
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
	openLabel, err := windows.UTF16PtrFromString(serviceGateOpenLabel(prompt))
	if err != nil {
		return serviceChoiceCancel, err
	}
	updateLabel, err := windows.UTF16PtrFromString(serviceGateUpdateLabel)
	if err != nil {
		return serviceChoiceCancel, err
	}
	cancelLabel, err := windows.UTF16PtrFromString(serviceGateCancelLabel)
	if err != nil {
		return serviceChoiceCancel, err
	}

	buttons := [3]taskDialogButton{
		{id: serviceGateOpenButtonID, text: openLabel},
		{id: serviceGateUpdateButtonID, text: updateLabel},
		{id: serviceGateCancelButtonID, text: cancelLabel},
	}
	configRecord, buttonRecord := marshalTaskDialogConfig(taskDialogConfig{
		dwFlags:            tdfAllowDialogCancellation,
		pszWindowTitle:     title,
		pszMainInstruction: instruction,
		pszContent:         content,
		nDefaultButton:     serviceGateOpenButtonID,
	}, buttons[:])

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
	runtime.KeepAlive(cancelLabel)
	runtime.KeepAlive(updateLabel)
	runtime.KeepAlive(openLabel)
	runtime.KeepAlive(content)
	runtime.KeepAlive(instruction)
	runtime.KeepAlive(title)

	// The call reports an HRESULT: a negative 32-bit value is a failure.
	if hresult := int32(uint32(result)); hresult < 0 {
		return serviceChoiceCancel, fmt.Errorf("the service choice dialog could not be shown (HRESULT 0x%08x)", uint32(result))
	}

	return serviceGateChoiceForButton(prompt, pressed)
}

// serviceGateFirstChoice returns the action of the first button of the choice
// dialog.
//
// The first button is the action the user asked for by starting HFDesk again:
// a running service is opened, and a service that does not run is started. The
// label of the button is derived from the same predicate, so the label and the
// action are two renderings of one decision and cannot drift apart.
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
// id alone, together with the status the dialog was opened for. The first button
// is the only id whose action depends on the status: it opens a running service
// and starts one that is not running. Everything else is a fixed answer.
//
// The mapping is a pure function, so a test can prove both states of the first
// button and the other ids without a dialog: the real dialog blocks the process
// until the user answers it.
func serviceGateChoiceForButton(prompt serviceChoicePrompt, pressed int32) (serviceChoice, error) {
	switch pressed {
	case serviceGateOpenButtonID:
		return serviceGateFirstChoice(prompt), nil
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
func messageBoxChoice(prompt serviceChoicePrompt) serviceChoice {
	text, err := windows.UTF16PtrFromString(serviceGateChooseText(prompt))
	if err != nil {
		return serviceChoiceCancel
	}
	caption, err := windows.UTF16PtrFromString(serviceGateCaption)
	if err != nil {
		return serviceChoiceCancel
	}
	result, _ := windows.MessageBox(0, text, caption,
		windows.MB_YESNOCANCEL|windows.MB_ICONQUESTION|windows.MB_DEFBUTTON3|
			windows.MB_SETFOREGROUND|windows.MB_TOPMOST)
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
