// Virtualized lists for long track lists (Songs, playlists of up to 10 000 items, the queue):
// only the rows near the viewport exist in the DOM. Rows have a fixed height (CSS --mu-row-h),
// so every position is plain arithmetic — the visible range, scrolling a row into view and the
// drop position of a drag. The list scrolls with the page (or a given scroller element).
//
// Pure math (unit tested in web/tests/music/vlist.test.mjs): visibleRange, dropIndexAt,
// autoScrollStep, scrollDelta. DOM: createVirtualList, attachReorder.

import { h, cls } from '../../core/dom.js';

const clamp = (v, lo, hi) => Math.max(lo, Math.min(hi, v));

/**
 * Rows to render: those intersecting the viewport plus `overscan` on each side.
 * Coordinates are in one space (e.g. viewport pixels): listTop is the list's top edge.
 * @returns {{start: number, end: number}} end is exclusive
 */
export function visibleRange({ listTop, viewTop, viewBottom, rowHeight, count, overscan = 6 }) {
  if (!(count > 0) || !(rowHeight > 0) || !(viewBottom > viewTop)) return { start: 0, end: 0 };
  const first = Math.floor((viewTop - listTop) / rowHeight);
  const last = Math.ceil((viewBottom - listTop) / rowHeight);
  const start = clamp(first - overscan, 0, count);
  const end = clamp(last + overscan, 0, count);
  return { start, end: Math.max(start, end) };
}

/**
 * Insertion index (0..count) for a pointer `offsetY` pixels below the list's top: between the
 * two rows nearest to it.
 */
export function dropIndexAt(offsetY, rowHeight, count) {
  if (!(rowHeight > 0) || !(count > 0)) return 0;
  return clamp(Math.round(offsetY / rowHeight), 0, count);
}

/**
 * Auto-scroll speed (px per frame) while dragging near the edges of [top, bottom]: negative near
 * the top, positive near the bottom, faster closer to the edge, 0 elsewhere.
 */
export function autoScrollStep(y, top, bottom, { edge = 56, max = 18 } = {}) {
  if (!(bottom > top)) return 0;
  const e = Math.min(edge, (bottom - top) / 3);
  if (y < top + e) return -Math.ceil(max * Math.min(1, (top + e - y) / e));
  if (y > bottom - e) return Math.ceil(max * Math.min(1, (y - (bottom - e)) / e));
  return 0;
}

/**
 * How far to scroll so the row [rowTop, rowTop + rowHeight) is fully inside [viewTop, viewBottom]
 * ('nearest': the smallest move; 'center': centred). 0 when already visible (nearest).
 */
export function scrollDelta({ rowTop, rowHeight, viewTop, viewBottom, block = 'nearest' }) {
  if (block === 'center') return Math.round(rowTop + rowHeight / 2 - (viewTop + viewBottom) / 2);
  if (rowTop < viewTop) return Math.floor(rowTop - viewTop);
  if (rowTop + rowHeight > viewBottom) return Math.ceil(rowTop + rowHeight - viewBottom);
  return 0;
}

/**
 * The part of the window not covered by the sticky top bar and the bottom dock.
 * @returns {{top: number, bottom: number}}
 */
export function windowInsets() {
  const doc = typeof document !== 'undefined' ? document : null;
  const topbar = doc?.querySelector('.topbar');
  const dock = doc?.getElementById('dock');
  const top = topbar ? Math.max(0, topbar.getBoundingClientRect().bottom) : 0;
  const bottom = dock && !dock.hidden ? dock.getBoundingClientRect().top : window.innerHeight;
  return { top, bottom: Math.max(top + 1, Math.min(window.innerHeight, bottom)) };
}

/**
 * A virtualized list.
 * @param {{className?: string, label?: string, rowHeight?: number, rowHeightVar?: string, overscan?: number,
 *          scroller?: HTMLElement|null, render: (item: any, index: number) => HTMLElement,
 *          onRange?: (r: {start: number, end: number, count: number}) => void, role?: string}} o
 */
