// Run through TestSelectableArtifactStatusUI: real app.js status reconciliation.
//
// The harness loads the shipped app.js inside Node's VM with DOM, localStorage
// and cache-fetch stubs. It drives the production sequence in
// loadRepositoryDetails/renderAnalysisResult: refreshDlStatusFromJobs runs
// before syncLocalAnalysisStatus, then renderSelectableItems reads the result.
// It also feeds real job_update/init WebSocket messages through
// handleWSMessage and flushes the coalesced live reconciliation.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const REPO = 'owner/model';
const SHARD1 = 'model-Q4_0-00001-of-00002.gguf';
const SHARD2 = 'model-Q4_0-00002-of-00002.gguf';
const MMPROJ = 'mmproj-model-f16.gguf';
const FILES = [SHARD1, SHARD2];
// downloadQuant sends root-level artifact files through artifactRequestPaths,
// which prefixes bare names with "./".
const JOB_FILTERS = FILES.map(name => './' + name);
const ARTIFACT_KEY = `artifact:${JSON.stringify(FILES.map(name => name.toLowerCase()))}`;
const MODEL_STORAGE = `dlStatus:${REPO}:model`;
const DATASET_STORAGE = `dlStatus:${REPO}:dataset`;
const LEGACY_STORAGE = `dlStatus:${REPO}`;

let assertions = 0;
function eq(actual, expected, message) { assertions++; assert.equal(actual, expected, message); }
function match(value, re, message) { assertions++; assert.match(value, re, message); }
function doesNotMatch(value, re, message) { assertions++; assert.doesNotMatch(value, re, message); }

function makeNode() {
  let html = '';
  let text = '';
  return {
    style: {},
    dataset: {},
    classList: { add() {}, remove() {}, contains() { return false; } },
    set innerHTML(value) { html = String(value); },
    get innerHTML() { return html || text.replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'); },
    set textContent(value) { text = String(value); },
    get textContent() { return text; },
    appendChild() {},
    addEventListener() {},
    querySelector() { return null; },
    querySelectorAll() { return []; },
    remove() {}
  };
}

const domNodes = new Map();
const document = {
  readyState: 'loading',
  body: makeNode(),
  handlers: {},
  addEventListener(type, fn) { (this.handlers[type] ||= []).push(fn); },
  createElement() { return makeNode(); },
  querySelector(selector) { return domNodes.get(selector) || null; },
  querySelectorAll() { return []; },
  getElementById() { return null; }
};

const storage = new Map();
const localStorage = {
  getItem(key) { return storage.has(key) ? storage.get(key) : null; },
  setItem(key, value) { storage.set(key, String(value)); },
  removeItem(key) { storage.delete(key); }
};

const cacheRequests = [];
function lastCacheRequest() { return cacheRequests[cacheRequests.length - 1] || ''; }

let cacheState = { status: 200, files: [], error: null, parseError: null };
const fetch = async (rawPath) => {
  const requestPath = String(rawPath);
  if (!requestPath.startsWith('/api/cache/')) {
    throw new Error('unexpected fetch: ' + rawPath);
  }
  cacheRequests.push(requestPath);
  if (cacheState.error) throw cacheState.error;
  return {
    status: cacheState.status,
    ok: cacheState.status >= 200 && cacheState.status < 300,
    json: async () => {
      if (cacheState.parseError) throw cacheState.parseError;
      return { files: cacheState.files.map(name => ({ name })) };
    }
  };
};

const window = {
  location: { protocol: 'https:', host: 'hfdesk.test', search: '', hash: '', pathname: '/' },
  addEventListener() {}
};

// Timers are coalescing details here; the harness flushes reconciliation
// directly instead of sleeping. Pending auto-dismiss timers never run.
const timers = [];
function fakeSetTimeout(fn, ms) {
  const timer = { fn, ms, cleared: false };
  timers.push(timer);
  return timer;
}
function fakeClearTimeout(timer) {
  if (timer) timer.cleared = true;
}

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
  console.error('selectable-status-ui: app.js injection marker is missing');
  process.exit(1);
}
const hook = `
  window.test = {
    state,
    refreshDlStatusFromJobs,
    syncLocalAnalysisStatus,
    renderSelectableItems,
    updateSelectableRowStatuses,
    handleWSMessage,
    reconcileAnalysisAfterLoad,
    getDlStatus,
    selectableStatusKey,
    hasLiveReconcileTimer() { return liveStatusReconcileTimer !== null; },
    setCurrentAnalysis(value) { currentAnalysis = value; },
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
    }
  };
`;
source = source.replace(marker, hook + '\n' + marker);
vm.runInNewContext(source, context);
const ui = window.test;
ui.state.settings = {};

function reset({ jobs = [], cacheFiles = [], cacheStatus = 200, cacheError = null, cacheParseError = null, initial = {}, dataset = false, items = [] } = {}) {
  ui.state.jobs.clear();
  jobs.forEach((job, index) => ui.state.jobs.set(job.id || `job-${index}`, job));
  ui.state.settings = {};
  ui.setCurrentAnalysis({ repo: REPO, is_dataset: dataset, selectable_items: items });
  ui.resetLiveReconcile();
  domNodes.delete('#selectableItems');
  cacheRequests.length = 0;
  storage.clear();
  if (Object.keys(initial).length) {
    storage.set(dataset ? DATASET_STORAGE : MODEL_STORAGE, JSON.stringify(initial));
  }
  cacheState = { status: cacheStatus, files: cacheFiles, error: cacheError, parseError: cacheParseError };
}

