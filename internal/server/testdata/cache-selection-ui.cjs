const assert = require('node:assert/strict');
const fs = require('node:fs');
const vm = require('node:vm');

const calls = [];
const nodes = new Map();
let modalHTML = '';
let currentSelection = null;
let delayNextSelection = null;
let delayNextDetails = null;
let detailsSource = 'Local';
let detailsFiles = [];
let deleteMode = 'success';
let deletedBodies = [];
let pendingDelete = null;
function fakeButton(dataset = {}) {
  return { dataset, disabled: false, listeners: {}, addEventListener(name, callback) { this.listeners[name] = callback; }, click() { return this.disabled ? undefined : this.listeners.click?.(); } };
}
function setSectionHTML(section, value) {
  section.html = value;
  modalHTML = modalHTML.replace(/<section id="cacheSelectionSection"[^>]*>[\s\S]*?<\/section>/, `<section id="cacheSelectionSection" class="cache-detail-section">${value}</section>`);
  const buttons = selector => {
    const attribute = selector === '.cache-selection-pick' ? 'data-group-index' : 'data-delete-group-index';
    return Array.from(value.matchAll(new RegExp(`<button[^>]*${attribute}="(\\d+)"[^>]*>`, 'g')), match => {
      const button = fakeButton({ [selector === '.cache-selection-pick' ? 'groupIndex' : 'deleteGroupIndex']: match[1] });
      button.disabled = /\sdisabled(?:\s|>)/.test(match[0]);
      return button;
    });
  };
  const buttonLists = new Map(['.cache-selection-pick', '.cache-selection-delete'].map(selector => [selector, buttons(selector)]));
  section.querySelectorAll = selector => buttonLists.get(selector) || [];
  if (value.includes('id="cacheSelectionLocation"')) {
    const select = { value: value.includes('Choose a currently available location') ? '-1' : '0', disabled: false, listeners: {}, addEventListener(name, callback) { this.listeners[name] = callback; } };
    nodes.set('#cacheSelectionLocation', select);
  } else nodes.delete('#cacheSelectionLocation');
}
const modalBody = {
  insertAdjacentHTML(position, html) {
    modalHTML = position === 'afterbegin' ? html + modalHTML : modalHTML + html;
    if (html.includes('id="cacheSelectionSection"') && !nodes.has('#cacheSelectionSection')) {
      const section = { set innerHTML(value) { setSectionHTML(this, value); }, insertAdjacentHTML(_p, value) { modalHTML += value; } };
      nodes.set('#cacheSelectionSection', section);
    }
    if (html.includes('id="cacheSelectionSection"')) {
      const inner = html.match(/<section id="cacheSelectionSection">([\s\S]*?)<\/section>/)?.[1];
      if (inner !== undefined) setSectionHTML(nodes.get('#cacheSelectionSection'), inner);
    }
    if (html.includes('id="cacheSelectionLocation"') && !nodes.has('#cacheSelectionSection')) setSectionHTML({ querySelectorAll() {} }, html);
    syncConfirmButtons();
  },
  set innerHTML(value) {
    modalHTML = value;
    nodes.delete('#cacheSelectionSection');
    nodes.delete('#cacheSelectionLocation');
    const sectionMatch = value.match(/<section id="cacheSelectionSection"[^>]*>([\s\S]*?)<\/section>/);
    if (sectionMatch) {
      const section = { set innerHTML(content) { setSectionHTML(this, content); }, insertAdjacentHTML(_p, content) { modalHTML += content; } };
      nodes.set('#cacheSelectionSection', section);
      setSectionHTML(section, sectionMatch[1]);
    }
    syncConfirmButtons();
  },
  get innerHTML() { return modalHTML; },
  querySelector() { return null; }
};
function syncConfirmButtons() {
  for (const match of modalHTML.matchAll(/id="(cache-delete-confirm-(\d+))"/g)) {
    nodes.set('#' + match[1], { id: match[1] });
  }
  for (const match of modalHTML.matchAll(/id="(cache-delete-(?:cancel|confirm-button)-(\d+))"/g)) {
    if (!nodes.has('#' + match[1])) nodes.set('#' + match[1], fakeButton());
  }
  if (modalHTML.includes('id="cache-delete-progress"')) nodes.set('#cache-delete-progress', { textContent: '' });
}
nodes.set('#modalBody', modalBody);
nodes.set('#modalTitle', { textContent: '' });
nodes.set('#modalBackdrop', { classList: { active: false, add() { this.active = true; }, remove() { this.active = false; }, contains(name) { return name === 'active' && this.active; } } });
nodes.set('#cacheList', { innerHTML: '' });
for (const id of ['#statModels', '#statDatasets', '#statSize', '#statFiles']) nodes.set(id, { textContent: '' });
const window = {};
window.location = { protocol: 'http:' };
const context = {
  window, console, URLSearchParams, encodeURIComponent,
  document: {
    readyState: 'loading', addEventListener() {}, querySelector: selector => nodes.get(selector) || null, querySelectorAll: () => [],
    createElement() { return { textContent: '', get innerHTML() { return String(this.textContent).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;'); } }; }
  },
  localStorage: { getItem() { return null; }, setItem() {} },
  fetch: async (path, options) => {
    const call = { path, method: options.method, body: options.body ? JSON.parse(options.body) : null };
    calls.push(call);
    let result;
    if (path === '/api/cache-selection' && options.method === 'DELETE') {
      deletedBodies.push(call.body);
      if (pendingDelete) await pendingDelete.promise;
      if (deleteMode === 'conflict') return { ok: false, json: async () => ({ ok: false, error: 'Selected GGUF is busy', error_detail: { message: 'Selected GGUF is busy' } }) };
      if (deleteMode.startsWith('refusal')) {
        const payload = { ok: false, error: 'HF deletion refused', error_detail: { message: 'HF deletion refused' } };
        if (deleteMode === 'refusal') payload.error_detail.details = 'retained friendly name depends on a selected snapshot entry <img src=x onerror=alert(1)>';
        if (deleteMode === 'refusal-legacy') payload.details = 'Legacy API detail';
        if (deleteMode === 'refusal-malformed') payload.error_detail.details = { reason: 'not plain text' };
        return { ok: false, status: 409, json: async () => payload };
      }
      if (deleteMode === 'partial') {
        call.status = 207;
        result = { ok: false, repo: call.body.repo, groupId: call.body.groupId, removed: [call.body.members[0].path], remaining: call.body.members.slice(1).map(member => member.path), errors: ['Fixture unlink failed'], linkOnlyEntries: call.body.members[0].linkOnly ? [call.body.members[0].path] : [], message: 'Fixture partial result' };
      } else {
        call.status = 200;
        const isHF = call.body.locationId === 'loc-2';
        result = {
          ok: true, repo: call.body.repo, groupId: call.body.groupId,
          removed: call.body.members.map(member => member.path), remaining: [],
          linkOnlyEntries: call.body.members.filter(member => member.linkOnly).map(member => member.path),
          ...(isHF ? { retainedPayloads: ['shared-blob-abc123'], message: 'Removed selected HF snapshot entries; shared payloads needed by other saved entries were retained' }
            : call.body.members.some(member => member.linkOnly) ? { message: 'Removed selected link-only entry; weight target remains' } : { message: 'Fixture removed selected group' })
        };
        currentSelection.locations = currentSelection.locations.map(location => location.id === call.body.locationId
          ? { ...location, groups: location.groups.filter(group => group.id !== call.body.groupId) }
          : location).filter(location => location.groups.length || location.warning);
      }
      return { ok: true, status: call.status, json: async () => result };
    } else if (path.startsWith('/api/cache-selection')) {
      if (delayNextSelection) {
        const delay = delayNextSelection;
        delayNextSelection = null;
        await delay.promise;
      }
      const locationID = new URL('http://fixture' + path).searchParams.get('locationId');
      result = locationID
        ? { ...currentSelection, locations: currentSelection.locations.filter(location => location.id === locationID) }
        : currentSelection;
    } else if (path.startsWith('/api/cache/')) {
      if (delayNextDetails) {
        const delay = delayNextDetails;
        delayNextDetails = null;
        await delay.promise;
      }
      const dataset = path.includes('type=dataset');
      result = { repo: dataset ? 'org/data' : 'org/model', type: dataset ? 'dataset' : 'model', owner: 'org', name: dataset ? 'data' : 'model', files: dataset ? [] : detailsFiles, path: dataset ? '/datasets/data' : '/friendly/model', sizeHuman: '1 GB', fileCount: 30, source: dataset ? 'HF cache' : detailsSource };
    } else if (path === '/api/cache') {
      result = { repos: [], stats: {} };
    } else {
      result = { repo: 'org/data', type: 'dataset', owner: 'org', name: 'data', files: [], path: '/datasets/data', sizeHuman: '1 GB', fileCount: 1, source: 'HF cache' };
    }
    return { ok: true, json: async () => result };
  }
};
let source = fs.readFileSync('../assets/static/js/app.js', 'utf8');
source = source.replace('  // Start\n', `window.test = { showCacheDetails: window.showCacheDetails };\n  // Start\n`);
vm.runInNewContext(source, context);

const paths = Array.from({ length: 25 }, (_, i) => `family/file-${i}.gguf`);
currentSelection = {
  repo: 'org/model', type: 'model', locations: [
    { id: 'loc-1', source: 'Local', path: '/scan/one/"archive"', canDelete: true, groups: [
      { id: 'grp-1', label: 'same-q-family-a', kind: 'llm', quant: 'Q4_K_M', canDelete: true, members: paths.map(path => ({ path, size: 4 })) },
      { id: 'grp-2', label: 'model.gguf', kind: 'llm', canDelete: true, members: [{ path: 'model.gguf', size: 0, linkOnly: true, message: 'Only this link; weights remain' }] },
      ...['family-b', 'family-c', 'family-d', 'family-e'].map((name, index) => ({
        id: `grp-${index + 3}`, label: `same Q4 family ${name}`, kind: 'llm', quant: 'Q4_K_M', canDelete: index !== 0, warning: index === 0 ? 'Could not verify this exact group' : '', members: [{ path: index === 0 ? 'bad/<img src=x onerror=alert(1)>.gguf' : `${name}/model.gguf`, size: 8 }]
      })),
      { id: 'grp-split', label: 'split Q5 family', kind: 'llm', quant: 'Q5_K_M', canDelete: true, members: [
        { path: 'parts/model-00001-of-00002.gguf', size: 9 },
        { path: 'parts/model-00002-of-00002.gguf', size: 9 }
      ] },
      { id: 'grp-mmproj-f16', label: 'mmproj-F16.gguf', kind: 'vision_encoder', canDelete: true, members: [{ path: 'mmproj-F16.gguf', size: 12 }] },
      { id: 'grp-mmproj-bf16', label: 'mmproj-BF16.gguf', kind: 'vision_encoder', canDelete: true, members: [{ path: 'mmproj-BF16.gguf', size: 12 }] },
      { id: 'grp-mmproj-f32', label: 'mmproj-F32.gguf', kind: 'vision_encoder', canDelete: true, members: [{ path: 'mmproj-F32.gguf', size: 12 }] }
    ] },
    { id: 'loc-2', source: 'HF cache', path: '/cache/hub/org--model', canDelete: true, groups: [
      { id: 'grp-hf', label: 'same-q-family-b', kind: 'llm', quant: 'Q4_K_M', canDelete: true, members: [
        { path: 'model-00001-of-00002.gguf', versions: ['commit-a'], size: 5 },
        { path: 'model-00001-of-00002.gguf', versions: ['commit-b'], size: 5 },
        { path: 'model-00002-of-00002.gguf', versions: ['commit-a'], size: 5 },
        { path: 'model-00002-of-00002.gguf', versions: ['commit-b'], size: 5 }
      ] },
      { id: 'grp-hf-mmproj', label: 'mmproj-F32.gguf', kind: 'vision_encoder', canDelete: true, members: [
        { path: 'mmproj-F32.gguf', versions: ['commit-a'], size: 6 }
      ] }
    ] }
  ]
};
const selectionFixture = JSON.parse(JSON.stringify(currentSelection));

(async () => {
  await window.test.showCacheDetails('org/model', 'model');
  assert(calls.some(call => call.path === '/api/cache/org%2Fmodel?type=model'));
  assert.equal((modalHTML.match(/class="cache-selection-group"/g) || []).length, 10);
  assert(modalHTML.includes('/scan/one') && modalHTML.includes('Could not verify this exact group'));
  assert(modalHTML.includes('title="/scan/one/&quot;archive&quot;"'), 'quoted path is safe in the selector title');
  assert(modalHTML.includes('model.gguf') && modalHTML.includes('Only this link; weights remain'));
  assert(modalHTML.includes('file-24.gguf'), 'all group files beyond 20 remain selectable/visible');
  assert(modalHTML.includes('&lt;img src=x onerror=alert(1)&gt;') && !modalHTML.includes('<img src=x'), 'server-provided filenames are escaped');
  assert.equal(nodes.get('#cacheSelectionSection').querySelectorAll('.cache-selection-delete').filter(button => button.disabled).length, 1, 'only the unverifiable local group is disabled');
  assert(modalHTML.includes('Only this group will be removed.'));
  assert(!modalHTML.includes('onclick="confirmDeleteCache'), 'model groups must not call whole-repository delete');
  assert(!calls.some(call => call.method === 'DELETE'));
  assert(!modalHTML.includes('cache-selection-pick'), 'Delete is direct; no redundant Select action');

  // Explicitly switch to the HF location and ensure it loads the opaque ID.
  currentSelection = { ...currentSelection, locations: [{ ...currentSelection.locations[1] }] };
  const select = nodes.get('#cacheSelectionLocation');
  select.value = '1';
  await select.listeners.change();
  assert(calls.some(call => call.path.includes('locationId=loc-2')));
  assert(modalHTML.includes('/scan/one'), 'location choices remain available after selecting one');
  assert(modalHTML.includes('commit-a') && modalHTML.includes('commit-b'));
  assert(modalHTML.includes('model-00002-of-00002.gguf'));
  assert.equal(nodes.get('#cacheSelectionSection').querySelectorAll('.cache-selection-delete').filter(button => button.disabled).length, 0, 'backend-advertised HF group action is enabled');
  assert.equal((modalHTML.match(/cache-badge-vision-encoder/g) || []).length, 1, 'the HF companion group renders its own badge');
  assert.equal((modalHTML.match(/class="cache-badge">Q4_K_M<\/span>/g) || []).length, 1, 'the HF llm group keeps its model-quant badge');

  // HF confirmation groups four stored entries into two distinct paths and
  // names both saved versions without turning versions into extra shards.
  let hfSequence = await startGroupConfirmation(0);
  assert(modalHTML.includes('all saved versions in this selected location'));
  assert(modalHTML.includes('Files included (2 distinct paths · 2 saved versions)'), modalHTML.slice(0, 2200));
  const hfConfirmation = modalHTML.slice(modalHTML.indexOf('<div id="cache-delete-confirm-'));
  assert.equal((hfConfirmation.match(/model-00001-of-00002\.gguf/g) || []).length, 1);
  assert.equal((hfConfirmation.match(/model-00002-of-00002\.gguf/g) || []).length, 1);
  assert(hfConfirmation.includes('Saved versions (2): commit-a, commit-b'));
  assert(!/<input[^>]*commit|<select[^>]*commit/i.test(hfConfirmation), 'HF confirmation does not offer revision input');
  assert(hfConfirmation.includes('GGUF group:'), 'an llm group is still confirmed as a GGUF group');
  assert(!hfConfirmation.includes('Vision encoder'), 'an llm versioned confirmation does not claim a vision-encoder role');
  assert(hfConfirmation.includes('This removes this GGUF group from all saved versions in this selected location. Other groups and other locations remain.'), 'the llm versioned scope statement is unchanged');
  const hfDeleteCount = deletedBodies.length;
  await cancelButton(hfSequence).click();
  assert.equal(deletedBodies.length, hfDeleteCount, 'HF confirmation Cancel sends no mutation');

  // Confirming sends the exact fresh path/version composition. Shared retained
  // payloads are reported without promising physical bytes were freed.
  await window.test.showCacheDetails('org/model', 'model', 'loc-2');
  hfSequence = await startGroupConfirmation(0);
  await confirmButton(hfSequence).click();
  const hfBody = deletedBodies.at(-1);
  assert.equal(hfBody.locationId, 'loc-2');
  assert.equal(hfBody.groupId, 'grp-hf');
  assert.equal(hfBody.members.length, 4);
  assert.deepEqual(hfBody.members.map(member => [member.path, member.versions[0]]), [
    ['model-00001-of-00002.gguf', 'commit-a'],
    ['model-00001-of-00002.gguf', 'commit-b'],
    ['model-00002-of-00002.gguf', 'commit-a'],
    ['model-00002-of-00002.gguf', 'commit-b']
  ]);
  assert(modalHTML.includes('shared payloads were retained'));
  assert(modalHTML.includes('Shared weight payloads retained because other saved entries still reference them'));
  assert(modalHTML.includes('shared-blob-abc123') && modalHTML.includes('No freed-space amount is implied'));

  // A companion in the HF cache names its role for the versioned case too.
  await window.test.showCacheDetails('org/model', 'model', 'loc-2');
  const versionedCompanionSequence = await startGroupConfirmation(0);
  const versionedCompanionConfirmation = modalHTML.slice(modalHTML.indexOf('<div id="cache-delete-confirm-'));
  assert(versionedCompanionConfirmation.includes('Vision encoder (mmproj companion):'), 'the versioned companion confirmation names the vision-encoder role');
  assert(versionedCompanionConfirmation.includes('This removes the selected vision encoder companion from all saved versions in this selected location. Other groups and other locations remain.'), 'the versioned companion scope statement names the role');
  assert(!versionedCompanionConfirmation.includes('This removes this GGUF group from all saved versions'), 'the versioned companion scope statement does not use the generic group wording');
  assert(versionedCompanionConfirmation.includes('Saved versions (1): commit-a'), 'the versioned companion still lists its saved versions');
  const beforeVersionedCompanion = deletedBodies.length;
  await cancelButton(versionedCompanionSequence).click();
  assert.equal(deletedBodies.length, beforeVersionedCompanion, 'cancelling the versioned companion confirmation sends no mutation');

  // Dataset details retain explicit typing and never request the GGUF endpoint.
  calls.length = 0;
  await window.test.showCacheDetails('org/data', 'dataset');
  assert(calls.some(call => call.path === '/api/cache/org%2Fdata?type=dataset'));
  assert(!calls.some(call => call.path.startsWith('/api/cache-selection')));
  assert(modalHTML.includes('Delete entire dataset cache'));
  assert(!calls.some(call => call.method === 'DELETE'));

  // Existing whole-repository deletion remains available for an HF model
  // without any GGUF groups, and its scope is explicit (never a group action).
  calls.length = 0;
  detailsSource = 'HF cache';
  detailsFiles = [{ name: 'weights.safetensors', sizeHuman: '1 GB' }, ...Array.from({ length: 24 }, (_, i) => ({ name: `metadata-${i}.json`, sizeHuman: '1 KB' }))];
  currentSelection = { repo: 'org/non-gguf', type: 'model', locations: [] };
  await window.test.showCacheDetails('org/non-gguf', 'model');
  assert(modalHTML.includes('weights.safetensors'));
  assert(modalHTML.includes('metadata-23.json'), 'non-GGUF fallback retains all snapshot files beyond 20');
  assert(modalHTML.includes('Delete entire model cache'));
  assert(!modalHTML.includes('Delete this GGUF group'));
  assert(!calls.some(call => call.method === 'DELETE'), 'displaying the legacy action must not execute it');

  async function openLocalDetails() {
    detailsSource = 'Local';
    detailsFiles = [];
    currentSelection = JSON.parse(JSON.stringify(selectionFixture));
    await window.test.showCacheDetails('org/model', 'model');
  }
  function deleteControl(index = 0) {
    return nodes.get('#cacheSelectionSection').querySelectorAll('.cache-selection-delete')[index];
  }
  // The rendered group kind decides the dialog title: a companion group is a
  // vision encoder, every other group is a GGUF group.
  function renderedGroupKind(index = 0) {
    const articles = modalHTML.match(/<article class="cache-selection-group">[\s\S]*?<\/article>/g) || [];
    const article = articles[index];
    assert(article, `rendered group ${index} is present in the current location`);
    return article.includes('cache-badge-vision-encoder') ? 'vision_encoder' : 'llm';
  }
  function expectedConfirmationTitle(kind) {
    return kind === 'vision_encoder' ? 'Confirm vision encoder deletion' : 'Confirm GGUF group deletion';
  }
  async function startGroupConfirmation(index = 0) {
    const control = deleteControl(index);
    assert(control && !control.disabled, 'eligible local group has an enabled delete action');
    const kind = renderedGroupKind(index);
    await control.click();
    assert.equal(nodes.get('#modalTitle').textContent, expectedConfirmationTitle(kind));
    return Number(modalHTML.match(/id="cache-delete-confirm-(\d+)"/)[1]);
  }
  function confirmButton(sequence) { return nodes.get(`#cache-delete-confirm-button-${sequence}`); }
  function cancelButton(sequence) { return nodes.get(`#cache-delete-cancel-${sequence}`); }

  // Confirmation is exact and complete (>20 members), and cancel performs no write.
  calls.length = 0;
  deletedBodies = [];
  await openLocalDetails();
  let sequence = await startGroupConfirmation(0);
  assert(modalHTML.includes('Repository:') && modalHTML.includes('/scan/one') && modalHTML.includes('same-q-family-a'));
  assert(modalHTML.includes('family/file-24.gguf') && modalHTML.includes('Files included (25 distinct paths)'));
  assert(modalHTML.includes('Other variants, companion files, metadata, directories, and other locations remain'));
  assert.equal(deletedBodies.length, 0);
  await cancelButton(sequence).click();
  assert.equal(deletedBodies.length, 0, 'Cancel does not send DELETE');
  assert.equal(nodes.get('#modalTitle').textContent, 'Repository Details');
  assert(modalHTML.includes('/scan/one'), 'Cancel restores the chosen location');

  // Link-only warning is explicit in confirmation and cancel remains read-only.
  sequence = await startGroupConfirmation(1);
  assert(modalHTML.includes('this removes the link; the weight file remains'));
  await cancelButton(sequence).click();
  assert.equal(deletedBodies.length, 0);

  // A recognized split confirms every shard together, not a matching quant label.
  sequence = await startGroupConfirmation(6);
  assert(modalHTML.includes('parts/model-00001-of-00002.gguf'));
  assert(modalHTML.includes('parts/model-00002-of-00002.gguf'));
  assert(modalHTML.includes('Files included (2 distinct paths)'));
  await cancelButton(sequence).click();
  assert.equal(deletedBodies.length, 0);

  // A vision-encoder companion is a separate artifact role: it renders its own
  // badge instead of a model-quant badge, keeps an enabled Delete action, and
  // sends only its own companion member composition.
  await openLocalDetails();
  const companionIndex = selectionFixture.locations[0].groups.findIndex(group => group.id === 'grp-mmproj-f32');
  assert(companionIndex >= 0, 'the companion group is present in the selection fixture');
  assert.equal((modalHTML.match(/cache-badge-vision-encoder/g) || []).length, 3, 'every companion group renders its own badge');
  assert.equal((modalHTML.match(/class="cache-badge">Q4_K_M<\/span>/g) || []).length, 5, 'every llm quantization group keeps its own model-quant badge');
  assert.equal((modalHTML.match(/class="cache-badge">Q5_K_M<\/span>/g) || []).length, 1, 'the llm split group keeps its own model-quant badge');
  assert(modalHTML.includes('Vision encoder'), 'the companion badge names the artifact role');
  assert(!modalHTML.includes('>F32<'), 'a companion never renders a model-quant badge');
  assert(modalHTML.includes('mmproj-F32.gguf'), 'the companion keeps its own group label');
  const companionSequence = await startGroupConfirmation(companionIndex);
  assert(modalHTML.includes('Files included (1 distinct paths)'), 'only the companion member is confirmed');
  const companionConfirmation = modalHTML.slice(modalHTML.indexOf('<div id="cache-delete-confirm-'));
  assert(companionConfirmation.includes('Vision encoder (mmproj companion):'), 'the companion confirmation names the vision-encoder role');
  assert(companionConfirmation.includes('mmproj-F32.gguf'), 'the companion confirmation names the selected companion');
  assert(companionConfirmation.includes('Delete selected vision encoder files'), 'the companion confirm button names the vision encoder');
  assert(!companionConfirmation.includes('Delete selected GGUF files'), 'the generic gguf confirm label is not used for a companion');
  assert(!companionConfirmation.includes('Other variants, companion files, metadata, directories, and other locations remain.'), 'the companion confirmation does not claim companion files remain');
  assert(companionConfirmation.includes('This removes only the selected vision encoder companion files.'), 'the companion scope statement is limited to the selected companion');
  assert(companionConfirmation.includes('Other quantization groups, other companion files, metadata, directories, and other locations remain.'), 'the companion scope statement keeps the remaining scope explicit');
  const beforeCompanion = deletedBodies.length;
  let resolveCompanionDelete;
  pendingDelete = { promise: new Promise(resolve => { resolveCompanionDelete = resolve; }) };
  const pendingCompanion = confirmButton(companionSequence).click();
  assert.equal(nodes.get('#cache-delete-progress').textContent, 'Deleting the selected vision encoder…', 'the companion progress line names the vision encoder');
  resolveCompanionDelete();
  pendingDelete = null;
  await pendingCompanion;
  assert.equal(deletedBodies.length, beforeCompanion + 1, 'the guarded companion confirmation still sends the DELETE');
  const companionBody = deletedBodies.at(-1);
  assert.equal(companionBody.groupId, 'grp-mmproj-f32');
  assert.deepEqual(companionBody.members.map(member => member.path), ['mmproj-F32.gguf']);
  assert.equal(companionBody.members[0].versions.length, 0);
  assert(!companionBody.members.some(member => member.path !== 'mmproj-F32.gguf'), 'only the companion member is sent');
  assert(modalHTML.includes('Selected vision encoder removed.'), 'the companion success outcome names the vision encoder');
  assert(!modalHTML.includes('Selected GGUF group removed.'), 'the generic gguf outcome heading is not used for a companion');

  // A cancelled companion confirmation stays read-only: the guarded cancel
  // path must not be rejected by the role-dependent title.
  await openLocalDetails();
  const cancelledCompanionSequence = await startGroupConfirmation(companionIndex);
  const beforeCancelledCompanion = deletedBodies.length;
  await cancelButton(cancelledCompanionSequence).click();
  assert.equal(deletedBodies.length, beforeCancelledCompanion, 'a cancelled companion confirmation sends no DELETE');
  assert.equal(nodes.get('#modalTitle').textContent, 'Repository Details', 'a cancelled companion confirmation restores the details view');

  // An LLM group keeps its current confirmation wording.
  await openLocalDetails();
  const llmSequence = await startGroupConfirmation(0);
  const llmConfirmation = modalHTML.slice(modalHTML.indexOf('<div id="cache-delete-confirm-'));
  assert(llmConfirmation.includes('GGUF group:'), 'an llm group is still confirmed as a GGUF group');
  assert(!llmConfirmation.includes('Vision encoder'), 'an llm confirmation does not claim a vision-encoder role');
  assert(llmConfirmation.includes('Delete selected GGUF files'), 'an llm group keeps its confirm button label');
  assert(!llmConfirmation.includes('Delete selected vision encoder files'), 'the companion confirm label is not used for an llm group');
  assert(llmConfirmation.includes('This removes only the selected GGUF group members. Other variants, companion files, metadata, directories, and other locations remain.'), 'the llm scope statement is unchanged');
  const beforeLLM = deletedBodies.length;
  let resolveLLMDelete;
  pendingDelete = { promise: new Promise(resolve => { resolveLLMDelete = resolve; }) };
  const pendingLLM = confirmButton(llmSequence).click();
  assert.equal(nodes.get('#cache-delete-progress').textContent, 'Deleting the selected GGUF group…', 'the llm progress line is unchanged');
  resolveLLMDelete();
  pendingDelete = null;
  await pendingLLM;
  assert.equal(deletedBodies.length, beforeLLM + 1, 'the guarded llm confirmation still sends the DELETE');
  assert(modalHTML.includes('Selected GGUF group removed.'), 'the llm success outcome heading is unchanged');
  assert(!modalHTML.includes('Selected vision encoder removed.'), 'the companion outcome heading is not used for an llm group');

  // Link-only local member metadata survives confirmation into the exact request.
  await openLocalDetails();
  sequence = await startGroupConfirmation(1);
  await confirmButton(sequence).click();
  const linkOnlyBody = deletedBodies.at(-1).members[0];
  assert.equal(linkOnlyBody.path, 'model.gguf');
  assert.equal(linkOnlyBody.linkOnly, true);
  assert.equal(linkOnlyBody.message, 'Only this link; weights remain');
  assert(modalHTML.includes('link-only entry; weight target remains') && modalHTML.includes('Links removed; weight targets remain'));

  // Successful DELETE sends only the fresh group selection, then refreshes both
  // cache list and exact location while retaining other groups/locations.
  await openLocalDetails();
  const beforeLocalSuccess = deletedBodies.length;
  sequence = await startGroupConfirmation(0);
  await confirmButton(sequence).click();
  assert.equal(deletedBodies.length, beforeLocalSuccess + 1);
  const localBody = deletedBodies.at(-1);
  assert.deepEqual(Object.keys(localBody).sort(), ['groupId', 'locationId', 'members', 'repo', 'type']);
  assert.equal(localBody.repo, 'org/model');
  assert.equal(localBody.type, 'model');
  assert.equal(localBody.locationId, 'loc-1');
  assert.equal(localBody.groupId, 'grp-1');
  assert.equal(localBody.members.length, 25);
  assert(localBody.members.some(member => member.path === 'family/file-24.gguf'));
  assert(calls.some(call => call.path === '/api/cache-selection' && call.method === 'DELETE'));
  assert(!calls.some(call => call.method === 'DELETE' && call.path !== '/api/cache-selection'), 'group action never falls back to whole-repository DELETE');
  assert(calls.some(call => call.path === '/api/cache'), 'Cache cards refresh after operation');
  assert(modalHTML.includes('Selected GGUF group removed'));
  assert(!modalHTML.includes('<strong>same-q-family-a</strong>'), 'successful group is not left presented as present');
  assert(modalHTML.includes('same Q4 family family-b') && modalHTML.includes('/cache/hub/org--model'), 'other groups and locations remain visible');

  // HTTP 207 is successful at the transport level but must render a partial
  // result from the JSON ok:false payload without hiding remaining members.
  await openLocalDetails();
  deleteMode = 'partial';
  sequence = await startGroupConfirmation(0);
  await confirmButton(sequence).click();
  assert(modalHTML.includes('Partial result while removing the selected GGUF group'));
  assert(calls.some(call => call.method === 'DELETE' && call.status === 207), 'fixture models HTTP 207 while response.ok remains true');
  assert(modalHTML.includes('Removed:') && modalHTML.includes('Remaining:') && modalHTML.includes('Fixture unlink failed'));
  assert(modalHTML.includes('family/file-24.gguf'), 'remaining long member list is visible after partial result');
  deleteMode = 'success';

  // Busy/stale conflict is targeted, refreshed, and never retried automatically.
  await openLocalDetails();
  deleteMode = 'conflict';
  const beforeConflict = deletedBodies.length;
  sequence = await startGroupConfirmation(0);
  await confirmButton(sequence).click();
  assert.equal(deletedBodies.length, beforeConflict + 1, 'conflict is not automatically retried');
  assert(modalHTML.includes('was not confirmed as removed') && modalHTML.includes('Selected GGUF is busy'));
  assert(modalHTML.includes('/scan/one'), 'conflict keeps the selected location in context');
  deleteMode = 'success';

  // A refusal includes the actionable backend reason, rendered as escaped text,
  // while retaining the no-fallback and refresh behavior.
  await openLocalDetails();
  deleteMode = 'refusal';
  const beforeRefusal = deletedBodies.length;
  sequence = await startGroupConfirmation(0);
  await confirmButton(sequence).click();
  assert.equal(deletedBodies.length, beforeRefusal + 1, 'refusal is not automatically retried');
  assert(modalHTML.includes('HF deletion refused'), 'the refusal title remains visible');
  assert(!modalHTML.includes('HF deletion was partial'), 'a refusal is not presented as a partial deletion');
  assert(modalHTML.includes('retained friendly name depends on a selected snapshot entry'));
  assert(modalHTML.includes('&lt;img src=x onerror=alert(1)&gt;') && !modalHTML.includes('<img src=x'), 'refusal details are escaped');
  assert(modalHTML.includes('no fallback deletion was attempted'));
  assert(calls.some(call => call.method === 'DELETE' && call.path === '/api/cache-selection'));
  assert(!calls.some(call => call.method === 'DELETE' && call.path !== '/api/cache-selection'));
  deleteMode = 'success';

  // Legacy top-level details remain readable; absent or malformed detail
  // payloads fall back to the refusal title instead of exposing objects.
  for (const [mode, expected, unexpected] of [
    ['refusal-legacy', 'Legacy API detail', 'retained friendly name'],
    ['refusal-malformed', 'HF deletion refused', '[object Object]'],
    ['refusal-no-details', 'HF deletion refused', '[object Object]']
  ]) {
    await openLocalDetails();
    deleteMode = mode;
    sequence = await startGroupConfirmation(0);
    await confirmButton(sequence).click();
    assert(modalHTML.includes(expected), `${mode} shows its safe fallback/detail`);
    assert(!modalHTML.includes(unexpected), `${mode} does not expose malformed or absent detail`);
  }
  deleteMode = 'success';

  // If deletion removes the last group from one location, another repository
  // location remains listed but is not silently selected as the new target.
  await openLocalDetails();
  currentSelection.locations[0].groups = [JSON.parse(JSON.stringify(currentSelection.locations[0].groups.find(group => group.id === 'grp-2')))];
  await window.test.showCacheDetails('org/model', 'model');
  sequence = await startGroupConfirmation(0);
  assert(modalHTML.includes('weight file remains'));
  await confirmButton(sequence).click();
  assert(modalHTML.includes('Selected GGUF group removed'));
  assert(modalHTML.includes('/scan/one'), 'result retains the location from which the link was removed');
  assert(modalHTML.includes('No other location was selected automatically'));
  assert.equal(nodes.get('#cacheSelectionLocation').value, '-1');
  assert(modalHTML.includes('/cache/hub/org--model'), 'the other location remains available but unselected');

  // Replacing Details during a pending write cannot let its old response
  // overwrite the new repository modal.
  await openLocalDetails();
  sequence = await startGroupConfirmation(0);
  let resolveDelete;
  pendingDelete = { promise: new Promise(resolve => { resolveDelete = resolve; }) };
  const pending = confirmButton(sequence).click();
  assert(confirmButton(sequence).disabled && cancelButton(sequence).disabled);
  await window.test.showCacheDetails('org/other', 'model');
  const replacementModal = modalHTML;
  resolveDelete();
  pendingDelete = null;
  await pending;
  assert.equal(nodes.get('#modalTitle').textContent, 'Repository Details');
  assert.equal(modalHTML, replacementModal, 'stale delete response does not replace different Details');

  // A completion from an older Details context preserves a newer in-flight
  // location choice for the same repository rather than resetting its root.
  await openLocalDetails();
  sequence = await startGroupConfirmation(0);
  let resolveSameRepoDelete;
  pendingDelete = { promise: new Promise(resolve => { resolveSameRepoDelete = resolve; }) };
  const oldContextDelete = confirmButton(sequence).click();
  await window.test.showCacheDetails('org/model', 'model');
  let resolveLocationRead;
  delayNextSelection = { promise: new Promise(resolve => { resolveLocationRead = resolve; }) };
  const locationSelect = nodes.get('#cacheSelectionLocation');
  locationSelect.value = '1';
  const locationChange = locationSelect.listeners.change();
  resolveSameRepoDelete();
  pendingDelete = null;
  resolveLocationRead();
  await Promise.all([oldContextDelete, locationChange]);
  assert(modalHTML.includes('/cache/hub/org--model'), modalHTML.slice(0, 1800));
  assert(modalHTML.includes('model-00001-of-00002.gguf'), 'the newer chosen location remains current after the old mutation finishes');

  // A late response from a replaced modal cannot overwrite its content.
  detailsSource = 'Local';
  let finishDetails;
  delayNextDetails = { promise: new Promise(resolve => { finishDetails = resolve; }) };
  const pendingDetails = window.test.showCacheDetails('org/model', 'model');
  nodes.get('#modalTitle').textContent = 'Another modal';
  finishDetails();
  await pendingDetails;
  assert.equal(modalHTML, '<div class="loading-state"><div class="spinner"></div></div>');
  if (process.env.CACHE_UI_FIXTURE_URL) await checkBrowserLayout();
  console.log('PASS: typed details, exact local/HF group confirmations and request composition, all saved versions, shared-payload/207/conflict results, cancel, exact refresh/context, legacy actions, full member lists, escaping');
})().catch(error => { console.error(error); process.exitCode = 1; });

// Optional real-DOM/layout layer against an isolated read-only fixture serving
// production index.html/app.js/style.css. No browser dependency is added to the app.
async function checkBrowserLayout() {
  const { chromium } = require('playwright');
  const browser = await chromium.launch({ headless: true, executablePath: process.env.CACHE_UI_BROWSER || undefined, args: ['--no-sandbox'] });
  const base = process.env.CACHE_UI_FIXTURE_URL;
  try {
    for (const width of [1000, 360]) {
      const page = await browser.newPage({ viewport: { width, height: 900 } });
      const errors = [];
      page.on('pageerror', error => errors.push(error.message));
      page.on('request', request => assert.equal(request.method(), 'GET', 'visual checks never send destructive requests'));
      await page.goto(base);
      await page.getByText('Show 25 file records', { exact: true }).waitFor();
      await page.evaluate(() => Promise.all(document.getAnimations().filter(animation => animation.effect?.target?.closest?.('#modal')).map(animation => animation.finished)));
      const structure = await page.evaluate(() => {
        const modal = document.querySelector('.cache-detail-modal');
        const scroll = modal.querySelector('.cache-detail-scroll');
        const section = document.querySelector('#cacheSelectionSection');
        return {
          children: Array.from(modal.children, node => node.className || node.id),
          scrollCount: modal.querySelectorAll('.cache-detail-scroll').length,
          groupsInside: scroll.contains(modal.querySelector('.cache-selection-groups')),
          othersInside: scroll.contains(modal.querySelector('.cache-selection-other')),
          metadataInside: scroll.contains(modal.querySelector('.cache-detail-metadata')),
          headingOutside: !scroll.contains(section.querySelector('h4')),
          collapsed: Array.from(modal.querySelectorAll('details')).every(node => !node.open),
          stats: modal.querySelector('.cache-detail-stats').children.length,
          pathTitle: document.querySelector('#cacheSelectionLocation').title,
          documentOverflow: document.documentElement.scrollWidth > innerWidth,
          modalWidth: document.querySelector('#modal').getBoundingClientRect().width
        };
      });
      assert.deepEqual(structure.children, ['cache-detail-header', 'cache-detail-stats', 'cache-detail-section', 'cache-detail-actions']);
      assert.equal(structure.scrollCount, 1);
      for (const key of ['groupsInside', 'othersInside', 'metadataInside', 'headingOutside', 'collapsed']) assert(structure[key], key);
      assert.equal(structure.stats, 3);
      assert(structure.pathTitle.includes('very-long-library-location'));
      assert(!structure.documentOverflow);
      assert(structure.modalWidth <= (width === 360 ? 360 : 600));
      if (width === 1000) assert.equal(structure.modalWidth, 600);
      const first = page.locator('.cache-selection-group').first();
      const title = await first.locator('strong').boundingBox();
      const button = await first.getByRole('button', { name: 'Delete', exact: true }).boundingBox();
      assert(title.x + title.width <= button.x, 'long name stays in its reserved left column');
      const stationary = () => page.evaluate(() => ['.modal-header', '.cache-detail-header', '.cache-detail-stats', '.cache-selection-sticky', '.cache-detail-actions'].map(selector => document.querySelector(selector).getBoundingClientRect().y));
      const before = await stationary();
      await page.screenshot({ path: `/tmp/opencode/repository-details-${width}-collapsed.png` });
      const summary = first.locator('summary');
      await summary.focus();
      await page.keyboard.press('Enter');
      assert.equal(await first.locator('details[open] li').count(), 25);
      await page.locator('.cache-detail-scroll').evaluate(node => { node.scrollTop = 450; });
      assert.deepEqual(await stationary(), before, 'chrome, identity, stats, location, and footer stay still');
      const scrolling = await page.evaluate(() => Array.from(document.querySelectorAll('#modal *')).filter(node => ['auto', 'scroll'].includes(getComputedStyle(node).overflowY) && node.scrollHeight > node.clientHeight).map(node => node.className));
      assert.deepEqual(scrolling, ['cache-detail-scroll'], 'no nested/outer scroll owners');
      await page.screenshot({ path: `/tmp/opencode/repository-details-${width}-expanded.png` });
      await page.getByText('Other files (2)', { exact: true }).click();
      assert.equal(await page.locator('.cache-selection-other[open] li').count(), 2);
      assert.deepEqual(await stationary(), before, 'expanding Other files does not move the fixed summary or footer');
      await page.getByLabel('Storage location', { exact: true }).selectOption('1');
      await page.getByText('2 files · 2 saved versions', { exact: true }).waitFor();
      assert((await page.getByLabel('Storage location', { exact: true }).getAttribute('title')).includes('/cache/hub/'));
      // The confirmation title names the artifact role, so read the rendered
      // group's badge and wait for the matching title.
      const firstIsVisionEncoder = await first.locator('.cache-badge-vision-encoder').count() > 0;
      await first.getByRole('button', { name: 'Delete', exact: true }).click();
      await page.getByRole('heading', { name: firstIsVisionEncoder ? 'Confirm vision encoder deletion' : 'Confirm GGUF group deletion', exact: true }).waitFor();
      assert.equal(await page.locator('.cache-detail-modal').count(), 0, 'confirmation does not inherit Details layout');
      assert.equal(await page.locator('#modalBody').evaluate(node => getComputedStyle(node).overflowY), 'auto');
      await page.getByRole('button', { name: 'Cancel', exact: true }).click();
      await page.getByRole('heading', { name: 'Repository Details', exact: true }).waitFor();
      for (const variant of ['non-gguf', 'dataset']) {
        await page.goto(`${base}?case=${variant}`);
        await page.getByText('weights.safetensors', { exact: true }).waitFor();
        await page.getByRole('button', { name: `Delete entire ${variant === 'dataset' ? 'dataset' : 'model'} cache`, exact: true }).waitFor();
        assert.equal(await page.locator('.cache-selection-group').count(), 0);
        assert.equal(await page.locator('.cache-detail-scroll').count(), 1);
        assert(await page.locator('.cache-detail-scroll .cache-detail-metadata').count());
        await page.getByText('Repository metadata', { exact: true }).click();
        assert(await page.getByText('mmproj-F16.gguf', { exact: true }).isVisible(), 'vision metadata remains reachable');
        assert(await page.getByText('d1a7b9c'.repeat(5), { exact: true }).isVisible(), 'commit is retained in supplementary metadata');
        await page.getByText('Repository metadata', { exact: true }).click();
        await page.screenshot({ path: `/tmp/opencode/repository-details-${width}-${variant}.png` });
      }
      await page.route('**/api/cache-selection*', route => route.fulfill({ status: 503, contentType: 'application/json', body: JSON.stringify({ error: 'Fixture unavailable' }) }));
      await page.goto(base);
      await page.getByText('Unable to load GGUF selection:', { exact: false }).waitFor();
      assert.equal(await page.locator('.cache-detail-scroll .cache-detail-files').count(), 1, 'failed selection preserves the snapshot file fallback');
      assert.equal(await page.locator('.cache-detail-scroll .cache-detail-metadata').count(), 1);
      await page.getByRole('button', { name: 'Delete entire model cache', exact: true }).waitFor();
      await page.screenshot({ path: `/tmp/opencode/repository-details-${width}-selection-error.png` });
      await page.unroute('**/api/cache-selection*');
      let releaseSelection;
      const pendingSelection = new Promise(resolve => { releaseSelection = resolve; });
      await page.route('**/api/cache-selection*', async route => { await pendingSelection; await route.continue(); });
      await page.goto(base);
      await page.getByText('Loading GGUF groups…', { exact: true }).waitFor();
      assert.equal(await page.locator('.cache-detail-scroll').count(), 1);
      await page.getByRole('button', { name: 'Close', exact: true }).waitFor();
      await page.screenshot({ path: `/tmp/opencode/repository-details-${width}-loading.png` });
      releaseSelection();
      await page.getByText('Show 25 file records', { exact: true }).waitFor();
      await page.keyboard.press('Escape');
      assert.equal(await page.locator('#modalBackdrop.active').count(), 0, 'Escape still closes Details');
      assert.deepEqual(errors, [], 'no page runtime errors');
      await page.close();
    }
    console.log('PASS: real DOM ancestry/order; 600px modal and 360px viewport; long-name reserved action column; 25 native-details members; sole central scroller with stationary chrome/footer; location switch; dataset/no-GGUF fallback; no writes');
  } finally { await browser.close(); }
}
