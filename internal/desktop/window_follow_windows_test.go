// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/bashrusakh/hfdesk/internal/service"
)

// fakeWindowView is the webView seam of the Logs window: it records the
// navigations the window receives IN ORDER, so a test can prove WHICH content
// a show and a follow load without a WebView2 runtime on the machine it runs
// on. It is the window, not the host: nothing of it reaches the thread of a
// real window.
type fakeWindowView struct {
	mu        sync.Mutex
	urls      []string
	documents []string
	shows     int
	closes    int
}

// The fake stands in for the WebView2 driver the window is built on.
var _ webView = (*fakeWindowView)(nil)

func (v *fakeWindowView) Show() error {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.shows++
	return nil
}

func (v *fakeWindowView) Hide() {}

func (v *fakeWindowView) Navigate(url string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.urls = append(v.urls, url)
}

func (v *fakeWindowView) NavigateDocument(html string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.documents = append(v.documents, html)
}

func (v *fakeWindowView) BindAction(string, func()) {}

func (v *fakeWindowView) Close() {
	v.mu.Lock()
	defer v.mu.Unlock()
	v.closes++
}

// navigations returns what the window loaded, in order and copied, so a test
// reads a stable record of the passes it drove.
func (v *fakeWindowView) navigations() (urls, documents []string) {
	v.mu.Lock()
	defer v.mu.Unlock()
	return append([]string(nil), v.urls...), append([]string(nil), v.documents...)
}

// openTestWindow wires a companion window around the fake view and publishes an
// open instance for it, the way a show does — without a message loop, so the
// follow of the tick can be driven directly.
func openTestWindow(view *fakeWindowView, live func() string, content func() string) *runtimeWindow {
	window := newCompanionWindow(live, content, nil, nil)
	window.instance = &windowInstance{view: view, done: make(chan struct{})}
	return window
}

// answerSwitch is the probe seam of one test: it answers the address per the
// value the test sets and counts the probes, so a test proves both the decision
// and its cost — one probe per pass, and none beside a closed window.
type answerSwitch struct {
	mu      sync.Mutex
	answers bool
	probes  int
}

func (s *answerSwitch) install(t *testing.T) {
	t.Helper()
	replaceSeam(t, &runtimePageAnswers, func(string) bool {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.probes++
		return s.answers
	})
}

func (s *answerSwitch) set(answers bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.answers = answers
}

func (s *answerSwitch) probeCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.probes
}

// TestWindowFollowSwitchesToTheFallbackWhenTheAddressStopsAnswering proves the
// defect this follow exists for: a window open on the live page of a service
// that stops must reach the fallback document — the state and its actions —
// with exactly one navigation of the CURRENT state, and the passes after it
// must leave the document alone instead of reloading it under the user.
func TestWindowFollowSwitchesToTheFallbackWhenTheAddressStopsAnswering(t *testing.T) {
	const (
		page     = "http://127.0.0.1:18080/runtime.html"
		document = "<!doctype html>fallback: the service is stopped"
	)
	answers := &answerSwitch{answers: true}
	answers.install(t)

	renders := 0
	view := &fakeWindowView{}
	window := openTestWindow(view, func() string { return page }, func() string {
		renders++
		return document
	})

	// The window was opened beside a service that answered: the live page.
	if err := window.Show(); err != nil {
		t.Fatalf("Show() = %v, want the live page", err)
	}
	if urls, documents := view.navigations(); len(urls) != 1 || urls[0] != page || len(documents) != 0 {
		t.Fatalf("Show() loaded (%q, %q), want the live page and no document", urls, documents)
	}

	// The service stops under the open window: the next pass of the tick
	// switches the KIND of the content to the fallback document.
	answers.set(false)
	window.follow(page)
	urls, documents := view.navigations()
	if len(urls) != 1 {
		t.Errorf("the window loaded %d pages, want the one page of the show and no navigation of the follow", len(urls))
	}
	if len(documents) != 1 || documents[0] != document {
		t.Fatalf("the follow loaded (%q), want exactly the fallback document of the current state", documents)
	}
	if renders != 1 {
		t.Errorf("the document was rendered %d time(s), want exactly one per switch", renders)
	}

	// The following passes see the same kind: nothing is reloaded, so the
	// tail of the log the user scrolled keeps its position.
	window.follow(page)
	window.follow(page)
	if _, documents := view.navigations(); len(documents) != 1 {
		t.Errorf("the window loaded %d documents, want the one of the switch and no reload of an unchanged state", len(documents))
	}
	if renders != 1 {
		t.Errorf("the document was rendered %d time(s) beside an unchanged state, want no render at all", renders)
	}
	if probes := answers.probeCount(); probes != 4 {
		t.Errorf("the passes probed %d time(s), want exactly one per pass (the show and three follows)", probes)
	}
}

