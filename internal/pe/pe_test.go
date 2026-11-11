// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package pe

import (
	"bytes"
	"encoding/binary"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// The layout of the synthesized PE image.
const (
	// testPeHeaderOffset is the offset of the PE header in the
	// synthesized image.
	testPeHeaderOffset = 0x80
	// testSectionHeaderSize is the size of one section header.
	testSectionHeaderSize = 40
	// testSectionRawOffset is the aligned location of the synthesized section.
	testSectionRawOffset = 0x200
	// testSectionRawSize is the size of the synthesized section data.
	testSectionRawSize = 0x200
	// testOptionalHeaderSize32 is the size of the optional header of a
	// PE32 image.
	testOptionalHeaderSize32 = 224
	// testOptionalHeaderSize64 is the size of the optional header of a
	// PE32+ image.
	testOptionalHeaderSize64      = 240
	testOptionalHeaderFixedSize32 = 96
	testOptionalHeaderFixedSize64 = 112
)

// TestSubsystemReadsThePE32OptionalHeader proves that the subsystem is read
// out of a 32 bit PE image.
func TestSubsystemReadsThePE32OptionalHeader(t *testing.T) {
	image := buildPEImage(SubsystemWindowsGUI, magicPE32, testOptionalHeaderSize32)

	subsystem, err := SubsystemOf(bytes.NewReader(image))
	assertNoError(t, err, "SubsystemOf: %v", err)
	if subsystem != SubsystemWindowsGUI {
		t.Errorf("subsystem = %d, want %d", subsystem, SubsystemWindowsGUI)
	}
}

// TestSubsystemReadsThePE32PlusOptionalHeader proves that the subsystem is
// read out of a 64 bit PE image, whose optional header has a different
// layout before the subsystem field.
func TestSubsystemReadsThePE32PlusOptionalHeader(t *testing.T) {
	image := buildPEImage(SubsystemWindowsCUI, magicPE32Plus, testOptionalHeaderSize64)

	subsystem, err := SubsystemOf(bytes.NewReader(image))
	assertNoError(t, err, "SubsystemOf: %v", err)
	if subsystem != SubsystemWindowsCUI {
		t.Errorf("subsystem = %d, want %d", subsystem, SubsystemWindowsCUI)
	}
}

// TestSubsystemReadsTheFile proves that the subsystem is read out of a file
// on disk.
func TestSubsystemReadsTheFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hfdesk.exe")
	image := buildPEImage(SubsystemWindowsCUI, magicPE32Plus, testOptionalHeaderSize64)
	err := os.WriteFile(path, image, 0o600)
	assertNoError(t, err, "write the image: %v", err)

	subsystem, err := Subsystem(path)
	assertNoError(t, err, "Subsystem: %v", err)
	if subsystem != SubsystemWindowsCUI {
		t.Errorf("subsystem = %d, want %d", subsystem, SubsystemWindowsCUI)
	}
}

// TestSubsystemRejectsANonPEFile proves that a file that is not a PE image is
// rejected with a clear error.
func TestSubsystemRejectsANonPEFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "notes.txt")
	text := []byte(strings.Repeat("this is not a PE image\n", 8))
	if err := os.WriteFile(path, text, 0o600); err != nil {
		t.Fatalf("write the file: %v", err)
	}

	_, err := Subsystem(path)
	if err == nil {
		t.Fatal("Subsystem accepted a file that is not a PE image")
	}
	if !strings.Contains(err.Error(), "not a PE image") {
		t.Errorf("error = %q, want it to say that the file is not a PE image", err.Error())
	}
}

// TestSubsystemRejectsATruncatedImage proves that a file that ends inside
// its headers is rejected instead of being read as a partial header.
func TestSubsystemRejectsATruncatedImage(t *testing.T) {
	image := buildPEImage(SubsystemWindowsGUI, magicPE32, testOptionalHeaderSize32)

	// The image is cut in the middle of the subsystem field, so the last
	// read of the headers comes up short.
	truncated := image[:testPeHeaderOffset+optionalHeaderOffset+subsystemOffset+1]

	_, err := SubsystemOf(bytes.NewReader(truncated))
	if err == nil {
		t.Fatal("SubsystemOf accepted a truncated image")
	}
	if !strings.Contains(err.Error(), "unexpected EOF") {
		t.Errorf("error = %q, want it to report the truncated header", err.Error())
	}
}