export function createVirtualList(o) {
  const overscan = o.overscan ?? 8;
  const scroller = o.scroller || null;
  const el = h('div', { class: cls('mu-vlist', o.className), role: o.role || 'list', aria: { label: o.label || undefined } });
  let items = [];
  let rowH = o.rowHeight || 52;
  const rows = new Map(); // index → row element
  let raf = 0;
  let rafTimer = 0;
  let range = { start: 0, end: 0 };
  let destroyed = false;
  let pendingAttach = false; // items set while detached: render once attached
  let focusAfterRender = null; // index of a focused row re-created by a forced render

  function measure() {
    if (o.rowHeight) return false;
    const v = parseFloat(getComputedStyle(el).getPropertyValue(o.rowHeightVar || '--mu-row-h'));
    if (v > 0 && v !== rowH) {
      rowH = v;
      return true;
    }
    return false;
  }

  function viewport() {
    if (scroller) {
      const r = scroller.getBoundingClientRect();
      return { top: r.top, bottom: r.bottom };
    }
    return windowInsets();
  }

  function place(row, i) {
    row.style.top = `${i * rowH}px`;
    row.dataset.index = String(i);
    row.setAttribute('aria-setsize', String(items.length));
    row.setAttribute('aria-posinset', String(i + 1));
  }

  function insertInOrder(row, i) {
    let after = null;
    for (const [j, r] of rows) if (j > i && (!after || j < after.j)) after = { j, r };
    if (after) el.insertBefore(row, after.r);
    else el.append(row);
  }

  function render(force = false) {
    raf = 0;
    if (destroyed) return;
    if (!el.isConnected) {
      if (force) clearRows();
      return;
    }
    if (measure()) force = true;
    el.style.height = `${items.length * rowH}px`;
    const vp = viewport();
    const r = el.getBoundingClientRect();
    const next = visibleRange({ listTop: r.top, viewTop: vp.top, viewBottom: vp.bottom, rowHeight: rowH, count: items.length, overscan });
    const active = document.activeElement;
    for (const [i, row] of rows) {
      const outside = i < next.start || i >= next.end || i >= items.length;
      // Keep a focused row while it is still a row of the list (keyboard users scroll with it).
      if (force || (outside && !(i < items.length && row.contains(active)))) {
        const hadFocus = row.contains(active);
        row.remove();
        rows.delete(i);
        if (hadFocus && force && i < items.length) focusAfterRender = i;
      }
    }
    for (let i = next.start; i < next.end; i++) {
      if (rows.has(i)) continue;
      const row = o.render(items[i], i);
      place(row, i);
      insertInOrder(row, i);
      rows.set(i, row);
    }
    range = next;
    pendingAttach = false;
    if (focusAfterRender !== null) {
      const f = rows.get(focusAfterRender);
      focusAfterRender = null;
      f?.focus({ preventScroll: true });
    }
    o.onRange?.({ ...next, count: items.length });
  }

  function clearRows() {
    for (const row of rows.values()) row.remove();
    rows.clear();
  }

  // requestAnimationFrame never fires in a background tab or a hidden pane: a timer backs it up.
  function schedule() {
    if (raf || destroyed) return;
    const run = () => {
      if (!raf) return;
      cancelAnimationFrame(raf);
      clearTimeout(rafTimer);
      render(false);
    };
    raf = requestAnimationFrame(run);
    rafTimer = setTimeout(run, 120);
  }

  /** Render as soon as the list is in the document (callers append it right after creating it). */
  function renderWhenAttached() {
    pendingAttach = true;
    let tries = 0;
    const attempt = () => {
      if (destroyed || !pendingAttach) return;
      if (el.isConnected) render(true);
      else if (tries++ < 20) setTimeout(attempt, tries < 3 ? 0 : 50);
    };
    queueMicrotask(attempt);
  }

  const target = scroller || window;
  const onScroll = () => schedule();
  const onResize = () => {
    if (measure()) render(true);
    else schedule();
  };
  target.addEventListener('scroll', onScroll, { passive: true });
  window.addEventListener('resize', onResize);
  // Content above the list changing size (a header image loading, a banner appearing) moves the
  // list without a scroll event: watch the page's size and re-render.
  const ro = typeof ResizeObserver === 'function' ? new ResizeObserver(() => schedule()) : null;
  if (ro && typeof document !== 'undefined') ro.observe(scroller || document.body);

  function scrollBy(dy) {
    if (!dy) return;
    if (scroller) scroller.scrollTop += dy;
    else window.scrollBy(0, dy);
  }

  const api = {
    el,
    get items() { return items; },
    get count() { return items.length; },
    get rowHeight() { return rowH; },
    get range() { return range; },
    /** Replace the items (rows re-render). */
    setItems(next) {
      items = Array.isArray(next) ? next : [];
      if (el.isConnected) render(true);
      else {
        clearRows();
        el.style.height = `${items.length * rowH}px`;
        renderWhenAttached();
      }
    },
    /** Re-render the visible rows now (e.g. after attaching the list). */
    refresh() {
      render(true);
    },
    /** Update the rendered rows in place. */
    forEachRow(fn) {
      for (const [i, row] of rows) fn(row, items[i], i);
    },
    /** The rendered row of index i, or null. */
    rowAt(i) {
      return rows.get(i) || null;
    },
    /** Scroll so row i is visible. */
    scrollToIndex(i, { block = 'nearest' } = {}) {
      if (!(i >= 0 && i < items.length)) return;
      measure();
      const vp = viewport();
      const r = el.getBoundingClientRect();
      scrollBy(scrollDelta({ rowTop: r.top + i * rowH, rowHeight: rowH, viewTop: vp.top, viewBottom: vp.bottom, block }));
      render(false);
    },
    /** Scroll to row i and focus it. */
    focusIndex(i, opts) {
      if (!(i >= 0 && i < items.length)) return null;
      api.scrollToIndex(i, opts);
      const row = rows.get(i);
      row?.focus({ preventScroll: true });
      return row || null;
    },
    /** Index under a viewport y coordinate (insertion index for drops). */
    insertionAt(clientY) {
      const r = el.getBoundingClientRect();
      return dropIndexAt(clientY - r.top, rowH, items.length);
    },
    viewport,
    scrollBy,
    destroy() {
      destroyed = true;
      cancelAnimationFrame(raf);
      clearTimeout(rafTimer);
      target.removeEventListener('scroll', onScroll);
      window.removeEventListener('resize', onResize);
      ro?.disconnect();
      clearRows();
    },
  };
  return api;
}

