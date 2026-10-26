// TV library — visual building blocks: lazy images with generated placeholder art,
// poster/episode/show cards, list and compact rows, progress bars and watched toggles.

import { h, cls } from '../../../core/dom.js';
import { icon } from '../../../core/icons.js';
import { formatBytes } from '../../../core/format.js';
import {
  itemHref, showHref, playHref, displayTitle, cardSubtitle, sizedUrl, thumbUrl, placeholderArt, progressFraction,
  runtime, resolutionLabel, episodeCode, isEpisode, canResume,
} from './model.js';

/* ------------------------------------------------------------ lazy images */

let io = null;

function loadImage(img) {
  const { src, srcset } = img.dataset;
  if (srcset) img.srcset = srcset;
  if (src) img.src = src;
  delete img.dataset.src;
  delete img.dataset.srcset;
}

function observer() {
  if (io || typeof IntersectionObserver === 'undefined') return io;
  io = new IntersectionObserver((entries) => {
    for (const e of entries) {
      if (!e.isIntersecting) continue;
      io.unobserve(e.target);
      loadImage(e.target);
    }
  }, { rootMargin: '400px 200px' });
  return io;
}

/**
 * An <img> that loads when it approaches the viewport. It fades in when loaded and
 * removes itself on error so the placeholder underneath stays visible.
 */
export function lazyImage({ src, srcset = '', alt = '', className = '' }) {
  const img = h('img', { class: cls('tvl-img', className), alt, decoding: 'async', draggable: 'false' });
  img.dataset.src = src;
  if (srcset) img.dataset.srcset = srcset;
  img.addEventListener('load', () => img.classList.add('is-loaded'), { once: true });
  img.addEventListener('error', () => img.remove(), { once: true });
  const o = observer();
  if (o) o.observe(img);
  else loadImage(img);
  return img;
}

/**
 * Artwork box: generated initials art underneath an optional lazy image.
 * @param {{title: string, kind?: 'poster'|'wide'|'square', src?: string, srcset?: string, className?: string}} o
 */
export function artBox({ title, caption = null, kind = 'poster', src = '', srcset = '', className = '', children = [] }) {
  const art = placeholderArt(title);
  return h('div', {
    class: cls('tvl-art', `tvl-art-${kind}`, className),
    style: { '--h1': String(art.hue), '--h2': String(art.hue2) },
  },
  h('span', { class: 'tvl-art-ph', aria: { hidden: 'true' } },
    h('span', { class: 'tvl-art-initials' }, art.initials),
    kind !== 'square' ? h('span', { class: 'tvl-art-caption' }, caption ?? title) : null),
  src ? lazyImage({ src, srcset }) : null,
  children);
}

/** Poster artwork of a Media object (custom poster or thumbnail). */
export function posterArt(media, { kind = 'poster', size = 'md', children = [] } = {}) {
  const m = media.media || {};
  const url = m.poster_url;
  return artBox({
    title: isEpisode(m) && kind === 'poster' ? m.show_title : displayTitle(m),
    kind, children,
    src: url ? sizedUrl(url, size) : '',
    srcset: url && size !== 'lg' ? `${sizedUrl(url, size)} 1x, ${sizedUrl(url, size === 'sm' ? 'md' : 'lg')} 2x` : '',
  });
}

/** Landscape artwork for episodes: video frame, else poster, else placeholder. */
export function wideArt(media, { children = [] } = {}) {
  const m = media.media || {};
  const frame = thumbUrl(media, 'md');
  const src = frame || (m.poster_url ? sizedUrl(m.poster_url, 'md') : '');
  const srcset = frame ? `${frame} 1x, ${thumbUrl(media, 'lg')} 2x` : '';
  const caption = isEpisode(m) ? [m.show_title, episodeCode(m.season, m.episode)].filter(Boolean).join(' · ') : null;
  return artBox({ title: isEpisode(m) ? m.show_title : displayTitle(m), caption, kind: 'wide', src, srcset, className: frame ? '' : 'tvl-art-cover', children });
}

/* ---------------------------------------------------------------- widgets */

export function progressBar(fraction, { label = '' } = {}) {
  const bar = h('span', { class: 'tvl-progress-bar' });
  bar.style.width = `${Math.round(Math.max(0, Math.min(1, fraction)) * 100)}%`;
  return h('span', {
    class: 'tvl-progress', role: 'progressbar',
    aria: { label: label || 'Watched', valuemin: '0', valuemax: '100', valuenow: String(Math.round(fraction * 100)) },
  }, bar);
}

/** Corner badges: watched check or progress. */
function statusOverlay(m) {
  const out = [];
  if (m.watched) out.push(h('span', { class: 'tvl-watched-badge', dataset: { tip: 'Watched' } }, icon('check', { size: 13, strokeWidth: 2.5 })));
  const f = progressFraction(m);
  if (f > 0) out.push(progressBar(f, { label: 'Progress' }));
  return out;
}

