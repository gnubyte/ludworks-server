// Virtual list math (apps/music/vlist.js) and row selection (apps/music/selection.js).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { visibleRange, dropIndexAt, autoScrollStep, scrollDelta } from '../../assets/js/apps/music/vlist.js';
import { Selection } from '../../assets/js/apps/music/selection.js';

test('visible range with overscan', () => {
  // A 20k-row list whose top is 1000 px above the viewport top; 52 px rows; 800 px viewport.
  const r = visibleRange({ listTop: -1000, viewTop: 0, viewBottom: 800, rowHeight: 52, count: 20000, overscan: 5 });
  assert.deepEqual(r, { start: Math.floor(1000 / 52) - 5, end: Math.ceil(1800 / 52) + 5 });
  // The list starts below the viewport: nothing visible except overscan clamped at 0.
  assert.deepEqual(visibleRange({ listTop: 2000, viewTop: 0, viewBottom: 800, rowHeight: 50, count: 100, overscan: 3 }), { start: 0, end: 0 });
  // Near the end: clamped to count.
  assert.deepEqual(visibleRange({ listTop: -4900, viewTop: 0, viewBottom: 800, rowHeight: 50, count: 100, overscan: 3 }), { start: 95, end: 100 });
  // Insets (top bar, dock) are part of the viewport coordinates.
  assert.deepEqual(visibleRange({ listTop: 56, viewTop: 56, viewBottom: 600, rowHeight: 50, count: 1000, overscan: 0 }), { start: 0, end: 11 });
  assert.deepEqual(visibleRange({ listTop: 0, viewTop: 0, viewBottom: 800, rowHeight: 52, count: 0 }), { start: 0, end: 0 });
  assert.deepEqual(visibleRange({ listTop: 0, viewTop: 0, viewBottom: 0, rowHeight: 52, count: 10 }), { start: 0, end: 0 });
});

test('drop index between rows', () => {
  assert.equal(dropIndexAt(0, 50, 10), 0);
  assert.equal(dropIndexAt(24, 50, 10), 0);
  assert.equal(dropIndexAt(26, 50, 10), 1);
  assert.equal(dropIndexAt(140, 50, 10), 3);
  assert.equal(dropIndexAt(-300, 50, 10), 0);
  assert.equal(dropIndexAt(9999, 50, 10), 10, 'after the last row');
  assert.equal(dropIndexAt(10, 0, 10), 0);
});

test('auto-scroll near the edges', () => {
  assert.equal(autoScrollStep(300, 0, 600), 0);
  assert.ok(autoScrollStep(5, 0, 600) < 0);
  assert.ok(autoScrollStep(595, 0, 600) > 0);
  assert.ok(Math.abs(autoScrollStep(1, 0, 600)) > Math.abs(autoScrollStep(40, 0, 600)), 'faster closer to the edge');
  assert.equal(autoScrollStep(-50, 0, 600, { max: 18 }), -18, 'capped');
  assert.equal(autoScrollStep(10, 10, 10), 0);
});

test('scroll a row into view', () => {
  assert.equal(scrollDelta({ rowTop: 100, rowHeight: 50, viewTop: 56, viewBottom: 700 }), 0, 'already visible');
  assert.equal(scrollDelta({ rowTop: 20, rowHeight: 50, viewTop: 56, viewBottom: 700 }), -36, 'under the top bar');
  assert.equal(scrollDelta({ rowTop: 690, rowHeight: 50, viewTop: 56, viewBottom: 700 }), 40, 'under the dock');
  assert.equal(scrollDelta({ rowTop: 1000, rowHeight: 50, viewTop: 0, viewBottom: 600, block: 'center' }), 725);
});

test('selection: toggle, shift ranges, select all, retain', () => {
  const keys = ['a', 'b', 'c', 'd', 'e'];
  const s = new Selection();
  assert.equal(s.toggle('b'), true);
  assert.equal(s.anchor, 'b');
  s.selectRange(keys, 'd');
  assert.deepEqual(s.inOrder(keys), ['b', 'c', 'd']);
  s.selectRange(keys, 'a');
  assert.deepEqual(s.inOrder(keys), ['a', 'b', 'c', 'd'], 'ranges pivot around the anchor');
  assert.equal(s.toggle('c'), false);
  assert.deepEqual(s.inOrder(keys), ['a', 'b', 'd']);
  s.retain(['a', 'd', 'x']);
  assert.deepEqual([...s.keys].sort(), ['a', 'd']);
  assert.equal(s.anchor, null, 'anchor c left the list');
  s.selectRange(keys, 'e');
  assert.ok(s.has('e'), 'without an anchor a range is the key itself');
  s.selectAll(keys);
  assert.equal(s.size, 5);
  s.set('a', false);
  assert.equal(s.size, 4);
  s.selectRange(keys, 'zz');
  assert.equal(s.size, 4, 'unknown key ignored');
  s.clear();
  assert.equal(s.size, 0);
  assert.equal(s.anchor, null);
});
