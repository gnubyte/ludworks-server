// Music dialogs: add to playlist, song details (tags, file info, lyrics), the tag editor and
// bulk editor (music:write + write access), playlist create/edit with the smart-rules editor,
// M3U import, and cover uploads. Loaded on demand (also from the dock).

import { h, pickFiles } from '../../core/dom.js';
import * as ui from '../../core/ui.js';
import { field, showFormError, clearErrors, setFieldError } from '../../core/forms.js';
import { formatBytes, formatDuration, formatDateTime } from '../../core/format.js';
import { emit } from './state.js';
import { navigate } from './player.js';
import {
  mapi, artBox, ratingControl, favoriteButton, playTracks, playNext, addToQueue, canEdit, who, loadSpaces,
} from './widgets.js';
import {
  paths, trackTitle, trackArtist, audioQuality, channelLabel, replayGainLabel, codecLabel, songCount, chunk,
  splitList, errorText, isVersionConflict,
} from './model.js';
import { normalizeLyrics, formatLrcTime } from './lyrics.js';
import * as rules from './smartrules.js';

/* ---------------------------------------------------------- add to playlist */

/**
 * Add tracks to one of the caller's editable manual playlists (or a new one).
 * @param {object[]} tracks
 */
export async function addToPlaylistDialog(tracks) {
  const ids = (tracks || []).filter(Boolean).map((t) => t.id);
  if (!ids.length) return;
  const search = h('input', { class: 'input', type: 'search', placeholder: 'Find a playlist…', autocomplete: 'off', aria: { label: 'Find a playlist' } });
  const listEl = h('div', { class: 'mu-pick-list', role: 'listbox', aria: { label: 'Playlists' } }, ui.loadingBlock('Loading playlists…'));
  let all = [];

  const m = ui.modal({
    title: 'Add to playlist',
    description: ids.length === 1 ? `“${trackTitle(tracks[0])}”` : `${songCount(ids.length)}`,
    size: 'sm', className: 'mu-pick-dialog',
    body: h('div', { class: 'stack-sm' }, search, listEl),
    footer: [
      ui.button({ label: 'New playlist', icon: 'plus', onClick: async () => {
        const name = await ui.prompt({ title: 'New playlist', label: 'Name', value: '', confirmLabel: 'Create and add', validate: (v) => (v.trim() ? null : 'Enter a name.') });
        if (!name) return;
        try {
          const created = await mapi.createPlaylist({ name: name.trim(), kind: 'manual', track_ids: ids.slice(0, 10000) });
          emit('playlists', { id: created.id });
          m.close(true);
          ui.toast.success(`Created “${created.name}” with ${songCount(created.track_count)}`, { action: { label: 'Open', onClick: () => navigate(paths.playlist(created.id)) } });
        } catch (err) {
          ui.toast.error(errorText(err));
        }
      } }),
      { label: 'Cancel', value: null },
    ],
  });

  function render() {
    const q = search.value.trim().toLowerCase();
    const shown = all.filter((p) => !q || p.name.toLowerCase().includes(q));
    if (!shown.length) {
      listEl.replaceChildren(ui.emptyState({ icon: 'queue', title: all.length ? 'No matching playlists' : 'No playlists you can add to', compact: true, message: all.length ? '' : 'Create one with “New playlist”.' }));
      return;
    }
    listEl.replaceChildren(...shown.map((p) => {
      const b = h('button', { type: 'button', class: 'mu-pick-item', role: 'option', aria: { selected: 'false' } },
        artBox(p.art_url, 'sm', { iconName: 'queue' }),
        h('span', { class: 'mu-pick-text' }, h('span', { class: 'truncate' }, p.name),
          h('span', { class: 'faint small truncate' }, [p.visibility === 'shared' ? (p.collaborative ? 'Collaborative' : 'Shared') : 'Private', songCount(p.track_count)].join(' · '))));
      b.addEventListener('click', () => add(p, b));
      return b;
    }));
  }

  async function add(p, b) {
    ui.setLoading(b, true);
    try {
      let last = null;
      for (const part of chunk(ids, 1000)) last = await mapi.insertItems(p.id, part);
      emit('playlists', { id: p.id });
      m.close(true);
      ui.toast.success(`Added ${ids.length === 1 ? `“${trackTitle(tracks[0])}”` : songCount(ids.length)} to “${p.name}”`, {
        action: { label: 'Open', onClick: () => navigate(paths.playlist(p.id)) },
      });
      return last;
    } catch (err) {
      ui.setLoading(b, false);
      ui.toast.error(errorText(err));
      return null;
    }
  }

  search.addEventListener('input', render);
  search.addEventListener('keydown', (e) => {
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      listEl.querySelector('button')?.focus();
    } else if (e.key === 'Enter') {
      e.preventDefault();
      listEl.querySelector('button')?.click();
    }
  });
  listEl.addEventListener('keydown', (e) => {
    const btns = [...listEl.querySelectorAll('button')];
    const i = btns.indexOf(document.activeElement);
    if (e.key === 'ArrowDown' && i < btns.length - 1) { e.preventDefault(); btns[i + 1].focus(); }
    if (e.key === 'ArrowUp') { e.preventDefault(); (i > 0 ? btns[i - 1] : search).focus(); }
  });
  try {
    let cursor = null;
    do {
      const page = await mapi.playlists({ kind: 'manual', owner: 'all', sort: 'updated', order: 'desc', limit: 200, cursor });
      all = all.concat((page.items || []).filter((p) => p.can_edit));
      cursor = page.next_cursor;
    } while (cursor && all.length < 1000 && m.isOpen);
    render();
  } catch (err) {
    listEl.replaceChildren(ui.errorState(err, null));
  }
  return m.closed;
}

