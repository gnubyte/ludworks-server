// Client-side workbook model: sheets with sparse cell maps, geometry and display caching, plus
// helpers turning cell snapshots into API updates (for undo/redo). Pure apart from no DOM use.

import { Axis } from './axis.js';
import { cellKey, keyRow, keyCol, parseCol, colName, refName } from './a1.js';
import { formatValue, isDateFormat, isPercentFormat, serialToText, generalNumber } from './numfmt.js';
import { toTSV } from './tsv.js';

export const DEFAULT_COL_W = 100;
export const DEFAULT_ROW_H = 21;

/** One sheet of a workbook. */
export class SheetModel {
  constructor(json) {
    this.cells = new Map();
    this.update(json);
    this.loaded = false;
    if (json.cells) this.setCells(json.cells);
  }

  /** Apply sheet metadata (the API Sheet/summary shape). */
  update(json) {
    this.id = json.id;
    this.name = json.name;
    this.index = json.index;
    this.rows = json.rows;
    this.cols = json.cols;
    this.frozenRows = json.frozen_rows || 0;
    this.frozenCols = json.frozen_cols || 0;
    const cw = new Map();
    for (const [k, v] of Object.entries(json.col_widths || {})) {
      const c = parseCol(k);
      if (c >= 0) cw.set(c, v);
    }
    const rh = new Map();
    for (const [k, v] of Object.entries(json.row_heights || {})) rh.set(Number(k) - 1, v);
    this.colAxis = new Axis(this.cols, DEFAULT_COL_W, cw);
    this.rowAxis = new Axis(this.rows, DEFAULT_ROW_H, rh);
  }

  /** Replace every cell from an API cell map {"A1": Cell}. */
  setCells(map) {
    this.cells.clear();
    for (const [ref, cell] of Object.entries(map)) {
      const m = /^([A-Z]{1,3})(\d+)$/.exec(ref);
      if (!m) continue;
      this.cells.set(cellKey(Number(m[2]) - 1, parseCol(m[1])), cell);
    }
    this.loaded = true;
  }

  get(row, col) {
    return this.cells.get(cellKey(row, col)) || null;
  }

  /** Store a cell (null or an empty, unstyled cell removes it). Grows the grid if needed. */
  set(row, col, cell) {
    const k = cellKey(row, col);
    if (!cell || (cell.t === 'z' && !cell.s && !cell.f)) this.cells.delete(k);
    else this.cells.set(k, cell);
    if (cell && row >= this.rows) { this.rows = row + 1; this.rowAxis.setCount(this.rows); }
    if (cell && col >= this.cols) { this.cols = col + 1; this.colAxis.setCount(this.cols); }
  }

  /** Display text and colour of a cell (cached on the cell object). */
  display(cell) {
    if (!cell) return null;
    if (cell._d === undefined) {
      const d = formatValue(cell.v, cell.t, cell.s?.fmt || '');
      cell._d = d.text;
      cell._c = d.color || null;
    }
    return cell._d;
  }

  /** Bounding box of cells with content (not style-only), or null. */
  contentExtent() {
    let r1 = -1;
    let c1 = -1;
    for (const [k, c] of this.cells) {
      if (c.t === 'z' && !c.f) continue;
      r1 = Math.max(r1, keyRow(k));
      c1 = Math.max(c1, keyCol(k));
    }
    return r1 < 0 ? null : { r0: 0, c0: 0, r1, c1 };
  }

  /** Iterate stored cells inside a rectangle: fn(row, col, cell). Sparse-aware. */
  eachIn(r, fn) {
    const area = (r.r1 - r.r0 + 1) * (r.c1 - r.c0 + 1);
    if (area <= this.cells.size * 2) {
      for (let row = r.r0; row <= r.r1; row++) {
        for (let col = r.c0; col <= r.c1; col++) {
          const c = this.cells.get(cellKey(row, col));
          if (c) fn(row, col, c);
        }
      }
      return;
    }
    for (const [k, c] of this.cells) {
      const row = keyRow(k);
      const col = keyCol(k);
      if (row >= r.r0 && row <= r.r1 && col >= r.c0 && col <= r.c1) fn(row, col, c);
    }
  }

  /** Sizes JSON for an API patch ({"A": 120} / {"3": 30}). */
  static colKey(c) { return colName(c); }
}

/** The workbook currently open in the editor. */
export class WorkbookModel {
  constructor(json) {
    this.id = json.id;
    this.name = json.name;
    this.version = json.version || 0;
    this.updatedAt = json.updated_at;
    this.sheets = (json.sheets || []).map((s) => new SheetModel(s));
    this.sortSheets();
  }

  sortSheets() {
    this.sheets.sort((a, b) => a.index - b.index);
  }

  sheet(id) {
    return this.sheets.find((s) => s.id === id) || null;
  }

  /** Apply {"<sheet_id>": {"A1": Cell}} from a mutation response; skip(sheetId, key) keeps local cells. */
  applyChanged(changed, skip) {
    const touched = new Set();
    for (const [sid, cells] of Object.entries(changed || {})) {
      const sh = this.sheet(sid);
      if (!sh) continue;
      touched.add(sid);
      for (const [ref, cell] of Object.entries(cells)) {
        const m = /^([A-Z]{1,3})(\d+)$/.exec(ref);
        if (!m) continue;
        const row = Number(m[2]) - 1;
        const col = parseCol(m[1]);
        if (skip && skip(sid, cellKey(row, col))) continue;
        sh.set(row, col, cell.t === 'z' && !cell.s && !cell.f ? null : cell);
      }
    }
    return touched;
  }

