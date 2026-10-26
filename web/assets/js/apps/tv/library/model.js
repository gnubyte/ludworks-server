// TV library — pure helpers (no DOM): routes, list queries, labels, lineup items and
// form diffing. Unit tested under node (web/tests/media).

export const BASE = '/tv/library';
export const PAGE_SIZE = 60;

/** Library views reachable at /tv/library/<rest>. */
export const VIEWS = [
  { id: 'home', label: 'Home', icon: 'home', path: '' },
  { id: 'movies', label: 'Movies', icon: 'film', path: 'movies' },
  { id: 'shows', label: 'Shows', icon: 'tv', path: 'shows' },
  { id: 'genres', label: 'Genres', icon: 'tag', path: 'genres' },
  { id: 'recent', label: 'Recently added', icon: 'clock', path: 'recent' },
  { id: 'unwatched', label: 'Unwatched', icon: 'eye-off', path: 'unwatched' },
  { id: 'years', label: 'By year', icon: 'calendar', path: 'years' },
  { id: 'videos', label: 'Other videos', icon: 'play', path: 'videos' },
];

/** Browse presets: fixed list parameters per view (user filters are added on top). */
export const PRESETS = {
  movies: { title: 'Movies', fixed: { category: 'movie' }, sort: 'title', order: 'asc' },
  recent: { title: 'Recently added', fixed: {}, sort: 'added', order: 'desc' },
  unwatched: { title: 'Unwatched', fixed: { watched: 'false' }, sort: 'title', order: 'asc' },
  videos: { title: 'Other videos', fixed: { category: 'none' }, sort: 'added', order: 'desc' },
  search: { title: 'Search', fixed: {}, sort: 'title', order: 'asc' },
  browse: { title: 'Library', fixed: {}, sort: 'title', order: 'asc' },
};

export const SORTS = [
  { value: 'title', label: 'Title', order: 'asc' },
  { value: 'added', label: 'Date added', order: 'desc' },
  { value: 'year', label: 'Year', order: 'desc' },
  { value: 'duration', label: 'Duration', order: 'desc' },
  { value: 'random', label: 'Shuffle', order: 'asc' },
];

/** Filters a browse view accepts from the URL. */
export const FILTER_KEYS = ['q', 'genre', 'year', 'subcategory', 'watched', 'tag', 'space_id', 'category'];

/**
 * Parse the part of the URL after /tv/library.
 * @param {string} rest
 * @returns {{view: string, key?: string, id?: string}}
 */
export function parseRoute(rest) {
  const parts = String(rest || '').split('/').filter(Boolean).map((p) => {
    try { return decodeURIComponent(p); } catch { return p; }
  });
  const [a, b] = parts;
  if (!a) return { view: 'home' };
  if (a === 'shows' && b) return { view: 'show', key: b };
  if (a === 'item' && b) return { view: 'item', id: b };
  if (a === 'shows' || a === 'genres' || a === 'years') return { view: a };
  if (Object.hasOwn(PRESETS, a)) return { view: 'browse', preset: a };
  return { view: 'notfound' };
}

/** Link to a library view. */
export function libraryHref(path = '', query = null) {
  const p = path ? `${BASE}/${path}` : BASE;
  return query ? p + toQueryString(query) : p;
}

export const itemHref = (id) => `${BASE}/item/${encodeURIComponent(id)}`;
export const showHref = (key) => `${BASE}/shows/${encodeURIComponent(key)}`;

/** Player link (implemented by the TV app); `t` = start offset in seconds. */
export function playHref(id, t = null) {
  const base = `/tv/play/${encodeURIComponent(id)}`;
  return t === null || t === undefined ? base : `${base}?t=${Math.max(0, Math.floor(Number(t) || 0))}`;
}

/** "?a=1&b=x" from an object (empty values dropped, keys sorted for stable URLs). */
export function toQueryString(obj) {
  const qs = new URLSearchParams();
  for (const k of Object.keys(obj || {}).sort()) {
    const v = obj[k];
    if (v === undefined || v === null || v === '') continue;
    qs.set(k, String(v));
  }
  const s = qs.toString();
  return s ? `?${s}` : '';
}

/**
 * Browse state from the URL query for a preset.
 * @returns {{filters: Record<string,string>, sort: string, order: string, seed: string}}
 */
