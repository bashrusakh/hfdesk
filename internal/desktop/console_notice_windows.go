// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/windows"
)

// The identifiers of the buttons of the missing-program notice.
//
// The task dialog reports the id of the pressed button back, so the answer is
// a value of the dialog and not a position in a list. The ids start above the
// ids of the standard buttons, and the progress dialog keeps its own id, so
// no answer of one dialog can be read as a button of the other. The id of the
// log link is the one the failure reports use (logFileButtonID), because it
// is the same action in both windows.
const (
	siblingMissingDownloadButtonID = 105
	siblingMissingDismissButtonID  = 106
	consoleDownloadCancelButtonID  = 107
	siblingMissingManualButtonID   = 108
	siblingMissingRetryButtonID    = 109
	// standardButtonOKID is the id the task dialog reports for its common
	// OK button.
	standardButtonOKID = 1
)

// The task dialog flags of CommCtrl.h beyond the ones the service gate uses.
const (
	// tdfUseCommandLinks renders the custom buttons of pButtons as
	// command links; the note of a link is the text after its first new
	// line.
	tdfUseCommandLinks = 0x0010
	// tdfShowProgressBar displays the hosted progress bar.
	tdfShowProgressBar = 0x0200
	// tdfCallbackTimer calls the dialog callback roughly every 200
	// milliseconds, which is how the progress bar follows the transfer.
	tdfCallbackTimer = 0x0800
)

// The task dialog messages and callback notifications of CommCtrl.h: the
// messages are WM_USER + n, the notifications are the msg argument of the
// dialog callback.
const (
	tdmClickButton       = 0x0400 + 102
	tdmSetProgressBarPos = 0x0400 + 106
	// tdnCreated is the notification of a task dialog that has been built
	// but is not on screen yet — the one moment a window can still be
	// ordered before the user sees where it landed.
	tdnCreated       = 0
	tdnButtonClicked = 2
	tdnTimer         = 4
)

// The common-button flag of the OK button of a task dialog.
const tdcOKButton = 0x0001

// user32SendMessageW is how the dialog callback drives its own dialog: the
// task dialog documents the progress-bar and button messages as messages to
// its window, and the shared system library carries no Go wrapper for them.
var user32SendMessageW = windows.NewLazySystemDLL("user32.dll").NewProc("SendMessageW")

// siblingMissingAnswer is what the notice of the missing console program asks
// the caller to do: download the paired release asset and continue, read the
// release again, or leave everything as it is.
type siblingMissingAnswer int

const (
	// siblingMissingDismiss closes the notice and changes nothing. Every
	// way of leaving the window without a choice — Not now, Enter, Esc,
	// the close button, and the log link, which opens a file but decides
	// nothing about the action — answers this.
	siblingMissingDismiss siblingMissingAnswer = iota
	// siblingMissingDownload runs the verified download and lets the
	// caller continue the very same service action.
	siblingMissingDownload
	// siblingMissingRetry reads the release again and shows the notice
	// anew: it is the answer of "Try again", the item that stands where
	// the download button would be whenever no download can be offered.
	siblingMissingRetry
)

// siblingMissingButton is one command link of the notice's task dialog: the id
// the dialog reports back, the label the user reads, the note line under it —
// empty when the link carries none — and the answer the press performs. The
// label and the answer are parts of one decision, so a test reads them side
// by side.
type siblingMissingButton struct {
	id     int32
	label  string
	note   string
	answer siblingMissingAnswer
}

// commandLinkText is what the dialog hands to Windows for this link: the
// label, and the note on the line under it — the note of a command link is
// everything after its first new line, which is why a link worth reading
// before the press carries one.
func (button siblingMissingButton) commandLinkText() string {
	if button.note == "" {
		return button.label
	}
	return button.label + "\n" + button.note
}

// siblingMissingDialog is the composition of the task dialog of the notice:
// the command links in display order and the id Enter selects.
//
// The composition is a pure function of the notice, so a test can pin the
// button set, their order, the default and the answer of every press without
// a dialog: the real dialog blocks the process until the user answers it.
type siblingMissingDialog struct {
	// buttons are the command links in display order. Every state of the
	// notice has them: a task dialog renders every custom button of a
	// command-link dialog as a link, and the answers of this notice are
	// all named actions — the download, the manual route, the retry, the
	// log, and the dismissal.
	buttons []siblingMissingButton
	// defaultButton is the id Enter selects. It is the dismiss answer of
	// every composition, so a stray Enter never starts a download and
	// never reads the release again either.
	defaultButton int32
}

