// Tiny observable store for session/user/server state shared across the UI.

/**
 * Create a store with shallow-merge updates and subscriptions.
 * @template T
 * @param {T} initial
 */
export function createStore(initial) {
  let state = { ...initial };
  const subs = new Set();
  const notify = (prev) => {
    for (const fn of [...subs]) {
      try {
        fn(state, prev);
      } catch (err) {
        console.error('store subscriber failed', err);
      }
    }
  };
  return {
    /** @returns {T} */
    get: () => state,
    /** Shallow-merge a patch (object or fn(state) → object). */
    set(patch) {
      const prev = state;
      const next = typeof patch === 'function' ? patch(state) : patch;
      state = { ...state, ...next };
      notify(prev);
    },
    /** Subscribe to every change. @returns {() => void} unsubscribe */
    subscribe(fn) {
      subs.add(fn);
      return () => subs.delete(fn);
    },
    /** Subscribe to a derived value; fn(newValue, oldValue) only fires when it changes (Object.is). */
    select(selector, fn) {
      let cur = selector(state);
      return this.subscribe((st) => {
        const n = selector(st);
        if (!Object.is(n, cur)) {
          const old = cur;
          cur = n;
          fn(n, old);
        }
      });
    },
  };
}

/**
 * Global app state.
 *  user      – current User (API.md §2) or null
 *  csrf      – CSRF token for cookie sessions
 *  scopes    – effective scopes of the session
 *  expiresAt – session expiry (ISO string)
 *  status    – GET /setup/status payload (server_name, registration_enabled, version, ...)
 */
export const store = createStore({ user: null, csrf: null, scopes: [], expiresAt: null, status: null });

/** @param {{role?: string}|null|undefined} user */
export const isAdmin = (user) => user?.role === 'admin';

/** Store a login/session/setup response ({user, csrf_token, expires_at, scopes}). */
export function setSession(sess) {
  store.set({
    user: sess.user ?? null,
    csrf: sess.csrf_token ?? store.get().csrf,
    expiresAt: sess.expires_at ?? null,
    scopes: sess.scopes ?? ['*'],
  });
}

/** Forget the local session (after logout or a 401). */
export function clearSession() {
  store.set({ user: null, csrf: null, scopes: [], expiresAt: null });
}

/** Replace the cached user object (after PATCH /me etc.). */
export function setUser(user) {
  store.set({ user });
}

/** Display name for a user: display_name, else username. */
export function displayName(user) {
  return (user?.display_name || '').trim() || user?.username || '';
}