const reduceMotion = () => globalThis.matchMedia?.('(prefers-reduced-motion: reduce)').matches;

/**
 * Drag-to-reorder for a virtual list: drags start on `handleSelector` inside a row (mouse, pen
 * and touch — the handle has touch-action: none), a ghost follows the pointer, a line shows the
 * drop position, the page auto-scrolls near the edges and Escape cancels.
 * @param {ReturnType<typeof createVirtualList>} list
 * @param {{handleSelector?: string, canDrag?: (index: number) => boolean,
 *          onDrop: (from: number, insertion: number) => void, label?: (index: number) => string}} o
 * @returns {() => void} detach
 */
export function attachReorder(list, o) {
  const handleSel = o.handleSelector || '.mu-grip';
  let press = null;
  let drag = null;

  function onDown(e) {
    if (drag || press || e.button !== 0) return;
    const handle = e.target instanceof Element ? e.target.closest(handleSel) : null;
    const row = handle?.closest('[data-index]');
    if (!handle || !row || !list.el.contains(row)) return;
    const index = Number(row.dataset.index);
    if (!Number.isInteger(index) || (o.canDrag && !o.canDrag(index))) return;
    e.preventDefault();
    press = { x: e.clientX, y: e.clientY, index, row, pointerId: e.pointerId };
    document.addEventListener('pointermove', onMove, { passive: false });
    document.addEventListener('pointerup', onUp);
    document.addEventListener('pointercancel', onCancel);
  }

  function begin() {
    const { row, index } = press;
    const r = row.getBoundingClientRect();
    const ghost = row.cloneNode(true);
    ghost.removeAttribute('id');
    ghost.removeAttribute('data-index');
    ghost.classList.add('mu-drag-ghost');
    ghost.setAttribute('aria-hidden', 'true');
    ghost.style.width = `${r.width}px`;
    ghost.style.left = `${r.left}px`;
    ghost.style.top = `${r.top}px`;
    document.body.append(ghost);
    const line = h('div', { class: 'mu-drop-line', aria: { hidden: 'true' } });
    list.el.append(line);
    row.classList.add('is-drag-source');
    document.documentElement.classList.add('mu-dragging');
    drag = { index, ghost, line, dy: press.y - r.top, x: press.x, y: press.y, insertion: index, raf: 0, pointerId: press.pointerId };
    press = null;
    document.addEventListener('keydown', onKey, true);
    move();
    drag.raf = requestAnimationFrame(tick);
  }

  function move() {
    drag.ghost.style.transform = `translateY(${drag.y - drag.dy - parseFloat(drag.ghost.style.top)}px)`;
    const ins = list.insertionAt(drag.y);
    drag.insertion = ins;
    drag.line.style.top = `${ins * list.rowHeight}px`;
    drag.line.hidden = ins === drag.index || ins === drag.index + 1;
  }

  function tick() {
    if (!drag) return;
    const vp = list.viewport();
    const v = autoScrollStep(drag.y, vp.top, vp.bottom);
    if (v) {
      list.scrollBy(v);
      move();
    }
    drag.raf = requestAnimationFrame(tick);
  }

  function onMove(e) {
    if (press && !drag) {
      if (Math.hypot(e.clientX - press.x, e.clientY - press.y) < 4) return;
      begin();
    }
    if (!drag) return;
    e.preventDefault();
    drag.x = e.clientX;
    drag.y = e.clientY;
    move();
  }

  function onUp() {
    if (press && !drag) {
      cleanupPress();
      return;
    }
    finish(true);
  }

  function onCancel() {
    if (press && !drag) cleanupPress();
    else finish(false);
  }

  function onKey(e) {
    if (e.key === 'Escape' && drag) {
      e.preventDefault();
      e.stopPropagation();
      finish(false);
    }
  }

  function cleanupPress() {
    press = null;
    document.removeEventListener('pointermove', onMove);
    document.removeEventListener('pointerup', onUp);
    document.removeEventListener('pointercancel', onCancel);
  }

  function finish(commit) {
    const d = drag;
    drag = null;
    cleanupPress();
    if (!d) return;
    cancelAnimationFrame(d.raf);
    document.removeEventListener('keydown', onKey, true);
    d.line.remove();
    document.documentElement.classList.remove('mu-dragging');
    list.forEachRow((row) => row.classList.remove('is-drag-source'));
    const swallow = (ev) => { ev.stopPropagation(); ev.preventDefault(); };
    window.addEventListener('click', swallow, true);
    setTimeout(() => window.removeEventListener('click', swallow, true), 0);
    const ins = d.insertion;
    const changed = commit && ins !== d.index && ins !== d.index + 1;
    if (changed && !reduceMotion() && d.ghost.animate) {
      const r = list.el.getBoundingClientRect();
      const to = r.top + ins * list.rowHeight - (ins > d.index ? list.rowHeight : 0);
      d.ghost.animate([{ transform: d.ghost.style.transform }, { transform: `translateY(${to - parseFloat(d.ghost.style.top)}px)` }], { duration: 120, easing: 'ease-out', fill: 'forwards' });
      setTimeout(() => d.ghost.remove(), 120);
    } else {
      d.ghost.remove();
    }
    if (changed) o.onDrop(d.index, ins);
  }

  list.el.addEventListener('pointerdown', onDown);
  return () => {
    list.el.removeEventListener('pointerdown', onDown);
    if (drag) finish(false);
    cleanupPress();
  };
}
