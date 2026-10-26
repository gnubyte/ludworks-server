// Ludworks web UI entry point: boot (setup status + session), routing, guards,
// layout switching (auth pages vs. app shell), app mounting with a scoped ctx,
// global shortcuts, command palette and the shared upload queue.

import { h, pickFiles } from './core/dom.js';
import { icon, logoMark } from './core/icons.js';
import { api } from './core/api.js';
import { Router, buildQuery, safeNext } from './core/router.js';
import { store, setSession, clearSession, isAdmin } from './core/store.js';
import { initTheme, toggleTheme, setThemePref } from './core/theme.js';
import * as ui from './core/ui.js';
import { initShortcuts, registerShortcut, listShortcuts } from './core/shortcuts.js';
import { openPalette, registerCommands, isPaletteOpen } from './core/palette.js';
import { createShell } from './core/shell.js';
import { UploadQueue, FingerprintStore } from './core/upload.js';
import { createUploadTray } from './core/uploadtray.js';
import { createAuthSync } from './core/authsync.js';
import { apps, visibleApps } from './apps/registry.js';

/** ReadTheDocs site for the project (linked from the user menu and Home). */
export const DOCS_URL = 'https://ludworks.readthedocs.io/';

const PAGES = {
  setup: () => import('./pages/setup.js'),
  login: () => import('./pages/login.js'),
  register: () => import('./pages/register.js'),
  forgot: () => import('./pages/forgot-password.js'),
  reset: () => import('./pages/reset-password.js'),
  verify: () => import('./pages/verify-email.js'),
  notfound: () => import('./pages/not-found.js'),
};

const routes = [
  { path: '/setup', page: 'setup', layout: 'auth' },
  { path: '/login', page: 'login', layout: 'auth', guestOnly: true },
  { path: '/register', page: 'register', layout: 'auth', guestOnly: true },
  { path: '/forgot-password', page: 'forgot', layout: 'auth', guestOnly: true },
  { path: '/reset-password', page: 'reset', layout: 'auth' },
  { path: '/verify-email', page: 'verify', layout: 'auth' },
  ...apps.map((a) => ({ path: a.path === '/' ? '/' : `${a.path}/*rest`, app: a.id, auth: true })),
  { path: '*', page: 'notfound', layout: 'auto' },
];

const root = document.getElementById('app');
let router = null;
let shell = null;
let current = null; // {kind, id, scope, unmount, update}
let renderToken = 0;

/* ------------------------------------------------------------ upload queue */

const fingerprints = new FingerprintStore();
const uploads = new UploadQueue({
  headers: () => {
    const csrf = store.get().csrf;
    return csrf ? { 'X-CSRF-Token': csrf } : {};
  },
  store: fingerprints,
  scope: '',
}, { concurrency: 3 });

/* ------------------------------------------------------------------ helpers */

function serverName() {
  return store.get().status?.server_name || 'Ludworks';
}

function setTitle(text) {
  document.title = text ? `${text} · ${serverName()}` : serverName();
  shell?.setTitle(text || '');
}

/** Per-mount scope: abort signal + cleanup callbacks run on unmount. */
function createScope() {
  const ctrl = new AbortController();
  const fns = [];
  return {
    signal: ctrl.signal,
    add(fn) {
      fns.push(fn);
      return fn;
    },
    dispose() {
      ctrl.abort();
      while (fns.length) {
        try {
          fns.pop()();
        } catch (err) {
          console.error('cleanup failed', err);
        }
      }
    },
  };
}

function teardown() {
  if (!current) return;
  const c = current;
  current = null;
  try {
    c.unmount?.();
  } catch (err) {
    console.error('unmount failed', err);
  }
  c.scope.dispose();
}

