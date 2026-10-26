// The spreadsheet editor: header, formatting toolbar, name box + formula bar, canvas grid,
// sheet tabs and status bar. Edits are applied optimistically, autosaved in batches and
// reconciled with the server's recalculated cells; undo/redo sends inverse updates.

import { h, cls, debounce } from '../../core/dom.js';
import { icon } from '../../core/icons.js';
import { formatNumber } from '../../core/format.js';
import {
  cellKey, keyRow, keyCol, colName, refName, rangeName, parseRange, parseRef, normRange, rangeArea, inRange, quoteSheet, MAX_ROWS,
} from './a1.js';
import { formulaRefs, shiftFormula } from './formula.js';
import { FORMAT_PRESETS, isDateFormat } from './numfmt.js';
import { parseTSV } from './tsv.js';
import { computeFill, History, cellStats } from './fill.js';
import {
  WorkbookModel, SheetModel, cellEditText, restoreUpdate, mergeStyle, provisionalCell, clipboardPayload, DEFAULT_COL_W, DEFAULT_ROW_H,
} from './model.js';
import { Grid, refColor, COL_HEADER_H } from './grid.js';
import { CellEditor } from './celleditor.js';
import { Saver } from './saver.js';
import { sicon } from './icons.js';
import { colorPopover, closePopover, apiPanel, saveToFilesDialog, functionsDialog } from './dialogs.js';

const FONT_SIZES = [8, 9, 10, 11, 12, 14, 16, 18, 20, 24, 30, 36];
const CLIP_MARK = 'ludworks-sheets';
let functionsCache = null;

/** Apply one API update to a local cell (mirrors the server: clear, then content, then style). */
function localApply(prev, u) {
  let cell = prev ? { ...prev } : null;
  delete cell?._d;
  if (u.clear === 'all') cell = null;
  else if (u.clear === 'content' && cell) cell = cell.s ? { v: null, t: 'z', s: cell.s } : null;
  else if (u.clear === 'style' && cell) { delete cell.s; if (cell.t === 'z' && !cell.f) cell = null; }
  const style = cell?.s;
  if (u.input !== undefined) cell = provisionalCell(u.input, cell);
  else if (u.formula !== undefined) cell = u.formula && u.formula !== '=' ? { v: '…', t: 's', f: u.formula.startsWith('=') ? u.formula : `=${u.formula}`, s: style, pending: true } : (style ? { v: null, t: 'z', s: style } : null);
  else if (u.value !== undefined) {
    const v = u.value;
    const t = v === null ? 'z' : typeof v === 'number' ? 'n' : typeof v === 'boolean' ? 'b' : 's';
    cell = t === 'z' && !style ? null : { v, t, s: style };
  }
  if (u.style) {
    const s = mergeStyle(cell?.s, u.style);
    cell = cell ? { ...cell, s } : (s ? { v: null, t: 'z', s } : null);
    if (cell && !cell.s) delete cell.s;
  }
  if (cell && cell.s === undefined) delete cell.s;
  return cell && (cell.t !== 'z' || cell.s || cell.f) ? cell : null;
}

const toApi = (u) => {
  const { row, col, ...rest } = u;
  return { ...rest, ref: refName(row, col) };
};

export class Editor {
  constructor(ctx, root, wbJSON) {
    this.ctx = ctx;
    this.ui = ctx.ui;
    this.api = ctx.api;
    this.root = root;
    this.wb = new WorkbookModel(wbJSON);
    this.history = new History(200);
    this.sheetState = new Map(); // sheetId → {sel, scroll}
    this.staleSheets = new Set();
    this.clip = null;
    this.findState = null;
    this.editTarget = null;
    this.external = false;
    this.base = `/sheets/${this.wb.id}`;
    this.saver = new Saver({
      api: this.api, base: this.base, version: this.wb.version,
      onResult: (res, op) => this.onSaved(res, op),
      onFail: (err, op) => this.onSaveFailed(err, op),
      onState: (s, d) => this.setSaveState(s, d),
      onExternalChange: () => this.showExternalChange(),
    });
    this.build();
    this.setSaveState('saved');
    const first = this.wb.sheets.find((s) => s.id === ctx.query.sheet) || this.wb.sheets[0];
    this.setSheet(first.id, { initial: true });
    this.loadFunctions();
    this.registerCommands();
  }

  /* ================================================================ layout */

