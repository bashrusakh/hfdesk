// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package service

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io/fs"
	"strings"
	"unsafe"

	"golang.org/x/sys/windows"
)

// grantLogReadToInteractiveUsers gives interactive users read access to one
// path of the log: the log directory of the instance or its active file.
//
// The requirement it serves: a log a window names must be readable from
// outside the window. The installation gives the directories it creates the
// restrictive descriptor of the instance (scmStateSDDL: full control for
// SYSTEM, the administrators and LocalService, nobody else), and the log file
// the service writes inherits exactly that descriptor — so an interactive
// user, whose token carries none of those three trustees, is refused the very
// file the Logs window and its open-the-log-file action name.
//
// The grant is deliberately narrow:
//
//   - It changes a descriptor only when that descriptor is still exactly the
//     restrictive list of the instance: three access-allowed entries, full
//     control for SYSTEM, the administrators and LocalService, nothing else.
//     That list is the one this code applied at installation, so it is the
//     one this code may widen. Every other descriptor — a log directory the
//     user named, a list somebody already widened or locked down further,
//     the state and config descriptors of the instance, the profile of the
//     desktop host — is left byte for byte alone. The config and state
//     policy (the owner-only family) is not touched: this function is only
//     ever called with the log directory and the active log file.
//   - The one thing it adds is read access for the INTERACTIVE well-known
//     SID — the users logged in at the console. Nothing is removed and
//     nothing is granted to anybody else.
//   - On a directory the entry inherits (OICI), so the file the sink creates
//     next receives it at creation and a rotated file carries it along; the
//     existing active file is granted directly, so a file written before
//     this grant existed does not wait for its next rotation.
//
// The descriptor stays protected: the list is rebuilt from the entries that
// are already there plus the one new entry, so the installation's decision to
// cut the directory off from its parent keeps holding.
//
// The grant runs at every start of the instance, so an installation that
// already exists repairs itself at its next start, and it is idempotent: a
// list that already carries the grant (or any other reader beside the
// instance accounts) is no longer the instance's untouched restrictive list
// and is left alone.
//
// An error is returned to the caller and never stops the instance: the
// window's honest one-line "could not be read" stays the path for a refusal
// this grant could not repair.
func grantLogReadToInteractiveUsers(path string, container bool) error {
	if path == "" {
		return nil
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.DACL_SECURITY_INFORMATION)
	if err != nil {
		if errors.Is(err, fs.ErrNotExist) {
			// The active file does not exist before its first write: it
			// inherits the grant of the directory when it is created.
			return nil
		}
		return err
	}
	if sd == nil {
		// The object exists but carries no security descriptor: nothing
		// restricts the log.
		return nil
	}
	dacl, _, err := sd.DACL()
	if err != nil {
		if errors.Is(err, windows.ERROR_OBJECT_NOT_FOUND) {
			// A descriptor without an access list restricts nobody.
			return nil
		}
		return err
	}
	if dacl == nil || !instanceStateDACLIsUnchanged(dacl) {
		return nil
	}
	grant, err := windows.SecurityDescriptorFromString(scmLogReadGrantSDDL(container))
	if err != nil {
		return fmt.Errorf("service: build the log access entry: %w", err)
	}
	grantList, _, err := grant.DACL()
	if err != nil {
		return fmt.Errorf("service: read the log access entry: %w", err)
	}
	merged, err := appendACLEntry(dacl, grantList)
	if err != nil {
		return err
	}
	if err := windows.SetNamedSecurityInfo(
		path,
		windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil,
		nil,
		merged,
		nil,
	); err != nil {
		return fmt.Errorf("service: give interactive users read access to %q: %w", path, err)
	}
	return nil
}

