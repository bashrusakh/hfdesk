// Run through TestDownloadPhaseUI: real app.js job-card phase rendering.
//
// The harness loads the shipped app.js in Node's VM and drives createJobCard /
// updateJobCard for the local phases the downloader reports. Node has no
// layout, so the assertions pin the rendered structure instead: the phase hint
// occupies the existing single-line stats row, replaces the speed/ETA pills,
// and never adds a visible pill that a transferring row does not already use.
// Row height is measured separately in a browser against the same CSS.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

let assertions = 0;
function eq(actual, expected, message) { assertions++; assert.equal(actual, expected, message); }
function ok(value, message) { assertions++; assert.ok(value, message); }

function makeNode() {
  const children = new Map();
  let text = '';
  let html = null;
  return {
    get textContent() { return text; },
    set textContent(value) { text = String(value); html = null; },
    get innerHTML() {
      if (html !== null) return html;
      return String(text).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
    },
    set innerHTML(value) { html = String(value); children.clear(); },
    className: '',
    title: '',
    style: { display: '' },
    dataset: {},
    classList: { add() {}, remove() {}, contains() { return false; }, toggle() {} },
    onclick: null,
    appendChild(child) { return child; },
    removeChild(child) { return child; },
    remove() {},
    addEventListener() {},
    querySelector(selector) {
      if (!children.has(selector)) children.set(selector, makeNode());
      return children.get(selector);
    },
    querySelectorAll() { return []; }
  };
}

const document = {
  readyState: 'loading',
  addEventListener() {},
  createElement() { return makeNode(); },
  querySelector() { return null; },
  querySelectorAll() { return []; },
  getElementById() { return null; }
};
const window = {
  location: { protocol: 'https:', host: 'hfdesk.test', search: '', hash: '', pathname: '/' },
  addEventListener() {}
};
const context = {
  window,
  document,
  console,
  URL,
  URLSearchParams,
  localStorage: { getItem() { return null; }, setItem() {}, removeItem() {} },
  CSS: { escape: value => String(value) },
  confirm: () => true,
  setTimeout: () => ({}),
  clearTimeout: () => {},
  fetch: async rawPath => { throw new Error('unexpected fetch: ' + rawPath); }
};

let source = fs.readFileSync(
  path.join(__dirname, '..', '..', 'assets', 'static', 'js', 'app.js'),
  'utf8'
);
const marker = '  // Start\n';
if (!source.includes(marker)) {
  console.error('download-phase-ui: app.js injection marker is missing');
  process.exit(1);
}
source = source.replace(marker, `
  window.test = { createJobCard, updateJobCard, formatBytes };
` + '\n' + marker);
vm.runInNewContext(source, context);
const ui = window.test;

function render(job) {
  const card = ui.createJobCard(job);
  ui.updateJobCard(card, job);
  return card;
}

function role(card, name) {
  return card.querySelector('[data-role="' + name + '"]');
}

function visiblePills(card) {
  return ['pct', 'phase', 'speed', 'eta', 'bytes', 'files']
    .filter(name => role(card, name).style.display !== 'none').length;
}

const TOTAL = 20 * 1024 ** 3;
function job(overrides) {
  return {
    id: 'job-phase',
    repo: 'owner/model',
    revision: 'main',
    status: 'running',
    progress: {
      totalFiles: 2,
      completedFiles: 0,
      totalBytes: TOTAL,
      downloadedBytes: 0,
      bytesPerSecond: 0,
      etaSeconds: 0
    },
    ...overrides
  };
}

// The template always carries the phase slot once; only its text/display
// changes per update, so progress ticks cannot add or recreate stats nodes.
const template = ui.createJobCard(job({})).innerHTML;
ok(template.includes('data-role="phase"'),
  'the real job-card template must pre-contain the phase node');
ok(template.includes('data-role="status" role="status"'),
  'the existing status element is a live region for phase changes');
const statsMarkup = /<div class="job-stats">([\s\S]*?)<\/div>/.exec(template)?.[1] || '';
eq((statsMarkup.match(/<span /g) || []).length, 6,
  'the stats row must keep exactly six spans: pct, phase, speed, eta, bytes, files');
eq(/data-role="phase"[^>]*style="display: none"/.test(statsMarkup), true,
  'the phase slot starts hidden so a plain card renders as before');