// siblingMissingDialogLayout returns the composition of the task dialog of
// the missing console program.
//
// With an offer the download is the first command link, carrying its note
// under it; the manual route is a visible item in every state — the way
// without a download used to hide behind a fold — and "Not now" is always the
// last link and the default button, so Enter, Esc and the close button all
// change nothing. Without an offer the download link is replaced by the
// reason line in the content and by "Try again", which reads the release
// again: the button never disappears without a way back. The log link joins
// every state that has a log file of this launch to name.
func siblingMissingDialogLayout(notice siblingMissingNotice) siblingMissingDialog {
	buttons := make([]siblingMissingButton, 0, 4)
	if notice.Download != nil {
		buttons = append(buttons, siblingMissingButton{
			id:     siblingMissingDownloadButtonID,
			label:  siblingMissingDownloadLabel,
			note:   siblingMissingDownloadNote(notice),
			answer: siblingMissingDownload,
		})
	}
	buttons = append(buttons, siblingMissingButton{
		id:     siblingMissingManualButtonID,
		label:  siblingMissingManualLabel,
		note:   notice.Manual,
		answer: siblingMissingDismiss,
	})
	if notice.Download == nil {
		buttons = append(buttons, siblingMissingButton{
			id:     siblingMissingRetryButtonID,
			label:  siblingMissingRetryLabel,
			answer: siblingMissingRetry,
		})
	}
	if notice.LogFile != "" {
		buttons = append(buttons, siblingMissingButton{
			id:     logFileButtonID,
			label:  logFileOpenLabel,
			note:   notice.LogFile,
			answer: siblingMissingDismiss,
		})
	}
	buttons = append(buttons, siblingMissingButton{
		id:     siblingMissingDismissButtonID,
		label:  siblingMissingDismissLabel,
		answer: siblingMissingDismiss,
	})
	return siblingMissingDialog{
		buttons:       buttons,
		defaultButton: siblingMissingDismissButtonID,
	}
}

// answer maps the id the dialog reports back onto the answer of the notice.
//
// The answer is read from the pressed id alone: the download link continues
// the action, the retry link reads the release again, and an id the dialog
// never sends is reported instead of guessed — a button the host does not know
// must never start a download. The log link opens its file inside the dialog
// and keeps the dialog open, so it never reaches this mapping while the window
// stands; if the window ends on it anyway, nothing about the action was
// decided, which is the dismiss answer.
func (dialog siblingMissingDialog) answer(pressed int32) (siblingMissingAnswer, error) {
	for _, button := range dialog.buttons {
		if button.id == pressed {
			return button.answer, nil
		}
	}
	switch pressed {
	case standardButtonOKID, messageBoxCancel, 0:
		// The id a closed dialog reports for Esc or the close button,
		// and a call that reported no button at all, all change
		// nothing.
		return siblingMissingDismiss, nil
	default:
		return siblingMissingDismiss, fmt.Errorf("the notice of the missing console program returned the unknown button %d", pressed)
	}
}

// The seams of the notice, the transfer and the offer.
//
// They exist for the same reason as the seams of the service gate: the real
// dialogs block a process without an interactive window station, and the
// offer reads the network. The tests replace them with scripted answers and
// recorded copy.
var (
	// showSiblingMissingNotice shows the notice and returns the answer of
	// the user.
	showSiblingMissingNotice = presentSiblingMissingNotice
	// showSiblingMissingMessageBox is the Yes/No box of an offer-bearing
	// notice; the fallback of the notice flow itself is manual-only
	// (presentSiblingMissingNotice), so only tests reach the offer
	// through it.
	showSiblingMissingMessageBox = showNativeSiblingMissingMessageBox
	// runConsoleDownload runs the transfer behind its progress dialog.
	runConsoleDownload = downloadConsoleReleaseWithProgress
	// consoleDownloadOfferFor resolves the release metadata the notice
	// needs to offer a verified download.
	consoleDownloadOfferFor = resolveConsoleDownloadOffer
)

