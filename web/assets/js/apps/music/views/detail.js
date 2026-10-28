// Album, artist and genre pages: header with art, play / shuffle / favorite and a menu, then the
// songs (album: by disc; artist: top songs, albums and appearances; genre: albums and songs).

import { h } from '../../../core/dom.js';
import * as ui from '../../../core/ui.js';
import { hueFor } from '../../../core/format.js';
import { on } from '../state.js';
import {
  mapi, createTrackList, playTracks, playNext, addToQueue, favoriteButton, albumCard, hero, heroArt, bigPlayButton,
  section, shelf, cardGrid, dialogs, canEdit, loadSpaces, trackFeed, playArtist, who,
} from '../widgets.js';
import {
  paths, albumTitle, songCount, formatLongDuration, totalDuration, groupByDisc, errorText, canEditAlbumArt,
} from '../model.js';
import { pagedTrackList, heroSkeleton, loadError } from './common.js';

/* ----------------------------------------------------------------- album */

export function mountAlbum(host, app, route) {
  const ctrl = new AbortController();
  const page = h('div', { class: 'mu-page mu-detail' }, heroSkeleton());
  host.append(page);
  let list = null;
  let album = null;
  const id = route.id;
  const ctx = { kind: 'album', id };

  async function load() {
    try {
      [album] = await Promise.all([mapi.album(id, ctrl.signal), loadSpaces()]);
    } catch (err) {
      if (ctrl.signal.aborted) return;
      page.replaceChildren(err?.status === 404
        ? h('div', { class: 'page' }, ui.emptyState({ icon: 'disc', title: 'Album not found', message: 'It may have been removed, or you cannot read its files.', action: ui.button({ label: 'All albums', icon: 'arrow-left', href: paths.albums() }) }))
        : loadError(err, load));
      return;
    }
    if (ctrl.signal.aborted) return;
    render();
  }

  function render() {
    app.setTitle(albumTitle(album));
    const tracks = album.tracks || [];
    const editArt = canEditAlbumArt(tracks, who());
    const groups = groupByDisc(tracks);
    const items = groups.length > 1 ? groups.flatMap((g) => [{ header: `Disc ${g.disc}` }, ...g.tracks]) : tracks;
    list?.destroy();
    list = createTrackList({ label: `Songs of ${albumTitle(album)}`, context: ctx, art: false, numbered: 'track', album: false, className: 'mu-album-list' });
    list.setItems(items);
    const genres = (album.genres || []).slice(0, 3);
    const menu = ui.menuButton({ icon: 'more', title: 'More album options', variant: 'ghost', items: () => [
      { label: 'Play next', icon: 'queue', onSelect: () => playNext(tracks, ctx) },
      { label: 'Add to queue', icon: 'list', onSelect: () => addToQueue(tracks, ctx) },
      { label: 'Add to playlist…', icon: 'playlist-add', onSelect: () => dialogs().then((d) => d.addToPlaylistDialog(tracks)) },
      ...(album.artist_id ? [{ label: 'Go to artist', icon: 'mic', onSelect: () => app.navigate(paths.artist(album.artist_id)) }] : []),
      ...(editArt ? [
        { separator: true },
        { label: 'Change cover…', icon: 'image', onSelect: () => changeCover() },
        ...(album.has_custom_art ? [{ label: 'Remove custom cover', icon: 'trash', onSelect: () => removeCover() }] : []),
      ] : []),
      ...(canEdit(tracks) ? [{ label: tracks.length === 1 ? 'Edit tags…' : `Edit tags of ${songCount(tracks.length)}…`, icon: 'edit', onSelect: () => dialogs().then((d) => (tracks.length === 1 ? d.editTagsDialog(tracks[0]) : d.bulkEditDialog(tracks))) }] : []),
    ] });
    page.replaceChildren(
      hero({
        art: heroArt(album.art_url, 'disc'),
        kicker: album.compilation ? 'Compilation' : 'Album',
        title: albumTitle(album),
        lines: [
          h('span', { class: 'mu-hero-artist' }, album.artist_id ? h('a', { href: paths.artist(album.artist_id) }, album.artist || 'Unknown artist') : (album.artist || 'Unknown artist')),
          [album.year ? String(album.year) : '', songCount(album.track_count || tracks.length), formatLongDuration(album.duration_seconds || totalDuration(tracks)),
            album.disc_count > 1 ? `${album.disc_count} discs` : ''].filter(Boolean).join(' · '),
          genres.length ? h('span', { class: 'mu-hero-genres' }, genres.map((g) => h('a', { class: 'chip', href: paths.genre(g) }, g))) : null,
        ],
        actions: [
          bigPlayButton(`Play ${albumTitle(album)}`, () => playTracks(tracks, { context: ctx })),
          ui.button({ label: 'Shuffle', icon: 'shuffle', onClick: () => playTracks(tracks, { index: null, shuffle: true, context: ctx }) }),
          favoriteButton('album', album),
          menu,
        ],
      }),
      list.el);
  }

  async function changeCover() {
    const d = await dialogs();
    const res = await d.uploadCover((file, o) => mapi.putAlbumArt(id, file, o));
    if (res) await reload();
  }

  async function removeCover() {
    const ok = await ui.confirm({ title: 'Remove the custom cover?', message: 'The album goes back to the cover in its files (or the folder image).', confirmLabel: 'Remove', danger: true });
    if (!ok) return;
    try {
      await mapi.deleteAlbumArt(id);
      await reload();
    } catch (err) {
      ui.toast.error(errorText(err));
    }
  }

  async function reload() {
    try {
      album = await mapi.album(id, ctrl.signal);
      render();
    } catch (err) {
      if (!ctrl.signal.aborted) ui.toast.error(errorText(err));
    }
  }

  load();
  const offs = [on('track-updated', (t) => {
    if (album && (album.tracks || []).some((x) => x.id === t.id) && t.album_id !== id) reload();
  })];
  return {
    destroy() {
      ctrl.abort();
      offs.forEach((off) => off());
      list?.destroy();
    },
  };
}

