// The unlocked vault: sidebar (all items, favorites, types, folder tree, tools), the item list
// (virtualised, keyboard navigable) and the detail pane; tool pages replace list + detail.

import { h, cls, uid, mount as fill } from '../../../core/dom.js';
import { icon } from '../../../core/icons.js';
import * as ui from '../../../core/ui.js';
import { TYPES, TYPE_INFO, SORTS, buildFolderTree, folderPathError, foldersUnder, haystack, inFolder, matchesQuery,
  normalizePath, renamePath, sortEntries, subtitle } from '../model.js';
import { ConflictError } from '../session.js';
import { vicon, itemAvatar } from '../widgets.js';
import { renderDetail } from './detail.js';
import { openEditor } from './editor.js';

const TOOLS = [
  { id: 'generator', label: 'Generator', icon: 'dice', load: () => import('./generator.js') },
  { id: 'security', label: 'Security report', icon: 'shield-check', load: () => import('./security.js') },
  { id: 'import', label: 'Import', icon: 'upload', load: () => import('./importer.js') },
  { id: 'export', label: 'Export', icon: 'download', load: () => import('./exporter.js') },
  { id: 'settings', label: 'Vault settings', icon: 'settings', load: () => import('./settings.js') },
];

const ROW_H = 56;
const OVERSCAN = 8;

/**
 * @param {HTMLElement} host
 * @param {{vault: object, ctx: object}} o
 * @returns {{update: (ctx: object) => void, destroy: () => void}}
 */
