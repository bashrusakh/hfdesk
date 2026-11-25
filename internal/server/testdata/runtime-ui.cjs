// Run through the runtime UI tests of internal/server: the real runtime.js
// view.
//
// The harness loads the shipped runtime.js inside Node's VM with DOM and
// fetch stubs. It drives the actual owners of the view (the status refresh,
// the poll and the re-sync) instead of reimplementing their rules here, so a
// regression in the shipped view is caught and not only a regression of this
// file.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const fixture = JSON.parse(process.env.HFDESK_RUNTIME_FIXTURE || '{}');
if (!Array.isArray(fixture.records) || !fixture.records.length) {
  console.error('runtime-ui: HFDESK_RUNTIME_FIXTURE is missing the log records');
  process.exit(1);
}
if (typeof fixture.clientCursor !== 'number' ||
  typeof fixture.restartHead !== 'number' ||
  typeof fixture.continuedHead !== 'number') {
  console.error('runtime-ui: HFDESK_RUNTIME_FIXTURE is missing the cursors of the scenarios');
  process.exit(1);
}

let assertions = 0;
function ok(value, message) { assertions++; assert.ok(value, message); }
function eq(actual, expected, message) { assertions++; assert.equal(actual, expected, message); }
function deepEq(actual, expected, message) { assertions++; assert.deepEqual(actual, expected, message); }

// =========================================
// DOM stubs
// =========================================

function makeClassList() {
  const values = new Set();
  return {
    add(...names) { names.forEach(name => values.add(name)); },
    remove(...names) { names.forEach(name => values.delete(name)); },
    contains(name) { return values.has(name); }
  };
}

// makeNode builds a node with the parts of the DOM the view uses.
//
// A child is moved when it is appended somewhere else, like the DOM does it,
// because the view drops the rows that fell out of the buffer by moving them
// into a fragment.
function makeNode(extra = {}) {
  const node = {
    value: '',
    checked: false,
    hidden: false,
    disabled: false,
    title: '',
    style: {},
    dataset: {},
    attrs: {},
    classList: makeClassList(),
    handlers: {},
    children: [],
    parentNode: null,
    firstChild: null,
    innerHTML: '',
    scrollTop: 0,
    scrollHeight: 0,
    clientHeight: 0,
    offsetWidth: 0,
    offsetHeight: 0,
    rect: { left: 0, right: 0, top: 0, bottom: 0, width: 0, height: 0 },
    getBoundingClientRect() { return this.rect; },
    _text: '',
    get textContent() { return this._text; },
    set textContent(value) {
      this._text = String(value);
      if (this._text === '') this.replaceChildren();
    },
    setAttribute(name, value) { this.attrs[name] = String(value); },
    removeAttribute(name) { delete this.attrs[name]; },
    getAttribute(name) { return Object.prototype.hasOwnProperty.call(this.attrs, name) ? this.attrs[name] : null; },
    addEventListener(type, fn) { (this.handlers[type] ||= []).push(fn); },
    removeEventListener() {},
    appendChild(child) {
      if (child && child.parentNode) child.parentNode.removeChild(child);
      if (child) child.parentNode = this;
      this.children.push(child);
      this.firstChild = this.children[0] || null;
      return child;
    },
    append(...nodes) { nodes.forEach(node => this.appendChild(node)); },
    removeChild(child) {
      this.children = this.children.filter(item => item !== child);
      if (child) child.parentNode = null;
      this.firstChild = this.children[0] || null;
      return child;
    },
    querySelectorAll(selector) {
      if (selector === '[role="menuitem"]') {
        return this.children.filter(child => child.getAttribute('role') === 'menuitem');
      }
      return [];
    },
    replaceChildren(...nodes) {
      this.children.forEach(child => { child.parentNode = null; });
      this.children = [];
      this.firstChild = null;
      nodes.forEach(node => this.appendChild(node));
    },
    contains(other) {
      for (let node = other; node; node = node.parentNode) {
        if (node === this) return true;
      }
      return false;
    },
    remove() {
      if (this.parentNode) this.parentNode.removeChild(this);
    },
    focus() { document.activeElement = this; },
    scrollTo() {}
  };
  return Object.assign(node, extra);
}

const nodes = new Map();
for (const id of [
  'statusDot', 'statusLabel', 'versionLabel',
  'serviceControl', 'serviceTrigger', 'serviceDot', 'serviceLabel', 'serviceMenu',
  'serviceInstallItem', 'serviceStartItem', 'serviceStopItem', 'serviceUpdateItem',
  'serviceDeleteItem', 'serviceSeparator', 'serviceChip', 'serviceChipDot', 'serviceChipLabel',
  'serverUrl', 'activeJobs',
  'queuedJobs', 'logFile', 'errorBanner', 'errorTitle', 'errorText',
  'openUiButton', 'openUiNote', 'openLogsButton', 'openLogFileButton', 'copyButton', 'clearButton',
  'levelFilter', 'searchInput', 'followToggle', 'streamDot', 'streamLabel',
  'recordCount', 'logView', 'logBody', 'emptyState', 'emptyTitle',
  'emptyText', 'liveRegion', 'toastContainer'
]) {
  nodes.set('#' + id, makeNode());
}

// The service control is one subtree in the document: the trigger, the
// menu with its items and the chip live inside it, the way the markup
// nests them. The outside click of the menu is decided by that subtree.
const serviceControl = nodes.get('#serviceControl');
const serviceMenu = nodes.get('#serviceMenu');
for (const id of [
  'serviceInstallItem', 'serviceStartItem', 'serviceStopItem', 'serviceUpdateItem',
  'serviceSeparator', 'serviceDeleteItem'
]) {
  serviceMenu.appendChild(nodes.get('#' + id));
}

// The menu items carry the role and the action name the shipped markup
// carries, so the service control unit finds them the way it finds the real
// ones.
const SERVICE_ITEM_ACTIONS = {
  serviceInstallItem: 'install',
  serviceStartItem: 'start',
  serviceStopItem: 'stop',
  serviceUpdateItem: 'update',
  serviceDeleteItem: 'delete'
};
for (const [id, action] of Object.entries(SERVICE_ITEM_ACTIONS)) {
  const item = nodes.get('#' + id);
  item.setAttribute('role', 'menuitem');
  item.setAttribute('data-service-action', action);
}
for (const id of [
  'serviceTrigger', 'serviceDot', 'serviceLabel', 'serviceMenu',
  'serviceChip', 'serviceChipDot', 'serviceChipLabel'
]) {
  serviceControl.appendChild(nodes.get('#' + id));
}

const document = {
  readyState: 'loading',
  body: makeNode(),
  activeElement: null,
  handlers: {},
  addEventListener(type, fn) { (this.handlers[type] ||= []).push(fn); },
  createElement() { return makeNode(); },
  createDocumentFragment() { return makeNode(); },
  querySelector() { return null; },
  querySelectorAll() { return []; },
  getElementById(id) { return nodes.get('#' + id) || null; }
};

// =========================================
// Timers
// =========================================

// The timers are recorded instead of run: the view removes a toast after a
// few seconds and flushes the rows in a batch, and the harness wants to look
// at both of them before they happen.
let timers = [];
let timerID = 0;

function fakeSetTimeout(fn, delay) {
  timerID += 1;
  timers.push({ id: timerID, fn, delay });
  return timerID;
}

function fakeClearTimeout(id) {
  timers = timers.filter(timer => timer.id !== id);
}

function fakeSetInterval(fn, delay) {
  timerID += 1;
  timers.push({ id: timerID, fn, delay, interval: true });
  return timerID;
}

function fakeClearInterval(id) {
  fakeClearTimeout(id);
}

// =========================================
// Server stub
// =========================================

// server is the fake the view talks to. The harness sets the head, the
// records and the truncation flag of a scenario before it drives the view.
const server = {
  head: 0,
  records: [],
  truncated: false,
  statusFails: false,
  version: 'test',
  // logFile is the path of the log file the status reports.
  logFile: '',
  // runId is the run of the server. The status reports it, the way the
  // server does. A null value means that the server does not report a run
  // id at all, the way an older build answers.
  runId: 'run-1',
  // service is the classified service view of the runtime status, exactly
  // as internal/serviceview builds it. A null value means that the server
  // reports no service view at all.
  service: null
};

// installs is the install-service action of the host behind the endpoint:
// the harness counts the dispatches and picks how the endpoint answers.
const installs = {
  posts: 0,
  // mode is "ok" (dispatched), "missing" (no host action, 404) or
  // "pending" (the answer waits until the harness releases it).
  mode: 'ok',
  release: null
};