/** Upload files picked by the user into the root of their home space. */
async function uploadToHome(files) {
  const list = files || await pickFiles({ multiple: true });
  if (!list.length) return [];
  let spaces;
  try {
    spaces = await api.get('/spaces');
  } catch (err) {
    ui.toast.error(err);
    return [];
  }
  const home = (Array.isArray(spaces) ? spaces : spaces?.items || []).find((s) => s.kind === 'home');
  if (!home) {
    ui.toast.error('No home space is available for your account.');
    return [];
  }
  const ups = list.map((f) => uploads.add(f, { spaceId: home.id, path: '' }));
  ui.toast.info(`Uploading ${list.length} ${list.length === 1 ? 'file' : 'files'} to Home`, {
    action: { label: 'Open Files', onClick: () => router.navigate('/files') },
  });
  return ups;
}

/**
 * Forget everything this tab holds for the signed-in user, without network calls: the upload
 * queue (file names, File handles, target spaces), the resume fingerprints in localStorage and
 * the session itself. Clearing the session tears down the open app and the shell and locks the
 * vault (store subscribers).
 */
function dropLocalSession() {
  uploads.reset();
  fingerprints.clear(); // file names/paths of past uploads must not outlive the session on shared machines
  clearSession();
}

async function signOut() {
  if (uploads.uploads.some((u) => !['completed', 'cancelled'].includes(u.state))) {
    const ok = await ui.confirm({
      title: 'Uploads not finished',
      message: 'Signing out cancels your unfinished uploads. Sign in and add the files again to upload them.',
      confirmLabel: 'Sign out anyway', danger: true,
    });
    if (!ok) return;
  }
  // Delete partial uploads on the server while this session can still authorise that, without
  // letting a stalled connection hold up the sign-out.
  await Promise.race([uploads.reset({ terminate: true }), new Promise((r) => setTimeout(r, 3000))]);
  try {
    await api.post('/auth/logout', null, { noRedirect: true });
  } catch {
    /* the session may already be gone; sign out locally regardless */
  }
  dropLocalSession();
  authSync?.post('signout'); // the cookie is shared: sign out the other tabs too
  await router.navigate('/login', { replace: true });
  ui.toast.success('Signed out');
}

function showShortcuts() {
  const groups = new Map();
  for (const s of listShortcuts()) {
    if (!groups.has(s.group)) groups.set(s.group, []);
    groups.get(s.group).push(s);
  }
  ui.modal({
    title: 'Keyboard shortcuts',
    size: 'md',
    body: h('div', { class: 'shortcut-groups' }, [...groups].map(([g, list]) => h('section', { class: 'shortcut-group' },
      h('h3', { class: 'shortcut-group-title' }, g),
      h('dl', { class: 'shortcut-list' }, list.map((s) => [h('dt', {}, s.description), h('dd', {}, h('kbd', {}, s.label))]))))),
  });
}

/* ------------------------------------------------------------------- shell */

function ensureShell() {
  if (shell) return shell;
  const st = store.get();
  root.replaceChildren();
  shell = createShell({
    apps: visibleApps(st.user),
    user: st.user,
    serverName: serverName(),
    version: st.status?.version,
    docsUrl: DOCS_URL,
    navigate: (p) => router.navigate(p),
    onSignOut: signOut,
    onShortcuts: showShortcuts,
  });
  root.append(shell.el);
  return shell;
}

function dropShell() {
  if (!shell) return;
  shell.destroy();
  shell = null;
}

function forbiddenView() {
  return h('div', { class: 'page' }, ui.emptyState({
    icon: 'shield', title: 'Administrators only',
    message: 'You need an administrator account to open this page.',
    action: ui.button({ label: 'Back to Home', icon: 'home', href: '/' }),
  }));
}

function makeCtx(def, to, scope) {
  return {
    api, router, ui, store, h, icon,
    get user() { return store.get().user; },
    get isAdmin() { return isAdmin(store.get().user); },
    get settings() { return store.get().status || {}; },
    params: to.params,
    query: to.query,
    searchParams: to.searchParams,
    path: to.path,
    base: def.path,
    setTitle,
    setActions: (nodes) => shell?.setActions(nodes),
    setSearch: (cfg) => shell?.setSearch(cfg),
    setLayout: (opts) => shell?.setLayout(opts),
    commands: { register: (cmds) => scope.add(registerCommands(cmds)) },
    shortcuts: { register: (combo, fn, opts) => scope.add(registerShortcut(combo, fn, { group: def.title, ...opts })) },
    uploads,
    actions: { uploadToHome, signOut, showShortcuts },
    signal: scope.signal,
    onCleanup: (fn) => scope.add(fn),
  };
}

