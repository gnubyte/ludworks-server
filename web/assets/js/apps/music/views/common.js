// Building blocks shared by the Music views: page headers, sort controls, paged card grids,
// infinite track lists and a small cache that lets "back" return to a long list instantly.

import { h } from '../../../core/dom.js';
import { icon } from '../../../core/icons.js';
import * as ui from '../../../core/ui.js';
import { createTrackList, cardGrid, playTracks } from '../widgets.js';
import { errorText } from '../model.js';

/** Page header: title, optional subtitle and actions. */
export function pageHead(title, { subtitle = '', actions = [] } = {}) {
  return h('header', { class: 'mu-head' },
    h('div', { class: 'mu-head-text' }, h('h1', { class: 'mu-head-title' }, title), subtitle ? h('p', { class: 'mu-head-sub' }, subtitle) : null),
    actions.length ? h('div', { class: 'mu-head-actions' }, actions) : null);
}

/**
 * Sort select plus an ascending/descending toggle.
 * @param {{value: string, label: string, order: string}[]} options
 * @param {{sort: string, order: string}} value
 * @param {(v: {sort: string, order: string}) => void} onChange
 */
export function sortControl(options, value, onChange, { label = 'Sort by' } = {}) {
  const sel = h('select', { class: 'select mu-sort-select', aria: { label } }, options.map((o) => h('option', { value: o.value }, o.label)));
  sel.value = value.sort;
  const dir = ui.button({ icon: 'arrow-up', title: 'Order', size: 'sm', variant: 'ghost', className: 'mu-sort-dir' });
  let cur = { ...value };
  const paint = () => {
    dir.replaceChildren(icon(cur.order === 'asc' ? 'arrow-up' : 'arrow-down', 14));
    const t = cur.order === 'asc' ? 'Ascending' : 'Descending';
    dir.setAttribute('aria-label', `Order: ${t}`);
    dir.dataset.tip = `Order: ${t}`;
  };
  sel.addEventListener('change', () => {
    const opt = options.find((o) => o.value === sel.value) || options[0];
    cur = { sort: opt.value, order: opt.order };
    paint();
    onChange(cur);
  });
  dir.addEventListener('click', () => {
    cur = { ...cur, order: cur.order === 'asc' ? 'desc' : 'asc' };
    paint();
    onChange(cur);
  });
  paint();
  return h('div', { class: 'mu-sort' }, sel, dir);
}

/** Toolbar row (wraps on phones). */
export function toolbar(...children) {
  return h('div', { class: 'mu-toolbar' }, children);
}

/**
 * A grid of cards loaded page by page as the user scrolls.
 * @param {{fetchPage: (cursor: string|null) => Promise<{items: any[], next_cursor: string|null}>,
 *          render: (item: any) => Node, empty: () => Node, initial?: {items: any[], cursor: string|null}|null,
 *          onItems?: (all: any[], cursor: string|null) => void, className?: string}} o
 */
export function pagedGrid(o) {
  const grid = cardGrid(o.className || '');
  const emptyBox = h('div', { hidden: true });
  let all = [];
  let cursor = null;
  const skeleton = h('div', { class: 'mu-grid', aria: { hidden: 'true' } }, Array.from({ length: 12 }, () => h('div', { class: 'mu-card is-skeleton' },
    h('span', { class: 'skeleton mu-card-art' }), h('span', { class: 'skeleton mu-sk-line' }), h('span', { class: 'skeleton mu-sk-line short' }))));
  const loader = ui.infiniteList({
    fetchPage: async (c) => {
      const page = await o.fetchPage(c);
      cursor = page?.next_cursor || null;
      return page;
    },
    onItems: (items, { reset }) => {
      skeleton.remove();
      if (reset) {
        grid.replaceChildren();
        all = [];
      }
      all = all.concat(items);
      grid.append(...items.map(o.render));
      emptyBox.hidden = all.length > 0;
      if (!all.length) emptyBox.replaceChildren(o.empty());
      o.onItems?.(all, cursor);
    },
  });
  const el = h('div', { class: 'mu-paged' }, skeleton, grid, emptyBox, loader.el);
  if (o.initial?.items?.length) {
    // Show a cached first part at once (back navigation), then continue from its cursor.
    skeleton.remove();
    all = o.initial.items.slice();
    cursor = o.initial.cursor;
    grid.append(...all.map(o.render));
    if (!cursor) loader.el.hidden = true;
  } else {
    loader.loadMore();
  }
  return {
    el,
    get items() { return all; },
    reload() {
      loader.el.hidden = false;
      return loader.reload();
    },
    destroy() {
      loader.destroy();
    },
  };
}