// openLogsPosts counts the requests the view made to the open-logs
// endpoint — the browser path of the folder action. Inside the window the
// window host owns that click, so the count must not move.
const openLogsPosts = { posts: 0 };

const calls = [];

function jsonResponse(body) {
  return {
    ok: true,
    status: 200,
    text: async () => JSON.stringify(body)
  };
}

// =========================================
// Event stream
// =========================================

// The streams the view opened.
//
// The harness counts them, so a view that opens a second stream is caught.
const streams = [];

// FakeEventSource is the event stream of the browser.
//
// The view opens it with the cursor it holds and closes it when it does
// not need it anymore. The harness opens the stream and hands the records
// to the view the way the server sends them.
class FakeEventSource {
  constructor(url) {
    this.url = url;
    this.onopen = null;
    this.onerror = null;
    this.closed = false;
    this.listeners = new Map();
    streams.push(this);
  }

  addEventListener(type, fn) {
    if (!this.listeners.has(type)) this.listeners.set(type, []);
    this.listeners.get(type).push(fn);
  }

  close() { this.closed = true; }

  // open reports that the stream is established.
  open() { if (this.onopen) this.onopen({}); }

  // fail reports an error on the stream.
  fail() { if (this.onerror) this.onerror({}); }

  // deliver hands a record to the view, the way the server sends it.
  deliver(record) {
    for (const fn of this.listeners.get('log') || []) {
      fn({ data: JSON.stringify(record) });
    }
  }
}

// openStreams returns the streams that are still open.
function openStreams() {
  return streams.filter(stream => !stream.closed);
}

// The window records its event handlers, so the harness can hide and show
// the view the way a browser does it.
const window = {
  location: { protocol: 'http:', host: 'hfdesk.test', search: '', hash: '', pathname: '/runtime.html' },
  handlers: {},
  EventSource: FakeEventSource,
  addEventListener(type, fn) { (this.handlers[type] ||= []).push(fn); }
};

const context = {
  window,
  document,
  console,
  URL,
  URLSearchParams,
  EventSource: FakeEventSource,
  setTimeout: fakeSetTimeout,
  clearTimeout: fakeClearTimeout,
  setInterval: fakeSetInterval,
  clearInterval: fakeClearInterval,
  sessionStorage: {
    values: new Map(),
    getItem(key) { return this.values.has(key) ? this.values.get(key) : null; },
    setItem(key, value) { this.values.set(key, String(value)); },
    removeItem(key) { this.values.delete(key); }
  },
  navigator: {},
  fetch: async (rawPath) => {
    calls.push(rawPath);
    if (rawPath === '/api/runtime') {
      if (server.statusFails) {
        throw new Error('the server is not reachable');
      }
      const status = {
        version: server.version,
        status: 'ok',
        url: 'http://127.0.0.1:8080',
        address: '127.0.0.1:8080',
        activeJobs: { running: 0, queued: 0 },
        logCursor: server.head,
        logFile: server.logFile
      };
      // A server that does not report a run id leaves the field out, the
      // way an older build answers.
      if (server.runId !== null && server.runId !== undefined) {
        status.runId = server.runId;
      }
      // A server that does not report the service view leaves the field
      // out, the way an older build answers.
      if (server.service !== null && server.service !== undefined) {
        status.service = server.service;
      }
      return jsonResponse(status);
    }
    if (rawPath === '/api/runtime/install-service') {
      installs.posts += 1;
      if (installs.mode === 'missing') {
        // The body of a 404 of the server, shape for shape.
        return {
          ok: false,
          status: 404,
          text: async () => JSON.stringify({
            ok: false,
            error: 'No service action',
            error_detail: { code: 'not_found', message: 'No service action' }
          })
        };
      }
      if (installs.mode === 'pending') {
        return new Promise((resolve) => {
          installs.release = () => resolve(jsonResponse({ ok: true }));
        });
      }
      return jsonResponse({ ok: true });
    }
    if (rawPath === '/api/runtime/open-logs') {
      openLogsPosts.posts += 1;
      return jsonResponse({ ok: true, path: '/logs' });
    }
    if (rawPath.startsWith('/api/runtime/logs?')) {
      // The server answers with the cursor of the last record it
      // delivered. A page without records keeps the cursor of the client:
      // the page taught the client nothing, so the cursor does not move.
      const after = Number(new URL(rawPath, 'http://hfdesk.test').searchParams.get('after')) || 0;
      const records = server.records;
      const cursor = records.length ? Number(records[records.length - 1].seq) : after;
      return jsonResponse({
        records,
        cursor,
        truncated: server.truncated
      });
    }
    throw new Error('unexpected fetch: ' + rawPath);
  }
};

// =========================================
// The shipped view
// =========================================

// The renderer of one record is a file of its own: the page loads it first,
// and the fallback document of the window host inlines it on its own. The
// harness loads it in the same context, so the view talks to the shipped
// renderer and not to a copy of it.
const logviewSource = fs.readFileSync(
  path.join(__dirname, '..', '..', 'assets', 'static', 'js', 'logview.js'), 'utf8');
vm.runInNewContext(logviewSource, context);

// The interaction of the service menu is a unit of its own as well: the
// page loads it before the view, and the fallback document of the window
// host inlines it.
const serviceControlSource = fs.readFileSync(
  path.join(__dirname, '..', '..', 'assets', 'static', 'js', 'servicecontrol.js'), 'utf8');
vm.runInNewContext(serviceControlSource, context);

let source = fs.readFileSync(path.join(__dirname, '..', '..', 'assets', 'static', 'js', 'runtime.js'), 'utf8');
const marker = '  // Start\n';
if (!source.includes(marker)) {
  console.error('runtime-ui: runtime.js injection marker is missing');
  process.exit(1);
}

// The hook names the owners the harness drives, so the harness runs against
// the shipped view and not a copy of its rules.
const hook = `
  window.test = {
    state,
    refreshStatus,
    pollOnce,
    resync,
    resyncAfterGap,
    readLogs,
    ingest,
    connectStream,
    startPolling,
    wireEvents,
    serviceControl
  };
`;
source = source.replace(marker, hook + '\n' + marker);
vm.runInNewContext(source, context);
const ui = window.test;

// The view registers the handlers of its events when it starts, and the
// harness does not start the view: it drives the owners of the view one by
// one. The handler of the pagehide event is registered here, so the
// scenario of the hidden page can hide the view.
ui.wireEvents();

// =========================================
// Helpers
// =========================================

async function settle() {
  for (let i = 0; i < 8; i++) await new Promise(resolve => setImmediate(resolve));
}

// isFeedTimer tells whether a timer belongs to the live feed of the view.
//
// The poll schedules itself, so running it would never stop, and the
// reconnect replaces the poll with the event stream. A scenario that
// settles the view leaves both of them alone.
function isFeedTimer(timer) {
  return timer.id === ui.state.pollTimer || timer.id === ui.state.reconnectTimer;
}

// runPendingTimers runs the pending timers of the view, oldest first.
//
// The view batches its DOM updates with a timer, so a scenario that wants
// to see the settled view runs the timers instead of waiting for the wall
// clock. The timers run one after the other, because a flush that leaves
// records in the buffer schedules the next batch.
//
// The timers of the live feed are left in the queue, see isFeedTimer.
async function runPendingTimers(budget = 100) {
  for (let i = 0; i < budget; i++) {
    const index = timers.findIndex(timer => !isFeedTimer(timer));
    if (index < 0) return;
    const [timer] = timers.splice(index, 1);
    timer.fn();
    await settle();
  }
  throw new Error(`the view scheduled more than ${budget} timers`);
}

// runPoll runs the pending poll of the fallback once.
//
// The poll schedules the next poll, so it runs once and not again.
async function runPoll() {
  const id = ui.state.pollTimer;
  const index = timers.findIndex(timer => timer.id === id);
  if (index < 0) return false;

  const [timer] = timers.splice(index, 1);
  timer.fn();
  await settle();
  return true;
}

// runPendingTimersOnce runs every pending timer exactly once and returns
// the number of log reads the timers made.
//
// The timers that a timer schedules while it runs stay in the queue, so a
// poll loop does not run again and again.
async function runPendingTimersOnce() {
  const pending = timers.splice(0, timers.length);
  let reads = 0;

  for (const timer of pending) {
    const before = countLogReads();
    timer.fn();
    await settle();
    reads += countLogReads() - before;
  }

  return reads;
}

// countLogReads returns the number of log reads the view did.
function countLogReads() {
  return calls.filter(path => path.startsWith('/api/runtime/logs?')).length;
}

// countStatusReads returns the number of status reads the view did.
function countStatusReads() {
  return calls.filter(path => path === '/api/runtime').length;
}

