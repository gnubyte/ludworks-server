// Shared Music widgets: cards (album, artist, playlist), the track list (virtualized rows with
// the playing indicator, multi-select, keyboard navigation, context menus and optional drag
// reorder), track menus, favorite buttons and the rating control. The dialogs live in
// dialogs.js. Everything here also works outside the Music app (the dock opens menus).

import { h, cls } from '../../core/dom.js';
import { icon } from '../../core/icons.js';
import * as ui from '../../core/ui.js';
import { formatDuration, hueFor } from '../../core/format.js';
import { store } from '../../core/store.js';
import { api as appApi } from '../../core/api.js';
import { musicApi } from './api.js';
import { emit, on as onBus } from './state.js';
import { getPlayer, ensurePlayer, navigate } from './player.js';
import { createVirtualList, attachReorder } from './vlist.js';
import { insertionToPosition } from './queue.js';
import { Selection } from './selection.js';
import { artBox } from './dock.js';
import {
  artUrl, paths, trackTitle, trackArtist, albumTitle, albumSubtitle, songCount, canEditTracks, errorText,
} from './model.js';

export { artBox };

export const mapi = musicApi(appApi);

/* ------------------------------------------------------------- who am I */

let spacesCache = null;
let spacesPromise = null;

/** Load (and cache) GET /spaces — write access decides which edit controls show. */
export function loadSpaces(force = false) {
  if (!spacesPromise || force) {
    spacesPromise = mapi.spaces().then((r) => {
      spacesCache = Array.isArray(r) ? r : r?.items || [];
      return spacesCache;
    }).catch(() => {
      spacesCache = spacesCache || [];
      return spacesCache;
    });
  }
  return spacesPromise;
}

/** The caller's scopes, role and spaces (spaces may still be loading: then no edit rights). */
export function who() {
  const st = store.get();
  return { scopes: st.scopes || [], isAdmin: st.user?.role === 'admin', spaces: spacesCache, userId: st.user?.id || '' };
}

/** May the caller edit the tags of these tracks? */
export function canEdit(tracks) {
  const w = who();
  if (!w.spaces && !w.isAdmin) return false;
  return canEditTracks(tracks, w);
}

/* --------------------------------------------------------------- playback */

/** Replace the queue with tracks and play (index null + shuffle = random start). */
export function playTracks(tracks, opts = {}) {
  const list = (tracks || []).filter(Boolean);
  if (!list.length) {
    ui.toast.info('Nothing to play here.');
    return;
  }
  ensurePlayer().playTracks(list, opts);
}

export function playNext(tracks, context = null) {
  if (tracks?.length) ensurePlayer().playNext(tracks, context);
}

export function addToQueue(tracks, context = null) {
  if (tracks?.length) ensurePlayer().addToQueue(tracks, context);
}

/** Id of the track playing now (or paused in the player), or null. */
export function currentTrackId() {
  return getPlayer()?.current?.id || null;
}

/* -------------------------------------------------------------- favorites */

/** Favorite / unfavorite a track (optimistic, everyone showing it updates). */
export async function setTrackFavorite(track, want) {
  emit('track-updated', { ...track, favorite: want });
  try {
    await mapi.favorite('track', track.id, want);
  } catch (err) {
    emit('track-updated', { ...track, favorite: !want });
    ui.toast.error(errorText(err));
  }
}

/**
 * A heart toggle for a track, album or artist.
 * @param {'track'|'album'|'artist'} kind
 * @param {object} obj the Track/Album/Artist (its .favorite is updated)
 */
export function favoriteButton(kind, obj, { size = 'md', className = '', variant = 'ghost', listen = true } = {}) {
  const b = h('button', { type: 'button', class: cls('btn', `btn-${variant}`, 'btn-icon', size !== 'md' && `btn-${size}`, 'mu-fav', className) });
  const render = () => {
    const on = !!obj.favorite;
    b.replaceChildren(icon(on ? 'heart-filled' : 'heart', size === 'sm' ? 15 : 18));
    b.classList.toggle('is-on', on);
    b.setAttribute('aria-pressed', String(on));
    const label = on ? 'Remove from favorites' : 'Add to favorites';
    b.setAttribute('aria-label', label);
    b.dataset.tip = label;
  };
  b.addEventListener('click', async (e) => {
    e.stopPropagation();
    const want = !obj.favorite;
    if (kind === 'track') {
      await setTrackFavorite(obj, want);
      return;
    }
    obj.favorite = want;
    render();
    emit('favorite', { kind, id: obj.id, favorite: want });
    try {
      await mapi.favorite(kind, obj.id, want);
    } catch (err) {
      obj.favorite = !want;
      render();
      emit('favorite', { kind, id: obj.id, favorite: !want });
      ui.toast.error(errorText(err));
    }
  });
  // Stand-alone buttons follow changes made elsewhere (track lists re-render their rows instead).
  if (kind === 'track' && listen) {
    const off = onBus('track-updated', (t) => {
      if (!b.isConnected) {
        off();
        return;
      }
      if (t.id === obj.id) {
        obj.favorite = t.favorite;
        render();
      }
    });
  }
  render();
  return b;
}

