// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/service"
	"github.com/bashrusakh/hfdesk/internal/serviceview"
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

// TestLogsFallbackPageRendersTheRuntimeFrame pins the fallback document of
// the Logs window: the frame of the live page — the shell, the header with
// the offline line and the build version, the service control, the fact
// banner and the status strip — the log panel with its toolbar and the
// table of the tail, and the one action that retries the live page. The
// address is stated as a fact, never linked, and the document carries no
// Open HFDesk button (the address just proved it is dead), no Follow and no
// Clear view (nothing streams in an offline document) and no second
// Service log strip.
func TestLogsFallbackPageRendersTheRuntimeFrame(t *testing.T) {
	const logPath = `C:\ProgramData\HFDesk\logs\hfdesk.log`
	page := logsFallbackPage(logsFallbackState{
		Version: "1.2.3",
		Address: "http://127.0.0.1:18080",
		Service: serviceview.From("stopped", false, false),
		LogPath: logPath,
		Log: serviceLogSection{
			Path: logPath,
			Tail: `{"seq":1,"time":"2026-11-25T10:00:00Z","level":"INFO","component":"server","message":"started","attrs":{}}`,
		},
	})

	assertNames(t, page, "the fallback document", []string{
		"<title>HFDesk - Logs</title>",
		`<div class="runtime-shell">`,
		`<header class="runtime-header">`,
		`<span class="logo-text">HFDesk</span>`,
		`<span class="status-indicator runtime-status-indicator is-error" id="statusDot"></span>`,
		`<span id="statusLabel">Offline</span>`,
		`<span class="runtime-version" id="versionLabel">Version 1.2.3</span>`,
		`id="serviceControl"`,
		`id="serviceLabel">Service: stopped</span>`,
		`The web UI at <code>http://127.0.0.1:18080</code> does not answer.`,
		`id="try-again" type="button">Try again</button>`,
		`<section class="runtime-strip" aria-label="Runtime status">`,
		`<span class="runtime-stat-label">Server</span>`,
		`<span class="runtime-stat-label">Active jobs</span>`,
		`<span class="runtime-stat-label">Log file</span>`,
		`<span class="config-chip"><code id="logFile">C:\ProgramData\HFDesk\logs\hfdesk.log</code></span>`,
		`<section class="runtime-log-panel">`,
		`id="levelFilter"`,
		`id="searchInput"`,
		`id="copyButton"`,
		`id="openLogsButton"`,
		`id="openLogFileButton"`,
		`<div class="runtime-log-head" aria-hidden="true">`,
		`<div class="runtime-log-body" id="logBody"></div>`,
		`Offline &mdash; last 1 lines`,
		`at most 200 lines, 64 KiB`,
	})

	// The frame is the runtime page's frame: no Open HFDesk button, no
	// streaming controls, and no second strip that would present the
	// service log as another section of facts.
	assertNamesWithout(t, page, "the fallback frame", nil, []string{
		"Open HFDesk",
		`id="followToggle"`,
		`id="clearButton"`,
		`<span class="runtime-stat-label">Service log</span>`,
		"<pre",
	})
	// The address is stated, never linked: a link to the address that
	// just failed to answer would be a dead path in the window.
	assertNamesWithout(t, page, "the address line", nil, []string{"<a ", "href="})

	// The section order: the header leads, the fact follows, the strip
	// states the runtime, and the log panel comes last — the document
	// reads top down from what is true to what can be done.
	header := strings.Index(page, `<header class="runtime-header">`)
	banner := strings.Index(page, `id="errorBanner"`)
	strip := strings.Index(page, `<section class="runtime-strip"`)
	panel := strings.Index(page, `<section class="runtime-log-panel">`)
	if !(header >= 0 && header < banner && banner < strip && strip < panel) {
		t.Errorf("the document order is header %d, banner %d, strip %d, panel %d, want header < banner < strip < panel",
			header, banner, strip, panel)
	}

	if count := strings.Count(page, "<script"); count != 1 {
		t.Errorf("the document carries %d script elements, want exactly the one local script", count)
	}
	if count := strings.Count(page, "<style"); count != 1 {
		t.Errorf("the document carries %d style elements, want exactly the one inline stylesheet", count)
	}
	assertPageReachesOutNowhere(t, page, "the fallback document")
	if !strings.HasPrefix(page, "<!doctype html>") {
		t.Errorf("the fallback document = %q, want a self-contained document", page)
	}
}

// pageExternalPins lists every cheap route out of the fallback document
// of the Logs window: network calls, sockets, beacons, frames,
// subresources and imports. The document fetches nothing — it is the
// fallback beside an address that does not answer, and the companion
// itself listens to nothing — so ANY of these is a defect, not a style
// choice: a document that starts reaching out fails here instead of
// passing as "just one more attribute". (The LIVE page of the window is a
// different mechanism: the window host navigates the WebView2 to the
// address of the service — an outgoing page load the host owns, not a
// channel of any document.)
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
// rendered document.
func assertPageReachesOutNowhere(t *testing.T, page, context string) {
	t.Helper()
	for _, forbidden := range pageExternalPins {
		if strings.Contains(page, forbidden) {
			t.Errorf("%s carries %q: it must not reach out of the document", context, forbidden)
		}
	}
}

