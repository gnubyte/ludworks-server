// The music player — one per page while the bottom dock shows it, so playback continues while the
// user moves between apps. It owns:
//   • the play queue (queue.js) and its paging feed (long lists load more tracks as they play),
//   • two <audio> elements: the current track and the preloaded next one (near-gapless changes),
//   • optional ReplayGain normalisation through Web Audio gain nodes (same-origin media),
//   • play reports (POST /music/plays, retried on transient errors, flushed on page hide),
//   • the MediaSession (lock screen / media keys), casting through core/cast.js,
//   • per-viewer preferences in localStorage (volume, shuffle/repeat, last queue as track ids),
//   • the dock UI (dock.js).
// The dock's onRemove (another app took the dock, or sign-out) destroys it and releases
// everything; on sign-out the stored queue is cleared too.

import { api as appApi, isAbortError } from '../../core/api.js';
import * as appUi from '../../core/ui.js';
import { store } from '../../core/store.js';
import { castButton, castControls, attachSession, findSession, currentItem, errorText as castErrorText, castErrorMessage } from '../../core/cast.js';
import { PlayQueue, MAX_STORED } from './queue.js';
import {
  chooseSource, streamUrl, transcodeUrl, listenedDelta, shouldReport, playRecord, createPlayReporter, replayGainFactor,
  volumeToGain, clampVolume, previousRestarts, shouldPreload, NORMALIZE_MODES,
} from './playback.js';
import { emit, on, loadPref, savePref } from './state.js';
import { musicApi } from './api.js';
import { artUrl, trackTitle, trackArtist, chunk } from './model.js';
import { createDock } from './dock.js';

let instance = null;
let host = null; // {dock, router} from the app's ctx (both outlive the app's mount)
let reattachChecked = false;

/** The live player, or null. */
export function getPlayer() {
  return instance;
}

/** Navigate in the app (also from the dock while another app is open). */
export function navigate(path) {
  host?.router?.navigate(path);
}

/** Remember how to reach the shell's dock (called on every Music mount). */
export function configurePlayer(ctx) {
  host = { dock: ctx.dock, router: ctx.router };
}

/** The live player, created (with its dock) when there is none. */
export function ensurePlayer() {
  if (!instance) instance = createPlayer();
  return instance;
}

/**
 * Called when the Music app mounts: restore the last queue (paused) or re-attach to a cast
 * session that still plays a track from an earlier visit.
 */
export async function restorePlayer() {
  if (instance) return instance;
  const me = store.get().user?.id || '';
  if (!reattachChecked) {
    reattachChecked = true;
    const sess = await findSession(appApi, (s, it) => it?.kind === 'track');
    if (sess && !instance && store.get().user?.id === me) {
      const p = ensurePlayer();
      await p.attachCastSession(sess);
      return p;
    }
  }
  if (instance) return instance;
  const data = loadPref(me, 'queue');
  if (!data || !Array.isArray(data.ids) || !data.ids.length) return null;
  const p = ensurePlayer();
  const ok = await p.restoreQueue(data);
  if (!ok && instance === p) p.close({ forget: true });
  return instance;
}

/** Can the browser use Web Audio for normalisation? */
function audioContextClass() {
  return globalThis.AudioContext || globalThis.webkitAudioContext || null;
}

const MS_ART = [['sm', 160], ['md', 320], ['lg', 640], ['xl', 1200]];
const MAX_ERROR_SKIPS = 5;

