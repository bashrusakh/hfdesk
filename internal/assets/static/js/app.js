/**
 * HFDesk - Modern Web UI
 */

(function() {
  'use strict';

  // =========================================
  // State
  // =========================================

  const state = {
    jobs: new Map(),
    settings: {},
    wsConnected: false,
    ws: null,
    currentPage: 'analyze'
  };

  // =========================================
  // DOM Elements
  // =========================================

  const $ = (sel) => document.querySelector(sel);
  const $$ = (sel) => document.querySelectorAll(sel);
  const isFilePreview = () => window.location.protocol === 'file:';

  // =========================================
  // Navigation
  // =========================================

  function initNavigation() {
    $$('.nav-item').forEach(item => {
      item.addEventListener('click', (e) => {
        e.preventDefault();
        const page = item.dataset.page;
        navigateTo(page);
      });
    });
  }

  function getInitialPage() {
    const allowed = new Set(['models', 'jobs', 'cache', 'mirror', 'history', 'settings']);
    const params = new URLSearchParams(window.location.search);
    const page = params.get('page') || window.location.hash.replace(/^#/, '');
    return allowed.has(page) ? page : 'models';
  }

  function navigateTo(page) {
    // Update nav
    $$('.nav-item').forEach(n => n.classList.remove('active'));
    $(`.nav-item[data-page="${page}"]`)?.classList.add('active');

    // Update page
    $$('.page').forEach(p => p.classList.remove('active'));
    $(`#page-${page}`)?.classList.add('active');

    state.currentPage = page;

    // Load page data
    if (isFilePreview()) {
      if (page === 'cache') loadCache();
      if (page === 'mirror') loadMirrorTargets();
    } else {
      if (page === 'cache') loadCache();
      if (page === 'jobs') loadJobs();
      if (page === 'settings') loadSettings();
      if (page === 'mirror') loadMirrorTargets();
      if (page === 'models') { loadModelsSearch(); loadStorageModeBadge(); loadDiskFreeIndicator(); }
      if (page === 'history') loadHistory();
    }
  }

  // =========================================
  // WebSocket
  // =========================================

  function initWebSocket() {
    const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:';
    const wsUrl = `${protocol}//${window.location.host}/api/ws`;

    try {
      state.ws = new WebSocket(wsUrl);

      state.ws.onopen = () => {
        state.wsConnected = true;
        updateConnectionStatus(true);
      };

      state.ws.onclose = () => {
        state.wsConnected = false;
        updateConnectionStatus(false);
        // Reconnect after 3 seconds
        setTimeout(initWebSocket, 3000);
      };

      state.ws.onmessage = (event) => {
        try {
          const msg = JSON.parse(event.data);
          handleWSMessage(msg);
        } catch (e) {
          console.error('WS parse error:', e);
        }
      };

      state.ws.onerror = (error) => {
        console.error('WS error:', error);
      };
    } catch (e) {
      console.error('WS connection failed:', e);
      setTimeout(initWebSocket, 3000);
    }
  }

  function updateConnectionStatus(connected) {
    const indicator = $('.status-indicator');
    const text = $('.status-text');

    if (connected) {
      indicator?.classList.add('connected');
      if (text) text.textContent = 'Connected';
    } else {
      indicator?.classList.remove('connected');
      if (text) text.textContent = 'Reconnecting...';
    }
  }

  // Jobs scheduled to be auto-moved into History so we don't double-schedule.
  const completedRemovalTimers = new Set();

  // When a download finishes successfully, move it out of Active Jobs into
  // History after a short delay (long enough to see it reach 100%). Only
  // completed jobs are auto-removed — failed/cancelled/paused stay visible
  // so unfinished work never disappears unexpectedly. The job is already
  // recorded in History server-side; we dismiss it so it doesn't linger.
  function scheduleCompletedRemoval(job) {
    if (!job || job.status !== 'completed' || completedRemovalTimers.has(job.id)) return;
    completedRemovalTimers.add(job.id);
    setTimeout(async () => {
      completedRemovalTimers.delete(job.id);
      const current = state.jobs.get(job.id);
      if (!current || current.status !== 'completed') return;
      try {
        await api('POST', `/jobs/${job.id}/dismiss`);
      } catch (e) { /* already removed server-side; ignore */ }
      state.jobs.delete(job.id);
      if (state.currentPage === 'jobs') renderJobs();
      updateJobsBadge();
      if (state.currentPage === 'history') loadHistory();
    }, 4000);
  }

  function handleWSMessage(msg) {
    if (msg.type === 'init') {
      // Initial state with all jobs
      const jobs = msg.data?.jobs || [];
      state.jobs.clear();
      jobs.forEach(job => {
        state.jobs.set(job.id, job);
        scheduleCompletedRemoval(job);
      });
      updateJobsBadge();
      if (state.currentPage === 'jobs') {
        renderJobs();
      }
    } else if (msg.type === 'job_update') {
      // Job update - data contains the full job object
      const job = msg.data;
      if (job && job.id) {
        state.jobs.set(job.id, job);
        updateJobsBadge();
        if (state.currentPage === 'jobs') {
          renderJobs();
        }
        scheduleCompletedRemoval(job);
      }
    }
  }

  function updateJobsBadge() {
    const activeCount = Array.from(state.jobs.values())
      .filter(j => j.status === 'running' || j.status === 'queued' || j.status === 'paused').length;

    const badge = $('#jobsBadge');
    if (badge) {
      if (activeCount > 0) {
        badge.textContent = activeCount;
        badge.style.display = 'block';
      } else {
        badge.style.display = 'none';
      }
    }
  }

  // =========================================
  // API Helpers
  // =========================================

  async function api(method, path, body = null) {
    const opts = {
      method,
      headers: { 'Content-Type': 'application/json' }
    };
    if (body) opts.body = JSON.stringify(body);

    const res = await fetch(`/api${path}`, opts);
    const data = await res.json();

    if (!res.ok) {
      const message = data.error_detail?.message || data.error || 'API error';
      throw new Error(message);
    }
    return data;
  }

  // =========================================
  // Analyze Page
  // =========================================

  function initAnalyzePage() {
    const input = $('#analyzeInput');
    const btn = $('#analyzeBtn');

    // Enter key
    input?.addEventListener('keypress', (e) => {
      if (e.key === 'Enter') analyzeRepo();
    });

    // Button click
    btn?.addEventListener('click', analyzeRepo);

    // Example buttons
    $$('.example-btn').forEach(btn => {
      btn.addEventListener('click', () => {
        if (input) input.value = btn.dataset.repo;
        // Pass 'dataset' type if specified on button
        const forceType = btn.dataset.type || null;
        analyzeRepo(forceType);
      });
    });
  }

// Store current analysis for wizard
let currentAnalysis = null;
let hasShownRevisionPicker = false; // Track if we've shown picker for this repo
let currentSelectedRepo = null;
let currentSelectedIsDataset = false;
let currentSelectedBranch = 'main';
let analyzeRequestId = 0;

function syncModelsDetailHeader(repoId, isDataset = false, branch = 'main') {
  const header = $('#modelsDetailHeader');
  const repoLabel = $('#modelsDetailRepo');
  const hfLink = $('#modelsDetailHFLink');

  if (header) header.style.display = '';
  if (repoLabel && repoId) repoLabel.textContent = repoId;
  if (hfLink && repoId) {
    hfLink.href = `https://huggingface.co/${isDataset ? 'datasets/' : ''}${repoId}`;
    hfLink.hidden = false;
  }
}

async function analyzeRepo(forceType = null, revision = null, repoOverride = null) {
  const input = $('#analyzeInput');
  const resultDiv = $('#analyzeResult');
  const isDataset = forceType === 'dataset'; // Only set if user explicitly selected dataset

  const repo = (repoOverride || input?.value || '').trim();
  if (!repo) {
    showToast('Please enter a repository', 'error');
    return;
  }

  const requestId = ++analyzeRequestId;

  // Reset revision picker flag when analyzing a new repo
  if (!revision) {
    hasShownRevisionPicker = false;
  }

  // Show loading — build with DOM methods to avoid XSS via repo/revision values
  {
    const loadingDiv = document.createElement('div');
    loadingDiv.className = 'loading-state';
    const spinner = document.createElement('div');
    spinner.className = 'spinner';
    const p = document.createElement('p');
    p.textContent = `Analyzing ${repo}${revision && revision !== 'main' ? ` (${revision})` : ''}...`;
    loadingDiv.appendChild(spinner);
    loadingDiv.appendChild(p);
    resultDiv.innerHTML = '';
    resultDiv.appendChild(loadingDiv);
  }

  try {
    let queryParams = [];
    if (forceType) queryParams.push(`dataset=${forceType === 'dataset'}`);
    if (revision) queryParams.push(`revision=${encodeURIComponent(revision)}`);
    const queryString = queryParams.length > 0 ? `?${queryParams.join('&')}` : '';

    const data = await api('GET', `/analyze/${repo}${queryString}`);

    if (requestId !== analyzeRequestId) return;

    // Check if we need user to select model vs dataset
    if (data.needsSelection) {
      renderTypeSelection(data);
      return;
    }

    // Check if there are multiple refs and we haven't shown the picker yet
    if (data.refs && data.refs.length > 1 && !hasShownRevisionPicker && !revision) {
      hasShownRevisionPicker = true;
      currentAnalysis = data;
      showRevisionPicker(data);
      return;
    }

    currentAnalysis = data;
    currentSelectedRepo = data.repo;
    currentSelectedIsDataset = !!data.is_dataset;
    currentSelectedBranch = data.branch || 'main';
    syncModelsDetailHeader(data.repo, data.is_dataset, currentSelectedBranch);
    await syncLocalAnalysisStatus(data);
    renderAnalysisResult(data);
  } catch (e) {
    if (requestId !== analyzeRequestId) return;
    resultDiv.innerHTML = `
      <div class="empty-state">
        <div class="empty-icon" style="color: var(--color-error);">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" width="64" height="64">
            <circle cx="12" cy="12" r="10"/><line x1="15" y1="9" x2="9" y2="15"/><line x1="9" y1="9" x2="15" y2="15"/>
          </svg>
        </div>
        <h3>Analysis Failed</h3>
        <p>${escapeHtml(e.message)}</p>
      </div>
    `;
  }
}

  // Show revision picker when multiple refs exist
  function showRevisionPicker(data) {
    const branches = data.refs.filter(r => r.type === 'branch');
    const tags = data.refs.filter(r => r.type === 'tag');

    let branchesHtml = '';
    if (branches.length > 0) {
      branchesHtml = `
        <div class="ref-group">
          <h5>Branches</h5>
          <div class="ref-list">
            ${branches.map(b => `
              <button class="ref-btn ${b.name === 'main' ? 'ref-default' : ''}" onclick="selectRevision('${escapeHtml(b.name)}', ${data.is_dataset}, '${escapeHtml(data.repo)}')">
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="16" height="16">
                  <line x1="6" y1="3" x2="6" y2="15"/><circle cx="18" cy="6" r="3"/><circle cx="6" cy="18" r="3"/><path d="M18 9a9 9 0 0 1-9 9"/>
                </svg>
                ${escapeHtml(b.name)}
                ${b.name === 'main' ? '<span class="ref-badge">default</span>' : ''}
              </button>
            `).join('')}
          </div>
        </div>
      `;
    }

    let tagsHtml = '';
    if (tags.length > 0) {
      tagsHtml = `
        <div class="ref-group">
          <h5>Tags</h5>
          <div class="ref-list">
            ${tags.slice(0, 10).map(t => `
              <button class="ref-btn" onclick="selectRevision('${escapeHtml(t.name)}', ${data.is_dataset}, '${escapeHtml(data.repo)}')">
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="16" height="16">
                  <path d="M20.59 13.41l-7.17 7.17a2 2 0 0 1-2.83 0L2 12V2h10l8.59 8.59a2 2 0 0 1 0 2.82z"/><line x1="7" y1="7" x2="7.01" y2="7"/>
                </svg>
                ${escapeHtml(t.name)}
              </button>
            `).join('')}
            ${tags.length > 10 ? `<div class="ref-more">... and ${tags.length - 10} more tags</div>` : ''}
          </div>
        </div>
      `;
    }

    showModal('Select Revision', `
      <p style="margin-bottom: 16px; color: var(--color-text-secondary);">
        This repository has multiple versions. Select which one to analyze:
      </p>
      ${branchesHtml}
      ${tagsHtml}
      <div class="form-actions" style="margin-top: 20px;">
        <button class="btn btn-ghost" onclick="hideModal(); selectRevision('main', ${data.is_dataset}, '${escapeHtml(data.repo)}')">Use default (main)</button>
      </div>
    `);
  }

  // Handle revision selection
  window.selectRevision = function(revision, isDataset, repoOverride = null) {
    hideModal();
    const forceType = isDataset ? 'dataset' : null;
    analyzeRepo(forceType, revision, repoOverride || currentAnalysis?.repo || null);
  };

  // Show revision picker from analysis result (user clicked "change")
  window.showRevisionPickerFromAnalysis = function() {
    if (currentAnalysis && currentAnalysis.refs) {
      hasShownRevisionPicker = false; // Allow showing picker again
      showRevisionPicker(currentAnalysis);
    }
  };

  // Render type selection when both model and dataset exist
  function renderTypeSelection(data) {
    const resultDiv = $('#analyzeResult');
    resultDiv.innerHTML = `
      <div class="analysis-card">
        <div class="analysis-header">
          <div class="analysis-repo">${escapeHtml(data.repo)}</div>
          <span class="analysis-type" style="background: var(--color-warning);">Selection Required</span>
        </div>
        <div class="analysis-body">
          <div class="analysis-section">
            <h4>${escapeHtml(data.message)}</h4>
            <div style="display: flex; gap: 16px; margin-top: 20px;">
              <button class="btn btn-primary" onclick="analyzeRepo('model', null, '${escapeHtml(data.repo)}')">
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="20" height="20" style="margin-right: 8px;">
                  <path d="M21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16z"/>
                </svg>
                Analyze as Model
              </button>
              <button class="btn btn-secondary" onclick="analyzeRepo('dataset', null, '${escapeHtml(data.repo)}')">
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="20" height="20" style="margin-right: 8px;">
                  <ellipse cx="12" cy="5" rx="9" ry="3"/><path d="M21 12c0 1.66-4 3-9 3s-9-1.34-9-3"/><path d="M3 5v14c0 1.66 4 3 9 3s9-1.34 9-3V5"/>
                </svg>
                Analyze as Dataset
              </button>
            </div>
          </div>
        </div>
      </div>
    `;
  }

  // Make analyzeRepo available globally for type selection buttons
  window.analyzeRepo = analyzeRepo;

  async function syncLocalAnalysisStatus(data) {
    if (!data?.repo || !data.selectable_items?.length) return;
    try {
      const cached = await api('GET', `/cache/${data.repo}`);
      const cachedFiles = new Set((cached.files || []).map(f => normalizeRepoPath(f.name)));
      if (!cachedFiles.size) return;

      const map = getDlStatus(data.repo);
      let changed = false;
      data.selectable_items.forEach(item => {
        const key = item.filter_value || item.id || '';
        const files = (item.files || []).map(normalizeRepoPath).filter(Boolean);
        if (!key || !files.length) return;
        if (files.every(f => cachedFiles.has(f))) {
          map[key] = 'done';
          changed = true;
        }
      });
      if (changed) {
        localStorage.setItem('dlStatus:' + data.repo, JSON.stringify(map));
      }
    } catch {
      // Cache lookup is best-effort; remote analysis should still render.
    }
  }

  function normalizeRepoPath(path) {
    return String(path || '').replace(/\\/g, '/').toLowerCase();
  }

  function renderAnalysisResult(data) {
    const resultDiv = $('#analyzeResult');
    if (!resultDiv) return;

    const filesHtml = data.files?.slice(0, 20).map(f => `
      <div class="analysis-file">
        <span class="analysis-file-name">${escapeHtml(f.path || f.name)}</span>
        <span class="analysis-file-size">${f.size_human || formatBytes(f.size)}</span>
      </div>
    `).join('') || '';

    const moreFiles = (data.files?.length || 0) > 20
      ? `<div class="analysis-file" style="justify-content: center; color: var(--color-text-muted);">
           ... and ${data.files.length - 20} more files
         </div>`
      : '';

    // Build type-specific info
    let typeInfoHtml = '';

    if (data.transformers) {
      const t = data.transformers;
      typeInfoHtml = `
        <div class="analysis-section">
          <h4>Model Configuration</h4>
          <div class="analysis-grid">
            ${t.architecture ? `<div class="analysis-stat"><div class="analysis-stat-label">Architecture</div><div class="analysis-stat-value">${escapeHtml(t.architecture)}</div></div>` : ''}
            ${t.estimated_parameters ? `<div class="analysis-stat"><div class="analysis-stat-label">Parameters</div><div class="analysis-stat-value">~${escapeHtml(t.estimated_parameters)}</div></div>` : ''}
            ${t.hidden_size ? `<div class="analysis-stat"><div class="analysis-stat-label">Hidden Size</div><div class="analysis-stat-value">${t.hidden_size}</div></div>` : ''}
            ${t.num_hidden_layers ? `<div class="analysis-stat"><div class="analysis-stat-label">Layers</div><div class="analysis-stat-value">${t.num_hidden_layers}</div></div>` : ''}
            ${t.context_length ? `<div class="analysis-stat"><div class="analysis-stat-label">Context Length</div><div class="analysis-stat-value">${t.context_length.toLocaleString()} tokens</div></div>` : ''}
            ${t.precision ? `<div class="analysis-stat"><div class="analysis-stat-label">Precision</div><div class="analysis-stat-value">${escapeHtml(t.precision)}</div></div>` : ''}
          </div>
        </div>
      `;
    }

    if (data.gguf) {
      const g = data.gguf;

      // Model lineage tree — Option B (indented tree, like HF card).
      // Only rendered when there are at least 2 nodes (i.e. chain has a parent).
      let chainHtml = '';
      if (g.model_chain && g.model_chain.length > 1) {
        const nodeHtml = g.model_chain.map((node, i) => {
          const isCurrent = !!node.is_current;
          const isRoot    = i === 0;
          const indent    = i * 14;
          const connector = i > 0 ? `<span style="color:var(--color-text-muted);font-size:11px;opacity:.6;flex-shrink:0">└─</span>` : '';
          const icon      = isCurrent ? '◉' : isRoot ? '◈' : '⚙';
          const relLabel  = isCurrent
            ? (node.relation ? node.relation + ' · this' : 'this')
            : isRoot
              ? 'base model'
              : (node.relation || '');
          const nameStyle = isCurrent
            ? 'font-size:11px;font-family:var(--font-mono);color:var(--color-text);font-weight:600;overflow:hidden;text-overflow:ellipsis;white-space:nowrap;min-width:0'
            : 'font-size:11px;font-family:var(--font-mono);color:var(--color-text-secondary);overflow:hidden;text-overflow:ellipsis;white-space:nowrap;min-width:0';
          const tagStyle = isCurrent
            ? 'font-size:10px;color:var(--color-info);flex-shrink:0;margin-left:2px'
            : 'font-size:10px;color:var(--color-text-muted);flex-shrink:0;margin-left:2px';
          return `<div style="display:flex;align-items:center;gap:5px;padding:2px 0;padding-left:${indent}px">
            ${connector}
            <span style="font-size:11px;opacity:.7;flex-shrink:0">${icon}</span>
            <span style="${nameStyle}">${escapeHtml(node.repo)}</span>
            ${relLabel ? `<span style="${tagStyle}">${escapeHtml(relLabel)}</span>` : ''}
          </div>`;
        }).join('');
        chainHtml = `
          <div style="margin-top:12px">
            <div class="analysis-stat-label" style="margin-bottom:6px">Model lineage</div>
            ${nodeHtml}
          </div>`;
      }

      typeInfoHtml = `
        <div class="analysis-section">
          <h4>GGUF Information</h4>
          <div class="analysis-grid">
            ${g.model_name ? `<div class="analysis-stat"><div class="analysis-stat-label">Model</div><div class="analysis-stat-value">${escapeHtml(g.model_name)}</div></div>` : ''}
            ${g.parameter_count ? `<div class="analysis-stat"><div class="analysis-stat-label">Parameters</div><div class="analysis-stat-value">${escapeHtml(g.parameter_count)}</div></div>` : ''}
          </div>
          ${chainHtml}
        </div>
      `;
    }

    if (data.diffusers) {
      const d = data.diffusers;
      typeInfoHtml = `
        <div class="analysis-section">
          <h4>Diffusers Pipeline</h4>
          <div class="analysis-grid">
            ${d.pipeline_type ? `<div class="analysis-stat"><div class="analysis-stat-label">Pipeline</div><div class="analysis-stat-value">${escapeHtml(d.pipeline_type)}</div></div>` : ''}
            ${d.diffusers_version ? `<div class="analysis-stat"><div class="analysis-stat-label">Version</div><div class="analysis-stat-value">${escapeHtml(d.diffusers_version)}</div></div>` : ''}
            ${d.variants?.length ? `<div class="analysis-stat"><div class="analysis-stat-label">Variants</div><div class="analysis-stat-value">${d.variants.join(', ')}</div></div>` : ''}
          </div>
        </div>
      `;
    }

    if (data.dataset) {
      const ds = data.dataset;
      typeInfoHtml = `
        <div class="analysis-section">
          <h4>Dataset Information</h4>
          <div class="analysis-grid">
            ${ds.primary_format ? `<div class="analysis-stat"><div class="analysis-stat-label">Format</div><div class="analysis-stat-value">${escapeHtml(ds.primary_format)}</div></div>` : ''}
            ${ds.configs?.length ? `<div class="analysis-stat"><div class="analysis-stat-label">Configs</div><div class="analysis-stat-value">${ds.configs.join(', ')}</div></div>` : ''}
          </div>
        </div>
      `;
    }

    if (data.lora) {
      const l = data.lora;
      typeInfoHtml = `
        <div class="analysis-section">
          <h4>LoRA Adapter Information</h4>
          <div class="analysis-grid">
            ${l.adapter_type ? `<div class="analysis-stat"><div class="analysis-stat-label">Adapter Type</div><div class="analysis-stat-value">${escapeHtml(l.adapter_type)}</div></div>` : ''}
            ${l.rank ? `<div class="analysis-stat"><div class="analysis-stat-label">Rank (r)</div><div class="analysis-stat-value">${l.rank}</div></div>` : ''}
            ${l.alpha ? `<div class="analysis-stat"><div class="analysis-stat-label">Alpha</div><div class="analysis-stat-value">${l.alpha}</div></div>` : ''}
            ${l.base_model ? `<div class="analysis-stat"><div class="analysis-stat-label">Base Model</div><div class="analysis-stat-value">${escapeHtml(l.base_model)}</div></div>` : ''}
          </div>
        </div>
      `;
    }

    if (data.quantized) {
      const q = data.quantized;
      typeInfoHtml = `
        <div class="analysis-section">
          <h4>Quantized Model Information</h4>
          <div class="analysis-grid">
            ${q.method ? `<div class="analysis-stat"><div class="analysis-stat-label">Method</div><div class="analysis-stat-value">${escapeHtml(q.method.toUpperCase())}</div></div>` : ''}
            ${q.bits ? `<div class="analysis-stat"><div class="analysis-stat-label">Bits</div><div class="analysis-stat-value">${q.bits}-bit</div></div>` : ''}
            ${q.group_size ? `<div class="analysis-stat"><div class="analysis-stat-label">Group Size</div><div class="analysis-stat-value">${q.group_size}</div></div>` : ''}
            ${q.backends?.length ? `<div class="analysis-stat"><div class="analysis-stat-label">Backends</div><div class="analysis-stat-value">${q.backends.slice(0,3).join(', ')}</div></div>` : ''}
          </div>
        </div>
      `;
    }

    // Build unified selectable items section (works for all types)
    let selectableItemsHtml = '';
    const hasSelectableItems = data.selectable_items && data.selectable_items.length > 0;

    if (hasSelectableItems) {
      selectableItemsHtml = `
        <div class="analysis-section">
          ${renderSelectableItems(data.selectable_items, 'selectableItems')}
        </div>
      `;
    }

    // Build related downloads section (for LoRA base models, etc.)
    const relatedDownloadsHtml = renderRelatedDownloads(data.related_downloads);
    const readmeHtml = `
      <div class="analysis-section readme-section" id="readmeSection" hidden>
        <h4>Description</h4>
        <div class="readme-preview" id="readmePreview"></div>
      </div>
    `;

    // Build branch/revision display
    const branchDisplay = data.branch && data.branch !== 'main'
      ? `<span class="analysis-branch" title="Revision: ${escapeHtml(data.branch)}">
           <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="14" height="14">
             <line x1="6" y1="3" x2="6" y2="15"/><circle cx="18" cy="6" r="3"/><circle cx="6" cy="18" r="3"/><path d="M18 9a9 9 0 0 1-9 9"/>
           </svg>
           ${escapeHtml(data.branch)}
         </span>`
      : '';

    // Show "Change" link if multiple refs available
    const changeRevisionLink = data.refs && data.refs.length > 1
      ? `<button class="btn-link" onclick="showRevisionPickerFromAnalysis()">change</button>`
      : '';

    // For non-GGUF/non-selectable models: show a simple file list with download buttons
    const nonSelectableFilesHtml = !hasSelectableItems && data.files?.length
      ? renderNonSelectableFiles(data)
      : '';

    resultDiv.innerHTML = `
      <div class="analysis-card">
        <div class="analysis-header">
          <div class="analysis-repo">${escapeHtml(data.repo)}${branchDisplay}${changeRevisionLink}</div>
          <span class="analysis-type">${escapeHtml(data.type_description || data.type)}</span>
          <div class="analysis-meta">
            <span>${data.file_count} files</span>
            <span>${data.total_size_human}</span>
          </div>
        </div>
        <div class="analysis-body">
          ${typeInfoHtml}
          ${selectableItemsHtml}
          ${relatedDownloadsHtml}
          ${nonSelectableFilesHtml}
          ${readmeHtml}
        </div>
        <div class="analysis-actions-wrapper">
          <div class="analysis-actions">
            <button class="btn btn-ghost" onclick="clearAnalysis()">Clear</button>
            ${!hasSelectableItems ? `
            <button class="btn btn-ghost" onclick="showAdvancedOptions()">Options</button>
            <button class="btn btn-primary" onclick="startWizardDownload('${escapeHtml(data.repo)}', ${data.is_dataset})">
              Download All
            </button>` : ''}
          </div>
        </div>
      </div>
    `;

    loadReadmePreview(data);
  }

  let _scrollArrowUp = null;
  let _scrollArrowDown = null;

  function updateScrollArrows() {
    const scroller = $('#analyzeResult');
    if (!_scrollArrowUp) _scrollArrowUp = $('#scrollArrowUp');
    if (!_scrollArrowDown) _scrollArrowDown = $('#scrollArrowDown');
    if (!scroller || !_scrollArrowUp || !_scrollArrowDown) return;

    const top = scroller.scrollTop;
    const maxScroll = scroller.scrollHeight - scroller.clientHeight;
    const atTop = top < 20;
    const atBottom = maxScroll <= 0 || top >= maxScroll - 20;

    _scrollArrowUp.classList.toggle('visible', !atTop);
    _scrollArrowDown.classList.toggle('visible', !atBottom);
  }

  async function loadReadmePreview(data) {
    const section = $('#readmeSection');
    const preview = $('#readmePreview');
    if (!section || !preview || !data?.repo) return;

    const params = new URLSearchParams();
    params.set('revision', data.branch || 'main');
    if (data.is_dataset) params.set('dataset', 'true');

    try {
      const readme = await api('GET', `/readme/${encodeURI(data.repo)}?${params.toString()}`);
      // The server returns sanitized HTML; fall back to the lightweight client
      // renderer only when it's missing (older server build).
      preview.innerHTML = readme.html
        ? readme.html
        : renderMarkdownPreview(readme.markdown || '', readme.baseRawURL || '');
      section.hidden = !preview.innerHTML.trim();
    } catch (_) {
      section.hidden = true;
    }
    updateScrollArrows();
  }

  function renderMarkdownPreview(markdown, baseRawURL) {
    if (!markdown) return '';

    let text = markdown
      .replace(/\r\n/g, '\n')
      .replace(/^---[\s\S]*?\n---\s*/m, '')
      .trim();
    if (!text) return '';

    const lines = text.split('\n');
    const html = [];
    let inCode = false;
    let paragraph = [];

    const flushParagraph = () => {
      if (!paragraph.length) return;
      html.push(`<p>${renderMarkdownInline(paragraph.join(' '), baseRawURL)}</p>`);
      paragraph = [];
    };

    for (const rawLine of lines.slice(0, 220)) {
      const line = rawLine.trimEnd();
      if (line.startsWith('```')) {
        flushParagraph();
        inCode = !inCode;
        if (inCode) html.push('<pre><code>');
        else html.push('</code></pre>');
        continue;
      }
      if (inCode) {
        html.push(escapeHtml(line) + '\n');
        continue;
      }
      if (!line.trim()) {
        flushParagraph();
        continue;
      }

      const imageMatch = line.match(/^!\[([^\]]*)\]\(([^)]+)\)/);
      if (imageMatch) {
        flushParagraph();
        const src = resolveReadmeURL(imageMatch[2], baseRawURL);
        html.push(`<img src="${escapeHtml(src)}" alt="${escapeHtml(imageMatch[1])}" loading="lazy">`);
        continue;
      }

      const heading = line.match(/^(#{1,3})\s+(.+)$/);
      if (heading) {
        flushParagraph();
        const level = Math.min(heading[1].length + 3, 6);
        html.push(`<h${level}>${renderMarkdownInline(heading[2], baseRawURL)}</h${level}>`);
        continue;
      }

      if (/^[-*]\s+/.test(line)) {
        flushParagraph();
        html.push(`<p class="readme-bullet">${renderMarkdownInline(line.replace(/^[-*]\s+/, ''), baseRawURL)}</p>`);
        continue;
      }

      paragraph.push(line);
    }
    flushParagraph();
    if (inCode) html.push('</code></pre>');
    return html.join('');
  }

  function renderMarkdownInline(text, baseRawURL) {
    let html = escapeHtml(text);
    html = html.replace(/`([^`]+)`/g, '<code>$1</code>');
    html = html.replace(/\*\*([^*]+)\*\*/g, '<strong>$1</strong>');
    html = html.replace(/\[([^\]]+)\]\(([^)]+)\)/g, (_, label, href) => {
      const url = resolveReadmeURL(href, baseRawURL);
      return `<a href="${escapeHtml(url)}" target="_blank" rel="noopener">${label}</a>`;
    });
    return html;
  }

  function resolveReadmeURL(url, baseRawURL) {
    const clean = String(url || '').trim();
    if (/^(https?:|data:)/i.test(clean)) return clean;
    try {
      return new URL(clean.replace(/^\.\//, ''), baseRawURL).toString();
    } catch (_) {
      return clean;
    }
  }

  // Clear analysis and reset to initial state
  window.clearAnalysis = function() {
    currentAnalysis = null;
    currentSelectedRepo = null;
    currentSelectedIsDataset = false;
    if (_scrollArrowUp) _scrollArrowUp.classList.remove('visible');
    if (_scrollArrowDown) _scrollArrowDown.classList.remove('visible');
    currentSelectedBranch = 'main';
    const header = $('#modelsDetailHeader');
    const hfLink = $('#modelsDetailHFLink');
    if (header) header.style.display = 'none';
    if (hfLink) hfLink.hidden = true;
    advancedOptions = { filter: '', exclude: '' };
    const input = $('#analyzeInput');
    if (input) input.value = '';
    _syncModelsSearchClear();

    const resultDiv = $('#analyzeResult');
    if (resultDiv) {
      resultDiv.innerHTML = `
        <div class="empty-state">
          <div class="empty-icon">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" width="64" height="64">
              <path d="M21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16z"/>
              <polyline points="3.27 6.96 12 12.01 20.73 6.96"/>
              <line x1="12" y1="22.08" x2="12" y2="12"/>
            </svg>
          </div>
          <h3>Analyze Model or Dataset</h3>
          <p>Enter a HuggingFace model or dataset ID - we'll auto-detect the type and show files, size, and download options.</p>
          <div class="example-repos">
            <span class="example-label">GGUF:</span>
            <button class="example-btn" data-repo="TheBloke/Mistral-7B-Instruct-v0.2-GGUF">Mistral-7B-GGUF</button>
            <button class="example-btn" data-repo="bartowski/Qwen2.5-7B-Instruct-GGUF">Qwen2.5-7B-GGUF</button>
          </div>
          <div class="example-repos">
            <span class="example-label">Diffusers:</span>
            <button class="example-btn" data-repo="stabilityai/stable-diffusion-xl-base-1.0">SDXL-base</button>
            <button class="example-btn" data-repo="runwayml/stable-diffusion-v1-5">SD-v1.5</button>
          </div>
          <div class="example-repos">
            <span class="example-label">LoRA/GPTQ:</span>
            <button class="example-btn" data-repo="predibase/glue_stsb">LoRA-adapter</button>
            <button class="example-btn" data-repo="TheBloke/Llama-2-7B-Chat-GPTQ">Llama-2-GPTQ</button>
          </div>
          <div class="example-repos">
            <span class="example-label">Datasets:</span>
            <button class="example-btn" data-repo="HuggingFaceFW/fineweb-edu" data-type="dataset">FineWeb-Edu</button>
            <button class="example-btn" data-repo="OpenAssistant/oasst1" data-type="dataset">OpenAssistant</button>
          </div>
        </div>
      `;
      // Re-attach example button handlers
      $$('.example-btn').forEach(btn => {
        btn.addEventListener('click', () => {
          if (input) input.value = btn.dataset.repo;
          const forceType = btn.dataset.type || null;
          analyzeRepo(forceType);
        });
      });
    }
  };

  // Store advanced options (filter/exclude only - revision comes from analysis)
  let advancedOptions = {
    filter: '',
    exclude: ''
  };

  // Show advanced options modal
  window.showAdvancedOptions = function() {
    if (!currentAnalysis) return;

    showModal('Advanced Options', `
      <div class="form-group">
        <label for="advFilter">File Filter (comma-separated)</label>
        <input type="text" id="advFilter" value="${escapeHtml(advancedOptions.filter)}" placeholder="e.g., q4_k_m,q5_k_m">
        <p class="form-hint">Only download files matching these patterns</p>
      </div>
      <div class="form-group">
        <label for="advExclude">Exclude Filter (comma-separated)</label>
        <input type="text" id="advExclude" value="${escapeHtml(advancedOptions.exclude)}" placeholder="e.g., fp16,bf16">
        <p class="form-hint">Skip files matching these patterns</p>
      </div>
      <div class="form-actions">
        <button class="btn btn-secondary" onclick="hideModal()">Cancel</button>
        <button class="btn btn-primary" onclick="applyAdvancedOptions()">Apply</button>
      </div>
    `);
  };

  // Apply advanced options and update command preview
  window.applyAdvancedOptions = function() {
    advancedOptions.filter = $('#advFilter')?.value || '';
    advancedOptions.exclude = $('#advExclude')?.value || '';

    hideModal();
    showToast('Options applied', 'success');
  };

  // Start download from wizard with selected options
  window.startWizardDownload = async function(repo, isDataset) {
    const filters = advancedOptions.filter
      ? advancedOptions.filter.split(',').map(s => s.trim()).filter(Boolean)
      : [];

    // Build excludes from advanced options
    const excludes = advancedOptions.exclude
      ? advancedOptions.exclude.split(',').map(s => s.trim()).filter(Boolean)
      : [];

    try {
      const body = {
        repo,
        revision: currentAnalysis?.branch || 'main',
        dataset: isDataset,
        filters,
        excludes,
        // Manual filters name specific quant/file segments, so keep matching
        // exact enough that q6_k does not also pull q6_k_xl.
        exactMatch: filters.length > 0
      };

      await api('POST', '/download', body);
      showToast(`Download started: ${repo}`, 'success');
      navigateTo('jobs');
    } catch (e) {
      showToast(`Failed: ${e.message}`, 'error');
    }
  };

  // downloadFromAnalysis: show a simple download modal in the unified Models page
  window.downloadFromAnalysis = function(repo, isDataset) {
    const rev = currentAnalysis?.branch || 'main';
    showModal('Download', `
      <div class="form-group">
        <label>Repository</label>
        <div style="font-weight:600; padding: 6px 0;">${escapeHtml(repo)}</div>
      </div>
      <div class="form-group">
        <label for="dlModalLocalDir">Save to folder <span class="form-hint-inline">(optional)</span></label>
        <input type="text" id="dlModalLocalDir" placeholder="leave blank for Settings default">
      </div>
      <div class="form-actions">
        <button class="btn btn-secondary" onclick="hideModal()">Cancel</button>
        <button class="btn btn-primary" onclick="confirmDirectDownload('${escapeHtml(repo)}', ${isDataset}, '${escapeHtml(rev)}')">
          Download
        </button>
      </div>
    `);
  };

  window.confirmDirectDownload = async function(repo, isDataset, revision) {
    hideModal();
    const localDir = $('#dlModalLocalDir')?.value?.trim() || '';
    try {
      const df = await fetch('/api/diskfree' + (localDir ? `?path=${encodeURIComponent(localDir)}` : '')).then(r => r.json()).catch(() => null);
      if (df && df.free < 100 * 1024 * 1024) {
        showToast(`Not enough disk space: ${formatBytes(df.free)} free`, 'error');
        return;
      }
      const body = { repo, revision: revision || 'main', dataset: isDataset };
      if (localDir) body.localDir = localDir;
      await api('POST', '/download', body);
      showToast(`Download started: ${repo}`, 'success');
      navigateTo('jobs');
    } catch (e) {
      showToast(`Failed: ${e.message}`, 'error');
    }
  };

  // =========================================
  // Download Page
  // =========================================

  function initDownloadPage() {
    // Model form
    $('#modelForm')?.addEventListener('submit', async (e) => {
      e.preventDefault();
      await startDownload('model');
    });

    // Dataset form
    $('#datasetForm')?.addEventListener('submit', async (e) => {
      e.preventDefault();
      await startDownload('dataset');
    });

    // Preview buttons
    $('#previewModelBtn')?.addEventListener('click', () => previewDownload('model'));
    $('#previewDatasetBtn')?.addEventListener('click', () => previewDownload('dataset'));

    // Show the server's storage mode and disk-free info
    loadStorageModeBadge();
    loadDiskFreeIndicator();
  }

  async function loadDiskFreeIndicator() {
    const el = $('#diskFreeIndicator');
    if (!el) return;
    try {
      const df = await fetch('/api/diskfree').then(r => r.json());
      const free  = df.free  || 0;
      const total = df.total || 0;
      // freePct = free / total × 100 (how much is available)
      const freePct = total ? Math.round(free / total * 100) : 0;
      const cls = free < 2  * 1024 * 1024 * 1024
        ? (free < 500 * 1024 * 1024 ? 'disk-free-danger' : 'disk-free-warn')
        : 'disk-free-ok';
      el.className = `disk-free-indicator ${cls}`;
      // Show drive letter / path root for clarity
      const drivePart = df.path ? ` (${df.path.replace(/^([A-Z]:\\?).*$/i,'$1').replace(/^(\/[^/]*).*$/,'$1')})` : '';
      el.textContent = `Disk${drivePart}: ${formatBytes(free)} free · ${freePct}% of ${formatBytes(total)}`;
      el.title = `Path: ${df.path || '?'} — ${formatBytes(free)} available of ${formatBytes(total)} total`;
      el.hidden = false;
    } catch (_) { el.hidden = true; }
  }

  async function loadStorageModeBadge() {
    const badge = $('#storageModeBadge');
    if (!badge) return;
    try {
      const s = await api('GET', '/settings');
      if (s.storageMode === 'local') {
        badge.textContent = `Storage: local files → ${s.localDir}`;
        badge.title = 'Server started with --local-dir: downloads are saved as real files and scanned by the Cache browser.';
        badge.classList.add('storage-mode-local');
      } else {
        badge.textContent = `Storage: HF cache → ${s.cacheDir}`;
        badge.title = 'Downloads use the HuggingFace cache layout. The Cache browser also scans friendly and local model folders.';
        badge.classList.remove('storage-mode-local');
      }
      badge.hidden = false;
    } catch (e) {
      badge.hidden = true;
    }
  }

  async function startDownload(type) {
    const isDataset = type === 'dataset';
    const prefix = isDataset ? 'dataset' : 'model';

    const repo = $(`#${prefix}Repo`)?.value.trim();
    const revision = $(`#${prefix}Revision`)?.value.trim() || 'main';
    const filter = $(`#${prefix}Filter`)?.value.trim();
    const exclude = $(`#${prefix}Exclude`)?.value.trim();
    const exactMatch = $(`#${prefix}Exact`)?.checked || false;
    const localDir = $(`#${prefix}LocalDir`)?.value.trim() || '';

    if (!repo) {
      showToast('Please enter a repository', 'error');
      return;
    }

    // Disk-free guard: warn if < 2 GB, block if < 100 MB
    try {
      const dfParams = localDir ? `?path=${encodeURIComponent(localDir)}` : '';
      const df = await fetch('/api/diskfree' + dfParams).then(r => r.json());
      const freeMB = df.free / (1024 * 1024);
      if (freeMB < 100) {
        showToast(`Not enough disk space: only ${formatBytes(df.free)} free`, 'error');
        return;
      }
      if (freeMB < 2048) {
        showToast(`Low disk space warning: ${formatBytes(df.free)} free on ${df.path}`, 'warning');
      }
    } catch (_) { /* non-fatal */ }

    const body = {
      repo,
      revision,
      dataset: isDataset,
      filters: filter ? filter.split(',').map(s => s.trim()).filter(Boolean) : [],
      excludes: exclude ? exclude.split(',').map(s => s.trim()).filter(Boolean) : [],
      exactMatch,
    };
    if (localDir) body.localDir = localDir;

    try {
      await api('POST', '/download', body);
      showToast(`Download started: ${repo}`, 'success');
      navigateTo('jobs');
    } catch (e) {
      showToast(`Failed: ${e.message}`, 'error');
    }
  }

  async function previewDownload(type) {
    const isDataset = type === 'dataset';
    const prefix = isDataset ? 'dataset' : 'model';

    const repo = $(`#${prefix}Repo`)?.value.trim();
    if (!repo) {
      showToast('Please enter a repository', 'error');
      return;
    }

    const body = {
      repo,
      revision: $(`#${prefix}Revision`)?.value.trim() || 'main',
      dataset: isDataset,
      filters: ($(`#${prefix}Filter`)?.value || '').split(',').map(s => s.trim()).filter(Boolean),
      excludes: ($(`#${prefix}Exclude`)?.value || '').split(',').map(s => s.trim()).filter(Boolean),
      exactMatch: $(`#${prefix}Exact`)?.checked || false,
      dryRun: true
    };

    try {
      showModal('Preview', '<div class="loading-state"><div class="spinner"></div><p>Scanning repository...</p></div>');

      const data = await api('POST', '/plan', body);

      const filesHtml = data.files?.map(f => `
        <div class="analysis-file">
          <span class="analysis-file-name">${escapeHtml(f.path)}</span>
          <span class="analysis-file-size">${formatBytes(f.size)}</span>
        </div>
      `).join('') || '<p>No files found</p>';

      setModalContent(`
        <p style="margin-bottom: 16px; color: var(--color-text-secondary);">
          ${data.totalFiles} files, ${formatBytes(data.totalSize)} total
        </p>
        <div class="analysis-files" style="max-height: 400px;">
          ${filesHtml}
        </div>
      `);
    } catch (e) {
      setModalContent(`<p style="color: var(--color-error);">${escapeHtml(e.message)}</p>`);
    }
  }

  // =========================================
  // Jobs Page
  // =========================================

  async function loadJobs() {
    try {
      const data = await api('GET', '/jobs');
      state.jobs.clear();
      (data.jobs || []).forEach(job => {
        state.jobs.set(job.id, job);
      });
      renderJobs();
      updateJobsBadge();
    } catch (e) {
      console.error('Failed to load jobs:', e);
    }
  }

  // Per-job DOM element cache. renderJobs() used to rebuild innerHTML on
  // every WebSocket progress event, which destroyed every element under
  // the cursor ~4 times per second — hover states flickered, and buttons
  // lost pointerdown/pointerup continuity mid-click. Now we keep stable
  // elements keyed by job ID and only mutate the fields that actually
  // changed. Buttons are only re-rendered when the status category
  // transitions (e.g. running→paused), not on every progress tick.
  const jobCardCache = new Map();

  // statusCategory groups statuses that share the same action buttons so
  // we only swap the buttons when the category changes (not on every
  // progress tick). Each status with a distinct button set gets its own
  // category: queued shows Cancel; running adds Pause; paused has
  // Resume+Cancel; terminal states show Dismiss. queued and running must
  // stay separate, otherwise the Pause button never appears when a job
  // transitions queued -> running.
  function statusCategory(status) {
    if (status === 'running') return 'running';
    if (status === 'queued') return 'queued';
    if (status === 'paused') return 'paused';
    if (status === 'completed' || status === 'failed' || status === 'cancelled') return 'done';
    return status || 'unknown';
  }

  function actionButtonsHTML(job) {
    const id = escapeHtml(job.id);
    const status = job.status || 'queued';
    if (status === 'running') {
      return `
          <button class="btn btn-sm btn-warning" onclick="pauseJob('${id}')">Pause</button>
          <button class="btn btn-sm btn-danger" onclick="cancelJob('${id}')">Cancel</button>
      `;
    }
    if (status === 'paused') {
      return `
          <button class="btn btn-sm btn-primary" onclick="resumeJob('${id}')">Resume</button>
          <button class="btn btn-sm btn-danger" onclick="cancelJob('${id}')">Cancel</button>
      `;
    }
    if (status === 'queued') {
      return `<button class="btn btn-sm btn-danger" onclick="cancelJob('${id}')">Cancel</button>`;
    }
    // failed / cancelled jobs can be restarted.
    if (status === 'failed' || status === 'cancelled') {
      return `
          <button class="btn btn-sm btn-primary" onclick="retryJob('${id}')">Retry</button>
          <button class="btn btn-sm btn-secondary" onclick="dismissJob('${id}')">Dismiss</button>
      `;
    }
    // completed
    return `<button class="btn btn-sm btn-secondary" onclick="dismissJob('${id}')">Dismiss</button>`;
  }

  function getJobQuantLabel(job) {
    const filters = Array.isArray(job.filters) ? job.filters : [];
    const quant = filters.find(f => !/mmproj/i.test(f)) || '';
    const mmproj = filters.find(f => /mmproj/i.test(f)) || '';
    return {
      quant: quant || (filters.length ? filters.join(', ') : ''),
      mmproj
    };
  }

  // formatDuration renders an ETA the way browser download managers do:
  // the longer the estimate, the coarser the unit. Second-level precision is
  // only meaningful (and only shown) under two minutes — beyond that it just
  // exposes estimator noise as flicker.
  function formatDuration(seconds) {
    if (!Number.isFinite(seconds) || seconds <= 0) return 'ETA --';
    const s = Math.max(1, Math.round(seconds));
    const h = Math.floor(s / 3600);
    const m = Math.floor((s % 3600) / 60);
    if (h > 0) return `ETA ${h}h ${m}m`;
    if (m >= 2) return `ETA ${m}m`;
    const sec = Math.max(5, Math.round(s / 5) * 5);
    if (sec >= 60) {
      const mm = Math.floor(sec / 60);
      const ss = sec % 60;
      return ss > 0 ? `ETA ${mm}m ${ss}s` : `ETA ${mm}m`;
    }
    return `ETA ${sec}s`;
  }

  function activeJobFile(job) {
    const files = Array.isArray(job.files) ? job.files : [];
    return files.find(f => f.status === 'active')
        || files.find(f => f.status === 'finalizing')
        || files.find(f => f.status === 'pending')
        || files[files.length - 1]
        || null;
  }

  function createJobCard(job) {
    const el = document.createElement('div');
    el.className = 'job-card';
    el.dataset.jobId = job.id;
    el.innerHTML = `
        <div class="job-header">
          <div>
            <div class="job-repo"></div>
            <div class="job-meta-line">
              <span data-role="revision"></span>
              <span data-role="quant" class="job-chip" style="display:none"></span>
              <span data-role="mmproj" class="job-chip job-chip-vision" style="display:none">mmproj</span>
            </div>
          </div>
          <div class="job-header-right">
            <span class="job-status" data-role="status"></span>
            <span class="job-actions" data-role="actions"></span>
          </div>
        </div>
        <div class="job-progress">
          <div class="progress-bar">
            <div class="progress-fill" data-role="fill" style="width: 0%"></div>
          </div>
        </div>
        <div class="job-stats">
          <span data-role="pct">0.0%</span>
          <span data-role="speed" style="display: none"></span>
          <span data-role="eta" style="display: none"></span>
          <span data-role="bytes"></span>
          <span data-role="files"></span>
        </div>
        <div class="job-file-line" data-role="active-file" style="display:none"></div>
        <div class="job-error" data-role="error" style="display: none"></div>
    `;
    // Static fields (set once, never changed by progress events).
    const repoEl = el.querySelector('.job-repo');
    repoEl.textContent = job.repo || '';
    if (job.repo) {
      repoEl.title = 'View model details';
      repoEl.classList.add('job-repo-clickable');
      repoEl.onclick = () => navigateToAnalyze(job.repo);
    }
    // Only show the revision when it isn't the default "main" branch —
    // every download is from main, so labelling it adds noise and height.
    const revEl = el.querySelector('[data-role="revision"]');
    if (job.revision && job.revision !== 'main') {
      revEl.textContent = '@ ' + job.revision;
    } else {
      revEl.textContent = '';
      revEl.style.display = 'none';
    }
    return el;
  }

  // updateJobCard mutates an existing card in place. It reads textContent
  // before writing so we avoid forcing the browser to recompute styles for
  // values that didn't actually change — keeps the DOM stable for hover.
  function updateJobCard(el, job) {
    const p = job.progress || {};
    const totalBytes = p.totalBytes || 0;
    const downloadedBytes = p.downloadedBytes || 0;
    const pct = totalBytes > 0 ? (downloadedBytes / totalBytes * 100) : 0;
    const speed = p.bytesPerSecond || 0;
    const status = job.status || 'queued';
    const jobLabels = getJobQuantLabel(job);

    // Status badge. While running, a "finalizing" phase (post-download
    // friendly-view/manifest work) is surfaced so the job doesn't look stuck
    // at 100%.
    const statusEl = el.querySelector('[data-role="status"]');
    const displayStatus = (status === 'running' && job.phase === 'finalizing') ? 'finalizing' : status;
    if (statusEl.textContent !== displayStatus) {
      statusEl.textContent = displayStatus;
      statusEl.className = 'job-status ' + displayStatus;
    }

    // Action buttons — only swap when the category changes, to avoid
    // killing the button the user is about to click or is currently
    // hovering. Within a single status category the DOM stays identical.
    const newCat = statusCategory(status);
    if (el.dataset.statusCategory !== newCat) {
      el.dataset.statusCategory = newCat;
      el.querySelector('[data-role="actions"]').innerHTML = actionButtonsHTML(job);
    }

    // Progress bar.
    const fillEl = el.querySelector('[data-role="fill"]');
    const nextWidth = pct + '%';
    if (fillEl.style.width !== nextWidth) {
      fillEl.style.width = nextWidth;
    }

    // Stats.
    const pctText = pct.toFixed(1) + '%';
    const pctEl = el.querySelector('[data-role="pct"]');
    if (pctEl.textContent !== pctText) pctEl.textContent = pctText;

    const speedEl = el.querySelector('[data-role="speed"]');
    if (speed > 0) {
      const speedText = formatBytes(speed) + '/s';
      if (speedEl.textContent !== speedText) speedEl.textContent = speedText;
      if (speedEl.style.display === 'none') speedEl.style.display = '';
    } else {
      if (speedEl.style.display !== 'none') speedEl.style.display = 'none';
    }

    const etaEl = el.querySelector('[data-role="eta"]');
    const eta = p.etaSeconds || 0;
    if (eta > 0 && totalBytes > downloadedBytes) {
      // Progress frames arrive ~4x per second; rewriting the ETA on every
      // frame makes the trailing seconds flicker. Refresh it at most once a
      // second — well within how fast a remaining-time estimate can
      // meaningfully change.
      const now = Date.now();
      const lastEta = Number(etaEl.dataset.lastUpdate || 0);
      if (etaEl.style.display === 'none' || now - lastEta >= 1000) {
        const etaText = formatDuration(eta);
        if (etaEl.textContent !== etaText) etaEl.textContent = etaText;
        etaEl.dataset.lastUpdate = String(now);
      }
      if (etaEl.style.display === 'none') etaEl.style.display = '';
    } else if (etaEl.style.display !== 'none') {
      etaEl.style.display = 'none';
    }

    const bytesText = formatBytes(downloadedBytes) + ' / ' + formatBytes(totalBytes);
    const bytesEl = el.querySelector('[data-role="bytes"]');
    if (bytesEl.textContent !== bytesText) bytesEl.textContent = bytesText;

    const filesText = 'completed: ' + (p.completedFiles || 0) + ' / ' + (p.totalFiles || 0) + ' files';
    const filesEl = el.querySelector('[data-role="files"]');
    if (filesEl.textContent !== filesText) filesEl.textContent = filesText;

    const quantEl = el.querySelector('[data-role="quant"]');
    if (jobLabels.quant) {
      if (quantEl.textContent !== jobLabels.quant) quantEl.textContent = jobLabels.quant;
      if (quantEl.style.display === 'none') quantEl.style.display = '';
    } else if (quantEl.style.display !== 'none') {
      quantEl.style.display = 'none';
    }

    const mmprojEl = el.querySelector('[data-role="mmproj"]');
    if (jobLabels.mmproj) {
      mmprojEl.title = jobLabels.mmproj;
      if (mmprojEl.style.display === 'none') mmprojEl.style.display = '';
    } else if (mmprojEl.style.display !== 'none') {
      mmprojEl.style.display = 'none';
    }

    // Collapse the meta line entirely when it has nothing to show (no
    // non-default revision, no quant, no mmproj) so it adds no height.
    const revVisible = el.querySelector('[data-role="revision"]').style.display !== 'none';
    const metaVisible = revVisible || !!jobLabels.quant || !!jobLabels.mmproj;
    const metaLine = el.querySelector('.job-meta-line');
    metaLine.style.display = metaVisible ? '' : 'none';

    const currentFile = activeJobFile(job);
    const fileEl = el.querySelector('[data-role="active-file"]');
    if (currentFile?.path) {
      const fileText = `${currentFile.status || 'file'}: ${currentFile.path}`;
      if (fileEl.textContent !== fileText) fileEl.textContent = fileText;
      fileEl.title = currentFile.path;
      if (fileEl.style.display === 'none') fileEl.style.display = '';
    } else if (fileEl.style.display !== 'none') {
      fileEl.style.display = 'none';
      fileEl.textContent = '';
      fileEl.title = '';
    }

    // Error.
    const errorEl = el.querySelector('[data-role="error"]');
    if (job.error) {
      if (errorEl.textContent !== job.error) errorEl.textContent = job.error;
      if (errorEl.style.display === 'none') errorEl.style.display = '';
    } else if (errorEl.style.display !== 'none') {
      errorEl.style.display = 'none';
      errorEl.textContent = '';
    }
  }

  function renderJobs() {
    const container = $('#jobsList');
    if (!container) return;

    // Sort by date added, newest first. Fall back to id ordering when
    // createdAt is missing or equal so the order stays stable.
    const jobs = Array.from(state.jobs.values()).sort((a, b) => {
      const ta = Date.parse(a.createdAt) || 0;
      const tb = Date.parse(b.createdAt) || 0;
      if (tb !== ta) return tb - ta;
      return String(b.id).localeCompare(String(a.id));
    });

    if (jobs.length === 0) {
      container.innerHTML = `
        <div class="empty-state">
          <div class="empty-icon">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" width="64" height="64">
              <polyline points="22 12 18 12 15 21 9 3 6 12 2 12"/>
            </svg>
          </div>
          <h3>No Active Downloads</h3>
          <p>Start a download from the Download page to see progress here.</p>
        </div>
      `;
      jobCardCache.clear();
      return;
    }

    // If we're transitioning from the empty-state template back into
    // the jobs view, clear the container and cache first.
    if (container.querySelector('.empty-state')) {
      container.innerHTML = '';
      jobCardCache.clear();
    }

    const seen = new Set();
    let prev = null;
    for (const job of jobs) {
      seen.add(job.id);
      let el = jobCardCache.get(job.id);
      if (!el) {
        el = createJobCard(job);
        jobCardCache.set(job.id, el);
      }
      updateJobCard(el, job);
      // Keep the DOM in sorted order: place this card right after the
      // previous one, but only touch the DOM when it is out of position.
      const expectedNext = prev ? prev.nextSibling : container.firstChild;
      if (el !== expectedNext) {
        container.insertBefore(el, expectedNext);
      }
      prev = el;
    }

    // Remove stale cards for jobs that are no longer tracked (dismiss).
    for (const [id, el] of jobCardCache) {
      if (!seen.has(id)) {
        el.remove();
        jobCardCache.delete(id);
      }
    }
  }

  // Cancel a running/queued job
  window.cancelJob = async function(jobId) {
    try {
      await api('DELETE', `/jobs/${jobId}`);
      showToast('Download cancelled', 'success');
      // Update local state immediately
      const job = state.jobs.get(jobId);
      if (job) {
        job.status = 'cancelled';
        state.jobs.set(jobId, job);
        renderJobs();
        updateJobsBadge();
      }
    } catch (e) {
      showToast(`Failed to cancel: ${e.message}`, 'error');
    }
  };

  // Pause a running job
  window.pauseJob = async function(jobId) {
    try {
      await api('POST', `/jobs/${jobId}/pause`);
      showToast('Download paused', 'success');
      const job = state.jobs.get(jobId);
      if (job) {
        job.status = 'paused';
        state.jobs.set(jobId, job);
        renderJobs();
        updateJobsBadge();
      }
    } catch (e) {
      showToast(`Failed to pause: ${e.message}`, 'error');
    }
  };

  // Resume a paused job
  window.resumeJob = async function(jobId) {
    try {
      await api('POST', `/jobs/${jobId}/resume`);
      showToast('Download resumed', 'success');
      const job = state.jobs.get(jobId);
      if (job) {
        job.status = 'queued';
        state.jobs.set(jobId, job);
        renderJobs();
        updateJobsBadge();
      }
    } catch (e) {
      showToast(`Failed to resume: ${e.message}`, 'error');
    }
  };

  // Retry a failed or cancelled job. Server reuses the same job ID and
  // restarts the download with the original parameters.
  window.retryJob = async function(jobId) {
    try {
      await api('POST', `/jobs/${jobId}/retry`);
      showToast('Download restarted', 'success');
      const job = state.jobs.get(jobId);
      if (job) {
        job.status = 'queued';
        job.error = '';
        state.jobs.set(jobId, job);
        renderJobs();
        updateJobsBadge();
      }
    } catch (e) {
      showToast(`Failed to retry: ${e.message}`, 'error');
    }
  };

  // Dismiss (permanently remove) a completed/failed/cancelled/paused job.
  // We call the server so the dismissal survives a page refresh (github #68).
  window.dismissJob = async function(jobId) {
    try {
      await api('POST', `/jobs/${jobId}/dismiss`);
      state.jobs.delete(jobId);
      renderJobs();
      updateJobsBadge();
    } catch (e) {
      showToast(`Failed to dismiss: ${e.message}`, 'error');
    }
  };

  // =========================================
  // Cache Page
  // =========================================

  let cacheData = { repos: [], stats: {}, cacheDir: '' };
  let cacheFilter = 'all';
  let cacheSort = 'name';
  let cacheView = 'list';
  let cacheSearch = '';

  async function loadCache() {
    const container = $('#cacheList');
    const statsContainer = $('#cacheStats');
    if (!container) return;

    if (isFilePreview()) {
      cacheData = { repos: [], stats: {}, cacheDir: '' };
      updateCacheStats();
      renderCacheList();
      return;
    }

    container.innerHTML = `
      <div class="loading-state">
        <div class="spinner"></div>
        <p>Loading cache...</p>
      </div>
    `;

    try {
      cacheData = await api('GET', '/cache');
      updateCacheStats();
      renderCacheList();
    } catch (e) {
      container.innerHTML = `
        <div class="empty-state">
          <h3>Failed to Load Cache</h3>
          <p>${escapeHtml(e.message)}</p>
        </div>
      `;
    }
  }

  function updateCacheStats() {
    const stats = cacheData.stats || {};
    $('#statModels').textContent = stats.totalModels || 0;
    $('#statDatasets').textContent = stats.totalDatasets || 0;
    $('#statSize').textContent = stats.totalSizeHuman || '0 B';
    $('#statFiles').textContent = stats.totalFiles || 0;
  }

  function capabilityIcon(kind, title) {
    const icons = {
      vision: `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="13" height="13"><path d="M2 12s3.5-6 10-6 10 6 10 6-3.5 6-10 6S2 12 2 12z"/><circle cx="12" cy="12" r="3"/></svg>`,
      tools: `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="13" height="13"><path d="M14.7 6.3a4 4 0 0 0-5.1 5.1l-6.1 6.1a2.1 2.1 0 0 0 3 3l6.1-6.1a4 4 0 0 0 5.1-5.1l-2.5 2.5-2.8-2.8 2.3-2.7z"/></svg>`,
      reasoning: `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="13" height="13"><path d="M12 3a5 5 0 0 0-5 5v1.5L5.5 11H7v3a3 3 0 0 0 3 3h1v3h5v-4.2A6 6 0 0 0 12 3z"/><path d="M10 8h.01M14 8h.01M11 12h3"/></svg>`
    };
    const labels = { vision: 'Vision', tools: 'Tools', reasoning: 'Reasoning' };
    return `<span class="capability-icon capability-${kind}" title="${escapeHtml(title || labels[kind] || kind)}" aria-label="${escapeHtml(labels[kind] || kind)}">${icons[kind] || ''}</span>`;
  }

  function renderCapabilityIcons(capabilities, titles = {}) {
    const ordered = ['vision', 'tools', 'reasoning'];
    const set = new Set(capabilities || []);
    const html = ordered
      .filter(kind => set.has(kind))
      .map(kind => capabilityIcon(kind, titles[kind]))
      .join('');
    return html ? `<span class="capability-icons">${html}</span>` : '';
  }

  function renderCacheList() {
    const container = $('#cacheList');
    if (!container) return;

    let repos = [...(cacheData.repos || [])];

    // Apply filter
    if (cacheFilter !== 'all') {
      repos = repos.filter(r => r.type === cacheFilter);
    }

    // Apply search
    if (cacheSearch) {
      const search = cacheSearch.toLowerCase();
      repos = repos.filter(r =>
        r.repo.toLowerCase().includes(search) ||
        r.owner.toLowerCase().includes(search) ||
        r.name.toLowerCase().includes(search)
      );
    }

    // Apply sort
    repos.sort((a, b) => {
      switch (cacheSort) {
        case 'size':
          return b.size - a.size;
        case 'date':
          return (b.downloaded || '').localeCompare(a.downloaded || '');
        default:
          return a.repo.localeCompare(b.repo);
      }
    });

    if (repos.length === 0) {
      const message = cacheData.repos?.length === 0
        ? 'No models or datasets downloaded yet.'
        : 'No repositories match your filters.';
      container.innerHTML = `
        <div class="empty-state">
          <div class="empty-icon">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" width="64" height="64">
              <path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z"/>
            </svg>
          </div>
          <h3>${cacheData.repos?.length === 0 ? 'Cache is Empty' : 'No Results'}</h3>
          <p>${message}</p>
          ${cacheData.cacheDir ? `<p class="cache-dir-hint">Cache: ${escapeHtml(cacheData.cacheDir)}</p>` : ''}
        </div>
      `;
      return;
    }

    // Render based on view mode
    container.className = `cache-list cache-${cacheView}-view`;

    if (cacheView === 'grid') {
      container.innerHTML = repos.map(repo => renderCacheCard(repo)).join('');
    } else {
      container.innerHTML = `
        <div class="cache-table">
          <div class="cache-table-header">
            <div class="cache-col-type">Type</div>
            <div class="cache-col-repo">Repository</div>
            <div class="cache-col-source">Source</div>
            <div class="cache-col-size">Size</div>
            <div class="cache-col-files">Files</div>
            <div class="cache-col-date">Downloaded</div>
            <div class="cache-col-actions"></div>
          </div>
          ${repos.map(repo => renderCacheRow(repo)).join('')}
        </div>
      `;
    }
  }

  function renderCacheCard(repo) {
    const typeIcon = repo.type === 'model'
      ? `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="16" height="16">
           <path d="M21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16z"/>
         </svg>`
      : `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="16" height="16">
           <path d="M4 19.5A2.5 2.5 0 0 1 6.5 17H20"/>
           <path d="M6.5 2H20v20H6.5A2.5 2.5 0 0 1 4 19.5v-15A2.5 2.5 0 0 1 6.5 2z"/>
         </svg>`;
    const sourceBadge = repo.source
      ? `<span class="cache-badge cache-source-badge">${escapeHtml(repo.source)}</span>`
      : '';
    // Merge server-provided capabilities (e.g. vision from an mmproj encoder)
    // with ones detected from the repo name/tags so tools/reasoning badges
    // also show for cached models.
    const detectedCaps = detectSearchCapabilities({ id: repo.repo, tags: repo.tags });
    const mergedCaps = Array.from(new Set([...(repo.capabilities || []), ...detectedCaps.capabilities]));
    const capabilityIcons = renderCapabilityIcons(mergedCaps, {
      vision: repo.hasMMProj ? 'Vision: mmproj encoder found' : detectedCaps.titles.vision,
      tools: detectedCaps.titles.tools,
      reasoning: detectedCaps.titles.reasoning
    });

    // Build status badge based on download status
    let statusBadge = '';
    if (repo.downloadStatus === 'complete') {
      statusBadge = '<span class="cache-badge cache-badge-complete" title="Fully downloaded with HFDesk">Complete</span>';
    } else if (repo.downloadStatus === 'filtered') {
      const filterTitle = repo.manifest?.filters ? `Filtered download: ${repo.manifest.filters}` : 'Partial download (filters applied)';
      statusBadge = `<span class="cache-badge cache-badge-filtered" title="${escapeHtml(filterTitle)}">Filtered</span>`;
    } else if (repo.manifest) {
      statusBadge = '<span class="cache-badge cache-badge-manifest" title="Has manifest file">Tracked</span>';
    }
    const quantSubtitle = renderCacheQuantSubtitle(repo);

    return `
      <div class="cache-card" onclick="showCacheDetails('${escapeHtml(repo.repo)}', '${escapeHtml(repo.type)}')">
        <div class="cache-card-header">
          <span class="cache-card-type cache-type-${repo.type}">
            ${typeIcon}
            ${repo.type}
          </span>
          <span class="cache-card-badges">${sourceBadge}${statusBadge}</span>
        </div>
        <div class="cache-card-body">
          <div class="cache-card-owner">${escapeHtml(repo.owner)}</div>
          <div class="cache-card-name">${escapeHtml(repo.name)}</div>
          ${capabilityIcons}
          ${quantSubtitle}
        </div>
        <div class="cache-card-meta">
          <div class="cache-card-stat">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="14" height="14">
              <path d="M21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16z"/>
              <polyline points="3.27 6.96 12 12.01 20.73 6.96"/>
              <line x1="12" y1="22.08" x2="12" y2="12"/>
            </svg>
            ${escapeHtml(repo.sizeHuman)}
          </div>
          <div class="cache-card-stat">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="14" height="14">
              <path d="M13 2H6a2 2 0 0 0-2 2v16a2 2 0 0 0 2 2h12a2 2 0 0 0 2-2V9z"/>
              <polyline points="13 2 13 9 20 9"/>
            </svg>
            ${repo.fileCount} files
          </div>
        </div>
        <div class="cache-card-footer">
          ${repo.commit ? `<code class="cache-commit" title="Commit: ${escapeHtml(repo.commit)}">${escapeHtml(repo.commit)}</code>` : ''}
          ${repo.downloaded ? `<span class="cache-date">${escapeHtml(repo.downloaded)}</span>` : ''}
        </div>
      </div>
    `;
  }

  function renderCacheRow(repo) {
    const typeIcon = repo.type === 'model'
      ? `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="14" height="14">
           <path d="M21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16z"/>
         </svg>`
      : `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="14" height="14">
           <path d="M4 19.5A2.5 2.5 0 0 1 6.5 17H20"/>
           <path d="M6.5 2H20v20H6.5A2.5 2.5 0 0 1 4 19.5v-15A2.5 2.5 0 0 1 6.5 2z"/>
         </svg>`;
    const sourceBadge = repo.source
      ? `<span class="cache-badge cache-source-badge">${escapeHtml(repo.source)}</span>`
      : '';
    // Merge server-provided capabilities (e.g. vision from an mmproj encoder)
    // with ones detected from the repo name/tags so tools/reasoning badges
    // also show for cached models.
    const detectedCaps = detectSearchCapabilities({ id: repo.repo, tags: repo.tags });
    const mergedCaps = Array.from(new Set([...(repo.capabilities || []), ...detectedCaps.capabilities]));
    const capabilityIcons = renderCapabilityIcons(mergedCaps, {
      vision: repo.hasMMProj ? 'Vision: mmproj encoder found' : detectedCaps.titles.vision,
      tools: detectedCaps.titles.tools,
      reasoning: detectedCaps.titles.reasoning
    });

    // Build status badge
    let statusBadge = '';
    if (repo.downloadStatus === 'complete') {
      statusBadge = '<span class="cache-badge cache-badge-complete">Complete</span>';
    } else if (repo.downloadStatus === 'filtered') {
      statusBadge = '<span class="cache-badge cache-badge-filtered">Filtered</span>';
    } else if (repo.manifest) {
      statusBadge = '<span class="cache-badge cache-badge-manifest">Tracked</span>';
    }
    const quantSubtitle = renderCacheQuantSubtitle(repo);

    return `
      <div class="cache-table-row" onclick="showCacheDetails('${escapeHtml(repo.repo)}', '${escapeHtml(repo.type)}')">
        <div class="cache-col-type">
          <span class="cache-type-badge cache-type-${repo.type}">
            ${typeIcon}
            ${repo.type}
          </span>
        </div>
        <div class="cache-col-repo">
          <span class="cache-repo-name">${escapeHtml(repo.repo)}</span>
          ${capabilityIcons}
          ${quantSubtitle}
          ${statusBadge}
        </div>
        <div class="cache-col-source">${sourceBadge}</div>
        <div class="cache-col-size">${escapeHtml(repo.sizeHuman)}</div>
        <div class="cache-col-files">${repo.fileCount}</div>
        <div class="cache-col-date">${escapeHtml(repo.downloaded || '-')}</div>
        <div class="cache-col-actions">
          <button class="btn btn-ghost btn-sm" onclick="event.stopPropagation(); showCacheDetails('${escapeHtml(repo.repo)}', '${escapeHtml(repo.type)}')" title="View details">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="16" height="16">
              <circle cx="12" cy="12" r="10"/><line x1="12" y1="16" x2="12" y2="12"/><line x1="12" y1="8" x2="12.01" y2="8"/>
            </svg>
          </button>
        </div>
      </div>
    `;
  }

  function renderCacheQuantSubtitle(repo) {
    const quants = repo.quantizations || [];
    if (!quants.length) return '';
    const shown = quants.slice(0, 8).join(', ');
    const more = quants.length > 8 ? ` +${quants.length - 8}` : '';
    return `<div class="cache-quant-subtitle" title="${escapeHtml(quants.join(', '))}">${escapeHtml(shown + more)}</div>`;
  }

  window.showCacheDetails = async function(repo, type) {
    try {
      showModal('Repository Details', '<div class="loading-state"><div class="spinner"></div></div>');
      const data = await api('GET', `/cache/${repo}`);

      const typeIcon = data.type === 'model'
        ? `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="20" height="20">
             <path d="M21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16z"/>
           </svg>`
        : `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="20" height="20">
             <path d="M4 19.5A2.5 2.5 0 0 1 6.5 17H20"/>
             <path d="M6.5 2H20v20H6.5A2.5 2.5 0 0 1 4 19.5v-15A2.5 2.5 0 0 1 6.5 2z"/>
           </svg>`;

      // Build files table
      const filesHtml = data.files?.length > 0
        ? `<div class="cache-detail-files">
             <h4>Files (${data.files.length})</h4>
             <div class="cache-files-list">
               ${data.files.slice(0, 20).map(f => `
                 <div class="cache-file-row">
                   <span class="cache-file-name" title="${escapeHtml(f.name)}">
                     ${f.isLfs ? '<span class="lfs-badge">LFS</span>' : ''}
                     ${escapeHtml(f.name)}
                   </span>
                   <span class="cache-file-size">${escapeHtml(f.sizeHuman)}</span>
                 </div>
               `).join('')}
               ${data.files.length > 20 ? `<div class="cache-files-more">... and ${data.files.length - 20} more files</div>` : ''}
             </div>
           </div>`
        : '';

      // Build download status badge for detail view
      let statusBadgeHtml = '';
      if (data.downloadStatus === 'complete') {
        statusBadgeHtml = '<span class="cache-badge cache-badge-complete">Complete</span>';
      } else if (data.downloadStatus === 'filtered') {
        statusBadgeHtml = '<span class="cache-badge cache-badge-filtered">Filtered</span>';
      } else if (data.downloadStatus === 'unknown') {
        statusBadgeHtml = '<span class="cache-badge cache-badge-unknown">External</span>';
      }
      const detailSourceBadge = data.source
        ? `<span class="cache-badge cache-source-badge">${escapeHtml(data.source)}</span>`
        : '';
      const detailCapabilities = renderCapabilityIcons(data.capabilities, {
        vision: data.hasMMProj ? 'Vision: mmproj encoder found' : 'Vision'
      });

      // Build manifest info
      const manifestHtml = data.manifest
        ? `<div class="cache-detail-section">
             <h4>Download Info</h4>
             <div class="cache-detail-grid">
               <div class="cache-detail-item">
                 <span class="cache-detail-label">Status</span>
                 <span class="cache-detail-value">
                   ${data.downloadStatus === 'complete' ? '✓ Complete download' : ''}
                   ${data.downloadStatus === 'filtered' ? '◐ Filtered download' : ''}
                   ${data.manifest.filters ? ` (${escapeHtml(data.manifest.filters)})` : ''}
                 </span>
               </div>
               <div class="cache-detail-item">
                 <span class="cache-detail-label">Downloaded</span>
                 <span class="cache-detail-value">${escapeHtml(data.manifest.downloaded)}</span>
               </div>
               ${data.manifest.command ? `
                 <div class="cache-detail-item cache-detail-full">
                   <span class="cache-detail-label">Command</span>
                   <code class="cache-detail-code">${escapeHtml(data.manifest.command)}</code>
                 </div>
               ` : ''}
             </div>
           </div>`
        : (data.downloadStatus === 'unknown' ? `<div class="cache-detail-section">
             <h4>Download Info</h4>
             <div class="cache-detail-note">
               <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="16" height="16">
                 <circle cx="12" cy="12" r="10"/><line x1="12" y1="16" x2="12" y2="12"/><line x1="12" y1="8" x2="12.01" y2="8"/>
               </svg>
               <span>Downloaded by external tool or found in a local model folder.</span>
             </div>
           </div>` : '');

      const mmprojHtml = data.mmprojFiles?.length
        ? `<div class="cache-detail-section">
             <h4>Vision Encoder</h4>
             <div class="cache-files-list">
               ${data.mmprojFiles.map(f => `
                 <div class="cache-file-row">
                   <span class="cache-file-name" title="${escapeHtml(f)}">${escapeHtml(f)}</span>
                 </div>
               `).join('')}
             </div>
           </div>`
        : '';

      setModalContent(`
        <div class="cache-detail-modal">
          <div class="cache-detail-header">
            <span class="cache-detail-type cache-type-${data.type}">
              ${typeIcon}
              ${data.type}
            </span>
            ${statusBadgeHtml}
            ${detailSourceBadge}
            ${detailCapabilities}
            <div class="cache-detail-repo">
              <span class="cache-detail-owner">${escapeHtml(data.owner)}/</span>
              <span class="cache-detail-name">${escapeHtml(data.name)}</span>
            </div>
          </div>

          <div class="cache-detail-stats">
            <div class="cache-detail-stat">
              <div class="cache-detail-stat-value">${escapeHtml(data.sizeHuman)}</div>
              <div class="cache-detail-stat-label">Total Size</div>
            </div>
            <div class="cache-detail-stat">
              <div class="cache-detail-stat-value">${data.fileCount}</div>
              <div class="cache-detail-stat-label">Files</div>
            </div>
            <div class="cache-detail-stat">
              <div class="cache-detail-stat-value">${escapeHtml(data.branch || 'main')}</div>
              <div class="cache-detail-stat-label">Branch</div>
            </div>
            ${data.commit ? `
              <div class="cache-detail-stat">
                <div class="cache-detail-stat-value"><code>${escapeHtml(data.commit)}</code></div>
                <div class="cache-detail-stat-label">Commit</div>
              </div>
            ` : ''}
          </div>

          ${manifestHtml}
          ${mmprojHtml}

          <div class="cache-detail-section">
            <h4>Paths</h4>
            <div class="cache-detail-paths">
              <div class="cache-path-item">
                <span class="cache-path-label">${data.source === 'HF cache' || !data.source ? 'Cache (HF format)' : 'Local path'}</span>
                <code class="cache-path-value">${escapeHtml(data.path)}</code>
              </div>
              ${data.friendlyPath ? `
                <div class="cache-path-item">
                  <span class="cache-path-label">Friendly view</span>
                  <code class="cache-path-value">${escapeHtml(data.friendlyPath)}</code>
                </div>
              ` : ''}
            </div>
          </div>

          ${filesHtml}

          <div class="cache-detail-actions">
            ${data.source === 'HF cache' || !data.source ? `<button class="btn btn-danger" onclick="confirmDeleteCache('${escapeHtml(data.repo)}', '${escapeHtml(data.type)}')">
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="16" height="16">
                <polyline points="3 6 5 6 21 6"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/>
                <line x1="10" y1="11" x2="10" y2="17"/><line x1="14" y1="11" x2="14" y2="17"/>
              </svg>
              Delete
            </button>` : ''}
            <a href="https://huggingface.co/${data.type === 'dataset' ? 'datasets/' : ''}${data.repo}" target="_blank" class="btn btn-secondary">
              <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="16" height="16">
                <path d="M18 13v6a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V8a2 2 0 0 1 2-2h6"/>
                <polyline points="15 3 21 3 21 9"/><line x1="10" y1="14" x2="21" y2="3"/>
              </svg>
              View on HuggingFace
            </a>
            <button class="btn btn-ghost" onclick="hideModal()">Close</button>
          </div>
        </div>
      `);
    } catch (e) {
      setModalContent(`<p style="color: var(--color-error);">${escapeHtml(e.message)}</p>`);
    }
  };

  // Rebuild cache (regenerate friendly view symlinks)
  async function rebuildCache() {
    const btn = $('#rebuildCacheBtn');
    if (btn) {
      btn.disabled = true;
      btn.innerHTML = `
        <div class="spinner" style="width: 18px; height: 18px;"></div>
        Rebuilding...
      `;
    }

    try {
      const result = await api('POST', '/cache/rebuild', { clean: true });

      // Show result modal
      showModal('Rebuild Complete', `
        <div class="rebuild-result">
          <div class="rebuild-success">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="48" height="48">
              <path d="M22 11.08V12a10 10 0 1 1-5.93-9.14"/>
              <polyline points="22 4 12 14.01 9 11.01"/>
            </svg>
          </div>
          <p class="rebuild-message">${escapeHtml(result.message)}</p>
          <div class="rebuild-stats">
            <div class="rebuild-stat">
              <span class="rebuild-stat-value">${result.reposScanned}</span>
              <span class="rebuild-stat-label">Repos Scanned</span>
            </div>
            <div class="rebuild-stat">
              <span class="rebuild-stat-value">${result.symlinksCreated}</span>
              <span class="rebuild-stat-label">Links Created</span>
            </div>
            <div class="rebuild-stat">
              <span class="rebuild-stat-value">${result.symlinksUpdated}</span>
              <span class="rebuild-stat-label">Links Updated</span>
            </div>
            <div class="rebuild-stat">
              <span class="rebuild-stat-value">${result.orphansRemoved || 0}</span>
              <span class="rebuild-stat-label">Orphans Removed</span>
            </div>
          </div>
          ${result.errors?.length > 0 ? `
            <div class="rebuild-errors">
              <h5>Errors:</h5>
              <ul>${result.errors.map(e => `<li>${escapeHtml(e)}</li>`).join('')}</ul>
            </div>
          ` : ''}
          <div class="form-actions" style="margin-top: 20px;">
            <button class="btn btn-primary" onclick="hideModal()">Done</button>
          </div>
        </div>
      `);

      // Refresh cache list
      loadCache();
    } catch (e) {
      showToast(`Rebuild failed: ${e.message}`, 'error');
    } finally {
      if (btn) {
        btn.disabled = false;
        btn.innerHTML = `
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="18" height="18">
            <path d="M19.73 14.87A8 8 0 1 1 12 4"/>
            <path d="M12 4V1l4 4-4 4V4"/>
          </svg>
          Rebuild
        `;
      }
    }
  }

  // Delete a cached repo with confirmation
  window.confirmDeleteCache = function(repo, type) {
    showModal('Delete from Cache', `
      <div class="delete-confirm">
        <div class="delete-warning">
          <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="48" height="48">
            <path d="M10.29 3.86L1.82 18a2 2 0 0 0 1.71 3h16.94a2 2 0 0 0 1.71-3L13.71 3.86a2 2 0 0 0-3.42 0z"/>
            <line x1="12" y1="9" x2="12" y2="13"/><line x1="12" y1="17" x2="12.01" y2="17"/>
          </svg>
        </div>
        <p class="delete-message">Are you sure you want to delete <strong>${escapeHtml(repo)}</strong> from the cache?</p>
        <p class="delete-note">This will permanently remove all cached files for this ${type}. This action cannot be undone.</p>
        <div class="form-actions" style="margin-top: 20px;">
          <button class="btn btn-ghost" onclick="hideModal()">Cancel</button>
          <button class="btn btn-danger" onclick="deleteCache('${escapeHtml(repo)}', '${escapeHtml(type)}')">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="16" height="16">
              <polyline points="3 6 5 6 21 6"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/>
            </svg>
            Delete
          </button>
        </div>
      </div>
    `);
  };

  // Actually delete the cache
  window.deleteCache = async function(repo, type) {
    try {
      await api('DELETE', `/cache/${repo}?type=${type}`);
      hideModal();
      showToast(`Deleted ${repo} from cache`, 'success');
      loadCache(); // Refresh the list
    } catch (e) {
      showToast(`Failed to delete: ${e.message}`, 'error');
    }
  };

  function initCachePage() {
    // Refresh button
    $('#refreshCacheBtn')?.addEventListener('click', loadCache);

    // Rebuild button
    $('#rebuildCacheBtn')?.addEventListener('click', rebuildCache);

    // Search
    const searchInput = $('#cacheSearch');
    if (searchInput) {
      let searchTimeout;
      searchInput.addEventListener('input', () => {
        clearTimeout(searchTimeout);
        searchTimeout = setTimeout(() => {
          cacheSearch = searchInput.value.trim();
          renderCacheList();
        }, 200);
      });
    }

    // Filter buttons
    $$('.cache-filters .filter-btn').forEach(btn => {
      btn.addEventListener('click', () => {
        $$('.cache-filters .filter-btn').forEach(b => b.classList.remove('active'));
        btn.classList.add('active');
        cacheFilter = btn.dataset.filter;
        renderCacheList();
      });
    });

    // Sort dropdown
    const sortSelect = $('#cacheSort');
    if (sortSelect) {
      sortSelect.addEventListener('change', () => {
        cacheSort = sortSelect.value;
        renderCacheList();
      });
    }

    // View toggle
    $$('.cache-view-toggle .view-btn').forEach(btn => {
      btn.addEventListener('click', () => {
        $$('.cache-view-toggle .view-btn').forEach(b => b.classList.remove('active'));
        btn.classList.add('active');
        cacheView = btn.dataset.view;
        renderCacheList();
      });
    });
  }

  // =========================================
  // Settings Page
  // =========================================

  // The Max speed control is shown in megabits per second, but the backend
  // stores a byte-rate string ("250KB"). These helpers convert between the two.
  // 1 Mbit/s = 125000 bytes/s = 125 KB (decimal KB matches the server parser).
  function parseHumanBytes(s) {
    if (!s) return 0;
    const m = String(s).trim().match(/^([\d.]+)\s*([KMGT]?i?B)?$/i);
    if (!m) return 0;
    const n = parseFloat(m[1]);
    if (!isFinite(n)) return 0;
    const u = (m[2] || 'B').toUpperCase();
    const map = { B: 1, KB: 1e3, MB: 1e6, GB: 1e9, TB: 1e12,
                  KIB: 1024, MIB: 1048576, GIB: 1073741824, TIB: 1099511627776 };
    return n * (map[u] || 1);
  }

  function maxSpeedToMbit(s) {
    const b = parseHumanBytes(s);
    if (!b) return '';
    return String(Math.round((b * 8 / 1e6) * 100) / 100); // 2 decimals
  }

  function mbitToMaxSpeedStr(mbit) {
    const v = parseFloat(mbit);
    if (!isFinite(v) || v <= 0) return ''; // empty = unlimited
    // Floor at 1 KB/s so a positive Mbit value (e.g. 0.003) never rounds down
    // to 0 KB on the wire — 0 KB is parsed by the server as "unlimited" and
    // would silently disable the cap the user just set.
    return Math.max(1, Math.round(v * 125)) + 'KB';
  }

  function syncSpeedPresets() {
    const raw = ($('#maxSpeed')?.value ?? '').toString().trim();
    const v = raw === '' ? 0 : parseFloat(raw);
    $$('#speedPresets .speed-btn').forEach(b => {
      const pv = parseFloat(b.dataset.mbit);
      b.classList.toggle('active', !isNaN(v) && pv === v);
    });
  }

  function bindSpeedPresets() {
    const presets = document.getElementById('speedPresets');
    if (!presets || presets.dataset.bound) return;
    presets.dataset.bound = '1';
    presets.querySelectorAll('.speed-btn').forEach(btn => {
      btn.addEventListener('click', () => {
        const mbit = parseFloat(btn.dataset.mbit) || 0;
        $('#maxSpeed').value = mbit > 0 ? String(mbit) : '';
        syncSpeedPresets();
      });
    });
    const field = $('#maxSpeed');
    if (field) field.addEventListener('input', syncSpeedPresets);
  }

  async function loadSettings() {
    try {
      const data = await api('GET', '/settings');
      state.settings = data;

      const cacheDirInput = $('#cacheDirInput');
      if (cacheDirInput) {
        cacheDirInput.value = data.cacheDir || '';
      }
      const localScanDirs = $('#localScanDirs');
      if (localScanDirs) {
        localScanDirs.value = (data.localScanDirs || []).join('\n');
      }
      const localDirInput = $('#localDirInput');
      if (localDirInput) {
        localDirInput.value = data.localDir || '';
      }
      const downloadLayout = $('#downloadLayout');
      if (downloadLayout) {
        downloadLayout.value = data.storageMode === 'local' ? 'local' : 'cache';
      }
      syncStorageFields();

      // Display config file paths
      const configPathEl = $('#settingsConfigPath');
      if (configPathEl && data.configFile) {
        const fileName = path => String(path || '').split(/[\\/]/).pop() || path;
        const escapeAttr = value => escapeHtml(value).replace(/"/g, '&quot;');
        configPathEl.innerHTML = `<span class="config-chip" title="${escapeAttr(data.configFile)}">Config: <code>${escapeHtml(fileName(data.configFile))}</code></span>`;
        if (data.targetsFile) {
          configPathEl.innerHTML += `<span class="config-chip" title="${escapeAttr(data.targetsFile)}">Targets: <code>${escapeHtml(fileName(data.targetsFile))}</code></span>`;
        }
      }

      $('#hfToken').value = data.token || '';
      $('#connections').value = data.connections ?? 8;
      $('#maxActive').value = data.maxActive ?? 3;
      $('#maxSpeed').value = maxSpeedToMbit(data.maxSpeed);
      bindSpeedPresets();
      syncSpeedPresets();
      $('#retries').value = data.retries ?? 4;
      $('#verify').value = data.verify || 'size';
      $('#endpoint').value = data.endpoint || '';

      // Load proxy settings
      if (data.proxy) {
        $('#proxyUrl').value = data.proxy.url || '';
        $('#proxyUsername').value = data.proxy.username || '';
        $('#proxyPassword').value = ''; // Never show saved password
        $('#proxyNoProxy').value = data.proxy.noProxy || '';
        $('#proxyNoEnvProxy').checked = data.proxy.noEnvProxy || false;
      } else {
        $('#proxyUrl').value = '';
        $('#proxyUsername').value = '';
        $('#proxyPassword').value = '';
        $('#proxyNoProxy').value = '';
        $('#proxyNoEnvProxy').checked = false;
      }
    } catch (e) {
      console.error('Failed to load settings:', e);
    }
  }

  function initSettingsPage() {
    $('#saveSettingsBtn')?.addEventListener('click', saveSettings);
    $('#resetSettingsBtn')?.addEventListener('click', resetSettings);
    $('#downloadLayout')?.addEventListener('change', syncStorageFields);

    // Toggle password visibility
    $$('.toggle-visibility').forEach(btn => {
      btn.addEventListener('click', () => {
        const target = btn.dataset.target;
        const input = $(`#${target}`);
        if (input) {
          const isPassword = input.type === 'password';
          input.type = isPassword ? 'text' : 'password';
          btn.querySelector('.icon-show').style.display = isPassword ? 'none' : 'block';
          btn.querySelector('.icon-hide').style.display = isPassword ? 'block' : 'none';
        }
      });
    });
  }

  async function saveSettings() {
    const layout = $('#downloadLayout')?.value || 'cache';
    const defaultLocalDir = $('#localDirInput')?.value?.trim() || '';
    if (layout === 'local' && !defaultLocalDir) {
      showToast('Set a default local download folder first', 'error');
      return;
    }
    const retries = parseInt($('#retries')?.value);
    const body = {
      token: $('#hfToken')?.value || '',
      cacheDir: $('#cacheDirInput')?.value?.trim() || '',
      localDir: layout === 'local' ? defaultLocalDir : '',
      localScanDirs: ($('#localScanDirs')?.value || '')
        .split(/\r?\n/)
        .map(v => v.trim())
        .filter(Boolean),
      connections: parseInt($('#connections')?.value) || 8,
      maxActive: parseInt($('#maxActive')?.value) || 3,
      maxSpeed: mbitToMaxSpeedStr($('#maxSpeed')?.value),
      retries: Number.isNaN(retries) ? 4 : retries,
      verify: $('#verify')?.value || 'size',
      endpoint: $('#endpoint')?.value || ''
    };

    // Add proxy settings if URL is provided
    const proxyUrl = $('#proxyUrl')?.value || '';
    if (proxyUrl || $('#proxyNoEnvProxy')?.checked) {
      body.proxy = {
        url: proxyUrl,
        username: $('#proxyUsername')?.value || '',
        noProxy: $('#proxyNoProxy')?.value || '',
        noEnvProxy: $('#proxyNoEnvProxy')?.checked || false
      };
      // Only send password if it was changed (not empty)
      const proxyPassword = $('#proxyPassword')?.value;
      if (proxyPassword) {
        body.proxy.password = proxyPassword;
      }
    }

    try {
      const result = await api('POST', '/settings', body);
      showToast(result.message || 'Settings saved', 'success');
      await loadSettings();
      // Clear password field after save
      if ($('#proxyPassword')) {
        $('#proxyPassword').value = '';
      }
    } catch (e) {
      showToast(`Failed: ${e.message}`, 'error');
    }
  }

  function syncStorageFields() {
    const layout = $('#downloadLayout')?.value || 'cache';
    const cacheGroup = $('#cacheDirGroup');
    const localGroup = $('#localDirGroup');
    if (cacheGroup) {
      cacheGroup.style.display = layout === 'cache' ? '' : 'none';
    }
    if (localGroup) {
      localGroup.style.display = layout === 'local' ? '' : 'none';
    }
  }

  function resetSettings() {
    if (!confirm('Reset all settings to defaults? This cannot be undone.')) return;

    const setVal = (id, val) => { const el = $(id); if (el) el.value = val; };
    const setChecked = (id, checked) => { const el = $(id); if (el) el.checked = checked; };

    setVal('#cacheDirInput', '');
    setVal('#localDirInput', '');
    setVal('#localScanDirs', '');
    setVal('#downloadLayout', 'cache');
    setVal('#hfToken', '');
    setVal('#connections', '8');
    setVal('#maxActive', '3');
    setVal('#maxSpeed', '');
    syncSpeedPresets();
    setVal('#retries', '4');
    setVal('#verify', 'size');
    setVal('#endpoint', '');
    setVal('#proxyUrl', '');
    setVal('#proxyUsername', '');
    setVal('#proxyPassword', '');
    setVal('#proxyNoProxy', '');
    setChecked('#proxyNoEnvProxy', false);
    syncStorageFields();
    showToast('Settings reset to defaults — save to apply', 'info');
  }

  // =========================================
  // Mirror Page
  // =========================================

  let mirrorData = { targets: [], localStats: null, diffResults: null };
  let diffFilter = 'all';
  let diffSearch = '';

  function initMirrorPage() {
    $('#addTargetBtn')?.addEventListener('click', showAddTargetModal);
    $('#refreshMirrorBtn')?.addEventListener('click', () => loadMirrorTargets());
    $('#mirrorDiffBtn')?.addEventListener('click', runMirrorDiff);
    $('#mirrorPushBtn')?.addEventListener('click', () => runMirrorSync('push'));
    $('#mirrorPullBtn')?.addEventListener('click', () => runMirrorSync('pull'));

    // Diff filter buttons
    $$('[data-diff-filter]').forEach(btn => {
      btn.addEventListener('click', () => {
        $$('[data-diff-filter]').forEach(b => b.classList.remove('active'));
        btn.classList.add('active');
        diffFilter = btn.dataset.diffFilter;
        renderDiffResults(mirrorData.diffResults);
      });
    });

    // Diff search
    $('#diffSearch')?.addEventListener('input', (e) => {
      diffSearch = e.target.value.toLowerCase();
      renderDiffResults(mirrorData.diffResults);
    });
  }

  async function loadMirrorTargets() {
    const container = $('#targetsList');
    if (isFilePreview()) {
      mirrorData.targets = [];
      renderMirrorTargets(mirrorData.targets);
      updateTargetSelect(mirrorData.targets);
      updateMirrorStats(mirrorData.targets);
      return;
    }

    if (container) {
      container.innerHTML = `
        <div class="loading-state">
          <div class="spinner"></div>
          <p>Loading targets...</p>
        </div>
      `;
    }

    try {
      const result = await api('GET', '/mirror/targets');
      mirrorData.targets = result.targets || [];
      renderMirrorTargets(mirrorData.targets);
      updateTargetSelect(mirrorData.targets);
      updateMirrorStats(mirrorData.targets);

      // Also load local cache stats
      try {
        const cacheResult = await api('GET', '/cache');
        mirrorData.localStats = cacheResult.stats;
        updateMirrorStats(mirrorData.targets);
      } catch (e) {
        // Ignore cache stats error
      }
    } catch (e) {
      if (container) {
        container.innerHTML = `
          <div class="empty-state">
            <p>Failed to load targets: ${escapeHtml(e.message)}</p>
          </div>
        `;
      }
    }
  }

  function updateMirrorStats(targets) {
    const onlineCount = targets.filter(t => t.exists).length;
    const totalCount = targets.length;

    $('#statTargets').textContent = totalCount;
    $('#statOnline').textContent = `${onlineCount}/${totalCount}`;

    // Local cache size
    if (mirrorData.localStats) {
      $('#statLocalSize').textContent = mirrorData.localStats.totalSizeHuman || '-';
    }

    // Sync status
    const syncStatusEl = $('#statSyncStatus');
    const syncStatusCard = $('#syncStatusCard');
    if (mirrorData.diffResults) {
      const s = mirrorData.diffResults.summary;
      if (s.inSync) {
        syncStatusEl.textContent = 'In Sync';
        syncStatusCard?.classList.remove('out-of-sync');
        syncStatusCard?.classList.add('in-sync');
      } else {
        syncStatusEl.textContent = `${s.missing || 0} pending`;
        syncStatusCard?.classList.remove('in-sync');
        syncStatusCard?.classList.add('out-of-sync');
      }
    } else {
      syncStatusEl.textContent = 'Unknown';
      syncStatusCard?.classList.remove('in-sync', 'out-of-sync');
    }
  }

  function renderMirrorTargets(targets) {
    const container = $('#targetsList');
    if (!container) return;

    // Build the targets grid with cards
    let html = '';

    if (!targets || targets.length === 0) {
      // Empty state as a card
      html = `
        <div class="target-card target-card-add" onclick="showAddTargetModal()">
          <div class="target-card-add-icon">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="24" height="24">
              <line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/>
            </svg>
          </div>
          <span class="target-card-add-text">Add your first mirror target</span>
        </div>
      `;
    } else {
      // Render target cards
      html = targets.map(t => `
        <div class="target-card ${t.exists ? 'target-online' : 'target-offline'}">
          <div class="target-card-header">
            <div class="target-card-title">
              <div class="target-card-icon">
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="20" height="20">
                  ${getTargetIcon(t.name)}
                </svg>
              </div>
              <span class="target-card-name">${escapeHtml(t.name)}</span>
            </div>
            <div class="target-card-status ${t.exists ? 'online' : 'offline'}">
              <span class="target-card-status-dot"></span>
              ${t.exists ? 'Online' : 'Offline'}
            </div>
          </div>
          <div class="target-card-path">${escapeHtml(t.path)}</div>
          ${t.description ? `<div class="target-card-description">${escapeHtml(t.description)}</div>` : ''}
          <div class="target-card-footer">
            <div class="target-card-meta">
              ${t.repoCount !== undefined ? `
                <div class="target-card-meta-item">
                  <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="14" height="14">
                    <path d="M21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16z"/>
                  </svg>
                  ${t.repoCount} repos
                </div>
              ` : ''}
              ${t.sizeHuman ? `
                <div class="target-card-meta-item">
                  <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="14" height="14">
                    <path d="M22 19a2 2 0 0 1-2 2H4a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h5l2 3h9a2 2 0 0 1 2 2z"/>
                  </svg>
                  ${t.sizeHuman}
                </div>
              ` : ''}
            </div>
            <div class="target-card-actions">
              <button class="btn btn-ghost btn-sm" onclick="event.stopPropagation(); selectTarget('${escapeHtml(t.name)}')" title="Select for sync">
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="16" height="16">
                  <polyline points="9 11 12 14 22 4"/>
                  <path d="M21 12v7a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2V5a2 2 0 0 1 2-2h11"/>
                </svg>
              </button>
              <button class="btn btn-ghost btn-sm" onclick="event.stopPropagation(); removeTarget('${escapeHtml(t.name)}')" title="Remove target">
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="16" height="16">
                  <polyline points="3 6 5 6 21 6"/><path d="M19 6v14a2 2 0 0 1-2 2H7a2 2 0 0 1-2-2V6m3 0V4a2 2 0 0 1 2-2h4a2 2 0 0 1 2 2v2"/>
                </svg>
              </button>
            </div>
          </div>
        </div>
      `).join('');

      // Add the "Add Target" card at the end
      html += `
        <div class="target-card target-card-add" onclick="showAddTargetModal()">
          <div class="target-card-add-icon">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="24" height="24">
              <line x1="12" y1="5" x2="12" y2="19"/><line x1="5" y1="12" x2="19" y2="12"/>
            </svg>
          </div>
          <span class="target-card-add-text">Add another target</span>
        </div>
      `;
    }

    container.innerHTML = html;
  }

  function getTargetIcon(name) {
    const lowerName = name.toLowerCase();
    if (lowerName.includes('nas') || lowerName.includes('network')) {
      return '<rect x="2" y="6" width="20" height="12" rx="2" ry="2"/><line x1="6" y1="10" x2="6" y2="14"/><line x1="10" y1="10" x2="10" y2="14"/>';
    } else if (lowerName.includes('usb') || lowerName.includes('drive') || lowerName.includes('external')) {
      return '<rect x="4" y="4" width="16" height="16" rx="2" ry="2"/><rect x="9" y="9" width="6" height="6"/><line x1="9" y1="2" x2="9" y2="4"/><line x1="15" y1="2" x2="15" y2="4"/>';
    } else if (lowerName.includes('office') || lowerName.includes('work') || lowerName.includes('server')) {
      return '<rect x="2" y="3" width="20" height="14" rx="2" ry="2"/><line x1="8" y1="21" x2="16" y2="21"/><line x1="12" y1="17" x2="12" y2="21"/>';
    } else if (lowerName.includes('cloud') || lowerName.includes('remote')) {
      return '<path d="M18 10h-1.26A8 8 0 1 0 9 20h9a5 5 0 0 0 0-10z"/>';
    } else {
      // Default: sync/mirror icon
      return '<polyline points="16 3 21 3 21 8"/><line x1="4" y1="20" x2="21" y2="3"/><polyline points="21 16 21 21 16 21"/><line x1="15" y1="15" x2="21" y2="21"/>';
    }
  }

  window.selectTarget = function(name) {
    const select = $('#syncTarget');
    if (select) {
      select.value = name;
      // Scroll to sync control
      document.querySelector('.sync-control-panel')?.scrollIntoView({ behavior: 'smooth', block: 'center' });
    }
  };

  function updateTargetSelect(targets) {
    const select = $('#syncTarget');
    if (!select) return;

    const currentValue = select.value;
    select.innerHTML = '<option value="">Select a target...</option>';

    targets.forEach(t => {
      const option = document.createElement('option');
      option.value = t.name;
      option.textContent = `${t.name} (${t.path})`;
      if (!t.exists) option.textContent += ' [offline]';
      select.appendChild(option);
    });

    if (currentValue) select.value = currentValue;
  }

  function showAddTargetModal() {
    showModal('Add Mirror Target', `
      <form id="addTargetForm" class="modal-form">
        <div class="form-group">
          <label for="targetName">Name</label>
          <input type="text" id="targetName" placeholder="e.g., office, usb, nas" required>
          <p class="form-hint">A short name to identify this target</p>
        </div>
        <div class="form-group">
          <label for="targetPath">Path</label>
          <input type="text" id="targetPath" placeholder="/path/to/hf/cache" required>
          <p class="form-hint">Absolute path to the HuggingFace cache directory</p>
        </div>
        <div class="form-group">
          <label for="targetDescription">Description (optional)</label>
          <input type="text" id="targetDescription" placeholder="e.g., Office NAS server">
        </div>
        <div class="form-actions">
          <button type="button" class="btn btn-ghost" onclick="hideModal()">Cancel</button>
          <button type="submit" class="btn btn-primary">Add Target</button>
        </div>
      </form>
    `);

    $('#addTargetForm')?.addEventListener('submit', async (e) => {
      e.preventDefault();
      const name = $('#targetName')?.value?.trim();
      const path = $('#targetPath')?.value?.trim();
      const description = $('#targetDescription')?.value?.trim();

      if (!name || !path) {
        showToast('Name and path are required', 'error');
        return;
      }

      try {
        await api('POST', '/mirror/targets', { name, path, description });
        hideModal();
        showToast(`Added target "${name}"`, 'success');
        loadMirrorTargets();
      } catch (e) {
        showToast(`Failed: ${e.message}`, 'error');
      }
    });
  }

  // Expose showAddTargetModal globally for onclick handlers
  window.showAddTargetModal = showAddTargetModal;

  window.removeTarget = async function(name) {
    if (!confirm(`Remove target "${name}"?`)) return;

    try {
      await api('DELETE', `/mirror/targets/${name}`);
      showToast(`Removed target "${name}"`, 'success');
      loadMirrorTargets();
    } catch (e) {
      showToast(`Failed: ${e.message}`, 'error');
    }
  };

  async function runMirrorDiff() {
    const target = $('#syncTarget')?.value;
    if (!target) {
      showToast('Please select a target', 'error');
      return;
    }

    const filter = $('#syncFilter')?.value?.trim() || '';
    const btn = $('#mirrorDiffBtn');

    try {
      btn.disabled = true;
      btn.innerHTML = '<span class="spinner-sm"></span> Comparing...';

      const result = await api('POST', '/mirror/diff', {
        target,
        repoFilter: filter
      });

      // Reset filters before showing new results
      diffFilter = 'all';
      diffSearch = '';
      $$('[data-diff-filter]').forEach(b => b.classList.remove('active'));
      $('[data-diff-filter="all"]')?.classList.add('active');
      const searchInput = $('#diffSearch');
      if (searchInput) searchInput.value = '';

      renderDiffResults(result);

      // Scroll to results
      $('#mirrorDiffSection')?.scrollIntoView({ behavior: 'smooth', block: 'start' });
    } catch (e) {
      showToast(`Failed: ${e.message}`, 'error');
    } finally {
      btn.disabled = false;
      btn.innerHTML = `
        <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="20" height="20">
          <circle cx="12" cy="12" r="10"/>
          <line x1="12" y1="8" x2="12" y2="12"/>
          <line x1="12" y1="16" x2="12.01" y2="16"/>
        </svg>
        Compare
      `;
    }
  }

  function renderDiffResults(result) {
    const section = $('#mirrorDiffSection');
    const container = $('#diffResults');
    const summary = $('#diffSummary');

    if (!section || !container) return;
    if (!result) {
      section.style.display = 'none';
      return;
    }

    section.style.display = 'block';

    // Store for filtering
    mirrorData.diffResults = result;

    const s = result.summary;

    // Update stats with sync status
    updateMirrorStats(mirrorData.targets);

    if (s.inSync) {
      summary.innerHTML = '<span class="badge badge-success">In Sync</span>';
      container.innerHTML = `
        <div class="empty-state" style="padding: 48px 24px;">
          <div class="empty-icon">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" width="64" height="64" style="color: var(--color-success);">
              <path d="M22 11.08V12a10 10 0 1 1-5.93-9.14"/>
              <polyline points="22 4 12 14.01 9 11.01"/>
            </svg>
          </div>
          <h3>Caches are in sync</h3>
          <p>No differences found between local and target.</p>
        </div>
      `;
      return;
    }

    // Build summary badges
    summary.innerHTML = `
      ${s.missing > 0 ? `<span class="badge badge-warning">${s.missing} to push (${s.missingSizeHuman})</span>` : ''}
      ${s.extra > 0 ? `<span class="badge badge-info">${s.extra} extra on target</span>` : ''}
      ${s.outdated > 0 ? `<span class="badge badge-secondary">${s.outdated} outdated</span>` : ''}
    `;

    if (!result.diffs || result.diffs.length === 0) {
      container.innerHTML = '<p style="padding: 24px; text-align: center; color: var(--color-text-muted);">No differences found.</p>';
      return;
    }

    // Apply filters
    let diffs = [...result.diffs];

    // Filter by status
    if (diffFilter !== 'all') {
      diffs = diffs.filter(d => d.status === diffFilter);
    }

    // Filter by search
    if (diffSearch) {
      diffs = diffs.filter(d => d.repo.toLowerCase().includes(diffSearch));
    }

    if (diffs.length === 0) {
      container.innerHTML = `
        <div class="empty-state" style="padding: 48px 24px;">
          <h3>No matches</h3>
          <p>No repositories match your current filter.</p>
        </div>
      `;
      return;
    }

    // Render the diff items
    const typeIcon = (type) => type === 'model'
      ? `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="14" height="14">
           <path d="M21 16V8a2 2 0 0 0-1-1.73l-7-4a2 2 0 0 0-2 0l-7 4A2 2 0 0 0 3 8v8a2 2 0 0 0 1 1.73l7 4a2 2 0 0 0 2 0l7-4A2 2 0 0 0 21 16z"/>
         </svg>`
      : `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="14" height="14">
           <path d="M4 19.5A2.5 2.5 0 0 1 6.5 17H20"/>
           <path d="M6.5 2H20v20H6.5A2.5 2.5 0 0 1 4 19.5v-15A2.5 2.5 0 0 1 6.5 2z"/>
         </svg>`;

    const statusLabel = (status) => {
      switch (status) {
        case 'missing': return 'To Push';
        case 'extra': return 'Extra';
        case 'outdated': return 'Outdated';
        default: return status;
      }
    };

    container.innerHTML = `
      <div class="diff-results-header">
        <span>Status</span>
        <span>Type</span>
        <span>Repository</span>
        <span>Size</span>
        <span></span>
      </div>
      ${diffs.map(d => `
        <div class="diff-item">
          <div>
            <span class="diff-item-status ${d.status}">${statusLabel(d.status)}</span>
          </div>
          <div class="diff-item-type">
            ${typeIcon(d.type)}
            ${d.type}
          </div>
          <div class="diff-item-repo">${escapeHtml(d.repo)}</div>
          <div class="diff-item-size">${d.sizeHuman || '-'}</div>
          <div class="diff-item-action">
            ${d.status === 'missing' ? `
              <button class="btn btn-ghost btn-sm" onclick="event.stopPropagation();" title="Will be pushed">
                <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" width="14" height="14">
                  <line x1="12" y1="19" x2="12" y2="5"/><polyline points="5 12 12 5 19 12"/>
                </svg>
              </button>
            ` : ''}
          </div>
        </div>
      `).join('')}
    `;
  }

  async function runMirrorSync(direction) {
    const target = $('#syncTarget')?.value;
    if (!target) {
      showToast('Please select a target', 'error');
      return;
    }

    const filter = $('#syncFilter')?.value?.trim() || '';
    const verify = $('#syncVerify')?.checked || false;
    const force = $('#syncForce')?.checked || false;
    const deleteExtra = $('#syncDelete')?.checked || false;

    const action = direction === 'push' ? 'Push to' : 'Pull from';
    if (!confirm(`${action} target "${target}"?\n\nThis will copy repos ${direction === 'push' ? 'from local to target' : 'from target to local'}.${deleteExtra ? '\n\nWARNING: Extra repos will be deleted!' : ''}`)) {
      return;
    }

    const btn = direction === 'push' ? $('#mirrorPushBtn') : $('#mirrorPullBtn');
    const originalHtml = btn.innerHTML;

    try {
      btn.disabled = true;
      btn.innerHTML = `<span class="spinner-sm"></span> ${direction === 'push' ? 'Pushing' : 'Pulling'}...`;

      const result = await api('POST', `/mirror/${direction}`, {
        target,
        repoFilter: filter,
        verify,
        force,
        deleteExtra,
        dryRun: false
      });

      if (result.success) {
        showToast(result.message, 'success');
      } else {
        showToast(result.message + (result.errors?.length ? ` (${result.errors.length} errors)` : ''), 'warning');
      }

      // Refresh diff after sync
      runMirrorDiff();
    } catch (e) {
      showToast(`Failed: ${e.message}`, 'error');
    } finally {
      btn.disabled = false;
      btn.innerHTML = originalHtml;
    }
  }

  // =========================================
  // Modal
  // =========================================

  function showModal(title, content) {
    $('#modalTitle').textContent = title;
    $('#modalBody').innerHTML = content;
    $('#modalBackdrop').classList.add('active');
  }

  function setModalContent(content) {
    $('#modalBody').innerHTML = content;
  }

  function hideModal() {
    $('#modalBackdrop').classList.remove('active');
  }

  // Expose hideModal globally for onclick handlers
  window.hideModal = hideModal;

  function initModal() {
    $('#modalClose')?.addEventListener('click', hideModal);
    $('#modalBackdrop')?.addEventListener('click', (e) => {
      if (e.target === $('#modalBackdrop')) hideModal();
    });

    // ESC key to close modals
    document.addEventListener('keydown', (e) => {
      if (e.key === 'Escape') {
        const modal = $('#modalBackdrop');
        if (modal?.classList.contains('active')) {
          hideModal();
        }
      }
    });
  }

  // =========================================
  // Toast
  // =========================================

  function showToast(message, type = 'info') {
    const container = $('#toastContainer');
    if (!container) return;

    const toast = document.createElement('div');
    toast.className = `toast ${type}`;
    toast.innerHTML = `<span class="toast-message">${escapeHtml(message)}</span>`;

    container.appendChild(toast);

    setTimeout(() => {
      toast.style.animation = 'slideIn 0.3s ease reverse';
      setTimeout(() => toast.remove(), 300);
    }, 4000);
  }

  // =========================================
  // Selectable Items Component
  // =========================================

  /**
   * Renders downloadable items grouped by category.
   * Works for all model types: GGUF quantizations, Diffusers components, etc.
   */
  // ── Quant/selectable row download status ──────────────────────────
  // localStorage key: "dlStatus:<repo>" → { "<filter_value>": "queued"|"running"|"done" }
  function getDlStatus(repo) {
    try { return JSON.parse(localStorage.getItem('dlStatus:' + repo) || '{}'); } catch { return {}; }
  }
  function setDlStatus(repo, filterValue, status) {
    const map = getDlStatus(repo);
    map[filterValue] = status;
    localStorage.setItem('dlStatus:' + repo, JSON.stringify(map));
  }
  function refreshDlStatusFromJobs(repo) {
    // Reconcile per-quant status with the live job list. Active jobs set
    // running/queued/done; stale running/queued entries (job gone, cancelled or
    // failed) are cleared so the Download button reappears. 'done' is kept.
    const map = getDlStatus(repo);
    const active = {};
    Array.from(state.jobs.values()).forEach(j => {
      if (j.repo !== repo) return;
      const f = (j.filters || []).join(',') || '__all__';
      if (j.status === 'completed') active[f] = 'done';
      else if (j.status === 'running') active[f] = 'running';
      else if (j.status === 'queued') active[f] = 'queued';
    });
    let changed = false;
    Object.keys(map).forEach(f => {
      if ((map[f] === 'running' || map[f] === 'queued') && !active[f]) {
        delete map[f];
        changed = true;
      }
    });
    Object.keys(active).forEach(f => {
      if (map[f] !== active[f]) {
        map[f] = active[f];
        changed = true;
      }
    });
    if (changed) localStorage.setItem('dlStatus:' + repo, JSON.stringify(map));
  }

  function renderSelectableItems(items, containerId) {
    if (!items || items.length === 0) return '';

    const repo = currentAnalysis?.repo || '';
    refreshDlStatusFromJobs(repo);
    const dlMap = getDlStatus(repo);

    const categories = {};
    items.forEach(item => {
      const cat = item.category || 'default';
      if (!categories[cat]) categories[cat] = [];
      categories[cat].push(item);
    });

    const categoryTitles = {
      'quantization': 'Quantizations',
      'variant': 'Variants',
      'component': 'Components',
      'split': 'Dataset Splits',
      'format': 'Weight Format',
      'precision': 'Precision',
      'vision_encoder': 'Vision Encoder',
      'default': 'Options'
    };

    let html = `<div class="selectable-items quant-list" id="${containerId}">`;

    for (const [category, categoryItems] of Object.entries(categories)) {
      const title = categoryTitles[category] || category.charAt(0).toUpperCase() + category.slice(1);

      html += `
        <div class="quant-category">
          <div class="quant-category-title">${escapeHtml(title)}</div>`;

      categoryItems.forEach(item => {
        const stars   = item.quality > 0 ? renderQualityStars(item.quality) : '';
        const recBadge= item.recommended ? '<span class="quant-rec">★ rec</span>' : '';
        const size    = item.size_human  ? `<span class="quant-size">${escapeHtml(item.size_human)}</span>` : '';
        const ram     = item.ram_human   ? `<span class="quant-ram">${escapeHtml(item.ram_human)} RAM</span>` : '';
        const desc    = item.description ? `<span class="quant-desc">${renderQuantDescription(item.description)}</span>` : '';

        const fv = item.filter_value || item.id || '';
        const dlStatus = dlMap[fv] || '';
        // upstream_repo is set for vision_encoder items sourced from a base-model
        // repo; in that case the download must target that repo, not the current one.
        const dlRepo = item.upstream_repo || repo;

        // Left control: a Download button when idle; otherwise it is REPLACED by
        // a status chip reflecting the quant's state in the download queue
        // (done / downloading / queued) so it can't be started twice.
        const dlIcon = `<svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" width="14" height="14">
                 <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/>
                 <polyline points="7 10 12 15 17 10"/><line x1="12" y1="15" x2="12" y2="3"/>
               </svg>`;
        const dlBtn = dlStatus === 'done'
          ? `<span class="quant-dl-btn quant-dl-btn-done" title="Already downloaded">✓</span>`
          : dlStatus === 'queued'
            ? `<span class="quant-dl-btn quant-dl-btn-queued" title="Queued">…</span>`
          : dlStatus === 'running'
            ? `<span class="quant-dl-btn quant-dl-btn-running" title="Downloading…"><span class="quant-spinner"></span></span>`
          : `<button class="quant-dl-btn" title="Download this quantization"
               onclick="downloadQuant('${escapeHtml(dlRepo)}','${escapeHtml(fv)}',${currentAnalysis?.is_dataset||false},'${escapeHtml(item.label)}','${item.upstream_repo ? escapeHtml(repo) : ''}')">${dlIcon}</button>`;

        html += `
          <div class="quant-row ${item.recommended ? 'quant-row-rec' : ''}" data-fv="${escapeHtml(fv)}">
            ${dlBtn}
            <span class="quant-label">${escapeHtml(item.label)}</span>
            ${recBadge}
            ${size}
            ${ram}
            ${stars}
            ${desc}
          </div>`;
      });

      html += `</div>`;
    }

    html += '</div>';
    return html;
  }

  // Download a single quantization from the quant list
  window.downloadQuant = async function(repo, filterValue, isDataset, label, localRepo) {
    try {
      // Disk-free guard
      const df = await fetch('/api/diskfree').then(r => r.json()).catch(() => null);
      if (df && df.free < 100 * 1024 * 1024) {
        showToast(`Not enough disk space: ${formatBytes(df.free)} free`, 'error');
        return;
      }

      // When downloading from an upstream repo (e.g. mmproj from base model),
      // always use "main" — the current analysis revision applies only to
      // the repo being analyzed, not to its ancestors.
      const revision = (repo === currentAnalysis?.repo)
        ? (currentAnalysis?.branch || 'main')
        : 'main';
      const body = {
        repo,
        revision,
        dataset: isDataset,
        filters: filterValue ? [filterValue] : [],
        exactMatch: !!filterValue
      };
      // When downloading from an upstream repo (e.g. mmproj from a base model),
      // tell the server to store the file under the current model's folder.
      if (localRepo) body.localRepo = localRepo;
      const data = await api('POST', '/download', body);

      if (data.message) {
        showToast(data.message, 'info');
        return;
      }

      state.jobs.set(data.id, data);
      setDlStatus(repo, filterValue, 'queued');
      // Replace the download button with the queued chip immediately.
      const row = document.querySelector(`.quant-row[data-fv="${CSS.escape(filterValue)}"]`);
      if (row) {
        const btn = row.querySelector('.quant-dl-btn');
        if (btn) btn.outerHTML = `<span class="quant-dl-btn quant-dl-btn-queued" title="Queued">…</span>`;
      }

      showToast(`Queued: ${label || repo}`, 'success');
    } catch (e) {
      showToast(`Failed: ${e.message}`, 'error');
    }
  };

  /**
   * For non-GGUF/non-selectable models: renders a plain file list with per-file download.
   */
  function renderNonSelectableFiles(data) {
    if (!data.files?.length) return '';
    const repo = data.repo;
    // Exclude non-model files from the list (keep only content files)
    const files = data.files.filter(f => {
      const n = (f.path || f.name || '').toLowerCase();
      return !n.endsWith('.md') && !n.endsWith('.gitattributes') && !n.endsWith('.txt') || data.files.length <= 5;
    });
    if (!files.length) return '';

    const rows = files.slice(0, 50).map(f => {
      const name = f.path || f.name || '';
      const size = f.size_human || (f.size ? formatBytes(f.size) : '');
      return `
        <div class="quant-row">
          <button class="quant-dl-btn" title="Download file"
            onclick="downloadSingleFile('${escapeHtml(repo)}', '${escapeHtml(name)}', ${data.is_dataset})">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2.5" width="14" height="14">
              <path d="M21 15v4a2 2 0 0 1-2 2H5a2 2 0 0 1-2-2v-4"/>
              <polyline points="7 10 12 15 17 10"/><line x1="12" y1="15" x2="12" y2="3"/>
            </svg>
          </button>
          <span class="quant-label" style="font-family:var(--font-mono);font-size:12px">${escapeHtml(name)}</span>
          ${size ? `<span class="quant-size">${escapeHtml(size)}</span>` : ''}
        </div>`;
    }).join('');

    const more = data.files.length > 50 ? `<div class="quant-row" style="color:var(--color-text-muted);font-size:12px;padding:6px 12px">… and ${data.files.length - 50} more files</div>` : '';

    return `
      <div class="analysis-section">
        <div class="quant-category">
          <div class="quant-category-title">Files (${data.files.length})</div>
          ${rows}${more}
        </div>
      </div>`;
  }

  // Download a single specific file by filter
  window.downloadSingleFile = async function(repo, filename, isDataset) {
    try {
      await api('POST', '/download', {
        repo, revision: currentAnalysis?.branch || 'main',
        dataset: isDataset, filters: [filename], exactMatch: true
      });
      showToast(`Queued: ${filename}`, 'success');
    } catch (e) {
      showToast(`Failed: ${e.message}`, 'error');
    }
  };

  /**
   * Renders quality stars (1-5).
   */
  function renderQualityStars(quality) {
    if (!quality || quality < 1 || quality > 5) return '';
    const filled = '★'.repeat(quality);
    const empty = '☆'.repeat(5 - quality);
    return `<span class="selector-stars">${filled}${empty}</span>`;
  }

  function renderQuantDescription(description) {
    if (!description) return '';
    return escapeHtml(description).replace(/\brecommended\b/gi, '<span class="quant-desc-rec">$&</span>');
  }

  /**
   * Renders related downloads section (e.g., base model for LoRA).
   */
  function renderRelatedDownloads(downloads) {
    if (!downloads || downloads.length === 0) return '';

    let html = `
      <div class="analysis-section related-downloads-section">
        <h4>Related Downloads</h4>
        <div class="related-downloads">`;

    downloads.forEach(dl => {
      const requiredBadge = dl.required ? '<span class="badge-required">Required</span>' : '';

      html += `
        <div class="related-download-card ${dl.required ? 'required' : ''}">
          <div class="related-download-info">
            <span class="related-download-label">${escapeHtml(dl.label)} ${requiredBadge}</span>
            <span class="related-download-repo">${escapeHtml(dl.repo)}</span>
            ${dl.description ? `<span class="related-download-desc">${escapeHtml(dl.description)}</span>` : ''}
            ${dl.size_human ? `<span class="related-download-size">${escapeHtml(dl.size_human)}</span>` : ''}
          </div>
          <button class="btn btn-secondary btn-sm" onclick="analyzeRelatedRepo('${escapeHtml(dl.repo)}')">
            Analyze
          </button>
        </div>`;
    });

    html += '</div></div>';
    return html;
  }

  /**
   * Analyze a related repo (from LoRA base model link).
   */
  window.analyzeRelatedRepo = function(repo) {
    analyzeRepo(null, null, repo);
  };

  // =========================================
  // Models Page (unified Search + Analyze + Download)
  // =========================================

  let _modelsDebounce = null;
  let _modelsInitialized = false;
  let _currentSelectedRow = null;
  let _syncModelsSearchClear = () => {};

  function clearModelsSelection() {
    if (_currentSelectedRow) {
      _currentSelectedRow.classList.remove('model-row-active');
      _currentSelectedRow = null;
    }
  }

  function initModelsPage() {
    if (_modelsInitialized) return;
    _modelsInitialized = true;

    const input = $('#analyzeInput');
    const btn   = $('#analyzeBtn');
    const clearBtn = $('#modelsSearchClear');

    // Toggle the clear (×) button based on whether the field has text
    const syncClearBtn = () => {
      if (clearBtn) clearBtn.hidden = !(input && input.value.length > 0);
    };
    _syncModelsSearchClear = syncClearBtn;
    syncClearBtn();

    // Debounced search on typing
    input?.addEventListener('input', () => {
      syncClearBtn();
      clearModelsSelection();
      clearTimeout(_modelsDebounce);
      _modelsDebounce = setTimeout(loadModelsSearch, 380);
    });

    // Clear button: empty the field, hide itself, refresh results
    clearBtn?.addEventListener('click', () => {
      if (!input) return;
      input.value = '';
      syncClearBtn();
      clearModelsSelection();
      input.focus();
      clearTimeout(_modelsDebounce);
      loadModelsSearch();
    });

    // Enter key → analyze if looks like repo, else search
    input?.addEventListener('keypress', (e) => {
      if (e.key === 'Enter') {
        clearTimeout(_modelsDebounce);
        const v = input.value.trim();
        if (v.includes('/')) {
          analyzeRepo();
        } else {
          loadModelsSearch();
        }
      }
    });

    // Analyze/refresh button. On the unified Models page this refreshes the
    // selected repo, not whatever text happens to be in the search field.
    btn?.addEventListener('click', () => {
      const repo = currentSelectedRepo || currentAnalysis?.repo || input?.value?.trim();
      if (!repo) {
        analyzeRepo();
        return;
      }
      analyzeRepo(
        currentSelectedIsDataset || currentAnalysis?.is_dataset ? 'dataset' : null,
        currentSelectedBranch || currentAnalysis?.branch || 'main',
        repo
      );
    });

    // Example buttons
    $$('.example-btn').forEach(b => {
      b.addEventListener('click', () => {
        if (input) input.value = b.dataset.repo;
        _syncModelsSearchClear();
        analyzeRepo(b.dataset.type || null);
      });
    });

    // Filter controls trigger new search
    ['modelsSortSelect','modelsTypeSelect','modelsGguf','modelsDatasets'].forEach(id => {
      $(`#${id}`)?.addEventListener('change', () => {
        clearTimeout(_modelsDebounce);
        clearModelsSelection();
        loadModelsSearch();
      });
    });

    // Scroll arrows: contextual top/bottom
    const scroller = $('#analyzeResult');
    const arrowUp = $('#scrollArrowUp');
    const arrowDown = $('#scrollArrowDown');

    arrowUp?.addEventListener('click', () => {
      if (scroller) scroller.scrollTo({ top: 0, behavior: 'smooth' });
    });
    arrowDown?.addEventListener('click', () => {
      if (scroller) scroller.scrollTo({ top: scroller.scrollHeight, behavior: 'smooth' });
    });
    scroller?.addEventListener('scroll', updateScrollArrows);
  }

  async function loadModelsSearch() {
    const input   = $('#analyzeInput');
    const spinner = $('#modelsSpinner');
    const list    = $('#modelsResultList');
    if (!list) return;

    const q      = input?.value?.trim() || '';
    const sortBy = $('#modelsSortSelect')?.value || 'downloads';
    const filter = buildSearchFilter($('#modelsTypeSelect')?.value, $('#modelsGguf')?.checked);
    const isDs   = $('#modelsDatasets')?.checked || false;

    const params = new URLSearchParams({ sort: sortBy, limit: '40' });
    if (q) params.set('q', q);
    if (filter) params.set('filter', filter);
    if (isDs) params.set('datasets', 'true');

    if (spinner) spinner.style.display = '';

    try {
      const res  = await fetch('/api/search?' + params);
      const data = await res.json();
      renderModelsResultList(data.results || [], q);
    } catch (e) {
      list.innerHTML = `<div class="models-list-hint" style="color:var(--color-error)">Search error: ${escapeHtml(e.message)}</div>`;
    } finally {
      if (spinner) spinner.style.display = 'none';
    }
  }

  function buildSearchFilter(type, ggufOnly) {
    const parts = [];
    if (type) parts.push(type);
    if (ggufOnly) parts.push('gguf');
    return parts.join(',');
  }

  function renderModelsResultList(items, q) {
    const list = $('#modelsResultList');
    if (!items.length) {
      list.innerHTML = `<div class="models-list-hint">${q ? 'No results for "' + escapeHtml(q) + '"' : 'No results'}</div>`;
      return;
    }
    list.innerHTML = items.map(renderModelRow).join('');
  }

  function renderModelRow(r) {
    const parts  = (r.id || '').split('/');
    const author = parts.length > 1 ? parts[0] : '';
    const name   = parts.length > 1 ? parts.slice(1).join('/') : r.id;
    const caps = detectSearchCapabilities(r);

    const isGguf = (r.tags || []).includes('gguf');
    const badge  = isGguf
      ? `<span class="model-row-badge model-row-badge-gguf">GGUF</span>`
      : '';
    const localBadge = r.cached
      ? `<span class="model-row-badge model-row-badge-local" title="${escapeHtml(r.cacheSource || 'Local cache')}">local</span>`
      : '';

    const dl = r.downloads ? `<span class="model-row-stat">↓${formatNumber(r.downloads)}</span>` : '';
    const lk = r.likes     ? `<span class="model-row-stat like-accent">♥${formatNumber(r.likes)}</span>` : '';
    const gated = r.gated  ? `<span class="model-row-stat" title="Gated">🔒</span>` : '';
    const capabilityIcons = renderCapabilityIcons(caps.capabilities, caps.titles);

    return `
      <div class="model-row" data-repo="${escapeHtml(r.id)}" onclick="modelsSelectRow(this,'${escapeHtml(r.id)}')">
        <div class="model-row-avatar">${escapeHtml((author || name).substring(0,2).toUpperCase())}</div>
        <div class="model-row-info">
          <div class="model-row-name">${escapeHtml(name)}</div>
          <div class="model-row-meta">${escapeHtml(author)}${dl}${lk}${gated}</div>
        </div>
        <div class="model-row-right">${capabilityIcons}${localBadge}${badge}</div>
      </div>`;
  }

  function detectSearchCapabilities(result) {
    const tags = (result.tags || []).map(t => String(t).toLowerCase());
    const text = [
      result.id,
      result.pipelineTag,
      result.libraryName,
      ...tags
    ].filter(Boolean).join(' ').toLowerCase();
    const capabilities = [];
    const titles = {};

    const hasAny = (needles) => needles.some(n => text.includes(n));
    const hasTag = (needles) => needles.some(n => tags.includes(n));
    const pipeline = String(result.pipelineTag || '').toLowerCase();

    if (
      ['image-to-text', 'image-text-to-text', 'visual-question-answering', 'image-classification', 'object-detection'].includes(pipeline) ||
      hasTag(['vision', 'computer-vision', 'multimodal', 'vlm']) ||
      hasAny([' vision', 'vision-', '-vision', 'vlm', 'multimodal', 'mmproj', 'image-to-text', 'image-text-to-text'])
    ) {
      capabilities.push('vision');
      titles.vision = 'Vision capability detected from HF tags/name';
    }

    if (hasTag(['tool', 'tools', 'tool-use', 'function-calling']) || hasAny(['tool-use', 'tool_use', 'function-calling', 'function_calling', 'tools', ' tool '])) {
      capabilities.push('tools');
      titles.tools = 'Tool-use capability detected from HF tags/name';
    }

    if (hasTag(['reasoning', 'reasoner', 'thinking']) || hasAny(['reasoning', 'reasoner', 'thinking', 'deepseek-r1', 'qwq', 'cot', 'chain-of-thought'])) {
      capabilities.push('reasoning');
      titles.reasoning = 'Reasoning signal detected from HF tags/name';
    }

    return { capabilities, titles };
  }

  // Called when a row is clicked
  window.modelsSelectRow = function(el, repoId) {
    // Highlight active row
    if (_currentSelectedRow) _currentSelectedRow.classList.remove('model-row-active');
    el.classList.add('model-row-active');
    _currentSelectedRow = el;

    // Update detail header
    currentSelectedRepo = repoId;
    currentSelectedIsDataset = false;
    currentSelectedBranch = 'main';
    syncModelsDetailHeader(repoId, false, currentSelectedBranch);

    // Keep the search field user-owned; analyze the clicked repo separately.
    analyzeRepo(null, null, repoId);
  };

  // (legacy compat — old Search page rendered cards with Analyze button)
  function renderSearchResults() {}
  function renderModelCard() { return ''; }

  function buildVisibleTags(tags) {
    const result = [];
    const skip = new Set(['region:us', 'endpoints_compatible', 'text-generation-inference', 'eval-results']);
    let count = 0;
    for (const t of tags) {
      if (skip.has(t)) continue;
      if (t.startsWith('base_model:') || t.startsWith('arxiv:') || t.startsWith('license:')) continue;
      const isGguf = t === 'gguf';
      if (isGguf) { result.unshift({ label: 'GGUF', cls: 'model-tag model-tag-gguf' }); continue; }
      if (count < 4) {
        result.push({ label: t, cls: 'model-tag' });
        count++;
      }
    }
    return result.slice(0, 6);
  }

  function navigateToAnalyze(repoId) {
    navigateTo('models');
    analyzeRepo(null, null, repoId);
  }

  function formatNumber(n) {
    if (!n) return '0';
    if (n >= 1_000_000) return (n / 1_000_000).toFixed(1) + 'M';
    if (n >= 1_000) return (n / 1_000).toFixed(1) + 'K';
    return String(n);
  }

  // =========================================
  // History Page
  // =========================================

  async function loadHistory() {
    const container = $('#historyList');
    if (!container) return;

    try {
      const res  = await fetch('/api/history');
      const data = await res.json();
      renderHistory(data.entries || []);
    } catch (e) {
      container.innerHTML = `<div class="empty-state"><h3>Failed to load history</h3><p>${escapeHtml(e.message)}</p></div>`;
    }
  }

  function renderHistory(entries) {
    const container = $('#historyList');
    if (!entries.length) {
      container.innerHTML = `
        <div class="empty-state">
          <div class="empty-icon">
            <svg viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="1.5" width="64" height="64">
              <circle cx="12" cy="12" r="10"/><polyline points="12 6 12 12 16 14"/>
            </svg>
          </div>
          <h3>No History Yet</h3>
          <p>Completed and failed downloads will appear here.</p>
        </div>`;
      return;
    }

    // newest first
    const sorted = [...entries].reverse();

    const rows = sorted.map(e => {
      const statusCls = e.status === 'completed' ? 'history-status-completed' : 'history-status-failed';
      const statusLabel = e.status === 'completed' ? '✓ Completed' : '✗ Failed';
      const date = e.endedAt ? formatRelativeDate(e.endedAt) : '—';
      const size = e.totalBytes ? formatBytes(e.totalBytes) : '—';
      const rev = e.revision && e.revision !== 'main' ? `<div class="history-repo-rev">@ ${escapeHtml(e.revision)}</div>` : '';
      const type = e.isDataset ? '<span class="model-tag" style="font-size:11px">dataset</span>' : '';
      return `
        <tr>
          <td>
            <div class="history-repo">${escapeHtml(e.repo)}${type}</div>
            ${rev}
          </td>
          <td><span class="${statusCls}">${statusLabel}</span>${e.error ? `<div style="font-size:11px;color:var(--color-error);margin-top:3px">${escapeHtml(e.error.substring(0,80))}</div>` : ''}</td>
          <td>${e.totalFiles ? e.totalFiles + ' files' : '—'}</td>
          <td>${size}</td>
          <td class="history-date" title="${e.endedAt || ''}">${date}</td>
          <td><div class="history-dir" title="${escapeHtml(e.outputDir || '')}">${escapeHtml(e.outputDir || '—')}</div></td>
        </tr>`;
    }).join('');

    container.innerHTML = `
      <div class="history-table-wrap">
        <table class="history-table">
          <thead>
            <tr>
              <th>Repository</th>
              <th>Status</th>
              <th>Files</th>
              <th>Size</th>
              <th>Completed</th>
              <th>Output Dir</th>
            </tr>
          </thead>
          <tbody>${rows}</tbody>
        </table>
      </div>`;
  }

  function formatRelativeDate(iso) {
    try {
      const d = new Date(iso);
      const diff = Date.now() - d.getTime();
      const mins = Math.floor(diff / 60000);
      if (mins < 1) return 'just now';
      if (mins < 60) return `${mins}m ago`;
      const hrs = Math.floor(mins / 60);
      if (hrs < 24) return `${hrs}h ago`;
      const days = Math.floor(hrs / 24);
      if (days < 7) return `${days}d ago`;
      return d.toLocaleDateString();
    } catch { return iso; }
  }

  // =========================================
  // Utilities
  // =========================================

  function escapeHtml(str) {
    if (!str) return '';
    const div = document.createElement('div');
    div.textContent = str;
    return div.innerHTML;
  }

  function formatBytes(bytes) {
    if (!bytes || bytes === 0) return '0 B';
    const k = 1024;
    const sizes = ['B', 'KB', 'MB', 'GB', 'TB'];
    const i = Math.floor(Math.log(bytes) / Math.log(k));
    return parseFloat((bytes / Math.pow(k, i)).toFixed(1)) + ' ' + sizes[i];
  }

  // =========================================
  // Sidebar resize
  // =========================================
  function initSidebarResize() {
    const handle  = $('#sidebarResizeHandle');
    const sidebar = handle?.closest('aside');
    if (!handle || !sidebar) return;

    // Restore saved width
    const saved = localStorage.getItem('sidebarWidth');
    if (saved) sidebar.style.width = saved + 'px';

    let startX, startW;

    handle.addEventListener('mousedown', (e) => {
      e.preventDefault();
      startX = e.clientX;
      startW = sidebar.getBoundingClientRect().width;
      handle.classList.add('dragging');
      document.body.style.cursor = 'col-resize';
      document.body.style.userSelect = 'none';
    });

    document.addEventListener('mousemove', (e) => {
      if (!handle.classList.contains('dragging')) return;
      const delta = e.clientX - startX;
      const newW  = Math.max(160, Math.min(600, startW + delta));
      sidebar.style.width = newW + 'px';
    });

    document.addEventListener('mouseup', () => {
      if (!handle.classList.contains('dragging')) return;
      handle.classList.remove('dragging');
      document.body.style.cursor = '';
      document.body.style.userSelect = '';
      const w = Math.round(sidebar.getBoundingClientRect().width);
      localStorage.setItem('sidebarWidth', w);
    });
  }

  // =========================================
  // Models list resize
  // =========================================
  function initModelsListResize() {
    const handle = $('#modelsListResizeHandle');
    const split = $('.models-split');
    const list = $('.models-list-panel');
    if (!handle || !split || !list) return;

    const minW = 300;
    const defaultW = 400;
    const preferredMaxW = 640;
    const detailMinW = 360;
    const clampWidth = (width) => {
      const splitW = split.getBoundingClientRect().width || (preferredMaxW + detailMinW);
      const maxW = Math.max(minW, Math.min(preferredMaxW, splitW - detailMinW));
      return Math.max(minW, Math.min(maxW, width));
    };
    const applyWidth = (width) => {
      split.style.setProperty('--models-list-width', clampWidth(width) + 'px');
    };

    const saved = parseInt(localStorage.getItem('modelsListWidth') || '', 10);
    if (Number.isFinite(saved) && saved > 0) {
      const migrated = localStorage.getItem('modelsListWidthV2') === '1';
      const width = !migrated && saved < defaultW ? defaultW : saved;
      applyWidth(width);
      if (!migrated) {
        localStorage.setItem('modelsListWidthV2', '1');
        localStorage.setItem('modelsListWidth', Math.round(clampWidth(width)));
      }
    } else {
      applyWidth(defaultW);
      localStorage.setItem('modelsListWidthV2', '1');
    }

    let startX = 0;
    let startW = 0;

    handle.addEventListener('mousedown', (e) => {
      e.preventDefault();
      startX = e.clientX;
      startW = list.getBoundingClientRect().width;
      handle.classList.add('dragging');
      document.body.style.cursor = 'col-resize';
      document.body.style.userSelect = 'none';
    });

    document.addEventListener('mousemove', (e) => {
      if (!handle.classList.contains('dragging')) return;
      applyWidth(startW + (e.clientX - startX));
    });

    document.addEventListener('mouseup', () => {
      if (!handle.classList.contains('dragging')) return;
      handle.classList.remove('dragging');
      document.body.style.cursor = '';
      document.body.style.userSelect = '';
      const width = Math.round(list.getBoundingClientRect().width);
      localStorage.setItem('modelsListWidth', width);
    });
  }

  // =========================================
  // Initialize
  // =========================================

  // Show the build version (from /api/health) in the sidebar, so the UI tracks
  // the binary instead of a hardcoded number.
  async function loadAppVersion() {
    try {
      const res = await fetch('/api/health');
      if (!res.ok) return;
      const data = await res.json();
      const el = $('#appVersion');
      if (el && data.version) el.textContent = 'v' + data.version;
    } catch (_) { /* leave blank if unavailable */ }
  }

  function init() {
    initNavigation();
    initSidebarResize();
    initModelsListResize();
    if (!isFilePreview()) {
      initWebSocket();
    } else {
      updateConnectionStatus(false);
      const statusText = $('#connectionStatus .status-text');
      if (statusText) statusText.textContent = 'Preview';
    }
    initModelsPage();      // unified: analyze + search + download
    initCachePage();
    initSettingsPage();
    initMirrorPage();
    initModal();
    loadAppVersion();

    // Load initial data
    if (!isFilePreview()) {
      loadJobs();
      loadModelsSearch();    // populate models list on startup
    }
    navigateTo(getInitialPage());
  }

  // Start
  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }

})();
