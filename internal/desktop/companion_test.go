// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/service"
)

// TestSplitCompanionArgsLeavesAnOrdinaryCommandLineUntouched proves that an
// ordinary launch passes its arguments on byte for byte — the flags are
// stripped only when the companion protocol is really present, so nothing
// about the established host parsing changes.
func TestSplitCompanionArgsLeavesAnOrdinaryCommandLineUntouched(t *testing.T) {
	args := []string{"--port", "9090", "--no-open"}
	launch, rest, err := SplitCompanionArgs(args)
	if err != nil {
		t.Fatalf("SplitCompanionArgs(%q) failed: %v", args, err)
	}
	if launch.Enabled || launch.Wait != 0 {
		t.Errorf("the launch = %+v, want no companion part for an ordinary command line", launch)
	}
	// Same slice, not only equal contents: the ordinary path must not
	// even rebuild the command line.
	if len(rest) != len(args) || (len(args) > 0 && &rest[0] != &args[0]) {
		t.Errorf("the remaining arguments = %q, want the ORIGINAL slice %q", rest, args)
	}
}

// TestSplitCompanionArgsSeparatesTheRelaunchProtocol proves both halves of
// the private flags: the companion mode with and without its bounded wait,
// both spellings of each flag, and the order of the remaining host flags.
func TestSplitCompanionArgsSeparatesTheRelaunchProtocol(t *testing.T) {
	for _, tc := range []struct {
		name     string
		args     []string
		wantRest []string
		wantWait time.Duration
	}{
		{
			name:     "the companion mode alone (a hand-typed launch)",
			args:     []string{companionModeFlag, "--log-level", "debug"},
			wantRest: []string{"--log-level", "debug"},
		},
		{
			name:     "the mode with the separate wait value",
			args:     []string{"--port", "0", companionModeFlag, companionWaitFlag, "2m30s"},
			wantRest: []string{"--port", "0"},
			wantWait: 150 * time.Second,
		},
		{
			name:     "the wait in the equals form",
			args:     []string{companionModeFlag, companionWaitFlag + "=150s"},
			wantRest: []string{},
			wantWait: 150 * time.Second,
		},
		{
			name:     "the single dash spellings",
			args:     []string{"-tray-companion", "-companion-wait", "1s"},
			wantRest: []string{},
			wantWait: time.Second,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			launch, rest, err := SplitCompanionArgs(tc.args)
			if err != nil {
				t.Fatalf("SplitCompanionArgs(%q) failed: %v", tc.args, err)
			}
			if !launch.Enabled {
				t.Error("the companion mode was not detected")
			}
			if launch.Wait != tc.wantWait {
				t.Errorf("the wait = %s, want %s", launch.Wait, tc.wantWait)
			}
			if !reflect.DeepEqual(rest, tc.wantRest) {
				t.Errorf("the remaining arguments = %q, want %q", rest, tc.wantRest)
			}
		})
	}
}

// TestSplitCompanionArgsRejectsHalfOfTheProtocol proves the fail-closed
// parse: a wait without the mode and an unreadable duration are command
// line problems that name their flag and their value, and neither is ever
// silently accepted as an ordinary launch.
func TestSplitCompanionArgsRejectsHalfOfTheProtocol(t *testing.T) {
	for _, tc := range []struct {
		name string
		args []string
		want string
	}{
		{
			name: "the wait without the companion mode",
			args: []string{"--port", "0", companionWaitFlag, "150s"},
			want: companionModeFlag,
		},
		{
			name: "an unreadable duration",
			args: []string{companionModeFlag, companionWaitFlag, "soon"},
			want: "soon",
		},
		{
			name: "a negative duration",
			args: []string{companionModeFlag, companionWaitFlag, "-5s"},
			want: companionWaitFlag,
		},
		{
			name: "the wait without its value",
			args: []string{companionModeFlag, companionWaitFlag},
			want: companionWaitFlag,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			launch, rest, err := SplitCompanionArgs(tc.args)
			if err == nil {
				t.Fatalf("SplitCompanionArgs(%q) = %+v, want an error", tc.args, launch)
			}
			if !strings.Contains(err.Error(), tc.want) {
				t.Errorf("the error %q does not name %q", err, tc.want)
			}
			// The caller reports the error with the usage of the host,
			// so the returned rest must still be parseable host flags.
			for _, arg := range rest {
				if arg == companionModeFlag || arg == companionWaitFlag {
					t.Errorf("the rest %q still carries a companion flag", rest)
				}
			}
		})
	}
}

