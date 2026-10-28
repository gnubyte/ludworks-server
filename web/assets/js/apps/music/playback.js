// Playback decisions (pure, no DOM): stream or transcode, source URLs, the play-report rule
// (API.md §12 "Plays"), a retrying play reporter, ReplayGain and the volume curve.
// Unit tested in web/tests/music/playback.test.mjs.

const enc = encodeURIComponent;

/* ------------------------------------------------------------ source choice */

const MP4_CODECS = { aac: 'mp4a.40.2', alac: 'alac', flac: 'flac', opus: 'opus', mp3: 'mp4a.6B', ac3: 'ac-3', eac3: 'ec-3' };
const OGG_CODECS = { vorbis: 'vorbis', opus: 'opus', flac: 'flac' };
const WAV_CODECS = { pcm: '1' };

/**
 * MIME types (with codecs where they matter) to ask HTMLMediaElement.canPlayType about for a
 * track, most specific first. Empty when nothing browsers could play is known.
 * @param {{mime?: string, codec?: string, container?: string}} t Track
 * @returns {string[]}
 */
export function mimeCandidates(t) {
  const codec = String(t?.codec || '').toLowerCase();
  const container = String(t?.container || '').toLowerCase();
  const mime = String(t?.mime || '').toLowerCase().split(';')[0].trim();
  const out = [];
  const add = (m) => { if (m && !out.includes(m)) out.push(m); };
  switch (container) {
    case 'mp3':
      add('audio/mpeg');
      break;
    case 'flac':
      add('audio/flac');
      add('audio/x-flac');
      break;
    case 'ogg': case 'oga': case 'opus':
      if (OGG_CODECS[codec]) add(`audio/ogg; codecs="${OGG_CODECS[codec]}"`);
      break;
    case 'mp4': case 'm4a': case 'm4b': case 'mov':
      if (MP4_CODECS[codec]) add(`audio/mp4; codecs="${MP4_CODECS[codec]}"`);
      break;
    case 'wav': case 'wave':
      if (WAV_CODECS[codec]) {
        add(`audio/wav; codecs="${WAV_CODECS[codec]}"`);
        add('audio/wav');
      }
      break;
    case 'aiff': case 'aif':
      if (codec === 'pcm' || codec === '') {
        add('audio/aiff');
        add('audio/x-aiff');
      }
      break;
    case 'webm': case 'matroska':
      if (codec === 'opus' || codec === 'vorbis') add(`audio/webm; codecs="${codec}"`);
      break;
    case '':
      // Unknown container (not probed yet): trust the MIME type, with the codec when known.
      if (codec === 'mp3' || mime === 'audio/mpeg' || mime === 'audio/mp3') add('audio/mpeg');
      else if (mime === 'audio/flac' || mime === 'audio/x-flac') add('audio/flac');
      else if (mime && /^audio\/(ogg|mp4|x-m4a|aac|wav|x-wav|wave|webm|aiff|x-aiff)$/.test(mime)) add(mime);
      else if (mime === 'application/ogg') add('audio/ogg');
      break;
    default:
      break;
  }
  return out;
}

/**
 * Can this browser play the original file? canPlayType answers '', 'maybe' or 'probably'.
 * @param {object} t Track
 * @param {(type: string) => string} canPlayType
 */
export function canPlayOriginal(t, canPlayType) {
  if (typeof canPlayType !== 'function') return false;
  return mimeCandidates(t).some((m) => {
    try {
      return canPlayType(m) !== '';
    } catch {
      return false;
    }
  });
}

/**
 * The transcode format this browser plays best: ADTS AAC, else MP3, else Ogg Opus; null when none.
 * @param {(type: string) => string} canPlayType
 * @returns {'aac'|'mp3'|'opus'|null}
 */
export function transcodeFormat(canPlayType) {
  const ok = (m) => {
    try {
      return typeof canPlayType === 'function' && canPlayType(m) !== '';
    } catch {
      return false;
    }
  };
  if (ok('audio/aac')) return 'aac';
  if (ok('audio/mpeg')) return 'mp3';
  if (ok('audio/ogg; codecs="opus"')) return 'opus';
  return null;
}

/**
 * Stream the original when the browser can play it, else a transcode.
 * @param {object} t Track
 * @param {{canPlayType: (type: string) => string, forceTranscode?: boolean, noTranscode?: boolean}} o
 * @returns {{kind: 'stream'}|{kind: 'transcode', format: string}|{kind: 'none', reason: string}}
 */
