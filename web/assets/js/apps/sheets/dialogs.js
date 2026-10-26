// Sheets dialogs and popovers: colour palette, "API & AI" drawer, Save to Files, the Files
// picker for imports and the function browser.

import { h, cls, on } from '../../core/dom.js';
import { icon } from '../../core/icons.js';
import { formatBytes } from '../../core/format.js';
import { sicon } from './icons.js';

const PALETTE = [
  ['#000000', '#434343', '#666666', '#999999', '#b7b7b7', '#cccccc', '#d9d9d9', '#efefef', '#f3f3f3', '#ffffff'],
  ['#980000', '#ff0000', '#ff9900', '#ffff00', '#00ff00', '#00ffff', '#4a86e8', '#0000ff', '#9900ff', '#ff00ff'],
  ['#e6b8af', '#f4cccc', '#fce5cd', '#fff2cc', '#d9ead3', '#d0e0e3', '#c9daf8', '#cfe2f3', '#d9d2e9', '#ead1dc'],
  ['#dd7e6b', '#ea9999', '#f9cb9c', '#ffe599', '#b6d7a8', '#a2c4c9', '#a4c2f4', '#9fc5e8', '#b4a7d6', '#d5a6bd'],
  ['#cc4125', '#e06666', '#f6b26b', '#ffd966', '#93c47d', '#76a5af', '#6d9eeb', '#6fa8dc', '#8e7cc3', '#c27ba0'],
  ['#a61c00', '#cc0000', '#e69138', '#f1c232', '#6aa84f', '#45818e', '#3c78d8', '#3d85c6', '#674ea7', '#a64d79'],
  ['#85200c', '#990000', '#b45f06', '#bf9000', '#38761d', '#134f5c', '#1155cc', '#0b5394', '#351c75', '#741b47'],
];

const NAMES = { '#000000': 'Black', '#ffffff': 'White', '#ff0000': 'Red', '#ffff00': 'Yellow', '#00ff00': 'Green', '#0000ff': 'Blue', '#fff2cc': 'Light yellow', '#ffe599': 'Yellow highlight' };

let openPop = null;

/** Close the open popover, if any. */
export function closePopover() {
  openPop?.close();
}

/**
 * Colour palette popover under an anchor.
 * @param {HTMLElement} anchor
 * @param {{title: string, current?: string, resetLabel: string, onPick: (color: string) => void}} o
 *   onPick('') resets the colour.
 */
export function colorPopover(anchor, o) {
  if (openPop?.anchor === anchor) { closePopover(); return; }
  closePopover();
  const grid = h('div', { class: 'sh-palette', role: 'grid', aria: { label: o.title } });
  const swatches = [];
  PALETTE.forEach((row) => {
    const r = h('div', { class: 'sh-palette-row', role: 'row' });
    for (const c of row) {
      const b = h('button', {
        type: 'button', class: cls('sh-swatch', { 'is-current': o.current === c }), role: 'gridcell',
        aria: { label: NAMES[c] || c }, dataset: { color: c, tip: NAMES[c] || c },
      });
      b.style.backgroundColor = c;
      b.addEventListener('click', () => { api.close(); o.onPick(c); });
      swatches.push(b);
      r.append(b);
    }
    grid.append(r);
  });
  const custom = h('input', { type: 'color', class: 'sh-color-input', value: o.current || '#000000', aria: { label: 'Custom colour' } });
  custom.addEventListener('change', () => { api.close(); o.onPick(custom.value.toLowerCase()); });
  const pop = h('div', { class: 'sh-popover', role: 'dialog', aria: { label: o.title } },
    h('button', { type: 'button', class: 'sh-pop-reset', onClick: () => { api.close(); o.onPick(''); } }, icon('ban', 14), o.resetLabel),
    grid,
    h('label', { class: 'sh-pop-custom' }, custom, h('span', {}, 'Custom…')));
  grid.addEventListener('keydown', (e) => {
    const i = swatches.indexOf(document.activeElement);
    if (i < 0) return;
    const d = { ArrowRight: 1, ArrowLeft: -1, ArrowDown: 10, ArrowUp: -10 }[e.key];
    if (!d) return;
    e.preventDefault();
    swatches[Math.max(0, Math.min(swatches.length - 1, i + d))].focus();
  });
  const api = popover(anchor, pop);
  requestAnimationFrame(() => (swatches.find((s) => s.classList.contains('is-current')) || swatches[0]).focus());
}

