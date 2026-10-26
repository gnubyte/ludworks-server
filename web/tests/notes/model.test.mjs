// Unit tests for the Notes app's pure logic (apps/notes/model.js).
import test from 'node:test';
import assert from 'node:assert/strict';
import {
  buildFolderTree, flattenTree, isWithin, folderPath, viewFromQuery, viewToQuery, sameView, listParams,
  compareNotes, upsertSorted, removeIds, neighbourAfterRemoval, dateGroup, groupNotes, listDate,
  highlightSegments, countWords, prefKey, readPref, writePref, classifyImportFile, createSaver, displayTitle,
  defaultOrder,
} from '../../assets/js/apps/notes/model.js';

const folders = [
  { id: 'aaaaaaaa1', name: 'Work', parent_id: null, position: 1, note_count: 2 },
  { id: 'aaaaaaaa2', name: 'Home', parent_id: null, position: 0, note_count: 1 },
  { id: 'aaaaaaaa3', name: 'Projects', parent_id: 'aaaaaaaa1', position: 0, note_count: 4 },
  { id: 'aaaaaaaa4', name: 'Deep', parent_id: 'aaaaaaaa3', position: 0, note_count: 1 },
  { id: 'aaaaaaaa5', name: 'Orphan', parent_id: 'missingxx', position: 5, note_count: 0 },
];

test('folder tree: order, depth, totals, orphans, flatten with collapse', () => {
  const { roots, byId } = buildFolderTree(folders);
  assert.deepEqual(roots.map((r) => r.name), ['Home', 'Work', 'Orphan']);
  const work = byId.get('aaaaaaaa1');
  assert.equal(work.total, 7);
  assert.equal(byId.get('aaaaaaaa4').depth, 2);
  assert.deepEqual(flattenTree(roots).map((n) => n.name), ['Home', 'Work', 'Projects', 'Deep', 'Orphan']);
  assert.deepEqual(flattenTree(roots, new Set(['aaaaaaaa1'])).map((n) => n.name), ['Home', 'Work', 'Orphan']);
  assert.ok(isWithin(byId, 'aaaaaaaa4', 'aaaaaaaa1'));
  assert.ok(isWithin(byId, 'aaaaaaaa1', 'aaaaaaaa1'));
  assert.ok(!isWithin(byId, 'aaaaaaaa2', 'aaaaaaaa1'));
  assert.equal(folderPath(byId, 'aaaaaaaa4'), 'Work › Projects › Deep');
});

test('views round-trip through the query string', () => {
  for (const v of [{ kind: 'all' }, { kind: 'none' }, { kind: 'folder', id: 'abcdefgh12' }, { kind: 'tag', tag: 'ops' }, { kind: 'pinned' }, { kind: 'trash' }]) {
    const q = Object.fromEntries(Object.entries(viewToQuery(v)).filter(([, x]) => x !== null));
    assert.ok(sameView(viewFromQuery(q), v), JSON.stringify(v));
  }
  assert.deepEqual(viewFromQuery({ f: '../../etc' }), { kind: 'all' }, 'invalid folder ids are ignored');
  assert.deepEqual(listParams({ kind: 'folder', id: 'x1234567' }, { sort: 'title' }), { folder_id: 'x1234567', sort: 'title' });
  assert.deepEqual(listParams({ kind: 'trash' }, { q: ' rsync ' }), { trashed: 1, q: 'rsync' });
  assert.deepEqual(listParams({ kind: 'tag', tag: 'ops' }, { q: 'x' }), { q: 'x' }, 'search spans all notes');
  assert.equal(defaultOrder('title'), 'asc');
  assert.equal(defaultOrder('updated'), 'desc');
});

const n = (id, extra) => ({ id, title: id, pinned: false, updated_at: '2026-01-01T00:00:00.000Z', created_at: '2026-01-01T00:00:00.000Z', ...extra });

