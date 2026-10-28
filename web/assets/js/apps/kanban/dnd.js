// Pointer-based drag and drop for the board: cards between columns (vertical lists) and whole
// columns (one horizontal strip). Mouse and pen drag after a few pixels; touch drags after a
// long press (a quick swipe still scrolls). A floating "ghost" follows the pointer while a
// placeholder shows where the item will land; siblings slide out of the way (FLIP) and the
// board / column lists auto-scroll near their edges. Escape cancels.
//
// The engine only moves DOM placeholders; the caller decides what a drop means (onDrop) and
// re-renders. The geometry math lives in model.js (dropIndex, regionAt, autoScrollSpeed).

import { h } from '../../core/dom.js';
import { autoScrollSpeed, dropIndex, regionAt } from './model.js';

const INTERACTIVE = 'button, a[href], input, textarea, select, label, [contenteditable="true"], .kb-no-drag';
const reduceMotion = () => globalThis.matchMedia?.('(prefers-reduced-motion: reduce)').matches;

/**
 * @param {{
 *   root: HTMLElement,                    // delegation root
 *   itemSelector: string,                 // draggable items (carry data-id)
 *   handleSelector?: string,              // when set, drags start only on this part of an item
 *   axis: 'x'|'y',                        // list direction inside a container
 *   containers: () => Array<{id: string, el: HTMLElement, list: HTMLElement, scroll?: HTMLElement}>,
 *   scroller?: HTMLElement,               // horizontal board scroller (auto-scroll)
 *   canDrag?: (item: HTMLElement) => boolean,
 *   onStart?: (id: string) => void,
 *   onDrop: (drop: {id: string, containerId: string, beforeId: string|null, fromContainerId: string}) => void,
 *   onEnd?: () => void,
 *   longPress?: number,
 * }} o
 * @returns {{destroy: () => void, get active(): boolean}}
 */