async function reconcile(items, { dataset = false } = {}) {
  const isDataset = !!dataset;
  ui.setCurrentAnalysis({ repo: REPO, is_dataset: isDataset, selectable_items: items });
  const data = { repo: REPO, is_dataset: isDataset, selectable_items: items };
  ui.refreshDlStatusFromJobs(REPO, items, isDataset);
  await ui.syncLocalAnalysisStatus(data);
  ui.refreshDlStatusFromJobs(REPO, items, isDataset);
  const html = ui.renderSelectableItems(items, 'selectableItems');
  return { map: ui.getDlStatus(REPO, isDataset), html };
}

// The live WebSocket path updates the already-rendered rows instead of
// re-rendering the analysis. These row stubs expose the same dataset/query
// surface updateSelectableRowStatuses uses.
function installRows(items, initialStatuses = {}) {
  const rows = items.map(item => {
    const statusKey = ui.selectableStatusKey(item);
    let chipHtml = '';
    const chip = {};
    Object.defineProperty(chip, 'outerHTML', {
      get() { return chipHtml; },
      set(value) { chipHtml = String(value); }
    });
    return {
      dataset: {
        statusKey: encodeURIComponent(statusKey),
        dlStatus: initialStatuses[statusKey] || ''
      },
      chip,
      querySelector(selector) { return selector === '.quant-dl-btn' ? this.chip : null; }
    };
  });
  domNodes.set('#selectableItems', { querySelectorAll() { return rows; } });
  return rows;
}

function completedJob(overrides = {}) {
  return {
    id: 'completed',
    repo: REPO,
    status: 'completed',
    filters: JOB_FILTERS,
    excludes: [],
    files: [
      { path: SHARD1, status: 'complete' },
      { path: SHARD2, status: 'complete' }
    ],
    ...overrides
  };
}

function artifactItem(files, label = 'Q4_0', filterValue = 'q4_0') {
  return {
    category: 'quantization',
    filter_value: filterValue,
    label,
    files
  };
}

function branchItem(revision, label = revision) {
  return {
    category: 'quantization',
    id: `exl-branch:${revision}`,
    revision,
    label
  };
}

function visionItem(files, label = 'MMProj', filterValue = 'mmproj') {
  return {
    category: 'vision_encoder',
    filter_value: filterValue,
    label,
    files
  };
}

function liveJob(overrides = {}) {
  return {
    id: 'live',
    repo: REPO,
    status: 'running',
    isDataset: false,
    filters: JOB_FILTERS,
    excludes: [],
    files: [],
    ...overrides
  };
}

function rowFor(html, label) {
  const chunks = String(html).split('<div class="quant-row');
  return chunks.find(chunk => chunk.includes(`>${label}<`)) || '';
}
function assertDone(html, label, message) {
  const row = label ? rowFor(html, label) : html;
  match(row, /quant-dl-btn-done/, `${message}: done chip`);
  doesNotMatch(row, /data-download=/, `${message}: no Download button`);
}
function assertIdle(html, label, message) {
  const row = label ? rowFor(html, label) : html;
  match(row, /data-download=/, `${message}: Download button`);
  doesNotMatch(row, /quant-dl-btn-done/, `${message}: no done chip`);
}
function assertRunning(html, message) {
  match(html, /quant-dl-btn-running/, `${message}: running chip`);
  doesNotMatch(html, /data-download=/, `${message}: no Download button while running`);
}
function assertQueued(html, message) {
  match(html, /quant-dl-btn-queued/, `${message}: queued chip`);
  doesNotMatch(html, /data-download=/, `${message}: no Download button while queued`);
}

const scenarios = [];
function scenario(name, fn) { scenarios.push({ name, fn }); }

// The regression: both shards requested, shard 2 excluded. The completed job
// and cache contain only shard 1, so the artifact must stay downloadable.
scenario('excluded shard is not done', async () => {
  const item = artifactItem(FILES);
  eq(ui.selectableStatusKey(item), ARTIFACT_KEY, 'artifact status key matches the job filters');
  reset({
    jobs: [completedJob({
      excludes: [SHARD2],
      files: [{ path: SHARD1, status: 'complete' }]
    })],
    cacheFiles: [SHARD1]
  });
  const { map, html } = await reconcile([item]);
  eq(map[ARTIFACT_KEY], undefined, 'incomplete artifact must not be done');
  assertIdle(html, '', 'incomplete artifact');
});

scenario('full completed job is done', async () => {
  reset({ jobs: [completedJob()], cacheFiles: FILES });
  const { map, html } = await reconcile([artifactItem(FILES)]);
  eq(map[ARTIFACT_KEY], 'done', 'full artifact must be done');
  assertDone(html, '', 'full artifact');
});

scenario('verified cache presence completes a partially planned job', async () => {
  reset({
    jobs: [completedJob({ files: [{ path: SHARD1, status: 'complete' }] })],
    cacheFiles: FILES
  });
  const { map, html } = await reconcile([artifactItem(FILES)]);
  eq(map[ARTIFACT_KEY], 'done', 'fully present cache artifact must be done');
  assertDone(html, '', 'cache-complete artifact');
});

