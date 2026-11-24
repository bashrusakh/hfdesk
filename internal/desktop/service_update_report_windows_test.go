// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/bashrusakh/hfdesk/internal/service"
)

// TestFinishHandoverPicksTheRelaunchOfItsAction pins the end state of every
// committed handover at the one place that decides it: a settled start or
// update continues as a tray companion beside the service, a settled removal
// continues through the ordinary launch — the address is free again, so the
// gate takes the desktop mode — and an authorization that FAILED relaunches
// nothing while its report names the action that did not finish.
func TestFinishHandoverPicksTheRelaunchOfItsAction(t *testing.T) {
	companion, desktop := 0, 0
	replaceSeam(t, &startCompanionRelaunch, func() { companion++ })
	replaceSeam(t, &startDesktopRelaunch, func() { desktop++ })

	finishHandover(nil, committedHandover{action: serviceActionUpdate, relaunch: handoverRelaunchCompanion})
	if companion != 1 || desktop != 0 {
		t.Fatalf("a settled update relaunched companion=%d desktop=%d, want exactly one companion", companion, desktop)
	}

	finishHandover(nil, committedHandover{action: serviceActionUninstall, relaunch: handoverRelaunchDesktop})
	if desktop != 1 || companion != 1 {
		t.Fatalf("a settled removal relaunched companion=%d desktop=%d, want exactly one ordinary launch",
			companion, desktop)
	}

	messages := recordMessages(t)
	finishHandover(errors.New("the shutdown never finished"),
		committedHandover{action: serviceActionStart, relaunch: handoverRelaunchCompanion})
	if companion != 1 || desktop != 1 {
		t.Errorf("a failed authorization relaunched the host: companion=%d desktop=%d, want neither",
			companion, desktop)
	}
	if len(*messages) != 1 || (*messages)[0].caption != serviceFailureCaption(serviceActionStart) {
		t.Fatalf("the failed handover showed %v, want the failure of the start alone", *messages)
	}
	assertNames(t, (*messages)[0].text, "the report", []string{
		"could not finish the service start",
		"the shutdown never finished",
	})
}

// TestUpdateReportOffersTheOldFileCommandOnlyForAMove pins the report of a
// finished update at the window it really shows: the registration moving to
// another file whose old program is still on the disk carries the one
// command — named with exactly that old path — and reaches the report seam
// that owns it instead of the plain message box, while an update in place and
// an old file that is already gone keep the established window with no
// command and no old path anywhere in it.
//
// The moved case is driven on BOTH launch surfaces — the desktop host and the
// tray companion — because the block follows the MOVE, never the route: the
// gate's direct update and the handover's update build this very window, so
// both carry it.
func TestUpdateReportOffersTheOldFileCommandOnlyForAMove(t *testing.T) {
	existing := filepath.Join(t.TempDir(), "hfdesk-old.exe")
	if err := os.WriteFile(existing, []byte("old"), 0o644); err != nil {
		t.Fatalf("write the fixture of the old program: %v", err)
	}
	missing := filepath.Join(t.TempDir(), "gone.exe")
	inPlace := filepath.Join(t.TempDir(), "hfdesk.exe")

	for _, tc := range []struct {
		name              string
		previous, current string
		companionSurface  bool
		wantCommand       bool
	}{
		{name: "the registration moved and the old file is still there",
			previous: existing, current: `C:\portable\new\hfdesk.exe`, wantCommand: true},
		{name: "the same move on the companion surface",
			previous: existing, current: `C:\portable\new\hfdesk.exe`,
			companionSurface: true, wantCommand: true},
		{name: "the registration kept the very same path",
			previous: inPlace, current: inPlace, wantCommand: false},
		{name: "the registration moved and the old file is gone",
			previous: missing, current: `C:\portable\new\hfdesk.exe`, wantCommand: false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stopped := runningServiceStatus(service.StateStopped, "127.0.0.1:18080")
			stopped.Executable = tc.previous
			manager := &fakeServiceManager{
				statuses: []service.Status{stopped},
				installResult: service.InstallOptions{
					Executable: tc.current,
					Listen:     "127.0.0.1:18080",
					Paths:      stopped.Paths,
				},
			}
			replaceSeam(t, &newServiceManager, func(service.Scope) (service.Manager, error) {
				return manager, nil
			})
			replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
				return tc.current, nil
			})
			messages := recordMessages(t)
			type offered struct{ text, oldFile string }
			var commands []offered
			replaceSeam(t, &showReportWithOldFileCommand,
				func(caption, text string, icon uint32, oldFile string) {
					if caption != serviceUpdatedCaption {
						t.Errorf("the command report is captioned %q, want %q", caption, serviceUpdatedCaption)
					}
					commands = append(commands, offered{text: text, oldFile: oldFile})
				})

			err := RunServiceAction(ServiceActionOptions{
				Action: "update", CompanionSurface: tc.companionSurface,
			})
			assertNoError(t, err, "the update failed: %v", err)

			if !tc.wantCommand {
				if len(commands) != 0 {
					t.Errorf("the report offered the old-file command %v, want none", commands)
				}
				if len(*messages) != 1 || (*messages)[0].caption != serviceUpdatedCaption {
					t.Fatalf("the report showed %v, want the plain update window alone", *messages)
				}
				assertNamesWithout(t, (*messages)[0].text, "the plain report", nil,
					[]string{"Old file still on disk", "may belong", deleteOldFileLabel})
				return
			}
			if len(*messages) != 0 {
				t.Errorf("the command report also showed the plain window %v", *messages)
			}
			if len(commands) != 1 {
				t.Fatalf("the report offered %d commands, want exactly one: %v", len(commands), commands)
			}
			if commands[0].oldFile != tc.previous {
				t.Errorf("the command names %q, want exactly the previously registered program %q",
					commands[0].oldFile, tc.previous)
			}
			assertNames(t, commands[0].text, "the command report", []string{
				"The registration now runs " + tc.current,
				"Old file still on disk: " + tc.previous + ".",
			})
			assertNamesWithout(t, commands[0].text, "the command report", nil,
				[]string{"Open the address", "Start HFDesk again", "choose Start service",
					"Start the desktop app again", "Install the service again", "Quit HFDesk"})
			// The report states the file and nothing behind it: the
			// cancelled pair-assumption must not come back in any
			// spelling (decision of 2026-11-24), and the command
			// keeps its exact label.
			assertNamesWithout(t, commands[0].text, "the command report", nil,
				[]string{"may belong", "old folder", "pair of"})
			if deleteOldFileLabel != "Delete the old file" {
				t.Errorf("the command is labelled %q, want %q", deleteOldFileLabel, "Delete the old file")
			}
		})
	}
}
