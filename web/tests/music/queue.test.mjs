// Music play queue (apps/music/queue.js): play order, shuffle/unshuffle, repeat, insert/append,
// remove/move/clear, cast windows and localStorage round trips.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  PlayQueue, REPEAT_MODES, MAX_STORED, nextRepeat, shuffleInPlace, moveWithin, insertionToPosition,
} from '../../assets/js/apps/music/queue.js';

const T = (n) => ({ id: `track${String(n).padStart(4, '0')}`, title: `T${n}` });
const tracks = (n) => Array.from({ length: n }, (_, i) => T(i + 1));
const ids = (q) => q.entries.map((e) => e.track.title);

/** Deterministic PRNG (mulberry32). */
function rng(seed = 1) {
  let a = seed >>> 0;
  return () => {
    a = (a + 0x6d2b79f5) >>> 0;
    let t = a;
    t = Math.imul(t ^ (t >>> 15), t | 1);
    t ^= t + Math.imul(t ^ (t >>> 7), t | 61);
    return ((t ^ (t >>> 14)) >>> 0) / 4294967296;
  };
}

test('repeat modes cycle', () => {
  assert.deepEqual(REPEAT_MODES, ['off', 'all', 'one']);
  assert.equal(nextRepeat('off'), 'all');
  assert.equal(nextRepeat('all'), 'one');
  assert.equal(nextRepeat('one'), 'off');
  assert.equal(nextRepeat('junk'), 'off');
});

test('shuffleInPlace is a permutation', () => {
  const a = Array.from({ length: 50 }, (_, i) => i);
  const b = shuffleInPlace(a.slice(), rng(7));
  assert.notDeepEqual(b, a);
  assert.deepEqual(b.slice().sort((x, y) => x - y), a);
});

test('moveWithin follows the playlist items/move rule', () => {
  const L = ['A', 'B', 'C', 'D'];
  assert.deepEqual(moveWithin(L, (x) => x === 'B', 3), ['A', 'C', 'D', 'B']);
  assert.deepEqual(moveWithin(L, (x) => x === 'D', 0), ['D', 'A', 'B', 'C']);
  assert.deepEqual(moveWithin(L, (x) => x === 'B' || x === 'D', 0), ['B', 'D', 'A', 'C'], 'keeps relative order');
  assert.deepEqual(moveWithin(L, (x) => x === 'A', 99), ['B', 'C', 'D', 'A'], 'clamped');
  assert.deepEqual(moveWithin(L, () => false, 1), L);
  // A drop "before row 3" of item 1 lands at position 2 of the list without it.
  assert.equal(insertionToPosition(1, 3), 2);
  assert.equal(insertionToPosition(3, 1), 1);
  assert.equal(insertionToPosition(2, 2), 2);
});

test('set, next and previous without shuffle', () => {
  const q = new PlayQueue();
  assert.equal(q.current, null);
  assert.equal(q.next(), null);
  q.set(tracks(3), { index: 1, context: { kind: 'album', id: 'album0001' } });
  assert.equal(q.current.track.title, 'T2');
  assert.deepEqual(q.current.context, { kind: 'album', id: 'album0001' });
  assert.equal(q.next().track.title, 'T3');
  assert.equal(q.next(), null, 'end of the queue');
  assert.equal(q.index, 2, 'index stays at the end');
  assert.equal(q.previous().track.title, 'T2');
  assert.equal(q.previous().track.title, 'T1');
  assert.equal(q.previous(), null, 'start of the queue');
  q.set(tracks(3), { index: 99 });
  assert.equal(q.index, 2, 'clamped');
  q.set([]);
  assert.equal(q.index, -1);
  q.set([null, { title: 'no id' }, T(9)]);
  assert.equal(q.length, 1, 'junk filtered');
});

test('repeat all wraps, repeat one only on automatic advance', () => {
  const q = new PlayQueue();
  q.set(tracks(3), { index: 2 });
  q.setRepeat('all');
  assert.equal(q.peekNext().track.title, 'T1');
  assert.equal(q.next().track.title, 'T1');
  assert.equal(q.previous().track.title, 'T3', 'previous wraps too');
  q.setRepeat('one');
  assert.equal(q.next({ auto: true }).track.title, 'T3', 'ended: same track again');
  assert.equal(q.next(), null, 'manual next at the end with repeat one does not wrap');
  assert.equal(q.index, 2);
  assert.equal(q.setRepeat('bogus'), false);
  assert.equal(q.cycleRepeat(), 'off');
});