// presentSiblingMissingNotice shows the notice as a task dialog and falls
// back to a message box when the task dialog call fails.
//
// The fallback shows the same notice in one box — instruction, content and
// manual steps — and the offer itself is only kept when the transfer behind
// it can actually run, and this fallback exists only because the task dialog
// of the notice just failed: the transfer behind the offer shows its
// progress and its Cancel in a task dialog of this same machine moments
// later, so a failed attempt never keeps the offer and the box degrades to
// manual-only — the same suppression rule a Windows without a progress
// dialog gets — instead of asking a Yes whose transfer would refuse without
// its progress dialog. The user is never left without the notice.
func presentSiblingMissingNotice(notice siblingMissingNotice) siblingMissingAnswer {
	answer, err := taskDialogSiblingMissing(notice)
	if err == nil {
		return answer
	}
	desktopLog.Warn("the notice of the missing console program is shown in a message box", "error", err)
	return messageBoxSiblingMissing(siblingMissingNoticeForFallback(notice, err))
}

// consoleProgressDialogGate is the check that decides whether the transfer
// behind the offer can run: a Windows whose common controls have no task
// dialog cannot show the progress and the Cancel the offer promises, so
// nothing is downloaded at all. The transfer reads this gate before any byte
// moves; the message box fallback needs no second read of it, because the
// fallback only exists for a failed task dialog attempt — and that failure
// already answers the same question for it (siblingMissingNoticeForFallback).
func consoleProgressDialogGate() error {
	return taskDialogIndirect.Find()
}

// taskDialogSiblingMissing shows the notice of the missing console program
// as a task dialog.
//
// It renders the composition of siblingMissingDialogLayout: the links, their
// order and the default button all come from that pure function, so the
// layout and what the dialog shows are one decision. The first glance is the
// instruction, the two facts and — when no download can be offered — the one
// reason line where the download button would be; the manual route is the note
// under its own visible link; the pairing rule and the raw cause of a failed
// lookup live in the folded details; and the log file of this launch is an
// action of its own, which opens the file and leaves this window standing.
func taskDialogSiblingMissing(notice siblingMissingNotice) (siblingMissingAnswer, error) {
	layout := siblingMissingDialogLayout(notice)

	title, err := windows.UTF16PtrFromString(notice.Title)
	if err != nil {
		return siblingMissingDismiss, err
	}
	instruction, err := windows.UTF16PtrFromString(notice.Instruction)
	if err != nil {
		return siblingMissingDismiss, err
	}
	content := notice.Content
	if notice.Reason != "" {
		content += "\n\n" + notice.Reason
	}
	contentPtr, err := windows.UTF16PtrFromString(content)
	if err != nil {
		return siblingMissingDismiss, err
	}

	config := taskDialogConfig{
		dwFlags:            tdfAllowDialogCancellation | tdfUseCommandLinks,
		pszWindowTitle:     title,
		pszMainInstruction: instruction,
		pszContent:         contentPtr,
		nDefaultButton:     layout.defaultButton,
	}
	if notice.Details != "" {
		details, err := windows.UTF16PtrFromString(notice.Details)
		if err != nil {
			return siblingMissingDismiss, err
		}
		detailsLabel, err := windows.UTF16PtrFromString(siblingMissingDetailsLabel)
		if err != nil {
			return siblingMissingDismiss, err
		}
		config.pszExpandedInformation = details
		config.pszExpandedControlText = detailsLabel
		config.pszCollapsedControlText = detailsLabel
	}
	if notice.LogFile != "" {
		config.pfCallback = logFileCallback(notice.LogFile, false)
	}

	var buttons []taskDialogButton
	for _, item := range layout.buttons {
		label, err := windows.UTF16PtrFromString(item.commandLinkText())
		if err != nil {
			return siblingMissingDismiss, err
		}
		buttons = append(buttons, taskDialogButton{id: item.id, text: label})
	}

	pressed, err := runTaskDialog(config, buttons)
	if err != nil {
		return siblingMissingDismiss, err
	}
	return layout.answer(pressed)
}

