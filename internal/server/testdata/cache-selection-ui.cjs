const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

const calls = [];
const nodes = new Map();
let modalHTML = '';
let currentSelection = null;
let delayNextSelection = null;
let delayNextDetails = null;
let detailsSource = 'Local';
let detailsPath = '/friendly/model';
let detailsFriendlyPath = '';
let detailsManifest = null;
let detailsDownloadStatus = 'unknown';
let detailsWholeFolder = null;
let detailsWholeFolderReason = '';
let datasetSource = 'HF cache';
let detailsFiles = [];
let detailsCommit = 'abcdef1234567890';
let detailsSizeHuman = '1 GB';
let detailsFileCount = 30;
let deleteMode = 'success';
let deletedBodies = [];
let pendingDelete = null;
function fakeButton(dataset = {}) {
  return { dataset, disabled: false, focused: false, listeners: {}, addEventListener(name, callback) { this.listeners[name] = callback; }, focus() { this.focused = true; }, click() { return this.disabled ? undefined : this.listeners.click?.(); } };
}
function decodeAttributeValue(value) {
  return String(value).replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&amp;/g, '&');
}
function textContent(html) {
  return decodeAttributeValue(String(html).replace(/<[^>]*>/g, '')).replace(/\s+/g, ' ').trim();
}
function registerSelectionLocation(html) {
  const selectTag = html.match(/<select id="cacheSelectionLocation"[^>]*>/)?.[0];
  if (!selectTag) { nodes.delete('#cacheSelectionLocation'); return; }
  const optionValues = Array.from(html.matchAll(/<option value="(-?\d+)"([^>]*)>/g));
  const chosen = optionValues.find(match => match[2].includes('selected'));
  const select = {
    value: chosen ? chosen[1] : (optionValues[0]?.[1] ?? '0'),
    disabled: /\sdisabled(?:\s|>)/.test(selectTag),
    focused: false,
    title: decodeAttributeValue((selectTag.match(/title="([^"]*)"/) || [])[1] || ''),
    ariaLabel: decodeAttributeValue((selectTag.match(/aria-label="([^"]*)"/) || [])[1] || ''),
    listeners: {},
    addEventListener(name, callback) { this.listeners[name] = callback; },
    focus() { this.focused = true; }
  };
  nodes.set('#cacheSelectionLocation', select);
}
function setSectionHTML(section, value) {
  section.html = value;
  const opening = modalHTML.match(/<section id="cacheSelectionSection"[^>]*>/)?.[0];
  modalHTML = modalHTML.replace(/<section id="cacheSelectionSection"[^>]*>[\s\S]*?<\/section>/, `${opening || '<section id="cacheSelectionSection" class="cache-detail-section" role="status" aria-live="polite" aria-busy="false">'}${value}</section>`);
  registerSelectionLocation(value);
  const buttons = selector => {
    const attribute = selector === '.cache-selection-pick' ? 'data-group-index' : 'data-delete-group-index';
    return Array.from(value.matchAll(new RegExp(`<button[^>]*${attribute}="(\\d+)"[^>]*>`, 'g')), match => {
      const button = fakeButton({ [selector === '.cache-selection-pick' ? 'groupIndex' : 'deleteGroupIndex']: match[1] });
      button.disabled = /\sdisabled(?:\s|>)/.test(match[0]);
      return button;
    });
  };
  const deleteButtons = buttons('.cache-selection-delete');
  const pickButtons = buttons('.cache-selection-pick');
  const rows = Array.from(value.matchAll(/<article class="cache-selection-group"[^>]*data-group-id="([^"]*)"[\s\S]*?<\/article>/g), (match, index) => ({
    dataset: { groupId: decodeAttributeValue(match[1]) },
    querySelector(selector) { return selector === '.cache-selection-delete' ? deleteButtons[index] : null; }
  }));
  section.querySelectorAll = selector => {
    if (selector === '.cache-selection-delete') return deleteButtons;
    if (selector === '.cache-selection-pick') return pickButtons;
    if (selector === '.cache-selection-group') return rows;
    return [];
  };
  section.querySelector = () => null;
  section.setAttribute = (name, attributeValue) => {
    section.attributes = { ...(section.attributes || {}), [name]: attributeValue };
    modalHTML = modalHTML.replace(/<section id="cacheSelectionSection"[^>]*>/, tag => {
      const without = tag.replace(/\saria-busy="[^"]*"/, '');
      return without.replace(/>$/, ` aria-busy="${attributeValue}">`);
    });
  };
  if (value.includes('id="cacheContentsHeading"')) {
    const heading = { focused: false, focus() { this.focused = true; } };
    nodes.set('#cacheContentsHeading', heading);
  } else nodes.delete('#cacheContentsHeading');
  if (value.includes('id="cacheContentsScroll"')) {
    const scroll = {
      scrollTop: 0,
      insertAdjacentHTML(_position, html) {
        modalHTML = modalHTML.replace(/(<div id="cacheContentsScroll"[^>]*>)/, `$1${html}`);
      }
    };
    nodes.set('#cacheContentsScroll', scroll);
  } else nodes.delete('#cacheContentsScroll');
}
const modalBody = {
  insertAdjacentHTML(position, html) {
    modalHTML = position === 'afterbegin' ? html + modalHTML : modalHTML + html;
    if (html.includes('id="cacheSelectionSection"') && !nodes.has('#cacheSelectionSection')) {
      const section = {
        attributes: {},
        set innerHTML(value) { setSectionHTML(this, value); },
        insertAdjacentHTML(_p, value) { modalHTML += value; }
      };
      nodes.set('#cacheSelectionSection', section);
    }
    if (html.includes('id="cacheSelectionSection"')) {
      const inner = html.match(/<section id="cacheSelectionSection"[^>]*>([\s\S]*?)<\/section>/)?.[1];
      if (inner !== undefined) setSectionHTML(nodes.get('#cacheSelectionSection'), inner);
    }
    syncConfirmButtons();
  },
  set innerHTML(value) {
    modalHTML = value;
    nodes.delete('#cacheSelectionSection');
    nodes.delete('#cacheContentsHeading');
    nodes.delete('#cacheContentsScroll');
    const sectionMatch = value.match(/<section id="cacheSelectionSection"[^>]*>([\s\S]*?)<\/section>/);
    if (sectionMatch) {
      const section = {
        attributes: {},
        set innerHTML(content) { setSectionHTML(this, content); },
        insertAdjacentHTML(_p, content) { modalHTML += content; }
      };
      const busy = value.match(/<section id="cacheSelectionSection"[^>]*aria-busy="([^"]*)"/);
      if (busy) section.attributes['aria-busy'] = busy[1];
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
    } else if (options.method === 'DELETE' && path.startsWith('/api/cache/')) {
      result = { success: true, message: 'Fixture removed the whole model folder' };
    } else if (path.startsWith('/api/cache/')) {
      if (delayNextDetails) {
        const delay = delayNextDetails;
        delayNextDetails = null;
        await delay.promise;
      }
      const dataset = path.includes('type=dataset');
      result = { repo: dataset ? 'org/data' : 'org/model', type: dataset ? 'dataset' : 'model', owner: 'org', name: dataset ? 'data' : 'model', files: detailsFiles, path: dataset ? '/datasets/data' : detailsPath, friendlyPath: detailsFriendlyPath, sizeHuman: detailsSizeHuman, fileCount: detailsFileCount, source: dataset ? datasetSource : detailsSource, commit: detailsCommit, downloadStatus: detailsDownloadStatus, manifest: detailsManifest, wholeFolderDeletable: detailsWholeFolder, wholeFolderDeleteReason: detailsWholeFolderReason };
    } else if (path === '/api/cache') {
      result = { repos: [], stats: {} };
    } else {
      result = { repo: 'org/data', type: 'dataset', owner: 'org', name: 'data', files: [], path: '/datasets/data', sizeHuman: '1 GB', fileCount: 1, source: 'HF cache' };
    }
    return { ok: true, json: async () => result };
  }
};
let source = fs.readFileSync(path.join(__dirname, '..', '..', 'assets', 'static', 'js', 'app.js'), 'utf8');
source = source.replace(
  '  function navigateToAnalyze(repoId, repoType = null, revision = null) {',
  '  function navigateToAnalyze(repoId, repoType = null, revision = null) {\n    window.__navCalls.push([repoId, repoType, revision]);\n    return;'
);
source = source.replace('  // Start\n', `window.test = { showCacheDetails: window.showCacheDetails };\n  // Start\n`);
window.__navCalls = [];
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
  assert(modalHTML.includes('title="same-q-family-a"'), 'the row names its group and keeps the full value in title');
  assert(modalHTML.includes('Only this link; weights remain') === false, 'member messages stay in the confirmation inventory');
  assert(modalHTML.includes('link only'), 'a link-only member adds a facts line');
  assert(modalHTML.includes('2 shards'), 'the split group names its shard count');
  assert(modalHTML.includes('aria-label="Delete Q4_K_M same-q-family-a"'), 'each row action names its variant for assistive technology');
  assert(!/Show \d+ file record/.test(modalHTML), 'the old per-row inventory disclosure is gone');
  assert(!modalHTML.includes('<img src=x'), 'server-provided text is escaped in the card');
  const headerHTML = modalHTML.match(/<div class="cache-detail-header">[\s\S]*?<\/div>\s*<section id="cacheSelectionSection"/)?.[0] || '';
  const factsHTML = headerHTML.match(/<p class="cache-detail-facts">([\s\S]*?)<\/p>/)?.[1] || '';
  assert.equal(textContent(factsHTML), 'This copy · 1 GB · 30 files · abcdef1', 'the facts line describes the opened copy compactly');
  assert(factsHTML.includes('<code class="cache-detail-commit" title="abcdef1234567890">abcdef1</code>'), 'the short commit keeps the full value in title');
  assert(!factsHTML.includes('<button') && !factsHTML.includes('onclick='), 'the facts line stays non-interactive');
  const repoButtonTag = headerHTML.match(/<button class="cache-detail-repo"[^>]*>/)?.[0] || '';
  assert(repoButtonTag.includes('type="button"'), 'the repository title is a real button');
  assert(repoButtonTag.includes('onclick="openCacheDetailsRepository()"'), 'the button delegates to the live-context handler');
  assert(repoButtonTag.includes('aria-label="Open org/model"'), 'the button names the repository it opens');
  assert(headerHTML.includes('<span class="cache-detail-owner">org/</span><span class="cache-detail-name">model</span></button>'), 'the repo line still reads owner/name');
  assert(!headerHTML.includes('cache-detail-hub-link'), 'a local copy keeps no external Hub link');
  assert(!modalHTML.includes('id="cacheDetailPanelTitle"') && !modalHTML.includes('cache-detail-def'), 'the old This copy panel and definition rows are gone');
  assert(!modalHTML.includes('id="cacheLocationChange"') && !modalHTML.includes('id="cacheLocationPicker"'), 'the Change button and hidden picker are gone');
  assert(!modalHTML.includes('Not reported'), 'unreported facts are omitted rather than labelled');
  assert(!modalHTML.includes('Repository metadata'), 'the collapsed metadata copy is gone');
  assert(!modalHTML.includes('cache-detail-paths') && !modalHTML.includes('cache-path-item'), 'no second full paths section remains');
  assert.equal((modalHTML.match(/id="cacheSelectionLocation"/g) || []).length, 1, 'the dialog has exactly one location select');
  assert.equal((modalHTML.match(/<select id="cacheSelectionLocation"/g) || []).length, 1, 'the location control is a native select');
  const firstSelect = nodes.get('#cacheSelectionLocation');
  assert.equal(firstSelect.ariaLabel, 'Files from', 'the location select has an accessible name');
  assert.equal(firstSelect.title, '/scan/one/"archive"', 'the location select keeps the full path in title');
  assert.equal(modalHTML.includes('title="/scan/one/&quot;archive&quot;"'), true);
  assert.equal(modalHTML.includes('GGUF groups in '), false, 'the heading no longer repeats the source');
  assert.equal(textContent(modalHTML.match(/<h4 id="cacheContentsHeading"[^>]*>([\s\S]*?)<\/h4>/)?.[1] || ''), 'GGUF groups', 'the contents heading is just GGUF groups');
  assert(!modalHTML.includes('cache-contents-location"><code'), 'the duplicate path line is gone');
  assert(!headerHTML.includes('/friendly/model') && !headerHTML.includes('/scan/one'), 'the header carries no location path');
  assert(!modalHTML.includes('cache-detail-secondary'), 'the header carries no friendly or download secondary line');
  assert.equal(nodes.get('#cacheSelectionSection').querySelectorAll('.cache-selection-delete').filter(button => button.disabled).length, 1, 'only the unverifiable local group is disabled');
  assert(!modalHTML.includes('Only this group will be removed.'), 'enabled rows no longer repeat a redundant help line');
  // The danger zone legitimately renders the whole-folder button for local
  // sources, so the group rows carry the original claim that they never call
  // whole-repository delete.
  const groupSectionHTML = nodes.get('#cacheSelectionSection').html || '';
  const groupArticles = groupSectionHTML.match(/<article class="cache-selection-group"[\s\S]*?<\/article>/g) || [];
  assert.equal(groupArticles.length, 10);
  assert(groupArticles.every(article => !article.includes('confirmDeleteCache')), 'model groups must not call whole-repository delete');
  assert(!calls.some(call => call.method === 'DELETE'));
  assert(!modalHTML.includes('cache-selection-pick'), 'Delete is direct; no redundant Select action');
  const footerHTML = modalHTML.match(/<div class="cache-detail-actions">([\s\S]*?)<\/div>/)?.[1] || '';
  assert(footerHTML.includes('refreshCacheDetails()') && footerHTML.includes('hideModal()'), 'the footer offers Refresh and Close');
  assert(!footerHTML.includes('btn-danger') && !footerHTML.includes('Delete entire'), 'the footer has no destructive action');

  // Explicitly switch to the HF location and ensure it loads the opaque ID.
  currentSelection = { ...currentSelection, locations: [{ ...currentSelection.locations[1] }] };
  const select = nodes.get('#cacheSelectionLocation');
  select.value = '1';
  await select.listeners.change();
  assert(calls.some(call => call.path.includes('locationId=loc-2')));
  assert(modalHTML.includes('/scan/one'), 'location choices remain available after selecting one');
  assert.equal(textContent(modalHTML.match(/<h4 id="cacheContentsHeading"[^>]*>([\s\S]*?)<\/h4>/)?.[1] || ''), 'GGUF groups', 'the contents heading stays one short label after switching');
  assert(modalHTML.includes('/cache/hub/org--model'));
  assert(modalHTML.includes('2 shards') && modalHTML.includes('2 saved versions'), 'the HF group shows shards and saved versions as facts');
  assert(modalHTML.includes('title="/cache/hub/org--model"'), 'the selected location keeps its full path in title');
  assert.equal((modalHTML.match(/id="cacheSelectionLocation"/g) || []).length, 1, 'switching keeps one location select');
  assert(nodes.get('#cacheContentsHeading').focused, 'a location change returns focus to the contents heading');
  const hfRowArticles = modalHTML.match(/<article class="cache-selection-group"[^>]*>[\s\S]*?<\/article>/g) || [];
  const hfCompanionRow = hfRowArticles.find(article => article.includes('title="mmproj-F32.gguf"'));
  assert(hfCompanionRow && !hfCompanionRow.includes('cache-selection-facts'), 'a single file with one version and no link gets no facts line');
  assert.equal(nodes.get('#cacheSelectionSection').attributes['aria-busy'], 'false', 'the contents region is not busy after the render');
  assert.equal(nodes.get('#cacheSelectionSection').querySelectorAll('.cache-selection-delete').filter(button => button.disabled).length, 0, 'backend-advertised HF group action is enabled');
  assert.equal((modalHTML.match(/cache-badge-vision-encoder/g) || []).length, 1, 'the HF companion group renders its own badge');
  assert.equal((modalHTML.match(/class="cache-badge">Q4_K_M<\/span>/g) || []).length, 1, 'the HF llm group keeps its model-quant badge');

  // Refresh reloads the details snapshot but keeps the selected location.
  const refreshCalls = calls.length;
  await window.refreshCacheDetails();
  assert(calls.slice(refreshCalls).some(call => call.path === '/api/cache/org%2Fmodel?type=model'), 'Refresh reloads the opened copy');
  assert.equal(nodes.get('#cacheSelectionLocation').title, '/cache/hub/org--model', 'Refresh keeps the selected contents location');
  assert.equal((modalHTML.match(/id="cacheSelectionLocation"/g) || []).length, 1, 'Refresh keeps one location select');

  // HF confirmation groups four stored entries into two distinct paths and
  // names both saved versions without turning versions into extra shards.
  let hfSequence = await startGroupConfirmation(0);
  const hfConfirmation = confirmationHTML();
  assert.equal(nodes.get('#modalTitle').textContent, 'Delete Q4_K_M?', 'the versioned title names the quantization');
  assert(hfConfirmation.includes('This removes this group from all saved versions in this location; other groups and locations stay.'), 'the versioned consequence is one short sentence');
  assert(hfConfirmation.includes('<dt>Size</dt><dd class="cache-delete-size">10 B</dd>'), 'the versioned size counts each distinct path once');
  assert(hfConfirmation.includes('<dt>Files</dt>') && hfConfirmation.includes('2 files') && hfConfirmation.includes('2 saved versions'), 'the files row splits distinct paths from saved versions');
  assert.equal((hfConfirmation.match(/class="cache-delete-file"/g) || []).length, 2, 'four stored entries render as two distinct paths');
  assert(!hfConfirmation.includes('<details'), 'two distinct paths stay inline without a disclosure');
  assert.equal((hfConfirmation.match(/>model-00001-of-00002\.gguf</g) || []).length, 1, 'the first shard is one visible row');
  assert.equal((hfConfirmation.match(/>model-00002-of-00002\.gguf</g) || []).length, 1, 'the second shard is one visible row');
  assert.equal((hfConfirmation.match(/2 saved versions · commit-a, commit-b/g) || []).length, 2, 'each row suffixes its saved versions');
  assert(hfConfirmation.includes('<span class="cache-delete-file-size">5 B</span>'), 'the inventory names each file size');
  assert(!/<input[^>]*commit|<select[^>]*commit/i.test(hfConfirmation), 'HF confirmation does not offer revision input');
  assert(hfConfirmation.includes('<dt>Group</dt>') && hfConfirmation.includes('class="cache-badge">Q4_K_M</span>'), 'an llm group is still confirmed by its quant badge');
  assert(!hfConfirmation.includes('cache-badge-vision-encoder'), 'an llm versioned confirmation does not claim a vision-encoder role');
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
  const versionedCompanionConfirmation = confirmationHTML();
  assert.equal(nodes.get('#modalTitle').textContent, 'Delete this group?', 'the versioned companion keeps the generic title');
  assert(versionedCompanionConfirmation.includes('cache-badge-vision-encoder'), 'the versioned companion group row keeps the vision-encoder badge');
  assert(versionedCompanionConfirmation.includes('This removes this vision encoder companion from all saved versions in this location; other groups and locations stay.'), 'the versioned companion consequence names the role');
  assert(!versionedCompanionConfirmation.includes('This removes this group from all saved versions'), 'the versioned companion consequence does not use the generic group wording');
  assert(versionedCompanionConfirmation.includes('1 saved version · commit-a'), 'the versioned companion row lists its saved version');
  assert(versionedCompanionConfirmation.includes('<dt>Files</dt>') && versionedCompanionConfirmation.includes('1 file') && versionedCompanionConfirmation.includes('1 saved version'), 'the companion files row uses one consistent count');
  const beforeVersionedCompanion = deletedBodies.length;
  await cancelButton(versionedCompanionSequence).click();
  assert.equal(deletedBodies.length, beforeVersionedCompanion, 'cancelling the versioned companion confirmation sends no mutation');

  // Dataset details retain explicit typing and never request the GGUF endpoint.
  calls.length = 0;
  detailsFiles = [
    { name: 'data/train-00000.parquet', sizeHuman: '10 MB' },
    { name: 'data/train-00001.parquet', sizeHuman: '11 MB' },
    { name: 'README.md', sizeHuman: '2 KB' }
  ];
  detailsFileCount = 3;
  await window.test.showCacheDetails('org/data', 'dataset');
  assert(calls.some(call => call.path === '/api/cache/org%2Fdata?type=dataset'));
  assert(!calls.some(call => call.path.startsWith('/api/cache-selection')));
  assert(modalHTML.includes('Delete entire dataset cache'));
  const datasetHeaderHTML = modalHTML.match(/<div class="cache-detail-header">[\s\S]*?<\/div>\s*<section id="cacheSelectionSection"/)?.[0] || '';
  assert(datasetHeaderHTML.includes('aria-label="Open org/data"'), 'the dataset title opens the dataset repository');
  assert(datasetHeaderHTML.includes('<a class="cache-detail-hub-link" href="https://huggingface.co/datasets/org/data" target="_blank" rel="noopener">HF Hub ↗</a>'), 'the external Hub link stays a separate anchor');
  assert(datasetHeaderHTML.indexOf('</button>') < datasetHeaderHTML.indexOf('cache-detail-hub-link'), 'the Hub link stays a separate sibling after the repository button');
  assert.equal((modalHTML.match(/<h4 id="cacheContentsHeading"/g) || []).length, 1, 'the dataset uses one contents heading');
  assert(modalHTML.includes('>Files <span class="cache-selection-counts">· 3</span></h4>'), 'the single Files heading carries the count once');
  assert(!modalHTML.includes('>Files <span class="cache-selection-counts">3 files</span></h4>'), 'the heading does not repeat the files noun');
  assert(!modalHTML.includes('Files (0)'), 'the duplicate inner Files heading is gone');
  assert.equal((modalHTML.match(/id="cacheSelectionLocation"/g) || []).length, 0, 'datasets keep no location switch');
  assert.equal((modalHTML.match(/class="cache-files-list"/g) || []).length, 1, 'the dataset keeps its file list');
  assert(modalHTML.includes('data/train-00001.parquet'), 'the dataset file list keeps its entries');
  assert(modalHTML.includes('Delete entire dataset cache'), 'the dataset keeps its delete block');
  assert(!modalHTML.includes('Repository metadata') && !modalHTML.includes('cache-detail-metadata'), 'datasets lose the metadata block too');
  assert(!calls.some(call => call.method === 'DELETE'));
  detailsFiles = [];
  detailsFileCount = 30;

  // Existing whole-repository deletion remains available for an HF model
  // without any GGUF groups, and its scope is explicit (never a group action).
  calls.length = 0;
  detailsSource = 'HF cache';
  detailsFiles = [{ name: 'weights.safetensors', sizeHuman: '1 GB' }, ...Array.from({ length: 24 }, (_, i) => ({ name: `metadata-${i}.json`, sizeHuman: '1 KB' }))];
  currentSelection = { repo: 'org/non-gguf', type: 'model', locations: [] };
  await window.test.showCacheDetails('org/non-gguf', 'model');
  assert(modalHTML.includes('weights.safetensors'));
  assert(modalHTML.includes('metadata-23.json'), 'non-GGUF fallback retains all snapshot files beyond 20');
  assert(modalHTML.includes('No selectable model locations are available.'), 'the no-locations fallback stays readable');
  assert(!modalHTML.includes('id="cacheSelectionLocation"'), 'no locations means no empty select');
  assert(modalHTML.includes('Delete entire model cache'));
  assert(!modalHTML.includes('Delete this GGUF group'));
  assert(!calls.some(call => call.method === 'DELETE'), 'displaying the legacy action must not execute it');

  // Whole-model folder deletion extends the details action bar to local
  // sources. These rows drive the rendered onclick arguments end-to-end:
  // (a) the HF request stays byte-identical with a null body; (b) a Local
  // model with NO GGUF files at all (the acceptance pin) renders the folder
  // button, confirms with the strengthened folder copy, and sends the exact
  // folder; (c) Friendly view routes its tree with the folder pin; (d) local
  // datasets get no button (the decided scope).
  function decodeAttrValue(value) {
    return value.replace(/&quot;/g, '"').replace(/&#39;/g, "'").replace(/&lt;/g, '<').replace(/&gt;/g, '>').replace(/&amp;/g, '&');
  }
  function onclickArgs(fnName) {
    const match = modalHTML.match(new RegExp(`onclick="${fnName}\\(([^"]*)\\)"`));
    assert(match, `${fnName} onclick is rendered in the current modal`);
    const decoded = decodeAttrValue(match[1]);
    try {
      return JSON.parse(`[${decoded}]`);
    } catch (_) {
      // The legacy HF confirmation quotes arguments as JS strings.
      return JSON.parse(`[${decoded.replace(/'/g, '"')}]`);
    }
  }
  async function openWholeFolderDetails(source, path, files, wholeFolder = true) {
    calls.length = 0;
    detailsSource = source;
    detailsPath = path;
    detailsFiles = files;
    detailsWholeFolder = wholeFolder;
    currentSelection = { repo: 'org/model', type: 'model', locations: [] };
    await window.test.showCacheDetails('org/model', 'model');
  }
  async function driveWholeRepoDelete() {
    const confirmArgs = onclickArgs('confirmDeleteCache');
    await window.confirmDeleteCache(...confirmArgs);
    const confirmHTML = modalHTML;
    await window.deleteCache(...onclickArgs('deleteCache'));
    return { confirmHTML, confirmArgs };
  }

  // (a) HF cache: byte-identical button markup, unchanged confirmation copy,
  // unchanged request — query only, body stays null.
  await openWholeFolderDetails('HF cache', '/cache/hub/org--model', [{ name: 'weights.safetensors', sizeHuman: '1 GB' }]);
  const hfButton = modalHTML.match(/onclick="(confirmDeleteCache[^"]*)"/);
  assert(hfButton, 'HF model details render the whole-repository button');
  assert.equal(hfButton[1], 'confirmDeleteCache(&quot;org/model&quot;, &quot;model&quot;)', 'HF whole-repo button stays byte-identical');
  const hfDrive = await driveWholeRepoDelete();
  assert.equal(hfDrive.confirmArgs.length, 2, 'HF confirmation takes only repo and type');
  assert(hfDrive.confirmHTML.includes('<div class="delete-confirm">'), 'the whole-copy dialog keeps the shared centred container');
  assert(!hfDrive.confirmHTML.includes('cache-selection-confirmation'), 'group confirmation styling never reaches the whole-copy dialog');
  assert(hfDrive.confirmHTML.includes('Are you sure you want to delete <strong>org/model</strong> from the cache?'), 'HF confirmation copy unchanged');
  assert(hfDrive.confirmHTML.includes('This will permanently remove all cached files for this model. This action cannot be undone.'), 'HF confirmation note unchanged');
  const hfDelete = calls.find(call => call.method === 'DELETE');
  assert.equal(hfDelete.path, '/api/cache/org/model?type=model', 'HF request path unchanged');
  assert.deepEqual(hfDelete.body, null, 'HF request body unchanged');

  // (b) Local model without any GGUF files: folder button, strengthened
  // confirm copy naming the folder and the user's own files, and the exact
  // folder in the request body.
  await openWholeFolderDetails('Local', '/models-local/org/model', [
    { name: 'config.json', sizeHuman: '1 KB' },
    { name: 'model-00001-of-00002.safetensors', sizeHuman: '2 GB' },
    { name: 'model-00002-of-00002.safetensors', sizeHuman: '2 GB' }
  ]);
  assert(!modalHTML.includes('.gguf'), 'the acceptance-pin model has no GGUF files at all');
  assert(modalHTML.includes('Delete entire model folder'), 'Local model renders the folder button');
  const localDrive = await driveWholeRepoDelete();
  assert.deepEqual(localDrive.confirmArgs, ['org/model', 'model', 'Local', '/models-local/org/model'], 'the rendered onclick carries source and folder');
  assert(localDrive.confirmHTML.includes('Are you sure you want to delete the whole model folder <strong>/models-local/org/model</strong>?'), 'the confirm copy names the folder');
  assert(localDrive.confirmHTML.includes('including any of your own files stored there'), 'the confirm copy warns about the user\'s own files');
  assert(localDrive.confirmHTML.includes('This action cannot be undone.'), 'the confirm copy states it cannot be undone');
  const localDelete = calls.find(call => call.method === 'DELETE');
  assert.equal(localDelete.path, '/api/cache/org/model?type=model', 'the local request keeps the established endpoint');
  assert.deepEqual(Object.keys(localDelete.body).sort(), ['path', 'source'], 'the local request body carries exactly the discriminator and folder');
  assert.deepEqual(localDelete.body, { source: 'Local', path: '/models-local/org/model' }, 'the local request body pins the confirmed folder');

  // (c) Friendly view: cache-label button, strengthened confirm copy, and the
  // friendly folder in the request body.
  await openWholeFolderDetails('Friendly view', '/friendly-cache/models/org/model', [{ name: 'weights.safetensors', sizeHuman: '4 GB' }]);
  assert(modalHTML.includes('Delete entire model cache'), 'friendly view renders the cache button');
  const friendlyDrive = await driveWholeRepoDelete();
  assert.deepEqual(friendlyDrive.confirmArgs, ['org/model', 'model', 'Friendly view', '/friendly-cache/models/org/model'], 'the friendly onclick carries source and folder');
  assert(friendlyDrive.confirmHTML.includes('Are you sure you want to delete the whole model folder <strong>/friendly-cache/models/org/model</strong>?'), 'the friendly confirm copy names the folder');
  assert(friendlyDrive.confirmHTML.includes('including any of your own files stored there'), 'the friendly confirm copy warns about the user\'s own files');
  const friendlyDelete = calls.find(call => call.method === 'DELETE');
  assert.deepEqual(friendlyDelete.body, { source: 'Friendly view', path: '/friendly-cache/models/org/model' }, 'the friendly request body pins the confirmed folder');

  // (d) The decided scope is models only: a local dataset listing offers no
  // whole-repository button at all.
  datasetSource = 'Local';
  calls.length = 0;
  await window.test.showCacheDetails('org/data', 'dataset');
  assert(!modalHTML.includes('Delete entire'), 'local datasets get no whole-repository button');
  assert(!modalHTML.includes('confirmDeleteCache'), 'local datasets never offer whole-repository delete');
  assert(modalHTML.includes('Local datasets are view-only here'), 'the danger zone says local datasets are view-only');
  assert(!calls.some(call => call.method === 'DELETE'), 'no local-dataset delete is sent');
  datasetSource = 'HF cache';
  detailsPath = '/friendly/model';

  // (e) A shared pile folder is never a whole-folder target: the server marks
  // it ineligible (wholeFolderDeletable false) and the Local button does not
  // render, while the per-group rows for its models remain available.
  calls.length = 0;
  detailsSource = 'Local';
  detailsPath = '/piles/org/shared';
  detailsFiles = ['modelA-Q4_K_M', 'modelB-Q5_K_M', 'modelC-Q6_K', 'modelD-Q8_0', 'modelE-Q4_K_S'].map(n => ({ name: `${n}.gguf`, sizeHuman: '4 GB' }));
  detailsWholeFolder = false;
  detailsWholeFolderReason = 'This folder contains 5 separate model families.';
  currentSelection = JSON.parse(JSON.stringify(selectionFixture));
  await window.test.showCacheDetails('org/model', 'model');
  assert(!modalHTML.includes('Delete entire model folder'), 'a shared pile folder never renders the whole-folder button');
  assert(!modalHTML.includes('onclick="confirmDeleteCache'), 'a pile listing offers no whole-repository delete at all');
  assert(modalHTML.includes('Whole-folder deletion is not available for this location.'), 'the danger zone explains the withheld action');
  assert(modalHTML.includes('This folder contains 5 separate model families.'), 'the server reason is shown when the action is withheld');
  assert.equal((modalHTML.match(/class="cache-selection-group"/g) || []).length, 10, 'the pile keeps its per-group rows for targeted deletion');
  detailsWholeFolder = null;
  detailsWholeFolderReason = '';

  async function openLocalDetails() {
    detailsSource = 'Local';
    detailsFiles = [];
    detailsWholeFolder = true;
    detailsWholeFolderReason = '';
    currentSelection = JSON.parse(JSON.stringify(selectionFixture));
    await window.test.showCacheDetails('org/model', 'model');
  }
  function deleteControl(index = 0) {
    return nodes.get('#cacheSelectionSection').querySelectorAll('.cache-selection-delete')[index];
  }
  // The rendered group decides the dialog title: a quantization names itself,
  // while a group without one uses the generic title.
  function renderedGroup(index = 0) {
    const articles = modalHTML.match(/<article class="cache-selection-group"[^>]*>[\s\S]*?<\/article>/g) || [];
    const article = articles[index];
    assert(article, `rendered group ${index} is present in the current location`);
    return {
      quant: (article.match(/class="cache-badge">([^<]+)<\/span>/) || [])[1] || ''
    };
  }
  function expectedConfirmationTitle({ quant }) {
    return quant ? `Delete ${quant}?` : 'Delete this group?';
  }
  function confirmationHTML() {
    const start = modalHTML.indexOf('<div id="cache-delete-confirm-');
    assert(start >= 0, 'the group confirmation is rendered');
    return modalHTML.slice(start);
  }
  async function startGroupConfirmation(index = 0) {
    const control = deleteControl(index);
    assert(control && !control.disabled, 'eligible local group has an enabled delete action');
    const group = renderedGroup(index);
    await control.click();
    assert.equal(nodes.get('#modalTitle').textContent, expectedConfirmationTitle(group));
    return Number(modalHTML.match(/id="cache-delete-confirm-(\d+)"/)[1]);
  }
  function confirmButton(sequence) { return nodes.get(`#cache-delete-confirm-button-${sequence}`); }
  function cancelButton(sequence) { return nodes.get(`#cache-delete-cancel-${sequence}`); }

  // Confirmation is exact and complete (>20 members), and cancel performs no write.
  calls.length = 0;
  deletedBodies = [];
  await openLocalDetails();
  let sequence = await startGroupConfirmation(0);
  let confirmation = confirmationHTML();
  assert.equal(nodes.get('#modalTitle').textContent, 'Delete Q4_K_M?', 'the title names the quantization');
  assert(confirmation.includes('<dt>Repository</dt><dd>org/model</dd>'), 'the repository row shows owner/name');
  assert(confirmation.includes('<dt>Location</dt>') && confirmation.includes('title="/scan/one/&quot;archive&quot;"'), 'the location row names the source and keeps the full path in title');
  assert(confirmation.includes('<dt>Group</dt>') && confirmation.includes('class="cache-badge">Q4_K_M</span>') && confirmation.includes('same-q-family-a'), 'the group row pairs the quant badge with the label');
  assert(confirmation.includes('<dt>Size</dt><dd class="cache-delete-size">100 B</dd>'), 'the size sums the distinct files once');
  assert(confirmation.includes('<dt>Files</dt>') && confirmation.includes('25 files'), 'the files row counts distinct paths');
  assert(confirmation.includes('This removes only this group; other variants, companions and locations stay.'), 'the unversioned consequence is a short sentence');
  assert(confirmation.includes('<details class="cache-delete-inventory" open><summary>Files (25)</summary>'), 'a long inventory keeps its open disclosure');
  assert.equal((confirmation.match(/class="cache-delete-file"/g) || []).length, 25, 'every distinct file gets one row');
  assert(confirmation.includes('family/file-24.gguf') && confirmation.includes('<span class="cache-delete-file-size">4 B</span>'), 'the inventory names each file size');
  const firstFileRow = confirmation.match(/<li class="cache-delete-file">[\s\S]*?<\/li>/)?.[0] || '';
  assert(firstFileRow.indexOf('cache-delete-file-path') < firstFileRow.indexOf('cache-delete-file-size'), 'the path leads and the size follows it');
  assert.equal(deletedBodies.length, 0);
  await cancelButton(sequence).click();
  assert.equal(deletedBodies.length, 0, 'Cancel does not send DELETE');
  assert.equal(nodes.get('#modalTitle').textContent, 'Repository Details');
  assert(modalHTML.includes('/scan/one'), 'Cancel restores the chosen location');

  // Link-only warning is explicit in confirmation and cancel remains read-only.
  sequence = await startGroupConfirmation(1);
  confirmation = confirmationHTML();
  assert.equal(nodes.get('#modalTitle').textContent, 'Delete this group?', 'a group without a quant label uses the generic title');
  assert(confirmation.includes('class="cache-delete-file-linkonly">link only — the weight file remains</span>'), 'the link-only row carries its inline note');
  assert(!confirmation.includes('This group includes link-only entries'), 'the old link-only paragraph is gone');
  assert(!confirmation.includes('<dt>Size</dt>'), 'an unreported size is omitted instead of understated');
  await cancelButton(sequence).click();
  assert.equal(deletedBodies.length, 0);

  // A recognized split confirms every shard together, not a matching quant label.
  sequence = await startGroupConfirmation(6);
  confirmation = confirmationHTML();
  assert(confirmation.includes('parts/model-00001-of-00002.gguf'));
  assert(confirmation.includes('parts/model-00002-of-00002.gguf'));
  assert.equal((confirmation.match(/class="cache-delete-file"/g) || []).length, 2, 'each shard is its own row');
  assert(!confirmation.includes('<details'), 'a short inventory renders inline without a disclosure');
  assert(confirmation.includes('<dt>Files</dt>') && confirmation.includes('2 files'), 'the files row counts the two shards');
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
  assert(modalHTML.includes('>VISION<'), 'the companion badge names the artifact role');
  assert(modalHTML.includes('aria-label="Delete vision encoder mmproj-F32.gguf"'), 'the companion action carries its variant in the accessible name');
  assert(!modalHTML.includes('>F32<'), 'a companion never renders a model-quant badge');
  assert(modalHTML.includes('mmproj-F32.gguf'), 'the companion keeps its own group label');
  const companionSequence = await startGroupConfirmation(companionIndex);
  const companionConfirmation = confirmationHTML();
  assert.equal(nodes.get('#modalTitle').textContent, 'Delete this group?', 'a vision encoder has no quant title');
  assert.equal((companionConfirmation.match(/class="cache-delete-file"/g) || []).length, 1, 'only the companion member is confirmed');
  assert(!companionConfirmation.includes('<details'), 'the single companion file needs no disclosure');
  assert(companionConfirmation.includes('cache-badge-vision-encoder') && companionConfirmation.includes('>VISION</span>'), 'the group row badges the vision-encoder role');
  assert(companionConfirmation.includes('mmproj-F32.gguf'), 'the group row names the selected companion');
  assert(companionConfirmation.includes('>Delete vision encoder</button>'), 'the companion confirm button names the vision encoder');
  assert(!companionConfirmation.includes('>Delete group</button>'), 'the generic group confirm label is not used for a companion');
  assert(companionConfirmation.includes('This removes only this vision encoder companion; other variants, companions and locations stay.'), 'the companion consequence keeps the role wording and stays short');
  assert(!companionConfirmation.includes('metadata, directories'), 'the companion consequence does not reintroduce the old paragraph');
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
  const llmConfirmation = confirmationHTML();
  assert(llmConfirmation.includes('<dt>Group</dt>') && llmConfirmation.includes('class="cache-badge">Q4_K_M</span>'), 'an llm group is confirmed by its quant badge');
  assert(!llmConfirmation.includes('cache-badge-vision-encoder'), 'an llm confirmation does not claim a vision-encoder role');
  assert(llmConfirmation.includes('>Delete group</button>'), 'an llm group uses the short group confirm label');
  assert(!llmConfirmation.includes('>Delete vision encoder</button>'), 'the companion confirm label is not used for an llm group');
  assert(llmConfirmation.includes('This removes only this group; other variants, companions and locations stay.'), 'the llm consequence is the short unversioned sentence');
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
  nodes.get('#cacheContentsScroll').scrollTop = 240;
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
  assert(!modalHTML.includes('title="same-q-family-a"'), 'successful group is not left presented as present');
  assert(modalHTML.includes('same Q4 family family-b') && modalHTML.includes('/cache/hub/org--model'), 'other groups and locations remain visible');
  assert.equal(nodes.get('#cacheContentsScroll').scrollTop, 240, 'variant deletion restores the contents scroll position');
  assert(nodes.get('#cacheContentsHeading').focused, 'a removed row returns focus to the contents heading');

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
  assert(deleteControl(0).focused, 'a kept row receives focus after a refused delete');
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
  assert(modalHTML.includes('no other location was selected automatically'));
  assert.equal(nodes.get('#cacheSelectionLocation').value, '-1');
  assert(modalHTML.includes('/cache/hub/org--model'), 'the other location remains available but unselected');

  // Replacing Details during a pending write cannot let its old response
  // overwrite the new repository modal.
  await openLocalDetails();
  sequence = await startGroupConfirmation(0);
  let resolveDelete;
  pendingDelete = { promise: new Promise(resolve => { resolveDelete = resolve; }) };
  const pending = confirmButton(sequence).click();
  assert(confirmButton(sequence).disabled && cancelButton(sequence).disabled, 'the pending delete disables both dialog actions');
  assert.equal(nodes.get('#cache-delete-progress').textContent, 'Deleting the selected GGUF group…', 'the pending status line stays in place');
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
  assert(modalHTML.includes('2 shards'), 'the newer chosen location remains current after the old mutation finishes');

  // A late response from a replaced modal cannot overwrite its content.
  detailsSource = 'Local';
  let finishDetails;
  delayNextDetails = { promise: new Promise(resolve => { finishDetails = resolve; }) };
  const pendingDetails = window.test.showCacheDetails('org/model', 'model');
  nodes.get('#modalTitle').textContent = 'Another modal';
  finishDetails();
  await pendingDetails;
  assert(modalHTML.includes('Loading repository details…'), 'a stale response cannot overwrite the loading shell');
  assert(modalHTML.includes('cache-detail-modal') && modalHTML.includes('cache-detail-owner">org/') && modalHTML.includes('cache-detail-name">model<'), 'the loading shell keeps the repository identity visible');

  // Facts drop unreported values instead of inventing "Not reported"; the
  // friendly path and download provenance are optional single lines.
  async function reopenWith(overrides) {
    detailsSource = 'Local';
    detailsPath = '/opened/copy';
    detailsFriendlyPath = '';
    detailsSizeHuman = '';
    detailsFileCount = null;
    detailsCommit = '';
    detailsManifest = null;
    detailsDownloadStatus = 'unknown';
    detailsFiles = [];
    detailsWholeFolder = true;
    for (const [key, value] of Object.entries(overrides)) {
      if (key === 'sizeHuman') detailsSizeHuman = value;
      else if (key === 'fileCount') detailsFileCount = value;
      else if (key === 'commit') detailsCommit = value;
      else if (key === 'friendlyPath') detailsFriendlyPath = value;
      else if (key === 'manifest') detailsManifest = value;
      else if (key === 'downloadStatus') detailsDownloadStatus = value;
    }
    currentSelection = JSON.parse(JSON.stringify(selectionFixture));
    await window.test.showCacheDetails('org/model', 'model');
  }
  await reopenWith({});
  assert(!modalHTML.includes('class="cache-detail-facts"'), 'a copy with no reported facts renders no facts line');
  assert(!modalHTML.includes('cache-detail-secondary'), 'a copy with no extras renders only the identity line');
  await reopenWith({ sizeHuman: '2 GB', fileCount: 2, commit: 'abcdef1234567890', friendlyPath: '/friendly/view' });
  assert.equal(textContent(modalHTML.match(/<p class="cache-detail-facts">([\s\S]*?)<\/p>/)?.[1] || ''), 'This copy · 2 GB · 2 files · abcdef1', 'the facts line stays when a friendly path is present');
  assert(!modalHTML.includes('cache-detail-friendly') && !modalHTML.includes('cache-detail-secondary'), 'a friendly path adds no secondary line');
  assert(!modalHTML.includes('Friendly view') && !modalHTML.includes('/friendly/view'), 'the friendly path is not rendered in the header or a tooltip');
  await reopenWith({
    sizeHuman: '2 GB', fileCount: 2, commit: 'abcdef1234567890', downloadStatus: 'complete',
    manifest: { downloaded: '2026-11-16 18:42', commit: 'abcdef1234567890', command: 'hf download org/model --include "*.gguf" --local-dir /opened/copy', filters: '' }
  });
  assert.equal(textContent(modalHTML.match(/<p class="cache-detail-facts">([\s\S]*?)<\/p>/)?.[1] || ''), 'This copy · 2 GB · 2 files · abcdef1', 'the facts line stays when a manifest is present');
  assert(!modalHTML.includes('cache-detail-download') && !modalHTML.includes('cache-detail-secondary'), 'a manifest adds no provenance line');
  assert(!modalHTML.includes('Downloaded 2026-11-16') && !modalHTML.includes('Complete download'), 'the manifest date and status stay out of the header');
  assert(!modalHTML.includes('hf download org/model') && !modalHTML.includes('title="hf download org/model'), 'the manifest command stays out of the header and its tooltips');
  await reopenWith({
    sizeHuman: '2 GB', fileCount: 2, commit: 'abcdef1234567890', downloadStatus: 'filtered',
    manifest: { downloaded: '2026-11-15 09:10', commit: 'abcdef1234567890', command: '', filters: 'Q4_K_M' }
  });
  assert(!modalHTML.includes('Filtered download') && !modalHTML.includes('(Q4_K_M)'), 'a filtered manifest adds no provenance or filter text');
  assert(!modalHTML.includes('cache-detail-download') && !modalHTML.includes('cache-detail-secondary'), 'a filtered manifest adds no secondary line');
  assert.equal(textContent(modalHTML.match(/<p class="cache-detail-facts">([\s\S]*?)<\/p>/)?.[1] || ''), 'This copy · 2 GB · 2 files · abcdef1', 'the facts line stays for a filtered manifest');

  // The repository title uses the live context for the in-app overview and
  // closes Details first; the revision is deliberately null.
  await reopenWith({ sizeHuman: '2 GB', fileCount: 2, commit: 'abcdef1234567890' });
  window.__navCalls = [];
  assert(nodes.get('#modalBackdrop').classList.contains('active'), 'model Details is open before navigation');
  window.openCacheDetailsRepository();
  assert(!nodes.get('#modalBackdrop').classList.contains('active'), 'the model title button closes Details');
  assert.deepEqual(JSON.parse(JSON.stringify(window.__navCalls)), [['org/model', 'model', null]], 'the model title opens the default model revision');

  window.__navCalls = [];
  detailsSource = 'HF cache';
  detailsFiles = [{ name: 'data/train-00000.parquet', sizeHuman: '10 MB' }];
  detailsFileCount = 1;
  currentSelection = { repo: 'org/data', type: 'dataset', locations: [] };
  await window.test.showCacheDetails('org/data', 'dataset');
  assert(nodes.get('#modalBackdrop').classList.contains('active'), 'dataset Details is open before navigation');
  window.openCacheDetailsRepository();
  assert(!nodes.get('#modalBackdrop').classList.contains('active'), 'the dataset title button closes Details');
  assert.deepEqual(JSON.parse(JSON.stringify(window.__navCalls)), [['org/data', 'dataset', null]], 'the dataset title opens the default dataset revision');

  detailsSizeHuman = '1 GB';
  detailsFileCount = 30;
  detailsCommit = 'abcdef1234567890';
  detailsFriendlyPath = '';
  detailsManifest = null;
  detailsDownloadStatus = 'unknown';

  if (process.env.CACHE_UI_FIXTURE_URL) await checkBrowserLayout();
  console.log('PASS: two-line Repository Details header with a live-context repository title button, one facts line, no friendly/download secondary lines, one native location select in the sticky heading, GGUF rows without inventory disclosures, danger zone and ghost footer; location focusing; delete focus/scroll restoration; quant-titled, left-aligned group confirmations with definition rows, one-sentence consequences, right-aligned size rows, inline link-only notes, versioned and vision-encoder variants, pending disabled state, untouched whole-copy dialog; exact local/HF request composition; all saved versions; shared-payload/207/conflict results; cancel; exact refresh/context; dataset single Files heading and file list; full member inventories with sizes; metadata removal; model/dataset title navigation at the default revision; escaping');
})().catch(error => { console.error(error); process.exitCode = 1; });