// logRows returns the rows of the log view.
//
// The DOM stub keeps the rows in the fragment they were built in, so the
// rows are collected from the whole tree of the log body.
function logRows() {
  const rows = [];
  const visit = (node) => {
    for (const child of node.children) {
      if (child.dataset && child.dataset.seq !== undefined) {
        rows.push(child);
        continue;
      }
      visit(child);
    }
  };
  visit(nodes.get('#logBody'));
  return rows;
}

function toastMessages() {
  const container = nodes.get('#toastContainer');
  return container.children.map(toast => {
    const label = toast.children[0];
    return label && label.textContent ? label.textContent : '';
  });
}

function announcement() {
  return nodes.get('#liveRegion').textContent;
}

// dispatchWindow fires an event of the window at the view.
//
// The browser hides a page before it puts it into its back/forward cache
// and shows it again when the user comes back, so the harness does the same
// to drive the view through that round trip.
function dispatchWindow(type, event = {}) {
  for (const handler of window.handlers[type] || []) handler(event);
}

function resetView() {
  ui.state.status = null;
  ui.state.cursor = 0;
  ui.state.cursorValid = false;
  ui.state.runId = '';
  ui.state.records = [];
  ui.state.renderedSeq = 0;
  ui.state.level = 'all';
  ui.state.search = '';
  ui.state.searchLower = '';
  ui.state.follow = true;
  ui.state.cleared = false;
  ui.state.resyncing = false;
  ui.state.polling = false;
  ui.state.logError = '';
  ui.state.statusError = '';
  ui.state.loggedErrors = new Set();
  ui.state.serviceActions = {};
  ui.state.serviceInstallMissing = false;

  // The menu of the previous scenario is closed and its call is released,
  // so a busy trigger or an open menu never reaches the next scenario.
  ui.serviceControl.close(false);
  ui.serviceControl.setBusy(false);

  // The live source of the previous scenario is closed, so the scenarios do
  // not share a stream.
  streams.forEach(stream => stream.close());
  streams.length = 0;
  ui.state.source = null;
  ui.state.streamState = 'connecting';
  ui.state.hidden = false;
  ui.state.resumeSource = 'stream';
  ui.state.resumeReconnect = false;

  // The view keeps the handle of its pending timer and skips the next batch
  // while the handle is set, so the handles are cleared together with the
  // timers the reset drops. A handle that outlives its timer would make the
  // view skip the batch, and the records that wait in the buffer would never
  // be rendered.
  ui.state.flushTimer = null;
  ui.state.renderTimer = null;
  ui.state.pollTimer = null;
  ui.state.reconnectTimer = null;
  ui.state.statusTimer = null;

  // The server of the previous scenario is reset, so the scenarios do not
  // share its records.
  server.head = 0;
  server.records = [];
  server.truncated = false;
  server.statusFails = false;
  server.version = 'test';
  server.logFile = '';
  server.runId = 'run-1';
  server.service = null;

  // The install-service action of the previous scenario is reset, so the
  // scenarios do not share its dispatches, and a pending answer of the
  // previous scenario can never resolve into the next one.
  installs.posts = 0;
  installs.mode = 'ok';
  installs.release = null;

  // The endpoint count of the previous scenario is reset with it.
  openLogsPosts.posts = 0;

  // The service control starts from the state the shipped markup carries,
  // so a scenario reads it from a known place: the wrapper is hidden, both
  // shapes are hidden, the menu is closed and every item waits.
  const control = nodes.get('#serviceControl');
  control.hidden = true;
  const trigger = nodes.get('#serviceTrigger');
  trigger.hidden = false;
  trigger.disabled = false;
  trigger.setAttribute('aria-expanded', 'false');
  nodes.get('#serviceMenu').hidden = true;
  nodes.get('#serviceMenu').style = {};
  nodes.get('#serviceMenu').offsetWidth = 190;
  nodes.get('#serviceTrigger').rect = { left: 352, right: 505, top: 14, bottom: 45, width: 153, height: 31 };
  window.innerWidth = 760;
  window.innerHeight = 800;
  nodes.get('#serviceChip').hidden = true;
  nodes.get('#serviceChip').title = '';
  nodes.get('#serviceLabel').textContent = 'Service —';
  nodes.get('#serviceChipLabel').textContent = 'Service —';
  for (const id of [
    'serviceDot', 'serviceChipDot'
  ]) {
    nodes.get('#' + id).classList.remove('is-success', 'is-warning', 'is-error', 'is-muted');
    nodes.get('#' + id).classList.add('is-muted');
  }
  for (const id of [
    'serviceInstallItem', 'serviceStartItem', 'serviceStopItem', 'serviceUpdateItem',
    'serviceDeleteItem', 'serviceSeparator'
  ]) {
    nodes.get('#' + id).hidden = true;
    nodes.get('#' + id).disabled = false;
  }
  document.activeElement = null;

  // No scenario inherits the window bindings or the calls of the previous
  // one.
  unbindServiceProviders();
  resetServiceCalls();

  nodes.get('#toastContainer').replaceChildren();
  nodes.get('#liveRegion').textContent = '';
  nodes.get('#logBody').replaceChildren();
  timers = [];
  calls.length = 0;
}

// seedClient gives the view the cursor of an earlier run of the server.
//
// The record is delivered the way the server delivers it, so the cursor of
// the view is a real cursor and not a number the harness wrote into the
// state of the view.
function seedClient(cursor) {
  ui.ingest([{
    seq: cursor,
    time: '2026-10-21T09:59:00Z',
    level: 'INFO',
    component: 'server',
    message: 'record of the earlier run',
    attrs: {}
  }]);
}

// runtimeUIRecord builds a log record for the scenarios.
//
// The record has the shape the server delivers, so the view takes it
// through the same path as a record of a running page.
function runtimeUIRecord(seq, message) {
  return {
    seq,
    time: '2026-10-21T10:00:00Z',
    level: 'INFO',
    component: 'server',
    message,
    attrs: {}
  };
}

// =========================================
// Scenarios
// =========================================

// A server that restarted reports another run. The view has to notice the
// restart, start over and tell the user about it.
//
// The run of the server is what tells a restart from a continued run: the
// sequence numbers of the log belong to the run of the process that emits
// them and start over with every run, so a view that keeps its cursor
// across a restart of the server waits for records that will never arrive.
async function scenarioRestart() {
  resetView();
  seedClient(fixture.clientCursor);

  // The view learns the run of the server. A run the view did not know yet
  // is not a restart.
  server.runId = 'run-before';
  server.head = fixture.continuedHead;
  server.records = [];
  server.truncated = false;

  await ui.refreshStatus();
  await settle();

  eq(ui.state.runId, 'run-before', 'the view knows the run of the server');
  eq(toastMessages().length, 0,
    'the view does not report a restart for the run it learns');

  // The server restarted: it reports another run.
  server.runId = 'run-after';
  server.head = fixture.restartHead;
  server.records = fixture.records;

  await ui.refreshStatus();
  await settle();

  eq(ui.state.cursor, fixture.restartHead,
    'the view takes the cursor of the new run of the server');
  eq(ui.state.records.length, fixture.records.length,
    'the view shows the records of the new run of the server');
  eq(ui.state.records[0].seq, fixture.records[0].seq,
    'the view starts with the oldest record of the new run');
  ok(toastMessages().some(message => /restart/i.test(message)),
    `the view tells the user that the server restarted (${JSON.stringify(toastMessages())})`);
  ok(/restart/i.test(announcement()),
    `the view announces the restart to a screen reader (${JSON.stringify(announcement())})`);

  // The follow mode and the filters survive the re-sync.
  eq(ui.state.follow, true, 'the follow mode survives the re-sync');
  eq(ui.state.level, 'all', 'the level filter survives the re-sync');

  // A restart is reported once: the view knows the run of the server now,
  // so the next status refresh does not re-sync again.
  await ui.refreshStatus();
  await settle();

  eq(toastMessages().length, 1,
    'the restart is reported once and not on every poll');
  eq(ui.state.cursor, fixture.restartHead,
    'the cursor of the view stays at the head of the server');
}

// A server that kept running has a head that is at or above the cursor of
// the client. The view keeps its cursor and does not re-sync.
async function scenarioContinued() {
  resetView();
  seedClient(fixture.clientCursor);

  server.head = fixture.continuedHead;
  server.records = [];
  server.truncated = false;

  await ui.refreshStatus();
  await settle();

  eq(ui.state.cursor, fixture.clientCursor,
    'the view keeps its cursor when the server kept running');
  eq(ui.state.records.length, 1,
    'the view keeps its records when the server kept running');
  eq(toastMessages().length, 0,
    'the view does not re-sync when the server kept running');
}

