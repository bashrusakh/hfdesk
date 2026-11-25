// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

package server

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// runtimeAssetText reads one shipped asset of the Runtime view.
//
// The name is slash separated and relative to internal/assets/static, so the
// test reads exactly the files the page serves.
func runtimeAssetText(t *testing.T, name string) string {
	t.Helper()

	path := filepath.Join("..", "assets", "static", filepath.FromSlash(name))
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read the shipped %s: %v", name, err)
	}
	return string(data)
}

// TestRuntimeUIRuntimeAssetsShareTheCard pins the split of the Runtime view:
// the layout lives in css/runtime.css, which the page loads after the design
// system, the renderer of one record lives in js/logview.js, and the
// interaction of the service menu lives in js/servicecontrol.js. The renderer
// and the menu control load before the view that drives them.
func TestRuntimeUIRuntimeAssetsShareTheCard(t *testing.T) {
	markup := runtimeAssetText(t, "runtime.html")

	cssLink := strings.Index(markup, `href="css/style.css"`)
	runtimeLink := strings.Index(markup, `href="css/runtime.css"`)
	if cssLink < 0 || runtimeLink < 0 {
		t.Fatal("runtime.html does not load both stylesheets")
	}
	if runtimeLink < cssLink {
		t.Error("css/runtime.css must load after css/style.css: the design system comes first")
	}
	if strings.Contains(markup, "<style") {
		t.Error("runtime.html carries an inline style block, want the layout in css/runtime.css")
	}

	logviewScript := strings.Index(markup, `src="js/logview.js"`)
	controlScript := strings.Index(markup, `src="js/servicecontrol.js"`)
	viewScript := strings.Index(markup, `src="js/runtime.js"`)
	if logviewScript < 0 || controlScript < 0 || viewScript < 0 {
		t.Fatal("runtime.html does not load the renderer, the service menu and the view")
	}
	if controlScript < logviewScript {
		t.Error("js/servicecontrol.js must load after js/logview.js")
	}
	if viewScript < controlScript {
		t.Error("js/servicecontrol.js must load before js/runtime.js: the view drives the menu it wires")
	}
}

// TestRuntimeUIServiceControlMarkup pins the one control of the header: the
// trigger, the menu with its items in the order the product names, and the
// chip for a document without an owner. The menu is part of the markup, not
// built by the script, so the fallback document of the window host can
// render the same control.
func TestRuntimeUIServiceControlMarkup(t *testing.T) {
	markup := runtimeAssetText(t, "runtime.html")

	for _, want := range []string{
		`id="serviceControl"`,
		`id="serviceTrigger"`,
		`aria-haspopup="menu"`,
		`aria-expanded="false"`,
		`aria-controls="serviceMenu"`,
		`id="serviceMenu"`,
		`role="menu"`,
		`aria-labelledby="serviceTrigger"`,
		`id="serviceChip"`,
	} {
		if !strings.Contains(markup, want) {
			t.Errorf("runtime.html does not carry %s", want)
		}
	}

	// The control replaced the badge and the inline button of the header.
	for _, unwanted := range []string{`id="serviceBadge"`, `id="installServiceButton"`} {
		if strings.Contains(markup, unwanted) {
			t.Errorf("runtime.html still carries %s: the service control replaced it", unwanted)
		}
	}

	menu := markup[strings.Index(markup, `id="serviceMenu"`):]
	if end := strings.Index(menu, `id="serviceChip"`); end >= 0 {
		menu = menu[:end]
	}

	want := []string{"Install", "Start", "Stop", "Update", "Delete"}
	var keys []string
	for _, match := range regexp.MustCompile(`id="service(Install|Start|Stop|Update|Delete)Item"`).
		FindAllStringSubmatch(menu, -1) {
		keys = append(keys, match[1])
	}
	if strings.Join(keys, ",") != strings.Join(want, ",") {
		t.Errorf("the menu items are %v, want %v", keys, want)
	}
	if got := strings.Count(menu, `role="menuitem"`); got != len(want) {
		t.Errorf("the menu carries %d menu items, want %d", got, len(want))
	}
	// Every item names its action: the control unit forwards the name and
	// never guesses it from an id.
	for _, action := range []string{"install", "start", "stop", "update", "delete"} {
		if !strings.Contains(menu, `data-service-action="`+action+`"`) {
			t.Errorf("the menu does not carry the action %q", action)
		}
	}
	if !strings.Contains(menu, `role="separator"`) {
		t.Error("the menu carries no separator before the destructive item")
	}
	if !strings.Contains(menu, "runtime-service-item-danger") {
		t.Error("the delete item is not marked as the destructive item")
	}

	// The trigger is the one place the state line appears: the menu carries
	// the actions and nothing else.
	if strings.Contains(menu, "serviceLabel") {
		t.Error("the menu carries a second state line, want the actions only")
	}
	for _, label := range []string{
		"Install service", "Start service", "Stop service", "Update service", "Delete service",
	} {
		if !strings.Contains(menu, label) {
			t.Errorf("the menu does not name %q", label)
		}
	}
}