// TestCompanionRelaunchArgsRoundTripThroughTheParser proves that the
// command line the installation spawns is exactly this protocol: the
// arguments of the departing host travel unchanged (the log settings of
// the session reach the child), the two flags complete it, and re-reading
// it — the very parse the child performs — recovers the companion mode and
// the bounded wait.
func TestCompanionRelaunchArgsRoundTripThroughTheParser(t *testing.T) {
	argv := []string{"--log-file", `C:\Users\First Last\hfdesk.log`, "--no-open"}
	args := companionRelaunchArgs(argv, 150*time.Second)

	launch, rest, err := SplitCompanionArgs(args)
	if err != nil {
		t.Fatalf("the relaunch command line %q does not parse: %v", args, err)
	}
	if !launch.Enabled {
		t.Error("the relaunch command line does not start a companion")
	}
	if launch.Wait != 150*time.Second {
		t.Errorf("the relaunch wait = %s, want 2m30s", launch.Wait)
	}
	if !reflect.DeepEqual(rest, argv) {
		t.Errorf("the host arguments = %q, want them unchanged %q", rest, argv)
	}
}

// TestCompanionPageRendersStateLinkAndLog pins the page of the companion
// window: the honest state as its heading, the link of the REGISTRATION
// (an anchor a click follows), the standard log-path line, and the
// service-log section — the bounded tail ending with the newest line, the
// one action that opens the full file, and the single script that scrolls
// that tail to its end and forwards the fixed action name.
//
// The page fetches nothing: its one script carries no channel of any kind,
// so the companion still listens to nothing — a model without a
// service-log source carries no script at all (pinned below and by the
// seam tests of the companion launch).
func TestCompanionPageRendersStateLinkAndLog(t *testing.T) {
	page := companionPage(companionPageState{
		State:   "The HFDesk service is stopped.",
		WebURL:  "http://127.0.0.1:18080",
		LogPath: `C:\Users\First Last\hfdesk.log`,
		ServiceLog: serviceLogSection{
			Path: `C:\ProgramData\HFDesk\logs\hfdesk.log`,
			Tail: "line one\nline two: newest",
		},
	})

	assertNames(t, page, "the companion page", []string{
		"<h1>The HFDesk service is stopped.</h1>",
		`<a href="http://127.0.0.1:18080">http://127.0.0.1:18080</a>`,
		"The log file is at",
		`<code>C:\Users\First Last\hfdesk.log</code>`,
		// The service log section: heading, the bounded tail in its
		// box, and the one action of the section.
		`<p class="label">Service log</p>`,
		`<pre id="service-log-tail">`,
		"line one\nline two: newest",
		"Open the full log file",
		`id="open-service-log"`,
	})
	// The tail ends with the newest line: what the window shows is the
	// END of the file (the script below scrolls the box to it), and the
	// section is the last content of the document.
	if tail := strings.Index(page, `<pre id="service-log-tail">`); tail > strings.Index(page, "line two: newest") {
		t.Errorf("the newest line sits before the tail box:\n%s", page)
	}
	// The section order: the log file of this launch is stated above
	// the Service log section, and the tail sits under its heading —
	// the section is the last content a reader reaches.
	launchLog := strings.Index(page, "The log file is at")
	label := strings.Index(page, `<p class="label">Service log</p>`)
	sectionBox := strings.Index(page, `<pre id="service-log-tail">`)
	if launchLog == -1 || label == -1 || sectionBox == -1 || !(launchLog < label && label < sectionBox) {
		t.Errorf("the page order is launch log %d, label %d, tail %d, want the section last:\n%s",
			launchLog, label, sectionBox, page)
	}
	// The only script of the page performs the two local operations and
	// nothing else: no fetch, no socket, no remote resource, no data
	// leaves the document — the companion listens to nothing.
	if count := strings.Count(page, "<script"); count != 1 {
		t.Errorf("the page carries %d script elements, want exactly the one local script", count)
	}
	assertPageReachesOutNowhere(t, page, "the companion page")
	assertNames(t, page, "the page script", []string{
		// Scroll the tail box to its end — the window shows the end
		// of the log.
		`getElementById("service-log-tail")`,
		"tail.scrollTop = tail.scrollHeight",
		// Forward the one fixed action to the host-bound function.
		"window." + companionOpenLogAction,
		`addEventListener("click"`,
	})
	if !strings.HasPrefix(page, "<!doctype html>") {
		t.Errorf("the companion page = %q, want a self-contained document", page)
	}
}

