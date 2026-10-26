// Account settings app: /settings/{profile|security|sessions|tokens|appearance}.

import { h } from '../../core/dom.js';
import * as profile from './profile.js';
import * as security from './security.js';
import * as sessions from './sessions.js';
import * as tokens from './tokens.js';
import * as appearance from './appearance.js';

const SECTIONS = [
  { id: 'profile', label: 'Profile', icon: 'user', mod: profile },
  { id: 'security', label: 'Password & 2FA', icon: 'shield-check', mod: security },
  { id: 'sessions', label: 'Sessions', icon: 'laptop', mod: sessions },
  { id: 'tokens', label: 'API tokens', icon: 'key', mod: tokens },
  { id: 'appearance', label: 'Appearance', icon: 'sun', mod: appearance },
];

export function mount(container, ctx) {
  const nav = ctx.ui.tabs({
    items: SECTIONS.map((s) => ({ id: s.id, label: s.label, icon: s.icon, href: `/settings/${s.id}` })),
    variant: 'vertical', ariaLabel: 'Settings sections',
  });
  const content = h('div', { class: 'settings-content' });
  container.append(h('div', { class: 'page' },
    ctx.ui.pageHeader({ title: 'Settings', subtitle: 'Manage your account, security and how Ludworks looks for you.' }),
    h('div', { class: 'settings-layout' }, h('div', { class: 'settings-nav' }, nav.el), content)));

  let active = null;
  let cleanup = null;
  let sectionCtrl = null;

  function show(c) {
    const id = SECTIONS.some((s) => s.id === c.params.rest) ? c.params.rest : 'profile';
    if (!c.params.rest || c.params.rest !== id) {
      c.router.navigate(`/settings/${id}`, { replace: true });
      return;
    }
    if (id === active) return;
    try {
      cleanup?.();
    } catch (err) {
      console.error(err);
    }
    sectionCtrl?.abort();
    sectionCtrl = new AbortController();
    active = id;
    nav.setActive(id);
    const section = SECTIONS.find((s) => s.id === id);
    ctx.setTitle(`${section.label} · Settings`);
    content.replaceChildren();
    const sectionCtx = Object.create(c, { signal: { value: AbortSignal.any ? AbortSignal.any([c.signal, sectionCtrl.signal]) : sectionCtrl.signal } });
    cleanup = section.mod.render(content, sectionCtx) || null;
  }

  show(ctx);
  return {
    update: (c) => show(c),
    unmount: () => {
      sectionCtrl?.abort();
      cleanup?.();
    },
  };
}