/* ------------------------------------------------------------ song details */

function kvRows(pairs) {
  return ui.kv(pairs.filter((p) => p && p[1] !== '' && p[1] !== null && p[1] !== undefined));
}

/**
 * Song details drawer: tags, file information, your stats, lyrics; edit actions when allowed.
 * @param {object} track
 */
export async function openTrackDetails(track) {
  await loadSpaces();
  let t = track;
  const body = h('div', { class: 'mu-details' }, ui.loadingBlock('Loading…'));
  const d = ui.drawer({ title: trackTitle(track), description: trackArtist(track), size: 'lg', body, className: 'mu-details-drawer' });
  try {
    t = await mapi.track(track.id);
  } catch (err) {
    body.replaceChildren(ui.errorState(err, null));
    return d.closed;
  }
  render();

  function render() {
    const spaces = who().spaces || [];
    const space = spaces.find((s) => s.id === t.space_id);
    const editable = canEdit([t]);
    const header = h('div', { class: 'mu-details-head' },
      artBox(t.art_url, 'lg', { className: 'mu-details-art' }),
      h('div', { class: 'mu-details-meta' },
        h('div', { class: 'mu-details-title' }, trackTitle(t)),
        h('div', { class: 'muted' }, t.artist_id ? h('a', { href: paths.artist(t.artist_id), onClick: () => d.close() }, trackArtist(t)) : trackArtist(t)),
        t.album ? h('div', { class: 'muted small' }, t.album_id ? h('a', { href: paths.album(t.album_id), onClick: () => d.close() }, t.album) : t.album) : null,
        h('div', { class: 'row wrap mu-details-actions' },
          ui.button({ label: 'Play', icon: 'play', size: 'sm', variant: 'primary', onClick: () => playTracks([t]) }),
          ui.button({ label: 'Play next', icon: 'queue', size: 'sm', onClick: () => playNext([t]) }),
          ui.button({ label: 'Add to queue', icon: 'list', size: 'sm', onClick: () => addToQueue([t]) }),
          favoriteButton('track', t, { size: 'sm' })),
        h('div', { class: 'row mu-details-rating' }, h('span', { class: 'muted small' }, 'Your rating'), ratingControl(t))));

    const tags = kvRows([
      ['Title', trackTitle(t)],
      ['Artist', t.artist],
      t.artists?.length > 1 ? ['Artists', t.artists.join('; ')] : null,
      ['Album', t.album],
      ['Album artist', t.album_artist],
      ['Track', t.track_number ? `${t.track_number}${t.track_total ? ` of ${t.track_total}` : ''}` : ''],
      ['Disc', t.disc_number ? `${t.disc_number}${t.disc_total ? ` of ${t.disc_total}` : ''}` : ''],
      ['Year', t.year ? String(t.year) : ''],
      ['Genres', (t.genres || []).join('; ')],
      ['Composer', t.composer],
      ['Compilation', t.compilation ? 'Yes' : ''],
      ['Subcategory', t.subcategory],
      ['Comment', t.comment],
    ]);
    const file = kvRows([
      ['File', t.name],
      ['Location', `${space ? `${space.name}/` : ''}${t.path}`],
      ['Size', formatBytes(t.size)],
      ['Format', [codecLabel(t.codec), t.container && t.container !== t.codec ? t.container.toUpperCase() : ''].filter(Boolean).join(' in ')],
      ['Quality', audioQuality(t)],
      ['Channels', channelLabel(t.channels)],
      ['Duration', t.duration_seconds ? formatDuration(t.duration_seconds) : ''],
      ['Track gain', replayGainLabel(t.replaygain, 'track')],
      ['Album gain', replayGainLabel(t.replaygain, 'album')],
      ['MIME type', t.mime],
      ['Added', formatDateTime(t.added_at)],
      ['Updated', formatDateTime(t.updated_at)],
      t.probe_status !== 'ok' ? ['Tag reading', t.probe_status === 'pending' ? 'Pending' : 'Failed'] : null,
    ]);
    const mine = kvRows([
      ['Plays', String(t.play_count || 0)],
      ['Last played', t.last_played_at ? formatDateTime(t.last_played_at) : 'Never'],
    ]);
    const actions = h('div', { class: 'row wrap' },
      ui.button({ label: 'Add to playlist…', icon: 'playlist-add', size: 'sm', onClick: () => addToPlaylistDialog([t]) }),
      ui.button({ label: 'Download', icon: 'download', size: 'sm', onClick: () => mapi.download(t.id) }),
      editable ? ui.button({ label: 'Edit tags…', icon: 'edit', size: 'sm', onClick: async () => {
        const updated = await editTagsDialog(t);
        if (updated) {
          t = updated;
          render();
        }
      } }) : null,
      editable ? ui.menuButton({ label: 'Re-read tags', icon: 'refresh', size: 'sm', variant: 'secondary', caret: true, items: [
        { label: 'Re-read tags and art (keep my edits)', onSelect: () => reprobe(false) },
        { label: 'Re-read and discard my edits', danger: true, onSelect: () => reprobe(true) },
      ] }) : null);

    const sections = [
      header,
      h('section', { class: 'mu-details-section' }, h('h3', {}, 'Tags'), tags),
      h('section', { class: 'mu-details-section' }, h('h3', {}, 'File'), file),
      h('section', { class: 'mu-details-section' }, h('h3', {}, 'You'), mine),
      actions,
    ];
    if (t.has_lyrics) {
      const lyr = h('div', { class: 'mu-details-lyrics' }, ui.loadingBlock('Loading lyrics…'));
      sections.splice(2, 0, h('section', { class: 'mu-details-section' }, h('h3', {}, 'Lyrics'), lyr));
      mapi.lyrics(t.id).then((raw) => {
        const l = normalizeLyrics(raw);
        lyr.replaceChildren(l.synced
          ? h('div', { class: 'mu-lyrics-static' }, l.lines.map((x) => h('div', { class: 'mu-lyrics-row' }, h('span', { class: 'faint nums small' }, x.time === null ? '' : formatLrcTime(x.time)), h('span', {}, x.text || '♪'))))
          : h('pre', { class: 'mu-lyrics-plain' }, l.lines.map((x) => x.text).join('\n')),
        h('div', { class: 'faint small' }, `${l.synced ? 'Synced' : 'Plain'} lyrics from ${l.source === 'lrc' ? 'an .lrc file' : 'the file’s tags'}`));
      }).catch((err) => lyr.replaceChildren(h('div', { class: 'muted small' }, err?.status === 404 ? 'No lyrics.' : errorText(err))));
    }
    body.replaceChildren(...sections);
  }

  async function reprobe(reset) {
    try {
      t = await mapi.probe(t.id, reset);
      emit('track-updated', t);
      ui.toast.success(reset ? 'Tags re-read; your edits were discarded' : 'Tags re-read');
      render();
    } catch (err) {
      ui.toast.error(errorText(err));
    }
  }
  return d.closed;
}

