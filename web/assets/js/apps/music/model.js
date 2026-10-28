// Music app pure helpers (no DOM): routes and URLs, display labels, sort options, permissions
// and error wording. Unit tested in web/tests/music/model.test.mjs.

/** API ids (API.md §0). */
export const ID_RE = /^[A-Za-z0-9_-]{8,64}$/;

/** True for a string that looks like an API id. */
export function isId(v) {
  return typeof v === 'string' && ID_RE.test(v);
}

/** The app's sections (tab bar). */
export const TABS = Object.freeze([
  { id: 'home', label: 'Home', icon: 'home', href: '/music' },
  { id: 'songs', label: 'Songs', icon: 'music', href: '/music/songs' },
  { id: 'albums', label: 'Albums', icon: 'disc', href: '/music/albums' },
  { id: 'artists', label: 'Artists', icon: 'mic', href: '/music/artists' },
  { id: 'genres', label: 'Genres', icon: 'tag', href: '/music/genres' },
  { id: 'playlists', label: 'Playlists', icon: 'queue', href: '/music/playlists' },
  { id: 'favorites', label: 'Favorites', icon: 'heart', href: '/music/favorites' },
  { id: 'history', label: 'History', icon: 'history', href: '/music/history' },
]);

const NOT_FOUND = Object.freeze({ view: 'notfound', tab: '' });

/**
 * Parse the path below /music.
 *   ''                  home          'songs' 'albums' 'artists' 'genres' 'playlists' 'favorites' 'history'
 *   'albums/<id>'       album         'artists/<id>' artist          'playlists/<id>' playlist
 *   'genre' (?name=)    genre         'search' (?q=)  search          'now-playing'   now playing
 * @param {string} rest
 * @returns {{view: string, tab: string, id?: string}}
 */
export function parseRoute(rest = '') {
  const parts = String(rest || '').split('/').filter(Boolean);
  const [a, b, ...more] = parts;
  if (!a) return { view: 'home', tab: 'home' };
  if (more.length) return NOT_FOUND;
  switch (a) {
    case 'songs': case 'albums': case 'artists': case 'genres': case 'playlists': case 'favorites': case 'history':
      if (!b) return { view: a, tab: a };
      if (a === 'albums' || a === 'artists' || a === 'playlists') return isId(b) ? { view: a.slice(0, -1), id: b, tab: a } : NOT_FOUND;
      return NOT_FOUND;
    case 'genre': return b ? NOT_FOUND : { view: 'genre', tab: 'genres' };
    case 'search': return b ? NOT_FOUND : { view: 'search', tab: '' };
    case 'now-playing': return b ? NOT_FOUND : { view: 'nowplaying', tab: '' };
    default: return NOT_FOUND;
  }
}

const enc = encodeURIComponent;

/** App URLs. */
export const paths = Object.freeze({
  home: () => '/music',
  songs: () => '/music/songs',
  albums: () => '/music/albums',
  artists: () => '/music/artists',
  genres: () => '/music/genres',
  playlists: () => '/music/playlists',
  favorites: (tab) => `/music/favorites${tab ? `?tab=${enc(tab)}` : ''}`,
  history: () => '/music/history',
  album: (id) => `/music/albums/${enc(id)}`,
  artist: (id) => `/music/artists/${enc(id)}`,
  playlist: (id) => `/music/playlists/${enc(id)}`,
  genre: (name) => `/music/genre?name=${enc(name)}`,
  search: (q) => `/music/search${q ? `?q=${enc(q)}` : ''}`,
  nowPlaying: (tab) => `/music/now-playing${tab ? `?tab=${enc(tab)}` : ''}`,
});

/** Art sizes served by the API (longest edge in px). */
export const ART_SIZES = Object.freeze({ sm: 160, md: 320, lg: 640, xl: 1200 });

/**
 * An art_url (API path with ?v=) for one size, or null.
 * @param {string|null|undefined} url
 * @param {'sm'|'md'|'lg'|'xl'} size
 */
export function artUrl(url, size = 'md') {
  if (typeof url !== 'string' || !url.startsWith('/api/')) return null;
  return `${url}${url.includes('?') ? '&' : '?'}size=${ART_SIZES[size] ? size : 'md'}`;
}

/* ------------------------------------------------------------------ labels */

/** File name without its extension. */
export function baseName(name) {
  const n = String(name || '');
  const i = n.lastIndexOf('.');
  return i > 0 ? n.slice(0, i) : n;
}

/** Display title of a track. */
export function trackTitle(t) {
  return (t?.title || '').trim() || baseName(t?.name) || 'Untitled';
}

