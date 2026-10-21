/**
 * HFDesk - Runtime / Logs view
 *
 * A small self contained view for the HFDesk runtime window. It shows the
 * status of the running server and tails the runtime log.
 *
 * The page is served by the HFDesk server itself, so every request is same
 * origin: no CORS handling and no credentials are needed.
 */

(function () {
  'use strict';

  // =========================================
  // Constants
  // =========================================

  // MAX_RECORDS is the number of records the client keeps. The oldest
  // records are dropped when the limit is reached, so a busy server cannot
  // grow the window without bound.
  const MAX_RECORDS = 2000;

  // MAX_ROWS_PER_FLUSH is the number of rows one flush appends. A burst of
  // records is rendered over several flushes, so a busy server cannot lock
  // the window.
  const MAX_ROWS_PER_FLUSH = 500;

  // PAGE_SIZE is the number of records a log page asks for.
  const PAGE_SIZE = 500;

  // STATUS_INTERVAL is the refresh interval of the runtime status.
  const STATUS_INTERVAL = 4000;

  // POLL_INTERVAL is the fallback interval used while the event stream is
  // not available.
  const POLL_INTERVAL = 2000;

  // POLL_MAX_INTERVAL bounds the backoff of the fallback poll. A server
  // that does not answer is polled less and less often.
  const POLL_MAX_INTERVAL = 15000;

  // FLUSH_INTERVAL batches the DOM updates: the rows of a burst are built
  // in a single fragment and appended once, never row by row.
  const FLUSH_INTERVAL = 200;

  // RENDER_DEBOUNCE delays the re-render of a filter change, so a fast
  // typist does not trigger a re-render per keystroke.
  const RENDER_DEBOUNCE = 120;

  // RECONNECT_MIN and RECONNECT_MAX bound the backoff of the stream
  // reconnect.
  const RECONNECT_MIN = 1000;
  const RECONNECT_MAX = 10000;

  // LEVEL_RANK maps a level onto its severity. The level filter is a
  // minimum severity, so a filter of "warn" shows the warn and the error
  // records.
  const LEVEL_RANK = { DEBUG: 0, INFO: 1, WARN: 2, ERROR: 3 };
  const FILTER_RANK = { debug: 0, info: 1, warn: 2, error: 3 };

  // LEVEL_BADGE maps a level onto the badge modifier of the design system.
  const LEVEL_BADGE = {
    DEBUG: 'badge-secondary',
    INFO: 'badge-info',
    WARN: 'badge-warning',
    ERROR: 'runtime-level-error'
  };

  // STORAGE_KEY holds the view state of the window. The state is a
  // convenience only: the page works the same when the storage is not
  // available.
  const STORAGE_KEY = 'hfdesk.runtime.view';

  // =========================================
  // State
  // =========================================

  const state = {
    // status is the last runtime status the server reported.
    status: null,
    // cursor is the sequence number of the last record the client received.
    // It is the cursor the server expects on the next read.
    cursor: 0,
    // cursorValid tells a fresh cursor from a cursor that still reads as
    // zero, so a replayed record is never mistaken for a new one.
    cursorValid: false,
    // records is the buffer of the retained records, oldest first.
    records: [],
    // renderedSeq is the sequence number of the last record the view
    // rendered. The records above it are still waiting for a flush.
    renderedSeq: 0,
    // level is the level filter, "all" or a minimum severity.
    level: 'all',
    // search is the raw search text, searchLower its folded counterpart.
    search: '',
    searchLower: '',
    // follow tells whether new records scroll into view.
    follow: true,
    // cleared marks a view the user emptied, so the empty state can say so.
    cleared: false,
    // source is the open event stream, null while there is none.
    source: null,
    // streamState is the state of the log feed: connecting, live,
    // reconnecting or polling.
    streamState: 'connecting',
    // reconnectDelay is the current backoff of the stream reconnect.
    reconnectDelay: RECONNECT_MIN,
    // reconnectTimer is the pending reconnect, null while there is none.
    reconnectTimer: null,
    // pollTimer is the pending fallback poll, null while there is none.
    pollTimer: null,
    // pollDelay is the current interval of the fallback poll. It grows
    // while the server does not answer.
    pollDelay: POLL_INTERVAL,
    // polling marks a poll that is already in flight.
    polling: false,
    // resyncing marks a re-sync that is already in flight.
    resyncing: false,
    // flushTimer is the pending DOM batch, null while there is none.
    flushTimer: null,
    // renderTimer is the pending filter re-render, null while there is none.
    renderTimer: null,
    // statusTimer is the status refresh interval.
    statusTimer: null,
    // statusError and logError hold the current failures. They are shown in
    // the error banner, which never blocks the view.
    statusError: '',
    logError: '',
    // loggedErrors holds the failures that were written to the console, so a
    // repeating failure is written once instead of on every poll.
    loggedErrors: new Set()
  };

  // =========================================
  // DOM Elements
  // =========================================

  const $ = (id) => document.getElementById(id);

  const el = {
    statusDot: $('statusDot'),
    statusLabel: $('statusLabel'),
    versionLabel: $('versionLabel'),
    serverUrl: $('serverUrl'),
    activeJobs: $('activeJobs'),
    queuedJobs: $('queuedJobs'),
    logFile: $('logFile'),
    errorBanner: $('errorBanner'),
    errorTitle: $('errorTitle'),
    errorText: $('errorText'),
    openUiButton: $('openUiButton'),
    openUiNote: $('openUiNote'),
    openLogsButton: $('openLogsButton'),
    copyButton: $('copyButton'),
    clearButton: $('clearButton'),
    levelFilter: $('levelFilter'),
    searchInput: $('searchInput'),
    followToggle: $('followToggle'),
    streamDot: $('streamDot'),
    streamLabel: $('streamLabel'),
    recordCount: $('recordCount'),
    logView: $('logView'),
    logBody: $('logBody'),
    emptyState: $('emptyState'),
    emptyTitle: $('emptyTitle'),
    emptyText: $('emptyText'),
    liveRegion: $('liveRegion'),
    toastContainer: $('toastContainer')
  };

  // =========================================
  // Small helpers
  // =========================================

  // fetchJSON performs a same origin request and returns the parsed body.
  //
  // A failure carries the HTTP status, so a caller can tell a missing
  // endpoint (404) from a server that could not be reached.
  async function fetchJSON(url, options) {
    const res = await fetch(url, options);
    const text = await res.text();

    let data = null;
    if (text) {
      try {
        data = JSON.parse(text);
      } catch (err) {
        data = null;
      }
    }

    if (!res.ok) {
      const message = (data && (data.error_detail?.message || data.error)) || `HTTP ${res.status}`;
      const error = new Error(message);
      error.status = res.status;
      error.payload = data;
      throw error;
    }

    return data;
  }

  // formatTime renders the timestamp of a record as a local clock time.
  // The raw timestamp stays available as the tooltip of the cell.
  function formatTime(value) {
    if (!value) return '--:--:--';

    const date = new Date(value);
    if (Number.isNaN(date.getTime())) {
      const raw = String(value);
      const sliced = raw.slice(11, 19);
      return sliced || raw;
    }

    const pad = (n) => String(n).padStart(2, '0');
    return `${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}`;
  }

  // formatAttrValue renders the value of an attribute as text.
  function formatAttrValue(value) {
    if (value === null || value === undefined) return '';
    if (typeof value === 'string') return value;
    if (typeof value === 'number' || typeof value === 'boolean') return String(value);
    try {
      return JSON.stringify(value);
    } catch (err) {
      return String(value);
    }
  }

  // attrEntries returns the attributes of a record as key/value pairs.
  function attrEntries(attrs) {
    if (!attrs || typeof attrs !== 'object') return [];
    return Object.keys(attrs).map((key) => [key, formatAttrValue(attrs[key])]);
  }

  // normalizeRecord turns a wire record into the shape the view uses.
  function normalizeRecord(raw) {
    if (!raw || typeof raw !== 'object') return null;

    const seq = Number(raw.seq);
    if (!Number.isFinite(seq)) return null;

    const level = String(raw.level || '').toUpperCase();
    const component = String(raw.component || '');
    const message = String(raw.message || '');
    const attrs = raw.attrs && typeof raw.attrs === 'object' ? raw.attrs : {};
    const attrsText = attrEntries(attrs).map(([key, value]) => `${key}=${value}`).join(' ');

    return {
      seq,
      time: String(raw.time || ''),
      level,
      component,
      message,
      attrs,
      attrsText,
      // searchable holds the text the search matches against: the message,
      // the component and the attributes.
      searchable: `${message}\n${component}\n${attrsText}`.toLowerCase()
    };
  }

  // =========================================
  // View state
  // =========================================

  // loadViewState restores the view state of the window. A storage that is
  // not available is not an error: the view then starts with the defaults.
  function loadViewState() {
    try {
      const raw = sessionStorage.getItem(STORAGE_KEY);
      if (!raw) return;
      const saved = JSON.parse(raw);
      if (!saved || typeof saved !== 'object') return;

      if (typeof saved.level === 'string' && (saved.level === 'all' || FILTER_RANK[saved.level] !== undefined)) {
        state.level = saved.level;
      }
      if (typeof saved.search === 'string') {
        state.search = saved.search;
        state.searchLower = saved.search.toLowerCase();
      }
      if (typeof saved.follow === 'boolean') {
        state.follow = saved.follow;
      }
    } catch (err) {
      // The storage is optional, the view works without it.
    }
  }

  // saveViewState stores the view state of the window.
  function saveViewState() {
    try {
      sessionStorage.setItem(STORAGE_KEY, JSON.stringify({
        level: state.level,
        search: state.search,
        follow: state.follow
      }));
    } catch (err) {
      // The storage is optional, the view works without it.
    }
  }

  // applyViewState writes the view state into the controls.
  function applyViewState() {
    el.levelFilter.value = state.level;
    el.searchInput.value = state.search;
    el.followToggle.checked = state.follow;
  }

  // =========================================
  // Status
  // =========================================

  // refreshStatus reads the runtime status of the server.
  async function refreshStatus() {
    try {
      const data = await fetchJSON('/api/runtime');
      state.status = data && typeof data === 'object' ? data : {};
      state.statusError = '';
      applyStatus();
      renderError();
    } catch (err) {
      state.statusError = `The runtime status could not be read: ${err.message}`;
      applyStatusFailure();
      renderError();
      logErrorOnce(state.statusError);
    }
  }

  // applyStatus renders the runtime status.
  function applyStatus() {
    const status = state.status || {};
    const ok = status.status === 'ok';

    setIndicator(el.statusDot, ok ? 'connected' : 'error');
    el.statusLabel.textContent = ok ? 'Running' : (status.status || 'Unknown');
    el.versionLabel.textContent = status.version ? `Version ${status.version}` : 'Version —';

    el.serverUrl.textContent = status.url || '—';
    if (status.address) el.serverUrl.title = `Bound to ${status.address}`;

    const running = Number(status.activeJobs?.running) || 0;
    const queued = Number(status.activeJobs?.queued) || 0;
    el.activeJobs.textContent = String(running);
    if (queued > 0) {
      el.queuedJobs.textContent = `${queued} queued`;
      el.queuedJobs.hidden = false;
    } else {
      el.queuedJobs.hidden = true;
    }

    const logFile = status.logFile || '';
    el.logFile.textContent = logFile || 'No log file';
    el.openLogsButton.disabled = !logFile;
    el.openLogsButton.title = logFile
      ? `Open the folder of ${logFile}`
      : 'The application does not write a log file';
  }

  // applyStatusFailure renders the state of a server that did not answer.
  function applyStatusFailure() {
    setIndicator(el.statusDot, 'error');
    el.statusLabel.textContent = 'Offline';
  }

  // setIndicator colors a status dot of the design system.
  //
  // "connected" is the green state of .status-indicator, "error" is the red
  // state of the runtime view, and the plain state is the amber one.
  function setIndicator(dot, mode) {
    dot.classList.remove('connected', 'runtime-status-indicator');
    if (mode === 'connected') {
      dot.classList.add('connected');
    } else if (mode === 'error') {
      dot.classList.add('runtime-status-indicator', 'is-error');
    }
  }

  // =========================================
  // Log records
  // =========================================

  // ingest takes the records of a page or of an event and adds the ones the
  // client does not know yet.
  function ingest(list) {
    let added = 0;

    for (const raw of list) {
      const record = normalizeRecord(raw);
      if (!record) continue;

      // A replay after a reconnect sends the records the client already
      // has. They are skipped, so no record is shown twice.
      if (state.cursorValid && record.seq <= state.cursor) continue;

      state.cursor = record.seq;
      state.cursorValid = true;
      state.records.push(record);
      added += 1;
    }

    if (!added) return;

    state.cleared = false;
    scheduleFlush();
  }

  // trimBuffer drops the oldest records when the buffer is full.
  //
  // The buffer is trimmed in one step, so the cost does not depend on the
  // number of records that arrive.
  function trimBuffer() {
    if (state.records.length <= MAX_RECORDS) return;
    state.records.splice(0, state.records.length - MAX_RECORDS);
  }

  // matchesFilter tells whether a record belongs to the current view.
  function matchesFilter(record) {
    if (!levelPasses(record.level)) return false;
    if (!state.searchLower) return true;
    return record.searchable.includes(state.searchLower);
  }

  // levelPasses applies the level filter. The filter is a minimum
  // severity: "warn" shows the warn and the error records.
  function levelPasses(level) {
    if (state.level === 'all') return true;

    const rank = LEVEL_RANK[level];
    if (rank === undefined) return true;

    return rank >= FILTER_RANK[state.level];
  }

  // buildRow builds the row of a record.
  function buildRow(record) {
    const row = document.createElement('div');
    row.className = 'runtime-log-row';
    row.dataset.seq = String(record.seq);

    const time = document.createElement('span');
    time.className = 'runtime-time';
    time.textContent = formatTime(record.time);
    if (record.time) time.title = record.time;

    const level = document.createElement('span');
    level.className = `runtime-level badge ${LEVEL_BADGE[record.level] || 'badge-secondary'}`;
    level.textContent = record.level || 'INFO';

    const component = document.createElement('span');
    component.className = 'runtime-component';
    component.textContent = record.component || '-';

    const message = document.createElement('span');
    message.className = 'runtime-message';
    message.textContent = record.message || '';

    const attrs = document.createElement('span');
    attrs.className = 'runtime-attrs';
    for (const [key, value] of attrEntries(record.attrs)) {
      const chip = document.createElement('span');
      chip.className = 'job-chip';
      chip.textContent = `${key}=${value}`;
      attrs.appendChild(chip);
    }

    row.append(time, level, component, message, attrs);
    return row;
  }

  // scheduleFlush batches the DOM updates.
  function scheduleFlush() {
    if (state.flushTimer) return;
    state.flushTimer = setTimeout(() => {
      state.flushTimer = null;
      flush();
    }, FLUSH_INTERVAL);
  }

  // flush renders the records that arrived since the last flush.
  //
  // The rows are built in a single fragment and appended once. The rows
  // that fell out of the buffer are removed in one step, so the tree is
  // touched once per flush and not once per record.
  function flush() {
    trimBuffer();
    const records = state.records;

    // The rows that fell out of the buffer are detached. They are moved
    // into a fragment, so the live tree is touched once.
    const oldest = records.length ? records[0].seq : 0;
    if (el.logBody.firstChild && Number(el.logBody.firstChild.dataset.seq) < oldest) {
      const dropped = document.createDocumentFragment();
      while (el.logBody.firstChild && Number(el.logBody.firstChild.dataset.seq) < oldest) {
        dropped.appendChild(el.logBody.firstChild);
      }
    }

    const fragment = document.createDocumentFragment();
    let added = 0;
    let rendered = state.renderedSeq;

    for (let i = 0; i < records.length; i += 1) {
      const record = records[i];
      if (record.seq <= state.renderedSeq) continue;

      if (!matchesFilter(record)) {
        // A record the filter hides is accounted for, so it is not looked
        // at again on the next flush.
        rendered = record.seq;
        continue;
      }

      if (added >= MAX_ROWS_PER_FLUSH) break;

      fragment.appendChild(buildRow(record));
      rendered = record.seq;
      added += 1;
    }

    if (added) el.logBody.appendChild(fragment);
    state.renderedSeq = rendered;

    updateCounts();
    updateEmptyState();
    if (state.follow) scrollToBottom();
  }

  // renderAll rebuilds the view from the buffer. It runs when the filters
  // change, not when a record arrives.
  function renderAll() {
    trimBuffer();
    const records = state.records;

    const fragment = document.createDocumentFragment();
    for (const record of records) {
      if (!matchesFilter(record)) continue;
      fragment.appendChild(buildRow(record));
    }

    el.logBody.textContent = '';
    el.logBody.appendChild(fragment);

    // Everything in the buffer is on the screen now.
    state.renderedSeq = records.length ? records[records.length - 1].seq : state.renderedSeq;

    updateCounts();
    updateEmptyState();
    if (state.follow) scrollToBottom();
  }

  // scheduleRender delays a re-render of the view.
  function scheduleRender() {
    if (state.renderTimer) clearTimeout(state.renderTimer);
    state.renderTimer = setTimeout(() => {
      state.renderTimer = null;
      renderAll();
    }, RENDER_DEBOUNCE);
  }

  // countShown returns the number of records the current filter shows.
  function countShown() {
    let count = 0;
    for (const record of state.records) {
      if (matchesFilter(record)) count += 1;
    }
    return count;
  }

  // updateCounts renders the number of records of the view.
  function updateCounts() {
    const total = state.records.length;
    const shown = countShown();

    if (total === 0) {
      el.recordCount.textContent = '0 records';
    } else if (shown === total) {
      el.recordCount.textContent = `${total} record${total === 1 ? '' : 's'}`;
    } else {
      el.recordCount.textContent = `${shown} of ${total} records`;
    }
  }

  // updateEmptyState renders the empty state of the log area.
  function updateEmptyState() {
    const shown = countShown();
    if (shown > 0) {
      el.emptyState.hidden = true;
      return;
    }

    el.emptyState.hidden = false;

    if (state.records.length === 0) {
      el.emptyTitle.textContent = state.cleared ? 'The view was cleared' : 'No log records yet';
      el.emptyText.textContent = state.cleared
        ? 'The rows of this window were cleared. New records keep arriving.'
        : 'The runtime log appears here as the server writes it.';
    } else {
      el.emptyTitle.textContent = 'No records match the filter';
      el.emptyText.textContent = 'Change the level filter or the search to see the other records.';
    }
  }

  // scrollToBottom moves the viewport to the newest record.
  function scrollToBottom() {
    el.logView.scrollTop = el.logView.scrollHeight;
  }

  // =========================================
  // Reading the log
  // =========================================

  // readLogs asks the server for the records after a cursor.
  async function readLogs(after, limit) {
    const data = await fetchJSON(`/api/runtime/logs?after=${encodeURIComponent(after)}&limit=${encodeURIComponent(limit)}`);
    const records = Array.isArray(data?.records) ? data.records : [];
    const cursor = Number(data?.cursor);
    const truncated = data?.truncated === true;

    return { records, cursor: Number.isFinite(cursor) ? cursor : null, truncated };
  }

  // loadInitial reads the newest window of the log.
  //
  // The window starts at the newest records, so the view shows the recent
  // activity instead of the oldest records the server still holds.
  async function loadInitial() {
    const head = Number(state.status?.logCursor) || 0;
    const after = head > PAGE_SIZE ? head - PAGE_SIZE : 0;

    try {
      const page = await readLogs(after, PAGE_SIZE);
      state.records = [];
      state.renderedSeq = 0;
      state.cursor = 0;
      state.cursorValid = false;
      ingest(page.records);
      if (page.cursor !== null && page.cursor > state.cursor) {
        state.cursor = page.cursor;
        state.cursorValid = true;
      }
      state.logError = '';
      renderError();
      renderAll();
    } catch (err) {
      state.logError = `The log could not be read: ${err.message}`;
      renderError();
      logErrorOnce(state.logError);
    }
  }

  // resync re-reads the newest window of the log.
  //
  // A gap in the history cannot be repaired, so the view starts again from
  // the records the server still holds and the user is told about it.
  async function resync(reason) {
    if (state.resyncing) return;
    state.resyncing = true;

    try {
      await refreshStatus();

      const head = Number(state.status?.logCursor) || 0;
      const after = head > PAGE_SIZE ? head - PAGE_SIZE : 0;
      const page = await readLogs(after, PAGE_SIZE);

      state.records = [];
      state.renderedSeq = 0;
      state.cursor = 0;
      state.cursorValid = false;
      ingest(page.records);
      if (page.cursor !== null && page.cursor > state.cursor) {
        state.cursor = page.cursor;
        state.cursorValid = true;
      }

      renderAll();
      state.logError = '';
      renderError();

      if (reason) {
        showToast(reason, 'error');
        announce(reason);
      }
    } catch (err) {
      state.logError = `The log could not be re-synced: ${err.message}`;
      renderError();
      logErrorOnce(state.logError);
    } finally {
      state.resyncing = false;
    }
  }

  // resyncAfterGap recovers the records a gap or a drop left out.
  async function resyncAfterGap(reason) {
    if (state.resyncing) return;
    state.resyncing = true;

    try {
      const page = await readLogs(state.cursor, PAGE_SIZE);

      if (page.truncated) {
        state.resyncing = false;
        await resync(reason);
        return;
      }

      ingest(page.records);
      if (page.cursor !== null && page.cursor > state.cursor) {
        state.cursor = page.cursor;
        state.cursorValid = true;
      }

      state.logError = '';
      renderError();
      showToast(reason, 'error');
      announce(reason);
    } catch (err) {
      state.logError = `The log could not be re-synced: ${err.message}`;
      renderError();
      logErrorOnce(state.logError);
    } finally {
      state.resyncing = false;
    }
  }

  // =========================================
  // Event stream
  // =========================================

  // connectStream opens the log stream.
  function connectStream() {
    closeStream();

    if (typeof window.EventSource !== 'function') {
      startPolling();
      return;
    }

    let source;
    try {
      source = new EventSource(`/api/runtime/logs/stream?after=${encodeURIComponent(state.cursor)}`);
    } catch (err) {
      startPolling();
      return;
    }

    state.source = source;

    // The poll keeps the view working while the stream is down. The label
    // then shows the feed the user really gets, not the reconnect attempt.
    if (!state.pollTimer) setStreamState('connecting');

    source.onopen = () => {
      if (state.source !== source) return;
      state.reconnectDelay = RECONNECT_MIN;
      setStreamState('live');
      stopPolling();
    };

    source.addEventListener('log', (event) => {
      if (state.source !== source) return;
      const record = parseEvent(event.data);
      if (record) ingest([record]);
    });

    source.addEventListener('dropped', (event) => {
      if (state.source !== source) return;
      const info = parseEvent(event.data) || {};
      const missed = Number(info.dropped) || 0;
      resyncAfterGap(missed > 0
        ? `Missed ${missed} log record${missed === 1 ? '' : 's'}, re-syncing`
        : 'Some log records were missed, re-syncing');
    });

    source.addEventListener('gap', () => {
      if (state.source !== source) return;
      resyncAfterGap('The log history had a gap, re-syncing');
    });

    source.onerror = () => {
      if (state.source !== source) return;
      handleStreamFailure();
    };
  }

  // closeStream closes the open event stream.
  function closeStream() {
    if (!state.source) return;
    const source = state.source;
    state.source = null;
    source.onopen = null;
    source.onerror = null;
    source.close();
  }

  // handleStreamFailure keeps the view alive when the stream drops.
  function handleStreamFailure() {
    closeStream();

    // The poll keeps the view working while the stream is down.
    startPolling();
    scheduleReconnect();
  }

  // scheduleReconnect reconnects the stream with the last known cursor.
  function scheduleReconnect() {
    if (state.reconnectTimer) return;

    const delay = state.reconnectDelay;
    state.reconnectDelay = Math.min(state.reconnectDelay * 2, RECONNECT_MAX);

    state.reconnectTimer = setTimeout(() => {
      state.reconnectTimer = null;
      connectStream();
    }, delay);
  }

  // parseEvent parses the payload of an event.
  function parseEvent(raw) {
    if (!raw) return null;
    try {
      return JSON.parse(raw);
    } catch (err) {
      return null;
    }
  }

  // setStreamState renders the state of the log feed.
  function setStreamState(mode) {
    state.streamState = mode;

    if (mode === 'live') {
      setIndicator(el.streamDot, 'connected');
      el.streamLabel.textContent = 'Live';
    } else if (mode === 'connecting') {
      setIndicator(el.streamDot, 'pending');
      el.streamLabel.textContent = 'Connecting…';
    } else if (mode === 'polling') {
      setIndicator(el.streamDot, 'pending');
      el.streamLabel.textContent = 'Polling…';
    } else {
      setIndicator(el.streamDot, 'error');
      el.streamLabel.textContent = 'Offline';
    }
  }

  // =========================================
  // Fallback polling
  // =========================================

  // startPolling reads the log with a poll while the stream is not
  // available.
  function startPolling() {
    if (state.pollTimer) {
      setStreamState('polling');
      return;
    }

    setStreamState('polling');
    pollOnce();
  }

  // stopPolling stops the fallback poll.
  function stopPolling() {
    if (!state.pollTimer) return;
    clearTimeout(state.pollTimer);
    state.pollTimer = null;
  }

  // schedulePoll queues the next poll of the fallback.
  function schedulePoll(delay) {
    if (state.pollTimer) return;
    state.pollTimer = setTimeout(() => {
      state.pollTimer = null;
      pollOnce();
    }, delay);
  }

  // pollOnce reads the records that arrived since the last cursor.
  async function pollOnce() {
    if (state.polling) return;
    state.polling = true;

    let truncated = false;

    try {
      const page = await readLogs(state.cursor, PAGE_SIZE);

      if (page.truncated) {
        // The window of the cursor fell out of the ring: the history has a
        // hole and cannot be continued.
        truncated = true;
      } else {
        ingest(page.records);
        if (page.cursor !== null && page.cursor > state.cursor) {
          state.cursor = page.cursor;
          state.cursorValid = true;
        }

        state.logError = '';
        renderError();
        state.pollDelay = POLL_INTERVAL;
        if (!state.source) setStreamState('polling');
      }
    } catch (err) {
      state.logError = `The log could not be read: ${err.message}`;
      renderError();
      logErrorOnce(state.logError);

      // A server that does not answer is polled less and less often.
      state.pollDelay = Math.min(state.pollDelay * 2, POLL_MAX_INTERVAL);
      if (!state.source) setStreamState('offline');
    } finally {
      state.polling = false;
    }

    if (truncated) {
      await resync('The server dropped older log records, re-syncing');
    }

    if (!state.source) schedulePoll(state.pollDelay);
  }

  // =========================================
  // Actions
  // =========================================

  // clearView empties the rows of the window.
  //
  // Nothing is deleted on disk and the server keeps its log: only the rows
  // of this window are removed.
  function clearView() {
    state.records = [];
    state.cleared = true;

    renderAll();
    announce('The log view was cleared');
    showToast('Cleared the log view. Nothing was deleted on disk.', 'success');
  }

  // formatLine renders a record as one line of text.
  function formatLine(record) {
    const parts = [
      formatTime(record.time),
      (record.level || '').padEnd(5, ' '),
      (record.component || '-').padEnd(10, ' '),
      record.message || ''
    ];
    if (record.attrsText) parts.push(record.attrsText);
    return parts.join('  ');
  }

  // copyView copies the visible rows to the clipboard.
  async function copyView() {
    const lines = [];

    for (const record of state.records) {
      // A record that is not on the screen yet is not copied: the clipboard
      // holds the rows the user sees.
      if (record.seq > state.renderedSeq) break;
      if (!matchesFilter(record)) continue;
      lines.push(formatLine(record));
    }

    if (!lines.length) {
      showToast('There is nothing to copy', 'error');
      announce('There is nothing to copy');
      return;
    }

    const text = lines.join('\n');

    try {
      await copyText(text);
      showToast(`Copied ${lines.length} record${lines.length === 1 ? '' : 's'}`, 'success');
      announce(`Copied ${lines.length} records to the clipboard`);
    } catch (err) {
      showToast(`Copy failed: ${err.message}`, 'error');
      announce('Copy failed');
    }
  }

  // copyText copies text to the clipboard.
  //
  // The clipboard API needs a secure context and a permission. When it is
  // missing or refused, the legacy copy command is used instead.
  async function copyText(text) {
    if (navigator.clipboard && typeof navigator.clipboard.writeText === 'function') {
      try {
        await navigator.clipboard.writeText(text);
        return;
      } catch (err) {
        // The clipboard refused the text: the legacy path is tried.
      }
    }

    legacyCopy(text);
  }

  // legacyCopy copies text with a hidden text area.
  function legacyCopy(text) {
    const area = document.createElement('textarea');
    area.value = text;
    area.setAttribute('readonly', 'readonly');
    area.style.position = 'fixed';
    area.style.top = '-1000px';
    area.style.left = '-1000px';
    area.style.opacity = '0';

    document.body.appendChild(area);
    area.select();
    if (typeof area.setSelectionRange === 'function') {
      area.setSelectionRange(0, text.length);
    }

    let copied = false;
    try {
      copied = document.execCommand('copy');
    } finally {
      document.body.removeChild(area);
    }

    if (!copied) throw new Error('the clipboard is not available');
  }

  // openUi opens the main HFDesk interface in the default browser.
  async function openUi() {
    el.openUiButton.disabled = true;

    try {
      await fetchJSON('/api/runtime/open-ui', { method: 'POST' });
      showToast('HFDesk was opened in your browser', 'success');
      el.openUiButton.disabled = false;
    } catch (err) {
      if (err.status === 404) {
        // The endpoint is not part of this build: the button is disabled
        // and the window says so, instead of failing on every click.
        el.openUiButton.title = 'This build of HFDesk does not provide the open-UI endpoint.';
        el.openUiNote.textContent = 'Opening the UI is not available in this build.';
        el.openUiNote.hidden = false;
        showToast('Opening the UI is not available in this build', 'error');
        announce('Opening the UI is not available in this build');
      } else {
        showToast(`HFDesk could not be opened: ${err.message}`, 'error');
        el.openUiButton.disabled = false;
      }
    }
  }

  // openLogs opens the folder of the active log file.
  async function openLogs() {
    el.openLogsButton.disabled = true;

    try {
      await fetchJSON('/api/runtime/open-logs', { method: 'POST' });
      showToast('The log folder was opened', 'success');
    } catch (err) {
      if (err.status === 404) {
        showToast('The application does not write a log file', 'error');
      } else {
        showToast(`The log folder could not be opened: ${err.message}`, 'error');
      }
    } finally {
      el.openLogsButton.disabled = !(state.status?.logFile);
    }
  }

  // =========================================
  // Feedback
  // =========================================

  // showToast shows a short message to the user.
  function showToast(message, type = 'info') {
    if (!el.toastContainer) return;

    const toast = document.createElement('div');
    toast.className = `toast ${type}`;

    const text = document.createElement('span');
    text.className = 'toast-message';
    text.textContent = message;

    toast.appendChild(text);
    el.toastContainer.appendChild(toast);

    setTimeout(() => {
      toast.style.animation = 'slideIn 0.3s ease reverse';
      setTimeout(() => toast.remove(), 300);
    }, 4000);
  }

  // announce reads a message to a screen reader.
  function announce(message) {
    if (!el.liveRegion) return;
    el.liveRegion.textContent = message;
  }

  // renderError shows the current failure in the error banner.
  function renderError() {
    const message = state.logError || state.statusError;
    if (!message) {
      el.errorBanner.hidden = true;
      el.errorText.textContent = '';
      return;
    }

    el.errorBanner.hidden = false;
    el.errorTitle.textContent = state.logError ? 'The log feed has a problem' : 'Cannot reach the HFDesk server';
    el.errorText.textContent = message;
  }

  // logErrorOnce writes a failure to the console once, so a repeating
  // failure does not fill the console.
  function logErrorOnce(message) {
    if (!message || state.loggedErrors.has(message)) return;
    if (state.loggedErrors.size > 20) state.loggedErrors.clear();
    state.loggedErrors.add(message);
    console.warn('[hfdesk runtime]', message);
  }

  // =========================================
  // Events
  // =========================================

  function wireEvents() {
    el.levelFilter.addEventListener('change', () => {
      state.level = el.levelFilter.value;
      saveViewState();
      renderAll();
    });

    el.searchInput.addEventListener('input', () => {
      state.search = el.searchInput.value;
      state.searchLower = state.search.toLowerCase();
      saveViewState();
      scheduleRender();
    });

    el.followToggle.addEventListener('change', () => {
      state.follow = el.followToggle.checked;
      saveViewState();
      if (state.follow) scrollToBottom();
    });

    el.copyButton.addEventListener('click', copyView);
    el.clearButton.addEventListener('click', clearView);
    el.openUiButton.addEventListener('click', openUi);
    el.openLogsButton.addEventListener('click', openLogs);

    window.addEventListener('pagehide', () => {
      closeStream();
      stopPolling();
      if (state.reconnectTimer) clearTimeout(state.reconnectTimer);
      if (state.flushTimer) clearTimeout(state.flushTimer);
      if (state.renderTimer) clearTimeout(state.renderTimer);
      if (state.statusTimer) clearInterval(state.statusTimer);
    });
  }

  // =========================================
  // Start
  // =========================================

  async function init() {
    loadViewState();
    applyViewState();
    wireEvents();
    updateCounts();
    updateEmptyState();

    await refreshStatus();
    await loadInitial();
    connectStream();

    state.statusTimer = setInterval(refreshStatus, STATUS_INTERVAL);
  }

  if (document.readyState === 'loading') {
    document.addEventListener('DOMContentLoaded', init);
  } else {
    init();
  }
})();
