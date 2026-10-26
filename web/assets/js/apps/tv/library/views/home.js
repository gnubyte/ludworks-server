// TV library — home: continue watching, recently added movies and episodes, shows,
// genres and other videos.

import { h } from '../../../../core/dom.js';
import { icon } from '../../../../core/icons.js';
import { plural } from '../../../../core/format.js';
import { posterCard, wideCard, showCard, skeletonGrid } from '../cards.js';
import { shelf, emptyLibrary, showError } from './common.js';
import { libraryHref } from '../model.js';

/**
 * @param {HTMLElement} root
 * @param {{ctx: object, api: ReturnType<typeof import('../api.js').mediaApi>, signal: AbortSignal, autoTag: () => void}} env
 */
export async function renderHome(root, env) {
  const { ctx, api, signal } = env;
  const { ui } = ctx;
  root.replaceChildren(h('div', { class: 'tvl-home' },
    h('section', { class: 'tvl-shelf' }, h('div', { class: 'tvl-shelf-track' }, skeletonGrid(8)))));
  const opts = { signal };
  let res;
  try {
    res = await Promise.all([
      api.list({ in_progress: 1, sort: 'progress', order: 'desc', limit: 20 }, opts),
      api.list({ category: 'movie', sort: 'added', order: 'desc', limit: 20 }, opts),
      api.list({ category: 'tv', sort: 'added', order: 'desc', limit: 20 }, opts),
      api.shows({ limit: 24 }, opts),
      api.facets({}, opts),
      api.list({ category: 'none', sort: 'added', order: 'desc', limit: 20 }, opts),
    ]);
  } catch (err) {
    showError(root, ui, err, () => renderHome(root, env));
    return;
  }
  if (signal.aborted) return;
  const [cont, movies, episodes, shows, facets, other] = res;
  const out = h('div', { class: 'tvl-home' });
  const total = movies.items.length + episodes.items.length + other.items.length;
  if (!total) {
    root.replaceChildren(emptyLibrary(ui, { onAutoTag: env.autoTag }));
    return;
  }
  const uncategorized = facets.categories?.find((c) => c.value === 'none')?.count || 0;
  if (uncategorized > 0) {
    out.append(h('div', { class: 'tvl-banner' },
      icon('sparkle', { size: 18 }),
      h('div', { class: 'tvl-banner-text' },
        h('strong', {}, `${plural(uncategorized, 'video')} without a category`),
        h('span', {}, ' — auto-tag a folder to recognise movies and episodes from their names and .nfo files.')),
      ui.button({ label: 'Auto-tag', size: 'sm', onClick: env.autoTag })));
  }
  if (cont.items.length) {
    out.append(shelf({ title: 'Continue watching', items: cont.items.map((m) => wideCard(m)), wide: true }));
  }
  if (movies.items.length) {
    out.append(shelf({ title: 'Recently added movies', href: libraryHref('recent', { category: 'movie' }), items: movies.items.map((m) => posterCard(m)) }));
  }
  if (episodes.items.length) {
    out.append(shelf({ title: 'Recently added episodes', href: libraryHref('recent', { category: 'tv' }), items: episodes.items.map((m) => wideCard(m)), wide: true }));
  }
  if (shows.items.length) {
    out.append(shelf({ title: 'Shows', href: libraryHref('shows'), items: shows.items.map((s) => showCard(s)) }));
  }
  if (facets.genres?.length) {
    out.append(h('section', { class: 'tvl-shelf' },
      h('header', { class: 'tvl-shelf-head' }, h('h2', { class: 'tvl-shelf-title' }, h('a', { href: libraryHref('genres') }, 'Genres', icon('chevron-right', { size: 16 })))),
      h('div', { class: 'tvl-genre-chips' }, facets.genres.slice(0, 24).map((g) => h('a', {
        class: 'tvl-genre-chip', href: libraryHref('browse', { genre: g.value }),
      }, g.value, h('span', { class: 'tvl-genre-count' }, String(g.count)))))));
  }
  if (other.items.length) {
    out.append(shelf({ title: 'Other videos', href: libraryHref('videos'), items: other.items.map((m) => wideCard(m)), wide: true }));
  }
  root.replaceChildren(out);
}
