// Files app pure helpers: paths, name validation, conflict names, kinds/preview, sorting,
// selection (click/ctrl/shift), keyboard focus, drop planning and URL state.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  joinPath, parentPath, baseName, isWithin, normalizePath, nameProblem, splitExt, numberedName, freeName, extOf,
  fileKind, previewKind, languageFor, isMedia, parseSort, compareEntries, clickSelect, moveFocus, intersects, rectFrom,
  planUpload, splitErrors, routeState, searchQuery, thumbVersion, selectionSummary, parseTags, KIND_ICONS, MAX_TEXT_PREVIEW,
} from '../../assets/js/apps/files/model.js';
import { iconNames } from '../../assets/js/core/icons.js';

test('path helpers', () => {
  assert.equal(joinPath('', 'a'), 'a');
  assert.equal(joinPath('a/b', 'c'), 'a/b/c');
  assert.equal(joinPath('a', ''), 'a');
  assert.equal(parentPath('a/b/c'), 'a/b');
  assert.equal(parentPath('a'), '');
  assert.equal(parentPath(''), '');
  assert.equal(baseName('a/b/c.txt'), 'c.txt');
  assert.ok(isWithin('a/b', 'a'));
  assert.ok(isWithin('a', 'a'));
  assert.ok(!isWithin('ab', 'a'));
  assert.ok(isWithin('x', ''));
  assert.equal(normalizePath('/a//b/./c/'), 'a/b/c');
});

test('name validation mirrors the server', () => {
  for (const ok of ['a', 'Movie (2020).mkv', '.hidden', 'über.txt', 'CONSOLE', 'a..b']) assert.equal(nameProblem(ok), '', ok);
  for (const bad of ['', '.', '..', 'a/b', 'a\\b', 'x\u0000', 'tab\t', 'end.', 'end ', 'CON', 'con.txt', 'Lpt1.tar.gz', `a${String.fromCharCode(0x2028)}b`, 'x'.repeat(256), 'é'.repeat(128)]) {
    assert.notEqual(nameProblem(bad), '', JSON.stringify(bad));
  }
});

test('extensions and numbered conflict names', () => {
  assert.deepEqual(splitExt('report.pdf'), ['report', '.pdf']);
  assert.deepEqual(splitExt('archive.tar.gz'), ['archive', '.tar.gz']);
  assert.deepEqual(splitExt('.bashrc'), ['.bashrc', '']);
  assert.deepEqual(splitExt('Folder.v2', true), ['Folder.v2', '']);
  assert.equal(numberedName('report.pdf', false, 2), 'report (2).pdf');
  assert.equal(numberedName('archive.tar.gz', false, 1), 'archive (1).tar.gz');
  assert.equal(freeName('a.txt', false, new Set(['a.txt', 'a (1).txt'])), 'a (2).txt');
  assert.equal(freeName('New folder', true, new Set()), 'New folder');
  assert.equal(extOf('Photo.JPG'), 'jpg');
  assert.equal(extOf('noext'), '');
});

