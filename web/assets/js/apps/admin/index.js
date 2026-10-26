// Admin app (/admin/:tab, admins only): users, server settings by namespace,
// libraries (shared spaces), category taxonomy and the audit log.

import { h } from '../../core/dom.js';
import { revealActiveTab } from '../../core/ui.js';
import * as users from './users.js';
import * as settingsForms from './settings-forms.js';
import * as libraries from './libraries.js';
import * as categories from './categories.js';
import * as audit from './audit.js';

const TABS = [
  { id: 'users', label: 'Users', icon: 'users', render: users.render },
  { id: 'general', label: 'General', icon: 'sliders', render: (host, c, s) => settingsForms.render(host, c, s, 'general') },
  { id: 'security', label: 'Security', icon: 'shield', render: (host, c, s) => settingsForms.render(host, c, s, 'security') },
  { id: 'email', label: 'Email', icon: 'mail', render: (host, c, s) => settingsForms.render(host, c, s, 'email') },
  { id: 'media', label: 'Media & TV', icon: 'film', render: (host, c, s) => settingsForms.render(host, c, s, 'media') },
  { id: 'files', label: 'Files', icon: 'folder', render: (host, c, s) => settingsForms.render(host, c, s, 'files') },
  { id: 'libraries', label: 'Libraries', icon: 'library', render: libraries.render },
  { id: 'categories', label: 'Categories', icon: 'tag', render: categories.render },
  { id: 'audit', label: 'Audit log', icon: 'history', render: audit.render },
];

/**
 * Shared admin state: the redacted settings document (GET /admin/settings) is
 * loaded once and refreshed from every PATCH response.
 */
function createState(api) {
  let settings = null;
  let pending = null;
  return {
    get settings() { return settings; },
    async loadSettings(force = false) {
      if (settings && !force) return settings;
      pending ??= api.get('/admin/settings').then((s) => {
        settings = s;
        return s;
      }).finally(() => { pending = null; });
      return pending;
    },
    setSettings(s) {
      settings = s;
    },
  };
}

export function mount(container, ctx) {
  const { ui } = ctx;
  const state = createState(ctx.api);
  const nav = ui.tabs({
    items: TABS.map((t) => ({ id: t.id, label: t.label, icon: t.icon, href: `/admin/${t.id}` })),
    ariaLabel: 'Admin sections',
  });
  const content = h('div', { class: 'admin-content mt-6' });
  container.append(h('div', { class: 'page wide' },
    ui.pageHeader({ title: 'Administration', subtitle: 'Manage people, policies and integrations for this server.' }),
    nav.el, content));

  let active = null;
  let cleanup = null;
  let tabCtrl = null;

  function show(c) {
    const tab = TABS.find((t) => t.id === c.params.rest) || null;
    if (!tab) {
      c.router.navigate('/admin/users', { replace: true });
      return;
    }
    if (tab.id === active) {
      cleanup?.update?.(c);
      return;
    }
    try {
      (typeof cleanup === 'function' ? cleanup : cleanup?.unmount)?.();
    } catch (err) {
      console.error(err);
    }
    tabCtrl?.abort();
    tabCtrl = new AbortController();
    active = tab.id;
    nav.setActive(tab.id);
    revealActiveTab(nav.el);
    ctx.setTitle(`${tab.label} · Admin`);
    ctx.router.setLeaveGuard(null);
    content.replaceChildren();
    const tabCtx = Object.create(c, { signal: { value: AbortSignal.any ? AbortSignal.any([c.signal, tabCtrl.signal]) : tabCtrl.signal } });
    cleanup = tab.render(content, tabCtx, state) || null;
  }

  show(ctx);
  return {
    update: (c) => show(c),
    unmount: () => {
      tabCtrl?.abort();
      (typeof cleanup === 'function' ? cleanup : cleanup?.unmount)?.();
    },
  };
}
