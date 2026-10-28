// Search (/music/search?q=): songs, albums, artists and playlists from GET /music/search, fed by
// the top-bar search box (debounced by the shell); "All songs" pages through GET /music/tracks?q=.

import { h } from '../../../core/dom.js';
import * as ui from '../../../core/ui.js';
import { mapi, albumCard, artistCard, playlistCard, createTrackList, section, shelf, trackFeed } from '../widgets.js';
import { pagedTrackList, pageHead } from './common.js';

export function mountSearch(host, app, route) {
  const page = h('div', { class: 'mu-page mu-search' });
  host.append(page);
  let ctrl = null;
  let q = null;
  const lists = [];

  function clearLists() {
    lists.splice(0).forEach((l) => l.destroy());
  }

  async function run(query) {
    q = query;
    ctrl?.abort();
    ctrl = new AbortController();
    const signal = ctrl.signal;
    clearLists();
    app.setTitle(q ? `Search: ${q}` : 'Search');
    if (!q) {
      page.replaceChildren(ui.emptyState({ icon: 'search', title: 'Search your music', message: 'Type in the search box above to find songs, albums, artists and playlists.' }));
      return;
    }
    page.replaceChildren(pageHead(`Results for “${q}”`), ui.loadingBlock('Searching…'));
    let res;
    try {
      res = await mapi.search(q, 20, signal);
    } catch (err) {
      if (signal.aborted) return;
      page.replaceChildren(pageHead(`Results for “${q}”`), ui.errorState(err, () => run(q)));
      return;
    }
    if (signal.aborted) return;
    const ctx = { kind: 'search', id: q };
    const parts = [pageHead(`Results for “${q}”`)];
    const tracks = res.tracks || [];
    if (tracks.length) {
      const list = createTrackList({ label: 'Songs found', context: ctx, art: true, album: true });
      list.setItems(tracks);
      lists.push(list);
      const songsBox = h('div', {}, list.el);
      const all = tracks.length >= 20 ? ui.button({ label: 'All songs', icon: 'list', size: 'sm', variant: 'ghost', onClick: () => {
        all.hidden = true;
        list.destroy();
        const query = { q, sort: 'title' };
        const paged = pagedTrackList({ label: 'All songs found', context: ctx, art: true, album: true }, {
          fetchPage: (cursor) => mapi.tracks({ ...query, cursor, limit: 200 }),
          more: (cursor) => trackFeed(query, cursor),
        });
        lists.push(paged);
        songsBox.replaceChildren(paged.el);
      } }) : null;
      parts.push(section('Songs', songsBox, { actions: all }));
    }
    if (res.albums?.length) parts.push(section('Albums', shelf(res.albums.map((a) => albumCard(a)))));
    if (res.artists?.length) parts.push(section('Artists', shelf(res.artists.map((a) => artistCard(a)))));
    if (res.playlists?.length) parts.push(section('Playlists', shelf(res.playlists.map((p) => playlistCard(p, { me: app.me })))));
    if (parts.length === 1) parts.push(ui.emptyState({ icon: 'search', title: 'Nothing found', message: 'Try fewer or different words. Search looks at titles, artists, albums and file names.' }));
    page.replaceChildren(...parts);
  }

  run((route.query.q || '').trim());
  return {
    update(r) {
      const next = (r.query.q || '').trim();
      if (next !== q) run(next);
    },
    destroy() {
      ctrl?.abort();
      clearLists();
    },
  };
}