// fallbackMenuItems answers the state of every item of the service menu of
// a rendered fallback document: "hidden" when the markup keeps it out of
// the menu, "disabled" when it is shown but cannot act, and "enabled" when
// it can.
func fallbackMenuItems(page string) map[string]string {
	pattern := regexp.MustCompile(`<button class="[^"]*" id="service(Install|Start|Stop|Update|Delete)Item"[^>]*>`)
	state := map[string]string{}
	for _, match := range pattern.FindAllStringSubmatch(page, -1) {
		key := strings.ToLower(match[1])
		tag := match[0]
		switch {
		case strings.Contains(tag, " hidden"):
			state[key] = "hidden"
		case strings.Contains(tag, " disabled"):
			state[key] = "disabled"
		default:
			state[key] = "enabled"
		}
	}
	return state
}

// fallbackControlShown tells whether the rendered document carries the
// service control (the wrapper of runtime.html) outside its hidden state.
func fallbackControlShown(page string) bool {
	return strings.Contains(page, `<div class="runtime-service" id="serviceControl">`)
}

// TestLogsFallbackPageServiceMenuFollowsTheStateTable pins the service
// control of the fallback document against the ONE table: the line, the
// tone and the actions of internal/serviceview, and nothing else. A broken
// registration never offers Start (its program is gone) and offers Update;
// a failed one offers Start and Update; a transition shows its items
// disabled; and a read that proved nothing has no control at all.
func TestLogsFallbackPageServiceMenuFollowsTheStateTable(t *testing.T) {
	allHidden := map[string]string{
		"install": "hidden", "start": "hidden", "stop": "hidden",
		"update": "hidden", "delete": "hidden",
	}
	settled := map[string]string{
		"install": "hidden", "start": "disabled", "stop": "enabled",
		"update": "enabled", "delete": "enabled",
	}

	for _, tc := range []struct {
		name     string
		snapshot serviceview.Snapshot
		label    string
		tone     string
		shown    bool
		items    map[string]string
	}{
		{
			name:     "a machine without a registration offers the installation",
			snapshot: serviceview.From("not-installed", false, false),
			label:    "Service: not installed",
			tone:     "is-muted",
			shown:    true,
			items: map[string]string{
				"install": "enabled", "start": "hidden", "stop": "hidden",
				"update": "hidden", "delete": "hidden",
			},
		},
		{
			name:     "a running service offers the stop",
			snapshot: serviceview.From("running", true, false),
			label:    "Service: running",
			tone:     "is-success",
			shown:    true,
			items:    settled,
		},
		{
			name:     "a stopped service offers the start",
			snapshot: serviceview.From("stopped", false, false),
			label:    "Service: stopped",
			tone:     "is-muted",
			shown:    true,
			items: map[string]string{
				"install": "hidden", "start": "enabled", "stop": "disabled",
				"update": "enabled", "delete": "enabled",
			},
		},
		{
			name:     "a failed service offers the start and the update",
			snapshot: serviceview.From("failed", false, false),
			label:    "Service: failed",
			tone:     "is-error",
			shown:    true,
			items: map[string]string{
				"install": "hidden", "start": "enabled", "stop": "disabled",
				"update": "enabled", "delete": "enabled",
			},
		},
		{
			name:     "a broken registration offers the update and never the start",
			snapshot: serviceview.From("broken", false, false),
			label:    "Service: failed",
			tone:     "is-error",
			shown:    true,
			items: map[string]string{
				"install": "hidden", "start": "hidden", "stop": "disabled",
				"update": "enabled", "delete": "enabled",
			},
		},
		{
			name:     "a service on its way up shows every lifecycle item disabled",
			snapshot: serviceview.From("starting", false, false),
			label:    "Service: starting",
			tone:     "is-warning",
			shown:    true,
			items: map[string]string{
				"install": "hidden", "start": "disabled", "stop": "disabled",
				"update": "disabled", "delete": "disabled",
			},
		},
		{
			name:     "a service on its way down shows every lifecycle item disabled",
			snapshot: serviceview.From("stopping", false, false),
			label:    "Service: stopping",
			tone:     "is-warning",
			shown:    true,
			items: map[string]string{
				"install": "hidden", "start": "disabled", "stop": "disabled",
				"update": "disabled", "delete": "disabled",
			},
		},
		{
			name:     "a state the supervisor could not report has no control",
			snapshot: serviceview.From("unknown", false, false),
			shown:    false,
			items:    allHidden,
		},
		{
			name:     "a read that failed has no control",
			snapshot: serviceview.From("", false, true),
			shown:    false,
			items:    allHidden,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := logsFallbackPage(logsFallbackState{
				Address: "http://127.0.0.1:18080",
				Service: tc.snapshot,
			})

			if got := fallbackControlShown(page); got != tc.shown {
				t.Errorf("the control is shown = %t, want %t:\n%s", got, tc.shown, page)
			}
			if got := fallbackMenuItems(page); !reflect.DeepEqual(got, tc.items) {
				t.Errorf("the menu items = %v, want %v", got, tc.items)
			}
			if !tc.shown {
				if strings.Contains(page, `<div class="runtime-service" id="serviceControl">`) {
					t.Error("the document draws a control although the read proved nothing")
				}
				return
			}

			assertNames(t, page, "the service control", []string{
				`id="serviceLabel">` + tc.label + `</span>`,
				`runtime-service-dot ` + tc.tone + `" id="serviceDot"`,
			})

			// The separator stands only before a visible Delete beside
			// another visible item.
			earlier := tc.items["install"] != "hidden" || tc.items["start"] != "hidden" ||
				tc.items["stop"] != "hidden" || tc.items["update"] != "hidden"
			wantSeparator := tc.items["delete"] != "hidden" && earlier
			gotSeparator := strings.Contains(page, `id="serviceSeparator" role="separator"></div>`)
			if gotSeparator != wantSeparator {
				t.Errorf("the separator is shown = %t, want %t", gotSeparator, wantSeparator)
			}
		})
	}
}

