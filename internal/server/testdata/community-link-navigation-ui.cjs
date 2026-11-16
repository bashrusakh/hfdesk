// Run through TestCommunityLinkNavigationUI: real app.js Community link
// delegation against sanitized comment HTML.
//
// The harness loads the shipped app.js inside Node's VM with DOM and fetch
// stubs. The fixture HTML is produced by the real Go sanitizer, so the anchors
// carry exactly what the browser receives: the external href, no
// repository-navigation attributes. The harness drives the panel's registered
// click delegation, the shared navigation owner (openRepositoryInDetails), and
// the link classifier directly. No client rule is reimplemented here.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const fixture = JSON.parse(process.env.HFDESK_COMMUNITY_FIXTURE || '{}');
if (!fixture.communityHtml || !fixture.pathPrefixedCommunityHtml || !fixture.endpoint || !fixture.pathEndpoint) {
  console.error('community-link-navigation-ui: HFDESK_COMMUNITY_FIXTURE is missing the Community fixtures');
  process.exit(1);
}

let assertions = 0;
function eq(actual, expected, message) { assertions++; assert.equal(actual, expected, message); }
function ok(value, message) { assertions++; assert.ok(value, message); }
function eqClassified(actual, repo, type, message) {
  assertions++;
  assert.ok(actual, message);
  assert.equal(actual.repo, repo, `${message}: repo`);
  assert.equal(actual.type, type, `${message}: type`);
}

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
    scrollHeight: 0,
    clientHeight: 0,
    setAttribute(name, value) { this.attributes[name] = String(value); },
    removeAttribute(name) { delete this.attributes[name]; },
    getAttribute(name) { return Object.prototype.hasOwnProperty.call(this.attributes, name) ? this.attributes[name] : null; },
    handlers: {},
    addEventListener(type, fn) { (this.handlers[type] ||= []).push(fn); },
    appendChild(child) { return child; },
    focus() {},
    remove() {},
    scrollTo() {},
    querySelector() { return null; },
    querySelectorAll() { return []; },
    contains() { return false; }
  }, extra);
}

const nodes = new Map();
for (const selector of [
  '#analyzeInput',
  '#analyzeResult',
  '#modelsOverviewPanel',
  '#modelsCommunityPanel',
  '#modelsFilesPanel',
  '#modelsDetailTabs',
  '#modelsTabOverview',
  '#modelsTabCommunity',
  '#modelsTabFiles',
  '#modelsDetailHeader',
  '#modelsDetailRepo',
  '#modelsDetailHFLink',
  '#modelsDetailBack',
  '#modelsDetailForward',
  '#readmeSection',
  '#readmePreview',
  '#scrollArrowUp',
  '#scrollArrowDown',
  '#modalTitle',
  '#modalBody',
  '#modalBackdrop'
]) {
  nodes.set(selector, makeNode());
}

// The header action's href property mirrors its attribute, as in the browser.
const detailHfLinkNode = nodes.get('#modelsDetailHFLink');
detailHfLinkNode.attributes.href = '#';
Object.defineProperty(detailHfLinkNode, 'href', {
  get() {
    return Object.prototype.hasOwnProperty.call(this.attributes, 'href') ? this.attributes.href : '';
  },
  set(value) { this.attributes.href = String(value); }
});

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

// Navigation requests stay pending: the harness only needs the request the
// transition starts, not its rendered result.
const calls = [];
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
  fetch: async rawPath => {
    calls.push(String(rawPath));
    return new Promise(() => {});
  }
};

