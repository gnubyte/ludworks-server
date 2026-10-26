// Preferences, clipboard auto-clear and idle auto-lock (pure logic with injected browser APIs).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { loadPrefs, savePrefs, sanitizePrefs, DEFAULT_PREFS } from '../../assets/js/apps/vault/prefs.js';
import { createClipboard } from '../../assets/js/apps/vault/clipboard.js';
import { createAutoLock } from '../../assets/js/apps/vault/autolock.js';

function memStorage() {
  const m = new Map();
  return { getItem: (k) => (m.has(k) ? m.get(k) : null), setItem: (k, v) => m.set(k, String(v)), map: m };
}

test('prefs: defaults, sanitising, per-user storage, broken storage', () => {
  assert.equal(sanitizePrefs(null).autoLockMinutes, 15);
  const bad = sanitizePrefs({ autoLockMinutes: 7, lockOnHide: 'yes', sort: 'evil', gen: { length: 1000, upper: false, lower: false, digits: false, symbols: false, separator: '\n', words: 2 } });
  assert.equal(bad.autoLockMinutes, 15);
  assert.equal(bad.lockOnHide, false);
  assert.equal(bad.sort, 'name');
  assert.equal(bad.gen.length, DEFAULT_PREFS.gen.length);
  assert.equal(bad.gen.lower, true, 'at least one set stays enabled');
  assert.equal(bad.gen.separator, '-');
  assert.equal(bad.gen.words, 6);
  const st = memStorage();
  savePrefs('u1', { autoLockMinutes: 5, lockOnHide: true, gen: { mode: 'passphrase', words: 8 } }, st);
  assert.equal(loadPrefs('u1', st).autoLockMinutes, 5);
  assert.equal(loadPrefs('u1', st).gen.words, 8);
  assert.equal(loadPrefs('u2', st).autoLockMinutes, 15);
  assert.ok([...st.map.keys()].every((k) => k.startsWith('lw.vault.prefs.')));
  st.setItem('lw.vault.prefs.u3', '{not json');
  assert.equal(loadPrefs('u3', st).autoLockMinutes, 15);
  const throwing = { getItem() { throw new Error('denied'); }, setItem() { throw new Error('denied'); } };
  assert.equal(loadPrefs('x', throwing).sort, 'name');
  assert.equal(savePrefs('x', { sort: 'updated' }, throwing).sort, 'updated');
});

function fakeTimers() {
  let now = 0;
  let seq = 0;
  const timers = new Map();
  return {
    now: () => now,
    setTimer: (fn, ms) => { const id = ++seq; timers.set(id, { fn, at: now + ms }); return id; },
    clearTimer: (id) => timers.delete(id),
    advance(ms) {
      now += ms;
      for (const [id, t] of [...timers]) {
        if (t.at <= now) {
          timers.delete(id);
          t.fn();
        }
      }
    },
    get count() { return timers.size; },
  };
}

test('clipboard: secrets are cleared after 30 s only while focused', async () => {
  const writes = [];
  let focused = true;
  const t = fakeTimers();
  const cb = createClipboard({ write: async (x) => { writes.push(x); }, hasFocus: () => focused, setTimer: t.setTimer, clearTimer: t.clearTimer });
  assert.equal(await cb.copy('s3cret'), true);
  assert.equal(cb.pending, true);
  t.advance(29_999);
  assert.deepEqual(writes, ['s3cret']);
  t.advance(1);
  await new Promise((r) => setImmediate(r));
  assert.deepEqual(writes, ['s3cret', '']);
  assert.equal(cb.pending, false);

  focused = false;
  await cb.copy('other');
  t.advance(30_000);
  await new Promise((r) => setImmediate(r));
  assert.deepEqual(writes, ['s3cret', '', 'other'], 'not cleared without focus');

  // Non-secret copies are not cleared; a new copy re-arms the timer.
  focused = true;
  await cb.copy('plain', { secret: false });
  assert.equal(cb.pending, false);
  await cb.copy('a');
  await cb.copy('b');
  assert.equal(t.count, 1);
  assert.equal(await cb.clearNow(), true);
  assert.equal(writes.at(-1), '');
  // Fallback when the async clipboard API fails.
  const cb2 = createClipboard({ write: async () => { throw new Error('denied'); }, fallback: () => true, hasFocus: () => true, setTimer: t.setTimer, clearTimer: t.clearTimer });
  assert.equal(await cb2.copy('x'), true);
  const cb3 = createClipboard({ write: async () => { throw new Error('denied'); }, fallback: () => false, setTimer: t.setTimer, clearTimer: t.clearTimer });
  assert.equal(await cb3.copy('x'), false);
  cb.dispose();
});

test('auto-lock: idle timeout, activity, hidden tab, stop', () => {
  const doc = new EventTarget();
  doc.visibilityState = 'visible';
  let now = 0;
  let iv = null;
  const locks = [];
  let minutes = 15;
  let onHide = false;
  const al = createAutoLock({
    lock: (why) => locks.push(why), minutes: () => minutes, lockOnHide: () => onHide, doc, now: () => now,
    setInterval: (fn) => { iv = fn; return 1; }, clearInterval: () => { iv = null; },
  });
  al.start();
  assert.equal(al.running, true);
  now = 14 * 60_000;
  iv();
  assert.deepEqual(locks, []);
  doc.dispatchEvent(new Event('keydown'));
  now += 14 * 60_000;
  iv();
  assert.deepEqual(locks, [], 'activity resets the idle timer');
  assert.ok(al.remaining() > 0);
  now += 60_000;
  iv();
  assert.deepEqual(locks, ['idle']);

  locks.length = 0;
  al.touch();
  doc.visibilityState = 'hidden';
  doc.dispatchEvent(new Event('visibilitychange'));
  assert.deepEqual(locks, [], 'lock on hide is off by default');
  onHide = true;
  doc.dispatchEvent(new Event('visibilitychange'));
  assert.deepEqual(locks, ['hidden']);

  // Coming back after the timeout locks immediately (timers are throttled in background tabs).
  locks.length = 0;
  onHide = false;
  minutes = 1;
  now += 5 * 60_000;
  doc.visibilityState = 'visible';
  doc.dispatchEvent(new Event('visibilitychange'));
  assert.deepEqual(locks, ['idle']);

  al.stop();
  assert.equal(al.running, false);
  assert.equal(iv, null);
  locks.length = 0;
  doc.visibilityState = 'hidden';
  onHide = true;
  doc.dispatchEvent(new Event('visibilitychange'));
  assert.deepEqual(locks, [], 'no listeners after stop');
});