/**
 * A track list fed by cursor pages: loads the next page as the visible rows near the end.
 * @param {object} listOpts createTrackList options (label, context, …)
 * @param {{fetchPage: (cursor: string|null) => Promise<{items: any[], next_cursor: string|null}>,
 *          initial?: {items: any[], cursor: string|null}|null, onItems?: (all: any[], cursor: string|null) => void,
 *          more?: (cursor: string) => Function|null}} o
 *   more(cursor) builds the queue feed for plays started from this list (continues past it).
 */
export function pagedTrackList(listOpts, o) {
  let cursor = null;
  let busy = false;
  let started = false;
  let gen = 0;
  let failed = null;
  const retry = h('div', { class: 'mu-list-retry', hidden: true });
  const list = createTrackList({
    ...listOpts,
    more: null,
    onRange: (r) => {
      if (started && cursor && !busy && !failed && r.end >= r.count - 40) loadMore();
    },
    onPlay: (ti, tracks) => {
      // Plays continue past what is loaded: the queue fetches the following pages itself.
      playTracks(tracks, { index: ti, context: listOpts.context || null, more: cursor && o.more ? o.more(cursor) : null });
    },
  });
  async function loadMore() {
    if (busy) return;
    busy = true;
    const my = gen;
    try {
      const page = await o.fetchPage(started ? cursor : null);
      if (my !== gen) return;
      const items = page?.items || [];
      if (!started) list.setItems(items);
      else list.appendItems(items);
      started = true;
      cursor = page?.next_cursor || null;
      failed = null;
      retry.hidden = true;
      o.onItems?.(list.items, cursor);
    } catch (err) {
      if (my !== gen || err?.name === 'AbortError') return;
      failed = err;
      if (!started) {
        list.setItems([]);
        list.setEmpty(ui.errorState(err, () => reload()));
      } else {
        retry.replaceChildren(h('span', { class: 'muted small' }, errorText(err)), ui.button({ label: 'Try again', size: 'sm', onClick: () => { failed = null; loadMore(); } }));
        retry.hidden = false;
      }
    } finally {
      if (my === gen) busy = false;
    }
  }
  function reload() {
    gen += 1;
    busy = false;
    started = false;
    cursor = null;
    failed = null;
    list.setLoading(true);
    return loadMore();
  }
  if (o.initial?.items) {
    list.setItems(o.initial.items);
    cursor = o.initial.cursor;
    started = true;
  } else {
    list.setLoading(true);
    loadMore();
  }
  const el = h('div', { class: 'mu-paged-list' }, list.el, retry);
  return { el, list, reload, get cursor() { return cursor; }, destroy: () => list.destroy() };
}

/* ------------------------------------------------------------------ cache */

const cache = new Map();
const TTL = 5 * 60 * 1000;

/** A cached list state (items + cursor) if still fresh. */
export function cached(key) {
  const c = cache.get(key);
  if (!c || Date.now() - c.at > TTL) {
    cache.delete(key);
    return null;
  }
  return c;
}

/** Remember a list state (at most a few lists, the newest win). */
export function remember(key, items, cursor) {
  cache.set(key, { items: items.slice(0, 2000), cursor, at: Date.now() });
  while (cache.size > 6) cache.delete(cache.keys().next().value);
}

/** Forget cached lists (e.g. after edits). */
export function forget(prefix = '') {
  for (const k of [...cache.keys()]) if (k.startsWith(prefix)) cache.delete(k);
}

/** Skeleton for page headers while a detail page loads. */
export function heroSkeleton() {
  return h('div', { class: 'mu-hero is-skeleton', aria: { hidden: 'true' } },
    h('div', { class: 'mu-hero-art' }, h('span', { class: 'skeleton mu-hero-img' })),
    h('div', { class: 'mu-hero-body' }, h('span', { class: 'skeleton mu-sk-line short' }), h('span', { class: 'skeleton mu-sk-title' }), h('span', { class: 'skeleton mu-sk-line' })));
}

/** "Couldn't load" block with a retry. */
export function loadError(err, retry) {
  return h('div', { class: 'page' }, ui.errorState(err, retry));
}