/**
 * Watched toggle button. onToggle(next) must persist and resolve; the button reflects the
 * state optimistically and reverts on failure.
 */
export function watchedToggle(watched, onToggle, { size = 'sm', labelled = false } = {}) {
  let state = !!watched;
  const b = h('button', { type: 'button', class: cls('btn', labelled ? 'btn-secondary' : ['btn-ghost', 'btn-icon'], size === 'sm' && 'btn-sm', 'tvl-watch-toggle') });
  const render = () => {
    b.classList.toggle('is-on', state);
    b.setAttribute('aria-pressed', String(state));
    const label = state ? 'Mark as unwatched' : 'Mark as watched';
    const ic = icon(state ? 'check-circle' : 'circle-dot', { size: size === 'sm' ? 16 : 18 });
    if (labelled) {
      b.removeAttribute('aria-label');
      b.dataset.tip = label;
      b.replaceChildren(ic, h('span', { class: 'btn-label' }, state ? 'Watched' : 'Mark watched'));
      return;
    }
    b.setAttribute('aria-label', label);
    b.dataset.tip = label;
    b.replaceChildren(ic);
  };
  b.addEventListener('click', async (e) => {
    e.preventDefault();
    e.stopPropagation();
    if (b.disabled) return;
    const next = !state;
    state = next;
    render();
    b.disabled = true;
    try {
      await onToggle(next);
    } catch {
      state = !next;
      render();
    } finally {
      b.disabled = false;
    }
  });
  render();
  return b;
}

/* ------------------------------------------------------------------ cards */

/**
 * Poster card for a Media item (grids and rows).
 * @param {object} media
 * @param {{selectable?: boolean, selected?: boolean, onSelect?: (id, ev) => void}} [o]
 */
export function posterCard(media, o = {}) {
  const m = media.media || {};
  const title = displayTitle(m);
  const href = itemHref(media.id);
  const genres = (m.genres || []).slice(0, 3).join(' · ');
  const overlay = h('div', { class: 'tvl-art-overlay', aria: { hidden: 'true' } },
    h('div', { class: 'tvl-overlay-body' },
      h('div', { class: 'tvl-overlay-top' },
        m.content_rating ? h('span', { class: 'tvl-rating' }, m.content_rating) : null,
        resolutionLabel(m.width, m.height) ? h('span', { class: 'tvl-rating' }, resolutionLabel(m.width, m.height)) : null),
      genres ? h('div', { class: 'tvl-overlay-genres' }, genres) : null,
      m.description ? h('p', { class: 'tvl-overlay-desc' }, m.description) : null));
  const card = h('div', { class: cls('tvl-card', { 'is-selected': o.selected }), role: 'listitem', dataset: { id: media.id } },
    h('div', { class: 'tvl-card-media' },
      h('a', { class: 'tvl-card-link', href, aria: { label: [title, cardSubtitle(m)].filter(Boolean).join(', ') } },
        posterArt(media, { children: [overlay, ...statusOverlay(m)] })),
      playButton(media, title),
      o.selectable ? selectBox(media, o) : null),
    h('div', { class: 'tvl-card-meta' },
      h('a', { class: 'tvl-card-title', href, tabindex: '-1' }, title),
      h('div', { class: 'tvl-card-sub' }, cardSubtitle(m) || ' ')));
  return card;
}

/** Round play button shown over the artwork on hover/focus (resumes when possible). */
function playButton(media, title) {
  const m = media.media || {};
  return h('a', { class: 'tvl-card-play', href: playHref(media.id, canResume(m) ? m.position_seconds : null), aria: { label: `Play ${title}` }, dataset: { tip: canResume(m) ? 'Resume' : 'Play' } },
    icon('play', { size: 18 }));
}

function selectBox(media, o) {
  const cb = h('input', { type: 'checkbox', class: 'checkbox tvl-select', checked: !!o.selected, aria: { label: `Select ${displayTitle(media.media || {})}` } });
  cb.addEventListener('click', (e) => {
    e.stopPropagation();
    o.onSelect?.(media.id, e, cb.checked);
  });
  return cb;
}