/** Display artist line of a track. */
export function trackArtist(t) {
  return (t?.artist || '').trim() || (Array.isArray(t?.artists) && t.artists.length ? t.artists.join(', ') : '') || 'Unknown artist';
}

/** Display album title. */
export function albumTitle(a) {
  return (typeof a === 'string' ? a : a?.title || '').trim() || 'Unknown album';
}

/** "Title — Artist" for announcements and menus. */
export function trackLabel(t) {
  return `${trackTitle(t)} — ${trackArtist(t)}`;
}

/** Duration in seconds of a track, or 0 when unknown. */
export function trackDuration(t) {
  const d = Number(t?.duration_seconds);
  return Number.isFinite(d) && d > 0 ? d : 0;
}

/** Sum of the known durations of tracks. */
export function totalDuration(tracks) {
  return (tracks || []).reduce((s, t) => s + trackDuration(t), 0);
}

/**
 * Long-form total time: "2 hr 5 min", "42 min", "35 sec".
 * @param {number} seconds
 */
export function formatLongDuration(seconds) {
  const t = Math.round(Number(seconds) || 0);
  if (t <= 0) return '0 min';
  if (t < 60) return `${t} sec`;
  const h = Math.floor(t / 3600);
  const m = Math.round((t % 3600) / 60);
  if (h === 0) return `${m} min`;
  if (m === 60) return `${h + 1} hr`;
  return m ? `${h} hr ${m} min` : `${h} hr`;
}

/** "1 song" / "12 songs". */
export function songCount(n) {
  const v = Number(n) || 0;
  return `${v.toLocaleString()} ${v === 1 ? 'song' : 'songs'}`;
}

/** "12 · 2" style disc/track number of a track ('' when unknown). */
export function trackNumberLabel(t) {
  return Number.isInteger(t?.track_number) && t.track_number > 0 ? String(t.track_number) : '';
}

/**
 * Album tracks grouped by disc (in order). A single group with disc null when the album has
 * one disc (or no disc numbers).
 * @param {object[]} tracks
 * @returns {{disc: number|null, tracks: object[]}[]}
 */
export function groupByDisc(tracks) {
  const list = tracks || [];
  const discs = new Set(list.map((t) => (Number.isInteger(t.disc_number) && t.disc_number > 0 ? t.disc_number : 1)));
  if (discs.size <= 1) return [{ disc: null, tracks: list }];
  const groups = [];
  for (const t of list) {
    const d = Number.isInteger(t.disc_number) && t.disc_number > 0 ? t.disc_number : 1;
    let g = groups.find((x) => x.disc === d);
    if (!g) {
      g = { disc: d, tracks: [] };
      groups.push(g);
    }
    g.tracks.push(t);
  }
  return groups;
}

const CODEC_LABELS = {
  mp3: 'MP3', aac: 'AAC', alac: 'ALAC', flac: 'FLAC', opus: 'Opus', vorbis: 'Vorbis', pcm: 'PCM', adpcm: 'ADPCM',
  ac3: 'AC-3', eac3: 'E-AC-3', wmav2: 'WMA', wmav1: 'WMA', wmapro: 'WMA Pro', ape: 'APE', wavpack: 'WavPack',
};

/** Human codec name. */
export function codecLabel(codec) {
  const c = String(codec || '').toLowerCase();
  return CODEC_LABELS[c] || (c ? c.toUpperCase() : '');
}

/**
 * Technical summary: "FLAC · 44.1 kHz · 16-bit" or "MP3 · 320 kbps".
 * @param {object} t Track
 */
export function audioQuality(t) {
  const parts = [codecLabel(t?.codec)];
  const lossless = ['flac', 'alac', 'pcm', 'wavpack', 'ape'].includes(String(t?.codec || '').toLowerCase());
  if (lossless) {
    if (t?.sample_rate) parts.push(`${Math.round(t.sample_rate / 10) / 100} kHz`);
    if (t?.bit_depth) parts.push(`${t.bit_depth}-bit`);
  } else if (t?.bitrate_kbps) {
    parts.push(`${t.bitrate_kbps} kbps`);
  }
  return parts.filter(Boolean).join(' · ');
}

/** "Mono" / "Stereo" / "5.1" style channel label. */
export function channelLabel(n) {
  const v = Number(n);
  if (!Number.isFinite(v) || v <= 0) return '';
  if (v === 1) return 'Mono';
  if (v === 2) return 'Stereo';
  if (v === 6) return '5.1';
  if (v === 8) return '7.1';
  return `${v} channels`;
}