// 1. preparing: local staging write/preallocation before any transfer. The
// badge names the phase, the hint uses the job total, and no speed or ETA is
// invented while the server reports 0 B/s and no estimate.
const preparing = job({ phase: 'preparing' });
const prepCard = render(preparing);
eq(role(prepCard, 'status').textContent, 'writing');
eq(role(prepCard, 'status').className, 'job-status preparing');
eq(role(prepCard, 'phase').textContent, 'writing 20 GB to disk');
eq(role(prepCard, 'phase').title, 'writing 20 GB to disk');
eq(role(prepCard, 'phase').style.display, '');
eq(role(prepCard, 'speed').style.display, 'none');
eq(role(prepCard, 'eta').style.display, 'none');
eq(role(prepCard, 'pct').textContent, '0.0%', 'the percentage stays truthful at 0%');
eq(role(prepCard, 'fill').style.width, '0%', 'the bar keeps its real value');
eq(role(prepCard, 'bytes').textContent, '0 B / 20 GB');
eq(visiblePills(prepCard), 4, 'preparing shows pct, phase, bytes and files');

// 2. finalizing: the transfer is complete and local verification/publish runs.
// The same slot carries the phase with the total size; the percentage is the
// real 100%, not a phase placeholder.
const finalizing = job({
  phase: 'finalizing',
  progress: { ...preparing.progress, downloadedBytes: TOTAL, completedFiles: 1 }
});
const finCard = render(finalizing);
eq(role(finCard, 'status').textContent, 'finalizing');
eq(role(finCard, 'status').className, 'job-status finalizing');
eq(role(finCard, 'phase').textContent, 'finalizing 20 GB');
eq(role(finCard, 'phase').style.display, '');
eq(role(finCard, 'speed').style.display, 'none');
eq(role(finCard, 'eta').style.display, 'none');
eq(role(finCard, 'pct').textContent, '100.0%');
eq(role(finCard, 'fill').style.width, '100%');

// 3. A normal transferring job is unchanged: no phase text, speed and ETA in
// their own pills, and the same badge category as before.
const running = job({
  progress: {
    ...preparing.progress,
    downloadedBytes: TOTAL / 2,
    bytesPerSecond: 12.5 * 1024 * 1024,
    etaSeconds: 300
  }
});
const runCard = render(running);
eq(role(runCard, 'status').textContent, 'running');
eq(role(runCard, 'status').className, 'job-status running');
eq(role(runCard, 'phase').style.display, 'none');
eq(role(runCard, 'phase').textContent, '');
eq(role(runCard, 'speed').style.display, '');
eq(role(runCard, 'speed').textContent, '12.5 MB/s');
eq(role(runCard, 'eta').style.display, '');
eq(role(runCard, 'eta').textContent, 'ETA 5m');
eq(role(runCard, 'pct').textContent, '50.0%');

// 3. writing: the disk is the bottleneck for one file. The badge names the
// phase, the hint sizes only the file(s) currently writing (not the whole
// job), speed/ETA stay hidden, and the file line names that same file even
// though a later file is still pending.
const writingJob = job({
  phase: 'writing',
  files: [
    { path: 'writing.gguf', totalBytes: 4 * 1024 ** 3, downloaded: 1024 ** 3, status: 'writing' },
    { path: 'waiting.gguf', totalBytes: 6 * 1024 ** 3, downloaded: 0, status: 'pending' }
  ],
  progress: {
    ...preparing.progress,
    downloadedBytes: 1024 ** 3,
    bytesPerSecond: 2 * 1024 * 1024,
    etaSeconds: 900
  }
});
const writeCard = render(writingJob);
eq(role(writeCard, 'status').textContent, 'writing');
eq(role(writeCard, 'status').className, 'job-status writing');
eq(role(writeCard, 'phase').textContent, 'writing 4 GB to disk',
  'the writing hint sizes the writing file, not the job total');
eq(role(writeCard, 'phase').title, 'writing 4 GB to disk');
eq(role(writeCard, 'phase').style.display, '');
eq(role(writeCard, 'speed').style.display, 'none');
eq(role(writeCard, 'eta').style.display, 'none');
eq(role(writeCard, 'pct').textContent, '5.0%', 'the percentage stays truthful while writing');
eq(role(writeCard, 'bytes').textContent, '1 GB / 20 GB');
eq(role(writeCard, 'active-file').textContent, 'writing: writing.gguf');
eq(role(writeCard, 'active-file').style.display, '');

