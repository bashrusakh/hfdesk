// Run through TestNavigationUIHistory: real app.js navigation, history and
// README link delegation.
//
// The harness loads the shipped app.js inside Node's VM with DOM and fetch
// stubs. It drives the actual navigation owner (openRepositoryInDetails,
// traverseRepoHistory, analyzeRepo, README link delegation, search rows and
// related-download entry points) rather than reimplementing its rules here.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const fixture = JSON.parse(process.env.HFDESK_NAV_FIXTURE || '{}');
if (!fixture.serverHtml || !fixture.pathPrefixedServerHtml || !fixture.markdown) {
  console.error('navigation-ui: HFDESK_NAV_FIXTURE is missing the README fixtures');
  process.exit(1);
}

// Auto-detection of this fixture repository finds both namespaces.
const DUAL_REPO = 'owner/both';
// This fixture repository exists only as a dataset, so auto-detection resolves
// it to the dataset namespace.
const DATASET_ONLY_REPO = 'owner/data-only';

let assertions = 0;
function ok(value, message) { assertions++; assert.ok(value, message); }
function eq(actual, expected, message) { assertions++; assert.equal(actual, expected, message); }
function deepEq(actual, expected, message) { assertions++; assert.deepEqual(actual, expected, message); }
function match(value, re, message) { assertions++; assert.match(value, re, message); }

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
    setAttribute(name, value) { this.attributes[name] = String(value); },
    removeAttribute(name) { delete this.attributes[name]; },
    getAttribute(name) { return Object.prototype.hasOwnProperty.call(this.attributes, name) ? this.attributes[name] : null; },
    handlers: {},
    addEventListener(type, fn) { (this.handlers[type] ||= []).push(fn); },
    appendChild(child) {
      const html = child && (child.innerHTML || child.textContent || '');
      if (html) this.innerHTML = String(this.innerHTML || '') + html;
      return child;
    },
    remove() {},
    focus() {},
    scrollTo() {},
    scrollTop: 0,
    scrollHeight: 0,
    clientHeight: 0
  }, extra);
}

const nodes = new Map();
for (const selector of [
  '#analyzeInput', '#analyzeBtn', '#analyzeResult', '#modelsOverviewPanel',
  '#modelsCommunityPanel', '#modelsDetailHeader',
  '#modelsDetailRepo', '#modelsDetailHFLink', '#modelsDetailBack',
  '#modelsDetailForward', '#readmeSection', '#readmePreview', '#modalTitle',
  '#modalBody', '#modalBackdrop', '#modalClose', '#toastContainer', '#modelsSpinner',
  '#modelsResultList', '#modelsSearchClear', '#modelsSortSelect',
  '#modelsTypeSelect', '#modelsGguf', '#modelsDatasets', '#scrollArrowUp',
  '#scrollArrowDown', '#storageModeBadge'
]) {
  nodes.set(selector, makeNode());
}

// The real Hub anchor's href property reflects its href attribute, so removing
// the attribute (the header hides an unknown link) must also clear the
// property. A plain node property would keep the previous URL visible.
const detailHfLinkNode = nodes.get('#modelsDetailHFLink');
detailHfLinkNode.attributes.href = '#';
Object.defineProperty(detailHfLinkNode, 'href', {
  get() {
    return Object.prototype.hasOwnProperty.call(this.attributes, 'href') ? this.attributes.href : '';
  },
  set(value) { this.attributes.href = String(value); }
});

// #modelsOverviewPanel and #modelsCommunityPanel are real children of
// #analyzeResult. Writing the shared container's innerHTML replaces both tab
// panels, so mirror that relationship: a regression that renders into
// #analyzeResult must also destroy the Community panel in this harness.
const analyzeResultNode = nodes.get('#analyzeResult');
const overviewPanelNode = nodes.get('#modelsOverviewPanel');
const communityPanelNode = nodes.get('#modelsCommunityPanel');
let analyzeResultHtml = '';
Object.defineProperty(analyzeResultNode, 'innerHTML', {
  get() { return analyzeResultHtml; },
  set(value) {
    analyzeResultHtml = String(value);
    overviewPanelNode.innerHTML = '';
    communityPanelNode.innerHTML = '';
  }
});