// pageExternalPins lists every cheap route out of the companion document:
// network calls, sockets, beacons, frames, subresources and imports. The
// page fetches nothing — the companion listens to nothing — so ANY of these
// is a defect, not a style choice: a page that starts reaching out fails
// here instead of passing as "just one more attribute". Both shapes of the
// page (with and without a service-log section) are pinned against it.
var pageExternalPins = []string{
	// Network calls, sockets and beacons.
	"fetch(", "XMLHttpRequest", "WebSocket", "EventSource", "sendBeacon",
	// Frames and embedded subresources.
	"<iframe", "<frame", "<object", "<embed", "<img", "<link ",
	"<script src", `src="http`, "srcset=",
	// Imports that pull code or styles from anywhere else.
	"import(", "@import", "importScripts", "new Worker",
	// A URL token in code (CSS url(), computed redirects).
	"url(",
}

// assertPageReachesOutNowhere pins the external-resource list against one
// rendered page.
func assertPageReachesOutNowhere(t *testing.T, page, context string) {
	t.Helper()
	for _, forbidden := range pageExternalPins {
		if strings.Contains(page, forbidden) {
			t.Errorf("%s carries %q: it must not reach out of the document", context, forbidden)
		}
	}
}

// TestCompanionPageScriptCallsTheOneBoundActionName pins the single source
// of truth of the bridge name: the page's script may reference ONLY the
// name substituted from companionOpenLogAction — the very constant
// companionWindowActions binds as its map key — so renaming the constant
// without the script (or the script without the constant) cannot pass: the
// template derives the name at generation time, and this test fails
// outright if any other literal name reaches the script, if the
// substitution placeholder leaks into the page, or if the constant stops
// being a usable JavaScript identifier.
//
// It also pins the two element ids the script looks up by: an id renamed in
// one of its two places is a dead button, and the click would silently do
// nothing.
func TestCompanionPageScriptCallsTheOneBoundActionName(t *testing.T) {
	identifier := regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)
	if !identifier.MatchString(companionOpenLogAction) {
		t.Fatalf("companionOpenLogAction = %q is not a JavaScript identifier: "+
			"substituting it would produce a script that cannot parse", companionOpenLogAction)
	}

	page := companionPage(companionPageState{
		State: "The HFDesk service is stopped.",
		ServiceLog: serviceLogSection{
			Path: `C:\ProgramData\HFDesk\logs\hfdesk.log`,
			Tail: "record one\nrecord two",
		},
	})

	// Every window.<name> reference of the document is THE bound action:
	// no second, foreign name may sit beside it, and the guard and the
	// call both carry it (at least twice).
	want := "window." + companionOpenLogAction
	refs := regexp.MustCompile(`window\.[A-Za-z_$][A-Za-z0-9_$]*`).FindAllString(page, -1)
	if len(refs) < 2 {
		t.Errorf("the script references the bound action %d time(s), want the guard and the call: %v", len(refs), refs)
	}
	for _, ref := range refs {
		if ref != want {
			t.Errorf("the page calls %q, want only the bound action %q: a foreign name is a dead button", ref, want)
		}
	}
	if strings.Contains(page, serviceLogScriptActionPlaceholder) {
		t.Errorf("the page carries the unsubstituted placeholder %q: the generation step is broken:\n%s",
			serviceLogScriptActionPlaceholder, page)
	}

	// The bare name must not appear either: a call without the window.
	// prefix is just as dead when the constant is renamed. The scan runs
	// inside the script element only — the page legitimately prints the
	// log file name (hfdesk.log) as text.
	script := regexp.MustCompile(`(?s)<script>.*?</script>`).FindString(page)
	if script == "" {
		t.Fatal("the page carries no script: the section renders no tail and no button")
	}
	for _, name := range regexp.MustCompile(`hfdesk[A-Za-z0-9_$]*`).FindAllString(script, -1) {
		if name != companionOpenLogAction {
			t.Errorf("the script carries the literal %q, want only companionOpenLogAction %q: "+
				"a hardcoded name is a dead button", name, companionOpenLogAction)
		}
	}

	// The button and the tail box are looked up by the very ids the
	// markup carries.
	assertNames(t, page, "the bridge elements", []string{
		`id="open-service-log"`, `getElementById("open-service-log")`,
		`id="service-log-tail"`, `getElementById("service-log-tail")`,
	})
}