export function browseState(query, presetId) {
  const preset = (Object.hasOwn(PRESETS, presetId) && PRESETS[presetId]) || PRESETS.browse;
  const q = query || {};
  const filters = {};
  for (const k of FILTER_KEYS) {
    const v = q[k];
    if (typeof v === 'string' && v.trim() !== '' && !(k in preset.fixed)) filters[k] = v.trim();
  }
  if (filters.watched && !['true', 'false'].includes(filters.watched)) delete filters.watched;
  if (filters.year && !/^\d{1,4}$/.test(filters.year)) delete filters.year;
  const sort = SORTS.some((s) => s.value === q.sort) ? q.sort : preset.sort;
  const order = q.order === 'asc' || q.order === 'desc' ? q.order : (q.sort && q.sort !== preset.sort ? defaultOrder(sort) : preset.order);
  const seed = typeof q.seed === 'string' && /^[A-Za-z0-9]{1,32}$/.test(q.seed) ? q.seed : '';
  return { filters, sort, order, seed };
}

export function defaultOrder(sort) {
  return SORTS.find((s) => s.value === sort)?.order || 'asc';
}

/** GET /media query parameters for a browse state. */
export function listQuery(presetId, state, limit = PAGE_SIZE) {
  const preset = (Object.hasOwn(PRESETS, presetId) && PRESETS[presetId]) || PRESETS.browse;
  const out = { ...preset.fixed, ...state.filters, sort: state.sort, order: state.order, limit };
  if (state.sort === 'random') out.seed = state.seed || 'x';
  return out;
}

/** Count of active user filters (for the filter button badge). */
export function activeFilterCount(filters) {
  return Object.keys(filters || {}).filter((k) => k !== 'q').length;
}

/* ------------------------------------------------------------------ labels */

/** "S01E02", "S01", "E05" or "". */
export function episodeCode(season, episode) {
  const pad = (n) => String(n).padStart(2, '0');
  const s = Number.isInteger(season) ? `S${pad(season)}` : '';
  const e = Number.isInteger(episode) ? `E${pad(episode)}` : '';
  return s + e;
}

/** Resolution class from pixel dimensions ("4K", "1080p", "720p", "SD"…), or "". */
export function resolutionLabel(width, height) {
  const w = Number(width) || 0;
  const hgt = Number(height) || 0;
  if (!w || !hgt) return '';
  const longSide = Math.max(w, hgt);
  const shortSide = Math.min(w, hgt);
  if (longSide >= 3800 || shortSide >= 2000) return '4K';
  if (longSide >= 2500 || shortSide >= 1400) return '1440p';
  if (longSide >= 1900 || shortSide >= 1000) return '1080p';
  if (longSide >= 1200 || shortSide >= 700) return '720p';
  if (shortSide >= 480) return '480p';
  return 'SD';
}

/** Compact runtime: "1h 32m", "45m", "50s" ("" when unknown). */
export function runtime(seconds) {
  const s = Number(seconds);
  if (!Number.isFinite(s) || s <= 0) return '';
  const m = Math.round(s / 60);
  if (m < 1) return `${Math.round(s)}s`;
  const hh = Math.floor(m / 60);
  const mm = m % 60;
  if (!hh) return `${mm}m`;
  return mm ? `${hh}h ${mm}m` : `${hh}h`;
}

/** Remaining time label for a resume point: "23m left". */
export function remainingLabel(media) {
  const d = Number(media?.duration_seconds) || 0;
  const p = Number(media?.position_seconds) || 0;
  if (!d || !p || p >= d) return '';
  const r = runtime(d - p);
  return r ? `${r} left` : '';
}

/** Playback progress 0..1 (0 when unknown or watched). */
export function progressFraction(media) {
  if (!media || media.watched) return 0;
  const d = Number(media.duration_seconds) || 0;
  const p = Number(media.position_seconds) || 0;
  if (!d || p <= 0) return 0;
  return Math.min(1, Math.max(0, p / d));
}

/** True when the item has a resume point worth offering. */
export function canResume(media) {
  const f = progressFraction(media);
  return f > 0.01 && f < 0.97 && Number(media.position_seconds) >= 10;
}

/** Is this item an episode (has a show)? */
export const isEpisode = (m) => !!(m && m.show_title);

/** Primary display title. */
export function displayTitle(m) {
  if (!m) return '';
  return m.title || m.episode_title || '';
}

/** Secondary line for cards: "1968 · 1h 36m" or "Show · S01E02". */
export function cardSubtitle(m, { withShow = true } = {}) {
  if (!m) return '';
  if (isEpisode(m)) {
    const code = episodeCode(m.season, m.episode);
    return [withShow ? m.show_title : '', code].filter(Boolean).join(' · ');
  }
  return [m.year, runtime(m.duration_seconds)].filter(Boolean).join(' · ');
}