test('manual next with repeat one advances; end without repeat stops', () => {
  const q = new PlayQueue();
  q.set(tracks(3), { index: 0 });
  q.setRepeat('one');
  assert.equal(q.next().track.title, 'T2');
  q.setRepeat('off');
  q.jump(2);
  assert.equal(q.peekNext({ auto: true }), null);
});

test('shuffle puts the current first and restores the original order', () => {
  const q = new PlayQueue({ rng: rng(3) });
  q.set(tracks(10), { index: 4 });
  const cur = q.current;
  q.setShuffle(true);
  assert.equal(q.index, 0);
  assert.equal(q.current, cur);
  assert.equal(q.length, 10);
  assert.notDeepEqual(ids(q), ['T5', 'T1', 'T2', 'T3', 'T4', 'T6', 'T7', 'T8', 'T9', 'T10']);
  assert.deepEqual(ids(q).slice().sort(), tracks(10).map((t) => t.title).sort());
  q.next();
  const playing = q.current;
  q.setShuffle(false);
  assert.deepEqual(ids(q), ['T1', 'T2', 'T3', 'T4', 'T5', 'T6', 'T7', 'T8', 'T9', 'T10']);
  assert.equal(q.current, playing, 'the playing entry stays current');
  assert.equal(q.setShuffle(false), false, 'no change');
});

test('set with shuffle and a random start', () => {
  const q = new PlayQueue({ rng: rng(11) });
  q.set(tracks(20), { index: null, shuffle: true });
  assert.equal(q.shuffle, true);
  assert.equal(q.index, 0);
  assert.equal(q.length, 20);
  assert.ok(q.original && q.original.length === 20);
});

test('play next and add to queue, also while shuffled', () => {
  const q = new PlayQueue({ rng: rng(5) });
  q.set(tracks(4), { index: 1 });
  q.insertNext([T(10)], { kind: 'search' });
  assert.deepEqual(ids(q), ['T1', 'T2', 'T10', 'T3', 'T4']);
  q.append([T(11), T(12)]);
  assert.deepEqual(ids(q).slice(-2), ['T11', 'T12']);
  // Shuffled: play next goes right after the current entry, and stays there after unshuffling.
  q.setShuffle(true);
  const cur = q.current.track.title;
  q.insertNext([T(20)]);
  assert.equal(q.entries[q.index + 1].track.title, 'T20');
  q.append([T(21)]);
  assert.equal(q.entries.at(-1).track.title, 'T21');
  q.setShuffle(false);
  const order = ids(q);
  assert.equal(order[order.indexOf(cur) + 1], 'T20');
  assert.equal(order.at(-1), 'T21');
  // An empty queue gets a current entry when tracks are added.
  const e = new PlayQueue();
  e.append([T(1)]);
  assert.equal(e.index, 0);
  assert.deepEqual(e.insertNext([]), []);
});

test('remove keeps the right current entry', () => {
  const q = new PlayQueue();
  q.set(tracks(5), { index: 2 });
  const [a, , c, d] = q.entries;
  assert.deepEqual(q.remove([a.uid]), { removed: 1, currentRemoved: false });
  assert.equal(q.current, c);
  assert.deepEqual(q.remove([c.uid]), { removed: 1, currentRemoved: true });
  assert.equal(q.current, d, 'the next entry becomes current');
  q.jump(q.length - 1);
  const last = q.current;
  assert.deepEqual(q.remove([last.uid]), { removed: 1, currentRemoved: true });
  assert.equal(q.index, q.length - 1, 'at the end the previous entry becomes current');
  assert.deepEqual(q.remove(['nope']), { removed: 0, currentRemoved: false });
  q.remove(q.entries.map((e) => e.uid));
  assert.equal(q.index, -1);
});

test('move, clear upcoming and clear', () => {
  const q = new PlayQueue();
  q.set(tracks(5), { index: 1 });
  const cur = q.current;
  assert.ok(q.move([q.entries[4].uid], 0));
  assert.deepEqual(ids(q), ['T5', 'T1', 'T2', 'T3', 'T4']);
  assert.equal(q.current, cur);
  assert.equal(q.index, 2);
  assert.equal(q.move([q.entries[0].uid], 0), false, 'no-op move');
  assert.ok(q.clearUpcoming());
  assert.deepEqual(ids(q), ['T5', 'T1', 'T2']);
  assert.equal(q.clearUpcoming(), false);
  assert.ok(q.clear());
  assert.equal(q.length, 0);
  assert.equal(q.clear(), false);
});

