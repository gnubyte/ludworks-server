// History-API router: pattern matching (pure, unit tested), guarded navigation,
// link interception and scroll restoration.

function escapeRe(s) {
  return s.replace(/[.*+?^${}()|[\]\\]/g, '\\$&');
}

/**
 * Compile a route pattern.
 *   "/"                 exact root
 *   "/settings/:tab?"   named (optionally optional) segment
 *   "/files/*rest"      wildcard tail (matches "/files" too, rest = "")
 *   "*"                 catch-all
 * @param {string} pattern
 * @returns {{re: RegExp, keys: string[], wildcard: string|null}}
 */
export function compilePattern(pattern) {
  if (pattern === '*') return { re: /^.*$/, keys: [], wildcard: null };
  const segs = pattern.split('/').filter(Boolean);
  const keys = [];
  let wildcard = null;
  let src = '^';
  for (const seg of segs) {
    if (seg.startsWith('*')) {
      wildcard = seg.slice(1) || 'rest';
      keys.push(wildcard);
      src += '(?:/(.*))?';
      break;
    }
    if (seg.startsWith(':')) {
      const optional = seg.endsWith('?');
      keys.push(seg.slice(1, optional ? -1 : undefined));
      src += optional ? '(?:/([^/]+))?' : '/([^/]+)';
    } else {
      src += `/${escapeRe(seg)}`;
    }
  }
  src += '/?$';
  return { re: new RegExp(src), keys, wildcard };
}

function safeDecode(v) {
  try {
    return decodeURIComponent(v);
  } catch {
    return null;
  }
}

/**
 * Match a pathname against a compiled pattern.
 * @returns {Record<string,string>|null} params, or null when it doesn't match
 */
export function matchPattern(compiled, pathname) {
  const m = compiled.re.exec(pathname);
  if (!m) return null;
  const params = {};
  for (let i = 0; i < compiled.keys.length; i++) {
    const key = compiled.keys[i];
    const raw = m[i + 1];
    if (raw === undefined) {
      params[key] = key === compiled.wildcard ? '' : undefined;
      continue;
    }
    if (key === compiled.wildcard) {
      const parts = raw.replace(/\/+$/, '').split('/').map(safeDecode);
      if (parts.some((p) => p === null)) return null;
      params[key] = parts.join('/');
    } else {
      const v = safeDecode(raw);
      if (v === null) return null;
      params[key] = v;
    }
  }
  return params;
}

/**
 * Find the first matching route (routes are tried in order).
 * @param {{path: string, _c?: object}[]} routes
 * @param {string} pathname
 * @returns {{route: object, params: Record<string,string>}|null}
 */
export function matchRoute(routes, pathname) {
  for (const route of routes) {
    route._c ??= compilePattern(route.path);
    const params = matchPattern(route._c, pathname);
    if (params) return { route, params };
  }
  return null;
}

/**
 * Parse a query string into a plain object (last value wins).
 * @param {string} search "?a=1&b=2" or "a=1"
 */
export function parseQuery(search) {
  const out = {};
  for (const [k, v] of new URLSearchParams(search || '')) out[k] = v;
  return out;
}

/**
 * Build "?a=1&b=2" from an object, skipping null/undefined/'' values.
 * @param {Record<string, any>} obj
 */
export function buildQuery(obj) {
  const qs = new URLSearchParams();
  for (const [k, v] of Object.entries(obj || {})) {
    if (v === undefined || v === null || v === '') continue;
    qs.set(k, String(v));
  }
  const s = qs.toString();
  return s ? `?${s}` : '';
}

/**
 * Validate a post-login redirect target. Only same-origin app paths are allowed
 * (prevents open redirects via ?next=//evil.example or /\evil.example).
 * @param {string|null|undefined} next
 * @param {string} [fallback]
 */
