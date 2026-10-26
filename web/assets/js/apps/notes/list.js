// Note list pane: header (view title, sort, selection, new note), date-grouped rows with
// search highlights, infinite scroll, keyboard navigation, multi-select and drag to folders.

import { h, cls } from '../../core/dom.js';
import { icon } from '../../core/icons.js';
import { plural } from '../../core/format.js';
import { displayTitle, groupNotes, listDate, highlightSegments, folderPath, listParams } from './model.js';
import { MIME_NOTES } from './sidebar.js';

function segmentsInto(parent, text, ranges) {
  parent.replaceChildren(...highlightSegments(text, ranges).map((s) => (s.hit ? h('mark', { class: 'nl-hit' }, s.text) : s.text)));
  return parent;
}

/**
 * @param {object} app controller (see index.js)
 */
export function createList(app) {
  const { ui } = app.ctx;
  const rows = new Map(); // id → {el, title, line, sub, check}
  let anchorId = null; // shift-click range anchor

  const titleEl = h('h2', { class: 'nl-title' });
  const countEl = h('div', { class: 'nl-count', aria: { live: 'polite' } });
  const backBtn = ui.button({ icon: 'chevron-left', title: 'Folders', variant: 'ghost', className: 'nl-back', onClick: () => app.showPane('folders') });
  const selectBtn = ui.button({ label: 'Select', variant: 'ghost', size: 'sm', className: 'nl-select-btn', onClick: () => app.setSelectMode(!app.state.selectMode) });
  const sortBtn = ui.menuButton({ icon: 'sort', title: 'Sort notes', size: 'sm', items: () => sortMenu() });
  const newBtn = ui.button({ icon: 'edit', title: 'New note (Ctrl+Alt+N)', variant: 'ghost', className: 'nl-new', onClick: () => app.newNote() });
  const header = h('header', { class: 'nl-header' },
    backBtn,
    h('div', { class: 'nl-heading' }, titleEl, countEl),
    h('div', { class: 'nl-actions' }, selectBtn, sortBtn, newBtn));

  const listEl = h('div', { class: 'nl-list', role: 'listbox', tabindex: '-1', aria: { label: 'Notes' } });
  const emptyEl = h('div', { class: 'nl-empty', hidden: true });
  const scroller = h('div', { class: 'nl-scroll' });
  const selCount = h('span', { class: 'nl-selbar-count' });
  const selActions = h('div', { class: 'nl-selbar-actions' });
  const selBar = h('div', { class: 'nl-selbar', hidden: true, role: 'toolbar', aria: { label: 'Selected notes' } },
    selCount, selActions);
  const el = h('section', { class: 'nl-pane', aria: { label: 'Note list' } }, header, scroller, selBar);

  const pager = ui.infiniteList({
    root: scroller,
    rootMargin: '800px',
    fetchPage: (cursor) => app.api.list(listParams(app.state.view, { q: app.state.q, sort: app.state.sort, order: app.state.order }), cursor, { limit: 60 }),
    onItems: (items, { reset }) => {
      app.listLoaded(items, reset);
      // The pager marks itself done only after onItems returns: refresh count/empty state then.
      queueMicrotask(() => render());
    },
  });
  scroller.append(listEl, emptyEl, pager.el);

  function sortMenu() {
    const s = app.state;
    const dateSort = s.sort !== 'title';
    return [
      { heading: 'Sort by' },
      { label: 'Date Edited', checked: s.sort === 'updated', onSelect: () => app.setSort('updated') },
      { label: 'Date Created', checked: s.sort === 'created', onSelect: () => app.setSort('created') },
      { label: 'Title', checked: s.sort === 'title', onSelect: () => app.setSort('title') },
      { separator: true },
      { label: dateSort ? 'Newest First' : 'A to Z', checked: s.order === (dateSort ? 'desc' : 'asc'), onSelect: () => app.setSort(s.sort, dateSort ? 'desc' : 'asc') },
      { label: dateSort ? 'Oldest First' : 'Z to A', checked: s.order === (dateSort ? 'asc' : 'desc'), onSelect: () => app.setSort(s.sort, dateSort ? 'asc' : 'desc') },
    ];
  }

  function viewTitle() {
    const s = app.state;
    if (s.q.trim()) return s.view.kind === 'trash' ? 'Search Trash' : 'Search';
    switch (s.view.kind) {
      case 'none': return 'Notes';
      case 'pinned': return 'Pinned';
      case 'trash': return 'Trash';
      case 'tag': return `#${s.view.tag}`;
      case 'folder': return s.tree.byId.get(s.view.id)?.name || 'Folder';
      default: return 'All Notes';
    }
  }

  function rowFor(n) {
    let r = rows.get(n.id);
    if (!r) {
      const check = h('span', { class: 'nl-check', aria: { hidden: 'true' } }, icon('check', 12));
      const title = h('div', { class: 'nl-row-title' });
      const line = h('div', { class: 'nl-row-line' });
      const sub = h('div', { class: 'nl-row-sub' });
      const rowEl = h('div', {
        class: 'nl-row', role: 'option', id: `nl-${n.id}`, tabindex: '-1', draggable: 'true', dataset: { id: n.id },
      }, check, h('div', { class: 'nl-row-main' }, title, line, sub));
      r = { el: rowEl, title, line, sub };
      rows.set(n.id, r);
    }
    fillRow(r, n);
    return r.el;
  }

  function fillRow(r, n) {
    const s = app.state;
    const searching = !!s.q.trim();
    const hl = searching ? n.highlights : null;
    const t = displayTitle(n);
    segmentsInto(r.title, t, hl && t === n.title ? hl.title : []);
    if (n.pinned && (searching || s.view.kind !== 'pinned')) r.title.prepend(icon('pin', { size: 12, className: 'nl-pin' }));
    const snippet = n.snippet || '';
    const snip = h('span', { class: cls('nl-snippet', { 'is-empty': !snippet }) });
    if (snippet) segmentsInto(snip, snippet, hl ? hl.snippet : []);
    else snip.textContent = 'No additional text';
    const when = s.view.kind === 'trash' && n.trashed_at ? n.trashed_at : (s.sort === 'created' ? n.created_at : n.updated_at);
    r.line.replaceChildren(h('span', { class: 'nl-date' }, listDate(when)), snip);
    const subs = [];
    const showFolder = searching || ['all', 'pinned', 'tag', 'trash'].includes(s.view.kind);
    if (showFolder && n.folder_id && s.tree.byId.has(n.folder_id)) {
      subs.push(h('span', { class: 'nl-folder' }, icon('folder', 12), folderPath(s.tree.byId, n.folder_id)));
    }
    if (n.has_code) subs.push(h('span', { class: 'nl-badge', dataset: { tip: 'Contains code' } }, icon('code', 12), 'Code'));
    for (const tag of (n.tags || []).slice(0, 3)) subs.push(h('span', { class: 'nl-tag' }, `#${tag}`));
    r.sub.replaceChildren(...subs);
    r.sub.hidden = subs.length === 0;
    const selected = s.selectMode ? s.selection.has(n.id) : n.id === s.noteId;
    r.el.classList.toggle('is-current', n.id === s.noteId);
    r.el.classList.toggle('is-selected', s.selection.has(n.id));
    r.el.setAttribute('aria-selected', String(selected));
    r.el.setAttribute('aria-label', `${t}. ${listDate(when)}. ${snippet}`.slice(0, 300));
  }

  function emptyContent() {
    const s = app.state;
    if (s.q.trim()) return ui.emptyState({ icon: 'search', title: 'No results', message: `Nothing matches “${s.q.trim()}”.`, compact: true });
    if (s.view.kind === 'trash') return ui.emptyState({ icon: 'trash', title: 'Trash is empty', message: 'Deleted notes stay here until you remove them for good.', compact: true });
    if (s.view.kind === 'pinned') return ui.emptyState({ icon: 'pin', title: 'No pinned notes', message: 'Pin a note to keep it at the top of every list.', compact: true });
    if (s.view.kind === 'tag') return ui.emptyState({ icon: 'hash', title: 'No notes with this tag', compact: true });
    return ui.emptyState({
      icon: 'note', title: 'No notes yet', compact: true,
      message: 'Capture ideas, checklists and code snippets.',
      action: h('div', { class: 'nl-empty-actions' },
        ui.button({ label: 'New note', icon: 'edit', variant: 'primary', size: 'sm', onClick: () => app.newNote() }),
        ui.button({ label: 'Import', icon: 'upload', size: 'sm', onClick: () => app.openImportExport('import') })),
    });
  }

  /** Re-render rows from app.state.items, reusing row elements. */
  function render() {
    const s = app.state;
    const searching = !!s.q.trim();
    titleEl.textContent = viewTitle();
    backBtn.hidden = false;
    countEl.textContent = pager.done ? plural(s.items.length, 'note') : (s.items.length ? `${s.items.length}+ notes` : '');
    selectBtn.querySelector('.btn-label').textContent = s.selectMode ? 'Done' : 'Select';
    selectBtn.classList.toggle('is-active', s.selectMode);
    sortBtn.hidden = searching;
    newBtn.hidden = s.view.kind === 'trash';
    el.classList.toggle('is-selecting', s.selectMode);
    listEl.setAttribute('aria-multiselectable', String(s.selectMode));

    const focusedId = document.activeElement?.closest?.('.nl-row')?.dataset.id || null;
    const nodes = [];
    for (const sec of groupNotes(s.items, { sort: s.sort, searching })) {
      nodes.push(h('div', { class: 'nl-section', role: 'presentation' }, sec.label));
      for (const n of sec.items) nodes.push(rowFor(n));
    }
    listEl.replaceChildren(...nodes);
    for (const id of [...rows.keys()]) if (!s.items.some((n) => n.id === id)) rows.delete(id);
    if (focusedId && rows.has(focusedId)) rows.get(focusedId).el.focus({ preventScroll: true });
    updateTabStop();

    const empty = s.items.length === 0 && pager.done;
    emptyEl.hidden = !empty;
    if (empty) emptyEl.replaceChildren(emptyContent());
    renderSelBar();
  }

  function updateTabStop() {
    const s = app.state;
    const target = rows.get(s.noteId) || rows.get(s.items[0]?.id);
    for (const r of rows.values()) r.el.tabIndex = r === target ? 0 : -1;
  }

  function renderSelBar() {
    const s = app.state;
    const n = s.selection.size;
    selBar.hidden = !s.selectMode;
    selCount.textContent = n ? `${n} selected` : 'Select notes';
    const ids = [...s.selection];
    const dis = n === 0;
    if (s.view.kind === 'trash') {
      selActions.replaceChildren(
        ui.button({ label: 'Restore', icon: 'restore', size: 'sm', disabled: dis, onClick: () => app.restoreNotes(ids) }),
        ui.button({ label: 'Delete', icon: 'trash', size: 'sm', variant: 'danger-ghost', disabled: dis, onClick: () => app.destroyNotes(ids) }));
    } else {
      selActions.replaceChildren(
        ui.button({ label: 'Move', icon: 'folder', size: 'sm', disabled: dis, onClick: () => app.pickFolderAndMove(ids) }),
        ui.button({ label: 'Delete', icon: 'trash', size: 'sm', variant: 'danger-ghost', disabled: dis, onClick: () => app.trashNotes(ids) }));
    }
  }

  /** Update a single row in place (title/snippet while typing). */
  function refreshRow(n) {
    const r = rows.get(n.id);
    if (r) fillRow(r, n);
  }

  function idsInOrder() {
    return app.state.items.map((n) => n.id);
  }

  function moveFocus(delta, { open = true } = {}) {
    const ids = idsInOrder();
    if (!ids.length) return;
    const cur = document.activeElement?.closest?.('.nl-row')?.dataset.id || app.state.noteId;
    let i = ids.indexOf(cur);
    i = i < 0 ? 0 : Math.max(0, Math.min(ids.length - 1, i + delta));
    focusRow(ids[i]);
    if (open && !app.state.selectMode) app.openNote(ids[i], { replace: true });
    if (i >= ids.length - 3) pager.loadMore();
  }

  function focusRow(id) {
    const r = rows.get(id);
    if (!r) return;
    for (const x of rows.values()) x.el.tabIndex = -1;
    r.el.tabIndex = 0;
    r.el.focus({ preventScroll: true });
    r.el.scrollIntoView({ block: 'nearest' });
  }

  function toggleSelect(id, { range = false } = {}) {
    const s = app.state;
    if (range && anchorId) {
      const ids = idsInOrder();
      const a = ids.indexOf(anchorId);
      const b = ids.indexOf(id);
      if (a >= 0 && b >= 0) {
        for (let i = Math.min(a, b); i <= Math.max(a, b); i++) s.selection.add(ids[i]);
      }
    } else {
      if (s.selection.has(id)) s.selection.delete(id);
      else s.selection.add(id);
      anchorId = id;
    }
    if (!s.selectMode) app.setSelectMode(true, { keep: true });
    else render();
  }

  function rowMenu(id) {
    const n = app.state.items.find((x) => x.id === id);
    if (!n) return [];
    const ids = app.state.selection.has(id) && app.state.selection.size > 1 ? [...app.state.selection] : [id];
    if (app.state.view.kind === 'trash') {
      return [
        { label: ids.length > 1 ? `Restore ${ids.length} notes` : 'Restore', icon: 'restore', onSelect: () => app.restoreNotes(ids) },
        { label: ids.length > 1 ? `Delete ${ids.length} notes…` : 'Delete permanently…', icon: 'trash', danger: true, onSelect: () => app.destroyNotes(ids) },
      ];
    }
    return [
      { label: 'Open', icon: 'note', onSelect: () => app.openNote(id, { focusEditor: true }) },
      ids.length === 1 ? { label: n.pinned ? 'Unpin' : 'Pin', icon: 'pin', shortcut: 'Ctrl+Shift+P', onSelect: () => app.setPinned([id], !n.pinned) } : null,
      { label: 'Move to…', icon: 'folder', onSelect: () => app.pickFolderAndMove(ids) },
      ids.length === 1 ? { label: 'Export as Markdown', icon: 'download', onSelect: () => app.api.exportNote(id) } : null,
      { separator: true },
      { label: ids.length > 1 ? `Delete ${ids.length} notes` : 'Delete', icon: 'trash', danger: true, shortcut: 'Del', onSelect: () => app.trashNotes(ids) },
    ];
  }

  listEl.addEventListener('click', (e) => {
    const row = e.target.closest('.nl-row');
    if (!row) return;
    const id = row.dataset.id;
    if (app.state.selectMode || e.metaKey || e.ctrlKey || e.shiftKey) {
      e.preventDefault();
      toggleSelect(id, { range: e.shiftKey });
      return;
    }
    anchorId = id;
    app.openNote(id, { push: true });
  });
  listEl.addEventListener('dblclick', (e) => {
    if (e.target.closest('.nl-row') && !app.state.selectMode) app.focusEditor();
  });
  listEl.addEventListener('contextmenu', (e) => {
    const row = e.target.closest('.nl-row');
    if (row) ui.contextMenu(e, rowMenu(row.dataset.id));
  });
  listEl.addEventListener('keydown', (e) => {
    const row = e.target.closest('.nl-row');
    const id = row?.dataset.id;
    const mod = e.ctrlKey || e.metaKey;
    switch (e.key) {
      case 'ArrowDown':
      case 'ArrowUp': {
        e.preventDefault();
        const delta = e.key === 'ArrowDown' ? 1 : -1;
        if (e.shiftKey && id) {
          const ids = idsInOrder();
          const next = ids[Math.max(0, Math.min(ids.length - 1, ids.indexOf(id) + delta))];
          if (!anchorId || !app.state.selection.size) {
            anchorId = id;
            app.state.selection.add(id);
          }
          focusRow(next);
          toggleSelect(next, { range: true });
        } else moveFocus(delta);
        break;
      }
      case 'Home': e.preventDefault(); moveFocus(-1e9); break;
      case 'End': e.preventDefault(); moveFocus(1e9); break;
      case 'PageDown': e.preventDefault(); moveFocus(10); break;
      case 'PageUp': e.preventDefault(); moveFocus(-10); break;
      case 'Enter':
        if (!id) break;
        e.preventDefault();
        if (app.state.selectMode) toggleSelect(id);
        else app.openNote(id, { focusEditor: true });
        break;
      case ' ':
        if (id && app.state.selectMode) { e.preventDefault(); toggleSelect(id); }
        break;
      case 'Escape':
        if (app.state.selectMode) { e.preventDefault(); app.setSelectMode(false); }
        break;
      case 'Delete':
      case 'Backspace': {
        if (!id || (e.key === 'Backspace' && !mod)) break;
        e.preventDefault();
        const ids = app.state.selection.size ? [...app.state.selection] : [id];
        if (app.state.view.kind === 'trash') app.destroyNotes(ids);
        else app.trashNotes(ids);
        break;
      }
      case 'a':
        if (mod && app.state.selectMode) {
          e.preventDefault();
          for (const x of idsInOrder()) app.state.selection.add(x);
          render();
        }
        break;
      case 'ContextMenu':
      case 'F10':
        if (id && (e.key === 'ContextMenu' || e.shiftKey)) {
          e.preventDefault();
          const r = row.getBoundingClientRect();
          ui.openMenu({ items: rowMenu(id), x: r.left + 24, y: r.top + r.height / 2 });
        }
        break;
      default: break;
    }
  });
  listEl.addEventListener('focusin', (e) => {
    if (e.target === listEl) {
      const id = app.state.noteId && rows.has(app.state.noteId) ? app.state.noteId : app.state.items[0]?.id;
      if (id) focusRow(id);
    }
  });
  listEl.addEventListener('dragstart', (e) => {
    const row = e.target.closest('.nl-row');
    if (!row) return;
    const id = row.dataset.id;
    const ids = app.state.selection.has(id) ? [...app.state.selection] : [id];
    e.dataTransfer.setData(MIME_NOTES, JSON.stringify(ids));
    e.dataTransfer.setData('text/plain', ids.length > 1 ? `${ids.length} notes` : displayTitle(app.state.items.find((n) => n.id === id)));
    e.dataTransfer.effectAllowed = 'move';
    row.classList.add('is-dragging');
  });
  listEl.addEventListener('dragend', (e) => e.target.closest?.('.nl-row')?.classList.remove('is-dragging'));

  return {
    el,
    render,
    refreshRow,
    focusRow,
    focus() {
      const id = app.state.noteId && rows.has(app.state.noteId) ? app.state.noteId : app.state.items[0]?.id;
      if (id) focusRow(id);
      else listEl.focus();
    },
    reload() {
      app.state.items = [];
      rows.clear();
      listEl.replaceChildren();
      emptyEl.hidden = true;
      scroller.scrollTop = 0;
      return pager.reload();
    },
    loadMore: () => pager.loadMore(),
    get done() { return pager.done; },
    destroy() { pager.destroy(); },
  };
}