// A page that comes back truncated reports that the window of the cursor is
// not covered by the server anymore. The view has to re-sync instead of
// continuing a history with a hole in it.
async function scenarioTruncated() {
  resetView();
  seedClient(fixture.clientCursor);

  server.head = fixture.restartHead;
  server.records = fixture.records;
  server.truncated = true;

  await ui.pollOnce();
  await settle();

  eq(ui.state.cursor, fixture.restartHead,
    'the view takes the cursor of the server after a truncated page');
  eq(ui.state.records.length, fixture.records.length,
    'the view shows the records of the server after a truncated page');
  ok(toastMessages().length > 0,
    'the view tells the user that it re-synced after a truncated page');
}

// A status that cannot be read keeps the previous head, and the previous
// head can be older than the cursor of the client. The view must not mistake
// that for a restart of the server: only a status that was really read says
// anything about the run of the server.
async function scenarioUnreachableStatus() {
  resetView();
  seedClient(fixture.clientCursor);

  // The status the view knows is older than its cursor: it looks like a
  // restart, but it was read before the records of the client arrived.
  ui.state.status = {
    version: 'test',
    status: 'ok',
    url: 'http://127.0.0.1:8080',
    address: '127.0.0.1:8080',
    activeJobs: { running: 0, queued: 0 },
    logCursor: fixture.clientCursor - 1,
    logFile: ''
  };

  server.head = fixture.continuedHead;
  server.records = [];
  server.truncated = false;
  server.statusFails = true;

  await ui.refreshStatus();
  await settle();

  server.statusFails = false;

  eq(ui.state.cursor, fixture.clientCursor,
    'the view keeps its cursor when the status cannot be read');
  eq(ui.state.records.length, 1,
    'the view keeps its records when the status cannot be read');
  eq(toastMessages().length, 0,
    'the view does not re-sync when the status cannot be read');
}

// A cursor of an earlier run of the server is ahead of every record the
// server holds. The server reports the loss, and the view has to name the
// restart: the run of the server is another one.
async function scenarioEarlierRun() {
  resetView();
  seedClient(fixture.clientCursor);

  // The view knows the run of the server.
  server.runId = 'run-before';
  server.head = fixture.continuedHead;
  server.records = [];
  server.truncated = false;

  await ui.refreshStatus();
  await settle();

  // The server restarted: the cursor of the client belongs to the earlier
  // run, so the server cannot serve it.
  server.runId = 'run-after';
  server.head = fixture.restartHead;
  server.records = [];
  server.truncated = true;

  await ui.pollOnce();
  await settle();

  ok(toastMessages().some(message => /restart/i.test(message)),
    `the view tells the user that the server restarted (${JSON.stringify(toastMessages())})`);
}

// The head of the server is behind the cursor of the view, and the run of
// the server did not change: the server kept running.
//
// The head of the status is a snapshot that was taken before the request
// itself was written to the log, so it is always behind the records the
// client already received. The view must not mistake that for a restart of
// the server: a restart that is reported on every refresh is worse than
// the restart it was meant to catch.
async function scenarioRunUnchanged() {
  resetView();

  // The view knows the run of the server.
  server.runId = 'run-1';
  server.head = 6;
  server.records = [];

  await ui.refreshStatus();
  await settle();

  eq(ui.state.runId, 'run-1', 'the view knows the run of the server');

  // The live feed delivers the newest record before the next status reads
  // the head of the log.
  ui.ingest([runtimeUIRecord(7, 'the newest record')]);
  eq(ui.state.cursor, 7, 'the view received the newest record of the server');

  // The status is a snapshot from before that record, so its head is
  // behind the cursor of the view.
  server.head = 6;

  await ui.refreshStatus();
  await settle();

  eq(ui.state.cursor, 7,
    'the view keeps its cursor when the head of the server is behind it');
  eq(ui.state.records.length, 1,
    'the view keeps its records when the head of the server is behind it');
  eq(toastMessages().length, 0,
    'the view does not report a restart when the run of the server did not change');
  eq(countLogReads(), 0,
    'the view does not re-sync when the run of the server did not change');
}

// The server does not report a run id: the run of the server is unknown,
// so the view cannot tell a restart from a continued run and must not
// report one.
async function scenarioRunMissing() {
  resetView();

  // The view received the newest record of the server.
  ui.ingest([runtimeUIRecord(7, 'the newest record')]);
  eq(ui.state.cursor, 7, 'the view received the newest record of the server');

  // The server does not report a run id, and the head of its status is
  // behind the cursor of the view.
  server.runId = null;
  server.head = 6;
  server.records = [];

  await ui.refreshStatus();
  await settle();

  eq(ui.state.runId, '', 'the run of the server stays unknown');
  eq(ui.state.cursor, 7,
    'the view keeps its cursor when the run of the server is unknown');
  eq(ui.state.records.length, 1,
    'the view keeps its records when the run of the server is unknown');
  eq(toastMessages().length, 0,
    'the view does not report a restart when the run of the server is unknown');
}

// A burst of records that is larger than one batch leaves records in the
// buffer. The view has to render them anyway: a flush that leaves records
// unrendered schedules the next batch, so the view converges to showing
// everything that is in the buffer, without another event to arrive.
async function scenarioLargeBatch() {
  for (const count of [501, 1000]) {
    resetView();

    const records = [];
    for (let seq = 1; seq <= count; seq++) {
      records.push({
        seq,
        time: '2026-10-21T10:00:00Z',
        level: 'INFO',
        component: 'server',
        message: `record ${seq}`,
        attrs: {}
      });
    }

    // The whole burst arrives before the first flush.
    ui.ingest(records);

    eq(logRows().length, 0,
      `the records of a burst of ${count} are not rendered before the first flush`);

    // The view settles: the pending batches run.
    await runPendingTimers();

    const rows = logRows();
    eq(rows.length, count,
      `every record of a burst of ${count} is on the screen after the view settled`);
    eq(Number(rows[0]?.dataset.seq), 1,
      `the first row of a burst of ${count} is the first record`);
    eq(Number(rows[rows.length - 1]?.dataset.seq), count,
      `the last row of a burst of ${count} is the last record`);
  }
}

// A page that is hidden and shown again has to draw the records that arrive
// after it.
//
// The view cancels its timers when the page is hidden, and a browser that
// keeps the page in its back/forward cache shows it again later. The
// handles of the cancelled timers have to be released with them: a handle
// that outlives its timer makes the view skip its next batch, so the
// records that arrive after the page was shown again are never drawn.
//
// The record is delivered the way the stream delivers it, so the view takes
// it through the same path as a record of a running page.
async function scenarioHiddenPage() {
  resetView();

  // The head of the server is ahead of the records of the client, so the
  // status refresh does not mistake the round trip for a restart.
  server.head = 2;

  // The view has a pending batch when the page is hidden.
  ui.ingest([runtimeUIRecord(1, 'record before the page was hidden')]);
  ok(ui.state.flushTimer, 'the view has a pending batch before the page is hidden');
  eq(logRows().length, 0, 'the record waits for its batch');

  // The page is hidden: the stream is closed, the poll stops and the
  // pending timers are cancelled.
  dispatchWindow('pagehide', { persisted: true });
  eq(ui.state.source, null, 'the stream is closed while the page is hidden');
  eq(ui.state.pollTimer, null, 'the poll stops while the page is hidden');
  eq(timers.length, 0, 'the pending timers are cancelled while the page is hidden');

  // Every timer the view cancelled is released with it, so the view can
  // schedule its next batch.
  eq(ui.state.flushTimer, null, 'the handle of the cancelled batch is released');
  eq(ui.state.reconnectTimer, null, 'the handle of the cancelled reconnect is released');
  eq(ui.state.renderTimer, null, 'the handle of the cancelled re-render is released');
  eq(ui.state.statusTimer, null, 'the handle of the cancelled status refresh is released');

  // The browser shows the page again. The view resumes what it was doing,
  // so the status refresh runs again.
  dispatchWindow('pageshow', { persisted: true });
  await settle();
  ok(ui.state.statusTimer, 'the status refresh runs again after the page was shown again');

  // A record arrives after the page was shown again.
  ui.ingest([runtimeUIRecord(2, 'record after the page was shown again')]);
  await runPendingTimers();

  const rows = logRows();
  ok(rows.some(row => Number(row.dataset.seq) === 2),
    'the record that arrived after the page was shown again is drawn');
  eq(rows.length, 2, 'the record that waited in the buffer is drawn as well');
}

