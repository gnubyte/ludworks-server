// Notes app: an Apple-Notes-style three-pane editor (folders | note list | editor) that
// collapses to stacked navigation on narrow screens. State that matters for navigation
// (view, open note) lives in the URL: /notes?f=<folder>|none, ?tag=, ?v=pinned|trash, &n=<note>.

import { h, on } from '../../core/dom.js';
import { ensureStyle } from '../../core/style.js';
import { isApple } from '../../core/shortcuts.js';
import { notesApi, eachLimited } from './api.js';
import {
  buildFolderTree, flattenTree, viewFromQuery, viewToQuery, sameView, compareNotes, upsertSorted, removeIds,
  neighbourAfterRemoval, defaultOrder, prefKey, readPref, writePref, displayTitle, folderPath, isWithin,
} from './model.js';
import { createSidebar } from './sidebar.js';
import { createList } from './list.js';
import { createEditorPane } from './editor.js';
import { importExportDialog, pickFolderDialog } from './dialogs.js';

const ID_RE = /^[A-Za-z0-9_-]{8,64}$/;
const SORTS = ['updated', 'created', 'title'];

export async function mount(container, ctx) {
  const styleReady = ensureStyle('/assets/css/apps/notes.css');
  ctx.setLayout({ full: true });
  ctx.setTitle('Notes');
  const { ui } = ctx;
  const storage = globalThis.localStorage;
  const uid = ctx.user?.id;
  const sort = readPref(storage, prefKey(uid, 'sort'), SORTS, 'updated');

  const app = {
    ctx,
    api: notesApi(ctx.api),
    isApple: isApple(),
    state: {
      view: { kind: 'all' },
      noteId: null,
      q: '',
      sort,
      order: readPref(storage, prefKey(uid, 'order'), ['asc', 'desc'], defaultOrder(sort)),
      items: [],
      folders: [],
      tree: buildFolderTree([]),
      tags: [],
      collapsed: new Set(),
      selection: new Set(),
      selectMode: false,
      pane: 'list',
    },
  };
  const S = app.state;
  try {
    const raw = JSON.parse(storage?.getItem(prefKey(uid, 'collapsed')) || '[]');
    if (Array.isArray(raw)) for (const id of raw) if (typeof id === 'string' && ID_RE.test(id)) S.collapsed.add(id);
  } catch { /* ignore */ }

  const sidebar = createSidebar(app);
  const list = createList(app);
  const editor = createEditorPane(app);
  const root = h('div', { class: 'notes-app', dataset: { pane: 'list' } }, sidebar.el, list.el, editor.el);
  await styleReady;
  container.append(root);

  const narrow = () => window.matchMedia('(max-width: 759px)').matches;

  /* ------------------------------------------------------------- helpers */
  const belongs = (n) => {
    const v = S.view;
    if (v.kind === 'trash') return !!n.trashed_at;
    if (n.trashed_at) return false;
    switch (v.kind) {
      case 'none': return !n.folder_id;
      case 'folder': return n.folder_id === v.id;
      case 'pinned': return !!n.pinned;
      case 'tag': return (n.tags || []).includes(v.tag);
      default: return true;
    }
  };
  const cmp = () => compareNotes(S.sort, S.order);
  const navigate = (patch, { push = false } = {}) => ctx.router.setQuery(patch, { replace: !push });

  let metaTimer = null;
  app.refreshMeta = () => {
    clearTimeout(metaTimer);
    metaTimer = setTimeout(() => {
      app.refreshFolders();
      app.refreshTags();
    }, 300);
  };
  app.refreshFolders = async () => {
    try {
      S.folders = await app.api.folders(ctx.signal);
      S.tree = buildFolderTree(S.folders);
      if (S.view.kind === 'folder' && !S.tree.byId.has(S.view.id)) app.selectView({ kind: 'all' });
      sidebar.render();
      list.render();
    } catch (err) {
      ui.toast.error(err);
    }
  };
  app.refreshTags = async () => {
    try {
      S.tags = await app.api.tags(ctx.signal);
      sidebar.render();
      editor.renderTags();
    } catch (err) {
      ui.toast.error(err);
    }
  };

  /* ------------------------------------------------------------- panes */
  app.showPane = (p) => {
    S.pane = p;
    root.dataset.pane = p;
    if (p === 'folders') requestAnimationFrame(() => (sidebar.el.querySelector('.nf-item.is-active') || sidebar.el.querySelector('.nf-item'))?.focus());
    else if (p === 'list' && narrow()) requestAnimationFrame(() => list.focus());
  };
  sidebar.el.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && root.dataset.pane === 'folders' && !narrow()) {
      e.preventDefault();
      app.showPane('list');
      list.focus();
    }
  });
  app.focusEditor = () => {
    app.showPane('editor');
    editor.focus();
  };

  /* ------------------------------------------------------------- views & notes */
  app.selectView = (view) => {
    const wasSearching = !!S.q;
    if (wasSearching) setSearch('', { reload: false, resetBox: true });
    if (sameView(view, S.view)) {
      if (wasSearching) list.reload();
      app.showPane('list');
      return;
    }
    navigate({ ...viewToQuery(view), n: null, new: null }, { push: true });
  };

  app.openNote = (id, { push = false, replace = false, focusEditor = false } = {}) => {
    if (!ID_RE.test(id)) return;
    if (id === S.noteId) {
      app.showPane('editor');
      if (focusEditor) editor.focus();
      else editor.open(id);
      return;
    }
    pendingFocus = focusEditor;
    navigate({ n: id }, { push: push && !replace });
  };
  let pendingFocus = false;

  app.listLoaded = (items, reset) => {
    if (reset) S.items = items.slice();
    else {
      const seen = new Set(S.items.map((n) => n.id));
      for (const n of items) if (!seen.has(n.id)) S.items.push(n);
    }
    list.render();
    if (reset && !S.noteId && !narrow() && S.items.length && !S.q) {
      app.openNote(S.items[0].id, { replace: true });
    }
  };

  app.previewNote = (summary) => {
    const i = S.items.findIndex((n) => n.id === summary.id);
    if (i < 0) return;
    S.items[i] = { ...S.items[i], title: summary.title, snippet: summary.snippet };
    list.refreshRow(S.items[i]);
  };

  app.noteSaved = (summary) => {
    const i = S.items.findIndex((n) => n.id === summary.id);
    const prev = i >= 0 ? S.items[i] : null;
    const merged = { ...(prev || {}), ...summary };
    delete merged.body;
    if (S.q) {
      if (prev) S.items[i] = { ...merged, highlights: prev.highlights };
    } else if (belongs(merged)) {
      S.items = upsertSorted(S.items, merged, cmp(), { complete: list.done });
    } else {
      S.items = removeIds(S.items, [merged.id]);
    }
    list.render();
    if (!prev || prev.folder_id !== merged.folder_id || prev.pinned !== merged.pinned || prev.trashed_at !== merged.trashed_at
      || (prev.tags || []).join('\n') !== (merged.tags || []).join('\n')) app.refreshMeta();
  };

  app.noteCreated = (n) => {
    const summary = { ...n };
    delete summary.body;
    if (!S.q && belongs(summary)) S.items = upsertSorted(S.items, summary, cmp(), { complete: true });
    list.render();
    app.refreshMeta();
  };

  app.noteMissing = (id, replacement = null) => {
    S.items = removeIds(S.items, [id]);
    if (S.noteId === id) S.noteId = null;
    list.render();
    navigate({ n: replacement });
    if (!replacement) editor.clear();
    app.refreshMeta();
  };

  app.discardEmpty = (id) => {
    S.items = removeIds(S.items, [id]);
    list.render();
    app.api.destroy(id).then(() => app.refreshMeta()).catch(() => {});
  };

  app.newNote = async ({ folderId } = {}) => {
    if (newBusy) return;
    newBusy = true;
    try {
      if (S.view.kind === 'trash') {
        await ctx.router.setQuery({ v: null, n: null }, { replace: false });
      }
      if (S.q) setSearch('', { reload: true, resetBox: true });
      const okLeave = await editor.close();
      if (!okLeave && !(await ui.confirm({ title: 'Unsaved changes', message: 'The current note could not be saved yet. Continue anyway?', confirmLabel: 'Continue', danger: true }))) return;
      const v = S.view;
      const body = {
        body: '',
        folder_id: folderId !== undefined ? folderId : (v.kind === 'folder' ? v.id : null),
        tags: v.kind === 'tag' ? [v.tag] : [],
        pinned: v.kind === 'pinned',
      };
      const n = await app.api.create(body);
      app.noteCreated(n);
      S.noteId = n.id;
      editor.openCreated(n, { focus: true });
      list.render();
      list.focusRow(n.id);
      app.showPane('editor');
      editor.focus();
      await navigate({ n: n.id, new: null }, { push: true });
      editor.focus();
    } catch (err) {
      ui.toast.error(err);
    } finally {
      newBusy = false;
    }
  };
  let newBusy = false;

  async function afterRemoval(ids, { toast }) {
    const next = ids.includes(S.noteId) ? neighbourAfterRemoval(S.items, ids, S.noteId) : undefined;
    S.items = removeIds(S.items, ids);
    for (const id of ids) S.selection.delete(id);
    if (!S.selection.size) S.selectMode = false;
    list.render();
    if (next !== undefined) {
      S.noteId = null;
      await editor.clear();
      if (next && !narrow()) app.openNote(next, { replace: true });
      else {
        navigate({ n: null });
        app.showPane('list');
      }
    }
    if (toast) toast();
    app.refreshMeta();
  }

  function reportFailures(failed, verb) {
    if (!failed.length) return;
    ui.toast.error(`${failed.length === 1 ? 'A note' : `${failed.length} notes`} could not be ${verb}.`, { description: ui.errorMessage(failed[0].error) });
  }

  app.trashNotes = async (ids) => {
    if (!ids.length) return;
    if (ids.includes(S.noteId)) await editor.flush();
    const { ok, failed } = await eachLimited(ids, 6, async (id) => { await app.api.trash(id); return id; });
    reportFailures(failed, 'deleted');
    await afterRemoval(ok, {
      toast: () => ok.length && ui.toast(ok.length === 1 ? 'Note moved to Trash' : `${ok.length} notes moved to Trash`, {
        type: 'success', action: { label: 'Undo', onClick: () => app.restoreNotes(ok, { quiet: true }) },
      }),
    });
  };

  app.restoreNotes = async (ids, { quiet = false } = {}) => {
    if (!ids.length) return;
    const { ok, failed } = await eachLimited(ids, 6, (id) => app.api.restore(id));
    reportFailures(failed, 'restored');
    if (S.view.kind === 'trash') {
      await afterRemoval(ok.map((n) => n.id), {});
    } else {
      for (const n of ok) app.noteCreated(n);
    }
    if (!quiet && ok.length) {
      const first = ok[0];
      ui.toast(ok.length === 1 ? `Restored “${displayTitle(first)}”` : `${ok.length} notes restored`, {
        type: 'success',
        action: ok.length === 1 ? { label: 'Open', onClick: () => navigate({ v: null, tag: null, f: first.folder_id || 'none', n: first.id }, { push: true }) } : null,
      });
    }
  };

  app.destroyNotes = async (ids) => {
    if (!ids.length) return;
    const one = ids.length === 1 ? S.items.find((n) => n.id === ids[0]) : null;
    const ok = await ui.confirm({
      title: one ? `Delete “${displayTitle(one)}” permanently?` : `Delete ${ids.length} notes permanently?`,
      message: `This can’t be undone. Images and attachments in ${ids.length === 1 ? 'this note are' : 'these notes are'} deleted too.`,
      confirmLabel: 'Delete permanently', danger: true,
    });
    if (!ok) return;
    const res = await eachLimited(ids, 6, async (id) => { await app.api.destroy(id); return id; });
    reportFailures(res.failed, 'deleted');
    await afterRemoval(res.ok, { toast: () => res.ok.length && ui.toast.success(res.ok.length === 1 ? 'Note deleted' : `${res.ok.length} notes deleted`) });
  };

  async function patchNotes(ids, fields, verb) {
    const open = ids.includes(S.noteId) && editor.note && !editor.note.trashed_at;
    const others = ids.filter((id) => !(open && id === S.noteId));
    const res = await eachLimited(others, 6, (id) => app.api.update(id, fields));
    if (open) {
      try {
        await editor.updateOpen(fields);
      } catch (err) {
        res.failed.push({ item: S.noteId, error: err });
      }
    }
    reportFailures(res.failed, verb);
    for (const n of res.ok) app.noteSaved(n);
    return res.ok.length + (open ? 1 : 0);
  }

  app.moveNotes = async (ids, folderId) => {
    const n = await patchNotes(ids, { folder_id: folderId }, 'moved');
    if (n) {
      const name = folderId ? folderPath(S.tree.byId, folderId) : 'Notes';
      ui.toast.success(n === 1 ? `Moved to ${name}` : `${n} notes moved to ${name}`, { timeout: 2500 });
    }
    S.selection.clear();
    S.selectMode = false;
    list.render();
    app.refreshMeta();
  };

  app.setPinned = (ids, pinned) => patchNotes(ids, { pinned }, pinned ? 'pinned' : 'unpinned');

  app.pickFolderAndMove = async (ids) => {
    if (!ids.length) return;
    const current = ids.length === 1 ? S.items.find((n) => n.id === ids[0])?.folder_id ?? null : undefined;
    const fid = await pickFolderDialog(app, { title: ids.length === 1 ? 'Move note to…' : `Move ${ids.length} notes to…`, current });
    if (fid !== undefined) app.moveNotes(ids, fid);
  };

  app.folderMenuItems = (currentId, onPick) => [
    { heading: 'Move to' },
    { label: 'Notes', checked: !currentId, onSelect: () => onPick(null) },
    ...flattenTree(S.tree.roots).map((f) => ({
      label: `${' '.repeat(f.depth)}${f.name}`, checked: f.id === currentId, onSelect: () => onPick(f.id),
    })),
  ];

  /* ------------------------------------------------------------- folders */
  app.newFolder = async (parentId, { select = true } = {}) => {
    const name = await ui.prompt({
      title: parentId ? `New folder in “${S.tree.byId.get(parentId)?.name || 'folder'}”` : 'New folder',
      label: 'Name', confirmLabel: 'Create', placeholder: 'Folder name',
      validate: (v) => (!v.trim() ? 'Enter a name.' : [...v.trim()].length > 200 ? 'Use at most 200 characters.' : null),
    });
    if (name === null) return null;
    try {
      const f = await app.api.createFolder(name.trim(), parentId);
      if (parentId) S.collapsed.delete(parentId);
      await app.refreshFolders();
      if (select) app.selectView({ kind: 'folder', id: f.id });
      return f;
    } catch (err) {
      ui.toast.error(err);
      return null;
    }
  };

  app.renameFolder = async (id) => {
    const f = S.tree.byId.get(id);
    if (!f) return;
    const name = await ui.prompt({
      title: 'Rename folder', label: 'Name', value: f.name, confirmLabel: 'Rename',
      validate: (v) => (!v.trim() ? 'Enter a name.' : null),
    });
    if (name === null || name.trim() === f.name) return;
    try {
      await app.api.updateFolder(id, { name: name.trim() });
      await app.refreshFolders();
    } catch (err) {
      ui.toast.error(err);
    }
  };

  app.deleteFolder = async (id) => {
    const f = S.tree.byId.get(id);
    if (!f) return;
    const subs = flattenTree(f.children).length;
    const ok = await ui.confirm({
      title: `Delete “${f.name}”?`,
      message: `${f.total ? `${f.total === 1 ? 'Its note' : `All ${f.total} notes`} will be moved to the Trash.` : 'The folder is empty.'}${subs ? ` ${subs === 1 ? 'Its subfolder is' : `Its ${subs} subfolders are`} deleted too.` : ''}`,
      confirmLabel: 'Delete folder', danger: true,
    });
    if (!ok) return;
    try {
      const inside = S.noteId && editor.note?.folder_id && isWithin(S.tree.byId, editor.note.folder_id, id);
      if (inside) await editor.flush();
      await app.api.deleteFolder(id);
      ui.toast.success(`Deleted “${f.name}”`);
      if (S.view.kind === 'folder' && isWithin(S.tree.byId, S.view.id, id)) {
        app.selectView({ kind: 'all' });
      } else {
        if (inside) {
          S.noteId = null;
          await editor.clear();
          navigate({ n: null });
        }
        list.reload();
      }
      await app.refreshFolders();
      app.refreshTags();
    } catch (err) {
      ui.toast.error(err);
    }
  };

  app.moveFolder = async (id, parentId) => {
    const f = S.tree.byId.get(id);
    if (!f || (f.parent_id || null) === (parentId || null)) return;
    try {
      await app.api.updateFolder(id, { parent_id: parentId });
      if (parentId) S.collapsed.delete(parentId);
      await app.refreshFolders();
    } catch (err) {
      ui.toast.error(err);
    }
  };

  app.toggleCollapsed = (id) => {
    if (S.collapsed.has(id)) S.collapsed.delete(id);
    else S.collapsed.add(id);
    writePref(storage, prefKey(uid, 'collapsed'), JSON.stringify([...S.collapsed].slice(0, 2000)));
    sidebar.render();
  };

  app.exportFolder = (id) => app.api.exportZip(id);
  app.openImportExport = (tab = 'import', files) => importExportDialog(app, { tab, files });
  app.afterImport = () => {
    app.refreshMeta();
    list.reload();
  };

  /* ------------------------------------------------------------- list options */
  app.setSort = (s, order) => {
    if (!SORTS.includes(s)) return;
    S.sort = s;
    S.order = order || defaultOrder(s);
    writePref(storage, prefKey(uid, 'sort'), S.sort);
    writePref(storage, prefKey(uid, 'order'), S.order);
    list.reload();
  };

  app.setSelectMode = (on, { keep = false } = {}) => {
    S.selectMode = !!on;
    if (!on || !keep) {
      if (!on) S.selection.clear();
    }
    list.render();
    if (on && !keep) list.focus();
  };

  /* ------------------------------------------------------------- search */
  function setSearch(q, { reload = true, resetBox = false } = {}) {
    if (q === S.q && !resetBox) return;
    S.q = q;
    S.selection.clear();
    S.selectMode = false;
    if (resetBox) ctx.setSearch(searchCfg());
    if (reload) list.reload();
  }
  const searchCfg = () => ({
    placeholder: 'Search notes',
    value: S.q,
    onInput: (q) => setSearch(q),
    onSubmit: (q) => {
      setSearch(q);
      list.focus();
    },
  });
  ctx.setSearch(searchCfg());

  function focusSearch() {
    const box = document.querySelector('.topbar-search');
    const input = document.querySelector('.topbar-search-input');
    if (box && getComputedStyle(box).display === 'none') document.querySelector('.topbar-search-btn')?.click();
    else input?.focus();
    input?.select();
  }

  /* ------------------------------------------------------------- routing */
  async function applyRoute(c) {
    const q = c.query || {};
    if (q.new === '1') {
      // /notes?new=1 (command palette, Home quick action). During the first mount the shell
      // is still finishing this navigation, so start the new note right after it.
      const view = viewFromQuery(q);
      if (!sameView(view, S.view)) {
        S.view = view;
        sidebar.render();
        if (loaded) list.reload();
      }
      if (loaded) app.newNote();
      else setTimeout(() => app.newNote(), 0);
      armGuard();
      return;
    }
    const view = viewFromQuery(q);
    const noteId = q.n && ID_RE.test(q.n) ? q.n : null;
    const viewChanged = !sameView(view, S.view) || !loaded;
    S.view = view;
    if (viewChanged) {
      S.selection.clear();
      S.selectMode = false;
      sidebar.render();
      if (loaded) list.reload();
    }
    if (noteId !== S.noteId) {
      S.noteId = noteId;
      list.render();
      if (noteId) {
        const focus = pendingFocus;
        pendingFocus = false;
        app.showPane('editor');
        await editor.open(noteId, { focus });
      } else {
        await editor.clear();
        if (viewChanged || S.pane === 'editor') app.showPane('list');
      }
    } else if (viewChanged) {
      if (!noteId) editor.renderEmpty();
      app.showPane('list');
    }
    armGuard();
  }

  const guard = async () => {
    const ok = await editor.flush();
    if (ok) return true;
    return ui.confirm({
      title: 'Unsaved changes',
      message: 'Your latest edits could not be saved yet. Leave anyway and lose them?',
      confirmLabel: 'Leave without saving', danger: true,
    });
  };
  function armGuard() {
    ctx.router.setLeaveGuard(guard);
  }

  /* ------------------------------------------------------------- shortcuts */
  const noOverlay = () => !document.documentElement.classList.contains('modal-open') && !document.querySelector('.palette-backdrop');
  ctx.onCleanup(on(document, 'keydown', (e) => {
    const mod = app.isApple ? e.metaKey : e.ctrlKey;
    const altGraph = e.getModifierState?.('AltGraph');
    if (mod && e.altKey && !e.shiftKey && !altGraph && e.code === 'KeyN' && noOverlay()) {
      e.preventDefault();
      app.newNote();
    }
  }));
  ctx.shortcuts.register('mod+alt+n', () => {}, { description: 'New note', when: () => false });
  ctx.shortcuts.register('mod+f', () => focusSearch(), { description: 'Search notes', allowInInputs: true, when: noOverlay });
  ctx.shortcuts.register('mod+shift+p', () => editor.togglePin(), { description: 'Pin or unpin the note', allowInInputs: true, when: noOverlay });
  ctx.shortcuts.register('mod+shift+m', () => editor.toggleMode(), { description: 'Toggle rich / Markdown editor', allowInInputs: true, when: noOverlay });
  ctx.shortcuts.register('delete', () => {}, { description: 'Move selected notes to Trash (in the list)', when: () => false });
  editor.el.addEventListener('keydown', (e) => {
    if (e.key === 'Escape' && !e.defaultPrevented && !editor.el.querySelector('.re-pop:not([hidden])') && !e.target.closest('.ne-tags')) {
      e.preventDefault();
      if (narrow()) app.showPane('list');
      list.focus();
    }
  });

  ctx.commands.register(() => [
    { id: 'notes:new', title: 'New note', icon: 'edit', group: 'Notes', shortcut: app.isApple ? '⌘⌥N' : 'Ctrl+Alt+N', keywords: ['create', 'write'], run: () => app.newNote() },
    { id: 'notes:new-folder', title: 'New notes folder', icon: 'folder', group: 'Notes', keywords: ['create'], run: () => app.newFolder(null) },
    { id: 'notes:search', title: 'Search notes', icon: 'search', group: 'Notes', keywords: ['find'], run: () => focusSearch() },
    { id: 'notes:import', title: 'Import notes…', icon: 'upload', group: 'Notes', keywords: ['markdown', 'zip', 'obsidian'], run: () => app.openImportExport('import') },
    { id: 'notes:export', title: 'Export notes as ZIP…', icon: 'download', group: 'Notes', keywords: ['backup', 'markdown'], run: () => app.openImportExport('export') },
    { id: 'notes:pin', title: 'Pin or unpin note', icon: 'pin', group: 'Notes', run: () => editor.togglePin() },
    { id: 'notes:mode', title: editor.mode === 'rich' ? 'Switch to Markdown editor' : 'Switch to rich editor', icon: 'code', group: 'Notes', keywords: ['source', 'wysiwyg'], run: () => editor.toggleMode() },
    { id: 'notes:all', title: 'All notes', icon: 'layers', group: 'Notes', run: () => app.selectView({ kind: 'all' }) },
    { id: 'notes:pinned', title: 'Pinned notes', icon: 'pin', group: 'Notes', run: () => app.selectView({ kind: 'pinned' }) },
    { id: 'notes:trash', title: 'Notes trash', icon: 'trash', group: 'Notes', keywords: ['deleted', 'recover'], run: () => app.selectView({ kind: 'trash' }) },
    ...flattenTree(S.tree.roots).slice(0, 200).map((f) => ({
      id: `notes:folder:${f.id}`, title: f.name, subtitle: `Notes folder${f.parent_id ? ` · ${folderPath(S.tree.byId, f.parent_id)}` : ''}`,
      icon: 'folder', group: 'Notes folders', run: () => app.selectView({ kind: 'folder', id: f.id }),
    })),
  ]);

  /* ------------------------------------------------------------- page lifecycle */
  ctx.onCleanup(on(window, 'beforeunload', (e) => {
    if (editor.dirty) {
      editor.flush();
      e.preventDefault();
      e.returnValue = '';
    }
  }));
  let lastRefresh = Date.now();
  ctx.onCleanup(on(document, 'visibilitychange', () => {
    if (document.visibilityState === 'hidden') {
      editor.flush();
    } else if (Date.now() - lastRefresh > 5000) {
      lastRefresh = Date.now();
      editor.refresh();
      app.refreshMeta();
    }
  }));
  // Files dropped anywhere on the app open the import dialog (images dropped on the editor
  // are handled by the editor itself).
  // Medium widths show the folders pane as an overlay; clicking elsewhere closes it.
  root.addEventListener('click', (e) => {
    if (root.dataset.pane === 'folders' && !narrow() && !e.target.closest('.nf-pane, .nl-back')) app.showPane('list');
  });
  root.addEventListener('dragover', (e) => {
    if (Array.from(e.dataTransfer?.types || []).includes('Files') && !e.target.closest('.ne-host')) e.preventDefault();
  });
  root.addEventListener('drop', (e) => {
    const files = Array.from(e.dataTransfer?.files || []);
    if (!files.length || e.target.closest('.ne-host') || e.defaultPrevented) return;
    e.preventDefault();
    app.openImportExport('import', files);
  });

  /* ------------------------------------------------------------- start */
  let loaded = false;
  S.view = viewFromQuery(ctx.query);
  await editor.clear();
  sidebar.render();
  await applyRoute(ctx);
  loaded = true;
  app.refreshFolders();
  app.refreshTags();
  list.reload();
  if (!S.noteId) app.showPane('list');

  return {
    update: (c) => applyRoute(c).catch((err) => ui.toast.error(err)),
    unmount: () => {
      clearTimeout(metaTimer);
      editor.flush();
      editor.destroy();
      list.destroy();
    },
  };
}
