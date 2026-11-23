// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"errors"
	"path/filepath"
	"strings"
	"testing"
)

// TestOfferMissingConsoleManualRouteOpensTheReleasePage pins the manual
// route of the missing-console notice end to end (decision of 2026-11-24):
// the press of the manual link opens the release page of this build — the
// very URL the manual instruction text carries — through the one browser
// opener every browser call of this host uses, a successful open reports
// nothing, and the press still ends with the program missing: the action
// changes nothing. «Not now», Esc and the close button answer dismiss, and
// that answer never reaches the browser.
func TestOfferMissingConsoleManualRouteOpensTheReleasePage(t *testing.T) {
	const folder = `C:\portable`
	executable := filepath.Join(folder, testReleaseDesktop)
	wantURL := releaseTagPagePrefix + testReleaseTag

	// The lookup fails, so the notice is shown; the offer resolves, so
	// the release of this build's own name names the page.
	replaceSeam(t, &findSiblingExecutable, func(string, string) (string, error) {
		return "", errors.New("there is no console program in " + folder)
	})
	replaceSeam(t, &consoleDownloadOfferFor, func(string) (*consoleDownloadOffer, error) {
		return &consoleDownloadOffer{
			Release: consoleRelease{Tag: testReleaseTag, Asset: testReleaseAsset},
			Size:    1024,
		}, nil
	})
	var opened []string
	replaceSeam(t, &openBrowser, func(url string) error {
		opened = append(opened, url)
		return nil
	})
	scripted := siblingMissingManual
	var shown []siblingMissingNotice
	replaceSeam(t, &showSiblingMissingNotice, func(notice siblingMissingNotice) siblingMissingAnswer {
		shown = append(shown, notice)
		return scripted
	})
	messages := recordMessages(t)

	console, ok := offerMissingConsoleProgram(serviceActionInstall, executable)
	if ok || console != "" {
		t.Errorf("the manual route continued the action with (%q, %t), want no program and no continuation",
			console, ok)
	}
	if len(shown) != 1 {
		t.Fatalf("the notice was shown %d time(s), want exactly one", len(shown))
	}
	if shown[0].ReleaseURL != wantURL {
		t.Errorf("the notice carries the release page %q, want %q", shown[0].ReleaseURL, wantURL)
	}
	if !strings.Contains(shown[0].Manual, wantURL) {
		t.Errorf("the manual instruction %q does not carry the URL the route opens %q",
			shown[0].Manual, wantURL)
	}
	if len(opened) != 1 || opened[0] != wantURL {
		t.Errorf("the browser opened %v, want exactly [%s]", opened, wantURL)
	}
	if len(*messages) != 0 {
		t.Errorf("a successful open showed %v, want no report", *messages)
	}

	// «Not now», Esc and the close button answer dismiss: the browser
	// is never reached, and a decline stays quiet.
	scripted = siblingMissingDismiss
	console, ok = offerMissingConsoleProgram(serviceActionInstall, executable)
	if ok || console != "" {
		t.Errorf("a dismissed notice continued the action with (%q, %t), want neither", console, ok)
	}
	if len(opened) != 1 {
		t.Errorf("the dismiss answer opened %v, want the browser untouched since the manual press", opened)
	}
	if len(*messages) != 0 {
		t.Errorf("a dismissed notice showed %v, want no report: a decline is quiet", *messages)
	}
}
