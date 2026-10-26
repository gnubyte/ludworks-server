import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  colName, parseCol, parseRef, parseRange, rangeName, refName, cellKey, keyRow, keyCol, quoteSheet, MAX_ROWS, MAX_COLS,
} from '../../assets/js/apps/sheets/a1.js';
import {
  tokenize, shiftFormula, formulaRefs, completionContext, callContext, canInsertRef, refAtCaret,
} from '../../assets/js/apps/sheets/formula.js';
import { formatValue, generalNumber, isDateFormat, isPercentFormat, serialToText } from '../../assets/js/apps/sheets/numfmt.js';
import { parseTSV, toTSV } from '../../assets/js/apps/sheets/tsv.js';
import { Axis } from '../../assets/js/apps/sheets/axis.js';
import { computeFill, History, cellStats } from '../../assets/js/apps/sheets/fill.js';

test('a1: columns, refs and ranges', () => {
  assert.equal(colName(0), 'A');
  assert.equal(colName(25), 'Z');
  assert.equal(colName(26), 'AA');
  assert.equal(colName(701), 'ZZ');
  assert.equal(colName(702), 'AAA');
  assert.equal(colName(MAX_COLS - 1), 'XFD');
  for (const c of [0, 25, 26, 51, 701, 702, 16383]) assert.equal(parseCol(colName(c)), c);
  assert.equal(parseCol('XFE'), -1);
  assert.equal(parseCol('a1'), -1);
  assert.deepEqual(parseRef('$b$12'), { row: 11, col: 1 });
  assert.equal(parseRef('A0'), null);
  assert.equal(parseRef('A1048577'), null);
  assert.equal(refName(0, 0), 'A1');
  assert.deepEqual(parseRange('C3:A1'), { r0: 0, c0: 0, r1: 2, c1: 2 });
  assert.deepEqual(parseRange('B:D'), { r0: 0, c0: 1, r1: MAX_ROWS - 1, c1: 3 });
  assert.deepEqual(parseRange('2:5'), { r0: 1, c0: 0, r1: 4, c1: MAX_COLS - 1 });
  assert.equal(parseRange('A1:B'), null);
  assert.equal(parseRange(''), null);
  assert.equal(rangeName({ r0: 0, c0: 0, r1: 0, c1: 0 }), 'A1');
  assert.equal(rangeName({ r0: 1, c0: 1, r1: 3, c1: 2 }), 'B2:C4');
  assert.equal(rangeName({ r0: 0, c0: 1, r1: 99, c1: 1 }, { rows: 100, cols: 26 }), 'B:B');
  assert.equal(rangeName({ r0: 4, c0: 0, r1: 4, c1: 25 }, { rows: 100, cols: 26 }), '5:5');
  const k = cellKey(1048575, 16383);
  assert.equal(keyRow(k), 1048575);
  assert.equal(keyCol(k), 16383);
  assert.equal(quoteSheet('Sheet1'), 'Sheet1');
  assert.equal(quoteSheet('My Sheet'), "'My Sheet'");
  assert.equal(quoteSheet("Bob's"), "'Bob''s'");
  assert.equal(quoteSheet('AB12'), "'AB12'");
});

test('formula: tokenizer distinguishes refs, functions, strings and names', () => {
  const types = tokenize('=SUM(A1:B2, Sheet2!C3, \'My Sheet\'!$D$4, "A1", LOG10(2), A:A, 1:3, TRUE, x_1)')
    .filter((t) => t.type !== 'ws' && t.type !== 'op').map((t) => `${t.type}:${t.text}`);
  assert.deepEqual(types, ['func:SUM', 'ref:A1:B2', 'ref:Sheet2!C3', "ref:'My Sheet'!$D$4", 'str:"A1"', 'func:LOG10',
    'num:2', 'ref:A:A', 'ref:1:3', 'bool:TRUE', 'name:x_1']);
  const refs = formulaRefs("=A1+'My Sheet'!B2:C3");
  assert.equal(refs.length, 2);
  assert.deepEqual(refs[1], { sheet: 'My Sheet', range: { r0: 1, c0: 1, r1: 2, c1: 2 }, start: 4, end: 20 });
  // tokens cover the whole input
  const src = '=IF(A1>=10,"big ""x""",#N/A)&"!"';
  assert.equal(tokenize(src).map((t) => t.text).join(''), src);
});

