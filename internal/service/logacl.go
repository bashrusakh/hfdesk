// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package service

import "fmt"

// This file holds the part of the log read grant (logacl_windows.go) that is
// not a Windows detail: the descriptor string of the entry the grant appends
// and the identifier of its trustee. The platform file owns the access list
// itself; this string builds on every platform, so a run of
// `go test ./internal/service` on any of them guards it — including the class
// of defect it exists for, an entry list without the section the platform's
// parser requires.

// scmInteractiveUsersSID is the well-known identifier of the Interactive
// group: the users logged in at the console, spelled IU in a descriptor
// string. It is declared beside the string that names it, so the trustee of
// the appended entry and the trustee the tests read are the same identifier.
const scmInteractiveUsersSID = "S-1-5-4"

// scmLogReadGrantSDDL returns the security descriptor string of the single
// entry the log read grant appends: access allowed, read access
// (FILE_GENERIC_READ, FR) and the Interactive group as the trustee. On a log
// directory the entry inherits (OICI), so the active file the sink creates
// next receives it at creation and a rotated file carries it along; on a log
// file the entry applies to that file alone.
//
// The "D:" section is not decoration. The platform parses a descriptor string
// with ConvertStringSecurityDescriptorToSecurityDescriptorW, which requires at
// least one section and refuses a bare entry list with ERROR_INVALID_PARAMETER
// — "The parameter is incorrect", reported at every start of an instance whose
// log then stays unreadable by interactive users.
func scmLogReadGrantSDDL(container bool) string {
	inherits := ""
	if container {
		inherits = "OICI"
	}
	return fmt.Sprintf("D:(A;%s;FR;;;%s)", inherits, scmInteractiveUsersSID)
}
