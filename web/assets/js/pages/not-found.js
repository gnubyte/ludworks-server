// 404 page (inside the shell when signed in, standalone otherwise).

import { h } from '../core/dom.js';
import { button } from '../core/ui.js';

export function mount(container, ctx) {
  ctx.setTitle('Not found');
  const body = h('div', { class: 'notfound' },
    h('div', { class: 'notfound-code', aria: { hidden: 'true' } }, '404'),
    h('h1', { class: 'notfound-title' }, 'This page doesn’t exist'),
    h('p', { class: 'notfound-msg' }, 'The link may be broken, or the page may have moved. Check the address or head back home.'),
    h('div', { class: 'row wrap' },
      button({ label: 'Go home', icon: 'home', variant: 'primary', href: '/' }),
      button({ label: 'Go back', icon: 'arrow-left', onClick: () => history.back() })));
  container.replaceChildren(ctx.user ? h('div', { class: 'page' }, body) : h('div', { class: 'auth' }, body));
}