test('formula: shiftFormula moves relative parts only', () => {
  assert.equal(shiftFormula('=A1+$B$2+C$3+$D4', 1, 1), '=B2+$B$2+D$3+$D5');
  assert.equal(shiftFormula('=SUM(A1:A10)*2', 2, 0), '=SUM(A3:A12)*2');
  assert.equal(shiftFormula('=Sheet2!A1+\'My Sheet\'!B2', 0, 2), '=Sheet2!C1+\'My Sheet\'!D2');
  assert.equal(shiftFormula('=SUM(A:A)+SUM(1:2)', 1, 1), '=SUM(B:B)+SUM(2:3)');
  assert.equal(shiftFormula('="A1"&A1', 1, 0), '="A1"&A2');
  assert.equal(shiftFormula('=A1', -1, 0), '=#REF!');
  assert.equal(shiftFormula('=LOG10(A1)', 0, 1), '=LOG10(B1)');
  assert.equal(shiftFormula('=a1*2', 1, 0), '=A2*2');
  assert.equal(shiftFormula('=XFD1', 0, 1), '=#REF!');
  assert.equal(shiftFormula('=A1', 0, 0), '=A1');
});

test('formula: completion, call and pointing contexts', () => {
  assert.deepEqual(completionContext('=SU', 3), { prefix: 'SU', start: 1 });
  assert.deepEqual(completionContext('=A1+vl', 6), { prefix: 'VL', start: 4 });
  assert.deepEqual(completionContext('=IF(A1,co', 9), { prefix: 'CO', start: 7 });
  assert.equal(completionContext('SU', 2), null);
  assert.equal(completionContext('="SU', 4), null);
  assert.equal(completionContext('=A1', 3)?.prefix, 'A1'); // could be a function like A1… harmless
  assert.equal(completionContext('=SUM(1)x', 8), null);
  assert.deepEqual(callContext('=SUM(A1, ', 9), { name: 'SUM', arg: 1 });
  assert.deepEqual(callContext('=IF(A1>0, ROUND(B1, ', 20), { name: 'ROUND', arg: 1 });
  assert.deepEqual(callContext('=IF(A1>0, ROUND(B1, 2), ', 24), { name: 'IF', arg: 2 });
  assert.equal(callContext('=1+2', 4), null);
  assert.equal(canInsertRef('=', 1), true);
  assert.equal(canInsertRef('=SUM(', 5), true);
  assert.equal(canInsertRef('=A1+', 4), true);
  assert.equal(canInsertRef('=A1', 3), false);
  assert.equal(canInsertRef('=SUM(A1)', 8), false);
  assert.equal(canInsertRef('12', 2), false);
  assert.equal(refAtCaret('=A1+B2', 6)?.text, 'B2');
  assert.equal(refAtCaret('=A1+B2', 5), null);
});

test('numfmt: general, fixed, separators, percent, currency, scientific', () => {
  assert.equal(generalNumber(0.1 + 0.2), '0.3');
  assert.equal(generalNumber(1 / 3), '0.3333333333');
  assert.equal(generalNumber(1234567), '1234567');
  assert.equal(generalNumber(1e15), '1E+15');
  assert.equal(generalNumber(-2.5e-12), '-2.5E-12');
  const f = (v, fmt, t = 'n') => formatValue(v, t, fmt).text;
  assert.equal(f(1234.5, ''), '1234.5');
  assert.equal(f(1234.5, 'general'), '1234.5');
  assert.equal(f(1234.567, '0'), '1235');
  assert.equal(f(1234.5, '0.00'), '1234.50');
  assert.equal(f(1234567.891, '#,##0.00'), '1,234,567.89');
  assert.equal(f(-1234.5, '#,##0'), '-1,235');
  assert.equal(f(0.1234, '0%'), '12%');
  assert.equal(f(0.12345, '0.00%'), '12.35%');
  assert.equal(f(1000.5, '$#,##0.00'), '$1,000.50');
  assert.equal(f(-1000.5, '$#,##0.00'), '-$1,000.50');
  assert.equal(f(-5, '$#,##0.00;[Red]-$#,##0.00'), '-$5.00');
  assert.equal(formatValue(-5, 'n', '$#,##0.00;[Red]-$#,##0.00').color, '#d93025');
  assert.equal(f(-5, '0.00;(0.00)'), '(5.00)');
  assert.equal(f(0, '0.00;-0.00;"zero"'), 'zero');
  assert.equal(f(12345, '0.00E+00'), '1.23E+04');
  assert.equal(f(0.00012, '0.0E+00'), '1.2E-04');
  assert.equal(f(1500000, '#,##0,"K"'), '1,500K');
  assert.equal(f(0.5, '#.00'), '.50');
  assert.equal(f(5, '0.##'), '5.');
  assert.equal(f(7, '"Qty: "0'), 'Qty: 7');
  assert.equal(f(-0.001, '0.00'), '0.00');
  assert.equal(f('text', '0.00', 's'), 'text');
  assert.equal(f('abc', '0;0;0;"<"@">"', 's'), '<abc>');
  assert.equal(f(true, '', 'b'), 'TRUE');
  assert.equal(f('#DIV/0!', '0.00', 'e'), '#DIV/0!');
  assert.equal(f(null, '0', 'z'), '');
});

