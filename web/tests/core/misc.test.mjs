// Pure helpers: password strength/generator, fuzzy palette ranking, form diffing,
// table sorting, cursor pager, shortcuts, store, and the category validator.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { estimateStrength, generatePassword } from '../../assets/js/core/password.js';
import { fuzzyScore, rankCommands } from '../../assets/js/core/palette.js';
import { diffObject, deepEqual, asSentence } from '../../assets/js/core/forms.js';
import { sortRows, createCursorPager } from '../../assets/js/core/ui.js';
import { parseCombo, eventMatches, comboLabel } from '../../assets/js/core/shortcuts.js';
import { createStore } from '../../assets/js/core/store.js';
import { slugify, validateCategories, DEFAULT_CATEGORIES } from '../../assets/js/apps/admin/categories.js';
import { categorySegments, usageVariant } from '../../assets/js/core/charts.js';
import { iconNames } from '../../assets/js/core/icons.js';
import { apps, appForPath, visibleApps } from '../../assets/js/apps/registry.js';

test('password strength orders obvious cases', () => {
  assert.equal(estimateStrength('').score, 0);
  assert.equal(estimateStrength('password').score, 0);
  assert.equal(estimateStrength('P@ssw0rd').score, 0);
  assert.ok(estimateStrength('Password2024').score <= 1);
  assert.ok(estimateStrength('aaaaaaaaaaaaaaaa').score <= 1, 'repeats');
  assert.ok(estimateStrength('abcdefghijklmnop').score <= 1, 'sequences');
  assert.ok(estimateStrength('qwertyuiop123').score <= 1, 'keyboard rows');
  assert.ok(estimateStrength('correct horse battery staple').score >= 3);
  assert.equal(estimateStrength('V7#kq9!Lp2@xZr4$').score, 4);
  const withName = estimateStrength('alicewonder99!X', ['alice']);
  const without = estimateStrength('qzmvwonder99!X');
  assert.ok(withName.bits < without.bits + 5);
  assert.ok(withName.warnings.some((w) => /name/.test(w)));
});