/* ---------------------------------------------------------------- artist */

export function mountArtist(host, app, route) {
  const ctrl = new AbortController();
  const page = h('div', { class: 'mu-page mu-detail' }, heroSkeleton());
  host.append(page);
  const id = route.id;
  const ctx = { kind: 'artist', id };
  let topList = null;
  let allSongs = null;

  async function load() {
    let artist;
    try {
      artist = await mapi.artist(id, ctrl.signal);
    } catch (err) {
      if (ctrl.signal.aborted) return;
      page.replaceChildren(err?.status === 404
        ? h('div', { class: 'page' }, ui.emptyState({ icon: 'mic', title: 'Artist not found', message: 'There is no music by this artist that you can read.', action: ui.button({ label: 'All artists', icon: 'arrow-left', href: paths.artists() }) }))
        : loadError(err, load));
      return;
    }
    if (ctrl.signal.aborted) return;
    app.setTitle(artist.name || 'Artist');
    const top = artist.top_tracks || [];
    topList?.destroy();
    topList = createTrackList({ label: `Top songs of ${artist.name}`, context: ctx, art: true, album: true });
    topList.setItems(top.slice(0, 10));
    const songsHost = h('div');
    const showAll = ui.button({ label: 'Show all songs', icon: 'list', size: 'sm', variant: 'ghost', onClick: () => {
      showAll.hidden = true;
      allSongs?.destroy();
      const q = { artist_id: id, sort: 'album' };
      allSongs = pagedTrackList({ label: `All songs of ${artist.name}`, context: ctx, art: true, album: true }, {
        fetchPage: (cursor) => mapi.tracks({ ...q, cursor, limit: 200 }),
        more: (cursor) => trackFeed(q, cursor),
      });
      songsHost.replaceChildren(section('All songs', allSongs.el));
    } });
    const albums = artist.albums || [];
    const appears = artist.appears_on || [];
    page.replaceChildren(
      hero({
        art: heroArt(artist.art_url, 'mic'), round: true, kicker: 'Artist', title: artist.name || 'Unknown artist',
        lines: [[artist.album_count ? `${artist.album_count} ${artist.album_count === 1 ? 'album' : 'albums'}` : '', songCount(artist.track_count)].filter(Boolean).join(' · ')],
        actions: [
          bigPlayButton(`Play ${artist.name}`, () => playArtist(id)),
          ui.button({ label: 'Shuffle', icon: 'shuffle', onClick: () => playArtist(id, { shuffle: true }) }),
          favoriteButton('artist', artist),
        ],
      }),
      top.length ? section('Top songs', topList.el, { actions: artist.track_count > top.length || artist.track_count > 10 ? showAll : null }) : null,
      songsHost,
      albums.length ? section('Albums', (() => { const g = cardGrid(); g.append(...albums.map((a) => albumCard(a))); return g; })()) : null,
      appears.length ? section('Appears on', shelf(appears.map((a) => albumCard(a)))) : null,
      !top.length && !albums.length ? ui.emptyState({ icon: 'music', title: 'No songs', compact: true }) : null);
  }
  load();
  return {
    destroy() {
      ctrl.abort();
      topList?.destroy();
      allSongs?.destroy();
    },
  };
}

