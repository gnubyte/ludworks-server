// Casting client (API.md §14) — the one module every player uses to send playback to Chromecast
// and AirPlay devices. There is no vendor SDK anywhere: the server discovers and controls the
// devices and hands them short-lived media URLs; this module lists devices, starts sessions,
// follows them with a long poll, and offers the browser's own casting (Safari's AirPlay picker,
// the Remote Playback API) as extra entries where the browser has them.
//
// API reference
// =============
//   import { castButton, castControls, startSession, attachSession, findSession } from '../../core/cast.js';
//
// castButton(opts) → HTMLButtonElement (+ .setCasting(device|null), .destroy())
//   A toolbar button that opens the device picker: devices from GET /cast/devices with kind icons,
//   availability, "In use" / "Paired" / "Needs pairing" badges, a refresh (POST /cast/devices/refresh),
//   the Apple TV PIN pairing flow, the "casting is turned off" state (with a settings link for admins),
//   empty/error states, and the browser's own options when it has them. Choosing a device starts a
//   session (422 pairing_required → PIN pairing, then one retry) and calls opts.onSession(controller).
//   opts:
//     getItems()     → {items: [{kind: 'media'|'track'|'channel', id}], index?, positionSeconds?,
//                       repeat?, shuffle?, volume?} | null — what to cast; read when a device is chosen
//     onSession(controller, device) — a session started here: pause local playback and show
//                       castControls(controller); stop the previous controller if you had one
//                       (the user moved the cast to another device)
//     beforeStart?() — awaited before the session starts (e.g. save the local position first)
//     media?()       → HTMLMediaElement|null — the local player, for the browser's own AirPlay /
//                       Remote Playback entries (shown only when the browser supports them)
//     api?, ui?      — the core/api.js client and core/ui.js module (default: the app-wide ones)
//     className?, variant? ('ghost'), size? ('sm'|'md'|'lg'), title?
//   setCasting(device|null) marks the button as active ("Casting to <device>").
//
// startSession(api, {deviceId, items, index?, positionSeconds?, repeat?, shuffle?, volume?})
//   → Promise<CastController>. POST /cast/sessions (connects to the device first, so it can take a few
//   seconds). Rejects with the ApiError (castErrorMessage(err) phrases it) or a TypeError/RangeError
//   for invalid input (see buildStartRequest).
// attachSession(api, session) → CastController — follow an existing CastSession (e.g. one found
//   with findSession after a reload).
// listSessions(api) → Promise<CastSession[]> — the caller's live sessions.
// findSession(api, match(session, item) → bool, {timeoutMs?}) → Promise<CastSession|null> — the
//   first live session whose current CastItem matches; null on any error (never redirects).
//
// CastController — one per consumer; while attached it long-polls GET /cast/sessions/{id}?wait_version=
//   .id, .session (latest CastSession), .device (CastDevice), .ended, .attached
//   .item()      — current CastItem or null
//   .position()  — seconds into the current item, interpolated locally while playing
//   .duration()  — seconds or null (for channels: the current program's length)
//   .canSeek(), .hasNext(), .hasPrevious()
//   .on('change', fn(session)) · .on('ended', fn({session, reason, byUser})) · .on('error', fn(err)) → off()
//       'ended' fires once when the session is over: reason is its error code (replaced, idle,
//       access_revoked, cast_disabled, device_unavailable, interrupted, …) or 'stopped'; byUser is
//       true when this controller's stop() ended it.
//   .play() .pause() .toggle() .seek(seconds) .next() .previous() .jump(index)
//   .setVolume(0..1) .setMuted(bool) .setRepeat('off'|'all'|'one') .setShuffle(bool)
//   .replaceItems(items, {index?, positionSeconds?}) .insertItems(items, position?) .refresh()
//       Each returns Promise<CastSession>; the session updates from the response at once (pause, play,
//       seek, volume, mute, repeat and shuffle also optimistically), identical concurrent calls are
//       shared, and failures reject with the ApiError (also emitted as 'error').
//   .stop() → Promise<{index, item, positionSeconds}> — ends the session (DELETE) and resolves with
//       where playback stopped, so the caller can resume locally.
//   .detach() — stop polling and drop listeners WITHOUT ending the session (call it on unmount).
//   Every controller detaches itself when the signed-in user changes (sign-out); detachAll() does
//   the same on demand.
//
// castControls(controller, {variant?: 'card'|'overlay'|'bar', onStop?(where), ui?, className?})
//   → {el, update(), destroy()} — compact "Casting to <device>" UI: art, title/subtitle, seek bar
//   (disabled for channels and unknown durations), times, previous / play-pause / next, mute + volume,
//   "Stop casting" (calls controller.stop(), then onStop(where)), state badge and error banner.
//
// Pure helpers (unit tested): reduceSession, interpolatePosition, buildStartRequest, normalizeItems,
// buildControl, currentItem, sessionDuration, canSeek, hasNext, hasPrevious, deviceLabel,
// deviceDetail, deviceBadges, deviceStatus, sortDevices, kindLabel, kindIcon, supportsItems,
// stateLabel, errorText, castErrorMessage, nativeCastOptions, castIcon.

import { h, s, cls } from './dom.js';
import { icon as coreIcon } from './icons.js';
import { ensureStyle } from './style.js';
import { formatDuration } from './format.js';
import { store, isAdmin } from './store.js';
import { api as appApi, isAbortError } from './api.js';
import * as appUi from './ui.js';

const CSS_HREF = '/assets/css/lib/cast.css';

/** Item kinds a session can play (API.md §14 CastItem.kind). */
export const ITEM_KINDS = Object.freeze(['media', 'track', 'channel']);
/** Most items one session queue holds. */
export const MAX_ITEMS = 1000;
/** Repeat modes. */
export const REPEAT_MODES = Object.freeze(['off', 'all', 'one']);
/** Actions of POST /cast/sessions/{id}/control. */
export const CONTROL_ACTIONS = Object.freeze(['play', 'pause', 'toggle', 'stop', 'seek', 'next', 'previous', 'jump', 'volume', 'mute', 'repeat', 'shuffle']);

const POLL_TIMEOUT_S = 20; // server-side long-poll window (1–30 s)
const POLL_SLACK_MS = 15000; // client timeout on top of it
const START_TIMEOUT_MS = 60000; // POST /cast/sessions connects to the device first
const REQUEST_TIMEOUT_MS = 30000;
const BACKOFF_MS = [1000, 2000, 5000, 10000, 15000];
const ID_RE = /^[A-Za-z0-9_-]{8,64}$/;
const VIDEO_KINDS = new Set(['media', 'channel']);

/* --------------------------------------------------------------------- icons */

// Player icons missing from the core set (same 24px grid, 1.75px round strokes, currentColor).
const EXTRA_ICONS = {
  volume: ['M4 9.5h3.5L12 5.5v13l-4.5-4H4z', 'M15.5 9a4 4 0 0 1 0 6', 'M18.2 6.5a7.5 7.5 0 0 1 0 11'],
  'volume-x': ['M4 9.5h3.5L12 5.5v13l-4.5-4H4z', 'M16 9.5l5 5', 'M21 9.5l-5 5'],
  repeat: ['M17 2.5l3.5 3.5L17 9.5', 'M3.5 11.5V10a4 4 0 0 1 4-4h13', 'M7 21.5L3.5 18 7 14.5', 'M20.5 12.5V14a4 4 0 0 1-4 4h-13'],
  'repeat-one': ['M17 2.5l3.5 3.5L17 9.5', 'M3.5 11.5V10a4 4 0 0 1 4-4h13', 'M7 21.5L3.5 18 7 14.5', 'M20.5 12.5V14a4 4 0 0 1-4 4h-13', 'M11 10.5l1.5-1v5'],
  stop: ['M7.5 6h9A1.5 1.5 0 0 1 18 7.5v9a1.5 1.5 0 0 1-1.5 1.5h-9A1.5 1.5 0 0 1 6 16.5v-9A1.5 1.5 0 0 1 7.5 6z'],
};