// instanceStateDACLIsUnchanged reports whether the access list of a path is
// exactly the restrictive list the installation applies to the paths it
// creates: three access-allowed entries — full control for SYSTEM, the
// administrators of the machine and LocalService — and nothing else.
//
// The comparison is structural (entry type, access mask, trustee), never a
// spelling of the list: the platform writes the same three grants differently
// on a directory and on a file below it (the entries of the file carry the
// inherited marker), and it writes a trustee either as its alias or as its
// raw identifier. Any other entry — a deny, a wider grant, an entry for
// anybody else — makes the descriptor one this function does not own, and
// the caller leaves it alone.
func instanceStateDACLIsUnchanged(acl *windows.ACL) bool {
	raw := accessListBytes(acl)
	if len(raw) < accessListHeaderSize {
		return false
	}
	count := binary.LittleEndian.Uint16(raw[4:6])
	if count != 3 {
		return false
	}
	seen := make(map[string]bool, 3)
	offset := accessListHeaderSize
	for i := uint16(0); i < count; i++ {
		if offset+8 > len(raw) {
			return false
		}
		size := int(binary.LittleEndian.Uint16(raw[offset+2 : offset+4]))
		if size < 8+4 || offset+size > len(raw) {
			return false
		}
		// Only access-allowed entries: a deny entry (or any other entry
		// type) means somebody else administers this descriptor.
		if raw[offset] != 0 {
			return false
		}
		if mask := binary.LittleEndian.Uint32(raw[offset+4 : offset+8]); mask != scmFileAllAccess {
			return false
		}
		trustee, ok := securityIdentifierString(raw[offset+8 : offset+size])
		if !ok {
			return false
		}
		seen[trustee] = true
		offset += size
	}
	return offset == len(raw) &&
		len(seen) == 3 &&
		seen["S-1-5-18"] && seen["S-1-5-32-544"] && seen["S-1-5-19"]
}

// accessListHeaderSize is the size of the fixed header of a Windows access
// list: the revision byte, a filler byte, the list size, the entry count and
// a second filler field.
const accessListHeaderSize = 8

// accessListBytes returns the raw bytes of an access list — the fixed header
// followed by its entries. The list size lives in an unexported field of
// windows.ACL, so the header layout is read from the bytes the platform
// returned; the layout is the fixed eight byte ACL header of Windows.
func accessListBytes(acl *windows.ACL) []byte {
	if acl == nil {
		return nil
	}
	header := unsafe.Slice((*byte)(unsafe.Pointer(acl)), accessListHeaderSize)
	size := int(binary.LittleEndian.Uint16(header[2:4]))
	if size < accessListHeaderSize {
		return nil
	}
	return unsafe.Slice((*byte)(unsafe.Pointer(acl)), size)
}

// appendACLEntry returns an access list with every entry of the given list
// followed by the single entry of the added list. The bytes of the existing
// entries are copied unchanged; only the size and the entry count of the
// header of the new list are written.
func appendACLEntry(list, added *windows.ACL) (*windows.ACL, error) {
	raw := accessListBytes(list)
	addedRaw := accessListBytes(added)
	if len(raw) < accessListHeaderSize || len(addedRaw) < accessListHeaderSize {
		return nil, errors.New("service: an access list of the log could not be read")
	}
	if binary.LittleEndian.Uint16(addedRaw[4:6]) != 1 {
		return nil, errors.New("service: the read entry of the log is not a single access entry")
	}
	merged := make([]byte, 0, len(raw)+len(addedRaw)-accessListHeaderSize)
	merged = append(merged, raw...)
	merged = append(merged, addedRaw[accessListHeaderSize:]...)
	binary.LittleEndian.PutUint16(merged[2:4], uint16(len(merged)))
	binary.LittleEndian.PutUint16(merged[4:6], binary.LittleEndian.Uint16(raw[4:6])+1)
	return (*windows.ACL)(unsafe.Pointer(&merged[0])), nil
}

// securityIdentifierString renders the bytes of a security identifier — the
// identifier structure that follows the access mask of an entry — in its
// S-1-… form.
func securityIdentifierString(raw []byte) (string, bool) {
	if len(raw) < 8 {
		return "", false
	}
	subAuthorities := int(raw[1])
	if subAuthorities > (len(raw)-8)/4 {
		return "", false
	}
	var authority uint64
	for _, b := range raw[2:8] {
		authority = authority<<8 | uint64(b)
	}
	var sb strings.Builder
	fmt.Fprintf(&sb, "S-%d-%d", raw[0], authority)
	for i := 0; i < subAuthorities; i++ {
		fmt.Fprintf(&sb, "-%d", binary.LittleEndian.Uint32(raw[8+4*i:]))
	}
	return sb.String(), true
}