// TestCompanionPageShowsTheAddressAsFactAndEscapesEverything proves the
// two honest shapes of the address line — a registration without a
// readable URL names the address itself, never an invented link, and a
// machine without a registration carries no address at all — and that
// every value from the machine is escaped, so none of it can become
// markup.
func TestCompanionPageShowsTheAddressAsFactAndEscapesEverything(t *testing.T) {
	t.Run("an unreadable address is named, not linked", func(t *testing.T) {
		page := companionPage(companionPageState{
			State:   "The HFDesk service is stopped.",
			Address: "The registration names no listen address, so the address of the service is not known.",
		})
		if strings.Contains(page, "<a href") {
			t.Errorf("the page links an address it could not read as a URL: %q", page)
		}
		assertNames(t, page, "the address line", []string{
			"The registration names no listen address",
		})
	})

	t.Run("no registration and no log carry no lines", func(t *testing.T) {
		page := companionPage(companionPageState{
			State: "HFDesk is not registered as a Windows service.",
		})
		if strings.Contains(page, "<a href") {
			t.Errorf("the page links an address beside a machine without a registration: %q", page)
		}
		if strings.Contains(page, "The log file is at") {
			t.Errorf("the page names a log file of a launch that has none: %q", page)
		}
		// A model without a service-log source carries no section and
		// therefore no script at all: there is nothing to scroll and
		// no button to forward.
		if strings.Contains(page, "<script") || strings.Contains(page, "Service log") {
			t.Errorf("the page carries the service-log section although its model has no source: %q", page)
		}
		// The sectionless shape reaches out of the document just as
		// little as the full one.
		assertPageReachesOutNowhere(t, page, "the sectionless page")
	})

	t.Run("every value is escaped", func(t *testing.T) {
		page := companionPage(companionPageState{
			State:   `<script>alert("state")</script>`,
			Address: `127.0.0.1:8080&<i>`,
			LogPath: `<b>log</b>`,
			ServiceLog: serviceLogSection{
				Path: `C:\logs\<i>hfdesk.log`,
				Tail: `</pre><script>alert("tail")</script>`,
			},
		})
		for _, raw := range []string{
			`<script>alert("state")</script>`, `&<i>`, `<b>log</b>`,
			`</pre><script>alert("tail")</script>`,
		} {
			if strings.Contains(page, raw) {
				t.Errorf("the page carries the raw value %q: nothing from the machine may become markup", raw)
			}
		}
		assertNames(t, page, "the escaped page", []string{
			"&lt;script&gt;",
			"&amp;&lt;i&gt;",
			"&lt;b&gt;log&lt;/b&gt;",
			"&lt;/pre&gt;&lt;script&gt;",
		})

		unreadable := companionPage(companionPageState{
			State: "The HFDesk service is stopped.",
			ServiceLog: serviceLogSection{
				Path:      `C:\logs\hfdesk.log`,
				ReadError: `access <i> denied`,
			},
		})
		assertNamesWithout(t, unreadable, "the escaped reason", []string{
			"could not be read: access &lt;i&gt; denied.",
		}, []string{`access <i> denied`})
	})
}

// TestCompanionPageServiceLogSectionHonestStates pins the three honest
// shapes of the service-log section on the page: a file the service has not
// created yet is one line and offers no action (there is nothing to open),
// a file that could not be read states its reason as one line and still
// offers the action (the default handler may show what the page was denied),
// and a readable file shows its bounded tail with the single
// open-the-full-file button.
func TestCompanionPageServiceLogSectionHonestStates(t *testing.T) {
	t.Run("a log that does not exist yet", func(t *testing.T) {
		page := companionPage(companionPageState{
			State:      "The HFDesk service is stopped.",
			ServiceLog: serviceLogSection{Path: `C:\ProgramData\HFDesk\logs\hfdesk.log`, Missing: true},
		})
		assertNamesWithout(t, page, "the missing-log section", []string{
			"The service log file has not been created yet.",
			`<p class="label">Service log</p>`,
		}, []string{"Open the full log file", "<pre"})
	})

	t.Run("a log that could not be read", func(t *testing.T) {
		page := companionPage(companionPageState{
			State: "The HFDesk service is stopped.",
			ServiceLog: serviceLogSection{
				Path:      `C:\ProgramData\HFDesk\logs\hfdesk.log`,
				ReadError: "open the file: access is denied",
			},
		})
		assertNames(t, page, "the unreadable-log section", []string{
			"The service log file could not be read: open the file: access is denied.",
			"Open the full log file",
		})
		if strings.Contains(page, "<pre") {
			t.Errorf("the unreadable section renders a tail although none could be read: %q", page)
		}
	})

	t.Run("a readable log shows its tail and the one action", func(t *testing.T) {
		page := companionPage(companionPageState{
			State: "The HFDesk service is stopped.",
			ServiceLog: serviceLogSection{
				Path: `C:\ProgramData\HFDesk\logs\hfdesk.log`,
				Tail: "oldest line\nnewest line",
			},
		})
		assertNames(t, page, "the readable-log section", []string{
			"<pre id=\"service-log-tail\">oldest line\nnewest line</pre>",
			"Open the full log file",
			"hfdeskOpenServiceLog",
		})
	})
}

