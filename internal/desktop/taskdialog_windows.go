// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
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

// taskDialogButton is the TASKDIALOG_BUTTON of the Windows API.
type taskDialogButton struct {
	// id is the command id the dialog reports for the button.
	id int32
	// text is the label of the button.
	text *uint16
}

// taskDialogConfig is the TASKDIALOGCONFIG of the Windows API.
//
// The struct mirrors the C declaration field by field, in the C order, and it
// declares no padding field of its own. The 64-bit ABI pads only where an
// eight-byte member (a pointer, HWND, HINSTANCE or HICON) must be aligned, and
// Go aligns a pointer to eight bytes for exactly the same reason, so the two
// layouts agree without help. Adjacent 32-bit members keep their four-byte
// slots: dwFlags and dwCommonButtons share one eight-byte slot, and so do
// nDefaultButton with cRadioButtons and cxWidth with the trailing padding of
// the struct. Spelling the padding out instead shifts every following field by
// four bytes: the dialog would read cButtons out of the low half of pButtons,
// pButtons out of a count, and the process would crash.
//
// The layout is checked by a test with unsafe.Offsetof, so a wrong struct fails
// in CI instead of failing at runtime:
//
//	cbSize 0, hwndParent 8, hInstance 16, dwFlags 24, dwCommonButtons 28,
//	pszWindowTitle 32, mainIcon 40, pszMainInstruction 48, pszContent 56,
//	cButtons 64, pButtons 72, nDefaultButton 80, cRadioButtons 84,
//	pRadioButtons 88, nDefaultRadioButton 96, pszVerificationText 104,
//	pszExpandedInformation 112, pszExpandedControlText 120,
//	pszCollapsedControlText 128, footerIcon 136, pszFooter 144,
//	pfCallback 152, lpCallbackData 160, cxWidth 168, size 176.
type taskDialogConfig struct {
	// cbSize is the size of the struct. The API uses it to tell the
	// versions of the struct apart.
	cbSize uint32
	// hwndParent is the owner of the dialog.
	hwndParent uintptr
	// hInstance is the instance that holds the resources of the dialog.
	hInstance uintptr
	// dwFlags are the TDF_* flags.
	dwFlags uint32
	// dwCommonButtons are the standard buttons of the dialog. The service
	// gate uses custom buttons instead, so the field stays zero.
	dwCommonButtons uint32
	// pszWindowTitle is the caption of the dialog.
	pszWindowTitle *uint16
	// mainIcon is the union of hMainIcon and pszMainIcon.
	mainIcon uintptr
	// pszMainInstruction is the instruction line of the dialog.
	pszMainInstruction *uint16
	// pszContent is the content of the dialog.
	pszContent *uint16
	// cButtons is the number of custom buttons.
	cButtons uint32
	// pButtons is the array of custom buttons.
	pButtons *taskDialogButton
	// nDefaultButton is the id of the default button.
	nDefaultButton int32
	// cRadioButtons is the number of radio buttons.
	cRadioButtons uint32
	// pRadioButtons is the array of radio buttons.
	pRadioButtons *taskDialogButton
	// nDefaultRadioButton is the index of the default radio button.
	nDefaultRadioButton int32
	// pszVerificationText is the label of the verification check box.
	pszVerificationText *uint16
	// pszExpandedInformation is the content of the expander.
	pszExpandedInformation *uint16
	// pszExpandedControlText is the label of the expander.
	pszExpandedControlText *uint16
	// pszCollapsedControlText is the label of the collapsed expander.
	pszCollapsedControlText *uint16
	// footerIcon is the union of hFooterIcon and pszFooterIcon.
	footerIcon uintptr
	// pszFooter is the footer of the dialog.
	pszFooter *uint16
	// pfCallback is the callback of the dialog.
	pfCallback uintptr
	// lpCallbackData is the data of the callback.
	lpCallbackData uintptr
	// cxWidth is the width of the dialog.
	cxWidth uint32
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
	config := taskDialogConfig{
		cbSize:             uint32(unsafe.Sizeof(taskDialogConfig{})),
		dwFlags:            tdfAllowDialogCancellation,
		pszWindowTitle:     title,
		pszMainInstruction: instruction,
		pszContent:         content,
		cButtons:           uint32(len(buttons)),
		pButtons:           &buttons[0],
		nDefaultButton:     serviceGateOpenButtonID,
	}

	var pressed int32
	result, _, _ := taskDialogIndirect.Call(
		uintptr(unsafe.Pointer(&config)),
		uintptr(unsafe.Pointer(&pressed)),
		0,
		0,
	)
	// The strings and the buttons are read by the dialog during the call,
	// so they are held until the call returned.
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

	switch pressed {
	case serviceGateOpenButtonID:
		return serviceChoiceOpen, nil
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
		if serviceStateRunning(prompt.State) {
			return serviceChoiceOpen
		}
		return serviceChoiceStart
	case messageBoxNo:
		return serviceChoiceUpdate
	default:
		return serviceChoiceCancel
	}
}
