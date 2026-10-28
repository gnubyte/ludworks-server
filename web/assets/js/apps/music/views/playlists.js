// Playlists: the list (own and shared, manual and smart, with art mosaics) and a playlist page
// with play/shuffle, settings for the owner (details, smart rules, cover, delete), duplicate,
// M3U export, and — when the caller may edit the items — removing and reordering songs by drag
// and drop or Alt+↑/↓. Every change sends base_version; a conflict reloads the playlist.

import { h } from '../../../core/dom.js';
import * as ui from '../../../core/ui.js';
import { formatRelative } from '../../../core/format.js';
import { on, emit } from '../state.js';
import {
  mapi, playlistCard, createTrackList, playTracks, hero, heroArt, bigPlayButton, dialogs,
} from '../widgets.js';
import {
  PLAYLIST_SORTS, normalizeSort, paths, songCount, formatLongDuration, isPlaylistOwner, canEditItems, errorText,
  isVersionConflict, addedByLabel, trackTitle,
} from '../model.js';
import { moveWithin } from '../queue.js';
import { describe as describeRules } from '../smartrules.js';
import { pageHead, sortControl, toolbar, pagedGrid, heroSkeleton, loadError } from './common.js';

/* ------------------------------------------------------------------ list */

export function mountPlaylists(host, app, route) {
  app.setTitle('Playlists');
  let st = normalizeSort(PLAYLIST_SORTS, route.query.sort, route.query.order);
  let owner = ['me', 'others'].includes(route.query.owner) ? route.query.owner : 'all';
  const page = h('div', { class: 'mu-page' });
  host.append(page);
  let grid = null;

  const newPlaylist = async (smart) => {
    const d = await dialogs();
    const p = await d.playlistDialog({ smart });
    if (p) app.navigate(paths.playlist(p.id));
  };

  function build() {
    grid?.destroy();
    grid = pagedGrid({
      fetchPage: (cursor) => mapi.playlists({ owner, sort: st.sort, order: st.order, cursor, limit: 100 }),
      render: (p) => playlistCard(p, { me: app.me }),
      empty: () => ui.emptyState({
        icon: 'queue', title: owner === 'others' ? 'Nobody shared a playlist yet' : 'No playlists yet',
        message: owner === 'others' ? 'Playlists other users share show up here.' : 'Collect songs in a playlist, or let a smart playlist pick them by rules.',
        action: owner === 'others' ? null : h('div', { class: 'row wrap' },
          ui.button({ label: 'New playlist', icon: 'plus', variant: 'primary', onClick: () => newPlaylist(false) }),
          ui.button({ label: 'New smart playlist', icon: 'sparkle', onClick: () => newPlaylist(true) })),
      }),
    });
    const ownerCtl = ui.segmented({
      options: [{ value: 'all', label: 'All' }, { value: 'me', label: 'Mine' }, { value: 'others', label: 'Shared with me' }],
      value: owner, size: 'sm', ariaLabel: 'Whose playlists',
      onChange: (v) => app.setQuery({ owner: v === 'all' ? null : v }),
    });
    page.replaceChildren(
      pageHead('Playlists', { actions: [
        ui.button({ label: 'New playlist', icon: 'plus', variant: 'primary', onClick: () => newPlaylist(false) }),
        ui.button({ label: 'Smart playlist', icon: 'sparkle', onClick: () => newPlaylist(true) }),
        ui.button({ label: 'Import', icon: 'upload', title: 'Import an .m3u / .m3u8 playlist', onClick: () => dialogs().then((d) => d.importDialog()) }),
      ] }),
      toolbar(ownerCtl.el, h('span', { class: 'spacer' }), sortControl(PLAYLIST_SORTS, st, (v) => app.setQuery({ sort: v.sort, order: v.order }))),
      grid.el);
  }
  build();
  let t = 0;
  const off = on('playlists', () => {
    clearTimeout(t);
    t = setTimeout(() => grid?.reload(), 300);
  });
  return {
    update(r) {
      const next = normalizeSort(PLAYLIST_SORTS, r.query.sort, r.query.order);
      const nextOwner = ['me', 'others'].includes(r.query.owner) ? r.query.owner : 'all';
      if (next.sort !== st.sort || next.order !== st.order || nextOwner !== owner) {
        st = next;
        owner = nextOwner;
        build();
      }
    },
    destroy() {
      clearTimeout(t);
      off();
      grid?.destroy();
    },
  };
}

