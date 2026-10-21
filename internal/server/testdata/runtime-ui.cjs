// Run through TestRuntimeUIRestartResync: the real runtime.js view.
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
  statusFails: false
};

const calls = [];

function jsonResponse(body) {
  return {
    ok: true,
    status: 200,
    text: async () => JSON.stringify(body)
  };
}

const window = {
  location: { protocol: 'http:', host: 'hfdesk.test', search: '', hash: '', pathname: '/runtime.html' },
  addEventListener() {}
};

const context = {
  window,
  document,
  console,
  URL,
  URLSearchParams,
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
      return jsonResponse({
        version: 'test',
        status: 'ok',
        url: 'http://127.0.0.1:8080',
        address: '127.0.0.1:8080',
        activeJobs: { running: 0, queued: 0 },
        logCursor: server.head,
        logFile: ''
      });
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

// The hook only names the owners the view already had, so the harness also
// runs against a view that does not know about a restart of the server.
const hook = `
  window.test = {
    state,
    refreshStatus,
    pollOnce,
    resync,
    resyncAfterGap,
    readLogs,
    ingest
  };
`;
source = source.replace(marker, hook + '\n' + marker);
vm.runInNewContext(source, context);
const ui = window.test;

// =========================================
// Helpers
// =========================================

async function settle() {
  for (let i = 0; i < 8; i++) await new Promise(resolve => setImmediate(resolve));
}

// runPendingTimers runs the pending timers of the view, oldest first.
//
// The view batches its DOM updates with a timer, so a scenario that wants
// to see the settled view runs the timers instead of waiting for the wall
// clock. The timers run one after the other, because a flush that leaves
// records in the buffer schedules the next batch.
async function runPendingTimers(budget = 100) {
  for (let i = 0; i < budget; i++) {
    const timer = timers.shift();
    if (!timer) return;
    timer.fn();
    await settle();
  }
  throw new Error(`the view scheduled more than ${budget} timers`);
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

function resetView() {
  ui.state.status = null;
  ui.state.cursor = 0;
  ui.state.cursorValid = false;
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

// =========================================
// Scenarios
// =========================================

// A server that restarted emits fewer records than the client has seen: its
// head is lower than the cursor of the client. The view has to notice the
// restart, start over and tell the user about it.
async function scenarioRestart() {
  resetView();
  seedClient(fixture.clientCursor);

  server.head = fixture.restartHead;
  server.records = fixture.records;
  server.truncated = false;

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

  // A restart is reported once: the cursor of the view is the head of the
  // server now, so the next status refresh does not re-sync again.
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
// restart: the cursor of the client cannot be used for that, because a
// page without records keeps the cursor of the client.
async function scenarioEarlierRun() {
  resetView();
  seedClient(fixture.clientCursor);

  server.head = fixture.restartHead;
  server.records = [];
  server.truncated = true;

  await ui.pollOnce();
  await settle();

  ok(toastMessages().some(message => /restart/i.test(message)),
    `the view tells the user that the server restarted (${JSON.stringify(toastMessages())})`);
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

// The groups of the scenarios of the harness.
//
// The group is picked with the HFDESK_RUNTIME_SCENARIO environment
// variable, so every Go test runs the group it is about. An empty value
// runs every group.
const scenarioGroups = {
  restart: [
    scenarioRestart,
    scenarioContinued,
    scenarioTruncated,
    scenarioUnreachableStatus,
    scenarioEarlierRun
  ],
  flush: [
    scenarioLargeBatch
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
