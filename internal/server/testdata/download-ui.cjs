// Run through TestDownloadUIDestination: real app.js + real server handlers.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');
const fixture = JSON.parse(process.env.HFDESK_UI_FIXTURE);
const nodes = new Map();
const calls = [];
const messages = [];
let lowDestination = false;
let sequence = 0;
const window = {};
// The template oracle. makeNode used to auto-vivify a memoized stub child for
// ANY selector, so a job-card assertion passed even when the real template no
// longer contained the node it claimed to check (or updateJobCard's selector
// drifted). Once markup is assigned through innerHTML — the real template a
// browser would parse — a queried selector must match an element parsed from
// that markup; otherwise querySelector throws instead of returning a stub.
// Nodes without assigned markup keep the memoizing stub behavior: there is no
// real template to discriminate against.
const VOID_TAGS = new Set(['area', 'base', 'br', 'col', 'embed', 'hr', 'img',
  'input', 'link', 'meta', 'param', 'source', 'track', 'wbr']);

// parseMarkup turns assigned markup into element records with their class and
// attribute sets plus a parent link, mirroring what a browser would parse.
function parseMarkup(markup) {
  const elements = [];
  const stack = [];
  const tagRe = /<(\/?)([a-zA-Z][\w-]*)((?:"[^"]*"|'[^']*'|[^>"'])*)>/g;
  let match;
  while ((match = tagRe.exec(markup)) !== null) {
    const tag = match[2].toLowerCase();
    if (match[1] === '/') {
      if (stack.length) stack.pop();
      continue;
    }
    const element = { tag, classes: new Set(), attrs: new Map(), parent: stack[stack.length - 1] || null };
    const attrRe = /([a-zA-Z_:][-\w:.]*)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s"'>]+)))?/g;
    let attr;
    while ((attr = attrRe.exec(match[3])) !== null) {
      const name = attr[1].toLowerCase();
      const value = attr[2] !== undefined ? attr[2] : attr[3] !== undefined ? attr[3] : attr[4] !== undefined ? attr[4] : '';
      if (name === 'class') {
        for (const cls of value.split(/\s+/)) if (cls) element.classes.add(cls);
      } else {
        element.attrs.set(name, value);
      }
    }
    elements.push(element);
    if (!/\/\s*$/.test(match[3]) && !VOID_TAGS.has(tag)) stack.push(element);
  }
  return elements;
}

// parseCompound parses tag/.class/#id/[attr](="value") pieces of one compound
// selector. It returns null for anything the oracle cannot evaluate
// (pseudo-classes, unquoted nesting), so the caller fails closed rather than
// silently accepting a selector it never checked.
function parseCompound(token) {
  const parts = /^([a-zA-Z][\w-]*)((?:[.#][\w-]+|\[[^\]]+\])*)$|^((?:[.#][\w-]+|\[[^\]]+\])+)$/.exec(token);
  if (!parts) return null;
  const compound = { tag: parts[1] ? parts[1].toLowerCase() : '', classes: [], attrs: [] };
  const rest = parts[2] !== undefined ? parts[2] : parts[3];
  const pieceRe = /[.#][\w-]+|\[[^\]]+\]/g;
  let piece;
  while ((piece = pieceRe.exec(rest)) !== null) {
    const text = piece[0];
    if (text[0] === '.') { compound.classes.push(text.slice(1)); continue; }
    if (text[0] === '#') { compound.attrs.push(['id', text.slice(1)]); continue; }
    const attr = /^\[([\w-]+)(?:([~|^$*]?=)"?([^"\]]*)"?)?\]$/.exec(text);
    if (!attr) return null;
    compound.attrs.push([attr[1].toLowerCase(), attr[3] === undefined ? null : attr[3]]);
  }
  return compound;
}

function matchesCompound(element, compound) {
  if (compound.tag && element.tag !== compound.tag) return false;
  for (const cls of compound.classes) if (!element.classes.has(cls)) return false;
  for (const [name, value] of compound.attrs) {
    if (!element.attrs.has(name)) return false;
    if (value !== null && element.attrs.get(name) !== value) return false;
  }
  return true;
}

function chainMatches(element, chain) {
  let index = chain.length - 1;
  if (!matchesCompound(element, chain[index].compound)) return false;
  let current = element;
  while (index > 0) {
    index--;
    if (chain[index + 1].combinator === 'child') {
      current = current.parent;
      if (!current || !matchesCompound(current, chain[index].compound)) return false;
      continue;
    }
    let ancestor = current.parent;
    while (ancestor && !matchesCompound(ancestor, chain[index].compound)) ancestor = ancestor.parent;
    if (!ancestor) return false;
    current = ancestor;
  }
  return true;
}

// markupHasSelector reports whether some element of the parsed markup matches
// the selector. Unsupported selector syntax throws: an unverifiable query must
// not pass as if it had been checked.
function markupHasSelector(elements, selector) {
  for (const alternative of selector.split(',')) {
    const tokens = alternative.replace(/\s*>\s*/g, ' > ').trim().split(/\s+/).filter(Boolean);
    const chain = [];
    let pending = ' ';
    for (const token of tokens) {
      if (token === '>') {
        if (!chain.length || pending !== ' ') throw new Error('oracle cannot verify selector ' + JSON.stringify(selector));
        pending = 'child';
        continue;
      }
      const compound = parseCompound(token);
      if (!compound) throw new Error('oracle cannot verify selector ' + JSON.stringify(selector));
      chain.push({ compound, combinator: chain.length === 0 ? null : pending });
      pending = ' ';
    }
    if (!chain.length) throw new Error('oracle cannot verify selector ' + JSON.stringify(selector));
    for (const element of elements) if (chainMatches(element, chain)) return true;
  }
  return false;
}

// makeNode is the observed DOM: a small element stub whose querySelector
// memoizes one child per selector, so real render code (createJobCard /
// updateJobCard) can write fields the assertions then read back. Reads of
// innerHTML keep the historical textContent-derived escaping (escapeHtml
// depends on it) until code assigns innerHTML itself.
function makeNode() {
  const children = new Map();
  let text = '';
  let html = null;
  let parsed = [];
  return {
    get textContent() { return text; },
    set textContent(value) { text = String(value); html = null; parsed = []; },
    get innerHTML() {
      if (html !== null) return html;
      return String(text).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
    },
    set innerHTML(value) { html = String(value); parsed = parseMarkup(html); children.clear(); },
    className: '',
    title: '',
    style: { display: '' },
    dataset: {},
    classList: {
      add() {},
      remove() {},
      contains() { return false; },
      toggle() {},
    },
    onclick: null,
    appendChild(child) { return child; },
    removeChild(child) { return child; },
    remove() {},
    addEventListener() {},
    querySelector(selector) {
      if (html !== null && !markupHasSelector(parsed, selector)) {
        throw new Error(
          'selector ' + JSON.stringify(selector) + ' matches no element in this node\'s markup; ' +
          'the queried element must pre-exist in the real template, not only in the memoizing stub');
      }
      if (!children.has(selector)) children.set(selector, makeNode());
      return children.get(selector);
    },
    querySelectorAll() { return []; },
  };
}
const context = {
  window,
  confirm: () => true,
  document: {
    readyState: 'loading', addEventListener() {},
    createElement() { return makeNode(); },
    querySelector: selector => nodes.get(selector) || null,
    querySelectorAll: () => []
  },
  localStorage: { getItem() { return null; }, setItem() {} },
  CSS: { escape: s => s },
  fetch: async (path, options) => {
    // Settings preservation is a request-body check only; do not persist test
    // configuration or change the server's scheduler/destination fixtures.
    if (path === '/api/settings') {
      calls.push({ path, body: options?.body ? JSON.parse(options.body) : null });
      return { ok: true, json: async () => ({downloadRoutes: fixture.routes || {}}) };
    }
    const response = await fetch(fixture.url + path, options);
    const data = await response.json();
    calls.push({ path, body: options?.body ? JSON.parse(options.body) : null, data });
    // Destination is resolved by the real handler. Only available capacity is
    // injected: cache and destination intentionally have opposite free space.
    if (path === '/api/diskfree' && options?.method === 'POST') {
      data.free = (lowDestination ? 1 : 1024) * 1024 * 1024;
    } else if (path.startsWith('/api/diskfree?') || (path === '/api/diskfree' && !options)) {
      data.free = (lowDestination ? 1024 : 1) * 1024 * 1024;
    }
    return { ok: response.ok, json: async () => data };
  }
};
let source = fs.readFileSync('../assets/static/js/app.js', 'utf8');
source = source.replace('  // Start\n', `
  window.test = {
    state, getConfiguredRouteOptions, routeKeyForAnalysis, renderRouteSelect,
    routeSelectValue, renderSelectableItems, renderFilesTab,
    resetFileTree, fileTreeToggleFile, startDownload,
    saveSettings, resetSettings,
    setAnalysis(value) { currentAnalysis = value; },
    setAdvancedOptions(value) { advancedOptions = value; },
    createJobCard, updateJobCard
  };
  showToast = (message, type) => window.messages.push({message, type});
  navigateTo = () => {};
  // Start\n`);
window.messages = messages;
vm.runInNewContext(source, context);
const ui = window.test;
ui.state.settings = { downloadRoutes: fixture.routes || {} };
nodes.set('#modalBackdrop', { classList: { remove() {} } });

function select(id, analysis) {
  nodes.delete('#quantRouteSelect');
  nodes.delete('#nonSelectableRoute');
  nodes.delete('#dlModalRoute');
  ui.setAnalysis(analysis);
  const html = ui.renderRouteSelect(id);
  assert(!/llm\/|value="audio"/.test(html), 'raw keys must not appear in markup');
  const chosen = html.match(/value="(-?\d+)" selected/);
  nodes.set('#' + id, { value: chosen[1], dataset: { optionCount: String(ui.getConfiguredRouteOptions().length) } });
  return ui.routeSelectValue(id);
}

async function run(entry, type, { dataset = false, manual = '', low = false, choice } = {}) {
  const repo = 'fixture/case' + (++sequence);
  const analysis = { repo, type, branch: 'custom', is_dataset: dataset, files: [{ path: 'model.bin', size: 123 }] };
  const id = entry === 'quant' ? 'quantRouteSelect' : entry === 'modal' ? 'dlModalRoute' : 'nonSelectableRoute';
  let key = select(id, analysis);
  if (choice !== undefined) {
    nodes.get('#' + id).value = String(ui.getConfiguredRouteOptions().findIndex(o => o.key === choice));
    key = ui.routeSelectValue(id);
  }
  nodes.set('#dlModalLocalDir', { value: manual });
  const semantic = dataset ? '' : type === 'gguf' ? 'llm/gguf'
    : ['transformers', 'gptq', 'awq'].includes(type) ? 'llm/safetensors' : 'audio';
  assert.equal(ui.routeKeyForAnalysis(analysis), semantic);
  const expectedKey = choice !== undefined ? choice
    : semantic && (fixture.routes?.[semantic] || fixture.routes?.llm && semantic.startsWith('llm/')) ? semantic : '';
  assert.equal(key, expectedKey);
  const expectedPath = manual || (expectedKey && (fixture.routes[expectedKey] || fixture.routes.llm)) || fixture.local || fixture.cache;
  calls.length = 0;
  messages.length = 0;
  lowDestination = low;
  if (entry === 'quant') await window.downloadQuant(repo, 'weights', dataset, 'Weights', 'fixture/parent', repo);
  if (entry === 'file') {
    // The Files tree start surface checks files through selection state, then
    // posts the strict request with the same Save to pick and disk guard the
    // other entries exercise.
    ui.resetFileTree(analysis);
    ui.fileTreeToggleFile('model.bin');
    await window.downloadSelectedFiles();
  }
  if (entry === 'all') await window.startWizardDownload(repo, dataset);
  if (entry === 'modal') await window.confirmDirectDownload(repo, dataset, 'custom');
  if (entry === 'form') {
    const prefix = dataset ? 'dataset' : 'model';
    for (const [name, value] of Object.entries({ Repo: repo, Revision: 'custom', Filter: '', Exclude: '', LocalDir: manual })) {
      nodes.set('#' + prefix + name, { value });
    }
    await ui.startDownload(dataset ? 'dataset' : 'model');
  }
  const preview = calls.find(c => c.path === '/api/diskfree');
  assert(preview, `${entry}/${type}: disk preview missing`);
  assert(preview.body, `${entry}/${type}: default GET checks the wrong disk`);
  const appPath = entry === 'form' ? manual || fixture.local || fixture.cache : expectedPath;
  const physicalPath = appPath === fixture.cache ? fixture.cache + '/hub' : appPath;
  assert.equal(preview.data.path, physicalPath);
  assert.equal(preview.body.routeKey || '', entry === 'form' ? '' : expectedKey);
  const download = calls.find(c => c.path === '/api/download');
  if (low) {
    assert(!download, `${entry}/${type}: destination full, cache free must block`);
    assert(messages.some(m => m.type === 'error'));
  } else {
    assert(download, `${entry}/${type}: destination free, cache full must allow: ${JSON.stringify(messages)}`);
    assert.deepEqual(download.body, preview.body, 'guard and creation must use the identical request');
    assert.equal(download.data.outputDir, appPath, 'CreateJob retains the app/friendly root');
    assert.equal(download.data.hubDir || download.data.outputDir, preview.data.path, 'Physical job destination must match disk preview');
    if (entry === 'quant') assert.equal(download.body.localRepo, 'fixture/parent');
    if (entry === 'file') assert.equal(download.body.strictFiles, true, 'the tree start surface posts a strict selection');
  }
}

(async () => {
  for (const type of ['gguf', 'transformers', 'gptq', 'awq']) {
    for (const low of [false, true]) await run('quant', type, { low });
  }
  for (const entry of ['file', 'all', 'modal', 'form']) {
    for (const low of [false, true]) await run(entry, 'audio', { low });
  }
  for (const entry of ['quant', 'file', 'all', 'modal', 'form']) {
    await run(entry, 'gguf', { dataset: true });
  }
  for (const entry of ['modal', 'form']) {
    for (const low of [false, true]) await run(entry, 'gguf', { manual: fixture.manual, low });
  }
  await run('quant', 'gguf', { choice: '' });
  if (fixture.routes?.audio) await run('quant', 'gguf', { choice: 'audio' });
  // The Files tree selects repository-relative paths exactly: a root-level
  // entry is encoded as "./name" so a same-named nested alias is not selected,
  // while a nested entry keeps its canonical relative path. The request is a
  // strict selection and the displayed names stay unencoded.
  const filesPanel = {
    innerHTML: '',
    dataset: {},
    addEventListener() {},
    setAttribute() {},
    removeAttribute() {},
    querySelector: () => null,
    querySelectorAll: () => []
  };
  nodes.set('#modelsFilesPanel', filesPanel);
  const rootAnalysis = { repo: 'fixture/filelist', type: 'audio', branch: 'main', files: [{ path: 'model.bin', size: 123 }] };
  ui.setAnalysis(rootAnalysis);
  ui.renderFilesTab(rootAnalysis);
  assert(filesPanel.innerHTML.includes('>model.bin<'), 'root file-list display name must stay unencoded');
  ui.fileTreeToggleFile('model.bin');
  calls.length = 0;
  messages.length = 0;
  await window.downloadSelectedFiles();
  let fileDownload = calls.find(c => c.path === '/api/download');
  assert(fileDownload, 'root file-list request missing');
  assert.deepEqual(fileDownload.body.filters, ['./model.bin']);
  assert.equal(fileDownload.body.strictFiles, true, 'root file list posts a strict selection');
  assert(!('exactMatch' in fileDownload.body), 'a strict request does not send exactMatch');
  assert(!fileDownload.body.filters.includes('model.bin'), 'root file list must not send the legacy basename selector');
  assert(messages.some(m => m.message === 'Queued: 1 file'), 'root file-list toast reports the strict selection');
  calls.length = 0;
  messages.length = 0;
  const nestedAnalysis = { repo: 'fixture/filelist', type: 'audio', branch: 'main', files: [{ path: 'nested/model.bin', size: 42 }] };
  ui.setAnalysis(nestedAnalysis);
  ui.resetFileTree(nestedAnalysis);
  ui.fileTreeToggleFile('nested/model.bin');
  await window.downloadSelectedFiles();
  fileDownload = calls.find(c => c.path === '/api/download');
  assert(fileDownload, 'nested file-list request missing');
  assert.deepEqual(fileDownload.body.filters, ['nested/model.bin']);
  assert.equal(fileDownload.body.strictFiles, true, 'nested file list posts a strict selection');
  assert(messages.some(m => m.message === 'Queued: 1 file'), 'nested file-list toast keeps the selection count');
  // D4 start-flow plaque: with custom filters, Start queues the download
  // first (non-blocking) and then dry-runs the identical request; the
  // filters_unmatched notice of that fresh plan surfaces as a warning
  // toast naming exactly the unmatched pattern.
  calls.length = 0;
  messages.length = 0;
  const plaqueAnalysis = { repo: 'owner/plaque', type: 'gguf', branch: 'main' };
  ui.setAnalysis(plaqueAnalysis);
  select('nonSelectableRoute', plaqueAnalysis);
  nodes.set('#dlModalLocalDir', { value: '' });
  ui.setAdvancedOptions({ filter: '.safetensors,q4', exclude: '', shardRanges: '' });
  await window.startWizardDownload('owner/plaque', false);
  assert(calls.find(c => c.path === '/api/download'), 'start flow must queue the download (non-blocking)');
  const plaquePlan = calls.find(c => c.path === '/api/plan');
  assert(plaquePlan && plaquePlan.body.dryRun === true, 'start flow must dry-run the same request for the plaque');
  assert.equal(plaquePlan.body.filters.join(','), '.safetensors,q4', 'plaque plan must use the started filters');
  assert(messages.some(m => m.type === 'warning' && m.message === 'filter "q4" matched nothing'),
    'unmatched filter must surface as a warning toast: ' + JSON.stringify(messages));
  ui.setAdvancedOptions({ filter: '', exclude: '', shardRanges: '' });

  // D4 job-card warnings: the card's single warnings block renders what
  // the job's own plan reported through job.warnings (strings) and hides
  // it when the plan reported none.
  const warnedJob = {
    id: 'warn-1', repo: 'owner/plaque', revision: 'main', status: 'running',
    progress: { totalFiles: 1, completedFiles: 0, totalBytes: 10, downloadedBytes: 5 },
    warnings: ['filter "q4" matched nothing']
  };
  const card = ui.createJobCard(warnedJob);
  assert(card.innerHTML.includes('data-role="warnings"'),
    'the real job-card template must pre-contain the data-role="warnings" node');
  ui.updateJobCard(card, warnedJob);
  const warnBlock = card.querySelector('[data-role="warnings"]');
  assert(warnBlock.innerHTML.includes('filter "q4" matched nothing'),
    'job card must render the unmatched-filter warning: ' + warnBlock.innerHTML);
  assert.notEqual(warnBlock.style.display, 'none', 'job warnings block must be visible');
  ui.updateJobCard(card, { ...warnedJob, warnings: undefined });
  assert.equal(warnBlock.style.display, 'none', 'job warnings block must hide when the plan reported none');
  // Dataset renderers must not offer inert controls, even with routes configured.
  ui.setAnalysis({ repo: 'fixture/data', type: 'gguf', is_dataset: true });
  assert(!ui.renderSelectableItems([{label: 'Data', id: 'data'}], 'items').includes('quantRouteSelect'));
  const datasetAnalysis = { repo: 'fixture/data', type: 'gguf', is_dataset: true, files: [{ path: 'data.bin', size: 7 }] };
  ui.setAnalysis(datasetAnalysis);
  ui.renderFilesTab(datasetAnalysis);
  assert(!filesPanel.innerHTML.includes('nonSelectableRoute'), 'a dataset tree must not offer a destination picker');
  // D5b config presets: the Dataset Configs category renders above Dataset
  // Splits, exactly one Recommended badge sits on the config row, and
  // downloading that row posts its folder filter with exactMatch.
  const configPresetAnalysis = { repo: 'fixture/configs', type: 'gguf', branch: 'main', is_dataset: true, files: [{ path: 'cc100/train/part.parquet', size: 10 }] };
  ui.setAnalysis(configPresetAnalysis);
  const configPresetHtml = ui.renderSelectableItems([
    { id: 'cc100/', label: 'cc100 (train+test)', category: 'config', filter_value: 'cc100/',
      recommended: true, size_human: '4.1 GB', files: ['cc100/train/part.parquet'] },
    { id: 'wiki/', label: 'wiki (validation)', category: 'config', filter_value: 'wiki/', size_human: '300 B' },
    { id: 'train', label: 'Train', category: 'split', filter_value: 'train', size_human: '3.0 GB' },
    { id: 'test', label: 'Test', category: 'split', filter_value: 'test', size_human: '1.1 GB' }
  ], 'configPresetItems');
  const configCatPos = configPresetHtml.indexOf('Dataset Configs');
  const splitCatPos = configPresetHtml.indexOf('Dataset Splits');
  assert(configCatPos !== -1 && splitCatPos !== -1, 'both dataset categories must render: ' + configPresetHtml);
  assert(configCatPos < splitCatPos, 'Dataset Configs must render above Dataset Splits');
  const recBadges = configPresetHtml.match(/★ rec/g) || [];
  assert.equal(recBadges.length, 1, 'exactly one Recommended badge must render');
  assert(configPresetHtml.slice(configCatPos, splitCatPos).includes('★ rec'),
    'the Recommended badge must sit on the config preset, not a split');
  const configRow = /data-fv="cc100\/"[\s\S]*?data-download="([^"]+)"/.exec(configPresetHtml);
  assert(configRow, 'the config preset row must carry a download payload');
  calls.length = 0;
  messages.length = 0;
  await window.downloadQuantFromButton({ dataset: { download: configRow[1] }, closest: () => null });
  const configDownload = calls.find(c => c.path === '/api/download');
  assert(configDownload, 'config preset download request missing');
  const configPreview = calls.find(c => c.path === '/api/diskfree');
  assert(configPreview && configPreview.body, 'config preset must check the disk first');
  assert.deepEqual(configDownload.body, configPreview.body, 'guard and creation must use the identical config request');
  assert.deepEqual(configDownload.body.filters, ['cc100/'], 'config row must send its folder filter');
  assert.equal(configDownload.body.exactMatch, true, 'the config folder filter is exact');
  assert.equal(configDownload.body.dataset, true, 'the config preset stays a dataset request');
  // Family-exact selection: a rare-format family row (OpenVINO here) routes
  // through the exact-artifact request path, so its request carries exactly
  // item.files, while the pre-existing files-less format row keeps its legacy
  // token request byte-identical.
  const familyAnalysis = { repo: 'fixture/family', type: 'transformers', branch: 'main',
    files: [{ path: 'model.xml', size: 10 }, { path: 'model.bin', size: 20 }] };
  ui.setAnalysis(familyAnalysis);
  const familyHtml = ui.renderSelectableItems([
    { id: 'safetensors', label: 'SafeTensors', category: 'format', filter_value: 'safetensors',
      recommended: true, size_human: '1.0 B' },
    { id: 'openvino', label: 'OpenVINO', category: 'format', filter_value: '.xml',
      size_human: '30 B', files: ['model.xml', 'model.bin'] }
  ], 'familyItems');
  const safeRow = /data-fv="safetensors"[\s\S]*?data-download="([^"]+)"/.exec(familyHtml);
  const familyRow = /data-fv="\.xml"[\s\S]*?data-download="([^"]+)"/.exec(familyHtml);
  assert(safeRow && familyRow, 'both format rows must carry download payloads: ' + familyHtml);
  calls.length = 0;
  messages.length = 0;
  await window.downloadQuantFromButton({ dataset: { download: safeRow[1] }, closest: () => null });
  const safeDownload = calls.find(c => c.path === '/api/download');
  assert(safeDownload, 'safetensors row download request missing');
  assert.deepEqual(safeDownload.body,
    { repo: 'fixture/family', revision: 'main', dataset: false, filters: ['safetensors'], exactMatch: true },
    'files-less format row request must stay byte-identical to the legacy token body');
  calls.length = 0;
  messages.length = 0;
  await window.downloadQuantFromButton({ dataset: { download: familyRow[1] }, closest: () => null });
  const familyDownload = calls.find(c => c.path === '/api/download');
  assert(familyDownload, 'family row download request missing');
  assert.deepEqual(familyDownload.body.filters, ['./model.xml', './model.bin'],
    'family row must send exactly item.files through the exact-artifact path');
  assert.equal(familyDownload.body.exactMatch, true, 'family row selection must be exact');
  assert(!familyDownload.body.strictFiles, 'the exact-artifact path does not escalate to strictFiles');
  // No configured destinations leave both analysis surfaces without a picker.
  if (!Object.keys(fixture.routes || {}).length) {
    const noRouteAnalysis = { repo: 'fixture/model', type: 'gguf', files: [{ path: 'model.bin', size: 7 }] };
    ui.setAnalysis(noRouteAnalysis);
    assert(!ui.renderSelectableItems([{label: 'Model', id: 'model'}], 'items').includes('quantRouteSelect'));
    ui.renderFilesTab(noRouteAnalysis);
    assert(!filesPanel.innerHTML.includes('nonSelectableRoute'), 'no configured destinations leave the tree without a picker');
  } else {
    const routedAnalysis = { repo: 'fixture/model', type: 'gguf', files: [{ path: 'model.bin', size: 7 }] };
    ui.setAnalysis(routedAnalysis);
    ui.renderFilesTab(routedAnalysis);
    assert(filesPanel.innerHTML.includes('nonSelectableRoute'), 'configured destinations keep the tree Save to picker');
  }
  // A changed configuration must not reinterpret a previously selected index.
  const oldRoutes = ui.state.settings.downloadRoutes;
  ui.state.settings.downloadRoutes = {audio: '/audio'};
  select('quantRouteSelect', {type: 'audio'});
  ui.state.settings.downloadRoutes = {diffusion: '/diffusion'};
  assert.equal(ui.routeSelectValue('quantRouteSelect'), '');
  ui.state.settings.downloadRoutes = oldRoutes;
  // Embedding specialization and unrelated mappings stay intact.
  assert.equal(ui.routeKeyForAnalysis({type: 'transformers', transformers: {task: 'feature-extraction'}}), 'embedding');
  assert.equal(ui.routeKeyForAnalysis({type: 'diffusers'}), 'diffusion');
  assert.equal(ui.routeKeyForAnalysis({type: 'unknown'}), '');
  // The config-only coarse key must survive a normal save and be cleared by a
  // reset, even though no form field exposes that internal key.
  ui.state.settings.downloadRoutes = {llm: '/config-only'};
  await ui.saveSettings();
  assert.equal(calls.filter(c => c.path === '/api/settings' && c.body).at(-1).body.downloadRoutes.llm, '/config-only');
  ui.state.settings.downloadRoutes = {llm: '/config-only'};
  ui.resetSettings();
  await ui.saveSettings();
  assert.equal(Object.keys(calls.filter(c => c.path === '/api/settings' && c.body).at(-1).body.downloadRoutes).length, 0);
  console.log(`PASS: ${sequence} download/guard cases against real server; four LLM types, Audio, dataset, explicit/default/chosen destinations`);
})().catch(error => { console.error(error); process.exitCode = 1; });
