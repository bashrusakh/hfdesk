// Copyright 2026
// SPDX-License-Identifier: Apache-2.0

//go:build windows

package desktop

import (
	"errors"
	"runtime"
	"sync"
	"time"

	"github.com/jchv/go-webview2"
	"golang.org/x/sys/windows"
)

// activateTimeout bounds the wait for the runtime window to answer.
//
// The window lives on its own thread, so an action that reaches it from
// another thread is marshalled onto that thread. The wait bounds the case
// where the window was closed in the meantime: the message is dropped and
// the caller creates a new window.
const activateTimeout = 3 * time.Second

// webView is the small interface the desktop host drives the runtime window
// with.
//
// The WebView2 binding stays behind this interface, so it cannot leak into
// the rest of the host: the host only shows, hides, navigates and closes a
// window.
type webView interface {
	// Show makes the window visible and gives it the focus. It reports
	// an error when the window is gone.
	Show() error
	// Hide hides the window without destroying it.
	Hide()
	// Navigate loads a URL in the window.
	Navigate(url string)
	// NavigateDocument loads a self-contained HTML document in the
	// window — the window of a tray companion, which has no server to
	// fetch its page from.
	NavigateDocument(html string)
	// BindAction registers a host handler for one fixed action name the
	// loaded document can call — the WebView2 bridge of the companion
	// window's single open-the-full-log button. The document names the
	// action and carries no data; the host owns everything the handler
	// acts on. Registration runs before the first document loads, so
	// the page finds the function already bound.
	BindAction(name string, handler func())
	// Close destroys the window.
	Close()
}

// runtimeWindow owns the Runtime / Logs window.
//
// The window lives on its own thread with its own message loop: the WebView2
// library posts WM_QUIT to the thread of the window when the window is
// destroyed, so its loop ends with the window and not with the application.
// Closing the window therefore leaves HFDesk running in the tray.
type runtimeWindow struct {
	// mu guards the current instance.
	mu sync.Mutex
	// instance is the window that is currently open.
	instance *windowInstance
	// createMu serializes the creation of a window, so two actions cannot
	// create two windows.
	createMu sync.Mutex
	// url is the page the window shows. It is the page of the running
	// own server, and it is only used in URL mode (content == nil).
	url string
	// content builds the page of the window in document mode — the tray
	// companion, which has no server. A non-nil content means the window
	// renders this document fresh on every show, because the state it
	// reports can change while the window exists; in URL mode nothing
	// re-navigates after the creation, exactly as before.
	content func() string
	// actions are the host handlers the document of a companion window
	// can call by name (nil for the URL-mode window of the desktop
	// host): they are bound when the window is created, before any
	// document loads, and they run on the host side — the page itself
	// reaches no server.
	actions map[string]func()
	// iconBytes are the bytes of the icon of the window.
	iconBytes []byte
}

// windowInstance is one runtime window.
type windowInstance struct {
	// view is the window. It is held behind the small webView interface,
	// so the WebView2 types stay inside this file.
	view webView
	// done is closed when the window was destroyed and its message loop
	// ended.
	done chan struct{}
	// icons are the icon handles of the window. They are released
	// together with the window.
	icons windowIcons
}

// newRuntimeWindow builds the owner of the Runtime / Logs window that
// shows the runtime page of the running own server.
func newRuntimeWindow(url string, iconBytes []byte) *runtimeWindow {
	return &runtimeWindow{url: url, iconBytes: iconBytes}
}

// newCompanionWindow builds the owner of the Runtime / Logs window of a
// tray companion: the window has no server of its own, so it renders the
// document content builds — the honest state of the service, its
// registered web-UI address, the log file of this launch and the
// service-log section — re-renders it on every show, and binds the host
// handlers the document's single action calls.
func newCompanionWindow(content func() string, iconBytes []byte, actions map[string]func()) *runtimeWindow {
	return &runtimeWindow{content: content, actions: actions, iconBytes: iconBytes}
}

// Show makes the runtime window visible.
//
// The window is created when it does not exist yet, so the action works
// again after the user closed it. A window in document mode renders its
// page right before it is shown: the state of the service can change
// while the window exists, and a show is the moment the user asks for
// it. A URL-mode window navigates once at its creation, exactly as
// before.
func (w *runtimeWindow) Show() error {
	// Two attempts: the first one can lose the race against a window
	// that was closed in the meantime, the second one works with a fresh
	// window.
	for attempt := 0; attempt < 2; attempt++ {
		instance, err := w.active()
		if err != nil {
			return err
		}
		if w.content != nil {
			instance.view.NavigateDocument(w.content())
		}
		if err := instance.view.Show(); err != nil {
			// The window was closed while the action was on its way:
			// try again with a new one.
			continue
		}
		return nil
	}
	return errors.New("desktop: the runtime window could not be shown")
}

// Close destroys the runtime window.
//
// The application keeps running: the window is only a view of the running
// server.
func (w *runtimeWindow) Close() {
	w.mu.Lock()
	instance := w.instance
	w.mu.Unlock()
	if instance == nil {
		return
	}
	instance.view.Close()
}

// active returns the window that is currently open, creating it when it
// does not exist.
func (w *runtimeWindow) active() (*windowInstance, error) {
	w.mu.Lock()
	current := w.instance
	w.mu.Unlock()
	if current != nil && !current.closed() {
		return current, nil
	}

	w.createMu.Lock()
	defer w.createMu.Unlock()

	// Another action may have created the window in the meantime.
	w.mu.Lock()
	current = w.instance
	w.mu.Unlock()
	if current != nil && !current.closed() {
		return current, nil
	}
	return w.create()
}