// A page that is hidden and shown again has to follow the event stream
// again.
//
// The browser puts a page into its back/forward cache and shows it again
// when the user comes back. The view closed its live source and cancelled
// its timers when the page was hidden, so it has to start the event stream
// again: exactly one stream, and the records that arrive after the restore
// have to be drawn.
//
// The record is delivered the way the stream delivers it, so the view takes
// it through the same path as a record of a running page.
async function scenarioHiddenPageStream() {
  resetView();

  // The browser shows a page that was not hidden, for example after the
  // first load. The view is already running then, so it does not resume.
  dispatchWindow('pageshow', { persisted: false });
  eq(streams.length, 0, 'the view does not resume a page that was not hidden');

  // The view follows the event stream.
  ui.connectStream();
  const stream = streams[streams.length - 1];
  stream.open();
  eq(ui.state.streamState, 'live', 'the view follows the event stream');
  eq(openStreams().length, 1, 'the view holds one stream');

  // A record arrives while the page is visible.
  stream.deliver(runtimeUIRecord(1, 'record before the page was hidden'));
  server.head = 1;
  await runPendingTimers();
  eq(logRows().length, 1, 'the record of the stream is drawn');

  // The page is hidden: the stream is closed and the pending timers are
  // cancelled.
  dispatchWindow('pagehide', { persisted: true });
  eq(ui.state.source, null, 'the stream is closed while the page is hidden');
  eq(openStreams().length, 0, 'no stream is open while the page is hidden');
  eq(ui.state.pollTimer, null, 'the poll stops while the page is hidden');
  eq(timers.length, 0, 'the pending timers are cancelled while the page is hidden');

  // The browser shows the page again.
  const statusReads = countStatusReads();
  server.version = 'restored';
  dispatchWindow('pageshow', { persisted: true });
  await settle();

  // The stream is open again, and it is the only live source of the view.
  eq(openStreams().length, 1, 'the stream is open again after the page was shown again');
  eq(streams.length, 2, 'the view opens the stream again instead of a second one');
  eq(ui.state.pollTimer, null, 'the view does not poll while the stream is open');

  // The status is refreshed and the refresh runs again.
  ok(countStatusReads() > statusReads, 'the status is refreshed after the page was shown again');
  eq(nodes.get('#versionLabel').textContent, 'Version restored',
    'the status of the view is the status of the server after the restore');
  ok(ui.state.statusTimer, 'the status refresh runs again after the page was shown again');

  // A record arrives after the page was shown again.
  const restored = streams[streams.length - 1];
  restored.open();
  restored.deliver(runtimeUIRecord(2, 'record after the page was shown again'));
  server.head = 2;
  await runPendingTimers();

  const rows = logRows();
  ok(rows.some(row => Number(row.dataset.seq) === 2),
    'the record that arrived after the page was shown again is drawn');
  eq(rows.length, 2, 'no record is drawn twice');
}

// A page that is hidden while the view falls back to polling has to poll
// again when the browser shows it.
//
// The view decides from its own state which live source it resumes: the
// event stream when it followed the stream, the fallback poll when it fell
// back to polling. The poll is the source that has to come back here, and
// the view must not open a stream and a poll at the same time.
async function scenarioHiddenPagePoll() {
  resetView();

  // The view follows the event stream first.
  ui.connectStream();
  const stream = streams[streams.length - 1];
  stream.open();
  eq(ui.state.streamState, 'live', 'the view follows the event stream');

  // The stream fails, so the view falls back to polling.
  stream.fail();
  await settle();
  eq(ui.state.source, null, 'the stream is closed when it fails');
  eq(ui.state.streamState, 'polling', 'the view falls back to polling');
  ok(ui.state.pollTimer, 'the poll of the fallback runs');
  ok(ui.state.reconnectTimer, 'the view tries the event stream again');

  // A record arrives while the page is visible.
  server.records = [runtimeUIRecord(1, 'record before the page was hidden')];
  server.head = 1;
  await runPoll();
  await runPendingTimers();
  eq(logRows().length, 1, 'the record of the poll is drawn');

  // The page is hidden: the poll stops and the pending timers are
  // cancelled.
  dispatchWindow('pagehide', { persisted: true });
  eq(ui.state.pollTimer, null, 'the poll stops while the page is hidden');
  eq(ui.state.reconnectTimer, null, 'the reconnect stops while the page is hidden');
  eq(openStreams().length, 0, 'no stream is open while the page is hidden');
  eq(timers.length, 0, 'the pending timers are cancelled while the page is hidden');

  // The browser shows the page again.
  const statusReads = countStatusReads();
  server.version = 'restored';
  dispatchWindow('pageshow', { persisted: true });
  await settle();

  // The poll comes back and the view does not open a stream.
  ok(ui.state.pollTimer, 'the poll runs again after the page was shown again');
  ok(ui.state.reconnectTimer, 'the view tries the event stream again after the page was shown again');
  eq(openStreams().length, 0, 'the view opens no stream while it polls');
  ok(countStatusReads() > statusReads, 'the status is refreshed after the page was shown again');
  eq(nodes.get('#versionLabel').textContent, 'Version restored',
    'the status of the view is the status of the server after the restore');

  // A record that arrives after the page was shown again is drawn.
  server.records = [
    runtimeUIRecord(1, 'record before the page was hidden'),
    runtimeUIRecord(2, 'record after the page was shown again')
  ];
  server.head = 2;
  await runPoll();
  await runPendingTimers();

  const rows = logRows();
  ok(rows.some(row => Number(row.dataset.seq) === 2),
    'the record that arrived after the page was shown again is drawn');
  eq(rows.length, 2, 'no record is drawn twice');

  // Exactly one poll loop is running: one tick reads the log once.
  const reads = await runPendingTimersOnce();
  eq(reads, 1, 'the view reads the log once per tick after the page was shown again');
}

// fireClick clicks a node the way the page does: it runs the handlers the
// view registered for the click event, with the clicked node as the target
// so the document-level handler of the service menu decides like a browser.
function fireClick(node) {
  for (const handler of node.handlers.click || []) handler({ target: node });
}

// fireKey presses a key on a node the way the page does.
function fireKey(node, key) {
  for (const handler of node.handlers.keydown || []) {
    handler({ key, target: node, preventDefault() {} });
  }
}

// dispatchDocumentClick hands a click to the document, the way a click
// outside the service control reaches it.
function dispatchDocumentClick(node) {
  for (const handler of document.handlers.click || []) handler({ target: node });
}

// serviceActions answers one action map of the runtime status. Every action
// starts hidden, so a scenario names exactly the actions its state carries.
function serviceActions(actions) {
  return Object.assign({
    install: { visible: false, enabled: false },
    start: { visible: false, enabled: false },
    stop: { visible: false, enabled: false },
    update: { visible: false, enabled: false },
    delete: { visible: false, enabled: false },
    open: { visible: false, enabled: false }
  }, actions);
}

// serviceSnapshot builds the classified service view internal/serviceview
// returns for one state: the line, the tone and the actions of that state.
function serviceSnapshot(label, tone, actions, className = 'present') {
  return { class: className, label, tone, actions: serviceActions(actions) };
}

// The calls of the window host. A scenario binds the five actions the host
// owns, counts the calls, and can hold or refuse the answer of one call.
const serviceCalls = {
  counts: {},
  pending: false,
  release: null,
  fail: false
};

const SERVICE_PROVIDERS = ['Start', 'Stop', 'Update', 'Delete', 'Install'];

// bindServiceProviders binds the five service actions the window host binds
// in the Logs window.
function bindServiceProviders() {
  resetServiceCalls();
  for (const name of SERVICE_PROVIDERS) {
    const key = name.toLowerCase();
    serviceCalls.counts[key] = 0;
    window['hfdesk' + name + 'Service'] = async () => {
      serviceCalls.counts[key] += 1;
      if (serviceCalls.pending) {
        await new Promise((resolve) => { serviceCalls.release = resolve; });
      }
      if (serviceCalls.fail) throw new Error('the host refused the action');
    };
  }
}

// unbindServiceProviders removes the bindings again, so no scenario inherits
// the window state of another one.
function unbindServiceProviders() {
  for (const name of SERVICE_PROVIDERS) delete window['hfdesk' + name + 'Service'];
}

function resetServiceCalls() {
  serviceCalls.counts = {};
  serviceCalls.pending = false;
  serviceCalls.release = null;
  serviceCalls.fail = false;
}

function serviceCallsTotal() {
  return Object.values(serviceCalls.counts).reduce((total, count) => total + count, 0);
}

const SERVICE_ITEM_IDS = {
  install: 'serviceInstallItem',
  start: 'serviceStartItem',
  stop: 'serviceStopItem',
  update: 'serviceUpdateItem',
  delete: 'serviceDeleteItem'
};