test('file kinds, icons and preview kinds', () => {
  const e = (name, mime, extra = {}) => ({ name, mime, size: 10, ...extra });
  assert.equal(fileKind({ is_dir: true, name: 'x' }), 'folder');
  assert.equal(fileKind(e('a.png', 'image/png')), 'image');
  assert.equal(fileKind(e('a.mkv', 'video/x-matroska')), 'video');
  assert.equal(fileKind(e('a.mp3', 'audio/mpeg')), 'audio');
  assert.equal(fileKind(e('a.pdf', 'application/pdf')), 'pdf');
  assert.equal(fileKind(e('README.md', 'text/markdown')), 'markdown');
  assert.equal(fileKind(e('main.go', 'text/x-go')), 'code');
  assert.equal(fileKind(e('x.zip', 'application/zip')), 'archive');
  assert.equal(fileKind(e('x.xlsx', 'application/vnd.openxmlformats-officedocument.spreadsheetml.sheet')), 'sheet');
  assert.equal(fileKind(e('rom.gba', 'application/x-gba-rom')), 'game');
  assert.equal(fileKind(e('blob', 'application/octet-stream')), 'file');
  for (const name of Object.values(KIND_ICONS)) assert.ok(iconNames.includes(name), `icon ${name} exists`);

  assert.equal(previewKind(e('a.png', 'image/png')), 'image');
  assert.equal(previewKind(e('a.svg', 'image/svg+xml')), null, 'SVG is never rendered');
  assert.equal(previewKind(e('a.mp4', 'video/mp4')), 'video');
  assert.equal(previewKind(e('a.pdf', 'application/pdf')), 'pdf');
  assert.equal(previewKind(e('notes.md', 'text/markdown')), 'markdown');
  assert.equal(previewKind(e('x.py', 'text/x-python')), 'code');
  assert.equal(previewKind(e('data.csv', 'text/csv')), 'text');
  assert.equal(previewKind(e('big.log', 'text/plain', { size: MAX_TEXT_PREVIEW + 1 })), null);
  assert.equal(previewKind(e('x.zip', 'application/zip')), null);
  assert.equal(previewKind({ is_dir: true, name: 'd' }), null);
  assert.equal(languageFor('main.go'), 'go');
  assert.equal(languageFor('Dockerfile'), 'dockerfile');
  assert.equal(languageFor('x.unknown'), '');
  assert.ok(isMedia(e('a.mkv', 'video/x-matroska')));
  assert.ok(isMedia(e('a.bin', 'application/octet-stream', { category: 'movie' })));
  assert.ok(!isMedia(e('a.mp3', 'audio/mpeg')));
});

test('sorting preferences and local ordering match the server', () => {
  assert.deepEqual(parseSort('size:desc'), { sort: 'size', order: 'desc' });
  assert.deepEqual(parseSort('evil:up'), { sort: 'name', order: 'asc' });
  const items = [
    { id: '1', name: 'b.txt', size: 5, mod_time: '2024', mime: 'text/plain' },
    { id: '2', name: 'A.txt', size: 9, mod_time: '2023', mime: 'text/plain' },
    { id: '3', name: 'zeta', is_dir: true, size: 0, mod_time: '2020', mime: 'inode/directory' },
  ];
  const by = (s, o) => [...items].sort((a, b) => compareEntries(a, b, s, o)).map((x) => x.name);
  assert.deepEqual(by('name', 'asc'), ['zeta', 'A.txt', 'b.txt']);
  assert.deepEqual(by('size', 'desc'), ['zeta', 'A.txt', 'b.txt']);
  assert.deepEqual(by('mod_time', 'asc'), ['zeta', 'A.txt', 'b.txt']);
  assert.deepEqual(by('name', 'desc'), ['zeta', 'b.txt', 'A.txt']);
});

test('click selection: plain, toggle, range and toggle+range', () => {
  const ids = ['a', 'b', 'c', 'd', 'e'];
  let st = { ids, selected: new Set(), anchor: null };
  st = { ids, ...clickSelect(st, 1) };
  assert.deepEqual([...st.selected], ['b']);
  st = { ids, ...clickSelect(st, 3, { range: true }) };
  assert.deepEqual([...st.selected].sort(), ['b', 'c', 'd']);
  assert.equal(st.anchor, 1, 'shift keeps the anchor');
  st = { ids, ...clickSelect(st, 0, { range: true }) };
  assert.deepEqual([...st.selected].sort(), ['a', 'b'], 'shift range replaces');
  st = { ids, ...clickSelect(st, 4, { toggle: true }) };
  assert.deepEqual([...st.selected].sort(), ['a', 'b', 'e']);
  assert.equal(st.anchor, 4);
  st = { ids, ...clickSelect(st, 4, { toggle: true }) };
  assert.deepEqual([...st.selected].sort(), ['a', 'b']);
  st = { ids, ...clickSelect({ ids, selected: new Set(['a']), anchor: 2 }, 4, { toggle: true, range: true }) };
  assert.deepEqual([...st.selected].sort(), ['a', 'c', 'd', 'e'], 'ctrl+shift adds the range');
  assert.deepEqual([...clickSelect(st, 99).selected].sort(), ['a', 'c', 'd', 'e'], 'out of range is a no-op');
});

