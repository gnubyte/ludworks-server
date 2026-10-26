// Files app (/files): a full file explorer over the spaces API (API.md §5).
//   /files?space=<id>&path=<dir>          browse a folder
//   /files?space=<id>&q=…&category=…       search (filters: kind, type, tag, scope)
//   /files/trash                           trash
// Left: spaces + lazy folder tree. Centre: list/grid with multi-select, rubber band,
// drag and drop (OS files & folders, internal moves), context menu and shortcuts.
// Right: details panel. Uploads use the shared resumable tus queue (ctx.uploads).

import { h, cls, debounce, pickFiles, isEditable } from '../../core/dom.js';
import { icon } from '../../core/icons.js';
import { formatBytes, formatDateTime, formatRelative, plural } from '../../core/format.js';
import { ensureStyle } from '../../core/style.js';
import { filesApi } from './fsapi.js';
import { createFolderTree } from './tree.js';
import { createDetails } from './details.js';
import { openPreview } from './preview.js';
import { renderTrash } from './trash.js';
import { pickDestination, askConflict, promptName, categorizeDialog } from './dialogs.js';
import { readDrop, pickedFiles, hasOSFiles, hasEntries, DRAG_TYPE } from './dnd.js';
import {
  parentPath, fileKind, KIND_ICONS, previewKind, isMedia, SORTS, parseSort, compareEntries,
  clickSelect, moveFocus, intersects, rectFrom, planUpload, splitErrors, routeState, searchQuery, thumbVersion,
  selectionSummary, freeName,
} from './model.js';

const LS = {
  view: 'lw.files.view', sort: 'lw.files.sort', details: 'lw.files.details',
};

function store(key, value) {
  try {
    if (value === undefined) return localStorage.getItem(key);
    localStorage.setItem(key, value);
  } catch { /* storage unavailable */ }
  return null;
}