test('numfmt: dates and times on 1900 serials', () => {
  const f = (v, fmt) => formatValue(v, 'n', fmt).text;
  assert.equal(f(46321, 'yyyy-mm-dd'), '2026-10-26');
  assert.equal(f(46321, 'mmm d, yyyy'), 'Oct 26, 2026');
  assert.equal(f(46321, 'dddd, mmmm d'), 'Monday, October 26');
  assert.equal(f(46321.75, 'yyyy-mm-dd h:mm'), '2026-10-26 18:00');
  assert.equal(f(0.6659722222, 'h:mm AM/PM'), '3:59 PM');
  assert.equal(f(0.5, 'hh:mm:ss'), '12:00:00');
  assert.equal(f(60, 'yyyy-mm-dd'), '1900-02-29');
  assert.equal(f(61, 'yyyy-mm-dd'), '1900-03-01');
  assert.equal(f(1, 'd/m/yy'), '1/1/00');
  assert.equal(f(1.5, '[h]:mm'), '36:00');
  assert.equal(isDateFormat('yyyy-mm-dd'), true);
  assert.equal(isDateFormat('0.00'), false);
  assert.equal(isDateFormat('"m"0'), false);
  assert.equal(isPercentFormat('0.0%'), true);
  assert.equal(serialToText(46321, false), '2026-10-26');
  assert.equal(serialToText(46321.5, true), '2026-10-26 12:00');
  assert.equal(serialToText(0.25, true), '6:00');
});

test('tsv: round trip with quotes, tabs and newlines', () => {
  const rows = [['a', 'b\tc', ''], ['line1\nline2', 'say "hi"', '3']];
  const text = toTSV(rows);
  assert.equal(text, 'a\t"b\tc"\t\n"line1\nline2"\t"say ""hi"""\t3');
  assert.deepEqual(parseTSV(text), rows);
  assert.deepEqual(parseTSV('1\t2\r\n3\t4\r\n'), [['1', '2'], ['3', '4']]);
  assert.deepEqual(parseTSV('x'), [['x']]);
  assert.deepEqual(parseTSV(''), []);
  assert.deepEqual(parseTSV('a\t\nb'), [['a', ''], ['b', '']]);
  assert.deepEqual(parseTSV('"unclosed\tx'), [['"unclosed', 'x']]);
});

test('axis: positions with sparse custom sizes', () => {
  const ax = new Axis(1000000, 21, { 2: 40, 10: 5 });
  assert.equal(ax.pos(0), 0);
  assert.equal(ax.pos(2), 42);
  assert.equal(ax.pos(3), 82);
  assert.equal(ax.pos(11), 11 * 21 + 19 - 16);
  assert.equal(ax.total(), 1000000 * 21 + 3);
  for (const i of [0, 1, 2, 3, 9, 10, 11, 500000, 999999]) {
    assert.equal(ax.indexAt(ax.pos(i)), i);
    assert.equal(ax.indexAt(ax.pos(i) + ax.size(i) - 0.5), i);
  }
  assert.equal(ax.indexAt(-5), 0);
  assert.equal(ax.indexAt(1e12), 999999);
  ax.setSize(2, null);
  assert.equal(ax.pos(3), 63);
});

