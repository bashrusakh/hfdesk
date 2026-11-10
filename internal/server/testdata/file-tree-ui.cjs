// Run through TestFileTreeUI: real app.js Files-tab file tree.
//
// The harness loads the shipped app.js inside Node's VM with DOM, localStorage
// and fetch stubs. It drives the production file-tree owner: build/reset from
// the analysis payload's flat file list, directory tri-state selection, the
// strict "Download selected" request body, existing-job message handling,
// per-file status mapping (cache presence for done, job selections for
// queued/running) and the coalesced live reconciliation.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

let assertions = 0;
function eq(actual, expected, message) { assertions++; assert.equal(actual, expected, message); }
function deepEq(actual, expected, message) { assertions++; assert.deepEqual(Array.from(actual), expected, message); }
function match(value, re, message) { assertions++; assert.match(String(value), re, message); }
function doesNotMatch(value, re, message) { assertions++; assert.doesNotMatch(String(value), re, message); }

const summaryNode = { textContent: '' };
const overlapNode = { textContent: '' };

// The panel stub parses the rendered markup into real checkbox elements, so
// the shipped syncFileTreeChecks mutates observable box state and the tests
// can assert checked/indeterminate against the selection model instead of
// only re-reading the model. The parse is cached until innerHTML or the rows
// subtree changes.
let panelHTML = '';
let panelBoxes = null;

// Counters the render-contract scenarios observe: rowsRewrites counts full
// rows-container rewrites (a targeted collapse must not cause one), and
// focusedToggle records the caret the production toggle last focused — a
// rows rebuild drops focus unless the handler hands it back.
let rowsRewrites = 0;
let focusedToggle = null;

function decodeAttr(value) {
  return value.replace(/&quot;/g, '"').replace(/&#39;/g, "'")
    .replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&amp;/g, '&');
}

function parseCheckBoxes(html) {
  const boxes = [];
  const re = /<input type="checkbox" class="file-tree-check" (data-dir|data-file)="([^"]*)"(\schecked)?\saria-label="[^"]*">/g;
  let m;
  while ((m = re.exec(html))) {
    const path = decodeAttr(m[2]);
    boxes.push({
      type: 'checkbox',
      classList: { contains: name => name === 'file-tree-check' },
      checked: !!m[3],
      indeterminate: false,
      dataset: m[1] === 'data-dir' ? { dir: path } : { file: path },
      setAttribute() {},
      removeAttribute() {}
    });
  }
  // Parse-drift guard: every rendered file-tree checkbox must materialize,
  // otherwise the DOM assertions below would silently observe nothing.
  const rendered = (html.match(/class="file-tree-check"/g) || []).length;
  if (rendered !== boxes.length) {
    throw new Error(`harness parsed ${boxes.length} of ${rendered} rendered checkboxes`);
  }
  return boxes;
}

// Locates a rendered data-children container (test paths are plain, so the
// raw attribute value matches) and spans it to its depth-matched </div>, so
// the production collapse handler can empty exactly that subtree in the panel
// markup, like a real DOM write.
function childrenStub(rawPath) {
  const idx = panelHTML.indexOf('data-children="' + rawPath + '"');
  if (idx === -1) return null;
  const openEnd = panelHTML.indexOf('>', idx);
  if (openEnd === -1) return null;
  let depth = 1;
  let pos = openEnd + 1;
  let closeIdx = -1;
  while (pos < panelHTML.length) {
    const open = panelHTML.indexOf('<div', pos);
    const close = panelHTML.indexOf('</div>', pos);
    if (close === -1) break;
    if (open !== -1 && open < close) { depth++; pos = open + 4; continue; }
    depth--;
    if (depth === 0) { closeIdx = close; break; }
    pos = close + 6;
  }
  if (closeIdx === -1) return null;
  return {
    get innerHTML() { return panelHTML.slice(openEnd + 1, closeIdx); },
    set innerHTML(value) {
      panelHTML = panelHTML.slice(0, openEnd + 1) + String(value) + panelHTML.slice(closeIdx);
      panelBoxes = null;
    }
  };
}

// The caret the production expand handler looks up after a rebuild: focus()
// records which caret holds keyboard focus so the scenarios can assert it.
function caretStub(rawPath) {
  const idx = panelHTML.indexOf('data-toggle="' + rawPath + '"');
  if (idx === -1) return null;
  const path = decodeAttr(rawPath);
  return {
    dataset: { toggle: path },
    textContent: '',
    setAttribute() {},
    focus() { focusedToggle = path; }
  };
}

const filesPanelNode = {
  dataset: {},
  handlers: {},
  addEventListener(type, fn) { (this.handlers[type] ||= []).push(fn); },
  setAttribute() {},
  removeAttribute() {},
  get innerHTML() { return panelHTML; },
  set innerHTML(value) { panelHTML = String(value); panelBoxes = null; focusedToggle = null; },
  querySelector(selector) {
    if (selector === '#fileTreeSummary') return summaryNode;
    if (selector === '#fileTreeOverlap') return overlapNode;
    if (selector.startsWith('[data-children="')) return childrenStub(selector.slice(16, -2));
    if (selector.startsWith('[data-toggle="')) return caretStub(selector.slice(14, -2));
    return null;
  },
  querySelectorAll(selector) {
    if (!panelBoxes) panelBoxes = parseCheckBoxes(panelHTML);
    if (selector === 'input.file-tree-check') return panelBoxes;
    if (selector === 'input.file-tree-check[data-dir]') {
      return panelBoxes.filter(box => box.dataset.dir !== undefined);
    }
    // Status chips stay asserted through the model and the markup string;
    // only checkbox state needs live element observability.
    return [];
  }
};

// The rows container lives inside the rendered panel: writing it rewrites the
// panel's row markup, exactly like innerHTML in the real DOM, so a re-render
// (Clear, filter keystroke) reparses the visible checkboxes. A rewrite also
// destroys the rendered carets, so keyboard focus on one is lost — matching
// the real DOM — until the production handler restores it.
const rowsNode = {
  set innerHTML(value) {
    rowsRewrites++;
    focusedToggle = null;
    const marker = panelHTML.indexOf('id="fileTreeRows"');
    const openEnd = marker === -1 ? -1 : panelHTML.indexOf('>', marker);
    const footer = openEnd === -1 ? -1 : panelHTML.indexOf('<div class="file-tree-footer">', openEnd);
    if (footer === -1) throw new Error('harness: rows container is not inside the rendered panel');
    panelHTML = panelHTML.slice(0, openEnd + 1) + String(value) + panelHTML.slice(footer);
    panelBoxes = null;
  },
  get innerHTML() { return panelHTML; }
};
const overviewPanelNode = {
  innerHTML: '',
  dataset: {},
  setAttribute() {},
  removeAttribute() {},
  querySelector() { return null; },
  querySelectorAll() { return []; },
  addEventListener() {}
};

const domNodes = new Map([
  ['#modelsFilesPanel', filesPanelNode],
  ['#modelsOverviewPanel', overviewPanelNode]
]);

