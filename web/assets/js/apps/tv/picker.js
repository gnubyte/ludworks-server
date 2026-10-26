// Built-in fallbacks used when the library module (./library/) is unavailable:
//  - openMediaPicker: a simple search picker for shows and media items (lineup editor),
//  - renderFallbackLibrary: a minimal searchable library grid (/tv/library).

import { h, cls, debounce, uid } from '../../core/dom.js';
import { formatDuration, plural } from '../../core/format.js';
import { poster } from './shared.js';
import { episodeCode, isId } from './model.js';
import { tvIcon } from './icons.js';

function mediaLabel(m) {
  const md = m.media || {};
  if (md.show_title) return { title: md.show_title, sub: [episodeCode(md), md.episode_title].filter(Boolean).join(' · ') };
  return { title: md.title || m.name, sub: md.year ? String(md.year) : '' };
}

/**
 * Search picker. Resolves with the picked objects (show summaries and Media items) or null.
 * @param {{api: any, ui: any, signal: AbortSignal}} vctx
 */
export function openMediaPicker({ api, ui, signal }, { multiple = true } = {}) {
  const picked = new Map(); // key → object
  const q = h('input', { class: 'input', type: 'search', placeholder: 'Search shows, movies and episodes', autocomplete: 'off', aria: { label: 'Search the library' } });
  const results = h('div', { class: 'tv-picker-results', role: 'listbox', aria: { multiselectable: String(multiple), label: 'Results' } });
  const count = h('span', { class: 'small muted' });
  let seq = 0;

  function row(key, obj, { icon: ic, title, sub, meta }) {
    const cb = h('input', { type: multiple ? 'checkbox' : 'radio', name: 'pick', class: 'checkbox', checked: picked.has(key), aria: { label: title } });
    const el = h('label', { class: cls('tv-picker-row', { 'is-picked': picked.has(key) }) }, cb,
      h('span', { class: 'tv-picker-icon' }, tvIcon(ic, 16)),
      h('span', { class: 'tv-picker-text' }, h('span', { class: 'truncate bold' }, title), sub ? h('span', { class: 'truncate small muted' }, sub) : null),
      meta ? h('span', { class: 'small faint nums' }, meta) : null);
    cb.addEventListener('change', () => {
      if (!multiple) picked.clear();
      if (cb.checked) picked.set(key, obj);
      else picked.delete(key);
      if (!multiple) for (const r of results.querySelectorAll('.tv-picker-row')) r.classList.remove('is-picked');
      el.classList.toggle('is-picked', cb.checked);
      count.textContent = picked.size ? `${plural(picked.size, 'item')} selected` : '';
      m.buttons[1].disabled = picked.size === 0;
    });
    return el;
  }

  async function search() {
    const my = ++seq;
    results.replaceChildren(ui.loadingBlock('Searching…'));
    try {
      const [shows, media] = await Promise.all([
        api.shows({ q: q.value.trim() || undefined, limit: 20 }, signal).catch(() => ({ items: [] })),
        api.mediaList({ q: q.value.trim() || undefined, limit: 50, sort: 'title' }, signal),
      ]);
      if (my !== seq) return;
      const rows = [];
      for (const s of shows.items || []) {
        rows.push(row(`show:${s.show_title}`, s, { icon: 'tv', title: s.show_title, sub: `Show · all episodes`, meta: s.episodes ? plural(s.episodes, 'episode') : '' }));
      }
      for (const it of media.items || []) {
        const l = mediaLabel(it);
        rows.push(row(`file:${it.id}`, it, { icon: it.media?.show_title ? 'layers' : 'film', title: l.title, sub: l.sub || it.name, meta: it.media?.duration_seconds ? formatDuration(it.media.duration_seconds) : 'no duration' }));
      }
      results.replaceChildren(...(rows.length ? rows : [ui.emptyState({ icon: 'search', title: 'Nothing found', compact: true })]));
    } catch (err) {
      if (my === seq) results.replaceChildren(ui.errorState(err, () => search()));
    }
  }
  q.addEventListener('input', debounce(search, 250));
  const m = ui.modal({
    title: 'Add from the library', size: 'lg', className: 'tv-picker',
    body: h('div', { class: 'stack-md' }, q, results),
    footer: [count, { label: 'Cancel', value: null }, { label: 'Add', variant: 'primary', disabled: true, onClick: (mm) => mm.close([...picked.values()]) }],
  });
  search();
  return m.closed.then((v) => (Array.isArray(v) && v.length ? v : null));
}

/**
 * Minimal library: searchable poster grid linking to the on-demand player, and a simple item
 * page for /tv/library/item/<id>.
 */
export async function renderFallbackLibrary(host, { api, ui, signal, rest }) {
  const parts = String(rest || '').split('/').filter(Boolean);
  if (parts[0] === 'item' && isId(parts[1])) {
    const page = h('div', { class: 'page' }, ui.loadingBlock());
    host.append(page);
    try {
      const it = await api.media(parts[1], signal);
      const l = mediaLabel(it);
      const md = it.media || {};
      page.replaceChildren(h('div', { class: 'tv-lib-item' },
        poster(md.poster_url, { className: 'tv-lib-item-poster', iconName: md.show_title ? 'tv' : 'film' }),
        h('div', { class: 'stack-md' },
          ui.breadcrumbs([{ label: 'Library', href: '/tv/library', icon: 'film' }, { label: l.title }]),
          h('h1', { class: 'page-title' }, l.title),
          l.sub ? h('p', { class: 'muted' }, l.sub) : null,
          md.description ? h('p', {}, md.description) : null,
          h('div', { class: 'row wrap' },
            ui.button({ label: md.position_seconds > 10 ? `Resume (${formatDuration(md.position_seconds)})` : 'Play', icon: 'play', variant: 'primary', href: `/tv/play/${it.id}` }),
            md.duration_seconds ? ui.badge(formatDuration(md.duration_seconds), 'neutral', { icon: 'clock' }) : null))));
    } catch (err) {
      if (!signal.aborted) page.replaceChildren(ui.errorState(err));
    }
    return null;
  }
  const qid = uid('libq');
  const q = h('input', { class: 'input', id: qid, type: 'search', placeholder: 'Search the library', autocomplete: 'off', aria: { label: 'Search the library' } });
  const grid = h('div', { class: 'tv-lib-grid' });
  const page = h('div', { class: 'page wide' },
    ui.pageHeader({ title: 'Library', subtitle: 'Movies and episodes from your files.' }),
    h('div', { class: 'toolbar' }, q), grid);
  host.append(page);
  let seq = 0;
  async function load() {
    const my = ++seq;
    grid.replaceChildren(...ui.skeletonCards(8, '220px'));
    try {
      const r = await api.mediaList({ q: q.value.trim() || undefined, limit: 120, sort: 'title' }, signal);
      if (my !== seq) return;
      const items = r.items || [];
      grid.replaceChildren(...(items.length ? items.map((it) => {
        const l = mediaLabel(it);
        return h('a', { class: 'tv-lib-card', href: `/tv/play/${it.id}` },
          poster(it.media?.poster_url, { iconName: it.media?.show_title ? 'tv' : 'film' }),
          h('span', { class: 'truncate bold' }, l.title), h('span', { class: 'truncate small muted' }, l.sub || ' '));
      }) : [ui.emptyState({ icon: 'film', title: 'No media yet', message: 'Upload movies or episodes in Files; they appear here once indexed.' })]));
    } catch (err) {
      if (my === seq && !signal.aborted) grid.replaceChildren(ui.errorState(err, () => load()));
    }
  }
  q.addEventListener('input', debounce(load, 250));
  await load();
  return null;
}
