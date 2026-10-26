// Application shell: collapsible sidebar (drawer on small screens), top bar with
// page title, app actions, global search, theme toggle and user menu.

import { h, cls, debounce, on } from './dom.js';
import { icon, logoMark } from './icons.js';
import { button, dropdown, avatar } from './ui.js';
import { displayName } from './store.js';
import { effectiveTheme, toggleTheme, onThemeChange, getThemePref, setThemePref } from './theme.js';
import { openPalette } from './palette.js';
import { comboLabel } from './shortcuts.js';

const SIDEBAR_KEY = 'lw.sidebar';

function loadCollapsed() {
  try {
    return localStorage.getItem(SIDEBAR_KEY) === 'collapsed';
  } catch {
    return false;
  }
}

/**
 * Create the shell.
 * @param {{apps: Array, user: object, serverName: string, version?: string, onSignOut: () => void,
 *          onShortcuts: () => void, docsUrl: string}} o
 */
export function createShell(o) {
  let user = o.user;
  let apps = o.apps;
  let collapsed = loadCollapsed();
  let searchCfg = null;
  const navLinks = new Map();

  /* ---------------------------------------------------------------- sidebar */
  const nav = h('nav', { class: 'nav', aria: { label: 'Apps' } });
  const brand = h('a', { class: 'sidebar-brand', href: '/', aria: { label: `${o.serverName} home` } },
    logoMark(28), h('span', { class: 'sidebar-brand-text' }, h('span', { class: 'sidebar-brand-name' }, o.serverName || 'Ludworks')));
  const collapseBtn = h('button', { class: 'nav-item nav-collapse', type: 'button', aria: { label: 'Collapse sidebar', expanded: 'true' } },
    icon('chevrons-left', { className: 'nav-icon' }), h('span', { class: 'nav-label' }, 'Collapse'));
  const versionEl = h('div', { class: 'sidebar-version' }, o.version ? `v${o.version}` : '');
  const sidebar = h('aside', { class: 'sidebar', id: 'sidebar', aria: { label: 'Sidebar' } },
    brand, nav, h('div', { class: 'sidebar-footer' }, collapseBtn, versionEl));
  const backdrop = h('div', { class: 'nav-backdrop', aria: { hidden: 'true' } });

  function buildNav() {
    nav.replaceChildren();
    navLinks.clear();
    const groups = [
      ['Apps', apps.filter((a) => ['home', 'files', 'tv', 'notes', 'vault', 'sheets'].includes(a.id))],
      ['System', apps.filter((a) => ['system', 'settings', 'admin'].includes(a.id))],
    ];
    for (const [label, list] of groups) {
      if (!list.length) continue;
      nav.append(h('div', { class: 'nav-section-label' }, label));
      for (const a of list) {
        const link = h('a', {
          class: cls('nav-item', `tint-${a.tint}`), href: a.path, dataset: { app: a.id, tipPlacement: 'right' },
        }, icon(a.icon, { className: 'nav-icon', size: 18 }), h('span', { class: 'nav-label' }, a.title));
        navLinks.set(a.id, link);
        nav.append(link);
      }
    }
    applyCollapsed();
  }

  function applyCollapsed() {
    shellEl.classList.toggle('sidebar-collapsed', collapsed);
    collapseBtn.replaceChildren(icon(collapsed ? 'chevrons-right' : 'chevrons-left', { className: 'nav-icon' }), h('span', { class: 'nav-label' }, 'Collapse'));
    collapseBtn.setAttribute('aria-label', collapsed ? 'Expand sidebar' : 'Collapse sidebar');
    collapseBtn.setAttribute('aria-expanded', String(!collapsed));
    collapseBtn.dataset.tip = collapsed ? 'Expand sidebar' : '';
    collapseBtn.dataset.tipPlacement = 'right';
    for (const [id, link] of navLinks) {
      const a = apps.find((x) => x.id === id);
      if (collapsed) {
        link.dataset.tip = a.title;
        link.setAttribute('aria-label', a.title);
      } else {
        delete link.dataset.tip;
        link.removeAttribute('aria-label');
      }
    }
  }

  collapseBtn.addEventListener('click', () => {
    collapsed = !collapsed;
    try {
      localStorage.setItem(SIDEBAR_KEY, collapsed ? 'collapsed' : 'expanded');
    } catch { /* ignore */ }
    applyCollapsed();
  });

  /* ----------------------------------------------------------------- topbar */
  const navToggle = button({ icon: 'menu', title: 'Open navigation', variant: 'ghost', className: 'nav-toggle' });
  navToggle.setAttribute('aria-controls', 'sidebar');
  navToggle.setAttribute('aria-expanded', 'false');
  const titleEl = h('h1', { class: 'topbar-title', id: 'page-title' });
  const actionsEl = h('div', { class: 'topbar-actions' });

  const searchInput = h('input', {
    class: 'input topbar-search-input', type: 'search', placeholder: 'Search…', autocomplete: 'off', spellcheck: 'false',
    aria: { label: 'Search', keyshortcuts: '/' },
  });
  const searchHint = h('kbd', { class: 'topbar-search-kbd', aria: { hidden: 'true' } }, comboLabel('mod+k'));
  const searchBox = h('div', { class: 'topbar-search' }, icon('search', { className: 'topbar-search-icon' }), searchInput, searchHint);
  const searchBtn = button({ icon: 'search', title: 'Search', variant: 'ghost', className: 'topbar-search-btn' });
  const themeBtn = button({ icon: effectiveTheme() === 'dark' ? 'sun' : 'moon', title: 'Toggle theme', variant: 'ghost' });
  const userBtn = h('button', { class: 'user-button', type: 'button', aria: { haspopup: 'menu', expanded: 'false', label: 'Account menu' } });
  const topbar = h('header', { class: 'topbar' }, navToggle, titleEl, actionsEl, searchBox, searchBtn, themeBtn, userBtn);

  const view = h('main', { class: 'view', id: 'main', tabindex: '-1' });
  const skip = h('a', { class: 'skip-link', href: '#main', dataset: { native: '' } }, 'Skip to content');
  skip.addEventListener('click', (e) => {
    e.preventDefault();
    view.focus();
  });
  const main = h('div', { class: 'main' }, topbar, view);
  const shellEl = h('div', { class: 'shell' }, skip, sidebar, backdrop, main);

  function renderUser() {
    userBtn.replaceChildren(avatar({ name: displayName(user) || user?.username, size: 28 }));
    userBtn.dataset.tip = displayName(user);
  }

  function themeIcon() {
    const dark = effectiveTheme() === 'dark';
    themeBtn.replaceChildren(icon(dark ? 'sun' : 'moon'));
    themeBtn.dataset.tip = dark ? 'Switch to light theme' : 'Switch to dark theme';
    themeBtn.setAttribute('aria-label', themeBtn.dataset.tip);
  }
  themeBtn.addEventListener('click', toggleTheme);
  const offTheme = onThemeChange(themeIcon);
  themeIcon();

  userBtn.addEventListener('click', () => {
    const pref = getThemePref();
    dropdown(userBtn, [
      { heading: `${displayName(user)}${user?.email ? ` · ${user.email}` : ''}` },
      { label: 'Account settings', icon: 'user', onSelect: () => o.navigate('/settings/profile') },
      { label: 'Security & 2FA', icon: 'shield-check', onSelect: () => o.navigate('/settings/security') },
      { label: 'API tokens', icon: 'key', onSelect: () => o.navigate('/settings/tokens') },
      { separator: true },
      { label: 'System theme', checked: pref === 'system', onSelect: () => setThemePref('system') },
      { label: 'Light theme', checked: pref === 'light', onSelect: () => setThemePref('light') },
      { label: 'Dark theme', checked: pref === 'dark', onSelect: () => setThemePref('dark') },
      { separator: true },
      { label: 'Keyboard shortcuts', icon: 'keyboard', shortcut: '?', onSelect: () => o.onShortcuts() },
      { label: 'API reference', icon: 'code', onSelect: () => window.open('/api/docs', '_blank', 'noopener') },
      { label: 'Documentation', icon: 'book', onSelect: () => window.open(o.docsUrl, '_blank', 'noopener,noreferrer') },
      { separator: true },
      { label: 'Sign out', icon: 'log-out', danger: true, onSelect: () => o.onSignOut() },
    ], { align: 'end' });
  });

  /* ------------------------------------------------------------ nav drawer */
  const openNav = () => {
    shellEl.classList.add('nav-open');
    navToggle.setAttribute('aria-expanded', 'true');
    requestAnimationFrame(() => nav.querySelector('.nav-item')?.focus());
  };
  const closeNav = () => {
    if (!shellEl.classList.contains('nav-open')) return;
    shellEl.classList.remove('nav-open');
    navToggle.setAttribute('aria-expanded', 'false');
  };
  navToggle.addEventListener('click', () => (shellEl.classList.contains('nav-open') ? closeNav() : openNav()));
  backdrop.addEventListener('click', closeNav);
  const offKey = on(document, 'keydown', (e) => {
    if (e.key === 'Escape' && shellEl.classList.contains('nav-open')) closeNav();
  });

  /* ----------------------------------------------------------------- search */
  const fireInput = debounce((v) => searchCfg?.onInput?.(v), 220);
  searchInput.addEventListener('input', () => {
    if (searchCfg) {
      fireInput(searchInput.value);
    } else {
      const v = searchInput.value;
      searchInput.value = '';
      openPalette(v);
    }
  });
  searchInput.addEventListener('keydown', (e) => {
    if (e.key === 'Enter') {
      e.preventDefault();
      if (searchCfg) {
        fireInput.cancel();
        searchCfg.onSubmit ? searchCfg.onSubmit(searchInput.value) : searchCfg.onInput?.(searchInput.value);
      } else {
        openPalette(searchInput.value);
        searchInput.value = '';
      }
    } else if (e.key === 'Escape' && searchInput.value) {
      e.preventDefault();
      e.stopPropagation();
      searchInput.value = '';
      fireInput.cancel();
      searchCfg?.onInput?.('');
    } else if (e.key === 'Escape') {
      searchInput.blur();
      topbar.classList.remove('search-expanded');
    }
  });
  searchInput.addEventListener('blur', () => {
    if (!searchInput.value) topbar.classList.remove('search-expanded');
  });
  searchBtn.addEventListener('click', () => {
    if (searchCfg) {
      topbar.classList.add('search-expanded');
      searchInput.focus();
    } else {
      openPalette('');
    }
  });

  buildNav();
  renderUser();

  return {
    el: shellEl,
    view,
    setTitle(text) {
      titleEl.textContent = text || '';
    },
    setActions(nodes) {
      actionsEl.replaceChildren(...(Array.isArray(nodes) ? nodes : nodes ? [nodes] : []));
    },
    setSearch(cfg) {
      searchCfg = cfg || null;
      fireInput.cancel();
      searchInput.value = cfg?.value || '';
      searchInput.placeholder = cfg?.placeholder || 'Search or jump to…';
      searchBox.classList.toggle('is-app-search', !!cfg);
      if (!cfg) topbar.classList.remove('search-expanded');
    },
    /** Programmatically focus the search box (the "/" shortcut). */
    focusSearch() {
      if (getComputedStyle(searchBox).display === 'none') searchBtn.click();
      else searchInput.focus();
    },
    setLayout(opts = {}) {
      view.classList.toggle('view-full', !!opts.full);
      shellEl.classList.toggle('layout-full', !!opts.full);
    },
    setActive(appId) {
      for (const [id, link] of navLinks) {
        if (id === appId) link.setAttribute('aria-current', 'page');
        else link.removeAttribute('aria-current');
      }
    },
    updateUser(u, newApps) {
      user = u;
      if (newApps) {
        apps = newApps;
        buildNav();
      }
      renderUser();
    },
    setServerName(name) {
      brand.querySelector('.sidebar-brand-name').textContent = name || 'Ludworks';
    },
    openNav,
    closeNav,
    destroy() {
      offTheme();
      offKey();
      shellEl.remove();
    },
  };
}