/** "−6.5 dB (peak 0.98)" or '' without ReplayGain. */
export function replayGainLabel(rg, which = 'track') {
  if (!rg) return '';
  const gain = which === 'album' ? rg.album_gain_db : rg.track_gain_db;
  const peak = which === 'album' ? rg.album_peak : rg.track_peak;
  if (gain === null || gain === undefined || !Number.isFinite(Number(gain))) return '';
  const g = Number(gain);
  const sign = g > 0 ? '+' : g < 0 ? '−' : '';
  const p = Number(peak);
  return `${sign}${Math.abs(g).toFixed(2)} dB${Number.isFinite(p) && p > 0 && p !== 1 ? ` (peak ${p.toFixed(3)})` : ''}`;
}

/** Secondary line for an album card: "Artist · 1999". */
export function albumSubtitle(a) {
  return [a?.artist || 'Unknown artist', a?.year ? String(a.year) : ''].filter(Boolean).join(' · ');
}

/* ------------------------------------------------------------------- sorts */

/** Sort options of GET /music/tracks (with the natural direction of each). */
export const SONG_SORTS = Object.freeze([
  { value: 'title', label: 'Title', order: 'asc' },
  { value: 'artist', label: 'Artist', order: 'asc' },
  { value: 'album', label: 'Album', order: 'asc' },
  { value: 'added', label: 'Recently added', order: 'desc' },
  { value: 'year', label: 'Year', order: 'desc' },
  { value: 'duration', label: 'Duration', order: 'asc' },
  { value: 'plays', label: 'Most played', order: 'desc' },
  { value: 'last_played', label: 'Recently played', order: 'desc' },
]);

/** Sort options of GET /music/albums. */
export const ALBUM_SORTS = Object.freeze([
  { value: 'title', label: 'Title', order: 'asc' },
  { value: 'artist', label: 'Artist', order: 'asc' },
  { value: 'year', label: 'Year', order: 'desc' },
  { value: 'added', label: 'Recently added', order: 'desc' },
  { value: 'plays', label: 'Most played', order: 'desc' },
]);

/** Sort options of GET /music/artists. */
export const ARTIST_SORTS = Object.freeze([
  { value: 'name', label: 'Name', order: 'asc' },
  { value: 'added', label: 'Recently added', order: 'desc' },
  { value: 'plays', label: 'Most played', order: 'desc' },
]);

/** Sort options of GET /music/playlists. */
export const PLAYLIST_SORTS = Object.freeze([
  { value: 'name', label: 'Name', order: 'asc' },
  { value: 'updated', label: 'Recently changed', order: 'desc' },
  { value: 'created', label: 'Recently created', order: 'desc' },
]);

/**
 * A valid {sort, order} from user input (unknown values fall back to the first option and its
 * natural order; a missing order uses the option's natural order).
 * @param {{value: string, order: string}[]} options
 * @param {string|null|undefined} sort
 * @param {string|null|undefined} order
 */
export function normalizeSort(options, sort, order) {
  const opt = options.find((o) => o.value === sort) || options[0];
  return { sort: opt.value, order: order === 'asc' || order === 'desc' ? order : opt.order };
}

/* ------------------------------------------------------------ permissions */

/** True when the session's scopes include `scope` (sessions get "*"). */
export function hasScope(scopes, scope) {
  return Array.isArray(scopes) && (scopes.includes('*') || scopes.includes(scope));
}

/**
 * Can the caller change things in a space? `spaces` is GET /spaces; admins may write every
 * space, also ones not listed (other users' homes).
 * @param {object[]|null} spaces
 * @param {string} spaceId
 * @param {boolean} [isAdmin]
 */
export function canWriteSpace(spaces, spaceId, isAdmin = false) {
  if (isAdmin) return true;
  const s = (spaces || []).find((x) => x.id === spaceId);
  return !!s && (s.access === 'owner' || s.access === 'write');
}

/**
 * Can the caller edit tags (and re-read them) of these tracks: the music:write scope and write
 * access to every track's space.
 * @param {object[]} tracks
 * @param {{scopes: string[], spaces: object[]|null, isAdmin?: boolean}} who
 */
export function canEditTracks(tracks, who) {
  const list = (tracks || []).filter(Boolean);
  return list.length > 0 && hasScope(who?.scopes, 'music:write') && list.every((t) => canWriteSpace(who.spaces, t.space_id, who.isAdmin));
}

/** Album art may be set with write access to the space of every track of the album (API.md §12). */
export function canEditAlbumArt(albumTracks, who) {
  return canEditTracks(albumTracks, who);
}

/** Is the caller the owner of a playlist (settings, art and delete are owner-only)? */
export function isPlaylistOwner(pl, userId) {
  return !!pl && !!userId && pl.owner_id === userId;
}

/** Can the caller change a playlist's items (manual playlists with can_edit)? */
export function canEditItems(pl) {
  return !!pl && pl.kind !== 'smart' && !!pl.can_edit;
}

