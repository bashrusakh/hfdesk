// Run through TestCacheDetailResizeUI: the shipped app.js is loaded in Node's
// VM with DOM stubs and driven through the real Repository Details renderer.
//
// Contract covered here: the grip and its accessible name exist only while
// resizing is available; a stored size is restored only when it fits the
// viewport; double-click clears the stored size; the clamp helper stays inside
// its bounds (including a viewport smaller than the nominal minimum); keyboard
// steps stay clamped; pointer capture is released and listeners are removed on
// pointerup/pointercancel.
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const vm = require('node:vm');

let assertions = 0;
function eq(actual, expected, message) { assertions++; assert.equal(actual, expected, message); }
function ok(value, message) { assertions++; assert.ok(value, message); }

const storage = new Map();
const localStorage = {
  getItem(key) { return storage.has(key) ? storage.get(key) : null; },
  setItem(key, value) { storage.set(key, String(value)); },
  removeItem(key) { storage.delete(key); }
};

let modalHTML = '';
const nodes = new Map();

function gripMarkupPresent() {
  return modalHTML.includes('cache-detail-resize-grip');
}

function makeGrip() {
  return {
    removed: false,
    handlers: {},
    pointerId: null,
    captured: false,
    classList: { add() {}, remove() {} },
    addEventListener(type, callback) { (this.handlers[type] ||= []).push(callback); },
    removeEventListener(type, callback) {
      this.handlers[type] = (this.handlers[type] || []).filter(item => item !== callback);
    },
    setPointerCapture(id) { this.pointerId = id; this.captured = true; },
    hasPointerCapture(id) { return this.captured && this.pointerId === id; },
    releasePointerCapture(id) { if (this.pointerId === id) this.captured = false; },
    fire(type, event) { [...(this.handlers[type] || [])].forEach(callback => callback(event)); },
    remove() {
      this.removed = true;
      this.captured = false;
      modalHTML = modalHTML.replace(/<button class="cache-detail-resize-grip"[\s\S]*?<\/button>/, '');
    }
  };
}

const grip = makeGrip();

const detailsHost = {
  querySelector(selector) {
    if (selector !== '.cache-detail-resize-grip') return null;
    return !grip.removed && gripMarkupPresent() ? grip : null;
  },
  insertAdjacentHTML(_position, html) {
    modalHTML += html;
    if (html.includes('cache-detail-resize-grip')) grip.removed = false;
  }
};

const modalBody = {
  handlers: {},
  addEventListener(type, callback) { (this.handlers[type] ||= []).push(callback); },
  dispatch(type, event) { [...(this.handlers[type] || [])].forEach(callback => callback(event)); },
  querySelector(selector) {
    if (selector === '.cache-detail-modal') return modalHTML.includes('class="cache-detail-modal"') ? detailsHost : null;
    if (selector === '.cache-detail-resize-grip') return !grip.removed && gripMarkupPresent() ? grip : null;
    return null;
  },
  set innerHTML(value) {
    modalHTML = String(value);
    grip.removed = false;
  },
  get innerHTML() { return modalHTML; }
};
nodes.set('#modalBody', modalBody);
nodes.set('#modalTitle', { textContent: '' });
nodes.set('#modalBackdrop', {
  classList: {
    active: false,
    add() { this.active = true; },
    remove() { this.active = false; },
    contains(name) { return name === 'active' && this.active; }
  },
  addEventListener() {}
});

const window = {
  innerWidth: 1280,
  innerHeight: 800,
  coarsePointer: false,
  location: { protocol: 'http:' },
  localStorage,
  resizeHandlers: [],
  addEventListener(type, callback) { if (type === 'resize') this.resizeHandlers.push(callback); },
  dispatchResize() { this.resizeHandlers.forEach(callback => callback()); },
  matchMedia(query) { return { matches: query.includes('coarse') ? this.coarsePointer : false }; }
};

