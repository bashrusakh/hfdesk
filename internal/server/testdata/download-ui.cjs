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
let diskPreviewGate = null;
const window = {};
const context = {
  window,
  confirm: () => true,
  document: {
    readyState: 'loading', addEventListener() {},
    createElement() {
      return { textContent: '', get innerHTML() {
        return String(this.textContent).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
      } };
    },
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
    if (path === '/api/diskfree' && options?.method === 'POST' && diskPreviewGate) {
      const gate = diskPreviewGate;
      diskPreviewGate = null;
      gate.started();
      await gate.release;
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
    routeSelectValue, renderSelectableItems, renderNonSelectableFiles, startDownload,
    saveSettings, resetSettings,
    setAnalysis(value) { currentAnalysis = value; }
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
  const analysis = { repo, type, branch: 'custom', is_dataset: dataset };
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
  if (entry === 'file') await window.downloadSingleFile(repo, 'model.bin', dataset);
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
    if (entry === 'quant') {
      assert.equal(download.body.localRepo, 'fixture/parent');
      assert.equal(download.body.revision, 'custom', 'legacy selectable rows keep the analyzed revision');
      assert.deepEqual(download.body.filters, ['weights'], 'legacy selectable rows keep their filter');
      assert.equal(download.body.exactMatch, true, 'legacy selectable rows keep exact filter matching');
    }
    if (entry === 'all') {
      assert.equal(download.body.revision, 'custom', 'the existing wizard keeps the analyzed revision');
      assert.deepEqual(download.body.filters, [], 'the existing wizard keeps its unfiltered Download All request');
    }
  }
}

async function runBranchVariant() {
  const repo = 'fixture/exl-branches';
  const revision = 'refs/bpw/4.0';
  const item = {
    id: `exl-branch:${revision}`,
    label: revision,
    description: 'EXL3 · 4 BPW · head 6 bits',
    category: 'quantization',
    recommended: true,
    revision,
    branch_variant: true,
    size: 12 * 1024 * 1024 * 1024,
    size_human: '12 GiB'
  };
  const analysis = {
    repo,
    type: 'generic',
    branch: 'main',
    is_dataset: false,
    selectable_items: [item],
    recommended_download: { repo, revision, filters: [], exactMatch: false }
  };
  let routeKey = select('quantRouteSelect', analysis);
  const preferredRoute = ui.getConfiguredRouteOptions().find(option => option.key === 'llm/safetensors')?.key;
  if (preferredRoute) {
    const selectNode = nodes.get('#quantRouteSelect');
    selectNode.value = String(ui.getConfiguredRouteOptions().findIndex(option => option.key === preferredRoute));
    routeKey = ui.routeSelectValue('quantRouteSelect');
  }
  const html = ui.renderSelectableItems([item], 'selectableItems');
  assert(html.includes('>refs/bpw/4.0</span>'), 'branch label is rendered in the existing selectable row');
  assert(html.includes('EXL3 · 4 BPW · head 6 bits'), 'branch details use the existing row description');
  assert(html.includes('12 GiB'), 'optional branch size uses the existing size affordance');
  assert(html.includes('★ rec'), 'recommended branch keeps the existing recommendation affordance');

  const encoded = html.match(/data-download="([^"]+)"/)?.[1];
  assert(encoded, 'branch row exposes its download action');
  const args = JSON.parse(decodeURIComponent(encoded));
  assert.equal(args.revision, revision, 'the row carries its own branch revision');
  assert.equal(args.filterValue, '', 'branch id is not reused as a file filter');
  assert.equal(args.files, null, 'whole-branch selection has no file list');
  assert.equal(args.statusKey, '', 'destination-qualified status identity is resolved from the current existing selector at click time');
  assert.equal(args.branchVariant, true, 'branch action carries only the explicit analyzer branch marker');

  calls.length = 0;
  messages.length = 0;
  const sourceRow = { dataset: {}, querySelector() { return null; } };
  await window.downloadQuantFromButton({ dataset: { download: encoded }, closest: () => sourceRow });
  const preview = calls.find(c => c.path === '/api/diskfree');
  const download = calls.find(c => c.path === '/api/download');
  assert(preview, 'branch download includes the normal disk-space guard');
  assert(download, 'branch download reaches the real download handler');
  assert.deepEqual(download.body, preview.body, 'branch guard and creation use the identical request');
  const selectedRoot = routeKey
    ? (fixture.routes[routeKey] || (routeKey.startsWith('llm/') ? fixture.routes.llm : ''))
    : '';
  const appRoot = selectedRoot || fixture.local || fixture.cache;
  assert.equal(preview.data.path, appRoot === fixture.cache ? fixture.cache + '/hub' : appRoot,
    'branch disk guard resolves the selected destination provider');
  assert.equal(download.data.outputDir, appRoot, 'branch job uses the same selected application destination');
  assert.equal(download.body.repo, repo);
  assert.equal(download.body.revision, revision, 'selected branch wins over currentAnalysis main');
  assert.equal(download.body.branchVariant, true, 'analyzed EXL branch sends the explicit branchVariant flag');
  assert.equal(download.body.routeKey || '', routeKey, 'branch request uses the selected existing destination provider');
  assert.deepEqual(download.body.filters, [], 'branch request includes all branch files');
  assert.equal(download.body.exactMatch, false, 'branch request does not use exact file matching');
  assert.equal(download.body.excludes, undefined, 'branch request keeps branch support files unfiltered');
  assert.equal(download.data.revision, revision, 'the created job retains the selected revision');
  assert.equal(download.data.branchVariant, true, 'the created job retains the branch variant identity');
  assert.equal(download.data.routeKey || '', routeKey, 'the created job retains the selected route identity');

  const options = ui.getConfiguredRouteOptions();
  const alternateRoute = options.find(option => option.key !== routeKey);
  if (routeKey && alternateRoute) {
    calls.length = 0;
    messages.length = 0;
    let markStarted;
    let releasePreview;
    const started = new Promise(resolve => { markStarted = resolve; });
    const release = new Promise(resolve => { releasePreview = resolve; });
    diskPreviewGate = { started: markStarted, release };
    const rowControl = { outerHTML: '<button data-download="branch">↓</button>' };
    const delayedRow = {
      dataset: {},
      querySelector(selector) { return selector === '.quant-dl-btn' ? rowControl : null; }
    };
    const delayed = window.downloadQuantFromButton({
      dataset: { download: encoded },
      closest: () => delayedRow
    });
    await started;
    nodes.get('#quantRouteSelect').value = String(options.findIndex(option => option.key === alternateRoute.key));
    assert.equal(ui.routeSelectValue('quantRouteSelect'), alternateRoute.key);
    releasePreview();
    await delayed;
    const delayedDownload = calls.find(c => c.path === '/api/download');
    assert.equal(delayedDownload.body.routeKey, routeKey, 'in-flight request keeps its captured destination selector');
    assert.equal(delayedRow.dataset.dlStatus, undefined, 'late response does not mark the newly selected destination queued');
    assert.equal(rowControl.outerHTML, '<button data-download="branch">↓</button>', 'late response does not replace the new destination row control');
  }

  // A revision by itself must remain an ordinary legacy request; only the
  // explicit analyzer marker opts into branch-isolated storage.
  const absentMarkerItem = { ...item };
  delete absentMarkerItem.branch_variant;
  for (const legacyItem of [{ ...item, branch_variant: false }, absentMarkerItem]) {
    const legacyHtml = ui.renderSelectableItems([legacyItem], 'selectableItems');
    const legacyEncoded = legacyHtml.match(/data-download="([^"]+)"/)?.[1];
    const legacyArgs = JSON.parse(decodeURIComponent(legacyEncoded));
    assert.equal(legacyArgs.revision, revision, 'revision-only row keeps its selected revision');
    assert.equal(legacyArgs.branchVariant, undefined, 'false/absent analyzer marker does not opt into branch storage');
    calls.length = 0;
    await window.downloadQuantFromButton({ dataset: { download: legacyEncoded }, closest: () => sourceRow });
    const legacyDownload = calls.find(c => c.path === '/api/download');
    assert(legacyDownload, 'revision-only selectable row reaches the legacy download path');
    assert.equal(legacyDownload.body.revision, revision);
    assert.equal(legacyDownload.body.branchVariant, undefined);
    assert.deepEqual(legacyDownload.body.filters, [legacyItem.id]);
    assert.equal(legacyDownload.body.exactMatch, true);
  }

  const noSizeItem = { ...item, id: 'exl-branch:refs/bpw/5.0', label: 'refs/bpw/5.0', revision: 'refs/bpw/5.0', recommended: false, size: undefined, size_human: undefined };
  const noSizeHtml = ui.renderSelectableItems([noSizeItem], 'selectableItems');
  assert(!noSizeHtml.includes('quant-size'), 'unknown branch size is omitted rather than rendered as zero');
  assert(!noSizeHtml.includes('0 B'), 'unknown branch size is not mislabeled as measured zero');
}

