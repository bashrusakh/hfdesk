// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

// Package pe reads the headers of a Windows PE image.
//
// The subsystem of a PE image is a build time property of the linker: a
// console subsystem image (3) opens a console window, a GUI subsystem image
// (2) does not. HFDesk ships two Windows binaries, the console host and the
// desktop host, and the difference between them has to be a fact of the
// produced file instead of a promise of the build script. The CI check and
// the desktop host therefore read the subsystem out of the PE header of the
// file they are given.
//
// The machine type of the image is read as well: a service installation
// registers a program for the architecture of the machine it runs on, and a
// binary of another architecture has to be refused before a running instance
// is touched.
//
// The package only reads: it never runs a program and it never writes
// anything. It works on every platform, so a build check runs on the machine
// that built the artifact and the desktop host can read its own executable.
package pe

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
)

// The subsystems of a PE image that HFDesk builds.
const (
	// SubsystemWindowsGUI is IMAGE_SUBSYSTEM_WINDOWS_GUI: the image has no
	// console window.
	SubsystemWindowsGUI = 2
	// SubsystemWindowsCUI is IMAGE_SUBSYSTEM_WINDOWS_CUI: the image runs
	// in a console window.
	SubsystemWindowsCUI = 3
)

// The offsets of the fields of the headers that are read.
const (
	// dosHeaderSize is the size of the DOS header, the part of the file
	// that precedes the PE header.
	dosHeaderSize = 64
	// dosSignatureOffset is the offset of the "MZ" signature.
	dosSignatureOffset = 0
	// dosHeaderOffsetField is the offset of e_lfanew, the offset of the
	// PE header.
	dosHeaderOffsetField = 0x3C
	// peSignatureSize is the size of the PE signature and of the COFF
	// header that follows it.
	peSignatureSize = 24
	// peSignatureOffset is the offset of the "PE\0\0" signature inside
	// the PE header.
	peSignatureOffset = 0
	// machineOffset is the offset of the machine field inside the PE
	// header: the machine type is the first field of the COFF header,
	// which follows the four bytes of the PE signature.
	machineOffset = peSignatureOffset + 4
	// machineSize is the size of the machine field.
	machineSize = 2
	// optionalHeaderOffset is the offset of the optional header inside
	// the PE header.
	optionalHeaderOffset = 24
	// optionalHeaderMagicSize is the size of the magic field of the
	// optional header.
	optionalHeaderMagicSize = 2
	// subsystemOffset is the offset of the subsystem field inside the
	// optional header.
	//
	// The offset is the same for PE32 and PE32+: the four bytes of
	// BaseOfData that PE32 carries between BaseOfCode and ImageBase are
	// balanced by the four extra bytes of the eight byte ImageBase of
	// PE32+, so every field behind ImageBase starts at the same offset in
	// both layouts.
	subsystemOffset = 68
	// subsystemSize is the size of the subsystem field.
	subsystemSize = 2
)

// The machine types of the Windows images the package names.
const (
	// MachineAMD64 is IMAGE_FILE_MACHINE_AMD64: an image for 64 bit x86.
	MachineAMD64 = 0x8664
	// MachineARM64 is IMAGE_FILE_MACHINE_ARM64: an image for 64 bit ARM.
	MachineARM64 = 0xAA64
	// MachineI386 is IMAGE_FILE_MACHINE_I386: an image for 32 bit x86.
	MachineI386 = 0x014C
)

// The magic values of the optional header.
const (
	// magicPE32 marks the 32 bit optional header.
	magicPE32 = 0x10B
	// magicPE32Plus marks the 64 bit optional header.
	magicPE32Plus = 0x20B
)

// Subsystem returns the subsystem of the PE image at path.
//
// It returns an error when the file cannot be read or when it is not a PE
// image.
func Subsystem(path string) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()

	return SubsystemOf(file)
}

