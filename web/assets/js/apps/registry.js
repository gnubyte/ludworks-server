// App registry — the contract between the shell and app modules.
//
// ┌─────────────────────────────────────────────────────────────────────────────┐
// │ APP MODULE CONTRACT                                                           │
// │                                                                               │
// │ Each app lives in assets/js/apps/<id>/index.js and exports:                   │
// │                                                                               │
// │   export function mount(container, ctx) { ...; return cleanup; }             │
// │                                                                               │
// │ `container` is an empty <div> inside the shell's main view. Build your UI     │
// │ with core/dom.js h() — never innerHTML with data (strict CSP).                │
// │                                                                               │
// │ mount() may return:                                                           │
// │   • nothing,                                                                  │
// │   • an unmount function, called when the user leaves the app, or             │
// │   • an object {unmount?(), update?(ctx)}. When `update` exists and the URL    │
// │     changes *within the same app* (e.g. /files/a → /files/b?q=x), the shell   │
// │     calls update(newCtx) instead of remounting — keep your state and react to │
// │     ctx.params / ctx.query.                                                   │
// │ mount() may be async (return a Promise of the above).                         │
// │                                                                               │
// │ ctx = {                                                                       │
// │   api        core/api.js client: get/post/put/patch/del(path, ...) (paths are │
// │              relative to /api/v1), request(), upload() (XHR + progress),      │
// │              download(path, query), url(path, query). Throws ApiError         │
// │              {status, code, message, details, fields}.                        │
// │   router     navigate(path, {replace}), setQuery({k: v|null}),                │
// │              setLeaveGuard(fn|null) for unsaved changes, current.             │
// │   user       the signed-in User (API.md §2); ctx.isAdmin boolean.            │
// │   settings   public server info from GET /setup/status                       │
// │              {server_name, registration_enabled, version, tls_fingerprint_sha256}.
// │   ui         core/ui.js (toast, modal, drawer, confirm, prompt, openMenu,     │
// │              contextMenu, menuButton, tabs, dataTable, infiniteList,          │
// │              createCursorPager, emptyState, errorState, skeleton…, button,    │
// │              badge, chip, progress, breadcrumbs, splitPane, segmented,        │
// │              copyText, copyButton, card, kv, alert, pageHeader, timeAgo…).    │
// │   params     route params; params.rest = path after the app's base path       │
// │              ('' at the app root, e.g. 'guide' for /tv/guide).                │
// │   query      parsed query string (last value wins); searchParams for multi.  │
// │   path, base current pathname and the app base path (e.g. '/tv').            │
// │   setTitle(text)       page title in the top bar and document.title.         │
// │   setActions(nodes)    buttons shown in the top bar (null clears).           │
// │   setSearch({placeholder, value, onInput(q), onSubmit(q)} | null)             │
// │              routes the top-bar search box to the app (debounced onInput);    │
// │              without it the box opens the command palette.                    │
// │   setLayout({full: true})  edge-to-edge, viewport-height view without page   │
// │              padding (for split-pane apps like Notes/TV player).              │
// │   commands.register([{id, title, icon?, subtitle?, keywords?, run()}])       │
// │              adds command-palette entries while the app is mounted.           │
// │   shortcuts.register(combo, handler, {description})  app-scoped shortcuts.   │
// │   uploads    the shared UploadQueue (core/upload.js): uploads.add(file,       │
// │              {spaceId, path, category, subcategory, tags, overwrite}) →       │
// │              TusUpload; uploads.on('complete', fn) → off(). Progress is shown │
// │              in the global upload tray.                                       │
// │   actions    shell actions: uploadToHome(files?) (picker → home root),        │
// │              signOut(), showShortcuts().                                      │
// │   signal     AbortSignal aborted on unmount (pass to api calls).             │
// │   onCleanup(fn)  run fn on unmount.                                          │
// │   store      core/store.js global store; h / icon helpers for convenience.   │
// │ }                                                                             │
// │ Everything registered through ctx (actions, search, commands, shortcuts,      │
// │ listeners added via ctx.onCleanup(fn)) is removed automatically on unmount.  │
// └─────────────────────────────────────────────────────────────────────────────┘

/**
 * @typedef {{id: string, title: string, icon: string, path: string, adminOnly?: boolean,
 *            description: string, tint: string, nav?: boolean, load: () => Promise<any>}} AppDef
 */

/** @type {AppDef[]} */
export const apps = [
  {
    id: 'home', title: 'Home', icon: 'home', path: '/', tint: 'indigo', description: 'Overview and quick actions',
    load: () => import('./home/index.js'),
  },
  {
    id: 'files', title: 'Files', icon: 'folder', path: '/files', tint: 'blue', description: 'Browse, upload and organize your storage',
    load: () => import('./files/index.js'),
  },
  {
    id: 'tv', title: 'TV', icon: 'tv', path: '/tv', tint: 'violet', description: 'Virtual channels, lineups and your media library',
    load: () => import('./tv/index.js'),
  },
  {
    id: 'notes', title: 'Notes', icon: 'note', path: '/notes', tint: 'amber', description: 'Notes, code snippets and Markdown',
    load: () => import('./notes/index.js'),
  },
  {
    id: 'vault', title: 'Passwords', icon: 'key', path: '/vault', tint: 'emerald', description: 'End-to-end encrypted password vault',
    load: () => import('./vault/index.js'),
  },
  {
    id: 'sheets', title: 'Sheets', icon: 'sheet', path: '/sheets', tint: 'teal', description: 'Spreadsheets with formulas, XLSX/XLS/CSV',
    load: () => import('./sheets/index.js'),
  },
  {
    id: 'system', title: 'Storage', icon: 'storage', path: '/system', tint: 'sky', description: 'Storage usage and server status',
    load: () => import('./system/index.js'),
  },
  {
    id: 'settings', title: 'Settings', icon: 'settings', path: '/settings', tint: 'slate', description: 'Profile, security, sessions and API tokens',
    load: () => import('./settings/index.js'),
  },
  {
    id: 'admin', title: 'Admin', icon: 'shield', path: '/admin', tint: 'rose', adminOnly: true, description: 'Users, server settings and audit log',
    load: () => import('./admin/index.js'),
  },
];

/** Apps visible to a user (admin-only apps hidden from non-admins). */
export function visibleApps(user) {
  return apps.filter((a) => !a.adminOnly || user?.role === 'admin');
}

/** Find the app owning a pathname (longest base-path match). */
export function appForPath(pathname) {
  let best = null;
  for (const a of apps) {
    const base = a.path;
    const hit = base === '/' ? pathname === '/' : pathname === base || pathname.startsWith(`${base}/`);
    if (hit && (!best || base.length > best.path.length)) best = a;
  }
  return best;
}