// serviceItemState answers one word per menu item: "hidden" when the markup
// keeps it out of the menu, "disabled" when it is shown but cannot act, and
// "enabled" when it can.
function serviceItemState() {
  const state = {};
  for (const [key, id] of Object.entries(SERVICE_ITEM_IDS)) {
    const node = nodes.get('#' + id);
    state[key] = node.hidden ? 'hidden' : (node.disabled ? 'disabled' : 'enabled');
  }
  return state;
}

// The service control of the header renders the classified view the server
// sends — the line, the tone and every action — and adds no rule about a
// state of its own. A state that proves nothing has no control at all.
async function scenarioServiceMenuFollowsActions() {
  resetView();
  bindServiceProviders();

  const cases = [
    {
      name: 'a machine without a registration',
      service: serviceSnapshot('Service: not installed', 'muted', {
        install: { visible: true, enabled: true }
      }),
      control: true,
      items: { install: 'enabled', start: 'hidden', stop: 'hidden', update: 'hidden', delete: 'hidden' }
    },
    {
      name: 'a running service',
      service: serviceSnapshot('Service: running', 'success', {
        start: { visible: true, enabled: false },
        stop: { visible: true, enabled: true },
        update: { visible: true, enabled: true },
        delete: { visible: true, enabled: true },
        open: { visible: true, enabled: true }
      }),
      control: true,
      items: { install: 'hidden', start: 'disabled', stop: 'enabled', update: 'enabled', delete: 'enabled' }
    },
    {
      name: 'a stopped service',
      service: serviceSnapshot('Service: stopped', 'muted', {
        start: { visible: true, enabled: true },
        stop: { visible: true, enabled: false },
        update: { visible: true, enabled: true },
        delete: { visible: true, enabled: true }
      }),
      control: true,
      items: { install: 'hidden', start: 'enabled', stop: 'disabled', update: 'enabled', delete: 'enabled' }
    },
    {
      // A broken registration cannot start: its program is gone, and the
      // update is the action that repairs it.
      name: 'a broken registration',
      service: serviceSnapshot('Service: failed', 'error', {
        start: { visible: false, enabled: false },
        stop: { visible: true, enabled: false },
        update: { visible: true, enabled: true },
        delete: { visible: true, enabled: true }
      }),
      control: true,
      items: { install: 'hidden', start: 'hidden', stop: 'disabled', update: 'enabled', delete: 'enabled' }
    },
    {
      name: 'a service that is starting',
      service: serviceSnapshot('Service: starting', 'warning', {
        start: { visible: true, enabled: false },
        stop: { visible: true, enabled: false },
        update: { visible: true, enabled: false },
        delete: { visible: true, enabled: false }
      }),
      control: true,
      items: { install: 'hidden', start: 'disabled', stop: 'disabled', update: 'disabled', delete: 'disabled' }
    },
    {
      name: 'a service that is stopping',
      service: serviceSnapshot('Service: stopping', 'warning', {
        start: { visible: true, enabled: false },
        stop: { visible: true, enabled: false },
        update: { visible: true, enabled: false },
        delete: { visible: true, enabled: false }
      }),
      control: true,
      items: { install: 'hidden', start: 'disabled', stop: 'disabled', update: 'disabled', delete: 'disabled' }
    },
    {
      // A read that proved nothing names no line and carries no action.
      name: 'a state the read could not settle',
      service: serviceSnapshot('', '', {}, 'unknown'),
      control: false
    },
    {
      name: 'a state that names no line',
      service: serviceSnapshot('', '', {}),
      control: false
    }
  ];

  for (const tc of cases) {
    server.service = tc.service;
    await ui.refreshStatus();
    await settle();

    const control = nodes.get('#serviceControl');
    eq(control.hidden, !tc.control,
      `the control of ${tc.name} is ${tc.control ? 'on screen' : 'gone'}`);

    if (!tc.control) {
      eq(nodes.get('#serviceMenu').hidden, true, `the menu of ${tc.name} is not drawn`);
      eq(nodes.get('#serviceTrigger').getAttribute('aria-expanded'), 'false',
        `the trigger of ${tc.name} claims no open menu`);
      continue;
    }

    eq(nodes.get('#serviceTrigger').hidden, false,
      `the trigger of ${tc.name} is the visible control`);
    eq(nodes.get('#serviceChip').hidden, true,
      `the chip of ${tc.name} is not drawn beside a menu`);
    eq(nodes.get('#serviceLabel').textContent, tc.service.label,
      `the trigger of ${tc.name} carries the line of the server`);
    ok(nodes.get('#serviceDot').classList.contains(`is-${tc.service.tone}`),
      `the dot of ${tc.name} carries the ${tc.service.tone} tone`);
    deepEq(serviceItemState(), tc.items,
      `the items of ${tc.name} follow the actions of the server`);

    const othersShown = Object.entries(tc.items)
      .some(([key, value]) => key !== 'delete' && value !== 'hidden');
    eq(nodes.get('#serviceSeparator').hidden, !(tc.items.delete !== 'hidden' && othersShown),
      `the separator of ${tc.name} stands only before a visible Delete`);
  }

  unbindServiceProviders();
}

// A document that owns none of the service actions — the plain browser on
// the page of a service host — shows the same line as a static chip: a fact
// about the machine, not a menu.
async function scenarioServiceMenuOwnerless() {
  resetView();

  server.service = serviceSnapshot('Service: running', 'success', {
    stop: { visible: true, enabled: true },
    update: { visible: true, enabled: true },
    delete: { visible: true, enabled: true }
  });

  await ui.refreshStatus();
  await settle();

  eq(nodes.get('#serviceControl').hidden, false,
    'the control stays as a fact about the machine');
  eq(nodes.get('#serviceTrigger').hidden, true,
    'a document without an owner draws no menu button');
  eq(nodes.get('#serviceChip').hidden, false, 'the chip is the whole control');
  eq(nodes.get('#serviceChipLabel').textContent, 'Service: running',
    'the chip carries the same line as the menu');
  eq(nodes.get('#serviceMenu').hidden, true, 'no menu is drawn');
  ok(nodes.get('#serviceChip').title.includes('HFDesk application'),
    `the chip says where the actions run (${nodes.get('#serviceChip').title})`);

  // The chip is a fact, not a control: a click opens nothing.
  fireClick(nodes.get('#serviceChip'));
  await settle();
  eq(nodes.get('#serviceMenu').hidden, true, 'the chip opens no menu');
  eq(nodes.get('#serviceTrigger').getAttribute('aria-expanded'), 'false',
    'no menu claims to be open');
}

// One activation reaches one owner exactly once. The menu closes with it,
// the trigger waits for the answer, and the status is read again right away;
// the page adds no success report beside the host's own.
async function scenarioServiceMenuAction() {
  resetView();
  bindServiceProviders();

  server.service = serviceSnapshot('Service: running', 'success', {
    start: { visible: true, enabled: false },
    stop: { visible: true, enabled: true },
    update: { visible: true, enabled: true },
    delete: { visible: true, enabled: true }
  });

  await ui.refreshStatus();
  await settle();

  const trigger = nodes.get('#serviceTrigger');
  const start = nodes.get('#serviceStartItem');
  const stop = nodes.get('#serviceStopItem');

  fireClick(trigger);
  await settle();
  eq(nodes.get('#serviceMenu').hidden, false, 'the trigger opens the menu');
  eq(trigger.getAttribute('aria-expanded'), 'true', 'the open menu says so');
  eq(start.hidden, false, 'the start item of a running service is shown greyed');
  eq(start.disabled, true, 'the start item of a running service cannot act');

  // A disabled item never reaches the host.
  fireClick(start);
  await settle();
  eq(serviceCallsTotal(), 0, 'a disabled item calls nobody');

  // One activation reaches the host once, even when the item is clicked
  // again while the call is in flight.
  serviceCalls.pending = true;
  const statusReads = countStatusReads();
  fireClick(stop);
  eq(serviceCalls.counts.stop, 1, 'the stop item called the host once');
  eq(nodes.get('#serviceMenu').hidden, true, 'the menu closes with the activation');
  eq(trigger.getAttribute('aria-expanded'), 'false', 'the closed menu says so');
  eq(trigger.disabled, true, 'the trigger waits while the action is in flight');

  fireClick(stop);
  fireClick(stop);
  eq(serviceCalls.counts.stop, 1, 'the clicks beside the first never reach the host');

  serviceCalls.release();
  await settle();
  eq(trigger.disabled, false, 'the trigger comes back when the action settled');
  ok(countStatusReads() > statusReads, 'the status is read again right away');
  eq(toastMessages().length, 0, "the page adds no success report beside the host's own");

  // A refused host action is not reported as a success either.
  server.service = serviceSnapshot('Service: stopped', 'muted', {
    start: { visible: true, enabled: true },
    stop: { visible: true, enabled: false },
    update: { visible: true, enabled: true },
    delete: { visible: true, enabled: true }
  });
  await ui.refreshStatus();
  await settle();

  serviceCalls.pending = false;
  serviceCalls.fail = true;
  fireClick(trigger);
  fireClick(nodes.get('#serviceStartItem'));
  await settle();
  eq(serviceCalls.counts.start, 1, 'the start item called the host once');
  eq(toastMessages().length, 0, 'a refused host action shows no report of the page');
  eq(trigger.disabled, false, 'the trigger comes back after a refused action');

  unbindServiceProviders();
}