// TestSubsystemRejectsAnImageWithoutThePESignature proves that a file with a
// DOS header but without a PE header is rejected.
func TestSubsystemRejectsAnImageWithoutThePESignature(t *testing.T) {
	image := buildPEImage(SubsystemWindowsGUI, magicPE32, testOptionalHeaderSize32)
	copy(image[testPeHeaderOffset:], []byte("PX\x00\x00"))

	_, err := SubsystemOf(bytes.NewReader(image))
	if err == nil {
		t.Fatal("SubsystemOf accepted an image without the PE signature")
	}
	if !strings.Contains(err.Error(), "not a PE image") {
		t.Errorf("error = %q, want it to say that the file is not a PE image", err.Error())
	}
}

// TestSubsystemRejectsAnUnknownOptionalHeaderMagic proves that an optional
// header that is neither PE32 nor PE32+ is rejected.
func TestSubsystemRejectsAnUnknownOptionalHeaderMagic(t *testing.T) {
	image := buildPEImage(SubsystemWindowsGUI, magicPE32, testOptionalHeaderSize32)
	optional := testPeHeaderOffset + optionalHeaderOffset
	binary.LittleEndian.PutUint16(image[optional:], 0x107)

	_, err := SubsystemOf(bytes.NewReader(image))
	if err == nil {
		t.Fatal("SubsystemOf accepted an unknown optional header")
	}
	if !strings.Contains(err.Error(), "not a PE image") {
		t.Errorf("error = %q, want it to say that the file is not a PE image", err.Error())
	}
}

// TestSubsystemRejectsAMissingFile proves that a file that does not exist is
// reported as an error.
func TestSubsystemRejectsAMissingFile(t *testing.T) {
	_, err := Subsystem(filepath.Join(t.TempDir(), "missing.exe"))
	if err == nil {
		t.Fatal("Subsystem accepted a file that does not exist")
	}
	if !strings.Contains(err.Error(), "missing.exe") {
		t.Errorf("error = %q, want it to name the file", err.Error())
	}
}

// TestSubsystemName proves that the subsystem values are named and that an
// unknown value is named as such.
func TestSubsystemName(t *testing.T) {
	cases := map[int]string{
		SubsystemWindowsGUI: "IMAGE_SUBSYSTEM_WINDOWS_GUI",
		SubsystemWindowsCUI: "IMAGE_SUBSYSTEM_WINDOWS_CUI",
		0:                   "unknown subsystem",
		42:                  "unknown subsystem",
	}
	for value, want := range cases {
		if got := SubsystemName(value); got != want {
			t.Errorf("SubsystemName(%d) = %q, want %q", value, got, want)
		}
	}
}

func TestExecutableMachineRejectsTheCOFFOnlyStub(t *testing.T) {
	image := buildPEImageWithMachine(SubsystemWindowsCUI, magicPE32Plus, testOptionalHeaderSize64, MachineAMD64)
	stub := image[:testPeHeaderOffset+peSignatureSize]

	if _, err := ExecutableMachineOf(bytes.NewReader(stub)); err == nil {
		t.Fatal("ExecutableMachineOf accepted a COFF-only stub without an optional header or section table")
	}
	// Field extraction from the same header still succeeds: the rejection
	// is the structural check, not an unreadable header.
	if machine, err := readCOFFMachineField(bytes.NewReader(stub)); err != nil || machine != MachineAMD64 {
		t.Errorf("readCOFFMachineField(COFF-only stub) = 0x%04X, %v, want the extracted machine 0x%04X", machine, err, MachineAMD64)
	}
}

func TestExecutableMachineRequiresCompleteOptionalAndSectionHeaders(t *testing.T) {
	image := buildPEImageWithMachine(SubsystemWindowsCUI, magicPE32Plus, testOptionalHeaderSize64, MachineAMD64)
	cases := []struct {
		name  string
		image []byte
	}{
		{"truncated optional header", image[:testPeHeaderOffset+optionalHeaderOffset+testOptionalHeaderSize64-1]},
		{"truncated section table", image[:testPeHeaderOffset+optionalHeaderOffset+testOptionalHeaderSize64+testSectionHeaderSize-1]},
		{"truncated section data", image[:testSectionRawOffset+testSectionRawSize-1]},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ExecutableMachineOf(bytes.NewReader(tc.image)); err == nil {
				t.Fatalf("ExecutableMachineOf accepted %s", tc.name)
			}
		})
	}
}

