// TV library — browse views (movies, recently added, unwatched, other videos, search and
// filtered lists): filters, sorting, grid/list/compact views, infinite scroll and
// multi-selection with bulk actions.

import { h, cls, mount } from '../../../../core/dom.js';
import { icon } from '../../../../core/icons.js';
import { plural } from '../../../../core/format.js';
import { posterCard, listRow, skeletonGrid } from '../cards.js';
import { viewHeader, showError } from './common.js';
import { PRESETS, SORTS, PAGE_SIZE, browseState, listQuery, activeFilterCount, defaultOrder } from '../model.js';
import { openBulkEditor } from '../bulk.js';

const VIEW_KEY = 'lw.tvlib.view';

function storedView() {
  try {
    const v = localStorage.getItem(VIEW_KEY);
    return ['grid', 'list', 'compact'].includes(v) ? v : 'grid';
  } catch {
    return 'grid';
  }
}

function saveView(v) {
  try { localStorage.setItem(VIEW_KEY, v); } catch { /* storage unavailable */ }
}

/**
 * @param {HTMLElement} root
 * @param {{ctx: object, api: object, signal: AbortSignal, preset: string}} env
 * @returns {{update: (ctx: object) => void, destroy: () => void}}
 */
export function renderBrowse(root, env) {
  const { api } = env;
  const presetId = env.preset;
  const preset = PRESETS[presetId] || PRESETS.browse;
  let ctx = env.ctx;
  const ui = () => ctx.ui;
  let state = browseState(ctx.query, presetId);
  let view = storedView();
  let selecting = false;
  const selected = new Set();
  let loaded = []; // Media objects in display order
  let facets = null;
  let spaces = [];
  let pager = null;
  let gen = 0;

  /* --------------------------------------------------------------- layout */
  const countEl = h('span', { class: 'tvl-count', aria: { live: 'polite' } });
  const results = h('div', { class: 'tvl-results' });
  const sentinelHost = h('div');
  const filterBar = h('div', { class: 'tvl-filters', role: 'group', aria: { label: 'Filters' } });
  const viewSeg = ui().segmented({
    options: [
      { value: 'grid', icon: 'grid', title: 'Grid' },
      { value: 'list', icon: 'list', title: 'List' },
      { value: 'compact', icon: 'menu', title: 'Compact' },
    ],
    value: view, size: 'sm', ariaLabel: 'View',
    onChange: (v) => { view = v; saveView(v); renderResults(); },
  });
  const sortBtn = ui().menuButton({
    label: 'Sort', icon: 'sort', size: 'sm', variant: 'ghost', align: 'end', title: 'Sort', caret: true,
    items: () => [
      { heading: 'Sort by' },
      ...SORTS.map((s) => ({ label: s.label, checked: state.sort === s.value, onSelect: () => setSort(s.value, defaultOrder(s.value)) })),
      { separator: true },
      { label: 'Ascending', checked: state.order === 'asc', disabled: state.sort === 'random', onSelect: () => setSort(state.sort, 'asc') },
      { label: 'Descending', checked: state.order === 'desc', disabled: state.sort === 'random', onSelect: () => setSort(state.sort, 'desc') },
      state.sort === 'random' ? { label: 'Reshuffle', icon: 'shuffle', onSelect: () => setSort('random', 'asc', true) } : null,
    ],
  });
  const filterToggle = ui().button({ label: 'Filters', icon: 'filter', size: 'sm', variant: 'ghost', className: 'tvl-filter-toggle', onClick: () => root.classList.toggle('show-filters') });
  const selectBtn = ui().button({ label: 'Select', icon: 'check', size: 'sm', variant: 'ghost', onClick: () => setSelecting(!selecting) });
  const bulkBar = h('div', { class: 'tvl-bulkbar', role: 'region', aria: { label: 'Selection' }, hidden: true });

  const header = viewHeader({
    title: preset.title,
    subtitle: '',
    actions: [countEl, filterToggle, sortBtn, viewSeg.el, selectBtn],
  });
  const qChip = h('div', { class: 'tvl-active-q' });
  root.replaceChildren(h('div', { class: 'tvl-browse' }, header, qChip, filterBar, results, sentinelHost, bulkBar));

  /* -------------------------------------------------------------- filters */
  function setQuery(patch) {
    ctx.router.setQuery(patch);
  }

  function setSort(sort, order, reseed = false) {
    const patch = { sort: sort === preset.sort ? null : sort, order: order === (sort === preset.sort ? preset.order : defaultOrder(sort)) ? null : order };
    patch.seed = sort === 'random' ? (reseed || !state.seed ? Math.random().toString(36).slice(2, 10) : state.seed) : null;
    setQuery(patch);
  }

  function filterSelect(key, label, options) {
    const sel = h('select', { class: 'select select-sm', aria: { label } },
      h('option', { value: '' }, label),
      options.map((o) => h('option', { value: String(o.value) }, o.label)));
    sel.value = state.filters[key] || '';
    if (sel.value !== (state.filters[key] || '')) {
      // The current value is not among the options (e.g. a genre with no matches): keep it.
      sel.append(h('option', { value: state.filters[key] }, state.filters[key]));
      sel.value = state.filters[key];
    }
    sel.addEventListener('change', () => setQuery({ [key]: sel.value || null }));
    return h('label', { class: cls('tvl-filter', { 'is-active': !!state.filters[key] }) }, sel);
  }

  function renderFilters() {
    const f = facets || {};
    const spaceName = (id) => spaces.find((s) => s.id === id)?.name || 'Space';
    const parts = [
      filterSelect('genre', 'All genres', (f.genres || []).map((g) => ({ value: g.value, label: `${g.value} (${g.count})` }))),
      filterSelect('year', 'Any year', (f.years || []).map((y) => ({ value: y.value, label: `${y.value} (${y.count})` }))),
      filterSelect('subcategory', 'Any type', (f.subcategories || []).map((s) => ({ value: s.value, label: `${titleCase(s.value)} (${s.count})` }))),
      'watched' in preset.fixed ? null : filterSelect('watched', 'Watched or not', [{ value: 'false', label: 'Unwatched' }, { value: 'true', label: 'Watched' }]),
      (f.tags || []).length ? filterSelect('tag', 'Any tag', f.tags.map((t) => ({ value: t.value, label: `#${t.value} (${t.count})` }))) : null,
      (f.spaces || []).length > 1 || state.filters.space_id
        ? filterSelect('space_id', 'All spaces', (f.spaces || []).map((s) => ({ value: s.value, label: `${spaceName(s.value)} (${s.count})` }))) : null,
    ];
    const n = activeFilterCount(state.filters);
    mount(filterBar, parts,
      n ? h('button', { type: 'button', class: 'btn btn-ghost btn-sm', onClick: () => clearFilters() }, icon('x', 14), 'Clear filters') : null);
    filterToggle.querySelector('.btn-label').textContent = n ? `Filters (${n})` : 'Filters';
    qChip.replaceChildren();
    if (state.filters.q) {
      qChip.append(ui().chip({ label: `“${state.filters.q}”`, icon: 'search', onRemove: () => setQuery({ q: null }) }));
    }
  }

  function clearFilters() {
    const patch = {};
    for (const k of Object.keys(state.filters)) if (k !== 'q') patch[k] = null;
    setQuery(patch);
  }

  async function loadFacets(signal) {
    try {
      const fq = {};
      if (preset.fixed.category) fq.category = preset.fixed.category;
      else if (state.filters.category) fq.category = state.filters.category;
      const [fc, sp] = await Promise.all([api.facets(fq, { signal }), spaces.length ? spaces : api.spaces().catch(() => [])]);
      facets = fc;
      spaces = sp;
      renderFilters();
    } catch {
      /* filters stay minimal */
    }
  }

  /* -------------------------------------------------------------- results */
  function cardFor(m) {
    const onSelect = (id, ev, checked) => toggle(id, checked, ev);
    if (view === 'grid') return posterCard(m, { selectable: true, selected: selected.has(m.id), onSelect });
    return listRow(m, {
      compact: view === 'compact', selected: selected.has(m.id), onSelect,
      onWatched: (next) => setWatched([m.id], next, { quiet: true }),
    });
  }

  function renderResults() {
    results.className = cls('tvl-results', `is-${view}`, { 'is-selecting': selecting || selected.size > 0 });
    results.setAttribute('role', 'list');
    results.setAttribute('aria-label', preset.title);
    results.replaceChildren(...loaded.map(cardFor));
  }

  function appendItems(items) {
    const nodes = items.map(cardFor);
    results.append(...nodes);
  }

  function reload() {
    gen += 1;
    const my = gen;
    loaded = [];
    selected.clear();
    updateBulk();
    pager?.destroy();
    results.className = cls('tvl-results', `is-${view}`);
    results.replaceChildren(...(view === 'grid' ? skeletonGrid(12) : []));
    countEl.textContent = '';
    const query = listQuery(presetId, state, PAGE_SIZE);
    pager = ui().infiniteList({
      fetchPage: (cursor) => api.list(cursor ? { ...query, cursor } : query, { signal: env.signal }),
      onItems: (items, { reset }) => {
        if (my !== gen) return;
        if (reset) {
          loaded = [];
          results.replaceChildren();
        }
        loaded.push(...items);
        if (!loaded.length) {
          results.className = 'tvl-results';
          results.replaceChildren(emptyResults());
        } else {
          appendItems(items);
        }
        countEl.textContent = pager && !pager.done ? `${loaded.length}+ items` : plural(loaded.length, 'item');
      },
    });
    sentinelHost.replaceChildren(pager.el);
    pager.loadMore().then(() => {
      if (my === gen) countEl.textContent = pager.done ? plural(loaded.length, 'item') : `${loaded.length}+ items`;
    });
  }

  function emptyResults() {
    const filtered = Object.keys(state.filters).length > 0;
    return h('div', { class: 'tvl-state' }, ui().emptyState({
      icon: filtered ? 'filter' : 'film',
      title: filtered ? 'Nothing matches these filters' : 'Nothing here yet',
      message: filtered ? 'Try removing a filter or searching for something else.' : 'Videos you upload in Files show up here.',
      action: filtered ? ui().button({ label: 'Clear filters', onClick: () => setQuery(Object.fromEntries(Object.keys(state.filters).map((k) => [k, null]))) }) : null,
    }));
  }

  /* ------------------------------------------------------------ selection */
  let lastClicked = null;
  function toggle(id, checked, ev) {
    if (ev?.shiftKey && lastClicked && lastClicked !== id) {
      const ids = loaded.map((m) => m.id);
      const a = ids.indexOf(lastClicked);
      const b = ids.indexOf(id);
      if (a >= 0 && b >= 0) {
        for (const x of ids.slice(Math.min(a, b), Math.max(a, b) + 1)) {
          if (checked) selected.add(x); else selected.delete(x);
        }
      }
    } else if (checked) selected.add(id);
    else selected.delete(id);
    lastClicked = id;
    syncSelection();
  }

  function syncSelection() {
    for (const el of results.querySelectorAll('[data-id]')) {
      const on = selected.has(el.dataset.id);
      el.classList.toggle('is-selected', on);
      const cb = el.querySelector('.tvl-select');
      if (cb) cb.checked = on;
    }
    results.classList.toggle('is-selecting', selecting || selected.size > 0);
    updateBulk();
  }

  function setSelecting(on) {
    selecting = on;
    if (!on) selected.clear();
    selectBtn.classList.toggle('is-active', on);
    selectBtn.setAttribute('aria-pressed', String(on));
    syncSelection();
  }

  // In selection mode a click on a card toggles it instead of opening it.
  results.addEventListener('click', (e) => {
    if (!(selecting || selected.size) || view !== 'grid') return;
    const card = e.target instanceof Element ? e.target.closest('.tvl-card[data-id]') : null;
    if (!card || e.target.closest('.tvl-select')) return;
    e.preventDefault();
    toggle(card.dataset.id, !selected.has(card.dataset.id), e);
  });

  function updateBulk() {
    const n = selected.size;
    bulkBar.hidden = n === 0;
    if (!n) return;
    const ids = [...selected];
    bulkBar.replaceChildren(
      h('span', { class: 'tvl-bulk-count' }, `${n} selected`),
      ui().button({ label: 'Edit', icon: 'edit', size: 'sm', onClick: () => bulkEdit(ids) }),
      ui().button({ label: 'Watched', icon: 'check-circle', size: 'sm', onClick: () => setWatched(ids, true) }),
      ui().button({ label: 'Unwatched', icon: 'circle-dot', size: 'sm', onClick: () => setWatched(ids, false) }),
      ui().button({ label: 'Select all', size: 'sm', variant: 'ghost', onClick: () => { for (const m of loaded) selected.add(m.id); syncSelection(); } }),
      ui().button({ icon: 'x', title: 'Clear selection', size: 'sm', variant: 'ghost', onClick: () => { selected.clear(); setSelecting(false); } }),
    );
  }

  async function bulkEdit(ids) {
    const result = await openBulkEditor(ctx, ids);
    if (result) reload();
  }

  async function setWatched(ids, watched, { quiet = false } = {}) {
    try {
      const r = await api.bulk(ids, { watched });
      for (const m of loaded) if (ids.includes(m.id)) m.media.watched = watched;
      if (!quiet) {
        ui().toast.success(watched ? `Marked ${plural(r.updated, 'item')} as watched` : `Marked ${plural(r.updated, 'item')} as unwatched`);
        renderResults();
        syncSelection();
      }
    } catch (err) {
      ui().toast.error(err);
      throw err;
    }
  }

  const onKey = (e) => {
    if (e.key === 'Escape' && (selected.size || selecting) && !document.querySelector('.modal-backdrop')) {
      selected.clear();
      setSelecting(false);
    }
  };
  document.addEventListener('keydown', onKey);

  /* -------------------------------------------------------------- startup */
  renderFilters();
  loadFacets(env.signal);
  reload();

  return {
    update(nextCtx) {
      ctx = nextCtx;
      const next = browseState(ctx.query, presetId);
      const facetsChanged = next.filters.category !== state.filters.category;
      state = next;
      renderFilters();
      if (facetsChanged) loadFacets(env.signal);
      reload();
    },
    destroy() {
      pager?.destroy();
      document.removeEventListener('keydown', onKey);
    },
  };
}

function titleCase(s) {
  return String(s || '').replace(/(^|[\s-])\w/g, (c) => c.toUpperCase());
}