// fallbackScript returns the one script element of a rendered document.
func fallbackScript(t *testing.T, page string) string {
	t.Helper()
	script := regexp.MustCompile(`(?s)<script>.*?</script>`).FindString(page)
	if script == "" {
		t.Fatal("the document carries no script: the actions have no forwarder")
	}
	return script
}

// TestLogsFallbackPageScriptCallsOnlyBoundActionNames pins the source of
// truth of the bridge names: the script may reference ONLY the names
// substituted from the bound-action constants — the open actions, the retry
// and all five service actions — so a rename on either side cannot pass.
// The configuration carries the names; the wiring calls them by the map
// entry, never by a literal of its own.
func TestLogsFallbackPageScriptCallsOnlyBoundActionNames(t *testing.T) {
	identifier := regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)
	bound := []string{
		openLogFileAction, openLogFolderAction, logsTryAgainAction,
		logsStartServiceAction, logsStopServiceAction,
		logsUpdateServiceAction, logsDeleteServiceAction,
		logsInstallServiceAction,
	}
	allowed := map[string]bool{}
	for _, name := range bound {
		if !identifier.MatchString(name) {
			t.Fatalf("%q is not a JavaScript identifier: "+
				"substituting it would produce a script that cannot parse", name)
		}
		allowed[name] = true
	}

	page := logsFallbackPage(logsFallbackState{
		Address: "http://127.0.0.1:18080",
		LogPath: `C:\ProgramData\HFDesk\logs\hfdesk.log`,
		Service: serviceview.From("stopped", false, false),
	})
	script := fallbackScript(t, page)

	for _, ref := range regexp.MustCompile(`hfdesk[A-Za-z0-9_$]*`).FindAllString(script, -1) {
		if !allowed[ref] {
			t.Errorf("the script carries the name %q, want only the bound actions: a foreign name is a dead button", ref)
		}
	}
	for _, name := range bound {
		if !strings.Contains(script, `"`+name+`"`) {
			t.Errorf("the script config never carries %q: the window host binds it", name)
		}
	}
	if strings.Contains(script, openUiAction) {
		t.Errorf("the fallback script names %s: the dead address is never opened from the document", openUiAction)
	}
	if strings.Contains(script, "{{") || strings.Contains(script, "}}") {
		t.Error("the document carries an unsubstituted placeholder")
	}
	if strings.Contains(script, "innerHTML") {
		t.Error("the script writes markup: every value must reach the DOM through textContent")
	}
	if strings.Contains(script, "fetch(") {
		t.Error("the script reaches out of the document")
	}

	// The wiring drives the shipped menu unit and looks its elements up by
	// the very ids the markup carries.
	assertNames(t, script, "the fallback wiring", []string{
		"window.HFDeskServiceControl.wire",
		`getElementById('serviceTrigger')`,
		`getElementById('serviceMenu')`,
		`getElementById('logBody')`,
		`bindOpen('openLogsButton'`,
		`bindOpen('openLogFileButton'`,
		`bindOpen('try-again'`,
		"window.HFDeskLogView",
	})
}