export function safeNext(next, fallback = '/') {
  if (typeof next !== 'string' || next.length > 2048) return fallback;
  if (!next.startsWith('/') || next.startsWith('//') || next.startsWith('/\\')) return fallback;
  if (/[\u0000-\u001f\u007f\\]/.test(next)) return fallback;
  const path = next.split(/[?#]/)[0];
  if (/^\/(api|assets)(\/|$)/.test(path)) return fallback;
  if (/^\/(login|register|setup|forgot-password|reset-password|verify-email)\/?$/.test(path)) return fallback;
  return next;
}

/**
 * Router.
 * options.routes   – [{path, ...meta}] tried in order
 * options.resolve  – async (to) => void; renders the matched route. `to` =
 *                    {path, search, hash, query, params, route, href, state, nav}
 * options.guard    – (to) => string|null; returning a path redirects (replace)
 * options.onError  – (err, to) => void
 */
export class Router {
  constructor({ routes, resolve, guard = () => null, onError = (e) => console.error(e) }) {
    this.routes = routes;
    this.resolveFn = resolve;
    this.guard = guard;
    this.onError = onError;
    this.current = null;
    this.navId = 0;
    this.leaveGuard = null;
    this.scrollPositions = new Map();
    this.listeners = new Set();
    this._onPop = this._onPop.bind(this);
    this._onClick = this._onClick.bind(this);
  }

  /** Begin listening and render the current location. */
  start() {
    if ('scrollRestoration' in history) history.scrollRestoration = 'manual';
    window.addEventListener('popstate', this._onPop);
    document.addEventListener('click', this._onClick);
    if (!history.state?.key) history.replaceState({ ...(history.state || {}), key: this._key() }, '');
    return this._go(location.pathname + location.search + location.hash, { replace: true, nav: 'initial' });
  }

  stop() {
    window.removeEventListener('popstate', this._onPop);
    document.removeEventListener('click', this._onClick);
  }

  _key() {
    return Math.random().toString(36).slice(2, 10);
  }

  /**
   * Navigate to a path ("/files?x=1"). Returns a promise that resolves after rendering.
   * @param {string} to
   * @param {{replace?: boolean, state?: object, scroll?: boolean}} [opts]
   */
  async navigate(to, { replace = false, state = {}, scroll = true } = {}) {
    if (this.leaveGuard && !(await this._checkLeave(to))) return false;
    return this._go(to, { replace, state, scroll, nav: replace ? 'replace' : 'push' });
  }

  /** Update query params of the current URL (merge; null removes) without a history entry by default. */
  setQuery(patch, { replace = true } = {}) {
    const q = { ...(this.current?.query || {}) };
    for (const [k, v] of Object.entries(patch)) {
      if (v === null || v === undefined || v === '') delete q[k];
      else q[k] = String(v);
    }
    return this.navigate(location.pathname + buildQuery(q), { replace, scroll: false });
  }

  back() {
    history.back();
  }

  /** Re-render the current route. */
  reload() {
    return this._go(location.pathname + location.search + location.hash, { replace: true, nav: 'reload', scroll: false });
  }

  /**
   * Register a guard consulted before leaving the current route (unsaved changes).
   * fn(to) → boolean | Promise<boolean>. Pass null to clear.
   */
  setLeaveGuard(fn) {
    this.leaveGuard = fn;
  }

  /** Subscribe to completed navigations. @returns {() => void} */
  onChange(fn) {
    this.listeners.add(fn);
    return () => this.listeners.delete(fn);
  }

  async _checkLeave(to) {
    try {
      const ok = await this.leaveGuard(to);
      if (ok) this.leaveGuard = null;
      return !!ok;
    } catch {
      return false;
    }
  }

  _saveScroll() {
    const key = history.state?.key;
    if (key) this.scrollPositions.set(key, window.scrollY);
  }

  async _go(href, { state = {}, scroll = true, nav = 'push', depth = 0 } = {}) {
    const id = ++this.navId;
    const u = new URL(href, location.origin);
    if (u.origin !== location.origin) {
      location.assign(u.href);
      return false;
    }
    const path = u.pathname;
    const match = matchRoute(this.routes, path);
    const to = {
      path, search: u.search, hash: u.hash, href: path + u.search + u.hash,
      query: parseQuery(u.search), searchParams: u.searchParams,
      params: match?.params || {}, route: match?.route || null, state, nav,
    };
    const redirect = depth < 5 ? this.guard(to) : null;
    if (redirect && redirect !== to.href) {
      // A redirected click still creates one history entry; anything else replaces.
      return this._go(redirect, { nav: nav === 'push' ? 'push' : 'replace', depth: depth + 1 });
    }

    if (nav === 'push' || nav === 'replace' || nav === 'initial' || nav === 'reload') {
      if (nav === 'push') {
        this._saveScroll();
        history.pushState({ ...state, key: this._key() }, '', to.href);
      } else if (location.pathname + location.search + location.hash !== to.href || nav === 'initial') {
        history.replaceState({ ...state, key: history.state?.key || this._key() }, '', to.href);
      }
    }
    this.current = to;
    try {
      await this.resolveFn(to);
    } catch (err) {
      if (id === this.navId) this.onError(err, to);
      return false;
    }
    if (id !== this.navId) return false; // superseded by a newer navigation
    if (scroll) this._restoreScroll(to);
    for (const fn of [...this.listeners]) fn(to);
    return true;
  }

  _restoreScroll(to) {
    if (to.nav === 'pop') {
      const y = this.scrollPositions.get(history.state?.key) ?? 0;
      requestAnimationFrame(() => window.scrollTo(0, y));
    } else if (to.hash) {
      const el = document.getElementById(decodeURIComponent(to.hash.slice(1)));
      if (el) el.scrollIntoView();
    } else if (to.nav !== 'reload') {
      window.scrollTo(0, 0);
    }
  }

  async _onPop(e) {
    const href = location.pathname + location.search + location.hash;
    if (this.leaveGuard) {
      const prev = this.current;
      if (!(await this._checkLeave({ href }))) {
        // Undo the browser's history move: re-push where we were.
        if (prev) history.pushState({ key: this._key() }, '', prev.href);
        return;
      }
    }
    this._go(href, { nav: 'pop', state: e.state || {} });
  }

  _onClick(e) {
    if (e.defaultPrevented || e.button !== 0 || e.metaKey || e.ctrlKey || e.shiftKey || e.altKey) return;
    const a = e.target instanceof Element ? e.target.closest('a[href]') : null;
    if (!a) return;
    if (a.target && a.target !== '_self') return;
    if (a.hasAttribute('download') || a.dataset.native !== undefined) return;
    const rel = (a.getAttribute('rel') || '').split(/\s+/);
    if (rel.includes('external')) return;
    const u = new URL(a.href, location.href);
    if (u.origin !== location.origin) return;
    if (/^\/(api|assets)(\/|$)/.test(u.pathname)) return; // served by the server, not the SPA
    if (u.pathname === location.pathname && u.search === location.search && u.hash) return; // in-page anchor
    e.preventDefault();
    this.navigate(u.pathname + u.search + u.hash);
  }
}