/* ------------------------------------------------------------------ errors */

/**
 * Friendly text for API errors in this app (409 limits and conflicts, 422, 429, 501).
 * @param {any} err ApiError
 * @param {string} [fallback]
 */
export function errorText(err, fallback = 'Something went wrong.') {
  if (!err) return fallback;
  if (typeof err === 'string') return err;
  const status = err.status;
  if (status === 501 || err.code === 'ffmpeg_unavailable') {
    return 'The server cannot convert audio (ffmpeg is not installed), so this is not available.';
  }
  if (status === 429) {
    const wait = Number(err.retryAfter);
    return `The server is busy right now.${Number.isFinite(wait) && wait > 0 ? ` Try again in ${wait} s.` : ' Try again in a moment.'}`;
  }
  if (status === 409 && err.details && err.details.current) {
    return 'Someone else changed this playlist meanwhile. It was reloaded — please try again.';
  }
  if (status === 422 && err.details && err.details.fields) {
    const msgs = Object.entries(err.details.fields).map(([k, v]) => `${humanField(k)}: ${v}`);
    if (msgs.length) return msgs.join(' · ');
  }
  if (status === 413) return 'That file is too large.';
  if (status === 415) return 'That image type is not supported (use JPEG, PNG or GIF).';
  return err.message || fallback;
}

/** "rules.conditions[2].value" → "Condition 3 value", "track_ids" → "Track ids". */
export function humanField(key) {
  const k = String(key || '');
  const m = /conditions\[(\d+)\]\.?(\w*)/.exec(k);
  if (m) return `Condition ${Number(m[1]) + 1}${m[2] ? ` ${m[2]}` : ''}`;
  const last = k.split('.').pop() || k;
  const words = last.replace(/_/g, ' ');
  return words.charAt(0).toUpperCase() + words.slice(1);
}

/** Is this a version conflict (409 with the current playlist attached)? */
export function isVersionConflict(err) {
  return err?.status === 409 && !!err?.details?.current;
}

/* -------------------------------------------------------------- utilities */

/**
 * Split a user-entered list ("Rock; Pop, Jazz") into trimmed unique values. `;` always separates;
 * commas only when `commas` is true (artist names often contain commas).
 */
export function splitList(text, { commas = false } = {}) {
  const re = commas ? /[;,]/ : /;/;
  const out = [];
  for (const part of String(text || '').split(re)) {
    const v = part.trim();
    if (v && !out.includes(v)) out.push(v);
  }
  return out;
}

/**
 * Parse "3:30", "1:02:03" or "210" into seconds; null when invalid or empty.
 * @param {string} text
 */
export function parseClock(text) {
  const s = String(text ?? '').trim();
  if (!s) return null;
  if (/^\d+(\.\d+)?$/.test(s)) return Number(s);
  const m = /^(?:(\d+):)?(\d{1,2}):(\d{1,2})$/.exec(s);
  if (!m) return null;
  const h = Number(m[1] || 0);
  const min = Number(m[2]);
  const sec = Number(m[3]);
  if (sec >= 60 || (m[1] !== undefined && min >= 60)) return null;
  return h * 3600 + min * 60 + sec;
}

/** Split an array into chunks of at most n items. */
export function chunk(list, n) {
  const out = [];
  for (let i = 0; i < list.length; i += n) out.push(list.slice(i, i + n));
  return out;
}

/** A random seed for server-side random sorts. */
export function randomSeed(rng = Math.random) {
  return Math.floor(rng() * 0xffffffff).toString(36);
}

/** The play context of a list (POST /music/plays context). */
export function playContext(kind, id) {
  if (!['album', 'artist', 'playlist', 'genre', 'search', 'queue'].includes(kind)) return null;
  return id ? { kind, id: String(id) } : { kind };
}

/** Link target of a GET /music/home recently_played item. */
export function recentLink(item) {
  switch (item?.kind) {
    case 'album': return item.album?.id ? paths.album(item.album.id) : null;
    case 'artist': return item.artist?.id ? paths.artist(item.artist.id) : null;
    case 'playlist': return item.playlist?.id ? paths.playlist(item.playlist.id) : null;
    case 'track': return item.track?.album_id ? paths.album(item.track.album_id) : null;
    default: return null;
  }
}

/** Who added a playlist item: "you", the owner's name, "another user" or '' (deleted user). */
export function addedByLabel(item, pl, userId) {
  const by = item?.added_by || '';
  if (!by) return '';
  if (by === userId) return 'you';
  if (pl && by === pl.owner_id) return pl.owner_name || 'the owner';
  return 'another user';
}
