// TV library — media picker used by the TV app to build lineups. Resolves to an array of
// LineupItem objects (API.md §7, without id) or null when cancelled.

import { h, cls, debounce } from '../../../core/dom.js';
import { icon } from '../../../core/icons.js';
import { plural } from '../../../core/format.js';
import { mediaApi } from './api.js';
import { posterArt, artBox } from './cards.js';
import { folderPicker, checkRow, categorySelects } from './widgets.js';
import {
  displayTitle, cardSubtitle, runtime, sizedUrl, seasonLabel, fileLineupItem,
  folderLineupItem, queryLineupItem, collectPicks,
} from './model.js';

const TABS = {
  files: { label: 'Movies & episodes', icon: 'film' },
  show: { label: 'Shows', icon: 'tv' },
  season: { label: 'Seasons', icon: 'layers' },
  folder: { label: 'Folder', icon: 'folder' },
  query: { label: 'Smart query', icon: 'filter' },
};

/**
 * Open the media picker.
 * @param {object} ctx app context
 * @param {{mode?: 'any'|'files'|'show'|'season'|'query'|'folder', multiple?: boolean, title?: string}} [opts]
 * @returns {Promise<Array<object>|null>} lineup items without id
 */
export async function pickMedia(ctx, { mode = 'any', multiple = true, title = '' } = {}) {
  const { ui } = ctx;
  const api = mediaApi(ctx.api);
  // Unknown modes (e.g. 'lineup') offer every source, like 'any'.
  const tabIds = Object.hasOwn(TABS, mode) ? [mode] : Object.keys(TABS);
  let active = tabIds[0];
  const ctrl = new AbortController();

  // Selections per tab.
  const picked = { files: new Map(), show: new Map(), season: new Map() };
  const countEl = h('span', { class: 'tvl-pick-count muted small', aria: { live: 'polite' } });
  const order = h('select', { class: 'select select-sm', aria: { label: 'Order' } },
    h('option', { value: 'default' }, 'In order'), h('option', { value: 'random' }, 'Shuffled'));
  const limit = h('input', { class: 'input input-sm tvl-limit', inputmode: 'numeric', placeholder: 'All', maxlength: '5', aria: { label: 'Limit' } });
  const options = h('div', { class: 'tvl-pick-options' },
    h('label', { class: 'small' }, 'Order ', order), h('label', { class: 'small' }, 'Limit ', limit));

  const toggle = (map, key, value, on) => {
    if (!multiple) {
      for (const m of Object.values(picked)) m.clear();
      for (const el of body.querySelectorAll('.tvl-pick-row.is-picked')) {
        el.classList.remove('is-picked');
        const cb = el.querySelector('input');
        if (cb) cb.checked = false;
      }
    }
    if (on) map.set(key, value); else map.delete(key);
    update();
  };

  function pickRow({ key, map, value, art, titleText, sub, extra = null }) {
    const input = h('input', { type: multiple ? 'checkbox' : 'radio', name: 'tvl-pick', class: multiple ? 'checkbox' : 'radio', checked: map.has(key), aria: { label: titleText } });
    const row = h('label', { class: cls('tvl-pick-row', { 'is-picked': map.has(key) }), role: 'listitem' },
      input, art, h('span', { class: 'tvl-pick-text' }, h('span', { class: 'tvl-pick-title' }, titleText), sub ? h('span', { class: 'tvl-pick-sub' }, sub) : null), extra);
    input.addEventListener('change', () => {
      toggle(map, key, value, input.checked);
      row.classList.toggle('is-picked', input.checked);
    });
    return row;
  }

  /* ---------------------------------------------------------------- files */
  function filesPanel() {
    const q = h('input', { type: 'search', class: 'input', placeholder: 'Search titles, shows, file names…', aria: { label: 'Search media' } });
    let category = '';
    const seg = ui.segmented({
      options: [{ value: '', label: 'All' }, { value: 'movie', label: 'Movies' }, { value: 'tv', label: 'TV' }, { value: 'none', label: 'Other' }],
      value: '', size: 'sm', ariaLabel: 'Category', onChange: (v) => { category = v; load(); },
    });
    const list = h('div', { class: 'tvl-pick-list', role: 'list', aria: { label: 'Media' } });
    const sentinel = h('div');
    let pager = null;
    let gen = 0;
    const load = () => {
      gen += 1;
      const my = gen;
      pager?.destroy();
      list.replaceChildren();
      const query = { q: q.value.trim() || undefined, category: category || undefined, sort: 'title', limit: 50 };
      if (!query.q && category === 'tv') query.sort = 'episode';
      pager = ui.infiniteList({
        root: list,
        fetchPage: (cursor) => api.list({ ...query, cursor: cursor || undefined }, { signal: ctrl.signal }),
        onItems: (items) => {
          if (my !== gen) return;
          list.append(...items.map((m) => pickRow({
            key: m.id, map: picked.files, value: fileLineupItem(m),
            art: posterArt(m, { size: 'sm' }), titleText: displayTitle(m.media),
            sub: m.media.show_title ? [cardSubtitle(m.media), runtime(m.media.duration_seconds)].filter(Boolean).join(' · ') : cardSubtitle(m.media),
          })));
          if (!list.childElementCount) list.append(h('div', { class: 'tvl-pick-empty muted small' }, 'Nothing found.'));
        },
      });
      list.append(pager.el);
      pager.loadMore();
    };
    q.addEventListener('input', debounce(load, 250));
    load();
    const el = h('div', { class: 'tvl-pick-panel' }, h('div', { class: 'tvl-pick-tools' }, q, seg.el), list, sentinel);
    el.focusFirst = () => q.focus();
    el.destroy = () => pager?.destroy();
    return el;
  }

  /* ---------------------------------------------------------------- shows */
  function showsPanel(seasonMode) {
    const q = h('input', { type: 'search', class: 'input', placeholder: 'Filter shows', aria: { label: 'Filter shows' } });
    const list = h('div', { class: 'tvl-pick-list', role: 'list', aria: { label: 'Shows' } });
    let gen = 0;
    let pager = null;
    const showArt = (s) => artBox({ title: s.show_title, src: s.poster_url ? sizedUrl(s.poster_url, 'sm') : '' });
    const load = () => {
      gen += 1;
      const my = gen;
      pager?.destroy();
      list.replaceChildren();
      pager = ui.infiniteList({
        root: list,
        fetchPage: (cursor) => api.shows({ q: q.value.trim() || undefined, cursor: cursor || undefined, limit: 60 }, { signal: ctrl.signal }),
        onItems: (items) => {
          if (my !== gen) return;
          for (const s of items) {
            const sub = `${plural(s.seasons, 'season')} · ${plural(s.episodes, 'episode')}`;
            if (!seasonMode) {
              list.append(pickRow({ key: s.show_key, map: picked.show, value: s, art: showArt(s), titleText: s.show_title, sub }));
            } else {
              list.append(seasonGroup(s, showArt(s), sub));
            }
          }
          if (!list.childElementCount) list.append(h('div', { class: 'tvl-pick-empty muted small' }, 'No shows yet.'));
        },
      });
      list.append(pager.el);
      pager.loadMore();
    };
    q.addEventListener('input', debounce(load, 250));
    load();
    const el = h('div', { class: 'tvl-pick-panel' }, h('div', { class: 'tvl-pick-tools' }, q), list);
    el.focusFirst = () => q.focus();
    el.destroy = () => pager?.destroy();
    return el;
  }

  function seasonGroup(s, art, sub) {
    const inner = h('div', { class: 'tvl-pick-seasons' });
    const btn = h('button', { type: 'button', class: 'tvl-pick-row tvl-pick-expand', aria: { expanded: 'false' } },
      icon('chevron-right', { size: 14, className: 'tvl-pick-chev' }), art,
      h('span', { class: 'tvl-pick-text' }, h('span', { class: 'tvl-pick-title' }, s.show_title), h('span', { class: 'tvl-pick-sub' }, sub)));
    let loaded = false;
    btn.addEventListener('click', async () => {
      const open = btn.getAttribute('aria-expanded') !== 'true';
      btn.setAttribute('aria-expanded', String(open));
      inner.hidden = !open;
      if (!open || loaded) return;
      loaded = true;
      inner.replaceChildren(ui.spinner({ size: 'sm' }));
      try {
        const detail = await api.show(s.show_key, { signal: ctrl.signal });
        inner.replaceChildren(...detail.seasons.map((se) => pickRow({
          key: `${s.show_key}#${se.season}`, map: picked.season, value: { show_title: detail.show_title, season: se.season },
          art: null, titleText: seasonLabel(se.season), sub: plural(se.episodes.length, 'episode'),
        })));
      } catch (err) {
        loaded = false;
        inner.replaceChildren(h('span', { class: 'danger-text small' }, ui.errorMessage(err)));
      }
    });
    inner.hidden = true;
    return h('div', { class: 'tvl-pick-group', role: 'listitem' }, btn, inner);
  }

  /* --------------------------------------------------------------- folder */
  let folder = null;
  let folderRecursive = null;
  function folderPanel() {
    folder = folderPicker({ api, ui });
    folderRecursive = checkRow('Include subfolders', true);
    return h('div', { class: 'tvl-pick-panel stack-sm' },
      h('p', { class: 'muted small' }, 'Every video in the folder, sorted by path. New files added later are included automatically.'),
      folder.el, folderRecursive);
  }

  /* ---------------------------------------------------------------- query */
  let queryForm = null;
  function queryPanel() {
    const genre = h('select', { class: 'select', aria: { label: 'Genre' } }, h('option', { value: '' }, 'Any genre'));
    const tag = h('select', { class: 'select', aria: { label: 'Tag' } }, h('option', { value: '' }, 'Any tag'));
    const year = h('input', { class: 'input', inputmode: 'numeric', maxlength: '4', placeholder: 'Any year', aria: { label: 'Year' } });
    const catHost = h('div', { class: 'form-grid cols-2' });
    let cats = categorySelects([], {});
    catHost.append(cats.category, cats.subcategory);
    Promise.all([api.categories().catch(() => []), api.facets({}).catch(() => null)]).then(([categories, f]) => {
      cats = categorySelects(categories, {});
      catHost.replaceChildren(cats.category, cats.subcategory);
      cats.category.firstChild.textContent = 'Any category';
      genre.append(...(f?.genres || []).map((g) => h('option', { value: g.value }, `${g.value} (${g.count})`)));
      tag.append(...(f?.tags || []).map((t) => h('option', { value: t.value }, `#${t.value} (${t.count})`)));
    });
    queryForm = () => ({ ...cats.value, genre: genre.value, tag: tag.value, year: /^\d{4}$/.test(year.value.trim()) ? year.value.trim() : '' });
    return h('div', { class: 'tvl-pick-panel stack-sm' },
      h('p', { class: 'muted small' }, 'A smart item plays everything that matches, sorted by title — and picks up new matches automatically.'),
      catHost, h('div', { class: 'form-grid cols-2' }, genre, tag), year);
  }

  /* ---------------------------------------------------------------- shell */
  const panels = {};
  const make = { files: filesPanel, show: () => showsPanel(false), season: () => showsPanel(true), folder: folderPanel, query: queryPanel };
  const body = h('div', { class: 'tvl-pick' });
  const tabs = tabIds.length > 1 ? ui.tabs({
    items: tabIds.map((id) => ({ id, label: TABS[id].label, icon: TABS[id].icon })),
    active, ariaLabel: 'Source', onChange: (id) => show(id),
  }) : null;
  function show(id) {
    active = id;
    if (!panels[id]) {
      panels[id] = make[id]();
      body.append(panels[id]);
    }
    for (const [k, p] of Object.entries(panels)) p.hidden = k !== id;
    options.hidden = id === 'files';
    panels[id].focusFirst?.();
    update();
  }

  // Picks from every list tab are added together (switching tabs used to drop the others
  // silently); the folder or smart-query form of the active tab is added on top.
  function collect() {
    const lim = /^\d+$/.test(limit.value.trim()) ? Number(limit.value.trim()) : null;
    const o = { order: order.value, limit: lim };
    let extra = null;
    if (active === 'folder') {
      const v = folder?.value;
      if (v?.spaceId) extra = folderLineupItem(v.spaceId, v.path, { ...o, recursive: folderRecursive.inputEl.checked, spaceName: v.spaceName });
    } else if (active === 'query' && queryForm) {
      extra = queryLineupItem(queryForm(), o);
    }
    return collectPicks(picked, extra, o);
  }

  function update() {
    const n = picked.files.size + picked.show.size + picked.season.size;
    const form = active === 'folder' || active === 'query';
    countEl.textContent = form ? (n ? `+ ${plural(n, 'item')} selected` : '') : (n ? `${plural(n, 'item')} selected` : 'Nothing selected');
    const btn = m?.buttons?.[1];
    if (btn) {
      const ok = active === 'folder' || active === 'query' || n > 0;
      btn.disabled = !ok;
      btn.dataset.locked = ok ? '' : '1';
    }
  }

  const m = ui.modal({
    title: title || (multiple ? 'Add media' : 'Choose media'),
    size: 'lg', className: 'tvl-picker',
    body: h('div', { class: 'stack-sm' }, tabs?.el || null, body),
    footer: [h('div', { class: 'tvl-pick-foot' }, countEl, options), { label: 'Cancel', value: null }, { label: multiple ? 'Add' : 'Choose', variant: 'primary', type: 'submit' }],
    onSubmit: () => {
      const items = collect();
      return items.length ? items : false;
    },
  });
  show(active);
  const result = await m.closed;
  ctrl.abort();
  for (const p of Object.values(panels)) p.destroy?.();
  return Array.isArray(result) && result.length ? result : null;
}
