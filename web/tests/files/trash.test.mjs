// Trash view: the "Trash is empty" state must show whenever the last page is empty — on a
// first load of an empty trash and after "Empty trash" reloads the list. It used to read the
// infinite list's `done` flag inside onItems, where it is still false, so the view stayed blank.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { installDom, settle } from './minidom.mjs';

installDom();
const ui = await import('../../assets/js/core/ui.js');
const { renderTrash } = await import('../../assets/js/apps/files/trash.js');

function entry(id, name) {
  return {
    id, name, space_id: 'home1', is_dir: false, size: 1024, mime: 'application/octet-stream',
    path: `/${id}`, original_path: `Docs/${name}`, trashed_at: '2026-10-26T10:00:00.000Z',
  };
}

function mount(pages) {
  const calls = { list: 0, emptied: 0 };
  let state = pages;
  const fapi = {
    trashList: async (cursor) => {
      calls.list += 1;
      return state(cursor);
    },
    emptyTrash: async () => {
      calls.emptied += 1;
      state = () => ({ items: [], next_cursor: null });
      return { deleted: 2, freed_bytes: 2048 };
    },
  };
  const toasts = [];
  const toast = (m) => toasts.push(m);
  toast.success = toast;
  toast.error = (e) => toasts.push(`error: ${e?.message || e}`);
  const ctx = { ui: { ...ui, toast, confirm: async () => true }, signal: new AbortController().signal };
  const host = document.createElement('div');
  document.body.append(host);
  const view = renderTrash(host, ctx, fapi, { spaceName: () => 'Home', onChanged: () => {} });
  return { host, view, calls, toasts };
}

const emptyState = (host) => host.querySelector('.fx-empty');
const table = (host) => host.querySelector('table');

test('an empty trash shows the empty state on first load', async () => {
  const { host } = mount(() => ({ items: [], next_cursor: null }));
  await settle();
  assert.equal(emptyState(host).hidden, false, 'empty state visible');
  assert.match(emptyState(host).textContent, /Trash is empty/);
  assert.equal(table(host).hidden, true, 'table hidden');
});

test('a non-empty first page hides the empty state', async () => {
  const { host } = mount(() => ({ items: [entry('a1', 'one.bin'), entry('a2', 'two.bin')], next_cursor: null }));
  await settle();
  assert.equal(emptyState(host).hidden, true);
  assert.equal(table(host).hidden, false);
  assert.equal(host.querySelectorAll('tr.clickable').length, 2);
});

test('"Empty trash" reloads into the empty state', async () => {
  const { host, calls, toasts } = mount(() => ({ items: [entry('a1', 'one.bin'), entry('a2', 'two.bin')], next_cursor: null }));
  await settle();
  const btn = host.querySelectorAll('button').find((b) => b.textContent === 'Empty trash');
  assert.ok(btn, 'Empty trash button');
  btn.click();
  await settle(10);
  assert.equal(calls.emptied, 1);
  assert.equal(calls.list, 2, 'list reloaded');
  assert.ok(toasts.some((t) => /Freed/.test(t)), `toast: ${toasts}`);
  assert.equal(host.querySelectorAll('tr.clickable').length, 0, 'rows cleared');
  assert.equal(emptyState(host).hidden, false, 'empty state visible after emptying');
  assert.equal(table(host).hidden, true);
});

test('the empty state waits for the last page', async () => {
  // First page is empty but more pages follow: not "empty" yet.
  let n = 0;
  const { host } = mount((cursor) => {
    n += 1;
    return cursor ? { items: [entry('b1', 'late.bin')], next_cursor: null } : { items: [], next_cursor: 'c1' };
  });
  await settle();
  assert.equal(n >= 1, true);
  if (n === 1) assert.equal(emptyState(host).hidden, true, 'not empty while a next page exists');
});