// TestWindowFollowLeavesTheLivePageAloneWhileItAnswers proves the other half of
// the switch rule: a live page that answers at the address it was loaded from
// is not reloaded — not once, not on several passes — so the scroll position of
// the open log survives every tick. The probe still runs once per pass, which is
// the whole cost of the follow.
func TestWindowFollowLeavesTheLivePageAloneWhileItAnswers(t *testing.T) {
	const page = "http://127.0.0.1:18080/runtime.html"
	answers := &answerSwitch{answers: true}
	answers.install(t)

	view := &fakeWindowView{}
	window := openTestWindow(view, func() string { return page }, func() string {
		t.Error("the fallback document was rendered beside an answering address")
		return "<!doctype html>fallback"
	})
	if err := window.Show(); err != nil {
		t.Fatalf("Show() = %v, want the live page", err)
	}

	for pass := 0; pass < 3; pass++ {
		window.follow(page)
	}
	urls, documents := view.navigations()
	if len(urls) != 1 || urls[0] != page {
		t.Errorf("the window loaded (%q), want the one page of the show and no reload of an unchanged live page", urls)
	}
	if len(documents) != 0 {
		t.Errorf("the window loaded %d document(s), want none beside an answering address", len(documents))
	}
	if probes := answers.probeCount(); probes != 4 {
		t.Errorf("the passes probed %d time(s), want exactly one per pass (the show and three follows)", probes)
	}
}

// TestWindowFollowSwitchesBackToTheLivePageWhenTheServiceReturns pins the
// direction the fallback document cannot recover from on its own: a service
// that comes back under an open window is shown as its live page again, once,
// and the passes after it leave that page alone.
func TestWindowFollowSwitchesBackToTheLivePageWhenTheServiceReturns(t *testing.T) {
	const page = "http://127.0.0.1:18080/runtime.html"
	answers := &answerSwitch{answers: false}
	answers.install(t)

	view := &fakeWindowView{}
	window := openTestWindow(view, func() string { return page }, func() string {
		return "<!doctype html>fallback: the service is stopped"
	})

	// The window was opened beside a service that did not answer.
	if err := window.Show(); err != nil {
		t.Fatalf("Show() = %v, want the fallback document", err)
	}
	if urls, documents := view.navigations(); len(urls) != 0 || len(documents) != 1 {
		t.Fatalf("Show() loaded (%q, %q), want the fallback document and no page", urls, documents)
	}

	// A pass beside the still silent address changes nothing.
	window.follow(page)
	if _, documents := view.navigations(); len(documents) != 1 {
		t.Fatalf("the follow reloaded the fallback document beside an unchanged state, want no navigation")
	}

	// The service comes back: the live page is loaded once.
	answers.set(true)
	window.follow(page)
	urls, documents := view.navigations()
	if len(urls) != 1 || urls[0] != page {
		t.Fatalf("the follow loaded (%q), want exactly the live page of the returned service", urls)
	}
	if len(documents) != 1 {
		t.Errorf("the follow loaded %d document(s), want no second document", len(documents))
	}

	// The page answers from here on: the following passes reload nothing.
	window.follow(page)
	window.follow(page)
	if urls, _ := view.navigations(); len(urls) != 1 {
		t.Errorf("the window loaded %d page(s), want the one of the switch and no reload of an unchanged live page", len(urls))
	}
}

// TestWindowFollowDoesNothingWhileTheWindowIsClosed pins the cost rule of a
// closed window: nothing to follow means no probe and no navigation, so a tick
// beside a window the user closed costs the same it costs without a window at
// all.
func TestWindowFollowDoesNothingWhileTheWindowIsClosed(t *testing.T) {
	const page = "http://127.0.0.1:18080/runtime.html"
	answers := &answerSwitch{answers: true}
	answers.install(t)

	t.Run("no window was ever opened", func(t *testing.T) {
		view := &fakeWindowView{}
		window := newCompanionWindow(func() string { return page }, func() string {
			t.Error("the fallback document was rendered beside a closed window")
			return "<!doctype html>fallback"
		}, nil, nil)

		window.follow(page)
		if probes := answers.probeCount(); probes != 0 {
			t.Errorf("the pass probed %d time(s) beside a window that was never opened, want none", probes)
		}
		if urls, documents := view.navigations(); len(urls) != 0 || len(documents) != 0 {
			t.Errorf("the pass loaded (%q, %q) beside a window that was never opened, want nothing", urls, documents)
		}
	})

	t.Run("the window was closed", func(t *testing.T) {
		view := &fakeWindowView{}
		window := openTestWindow(view, func() string { return page }, func() string {
			t.Error("the fallback document was rendered beside a closed window")
			return "<!doctype html>fallback"
		})
		done := make(chan struct{})
		close(done)
		window.instance = &windowInstance{view: view, done: done}

		window.follow(page)
		if probes := answers.probeCount(); probes != 0 {
			t.Errorf("the pass probed %d time(s) beside a closed window, want none", probes)
		}
		if urls, documents := view.navigations(); len(urls) != 0 || len(documents) != 0 {
			t.Errorf("the pass loaded (%q, %q) beside a closed window, want nothing", urls, documents)
		}
	})
}