/**
 * Five-star rating (radiogroup). Choosing the current rating clears it.
 * @param {object} track
 */
export function ratingControl(track, { compact = false } = {}) {
  const el = h('div', { class: cls('mu-rating', { 'is-compact': compact }), role: 'radiogroup', aria: { label: 'Your rating' } });
  const stars = [1, 2, 3, 4, 5].map((n) => {
    const b = h('button', { type: 'button', class: 'mu-star', role: 'radio', aria: { label: `${n} ${n === 1 ? 'star' : 'stars'}` } });
    b.addEventListener('click', () => set(track.rating === n ? null : n));
    el.append(b);
    return b;
  });
  function render() {
    const r = Number(track.rating) || 0;
    stars.forEach((b, i) => {
      const on = i < r;
      b.replaceChildren(icon(on ? 'star-filled' : 'star', compact ? 14 : 16));
      b.classList.toggle('is-on', on);
      b.setAttribute('aria-checked', String(i + 1 === r));
      b.tabIndex = (r ? i + 1 === r : i === 0) ? 0 : -1;
    });
  }
  async function set(value) {
    const prev = track.rating ?? null;
    track.rating = value;
    render();
    emit('track-updated', { ...track, rating: value });
    try {
      await mapi.rate(track.id, value);
    } catch (err) {
      track.rating = prev;
      render();
      emit('track-updated', { ...track, rating: prev });
      ui.toast.error(errorText(err));
    }
  }
  el.addEventListener('keydown', (e) => {
    const i = stars.indexOf(document.activeElement);
    if (i < 0) return;
    const d = { ArrowRight: 1, ArrowUp: 1, ArrowLeft: -1, ArrowDown: -1 }[e.key];
    if (!d) return;
    e.preventDefault();
    const j = Math.max(0, Math.min(4, i + d));
    stars[j].focus();
    set(j + 1);
  });
  render();
  return el;
}

/* ------------------------------------------------------------------ cards */

/** Grid of cards that fills the width. */
export function cardGrid(className = '') {
  return h('div', { class: cls('mu-grid', className) });
}

function cardPlayButton(label, onPlay) {
  const b = h('button', { type: 'button', class: 'mu-card-play', aria: { label }, dataset: { tip: label } }, icon('play', 18));
  b.addEventListener('click', (e) => {
    e.preventDefault();
    e.stopPropagation();
    onPlay();
  });
  return b;
}

/** Album card: art, title, artist · year; the play button plays the album. */
export function albumCard(album, { onMenu = null } = {}) {
  const a = h('a', { class: 'mu-card', href: paths.album(album.id) },
    h('span', { class: 'mu-card-art' }, artBox(album.art_url, 'md', { iconName: 'disc' }),
      cardPlayButton(`Play ${albumTitle(album)}`, () => playAlbum(album.id))),
    h('span', { class: 'mu-card-title truncate', title: albumTitle(album) }, albumTitle(album)),
    h('span', { class: 'mu-card-sub truncate' }, albumSubtitle(album)));
  a.addEventListener('contextmenu', (e) => {
    e.preventDefault();
    ui.openMenu({ items: albumMenuItems(album, onMenu), x: e.clientX, y: e.clientY });
  });
  return a;
}

/** Artist card: round art, name, album count. */
export function artistCard(artist) {
  const a = h('a', { class: 'mu-card mu-card-artist', href: paths.artist(artist.id) },
    h('span', { class: 'mu-card-art is-round' }, artBox(artist.art_url, 'md', { iconName: 'mic' }),
      cardPlayButton(`Play ${artist.name}`, () => playArtist(artist.id))),
    h('span', { class: 'mu-card-title truncate', title: artist.name }, artist.name || 'Unknown artist'),
    h('span', { class: 'mu-card-sub truncate' }, artist.album_count ? `${artist.album_count} ${artist.album_count === 1 ? 'album' : 'albums'}` : songCount(artist.track_count)));
  return a;
}

/** Playlist card: mosaic/custom art, name, owner and count. */
export function playlistCard(pl, { me = '' } = {}) {
  const sub = [pl.owner_id !== me ? `By ${pl.owner_name || 'someone'}` : '', pl.kind === 'smart' ? 'Smart' : '', songCount(pl.track_count)].filter(Boolean).join(' · ');
  const a = h('a', { class: 'mu-card', href: paths.playlist(pl.id) },
    h('span', { class: 'mu-card-art' }, artBox(pl.art_url, 'md', { iconName: pl.kind === 'smart' ? 'sparkle' : 'queue' }),
      pl.track_count ? cardPlayButton(`Play ${pl.name}`, () => playPlaylist(pl.id)) : null),
    h('span', { class: 'mu-card-title truncate', title: pl.name }, pl.name),
    h('span', { class: 'mu-card-sub truncate' }, sub));
  return a;
}

