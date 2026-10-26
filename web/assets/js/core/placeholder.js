// "Coming in a later build" placeholder used by app stubs until their module
// engineers replace them. Exported so stubs stay tiny.

import { h } from './dom.js';
import { icon } from './icons.js';
import { button } from './ui.js';

/**
 * Render a polished placeholder for an app that is not shipped yet.
 * @param {HTMLElement} container
 * @param {object} ctx app context (see apps/registry.js)
 * @param {{title: string, icon: string, tint: string, tagline: string, features: string[]}} info
 */
export function comingSoon(container, ctx, info) {
  ctx.setTitle(info.title);
  container.append(h('div', { class: 'page' },
    h('div', { class: 'placeholder' },
      h('div', { class: `placeholder-art tint-${info.tint}` }, icon(info.icon, { size: 32 })),
      h('h1', { class: 'placeholder-title' }, info.title),
      h('p', { class: 'placeholder-tagline' }, info.tagline),
      h('div', { class: 'placeholder-badge' }, h('span', { class: 'badge badge-accent' }, icon('sparkle', 12), 'Arriving in a later build')),
      h('ul', { class: 'placeholder-features' }, info.features.map((f) => h('li', {}, icon('check', { size: 14, className: 'placeholder-check' }), f))),
      h('div', { class: 'placeholder-actions' },
        button({ label: 'Back to Home', icon: 'home', href: '/' }),
        button({ label: 'API docs', icon: 'external', variant: 'ghost', href: '/api/docs', target: '_blank' })))));
}
