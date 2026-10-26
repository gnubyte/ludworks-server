// Cross-tab auth signals (core/authsync.js). Regression: signing out in one tab left every other
// tab (including an unlocked vault in a side-by-side window) showing the previous user's data,
// because nothing told the other tabs; they only noticed on their next failing API call.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createAuthSync, AUTH_CHANNEL, AUTH_STORAGE_KEY } from '../../assets/js/core/authsync.js';

/** One browser origin: a BroadcastChannel bus plus localStorage that fires 'storage' in the other tabs. */
function fakeOrigin() {
  const channels = new Set();
  class FakeBC {
    constructor(name) {
      this.name = name;
      this.onmessage = null;
      channels.add(this);
    }
    postMessage(data) {
      const copy = structuredClone(data);
      for (const c of channels) if (c !== this && c.name === this.name) queueMicrotask(() => c.onmessage?.({ data: copy }));
    }
    close() { channels.delete(this); }
  }
  const tabs = new Set();
  const map = new Map();
  const tab = () => {
    const listeners = new Set();
    const target = {
      addEventListener: (type, fn) => type === 'storage' && listeners.add(fn),
      removeEventListener: (type, fn) => type === 'storage' && listeners.delete(fn),
      fire: (e) => listeners.forEach((fn) => fn(e)),
    };
    const storage = {
      getItem: (k) => (map.has(k) ? map.get(k) : null),
      setItem(k, v) {
        const old = map.has(k) ? map.get(k) : null;
        map.set(k, String(v));
        for (const t of tabs) if (t !== target) t.fire({ key: k, oldValue: old, newValue: String(v) });
      },
      removeItem(k) {
        const old = map.has(k) ? map.get(k) : null;
        map.delete(k);
        for (const t of tabs) if (t !== target) t.fire({ key: k, oldValue: old, newValue: null });
      },
    };
    tabs.add(target);
    return { target, storage };
  };
  return { FakeBC, tab, map, channels };
}

const tick = () => new Promise((r) => setTimeout(r, 0));

function open(origin, { bc = true, store = true } = {}) {
  const { target, storage } = origin.tab();
  const got = [];
  const sync = createAuthSync({
    onSignal: (s) => got.push(s),
    BroadcastChannel: bc ? origin.FakeBC : null,
    storage: store ? storage : null,
    target,
  });
  return { sync, got, target, storage };
}

test('a sign-out in one tab reaches every other tab once, not itself', async () => {
  const origin = fakeOrigin();
  const a = open(origin);
  const b = open(origin);
  const c = open(origin);
  a.sync.post('signout');
  await tick();
  assert.deepEqual(a.got, [], 'the sender handles its own sign-out directly');
  assert.deepEqual(b.got, [{ type: 'signout', user: '' }], 'delivered once although sent on both paths');
  assert.deepEqual(c.got, [{ type: 'signout', user: '' }]);
  assert.equal(origin.map.has(AUTH_STORAGE_KEY), false, 'nothing is left in localStorage');
  assert.equal([...origin.channels][0].name, AUTH_CHANNEL);
});

test('a sign-in tells the other tabs which account the shared cookie now belongs to', async () => {
  const origin = fakeOrigin();
  const a = open(origin);
  const b = open(origin);
  b.sync.post('session', 'user-B');
  await tick();
  assert.deepEqual(a.got, [{ type: 'session', user: 'user-B' }]);
  assert.throws(() => a.sync.post('pwned'), /unknown auth signal/);
});

test('the storage event alone carries the signal when BroadcastChannel is unavailable', async () => {
  const origin = fakeOrigin();
  const a = open(origin, { bc: false });
  const b = open(origin, { bc: false });
  a.sync.post('signout');
  await tick();
  assert.deepEqual(b.got, [{ type: 'signout', user: '' }], 'removeItem (newValue null) is ignored');
  // And BroadcastChannel alone (storage blocked) works too.
  const o2 = fakeOrigin();
  const c = open(o2, { store: false });
  const d = open(o2, { store: false });
  c.sync.post('signout');
  await tick();
  assert.deepEqual(d.got, [{ type: 'signout', user: '' }]);
});

test('malformed or foreign messages are ignored; blocked storage never throws', async () => {
  const origin = fakeOrigin();
  const a = open(origin);
  const bc = new origin.FakeBC(AUTH_CHANNEL);
  for (const data of [null, 'signout', { v: 2, type: 'signout', nonce: 'n1' }, { v: 1, type: 'other', nonce: 'n2' }, { v: 1, type: 'signout' }, { v: 1, type: 'signout', nonce: '' }]) {
    bc.postMessage(data);
  }
  a.target.fire({ key: 'other.key', newValue: JSON.stringify({ v: 1, type: 'signout', nonce: 'n3' }) });
  a.target.fire({ key: AUTH_STORAGE_KEY, newValue: '{not json' });
  await tick();
  assert.deepEqual(a.got, []);
  // A replayed nonce is handled once.
  const msg = { v: 1, type: 'signout', user: '', from: 'elsewhere', nonce: 'dup' };
  bc.postMessage(msg);
  a.target.fire({ key: AUTH_STORAGE_KEY, newValue: JSON.stringify(msg) });
  await tick();
  assert.equal(a.got.length, 1);

  const blocked = { setItem() { throw new Error('QuotaExceededError'); }, removeItem() { throw new Error('blocked'); } };
  const s = createAuthSync({ onSignal() {}, BroadcastChannel: null, storage: blocked, target: null });
  assert.doesNotThrow(() => s.post('signout'));
  const throwingBC = class { constructor() { throw new Error('SecurityError'); } };
  assert.doesNotThrow(() => createAuthSync({ onSignal() {}, BroadcastChannel: throwingBC, storage: null, target: null }).post('signout'));
});

test('close() stops listening', async () => {
  const origin = fakeOrigin();
  const a = open(origin);
  const b = open(origin);
  b.sync.close();
  a.sync.post('signout');
  await tick();
  assert.deepEqual(b.got, []);
});
