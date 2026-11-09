// Run through TestSelectableArtifactStatusUI: real app.js status reconciliation.
//
// The harness loads the shipped app.js inside Node's VM with DOM, localStorage
// and cache-fetch stubs. It drives the production sequence in
// loadRepositoryDetails/renderAnalysisResult: refreshDlStatusFromJobs runs
// before syncLocalAnalysisStatus, then renderSelectableItems reads the result.
// It also feeds real job_update/init WebSocket messages through
// handleWSMessage and flushes the coalesced live reconciliation.
//
// The EXL branch-variant path is covered the same way: a branch item is keyed
// by its revision, its presence comes from the qualified branch-variant lookup
// (GET /api/cache/<repo>?branchVariant=true&revision=...) instead of the
// repository file list, and its download control posts a whole-branch request.
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
// An EXL branch variant is one whole revision. Its status is keyed by the
// revision instead of a file set, and its presence comes from the qualified
// branch-variant lookup instead of the repository file list.
const BRANCH_REVISION = '4.00bpw';
const BRANCH_COMMIT = '2222222222222222222222222222222222222222';
const BRANCH_KEY = `branchVariant:${BRANCH_REVISION}`;

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

// The download control previews its destination and then posts the download
// request. The captured body is what the server's branch-variant admission
// has to accept.
const downloadRequests = [];

let cacheState = { status: 200, files: [], error: null, parseError: null };
let branchState = { status: 200, downloaded: false, error: null, parseError: null };

// The presence response mirrors the documented shape of
// GET /api/cache/<repo>?branchVariant=true&revision=<rev>: a qualified lookup
// for one revision, not the repository file list.
function branchPresence(revision, downloaded) {
  const presence = {
    repo: REPO,
    type: 'model',
    branchVariant: true,
    revision,
    downloaded,
    downloadStatus: downloaded ? 'complete' : 'missing',
    storageMode: 'hf-cache',
    destinationRoot: '/cache/hub/models--owner--model',
    expectedFiles: downloaded ? 1 : 0,
    files: downloaded ? [{ name: 'config.json', size: 123 }] : []
  };
  if (downloaded) {
    presence.commit = BRANCH_COMMIT;
    presence.artifactPath = `/cache/hub/models--owner--model/.hfdesk-variants/${revision}/${BRANCH_COMMIT}/hfd.yaml`;
  }
  return presence;
}