// TestReadServiceLogTailBoundsOrderAndEndsAtTheFileEnd pins the read of the
// tail itself, independent of any window: the byte bound caps what a large
// file yields (the beginning of the file never leaves the disk as a whole),
// every shown line is whole and in file order, the last line shown is the
// newest line of the file, and the line bound caps what a small file with
// many short lines yields.
func TestReadServiceLogTailBoundsOrderAndEndsAtTheFileEnd(t *testing.T) {
	t.Run("a large file is read only at its end, under the byte bound", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "hfdesk.log")
		// ~400 bytes per line: 2000 lines are far more than the byte
		// bound, so the read has to seek to the end.
		var file strings.Builder
		for i := 0; i < 2000; i++ {
			line := fmt.Sprintf("line %06d:", i)
			fmt.Fprintf(&file, "%s%.*s\n", line, 400-len(line), strings.Repeat("x", 400))
		}
		file.WriteString("line 999999: newest\n")
		if err := os.WriteFile(path, []byte(file.String()), 0o600); err != nil {
			t.Fatalf("write the log: %v", err)
		}

		tail, err := readServiceLogTail(path)
		if err != nil {
			t.Fatalf("readServiceLogTail: %v", err)
		}
		if len(tail) > serviceLogTailBytes {
			t.Errorf("the tail is %d bytes, want at most the %d byte bound: a large file must not be read whole",
				len(tail), serviceLogTailBytes)
		}
		if strings.Contains(tail, "line 000000:") {
			t.Error("the tail carries the first line of the file: the file was read as a whole")
		}
		if !strings.HasSuffix(tail, "line 999999: newest") {
			t.Errorf("the tail does not end with the newest line of the file: %q", tail[len(tail)-80:])
		}
		lines := strings.Split(tail, "\n")
		if len(lines) > serviceLogTailLines {
			t.Errorf("the tail carries %d lines, want at most %d", len(lines), serviceLogTailLines)
		}
		// Every line is whole and in file order: the first shown line
		// starts at a line boundary and the numbers only grow.
		first := strings.Split(lines[0], ":")[0]
		if !strings.HasPrefix(lines[0], "line ") {
			t.Errorf("the first shown line is a fragment: %q", lines[0])
		}
		previous := -1
		for _, line := range lines {
			var number int
			if _, err := fmt.Sscanf(strings.SplitN(line, ":", 2)[0], "line %d", &number); err != nil {
				t.Fatalf("the line %q is not a whole line of the file: %v", line, err)
			}
			if number <= previous {
				t.Errorf("the line %d follows %d: the order of the file was not kept", number, previous)
			}
			previous = number
		}
		if first == "" {
			t.Error("the first shown line is empty")
		}
	})

	t.Run("a small file is capped by the line bound, in order", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "hfdesk.log")
		var file strings.Builder
		for i := 1; i <= 1000; i++ {
			fmt.Fprintf(&file, "L%04d\n", i)
		}
		if err := os.WriteFile(path, []byte(file.String()), 0o600); err != nil {
			t.Fatalf("write the log: %v", err)
		}

		tail, err := readServiceLogTail(path)
		if err != nil {
			t.Fatalf("readServiceLogTail: %v", err)
		}
		lines := strings.Split(tail, "\n")
		if len(lines) != serviceLogTailLines {
			t.Errorf("the tail carries %d lines, want the last %d of the file", len(lines), serviceLogTailLines)
		}
		if lines[0] != "L0801" || lines[len(lines)-1] != "L1000" {
			t.Errorf("the tail spans %q..%q, want L0801..L1000: the last lines of the file, in order",
				lines[0], lines[len(lines)-1])
		}
	})

	t.Run("a short file is shown whole, without its line endings", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "hfdesk.log")
		if err := os.WriteFile(path, []byte("first\r\nsecond\r\nthird\r\n"), 0o600); err != nil {
			t.Fatalf("write the log: %v", err)
		}
		tail, err := readServiceLogTail(path)
		if err != nil {
			t.Fatalf("readServiceLogTail: %v", err)
		}
		if tail != "first\nsecond\nthird" {
			t.Errorf("the tail = %q, want the whole file as three whole lines", tail)
		}
		if strings.Contains(tail, "\r") {
			t.Errorf("the tail keeps carriage returns: %q", tail)
		}
	})

	t.Run("a missing file reports its error instead of a tail", func(t *testing.T) {
		_, err := readServiceLogTail(filepath.Join(t.TempDir(), "absent.log"))
		if !os.IsNotExist(err) {
			t.Errorf("readServiceLogTail of a missing file = %v, want a not-exist error", err)
		}
	})
}

