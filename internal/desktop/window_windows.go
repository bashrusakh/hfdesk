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

// windowShutdownTimeout bounds the wait for a window that is destroyed by the
// shutdown of its host.
//
// The window ends its message loop when it is destroyed, so the wait is the
// moment between the request and the end of that loop. The budget only covers
// a window whose runtime does not answer: a shutdown that waited longer would
// have to keep a server alive for a window nobody can see.
const windowShutdownTimeout = activateTimeout

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
	// Navigate loads a URL in the window: the runtime page of a
	// server — the own server of the desktop host, or the server of the
	// registered service beside a tray companion.
	Navigate(url string)
	// NavigateDocument loads a self-contained HTML document in the
	// window — the fallback state of a tray companion whose registered
	// address does not answer, which has no server to fetch its page
	// from.
	NavigateDocument(html string)
	// BindAction registers a host handler for one fixed action name the
	// loaded document can call — the WebView2 bridge of the window's
	// host actions (open the log file, open the log folder, open the
	// web UI, retry). The document names the action and carries no
	// data; the host owns everything the handler acts on, and it acts
	// in the process of the window — in the user's session.
	// Registration runs before the first document loads, so
	// the page finds the function already bound.
	BindAction(name string, handler func())
	// Close destroys the window.
	Close()
}

// runtimeWindow owns the Logs window — the ONE window both hosts open.
//
// The window lives on its own thread with its own message loop: the WebView2
// library posts WM_QUIT to the thread of the window when the window is
// destroyed, so its loop ends with the window and not with the application.
// Closing the window therefore leaves HFDesk running in the tray.
//
// Every show decides what the window shows, so a repeated open shows the
// actual state: in URL mode the page of the own server is (re)loaded, and
// beside a tray companion the registered service address is probed and the
// live runtime page is loaded when it answers, the fallback document when
// it does not. A window that stays open follows that state as well: on the
// tick of the tray menu it switches its content when the KIND of the
// content changed, and it re-renders the fallback document when the STATE
// that document was built from changed, so a service that stops under an
// open window reaches the state and the actions of the fallback document,
// too, and every later state of that window is the state of its own read
// (see follow).
type runtimeWindow struct {
	// mu guards the current instance.
	mu sync.Mutex
	// instance is the window that is currently open.
	instance *windowInstance
	// createMu serializes the creation of a window, so two actions cannot
	// create two windows.
	createMu sync.Mutex
	// navMu serializes the content decisions of the window and guards the
	// shown record of its instance: the show of the user and the follow of
	// the tray tick decide one at a time, so the record always states what
	// the window actually loaded and no decision is taken from a state
	// another decision has already replaced.
	navMu sync.Mutex
	// url is the page the window loads on every show in URL mode: the
	// runtime page of the running own server (the desktop host). It is
	// only used when live is nil.
	url string
	// live resolves the runtime page of the REGISTERED service address
	// on every show — the tray companion, which has no server of its
	// own. An empty result (no readable registration) and an address
	// that does not answer both lead to the fallback document in
	// content. Nil for the desktop host.
	live func() string
	// content builds the fallback document of the window and reports the
	// key of the read it rendered: the state beside an address that does
	// not answer, so the record of the window always states what the
	// document was built from. Nil for the desktop host, whose own server
	// always answers.
	content func() (key string, document string)
	// activate asks the host to re-read the state when the user comes back
	// to the window: the established refresh signal, which runs the same
	// read and the same follow as the tray tick. It only posts a signal and
	// never blocks the message loop. Nil for the desktop host, which
	// follows no service.
	activate func()
	// actions are the host handlers the loaded document can call by
	// name: they are bound when the window is created, before any
	// document loads — the live runtime page and the fallback document
	// alike — and they run on the host side, in the process of the
	// window, so an open action reaches the user's session even when
	// the server behind the page runs as a service in session 0.
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
	// hwnd is the native handle of the window.
	hwnd windows.HWND
	// previousProc and windowProc are the window procedure the window had
	// before this owner subscribed it to the return of the focus, and the
	// callback of that subscription. Zero when nothing is subscribed.
	previousProc uintptr
	windowProc   uintptr
	// requestRefresh asks the host to re-read the state, on the thread of
	// the window, through the established refresh signal.
	requestRefresh func()
	// shown is what the window currently loads. It is written and read
	// under the navMu of the window that owns this instance, and it is the
	// record the follow of the tray tick decides on: the same kind at the
	// same address — or the same fallback state — has nothing to switch.
	shown shownContent
}