// TestLogsFallbackPageHonestShapesAndEscapesEverything proves the honest
// shapes of the fallback document — an address that did not answer is
// stated as a fact and never linked, a machine without an address states
// the honest reason instead — and that every value from the machine is
// escaped, both in the HTML and in the one script that carries the tail.
func TestLogsFallbackPageHonestShapesAndEscapesEverything(t *testing.T) {
	t.Run("an address that did not answer is stated, never linked", func(t *testing.T) {
		page := logsFallbackPage(logsFallbackState{
			Address: "http://127.0.0.1:18080",
			Service: serviceview.From("stopped", false, false),
		})
		assertNamesWithout(t, page, "the address line", []string{
			"The web UI at <code>http://127.0.0.1:18080</code> does not answer.",
		}, []string{"<a ", "href="})
	})

	t.Run("a machine without an address states the honest reason", func(t *testing.T) {
		page := logsFallbackPage(logsFallbackState{
			Note:    "The registration names no address, so there is nothing to open.",
			Service: serviceview.From("not-installed", false, false),
		})
		assertNames(t, page, "the note line", []string{
			"The registration names no address, so there is nothing to open.",
		})
		if strings.Contains(page, "does not answer") {
			t.Errorf("the document claims an address did not answer although it names none")
		}
	})

	t.Run("no log path states the fact and offers no open action", func(t *testing.T) {
		page := logsFallbackPage(logsFallbackState{
			Note:    "The address of the web UI is not known.",
			Service: serviceview.From("stopped", false, false),
		})
		assertNames(t, page, "the pathless document", []string{
			`<h3 id="emptyTitle">The log file is not known.</h3>`,
			`<code id="logFile">&mdash;</code>`,
			`id="openLogsButton" type="button"`,
		})
		if !strings.Contains(page, `id="openLogsButton" type="button"
              title="Open the folder of the service log file" disabled`) {
			t.Error("the pathless document offers the log folder: there is nothing to open")
		}
		if !strings.Contains(page, `id="openLogFileButton" type="button"
              title="Open the service log file in the default program" disabled`) {
			t.Error("the pathless document offers the log file: there is nothing to open")
		}
		assertPageReachesOutNowhere(t, page, "the pathless document")
	})

	t.Run("every value is escaped", func(t *testing.T) {
		page := logsFallbackPage(logsFallbackState{
			Version: `1.0<&>`,
			Address: `127.0.0.1:8080&<i>`,
			LogPath: `<b>log</b>`,
			Service: serviceview.From("stopped", false, false),
			Log: serviceLogSection{
				Path: `<b>log</b>`,
				Tail: "plain </script><img src=x>&\u2028tail",
			},
		})
		for _, raw := range []string{
			`</script><img`, `<b>log</b>`, `1.0<&>`,
		} {
			if strings.Contains(page, raw) {
				t.Errorf("the document carries the raw value %q: nothing from the machine may become markup", raw)
			}
		}
		assertNames(t, page, "the escaped document", []string{
			"1.0&lt;&amp;&gt;",
			"127.0.0.1:8080&amp;&lt;i&gt;",
			`\u003c/script\u003e\u003cimg`,
		})
		if strings.Contains(page, "\u2028") {
			t.Error("the document carries a raw U+2028: the script could break on it")
		}
		if !strings.Contains(page, `\u2028`) {
			t.Error("the document does not escape U+2028 in the script data")
		}
	})
}

// TestLogsFallbackPageCarriesTheServiceLogTail proves the tail of the
// fallback: the records of the service log in the table of the live page —
// the wire records parsed, the lines the service did not write kept as
// their own text — under the honest meta (the last N lines, at most the
// documented bounds), with the missing, unreadable and empty file stated in
// the empty state of the panel, never in the status strip.
func TestLogsFallbackPageCarriesTheServiceLogTail(t *testing.T) {
	const logPath = `C:\ProgramData\HFDesk\logs\hfdesk.log`
	tail := `{"seq":4,"time":"2026-11-25T10:00:00Z","level":"WARN","component":"server","message":"first line","attrs":{"queued":2}}` + "\n" +
		`plain <b>line</b>`

	t.Run("a readable file shows its records in the table of the page", func(t *testing.T) {
		page := logsFallbackPage(logsFallbackState{
			Address: "http://127.0.0.1:18080",
			Service: serviceview.From("stopped", false, false),
			LogPath: logPath,
			Log:     serviceLogSection{Path: logPath, Tail: tail},
		})

		assertNames(t, page, "the fallback with a tail", []string{
			`<div class="runtime-log-head" aria-hidden="true">`,
			`id="logBody"`,
			`"message":"first line"`,
			`"level":"WARN"`,
			`"component":"server"`,
			`"queued":2`,
			`"raw":"plain \u003cb\u003eline\u003c/b\u003e"`,
			`Offline &mdash; last 2 lines`,
			`at most 200 lines, 64 KiB`,
			`id="emptyState" hidden`,
			`id="openLogsButton" type="button"
              title="Open the folder of the service log file">Open log folder</button>`,
			`id="openLogFileButton" type="button"
              title="Open the service log file in the default program">Open log file</button>`,
		})
		// The tail is the table of the live page, not a preformatted
		// block of a second shape.
		assertNamesWithout(t, page, "the fallback with a tail", nil,
			[]string{"<pre", `id="service-log-tail"`})
		if count := strings.Count(page, "<script"); count != 1 {
			t.Errorf("the document carries %d script elements, want exactly the one local script", count)
		}
		assertPageReachesOutNowhere(t, page, "the fallback with a tail")
	})

	for _, tc := range []struct {
		name    string
		section serviceLogSection
		title   string
	}{
		{
			name:    "a file that does not exist yet says so",
			section: serviceLogSection{Path: logPath, Missing: true},
			title:   "The service log file has not been created yet.",
		},
		{
			name:    "a file that could not be read names the reason",
			section: serviceLogSection{Path: logPath, ReadError: "access is denied."},
			title:   "The service log file could not be read: access is denied.",
		},
		{
			name:    "an empty file says so",
			section: serviceLogSection{Path: logPath},
			title:   "The service log is empty.",
		},
		{
			name:    "a model without a log source claims nothing",
			section: serviceLogSection{},
			title:   "The log file is not known.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			page := logsFallbackPage(logsFallbackState{
				Service: serviceview.From("stopped", false, false),
				LogPath: tc.section.Path,
				Log:     tc.section,
			})
			assertNames(t, page, "the empty state", []string{
				`<div class="empty-state" id="emptyState">`,
				`<h3 id="emptyTitle">` + tc.title + `</h3>`,
			})
			// The honest state sits inside the panel, never in the strip.
			if empty := strings.Index(page, `id="emptyState"`); empty < 0 || strings.Index(page, tc.title) < empty {
				t.Errorf("the honest state %q is not inside the empty state of the panel", tc.title)
			}
			assertPageReachesOutNowhere(t, page, "the fallback without records")
		})
	}
}