export function createDnd(o) {
  const longPress = o.longPress ?? 320;
  let press = null; // pending press: {pointerId, item, x, y, type, timer}
  let drag = null; // active drag state

  function onPointerDown(e) {
    if (drag || press || e.button !== 0 || e.isPrimary === false) return;
    const item = e.target instanceof Element ? e.target.closest(o.itemSelector) : null;
    if (!item || !o.root.contains(item)) return;
    if (o.handleSelector && !e.target.closest(o.handleSelector)) return;
    const interactive = e.target.closest(INTERACTIVE);
    if (interactive && !interactive.classList.contains('kb-drag-handle')) return;
    if (o.canDrag && !o.canDrag(item)) return;
    press = { pointerId: e.pointerId, item, x: e.clientX, y: e.clientY, type: e.pointerType, timer: 0 };
    if (e.pointerType === 'touch') {
      press.timer = setTimeout(() => {
        if (!press) return;
        globalThis.navigator?.vibrate?.(8);
        begin(press.x, press.y);
      }, longPress);
      item.classList.add('kb-pressing');
    }
    document.addEventListener('pointermove', onPointerMove, { passive: false });
    document.addEventListener('pointerup', onPointerUp);
    document.addEventListener('pointercancel', onPointerCancel);
    document.addEventListener('touchmove', onTouchMove, { passive: false });
    document.addEventListener('contextmenu', onContextMenu, true);
  }

  // While a touch drag runs, the page must not scroll (or pan) under the finger.
  function onTouchMove(e) {
    if (drag) e.preventDefault();
  }

  function onContextMenu(e) {
    if (press?.type === 'touch' || drag) e.preventDefault();
  }

  function onPointerMove(e) {
    if (press && e.pointerId === press.pointerId && !drag) {
      const dist = Math.hypot(e.clientX - press.x, e.clientY - press.y);
      if (press.type === 'touch') {
        if (dist > 8) release(); // a swipe: let the browser scroll
        else { press.x = e.clientX; press.y = e.clientY; }
        return;
      }
      if (dist < 5) return;
      begin(press.x, press.y);
    }
    if (!drag || e.pointerId !== drag.pointerId) return;
    e.preventDefault();
    drag.x = e.clientX;
    drag.y = e.clientY;
    moveGhost();
    retarget();
  }

  function onPointerUp(e) {
    if (press && !drag) { release(); return; }
    if (!drag || e.pointerId !== drag.pointerId) return;
    finish(true);
  }

  function onPointerCancel(e) {
    if (press && !drag) { release(); return; }
    if (drag && e.pointerId === drag.pointerId) finish(false);
  }

  function onKeyDown(e) {
    if (drag && e.key === 'Escape') {
      e.preventDefault();
      e.stopPropagation();
      finish(false);
    }
  }

  function release() {
    if (!press) return;
    clearTimeout(press.timer);
    press.item.classList.remove('kb-pressing');
    press = null;
    if (!drag) detach();
  }

  function detach() {
    document.removeEventListener('pointermove', onPointerMove);
    document.removeEventListener('pointerup', onPointerUp);
    document.removeEventListener('pointercancel', onPointerCancel);
    document.removeEventListener('touchmove', onTouchMove);
    document.removeEventListener('contextmenu', onContextMenu, true);
  }

  function containerOf(item) {
    return o.containers().find((c) => c.list.contains(item)) || null;
  }

  function begin(x, y) {
    const item = press.item;
    const from = containerOf(item);
    if (!from) { release(); return; }
    const rect = item.getBoundingClientRect();
    const ghost = item.cloneNode(true);
    ghost.removeAttribute('id');
    ghost.classList.add('kb-ghost');
    ghost.classList.remove('kb-pressing');
    ghost.setAttribute('aria-hidden', 'true');
    ghost.style.width = `${rect.width}px`;
    ghost.style.height = `${rect.height}px`;
    ghost.style.left = `${rect.left}px`;
    ghost.style.top = `${rect.top}px`;
    const placeholder = h('div', { class: `kb-placeholder kb-placeholder-${o.axis}`, aria: { hidden: 'true' } });
    if (o.axis === 'y') placeholder.style.height = `${rect.height}px`;
    else { placeholder.style.width = `${rect.width}px`; placeholder.style.height = `${rect.height}px`; }
    item.after(placeholder);
    item.classList.add('kb-drag-source');
    document.body.appendChild(ghost);
    document.documentElement.classList.add('kb-dragging');
    drag = {
      pointerId: press.pointerId, item, id: item.dataset.id, from: from.id, ghost, placeholder,
      dx: x - rect.left, dy: y - rect.top, x, y, raf: 0, container: from,
    };
    clearTimeout(press.timer);
    item.classList.remove('kb-pressing');
    press = null;
    window.getSelection?.()?.removeAllRanges();
    document.addEventListener('keydown', onKeyDown, true);
    o.onStart?.(drag.id);
    moveGhost();
    drag.raf = requestAnimationFrame(tick);
  }

  function moveGhost() {
    drag.ghost.style.transform = `translate(${drag.x - drag.dx - parseFloat(drag.ghost.style.left)}px, ${drag.y - drag.dy - parseFloat(drag.ghost.style.top)}px) rotate(${o.axis === 'y' ? 2 : 1}deg)`;
  }

  function itemsOf(container) {
    return [...container.list.children].filter((el) => el.matches(o.itemSelector) && el !== drag.item);
  }

  // The next draggable sibling of el, skipping the (hidden) source item.
  function nextItem(el) {
    for (let x = el.nextElementSibling; x; x = x.nextElementSibling) {
      if (x !== drag?.item && x.matches(o.itemSelector)) return x;
    }
    return null;
  }

  // Place the placeholder where the pointer points: the nearest container across the list
  // direction, then the insertion index along it.
  function retarget() {
    const containers = o.containers();
    if (!containers.length) return;
    let target;
    if (o.axis === 'y') {
      const regions = containers.map((c) => {
        const r = c.el.getBoundingClientRect();
        return { start: r.left, size: r.width };
      });
      target = containers[regionAt(regions, drag.x)];
    } else {
      target = containers[0];
    }
    const items = itemsOf(target);
    const boxes = items.map((el) => {
      const r = el.getBoundingClientRect();
      return o.axis === 'y' ? { start: r.top, size: r.height } : { start: r.left, size: r.width };
    });
    const idx = dropIndex(boxes, o.axis === 'y' ? drag.y : drag.x);
    const ref = items[idx] || null;
    const cur = drag.placeholder;
    drag.container = target;
    if (cur.parentElement === target.list && nextItem(cur) === ref) return;
    const affected = new Set([cur.parentElement, target.list]);
    const before = snapshot(affected);
    if (ref) target.list.insertBefore(cur, ref);
    else appendAfterItems(target, cur);
    animate(before);
  }

  // Insert after the last item (lists may end with non-item elements such as composers).
  function appendAfterItems(container, el) {
    const items = [...container.list.children].filter((x) => x.matches(o.itemSelector));
    const last = items[items.length - 1];
    if (last) last.after(el);
    else container.list.prepend(el);
  }

  function snapshot(lists) {
    const m = new Map();
    if (reduceMotion()) return m;
    for (const list of lists) {
      if (!list) continue;
      for (const el of list.children) if (el.matches(o.itemSelector) && el !== drag.item) m.set(el, el.getBoundingClientRect());
    }
    return m;
  }

  function animate(before) {
    for (const [el, r] of before) {
      const n = el.getBoundingClientRect();
      const dx = r.left - n.left;
      const dy = r.top - n.top;
      if (dx || dy) el.animate?.([{ transform: `translate(${dx}px, ${dy}px)` }, { transform: 'none' }], { duration: 160, easing: 'cubic-bezier(0.2, 0.8, 0.2, 1)' });
    }
  }

  function tick() {
    if (!drag) return;
    let moved = false;
    if (o.scroller) {
      const r = o.scroller.getBoundingClientRect();
      const v = autoScrollSpeed(drag.x, r.left, r.right);
      if (v && canScroll(o.scroller, 'x', v)) { o.scroller.scrollLeft += v; moved = true; }
    }
    const sc = drag.container?.scroll;
    if (sc && o.axis === 'y') {
      const r = sc.getBoundingClientRect();
      if (drag.x >= r.left && drag.x <= r.right) {
        const v = autoScrollSpeed(drag.y, r.top, r.bottom, { edge: 48, max: 16 });
        if (v && canScroll(sc, 'y', v)) { sc.scrollTop += v; moved = true; }
      }
    }
    if (moved) retarget();
    drag.raf = requestAnimationFrame(tick);
  }

  function canScroll(el, axis, v) {
    if (axis === 'x') return v < 0 ? el.scrollLeft > 0 : el.scrollLeft < el.scrollWidth - el.clientWidth;
    return v < 0 ? el.scrollTop > 0 : el.scrollTop < el.scrollHeight - el.clientHeight;
  }

  function finish(commit) {
    const d = drag;
    drag = null;
    cancelAnimationFrame(d.raf);
    document.removeEventListener('keydown', onKeyDown, true);
    detach();
    const target = d.placeholder.parentElement ? (o.containers().find((c) => c.list === d.placeholder.parentElement) || null) : null;
    let beforeId = null;
    for (let el = d.placeholder.nextElementSibling; el; el = el.nextElementSibling) {
      if (el === d.item) continue;
      if (el.matches(o.itemSelector)) { beforeId = el.dataset.id; break; }
    }
    const end = () => {
      d.ghost.remove();
      d.placeholder.remove();
      d.item.classList.remove('kb-drag-source');
      document.documentElement.classList.remove('kb-dragging');
      o.onEnd?.();
    };
    // The click that follows the pointerup of a drag must not open the card under it.
    const swallow = (ev) => { ev.stopPropagation(); ev.preventDefault(); };
    window.addEventListener('click', swallow, true);
    setTimeout(() => window.removeEventListener('click', swallow, true), 0);
    const dropped = commit && target;
    // The source item is hidden; a placeholder right next to it is the original place.
    const unchanged = dropped && target.id === d.from && (d.placeholder.nextElementSibling === d.item || d.item.nextElementSibling === d.placeholder);
    if (dropped && !unchanged) {
      // Land the ghost on the placeholder, then let the caller re-render the board.
      const r = d.placeholder.getBoundingClientRect();
      if (!reduceMotion() && d.ghost.animate) {
        const to = `translate(${r.left - parseFloat(d.ghost.style.left)}px, ${r.top - parseFloat(d.ghost.style.top)}px)`;
        d.ghost.animate([{ transform: d.ghost.style.transform }, { transform: to }], { duration: 120, easing: 'ease-out', fill: 'forwards' });
      }
      try {
        o.onDrop({ id: d.id, containerId: target.id, beforeId, fromContainerId: d.from });
      } finally {
        setTimeout(end, reduceMotion() ? 0 : 110);
      }
      return;
    }
    end();
  }

  o.root.addEventListener('pointerdown', onPointerDown);
  return {
    destroy() {
      o.root.removeEventListener('pointerdown', onPointerDown);
      if (drag) finish(false);
      release();
      detach();
    },
    get active() {
      return !!drag || !!press;
    },
  };
}