// Optional real-DOM/layout layer against an isolated read-only fixture serving
// production index.html/app.js/style.css. No browser dependency is added to the app.
async function checkBrowserLayout() {
  const { chromium } = require('playwright');
  const browser = await chromium.launch({ headless: true, executablePath: process.env.CACHE_UI_BROWSER || undefined, args: ['--no-sandbox'] });
  const base = process.env.CACHE_UI_FIXTURE_URL;
  try {
    for (const width of [1000, 360]) {
      const page = await browser.newPage({ viewport: { width, height: 760 } });
      const errors = [];
      page.on('pageerror', error => errors.push(error.message));
      page.on('request', request => assert.equal(request.method(), 'GET', 'visual checks never send destructive requests'));
      await page.goto(base);
      await page.getByRole('heading', { name: 'GGUF groups', exact: true }).waitFor();
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
          dangerInside: scroll.contains(modal.querySelector('.cache-detail-danger')),
          headingOutside: !scroll.contains(section.querySelector('h4')),
          collapsed: Array.from(modal.querySelectorAll('details')).every(node => !node.open),
          facts: Array.from(modal.querySelectorAll('.cache-detail-header .cache-detail-facts'), node => node.textContent.trim()),
          selectCount: modal.querySelectorAll('select#cacheSelectionLocation').length,
          locationLabel: modal.querySelector('select#cacheSelectionLocation')?.getAttribute('aria-label'),
          pathTitle: modal.querySelector('select#cacheSelectionLocation')?.title,
          metadata: modal.querySelectorAll('.cache-detail-metadata, .cache-detail-panel, .cache-detail-def').length,
          documentOverflow: document.documentElement.scrollWidth > innerWidth,
          modalWidth: document.querySelector('#modal').getBoundingClientRect().width,
          actions: Array.from(modal.querySelectorAll('.cache-detail-actions .btn'), node => node.textContent.trim()),
          scrollHeight: scroll.clientHeight,
          rowHeight: modal.querySelector('.cache-selection-group').getBoundingClientRect().height,
          headerTop: modal.querySelector('.cache-detail-header').getBoundingClientRect().top,
          dangerTop: modal.querySelector('.cache-detail-danger').getBoundingClientRect().top
        };
      });
      assert.deepEqual(structure.children, width === 360
        ? ['cache-detail-header', 'cache-detail-section', 'cache-detail-actions']
        : ['cache-detail-header', 'cache-detail-section', 'cache-detail-actions', 'cache-detail-resize-grip']);
      assert.equal(structure.scrollCount, 1);
      for (const key of ['groupsInside', 'othersInside', 'dangerInside', 'headingOutside', 'collapsed']) assert(structure[key], key);
      assert.equal(structure.selectCount, 1);
      assert.equal(structure.locationLabel, 'Files from');
      assert.equal(structure.metadata, 0, 'no panel, metadata block, or definition rows');
      assert(structure.facts[0].startsWith('This copy · '), 'the facts line survives the compact header');
      assert(structure.pathTitle.includes('very-long-library-location'));
      assert.deepEqual(structure.actions, ['Refresh', 'Close']);
      assert(!structure.documentOverflow);
      assert(structure.modalWidth <= (width === 360 ? 360 : 600));
      if (width === 1000) assert.equal(structure.modalWidth, 600);
      if (width === 1000) assert(structure.scrollHeight >= 300, '760px keeps a usable contents viewport');
      else assert(structure.scrollHeight >= 150, '360px keeps usable contents rows');
      const repoTitle = page.locator('button.cache-detail-repo');
      assert.equal(await repoTitle.count(), 1, 'the repository title is the only title button');
      assert.equal(await repoTitle.getAttribute('aria-label'), 'Open org/model');
      const titleLayout = () => page.evaluate(() => {
        const button = document.querySelector('button.cache-detail-repo').getBoundingClientRect();
        const row = document.querySelector('.cache-detail-title-row').getBoundingClientRect();
        const facts = document.querySelector('.cache-detail-facts').getBoundingClientRect();
        const round = value => Math.round(value * 10) / 10;
        return { x: round(button.x), y: round(button.y), width: round(button.width), height: round(button.height), rowHeight: round(row.height), factsTop: round(facts.top) };
      });
      const titleBefore = await titleLayout();
      await repoTitle.hover();
      const hoverStyle = await repoTitle.evaluate(node => ({ decoration: getComputedStyle(node).textDecorationLine, cursor: getComputedStyle(node).cursor }));
      assert(hoverStyle.decoration.includes('underline'), 'the repository title underlines on hover');
      assert.equal(hoverStyle.cursor, 'pointer');
      assert.deepEqual(await titleLayout(), titleBefore, 'hover styling does not shift the header');
      await page.locator('#modalClose').focus();
      await page.keyboard.press('Tab');
      assert.equal(await page.evaluate(() => document.activeElement?.className), 'cache-detail-repo', 'Tab reaches the repository title');
      const focusStyle = await repoTitle.evaluate(node => ({
        visible: node.matches(':focus-visible'),
        outlineWidth: getComputedStyle(node).outlineWidth,
        outlineStyle: getComputedStyle(node).outlineStyle
      }));
      assert(focusStyle.visible && focusStyle.outlineStyle !== 'none' && focusStyle.outlineWidth === '2px', 'the repository title has a visible focus ring');
      assert.deepEqual(await titleLayout(), titleBefore, 'focus styling does not shift the header');
      await page.evaluate(() => document.activeElement?.blur());
      await page.mouse.move(1, 1);
      const compactRow = page.locator('.cache-selection-group').filter({ hasText: 'mmproj-F16.gguf' }).first();
      const compactBox = await compactRow.boundingBox();
      assert(compactBox.height <= 70, 'a row with one file and one version stays compact');
      assert(await page.locator('.cache-selection-group').filter({ hasText: '25 shards' }).count(), 'a multi-shard row keeps its facts line');
      const first = page.locator('.cache-selection-group').first();
      const file = await first.locator('.cache-selection-file-path').boundingBox();
      const del = await first.getByRole('button', { name: /^Delete Q4_K_M / }).boundingBox();
      assert(file.x + file.width <= del.x + 0.5, 'long path stays left of its action');
      const stationary = () => page.evaluate(() => ['.modal-header', '.cache-detail-header', '.cache-selection-sticky', '.cache-detail-actions'].map(selector => document.querySelector(selector).getBoundingClientRect().y));
      const before = await stationary();
      await page.screenshot({ path: `/tmp/opencode/repository-details-${width}-local.png` });
      await page.locator('.cache-detail-scroll').evaluate(node => { node.scrollTop = 450; });
      assert.deepEqual(await stationary(), before, 'chrome, identity, panel, heading, and footer stay still');
      const scrolling = await page.evaluate(() => Array.from(document.querySelectorAll('#modal *')).filter(node => ['auto', 'scroll'].includes(getComputedStyle(node).overflowY) && node.scrollHeight > node.clientHeight).map(node => node.className));
      assert.deepEqual(scrolling, ['cache-detail-scroll'], 'no nested/outer scroll owners');
      await page.getByText('Other files (2)', { exact: true }).click();
      assert.equal(await page.locator('.cache-selection-other[open] li').count(), 2);
      assert.deepEqual(await stationary(), before, 'expanding Other files does not move the fixed chrome');

      // The LLM confirmation is left-aligned, structured as definition rows,
      // and keeps a long inventory in one open disclosure.
      await page.getByRole('button', { name: 'Delete Q4_K_M same-q-family-a', exact: true }).click();
      await page.getByRole('heading', { name: 'Delete Q4_K_M?', exact: true }).waitFor();
      const llmDialog = await page.locator('.cache-selection-confirmation').evaluate(node => ({
        textAlign: getComputedStyle(node).textAlign,
        facts: Array.from(node.querySelectorAll('dt'), dt => dt.textContent),
        summary: node.querySelector('details > summary')?.textContent,
        files: node.querySelectorAll('.cache-delete-file').length
      }));
      assert.equal(llmDialog.textAlign, 'left', 'the group confirmation is left-aligned');
      assert.equal(await page.locator('.delete-confirm').count(), 0, 'the group confirmation does not opt into the centred shared layout');
      assert.deepEqual(llmDialog.facts, ['Repository', 'Location', 'Group', 'Size', 'Files']);
      assert.equal(llmDialog.summary, 'Files (25)');
      assert.equal(llmDialog.files, 25);
      assert.equal(await page.locator('.cache-delete-location-path').getAttribute('title'), structure.pathTitle);
      assert(!await page.evaluate(() => document.documentElement.scrollWidth > innerWidth), 'the dialog never widens the document');
      const llmRow = page.locator('.cache-delete-file').first();
      const llmRowBox = await llmRow.boundingBox();
      const llmPathBox = await llmRow.locator('.cache-delete-file-path').boundingBox();
      const llmSizeBox = await llmRow.locator('.cache-delete-file-size').boundingBox();
      assert(Math.abs((llmSizeBox.x + llmSizeBox.width) - (llmRowBox.x + llmRowBox.width)) <= 1, 'the size is right-aligned in its row');
      assert(Math.abs(llmPathBox.x - llmRowBox.x) <= 1, 'the path is left-aligned in its row');
      await page.screenshot({ path: `/tmp/opencode/gguf-delete-confirmation-llm-long-${width}x760.png` });
      await page.getByRole('button', { name: 'Cancel', exact: true }).click();
      await page.getByRole('heading', { name: 'Repository Details', exact: true }).waitFor();

      // A link-only member carries its warning inline in the error colour.
      await page.getByRole('button', { name: 'Delete model.gguf', exact: true }).click();
      await page.getByRole('heading', { name: 'Delete this group?', exact: true }).waitFor();
      const linkNote = page.locator('.cache-delete-file-linkonly');
      assert.equal((await linkNote.textContent()).trim(), 'link only — the weight file remains');
      assert.equal(await linkNote.evaluate(node => getComputedStyle(node).color), 'rgb(239, 68, 68)');
      await page.getByRole('button', { name: 'Cancel', exact: true }).click();
      await page.getByRole('heading', { name: 'Repository Details', exact: true }).waitFor();

      // A short LLM group shows the whole dialog including its actions.
      await page.getByRole('button', { name: 'Delete Q4_K_M same Q4 family family-c', exact: true }).click();
      await page.getByRole('heading', { name: 'Delete Q4_K_M?', exact: true }).waitFor();
      assert.equal(await page.locator('.cache-selection-confirmation details').count(), 0, 'one file stays inline');
      assert.equal(await page.getByRole('button', { name: 'Delete group', exact: true }).count(), 1);
      await page.screenshot({ path: `/tmp/opencode/gguf-delete-confirmation-llm-${width}x760.png` });
      await page.getByRole('button', { name: 'Cancel', exact: true }).click();
      await page.getByRole('heading', { name: 'Repository Details', exact: true }).waitFor();

      // A vision encoder is badged by role and uses the same short structure.
      await page.getByRole('button', { name: 'Delete vision encoder mmproj-F32.gguf', exact: true }).click();
      await page.getByRole('heading', { name: 'Delete this group?', exact: true }).waitFor();
      assert.equal(await page.locator('.cache-selection-confirmation .cache-badge-vision-encoder').textContent(), 'VISION');
      assert.equal(await page.getByRole('button', { name: 'Delete vision encoder', exact: true }).count(), 1);
      assert((await page.locator('.cache-delete-consequence').textContent()).includes('vision encoder companion'));
      assert.equal(await page.locator('.cache-selection-confirmation details').count(), 0, 'one companion file needs no disclosure');
      await page.screenshot({ path: `/tmp/opencode/gguf-delete-confirmation-vision-${width}x760.png` });
      await page.getByRole('button', { name: 'Cancel', exact: true }).click();
      await page.getByRole('heading', { name: 'Repository Details', exact: true }).waitFor();

      const locationSelect = page.getByLabel('Files from', { exact: true });
      await locationSelect.selectOption('1');
      await page.getByRole('heading', { name: 'GGUF groups', exact: true }).waitFor();
      await page.getByText('2 shards · 2 saved versions', { exact: true }).waitFor();
      assert((await locationSelect.getAttribute('title')).includes('/cache/hub/'));
      assert.equal(await page.evaluate(() => document.activeElement?.id), 'cacheContentsHeading', 'location change focuses the heading');
      assert.deepEqual(await stationary(), before, 'switching location keeps the chrome still');

      // Four stored entries stay two distinct file rows with one right-aligned
      // size each and the saved versions as a muted suffix.
      await page.getByRole('button', { name: 'Delete Q4_K_M same-q-family-b', exact: true }).click();
      await page.getByRole('heading', { name: 'Delete Q4_K_M?', exact: true }).waitFor();
      assert.equal(await page.locator('.cache-detail-modal').count(), 0, 'confirmation does not inherit Details layout');
      assert.equal(await page.locator('.cache-selection-confirmation details').count(), 0, 'a two-file inventory stays inline');
      assert.equal(await page.locator('.cache-delete-file').count(), 2, 'the four stored entries stay two distinct files');
      assert((await page.locator('.cache-delete-consequence').textContent()).includes('all saved versions in this location'));
      assert.equal((await page.locator('.cache-delete-saved-versions').textContent()).trim(), '· 2 saved versions');
      assert.equal((await page.locator('.cache-delete-size').textContent()).trim(), '10 B', 'the size counts each distinct path once');
      assert.equal((await page.locator('.cache-delete-file-size').first().textContent()).trim(), '5 B');
      assert.equal(await page.locator('.cache-delete-file-versions').first().textContent(), '2 saved versions · commit-a, commit-b');
      await page.screenshot({ path: `/tmp/opencode/gguf-delete-confirmation-versioned-${width}x760.png` });
      await page.getByRole('button', { name: 'Cancel', exact: true }).click();
      await page.getByRole('heading', { name: 'Repository Details', exact: true }).waitFor();
      await page.screenshot({ path: `/tmp/opencode/repository-details-${width}-hf.png` });

      // The whole-copy confirmation keeps the shared centred layout and style.
      const wholeCopyButton = page.locator('.cache-detail-danger .btn-danger');
      assert.equal(await wholeCopyButton.count(), 1, 'the danger zone keeps its whole-copy action');
      await wholeCopyButton.click();
      await page.getByRole('heading', { name: 'Delete from Cache', exact: true }).waitFor();
      const wholeCopy = await page.locator('.delete-confirm').evaluate(node => ({
        textAlign: getComputedStyle(node).textAlign,
        groupScoped: node.classList.contains('cache-selection-confirmation')
      }));
      assert.equal(wholeCopy.textAlign, 'center', 'the whole-copy confirmation stays centred');
      assert.equal(wholeCopy.groupScoped, false, 'the group confirmation styling never reaches the whole-copy dialog');
      assert(await page.getByText('Are you sure you want to delete', { exact: false }).isVisible());
      await page.getByRole('button', { name: 'Cancel', exact: true }).click();
      for (const variant of ['non-gguf', 'dataset']) {
        await page.goto(`${base}?case=${variant}`);
        if (variant === 'dataset') {
          await page.locator('.cache-files-list .cache-file-name').first().waitFor();
          await page.getByRole('button', { name: 'Delete entire dataset cache', exact: true }).waitFor();
          assert.equal(await page.locator('.cache-selection-group').count(), 0);
          assert.equal(await page.locator('select#cacheSelectionLocation').count(), 0, 'datasets keep no location switch');
          assert.equal(await page.locator('#cacheContentsHeading').count(), 1);
          assert.match((await page.locator('#cacheContentsHeading').textContent()).trim(), /^Files · \d+$/u, 'the dataset heading renders the count once without repeating the noun');
          assert.equal(await page.getByText('Files (1)', { exact: true }).count(), 0, 'no duplicate inner Files heading');
        } else {
          await page.getByText('weights.safetensors', { exact: true }).first().waitFor();
          await page.getByRole('button', { name: 'Delete entire model cache', exact: true }).waitFor();
          assert.equal(await page.locator('select#cacheSelectionLocation').count(), 0, 'no locations means no empty switch');
        }
        assert.equal(await page.locator('.cache-detail-scroll').count(), 1);
        assert.equal(await page.locator('.cache-detail-scroll .cache-detail-danger').count(), 1);
        assert.equal(await page.locator('.cache-detail-scroll .cache-detail-metadata').count(), 0);
        assert.equal(await page.locator('.cache-detail-scroll .cache-detail-section h4').count(), 0, 'no metadata headings remain');
        assert(await page.locator('.cache-detail-header .cache-detail-facts').isVisible(), 'the opened-copy facts stay visible');
        await page.screenshot({ path: `/tmp/opencode/repository-details-${width}-${variant}.png` });
      }
      await page.route('**/api/cache-selection*', route => route.fulfill({ status: 503, contentType: 'application/json', body: JSON.stringify({ error: 'Fixture unavailable' }) }));
      await page.goto(base);
      await page.getByText('Unable to load repository details:', { exact: false }).waitFor();
      assert.equal(await page.locator('.cache-detail-scroll .cache-files-list').count(), 1, 'failed selection preserves the snapshot file fallback');
      assert.equal(await page.locator('.cache-detail-scroll .cache-detail-metadata').count(), 0);
      assert.equal(await page.locator('.cache-detail-scroll .cache-detail-danger').count(), 1);
      assert(await page.locator('.cache-detail-header .cache-detail-name').isVisible(), 'the failed selection keeps identity visible');
      await page.getByRole('button', { name: 'Delete entire model folder', exact: true }).waitFor();
      await page.screenshot({ path: `/tmp/opencode/repository-details-${width}-selection-error.png` });
      await page.unroute('**/api/cache-selection*');
      let releaseSelection;
      const pendingSelection = new Promise(resolve => { releaseSelection = resolve; });
      await page.route('**/api/cache-selection*', async route => { await pendingSelection; await route.continue(); });
      await page.goto(base);
      await page.getByText('Loading GGUF groups…', { exact: true }).waitFor();
      assert.equal(await page.locator('.cache-detail-scroll').count(), 1);
      assert(await page.locator('.cache-detail-header .cache-detail-name').isVisible(), 'loading keeps identity visible');
      assert.equal(await page.getByRole('button', { name: 'Refresh', exact: true }).count(), 1);
      await page.getByRole('button', { name: 'Close', exact: true }).waitFor();
      await page.screenshot({ path: `/tmp/opencode/repository-details-${width}-loading.png` });
      releaseSelection();
      await page.getByRole('heading', { name: 'GGUF groups', exact: true }).waitFor();
      await page.keyboard.press('Escape');
      assert.equal(await page.locator('#modalBackdrop.active').count(), 0, 'Escape still closes Details');
      assert.deepEqual(errors, [], 'no page runtime errors');
      await page.close();
    }
    console.log('PASS: real DOM region order/ancestry; 760px contents viewport; 360px two-line rows and no horizontal scroll; compact facts header; title button hover/focus stability; one native location select in the sticky heading; HF shards/versions facts; left-aligned confirmation with definition rows, open long inventory, right-aligned sizes, versioned/vision screenshots, centred whole-copy dialog; dataset single Files heading and file list; no metadata block; loading/error states inside contents; no writes');
  } finally { await browser.close(); }
}