func TestExecutableMachineAcceptsOptionalHeadersWithFewerDeclaredDirectories(t *testing.T) {
	cases := []struct {
		name    string
		magic   uint16
		size    int
		machine uint16
	}{
		{"PE32 with no directories", magicPE32, testOptionalHeaderFixedSize32, MachineI386},
		{"PE32 with one directory", magicPE32, testOptionalHeaderFixedSize32 + dataDirectoryEntrySize, MachineI386},
		{"PE32+ with no directories", magicPE32Plus, testOptionalHeaderFixedSize64, MachineAMD64},
		{"PE32+ with one directory", magicPE32Plus, testOptionalHeaderFixedSize64 + dataDirectoryEntrySize, MachineAMD64},
		{"PE32+ with all current directories", magicPE32Plus, testOptionalHeaderSize64, MachineAMD64},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			image := buildPEImageWithMachine(SubsystemWindowsCUI, tc.magic, tc.size, tc.machine)
			got, err := ExecutableMachineOf(bytes.NewReader(image))
			assertNoError(t, err, "ExecutableMachineOf: %v", err)
			if got != tc.machine {
				t.Errorf("machine = 0x%04X, want 0x%04X", got, tc.machine)
			}
		})
	}
}

func TestExecutableMachineRejectsTruncatedOrOutOfBoundsDataDirectories(t *testing.T) {
	cases := []struct {
		name  string
		magic uint16
		size  int
		count uint32
	}{
		{"PE32 missing the directory count", magicPE32, testOptionalHeaderFixedSize32 - 1, 0},
		{"PE32+ missing the directory count", magicPE32Plus, testOptionalHeaderFixedSize64 - 1, 0},
		{"PE32 count exceeds its declared optional header", magicPE32, testOptionalHeaderFixedSize32, 1},
		{"PE32+ count exceeds its declared optional header", magicPE32Plus, testOptionalHeaderFixedSize64, 1},
		{"directory is cut short by SizeOfOptionalHeader", magicPE32Plus, testOptionalHeaderFixedSize64 + dataDirectoryEntrySize - 1, 1},
		{"count exceeds available directory entries", magicPE32Plus, testOptionalHeaderFixedSize64 + dataDirectoryEntrySize, 2},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			image := buildPEImageWithMachine(SubsystemWindowsCUI, tc.magic, tc.size, MachineAMD64)
			setNumberOfRvaAndSizes(image, tc.magic, tc.count)
			if _, err := ExecutableMachineOf(bytes.NewReader(image)); err == nil {
				t.Fatal("ExecutableMachineOf accepted an optional-header directory count outside its declared bounds")
			}
		})
	}
}

func TestExecutableMachineReadsStructurallyCompleteImage(t *testing.T) {
	image := buildPEImageWithMachine(SubsystemWindowsCUI, magicPE32Plus, testOptionalHeaderSize64, MachineAMD64)
	machine, err := ExecutableMachineOf(bytes.NewReader(image))
	assertNoError(t, err, "ExecutableMachineOf: %v", err)
	if machine != MachineAMD64 {
		t.Errorf("machine = 0x%04X, want 0x%04X", machine, MachineAMD64)
	}
}