// TestRuntimeUIServiceMenuStyles pins what keeps the menu usable in the
// narrow window: the shell hides its overflow, so the menu is positioned
// inside the header and bounded by the viewport, in the visual language of
// the design system.
func TestRuntimeUIServiceMenuStyles(t *testing.T) {
	css := runtimeAssetText(t, "css/runtime.css")

	if !strings.Contains(css, "[hidden] { display: none !important; }") {
		t.Error("css/runtime.css does not keep the hidden attribute effective")
	}

	block := regexp.MustCompile(`(?s)\.runtime-service-menu \{[^}]*\}`).FindString(css)
	if block == "" {
		t.Fatal("css/runtime.css carries no .runtime-service-menu rule")
	}
	for _, want := range []string{
		"position: fixed",
		"z-index:",
		"max-width: calc(100vw - 28px)",
		"box-shadow: var(--shadow-lg)",
		"border-radius: var(--radius-md)",
		"background: var(--color-surface)",
	} {
		if !strings.Contains(block, want) {
			t.Errorf("the menu rule does not carry %q, want a menu inside the viewport", want)
		}
	}
	if !strings.Contains(css, ":focus-visible") {
		t.Error("css/runtime.css carries no visible focus for the control")
	}
	// The menu is anchored to the trigger by the control unit: a fixed menu
	// without a coordinate would sit in the corner of the viewport.
	if !strings.Contains(runtimeAssetText(t, "js/servicecontrol.js"), "getBoundingClientRect()") {
		t.Error("js/servicecontrol.js does not anchor the menu at its trigger")
	}
}

// TestRuntimeUIAssetsAreSelfContained pins the files the fallback document
// of the window host inlines: none may name an external resource, open a
// connection of its own or import anything.
func TestRuntimeUIAssetsAreSelfContained(t *testing.T) {
	cases := []struct {
		name      string
		forbidden []string
	}{
		{
			name: "css/style.css",
			forbidden: []string{
				"url(", "@import", "http://", "https://",
				"<img", "<link", "<script", "<iframe", "@font-face",
			},
		},
		{
			name: "css/runtime.css",
			forbidden: []string{
				"url(", "@import", "http://", "https://",
				"<img", "<link", "<script", "<iframe", "@font-face",
			},
		},
		{
			name: "js/logview.js",
			forbidden: []string{
				"fetch(", "EventSource", "XMLHttpRequest", "WebSocket",
				"import ", "import(", "require(",
				"url(", "http://", "https://",
				"<img", "<link", "<script", "<iframe", "src=",
			},
		},
		{
			name: "js/servicecontrol.js",
			forbidden: []string{
				"fetch(", "EventSource", "XMLHttpRequest", "WebSocket",
				"import ", "import(", "require(",
				"url(", "http://", "https://",
				"<img", "<link", "<script", "<iframe", "src=",
				"innerHTML",
			},
		},
	}
	for _, tc := range cases {
		text := runtimeAssetText(t, tc.name)
		for _, token := range tc.forbidden {
			if strings.Contains(text, token) {
				t.Errorf("%s carries %q: the file must be self contained", tc.name, token)
			}
		}
	}

	// The page itself may load its own local assets, but nothing remote.
	markup := runtimeAssetText(t, "runtime.html")
	if match := regexp.MustCompile(`(?i)(href|src)="(https?:)?//`).FindString(markup); match != "" {
		t.Errorf("runtime.html loads a remote resource: %s", match)
	}
	if strings.Contains(markup, "@import") {
		t.Error("runtime.html imports a stylesheet, want the local links")
	}
}