/** Colourful genre tile. */
export function genreTile(g) {
  const a = h('a', { class: 'mu-genre', href: paths.genre(g.name), style: { '--hue': String(hueFor(g.name)) } },
    h('span', { class: 'mu-genre-name' }, g.name),
    h('span', { class: 'mu-genre-count' }, `${songCount(g.track_count)}${g.album_count ? ` · ${g.album_count} ${g.album_count === 1 ? 'album' : 'albums'}` : ''}`));
  return a;
}

/* ------------------------------------------------------- whole collections */

/** Play an album (optionally shuffled). */
export async function playAlbum(id, { shuffle = false } = {}) {
  try {
    const a = await mapi.album(id);
    playTracks(a.tracks || [], { index: shuffle ? null : 0, shuffle, context: { kind: 'album', id } });
  } catch (err) {
    ui.toast.error(errorText(err));
  }
}

/** A queue feed over GET /music/tracks pages (long lists keep loading as they play). */
export function trackFeed(query, cursor) {
  let next = cursor;
  return async () => {
    if (!next) return { tracks: [], done: true };
    const page = await mapi.tracks({ ...query, cursor: next, limit: 200 });
    next = page?.next_cursor || null;
    return { tracks: page?.items || [], done: !next };
  };
}

/** Play every track of an artist (by album), optionally shuffled (server random order). */
export async function playArtist(id, { shuffle = false } = {}) {
  const query = shuffle ? { artist_id: id, sort: 'random', seed: Math.random().toString(36).slice(2, 10) } : { artist_id: id, sort: 'album' };
  try {
    const page = await mapi.tracks({ ...query, limit: 200 });
    playTracks(page.items || [], { index: 0, shuffle, context: { kind: 'artist', id }, more: page.next_cursor ? trackFeed(query, page.next_cursor) : null });
  } catch (err) {
    ui.toast.error(errorText(err));
  }
}

/** Shuffle the whole library (server-side random order, loaded page by page as it plays). */
export async function shuffleAll() {
  const query = { sort: 'random', seed: Math.random().toString(36).slice(2, 10) };
  try {
    const page = await mapi.tracks({ ...query, limit: 200 });
    playTracks(page.items || [], { index: 0, shuffle: true, more: page.next_cursor ? trackFeed(query, page.next_cursor) : null });
  } catch (err) {
    ui.toast.error(errorText(err));
  }
}

/** Play a playlist (optionally shuffled). */
export async function playPlaylist(id, { shuffle = false } = {}) {
  try {
    const p = await mapi.playlist(id);
    playTracks((p.items || []).map((it) => it.track), { index: shuffle ? null : 0, shuffle, context: { kind: 'playlist', id } });
  } catch (err) {
    ui.toast.error(errorText(err));
  }
}

function albumMenuItems(album, extra) {
  return [
    { label: 'Play', icon: 'play', onSelect: () => playAlbum(album.id) },
    { label: 'Shuffle', icon: 'shuffle', onSelect: () => playAlbum(album.id, { shuffle: true }) },
    { label: 'Play next', icon: 'queue', onSelect: () => withAlbumTracks(album.id, (t) => playNext(t, { kind: 'album', id: album.id })) },
    { label: 'Add to queue', icon: 'list', onSelect: () => withAlbumTracks(album.id, (t) => addToQueue(t, { kind: 'album', id: album.id })) },
    { label: 'Add to playlist…', icon: 'playlist-add', onSelect: () => withAlbumTracks(album.id, (t) => dialogs().then((d) => d.addToPlaylistDialog(t))) },
    ...(album.artist_id ? [{ label: 'Go to artist', icon: 'mic', onSelect: () => navigate(paths.artist(album.artist_id)) }] : []),
    ...(extra ? extra(album) : []),
  ];
}

async function withAlbumTracks(id, fn) {
  try {
    const a = await mapi.album(id);
    fn(a.tracks || []);
  } catch (err) {
    ui.toast.error(errorText(err));
  }
}

/** Lazily loaded dialogs module. */
export function dialogs() {
  return import('./dialogs.js');
}

/* ------------------------------------------------------------- track menu */

/**
 * Menu items for one or more tracks.
 * @param {object[]} tracks
 * @param {{context?: object|null, extra?: object[], onPlay?: () => void}} [o]
 */