/* ------------------------------------------------------------------ render */

async function showApp(route, to, token) {
  const def = apps.find((a) => a.id === route.app);
  const sh = ensureShell();
  sh.setActive(def.id);
  sh.closeNav();

  if (def.adminOnly && !isAdmin(store.get().user)) {
    teardown();
    setTitle('Forbidden');
    sh.view.replaceChildren(forbiddenView());
    return;
  }

  // Navigation inside the same app: let it update in place when it supports that.
  if (current?.kind === 'app' && current.id === def.id && current.update) {
    current.ctx = makeCtx(def, to, current.scope);
    await current.update(current.ctx);
    return;
  }

  teardown();
  const scope = createScope();
  scope.add(() => {
    shell?.setActions(null);
    shell?.setSearch(null);
    shell?.setLayout({});
    router.setLeaveGuard(null);
  });
  setTitle(def.title);
  const loadingTimer = setTimeout(() => {
    if (token === renderToken) sh.view.replaceChildren(ui.loadingBlock());
  }, 120);
  let mod;
  try {
    mod = await def.load();
  } catch (err) {
    clearTimeout(loadingTimer);
    if (token !== renderToken) return;
    console.error(err);
    sh.view.replaceChildren(h('div', { class: 'page' }, ui.errorState(new Error(`The ${def.title} app failed to load. Try reloading the page.`), () => location.reload())));
    scope.dispose();
    return;
  }
  clearTimeout(loadingTimer);
  if (token !== renderToken) {
    scope.dispose();
    return;
  }
  const container = h('div', { class: 'app-root', dataset: { app: def.id } });
  sh.view.replaceChildren(container);
  const ctx = makeCtx(def, to, scope);
  current = { kind: 'app', id: def.id, scope, ctx, unmount: null, update: null };
  const mine = current;
  try {
    const result = await mod.mount(container, ctx);
    if (typeof result === 'function') mine.unmount = result;
    else if (result && typeof result === 'object') {
      mine.unmount = typeof result.unmount === 'function' ? result.unmount : null;
      mine.update = typeof result.update === 'function' ? result.update : null;
    }
    if (current !== mine) {
      // Navigated away while mounting.
      mine.unmount?.();
    }
  } catch (err) {
    console.error(err);
    if (current === mine) container.replaceChildren(h('div', { class: 'page' }, ui.errorState(err, () => router.reload())));
  }
}

async function showPage(route, to, token) {
  const st = store.get();
  const inShell = route.layout === 'auto' && !!st.user && !st.status?.setup_required;
  teardown();
  let container;
  if (inShell) {
    const sh = ensureShell();
    sh.setActive(null);
    container = h('div', { class: 'app-root' });
    sh.view.replaceChildren(container);
  } else {
    dropShell();
    container = h('div', { class: 'auth-root' });
    root.replaceChildren(container);
  }
  const mod = await PAGES[route.page]();
  if (token !== renderToken) return;
  const scope = createScope();
  current = { kind: 'page', id: route.page, scope, unmount: null };
  const mine = current;
  const ctx = {
    api, router, ui, store, h, icon,
    get settings() { return store.get().status || {}; },
    get user() { return store.get().user; },
    query: to.query,
    path: to.path,
    setTitle,
    signal: scope.signal,
    onCleanup: (fn) => scope.add(fn),
    /** Store a session from login/setup/register and continue to ?next= (or `fallback`). */
    completeLogin: (session, fallback = '/') => {
      setSession(session);
      // The shared cookie now belongs to this account: tabs still showing another one must reset.
      authSync?.post('session', session?.user?.id || '');
      return router.navigate(safeNext(to.query.next, fallback), { replace: true });
    },
  };
  try {
    const result = await mod.mount(container, ctx);
    if (typeof result === 'function') mine.unmount = result;
    if (current !== mine) mine.unmount?.();
  } catch (err) {
    console.error(err);
    container.replaceChildren(ui.errorState(err, () => router.reload()));
  }
}