// TestServiceLogSectionStates pins the loader of the section: missing,
// unreadable and readable are told apart honestly, and a resolved path
// travels with the section — the very path the open action hands to the
// default handler.
func TestServiceLogSectionStates(t *testing.T) {
	t.Run("a file that does not exist is missing", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "hfdesk.log")
		section := serviceLogSectionAt(path)
		if !section.Missing || section.ReadError != "" || section.Tail != "" {
			t.Errorf("the section = %+v, want the honest missing state", section)
		}
		if section.Path != path {
			t.Errorf("the section path = %q, want the file it spoke about %q", section.Path, path)
		}
	})

	t.Run("a file that cannot be read carries its reason", func(t *testing.T) {
		// A directory in the place of the file: opening or reading it
		// fails on every platform, which is exactly the class of a
		// file this process may not read.
		path := t.TempDir()
		section := serviceLogSectionAt(path)
		if section.Missing || section.ReadError == "" || section.Tail != "" {
			t.Errorf("the section = %+v, want one line of reason and no tail", section)
		}
		if strings.ContainsAny(section.ReadError, "\r\n") {
			t.Errorf("the reason spans several lines: %q", section.ReadError)
		}
	})

	t.Run("a readable file carries its tail", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "hfdesk.log")
		if err := os.WriteFile(path, []byte("record one\nrecord two\n"), 0o600); err != nil {
			t.Fatalf("write the log: %v", err)
		}
		section := serviceLogSectionAt(path)
		if section.Missing || section.ReadError != "" || section.Tail != "record one\nrecord two" {
			t.Errorf("the section = %+v, want the tail of the file", section)
		}
	})

	t.Run("no path claims nothing", func(t *testing.T) {
		section := serviceLogSectionAt("")
		if section.Path != "" || section.Missing || section.ReadError != "" || section.Tail != "" {
			t.Errorf("the section = %+v, want the empty section that claims nothing", section)
		}
	})
}

// TestServiceLogPathFollowsTheRegistrationOrTheDefault pins the source of
// the file the section reads and the action opens: a log directory the
// registration names decides, and a machine without one falls back to the
// default of the machine-wide scope — the documented ProgramData folder on
// Windows — always joined with the file name the service really writes.
func TestServiceLogPathFollowsTheRegistrationOrTheDefault(t *testing.T) {
	if got, want := serviceLogPath(`D:\HFDesk-service`), filepath.Join(`D:\HFDesk-service`, "hfdesk.log"); got != want {
		t.Errorf("serviceLogPath of a registered directory = %q, want %q", got, want)
	}
	paths, err := service.DefaultPaths(service.ScopeSystem)
	if err != nil {
		t.Fatalf("the default paths of the machine-wide scope: %v", err)
	}
	if got, want := serviceLogPath(""), filepath.Join(paths.LogDir, "hfdesk.log"); got != want {
		t.Errorf("serviceLogPath without a registration = %q, want the default %q", got, want)
	}
	if !strings.HasSuffix(serviceLogPath(""), serviceLogFileName) {
		t.Errorf("serviceLogPath = %q, want the active log file of the service", serviceLogPath(""))
	}
}