scenario('job outcome does not complete an artifact when cache lookup errors', async () => {
  reset({ jobs: [completedJob()], cacheError: new Error('cache lookup failed') });
  const { map, html } = await reconcile([artifactItem(FILES)]);
  eq(map[ARTIFACT_KEY], undefined, 'cache-unknown artifact must stay not-done');
  assertIdle(html, '', 'cache-unknown artifact');
});

scenario('skipped in-flight shard with cache lookup error stays not done', async () => {
  // A BlobDownloading skip reaches the server as a completed per-file outcome;
  // the removed helper also accepted skipped. Either way, a completed job can
  // include a shard another process is still downloading, so it does not prove
  // the shard is cached.
  reset({
    jobs: [completedJob({
      files: [
        { path: SHARD1, status: 'complete' },
        { path: SHARD2, status: 'skipped' }
      ]
    })],
    cacheError: new Error('cache lookup failed')
  });
  const { map, html } = await reconcile([artifactItem(FILES)]);
  eq(map[ARTIFACT_KEY], undefined, 'unverified skip must not become done');
  assertIdle(html, '', 'skipped in-flight artifact');
});

scenario('cache 404 clears previously cached done', async () => {
  reset({
    jobs: [completedJob()],
    cacheStatus: 404,
    initial: { [ARTIFACT_KEY]: 'done' }
  });
  const { map, html } = await reconcile([artifactItem(FILES)]);
  eq(map[ARTIFACT_KEY], undefined, '404 must remain authoritative');
  assertIdle(html, '', '404 artifact');
});

scenario('cache error clears a previously stored artifact done', async () => {
  const item = artifactItem(FILES);

  // A transient cache failure cannot confirm the artifact is present, so a
  // previously stored done must not hide the Download button.
  reset({
    cacheError: new Error('cache lookup failed'),
    initial: { [ARTIFACT_KEY]: 'done' }
  });
  let { map, html } = await reconcile([item]);
  eq(map[ARTIFACT_KEY], undefined, 'cache error must clear a stale artifact done');
  assertIdle(html, '', 'cache-error artifact');

  // A malformed cache response reaches the same unverified-cache catch path.
  reset({
    cacheParseError: new SyntaxError('invalid cache response'),
    initial: { [ARTIFACT_KEY]: 'done' }
  });
  ({ map, html } = await reconcile([item]));
  eq(map[ARTIFACT_KEY], undefined, 'cache parse error must clear a stale artifact done');
  assertIdle(html, '', 'cache-parse-error artifact');

  // Cache presence is still authoritative for artifact done.
  reset({ cacheFiles: FILES });
  ({ map, html } = await reconcile([item]));
  eq(map[ARTIFACT_KEY], 'done', 'cache presence still completes the artifact');
  assertDone(html, '', 'cache-present artifact');

  // A successful response missing one requested file still clears stored done.
  reset({ cacheFiles: [SHARD1], initial: { [ARTIFACT_KEY]: 'done' } });
  ({ map, html } = await reconcile([item]));
  eq(map[ARTIFACT_KEY], undefined, 'missing file still clears a stored artifact done');
  assertIdle(html, '', 'missing-file artifact');

  // A 404 is still authoritative too.
  reset({ cacheStatus: 404, initial: { [ARTIFACT_KEY]: 'done' } });
  ({ map, html } = await reconcile([item]));
  eq(map[ARTIFACT_KEY], undefined, '404 still clears a stored artifact done');
  assertIdle(html, '', '404 artifact');

  // Legacy non-artifact keys keep their last-known-good status on cache error.
  const legacyFilter = { category: 'format', filter_value: 'Q4_K_M', label: 'Legacy Q4_K_M' };
  const legacyId = { category: 'format', id: 'legacy-id', label: 'Legacy ID' };
  reset({
    cacheError: new Error('cache lookup failed'),
    initial: { Q4_K_M: 'done', 'legacy-id': 'done' }
  });
  ({ map, html } = await reconcile([legacyFilter, legacyId]));
  eq(map['Q4_K_M'], 'done', 'legacy filter_value status survives a cache error');
  eq(map['legacy-id'], 'done', 'legacy id status survives a cache error');
  assertDone(html, 'Legacy Q4_K_M', 'legacy filter_value row');
  assertDone(html, 'Legacy ID', 'legacy id row');
});

scenario('running status is preserved', async () => {
  reset({ jobs: [completedJob({ status: 'running', files: [] })], cacheFiles: FILES });
  const { map, html } = await reconcile([artifactItem(FILES)]);
  eq(map[ARTIFACT_KEY], 'running', 'running status must be preserved');
  assertRunning(html, 'running artifact');
});

scenario('queued status is preserved', async () => {
  reset({ jobs: [completedJob({ status: 'queued', files: [] })], cacheFiles: FILES });
  const { map, html } = await reconcile([artifactItem(FILES)]);
  eq(map[ARTIFACT_KEY], 'queued', 'queued status must be preserved');
  assertQueued(html, 'queued artifact');
});

scenario('missing file evidence is not inferred from filters', async () => {
  reset({
    jobs: [completedJob({ files: undefined })],
    cacheError: new Error('cache lookup failed')
  });
  const { map, html } = await reconcile([artifactItem(FILES)]);
  eq(map[ARTIFACT_KEY], undefined, 'missing job file evidence must not become done');
  assertIdle(html, '', 'missing job evidence artifact');
});