function makeCreatedNode() {
  const node = makeNode();
  let html = '';
  Object.defineProperty(node, 'innerHTML', {
    get() {
      return html || String(node.textContent)
        .replace(/&/g, '&amp;')
        .replace(/</g, '&lt;')
        .replace(/>/g, '&gt;');
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

let calls = [];
let analyzeImpl = defaultAnalyze;
let readmeImpl = () => ({ html: '<p>readme</p>' });
let healthImpl = () => ({ status: 'ok', version: 'test', endpoint: fixture.endpoint });
let settingsImpl = () => ({ endpoint: fixture.endpoint });

function parseAnalyzePath(rawPath) {
  const u = new URL(rawPath, 'https://hfdesk.test');
  return {
    repo: decodeURIComponent(u.pathname.slice('/api/analyze/'.length)),
    type: u.searchParams.get('type'),
    dataset: u.searchParams.get('dataset'),
    revision: u.searchParams.get('revision') || null
  };
}

function defaultAnalyze(rawPath) {
  const p = parseAnalyzePath(rawPath);
  // type=model|dataset is the only explicit namespace selector. An absent type
  // auto-detects; the legacy dataset parameter only forces dataset for the
  // literal "true".
  const selectedType = p.type === 'model' || p.type === 'dataset'
    ? p.type
    : null;
  const autoDetect = selectedType === null;
  if (autoDetect && p.repo === DUAL_REPO) {
    return {
      needsSelection: true,
      repo: p.repo,
      message: 'This repository exists as both a model and a dataset.',
      options: ['model', 'dataset']
    };
  }
  // Legacy dataset only forces the dataset namespace for the literal "true";
  // every other value auto-detects, matching the server contract.
  const isDataset = selectedType
    ? selectedType === 'dataset'
    : p.dataset === 'true' || (autoDetect && p.repo === DATASET_ONLY_REPO);
  return {
    repo: p.repo,
    is_dataset: isDataset,
    branch: p.revision || 'main',
    type: isDataset ? 'dataset' : 'gguf',
    type_description: isDataset ? 'dataset' : 'gguf',
    file_count: 0,
    total_size_human: '0 B',
    files: []
  };
}

function okResponse(data) {
  return { ok: true, status: 200, json: async () => data };
}
function notFoundResponse() {
  return { ok: false, status: 404, json: async () => ({ error: 'not found' }) };
}

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
  fetch: async (rawPath) => {
    calls.push(rawPath);
    if (rawPath === '/api/health') return okResponse(await healthImpl());
    if (rawPath === '/api/settings') return okResponse(await settingsImpl());
    if (rawPath.startsWith('/api/analyze/')) return okResponse(await analyzeImpl(rawPath));
    if (rawPath.startsWith('/api/readme/')) return okResponse(await readmeImpl(rawPath));
    if (rawPath.startsWith('/api/cache/')) return notFoundResponse();
    throw new Error('unexpected fetch: ' + rawPath);
  }
};

let source = fs.readFileSync(path.join(__dirname, '..', '..', 'assets', 'static', 'js', 'app.js'), 'utf8');
const marker = '  // Start\n';
if (!source.includes(marker)) {
  console.error('navigation-ui: app.js injection marker is missing');
  process.exit(1);
}
const hook = `
  window.test = {
    state,
    openRepositoryInDetails,
    analyzeRepo,
    traverseRepoHistory,
    currentRepoHistoryEntry,
    history: () => repoHistory.map(entry => ({ ...entry })),
    historyIndex: () => repoHistoryIndex,
    loadReadmePreview,
    handleReadmePreviewClick,
    loadAppVersion,
    loadStorageModeBadge,
    renderRelatedDownloads,
    analyzeRelatedRepo: window.analyzeRelatedRepo,
    selectAnalysisType: window.selectAnalysisType,
    modelsSelectRow: window.modelsSelectRow,
    initModelsPage,
    initModal,
    showModal,
    hideModal,
    reopenRevisionPicker: window.reopenRevisionPicker,
    showRevisionPickerFromAnalysis: window.showRevisionPickerFromAnalysis,
    getCurrentAnalysis: () => currentAnalysis,
    getSelectionState: () => ({
      repo: currentSelectedRepo,
      type: currentSelectedType,
      isDataset: currentSelectedIsDataset,
      branch: currentSelectedBranch
    }),
    getModal: () => ({
      active: $('#modalBackdrop')?.classList.contains('active') || false,
      title: $('#modalTitle')?.textContent || '',
      body: $('#modalBody')?.innerHTML || ''
    }),
    getDetailHeader: () => ({
      repo: $('#modelsDetailRepo')?.textContent || '',
      href: $('#modelsDetailHFLink')?.href || '',
      hidden: $('#modelsDetailHFLink')?.hidden
    }),
    resetNavigation() {
      analyzeRequestId++;
      readmeRequestId++;
      currentAnalysis = null;
      currentSelectedRepo = null;
      currentSelectedIsDataset = false;
      currentSelectedBranch = 'main';
      hasShownRevisionPicker = false;
      revisionPickerNavigationMode = 'replace';
      revisionPickerCancelHandler = null;
      revisionPickerRequestId = null;
      revisionPickerRepo = null;
      repoHistory = [];
      repoHistoryIndex = -1;
      syncRepoHistoryButtons();
    }
  };
`;
source = source.replace(marker, hook + '\n' + marker);
vm.runInNewContext(source, context);
const ui = window.test;
ui.initModelsPage();
ui.initModal();

function resetTest() {
  ui.resetNavigation();
  calls = [];
  analyzeImpl = defaultAnalyze;
  readmeImpl = () => ({ html: '<p>readme</p>' });
  healthImpl = () => ({ status: 'ok', version: 'test', endpoint: fixture.endpoint });
  settingsImpl = () => ({ endpoint: fixture.endpoint });
  ui.state.settings = { endpoint: fixture.endpoint };
  ui.state.settingsLoadFailed = false;
  ui.state.serverEndpoint = '';
  ui.state.settingsEndpointResolved = false;
  nodes.get('#analyzeInput').value = '';
  nodes.get('#analyzeResult').innerHTML = '';
  nodes.get('#modelsOverviewPanel').innerHTML = '';
  nodes.get('#modelsCommunityPanel').innerHTML = '';
  nodes.get('#modelsCommunityPanel').hidden = true;
  nodes.get('#readmePreview').innerHTML = '';
  nodes.get('#readmeSection').hidden = true;
  nodes.get('#modelsDetailRepo').textContent = '';
  nodes.get('#modelsDetailHFLink').href = '';
  nodes.get('#modelsDetailHFLink').hidden = false;
  nodes.get('#modelsDetailBack').disabled = true;
  nodes.get('#modelsDetailForward').disabled = true;
  nodes.get('#modalBackdrop').classList.remove('active');
  nodes.get('#modalBody').innerHTML = '';
  nodes.get('#modalTitle').textContent = '';
}

async function flush() {
  for (let i = 0; i < 8; i++) await new Promise(resolve => setImmediate(resolve));
}

function pressEscape() {
  const handlers = document.handlers.keydown || [];
  ok(handlers.length > 0, 'Escape keydown listener must be registered');
  handlers.forEach(handler => handler({ key: 'Escape' }));
}

function analyzePaths() {
  return calls.filter(call => call.startsWith('/api/analyze/'));
}
function readmePaths() {
  return calls.filter(call => call.startsWith('/api/readme/'));
}
function lastAnalyzePath() { return analyzePaths().at(-1); }
function lastReadmePath() { return readmePaths().at(-1); }

// Defer one analyze response so a test can inspect the transition state while
// the request is in flight. settle.resolve() uses the normal fixture response
// for the request path (or supplied data); settle.reject(error) fails it.
function deferredAnalyze() {
  let settle;
  const pending = new Promise((resolve, reject) => { settle = { resolve, reject }; });
  let requestPath = null;
  analyzeImpl = (rawPath) => {
    requestPath = rawPath;
    return pending.then(data => data || defaultAnalyze(rawPath));
  };
  return { path: () => requestPath, settle };
}
function historyRepos() { return Array.from(ui.history(), entry => entry.repo); }
function historyRevisions() { return Array.from(ui.history(), entry => entry.revision); }
function historyTuples() { return Array.from(ui.history(), entry => [entry.repo, entry.repoType, entry.revision]); }

function click(selector) {
  const node = nodes.get(selector);
  const handlers = node?.handlers?.click || [];
  ok(handlers.length > 0, `${selector} must have a click handler`);
  return handlers.at(-1)();
}

function fakeAnchor(attrs) {
  return {
    getAttribute(name) {
      return Object.prototype.hasOwnProperty.call(attrs, name) ? attrs[name] : null;
    }
  };
}
function clickEvent(anchor, hasInternalLink = true, overrides = {}) {
  return Object.assign({
    target: { closest() { return hasInternalLink ? anchor : null; } },
    defaultPrevented: false,
    button: 0,
    metaKey: false,
    ctrlKey: false,
    shiftKey: false,
    altKey: false,
    preventDefaultCalls: 0,
    preventDefault() {
      this.defaultPrevented = true;
      this.preventDefaultCalls++;
    }
  }, overrides);
}

function decodeEntities(value) {
  return String(value)
    .replace(/&quot;/g, '"')
    .replace(/&#39;/g, "'")
    .replace(/&amp;/g, '&');
}

function extractLinks(html) {
  const links = [];
  const anchorRe = /<a\b([^>]*)>/gi;
  let anchor;
  while ((anchor = anchorRe.exec(html))) {
    const attrs = {};
    const attrRe = /([a-zA-Z0-9:-]+)="([^"]*)"/g;
    let attr;
    while ((attr = attrRe.exec(anchor[1]))) attrs[attr[1].toLowerCase()] = decodeEntities(attr[2]);
    links.push({
      href: attrs.href || '',
      repo: attrs['data-hf-repo'] || null,
      type: attrs['data-hf-repo-type'] || null,
      revision: attrs['data-hf-repo-revision'] || null
    });
  }
  return links;
}

// internalLinkTuples reduces server-rendered HTML to the repository identity
// the click delegation consumes. The Go renderer owns this classification.
function internalLinkTuples(html) {
  return extractLinks(html)
    .filter(link => link.repo)
    .map(link => [link.repo, link.type, link.revision]);
}

function serverLink(html, repo, revision = undefined) {
  return extractLinks(html).find(link =>
    link.repo === repo && (revision === undefined || link.revision === revision));
}

// linkAnchor turns a server-emitted link into the anchor attributes the
// delegated click handler reads. The harness never re-derives classification.
function linkAnchor(link) {
  const attrs = { 'data-hf-repo': link.repo };
  if (link.type) attrs['data-hf-repo-type'] = link.type;
  if (link.revision) attrs['data-hf-repo-revision'] = link.revision;
  return fakeAnchor(attrs);
}

async function testAnalyzeFieldAutoDetectAndRefresh() {
  resetTest();

  // Start from a resolved dataset view of the same repository.
  await ui.openRepositoryInDetails('owner/shared', 'dataset', null, 'push');
  eq(ui.history().length, 1, 'dataset view must create one history stop');
  eq(ui.history()[0].repoType, 'dataset', 'explicit dataset view must be stored as dataset');
  eq(lastAnalyzePath(), '/api/analyze/owner/shared?type=dataset', 'dataset entry sends type=dataset');

  // Analyze field omits the type and must not inherit the current dataset type.
  calls = [];
  analyzeImpl = () => ({ needsSelection: true, repo: 'owner/shared', message: 'choose model or dataset' });
  nodes.get('#analyzeInput').value = 'owner/shared';
  await ui.analyzeRepo();
  eq(lastAnalyzePath(), '/api/analyze/owner/shared', 'Analyze field must auto-detect, not inherit type=dataset');
  eq(ui.history().at(-1).repoType, null, 'pending auto-detect entry must not force a type');

  // Refresh while type selection is pending targets the pending repo and keeps
  // auto-detection; it must not fall back to the previously resolved dataset.
  calls = [];
  click('#analyzeBtn');
  await flush();
  eq(lastAnalyzePath(), '/api/analyze/owner/shared', 'refresh during type selection must auto-detect the pending repo');
  eq(ui.history().at(-1).repo, 'owner/shared', 'refresh keeps the pending history entry');
  eq(ui.history().length, 1, 'refresh must refine, not duplicate, the pending entry');

  // Refresh of a resolved dataset view keeps its explicit type.
  resetTest();
  await ui.openRepositoryInDetails('owner/shared', 'dataset', null, 'push');
  calls = [];
  click('#analyzeBtn');
  await flush();
  eq(lastAnalyzePath(), '/api/analyze/owner/shared?type=dataset&revision=main', 'refresh of a resolved dataset keeps type=dataset and its resolved revision');
}

async function testDatasetOnlyAutoDetect() {
  resetTest();

  // A dataset-only repository auto-detects successfully through a
  // parameter-free Analyze-field request.
  nodes.get('#analyzeInput').value = DATASET_ONLY_REPO;
  await ui.analyzeRepo();
  eq(lastAnalyzePath(), `/api/analyze/${DATASET_ONLY_REPO}`, 'Analyze field on a dataset-only repository sends no type selector');
  eq(ui.getCurrentAnalysis().is_dataset, true, 'dataset-only auto-detection resolves the dataset namespace');
  eq(ui.history().at(-1).repoType, 'dataset', 'resolved dataset-only auto-detection stores dataset identity');
  eq(ui.history().length, 1, 'dataset-only auto-detection creates one resolved stop');
}

async function testReadmeServerLinks() {
  // The Go renderer is the only README HTML/URL owner. These fixtures are
  // produced by renderReadmeHTML in TestNavigationUIHistory, so asserting the
  // emitted link set covers classification without a client-side mirror.
  deepEq(
    internalLinkTuples(fixture.serverHtml),
    [
      ['owner/model', 'model', null],
      ['owner/data', 'dataset', null],
      ['owner/model', 'model', 'dev'],
      ['owner/model', 'model', 'refs/pr/1']
    ],
    'server-rendered README internal links (default endpoint)'
  );

  // Reserved Hub routes, blob/tree file paths, same-host links outside the
  // endpoint prefix and other hosts all stay external.
  for (const href of [
    'https://huggingface.co/hardware/a10',
    'https://huggingface.co/inference/models',
    'https://huggingface.co/solutions/models',
    'https://huggingface.co/chat/assistants',
    'https://huggingface.co/owner/model/blob/main/config.json',
    'https://huggingface.co/owner/model/tree/main/folder/file.md',
    'https://huggingface.co/datasets/owner/data/tree/v1/train',
    'https://example.com/owner/model',
    'https://example.com/?a=1&b=2'
  ]) {
    const link = extractLinks(fixture.serverHtml).find(candidate => candidate.href === href);
    ok(link, `server-rendered README must keep ${href}`);
    eq(link.repo, null, `${href} must stay external`);
  }

  deepEq(
    internalLinkTuples(fixture.pathPrefixedServerHtml),
    [
      ['owner/model', 'model', null],
      ['owner/data', 'dataset', 'v1']
    ],
    'server-rendered README internal links (path-prefixed endpoint)'
  );
  for (const href of [
    'https://mirror.example/owner/model',
    'https://mirror.example/hf/owner/model/tree/dev/folder/file.md'
  ]) {
    const link = extractLinks(fixture.pathPrefixedServerHtml).find(candidate => candidate.href === href);
    ok(link, `path-prefixed README must keep ${href}`);
    eq(link.repo, null, `${href} must stay external under the endpoint prefix`);
  }

  // Drive the real server-emitted anchors through the delegated click handler.
  resetTest();
  let link = serverLink(fixture.serverHtml, 'owner/model', null);
  ok(link, 'server-rendered README must expose the model root link');
  let event = clickEvent(linkAnchor(link));
  ui.handleReadmePreviewClick(event);
  eq(event.defaultPrevented, true, 'model README link must prevent the external navigation');
  await flush();
  eq(lastAnalyzePath(), '/api/analyze/owner/model?type=model', 'model README link navigates internally with an explicit model type');
  eq(ui.history().at(-1).repo, 'owner/model', 'model README link enters history');
  eq(ui.history().at(-1).repoType, 'model', 'model README link restores its model type');

  resetTest();
  link = serverLink(fixture.serverHtml, 'owner/data', null);
  ok(link, 'server-rendered README must expose the dataset root link');
  event = clickEvent(linkAnchor(link));
  ui.handleReadmePreviewClick(event);
  eq(event.defaultPrevented, true, 'dataset README link must prevent the external navigation');
  await flush();
  eq(lastAnalyzePath(), '/api/analyze/owner/data?type=dataset', 'dataset README link navigates internally with type=dataset');
  eq(ui.history().at(-1).repoType, 'dataset', 'dataset README link restores its dataset type');

  resetTest();
  link = serverLink(fixture.serverHtml, 'owner/model', 'dev');
  ok(link, 'server-rendered README must expose the revision link');
  event = clickEvent(linkAnchor(link));
  ui.handleReadmePreviewClick(event);
  eq(event.defaultPrevented, true, 'revision README link must prevent the external navigation');
  await flush();
  eq(lastAnalyzePath(), '/api/analyze/owner/model?type=model&revision=dev', 'revision README link keeps the revision');
  eq(ui.history().at(-1).revision, 'dev', 'revision README link restores the revision');

  resetTest();
  link = serverLink(fixture.serverHtml, 'owner/model', 'refs/pr/1');
  ok(link, 'server-rendered README must expose the encoded-slash revision link');
  event = clickEvent(linkAnchor(link));
  ui.handleReadmePreviewClick(event);
  eq(event.defaultPrevented, true, 'encoded revision README link must prevent the external navigation');
  await flush();
  eq(lastAnalyzePath(), '/api/analyze/owner/model?type=model&revision=refs%2Fpr%2F1', 'encoded revision stays one revision segment');
  eq(ui.history().at(-1).revision, 'refs/pr/1', 'encoded revision restores its decoded value');
}

async function assertReadmeClickStaysExternal(event, label, expectedPrevented = false) {
  ui.handleReadmePreviewClick(event);
  eq(event.defaultPrevented, expectedPrevented, `${label} must not change the default action`);
  eq(event.preventDefaultCalls, 0, `${label} must not call preventDefault`);
  await flush();
  eq(ui.history().length, 0, `${label} must not enter history`);
  eq(analyzePaths().length, 0, `${label} must not request analysis`);
}

async function testReadmeClickDelegation() {
  const modelLink = serverLink(fixture.serverHtml, 'owner/model', null);
  ok(modelLink, 'server-rendered README must expose the model root link');

  for (const [label, overrides] of [
    ['Ctrl-modified', { ctrlKey: true }],
    ['Meta-modified', { metaKey: true }],
    ['Shift-modified', { shiftKey: true }],
    ['Alt-modified', { altKey: true }],
    ['middle', { button: 1 }]
  ]) {
    resetTest();
    calls = [];
    const event = clickEvent(linkAnchor(modelLink), true, overrides);
    await assertReadmeClickStaysExternal(event, `${label} README link click`);
  }

  resetTest();
  calls = [];
  const preventedEvent = clickEvent(linkAnchor(modelLink), true, { defaultPrevented: true });
  await assertReadmeClickStaysExternal(preventedEvent, 'already-prevented README link click', true);

  resetTest();
  calls = [];
  const externalEvent = clickEvent(fakeAnchor({}), false);
  await assertReadmeClickStaysExternal(externalEvent, 'anchor without data-hf-repo');

  // Server attributes are the only internal-link source; forged repository or
  // revision values must fail closed before navigation starts.
  for (const forged of ['owner', 'owner/model/extra', '../model', 'owner/..', 'owner\\model', 'owner/model\x00']) {
    resetTest();
    calls = [];
    const event = clickEvent(fakeAnchor({
      'data-hf-repo': forged,
      'data-hf-repo-type': 'model'
    }));
    await assertReadmeClickStaysExternal(event, `forged repository ${JSON.stringify(forged)}`);
  }

  for (const forged of ['..', '.', 'bad\nrevision', 'bad\x00revision', 'bad\\revision']) {
    resetTest();
    calls = [];
    const event = clickEvent(fakeAnchor({
      'data-hf-repo': 'owner/model',
      'data-hf-repo-type': 'model',
      'data-hf-repo-revision': forged
    }));
    await assertReadmeClickStaysExternal(event, `forged revision ${JSON.stringify(forged)}`);
  }
}

async function testReadmePreviewContract() {
  const serverHtml = '<p class="server-markup">server</p>';
  const unsafeMarkdown = "[x](javascript:document.body.innerHTML='pwn')";

  resetTest();
  readmeImpl = () => ({ html: serverHtml });
  await ui.loadReadmePreview({ repo: 'owner/model', branch: 'main' });
  eq(lastReadmePath(), '/api/readme/owner/model?revision=main', 'README request keeps the revision parameter');
  eq(nodes.get('#readmePreview').innerHTML, serverHtml, 'server-rendered README HTML is used verbatim');
  eq(nodes.get('#readmeSection').hidden, false, 'server-rendered README reveals the section');

  resetTest();
  readmeImpl = () => ({ html: serverHtml });
  await ui.loadReadmePreview({ repo: 'owner/data', branch: 'dev', is_dataset: true });
  eq(lastReadmePath(), '/api/readme/owner/data?revision=dev&dataset=true', 'README request keeps revision and dataset');

  // Empty server HTML is valid: sanitization removed everything, so raw
  // markdown must never be rendered and the section stays hidden.
  resetTest();
  readmeImpl = () => ({ html: '', markdown: unsafeMarkdown });
  await ui.loadReadmePreview({ repo: 'owner/readme', branch: 'main' });
  eq(nodes.get('#readmePreview').innerHTML, '', 'empty html leaves the preview empty');
  eq(nodes.get('#readmeSection').hidden, true, 'empty html hides the README section');
  ok(!/javascript:/i.test(nodes.get('#readmePreview').innerHTML), 'empty html never emits a javascript: URL');
  ok(!/document\.body/.test(nodes.get('#readmePreview').innerHTML), 'empty html never renders raw markdown');

  // A 200 response missing html (or carrying null/non-string html) is a
  // contract error even when markdown is present. The raw markdown must not be
  // rendered, so the preview stays empty and the section stays hidden.
  for (const [html, label, present] of [
    [null, 'null html', true],
    [42, 'non-string html', true],
    [undefined, 'missing html', false]
  ]) {
    resetTest();
    const payload = { markdown: fixture.markdown };
    if (present) payload.html = html;
    readmeImpl = () => payload;
    await ui.loadReadmePreview({ repo: 'owner/readme', branch: 'main' });
    eq(nodes.get('#readmePreview').innerHTML, '', `${label} leaves the preview empty`);
    eq(nodes.get('#readmeSection').hidden, true, `${label} hides the README section`);
    ok(!/<a\b/i.test(nodes.get('#readmePreview').innerHTML), `${label} never renders markdown links`);
    ok(!/data-hf-repo|javascript:/i.test(nodes.get('#readmePreview').innerHTML), `${label} never exposes unsafe markup or URLs`);
  }

  // A fetch or API error clears any previous README and hides the section.
  resetTest();
  nodes.get('#readmePreview').innerHTML = '<p class="stale">stale</p>';
  nodes.get('#readmeSection').hidden = false;
  readmeImpl = () => { throw new Error('readme unavailable'); };
  await ui.loadReadmePreview({ repo: 'owner/readme', branch: 'main' });
  eq(nodes.get('#readmePreview').innerHTML, '', 'failed README request clears the preview');
  eq(nodes.get('#readmeSection').hidden, true, 'failed README request hides the README section');
}

async function testDualNamespaceSelection() {
  resetTest();

  // The Analyze field omits the type. On a dual-namespace repository the
  // server must be allowed to return needsSelection instead of inheriting a
  // previously resolved namespace.
  analyzeImpl = defaultAnalyze;
  nodes.get('#analyzeInput').value = DUAL_REPO;
  await ui.analyzeRepo();
  eq(lastAnalyzePath(), `/api/analyze/${DUAL_REPO}`, 'Analyze field on a dual repository omits type and dataset and auto-detects');
  eq(ui.history().at(-1).repoType, null, 'pending type selection keeps the history entry untyped');
  match(nodes.get('#modelsOverviewPanel').innerHTML, /Selection Required/, 'dual repository renders the type selector');

  // Clicking "Analyze as Model" resolves through the explicit model path and
  // replaces the pending stop, so the selector cannot loop.
  calls = [];
  ui.selectAnalysisType('model', DUAL_REPO);
  await flush();
  eq(lastAnalyzePath(), `/api/analyze/${DUAL_REPO}?type=model`, 'Analyze as Model sends exactly type=model');
  eq(ui.getCurrentAnalysis().is_dataset, false, 'Analyze as Model restores the model view');
  eq(ui.getCurrentAnalysis().type, 'gguf', 'Analyze as Model renders model analysis');
  eq(ui.history().at(-1).repoType, 'model', 'Analyze as Model replaces the pending entry with a model stop');
  eq(ui.history().length, 1, 'type selection refines the pending stop without adding history');
  ok(!/Selection Required/.test(nodes.get('#modelsOverviewPanel').innerHTML), 'explicit model view does not loop back to type selection');
  eq(analyzePaths().filter(p => p === `/api/analyze/${DUAL_REPO}`).length, 0, 'explicit model selection never re-probes auto-detection');

  // A model README link on the same dual repository also keeps model identity.
  resetTest();
  const modelEvent = clickEvent(fakeAnchor({
    'data-hf-repo': DUAL_REPO,
    'data-hf-repo-type': 'model'
  }));
  ui.handleReadmePreviewClick(modelEvent);
  eq(modelEvent.defaultPrevented, true, 'dual model README link prevents external navigation');
  await flush();
  eq(lastAnalyzePath(), `/api/analyze/${DUAL_REPO}?type=model`, 'dual model README link sends type=model');
  eq(ui.getCurrentAnalysis().is_dataset, false, 'dual model README link restores the model view');
  eq(ui.history().at(-1).repoType, 'model', 'dual model README link restores model history');

  // The dataset link selects the other namespace explicitly.
  resetTest();
  const datasetEvent = clickEvent(fakeAnchor({
    'data-hf-repo': DUAL_REPO,
    'data-hf-repo-type': 'dataset'
  }));
  ui.handleReadmePreviewClick(datasetEvent);
  eq(datasetEvent.defaultPrevented, true, 'dual dataset README link prevents external navigation');
  await flush();
  eq(lastAnalyzePath(), `/api/analyze/${DUAL_REPO}?type=dataset`, 'dual dataset README link sends type=dataset');
  eq(ui.getCurrentAnalysis().is_dataset, true, 'dual dataset README link restores the dataset view');
  eq(ui.history().at(-1).repoType, 'dataset', 'dual dataset README link restores dataset history');
}

async function testLineageAndRelatedDownloads() {
  resetTest();
  analyzeImpl = (rawPath) => {
    const data = defaultAnalyze(rawPath);
    data.repo = 'owner/tuned';
    data.gguf = {
      model_name: 'tuned',
      model_chain: [
        { repo: 'owner/base', is_current: false, relation: 'finetune' },
        { repo: 'owner/tuned', is_current: true }
      ]
    };
    data.related_downloads = [
      { repo: 'owner/mmproj', label: 'Vision projector', description: 'mmproj', size_human: '1 MB' }
    ];
    return data;
  };
  await ui.openRepositoryInDetails('owner/tuned', 'model', null, 'push');

  const resultHtml = nodes.get('#modelsOverviewPanel').innerHTML;
  match(resultHtml, /class="lineage-repo-link"/, 'lineage ancestor must render as a link');
  match(resultHtml, /data-repo="owner\/base"/, 'lineage ancestor must carry its repository identity');
  match(resultHtml, /data-repo="owner\/mmproj"/, 'Related Downloads must carry its repository identity');

  const relatedHtml = ui.renderRelatedDownloads([{ repo: 'owner/mmproj', label: 'Vision projector' }]);
  match(relatedHtml, /analyzeRelatedRepo\(this\.dataset\.repo\)/, 'Related Downloads Analyze must use the centralized navigation action');

  calls = [];
  analyzeImpl = defaultAnalyze;
  ui.analyzeRelatedRepo('owner/base');
  await flush();
  eq(lastAnalyzePath(), '/api/analyze/owner/base?type=model', 'lineage ancestor navigates internally as a model');
  deepEq(historyRepos(), ['owner/tuned', 'owner/base'], 'lineage ancestor participates in history');

  await ui.traverseRepoHistory(-1);
  eq(ui.currentRepoHistoryEntry().repo, 'owner/tuned', 'Back returns from the lineage ancestor');

  calls = [];
  ui.analyzeRelatedRepo('owner/mmproj');
  await flush();
  eq(lastAnalyzePath(), '/api/analyze/owner/mmproj?type=model', 'Related Downloads navigates internally');
  deepEq(historyRepos(), ['owner/tuned', 'owner/mmproj'], 'Related Downloads participates in history');
  await ui.traverseRepoHistory(-1);
  eq(ui.currentRepoHistoryEntry().repo, 'owner/tuned', 'Back returns from Related Downloads');
}

async function testSameRepoModelDatasetHistory() {
  resetTest();
  await ui.openRepositoryInDetails('owner/both', 'model', null, 'push');
  await ui.openRepositoryInDetails('owner/both', 'dataset', null, 'push');
  deepEq(
    historyTuples(),
    [['owner/both', 'model', 'main'], ['owner/both', 'dataset', 'main']],
    'explicit model and dataset views of the same repo are separate stops'
  );

  calls = [];
  await ui.traverseRepoHistory(-1);
  eq(lastAnalyzePath(), '/api/analyze/owner/both?type=model&revision=main', 'Back restores the model view of the same repo');
  await ui.traverseRepoHistory(1);
  eq(lastAnalyzePath(), '/api/analyze/owner/both?type=dataset&revision=main', 'Forward restores the dataset view of the same repo');
}

async function testHistoryTraversalAndForwardTruncation() {
  resetTest();
  await ui.openRepositoryInDetails('owner/a', 'model', 'dev', 'push');
  await ui.openRepositoryInDetails('owner/b', 'dataset', null, 'push');
  await ui.openRepositoryInDetails('owner/c', 'model', 'v2', 'push');

  deepEq(
    historyTuples(),
    [['owner/a', 'model', 'dev'], ['owner/b', 'dataset', 'main'], ['owner/c', 'model', 'v2']],
    'A/B/C history must keep model, dataset and revision stops'
  );
  eq(ui.historyIndex(), 2, 'A/B/C leaves the cursor on C');

  calls = [];
  await ui.traverseRepoHistory(-1);
  eq(lastAnalyzePath(), '/api/analyze/owner/b?type=dataset&revision=main', 'first Back restores B as a dataset');
  eq(ui.historyIndex(), 1, 'first Back moves the cursor to B');

  await ui.traverseRepoHistory(-1);
  eq(lastAnalyzePath(), '/api/analyze/owner/a?type=model&revision=dev', 'second Back restores A at dev');
  eq(ui.historyIndex(), 0, 'second Back moves the cursor to A');

  await ui.traverseRepoHistory(1);
  eq(lastAnalyzePath(), '/api/analyze/owner/b?type=dataset&revision=main', 'first Forward restores B again');
  await ui.traverseRepoHistory(1);
  eq(lastAnalyzePath(), '/api/analyze/owner/c?type=model&revision=v2', 'second Forward restores C at v2');
  eq(ui.historyIndex(), 2, 'Forward returns to C');

  // Back then a new target truncates the forward branch.
  await ui.traverseRepoHistory(-1);
  await ui.openRepositoryInDetails('owner/d', 'model', null, 'push');
  deepEq(historyRepos(), ['owner/a', 'owner/b', 'owner/d'], 'a new target after Back discards the Forward branch');
  eq(ui.historyIndex(), 2, 'the new target becomes the current stop');
  eq(nodes.get('#modelsDetailForward').disabled, true, 'Forward is disabled after truncating the branch');
  await ui.traverseRepoHistory(1);
  eq(ui.historyIndex(), 2, 'Forward cannot move past the truncated branch');
}

async function testSameRepoRootKeepsRevisionStop() {
  resetTest();
  await ui.openRepositoryInDetails('owner/model', 'model', 'dev', 'push');
  eq(ui.history()[0].revision, 'dev', 'non-default revision is the current stop');

  calls = [];
  click('#analyzeBtn');
  await flush();
  eq(lastAnalyzePath(), '/api/analyze/owner/model?type=model&revision=dev', 'refresh of a resolved model keeps its explicit type and revision');

  await ui.openRepositoryInDetails('owner/model', 'model', null, 'push');
  deepEq(historyRevisions(), ['dev', 'main'], 'same-repo root pushes a new stop instead of replacing dev');
  eq(ui.historyIndex(), 1, 'the root is current');

  calls = [];
  await ui.traverseRepoHistory(-1);
  eq(lastAnalyzePath(), '/api/analyze/owner/model?type=model&revision=dev', 'Back restores the preserved dev stop');
  eq(ui.historyIndex(), 0, 'Back returns to dev');
}

async function testSameRepoRootLinkKeepsResolvedRevision() {
  resetTest();
  analyzeImpl = (rawPath) => {
    const data = defaultAnalyze(rawPath);
    data.refs = [{ name: 'main', type: 'branch' }, { name: 'dev', type: 'branch' }];
    return data;
  };

  // Resolve the repository view explicitly at main so this regression starts
  // from a settled stop instead of the initial pending picker.
  await ui.openRepositoryInDetails('owner/model', 'model', 'main', 'push');
  deepEq(historyTuples(), [['owner/model', 'model', 'main']], 'resolved main view creates one stop');
  eq(lastAnalyzePath(), '/api/analyze/owner/model?type=model&revision=main', 'resolved main view loads revision=main');
  eq(nodes.get('#modalBackdrop').classList.contains('active'), false, 'resolved main view shows no revision picker');

  // The real server renderer emits a same-repo root link without a revision.
  const rootLink = serverLink(fixture.serverHtml, 'owner/model', null);
  ok(rootLink, 'server-rendered README must expose the same-repo root link');
  eq(rootLink.type, 'model', 'README root link keeps the model type');

  calls = [];
  const event = clickEvent(linkAnchor(rootLink));
  ui.handleReadmePreviewClick(event);
  eq(event.defaultPrevented, true, 'same-repo README root link must prevent external navigation');
  await flush();

  deepEq(historyTuples(), [['owner/model', 'model', 'main']], 'same-repo root link keeps the single resolved main stop');
  eq(ui.history().length, 1, 'same-repo root link does not add or replace with an unresolved entry');
  eq(ui.currentRepoHistoryEntry().revision, 'main', 'same-repo root link keeps the resolved revision');
  eq(ui.historyIndex(), 0, 'same-repo root link keeps the current stop');
  eq(analyzePaths().length, 1, 'same-repo root link makes one analyze request');
  eq(lastAnalyzePath(), '/api/analyze/owner/model?type=model&revision=main', 'same-repo root link refreshes with revision=main');
  ok(!analyzePaths().includes('/api/analyze/owner/model?type=model'), 'same-repo root link never reloads the unresolved model view');
  eq(nodes.get('#modalBackdrop').classList.contains('active'), false, 'same-repo root link does not reopen the revision picker');
  eq(nodes.get('#modelsDetailBack').disabled, true, 'same-repo root link leaves Back disabled');
}

async function testRefreshPendingRevisionPicker() {
  resetTest();
  await ui.openRepositoryInDetails('owner/previous', 'model', null, 'push');

  calls = [];
  analyzeImpl = (rawPath) => {
    const data = defaultAnalyze(rawPath);
    data.repo = 'owner/pending';
    data.refs = [{ name: 'main', type: 'branch' }, { name: 'dev', type: 'branch' }];
    return data;
  };
  await ui.openRepositoryInDetails('owner/pending', 'model', null, 'push');
  eq(ui.currentRepoHistoryEntry().repo, 'owner/pending', 'revision-picker navigation keeps the pending repo');
  eq(ui.currentRepoHistoryEntry().revision, null, 'revision-picker navigation is still pending');

  calls = [];
  click('#analyzeBtn');
  await flush();
  eq(lastAnalyzePath(), '/api/analyze/owner/pending?type=model', 'refresh during the revision picker targets the pending repo');
  eq(ui.history().length, 2, 'refresh refines the pending entry instead of duplicating it');
  eq(ui.historyIndex(), 1, 'refresh keeps the pending entry current');
}

async function testCancelInitialRevisionPickerWithCloseButton() {
  resetTest();
  analyzeImpl = (rawPath) => {
    const data = defaultAnalyze(rawPath);
    data.refs = [{ name: 'main', type: 'branch' }, { name: 'dev', type: 'branch' }];
    return data;
  };

  // A resolved view, then a pending navigation whose repository has multiple
  // refs: the real picker opens and loadRepositoryDetails returns with the pane
  // still showing its loading spinner.
  await ui.openRepositoryInDetails('owner/previous', 'model', 'main', 'push');
  await ui.openRepositoryInDetails('owner/pending', 'model', null, 'push');
  deepEq(
    historyTuples(),
    [['owner/previous', 'model', 'main'], ['owner/pending', 'model', null]],
    'pending picker navigation pushes one unresolved stop'
  );
  eq(ui.historyIndex(), 1, 'the pending picker entry is current');
  eq(ui.getModal().active, true, 'multiple refs open the revision picker');
  match(nodes.get('#modelsOverviewPanel').innerHTML, /Analyzing owner\/pending/, 'picker navigation leaves the pane loading before cancel');

  // The real X close path.
  click('#modalClose');
  eq(ui.getModal().active, false, 'the X button closes the picker modal');
  ok(!/loading-state|Analyzing/.test(nodes.get('#modelsOverviewPanel').innerHTML), 'cancel replaces the indefinite loading state');
  match(nodes.get('#modelsOverviewPanel').innerHTML, /Select a revision/, 'cancel renders an explicit pending prompt');
  match(nodes.get('#modelsOverviewPanel').innerHTML, /reopenRevisionPicker\(\)/, 'the pending prompt can reopen the picker');
  deepEq(
    historyTuples(),
    [['owner/previous', 'model', 'main'], ['owner/pending', 'model', null]],
    'cancel keeps the previous stop and the pending stop'
  );
  eq(ui.historyIndex(), 1, 'cancel keeps the pending stop current');
  eq(ui.currentRepoHistoryEntry().repo, 'owner/pending', 'the current history entry is still the pending repository');
  eq(ui.getDetailHeader().repo, 'owner/pending', 'the header agrees with the pending history entry');
  eq(nodes.get('#modelsDetailBack').disabled, false, 'Back stays enabled with a previous stop');
  eq(nodes.get('#modelsDetailForward').disabled, true, 'Forward stays disabled at the latest pending stop');
  deepEq(
    { ...ui.getSelectionState() },
    { repo: 'owner/pending', type: 'model', isDataset: false, branch: null },
    'cancel leaves no revision selected while the pending repository/type stay resolved'
  );

  // Reopen from the pending prompt and use the default action, which must resolve
  // the pending stop rather than be mistaken for another cancel.
  ui.reopenRevisionPicker();
  eq(ui.getModal().active, true, 'the pending prompt reopens the picker');
  match(ui.getModal().body, /data-revision="dev"/, 'the reopened picker lists the revisions again');
  window.selectRevision('main', false, 'owner/pending');
  await flush();
  eq(ui.getModal().active, false, 'selecting a revision closes the picker');
  deepEq(
    historyTuples(),
    [['owner/previous', 'model', 'main'], ['owner/pending', 'model', 'main']],
    'the reopened default action refines the pending stop without cancelling it'
  );
  eq(ui.historyIndex(), 1, 'selection keeps the refined stop current');
  eq(ui.currentRepoHistoryEntry().revision, 'main', 'the pending stop resolves to the selected revision');
  eq(ui.getCurrentAnalysis().branch, 'main', 'the selected revision is rendered');
  match(nodes.get('#modelsOverviewPanel').innerHTML, /onclick="showRevisionPickerFromAnalysis\(\)"/, 'the resolved view is no longer pending');
  eq(nodes.get('#modelsDetailBack').disabled, false, 'Back stays enabled after resolving the pending stop');
  eq(nodes.get('#modelsDetailForward').disabled, true, 'Forward stays disabled after resolving the pending stop');
}

async function testCancelInitialRevisionPickerWithEscape() {
  resetTest();
  analyzeImpl = (rawPath) => {
    const data = defaultAnalyze(rawPath);
    if (data.repo === 'owner/solo') {
      data.refs = [{ name: 'main', type: 'branch' }, { name: 'dev', type: 'branch' }];
    }
    return data;
  };

  // No previous history entry: the pending state itself must stay coherent.
  nodes.get('#analyzeInput').value = 'owner/solo';
  await ui.analyzeRepo();
  deepEq(historyTuples(), [['owner/solo', null, null]], 'auto-detected picker navigation starts as one untyped pending stop');
  eq(ui.getModal().active, true, 'auto-detected multiple refs open the picker');

  pressEscape();
  eq(ui.getModal().active, false, 'Escape closes the picker modal');
  ok(!/loading-state|Analyzing/.test(nodes.get('#modelsOverviewPanel').innerHTML), 'Escape replaces the indefinite loading state');
  match(nodes.get('#modelsOverviewPanel').innerHTML, /Select a revision/, 'Escape renders the explicit pending prompt');
  deepEq(
    historyTuples(),
    [['owner/solo', 'model', null]],
    'cancel refines the pending stop with the namespace resolved by auto-detection'
  );
  eq(ui.historyIndex(), 0, 'the only pending stop stays current');
  eq(nodes.get('#modelsDetailBack').disabled, true, 'Back is disabled without a previous stop');
  eq(nodes.get('#modelsDetailForward').disabled, true, 'Forward is disabled without a forward branch');
  eq(ui.getDetailHeader().repo, 'owner/solo', 'the header identifies the pending repository without a previous view');
  deepEq(
    { ...ui.getSelectionState() },
    { repo: 'owner/solo', type: 'model', isDataset: false, branch: null },
    'the pending selection state agrees with the resolved type and missing revision'
  );

  ui.reopenRevisionPicker();
  eq(ui.getModal().active, true, 'the pending prompt reopens the picker with no previous history entry');
  window.selectRevision('dev', false, 'owner/solo');
  await flush();
  deepEq(
    historyTuples(),
    [['owner/solo', 'model', 'dev']],
    'selection resolves the single pending stop instead of adding a duplicate'
  );
  eq(ui.historyIndex(), 0, 'the resolved stop remains current');
  eq(ui.getCurrentAnalysis().branch, 'dev', 'the selected revision is rendered');
}

async function testCancelAnalysisRevisionPickerKeepsRenderedView() {
  resetTest();
  analyzeImpl = (rawPath) => {
    const data = defaultAnalyze(rawPath);
    data.refs = [{ name: 'main', type: 'branch' }, { name: 'dev', type: 'branch' }];
    return data;
  };
  await ui.openRepositoryInDetails('owner/model', 'model', 'main', 'push');

  const rendered = nodes.get('#modelsOverviewPanel').innerHTML;
  const renderedHistory = historyTuples();
  const renderedHeader = ui.getDetailHeader();
  ok(rendered.includes('showRevisionPickerFromAnalysis'), 'the rendered analysis exposes the change-revision action');

  // X close path of a picker opened from a rendered analysis.
  window.showRevisionPickerFromAnalysis();
  eq(ui.getModal().active, true, 'the rendered analysis reopens the picker');
  click('#modalClose');
  eq(ui.getModal().active, false, 'the X button closes the analysis picker');
  deepEq(historyTuples(), renderedHistory, 'cancelling the analysis picker adds no history stop and rolls nothing back');
  eq(ui.historyIndex(), 0, 'cancelling the analysis picker keeps the rendered stop current');
  eq(ui.getCurrentAnalysis().branch, 'main', 'cancelling the analysis picker keeps the rendered revision');
  eq(ui.getDetailHeader().repo, renderedHeader.repo, 'cancelling the analysis picker keeps the header unchanged');
  eq(nodes.get('#modelsOverviewPanel').innerHTML, rendered, 'cancelling the analysis picker keeps the rendered view unchanged');
  ok(!/Select a revision|loading-state/.test(nodes.get('#modelsOverviewPanel').innerHTML), 'cancelling the analysis picker renders neither a pending prompt nor a spinner');

  // Escape close path behaves the same way.
  window.showRevisionPickerFromAnalysis();
  eq(ui.getModal().active, true, 'the rendered analysis picker reopens after cancel');
  pressEscape();
  eq(ui.getModal().active, false, 'Escape closes the analysis picker');
  deepEq(historyTuples(), renderedHistory, 'Escape on the analysis picker adds no history stop');
  eq(ui.historyIndex(), 0, 'Escape on the analysis picker keeps the rendered stop current');
  eq(ui.getCurrentAnalysis().branch, 'main', 'Escape on the analysis picker keeps the rendered revision');
  eq(nodes.get('#modelsOverviewPanel').innerHTML, rendered, 'Escape on the analysis picker keeps the rendered view unchanged');
}

async function testUnrelatedModalSettlesPendingRevisionPicker() {
  resetTest();
  analyzeImpl = (rawPath) => {
    const data = defaultAnalyze(rawPath);
    data.refs = [{ name: 'main', type: 'branch' }, { name: 'dev', type: 'branch' }];
    return data;
  };

  // A resolved view, then a pending navigation whose repository has multiple
  // refs: the real picker opens and the pane shows its loading spinner.
  await ui.openRepositoryInDetails('owner/previous', 'model', 'main', 'push');
  await ui.openRepositoryInDetails('owner/pending', 'model', null, 'push');
  eq(ui.getModal().active, true, 'the initial pending picker opens before the unrelated modal');
  match(nodes.get('#modelsOverviewPanel').innerHTML, /Analyzing owner\/pending/, 'the pending picker leaves the pane loading');

  // An unrelated dialog (for example a finished cache rebuild) replaces the
  // picker. It must settle the pending navigation instead of silently
  // discarding the picker identity and leaving the spinner behind.
  ui.showModal('Rebuild Complete', '<p>Rebuild finished</p>');
  eq(ui.getModal().active, true, 'the unrelated modal opens over the pending navigation');
  eq(ui.getModal().title, 'Rebuild Complete', 'the unrelated modal replaces the revision picker');
  ok(!/loading-state|Analyzing/.test(nodes.get('#modelsOverviewPanel').innerHTML), 'superseding the pending picker clears the loading spinner');
  match(nodes.get('#modelsOverviewPanel').innerHTML, /Select a revision/, 'superseding the pending picker renders the explicit pending prompt');
  match(nodes.get('#modelsOverviewPanel').innerHTML, /reopenRevisionPicker\(\)/, 'the settled pending prompt can reopen the picker');
  deepEq(
    historyTuples(),
    [['owner/previous', 'model', 'main'], ['owner/pending', 'model', null]],
    'superseding the picker keeps both history stops and no selection'
  );
  eq(ui.historyIndex(), 1, 'the pending stop stays current after supersession');
  eq(ui.getDetailHeader().repo, 'owner/pending', 'the header agrees with the pending history entry after supersession');
  eq(nodes.get('#modelsDetailBack').disabled, false, 'Back stays enabled after supersession');
  eq(nodes.get('#modelsDetailForward').disabled, true, 'Forward stays disabled at the latest pending stop');
  deepEq(
    { ...ui.getSelectionState() },
    { repo: 'owner/pending', type: 'model', isDataset: false, branch: null },
    'supersession leaves no revision selected while the pending repository/type stay resolved'
  );

  // Closing the unrelated modal must leave the coherent pending state, not the
  // indefinite loading state.
  click('#modalClose');
  eq(ui.getModal().active, false, 'the X button closes the unrelated modal');
  ok(!/loading-state|Analyzing/.test(nodes.get('#modelsOverviewPanel').innerHTML), 'closing the unrelated modal leaves no loading spinner');
  match(nodes.get('#modelsOverviewPanel').innerHTML, /Select a revision/, 'closing the unrelated modal keeps the explicit pending prompt');
  deepEq(
    historyTuples(),
    [['owner/previous', 'model', 'main'], ['owner/pending', 'model', null]],
    'closing the unrelated modal does not rewrite history'
  );
  eq(ui.historyIndex(), 1, 'closing the unrelated modal keeps the pending stop current');
  eq(ui.getDetailHeader().repo, 'owner/pending', 'closing the unrelated modal keeps the header coherent');
  eq(nodes.get('#modelsDetailBack').disabled, false, 'Back stays enabled after closing the unrelated modal');
  eq(nodes.get('#modelsDetailForward').disabled, true, 'Forward stays disabled after closing the unrelated modal');

  // The pending stop can still be reopened and resolved normally.
  ui.reopenRevisionPicker();
  eq(ui.getModal().active, true, 'the settled prompt reopens the picker after the unrelated modal');
  match(ui.getModal().body, /data-revision="dev"/, 'the reopened picker lists the revisions again');
  window.selectRevision('dev', false, 'owner/pending');
  await flush();
  eq(ui.getModal().active, false, 'selecting a revision closes the reopened picker');
  deepEq(
    historyTuples(),
    [['owner/previous', 'model', 'main'], ['owner/pending', 'model', 'dev']],
    'the pending stop resolves normally after the unrelated modal'
  );
  eq(ui.getCurrentAnalysis().branch, 'dev', 'the revision selected after supersession is rendered');

  // Escape on the unrelated modal follows the same coherent path.
  await ui.openRepositoryInDetails('owner/pending2', 'model', null, 'push');
  eq(ui.getModal().active, true, 'a second initial pending picker opens');
  match(nodes.get('#modelsOverviewPanel').innerHTML, /Analyzing owner\/pending2/, 'the second pending picker leaves the pane loading');
  ui.showModal('Rebuild Complete', '<p>Rebuild finished</p>');
  match(nodes.get('#modelsOverviewPanel').innerHTML, /Select a revision/, 'supersession settles the second pending picker');
  pressEscape();
  eq(ui.getModal().active, false, 'Escape closes the unrelated modal');
  ok(!/loading-state|Analyzing/.test(nodes.get('#modelsOverviewPanel').innerHTML), 'Escape close leaves no loading spinner');
  match(nodes.get('#modelsOverviewPanel').innerHTML, /Select a revision/, 'Escape close keeps the explicit pending prompt');
  eq(ui.currentRepoHistoryEntry().repo, 'owner/pending2', 'Escape close keeps the second pending stop current');
  eq(ui.currentRepoHistoryEntry().revision, null, 'Escape close does not invent a revision selection');
}

async function testUnrelatedModalKeepsAnalysisRevisionPickerView() {
  resetTest();
  analyzeImpl = (rawPath) => {
    const data = defaultAnalyze(rawPath);
    data.refs = [{ name: 'main', type: 'branch' }, { name: 'dev', type: 'branch' }];
    return data;
  };
  await ui.openRepositoryInDetails('owner/model', 'model', 'main', 'push');

  const rendered = nodes.get('#modelsOverviewPanel').innerHTML;
  const renderedHistory = historyTuples();
  const renderedHeader = ui.getDetailHeader();
  const renderedSelection = { ...ui.getSelectionState() };
  ok(rendered.includes('showRevisionPickerFromAnalysis'), 'the rendered analysis exposes the change-revision action');

  // A picker reopened from a rendered analysis is a view transition. An
  // unrelated modal that replaces it must not settle anything or touch the
  // rendered analysis.
  window.showRevisionPickerFromAnalysis();
  eq(ui.getModal().active, true, 'the analysis picker opens');
  ui.showModal('Rebuild Complete', '<p>Rebuild finished</p>');
  eq(ui.getModal().active, true, 'the unrelated modal replaces the analysis picker');
  eq(nodes.get('#modelsOverviewPanel').innerHTML, rendered, 'an unrelated modal leaves the rendered analysis unchanged');
  ok(!/Select a revision|loading-state/.test(nodes.get('#modelsOverviewPanel').innerHTML), 'an unrelated modal renders neither a pending prompt nor a spinner for the analysis picker');
  deepEq(historyTuples(), renderedHistory, 'an unrelated modal adds no history stop for the analysis picker');
  eq(ui.getDetailHeader().repo, renderedHeader.repo, 'an unrelated modal keeps the analysis header unchanged');
  deepEq({ ...ui.getSelectionState() }, renderedSelection, 'an unrelated modal keeps the rendered selection state unchanged');

  click('#modalClose');
  eq(ui.getModal().active, false, 'the unrelated modal closes');
  eq(nodes.get('#modelsOverviewPanel').innerHTML, rendered, 'closing the unrelated modal leaves the rendered analysis unchanged');
  ok(!/Select a revision|loading-state/.test(nodes.get('#modelsOverviewPanel').innerHTML), 'closing the unrelated modal renders neither a pending prompt nor a spinner for the analysis picker');
  deepEq(historyTuples(), renderedHistory, 'closing the unrelated modal keeps the analysis history unchanged');
}

async function testRevisionChangeFromRenderedAnalysis() {
  resetTest();
  analyzeImpl = (rawPath) => {
    const data = defaultAnalyze(rawPath);
    data.refs = [{ name: 'main', type: 'branch' }, { name: 'dev', type: 'branch' }];
    return data;
  };

  // The initial pending picker has no rendered view yet. Choosing main refines
  // the one pending entry instead of adding a stop.
  await ui.openRepositoryInDetails('owner/model', 'model', null, 'push');
  eq(ui.history().length, 1, 'initial revision navigation creates one pending stop');
  eq(ui.currentRepoHistoryEntry().revision, null, 'initial picker keeps the entry pending');
  match(nodes.get('#modalBody').innerHTML, /data-revision="main"/, 'initial picker lists main');

  calls = [];
  window.selectRevision('main', false, 'owner/model');
  await flush();
  deepEq(
    historyTuples(),
    [['owner/model', 'model', 'main']],
    'initial picker selection refines the pending entry into one main stop'
  );
  eq(lastAnalyzePath(), '/api/analyze/owner/model?type=model&revision=main', 'initial picker selection loads main');
  match(nodes.get('#modelsOverviewPanel').innerHTML, /onclick="showRevisionPickerFromAnalysis\(\)"/, 'rendered analysis header exposes the change picker link');

  // The rendered analysis header's change link reopens the picker for a view
  // that is already a history stop; choosing dev must preserve main.
  calls = [];
  window.showRevisionPickerFromAnalysis();
  match(nodes.get('#modalBody').innerHTML, /data-revision="dev"/, 'rendered analysis picker lists dev');

  window.selectRevision('dev', false, 'owner/model');
  await flush();
  deepEq(
    historyTuples(),
    [['owner/model', 'model', 'main'], ['owner/model', 'model', 'dev']],
    'rendered analysis revision change pushes dev after main'
  );
  eq(ui.historyIndex(), 1, 'rendered analysis revision change is the current stop');
  eq(ui.currentRepoHistoryEntry().revision, 'dev', 'the current stop is dev');
  eq(ui.getCurrentAnalysis().branch, 'dev', 'the current analysis renders dev');
  eq(lastAnalyzePath(), '/api/analyze/owner/model?type=model&revision=dev', 'revision change loads dev');
  eq(nodes.get('#modelsDetailBack').disabled, false, 'Back remains enabled after the revision change');

  calls = [];
  await ui.traverseRepoHistory(-1);
  eq(ui.historyIndex(), 0, 'Back returns to the preserved main stop');
  eq(ui.currentRepoHistoryEntry().revision, 'main', 'the current stop is main again');
  eq(ui.getCurrentAnalysis().branch, 'main', 'the rendered analysis is main again');
  eq(lastAnalyzePath(), '/api/analyze/owner/model?type=model&revision=main', 'Back reloads the main analysis');
  eq(nodes.get('#modelsDetailBack').disabled, true, 'Back is disabled at the first stop');
  eq(nodes.get('#modelsDetailForward').disabled, false, 'Forward is enabled after returning to main');

  // Re-selecting the revision already rendered refines the current stop
  // instead of duplicating it, and keeps the Forward branch intact.
  calls = [];
  window.showRevisionPickerFromAnalysis();
  window.selectRevision('main', false, 'owner/model');
  await flush();
  deepEq(
    historyTuples(),
    [['owner/model', 'model', 'main'], ['owner/model', 'model', 'dev']],
    're-selecting the rendered revision does not duplicate the stop'
  );
  eq(ui.historyIndex(), 0, 're-selecting the rendered revision keeps the current stop');
  eq(nodes.get('#modelsDetailForward').disabled, false, 'the preserved Forward stop remains reachable');

  calls = [];
  await ui.traverseRepoHistory(1);
  eq(ui.historyIndex(), 1, 'Forward returns to dev');
  eq(lastAnalyzePath(), '/api/analyze/owner/model?type=model&revision=dev', 'Forward reloads the dev analysis');
}

async function testRevisionPickerScopedToItsNavigation() {
  resetTest();
  analyzeImpl = (rawPath) => {
    const data = defaultAnalyze(rawPath);
    if (data.repo === 'owner/a' || data.repo === 'owner/c') {
      data.refs = [{ name: 'main', type: 'branch' }, { name: 'dev', type: 'branch' }];
    }
    return data;
  };

  // Open the picker for A, then leave its revision unresolved.
  await ui.openRepositoryInDetails('owner/a', 'model', null, 'push');
  eq(ui.getModal().active, true, 'A revision picker opens');
  eq(ui.currentRepoHistoryEntry().repo, 'owner/a', 'A pending stop is current while its picker is open');
  eq(ui.currentRepoHistoryEntry().revision, null, 'A pending stop has no revision yet');

  // Start a new repository transition to B. B has one ref, so it resolves
  // without opening a replacement picker.
  const navigation = ui.openRepositoryInDetails('owner/b', 'model', null, 'push');
  eq(ui.getModal().active, false, 'a new repository transition closes the A picker immediately');
  await flush();
  eq(ui.currentRepoHistoryEntry().repo, 'owner/b', 'B becomes the current history stop');
  eq(ui.currentRepoHistoryEntry().revision, 'main', 'B resolves to its default revision');
  eq(ui.getCurrentAnalysis().repo, 'owner/b', 'B is the rendered analysis');
  await navigation;

  const beforeHistory = historyTuples();
  const beforeSelection = { ...ui.getSelectionState() };

  // A stale A button call must be ignored: it belongs to the superseded
  // navigation and would otherwise replace B's stop with A at dev.
  window.selectRevision('dev', false, 'owner/a');
  await flush();
  eq(ui.currentRepoHistoryEntry().repo, 'owner/b', 'stale A selection leaves B as the current stop');
  eq(ui.currentRepoHistoryEntry().revision, 'main', 'stale A selection leaves B on its revision');
  eq(ui.getCurrentAnalysis().repo, 'owner/b', 'stale A selection leaves B rendered');
  deepEq(historyTuples(), beforeHistory, 'stale A selection does not rewrite history');
  deepEq({ ...ui.getSelectionState() }, beforeSelection, 'stale A selection does not change the selection state');

  // Normal selection still applies while its own navigation is current.
  await ui.openRepositoryInDetails('owner/c', 'model', null, 'push');
  eq(ui.getModal().active, true, 'C revision picker opens for the current navigation');
  window.selectRevision('dev', false, 'owner/c');
  await flush();
  eq(ui.getModal().active, false, 'C selection closes its picker');
  eq(ui.currentRepoHistoryEntry().repo, 'owner/c', 'C remains the current stop after selection');
  eq(ui.currentRepoHistoryEntry().revision, 'dev', 'C selection resolves the same stop to dev');
  eq(ui.getCurrentAnalysis().branch, 'dev', 'C selection renders the selected revision');
}

async function testRefreshInFlightNavigation() {
  resetTest();
  await ui.openRepositoryInDetails('owner/previous', 'model', null, 'push');

  const pending = [];
  calls = [];
  analyzeImpl = (rawPath) => new Promise(resolve => {
    pending.push(() => resolve({ ...defaultAnalyze(rawPath), repo: 'owner/inflight' }));
  });
  const navigation = ui.openRepositoryInDetails('owner/inflight', 'model', null, 'push');
  await new Promise(resolve => setImmediate(resolve));
  eq(lastAnalyzePath(), '/api/analyze/owner/inflight?type=model', 'in-flight navigation starts against the new repo');

  calls = [];
  click('#analyzeBtn');
  await flush();
  eq(lastAnalyzePath(), '/api/analyze/owner/inflight?type=model', 'refresh during an in-flight navigation targets the pending repo');

  while (pending.length) pending.shift()();
  await navigation;
  await flush();
  eq(ui.currentRepoHistoryEntry().repo, 'owner/inflight', 'refresh keeps the in-flight repo current');
}

async function testDetailHeaderTracksTypedInFlightNavigation() {
  resetTest();
  await ui.openRepositoryInDetails('owner/previous', 'model', null, 'push');
  eq(ui.getDetailHeader().repo, 'owner/previous', 'rendered model view labels the previous repository');
  eq(ui.getDetailHeader().href, 'https://huggingface.co/owner/previous', 'rendered model view links the previous repository');

  // Start A -> B typed navigation and stop before the response resolves.
  const deferred = deferredAnalyze();
  const navigation = ui.openRepositoryInDetails('owner/next', 'dataset', null, 'push');
  await flush();
  eq(deferred.path(), '/api/analyze/owner/next?type=dataset', 'typed transition requests the target dataset namespace');
  eq(ui.getDetailHeader().repo, 'owner/next', 'header identifies the target while it loads');
  eq(ui.getDetailHeader().href, 'https://huggingface.co/datasets/owner/next', 'dataset Hub link targets the new repository while loading');
  ok(!/owner\/previous/.test(ui.getDetailHeader().href), 'in-flight dataset link never keeps the previous href');
  deepEq(
    { ...ui.getSelectionState() },
    { repo: 'owner/next', type: 'dataset', isDataset: true, branch: 'main' },
    'selection state reflects the target transition before the response'
  );

  deferred.settle.resolve();
  await navigation;
  await flush();
  eq(ui.getDetailHeader().repo, 'owner/next', 'header stays on the target after resolution');
  eq(ui.getDetailHeader().href, 'https://huggingface.co/datasets/owner/next', 'dataset Hub link stays correct after resolution');
  eq(ui.getDetailHeader().hidden, false, 'resolved dataset keeps the Hub action visible');
}

async function testDetailHeaderSurvivesGenericAnalyzeError() {
  resetTest();
  await ui.openRepositoryInDetails('owner/previous', 'model', null, 'push');

  const deferred = deferredAnalyze();
  const navigation = ui.openRepositoryInDetails('owner/failing', 'dataset', null, 'push');
  await flush();
  eq(ui.getDetailHeader().repo, 'owner/failing', 'loading target labels the failing repository');
  eq(ui.getDetailHeader().href, 'https://huggingface.co/datasets/owner/failing', 'loading target links the failing dataset');

  deferred.settle.reject(new Error('analysis unavailable'));
  await navigation;
  await flush();
  eq(ui.getDetailHeader().repo, 'owner/failing', 'generic analyze error leaves the header on the target repository');
  eq(ui.getDetailHeader().href, 'https://huggingface.co/datasets/owner/failing', 'generic analyze error keeps the target dataset Hub link');
  eq(ui.getDetailHeader().hidden, false, 'generic analyze error keeps the known target type visible');
  deepEq(
    { ...ui.getSelectionState() },
    { repo: 'owner/failing', type: 'dataset', isDataset: true, branch: 'main' },
    'generic analyze error leaves selection state on the target repository'
  );
  match(nodes.get('#modelsOverviewPanel').innerHTML, /Analysis Failed/, 'generic analyze error renders the failure state');
  ok(!/owner\/previous/.test(ui.getDetailHeader().repo + ui.getDetailHeader().href), 'generic analyze error never reverts to the previous repository');
}

async function testSearchRowDatasetHeaderDuringLoading() {
  resetTest();
  const deferred = deferredAnalyze();
  const row = makeNode({ dataset: { repoType: 'dataset' } });
  ui.modelsSelectRow(row, 'owner/data');
  await flush();

  eq(deferred.path(), '/api/analyze/owner/data?type=dataset', 'dataset search row requests the dataset namespace');
  eq(ui.getDetailHeader().repo, 'owner/data', 'dataset search row labels the target while loading');
  eq(ui.getDetailHeader().href, 'https://huggingface.co/datasets/owner/data', 'dataset search row uses the dataset Hub URL while loading');
  ok(ui.getDetailHeader().href !== 'https://huggingface.co/owner/data', 'dataset search row never builds the model Hub URL');
  eq(ui.getSelectionState().isDataset, true, 'dataset search row state is dataset during loading');
  eq(ui.getSelectionState().type, 'dataset', 'dataset search row type is dataset during loading');

  deferred.settle.resolve();
  await flush();
  eq(ui.getDetailHeader().href, 'https://huggingface.co/datasets/owner/data', 'dataset search row keeps the dataset Hub URL after resolution');
}

async function testUntypedNavigationHidesHubLinkUntilResolved() {
  resetTest();
  await ui.openRepositoryInDetails('owner/previous', 'model', null, 'push');
  ok(ui.getDetailHeader().href !== '', 'previous view exposes its Hub link');

  const deferred = deferredAnalyze();
  nodes.get('#analyzeInput').value = 'owner/auto';
  const navigation = ui.analyzeRepo();
  await flush();
  eq(deferred.path(), '/api/analyze/owner/auto', 'untyped Analyze-field transition keeps auto-detection');
  eq(ui.getDetailHeader().repo, 'owner/auto', 'untyped transition labels the target immediately');
  eq(ui.getDetailHeader().hidden, true, 'untyped transition hides the Hub action while the type is unknown');
  eq(ui.getDetailHeader().href, '', 'untyped transition clears the previous Hub href');
  ok(!/owner\/previous/.test(ui.getDetailHeader().href), 'untyped transition never keeps the previous href');
  eq(ui.getSelectionState().repo, 'owner/auto', 'untyped transition selects the target repository');
  eq(ui.getSelectionState().type, null, 'untyped transition leaves the namespace unresolved');

  deferred.settle.resolve();
  await navigation;
  await flush();
  eq(ui.getDetailHeader().repo, 'owner/auto', 'auto-detected view keeps the target repository');
  eq(ui.getDetailHeader().hidden, false, 'auto-detection reveals the Hub action once the type resolves');
  eq(ui.getDetailHeader().href, 'https://huggingface.co/owner/auto', 'auto-detected model resolves the model Hub link');
  eq(ui.getSelectionState().type, 'model', 'auto-detection resolves the selection type');
}

async function testHeaderResyncDuringInFlightNavigation() {
  resetTest();
  await ui.openRepositoryInDetails('owner/previous', 'model', null, 'push');

  const deferred = deferredAnalyze();
  const navigation = ui.openRepositoryInDetails('owner/next', 'dataset', null, 'push');
  await flush();
  eq(ui.getDetailHeader().repo, 'owner/next', 'in-flight target is selected before re-sync');

  // Settings and health can resolve after a navigation starts. Their re-sync
  // must use the target selection state rather than the previous view.
  await ui.loadStorageModeBadge();
  await flush();
  eq(ui.getDetailHeader().repo, 'owner/next', 'settings re-sync keeps the in-flight target repository');
  eq(ui.getDetailHeader().href, 'https://huggingface.co/datasets/owner/next', 'settings re-sync keeps the in-flight dataset link');

  await ui.loadAppVersion();
  await flush();
  eq(ui.getDetailHeader().repo, 'owner/next', 'health re-sync keeps the in-flight target repository');
  eq(ui.getDetailHeader().href, 'https://huggingface.co/datasets/owner/next', 'health re-sync keeps the in-flight dataset link');

  deferred.settle.resolve();
  await navigation;
  await flush();
  eq(ui.getDetailHeader().repo, 'owner/next', 're-synced target survives response resolution');
  eq(ui.getDetailHeader().href, 'https://huggingface.co/datasets/owner/next', 're-synced dataset link survives response resolution');
}

async function testSearchRowAndDetailHeader() {
  resetTest();
  const row = makeNode({ dataset: { repoType: 'dataset' } });
  ui.modelsSelectRow(row, 'owner/data');
  await flush();
  eq(lastAnalyzePath(), '/api/analyze/owner/data?type=dataset', 'dataset search rows keep their explicit type');

  resetTest();
  ui.state.settings = { endpoint: 'https://hf.example.test:443' };
  await ui.openRepositoryInDetails('owner/model', 'dataset', null, 'push');
  eq(ui.getDetailHeader().href, 'https://hf.example.test/datasets/owner/model', 'external Hub action uses the configured endpoint');
  eq(ui.getDetailHeader().repo, 'owner/model', 'detail header keeps the repository identity');
}

async function testDetailHeaderEndpointFromHealth() {
  const endpoint = fixture.pathPrefixedEndpoint;

  // Counterexample: a custom endpoint is configured server-side, but only
  // /api/health reports it; /api/settings is empty or failed. Before health
  // resolves, the Hub action must stay hidden instead of guessing the default.
  resetTest();
  ui.state.settings = {};
  ui.state.settingsLoadFailed = true;
  healthImpl = () => ({ status: 'ok', version: 'test', endpoint });

  await ui.openRepositoryInDetails('owner/model', 'model', null, 'push');
  await flush();
  eq(ui.getDetailHeader().repo, 'owner/model', 'detail header still shows the repository without endpoint settings');
  eq(ui.getDetailHeader().hidden, true, 'Hub action stays hidden while the endpoint is unknown');
  eq(ui.getDetailHeader().href, '', 'Hub action keeps no guessed href while the endpoint is unknown');
  ok(!/huggingface\.co/.test(ui.getDetailHeader().href), 'Hub action never guesses huggingface.co');

  await ui.loadAppVersion();
  await flush();
  eq(ui.getDetailHeader().hidden, false, 'health endpoint reveals the Hub action');
  eq(ui.getDetailHeader().href, `${endpoint}/owner/model`, 'health endpoint including its path prefix drives the Hub action');
  ok(!/huggingface\.co/.test(ui.getDetailHeader().href), 'custom health endpoint never falls back to huggingface.co');

  // Empty server config: health reports the public Hub, so the link works and
  // the dataset namespace prefix is preserved.
  resetTest();
  ui.state.settings = {};
  ui.state.settingsLoadFailed = true;
  healthImpl = () => ({ status: 'ok', version: 'test', endpoint: 'https://huggingface.co' });

  await ui.openRepositoryInDetails('owner/model', 'dataset', null, 'push');
  await flush();
  eq(ui.getDetailHeader().hidden, true, 'Hub action waits for the health response before using the default endpoint');
  await ui.loadAppVersion();
  await flush();
  eq(ui.getDetailHeader().href, 'https://huggingface.co/datasets/owner/model', 'default health endpoint drives the dataset Hub action');
  eq(ui.getDetailHeader().hidden, false, 'default health endpoint reveals the dataset Hub action');
}

async function testDetailHeaderEndpointFromSettings() {
  const customEndpoint = 'https://hf.example.test';

  // A custom endpoint arrived from /api/health, then a settings load reports
  // the default (empty) endpoint. The stale health value must not keep driving
  // the Hub action.
  resetTest();
  ui.state.settings = {};
  ui.state.settingsLoadFailed = true;
  healthImpl = () => ({ status: 'ok', version: 'test', endpoint: customEndpoint });
  settingsImpl = () => ({ endpoint: '' });

  await ui.openRepositoryInDetails('owner/model', 'model', null, 'push');
  await ui.loadAppVersion();
  await flush();
  eq(ui.getDetailHeader().href, `${customEndpoint}/owner/model`, 'health endpoint drives the Hub action before settings load');

  await ui.loadStorageModeBadge();
  await flush();
  eq(ui.getDetailHeader().href, 'https://huggingface.co/owner/model', 'default settings endpoint replaces the stale health endpoint');
  eq(ui.getDetailHeader().hidden, false, 'default settings endpoint keeps the Hub action visible');
  ok(!/hf\.example\.test/.test(ui.getDetailHeader().href), 'stale custom health endpoint no longer drives the Hub action');

  // Health failed or returned no endpoint, then a settings load reports the
  // default endpoint. The default is known, so the action must appear instead
  // of staying hidden.
  resetTest();
  ui.state.settings = {};
  ui.state.settingsLoadFailed = true;
  healthImpl = () => ({ status: 'ok', version: 'test', endpoint: '' });
  settingsImpl = () => ({ endpoint: '' });

  await ui.openRepositoryInDetails('owner/model', 'model', null, 'push');
  await flush();
  eq(ui.getDetailHeader().hidden, true, 'Hub action stays hidden while the endpoint is unknown');

  await ui.loadAppVersion();
  await flush();
  eq(ui.getDetailHeader().hidden, true, 'empty health endpoint leaves the Hub action hidden');

  await ui.loadStorageModeBadge();
  await flush();
  eq(ui.getDetailHeader().hidden, false, 'default settings endpoint reveals the Hub action');
  eq(ui.getDetailHeader().href, 'https://huggingface.co/owner/model', 'default settings endpoint supplies the Hub href');
}

async function testSettingsEndpointAuthoritativeOverLateHealth() {
  const customEndpoint = 'https://hf.example.test';

  resetTest();
  ui.state.settings = {};
  ui.state.settingsLoadFailed = true;
  settingsImpl = () => ({ endpoint: '' });

  // Health snapshotted the custom endpoint at startup but its response is
  // deferred. Settings resolve first with the default (empty) preference.
  let settleHealth;
  healthImpl = () => new Promise(resolve => { settleHealth = resolve; });
  const health = ui.loadAppVersion();

  await ui.openRepositoryInDetails('owner/model', 'model', null, 'push');
  await flush();
  eq(ui.getDetailHeader().hidden, true, 'Hub action stays hidden until settings or health supplies an endpoint');

  await ui.loadStorageModeBadge();
  await flush();
  eq(ui.state.serverEndpoint, 'https://huggingface.co', 'default settings endpoint is remembered');
  eq(ui.getDetailHeader().href, 'https://huggingface.co/owner/model', 'default settings endpoint drives the Hub action before health resolves');

  // The late health response must not restore its stale custom snapshot over
  // the settings-authoritative default.
  settleHealth({ status: 'ok', version: 'test', endpoint: customEndpoint });
  await health;
  await flush();

  eq(ui.state.serverEndpoint, 'https://huggingface.co', 'late health response does not overwrite the remembered endpoint');
  eq(ui.getDetailHeader().href, 'https://huggingface.co/owner/model', 'late health endpoint never replaces the settings-authoritative link');
  ok(!/hf\.example\.test/.test(ui.getDetailHeader().href), 'late custom health endpoint never appears in the Hub link');
}

async function testNavigationPreservesCommunityPanel() {
  resetTest();

  // Establish the Community scope for this repository first: a scope change
  // legitimately clears Community state, so this test exercises an ordinary
  // navigation of the same repository view.
  await ui.openRepositoryInDetails('owner/model', 'model', 'main', 'push');

  const overview = nodes.get('#modelsOverviewPanel');
  const community = nodes.get('#modelsCommunityPanel');
  const shared = nodes.get('#analyzeResult');
  const seeded = '<div class="community-view">preserved community content</div>';
  community.innerHTML = seeded;

  const pending = ui.openRepositoryInDetails('owner/model', 'model', 'main', 'push');
  // loadRepositoryDetails writes its loading state synchronously, before the
  // first await, so the Overview panel must already own it here.
  match(overview.innerHTML, /Analyzing owner\/model/, 'loading state renders in the Overview panel');
  ok(!/Analyzing|loading-state/.test(shared.innerHTML), 'navigation leaves the shared result container untouched');
  eq(community.innerHTML, seeded, 'Community panel survives the loading state');

  await pending;
  await flush();
  match(overview.innerHTML, /analysis-card/, 'analysis result renders in the Overview panel');
  eq(community.innerHTML, seeded, 'Community panel is preserved across repository navigation');
  eq(shared.innerHTML, '', 'shared result container is not the render target');
}

(async () => {
  await testAnalyzeFieldAutoDetectAndRefresh();
  await testDatasetOnlyAutoDetect();
  await testReadmeServerLinks();
  await testReadmeClickDelegation();
  await testReadmePreviewContract();
  await testDualNamespaceSelection();
  await testLineageAndRelatedDownloads();
  await testSameRepoModelDatasetHistory();
  await testHistoryTraversalAndForwardTruncation();
  await testSameRepoRootKeepsRevisionStop();
  await testSameRepoRootLinkKeepsResolvedRevision();
  await testRefreshPendingRevisionPicker();
  await testCancelInitialRevisionPickerWithCloseButton();
  await testCancelInitialRevisionPickerWithEscape();
  await testCancelAnalysisRevisionPickerKeepsRenderedView();
  await testUnrelatedModalSettlesPendingRevisionPicker();
  await testUnrelatedModalKeepsAnalysisRevisionPickerView();
  await testRevisionChangeFromRenderedAnalysis();
  await testRevisionPickerScopedToItsNavigation();
  await testRefreshInFlightNavigation();
  await testDetailHeaderTracksTypedInFlightNavigation();
  await testDetailHeaderSurvivesGenericAnalyzeError();
  await testSearchRowDatasetHeaderDuringLoading();
  await testUntypedNavigationHidesHubLinkUntilResolved();
  await testHeaderResyncDuringInFlightNavigation();
  await testSearchRowAndDetailHeader();
  await testDetailHeaderEndpointFromHealth();
  await testDetailHeaderEndpointFromSettings();
  await testSettingsEndpointAuthoritativeOverLateHealth();
  await testNavigationPreservesCommunityPanel();
  console.log(`PASS: ${assertions} navigation/history/readme assertions against real app.js`);
})().catch(error => {
  console.error('navigation-ui FAILED:', error && error.message ? error.message : error);
  if (error && error.stack) console.error(error.stack);
  process.exitCode = 1;
});
