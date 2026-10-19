// Run through TestNavigationUIHistory: real app.js navigation/history/classifier.
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
if (!fixture.serverHtml || !fixture.markdown || !fixture.serverReservedRoutes ||
    !fixture.pathPrefixedServerHtml || !fixture.pathPrefixedMarkdown) {
  console.error('navigation-ui: HFDESK_NAV_FIXTURE is missing server/client README fixtures');
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
  '#analyzeInput', '#analyzeBtn', '#analyzeResult', '#modelsDetailHeader',
  '#modelsDetailRepo', '#modelsDetailHFLink', '#modelsDetailBack',
  '#modelsDetailForward', '#readmeSection', '#readmePreview', '#modalTitle',
  '#modalBody', '#modalBackdrop', '#modalClose', '#toastContainer', '#modelsSpinner',
  '#modelsResultList', '#modelsSearchClear', '#modelsSortSelect',
  '#modelsTypeSelect', '#modelsGguf', '#modelsDatasets', '#scrollArrowUp',
  '#scrollArrowDown'
]) {
  nodes.set(selector, makeNode());
}

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
    classifyHfRepoURL,
    readmeRepoLinkAttributes,
    renderMarkdownPreview,
    loadReadmePreview,
    handleReadmePreviewClick,
    loadAppVersion,
    renderRelatedDownloads,
    analyzeRelatedRepo: window.analyzeRelatedRepo,
    selectAnalysisType: window.selectAnalysisType,
    modelsSelectRow: window.modelsSelectRow,
    initModelsPage,
    initModal,
    reopenRevisionPicker: window.reopenRevisionPicker,
    showRevisionPickerFromAnalysis: window.showRevisionPickerFromAnalysis,
    readmeReservedRoutes: () => Array.from(README_RESERVED_MODEL_ROUTES),
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
  ui.state.settings = { endpoint: fixture.endpoint };
  ui.state.settingsLoadFailed = false;
  ui.state.serverEndpoint = '';
  nodes.get('#analyzeInput').value = '';
  nodes.get('#analyzeResult').innerHTML = '';
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
function clickEvent(anchor, hasInternalLink = true) {
  return {
    target: { closest() { return hasInternalLink ? anchor : null; } },
    defaultPrevented: false,
    button: 0,
    metaKey: false,
    ctrlKey: false,
    shiftKey: false,
    altKey: false,
    preventDefault() { this.defaultPrevented = true; }
  };
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

async function testReadmeLinkNavigation() {
  resetTest();

  const fallback = ui.renderMarkdownPreview(
    '[model](https://huggingface.co/owner/model)\n\n' +
    '[data](https://huggingface.co/datasets/owner/data)\n\n' +
    '[dev](https://huggingface.co/owner/model/tree/dev)\n\n' +
    '[encoded](https://huggingface.co/owner/model/tree/refs%2Fpr%2F1)\n\n' +
    '[nested](https://huggingface.co/owner/model/tree/main/folder/file.md)\n\n' +
    '[pr](https://huggingface.co/owner/model/tree/refs/pr/1)',
    fixture.baseRawURL,
    fixture.baseBlobURL
  );
  match(fallback, /data-hf-repo="owner\/model"/, 'model README link must be classified internal');
  match(fallback, /data-hf-repo-type="dataset"/, 'dataset README link must be classified internal');
  match(fallback, /data-hf-repo-revision="dev"/, 'revision README link must keep its revision');
  match(fallback, /data-hf-repo-revision="refs\/pr\/1"/, 'encoded revision slash must stay within one revision');
  const nestedAnchor = fallback.match(/<a href="[^"]*tree\/main\/folder\/file\.md"[^>]*>/)?.[0] || '';
  ok(nestedAnchor !== '' && !/data-hf-repo/.test(nestedAnchor), 'tree subpath README link must stay external');
  const rawRefsAnchor = fallback.match(/<a href="[^"]*tree\/refs\/pr\/1"[^>]*>/)?.[0] || '';
  ok(rawRefsAnchor !== '' && !/data-hf-repo/.test(rawRefsAnchor), 'raw multi-segment revision must stay external');

  calls = [];
  const modelEvent = clickEvent(fakeAnchor({
    'data-hf-repo': 'owner/model',
    'data-hf-repo-type': 'model'
  }));
  ui.handleReadmePreviewClick(modelEvent);
  eq(modelEvent.defaultPrevented, true, 'model README link must prevent the external navigation');
  await flush();
  eq(lastAnalyzePath(), '/api/analyze/owner/model?type=model', 'model README link navigates internally with an explicit model type');
  eq(ui.history().at(-1).repo, 'owner/model', 'model README link enters history');
  eq(ui.history().at(-1).repoType, 'model', 'model README link restores its model type');

  resetTest();
  calls = [];
  const datasetEvent = clickEvent(fakeAnchor({
    'data-hf-repo': 'owner/data',
    'data-hf-repo-type': 'dataset'
  }));
  ui.handleReadmePreviewClick(datasetEvent);
  eq(datasetEvent.defaultPrevented, true, 'dataset README link must prevent the external navigation');
  await flush();
  eq(lastAnalyzePath(), '/api/analyze/owner/data?type=dataset', 'dataset README link navigates internally with type=dataset');
  eq(ui.history().at(-1).repoType, 'dataset', 'dataset README link restores its dataset type');

  resetTest();
  calls = [];
  const revisionEvent = clickEvent(fakeAnchor({
    'data-hf-repo': 'owner/model',
    'data-hf-repo-type': 'model',
    'data-hf-repo-revision': 'dev'
  }));
  ui.handleReadmePreviewClick(revisionEvent);
  eq(revisionEvent.defaultPrevented, true, 'revision README link must prevent the external navigation');
  await flush();
  eq(lastAnalyzePath(), '/api/analyze/owner/model?type=model&revision=dev', 'revision README link keeps the revision');
  eq(ui.history().at(-1).revision, 'dev', 'revision README link restores the revision');
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
  match(nodes.get('#analyzeResult').innerHTML, /Selection Required/, 'dual repository renders the type selector');

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
  ok(!/Selection Required/.test(nodes.get('#analyzeResult').innerHTML), 'explicit model view does not loop back to type selection');
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

  const resultHtml = nodes.get('#analyzeResult').innerHTML;
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

  // The real README renderer emits a same-repo root link without a revision.
  const rootHtml = ui.renderMarkdownPreview(
    '[root](https://huggingface.co/owner/model)',
    fixture.baseRawURL,
    fixture.baseBlobURL
  );
  const rootLink = extractLinks(rootHtml).find(link => link.repo === 'owner/model');
  ok(rootLink, 'README root link must be classified internal');
  eq(rootLink.type, 'model', 'README root link keeps the model type');
  eq(rootLink.revision, null, 'README root link carries no revision');

  calls = [];
  const event = clickEvent(fakeAnchor({
    'data-hf-repo': rootLink.repo,
    'data-hf-repo-type': rootLink.type
  }));
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
  match(nodes.get('#analyzeResult').innerHTML, /Analyzing owner\/pending/, 'picker navigation leaves the pane loading before cancel');

  // The real X close path.
  click('#modalClose');
  eq(ui.getModal().active, false, 'the X button closes the picker modal');
  ok(!/loading-state|Analyzing/.test(nodes.get('#analyzeResult').innerHTML), 'cancel replaces the indefinite loading state');
  match(nodes.get('#analyzeResult').innerHTML, /Select a revision/, 'cancel renders an explicit pending prompt');
  match(nodes.get('#analyzeResult').innerHTML, /reopenRevisionPicker\(\)/, 'the pending prompt can reopen the picker');
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
  match(nodes.get('#analyzeResult').innerHTML, /onclick="showRevisionPickerFromAnalysis\(\)"/, 'the resolved view is no longer pending');
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
  ok(!/loading-state|Analyzing/.test(nodes.get('#analyzeResult').innerHTML), 'Escape replaces the indefinite loading state');
  match(nodes.get('#analyzeResult').innerHTML, /Select a revision/, 'Escape renders the explicit pending prompt');
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

  const rendered = nodes.get('#analyzeResult').innerHTML;
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
  eq(nodes.get('#analyzeResult').innerHTML, rendered, 'cancelling the analysis picker keeps the rendered view unchanged');
  ok(!/Select a revision|loading-state/.test(nodes.get('#analyzeResult').innerHTML), 'cancelling the analysis picker renders neither a pending prompt nor a spinner');

  // Escape close path behaves the same way.
  window.showRevisionPickerFromAnalysis();
  eq(ui.getModal().active, true, 'the rendered analysis picker reopens after cancel');
  pressEscape();
  eq(ui.getModal().active, false, 'Escape closes the analysis picker');
  deepEq(historyTuples(), renderedHistory, 'Escape on the analysis picker adds no history stop');
  eq(ui.historyIndex(), 0, 'Escape on the analysis picker keeps the rendered stop current');
  eq(ui.getCurrentAnalysis().branch, 'main', 'Escape on the analysis picker keeps the rendered revision');
  eq(nodes.get('#analyzeResult').innerHTML, rendered, 'Escape on the analysis picker keeps the rendered view unchanged');
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
  match(nodes.get('#analyzeResult').innerHTML, /onclick="showRevisionPickerFromAnalysis\(\)"/, 'rendered analysis header exposes the change picker link');

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

function testClassifierAndReservedRoutes() {
  resetTest();

  const reserved = Array.from(ui.readmeReservedRoutes()).sort();
  deepEq(reserved, fixture.serverReservedRoutes.slice().sort(), 'client and server reserved Hub route lists must stay identical');
  for (const route of ['hardware', 'inference', 'solutions', 'chat']) {
    ok(reserved.includes(route), `reserved Hub route ${route} must be rejected as an owner`);
  }

  for (const url of [
    'https://huggingface.co/hardware/a10',
    'https://huggingface.co/inference/models',
    'https://huggingface.co/solutions/models',
    'https://huggingface.co/chat/assistants',
    'https://huggingface.co/owner/model/blob/main/config.json',
    'https://huggingface.co/owner/model/resolve/main/model.gguf',
    'https://huggingface.co/owner/model/raw/main/config.json',
    'https://huggingface.co/owner/model/tree/main/folder/file.md',
    'https://huggingface.co/datasets/owner/data/tree/v1/train',
    'https://huggingface.co/owner/model/tree/refs/pr/1',
    'https://example.com/owner/model'
  ]) {
    eq(ui.classifyHfRepoURL(url, fixture.baseBlobURL), null, `${url} must stay external`);
  }
  ok(ui.classifyHfRepoURL('https://huggingface.co/datasets/hardware/a10', fixture.baseBlobURL), 'datasets namespace remains a repository even with a reserved-looking owner');
  deepEq(
    { ...ui.classifyHfRepoURL('https://huggingface.co/owner/model/tree/refs%2Fpr%2F1', fixture.baseBlobURL) },
    { repo: 'owner/model', repoType: 'model', revision: 'refs/pr/1' },
    'encoded revision slash remains one revision segment'
  );
  deepEq(
    { ...ui.classifyHfRepoURL('https://huggingface.co/datasets/owner/data/tree/v1', fixture.baseBlobURL) },
    { repo: 'owner/data', repoType: 'dataset', revision: 'v1' },
    'dataset tree revision remains internal'
  );

  // A click without the server/fallback-owned internal attribute is untouched.
  const externalEvent = clickEvent(fakeAnchor({}), false);
  ui.handleReadmePreviewClick(externalEvent);
  eq(externalEvent.defaultPrevented, false, 'unsupported external URL must stay external');
  eq(ui.history().length, 0, 'unsupported external URL must not enter history');

  // Custom configured endpoint.
  ui.state.settings = { endpoint: 'https://hf.example.test' };
  const custom = ui.classifyHfRepoURL('https://hf.example.test/owner/model/tree/dev', fixture.baseBlobURL);
  deepEq(
    { ...custom },
    { repo: 'owner/model', repoType: 'model', revision: 'dev' },
    'custom endpoint repository links are internal'
  );
  eq(ui.classifyHfRepoURL('https://huggingface.co/owner/model', fixture.baseBlobURL), null, 'default Hub links are external on a custom endpoint');

  // A configured endpoint path prefix must precede the repository path. The
  // same host without that prefix stays external, and the prefix must match a
  // whole path segment.
  ui.state.settings = { endpoint: fixture.pathPrefixedEndpoint };
  deepEq(
    { ...ui.classifyHfRepoURL(fixture.pathPrefixedEndpoint + '/owner/model/tree/dev', fixture.pathPrefixedBaseBlobURL) },
    { repo: 'owner/model', repoType: 'model', revision: 'dev' },
    'path-prefixed endpoint repository tree links are internal'
  );
  deepEq(
    { ...ui.classifyHfRepoURL(fixture.pathPrefixedEndpoint + '/datasets/owner/data/tree/v1', fixture.pathPrefixedBaseBlobURL) },
    { repo: 'owner/data', repoType: 'dataset', revision: 'v1' },
    'path-prefixed endpoint dataset tree revisions are internal'
  );
  eq(ui.classifyHfRepoURL('https://mirror.example/owner/model', fixture.pathPrefixedBaseBlobURL), null, 'same-host link without the endpoint prefix stays external');
  eq(ui.classifyHfRepoURL('https://mirror.example/hfold/owner/model', fixture.pathPrefixedBaseBlobURL), null, 'endpoint prefix must match a whole path segment');

  // Default ports are normalized the same way the server normalizes them.
  ui.state.settings = { endpoint: 'https://huggingface.co' };
  ok(ui.classifyHfRepoURL('https://huggingface.co:443/owner/model', fixture.baseBlobURL), 'explicit https default port is internal');
  ui.state.settings = { endpoint: 'https://huggingface.co:443' };
  ok(ui.classifyHfRepoURL('https://huggingface.co/owner/model', fixture.baseBlobURL), 'endpoint with an explicit default port is equivalent');
  ui.state.settings = { endpoint: 'http://hf.example.test:80' };
  ok(ui.classifyHfRepoURL('http://hf.example.test/owner/model', 'http://hf.example.test:80/owner/model/blob/main/'), 'explicit http default port is internal');
}

async function testServerClientEquivalence() {
  resetTest();
  ui.state.settings = { endpoint: fixture.endpoint };
  const clientHtml = ui.renderMarkdownPreview(fixture.markdown, fixture.baseRawURL, fixture.baseBlobURL);
  deepEq(extractLinks(clientHtml), extractLinks(fixture.serverHtml), 'server-rendered and client-fallback README links must agree');

  const internal = extractLinks(fixture.serverHtml).filter(link => link.repo);
  deepEq(
    internal.map(link => [link.repo, link.type, link.revision]),
    [
      ['owner/model', 'model', null],
      ['owner/data', 'dataset', null],
      ['owner/model', 'model', 'dev'],
      ['owner/model', 'model', 'refs/pr/1']
    ],
    'only repository roots and single-segment tree revisions become internal links'
  );

  ui.state.settings = { endpoint: fixture.pathPrefixedEndpoint };
  const prefixedClientHtml = ui.renderMarkdownPreview(
    fixture.pathPrefixedMarkdown,
    fixture.pathPrefixedBaseRawURL,
    fixture.pathPrefixedBaseBlobURL
  );
  deepEq(
    extractLinks(prefixedClientHtml),
    extractLinks(fixture.pathPrefixedServerHtml),
    'server-rendered and client-fallback README links must agree under a path-prefixed endpoint'
  );
  const prefixedInternal = extractLinks(fixture.pathPrefixedServerHtml).filter(link => link.repo);
  deepEq(
    prefixedInternal.map(link => [link.repo, link.type, link.revision]),
    [
      ['owner/model', 'model', null],
      ['owner/data', 'dataset', 'v1']
    ],
    'path-prefixed endpoint classifies only prefixed repository views as internal'
  );
}

async function testBaseBlobURLConsumption() {
  resetTest();
  const baseRaw = 'https://huggingface.co/owner/readme/resolve/main/';
  const baseBlob = 'https://huggingface.co/owner/readme/blob/main/';

  calls = [];
  readmeImpl = () => ({
    markdown: '[file](config.json)\n\n![logo](assets/logo.png)',
    baseRawURL: baseRaw,
    baseBlobURL: baseBlob
  });
  await ui.loadReadmePreview({ repo: 'owner/readme', branch: 'dev', is_dataset: true });
  eq(lastReadmePath(), '/api/readme/owner/readme?revision=dev&dataset=true', 'README request keeps revision and dataset');
  const fallbackHtml = nodes.get('#readmePreview').innerHTML;
  match(fallbackHtml, /href="https:\/\/huggingface\.co\/owner\/readme\/blob\/main\/config\.json"/, 'fallback links consume baseBlobURL');
  match(fallbackHtml, /src="https:\/\/huggingface\.co\/owner\/readme\/resolve\/main\/assets\/logo\.png"/, 'fallback images consume baseRawURL');

  readmeImpl = () => ({
    html: '<p class="server-markup">server</p>',
    markdown: '[file](config.json)',
    baseRawURL: baseRaw,
    baseBlobURL: baseBlob
  });
  await ui.loadReadmePreview({ repo: 'owner/readme', branch: 'main' });
  eq(nodes.get('#readmePreview').innerHTML, '<p class="server-markup">server</p>', 'server-rendered HTML is used verbatim');

  readmeImpl = () => ({ markdown: '[file](config.json)', baseRawURL: baseRaw });
  await ui.loadReadmePreview({ repo: 'owner/readme', branch: 'main' });
  match(nodes.get('#readmePreview').innerHTML, /href="https:\/\/huggingface\.co\/owner\/readme\/blob\/main\/config\.json"/, 'missing baseBlobURL derives the blob base');
}

async function testReadmeFallbackDerivesEndpointWithoutSettings() {
  resetTest();
  ui.state.settings = {};

  const endpoint = fixture.pathPrefixedEndpoint;
  const endpointOrigin = new URL(endpoint).origin;

  // Initial page load or a failed settings fetch leaves no configured endpoint.
  // A legacy server response has markdown but no html, so the client fallback
  // must recover the endpoint path prefix from baseBlobURL + the known repo.
  readmeImpl = () => ({
    markdown: `[model](${endpoint}/owner/model)\n\n[same-host](${endpointOrigin}/owner/model)`,
    baseRawURL: fixture.pathPrefixedBaseRawURL,
    baseBlobURL: fixture.pathPrefixedBaseBlobURL
  });
  await ui.loadReadmePreview({ repo: 'owner/model', branch: 'main' });

  const links = extractLinks(nodes.get('#readmePreview').innerHTML);
  const prefixed = links.find(link => link.href === `${endpoint}/owner/model`);
  const sameHost = links.find(link => link.href === `${endpointOrigin}/owner/model`);
  ok(prefixed, 'settings-less fallback renders the prefixed repository link');
  eq(prefixed.repo, 'owner/model', 'settings-less fallback classifies the prefixed link as owner/model');
  eq(prefixed.type, 'model', 'settings-less fallback keeps the model type');
  ok(sameHost, 'settings-less fallback renders the same-host link');
  eq(sameHost.repo, null, 'same-host link without the endpoint prefix stays external');

  calls = [];
  const event = clickEvent(fakeAnchor({
    'data-hf-repo': prefixed.repo,
    'data-hf-repo-type': prefixed.type
  }));
  ui.handleReadmePreviewClick(event);
  eq(event.defaultPrevented, true, 'settings-less prefixed README link prevents external navigation');
  await flush();
  eq(lastAnalyzePath(), '/api/analyze/owner/model?type=model', 'settings-less prefixed README link navigates to Model Details');

  // Older server responses omit baseBlobURL. The blob base derived from
  // baseRawURL must feed the same endpoint recovery, including the dataset
  // namespace.
  resetTest();
  ui.state.settings = {};
  readmeImpl = () => ({
    markdown: `[data](${endpoint}/datasets/owner/data)\n\n[same-host](${endpointOrigin}/datasets/owner/data)`,
    baseRawURL: `${endpoint}/datasets/owner/data/resolve/main/`
  });
  await ui.loadReadmePreview({ repo: 'owner/data', branch: 'main', is_dataset: true });

  const datasetLinks = extractLinks(nodes.get('#readmePreview').innerHTML);
  const datasetLink = datasetLinks.find(link => link.href === `${endpoint}/datasets/owner/data`);
  const externalDataset = datasetLinks.find(link => link.href === `${endpointOrigin}/datasets/owner/data`);
  ok(datasetLink, 'derived baseBlobURL still renders the prefixed dataset link');
  eq(datasetLink.repo, 'owner/data', 'derived baseBlobURL strips the endpoint prefix for datasets');
  eq(datasetLink.type, 'dataset', 'derived baseBlobURL keeps the dataset type');
  ok(externalDataset, 'derived baseBlobURL still renders the same-host dataset link');
  eq(externalDataset.repo, null, 'same-host dataset link without the endpoint prefix stays external');
}

async function testReadmeFallbackSafety() {
  resetTest();
  const baseRaw = 'https://huggingface.co/owner/readme/resolve/main/';
  const baseBlob = 'https://huggingface.co/owner/readme/blob/main/';

  // A server response with html:'' means sanitization removed everything. It
  // must never fall back to the raw markdown, even when that markdown contains
  // an executable link.
  readmeImpl = () => ({
    html: '',
    markdown: "[x](javascript:document.body.innerHTML='pwn')",
    baseRawURL: baseRaw,
    baseBlobURL: baseBlob
  });
  await ui.loadReadmePreview({ repo: 'owner/readme', branch: 'main' });
  eq(nodes.get('#readmePreview').innerHTML, '', 'empty server html suppresses the markdown fallback');
  eq(nodes.get('#readmeSection').hidden, true, 'empty server html hides the README section');
  ok(!/javascript:/i.test(nodes.get('#readmePreview').innerHTML), 'empty server html never emits javascript:');

  // Older servers have no html property, so the fallback renders markdown.
  // Unsafe schemes become plain escaped labels; safe URLs are single-escaped.
  resetTest();
  readmeImpl = () => ({
    markdown: [
      '[js](javascript:alert)',
      '[html](data:text/html,alert)',
      '[query](https://example.com/?a=1&b=2)',
      '![jsimg](javascript:alert)',
      '![dataimg](data:image/png;base64,AAAA)',
      '<img src=x onerror=alert(1)>'
    ].join('\n\n'),
    baseRawURL: baseRaw,
    baseBlobURL: baseBlob
  });
  await ui.loadReadmePreview({ repo: 'owner/readme', branch: 'main' });
  const fallbackHtml = nodes.get('#readmePreview').innerHTML;
  ok(!/href="javascript:/i.test(fallbackHtml), 'unsafe javascript links are not emitted');
  ok(!/href="data:/i.test(fallbackHtml), 'data: links are not emitted as anchors');
  ok(!/src="javascript:/i.test(fallbackHtml), 'unsafe javascript images are omitted');
  ok(!/src="data:text\/html/i.test(fallbackHtml), 'unsafe data images are omitted');
  match(fallbackHtml, />js</, 'unsafe link label remains visible as escaped text');
  match(fallbackHtml, /href="https:\/\/example\.com\/\?a=1&amp;b=2"/, 'safe query-string href is escaped exactly once');
  ok(!/&amp;amp;/.test(fallbackHtml), 'query-string URL is not double-escaped');
  match(fallbackHtml, /src="data:image\/png;base64,AAAA"/, 'safe data:image sources remain allowed');
  ok(!/<img src=x onerror/i.test(fallbackHtml), 'raw HTML is not emitted as markup');
  match(fallbackHtml, /&lt;img src=x onerror=alert\(1\)&gt;/, 'raw HTML remains escaped text');
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

(async () => {
  await testAnalyzeFieldAutoDetectAndRefresh();
  await testDatasetOnlyAutoDetect();
  await testReadmeLinkNavigation();
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
  await testRevisionChangeFromRenderedAnalysis();
  await testRefreshInFlightNavigation();
  testClassifierAndReservedRoutes();
  await testServerClientEquivalence();
  await testBaseBlobURLConsumption();
  await testReadmeFallbackDerivesEndpointWithoutSettings();
  await testReadmeFallbackSafety();
  await testSearchRowAndDetailHeader();
  await testDetailHeaderEndpointFromHealth();
  console.log(`PASS: ${assertions} navigation/history/classifier assertions against real app.js`);
})().catch(error => {
  console.error('navigation-ui FAILED:', error && error.message ? error.message : error);
  if (error && error.stack) console.error(error.stack);
  process.exitCode = 1;
});
