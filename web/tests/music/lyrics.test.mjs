// Music lyrics helpers (apps/music/lyrics.js): normalising the API response and finding the line
// sung at a position (LRC sync).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { normalizeLyrics, activeLineIndex, formatLrcTime } from '../../assets/js/apps/music/lyrics.js';

const synced = {
  synced: true, source: 'lrc', text: '',
  lines: [
    { time_seconds: 0.5, text: 'First' },
    { time_seconds: 4.25, text: 'Second' },
    { time_seconds: 9, text: 'Third' },
    { time_seconds: 12.75, text: '' },
  ],
};

test('normalise synced lyrics', () => {
  const l = normalizeLyrics(synced);
  assert.equal(l.synced, true);
  assert.equal(l.source, 'lrc');
  assert.deepEqual(l.lines.map((x) => x.time), [0.5, 4.25, 9, 12.75]);
  assert.equal(l.text, 'First\nSecond\nThird\n', 'text rebuilt from the lines');
});

test('out-of-order stamps are sorted; untimed lines travel with the line before them', () => {
  const l = normalizeLyrics({
    synced: true,
    lines: [
      { time_seconds: 10, text: 'C' },
      { time_seconds: null, text: 'C2' },
      { time_seconds: 2, text: 'A' },
      { time_seconds: 5, text: 'B' },
      { time_seconds: 5, text: 'B again' },
    ],
  });
  assert.deepEqual(l.lines.map((x) => x.text), ['A', 'B', 'B again', 'C', 'C2']);
});

test('plain lyrics and synced flags without times', () => {
  const plain = normalizeLyrics({ synced: false, lines: [{ time_seconds: null, text: '' }, { time_seconds: null, text: 'Hello' }, { time_seconds: null, text: 'World' }, { time_seconds: null, text: ' ' }], text: '\nHello\nWorld\n ', source: 'embedded' });
  assert.equal(plain.synced, false);
  assert.deepEqual(plain.lines.map((x) => x.text), ['Hello', 'World'], 'blank edges trimmed');
  const claimed = normalizeLyrics({ synced: true, lines: [{ time_seconds: null, text: 'x' }] });
  assert.equal(claimed.synced, false, 'synced needs at least one time');
  const textOnly = normalizeLyrics({ synced: false, lines: [], text: 'one\r\ntwo' });
  assert.deepEqual(textOnly.lines.map((x) => x.text), ['one', 'two']);
  assert.deepEqual(normalizeLyrics(null), { synced: false, lines: [], text: '', source: '' });
  const bad = normalizeLyrics({ synced: true, lines: [{ time_seconds: -3, text: 'a' }, { time_seconds: 'x', text: 'b' }, { time_seconds: 1, text: 7 }] });
  assert.deepEqual(bad.lines.map((x) => x.time), [null, null, 1], 'invalid stamps become untimed lines');
  assert.equal(bad.lines[2].text, '', 'non-string text');
});

test('active line', () => {
  const { lines } = normalizeLyrics(synced);
  assert.equal(activeLineIndex(lines, 0), -1, 'before the first line');
  assert.equal(activeLineIndex(lines, 0.5), 0);
  assert.equal(activeLineIndex(lines, 4.24), 0);
  assert.equal(activeLineIndex(lines, 4.25), 1);
  assert.equal(activeLineIndex(lines, 100), 3);
  assert.equal(activeLineIndex(lines, 4, 0.3), 1, 'offset shows a line a little early');
  assert.equal(activeLineIndex([], 3), -1);
  assert.equal(activeLineIndex(lines, NaN), -1);
  const withGaps = [{ time: 1, text: 'a' }, { time: null, text: 'a2' }, { time: 3, text: 'b' }];
  assert.equal(activeLineIndex(withGaps, 2), 0);
  assert.equal(activeLineIndex(withGaps, 3), 2);
  // Large lists use the binary search.
  const many = Array.from({ length: 1000 }, (_, i) => ({ time: i * 2, text: String(i) }));
  assert.equal(activeLineIndex(many, 1001), 500);
});

test('LRC time labels', () => {
  assert.equal(formatLrcTime(83.45), '1:23.45');
  assert.equal(formatLrcTime(5), '0:05.00');
  assert.equal(formatLrcTime(-1), '0:00.00');
});
