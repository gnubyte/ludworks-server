// Formula text helpers (pure): a forgiving tokenizer, relative-reference shifting for fills
// and pastes, reference extraction for highlighting, and autocomplete/signature context.

import { MAX_ROWS, MAX_COLS, parseCol } from './a1.js';

const ERRORS = /#(?:NULL!|DIV\/0!|VALUE!|REF!|NAME\?|NUM!|N\/A|CIRC!)/iy;
const WS = /\s+/y;
const STR = /"(?:[^"]|"")*"?/y;
const SHEET_Q = /'(?:[^']|'')+'!/y;
const SHEET_U = /[A-Za-z_À-￿][A-Za-z0-9_.À-￿]*!/y;
const CELL = /(\$?)([A-Za-z]{1,3})(\$?)(\d{1,7})(?::(\$?)([A-Za-z]{1,3})(\$?)(\d{1,7}))?(?![A-Za-z0-9_(.!])/y;
const COLS = /(\$?)([A-Za-z]{1,3}):(\$?)([A-Za-z]{1,3})(?![A-Za-z0-9_(.!])/y;
const ROWS = /(\$?)(\d{1,7}):(\$?)(\d{1,7})(?![A-Za-z0-9_(.])/y;
const FUNC = /[A-Za-z_][A-Za-z0-9_.]*(?=\s*\()/y;
const IDENT = /[A-Za-z_\\][A-Za-z0-9_.?\\]*/y;
const NUM = /(?:\d+\.?\d*|\.\d+)(?:[eE][+-]?\d+)?/y;
const OP = /<>|<=|>=|[-+*/^&=<>%,;:(){}]/y;

function at(re, s, i) {
  re.lastIndex = i;
  return re.exec(s);
}

/**
 * Tokenize formula text (with or without the leading "="). Tokens:
 * {type, text, start, end} with type one of ws, str, err, ref, func, name, bool, num, op, other;
 * ref tokens also carry {sheet, kind: 'cell'|'cols'|'rows', m} (regex groups).
 * @param {string} src
 */
export function tokenize(src) {
  const out = [];
  let i = 0;
  if (src.startsWith('=')) {
    out.push({ type: 'op', text: '=', start: 0, end: 1 });
    i = 1;
  }
  while (i < src.length) {
    let m;
    const push = (type, text, extra) => {
      out.push({ type, text, start: i, end: i + text.length, ...extra });
      i += text.length;
    };
    if ((m = at(WS, src, i))) { push('ws', m[0]); continue; }
    if (src[i] === '"') { m = at(STR, src, i); push('str', m[0]); continue; }
    if (src[i] === '#' && (m = at(ERRORS, src, i))) { push('err', m[0]); continue; }
    // optional sheet prefix + reference
    let sheet = '';
    let j = i;
    if ((m = at(SHEET_Q, src, i)) || (m = at(SHEET_U, src, i))) {
      sheet = m[0];
      j = i + m[0].length;
    }
    let ref = null;
    if ((m = at(CELL, src, j))) ref = { kind: 'cell', m };
    else if ((m = at(COLS, src, j))) ref = { kind: 'cols', m };
    else if ((m = at(ROWS, src, j))) ref = { kind: 'rows', m };
    if (ref) {
      const text = sheet + ref.m[0];
      push('ref', text, { sheet: sheet ? unquoteSheet(sheet.slice(0, -1)) : '', kind: ref.kind, m: Array.from(ref.m) });
      continue;
    }
    if ((m = at(FUNC, src, i))) { push('func', m[0]); continue; }
    if ((m = at(IDENT, src, i))) {
      push(/^(TRUE|FALSE)$/i.test(m[0]) ? 'bool' : 'name', m[0]);
      continue;
    }
    if ((m = at(NUM, src, i))) { push('num', m[0]); continue; }
    if ((m = at(OP, src, i))) { push('op', m[0]); continue; }
    push('other', src[i]);
  }
  return out;
}

function unquoteSheet(s) {
  return s.startsWith("'") ? s.slice(1, -1).replace(/''/g, "'") : s;
}

/** Bounds of a ref token as {r0,c0,r1,c1}, or null when invalid. */
export function refBounds(tok) {
  const m = tok.m;
  if (tok.kind === 'cell') {
    const c0 = parseCol(m[2]);
    const r0 = Number(m[4]) - 1;
    const c1 = m[6] ? parseCol(m[6]) : c0;
    const r1 = m[8] ? Number(m[8]) - 1 : r0;
    if (c0 < 0 || c1 < 0 || r0 < 0 || r1 < 0 || r0 >= MAX_ROWS || r1 >= MAX_ROWS) return null;
    return { r0: Math.min(r0, r1), c0: Math.min(c0, c1), r1: Math.max(r0, r1), c1: Math.max(c0, c1) };
  }
  if (tok.kind === 'cols') {
    const c0 = parseCol(m[2]);
    const c1 = parseCol(m[4]);
    if (c0 < 0 || c1 < 0) return null;
    return { r0: 0, c0: Math.min(c0, c1), r1: MAX_ROWS - 1, c1: Math.max(c0, c1) };
  }
  const r0 = Number(m[2]) - 1;
  const r1 = Number(m[4]) - 1;
  if (r0 < 0 || r1 < 0 || r0 >= MAX_ROWS || r1 >= MAX_ROWS) return null;
  return { r0: Math.min(r0, r1), c0: 0, r1: Math.max(r0, r1), c1: MAX_COLS - 1 };
}

const colStr = (c) => {
  let s = '';
  let n = c + 1;
  while (n > 0) { n -= 1; s = String.fromCharCode(65 + (n % 26)) + s; n = Math.floor(n / 26); }
  return s;
};

/**
 * Return the formula as if copied by (dRow, dCol): relative row/column parts move, absolute ($)
 * parts stay, references pushed off the grid become #REF!. Text inside strings is untouched.
 * @param {string} formula
 * @param {number} dRow
 * @param {number} dCol
 */
export function shiftFormula(formula, dRow, dCol) {
  if (!dRow && !dCol) return formula;
  let out = '';
  for (const t of tokenize(formula)) {
    if (t.type !== 'ref') { out += t.text; continue; }
    const m = t.m;
    const prefix = t.text.slice(0, t.text.length - m[0].length);
    let bad = false;
    const col = (abs, letters) => {
      if (abs) return abs + letters.toUpperCase();
      const c = parseCol(letters) + dCol;
      if (c < 0 || c >= MAX_COLS) bad = true;
      return colStr(c);
    };
    const row = (abs, digits) => {
      if (abs) return abs + digits;
      const r = Number(digits) - 1 + dRow;
      if (r < 0 || r >= MAX_ROWS) bad = true;
      return String(r + 1);
    };
    let body;
    if (t.kind === 'cell') {
      body = col(m[1], m[2]) + row(m[3], m[4]);
      if (m[6]) body += ':' + col(m[5], m[6]) + row(m[7], m[8]);
    } else if (t.kind === 'cols') {
      body = col(m[1], m[2]) + ':' + col(m[3], m[4]);
    } else {
      body = row(m[1], m[2]) + ':' + row(m[3], m[4]);
    }
    out += bad ? '#REF!' : prefix + body;
  }
  return out;
}

/**
 * References in a formula with their position in the text, for highlighting.
 * @param {string} formula
 * @returns {Array<{sheet: string, range: {r0,c0,r1,c1}, start: number, end: number}>}
 */
export function formulaRefs(formula) {
  const out = [];
  for (const t of tokenize(formula)) {
    if (t.type !== 'ref') continue;
    const range = refBounds(t);
    if (range) out.push({ sheet: t.sheet, range, start: t.start, end: t.end });
  }
  return out;
}

/**
 * Function-name prefix being typed at the caret, e.g. "=SU|" → {prefix:'SU', start:1}.
 * Returns null outside formulas, inside strings or when the caret is not after a name.
 * @param {string} text
 * @param {number} caret
 */
export function completionContext(text, caret) {
  if (!text.startsWith('=')) return null;
  const before = text.slice(0, caret);
  const toks = tokenize(before);
  const last = toks[toks.length - 1];
  if (!last || last.end !== caret) return null;
  if (last.type !== 'name' && last.type !== 'func' && !(last.type === 'ref' && last.kind === 'cell' && !last.sheet)) return null;
  if (!/^[A-Za-z][A-Za-z0-9_.]*$/.test(last.text)) return null;
  // the next character must not continue the identifier
  if (/[A-Za-z0-9_.(]/.test(text[caret] || '')) return null;
  const prev = toks.slice(0, -1).filter((t) => t.type !== 'ws').at(-1);
  if (prev && prev.type !== 'op') return null;
  if (prev && prev.text === ')') return null;
  return { prefix: last.text.toUpperCase(), start: last.start };
}

/**
 * The innermost function call around the caret: {name, arg} (0-based argument index), or null.
 * @param {string} text
 * @param {number} caret
 */
export function callContext(text, caret) {
  if (!text.startsWith('=')) return null;
  const stack = [];
  let pendingFunc = null;
  for (const t of tokenize(text.slice(0, caret))) {
    if (t.type === 'func') { pendingFunc = t.text.toUpperCase(); continue; }
    if (t.type === 'ws') continue;
    if (t.type === 'op' && t.text === '(') {
      stack.push({ name: pendingFunc, arg: 0 });
      pendingFunc = null;
      continue;
    }
    pendingFunc = null;
    if (t.type === 'op' && t.text === ')') stack.pop();
    else if (t.type === 'op' && (t.text === ',' || t.text === ';') && stack.length) stack[stack.length - 1].arg += 1;
  }
  for (let i = stack.length - 1; i >= 0; i--) if (stack[i].name) return stack[i];
  return null;
}

/**
 * Whether a cell reference may be inserted at the caret ("point mode"): the formula's text
 * before the caret ends in an operator, "(" or "," (or a reference being replaced).
 * @param {string} text
 * @param {number} caret
 */
export function canInsertRef(text, caret) {
  if (!text.startsWith('=')) return false;
  const toks = tokenize(text.slice(0, caret)).filter((t) => t.type !== 'ws');
  const last = toks[toks.length - 1];
  if (!last) return false;
  if (last.type === 'op') return last.text !== ')' && last.text !== '%';
  return false;
}

/**
 * The reference token ending exactly at the caret (to replace while pointing), or null.
 * @param {string} text
 * @param {number} caret
 */
export function refAtCaret(text, caret) {
  const toks = tokenize(text);
  return toks.find((t) => t.type === 'ref' && t.end === caret) || null;
}