const modal = {
  style: {},
  getBoundingClientRect() {
    const style = this.style;
    const width = parseFloat(style.width) || 600;
    const height = parseFloat(style.height) || 704;
    const fixed = style.position === 'fixed';
    return {
      width,
      height,
      left: fixed ? (parseFloat(style.left) || 0) : (window.innerWidth - width) / 2,
      top: fixed ? (parseFloat(style.top) || 0) : (window.innerHeight - height) / 2
    };
  }
};
nodes.set('#modal', modal);

const context = {
  window,
  console,
  URL,
  URLSearchParams,
  encodeURIComponent,
  localStorage,
  setTimeout,
  clearTimeout,
  getComputedStyle: () => ({ paddingLeft: '40px', paddingRight: '40px', paddingTop: '40px', paddingBottom: '40px' }),
  document: {
    readyState: 'loading',
    body: { style: {} },
    addEventListener() {},
    querySelector: selector => nodes.get(selector) || null,
    querySelectorAll: () => [],
    createElement() {
      return {
        textContent: '',
        get innerHTML() {
          return String(this.textContent).replace(/&/g, '&amp;').replace(/</g, '&lt;').replace(/>/g, '&gt;');
        }
      };
    }
  },
  fetch: async rawPath => {
    if (rawPath.startsWith('/api/cache/')) {
      return {
        ok: true,
        json: async () => ({
          repo: 'org/data', type: 'dataset', owner: 'org', name: 'data', files: [],
          path: '/datasets/data', sizeHuman: '1 GB', fileCount: 0, source: 'HF cache',
          commit: 'abcdef1234567890'
        })
      };
    }
    return { ok: true, json: async () => ({}) };
  }
};

let source = fs.readFileSync(path.join(__dirname, '..', '..', 'assets', 'static', 'js', 'app.js'), 'utf8');
const marker = '  // Start\n';
if (!source.includes(marker)) {
  console.error('cache-detail-resize-ui: app.js injection marker is missing');
  process.exit(1);
}
const hook = `
  window.test = {
    showCacheDetails: window.showCacheDetails,
    hideModal: window.hideModal,
    initModal,
    cacheDetailClampSize,
    cacheDetailResizeBounds,
    cacheDetailResizeEnabled,
    storageKey: CACHE_DETAIL_SIZE_STORAGE_KEY,
    state: cacheDetailResize
  };
`;
source = source.replace(marker, hook + '\n' + marker);
vm.runInNewContext(source, context);
const ui = window.test;
ui.initModal();

const KEY = ui.storageKey;
const gripTarget = () => ({ closest: selector => selector === '.cache-detail-resize-grip' ? grip : null });

function storeSize(width, height, version = 1) {
  localStorage.setItem(KEY, JSON.stringify({ version, width, height }));
}
function storedSize() {
  const raw = localStorage.getItem(KEY);
  return raw ? JSON.parse(raw) : null;
}
function assertInside(value, min, max, label) {
  ok(value >= Math.min(min, max), `${label} is not below the effective minimum`);
  ok(value <= max, `${label} never exceeds the viewport ceiling`);
}
async function openDetails() {
  ui.hideModal();
  await ui.showCacheDetails('org/data', 'dataset');
}