test('comparator matches server ordering and upsert keeps lists sorted', () => {
  const cmp = compareNotes('updated', 'desc');
  const items = [
    n('p1', { pinned: true, updated_at: '2026-01-01T00:00:00.000Z' }),
    n('b', { updated_at: '2026-01-03T00:00:00.000Z' }),
    n('a', { updated_at: '2026-01-02T00:00:00.000Z' }),
  ];
  assert.deepEqual([...items].sort(cmp).map((x) => x.id), ['p1', 'b', 'a']);
  const moved = upsertSorted(items, n('a', { updated_at: '2026-01-04T00:00:00.000Z' }), cmp);
  assert.deepEqual(moved.map((x) => x.id), ['p1', 'a', 'b']);
  // A note sorting after the loaded window is dropped unless the list is complete.
  const old = n('z', { updated_at: '2020-01-01T00:00:00.000Z' });
  assert.deepEqual(upsertSorted(items, old, cmp).map((x) => x.id), ['p1', 'b', 'a']);
  assert.deepEqual(upsertSorted(items, old, cmp, { complete: true }).map((x) => x.id), ['p1', 'b', 'a', 'z']);
  const t = compareNotes('title', 'asc');
  assert.deepEqual([n('b'), n('A'), n('c', { pinned: true })].sort(t).map((x) => x.id), ['c', 'A', 'b']);
  assert.deepEqual(removeIds(items, ['b']).map((x) => x.id), ['p1', 'a']);
  assert.equal(neighbourAfterRemoval(items, ['b'], 'b'), 'a');
  assert.equal(neighbourAfterRemoval(items, ['a'], 'a'), 'b');
  assert.equal(neighbourAfterRemoval([items[0]], ['p1'], 'p1'), null);
});

test('date groups and list dates', () => {
  const now = new Date(2026, 9, 26, 15, 0); // Oct 26 2026 local
  const at = (y, m, d, h = 12) => new Date(y, m, d, h).toISOString();
  assert.equal(dateGroup(at(2026, 9, 26, 1), now).label, 'Today');
  assert.equal(dateGroup(at(2026, 9, 25), now).label, 'Yesterday');
  assert.equal(dateGroup(at(2026, 9, 21), now).key, 'week');
  assert.equal(dateGroup(at(2026, 9, 1), now).key, 'month');
  assert.equal(dateGroup(at(2026, 2, 1), now).key, 'm2');
  assert.equal(dateGroup(at(2024, 2, 1), now).label, '2024');
  assert.equal(dateGroup('garbage', now).key, 'unknown');
  assert.equal(listDate(at(2026, 9, 25), now), 'Yesterday');
  assert.match(listDate(at(2026, 9, 26, 9), now), /9/);
  const sections = groupNotes([
    n('p', { pinned: true }), n('t', { updated_at: at(2026, 9, 26) }), n('t2', { updated_at: at(2026, 9, 26) }), n('y', { updated_at: at(2026, 9, 25) }),
  ], { sort: 'updated', searching: false, now });
  assert.deepEqual(sections.map((s) => [s.label, s.items.length]), [['Pinned', 1], ['Today', 2], ['Yesterday', 1]]);
  assert.deepEqual(groupNotes([n('a'), n('b', { pinned: true })], { sort: 'title', searching: true }).map((s) => s.key), ['results']);
});

test('highlight segments use code points and tolerate bad ranges', () => {
  assert.deepEqual(highlightSegments('the backup ran', [[4, 10]]), [
    { text: 'the ', hit: false }, { text: 'backup', hit: true }, { text: ' ran', hit: false }]);
  // Astral characters count as one code point (server offsets), not two UTF-16 units.
  assert.deepEqual(highlightSegments('😀 café', [[2, 6]]), [{ text: '😀 ', hit: false }, { text: 'café', hit: true }]);
  assert.deepEqual(highlightSegments('abc', [[2, 99], [-5, 1], [1, 1], ['x', 2]]), [
    { text: 'a', hit: true }, { text: 'b', hit: false }, { text: 'c', hit: true }]);
  assert.deepEqual(highlightSegments('', []), [{ text: '', hit: false }]);
  assert.deepEqual(highlightSegments('aaaa', [[0, 3], [1, 4]]), [{ text: 'aaa', hit: true }, { text: 'a', hit: true }]);
});

test('word counts ignore Markdown syntax', () => {
  assert.deepEqual(countWords(''), { words: 0, chars: 0 });
  const c = countWords('# Title here\n\n- [ ] buy **milk**\n- see [the docs](https://example.com)\n```js\nlet x = 1;\n```');
  assert.equal(c.words, 10);
  assert.equal(countWords("don't stop-gap 3.14").words, 3);
});