test('fill: series, text counters, formulas and cycles', () => {
  const grid = new Map();
  const set = (r, c, cell) => grid.set(`${r},${c}`, cell);
  const get = (r, c) => grid.get(`${r},${c}`) || null;
  set(0, 0, { v: 1, t: 'n' });
  set(1, 0, { v: 3, t: 'n' });
  set(0, 1, { v: 'Item 9', t: 's' });
  set(0, 2, { v: 2, t: 'n', f: '=A1*2', s: { b: true } });
  set(0, 3, { v: 46321, t: 'n', s: { fmt: 'yyyy-mm-dd' } });
  set(0, 4, { v: 'x', t: 's' });
  set(1, 4, { v: 5, t: 'n' });
  // A1:A2 series down to A5
  let out = computeFill(get, { r0: 0, c0: 0, r1: 1, c1: 0 }, { r0: 0, c0: 0, r1: 4, c1: 0 });
  assert.deepEqual(out.map((o) => o.cell.v), [5, 7, 9]);
  // up: continues backwards
  out = computeFill(get, { r0: 0, c0: 0, r1: 1, c1: 0 }, { r0: -2 + 2, c0: 0, r1: 1, c1: 0 });
  assert.equal(out.length, 0);
  out = computeFill(get, { r0: 0, c0: 1, r1: 0, c1: 1 }, { r0: 0, c0: 1, r1: 2, c1: 1 });
  assert.deepEqual(out.map((o) => o.cell.v), ['Item 10', 'Item 11']);
  out = computeFill(get, { r0: 0, c0: 2, r1: 0, c1: 2 }, { r0: 0, c0: 2, r1: 2, c1: 2 });
  assert.deepEqual(out.map((o) => [o.row, o.cell.f, o.cell.s.b]), [[1, '=A2*2', true], [2, '=A3*2', true]]);
  out = computeFill(get, { r0: 0, c0: 3, r1: 0, c1: 3 }, { r0: 0, c0: 3, r1: 1, c1: 3 });
  assert.equal(out[0].cell.v, 46322);
  // mixed → cycle
  out = computeFill(get, { r0: 0, c0: 4, r1: 1, c1: 4 }, { r0: 0, c0: 4, r1: 4, c1: 4 });
  assert.deepEqual(out.map((o) => o.cell.v), ['x', 5, 'x']);
  // horizontal formula fill shifts columns
  out = computeFill(get, { r0: 0, c0: 2, r1: 0, c1: 2 }, { r0: 0, c0: 2, r1: 0, c1: 3 });
  assert.equal(out[0].cell.f, '=B1*2');
  // single number copies; blanks fill blank
  set(5, 0, { v: 7, t: 'n' });
  out = computeFill(get, { r0: 5, c0: 0, r1: 6, c1: 0 }, { r0: 5, c0: 0, r1: 8, c1: 0 });
  assert.deepEqual(out.map((o) => o.cell?.v ?? null), [7, null]);
  // decimals do not accumulate float noise
  set(10, 0, { v: 0.1, t: 'n' });
  set(11, 0, { v: 0.2, t: 'n' });
  out = computeFill(get, { r0: 10, c0: 0, r1: 11, c1: 0 }, { r0: 10, c0: 0, r1: 12, c1: 0 });
  assert.equal(out[0].cell.v, 0.3);
});

test('history and stats', () => {
  const h = new History(2);
  h.push({ id: 1 });
  h.push({ id: 2 });
  h.push({ id: 3 });
  assert.equal(h.undoStack.length, 2);
  assert.equal(h.undo().id, 3);
  assert.equal(h.canRedo, true);
  assert.equal(h.redo().id, 3);
  h.undo();
  h.push({ id: 4 });
  assert.equal(h.canRedo, false);
  const s = cellStats([{ v: 1, t: 'n' }, { v: 2.5, t: 'n' }, { v: 'x', t: 's' }, null, { t: 'z' }, { v: true, t: 'b' }]);
  assert.deepEqual(s, { count: 4, numCount: 2, sum: 3.5, avg: 1.75, min: 1, max: 2.5 });
  assert.deepEqual(cellStats([]).avg, null);
});

// Regression (e2e): copying a formula cell put "=SUM(B2:B4)" on the OS clipboard, so pasting into
// a document or email showed the formula instead of the value. text/plain must carry what the
// cells display (like Excel/Google Sheets); formulas and styles travel in `cells` for own pastes.
test('clipboard: text carries displayed values, cells keep formulas and styles', async () => {
  const { SheetModel, clipboardPayload } = await import('../../assets/js/apps/sheets/model.js');
  const sh = new SheetModel({
    id: 's1', name: 'Sheet1', index: 0, rows: 10, cols: 5,
    cells: {
      A1: { v: 'Item', t: 's' },
      B1: { v: 1200, t: 'n', s: { fmt: '$#,##0.00' } },
      A2: { v: 'Total', t: 's' },
      B2: { v: 1740.49, t: 'n', f: '=SUM(B1:B1)', s: { fmt: '$#,##0.00' } },
      C2: { v: 0.12, t: 'n', s: { fmt: '0%' } },
      C1: { v: 'tab\there', t: 's' },
    },
  });
  const p = clipboardPayload(sh, { r0: 0, c0: 0, r1: 2, c1: 2 });
  assert.equal(p.text, 'Item\t$1,200.00\t"tab\there"\nTotal\t$1,740.49\t12%\n\t\t');
  assert.ok(!p.text.includes('='), 'no formula text on the clipboard');
  assert.equal(p.cells[1][1].f, '=SUM(B1:B1)');
  assert.deepEqual(p.cells[1][1].s, { fmt: '$#,##0.00' });
  assert.equal(p.cells[2][0], null);
  assert.deepEqual(parseTSV(p.text)[1], ['Total', '$1,740.49', '12%']);
});
