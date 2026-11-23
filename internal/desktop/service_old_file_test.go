// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/bashrusakh/hfdesk/internal/service"
)

// restoreOldFileRemover points the remover of the report's command at the
// function of the test and puts the production one back when the test ends.
// It lives here because replaceSeam belongs to the Windows test files and
// these assertions run on every platform.
func restoreOldFileRemover(t *testing.T, remove func(string) error) {
	t.Helper()
	previous := removeOldServiceProgram
	removeOldServiceProgram = remove
	t.Cleanup(func() { removeOldServiceProgram = previous })
}

// TestServiceUpdateOldFileNamesOnlyARealMoveToAnotherFile pins the decision
// behind the report's old-file command: it names the program the
// registration ran before the update ONLY when the registration really moved
// to another file and that old file is still on the disk. An update in place
// (the same path, in whatever case the two spellings use), an old file that
// is already gone and a registration that reports no previous program all
// answer with nothing, so their report carries no mention of an old file and
// no command for one.
func TestServiceUpdateOldFileNamesOnlyARealMoveToAnotherFile(t *testing.T) {
	existing := filepath.Join(t.TempDir(), "hfdesk-old.exe")
	if err := os.WriteFile(existing, []byte("old"), 0o644); err != nil {
		t.Fatalf("write the fixture of the old program: %v", err)
	}
	missing := filepath.Join(t.TempDir(), "gone.exe")

	for _, tc := range []struct {
		name              string
		previous, current string
		want              string
	}{
		{name: "the registration moved to another file that is still there",
			previous: existing, current: `C:\new\hfdesk.exe`, want: existing},
		{name: "the registration kept the very same path",
			previous: `C:\portable\hfdesk.exe`, current: `c:\PORTABLE\hfdesk.EXE`, want: ""},
		{name: "the old file is already gone",
			previous: missing, current: `C:\new\hfdesk.exe`, want: ""},
		{name: "the registration reports no previous program",
			previous: "", current: `C:\new\hfdesk.exe`, want: ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := serviceUpdateOldFile(tc.previous, tc.current); got != tc.want {
				t.Errorf("serviceUpdateOldFile(%q, %q) = %q, want %q",
					tc.previous, tc.current, got, tc.want)
			}
		})
	}
}

// TestServiceUpdatedTextCarriesTheOldFileBlockOnlyForAMove pins the report of
// a finished update in both shapes: with an old file it states exactly that
// file as the one fact line — still on the disk, and nothing behind it: no
// assumption about the file (decision of 2026-11-24: reports carry facts,
// never guesses) — while the established state and its single way back of the
// report stay what they are; without one the report is the report this family
// has always shown, with no mention of an old file anywhere in it.
//
// The block follows the MOVE and never the route: the direct and the handover
// surface build the report through this one function, so both carry it, and
// the surface only ever picks the way back — which each subtest asserts, so
// the surface dimension really varies.
func TestServiceUpdatedTextCarriesTheOldFileBlockOnlyForAMove(t *testing.T) {
	const (
		oldFile = `C:\old\hfdesk.exe`
		newFile = `C:\new\hfdesk.exe`
	)
	build := func(surface launchSurface, oldFile string) string {
		return serviceUpdatedText("1.3.0", newFile, `C:\ProgramData\HFDesk`, "",
			false, service.StateStopped, surface, oldFile)
	}

	for _, tc := range []struct {
		name    string
		surface launchSurface
		wayBack string
	}{
		{name: "the desktop surface", surface: launchSurfaceDesktop,
			wayBack: "Start the desktop app again and choose Start service."},
		{name: "the companion surface", surface: launchSurfaceCompanion,
			wayBack: "Choose Start service from the tray menu."},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withOld := build(tc.surface, oldFile)
			assertNames(t, withOld, "the report with an old file", []string{
				// The new program, then the old file as the one
				// fact line beside it.
				"The registration now runs " + newFile,
				"Old file still on disk: " + oldFile + ".",
				// The established state and its single way back
				// survive the block: the report still says what
				// the update left and names exactly one route back —
				// the route of THIS surface.
				"The service stays stopped.",
				"Nothing serves HFDesk right now.",
				tc.wayBack,
			})
			if got := strings.Count(withOld, "Start service"); got != 1 {
				t.Errorf("the report names Start service %d times, want exactly one:\n%s", got, withOld)
			}
			// The block states the file and nothing behind it: no
			// assumption about the file, never a second object, and
			// never a deletion the report performed itself.
			assertNamesWithout(t, withOld, "the report with an old file", nil,
				[]string{"hfdesk-desktop.exe", "may belong", "old folder", "pair of",
					"was deleted", "could not be deleted"})

			without := build(tc.surface, "")
			if strings.Contains(without, oldFile) || strings.Contains(without, "Old file still on disk") {
				t.Errorf("the report without an old file mentions one:\n%s", without)
			}
			// The same report the family always built: the addition is
			// exactly the block above and nothing else.
			assertNames(t, without, "the plain report", []string{
				"The registration now runs " + newFile,
				"The service stays stopped.",
				tc.wayBack,
			})
			if without == withOld {
				t.Error("the two shapes of the report are identical: the old-file block never reached the text")
			}
		})
	}
}

