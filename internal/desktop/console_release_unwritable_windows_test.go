// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import "golang.org/x/sys/windows"

// denyNewFiles refuses the creation of new files in a folder.
//
// Windows does not honour the read-only mode bits of a directory: a folder
// made read-only by chmod still accepts new files inside it, so the
// unwritable-folder premise has to be expressed as access control. The deny
// entry refuses FILE_WRITE_DATA and FILE_APPEND_DATA (0x6) — the two rights
// a new directory entry needs — for everyone before the allow entries apply,
// while reading, listing, changing the mode and deleting stay allowed so the
// test can still inspect the folder and the temporary directory can still be
// removed afterwards.
func denyNewFiles(folder string) error {
	sd, err := windows.SecurityDescriptorFromString(
		"D:P(D;;0x00000006;;;WD)(A;;FA;;;SY)(A;;FA;;;BA)(A;;FA;;;WD)")
	if err != nil {
		return err
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		return err
	}
	return windows.SetNamedSecurityInfo(
		folder,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		dacl,
		nil,
	)
}