/* --------------------------------------------------------------- tag editor */

let categoriesPromise = null;
function musicSubcategories() {
  categoriesPromise ||= mapi.categories().then((list) => (Array.isArray(list) ? list : []).find((c) => c.slug === 'music')?.subcategories || [])
    .catch(() => []);
  return categoriesPromise;
}

const numberInput = (value, props = {}) => h('input', { class: 'input', type: 'number', min: '0', step: '1', inputmode: 'numeric', value: value ?? '', ...props });
const textInput = (value, props = {}) => h('input', { class: 'input', type: 'text', value: value ?? '', autocomplete: 'off', spellcheck: 'false', ...props });

function numOrNull(v) {
  const s = String(v ?? '').trim();
  if (!s) return null;
  const n = Number(s);
  return Number.isFinite(n) ? Math.trunc(n) : NaN;
}

async function subcategorySelect(value) {
  const subs = await musicSubcategories();
  const opts = [{ slug: '', name: '(none)' }, ...subs];
  if (value && !opts.some((s) => s.slug === value)) opts.push({ slug: value, name: value });
  const sel = h('select', { class: 'select' }, opts.map((s) => h('option', { value: s.slug }, s.name)));
  sel.value = value || '';
  return sel;
}

/**
 * Edit one track's tags (PATCH only what changed). Resolves the updated Track or null.
 * @param {object} track
 */
export async function editTagsDialog(track) {
  const t = track;
  const f = {
    title: textInput(t.title),
    artist: textInput(t.artist),
    artists: textInput((t.artists || []).join('; ')),
    album: textInput(t.album),
    album_artist: textInput(t.album_artist),
    track_number: numberInput(t.track_number),
    track_total: numberInput(t.track_total),
    disc_number: numberInput(t.disc_number),
    disc_total: numberInput(t.disc_total),
    year: numberInput(t.year, { min: '1', max: '9999' }),
    genres: textInput((t.genres || []).join('; ')),
    composer: textInput(t.composer),
    compilation: h('input', { type: 'checkbox', class: 'checkbox', checked: !!t.compilation }),
    subcategory: await subcategorySelect(t.subcategory),
  };
  const form = h('div', { class: 'form-grid cols-2 mu-tag-form' },
    field({ label: 'Title', control: f.title, name: 'title', className: 'span-2' }),
    field({ label: 'Artist (as displayed)', control: f.artist, name: 'artist' }),
    field({ label: 'Artists', control: f.artists, name: 'artists', hint: 'Separate several artists with ;' }),
    field({ label: 'Album', control: f.album, name: 'album' }),
    field({ label: 'Album artist', control: f.album_artist, name: 'album_artist' }),
    h('div', { class: 'mu-pair' }, field({ label: 'Track', control: f.track_number, name: 'track_number' }), field({ label: 'of', control: f.track_total, name: 'track_total' })),
    h('div', { class: 'mu-pair' }, field({ label: 'Disc', control: f.disc_number, name: 'disc_number' }), field({ label: 'of', control: f.disc_total, name: 'disc_total' })),
    field({ label: 'Year', control: f.year, name: 'year' }),
    field({ label: 'Genres', control: f.genres, name: 'genres', hint: 'Separate several genres with ;' }),
    field({ label: 'Composer', control: f.composer, name: 'composer' }),
    field({ label: 'Subcategory', control: f.subcategory, name: 'subcategory' }),
    h('label', { class: 'check-row span-2', dataset: { field: 'compilation' } }, f.compilation,
      h('span', { class: 'check-text' }, h('span', { class: 'check-label' }, 'Part of a compilation'), h('span', { class: 'check-hint' }, 'Compilations without an album artist are listed under “Various Artists”.'))));

  const m = ui.modal({
    title: 'Edit tags', description: `${t.name} — your changes are kept when the file is read again.`, size: 'lg', body: form,
    footer: [{ label: 'Cancel', value: null }, { label: 'Save', variant: 'primary', type: 'submit' }],
    onSubmit: async () => {
      clearErrors(form);
      const patch = {};
      const text = (k) => { const v = f[k].value.trim(); if (v !== (t[k] || '')) patch[k] = v; };
      ['title', 'artist', 'album', 'album_artist', 'composer'].forEach(text);
      for (const k of ['track_number', 'track_total', 'disc_number', 'disc_total', 'year']) {
        const v = numOrNull(f[k].value);
        if (Number.isNaN(v)) {
          setFieldError(f[k].closest('.field'), 'Enter a whole number.');
          return false;
        }
        if (v !== (t[k] ?? null)) patch[k] = v;
      }
      const artists = splitList(f.artists.value);
      if (artists.join('\u0000') !== (t.artists || []).join('\u0000')) patch.artists = artists;
      const genres = splitList(f.genres.value);
      if (genres.join('\u0000') !== (t.genres || []).join('\u0000')) patch.genres = genres;
      if (f.compilation.checked !== !!t.compilation) patch.compilation = f.compilation.checked;
      if (f.subcategory.value !== (t.subcategory || '')) patch.subcategory = f.subcategory.value;
      if (!Object.keys(patch).length) return null;
      try {
        const updated = await mapi.patchTrack(t.id, patch);
        emit('track-updated', updated);
        ui.toast.success('Tags saved');
        return updated;
      } catch (err) {
        showFormError(form, err);
        return false;
      }
    },
  });
  return m.closed.then((v) => v || null);
}