async function resolve(to) {
  const token = ++renderToken;
  if (to.route?.app) await showApp(to.route, to, token);
  else await showPage(to.route, to, token);
  if (token === renderToken) {
    root.removeAttribute('aria-busy');
    root.classList.remove('boot');
  }
}

function guard(to) {
  const st = store.get();
  const r = to.route;
  const setupRequired = !!st.status?.setup_required;
  if (setupRequired) return r?.page === 'setup' ? null : '/setup';
  if (r?.page === 'setup') return st.user ? '/' : '/login';
  if (r?.guestOnly && st.user) return safeNext(to.query.next, '/');
  if (r?.auth && !st.user) return `/login${buildQuery({ next: to.href !== '/' ? to.href : null })}`;
  return null;
}

/* -------------------------------------------------------- auth side effects */

let redirecting = false;
function handleUnauthorized() {
  if (redirecting || !store.get().user) return;
  redirecting = true;
  dropLocalSession();
  authSync?.post('signout'); // the session cookie is shared, so every other tab lost it too
  const here = router?.current?.href;
  ui.toast.info('Your session has ended. Please sign in again.');
  router.navigate(`/login${buildQuery({ next: here && here !== '/' ? here : null })}`, { replace: true })
    .finally(() => { redirecting = false; });
}

/* ------------------------------------------------- cross-tab session sync */

let authSync = null;

/**
 * Another tab signed out, lost its session, or signed in. Sessions live in one cookie shared by
 * all tabs, so this tab's in-memory user is stale: drop it (and everything apps loaded or
 * decrypted for it) right away instead of waiting for the next API call to fail.
 */
function onAuthSignal({ type, user }) {
  const me = store.get().user;
  if (type === 'signout') {
    if (!me) return;
    dropLocalSession();
    location.reload(); // a fresh page holds nothing from the previous session; boot lands on /login
    return;
  }
  // type 'session': the cookie now belongs to `user`.
  if (!me) {
    adoptSession();
  } else if (me.id === user) {
    recheckSession(true); // same account signed in again: pick up the new session's CSRF token
  } else {
    dropLocalSession();
    location.reload();
  }
}

/** A tab with nobody signed in picks up a session started in another tab. */
async function adoptSession() {
  if (store.get().status?.setup_required) {
    location.reload(); // setup finished elsewhere: boot again to read the new server status
    return;
  }
  try {
    const s = await api.get('/auth/session', { noRedirect: true, timeout: 15000 });
    if (s?.user && !store.get().user) {
      setSession(s);
      await router.reload(); // guest-only pages (login, register) move on to the app
    }
  } catch {
    /* still signed out */
  }
}

const RECHECK_MS = 15000;
let lastRecheck = 0;
let rechecking = false;

/**
 * Confirm the session is still the one this tab shows, when the window regains focus or is
 * shown again (or restored from the back/forward cache). Covers sessions ended elsewhere
 * (another device, an admin, expiry) and signals this tab missed. Throttled; never polled in the
 * background, so it does not keep an idle session alive.
 */
async function recheckSession(force = false) {
  const me = store.get().user;
  if (!me || rechecking || redirecting) return;
  if (!force && Date.now() - lastRecheck < RECHECK_MS) return;
  rechecking = true;
  lastRecheck = Date.now();
  try {
    const s = await api.get('/auth/session', { noRedirect: true, timeout: 15000 });
    const cur = store.get().user;
    if (!cur) return;
    if (!s?.user) handleUnauthorized();
    else if (s.user.id !== cur.id) {
      dropLocalSession();
      location.reload();
    } else if (s.csrf_token && s.csrf_token !== store.get().csrf) setSession(s);
  } catch (err) {
    if (err?.status === 401) handleUnauthorized();
  } finally {
    rechecking = false;
  }
}

function initSessionSync() {
  authSync = createAuthSync({ onSignal: onAuthSignal });
  window.addEventListener('focus', () => recheckSession());
  document.addEventListener('visibilitychange', () => {
    if (document.visibilityState === 'visible') recheckSession();
  });
  window.addEventListener('pageshow', (e) => {
    if (e.persisted) recheckSession(true);
  });
}