export function trackMenuItems(tracks, { context = null, extra = [], onPlay = null } = {}) {
  const list = tracks.filter(Boolean);
  if (!list.length) return [];
  const one = list.length === 1 ? list[0] : null;
  const editable = canEdit(list);
  const items = [
    { label: one ? 'Play' : `Play ${list.length} songs`, icon: 'play', onSelect: () => (onPlay ? onPlay() : playTracks(list, { context })) },
    { label: 'Play next', icon: 'queue', onSelect: () => playNext(list, context) },
    { label: 'Add to queue', icon: 'list', onSelect: () => addToQueue(list, context) },
    { label: 'Add to playlist…', icon: 'playlist-add', onSelect: () => dialogs().then((d) => d.addToPlaylistDialog(list)) },
    { separator: true },
  ];
  if (one) {
    if (one.album_id) items.push({ label: 'Go to album', icon: 'disc', onSelect: () => navigate(paths.album(one.album_id)) });
    if (one.artist_id) items.push({ label: 'Go to artist', icon: 'mic', onSelect: () => navigate(paths.artist(one.artist_id)) });
    items.push(
      { label: one.favorite ? 'Remove from favorites' : 'Add to favorites', icon: one.favorite ? 'heart-filled' : 'heart', onSelect: () => setTrackFavorite(one, !one.favorite) },
      { label: 'Song details', icon: 'info', onSelect: () => dialogs().then((d) => d.openTrackDetails(one)) },
      { label: 'Download', icon: 'download', onSelect: () => mapi.download(one.id) },
    );
  } else {
    items.push({ label: 'Add to favorites', icon: 'heart', onSelect: () => list.forEach((t) => { if (!t.favorite) setTrackFavorite(t, true); }) });
  }
  if (editable) {
    items.push({ separator: true }, { label: one ? 'Edit tags…' : `Edit tags of ${list.length} songs…`, icon: 'edit', onSelect: () => dialogs().then((d) => (one ? d.editTagsDialog(one) : d.bulkEditDialog(list))) });
  }
  if (extra.length) items.push({ separator: true }, ...extra);
  return items;
}

/* ------------------------------------------------------------- track list */

const DEFAULT_KEY = (it) => it?.id;

/**
 * A virtualized track list. Items are Tracks (or wrappers with getTrack), and may include
 * {header: 'Disc 2'} rows. Clicking a row plays the list from there (or toggles it while rows are
 * selected); keyboard: ↑/↓ Home/End PageUp/PageDown move, Enter plays, Space selects, Shift+arrows
 * extend, Ctrl/⌘+A selects all, Esc clears, the menu key or Shift+F10 opens the row menu, Delete
 * calls onDelete and Alt+↑/↓/Home/End reorder (with `reorder`).
 * @param {{label: string, getTrack?: (item: any) => object, keyOf?: (item: any, i: number) => string,
 *          context?: object|null, numbered?: 'index'|'track'|false, art?: boolean, album?: boolean,
 *          extra?: (item: any) => string, selectable?: boolean, more?: Function|null,
 *          onPlay?: (trackIndex: number, tracks: object[]) => void,
 *          reorder?: {onMove: (keys: string[], position: number, meta?: {focusKey?: string, keyboard?: boolean}) => void}|null,
 *          menuExtra?: (items: any[]) => object[], selectionExtra?: (items: any[]) => Node[],
 *          onDelete?: (items: any[]) => void, onRange?: Function, onSelectionChange?: (items: any[]) => void,
 *          scroller?: HTMLElement|null, isCurrent?: (item: any, i: number) => boolean,
 *          rowClass?: (item: any, i: number) => string, empty?: () => Node, className?: string}} o
 *   reorder.onMove gets item keys and the position in the list without them (playlist items/move rule);
 *   scroller: a scrolling element instead of the page; isCurrent: which row is "now playing" (default:
 *   same track id as the player's); rowClass: extra classes per row (e.g. 'is-past').
 */