scenario('legacy non-artifact token status is unchanged', async () => {
  const item = { category: 'format', filter_value: 'Q4_K_M', label: 'Legacy Q4_K_M' };
  reset({
    jobs: [completedJob({ filters: ['Q4_K_M'], files: undefined })],
    cacheFiles: []
  });
  const { map, html } = await reconcile([item]);
  eq(map['Q4_K_M'], 'done', 'legacy token remains done from a completed job');
  assertDone(html, '', 'legacy token row');
});

scenario('legacy token status survives cache 404', async () => {
  const item = { category: 'format', filter_value: 'Q4_K_M', label: 'Legacy Q4_K_M' };
  reset({
    jobs: [completedJob({ filters: ['Q4_K_M'], files: undefined })],
    cacheStatus: 404,
    initial: { 'Q4_K_M': 'done' }
  });
  const { map, html } = await reconcile([item]);
  eq(map['Q4_K_M'], 'done', 'legacy token remains done after 404 reconciliation');
  assertDone(html, '', 'legacy 404 token row');
});

scenario('branch variants isolate status by revision and never infer done from unqualified cache', async () => {
  const branchA = branchItem('refs/bpw/4.0', 'EXL3 4.0 BPW');
  const branchB = branchItem('refs/bpw/5.0', 'EXL3 5.0 BPW');
  const keyA = ui.selectableStatusKey(branchA);
  const keyB = ui.selectableStatusKey(branchB);
  eq(keyA !== keyB, true, 'different branch revisions have independent status keys');

  // The cache endpoint reports a repository snapshot without revision proof;
  // even old stored done values cannot disable either full-branch row.
  reset({
    cacheFiles: FILES,
    initial: { [keyA]: 'done', [keyB]: 'done' }
  });
  let result = await reconcile([branchA, branchB]);
  eq(result.map[keyA], undefined, 'unqualified cache cannot retain branch A done');
  eq(result.map[keyB], undefined, 'unqualified cache cannot retain branch B done');
  assertIdle(result.html, branchA.label, 'branch A without revision-specific cache evidence');
  assertIdle(result.html, branchB.label, 'branch B without revision-specific cache evidence');

  // Only a full-branch job at the exact revision marks the matching row active.
  reset({
    jobs: [liveJob({ status: 'running', revision: branchA.revision, filters: [] })],
    cacheStatus: 404
  });
  result = await reconcile([branchA, branchB]);
  eq(result.map[keyA], 'running', 'matching revision job marks branch A running');
  eq(result.map[keyB], undefined, 'branch A job does not mark branch B running');
  match(rowFor(result.html, branchA.label), /quant-dl-btn-running/, 'matching branch row shows running');
  assertIdle(result.html, branchB.label, 'different branch row stays downloadable');

  reset({
    jobs: [liveJob({ status: 'queued', revision: branchB.revision, filters: [] })],
    cacheStatus: 404
  });
  result = await reconcile([branchA, branchB]);
  eq(result.map[keyA], undefined, 'branch B queued job does not mark branch A');
  eq(result.map[keyB], 'queued', 'matching revision job marks branch B queued');
  assertQueued(rowFor(result.html, branchB.label), 'matching branch row shows queued');

  // A filtered request is not the whole selected branch and must not be
  // represented as downloading the branch-backed selectable row.
  reset({
    jobs: [liveJob({ status: 'queued', revision: branchA.revision, filters: ['config.json'] })],
    cacheStatus: 404
  });
  result = await reconcile([branchA]);
  eq(result.map[keyA], undefined, 'filtered job does not mark a full-branch row queued');
  assertIdle(result.html, branchA.label, 'filtered job leaves the full-branch row downloadable');

  reset({
    jobs: [liveJob({ status: 'queued', revision: branchA.revision, filters: [], excludes: ['*.md'] })],
    cacheStatus: 404
  });
  result = await reconcile([branchA]);
  eq(result.map[keyA], undefined, 'job excluding branch support files does not mark the full-branch row queued');

  // Completion is not revision-specific cache evidence; it clears the active
  // chip but does not make the branch row permanently done.
  reset({
    jobs: [liveJob({ status: 'completed', revision: branchA.revision, filters: [] })],
    cacheFiles: FILES,
    initial: { [keyA]: 'queued' }
  });
  result = await reconcile([branchA]);
  eq(result.map[keyA], undefined, 'completed branch job does not infer done without revision-specific cache evidence');
  assertIdle(result.html, branchA.label, 'completed branch row remains available');
});

