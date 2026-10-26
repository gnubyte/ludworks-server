// Storage page rendered on a small DOM: the Server card keeps its actions out of the card
// header (they never wrap there and overlapped the "Server" title on phones).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { installDom, settle } from '../files/minidom.mjs';

installDom();
const ui = await import('../../assets/js/core/ui.js');
const { mount } = await import('../../assets/js/apps/system/index.js');

const STORAGE = {
  user: { usage_bytes: 3000, quota_bytes: 0, by_category: { tv: 1000, movie: 2000 }, trash_bytes: 0 },
  volumes: [{ path: '/data', label: 'data, storage', total_bytes: 1000, free_bytes: 400, used_bytes: 600 }],
  spaces: [{ id: 'h1', name: 'Home', kind: 'home', usage_bytes: 3000, file_count: 3 }],
  users: [{ id: 'u1', username: 'pat', usage_bytes: 3000, quota_bytes: 0 }],
  uploads_pending_bytes: 0, data_dir: '/data',
};
const INFO = {
  version: '1.0.0', go_version: 'go1.27', os: 'linux', arch: 'amd64', hostname: 'nas', uptime_seconds: 60,
  started_at: '2026-10-26T10:00:00.000Z', db_size_bytes: 4096, user_count: 1, file_count: 3,
  ffmpeg: { available: false, version: '' }, tls: { mode: 'off', fingerprint_sha256: null, not_after: null },
};

test('server card actions live in the card footer', async () => {
  const container = document.createElement('div');
  document.body.append(container);
  const ctx = {
    ui, isAdmin: true, signal: new AbortController().signal,
    setTitle() {}, setActions() {},
    api: { get: async (p) => (p === '/system/storage' ? STORAGE : INFO), post: async () => ({}) },
  };
  mount(container, ctx);
  await settle(10);
  const cards = container.querySelectorAll('section.card');
  const server = cards.find((c) => c.querySelector('h3')?.textContent === 'Server');
  assert.ok(Boolean(server), `server card among ${cards.map((c) => c.querySelector('h3')?.textContent)}`);
  assert.equal(server.querySelector('.card-actions') === null, true, 'no actions in the header'); // booleans: inspecting DOM nodes in a failure message is very slow
  const footer = server.querySelector('footer.card-footer');
  assert.ok(Boolean(footer), 'footer present');
  assert.match(footer.textContent, /API docs/);
  assert.match(footer.textContent, /Back up database/);
});
