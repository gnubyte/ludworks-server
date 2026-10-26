// TV library — shared view helpers: horizontal shelves, grids, headers, empty states.

import { h, cls } from '../../../../core/dom.js';
import { icon } from '../../../../core/icons.js';
import { isAbortError } from '../../../../core/api.js';

/**
 * A titled horizontal shelf with scroll buttons and an optional "See all" link.
 * @param {{title: string, href?: string, items: Node[], wide?: boolean, sub?: string}} o
 */
export function shelf({ title, href = '', items, wide = false, sub = '' }) {
  const track = h('div', { class: cls('tvl-shelf-track', { 'is-wide': wide }), role: 'list', aria: { label: title } }, items);
  const scrollBy = (dir) => track.scrollBy({ left: dir * Math.max(240, track.clientWidth * 0.85), behavior: 'smooth' });
  const prev = h('button', { type: 'button', class: 'btn btn-ghost btn-icon btn-sm', aria: { label: `Scroll ${title} left` }, onClick: () => scrollBy(-1) }, icon('chevron-left'));
  const next = h('button', { type: 'button', class: 'btn btn-ghost btn-icon btn-sm', aria: { label: `Scroll ${title} right` }, onClick: () => scrollBy(1) }, icon('chevron-right'));
  const sync = () => {
    prev.disabled = track.scrollLeft <= 2;
    next.disabled = track.scrollLeft + track.clientWidth >= track.scrollWidth - 2;
  };
  track.addEventListener('scroll', sync, { passive: true });
  requestAnimationFrame(sync);
  if (typeof ResizeObserver !== 'undefined') new ResizeObserver(sync).observe(track);
  return h('section', { class: 'tvl-shelf' },
    h('header', { class: 'tvl-shelf-head' },
      h('div', { class: 'tvl-shelf-heading' },
        h('h2', { class: 'tvl-shelf-title' }, href ? h('a', { href }, title, icon('chevron-right', { size: 16 })) : title),
        sub ? h('span', { class: 'tvl-shelf-sub' }, sub) : null),
      h('div', { class: 'tvl-shelf-nav' }, prev, next)),
    track);
}

/** Responsive grid of cards. */
export function grid(items = [], { wide = false, label = 'Items' } = {}) {
  return h('div', { class: cls('tvl-grid', { 'is-wide': wide }), role: 'list', aria: { label } }, items);
}

/** View header: title, subtitle and actions. */
export function viewHeader({ title, subtitle = '', actions = null, back = null }) {
  return h('header', { class: 'tvl-view-head' },
    h('div', { class: 'tvl-view-heading' },
      back ? h('a', { class: 'tvl-back', href: back.href }, icon('chevron-left', 14), back.label) : null,
      h('h1', { class: 'tvl-view-title' }, title),
      subtitle ? h('p', { class: 'tvl-view-sub' }, subtitle) : null),
    actions ? h('div', { class: 'tvl-view-actions' }, actions) : null);
}

/** Render an error into root unless the request was aborted. */
export function showError(root, ui, err, retry) {
  if (isAbortError(err)) return;
  root.replaceChildren(h('div', { class: 'tvl-state' }, ui.errorState(err, retry)));
}

/** The empty-library state with next steps. */
export function emptyLibrary(ui, { onAutoTag } = {}) {
  return h('div', { class: 'tvl-state' }, ui.emptyState({
    icon: 'film',
    title: 'Your library is empty',
    message: 'Upload movies and shows in Files — for example into “Movies/” and “Shows/Name/Season 1/”. Videos appear here automatically; auto-tag fills in titles, seasons and episodes.',
    action: h('div', { class: 'row' },
      ui.button({ label: 'Open Files', icon: 'folder', href: '/files' }),
      onAutoTag ? ui.button({ label: 'Auto-tag a folder', icon: 'sparkle', variant: 'primary', onClick: onAutoTag }) : null),
  }));
}