// contentKind is the kind of the content the Logs window loads.
type contentKind int

const (
	// contentUndecided is a window that was created but not navigated
	// yet. Show decides the content right after the creation, so this is
	// the state of the moments in between — and of a window whose first
	// decision still has to be taken by a follow.
	contentUndecided contentKind = iota
	// contentLive is the live runtime page of a server: the own server of
	// the desktop host, or the server of the registered service beside a
	// tray companion.
	contentLive
	// contentFallback is the self-contained fallback document of a tray
	// companion, shown beside an address that does not answer or names
	// none.
	contentFallback
)

// shownContent records what the window was last navigated to: the kind of
// that content, the address of the live page, and the key of the read the
// fallback document was rendered from.
//
// The address matters as much as the kind: a registration that moves to
// another address is a change the window has to follow, while a page that
// still answers at the very same address is not — reloading it would only
// reset the scroll position of the user in the live log.
//
// The key is what the document SAYS, not what kind it is: a document rendered
// from one read stays while the next read of the same state comes back with
// the same key, and it is re-rendered when that state changed.
type shownContent struct {
	// kind is the kind of the content the window loads.
	kind contentKind
	// url is the address of the live page the window loads. Empty beside
	// the fallback document.
	url string
	// key identifies the state the fallback document was rendered from.
	// Empty beside the live page.
	key string
}

// newRuntimeWindow builds the owner of the Logs window that shows the
// runtime page of the running own server: every show reloads that page,
// so a repeated open shows the actual state of the server instead of a
// view the window remembered. actions are the host handlers the page can
// call (nil binds nothing).
func newRuntimeWindow(url string, iconBytes []byte, actions map[string]func()) *runtimeWindow {
	return &runtimeWindow{url: url, actions: actions, iconBytes: iconBytes}
}

// newCompanionWindow builds the owner of the Logs window of a tray
// companion: the window has no server of its own, so every show resolves
// the REGISTERED service address through live and loads its runtime page
// when the address answers — the same live log, filters and search the
// desktop window shows — or renders the fallback document content builds
// from that same read when it does not, and it binds the host handlers the
// documents call. activate is the established refresh signal of the host,
// which the window posts when the user comes back to it.
func newCompanionWindow(live func() string, content func() (string, string), activate func(),
	iconBytes []byte, actions map[string]func()) *runtimeWindow {
	return &runtimeWindow{live: live, content: content, activate: activate, actions: actions, iconBytes: iconBytes}
}

// Show makes the Logs window visible.
//
// The window is created when it does not exist yet, so the action works
// again after the user closed it. Every show decides the content first:
// in URL mode the page of the own server is loaded again, and beside a
// tray companion the registered address is probed and the live runtime
// page or the fallback document is loaded — the state can change while
// the window exists, and a show is the moment the user asks for it, so a
// repeated open shows what is actual now.
func (w *runtimeWindow) Show() error {
	// Two attempts: the first one can lose the race against a window
	// that was closed in the meantime, the second one works with a fresh
	// window.
	for attempt := 0; attempt < 2; attempt++ {
		instance, err := w.active()
		if err != nil {
			return err
		}
		w.load(instance)
		if err := instance.view.Show(); err != nil {
			// The window was closed while the action was on its way:
			// try again with a new one.
			continue
		}
		return nil
	}
	return errors.New("desktop: the logs window could not be shown")
}

// resolve decides what this show loads: the URL of the own server in URL
// mode, and beside a tray companion the live runtime page of the
// registered address when it answers, or the key and the document of the
// read that proves it does not (see resolveLogsWindow).
func (w *runtimeWindow) resolve() (target, key, document string) {
	if w.live == nil {
		return w.url, "", ""
	}
	return resolveLogsWindow(w.live, w.content)
}

// load decides what the window shows and loads it, recording the decision so
// the follow of the tray tick can tell whether the kind of the content, its
// address, or the state the fallback document was rendered from changed.
//
// A show always loads its content, even the content the window already shows:
// a show is the user asking for the actual state, so a repeated open of a live
// page reloads it. Only the follow of the tick leaves an unchanged content
// alone (see follow).
func (w *runtimeWindow) load(instance *windowInstance) {
	w.navMu.Lock()
	defer w.navMu.Unlock()
	target, key, document := w.resolve()
	if document != "" {
		instance.view.NavigateDocument(document)
		instance.shown = shownContent{kind: contentFallback, key: key}
		return
	}
	instance.view.Navigate(target)
	instance.shown = shownContent{kind: contentLive, url: target}
}

