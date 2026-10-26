// Home dashboard: the "Your storage" card follows uploads instead of showing the old total
// until the next visit.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { installDom, settle } from '../files/minidom.mjs';

installDom();
globalThis.location ??= { origin: 'https://nas.example', pathname: '/', search: '', hash: '' };
const ui = await import('../../assets/js/core/ui.js');
const { mount } = await import('../../assets/js/apps/home/index.js');

function fakeUploads() {
  const fns = new Set();
  return {
    on(type, fn) { if (type === 'complete') fns.add(fn); return () => fns.delete(fn); },
    emit(u) { for (const fn of [...fns]) fn(u); },
    get listeners() { return fns.size; },
  };
}

// Regression: uploading from Home left the card at its old total (4.5 KiB after a 3 MiB
// upload) until reload.
test('storage card refreshes after uploads complete, and stops listening on unmount', async () => {
  let usage = 4608;
  let calls = 0;
  const cleanups = [];
  const ac = new AbortController();
  const uploads = fakeUploads();
  const ctx = {
    ui, user: { username: 'pat', role: 'user' }, settings: {}, signal: ac.signal,
    setTitle() {}, actions: { uploadToHome() {} }, uploads,
    onCleanup: (fn) => cleanups.push(fn),
    api: { get: async (p) => { assert.equal(p, '/system/storage'); calls++; return { user: { usage_bytes: usage, quota_bytes: 0, by_category: { other: usage }, trash_bytes: 0 } }; } },
  };
  const container = document.createElement('div');
  document.body.append(container);
  mount(container, ctx);
  await settle(10);
  assert.equal(calls, 1);
  assert.match(container.textContent, /4\.5 KiB/);

  usage = 4608 + 3 * 1024 * 1024;
  uploads.emit({ fileId: 'f1' });
  uploads.emit({ fileId: 'f2' }); // a burst of completions → one refresh
  await new Promise((r) => setTimeout(r, 500));
  await settle(10);
  assert.equal(calls, 2);
  assert.match(container.textContent, /3(\.0)? MiB/);

  for (const fn of cleanups) fn();
  ac.abort();
  assert.equal(uploads.listeners, 0, 'listener removed on unmount');
});