// buildPEImage builds a coherent small PE32/PE32+ image with one code section
// and a directory count that matches the optional-header size.
func buildPEImage(subsystem int, magic uint16, optionalHeaderSize int) []byte {
	optional := testPeHeaderOffset + optionalHeaderOffset
	sectionTable := optional + optionalHeaderSize
	image := make([]byte, testSectionRawOffset+testSectionRawSize)

	copy(image[dosSignatureOffset:], []byte("MZ"))
	binary.LittleEndian.PutUint32(image[dosHeaderOffsetField:], uint32(testPeHeaderOffset))

	copy(image[testPeHeaderOffset+peSignatureOffset:], []byte("PE\x00\x00"))
	// The COFF header carries one executable section and the optional
	// header size.
	binary.LittleEndian.PutUint16(image[testPeHeaderOffset+4+coffSectionCountOffset:], 1)
	binary.LittleEndian.PutUint16(image[testPeHeaderOffset+4+16:], uint16(optionalHeaderSize))
	characteristics := uint16(coffExecutableImage)
	if magic == magicPE32 {
		characteristics |= 0x0100 // IMAGE_FILE_32BIT_MACHINE
	}
	binary.LittleEndian.PutUint16(image[testPeHeaderOffset+4+coffCharacteristicsOffset:], characteristics)

	binary.LittleEndian.PutUint16(image[optional:], magic)
	binary.LittleEndian.PutUint32(image[optional+4:], testSectionRawSize)
	binary.LittleEndian.PutUint32(image[optional+optionalHeaderEntryPointOffset:], 0x1000)
	binary.LittleEndian.PutUint32(image[optional+20:], 0x1000) // BaseOfCode
	if magic == magicPE32 {
		binary.LittleEndian.PutUint32(image[optional+24:], 0) // no data section
		binary.LittleEndian.PutUint32(image[optional+28:], 0x00400000)
		binary.LittleEndian.PutUint32(image[optional+72:], 0x00100000) // Stack reserve
		binary.LittleEndian.PutUint32(image[optional+76:], 0x1000)     // Stack commit
		binary.LittleEndian.PutUint32(image[optional+80:], 0x00100000) // Heap reserve
		binary.LittleEndian.PutUint32(image[optional+84:], 0x1000)     // Heap commit
	} else {
		binary.LittleEndian.PutUint64(image[optional+24:], 0x0000000140000000)
		binary.LittleEndian.PutUint64(image[optional+72:], 0x00100000) // Stack reserve
		binary.LittleEndian.PutUint64(image[optional+80:], 0x1000)     // Stack commit
		binary.LittleEndian.PutUint64(image[optional+88:], 0x00100000) // Heap reserve
		binary.LittleEndian.PutUint64(image[optional+96:], 0x1000)     // Heap commit
	}
	binary.LittleEndian.PutUint32(image[optional+32:], 0x1000) // Section alignment
	binary.LittleEndian.PutUint32(image[optional+36:], 0x200)  // File alignment
	binary.LittleEndian.PutUint16(image[optional+40:], 6)      // Major OS version
	binary.LittleEndian.PutUint16(image[optional+48:], 6)      // Major subsystem version
	binary.LittleEndian.PutUint32(image[optional+56:], 0x2000)
	binary.LittleEndian.PutUint32(image[optional+60:], uint32(testSectionRawOffset))
	binary.LittleEndian.PutUint16(image[optional+subsystemOffset:], uint16(subsystem))
	setNumberOfRvaAndSizes(image, magic, uint32(testDirectoryCount(optionalHeaderSize, magic)))
	copy(image[sectionTable:], []byte(".text"))
	binary.LittleEndian.PutUint32(image[sectionTable+8:], testSectionRawSize)
	binary.LittleEndian.PutUint32(image[sectionTable+12:], 0x1000)
	binary.LittleEndian.PutUint32(image[sectionTable+16:], testSectionRawSize)
	binary.LittleEndian.PutUint32(image[sectionTable+20:], testSectionRawOffset)
	binary.LittleEndian.PutUint32(image[sectionTable+36:], 0x60000020) // code, execute, read
	image[testSectionRawOffset] = 0xC3

	return image
}

func testDirectoryCount(optionalHeaderSize int, magic uint16) int {
	fixedSize := testOptionalHeaderFixedSize32
	if magic == magicPE32Plus {
		fixedSize = testOptionalHeaderFixedSize64
	}
	if optionalHeaderSize <= fixedSize {
		return 0
	}
	return (optionalHeaderSize - fixedSize) / dataDirectoryEntrySize
}

func setNumberOfRvaAndSizes(image []byte, magic uint16, count uint32) {
	offset := testPeHeaderOffset + optionalHeaderOffset + optionalHeader32DirectoryCount
	if magic == magicPE32Plus {
		offset = testPeHeaderOffset + optionalHeaderOffset + optionalHeader64DirectoryCount
	}
	binary.LittleEndian.PutUint32(image[offset:], count)
}

// buildPEImageWithMachine builds a PE image with the given subsystem, optional
// header and machine type.
func buildPEImageWithMachine(subsystem int, magic uint16, optionalHeaderSize int, machine uint16) []byte {
	image := buildPEImage(subsystem, magic, optionalHeaderSize)
	binary.LittleEndian.PutUint16(image[testPeHeaderOffset+machineOffset:], machine)
	return image
}

// readCOFFMachineField extracts the machine field out of the COFF header of a
// PE image without any structural checks. The COFF-only-stub test uses it as
// the contrast that proves ExecutableMachineOf's rejection comes from the
// structural check, not from an unreadable header.
func readCOFFMachineField(r io.ReaderAt) (uint16, error) {
	_, header, err := readPEHeader(r)
	if err != nil {
		return 0, err
	}
	return binary.LittleEndian.Uint16(header[machineOffset : machineOffset+machineSize]), nil
}

// assertNoError fails the test when the call it wraps reported a failure,
// with the diagnostic the inline check printed.
func assertNoError(t *testing.T, err error, format string, a ...any) {
	t.Helper()
	if err == nil {
		return
	}
	if format == "" {
		t.Fatal(err)
		return
	}
	if len(a) == 0 {
		t.Fatal(format)
		return
	}
	t.Fatalf(format, a...)
}