// 4. A mixed job (one file writing, another still pulling from the network)
// keeps the normal running row and aggregate speed; only the file line names
// the writing file.
const mixedCard = render(job({
  files: [
    { path: 'writing.gguf', totalBytes: 3 * 1024 ** 3, downloaded: 1024 ** 3, status: 'writing' },
    { path: 'active.gguf', totalBytes: 8 * 1024 ** 3, downloaded: 2 * 1024 ** 3, status: 'active' }
  ],
  progress: running.progress
}));
eq(role(mixedCard, 'status').textContent, 'running');
eq(role(mixedCard, 'status').className, 'job-status running');
eq(role(mixedCard, 'phase').style.display, 'none');
eq(role(mixedCard, 'speed').style.display, '');
eq(role(mixedCard, 'speed').textContent, '12.5 MB/s');
eq(role(mixedCard, 'active-file').textContent, 'writing: writing.gguf');

// A local phase must never need more visible pills than the transferring row
// it replaces; the stats row keeps the same single-line flex layout.
ok(visiblePills(prepCard) <= visiblePills(runCard),
  'preparing must not add a visible stats pill over a transferring job');
ok(visiblePills(finCard) <= visiblePills(runCard),
  'finalizing must not add a visible stats pill over a transferring job');
ok(visiblePills(writeCard) <= visiblePills(runCard),
  'writing must not add a visible stats pill over a transferring job');
ok(!/[\r\n]/.test(role(writeCard, 'phase').textContent),
  'the writing hint must stay on one line');
ok(!/[\r\n]/.test(role(prepCard, 'phase').textContent),
  'the phase hint must stay on one line');
ok(!/[\r\n]/.test(role(finCard, 'phase').textContent),
  'the finalizing hint must stay on one line');

// 5. Completed jobs never show a phase, even if a stale phase value survived
// in the snapshot. Terminal state owns the row.
const completed = job({
  status: 'completed',
  phase: 'finalizing',
  progress: { ...preparing.progress, downloadedBytes: TOTAL, completedFiles: 2 }
});
const doneCard = render(completed);
eq(role(doneCard, 'status').textContent, 'completed');
eq(role(doneCard, 'status').className, 'job-status completed');
eq(role(doneCard, 'phase').style.display, 'none');
eq(role(doneCard, 'speed').style.display, 'none');
eq(role(doneCard, 'eta').style.display, 'none');

// 6. Unknown or absent phases degrade to the plain running row.
const unknownCard = render(job({ phase: 'verifying', progress: running.progress }));
eq(role(unknownCard, 'status').textContent, 'running');
eq(role(unknownCard, 'phase').style.display, 'none');
eq(role(unknownCard, 'speed').textContent, '12.5 MB/s');

// 7. Progress updates mutate the existing nodes; they never rebuild the card
// or the hint. updateJobCard must not assign the card innerHTML per tick.
const phaseRef = role(prepCard, 'phase');
const statusRef = role(prepCard, 'status');
ui.updateJobCard(prepCard, job({
  phase: 'preparing',
  progress: { ...preparing.progress, downloadedBytes: 1024 ** 3 }
}));
eq(role(prepCard, 'phase'), phaseRef, 'a progress tick must not recreate the phase node');
eq(role(prepCard, 'status'), statusRef, 'a progress tick must not recreate the status node');
eq(role(prepCard, 'phase').textContent, 'writing 20 GB to disk');
eq(role(prepCard, 'pct').textContent, '5.0%');

// Leaving the phase clears the hint and restores the speed pill in place.
ui.updateJobCard(prepCard, running);
eq(role(prepCard, 'phase'), phaseRef, 'leaving the phase keeps the same hint node');
eq(role(prepCard, 'status'), statusRef, 'leaving the phase keeps the same status node');
eq(role(prepCard, 'phase').style.display, 'none');
eq(role(prepCard, 'phase').textContent, '');
eq(role(prepCard, 'speed').style.display, '');
eq(role(prepCard, 'speed').textContent, '12.5 MB/s');

console.log(`PASS: ${assertions} download phase row assertions (preparing, writing, running, mixed, finalizing, completed, unknown, DOM stability)`);
