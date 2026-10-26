// Folders pane: smart views (All Notes, Notes, Pinned), the folder tree (create, rename,
// delete, drag to nest, drop notes to move), tags, Trash and the import/export entry.

import { h, cls, mount as mountEl } from '../../core/dom.js';
import { icon } from '../../core/icons.js';
import { flattenTree, isWithin } from './model.js';

export const MIME_NOTES = 'application/x-lw-notes';
export const MIME_FOLDER = 'application/x-lw-folder';

function readDrag(e) {
  const dt = e.dataTransfer;
  if (!dt) return null;
  const types = Array.from(dt.types || []);
  if (types.includes(MIME_NOTES)) return { kind: 'notes' };
  if (types.includes(MIME_FOLDER)) return { kind: 'folder' };
  return null;
}

function parseIds(raw) {
  try {
    const v = JSON.parse(raw);
    return Array.isArray(v) ? v.filter((x) => typeof x === 'string' && /^[A-Za-z0-9_-]{8,64}$/.test(x)) : [];
  } catch {
    return [];
  }
}

/**
 * @param {object} app controller (see index.js)
 */
export function createSidebar(app) {
  const { ui } = app.ctx;
  const body = h('div', { class: 'nf-scroll' });
  const el = h('nav', { class: 'nf-pane', aria: { label: 'Folders' } },
    h('div', { class: 'nf-header' },
      h('h2', { class: 'nf-heading' }, 'Notes'),
      ui.button({ icon: 'plus', title: 'New folder', variant: 'ghost', size: 'sm', onClick: () => app.newFolder(null) })),
    body,
    h('div', { class: 'nf-footer' },
      ui.button({ label: 'Import & Export', icon: 'archive', variant: 'ghost', size: 'sm', block: true, onClick: () => app.openImportExport() })));

  /** Attach drop handling to a row. accept(drag) → bool; drop(data) → void */
  function dropTarget(node, { notes, folder }) {
    let depth = 0;
    node.addEventListener('dragenter', (e) => {
      const d = readDrag(e);
      if (!d || (d.kind === 'notes' && !notes) || (d.kind === 'folder' && !folder)) return;
      e.preventDefault();
      depth += 1;
      node.classList.add('is-drop');
    });
    node.addEventListener('dragover', (e) => {
      const d = readDrag(e);
      if (!d || (d.kind === 'notes' && !notes) || (d.kind === 'folder' && !folder)) return;
      e.preventDefault();
      e.dataTransfer.dropEffect = 'move';
    });
    node.addEventListener('dragleave', () => {
      depth = Math.max(0, depth - 1);
      if (!depth) node.classList.remove('is-drop');
    });
    node.addEventListener('drop', (e) => {
      depth = 0;
      node.classList.remove('is-drop');
      const dt = e.dataTransfer;
      if (notes && dt.types.includes(MIME_NOTES)) {
        e.preventDefault();
        const ids = parseIds(dt.getData(MIME_NOTES));
        if (ids.length) notes(ids);
      } else if (folder && dt.types.includes(MIME_FOLDER)) {
        e.preventDefault();
        const id = dt.getData(MIME_FOLDER);
        if (/^[A-Za-z0-9_-]{8,64}$/.test(id)) folder(id);
      }
    });
  }

  function smartRow({ label, ic, view, active, drop, extra }) {
    const b = h('button', {
      class: cls('nf-item', { 'is-active': active }), type: 'button',
      aria: { current: active ? 'page' : undefined },
      onClick: () => app.selectView(view),
    }, icon(ic, { size: 16, className: 'nf-icon' }), h('span', { class: 'nf-label' }, label), extra || null);
    const row = h('li', { class: 'nf-row' }, b);
    if (drop) dropTarget(b, drop);
    return row;
  }

  function folderMenu(f) {
    return [
      { label: 'New note here', icon: 'edit', onSelect: () => app.newNote({ folderId: f.id }) },
      { label: 'New subfolder', icon: 'folder', onSelect: () => app.newFolder(f.id) },
      { label: 'Rename…', icon: 'edit', shortcut: 'F2', onSelect: () => app.renameFolder(f.id) },
      f.parent_id ? { label: 'Move to top level', icon: 'arrow-up', onSelect: () => app.moveFolder(f.id, null) } : null,
      { label: 'Export as ZIP', icon: 'download', onSelect: () => app.exportFolder(f.id) },
      { separator: true },
      { label: 'Delete folder…', icon: 'trash', danger: true, onSelect: () => app.deleteFolder(f.id) },
    ];
  }

  function folderRow(f, view, collapsed) {
    const active = view.kind === 'folder' && view.id === f.id;
    const hasKids = f.children.length > 0;
    const open = hasKids && !collapsed.has(f.id);
    const chevron = hasKids
      ? h('button', {
        class: 'nf-chevron', type: 'button', tabindex: '-1',
        aria: { label: open ? `Collapse ${f.name}` : `Expand ${f.name}`, expanded: String(open) },
        onClick: (e) => { e.stopPropagation(); app.toggleCollapsed(f.id); },
      }, icon(open ? 'chevron-down' : 'chevron-right', 14))
      : h('span', { class: 'nf-chevron-spacer', aria: { hidden: 'true' } });
    const item = h('button', {
      class: cls('nf-item', 'nf-folder', { 'is-active': active }), type: 'button', draggable: 'true',
      aria: { current: active ? 'page' : undefined, expanded: hasKids ? String(open) : undefined },
      dataset: { folder: f.id },
      onClick: () => app.selectView({ kind: 'folder', id: f.id }),
    },
    icon('folder', { size: 16, className: 'nf-icon' }),
    h('span', { class: 'nf-label' }, f.name),
    f.note_count ? h('span', { class: 'nf-count' }, String(f.note_count)) : null);
    item.addEventListener('dragstart', (e) => {
      e.dataTransfer.setData(MIME_FOLDER, f.id);
      e.dataTransfer.effectAllowed = 'move';
    });
    item.addEventListener('contextmenu', (e) => ui.contextMenu(e, folderMenu(f)));
    item.addEventListener('keydown', (e) => {
      if (e.key === 'F2') { e.preventDefault(); app.renameFolder(f.id); }
      else if (e.key === 'Delete' || (e.key === 'Backspace' && (e.metaKey || e.ctrlKey))) { e.preventDefault(); app.deleteFolder(f.id); }
      else if (e.key === 'ArrowRight' && hasKids && !open) { e.preventDefault(); app.toggleCollapsed(f.id); }
      else if (e.key === 'ArrowLeft' && open) { e.preventDefault(); app.toggleCollapsed(f.id); }
      else if (e.key === 'ContextMenu' || (e.shiftKey && e.key === 'F10')) {
        e.preventDefault();
        const r = item.getBoundingClientRect();
        ui.openMenu({ items: folderMenu(f), x: r.left + 24, y: r.bottom });
      }
    });
    dropTarget(item, {
      notes: (ids) => app.moveNotes(ids, f.id),
      folder: (id) => {
        if (id === f.id || isWithin(app.state.tree.byId, f.id, id)) {
          ui.toast.warning('A folder can’t be moved into itself.');
          return;
        }
        app.moveFolder(id, f.id);
      },
    });
    const more = ui.menuButton({ icon: 'more', title: `${f.name} actions`, size: 'sm', items: () => folderMenu(f), className: 'nf-more' });
    more.tabIndex = -1;
    const row = h('li', { class: cls('nf-row', 'nf-row-folder', { 'is-active': active }) }, chevron, item, more);
    row.style.setProperty('--depth', String(f.depth));
    return row;
  }

  function render() {
    const s = app.state;
    const view = s.view;
    const flat = flattenTree(s.tree.roots, s.collapsed);
    const folderHeader = h('div', { class: 'nf-section-head' },
      h('span', { class: 'nf-section-title' }, 'Folders'),
      ui.button({ icon: 'plus', title: 'New folder', variant: 'ghost', size: 'sm', onClick: () => app.newFolder(null) }));
    dropTarget(folderHeader, { folder: (id) => app.moveFolder(id, null) });
    mountEl(body,
      h('ul', { class: 'nf-list' },
        smartRow({ label: 'All Notes', ic: 'layers', view: { kind: 'all' }, active: view.kind === 'all' }),
        smartRow({
          label: 'Notes', ic: 'note', view: { kind: 'none' }, active: view.kind === 'none',
          drop: { notes: (ids) => app.moveNotes(ids, null) },
        }),
        smartRow({ label: 'Pinned', ic: 'pin', view: { kind: 'pinned' }, active: view.kind === 'pinned' })),
      folderHeader,
      flat.length
        ? h('ul', { class: 'nf-list nf-tree' }, flat.map((f) => folderRow(f, view, s.collapsed)))
        : h('button', { class: 'nf-empty-hint', type: 'button', onClick: () => app.newFolder(null) }, icon('plus', 14), 'Create a folder'),
      s.tags.length ? h('div', { class: 'nf-section-head' }, h('span', { class: 'nf-section-title' }, 'Tags')) : null,
      s.tags.length ? h('ul', { class: 'nf-list nf-tags' }, s.tags.map((t) => smartRow({
        label: t.tag, ic: 'hash', view: { kind: 'tag', tag: t.tag }, active: view.kind === 'tag' && view.tag === t.tag,
        extra: h('span', { class: 'nf-count' }, String(t.count)),
      }))) : null,
      h('ul', { class: 'nf-list nf-trash' },
        smartRow({
          label: 'Trash', ic: 'trash', view: { kind: 'trash' }, active: view.kind === 'trash',
          drop: { notes: (ids) => app.trashNotes(ids) },
        })));
  }

  return { el, render };
}