export function chooseSource(t, { canPlayType, forceTranscode = false, noTranscode = false } = {}) {
  if (!forceTranscode && canPlayOriginal(t, canPlayType)) return { kind: 'stream' };
  if (noTranscode) return { kind: 'none', reason: 'ffmpeg' };
  const format = transcodeFormat(canPlayType);
  return format ? { kind: 'transcode', format } : { kind: 'none', reason: 'browser' };
}

/** Direct stream URL of a track (cookie auth: no token in the URL). */
export function streamUrl(id) {
  return `/api/v1/music/tracks/${enc(id)}/stream`;
}

/**
 * Transcode URL; `start` (seconds) restarts the transcode there (seeking in a transcode).
 * @param {string} id
 * @param {{format?: string, bitrate?: number, start?: number}} [o]
 */
export function transcodeUrl(id, { format = 'aac', bitrate = 160, start = 0 } = {}) {
  const q = new URLSearchParams({ format });
  const b = Math.round(Number(bitrate));
  if (Number.isFinite(b) && b !== 160) q.set('bitrate', String(Math.max(64, Math.min(320, b))));
  const s = Math.floor(Math.max(0, Number(start) || 0) * 10) / 10;
  if (s > 0) q.set('start', String(s));
  return `/api/v1/music/tracks/${enc(id)}/transcode?${q}`;
}

/** Original-file download URL (always an attachment). */
export function downloadUrl(id) {
  return `/api/v1/music/tracks/${enc(id)}/download?quality=original`;
}

/* ------------------------------------------------------------------- plays */

/** A play counts after min(50% of the duration, 240 s) of listening (API.md §12). */
export const PLAY_MAX_SECONDS = 240;

/** Seconds of listening a play needs for a track of this duration. */
export function playThreshold(duration) {
  const d = Number(duration);
  return Number.isFinite(d) && d > 0 ? Math.min(d / 2, PLAY_MAX_SECONDS) : PLAY_MAX_SECONDS;
}

/**
 * Seconds of real listening between two currentTime readings: normal progress counts, seeks
 * (jumps) and going backwards do not.
 * @param {number} prev
 * @param {number} cur
 * @param {number} [maxStep] largest gap between readings that still counts as listening
 */
export function listenedDelta(prev, cur, maxStep = 2) {
  const a = Number(prev);
  const b = Number(cur);
  if (!Number.isFinite(a) || !Number.isFinite(b)) return 0;
  const d = b - a;
  return d > 0 && d <= maxStep ? d : 0;
}

/**
 * Should a play be reported now? Once `listened` reaches the threshold (not yet reported), or
 * when the track ended without a known duration (the server cannot apply the 50% rule then;
 * `completed` makes it count).
 * @param {{listened: number, duration?: number|null, ended?: boolean, reported?: boolean}} s
 * @returns {{report: boolean, completed: boolean}}
 */
export function shouldReport({ listened, duration = null, ended = false, reported = false }) {
  if (reported) return { report: false, completed: false };
  if ((Number(listened) || 0) >= playThreshold(duration)) return { report: true, completed: !!ended };
  const known = Number.isFinite(Number(duration)) && Number(duration) > 0;
  if (ended && !known && (Number(listened) || 0) >= 30) return { report: true, completed: true };
  return { report: false, completed: false };
}

/** Body item of POST /music/plays. */
export function playRecord({ trackId, playedAt, seconds, completed = false, context = null }) {
  const rec = { track_id: trackId, played_at: playedAt, seconds_played: Math.round(Math.max(0, Number(seconds) || 0) * 10) / 10 };
  if (completed) rec.completed = true;
  if (context && context.kind) rec.context = context.id === undefined ? { kind: context.kind } : { kind: context.kind, id: context.id };
  return rec;
}

/** Errors worth retrying: network failures, timeouts, 408, 429 and 5xx. */
export function isTransient(err) {
  const s = Number(err?.status);
  return !Number.isFinite(s) || s === 0 || s === 408 || s === 429 || s >= 500;
}

/**
 * Sends plays (POST /music/plays) and retries briefly on transient errors; repeats of the same
 * (track_id, played_at) are ignored by the server, so retrying is safe.
 * @param {{send: (plays: object[]) => Promise<any>, delays?: number[], schedule?: Function, cancel?: Function,
 *          onDrop?: (plays: object[], err: any) => void}} o
 */