// TestWindowFollowLeavesUrlModeAlone pins the desktop rule: a window of the
// desktop host shows the page of its own server and has no other kind of
// content, so a pass must not probe, reload or otherwise touch it — the
// follow belongs to the tray companion alone.
func TestWindowFollowLeavesUrlModeAlone(t *testing.T) {
	const (
		ownPage   = "http://127.0.0.1:8090/runtime.html"
		otherPage = "http://127.0.0.1:18080/runtime.html"
	)
	answers := &answerSwitch{answers: true}
	answers.install(t)

	view := &fakeWindowView{}
	window := newRuntimeWindow(ownPage, nil, nil)
	window.instance = &windowInstance{view: view, done: make(chan struct{})}
	if err := window.Show(); err != nil {
		t.Fatalf("Show() = %v, want the page of the own server", err)
	}

	window.follow(otherPage)
	urls, documents := view.navigations()
	if len(urls) != 1 || urls[0] != ownPage {
		t.Errorf("the window loaded (%q), want only the page of the own server", urls)
	}
	if len(documents) != 0 {
		t.Errorf("the window loaded %d document(s), want none in URL mode", len(documents))
	}
	if probes := answers.probeCount(); probes != 0 {
		t.Errorf("the pass probed %d time(s) in URL mode, want none: that window follows no service", probes)
	}
}

// TestWindowFollowLoadsTheNewAddressOfAMovedRegistration pins the address half
// of the change rule: a registration that names another address is a change the
// open window follows, even though the kind of its content stays the live page —
// the old address is not the page the state now has, so the window loads the new
// one.
func TestWindowFollowLoadsTheNewAddressOfAMovedRegistration(t *testing.T) {
	const (
		oldPage = "http://127.0.0.1:18080/runtime.html"
		newPage = "http://127.0.0.1:18081/runtime.html"
	)
	answers := &answerSwitch{answers: true}
	answers.install(t)

	view := &fakeWindowView{}
	window := openTestWindow(view, func() string { return oldPage }, func() string {
		return "<!doctype html>fallback"
	})
	if err := window.Show(); err != nil {
		t.Fatalf("Show() = %v, want the live page", err)
	}

	window.follow(newPage)
	urls, _ := view.navigations()
	if len(urls) != 2 || urls[0] != oldPage || urls[1] != newPage {
		t.Fatalf("the window loaded (%q), want the old page of the show and then the new address", urls)
	}

	// From here on the new address is the one that answers: no more loads.
	window.follow(newPage)
	if urls, _ := view.navigations(); len(urls) != 2 {
		t.Errorf("the window loaded %d page(s), want no reload of the unchanged new address", len(urls))
	}
}

// TestServiceMenuTickCarriesItsReadToTheLogsWindow proves the wiring the follow
// runs on: the established tick of the tray menu — the refresh the host posts
// and the 30 second cadence it already keeps — carries ONE read of the
// registration to both surfaces, the status line it applies and the open Logs
// window it follows. No timer of the window's own exists.
func TestServiceMenuTickCarriesItsReadToTheLogsWindow(t *testing.T) {
	var mu sync.Mutex
	current := service.Status{State: service.StateRunning, Running: true}
	replaceSeam(t, &serviceStatus, func(context.Context) (service.Status, error) {
		mu.Lock()
		defer mu.Unlock()
		return current, nil
	})

	line := newFakeTrayItem()
	install := newFakeTrayItem()
	start := newFakeTrayItem()
	menu := &trayServiceMenu{
		line: line, install: install,
		start: start, stop: newFakeTrayItem(),
		update: newFakeTrayItem(), delete: newFakeTrayItem(),
	}

	refresh := make(chan struct{}, 1)
	followed := make(chan service.Status, 4)
	view := trayServiceView{
		read:    serviceState.readForDisplay,
		refresh: refresh,
		follow: func(status service.Status, readErr error) {
			if readErr != nil {
				t.Errorf("the tick carried the read error %v to the window, want the read of the registration", readErr)
			}
			followed <- status
		},
	}
	stopped := make(chan struct{})
	go menu.keepFresh(view, stopped)
	t.Cleanup(func() { close(stopped) })

	// The request of a window read is the very signal the host posts.
	refresh <- struct{}{}
	select {
	case status := <-followed:
		if status.State != service.StateRunning || !status.Running {
			t.Errorf("the window followed the state %+v, want the running service the menu applied", status)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("the tick did not carry its read to the Logs window")
	}
	waitForServiceMenuItem(t, "the running service", line, install, start,
		"Service: running", false, true)
}