// TestReadServiceLogTailBoundsTheRead pins the mechanics the section reads
// with: a small file is shown whole in file order ending with the newest
// line, a file past the byte bound is read only at its end — cut to whole
// lines and capped at the line bound — and a missing file reports the
// not-exist error, which is the state the section turns into its honest
// "not created yet" shape. The bound is the view, never a failure.
func TestReadServiceLogTailBoundsTheRead(t *testing.T) {
	small := filepath.Join(t.TempDir(), "hfdesk.log")
	if err := os.WriteFile(small, []byte("one\ntwo\r\nthree\n"), 0o644); err != nil {
		t.Fatalf("write the small log: %v", err)
	}
	tail, err := readServiceLogTail(small)
	if err != nil {
		t.Fatalf("readServiceLogTail(%q) failed: %v", small, err)
	}
	if tail != "one\ntwo\nthree" {
		t.Errorf("the tail = %q, want the whole file in file order with the line endings normalized", tail)
	}

	big := filepath.Join(t.TempDir(), "big.log")
	var buffer strings.Builder
	for i := 0; i < 400; i++ {
		fmt.Fprintf(&buffer, "line %04d %s\n", i, strings.Repeat("x", 200))
	}
	if err := os.WriteFile(big, []byte(buffer.String()), 0o644); err != nil {
		t.Fatalf("write the big log: %v", err)
	}
	tail, err = readServiceLogTail(big)
	if err != nil {
		t.Fatalf("readServiceLogTail(%q) failed: %v", big, err)
	}
	lines := strings.Split(tail, "\n")
	if len(lines) != serviceLogTailLines {
		t.Errorf("the tail has %d lines, want the line bound %d", len(lines), serviceLogTailLines)
	}
	if len(tail) > serviceLogTailBytes {
		t.Errorf("the tail is %d bytes, want at most the byte bound %d", len(tail), serviceLogTailBytes)
	}
	if !strings.HasPrefix(lines[0], "line ") || strings.HasPrefix(lines[0], "line 0000") {
		t.Errorf("the tail starts with %q, want a whole line beyond the seeked-over prefix", lines[0])
	}
	if !strings.HasPrefix(lines[len(lines)-1], "line 0399") {
		t.Errorf("the tail ends with %q, want the newest line", lines[len(lines)-1])
	}

	if _, err := readServiceLogTail(filepath.Join(t.TempDir(), "gone.log")); !os.IsNotExist(err) {
		t.Errorf("a missing file reported %v, want the not-exist error", err)
	}
}

// TestParseServiceLogTailKeepsEveryLine proves the parse of the tail: a wire
// record keeps its fields, and any other line keeps its text verbatim under
// Raw instead of being dropped or parsed into fields it does not have. An
// empty line is a line too: it keeps its empty text and the view renders it
// as an empty row rather than dropping it.
func TestParseServiceLogTailKeepsEveryLine(t *testing.T) {
	records := parseServiceLogTail(
		`{"seq":7,"time":"2026-11-25T10:00:00Z","level":"ERROR","component":"cache","message":"unreadable","attrs":{"path":"a"}}` + "\n" +
			"not json <tag>\n" +
			`{"message":"no sequence"}` + "\n" +
			"")

	if len(records) != 4 {
		t.Fatalf("the tail parsed into %d records, want 4", len(records))
	}
	if records[0].Raw != nil {
		t.Errorf("the wire record = %+v, want no raw text", records[0])
	}
	if records[0].Seq != 7 || records[0].Message != "unreadable" || records[0].Level != "ERROR" {
		t.Errorf("the wire record = %+v, want its fields", records[0])
	}
	if records[1].Raw == nil || *records[1].Raw != "not json <tag>" {
		t.Errorf("the plain line = %+v, want its text under Raw", records[1])
	}
	if records[2].Raw == nil || *records[2].Raw != `{"message":"no sequence"}` {
		t.Errorf("the JSON without a sequence = %+v, want its text under Raw", records[2])
	}
	if records[3].Raw == nil || *records[3].Raw != "" {
		t.Errorf("the empty line = %+v, want its empty text under Raw", records[3])
	}
	if got := parseServiceLogTail(""); len(got) != 0 {
		t.Errorf("an empty tail parsed into %v, want no records", got)
	}
}

// TestLogsFallbackPageInlinesTheShippedAssets proves that the fallback
// document really carries the shipped frame and units, not references to
// them: the stylesheets and the scripts of the live page are embedded, so a
// build that loses one of them fails here instead of rendering an unstyled
// or inert document.
func TestLogsFallbackPageInlinesTheShippedAssets(t *testing.T) {
	for _, asset := range []struct {
		name  string
		proof string
	}{
		{name: "css/style.css", proof: "--color-bg:"},
		{name: "css/runtime.css", proof: ".runtime-log-panel"},
		{name: "js/logview.js", proof: "window.HFDeskLogView"},
		{name: "js/servicecontrol.js", proof: "window.HFDeskServiceControl"},
	} {
		if text := fallbackAsset(asset.name); !strings.Contains(text, asset.proof) {
			t.Errorf("the embedded asset %s does not carry %q", asset.name, asset.proof)
		}
	}

	page := logsFallbackPage(logsFallbackState{
		Service: serviceview.From("stopped", false, false),
	})
	for _, proof := range []string{
		".runtime-log-panel",          // css/runtime.css
		".runtime-shell",              // css/style.css
		"window.HFDeskLogView",        // js/logview.js
		"window.HFDeskServiceControl", // js/servicecontrol.js
	} {
		if !strings.Contains(page, proof) {
			t.Errorf("the fallback document does not inline %q", proof)
		}
	}
}