/**
 * Bulk edit: change chosen fields of many tracks at once (POST /music/tracks/bulk, ≤ 1000 per call).
 * @param {object[]} tracks
 */
export async function bulkEditDialog(tracks) {
  const ids = tracks.map((t) => t.id);
  const common = (k) => {
    const vals = new Set(tracks.map((t) => JSON.stringify(t[k] ?? null)));
    return vals.size === 1 ? tracks[0][k] : undefined;
  };
  const rows = [];
  const mk = (key, label, control, hint = '') => {
    const cb = h('input', { type: 'checkbox', class: 'checkbox', aria: { label: `Change ${label}` } });
    control.disabled = true;
    cb.addEventListener('change', () => {
      control.disabled = !cb.checked;
      if (cb.checked) control.focus();
    });
    control.addEventListener('input', () => { cb.checked = true; });
    const wrap = field({ label, control, name: key, hint });
    rows.push({ key, cb, control });
    return h('div', { class: 'mu-bulk-row' }, cb, wrap);
  };
  const g = common('genres');
  const a = common('artists');
  const compSel = h('select', { class: 'select' }, h('option', { value: 'true' }, 'Yes'), h('option', { value: 'false' }, 'No'));
  compSel.value = common('compilation') ? 'true' : 'false';
  const body = h('div', { class: 'stack-sm mu-bulk-form' },
    h('p', { class: 'muted small' }, 'Tick the fields to change; the others stay as they are on each song.'),
    mk('artist', 'Artist', textInput(common('artist') ?? '', { placeholder: common('artist') === undefined ? '(several values)' : '' })),
    mk('artists', 'Artists', textInput(a ? a.join('; ') : '', { placeholder: a === undefined ? '(several values)' : '' }), 'Separate several artists with ;'),
    mk('album', 'Album', textInput(common('album') ?? '', { placeholder: common('album') === undefined ? '(several values)' : '' })),
    mk('album_artist', 'Album artist', textInput(common('album_artist') ?? '', { placeholder: common('album_artist') === undefined ? '(several values)' : '' })),
    mk('year', 'Year', numberInput(common('year') ?? '', { min: '1', max: '9999' })),
    mk('genres', 'Genres', textInput(g ? g.join('; ') : '', { placeholder: g === undefined ? '(several values)' : '' }), 'Separate several genres with ;'),
    mk('composer', 'Composer', textInput(common('composer') ?? '')),
    mk('disc_number', 'Disc number', numberInput(common('disc_number') ?? '')),
    mk('disc_total', 'Disc total', numberInput(common('disc_total') ?? '')),
    mk('track_total', 'Track total', numberInput(common('track_total') ?? '')),
    mk('compilation', 'Compilation', compSel),
    mk('subcategory', 'Subcategory', await subcategorySelect(common('subcategory') ?? '')));

  const m = ui.modal({
    title: `Edit ${songCount(tracks.length)}`, size: 'md', body,
    footer: [{ label: 'Cancel', value: null }, { label: 'Save', variant: 'primary', type: 'submit' }],
    onSubmit: async () => {
      clearErrors(body);
      const set = {};
      for (const r of rows) {
        if (!r.cb.checked) continue;
        const v = r.control.value;
        if (['year', 'disc_number', 'disc_total', 'track_total'].includes(r.key)) {
          const n = numOrNull(v);
          if (Number.isNaN(n)) {
            setFieldError(r.control.closest('.field'), 'Enter a whole number.');
            return false;
          }
          set[r.key] = n;
        } else if (r.key === 'artists' || r.key === 'genres') set[r.key] = splitList(v);
        else if (r.key === 'compilation') set.compilation = v === 'true';
        else set[r.key] = v.trim();
      }
      if (!Object.keys(set).length) return null;
      let updated = 0;
      const errors = [];
      try {
        for (const part of chunk(ids, 1000)) {
          const res = await mapi.bulk(part, set);
          updated += res?.updated || 0;
          errors.push(...(res?.errors || []));
        }
      } catch (err) {
        showFormError(body, err, { prefix: 'set.' });
        return false;
      }
      // Refresh the edited tracks everywhere they are shown.
      try {
        for (const part of chunk(ids, 500)) {
          const res = await mapi.lookup(part);
          for (const tr of res?.tracks || []) emit('track-updated', tr);
        }
      } catch { /* the lists refresh on their next load */ }
      if (errors.length) {
        ui.toast.warning(`Updated ${songCount(updated)}; ${errors.length} could not be changed`, { description: errors.slice(0, 3).map((e) => e.error).join(' · '), timeout: 9000 });
      } else {
        ui.toast.success(`Updated ${songCount(updated)}`);
      }
      return true;
    },
  });
  return m.closed;
}