// TestRuntimeUIViewCarriesNoServiceStateTable pins the single owner of the
// service rules. The words and the action map come from internal/serviceview
// over the status endpoint; neither script may decide anything from a state
// word of its own.
func TestRuntimeUIViewCarriesNoServiceStateTable(t *testing.T) {
	forbidden := []string{
		"serviceState",
		"serviceStatusView",
		"Service: not installed",
		"Service: installed",
		"Service: unknown",
		"'not-installed'",
		`"not-installed"`,
		"'broken'",
		`"broken"`,
		"'starting'",
		`"starting"`,
		"'stopping'",
		`"stopping"`,
	}
	for _, name := range []string{"js/runtime.js", "js/logview.js", "js/servicecontrol.js"} {
		text := runtimeAssetText(t, name)
		for _, token := range forbidden {
			if strings.Contains(text, token) {
				t.Errorf("%s carries %q: the service rules belong to internal/serviceview", name, token)
			}
		}
	}
}

// TestRuntimeUIServiceControlUnitIsOneGlobal pins the unit of the menu as a
// file of its own: it exposes exactly one global, carries no bound action
// name of its own (the caller supplies what an activation means) and no
// state vocabulary (the server classifies the state).
func TestRuntimeUIServiceControlUnitIsOneGlobal(t *testing.T) {
	unit := runtimeAssetText(t, "js/servicecontrol.js")

	if got := strings.Count(unit, "global.HFDesk"); got != 1 {
		t.Errorf("js/servicecontrol.js assigns %d globals, want exactly one", got)
	}
	if !strings.Contains(unit, "global.HFDeskServiceControl = { wire: wire };") {
		t.Error("js/servicecontrol.js does not expose window.HFDeskServiceControl.wire")
	}
	for _, token := range regexp.MustCompile(`hfdesk[A-Za-z0-9_$]*`).FindAllString(unit, -1) {
		t.Errorf("js/servicecontrol.js carries the bound action name %q: the caller owns the actions", token)
	}
}

// TestRuntimeUIPageCallsOnlyBoundWindowActions pins the JavaScript side of
// the window bridge: the page may reference only the action names the window
// host binds, and it must reference the five service actions — a rename on
// either side that leaves the other behind is a dead item in the live page.
func TestRuntimeUIPageCallsOnlyBoundWindowActions(t *testing.T) {
	view := runtimeAssetText(t, "js/runtime.js")

	allowed := map[string]bool{
		"hfdesk":               true, // the plain prefix of the storage key
		"hfdeskOpenUi":         true,
		"hfdeskOpenLogFolder":  true,
		"hfdeskOpenLogFile":    true,
		"hfdeskTryAgain":       true,
		"hfdeskStartService":   true,
		"hfdeskStopService":    true,
		"hfdeskUpdateService":  true,
		"hfdeskDeleteService":  true,
		"hfdeskInstallService": true,
	}
	for _, token := range regexp.MustCompile(`hfdesk[A-Za-z0-9_$]*`).FindAllString(view, -1) {
		if !allowed[token] {
			t.Errorf("runtime.js carries the token %q, want only the bound action names", token)
		}
	}
	for _, name := range []string{
		"hfdeskOpenUi", "hfdeskOpenLogFolder", "hfdeskOpenLogFile",
		"hfdeskStartService", "hfdeskStopService", "hfdeskUpdateService",
		"hfdeskDeleteService", "hfdeskInstallService",
	} {
		if !strings.Contains(view, name) {
			t.Errorf("runtime.js never references %q: the window host binds it", name)
		}
	}
}
