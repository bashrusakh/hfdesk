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
// machine reaches such a service through the loopback interface of the same
// family: the address the readiness probe verifies is the address the user is
// sent to. The URL is derived from the registration, so an installation that
// moved the service to another port is opened on that port and not on a default.
func TestServiceURLReturnsTheAddressABrowserCanOpen(t *testing.T) {
	tests := []struct {
		name string
		addr string
		want string
	}{
		{"every IPv4 interface", "0.0.0.0:8080", "http://127.0.0.1:8080"},
		{"every IPv6 interface", "[::]:8080", "http://[::1]:8080"},
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
// invents an address: a registration that cannot be read as a host and a port,
// and one that names no address at all, is an error the caller reports, not a URL
// the user is sent to.
func TestServiceURLRejectsAnAddressWithoutAHostAndPort(t *testing.T) {
	for _, addr := range []string{"", "   ", "8080", "127.0.0.1", "127.0.0.1:"} {
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

// TestFindConsoleExecutableFindsThePlainNameOfALocalBuild proves that the plain
// name hfdesk.exe is the console program of a desktop program without a version
// stamp.
//
// The comparison is case-insensitive, because the file systems of Windows are:
// a folder that carries HFDesk.EXE has the program under the name the
// installation looks for. A local build carries no version, so no release pins
// the name of its console program, and the plain name decides over the
// version-stamped program of another release in the same folder.
func TestFindConsoleExecutableFindsThePlainNameOfALocalBuild(t *testing.T) {
	dir := t.TempDir()
	preferred := filepath.Join(dir, "HFDesk.EXE")
	writeFile(t, preferred)
	writeFile(t, filepath.Join(dir, "hfdesk_windows_amd64_1.3.0.exe"))

	found, err := findConsoleExecutable(dir, filepath.Join(dir, "hfdesk-desktop.exe"))
	if err != nil {
		t.Fatalf("find the console program: %v", err)
	}
	if found != preferred {
		t.Errorf("the console program is %q, want the plain name %q", found, preferred)
	}
}

// TestFindConsoleExecutableAcceptsTheSingleProgramOfALocalBuild proves that the
// fallback of a desktop program without a version stamp accepts the only
// console program of the folder.
//
// The release carries the version in the name of the program, and a local build
// has no version that could name one program: a single hfdesk*.exe is the
// program the user put next to the desktop host.
func TestFindConsoleExecutableAcceptsTheSingleProgramOfALocalBuild(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, "hfdesk_windows_amd64_1.3.0.exe")
	writeFile(t, want)

	found, err := findConsoleExecutable(dir, filepath.Join(dir, "hfdesk-desktop.exe"))
	if err != nil {
		t.Fatalf("find the console program: %v", err)
	}
	if found != want {
		t.Errorf("the console program is %q, want %q", found, want)
	}
}

// TestFindConsoleExecutableRequiresThePairOfAVersionedProgram proves that a
// version-stamped desktop program is only installed with the console program of
// its own release.
//
// Copying only the new desktop program next to an old hfdesk.exe leaves a folder
// whose only console program belongs to another release. Registering it would
// name the new version in the success report while the service runs the old
// program, so the lookup refuses the folder and names the file it expects. The
// single candidate of an older release is refused the same way: the fallbacks
// belong to a desktop program without a version.
func TestFindConsoleExecutableRequiresThePairOfAVersionedProgram(t *testing.T) {
	const desktop = "hfdesk-desktop_windows_amd64_1.4.0.exe"

	plain := t.TempDir()
	writeFile(t, filepath.Join(plain, "hfdesk.exe"))
	versioned := t.TempDir()
	writeFile(t, filepath.Join(versioned, "hfdesk_windows_amd64_1.3.0.exe"))

	for _, dir := range []string{plain, versioned} {
		found, err := findConsoleExecutable(dir, filepath.Join(dir, desktop))
		if err == nil {
			t.Errorf("the version-stamped desktop program in %s yielded %q, want an error", dir, found)
			continue
		}
		for _, want := range []string{"hfdesk_windows_amd64_1.4.0.exe", dir} {
			if !strings.Contains(err.Error(), want) {
				t.Errorf("the error is %q, want it to mention %q", err, want)
			}
		}
	}
}

// TestFindConsoleExecutableFindsThePairOfAVersionedProgram proves that the
// console program of the same release is registered for a version-stamped
// desktop program.
//
// The folder also holds an older hfdesk.exe, which is not the program of this
// release: the pair decides, so the success report and the registration name the
// same version.
func TestFindConsoleExecutableFindsThePairOfAVersionedProgram(t *testing.T) {
	dir := t.TempDir()
	want := filepath.Join(dir, "hfdesk_windows_amd64_1.4.0.exe")
	for _, name := range []string{
		"hfdesk.exe",
		"hfdesk_windows_amd64_1.3.0.exe",
		"hfdesk_windows_amd64_1.4.0.exe",
		"hfdesk-desktop_windows_amd64_1.4.0.exe",
	} {
		writeFile(t, filepath.Join(dir, name))
	}

	found, err := findConsoleExecutable(dir, filepath.Join(dir, "hfdesk-desktop_windows_amd64_1.4.0.exe"))
	if err != nil {
		t.Fatalf("find the console program: %v", err)
	}
	if found != want {
		t.Errorf("the console program is %q, want the pair %q", found, want)
	}
}

// TestFindConsoleExecutablePrefersThePairedProgram proves that the console
// program of the running desktop program is chosen when the folder holds two
// releases.
//
// Unpacking a new release next to the old one leaves two version-stamped console
// programs and two version-stamped desktop programs in the folder. The pair of the
// program that asks is the program the user meant, so the installation registers
// it instead of refusing the folder.
func TestFindConsoleExecutablePrefersThePairedProgram(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{
		"hfdesk-desktop_windows_amd64_v1.3.0.exe",
		"hfdesk-desktop_windows_amd64_v1.4.0.exe",
		"hfdesk_windows_amd64_v1.3.0.exe",
		"hfdesk_windows_amd64_v1.4.0.exe",
	} {
		writeFile(t, filepath.Join(dir, name))
	}

	tests := []struct {
		name    string
		desktop string
		want    string
	}{
		{"the running release", "hfdesk-desktop_windows_amd64_v1.4.0.exe", "hfdesk_windows_amd64_v1.4.0.exe"},
		{"the older release", "hfdesk-desktop_windows_amd64_v1.3.0.exe", "hfdesk_windows_amd64_v1.3.0.exe"},
		// The prefix is matched without regard to case, as the file systems
		// of Windows name a program.
		{"another case", "HFDESK-DESKTOP_WINDOWS_AMD64_V1.4.0.EXE", "hfdesk_windows_amd64_v1.4.0.exe"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			found, err := findConsoleExecutable(dir, filepath.Join(dir, test.desktop))
			if err != nil {
				t.Fatalf("find the console program of %q: %v", test.desktop, err)
			}
			if want := filepath.Join(dir, test.want); found != want {
				t.Errorf("the console program is %q, want the pair %q", found, want)
			}
		})
	}
}

// TestPairedConsoleExecutableName proves how the pair of a desktop program is
// derived, and that a program without the prefix names no pair.
func TestPairedConsoleExecutableName(t *testing.T) {
	tests := []struct {
		desktop string
		want    string
	}{
		{"hfdesk-desktop_windows_amd64_v1.4.0.exe", "hfdesk_windows_amd64_v1.4.0.exe"},
		{"HFDesk-Desktop_windows_amd64_v1.4.0.exe", "hfdesk_windows_amd64_v1.4.0.exe"},
		{"hfdesk-desktop.exe", "hfdesk.exe"},
		{filepath.Join("portable", "hfdesk-desktop_windows_amd64_v1.4.0.exe"), "hfdesk_windows_amd64_v1.4.0.exe"},
		{"hfdesk.exe", ""},
		{"desktop.exe", ""},
		{"", ""},
	}
	for _, test := range tests {
		if got := pairedConsoleExecutableName(test.desktop); got != test.want {
			t.Errorf("pairedConsoleExecutableName(%q) = %q, want %q", test.desktop, got, test.want)
		}
	}
}

// TestFindConsoleExecutableRefusesSeveralPrograms proves that a folder with
// more than one candidate and no pair for the running desktop program is an
// error that names the folder.
//
// Registering one of several programs would register a program the user did
// not choose, so the installation refuses the folder instead of guessing. The
// desktop program of the test carries no version, so the folder is decided by
// the fallbacks and not by the pair rule.
func TestFindConsoleExecutableRefusesSeveralPrograms(t *testing.T) {
	dir := t.TempDir()
	writeFile(t, filepath.Join(dir, "hfdesk_windows_amd64_1.3.0.exe"))
	writeFile(t, filepath.Join(dir, "hfdesk_windows_amd64_1.4.0.exe"))

	found, err := findConsoleExecutable(dir, filepath.Join(dir, "hfdesk-desktop.exe"))
	if err == nil {
		t.Fatalf("the folder with two candidates yielded %q, want an error", found)
	}
	for _, want := range []string{dir, "hfdesk_windows_amd64_1.3.0.exe", "hfdesk_windows_amd64_1.4.0.exe"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the error is %q, want it to mention %q", err, want)
		}
	}
}

// TestDesktopVersionStamp proves which names of a desktop program carry a
// release version.
//
// The release names both programs with the same version stamp, and the pair rule
// depends on reading that stamp: a name that only carries the platform or the
// architecture is a local build without a release, so it keeps the fallbacks.
func TestDesktopVersionStamp(t *testing.T) {
	tests := []struct {
		desktop string
		want    string
		version bool
	}{
		{"hfdesk-desktop_windows_amd64_1.4.0.exe", "1.4.0", true},
		{"hfdesk-desktop_windows_amd64_v1.3.0.exe", "v1.3.0", true},
		{"hfdesk-desktop_windows_amd64_1.3.0-beta.1.exe", "1.3.0-beta.1", true},
		{"HFDESK-DESKTOP_WINDOWS_AMD64_1.4.0.EXE", "1.4.0", true},
		{"hfdesk-desktop_windows_amd64_1.4.exe", "1.4", true},
		// The CI build carries a marker instead of a version, and the
		// platform alone names no release.
		{"hfdesk-desktop_windows_amd64_ci.exe", "", false},
		{"hfdesk-desktop.exe", "", false},
		{"hfdesk-desktop_windows_amd64.exe", "", false},
		{"hfdesk.exe", "", false},
		{"", "", false},
	}
	for _, test := range tests {
		stamp, versioned := desktopVersionStamp(test.desktop)
		if stamp != test.want || versioned != test.version {
			t.Errorf("desktopVersionStamp(%q) = %q, %v, want %q, %v",
				test.desktop, stamp, versioned, test.want, test.version)
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

	found, err := findConsoleExecutable(dir, filepath.Join(dir, "hfdesk-desktop.exe"))
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
