// Music app (/music/*): library (home, songs, albums, artists, genres), album/artist/genre and
// playlist pages, favorites, history, search and the Now playing view. Playback lives in
// player.js and keeps going in the shell's bottom dock when the user opens another app.
//   /music                     home              /music/songs | albums | artists | genres
//   /music/albums/<id>         album             /music/artists/<id>   artist
//   /music/genre?name=         genre             /music/playlists[/<id>]
//   /music/favorites?tab=      favorites         /music/history
//   /music/search?q=           search            /music/now-playing?tab=queue|lyrics

import { h } from '../../core/dom.js';
import { ensureStyle } from '../../core/style.js';
import { TABS, parseRoute, paths, hasScope } from './model.js';
import { configurePlayer, restorePlayer, getPlayer } from './player.js';
import { loadSpaces, dialogs, shuffleAll } from './widgets.js';
import { mountHome } from './views/home.js';
import { mountSongs } from './views/songs.js';
import { mountAlbums, mountArtists, mountGenres } from './views/browse.js';
import { mountAlbum, mountArtist, mountGenre } from './views/detail.js';
import { mountPlaylists, mountPlaylist } from './views/playlists.js';
import { mountFavorites, mountHistory } from './views/mine.js';
import { mountSearch } from './views/search.js';
import { mountNowPlaying } from './views/nowplaying.js';

const VIEWS = {
  home: mountHome, songs: mountSongs, albums: mountAlbums, artists: mountArtists, genres: mountGenres,
  album: mountAlbum, artist: mountArtist, genre: mountGenre, playlists: mountPlaylists, playlist: mountPlaylist,
  favorites: mountFavorites, history: mountHistory, search: mountSearch, nowplaying: mountNowPlaying,
};

