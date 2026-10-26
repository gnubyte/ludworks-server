// Small TV widgets shared by the views: channel logos, posters, program text, buttons.

import { h, cls } from '../../core/dom.js';
import { formatTime, hueFor } from '../../core/format.js';
import { tvIcon } from './icons.js';
import { programTitle, programSubtitle } from './model.js';

/**
 * Channel logo with a numbered fallback tile (hue derived from the name).
 * @param {{id: string, number: number, name: string, logo_url?: string|null}} ch
 * @param {{size?: 'sm'|'md'|'lg'|'xl', className?: string}} [o]
 */
export function channelLogo(ch, { size = 'md', className = '' } = {}) {
  const tile = h('span', {
    class: cls('tv-logo', `tv-logo-${size}`, className),
    style: { '--hue': String(hueFor(ch.name || String(ch.number))) },
    aria: { hidden: 'true' },
  });
  const fallback = () => tile.replaceChildren(h('span', { class: 'tv-logo-num' }, String(ch.number)));
  if (ch.logo_url) {
    const img = h('img', { src: ch.logo_url, alt: '', loading: 'lazy', decoding: 'async', draggable: 'false' });
    img.addEventListener('error', fallback, { once: true });
    tile.classList.add('has-img');
    tile.append(img);
  } else {
    fallback();
  }
  return tile;
}

/** Poster image with an icon placeholder when missing or broken. */
export function poster(url, { className = '', iconName = 'film', alt = '' } = {}) {
  const box = h('span', { class: cls('tv-poster', className) });
  const ph = () => {
    box.classList.add('is-empty');
    box.replaceChildren(tvIcon(iconName, { size: 22 }));
  };
  if (url) {
    const img = h('img', { src: url, alt, loading: 'lazy', decoding: 'async', draggable: 'false' });
    img.addEventListener('error', ph, { once: true });
    box.append(img);
  } else {
    ph();
  }
  return box;
}

/** "14:00 – 14:30" for a program. */
export function timeRange(p) {
  return p ? `${formatTime(p.starts_at)} – ${formatTime(p.ends_at)}` : '';
}

/** Title + subtitle block for a program. */
export function programText(p, { className = '' } = {}) {
  if (!p) return h('div', { class: cls('tv-prog-text', className) }, h('div', { class: 'tv-prog-title faint' }, 'Nothing on air'));
  const sub = programSubtitle(p);
  return h('div', { class: cls('tv-prog-text', className) },
    h('div', { class: 'tv-prog-title truncate', title: programTitle(p) }, programTitle(p)),
    sub ? h('div', { class: 'tv-prog-sub truncate', title: sub }, sub) : null);
}

/** Thin progress bar; returns {el, set(fraction)}. */
export function miniProgress(label = 'Progress') {
  const bar = h('span', { class: 'tv-mini-bar' });
  const el = h('span', { class: 'tv-mini-progress', role: 'progressbar', aria: { label, valuemin: '0', valuemax: '100' } }, bar);
  return {
    el,
    set(f) {
      const pct = Math.round(Math.max(0, Math.min(1, f || 0)) * 1000) / 10;
      bar.style.width = `${pct}%`;
      el.setAttribute('aria-valuenow', String(Math.round(pct)));
    },
  };
}

/**
 * Icon button using the TV icon set (core button styles).
 * @param {{icon: string, title: string, onClick?: (e: Event) => void, className?: string, variant?: string, size?: string, pressed?: boolean}} o
 */
export function iconButton({ icon: ic, title, onClick, className = '', variant = 'ghost', size = 'md', pressed }) {
  const b = h('button', {
    type: 'button', class: cls('btn', `btn-${variant}`, 'btn-icon', size !== 'md' && `btn-${size}`, className),
    aria: { label: title, pressed: pressed === undefined ? undefined : String(!!pressed) }, dataset: { tip: title },
  }, tvIcon(ic, { size: size === 'sm' ? 14 : size === 'lg' ? 20 : 18 }));
  if (onClick) b.addEventListener('click', onClick);
  return b;
}

/** Live "on air" pill. */
export function liveBadge(text = 'Live') {
  return h('span', { class: 'tv-live-badge' }, h('span', { class: 'tv-live-dot', aria: { hidden: 'true' } }), text);
}

/** Persist a small per-browser preference (storage may be unavailable). */
export function pref(key, value) {
  try {
    if (value === undefined) return localStorage.getItem(key);
    if (value === null) localStorage.removeItem(key);
    else localStorage.setItem(key, String(value));
  } catch { /* storage unavailable */ }
  return null;
}

// Moved to core (ui.tabs now reveals the active tab itself); re-exported for callers.
export { revealActiveTab } from '../../core/ui.js';