function createPlayer() {
  const me = store.get().user?.id || '';
  const mapi = musicApi(appApi);
  const ui = appUi;
  const queue = new PlayQueue();
  const prefs = loadPref(me, 'prefs') || {};
  queue.repeat = ['off', 'all', 'one'].includes(prefs.repeat) ? prefs.repeat : 'off';
  queue.shuffle = !!prefs.shuffle;

  const st = {
    playing: false, // the active element is playing
    wantPlay: false, // the user wants playback (survives loading/buffering)
    loading: false,
    error: '',
    volume: clampVolume(prefs.volume, 0.8),
    muted: !!prefs.muted,
    normalize: NORMALIZE_MODES.includes(prefs.normalize) ? prefs.normalize : 'off',
    noFfmpeg: false, // the server answered 501 for a transcode
    resumeAt: 0, // position to start the current entry at when nothing is loaded yet
    errorSkips: 0,
    destroyed: false,
    forget: false,
  };

  /* ------------------------------------------------------------ elements */
  const makeSlot = () => {
    const el = new Audio();
    el.preload = 'auto';
    return { el, entry: null, source: null, node: null, gain: null };
  };
  const slots = [makeSlot(), makeSlot()];
  let active = 0;
  const cur = () => slots[active];
  const spare = () => slots[1 - active];
  let audioCtx = null;

  // The play being listened to: plays are reported once per instance.
  let play = null; // {uid, trackId, context, duration, playedAt, listened, last, reported}
  let feed = null; // {more, done, busy} — loads more tracks for long lists
  let cast = null; // {controller, controls, windowStart, offs}

  const reporter = createPlayReporter({
    send: (plays) => mapi.plays(plays, { timeout: 15000 }),
    onDrop: (plays, err) => console.warn('music: dropped play reports', plays.length, err?.message || err),
  });

  /* ------------------------------------------------------------- helpers */
  const entry = () => queue.current;
  const track = () => queue.current?.track || null;

  function trackDurationOf(e) {
    const d = Number(e?.track?.duration_seconds);
    return Number.isFinite(d) && d > 0 ? d : 0;
  }

  function position() {
    if (cast) return cast.controller.position();
    const s = cur();
    if (!s.source || s.entry !== entry()) return st.resumeAt;
    const t = Number(s.el.currentTime) || 0;
    return (s.source.kind === 'transcode' ? s.source.base : 0) + t;
  }

  function duration() {
    if (cast) return cast.controller.duration() || trackDurationOf(entry());
    const d = trackDurationOf(entry());
    if (d) return d;
    const s = cur();
    return s.source?.kind === 'stream' && Number.isFinite(s.el.duration) ? s.el.duration : 0;
  }

  function canSeek() {
    if (cast) return cast.controller.canSeek();
    return duration() > 0;
  }

  function rgFactor(e) {
    return st.normalize === 'off' ? 1 : replayGainFactor(e?.track?.replaygain || null, st.normalize);
  }

  function applyVolume() {
    const base = st.muted ? 0 : volumeToGain(st.volume);
    for (const s of slots) {
      if (s.gain) {
        s.el.muted = false;
        s.el.volume = 1;
        s.gain.gain.value = base * rgFactor(s.entry);
      } else {
        s.el.muted = st.muted;
        s.el.volume = Math.min(1, base);
      }
    }
  }

  /** Route both elements through Web Audio (call from a user gesture: the context starts suspended otherwise). */
  function ensureGraph() {
    if (audioCtx) return true;
    const AC = audioContextClass();
    if (!AC) return false;
    try {
      audioCtx = new AC();
      for (const s of slots) {
        s.node = audioCtx.createMediaElementSource(s.el);
        s.gain = audioCtx.createGain();
        s.node.connect(s.gain).connect(audioCtx.destination);
      }
    } catch (err) {
      console.warn('music: Web Audio unavailable', err);
      return false;
    }
    applyVolume();
    return true;
  }

  function resumeGraph() {
    if (st.normalize !== 'off') ensureGraph();
    if (audioCtx && audioCtx.state === 'suspended') audioCtx.resume().catch(() => {});
  }

  function savePrefs() {
    savePref(me, 'prefs', { volume: st.volume, muted: st.muted, normalize: st.normalize, shuffle: queue.shuffle, repeat: queue.repeat });
  }

  let saveTimer = 0;
  function saveQueueSoon() {
    clearTimeout(saveTimer);
    saveTimer = setTimeout(saveQueue, 600);
  }

  function saveQueue() {
    clearTimeout(saveTimer);
    if (st.forget || !me || store.get().user?.id !== me) return;
    if (queue.isEmpty) {
      savePref(me, 'queue', null);
      return;
    }
    const data = queue.toJSON();
    const pos = position();
    data.pos = Number.isFinite(pos) ? Math.round(pos * 10) / 10 : 0;
    savePref(me, 'queue', data);
  }

  function changed(kind) {
    emit(kind, api);
    if (kind === 'queue') saveQueueSoon();
    dock.update(kind);
  }

  /* -------------------------------------------------------- play reports */
  function startPlayInstance(e, at) {
    play = e ? { uid: e.uid, trackId: e.id, context: e.context, duration: trackDurationOf(e), playedAt: null, listened: 0, last: at, reported: false } : null;
  }

  function maybeReport(ended = false) {
    if (!play || play.reported || !play.playedAt) return;
    const d = play.duration || duration();
    const r = shouldReport({ listened: play.listened, duration: d || null, ended, reported: play.reported });
    if (!r.report) return;
    play.reported = true;
    reporter.add(playRecord({ trackId: play.trackId, playedAt: play.playedAt, seconds: play.listened, completed: r.completed, context: play.context }));
  }

  /* ------------------------------------------------------------- loading */
  function canPlayType(m) {
    return cur().el.canPlayType(m);
  }

  function sourceFor(e, at, { forceTranscode = false } = {}) {
    const choice = chooseSource(e.track, { canPlayType, forceTranscode, noTranscode: st.noFfmpeg });
    if (choice.kind === 'stream') return { kind: 'stream', url: streamUrl(e.id), base: 0, seek: at, format: null };
    if (choice.kind === 'transcode') return { kind: 'transcode', url: transcodeUrl(e.id, { format: choice.format, start: at }), base: Math.floor(at * 10) / 10, seek: 0, format: choice.format };
    return { kind: 'none', reason: choice.reason };
  }

  function resetSlot(s) {
    s.entry = null;
    s.source = null;
    try {
      s.el.pause();
      s.el.removeAttribute('src');
      s.el.load();
    } catch { /* ignore */ }
  }

  function setError(msg) {
    st.error = msg;
    st.loading = false;
    changed('state');
    if (msg) dock.announce(msg);
  }

  // A track this browser cannot play (and the server cannot convert): say why and offer the
  // original file. At most one such toast every 10 s, so skipping a run of them stays quiet.
  let unplayableToastAt = 0;
  function unplayable(e, why) {
    setError(why);
    if (Date.now() - unplayableToastAt < 10000) return;
    unplayableToastAt = Date.now();
    ui.toast(why, { type: 'warning', timeout: 9000, action: { label: 'Download', onClick: () => mapi.download(e.id) } });
  }

  /**
   * Load the current entry into the active element.
   * @param {{autoplay?: boolean, at?: number, forceTranscode?: boolean}} o
   */
  function load({ autoplay = false, at = 0, forceTranscode = false } = {}) {
    const e = entry();
    st.error = '';
    if (!e) {
      for (const s of slots) resetSlot(s);
      st.playing = false;
      st.wantPlay = false;
      play = null;
      changed('track');
      changed('state');
      return;
    }
    // The preloaded element already holds this entry from the start: switch to it.
    const sp = spare();
    if (!forceTranscode && at === 0 && sp.entry === e && sp.source && sp.source.kind === 'stream' && !sp.el.error) {
      resetSlot(cur());
      active = 1 - active;
      applyVolume();
    } else {
      const s = cur();
      const src = sourceFor(e, at, { forceTranscode });
      resetSlot(s);
      if (sp.entry === e) resetSlot(sp);
      if (src.kind === 'none') {
        startPlayInstance(null, 0);
        st.playing = false;
        changed('track');
        unplayable(e, src.reason === 'ffmpeg'
          ? `“${trackTitle(e.track)}” can’t play here: your browser does not support its format and the server cannot convert it (ffmpeg is not installed).`
          : `“${trackTitle(e.track)}” can’t play in this browser.`);
        if (autoplay) skipAfterError();
        return;
      }
      s.entry = e;
      s.source = { ...src, tried: forceTranscode || src.kind === 'transcode' };
      s.el.src = src.url;
      applyVolume();
    }
    st.resumeAt = 0;
    const s = cur();
    // Listening is measured on the element's clock, which starts at 0 for a transcode.
    startPlayInstance(e, s.source.kind === 'transcode' ? 0 : at);
    st.wantPlay = autoplay;
    st.loading = autoplay;
    if (s.source.kind === 'stream' && s.source.seek > 0) {
      const target = s.source.seek;
      const seekWhenReady = () => {
        if (s.entry !== e) return;
        try {
          s.el.currentTime = target;
        } catch { /* ignore */ }
        if (play && play.uid === e.uid) play.last = target;
      };
      if (s.el.readyState >= 1) seekWhenReady();
      else s.el.addEventListener('loadedmetadata', seekWhenReady, { once: true });
      s.source.seek = 0;
    }
    if (autoplay) startElement();
    else st.playing = false;
    updateMediaSession();
    changed('track');
    changed('state');
    maybeExtend();
  }

  function startElement() {
    const s = cur();
    resumeGraph();
    const p = s.el.play();
    if (p && typeof p.catch === 'function') {
      p.catch((err) => {
        if (isAbortError(err) || s !== cur()) return;
        st.wantPlay = false;
        st.loading = false;
        st.playing = false;
        changed('state');
        if (err?.name === 'NotAllowedError') dock.announce('Press play to start playback.');
      });
    }
  }

  /** After an unplayable track: move on (bounded, so a list of unplayable files cannot loop). */
  function skipAfterError() {
    st.errorSkips += 1;
    if (st.errorSkips > MAX_ERROR_SKIPS) {
      st.errorSkips = 0;
      st.wantPlay = false;
      return;
    }
    const n = queue.peekNext({ auto: false });
    if (!n || n === entry()) return;
    setTimeout(() => {
      if (st.destroyed || cast) return;
      queue.next();
      load({ autoplay: true });
    }, 1200);
  }

  /** Why did a transcode fail? Ask the server (status only; the body is not read). */
  async function diagnose(url) {
    const ctrl = new AbortController();
    const timer = setTimeout(() => ctrl.abort(), 8000);
    try {
      const res = await fetch(url, { credentials: 'same-origin', cache: 'no-store', signal: ctrl.signal });
      const status = res.status;
      const retry = Number(res.headers.get('retry-after')) || 0;
      ctrl.abort();
      return { status, retry };
    } catch {
      return { status: 0, retry: 0 };
    } finally {
      clearTimeout(timer);
    }
  }

  async function onElementError(s) {
    if (s !== cur() || !s.entry || s.entry !== entry()) {
      if (s === spare()) resetSlot(s); // a failed preload: load normally later
      return;
    }
    const e = s.entry;
    const at = position();
    const wanted = st.wantPlay;
    if (s.source && !s.source.tried && !st.noFfmpeg) {
      // The browser could not decode the original after all: try a transcode.
      load({ autoplay: wanted, at, forceTranscode: true });
      return;
    }
    const d = s.source?.kind === 'transcode' ? await diagnose(s.source.url) : { status: 0 };
    if (st.destroyed || entry() !== e) return;
    st.playing = false;
    if (d.status === 501) {
      st.noFfmpeg = true;
      unplayable(e, `“${trackTitle(e.track)}” can’t play here: your browser does not support its format and the server cannot convert it (ffmpeg is not installed).`);
    } else if (d.status === 429) {
      setError(`The server is busy converting other audio. Retrying “${trackTitle(e.track)}”…`);
      setTimeout(() => {
        if (!st.destroyed && entry() === e && !cast) load({ autoplay: wanted, at, forceTranscode: true });
      }, Math.max(2, Math.min(30, d.retry || 5)) * 1000);
      return;
    } else if (d.status === 404) {
      setError(`“${trackTitle(e.track)}” is no longer available.`);
    } else {
      setError(`“${trackTitle(e.track)}” could not be played.`);
    }
    if (wanted) skipAfterError();
  }

  /* -------------------------------------------------------------- preload */
  function maybePreload() {
    if (cast) return;
    const s = cur();
    if (!s.source || !st.playing) return;
    if (!shouldPreload({ position: position(), duration: duration(), sourceKind: s.source.kind })) return;
    const n = queue.peekNext({ auto: true });
    const sp = spare();
    if (!n || n === entry() || sp.entry === n) return;
    const src = sourceFor(n, 0);
    if (src.kind !== 'stream') return; // transcodes hold a server slot: start them when needed
    resetSlot(sp);
    sp.entry = n;
    sp.source = { ...src, tried: false };
    sp.el.src = src.url;
    applyVolume();
  }

  /* ---------------------------------------------------------- feed paging */
  async function maybeExtend() {
    if (!feed || feed.done || feed.busy || queue.upcoming().length >= 5) return;
    feed.busy = true;
    const f = feed;
    try {
      const res = await f.more();
      if (st.destroyed || feed !== f) return;
      f.done = !!res?.done;
      const tracks = res?.tracks || [];
      if (tracks.length) {
        const added = queue.append(tracks, f.context);
        if (cast) castAppend(added);
        changed('queue');
      }
    } catch (err) {
      if (!isAbortError(err)) console.warn('music: could not load more tracks', err);
      if (feed === f) f.done = true;
    } finally {
      f.busy = false;
    }
  }

  /* -------------------------------------------------------- media events */
  function bind(s) {
    const el = s.el;
    el.addEventListener('playing', () => {
      if (s !== cur()) return;
      st.playing = true;
      st.loading = false;
      st.error = '';
      st.errorSkips = 0;
      if (play && !play.playedAt && play.uid === s.entry?.uid) play.playedAt = new Date().toISOString();
      setPlaybackState();
      changed('state');
    });
    el.addEventListener('pause', () => {
      // Switching sources pauses the element; that event arrives after play() was called for the
      // new source (paused is false again) and must not count as the user pausing.
      if (s !== cur() || !el.paused) return;
      st.playing = false;
      st.loading = false;
      if (!el.ended) st.wantPlay = false;
      setPlaybackState();
      saveQueue();
      changed('state');
    });
    el.addEventListener('waiting', () => {
      if (s !== cur()) return;
      st.loading = true;
      changed('state');
    });
    el.addEventListener('canplay', () => {
      if (s !== cur() || !st.loading) return;
      st.loading = st.wantPlay && el.paused;
      changed('state');
    });
    el.addEventListener('durationchange', () => {
      if (s === cur()) changed('time');
    });
    el.addEventListener('seeked', () => {
      if (s !== cur()) return;
      if (play) play.last = el.currentTime;
      setPositionState();
      changed('time');
    });
    el.addEventListener('timeupdate', () => {
      if (s !== cur() || !s.source) return;
      if (play && play.uid === s.entry?.uid && !el.paused) {
        const t = el.currentTime;
        play.listened += listenedDelta(play.last, t);
        play.last = t;
        maybeReport(false);
      }
      maybePreload();
      changed('time');
    });
    el.addEventListener('ended', () => {
      if (s !== cur()) return;
      maybeReport(true);
      onEnded();
    });
    el.addEventListener('error', () => {
      onElementError(s);
    });
  }
  slots.forEach(bind);

  function onEnded() {
    const n = queue.next({ auto: true });
    if (!n) {
      // End of the queue: back to the start of the last track, paused.
      st.playing = false;
      st.wantPlay = false;
      const s = cur();
      if (s.source?.kind === 'stream') {
        try { s.el.currentTime = 0; } catch { /* ignore */ }
      } else {
        resetSlot(s);
      }
      startPlayInstance(entry(), 0);
      setPlaybackState();
      changed('state');
      changed('time');
      saveQueue();
      return;
    }
    if (n === entry() && queue.repeat === 'one') {
      const s = cur();
      startPlayInstance(n, 0);
      if (s.source?.kind === 'stream') {
        s.el.currentTime = 0;
        startElement();
      } else {
        load({ autoplay: true });
      }
      return;
    }
    load({ autoplay: true });
    changed('queue');
  }

  /* -------------------------------------------------------- MediaSession */
  const ms = typeof navigator !== 'undefined' ? navigator.mediaSession : null;

  function updateMediaSession() {
    if (!ms) return;
    const t = track();
    try {
      if (!t || typeof MediaMetadata === 'undefined') {
        ms.metadata = null;
        return;
      }
      const art = t.art_url || null;
      ms.metadata = new MediaMetadata({
        title: trackTitle(t), artist: trackArtist(t), album: t.album || '',
        artwork: art ? MS_ART.map(([size, px]) => ({ src: artUrl(art, size), sizes: `${px}x${px}`, type: 'image/jpeg' })) : [],
      });
    } catch { /* unsupported */ }
    setPlaybackState();
  }

  function setPlaybackState() {
    if (!ms) return;
    try {
      ms.playbackState = !entry() ? 'none' : (cast ? isPlayingCast() : st.playing) ? 'playing' : 'paused';
    } catch { /* ignore */ }
    setPositionState();
  }

  function setPositionState() {
    if (!ms?.setPositionState) return;
    const d = duration();
    try {
      if (d > 0) ms.setPositionState({ duration: d, playbackRate: 1, position: Math.max(0, Math.min(position(), d)) });
      else ms.setPositionState();
    } catch { /* invalid state */ }
  }

  const msHandlers = {
    play: () => api.play(),
    pause: () => api.pause(),
    stop: () => api.pause(),
    previoustrack: () => api.previous(),
    nexttrack: () => api.next(),
    seekto: (d) => { if (Number.isFinite(d?.seekTime)) api.seek(d.seekTime); },
    seekbackward: (d) => api.seekBy(-(d?.seekOffset || 10)),
    seekforward: (d) => api.seekBy(d?.seekOffset || 10),
  };
  function setMsHandlers(on) {
    if (!ms) return;
    for (const [action, fn] of Object.entries(msHandlers)) {
      try {
        ms.setActionHandler(action, on ? fn : null);
      } catch { /* action not supported */ }
    }
  }
  setMsHandlers(true);

  /* --------------------------------------------------------------- cast */
  const toItems = (entries) => entries.map((e) => ({ kind: 'track', id: e.id }));
  let pendingWindow = 0;

  const castBtn = castButton({
    api: appApi, ui, size: 'sm', className: 'mu-dock-btn mu-cast-btn', title: 'Cast to a device',
    getItems: () => {
      if (queue.isEmpty) return null;
      const w = queue.window(MAX_STORED);
      pendingWindow = w.start;
      return { items: toItems(w.entries), index: Math.max(0, queue.index - w.start), positionSeconds: Math.max(0, position()), repeat: queue.repeat, shuffle: false, volume: st.volume };
    },
    beforeStart: () => saveQueue(),
    media: () => cur().el,
    onSession: (c) => beginCast(c, pendingWindow),
  });

  function isPlayingCast() {
    const s = cast?.controller.session?.state;
    return s === 'playing' || s === 'buffering' || s === 'loading';
  }

  function castRun(p) {
    p?.catch?.((err) => ui.toast(castErrorMessage(err), { type: 'warning', timeout: 5000 }));
    return p;
  }

  function teardownCast() {
    if (!cast) return null;
    const c = cast;
    cast = null;
    for (const off of c.offs) off();
    c.controls.destroy();
    castBtn.setCasting(null);
    dock.setCastControls(null);
    return c;
  }

  /** A device plays the queue now: hand over from the local elements. */
  function beginCast(c, windowStart = 0) {
    if (cast) {
      if (cast.controller.id === c.id) return;
      const old = teardownCast(); // moved to another device: end the previous session
      if (!old.controller.ended) old.controller.stop().catch(() => {});
    }
    maybeReport(false);
    st.wantPlay = false;
    for (const s of slots) {
      try { s.el.pause(); } catch { /* ignore */ }
    }
    resetSlot(spare());
    const controls = castControls(c, { variant: 'bar', className: 'mu-cast-controls', onStop: (where) => endCast(where, null) });
    cast = {
      controller: c, controls, windowStart,
      offs: [
        c.on('change', (sess) => syncFromCast(sess)),
        c.on('ended', ({ reason, byUser }) => {
          if (!byUser) endCast({ index: Number.isInteger(c.session.index) ? c.session.index : 0, item: c.item(), positionSeconds: c.position() }, reason);
        }),
      ],
    };
    castBtn.setCasting(c.device);
    dock.setCastControls(controls.el, c.device);
    dock.announce(`Casting to ${c.device?.name || 'the device'}.`);
    syncFromCast(c.session);
    setPlaybackState();
    changed('state');
  }

  /** Follow the device's queue position in ours. */
  function syncFromCast(sess) {
    if (!cast || !sess) return;
    const it = currentItem(sess);
    if (!it) return;
    let i = cast.windowStart + (Number.isInteger(sess.index) ? sess.index : 0);
    if (queue.entries[i]?.id !== it.id) {
      i = queue.entries.findIndex((e) => e.id === it.id);
    }
    if (i >= 0 && i !== queue.index) {
      queue.jump(i);
      updateMediaSession();
      changed('track');
      changed('queue');
      maybeExtend();
    }
    setPlaybackState();
    changed('state');
  }

  /** Casting ended: continue locally where the device stopped (playing when the user stopped it). */
  function endCast(where, reason) {
    const c = teardownCast();
    if (!c) return;
    const it = where?.item;
    if (it) {
      let i = c.windowStart + (Number.isInteger(where.index) ? where.index : 0);
      if (queue.entries[i]?.id !== it.id) i = queue.entries.findIndex((e) => e.id === it.id);
      if (i >= 0) queue.jump(i);
    }
    const pos = Number(where?.positionSeconds);
    const at = Number.isFinite(pos) && pos > 0 && (!it || it.id === entry()?.id) ? pos : 0;
    if (!reason) {
      load({ autoplay: true, at });
    } else {
      load({ autoplay: false, at: reason === 'idle' ? 0 : at });
      if (reason !== 'idle') {
        ui.toast(castErrorText(reason), {
          type: reason === 'stopped' ? 'info' : 'warning', timeout: 9000,
          action: { label: 'Play here', onClick: () => api.play() },
        });
      }
    }
    changed('state');
  }

  /** Mirror a queue change on the device (replacing its queue reloads the current track there). */
  function castReplace() {
    if (!cast) return;
    if (queue.isEmpty) {
      castRun(cast.controller.stop());
      return;
    }
    const w = queue.window(MAX_STORED);
    cast.windowStart = w.start;
    castRun(cast.controller.replaceItems(toItems(w.entries), { index: Math.max(0, queue.index - w.start), positionSeconds: Math.max(0, cast.controller.position()) }));
  }

  function castInsertNext(added) {
    if (!cast || !added.length) return;
    if (queue.length > MAX_STORED || queue.index - cast.windowStart < 0) {
      castReplace();
      return;
    }
    castRun(cast.controller.insertItems(toItems(added), queue.index - cast.windowStart + 1));
  }

  function castAppend(added) {
    if (!cast || !added.length) return;
    if (queue.length > MAX_STORED) return; // the device keeps its window; replaced when it gets there
    castRun(cast.controller.insertItems(toItems(added)));
  }

  /* --------------------------------------------------------- global hooks */
  // Any other media on the page starting (the TV player, a video preview…) pauses the music.
  function onAnyPlay(e) {
    const t = e.target;
    if (!(t instanceof HTMLMediaElement) || slots.some((s) => s.el === t)) return;
    if (st.playing || st.wantPlay) api.pause();
  }
  document.addEventListener('play', onAnyPlay, true);

  function onPageHide() {
    saveQueue();
    const pending = reporter.takePending();
    if (!pending.length) return;
    try {
      fetch('/api/v1/music/plays', {
        method: 'POST', keepalive: true, credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': appApi.csrf() || '' },
        body: JSON.stringify({ plays: pending }),
      }).catch(() => {});
    } catch { /* ignore */ }
  }
  window.addEventListener('pagehide', onPageHide);
  const onVisibility = () => { if (document.visibilityState === 'hidden') saveQueue(); };
  document.addEventListener('visibilitychange', onVisibility);
  const posTimer = setInterval(() => { if (st.playing) saveQueue(); }, 15000);
  const offTrackUpdated = on('track-updated', (t) => {
    if (t && queue.updateTrack(t)) {
      if (t.id === entry()?.id) updateMediaSession();
      dock.update('track');
    }
  });

  /* ------------------------------------------------------------ the API */
  const api = {
    get queue() { return queue; },
    get current() { return queue.current; },
    get playing() { return cast ? isPlayingCast() : st.playing; },
    get loading() { return cast ? ['connecting', 'loading', 'buffering'].includes(cast.controller.session?.state) : st.loading; },
    get wantPlay() { return st.wantPlay; },
    get error() { return st.error; },
    get volume() { return st.volume; },
    get muted() { return st.muted; },
    get normalize() { return st.normalize; },
    get casting() { return cast ? (cast.controller.device || {}) : null; },
    get castController() { return cast?.controller || null; },
    get noFfmpeg() { return st.noFfmpeg; },
    get destroyed() { return st.destroyed; },
    position,
    duration,
    canSeek,
    castButton: castBtn,

    /**
     * Replace the queue and start playing.
     * @param {object[]} tracks
     * @param {{index?: number|null, context?: object|null, shuffle?: boolean,
     *          more?: () => Promise<{tracks: object[], done: boolean}>}} [o]
     *   more: loads further tracks of a long list as the queue runs low.
     */
    playTracks(tracks, { index = 0, context = null, shuffle, more = null } = {}) {
      if (!tracks?.length) return;
      maybeReport(false);
      st.errorSkips = 0;
      queue.set(tracks, { index, context, shuffle });
      feed = more ? { more, done: false, busy: false, context } : null;
      savePrefs();
      changed('queue');
      if (cast) {
        castReplaceFresh();
        changed('track');
        return;
      }
      load({ autoplay: true });
    },
    /** Insert tracks after the current one. */
    playNext(tracks, context = null) {
      const wasEmpty = queue.isEmpty;
      const added = queue.insertNext(tracks, context);
      if (!added.length) return;
      changed('queue');
      if (cast) castInsertNext(added);
      else if (wasEmpty) load({ autoplay: false });
      ui.toast(added.length === 1 ? `Playing next: ${trackTitle(added[0].track)}` : `${added.length} songs play next`, { timeout: 2500 });
    },
    /** Append tracks to the queue. */
    addToQueue(tracks, context = null) {
      const wasEmpty = queue.isEmpty;
      const added = queue.append(tracks, context);
      if (!added.length) return;
      changed('queue');
      if (cast) castAppend(added);
      else if (wasEmpty) load({ autoplay: false });
      ui.toast(added.length === 1 ? `Added to the queue: ${trackTitle(added[0].track)}` : `${added.length} songs added to the queue`, { timeout: 2500 });
    },
    play() {
      if (cast) return castRun(cast.controller.play());
      if (!entry()) return null;
      const s = cur();
      if (!s.source || s.entry !== entry()) {
        load({ autoplay: true, at: st.resumeAt || 0 });
        return null;
      }
      st.wantPlay = true;
      if (s.el.ended && s.source.kind !== 'stream') {
        load({ autoplay: true });
        return null;
      }
      startElement();
      changed('state');
      return null;
    },
    pause() {
      if (cast) return castRun(cast.controller.pause());
      st.wantPlay = false;
      try { cur().el.pause(); } catch { /* ignore */ }
      changed('state');
      return null;
    },
    toggle() {
      return api.playing || (!cast && st.wantPlay && st.loading) ? api.pause() : api.play();
    },
    next() {
      if (cast) return castRun(cast.controller.next());
      maybeReport(false);
      const n = queue.next();
      if (!n) return null;
      st.errorSkips = 0;
      load({ autoplay: st.playing || st.wantPlay });
      changed('queue');
      return null;
    },
    previous() {
      if (cast) {
        if (previousRestarts(cast.controller.position()) && cast.controller.canSeek()) return castRun(cast.controller.seek(0));
        return castRun(cast.controller.previous());
      }
      if (previousRestarts(position()) || !queue.peekPrevious()) {
        api.seek(0);
        return null;
      }
      maybeReport(false);
      queue.previous();
      load({ autoplay: st.playing || st.wantPlay });
      changed('queue');
      return null;
    },
    hasNext() {
      return cast ? cast.controller.hasNext() : !!queue.peekNext();
    },
    hasPrevious() {
      return !!entry();
    },
    /** Play queue entry i. */
    jump(i) {
      const e = queue.jump(i);
      if (!e) return;
      if (cast) {
        const ci = i - cast.windowStart;
        if (ci >= 0 && ci < MAX_STORED && queue.length <= MAX_STORED) castRun(cast.controller.jump(ci));
        else castReplaceFresh();
        changed('queue');
        return;
      }
      maybeReport(false);
      st.errorSkips = 0;
      load({ autoplay: true });
      changed('queue');
    },
    seek(t) {
      const target = Math.max(0, Number(t) || 0);
      if (cast) return castRun(cast.controller.seek(target));
      const s = cur();
      if (!entry()) return null;
      if (!s.source || s.entry !== entry()) {
        st.resumeAt = target;
        changed('time');
        return null;
      }
      if (play) play.last = target;
      if (s.source.kind === 'stream') {
        try {
          s.el.currentTime = target;
        } catch { /* not seekable yet */ }
      } else {
        // A transcode is a live stream: restart it at the new position.
        const wanted = st.wantPlay || st.playing;
        const e = entry();
        s.source = { ...s.source, url: transcodeUrl(e.id, { format: s.source.format, start: target }), base: Math.floor(target * 10) / 10 };
        s.el.src = s.source.url;
        if (play) play.last = 0;
        if (wanted) startElement();
      }
      setPositionState();
      changed('time');
      return null;
    },
    seekBy(dt) {
      return api.seek(Math.max(0, position() + (Number(dt) || 0)));
    },
    setVolume(v) {
      if (cast) return castRun(cast.controller.setVolume(clampVolume(v)));
      st.volume = clampVolume(v);
      if (st.volume > 0) st.muted = false;
      applyVolume();
      savePrefs();
      changed('state');
      return null;
    },
    toggleMute() {
      if (cast) return castRun(cast.controller.setMuted(!cast.controller.session.muted));
      st.muted = !st.muted;
      applyVolume();
      savePrefs();
      changed('state');
      return null;
    },
    setShuffle(onOff) {
      if (!queue.setShuffle(onOff)) return;
      savePrefs();
      changed('queue');
      if (cast) castReplace();
    },
    toggleShuffle() {
      api.setShuffle(!queue.shuffle);
    },
    cycleRepeat() {
      const mode = queue.cycleRepeat();
      savePrefs();
      changed('queue');
      if (cast) castRun(cast.controller.setRepeat(mode));
      return mode;
    },
    /** ReplayGain normalisation: 'off' | 'track' | 'album' (call from a user gesture). */
    setNormalize(mode) {
      if (!NORMALIZE_MODES.includes(mode)) return false;
      if (mode !== 'off' && !ensureGraph()) {
        ui.toast('Volume normalization needs Web Audio, which this browser does not offer.', { type: 'warning' });
        return false;
      }
      st.normalize = mode;
      if (audioCtx?.state === 'suspended') audioCtx.resume().catch(() => {});
      applyVolume();
      savePrefs();
      changed('state');
      return true;
    },
    removeFromQueue(uids) {
      const r = queue.remove(uids);
      if (!r.removed) return;
      changed('queue');
      if (cast) {
        castReplace();
        changed('track');
        return;
      }
      if (r.currentRemoved) load({ autoplay: st.playing || st.wantPlay });
    },
    moveInQueue(uids, pos) {
      if (!queue.move(uids, pos)) return;
      changed('queue');
      if (cast) castReplace();
    },
    clearUpcoming() {
      if (!queue.clearUpcoming()) return;
      feed = null;
      changed('queue');
      if (cast) castReplace();
    },
    /** Stop, forget the queue and close the player (removes the dock). */
    close({ forget = true } = {}) {
      st.forget = forget;
      if (forget) savePref(me, 'queue', null);
      if (host?.dock?.current === dock.el) host.dock.set(null);
      else destroy();
    },
    /** Restore a stored queue (paused at the stored position). */
    async restoreQueue(data) {
      const ids = data.ids.filter((x) => typeof x === 'string').slice(0, MAX_STORED);
      const byId = new Map();
      try {
        for (const part of chunk([...new Set(ids)], 500)) {
          const res = await mapi.lookup(part);
          for (const t of res?.tracks || []) byId.set(t.id, t);
        }
      } catch {
        return false;
      }
      if (st.destroyed || !queue.isEmpty) return !st.destroyed;
      const restored = PlayQueue.fromJSON(data, byId);
      if (restored.isEmpty) return false;
      queue.entries = restored.entries;
      queue.index = restored.index;
      queue.original = restored.original;
      queue.shuffle = restored.shuffle;
      queue.repeat = restored.repeat;
      queue.version += 1;
      const sameTrack = data.ids[data.index] === queue.current?.id;
      st.resumeAt = sameTrack && Number.isFinite(Number(data.pos)) ? Math.max(0, Number(data.pos)) : 0;
      startPlayInstance(queue.current, st.resumeAt);
      updateMediaSession();
      changed('queue');
      changed('track');
      changed('state');
      return true;
    },
    /** Re-attach to a cast session from an earlier visit (rebuilds the queue from its items). */
    async attachCastSession(sess) {
      const items = (sess.items || []).filter((it) => it?.kind === 'track');
      const byId = new Map();
      try {
        for (const part of chunk([...new Set(items.map((it) => it.id))], 500)) {
          const res = await mapi.lookup(part);
          for (const t of res?.tracks || []) byId.set(t.id, t);
        }
      } catch { /* show what we can */ }
      if (st.destroyed) return;
      const tracks = [];
      let index = 0;
      (sess.items || []).forEach((it, i) => {
        const t = it?.kind === 'track' ? (byId.get(it.id) || { id: it.id, title: it.title, artist: it.subtitle, art_url: null, duration_seconds: it.duration_seconds }) : null;
        if (!t) return;
        if (i === sess.index) index = tracks.length;
        tracks.push(t);
      });
      if (!tracks.length) return;
      queue.set(tracks, { index, shuffle: false });
      queue.shuffle = !!sess.shuffle;
      queue.repeat = ['off', 'all', 'one'].includes(sess.repeat) ? sess.repeat : queue.repeat;
      beginCast(attachSession(appApi, sess), 0);
      changed('queue');
      changed('track');
    },
    destroy,
  };

  function castReplaceFresh() {
    if (!cast) return;
    const w = queue.window(MAX_STORED);
    cast.windowStart = w.start;
    castRun(cast.controller.replaceItems(toItems(w.entries), { index: Math.max(0, queue.index - w.start), positionSeconds: 0 }));
  }

  /* ------------------------------------------------------------ teardown */
  function destroy() {
    if (st.destroyed) return;
    st.destroyed = true;
    const signedOut = store.get().user?.id !== me;
    if (!signedOut && !st.forget) saveQueue();
    if (signedOut) savePref(me, 'queue', null);
    clearTimeout(saveTimer);
    clearInterval(posTimer);
    document.removeEventListener('play', onAnyPlay, true);
    window.removeEventListener('pagehide', onPageHide);
    document.removeEventListener('visibilitychange', onVisibility);
    offTrackUpdated();
    if (!signedOut) reporter.flush().finally(() => reporter.destroy());
    else reporter.destroy();
    const c = teardownCast();
    c?.controller.detach(); // the device keeps playing; the server records its plays
    castBtn.destroy();
    for (const s of slots) resetSlot(s);
    if (audioCtx) {
      audioCtx.close().catch(() => {});
      audioCtx = null;
    }
    setMsHandlers(false);
    if (ms) {
      try {
        ms.metadata = null;
        ms.playbackState = 'none';
      } catch { /* ignore */ }
    }
    dock.destroy();
    if (instance === api) instance = null;
    emit('player', null);
  }

  const dock = createDock(api, {
    navigate: (p) => host?.router?.navigate(p),
    onRoute: (fn) => (host?.router?.onChange ? host.router.onChange(fn) : null),
  });
  host?.dock?.set(dock.el, { onRemove: destroy });
  applyVolume();
  setTimeout(() => emit('player', api), 0);
  return api;
}