// runTaskDialog shows one task dialog and returns the id of the pressed
// button.
//
// The dialog reads the packed record and the UTF-16 strings during the call,
// so everything that owns them is kept alive until the call returned.
func runTaskDialog(config taskDialogConfig, buttons []taskDialogButton) (int32, error) {
	if err := taskDialogIndirect.Find(); err != nil {
		return 0, fmt.Errorf("the task dialog of the common controls is not available: %w", err)
	}
	configRecord, buttonRecord := marshalTaskDialogConfig(config, buttons)

	var pressed int32
	result, _, _ := taskDialogIndirect.Call(
		uintptr(unsafe.Pointer(&configRecord[0])),
		uintptr(unsafe.Pointer(&pressed)),
		0,
		0,
	)
	runtime.KeepAlive(configRecord)
	runtime.KeepAlive(buttonRecord)
	runtime.KeepAlive(buttons)
	runtime.KeepAlive(config)

	if hresult := int32(uint32(result)); hresult < 0 {
		return 0, fmt.Errorf("the dialog could not be shown (HRESULT 0x%08x)", uint32(result))
	}
	return pressed, nil
}

// messageBoxSiblingMissing asks the notice with a message box: the offered
// download as a Yes/No question whose text maps the answers, the
// manual-only notice as one box with its whole text.
func messageBoxSiblingMissing(notice siblingMissingNotice) siblingMissingAnswer {
	if notice.Download == nil {
		showMessage(notice.Title, siblingMissingManualNoticeText(notice), windows.MB_ICONERROR)
		return siblingMissingDismiss
	}
	if showSiblingMissingMessageBox(siblingMissingChooseText(notice)) == messageBoxYes {
		return siblingMissingDownload
	}
	return siblingMissingDismiss
}

// showNativeSiblingMissingMessageBox shows the Yes/No question of an
// offer-bearing notice in a native message box and reports the id of the
// answer.
//
// No is the default button: a stray Enter must never start a download, the
// same rule the task dialog's default button follows. A text that cannot be
// converted reports the No id: without a readable question the box offers no
// download.
func showNativeSiblingMissingMessageBox(text string) int32 {
	textPtr, err := windows.UTF16PtrFromString(text)
	if err != nil {
		return messageBoxNo
	}
	captionPtr, err := windows.UTF16PtrFromString(serviceSiblingMissingCaption)
	if err != nil {
		return messageBoxNo
	}
	result, _ := windows.MessageBox(0, textPtr, captionPtr,
		windows.MB_YESNO|windows.MB_ICONQUESTION|windows.MB_DEFBUTTON2|
			windows.MB_SETFOREGROUND|windows.MB_TOPMOST)
	return result
}

// consoleDownloadProgressState is what the progress dialog reads while the
// transfer runs on its own goroutine: the bytes read so far and whether the
// transfer already produced its result.
type consoleDownloadProgressState struct {
	mu         sync.Mutex
	downloaded int64
	total      int64
	finished   bool
}

func (state *consoleDownloadProgressState) set(downloaded, total int64) {
	state.mu.Lock()
	state.downloaded = downloaded
	if total > 0 {
		state.total = total
	}
	state.mu.Unlock()
}

// markFinished tells the dialog that the result is final, so the next tick
// of its timer closes it.
func (state *consoleDownloadProgressState) markFinished() {
	state.mu.Lock()
	state.finished = true
	state.mu.Unlock()
}

// snapshot returns the percentage the bar shows and the finished fact.
func (state *consoleDownloadProgressState) snapshot() (percent int, finished bool) {
	state.mu.Lock()
	defer state.mu.Unlock()
	if state.total > 0 {
		percent = int(state.downloaded * 100 / state.total)
		if percent < 0 {
			percent = 0
		}
		if percent > 100 {
			percent = 100
		}
	}
	return percent, state.finished
}