export function renderMain(host, { vault, ctx: ctx0 }) {
  let ctx = ctx0;
  const { session } = vault;
  const st = vault.ui;
  st.collapsed ||= new Set();
  const cleanups = [];
  let toolCleanup = null;
  let detailCleanup = null;
  let currentTool = null;
  let entries = []; // filtered + sorted list entries
  let all = []; // every decrypted entry
  let broken = []; // undecryptable records

  /* ------------------------------------------------------------------ layout */
  const nav = h('nav', { class: 'vault-nav', aria: { label: 'Vault' } });
  const lockBtn = ui.button({ icon: 'lock', title: 'Lock vault (Shift+L)', variant: 'ghost', size: 'sm', onClick: () => vault.lock('') });
  const side = h('aside', { class: 'vault-side', id: uid('vside') },
    h('div', { class: 'vault-side-head' }, h('span', { class: 'vault-side-title' }, vicon('vault', 18), 'Passwords'), lockBtn),
    nav);
  const backdrop = h('div', { class: 'vault-side-backdrop', aria: { hidden: 'true' } });
  backdrop.addEventListener('click', () => toggleSide(false));
  side.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && app.classList.contains('side-open')) {
      e.preventDefault();
      toggleSide(false);
      app.querySelector('.vault-side-toggle')?.focus();
    }
  });
  const content = h('div', { class: 'vault-content' });
  const app = h('div', { class: 'vault-app' }, side, backdrop, content);
  host.replaceChildren(app);

  function toggleSide(open) {
    app.classList.toggle('side-open', open);
    if (open) side.querySelector('.vnav-item.is-active, .vnav-item')?.focus();
  }
  const sideToggle = () => {
    const b = ui.button({ icon: 'panel-left', title: 'Show filters and folders', variant: 'ghost', size: 'sm', className: 'vault-side-toggle' });
    b.setAttribute('aria-controls', side.id);
    b.addEventListener('click', () => toggleSide(!app.classList.contains('side-open')));
    return b;
  };

  /* ------------------------------------------------------------- list pane */
  const listTitle = h('h2', { class: 'vault-list-title' });
  const listCount = h('span', { class: 'vault-list-count' });
  const sortBtn = ui.button({ icon: 'sort', title: 'Sort', variant: 'ghost', size: 'sm' });
  sortBtn.addEventListener('click', () => ui.openMenu({
    anchor: sortBtn, align: 'end',
    items: [{ heading: 'Sort by' }, ...SORTS.map((s) => ({
      label: s.label, checked: vault.prefs.sort === s.value,
      onSelect: () => { vault.setPrefs({ sort: s.value }); refreshList(); },
    }))],
  }));
  const newBtn = ui.button({ label: 'New', icon: 'plus', variant: 'primary', size: 'sm', iconRight: 'chevron-down' });
  newBtn.addEventListener('click', () => ui.openMenu({
    anchor: newBtn, align: 'end',
    items: TYPES.map((t) => ({ label: TYPE_INFO[t].label, icon: TYPE_INFO[t].icon, onSelect: () => newItem(t) })),
  }));
  const spacer = h('div', { class: 'vault-list-spacer' });
  const list = h('div', { class: 'vault-list', role: 'listbox', tabindex: '0', aria: { label: 'Items' } }, spacer);
  const listEmpty = h('div', { class: 'vault-list-empty', hidden: true });
  const listPane = h('section', { class: 'vault-list-pane', aria: { label: 'Item list' } },
    h('header', { class: 'vault-list-head' }, sideToggle(), h('div', { class: 'vault-list-heading' }, listTitle, listCount),
      h('div', { class: 'row' }, sortBtn, newBtn)),
    list, listEmpty);
  const detail = h('section', { class: 'vault-detail-pane', aria: { label: 'Item details' }, tabindex: '-1' });
  const browser = h('div', { class: 'vault-browser' }, listPane, detail);
  const toolHost = h('div', { class: 'vault-tool' });

  /* ----------------------------------------------------------------- data */
  function rebuild() {
    all = [];
    broken = [];
    for (const r of session.items.values()) {
      if (!r.view) {
        broken.push(r);
        continue;
      }
      const folderPath = session.folderPath(r.folder_id);
      all.push({ id: r.id, rec: r, view: r.view, folderPath, hay: haystack(r.view, folderPath), updated: r.view.updated_at || r.updated_at, created: r.view.created_at || r.created_at });
    }
  }

  function matchesFilter(e, f) {
    switch (f.kind) {
      case 'favorites': return e.view.favorite;
      case 'type': return e.view.type === f.type;
      case 'folder': return !!e.folderPath && inFolder(e.folderPath, f.path);
      case 'nofolder': return !e.folderPath;
      default: return true;
    }
  }

  function filterTitle(f) {
    switch (f.kind) {
      case 'favorites': return 'Favorites';
      case 'type': return TYPE_INFO[f.type].plural;
      case 'folder': return f.path.split('/').pop();
      case 'nofolder': return 'No folder';
      case 'broken': return 'Cannot decrypt';
      default: return 'All items';
    }
  }

  function refreshList({ keepScroll = true } = {}) {
    const f = st.filter;
    if (f.kind === 'broken') {
      entries = broken.map((r) => ({ id: r.id, rec: r, view: null, broken: true }));
    } else {
      entries = all.filter((e) => matchesFilter(e, f) && matchesQuery(e.hay, st.query));
      sortEntries(entries, vault.prefs.sort);
    }
    listTitle.textContent = st.query ? `Search: ${st.query}` : filterTitle(f);
    listCount.textContent = String(entries.length);
    if (!keepScroll) list.scrollTop = 0;
    spacer.style.height = `${entries.length * ROW_H}px`;
    listEmpty.hidden = entries.length > 0;
    list.hidden = entries.length === 0;
    if (!entries.length) {
      listEmpty.replaceChildren(st.query
        ? ui.emptyState({ icon: 'search', title: 'No matches', message: 'Try another search term.', compact: true })
        : all.length === 0 && f.kind === 'all'
          ? ui.emptyState({ icon: 'key', title: 'Your vault is empty', message: 'Add your first login or import from KeePass or Bitwarden.',
            action: h('div', { class: 'row wrap center' }, ui.button({ label: 'New login', icon: 'plus', variant: 'primary', onClick: () => { newItem('login'); } }),
              ui.button({ label: 'Import', icon: 'upload', onClick: () => ctx.router.navigate('/vault/import') })) })
          : ui.emptyState({ icon: 'layers', title: 'Nothing here', message: 'No items match this filter.', compact: true }));
    }
    paintRows(true);
    renderNav();
  }

  /* --------------------------------------------------------- virtual rows */
  const rowEls = new Map(); // index → element
  let activeIndex = -1;

  function rowFor(i) {
    const e = entries[i];
    const selected = e.id === st.selectedId;
    const id = `vrow-${e.id}`;
    const el = e.broken
      ? h('div', { class: cls('vault-row', 'is-broken', { 'is-selected': selected }), role: 'option', id, aria: { selected: String(selected) }, dataset: { index: i } },
        h('span', { class: 'vault-row-icon' }, icon('alert', 16)),
        h('div', { class: 'vault-row-text' }, h('div', { class: 'vault-row-name' }, 'Cannot decrypt this item'), h('div', { class: 'vault-row-sub mono' }, e.id)))
      : h('div', { class: cls('vault-row', { 'is-selected': selected }), role: 'option', id, aria: { selected: String(selected) }, dataset: { index: i } },
        itemAvatar(e.view.name, 32),
        h('div', { class: 'vault-row-text' },
          h('div', { class: 'vault-row-name' }, h('span', { class: 'truncate' }, e.view.name), e.view.favorite ? icon('star', { size: 12, className: 'vault-fav-mark', label: 'Favorite' }) : null),
          h('div', { class: 'vault-row-sub' }, subtitle(e.view) || TYPE_INFO[e.view.type].label)),
        e.view.type !== 'login' ? h('span', { class: 'vault-row-type', title: TYPE_INFO[e.view.type].label }, vicon(TYPE_INFO[e.view.type].icon, 14)) : null);
    el.style.transform = `translateY(${i * ROW_H}px)`;
    return el;
  }

  function paintRows(force = false) {
    const viewH = list.clientHeight || 600;
    const first = Math.max(0, Math.floor(list.scrollTop / ROW_H) - OVERSCAN);
    const last = Math.min(entries.length - 1, Math.ceil((list.scrollTop + viewH) / ROW_H) + OVERSCAN);
    if (force) {
      for (const el of rowEls.values()) el.remove();
      rowEls.clear();
    }
    for (const [i, el] of rowEls) {
      if (i < first || i > last) {
        el.remove();
        rowEls.delete(i);
      }
    }
    for (let i = first; i <= last; i++) {
      if (!rowEls.has(i)) {
        const el = rowFor(i);
        rowEls.set(i, el);
        list.append(el);
      }
    }
    activeIndex = entries.findIndex((e) => e.id === st.selectedId);
    if (activeIndex >= 0) list.setAttribute('aria-activedescendant', `vrow-${st.selectedId}`);
    else list.removeAttribute('aria-activedescendant');
  }

  let raf = 0;
  list.addEventListener('scroll', () => {
    if (raf) return;
    raf = requestAnimationFrame(() => {
      raf = 0;
      paintRows();
    });
  }, { passive: true });
  const ro = typeof ResizeObserver !== 'undefined' ? new ResizeObserver(() => paintRows()) : null;
  ro?.observe(list);
  cleanups.push(() => ro?.disconnect());

  list.addEventListener('click', (e) => {
    const row = e.target instanceof Element ? e.target.closest('.vault-row') : null;
    if (row) select(entries[Number(row.dataset.index)]?.id, { focusList: true });
  });
  list.addEventListener('dblclick', (e) => {
    const row = e.target instanceof Element ? e.target.closest('.vault-row') : null;
    const ent = row ? entries[Number(row.dataset.index)] : null;
    if (ent?.rec?.plain) openEdit(ent.rec);
  });
  list.addEventListener('keydown', (e) => {
    if (!entries.length) return;
    const page = Math.max(1, Math.floor(list.clientHeight / ROW_H) - 1);
    let i = activeIndex;
    switch (e.key) {
      case 'ArrowDown': i = Math.min(entries.length - 1, i + 1); break;
      case 'ArrowUp': i = Math.max(0, i - 1); break;
      case 'PageDown': i = Math.min(entries.length - 1, i + page); break;
      case 'PageUp': i = Math.max(0, i - page); break;
      case 'Home': i = 0; break;
      case 'End': i = entries.length - 1; break;
      case 'Enter':
        if (activeIndex >= 0) {
          e.preventDefault();
          detail.querySelector('button, a[href]')?.focus();
        }
        return;
      default: return;
    }
    e.preventDefault();
    select(entries[Math.max(0, i)].id, { focusList: true });
  });

  function scrollIntoView(i) {
    const top = i * ROW_H;
    if (top < list.scrollTop) list.scrollTop = top;
    else if (top + ROW_H > list.scrollTop + list.clientHeight) list.scrollTop = top + ROW_H - list.clientHeight;
  }

  /* ---------------------------------------------------------------- detail */
  function select(id, { focusList = false } = {}) {
    st.selectedId = id || null;
    const i = entries.findIndex((e) => e.id === st.selectedId);
    if (i >= 0) scrollIntoView(i);
    paintRows(true);
    showDetail();
    browser.classList.toggle('has-selection', !!st.selectedId);
    if (focusList) list.focus({ preventScroll: true });
  }

  function showDetail() {
    detailCleanup?.();
    detailCleanup = null;
    const rec = st.selectedId ? session.items.get(st.selectedId) : null;
    if (!rec) {
      st.selectedId = null;
      browser.classList.remove('has-selection');
      detail.replaceChildren(h('div', { class: 'vault-detail-empty' },
        ui.emptyState({ icon: 'key', title: 'Select an item', message: `${all.length} item${all.length === 1 ? '' : 's'} · end-to-end encrypted` })));
      return;
    }
    detailCleanup = renderDetail(detail, {
      vault, ctx, rec,
      onBack: () => select(null),
      onEdit: () => openEdit(rec),
      onClone: () => cloneItem(rec),
      onDelete: () => deleteItem(rec),
      onMove: () => moveItem(rec),
      onToggleFavorite: () => toggleFavorite(rec),
    });
  }

  /* --------------------------------------------------------------- actions */
  function defaultFolderId() {
    const f = st.filter;
    if (f.kind !== 'folder') return null;
    for (const fr of session.folders.values()) if (fr.path === f.path) return fr.id;
    return null;
  }

  async function newItem(type) {
    const rec = await openEditor({ vault, ctx, type, folderId: defaultFolderId() });
    if (rec) afterSave(rec);
  }
  async function openEdit(rec) {
    if (!rec.plain) return;
    const saved = await openEditor({ vault, ctx, rec });
    if (saved) afterSave(saved);
  }
  async function cloneItem(rec) {
    const copy = structuredClone(rec.plain);
    copy.name = `${copy.name || 'Untitled'} (copy)`;
    copy.password_history = [];
    const saved = await openEditor({ vault, ctx, type: copy.type, folderId: rec.folder_id, template: copy });
    if (saved) afterSave(saved);
  }
  function afterSave(rec) {
    if (st.query && !matchesQuery(haystack(rec.view, session.folderPath(rec.folder_id)), st.query)) {
      st.query = '';
      ctx.setSearch(searchCfg());
    }
    st.selectedId = rec.id;
    rebuild();
    refreshList();
    select(rec.id);
  }

  async function deleteItem(rec) {
    const name = rec.view?.name || 'this item';
    const ok = await ui.confirm({ title: 'Delete item?', message: `“${name}” will be deleted from your vault on every device. This cannot be undone.`, confirmLabel: 'Delete', danger: true });
    if (!ok) return;
    const i = entries.findIndex((e) => e.id === rec.id);
    try {
      await session.deleteItem(rec.id);
      ui.toast.success('Item deleted');
      const next = entries.filter((e) => e.id !== rec.id)[Math.min(i, entries.length - 2)];
      st.selectedId = next?.id || null;
    } catch (err) {
      ui.toast.error(err);
    }
  }

  async function toggleFavorite(rec) {
    const plain = structuredClone(rec.plain);
    plain.favorite = !(plain.favorite === true);
    plain.updated_at = new Date().toISOString();
    try {
      await session.saveItem({ plain, folderId: rec.folder_id, original: rec });
    } catch (err) {
      if (err instanceof ConflictError) ui.toast.warning('This item was changed on another device. Showing the latest version.');
      else ui.toast.error(err);
    }
  }

  async function moveItem(rec) {
    const paths = [...session.folders.values()].filter((f) => !f.error).sort((a, b) => a.path.localeCompare(b.path));
    const sel = h('select', { class: 'select', id: uid('mv') },
      h('option', { value: '' }, 'No folder'), paths.map((f) => h('option', { value: f.id }, f.path)));
    sel.value = rec.folder_id || '';
    const m = ui.modal({
      title: 'Move to folder', size: 'sm',
      body: h('div', { class: 'field' }, h('label', { class: 'field-label', for: sel.id }, 'Folder'), sel),
      footer: [{ label: 'Cancel', value: null }, { label: 'Move', variant: 'primary', type: 'submit' }],
      onSubmit: () => sel.value || '',
    });
    const target = await m.closed;
    if (target === undefined || target === null) return;
    if ((target || null) === (rec.folder_id || null)) return;
    try {
      await session.moveItem(rec, target || null);
      ui.toast.success('Item moved');
    } catch (err) {
      ui.toast.error(err instanceof ConflictError ? 'This item was changed on another device; try again.' : err);
    }
  }

  /* ------------------------------------------------------------- folders */
  function folderRecords() {
    return [...session.folders.values()].filter((f) => !f.error);
  }

  async function newFolder(parent = '') {
    const existing = folderRecords().map((f) => f.path);
    const name = await ui.prompt({
      title: parent ? `New folder in “${parent.split('/').pop()}”` : 'New folder', label: 'Folder name', confirmLabel: 'Create',
      hint: parent ? '' : 'Use “/” to nest folders, e.g. Work/Email.',
      validate: (v) => folderPathError(normalizePath(parent ? `${parent}/${v}` : v), existing),
    });
    if (name === null) return;
    const path = normalizePath(parent ? `${parent}/${name}` : name);
    try {
      await session.createFolder(path);
      if (parent) st.collapsed.delete(parent);
      ui.toast.success('Folder created');
    } catch (err) {
      ui.toast.error(err);
    }
  }

  async function renameFolder(node) {
    const recs = foldersUnder(folderRecords(), node.path);
    const others = folderRecords().filter((f) => !recs.includes(f)).map((f) => f.path);
    const parent = node.path.includes('/') ? node.path.slice(0, node.path.lastIndexOf('/')) : '';
    const name = await ui.prompt({
      title: 'Rename folder', label: 'Name', value: node.name, confirmLabel: 'Rename',
      validate: (v) => (v.includes('/') ? 'Folder names cannot contain “/”.' : folderPathError(normalizePath(parent ? `${parent}/${v}` : v), others)),
    });
    if (name === null) return;
    const to = normalizePath(parent ? `${parent}/${name}` : name);
    if (to === node.path) return;
    try {
      for (const f of recs) await session.renameFolder(f, renamePath(f.path, node.path, to));
      if (!node.id) await session.createFolder(to).catch(() => {});
      if (st.filter.kind === 'folder' && inFolder(st.filter.path, node.path)) st.filter = { kind: 'folder', path: renamePath(st.filter.path, node.path, to) };
      ui.toast.success('Folder renamed');
    } catch (err) {
      ui.toast.error(err instanceof ConflictError ? 'The folder was changed on another device; try again.' : err);
      session.sync().catch(() => {});
    }
  }

  async function deleteFolder(node) {
    const recs = foldersUnder(folderRecords(), node.path);
    const items = all.filter((e) => e.folderPath && inFolder(e.folderPath, node.path)).length;
    const subs = recs.filter((f) => f.path !== node.path).length;
    const ok = await ui.confirm({
      title: 'Delete folder?', danger: true, confirmLabel: 'Delete folder',
      message: `“${node.name}”${subs ? ` and ${subs} subfolder${subs === 1 ? '' : 's'}` : ''} will be deleted. ${items ? `${items} item${items === 1 ? '' : 's'} will move to “No folder”; nothing is deleted.` : 'It contains no items.'}`,
    });
    if (!ok) return;
    try {
      await session.deleteFolders(recs.map((f) => f.id));
      if (st.filter.kind === 'folder' && inFolder(st.filter.path, node.path)) st.filter = { kind: 'all' };
      ui.toast.success('Folder deleted');
    } catch (err) {
      ui.toast.error(err);
    }
  }

  function folderMenu(node, anchor, ev) {
    const items = [
      { label: 'New subfolder', icon: 'plus', onSelect: () => newFolder(node.path) },
      { label: 'Rename', icon: 'edit', onSelect: () => renameFolder(node) },
      { separator: true },
      { label: 'Delete', icon: 'trash', danger: true, onSelect: () => deleteFolder(node) },
    ];
    if (ev) ui.contextMenu(ev, items);
    else ui.openMenu({ anchor, items, align: 'end' });
  }

  /* ----------------------------------------------------------------- nav */
  function navItem({ label, ic, active, count, onClick, extra, depth = 0, expander = null, title }) {
    const b = h('button', { type: 'button', class: cls('vnav-item', { 'is-active': active }), aria: { current: active ? 'page' : undefined }, title });
    b.style.setProperty('--depth', String(depth));
    b.append(expander || h('span', { class: 'vnav-icon' }, vicon(ic, 16)), h('span', { class: 'vnav-text truncate' }, label));
    if (count !== undefined) b.append(h('span', { class: 'vnav-count' }, String(count)));
    b.addEventListener('click', onClick);
    return extra ? h('div', { class: 'vnav-row' }, b, extra) : b;
  }

  function setFilter(f) {
    st.filter = f;
    if (currentTool) ctx.router.navigate('/vault');
    toggleSide(false);
    refreshList({ keepScroll: false });
    if (st.selectedId && !entries.some((e) => e.id === st.selectedId)) select(null);
  }

  function renderNav() {
    const f = st.filter;
    const onList = !currentTool;
    const is = (kind, extra = () => true) => onList && f.kind === kind && extra();
    const count = (pred) => all.reduce((n, e) => n + (pred(e) ? 1 : 0), 0);
    const tree = buildFolderTree(folderRecords());
    const folderNodes = [];
    const walk = (node) => {
      for (const c of node.children) {
        const open = !st.collapsed.has(c.path);
        const exp = c.children.length
          ? h('span', { class: 'vnav-icon vnav-expander', role: 'button', tabindex: '-1', aria: { label: open ? 'Collapse' : 'Expand', expanded: String(open) } }, icon(open ? 'chevron-down' : 'chevron-right', 14))
          : h('span', { class: 'vnav-icon' }, icon('folder', 15));
        exp.addEventListener('click', (e) => {
          if (!c.children.length) return;
          e.stopPropagation();
          if (open) st.collapsed.add(c.path);
          else st.collapsed.delete(c.path);
          renderNav();
        });
        const more = h('button', { type: 'button', class: 'btn btn-ghost btn-icon btn-sm vnav-more', aria: { label: `Folder actions for ${c.name}` } }, icon('more', 14));
        more.addEventListener('click', () => folderMenu(c, more));
        const row = navItem({
          label: c.name, depth: c.depth, expander: exp, title: c.path,
          active: is('folder', () => f.path === c.path),
          count: count((e) => !!e.folderPath && inFolder(e.folderPath, c.path)),
          onClick: () => setFilter({ kind: 'folder', path: c.path }), extra: more,
        });
        row.addEventListener('contextmenu', (e) => {
          e.preventDefault();
          folderMenu(c, more, e);
        });
        folderNodes.push(row);
        if (open) walk(c);
      }
    };
    walk(tree);
    const addFolder = ui.button({ icon: 'plus', title: 'New folder', variant: 'ghost', size: 'sm', onClick: () => newFolder('') });
    fill(nav,
      navItem({ label: 'All items', ic: 'layers', active: is('all') && !st.query, count: all.length, onClick: () => setFilter({ kind: 'all' }) }),
      navItem({ label: 'Favorites', ic: 'star', active: is('favorites'), count: count((e) => e.view.favorite), onClick: () => setFilter({ kind: 'favorites' }) }),
      h('div', { class: 'vnav-heading' }, 'Types'),
      TYPES.map((t) => navItem({ label: TYPE_INFO[t].plural, ic: TYPE_INFO[t].icon, active: is('type', () => f.type === t), count: count((e) => e.view.type === t), onClick: () => setFilter({ kind: 'type', type: t }) })),
      h('div', { class: 'vnav-heading' }, h('span', {}, 'Folders'), addFolder),
      folderNodes,
      navItem({ label: 'No folder', ic: 'folder-open', active: is('nofolder'), count: count((e) => !e.folderPath), onClick: () => setFilter({ kind: 'nofolder' }) }),
      broken.length ? navItem({ label: 'Cannot decrypt', ic: 'alert', active: is('broken'), count: broken.length, onClick: () => setFilter({ kind: 'broken' }) }) : null,
      h('div', { class: 'vnav-heading' }, 'Tools'),
      TOOLS.map((t) => navItem({ label: t.label, ic: t.icon, active: currentTool === t.id, onClick: () => { toggleSide(false); ctx.router.navigate(`/vault/${t.id}`); } })),
    );
  }

  /* -------------------------------------------------------------- routing */
  function searchCfg() {
    return {
      placeholder: 'Search passwords', value: st.query,
      onInput: (q) => {
        st.query = q.trim();
        if (currentTool) ctx.router.navigate('/vault');
        else refreshList({ keepScroll: false });
      },
      onSubmit: () => {
        if (entries.length) select(entries[0].id, { focusList: true });
      },
    };
  }

  async function route(c) {
    ctx = c;
    const tool = TOOLS.find((t) => t.id === c.params.rest) || null;
    if (c.params.rest && !tool) {
      c.router.navigate('/vault', { replace: true });
      return;
    }
    toolCleanup?.();
    toolCleanup = null;
    currentTool = tool?.id || null;
    if (!tool) {
      ctx.setTitle('Passwords');
      if (content.firstChild !== browser) content.replaceChildren(browser);
      refreshList();
      showDetail();
      requestAnimationFrame(() => paintRows());
      return;
    }
    ctx.setTitle(`${tool.label} · Passwords`);
    toolHost.replaceChildren(ui.loadingBlock());
    content.replaceChildren(toolHost);
    renderNav();
    try {
      const mod = await tool.load();
      if (currentTool !== tool.id) return;
      const page = h('div', { class: 'vault-tool-page' });
      toolHost.replaceChildren(h('div', { class: 'vault-tool-head' }, sideToggle()), page);
      toolCleanup = mod.render(page, { vault, ctx, openItem: (id) => { st.filter = { kind: 'all' }; st.query = ''; ctx.setSearch(searchCfg()); st.selectedId = id; ctx.router.navigate('/vault'); } }) || null;
    } catch (err) {
      toolHost.replaceChildren(ui.errorState(err, () => route(ctx)));
    }
  }

  /* ---------------------------------------------------------------- wiring */
  cleanups.push(session.subscribe((what) => {
    if (what !== 'data') return;
    rebuild();
    if (!currentTool) {
      refreshList();
      showDetail();
    } else renderNav();
  }));
  ctx.setSearch(searchCfg());
  // Registrations return their removers: drop them when the vault locks (the app stays mounted).
  cleanups.push(ctx.shortcuts.register('n', () => newItem('login'), { description: 'New login' }));
  cleanups.push(ctx.shortcuts.register('e', () => {
    const rec = st.selectedId && session.items.get(st.selectedId);
    if (rec && !currentTool) openEdit(rec);
  }, { description: 'Edit selected item' }));
  cleanups.push(ctx.shortcuts.register('shift+l', () => vault.lock(''), { description: 'Lock vault' }));
  cleanups.push(ctx.commands.register([
    { id: 'vault.lock', title: 'Lock vault', icon: 'lock', keywords: 'passwords lock', run: () => vault.lock('') },
    { id: 'vault.new-login', title: 'New login', icon: 'plus', keywords: 'passwords add create', run: () => { ctx.router.navigate('/vault'); newItem('login'); } },
    { id: 'vault.new-note', title: 'New secure note', icon: 'note', keywords: 'passwords add', run: () => { ctx.router.navigate('/vault'); newItem('note'); } },
    ...TOOLS.map((t) => ({ id: `vault.${t.id}`, title: `Passwords: ${t.label}`, icon: t.icon === 'dice' ? 'zap' : t.icon, keywords: 'vault', run: () => ctx.router.navigate(`/vault/${t.id}`) })),
  ]));

  rebuild();
  route(ctx);

  return {
    update: (c) => route(c),
    destroy() {
      toolCleanup?.();
      detailCleanup?.();
      if (raf) cancelAnimationFrame(raf);
      cleanups.forEach((fn) => fn());
    },
  };
}