// The menu is a menu for the keyboard as well: it opens from the trigger,
// moves over the items the user can act on, closes on Escape and returns
// the focus, and a click outside closes it.
async function scenarioServiceMenuKeyboard() {
  resetView();
  bindServiceProviders();

  server.service = serviceSnapshot('Service: running', 'success', {
    start: { visible: true, enabled: false },
    stop: { visible: true, enabled: true },
    update: { visible: true, enabled: true },
    delete: { visible: true, enabled: true }
  });

  await ui.refreshStatus();
  await settle();

  const trigger = nodes.get('#serviceTrigger');
  const menu = nodes.get('#serviceMenu');
  const stop = nodes.get('#serviceStopItem');
  const update = nodes.get('#serviceUpdateItem');
  const del = nodes.get('#serviceDeleteItem');

  // The start of this state is greyed, so the focus lands on the first item
  // the user can act on.
  fireKey(trigger, 'ArrowDown');
  await settle();
  eq(menu.hidden, false, 'ArrowDown opens the menu');
  eq(trigger.getAttribute('aria-expanded'), 'true', 'the open menu says so');
  eq(document.activeElement, stop, 'ArrowDown focuses the first enabled item');

  fireKey(menu, 'ArrowDown');
  eq(document.activeElement, update, 'ArrowDown moves to the next enabled item');

  fireKey(menu, 'ArrowUp');
  eq(document.activeElement, stop, 'ArrowUp moves back');

  fireKey(menu, 'End');
  eq(document.activeElement, del, 'End jumps to the last enabled item');

  fireKey(menu, 'Home');
  eq(document.activeElement, stop, 'Home jumps to the first enabled item');

  fireKey(menu, 'Escape');
  eq(menu.hidden, true, 'Escape closes the menu');
  eq(trigger.getAttribute('aria-expanded'), 'false', 'the closed menu says so');
  eq(document.activeElement, trigger, 'Escape returns the focus to the trigger');

  fireKey(trigger, 'ArrowUp');
  eq(menu.hidden, false, 'ArrowUp opens the menu');
  eq(document.activeElement, del, 'ArrowUp opens the menu on the last enabled item');

  // The open menu is anchored at the trigger, inside the viewport: the
  // narrow window and the hidden overflow of the shell never cut it off.
  eq(menu.style.left, '352px', 'the menu starts at the trigger');
  eq(menu.style.top, '51px', 'the menu hangs below the trigger');

  window.innerWidth = 360;
  trigger.rect = { left: 300, right: 453, top: 14, bottom: 45, width: 153, height: 31 };
  fireKey(menu, 'Escape');
  fireKey(trigger, 'ArrowDown');
  eq(menu.style.left, '162px', 'a narrow viewport clamps the menu inside itself');

  trigger.rect = { left: 14, right: 167, top: 60, bottom: 91, width: 153, height: 31 };
  fireKey(menu, 'Escape');
  fireKey(trigger, 'ArrowDown');
  eq(menu.style.left, '14px', 'the menu of a wrapped trigger stays inside the narrow viewport');
  eq(menu.style.top, '97px', 'the menu follows the wrapped trigger');

  dispatchDocumentClick(nodes.get('#openUiButton'));
  eq(menu.hidden, true, 'a click outside closes the menu');

  // A click inside the control does not close the menu before its own
  // handler runs.
  fireClick(trigger);
  eq(menu.hidden, false, 'the trigger opens the menu again');
  dispatchDocumentClick(menu);
  eq(menu.hidden, false, 'a click inside the control keeps the menu open');
  fireClick(trigger);
  eq(menu.hidden, true, 'the trigger closes the menu again');

  // The menu is anchored to the trigger, so a viewport that changes size
  // closes it instead of leaving it beside an element that moved.
  fireClick(trigger);
  eq(menu.hidden, false, 'the trigger opens the menu again');
  dispatchWindow('resize');
  eq(menu.hidden, true, 'a resized viewport closes the open menu');

  unbindServiceProviders();
}

// The installation is the one service action the server performs itself, so
// a browser on the page of a desktop host performs it over the endpoint —
// and a build without the endpoint withdraws the item instead of failing on
// every activation.
async function scenarioServiceInstallEndpoint() {
  resetView();

  server.service = serviceSnapshot('Service: not installed', 'muted', {
    install: { visible: true, enabled: true }
  });
  await ui.refreshStatus();
  await settle();

  const control = nodes.get('#serviceControl');
  const trigger = nodes.get('#serviceTrigger');
  const install = nodes.get('#serviceInstallItem');

  eq(control.hidden, false, 'the machine without a registration has a control');
  eq(trigger.hidden, false, 'the install action is a menu, not a chip');
  eq(nodes.get('#serviceChip').hidden, true, 'the chip is not drawn beside the menu');
  eq(install.hidden, false, 'the install item is offered');
  eq(install.disabled, false, 'the install item can act through the endpoint');

  fireClick(trigger);
  eq(nodes.get('#serviceMenu').hidden, false, 'the trigger opens the menu');
  eq(nodes.get('#serviceStartItem').hidden, true,
    'the menu carries no action the state does not have');

  installs.mode = 'pending';
  fireClick(install);
  eq(installs.posts, 1, 'the install item reached the endpoint once');
  eq(trigger.disabled, true, 'the trigger waits while the request is in flight');
  eq(nodes.get('#serviceMenu').hidden, true, 'the menu closes with the activation');

  fireClick(install);
  eq(installs.posts, 1, 'the clicks beside the first never reach the endpoint');

  installs.mode = 'ok';
  installs.release();
  await settle();
  ok(countStatusReads() >= 2, 'the status is read again right away');
  eq(toastMessages().length, 0, 'the page adds no success report');

  // The endpoint is not part of this build: the page says so once, and the
  // item is not offered again.
  installs.mode = 'missing';
  fireClick(trigger);
  fireClick(install);
  await settle();
  eq(installs.posts, 2, 'the click on the missing endpoint still reached the server once');
  ok(toastMessages().some(message => /not available in this build/.test(message)),
    `the page reports the missing endpoint (${JSON.stringify(toastMessages())})`);
  eq(control.hidden, true,
    'a state whose every action is withdrawn has no control');
  eq(nodes.get('#serviceMenu').hidden, true, 'no menu is left open');

  installs.mode = 'ok';
}

// A status that cannot be read proves nothing about the service either, so
// the control must not keep the claim of its last render.
async function scenarioServiceStatusOffline() {
  resetView();
  bindServiceProviders();

  server.service = serviceSnapshot('Service: running', 'success', {
    start: { visible: true, enabled: false },
    stop: { visible: true, enabled: true },
    update: { visible: true, enabled: true },
    delete: { visible: true, enabled: true }
  });
  await ui.refreshStatus();
  await settle();
  eq(nodes.get('#serviceControl').hidden, false,
    'the control reports the running service');

  // The server stops answering: the service view could not be read.
  server.statusFails = true;
  await ui.refreshStatus();
  await settle();

  eq(nodes.get('#statusLabel').textContent, 'Offline',
    'the connection dot reports the server that did not answer');
  eq(nodes.get('#serviceControl').hidden, true,
    'the service control disappears with the read that could not settle it');

  // A build that reports no service view at all is read the same way.
  server.statusFails = false;
  server.service = null;
  await ui.refreshStatus();
  await settle();
  eq(nodes.get('#serviceControl').hidden, true,
    'a build that reports no service view has no control');

  unbindServiceProviders();
}