/* ---------------------------------------------------------------- detail */

export function mountPlaylist(host, app, route) {
  const ctrl = new AbortController();
  const id = route.id;
  const ctx = { kind: 'playlist', id };
  const page = h('div', { class: 'mu-page mu-detail' }, heroSkeleton());
  host.append(page);
  let pl = null; // Playlist + items
  let list = null;
  let busy = Promise.resolve();
  const headBox = h('div');
  const noteBox = h('div');
  const listBox = h('div');

  async function load({ keepScroll = false } = {}) {
    let res;
    try {
      res = await mapi.playlist(id, ctrl.signal);
    } catch (err) {
      if (ctrl.signal.aborted) return;
      page.replaceChildren(err?.status === 404
        ? h('div', { class: 'page' }, ui.emptyState({ icon: 'queue', title: 'Playlist not found', message: 'It was deleted, or it is private.', action: ui.button({ label: 'All playlists', icon: 'arrow-left', href: paths.playlists() }) }))
        : loadError(err, () => load()));
      return;
    }
    if (ctrl.signal.aborted) return;
    if (pl && list && keepScroll && page.contains(list.el)) {
      apply(res);
      return;
    }
    pl = res;
    renderAll();
  }

  const owner = () => isPlaylistOwner(pl, app.me);
  const editable = () => canEditItems(pl);
  const tracks = () => (pl.items || []).map((it) => it.track);

  function renderHead() {
    app.setTitle(pl.name);
    const smart = pl.kind === 'smart';
    const kicker = [smart ? 'Smart playlist' : 'Playlist', pl.visibility === 'shared' ? (pl.collaborative ? 'Collaborative' : 'Shared') : 'Private'].join(' · ');
    const byLine = owner() ? 'By you' : `By ${pl.owner_name || 'another user'}`;
    const menuItems = () => [
      ...(owner() ? [{ label: smart ? 'Edit details and rules…' : 'Edit details…', icon: 'edit', onSelect: () => editDetails() }] : []),
      { label: smart ? 'Duplicate (or freeze)…' : 'Duplicate…', icon: 'copy', onSelect: () => duplicate() },
      { label: 'Export as M3U8', icon: 'download', onSelect: () => mapi.exportPlaylist(id) },
      ...(owner() ? [
        { separator: true },
        { label: 'Change cover…', icon: 'image', onSelect: () => changeCover() },
        ...(pl.has_custom_art ? [{ label: 'Use the album mosaic', icon: 'grid', onSelect: () => removeCover() }] : []),
        { separator: true },
        { label: 'Delete playlist…', icon: 'trash', danger: true, onSelect: () => remove() },
      ] : []),
    ];
    const items = pl.items || [];
    headBox.replaceChildren(hero({
      art: heroArt(pl.art_url, smart ? 'sparkle' : 'queue'),
      kicker, title: pl.name,
      lines: [
        pl.description ? h('span', { class: 'mu-hero-desc' }, pl.description) : null,
        [byLine, songCount(items.length), pl.duration_seconds > 0 ? formatLongDuration(pl.duration_seconds) : '', `updated ${formatRelative(pl.updated_at)}`].filter(Boolean).join(' · '),
      ],
      actions: [
        bigPlayButton(`Play ${pl.name}`, () => playTracks(tracks(), { context: ctx })),
        ui.button({ label: 'Shuffle', icon: 'shuffle', onClick: () => playTracks(tracks(), { index: null, shuffle: true, context: ctx }) }),
        owner() ? ui.button({ label: 'Edit', icon: 'edit', onClick: () => editDetails() }) : null,
        ui.menuButton({ icon: 'more', title: 'More playlist options', variant: 'ghost', items: menuItems }),
      ].filter(Boolean),
    }));
    const notes = [];
    if (smart && pl.rules) notes.push(ui.alert({ variant: 'info', icon: 'sparkle', title: 'Smart playlist', message: describeRules(pl.rules) }));
    if (pl.unavailable_count > 0) {
      notes.push(ui.alert({ variant: 'warning', message: `${songCount(pl.unavailable_count)} in this playlist ${pl.unavailable_count === 1 ? 'is' : 'are'} in files you cannot read, so ${pl.unavailable_count === 1 ? 'it is' : 'they are'} not shown.` }));
    }
    noteBox.replaceChildren(...notes);
  }

  function renderList() {
    list?.destroy();
    const canReorder = editable();
    list = createTrackList({
      label: `Songs of ${pl.name}`, context: ctx, art: true, album: true,
      getTrack: (it) => it.track, keyOf: (it) => it.id,
      extra: pl.collaborative ? (it) => {
        const by = addedByLabel(it, pl, app.me);
        return by ? `Added by ${by}` : '';
      } : (it) => (it.added_at ? `Added ${formatRelative(it.added_at)}` : ''),
      reorder: canReorder ? { onMove: (keys, position, meta) => moveItems(keys, position, meta) } : null,
      menuExtra: canReorder ? (targets) => [
        { label: targets.length === 1 ? 'Remove from playlist' : `Remove ${targets.length} songs from playlist`, icon: 'trash', danger: true, onSelect: () => removeItems(targets.map((t) => t.id)) },
        ...(targets.length === 1 ? [{ label: 'Move to position…', icon: 'sort', onSelect: () => moveToDialog(targets[0]) }] : []),
      ] : null,
      selectionExtra: canReorder ? (chosen) => [ui.button({ label: 'Remove', icon: 'trash', size: 'sm', variant: 'danger-ghost', onClick: () => removeItems(chosen.map((c) => c.id)) })] : null,
      onDelete: canReorder ? (targets) => removeItems(targets.map((t) => t.id)) : null,
      empty: () => ui.emptyState({
        icon: 'music', title: pl.kind === 'smart' ? 'No songs match the rules' : 'This playlist is empty',
        message: pl.kind === 'smart' ? (owner() ? 'Edit the rules to include more songs.' : '') : 'Add songs from any song’s menu (⋯ → Add to playlist).',
        compact: true,
      }),
    });
    list.setItems(pl.items || []);
    listBox.replaceChildren(list.el);
  }

  function renderAll() {
    renderHead();
    renderList();
    page.replaceChildren(headBox, noteBox, listBox);
  }

  /** Apply a server answer (Playlist + items) without losing the scroll position. */
  function apply(res, { focusKey = null } = {}) {
    if (!res) return;
    const smartChanged = res.kind !== pl.kind || res.can_edit !== pl.can_edit || res.collaborative !== pl.collaborative;
    pl = res;
    renderHead();
    if (smartChanged) renderList();
    else list.setItems(pl.items || []);
    if (focusKey) list.focusKey(focusKey);
  }

  /** Serialise item changes; a version conflict reloads the playlist. */
  function mutate(optimistic, call, { focusKey = null, announce = null } = {}) {
    const run = async () => {
      if (ctrl.signal.aborted || !pl) return;
      const before = pl;
      if (optimistic) {
        pl = { ...pl, items: optimistic(pl.items || []) };
        list.setItems(pl.items);
        if (focusKey) list.focusKey(focusKey);
        if (announce) list.announce(announce());
      }
      try {
        const res = await call(before.version);
        apply(res, { focusKey });
      } catch (err) {
        if (isVersionConflict(err)) {
          ui.toast.warning(errorText(err));
          await load({ keepScroll: true });
          return;
        }
        pl = before;
        list.setItems(pl.items || []);
        ui.toast.error(errorText(err), { description: 'The playlist was not changed.' });
      }
    };
    busy = busy.then(run, run);
    return busy;
  }

  function moveItems(keys, position, meta = {}) {
    const set = new Set(keys);
    const first = (pl.items || []).find((it) => set.has(it.id));
    return mutate(
      (items) => moveWithin(items, (it) => set.has(it.id), position),
      (v) => mapi.moveItems(id, keys, position, v),
      {
        focusKey: meta.focusKey || null,
        announce: () => {
          const count = (pl.items || []).length;
          const at = (pl.items || []).findIndex((it) => it.id === first?.id) + 1;
          return keys.length === 1 ? `Moved “${trackTitle(first?.track)}” to position ${at} of ${count}.` : `Moved ${keys.length} songs to position ${at} of ${count}.`;
        },
      },
    );
  }

  function removeItems(itemIds) {
    if (!itemIds.length) return null;
    const set = new Set(itemIds);
    const removed = (pl.items || []).filter((it) => set.has(it.id));
    list.clearSelection();
    return mutate(
      (items) => items.filter((it) => !set.has(it.id)),
      (v) => mapi.removeItems(id, itemIds, v),
      { announce: () => (removed.length === 1 ? `Removed “${trackTitle(removed[0].track)}”.` : `Removed ${removed.length} songs.`) },
    ).then(() => {
      if (removed.length && !ctrl.signal.aborted) {
        ui.toast(removed.length === 1 ? `Removed “${trackTitle(removed[0].track)}”` : `Removed ${removed.length} songs`, {
          timeout: 9000, action: { label: 'Undo', onClick: () => undoRemove(removed) },
        });
      }
    });
  }

  function undoRemove(removed) {
    // Put the songs back where they were (positions in the list as it was).
    const sorted = removed.slice().sort((a, b) => a.position - b.position);
    let chain = Promise.resolve();
    for (const it of sorted) {
      chain = chain.then(() => mutate(null, (v) => mapi.insertItems(id, [it.track.id], { position: Math.min(it.position, (pl.items || []).length), baseVersion: v })));
    }
    return chain;
  }

  async function moveToDialog(item) {
    const count = (pl.items || []).length;
    const cur = (pl.items || []).findIndex((it) => it.id === item.id) + 1;
    const v = await ui.prompt({
      title: 'Move to position', label: `Position (1–${count})`, value: String(cur), type: 'number', confirmLabel: 'Move',
      validate: (s) => (Number.isInteger(Number(s)) && Number(s) >= 1 && Number(s) <= count ? null : `Enter a number from 1 to ${count}.`),
    });
    if (v === null) return;
    const pos = Number(v) - 1;
    if (pos !== cur - 1) moveItems([item.id], pos, { focusKey: item.id });
  }

  async function editDetails() {
    // The dialog announces the change ('playlists'), which reloads this page.
    const d = await dialogs();
    await d.playlistDialog({ playlist: pl });
  }

  async function duplicate() {
    const d = await dialogs();
    const res = await d.duplicateDialog(pl);
    if (res) {
      ui.toast.success(`Created “${res.name}”`);
      app.navigate(paths.playlist(res.id));
    }
  }

  async function changeCover() {
    const d = await dialogs();
    const res = await d.uploadCover((file, o) => mapi.putPlaylistArt(id, file, o));
    if (res) emit('playlists', { id }); // reloads this page and the lists showing the cover
  }

  async function removeCover() {
    try {
      await mapi.deletePlaylistArt(id);
      await load({ keepScroll: true });
    } catch (err) {
      ui.toast.error(errorText(err));
    }
  }

  async function remove() {
    const ok = await ui.confirm({
      title: `Delete “${pl.name}”?`, message: pl.visibility === 'shared' ? 'Other users lose access to it too. The songs themselves stay in your library.' : 'The songs themselves stay in your library.',
      confirmLabel: 'Delete playlist', danger: true,
    });
    if (!ok) return;
    try {
      await mapi.deletePlaylist(id);
      ui.toast.success(`Deleted “${pl.name}”`);
      emit('playlists', {});
      app.navigate(paths.playlists(), { replace: true });
    } catch (err) {
      ui.toast.error(errorText(err));
    }
  }

  // Songs added from elsewhere (Add to playlist) show up right away.
  const offs = [on('playlists', (e) => {
    if (e?.id === id && pl) load({ keepScroll: true });
  })];
  load();
  return {
    destroy() {
      ctrl.abort();
      offs.forEach((off) => off());
      list?.destroy();
    },
  };
}

