// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"fmt"
	"os"
	"unsafe"

	"golang.org/x/sys/windows"
)

var (
	// user32 is the Windows user interface library.
	user32 = windows.NewLazySystemDLL("user32.dll")

	// procLoadImageW loads an image from a file.
	procLoadImageW = user32.NewProc("LoadImageW")
	// procSendMessageW sends a message to a window.
	procSendMessageW = user32.NewProc("SendMessageW")
	// procDestroyIcon releases an icon handle.
	procDestroyIcon = user32.NewProc("DestroyIcon")
	// procShowWindow shows or hides a window.
	procShowWindow = user32.NewProc("ShowWindow")
	// procSetForegroundWindow gives a window the focus.
	procSetForegroundWindow = user32.NewProc("SetForegroundWindow")
)

// Constants of the icon and window handling.
const (
	// imageIcon tells LoadImageW to load an icon.
	imageIcon = 1
	// lrLoadFromFile tells LoadImageW to load the image from a file.
	lrLoadFromFile = 0x00000010
	// wmSetIcon is the message that sets the icon of a window.
	wmSetIcon = 0x0080
	// iconBig is the large icon of a window.
	iconBig = 1
	// iconSmall is the small icon of a window.
	iconSmall = 0
	// smallIconSize is the size of the small icon of the system. It is
	// the size the shell renders the icon of a window with.
	smallIconSize = 16
)

// windowIcons holds the icon handles of a window.
//
// The handles stay alive as long as the window uses them, so they are
// released together with the window.
type windowIcons struct {
	// big is the large icon of the window.
	big windows.Handle
	// small is the small icon of the window.
	small windows.Handle
}

// applyWindowIcon sets the icon of a window from the icon bytes.
//
// The icon is not read from a resource of the executable: the resource id
// depends on the order the resources were numbered in when the binary was
// linked, so it is not a stable value that could be hard coded. The bytes
// are written to a temporary file, loaded with LoadImageW and the file is
// removed again.
func applyWindowIcon(hwnd windows.HWND, iconBytes []byte) (windowIcons, error) {
	if len(iconBytes) == 0 {
		return windowIcons{}, fmt.Errorf("desktop: the icon is empty")
	}

	path, err := writeTemporaryIcon(iconBytes)
	if err != nil {
		return windowIcons{}, err
	}

	pathPtr, err := windows.UTF16PtrFromString(path)
	if err != nil {
		_ = os.Remove(path)
		return windowIcons{}, fmt.Errorf("desktop: build the path of the window icon: %w", err)
	}

	// The large icon is loaded at the default icon size of the system,
	// the small one at the size of the small icon, so the window shows
	// the image of the icon that fits.
	big, err := loadIcon(pathPtr, 0, 0)
	if err != nil {
		_ = os.Remove(path)
		return windowIcons{}, err
	}
	small, err := loadIcon(pathPtr, smallIconSize, smallIconSize)
	if err != nil {
		destroyIcon(big)
		_ = os.Remove(path)
		return windowIcons{}, err
	}

	// The temporary file is not needed anymore: the icons are in memory
	// now.
	_ = os.Remove(path)

	procSendMessageW.Call(uintptr(hwnd), wmSetIcon, iconBig, uintptr(big))
	procSendMessageW.Call(uintptr(hwnd), wmSetIcon, iconSmall, uintptr(small))
	return windowIcons{big: big, small: small}, nil
}

// writeTemporaryIcon writes the icon bytes to a temporary file and returns
// its path.
func writeTemporaryIcon(iconBytes []byte) (string, error) {
	file, err := os.CreateTemp("", "hfdesk-icon-*.ico")
	if err != nil {
		return "", fmt.Errorf("desktop: write the window icon: %w", err)
	}
	path := file.Name()
	if _, err := file.Write(iconBytes); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", fmt.Errorf("desktop: write the window icon: %w", err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(path)
		return "", fmt.Errorf("desktop: write the window icon: %w", err)
	}
	return path, nil
}

// loadIcon loads an icon from a file with the given size.
//
// A size of zero lets the system pick the default icon size.
func loadIcon(path *uint16, width, height uint32) (windows.Handle, error) {
	handle, _, callErr := procLoadImageW.Call(
		0,
		uintptr(unsafe.Pointer(path)),
		imageIcon,
		uintptr(width),
		uintptr(height),
		lrLoadFromFile,
	)
	if handle == 0 {
		return 0, fmt.Errorf("desktop: load the window icon: %w", callErr)
	}
	return windows.Handle(handle), nil
}

// destroy releases the icon handles of a window.
func (i windowIcons) destroy() {
	destroyIcon(i.big)
	destroyIcon(i.small)
}

// destroyIcon releases an icon handle.
func destroyIcon(handle windows.Handle) {
	if handle != 0 {
		procDestroyIcon.Call(uintptr(handle))
	}
}

// showWindow makes a window visible and gives it the focus.
func showWindow(hwnd windows.HWND) {
	procShowWindow.Call(uintptr(hwnd), uintptr(windows.SW_SHOW))
	procSetForegroundWindow.Call(uintptr(hwnd))
}

// hideWindow hides a window without destroying it.
func hideWindow(hwnd windows.HWND) {
	procShowWindow.Call(uintptr(hwnd), uintptr(windows.SW_HIDE))
}
