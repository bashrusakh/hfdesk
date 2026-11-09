// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"strings"
	"testing"

	"github.com/bashrusakh/hfdesk/internal/pe"
)

// TestSubsystemWarningNamesTheMissingFlagAndTheRebuild proves that the
// warning tells the user what is wrong with the build, what the user sees
// because of it and how the binary is rebuilt correctly.
func TestSubsystemWarningNamesTheMissingFlagAndTheRebuild(t *testing.T) {
	warning := subsystemWarning(pe.SubsystemWindowsCUI)

	for _, want := range []string{
		"-H=windowsgui",
		"console window",
		"go build -ldflags",
		"./cmd/hfdesk-desktop",
	} {
		if !strings.Contains(warning, want) {
			t.Errorf("warning = %q, want it to mention %q", warning, want)
		}
	}
}

// TestSubsystemWarningNamesTheSubsystemOfTheBuild proves that the warning
// names the subsystem of the build that is running, so a support record
// carries the fact that the binary was built wrong.
func TestSubsystemWarningNamesTheSubsystemOfTheBuild(t *testing.T) {
	warning := subsystemWarning(pe.SubsystemWindowsCUI)
	if !strings.Contains(warning, pe.SubsystemName(pe.SubsystemWindowsCUI)) {
		t.Errorf("warning = %q, want it to name the subsystem of the build", warning)
	}
}

// TestSubsystemWarningIsEmptyForTheGUISubsystem proves that a binary that
// was built with the GUI subsystem is not reported: the check only speaks
// up when the build is wrong.
func TestSubsystemWarningIsEmptyForTheGUISubsystem(t *testing.T) {
	if warning := subsystemWarning(pe.SubsystemWindowsGUI); warning != "" {
		t.Errorf("warning = %q, want it to stay empty for the GUI subsystem", warning)
	}
}
