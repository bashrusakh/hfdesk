//go:build ignore

// Command pesubsystem reports the subsystem of a Windows PE image.
//
// The subsystem of an executable is a build time property of the linker: a
// console subsystem image (3) opens a console window, a GUI subsystem image
// (2) does not. HFDesk ships two Windows binaries, the console host and the
// desktop host, and the difference between them has to be a fact of the
// produced file instead of a promise of the build script. The release
// pipeline and the CI check therefore read the subsystem out of the PE
// header of the artifact they built.
//
// Usage:
//
//	pesubsystem <file.exe> [expected]
//
// The expected subsystem is optional. It is a number (2 or 3) or a name
// (gui, cui). When it is given, the command fails when the subsystem of the
// file differs from it, so it can be used as an assertion.
//
// The program is not part of the module: it carries the ignore build tag and
// is run with "go run", like the icon generator of cmd/hfdesk.
package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
)

// The subsystems of a PE image that HFDesk builds.
const (
	// subsystemWindowsGUI is IMAGE_SUBSYSTEM_WINDOWS_GUI: the image has no
	// console window.
	subsystemWindowsGUI = 2
	// subsystemWindowsCUI is IMAGE_SUBSYSTEM_WINDOWS_CUI: the image runs
	// in a console window.
	subsystemWindowsCUI = 3
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
	// optionalHeaderOffset is the offset of the optional header inside the
	// PE header.
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

// main runs the command and reports a failure with a non-zero exit code.
func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "pesubsystem: %v\n", err)
		os.Exit(1)
	}
}

// run performs the command: it reads the subsystem of the file and compares
// it with the expected subsystem when one was given.
func run(args []string, out io.Writer) error {
	if len(args) < 1 || len(args) > 2 {
		return errors.New("usage: pesubsystem <file.exe> [expected]")
	}

	path := args[0]
	value, err := readSubsystem(path)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "%s: subsystem %d (%s)\n", path, value, subsystemName(value))

	if len(args) < 2 {
		return nil
	}

	expected, err := parseExpected(args[1])
	if err != nil {
		return err
	}
	if value != expected {
		return fmt.Errorf("%s has subsystem %d (%s), want %d (%s)",
			path, value, subsystemName(value), expected, subsystemName(expected))
	}
	return nil
}

// readSubsystem reads the subsystem of the PE image at path.
func readSubsystem(path string) (int, error) {
	file, err := os.Open(path)
	if err != nil {
		return 0, fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()

	return subsystemOf(file)
}

// subsystemOf reads the subsystem out of the headers of a PE image.
func subsystemOf(r io.ReaderAt) (int, error) {
	dos := make([]byte, dosHeaderSize)
	if err := readAt(r, 0, dos); err != nil {
		return 0, fmt.Errorf("read the DOS header: %w", err)
	}
	if dos[dosSignatureOffset] != 'M' || dos[dosSignatureOffset+1] != 'Z' {
		return 0, errors.New("the file is not a PE image: it has no MZ signature")
	}

	peOffset := int64(binary.LittleEndian.Uint32(dos[dosHeaderOffsetField : dosHeaderOffsetField+4]))
	if peOffset < 0 {
		return 0, errors.New("the file is not a PE image: the offset of the PE header is invalid")
	}

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
// image this program can judge.
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

// parseExpected converts the expected subsystem of the command line into its
// number.
func parseExpected(value string) (int, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "gui", "windows", "windowsgui":
		return subsystemWindowsGUI, nil
	case "cui", "console", "windowscui":
		return subsystemWindowsCUI, nil
	}

	number, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("invalid expected subsystem %q: expected 2 (gui), 3 (cui) or a number", value)
	}
	if number != subsystemWindowsGUI && number != subsystemWindowsCUI {
		return 0, fmt.Errorf("invalid expected subsystem %d: expected 2 (gui) or 3 (cui)", number)
	}
	return number, nil
}

// subsystemName returns the name of a subsystem.
func subsystemName(value int) string {
	switch value {
	case subsystemWindowsGUI:
		return "IMAGE_SUBSYSTEM_WINDOWS_GUI"
	case subsystemWindowsCUI:
		return "IMAGE_SUBSYSTEM_WINDOWS_CUI"
	default:
		return "unknown subsystem"
	}
}