const fetch = async (rawPath, options = {}) => {
  const requestPath = String(rawPath);
  if (requestPath.startsWith('/api/cache/')) {
    cacheRequests.push(requestPath);
    const query = new URLSearchParams(requestPath.split('?')[1] || '');
    if (query.get('branchVariant') === 'true') {
      if (branchState.error) throw branchState.error;
      const revision = query.get('revision') || '';
      // The server rejects a presence query that names no branch revision.
      if (!revision) {
        return { status: 400, ok: false, json: async () => ({ error: 'Invalid revision' }) };
      }
      return {
        status: branchState.status,
        ok: branchState.status >= 200 && branchState.status < 300,
        json: async () => {
          if (branchState.parseError) throw branchState.parseError;
          return branchPresence(revision, branchState.downloaded);
        }
      };
    }
    if (cacheState.error) throw cacheState.error;
    return {
      status: cacheState.status,
      ok: cacheState.status >= 200 && cacheState.status < 300,
      json: async () => {
        if (cacheState.parseError) throw cacheState.parseError;
        return { files: cacheState.files.map(name => ({ name })) };
      }
    };
  }
  if (requestPath === '/api/diskfree') {
    return {
      status: 200,
      ok: true,
      json: async () => ({ free: 1024 * 1024 * 1024, path: '/cache/hub/models--owner--model' })
    };
  }
  if (requestPath === '/api/download') {
    const body = options?.body ? JSON.parse(options.body) : null;
    downloadRequests.push({ path: requestPath, body });
    return {
      status: 200,
      ok: true,
      json: async () => ({
        id: 'branch-download',
        repo: body?.repo,
        revision: body?.revision,
        status: 'queued',
        branchVariant: body?.branchVariant === true,
        filters: [],
        excludes: []
      })
    };
  }
  throw new Error('unexpected fetch: ' + rawPath);
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
    renderAnalysisWarnings,
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

function reset({
  jobs = [],
  cacheFiles = [],
  cacheStatus = 200,
  cacheError = null,
  cacheParseError = null,
  initial = {},
  dataset = false,
  items = [],
  branchDownloaded = false,
  branchStatus = 200,
  branchError = null,
  branchParseError = null
} = {}) {
  ui.state.jobs.clear();
  jobs.forEach((job, index) => ui.state.jobs.set(job.id || `job-${index}`, job));
  ui.state.settings = {};
  ui.setCurrentAnalysis({ repo: REPO, is_dataset: dataset, selectable_items: items });
  ui.resetLiveReconcile();
  domNodes.delete('#selectableItems');
  cacheRequests.length = 0;
  downloadRequests.length = 0;
  storage.clear();
  if (Object.keys(initial).length) {
    storage.set(dataset ? DATASET_STORAGE : MODEL_STORAGE, JSON.stringify(initial));
  }
  cacheState = { status: cacheStatus, files: cacheFiles, error: cacheError, parseError: cacheParseError };
  branchState = { status: branchStatus, downloaded: branchDownloaded, error: branchError, parseError: branchParseError };
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

function visionItem(files, label = 'MMProj', filterValue = 'mmproj') {
  return {
    category: 'vision_encoder',
    filter_value: filterValue,
    label,
    files
  };
}

// An EXL branch variant is offered as one whole-revision item. It selects no
// files: the branch is named by its revision.
function branchItem(revision = BRANCH_REVISION, label = revision) {
  return {
    id: `exl-branch:${revision}`,
    category: 'quantization',
    label,
    revision,
    branch_variant: true,
    method: 'exl3',
    bits_per_weight: 4,
    head_bits: 5
  };
}

// A branch job is a whole-revision download: it selects no files.
function branchJob(overrides = {}) {
  return {
    id: 'branch',
    repo: REPO,
    revision: BRANCH_REVISION,
    status: 'running',
    isDataset: false,
    branchVariant: true,
    filters: [],
    excludes: [],
    files: [],
    ...overrides
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

// A done branch variant keeps its download control: the user can download the
// branch again. The done marker is the chip class, not the absence of a
// button, so the branch row needs its own assertion.
function assertBranchDone(html, label, message) {
  const row = label ? rowFor(html, label) : html;
  match(row, /quant-dl-btn-done/, `${message}: done chip`);
  doesNotMatch(row, /quant-dl-btn-running|quant-dl-btn-queued/, `${message}: no active chip`);
}

// The rendered control carries the request payload the download button posts.
function downloadPayload(rowHtml) {
  const found = /data-download="([^"]*)"/.exec(String(rowHtml));
  if (!found) throw new Error('row has no download control');
  return JSON.parse(decodeURIComponent(found[1]));
}

function downloadButton(payload) {
  return {
    dataset: { download: encodeURIComponent(JSON.stringify(payload)) },
    closest: () => null
  };
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

scenario('analysis warning renders escaped message, causes, and retry', async () => {
  eq(ui.renderAnalysisWarnings({}), '', 'no warnings renders nothing');
  eq(ui.renderAnalysisWarnings({ analysis_warnings: [{ code: 'other', message: 'ignore' }] }), '', 'unrelated warning code is not rendered');
  const html = ui.renderAnalysisWarnings({
    analysis_warnings: [{
      code: 'exl_branch_discovery_incomplete',
      message: 'Branch discovery incomplete <retry>',
      causes: ['header_unavailable', 'probe_timeout'],
      retryable: true
    }]
  });
  match(html, /Branch discovery incomplete &lt;retry&gt;/, 'warning message is escaped');
  match(html, /Safetensors header unavailable/, 'header cause label is rendered');
  match(html, /Header probe timed out/, 'probe cause label is rendered');
  match(html, /data-analysis-warning-retry/, 'retryable warning offers a retry button');
  doesNotMatch(html, /<retry>/, 'warning message is not injected as markup');

  const pickerHtml = ui.renderAnalysisWarnings({
    analysis_warnings: [{
      code: 'exl_branch_discovery_incomplete',
      message: 'Branch discovery incomplete',
      causes: ['header_unavailable'],
      retryable: true
    }]
  }, { showRetry: false });
  doesNotMatch(pickerHtml, /data-analysis-warning-retry/, 'revision picker warning omits the retry button');
});

scenario('branch variant reported present is done', async () => {
  const item = branchItem();
  eq(ui.selectableStatusKey(item), BRANCH_KEY, 'a branch item is keyed by its revision');
  reset({ branchDownloaded: true, items: [item] });
  const { map, html } = await reconcile([item]);
  eq(map[BRANCH_KEY], 'done', 'a present branch variant must be done');
  assertBranchDone(html, BRANCH_REVISION, 'present branch variant');
  match(lastCacheRequest(), /^\/api\/cache\/owner\/model\?branchVariant=true&revision=/, 'branch presence uses the qualified lookup');
  const branchQuery = new URLSearchParams({ branchVariant: 'true', revision: BRANCH_REVISION }).toString();
  eq(lastCacheRequest(), `/api/cache/${REPO}?${branchQuery}`, 'the qualified lookup names the branch revision');
  eq(cacheRequests.length, 1, 'a branch-only analysis does not read the repository file list');
});

scenario('running branch job is not done', async () => {
  const item = branchItem();
  reset({ jobs: [branchJob({ status: 'running' })], items: [item] });
  const { map, html } = await reconcile([item]);
  eq(map[BRANCH_KEY], 'running', 'a running branch job must keep the running state');
  assertRunning(rowFor(html, BRANCH_REVISION), 'running branch variant');

  // A branch whose download is still running is not finished, even when the
  // presence lookup already reports the variant as downloaded.
  reset({ jobs: [branchJob({ status: 'running' })], branchDownloaded: true, items: [item] });
  const active = await reconcile([item]);
  eq(active.map[BRANCH_KEY], 'running', 'a running branch job stays running while the variant is present');
  assertRunning(rowFor(active.html, BRANCH_REVISION), 'running branch variant with presence');
});

scenario('queued branch job is not done', async () => {
  const item = branchItem();
  reset({ jobs: [branchJob({ status: 'queued' })], items: [item] });
  const { map, html } = await reconcile([item]);
  eq(map[BRANCH_KEY], 'queued', 'a queued branch job must keep the queued state');
  assertQueued(rowFor(html, BRANCH_REVISION), 'queued branch variant');
});

scenario('branch job for another revision does not mark the branch', async () => {
  const item = branchItem();
  const other = branchItem('8.00bpw');
  reset({
    jobs: [branchJob({ id: 'other-branch', revision: '8.00bpw', status: 'running' })],
    items: [item, other]
  });
  const { map, html } = await reconcile([item, other]);
  eq(map[BRANCH_KEY], undefined, 'another revision\'s job must not mark this branch');
  eq(map[ui.selectableStatusKey(other)], 'running', 'the job marks the branch with its own revision');
  assertIdle(html, BRANCH_REVISION, 'unrelated branch row');
  assertRunning(rowFor(html, '8.00bpw'), 'matching branch row');
});

scenario('branch job with file selection does not mark the branch', async () => {
  // A whole-branch job selects no files. A job that carries a file selection
  // is not the branch download, whatever its branch flag says.
  const item = branchItem();
  reset({
    jobs: [branchJob({ status: 'running', filters: ['./config.json'] })],
    items: [item]
  });
  const { map, html } = await reconcile([item]);
  eq(map[BRANCH_KEY], undefined, 'a branch job that selects files must not mark the branch');
  assertIdle(html, BRANCH_REVISION, 'unselected branch row');
});

scenario('live branch job updates the branch row', async () => {
  const item = branchItem();
  reset({ items: [item] });
  const rows = installRows([item]);
  ui.handleWSMessage({ type: 'job_update', data: branchJob({ status: 'running' }) });
  await ui.flushLiveReconcile();
  eq(ui.getDlStatus(REPO, false)[BRANCH_KEY], 'running', 'the live branch job marks the branch row');
  eq(rows[0].dataset.dlStatus, 'running', 'the branch row shows running');
  match(rows[0].chip.outerHTML, /quant-dl-btn-running/, 'the branch row chip shows running');
  doesNotMatch(rows[0].chip.outerHTML, /quant-dl-btn-done/, 'the branch row is not done while its job runs');
});

scenario('completed branch job is not proof that the variant is present', async () => {
  const item = branchItem();
  reset({ jobs: [branchJob({ status: 'completed' })], items: [item] });
  const { map, html } = await reconcile([item]);
  eq(map[BRANCH_KEY], undefined, 'a completed branch job must not make the variant done');
  assertIdle(html, BRANCH_REVISION, 'absent branch variant');

  // A failing presence lookup cannot confirm the variant either.
  reset({
    jobs: [branchJob({ status: 'completed' })],
    branchError: new Error('branch presence lookup failed'),
    initial: { [BRANCH_KEY]: 'done' },
    items: [item]
  });
  const failed = await reconcile([item]);
  eq(failed.map[BRANCH_KEY], undefined, 'a failed branch presence lookup must clear a stored done');
  assertIdle(failed.html, BRANCH_REVISION, 'unverified branch variant');

  // A presence endpoint that answers with a server error is not a confirmation.
  reset({
    jobs: [branchJob({ status: 'completed' })],
    branchStatus: 500,
    initial: { [BRANCH_KEY]: 'done' },
    items: [item]
  });
  const errored = await reconcile([item]);
  eq(errored.map[BRANCH_KEY], undefined, 'a failed branch presence status must clear a stored done');
  assertIdle(errored.html, BRANCH_REVISION, 'failed branch presence variant');
});

scenario('branch and artifact statuses stay independent', async () => {
  const artifact = artifactItem(FILES, 'Q4_0');
  const branch = branchItem();
  reset({ cacheFiles: FILES, branchDownloaded: true, items: [artifact, branch] });
  const { map, html } = await reconcile([artifact, branch]);
  eq(ui.selectableStatusKey(artifact), ARTIFACT_KEY, 'the artifact keeps its own status key');
  eq(ui.selectableStatusKey(branch), BRANCH_KEY, 'the branch keeps its own status key');
  eq(map[ARTIFACT_KEY], 'done', 'the artifact is done from the repository file list');
  eq(map[BRANCH_KEY], 'done', 'the branch is done from the qualified lookup');
  assertDone(html, 'Q4_0', 'artifact row');
  assertBranchDone(html, BRANCH_REVISION, 'branch row');
});

scenario('branch download control posts a whole-branch request', async () => {
  const item = branchItem();
  reset({ items: [item] });
  const row = rowFor(ui.renderSelectableItems([item], 'selectableItems'), BRANCH_REVISION);
  match(row, /data-download="/, 'the branch row offers a download control');
  const payload = downloadPayload(row);
  eq(payload.branchVariant, true, 'the control marks the request as a branch variant');
  eq(payload.revision, BRANCH_REVISION, 'the control carries the branch revision');
  eq(payload.isDataset, false, 'the control is not a dataset download');
  eq(payload.localRepo, undefined, 'the control does not redirect to a local repository');

  await window.downloadQuantFromButton(downloadButton(payload));

  eq(downloadRequests.length, 1, 'the branch control posts one download request');
  const body = downloadRequests[0].body;
  eq(body.repo, REPO, 'the request names the analyzed repository');
  eq(body.revision, BRANCH_REVISION, 'the request names the branch revision');
  eq(body.branchVariant, true, 'the request selects a whole branch');
  eq(body.dataset, false, 'the request is not a dataset download');
  eq((body.filters || []).length, 0, 'the request carries no file selection');
  eq((body.excludes || []).length, 0, 'the request carries no excludes');
  eq(body.appendFilterSubdir || false, false, 'the request does not ask for a filter subdir');
  eq(body.localRepo || '', '', 'the request carries no localRepo redirection');
  eq(ui.getDlStatus(REPO, false)[BRANCH_KEY], 'queued', 'the branch row is queued after the request');
  // The Go test feeds this body to the server's own branch-variant admission.
  console.log('BRANCH_DOWNLOAD_REQUEST ' + JSON.stringify(body));
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