function handleSetupRequired() {
  const st = store.get();
  if (st.status?.setup_required) return;
  store.set({ status: { ...(st.status || {}), setup_required: true } });
  router?.navigate('/setup', { replace: true });
}

/* --------------------------------------------- global shortcuts & commands */

function globalCommands() {
  const st = store.get();
  if (!st.user) return [];
  const nav = (p) => () => router.navigate(p);
  const cmds = visibleApps(st.user).map((a) => ({
    id: `app:${a.id}`, title: a.title, subtitle: a.description, icon: a.icon, group: 'Apps',
    keywords: [a.id, 'open', 'go to'], run: nav(a.path),
  }));
  const settings = [
    ['profile', 'Profile', 'user'], ['security', 'Password & two-factor', 'shield-check'], ['sessions', 'Sessions & devices', 'laptop'],
    ['tokens', 'API tokens', 'key'], ['appearance', 'Appearance', 'sun'],
  ].map(([id, title, ic]) => ({ id: `settings:${id}`, title, subtitle: 'Settings', icon: ic, group: 'Settings', keywords: ['account', id], run: nav(`/settings/${id}`) }));
  const admin = isAdmin(st.user) ? [
    ['users', 'Users', 'users'], ['general', 'General settings', 'sliders'], ['security', 'Security policy', 'shield'],
    ['email', 'Email (SMTP & IMAP)', 'mail'], ['media', 'Media & TV', 'film'], ['files', 'File settings', 'folder'],
    ['libraries', 'Libraries', 'library'], ['categories', 'Categories', 'tag'], ['audit', 'Audit log', 'history'],
  ].map(([id, title, ic]) => ({ id: `admin:${id}`, title, subtitle: 'Admin', icon: ic, group: 'Admin', keywords: ['admin', id], run: nav(`/admin/${id}`) })) : [];
  const actions = [
    { id: 'act:upload', title: 'Upload files…', icon: 'upload', group: 'Actions', keywords: ['add', 'send'], run: () => uploadToHome() },
    { id: 'act:note', title: 'New note', icon: 'note', group: 'Actions', keywords: ['create', 'write'], run: nav('/notes?new=1') },
    { id: 'act:guide', title: 'Open TV guide', icon: 'tv', group: 'Actions', keywords: ['channels', 'epg', 'watch'], run: nav('/tv/guide') },
    { id: 'act:theme', title: 'Toggle light/dark theme', icon: 'moon', group: 'Actions', keywords: ['dark', 'light', 'appearance'], run: toggleTheme },
    { id: 'act:theme-system', title: 'Use system theme', icon: 'monitor', group: 'Actions', keywords: ['auto', 'appearance'], run: () => setThemePref('system') },
    { id: 'act:shortcuts', title: 'Keyboard shortcuts', icon: 'keyboard', group: 'Help', shortcut: '?', run: showShortcuts },
    { id: 'act:api', title: 'API reference (Swagger)', icon: 'code', group: 'Help', keywords: ['openapi', 'docs', 'rest'], run: () => window.open('/api/docs', '_blank', 'noopener') },
    { id: 'act:docs', title: 'Documentation', icon: 'book', group: 'Help', keywords: ['help', 'readthedocs', 'manual'], run: () => window.open(DOCS_URL, '_blank', 'noopener,noreferrer') },
    { id: 'act:signout', title: 'Sign out', icon: 'log-out', group: 'Account', keywords: ['logout', 'log out'], run: signOut },
  ];
  return [...actions, ...cmds, ...settings, ...admin];
}