// TestCompanionReportsFollowTheWindowRule pins every report of the
// companion family under the established window rule — the state plus AT
// MOST one action, with the standard log-path line — as ONE map over all
// four reports, the way the log-path rule is pinned as one map: a report
// that joins the family without the rule, or a report that grows a second
// action, fails here instead of passing unnoticed.
//
// The family's action vocabulary is the three commands a report of this
// family can ask for (read the state, install the runtime, start the app
// again); each entry forbids the ones that are not its own, so "at most
// one" is a count and not a hope. The two honest exceptions stay explicit:
// the relaunch wait report runs before the log exists and names no log at
// all, and the two P5b service-log reports are state (+ cause) with NO
// action — "at most one" includes none, because there is nothing a click
// could do about either state.
func TestCompanionReportsFollowTheWindowRule(t *testing.T) {
	const (
		launchLogPath  = `C:\Users\First Last\hfdesk.log`
		serviceLogPath = `C:\ProgramData\HFDesk\logs\hfdesk.log`
	)
	previous := runLogPath
	runLogPath = launchLogPath
	t.Cleanup(func() { runLogPath = previous })

	cause := errors.New("the default handler refused the file")

	// The action vocabulary of the family: a report carries at most one
	// of these, and every entry below names which one is its own.
	const (
		statusCommand        = "hfdesk service status"
		installRuntimeAction = "https://developer.microsoft.com/microsoft-edge/webview2/"
		relaunchAction       = "Start the desktop app again in a moment"
	)

	for name, tc := range map[string]struct {
		text string
		// want is the state, the report's one action (when it has
		// one) and its log-path expectation.
		want []string
		// forbidden is every action that is NOT this report's own,
		// plus the facts its state contradicts.
		forbidden []string
	}{
		"the WebView2 report": {
			text: companionWebView2MissingText(),
			want: []string{
				// The state: the window cannot be shown.
				"cannot be shown",
				// The one action: install the runtime.
				installRuntimeAction,
				// The standard log-path line.
				"The log file is at " + launchLogPath,
			},
			forbidden: []string{
				statusCommand, relaunchAction,
				// This report is not about the service log.
				"The service log",
			},
		},
		"the relaunch wait report": {
			text: companionRelaunchWaitTimeoutText(),
			want: []string{
				// The state: the session is still held.
				"still holds the single-instance object",
				// The one action: start again in a moment.
				relaunchAction,
			},
			// This report runs BEFORE the log file is opened — the
			// lock is taken first — so naming a log would point at a
			// file this launch does not write: the established rule
			// of the lock reports is the rule here.
			forbidden: []string{
				"The log file is at", statusCommand, installRuntimeAction,
			},
		},
		"the service-log open failure report": {
			text: serviceLogOpenFailedText(serviceLogPath, cause),
			want: []string{
				// The state: the file could not be opened.
				"could not be opened",
				// The cause: the one fact line of this window.
				cause.Error(),
				// The path of the service log itself, and the
				// standard line of this launch.
				"The service log is at " + serviceLogPath,
				"The log file is at " + launchLogPath,
			},
			// State plus cause, no action: nothing a click here can
			// fix, so the report offers none of the family's
			// commands — and it never claims the file is absent.
			forbidden: []string{
				statusCommand, installRuntimeAction, relaunchAction,
				"Do you want to", "has not been created yet",
			},
		},
		"the service-log missing report": {
			text: serviceLogMissingText(serviceLogPath),
			want: []string{
				// The honest state: the service wrote no log yet.
				"has not been created yet",
				// Where that log will be, and the standard line
				// of this launch.
				"The service writes its log at " + serviceLogPath,
				"The log file is at " + launchLogPath,
			},
			// State only, no action: there is nothing to open, so
			// the report offers none of the family's commands — and
			// it never claims an open was refused.
			forbidden: []string{
				statusCommand, installRuntimeAction, relaunchAction,
				"Do you want to", "could not be opened",
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			assertNames(t, tc.text, name, tc.want)
			for _, unwanted := range tc.forbidden {
				if strings.Contains(tc.text, unwanted) {
					t.Errorf("the %s carries %q beside its state and its one action:\n%s",
						name, unwanted, tc.text)
				}
			}
			// A report that names the log of this launch names it
			// once — never twice beside a path it also states
			// itself.
			if got := strings.Count(tc.text, "The log file is at"); got > 1 {
				t.Errorf("the %s names the log file %d times, want at most once:\n%s", name, got, tc.text)
			}
		})
	}
}