  /** Replace a sheet from a full Sheet JSON (structural responses). */
  replaceSheet(json) {
    const sh = this.sheet(json.id);
    if (!sh) return null;
    sh.update(json);
    if (json.cells) sh.setCells(json.cells);
    return sh;
  }
}

/**
 * What a copy of rectangle `r` puts on the clipboard: `text` is TSV of the displayed values (what
 * Excel, Google Sheets and LibreOffice put in text/plain, so pasting into a document or email gives
 * "$1,740.49", not "=SUM(B2:B4)"); `cells` keeps values, formulas and styles for pastes back into
 * this app, which recognises its own copy by the identical text.
 * @param {SheetModel} sheet
 * @param {{r0:number,c0:number,r1:number,c1:number}} r
 * @returns {{text: string, cells: Array<Array<object|null>>}}
 */
export function clipboardPayload(sheet, r) {
  const rows = [];
  const cells = [];
  for (let row = r.r0; row <= r.r1; row++) {
    const line = [];
    const crow = [];
    for (let col = r.c0; col <= r.c1; col++) {
      const c = sheet.get(row, col);
      crow.push(c ? { v: c.v, t: c.t, f: c.f, s: c.s } : null);
      line.push(c ? sheet.display(c) ?? '' : '');
    }
    rows.push(line);
    cells.push(crow);
  }
  return { text: toTSV(rows), cells };
}

/** Text shown when editing a cell: the formula, or the value in a re-enterable form. */
export function cellEditText(cell) {
  if (!cell) return '';
  if (cell.f) return cell.f;
  switch (cell.t) {
    case 'n': {
      const fmt = cell.s?.fmt || '';
      const n = Number(cell.v);
      if (isDateFormat(fmt)) return serialToText(n, /[hs]/i.test(fmt.replace(/"[^"]*"/g, '')));
      if (isPercentFormat(fmt)) return `${generalNumber(n * 100)}%`;
      return String(Number(n.toPrecision(15)));
    }
    case 'b': return cell.v ? 'TRUE' : 'FALSE';
    case 'e': return String(cell.v);
    case 's': {
      const s = String(cell.v);
      // keep text that would be re-interpreted as something else as text
      if (/^[=+'-]/.test(s) || /^\s*(TRUE|FALSE)\s*$/i.test(s) || /^#/.test(s) || looksNumeric(s)) return `'${s}`;
      return s;
    }
    default: return '';
  }
}

function looksNumeric(s) {
  const t = s.trim();
  if (!t) return false;
  return /^[(+-]?\$?\s*(\d{1,3}(,\d{3})+|\d+)?(\.\d+)?([eE][+-]?\d+)?\s*%*\)?$/.test(t) && /\d/.test(t)
    || /^\d{4}-\d{1,2}-\d{1,2}/.test(t) || /^\d{1,2}[/.]\d{1,2}[/.]\d{2,4}$/.test(t) || /^\d{1,2}:\d{2}/.test(t);
}

/**
 * API update that restores a cell snapshot exactly (clear first, then content and full style).
 * @param {number} row
 * @param {number} col
 * @param {any} snap cell or null
 */
export function restoreUpdate(row, col, snap) {
  const u = { ref: refName(row, col), clear: 'all' };
  if (!snap) return u;
  if (snap.f) u.formula = snap.f;
  else if (snap.t === 'e') u.input = String(snap.v);
  else if (snap.t !== 'z') u.value = snap.v;
  if (snap.s) u.style = fullStyle(snap.s);
  return u;
}

/** A style object with every attribute set (so merging it reproduces the style exactly). */
export function fullStyle(s) {
  return {
    b: !!s.b, i: !!s.i, u: !!s.u, strike: !!s.strike, color: s.color || '', bg: s.bg || '',
    align: s.align || '', valign: s.valign || '', wrap: !!s.wrap, fmt: s.fmt || '', size: s.size || 0,
    border: { t: !!s.border?.t, b: !!s.border?.b, l: !!s.border?.l, r: !!s.border?.r },
  };
}

/** Merge a style patch into a style (local optimistic display), dropping cleared keys. */
export function mergeStyle(style, patch) {
  const out = { ...(style || {}) };
  for (const [k, v] of Object.entries(patch || {})) {
    if (k === 'border') {
      const b = { ...(out.border || {}) };
      for (const [e, on] of Object.entries(v || {})) {
        if (on && on !== 'none') b[e] = true;
        else delete b[e];
      }
      if (Object.keys(b).length) out.border = b;
      else delete out.border;
    } else if (v === false || v === '' || v === 0 || v === null || (k === 'fmt' && String(v).toLowerCase() === 'general')) delete out[k];
    else out[k] = v;
  }
  return Object.keys(out).length ? out : undefined;
}

/** Best local guess of what typed input becomes (shown until the server answers). */
export function provisionalCell(text, prev) {
  const s = prev?.s;
  if (text === '') return s ? { v: null, t: 'z', s } : null;
  if (text.startsWith('=') && text.length > 1) return { v: '…', t: 's', f: text, s, pending: true };
  if (text.startsWith("'")) return { v: text.slice(1), t: 's', s };
  if (/^(TRUE|FALSE)$/i.test(text.trim())) return { v: /^true$/i.test(text.trim()), t: 'b', s };
  const plain = text.trim().replace(/,(?=\d{3}(\D|$))/g, '');
  if (/^[+-]?(\d+\.?\d*|\.\d+)([eE][+-]?\d+)?$/.test(plain)) return { v: Number(plain), t: 'n', s };
  return { v: text, t: 's', s };
}