test('jump and version bumps', () => {
  const q = new PlayQueue();
  q.set(tracks(3));
  const v = q.version;
  assert.equal(q.jump(2).track.title, 'T3');
  assert.ok(q.version > v);
  assert.equal(q.jump(9), null);
  assert.equal(q.jump(1.5), null);
});

test('updateTrack replaces the track of every matching entry', () => {
  const q = new PlayQueue();
  q.set([T(1), T(2), T(1)]);
  const t = { ...T(1), favorite: true };
  assert.ok(q.updateTrack(t));
  assert.equal(q.entries[0].track.favorite, true);
  assert.equal(q.entries[2].track.favorite, true);
  assert.equal(q.updateTrack({ id: 'missing00' }), false);
});

test('cast window: at most max items around the current entry', () => {
  const q = new PlayQueue();
  q.set(tracks(30), { index: 20 });
  assert.deepEqual(q.window(100), { start: 0, entries: q.entries });
  const w = q.window(10);
  assert.equal(w.entries.length, 10);
  assert.ok(w.start <= 20 && w.start + 10 > 20, 'contains the current entry');
  q.jump(0);
  assert.equal(q.window(10).start, 0);
  q.jump(29);
  assert.equal(q.window(10).start, 20);
});

test('toJSON / fromJSON round trip with contexts, shuffle and missing tracks', () => {
  const q = new PlayQueue({ rng: rng(9) });
  q.set(tracks(6), { index: 2, context: { kind: 'playlist', id: 'plist0001' } });
  q.append([T(7)], { kind: 'search' });
  q.setRepeat('all');
  q.setShuffle(true);
  const data = JSON.parse(JSON.stringify(q.toJSON()));
  assert.equal(data.v, 1);
  assert.equal(data.ids.length, 7);
  assert.equal(data.ctx.length, 2, 'contexts are stored once');
  const byId = new Map(tracks(7).map((t) => [t.id, t]));
  const r = PlayQueue.fromJSON(data, byId);
  assert.deepEqual(ids(r), ids(q));
  assert.equal(r.index, q.index);
  assert.equal(r.repeat, 'all');
  assert.equal(r.shuffle, true);
  assert.deepEqual(r.entries.find((e) => e.track.title === 'T7').context, { kind: 'search' });
  r.setShuffle(false);
  assert.deepEqual(ids(r), ['T1', 'T2', 'T3', 'T4', 'T5', 'T6', 'T7'], 'original order restored after reload');
  assert.deepEqual(r.entries[0].context, { kind: 'playlist', id: 'plist0001' });

  // The current track is gone: the next available one becomes current.
  const plain = new PlayQueue();
  plain.set(tracks(4), { index: 1 });
  const d2 = plain.toJSON();
  byId.delete(T(2).id);
  const r2 = PlayQueue.fromJSON(d2, byId);
  assert.deepEqual(ids(r2), ['T1', 'T3', 'T4']);
  assert.equal(r2.current.track.title, 'T3');
  // Everything after it is gone too: the last one.
  const r3 = PlayQueue.fromJSON({ v: 1, ids: [T(1).id, T(2).id], index: 1 }, new Map([[T(1).id, T(1)]]));
  assert.equal(r3.current.track.title, 'T1');
  // Junk input.
  assert.equal(PlayQueue.fromJSON(null, byId).length, 0);
  assert.equal(PlayQueue.fromJSON({ v: 2, ids: [] }, byId).length, 0);
  assert.equal(PlayQueue.fromJSON({ v: 1, ids: [1, null, T(3).id], index: 'x' }, byId).length, 1);
});

test('toJSON keeps at most MAX_STORED entries around the current one', () => {
  const q = new PlayQueue();
  q.set(tracks(MAX_STORED + 200), { index: MAX_STORED + 150 });
  const d = q.toJSON();
  assert.equal(d.ids.length, MAX_STORED);
  assert.equal(d.ids[d.index], q.current.id);
});