  build() {
    const { ui } = this;
    this.nameBtn = h('button', { type: 'button', class: 'sh-wb-name', aria: { label: 'Rename workbook' }, dataset: { tip: 'Rename' }, onClick: () => this.renameWorkbook() }, this.wb.name);
    this.saveState = h('span', { class: 'sh-save', role: 'status', aria: { live: 'polite' } });
    this.findBtn = ui.button({ icon: 'search', title: 'Find (Ctrl+F)', variant: 'ghost', size: 'sm', onClick: () => this.openFind() });
    this.exportBtn = ui.menuButton({
      label: 'Download', icon: 'download', variant: 'ghost', size: 'sm', caret: true, align: 'end',
      items: () => [
        { label: 'Excel workbook (.xlsx)', icon: 'download', onSelect: () => this.download('xlsx') },
        { label: 'Excel 97–2003 (.xls)', icon: 'download', onSelect: () => this.download('xls') },
        { label: `CSV — ${this.sheet.name}`, icon: 'download', onSelect: () => this.download('csv') },
        { separator: true },
        { label: 'Save a copy to Files…', icon: 'folder', onSelect: () => saveToFilesDialog(this.ctx, this.wb, this.sheet) },
      ],
    });
    const apiBtn = ui.button({ label: 'API & AI', icon: 'code', variant: 'ghost', size: 'sm', onClick: () => apiPanel(this.ctx, this.wb, this.sheet) });
    const header = h('div', { class: 'sh-header' },
      ui.button({ icon: 'arrow-left', title: 'All workbooks', variant: 'ghost', size: 'sm', href: '/sheets' }),
      h('span', { class: 'sh-wb-icon' }, icon('sheet', 18)),
      this.nameBtn, this.saveState,
      h('span', { class: 'spacer' }),
      this.findBtn, this.exportBtn, apiBtn);
    this.banner = h('div', { class: 'sh-banner', hidden: true });

    this.toolbar = this.buildToolbar();

    this.nameBox = h('input', { class: 'sh-namebox', spellcheck: 'false', autocomplete: 'off', aria: { label: 'Name box: selected range' } });
    this.nameBox.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') {
        e.preventDefault();
        this.gotoRef(this.nameBox.value);
      } else if (e.key === 'Escape') {
        e.preventDefault();
        this.updateNameBox();
        this.focusGrid();
      }
    });
    this.nameBox.addEventListener('focus', () => this.nameBox.select());
    this.formulaInput = h('input', { class: 'sh-formula', spellcheck: 'false', autocomplete: 'off', aria: { label: 'Formula bar' } });
    const formulaBar = h('div', { class: 'sh-formulabar' },
      this.nameBox, h('span', { class: 'sh-fx', aria: { hidden: 'true' } }, sicon('function', 16)), this.formulaInput);

    this.grid = new Grid({
      onPointer: (phase, hit, e) => this.onGridPointer(phase, hit, e),
      onDblClick: (hit) => { if (hit.area === 'cell') this.startEdit('edit'); },
      onContextMenu: (hit, e) => this.onContextMenu(hit, e),
      onResizeEnd: (rz, size, orig) => this.resized(rz.axis, rz.index, size, orig),
      onAutoFit: (rz) => this.autoFit(rz.axis, rz.index),
      onFillEnd: (target) => this.fill(target),
      onScroll: () => this.onScroll(),
      onFocus: () => {},
    });
    this.live = h('div', { class: 'sr-only', aria: { live: 'polite', atomic: 'true' } });
    this.gridWrap = h('div', { class: 'sh-gridwrap' }, this.grid.el, this.live);
    this.cellEditor = new CellEditor({
      host: this.gridWrap, bar: this.formulaInput,
      functions: () => functionsCache,
      onCommit: (text, move, opts) => this.commitEdit(text, move, opts),
      onCancel: () => this.cancelEdit(),
      onChange: (text) => this.onEditText(text),
      onPointKey: (dr, dc, extend) => this.pointKey(dr, dc, extend),
      onBarFocus: (typed) => this.startEditFromBar(typed),
      onIdleKey: (e) => this.onKey(e),
      onIdleInput: (text) => this.startEdit('enter', text),
      onIdleCompose: () => this.startEdit('enter', null),
    });
    const sink = this.cellEditor.ta;
    sink.addEventListener('copy', (e) => this.onCopy(e, false));
    sink.addEventListener('cut', (e) => this.onCopy(e, true));
    sink.addEventListener('paste', (e) => this.onPaste(e));
    sink.addEventListener('focus', () => this.root.classList.add('grid-focused'));
    sink.addEventListener('blur', () => this.root.classList.remove('grid-focused'));

    this.tabs = h('div', { class: 'sh-tabs', role: 'tablist', aria: { label: 'Sheets' } });
    this.stats = h('div', { class: 'sh-stats', aria: { live: 'off' } });
    const footer = h('div', { class: 'sh-footer' },
      ui.button({ icon: 'plus', title: 'Add sheet', variant: 'ghost', size: 'sm', onClick: () => this.addSheet() }),
      ui.menuButton({ icon: 'menu', title: 'All sheets', variant: 'ghost', size: 'sm', align: 'start', items: () => this.wb.sheets.map((s) => ({ label: s.name, checked: s === this.sheet, onSelect: () => this.setSheet(s.id) })) }),
      this.tabs, this.stats);

    this.root.replaceChildren(h('div', { class: 'sh-editor' }, header, this.banner, this.toolbar, formulaBar, this.gridWrap, footer));
    const onResize = debounce(() => this.placeEditor(), 50);
    window.addEventListener('resize', onResize);
    this.cleanups = [() => window.removeEventListener('resize', onResize)];
  }

  buildToolbar() {
    const { ui } = this;
    const tb = (name, title, fn, opts = {}) => {
      const b = h('button', {
        type: 'button', class: cls('sh-tb', opts.className), aria: { label: title, pressed: opts.toggle ? 'false' : undefined },
        dataset: { tip: title }, onClick: (e) => fn(e, b),
      }, sicon(name, 16), opts.extra || null);
      if (opts.toggle) this.toggles[opts.toggle] = b;
      return b;
    };
    const menu = (name, title, items, label) => {
      const b = h('button', { type: 'button', class: cls('sh-tb', 'sh-tb-menu'), aria: { label: title, haspopup: 'menu', expanded: 'false' }, dataset: { tip: title } },
        label ? h('span', { class: 'sh-tb-label' }, label) : sicon(name, 16), icon('chevron-down', { size: 12, className: 'sh-tb-caret' }));
      b.addEventListener('click', () => this.ui.dropdown(b, () => this.refocusing(typeof items === 'function' ? items() : items), { align: 'start' }));
      return b;
    };
    const sep = () => h('span', { class: 'sh-tb-sep', aria: { hidden: 'true' } });
    this.toggles = {};
    this.sizeLabel = h('span', { class: 'sh-tb-label' }, '10');
    const sizeBtn = h('button', { type: 'button', class: 'sh-tb sh-tb-menu sh-tb-size', aria: { label: 'Font size', haspopup: 'menu' }, dataset: { tip: 'Font size' } }, this.sizeLabel, icon('chevron-down', { size: 12, className: 'sh-tb-caret' }));
    sizeBtn.addEventListener('click', () => this.ui.dropdown(sizeBtn, () => this.refocusing(FONT_SIZES.map((n) => ({
      label: String(n), checked: (this.activeStyle()?.size || 10) === n, onSelect: () => this.applyStyle({ size: n === 10 ? 0 : n }),
    }))), { align: 'start' }));
    this.colorBar = h('span', { class: 'sh-color-bar' });
    this.fillBar = h('span', { class: 'sh-color-bar sh-fill-bar' });
    const textColor = tb('text-color', 'Text colour', (e, b) => colorPopover(b, {
      title: 'Text colour', current: this.activeStyle()?.color, resetLabel: 'Automatic',
      onPick: (c) => { this.lastColor = c; this.applyStyle({ color: c }); },
    }), { extra: this.colorBar });
    const fillColor = tb('fill-color', 'Fill colour', (e, b) => colorPopover(b, {
      title: 'Fill colour', current: this.activeStyle()?.bg, resetLabel: 'No fill',
      onPick: (c) => { this.lastFill = c; this.applyStyle({ bg: c }); },
    }), { extra: this.fillBar });
    const el = h('div', { class: 'sh-toolbar', role: 'toolbar', aria: { label: 'Formatting' } },
      tb('undo', 'Undo (Ctrl+Z)', () => this.undo()),
      tb('redo', 'Redo (Ctrl+Y)', () => this.redo()),
      sep(),
      tb('currency', 'Format as currency', () => this.applyStyle({ fmt: '$#,##0.00' })),
      tb('percent', 'Format as percent', () => this.applyStyle({ fmt: '0.00%' })),
      tb('decimal-dec', 'Fewer decimal places', () => this.adjustDecimals(-1)),
      tb('decimal-inc', 'More decimal places', () => this.adjustDecimals(1)),
      menu('', 'Number format', () => this.formatMenu(), '123'),
      sep(),
      sizeBtn,
      sep(),
      tb('bold', 'Bold (Ctrl+B)', () => this.toggleStyle('b'), { toggle: 'b' }),
      tb('italic', 'Italic (Ctrl+I)', () => this.toggleStyle('i'), { toggle: 'i' }),
      tb('underline', 'Underline (Ctrl+U)', () => this.toggleStyle('u'), { toggle: 'u' }),
      tb('strike', 'Strikethrough (Alt+Shift+5)', () => this.toggleStyle('strike'), { toggle: 'strike' }),
      textColor, fillColor,
      sep(),
      menu('border-all', 'Borders', () => [
        { label: 'All borders', icon: 'grid', onSelect: () => this.borders('all') },
        { label: 'Outer border', onSelect: () => this.borders('outer') },
        { label: 'Top border', onSelect: () => this.borders('t') },
        { label: 'Bottom border', onSelect: () => this.borders('b') },
        { label: 'Left border', onSelect: () => this.borders('l') },
        { label: 'Right border', onSelect: () => this.borders('r') },
        { separator: true },
        { label: 'No borders', icon: 'ban', onSelect: () => this.borders('none') },
      ]),
      menu('align-left', 'Alignment', () => {
        const st = this.activeStyle() || {};
        return [
          { label: 'Left', checked: st.align === 'left', onSelect: () => this.applyStyle({ align: st.align === 'left' ? '' : 'left' }) },
          { label: 'Center', checked: st.align === 'center', onSelect: () => this.applyStyle({ align: st.align === 'center' ? '' : 'center' }) },
          { label: 'Right', checked: st.align === 'right', onSelect: () => this.applyStyle({ align: st.align === 'right' ? '' : 'right' }) },
          { separator: true },
          { label: 'Top', checked: st.valign === 'top', onSelect: () => this.applyStyle({ valign: 'top' }) },
          { label: 'Middle', checked: st.valign === 'middle', onSelect: () => this.applyStyle({ valign: 'middle' }) },
          { label: 'Bottom', checked: !st.valign || st.valign === 'bottom', onSelect: () => this.applyStyle({ valign: '' }) },
        ];
      }),
      tb('wrap', 'Wrap text', () => this.toggleStyle('wrap'), { toggle: 'wrap' }),
      tb('clear-format', 'Clear formatting (Ctrl+\\)', () => this.clearFormatting()),
      sep(),
      menu('freeze', 'Freeze rows and columns', () => this.freezeMenu()),
      menu('sigma', 'Functions', () => [
        ...['SUM', 'AVERAGE', 'COUNT', 'MAX', 'MIN'].map((f) => ({ label: f, onSelect: () => this.autoFunction(f) })),
        { separator: true },
        { label: 'More functions…', icon: 'search', onSelect: () => this.browseFunctions() },
      ]));
    return el;
  }

  formatMenu() {
    const cur = this.activeStyle()?.fmt || '';
    return [
      ...FORMAT_PRESETS.map((p) => ({ label: `${p.label}  ·  ${p.sample}`, checked: cur === p.fmt, onSelect: () => this.applyStyle({ fmt: p.fmt }) })),
      { separator: true },
      {
        label: 'Custom number format…', icon: 'edit', onSelect: async () => {
          const v = await this.ui.prompt({
            title: 'Custom number format', label: 'Format code', value: cur || '#,##0.00', confirmLabel: 'Apply',
            hint: 'Excel-style codes, e.g. 0.000, #,##0;[Red]-#,##0, "kg "0.0, yyyy-mm-dd hh:mm',
            validate: (s) => (s.length > 255 ? 'At most 255 characters' : null),
          });
          if (v !== null) this.applyStyle({ fmt: v.trim() });
        },
      },
    ];
  }

  freezeMenu() {
    const s = this.sheet;
    const r = this.sel.ar + 1;
    const c = this.sel.ac + 1;
    const set = (patch) => this.updateSheetMeta(patch);
    return [
      { heading: 'Rows' },
      { label: 'No rows', checked: s.frozenRows === 0, onSelect: () => set({ frozen_rows: 0 }) },
      { label: '1 row', checked: s.frozenRows === 1, onSelect: () => set({ frozen_rows: 1 }) },
      { label: '2 rows', checked: s.frozenRows === 2, onSelect: () => set({ frozen_rows: 2 }) },
      { label: `Up to row ${r}`, checked: s.frozenRows === r && r > 2, disabled: r >= s.rows || r > 1000, onSelect: () => set({ frozen_rows: r }) },
      { heading: 'Columns' },
      { label: 'No columns', checked: s.frozenCols === 0, onSelect: () => set({ frozen_cols: 0 }) },
      { label: '1 column', checked: s.frozenCols === 1, onSelect: () => set({ frozen_cols: 1 }) },
      { label: '2 columns', checked: s.frozenCols === 2, onSelect: () => set({ frozen_cols: 2 }) },
      { label: `Up to column ${colName(c - 1)}`, checked: s.frozenCols === c && c > 2, disabled: c >= s.cols || c > 1000, onSelect: () => set({ frozen_cols: c }) },
    ];
  }

  /** Menu items that return keyboard focus to the grid after running. */
  refocusing(items) {
    return items.map((it) => (it && it.onSelect ? { ...it, onSelect: async () => { await it.onSelect(); if (!this.cellEditor.open && !document.querySelector('.modal-backdrop')) this.focusGrid(); } } : it));
  }

  registerCommands() {
    // when focus is on the toolbar or elsewhere in the app (the grid handles its own keys)
    const sc = (combo, fn, description) => this.ctx.shortcuts.register(combo, fn, { description });
    this.offShortcuts = [
      sc('mod+z', () => this.undo(), 'Undo'),
      sc('mod+shift+z', () => this.redo(), 'Redo'),
      sc('mod+y', () => this.redo(), 'Redo'),
      sc('mod+f', () => this.openFind(), 'Find in sheet'),
    ];
    const cmd = (id, title, run, icon2 = 'sheet') => ({ id: `sheets:${id}`, title, icon: icon2, subtitle: this.wb.name, run });
    this.offCommands = this.ctx.commands.register([
      cmd('download-xlsx', 'Download workbook as XLSX', () => this.download('xlsx'), 'download'),
      cmd('download-csv', 'Download sheet as CSV', () => this.download('csv'), 'download'),
      cmd('save-files', 'Save a copy to Files', () => saveToFilesDialog(this.ctx, this.wb, this.sheet), 'folder'),
      cmd('add-sheet', 'Add sheet', () => this.addSheet(), 'plus'),
      cmd('api', 'Show API & AI snippets', () => apiPanel(this.ctx, this.wb, this.sheet), 'code'),
      cmd('find', 'Find in sheet', () => this.openFind(), 'search'),
      cmd('functions', 'Insert function…', () => this.browseFunctions(), 'hash'),
    ]);
  }

  async loadFunctions() {
    if (functionsCache) return;
    try {
      functionsCache = await this.api.get('/sheets/functions', { signal: this.ctx.signal });
    } catch {
      functionsCache = null;
    }
  }

  destroy() {
    this.saver.flush();
    this.saver.close();
    this.grid.destroy();
    closePopover();
    this.cleanups.forEach((f) => f());
    this.offCommands?.();
    this.offShortcuts?.forEach((f) => f?.());
  }

  /* ================================================================ sheets */

  get sheet() {
    return this.activeSheet;
  }

  setSheet(id, opts = {}) {
    const sh = this.wb.sheet(id);
    if (!sh) return;
    if (this.activeSheet && !opts.initial) {
      this.sheetState.set(this.activeSheet.id, { sel: this.sel, scroll: [this.grid.scroller.scrollLeft, this.grid.scroller.scrollTop] });
    }
    const pointing = this.cellEditor.open && this.cellEditor.canPoint();
    if (this.cellEditor.open && !pointing) this.commitEdit(this.cellEditor.value, null);
    this.activeSheet = sh;
    this.grid.setSheet(sh);
    this.renderTabs();
    if (this.staleSheets.has(sh.id)) this.reloadWorkbook();
    const st = this.sheetState.get(sh.id);
    const sel = st?.sel || { r0: 0, c0: 0, r1: 0, c1: 0, ar: 0, ac: 0, anchor: { row: 0, col: 0 }, head: { row: 0, col: 0 } };
    this.setSelection(sel, { scroll: false });
    requestAnimationFrame(() => {
      if (st) {
        this.grid.scroller.scrollLeft = st.scroll[0];
        this.grid.scroller.scrollTop = st.scroll[1];
      } else this.grid.setScroll(0, 0);
      this.grid.invalidate();
    });
    if (pointing) {
      // keep editing the formula in the bar while pointing into another sheet
      const away = this.editTarget && this.editTarget.sheetId !== sh.id;
      this.cellEditor.ta.hidden = away;
      if (away) {
        const n = this.formulaInput.value.length;
        this.formulaInput.focus({ preventScroll: true });
        this.formulaInput.setSelectionRange(n, n);
      } else {
        this.placeEditor();
        this.cellEditor.ta.focus({ preventScroll: true });
      }
      this.onEditText(this.cellEditor.value);
    } else this.focusGrid();
    if (!opts.initial) this.ctx.router.setQuery({ sheet: sh.id });
    this.findState = null;
    this.grid.setFind(null);
    this.grid.setCopyRange(this.clip && this.clip.sheetId === sh.id ? this.clip.range : null);
  }

  renderTabs() {
    const frag = [];
    for (const s of this.wb.sheets) {
      const tab = h('button', {
        type: 'button', class: cls('sh-tab', { 'is-active': s === this.sheet }), role: 'tab',
        aria: { selected: String(s === this.sheet) }, tabindex: s === this.sheet ? '0' : '-1', dataset: { id: s.id },
        draggable: 'true',
      }, h('span', { class: 'sh-tab-name' }, s.name));
      tab.addEventListener('click', () => { if (s !== this.sheet) this.setSheet(s.id); });
      tab.addEventListener('dblclick', () => this.renameSheet(s));
      tab.addEventListener('contextmenu', (e) => this.ui.contextMenu(e, this.refocusing(this.tabMenu(s))));
      tab.addEventListener('keydown', (e) => {
        const i = this.wb.sheets.indexOf(s);
        if (e.key === 'ArrowRight' || e.key === 'ArrowLeft') {
          e.preventDefault();
          const next = this.wb.sheets[(i + (e.key === 'ArrowRight' ? 1 : -1) + this.wb.sheets.length) % this.wb.sheets.length];
          this.setSheet(next.id);
          this.tabs.querySelector(`[data-id="${next.id}"]`)?.focus();
        } else if (e.key === 'F2') { e.preventDefault(); this.renameSheet(s); } else if (e.key === 'Delete') { e.preventDefault(); this.deleteSheet(s); }
      });
      tab.addEventListener('dragstart', (e) => {
        e.dataTransfer.setData('application/x-ludworks-sheet', s.id);
        e.dataTransfer.effectAllowed = 'move';
        tab.classList.add('is-dragging');
      });
      tab.addEventListener('dragend', () => tab.classList.remove('is-dragging'));
      tab.addEventListener('dragover', (e) => {
        if (!e.dataTransfer.types.includes('application/x-ludworks-sheet')) return;
        e.preventDefault();
        const r = tab.getBoundingClientRect();
        tab.classList.toggle('drop-before', e.clientX < r.left + r.width / 2);
        tab.classList.toggle('drop-after', e.clientX >= r.left + r.width / 2);
      });
      tab.addEventListener('dragleave', () => tab.classList.remove('drop-before', 'drop-after'));
      tab.addEventListener('drop', (e) => {
        const id = e.dataTransfer.getData('application/x-ludworks-sheet');
        tab.classList.remove('drop-before', 'drop-after');
        if (!id || id === s.id) return;
        e.preventDefault();
        const r = tab.getBoundingClientRect();
        const after = e.clientX >= r.left + r.width / 2;
        const from = this.wb.sheets.findIndex((x) => x.id === id);
        let to = this.wb.sheets.indexOf(s) + (after ? 1 : 0);
        if (from < to) to -= 1;
        this.moveSheet(id, to);
      });
      frag.push(tab);
    }
    this.tabs.replaceChildren(...frag);
    this.tabs.querySelector('.is-active')?.scrollIntoView({ block: 'nearest', inline: 'nearest' });
  }

  tabMenu(s) {
    const i = this.wb.sheets.indexOf(s);
    return [
      { label: 'Rename…', icon: 'edit', onSelect: () => this.renameSheet(s) },
      { label: 'Move left', icon: 'arrow-left', disabled: i === 0, onSelect: () => this.moveSheet(s.id, i - 1) },
      { label: 'Move right', icon: 'arrow-right', disabled: i === this.wb.sheets.length - 1, onSelect: () => this.moveSheet(s.id, i + 1) },
      { separator: true },
      { label: 'Delete sheet', icon: 'trash', danger: true, disabled: this.wb.sheets.length === 1, onSelect: () => this.deleteSheet(s) },
    ];
  }

  async addSheet() {
    try {
      const res = await this.saver.call(() => this.api.post(`${this.base}/sheets`, {}));
      this.wb.sheets.push(new SheetModel(res));
      this.wb.sheets.forEach((s, i) => { s.index = i; });
      this.setSheet(res.id);
      this.ui.toast.success(`Added ${res.name}`, { timeout: 2000 });
    } catch (err) {
      this.ui.toast.error(err);
    }
  }

  async renameSheet(s) {
    const name = await this.ui.prompt({
      title: 'Rename sheet', label: 'Sheet name', value: s.name, confirmLabel: 'Rename',
      validate: (v) => {
        const t = v.trim();
        if (!t) return 'Enter a name';
        if (t.length > 31) return 'At most 31 characters';
        if (/[:\\/?*[\]]/.test(t)) return 'Names cannot contain : \\ / ? * [ ]';
        if (/^'|'$/.test(t)) return 'Names cannot start or end with an apostrophe';
        if (this.wb.sheets.some((o) => o !== s && o.name.toLowerCase() === t.toLowerCase())) return 'Another sheet has this name';
        return null;
      },
    });
    if (name === null || name.trim() === s.name) { this.focusGrid(); return; }
    try {
      const res = await this.saver.call(() => this.api.patch(`${this.base}/sheets/${s.id}`, { name: name.trim() }));
      s.name = res.name;
      // formulas elsewhere were rewritten: refresh other sheets lazily
      for (const o of this.wb.sheets) if (o !== s) this.staleSheets.add(o.id);
      this.renderTabs();
      this.focusGrid();
    } catch (err) {
      this.ui.toast.error(err);
    }
  }

  async deleteSheet(s) {
    if (this.wb.sheets.length === 1) return;
    const ok = await this.ui.confirm({
      title: `Delete “${s.name}”?`, message: 'The sheet and its cells are deleted. Formulas in other sheets that refer to it show #REF!. This cannot be undone.',
      confirmLabel: 'Delete sheet', danger: true,
    });
    if (!ok) return;
    try {
      await this.saver.call(() => this.api.del(`${this.base}/sheets/${s.id}`));
      const i = this.wb.sheets.indexOf(s);
      this.wb.sheets.splice(i, 1);
      this.wb.sheets.forEach((x, j) => { x.index = j; this.staleSheets.add(x.id); });
      this.history.undoStack = this.history.undoStack.filter((e) => e.sheetId !== s.id);
      this.history.redoStack = this.history.redoStack.filter((e) => e.sheetId !== s.id);
      this.setSheet(this.wb.sheets[Math.max(0, i - 1)].id);
    } catch (err) {
      this.ui.toast.error(err);
    }
  }

  async moveSheet(id, to) {
    const from = this.wb.sheets.findIndex((s) => s.id === id);
    if (from < 0 || to === from) return;
    const [s] = this.wb.sheets.splice(from, 1);
    this.wb.sheets.splice(Math.max(0, Math.min(to, this.wb.sheets.length)), 0, s);
    this.wb.sheets.forEach((x, i) => { x.index = i; });
    this.renderTabs();
    try {
      await this.saver.call(() => this.api.patch(`${this.base}/sheets/${id}`, { index: this.wb.sheets.indexOf(s) }));
    } catch (err) {
      this.ui.toast.error(err);
      this.reloadWorkbook();
    }
  }

  async updateSheetMeta(patch, { history = true } = {}) {
    const s = this.sheet;
    const before = { frozen_rows: s.frozenRows, frozen_cols: s.frozenCols };
    if ('frozen_rows' in patch) s.frozenRows = patch.frozen_rows;
    if ('frozen_cols' in patch) s.frozenCols = patch.frozen_cols;
    this.grid.refresh();
    if (history && ('frozen_rows' in patch || 'frozen_cols' in patch)) {
      this.history.push({ kind: 'meta', sheetId: s.id, forward: patch, backward: Object.fromEntries(Object.keys(patch).map((k) => [k, before[k]])) });
    }
    try {
      await this.saver.call(() => this.api.patch(`${this.base}/sheets/${s.id}`, patch));
    } catch (err) {
      this.ui.toast.error(err);
      this.reloadWorkbook();
    }
  }

  async renameWorkbook() {
    const name = await this.ui.prompt({
      title: 'Rename workbook', label: 'Name', value: this.wb.name, confirmLabel: 'Rename',
      validate: (v) => (!v.trim() ? 'Enter a name' : v.trim().length > 200 ? 'At most 200 characters' : null),
    });
    if (name === null || name.trim() === this.wb.name) return;
    try {
      const res = await this.saver.call(() => this.api.patch(this.base, { name: name.trim() }));
      this.wb.name = res.name;
      this.nameBtn.textContent = res.name;
      this.ctx.setTitle(res.name);
    } catch (err) {
      this.ui.toast.error(err);
    }
  }

  /** Re-fetch the whole workbook (after errors, structural edits elsewhere or external changes). */
  async reloadWorkbook() {
    try {
      const json = await this.api.get(this.base, { signal: this.ctx.signal });
      const curId = this.sheet?.id;
      const fresh = new WorkbookModel(json);
      this.wb.sheets = fresh.sheets;
      this.wb.name = fresh.name;
      this.saver.version = Math.max(this.saver.version, fresh.version);
      this.nameBtn.textContent = fresh.name;
      this.staleSheets.clear();
      this.external = false;
      this.banner.hidden = true;
      if (!this.wb.sheet(curId)) this.setSheet(this.wb.sheets[0].id);
      else {
        this.activeSheet = this.wb.sheet(curId);
        this.grid.setSheet(this.activeSheet);
        this.renderTabs();
        this.setSelection(this.clampSel(this.sel), { scroll: false });
      }
    } catch (err) {
      if (err?.name !== 'AbortError') this.ui.toast.error(err);
    }
  }

  /* ============================================================= selection */

  clampSel(s) {
    const sh = this.sheet;
    const c = (v, m) => Math.max(0, Math.min(v, m - 1));
    return {
      r0: c(s.r0, sh.rows), r1: c(s.r1, sh.rows), c0: c(s.c0, sh.cols), c1: c(s.c1, sh.cols),
      ar: c(s.ar, sh.rows), ac: c(s.ac, sh.cols),
      anchor: { row: c(s.anchor.row, sh.rows), col: c(s.anchor.col, sh.cols) },
      head: { row: c(s.head.row, sh.rows), col: c(s.head.col, sh.cols) },
    };
  }

  setSelection(sel, { scroll = true } = {}) {
    this.sel = this.clampSel(sel);
    this.grid.setSelection(this.sel);
    if (scroll) this.grid.scrollIntoView(this.sel.head.row, this.sel.head.col);
    this.updateNameBox();
    this.updateFormulaBar();
    this.updateToolbarState();
    this.scheduleStats();
    const cell = this.activeCell();
    const desc = `${refName(this.sel.ar, this.sel.ac)}${cell ? `, ${cell.f ? `formula ${cell.f}, ` : ''}${this.sheet.display(cell) || ''}` : ', empty'}`;
    this.live.textContent = desc;
    this.cellEditor.setLabel(`${this.sheet.name}, ${desc}`);
    this.cellEditor.idlePlace(this.grid.cellRect(this.sel.ar, this.sel.ac));
  }

  /** Select a rectangle with the active cell at (ar, ac). */
  selectRange(r0, c0, r1, c1, ar = r0, ac = c0, scroll = true) {
    const n = normRange({ r0, c0, r1, c1 });
    this.setSelection({ ...n, ar, ac, anchor: { row: ar, col: ac }, head: { row: r1, col: c1 } }, { scroll });
  }

  selectCell(row, col) {
    this.setSelection({ r0: row, c0: col, r1: row, c1: col, ar: row, ac: col, anchor: { row, col }, head: { row, col } });
  }

  activeCell() {
    return this.sheet.get(this.sel.ar, this.sel.ac);
  }

  activeStyle() {
    return this.activeCell()?.s;
  }

  updateNameBox() {
    if (document.activeElement === this.nameBox) return;
    const s = this.sel;
    this.nameBox.value = rangeName(s, { rows: this.sheet.rows, cols: this.sheet.cols });
  }

  updateFormulaBar() {
    if (this.cellEditor.open) return;
    this.formulaInput.value = cellEditText(this.activeCell());
  }

  updateToolbarState() {
    const st = this.activeStyle() || {};
    for (const [k, b] of Object.entries(this.toggles)) b.setAttribute('aria-pressed', String(!!st[k]));
    this.sizeLabel.textContent = String(st.size || 10);
    this.colorBar.style.backgroundColor = this.lastColor || st.color || 'var(--c-text)';
    this.fillBar.style.backgroundColor = this.lastFill || st.bg || 'transparent';
    const hist = this.toolbar.querySelectorAll('.sh-tb');
    if (hist[0]) hist[0].disabled = !this.history.canUndo;
    if (hist[1]) hist[1].disabled = !this.history.canRedo;
  }

  scheduleStats() {
    clearTimeout(this.statsTimer);
    this.statsTimer = setTimeout(() => this.updateStats(), 60);
  }

  updateStats() {
    const s = this.sel;
    if (!s || rangeArea(s) <= 1) {
      this.stats.replaceChildren();
      return;
    }
    const cells = [];
    this.sheet.eachIn(s, (_r, _c, cell) => cells.push(cell));
    const st = cellStats(cells);
    if (!st.count) {
      this.stats.replaceChildren();
      return;
    }
    const fmt = (n) => formatNumber(n, { maximumFractionDigits: 4 });
    const item = (label, val) => h('span', { class: 'sh-stat' }, h('span', { class: 'sh-stat-label' }, label), h('span', { class: 'sh-stat-value nums' }, val));
    this.stats.replaceChildren(...(st.numCount
      ? [item('Sum', fmt(st.sum)), item('Average', fmt(st.avg)), item('Count', fmt(st.count))]
      : [item('Count', fmt(st.count))]));
  }

  gotoRef(text) {
    let t = text.trim();
    let target = this.sheet;
    const bang = t.lastIndexOf('!');
    if (bang > 0) {
      const name = t.slice(0, bang).replace(/^'(.*)'$/, '$1').replace(/''/g, "'");
      target = this.wb.sheets.find((s) => s.name.toLowerCase() === name.toLowerCase()) || null;
      t = t.slice(bang + 1);
    }
    const r = parseRange(t);
    if (!r || !target) {
      this.ui.toast.error(`“${text}” is not a cell or range`);
      this.updateNameBox();
      return;
    }
    if (target !== this.sheet) this.setSheet(target.id);
    const r1 = Math.min(r.r1, this.sheet.rows - 1);
    const c1 = Math.min(r.c1, this.sheet.cols - 1);
    this.selectRange(r.r0, r.c0, r1, c1);
    this.focusGrid();
  }

  focusGrid() {
    this.cellEditor.focusIdle();
  }

  onScroll() {
    if (this.cellEditor.open && this.editTarget?.sheetId === this.sheet.id) this.placeEditor();
    else if (!this.cellEditor.open) this.cellEditor.idlePlace(this.grid.cellRect(this.sel.ar, this.sel.ac));
  }

  /* ========================================================== pointer input */

  onGridPointer(phase, hit, e) {
    const sh = this.sheet;
    if (phase === 'down') closePopover();
    // point mode: clicking cells while typing a formula inserts references
    if (this.cellEditor.open && (phase !== 'down' || this.cellEditor.canPoint() || this.pointing)) {
      if (phase === 'down' && hit.area !== 'cell') return;
      if (phase === 'down') {
        e.preventDefault?.();
        this.pointing = { row: hit.row, col: hit.col };
      }
      if (!this.pointing) return;
      const r = normRange({ r0: this.pointing.row, c0: this.pointing.col, r1: hit.row, c1: hit.col });
      this.cellEditor.pointRef(this.refText(r));
      if (phase === 'up') this.pointing = null;
      return;
    }
    if (phase === 'down' && this.cellEditor.open) this.commitEdit(this.cellEditor.value, null);
    if (phase === 'down') {
      this.focusGrid();
      const shift = e.shiftKey;
      const a = this.sel.anchor;
      if (hit.area === 'corner') {
        this.selectRange(0, 0, sh.rows - 1, sh.cols - 1, 0, 0, false);
        this.dragSel = null;
        return;
      }
      if (hit.area === 'colhead') {
        const c0 = shift ? a.col : hit.col;
        this.setSelection({ r0: 0, r1: sh.rows - 1, c0: Math.min(c0, hit.col), c1: Math.max(c0, hit.col), ar: 0, ac: c0, anchor: { row: 0, col: c0 }, head: { row: 0, col: hit.col } }, { scroll: false });
        this.dragSel = { area: 'colhead', col: c0 };
        return;
      }
      if (hit.area === 'rowhead') {
        const r0 = shift ? a.row : hit.row;
        this.setSelection({ r0: Math.min(r0, hit.row), r1: Math.max(r0, hit.row), c0: 0, c1: sh.cols - 1, ar: r0, ac: 0, anchor: { row: r0, col: 0 }, head: { row: hit.row, col: 0 } }, { scroll: false });
        this.dragSel = { area: 'rowhead', row: r0 };
        return;
      }
      if (shift) {
        this.setSelection({ ...normRange({ r0: a.row, c0: a.col, r1: hit.row, c1: hit.col }), ar: this.sel.ar, ac: this.sel.ac, anchor: a, head: { row: hit.row, col: hit.col } }, { scroll: false });
      } else this.setSelection({ r0: hit.row, c0: hit.col, r1: hit.row, c1: hit.col, ar: hit.row, ac: hit.col, anchor: { row: hit.row, col: hit.col }, head: { row: hit.row, col: hit.col } }, { scroll: false });
      this.dragSel = { area: 'cell', row: this.sel.anchor.row, col: this.sel.anchor.col };
      return;
    }
    if (phase === 'move' && this.dragSel) {
      const d = this.dragSel;
      if (d.area === 'colhead') {
        this.setSelection({ ...this.sel, c0: Math.min(d.col, hit.col), c1: Math.max(d.col, hit.col), head: { row: 0, col: hit.col } }, { scroll: false });
      } else if (d.area === 'rowhead') {
        this.setSelection({ ...this.sel, r0: Math.min(d.row, hit.row), r1: Math.max(d.row, hit.row), head: { row: hit.row, col: 0 } }, { scroll: false });
      } else {
        this.setSelection({ ...this.sel, ...normRange({ r0: d.row, c0: d.col, r1: hit.row, c1: hit.col }), head: { row: hit.row, col: hit.col } }, { scroll: false });
      }
    }
    if (phase === 'up') this.dragSel = null;
  }

  /** Reference text for pointing: "B2", "B2:C5", with sheet prefix when pointing into another sheet. */
  refText(r) {
    const text = rangeName(r);
    if (this.editTarget && this.editTarget.sheetId !== this.sheet.id) return `${quoteSheet(this.sheet.name)}!${text}`;
    return text;
  }

  pointKey(dr, dc, extend) {
    if (!this.pointCursor || !this.cellEditor.pointSpan) {
      const base = this.editTarget && this.editTarget.sheetId === this.sheet.id ? { row: this.editTarget.row, col: this.editTarget.col } : { row: this.sel.ar, col: this.sel.ac };
      this.pointCursor = { anchor: { ...base }, head: { ...base } };
    }
    const pc = this.pointCursor;
    pc.head.row = Math.max(0, Math.min(this.sheet.rows - 1, pc.head.row + dr));
    pc.head.col = Math.max(0, Math.min(this.sheet.cols - 1, pc.head.col + dc));
    if (!extend) pc.anchor = { ...pc.head };
    const r = normRange({ r0: pc.anchor.row, c0: pc.anchor.col, r1: pc.head.row, c1: pc.head.col });
    this.cellEditor.pointRef(this.refText(r));
    this.grid.scrollIntoView(pc.head.row, pc.head.col);
  }

  onContextMenu(hit, e) {
    if (this.cellEditor.open) this.commitEdit(this.cellEditor.value, null);
    const sh = this.sheet;
    const s = this.sel;
    // right-click outside the selection selects the clicked cell/row/column
    if (hit.area === 'cell' && !inRange(s, hit.row, hit.col)) this.selectCell(hit.row, hit.col);
    if (hit.area === 'colhead' && !(s.r0 === 0 && s.r1 >= sh.rows - 1 && hit.col >= s.c0 && hit.col <= s.c1)) {
      this.setSelection({ r0: 0, r1: sh.rows - 1, c0: hit.col, c1: hit.col, ar: 0, ac: hit.col, anchor: { row: 0, col: hit.col }, head: { row: 0, col: hit.col } }, { scroll: false });
    }
    if (hit.area === 'rowhead' && !(s.c0 === 0 && s.c1 >= sh.cols - 1 && hit.row >= s.r0 && hit.row <= s.r1)) {
      this.setSelection({ r0: hit.row, r1: hit.row, c0: 0, c1: sh.cols - 1, ar: hit.row, ac: 0, anchor: { row: hit.row, col: 0 }, head: { row: hit.row, col: 0 } }, { scroll: false });
    }
    const sel = this.sel;
    const nr = sel.r1 - sel.r0 + 1;
    const nc = sel.c1 - sel.c0 + 1;
    const rowsWord = nr === 1 ? 'row' : `${formatNumber(nr)} rows`;
    const colsWord = nc === 1 ? 'column' : `${formatNumber(nc)} columns`;
    const items = [
      { label: 'Cut', icon: 'x', shortcut: 'Ctrl+X', onSelect: () => this.clipboardAction('cut') },
      { label: 'Copy', icon: 'copy', shortcut: 'Ctrl+C', onSelect: () => this.clipboardAction('copy') },
      { label: 'Paste', icon: 'file-text', shortcut: 'Ctrl+V', onSelect: () => this.clipboardAction('paste') },
      { label: 'Paste values only', icon: 'hash', shortcut: 'Ctrl+Shift+V', onSelect: () => this.clipboardAction('paste-values') },
      { separator: true },
    ];
    if (hit.area !== 'colhead') {
      items.push(
        { label: `Insert ${rowsWord} above`, icon: 'plus', onSelect: () => this.insertAxis('row', sel.r0, nr) },
        { label: `Insert ${rowsWord} below`, icon: 'plus', onSelect: () => this.insertAxis('row', sel.r1 + 1, nr) },
      );
    }
    if (hit.area !== 'rowhead') {
      items.push(
        { label: `Insert ${colsWord} left`, icon: 'plus', onSelect: () => this.insertAxis('col', sel.c0, nc) },
        { label: `Insert ${colsWord} right`, icon: 'plus', onSelect: () => this.insertAxis('col', sel.c1 + 1, nc) },
      );
    }
    items.push({ separator: true });
    if (hit.area !== 'colhead') items.push({ label: `Delete ${rowsWord}`, icon: 'trash', onSelect: () => this.deleteAxis('row', sel.r0, nr) });
    if (hit.area !== 'rowhead') items.push({ label: `Delete ${colsWord}`, icon: 'trash', onSelect: () => this.deleteAxis('col', sel.c0, nc) });
    items.push(
      { separator: true },
      { label: hit.area === 'colhead' ? `Sort sheet A → Z by ${colName(sel.ac)}` : 'Sort A → Z', icon: 'sort', onSelect: () => this.sort(false) },
      { label: hit.area === 'colhead' ? `Sort sheet Z → A by ${colName(sel.ac)}` : 'Sort Z → A', icon: 'sort', onSelect: () => this.sort(true) },
      { separator: true },
      { label: 'Clear contents', icon: 'x-circle', shortcut: 'Del', onSelect: () => this.clearContents() },
      { label: 'Clear formatting', icon: 'ban', shortcut: 'Ctrl+\\', onSelect: () => this.clearFormatting() },
    );
    if (hit.area === 'rowhead') items.push({ separator: true }, { label: `Freeze up to row ${sel.r1 + 1}`, icon: 'pin', disabled: sel.r1 + 1 > 1000, onSelect: () => this.updateSheetMeta({ frozen_rows: sel.r1 + 1 }) });
    if (hit.area === 'colhead') items.push({ separator: true }, { label: `Freeze up to column ${colName(sel.c1)}`, icon: 'pin', disabled: sel.c1 + 1 > 1000, onSelect: () => this.updateSheetMeta({ frozen_cols: sel.c1 + 1 }) });
    this.ui.contextMenu(e, this.refocusing(items));
  }

  /* ============================================================== keyboard */

  onKey(e) {
    if (this.cellEditor.open || e.isComposing || e.key === 'Process') return;
    const mod = e.ctrlKey || e.metaKey;
    const k = e.key;
    const sh = this.sheet;
    const s = this.sel;
    const handled = () => { e.preventDefault(); e.stopPropagation(); };
    if (mod && !e.altKey) {
      switch (k.toLowerCase()) {
        case 'z': handled(); if (e.shiftKey) this.redo(); else this.undo(); return;
        case 'y': handled(); this.redo(); return;
        case 'b': handled(); this.toggleStyle('b'); return;
        case 'i': handled(); this.toggleStyle('i'); return;
        case 'u': handled(); this.toggleStyle('u'); return;
        case 'a': handled(); this.selectRange(0, 0, sh.rows - 1, sh.cols - 1, s.ar, s.ac, false); return;
        case 'f': handled(); this.openFind(); return;
        case 's': handled(); this.ui.toast.info('Changes are saved automatically', { timeout: 2000 }); return;
        case '\\': handled(); this.clearFormatting(); return;
        case 'enter': handled(); return;
        case ' ': handled(); this.setSelection({ ...s, r0: 0, r1: sh.rows - 1 }, { scroll: false }); return;
        case 'v':
          if (e.shiftKey) { handled(); this.clipboardAction('paste-values'); return; }
          this.expectClipboard('paste');
          return;
        case 'c': this.expectClipboard('copy'); return;
        case 'x': this.expectClipboard('cut'); return;
        case 'home': handled(); this.selectCell(0, 0); return;
        case 'end': {
          handled();
          const ext = sh.contentExtent();
          this.selectCell(ext ? ext.r1 : 0, ext ? ext.c1 : 0);
          return;
        }
        default: break;
      }
    }
    if (e.altKey && e.shiftKey && (k === '5' || k === '%')) { handled(); this.toggleStyle('strike'); return; }
    if (e.shiftKey && k === ' ' && !mod) { handled(); this.setSelection({ ...s, c0: 0, c1: sh.cols - 1 }, { scroll: false }); return; }
    const arrows = { ArrowUp: [-1, 0], ArrowDown: [1, 0], ArrowLeft: [0, -1], ArrowRight: [0, 1] };
    if (arrows[k]) {
      handled();
      this.move(arrows[k][0], arrows[k][1], e.shiftKey, mod);
      return;
    }
    switch (k) {
      case 'Enter':
        handled();
        if (e.altKey) return;
        if (rangeArea(s) > 1 && !e.altKey) this.cycle(e.shiftKey ? -1 : 1, true);
        else if (e.shiftKey) this.move(-1, 0, false, false);
        else this.startEdit('edit');
        return;
      case 'Tab':
        handled();
        if (rangeArea(s) > 1) this.cycle(e.shiftKey ? -1 : 1, false);
        else this.move(0, e.shiftKey ? -1 : 1, false, false);
        return;
      case 'F2': handled(); this.startEdit('edit'); return;
      case 'Delete': case 'Backspace': handled(); this.clearContents(); return;
      case 'Escape':
        if (this.clip) { handled(); this.clip = null; this.grid.setCopyRange(null); }
        return;
      case 'Home': handled(); this.move(0, -s.head.col, e.shiftKey, false); return;
      case 'End': handled(); this.move(0, sh.cols - 1 - s.head.col, e.shiftKey, false); return;
      case 'PageDown': handled(); this.move(e.altKey ? 0 : this.grid.pageRows(), 0, e.shiftKey, false); return;
      case 'PageUp': handled(); this.move(e.altKey ? 0 : -this.grid.pageRows(), 0, e.shiftKey, false); return;
      default: break;
    }
    if (!mod && !e.altKey && k.length === 1) {
      handled();
      this.startEdit('enter', k);
    }
  }

  /** Move the active cell (or the selection head with extend), with Ctrl jumping to data edges. */
  move(dr, dc, extend, jump) {
    const sh = this.sheet;
    const s = this.sel;
    const from = extend ? s.head : { row: s.ar, col: s.ac };
    let row = from.row;
    let col = from.col;
    if (jump) [row, col] = this.jumpTarget(row, col, Math.sign(dr), Math.sign(dc));
    else {
      row += dr;
      col += dc;
    }
    row = Math.max(0, Math.min(sh.rows - 1, row));
    col = Math.max(0, Math.min(sh.cols - 1, col));
    if (extend) {
      this.setSelection({ ...normRange({ r0: s.anchor.row, c0: s.anchor.col, r1: row, c1: col }), ar: s.ar, ac: s.ac, anchor: s.anchor, head: { row, col } });
    } else this.selectCell(row, col);
  }

  jumpTarget(row, col, dr, dc) {
    const sh = this.sheet;
    const has = (r, c) => {
      const x = sh.get(r, c);
      return !!x && (x.t !== 'z' || !!x.f);
    };
    const inside = (r, c) => r >= 0 && c >= 0 && r < sh.rows && c < sh.cols;
    let r = row + dr;
    let c = col + dc;
    if (!inside(r, c)) return [row, col];
    if (has(row, col) && has(r, c)) {
      while (inside(r + dr, c + dc) && has(r + dr, c + dc)) { r += dr; c += dc; }
      return [r, c];
    }
    // skip blanks to the next value (or the edge)
    const limit = dr ? (dr > 0 ? sh.rows : row + 1) : (dc > 0 ? sh.cols : col + 1);
    if (dr > 0 && sh.cells.size < limit) {
      // sparse fast path: nearest populated row below in this column
      let best = -1;
      for (const k of sh.cells.keys()) {
        if (keyCol(k) === col && keyRow(k) > row && has(keyRow(k), col) && (best < 0 || keyRow(k) < best)) best = keyRow(k);
      }
      return best >= 0 ? [best, col] : [sh.rows - 1, col];
    }
    while (inside(r, c) && !has(r, c)) { r += dr; c += dc; }
    if (!inside(r, c)) return [Math.max(0, Math.min(sh.rows - 1, r - dr)), Math.max(0, Math.min(sh.cols - 1, c - dc))];
    return [r, c];
  }

  /** Enter/Tab inside a multi-cell selection moves the active cell within it. */
  cycle(dir, vertical) {
    const s = this.sel;
    let { ar, ac } = s;
    if (vertical) {
      ar += dir;
      if (ar > s.r1) { ar = s.r0; ac = ac + 1 > s.c1 ? s.c0 : ac + 1; }
      if (ar < s.r0) { ar = s.r1; ac = ac - 1 < s.c0 ? s.c1 : ac - 1; }
    } else {
      ac += dir;
      if (ac > s.c1) { ac = s.c0; ar = ar + 1 > s.r1 ? s.r0 : ar + 1; }
      if (ac < s.c0) { ac = s.c1; ar = ar - 1 < s.r0 ? s.r1 : ar - 1; }
    }
    this.setSelection({ ...s, ar, ac });
    this.grid.scrollIntoView(ar, ac);
  }

  /* =============================================================== editing */

  cellFont(cell) {
    const st = cell?.s;
    const px = st?.size ? Math.round(st.size * 4 / 3) : 13;
    return `${st?.i ? 'italic ' : ''}${st?.b ? 'bold ' : ''}${px}px ${getComputedStyle(this.root).getPropertyValue('--font-sans') || 'system-ui'}`;
  }

  startEdit(mode, text) {
    const s = this.sel;
    const cell = this.activeCell();
    this.editTarget = { sheetId: this.sheet.id, row: s.ar, col: s.ac, original: cellEditText(cell) };
    this.grid.scrollIntoView(s.ar, s.ac);
    this.grid.hideFillHandle = true;
    const rect = this.grid.cellRect(s.ar, s.ac);
    this.cellEditor.start({
      text: text === undefined ? this.editTarget.original : text, mode, rect, font: this.cellFont(cell),
      bg: cell?.s?.bg, color: cell?.s?.color, align: cell?.s?.align,
    });
    this.root.classList.add('is-editing');
  }

  startEditFromBar(typed) {
    if (this.cellEditor.open) return;
    const s = this.sel;
    const cell = this.activeCell();
    this.editTarget = { sheetId: this.sheet.id, row: s.ar, col: s.ac, original: cellEditText(cell) };
    const text = this.formulaInput.value;
    this.grid.hideFillHandle = true;
    this.cellEditor.start({
      text, mode: 'edit', rect: this.grid.cellRect(s.ar, s.ac), font: this.cellFont(cell), viaBar: true,
      bg: cell?.s?.bg, color: cell?.s?.color, align: cell?.s?.align,
    });
    if (!typed) this.cellEditor.setText(text, text.length, this.formulaInput);
    this.root.classList.add('is-editing');
  }

  placeEditor() {
    if (!this.cellEditor.open || !this.editTarget || this.editTarget.sheetId !== this.sheet.id) return;
    this.cellEditor.place(this.grid.cellRect(this.editTarget.row, this.editTarget.col));
  }

  onEditText(text) {
    if (!this.editTarget) return;
    this.pointCursor = this.cellEditor.pointSpan ? this.pointCursor : null;
    if (!text.startsWith('=')) {
      this.grid.setHighlights([]);
      return;
    }
    const own = this.wb.sheet(this.editTarget.sheetId);
    const list = [];
    formulaRefs(text).forEach((ref, i) => {
      const target = ref.sheet ? this.wb.sheets.find((s) => s.name.toLowerCase() === ref.sheet.toLowerCase()) : own;
      if (target === this.sheet) list.push({ range: { ...ref.range, r1: Math.min(ref.range.r1, this.sheet.rows - 1), c1: Math.min(ref.range.c1, this.sheet.cols - 1) }, color: refColor(i) });
    });
    this.grid.setHighlights(list);
  }

  endEditUI() {
    this.cellEditor.close();
    this.grid.setHighlights([]);
    this.grid.hideFillHandle = false;
    this.root.classList.remove('is-editing');
    this.pointing = null;
    this.pointCursor = null;
  }

  cancelEdit() {
    const t = this.editTarget;
    this.endEditUI();
    this.editTarget = null;
    if (t && t.sheetId !== this.sheet.id) this.setSheet(t.sheetId);
    this.updateFormulaBar();
    this.grid.invalidate();
    this.focusGrid();
  }

  commitEdit(text, move, opts = {}) {
    const t = this.editTarget;
    this.endEditUI();
    this.editTarget = null;
    if (!t) return;
    if (t.sheetId !== this.sheet.id) this.setSheet(t.sheetId);
    if (text !== t.original || opts.all) {
      const targets = opts.all && rangeArea(this.sel) > 1 ? this.cellsOf(this.sel) : [[t.row, t.col]];
      const ups = targets.map(([row, col]) => {
        if (text.startsWith('=') && (row !== t.row || col !== t.col)) return { row, col, input: shiftFormula(text, row - t.row, col - t.col) };
        return { row, col, input: text };
      });
      this.edit(ups, 'Edit');
    }
    if (move) {
      // like Excel: Enter after a run of Tab entries returns to the column the run started in
      const tabRun = this.tabRun && this.tabRun.row === t.row ? this.tabRun : null;
      if (rangeArea(this.sel) > 1 && !opts.all) this.cycle(move.dr || move.dc, !!move.dr);
      else if (move.dc && !move.dr) {
        this.move(0, move.dc, false, false);
        this.tabRun = { row: t.row, col: tabRun ? tabRun.col : t.col };
        return this.focusGrid();
      } else if (move.dr === 1 && tabRun) this.selectCell(Math.min(t.row + 1, this.sheet.rows - 1), tabRun.col);
      else this.move(move.dr, move.dc, false, false);
    } else this.updateFormulaBar();
    this.tabRun = null;
    this.focusGrid();
  }

  cellsOf(r) {
    const out = [];
    for (let row = r.r0; row <= r.r1; row++) for (let col = r.c0; col <= r.c1; col++) out.push([row, col]);
    return out;
  }

  /**
   * Apply updates locally, queue them for saving and record undo history.
   * @param {Array<object>} ups internal updates {row, col, ...api fields}
   * @param {string} label
   * @param {{sheet?: any, history?: boolean, rangeStyle?: {range, style}}} [o]
   */
  edit(ups, label, o = {}) {
    const sh = o.sheet || this.sheet;
    if (!ups.length && !o.rangeStyle) return;
    const backward = [];
    const seen = new Set();
    const snap = (row, col) => {
      const k = cellKey(row, col);
      if (seen.has(k)) return;
      seen.add(k);
      backward.push(restoreUpdate(row, col, sh.get(row, col)));
      backward[backward.length - 1].row = row;
      backward[backward.length - 1].col = col;
    };
    for (const u of ups) snap(u.row, u.col);
    let rsKeys = [];
    if (o.rangeStyle) {
      const r = o.rangeStyle.range;
      if (rangeArea(r) <= 400000) {
        for (let row = r.r0; row <= r.r1; row++) for (let col = r.c0; col <= r.c1; col++) snap(row, col);
      }
      rsKeys = this.applyRangeStyleLocal(sh, r, o.rangeStyle.style);
    }
    for (const u of ups) sh.set(u.row, u.col, localApply(sh.get(u.row, u.col), u));
    if (ups.length) {
      const api = ups.map(toApi);
      for (let i = 0; i < api.length; i += 100000) {
        this.saver.cells(sh.id, api.slice(i, i + 100000), ups.slice(i, i + 100000).map((u) => cellKey(u.row, u.col)));
      }
    }
    if (o.rangeStyle) this.saver.rangeStyle(sh.id, rangeName(o.rangeStyle.range), o.rangeStyle.style, rsKeys);
    if (o.history !== false) {
      this.history.push({ kind: 'cells', label, sheetId: sh.id, forward: ups, rangeStyle: o.rangeStyle, backward: backward.reverse() });
    }
    this.afterLocalChange();
  }

  applyRangeStyleLocal(sh, r, style) {
    const keys = [];
    for (let row = r.r0; row <= r.r1; row++) {
      for (let col = r.c0; col <= r.c1; col++) {
        sh.set(row, col, localApply(sh.get(row, col), { style }));
        keys.push(cellKey(row, col));
      }
    }
    return keys;
  }

  afterLocalChange() {
    this.grid.invalidate();
    this.updateFormulaBar();
    this.updateToolbarState();
    this.scheduleStats();
  }

  /* ================================================================ saving */

  onSaved(res, op) {
    if (res && res.changed) {
      this.wb.applyChanged(res.changed, (sid, key) => this.saver.isPending(sid, key));
      this.grid.invalidate();
      this.updateFormulaBar();
      this.scheduleStats();
    }
    void op;
  }

  onSaveFailed(err, op) {
    if (err?.code === 'recalc_timeout') {
      this.ui.toast.error('That change made the workbook too slow to recalculate, so it was not saved.', { description: 'Try a smaller range or simpler formulas.' });
    } else this.ui.toast.error(err, { description: 'The change was not saved; showing the last saved version.' });
    void op;
    this.reloadWorkbook();
  }

  setSaveState(state, detail) {
    const map = {
      pending: ['Saving…', 'is-saving'], saving: ['Saving…', 'is-saving'], saved: ['All changes saved', 'is-saved'],
      offline: [`Offline — retrying${detail?.retryIn ? ` in ${Math.round(detail.retryIn / 1000)}s` : ''}`, 'is-offline'],
    };
    const [text, c] = map[state] || map.saved;
    this.saveState.className = `sh-save ${c}`;
    this.saveState.replaceChildren(icon(state === 'saved' ? 'check' : state === 'offline' ? 'alert' : 'refresh', { size: 13 }), h('span', {}, text));
  }

  showExternalChange() {
    if (this.external) return;
    this.external = true;
    this.banner.replaceChildren(
      icon('info', 16), h('span', {}, 'This workbook was changed somewhere else (another tab, device or script).'),
      this.ui.button({ label: 'Reload', size: 'sm', icon: 'refresh', onClick: () => this.reloadWorkbook() }));
    this.banner.hidden = false;
  }

  /* ============================================================ formatting */

  /** The selection clipped to the grid (whole rows/columns included). */
  selRange() {
    const s = this.sel;
    return { r0: s.r0, c0: s.c0, r1: Math.min(s.r1, this.sheet.rows - 1), c1: Math.min(s.c1, this.sheet.cols - 1) };
  }

  applyStyle(patch) {
    const r = this.selRange();
    if (rangeArea(r) > 200000) {
      this.ui.toast.error('Formatting is limited to 200,000 cells at a time. Select a smaller range.');
      return;
    }
    if (rangeArea(r) === 1) this.edit([{ row: r.r0, col: r.c0, style: patch }], 'Format');
    else this.edit([], 'Format', { rangeStyle: { range: r, style: patch } });
    this.focusGrid();
  }

  toggleStyle(key) {
    const on = !this.activeStyle()?.[key];
    this.applyStyle({ [key]: on });
  }

  adjustDecimals(delta) {
    const cell = this.activeCell();
    let fmt = cell?.s?.fmt || '';
    if (!fmt || isDateFormat(fmt)) {
      const v = cell?.t === 'n' ? Number(cell.v) : 0;
      const dec = Math.min(10, (String(v).split('.')[1] || '').length);
      fmt = dec ? `0.${'0'.repeat(dec)}` : '0';
    }
    const m = /(0|#)(\.([0#]+))?(?![^"]*")/.exec(fmt);
    if (!m) return;
    const decs = (m[3] || '').length;
    const next = Math.max(0, Math.min(10, decs + delta));
    const repl = next ? `${m[1]}.${'0'.repeat(next)}` : m[1];
    this.applyStyle({ fmt: fmt.replace(/(0|#)(\.([0#]+))?/g, (all, a, b, c, off) => (off === m.index ? repl : all)) });
  }

  borders(kind) {
    const r = this.selRange();
    if (rangeArea(r) > 200000) {
      this.ui.toast.error('Select a smaller range (at most 200,000 cells).');
      return;
    }
    const all = { t: true, b: true, l: true, r: true };
    const none = { t: false, b: false, l: false, r: false };
    if (kind === 'all' || kind === 'none') {
      this.edit([], 'Borders', { rangeStyle: { range: r, style: { border: kind === 'all' ? all : none } } });
      return;
    }
    const ups = [];
    for (let row = r.r0; row <= r.r1; row++) {
      for (let col = r.c0; col <= r.c1; col++) {
        const b = {};
        if ((kind === 'outer' || kind === 't') && row === r.r0) b.t = true;
        if ((kind === 'outer' || kind === 'b') && row === r.r1) b.b = true;
        if ((kind === 'outer' || kind === 'l') && col === r.c0) b.l = true;
        if ((kind === 'outer' || kind === 'r') && col === r.c1) b.r = true;
        if (Object.keys(b).length) ups.push({ row, col, style: { border: b } });
      }
    }
    this.edit(ups, 'Borders');
  }

  clearFormatting() {
    const ups = [];
    this.sheet.eachIn(this.selRange(), (row, col, cell) => { if (cell.s) ups.push({ row, col, clear: 'style' }); });
    if (ups.length) this.edit(ups, 'Clear formatting');
  }

  clearContents() {
    const ups = [];
    this.sheet.eachIn(this.selRange(), (row, col, cell) => { if (cell.t !== 'z' || cell.f) ups.push({ row, col, clear: 'content' }); });
    if (ups.length) this.edit(ups, 'Clear');
  }

  /* ============================================================ functions */

  async browseFunctions() {
    await this.loadFunctions();
    if (!functionsCache) {
      this.ui.toast.error('Could not load the function list.');
      return;
    }
    const name = await functionsDialog(this.ctx, functionsCache);
    if (name) this.startEdit('edit', `=${name}(`);
    else this.focusGrid();
  }

  /** AutoSum-style insertion of SUM/AVERAGE/… */
  autoFunction(fn) {
    const sh = this.sheet;
    const s = this.selRange();
    const num = (r, c) => sh.get(r, c)?.t === 'n';
    if (rangeArea(s) > 1 && s.r1 + 1 < sh.rows) {
      const ups = [];
      for (let c = s.c0; c <= s.c1; c++) ups.push({ row: s.r1 + 1, col: c, input: `=${fn}(${refName(s.r0, c)}:${refName(s.r1, c)})` });
      this.edit(ups, fn);
      this.selectCell(s.r1 + 1, s.c0);
      return;
    }
    const { ar, ac } = this.sel;
    let guess = '';
    let r = ar - 1;
    while (r >= 0 && num(r, ac)) r--;
    if (r < ar - 1) guess = `${refName(r + 1, ac)}:${refName(ar - 1, ac)}`;
    else {
      let c = ac - 1;
      while (c >= 0 && num(ar, c)) c--;
      if (c < ac - 1) guess = `${refName(ar, c + 1)}:${refName(ar, ac - 1)}`;
    }
    this.startEdit('edit', `=${fn}(${guess})`);
  }

  /* ========================================================= clipboard */

  copyPayload(r) {
    return clipboardPayload(this.sheet, r);
  }

  /** Native copy/cut/paste events follow the shortcut's keydown synchronously; when the browser
   *  does not run them (synthetic input, missing selection), fall back to the async Clipboard API. */
  expectClipboard(kind) {
    this.clipPending = kind;
    setTimeout(() => {
      if (this.clipPending !== kind) return;
      this.clipPending = null;
      this.clipboardAction(kind);
    }, 0);
  }

  onCopy(e, cut) {
    this.clipPending = null;
    if (this.cellEditor.open) return;
    const r = this.selRange();
    if (rangeArea(r) > 200000) {
      e.preventDefault();
      this.ui.toast.error('Copying is limited to 200,000 cells.');
      return;
    }
    const p = this.copyPayload(r);
    e.clipboardData.setData('text/plain', p.text);
    e.preventDefault();
    this.clip = { range: r, sheetId: this.sheet.id, cells: p.cells, text: p.text, cut, mark: CLIP_MARK };
    this.grid.setCopyRange(r);
  }

  onPaste(e) {
    this.clipPending = null;
    if (this.cellEditor.open) return;
    e.preventDefault();
    const text = e.clipboardData.getData('text/plain');
    this.paste(text, false);
  }

  async clipboardAction(kind) {
    const r = this.selRange();
    if (kind === 'copy' || kind === 'cut') {
      const p = this.copyPayload(r);
      this.clip = { range: r, sheetId: this.sheet.id, cells: p.cells, text: p.text, cut: kind === 'cut' };
      this.grid.setCopyRange(r);
      const ok = await this.ui.copyText(p.text, { silent: true });
      if (!ok) this.ui.toast.info('Use Ctrl+C / Ctrl+V to copy and paste.');
      this.focusGrid();
      return;
    }
    let text = null;
    try {
      if (navigator.clipboard?.readText && window.isSecureContext) text = await navigator.clipboard.readText();
    } catch {
      text = null;
    }
    if (text === null) {
      if (this.clip) text = this.clip.text;
      else {
        this.ui.toast.info('Use Ctrl+V to paste from other apps.');
        return;
      }
    }
    this.paste(text, kind === 'paste-values');
    this.focusGrid();
  }

  /** Paste TSV text; our own copies keep formulas (shifted) and styles. */
  paste(text, valuesOnly) {
    const sh = this.sheet;
    const at = { row: this.sel.r0, col: this.sel.c0 };
    const own = this.clip && this.clip.text === text ? this.clip : null;
    const ups = [];
    if (own) {
      const src = own.range;
      const h0 = src.r1 - src.r0 + 1;
      const w0 = src.c1 - src.c0 + 1;
      // a selection that is a multiple of the copied block is tiled
      const selH = this.sel.r1 - this.sel.r0 + 1;
      const selW = this.sel.c1 - this.sel.c0 + 1;
      const reps = { r: selH % h0 === 0 && !own.cut ? selH / h0 : 1, c: selW % w0 === 0 && !own.cut ? selW / w0 : 1 };
      for (let rr = 0; rr < reps.r; rr++) {
        for (let cc = 0; cc < reps.c; cc++) {
          for (let i = 0; i < h0; i++) {
            for (let j = 0; j < w0; j++) {
              const row = at.row + rr * h0 + i;
              const col = at.col + cc * w0 + j;
              if (row >= MAX_ROWS) continue;
              const c = own.cells[i][j];
              const u = { row, col, clear: valuesOnly ? 'content' : 'all' };
              if (c) {
                if (c.f && !valuesOnly) u.formula = own.cut ? c.f : shiftFormula(c.f, row - (src.r0 + i), col - (src.c0 + j));
                else if (c.t === 'e') u.input = String(c.v);
                else if (c.t !== 'z') u.value = c.v;
                if (c.s && !valuesOnly) u.style = { ...c.s };
              }
              ups.push(u);
            }
          }
        }
      }
      if (own.cut && own.sheetId === sh.id) {
        const dst = { r0: at.row, c0: at.col, r1: at.row + h0 - 1, c1: at.col + w0 - 1 };
        for (let i = 0; i < h0; i++) {
          for (let j = 0; j < w0; j++) {
            const row = src.r0 + i;
            const col = src.c0 + j;
            if (!inRange(dst, row, col) && sh.get(row, col)) ups.unshift({ row, col, clear: 'all' });
          }
        }
      } else if (own.cut) {
        const srcSheet = this.wb.sheet(own.sheetId);
        if (srcSheet) {
          const clears = [];
          srcSheet.eachIn(src, (row, col) => clears.push({ row, col, clear: 'all' }));
          if (clears.length) this.edit(clears, 'Cut', { sheet: srcSheet });
        }
      }
      if (own.cut) {
        this.clip = null;
        this.grid.setCopyRange(null);
      }
      this.edit(ups, 'Paste');
      this.selectRange(at.row, at.col, at.row + h0 * reps.r - 1, at.col + w0 * reps.c - 1, at.row, at.col);
      return;
    }
    const rows = parseTSV(text);
    if (!rows.length) return;
    if (rows.length * rows[0].length > 100000) {
      this.ui.toast.error('Pasting is limited to 100,000 cells at a time.');
      return;
    }
    rows.forEach((line, i) => line.forEach((v, j) => {
      const row = at.row + i;
      const col = at.col + j;
      if (row < MAX_ROWS) ups.push(valuesOnly && v.startsWith('=') ? { row, col, value: v } : { row, col, input: v });
    }));
    this.edit(ups, 'Paste');
    this.selectRange(at.row, at.col, at.row + rows.length - 1, at.col + rows[0].length - 1, at.row, at.col);
  }

  /* ============================================================ fill handle */

  fill(target) {
    const sh = this.sheet;
    const src = this.selRange();
    if (rangeArea(target) > 100000) {
      this.ui.toast.error('Fills are limited to 100,000 cells.');
      return;
    }
    const out = computeFill((r, c) => sh.get(r, c), src, target);
    const ups = out.map(({ row, col, cell }) => {
      const u = { row, col, clear: 'all' };
      if (cell) {
        if (cell.f) u.formula = cell.f;
        else if (cell.t === 'e') u.input = String(cell.v);
        else if (cell.t && cell.t !== 'z') u.value = cell.v;
        if (cell.s) u.style = { ...cell.s };
      }
      return u;
    });
    this.edit(ups, 'Fill');
    this.selectRange(target.r0, target.c0, target.r1, target.c1, this.sel.ar, this.sel.ac, false);
  }

  /* ======================================================== rows & columns */

  async insertAxis(axis, at, count, { history = true } = {}) {
    const sh = this.sheet;
    try {
      const res = await this.saver.call(() => this.api.post(`${this.base}/sheets/${sh.id}/insert`, { axis, at, count }));
      this.wb.replaceSheet(res);
      this.afterStructure(sh);
      if (history) this.history.push({ kind: 'axis', op: 'insert', sheetId: sh.id, axis, at, count });
      this.updateToolbarState();
    } catch (err) {
      this.ui.toast.error(err);
    }
  }

  async deleteAxis(axis, at, count, { history = true } = {}) {
    const sh = this.sheet;
    // snapshot what an undo must restore: the deleted band and formulas that may turn into #REF!
    const restore = [];
    if (history) {
      for (const [k, c] of sh.cells) {
        const row = keyRow(k);
        const col = keyCol(k);
        const inBand = axis === 'row' ? row >= at && row < at + count : col >= at && col < at + count;
        if (inBand || c.f) restore.push({ row, col, cell: c, inBand });
      }
      if (restore.length > 200000) {
        const ok = await this.ui.confirm({ title: 'Delete without undo?', message: 'This sheet is too large to undo deleting rows or columns. Continue?', confirmLabel: 'Delete', danger: true });
        if (!ok) return;
        restore.length = 0;
      }
    }
    const others = history ? this.wb.sheets.filter((s) => s !== sh).flatMap((s) => [...s.cells].filter(([, c]) => c.f && c.f.includes('!')).map(([k, c]) => ({ sheet: s, k, c }))) : [];
    try {
      const res = await this.saver.call(() => this.api.post(`${this.base}/sheets/${sh.id}/delete`, { axis, at, count }));
      this.wb.replaceSheet(res);
      this.afterStructure(sh);
      if (history && restore.length) this.history.push({ kind: 'axis', op: 'delete', sheetId: sh.id, axis, at, count, restore, others });
      this.updateToolbarState();
    } catch (err) {
      this.ui.toast.error(err);
    }
  }

  afterStructure(sh) {
    for (const o of this.wb.sheets) if (o !== sh) this.staleSheets.add(o.id);
    if (sh === this.sheet) {
      this.grid.setSheet(sh);
      this.setSelection(this.sel, { scroll: false });
    }
  }

  async sort(desc) {
    const sh = this.sheet;
    const s = this.selRange();
    let ref;
    const byCol = this.sel.ac;
    if (s.r1 > s.r0 && s.c1 > s.c0 && !(s.r0 === 0 && s.r1 >= sh.rows - 1)) ref = s;
    else {
      const ext = sh.contentExtent();
      if (!ext || ext.r1 < sh.frozenRows) return;
      ref = { r0: sh.frozenRows, c0: 0, r1: ext.r1, c1: Math.max(ext.c1, byCol) };
    }
    if (rangeArea(ref) > 2000000) {
      this.ui.toast.error('That range is too large to sort.');
      return;
    }
    const backward = [];
    sh.eachIn(ref, (row, col, cell) => { backward.push({ ...restoreUpdate(row, col, cell), row, col }); });
    const before = new Set(backward.map((u) => cellKey(u.row, u.col)));
    try {
      const res = await this.saver.call(() => this.api.post(`${this.base}/sheets/${sh.id}/sort`, { ref: rangeName(ref), by_col: colName(byCol), desc }));
      this.wb.replaceSheet(res);
      // cells that became empty must be cleared on undo too
      sh.eachIn(ref, (row, col) => { if (!before.has(cellKey(row, col))) backward.push({ row, col, clear: 'all' }); });
      this.afterStructure(sh);
      if (backward.length <= 100000) this.history.push({ kind: 'sort', sheetId: sh.id, ref, byCol, desc, backward });
      this.updateToolbarState();
      this.ui.toast.success(`Sorted ${rangeName(ref)} by column ${colName(byCol)} (${desc ? 'Z → A' : 'A → Z'})`, { timeout: 2500 });
    } catch (err) {
      this.ui.toast.error(err);
    }
  }

  resized(axis, index, size, orig, { history = true } = {}) {
    const sh = this.sheet;
    const ax = axis === 'col' ? sh.colAxis : sh.rowAxis;
    ax.setSize(index, size);
    this.grid.refresh();
    this.placeEditor();
    const key = axis === 'col' ? colName(index) : String(index + 1);
    const body = axis === 'col' ? { col_widths: { [key]: size } } : { row_heights: { [key]: size } };
    if (history) this.history.push({ kind: 'size', sheetId: sh.id, axis, index, before: orig, after: size });
    this.updateToolbarState();
    this.saver.call(() => this.api.patch(`${this.base}/sheets/${sh.id}`, body)).catch((err) => {
      this.ui.toast.error(err);
      this.reloadWorkbook();
    });
  }

  autoFit(axis, index) {
    const sh = this.sheet;
    if (axis !== 'col') {
      this.resized('row', index, DEFAULT_ROW_H, sh.rowAxis.size(index));
      return;
    }
    let width = 0;
    let n = 0;
    for (const [k, c] of sh.cells) {
      if (keyCol(k) !== index || c.t === 'z') continue;
      width = Math.max(width, this.grid.measure(this.grid.fontOf(c.s), (sh.display(c) || '').split('\n')[0]));
      if (++n > 5000) break;
    }
    const target = Math.max(32, Math.min(800, Math.ceil(width + 14)));
    this.resized('col', index, n ? target : DEFAULT_COL_W, sh.colAxis.size(index));
  }

  /* ============================================================ undo / redo */

  async undo() {
    const e = this.history.undo();
    if (!e) return;
    await this.applyHistory(e, true);
  }

  async redo() {
    const e = this.history.redo();
    if (!e) return;
    await this.applyHistory(e, false);
  }

  async applyHistory(e, back) {
    const sh = this.wb.sheet(e.sheetId);
    if (!sh) return;
    if (sh !== this.sheet) this.setSheet(sh.id);
    switch (e.kind) {
      case 'cells': {
        if (back) this.edit(e.backward, e.label, { history: false });
        else this.edit(e.forward, e.label, { history: false, rangeStyle: e.rangeStyle });
        const all = back ? e.backward : e.forward;
        if (all.length) {
          const rows = all.map((u) => u.row);
          const cols = all.map((u) => u.col);
          this.selectRange(Math.min(...rows.slice(0, 5000)), Math.min(...cols.slice(0, 5000)), Math.max(...rows.slice(0, 5000)), Math.max(...cols.slice(0, 5000)));
        } else if (e.rangeStyle) {
          const r = e.rangeStyle.range;
          this.selectRange(r.r0, r.c0, r.r1, r.c1);
        }
        break;
      }
      case 'axis': {
        const inverse = (e.op === 'insert') === back ? 'delete' : 'insert';
        if (inverse === 'delete') await this.deleteAxis(e.axis, e.at, e.count, { history: false });
        else {
          await this.insertAxis(e.axis, e.at, e.count, { history: false });
          if (e.op === 'delete' && back) this.restoreAfterUndoDelete(e);
        }
        break;
      }
      case 'sort':
        if (back) this.edit(e.backward, 'Undo sort', { history: false });
        else {
          await this.saver.call(() => this.api.post(`${this.base}/sheets/${sh.id}/sort`, { ref: rangeName(e.ref), by_col: colName(e.byCol), desc: e.desc }))
            .then((res) => { this.wb.replaceSheet(res); this.afterStructure(sh); })
            .catch((err) => this.ui.toast.error(err));
        }
        break;
      case 'size':
        this.resized(e.axis, e.index, back ? e.before : e.after, back ? e.after : e.before, { history: false });
        break;
      case 'meta':
        await this.updateSheetMeta(back ? e.backward : e.forward, { history: false });
        break;
      default: break;
    }
    this.updateToolbarState();
  }

  restoreAfterUndoDelete(e) {
    const sh = this.wb.sheet(e.sheetId);
    const ups = [];
    for (const { row, col, cell, inBand } of e.restore) {
      const cur = sh.get(row, col);
      if (inBand || (cell.f && cur?.f !== cell.f)) ups.push({ ...restoreUpdate(row, col, cell), row, col });
    }
    if (ups.length) this.edit(ups, 'Restore', { history: false });
    const bySheet = new Map();
    for (const { sheet, k, c } of e.others || []) {
      const cur = sheet.cells.get(k);
      if (cur?.f === c.f) continue;
      if (!bySheet.has(sheet)) bySheet.set(sheet, []);
      bySheet.get(sheet).push({ ...restoreUpdate(keyRow(k), keyCol(k), c), row: keyRow(k), col: keyCol(k) });
    }
    for (const [sheet, list] of bySheet) this.edit(list, 'Restore', { history: false, sheet });
  }

  /* ================================================================== find */

  openFind() {
    if (this.findBar) {
      this.findInput.focus();
      this.findInput.select();
      return;
    }
    const { ui } = this;
    this.findInput = h('input', { class: 'input input-sm', type: 'search', placeholder: 'Find in sheet', aria: { label: 'Find in sheet' }, spellcheck: 'false' });
    this.findCount = h('span', { class: 'sh-find-count nums', aria: { live: 'polite' } });
    this.findCase = h('input', { type: 'checkbox', class: 'checkbox', aria: { label: 'Match case' } });
    const run = debounce(() => this.runFind(), 120);
    this.findInput.addEventListener('input', run);
    this.findCase.addEventListener('change', () => this.runFind());
    this.findInput.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') { e.preventDefault(); this.findStep(e.shiftKey ? -1 : 1); }
      if (e.key === 'Escape') { e.preventDefault(); this.closeFind(); }
    });
    this.findBar = h('div', { class: 'sh-findbar', role: 'search' },
      icon('search', { size: 14, className: 'faint' }), this.findInput, this.findCount,
      h('label', { class: 'sh-find-case', dataset: { tip: 'Match case' } }, this.findCase, h('span', {}, 'Aa')),
      ui.button({ icon: 'chevron-up', title: 'Previous (Shift+Enter)', variant: 'ghost', size: 'sm', onClick: () => this.findStep(-1) }),
      ui.button({ icon: 'chevron-down', title: 'Next (Enter)', variant: 'ghost', size: 'sm', onClick: () => this.findStep(1) }),
      ui.button({ icon: 'x', title: 'Close (Esc)', variant: 'ghost', size: 'sm', onClick: () => this.closeFind() }));
    this.gridWrap.append(this.findBar);
    this.findInput.focus();
  }

  runFind() {
    const q = this.findInput.value;
    const sh = this.sheet;
    if (!q) {
      this.findState = null;
      this.grid.setFind(null);
      this.findCount.textContent = '';
      return;
    }
    const cs = this.findCase.checked;
    const needle = cs ? q : q.toLowerCase();
    const hits = [];
    for (const [k, c] of sh.cells) {
      if (c.t === 'z') continue;
      const text = sh.display(c) || '';
      const hay = cs ? text : text.toLowerCase();
      const f = c.f ? (cs ? c.f : c.f.toLowerCase()) : '';
      if (hay.includes(needle) || (f && f.includes(needle))) hits.push(k);
    }
    hits.sort((a, b) => keyRow(a) - keyRow(b) || keyCol(a) - keyCol(b));
    const cur = cellKey(this.sel.ar, this.sel.ac);
    let idx = hits.findIndex((k) => k >= cur);
    if (idx < 0) idx = 0;
    this.findState = { hits, idx, keys: new Set(hits) };
    this.showFindResult();
  }

  findStep(d) {
    if (!this.findState?.hits.length) return;
    const st = this.findState;
    st.idx = (st.idx + d + st.hits.length) % st.hits.length;
    this.showFindResult();
  }

  showFindResult() {
    const st = this.findState;
    if (!st.hits.length) {
      this.findCount.textContent = 'No results';
      this.grid.setFind({ keys: st.keys, current: null });
      return;
    }
    const k = st.hits[st.idx];
    this.findCount.textContent = `${formatNumber(st.idx + 1)} of ${formatNumber(st.hits.length)}`;
    this.grid.setFind({ keys: st.keys, current: k });
    this.selectCell(keyRow(k), keyCol(k));
  }

  closeFind() {
    this.findBar?.remove();
    this.findBar = null;
    this.findState = null;
    this.grid.setFind(null);
    this.focusGrid();
  }

  /* ================================================================ export */

  download(format) {
    const q = { format };
    if (format === 'csv') q.sheet_id = this.sheet.id;
    this.saver.flush();
    const go = () => this.api.download(`${this.base}/export`, q);
    if (this.saver.idle) go();
    else {
      const t = setInterval(() => {
        if (this.saver.idle) { clearInterval(t); go(); }
      }, 100);
      setTimeout(() => clearInterval(t), 15000);
    }
  }
}

export { COL_HEADER_H, parseRef };