// The renderer of one record is a unit of its own: it loads without
// touching the page, exposes exactly one global, and builds the row, the
// filter and the copy line the view has always used.
async function scenarioLogViewRenderer() {
  resetView();

  const logView = window.HFDeskLogView;
  ok(logView, 'logview.js exposes one renderer object');
  for (const name of [
    'normalizeRecord', 'buildRow', 'buildRawRow', 'matches', 'levelPasses', 'isFilter',
    'formatTime', 'formatAttrValue', 'attrEntries', 'formatLine'
  ]) {
    eq(typeof logView[name], 'function', `the renderer carries ${name}`);
  }

  const record = logView.normalizeRecord({
    seq: 7,
    time: '2026-10-21T10:00:00Z',
    level: 'warn',
    component: 'jobs',
    message: 'Slow queue',
    attrs: { queued: 2, note: 'x' }
  });
  eq(record.seq, 7, 'the renderer keeps the sequence number');
  eq(record.level, 'WARN', 'the renderer folds the level');
  eq(record.attrsText, 'queued=2 note=x', 'the renderer renders the attributes once');
  ok(record.searchable.includes('slow queue'), 'the searchable text carries the message');
  eq(logView.normalizeRecord({ seq: 'no number' }), null,
    'a record without a sequence number is not a record');

  // The filter is a minimum severity, and the search reads the message, the
  // component and the attributes.
  eq(logView.matches(record, { level: 'warn', searchLower: '' }), true,
    'a warn record passes a warn filter');
  eq(logView.matches(record, { level: 'error', searchLower: '' }), false,
    'a warn record does not pass an error filter');
  eq(logView.matches(record, { level: 'all', searchLower: 'jobs' }), true,
    'the search reads the component');
  eq(logView.matches(record, { level: 'all', searchLower: 'queued=2' }), true,
    'the search reads the attributes');
  eq(logView.matches(record, { level: 'all', searchLower: 'absent' }), false,
    'the search misses what is not there');
  eq(logView.isFilter('warn'), true, 'the renderer knows the level filters');
  eq(logView.isFilter('installed'), false, 'the renderer rejects a word that is no filter');

  // The row is the row of the log view.
  const row = logView.buildRow(record);
  eq(row.className, 'runtime-log-row', 'the renderer builds the row of the log');
  eq(row.dataset.seq, '7', 'the row carries its sequence number');
  eq(row.children.length, 5, 'the row carries the five cells of the view');

  // The copy line is the line of the view.
  eq(logView.formatLine(record),
    `${logView.formatTime(record.time)}  WARN   jobs        Slow queue  queued=2 note=x`,
    'the copy line is the line of the view');

  // A line the service wrote that is not a record is one text cell: the
  // markup of the line stays text.
  const rawRow = logView.buildRawRow('plain <b>line</b>');
  eq(rawRow.className, 'runtime-log-row runtime-log-row-text',
    'the raw line keeps the row of the log');
  eq(rawRow.children.length, 1, 'the raw line is one cell');
  eq(rawRow.children[0].textContent, 'plain <b>line</b>',
    'the raw line reaches the cell as text');

  // The renderer loads on its own: one global, and no page code runs.
  const fresh = { window: {} };
  vm.runInNewContext(logviewSource, fresh);
  eq(Object.keys(fresh.window).join(','), 'HFDeskLogView',
    'loading the renderer adds exactly one global');
}

// The log open actions belong to the right owner in the right context:
//
//   - in a PLAIN BROWSER the folder action goes to the endpoint of the
//     server, and the file action has no owner — its button stays
//     disabled and names the window where it works, instead of failing
//     on every click;
//   - inside the Logs WINDOW the window host binds both functions, the
//     clicks reach the host (which runs in the user's session — a
//     service server runs in session 0 and could not open a folder or a
//     file on the user's desktop), and NO request goes to the server:
//     the endpoint must not be the owner of the click in the window.
//
// The host reports its own outcome natively, so the host path claims
// nothing in the page: no success toast appears beside it.
async function scenarioOpenLogActions() {
  resetView();
  server.logFile = '/logs/hfdesk.log';
  await ui.refreshStatus();
  await settle();

  const folder = nodes.get('#openLogsButton');
  const file = nodes.get('#openLogFileButton');

  // A plain browser: the folder acts through the endpoint, the file
  // button is disabled and points at the window.
  eq(folder.disabled, false, 'the folder button is enabled beside a log file');
  eq(file.disabled, true,
    'the file button is disabled outside the window: no host owns the action there');
  ok(file.title.includes('HFDesk window'),
    `the file button says where it works (${file.title})`);

  fireClick(folder);
  await settle();
  eq(openLogsPosts.posts, 1, 'the folder click in a browser reached the endpoint');
  ok(toastMessages().some(message => /log folder was opened/.test(message)),
    `the endpoint path reports the opened folder (${JSON.stringify(toastMessages())})`);

  // The window host binds both actions.
  let hostFolder = 0;
  let hostFile = 0;
  window.hfdeskOpenLogFolder = async () => { hostFolder += 1; };
  window.hfdeskOpenLogFile = async () => { hostFile += 1; };
  try {
    await ui.refreshStatus();
    await settle();
    eq(file.disabled, false,
      'the file button is enabled inside the window: the host owns the action');
    eq(file.title, 'Open the file /logs/hfdesk.log',
      'the file button names the file it opens inside the window');

    fireClick(file);
    await settle();
    eq(hostFile, 1, 'the file click reached the window host');
    eq(openLogsPosts.posts, 1, 'the file click sent no request to the server');

    fireClick(folder);
    await settle();
    eq(hostFolder, 1, 'the folder click reached the window host');
    eq(openLogsPosts.posts, 1,
      'the folder click sent no request to the server: the host owns it inside the window');
    eq(toastMessages().length, 1,
      'the host path claims nothing beside the host\u2019s own native report');

    // The open-UI action goes to the host as well: the browser belongs
    // to the user's session, and the endpoint of a service server could
    // not reach the desktop from session 0. The calls list records
    // every request the view made, so a fetch would show up there even
    // though the stub would reject it.
    let hostOpenUi = 0;
    window.hfdeskOpenUi = async () => { hostOpenUi += 1; };
    const openUiRequests = () => calls.filter(path => path === '/api/runtime/open-ui').length;
    const openUiBefore = openUiRequests();
    fireClick(nodes.get('#openUiButton'));
    await settle();
    eq(hostOpenUi, 1, 'the open-UI click reached the window host');
    eq(openUiRequests(), openUiBefore,
      'the open-UI click sent no request to the server: the host owns it inside the window');
    delete window.hfdeskOpenUi;
  } finally {
    delete window.hfdeskOpenLogFolder;
    delete window.hfdeskOpenLogFile;
  }

  // The bindings are gone again: the next status refresh decides the
  // buttons by the browser rule, so no scenario inherits a window state.
  await ui.refreshStatus();
  await settle();
  eq(file.disabled, true, 'the file button falls back to the browser rule without the host');
}

// The groups of the scenarios of the harness.
//
// The group is picked with the HFDESK_RUNTIME_SCENARIO environment
// variable, so every Go test runs the group it is about. An empty value
// runs every group in one process, so the reset between the groups is
// exercised as well.
const scenarioGroups = {
  restart: [
    scenarioRestart,
    scenarioContinued,
    scenarioTruncated,
    scenarioUnreachableStatus,
    scenarioEarlierRun
  ],
  run: [
    scenarioRunUnchanged,
    scenarioRunMissing
  ],
  flush: [
    scenarioLargeBatch
  ],
  hidden: [
    scenarioHiddenPage
  ],
  restore: [
    scenarioHiddenPageStream,
    scenarioHiddenPagePoll
  ],
  service: [
    scenarioServiceMenuFollowsActions
  ],
  serviceChip: [
    scenarioServiceMenuOwnerless
  ],
  serviceAction: [
    scenarioServiceMenuAction
  ],
  serviceKeys: [
    scenarioServiceMenuKeyboard
  ],
  serviceInstall: [
    scenarioServiceInstallEndpoint
  ],
  serviceOffline: [
    scenarioServiceStatusOffline
  ],
  logview: [
    scenarioLogViewRenderer
  ],
  open: [
    scenarioOpenLogActions
  ]
};

(async () => {
  const wanted = (process.env.HFDESK_RUNTIME_SCENARIO || '').trim();
  const groups = wanted ? wanted.split(',').map(name => name.trim()).filter(Boolean) : Object.keys(scenarioGroups);

  for (const group of groups) {
    const scenarios = scenarioGroups[group];
    if (!scenarios) {
      console.error(`runtime-ui: unknown scenario group ${group}`);
      process.exit(1);
    }
    for (const scenario of scenarios) await scenario();
  }

  console.log(`runtime-ui: ${assertions} assertions passed`);
})().catch(err => {
  console.error(err && err.stack ? err.stack : String(err));
  process.exit(1);
});
