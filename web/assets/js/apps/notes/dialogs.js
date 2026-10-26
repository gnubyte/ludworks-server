// Import/export dialog and the folder picker used for moving notes.

import { h, cls, pickFiles, mount as mountEl } from '../../core/dom.js';
import { icon } from '../../core/icons.js';
import { formatBytes, plural } from '../../core/format.js';
import { flattenTree, classifyImportFile } from './model.js';
import { MAX_IMPORT } from './api.js';

/** <select> options for "Top level" + every folder (indented by depth). */
function folderOptions(tree, { none = 'Top level', selected = '' } = {}) {
  const opts = [h('option', { value: '' }, none)];
  for (const f of flattenTree(tree.roots)) {
    const o = h('option', { value: f.id }, `${'   '.repeat(f.depth)}${f.name}`);
    if (f.id === selected) o.selected = true;
    opts.push(o);
  }
  return opts;
}

/**
 * Folder picker dialog. Resolves with a folder id, null (= "Notes", no folder) or undefined
 * when cancelled.
 */
export function pickFolderDialog(app, { title = 'Move to folder', current = undefined } = {}) {
  const { ui } = app.ctx;
  let choice;
  const items = [{ id: null, name: 'Notes', depth: 0, icon: 'note' }, ...flattenTree(app.state.tree.roots).map((f) => ({ ...f, icon: 'folder' }))];
  const list = h('div', { class: 'np-list', role: 'listbox', aria: { label: 'Folders' } }, items.map((f) => {
    const isCurrent = current !== undefined && f.id === current;
    const b = h('button', {
      class: cls('np-item', { 'is-current': isCurrent }), type: 'button', role: 'option',
      onClick: () => { choice = f.id; m.close(f.id); },
    }, icon(f.icon, { size: 16 }), h('span', { class: 'np-name' }, f.name), isCurrent ? h('span', { class: 'faint small' }, 'current') : null);
    b.style.setProperty('--depth', String(f.depth));
    return b;
  }));
  list.addEventListener('keydown', (e) => {
    const all = Array.from(list.querySelectorAll('.np-item'));
    const i = all.indexOf(document.activeElement);
    if (e.key === 'ArrowDown') { e.preventDefault(); all[Math.min(all.length - 1, i + 1)]?.focus(); }
    if (e.key === 'ArrowUp') { e.preventDefault(); all[Math.max(0, i - 1)]?.focus(); }
  });
  const m = ui.modal({
    title, size: 'sm', body: list,
    footer: [{ label: 'New folder…', icon: 'plus', onClick: async () => {
      const f = await app.newFolder(null, { select: false });
      if (f) { choice = f.id; m.close(f.id); }
    } }, { label: 'Cancel', value: undefined }],
    initialFocus: list.querySelector('.np-item'),
  });
  return m.closed.then((v) => (v === undefined ? (choice === undefined ? undefined : choice) : v));
}

/**
 * Import & export dialog.
 * @param {object} app
 * @param {{tab?: 'import'|'export', files?: File[]}} [o]
 */