// SubsystemOf reads the subsystem out of the headers of a PE image.
//
// It returns an error when the image is truncated or when it is not a PE
// image at all: a file that carries no MZ signature, no PE signature or an
// unknown optional header magic is not a PE image this package can judge.
func SubsystemOf(r io.ReaderAt) (int, error) {
	peOffset, _, err := readPEHeader(r)
	if err != nil {
		return 0, err
	}

	optional := make([]byte, optionalHeaderMagicSize)
	if err := readAt(r, peOffset+optionalHeaderOffset, optional); err != nil {
		return 0, fmt.Errorf("read the optional header: %w", err)
	}
	magic := binary.LittleEndian.Uint16(optional)
	if magic != magicPE32 && magic != magicPE32Plus {
		return 0, fmt.Errorf("the file is not a PE image: the optional header has the unknown magic 0x%04X", magic)
	}

	field := make([]byte, subsystemSize)
	if err := readAt(r, peOffset+optionalHeaderOffset+subsystemOffset, field); err != nil {
		return 0, fmt.Errorf("read the subsystem: %w", err)
	}
	return int(binary.LittleEndian.Uint16(field)), nil
}

// Machine returns the machine type of the PE image at path: the architecture
// the image was built for, e.g. MachineAMD64.
//
// It returns an error when the file cannot be read or when it is not a PE
// image.
func Machine(path string) (uint16, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()

	return MachineOf(file)
}

// MachineOf reads the machine type out of the headers of a PE image.
//
// It returns an error when the image is truncated or when it is not a PE
// image at all: a file that carries no MZ signature or no PE signature is not
// a PE image this package can judge.
//
// The machine type is a field of the COFF header and not of the optional
// header, so an image whose optional header this package cannot judge still
// names the architecture it was built for.
func MachineOf(r io.ReaderAt) (uint16, error) {
	_, header, err := readPEHeader(r)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(header[machineOffset : machineOffset+machineSize]), nil
}

// SubsystemName returns the name of a subsystem.
func SubsystemName(value int) string {
	switch value {
	case SubsystemWindowsGUI:
		return "IMAGE_SUBSYSTEM_WINDOWS_GUI"
	case SubsystemWindowsCUI:
		return "IMAGE_SUBSYSTEM_WINDOWS_CUI"
	default:
		return "unknown subsystem"
	}
}

// readPEHeader returns the offset of the PE header of an image and the PE
// signature and COFF header it carries.
//
// It returns an error when the image is truncated or when it is not a PE
// image at all. Both readers of the package locate the header here, so a file
// this package cannot judge is refused the same way whichever field is read.
func readPEHeader(r io.ReaderAt) (int64, []byte, error) {
	dos := make([]byte, dosHeaderSize)
	if err := readAt(r, 0, dos); err != nil {
		return 0, nil, fmt.Errorf("read the DOS header: %w", err)
	}
	if dos[dosSignatureOffset] != 'M' || dos[dosSignatureOffset+1] != 'Z' {
		return 0, nil, errors.New("the file is not a PE image: it has no MZ signature")
	}

	// The offset of the PE header is a field of the DOS header, so it is
	// read as an unsigned value: a file that points outside of itself is
	// reported by the read of the header below.
	peOffset := int64(binary.LittleEndian.Uint32(dos[dosHeaderOffsetField : dosHeaderOffsetField+4]))

	header := make([]byte, peSignatureSize)
	if err := readAt(r, peOffset, header); err != nil {
		return 0, nil, fmt.Errorf("read the PE header: %w", err)
	}
	if !hasPESignature(header) {
		return 0, nil, errors.New("the file is not a PE image: it has no PE signature")
	}
	return peOffset, header, nil
}

// hasPESignature reports whether the header carries the PE signature.
func hasPESignature(header []byte) bool {
	return len(header) >= peSignatureSize &&
		header[peSignatureOffset] == 'P' &&
		header[peSignatureOffset+1] == 'E' &&
		header[peSignatureOffset+2] == 0 &&
		header[peSignatureOffset+3] == 0
}

// readAt fills buf with the bytes at the given offset.
//
// A short read is an error: a file that ends inside a header is not a PE
// image this package can judge.
func readAt(r io.ReaderAt, offset int64, buf []byte) error {
	n, err := r.ReadAt(buf, offset)
	if err != nil && !errors.Is(err, io.EOF) {
		return err
	}
	if n < len(buf) {
		return io.ErrUnexpectedEOF
	}
	return nil
}
