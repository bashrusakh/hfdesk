/**
 * HFDesk - Service menu
 *
 * The interaction of one service control: the trigger opens the menu of the
 * service actions, the keyboard moves over the items the user can act on,
 * Escape closes the menu and returns the focus, a click outside closes it,
 * and a resized viewport closes it instead of leaving it beside an element
 * that moved. The menu is anchored below its trigger and clamped into the
 * viewport, because the shell of the view clips its overflow.
 *
 * The unit owns the interaction only. The markup owns the control - one
 * trigger, the menu with its items, and the chip of a document that owns
 * none of the actions - and the caller owns what an activation means: every
 * enabled item reaches onAction(name) exactly once, and nothing here decides
 * which item is offered.
 *
 * The file is pure: it loads without touching the page, opens no connection
 * and names no external resource, so the window host may inline the whole
 * file into a document of its own. It exposes exactly one global,
 * window.HFDeskServiceControl.
 */

(function (global) {
  'use strict';

  // ITEM_SELECTOR finds the actions of a menu. The items are markup, in
  // their order; the menu never builds one.
  const ITEM_SELECTOR = '[role="menuitem"]';

  // itemName reads the action name an item carries. The attribute is the
  // one contract between the markup and this unit: an item without it is
  // inert, so a renamed id can never wire a stray click to an action.
  function itemName(item) {
    return item.getAttribute('data-service-action') || '';
  }

  // wire connects one control to its trigger and its menu.
  //
  // options carries the trigger, the menu, and onAction(name), which runs
  // one activation. The return value is the control the caller drives when
  // the state changes: open, close, toggle, setBusy and isOpen.
  function wire(options) {
    const trigger = options && options.trigger;
    const menu = options && options.menu;
    const onAction = options && typeof options.onAction === 'function'
      ? options.onAction
      : function () {};

    if (!trigger || !menu) return null;

    // doc is the document the control listens on. The trigger carries it
    // where the DOM offers it; the global is the same document of the page.
    const doc = trigger.ownerDocument ||
      (typeof document !== 'undefined' ? document : null);

    // busy marks a call in flight: the trigger waits for the answer and a
    // second activation never reaches the owner beside the first one.
    let busy = false;

    function items() {
      if (typeof menu.querySelectorAll !== 'function') return [];
      return Array.prototype.slice.call(menu.querySelectorAll(ITEM_SELECTOR));
    }

    function visibleItems() {
      return items().filter(function (item) { return !item.hidden; });
    }

    function actionableItems() {
      return items().filter(function (item) {
        return !item.hidden && !item.disabled;
      });
    }

    function focusEdge(edge) {
      const candidates = actionableItems();
      if (!candidates.length) return;
      const item = edge === 'last' ? candidates[candidates.length - 1] : candidates[0];
      item.focus();
    }

    function focusStep(step) {
      const candidates = actionableItems();
      if (!candidates.length) return;
      const current = candidates.indexOf(doc ? doc.activeElement : null);
      const next = current === -1
        ? (step > 0 ? 0 : candidates.length - 1)
        : (current + step + candidates.length) % candidates.length;
      candidates[next].focus();
    }

    // place puts the menu below the trigger and inside the viewport: the
    // trigger's viewport coordinate is the reliable anchor of a fixed menu,
    // and the clamp keeps a narrow window from cutting the menu off.
    function place() {
      if (typeof trigger.getBoundingClientRect !== 'function') return;

      const rect = trigger.getBoundingClientRect();
      const viewport = global.innerWidth || 0;
      if (!viewport) return;

      const width = menu.offsetWidth || 0;
      const margin = 8;
      const left = Math.min(Math.max(rect.left, margin), Math.max(viewport - width - margin, margin));

      menu.style.left = Math.round(left) + 'px';
      menu.style.top = Math.round(rect.bottom + 6) + 'px';
    }

    function isOpen() {
      return !menu.hidden;
    }

    function open(edge) {
      if (busy) return;
      if (!visibleItems().length) return;

      menu.hidden = false;
      place();
      trigger.setAttribute('aria-expanded', 'true');
      if (edge) focusEdge(edge);
    }

    function close(focusTrigger) {
      if (menu.hidden && trigger.getAttribute('aria-expanded') === 'false') return;

      menu.hidden = true;
      trigger.setAttribute('aria-expanded', 'false');
      if (focusTrigger) trigger.focus();
    }

    function toggle() {
      if (busy) return;
      if (isOpen()) {
        close(true);
        return;
      }
      open('first');
    }

    function setBusy(value) {
      busy = value === true;
      trigger.disabled = busy;
    }

    function activate(item) {
      if (busy || item.hidden || item.disabled) return;

      const name = itemName(item);
      if (!name) return;

      close(true);
      setBusy(true);

      let result;
      try {
        result = onAction(name);
      } catch (err) {
        setBusy(false);
        return;
      }

      // The caller reports the outcome of an action itself. The only duty
      // here is to release the trigger, and a refused action must not
      // leave a rejected promise behind.
      Promise.resolve(result).then(
        function () { setBusy(false); },
        function () { setBusy(false); }
      );
    }

    trigger.addEventListener('click', toggle);

    trigger.addEventListener('keydown', function (event) {
      if (event.key === 'ArrowDown' || event.key === 'ArrowUp') {
        event.preventDefault();
        open(event.key === 'ArrowDown' ? 'first' : 'last');
      } else if (event.key === 'Escape') {
        close(false);
      }
    });

    menu.addEventListener('keydown', function (event) {
      if (event.key === 'ArrowDown') {
        event.preventDefault();
        focusStep(1);
      } else if (event.key === 'ArrowUp') {
        event.preventDefault();
        focusStep(-1);
      } else if (event.key === 'Home') {
        event.preventDefault();
        focusEdge('first');
      } else if (event.key === 'End') {
        event.preventDefault();
        focusEdge('last');
      } else if (event.key === 'Escape') {
        event.preventDefault();
        close(true);
      } else if (event.key === 'Tab') {
        // The menu is not a dialog: the next tab stop belongs to the page.
        close(false);
      }
    });

    for (const item of items()) {
      item.addEventListener('click', function () { activate(item); });
    }

    // A click outside the control closes the menu; its own clicks never do.
    if (doc) {
      doc.addEventListener('click', function (event) {
        if (!isOpen()) return;
        const target = event.target;
        if (trigger.contains(target) || menu.contains(target)) return;
        close(false);
      });
    }

    // A resized viewport moves the anchor of the open menu.
    global.addEventListener('resize', function () { close(false); });

    return {
      open: open,
      close: close,
      toggle: toggle,
      setBusy: setBusy,
      isOpen: isOpen
    };
  }

  global.HFDeskServiceControl = { wire: wire };
})(window);
