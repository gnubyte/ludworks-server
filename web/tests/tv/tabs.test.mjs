// The TV section tabs scroll horizontally (scrollbar hidden). On a phone the active tab
// (Channels, Integrations) was off-screen; revealActiveTab scrolls it into view.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { installDom, settle } from '../files/minidom.mjs';

installDom();
const { revealActiveTab } = await import('../../assets/js/apps/tv/shared.js');

function strip(width, tabs, activeIndex, scrollLeft = 0) {
  const nav = document.createElement('nav');
  nav.className = 'tabs';
  nav.scrollLeft = scrollLeft;
  Object.defineProperty(nav, 'clientWidth', { value: width });
  Object.defineProperty(nav, 'scrollWidth', { value: tabs * 100 });
  nav.getBoundingClientRect = () => ({ left: 0, right: width, width });
  for (let i = 0; i < tabs; i++) {
    const a = document.createElement('a');
    a.className = i === activeIndex ? 'tab is-active' : 'tab';
    a.getBoundingClientRect = () => ({ left: i * 100 - nav.scrollLeft, right: (i + 1) * 100 - nav.scrollLeft, width: 100 });
    nav.append(a);
  }
  return nav;
}

test('scrolls an off-screen active tab into view', async () => {
  const nav = strip(375, 6, 4); // tab 5 spans 400–500 in a 375 px strip
  revealActiveTab(nav);
  await settle();
  assert.equal(nav.scrollLeft, 500 - 375 + 16);
});

test('scrolls back for a tab left of the visible part', async () => {
  const nav = strip(375, 6, 0, 300);
  revealActiveTab(nav);
  await settle();
  assert.equal(nav.scrollLeft, 300 - 316);
});

test('leaves visible tabs and non-overflowing strips alone', async () => {
  const nav = strip(375, 6, 1, 0);
  revealActiveTab(nav);
  const wide = strip(800, 6, 5);
  revealActiveTab(wide);
  await settle();
  assert.equal(nav.scrollLeft, 0);
  assert.equal(wide.scrollLeft, 0);
});

test('scrolls the wrapper when the tab list itself does not overflow', async () => {
  // The TV bar: <div class="tv-tabs-inner" (overflow-x: auto)><nav class="tabs"> (full width).
  const wrap = document.createElement('div');
  wrap.scrollLeft = 0;
  Object.defineProperty(wrap, 'clientWidth', { value: 375 });
  Object.defineProperty(wrap, 'scrollWidth', { value: 600 });
  wrap.getBoundingClientRect = () => ({ left: 0, right: 375, width: 375 });
  const nav = document.createElement('nav');
  Object.defineProperty(nav, 'clientWidth', { value: 600 });
  Object.defineProperty(nav, 'scrollWidth', { value: 600 });
  const a = document.createElement('a');
  a.className = 'tab is-active';
  a.getBoundingClientRect = () => ({ left: 500 - wrap.scrollLeft, right: 600 - wrap.scrollLeft, width: 100 });
  nav.append(a);
  wrap.append(nav);
  revealActiveTab(nav);
  await settle();
  assert.equal(wrap.scrollLeft, 600 - 375 + 16);
});
