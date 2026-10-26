// Editor pane: rich (WYSIWYG) or Markdown source editor for the open note, autosave with
// optimistic concurrency and a conflict dialog, pin/tags/move/info/export/delete, and a
// read-only view for notes in the Trash.

import { h, cls, mount as mountEl } from '../../core/dom.js';
import { icon } from '../../core/icons.js';
import { formatDateTime, plural } from '../../core/format.js';
import { createRichEditor } from '../../lib/richeditor.js';
import { createMarkdownEditor } from '../../lib/mdeditor.js';
import { renderInto, toPlainText } from '../../lib/markdown.js';
import { MAX_ATTACHMENT } from './api.js';
import {
  createSaver, countWords, displayTitle, listDate, prefKey, readPref, writePref, folderPath,
  hasExplicitTitle, previewSummary, blankMarkdown,
} from './model.js';

const MODES = ['rich', 'markdown'];
const MD_VIEWS = ['source', 'split', 'preview'];


/**
 * @param {object} app controller (see index.js)
 */
export function createEditorPane(app) {
  const { ui } = app.ctx;
  const userId = app.ctx.user?.id;
  const storage = globalThis.localStorage;
  let mode = readPref(storage, prefKey(userId, 'editor'), MODES, 'rich');
  // Side-by-side preview only when there is room for it (the choice is remembered).
  let mdView = readPref(storage, prefKey(userId, 'mdview'), MD_VIEWS, (globalThis.innerWidth || 0) >= 1280 ? 'split' : 'source');

  let note = null; // full server note (with body)
  let editor = null;
  let saver = null;
  let loadToken = 0;
  let previewTimer = null;
  let tagEditing = false;
  let explicitTitle = false; // the open note has an explicit (not derived) title
  const createdHere = new Set();

  /* -------------------------------------------------------------- header */
  const backBtn = ui.button({ icon: 'chevron-left', label: 'Notes', variant: 'ghost', size: 'sm', className: 'ne-back', onClick: () => app.showPane('list') });
  const statusEl = h('div', { class: 'ne-status', role: 'status', aria: { live: 'polite' } });
  const pinBtn = ui.button({ icon: 'pin', title: 'Pin note (Ctrl+Shift+P)', variant: 'ghost', size: 'sm', onClick: () => togglePin() });
  const tagBtn = ui.button({ icon: 'tag', title: 'Tags', variant: 'ghost', size: 'sm', onClick: () => editTags() });
  const moveBtn = ui.button({ icon: 'folder', title: 'Move to folder', variant: 'ghost', size: 'sm', onClick: () => pickFolder() });
  const modeSeg = ui.segmented({
    options: [{ value: 'rich', label: 'Rich', icon: 'edit' }, { value: 'markdown', label: 'Markdown', icon: 'code' }],
    value: mode, size: 'sm', ariaLabel: 'Editor mode', onChange: (v) => setMode(v),
  });
  const moreBtn = ui.menuButton({ icon: 'more', title: 'More', size: 'sm', items: () => moreMenu() });
  const liveTools = h('div', { class: 'ne-tools' }, pinBtn, tagBtn, moveBtn, modeSeg.el, moreBtn);
  const restoreBtn = ui.button({ label: 'Restore', icon: 'restore', size: 'sm', onClick: () => note && app.restoreNotes([note.id]) });
  const destroyBtn = ui.button({ label: 'Delete', icon: 'trash', size: 'sm', variant: 'danger-ghost', onClick: () => note && app.destroyNotes([note.id]) });
  const trashTools = h('div', { class: 'ne-tools' }, restoreBtn, destroyBtn);
  const header = h('header', { class: 'ne-header' }, backBtn, statusEl, h('div', { class: 'spacer' }), liveTools, trashTools);

  const banner = h('div', { class: 'ne-banner', hidden: true });
  const tagInput = h('input', {
    class: 'ne-tag-input', type: 'text', placeholder: 'Add tag', autocomplete: 'off', spellcheck: 'false', maxlength: '64',
    aria: { label: 'Add tag' }, list: 'ne-tag-suggestions',
  });
  const tagList = h('datalist', { id: 'ne-tag-suggestions' });
  const tagsRow = h('div', { class: 'ne-tags', hidden: true });
  const host = h('div', { class: 'ne-host' });
  const readView = h('article', { class: 'ne-read md', hidden: true, tabindex: '0', aria: { label: 'Note (read only)' } });
  const emptyEl = h('div', { class: 'ne-empty' });
  const loadingEl = h('div', { class: 'ne-loading', hidden: true }, ui.skeletonText(5));
  const el = h('section', { class: 'ne-pane', aria: { label: 'Note editor' } },
    header, banner, tagsRow, loadingEl, host, readView, emptyEl, tagList);

  /* -------------------------------------------------------------- empty */
  function renderEmpty() {
    const trash = app.state.view.kind === 'trash';
    emptyEl.replaceChildren(h('div', { class: 'ne-empty-inner' },
      h('div', { class: 'ne-empty-art', aria: { hidden: 'true' } }, icon(trash ? 'trash' : 'note', 36)),
      h('h3', { class: 'ne-empty-title' }, trash ? 'Nothing selected' : 'No note selected'),
      h('p', { class: 'ne-empty-msg' }, trash ? 'Select a note in the Trash to preview, restore or delete it.' : 'Pick a note from the list or start a new one.'),
      trash ? null : h('div', { class: 'ne-empty-actions' },
        ui.button({ label: 'New note', icon: 'edit', variant: 'primary', onClick: () => app.newNote() }),
        ui.kbd(app.isApple ? '⌘' : 'Ctrl', app.isApple ? '⌥' : 'Alt', 'N'))));
  }

  function showState(which) {
    emptyEl.hidden = which !== 'empty';
    loadingEl.hidden = which !== 'loading';
    host.hidden = which !== 'edit';
    readView.hidden = which !== 'read';
    header.hidden = which === 'empty' || which === 'loading';
    tagsRow.hidden = which === 'empty' || which === 'loading' || !(note && (note.tags.length || tagEditing));
    banner.hidden = which !== 'read';
    el.classList.toggle('is-empty', which === 'empty');
  }

  /* -------------------------------------------------------------- status */
  function renderStatus() {
    if (!note) return;
    const st = saver?.state || 'saved';
    let content;
    switch (st) {
      case 'saving': content = [ui.spinner({ size: 'sm', label: 'Saving' }), 'Saving…']; break;
      case 'dirty': content = [h('span', { class: 'ne-dot', aria: { hidden: 'true' } }), 'Edited']; break;
      case 'error': content = [icon('alert', 14), 'Offline · retrying']; break;
      case 'conflict': content = [icon('alert-circle', 14), 'Conflict']; break;
      case 'gone': content = [icon('alert-circle', 14), 'Deleted elsewhere']; break;
      default: content = note.trashed_at
        ? [icon('trash', 14), `Deleted ${listDate(note.trashed_at)}`]
        : [icon('check', 14), `Saved · ${listDate(note.updated_at)}`];
    }
    statusEl.className = cls('ne-status', `is-${st}`);
    statusEl.replaceChildren(...content);
    statusEl.dataset.tip = `Last edited ${formatDateTime(note.updated_at)}`;
  }

  function renderHeader() {
    if (!note) return;
    const trashed = !!note.trashed_at;
    liveTools.hidden = trashed;
    trashTools.hidden = !trashed;
    pinBtn.classList.toggle('is-on', !!note.pinned);
    pinBtn.setAttribute('aria-pressed', String(!!note.pinned));
    pinBtn.dataset.tip = note.pinned ? 'Unpin note (Ctrl+Shift+P)' : 'Pin note (Ctrl+Shift+P)';
    const folderName = note.folder_id ? folderPath(app.state.tree.byId, note.folder_id) : 'Notes';
    moveBtn.dataset.tip = `Folder: ${folderName || 'Notes'}`;
    modeSeg.setValue(mode);
    banner.replaceChildren(icon('trash', 16), h('span', {}, 'This note is in the Trash. Restore it to edit.'));
    renderTags();
    renderStatus();
  }

  /* -------------------------------------------------------------- tags */
  function renderTags() {
    if (!note) return;
    const chips = note.tags.map((t) => ui.chip({ label: `#${t}`, onRemove: note.trashed_at ? null : () => setTags(note.tags.filter((x) => x !== t)) }));
    tagList.replaceChildren(...app.state.tags.filter((t) => !note.tags.includes(t.tag)).map((t) => h('option', { value: t.tag })));
    mountEl(tagsRow, icon('tag', { size: 14, className: 'ne-tags-icon' }), ...chips, tagEditing && !note.trashed_at ? tagInput : null,
      !tagEditing && !note.trashed_at ? h('button', { class: 'ne-tag-add', type: 'button', onClick: () => editTags() }, icon('plus', 12), 'Tag') : null);
    tagsRow.hidden = !(note.tags.length || tagEditing);
  }

  function editTags() {
    if (!note || note.trashed_at) return;
    tagEditing = true;
    renderTags();
    tagInput.value = '';
    tagInput.focus();
  }

  function normTag(raw) {
    return String(raw || '').trim().replace(/^#+/, '').replace(/\s+/g, ' ').toLowerCase();
  }

  function addTagFromInput() {
    const parts = tagInput.value.split(',').map(normTag).filter(Boolean);
    tagInput.value = '';
    const bad = parts.find((t) => /["'[\]]/.test(t) || [...t].length > 64);
    if (bad) {
      ui.toast.warning('Tags can’t contain quotes or brackets and are limited to 64 characters.');
      return;
    }
    const next = [...note.tags];
    for (const t of parts) if (!next.includes(t)) next.push(t);
    if (next.length > 100) {
      ui.toast.warning('A note can have at most 100 tags.');
      return;
    }
    if (next.length !== note.tags.length) setTags(next);
  }

  tagInput.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' || e.key === ',' || e.key === 'Tab') {
      if (tagInput.value.trim()) {
        e.preventDefault();
        addTagFromInput();
      } else if (e.key === 'Enter') {
        e.preventDefault();
        stopTagEditing(true);
      }
    } else if (e.key === 'Backspace' && !tagInput.value && note?.tags.length) {
      e.preventDefault();
      setTags(note.tags.slice(0, -1));
    } else if (e.key === 'Escape') {
      e.preventDefault();
      e.stopPropagation();
      tagInput.value = '';
      stopTagEditing(true);
    }
  });
  tagInput.addEventListener('blur', () => {
    setTimeout(() => {
      if (document.activeElement === tagInput) return;
      if (tagInput.value.trim()) addTagFromInput();
      stopTagEditing(false);
    }, 120);
  });

  function stopTagEditing(focusEditor) {
    if (!tagEditing) return;
    tagEditing = false;
    renderTags();
    if (focusEditor) editor?.focus();
  }

  function setTags(tags) {
    if (!note || !saver) return;
    note = { ...note, tags };
    renderTags();
    if (tagEditing) tagInput.focus();
    saver.update({ tags }).catch(() => {});
  }

  /* -------------------------------------------------------------- actions */
  function togglePin() {
    if (!note || !saver || note.trashed_at) return;
    note = { ...note, pinned: !note.pinned };
    renderHeader();
    saver.update({ pinned: note.pinned }).catch(() => {});
  }

  function pickFolder() {
    if (!note || !saver) return;
    ui.openMenu({
      anchor: moveBtn, align: 'end', className: 'ne-folder-menu',
      items: app.folderMenuItems(note.folder_id, (fid) => {
        note = { ...note, folder_id: fid };
        renderHeader();
        saver.update({ folder_id: fid }).then(() => {
          const name = fid ? folderPath(app.state.tree.byId, fid) : 'Notes';
          ui.toast.success(`Moved to ${name}`, { timeout: 2500 });
        }).catch(() => {});
      }),
    });
  }

  function moreMenu() {
    if (!note) return [];
    return [
      { label: 'Note info', icon: 'info', onSelect: () => showInfo() },
      { label: 'Move to folder…', icon: 'folder', onSelect: () => app.pickFolderAndMove([note.id]) },
      { label: note.pinned ? 'Unpin' : 'Pin', icon: 'pin', shortcut: app.isApple ? '⇧⌘P' : 'Ctrl+Shift+P', onSelect: () => togglePin() },
      { label: 'Export as Markdown', icon: 'download', onSelect: async () => { await flush(); app.api.exportNote(note.id); } },
      { label: 'Copy Markdown', icon: 'copy', onSelect: () => ui.copyText(editor ? editor.getMarkdown() : note.body, { message: 'Markdown copied' }) },
      { separator: true },
      { label: 'Delete note', icon: 'trash', danger: true, shortcut: 'Del', onSelect: () => app.trashNotes([note.id]) },
    ];
  }

  function showInfo() {
    if (!note) return;
    const md = editor ? editor.getMarkdown() : note.body;
    const c = countWords(md);
    ui.modal({
      title: displayTitle(note), size: 'sm',
      body: ui.kv([
        ['Created', formatDateTime(note.created_at)],
        ['Modified', formatDateTime(note.updated_at)],
        ['Folder', note.folder_id ? folderPath(app.state.tree.byId, note.folder_id) || '—' : 'Notes'],
        ['Tags', note.tags.length ? note.tags.map((t) => `#${t}`).join(' ') : '—'],
        ['Words', plural(c.words, 'word')],
        ['Characters', plural(c.chars, 'character')],
        ['Contains code', note.has_code ? 'Yes' : 'No'],
        ['Version', String(note.version)],
      ]),
      footer: [{ label: 'Close', value: null, variant: 'primary' }],
    });
  }

  /* -------------------------------------------------------------- editor */
  async function uploadImage(file) {
    if (!note) throw new Error('No note is open.');
    if (file.size > MAX_ATTACHMENT) throw new Error('Images are limited to 25 MiB.');
    const res = await app.api.uploadAttachment(note.id, file);
    return res.url;
  }

  function onEdit(md) {
    if (!note || !saver) return;
    saver.edit(md);
    clearTimeout(previewTimer);
    previewTimer = setTimeout(() => {
      if (!note) return;
      const p = previewSummary(note, md, explicitTitle);
      app.previewNote({ ...summaryOf(note), ...p });
    }, 120);
  }

  function makeEditor(md) {
    host.replaceChildren();
    const common = {
      container: host, markdown: md, onChange: onEdit, onUploadImage: uploadImage, debounceMs: 100,
      placeholder: 'Start writing…', ariaLabel: 'Note body',
    };
    editor = mode === 'markdown' ? createMarkdownEditor({ ...common, mode: mdView }) : createRichEditor(common);
    editor.on('error', (err) => ui.toast.error(err));
    if (mode === 'markdown') {
      editor.on('mode', (m) => {
        mdView = m;
        writePref(storage, prefKey(userId, 'mdview'), m);
      });
    }
    host.classList.toggle('is-markdown', mode === 'markdown');
  }

  function destroyEditor() {
    if (!editor) return;
    try {
      editor.destroy();
    } catch (err) {
      console.error(err);
    }
    editor = null;
    host.replaceChildren();
  }

  function setMode(m) {
    if (!MODES.includes(m) || m === mode) return;
    mode = m;
    writePref(storage, prefKey(userId, 'editor'), m);
    modeSeg.setValue(m);
    if (!note || note.trashed_at) return;
    const md = editor ? editor.getMarkdown() : note.body;
    destroyEditor();
    makeEditor(md);
    editor.focus();
  }

  function pickMeta(p) {
    const out = {};
    for (const k of ['tags', 'pinned', 'folder_id']) if (k in p) out[k] = p[k];
    return out;
  }

  function summaryOf(n) {
    const { body, ...rest } = n;
    return rest;
  }

  function makeSaver(n) {
    saver?.stop();
    saver = createSaver({
      note: { id: n.id, version: n.version, body: n.body },
      delay: 800,
      send: (id, patch) => app.api.update(id, patch),
      onState: () => renderStatus(),
      onSaved: (server) => {
        if (!note || server.id !== note.id) {
          app.noteSaved(summaryOf(server));
          return;
        }
        // Fields still pending locally win over the (older) server copy in the UI.
        const pend = saver?.pending || {};
        note = { ...server };
        for (const k of ['tags', 'pinned', 'folder_id']) if (k in pend) note[k] = pend[k];
        renderHeader();
        app.noteSaved(summaryOf(server));
      },
      onConflict: ({ current, mine }) => resolveConflict(current, mine),
      onGone: () => resolveGone(),
      onError: (err) => ui.toast.error(err),
    });
  }

  /* -------------------------------------------------------------- conflicts */
  async function resolveConflict(current, mine) {
    if (!note || !current || current.id !== note.id) return;
    editor?.flush(); // capture keystrokes still inside the editor's debounce
    const textEdits = 'body' in saver.pending || 'title' in saver.pending || 'body' in mine || 'title' in mine;
    if (!textEdits) {
      // Only metadata (pin/tags/folder) is pending and the text changed elsewhere: take
      // their text and re-apply our metadata on top of the new version.
      note = { ...current, ...pickMeta(saver.pending) };
      editor?.setMarkdown(current.body);
      saver.resolve({ version: current.version, body: current.body });
      renderHeader();
      app.noteSaved(summaryOf(current));
      return;
    }
    const myBody = editor ? editor.getMarkdown() : (mine.body ?? note.body);
    const col = (label, when, text) => h('div', { class: 'ne-conflict-col' },
      h('div', { class: 'ne-conflict-label' }, label, when ? h('span', { class: 'faint small' }, ` · ${when}`) : null),
      h('div', { class: 'ne-conflict-text' }, toPlainText(text, 360) || '(empty)'));
    const m = ui.modal({
      title: 'This note changed on another device',
      description: 'Both versions were edited. Choose what to keep — nothing is lost if you keep both.',
      size: 'lg', dismissible: false,
      body: h('div', { class: 'ne-conflict' },
        col('Your version', 'not saved yet', myBody),
        col('Other version', `saved ${formatDateTime(current.updated_at)}`, current.body)),
      footer: [
        { label: 'Keep both', value: 'both', icon: 'copy' },
        { label: 'Use other version', value: 'theirs' },
        { label: 'Keep mine', value: 'mine', variant: 'primary' },
      ],
    });
    const choice = await m.closed;
    if (!note || note.id !== current.id || !saver) return;
    if (choice === 'mine') {
      note = { ...current, body: current.body };
      saver.resolve({ version: current.version, body: current.body });
      renderHeader();
      return;
    }
    if (choice === 'both') {
      try {
        const copy = await app.api.create({ body: myBody, folder_id: current.folder_id, tags: current.tags });
        app.noteCreated(copy);
        ui.toast.success('Your version was saved as a separate note.', {
          action: { label: 'Open', onClick: () => app.openNote(copy.id, { push: true }) },
        });
      } catch (err) {
        ui.toast.error(err);
        saver.resolve({ version: note.version, body: saver.savedBody }); // keep editing; the conflict will reappear
        return;
      }
    }
    // theirs (or both): load the server version.
    note = current;
    saver.resolve({ version: current.version, body: current.body, keepPending: false });
    editor?.setMarkdown(current.body);
    renderHeader();
    app.noteSaved(summaryOf(current));
  }

  async function resolveGone() {
    if (!note) return;
    const id = note.id;
    const body = editor ? editor.getMarkdown() : note.body;
    const m = ui.modal({
      title: 'This note was deleted', size: 'sm', dismissible: false,
      description: 'It was deleted permanently on another device while you were editing it.',
      footer: [{ label: 'Discard my changes', value: 'discard', variant: 'danger-ghost' }, { label: 'Save as new note', value: 'save', variant: 'primary' }],
    });
    const choice = await m.closed;
    saver?.stop();
    saver = null;
    if (choice === 'save') {
      try {
        const copy = await app.api.create({ body, folder_id: note?.folder_id || null, tags: note?.tags || [] });
        app.noteCreated(copy);
        note = null;
        app.noteMissing(id, copy.id);
        return;
      } catch (err) {
        ui.toast.error(err);
      }
    }
    note = null;
    app.noteMissing(id);
  }

  /* -------------------------------------------------------------- lifecycle */
  function load(n, { focus = false } = {}) {
    note = n;
    tagEditing = false;
    explicitTitle = hasExplicitTitle(n);
    if (n.trashed_at) {
      saver?.stop();
      saver = null;
      destroyEditor();
      renderInto(readView, n.body || '', { onTaskToggle: null });
      if (!n.body?.trim()) readView.replaceChildren(h('p', { class: 'faint' }, 'This note is empty.'));
      showState('read');
    } else {
      makeSaver(n);
      if (!editor) makeEditor(n.body);
      else editor.setMarkdown(n.body);
      showState('edit');
      if (focus) editor.focus();
    }
    renderHeader();
  }

  async function flush() {
    try {
      editor?.flush();
    } catch (err) {
      console.error(err);
    }
    if (!saver) return true;
    return saver.flush();
  }

  /** Flush, stop and forget the current note (deleting it when it is a still-empty new note). */
  async function close() {
    loadToken += 1;
    clearTimeout(previewTimer);
    const prev = note;
    const md = editor && prev && !prev.trashed_at ? editor.getMarkdown() : prev?.body;
    const ok = await flush();
    saver?.stop();
    saver = null;
    note = null;
    if (prev && createdHere.has(prev.id) && blankMarkdown(md) && !explicitTitle && !prev.tags.length && ok) {
      createdHere.delete(prev.id);
      app.discardEmpty(prev.id);
    }
    return ok;
  }

  return {
    el,
    get note() { return note; },
    get dirty() { return !!saver?.dirty; },
    get mode() { return mode; },
    async open(id, { focus = false } = {}) {
      if (note && note.id === id) {
        if (focus) editor?.focus();
        return;
      }
      await close();
      const my = loadToken;
      showState('loading');
      let n;
      try {
        n = await app.api.get(id);
      } catch (err) {
        if (my !== loadToken) return;
        if (err?.status === 404) {
          showState('empty');
          renderEmpty();
          app.noteMissing(id);
          return;
        }
        showState('empty');
        emptyEl.replaceChildren(ui.errorState(err, () => app.openNote(id, { replace: true })));
        return;
      }
      if (my !== loadToken) return;
      load(n, { focus });
    },
    /** Show a note that was just created (no extra GET). */
    openCreated(n, { focus = true } = {}) {
      createdHere.add(n.id);
      loadToken += 1;
      load(n, { focus });
    },
    async clear() {
      await close();
      destroyEditor();
      showState('empty');
      renderEmpty();
    },
    close,
    flush,
    focus() { if (editor) editor.focus(); else readView.focus(); },
    togglePin,
    setMode,
    toggleMode() { setMode(mode === 'rich' ? 'markdown' : 'rich'); },
    /** External change to the open note (multi-select move/pin, restore, …). */
    patchLocal(fields) {
      if (!note) return;
      note = { ...note, ...fields };
      renderHeader();
    },
    /** Saver for list-driven metadata changes of the open note (keeps versions in sync). */
    updateOpen(fields) {
      if (!saver || !note) return null;
      note = { ...note, ...fields };
      renderHeader();
      return saver.update(fields);
    },
    /** Reload the open note if it changed elsewhere and there are no local edits. */
    async refresh() {
      if (!note || saver?.dirty) return;
      const id = note.id;
      try {
        const n = await app.api.get(id);
        if (!note || note.id !== id || saver?.dirty) return;
        if (n.version !== note.version) {
          const hadFocus = editor && el.contains(document.activeElement);
          load(n, { focus: hadFocus });
          app.noteSaved(summaryOf(n));
        }
      } catch (err) {
        if (err?.status === 404 && note?.id === id && !saver?.dirty) {
          note = null;
          app.noteMissing(id);
        }
      }
    },
    renderTags() { if (note) renderTags(); },
    renderEmpty,
    destroy() {
      clearTimeout(previewTimer);
      saver?.stop();
      destroyEditor();
    },
  };
}
