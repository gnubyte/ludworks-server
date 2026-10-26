// TV library — organisational overviews: genres and years (by decade).

import { h } from '../../../../core/dom.js';
import { plural } from '../../../../core/format.js';
import { viewHeader, showError } from './common.js';
import { libraryHref, placeholderArt, decades } from '../model.js';

async function loadFacets(root, env) {
  root.replaceChildren(h('div', { class: 'tvl-browse' }, h('div', { class: 'tvl-tiles' },
    Array.from({ length: 8 }, () => h('div', { class: 'tvl-tile skeleton' })))));
  try {
    return await env.api.facets({}, { signal: env.signal });
  } catch (err) {
    showError(root, env.ctx.ui, err, () => (env.view === 'years' ? renderYears(root, env) : renderGenres(root, env)));
    return null;
  }
}

function tile(label, count, href) {
  const art = placeholderArt(label);
  return h('a', { class: 'tvl-tile', href, style: { '--h1': String(art.hue), '--h2': String(art.hue2) } },
    h('span', { class: 'tvl-tile-label' }, label),
    h('span', { class: 'tvl-tile-count' }, plural(count, 'title')));
}

export async function renderGenres(root, env) {
  const { ui } = env.ctx;
  const f = await loadFacets(root, env);
  if (!f || env.signal.aborted) return {};
  const genres = f.genres || [];
  root.replaceChildren(h('div', { class: 'tvl-browse' },
    viewHeader({ title: 'Genres', subtitle: genres.length ? plural(genres.length, 'genre') : '' }),
    genres.length
      ? h('div', { class: 'tvl-tiles', role: 'list' }, genres.map((g) => tile(g.value, g.count, libraryHref('browse', { genre: g.value }))))
      : h('div', { class: 'tvl-state' }, ui.emptyState({
        icon: 'tag', title: 'No genres yet',
        message: 'Genres come from .nfo files, TMDB identification or your own edits.',
      }))));
  return {};
}

export async function renderYears(root, env) {
  const { ui } = env.ctx;
  const f = await loadFacets(root, { ...env, view: 'years' });
  if (!f || env.signal.aborted) return {};
  const groups = decades(f.years || []);
  root.replaceChildren(h('div', { class: 'tvl-browse' },
    viewHeader({ title: 'By year' }),
    groups.length
      ? h('div', { class: 'stack-lg' }, groups.map((d) => h('section', { class: 'tvl-decade' },
        h('h2', { class: 'tvl-shelf-title' }, `${d.decade}s`, h('span', { class: 'tvl-shelf-sub' }, plural(d.count, 'title'))),
        h('div', { class: 'tvl-year-chips' }, d.years.map((y) => h('a', { class: 'tvl-genre-chip', href: libraryHref('browse', { year: y.value, sort: 'title' }) },
          String(y.value), h('span', { class: 'tvl-genre-count' }, String(y.count))))))))
      : h('div', { class: 'tvl-state' }, ui.emptyState({ icon: 'calendar', title: 'No years yet', message: 'Release years come from file names, .nfo files or TMDB.' }))));
  return {};
}
