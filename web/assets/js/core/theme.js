// Theme preference: "system" (default) | "light" | "dark", persisted per browser.
// theme-boot.js applies the stored override before first paint to avoid a flash.

const KEY = 'lw.theme';
const listeners = new Set();
let media = null;

function storage() {
  try {
    return globalThis.localStorage ?? null;
  } catch {
    return null;
  }
}

/** @returns {'system'|'light'|'dark'} */
export function getThemePref() {
  try {
    const v = storage()?.getItem(KEY);
    return v === 'light' || v === 'dark' ? v : 'system';
  } catch {
    return 'system';
  }
}

/** @returns {'light'|'dark'} the OS preference */
export function systemTheme() {
  return globalThis.matchMedia?.('(prefers-color-scheme: dark)').matches ? 'dark' : 'light';
}

/** @returns {'light'|'dark'} the theme actually shown */
export function effectiveTheme() {
  const p = getThemePref();
  return p === 'system' ? systemTheme() : p;
}

/** Apply the current preference to <html>. */
export function applyTheme() {
  const root = document.documentElement;
  const pref = getThemePref();
  if (pref === 'system') root.removeAttribute('data-theme');
  else root.setAttribute('data-theme', pref);
  const eff = effectiveTheme();
  root.style.colorScheme = eff;
  for (const meta of document.querySelectorAll('meta[name="theme-color"]')) {
    // With an override, both media variants must show the chosen theme colour.
    const isDarkMeta = (meta.getAttribute('media') || '').includes('dark');
    const theme = pref === 'system' ? (isDarkMeta ? 'dark' : 'light') : eff;
    meta.setAttribute('content', theme === 'dark' ? '#0e0e11' : '#fbfbfc');
  }
}

function emit() {
  const eff = effectiveTheme();
  for (const fn of [...listeners]) fn(eff, getThemePref());
}

/**
 * Persist a theme preference and apply it.
 * @param {'system'|'light'|'dark'} pref
 */
export function setThemePref(pref) {
  try {
    if (pref === 'light' || pref === 'dark') storage()?.setItem(KEY, pref);
    else storage()?.removeItem(KEY);
  } catch {
    /* storage unavailable (private mode): apply for this page only */
    if (pref === 'light' || pref === 'dark') document.documentElement.setAttribute('data-theme', pref);
  }
  applyTheme();
  emit();
}

/** Flip between light and dark (sets an explicit override). */
export function toggleTheme() {
  setThemePref(effectiveTheme() === 'dark' ? 'light' : 'dark');
}

/** Listen for effective theme changes. @returns {() => void} */
export function onThemeChange(fn) {
  listeners.add(fn);
  return () => listeners.delete(fn);
}

/** Apply the theme and follow OS changes while the preference is "system". */
export function initTheme() {
  applyTheme();
  if (!media && globalThis.matchMedia) {
    media = matchMedia('(prefers-color-scheme: dark)');
    media.addEventListener('change', () => {
      applyTheme();
      emit();
    });
  }
}
