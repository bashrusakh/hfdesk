// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"reflect"
	"strings"
	"testing"
	"time"
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
// (an anchor a click follows), the standard log-path line, the reserved
// place of the future service log section — and no script anywhere: the
// page is a document, never a client of a listener, because nothing of a
// companion listens.
func TestCompanionPageRendersStateLinkAndLog(t *testing.T) {
	page := companionPage(companionPageState{
		State:   "The HFDesk service is stopped.",
		WebURL:  "http://127.0.0.1:18080",
		LogPath: `C:\Users\First Last\hfdesk.log`,
	})

	assertNames(t, page, "the companion page", []string{
		"<h1>The HFDesk service is stopped.</h1>",
		`<a href="http://127.0.0.1:18080">http://127.0.0.1:18080</a>`,
		"The log file is at",
		`<code>C:\Users\First Last\hfdesk.log</code>`,
		// The interface is ready for the section whose source the
		// user has not decided yet — the place is reserved, the
		// section itself renders nothing.
		"service log section renders here",
	})
	if strings.Contains(page, "<script") {
		t.Error("the companion page carries a script: it would need a channel to a listener, and the companion listens to nothing")
	}
	if !strings.HasPrefix(page, "<!doctype html>") {
		t.Errorf("the companion page = %q, want a self-contained document", page)
	}
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
	})

	t.Run("every value is escaped", func(t *testing.T) {
		page := companionPage(companionPageState{
			State:   `<script>alert("state")</script>`,
			Address: `127.0.0.1:8080&<i>`,
			LogPath: `<b>log</b>`,
		})
		for _, raw := range []string{`<script>alert("state")</script>`, `&<i>`, `<b>log</b>`} {
			if strings.Contains(page, raw) {
				t.Errorf("the page carries the raw value %q: nothing from the machine may become markup", raw)
			}
		}
		assertNames(t, page, "the escaped page", []string{
			"&lt;script&gt;",
			"&amp;&lt;i&gt;",
			"&lt;b&gt;log&lt;/b&gt;",
		})
	})
}

// TestCompanionReportsFollowTheWindowRule pins the report texts of the
// companion under the established rule for every window and report of this
// family — the state plus ONE action, with the standard log-path line —
// and the honest pre-log exception of the relaunch wait report: a launch
// that has no log file yet names none, because there is none (the
// established rule of the lock reports).
func TestCompanionReportsFollowTheWindowRule(t *testing.T) {
	t.Run("the WebView2 report carries its one action and the log path", func(t *testing.T) {
		previous := runLogPath
		runLogPath = `C:\Users\First Last\hfdesk.log`
		t.Cleanup(func() { runLogPath = previous })

		text := companionWebView2MissingText()
		assertNames(t, text, "the WebView2 report", []string{
			// The state: the window cannot be shown.
			"cannot be shown",
			// The one action: install the runtime.
			"https://developer.microsoft.com/microsoft-edge/webview2/",
			// The standard log-path line.
			"The log file is at " + runLogPath,
		})
		if strings.Contains(text, "hfdesk service status") {
			t.Errorf("the report %q carries a second action beside the installation", text)
		}
	})

	t.Run("the relaunch wait report is state plus one action without a log", func(t *testing.T) {
		text := companionRelaunchWaitTimeoutText()
		assertNames(t, text, "the relaunch wait report", []string{
			// The state: the session is still held.
			"still holds the single-instance object",
			// The one action: start again in a moment.
			"Start the desktop app again in a moment",
		})
		// This report runs BEFORE the log file is opened — the lock is
		// taken first — so naming a log would point at a file this
		// launch does not write: the established rule of the lock
		// reports is the rule here.
		if strings.Contains(text, "The log file is at") {
			t.Errorf("the report %q names a log file the launch never opened", text)
		}
	})
}
