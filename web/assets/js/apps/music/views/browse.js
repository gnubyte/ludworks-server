// Albums and artists (art grids loaded page by page, sortable) and genres (colour tiles).

import { h } from '../../../core/dom.js';
import * as ui from '../../../core/ui.js';
import { on } from '../state.js';
import { mapi, albumCard, artistCard, genreTile, cardGrid } from '../widgets.js';
import { ALBUM_SORTS, ARTIST_SORTS, normalizeSort } from '../model.js';
import { pageHead, sortControl, toolbar, pagedGrid, cached, remember, loadError } from './common.js';

/** /music/albums */
export function mountAlbums(host, app, route) {
  app.setTitle('Albums');
  let st = normalizeSort(ALBUM_SORTS, route.query.sort, route.query.order);
  const page = h('div', { class: 'mu-page' });
  host.append(page);
  let grid = null;

  function build() {
    grid?.destroy();
    const key = `albums:${st.sort}:${st.order}`;
    grid = pagedGrid({
      fetchPage: (cursor) => mapi.albums({ sort: st.sort, order: st.order, cursor, limit: 100 }),
      render: (a) => albumCard(a),
      initial: cached(key),
      onItems: (all, cursor) => remember(key, all, cursor),
      empty: () => ui.emptyState({ icon: 'disc', title: 'No albums yet', message: 'Albums come from the album tags of your music files.' }),
    });
    page.replaceChildren(
      pageHead('Albums'),
      toolbar(sortControl(ALBUM_SORTS, st, (v) => app.setQuery({ sort: v.sort, order: v.order }))),
      grid.el);
  }
  build();
  const off = on('favorite', (f) => {
    if (f.kind === 'album') for (const a of grid?.items || []) if (a.id === f.id) a.favorite = f.favorite;
  });
  return {
    update(r) {
      const next = normalizeSort(ALBUM_SORTS, r.query.sort, r.query.order);
      if (next.sort !== st.sort || next.order !== st.order) {
        st = next;
        build();
      }
    },
    destroy() {
      off();
      grid?.destroy();
    },
  };
}

/** /music/artists (album artists; "All artists" adds artists that only appear on others' albums) */
export function mountArtists(host, app, route) {
  app.setTitle('Artists');
  let st = normalizeSort(ARTIST_SORTS, route.query.sort, route.query.order);
  let role = route.query.role === 'all' ? 'all' : 'album';
  const page = h('div', { class: 'mu-page' });
  host.append(page);
  let grid = null;

  function build() {
    grid?.destroy();
    const key = `artists:${st.sort}:${st.order}:${role}`;
    grid = pagedGrid({
      className: 'mu-grid-artists',
      fetchPage: (cursor) => mapi.artists({ sort: st.sort, order: st.order, role, cursor, limit: 100 }),
      render: (a) => artistCard(a),
      initial: cached(key),
      onItems: (all, cursor) => remember(key, all, cursor),
      empty: () => ui.emptyState({ icon: 'mic', title: 'No artists yet', message: 'Artists come from the artist tags of your music files.' }),
    });
    const roleCtl = ui.segmented({
      options: [{ value: 'album', label: 'Album artists' }, { value: 'all', label: 'All artists' }], value: role, size: 'sm', ariaLabel: 'Which artists',
      onChange: (v) => app.setQuery({ role: v === 'all' ? 'all' : null }),
    });
    page.replaceChildren(
      pageHead('Artists'),
      toolbar(sortControl(ARTIST_SORTS, st, (v) => app.setQuery({ sort: v.sort, order: v.order })), roleCtl.el),
      grid.el);
  }
  build();
  return {
    update(r) {
      const next = normalizeSort(ARTIST_SORTS, r.query.sort, r.query.order);
      const nextRole = r.query.role === 'all' ? 'all' : 'album';
      if (next.sort !== st.sort || next.order !== st.order || nextRole !== role) {
        st = next;
        role = nextRole;
        build();
      }
    },
    destroy() {
      grid?.destroy();
    },
  };
}

/** /music/genres */
export function mountGenres(host, app) {
  app.setTitle('Genres');
  const ctrl = new AbortController();
  const page = h('div', { class: 'mu-page' });
  host.append(page);
  const filter = h('input', { class: 'input mu-filter', type: 'search', placeholder: 'Filter genres', aria: { label: 'Filter genres' } });
  const grid = cardGrid('mu-genres');
  let all = [];

  function render() {
    const q = filter.value.trim().toLowerCase();
    const shown = all.filter((g) => !q || g.name.toLowerCase().includes(q));
    grid.replaceChildren(...(shown.length ? shown.map(genreTile) : [ui.emptyState({ icon: 'tag', title: all.length ? 'No matching genres' : 'No genres yet', compact: true, message: all.length ? '' : 'Genres come from the genre tags of your music files.' })]));
  }

  async function load() {
    page.replaceChildren(pageHead('Genres'), ui.loadingBlock('Loading genres…'));
    try {
      all = await mapi.genres(ctrl.signal) || [];
    } catch (err) {
      if (!ctrl.signal.aborted) page.replaceChildren(loadError(err, load));
      return;
    }
    if (ctrl.signal.aborted) return;
    filter.addEventListener('input', render);
    page.replaceChildren(pageHead('Genres', { subtitle: all.length ? `${all.length} ${all.length === 1 ? 'genre' : 'genres'}` : '' }), all.length > 12 ? toolbar(filter) : null, grid);
    render();
  }
  load();
  return { destroy: () => ctrl.abort() };
}
