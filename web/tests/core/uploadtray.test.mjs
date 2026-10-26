// The upload tray lifts the toast region by its own height (--tray-offset).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { installDom, settle } from '../files/minidom.mjs';

installDom();
const observers = [];
globalThis.ResizeObserver = class {
  constructor(cb) { this.cb = cb; this.targets = []; observers.push(this); }
  observe(t) { this.targets.push(t); }
  disconnect() { this.targets = []; }
};
globalThis.window ??= globalThis;
window.addEventListener ??= () => {};
window.removeEventListener ??= () => {};
const { createUploadTray } = await import('../../assets/js/core/uploadtray.js');

function fakeQueue(uploads) {
  return {
    uploads, busy: true,
    totals: { counts: { uploading: uploads.length, retrying: 0, queued: 0, error: 0, paused: 0 }, bytesUploaded: 0, bytesTotal: 10, speed: 0 },
    on() { return () => {}; },
  };
}

// Regression: the offset was measured only when the tray rendered (before its rows were laid
// out and its slide-in finished), so "Moved 2 items to the trash" covered "Uploads complete".
test('toast offset follows the tray size as it changes after rendering', async () => {
  const up = { name: 'a.bin', state: 'uploading', progress: { bytesUploaded: 0, bytesTotal: 10, speed: 0, eta: null } };
  const tray = createUploadTray(fakeQueue([up]));
  const offset = () => document.documentElement.style.getPropertyValue('--tray-offset');
  let height = 0;
  Object.defineProperty(tray.el, 'offsetHeight', { get: () => height });
  const ro = observers.find((o) => o.targets.includes(tray.el));
  assert.ok(ro, 'tray observed for size changes');

  height = 60; // header only, mid-animation
  ro.cb([{ target: tray.el }]);
  assert.equal(offset(), '72px');
  height = 190; // rows laid out
  ro.cb([{ target: tray.el }]);
  assert.equal(offset(), '202px');
  tray.el.hidden = true;
  ro.cb([{ target: tray.el }]);
  assert.equal(offset(), '0px');

  tray.destroy();
  assert.equal(ro.targets.length, 0, 'observer disconnected');
  assert.equal(offset(), '0px');
  await settle(2);
});

// Regression: sign-out only paused the queue, so the tray stayed on the login screen listing the
// previous user's file names with Resume buttons. reset() must empty it and hide the tray.
test('the tray hides and drops every row when the queue is reset (sign-out)', async () => {
  const { UploadQueue, FingerprintStore } = await import('../../assets/js/core/upload.js');
  const mem = new Map();
  const storage = { getItem: (k) => (mem.has(k) ? mem.get(k) : null), setItem: (k, v) => mem.set(k, String(v)), removeItem: (k) => mem.delete(k) };
  const pending = () => new Promise(() => {}); // requests never answer: uploads stay in flight
  const q = new UploadQueue({ transport: pending, store: new FingerprintStore(storage), scope: 'user-A', checksum: false }, { concurrency: 1 });
  const tray = createUploadTray(q);
  q.add(new File([new Uint8Array(10)], 'previous-user-secret.pdf'), { spaceId: 's1' });
  const b = q.add(new File([new Uint8Array(10)], 'second.pdf'), { spaceId: 's1' });
  q.pause(b);
  await settle(3);
  assert.equal(tray.el.hidden, false);
  assert.match(tray.el.textContent, /previous-user-secret\.pdf/);

  q.reset();
  await settle(3);
  assert.equal(tray.el.hidden, true, 'tray hidden');
  assert.equal(tray.el.textContent.includes('previous-user-secret.pdf'), false, 'file names gone from the DOM');
  assert.equal(tray.el.querySelectorAll('.upload-item').length, 0);
  tray.destroy();
});
