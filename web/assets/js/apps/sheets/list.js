// Workbook list: cards or table, search (top bar), create, rename, delete, download, and
// import from the computer (button or drag-and-drop) or from Files.

import { h, cls, pickFiles } from '../../core/dom.js';
import { icon } from '../../core/icons.js';
import { formatBytes, plural } from '../../core/format.js';
import { filesPicker } from './dialogs.js';

const VIEW_KEY = 'lw.sheets.view';
const ACCEPT = '.xlsx,.xlsm,.xls,.csv,.tsv,.txt,application/vnd.openxmlformats-officedocument.spreadsheetml.sheet,application/vnd.ms-excel,text/csv';
const MAX_IMPORT = 50 * 1024 * 1024;

function readView() {
  try {
    return localStorage.getItem(VIEW_KEY) === 'list' ? 'list' : 'cards';
  } catch {
    return 'cards';
  }
}

function formatOf(file) {
  const m = /\.(xlsx|xlsm|xls|csv|tsv|txt)$/i.exec(file.name);
  if (!m) return '';
  const ext = m[1].toLowerCase();
  return ext === 'xlsm' ? 'xlsx' : ext === 'tsv' || ext === 'txt' ? 'csv' : ext;
}

const baseName = (n) => n.replace(/\.[^.]+$/, '').trim().slice(0, 200) || 'Imported workbook';

/**
 * Mount the list view.
 * @returns {{update: (ctx: any) => void, unmount: () => void}}
 */
