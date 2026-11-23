// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package desktop

import (
	"errors"
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

// TestLogsFallbackPageRendersStateAddressLogAndActions pins the fallback
// document of the Logs window: the honest state as its heading, the
// honest line that the registered address does not answer (stated as a
// fact in a code chip, never linked — the address just proved it does not
// answer), the path of the service log with the one action that opens
// that very file, and the retry action. The window title is the short
// one: "Logs", without internal names.
//
// The document fetches nothing: its one script carries no channel of any
// kind, so the fallback beside a dead address reaches out of the document
// just as little as the companion listened to nothing before — a model
// without a log path still carries the retry and no open button (there is
// nothing to open), pinned below.
func TestLogsFallbackPageRendersStateAddressLogAndActions(t *testing.T) {
	page := logsFallbackPage(logsFallbackState{
		State:   "The HFDesk service is stopped.",
		Address: "http://127.0.0.1:18080",
		LogPath: `C:\ProgramData\HFDesk\logs\hfdesk.log`,
	})

	assertNames(t, page, "the fallback document", []string{
		"<title>HFDesk - Logs</title>",
		"<h1>The HFDesk service is stopped.</h1>",
		"The web UI at <code>http://127.0.0.1:18080</code> does not answer.",
		`<div class="stat-label">Log file</div>`,
		`<code>C:\ProgramData\HFDesk\logs\hfdesk.log</code>`,
		`id="open-log-file"`,
		"Open the log file",
		`id="try-again"`,
		">Try again<",
	})
	// The address is stated, never linked: a link to the address that
	// just failed to answer would be a dead path in the window.
	assertNamesWithout(t, page, "the address line", nil, []string{"<a ", "href="})

	// The section order: the state leads, the honest line follows, the
	// log fact and its action come last — the document reads top down
	// from what is true to what can be done.
	state := strings.Index(page, "<h1>")
	address := strings.Index(page, "does not answer.")
	logFact := strings.Index(page, `<code>C:\ProgramData\HFDesk\logs\hfdesk.log</code>`)
	if !(state >= 0 && state < address && address < logFact) {
		t.Errorf("the document order is state %d, address %d, log %d, want state < address < log:\n%s",
			state, address, logFact, page)
	}

	// The only script of the document performs the two local operations
	// and nothing else: no fetch, no socket, no remote resource, no data
	// leaves the document — the fallback reaches nowhere.
	if count := strings.Count(page, "<script"); count != 1 {
		t.Errorf("the document carries %d script elements, want exactly the one local script", count)
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
// passing as "just one more attribute". Both shapes of the document (with
// and without a log path) are pinned against it. (The LIVE page of the
// window is a different mechanism: the window host navigates the WebView2
// to the address of the service — an outgoing page load the host owns,
// not a channel of any document.)
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

// TestLogsFallbackPageScriptCallsOnlyBoundActionNames pins the source of
// truth of the bridge names: the document's script may reference ONLY the
// names substituted from openLogFileAction and logsTryAgainAction — the
// very constants companionWindowActions binds as its map keys — so
// renaming a constant without the script (or the script without the
// constant) cannot pass: the template derives the names at generation
// time, and this test fails outright if any other literal name reaches
// the script, if a substitution placeholder leaks into the document, or
// if a constant stops being a usable JavaScript identifier.
//
// It also pins the element ids the script looks up by: an id renamed in
// one of its two places is a dead button, and the click would silently do
// nothing.
func TestLogsFallbackPageScriptCallsOnlyBoundActionNames(t *testing.T) {
	identifier := regexp.MustCompile(`^[A-Za-z_$][A-Za-z0-9_$]*$`)
	for _, name := range []string{openLogFileAction, logsTryAgainAction} {
		if !identifier.MatchString(name) {
			t.Fatalf("%q is not a JavaScript identifier: "+
				"substituting it would produce a script that cannot parse", name)
		}
	}

	page := logsFallbackPage(logsFallbackState{
		State:   "The HFDesk service is stopped.",
		Address: "http://127.0.0.1:18080",
		LogPath: `C:\ProgramData\HFDesk\logs\hfdesk.log`,
	})

	// Every window.<name> reference of the document is one of THE bound
	// actions: no foreign name may sit beside them, and each one carries
	// its guard and its call (twice).
	bound := map[string]bool{openLogFileAction: true, logsTryAgainAction: true}
	counts := map[string]int{}
	refs := regexp.MustCompile(`window\.[A-Za-z_$][A-Za-z0-9_$]*`).FindAllString(page, -1)
	if len(refs) < 4 {
		t.Errorf("the script references bound actions %d time(s), want the guard and the call of each: %v",
			len(refs), refs)
	}
	for _, ref := range refs {
		name := strings.TrimPrefix(ref, "window.")
		if !bound[name] {
			t.Errorf("the document calls %q, want only %q and %q: a foreign name is a dead button",
				ref, openLogFileAction, logsTryAgainAction)
		}
		counts[name]++
	}
	for _, name := range []string{openLogFileAction, logsTryAgainAction} {
		if counts[name] < 2 {
			t.Errorf("the script references %q %d time(s), want its guard and its call", name, counts[name])
		}
	}
	for _, placeholder := range []string{logsFallbackOpenPlaceholder, logsFallbackTryPlaceholder} {
		if strings.Contains(page, placeholder) {
			t.Errorf("the document carries the unsubstituted placeholder %q: the generation step is broken:\n%s",
				placeholder, page)
		}
	}

	// The bare names must not appear either: a call without the window.
	// prefix is just as dead when a constant is renamed. The scan runs
	// inside the script element only — the document legitimately prints
	// the log file name (hfdesk.log) as text.
	script := regexp.MustCompile(`(?s)<script>.*?</script>`).FindString(page)
	if script == "" {
		t.Fatal("the document carries no script: the actions have no forwarder")
	}
	for _, name := range regexp.MustCompile(`hfdesk[A-Za-z0-9_$]*`).FindAllString(script, -1) {
		if !bound[name] {
			t.Errorf("the script carries the literal %q, want only openLogFileAction %q and logsTryAgainAction %q: "+
				"a hardcoded name is a dead button", name, openLogFileAction, logsTryAgainAction)
		}
	}

	// The buttons are looked up by the very ids the markup carries.
	assertNames(t, page, "the bridge elements", []string{
		`id="open-log-file"`, `getElementById("open-log-file")`,
		`id="try-again"`, `getElementById("try-again")`,
	})
}

// TestLogsFallbackPageHonestShapesAndEscapesEverything proves the honest
// shapes of the fallback document — an address that did not answer is
// stated as a fact and never linked, a machine without an address states
// the honest reason instead, and a model without a log path states that
// fact and offers no open button beside the retry (a button with nothing
// to open would be a dead action) — and that every value from the machine
// is escaped, so none of it can become markup.
func TestLogsFallbackPageHonestShapesAndEscapesEverything(t *testing.T) {
	t.Run("an address that did not answer is stated, never linked", func(t *testing.T) {
		page := logsFallbackPage(logsFallbackState{
			State:   "The HFDesk service is stopped.",
			Address: "http://127.0.0.1:18080",
		})
		assertNamesWithout(t, page, "the address line", []string{
			"The web UI at <code>http://127.0.0.1:18080</code> does not answer.",
		}, []string{"<a ", "href="})
	})

	t.Run("a machine without an address states the honest reason", func(t *testing.T) {
		page := logsFallbackPage(logsFallbackState{
			State: "HFDesk is not registered as a Windows service.",
			Note:  "The registration names no address, so there is nothing to open.",
		})
		assertNames(t, page, "the note line", []string{
			"The registration names no address, so there is nothing to open.",
		})
		if strings.Contains(page, "does not answer") {
			t.Errorf("the document claims an address did not answer although it names none: %q", page)
		}
	})

	t.Run("no log path states the fact and offers no open button", func(t *testing.T) {
		page := logsFallbackPage(logsFallbackState{
			State: "The HFDesk service is stopped.",
			Note:  "The address of the web UI is not known.",
		})
		assertNamesWithout(t, page, "the pathless document", []string{
			"The log file is not known.",
			`id="try-again"`,
		}, []string{`id="open-log-file"`, "Open the log file"})
		// The pathless shape reaches out of the document just as
		// little as the full one.
		assertPageReachesOutNowhere(t, page, "the pathless document")
	})

	t.Run("every value is escaped", func(t *testing.T) {
		page := logsFallbackPage(logsFallbackState{
			State:   `<script>alert("state")</script>`,
			Address: `127.0.0.1:8080&<i>`,
			LogPath: `<b>log</b>`,
		})
		for _, raw := range []string{
			`<script>alert("state")</script>`, `&<i>`, `<b>log</b>`,
		} {
			if strings.Contains(page, raw) {
				t.Errorf("the document carries the raw value %q: nothing from the machine may become markup", raw)
			}
		}
		assertNames(t, page, "the escaped document", []string{
			"&lt;script&gt;",
			"&amp;&lt;i&gt;",
			"&lt;b&gt;log&lt;/b&gt;",
		})
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
// answer yields the fallback document and no URL, and a registration that
// names no address yields the fallback WITHOUT probing — there is nothing
// to ask, and a probe of an empty URL would be a defect.
func TestResolveLogsWindowPicksTheLivePageOrTheFallback(t *testing.T) {
	fallback := func() string { return "<!doctype html>fallback" }
	probeCalls := 0
	previous := runtimePageAnswers
	t.Cleanup(func() { runtimePageAnswers = previous })

	t.Run("an answering address yields the live page", func(t *testing.T) {
		probeCalls = 0
		runtimePageAnswers = func(url string) bool {
			probeCalls++
			return url == "http://127.0.0.1:18080/runtime.html"
		}
		target, document := resolveLogsWindow(
			func() string { return "http://127.0.0.1:18080/runtime.html" }, fallback)
		if target != "http://127.0.0.1:18080/runtime.html" || document != "" {
			t.Errorf("resolveLogsWindow = (%q, %q), want the live page and no document", target, document)
		}
		if probeCalls != 1 {
			t.Errorf("the probe ran %d time(s), want exactly one per open", probeCalls)
		}
	})

	t.Run("an address that does not answer yields the fallback", func(t *testing.T) {
		runtimePageAnswers = func(string) bool { return false }
		target, document := resolveLogsWindow(
			func() string { return "http://127.0.0.1:18080/runtime.html" }, fallback)
		if target != "" || document != "<!doctype html>fallback" {
			t.Errorf("resolveLogsWindow = (%q, %q), want no URL and the fallback document", target, document)
		}
	})

	t.Run("a registration without an address yields the fallback without a probe", func(t *testing.T) {
		probeCalls = 0
		runtimePageAnswers = func(string) bool {
			probeCalls++
			return true
		}
		target, document := resolveLogsWindow(func() string { return "" }, fallback)
		if target != "" || document != "<!doctype html>fallback" {
			t.Errorf("resolveLogsWindow = (%q, %q), want no URL and the fallback document", target, document)
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
// reference the two bound action names (plus the plain product prefix of
// its storage key), and it must reference both of them — a rename on
// either side that leaves the other behind is a dead button in the live
// window, and this test fails outright. The shipped runtime.html must
// carry both buttons the view drives, under the ids the view looks up.
func TestRuntimeAssetsNameTheBoundWindowActions(t *testing.T) {
	view, err := os.ReadFile(filepath.Join("..", "assets", "static", "js", "runtime.js"))
	if err != nil {
		t.Fatalf("read the shipped runtime.js: %v", err)
	}
	allowed := map[string]bool{
		openLogFileAction:   true,
		openLogFolderAction: true,
		openUiAction:        true,
		"hfdesk":            true, // the plain prefix of the storage key
	}
	for _, token := range regexp.MustCompile(`hfdesk[A-Za-z0-9_$]*`).FindAllString(string(view), -1) {
		if !allowed[token] {
			t.Errorf("runtime.js carries the token %q, want only the bound action names: a foreign name is a dead button",
				token)
		}
	}
	for _, name := range []string{openLogFolderAction, openLogFileAction, openUiAction} {
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