test('preferences are whitelisted and storage failures are tolerated', () => {
  const mem = new Map();
  const storage = { getItem: (k) => mem.get(k) ?? null, setItem: (k, v) => mem.set(k, v) };
  const key = prefKey('u1', 'editor');
  assert.equal(key, 'lw.notes.editor.u1');
  assert.equal(readPref(storage, key, ['rich', 'markdown'], 'rich'), 'rich');
  writePref(storage, key, 'markdown');
  assert.equal(readPref(storage, key, ['rich', 'markdown'], 'rich'), 'markdown');
  mem.set(key, '<script>');
  assert.equal(readPref(storage, key, ['rich', 'markdown'], 'rich'), 'rich');
  const broken = { getItem() { throw new Error('denied'); }, setItem() { throw new Error('denied'); } };
  assert.equal(readPref(broken, key, ['rich'], 'rich'), 'rich');
  writePref(broken, key, 'x');
  assert.equal(displayTitle({ title: '  ' }), 'New Note');
  assert.equal(classifyImportFile({ name: 'Notes.ZIP' }), 'zip');
  assert.equal(classifyImportFile({ name: 'a.markdown' }), 'markdown');
  assert.equal(classifyImportFile({ name: 'a.pdf' }), 'unsupported');
});

/* ------------------------------------------------------------ autosave */

function fakeTimers() {
  let now = 0;
  let seq = 0;
  const pending = new Map();
  return {
    setTimeout(fn, ms) { const id = ++seq; pending.set(id, { fn, at: now + ms }); return id; },
    clearTimeout(id) { pending.delete(id); },
    async advance(ms) {
      now += ms;
      for (;;) {
        const due = [...pending.entries()].filter(([, t]) => t.at <= now).sort((a, b) => a[1].at - b[1].at);
        if (!due.length) break;
        const [id, t] = due[0];
        pending.delete(id);
        t.fn();
        await flushMicrotasks();
      }
      await flushMicrotasks();
    },
    get count() { return pending.size; },
  };
}
const flushMicrotasks = async () => { for (let i = 0; i < 20; i++) await Promise.resolve(); };

function apiError(status, details) {
  const e = new Error('x');
  e.status = status;
  e.details = details;
  return e;
}

test('saver debounces edits into one PATCH with base_version and tracks versions', async () => {
  const timers = fakeTimers();
  const sent = [];
  const states = [];
  const saver = createSaver({
    note: { id: 'n1', version: 3, body: 'a' }, timers, delay: 800,
    send: async (id, patch) => { sent.push(patch); return { id, version: patch.base_version + 1, body: patch.body ?? 'a' }; },
    onState: (s) => states.push(s),
  });
  saver.edit('ab');
  saver.edit('abc');
  await timers.advance(799);
  assert.equal(sent.length, 0);
  assert.equal(saver.state, 'dirty');
  await timers.advance(1);
  assert.deepEqual(sent, [{ body: 'abc', base_version: 3 }]);
  assert.equal(saver.version, 4);
  assert.equal(saver.state, 'saved');
  saver.edit('abc'); // unchanged body: nothing to do
  assert.equal(saver.dirty, false);
  await saver.update({ pinned: true });
  assert.deepEqual(sent[1], { pinned: true, base_version: 4 });
  assert.deepEqual(states, ['dirty', 'saving', 'saved', 'saving', 'saved']);
});

test('saver serialises requests and sends edits made while saving afterwards', async () => {
  const timers = fakeTimers();
  const sent = [];
  let release;
  const saver = createSaver({
    note: { id: 'n1', version: 1, body: '' }, timers,
    send: (id, patch) => new Promise((resolve) => { sent.push(patch); release = () => resolve({ id, version: patch.base_version + 1, body: patch.body }); }),
  });
  saver.edit('one');
  await timers.advance(800);
  assert.equal(sent.length, 1);
  saver.edit('two');
  await timers.advance(800);
  assert.equal(sent.length, 1, 'second request waits for the first');
  release();
  await flushMicrotasks();
  await timers.advance(0);
  assert.equal(sent.length, 2);
  assert.deepEqual(sent[1], { body: 'two', base_version: 2 });
  release();
  assert.equal(await saver.flush(), true);
  assert.equal(saver.version, 3);
});