export async function mount(container, ctx) {
  await ensureStyle('/assets/css/apps/music.css');
  configurePlayer(ctx);
  loadSpaces(true);
  const { ui } = ctx;

  const app = {
    ctx,
    get me() { return app.ctx.user?.id || ''; },
    get canWrite() { return hasScope(app.ctx.store?.get().scopes, 'music:write'); },
    navigate: (p, o) => app.ctx.router.navigate(p, o),
    setQuery: (q) => app.ctx.router.setQuery(q),
    setTitle: (t) => app.ctx.setTitle(t),
    setLayout: (full) => {
      root.dataset.layout = full ? 'full' : 'page';
      app.ctx.setLayout({ full });
    },
  };

  const nav = ui.tabs({ items: TABS.map((t) => ({ id: t.id, label: t.label, icon: t.icon, href: t.href })), ariaLabel: 'Music sections' });
  const bar = h('div', { class: 'mu-tabs-bar' }, h('div', { class: 'mu-tabs-inner' }, nav.el));
  const content = h('div', { class: 'mu-content' });
  const root = h('div', { class: 'mu-app tint-pink', dataset: { layout: 'page' } }, bar, content);
  container.append(root);

  /* ------------------------------------------------------------ routing */
  let view = null; // {key, ctl}

  function route(c) {
    app.ctx = c;
    const r = parseRoute(c.params?.rest || '');
    const info = { ...r, query: c.query || {} };
    const key = r.view === 'album' || r.view === 'artist' || r.view === 'playlist' ? `${r.view}:${r.id}` : r.view;
    nav.setActive(r.tab || null);
    bar.hidden = r.view === 'nowplaying';
    syncSearch(r.view === 'search' ? (c.query?.q || '') : null);
    if (view && view.key === key && view.ctl?.update) {
      view.ctl.update(info);
      return;
    }
    view?.ctl?.destroy?.();
    view = null;
    content.replaceChildren();
    app.setLayout(r.view === 'nowplaying');
    if (r.view === 'notfound') {
      app.setTitle('Music');
      content.append(h('div', { class: 'page' }, ui.emptyState({
        icon: 'music', title: 'Nothing here', message: 'That page of the Music app does not exist.',
        action: ui.button({ label: 'Back to Music', icon: 'arrow-left', href: '/music' }),
      })));
      return;
    }
    const host = h('div', { class: 'mu-view', dataset: { view: r.view } });
    content.append(host);
    try {
      view = { key, ctl: VIEWS[r.view](host, app, info) };
    } catch (err) {
      console.error(err);
      host.replaceChildren(h('div', { class: 'page' }, ui.errorState(err, () => c.router.reload())));
    }
  }

  /* ------------------------------------------------------------- search */
  let searchValue = null;
  function syncSearch(q) {
    // Keep the top-bar box in sync with the search page (and empty elsewhere). The URL holds the
    // trimmed query: compare trimmed, or a space being typed between two words would be undone.
    const want = q ?? '';
    if (searchValue !== null && searchValue.trim() === want.trim()) return;
    searchValue = want;
    ctx.setSearch({
      placeholder: 'Search music',
      value: want,
      onInput: (v) => goSearch(v, true),
      onSubmit: (v) => goSearch(v, false),
    });
  }
  function goSearch(v, typing) {
    const q = v.trim();
    const onSearch = location.pathname === '/music/search';
    searchValue = v;
    if (!q && !onSearch) return;
    app.navigate(paths.search(q), { replace: onSearch && typing });
  }

  /* ----------------------------------------------- commands & shortcuts */
  const player = () => getPlayer();
  ctx.commands.register(() => [
    { id: 'music:toggle', title: 'Music: Play or pause', icon: 'play', keywords: ['pause', 'resume'], run: () => player()?.toggle() },
    { id: 'music:next', title: 'Music: Next song', icon: 'next', keywords: ['skip'], run: () => player()?.next() },
    { id: 'music:prev', title: 'Music: Previous song', icon: 'prev', run: () => player()?.previous() },
    { id: 'music:shuffle-all', title: 'Music: Shuffle all songs', icon: 'shuffle', keywords: ['random', 'play'], run: () => shuffleAll() },
    { id: 'music:now', title: 'Music: Now playing', icon: 'music', keywords: ['queue', 'lyrics'], run: () => app.navigate(paths.nowPlaying()) },
    { id: 'music:new-playlist', title: 'Music: New playlist', icon: 'playlist-add', run: () => dialogs().then(async (d) => { const p = await d.playlistDialog(); if (p) app.navigate(paths.playlist(p.id)); }) },
    { id: 'music:new-smart', title: 'Music: New smart playlist', icon: 'sparkle', keywords: ['rules', 'auto'], run: () => dialogs().then(async (d) => { const p = await d.playlistDialog({ smart: true }); if (p) app.navigate(paths.playlist(p.id)); }) },
    { id: 'music:import', title: 'Music: Import M3U playlist', icon: 'upload', keywords: ['m3u', 'm3u8'], run: () => dialogs().then((d) => d.importDialog()) },
    { id: 'music:history', title: 'Music: Play history', icon: 'history', run: () => app.navigate(paths.history()) },
    ...(app.canWrite ? [{ id: 'music:rescan', title: 'Music: Rescan tags', icon: 'refresh', keywords: ['reread', 'scan', 'library'], run: () => dialogs().then((d) => d.rescanDialog()) }] : []),
  ]);

  const noOverlay = () => !document.documentElement.classList.contains('modal-open') && !document.documentElement.classList.contains('palette-open');
  // Space and the arrows also work buttons, sliders, tabs and lists: only when nothing like that has focus.
  const free = () => {
    const a = document.activeElement;
    return !(a instanceof Element) || !a.closest('button, a[href], input, select, textarea, [contenteditable], [role="slider"], [role="tab"], .mu-row, .menu');
  };
  const hasPlayer = () => !!player()?.current && noOverlay();
  const sc = (combo, fn, description, when = hasPlayer) => ctx.shortcuts.register(combo, (e) => fn(e), { description, when });
  sc('space', () => player().toggle(), 'Play or pause', () => hasPlayer() && free());
  sc('k', () => player().toggle(), 'Play or pause');
  sc('arrowleft', () => player().seekBy(-10), 'Back 10 seconds', () => hasPlayer() && free());
  sc('arrowright', () => player().seekBy(10), 'Forward 10 seconds', () => hasPlayer() && free());
  sc('shift+p', () => player().previous(), 'Previous song');
  sc('shift+n', () => player().next(), 'Next song');
  sc('-', () => player().setVolume((player().volume || 0) - 0.1), 'Volume down');
  sc('=', () => player().setVolume((player().volume || 0) + 0.1), 'Volume up');
  sc('plus', () => player().setVolume((player().volume || 0) + 0.1), '', hasPlayer);
  sc('m', () => player().toggleMute(), 'Mute or unmute');
  sc('s', () => player().toggleShuffle(), 'Shuffle on or off');
  sc('r', () => player().cycleRepeat(), 'Repeat: off, all, this song');
  sc('f', () => {
    const t = player()?.current?.track;
    if (t) import('./widgets.js').then((w) => w.setTrackFavorite(t, !t.favorite));
  }, 'Favorite the playing song');
  sc('q', () => app.navigate(paths.nowPlaying('queue')), 'Show the queue', noOverlay);
  sc('l', () => app.navigate(paths.nowPlaying('lyrics')), 'Show the lyrics', noOverlay);

  route(ctx);
  // Bring back the last queue (paused) or the cast session of an earlier visit.
  restorePlayer().catch((err) => console.warn('music: restore failed', err));

  return {
    update: (c) => route(c),
    unmount: () => {
      view?.ctl?.destroy?.();
      view = null;
    },
  };
}
