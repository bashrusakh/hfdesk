// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"strconv"
	"testing"
	"unsafe"
)

// TestShellExecuteInfoMatchesTheNativeLayout pins the layout of
// shellExecuteInfo to the native SHELLEXECUTEINFOW of the running
// architecture: 64-bit pointers align to 8 bytes and insert padding after
// nShow and dwHotKey, 32-bit pointers align to 4 bytes and the native
// structure has no padding there. cbSize is unsafe.Sizeof at the call site,
// so both the size and the field offsets have to be the native ones — a
// spelled-out padding field shifts hProcess on 386 and the shell would write
// the started process handle into the wrong slot.
func TestShellExecuteInfoMatchesTheNativeLayout(t *testing.T) {
	var info shellExecuteInfo

	wantSize, wantNShow, wantHInstApp, wantDwHotKey, wantHProcess := uintptr(112), uintptr(48), uintptr(56), uintptr(88), uintptr(104)
	if strconv.IntSize == 32 {
		wantSize, wantNShow, wantHInstApp, wantDwHotKey, wantHProcess = 60, 28, 32, 48, 56
	}
	arch := strconv.Itoa(strconv.IntSize) + "-bit"

	for _, tc := range []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"size of the structure", unsafe.Sizeof(info), wantSize},
		{"offset of cbSize", unsafe.Offsetof(info.cbSize), 0},
		{"offset of fMask", unsafe.Offsetof(info.fMask), 4},
		{"offset of nShow", unsafe.Offsetof(info.nShow), wantNShow},
		{"offset of hInstApp", unsafe.Offsetof(info.hInstApp), wantHInstApp},
		{"offset of dwHotKey", unsafe.Offsetof(info.dwHotKey), wantDwHotKey},
		{"offset of hProcess", unsafe.Offsetof(info.hProcess), wantHProcess},
	} {
		if tc.got != tc.want {
			t.Errorf("%s = %d, want %d (%s)", tc.name, tc.got, tc.want, arch)
		}
	}

	// The value cbSize is filled in with has to be the native size of this
	// architecture, which the code derives from the layout itself.
	if got := uint32(unsafe.Sizeof(shellExecuteInfo{})); got != uint32(wantSize) {
		t.Errorf("cbSize would be %d, want the native size %d (%s)", got, wantSize, arch)
	}
}