/**
 * Icon by name: the few casting-specific paths first, then the core icon set.
 * @param {string} name
 * @param {number|{size?: number, className?: string, label?: string}} [opts]
 * @returns {SVGSVGElement}
 */
export function castIcon(name, opts = {}) {
  const parts = EXTRA_ICONS[name];
  if (!parts) return coreIcon(name, opts);
  const { size = 16, className = '', label = '' } = typeof opts === 'number' ? { size: opts } : opts;
  const svg = s('svg', {
    viewBox: '0 0 24 24', width: size, height: size, fill: 'none', stroke: 'currentColor',
    'stroke-width': 1.75, 'stroke-linecap': 'round', 'stroke-linejoin': 'round', class: ['icon', className], focusable: 'false',
  });
  if (label) {
    svg.setAttribute('role', 'img');
    svg.setAttribute('aria-label', label);
    svg.appendChild(s('title', { text: label }));
  } else {
    svg.setAttribute('aria-hidden', 'true');
  }
  for (const d of parts) svg.appendChild(s('path', { d }));
  return svg;
}

/* -------------------------------------------------------------- pure helpers */

const finite = (v) => typeof v === 'number' && Number.isFinite(v);

/** The current CastItem of a session, or null. */
export function currentItem(session) {
  const items = Array.isArray(session?.items) ? session.items : [];
  const i = session?.index;
  return Number.isInteger(i) && i >= 0 && i < items.length ? items[i] || null : null;
}

/** Duration of the session's current item in seconds, or null when unknown. */
export function sessionDuration(session) {
  const d = Number(session?.duration_seconds);
  if (Number.isFinite(d) && d > 0) return d;
  const it = Number(currentItem(session)?.duration_seconds);
  return Number.isFinite(it) && it > 0 ? it : null;
}

/**
 * Fold a received CastSession into the local snapshot {session, receivedAt}. Answers for an older
 * version of the same session are ignored (a slow request must not undo a newer state); with
 * `strict` the same version is ignored too (while optimistic updates are in flight). A session that
 * is over never comes back to life. Another session id replaces the snapshot.
 * @param {{session: object, receivedAt: number}|null} prev
 * @param {object|null} next CastSession
 * @param {number} receivedAt ms timestamp of the answer (same clock as interpolatePosition's now)
 * @param {{strict?: boolean}} [opts]
 * @returns {{session: object, receivedAt: number}|null} prev itself when nothing changed
 */
export function reduceSession(prev, next, receivedAt, { strict = false } = {}) {
  if (!next || typeof next !== 'object' || typeof next.id !== 'string') return prev || null;
  if (prev?.session && prev.session.id === next.id) {
    const pv = Number(prev.session.version) || 0;
    const nv = Number(next.version) || 0;
    if (nv < pv || (strict && nv === pv)) return prev;
    if (prev.session.state === 'stopped' && next.state !== 'stopped') return prev;
  }
  return { session: next, receivedAt: finite(receivedAt) ? receivedAt : Date.now() };
}

/**
 * Position in seconds at nowMs: the reported position_seconds (true at receivedAt), advanced by the
 * elapsed time while the session is playing, clamped to [0, duration].
 * @param {{session: object, receivedAt: number}|null} snapshot
 * @param {number} nowMs
 */
export function interpolatePosition(snapshot, nowMs) {
  const sess = snapshot?.session;
  if (!sess) return 0;
  let pos = Number(sess.position_seconds);
  if (!Number.isFinite(pos) || pos < 0) pos = 0;
  if (sess.state === 'playing' && finite(snapshot.receivedAt) && finite(nowMs) && nowMs > snapshot.receivedAt) {
    pos += (nowMs - snapshot.receivedAt) / 1000;
  }
  const dur = sessionDuration(sess);
  return dur ? Math.min(pos, dur) : pos;
}

/**
 * Validate and normalise a queue for the API: 1–1000 {kind, id} with known kinds and API ids.
 * @param {Array<{kind: string, id: string}>} items
 * @returns {Array<{kind: string, id: string}>}
 */
export function normalizeItems(items) {
  if (!Array.isArray(items) || items.length === 0) throw new TypeError('Nothing to cast: add at least one item.');
  if (items.length > MAX_ITEMS) throw new RangeError(`Too many items to cast (at most ${MAX_ITEMS}).`);
  return items.map((it, i) => {
    if (!ITEM_KINDS.includes(it?.kind)) throw new TypeError(`items[${i}].kind must be media, track or channel.`);
    if (typeof it.id !== 'string' || !ID_RE.test(it.id)) throw new TypeError(`items[${i}].id is not a valid id.`);
    return { kind: it.kind, id: it.id };
  });
}

function optIndex(index, length) {
  if (index === undefined || index === null) return undefined;
  if (!Number.isInteger(index) || index < 0 || index >= length) throw new RangeError('index must point into items.');
  return index;
}

function optSeconds(v, what) {
  if (v === undefined || v === null) return undefined;
  const n = Number(v);
  if (typeof v === 'boolean' || !Number.isFinite(n) || n < 0) throw new RangeError(`${what} must be a number of seconds ≥ 0.`);
  return Math.round(n * 1000) / 1000;
}

/**
 * Body of POST /cast/sessions. Optional fields left undefined/null are omitted.
 * @param {{deviceId: string, items: Array<{kind: string, id: string}>, index?: number, positionSeconds?: number,
 *          repeat?: 'off'|'all'|'one', shuffle?: boolean, volume?: number}} o
 * @returns {object}
 */