export function importExportDialog(app, o = {}) {
  const { ui } = app.ctx;
  let files = [];
  let busy = false;

  /* --------------------------------------------------------- import */
  const fileList = h('ul', { class: 'nio-files' });
  const target = h('select', { class: 'select', id: 'nio-target' }, folderOptions(app.state.tree, {
    selected: app.state.view.kind === 'folder' ? app.state.view.id : '',
  }));
  const results = h('div', { class: 'nio-results', aria: { live: 'polite' } });
  const prog = ui.progress({ value: 0, label: 'Import progress' });
  prog.el.hidden = true;
  const drop = h('div', { class: 'nio-drop', tabindex: '0', role: 'button', aria: { label: 'Choose Markdown files or a ZIP archive to import' } },
    icon('upload', 28),
    h('div', { class: 'nio-drop-title' }, 'Drop .md files or a .zip here'),
    h('div', { class: 'nio-drop-sub' }, 'or click to choose · folders inside a ZIP become note folders · front matter (title, tags, dates, pinned) is kept'));
  const importBtn = ui.button({ label: 'Import', icon: 'upload', variant: 'primary', disabled: true, onClick: () => runImport() });

  function renderFiles() {
    fileList.replaceChildren(...files.map((f, i) => h('li', { class: 'nio-file' },
      icon(classifyImportFile(f) === 'zip' ? 'archive' : 'file-text', 16),
      h('span', { class: 'nio-file-name' }, f.name),
      h('span', { class: 'faint small nums' }, formatBytes(f.size)),
      busy ? null : h('button', { class: 'btn btn-ghost btn-icon btn-sm', type: 'button', aria: { label: `Remove ${f.name}` }, onClick: () => { files.splice(i, 1); renderFiles(); } }, icon('x', 14)))));
    fileList.hidden = files.length === 0;
    importBtn.disabled = busy || files.length === 0;
  }

  function addFiles(list) {
    const rejected = [];
    for (const f of list) {
      const kind = classifyImportFile(f);
      if (kind === 'unsupported') rejected.push(`${f.name}: only .md, .markdown, .txt and .zip files`);
      else if (f.size > MAX_IMPORT) rejected.push(`${f.name}: larger than 50 MiB`);
      else if (!files.some((x) => x.name === f.name && x.size === f.size)) files.push(f);
    }
    if (rejected.length) ui.toast.warning('Some files were skipped', { description: rejected.slice(0, 4).join('\n') });
    results.replaceChildren();
    renderFiles();
  }

  drop.addEventListener('click', async () => addFiles(await pickFiles({ multiple: true, accept: '.md,.markdown,.txt,.zip,text/markdown,text/plain,application/zip' })));
  drop.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); drop.click(); }
  });
  drop.addEventListener('dragover', (e) => {
    if (Array.from(e.dataTransfer?.types || []).includes('Files')) {
      e.preventDefault();
      drop.classList.add('is-over');
    }
  });
  drop.addEventListener('dragleave', () => drop.classList.remove('is-over'));
  drop.addEventListener('drop', (e) => {
    e.preventDefault();
    drop.classList.remove('is-over');
    addFiles(Array.from(e.dataTransfer?.files || []));
  });

  async function runImport() {
    if (busy || !files.length) return;
    busy = true;
    renderFiles();
    prog.el.hidden = false;
    prog.set(0);
    results.replaceChildren();
    const folderId = target.value || null;
    const total = files.reduce((s, f) => s + f.size, 0) || 1;
    let done = 0;
    const sum = { imported: 0, folders_created: 0, errors: [] };
    for (const f of [...files]) {
      try {
        const r = await app.api.importFile(f, folderId, (p) => prog.set(done + p.loaded, total));
        sum.imported += r.imported;
        sum.folders_created += r.folders_created;
        for (const er of r.errors || []) sum.errors.push({ file: files.length > 1 ? `${f.name} › ${er.file}` : er.file, error: er.error });
      } catch (err) {
        sum.errors.push({ file: f.name, error: ui.errorMessage(err) });
      }
      done += f.size;
      prog.set(done, total);
    }
    busy = false;
    files = [];
    renderFiles();
    prog.el.hidden = true;
    mountEl(results,
      ui.alert({
        variant: sum.errors.length ? (sum.imported ? 'warning' : 'danger') : 'success',
        title: `Imported ${plural(sum.imported, 'note')}${sum.folders_created ? ` and created ${plural(sum.folders_created, 'folder')}` : ''}`,
        message: sum.errors.length ? `${plural(sum.errors.length, 'file')} could not be imported.` : 'Everything was imported.',
      }),
      sum.errors.length ? h('div', { class: 'nio-errors table-wrap' }, h('table', { class: 'table' },
        h('thead', {}, h('tr', {}, h('th', {}, 'File'), h('th', {}, 'Problem'))),
        h('tbody', {}, sum.errors.slice(0, 500).map((er) => h('tr', {}, h('td', { class: 'mono small' }, er.file), h('td', { class: 'small' }, er.error)))))) : null);
    if (sum.imported || sum.folders_created) app.afterImport();
    if (sum.folders_created) {
      // The imported folders must be offered as import targets and export scopes right away.
      await app.refreshFolders();
      syncFolderOptions();
    }
  }

  /** Rebuild both folder <select>s from the current tree, keeping their selection when it still exists. */
  function syncFolderOptions() {
    for (const [sel, opts] of [[target, {}], [exportScope, { none: 'All notes' }]]) {
      const cur = sel.value;
      sel.replaceChildren(...folderOptions(app.state.tree, { ...opts, selected: cur }));
      if (![...sel.options].some((op) => op.value === cur)) sel.value = '';
    }
  }

  const importPanel = h('div', { class: 'nio-panel stack-md' },
    drop, fileList,
    h('div', { class: 'field' }, h('label', { class: 'field-label', for: 'nio-target' }, 'Import into'), target),
    prog.el, h('div', { class: 'nio-row' }, h('span', { class: 'faint small' }, 'Up to 50 MiB per file · 10,000 files per ZIP · 5 MiB per note'), importBtn),
    results);

  /* --------------------------------------------------------- export */
  const exportScope = h('select', { class: 'select', id: 'nio-export' }, folderOptions(app.state.tree, {
    none: 'All notes', selected: app.state.view.kind === 'folder' ? app.state.view.id : '',
  }));
  const exportPanel = h('div', { class: 'nio-panel stack-md', hidden: true },
    h('p', { class: 'muted' }, 'Download your notes as Markdown files in a ZIP archive, laid out in your folder structure. Each file starts with front matter (title, tags, dates, pinned) so it can be imported again here or opened in any Markdown app.'),
    h('div', { class: 'field' }, h('label', { class: 'field-label', for: 'nio-export' }, 'Export'), exportScope),
    h('div', { class: 'nio-row' }, h('span', { class: 'faint small' }, 'Trashed notes and attachments are not included.'),
      ui.button({ label: 'Download ZIP', icon: 'download', variant: 'primary', onClick: () => app.api.exportZip(exportScope.value || null) })));

  const tabs = ui.segmented({
    options: [{ value: 'import', label: 'Import', icon: 'upload' }, { value: 'export', label: 'Export', icon: 'download' }],
    value: o.tab || 'import', ariaLabel: 'Import or export',
    onChange: (v) => { syncFolderOptions(); importPanel.hidden = v !== 'import'; exportPanel.hidden = v !== 'export'; },
  });
  importPanel.hidden = (o.tab || 'import') !== 'import';
  exportPanel.hidden = !importPanel.hidden;
  if (o.files?.length) addFiles(o.files);

  const m = ui.modal({
    title: 'Import & Export', size: 'md', className: 'nio-dialog',
    body: h('div', { class: 'stack-md' }, tabs.el, importPanel, exportPanel),
    footer: [{ label: 'Close', value: null }],
    onClose: () => { busy = false; },
  });
  renderFiles();
  return m;
}