// TestLogsFallbackPageWearsTheRuntimeFrame pins the structural parity of the
// fallback and the live page: the same regions, the same classes, the same
// menu items in the same order. The fallback must not grow a second visual
// language or a second state line inside the menu.
func TestLogsFallbackPageWearsTheRuntimeFrame(t *testing.T) {
	live, err := os.ReadFile(filepath.Join("..", "assets", "static", "runtime.html"))
	if err != nil {
		t.Fatalf("read the shipped runtime.html: %v", err)
	}
	page := logsFallbackPage(logsFallbackState{
		Address: "http://127.0.0.1:18080",
		Service: serviceview.From("running", true, false),
	})

	classesIn := func(markup string) map[string]bool {
		found := map[string]bool{}
		for _, match := range regexp.MustCompile(`class="([^"]*)"`).FindAllStringSubmatch(markup, -1) {
			for _, class := range strings.Fields(match[1]) {
				found[class] = true
			}
		}
		return found
	}
	liveClasses := classesIn(string(live))
	fallbackClasses := classesIn(page)

	for _, class := range []string{
		"runtime-shell", "runtime-header", "logo", "logo-icon", "logo-text",
		"runtime-header-status", "connection-status", "status-indicator",
		"runtime-version", "runtime-service", "runtime-service-trigger",
		"runtime-service-dot", "runtime-service-label", "runtime-service-chevron",
		"runtime-service-menu", "runtime-service-item", "runtime-service-item-danger",
		"runtime-service-separator", "runtime-service-chip", "badge", "badge-secondary",
		"runtime-error", "runtime-strip", "runtime-stat", "runtime-stat-label",
		"runtime-stat-value", "runtime-url", "config-chip", "runtime-log-panel",
		"runtime-log-toolbar", "runtime-toolbar-group", "runtime-toolbar-label",
		"runtime-search", "runtime-log-meta", "runtime-log-view", "runtime-log-head",
		"runtime-log-body", "empty-state",
	} {
		if !liveClasses[class] {
			t.Errorf("runtime.html does not carry the frame class %q", class)
		}
		if !fallbackClasses[class] {
			t.Errorf("the fallback does not carry the frame class %q", class)
		}
	}

	// The menu is the same list in the same order, with the same classes.
	order := regexp.MustCompile(`id="service(Install|Start|Stop|Update|Delete)Item"`)
	keys := func(markup string) string {
		var items []string
		for _, match := range order.FindAllStringSubmatch(markup, -1) {
			items = append(items, match[1])
		}
		return strings.Join(items, ",")
	}
	if got, want := keys(page), keys(string(live)); got != want {
		t.Errorf("the fallback menu is %q, want the order of runtime.html %q", got, want)
	}

	itemClass := regexp.MustCompile(`<button class="([^"]*)" id="service(Install|Start|Stop|Update|Delete)Item"`)
	classes := func(markup string) map[string]string {
		found := map[string]string{}
		for _, match := range itemClass.FindAllStringSubmatch(markup, -1) {
			found[match[2]] = match[1]
		}
		return found
	}
	if got, want := classes(page), classes(string(live)); !reflect.DeepEqual(got, want) {
		t.Errorf("the fallback item classes = %v, want %v", got, want)
	}

	// The control keeps its menu semantics, and the state line stays in the
	// trigger: the menu carries the actions and nothing else.
	assertNames(t, page, "the fallback control", []string{
		`aria-haspopup="menu"`,
		`aria-expanded="false"`,
		`aria-controls="serviceMenu"`,
		`role="menu"`,
		`aria-labelledby="serviceTrigger"`,
		`role="menuitem"`,
		`role="separator"`,
	})
	menu := page[strings.Index(page, `id="serviceMenu"`):]
	if end := strings.Index(menu, `id="serviceChip"`); end >= 0 {
		menu = menu[:end]
	}
	if strings.Contains(menu, "serviceLabel") {
		t.Error("the fallback menu carries a second state line, want the actions only")
	}
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
				"is still running",
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

// TestRuntimePageAnswersProbesTheAddressHonestly pins the probe that
// decides between the live page and the fallback document: a runtime page
// that answers 200 counts as reachable, an error answer does not, a
// refused connection does not, and an address that stays silent past the
// bounded budget does not — the budget is the reason a stopped service
// meets the fallback instead of a window that hangs on a navigation that
// never completes.
func TestRuntimePageAnswersProbesTheAddressHonestly(t *testing.T) {
	t.Run("an answering page counts as reachable", func(t *testing.T) {
		page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusOK)
		}))
		defer page.Close()

		if !runtimePageAnswers(page.URL) {
			t.Errorf("runtimePageAnswers(%q) = false, want true: the page answered 200", page.URL)
		}
	})

	t.Run("an error answer counts as unreachable", func(t *testing.T) {
		page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.WriteHeader(http.StatusServiceUnavailable)
		}))
		defer page.Close()

		if runtimePageAnswers(page.URL) {
			t.Errorf("runtimePageAnswers(%q) = true, want false: the page answered 503", page.URL)
		}
	})

	t.Run("a refused address counts as unreachable", func(t *testing.T) {
		page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
		url := page.URL
		page.Close()

		if runtimePageAnswers(url) {
			t.Errorf("runtimePageAnswers(%q) = true, want false: nothing listens there anymore", url)
		}
	})

	t.Run("a silent address is bounded by the probe budget", func(t *testing.T) {
		previous := runtimePageProbeTimeout
		runtimePageProbeTimeout = 50 * time.Millisecond
		t.Cleanup(func() { runtimePageProbeTimeout = previous })

		page := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			time.Sleep(2 * time.Second)
		}))
		defer page.Close()

		start := time.Now()
		if runtimePageAnswers(page.URL) {
			t.Errorf("runtimePageAnswers(%q) = true, want false: the answer never arrived", page.URL)
		}
		if elapsed := time.Since(start); elapsed > time.Second {
			t.Errorf("the probe took %s, want a budget close to %s: an unbounded probe hangs the window",
				elapsed, runtimePageProbeTimeout)
		}
	})
}

