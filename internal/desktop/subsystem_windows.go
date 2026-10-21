// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"os"

	"github.com/bashrusakh/hfdesk/internal/pe"
	"golang.org/x/sys/windows"
)

// checkSubsystem reports a binary that was not linked with the Windows GUI
// subsystem.
//
// The GUI subsystem is a build time property of the linker: a desktop host
// that was built without -H=windowsgui is a console subsystem program and
// opens a console window next to the notification area icon. The check reads
// the PE header of the running executable and tells the user what is wrong
// with the build and how to rebuild it.
//
// The check is cheap: it reads the header of one small file. It reports the
// problem and nothing else: a mis-built binary is still a working
// application, so the host starts and keeps running.
func checkSubsystem() {
	path, err := os.Executable()
	if err != nil {
		desktopLog.Warn("could not read the path of the running executable", "error", err)
		return
	}

	subsystem, err := pe.Subsystem(path)
	if err != nil {
		desktopLog.Warn("could not read the PE subsystem of the running executable",
			"executable", path, "error", err)
		return
	}

	warning := subsystemWarning(subsystem)
	if warning == "" {
		return
	}

	desktopLog.Warn("the desktop host was not built with the Windows GUI subsystem",
		"executable", path,
		"subsystem", subsystem,
		"subsystemName", pe.SubsystemName(subsystem))
	showMessage("HFDesk - this build opens a console window", warning, windows.MB_ICONWARNING)
}