export function createTrackList(o) {
  const getTrack = o.getTrack || ((it) => it);
  const keyOf = o.keyOf || DEFAULT_KEY;
  const sel = new Selection();
  let items = [];
  let tracks = [];
  let trackIndexOf = []; // item index → track index (-1 for headers)
  let activeIndex = 0;
  let loading = false;
  const isHeader = (it) => !!it && typeof it === 'object' && 'header' in it && !('id' in it);

  const live = h('div', { class: 'sr-only', aria: { live: 'polite', atomic: 'true' } });
  const emptyBox = h('div', { class: 'mu-list-empty', hidden: true });
  const skeleton = h('div', { class: 'mu-list-skeleton', hidden: true, aria: { hidden: 'true' } },
    Array.from({ length: 8 }, () => h('div', { class: 'mu-row is-skeleton' }, h('span', { class: 'skeleton mu-sk-art' }), h('span', { class: 'skeleton mu-sk-line' }))));

  // Is this item the one playing? (the queue compares entries; other lists compare track ids)
  const isCurrent = o.isCurrent || ((item) => {
    const t = getTrack(item);
    return !!t && t.id === currentTrackId();
  });

  const list = createVirtualList({
    label: o.label,
    scroller: o.scroller || null,
    className: cls('mu-tracklist', { 'has-art': o.art !== false, 'has-album': o.album !== false, 'has-extra': !!o.extra, 'is-reorderable': !!o.reorder, 'is-selectable': o.selectable !== false }, o.className),
    render: (item, i) => (isHeader(item) ? headerRow(item, i) : trackRow(item, i)),
    onRange: (r) => o.onRange?.(r),
  });

  const selBar = h('div', { class: 'mu-selbar', hidden: true, role: 'toolbar', aria: { label: 'Selected songs' } });
  const el = h('div', { class: 'mu-tracklist-wrap' }, skeleton, list.el, emptyBox, selBar, live);

  function headerRow(item, i) {
    return h('div', { class: 'mu-row mu-row-header', role: 'presentation', dataset: { index: String(i) } },
      icon('disc', 14), h('span', {}, item.header));
  }

  function selectedItems() {
    return items.filter((it, i) => !isHeader(it) && sel.has(keyOf(it, i)));
  }

  function trackRow(item, i) {
    const t = getTrack(item);
    const key = keyOf(item, i);
    const selected = sel.has(key);
    const isCur = isCurrent(item, i);
    const row = h('div', {
      class: cls('mu-row', { 'is-selected': selected, 'is-current': isCur, 'is-playing': isCur && !!getPlayer()?.playing, 'is-unavailable': !t }, o.rowClass ? o.rowClass(item, i) : null),
      role: 'listitem', tabindex: i === activeIndex ? '0' : '-1',
      aria: { label: t ? `${trackTitle(t)}, ${trackArtist(t)}${isCur ? ', now playing' : ''}` : 'Unavailable song', selected: o.selectable !== false ? String(selected) : undefined },
    });
    const inner = i === activeIndex ? '0' : '-1';
    if (o.reorder) {
      row.append(h('button', { type: 'button', class: 'mu-grip', tabindex: '-1', aria: { label: `Reorder ${t ? trackTitle(t) : ''} (drag, or Alt+↑/↓)` }, dataset: { tip: 'Drag to reorder' } }, icon('grip', 16)));
    }
    if (o.selectable !== false) {
      const cb = h('input', { type: 'checkbox', class: 'checkbox mu-row-check', checked: selected, tabindex: inner, aria: { label: `Select ${t ? trackTitle(t) : 'song'}` } });
      cb.addEventListener('click', (e) => {
        e.stopPropagation();
        if (e.shiftKey) sel.selectRange(orderedKeys(), key);
        else sel.toggle(key);
        syncSelection();
      });
      row.append(h('span', { class: 'mu-row-checkcell' }, cb));
    }
    const lead = h('span', { class: 'mu-row-lead' });
    if (o.art !== false) lead.append(artBox(t?.art_url || null, 'sm'));
    else lead.append(h('span', { class: 'mu-row-num nums' }, o.numbered === 'track' ? (t?.track_number ? String(t.track_number) : '') : String(trackIndexOf[i] + 1)));
    lead.append(h('span', { class: 'mu-eq', aria: { hidden: 'true' } }, h('i'), h('i'), h('i')), h('span', { class: 'mu-row-playicon', aria: { hidden: 'true' } }, icon('play', 14)));
    row.append(lead);
    // The album shows in its own column on wide screens and after the artist on narrow ones.
    const main = h('span', { class: 'mu-row-main' },
      h('span', { class: 'mu-row-title truncate' }, t ? trackTitle(t) : 'Unavailable'),
      h('span', { class: 'mu-row-sub truncate' }, t ? trackArtist(t) : '',
        t && o.album !== false && t.album ? h('span', { class: 'mu-row-subalbum' }, ` · ${t.album}`) : null));
    row.append(main);
    if (o.album !== false) {
      row.append(h('span', { class: 'mu-row-album truncate' }, t?.album_id
        ? h('a', { href: paths.album(t.album_id), tabindex: '-1', title: t.album }, t.album)
        : (t?.album || '')));
    }
    if (o.extra) row.append(h('span', { class: 'mu-row-extra truncate' }, o.extra(item) || ''));
    if (t) {
      const fav = favoriteButton('track', t, { size: 'sm', className: 'mu-row-fav', listen: false });
      fav.tabIndex = Number(inner);
      row.append(fav);
    } else {
      row.append(h('span'));
    }
    row.append(h('span', { class: 'mu-row-dur nums' }, t?.duration_seconds ? formatDuration(t.duration_seconds) : ''));
    const more = h('button', { type: 'button', class: 'btn btn-ghost btn-icon btn-sm mu-row-more', tabindex: inner, aria: { label: `More options for ${t ? trackTitle(t) : 'this song'}`, haspopup: 'menu' } }, icon('more', 16));
    more.addEventListener('click', (e) => {
      e.stopPropagation();
      openRowMenu(i, { anchor: more });
    });
    row.append(more);
    return row;
  }

  function orderedKeys() {
    const out = [];
    items.forEach((it, i) => { if (!isHeader(it)) out.push(keyOf(it, i)); });
    return out;
  }

  function play(i) {
    const ti = trackIndexOf[i];
    if (ti === undefined || ti < 0 || !tracks[ti]) return;
    if (o.onPlay) o.onPlay(ti, tracks);
    else playTracks(tracks, { index: ti, context: o.context || null, more: o.more || null });
  }

  function menuTargets(i) {
    const item = items[i];
    if (sel.size > 1 && sel.has(keyOf(item, i))) return selectedItems();
    return [item];
  }

  function openRowMenu(i, { anchor = null, x = 0, y = 0 } = {}) {
    const targets = menuTargets(i).filter((it) => !isHeader(it));
    const ts = targets.map(getTrack).filter(Boolean);
    const extra = o.menuExtra ? o.menuExtra(targets) : [];
    if (!ts.length && !extra.length) return;
    const menu = ts.length ? trackMenuItems(ts, { context: o.context || null, extra, onPlay: targets.length === 1 ? () => play(i) : null }) : extra;
    ui.openMenu({ items: menu, anchor, x, y, align: 'end' });
  }

  function setActive(i, focus = false) {
    if (i < 0 || i >= items.length) return;
    const prev = list.rowAt(activeIndex);
    activeIndex = i;
    if (prev) applyTabbable(prev, false);
    const row = focus ? list.focusIndex(i) : list.rowAt(i);
    if (row) applyTabbable(row, true);
  }

  function applyTabbable(row, on) {
    row.tabIndex = on ? 0 : -1;
    for (const c of row.querySelectorAll('input,.mu-row-fav,.mu-row-more')) c.tabIndex = on ? 0 : -1;
  }

  function nextTrackIndex(i, d) {
    let j = i + d;
    while (j >= 0 && j < items.length && isHeader(items[j])) j += d;
    return j >= 0 && j < items.length ? j : i;
  }

  function rowIndexFromEvent(e) {
    const row = e.target instanceof Element ? e.target.closest('.mu-row[data-index]') : null;
    if (!row || !list.el.contains(row) || row.classList.contains('mu-row-header')) return -1;
    return Number(row.dataset.index);
  }

  list.el.addEventListener('click', (e) => {
    const i = rowIndexFromEvent(e);
    if (i < 0) return;
    if (e.target instanceof Element && e.target.closest('button,a,input,label')) return;
    const key = keyOf(items[i], i);
    setActive(i);
    if (o.selectable !== false && (e.shiftKey || e.ctrlKey || e.metaKey || sel.size > 0)) {
      if (e.shiftKey) sel.selectRange(orderedKeys(), key);
      else sel.toggle(key);
      syncSelection();
      return;
    }
    play(i);
  });

  list.el.addEventListener('dblclick', (e) => {
    if (sel.size === 0) return;
    const i = rowIndexFromEvent(e);
    if (i >= 0 && !(e.target instanceof Element && e.target.closest('button,a,input'))) play(i);
  });

  list.el.addEventListener('contextmenu', (e) => {
    const i = rowIndexFromEvent(e);
    if (i < 0) return;
    e.preventDefault();
    setActive(i);
    openRowMenu(i, { x: e.clientX, y: e.clientY });
  });

  list.el.addEventListener('focusin', (e) => {
    const i = rowIndexFromEvent(e);
    if (i >= 0 && i !== activeIndex) setActive(i);
  });

  list.el.addEventListener('keydown', (e) => {
    const i = rowIndexFromEvent(e);
    if (i < 0) return;
    const onRow = e.target instanceof Element && e.target.classList.contains('mu-row');
    const key = keyOf(items[i], i);
    if (e.altKey && o.reorder && (e.key === 'ArrowUp' || e.key === 'ArrowDown' || e.key === 'Home' || e.key === 'End')) {
      e.preventDefault();
      keyMove(i, e.key);
      return;
    }
    switch (e.key) {
      case 'ArrowDown': case 'ArrowUp': {
        e.preventDefault();
        const j = nextTrackIndex(i, e.key === 'ArrowDown' ? 1 : -1);
        if (e.shiftKey && o.selectable !== false) {
          if (!sel.has(key)) sel.set(key, true);
          sel.selectRange(orderedKeys(), keyOf(items[j], j));
          syncSelection();
        }
        setActive(j, true);
        break;
      }
      case 'Home': case 'End':
        if (!onRow) return;
        e.preventDefault();
        setActive(nextTrackIndex(e.key === 'Home' ? -1 : items.length, e.key === 'Home' ? 1 : -1), true);
        break;
      case 'PageDown': case 'PageUp': {
        if (!onRow) return;
        e.preventDefault();
        const step = Math.max(1, Math.floor((list.viewport().bottom - list.viewport().top) / list.rowHeight) - 1);
        setActive(Math.max(0, Math.min(items.length - 1, i + (e.key === 'PageDown' ? step : -step))), true);
        break;
      }
      case 'Enter':
        if (!onRow) return;
        e.preventDefault();
        play(i);
        break;
      case ' ':
        if (!onRow) return;
        e.preventDefault();
        if (o.selectable !== false) {
          sel.toggle(key);
          syncSelection();
        } else {
          play(i);
        }
        break;
      case 'Escape':
        if (sel.size) {
          e.preventDefault();
          e.stopPropagation();
          sel.clear();
          syncSelection();
        }
        break;
      case 'Delete': case 'Backspace':
        if (o.onDelete && onRow) {
          e.preventDefault();
          o.onDelete(menuTargets(i));
        }
        break;
      case 'ContextMenu':
        e.preventDefault();
        openRowMenu(i, { anchor: list.rowAt(i)?.querySelector('.mu-row-more') || null });
        break;
      case 'F10':
        if (e.shiftKey) {
          e.preventDefault();
          openRowMenu(i, { anchor: list.rowAt(i)?.querySelector('.mu-row-more') || null });
        }
        break;
      case 'a': case 'A':
        if ((e.ctrlKey || e.metaKey) && o.selectable !== false) {
          e.preventDefault();
          sel.selectAll(orderedKeys());
          syncSelection();
        }
        break;
      default:
    }
  });

  /* ---------------------------------------------------------- reordering */
  function keyMove(i, keyName) {
    const ti = trackIndexOf[i];
    if (ti < 0) return;
    const count = tracks.length;
    let pos = ti;
    if (keyName === 'ArrowUp') pos = ti - 1;
    else if (keyName === 'ArrowDown') pos = ti + 1;
    else if (keyName === 'Home') pos = 0;
    else pos = count - 1;
    if (pos < 0 || pos >= count || pos === ti) return;
    const item = items[i];
    o.reorder.onMove([keyOf(item, i)], pos, { keyboard: true, focusKey: keyOf(item, i) });
  }

  let detachReorder = null;
  if (o.reorder) {
    detachReorder = attachReorder(list, {
      canDrag: (i) => !isHeader(items[i]),
      onDrop: (from, insertion) => {
        const item = items[from];
        if (!item || isHeader(item)) return; // the list changed under the drag
        const k = keyOf(item, from);
        const moving = sel.has(k) && sel.size > 1 ? selectedItems().map((it) => keyOf(it, items.indexOf(it))) : [k];
        if (moving.length === 1) {
          o.reorder.onMove(moving, insertionToPosition(trackIndexOf[from], trackIndexAtInsertion(insertion)), { focusKey: k });
        } else {
          // Several selected rows: position in the list without them.
          const before = items.slice(0, insertion).filter((it, j) => !isHeader(it) && !moving.includes(keyOf(it, j))).length;
          o.reorder.onMove(moving, before, { focusKey: k });
        }
      },
    });
  }

  function trackIndexAtInsertion(insertion) {
    let n = 0;
    for (let j = 0; j < insertion && j < items.length; j++) if (!isHeader(items[j])) n += 1;
    return n;
  }

  /* ----------------------------------------------------------- selection */
  function syncSelection() {
    list.forEachRow((row, item, i) => {
      if (isHeader(item)) return;
      const on = sel.has(keyOf(item, i));
      row.classList.toggle('is-selected', on);
      if (o.selectable !== false) row.setAttribute('aria-selected', String(on));
      const cb = row.querySelector('.mu-row-check');
      if (cb) cb.checked = on;
    });
    el.classList.toggle('has-selection', sel.size > 0);
    renderSelBar();
    o.onSelectionChange?.(selectedItems());
  }

  function renderSelBar() {
    const chosen = selectedItems();
    selBar.hidden = chosen.length === 0;
    if (!chosen.length) {
      selBar.replaceChildren();
      return;
    }
    const ts = chosen.map(getTrack).filter(Boolean);
    const ctx = o.context || null;
    const btn = (label, ic, fn, variant = 'ghost') => ui.button({ label, icon: ic, size: 'sm', variant, onClick: fn });
    selBar.replaceChildren(
      h('span', { class: 'mu-selbar-count', role: 'status' }, `${chosen.length} selected`),
      btn('Play', 'play', () => playTracks(ts, { context: ctx }), 'primary'),
      btn('Play next', 'queue', () => playNext(ts, ctx)),
      btn('Add to queue', 'list', () => addToQueue(ts, ctx)),
      btn('Add to playlist', 'playlist-add', () => dialogs().then((d) => d.addToPlaylistDialog(ts))),
      canEdit(ts) ? btn('Edit tags', 'edit', () => dialogs().then((d) => (ts.length === 1 ? d.editTagsDialog(ts[0]) : d.bulkEditDialog(ts)))) : null,
      ...(o.selectionExtra ? o.selectionExtra(chosen) : []),
      ui.button({ icon: 'x', title: 'Clear selection (Esc)', size: 'sm', variant: 'ghost', onClick: () => { sel.clear(); syncSelection(); } }),
    );
  }

  /* ------------------------------------------------------- playing state */
  function refreshPlaying() {
    const playing = !!getPlayer()?.playing;
    list.forEachRow((row, item, i) => {
      if (isHeader(item)) return;
      const isCur = isCurrent(item, i);
      row.classList.toggle('is-current', isCur);
      row.classList.toggle('is-playing', isCur && playing);
      if (o.rowClass) {
        // Keep per-row classes (e.g. played queue entries) current without a re-render.
        const extra = o.rowClass(item, i);
        for (const c of ['is-past']) row.classList.toggle(c, String(extra || '').split(' ').includes(c));
      }
    });
  }

  const offs = [
    onBus('track', refreshPlaying),
    onBus('state', refreshPlaying),
    onBus('player', refreshPlaying),
    onBus('track-updated', (t) => {
      let hit = false;
      items.forEach((it, i) => {
        if (isHeader(it)) return;
        const cur = getTrack(it);
        if (cur && cur.id === t.id) {
          const next = { ...cur, ...t };
          if (o.getTrack) it.track = next;
          else items[i] = next;
          hit = true;
        }
      });
      if (hit) rebuild(false, focusedRowKey()); // same indexes: the focused row can be found again
    }),
  ];

  /** Key of the row that has focus, read from the items the rows were rendered from. */
  function focusedRowKey() {
    const row = document.activeElement?.closest?.('.mu-row[data-index]');
    if (!row || !list.el.contains(row)) return null;
    const i = Number(row.dataset.index);
    const it = items[i];
    return it && !isHeader(it) ? keyOf(it, i) : null;
  }

  /** Re-derive tracks and re-render after `items` changed; focus follows `focusedKey`'s row. */
  function rebuild(resetSel = true, focusedKey = null) {
    tracks = [];
    trackIndexOf = items.map((it) => {
      if (isHeader(it)) return -1;
      const t = getTrack(it);
      if (!t) return -1;
      tracks.push(t);
      return tracks.length - 1;
    });
    if (resetSel) sel.retain(orderedKeys());
    if (activeIndex >= items.length) activeIndex = Math.max(0, items.length - 1);
    // Settle what sits above the rows first: the visible range is computed from the list's position.
    emptyBox.hidden = loading || items.length > 0;
    skeleton.hidden = !loading;
    list.setItems(items);
    if (focusedKey !== null) {
      const j = items.findIndex((it, k) => !isHeader(it) && keyOf(it, k) === focusedKey);
      if (j >= 0) setActive(j, true);
    }
    renderSelBar();
  }

  if (o.empty) emptyBox.append(o.empty());

  return {
    el,
    get items() { return items; },
    get tracks() { return tracks; },
    get selection() { return sel; },
    selectedItems,
    /** Replace the items. */
    setItems(next) {
      const focusedKey = focusedRowKey();
      items = Array.isArray(next) ? next.slice() : [];
      loading = false;
      rebuild(true, focusedKey);
    },
    /** Append items (next page). */
    appendItems(more) {
      if (!more?.length) return;
      const focusedKey = focusedRowKey();
      items = items.concat(more);
      rebuild(false, focusedKey);
    },
    setLoading(on) {
      loading = !!on;
      skeleton.hidden = !loading;
      emptyBox.hidden = true;
    },
    setEmpty(node) {
      emptyBox.replaceChildren(node);
      emptyBox.hidden = loading || items.length > 0;
    },
    clearSelection() {
      sel.clear();
      syncSelection();
    },
    /** Focus the row of a key (after a move). */
    focusKey(key) {
      const j = items.findIndex((it, k) => !isHeader(it) && keyOf(it, k) === key);
      if (j >= 0) setActive(j, true);
    },
    announce(msg) {
      live.textContent = '';
      setTimeout(() => { live.textContent = msg; }, 30);
    },
    refresh: () => list.refresh(),
    refreshPlaying,
    play,
    destroy() {
      offs.forEach((off) => off());
      detachReorder?.();
      list.destroy();
    },
  };
}