// downloadConsoleReleaseWithProgress runs the transfer of the offer behind
// its progress dialog.
//
// The task dialog owns the message loop of this thread, so the transfer runs
// on a goroutine of its own. The dialog can end in two ways and the
// settlement tells them apart: the user's Cancel — including one in the
// window after the transfer already finished and verified — aborts the whole
// action, removing the file this transfer placed and reporting the
// cancellation with its cause and the manual path, while the dialog's own
// close after the transfer produced its result is not a cancel and hands the
// verified file to the caller. The result is only read after the goroutine
// finished — the report of a cancelled transfer must never race the cleanup
// that removes the staging file. A Windows whose common controls have no
// task dialog cannot show the progress and the Cancel the offer promises, so
// nothing is downloaded at all and the failure names the manual path.
func downloadConsoleReleaseWithProgress(notice siblingMissingNotice) (string, error) {
	offer := notice.Download
	if offer == nil {
		return "", errors.New("no download of the console program was offered")
	}
	if err := consoleProgressDialogGate(); err != nil {
		return "", fmt.Errorf("the progress dialog of Windows is not available, so nothing was downloaded: %w", err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	progress := &consoleDownloadProgressState{total: offer.Size}

	var (
		outcome   consoleDownloadOutcome
		resultErr error
	)
	done := make(chan struct{})
	go func() {
		defer close(done)
		outcome, resultErr = downloadConsoleRelease(ctx, offer, notice.Folder, progress.set)
		progress.markFinished()
	}()

	userCancelled, dialogErr := showConsoleDownloadDialog(notice, progress)
	cancel()
	// The wait closes the window between the dialog and the transfer: a
	// cancelled transfer removes its staging file before this returns, so
	// the report that follows can state what is actually left.
	<-done
	if dialogErr != nil {
		desktopLog.Warn("the progress dialog of the download ended with an error", "error", dialogErr)
	}
	return settleConsoleDownload(userCancelled, dialogErr, outcome, resultErr)
}

// showConsoleDownloadDialog shows the progress dialog of the transfer: a
// bar that follows the bytes read and one Cancel button, which is also what
// Esc and the close button press. The dialog closes itself on the first
// timer tick after the transfer produced its result — announcing that
// programmatic close before pressing the button — and reports whether the
// user was the one who ended it: a dialog that closed without that
// announcement was ended by the user's Cancel, Esc or close button, a
// dialog that closed with it was ended by itself, and a dialog whose run
// failed was answered by no one.
func showConsoleDownloadDialog(notice siblingMissingNotice, progress *consoleDownloadProgressState) (bool, error) {
	offer := notice.Download
	instruction, content := consoleDownloadProgressText(offer)

	title, err := windows.UTF16PtrFromString(serviceSiblingMissingCaption)
	if err != nil {
		return false, err
	}
	instructionPtr, err := windows.UTF16PtrFromString(instruction)
	if err != nil {
		return false, err
	}
	contentPtr, err := windows.UTF16PtrFromString(content)
	if err != nil {
		return false, err
	}
	cancelLabel, err := windows.UTF16PtrFromString("Cancel")
	if err != nil {
		return false, err
	}

	closeCause := &consoleDialogClose{}
	callback := syscall.NewCallback(func(hwnd, msg, wParam, lParam, data uintptr) uintptr {
		switch msg {
		case tdnTimer:
			percent, finished := progress.snapshot()
			user32SendMessageW.Call(hwnd, tdmSetProgressBarPos, uintptr(percent), 0)
			if finished {
				// The transfer has its result: announce the
				// programmatic close, then press the only
				// button to end the dialog. The announcement
				// precedes the click — the notification it
				// causes arrives nested in the send below,
				// on this same thread — so this close can
				// never read as a cancel of the user, while
				// a press that never saw the announcement
				// was the user's.
				closeCause.transferSettled()
				user32SendMessageW.Call(hwnd, tdmClickButton, consoleDownloadCancelButtonID, 0)
			}
		case tdnButtonClicked:
			// Every press ends the dialog here — the Cancel
			// button, Esc and the close button alike — and
			// whose press it was is read from the
			// announcement above when runTaskDialog returns,
			// not from this id.
		}
		return 0
	})

	config := taskDialogConfig{
		dwFlags:            tdfAllowDialogCancellation | tdfShowProgressBar | tdfCallbackTimer,
		pszWindowTitle:     title,
		pszMainInstruction: instructionPtr,
		pszContent:         contentPtr,
		nDefaultButton:     consoleDownloadCancelButtonID,
		pfCallback:         callback,
	}
	if _, err := runTaskDialog(config, []taskDialogButton{
		{id: consoleDownloadCancelButtonID, text: cancelLabel},
	}); err != nil {
		return false, err
	}
	return closeCause.userCancelled(nil), nil
}
