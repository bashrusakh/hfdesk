// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestServiceURLReturnsTheAddressABrowserCanOpen proves that the registration
// of the service is turned into an address the browser of the user opens.
//
// A service binds the address of its registration. A wildcard address is the
// same bind as every other spelling of "every interface", and a browser of the
// machine reaches such a service through the loopback interface. The URL is
// derived from the registration, so an installation that moved the service to
// another port is opened on that port and not on a default.
func TestServiceURLReturnsTheAddressABrowserCanOpen(t *testing.T) {
	tests := []struct {
		name string
		addr string
		want string
	}{
		{"every IPv4 interface", "0.0.0.0:8080", "http://127.0.0.1:8080"},
		{"every IPv6 interface", "[::]:8080", "http://127.0.0.1:8080"},
		{"no host", ":8080", "http://127.0.0.1:8080"},
		{"loopback", "127.0.0.1:18080", "http://127.0.0.1:18080"},
		{"IPv6 loopback", "[::1]:18080", "http://[::1]:18080"},
		{"the literal localhost", "localhost:8080", "http://localhost:8080"},
		{"a named interface", "192.168.1.20:8080", "http://192.168.1.20:8080"},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			url, err := serviceURL(test.addr)
			if err != nil {
				t.Fatalf("serviceURL(%q) failed: %v", test.addr, err)
			}
			if url != test.want {
				t.Errorf("serviceURL(%q) = %q, want %q", test.addr, url, test.want)
			}
		})
	}
}

// TestServiceURLRejectsAnAddressWithoutAHostAndPort proves that the gate never
// invents an address: a registration that cannot be read as a host and a port
// is an error the caller reports, not a URL the user is sent to.
func TestServiceURLRejectsAnAddressWithoutAHostAndPort(t *testing.T) {
	for _, addr := range []string{"", "   ", "8080", "127.0.0.1"} {
		if url, err := serviceURL(addr); err == nil {
			t.Errorf("serviceURL(%q) = %q, want an error", addr, url)
		}
	}
}

// TestParseServiceActionAcceptsTheHiddenActions proves that the helper accepts
// the three actions the desktop host launches and nothing else.
func TestParseServiceActionAcceptsTheHiddenActions(t *testing.T) {
	for _, test := range []struct {
		value string
		want  serviceAction
	}{
		{"install", serviceActionInstall},
		{"update", serviceActionUpdate},
		{"start", serviceActionStart},
		{" Install ", serviceActionInstall},
	} {
		action, err := parseServiceAction(test.value)
		if err != nil {
			t.Errorf("parseServiceAction(%q) failed: %v", test.value, err)
			continue
		}
		if action != test.want {
			t.Errorf("parseServiceAction(%q) = %q, want %q", test.value, action, test.want)
		}
	}
}

// TestParseServiceActionRejectsEveryOtherValue proves that a helper never
// guesses the action it was asked to perform.
func TestParseServiceActionRejectsEveryOtherValue(t *testing.T) {
	for _, value := range []string{"", "uninstall", "restart", "install service"} {
		if action, err := parseServiceAction(value); err == nil {
			t.Errorf("parseServiceAction(%q) = %q, want an error", value, action)
		}
	}
}

// TestFindConsoleExecutablePrefersThePlainName proves that the console program
// of the service is the program with the plain name when the folder has one.
//
// The comparison is case-insensitive, because the file systems of Windows are:
// a folder that carries HFDesk.EXE has the program under the name the
// installation looks for.
func TestFindConsoleExecutablePrefersThePlainName(t *testing.T) {
	dir := t.TempDir()
	preferred := filepath.Join(dir, "HFDesk.EXE")
	writeFile(t, preferred)
	writeFile(t, filepath.Join(dir, "hfdesk_windows_amd64_1.3.0.exe"))

	found, err := findConsoleExecutable(dir)
	if err != nil {
		t.Fatalf("find the console program: %v", err)
	}
	if found != preferred {
		t.Errorf("the console program is %q, want the plain name %q", found, preferred)
	}
}

// TestFindConsoleExecutableAcceptsASingleVersionedProgram proves that a folder
// of one release artifact installs the program of that artifact.
//
// The release carries the version in the name of the program, so a single
// hfdesk*.exe is the program the user put next to the desktop host.
func TestFindConsoleExecutableAcceptsASingleVersionedProgram(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, "hfdesk_windows_amd64_1.3.0.exe")
	writeFile(t, want)

	found, err := findConsoleExecutable(dir)
	if err != nil {
		t.Fatalf("find the console program: %v", err)
	}
	if found != want {
		t.Errorf("the console program is %q, want %q", found, want)
	}
}

// TestFindConsoleExecutableRefusesSeveralPrograms proves that a folder with
// more than one candidate is an error that names the folder.
//
// Registering one of several programs would register a program the user did
// not choose, so the installation refuses the folder instead of guessing.
func TestFindConsoleExecutableRefusesSeveralPrograms(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "hfdesk_windows_amd64_1.3.0.exe"))
	writeFile(t, filepath.Join(dir, "hfdesk_windows_amd64_1.4.0.exe"))

	found, err := findConsoleExecutable(dir)
	if err == nil {
		t.Fatalf("the folder with two candidates yielded %q, want an error", found)
	}
	for _, want := range []string{dir, "hfdesk_windows_amd64_1.3.0.exe", "hfdesk_windows_amd64_1.4.0.exe"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error is %q, want it to mention %q", err, want)
		}
	}
}

// TestFindConsoleExecutableRefusesAFolderWithoutTheProgram proves that a folder
// without the console program is an error that names the folder, and that the
// desktop program is never mistaken for it.
func TestFindConsoleExecutableRefusesAFolderWithoutTheProgram(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "hfdesk-desktop.exe"))
	writeFile(t, filepath.Join(dir, "hfdesk-desktop_windows_amd64_1.3.0.exe"))
	writeFile(t, filepath.Join(dir, "README.txt"))

	found, err := findConsoleExecutable(dir)
	if err == nil {
		t.Fatalf("the folder without the console program yielded %q, want an error", found)
	}
	if !strings.Contains(err.Error(), dir) {
		t.Errorf("the error is %q, want it to name the folder %q", err, dir)
	}
}

// writeFile creates a file of the given size, so the lookup finds a name that is
// worth reporting.
func writeFile(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, []byte("hfdesk"), 0o600); err != nil {
		t.Fatalf("create %s: %v", path, err)
	}
}