export function createPlayReporter({ send, delays = [2000, 5000, 15000, 30000, 60000], schedule = setTimeout, cancel = clearTimeout, onDrop = null }) {
  let pending = [];
  let attempt = 0;
  let timer = null;
  let inflight = null;
  let destroyed = false;

  function arm(ms) {
    if (timer !== null || destroyed) return;
    timer = schedule(() => {
      timer = null;
      flush();
    }, ms);
  }

  async function flush() {
    if (destroyed || inflight || !pending.length) return inflight;
    const batch = pending.slice(0, 500);
    inflight = (async () => {
      try {
        await send(batch);
        pending = pending.filter((p) => !batch.includes(p));
        attempt = 0;
      } catch (err) {
        if (!isTransient(err) || attempt >= delays.length) {
          pending = pending.filter((p) => !batch.includes(p));
          attempt = 0;
          onDrop?.(batch, err);
        } else {
          arm(delays[attempt]);
          attempt += 1;
        }
      } finally {
        inflight = null;
      }
      if (pending.length && timer === null && attempt === 0) flush();
    })();
    return inflight;
  }

  return {
    /** Queue a play record and try to send it. */
    add(rec) {
      if (destroyed || !rec) return;
      pending.push(rec);
      if (timer === null) flush();
    },
    flush,
    /** Plays not sent yet (for a keepalive request when the page goes away); empties the queue. */
    takePending() {
      const out = pending;
      pending = [];
      if (timer !== null) {
        cancel(timer);
        timer = null;
      }
      attempt = 0;
      return out;
    },
    get pending() { return pending.slice(); },
    destroy() {
      destroyed = true;
      if (timer !== null) cancel(timer);
      timer = null;
      pending = [];
    },
  };
}

/* ------------------------------------------------------------ loudness */

/** Normalisation modes. */
export const NORMALIZE_MODES = Object.freeze(['off', 'track', 'album']);

/**
 * Linear gain for ReplayGain normalisation: 10^(gain/20), limited by the peak so the result
 * never clips (track_peak is 1.0 when the file has none, so unknown peaks never boost).
 * Album mode uses the album gain when the track has one. 1 when off or without ReplayGain.
 * @param {{track_gain_db: number, track_peak: number, album_gain_db?: number|null, album_peak?: number|null}|null} rg
 * @param {'off'|'track'|'album'} mode
 * @param {{preampDb?: number}} [opts]
 */
export function replayGainFactor(rg, mode = 'track', { preampDb = 0 } = {}) {
  if (!rg || mode === 'off') return 1;
  let gain = Number(rg.track_gain_db);
  let peak = Number(rg.track_peak);
  if (mode === 'album' && rg.album_gain_db !== null && rg.album_gain_db !== undefined && Number.isFinite(Number(rg.album_gain_db))) {
    gain = Number(rg.album_gain_db);
    if (rg.album_peak !== null && rg.album_peak !== undefined && Number.isFinite(Number(rg.album_peak))) peak = Number(rg.album_peak);
  }
  if (!Number.isFinite(gain)) return 1;
  let f = 10 ** ((gain + (Number(preampDb) || 0)) / 20);
  if (Number.isFinite(peak) && peak > 0) f = Math.min(f, 1 / peak);
  return Math.max(0.001, Math.min(f, 8));
}

/**
 * Perceptual volume: the slider position (0–1) → linear amplitude. Squared, so the lower half
 * of the slider is usable instead of everything above 30% sounding the same.
 */
export function volumeToGain(v) {
  const x = Number(v);
  if (!Number.isFinite(x) || x <= 0) return 0;
  if (x >= 1) return 1;
  return x * x;
}

/** Clamp a volume to 0–1 (default 1 for junk). */
export function clampVolume(v, fallback = 1) {
  const x = Number(v);
  return Number.isFinite(x) ? Math.max(0, Math.min(1, x)) : fallback;
}

/**
 * Previous-button rule: restart the current track after the first seconds, else go back.
 * @param {number} position seconds into the current track
 */
export function previousRestarts(position, threshold = 3) {
  return (Number(position) || 0) > threshold;
}

/** Should the next track be preloaded now (near the end of a direct stream)? */
export function shouldPreload({ position, duration, sourceKind, lead = 25 }) {
  if (sourceKind !== 'stream') return false;
  const d = Number(duration);
  const p = Number(position);
  if (!Number.isFinite(d) || d <= 0 || !Number.isFinite(p)) return false;
  return d - p <= lead;
}