/** Position a floating element under an anchor; closes on outside click, Escape, resize. */
export function popover(anchor, el) {
  document.body.appendChild(el);
  const r = anchor.getBoundingClientRect();
  const vw = document.documentElement.clientWidth;
  const left = Math.max(8, Math.min(r.left, vw - el.offsetWidth - 8));
  let top = r.bottom + 4;
  if (top + el.offsetHeight > window.innerHeight - 8) top = Math.max(8, r.top - el.offsetHeight - 4);
  el.style.left = `${left}px`;
  el.style.top = `${top}px`;
  anchor.setAttribute('aria-expanded', 'true');
  const offs = [];
  const api = {
    anchor,
    close() {
      if (openPop !== api) return;
      openPop = null;
      offs.forEach((f) => f());
      el.remove();
      anchor.setAttribute('aria-expanded', 'false');
      anchor.focus({ preventScroll: true });
    },
  };
  setTimeout(() => {
    offs.push(on(document, 'pointerdown', (e) => { if (!el.contains(e.target) && !anchor.contains(e.target)) api.close(); }, true));
    offs.push(on(document, 'keydown', (e) => { if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); api.close(); } }, true));
    offs.push(on(window, 'resize', () => api.close()));
  }, 0);
  openPop = api;
  return api;
}

/* ------------------------------------------------------------------ API & AI */

function codeBlock(ui, label, code) {
  return h('div', { class: 'sh-code' },
    h('div', { class: 'sh-code-head' }, h('span', {}, label), ui.copyButton(code, { title: `Copy ${label}` })),
    h('pre', { class: 'sh-code-body mono' }, code));
}

/**
 * Drawer with ids and ready-to-copy snippets for automating this workbook.
 * @param {any} ctx app context
 * @param {{id: string, name: string}} wb
 * @param {{id: string, name: string}} sheet
 */
export function apiPanel(ctx, wb, sheet) {
  const { ui } = ctx;
  const origin = location.origin;
  const base = `${origin}/api/v1/sheets/${wb.id}`;
  const sp = `${base}/sheets/${sheet.id}`;
  const curlGet = `curl -s -H "Authorization: Bearer $LUDWORKS_TOKEN" \\\n  "${sp}/range?ref=A1:D20"`;
  const curlPut = `curl -s -X PUT -H "Authorization: Bearer $LUDWORKS_TOKEN" \\\n  -H "Content-Type: application/json" \\\n  -d '{"ref":"A1","values":[["Item","Cost"],["Disk",189.99],["Total","=SUM(B2:B2)"]]}' \\\n  "${sp}/range"`;
  const curlPatch = `curl -s -X PATCH -H "Authorization: Bearer $LUDWORKS_TOKEN" \\\n  -H "Content-Type: application/json" \\\n  -d '{"updates":[{"ref":"C2","input":"12%"}],"range_style":{"ref":"A1:B1","style":{"b":true,"bg":"#fff2cc"}}}' \\\n  "${sp}/cells"`;
  const curlExport = `curl -s -H "Authorization: Bearer $LUDWORKS_TOKEN" \\\n  -o "${wb.name.replace(/["\\$`]/g, '_')}.xlsx" "${base}/export?format=xlsx"`;
  const py = `import ludworks

lw = ludworks.Ludworks("${origin}", "lwp_…your token…")
wb, sheet = "${wb.id}", "${sheet.id}"   # or the sheet name: "${sheet.name.replace(/["\\]/g, '_')}"

# read values and formulas
rng = lw.sheets.get_range(wb, sheet, "A1:D20")
print(rng.values[0], rng.formulas[0])

# write a block (strings starting with "=" are formulas)
lw.sheets.set_range(wb, sheet, "A1", [
    ["Item", "Cost"],
    ["Disk", 189.99],
    ["Total", "=SUM(B2:B2)"],
])

# fine-grained edits and formatting
lw.sheets.update_cells(wb, sheet, [
    {"ref": "C2", "input": "12%"},
    {"ref": "A1", "style": {"b": True, "bg": "#fff2cc"}},
])`;
  const agent = `You can edit the Ludworks workbook "${wb.name}" through its HTTP API.
Base URL: ${origin}/api/v1  (send "Authorization: Bearer <token>")
Workbook id: ${wb.id}; sheet "${sheet.name}" id: ${sheet.id}
- Read:  GET /sheets/${wb.id}/sheets/${sheet.id}/range?ref=A1:D20 → {values, formulas, styles}
- Write: PUT /sheets/${wb.id}/sheets/${sheet.id}/range {"ref":"A1","values":[[...]]} ("=" strings are formulas)
- Edit/format: PATCH /sheets/${wb.id}/sheets/${sheet.id}/cells {"updates":[{"ref","value"|"formula"|"input","style","clear"}],"range_style":{"ref","style"}}
- Functions: GET /sheets/functions; try formulas with POST /sheets/evaluate
Full reference: ${origin}/api/docs`;
  const idRow = (label, value) => h('div', { class: 'sh-id-row' },
    h('span', { class: 'sh-id-label' }, label), h('code', { class: 'sh-id-value' }, value), ui.copyButton(value, { title: `Copy ${label}` }));
  ui.drawer({
    title: 'API & AI',
    description: 'Automate this workbook from scripts, the Python SDK or an AI agent.',
    size: 'lg',
    className: 'sh-api-drawer',
    body: h('div', { class: 'stack' },
      h('div', { class: 'sh-ids' }, idRow('Workbook ID', wb.id), idRow(`Sheet ID (${sheet.name})`, sheet.id)),
      ui.alert({
        variant: 'info', title: 'Authentication',
        message: h('span', {}, 'Create a personal access token with the ', h('code', {}, 'sheets:read'), ' and ', h('code', {}, 'sheets:write'),
          ' scopes in Settings → API tokens, then export it as ', h('code', {}, 'LUDWORKS_TOKEN'), '.'),
        actions: ui.button({ label: 'API tokens', size: 'sm', href: '/settings/tokens' }),
      }),
      h('section', { class: 'stack-sm' }, h('h3', { class: 'section-title' }, 'curl'),
        codeBlock(ui, 'Read a range', curlGet), codeBlock(ui, 'Write values and formulas', curlPut),
        codeBlock(ui, 'Edit and format cells', curlPatch), codeBlock(ui, 'Download as XLSX', curlExport)),
      h('section', { class: 'stack-sm' }, h('h3', { class: 'section-title' }, 'Python SDK'),
        codeBlock(ui, 'pip', 'pip install ludworks'), codeBlock(ui, 'Python', py)),
      h('section', { class: 'stack-sm' }, h('h3', { class: 'section-title' }, 'Prompt for an AI agent'), codeBlock(ui, 'Agent instructions', agent)),
      h('div', { class: 'row wrap' },
        ui.button({ label: 'API reference', icon: 'code', size: 'sm', href: '/api/docs', target: '_blank' }))),
  });
}