let source = fs.readFileSync(
  path.join(__dirname, '..', '..', 'assets', 'static', 'js', 'app.js'),
  'utf8'
);
const marker = '  // Start\n';
if (!source.includes(marker)) {
  console.error('community-link-navigation-ui: app.js injection marker is missing');
  process.exit(1);
}
const hook = `
  window.test = {
    state,
    initModelsPage,
    renderCommunityPanel,
    classifyHubRepoHref,
    handleCommunityLinkClick,
    activeTab: () => detailActiveTab,
    history: () => repoHistory.map(entry => ({ ...entry })),
    setCommunityFixture(html) {
      communityState.repo = 'owner/model';
      communityState.isDataset = false;
      communityState.listLoaded = false;
      communityState.listLoading = false;
      communityState.listError = '';
      communityState.view = 'thread';
      communityState.threadNum = 1;
      communityState.threadLoading = false;
      communityState.threadError = '';
      communityState.thread = {
        num: 1,
        title: 'Community fixture',
        author: 'tester',
        created_at: '',
        status: 'open',
        is_pull_request: false,
        events: [{ type: 'comment', author: 'tester', created_at: '', body_html: html }]
      };
      detailActiveTab = 'community';
      renderCommunityPanel();
    },
    resetNavigation() {
      analyzeRequestId++;
      readmeRequestId++;
      currentAnalysis = null;
      currentSelectedRepo = null;
      currentSelectedType = null;
      currentSelectedIsDataset = false;
      currentSelectedBranch = 'main';
      hasShownRevisionPicker = false;
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

function resetClicks() {
  calls.length = 0;
  ui.resetNavigation();
}
function analyzePaths() {
  return calls.filter(call => call.startsWith('/api/analyze/'));
}
function lastAnalyzePath() { return analyzePaths().at(-1); }

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
  while ((anchor = anchorRe.exec(String(html)))) {
    const attrs = {};
    const attrRe = /([a-zA-Z0-9:-]+)="([^"]*)"/g;
    let attr;
    while ((attr = attrRe.exec(anchor[1]))) attrs[attr[1].toLowerCase()] = decodeEntities(attr[2]);
    links.push({ href: attrs.href || '', attrs });
  }
  return links;
}

function fakeAnchor(attrs) {
  return {
    getAttribute(name) {
      return Object.prototype.hasOwnProperty.call(attrs, name) ? attrs[name] : null;
    }
  };
}
function clickEvent(anchor, hasLink = true, overrides = {}) {
  return Object.assign({
    target: { closest() { return hasLink ? anchor : null; } },
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

// The panel's registered delegation comes from initModelsPage; using it proves
// the handler is actually installed on the rendered panel.
function panelClick(anchor, overrides = {}) {
  const handlers = nodes.get('#modelsCommunityPanel').handlers.click || [];
  ok(handlers.length > 0, 'Community panel must have a click delegation');
  const event = clickEvent(anchor, true, overrides);
  handlers.at(-1)(event);
  return event;
}

function anchorFor(html, href) {
  const found = extractLinks(html).find(link => link.href === href);
  ok(found, `sanitized Community HTML must expose ${href}`);
  return fakeAnchor(found.attrs);
}

// --- Classifier: only repository roots on the configured endpoint are in-app.
ui.state.settings = { endpoint: fixture.endpoint };
ui.state.serverEndpoint = '';
ui.state.settingsEndpointResolved = true;

eqClassified(
  ui.classifyHubRepoHref(fixture.endpoint + '/owner/model'),
  'owner/model', 'model',
  'Hub model root is an in-app model view'
);
eqClassified(
  ui.classifyHubRepoHref(fixture.endpoint + '/datasets/owner/data'),
  'owner/data', 'dataset',
  'Hub dataset root is an in-app dataset view'
);
eqClassified(
  ui.classifyHubRepoHref(fixture.endpoint + '/owner/model/'),
  'owner/model', 'model',
  'a trailing slash does not change the repository root'
);
for (const [href, label] of [
  [fixture.endpoint + '/owner/model/blob/main/config.json', 'blob URL'],
  [fixture.endpoint + '/owner/model/tree/main', 'tree URL'],
  [fixture.endpoint + '/owner/model/tree/main/folder/file.md', 'nested tree URL'],
  [fixture.endpoint + '/owner/model/resolve/main/config.json', 'resolve URL'],
  [fixture.endpoint + '/owner/model/commit/abc123', 'commit URL'],
  [fixture.endpoint + '/owner/model/raw/main/config.json', 'raw URL'],
  [fixture.endpoint + '/papers/2401.04088', 'reserved Hub page'],
  [fixture.endpoint + '/owner', 'owner-only path'],
  [fixture.endpoint + '/owner/model/extra/path', 'path beyond a repository root'],
  ['https://example.com/owner/model', 'foreign host'],
  ['mailto:someone@example.com', 'mailto link'],
  ['javascript:alert(1)', 'javascript scheme'],
  ['#readme', 'in-page anchor']
]) {
  eq(ui.classifyHubRepoHref(href), null, `${label} must stay external`);
}

// The endpoint is server-owned: settings override health, health alone works,
// and an unknown endpoint never guesses huggingface.co.
ui.state.settings = {};
ui.state.serverEndpoint = fixture.endpoint;
eqClassified(
  ui.classifyHubRepoHref(fixture.endpoint + '/owner/model'),
  'owner/model', 'model',
  'the /api/health endpoint classifies links before settings load'
);
ui.state.serverEndpoint = '';
eq(ui.classifyHubRepoHref(fixture.endpoint + '/owner/model'), null, 'no configured endpoint means no in-app link');
eq(ui.classifyHubRepoHref('/owner/model'), null, 'a relative repository path needs a configured endpoint');
ui.state.settings = { endpoint: fixture.endpoint + '/' };
eqClassified(
  ui.classifyHubRepoHref(fixture.endpoint + '/owner/model'),
  'owner/model', 'model',
  'a trailing slash on the configured endpoint is ignored'
);
eqClassified(
  ui.classifyHubRepoHref(fixture.endpoint + '/datasets/owner/data'),
  'owner/data', 'dataset',
  'the dataset namespace stays in-app with a trailing-slash endpoint'
);

// A path-prefixed endpoint classifies only links under its prefix.
ui.state.settings = { endpoint: fixture.pathEndpoint };
eqClassified(
  ui.classifyHubRepoHref(fixture.pathEndpoint + '/owner/model'),
  'owner/model', 'model',
  'a path-prefixed endpoint classifies repository roots under the prefix'
);
eqClassified(
  ui.classifyHubRepoHref(fixture.pathEndpoint + '/datasets/owner/data'),
  'owner/data', 'dataset',
  'a path-prefixed endpoint keeps the dataset namespace'
);
for (const [href, label] of [
  ['https://mirror.example/owner/model', 'same host outside the endpoint prefix'],
  [fixture.pathEndpoint + '/owner/model/tree/dev', 'tree URL under the path prefix'],
  [fixture.pathEndpoint + '/hfold/owner/model', 'a path that only shares a prefix spelling']
]) {
  eq(ui.classifyHubRepoHref(href), null, `${label} must stay external`);
}

// --- End-to-end: sanitized panel anchors route repository roots in-app.
ui.state.settings = { endpoint: fixture.endpoint };
ui.state.serverEndpoint = '';

ui.setCommunityFixture(fixture.communityHtml);
const panelHtml = nodes.get('#modelsCommunityPanel').innerHTML;
ok(panelHtml.includes(fixture.endpoint + '/owner/model'), 'the panel renders the sanitized repository link');
ok(!panelHtml.includes('data-hf-repo'), 'the panel never renders repository-navigation attributes from a comment');

resetClicks();
let event = panelClick(anchorFor(panelHtml, fixture.endpoint + '/owner/model'));
eq(event.defaultPrevented, true, 'a Community repository link must prevent the external navigation');
eq(event.preventDefaultCalls, 1, 'the Community repository link cancels exactly one default action');
eq(lastAnalyzePath(), '/api/analyze/owner/model?type=model', 'a Community repository link opens Model Details as a model');
eq(ui.history().at(-1).repo, 'owner/model', 'the Community link enters the navigation history');
eq(ui.history().at(-1).repoType, 'model', 'the Community link records the model type');

resetClicks();
event = panelClick(anchorFor(panelHtml, fixture.endpoint + '/datasets/owner/data'));
eq(event.defaultPrevented, true, 'a Community dataset link must prevent the external navigation');
eq(lastAnalyzePath(), '/api/analyze/owner/data?type=dataset', 'a Community dataset link opens Model Details as a dataset');
eq(ui.history().at(-1).repoType, 'dataset', 'the Community dataset link records the dataset type');

for (const href of [
  fixture.endpoint + '/owner/model/blob/main/config.json',
  fixture.endpoint + '/owner/model/tree/main',
  fixture.endpoint + '/owner/model/resolve/main/config.json',
  fixture.endpoint + '/owner/model/commit/abc123',
  fixture.endpoint + '/papers/2401.04088',
  'https://example.com/owner/model',
  'mailto:someone@example.com'
]) {
  resetClicks();
  event = panelClick(anchorFor(panelHtml, href));
  eq(event.defaultPrevented, false, `${href} must keep its external navigation`);
  eq(event.preventDefaultCalls, 0, `${href} must not call preventDefault`);
  eq(analyzePaths().length, 0, `${href} must not start an analysis request`);
  eq(ui.history().length, 0, `${href} must not enter the navigation history`);
}

// A comment cannot forge internal routing: the sanitizer removes the
// attributes, and the handler ignores them anyway, so only the href decides.
resetClicks();
event = panelClick(fakeAnchor({
  href: 'https://example.com/forged',
  'data-hf-repo': 'owner/model',
  'data-hf-repo-type': 'model',
  'data-hf-repo-revision': 'dev'
}));
eq(event.defaultPrevented, false, 'forged repository attributes must not turn an external link into in-app navigation');
eq(analyzePaths().length, 0, 'forged repository attributes must not start an analysis request');

// Modified and non-primary clicks keep the browser's own behavior, matching
// the README delegation.
for (const [label, overrides] of [
  ['Ctrl-modified', { ctrlKey: true }],
  ['Meta-modified', { metaKey: true }],
  ['Shift-modified', { shiftKey: true }],
  ['Alt-modified', { altKey: true }],
  ['middle', { button: 1 }]
]) {
  resetClicks();
  event = panelClick(anchorFor(panelHtml, fixture.endpoint + '/owner/model'), overrides);
  eq(event.defaultPrevented, false, `${label} Community click keeps the browser default`);
  eq(analyzePaths().length, 0, `${label} Community click starts no navigation`);
}
resetClicks();
const modified = clickEvent(anchorFor(panelHtml, fixture.endpoint + '/owner/model'), true, { ctrlKey: true });
ui.handleCommunityLinkClick(modified);
eq(modified.defaultPrevented, false, 'a modified click is left to the browser');
eq(analyzePaths().length, 0, 'a modified click starts no navigation');
const alreadyPrevented = clickEvent(anchorFor(panelHtml, fixture.endpoint + '/owner/model'), true, { defaultPrevented: true });
ui.handleCommunityLinkClick(alreadyPrevented);
eq(alreadyPrevented.preventDefaultCalls, 0, 'an already-prevented click is not handled again');
eq(analyzePaths().length, 0, 'an already-prevented click starts no navigation');

// A link outside the panel's anchors is ignored.
resetClicks();
const notALink = clickEvent(fakeAnchor({}), false);
ui.handleCommunityLinkClick(notALink);
eq(notALink.defaultPrevented, false, 'a non-anchor target is ignored');

// --- Path-prefixed endpoint end-to-end: in-app under the prefix, external
// outside it, using the real sanitized output for that endpoint.
ui.state.settings = { endpoint: fixture.pathEndpoint };
ui.setCommunityFixture(fixture.pathPrefixedCommunityHtml);
const pathPanelHtml = nodes.get('#modelsCommunityPanel').innerHTML;

resetClicks();
event = panelClick(anchorFor(pathPanelHtml, fixture.pathEndpoint + '/owner/model'));
eq(event.defaultPrevented, true, 'a path-prefixed repository root opens in-app');
eq(lastAnalyzePath(), '/api/analyze/owner/model?type=model', 'the path prefix is not part of the repository id');

resetClicks();
event = panelClick(anchorFor(pathPanelHtml, fixture.pathEndpoint + '/datasets/owner/data'));
eq(event.defaultPrevented, true, 'a path-prefixed dataset root opens in-app');
eq(lastAnalyzePath(), '/api/analyze/owner/data?type=dataset', 'the path-prefixed dataset keeps its namespace');

resetClicks();
event = panelClick(anchorFor(pathPanelHtml, 'https://mirror.example/owner/model'));
eq(event.defaultPrevented, false, 'the same host outside the endpoint prefix stays external');
eq(analyzePaths().length, 0, 'the same host outside the endpoint prefix starts no analysis');

resetClicks();
event = panelClick(anchorFor(pathPanelHtml, fixture.pathEndpoint + '/owner/model/tree/dev'));
eq(event.defaultPrevented, false, 'a tree URL under the endpoint prefix stays external');
eq(analyzePaths().length, 0, 'a tree URL under the endpoint prefix starts no analysis');

console.log(`PASS: community link navigation (${assertions} assertions)`);
