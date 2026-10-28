// Music home: library totals with "Shuffle all", recently played, recently added albums, most
// played songs, favorite albums and playlists (GET /music/home + /music/facets).

import { h } from '../../../core/dom.js';
import * as ui from '../../../core/ui.js';
import { formatNumber } from '../../../core/format.js';
import { on } from '../state.js';
import {
  mapi, albumCard, artistCard, playlistCard, artBox, section, shelf, createTrackList, playTracks, shuffleAll,
} from '../widgets.js';
import { paths, formatLongDuration, recentLink, trackTitle, trackArtist, errorText } from '../model.js';
import { loadError } from './common.js';

function recentCard(item) {
  switch (item.kind) {
    case 'album': return item.album ? albumCard(item.album) : null;
    case 'artist': return item.artist ? artistCard(item.artist) : null;
    case 'playlist': return item.playlist ? playlistCard(item.playlist) : null;
    case 'track': {
      const t = item.track;
      if (!t) return null;
      const href = recentLink(item);
      const card = h(href ? 'a' : 'div', { class: 'mu-card', href: href || undefined },
        h('span', { class: 'mu-card-art' }, artBox(t.art_url, 'md')),
        h('span', { class: 'mu-card-title truncate' }, trackTitle(t)),
        h('span', { class: 'mu-card-sub truncate' }, trackArtist(t)));
      card.addEventListener('dblclick', (e) => {
        e.preventDefault();
        playTracks([t]);
      });
      return card;
    }
    default: return null;
  }
}

export function mountHome(host, app) {
  app.setTitle('Music');
  const ctrl = new AbortController();
  const page = h('div', { class: 'mu-page mu-home' });
  host.append(page);
  const lists = [];
  let reloadTimer = 0;

  async function load() {
    page.replaceChildren(ui.loadingBlock('Loading your music…'));
    let home;
    let facets = null;
    try {
      [home, facets] = await Promise.all([mapi.home(ctrl.signal), mapi.facets(ctrl.signal).catch(() => null)]);
    } catch (err) {
      if (ctrl.signal.aborted) return;
      page.replaceChildren(loadError(err, load));
      return;
    }
    if (ctrl.signal.aborted) return;
    lists.splice(0).forEach((l) => l.destroy());
    const totals = facets?.totals || null;
    if (totals && totals.tracks === 0) {
      page.replaceChildren(ui.emptyState({
        icon: 'music', title: 'Your music library is empty',
        message: 'Audio files you keep in Files (in your home or a library you can read) show up here — MP3, FLAC, AAC/M4A, Ogg, Opus or WAV. Albums, artists and covers come from their tags.',
        action: h('div', { class: 'row wrap' },
          ui.button({ label: 'Open Files', icon: 'folder', variant: 'primary', href: '/files' }),
          ui.button({ label: 'Import a playlist', icon: 'upload', onClick: () => import('../dialogs.js').then((d) => d.importDialog()) })),
      }));
      return;
    }

    const stats = totals ? [
      `${formatNumber(totals.tracks)} songs`, `${formatNumber(totals.albums)} albums`, `${formatNumber(totals.artists)} artists`,
      totals.duration_seconds ? formatLongDuration(totals.duration_seconds) : '',
    ].filter(Boolean).join(' · ') : '';
    const head = h('header', { class: 'mu-home-head' },
      h('div', {}, h('h1', { class: 'mu-head-title' }, 'Listen now'), stats ? h('p', { class: 'mu-head-sub' }, stats) : null),
      h('div', { class: 'mu-head-actions' },
        ui.button({ label: 'Shuffle all', icon: 'shuffle', variant: 'primary', onClick: () => shuffleAll() }),
        ui.button({ label: 'Songs', icon: 'music', href: paths.songs() })));

    const parts = [head];
    const recent = (home.recently_played || []).map(recentCard).filter(Boolean);
    if (recent.length) parts.push(section('Recently played', shelf(recent), { href: paths.history() }));
    if (home.recently_added?.length) {
      parts.push(section('Recently added', shelf(home.recently_added.map((a) => albumCard(a))), { href: `${paths.albums()}?sort=added&order=desc` }));
    }
    if (home.most_played?.length) {
      const list = createTrackList({ label: 'Most played songs', art: true, album: true, selectable: false, extra: (t) => `${t.play_count} ${t.play_count === 1 ? 'play' : 'plays'}` });
      list.setItems(home.most_played.slice(0, 10));
      lists.push(list);
      parts.push(section('Most played', list.el, { href: `${paths.songs()}?sort=plays&order=desc` }));
    }
    if (home.favorite_albums?.length) {
      parts.push(section('Favorite albums', shelf(home.favorite_albums.map((a) => albumCard(a))), { href: paths.favorites('albums') }));
    }
    const pls = home.playlists || [];
    parts.push(section('Playlists', pls.length
      ? shelf(pls.map((p) => playlistCard(p, { me: app.me })))
      : h('div', { class: 'mu-inline-empty' }, h('span', { class: 'muted' }, 'No playlists yet.'),
        ui.button({ label: 'New playlist', icon: 'plus', size: 'sm', onClick: () => import('../dialogs.js').then(async (d) => { const p = await d.playlistDialog(); if (p) app.navigate(paths.playlist(p.id)); }) })),
    { href: paths.playlists() }));
    if (parts.length === 2 && !pls.length) {
      parts.push(h('p', { class: 'muted mu-home-hint' }, 'Play something and your recent music shows up here.'));
    }
    page.replaceChildren(...parts);
  }

  const offs = [on('playlists', () => {
    clearTimeout(reloadTimer);
    reloadTimer = setTimeout(() => { if (!ctrl.signal.aborted) load().catch((err) => ui.toast.error(errorText(err))); }, 400);
  })];
  load();

  return {
    destroy() {
      ctrl.abort();
      clearTimeout(reloadTimer);
      offs.forEach((off) => off());
      lists.forEach((l) => l.destroy());
    },
  };
}