scenario('live branch job updates only its matching selectable row', async () => {
  const branchA = branchItem('refs/bpw/4.0', 'EXL3 4.0 BPW');
  const branchB = branchItem('refs/bpw/5.0', 'EXL3 5.0 BPW');
  const keyA = ui.selectableStatusKey(branchA);
  const keyB = ui.selectableStatusKey(branchB);
  reset({ items: [branchA, branchB], cacheStatus: 404 });
  const rows = installRows([branchA, branchB]);

  ui.handleWSMessage({
    type: 'job_update',
    data: liveJob({ id: 'branch-running', status: 'running', revision: branchA.revision, filters: [] })
  });
  await ui.flushLiveReconcile();

  eq(ui.getDlStatus(REPO, false)[keyA], 'running', 'live matching revision updates its status key');
  eq(ui.getDlStatus(REPO, false)[keyB], undefined, 'live job does not update another revision key');
  eq(rows[0].dataset.dlStatus, 'running', 'live matching row is updated in place');
  eq(rows[1].dataset.dlStatus, '', 'other branch row remains idle');
  match(rows[0].chip.outerHTML, /quant-dl-btn-running/, 'matching branch row shows running chip');
  assertIdle(ui.renderSelectableItems([branchA, branchB], 'selectableItems'), branchB.label, 'other branch row remains downloadable');
});

scenario('distinct artifacts sharing a quant/filter stay independent', async () => {
  const a1 = 'model-A-Q4_K_M-00001-of-00002.gguf';
  const a2 = 'model-A-Q4_K_M-00002-of-00002.gguf';
  const b1 = 'model-B-Q4_K_M-00001-of-00002.gguf';
  const b2 = 'model-B-Q4_K_M-00002-of-00002.gguf';
  const itemA = artifactItem([a1, a2], 'Artifact A', 'q4_k_m');
  const itemB = artifactItem([b1, b2], 'Artifact B', 'q4_k_m');
  reset({
    jobs: [completedJob({
      filters: [a1, a2].map(name => './' + name),
      files: [{ path: a1, status: 'complete' }, { path: a2, status: 'complete' }]
    })],
    cacheFiles: [a1, a2]
  });
  const { map, html } = await reconcile([itemA, itemB]);
  eq(map[ui.selectableStatusKey(itemA)], 'done', 'artifact A is done');
  eq(map[ui.selectableStatusKey(itemB)], undefined, 'artifact B stays idle');
  assertDone(html, 'Artifact A', 'artifact A');
  assertIdle(html, 'Artifact B', 'artifact B');
});

scenario('live progress and completion reconcile open analysis rows', async () => {
  const item = artifactItem(FILES);
  reset({ cacheFiles: FILES, initial: { [ARTIFACT_KEY]: 'queued' }, items: [item] });
  const rows = installRows([item], { [ARTIFACT_KEY]: 'queued' });
  assertQueued(ui.renderSelectableItems([item], 'selectableItems'), 'staged queued row');

  // Progress updates move the chip without paying for another cache lookup,
  // and a burst of them coalesces into one scheduled reconciliation.
  const timersBefore = timers.length;
  ui.handleWSMessage({ type: 'job_update', data: liveJob({ status: 'running' }) });
  ui.handleWSMessage({ type: 'job_update', data: liveJob({ status: 'running' }) });
  eq(timers.length, timersBefore + 1, 'progress burst coalesces into one reconcile timer');
  await ui.flushLiveReconcile();
  eq(ui.getDlStatus(REPO, false)[ARTIFACT_KEY], 'running', 'live running status reaches storage');
  eq(lastCacheRequest(), '', 'running progress must not re-read cache presence');
  eq(rows[0].dataset.dlStatus, 'running', 'live running status updates the rendered row');
  match(rows[0].chip.outerHTML, /quant-dl-btn-running/, 'running chip replaces queued');
  doesNotMatch(rows[0].chip.outerHTML, /quant-dl-btn-queued/, 'no stale queued chip while running');

  // Completion reconciles again; cache presence is what finalizes done.
  ui.handleWSMessage({
    type: 'job_update',
    data: liveJob({
      status: 'completed',
      files: [
        { path: SHARD1, status: 'complete' },
        { path: SHARD2, status: 'complete' }
      ]
    })
  });
  await ui.flushLiveReconcile();
  match(lastCacheRequest(), /^\/api\/cache\/owner\/model\?type=model$/, 'completion re-reads the model namespace');
  eq(ui.getDlStatus(REPO, false)[ARTIFACT_KEY], 'done', 'cache presence finalizes done after completion');
  eq(rows[0].dataset.dlStatus, 'done', 'completed row leaves running/queued state');
  match(rows[0].chip.outerHTML, /quant-dl-btn-done/, 'done chip after cache presence');
  doesNotMatch(rows[0].chip.outerHTML, /quant-dl-btn-queued|quant-dl-btn-running/, 'no stale active chip after completion');
});

scenario('initial socket snapshot reconciles a job that finished while disconnected', async () => {
  const item = artifactItem(FILES);
  reset({ cacheFiles: FILES, initial: { [ARTIFACT_KEY]: 'queued' }, items: [item] });
  const rows = installRows([item], { [ARTIFACT_KEY]: 'queued' });
  ui.handleWSMessage({ type: 'init', data: { jobs: [completedJob({ id: 'offline' })] } });
  await ui.flushLiveReconcile();
  match(lastCacheRequest(), /^\/api\/cache\/owner\/model\?type=model$/, 'initial snapshot re-reads cache presence');
  eq(ui.getDlStatus(REPO, false)[ARTIFACT_KEY], 'done', 'initial snapshot completes from cache');
  eq(rows[0].dataset.dlStatus, 'done', 'initial snapshot updates the open rows');
});

