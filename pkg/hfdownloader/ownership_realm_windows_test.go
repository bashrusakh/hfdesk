// Copyright 2025
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package hfdownloader

import (
	"errors"
	"fmt"
	"syscall"
	"testing"
	"unsafe"

	"golang.org/x/sys/windows"
)

var ownershipCompareStringOrdinal = syscall.NewLazyDLL("kernel32.dll").NewProc("CompareStringOrdinal")

func windowsOrdinalEqual(a, b string) (bool, error) {
	a16, err := syscall.UTF16FromString(a)
	if err != nil {
		return false, err
	}
	b16, err := syscall.UTF16FromString(b)
	if err != nil {
		return false, err
	}
	result, _, callErr := ownershipCompareStringOrdinal.Call(
		uintptr(unsafe.Pointer(&a16[0])), uintptr(len(a16)-1),
		uintptr(unsafe.Pointer(&b16[0])), uintptr(len(b16)-1), 1,
	)
	if result == 0 {
		return false, callErr
	}
	return result == 2, nil
}

func TestOwnershipWindowsCaseKeyMatchesOrdinalComparison(t *testing.T) {
	parent := t.TempDir()
	for _, pair := range [][2]string{{"Model.bin", "model.BIN"}, {"Étage", "étage"}} {
		want, err := windowsOrdinalEqual(pair[0], pair[1])
		if err != nil {
			t.Fatal(err)
		}
		left, err := ownershipComponentKey(parent, pair[0])
		if err != nil {
			t.Fatal(err)
		}
		right, err := ownershipComponentKey(parent, pair[1])
		if err != nil {
			t.Fatal(err)
		}
		if (left == right) != want {
			t.Errorf("key equality for %q and %q = %v, CompareStringOrdinal = %v", pair[0], pair[1], left == right, want)
		}
	}
	left, err := ownershipComponentKey(parent, "foo")
	if err != nil {
		t.Fatal(err)
	}
	right, err := ownershipComponentKey(parent, "foo.incomplete.lock")
	if err != nil {
		t.Fatal(err)
	}
	if left == right {
		t.Fatal("suffix-shaped payload names share an ownership component key")
	}
}

func TestOwnershipWindowsCaseSensitiveParentKeepsCaseDistinct(t *testing.T) {
	parent := t.TempDir()
	p, err := windows.UTF16PtrFromString(parent)
	if err != nil {
		t.Fatal(err)
	}
	h, err := windows.CreateFile(
		p,
		windows.FILE_READ_ATTRIBUTES|windows.FILE_WRITE_ATTRIBUTES,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil,
		windows.OPEN_EXISTING,
		windows.FILE_FLAG_BACKUP_SEMANTICS,
		0,
	)
	if err != nil {
		t.Fatalf("open case-sensitivity test directory: %v", err)
	}
	defer windows.CloseHandle(h)
	info := ownershipFileCaseSensitiveInfo{Flags: windows.FILE_CS_FLAG_CASE_SENSITIVE_DIR}
	err = windows.SetFileInformationByHandle(
		h,
		windows.FileCaseSensitiveInfo,
		(*byte)(unsafe.Pointer(&info)),
		uint32(unsafe.Sizeof(info)),
	)
	if err != nil {
		if errors.Is(err, windows.ERROR_INVALID_PARAMETER) || errors.Is(err, windows.ERROR_INVALID_FUNCTION) || errors.Is(err, windows.ERROR_NOT_SUPPORTED) {
			t.Skipf("filesystem does not support per-directory case sensitivity: %v", err)
		}
		t.Fatalf("enable case-sensitive directory: %v", err)
	}
	left, err := ownershipComponentKey(parent, "Foo")
	if err != nil {
		t.Fatal(err)
	}
	right, err := ownershipComponentKey(parent, "foo")
	if err != nil {
		t.Fatal(err)
	}
	if left == right {
		t.Fatal(fmt.Sprintf("case-sensitive parent collapsed distinct keys: %q", left))
	}
}