/** Add/replace ?size= on a relative API URL. */
export function sizedUrl(url, size) {
  if (!url) return '';
  const [path, q = ''] = String(url).split('?');
  const qs = new URLSearchParams(q);
  qs.set('size', size);
  return `${path}?${qs.toString()}`;
}

/** Thumbnail (video frame) URL of a file, or "" when the server has none. */
export function thumbUrl(entry, size = 'md') {
  return entry?.has_thumbnail ? `/api/v1/files/${encodeURIComponent(entry.id)}/thumbnail?size=${size}` : '';
}

/** Initials and hue for generated placeholder art. */
const MINOR_WORDS = new Set(['the', 'a', 'an', 'of', 'and', 'in', 'on', 'at', 'to', 'for', 'de', 'la', 'le', 'der', 'die', 'das', '&']);

export function placeholderArt(title) {
  const words = String(title || '?').split(/[\s._:()[\]-]+/).filter((w) => /[\p{L}\p{N}]/u.test(w));
  // Prefer significant words that start with a letter ("Night of the Living Dead" → NL,
  // "VID 20240101 123456" → VI).
  const significant = words.filter((w) => !MINOR_WORDS.has(w.toLowerCase()));
  const lettered = (significant.length ? significant : words).filter((w) => /^\p{L}/u.test(w));
  const pool = lettered.length ? lettered : (significant.length ? significant : words);
  let initials = '?';
  if (lettered.length >= 2) initials = [...pool[0]][0] + [...pool[1]][0];
  else if (pool.length) initials = [...pool[0]].slice(0, 2).join('');
  let hash = 0;
  for (const ch of String(title || '')) hash = (hash * 31 + ch.codePointAt(0)) >>> 0;
  return { initials: initials.toUpperCase(), hue: hash % 360, hue2: (hash >>> 9) % 360 };
}

/** Badges describing the technical side of an item. */
export function techBadges(m) {
  const out = [];
  const res = resolutionLabel(m?.width, m?.height);
  if (res) out.push(res);
  if (m?.video_codec) out.push(m.video_codec.toUpperCase());
  if (m?.audio_codec) out.push(m.audio_codec.toUpperCase());
  if (m?.container) out.push(m.container.toUpperCase());
  return out;
}

/** Human names for subtitle formats. */
export function subtitleFormat(f) {
  return { srt: 'SRT', vtt: 'WebVTT', ass: 'ASS', embedded: 'Embedded' }[f] || String(f || '').toUpperCase();
}

/* ----------------------------------------------------------------- editing */

/** Split a genre text input ("Drama, Sci-Fi") into normalized genres. */
export function parseGenres(text) {
  return normalizeGenres(String(text || '').split(/[,;\n]/));
}

/** Trim, collapse spaces and drop case-insensitive duplicates and empties. */
export function normalizeGenres(list) {
  const out = [];
  const seen = new Set();
  for (const g of list || []) {
    const v = String(g).replace(/\s+/g, ' ').trim().slice(0, 64);
    if (!v || seen.has(v.toLowerCase())) continue;
    seen.add(v.toLowerCase());
    out.push(v);
  }
  return out.slice(0, 50);
}

const EDIT_FIELDS = ['title', 'sort_title', 'year', 'description', 'genres', 'content_rating', 'show_title', 'season', 'episode',
  'episode_title', 'tmdb_id', 'imdb_id'];

/** Integer field from a form string: '' → null, invalid → NaN. */
export function intField(v) {
  const s = String(v ?? '').trim();
  if (s === '') return null;
  if (!/^\d+$/.test(s)) return Number.NaN;
  return Number(s);
}

/**
 * Validate an edit form; returns {fields: {name: message}} or {} when valid.
 * Mirrors the server limits so most mistakes are caught before saving.
 */
export function validateEdit(form) {
  const fields = {};
  if (!String(form.title || '').trim()) fields.title = 'Title is required.';
  const ranges = { year: [1800, 3000], season: [0, 10000], episode: [0, 100000], tmdb_id: [1, 2 ** 40] };
  for (const [k, [lo, hi]] of Object.entries(ranges)) {
    const v = form[k];
    if (v === null || v === undefined) continue;
    if (Number.isNaN(v) || v < lo || v > hi) fields[k] = `Enter a whole number between ${lo} and ${hi}.`;
  }
  if (form.imdb_id && !/^tt\d{5,10}$/.test(form.imdb_id)) fields.imdb_id = 'IMDb IDs look like tt0063350.';
  for (const [k, max] of Object.entries({ title: 500, sort_title: 500, show_title: 500, episode_title: 500, content_rating: 32, description: 20000 })) {
    if (String(form[k] || '').length > max) fields[k] = `At most ${max} characters.`;
  }
  return fields;
}

