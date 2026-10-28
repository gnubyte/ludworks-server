// Your music: favorites (songs, albums, artists) and the play history (with "Clear history").

import { h } from '../../../core/dom.js';
import * as ui from '../../../core/ui.js';
import { formatRelative } from '../../../core/format.js';
import { on } from '../state.js';
import { mapi, albumCard, artistCard, trackFeed, dialogs } from '../widgets.js';
import { pageHead, toolbar, pagedGrid, pagedTrackList } from './common.js';

const FAV_TABS = [
  { id: 'songs', label: 'Songs', icon: 'music' },
  { id: 'albums', label: 'Albums', icon: 'disc' },
  { id: 'artists', label: 'Artists', icon: 'mic' },
];

/** /music/favorites?tab=songs|albums|artists */
export function mountFavorites(host, app, route) {
  app.setTitle('Favorites');
  let tab = FAV_TABS.some((t) => t.id === route.query.tab) ? route.query.tab : 'songs';
  const page = h('div', { class: 'mu-page' });
  host.append(page);
  const body = h('div');
  const tabs = ui.tabs({
    items: FAV_TABS.map((t) => ({ id: t.id, label: t.label, icon: t.icon })), active: tab, variant: 'pills', ariaLabel: 'Favorite kinds',
    onChange: (id) => app.setQuery({ tab: id === 'songs' ? null : id }),
  });
  page.append(pageHead('Favorites', { subtitle: 'Only you see your favorites.' }), toolbar(tabs.el), body);
  let view = null;
  const offs = [];

  function build() {
    view?.destroy();
    tabs.setActive(tab);
    if (tab === 'songs') {
      const q = { favorite: 'true', sort: 'added', order: 'desc' };
      view = pagedTrackList({ label: 'Favorite songs', art: true, album: true }, {
        fetchPage: (cursor) => mapi.tracks({ ...q, cursor, limit: 200 }),
        more: (cursor) => trackFeed(q, cursor),
      });
      view.list.setEmpty(ui.emptyState({ icon: 'heart', title: 'No favorite songs yet', message: 'Tap the heart next to a song to keep it here.', compact: true }));
    } else if (tab === 'albums') {
      view = pagedGrid({
        fetchPage: (cursor) => mapi.albums({ favorite: 'true', sort: 'title', cursor, limit: 100 }),
        render: (a) => albumCard(a),
        empty: () => ui.emptyState({ icon: 'heart', title: 'No favorite albums yet', message: 'Use the heart on an album page.', compact: true }),
      });
    } else {
      view = pagedGrid({
        className: 'mu-grid-artists',
        fetchPage: (cursor) => mapi.artists({ favorite: 'true', role: 'all', sort: 'name', cursor, limit: 100 }),
        render: (a) => artistCard(a),
        empty: () => ui.emptyState({ icon: 'heart', title: 'No favorite artists yet', message: 'Use the heart on an artist page.', compact: true }),
      });
    }
    body.replaceChildren(view.el);
  }

  // A song unfavorited elsewhere leaves this list.
  offs.push(on('track-updated', (t) => {
    if (tab !== 'songs' || t.favorite || !view?.list) return;
    const items = view.list.items;
    if (items.some((x) => x.id === t.id)) view.list.setItems(items.filter((x) => x.id !== t.id));
  }));
  build();
  return {
    update(r) {
      const next = FAV_TABS.some((t) => t.id === r.query.tab) ? r.query.tab : 'songs';
      if (next !== tab) {
        tab = next;
        build();
      }
    },
    destroy() {
      offs.forEach((off) => off());
      view?.destroy();
    },
  };
}

/** /music/history */
export function mountHistory(host, app) {
  app.setTitle('History');
  const page = h('div', { class: 'mu-page' });
  host.append(page);
  let view = null;

  function build() {
    view?.destroy();
    view = pagedTrackList({
      label: 'Play history', art: true, album: true, selectable: true,
      getTrack: (e) => e.track, keyOf: (e) => e.id,
      extra: (e) => formatRelative(e.played_at),
    }, {
      fetchPage: (cursor) => mapi.history(cursor, undefined, 100),
    });
    view.list.setEmpty(ui.emptyState({ icon: 'history', title: 'Nothing played yet', message: 'Songs you listen to (at least half, or four minutes) show up here.', compact: true }));
    page.replaceChildren(
      pageHead('History', {
        subtitle: 'Your plays, newest first. They feed “Recently played” and “Most played”.',
        actions: [ui.button({ label: 'Clear history', icon: 'trash', variant: 'danger-ghost', onClick: async () => {
          const d = await dialogs();
          if (await d.clearHistoryDialog()) build();
        } })],
      }),
      view.el);
  }
  build();
  return { destroy: () => view?.destroy() };
}