scenario('post-load reconciliation catches a completion from the load window', async () => {
  const item = artifactItem(FILES);
  const previousItem = artifactItem(['previous-Q4_0.gguf'], 'Previous', 'previous');
  ui.state.jobs.clear();
  ui.state.settings = {};
  ui.setCurrentAnalysis({ repo: 'owner/previous', is_dataset: false, selectable_items: [previousItem] });
  ui.resetLiveReconcile();
  domNodes.delete('#selectableItems');
  cacheRequests.length = 0;
  storage.clear();
  cacheState = { status: 200, files: FILES, error: null, parseError: null };
  const rows = installRows([item], { [ARTIFACT_KEY]: 'queued' });

  // The new analysis has not been committed yet, so handleWSMessage compares
  // this completion against the previous analysis and cannot schedule it.
  ui.handleWSMessage({ type: 'job_update', data: liveJob({ id: 'load-race', status: 'completed', files: [] }) });
  eq(ui.hasLiveReconcileTimer(), false, 'completion during load is not scheduled against the previous analysis');

  // The successful load commits currentAnalysis and renders, then schedules
  // the post-load reconciliation used by loadRepositoryDetails.
  ui.setCurrentAnalysis({ repo: REPO, is_dataset: false, selectable_items: [item] });
  ui.reconcileAnalysisAfterLoad();
  eq(ui.hasLiveReconcileTimer(), true, 'post-load reconciliation is scheduled');
  await ui.flushLiveReconcile();

  match(lastCacheRequest(), /^\/api\/cache\/owner\/model\?type=model$/, 'post-load reconciliation reads the committed namespace');
  eq(cacheRequests.length, 1, 'post-load reconciliation uses one coalesced cache read');
  eq(ui.getDlStatus(REPO, false)[ARTIFACT_KEY], 'done', 'completion from the load window is finalized from cache');
  eq(rows[0].dataset.dlStatus, 'done', 'post-load reconciliation updates the rendered row');
  eq(storage.has(MODEL_STORAGE), true, 'post-load status is stored in the model namespace');
});

scenario('live completion with 404 clears the active chip', async () => {
  const item = artifactItem(FILES);
  reset({ cacheStatus: 404, initial: { [ARTIFACT_KEY]: 'queued' }, items: [item] });
  const rows = installRows([item], { [ARTIFACT_KEY]: 'queued' });
  ui.handleWSMessage({ type: 'job_update', data: liveJob({ status: 'completed', files: [] }) });
  await ui.flushLiveReconcile();
  eq(ui.getDlStatus(REPO, false)[ARTIFACT_KEY], undefined, '404 leaves the artifact not-done');
  eq(rows[0].dataset.dlStatus, '', 'completed job with 404 leaves no active chip');
  match(rows[0].chip.outerHTML, /data-download=/, '404 restores the Download button');
  doesNotMatch(rows[0].chip.outerHTML, /quant-dl-btn-queued|quant-dl-btn-running/, 'no stale queued chip');
});

scenario('live completion with a cache error clears a stale artifact done', async () => {
  const item = artifactItem(FILES);
  reset({ cacheError: new Error('cache lookup failed'), initial: { [ARTIFACT_KEY]: 'done' }, items: [item] });
  const rows = installRows([item], { [ARTIFACT_KEY]: 'done' });
  ui.handleWSMessage({ type: 'job_update', data: liveJob({ status: 'completed', files: [] }) });
  await ui.flushLiveReconcile();
  eq(ui.getDlStatus(REPO, false)[ARTIFACT_KEY], undefined, 'cache error clears stale artifact done');
  eq(rows[0].dataset.dlStatus, '', 'cache error restores the Download control');
  match(rows[0].chip.outerHTML, /data-download=/, 'cache-error row is downloadable');
});

scenario('combined quant and mmproj job marks both artifact rows active', async () => {
  const quant = artifactItem(FILES, 'Quant', 'q4_0');
  const mmproj = visionItem([MMPROJ]);
  reset({ items: [quant, mmproj] });
  const rows = installRows([quant, mmproj]);
  const combined = [...JOB_FILTERS, './' + MMPROJ];

  ui.handleWSMessage({ type: 'job_update', data: liveJob({ status: 'queued', filters: combined }) });
  await ui.flushLiveReconcile();
  eq(ui.getDlStatus(REPO, false)[ui.selectableStatusKey(quant)], 'queued', 'quant queued by combined job');
  eq(ui.getDlStatus(REPO, false)[ui.selectableStatusKey(mmproj)], 'queued', 'mmproj queued by combined job');
  eq(rows[0].dataset.dlStatus, 'queued', 'quant row shows queued');
  eq(rows[1].dataset.dlStatus, 'queued', 'mmproj row shows queued');

  const queuedHtml = ui.renderSelectableItems([quant, mmproj], 'selectableItems');
  assertQueued(rowFor(queuedHtml, 'Quant'), 'combined queued quant');
  assertQueued(rowFor(queuedHtml, 'MMProj'), 'combined queued mmproj');

  ui.handleWSMessage({ type: 'job_update', data: liveJob({ status: 'running', filters: combined }) });
  await ui.flushLiveReconcile();
  eq(ui.getDlStatus(REPO, false)[ui.selectableStatusKey(quant)], 'running', 'combined job moves quant to running');
  eq(ui.getDlStatus(REPO, false)[ui.selectableStatusKey(mmproj)], 'running', 'combined job moves mmproj to running');
  eq(rows[0].dataset.dlStatus, 'running', 'quant row shows running');
  eq(rows[1].dataset.dlStatus, 'running', 'mmproj row shows running');
});

