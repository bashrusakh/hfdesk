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
// The program is not part of the build: it carries the ignore build tag and
// is run with "go run", like the icon generator of cmd/hfdesk.
//
// The PE header itself is read by the internal/pe package, so the check of
// the pipeline and the startup check of the desktop host read the subsystem
// of an image with the same code.
package main

import (
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/bashrusakh/hfdesk/internal/pe"
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
	value, err := pe.Subsystem(path)
	if err != nil {
		return err
	}

	fmt.Fprintf(out, "%s: subsystem %d (%s)\n", path, value, pe.SubsystemName(value))

	if len(args) < 2 {
		return nil
	}

	expected, err := parseExpected(args[1])
	if err != nil {
		return err
	}
	if value != expected {
		return fmt.Errorf("%s has subsystem %d (%s), want %d (%s)",
			path, value, pe.SubsystemName(value), expected, pe.SubsystemName(expected))
	}
	return nil
}

// parseExpected converts the expected subsystem of the command line into its
// number.
func parseExpected(value string) (int, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "gui", "windows", "windowsgui":
		return pe.SubsystemWindowsGUI, nil
	case "cui", "console", "windowscui":
		return pe.SubsystemWindowsCUI, nil
	}

	number, err := strconv.Atoi(strings.TrimSpace(value))
	if err != nil {
		return 0, fmt.Errorf("invalid expected subsystem %q: expected 2 (gui), 3 (cui) or a number", value)
	}
	if number != pe.SubsystemWindowsGUI && number != pe.SubsystemWindowsCUI {
		return 0, fmt.Errorf("invalid expected subsystem %d: expected 2 (gui) or 3 (cui)", number)
	}
	return number, nil
}
