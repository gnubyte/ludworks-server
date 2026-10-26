// Core tab strips hide their scrollbar. Regression: setActive never scrolled the active tab into
// view (Admin's nine tabs below ~1100 px, TV at 375 px), and nothing hinted at hidden tabs.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { installDom, settle } from '../files/minidom.mjs';

installDom();
const ui = await import('../../assets/js/core/ui.js');

function geometry(nav, width) {
  const tabs = [...nav.querySelectorAll('.tab')];
  let left = 0; // browsers clamp scrollLeft to [0, scrollWidth - clientWidth]
  Object.defineProperty(nav, 'scrollLeft', { get: () => left, set: (v) => { left = Math.max(0, Math.min(v, tabs.length * 100 - width)); } });
  Object.defineProperty(nav, 'clientWidth', { value: width });
  Object.defineProperty(nav, 'scrollWidth', { value: tabs.length * 100 });
  nav.getBoundingClientRect = () => ({ left: 0, right: width, width });
  tabs.forEach((a, i) => { a.getBoundingClientRect = () => ({ left: i * 100 - nav.scrollLeft, right: (i + 1) * 100 - nav.scrollLeft, width: 100 }); });
}

test('setActive scrolls the active tab into view and fades the edges with hidden tabs', async () => {
  const items = Array.from({ length: 9 }, (_, i) => ({ id: `t${i}`, label: `Tab ${i}`, href: `/admin/t${i}` }));
  const t = ui.tabs({ items, active: 't0' });
  document.body.append(t.el);
  geometry(t.el, 375);
  t.setActive('t7'); // spans 700–800 in a 375 px strip
  assert.equal(t.el.scrollLeft, 800 - 375 + 16);
  await settle(5);
  assert.equal(t.el.classList.contains('fade-start'), true);
  assert.equal(t.el.classList.contains('fade-end'), true);
  t.setActive('t0');
  assert.equal(t.el.scrollLeft, 0);
  await settle(5);
  assert.equal(t.el.classList.contains('fade-start'), false);
  assert.equal(t.el.classList.contains('fade-end'), true);
});

test('a strip that fits gets no scrolling and no fades', async () => {
  const t = ui.tabs({ items: [{ id: 'a', label: 'A' }, { id: 'b', label: 'B' }], active: 'a' });
  document.body.append(t.el);
  geometry(t.el, 800);
  t.setActive('b');
  await settle(5);
  assert.equal(t.el.scrollLeft, 0);
  assert.equal(t.el.classList.contains('fade-start') || t.el.classList.contains('fade-end'), false);
});