export function buildStartRequest(o = {}) {
  const { deviceId, items, index, positionSeconds, repeat, shuffle, volume } = o;
  if (typeof deviceId !== 'string' || !deviceId || deviceId.length > 200 || /[\s/?#]/.test(deviceId)) {
    throw new TypeError('deviceId must be a device id.');
  }
  const body = { device_id: deviceId, items: normalizeItems(items) };
  const i = optIndex(index, body.items.length);
  if (i !== undefined) body.index = i;
  const pos = optSeconds(positionSeconds, 'positionSeconds');
  if (pos !== undefined) body.position_seconds = pos;
  if (repeat !== undefined && repeat !== null) {
    if (!REPEAT_MODES.includes(repeat)) throw new TypeError('repeat must be off, all or one.');
    body.repeat = repeat;
  }
  if (shuffle !== undefined && shuffle !== null) {
    if (typeof shuffle !== 'boolean') throw new TypeError('shuffle must be true or false.');
    body.shuffle = shuffle;
  }
  if (volume !== undefined && volume !== null) {
    const v = Number(volume);
    if (typeof volume === 'boolean' || !Number.isFinite(v) || v < 0 || v > 1) throw new RangeError('volume must be between 0 and 1.');
    body.volume = v;
  }
  return body;
}

/**
 * Body of POST /cast/sessions/{id}/control for an action (see CONTROL_ACTIONS).
 * @param {string} action
 * @param {{positionSeconds?: number, index?: number, volume?: number, muted?: boolean, repeat?: string, shuffle?: boolean}} [args]
 * @returns {object}
 */
export function buildControl(action, args = {}) {
  switch (action) {
    case 'play': case 'pause': case 'toggle': case 'stop': case 'next': case 'previous':
      return { action };
    case 'seek': {
      const p = optSeconds(args.positionSeconds ?? args.position_seconds, 'seek position');
      if (p === undefined) throw new RangeError('seek needs a position in seconds.');
      return { action, position_seconds: p };
    }
    case 'jump':
      if (!Number.isInteger(args.index) || args.index < 0) throw new RangeError('jump needs an item index ≥ 0.');
      return { action, index: args.index };
    case 'volume': {
      const v = Number(args.volume);
      if (typeof args.volume !== 'number' || !Number.isFinite(v) || v < 0 || v > 1) throw new RangeError('volume must be between 0 and 1.');
      return { action, volume: Math.round(v * 100) / 100 };
    }
    case 'mute':
      if (typeof args.muted !== 'boolean') throw new TypeError('mute needs muted: true or false.');
      return { action, muted: args.muted };
    case 'repeat':
      if (!REPEAT_MODES.includes(args.repeat)) throw new TypeError('repeat must be off, all or one.');
      return { action, repeat: args.repeat };
    case 'shuffle':
      if (typeof args.shuffle !== 'boolean') throw new TypeError('shuffle needs true or false.');
      return { action, shuffle: args.shuffle };
    default:
      throw new TypeError(`Unknown cast action "${action}".`);
  }
}

const ACTIVE_STATES = new Set(['loading', 'buffering', 'playing', 'paused']);

/** Whether the current item can be seeked: not a channel, known duration, something loaded. */
export function canSeek(session) {
  const it = currentItem(session);
  return !!it && it.kind !== 'channel' && !!sessionDuration(session) && ACTIVE_STATES.has(session.state);
}

const isOver = (session) => !session || session.state === 'stopped';

/** Whether "next" can do something (the server has the final say, e.g. a shuffled order). */
export function hasNext(session) {
  const n = Array.isArray(session?.items) ? session.items.length : 0;
  if (isOver(session) || n === 0) return false;
  if (session.repeat === 'all' || session.repeat === 'one') return true;
  if (session.shuffle) return n > 1;
  return Number.isInteger(session.index) && session.index < n - 1;
}

/** Whether "previous" can do something: an earlier item, wrapping, or restarting the current one. */
export function hasPrevious(session) {
  const n = Array.isArray(session?.items) ? session.items.length : 0;
  if (isOver(session) || n === 0) return false;
  if (Number.isInteger(session.index) && session.index > 0) return true;
  if (session.repeat === 'all' || (session.shuffle && n > 1)) return true;
  return canSeek(session);
}

/** "Chromecast" | "AirPlay". */
export function kindLabel(kind) {
  if (kind === 'chromecast') return 'Chromecast';
  if (kind === 'airplay') return 'AirPlay';
  return 'Cast device';
}

/** Icon name for a device kind. */
export function kindIcon(kind) {
  return kind === 'airplay' ? 'airplay' : 'cast';
}

/** Display name of a CastDevice. */
export function deviceLabel(d) {
  return String(d?.name || '').trim() || String(d?.host || '').trim() || 'Unnamed device';
}

/** Secondary line of a CastDevice: "Chromecast · Chromecast Ultra · 192.168.1.20". */
export function deviceDetail(d) {
  const parts = [kindLabel(d?.kind)];
  const model = String(d?.model || '').trim();
  if (model && model.toLowerCase() !== kindLabel(d?.kind).toLowerCase()) parts.push(model);
  const host = String(d?.host || '').trim();
  if (host && host !== deviceLabel(d)) parts.push(host);
  if (d?.source === 'manual') parts.push('added manually');
  return parts.join(' · ');
}

/**
 * Whether a device can play these items according to its advertised capabilities (video items —
 * media and channels — need "video"); unknown capabilities leave the decision to the server.
 * @param {object} device CastDevice
 * @param {Array<{kind: string}>|null} items
 */
export function supportsItems(device, items) {
  const caps = Array.isArray(device?.capabilities) ? device.capabilities : null;
  if (!caps || !Array.isArray(items) || !items.length) return true;
  if (items.some((it) => VIDEO_KINDS.has(it?.kind)) && !caps.includes('video')) return false;
  if (items.some((it) => it?.kind === 'track') && !caps.includes('audio') && !caps.includes('video')) return false;
  return true;
}

/**
 * Badges for a device row (most important first), each {key, label, tone} where tone is a
 * core badge variant.
 * @param {object} d CastDevice
 * @param {Array<{kind: string}>|null} [items] what would be cast (marks unsuitable devices)
 */
export function deviceBadges(d, items = null) {
  if (!d) return [];
  const out = [];
  if (d.session_id) out.push({ key: 'yours', label: 'Casting', tone: 'accent' });
  else if (d.busy) out.push({ key: 'busy', label: 'In use', tone: 'warning' });
  if (!d.available) out.push({ key: 'unavailable', label: 'Unavailable', tone: 'neutral' });
  else if (items && !supportsItems(d, items)) out.push({ key: 'unsupported', label: d.capabilities?.includes('video') ? 'Video only' : 'Audio only', tone: 'neutral' });
  if (d.requires_pairing) out.push(d.paired ? { key: 'paired', label: 'Paired', tone: 'success' } : { key: 'pair', label: 'Needs pairing', tone: 'info' });
  return out;
}

/** The most important badge of a device, or a neutral "Ready" one. */
export function deviceStatus(d, items = null) {
  if (!d) return { key: 'unknown', label: '', tone: 'neutral' };
  const b = deviceBadges(d, items);
  const order = ['unavailable', 'unsupported', 'yours', 'busy', 'pair', 'paired'];
  for (const key of order) {
    const hit = b.find((x) => x.key === key);
    if (hit) return hit;
  }
  return { key: 'ready', label: 'Ready', tone: 'success' };
}

/** Devices for the picker: available first, then by name. Does not modify the input. */
export function sortDevices(devices) {
  return [...(Array.isArray(devices) ? devices : [])].sort((a, b) =>
    Number(!!b.available) - Number(!!a.available) || deviceLabel(a).localeCompare(deviceLabel(b), undefined, { sensitivity: 'base' }));
}

const STATE_LABELS = {
  connecting: 'Connecting…', loading: 'Loading…', buffering: 'Buffering…', playing: 'Playing', paused: 'Paused',
  idle: 'Finished', stopped: 'Stopped', error: 'Problem',
};

/** Short label of a session state ("Paused", "Buffering…"). */
export function stateLabel(session) {
  return STATE_LABELS[session?.state] || '';
}

const ERROR_TEXT = {
  replaced: 'Someone started casting something else to this device.',
  idle: 'Casting ended after 30 minutes without activity.',
  access_revoked: 'Casting ended because the account no longer has access.',
  cast_disabled: 'Casting is turned off on this server. An administrator can enable it in Admin → Settings.',
  device_unavailable: 'The device could not be reached. Check that it is on and on the same network as the server.',
  playback_failed: 'The device could not play this item.',
  unsupported_media: 'This device can’t play this kind of media (for example video on a speaker).',
  pairing_required: 'This device has to be paired first.',
  pairing_unsupported: 'This device only allows AirPlay with a password or for members of its Home (its “Allow speaker access” setting), which Ludworks does not support. Set it to allow everyone on the same network.',
  interrupted: 'Playback was stopped or taken over on the device itself.',
  cast_unavailable: 'The server has no address the device can load media from. An administrator must set a cast media port, a media base URL or the public URL (Admin → Settings → Media & TV).',
  session_limit: 'You are already casting to 3 devices. Stop one of them first.',
  stopped: 'Casting stopped.',
  network_error: 'Could not reach the server. Check your connection.',
  timeout: 'The server took too long to respond.',
  rate_limited: 'Too many requests. Try again shortly.',
};

/**
 * A friendly sentence for a casting error code (session error.code, ended reason or API error
 * code), else fallback, else a generic message.
 * @param {string} code
 * @param {string} [fallback]
 */
export function errorText(code, fallback = '') {
  return ERROR_TEXT[code] || String(fallback || '').trim() || 'Casting failed.';
}

/** A friendly message for an error thrown by this module (ApiError, TypeError, string). */
export function castErrorMessage(err) {
  if (!err) return errorText('');
  if (typeof err === 'string') return err;
  if (err.code === 'validation_failed') {
    const fields = err.details?.fields;
    const msgs = fields && typeof fields === 'object' ? Object.values(fields).filter(Boolean) : [];
    return msgs.length ? msgs.join(' ') : err.message || errorText('');
  }
  if (err.code === 'rate_limited' && err.retryAfter) return `${ERROR_TEXT.rate_limited} Try again in ${err.retryAfter} s.`;
  if (ERROR_TEXT[err.code]) return ERROR_TEXT[err.code];
  return err.message || errorText('');
}

/**
 * The browser's own casting options for a media element (feature detection only):
 * Safari's AirPlay picker (webkitShowPlaybackTargetPicker) and the Remote Playback API.
 * @param {HTMLMediaElement|null} media
 * @param {object} [win] window (tests)
 * @returns {Array<{id: string, icon: string, label: string, hint: string, run: () => any}>}
 */
export function nativeCastOptions(media, win = typeof window !== 'undefined' ? window : undefined) {
  const out = [];
  if (!media || typeof media !== 'object') return out;
  const webkitAirPlay = !!win && typeof win.WebKitPlaybackTargetAvailabilityEvent !== 'undefined'
    && typeof media.webkitShowPlaybackTargetPicker === 'function';
  if (webkitAirPlay) {
    out.push({
      id: 'webkit-airplay', icon: 'airplay', label: 'This browser’s AirPlay',
      hint: 'Safari plays this player on an Apple TV or AirPlay speaker itself.',
      run: () => media.webkitShowPlaybackTargetPicker(),
    });
  }
  // Safari also implements the Remote Playback API (for the same AirPlay picker): offer it once.
  if (!webkitAirPlay && media.remote && typeof media.remote.prompt === 'function' && !media.disableRemotePlayback) {
    out.push({
      id: 'remote-playback', icon: 'cast', label: 'Cast this tab’s player',
      hint: 'Uses the browser’s own casting for this player.',
      run: () => media.remote.prompt(),
    });
  }
  return out;
}

function nativeErrorText(err) {
  switch (err?.name) {
    case 'NotAllowedError': case 'AbortError': return ''; // the user closed the browser's picker
    case 'NotFoundError': return 'The browser found no devices to cast to.';
    case 'NotSupportedError': return 'The browser can’t cast this player.';
    case 'InvalidStateError': return 'The browser is already casting this player.';
    default: return err?.message || 'The browser could not cast this player.';
  }
}

/* ---------------------------------------------------------------- controllers */

const live = new Set();
let storeOff = null;

function track(c) {
  live.add(c);
  // Sign-out (or another account in this tab): nothing keeps polling for the previous user.
  storeOff ??= store.select((st) => st.user?.id || null, () => detachAll());
}

/** Detach every live controller (sign-out). */
export function detachAll() {
  for (const c of [...live]) c.detach();
}

function sleep(ms, signal) {
  return new Promise((resolve) => {
    const t = setTimeout(resolve, ms);
    signal?.addEventListener('abort', () => { clearTimeout(t); resolve(); }, { once: true });
  });
}

function optimisticPatch(body, sess) {
  switch (body.action) {
    case 'pause': return sess.state === 'playing' || sess.state === 'buffering' ? { state: 'paused' } : null;
    case 'play': return sess.state === 'paused' ? { state: 'playing' } : null;
    case 'toggle':
      if (sess.state === 'playing' || sess.state === 'buffering') return { state: 'paused' };
      return sess.state === 'paused' ? { state: 'playing' } : null;
    case 'seek': return { position_seconds: body.position_seconds };
    case 'volume': return { volume: body.volume };
    case 'mute': return { muted: body.muted };
    case 'repeat': return { repeat: body.repeat };
    case 'shuffle': return { shuffle: body.shuffle };
    default: return null;
  }
}

function endedError() {
  const e = new Error(ERROR_TEXT.stopped);
  e.code = 'stopped';
  return e;
}

/**
 * Follow and control an existing CastSession.
 * @param {any} api core/api.js client
 * @param {object} session CastSession
 * @param {{now?: () => number}} [opts] clock (tests)
 * @returns {object} CastController (see the API reference at the top)
 */
export function attachSession(api, session, opts = {}) {
  if (!session || typeof session.id !== 'string') throw new TypeError('attachSession needs a CastSession.');
  const client = api || appApi;
  const now = opts.now || (() => Date.now());
  const listeners = { change: new Set(), ended: new Set(), error: new Set() };
  const base = `/cast/sessions/${encodeURIComponent(session.id)}`;
  const inflight = new Map();
  let snap = reduceSession(null, session, now());
  let attached = true;
  let ended = false;
  let pending = 0; // control requests in flight
  let pollGen = 0;
  let pollAbort = null;

  const emit = (type, payload) => {
    for (const fn of [...listeners[type]]) {
      try {
        fn(payload);
      } catch (err) {
        console.error(err);
      }
    }
  };

  function finish(reason, byUser) {
    if (ended) return;
    ended = true;
    stopPolling();
    live.delete(ctrl);
    emit('ended', { session: snap.session, reason, byUser });
  }

  function apply(next, { strict = pending > 0 } = {}) {
    const prev = snap;
    snap = reduceSession(prev, next, now(), { strict });
    if (snap === prev) return false;
    emit('change', snap.session);
    if (snap.session.state === 'stopped') finish(snap.session.error?.code || 'stopped', false);
    return true;
  }

  function applyLocal(patch) {
    const sess = snap.session;
    snap = { session: { ...sess, position_seconds: interpolatePosition(snap, now()), ...patch }, receivedAt: now() };
    emit('change', snap.session);
  }

  /** The session vanished (404) or this caller lost access: it is over for us. */
  function markGone(reason) {
    snap = { session: { ...snap.session, position_seconds: interpolatePosition(snap, now()), state: 'stopped' }, receivedAt: now() };
    emit('change', snap.session);
    finish(reason, false);
  }

  function stopPolling() {
    pollGen += 1;
    pollAbort?.abort();
    pollAbort = null;
  }

  async function poll(gen) {
    let failures = 0;
    while (gen === pollGen && attached && !ended) {
      const ctl = new AbortController();
      pollAbort = ctl;
      const version = Number(snap.session.version) || 0;
      const started = Date.now();
      try {
        const next = await client.get(base, {
          query: { wait_version: version, timeout: POLL_TIMEOUT_S },
          signal: ctl.signal, timeout: POLL_TIMEOUT_S * 1000 + POLL_SLACK_MS,
        });
        if (gen !== pollGen) return;
        failures = 0;
        apply(next);
        // An unchanged answer that came back at once (a proxy cutting long polls short, say)
        // must not turn this loop into a busy one.
        if (Number(next?.version) === version && Date.now() - started < 1000) await sleep(1000, ctl.signal);
      } catch (err) {
        if (gen !== pollGen || ctl.signal.aborted || isAbortError(err)) return;
        if (err?.status === 401) { // signed out: the app redirects to the login page
          detach();
          return;
        }
        if (err?.status === 404 || err?.status === 403) {
          markGone(err.status === 404 ? 'stopped' : 'access_revoked');
          return;
        }
        failures += 1;
        await sleep(BACKOFF_MS[Math.min(failures, BACKOFF_MS.length) - 1], ctl.signal);
      }
    }
  }

  function startPolling() {
    stopPolling();
    if (!attached || ended) return;
    poll(pollGen);
  }

  function send(method, path, body, patch) {
    if (ended) return Promise.reject(endedError());
    const key = `${method} ${path} ${JSON.stringify(body ?? null)}`;
    const shared = inflight.get(key);
    if (shared) return shared;
    if (patch) applyLocal(patch);
    pending += 1;
    const p = client.request(method, path, { body, timeout: REQUEST_TIMEOUT_MS })
      .then((next) => {
        apply(next, { strict: false });
        return snap.session;
      })
      .catch((err) => {
        if (patch) refresh().catch(() => {}); // undo the optimistic change with the real state
        emit('error', err);
        throw err;
      })
      .finally(() => {
        pending -= 1;
        inflight.delete(key);
      });
    inflight.set(key, p);
    return p;
  }

  function control(action, args) {
    let body;
    try {
      body = buildControl(action, args);
    } catch (err) {
      return Promise.reject(err);
    }
    return send('POST', `${base}/control`, body, ended ? null : optimisticPatch(body, snap.session));
  }

  function refresh() {
    if (ended) return Promise.resolve(snap.session);
    return client.get(base, { timeout: REQUEST_TIMEOUT_MS }).then((next) => {
      apply(next, { strict: false });
      return snap.session;
    }, (err) => {
      if (err?.status === 404) markGone('stopped');
      throw err;
    });
  }

  function where() {
    return { index: Number.isInteger(snap.session.index) ? snap.session.index : 0, item: currentItem(snap.session), positionSeconds: interpolatePosition(snap, now()) };
  }

  async function stop() {
    if (ended) return where();
    const w = where();
    stopPolling();
    try {
      await client.del(base, { timeout: REQUEST_TIMEOUT_MS });
    } catch (err) {
      if (err?.status !== 404) {
        if (attached) startPolling();
        emit('error', err);
        throw err;
      }
    }
    snap = { session: { ...snap.session, state: 'stopped', position_seconds: w.positionSeconds }, receivedAt: now() };
    emit('change', snap.session);
    finish('stopped', true);
    return w;
  }

  function detach() {
    if (!attached) return;
    attached = false;
    stopPolling();
    live.delete(ctrl);
    for (const set of Object.values(listeners)) set.clear();
  }

  const ctrl = {
    get id() { return session.id; },
    get session() { return snap.session; },
    get device() { return snap.session.device || null; },
    get ended() { return ended; },
    get attached() { return attached; },
    item: () => currentItem(snap.session),
    position: () => interpolatePosition(snap, now()),
    duration: () => sessionDuration(snap.session),
    canSeek: () => canSeek(snap.session),
    hasNext: () => hasNext(snap.session),
    hasPrevious: () => hasPrevious(snap.session),
    /** Subscribe to 'change' | 'ended' | 'error'; returns the unsubscribe function. */
    on(type, fn) {
      const set = listeners[type];
      if (!set) throw new TypeError(`Unknown cast controller event "${type}".`);
      if (!attached) return () => {};
      set.add(fn);
      return () => set.delete(fn);
    },
    play: () => control('play'),
    pause: () => control('pause'),
    toggle: () => control('toggle'),
    seek: (seconds) => control('seek', { positionSeconds: Math.max(0, Number(seconds) || 0) }),
    next: () => control('next'),
    previous: () => control('previous'),
    jump: (index) => control('jump', { index }),
    setVolume: (v) => control('volume', { volume: Math.min(1, Math.max(0, Number(v) || 0)) }),
    setMuted: (muted) => control('mute', { muted: !!muted }),
    setRepeat: (mode) => control('repeat', { repeat: mode }),
    setShuffle: (on) => control('shuffle', { shuffle: !!on }),
    replaceItems(items, { index, positionSeconds } = {}) {
      let body;
      try {
        body = { items: normalizeItems(items) };
        const i = optIndex(index, body.items.length);
        if (i !== undefined) body.index = i;
        const pos = optSeconds(positionSeconds, 'positionSeconds');
        if (pos !== undefined) body.position_seconds = pos;
      } catch (err) {
        return Promise.reject(err);
      }
      return send('PUT', `${base}/items`, body, null);
    },
    insertItems(items, position) {
      let body;
      try {
        body = { items: normalizeItems(items) };
        if (position !== undefined && position !== null) {
          if (!Number.isInteger(position) || position < 0) throw new RangeError('position must be an index ≥ 0.');
          body.position = position;
        }
      } catch (err) {
        return Promise.reject(err);
      }
      return send('POST', `${base}/items`, body, null);
    },
    refresh,
    stop,
    detach,
  };

  if (snap.session.state === 'stopped') {
    ended = true;
  } else {
    track(ctrl);
    startPolling();
  }
  return ctrl;
}

/**
 * Start casting: POST /cast/sessions, then follow the new session.
 * @param {any} api core/api.js client
 * @param {{deviceId: string, items: Array<{kind: string, id: string}>, index?: number, positionSeconds?: number,
 *          repeat?: string, shuffle?: boolean, volume?: number}} opts
 * @returns {Promise<object>} CastController
 */
export async function startSession(api, opts) {
  const client = api || appApi;
  const body = buildStartRequest(opts);
  const session = await client.post('/cast/sessions', body, { timeout: START_TIMEOUT_MS });
  return attachSession(client, session);
}

/** The caller's live sessions (GET /cast/sessions). */
export async function listSessions(api) {
  const list = await (api || appApi).get('/cast/sessions', { timeout: REQUEST_TIMEOUT_MS });
  return Array.isArray(list) ? list : [];
}

/**
 * The first live session whose current item matches, or null (also on any error — casting may
 * be off or the credential may lack the cast scope; never redirects to the login page).
 * @param {any} api
 * @param {(session: object, item: object|null) => boolean} match
 * @param {{timeoutMs?: number}} [opts]
 */
export async function findSession(api, match, { timeoutMs = 2500 } = {}) {
  try {
    const list = await (api || appApi).get('/cast/sessions', { timeout: timeoutMs, noRedirect: true });
    return (Array.isArray(list) ? list : []).find((sess) => sess && sess.state !== 'stopped' && match(sess, currentItem(sess))) || null;
  } catch {
    return null;
  }
}

/* --------------------------------------------------------------- device picker */

const HINTS = [
  'Make sure the device is switched on and on the same network as the server (not just as this computer).',
  'Running the server in Docker? Discovering devices needs host networking.',
  'An administrator can add a device by its address in Admin → Settings → Media & TV.',
];

/** Small spinner: announced with a label, decorative (next to visible text) without. */
function smallSpinner(label = '') {
  return label
    ? h('span', { class: 'spinner spinner-sm', role: 'status', aria: { label } })
    : h('span', { class: 'spinner spinner-sm', aria: { hidden: 'true' } });
}

function openPicker(o) {
  const { api, ui } = o;
  const state = {
    loading: true, refreshing: false, autoRefreshed: false, enabled: true, devices: [], loadError: null,
    message: null, // {variant, text}
    connecting: null, // device id
    pair: null, // {device, step: 'starting'|'pin'|'confirming'|'failed', pairingId, error, wrongPin, thenStart}
  };
  let closed = false;
  let focused = false;
  const body = h('div', { class: 'cast-picker' });
  const m = ui.modal({
    title: 'Cast to a device', size: 'sm', className: 'cast-picker-modal', body,
    onClose: () => {
      closed = true;
      o.onClose?.();
    },
  });

  const request = () => {
    try {
      const r = o.getItems?.();
      return r && Array.isArray(r.items) && r.items.length ? r : null;
    } catch (err) {
      console.error(err);
      return null;
    }
  };

  function render() {
    if (closed) return;
    body.replaceChildren(state.pair ? pairView(state.pair) : listView());
    if (!focused && !state.pair && !state.loading) {
      const first = [...body.querySelectorAll('.cast-device')].find((b) => !b.disabled);
      if (first) {
        focused = true;
        // After the dialog's own initial focus (also scheduled for the next frame).
        const go = () => { if (first.isConnected) first.focus({ preventScroll: true }); };
        if (typeof requestAnimationFrame === 'function') requestAnimationFrame(() => requestAnimationFrame(go));
        else go();
      }
    }
  }

  /* ---- device list */
  function listView() {
    const req = request();
    const parts = [];
    if (state.enabled) {
      const busy = state.loading || state.refreshing;
      const refresh = h('button', {
        type: 'button', class: 'btn btn-ghost btn-sm cast-refresh', disabled: busy || !!state.connecting,
        aria: { label: 'Search for devices again' },
      }, state.refreshing ? smallSpinner() : castIcon('refresh', 14), h('span', { class: 'btn-label' }, state.refreshing ? 'Searching…' : 'Refresh'));
      refresh.addEventListener('click', () => load(true));
      parts.push(h('div', { class: 'cast-toolbar' }, h('span', { class: 'cast-toolbar-title' }, 'Devices on your network'), refresh));
    }
    if (state.message) parts.push(ui.alert({ variant: state.message.variant, message: state.message.text }));
    if (!req && !state.loading && state.enabled) {
      parts.push(ui.alert({ variant: 'info', message: 'Nothing to cast right now — start playing something first.' }));
    }
    if (state.loading) {
      parts.push(h('div', { class: 'cast-loading' }, smallSpinner('Loading'), h('span', {}, 'Looking for devices…')));
    } else if (state.loadError) {
      parts.push(h('div', { class: 'cast-state' }, ui.errorState(state.loadError, () => load(false))));
    } else if (!state.enabled) {
      parts.push(disabledView());
    } else if (!state.devices.length) {
      parts.push(h('div', { class: 'cast-state' }, ui.emptyState({
        icon: 'cast', compact: true, title: state.refreshing ? 'Searching…' : 'No devices found',
        message: h('ul', { class: 'cast-hints' }, HINTS.map((t) => h('li', {}, t))),
      })));
    } else {
      parts.push(deviceList(req ? req.items : null));
    }
    const natives = nativeCastOptions(o.media?.() || null);
    if (natives.length) parts.push(nativeSection(natives));
    return h('div', { class: 'cast-picker-body' }, parts);
  }

  function disabledView() {
    const admin = isAdmin(store.get().user);
    return h('div', { class: 'cast-state' }, ui.emptyState({
      icon: 'cast', compact: true, title: 'Casting is turned off',
      message: 'An administrator can enable it in Admin → Settings.',
      action: admin ? ui.button({ label: 'Open settings', icon: 'settings', size: 'sm', href: '/admin/media', onClick: () => m.close() }) : null,
    }));
  }

  function deviceList(items) {
    const list = h('ul', { class: 'cast-devices', aria: { label: 'Cast devices' } });
    for (const d of sortDevices(state.devices)) list.append(deviceRow(d, items));
    list.addEventListener('keydown', (e) => {
      if (e.key !== 'ArrowDown' && e.key !== 'ArrowUp') return;
      const btns = [...list.querySelectorAll('.cast-device')].filter((b) => !b.disabled);
      if (!btns.length) return;
      e.preventDefault();
      const i = btns.indexOf(document.activeElement);
      const j = i < 0 ? 0 : (i + (e.key === 'ArrowDown' ? 1 : -1) + btns.length) % btns.length;
      btns[j].focus();
    });
    return list;
  }

  function deviceRow(d, items) {
    const connecting = state.connecting === d.id;
    const unusable = !items || !d.available || !supportsItems(d, items);
    const main = h('button', {
      type: 'button',
      class: cls('cast-device', { 'is-unavailable': !d.available, 'is-connecting': connecting, 'is-busy': !!d.busy }),
      disabled: unusable || !!state.connecting,
      aria: { busy: connecting ? 'true' : undefined },
      dataset: { device: d.id },
    },
    h('span', { class: 'cast-device-icon' }, castIcon(kindIcon(d.kind), 20)),
    h('span', { class: 'cast-device-text' },
      h('span', { class: 'cast-device-name truncate' }, deviceLabel(d)),
      h('span', { class: 'cast-device-detail truncate' }, connecting ? 'Connecting…' : deviceDetail(d))),
    h('span', { class: 'cast-device-badges' }, connecting
      ? smallSpinner()
      : deviceBadges(d, items).map((b) => ui.badge(b.label, b.tone))));
    main.addEventListener('click', () => choose(d));
    let pairBtn = null;
    if (d.requires_pairing && !d.paired && d.available) {
      pairBtn = h('button', { type: 'button', class: 'btn btn-secondary btn-sm cast-pair-btn', disabled: !!state.connecting }, 'Pair');
      pairBtn.addEventListener('click', () => startPairing(d));
    }
    return h('li', { class: 'cast-device-row' }, main, pairBtn);
  }

  function nativeSection(options) {
    return h('div', { class: 'cast-native' },
      h('div', { class: 'cast-native-title' }, 'Browser options'),
      h('ul', { class: 'cast-devices' }, options.map((n) => {
        const b = h('button', { type: 'button', class: 'cast-device cast-device-native' },
          h('span', { class: 'cast-device-icon' }, castIcon(n.icon, 20)),
          h('span', { class: 'cast-device-text' },
            h('span', { class: 'cast-device-name' }, n.label),
            h('span', { class: 'cast-device-detail' }, n.hint)));
        b.addEventListener('click', () => runNative(n));
        return h('li', { class: 'cast-device-row' }, b);
      })));
  }

  function runNative(n) {
    let r;
    try {
      r = n.run(); // must run inside the click (user gesture)
    } catch (err) {
      const t = nativeErrorText(err);
      if (t) ui.toast(t, { type: 'warning' });
      return;
    }
    m.close();
    if (r && typeof r.then === 'function') {
      r.catch((err) => {
        const t = nativeErrorText(err);
        if (t) ui.toast(t, { type: 'warning' });
      });
    }
  }

  async function load(refresh) {
    if (refresh) state.refreshing = true;
    else if (!state.devices.length) state.loading = true;
    state.loadError = null;
    render();
    try {
      const r = refresh
        ? await api.post('/cast/devices/refresh', {}, { timeout: REQUEST_TIMEOUT_MS })
        : await api.get('/cast/devices', { timeout: REQUEST_TIMEOUT_MS });
      state.enabled = r?.enabled !== false;
      state.devices = Array.isArray(r?.devices) ? r.devices : [];
    } catch (err) {
      if (err?.code === 'cast_disabled') state.enabled = false;
      else state.loadError = err;
    } finally {
      state.loading = false;
      state.refreshing = false;
    }
    if (closed) return;
    render();
    // Nothing known yet (e.g. right after casting was enabled): browse once straight away.
    if (!refresh && !state.autoRefreshed && state.enabled && !state.loadError && !state.devices.length) {
      state.autoRefreshed = true;
      load(true);
    }
  }

  /* ---- starting a session */
  async function choose(d, { afterPairing = false } = {}) {
    if (state.connecting) return;
    if (d.busy && !afterPairing) {
      const ok = await ui.confirm({
        title: `Cast to ${deviceLabel(d)}?`,
        message: d.session_id
          ? 'You are already casting to this device. Casting here replaces what plays on it.'
          : 'Someone is already casting to this device. Casting here stops what they are playing.',
        confirmLabel: 'Cast here',
      });
      if (!ok) return;
    }
    state.connecting = d.id;
    state.message = null;
    render();
    try {
      await o.beforeStart?.();
    } catch (err) {
      console.error(err);
    }
    const req = request();
    if (!req) { // the list says so already
      state.connecting = null;
      render();
      return;
    }
    let controller;
    try {
      controller = await startSession(api, { ...req, deviceId: d.id });
    } catch (err) {
      state.connecting = null;
      if (err?.code === 'pairing_required' && !afterPairing && !closed) {
        startPairing(d, { thenStart: true });
        return;
      }
      if (err?.code === 'cast_disabled') state.enabled = false;
      if (closed) ui.toast.error(castErrorMessage(err));
      state.message = { variant: 'danger', text: castErrorMessage(err) };
      render();
      return;
    }
    state.connecting = null;
    if (!closed) m.close();
    ui.toast.success(`Casting to ${deviceLabel(d)}`, { timeout: 3000 });
    o.onSession?.(controller, d);
  }

  /* ---- PIN pairing (Apple TV) */
  function startPairing(d, { thenStart = false } = {}) {
    const p = { device: d, step: 'starting', pairingId: null, error: null, wrongPin: false, thenStart };
    state.pair = p;
    state.message = null;
    render();
    api.post(`/cast/devices/${encodeURIComponent(d.id)}/pair`, {}, { timeout: REQUEST_TIMEOUT_MS }).then((r) => {
      if (state.pair !== p) return;
      p.pairingId = r?.pairing_id || null;
      p.step = 'pin';
      render();
      body.querySelector('.cast-pin')?.focus();
    }, (err) => {
      if (state.pair !== p) return;
      p.step = 'failed';
      p.error = err;
      render();
    });
  }

  async function confirmPin(p, pin) {
    p.step = 'confirming';
    render();
    try {
      await api.post(`/cast/devices/${encodeURIComponent(p.device.id)}/pair/confirm`, { pairing_id: p.pairingId, pin }, { timeout: REQUEST_TIMEOUT_MS });
    } catch (err) {
      if (state.pair !== p) return;
      p.step = 'failed';
      p.error = err;
      p.wrongPin = err?.code === 'validation_failed' && !!err?.details?.fields?.pin;
      render();
      return;
    }
    if (state.pair !== p) return;
    const d = state.devices.find((x) => x.id === p.device.id) || p.device;
    d.paired = true;
    ui.toast.success(`Paired with ${deviceLabel(d)}`, { timeout: 3000 });
    state.pair = null;
    render();
    if (p.thenStart) choose(d, { afterPairing: true });
  }

  function backToList() {
    state.pair = null;
    render();
  }

  function pairView(p) {
    const d = p.device;
    const head = h('div', { class: 'cast-pair-head' },
      h('span', { class: 'cast-device-icon' }, castIcon(kindIcon(d.kind), 20)),
      h('div', { class: 'cast-device-text' },
        h('span', { class: 'cast-device-name' }, `Pair with ${deviceLabel(d)}`),
        h('span', { class: 'cast-device-detail' }, 'A one-time PIN pairing lets this server play video on the TV.')));
    const back = ui.button({ label: 'Back', variant: 'ghost', onClick: backToList });
    if (p.step === 'starting') {
      return h('div', { class: 'cast-pair' }, head,
        h('div', { class: 'cast-loading' }, smallSpinner('Starting pairing'), h('span', {}, 'Asking the TV to show a PIN…')),
        h('div', { class: 'cast-pair-actions' }, back));
    }
    if (p.step === 'failed') {
      const unsupported = p.error?.code === 'pairing_unsupported';
      const text = p.wrongPin
        ? 'That PIN didn’t match. Start again to get a new PIN on the TV.'
        : unsupported ? errorText('pairing_unsupported') : castErrorMessage(p.error);
      return h('div', { class: 'cast-pair' }, head,
        ui.alert({ variant: p.wrongPin ? 'warning' : 'danger', message: text }),
        h('div', { class: 'cast-pair-actions' }, back,
          unsupported ? null : ui.button({ label: 'Try again', icon: 'refresh', variant: 'primary', onClick: () => startPairing(d, { thenStart: p.thenStart }) })));
    }
    // 'pin' | 'confirming'
    const busy = p.step === 'confirming';
    const pinInput = h('input', {
      class: 'input cast-pin nums', type: 'text', inputmode: 'numeric', autocomplete: 'one-time-code', maxlength: '4',
      pattern: '[0-9]{4}', required: true, spellcheck: 'false', disabled: busy, id: `cast-pin-${d.id}`,
      aria: { describedby: `cast-pin-hint-${d.id}` },
    });
    pinInput.addEventListener('input', () => {
      const digits = pinInput.value.replace(/\D+/g, '').slice(0, 4);
      if (digits !== pinInput.value) pinInput.value = digits;
    });
    const submit = h('button', { type: 'submit', class: cls('btn btn-primary', { 'is-loading': busy }), disabled: busy }, h('span', { class: 'btn-label' }, 'Pair'));
    const form = h('form', { class: 'cast-pair-form', novalidate: true },
      h('label', { class: 'field-label', for: `cast-pin-${d.id}` }, 'PIN shown on the TV'),
      pinInput,
      h('div', { class: 'field-hint', id: `cast-pin-hint-${d.id}` }, 'Enter the 4 digits your TV shows right now.'),
      h('div', { class: 'cast-pair-actions' }, back, submit));
    form.addEventListener('submit', (e) => {
      e.preventDefault();
      const pin = pinInput.value.trim();
      if (!/^\d{4}$/.test(pin)) {
        pinInput.setAttribute('aria-invalid', 'true');
        pinInput.focus();
        return;
      }
      confirmPin(p, pin);
    });
    return h('div', { class: 'cast-pair' }, head, form);
  }

  render();
  load(false);
  return m;
}

/**
 * The cast toolbar button (opens the device picker). See the API reference at the top.
 * @param {{getItems: () => any, onSession?: (controller: object, device: object) => void, beforeStart?: () => any,
 *          media?: () => HTMLMediaElement|null, api?: any, ui?: any, className?: string, variant?: string,
 *          size?: 'sm'|'md'|'lg', title?: string}} opts
 * @returns {HTMLButtonElement}
 */
export function castButton(opts = {}) {
  ensureStyle(CSS_HREF);
  const api = opts.api || appApi;
  const ui = opts.ui || appUi;
  const title = opts.title || 'Cast to a device';
  const size = opts.size || 'md';
  const btn = h('button', {
    type: 'button',
    class: cls('btn', `btn-${opts.variant || 'ghost'}`, 'btn-icon', size !== 'md' && `btn-${size}`, 'cast-button', opts.className),
    aria: { label: title, haspopup: 'dialog' },
    dataset: { tip: title },
  }, castIcon('cast', size === 'sm' ? 14 : size === 'lg' ? 20 : 18));
  let picker = null;
  btn.addEventListener('click', async () => {
    if (picker?.isOpen) return;
    // The picker is a dialog on <body>, which a fullscreen player stage would hide.
    if (typeof document !== 'undefined' && document.fullscreenElement) {
      try {
        await document.exitFullscreen();
      } catch { /* not in fullscreen after all */ }
    }
    picker = openPicker({
      api, ui, getItems: opts.getItems, media: opts.media, beforeStart: opts.beforeStart, onSession: opts.onSession,
      onClose: () => { picker = null; },
    });
  });
  /** Mark the button as casting to device (null = not casting). */
  btn.setCasting = (device) => {
    const casting = !!device;
    btn.classList.toggle('is-casting', casting);
    btn.classList.toggle('is-on', casting);
    const t = casting ? `Casting to ${deviceLabel(device)}` : title;
    btn.setAttribute('aria-label', t);
    btn.dataset.tip = t;
  };
  btn.destroy = () => {
    picker?.close();
    picker = null;
  };
  return btn;
}

/* ------------------------------------------------------------ control strip */

const ART_ICON = { media: 'film', channel: 'tv', track: 'music' };

/**
 * Compact "Casting to <device>" controls for a CastController. See the API reference at the top.
 * @param {object} controller CastController
 * @param {{variant?: 'card'|'overlay'|'bar', onStop?: (where: object) => void, ui?: any, className?: string}} [opts]
 * @returns {{el: HTMLElement, update: () => void, destroy: () => void}}
 */
export function castControls(controller, opts = {}) {
  ensureStyle(CSS_HREF);
  const ui = opts.ui || appUi;
  const variant = opts.variant || 'card';
  let seeking = false;
  let volumeDrag = false;
  let volTimer = 0;
  let noteTimer = 0;
  let note = '';
  let shownArt;
  let brokenArt = null; // an art URL that failed to load is not retried on every update

  const ctlButton = (ic, label, onClick, className = '') => {
    const b = h('button', { type: 'button', class: cls('btn btn-ghost btn-icon cast-ctl-btn', className), aria: { label }, dataset: { tip: label } }, castIcon(ic, 18));
    b.addEventListener('click', onClick);
    return b;
  };
  const setIcon = (btn, ic, label, size = 18) => {
    btn.replaceChildren(castIcon(ic, size));
    btn.setAttribute('aria-label', label);
    btn.dataset.tip = label;
  };

  const kindEl = h('span', { class: 'cast-ctl-kind', aria: { hidden: 'true' } });
  const deviceEl = h('strong', { class: 'cast-ctl-device' });
  const stateEl = h('span', { class: 'cast-ctl-state', aria: { live: 'polite' } });
  const artImg = h('img', { alt: '', decoding: 'async', draggable: 'false' });
  const art = h('span', { class: 'cast-ctl-art', aria: { hidden: 'true' } });
  artImg.addEventListener('error', () => {
    brokenArt = shownArt;
    shownArt = null;
    render();
  });
  const titleEl = h('div', { class: 'cast-ctl-title truncate' });
  const subEl = h('div', { class: 'cast-ctl-sub truncate' });
  const seek = h('input', { type: 'range', class: 'cast-ctl-seek', min: '0', max: '1000', step: '1', value: '0', aria: { label: 'Seek' } });
  const timeEl = h('span', { class: 'cast-ctl-time nums' });
  const prevBtn = ctlButton('prev', 'Previous', () => act(() => controller.previous()));
  const playBtn = ctlButton('play', 'Play', () => act(() => controller.toggle()), 'cast-ctl-play');
  const nextBtn = ctlButton('next', 'Next', () => act(() => controller.next()));
  const muteBtn = ctlButton('volume', 'Mute', () => act(() => controller.setMuted(!controller.session.muted)));
  const volume = h('input', { type: 'range', class: 'cast-ctl-volume', min: '0', max: '1', step: '0.05', aria: { label: 'Device volume' } });
  const stopBtn = h('button', { type: 'button', class: 'btn btn-secondary btn-sm cast-ctl-stop' }, castIcon('stop', 14), h('span', { class: 'btn-label' }, 'Stop casting'));
  const banner = h('div', { class: 'cast-ctl-banner', role: 'alert', hidden: true });

  const el = h('section', { class: cls('cast-controls', `cast-controls-${variant}`, opts.className), aria: { label: 'Cast controls' } },
    h('div', { class: 'cast-ctl-head' }, kindEl, h('span', { class: 'cast-ctl-to truncate' }, 'Casting to ', deviceEl), stateEl),
    h('div', { class: 'cast-ctl-main' }, art, h('div', { class: 'cast-ctl-meta' }, titleEl, subEl)),
    h('div', { class: 'cast-ctl-progress' }, seek, timeEl),
    h('div', { class: 'cast-ctl-buttons' },
      h('div', { class: 'cast-ctl-group' }, prevBtn, playBtn, nextBtn),
      h('div', { class: 'cast-ctl-group cast-ctl-vol' }, muteBtn, volume),
      stopBtn),
    banner);

  function showNote(text) {
    note = text;
    clearTimeout(noteTimer);
    noteTimer = setTimeout(() => { note = ''; render(); }, 6000);
    render();
  }

  function act(fn) {
    let p;
    try {
      p = fn();
    } catch (err) {
      showNote(castErrorMessage(err));
      return;
    }
    p?.catch?.((err) => showNote(castErrorMessage(err)));
  }

  function tick() {
    const sess = controller.session;
    const dur = controller.duration();
    const pos = controller.position();
    if (!seeking) {
      const f = dur ? Math.min(1, pos / dur) : 0;
      seek.value = String(Math.round(f * 1000));
      seek.style.setProperty('--fill', `${f * 100}%`);
      timeEl.textContent = dur ? `${formatDuration(pos)} / ${formatDuration(dur)}` : (currentItem(sess)?.kind === 'channel' ? 'Live' : formatDuration(pos));
    }
  }

  function render() {
    const sess = controller.session;
    const dev = sess.device || {};
    const item = currentItem(sess);
    const over = sess.state === 'stopped';
    kindEl.replaceChildren(castIcon(kindIcon(dev.kind), 14));
    deviceEl.textContent = deviceLabel(dev);
    stateEl.textContent = stateLabel(sess);
    stateEl.className = cls('cast-ctl-state', `is-${sess.state || 'idle'}`);
    titleEl.textContent = item?.title || (sess.state === 'idle' ? 'Nothing playing' : '');
    titleEl.title = titleEl.textContent;
    subEl.textContent = item?.subtitle || '';
    subEl.hidden = !item?.subtitle;
    const artUrl = item?.art_url && item.art_url !== brokenArt ? item.art_url : null;
    const artKey = artUrl || `icon:${item?.kind || ''}`;
    if (artKey !== shownArt) {
      shownArt = artKey;
      if (artUrl) {
        artImg.src = artUrl;
        art.replaceChildren(artImg);
      } else {
        art.replaceChildren(castIcon(ART_ICON[item?.kind] || 'cast', 22));
      }
    }
    const playing = sess.state === 'playing' || sess.state === 'buffering' || sess.state === 'loading';
    setIcon(playBtn, playing ? 'pause' : 'play', playing ? 'Pause' : 'Play', 20);
    playBtn.disabled = over || sess.state === 'connecting' || !item;
    prevBtn.disabled = over || !hasPrevious(sess);
    nextBtn.disabled = over || !hasNext(sess);
    seek.disabled = over || !canSeek(sess);
    const muted = !!sess.muted;
    setIcon(muteBtn, muted || !(sess.volume > 0) ? 'volume-x' : 'volume', muted ? 'Unmute' : 'Mute');
    muteBtn.disabled = over;
    volume.disabled = over;
    if (!volumeDrag) {
      const v = muted ? 0 : Math.min(1, Math.max(0, Number(sess.volume) || 0));
      volume.value = String(v);
      volume.style.setProperty('--fill', `${v * 100}%`);
    }
    stopBtn.disabled = over;
    const errText = sess.error && (sess.state === 'error' || over) ? errorText(sess.error.code, sess.error.message) : '';
    banner.textContent = errText || note;
    banner.hidden = !(errText || note);
    banner.classList.toggle('is-error', !!errText);
    el.classList.toggle('is-over', over);
    tick();
  }

  seek.addEventListener('input', () => {
    seeking = true;
    const dur = controller.duration() || 0;
    const t = (Number(seek.value) / 1000) * dur;
    timeEl.textContent = `${formatDuration(t)} / ${formatDuration(dur)}`;
    seek.style.setProperty('--fill', `${Number(seek.value) / 10}%`);
  });
  seek.addEventListener('change', () => {
    const dur = controller.duration() || 0;
    seeking = false;
    if (dur) act(() => controller.seek((Number(seek.value) / 1000) * dur));
  });

  const sendVolume = () => {
    const v = Math.round(Math.min(1, Math.max(0, Number(volume.value) || 0)) * 100) / 100;
    if (controller.session.muted && v > 0) act(() => controller.setMuted(false));
    act(() => controller.setVolume(v));
  };
  volume.addEventListener('input', () => {
    volumeDrag = true;
    volume.style.setProperty('--fill', `${Number(volume.value) * 100}%`);
    clearTimeout(volTimer);
    volTimer = setTimeout(sendVolume, 150);
  });
  volume.addEventListener('change', () => {
    clearTimeout(volTimer);
    sendVolume();
    volumeDrag = false;
  });

  stopBtn.addEventListener('click', async () => {
    if (stopBtn.classList.contains('is-loading')) return;
    ui.setLoading?.(stopBtn, true);
    let where = null;
    try {
      where = await controller.stop();
    } catch (err) {
      showNote(castErrorMessage(err));
    } finally {
      ui.setLoading?.(stopBtn, false);
      render(); // setLoading restored the pre-click disabled state; the session may be over now
    }
    if (where) opts.onStop?.(where);
  });

  const offs = [controller.on('change', render), controller.on('ended', render)];
  const timer = setInterval(() => {
    if (controller.session.state === 'playing') tick();
  }, 1000);
  render();

  return {
    el,
    update: render,
    destroy() {
      clearInterval(timer);
      clearTimeout(volTimer);
      clearTimeout(noteTimer);
      for (const off of offs) off();
      el.remove();
    },
  };
}