const storage = new Map();
const localStorage = {
  getItem(key) { return storage.has(key) ? storage.get(key) : null; },
  setItem(key, value) { storage.set(key, String(value)); },
  removeItem(key) { storage.delete(key); }
};

let cacheState = { status: 200, files: [], error: null };
const cacheRequests = [];
const downloadRequests = [];
// Response factory for POST /api/download. The duplicate scenario swaps in
// the existing-job message shape documented in docs/API.md; a throwing factory
// simulates a transport failure of the start surface.
let downloadReply = null;
// When set, POST /api/diskfree rejects — the disk preview of the start surface
// is best-effort, so the strict request must still be posted.
let diskfreeError = null;

const document = {
  readyState: 'loading',
  body: { appendChild() {}, addEventListener() {} },
  handlers: {},
  addEventListener(type, fn) { (this.handlers[type] ||= []).push(fn); },
  createElement() {
    let html = '';
    let text = '';
    return {
      set textContent(value) { text = String(value); },
      get textContent() { return text; },
      set innerHTML(value) { html = String(value); },
      get innerHTML() { return html || text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'); }
    };
  },
  querySelector(selector) {
    // The rows container exists only while the panel markup carries it.
    if (selector === '#fileTreeRows') {
      return panelHTML.includes('id="fileTreeRows"') ? rowsNode : null;
    }
    return domNodes.get(selector) || null;
  },
  querySelectorAll() { return []; },
  getElementById() { return null; }
};

const window = {
  location: { protocol: 'https:', host: 'hfdesk.test', search: '', hash: '', pathname: '/' },
  addEventListener() {}
};

const timers = [];
function fakeSetTimeout(fn, ms) {
  const timer = { fn, ms, cleared: false };
  timers.push(timer);
  return timer;
}
function fakeClearTimeout(timer) {
  if (timer) timer.cleared = true;
}

const fetch = async (rawPath, options = {}) => {
  const requestPath = String(rawPath);
  if (requestPath.startsWith('/api/cache/')) {
    cacheRequests.push(requestPath);
    if (cacheState.error) throw cacheState.error;
    return {
      status: cacheState.status,
      ok: cacheState.status >= 200 && cacheState.status < 300,
      json: async () => ({ files: cacheState.files.map(name => ({ name })) })
    };
  }
  if (requestPath === '/api/diskfree') {
    if (diskfreeError) throw diskfreeError;
    return {
      status: 200,
      ok: true,
      json: async () => ({ free: 100 * 1024 * 1024 * 1024, path: '/cache/hub' })
    };
  }
  if (requestPath === '/api/download') {
    const body = options?.body ? JSON.parse(options.body) : null;
    downloadRequests.push(body);
    const reply = downloadReply ? downloadReply(body) : {
      id: `job-${downloadRequests.length}`,
      repo: body?.repo,
      revision: body?.revision,
      status: 'queued',
      filters: body?.filters || [],
      strictFiles: body?.strictFiles === true,
      excludes: []
    };
    return { status: 200, ok: true, json: async () => reply };
  }
  throw new Error('unexpected fetch: ' + rawPath);
};

const context = {
  window,
  document,
  console,
  URL,
  URLSearchParams,
  setTimeout: fakeSetTimeout,
  clearTimeout: fakeClearTimeout,
  localStorage,
  CSS: { escape: value => String(value) },
  confirm: () => true,
  fetch
};

let source = fs.readFileSync(
  path.join(__dirname, '..', '..', 'assets', 'static', 'js', 'app.js'),
  'utf8'
);
const marker = '  // Start\n';
if (!source.includes(marker)) {
  console.error('file-tree-ui: app.js injection marker is missing');
  process.exit(1);
}
const hook = `
  window.test = {
    state,
    fileTreeState,
    renderAnalysisResult,
    renderFilesTab,
    renderFileTreeRows,
    resetFileTree,
    fileTreeToggleDir,
    fileTreeToggleFile,
    fileTreeDirState,
    fileTreeSelectedPaths,
    fileTreeFileStates,
    updateFileTreeStatuses,
    downloadSelectedFiles: (...args) => window.downloadSelectedFiles(...args),
    syncLocalAnalysisStatus,
    handleWSMessage,
    getJobQuantLabel,
    getDetailActiveTab() { return detailActiveTab; },
    async flushLiveReconcile() {
      if (liveStatusReconcileTimer !== null) {
        clearTimeout(liveStatusReconcileTimer);
        liveStatusReconcileTimer = null;
      }
      await runLiveAnalysisReconcile();
    },
    resetLiveReconcile() {
      if (liveStatusReconcileTimer !== null) clearTimeout(liveStatusReconcileTimer);
      liveStatusReconcileTimer = null;
      liveStatusReconcilePending = false;
      liveStatusReconcileNeedsCache = false;
    },
    setAnalysis(value) { currentAnalysis = value; },
    getJobs() { return state.jobs; }
  };
  showToast = (message, type) => window.messages.push({ message, type: type || 'info' });
  navigateTo = () => {};
  // Start\n`;
source = source.replace(marker, hook + '\n' + marker);
vm.runInNewContext(source, context);
const ui = window.test;
window.messages = [];

// The cap and the auto-collapse threshold are read from the loaded app.js so
// this harness can never drift from the shipped constants; the fallbacks keep
// it runnable against a pre-fix app.js that defines neither (used to capture
// the unbounded-baseline failure evidence).
const capSource = /const FILE_TREE_RENDER_CAP = (\d+)/.exec(source);
const ROW_CAP = capSource ? Number(capSource[1]) : 300;
const collapseSource = /const FILE_TREE_AUTO_COLLAPSE_FILES = (\d+)/.exec(source);
const AUTO_COLLAPSE = collapseSource ? Number(collapseSource[1]) : 1000;

const countMatches = (html, re) => (html.match(re) || []).length;
// Row identities: a tree row (dir or file) vs the one "show more" row.
const rowPattern = /file-tree-row file-tree-(?:dir|file)/g;
const morePattern = /class="quant-row file-tree-more"/g;
function lte(actual, limit, message) {
  assertions++;
  assert.ok(actual <= limit, `${message} (got ${actual}, limit ${limit})`);
}

// Directory-shaped fixture: dirCount directories of filesPerDir files each,
// plus optional root-level files.
function makeDirTree(dirCount, filesPerDir, rootFiles = 0) {
  const files = [];
  for (let d = 0; d < dirCount; d++) {
    const dir = `dir${String(d).padStart(4, '0')}`;
    for (let f = 0; f < filesPerDir; f++) files.push(file(`${dir}/f${String(f).padStart(4, '0')}.bin`, 64));
  }
  for (let r = 0; r < rootFiles; r++) files.push(file(`root${String(r).padStart(4, '0')}.bin`, 64));
  return files;
}

// Simulates activating a rendered caret: the browser focuses the caret the
// user activated before the click reaches the panel handler.
function caretClick(path) {
  const caret = {
    dataset: { toggle: path },
    textContent: '',
    setAttribute() {},
    closest(selector) { return selector === '[data-toggle]' ? caret : null; }
  };
  focusedToggle = path;
  firePanel('click', caret);
  return caret;
}

const ROOT = 'tree-case';
let sequence = 0;

// Each scenario gets its own repository identity so resetFileTree starts from
// a clean selection, expansion and query — mirroring a fresh analysis render.
function setAnalysis(files, overrides = {}) {
  sequence++;
  const analysis = {
    repo: `${ROOT}/case${sequence}`,
    type: 'unknown',
    branch: 'main',
    is_dataset: false,
    file_count: files.length,
    total_size_human: '1 KB',
    files,
    selectable_items: [],
    ...overrides
  };
  ui.setAnalysis(analysis);
  ui.resetFileTree(analysis);
  return analysis;
}

function file(path, size, extra = {}) {
  return { path, name: path.split('/').pop(), size, ...extra };
}

function resetWorld() {
  ui.getJobs().clear();
  ui.state.settings = {};
  ui.resetLiveReconcile();
  cacheRequests.length = 0;
  downloadRequests.length = 0;
  downloadReply = null;
  diskfreeError = null;
  cacheState = { status: 200, files: [], error: null };
  window.messages.length = 0;
  filesPanelNode.innerHTML = '';
  summaryNode.textContent = '';
  overlapNode.textContent = '';
}

function statusesOf() {
  const map = {};
  ui.fileTreeFileStates().forEach((value, key) => { map[key] = value; });
  return map;
}

function domBox(kind, path) {
  const boxes = filesPanelNode.querySelectorAll('input.file-tree-check');
  const box = boxes.find(b => (kind === 'dir' ? b.dataset.dir : b.dataset.file) === path);
  if (!box) throw new Error(`no rendered ${kind} checkbox for ${path}`);
  return box;
}

// Dispatches an event to the panel listeners the production code registered
// through bindFileTreePanel.
function firePanel(type, target) {
  (filesPanelNode.handlers[type] || []).forEach(fn => fn({ type, target }));
}

// Simulates a user click on a checkbox: the browser flips the box's own
// visual state before the change event reaches the panel handler, and the
// handler then reconciles model and DOM exactly as in the shipped app.
function clickBox(kind, path) {
  const box = domBox(kind, path);
  box.indeterminate = false;
  box.checked = !box.checked;
  firePanel('change', box);
  return box;
}

function clickClear() { firePanel('click', { id: 'fileTreeClear' }); }
function typeFilter(value) { firePanel('input', { id: 'fileTreeSearch', value }); }

// Every rendered checkbox must equal what the selection model says, in both
// directions: file boxes mirror selected-set membership, directory boxes carry
// the tri-state (checked for complete, indeterminate for mixed).
function assertChecksMatchModel(label) {
  const boxes = filesPanelNode.querySelectorAll('input.file-tree-check');
  assertions++;
  assert.ok(boxes.length > 0, `${label}: expected rendered checkboxes`);
  for (const box of boxes) {
    if (box.dataset.dir !== undefined) {
      const state = ui.fileTreeDirState(box.dataset.dir);
      eq(box.checked, state === 'on',
        `${label}: dir box ${box.dataset.dir} checked=${box.checked} vs model ${state}`);
      eq(box.indeterminate, state === 'mixed',
        `${label}: dir box ${box.dataset.dir} indeterminate=${box.indeterminate} vs model ${state}`);
    } else {
      const want = ui.fileTreeState.selected.has(box.dataset.file);
      eq(box.checked, want,
        `${label}: file box ${box.dataset.file} checked=${box.checked} vs model selected=${want}`);
      eq(box.indeterminate, false,
        `${label}: file box ${box.dataset.file} must never be indeterminate`);
    }
  }
}

const scenarios = [];
function scenario(name, fn) { scenarios.push({ name, fn }); }

scenario('tree renders from the analysis file list', () => {
  resetWorld();
  const files = [
    file('unet/model.safetensors', 2 * 1024, { is_lfs: true }),
    file('unet/config.json', 1024),
    file('README.md', 100),
    file('config.json', 24)
  ];
  const analysis = setAnalysis(files);
  overviewPanelNode.innerHTML = '';
  ui.renderAnalysisResult(analysis);

  const html = filesPanelNode.innerHTML;
  eq(ui.getDetailActiveTab(), 'files', 'an unrecognized analysis defaults to the Files tab');
  match(html, /data-dir="unet"/, 'directory row renders');
  match(html, /2 files<\/span>/, 'directory shows its rolled-up file count');
  match(html, />unet\/<\/span>/, 'directory name carries its trailing slash');
  match(html, /data-file="unet\/model\.safetensors"/, 'nested file row renders with its full path');
  match(html, /class="file-tree-lfs"/, 'LFS file gets the LFS badge');
  doesNotMatch(html, /quant-dl-btn/, 'an idle tree has no status chips');
  match(html, /Only checked files — nothing else is added/, 'footer carries the guarantee line');
  match(html, /0 selected · 0 B/, 'footer starts empty');
  match(html, /id="fileTreeDownload"/, 'footer offers Download selected');
  doesNotMatch(html, /nonSelectableRoute/, 'no configured routes means no Save to picker');
});

scenario('recognized analysis keeps Overview as the default tab', () => {
  resetWorld();
  const files = [file('model-Q4_0.gguf', 4096)];
  const analysis = setAnalysis(files, {
    type: 'gguf',
    selectable_items: [{ category: 'quantization', filter_value: 'q4_0', label: 'Q4_0', files: ['model-Q4_0.gguf'] }]
  });
  overviewPanelNode.innerHTML = '';
  ui.renderAnalysisResult(analysis);
  eq(ui.getDetailActiveTab(), 'overview', 'a recognized analysis defaults to Overview');
  match(overviewPanelNode.innerHTML, /analysis-card/, 'overview renders the analysis card');
  match(filesPanelNode.innerHTML, /data-file="model-Q4_0\.gguf"/, 'Files tab renders alongside');
});

scenario('directory tri-state collapses to its exact member files', () => {
  resetWorld();
  const files = [
    file('unet/a.bin', 1024),
    file('unet/sub/b.bin', 2048),
    file('other/c.bin', 4096)
  ];
  setAnalysis(files);

  eq(ui.fileTreeDirState('unet'), 'off', 'untouched directory is unchecked');
  ui.fileTreeToggleDir('unet');
  eq(ui.fileTreeDirState('unet'), 'on', 'checking the directory checks it fully');
  eq(ui.fileTreeDirState('unet/sub'), 'on', 'the check is recursive into subdirectories');
  deepEq(ui.fileTreeSelectedPaths(), ['unet/a.bin', 'unet/sub/b.bin'], 'selection is exactly the member files, sorted');
  doesNotMatch(ui.fileTreeSelectedPaths(), /other/, 'sibling files stay unselected');

  ui.fileTreeToggleFile('unet/a.bin');
  eq(ui.fileTreeDirState('unet'), 'mixed', 'a partial subtree reads as indeterminate');
  eq(ui.fileTreeDirState('unet/sub'), 'on', 'fully checked subdirectory stays checked');
  ui.fileTreeToggleDir('unet');
  eq(ui.fileTreeDirState('unet'), 'on', 'clicking a partially checked directory completes the selection');
  deepEq(ui.fileTreeSelectedPaths(), ['unet/a.bin', 'unet/sub/b.bin'], 'completion restores every member file');
  ui.fileTreeToggleDir('unet');
  eq(ui.fileTreeDirState('unet'), 'off', 'clicking a fully checked directory clears it');
  deepEq(ui.fileTreeSelectedPaths(), [], 'clearing the directory empties the selection');
});

scenario('rendered checkboxes follow the model through a directory toggle (A/A2)', () => {
  resetWorld();
  const files = [
    file('unet/a.bin', 1024),
    file('unet/b.bin', 1024),
    file('config.json', 24)
  ];
  const analysis = setAnalysis(files);
  ui.renderFilesTab(analysis);

  // (A) A directory toggle changes the model without re-rendering rows, so
  // every member file box must be synced to checked, the dir box to complete,
  // and the footer to the selection count — all three agreeing.
  const dirBox = clickBox('dir', 'unet');
  eq(domBox('file', 'unet/a.bin').checked, true, 'member file box unet/a.bin shows checked after the dir toggle');
  eq(domBox('file', 'unet/b.bin').checked, true, 'member file box unet/b.bin shows checked after the dir toggle');
  eq(domBox('file', 'config.json').checked, false, 'an unrelated file box stays unchecked');
  eq(dirBox.checked, true, 'the directory box reads complete');
  eq(dirBox.indeterminate, false, 'the complete directory box is not indeterminate');
  deepEq(ui.fileTreeSelectedPaths(), ['unet/a.bin', 'unet/b.bin'], 'the model holds exactly the two member files');
  eq(summaryNode.textContent, '2 selected · 2 KB', 'the footer counts the two selected files');
  assertChecksMatchModel('after directory toggle on');

  // (A2) Clicking a member file that displays checked must uncheck it and
  // remove it from the model — no inversion between display and request.
  const aBox = clickBox('file', 'unet/a.bin');
  eq(aBox.checked, false, 'the clicked member box shows unchecked');
  eq(domBox('file', 'unet/b.bin').checked, true, 'the sibling member box stays checked');
  eq(dirBox.checked, false, 'the now-mixed directory box is not complete');
  eq(dirBox.indeterminate, true, 'the mixed directory box reads indeterminate');
  deepEq(ui.fileTreeSelectedPaths(), ['unet/b.bin'], 'the model contains exactly unet/b.bin');
  eq(summaryNode.textContent, '1 selected · 1 KB', 'the footer counts one selected file');
  assertChecksMatchModel('after member file toggle off');
});

scenario('directory mixed/complete/off transitions leave every file box on the model', () => {
  resetWorld();
  const files = [
    file('unet/a.bin', 1024),
    file('unet/sub/b.bin', 2048),
    file('other/c.bin', 4096)
  ];
  const analysis = setAnalysis(files);
  ui.renderFilesTab(analysis);

  // off → mixed through a file click: boxes and model agree.
  clickBox('file', 'unet/a.bin');
  deepEq(ui.fileTreeSelectedPaths(), ['unet/a.bin'], 'one member file is selected');
  assertChecksMatchModel('after file toggle into mixed');

  // mixed → complete through a directory click: every member box flips on.
  const unetBox = clickBox('dir', 'unet');
  eq(unetBox.checked, true, 'the mixed directory click completes the dir box');
  eq(unetBox.indeterminate, false, 'the completed directory box is not indeterminate');
  eq(domBox('file', 'unet/a.bin').checked, true, 'member a stays checked');
  eq(domBox('file', 'unet/sub/b.bin').checked, true, 'the previously unchecked member box flips to checked');
  deepEq(ui.fileTreeSelectedPaths(), ['unet/a.bin', 'unet/sub/b.bin'], 'the model holds every member file');
  assertChecksMatchModel('after mixed to complete');

  // complete → off through a directory click: every member box flips off.
  clickBox('dir', 'unet');
  eq(unetBox.checked, false, 'the complete directory click clears the dir box');
  eq(unetBox.indeterminate, false, 'the cleared directory box is not indeterminate');
  eq(domBox('file', 'unet/a.bin').checked, false, 'member a flips to unchecked');
  eq(domBox('file', 'unet/sub/b.bin').checked, false, 'member b flips to unchecked');
  deepEq(ui.fileTreeSelectedPaths(), [], 'the model is empty');
  assertChecksMatchModel('after complete to off');

  // A sibling directory toggles independently.
  const otherBox = clickBox('dir', 'other');
  eq(otherBox.checked, true, 'the sibling directory box checks');
  eq(domBox('file', 'other/c.bin').checked, true, 'its member box checks with it');
  assertChecksMatchModel('after sibling directory on');
  clickBox('dir', 'other');
  eq(otherBox.checked, false, 'the sibling directory box clears');
  assertChecksMatchModel('after sibling directory off');
});

scenario('Clear, filter re-render and live reconcile keep rendered boxes on the model', async () => {
  resetWorld();
  const files = [
    file('unet/a.bin', 1024),
    file('unet/b.bin', 1024),
    file('scripts/train.py', 2048)
  ];
  const analysis = setAnalysis(files);
  await ui.syncLocalAnalysisStatus(analysis);
  ui.renderFilesTab(analysis);
  clickBox('dir', 'unet');
  assertChecksMatchModel('initial directory selection');

  // A filter keystroke re-renders the rows: visible boxes follow the model,
  // filtered-out rows disappear from the DOM.
  typeFilter('train');
  eq(domBox('file', 'scripts/train.py').checked, false, 'the visible non-selected file box reads unchecked');
  let absent = false;
  try { domBox('file', 'unet/a.bin'); } catch (_) { absent = true; }
  eq(absent, true, 'the filtered-out file row is not rendered');
  deepEq(ui.fileTreeSelectedPaths(), ['unet/a.bin', 'unet/b.bin'], 'filtering never changes the model');
  eq(summaryNode.textContent, '2 selected · 2 KB', 'the footer keeps the selection while filtered');
  assertChecksMatchModel('while filtered');

  // Clearing the filter restores every row with its model state.
  typeFilter('');
  eq(domBox('file', 'unet/a.bin').checked, true, 'the restored member box reads checked');
  eq(domBox('file', 'scripts/train.py').checked, false, 'the restored non-selected box reads unchecked');
  assertChecksMatchModel('after filter cleared');

  // Clear empties the model and every rendered box together.
  clickClear();
  eq(domBox('file', 'unet/a.bin').checked, false, 'Clear unchecks the member box');
  eq(domBox('dir', 'unet').checked, false, 'Clear unchecks the directory box');
  eq(domBox('dir', 'unet').indeterminate, false, 'Clear leaves no indeterminate directory');
  deepEq(ui.fileTreeSelectedPaths(), [], 'Clear empties the model');
  eq(summaryNode.textContent, '0 selected · 0 B', 'Clear empties the footer');
  assertChecksMatchModel('after Clear');

  // The coalesced live reconcile re-renders statuses without touching rows:
  // boxes must still match the model afterwards.
  clickBox('dir', 'unet');
  ui.handleWSMessage({
    type: 'job_update',
    data: {
      id: 'strict-live', repo: analysis.repo, isDataset: false, status: 'running',
      strictFiles: true, filters: ['unet/a.bin'], excludes: []
    }
  });
  await ui.flushLiveReconcile();
  deepEq(ui.fileTreeSelectedPaths(), ['unet/a.bin', 'unet/b.bin'], 'the live update keeps the selection');
  assertChecksMatchModel('after live reconcile');
});

scenario('a large file tree renders with one index and one sort per node', () => {
  resetWorld();
  const time = fn => {
    const t0 = process.hrtime.bigint();
    const out = fn();
    return { out, ms: Number(process.hrtime.bigint() - t0) / 1e6 };
  };
  const sample = (label, dirCount, filesPerDir) => {
    const analysis = setAnalysis(makeDirTree(dirCount, filesPerDir));
    ui.renderFilesTab(analysis);
    eq(ui.fileTreeState.nodes.size, dirCount * filesPerDir + dirCount + 1,
      `${label}: one indexed node per file, directory and root`);

    // Auto-collapsed initial view: root rows only, bounded by the cap — the
    // old expectation (one rendered row per file) was exactly the defect.
    const initial = time(() => { ui.fileTreeState.query = ''; return ui.renderFileTreeRows(); });
    eq(countMatches(initial.out, /data-file="/g), 0,
      `${label}: the initial view lists no file rows`);
    eq(countMatches(initial.out, rowPattern), Math.min(dirCount, ROW_CAP),
      `${label}: the initial view renders exactly min(root rows, cap) rows`);
    if (dirCount > ROW_CAP) {
      eq(countMatches(initial.out, morePattern), 1,
        `${label}: a truncated root level carries exactly one show-more row`);
      match(initial.out, new RegExp(`… ${dirCount - ROW_CAP} more not shown`),
        `${label}: the show-more row states the hidden count`);
    }

    // Expanding one directory emits its children, still within the cap.
    const beforeExpand = rowsRewrites;
    caretClick('dir0000');
    eq(rowsRewrites, beforeExpand + 1, `${label}: expanding rebuilds the bounded view`);
    const expandedHtml = filesPanelNode.innerHTML;
    eq(countMatches(expandedHtml, /data-file="/g), filesPerDir,
      `${label}: expanding a directory lists exactly its children`);
    const wouldBe = dirCount + filesPerDir;
    eq(countMatches(expandedHtml, rowPattern), Math.min(wouldBe, ROW_CAP),
      `${label}: the expanded view stays within the cap`);
    if (wouldBe > ROW_CAP) {
      match(expandedHtml, new RegExp(`… ${wouldBe - ROW_CAP} more not shown`),
        `${label}: the expanded view's hidden count is exact`);
    }

    // Collapsing is a targeted update: the descendant rows go away without a
    // rows-container rewrite, and the view stays bounded.
    const beforeCollapse = rowsRewrites;
    caretClick('dir0000');
    eq(rowsRewrites, beforeCollapse, `${label}: collapsing does not rewrite the rows container`);
    const collapsedHtml = filesPanelNode.innerHTML;
    eq(countMatches(collapsedHtml, /data-file="/g), 0,
      `${label}: collapsing removes every descendant row`);
    lte(countMatches(collapsedHtml, rowPattern) + countMatches(collapsedHtml, morePattern),
      ROW_CAP + 1, `${label}: the collapsed view stays bounded`);

    // A filter keystroke renders matches up to the cap with an honest total.
    const keystroke = time(() => { ui.fileTreeState.query = 'f0001'; return ui.renderFileTreeRows(); });
    const matchWouldBe = dirCount * 2; // one directory row plus one file row per directory
    if (matchWouldBe <= ROW_CAP) {
      eq(countMatches(keystroke.out, /data-file="/g), dirCount,
        `${label}: a filter keystroke renders one match per directory`);
      eq(countMatches(keystroke.out, morePattern), 0,
        `${label}: an uncapped search needs no show-more row`);
    } else {
      eq(countMatches(keystroke.out, rowPattern), ROW_CAP,
        `${label}: a capped search stops at the cap`);
      match(keystroke.out, new RegExp(`Showing ${ROW_CAP} of ${matchWouldBe} matches`),
        `${label}: the search states its total match count honestly`);
    }
    ui.fileTreeState.query = '';
    console.log(`TIME ${label}: initial ${initial.ms.toFixed(1)} ms, keystroke ${keystroke.ms.toFixed(1)} ms`);
  };
  sample('3000 files / 40 dirs', 40, 75);
  sample('8000 files / 800 dirs', 800, 10);
});

scenario('the auto-collapse threshold is documented and honored', () => {
  resetWorld();
  eq(AUTO_COLLAPSE, 1000, 'the documented auto-collapse threshold is 1000 files');

  // At the threshold the fully expanded look is preserved exactly.
  const atLimit = setAnalysis(makeDirTree(100, 10));
  eq(atLimit.files.length, AUTO_COLLAPSE, 'the fixture sits exactly at the threshold');
  eq(ui.fileTreeState.collapsed.size, 0, 'a revision at the threshold keeps every directory expanded');
  ui.renderFilesTab(atLimit);
  match(filesPanelNode.innerHTML, /data-children="dir0000">/, 'the root directory renders expanded');

  // One file above the threshold: root rows and root files only.
  const above = setAnalysis(makeDirTree(100, 10, 1));
  eq(above.files.length, AUTO_COLLAPSE + 1, 'the fixture sits one file above the threshold');
  eq(ui.fileTreeState.collapsed.size, 100, 'a revision above the threshold collapses every directory');
  ui.renderFilesTab(above);
  const html = filesPanelNode.innerHTML;
  eq(countMatches(html, /data-file="/g), 1, 'collapsed directories contribute no descendant rows');
  match(html, /data-file="root0000\.bin"/, 'the root file stays visible');
  match(html, /data-children="dir0000" hidden/, 'the directory children container starts collapsed');
  eq(countMatches(html, rowPattern), 101, 'the initial view is root rows plus the root file');
  assertChecksMatchModel('auto-collapsed initial view');

  // Expansion state survives a same-identity refresh, as before.
  caretClick('dir0000');
  eq(ui.fileTreeState.collapsed.size, 99, 'expanding removes the directory from the collapsed set');
  ui.renderFilesTab(above);
  eq(countMatches(filesPanelNode.innerHTML, /data-file="dir0000\//g), 10,
    'the expansion survives a refresh of the same revision');
});

scenario('initial render of 5k–70k-file trees emits only root-level rows', () => {
  resetWorld();
  const rootRowsOnly = (label, dirCount, filesPerDir, rootFiles, totalFiles) => {
    setAnalysis(makeDirTree(dirCount, filesPerDir, rootFiles));
    eq(ui.fileTreeState.files.length, totalFiles, `${label}: the fixture lists the full file count`);
    const html = ui.renderFileTreeRows();
    eq(countMatches(html, /data-file="/g), 0, `${label}: the initial render emits no file rows`);
    const rows = countMatches(html, rowPattern);
    lte(rows, ROW_CAP, `${label}: the initial render is bounded by the cap`);
    eq(countMatches(html, /file-tree-row file-tree-\w+" data-path="[^"]*" style="padding-left:8px"/g), rows,
      `${label}: every emitted row is root-level`);
    const rootRows = dirCount + rootFiles;
    if (rootRows > ROW_CAP) {
      eq(countMatches(html, morePattern), 1, `${label}: truncation adds exactly one show-more row`);
      match(html, new RegExp(`… ${rootRows - ROW_CAP} more not shown`),
        `${label}: the hidden count is exact`);
    }
  };
  rootRowsOnly('5k files', 500, 10, 1, 5001);
  rootRowsOnly('c4-scale files', 800, 86, 421, 69221);
});

scenario('expanding a huge directory stays within the row cap', () => {
  resetWorld();
  const analysis = setAnalysis(makeDirTree(1, 5000));
  ui.renderFilesTab(analysis);
  eq(countMatches(filesPanelNode.innerHTML, rowPattern), 1, 'the auto-collapsed view is one directory row');
  eq(countMatches(filesPanelNode.innerHTML, /data-file="/g), 0, 'no child rows exist before expansion');

  // Selection is model-based: the collapsed directory selects all 5000
  // members while none of them is rendered.
  clickBox('dir', 'dir0000');
  eq(ui.fileTreeState.selected.size, 5000, 'a collapsed directory still selects its whole subtree');
  match(summaryNode.textContent, /^5000 selected/, 'the footer totals the fully collapsed selection');
  assertChecksMatchModel('selection while collapsed');

  // Expansion creates the rows now, capped, with the honest remainder, and
  // hands keyboard focus back to the caret that was activated.
  const before = rowsRewrites;
  caretClick('dir0000');
  eq(rowsRewrites, before + 1, 'expanding rebuilds the bounded view');
  eq(focusedToggle, 'dir0000', 'the caret keeps keyboard focus across the rebuild');
  const expandedHtml = filesPanelNode.innerHTML;
  const rows = countMatches(expandedHtml, rowPattern);
  eq(rows, ROW_CAP, 'the expanded view renders exactly the cap');
  eq(countMatches(expandedHtml, morePattern), 1, 'the overflow adds exactly one show-more row');
  lte(rows + 1, ROW_CAP + 1, 'the expanded view never exceeds cap+1 rows');
  eq(countMatches(expandedHtml, /data-file="/g), ROW_CAP - 1, 'one cap row is the directory itself');
  match(expandedHtml, new RegExp(`… ${5000 - (ROW_CAP - 1)} more not shown`),
    'the show-more row states the exact hidden count');
  assertChecksMatchModel('after expanding a fully selected directory');

  // Collapse: targeted row removal, bounded result, model untouched.
  const beforeCollapse = rowsRewrites;
  caretClick('dir0000');
  eq(rowsRewrites, beforeCollapse, 'collapsing does not rewrite the rows container');
  eq(focusedToggle, 'dir0000', 'the caret keeps keyboard focus through the targeted collapse');
  eq(countMatches(filesPanelNode.innerHTML, /data-file="/g), 0, 'collapsing removes every descendant row');
  lte(countMatches(filesPanelNode.innerHTML, rowPattern) + countMatches(filesPanelNode.innerHTML, morePattern),
    ROW_CAP + 1, 'the collapsed view stays bounded');
  eq(ui.fileTreeState.selected.size, 5000, 'collapsing never touches the selection model');
});

scenario('a search matching thousands is capped and reports its total', () => {
  resetWorld();
  const analysis = setAnalysis(makeDirTree(500, 10));
  ui.renderFilesTab(analysis);
  typeFilter('.bin');
  const html = filesPanelNode.innerHTML;
  const rows = countMatches(html, rowPattern);
  eq(rows, ROW_CAP, 'the capped search renders exactly the cap');
  eq(countMatches(html, morePattern), 1, 'the capped search adds exactly one show-more row');
  lte(rows + 1, ROW_CAP + 1, 'the search view never exceeds cap+1 rows');
  const totalMatches = 500 + 5000; // every directory row and file row matches '.bin'
  match(html, new RegExp(`Showing ${ROW_CAP} of ${totalMatches} matches — narrow the filter`),
    'the search states its total honestly and suggests narrowing');
  doesNotMatch(html, /No files match the filter/, 'a capped result is not an empty result');
  assertChecksMatchModel('while the search is capped');

  // Clearing the filter returns to the collapsed root view, still bounded.
  typeFilter('');
  eq(countMatches(filesPanelNode.innerHTML, /data-file="/g), 0, 'clearing the filter restores the collapsed view');
  lte(countMatches(filesPanelNode.innerHTML, rowPattern) + countMatches(filesPanelNode.innerHTML, morePattern),
    ROW_CAP + 1, 'the restored view stays bounded');
});

scenario('Download selected posts the exact strict selection', async () => {
  resetWorld();
  const files = [
    file('unet/a.bin', 1024),
    file('unet/sub/b.bin', 2048),
    file('config.json', 12)
  ];
  const analysis = setAnalysis(files);
  ui.fileTreeToggleDir('unet');
  ui.fileTreeToggleFile('config.json');

  await ui.downloadSelectedFiles();

  eq(downloadRequests.length, 1, 'one download request is posted');
  const body = downloadRequests[0];
  eq(body.repo, analysis.repo, 'the request names the analyzed repository');
  eq(body.revision, 'main', 'the request pins the analyzed revision');
  eq(body.dataset, false, 'the request is not a dataset download');
  eq(body.strictFiles, true, 'the request is a strict selection');
  deepEq(body.filters, ['./config.json', 'unet/a.bin', 'unet/sub/b.bin'],
    'filters are the exact checked paths, sorted, with the ./ root selector');
  eq('excludes' in body, false, 'a strict request carries no excludes');
  eq('appendFilterSubdir' in body, false, 'a strict request carries no filter subdir');
  eq('branchVariant' in body, false, 'a strict request is not a branch variant');
  eq(body.routeKey, undefined, 'no configured routes means no routeKey');
  eq(window.messages.at(-1).message, 'Queued: 3 files', 'the toast reports the selected count');
  eq(window.messages.at(-1).type, 'success', 'a new job toasts success');
});

scenario('routeKey follows the Files-tab Save to picker', async () => {
  resetWorld();
  ui.state.settings = { downloadRoutes: { audio: '/dest/audio' } };
  const files = [file('voice.wav', 1024)];
  const analysis = setAnalysis(files);
  ui.renderFilesTab(analysis);
  match(filesPanelNode.innerHTML, /id="nonSelectableRoute"/, 'the Save to picker renders with routes configured');

  // The rendered select resolves its index through routeSelectValue; index 0
  // is the configured audio route here.
  domNodes.set('#nonSelectableRoute', {
    value: '0',
    dataset: { optionCount: String(ui.state.settings.downloadRoutes ? 1 : 0) }
  });
  ui.fileTreeToggleFile('voice.wav');
  await ui.downloadSelectedFiles();
  eq(downloadRequests.at(-1).routeKey, 'audio', 'the selected routeKey is sent with the strict request');

  // Datasets are never routed, even with the picker present.
  domNodes.delete('#nonSelectableRoute');
  const datasetFiles = [file('data.bin', 512)];
  const datasetAnalysis = setAnalysis(datasetFiles, { is_dataset: true, type: 'dataset' });
  ui.renderFilesTab(datasetAnalysis);
  doesNotMatch(filesPanelNode.innerHTML, /nonSelectableRoute/, 'a dataset tree offers no Save to picker');
  ui.fileTreeToggleFile('data.bin');
  await ui.downloadSelectedFiles();
  eq(downloadRequests.at(-1).dataset, true, 'the dataset flag is sent');
  eq(downloadRequests.at(-1).routeKey, undefined, 'a dataset request carries no routeKey');
});

scenario('an existing-job message does not queue a duplicate', async () => {
  resetWorld();
  const files = [file('unet/a.bin', 1024)];
  const analysis = setAnalysis(files);
  ui.fileTreeToggleFile('unet/a.bin');
  downloadReply = () => ({ message: 'Download already in progress', job: { id: 'existing' } });
  const jobsBefore = ui.getJobs().size;

  await ui.downloadSelectedFiles();

  eq(ui.getJobs().size, jobsBefore, 'a reused job is not re-added to the job map');
  eq(window.messages.length, 1, 'exactly one toast is shown');
  eq(window.messages[0].message, 'Download already in progress', 'the server message is surfaced verbatim');
  eq(window.messages[0].type, 'info', 'a reused job toasts as info, not success');
  eq(statusesOf()['unet/a.bin'].active, '', 'no queued status is fabricated for the reused job');
  eq(window.messages.some(m => m.type === 'success'), false, 'no success toast for the reused job');
});

scenario('per-file status maps from cache presence and job selections', async () => {
  resetWorld();
  const files = [
    file('unet/a.bin', 1024),
    file('unet/b.bin', 2048),
    file('other/c.bin', 4096)
  ];
  const analysis = setAnalysis(files, {
    selectable_items: [{ category: 'quantization', filter_value: 'q4_0', label: 'Q4_0', files: ['other/c.bin'] }]
  });

  // done comes only from cache presence, even with no selectable items.
  cacheState = { status: 200, files: ['unet/a.bin', 'other/c.bin'], error: null };
  await ui.syncLocalAnalysisStatus(analysis);
  let statuses = statusesOf();
  eq(statuses['unet/a.bin'].cached, true, 'a cached file is marked done');
  eq(statuses['unet/b.bin'].cached, false, 'an uncached file is not done');
  eq(statuses['other/c.bin'].cached, true, 'cache presence does not require selectable items');

  // A failed lookup clears done entirely — cache presence is authoritative.
  cacheState = { status: 200, files: [], error: new Error('cache lookup failed') };
  await ui.syncLocalAnalysisStatus(analysis);
  statuses = statusesOf();
  eq(statuses['unet/a.bin'].cached, false, 'a failed cache lookup clears done');

  // A strict job projects its resolved selection per file.
  ui.getJobs().set('strict-1', {
    id: 'strict-1', repo: analysis.repo, isDataset: false, status: 'running',
    strictFiles: true, filters: ['unet/a.bin', 'unet/b.bin'], excludes: []
  });
  ui.getJobs().set('strict-2', {
    id: 'strict-2', repo: analysis.repo, isDataset: false, status: 'queued',
    strictFiles: true, filters: ['unet/b.bin'], excludes: []
  });
  statuses = statusesOf();
  eq(statuses['unet/a.bin'].active, 'running', 'a strict job marks its selected file running');
  eq(statuses['unet/b.bin'].active, 'running', 'running outranks queued across jobs');
  eq(statuses['other/c.bin'].active, '', 'an unselected file stays inactive');

  // A path-shaped non-strict filter projects through the shared normalization.
  ui.getJobs().clear();
  ui.getJobs().set('paths-1', {
    id: 'paths-1', repo: analysis.repo, isDataset: false, status: 'queued',
    filters: ['./unet/b.bin'], excludes: []
  });
  eq(statusesOf()['unet/b.bin'].active, 'queued', 'a path-shaped filter marks the file queued');

  // A token filter projects only through a selectable artifact item's files.
  ui.getJobs().clear();
  ui.getJobs().set('token-1', {
    id: 'token-1', repo: analysis.repo, isDataset: false, status: 'running',
    filters: ['q4_0'], excludes: []
  });
  statuses = statusesOf();
  eq(statuses['other/c.bin'].active, 'running', 'an artifact token marks its item files');
  eq(statuses['unet/a.bin'].active, '', 'an unrelated file stays inactive for the token job');

  ui.getJobs().clear();
  ui.getJobs().set('token-2', {
    id: 'token-2', repo: analysis.repo, isDataset: false, status: 'running',
    filters: ['advanced-token'], excludes: []
  });
  eq(statusesOf()['other/c.bin'].active, '', 'an arbitrary token filter is not projected per file');

  ui.getJobs().clear();
  ui.getJobs().set('foreign', {
    id: 'foreign', repo: 'other/repo', isDataset: false, status: 'running',
    strictFiles: true, filters: ['unet/a.bin'], excludes: []
  });
  eq(statusesOf()['unet/a.bin'].active, '', 'another repository\'s job does not mark this tree');
});

scenario('filter narrows the tree to matching paths', () => {
  resetWorld();
  const files = [
    file('unet/a.bin', 1024),
    file('scripts/train.py', 2048)
  ];
  setAnalysis(files);

  match(ui.renderFileTreeRows(), /data-file="unet\/a\.bin"/, 'unfiltered tree lists every file');
  match(ui.renderFileTreeRows(), /data-file="scripts\/train\.py"/, 'unfiltered tree lists scripts too');

  // The production input handler only writes this query and rewrites the rows
  // container, so the filter input keeps focus while typing.
  ui.fileTreeState.query = 'train';
  const filtered = ui.renderFileTreeRows();
  match(filtered, /data-file="scripts\/train\.py"/, 'the matching file stays visible');
  doesNotMatch(filtered, /data-dir="unet"/, 'non-matching directories drop out with their files');

  ui.fileTreeState.query = 'no-such-file';
  match(ui.renderFileTreeRows(), /No files match the filter/, 'an empty result reports it');

  ui.fileTreeState.query = '';
  match(ui.renderFileTreeRows(), /data-file="unet\/a\.bin"/, 'clearing the filter restores the tree');
});

scenario('live reconcile updates statuses without resetting selection', async () => {
  resetWorld();
  const files = [file('unet/a.bin', 1024), file('unet/b.bin', 2048)];
  const analysis = setAnalysis(files);
  // Establish an empty cache-presence snapshot for this identity so the
  // overlap counts start from known state, as the load path does.
  await ui.syncLocalAnalysisStatus(analysis);
  ui.fileTreeToggleDir('unet');
  ui.renderFilesTab(analysis);
  eq(summaryNode.textContent, '2 selected · 3 KB', 'footer totals the selection');
  eq(overlapNode.textContent, '0 already downloaded · 0 in progress', 'no overlap before any job');

  ui.handleWSMessage({
    type: 'job_update',
    data: {
      id: 'strict-live', repo: analysis.repo, isDataset: false, status: 'running',
      strictFiles: true, filters: ['unet/a.bin'], excludes: []
    }
  });
  await ui.flushLiveReconcile();

  const statuses = statusesOf();
  eq(statuses['unet/a.bin'].active, 'running', 'the live strict job marks the file running');
  eq(statuses['unet/b.bin'].active, '', 'the unselected sibling stays inactive');
  deepEq(ui.fileTreeSelectedPaths(), ['unet/a.bin', 'unet/b.bin'], 'selection survives the live update');
  eq(overlapNode.textContent, '0 already downloaded · 1 in progress', 'footer reports the in-progress overlap');
  eq(summaryNode.textContent, '2 selected · 3 KB', 'footer selection totals are unchanged');

  // Completion re-reads cache presence; a cached file becomes done while the
  // selection still holds.
  ui.handleWSMessage({
    type: 'job_update',
    data: {
      id: 'strict-live', repo: analysis.repo, isDataset: false, status: 'completed',
      strictFiles: true, filters: ['unet/a.bin'], excludes: []
    }
  });
  cacheState = { status: 200, files: ['unet/a.bin'], error: null };
  await ui.flushLiveReconcile();
  const after = statusesOf();
  eq(after['unet/a.bin'].active, '', 'the completed job is no longer active');
  eq(after['unet/a.bin'].cached, true, 'cache presence finalizes done after completion');
  eq(overlapNode.textContent, '1 already downloaded · 0 in progress', 'footer reports the downloaded overlap');
  deepEq(ui.fileTreeSelectedPaths(), ['unet/a.bin', 'unet/b.bin'], 'selection survives completion');
});

scenario('clearing the selection empties the footer', () => {
  resetWorld();
  const files = [file('a.bin', 1024)];
  const analysis = setAnalysis(files);
  ui.fileTreeToggleFile('a.bin');
  ui.renderFilesTab(analysis);
  eq(summaryNode.textContent, '1 selected · 1 KB', 'one checked file totals');
  ui.fileTreeToggleFile('a.bin');
  ui.updateFileTreeStatuses();
  eq(summaryNode.textContent, '0 selected · 0 B', 'unchecking empties the footer');
});

scenario('a failed start surfaces an error toast and fabricates nothing', async () => {
  // Regression for the observed first-click gap: a transport failure of the
  // strict POST must be surfaced as an error toast at the client (it can never
  // end silently), must add no job or status, and must keep the selection for
  // a retry. The original smoke's "no toast" was observation latency against
  // the 4s toast lifetime; this pins the layer a fixture can control.
  resetWorld();
  const files = [file('unet/a.bin', 1024)];
  setAnalysis(files);
  ui.fileTreeToggleFile('unet/a.bin');
  const jobsBefore = ui.getJobs().size;

  downloadReply = () => { throw new Error('network down'); };
  await ui.downloadSelectedFiles();

  eq(window.messages.length, 1, 'a failed start toasts exactly once');
  eq(window.messages[0].type, 'error', 'the failure toast is an error');
  match(window.messages[0].message, /^Failed: network down$/, 'the toast names the transport error');
  eq(ui.getJobs().size, jobsBefore, 'a failed start adds no job');
  eq(statusesOf()['unet/a.bin'].active, '', 'a failed start fabricates no queued status');
  deepEq(ui.fileTreeSelectedPaths(), ['unet/a.bin'], 'a failed start keeps the selection for a retry');

  // A failed disk preview is best-effort: it must not swallow the start — the
  // flow continues to the strict POST, which the server validates authoritatively.
  downloadReply = null;
  window.messages.length = 0;
  downloadRequests.length = 0;
  diskfreeError = new Error('disk probe failed');
  await ui.downloadSelectedFiles();
  eq(downloadRequests.length, 1, 'a failed disk preview still posts the strict request');
  eq(downloadRequests[0].strictFiles, true, 'the posted request is the strict selection');
  eq(window.messages.some(m => m.type === 'error'), false, 'a swallowed disk failure shows no false error');
  diskfreeError = null;
});

scenario('strict job chip label shows a count and the path list', () => {
  const strictLabel = ui.getJobQuantLabel({
    strictFiles: true,
    filters: ['unet/a.bin', 'unet/b.bin', 'config.json']
  });
  eq(strictLabel.quant, '3 files', 'a strict job is labelled by count');
  eq(strictLabel.mmproj, '', 'a strict job never splits out an mmproj chip');
  eq(strictLabel.quantTitle, 'unet/a.bin\nunet/b.bin\nconfig.json', 'the chip tooltip lists the exact paths');

  const quantLabel = ui.getJobQuantLabel({ filters: ['./model-Q4_0.gguf'] });
  eq(quantLabel.quant, './model-Q4_0.gguf', 'a non-strict job keeps its filter chip label');
  eq(quantLabel.quantTitle, undefined, 'a non-strict job carries no path-list tooltip');
});

scenario('rendering survives a missing Files panel', () => {
  resetWorld();
  const panel = domNodes.get('#modelsFilesPanel');
  domNodes.delete('#modelsFilesPanel');
  const files = [file('a.bin', 1)];
  const analysis = setAnalysis(files);
  let threw = false;
  try { ui.renderFilesTab(analysis); ui.updateFileTreeStatuses(); } catch (_) { threw = true; }
  eq(threw, false, 'a missing panel node must not throw');
  domNodes.set('#modelsFilesPanel', panel);
});

(async () => {
  let failed = 0;
  for (const { name, fn } of scenarios) {
    try {
      await fn();
      console.log('PASS ' + name);
    } catch (error) {
      failed++;
      console.log('FAIL ' + name + ': ' + error.message);
    }
  }
  console.log(`${assertions} assertions passed, ${failed} scenario failure(s), ${scenarios.length} scenarios`);
  if (failed) process.exit(1);
})().catch(error => {
  console.error(error);
  process.exit(1);
});
