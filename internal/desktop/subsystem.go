// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"github.com/bashrusakh/hfdesk/internal/pe"
)

// subsystemWarning returns the warning shown to the user when the running
// binary is not a GUI subsystem image, and an empty string when it is.
//
// The warning names the missing linker flag, the visible consequence of the
// missing flag and the command that rebuilds the binary correctly, so the
// user can fix the build without reading the source.
func subsystemWarning(subsystem int) string {
	if subsystem == pe.SubsystemWindowsGUI {
		return ""
	}

	return "This build of HFDesk was not linked with the Windows GUI subsystem (-H=windowsgui), " +
		"so a console window is visible next to the notification area icon.\n\n" +
		"Rebuild the desktop host with the GUI subsystem:\n\n" +
		"    go build -ldflags \"-H=windowsgui\" -o hfdesk-desktop.exe ./cmd/hfdesk-desktop\n\n" +
		"HFDesk keeps running. The subsystem of this build is " + pe.SubsystemName(subsystem) + "."
}
