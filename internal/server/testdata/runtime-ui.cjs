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
    classList: makeClassList(),
    handlers: {},
    children: [],
    parentNode: null,
    firstChild: null,
    innerHTML: '',
    scrollTop: 0,
    scrollHeight: 0,
    clientHeight: 0,
    _text: '',
    get textContent() { return this._text; },
    set textContent(value) {
      this._text = String(value);
      if (this._text === '') this.replaceChildren();
    },
    setAttribute() {},
    removeAttribute() {},
    getAttribute() { return null; },
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
    replaceChildren(...nodes) {
      this.children.forEach(child => { child.parentNode = null; });
      this.children = [];
      this.firstChild = null;
      nodes.forEach(node => this.appendChild(node));
    },
    remove() {
      if (this.parentNode) this.parentNode.removeChild(this);
    },
    focus() {},
    scrollTo() {}
  };
  return Object.assign(node, extra);
}

const nodes = new Map();
for (const id of [
  'statusDot', 'statusLabel', 'versionLabel', 'serverUrl', 'activeJobs',
  'queuedJobs', 'logFile', 'errorBanner', 'errorTitle', 'errorText',
  'openUiButton', 'openUiNote', 'openLogsButton', 'copyButton', 'clearButton',
  'levelFilter', 'searchInput', 'followToggle', 'streamDot', 'streamLabel',
  'recordCount', 'logView', 'logBody', 'emptyState', 'emptyTitle',
  'emptyText', 'liveRegion', 'toastContainer'
]) {
  nodes.set('#' + id, makeNode());
}

const document = {
  readyState: 'loading',
  body: makeNode(),
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
  // runId is the run of the server. The status reports it, the way the
  // server does. A null value means that the server does not report a run
  // id at all, the way an older build answers.
  runId: 'run-1'
};

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
        logFile: ''
      };
      // A server that does not report a run id leaves the field out, the
      // way an older build answers.
      if (server.runId !== null && server.runId !== undefined) {
        status.runId = server.runId;
      }
      return jsonResponse(status);
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
    wireEvents
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
  server.runId = 'run-1';

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
