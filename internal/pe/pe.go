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
	dos := make([]byte, dosHeaderSize)
	if err := readAt(r, 0, dos); err != nil {
		return 0, fmt.Errorf("read the DOS header: %w", err)
	}
	if dos[dosSignatureOffset] != 'M' || dos[dosSignatureOffset+1] != 'Z' {
		return 0, errors.New("the file is not a PE image: it has no MZ signature")
	}

	// The offset of the PE header is a field of the DOS header, so it is
	// read as an unsigned value: a file that points outside of itself is
	// reported by the read of the header below.
	peOffset := int64(binary.LittleEndian.Uint32(dos[dosHeaderOffsetField : dosHeaderOffsetField+4]))

	header := make([]byte, peSignatureSize)
	if err := readAt(r, peOffset, header); err != nil {
		return 0, fmt.Errorf("read the PE header: %w", err)
	}
	if !hasPESignature(header) {
		return 0, errors.New("the file is not a PE image: it has no PE signature")
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