test('keyboard focus movement in lists and grids', () => {
  assert.equal(moveFocus(-1, 'ArrowDown', 5), 0);
  assert.equal(moveFocus(4, 'ArrowDown', 5), 4);
  assert.equal(moveFocus(2, 'ArrowUp', 5), 1);
  assert.equal(moveFocus(2, 'ArrowRight', 5, 1), 2, 'no horizontal move in lists');
  assert.equal(moveFocus(1, 'ArrowDown', 10, 4), 5);
  assert.equal(moveFocus(1, 'ArrowRight', 10, 4), 2);
  assert.equal(moveFocus(1, 'End', 10, 4), 9);
  assert.equal(moveFocus(5, 'Home', 10, 4), 0);
  assert.equal(moveFocus(0, 'PageDown', 100, 1, 10), 10);
  assert.equal(moveFocus(0, 'x', 3), 0);
  assert.equal(moveFocus(0, 'ArrowDown', 0), -1);
});

test('rubber band geometry', () => {
  const r = rectFrom(50, 60, 10, 20);
  assert.deepEqual(r, { left: 10, top: 20, right: 50, bottom: 60 });
  assert.ok(intersects(r, { left: 40, top: 50, right: 80, bottom: 90 }));
  assert.ok(!intersects(r, { left: 51, top: 0, right: 80, bottom: 90 }));
});

test('upload planning keeps folder structure and finds empty folders', () => {
  const f = (name) => ({ name });
  const plan = planUpload([
    { file: f('a.txt'), relPath: 'Photos/2024/a.txt' },
    { file: f('b.txt'), relPath: 'Photos/b.txt' },
    { file: f('top.txt'), relPath: 'top.txt' },
    { file: f('bad'), relPath: 'Photos/CON/x.txt' },
  ], ['Photos', 'Photos/2024', 'Photos/empty', 'Photos/empty/deeper', 'Other'], 'Uploads');
  assert.deepEqual(plan.uploads.map((u) => `${u.dir}|${u.name}`), ['Uploads/Photos/2024|a.txt', 'Uploads/Photos|b.txt', 'Uploads|top.txt']);
  assert.deepEqual(plan.emptyDirs, ['Uploads/Other', 'Uploads/Photos/empty/deeper']);
  assert.equal(plan.problems.length, 1);
  const root = planUpload([{ file: f('x'), relPath: 'x' }], [], '');
  assert.deepEqual(root.uploads.map((u) => [u.dir, u.name]), [['', 'x']]);
});

test('bulk error grouping', () => {
  const { conflicts, other } = splitErrors([{ id: 'a', code: 'conflict' }, { id: 'b', code: 'forbidden' }, { id: 'c', code: 'conflict' }]);
  assert.deepEqual(conflicts.map((c) => c.id), ['a', 'c']);
  assert.deepEqual(other.map((c) => c.id), ['b']);
  assert.deepEqual(splitErrors(null), { conflicts: [], other: [] });
});

test('route state and search query', () => {
  assert.equal(routeState('', {}).mode, 'browse');
  assert.equal(routeState('trash', {}).mode, 'trash');
  const s = routeState('', { space: 'abc', path: '/A//B/', q: 'cat', kind: 'image', type: 'file', scope: 'all', category: 'image' });
  assert.equal(s.mode, 'search');
  assert.equal(s.path, 'A/B');
  assert.deepEqual(searchQuery(s, 50), { q: 'cat', space_id: undefined, category: 'image', tag: undefined, type: 'file', mime_prefix: 'image/', limit: 50 });
  const t = routeState('', { space: 'abc', category: 'movie', kind: 'evil', type: 'x' });
  assert.equal(t.mode, 'search');
  assert.equal(t.filters.kind, '');
  assert.equal(t.filters.type, '');
  assert.equal(searchQuery(t).space_id, 'abc');
});

test('misc helpers', () => {
  assert.equal(thumbVersion({ size: 10, mod_time: '2024-01-02T03:04:05.000Z' }), '10-20240102030405000');
  assert.deepEqual(selectionSummary([{ is_dir: true }, { size: 5 }, { size: 7 }]), { count: 3, files: 2, folders: 1, bytes: 12 });
  assert.deepEqual(parseTags(' a, b;A ,, c\nd '), ['a', 'b', 'c', 'd']);
  assert.deepEqual(parseTags('x'.repeat(65)), []);
});