// follow re-checks the content of the open window against the state of the
// service beside it and switches it when the content or its state changed:
// the live page of the registered address becomes the fallback document when
// the address stops answering, the fallback document becomes the live page
// when the address answers again, and a fallback document is re-rendered when
// the STATE it was rendered from changed. Without it a window left open beside
// a service that stops would keep a page whose server is gone, and a fallback
// document would keep the state and the action of a service that moved on
// under it, until the user closed and opened it again.
//
// page, key and document are ONE display read — the very read the tray menu
// just made: the runtime page of the registered address (empty beside a failed
// read, a machine without a registration and an address that cannot be read as
// a URL), the key of the state that read proves, and the fallback document
// rendered from that same read. The document is built only when the window
// really has to switch, so an unchanged state costs no render.
//
// The window follows the established tick of the tray menu (the cadence of the
// service status line, see keepFresh): it owns no timer of its own, so an open
// window follows the machine at the speed the menu already follows it. One
// pass costs at most one probe of the address, and only while the window
// exists — a closed window costs nothing — and a window that already shows the
// content of the state is left alone: the same kind at the same address, and
// the same key beside the fallback document, is not a change, so the live log
// of the user keeps its scroll position and an unchanged fallback document is
// not reloaded under the user.
//
// URL mode has no fallback kind to switch to — the page of the own server of
// the desktop host is the only content of that window — so the follow ends
// there and never touches a window of the desktop host. The same holds for a
// window wired without a fallback builder: there is no document to switch to.
func (w *runtimeWindow) follow(page, key string, document func() string) {
	if w.live == nil || w.content == nil {
		return
	}
	instance := w.open()
	if instance == nil {
		// No window is open: there is nothing to follow, and the tick
		// costs no probe.
		return
	}

	w.navMu.Lock()
	defer w.navMu.Unlock()
	if instance.closed() {
		// The window was closed between the two checks: the actions of a
		// window that is gone are dropped with it.
		return
	}
	if page != "" && runtimePageAnswers(page) {
		if instance.shown.kind == contentLive && instance.shown.url == page {
			// The live page answers at the very address it was
			// loaded from: the window shows what the state is.
			return
		}
		instance.view.Navigate(page)
		instance.shown = shownContent{kind: contentLive, url: page}
		return
	}
	if instance.shown.kind == contentFallback && instance.shown.key == key {
		// The window already shows the document of this very state:
		// nothing it says changed, and reloading it would only reset
		// the tail of the log the user scrolled.
		return
	}
	// The document is rendered now, from the state of this moment, so it
	// states the state the window switches to and not the one it left.
	// Rendering it is a display read like every other: it asks the menu to
	// catch up, so this switch costs one more pass, which finds the same
	// kind and key and loads nothing.
	instance.view.NavigateDocument(document())
	instance.shown = shownContent{kind: contentFallback, key: key}
}

// Close destroys the runtime window.
//
// The application keeps running: the window is only a view of the running
// server.
func (w *runtimeWindow) Close() {
	w.close()
}

// CloseAndWait destroys the runtime window and waits, up to budget, for its
// message loop to end.
//
// It is the close of a host that stops the server it shows: the page of the
// window is a page of that server, and it paints its own error state — the
// red error banner and the red status indicators — as soon as the server
// stops answering. Destroying the window BEFORE the server stops, and waiting
// for the end of its loop, is what keeps a committed handover (and every
// ordinary quit) from flashing that state: there is no page left to paint it,
// while a server that fails WITHOUT this shutdown leaves the window open and
// its errors visible.
//
// A window that does not answer within the budget is left to the process
// exit: the shutdown of the host is never held by a window nobody can see.
func (w *runtimeWindow) CloseAndWait(budget time.Duration) {
	instance := w.close()
	if instance == nil {
		return
	}

	select {
	case <-instance.done:
	case <-time.After(budget):
	}
}

// close destroys the window that is currently open and returns its instance,
// or nil when no window is open.
func (w *runtimeWindow) close() *windowInstance {
	w.mu.Lock()
	instance := w.instance
	w.mu.Unlock()
	if instance == nil {
		return nil
	}
	instance.view.Close()
	return instance
}