/* ------------------------------------------------------------- page bits */

/** Section with a title, optional "See all" link and content. */
export function section(title, content, { href = null, actions = null, className = '' } = {}) {
  return h('section', { class: cls('mu-section', className) },
    h('div', { class: 'mu-section-head' },
      h('h2', { class: 'mu-section-title' }, title),
      h('div', { class: 'mu-section-actions' }, actions, href ? h('a', { class: 'mu-see-all', href }, 'See all') : null)),
    content);
}

/** Horizontal scroller of cards (Home shelves). */
export function shelf(cards) {
  return h('div', { class: 'mu-shelf' }, cards);
}

/**
 * Big header for album / artist / playlist pages.
 * @param {{art: Node, kicker?: string, title: string, lines?: any[], actions?: Node[], round?: boolean}} o
 */
export function hero({ art, kicker = '', title, lines = [], actions = [], round = false }) {
  return h('header', { class: cls('mu-hero', { 'is-round': round }) },
    h('div', { class: 'mu-hero-art' }, art),
    h('div', { class: 'mu-hero-body' },
      kicker ? h('div', { class: 'mu-hero-kicker' }, kicker) : null,
      h('h1', { class: 'mu-hero-title' }, title),
      ...lines.filter(Boolean).map((l) => h('div', { class: 'mu-hero-line' }, l)),
      actions.length ? h('div', { class: 'mu-hero-actions' }, actions) : null));
}

/** The big round play button of page headers. */
export function bigPlayButton(label, onClick) {
  const b = h('button', { type: 'button', class: 'mu-bigplay', aria: { label }, dataset: { tip: label } }, icon('play', 22));
  b.addEventListener('click', onClick);
  return b;
}

/** Large art for page headers (lg, xl on hi-dpi via srcset). */
export function heroArt(url, iconName = 'disc') {
  const box = artBox(url, 'lg', { className: 'mu-hero-img', iconName });
  const img = box.querySelector('img');
  if (img && url) img.srcset = `${artUrl(url, 'lg')} 640w, ${artUrl(url, 'xl')} 1200w`;
  if (img) img.sizes = '(max-width: 680px) 60vw, 232px';
  return box;
}
