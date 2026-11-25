/**
 * HFDesk - Log rendering
 *
 * The renderer of one log record: the wire record is normalized once, and
 * the row, the filter and the text of the copy action are built from it.
 *
 * The unit is pure rendering. It loads without touching the page, opens no
 * connection and names no external resource, so the window host may inline
 * the whole file into a document of its own. It exposes exactly one global,
 * window.HFDeskLogView, and js/runtime.js is its only consumer.
 */

(function (global) {
  'use strict';

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

  // levelPasses applies the level filter. The filter is a minimum
  // severity: "warn" shows the warn and the error records.
  function levelPasses(level, filter) {
    if (filter === 'all') return true;

    const rank = LEVEL_RANK[level];
    if (rank === undefined) return true;

    return rank >= FILTER_RANK[filter];
  }

  // isFilter tells whether a value is one of the level filters of a view.
  function isFilter(value) {
    return typeof value === 'string' && (value === 'all' || FILTER_RANK[value] !== undefined);
  }

  // matches tells whether a record belongs to a view. view carries the
  // level filter and the folded search text of the view.
  function matches(record, view) {
    if (!levelPasses(record.level, view.level)) return false;
    if (!view.searchLower) return true;
    return record.searchable.includes(view.searchLower);
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

  global.HFDeskLogView = {
    normalizeRecord,
    buildRow,
    matches,
    levelPasses,
    isFilter,
    formatTime,
    formatAttrValue,
    attrEntries,
    formatLine
  };
})(window);
