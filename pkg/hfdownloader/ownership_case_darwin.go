// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build darwin

package hfdownloader

import (
	"encoding/binary"
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

const (
	darwinVolumeCaseSensitive = 0x00000100
	darwinAttrBufferSize      = 36 // 4-byte length + vol_capabilities_attr_t
)

type darwinAttrList struct {
	bitmapCount uint16
	reserved    uint16
	commonAttr  uint32
	volAttr     uint32
	dirAttr     uint32
	fileAttr    uint32
	forkAttr    uint32
}

func ownershipDirectoryCaseProfile(path, _ string) (ownershipCaseProfile, error) {
	var attrs darwinAttrList
	attrs.bitmapCount = 5
	attrs.volAttr = unix.ATTR_VOL_INFO | unix.ATTR_VOL_CAPABILITIES
	name, err := unix.BytePtrFromString(path)
	if err != nil {
		return ownershipCaseProfile{}, err
	}
	var buffer [darwinAttrBufferSize]byte
	_, _, errno := unix.Syscall6(
		unix.SYS_GETATTRLIST,
		uintptr(unsafe.Pointer(name)),
		uintptr(unsafe.Pointer(&attrs)),
		uintptr(unsafe.Pointer(&buffer[0])),
		uintptr(len(buffer)),
		0,
		0,
	)
	if errno != 0 {
		return ownershipCaseProfile{}, fmt.Errorf("%w: getattrlist volume capabilities for %q: %v", errOwnershipCaseProfileUnavailable, path, errno)
	}
	if binary.LittleEndian.Uint32(buffer[0:4]) < uint32(len(buffer)) {
		return ownershipCaseProfile{}, fmt.Errorf("%w: incomplete volume capabilities for %q", errOwnershipCaseProfileUnavailable, path)
	}
	capabilities := binary.LittleEndian.Uint32(buffer[4:8])
	valid := binary.LittleEndian.Uint32(buffer[20:24])
	if valid&darwinVolumeCaseSensitive == 0 {
		return ownershipCaseProfile{}, fmt.Errorf("%w: volume does not report case sensitivity for %q", errOwnershipCaseProfileUnavailable, path)
	}
	return ownershipCaseProfile{
		Version:   ownershipNodeFormatVersion,
		Provider:  "darwin-getattrlist-volume-capabilities-v1",
		Sensitive: capabilities&darwinVolumeCaseSensitive != 0,
	}, nil
}

func ownershipControlDirectoryCaseProfile(path, controlProbe string) (ownershipCaseProfile, error) {
	return ownershipDirectoryCaseProfile(path, controlProbe)
}

func setOwnershipControlDirectoryCaseProfile(_ string, profile ownershipCaseProfile) error {
	if profile.Provider != "darwin-getattrlist-volume-capabilities-v1" {
		return errOwnershipCaseProfileUnavailable
	}
	return nil
}
