// Pure TV app logic (no DOM): routing, program labels, guide geometry, channel
// navigation, number entry, source selection, lineup item helpers. Unit tested under node.

/** Top-level TV tabs (route segment → label/icon). */
export const TABS = [
  { id: 'live', label: 'Live', icon: 'tv', href: '/tv' },
  { id: 'guide', label: 'Guide', icon: 'calendar', href: '/tv/guide' },
  { id: 'library', label: 'Library', icon: 'film', href: '/tv/library' },
  { id: 'lineups', label: 'Lineups', icon: 'list', href: '/tv/lineups' },
  { id: 'channels', label: 'Channels', icon: 'radio', href: '/tv/channels' },
  { id: 'integrations', label: 'Integrations', icon: 'link', href: '/tv/integrations' },
];

const ID_RE = /^[A-Za-z0-9_-]{8,64}$/;

/** True for strings shaped like server ids. */
export function isId(s) {
  return typeof s === 'string' && ID_RE.test(s);
}

/**
 * Parse the path after /tv into a view descriptor.
 * @param {string} rest e.g. '', 'guide', 'watch/<id>', 'lineups/<id>', 'library/show/x'
 * @returns {{view: string, id?: string, rest?: string, tab: string}}
 */
export function parseRoute(rest) {
  const parts = String(rest || '').split('/').filter(Boolean);
  const [head = '', second = ''] = parts;
  switch (head) {
    case '':
      return { view: 'live', tab: 'live' };
    case 'guide':
      return parts.length === 1 ? { view: 'guide', tab: 'guide' } : { view: 'notfound', tab: '' };
    case 'watch':
      if (parts.length === 1) return { view: 'watch', id: '', tab: '' };
      return parts.length === 2 && isId(second) ? { view: 'watch', id: second, tab: '' } : { view: 'notfound', tab: '' };
    case 'play':
      return parts.length === 2 && isId(second) ? { view: 'play', id: second, tab: '' } : { view: 'notfound', tab: '' };
    case 'lineups':
      if (parts.length === 1) return { view: 'lineups', tab: 'lineups' };
      return parts.length === 2 && isId(second) ? { view: 'lineup', id: second, tab: 'lineups' } : { view: 'notfound', tab: '' };
    case 'channels':
      return parts.length === 1 ? { view: 'channels', tab: 'channels' } : { view: 'notfound', tab: '' };
    case 'integrations':
      return parts.length === 1 ? { view: 'integrations', tab: 'integrations' } : { view: 'notfound', tab: '' };
    case 'library':
      return { view: 'library', rest: parts.slice(1).join('/'), tab: 'library' };
    default:
      return { view: 'notfound', tab: '' };
  }
}

/* ---------------------------------------------------------------- programs */

const pad2 = (n) => String(n).padStart(2, '0');

/** "S01E02" (or "E02" / "S01") for a program/media with season/episode; '' otherwise. */
export function episodeCode(p) {
  const s = Number.isInteger(p?.season) ? p.season : null;
  const e = Number.isInteger(p?.episode) ? p.episode : null;
  if (s !== null && e !== null) return `S${pad2(s)}E${pad2(e)}`;
  if (e !== null) return `E${pad2(e)}`;
  if (s !== null) return `S${pad2(s)}`;
  return '';
}

/** Main title of a program: the show for episodes, else the title. */
export function programTitle(p) {
  if (!p) return '';
  return p.show_title || p.title || 'Untitled';
}

/**
 * Whether an episode's title only repeats its show and episode number, as titles guessed from
 * file names do ("Garden Hour S01E02" from Garden.Hour.S01E02.mkv, "Show - 1x02").
 */
export function titleRepeatsShow(title, showTitle, season, episode) {
  const norm = (s) => String(s || '').toLowerCase().replace(/[^\p{L}\p{N}]+/gu, '');
  let rest = norm(title);
  const show = norm(showTitle);
  if (!rest || rest === show) return true;
  if (!show || !rest.startsWith(show)) return false;
  rest = rest.slice(show.length);
  const s = Number.isInteger(season) ? season : null;
  const e = Number.isInteger(episode) ? episode : null;
  if (e === null) return false;
  const forms = [`e${e}`, `e${pad2(e)}`, `episode${e}`, `ep${e}`, `ep${pad2(e)}`];
  if (s !== null) {
    for (const ss of [String(s), pad2(s)]) for (const ee of [String(e), pad2(e)]) forms.push(`s${ss}e${ee}`, `${ss}x${ee}`);
  }
  return forms.includes(rest);
}