test('saver reports real conflicts and resolves them', async () => {
  const timers = fakeTimers();
  const conflicts = [];
  let fail = true;
  const sent = [];
  const saver = createSaver({
    note: { id: 'n1', version: 1, body: 'base' }, timers,
    send: async (id, patch) => {
      sent.push(patch);
      if (fail) throw apiError(409, { current: { id, version: 5, body: 'theirs' } });
      return { id, version: patch.base_version + 1, body: patch.body };
    },
    onConflict: (c) => conflicts.push(c),
  });
  saver.edit('mine');
  await timers.advance(800);
  assert.equal(saver.state, 'conflict');
  assert.equal(conflicts.length, 1);
  assert.equal(conflicts[0].current.body, 'theirs');
  assert.deepEqual(conflicts[0].mine, { body: 'mine' });
  saver.edit('mine 2'); // edits continue to accumulate while paused
  await timers.advance(5000);
  assert.equal(sent.length, 1, 'nothing is sent while the conflict is open');
  // "Keep mine": overwrite on top of the server version.
  fail = false;
  saver.resolve({ version: 5, body: 'theirs' });
  await timers.advance(0);
  assert.deepEqual(sent[1], { body: 'mine 2', base_version: 5 });
  assert.equal(saver.state, 'saved');
});

test('saver retries metadata-only conflicts and transient errors automatically', async () => {
  const timers = fakeTimers();
  const sent = [];
  let mode = 'meta-conflict';
  const saver = createSaver({
    note: { id: 'n1', version: 1, body: 'same' }, timers,
    send: async (id, patch) => {
      sent.push(patch);
      if (mode === 'meta-conflict') { mode = 'ok'; throw apiError(409, { current: { id, version: 2, body: 'same' } }); }
      if (mode === 'offline') { mode = 'ok'; throw apiError(0); }
      return { id, version: patch.base_version + 1, body: patch.body ?? 'same' };
    },
  });
  saver.edit('edited');
  await timers.advance(800);
  await timers.advance(0);
  assert.deepEqual(sent.map((p) => p.base_version), [1, 2]);
  assert.equal(saver.state, 'saved');
  mode = 'offline';
  saver.edit('edited again');
  await timers.advance(800);
  assert.equal(saver.state, 'error');
  await timers.advance(2000);
  assert.equal(saver.state, 'saved');
  assert.equal(sent.at(-1).body, 'edited again');
});

test('saver signals deleted notes and stops on stop()', async () => {
  const timers = fakeTimers();
  let gone = 0;
  const saver = createSaver({
    note: { id: 'n1', version: 1, body: '' }, timers,
    send: async () => { throw apiError(404); },
    onGone: () => { gone += 1; },
  });
  saver.edit('x');
  await timers.advance(800);
  assert.equal(gone, 1);
  assert.equal(saver.state, 'gone');
  assert.equal(await saver.flush(), false);
  saver.stop();
  saver.edit('y');
  assert.equal(timers.count, 0);
});

test('derived titles, explicit-title detection and typing previews follow the server rules', async () => {
  const { derivedTitle, hasExplicitTitle, previewSummary, blankMarkdown } = await import('../../assets/js/apps/notes/model.js');
  assert.equal(derivedTitle('# Rack photo![rack](/api/v1/notes/attachments/x)\nnext'), 'Rack photo');
  assert.equal(derivedTitle('![only image](/x.png)\nSecond line'), 'Second line');
  assert.equal(hasExplicitTitle({ title: 'Mail subject', body: 'mail body' }), true);
  assert.equal(hasExplicitTitle({ title: 'Backup rotation', body: '# Backup rotation\ntext' }), false);
  assert.equal(hasExplicitTitle({ title: '', body: '' }), false);
  assert.deepEqual(previewSummary({ title: 'x' }, '# Groceries\n\n- milk\n- eggs'), { title: 'Groceries', snippet: 'milk eggs' });
  assert.equal(previewSummary({ title: 'Kept' }, 'first line', true).title, 'Kept');
  assert.ok([...previewSummary({}, `T\n${'word '.repeat(100)}`).snippet].length <= 200);
  assert.ok(blankMarkdown('&nbsp;\n   '));
  assert.ok(!blankMarkdown('x'));
});