scenario('completed combined job does not finalize artifacts without cache presence', async () => {
  const quant = artifactItem(FILES, 'Quant', 'q4_0');
  const mmproj = visionItem([MMPROJ]);
  reset({ cacheStatus: 404, items: [quant, mmproj] });
  const rows = installRows([quant, mmproj]);
  ui.handleWSMessage({
    type: 'job_update',
    data: liveJob({ status: 'completed', filters: [...JOB_FILTERS, './' + MMPROJ] })
  });
  await ui.flushLiveReconcile();
  eq(ui.getDlStatus(REPO, false)[ui.selectableStatusKey(quant)], undefined, 'completed job never makes quant done');
  eq(ui.getDlStatus(REPO, false)[ui.selectableStatusKey(mmproj)], undefined, 'completed job never makes mmproj done');
  eq(rows[0].dataset.dlStatus, '', 'quant row remains downloadable');
  eq(rows[1].dataset.dlStatus, '', 'mmproj row remains downloadable');
});

scenario('quant-only job does not mark the mmproj artifact', async () => {
  const quant = artifactItem(FILES, 'Quant', 'q4_0');
  const mmproj = visionItem([MMPROJ]);
  reset({ items: [quant, mmproj] });
  const rows = installRows([quant, mmproj]);
  ui.handleWSMessage({ type: 'job_update', data: liveJob({ status: 'running', filters: JOB_FILTERS }) });
  await ui.flushLiveReconcile();
  eq(ui.getDlStatus(REPO, false)[ui.selectableStatusKey(quant)], 'running', 'quant-only job marks quant');
  eq(ui.getDlStatus(REPO, false)[ui.selectableStatusKey(mmproj)], undefined, 'quant-only job must not mark mmproj');
  eq(rows[0].dataset.dlStatus, 'running', 'quant row active');
  eq(rows[1].dataset.dlStatus, '', 'mmproj row stays idle');
});

scenario('mmproj-only upstream job does not mark the quant artifact', async () => {
  const quant = artifactItem(FILES, 'Quant', 'q4_0');
  const mmproj = visionItem([MMPROJ]);
  reset({ items: [quant, mmproj] });
  const rows = installRows([quant, mmproj]);
  ui.handleWSMessage({
    type: 'job_update',
    data: liveJob({ repo: 'upstream/base', localRepo: REPO, status: 'running', filters: ['./' + MMPROJ] })
  });
  await ui.flushLiveReconcile();
  eq(ui.getDlStatus(REPO, false)[ui.selectableStatusKey(mmproj)], 'running', 'upstream mmproj job marks mmproj');
  eq(ui.getDlStatus(REPO, false)[ui.selectableStatusKey(quant)], undefined, 'mmproj-only job must not mark quant');
  eq(rows[0].dataset.dlStatus, '', 'quant row stays idle');
  eq(rows[1].dataset.dlStatus, 'running', 'mmproj row active');
});

scenario('model and dataset cache namespaces stay isolated', async () => {
  const item = artifactItem(FILES);
  reset({ cacheFiles: FILES });
  let result = await reconcile([item]);
  eq(result.map[ARTIFACT_KEY], 'done', 'model cache presence is done');
  match(lastCacheRequest(), /^\/api\/cache\/owner\/model\?type=model$/, 'model analysis requests type=model');
  eq(storage.has(MODEL_STORAGE), true, 'model status is stored under the model namespace');
  eq(storage.has(DATASET_STORAGE), false, 'dataset namespace is untouched');

  // Switch the open analysis to the dataset namespace without clearing storage.
  ui.state.jobs.clear();
  ui.setCurrentAnalysis({ repo: REPO, is_dataset: true, selectable_items: [item] });
  ui.resetLiveReconcile();
  cacheRequests.length = 0;
  cacheState = { status: 404, files: [], error: null, parseError: null };
  result = await reconcile([item], { dataset: true });
  match(lastCacheRequest(), /^\/api\/cache\/owner\/model\?type=dataset$/, 'dataset analysis requests type=dataset');
  eq(result.map[ARTIFACT_KEY], undefined, 'dataset 404 is not done');
  eq(ui.getDlStatus(REPO, false)[ARTIFACT_KEY], 'done', 'dataset sync leaves the model namespace intact');
  eq(storage.has(DATASET_STORAGE), false, 'empty dataset status is removed from its own key');

  // A model job must not populate the dataset namespace, and vice versa.
  ui.handleWSMessage({ type: 'job_update', data: liveJob({ id: 'model-job', status: 'running' }) });
  await ui.flushLiveReconcile();
  eq(ui.getDlStatus(REPO, true)[ARTIFACT_KEY], undefined, 'model job does not cross into dataset status');

  ui.handleWSMessage({ type: 'job_update', data: liveJob({ id: 'dataset-job', isDataset: true, status: 'running' }) });
  await ui.flushLiveReconcile();
  eq(ui.getDlStatus(REPO, true)[ARTIFACT_KEY], 'running', 'dataset job updates the dataset namespace');
  eq(ui.getDlStatus(REPO, false)[ARTIFACT_KEY], 'done', 'model status remains its own value');
});