/* --------------------------------------------------------------- Save to Files */

async function loadSpaces(api, signal) {
  const res = await api.get('/spaces', { signal });
  const list = Array.isArray(res) ? res : res?.items || [];
  return list.filter((s) => s.access !== 'read');
}

/**
 * Export the workbook into Files.
 * @param {any} ctx
 * @param {{id: string, name: string}} wb
 * @param {{id: string, name: string}} sheet the current sheet (for CSV)
 */
export async function saveToFilesDialog(ctx, wb, sheet) {
  const { ui, api } = ctx;
  let spaces;
  try {
    spaces = await loadSpaces(api, ctx.signal);
  } catch (err) {
    ui.toast.error(err);
    return;
  }
  if (!spaces.length) {
    ui.toast.error('You have no writable space in Files.');
    return;
  }
  const spaceSel = h('select', { class: 'select', id: 'sh-sf-space' }, spaces.map((s) => h('option', { value: s.id }, s.kind === 'home' && !/home/i.test(s.name) ? `${s.name} (Home)` : s.name)));
  const pathIn = h('input', { class: 'input', id: 'sh-sf-path', placeholder: 'Documents/Spreadsheets', autocomplete: 'off', spellcheck: 'false' });
  const nameIn = h('input', { class: 'input', id: 'sh-sf-name', autocomplete: 'off' });
  nameIn.value = wb.name.replace(/[/\\:*?"<>|]/g, '_');
  let format = 'xlsx';
  const fmt = ui.segmented({
    options: [{ value: 'xlsx', label: 'XLSX' }, { value: 'xls', label: 'XLS' }, { value: 'csv', label: 'CSV (this sheet)' }],
    value: format, ariaLabel: 'File format', onChange: (v) => { format = v; },
  });
  const overwrite = h('input', { type: 'checkbox', class: 'checkbox', id: 'sh-sf-over' });
  const err = h('div', { class: 'field-error', role: 'alert', hidden: true });
  const field = (label, id, ctl, hint) => h('div', { class: 'field' }, h('label', { class: 'field-label', for: id }, label), ctl, hint ? h('div', { class: 'field-hint' }, hint) : null);
  const m = ui.modal({
    title: 'Save a copy to Files',
    size: 'md',
    body: h('div', { class: 'stack-md' },
      field('Space', 'sh-sf-space', spaceSel),
      field('Folder', 'sh-sf-path', pathIn, 'Leave empty for the top level. Missing folders must exist already.'),
      field('File name', 'sh-sf-name', nameIn, 'The extension is added automatically.'),
      h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Format'), fmt.el),
      h('label', { class: 'check-row', for: 'sh-sf-over' }, overwrite, h('span', { class: 'check-text' }, h('span', { class: 'check-label' }, 'Replace an existing file with the same name'))),
      err),
    footer: [{ label: 'Cancel', value: null }, { label: 'Save to Files', variant: 'primary', type: 'submit', icon: 'upload' }],
    onSubmit: async () => {
      err.hidden = true;
      try {
        const entry = await api.post(`/sheets/${wb.id}/export-to-files`, {
          format, space_id: spaceSel.value, path: pathIn.value.trim().replace(/^\/+|\/+$/g, ''),
          name: nameIn.value.trim() || undefined, sheet_id: format === 'csv' ? sheet.id : undefined, overwrite: overwrite.checked,
        });
        ui.toast.success(`Saved “${entry.name}” to Files`, {
          action: { label: 'Open folder', onClick: () => ctx.router.navigate(`/files?space=${encodeURIComponent(entry.space_id)}&path=${encodeURIComponent(entry.path.split('/').slice(0, -1).join('/'))}`) },
        });
        return true;
      } catch (e) {
        err.textContent = e.code === 'conflict' ? 'A file with that name already exists. Tick “Replace” or choose another name.' : ui.errorMessage(e);
        err.hidden = false;
        return false;
      }
    },
  });
  return m.closed;
}

/* ------------------------------------------------------------- Files picker */

const SHEET_EXT = /\.(xlsx|xlsm|xls|csv|tsv|txt)$/i;

/**
 * Pick a spreadsheet stored in Files. Resolves {id, name} or null.
 * @param {any} ctx
 */
export function filesPicker(ctx) {
  const { ui, api } = ctx;
  let spaces = [];
  let space = null;
  let path = '';
  let chosen = null;
  const list = h('div', { class: 'sh-picker-list', role: 'listbox', aria: { label: 'Files' } });
  const crumbs = h('div', { class: 'sh-picker-crumbs' });
  const spaceSel = h('select', { class: 'select sh-picker-space', aria: { label: 'Space' } });
  const search = h('input', { class: 'input', type: 'search', placeholder: 'Search spreadsheets…', aria: { label: 'Search files' } });
  let seq = 0;

  const row = (entry) => {
    const b = h('button', {
      type: 'button', class: cls('sh-picker-item', { 'is-dir': entry.is_dir }), role: 'option',
      aria: { selected: 'false' }, dataset: { id: entry.id },
    },
    icon(entry.is_dir ? 'folder' : 'sheet', { size: 16 }),
    h('span', { class: 'sh-picker-name truncate' }, entry.name),
    entry.is_dir ? icon('chevron-right', { size: 14, className: 'faint' }) : h('span', { class: 'faint small nums' }, formatBytes(entry.size)));
    b.addEventListener('click', () => {
      if (entry.is_dir) { path = entry.path; search.value = ''; load(); return; }
      chosen = entry;
      for (const el of list.querySelectorAll('[aria-selected="true"]')) el.setAttribute('aria-selected', 'false');
      b.setAttribute('aria-selected', 'true');
      m.buttons[1].disabled = false;
      m.buttons[1].dataset.locked = '';
    });
    b.addEventListener('dblclick', () => { if (!entry.is_dir) m.close(entry); });
    return b;
  };

  async function load() {
    const my = ++seq;
    chosen = null;
    m.buttons[1].disabled = true;
    list.replaceChildren(ui.loadingBlock('Loading…'));
    try {
      let items;
      const q = search.value.trim();
      if (q) {
        const res = await api.get('/files/search', { query: { q, type: 'file', limit: 100 }, signal: ctx.signal });
        items = (res.items || []).filter((e) => SHEET_EXT.test(e.name));
        crumbs.replaceChildren(h('span', { class: 'muted small' }, `Results for “${q}”`));
      } else {
        const res = await api.get('/files', { query: { space_id: space, path, sort: 'name', limit: 200 }, signal: ctx.signal });
        items = (res.items || []).filter((e) => e.is_dir || SHEET_EXT.test(e.name));
        const parts = path ? path.split('/') : [];
        crumbs.replaceChildren(
          h('button', { type: 'button', class: 'link-button', onClick: () => { path = ''; load(); } }, spaces.find((s) => s.id === space)?.name || 'Root'),
          ...parts.flatMap((p, i) => [h('span', { class: 'faint' }, ' / '),
            i === parts.length - 1 ? h('span', {}, p) : h('button', { type: 'button', class: 'link-button', onClick: () => { path = parts.slice(0, i + 1).join('/'); load(); } }, p)]));
      }
      if (my !== seq) return;
      list.replaceChildren(...(items.length ? items.map(row) : [ui.emptyState({ icon: 'sheet', title: 'No spreadsheets here', message: 'XLSX, XLS and CSV files appear in this list.', compact: true })]));
    } catch (err) {
      if (my !== seq || err?.name === 'AbortError') return;
      list.replaceChildren(ui.errorState(err, load));
    }
  }

  const m = ui.modal({
    title: 'Import from Files',
    description: 'Choose an XLSX, XLS or CSV file. The original file stays unchanged.',
    size: 'lg',
    className: 'sh-picker',
    body: h('div', { class: 'stack-sm' }, h('div', { class: 'row' }, spaceSel, search), crumbs, list),
    footer: [{ label: 'Cancel', value: null }, { label: 'Import', variant: 'primary', disabled: true, onClick: (mm) => mm.close(chosen) }],
  });
  spaceSel.addEventListener('change', () => { space = spaceSel.value; path = ''; load(); });
  let t = 0;
  search.addEventListener('input', () => { clearTimeout(t); t = setTimeout(load, 250); });
  (async () => {
    try {
      const res = await api.get('/spaces', { signal: ctx.signal });
      spaces = Array.isArray(res) ? res : res?.items || [];
      spaceSel.replaceChildren(...spaces.map((s) => h('option', { value: s.id }, s.kind === 'home' && !/home/i.test(s.name) ? `${s.name} (Home)` : s.name)));
      space = spaces[0]?.id || null;
      if (!space) {
        list.replaceChildren(ui.emptyState({ icon: 'folder', title: 'No spaces available', compact: true }));
        return;
      }
      load();
    } catch (err) {
      list.replaceChildren(ui.errorState(err, null));
    }
  })();
  return m.closed.then((v) => (v && v.id ? v : null));
}

/* ----------------------------------------------------------- function browser */

/**
 * Searchable list of functions. Resolves the chosen function name or null.
 * @param {any} ctx
 * @param {Array<{name, syntax, description}>} fns
 */
export function functionsDialog(ctx, fns) {
  const { ui } = ctx;
  const search = h('input', { class: 'input', type: 'search', placeholder: 'Search functions…', aria: { label: 'Search functions' } });
  const list = h('div', { class: 'sh-fn-list', role: 'listbox', aria: { label: 'Functions' } });
  const render = () => {
    const q = search.value.trim().toLowerCase();
    const items = fns.filter((f) => !q || f.name.toLowerCase().includes(q) || f.description.toLowerCase().includes(q));
    list.replaceChildren(...items.slice(0, 300).map((f) => {
      const b = h('button', { type: 'button', class: 'sh-fn-item', role: 'option' },
        h('span', { class: 'sh-fn-syntax mono' }, f.syntax), h('span', { class: 'sh-fn-desc' }, f.description));
      b.addEventListener('click', () => m.close(f.name));
      return b;
    }));
    if (!items.length) list.replaceChildren(ui.emptyState({ icon: 'search', title: 'No matching functions', compact: true }));
  };
  search.addEventListener('input', render);
  search.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') {
      e.preventDefault();
      list.querySelector('button')?.click();
    }
  });
  const m = ui.modal({
    title: 'Insert function', description: `${fns.length} functions. Click one to start a formula in the active cell.`, size: 'lg',
    className: 'sh-fn-dialog', body: h('div', { class: 'stack-sm' }, search, list), footer: [{ label: 'Close', value: null }],
  });
  render();
  return m.closed.then((v) => (typeof v === 'string' ? v : null));
}

export { sicon };