/* ----------------------------------------------------------------- genre */

export function mountGenre(host, app, route) {
  const ctrl = new AbortController();
  const page = h('div', { class: 'mu-page mu-detail' });
  host.append(page);
  let songs = null;
  let name = '';

  function build(genre) {
    name = genre;
    songs?.destroy();
    songs = null;
    if (!name) {
      page.replaceChildren(ui.emptyState({ icon: 'tag', title: 'No genre chosen', action: ui.button({ label: 'All genres', icon: 'arrow-left', href: paths.genres() }) }));
      return;
    }
    app.setTitle(name);
    const ctx = { kind: 'genre', id: name };
    const q = { genre: name, sort: 'artist' };
    songs = pagedTrackList({ label: `${name} songs`, context: ctx, art: true, album: true }, {
      fetchPage: (cursor) => mapi.tracks({ ...q, cursor, limit: 200 }),
      more: (cursor) => trackFeed(q, cursor),
    });
    songs.list.setEmpty(ui.emptyState({ icon: 'music', title: 'No songs in this genre', compact: true }));
    const albumsBox = h('div');
    const tile = h('div', { class: 'mu-genre-hero', style: { '--hue': String(hueFor(name)) } }, h('span', {}, name));
    const playAll = async (shuffle) => {
      const qq = shuffle ? { genre: name, sort: 'random', seed: Math.random().toString(36).slice(2, 10) } : q;
      try {
        const first = await mapi.tracks({ ...qq, limit: 200 });
        playTracks(first.items || [], { index: 0, shuffle, context: ctx, more: first.next_cursor ? trackFeed(qq, first.next_cursor) : null });
      } catch (err) {
        ui.toast.error(errorText(err));
      }
    };
    page.replaceChildren(
      hero({
        art: tile, kicker: 'Genre', title: name,
        actions: [
          bigPlayButton(`Play ${name}`, () => playAll(false)),
          ui.button({ label: 'Shuffle', icon: 'shuffle', onClick: () => playAll(true) }),
        ],
      }),
      albumsBox,
      section('Songs', songs.el));
    mapi.albums({ genre: name, sort: 'artist', limit: 50 }, ctrl.signal).then((res) => {
      if (ctrl.signal.aborted || name !== genre) return;
      const albums = res?.items || [];
      if (albums.length) albumsBox.replaceChildren(section('Albums', shelf(albums.map((a) => albumCard(a)))));
    }).catch(() => {});
  }
  build((route.query.name || '').trim());
  return {
    update(r) {
      const next = (r.query.name || '').trim();
      if (next !== name) build(next);
    },
    destroy() {
      ctrl.abort();
      songs?.destroy();
    },
  };
}