// TestResolveLogsWindowPicksTheLivePageOrTheFallback pins the decision of
// one open of the companion window: an address that answers yields the
// live runtime page and no fallback document, an address that does not
// answer yields the fallback document together with the key of the read it
// was rendered from and no URL, and a registration that names no address
// yields the fallback WITHOUT probing — there is nothing to ask, and a probe
// of an empty URL would be a defect.
func TestResolveLogsWindowPicksTheLivePageOrTheFallback(t *testing.T) {
	fallback := func() (string, string) { return "stopped", "<!doctype html>fallback" }
	probeCalls := 0
	previous := runtimePageAnswers
	t.Cleanup(func() { runtimePageAnswers = previous })

	t.Run("an answering address yields the live page", func(t *testing.T) {
		probeCalls = 0
		runtimePageAnswers = func(url string) bool {
			probeCalls++
			return url == "http://127.0.0.1:18080/runtime.html"
		}
		target, key, document := resolveLogsWindow(
			func() string { return "http://127.0.0.1:18080/runtime.html" }, fallback)
		if target != "http://127.0.0.1:18080/runtime.html" || key != "" || document != "" {
			t.Errorf("resolveLogsWindow = (%q, %q, %q), want the live page and no document", target, key, document)
		}
		if probeCalls != 1 {
			t.Errorf("the probe ran %d time(s), want exactly one per open", probeCalls)
		}
	})

	t.Run("an address that does not answer yields the fallback", func(t *testing.T) {
		runtimePageAnswers = func(string) bool { return false }
		target, key, document := resolveLogsWindow(
			func() string { return "http://127.0.0.1:18080/runtime.html" }, fallback)
		if target != "" || key != "stopped" || document != "<!doctype html>fallback" {
			t.Errorf("resolveLogsWindow = (%q, %q, %q), want no URL and the fallback document of the read",
				target, key, document)
		}
	})

	t.Run("a registration without an address yields the fallback without a probe", func(t *testing.T) {
		probeCalls = 0
		runtimePageAnswers = func(string) bool {
			probeCalls++
			return true
		}
		target, key, document := resolveLogsWindow(func() string { return "" }, fallback)
		if target != "" || key != "stopped" || document != "<!doctype html>fallback" {
			t.Errorf("resolveLogsWindow = (%q, %q, %q), want no URL and the fallback document of the read",
				target, key, document)
		}
		if probeCalls != 0 {
			t.Errorf("the probe ran %d time(s) beside an empty address, want no probe at all", probeCalls)
		}
	})
}

// TestTheOpenReportsOfTheWindowFollowTheWindowRule pins the new reports of
// the host-owned open actions under the established window rule — the
// state plus AT MOST one action, with the standard log-path line of this
// launch named exactly once — as one map, the way the sibling reports are
// pinned: a report that joins the family without the rule, or one that
// names the log twice beside a path it also states itself, fails here.
func TestTheOpenReportsOfTheWindowFollowTheWindowRule(t *testing.T) {
	const launchLogPath = `C:\Users\First Last\hfdesk.log`
	previous := runLogPath
	runLogPath = launchLogPath
	t.Cleanup(func() { runLogPath = previous })

	cause := errors.New("the default handler refused the file")
	logLine := "The log file is at " + launchLogPath + "."
	folder := `C:\Users\First Last\logs`

	for name, tc := range map[string]struct {
		text string
		// want is the state, the cause and the path facts the report
		// must carry.
		want []string
		// forbidden is every action a state-only report must not
		// offer and the facts its state contradicts.
		forbidden []string
	}{
		"the no-log report": {
			text: noLogFileText(),
			want: []string{
				"There is no log file to open.",
				logLine,
			},
			forbidden: []string{
				"Do you want to", "could not be opened", "has not been created yet",
			},
		},
		"the missing-log report": {
			text: logFileMissingText(),
			want: []string{
				"The log file has not been created yet, so there is nothing to open.",
				logLine,
			},
			forbidden: []string{
				"Do you want to", "could not be opened",
			},
		},
		"the log open failure report": {
			text: logFileOpenFailedText(cause),
			want: []string{
				"The log file could not be opened in the default program.",
				cause.Error(),
				logLine,
			},
			forbidden: []string{
				"Do you want to", "has not been created yet",
			},
		},
		"the missing-folder report": {
			text: logFolderMissingText(folder),
			want: []string{
				"The log folder has not been created yet, so there is nothing to open.",
				"The log folder is at " + folder + ".",
				logLine,
			},
			forbidden: []string{
				"Do you want to", "could not be opened",
			},
		},
		"the folder open failure report": {
			text: logFolderOpenFailedText(folder, cause),
			want: []string{
				"The log folder could not be opened.",
				cause.Error(),
				"The log folder is at " + folder + ".",
				logLine,
			},
			forbidden: []string{
				"Do you want to", "has not been created yet",
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			assertNames(t, tc.text, name, tc.want)
			for _, unwanted := range tc.forbidden {
				if strings.Contains(tc.text, unwanted) {
					t.Errorf("the %s carries %q beside its state and its cause:\n%s", name, unwanted, tc.text)
				}
			}
			if got := strings.Count(tc.text, "The log file is at"); got != 1 {
				t.Errorf("the %s names the log file %d times, want exactly once:\n%s", name, got, tc.text)
			}
		})
	}
}