export async function mount(container, ctx) {
  const { ui } = ctx;
  await ensureStyle('/assets/css/apps/files.css'); // avoid a flash of unstyled layout
  ctx.setLayout({ full: true });
  ctx.setTitle('Files');
  const fapi = filesApi(ctx.api);

  /* ------------------------------------------------------------- state */
  const sortPref = parseSort(store(LS.sort));
  const S = {
    spaces: [], categories: null,
    mode: 'browse', spaceId: '', path: '', q: '', filters: {},
    sort: sortPref.sort, order: sortPref.order,
    view: store(LS.view) === 'grid' ? 'grid' : 'list',
    // The panel is inline on wide screens (remembered) and an overlay on narrow ones
    // (closed until asked for).
    details: window.matchMedia?.('(max-width: 1240px)').matches ? false
      : (store(LS.details) !== null ? store(LS.details) === '1' : true),
    dir: null, items: [], byId: new Map(), selected: new Set(), anchor: null, focus: -1,
    clipboard: null, gen: 0, loaded: false, error: null,
  };
  const nodes = new Map(); // id → {el, box}

  /* ------------------------------------------------------------ layout */
  const crumbs = h('nav', { class: 'fx-crumbs', aria: { label: 'Folder path' } });
  const sideToggle = ui.button({ icon: 'panel-left', title: 'Folders', variant: 'ghost', className: 'fx-side-toggle', onClick: () => toggleSide() });
  const viewSeg = ui.segmented({
    options: [{ value: 'list', icon: 'list', title: 'List view' }, { value: 'grid', icon: 'grid', title: 'Grid view' }],
    value: S.view, size: 'sm', ariaLabel: 'View',
    onChange: (v) => { S.view = v; store(LS.view, v); renderItems(); },
  });
  const sortBtn = ui.menuButton({
    label: 'Sort', icon: 'sort', size: 'sm', variant: 'ghost', align: 'end', title: 'Sort',
    items: () => [
      { heading: 'Sort by' },
      ...SORTS.map((s) => ({ label: s.label, checked: S.sort === s.value, onSelect: () => setSort(s.value, S.order) })),
      { separator: true },
      { label: 'Ascending', checked: S.order === 'asc', onSelect: () => setSort(S.sort, 'asc') },
      { label: 'Descending', checked: S.order === 'desc', onSelect: () => setSort(S.sort, 'desc') },
    ],
  });
  sortBtn.classList.add('fx-sort-btn');
  const catFilter = h('select', { class: 'select fx-cat-filter', aria: { label: 'Filter by category' } }, h('option', { value: '' }, 'All categories'));
  catFilter.addEventListener('change', () => ctx.router.setQuery({ category: catFilter.value || null }, { replace: false }));
  const detailsBtn = ui.button({ icon: 'info', title: 'Details (I)', variant: 'ghost', size: 'sm', onClick: () => toggleDetails() });
  const newFolderBtn = ui.button({ label: 'New folder', icon: 'plus', size: 'sm', className: 'fx-newfolder', onClick: () => newFolder() });
  const uploadBtn = ui.menuButton({
    label: 'Upload', icon: 'upload', size: 'sm', variant: 'primary', align: 'end', caret: true,
    items: () => [
      { label: 'Upload files…', icon: 'file', shortcut: 'U', onSelect: () => uploadPicked(false) },
      { label: 'Upload folder…', icon: 'folder', onSelect: () => uploadPicked(true) },
    ],
  });
  const filterBar = h('div', { class: 'fx-filters', hidden: true });
  const bulkCount = h('span', { class: 'fx-bulk-count' });
  // The selection bar covers the toolbar (like most file managers) instead of being inserted
  // above the list: inserting it shifted every row down on the first click, so a following
  // Shift-click or the context menu landed on the wrong item.
  const bulkBar = h('div', { class: 'fx-bulkbar', hidden: true, role: 'toolbar', aria: { label: 'Selection actions' } });
  const toolbar = h('div', { class: 'fx-toolbar' },
    sideToggle, crumbs, h('div', { class: 'spacer' }),
    catFilter, sortBtn, viewSeg.el, detailsBtn, newFolderBtn, uploadBtn, bulkBar);

  const listHost = h('div', { class: 'fx-items' });
  const emptyHost = h('div', { class: 'fx-empty', hidden: true });
  const band = h('div', { class: 'fx-band', hidden: true });
  const main = h('div', { class: 'fx-main', tabindex: '0', aria: { label: 'Files' } }, listHost, emptyHost, band);
  const dropOverlay = h('div', { class: 'fx-drop', hidden: true },
    h('div', { class: 'fx-drop-card' }, icon('upload', 28), h('div', { class: 'fx-drop-title' }), h('div', { class: 'fx-drop-sub small' }, 'Files and whole folders are uploaded with resume support.')));
  const center = h('div', { class: 'fx-center' }, filterBar, main, dropOverlay);
  const trashHost = h('div', { class: 'fx-center fx-trash-host', hidden: true });

  const tree = createFolderTree({ fapi, onSelect: (spaceId, path) => { closeSide(); navigate(spaceId, path); }, showUsage: true, label: 'Spaces and folders', signal: ctx.signal, onError: (e) => ui.toast.error(e) });
  const trashLink = h('a', { class: 'fx-side-link', href: '/files/trash' }, icon('trash', 16), h('span', {}, 'Trash'));
  const side = h('div', { class: 'fx-side' },
    h('div', { class: 'fx-side-section' }, h('div', { class: 'fx-side-heading' }, 'Spaces'), tree.el),
    h('div', { class: 'fx-side-foot' }, trashLink,
      h('a', { class: 'fx-side-link', href: '/system' }, icon('storage', 16), h('span', {}, 'Storage'))));
  const details = createDetails(ctx, fapi, {
    categories: () => loadCategories(),
    onChanged: (e) => { upsert(e); },
    onAction: (a, entries) => runAction(a, entries),
    onClose: () => toggleDetails(false),
    spaceName,
  });
  const body = h('div', { class: 'fx-body' }, center, trashHost, details.el);
  const split = ui.splitPane({ start: side, end: body, initial: 264, min: 200, max: 440, storageKey: 'lw.files.split', className: 'fx-split' });
  const sideBackdrop = h('div', { class: 'fx-side-backdrop' });
  sideBackdrop.addEventListener('click', () => closeSide());
  const root = h('div', { class: cls('fx', `fx-view-${S.view}`) }, toolbar, split, sideBackdrop);
  container.append(root);

  /* ---------------------------------------------------------- helpers */
  const space = () => S.spaces.find((s) => s.id === S.spaceId) || null;
  const writable = () => !!space() && space().access !== 'read';
  function spaceName(id) {
    const sp = S.spaces.find((s) => s.id === id);
    return sp ? (sp.kind === 'home' ? 'Home' : sp.name) : '';
  }
  function loadCategories() {
    S.categories ??= fapi.categories({ signal: ctx.signal }).catch((err) => { S.categories = null; throw err; });
    return S.categories;
  }
  function navigate(spaceId, path, { replace = false } = {}) {
    const q = { space: spaceId, path: path || null };
    ctx.router.navigate(`/files${buildQS(q)}`, { replace });
  }
  function buildQS(q) {
    const p = new URLSearchParams();
    for (const [k, v] of Object.entries(q)) if (v !== null && v !== undefined && v !== '') p.set(k, v);
    const s = p.toString();
    return s ? `?${s}` : '';
  }
  const selectedEntries = () => S.items.filter((e) => S.selected.has(e.id));
  const overlayOpen = () => document.documentElement.classList.contains('modal-open') || document.documentElement.classList.contains('palette-open');
  const active = () => !overlayOpen() && S.mode !== 'trash' && root.isConnected;

  /* ------------------------------------------------------- toolbar UI */
  function setSort(sort, order) {
    S.sort = sort;
    S.order = order;
    store(LS.sort, `${sort}:${order}`);
    load();
  }

  function toggleDetails(force) {
    S.details = force ?? !S.details;
    store(LS.details, S.details ? '1' : '0');
    root.classList.toggle('fx-details-open', S.details);
    detailsBtn.classList.toggle('is-active', S.details);
    detailsBtn.setAttribute('aria-pressed', String(S.details));
    if (S.details) showDetails();
  }

  function toggleSide() {
    root.classList.toggle('fx-side-open');
  }
  function closeSide() {
    root.classList.remove('fx-side-open');
  }

  function renderCrumbs(list) {
    const sp = space();
    const items = (list || []).map((b, i) => ({
      label: i === 0 ? (sp?.kind === 'home' ? 'Home' : sp?.name || b.name) : b.name,
      icon: i === 0 ? (sp?.kind === 'home' ? 'home' : 'library') : undefined,
      onClick: () => navigate(S.spaceId, b.path),
    }));
    const nav = ui.breadcrumbs(items);
    // Breadcrumbs are drop targets for moves and uploads.
    nav.querySelectorAll('li').forEach((li, i) => {
      li.dataset.drop = '1';
      li.dataset.space = S.spaceId;
      li.dataset.path = list[i].path;
      // Ancestors ellipsize when the path is long: keep their full names as tooltips.
      li.querySelector('.crumb')?.setAttribute('title', items[i].label);
    });
    crumbs.replaceChildren(nav);
    if (sp?.access === 'read') crumbs.append(ui.badge('Read only', 'neutral', { icon: 'lock' }));
  }

  function renderSearchHeader() {
    const label = S.q ? `Results for “${S.q}”` : 'Filtered files';
    crumbs.replaceChildren(h('div', { class: 'fx-search-title' }, icon('search', 16), h('span', { class: 'truncate' }, label),
      ui.button({ label: 'Clear', size: 'sm', variant: 'ghost', onClick: () => clearSearch() })));
  }

  function clearSearch() {
    navigate(S.spaceId, S.path);
  }

  function renderFilters() {
    filterBar.hidden = S.mode !== 'search';
    if (filterBar.hidden) return;
    const f = S.filters;
    const set = (patch) => ctx.router.setQuery(patch);
    const scope = ui.segmented({
      options: [{ value: 'space', label: spaceName(S.spaceId) || 'This space' }, { value: 'all', label: 'All spaces' }],
      value: f.scope, size: 'sm', ariaLabel: 'Search scope', onChange: (v) => set({ scope: v === 'all' ? 'all' : null }),
    });
    const kind = ui.segmented({
      options: [{ value: '', label: 'Any' }, { value: 'image', label: 'Images' }, { value: 'video', label: 'Videos' }, { value: 'audio', label: 'Audio' }, { value: 'document', label: 'PDFs' }],
      value: f.kind, size: 'sm', ariaLabel: 'Kind', onChange: (v) => set({ kind: v || null }),
    });
    const type = h('select', { class: 'select input-sm fx-filter-select', aria: { label: 'Type' } },
      h('option', { value: '' }, 'Files & folders'), h('option', { value: 'file' }, 'Files only'), h('option', { value: 'dir' }, 'Folders only'));
    type.value = f.type;
    type.addEventListener('change', () => set({ type: type.value || null }));
    const cat = h('select', { class: 'select input-sm fx-filter-select', aria: { label: 'Category' } },
      h('option', { value: '' }, 'All categories'), (S.catList || []).map((c) => h('option', { value: c.slug }, c.name)));
    cat.value = f.category;
    cat.addEventListener('change', () => set({ category: cat.value || null }));
    const tag = f.tag ? ui.chip({ label: f.tag, icon: 'tag', onRemove: () => set({ tag: null }) }) : null;
    filterBar.replaceChildren(...[scope.el, kind.el, type, cat, tag].filter(Boolean));
  }

  /* ---------------------------------------------------------- loading */
  let list = null;

  async function init() {
    try {
      S.spaces = await fapi.spaces({ signal: ctx.signal });
    } catch (err) {
      if (err?.name === 'AbortError') return;
      center.replaceChildren(ui.errorState(err, () => location.reload()));
      return;
    }
    tree.setSpaces(S.spaces);
    loadCategories().then((cats) => {
      catFilter.append(...cats.map((c) => h('option', { value: c.slug }, c.name)));
      catFilter.value = S.filters.category || '';
    }).catch(() => { catFilter.hidden = true; });
    S.loaded = true;
    applyRoute(ctx);
  }

  function applyRoute(c) {
    const rs = routeState(c.params?.rest || '', c.query || {});
    if (!rs.spaceId || !S.spaces.some((s) => s.id === rs.spaceId)) {
      if (rs.spaceId && S.spaces.length) ui.toast.warning('That space is not available.');
      rs.spaceId = S.spaces[0]?.id || '';
      if (rs.mode === 'browse' && rs.spaceId) {
        navigate(rs.spaceId, rs.path, { replace: true });
        return;
      }
    }
    const changed = rs.mode !== S.mode || rs.spaceId !== S.spaceId || rs.path !== S.path || rs.q !== S.q || JSON.stringify(rs.filters) !== JSON.stringify(S.filters);
    const leftSearch = S.mode === 'search' && rs.mode !== 'search';
    Object.assign(S, rs);
    if (leftSearch) ctx.setSearch(searchCfg()); // clear the top-bar box
    trashLink.classList.toggle('is-active', S.mode === 'trash');
    trashLink.setAttribute('aria-current', S.mode === 'trash' ? 'page' : 'false');
    catFilter.value = S.filters.category || '';
    if (S.mode === 'trash') {
      showTrash();
      return;
    }
    hideTrash();
    if (changed || !list) load();
  }

  let trashView = null;
  function showTrash() {
    ctx.setTitle('Trash');
    center.hidden = true;
    trashHost.hidden = false;
    root.classList.add('fx-mode-trash');
    tree.setActive('', '', { reveal: false });
    crumbs.replaceChildren(h('div', { class: 'fx-search-title' }, icon('trash', 16), h('span', {}, 'Trash')));
    filterBar.hidden = true;
    trashView?.destroy();
    trashView = renderTrash(trashHost, ctx, fapi, { spaceName, onChanged: () => refreshSpaces() });
  }
  function hideTrash() {
    if (!trashView) return;
    trashView.destroy();
    trashView = null;
    trashHost.replaceChildren();
    trashHost.hidden = true;
    center.hidden = false;
    root.classList.remove('fx-mode-trash');
  }

  function load() {
    const gen = ++S.gen;
    S.items = [];
    S.byId = new Map();
    S.selected = new Set();
    S.anchor = null;
    S.focus = -1;
    S.error = null;
    nodes.clear();
    tableBody = null;
    gridEl = null;
    listHost.replaceChildren(skeletonView());
    emptyHost.hidden = true;
    list?.destroy();
    list?.el.remove();
    const searching = S.mode === 'search';
    root.classList.toggle('fx-mode-search', searching);
    root.classList.toggle('fx-readonly', !writable());
    ctx.setTitle(searching ? 'Search files' : (S.path ? S.path.split('/').pop() : spaceName(S.spaceId) || 'Files'));
    if (searching) renderSearchHeader();
    renderFilters();
    sortBtn.disabled = searching;
    tree.setActive(S.spaceId, searching ? '' : S.path);
    list = ui.infiniteList({
      root: main,
      rootMargin: '800px',
      fetchPage: async (cursor) => {
        try {
          if (searching) return await fapi.search(searchQuery(S), cursor, ctx.signal);
          const page = await fapi.list(S.spaceId, S.path, { sort: S.sort, order: S.order, cursor, signal: ctx.signal });
          if (gen === S.gen) {
            S.dir = page.dir;
            renderCrumbs(page.breadcrumbs);
          }
          return page;
        } catch (err) {
          if (err?.name === 'AbortError') throw err;
          if (gen === S.gen) showError(err);
          return { items: [], next_cursor: null };
        }
      },
      onItems: (items, { reset }) => {
        if (gen !== S.gen) return;
        if (reset) {
          S.items = [];
          S.byId.clear();
          renderItems(); // fresh table/grid (replaces the skeleton)
        }
        appendItems(items);
        updateEmpty();
        if (reset) showDetails();
      },
    });
    main.append(list.el);
    list.loadMore().then(() => updateEmpty());
    syncToolbar();
  }

  function showError(err) {
    S.error = err;
    listHost.replaceChildren();
    emptyHost.hidden = false;
    if (err?.status === 404) {
      emptyHost.replaceChildren(ui.emptyState({
        icon: 'folder', title: 'Folder not found', message: 'It may have been moved or deleted.',
        action: ui.button({ label: 'Go to the space root', icon: 'home', onClick: () => navigate(S.spaceId, '') }),
      }));
      crumbs.replaceChildren();
    } else {
      emptyHost.replaceChildren(ui.errorState(err, () => load()));
    }
  }

  function skeletonView() {
    return h('div', { class: 'fx-skeleton', aria: { hidden: 'true' } },
      Array.from({ length: 8 }, () => h('div', { class: 'fx-skel-row' }, ui.skeleton({ width: '22px', height: '22px' }), ui.skeleton({ width: `${30 + Math.round(Math.random() * 40)}%` }))));
  }

  function updateEmpty() {
    if (S.error) return;
    const empty = S.items.length === 0 && (list?.done ?? true);
    emptyHost.hidden = !empty;
    listHost.hidden = empty;
    if (!empty) return;
    if (S.mode === 'search') {
      emptyHost.replaceChildren(ui.emptyState({
        icon: 'search', title: 'No matches', message: 'Try other words, another category or search all spaces.',
        action: ui.button({ label: 'Clear search', icon: 'x', onClick: () => clearSearch() }),
      }));
      return;
    }
    emptyHost.replaceChildren(writable()
      ? ui.emptyState({
        icon: 'folder-open', title: S.path ? 'This folder is empty' : 'Nothing here yet',
        message: 'Drag files or whole folders here, or use Upload. Uploads resume automatically if your connection drops.',
        action: h('div', { class: 'row' },
          ui.button({ label: 'Upload files', icon: 'upload', variant: 'primary', onClick: () => uploadPicked(false) }),
          ui.button({ label: 'New folder', icon: 'plus', onClick: () => newFolder() })),
      })
      : ui.emptyState({ icon: 'folder-open', title: 'This folder is empty', message: 'You have read-only access to this space.' }));
  }

  /* --------------------------------------------------------- rendering */
  let tableBody = null;
  let gridEl = null;

  function headerCell(label, key, extra = '') {
    if (S.mode === 'search' || !key) return h('th', { class: extra }, label);
    const on = S.sort === key;
    const th = h('th', { class: cls(extra, { 'is-sorted': on }), aria: { sort: on ? (S.order === 'asc' ? 'ascending' : 'descending') : 'none' } },
      h('button', { class: 'th-sort', type: 'button', onClick: () => setSort(key, on && S.order === 'asc' ? 'desc' : 'asc') },
        label, h('span', { class: 'th-sort-icon' }, icon(on ? (S.order === 'asc' ? 'arrow-up' : 'arrow-down') : 'chevrons-up-down', 12))));
    return th;
  }

  function renderItems() {
    root.classList.toggle('fx-view-list', S.view === 'list');
    root.classList.toggle('fx-view-grid', S.view === 'grid');
    viewSeg.setValue(S.view);
    nodes.clear();
    tableBody = null;
    gridEl = null;
    listHost.replaceChildren();
    if (S.view === 'list') {
      const allBox = h('input', { type: 'checkbox', class: 'checkbox fx-all', aria: { label: 'Select all' } });
      allBox.addEventListener('change', () => setSelection(allBox.checked ? new Set(S.items.map((e) => e.id)) : new Set()));
      tableBody = h('tbody');
      listHost.append(h('div', { class: 'fx-table-wrap' }, h('table', { class: 'table fx-table', role: 'grid', aria: { multiselectable: 'true', label: 'Files' } },
        h('thead', {}, h('tr', {},
          h('th', { class: 'td-check' }, allBox),
          headerCell('Name', 'name'),
          headerCell('Size', 'size', 'align-right fx-col-size'),
          headerCell('Modified', 'mod_time', 'fx-col-mod'),
          headerCell('Category', null, 'fx-col-cat'),
          headerCell('Tags', null, 'fx-col-tags'))),
        tableBody)));
    } else {
      gridEl = h('div', { class: 'fx-grid', role: 'listbox', aria: { multiselectable: 'true', label: 'Files' } });
      listHost.append(gridEl);
    }
    S.items.forEach((e, i) => addNode(e, i));
    paintSelection();
  }

  function appendItems(items) {
    if (!tableBody && !gridEl) renderItems();
    for (const e of items) {
      if (S.byId.has(e.id)) continue;
      S.byId.set(e.id, e);
      S.items.push(e);
      addNode(e, S.items.length - 1);
    }
    paintSelection();
  }

  function thumbOrIcon(e, size) {
    const k = fileKind(e);
    const ic = h('span', { class: `fx-icon fx-kind-${k}` }, icon(KIND_ICONS[k], size === 'sm' ? 18 : 36));
    if (!e.has_thumbnail) return ic;
    const img = h('img', { class: size === 'sm' ? 'fx-thumb-sm' : 'fx-thumb', alt: '', loading: 'lazy', decoding: 'async', draggable: 'false', src: fapi.thumbUrl(e.id, size, thumbVersion(e)) });
    img.addEventListener('error', () => img.replaceWith(ic));
    return img;
  }

  function catLabel(e) {
    if (!e.category) return null;
    const cats = S.catList || [];
    const c = cats.find((x) => x.slug === e.category);
    const sub = c?.subcategories?.find((s) => s.slug === e.subcategory);
    return h('span', { class: `fx-cat fx-cat-${/^[a-z0-9_-]+$/.test(e.category) ? e.category : 'x'}` }, c ? c.name : e.category, sub ? h('span', { class: 'faint' }, ` · ${sub.name}`) : null);
  }

  function addNode(e, index) {
    const box = h('input', { type: 'checkbox', class: 'checkbox', tabindex: '-1', aria: { label: `Select ${e.name}` } });
    const k = fileKind(e);
    const sub = S.mode === 'search' ? [spaceName(e.space_id), ...parentPath(e.path).split('/')].filter(Boolean).join(' › ') : null;
    let el;
    if (S.view === 'list') {
      el = h('tr', {
        class: 'fx-row', tabindex: '-1', draggable: 'true',
        dataset: { id: e.id, index: String(index), ...(e.is_dir ? { drop: '1', space: e.space_id, path: e.path } : {}) },
        aria: { selected: 'false', label: e.name },
      },
      h('td', { class: 'td-check' }, box),
      h('td', { class: 'fx-col-name' }, h('div', { class: 'cell-main' }, thumbOrIcon(e, 'sm'),
        h('div', { class: 'cell-stack' },
          h('span', { class: 'cell-title truncate fx-name' }, e.name),
          sub ? h('span', { class: 'cell-sub truncate' }, sub) : null,
          h('span', { class: 'cell-sub fx-subline truncate' }, e.is_dir ? formatRelative(e.mod_time) : `${formatBytes(e.size)} · ${formatRelative(e.mod_time)}`)))),
      h('td', { class: 'align-right nums nowrap fx-col-size' }, e.is_dir ? '—' : formatBytes(e.size)),
      h('td', { class: 'nowrap fx-col-mod', dataset: { tip: formatDateTime(e.mod_time) } }, formatRelative(e.mod_time)),
      h('td', { class: 'fx-col-cat' }, catLabel(e)),
      h('td', { class: 'fx-col-tags' }, e.tags?.length ? h('div', { class: 'fx-tags' }, e.tags.slice(0, 3).map((t) => h('span', { class: 'fx-tag' }, t)), e.tags.length > 3 ? h('span', { class: 'faint small' }, `+${e.tags.length - 3}`) : null) : null));
      tableBody.append(el);
    } else {
      el = h('div', {
        class: cls('fx-card', `fx-kind-${k}`), role: 'option', tabindex: '-1', draggable: 'true',
        dataset: { id: e.id, index: String(index), ...(e.is_dir ? { drop: '1', space: e.space_id, path: e.path } : {}) },
        aria: { selected: 'false', label: e.name },
      },
      h('div', { class: 'fx-card-thumb' }, thumbOrIcon(e, 'md'), e.is_dir ? null : h('span', { class: 'fx-card-ext' }, (e.name.split('.').pop() || '').slice(0, 5)), box),
      h('div', { class: 'fx-card-body' },
        h('div', { class: 'fx-card-name', title: e.name }, e.name),
        h('div', { class: 'fx-card-meta' }, sub || (e.is_dir ? formatRelative(e.mod_time) : formatBytes(e.size)))));
      if (!e.has_thumbnail || e.is_dir) el.querySelector('.fx-card-ext')?.remove();
      gridEl.append(el);
    }
    nodes.set(e.id, { el, box });
  }

  function paintSelection() {
    for (const [id, n] of nodes) {
      const on = S.selected.has(id);
      n.el.classList.toggle('is-selected', on);
      n.el.setAttribute('aria-selected', String(on));
      n.box.checked = on;
    }
    const all = listHost.querySelector('.fx-all');
    if (all) {
      all.checked = S.items.length > 0 && S.selected.size === S.items.length;
      all.indeterminate = S.selected.size > 0 && S.selected.size < S.items.length;
    }
    nodes.forEach((n, id) => { n.el.tabIndex = S.items[S.focus]?.id === id ? 0 : -1; });
    renderBulk();
  }

  function setSelection(next, { anchor } = {}) {
    S.selected = next;
    if (anchor !== undefined) S.anchor = anchor;
    paintSelection();
    showDetails();
  }

  function setFocus(i, { scroll = true } = {}) {
    S.focus = i;
    const e = S.items[i];
    if (!e) return;
    const n = nodes.get(e.id);
    nodes.forEach((x) => { x.el.tabIndex = -1; });
    if (n) {
      n.el.tabIndex = 0;
      n.el.focus({ preventScroll: !scroll });
      if (scroll) n.el.scrollIntoView({ block: 'nearest' });
    }
  }

  function showDetails() {
    if (!S.details) return;
    details.show(selectedEntries(), { canWrite: writable() || S.mode === 'search', dir: S.mode === 'browse' ? { ...(S.dir || {}), name: S.path ? S.path.split('/').pop() : spaceName(S.spaceId) } : null });
  }

  function renderBulk() {
    const sel = selectedEntries();
    bulkBar.hidden = sel.length === 0;
    if (!sel.length) return;
    const s = selectionSummary(sel);
    bulkCount.textContent = `${plural(s.count, 'item')} selected${s.bytes ? ` · ${formatBytes(s.bytes)}` : ''}`;
    const canWrite = writable() || S.mode === 'search';
    const zipOnly = sel.length === 1 && sel[0].is_dir;
    // Labels collapse to icons when the bar is narrow, so every button keeps an aria-label.
    const action = (label, ic, act, variant = 'ghost') => ui.button({ label, ariaLabel: label, icon: ic, size: 'sm', variant, onClick: () => runAction(act, sel) });
    bulkBar.replaceChildren(...[
      ui.button({ icon: 'x', title: 'Clear selection (Esc)', size: 'sm', variant: 'ghost', onClick: () => setSelection(new Set()) }),
      bulkCount, h('div', { class: 'spacer' }),
      canWrite ? action('Move', 'arrow-right', 'move') : null,
      action('Copy', 'copy', 'copy'),
      canWrite ? action('Categorize', 'tag', 'categorize') : null,
      action(zipOnly ? 'Download ZIP' : 'Download', 'download', 'download'),
      canWrite ? action('Delete', 'trash', 'delete', 'danger-ghost') : null,
    ].filter(Boolean));
  }

  function syncToolbar() {
    const w = writable() && S.mode === 'browse';
    newFolderBtn.hidden = !w;
    uploadBtn.hidden = !w;
    catFilter.hidden = S.mode === 'search' || !S.catList?.length;
    root.classList.toggle('fx-details-open', S.details);
    detailsBtn.classList.toggle('is-active', S.details);
  }

  /** Insert or update an entry in the current listing (keeps scroll and selection). */
  function upsert(e) {
    const idx = S.items.findIndex((x) => x.id === e.id);
    const belongs = S.mode === 'browse' && e.space_id === S.spaceId && parentPath(e.path) === S.path;
    if (idx >= 0) {
      if (!belongs && S.mode === 'browse') {
        removeItems([e.id]);
        return;
      }
      S.items[idx] = { ...S.items[idx], ...e };
      S.byId.set(e.id, S.items[idx]);
      rerenderKeepScroll();
      return;
    }
    if (!belongs) return;
    S.items.push(e);
    S.items.sort((a, b) => compareEntries(a, b, S.sort, S.order));
    S.byId.set(e.id, e);
    rerenderKeepScroll();
    updateEmpty();
  }

  function removeItems(ids) {
    const gone = new Set(ids);
    S.items = S.items.filter((e) => !gone.has(e.id));
    for (const id of ids) {
      S.byId.delete(id);
      S.selected.delete(id);
    }
    const hadFocus = main.contains(document.activeElement) || document.activeElement === document.body;
    S.focus = Math.min(S.focus, S.items.length - 1);
    rerenderKeepScroll();
    updateEmpty();
    showDetails();
    // Keep keyboard users in the list after their focused row disappeared.
    if (hadFocus && !main.contains(document.activeElement)) {
      if (S.focus >= 0) setFocus(S.focus, { scroll: false });
      else main.focus({ preventScroll: true });
    }
  }

  function rerenderKeepScroll() {
    const top = main.scrollTop;
    renderItems();
    main.scrollTop = top;
  }

  /* ------------------------------------------------------------ events */
  function itemFromEvent(ev) {
    const el = ev.target.closest?.('[data-id]');
    if (!el || !main.contains(el)) return null;
    const i = Number(el.dataset.index);
    return { el, index: i, entry: S.items[i] };
  }

  main.addEventListener('click', (ev) => {
    const it = itemFromEvent(ev);
    if (!it) {
      if (!ev.target.closest('button, a, input') && !bandMoved) setSelection(new Set());
      return;
    }
    const onBox = ev.target.matches('input[type="checkbox"]');
    const r = clickSelect({ ids: S.items.map((e) => e.id), selected: S.selected, anchor: S.anchor }, it.index,
      { toggle: onBox || ev.ctrlKey || ev.metaKey, range: ev.shiftKey });
    S.focus = it.index;
    setSelection(r.selected, { anchor: r.anchor });
  });

  main.addEventListener('dblclick', (ev) => {
    const it = itemFromEvent(ev);
    if (it && !ev.target.matches('input')) open(it.entry);
  });

  main.addEventListener('contextmenu', (ev) => {
    const it = itemFromEvent(ev);
    if (!it) {
      ui.contextMenu(ev, backgroundMenu());
      return;
    }
    if (!S.selected.has(it.entry.id)) {
      S.focus = it.index;
      setSelection(new Set([it.entry.id]), { anchor: it.index });
    }
    ui.contextMenu(ev, itemMenu(selectedEntries()));
  });

  main.addEventListener('keydown', (ev) => {
    if (isEditable(ev.target) && ev.target.type !== 'checkbox') return;
    const nav = ['ArrowDown', 'ArrowUp', 'ArrowLeft', 'ArrowRight', 'Home', 'End', 'PageDown', 'PageUp'];
    if (nav.includes(ev.key)) {
      if (!S.items.length) return;
      ev.preventDefault();
      const cols = S.view === 'grid' ? gridColumns() : 1;
      const next = moveFocus(S.focus, ev.key, S.items.length, cols);
      if (ev.shiftKey) {
        const anchor = S.anchor ?? (S.focus >= 0 ? S.focus : next);
        const r = clickSelect({ ids: S.items.map((e) => e.id), selected: S.selected, anchor }, next, { range: true });
        setSelection(r.selected, { anchor });
      } else if (!(ev.ctrlKey || ev.metaKey)) {
        setSelection(new Set([S.items[next].id]), { anchor: next });
      }
      setFocus(next);
      if (next >= S.items.length - 5 && list && !list.done) list.loadMore();
    } else if (ev.key === ' ' && S.focus >= 0) {
      ev.preventDefault();
      const e = S.items[S.focus];
      if (ev.ctrlKey || ev.metaKey) {
        const r = clickSelect({ ids: S.items.map((x) => x.id), selected: S.selected, anchor: S.anchor }, S.focus, { toggle: true });
        setSelection(r.selected, { anchor: r.anchor });
      } else if (e && !e.is_dir) open(e, { preview: true });
    }
  });

  // Focus stays on the list container until an arrow key picks the first item.

  function gridColumns() {
    if (!gridEl) return 1;
    const cols = getComputedStyle(gridEl).gridTemplateColumns.split(' ').filter(Boolean).length;
    return Math.max(1, cols);
  }

  /* --------------------------------------------------- rubber band */
  let bandStart = null;
  let bandMoved = false;
  main.addEventListener('pointerdown', (ev) => {
    bandMoved = false;
    if (ev.button !== 0 || S.view !== 'grid' || itemFromEvent(ev) || ev.target.closest('button, a, input, select, .infinite-sentinel')) return;
    const r = main.getBoundingClientRect();
    if (ev.clientX > r.right - 14) return; // scrollbar
    bandStart = { x: ev.clientX, y: ev.clientY + main.scrollTop, base: ev.ctrlKey || ev.metaKey || ev.shiftKey ? new Set(S.selected) : new Set() };
    main.setPointerCapture(ev.pointerId);
  });
  main.addEventListener('pointermove', (ev) => {
    if (!bandStart) return;
    const mr = main.getBoundingClientRect();
    const y = ev.clientY + main.scrollTop;
    if (!bandMoved && Math.hypot(ev.clientX - bandStart.x, y - bandStart.y) < 5) return;
    bandMoved = true;
    const rect = rectFrom(bandStart.x, bandStart.y - main.scrollTop, ev.clientX, ev.clientY);
    band.hidden = false;
    band.style.left = `${rect.left - mr.left}px`;
    band.style.top = `${rect.top - mr.top + main.scrollTop}px`;
    band.style.width = `${rect.right - rect.left}px`;
    band.style.height = `${rect.bottom - rect.top}px`;
    const next = new Set(bandStart.base);
    for (const [id, n] of nodes) if (intersects(rect, n.el.getBoundingClientRect())) next.add(id);
    S.selected = next;
    paintSelection();
    if (ev.clientY > mr.bottom - 24) main.scrollTop += 16;
    else if (ev.clientY < mr.top + 24) main.scrollTop -= 16;
  });
  const endBand = (ev) => {
    if (!bandStart) return;
    bandStart = null;
    band.hidden = true;
    try { main.releasePointerCapture(ev.pointerId); } catch { /* not captured */ }
    if (bandMoved) showDetails();
  };
  main.addEventListener('pointerup', endBand);
  main.addEventListener('pointercancel', endBand);

  /* ------------------------------------------------------ drag & drop */
  let dragIds = null;
  let dropEl = null;
  let dragDepth = 0;

  root.addEventListener('dragstart', (ev) => {
    const it = itemFromEvent(ev);
    if (!it) return;
    if (!S.selected.has(it.entry.id)) setSelection(new Set([it.entry.id]), { anchor: it.index });
    dragIds = [...S.selected];
    ev.dataTransfer.effectAllowed = 'copyMove';
    ev.dataTransfer.setData(DRAG_TYPE, JSON.stringify(dragIds));
    ev.dataTransfer.setData('text/plain', selectedEntries().map((e) => e.name).join('\n'));
  });
  root.addEventListener('dragend', () => {
    dragIds = null;
    setDropTarget(null);
  });

  function dropTargetFrom(ev) {
    const el = ev.target.closest?.('[data-drop]');
    if (el && root.contains(el)) {
      if (dragIds && el.dataset.id && dragIds.includes(el.dataset.id)) return null;
      return el;
    }
    return null;
  }

  function setDropTarget(el) {
    if (dropEl === el) return;
    dropEl?.classList.remove('is-drop-target');
    dropEl = el;
    dropEl?.classList.add('is-drop-target');
  }

  function targetSpaceWritable(spaceId) {
    const sp = S.spaces.find((s) => s.id === spaceId);
    return !!sp && sp.access !== 'read';
  }

  root.addEventListener('dragenter', (ev) => {
    if (!hasOSFiles(ev.dataTransfer)) return;
    dragDepth += 1;
    if (S.mode === 'browse' && writable()) {
      dropOverlay.hidden = false;
      dropOverlay.querySelector('.fx-drop-title').textContent = `Drop to upload to ${S.path ? S.path.split('/').pop() : spaceName(S.spaceId)}`;
    }
  });
  root.addEventListener('dragleave', () => {
    if (dragDepth > 0) dragDepth -= 1;
    if (dragDepth === 0) {
      dropOverlay.hidden = true;
      setDropTarget(null);
    }
  });
  root.addEventListener('dragover', (ev) => {
    const os = hasOSFiles(ev.dataTransfer);
    const internal = hasEntries(ev.dataTransfer);
    if (!os && !internal) return;
    const t = dropTargetFrom(ev);
    const inMain = center.contains(ev.target);
    const spaceId = t ? t.dataset.space : S.spaceId;
    const ok = (t || (inMain && S.mode === 'browse' && os)) && targetSpaceWritable(spaceId);
    setDropTarget(ok && t ? t : null);
    if (!ok) {
      ev.dataTransfer.dropEffect = 'none';
      return;
    }
    ev.preventDefault();
    ev.dataTransfer.dropEffect = os ? 'copy' : (ev.ctrlKey || ev.altKey ? 'copy' : 'move');
    if (os) {
      const label = t ? (t.dataset.path ? t.dataset.path.split('/').pop() : spaceName(t.dataset.space)) : (S.path ? S.path.split('/').pop() : spaceName(S.spaceId));
      dropOverlay.querySelector('.fx-drop-title').textContent = `Drop to upload to ${label}`;
    }
  });
  root.addEventListener('drop', (ev) => {
    const os = hasOSFiles(ev.dataTransfer);
    const internal = hasEntries(ev.dataTransfer);
    dragDepth = 0;
    dropOverlay.hidden = true;
    const t = dropTargetFrom(ev);
    setDropTarget(null);
    if (!os && !internal) return;
    ev.preventDefault();
    const spaceId = t ? t.dataset.space : S.spaceId;
    const path = t ? t.dataset.path : S.path;
    if (!targetSpaceWritable(spaceId) || (!t && S.mode !== 'browse')) return;
    if (internal) {
      let ids;
      try {
        ids = JSON.parse(ev.dataTransfer.getData(DRAG_TYPE));
      } catch { return; }
      const entries = ids.map((id) => S.byId.get(id)).filter(Boolean);
      if (!entries.length) return;
      const copy = ev.ctrlKey || ev.altKey;
      if (!copy && entries.every((e) => e.space_id === spaceId && parentPath(e.path) === path)) return;
      transfer(copy ? 'copy' : 'move', entries, { spaceId, path });
      return;
    }
    const pending = readDrop(ev.dataTransfer);
    pending.then(({ files, dirs }) => uploadPlanned(files, dirs, spaceId, path)).catch((err) => ui.toast.error(err));
  });

  /* ------------------------------------------------------------ actions */
  function open(e, { preview = false } = {}) {
    if (!e) return;
    if (e.is_dir) {
      navigate(e.space_id, e.path);
      return;
    }
    // Previewable files step through the other previewable files of the listing.
    const pk = previewKind(e);
    const entries = pk ? S.items.filter((x) => !x.is_dir && previewKind(x)) : [e];
    const idx = Math.max(0, entries.findIndex((x) => x.id === e.id));
    openPreview(ctx, fapi, { entries, index: preview || pk ? idx : 0, onDetails: (x) => focusDetails(x) });
  }

  function focusDetails(e) {
    const i = S.items.findIndex((x) => x.id === e.id);
    if (i >= 0) setSelection(new Set([e.id]), { anchor: i });
    toggleDetails(true);
  }

  function itemMenu(sel) {
    const one = sel.length === 1 ? sel[0] : null;
    const canWrite = writable() || S.mode === 'search';
    const items = [];
    if (one) {
      items.push({ label: one.is_dir ? 'Open' : previewKind(one) ? 'Preview' : 'Open', icon: one.is_dir ? 'folder-open' : 'eye', shortcut: 'Enter', onSelect: () => open(one) });
      if (S.mode === 'search') items.push({ label: 'Show in folder', icon: 'folder', onSelect: () => revealInFolder(one) });
      if (isMedia(one)) items.push({ label: 'Open in TV library', icon: 'tv', onSelect: () => ctx.router.navigate(`/tv/library/item/${encodeURIComponent(one.id)}`) });
    }
    items.push({ label: one?.is_dir ? 'Download as ZIP' : 'Download', icon: 'download', onSelect: () => runAction('download', sel) });
    items.push({ separator: true });
    if (one && canWrite) items.push({ label: 'Rename', icon: 'edit', shortcut: 'F2', onSelect: () => runAction('rename', sel) });
    if (canWrite) items.push({ label: 'Move to…', icon: 'arrow-right', onSelect: () => runAction('move', sel) });
    items.push({ label: 'Copy to…', icon: 'copy', onSelect: () => runAction('copy', sel) });
    if (canWrite) items.push({ label: 'Cut', icon: 'minus', shortcut: kbdLabel('X'), onSelect: () => clip('cut', sel) });
    items.push({ label: 'Copy', icon: 'copy', shortcut: kbdLabel('C'), onSelect: () => clip('copy', sel) });
    if (canWrite) items.push({ label: 'Categorize & tag…', icon: 'tag', onSelect: () => runAction('categorize', sel) });
    items.push({ label: 'Details', icon: 'info', shortcut: 'I', onSelect: () => toggleDetails(true) });
    if (canWrite) {
      items.push({ separator: true });
      items.push({ label: 'Delete', icon: 'trash', danger: true, shortcut: 'Del', onSelect: () => runAction('delete', sel) });
    }
    return items;
  }

  function backgroundMenu() {
    const w = writable() && S.mode === 'browse';
    return [
      w ? { label: 'New folder', icon: 'plus', shortcut: 'Shift+N', onSelect: () => newFolder() } : null,
      w ? { label: 'Upload files…', icon: 'upload', shortcut: 'U', onSelect: () => uploadPicked(false) } : null,
      w ? { label: 'Upload folder…', icon: 'folder', onSelect: () => uploadPicked(true) } : null,
      w && S.clipboard ? { label: `Paste ${plural(S.clipboard.entries.length, 'item')}`, icon: 'copy', shortcut: kbdLabel('V'), onSelect: () => paste() } : null,
      w ? { separator: true } : null,
      { label: 'Select all', icon: 'check', shortcut: kbdLabel('A'), onSelect: () => selectAll() },
      { label: 'Refresh', icon: 'refresh', onSelect: () => load() },
      w ? { label: 'Rescan from disk', icon: 'database', onSelect: () => rescan() } : null,
      S.mode === 'browse' && S.dir && S.path ? { label: 'Download folder as ZIP', icon: 'download', onSelect: () => fapi.downloadZip(S.dir.id) } : null,
    ].filter(Boolean);
  }

  function kbdLabel(k) {
    return /mac|iphone|ipad/i.test(navigator.platform || '') ? `⌘${k}` : `Ctrl+${k}`;
  }

  function revealInFolder(e) {
    navigate(e.space_id, parentPath(e.path));
  }

  async function runAction(action, sel) {
    if (!sel?.length) return;
    try {
      switch (action) {
        case 'open': open(sel[0]); break;
        case 'rename': await rename(sel[0]); break;
        case 'delete': await trashEntries(sel); break;
        case 'move':
        case 'copy': {
          const dest = await pickDestination(ctx, fapi, {
            title: `${action === 'move' ? 'Move' : 'Copy'} ${sel.length === 1 ? `“${sel[0].name}”` : plural(sel.length, 'item')}`,
            confirmLabel: action === 'move' ? 'Move here' : 'Copy here', spaces: S.spaces,
            initialSpace: S.spaceId, initialPath: S.path, sources: action === 'move' ? sel : [],
          });
          if (dest) await transfer(action, sel, dest);
          break;
        }
        case 'categorize': {
          const cats = await loadCategories();
          const patch = await categorizeDialog(ctx, { categories: cats, entries: sel });
          if (!patch) return;
          const res = await fapi.bulkMeta({ ids: sel.map((e) => e.id), ...patch });
          if (res.errors?.length) ui.toast.warning(`${plural(res.errors.length, 'item')} not updated`, { description: res.errors[0].error });
          else ui.toast.success(`Updated ${plural(res.updated, 'item')}`);
          await refreshEntries(sel.map((e) => e.id));
          break;
        }
        case 'download': downloadEntries(sel); break;
        default:
      }
    } catch (err) {
      ui.toast.error(err);
    }
  }

  async function refreshEntries(ids) {
    for (const id of ids) {
      try {
        upsert(await fapi.get(id, { signal: ctx.signal }));
      } catch { /* gone */ }
    }
    showDetails();
  }

  function downloadEntries(sel) {
    sel.forEach((e, i) => setTimeout(() => (e.is_dir ? fapi.downloadZip(e.id) : fapi.download(e.id)), i * 400));
    if (sel.length > 1) ui.toast.info(`Downloading ${plural(sel.length, 'item')}`, { description: 'Folders are downloaded as ZIP files. Your browser may ask to allow multiple downloads.' });
  }

  async function newFolder() {
    if (!writable() || S.mode !== 'browse') return;
    const taken = new Set(S.items.map((e) => e.name));
    const name = await promptName(ctx, { title: 'New folder', label: 'Folder name', value: freeName('New folder', true, taken), confirmLabel: 'Create' });
    if (!name) return;
    try {
      const e = await fapi.mkdir(S.spaceId, S.path, name);
      upsert(e);
      const i = S.items.findIndex((x) => x.id === e.id);
      setSelection(new Set([e.id]), { anchor: i });
      setFocus(i);
      tree.refresh(S.spaceId, S.path);
    } catch (err) {
      ui.toast.error(err);
    }
  }

  async function rename(e) {
    const name = await promptName(ctx, { title: e.is_dir ? 'Rename folder' : 'Rename file', label: 'New name', value: e.name, isDir: e.is_dir, confirmLabel: 'Rename' });
    if (!name || name === e.name) return;
    const upd = await fapi.patch(e.id, { name });
    upsert(upd);
    if (e.is_dir) tree.refresh(e.space_id, parentPath(e.path));
    ui.toast.success('Renamed');
  }

  async function trashEntries(sel) {
    const res = await fapi.trash(sel.map((e) => e.id));
    removeItems(res.deleted);
    if (sel.some((e) => e.is_dir)) tree.refresh(S.spaceId, S.path);
    refreshSpaces();
    if (res.errors.length) ui.toast.error(`${plural(res.errors.length, 'item')} could not be deleted`, { description: res.errors[0].error });
    if (res.deleted.length) {
      ui.toast.success(`Moved ${plural(res.deleted.length, 'item')} to the trash`, {
        action: {
          label: 'Undo',
          onClick: async () => {
            for (const id of res.deleted) {
              try {
                await fapi.restore(id);
              } catch (err) {
                ui.toast.error(err);
              }
            }
            load();
            tree.refresh(S.spaceId, S.path);
          },
        },
      });
    }
  }

  /**
   * Move or copy with interactive conflict resolution (Replace / Keep both / Skip).
   */
  async function transfer(kind, entries, dest) {
    const ids = entries.map((e) => e.id);
    const call = kind === 'move' ? fapi.move : fapi.copy;
    const key = kind === 'move' ? 'moved' : 'copied';
    const sameDir = entries.every((e) => e.space_id === dest.spaceId && parentPath(e.path) === dest.path);
    let res;
    try {
      res = await call(ids, dest.spaceId, dest.path, kind === 'copy' && sameDir ? 'rename' : 'fail');
    } catch (err) {
      ui.toast.error(err);
      return;
    }
    let done = res[key];
    const { conflicts, other } = splitErrors(res.errors);
    if (conflicts.length) {
      const names = conflicts.map((c) => entries.find((e) => e.id === c.id)?.name || S.byId.get(c.id)?.name || c.id);
      const choice = await askConflict(ctx, { count: conflicts.length, names, verb: kind });
      if (choice === 'overwrite' || choice === 'rename') {
        try {
          const r2 = await call(conflicts.map((c) => c.id), dest.spaceId, dest.path, choice);
          done = done.concat(r2[key]);
          other.push(...r2.errors);
        } catch (err) {
          ui.toast.error(err);
        }
      }
    }
    if (other.length) ui.toast.error(`${plural(other.length, 'item')} could not be ${key}`, { description: other[0].error });
    if (done.length) {
      const where = [spaceName(dest.spaceId), ...dest.path.split('/').filter(Boolean)].join(' › ');
      ui.toast.success(`${kind === 'move' ? 'Moved' : 'Copied'} ${plural(done.length, 'item')} to ${where}`, {
        action: { label: 'Show', onClick: () => navigate(dest.spaceId, dest.path) },
      });
    }
    if (kind === 'move') removeItems(done.map((e) => e.id).filter((id) => {
      const e = done.find((x) => x.id === id);
      return !(e.space_id === S.spaceId && parentPath(e.path) === S.path);
    }));
    for (const e of done) upsert(e);
    if (entries.some((e) => e.is_dir) || done.some((e) => e.is_dir)) {
      tree.refresh(S.spaceId, S.path);
      tree.refresh(dest.spaceId, dest.path);
    }
    refreshSpaces();
  }

  /* --------------------------------------------------------- clipboard */
  function clip(mode, sel) {
    if (!sel.length) return;
    S.clipboard = { mode, entries: sel.map((e) => ({ ...e })) };
    ui.toast.info(`${mode === 'cut' ? 'Cut' : 'Copied'} ${plural(sel.length, 'item')}`, { description: `Open the destination folder and press ${kbdLabel('V')} to paste.`, timeout: 3000 });
  }

  async function paste() {
    if (!S.clipboard || !writable() || S.mode !== 'browse') return;
    const { mode, entries } = S.clipboard;
    if (mode === 'cut' && entries.some((e) => e.is_dir && e.space_id === S.spaceId && (S.path === e.path || S.path.startsWith(`${e.path}/`)))) {
      ui.toast.error('A folder cannot be moved into itself.');
      return;
    }
    await transfer(mode === 'cut' ? 'move' : 'copy', entries, { spaceId: S.spaceId, path: S.path });
    if (mode === 'cut') S.clipboard = null;
  }

  function selectAll() {
    setSelection(new Set(S.items.map((e) => e.id)), { anchor: 0 });
  }

  async function rescan() {
    try {
      await fapi.rescan(S.spaceId, S.path);
      ui.toast.info('Rescanning from disk…', { description: 'New and changed files will appear shortly.' });
      setTimeout(() => { if (root.isConnected) load(); }, 2500);
    } catch (err) {
      ui.toast.error(err);
    }
  }

  async function refreshSpaces() {
    try {
      S.spaces = await fapi.spaces({ signal: ctx.signal });
      tree.updateSpaces(S.spaces);
    } catch { /* non-critical */ }
  }

  /* ----------------------------------------------------------- uploads */
  async function uploadPicked(directory) {
    if (!writable() || S.mode !== 'browse') return;
    const files = await pickFiles({ multiple: true, directory });
    if (!files.length) return;
    const picked = pickedFiles(files);
    const dirs = directory ? [...new Set(picked.map((p) => parentPath(p.relPath)).filter(Boolean))] : [];
    uploadPlanned(picked, dirs, S.spaceId, S.path);
  }

  async function uploadPlanned(files, dirs, spaceId, path) {
    const plan = planUpload(files, dirs, path);
    if (plan.problems.length) ui.toast.warning(`${plural(plan.problems.length, 'item')} skipped`, { description: plan.problems[0] });
    if (!plan.uploads.length && !plan.emptyDirs.length) return;
    // Name conflicts with what is visible right now (top-level files only).
    let uploads = plan.uploads;
    if (spaceId === S.spaceId && path === S.path && S.mode === 'browse') {
      const existing = new Map(S.items.map((e) => [e.name, e]));
      const clashes = uploads.filter((u) => u.dir === path && existing.has(u.name));
      if (clashes.length) {
        const choice = await askConflict(ctx, { count: clashes.length, names: clashes.map((c) => c.name), verb: 'upload' });
        if (!choice) return;
        const taken = new Set([...existing.keys(), ...uploads.map((u) => u.name)]);
        uploads = uploads.flatMap((u) => {
          if (!clashes.includes(u)) return [u];
          if (choice === 'skip') return [];
          if (choice === 'overwrite') return [{ ...u, overwrite: true }];
          const name = freeName(u.name, false, taken);
          taken.add(name);
          return [{ ...u, name }];
        });
      }
    }
    for (const d of plan.emptyDirs) {
      try {
        await mkdirP(spaceId, d);
      } catch (err) {
        ui.toast.error(err);
      }
    }
    for (const u of uploads) {
      const file = u.name === u.file.name ? u.file : renamedFile(u.file, u.name);
      ctx.uploads.add(file, { spaceId, path: u.dir, overwrite: !!u.overwrite });
    }
    if (uploads.length) {
      ui.toast.info(`Uploading ${plural(uploads.length, 'file')}`, { description: `to ${[spaceName(spaceId), ...path.split('/').filter(Boolean)].join(' › ')}`, timeout: 3000 });
    }
    if (plan.emptyDirs.length && spaceId === S.spaceId) load();
  }

  async function mkdirP(spaceId, full) {
    const parts = full.split('/');
    for (let i = 0; i < parts.length; i++) {
      try {
        await fapi.mkdir(spaceId, parts.slice(0, i).join('/'), parts[i]);
      } catch (err) {
        if (err.code !== 'conflict') throw err;
      }
    }
  }

  function renamedFile(file, name) {
    try {
      return new File([file], name, { type: file.type, lastModified: file.lastModified });
    } catch {
      return Object.assign(file.slice(0, file.size, file.type), { name, lastModified: file.lastModified });
    }
  }

  const reloadSoon = debounce(() => { if (root.isConnected && S.mode === 'browse') load(); }, 600);
  const offComplete = ctx.uploads.on('complete', async (u) => {
    const sid = u.opts?.spaceId;
    const dir = u.opts?.path || '';
    if (sid !== S.spaceId || S.mode !== 'browse') {
      refreshSpaces();
      return;
    }
    if (dir === S.path && u.fileId) {
      try {
        upsert(await fapi.get(u.fileId, { signal: ctx.signal }));
      } catch { /* ignore */ }
    } else if (dir.startsWith(S.path ? `${S.path}/` : '')) {
      const child = dir.slice(S.path ? S.path.length + 1 : 0).split('/')[0];
      if (child && !S.items.some((e) => e.is_dir && e.name === child)) {
        reloadSoon();
        tree.refresh(S.spaceId, S.path);
      }
    }
    refreshSpacesSoon();
  });
  const refreshSpacesSoon = debounce(() => refreshSpaces(), 1500);
  ctx.onCleanup(offComplete);

  /* --------------------------------------------------------- shortcuts */
  // Only while the file list (or nothing in particular) has focus, so toolbar buttons,
  // the tree and the details panel keep their own Enter/Delete/typing behaviour.
  const when = () => {
    const a = document.activeElement;
    return active() && (!a || a === document.body || main.contains(a)) && !(isEditable(a) && a.type !== 'checkbox');
  };
  const sc = (combo, fn, description) => ctx.shortcuts.register(combo, fn, { description, when });
  sc('enter', () => { const e = S.items[S.focus] || selectedEntries()[0]; if (e) open(e); }, 'Open');
  sc('backspace', () => { if (S.mode === 'browse' && S.path) navigate(S.spaceId, parentPath(S.path)); }, 'Up one folder');
  sc('delete', () => { const sel = selectedEntries(); if (sel.length && (writable() || S.mode === 'search')) runAction('delete', sel); }, 'Move to trash');
  sc('f2', () => { const sel = selectedEntries(); if (sel.length === 1 && (writable() || S.mode === 'search')) runAction('rename', sel); }, 'Rename');
  sc('mod+a', () => selectAll(), 'Select all');
  sc('mod+c', () => clip('copy', selectedEntries()), 'Copy');
  sc('mod+x', () => { if (writable() || S.mode === 'search') clip('cut', selectedEntries()); }, 'Cut');
  sc('mod+v', () => paste(), 'Paste');
  sc('escape', () => { if (S.selected.size) setSelection(new Set()); else if (S.mode === 'search') clearSearch(); }, 'Clear selection');
  sc('i', () => toggleDetails(), 'Toggle details');
  sc('shift+n', () => newFolder(), 'New folder');
  sc('u', () => uploadPicked(false), 'Upload files');

  ctx.commands.register([
    { id: 'files:upload', title: 'Upload files to this folder…', icon: 'upload', run: () => uploadPicked(false) },
    { id: 'files:upload-folder', title: 'Upload a folder…', icon: 'folder', run: () => uploadPicked(true) },
    { id: 'files:new-folder', title: 'New folder', icon: 'plus', run: () => newFolder() },
    { id: 'files:trash', title: 'Open trash', icon: 'trash', run: () => ctx.router.navigate('/files/trash') },
  ]);

  /* ------------------------------------------------------------ search */
  function searchCfg() {
    return {
      placeholder: 'Search files',
      value: S.q,
      onInput: (q) => setQueryText(q),
      onSubmit: (q) => setQueryText(q),
    };
  }
  function setQueryText(q) {
    const text = q.trim();
    if (text === S.q) return;
    if (!text && S.mode === 'search' && !S.filters.category && !S.filters.kind && !S.filters.tag) {
      navigate(S.spaceId, S.path, { replace: true });
      return;
    }
    const base = { space: S.spaceId, q: text || null };
    if (S.mode === 'search') ctx.router.setQuery({ q: text || null });
    else ctx.router.navigate(`/files${buildQS(base)}`);
  }
  ctx.setSearch(searchCfg());

  // Track category names for list badges.
  loadCategories().then((cats) => { S.catList = cats; syncToolbar(); if (S.items.length) rerenderKeepScroll(); }).catch(() => {});

  toggleDetails(S.details);
  renderItems();
  // A deep link to the trash must not flash the browse toolbar and an empty file table while
  // the spaces load (applyRoute then renders the trash itself).
  if (routeState(ctx.params?.rest || '', ctx.query || {}).mode === 'trash') {
    root.classList.add('fx-mode-trash');
    center.hidden = true;
    crumbs.replaceChildren(h('div', { class: 'fx-search-title' }, icon('trash', 16), h('span', {}, 'Trash')));
  }
  init();

  return {
    update(next) {
      Object.assign(ctx, { params: next.params, query: next.query });
      if (S.loaded) applyRoute(next);
    },
    unmount() {
      list?.destroy();
      trashView?.destroy();
      reloadSoon.cancel();
      refreshSpacesSoon.cancel();
    },
  };
}

