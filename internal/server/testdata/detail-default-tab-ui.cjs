// Run through TestDetailDefaultTabUI: real app.js analysis rendering decides
// the default Model Details tab.
//
// The harness loads the shipped app.js inside Node's VM with DOM stubs. It
// calls the production renderAnalysisResult, so the assertion covers the tab
// state and panel visibility a user actually gets. The regression: a recognized
// repository that produces no selectable items used to open on Files, while the
// documented behavior is Overview for every recognized format and Files only for
// an unrecognized/generic repository.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

let assertions = 0;
function eq(actual, expected, message) { assertions++; assert.equal(actual, expected, message); }
function ok(value, message) { assertions++; assert.ok(value, message); }

function makeClassList() {
  const values = new Set();
  return {
    add(...names) { names.forEach(name => values.add(name)); },
    remove(...names) { names.forEach(name => values.delete(name)); },
    toggle(name, force) {
      const on = force === undefined ? !values.has(name) : !!force;
      if (on) values.add(name); else values.delete(name);
      return on;
    },
    contains(name) { return values.has(name); }
  };
}

function makeNode(extra = {}) {
  return Object.assign({
    value: '',
    textContent: '',
    innerHTML: '',
    hidden: false,
    disabled: false,
    style: {},
    dataset: {},
    classList: makeClassList(),
    attributes: {},
    tabIndex: 0,
    scrollTop: 0,
    setAttribute(name, value) { this.attributes[name] = String(value); },
    removeAttribute(name) { delete this.attributes[name]; },
    getAttribute(name) { return Object.prototype.hasOwnProperty.call(this.attributes, name) ? this.attributes[name] : null; },
    handlers: {},
    addEventListener(type, fn) { (this.handlers[type] ||= []).push(fn); },
    appendChild(child) { return child; },
    focus() {},
    remove() {},
    querySelector() { return null; },
    querySelectorAll() { return []; }
  }, extra);
}

const nodes = new Map();
for (const selector of [
  '#analyzeResult',
  '#modelsOverviewPanel',
  '#modelsCommunityPanel',
  '#modelsFilesPanel',
  '#modelsDetailTabs',
  '#modelsTabOverview',
  '#modelsTabCommunity',
  '#modelsTabFiles'
]) {
  nodes.set(selector, makeNode());
}

function makeCreatedNode() {
  const node = makeNode();
  let text = '';
  let html = '';
  Object.defineProperty(node, 'textContent', {
    get() { return text; },
    set(value) { text = String(value); }
  });
  Object.defineProperty(node, 'innerHTML', {
    get() {
      return html || text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
    },
    set(value) { html = String(value); }
  });
  return node;
}

const document = {
  readyState: 'loading',
  body: makeNode(),
  handlers: {},
  addEventListener(type, fn) { (this.handlers[type] ||= []).push(fn); },
  createElement() { return makeCreatedNode(); },
  querySelector(selector) { return nodes.get(selector) || null; },
  querySelectorAll() { return []; },
  getElementById(id) { return nodes.get('#' + id) || null; }
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
  setTimeout,
  clearTimeout,
  localStorage: {
    values: new Map(),
    getItem(key) { return this.values.has(key) ? this.values.get(key) : null; },
    setItem(key, value) { this.values.set(key, String(value)); },
    removeItem(key) { this.values.delete(key); }
  },
  CSS: { escape: value => String(value) },
  confirm: () => true,
  fetch: async rawPath => { throw new Error('unexpected fetch: ' + rawPath); }
};

let source = fs.readFileSync(
  path.join(__dirname, '..', '..', 'assets', 'static', 'js', 'app.js'),
  'utf8'
);
const marker = '  // Start\n';
if (!source.includes(marker)) {
  console.error('detail-default-tab-ui: app.js injection marker is missing');
  process.exit(1);
}
const hook = `
  window.test = {
    renderAnalysisResult,
    activeTab: () => detailActiveTab,
    panelVisible: name => {
      const panel = $('#models' + name + 'Panel');
      return !!panel && !panel.hidden;
    },
    tabsVisible: () => {
      const tabs = $('#modelsDetailTabs');
      return !!tabs && !tabs.hidden;
    }
  };
`;
source = source.replace(marker, hook + '\n' + marker);
vm.runInNewContext(source, context);
const ui = window.test;

function analysis(overrides = {}) {
  return {
    repo: 'owner/model',
    is_dataset: false,
    branch: 'main',
    type: 'transformers',
    type_description: 'Transformers model (safetensors)',
    file_count: 1,
    total_size_human: '1 KB',
    files: [{ path: 'config.json', name: 'config.json', size: 1024 }],
    ...overrides
  };
}

function render(data) {
  nodes.get('#modelsOverviewPanel').innerHTML = '';
  nodes.get('#modelsFilesPanel').innerHTML = '';
  ui.renderAnalysisResult(data);
}

// A recognized format with no smart selection is still a recognized repository:
// the description/README on Overview is its default view. This is the reported
// regression for a plain Transformers base model.
render(analysis());
eq(ui.activeTab(), 'overview', 'recognized analysis without selectable items must open on Overview');
ok(ui.panelVisible('Overview'), 'Overview panel is visible for a recognized analysis');
eq(ui.panelVisible('Files'), false, 'Files panel is not the default for a recognized analysis');
eq(ui.tabsVisible(), true, 'detail tabs stay visible for a recognized analysis');

// The Files tab itself is unchanged: it still renders the revision file list.
ok(
  String(nodes.get('#modelsFilesPanel').innerHTML).includes('config.json'),
  'the Files tab still renders the file tree when it is not the default'
);

// A recognized analysis with selectable items keeps its established default.
render(analysis({
  type: 'gguf',
  type_description: 'GGUF quantized model (llama.cpp, Ollama)',
  selectable_items: [{ category: 'quantization', filter_value: 'q4_k_m', label: 'Q4_K_M', files: ['model-Q4_K_M.gguf'] }]
}));
eq(ui.activeTab(), 'overview', 'recognized analysis with selectable items keeps Overview');
eq(ui.panelVisible('Overview'), true, 'Overview is visible for a selectable analysis');
eq(ui.panelVisible('Files'), false, 'Files is not the default for a selectable analysis');

// A recognized dataset has no selectable items in this fixture but must keep
// Overview too: the discriminator is the repository type, not the item list.
render(analysis({ type: 'dataset', is_dataset: true, type_description: 'HuggingFace dataset' }));
eq(ui.activeTab(), 'overview', 'recognized dataset without selectable items opens on Overview');

// A genuinely generic/unrecognized repository has no smart selection, so its
// file tree is the only download path and Files stays the default.
render(analysis({
  repo: 'owner/unknown',
  type: 'generic',
  type_description: 'Generic repository',
  file_count: 1,
  total_size_human: '1 KB',
  files: [{ path: 'weights.bin', name: 'weights.bin', size: 1024 }],
  selectable_items: undefined
}));
eq(ui.activeTab(), 'files', 'generic analysis must still open on Files');
eq(ui.panelVisible('Files'), true, 'Files panel is visible for a generic analysis');
eq(ui.panelVisible('Overview'), false, 'Overview is not the default for a generic analysis');
eq(ui.tabsVisible(), true, 'detail tabs stay visible for a generic analysis');

console.log(`PASS: detail default tab (${assertions} assertions)`);