// TestRuntimeAssetsNameTheBoundWindowActions pins the JavaScript side of
// the bridge against the Go constants: the shipped runtime.js may only
// reference the bound action names (plus the plain product prefix of its
// storage key), and it must reference the open actions and the five service
// actions — a rename on either side that leaves the other behind is a dead
// button or a dead menu item in the live window, and this test fails
// outright. The shipped runtime.html must carry both buttons the view drives,
// under the ids the view looks up, and the service control with its menu.
func TestRuntimeAssetsNameTheBoundWindowActions(t *testing.T) {
	view, err := os.ReadFile(filepath.Join("..", "assets", "static", "js", "runtime.js"))
	if err != nil {
		t.Fatalf("read the shipped runtime.js: %v", err)
	}
	allowed := map[string]bool{
		openLogFileAction:        true,
		openLogFolderAction:      true,
		openUiAction:             true,
		logsStartServiceAction:   true,
		logsStopServiceAction:    true,
		logsUpdateServiceAction:  true,
		logsDeleteServiceAction:  true,
		logsInstallServiceAction: true,
		"hfdesk":                 true, // the plain prefix of the storage key
	}
	for _, token := range regexp.MustCompile(`hfdesk[A-Za-z0-9_$]*`).FindAllString(string(view), -1) {
		if !allowed[token] {
			t.Errorf("runtime.js carries the token %q, want only the bound action names: a foreign name is a dead button",
				token)
		}
	}
	for _, name := range []string{
		openLogFolderAction, openLogFileAction, openUiAction,
		logsStartServiceAction, logsStopServiceAction, logsUpdateServiceAction,
		logsDeleteServiceAction, logsInstallServiceAction,
	} {
		if !strings.Contains(string(view), name) {
			t.Errorf("runtime.js never references %q: the window host binds it, so the view must call it", name)
		}
	}

	markup, err := os.ReadFile(filepath.Join("..", "assets", "static", "runtime.html"))
	if err != nil {
		t.Fatalf("read the shipped runtime.html: %v", err)
	}
	assertNames(t, string(markup), "the shipped runtime.html", []string{
		`id="openLogsButton"`,
		"Open log folder",
		`id="openLogFileButton"`,
		"Open the log file",
		`id="serviceTrigger"`,
		`id="serviceMenu"`,
		"Start service",
		"Stop service",
		"Update service",
		"Delete service",
	})
}

// TestDesktopRelaunchArgsReturnToTheOrdinaryLaunch pins the other end state
// of a handover at the command line that produces it: the removal's relaunch
// child carries the bounded lock wait and NO companion flag, so it parses
// back as an ordinary launch — the one whose own service gate finds the
// machine without a registration and takes the desktop mode again — while the
// companion relaunch keeps asking for its mode explicitly.
func TestDesktopRelaunchArgsReturnToTheOrdinaryLaunch(t *testing.T) {
	argv := []string{"--log-file", `C:\Users\First Last\hfdesk.log`, "--no-open"}

	removal := desktopRelaunchArgs(argv, 150*time.Second)
	launch, rest, err := SplitCompanionArgs(removal)
	if err != nil {
		t.Fatalf("the removal relaunch command line %q does not parse: %v", removal, err)
	}
	if launch.Enabled {
		t.Error("the removal relaunch starts a tray companion, want the ordinary launch the gate decides for")
	}
	if launch.Wait != 150*time.Second {
		t.Errorf("the removal relaunch wait = %s, want 2m30s", launch.Wait)
	}
	if !reflect.DeepEqual(rest, argv) {
		t.Errorf("the host arguments = %q, want them unchanged %q", rest, argv)
	}

	companion := companionRelaunchArgs(argv, 150*time.Second)
	companionLaunch, _, err := SplitCompanionArgs(companion)
	if err != nil {
		t.Fatalf("the companion relaunch command line %q does not parse: %v", companion, err)
	}
	if !companionLaunch.Enabled {
		t.Error("the companion relaunch does not start a tray companion")
	}
}