// open returns the window that is currently open, without creating one.
//
// The follow of the tray tick asks this question: a window the user closed is
// no window to follow, and no one but a show may create one.
func (w *runtimeWindow) open() *windowInstance {
	w.mu.Lock()
	current := w.instance
	w.mu.Unlock()
	if current == nil || current.closed() {
		return nil
	}
	return current
}

// active returns the window that is currently open, creating it when it
// does not exist.
func (w *runtimeWindow) active() (*windowInstance, error) {
	if current := w.open(); current != nil {
		return current, nil
	}

	w.createMu.Lock()
	defer w.createMu.Unlock()

	// Another action may have created the window in the meantime.
	if current := w.open(); current != nil {
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
				Title:  "HFDesk - Logs",
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
			desktopLog.Warn("could not set the icon of the logs window", "error", err)
		}

		done := make(chan struct{})
		driver := &webview2Driver{view: view, hwnd: hwnd, done: done}
		// The host handlers the document can call are bound before
		// any document loads: the binding injects its function into
		// every document the window creates from here on, so the live
		// runtime page of a server and the local fallback document
		// find the functions already there when their buttons are
		// clicked.
		for name, handler := range w.actions {
			driver.BindAction(name, handler)
		}
		instance := &windowInstance{view: driver, done: done, icons: icons, hwnd: hwnd}
		// The focus is followed before the first message of the loop: a
		// window the user comes back to re-reads its state through the
		// established refresh signal of the host instead of keeping a
		// page whose server is gone until the next tick.
		instance.followActivation(w.activate)

		w.mu.Lock()
		w.instance = instance
		w.mu.Unlock()

		// The window starts blank: Show decides the content right
		// after the creation, so what the window shows is the state
		// of the moment the user opens it — the page of the own
		// server in URL mode, the live page of the registered
		// address or the fallback document beside a companion.
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

// The message and index of the focus subscription of the Logs window.
const (
	// wmActivate is the message Windows sends when a window gains or
	// loses the activation.
	wmActivate = 0x0006
	// waInactive is the wParam of wmActivate that reports the LOSS of the
	// focus; every other value reports that the window got it.
	waInactive = 0
	// gwlpWndProc is the index of the window procedure of a window.
	gwlpWndProc = ^uintptr(3)
)

var (
	// procSetWindowLongPtrW replaces the window procedure of a window.
	procSetWindowLongPtrW = user32.NewProc("SetWindowLongPtrW")
	// procCallWindowProcW calls the window procedure a window had.
	procCallWindowProcW = user32.NewProc("CallWindowProcW")
)

// windowActivationRequestsRefresh reports whether one window message is the
// return of the focus to the window: wmActivate with any wParam but
// waInactive.
func windowActivationRequestsRefresh(msg, wp uintptr) bool {
	return msg == wmActivate && wp != waInactive
}

// followActivation subscribes the window to the return of the focus, so an
// open window asks the host to re-read its state through the very path of the
// tray tick instead of keeping a page whose server is gone until the next
// tick — with no timer of the window's own.
//
// request runs on the thread of the window and only posts the established
// refresh signal: it never blocks the message loop. A nil request — the
// desktop host, which follows no service — installs nothing, and a window
// procedure that cannot be replaced leaves the window as it is.
func (i *windowInstance) followActivation(request func()) {
	if request == nil || i.hwnd == 0 {
		return
	}
	i.requestRefresh = request
	i.windowProc = windows.NewCallback(func(hwnd, msg, wp, lp uintptr) uintptr {
		if windowActivationRequestsRefresh(msg, wp) {
			i.requestRefresh()
		}
		answer, _, _ := procCallWindowProcW.Call(i.previousProc, hwnd, msg, wp, lp)
		return answer
	})
	previous, _, callErr := procSetWindowLongPtrW.Call(uintptr(i.hwnd), gwlpWndProc, i.windowProc)
	if previous == 0 {
		desktopLog.Warn("could not follow the focus of the logs window", "error", callErr)
		i.requestRefresh = nil
		return
	}
	i.previousProc = previous
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
		return errors.New("desktop: the logs window was closed")
	case <-time.After(activateTimeout):
		return errors.New("desktop: the logs window did not answer")
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
// every other action of the driver: the fallback document needs no
// listener — nothing of it reaches out of the document, which is the
// point of the fallback beside a dead address.
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
