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
  assert.equal(preview.data.path, entry === 'form' ? manual || fixture.local || fixture.cache : expectedPath);
  assert.equal(preview.body.routeKey || '', entry === 'form' ? '' : expectedKey);
  const download = calls.find(c => c.path === '/api/download');
  if (low) {
    assert(!download, `${entry}/${type}: destination full, cache free must block`);
    assert(messages.some(m => m.type === 'error'));
  } else {
    assert(download, `${entry}/${type}: destination free, cache full must allow: ${JSON.stringify(messages)}`);
    assert.deepEqual(download.body, preview.body, 'guard and creation must use the identical request');
    assert.equal(download.data.outputDir, preview.data.path, 'CreateJob destination must match disk preview');
    if (entry === 'quant') assert.equal(download.body.localRepo, 'fixture/parent');
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