/** Secondary line: "S01E02 · Episode title" for episodes, '' for movies. */
export function programSubtitle(p) {
  if (!p || !p.show_title) return '';
  const code = episodeCode(p);
  const ep = p.episode_title || (p.title && !titleRepeatsShow(p.title, p.show_title, p.season, p.episode) ? p.title : '');
  return [code, ep].filter(Boolean).join(' · ');
}

/** One-line label: "Show · S01E02 · Title" or "Movie title". */
export function programLabel(p) {
  const sub = programSubtitle(p);
  return sub ? `${programTitle(p)} · ${sub}` : programTitle(p);
}

/** A poster/thumbnail URL with its ?size= set (added or replaced); null stays null. */
export function sizedPoster(url, size = 'lg') {
  if (!url) return null;
  if (/[?&]size=/.test(url)) return url.replace(/([?&]size=)[^&#]*/, `$1${size}`);
  return `${url}${url.includes('?') ? '&' : '?'}size=${size}`;
}

/**
 * Image shown on the player stage behind a program: its poster, else the channel logo. Audio-
 * only programs (podcasts, radio dramas) never paint a video frame, so without it the stage
 * stayed black for the whole program.
 */
export function stagePoster(p, ch = null) {
  return sizedPoster(p?.poster_url, 'lg') || ch?.logo_url || null;
}

/** Fraction (0..1) of program p elapsed at nowMs. */
export function programProgress(p, nowMs) {
  if (!p) return 0;
  const s = Date.parse(p.starts_at);
  const e = Date.parse(p.ends_at);
  if (!Number.isFinite(s) || !Number.isFinite(e) || e <= s) return 0;
  return Math.min(1, Math.max(0, (nowMs - s) / (e - s)));
}

/** The program airing at nowMs and the one after it, from a time-ordered list. */
export function currentAndNext(programs, nowMs) {
  const list = programs || [];
  let i = list.findIndex((p) => Date.parse(p.starts_at) <= nowMs && nowMs < Date.parse(p.ends_at));
  if (i < 0) i = list.findIndex((p) => Date.parse(p.starts_at) > nowMs) - 1;
  if (i < 0 && list.length && Date.parse(list[0].starts_at) > nowMs) return { current: null, next: list[0] };
  return { current: i >= 0 ? list[i] : null, next: i >= 0 ? list[i + 1] || null : null };
}

/**
 * Where to seek right now in a /now response fetched at fetchedAt (ms, performance or
 * wall clock — the same clock as nowMs), clamped to the program's duration.
 */
export function liveOffset(nowResp, fetchedAt, nowMs) {
  const p = nowResp?.program;
  if (!p) return 0;
  const elapsed = Math.max(0, (nowMs - fetchedAt) / 1000);
  const dur = Number(p.duration_seconds) || 0;
  const off = (Number(nowResp.offset_seconds) || 0) + elapsed;
  return dur > 0 ? Math.min(off, Math.max(0, dur - 0.25)) : off;
}

/**
 * Pick the playback source for a program at offset seconds.
 * Direct stream when browser_playable (seek in the player) — unless forceTranscode — else the
 * transcode URL starting at the offset (the player starts at 0 = offset). Falls back to the
 * direct stream when there is no transcoder.
 * @returns {{url: string, kind: 'direct'|'transcode', base: number, seek: number}}
 */
export function chooseSource(p, offset = 0, { forceTranscode = false } = {}) {
  const off = Math.max(0, Number(offset) || 0);
  const wantTranscode = (forceTranscode || !p.browser_playable) && p.transcode_url;
  if (wantTranscode) {
    const sep = p.transcode_url.includes('?') ? '&' : '?';
    const start = Math.floor(off * 10) / 10;
    return { url: `${p.transcode_url}${sep}start=${start}`, kind: 'transcode', base: start, seek: 0 };
  }
  return { url: p.stream_url, kind: 'direct', base: 0, seek: off };
}

/* --------------------------------------------------------------- subtitles */

const LANG_RE = /^[a-z]{2,3}(-[A-Za-z]{2})?$/;
const SUB_URL_RE = /^\/api\/v1\/tv\/channels\/[A-Za-z0-9_-]+\/programs\/[A-Za-z0-9_-]+\/subtitles\/\d+\.vtt$/;

/**
 * <track> descriptors for a program's subtitles (Program.subtitles: channel-scoped WebVTT
 * URLs). Only same-origin program subtitle URLs are accepted.
 * @returns {{src: string, label: string, srclang?: string, key: string}[]}
 */
export function programTracks(p) {
  const subs = Array.isArray(p?.subtitles) ? p.subtitles : [];
  return subs.filter((s) => typeof s?.url === 'string' && SUB_URL_RE.test(s.url)).map((s, i) => {
    const lang = LANG_RE.test(s.language || '') ? s.language : undefined;
    const label = String(s.label || '').trim() || (s.language ? String(s.language) : `Track ${i + 1}`);
    return { src: s.url, label, srclang: lang, key: captionKey({ srclang: lang, label }) };
  });
}

/** The remembered identity of a caption choice: its language when known, else its label. */
export function captionKey(t) {
  if (!t) return '';
  return t.srclang ? `lang:${String(t.srclang).toLowerCase()}` : `label:${String(t.label || '').toLowerCase()}`;
}

/**
 * Index of the track to show for a remembered choice (captionKey, '' = off): the same
 * language, else the same label; -1 for none. Programs change under a live channel, so the
 * viewer's choice carries over by language rather than by track number.
 */
export function pickCaption(tracks, key) {
  if (!key || !Array.isArray(tracks)) return -1;
  const i = tracks.findIndex((t) => captionKey(t) === key);
  if (i >= 0 || !key.startsWith('lang:')) return i;
  // "en" also matches "en-GB" and vice versa.
  const base = key.slice(5).split('-')[0];
  return tracks.findIndex((t) => t.srclang && String(t.srclang).toLowerCase().split('-')[0] === base);
}

/**
 * Cue times for a source that starts base seconds into the file (a transcode from
 * ?start=base plays from 0): WebVTT times are file-relative, so they move back by base.
 */
export function shiftCue(start, end, base) {
  const b = Number(base) > 0 ? Number(base) : 0;
  return [Math.max(0, start - b), Math.max(0, end - b)];
}

/* ------------------------------------------------------------------- guide */

export const SLOT_MIN = 30;

/** Floor a time (ms) to the previous slot boundary (local half hours align with UTC ones). */
export function floorToSlot(ms, slotMin = SLOT_MIN) {
  const size = slotMin * 60000;
  return Math.floor(ms / size) * size;
}

/** Minutes of the past the guide always shows before "now". */
export const GUIDE_LEAD_MIN = 15;

/**
 * First slot of a guide window that should show nowMs. Flooring "now" itself put the now line
 * on the window's left edge right after every half hour (its NOW marker cut in half and the
 * programs on air clipped); starting a slot earlier keeps 15–45 minutes of context.
 */
export function guideStart(nowMs, slotMin = SLOT_MIN) {
  return floorToSlot(nowMs - GUIDE_LEAD_MIN * 60000, slotMin);
}

/** Slot boundaries (ms) covering [startMs, endMs). */
export function guideSlots(startMs, endMs, slotMin = SLOT_MIN) {
  const out = [];
  for (let t = floorToSlot(startMs, slotMin); t < endMs; t += slotMin * 60000) out.push(t);
  return out;
}

/**
 * Horizontal geometry of a program inside the guide window (pixels), clipped to the window.
 * @returns {{left: number, width: number, clippedStart: boolean, clippedEnd: boolean}|null}
 */
export function guideBox(p, windowStart, windowEnd, pxPerMin) {
  const s = Date.parse(p.starts_at);
  const e = Date.parse(p.ends_at);
  if (!(e > windowStart) || !(s < windowEnd)) return null;
  const cs = Math.max(s, windowStart);
  const ce = Math.min(e, windowEnd);
  return {
    left: ((cs - windowStart) / 60000) * pxPerMin,
    width: Math.max(1, ((ce - cs) / 60000) * pxPerMin),
    clippedStart: s < windowStart,
    clippedEnd: e > windowEnd,
  };
}

/**
 * Keyboard navigation in the guide grid. rows[r] is a time-ordered program list.
 * dir: 'left'|'right' move within the row; 'up'|'down' move to the adjacent row's program
 * airing at the focused program's (clamped) start time `atMs`.
 * @returns {{row: number, index: number}|null}
 */
export function guideNeighbor(rows, row, index, dir, atMs) {
  if (dir === 'left' || dir === 'right') {
    const j = index + (dir === 'left' ? -1 : 1);
    return j >= 0 && j < (rows[row] || []).length ? { row, index: j } : null;
  }
  const step = dir === 'up' ? -1 : 1;
  for (let r = row + step; r >= 0 && r < rows.length; r += step) {
    const list = rows[r] || [];
    if (!list.length) continue;
    let k = list.findIndex((p) => Date.parse(p.starts_at) <= atMs && atMs < Date.parse(p.ends_at));
    if (k < 0) k = Date.parse(list[0].starts_at) > atMs ? 0 : list.length - 1;
    return { row: r, index: k };
  }
  return null;
}

/* ---------------------------------------------------------------- channels */

/** Channels sorted by number then name (stable). */
export function sortChannels(channels) {
  return [...(channels || [])].sort((a, b) => a.number - b.number || String(a.name).localeCompare(String(b.name)));
}

/** The channel dir steps (+1 up / -1 down) away from currentId, wrapping around. */
export function channelStep(channels, currentId, dir) {
  const list = sortChannels(channels);
  if (!list.length) return null;
  const i = list.findIndex((c) => c.id === currentId);
  if (i < 0) return list[0];
  return list[(i + (dir > 0 ? 1 : -1) + list.length) % list.length];
}

/** Append a typed digit to the channel-number buffer (max 4 digits, no leading zeros). */
export function pushDigit(buffer, digit) {
  const d = String(digit);
  if (!/^\d$/.test(d)) return buffer;
  const next = (buffer + d).replace(/^0+/, '');
  return next.slice(-4);
}

/** The channel with number n (own channels win ties because the list is server-sorted). */
export function channelByNumber(channels, n) {
  const num = Number(n);
  return (channels || []).find((c) => c.number === num) || null;
}

/* ------------------------------------------------------------ lineup items */

export const ITEM_KINDS = {
  file: { icon: 'film', label: 'File' },
  show: { icon: 'tv', label: 'Show' },
  season: { icon: 'layers', label: 'Season' },
  folder: { icon: 'folder', label: 'Folder' },
  query: { icon: 'filter', label: 'Smart query' },
};

/** Human description of a lineup item: {icon, title, detail}. titles maps file ids → titles. */
export function describeItem(item, titles = {}) {
  const k = ITEM_KINDS[item?.kind] || { icon: 'layers', label: 'Item' };
  let title = item?.label || '';
  let detail = k.label;
  switch (item?.kind) {
    case 'file':
      title ||= titles[item.file_id] || 'File';
      break;
    case 'show':
      title ||= item.show_title || 'Show';
      detail = 'All episodes';
      break;
    case 'season':
      title ||= item.show_title || 'Show';
      detail = Number.isInteger(item.season) ? (item.season === 0 ? 'Specials' : `Season ${item.season}`) : 'Season';
      break;
    case 'folder':
      title ||= item.path ? item.path.split('/').pop() : 'Space root';
      detail = `${item.path ? `/${item.path}` : '/'}${item.recursive === false ? '' : ' · with subfolders'}`;
      break;
    case 'query': {
      const q = item.query || {};
      const bits = [q.category || 'movie,tv', q.subcategory, q.genre && `genre ${q.genre}`, q.tag && `#${q.tag}`, q.year && String(q.year)].filter(Boolean);
      title ||= q.genre ? `${q.genre}` : (q.category ? q.category.split(',').join(' & ') : 'Movies & TV');
      detail = `Query · ${bits.join(' · ')}`;
      break;
    }
    default:
  }
  return { icon: k.icon, title, detail };
}

const INPUT_KEYS = ['id', 'kind', 'file_id', 'show_title', 'season', 'space_id', 'path', 'recursive', 'query', 'order', 'limit', 'label'];

/** A lineup item reduced to the PUT body shape (keeps `id` so the server keeps identity). */
export function itemToInput(item) {
  const out = {};
  for (const k of INPUT_KEYS) {
    if (item[k] !== undefined && item[k] !== null && item[k] !== '') out[k] = item[k];
  }
  return out;
}

/** Move element `from` to index `to` (returns a new array). */
export function moveItem(arr, from, to) {
  const list = [...arr];
  if (from < 0 || from >= list.length) return list;
  const t = Math.max(0, Math.min(list.length - 1, to));
  const [x] = list.splice(from, 1);
  list.splice(t, 0, x);
  return list;
}

/**
 * Normalise what a media picker returned into lineup item inputs. Accepts lineup item
 * inputs ({kind, …}), Media/Entry objects ({id, …} → file), show summaries ({show_title,
 * show_key} → show) and {show_title, season} (→ season).
 */
export function normalizePicked(picked) {
  const out = [];
  for (const p of picked || []) {
    if (!p || typeof p !== 'object') continue;
    if (p.kind && ITEM_KINDS[p.kind]) {
      out.push(itemToInput({ ...p, id: undefined }));
    } else if (p.show_title && Number.isInteger(p.season) && !p.id) {
      out.push({ kind: 'season', show_title: p.show_title, season: p.season });
    } else if (p.show_title && (p.show_key || p.episodes !== undefined || p.seasons !== undefined) && !p.id) {
      out.push({ kind: 'show', show_title: p.show_title });
    } else if (isId(p.file_id) && !p.kind) {
      out.push({ kind: 'file', file_id: p.file_id });
    } else if (isId(p.id) && !p.is_dir) {
      out.push({ kind: 'file', file_id: p.id });
    }
  }
  return out;
}

/** Titles of picked media keyed by file id (for describing new file items). */
export function pickedTitles(picked) {
  const out = {};
  for (const p of picked || []) {
    const id = p?.file_id || p?.id;
    const t = p?.media?.title || p?.title || p?.name;
    if (isId(id) && t) out[id] = t;
  }
  return out;
}

/* --------------------------------------------------------------- on demand */

/** Where to resume a media item (seconds); 0 when barely started or nearly finished. */
export function resumeFrom(media) {
  const m = media?.media || media || {};
  const pos = Number(m.position_seconds) || 0;
  const dur = Number(m.duration_seconds) || 0;
  if (pos < 10) return 0;
  if (dur > 0 && pos > dur * 0.95) return 0;
  return pos;
}

/** True when position counts as watched to the end. */
export function isCompleted(position, duration) {
  return duration > 0 && position >= duration * 0.95;
}

/** The episode after `current` in a list of Media (sorted by season/episode here). */
export function nextEpisode(episodes, current) {
  const cm = current?.media || {};
  if (!cm.show_title) return null;
  const key = (m) => [Number.isInteger(m.season) ? m.season : 1e9, Number.isInteger(m.episode) ? m.episode : 1e9];
  const list = (episodes || []).filter((e) => e?.media && e.media.show_title === cm.show_title)
    .sort((a, b) => {
      const [as, ae] = key(a.media);
      const [bs, be] = key(b.media);
      return as - bs || ae - be || String(a.name || '').localeCompare(String(b.name || ''));
    });
  const i = list.findIndex((e) => e.id === current.id);
  return i >= 0 ? list[i + 1] || null : null;
}

/* ------------------------------------------------------------------ gestures */

/** Swipe direction of a pointer gesture, or null when too short / ambiguous. */
export function swipeDirection(dx, dy, min = 40) {
  const ax = Math.abs(dx);
  const ay = Math.abs(dy);
  if (Math.max(ax, ay) < min) return null;
  if (ay > ax * 1.4) return dy < 0 ? 'up' : 'down';
  if (ax > ay * 1.4) return dx < 0 ? 'left' : 'right';
  return null;
}

/** Clamp a volume value to 0..1 with 2-decimal precision. */
export function clampVolume(v) {
  const n = Number(v);
  if (!Number.isFinite(n)) return 1;
  return Math.round(Math.min(1, Math.max(0, n)) * 100) / 100;
}