// create creates the runtime window on a thread of its own.
//
// The window is created and pumped on that thread: the WebView2 library
// ends the loop of the thread of the window when the window is destroyed, so
// the loop must not be the loop of the tray.
func (w *runtimeWindow) create() (*windowInstance, error) {
	type creation struct {
		instance *windowInstance
		err      error
	}
	ready := make(chan creation, 1)

	go func() {
		// The window belongs to this thread, so the thread is locked to
		// it for the whole life of the window.
		runtime.LockOSThread()
		defer runtime.UnlockOSThread()

		view := webview2.NewWithOptions(webview2.WebViewOptions{
			Debug:     false,
			AutoFocus: true,
			WindowOptions: webview2.WindowOptions{
				Title:  "HFDesk - Runtime / Logs",
				Width:  760,
				Height: 560,
				Center: true,
			},
		})
		if view == nil {
			ready <- creation{err: errors.New("desktop: the WebView2 window could not be created")}
			return
		}

		hwnd := windows.HWND(uintptr(view.Window()))
		icons, err := applyWindowIcon(hwnd, w.iconBytes)
		if err != nil {
			desktopLog.Warn("could not set the icon of the runtime window", "error", err)
		}

		done := make(chan struct{})
		driver := &webview2Driver{view: view, hwnd: hwnd, done: done}
		// The host handlers the document can call are bound before
		// any document loads: the binding injects its function into
		// every document the window creates from here on, so the
		// companion page finds it already there when its button is
		// clicked. The URL-mode window carries no actions and binds
		// nothing.
		for name, handler := range w.actions {
			driver.BindAction(name, handler)
		}
		instance := &windowInstance{view: driver, done: done, icons: icons}

		w.mu.Lock()
		w.instance = instance
		w.mu.Unlock()

		if w.content == nil {
			// URL mode: the page of the own server is loaded right
			// here, once. A companion window (content != nil)
			// starts blank: Show renders its document, so the
			// state the page reports is the state of the moment
			// the user opens it.
			view.Navigate(w.url)
		}
		ready <- creation{instance: instance}

		// The message loop ends when the window is destroyed.
		view.Run()
		close(done)
		instance.icons.destroy()

		w.mu.Lock()
		if w.instance == instance {
			w.instance = nil
		}
		w.mu.Unlock()
	}()

	outcome := <-ready
	if outcome.err != nil {
		return nil, outcome.err
	}
	return outcome.instance, nil
}

// closed reports that the window was destroyed.
func (i *windowInstance) closed() bool {
	select {
	case <-i.done:
		return true
	default:
		return false
	}
}

// webview2Driver drives a WebView2 window.
//
// Every action is marshalled onto the thread of the window with Dispatch,
// because the window belongs to that thread and the library ends the loop of
// its thread when the window is destroyed.
type webview2Driver struct {
	// view is the WebView2 view of the window.
	view webview2.WebView
	// hwnd is the native handle of the window.
	hwnd windows.HWND
	// done is closed when the window was destroyed.
	done chan struct{}
}

// The driver is the implementation of the small window interface.
var _ webView = (*webview2Driver)(nil)

// Show makes the window visible and gives it the focus.
//
// It reports an error when the window was closed in the meantime: the
// message is dropped then, and the caller creates a new window.
func (d *webview2Driver) Show() error {
	shown := make(chan struct{})
	d.view.Dispatch(func() {
		showWindow(d.hwnd)
		close(shown)
	})

	select {
	case <-shown:
		return nil
	case <-d.done:
		// The window was closed while the action was on its way. It
		// may still have been shown, so the confirmation decides.
		select {
		case <-shown:
			return nil
		default:
		}
		return errors.New("desktop: the runtime window was closed")
	case <-time.After(activateTimeout):
		return errors.New("desktop: the runtime window did not answer")
	}
}

// Hide hides the window without destroying it.
func (d *webview2Driver) Hide() {
	d.view.Dispatch(func() { hideWindow(d.hwnd) })
}

// Navigate loads a URL in the window.
func (d *webview2Driver) Navigate(url string) {
	d.view.Dispatch(func() { d.view.Navigate(url) })
}

// NavigateDocument loads a self-contained HTML document in the window.
//
// The navigation runs through the documented HTML entry of the WebView2
// binding (SetHtml → NavigateToString) on the thread of the window, like
// every other action of the driver: the document needs no listener, which
// is the point of the companion window — nothing of it listens.
func (d *webview2Driver) NavigateDocument(html string) {
	d.view.Dispatch(func() { d.view.SetHtml(html) })
}

// BindAction registers a host handler for one fixed action name under the
// WebView2 binding of the window (the binding injects a global function of
// that name into every document the window creates).
//
// It runs at the creation of the window, on the thread that owns it, before
// any document loads. A binding the library refuses is recorded — the
// button of the page then finds no function and does nothing — instead of
// failing the window that is otherwise fine.
func (d *webview2Driver) BindAction(name string, handler func()) {
	if err := d.view.Bind(name, handler); err != nil {
		desktopLog.Warn("could not bind the action of the window document",
			"action", name, "error", err)
	}
}

// Close destroys the window.
func (d *webview2Driver) Close() {
	d.view.Dispatch(func() { d.view.Destroy() })
}
