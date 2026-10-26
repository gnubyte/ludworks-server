// Fill-handle logic, undo history and selection statistics (pure, unit-tested).

import { shiftFormula } from './formula.js';
import { isDateFormat } from './numfmt.js';

const TRAILING_INT = /^(.*?)(\d+)$/;

/**
 * Compute the cells produced by dragging the fill handle from `src` to `target` (which
 * contains src and extends it along one axis).
 *  - formulas are copied with relative references shifted;
 *  - two or more numbers form a linear series, a single date counts up by days;
 *  - text ending in a number ("Item 1", "Q3") counts up;
 *  - everything else (and styles) repeats cyclically.
 * @param {(row: number, col: number) => any} get returns the cell {v,t,f,s} or null
 * @param {{r0,c0,r1,c1}} src
 * @param {{r0,c0,r1,c1}} target
 * @returns {Array<{row: number, col: number, cell: {v?: any, t?: string, f?: string, s?: object}|null}>}
 */
export function computeFill(get, src, target) {
  const vertical = target.r0 < src.r0 || target.r1 > src.r1;
  const out = [];
  const lines = vertical ? [src.c0, src.c1] : [src.r0, src.r1];
  const [a0, a1] = vertical ? [src.r0, src.r1] : [src.c0, src.c1];
  const [t0, t1] = vertical ? [target.r0, target.r1] : [target.c0, target.c1];
  const n = a1 - a0 + 1;
  for (let line = lines[0]; line <= lines[1]; line++) {
    const seq = [];
    for (let i = a0; i <= a1; i++) seq.push(vertical ? get(i, line) : get(line, i));
    const series = detectSeries(seq);
    for (let p = t0; p <= t1; p++) {
      if (p >= a0 && p <= a1) continue;
      const k = p - a0; // position relative to the source start (negative when filling up/left)
      const si = ((k % n) + n) % n;
      const offset = k - si;
      const srcCell = seq[si];
      const row = vertical ? p : line;
      const col = vertical ? line : p;
      let cell = null;
      if (series) cell = series(k, srcCell);
      else if (srcCell && srcCell.f) {
        cell = { f: shiftFormula(srcCell.f, vertical ? offset : 0, vertical ? 0 : offset) };
      } else if (srcCell && srcCell.t !== 'z') cell = { v: srcCell.v, t: srcCell.t };
      if (srcCell && srcCell.s) cell = { ...(cell || { v: null, t: 'z' }), s: srcCell.s };
      out.push({ row, col, cell });
    }
  }
  return out;
}

function detectSeries(seq) {
  if (!seq.length || seq.some((c) => !c || c.f)) return null;
  if (seq.every((c) => c.t === 'n')) {
    const first = Number(seq[0].v);
    if (seq.length === 1) {
      if (!isDateFormat(seq[0].s?.fmt)) return null;
      return (k, srcCell) => ({ v: first + k, t: 'n', s: srcCell?.s });
    }
    const last = Number(seq[seq.length - 1].v);
    const step = (last - first) / (seq.length - 1);
    return (k, srcCell) => ({ v: roundSeries(first + step * k), t: 'n', s: srcCell?.s });
  }
  if (seq.every((c) => c.t === 's' && TRAILING_INT.test(String(c.v)))) {
    const parts = seq.map((c) => TRAILING_INT.exec(String(c.v)));
    const prefix = parts[0][1];
    if (!parts.every((m) => m[1] === prefix)) return null;
    const width = parts[0][2].length;
    const first = Number(parts[0][2]);
    const step = seq.length > 1 ? (Number(parts[parts.length - 1][2]) - first) / (seq.length - 1) : 1;
    if (!Number.isInteger(step)) return null;
    return (k, srcCell) => {
      const num = first + step * k;
      const digits = String(Math.abs(num)).padStart(parts[0][2].startsWith('0') ? width : 1, '0');
      return { v: `${prefix}${num < 0 ? '-' : ''}${digits}`, t: 's', s: srcCell?.s };
    };
  }
  return null;
}

// Avoid 0.30000000000000004-style artefacts in decimal series.
function roundSeries(x) {
  return Number.isFinite(x) ? Number(x.toPrecision(15)) : x;
}

/**
 * Undo/redo stack of plain entries (the editor knows how to apply them).
 */
export class History {
  constructor(limit = 100) {
    this.limit = limit;
    this.undoStack = [];
    this.redoStack = [];
  }

  /** Record a new action (clears the redo stack). */
  push(entry) {
    this.undoStack.push(entry);
    if (this.undoStack.length > this.limit) this.undoStack.shift();
    this.redoStack = [];
  }

  /** Pop the action to undo (moved to the redo stack), or null. */
  undo() {
    const e = this.undoStack.pop() || null;
    if (e) this.redoStack.push(e);
    return e;
  }

  /** Pop the action to redo (moved back to the undo stack), or null. */
  redo() {
    const e = this.redoStack.pop() || null;
    if (e) this.undoStack.push(e);
    return e;
  }

  get canUndo() { return this.undoStack.length > 0; }

  get canRedo() { return this.redoStack.length > 0; }

  clear() {
    this.undoStack = [];
    this.redoStack = [];
  }
}

/**
 * Status-bar statistics of cells: count of non-empty cells, and sum/average/min/max of numbers.
 * @param {Iterable<any>} cells
 */
export function cellStats(cells) {
  let count = 0;
  let numCount = 0;
  let sum = 0;
  let min = Infinity;
  let max = -Infinity;
  for (const c of cells) {
    if (!c || c.t === 'z') continue;
    count++;
    if (c.t === 'n') {
      const x = Number(c.v);
      numCount++;
      sum += x;
      if (x < min) min = x;
      if (x > max) max = x;
    }
  }
  return { count, numCount, sum: roundSeries(sum), avg: numCount ? roundSeries(sum / numCount) : null, min: numCount ? min : null, max: numCount ? max : null };
}