test('generated passwords use every class, requested length and no ambiguous chars', () => {
  for (let i = 0; i < 50; i++) {
    const pw = generatePassword(20);
    assert.equal(pw.length, 20);
    assert.match(pw, /[a-z]/);
    assert.match(pw, /[A-Z]/);
    assert.match(pw, /[2-9]/);
    assert.match(pw, /[!#$%&*+\-=?@^_~]/);
    assert.doesNotMatch(pw, /[lIO01]/);
  }
  assert.doesNotMatch(generatePassword(30, { symbols: false }), /[^A-Za-z0-9]/);
  assert.equal(generatePassword(2).length, 4, 'at least one per class');
  // Deterministic with an injected RNG.
  let x = 0;
  const rng = (buf) => { buf[0] = (x = (x * 1103515245 + 12345) >>> 0); };
  x = 1;
  const a = generatePassword(16, { rng });
  x = 1;
  assert.equal(generatePassword(16, { rng }), a);
});

test('fuzzy palette ranking', () => {
  assert.equal(fuzzyScore('', 'Files'), 0);
  assert.equal(fuzzyScore('zz', 'Files'), null);
  assert.ok(fuzzyScore('fil', 'Files') > fuzzyScore('fil', 'Profile'));
  assert.ok(fuzzyScore('atk', 'API tokens') !== null);
  const cmds = [{ title: 'Sign out' }, { title: 'Settings', keywords: ['preferences'] }, { title: 'Sheets' }, { title: 'Storage', keywords: ['disk'] }];
  assert.equal(rankCommands(cmds, 'set')[0].title, 'Settings');
  assert.equal(rankCommands(cmds, 'disk')[0].title, 'Storage');
  assert.equal(rankCommands(cmds, 'prefs')[0].title, 'Settings');
  assert.deepEqual(rankCommands(cmds, '').map((c) => c.title), cmds.map((c) => c.title));
  assert.equal(rankCommands(cmds, 'xyzzy').length, 0);
});

test('diffObject sends only changed fields (deep)', () => {
  const orig = { a: 1, b: 'x', list: ['p', 'q'], nested: { k: true } };
  assert.deepEqual(diffObject(orig, { a: 1, b: 'x', list: ['p', 'q'], nested: { k: true } }), {});
  assert.deepEqual(diffObject(orig, { a: 2, b: 'x', list: ['q', 'p'], nested: { k: false } }), { a: 2, list: ['q', 'p'], nested: { k: false } });
  assert.deepEqual(diffObject(orig, { c: null }), { c: null });
  assert.deepEqual(diffObject(orig, { a: undefined }), {});
  assert.ok(deepEqual([1, { x: [2] }], [1, { x: [2] }]));
  assert.ok(!deepEqual([1], { 0: 1 }));
});

test('table sorting is stable, typed and keeps empties last', () => {
  const rows = [{ n: 'b', v: 10 }, { n: 'a10', v: null }, { n: 'a2', v: 2 }, { n: 'C', v: 2 }];
  assert.deepEqual(sortRows(rows, { key: 'n' }, 'asc').map((r) => r.n), ['a2', 'a10', 'b', 'C']);
  assert.deepEqual(sortRows(rows, { key: 'v' }, 'asc').map((r) => r.n), ['a2', 'C', 'b', 'a10']);
  assert.deepEqual(sortRows(rows, { key: 'v' }, 'desc').map((r) => r.n), ['b', 'a2', 'C', 'a10']);
  assert.deepEqual(sortRows(rows, { key: 'x', sortValue: (r) => r.n.length }, 'asc').map((r) => r.n), ['b', 'C', 'a2', 'a10']);
});

test('cursor pager walks forward and back', async () => {
  const pages = { null: { items: [1, 2], next_cursor: 'c2' }, c2: { items: [3, 4], next_cursor: 'c3' }, c3: { items: [5], next_cursor: null } };
  const seen = [];
  const pager = createCursorPager(async (c) => { seen.push(c); return pages[String(c)]; });
  assert.deepEqual(await pager.first(), [1, 2]);
  assert.deepEqual(pager.state.hasPrev, false);
  assert.deepEqual(await pager.next(), [3, 4]);
  assert.deepEqual(await pager.next(), [5]);
  assert.equal(pager.state.hasNext, false);
  assert.equal(pager.state.page, 3);
  assert.deepEqual(await pager.next(), [5], 'no-op at the end');
  assert.deepEqual(await pager.prev(), [3, 4]);
  assert.deepEqual(await pager.prev(), [1, 2]);
  assert.deepEqual(seen, [null, 'c2', 'c3', 'c2', null]);
});

test('shortcut combos', () => {
  const k = parseCombo('mod+k', false);
  assert.deepEqual(k, { key: 'k', ctrl: true, meta: false, alt: false, shift: false });
  assert.ok(eventMatches({ key: 'k', ctrlKey: true }, k));
  assert.ok(!eventMatches({ key: 'k', metaKey: true }, k));
  assert.ok(!eventMatches({ key: 'k', ctrlKey: true, shiftKey: true }, k));
  const mac = parseCombo('mod+k', true);
  assert.ok(eventMatches({ key: 'K', metaKey: true }, mac));
  assert.ok(eventMatches({ key: '?', shiftKey: true }, parseCombo('?', false)), 'shift ignored for symbols');
  assert.ok(eventMatches({ key: 'Escape' }, parseCombo('esc')));
  assert.equal(comboLabel('mod+k', false), 'Ctrl+K');
  assert.equal(comboLabel('mod+k', true), '⌘K');
  assert.equal(comboLabel('g h', false), 'G then H');
});

test('store select fires only on change', () => {
  const st = createStore({ user: null, n: 0 });
  const seen = [];
  const off = st.select((s) => s.user, (u) => seen.push(u));
  st.set({ n: 1 });
  st.set({ user: 'a' });
  st.set((s) => ({ n: s.n + 1 }));
  st.set({ user: 'b' });
  off();
  st.set({ user: 'c' });
  assert.deepEqual(seen, ['a', 'b']);
  assert.equal(st.get().n, 2);
});

test('category taxonomy validation and slugs', () => {
  assert.deepEqual(validateCategories(DEFAULT_CATEGORIES), []);
  assert.equal(slugify('Blu-ray Rips!'), 'blu-ray-rips');
  assert.equal(slugify('Ánime  Séries'), 'anime-series');
  const bad = validateCategories([
    { slug: 'Movie', name: 'X', subcategories: [] },
    { slug: 'tv', name: '', subcategories: [{ slug: 'a', name: 'A' }, { slug: 'a', name: 'B' }] },
    { slug: 'tv', name: 'TV2', subcategories: [] },
  ]);
  assert.equal(bad.length, 4);
});

test('category segments and usage thresholds', () => {
  const segs = categorySegments({ movie: 100, tv: 300, '': 5, zero: 0 }, { trashBytes: 7 });
  assert.deepEqual(segs.map((s) => s.key), ['tv', 'movie', '', 'trash']);
  assert.equal(segs[0].label, 'TV');
  assert.equal(usageVariant(0.5), 'accent');
  assert.equal(usageVariant(0.85), 'warning');
  assert.equal(usageVariant(0.95), 'danger');
});

test('icon set covers the required names and apps reference real icons', () => {
  const required = ['file', 'folder', 'image', 'film', 'tv', 'note', 'lock', 'key', 'sheet', 'settings', 'shield', 'upload', 'download',
    'search', 'filter', 'grid', 'list', 'play', 'pause', 'next', 'prev', 'shuffle', 'edit', 'trash', 'restore', 'star', 'pin', 'tag', 'link',
    'copy', 'eye', 'eye-off', 'user', 'users', 'mail', 'server', 'storage', 'chevron-down', 'chevron-up', 'chevron-left', 'chevron-right',
    'plus', 'minus', 'x', 'check', 'more', 'menu', 'sun', 'moon', 'refresh', 'external'];
  for (const n of required) assert.ok(iconNames.includes(n), n);
  assert.ok(iconNames.length >= 60);
  for (const a of apps) assert.ok(iconNames.includes(a.icon), a.id);
  // Regression: menus take core icon names only, and the Passwords "New" menu showed Card and
  // Identity without icons (misaligned labels) because core had none.
  for (const n of ['card', 'id']) assert.ok(iconNames.includes(n), n);
});

test('registry helpers', () => {
  assert.equal(appForPath('/').id, 'home');
  assert.equal(appForPath('/files/Movies').id, 'files');
  assert.equal(appForPath('/settings').id, 'settings');
  assert.equal(appForPath('/sheetsx'), null);
  assert.ok(!visibleApps({ role: 'user' }).some((a) => a.id === 'admin'));
  assert.ok(visibleApps({ role: 'admin' }).some((a) => a.id === 'admin'));
  for (const a of apps) assert.equal(typeof a.load, 'function');
});

// Regression: server field errors ("must be at least 12 characters") were shown under the
// field as raw lower-case fragments.
test('server field messages read as sentences', () => {
  assert.equal(asSentence('must be at least 12 characters'), 'Must be at least 12 characters.');
  assert.equal(asSentence('  required '), 'Required.');
  assert.equal(asSentence('Already taken.'), 'Already taken.');
  assert.equal(asSentence('is this right?'), 'Is this right?');
  assert.equal(asSentence('must be one of: fail, rename'), 'Must be one of: fail, rename.');
  assert.equal(asSentence('é invalide'), 'É invalide.');
  assert.equal(asSentence(''), '');
  assert.equal(asSentence(null), '');
});

// Regression: responsive CSS hid button labels with display:none, which also removes them from
// the accessibility tree, so labelled toolbar buttons (Sort, New folder, Upload…) had no
// accessible name on phones. Collapsed labels must be hidden visually only.
test('collapsed button labels stay accessible', async () => {
  const { readdir, readFile } = await import('node:fs/promises');
  const { join } = await import('node:path');
  const dir = new URL('../../assets/css/', import.meta.url).pathname;
  const files = [join(dir, 'app.css'), ...(await readdir(join(dir, 'apps'))).filter((f) => f.endsWith('.css')).map((f) => join(dir, 'apps', f))];
  const offenders = [];
  for (const f of files) {
    const css = (await readFile(f, 'utf8')).replace(/\/\*[\s\S]*?\*\//g, '');
    for (const m of css.matchAll(/([^{}]*\.btn-label[^{}]*)\{([^}]*)\}/g)) {
      if (/display\s*:\s*none/.test(m[2]) || /visibility\s*:\s*hidden/.test(m[2])) offenders.push(`${f}: ${m[1].trim()}`);
    }
  }
  assert.deepEqual(offenders, []);
});
