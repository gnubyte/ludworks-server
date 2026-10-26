// Shared pieces for the signed-out pages: centered card layout, device naming,
// and URL token scrubbing.

import { h, cls } from '../core/dom.js';
import { icon, logoMark } from '../core/icons.js';
import { formBanner } from '../core/forms.js';

/**
 * Centered auth card.
 * @param {{title: string, subtitle?: any, icon?: string, iconVariant?: ''|'success'|'danger', body?: any,
 *          footer?: any, wide?: boolean, settings?: object}} o
 */
export function authLayout({ title, subtitle = '', icon: ic, iconVariant = '', body = null, footer = null, wide = false, settings = {} }) {
  const meta = [settings.server_name || 'Ludworks', settings.version ? `v${settings.version}` : null].filter(Boolean).join(' · ');
  return h('div', { class: 'auth' },
    h('div', { class: cls('auth-card', { wide }) },
      ic ? h('div', { class: cls('auth-icon', iconVariant) }, icon(ic, { size: 24 })) : h('div', { class: 'auth-brand' }, logoMark(40)),
      h('h1', { class: 'auth-title' }, title),
      subtitle ? h('p', { class: 'auth-subtitle' }, subtitle) : null,
      body),
    footer ? h('div', { class: 'auth-footer' }, footer) : null,
    h('div', { class: 'auth-meta' }, icon('lock', 12), meta));
}

/** Short human device label for the sessions list, e.g. "Firefox on Linux". */
export function deviceName(ua = navigator.userAgent) {
  const browser = /Edg\//.test(ua) ? 'Edge' : /OPR\//.test(ua) ? 'Opera' : /Firefox\//.test(ua) ? 'Firefox'
    : /Chrome\//.test(ua) ? 'Chrome' : /Safari\//.test(ua) ? 'Safari' : 'Browser';
  const os = /Windows/.test(ua) ? 'Windows' : /Android/.test(ua) ? 'Android' : /iPhone|iPad|iPod/.test(ua) ? 'iOS'
    : /Mac OS X/.test(ua) ? 'macOS' : /CrOS/.test(ua) ? 'ChromeOS' : /Linux/.test(ua) ? 'Linux' : '';
  return `${browser}${os ? ` on ${os}` : ''} (web)`;
}

/**
 * Remove sensitive query parameters (tokens) from the address bar without
 * navigating, so they don't linger in history, screenshots or bookmarks.
 * @param {string[]} keys
 */
export function scrubQuery(keys) {
  const u = new URL(location.href);
  let changed = false;
  for (const k of keys) {
    if (u.searchParams.has(k)) {
      u.searchParams.delete(k);
      changed = true;
    }
  }
  if (changed) history.replaceState(history.state, '', u.pathname + u.search + u.hash);
}

/** Replace any banner in `host` with a new one (or clear when message is empty). */
export function setBanner(host, message, variant = 'danger') {
  host.querySelectorAll('.form-error-banner').forEach((b) => b.remove());
  if (message) host.prepend(formBanner(message, variant));
}

/** Friendly text for common auth errors. */
export function authErrorText(err) {
  if (!err) return '';
  switch (err.code) {
    case 'invalid_credentials': return 'Incorrect username or password.';
    case 'forbidden': return err.message || 'This account is disabled. Contact your administrator.';
    case 'rate_limited': return `Too many attempts. Try again${err.retryAfter ? ` in ${err.retryAfter} seconds` : ' later'}.`;
    case 'registration_disabled': return 'Registration is currently closed on this server.';
    case 'network_error': return err.message;
    default: return err.message || 'Something went wrong. Please try again.';
  }
}