/** Landscape card for episodes / continue watching. */
export function wideCard(media, { showName = true } = {}) {
  const m = media.media || {};
  const href = itemHref(media.id);
  const ep = isEpisode(m);
  const title = ep && showName ? m.show_title : displayTitle(m);
  const sub = ep ? [episodeCode(m.season, m.episode), m.episode_title].filter(Boolean).join(' · ') : cardSubtitle(m);
  return h('div', { class: 'tvl-card tvl-card-wide', role: 'listitem', dataset: { id: media.id } },
    h('div', { class: 'tvl-card-media' },
      h('a', { class: 'tvl-card-link', href, aria: { label: [title, sub].filter(Boolean).join(', ') } },
        wideArt(media, { children: statusOverlay(m) })),
      playButton(media, title)),
    h('div', { class: 'tvl-card-meta' },
      h('a', { class: 'tvl-card-title', href, tabindex: '-1' }, title),
      h('div', { class: 'tvl-card-sub' }, sub || ' ')));
}

/** Show card for the shows grid. */
export function showCard(show) {
  const href = showHref(show.show_key);
  const sub = [show.seasons === 1 ? '1 season' : `${show.seasons} seasons`, show.episodes === 1 ? '1 episode' : `${show.episodes} episodes`].join(' · ');
  const frac = show.episodes ? show.watched / show.episodes : 0;
  const children = [];
  if (show.episodes && show.watched >= show.episodes) {
    children.push(h('span', { class: 'tvl-watched-badge', dataset: { tip: 'All watched' } }, icon('check', { size: 13, strokeWidth: 2.5 })));
  } else if (frac > 0) children.push(progressBar(frac, { label: 'Episodes watched' }));
  const url = show.poster_url;
  return h('div', { class: 'tvl-card', role: 'listitem' },
    h('a', { class: 'tvl-card-link', href, aria: { label: `${show.show_title}, ${sub}` } },
      artBox({ title: show.show_title, src: url ? sizedUrl(url, 'md') : '', srcset: url ? `${sizedUrl(url, 'md')} 1x, ${sizedUrl(url, 'lg')} 2x` : '', children })),
    h('div', { class: 'tvl-card-meta' },
      h('a', { class: 'tvl-card-title', href, tabindex: '-1' }, show.show_title),
      h('div', { class: 'tvl-card-sub' }, sub)));
}

/**
 * Row for the list / compact views.
 * @param {object} media
 * @param {{compact?: boolean, selected?: boolean, onSelect?: Function, onWatched?: (next) => Promise, menu?: () => Array}} o
 */
export function listRow(media, o = {}) {
  const m = media.media || {};
  const title = displayTitle(m);
  const href = itemHref(media.id);
  const res = resolutionLabel(m.width, m.height);
  const sub = isEpisode(m)
    ? [m.show_title, episodeCode(m.season, m.episode)].filter(Boolean).join(' · ')
    : (m.genres || []).slice(0, 3).join(', ');
  const cells = [
    h('span', { class: 'tvl-row-check' }, selectBox(media, o)),
    o.compact ? null : h('a', { class: 'tvl-row-art', href, tabindex: '-1', aria: { hidden: 'true' } }, posterArt(media, { size: 'sm', kind: 'poster' })),
    h('span', { class: 'tvl-row-main' },
      h('a', { class: 'tvl-row-title', href }, title),
      sub ? h('span', { class: 'tvl-row-sub' }, sub) : null,
      !o.compact && progressFraction(m) > 0 ? progressBar(progressFraction(m)) : null),
    h('span', { class: 'tvl-row-year nums' }, m.year ? String(m.year) : ''),
    h('span', { class: 'tvl-row-dur nums' }, runtime(m.duration_seconds)),
    o.compact ? h('span', { class: 'tvl-row-size nums' }, formatBytes(media.size)) : null,
    h('span', { class: 'tvl-row-badges' },
      res ? h('span', { class: 'badge' }, res) : null,
      o.compact && m.video_codec ? h('span', { class: 'badge' }, m.video_codec.toUpperCase()) : null),
    h('span', { class: 'tvl-row-actions' },
      o.onWatched ? watchedToggle(m.watched, o.onWatched) : null,
      h('a', { class: 'btn btn-ghost btn-icon btn-sm', href: playHref(media.id, canResume(m) ? m.position_seconds : null), aria: { label: `Play ${title}` }, dataset: { tip: 'Play' } }, icon('play', 14))),
  ];
  return h('div', { class: cls('tvl-row', { 'tvl-row-compact': o.compact, 'is-selected': o.selected, 'is-watched': m.watched }), role: 'listitem', dataset: { id: media.id } }, cells);
}

/** Skeleton placeholders for a grid of poster cards. */
export function skeletonGrid(n = 12, kind = 'poster') {
  return Array.from({ length: n }, () => h('div', { class: 'tvl-card tvl-card-skel', aria: { hidden: 'true' } },
    h('div', { class: cls('tvl-art', `tvl-art-${kind}`, 'skeleton') }),
    h('div', { class: 'tvl-card-meta' }, h('span', { class: 'skeleton tvl-skel-line' }), h('span', { class: 'skeleton tvl-skel-line short' }))));
}
