// Run through TestSelectableArtifactStatusUI: real app.js status reconciliation.
//
// The harness loads the shipped app.js inside Node's VM with DOM, localStorage
// and cache-fetch stubs. It drives the production sequence in
// loadRepositoryDetails/renderAnalysisResult: refreshDlStatusFromJobs runs
// before syncLocalAnalysisStatus, then renderSelectableItems reads the result.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const REPO = 'owner/model';
const SHARD1 = 'model-Q4_0-00001-of-00002.gguf';
const SHARD2 = 'model-Q4_0-00002-of-00002.gguf';
const FILES = [SHARD1, SHARD2];
// downloadQuant sends root-level artifact files through artifactRequestPaths,
// which prefixes bare names with "./".
const JOB_FILTERS = FILES.map(name => './' + name);
const ARTIFACT_KEY = `artifact:${JSON.stringify(FILES.map(name => name.toLowerCase()))}`;

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

const document = {
  readyState: 'loading',
  body: makeNode(),
  handlers: {},
  addEventListener(type, fn) { (this.handlers[type] ||= []).push(fn); },
  createElement() { return makeNode(); },
  querySelector() { return null; },
  querySelectorAll() { return []; },
  getElementById() { return null; }
};

const storage = new Map();
const localStorage = {
  getItem(key) { return storage.has(key) ? storage.get(key) : null; },
  setItem(key, value) { storage.set(key, String(value)); },
  removeItem(key) { storage.delete(key); }
};

let cacheState = { status: 200, files: [], error: null, parseError: null };
const fetch = async (rawPath) => {
  if (!String(rawPath).startsWith('/api/cache/')) {
    throw new Error('unexpected fetch: ' + rawPath);
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
    getDlStatus,
    selectableStatusKey,
    artifactJobStatusKey,
    setCurrentAnalysis(value) { currentAnalysis = value; }
  };
`;
source = source.replace(marker, hook + '\n' + marker);
vm.runInNewContext(source, context);
const ui = window.test;
ui.state.settings = {};

function reset({ jobs = [], cacheFiles = [], cacheStatus = 200, cacheError = null, cacheParseError = null, initial = {} } = {}) {
  ui.state.jobs.clear();
  jobs.forEach((job, index) => ui.state.jobs.set(job.id || `job-${index}`, job));
  ui.state.settings = {};
  ui.setCurrentAnalysis({ repo: REPO, is_dataset: false });
  storage.clear();
  if (Object.keys(initial).length) {
    storage.set('dlStatus:' + REPO, JSON.stringify(initial));
  }
  cacheState = { status: cacheStatus, files: cacheFiles, error: cacheError, parseError: cacheParseError };
}

async function reconcile(items) {
  const data = { repo: REPO, selectable_items: items };
  ui.refreshDlStatusFromJobs(REPO);
  await ui.syncLocalAnalysisStatus(data);
  ui.refreshDlStatusFromJobs(REPO);
  const html = ui.renderSelectableItems(items, 'selectableItems');
  return { map: ui.getDlStatus(REPO), html };
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
