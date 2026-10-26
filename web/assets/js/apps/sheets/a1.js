// A1-notation helpers shared by the Sheets app (pure; unit-tested under node).

export const MAX_ROWS = 1048576;
export const MAX_COLS = 16384;
const KEY = 16384; // row * KEY + col fits easily in a double (< 2^35)

/** Map key of a cell. */
export const cellKey = (row, col) => row * KEY + col;
/** Row of a cell key. */
export const keyRow = (k) => Math.floor(k / KEY);
/** Column of a cell key. */
export const keyCol = (k) => k % KEY;

/**
 * Column letters for a 0-based index (0 → A, 26 → AA).
 * @param {number} c
 */
export function colName(c) {
  let s = '';
  let n = c + 1;
  while (n > 0) {
    n -= 1;
    s = String.fromCharCode(65 + (n % 26)) + s;
    n = Math.floor(n / 26);
  }
  return s;
}

/**
 * 0-based column index for letters (case-insensitive), or -1.
 * @param {string} s
 */
export function parseCol(s) {
  if (!/^[A-Za-z]{1,3}$/.test(s)) return -1;
  let n = 0;
  for (const ch of s.toUpperCase()) n = n * 26 + (ch.charCodeAt(0) - 64);
  return n <= MAX_COLS ? n - 1 : -1;
}

/** "B3" for (2, 1). */
export const refName = (row, col) => `${colName(col)}${row + 1}`;

/**
 * Parse "A1" / "$B$12" (case-insensitive). Returns {row, col} or null.
 * @param {string} s
 */
export function parseRef(s) {
  const m = /^\s*\$?([A-Za-z]{1,3})\$?([1-9]\d{0,6})\s*$/.exec(s || '');
  if (!m) return null;
  const col = parseCol(m[1]);
  const row = Number(m[2]) - 1;
  if (col < 0 || row >= MAX_ROWS) return null;
  return { row, col };
}

/**
 * Normalised rectangle (r0 ≤ r1, c0 ≤ c1).
 * @param {{r0:number,c0:number,r1:number,c1:number}} r
 */
export function normRange(r) {
  return { r0: Math.min(r.r0, r.r1), c0: Math.min(r.c0, r.c1), r1: Math.max(r.r0, r.r1), c1: Math.max(r.c0, r.c1) };
}

/**
 * Parse "A1", "A1:C3", "A:C" (whole columns), "2:5" (whole rows). Returns a rectangle or null.
 * @param {string} s
 */
export function parseRange(s) {
  const t = String(s || '').trim();
  const parts = t.split(':');
  if (parts.length === 1) {
    const r = parseRef(t);
    return r ? { r0: r.row, c0: r.col, r1: r.row, c1: r.col } : null;
  }
  if (parts.length !== 2) return null;
  const [a, b] = parts.map((p) => p.replace(/\$/g, ''));
  const ra = parseRef(a);
  const rb = parseRef(b);
  if (ra && rb) return normRange({ r0: ra.row, c0: ra.col, r1: rb.row, c1: rb.col });
  const ca = parseCol(a);
  const cb = parseCol(b);
  if (ca >= 0 && cb >= 0) return normRange({ r0: 0, c0: ca, r1: MAX_ROWS - 1, c1: cb });
  if (/^[1-9]\d{0,6}$/.test(a) && /^[1-9]\d{0,6}$/.test(b) && Number(a) <= MAX_ROWS && Number(b) <= MAX_ROWS) {
    return normRange({ r0: Number(a) - 1, c0: 0, r1: Number(b) - 1, c1: MAX_COLS - 1 });
  }
  return null;
}

/**
 * Render a rectangle: "B2", "A1:C3"; ranges spanning the whole grid of `rows`/`cols` render as
 * "A:C" / "2:5".
 * @param {{r0:number,c0:number,r1:number,c1:number}} r
 * @param {{rows?: number, cols?: number}} [grid]
 */
export function rangeName(r, grid = {}) {
  const rows = grid.rows || MAX_ROWS;
  const cols = grid.cols || MAX_COLS;
  const wholeCols = r.r0 === 0 && r.r1 >= rows - 1;
  const wholeRows = r.c0 === 0 && r.c1 >= cols - 1;
  if (wholeCols && !wholeRows) return `${colName(r.c0)}:${colName(r.c1)}`;
  if (wholeRows && !wholeCols) return `${r.r0 + 1}:${r.r1 + 1}`;
  if (r.r0 === r.r1 && r.c0 === r.c1) return refName(r.r0, r.c0);
  return `${refName(r.r0, r.c0)}:${refName(r.r1, r.c1)}`;
}

/** Number of cells in a rectangle. */
export const rangeArea = (r) => (r.r1 - r.r0 + 1) * (r.c1 - r.c0 + 1);

/** True when (row, col) lies in r. */
export const inRange = (r, row, col) => row >= r.r0 && row <= r.r1 && col >= r.c0 && col <= r.c1;

/** Quote a sheet name for use in a formula when needed ('My Sheet'!A1). */
export function quoteSheet(name) {
  if (/^[A-Za-z_][A-Za-z0-9_.]*$/.test(name) && !/^[A-Za-z]{1,3}\d+$/.test(name) && !/^(TRUE|FALSE)$/i.test(name)) return name;
  return `'${name.replace(/'/g, "''")}'`;
}