/* ----------------------------------------------------------- cover uploads */

/**
 * Pick an image and upload it (album or playlist cover). Resolves the API answer or null.
 * @param {(file: File, opts: object) => Promise<any>} upload
 */
export async function uploadCover(upload) {
  const [file] = await pickFiles({ multiple: false, accept: 'image/jpeg,image/png,image/gif' });
  if (!file) return null;
  if (file.size > 20 * 1024 * 1024) {
    ui.toast.error('That image is larger than 20 MiB.');
    return null;
  }
  const dismiss = ui.toast('Uploading cover…', { timeout: 0, description: file.name });
  try {
    const res = await upload(file, {});
    ui.toast.success('Cover updated');
    return res;
  } catch (err) {
    ui.toast.error(errorText(err));
    return null;
  } finally {
    dismiss();
  }
}

/* -------------------------------------------------------- playlist dialogs */

function visibilityControls(pl) {
  const vis = h('select', { class: 'select' }, h('option', { value: 'private' }, 'Private — only you'), h('option', { value: 'shared' }, 'Shared — every user can see it'));
  vis.value = pl?.visibility || 'private';
  const collab = h('input', { type: 'checkbox', class: 'checkbox', checked: !!pl?.collaborative });
  const collabRow = h('label', { class: 'check-row', dataset: { field: 'collaborative' } }, collab,
    h('span', { class: 'check-text' }, h('span', { class: 'check-label' }, 'Collaborative'), h('span', { class: 'check-hint' }, 'Everyone can add, remove and reorder songs.')));
  const sync = () => { collabRow.hidden = vis.value !== 'shared'; };
  vis.addEventListener('change', sync);
  sync();
  return { vis, collab, collabRow };
}

/**
 * Create a playlist (manual or smart) or edit an owned playlist's settings and rules.
 * Resolves the Playlist or null.
 * @param {{playlist?: object|null, smart?: boolean, trackIds?: string[]}} o
 */
export function playlistDialog({ playlist = null, smart = false, trackIds = [] } = {}) {
  const isSmart = playlist ? playlist.kind === 'smart' : smart;
  const name = textInput(playlist?.name || '', { required: true, maxlength: '200', spellcheck: 'true' });
  const desc = h('textarea', { class: 'textarea', rows: 3, maxlength: '2000' });
  desc.value = playlist?.description || '';
  const { vis, collab, collabRow } = visibilityControls(playlist);
  const editor = isSmart ? rulesEditor(rules.fromApi(playlist?.rules || null)) : null;
  const body = h('div', { class: 'stack mu-playlist-form' },
    field({ label: 'Name', control: name, name: 'name', required: true }),
    field({ label: 'Description', control: desc, name: 'description' }),
    field({ label: 'Visibility', control: vis, name: 'visibility' }),
    isSmart ? null : collabRow,
    editor ? h('div', { class: 'stack-sm' }, h('div', { class: 'field-label' }, 'Rules'), editor.el) : null);
  const m = ui.modal({
    title: playlist ? (isSmart ? 'Edit smart playlist' : 'Edit playlist') : (isSmart ? 'New smart playlist' : 'New playlist'),
    description: isSmart ? 'Songs that match the rules — worked out for each listener from their own library, plays and favorites.' : '',
    size: isSmart ? 'lg' : 'md', body,
    footer: [{ label: 'Cancel', value: null }, { label: playlist ? 'Save' : 'Create', variant: 'primary', type: 'submit' }],
    onSubmit: async () => {
      clearErrors(body);
      if (!name.value.trim()) {
        setFieldError(name.closest('.field'), 'Enter a name.');
        name.focus();
        return false;
      }
      let apiRules;
      if (editor) {
        const draft = editor.draft();
        const errs = rules.validate(draft);
        if (Object.keys(errs).length) {
          editor.showErrors(errs);
          return false;
        }
        apiRules = rules.toApi(draft);
      }
      const shared = vis.value === 'shared';
      try {
        let res;
        if (playlist) {
          const patch = { base_version: playlist.version };
          if (name.value.trim() !== playlist.name) patch.name = name.value.trim();
          if (desc.value.trim() !== (playlist.description || '')) patch.description = desc.value.trim();
          if (vis.value !== playlist.visibility) patch.visibility = vis.value;
          const c = shared && !isSmart ? collab.checked : false;
          if (c !== !!playlist.collaborative) patch.collaborative = c;
          if (apiRules) patch.rules = apiRules;
          res = await mapi.patchPlaylist(playlist.id, patch);
        } else {
          const bodyIn = { name: name.value.trim(), description: desc.value.trim() || undefined, visibility: vis.value, kind: isSmart ? 'smart' : 'manual' };
          if (!isSmart) bodyIn.collaborative = shared ? collab.checked : false;
          if (apiRules) bodyIn.rules = apiRules;
          if (!isSmart && trackIds.length) bodyIn.track_ids = trackIds;
          res = await mapi.createPlaylist(bodyIn);
        }
        emit('playlists', { id: res.id });
        return res;
      } catch (err) {
        if (isVersionConflict(err)) {
          ui.toast.error(errorText(err));
          emit('playlists', { id: playlist.id }); // pages showing it reload the current version
          return err.details.current;
        }
        if (err?.fields && editor) {
          const ruleErrs = Object.fromEntries(Object.entries(err.fields).filter(([k]) => k.startsWith('rules')));
          if (Object.keys(ruleErrs).length) editor.showErrors(ruleErrs);
          const rest = Object.fromEntries(Object.entries(err.fields).filter(([k]) => !k.startsWith('rules')));
          if (Object.keys(rest).length) showFormError(body, { message: err.message, fields: rest });
          return false;
        }
        showFormError(body, err);
        return false;
      }
    },
  });
  return m.closed.then((v) => v || null);
}