/** PATCH body with only the media fields that changed. */
export function diffMedia(original, form) {
  const out = {};
  for (const k of EDIT_FIELDS) {
    if (!(k in form)) continue;
    const a = original?.[k] ?? (k === 'genres' ? [] : (['year', 'season', 'episode', 'tmdb_id'].includes(k) ? null : ''));
    const b = form[k];
    if (JSON.stringify(a) !== JSON.stringify(b)) out[k] = b;
  }
  return out;
}

/* ------------------------------------------------------------------ shows */

/** First episode to play: in progress, else the first unwatched, else the first. */
export function nextEpisode(seasons) {
  const eps = (seasons || []).flatMap((s) => s.episodes || []);
  return eps.find((e) => canResume(e.media)) || eps.find((e) => !e.media?.watched) || eps[0] || null;
}

export function seasonLabel(n) {
  return n === 0 ? 'Specials' : `Season ${n}`;
}

/** Group year facet values by decade: [{decade: 1990, years: [{value, count}], count}]. */
export function decades(years) {
  const map = new Map();
  for (const y of years || []) {
    const v = Number(y.value);
    if (!Number.isInteger(v)) continue;
    const d = Math.floor(v / 10) * 10;
    if (!map.has(d)) map.set(d, { decade: d, years: [], count: 0 });
    const e = map.get(d);
    e.years.push(y);
    e.count += y.count;
  }
  return [...map.values()].sort((a, b) => b.decade - a.decade)
    .map((d) => ({ ...d, years: d.years.sort((a, b) => b.value - a.value) }));
}

/* ------------------------------------------------------- lineup items (TV) */

export function fileLineupItem(media) {
  const m = media.media || {};
  const label = isEpisode(m) ? `${m.show_title} ${episodeCode(m.season, m.episode)}`.trim() + (m.episode_title ? ` · ${m.episode_title}` : '') : displayTitle(m);
  return { kind: 'file', file_id: media.id, order: 'default', label: label || media.name };
}

export function showLineupItem(show, { order = 'default', limit = null } = {}) {
  return withLimit({ kind: 'show', show_title: show.show_title, order, label: show.show_title }, limit);
}

export function seasonLineupItem(showTitle, season, { order = 'default', limit = null } = {}) {
  return withLimit({ kind: 'season', show_title: showTitle, season, order, label: `${showTitle} · ${seasonLabel(season)}` }, limit);
}

export function folderLineupItem(spaceId, path, { recursive = true, order = 'default', limit = null, spaceName = '' } = {}) {
  const where = path ? path.split('/').pop() : (spaceName || 'Space root');
  return withLimit({ kind: 'folder', space_id: spaceId, path: path || '', recursive: !!recursive, order, label: `Folder · ${where}` }, limit);
}

export function queryLineupItem(query, { order = 'default', limit = null } = {}) {
  const q = {};
  for (const k of ['category', 'subcategory', 'genre', 'tag']) if (query?.[k]) q[k] = String(query[k]);
  if (query?.year !== undefined && query?.year !== null && query?.year !== '') q.year = Number(query.year);
  const parts = [q.genre, q.category && `category ${q.category}`, q.subcategory, q.tag && `#${q.tag}`, q.year].filter(Boolean);
  return withLimit({ kind: 'query', query: q, order, label: parts.length ? `Smart · ${parts.join(', ')}` : 'Smart · everything' }, limit);
}

/**
 * Lineup items for everything picked in the media picker. Selections made on different tabs
 * (movies & episodes, shows, seasons) all count, in that order; `extra` is the item built by
 * the active form tab (folder or smart query), appended last. `opts` (order/limit) apply to
 * the show and season items.
 * @param {{files?: Map<string, object>, show?: Map<string, object>, season?: Map<string, {show_title: string, season: number}>}} picked
 * @param {object|null} extra
 * @param {{order?: string, limit?: number|null}} [opts]
 */
export function collectPicks(picked, extra = null, opts = {}) {
  const out = [
    ...(picked.files ? picked.files.values() : []),
    ...[...(picked.show ? picked.show.values() : [])].map((s) => showLineupItem(s, opts)),
    ...[...(picked.season ? picked.season.values() : [])].map((s) => seasonLineupItem(s.show_title, s.season, opts)),
  ];
  if (extra) out.push(extra);
  return out;
}

function withLimit(item, limit) {
  const n = Number(limit);
  if (Number.isInteger(n) && n > 0) item.limit = n;
  return item;
}