scenario('untyped legacy storage is ignored for both namespaces', async () => {
  const artifact = artifactItem(FILES);
  const modelToken = { category: 'format', filter_value: 'Q4_K_M', label: 'Model Legacy' };
  const datasetToken = { category: 'format', filter_value: 'train', label: 'Dataset Legacy' };
  reset({});
  storage.clear();
  const legacyRaw = JSON.stringify({ Q4_K_M: 'done', train: 'done' });
  storage.set(LEGACY_STORAGE, legacyRaw);

  // The old untyped key has no namespace, so neither lookup may claim it.
  eq(ui.getDlStatus(REPO, false)['Q4_K_M'], undefined, 'legacy status is ignored for model lookup');
  eq(ui.getDlStatus(REPO, true)['train'], undefined, 'legacy status is ignored for dataset lookup');
  eq(storage.get(LEGACY_STORAGE), legacyRaw, 'legacy untyped key is left untouched');
  eq(storage.has(MODEL_STORAGE), false, 'model namespace is not populated from legacy');
  eq(storage.has(DATASET_STORAGE), false, 'dataset namespace is not populated from legacy');

  ui.setCurrentAnalysis({ repo: REPO, is_dataset: false, selectable_items: [modelToken] });
  assertIdle(ui.renderSelectableItems([modelToken], 'selectableItems'), 'Model Legacy', 'legacy model token row');
  ui.setCurrentAnalysis({ repo: REPO, is_dataset: true, selectable_items: [datasetToken] });
  assertIdle(ui.renderSelectableItems([datasetToken], 'selectableItems'), 'Dataset Legacy', 'legacy dataset token row');

  // Type-scoped keys remain the only source and stay isolated.
  storage.set(MODEL_STORAGE, JSON.stringify({ Q4_K_M: 'done', [ARTIFACT_KEY]: 'done' }));
  storage.set(DATASET_STORAGE, JSON.stringify({ train: 'done', [ARTIFACT_KEY]: 'done' }));
  eq(ui.getDlStatus(REPO, false)['Q4_K_M'], 'done', 'model namespace keeps its own token');
  eq(ui.getDlStatus(REPO, false)['train'], undefined, 'dataset token does not cross into model');
  eq(ui.getDlStatus(REPO, true)['train'], 'done', 'dataset namespace keeps its own token');
  eq(ui.getDlStatus(REPO, true)['Q4_K_M'], undefined, 'model token does not cross into dataset');
  eq(storage.get(LEGACY_STORAGE), legacyRaw, 'legacy key remains untouched after typed reads');

  // Cache presence still finalizes model done; a dataset 404 clears only the
  // dataset namespace and leaves the model namespace intact.
  cacheState = { status: 200, files: FILES, error: null, parseError: null };
  let result = await reconcile([artifact]);
  eq(result.map[ARTIFACT_KEY], 'done', 'model cache presence finalizes done');
  cacheState = { status: 404, files: [], error: null, parseError: null };
  result = await reconcile([artifact], { dataset: true });
  eq(result.map[ARTIFACT_KEY], undefined, 'dataset 404 clears its artifact done');
  eq(ui.getDlStatus(REPO, false)[ARTIFACT_KEY], 'done', 'model artifact done survives dataset 404');
  eq(storage.get(LEGACY_STORAGE), legacyRaw, 'legacy key is untouched after 404');

  // A cache error clears only the dataset artifact's stale done; the typed
  // dataset token keeps its last-known-good value.
  storage.set(DATASET_STORAGE, JSON.stringify({ train: 'done', [ARTIFACT_KEY]: 'done' }));
  cacheState = { status: 200, files: [], error: new Error('cache lookup failed'), parseError: null };
  result = await reconcile([artifact], { dataset: true });
  eq(result.map[ARTIFACT_KEY], undefined, 'dataset cache error clears stale artifact done');
  eq(result.map['train'], 'done', 'dataset cache error preserves typed token status');
  eq(ui.getDlStatus(REPO, false)[ARTIFACT_KEY], 'done', 'model artifact done survives dataset cache error');
  eq(storage.get(LEGACY_STORAGE), legacyRaw, 'legacy key is untouched after cache error');

  // A successful response missing one file clears the dataset done as before.
  storage.set(DATASET_STORAGE, JSON.stringify({ train: 'done', [ARTIFACT_KEY]: 'done' }));
  cacheState = { status: 200, files: [SHARD1], error: null, parseError: null };
  result = await reconcile([artifact], { dataset: true });
  eq(result.map[ARTIFACT_KEY], undefined, 'dataset missing file clears artifact done');
  eq(result.map['train'], 'done', 'missing-file sync leaves typed token status');
  eq(storage.get(LEGACY_STORAGE), legacyRaw, 'legacy key is untouched after missing file');

  // Cache presence still finalizes done after the clearing paths.
  cacheState = { status: 200, files: FILES, error: null, parseError: null };
  result = await reconcile([artifact], { dataset: true });
  eq(result.map[ARTIFACT_KEY], 'done', 'dataset cache presence finalizes done');
  eq(ui.getDlStatus(REPO, false)[ARTIFACT_KEY], 'done', 'model artifact done remains isolated');
  eq(storage.get(LEGACY_STORAGE), legacyRaw, 'legacy key is untouched after cache presence');
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