(async () => {
  // Clamp helper: ordinary and degenerate viewport bounds, NaN input.
  const bounds = { minWidth: 520, maxWidth: 1200, minHeight: 420, maxHeight: 720 };
  for (const [width, height] of [[10, 10], [5000, 5000], [650, 650], [NaN, 300]]) {
    const size = ui.cacheDetailClampSize(width, height, bounds);
    assertInside(size.width, bounds.minWidth, bounds.maxWidth, `width ${width}`);
    assertInside(size.height, bounds.minHeight, bounds.maxHeight, `height ${height}`);
  }
  const tight = ui.cacheDetailClampSize(800, 900, { minWidth: 520, maxWidth: 300, minHeight: 420, maxHeight: 200 });
  eq(tight.width, 300, 'a viewport ceiling smaller than the minimum wins for width');
  eq(tight.height, 200, 'a viewport ceiling smaller than the minimum wins for height');

  // Live bounds: the 40px backdrop gutter on each side, the 520px minimum
  // width, and the 500px minimum height that keeps a usable contents viewport
  // below the fixed header, facts lines, and footer.
  const live = ui.cacheDetailResizeBounds();
  eq(live.minWidth, 520);
  eq(live.minHeight, 500);
  eq(live.maxWidth, 1200);
  eq(live.maxHeight, 720);

  // Grip exists with its accessible name at a fine-pointer desktop viewport.
  window.innerWidth = 1280;
  window.innerHeight = 800;
  window.coarsePointer = false;
  await openDetails();
  ok(gripMarkupPresent(), 'the details dialog renders the resize grip');
  ok(modalHTML.includes('aria-label="Resize dialog"'), 'the grip has the accessible name Resize dialog');
  eq(modalBody.querySelector('.cache-detail-resize-grip'), grip, 'the grip is reachable through the details host');
  eq(ui.cacheDetailResizeEnabled(), true, 'resizing is enabled at 1280px with a fine pointer');

  // A stored size restores when it fits; the restored dialog stays centered
  // until a drag freezes it.
  storeSize(900, 600);
  await openDetails();
  eq(modal.style.width, '900px', 'a stored size within the viewport is restored');
  eq(modal.style.height, '600px', 'the restored height is applied');
  ok(!modal.style.position, 'restoring a size does not freeze the dialog position');

  // A stored size that does not fit is ignored and the CSS default remains.
  storeSize(5000, 5000);
  await openDetails();
  ok(!modal.style.width, 'an oversized stored width is ignored');
  ok(!modal.style.height, 'an oversized stored height is ignored');

  // A stored size below the minimum or from a stale storage version is ignored.
  storeSize(300, 300);
  await openDetails();
  ok(!modal.style.width, 'a stored size below the minimum is ignored');
  storeSize(900, 600, 2);
  await openDetails();
  ok(!modal.style.width, 'an unknown storage version is ignored');

  // Below the narrow breakpoint the grip is absent, resizing is disabled, and
  // even a fitting stored size is not restored.
  window.innerWidth = 700;
  window.dispatchResize();
  eq(modalBody.querySelector('.cache-detail-resize-grip'), null, 'the open dialog drops the grip below 720px');
  ok(!gripMarkupPresent(), 'the grip markup is removed below the narrow breakpoint');
  eq(ui.cacheDetailResizeEnabled(), false, 'resizing is disabled at 700px');
  storeSize(600, 500);
  await openDetails();
  eq(modalBody.querySelector('.cache-detail-resize-grip'), null, 'a narrow open has no grip');
  ok(!modal.style.width, 'a narrow open ignores the stored size');
  ok(!modal.style.height, 'the narrow dialog keeps its 92dvh CSS height');

  // Widening the same open dialog restores resizing availability and the
  // stored size.
  window.innerWidth = 1280;
  window.dispatchResize();
  eq(modalBody.querySelector('.cache-detail-resize-grip'), grip, 'widening re-adds the grip');
  eq(modal.style.width, '600px', 'widening restores the fitting stored width');

  // Coarse pointers disable the grip and restore.
  window.coarsePointer = true;
  storeSize(900, 600);
  await openDetails();
  eq(modalBody.querySelector('.cache-detail-resize-grip'), null, 'a coarse pointer has no grip');
  ok(!gripMarkupPresent(), 'a coarse pointer omits the grip markup');
  eq(ui.cacheDetailResizeEnabled(), false, 'resizing is disabled for a coarse pointer');
  ok(!modal.style.width, 'a coarse pointer ignores the stored size');
  window.coarsePointer = false;

  // Double-click resets to the CSS default and clears the one stored key.
  storeSize(900, 600);
  await openDetails();
  eq(modal.style.width, '900px', 'the fixture starts from a restored size');
  modalBody.dispatch('dblclick', { target: gripTarget(), preventDefault() {} });
  eq(localStorage.getItem(KEY), null, 'double-click clears the stored size');
  ok(!modal.style.width, 'double-click resets the width to the default');
  ok(!modal.style.height, 'double-click resets the height to the default');
  ok(!modal.style.position, 'double-click re-centers the dialog');
  ok(gripMarkupPresent(), 'the grip remains available after a reset');

  // Keyboard steps: 16px, then 64px clamped by the viewport ceiling.
  window.innerWidth = 1280;
  window.innerHeight = 800;
  const preventDefault = () => { preventDefault.called = true; };
  modalBody.dispatch('keydown', { key: 'ArrowRight', shiftKey: false, target: gripTarget(), preventDefault });
  eq(modal.style.width, '616px', 'ArrowRight grows the dialog by 16px');
  ok(preventDefault.called, 'resize arrow keys prevent the default scroll');
  modalBody.dispatch('keydown', { key: 'ArrowDown', shiftKey: true, target: gripTarget(), preventDefault });
  eq(modal.style.height, '720px', 'Shift+ArrowDown is clamped to the viewport ceiling');
  eq(modal.style.position, 'fixed', 'the first keyboard resize anchors the top-left position');
  eq(storedSize().width, 616, 'keyboard resizing persists the width');
  eq(storedSize().height, 720, 'keyboard resizing persists the clamped height');

  // Pointer drag uses capture, applies both axes, persists on pointerup, and
  // removes every listener afterwards.
  localStorage.removeItem(KEY);
  await openDetails();
  const startX = 100;
  const startY = 100;
  modalBody.dispatch('pointerdown', {
    button: 0, pointerId: 7, clientX: startX, clientY: startY, target: gripTarget()
  });
  eq(modal.style.position, 'fixed', 'a drag freezes the dialog position');
  eq(grip.captured, true, 'a drag captures the pointer');
  grip.fire('pointermove', { pointerId: 7, clientX: startX + 120, clientY: startY + 60 });
  eq(modal.style.width, '720px', 'the corner drag changes width');
  eq(modal.style.height, '720px', 'the corner drag clamps height to the viewport ceiling');
  grip.fire('pointerup', { pointerId: 7 });
  eq(grip.captured, false, 'pointerup releases capture');
  eq(modalBody.querySelector('.cache-detail-resize-grip').handlers.pointermove.length, 0, 'pointermove is removed after pointerup');
  eq(modalBody.querySelector('.cache-detail-resize-grip').handlers.pointerup.length, 0, 'pointerup is removed after pointerup');
  eq(storedSize().width, 720, 'the drag persists the final width');
  eq(storedSize().height, 720, 'the drag persists the final height');
  const frozenWidth = modal.style.width;
  grip.fire('pointermove', { pointerId: 7, clientX: startX + 200, clientY: startY + 200 });
  eq(modal.style.width, frozenWidth, 'a move after cleanup does not resize again');

  // pointercancel follows the same cleanup path and still persists the size.
  modalBody.dispatch('pointerdown', {
    button: 0, pointerId: 9, clientX: 50, clientY: 50, target: gripTarget()
  });
  eq(grip.captured, true, 'a second drag captures the new pointer');
  grip.fire('pointercancel', { pointerId: 9 });
  eq(grip.captured, false, 'pointercancel releases capture');
  eq(modalBody.querySelector('.cache-detail-resize-grip').handlers.pointermove.length, 0, 'pointercancel removes pointermove');
  eq(ui.state.drag, null, 'pointercancel clears the drag state');
  eq(context.document.body.style.userSelect, '', 'pointercancel restores text selection');

  // A viewport shrink re-clamps the framed size and keeps the left edge on
  // screen without rewriting the persisted preference.
  eq(modal.style.width, '720px');
  window.innerWidth = 1000;
  window.innerHeight = 700;
  window.dispatchResize();
  eq(modal.style.width, '720px', 'a viewport that still fits the width keeps it');
  eq(modal.style.height, '620px', 'the height is clamped to the new viewport ceiling');
  eq(modal.style.left, '280px', 'the frozen left edge shifts until the dialog fits');
  eq(storedSize().width, 720, 'the viewport clamp does not rewrite the stored width');

  console.log(`PASS: Repository Details resizing (${assertions} assertions)`);
})().catch(error => { console.error(error); process.exitCode = 1; });