/**
 * The smart-rules editor: match all/any, 1–20 conditions (field, operator, value), sort, order
 * and limit. {el, draft(), showErrors(errors)}.
 * @param {object} initial draft (smartrules.fromApi)
 */
export function rulesEditor(initial) {
  const state = JSON.parse(JSON.stringify(initial));
  const matchSel = h('select', { class: 'select select-inline', aria: { label: 'Match' } },
    h('option', { value: 'all' }, 'all'), h('option', { value: 'any' }, 'any'));
  matchSel.value = state.match;
  matchSel.addEventListener('change', () => { state.match = matchSel.value; });
  const list = h('div', { class: 'mu-rules-list', role: 'list' });
  const addBtn = ui.button({ label: 'Add condition', icon: 'plus', size: 'sm', onClick: () => {
    state.conditions.push(rules.newCondition('genre'));
    renderList();
    list.lastElementChild?.querySelector('select')?.focus();
  } });
  const sortSel = h('select', { class: 'select' }, rules.SORTS.map(([v, l]) => h('option', { value: v }, l)));
  sortSel.value = state.sort;
  const orderSel = h('select', { class: 'select' }, h('option', { value: 'asc' }, 'Ascending'), h('option', { value: 'desc' }, 'Descending'));
  orderSel.value = state.order;
  const limitIn = h('input', { class: 'input', type: 'number', min: '1', max: String(rules.MAX_LIMIT), step: '1', placeholder: `No limit (up to ${rules.UNLIMITED.toLocaleString()})`, value: state.limit });
  sortSel.addEventListener('change', () => { state.sort = sortSel.value; orderWrap.hidden = state.sort === 'random'; });
  orderSel.addEventListener('change', () => { state.order = orderSel.value; });
  limitIn.addEventListener('input', () => { state.limit = limitIn.value; });
  const generalErr = h('div', { class: 'field-error', role: 'alert', hidden: true });
  const orderWrap = field({ label: 'Order', control: orderSel, name: 'rules.order' });
  orderWrap.hidden = state.sort === 'random';
  const el = h('div', { class: 'mu-rules' },
    h('div', { class: 'mu-rules-match' }, h('span', {}, 'Include songs that match '), matchSel, h('span', {}, ' of these conditions:')),
    list, h('div', { class: 'row' }, addBtn), generalErr,
    h('div', { class: 'form-grid mu-rules-sort' },
      field({ label: 'Sort by', control: sortSel, name: 'rules.sort' }), orderWrap,
      field({ label: 'Limit', control: limitIn, name: 'rules.limit', hint: `At most ${rules.MAX_LIMIT.toLocaleString()} songs.` })));

  function valueInputs(c, i) {
    const kind = rules.valueKind(c.field, c.op);
    const fieldDef = rules.FIELDS[c.field] || {};
    const mkText = (value, onInput, props = {}) => {
      const inp = h('input', { class: 'input', type: 'text', value: value ?? '', autocomplete: 'off', ...props });
      inp.addEventListener('input', () => onInput(inp.value));
      return inp;
    };
    const numberProps = fieldDef.unit === 'duration'
      ? { placeholder: 'm:ss', inputmode: 'numeric', aria: { label: `Condition ${i + 1} duration` } }
      : { inputmode: 'decimal', placeholder: fieldDef.unit === 'stars' ? '1–5' : '', aria: { label: `Condition ${i + 1} value` } };
    switch (kind) {
      case 'none': return [];
      case 'number': return [mkText(c.value, (v) => { c.value = v; }, numberProps)];
      case 'range': return [
        mkText(c.value[0], (v) => { c.value[0] = v; }, { ...numberProps, aria: { label: `Condition ${i + 1} from` } }),
        h('span', { class: 'muted small' }, 'and'),
        mkText(c.value[1], (v) => { c.value[1] = v; }, { ...numberProps, aria: { label: `Condition ${i + 1} to` } }),
      ];
      case 'days': {
        const inp = h('input', { class: 'input mu-rule-days', type: 'number', min: '1', max: String(rules.MAX_DAYS), step: '1', value: c.value ?? '', aria: { label: `Condition ${i + 1} days` } });
        inp.addEventListener('input', () => { c.value = inp.value; });
        return [inp, h('span', { class: 'muted small' }, 'days')];
      }
      case 'date': {
        const inp = h('input', { class: 'input', type: 'date', value: c.value ?? '', aria: { label: `Condition ${i + 1} date` } });
        inp.addEventListener('input', () => { c.value = inp.value; });
        return [inp];
      }
      default: return [mkText(c.value, (v) => { c.value = v; }, { aria: { label: `Condition ${i + 1} text` }, maxlength: '500' })];
    }
  }

  function renderList() {
    list.replaceChildren(...state.conditions.map((c, i) => {
      const fieldSel = h('select', { class: 'select', aria: { label: `Condition ${i + 1} field` } },
        rules.FIELD_ORDER.map((f) => h('option', { value: f }, rules.FIELDS[f].label)));
      if (rules.FIELDS[c.field]) fieldSel.value = c.field;
      const opSel = h('select', { class: 'select', aria: { label: `Condition ${i + 1} operator` } },
        rules.opsFor(c.field).map(([op, label]) => h('option', { value: op }, label)));
      opSel.value = c.op;
      fieldSel.addEventListener('change', () => {
        state.conditions[i] = rules.withField(c, fieldSel.value);
        renderList();
        list.children[i]?.querySelector('select')?.focus();
      });
      opSel.addEventListener('change', () => {
        state.conditions[i] = rules.withOp(c, opSel.value);
        renderList();
        list.children[i]?.querySelectorAll('select')[1]?.focus();
      });
      const remove = ui.button({ icon: 'x', title: `Remove condition ${i + 1}`, size: 'sm', variant: 'ghost', onClick: () => {
        state.conditions.splice(i, 1);
        renderList();
        (list.children[Math.min(i, list.children.length - 1)]?.querySelector('select') || addBtn).focus();
      } });
      remove.disabled = state.conditions.length <= 1;
      const err = h('div', { class: 'field-error mu-rule-error', role: 'alert', hidden: true });
      return h('div', { class: 'mu-rule', role: 'listitem', dataset: { index: String(i) } },
        h('div', { class: 'mu-rule-inputs' }, fieldSel, opSel, ...valueInputs(c, i), remove), err);
    }));
    addBtn.disabled = state.conditions.length >= rules.MAX_CONDITIONS;
  }
  renderList();

  return {
    el,
    draft: () => JSON.parse(JSON.stringify(state)),
    showErrors(errs) {
      for (const r of list.querySelectorAll('.mu-rule-error')) { r.hidden = true; r.textContent = ''; }
      generalErr.hidden = true;
      let first = null;
      const general = [];
      for (const [k, msg] of Object.entries(errs)) {
        const tgt = rules.errorTarget(k);
        if (tgt.index >= 0) {
          const row = list.children[tgt.index];
          const box = row?.querySelector('.mu-rule-error');
          if (box) {
            box.textContent = msg.charAt(0).toUpperCase() + msg.slice(1);
            box.hidden = false;
            first ||= row.querySelector('input,select');
          }
        } else if (tgt.part === 'limit') {
          setFieldError(limitIn.closest('.field'), msg);
          first ||= limitIn;
        } else if (tgt.part === 'sort' || tgt.part === 'order') {
          setFieldError(sortSel.closest('.field'), msg);
        } else {
          general.push(msg);
        }
      }
      if (general.length) {
        generalErr.textContent = general.join(' ');
        generalErr.hidden = false;
      }
      first?.focus();
    },
  };
}