function registerGlobals() {
  const noOverlay = () => !document.documentElement.classList.contains('modal-open') && !isPaletteOpen();
  const signedIn = () => !!store.get().user && noOverlay();
  registerShortcut('mod+k', () => openPalette(), { description: 'Command palette', group: 'General', allowInInputs: true, when: signedIn });
  registerShortcut('/', () => shell?.focusSearch(), { description: 'Search', group: 'General', when: () => !!shell && noOverlay() });
  registerShortcut('?', showShortcuts, { description: 'Show keyboard shortcuts', group: 'General', when: signedIn });
  const goto = [['h', '/', 'Home'], ['f', '/files', 'Files'], ['t', '/tv', 'TV'], ['n', '/notes', 'Notes'], ['p', '/vault', 'Passwords'],
    ['s', '/sheets', 'Sheets'], ['d', '/system', 'Storage'], [',', '/settings', 'Settings']];
  for (const [k, path, label] of goto) {
    registerShortcut(`g ${k}`, () => router.navigate(path), { description: `Go to ${label}`, group: 'Navigation', when: signedIn });
  }
  registerShortcut('g a', () => router.navigate('/admin'), { description: 'Go to Admin', group: 'Navigation', when: () => signedIn() && isAdmin(store.get().user) });
  registerShortcut('shift+t', toggleTheme, { description: 'Toggle theme', group: 'General', when: signedIn });
  registerCommands(globalCommands);
}

/* -------------------------------------------------------------------- boot */

function bootError(err) {
  root.removeAttribute('aria-busy');
  root.classList.remove('boot');
  root.replaceChildren(h('div', { class: 'auth-root' }, h('div', { class: 'auth' },
    h('div', { class: 'auth-card' },
      h('div', { class: 'auth-brand' }, logoMark(40)),
      h('h1', { class: 'auth-title' }, 'Can’t reach the server'),
      h('p', { class: 'auth-subtitle' }, ui.errorMessage(err)),
      h('div', { class: 'auth-actions' }, ui.button({ label: 'Try again', icon: 'refresh', variant: 'primary', block: true, onClick: () => location.reload() }))))));
}

async function boot() {
  initTheme();
  ui.initTooltips();
  initShortcuts();
  api.configure({
    getCsrf: () => store.get().csrf,
    onUnauthorized: handleUnauthorized,
    onSetupRequired: handleSetupRequired,
  });

  const [statusRes, sessionRes] = await Promise.allSettled([
    api.get('/setup/status', { noRedirect: true, timeout: 15000 }),
    api.get('/auth/session', { noRedirect: true, timeout: 15000 }),
  ]);
  if (statusRes.status === 'rejected') {
    const e = statusRes.reason;
    if (e?.status === 0 || e?.status >= 500) {
      bootError(e);
      return;
    }
  } else {
    store.set({ status: statusRes.value });
  }
  if (sessionRes.status === 'fulfilled' && sessionRes.value?.user) {
    setSession(sessionRes.value);
  } else if (sessionRes.reason?.code === 'setup_required') {
    store.set({ status: { ...(store.get().status || {}), setup_required: true } });
  }

  // Keep the shell, upload scope and title in sync with the session.
  store.select((s) => s.user, (u, prev) => {
    uploads.configure({ scope: u?.id || '' });
    // Signed out, expired, or another account: none of the previous user's uploads survive.
    if (!u || u.id !== prev?.id) uploads.reset();
    if (!u) {
      teardown();
      dropShell();
      return;
    }
    if (shell && prev && u.id === prev.id) shell.updateUser(u, u.role !== prev.role ? visibleApps(u) : null);
    else if (shell) {
      dropShell(); // different user: rebuild nav for their role
    }
  });
  store.select((s) => s.status?.server_name, (name) => {
    shell?.setServerName(name);
    document.title = document.title.replace(/ · .*$/, '') + ` · ${name || 'Ludworks'}`;
  });
  uploads.configure({ scope: store.get().user?.id || '' });
  // An upload rejected with 401 means the session cookie is gone (expired or revoked).
  uploads.on('error', ({ error }) => {
    if (error?.status === 401) handleUnauthorized();
  });
  createUploadTray(uploads);
  initSessionSync();
  registerGlobals();

  router = new Router({
    routes,
    resolve,
    guard,
    onError: (err) => {
      console.error(err);
      ui.toast.error(err);
    },
  });
  await router.start();
}

boot().catch((err) => {
  console.error(err);
  bootError(err);
});