// TestTheOldFileCommandTouchesExactlyTheNamedFileAndOnlyWhenItRuns pins the
// deletion behind the report's command: building the report and deciding
// that it may carry the command delete NOTHING, while the command itself
// hands the remover exactly the path it was given — once — and reports the
// honest outcome of that attempt in both directions.
func TestTheOldFileCommandTouchesExactlyTheNamedFileAndOnlyWhenItRuns(t *testing.T) {
	const target = `C:\old\hfdesk.exe`
	var removed []string
	restoreOldFileRemover(t, func(path string) error {
		removed = append(removed, path)
		return nil
	})

	// The report and its old-file decision are pure: neither of them
	// reaches the remover, so no outcome of an update ever deletes a file
	// without the press of the command.
	_ = serviceUpdatedText("1.3.0", `C:\new\hfdesk.exe`, `C:\ProgramData\HFDesk`, "",
		true, service.StateRunning, launchSurfaceCompanion, target)
	_ = serviceUpdateOldFile(target, `C:\new\hfdesk.exe`)
	if len(removed) != 0 {
		t.Fatalf("building the report deleted %v, want nothing: the deletion needs the press of the command", removed)
	}

	// The command keeps its exact label: one press, this one file.
	if deleteOldFileLabel != "Delete the old file" {
		t.Errorf("the command is labelled %q, want %q", deleteOldFileLabel, "Delete the old file")
	}

	caption, text, failed := runDeleteOldFileCommand(target)
	if failed {
		t.Fatalf("the command on an accepting remover reported a failure: %q", text)
	}
	if len(removed) != 1 || removed[0] != target {
		t.Errorf("the command deleted %v, want exactly [%s] and nothing else", removed, target)
	}
	if caption != oldFileDeletedCaption {
		t.Errorf("the outcome is captioned %q, want %q", caption, oldFileDeletedCaption)
	}
	assertNames(t, text, "the outcome", []string{"The old file was deleted.", target})

	// A refusal is reported as the fact it is: the cause on its own line
	// beside the very file that could not be deleted, and never as a
	// deletion that happened.
	restoreOldFileRemover(t, func(path string) error {
		return errors.New("Access is denied.")
	})
	caption, text, failed = runDeleteOldFileCommand(target)
	if !failed {
		t.Fatal("the refused deletion reported a success")
	}
	if caption != oldFileDeleteFailedCaption {
		t.Errorf("the failure is captioned %q, want %q", caption, oldFileDeleteFailedCaption)
	}
	assertNames(t, text, "the failure", []string{
		"The old file could not be deleted.",
		"Access is denied.",
		"The file is at " + target + ".",
	})
	assertNamesWithout(t, text, "the failure", nil, []string{"The old file was deleted."})
}