/** Rename/duplicate helper: ask for a name. */
export function duplicateDialog(pl) {
  const name = textInput(`${pl.name} (copy)`.slice(0, 200), { required: true, maxlength: '200' });
  const freeze = h('input', { type: 'checkbox', class: 'checkbox' });
  const body = h('div', { class: 'stack-sm' },
    field({ label: 'Name', control: name, name: 'name' }),
    pl.kind === 'smart' ? h('label', { class: 'check-row' }, freeze, h('span', { class: 'check-text' },
      h('span', { class: 'check-label' }, 'Freeze as a manual playlist'), h('span', { class: 'check-hint' }, 'Keep today’s songs instead of the rules.'))) : null);
  const m = ui.modal({
    title: 'Duplicate playlist', size: 'sm', body,
    footer: [{ label: 'Cancel', value: null }, { label: 'Duplicate', variant: 'primary', type: 'submit' }],
    onSubmit: async () => {
      try {
        const res = await mapi.duplicate(pl.id, { name: name.value.trim() || undefined, freeze: pl.kind === 'smart' ? freeze.checked : undefined });
        emit('playlists', { id: res.id });
        return res;
      } catch (err) {
        showFormError(body, err);
        return false;
      }
    },
  });
  return m.closed.then((v) => v || null);
}

/* -------------------------------------------------------------- M3U import */