async function runUpstreamCompanion() {
  const repo = 'fixture/parent-model';
  ui.setAnalysis({ repo, type: 'gguf', branch: 'refs/bpw/4.0', is_dataset: false });
  calls.length = 0;
  const sourceRow = { dataset: {}, querySelector() { return null; } };
  await window.downloadQuant(
    'fixture/upstream-base', 'mmproj', false, 'MMProj', repo, repo,
    ['mmproj-model-f16.gguf'], sourceRow, 'artifact:mmproj', 'refs/bpw/4.0'
  );
  const download = calls.find(c => c.path === '/api/download');
  assert(download, 'upstream companion reaches the real download handler');
  assert.equal(download.body.revision, 'main', 'upstream companion remains pinned to main');
  assert.equal(download.body.localRepo, repo, 'upstream companion remains stored with the analyzed model');
  assert.deepEqual(download.body.filters, ['./mmproj-model-f16.gguf'], 'upstream companion keeps exact file selection');
  assert.equal(download.body.exactMatch, true);
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
  await runBranchVariant();
  await runUpstreamCompanion();
  // File-list downloads select a repository-relative path exactly: a root-level
  // entry is encoded as "./name" so a same-named nested alias is not selected,
  // while a nested entry keeps its canonical relative path. The displayed name
  // and toast stay unencoded.
  ui.setAnalysis({ repo: 'fixture/filelist', type: 'audio', branch: 'main' });
  const rootFileList = ui.renderNonSelectableFiles({ repo: 'fixture/filelist', files: [{ path: 'model.bin' }] });
  assert(rootFileList.includes('>model.bin<'), 'root file-list display name must stay unencoded');
  calls.length = 0;
  messages.length = 0;
  await window.downloadSingleFile('fixture/filelist', 'model.bin', false);
  let fileDownload = calls.find(c => c.path === '/api/download');
  assert(fileDownload, 'root file-list request missing');
  assert.deepEqual(fileDownload.body.filters, ['./model.bin']);
  assert(fileDownload.body.exactMatch);
  assert(!fileDownload.body.filters.includes('model.bin'), 'root file list must not send the legacy basename selector');
  assert(messages.some(m => m.message === 'Queued: model.bin'), 'root file-list toast must stay unencoded');
  calls.length = 0;
  messages.length = 0;
  await window.downloadSingleFile('fixture/filelist', 'nested/model.bin', false);
  fileDownload = calls.find(c => c.path === '/api/download');
  assert(fileDownload, 'nested file-list request missing');
  assert.deepEqual(fileDownload.body.filters, ['nested/model.bin']);
  assert(fileDownload.body.exactMatch);
  assert(messages.some(m => m.message === 'Queued: nested/model.bin'), 'nested file-list toast must keep the relative path');
  // Dataset renderers must not offer inert controls, even with routes configured.
  ui.setAnalysis({ repo: 'fixture/data', type: 'gguf', is_dataset: true });
  assert(!ui.renderSelectableItems([{label: 'Data', id: 'data'}], 'items').includes('quantRouteSelect'));
  assert(!ui.renderNonSelectableFiles({ repo: 'fixture/data', is_dataset: true, files: [{path: 'data.bin'}] }).includes('nonSelectableRoute'));
  // No configured destinations leave both analysis surfaces without a picker.
  if (!Object.keys(fixture.routes || {}).length) {
    ui.setAnalysis({ repo: 'fixture/model', type: 'gguf' });
    assert(!ui.renderSelectableItems([{label: 'Model', id: 'model'}], 'items').includes('quantRouteSelect'));
    assert(!ui.renderNonSelectableFiles({ repo: 'fixture/model', files: [{path: 'model.bin'}] }).includes('nonSelectableRoute'));
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
