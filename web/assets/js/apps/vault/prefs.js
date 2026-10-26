// Non-sensitive per-user preferences of the Passwords app (auto-lock, sort order, generator
// options) in localStorage. Nothing secret is ever stored here: no keys, no passwords, no
// item data, no folder names. Storage failures (private mode, quota) fall back to defaults.

export const AUTO_LOCK_CHOICES = Object.freeze([1, 2, 5, 10, 15, 30, 60, 120, 240, 480]);
const SORTS = new Set(['name', 'name-desc', 'updated', 'created']);

export const DEFAULT_PREFS = Object.freeze({
  autoLockMinutes: 15,
  lockOnHide: false,
  sort: 'name',
  gen: Object.freeze({
    mode: 'password', length: 20, upper: true, lower: true, digits: true, symbols: true, excludeAmbiguous: false,
    words: 6, separator: '-', capitalize: false, includeNumber: false,
  }),
});

const clampInt = (v, lo, hi, def) => (Number.isInteger(v) && v >= lo && v <= hi ? v : def);
const bool = (v, def) => (typeof v === 'boolean' ? v : def);

/** Coerce anything into a valid prefs object. */
export function sanitizePrefs(raw) {
  const r = raw && typeof raw === 'object' ? raw : {};
  const g = r.gen && typeof r.gen === 'object' ? r.gen : {};
  const d = DEFAULT_PREFS.gen;
  const gen = {
    mode: g.mode === 'passphrase' ? 'passphrase' : 'password',
    length: clampInt(g.length, 8, 128, d.length),
    upper: bool(g.upper, d.upper),
    lower: bool(g.lower, d.lower),
    digits: bool(g.digits, d.digits),
    symbols: bool(g.symbols, d.symbols),
    excludeAmbiguous: bool(g.excludeAmbiguous, d.excludeAmbiguous),
    words: clampInt(g.words, 3, 12, d.words),
    separator: typeof g.separator === 'string' && g.separator.length <= 3 && !/[\u0000-\u001f]/.test(g.separator) ? g.separator : d.separator,
    capitalize: bool(g.capitalize, d.capitalize),
    includeNumber: bool(g.includeNumber, d.includeNumber),
  };
  if (!gen.upper && !gen.lower && !gen.digits && !gen.symbols) gen.lower = true;
  return {
    autoLockMinutes: AUTO_LOCK_CHOICES.includes(r.autoLockMinutes) ? r.autoLockMinutes : DEFAULT_PREFS.autoLockMinutes,
    lockOnHide: bool(r.lockOnHide, DEFAULT_PREFS.lockOnHide),
    sort: SORTS.has(r.sort) ? r.sort : DEFAULT_PREFS.sort,
    gen,
  };
}

const keyFor = (userId) => `lw.vault.prefs.${userId || 'anon'}`;

function storageOf(storage) {
  if (storage !== undefined) return storage;
  try {
    return globalThis.localStorage ?? null;
  } catch {
    return null;
  }
}

/** Load prefs for a user (defaults when absent or unreadable). */
export function loadPrefs(userId, storage) {
  const st = storageOf(storage);
  try {
    const raw = st?.getItem(keyFor(userId));
    return sanitizePrefs(raw ? JSON.parse(raw) : null);
  } catch {
    return sanitizePrefs(null);
  }
}

/** Save prefs for a user; returns the sanitised value actually stored. */
export function savePrefs(userId, prefs, storage) {
  const clean = sanitizePrefs(prefs);
  const st = storageOf(storage);
  try {
    st?.setItem(keyFor(userId), JSON.stringify(clean));
  } catch {
    /* storage unavailable: keep in memory only */
  }
  return clean;
}