/** Import an .m3u/.m3u8 file as a new playlist; shows unmatched entries. Resolves the Playlist or null. */
export async function importDialog() {
  const spaces = await loadSpaces();
  const fileIn = h('input', { type: 'file', class: 'input mu-file-input', accept: '.m3u,.m3u8,audio/x-mpegurl,audio/mpegurl,application/vnd.apple.mpegurl' });
  const name = textInput('', { placeholder: 'From the file name', maxlength: '200' });
  const spaceSel = h('select', { class: 'select' }, spaces.map((s) => h('option', { value: s.id }, s.kind === 'home' ? `${s.name} (Home)` : s.name)));
  const base = textInput('', { placeholder: 'e.g. Music' });
  fileIn.addEventListener('change', () => {
    const f = fileIn.files?.[0];
    if (f && !name.value.trim()) name.placeholder = f.name.replace(/\.m3u8?$/i, '');
  });
  const body = h('div', { class: 'stack-sm' },
    h('p', { class: 'muted small' }, 'Entries are matched by path (relative to the folder below), by “Space/path” as Ludworks exports them, or by the “Artist - Title” line.'),
    field({ label: 'Playlist file', control: fileIn, name: 'file' }),
    field({ label: 'Name', control: name, name: 'name' }),
    field({ label: 'Space', control: spaceSel, name: 'space_id' }),
    field({ label: 'Folder the paths are relative to', control: base, name: 'base_path', hint: 'Leave empty for the root of the space.' }));
  const m = ui.modal({
    title: 'Import playlist', size: 'md', body,
    footer: [{ label: 'Cancel', value: null }, { label: 'Import', variant: 'primary', type: 'submit' }],
    onSubmit: async () => {
      clearErrors(body);
      const f = fileIn.files?.[0];
      if (!f) {
        setFieldError(fileIn.closest('.field'), 'Choose an .m3u or .m3u8 file.');
        return false;
      }
      if (f.size > 5 * 1024 * 1024) {
        setFieldError(fileIn.closest('.field'), 'The file is larger than 5 MiB.');
        return false;
      }
      try {
        const res = await mapi.importPlaylist(f, { name: name.value.trim() || f.name.replace(/\.m3u8?$/i, ''), spaceId: spaceSel.value, basePath: base.value.trim().replace(/^\/+|\/+$/g, '') });
        emit('playlists', { id: res.id });
        return res;
      } catch (err) {
        if (err?.status === 422 && !err.fields) {
          showFormError(body, { message: errorText(err) });
          return false;
        }
        showFormError(body, { ...err, message: errorText(err) });
        return false;
      }
    },
  });
  const res = await m.closed;
  if (!res) return null;
  const unmatched = Array.isArray(res.unmatched) ? res.unmatched : [];
  if (unmatched.length) {
    ui.modal({
      title: `Imported “${res.name}”`,
      description: `${songCount(res.track_count)} matched; ${unmatched.length} ${unmatched.length === 1 ? 'entry was' : 'entries were'} not found.`,
      size: 'md',
      body: h('ul', { class: 'mu-unmatched' }, unmatched.slice(0, 500).map((u) => h('li', { class: 'mono small' }, u)),
        unmatched.length > 500 ? h('li', { class: 'faint small' }, `…and ${unmatched.length - 500} more`) : null),
      footer: [{ label: 'Open playlist', variant: 'primary', value: true }],
    }).closed.then(() => navigate(paths.playlist(res.id)));
  } else {
    ui.toast.success(`Imported “${res.name}” with ${songCount(res.track_count)}`);
    navigate(paths.playlist(res.id));
  }
  return res;
}

/* ------------------------------------------------------------- rescan */

/** Re-read the tags of the music below a folder (POST /music/rescan). */
export async function rescanDialog() {
  const spaces = (await loadSpaces()) || [];
  const spaceSel = h('select', { class: 'select' }, h('option', { value: '' }, 'All my spaces'),
    spaces.map((s) => h('option', { value: s.id }, s.kind === 'home' ? `${s.name} (Home)` : s.name)));
  const path = textInput('', { placeholder: 'Whole space' });
  const body = h('div', { class: 'stack-sm' },
    h('p', { class: 'muted small' }, 'Reads the tags, cover art and lyrics of the music files again (your edits are kept). It runs in the background.'),
    field({ label: 'Space', control: spaceSel, name: 'space_id' }),
    field({ label: 'Folder', control: path, name: 'path', hint: 'Optional: only below this folder.' }));
  const m = ui.modal({
    title: 'Rescan music', size: 'sm', body,
    footer: [{ label: 'Cancel', value: null }, { label: 'Rescan', variant: 'primary', type: 'submit' }],
    onSubmit: async () => {
      try {
        await mapi.rescan({ space_id: spaceSel.value || undefined, path: path.value.trim().replace(/^\/+|\/+$/g, '') || undefined });
        ui.toast.success('Rescan started', { description: 'New tags show up as the files are read.' });
        return true;
      } catch (err) {
        showFormError(body, { ...err, message: errorText(err) });
        return false;
      }
    },
  });
  return m.closed;
}

/** Clear the play history (confirm first). */
export async function clearHistoryDialog() {
  const ok = await ui.confirm({
    title: 'Clear play history?', message: 'This removes all your plays and resets your play counts, “recently played” and “most played”. It cannot be undone.',
    confirmLabel: 'Clear history', danger: true,
  });
  if (!ok) return false;
  try {
    await mapi.clearHistory();
    ui.toast.success('Play history cleared');
    return true;
  } catch (err) {
    ui.toast.error(errorText(err));
    return false;
  }
}