export function mountList(container, ctx) {
  const { ui, api } = ctx;
  let view = readView();
  let q = ctx.query.q || '';
  let items = [];
  let next = null;
  let loading = false;
  let seq = 0;

  ctx.setTitle('Sheets');
  ctx.setLayout({});
  ctx.setSearch({
    placeholder: 'Search workbooks', value: q,
    onInput: (v) => { q = v.trim(); load(true); },
    onSubmit: (v) => { q = v.trim(); load(true); },
  });

  const imports = h('div', { class: 'sh-imports', aria: { live: 'polite' } });
  const body = h('div', { class: 'sh-list-body' });
  const more = h('div', { class: 'sh-more' });
  const viewSeg = ui.segmented({
    options: [{ value: 'cards', icon: 'grid', title: 'Cards' }, { value: 'list', icon: 'list', title: 'List' }],
    value: view, ariaLabel: 'Layout', size: 'sm',
    onChange: (v) => {
      view = v;
      try { localStorage.setItem(VIEW_KEY, v); } catch { /* storage unavailable */ }
      render();
    },
  });
  const header = ui.pageHeader({
    title: 'Sheets',
    subtitle: 'Spreadsheets with formulas — import and export XLSX, XLS and CSV.',
    actions: [
      viewSeg.el,
      ui.menuButton({
        label: 'Import', icon: 'upload', variant: 'secondary', caret: true, align: 'end',
        items: [
          { label: 'From this computer…', icon: 'laptop', onSelect: () => pickAndImport() },
          { label: 'From Files…', icon: 'folder', onSelect: () => importFromFiles() },
        ],
      }),
      ui.button({ label: 'New workbook', icon: 'plus', variant: 'primary', onClick: () => createWorkbook() }),
    ],
  });
  const drop = h('div', { class: 'sh-drop', hidden: true, aria: { hidden: 'true' } },
    h('div', { class: 'sh-drop-inner' }, icon('upload', 28), h('div', { class: 'sh-drop-title' }, 'Drop to import'), h('div', { class: 'muted small' }, 'XLSX, XLS or CSV · up to 50 MiB')));
  const page = h('div', { class: 'page wide sh-list' }, header, imports, body, more, drop);
  container.replaceChildren(page);

  // drag & drop import anywhere on the page
  let depth = 0;
  const hasFiles = (e) => [...(e.dataTransfer?.types || [])].includes('Files');
  page.addEventListener('dragenter', (e) => { if (!hasFiles(e)) return; e.preventDefault(); depth++; drop.hidden = false; });
  page.addEventListener('dragover', (e) => { if (!hasFiles(e)) return; e.preventDefault(); e.dataTransfer.dropEffect = 'copy'; });
  page.addEventListener('dragleave', () => { depth = Math.max(0, depth - 1); if (!depth) drop.hidden = true; });
  page.addEventListener('drop', (e) => {
    if (!hasFiles(e)) return;
    e.preventDefault();
    depth = 0;
    drop.hidden = true;
    importFiles([...e.dataTransfer.files]);
  });

  const offCmds = ctx.commands.register([
    { id: 'sheets:new', title: 'New workbook', icon: 'plus', run: () => createWorkbook() },
    { id: 'sheets:import', title: 'Import spreadsheet…', icon: 'upload', run: () => pickAndImport() },
    { id: 'sheets:import-files', title: 'Import spreadsheet from Files…', icon: 'folder', run: () => importFromFiles() },
  ]);
  const offKey = ctx.shortcuts.register('n', () => createWorkbook(), { description: 'New workbook' });

  async function load(reset) {
    const my = ++seq;
    loading = true;
    if (reset) {
      items = [];
      next = null;
      body.replaceChildren(h('div', { class: 'sh-cards' }, ui.skeletonCards(6, '96px')));
    }
    try {
      const res = await api.get('/sheets', { query: { q: q || undefined, cursor: reset ? undefined : next, limit: 60 }, signal: ctx.signal });
      if (my !== seq) return;
      items = reset ? res.items : items.concat(res.items);
      next = res.next_cursor;
      render();
    } catch (err) {
      if (my !== seq || err?.name === 'AbortError') return;
      body.replaceChildren(ui.errorState(err, () => load(true)));
      more.replaceChildren();
    } finally {
      if (my === seq) loading = false;
    }
  }

  function render() {
    if (!items.length) {
      body.replaceChildren(q
        ? ui.emptyState({ icon: 'search', title: 'No matching workbooks', message: `Nothing is called “${q}”.` })
        : ui.emptyState({
          icon: 'sheet', title: 'No workbooks yet',
          message: 'Create a spreadsheet, or drop an XLSX, XLS or CSV file here to import it.',
          action: [ui.button({ label: 'New workbook', icon: 'plus', variant: 'primary', onClick: () => createWorkbook() }),
            ui.button({ label: 'Import file', icon: 'upload', onClick: () => pickAndImport() })],
        }));
      more.replaceChildren();
      return;
    }
    body.replaceChildren(view === 'list' ? table() : cards());
    more.replaceChildren(...(next ? [ui.button({ label: 'Load more', icon: 'chevron-down', onClick: () => (loading ? null : load(false)) })] : []));
  }

  const open = (wb) => ctx.router.navigate(`/sheets/${wb.id}`);
  const menuItems = (wb) => [
    { label: 'Open', icon: 'external', onSelect: () => open(wb) },
    { label: 'Rename…', icon: 'edit', onSelect: () => rename(wb) },
    { label: 'Download as XLSX', icon: 'download', onSelect: () => api.download(`/sheets/${wb.id}/export`, { format: 'xlsx' }) },
    { separator: true },
    { label: 'Delete…', icon: 'trash', danger: true, onSelect: () => remove(wb) },
  ];

  function cards() {
    return h('div', { class: 'sh-cards' }, items.map((wb) => {
      const sheets = wb.sheets || [];
      const card = h('div', { class: 'card card-hover sh-card', role: 'link', tabindex: '0', aria: { label: `Open ${wb.name}` } },
        h('div', { class: 'sh-card-thumb', aria: { hidden: 'true' } }, miniGrid()),
        h('div', { class: 'sh-card-body' },
          h('div', { class: 'sh-card-title truncate', title: wb.name }, wb.name),
          h('div', { class: 'sh-card-meta' }, `${plural(sheets.length, 'sheet')} · `, ui.timeAgo(wb.updated_at)),
          h('div', { class: 'sh-card-sheets' }, sheets.slice(0, 3).map((s) => h('span', { class: 'sh-chip truncate' }, s.name)),
            sheets.length > 3 ? h('span', { class: 'sh-chip faint' }, `+${sheets.length - 3}`) : null)),
        ui.menuButton({ icon: 'more', title: `Actions for ${wb.name}`, size: 'sm', className: 'sh-card-menu', items: () => menuItems(wb) }));
      card.addEventListener('click', (e) => { if (!e.target.closest('.sh-card-menu')) open(wb); });
      card.addEventListener('keydown', (e) => { if (e.key === 'Enter' && e.target === card) open(wb); });
      card.addEventListener('contextmenu', (e) => ui.contextMenu(e, menuItems(wb)));
      return card;
    }));
  }

  function table() {
    const rows = items.map((wb) => {
      const tr = h('tr', { class: 'clickable', tabindex: '0' },
        h('td', {}, h('div', { class: 'cell-main' }, h('span', { class: 'sh-row-icon' }, icon('sheet', 16)),
          h('span', { class: 'cell-title truncate' }, wb.name))),
        h('td', { class: 'muted' }, (wb.sheets || []).map((s) => s.name).join(', ')),
        h('td', { class: 'muted nowrap' }, ui.timeAgo(wb.updated_at)),
        h('td', { class: 'align-right' }, ui.menuButton({ icon: 'more', title: `Actions for ${wb.name}`, size: 'sm', items: () => menuItems(wb) })));
      tr.addEventListener('click', (e) => { if (!e.target.closest('button')) open(wb); });
      tr.addEventListener('keydown', (e) => { if (e.key === 'Enter' && e.target === tr) open(wb); });
      return tr;
    });
    return h('div', { class: 'table-wrap' }, h('table', { class: 'table sh-table' },
      h('thead', {}, h('tr', {}, h('th', {}, 'Name'), h('th', {}, 'Sheets'), h('th', {}, 'Updated'), h('th', { class: 'align-right' }, h('span', { class: 'sr-only' }, 'Actions')))),
      h('tbody', {}, rows)));
  }

  async function createWorkbook() {
    const name = await ui.prompt({
      title: 'New workbook', label: 'Name', value: 'Untitled spreadsheet', confirmLabel: 'Create',
      validate: (v) => (!v.trim() ? 'Enter a name' : v.trim().length > 200 ? 'At most 200 characters' : null),
    });
    if (name === null) return;
    try {
      const wb = await api.post('/sheets', { name: name.trim() });
      ctx.router.navigate(`/sheets/${wb.id}`);
    } catch (err) {
      ui.toast.error(err);
    }
  }

  async function rename(wb) {
    const name = await ui.prompt({
      title: 'Rename workbook', label: 'Name', value: wb.name, confirmLabel: 'Rename',
      validate: (v) => (!v.trim() ? 'Enter a name' : v.trim().length > 200 ? 'At most 200 characters' : null),
    });
    if (name === null || name.trim() === wb.name) return;
    try {
      const res = await api.patch(`/sheets/${wb.id}`, { name: name.trim() });
      Object.assign(wb, res);
      render();
    } catch (err) {
      ui.toast.error(err);
    }
  }

  async function remove(wb) {
    const ok = await ui.confirm({
      title: `Delete “${wb.name}”?`, message: 'The workbook and all its sheets are permanently deleted. Exported copies in Files are not affected.',
      confirmLabel: 'Delete workbook', danger: true,
    });
    if (!ok) return;
    try {
      await api.del(`/sheets/${wb.id}`);
      items = items.filter((x) => x !== wb);
      render();
      ui.toast.success(`Deleted “${wb.name}”`);
    } catch (err) {
      ui.toast.error(err);
    }
  }

  async function pickAndImport() {
    const files = await pickFiles({ multiple: true, accept: ACCEPT });
    if (files.length) importFiles(files);
  }

  function importFiles(files) {
    let opened = 0;
    for (const file of files) {
      const format = formatOf(file);
      if (!format) {
        ui.toast.error(`“${file.name}” is not an XLSX, XLS or CSV file.`);
        continue;
      }
      if (file.size > MAX_IMPORT) {
        ui.toast.error(`“${file.name}” is larger than 50 MiB.`);
        continue;
      }
      const bar = ui.progress({ value: 0, size: 'sm', label: `Importing ${file.name}` });
      const status = h('span', { class: 'muted small nums' }, formatBytes(file.size));
      const row = h('div', { class: 'card sh-import' },
        icon('sheet', 18), h('div', { class: 'sh-import-main' }, h('div', { class: 'truncate' }, file.name), bar.el), status);
      imports.append(row);
      api.upload('POST', '/sheets/import', file, {
        query: { format, name: baseName(file.name) },
        headers: { 'Content-Type': 'application/octet-stream' },
        signal: ctx.signal,
        onProgress: (p) => {
          bar.set(p.fraction);
          if (p.fraction >= 1) status.textContent = 'Processing…';
        },
      }).then((wb) => {
        row.remove();
        items = [wb, ...items.filter((x) => x.id !== wb.id)];
        render();
        ui.toast.success(`Imported “${wb.name}”`, { action: { label: 'Open', onClick: () => open(wb) } });
        if (files.length === 1 && ++opened === 1) open(wb);
      }).catch((err) => {
        row.remove();
        if (err?.name !== 'AbortError') ui.toast.error(err, { description: `Could not import “${file.name}”.` });
      });
    }
  }

  async function importFromFiles() {
    const entry = await filesPicker(ctx);
    if (!entry) return;
    const t = ui.toast.info(`Importing “${entry.name}”…`, { timeout: 0 });
    try {
      const wb = await api.post('/sheets/import-file', { file_id: entry.id });
      t();
      ui.toast.success(`Imported “${wb.name}”`);
      open(wb);
    } catch (err) {
      t();
      ui.toast.error(err);
    }
  }

  load(true);
  return {
    update(next) {
      ctx = next;
    },
    unmount() {
      offCmds?.();
      offKey?.();
      ctx.setSearch(null);
    },
  };
}

/** Decorative mini spreadsheet for cards. */
function miniGrid() {
  return h('div', { class: 'sh-mini' }, Array.from({ length: 12 }, (_, i) => h('span', { class: cls('sh-mini-cell', { head: i < 3 }) })));
}
