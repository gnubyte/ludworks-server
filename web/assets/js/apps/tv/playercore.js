// Player chrome shared by the TV player and the on-demand player: a <video> stage with an
// auto-hiding control bar (play/pause, seek or live progress, time, volume/mute, captions,
// picture-in-picture, fullscreen), autoplay fallback (muted + "tap to unmute") and
// transcode-aware seeking (a transcoded stream restarts at the requested position).

import { h, cls } from '../../core/dom.js';
import { formatDuration } from '../../core/format.js';
import { tvIcon } from './icons.js';
import { iconButton, pref } from './shared.js';
import { clampVolume, shiftCue } from './model.js';

const HIDE_MS = 3200;
// The server drops a transcode whose client stops reading for 60 s (a paused player stops
// reading once its buffer is full), so a transcode paused longer than this restarts at the
// current position on resume, and a network error after playback restarts it once.
export const TRANSCODE_STALE_MS = 45000;
const RESTART_COOLDOWN_MS = 15000;

/**
 * @param {{live?: boolean, onEnded?: () => void, onError?: (err: MediaError|null) => void,
 *          onTime?: (pos: number) => void, onUserActivity?: () => void, sourceAt?: (sec: number) => any,
 *          onCaptions?: (track: {label: string, srclang?: string}|null) => void,
 *          extraControls?: Node[], label?: string}} opts
 */
export function createPlayer(opts = {}) {
  const live = !!opts.live;
  const video = h('video', { class: 'tv-video', playsinline: true, preload: 'auto', aria: { label: opts.label || 'Video' } });
  video.setAttribute('playsinline', '');
  video.disablePictureInPicture = false;
  const overlays = h('div', { class: 'tv-overlays' });
  // Artwork shown over the (black) video area while an audio-only program plays.
  const artImg = h('img', { alt: '', draggable: 'false', decoding: 'async' });
  const art = h('div', { class: 'tv-art', hidden: true, aria: { hidden: 'true' } }, artImg);
  artImg.addEventListener('error', () => { art.hidden = true; });
  const spinner = h('div', { class: 'tv-buffering', hidden: true }, h('span', { class: 'spinner spinner-lg', role: 'status', aria: { label: 'Loading' } }));
  const unmutePill = h('button', { type: 'button', class: 'tv-unmute', hidden: true }, tvIcon('volume-x', 16), 'Tap to unmute');

  // ---- controls
  const playBtn = iconButton({ icon: 'pause', title: 'Pause (k)', className: 'tv-ctl', onClick: () => api.togglePlay() });
  const muteBtn = iconButton({ icon: 'volume', title: 'Mute (m)', className: 'tv-ctl', onClick: () => api.toggleMute() });
  const volume = h('input', { type: 'range', class: 'tv-volume', min: '0', max: '1', step: '0.05', aria: { label: 'Volume' } });
  const timeLabel = h('span', { class: 'tv-time nums' }, '0:00');
  const seek = h('input', { type: 'range', class: 'tv-seek', min: '0', max: '1000', step: '1', value: '0', aria: { label: live ? 'Program progress' : 'Seek' } });
  if (live) seek.disabled = true;
  const ccBtn = iconButton({ icon: 'captions', title: 'Subtitles (c)', className: 'tv-ctl', onClick: () => api.cycleCaptions() });
  ccBtn.hidden = true;
  const pipBtn = iconButton({ icon: 'pip', title: 'Picture in picture (p)', className: 'tv-ctl', onClick: () => api.togglePiP() });
  pipBtn.hidden = !document.pictureInPictureEnabled;
  const fsBtn = iconButton({ icon: 'maximize', title: 'Fullscreen (f)', className: 'tv-ctl', onClick: () => api.toggleFullscreen() });
  const bar = h('div', { class: 'tv-controls', role: 'toolbar', aria: { label: 'Player controls' } },
    h('div', { class: 'tv-seek-row' }, seek),
    h('div', { class: 'tv-ctl-row' },
      h('div', { class: 'tv-ctl-group' }, live ? null : playBtn, muteBtn, volume, timeLabel),
      h('div', { class: 'tv-ctl-group tv-ctl-extra' }, ...(opts.extraControls || [])),
      h('div', { class: 'tv-ctl-group' }, ccBtn, pipBtn, fsBtn)));
  const stage = h('div', { class: cls('tv-stage', { 'is-live': live }), tabindex: '-1' }, video, art, spinner, overlays, unmutePill, bar);

  let src = null; // {url, kind, base, seek, duration}
  let hideTimer = 0;
  let seeking = false;
  let destroyed = false;
  let liveProgress = null; // () => fraction (live mode)

  // ---- preferences
  const savedVol = Number(pref('lw.tv.volume'));
  video.volume = Number.isFinite(savedVol) && pref('lw.tv.volume') !== null ? clampVolume(savedVol) : 1;
  video.muted = pref('lw.tv.muted') === '1';
  volume.value = String(video.volume);

  function syncVolumeUI() {
    const muted = video.muted || video.volume === 0;
    muteBtn.replaceChildren(tvIcon(muted ? 'volume-x' : 'volume', 18));
    muteBtn.setAttribute('aria-label', muted ? 'Unmute (m)' : 'Mute (m)');
    muteBtn.dataset.tip = muted ? 'Unmute (m)' : 'Mute (m)';
    volume.value = String(video.muted ? 0 : video.volume);
    volume.style.setProperty('--fill', `${(video.muted ? 0 : video.volume) * 100}%`);
    if (!video.muted) unmutePill.hidden = true;
  }

  function syncPlayUI() {
    playBtn.replaceChildren(tvIcon(video.paused ? 'play' : 'pause', 18));
    const t = video.paused ? 'Play (k)' : 'Pause (k)';
    playBtn.setAttribute('aria-label', t);
    playBtn.dataset.tip = t;
    stage.classList.toggle('is-paused', video.paused);
  }

  function position() {
    return (src?.base || 0) + (video.currentTime || 0);
  }

  // WebVTT cue times are relative to the file; a transcode from ?start=<base> plays from 0, so
  // cues move back by base (re-applied from the original times whenever the source changes).
  const cueTimes = new WeakMap();
  function alignCues() {
    const base = src?.base || 0;
    for (const el of video.querySelectorAll('track')) {
      const cues = el.track?.cues;
      if (!cues) continue;
      for (const cue of cues) {
        let t = cueTimes.get(cue);
        if (!t) {
          t = [cue.startTime, cue.endTime];
          cueTimes.set(cue, t);
        }
        const [start, end] = shiftCue(t[0], t[1], base);
        try {
          cue.startTime = start;
          cue.endTime = end;
        } catch { /* read-only in this browser */ }
      }
    }
  }

  function syncTime() {
    if (live) {
      const f = liveProgress ? liveProgress() : 0;
      seek.value = String(Math.round(f * 1000));
      seek.style.setProperty('--fill', `${f * 100}%`);
      return;
    }
    const dur = src?.duration || (Number.isFinite(video.duration) ? src.base + video.duration : 0);
    const pos = position();
    timeLabel.textContent = dur ? `${formatDuration(pos)} / ${formatDuration(dur)}` : formatDuration(pos);
    if (!seeking && dur) {
      const f = Math.min(1, pos / dur);
      seek.value = String(Math.round(f * 1000));
      seek.style.setProperty('--fill', `${f * 100}%`);
    }
  }

  // ---- auto-hide
  function showControls() {
    stage.classList.add('show-controls');
    clearTimeout(hideTimer);
    hideTimer = setTimeout(() => {
      if (!video.paused && !bar.contains(document.activeElement) && !stage.classList.contains('keep-controls')) stage.classList.remove('show-controls');
    }, HIDE_MS);
    opts.onUserActivity?.();
  }
  stage.addEventListener('pointermove', showControls);
  stage.addEventListener('pointerdown', showControls);
  stage.addEventListener('focusin', showControls);
  stage.addEventListener('keydown', showControls);
  video.addEventListener('click', () => {
    if (!live) api.togglePlay();
  });
  video.addEventListener('dblclick', () => api.toggleFullscreen());

  // ---- media events
  video.addEventListener('waiting', () => { spinner.hidden = false; });
  video.addEventListener('playing', () => { spinner.hidden = true; syncPlayUI(); });
  video.addEventListener('canplay', () => { spinner.hidden = true; });
  let pausedSince = 0;
  let lastRestart = 0;
  /** Restart a transcoded source at the current position (needs opts.sourceAt). */
  function restartTranscode() {
    if (src?.kind !== 'transcode' || !opts.sourceAt) return false;
    lastRestart = performance.now();
    api.load({ ...opts.sourceAt(position()), duration: src.duration, poster: src.poster });
    return true;
  }
  video.addEventListener('pause', () => {
    pausedSince = performance.now();
    syncPlayUI();
    showControls();
  });
  video.addEventListener('play', () => {
    const stale = pausedSince && performance.now() - pausedSince > TRANSCODE_STALE_MS;
    pausedSince = 0;
    if (stale) restartTranscode();
  });
  video.addEventListener('play', syncPlayUI);
  video.addEventListener('volumechange', () => {
    pref('lw.tv.volume', video.volume);
    pref('lw.tv.muted', video.muted ? '1' : '0');
    syncVolumeUI();
  });
  video.addEventListener('timeupdate', () => {
    syncTime();
    opts.onTime?.(position());
  });
  video.addEventListener('ended', () => opts.onEnded?.());
  video.addEventListener('error', () => {
    if (destroyed || !video.getAttribute('src')) return;
    // A transcode cut by the server (e.g. after a long pause) resumes where it stopped.
    if (video.error?.code === 2 && video.currentTime > 0 && performance.now() - lastRestart > RESTART_COOLDOWN_MS && restartTranscode()) return;
    spinner.hidden = true;
    opts.onError?.(video.error);
  });
  // Initial seek (live offset / resume position). Files without a duration in their header
  // (e.g. streamed WebM) report Infinity here, so fall back to the server's duration; retry
  // once playback can start in case the first assignment was ignored.
  let pendingSeek = 0;
  const applySeek = () => {
    if (!(pendingSeek > 0)) return;
    const dur = Number.isFinite(video.duration) ? video.duration : (src?.duration || 0) - (src?.base || 0);
    const t = dur > 0 ? Math.min(pendingSeek, Math.max(0, dur - 0.5)) : pendingSeek;
    try {
      video.currentTime = t;
    } catch { /* not seekable yet */ }
    if (Math.abs(video.currentTime - t) < 1) pendingSeek = 0;
  };
  video.addEventListener('loadedmetadata', () => {
    art.hidden = !(src?.poster && video.videoWidth === 0);
    pendingSeek = src?.seek > 0 ? src.seek : 0;
    applySeek();
    syncTime();
  });
  video.addEventListener('canplay', applySeek);
  volume.addEventListener('input', () => {
    video.volume = clampVolume(volume.value);
    video.muted = video.volume === 0;
  });
  seek.addEventListener('input', () => {
    seeking = true;
    const dur = src?.duration || 0;
    const t = (Number(seek.value) / 1000) * dur;
    timeLabel.textContent = `${formatDuration(t)} / ${formatDuration(dur)}`;
    seek.style.setProperty('--fill', `${(Number(seek.value) / 10)}%`);
  });
  seek.addEventListener('change', () => {
    seeking = false;
    const dur = src?.duration || 0;
    if (dur) api.seekTo((Number(seek.value) / 1000) * dur);
  });
  unmutePill.addEventListener('click', () => {
    video.muted = false;
    if (video.volume === 0) video.volume = 1;
  });
  const onFs = () => {
    const fs = document.fullscreenElement === stage;
    fsBtn.replaceChildren(tvIcon(fs ? 'minimize' : 'maximize', 18));
    stage.classList.toggle('is-fullscreen', fs);
  };
  document.addEventListener('fullscreenchange', onFs);

  async function play() {
    try {
      await video.play();
    } catch (err) {
      if (err?.name === 'NotAllowedError' && !video.muted) {
        // Autoplay with sound was blocked: start muted and offer to unmute.
        video.muted = true;
        unmutePill.hidden = false;
        try {
          await video.play();
        } catch { /* still blocked: the user presses play */ }
      }
    }
  }

  const api = {
    el: stage,
    video,
    overlays,
    get source() { return src; },
    position,
    /** Load a source {url, kind, base, seek, duration, poster?} and start playing. */
    load(s) {
      src = { base: 0, seek: 0, ...s };
      pausedSince = 0; // a new source is never stale
      spinner.hidden = false;
      // Artwork for audio-only media is revealed once the metadata shows there is no video.
      art.hidden = true;
      if (s.poster) artImg.src = s.poster;
      else artImg.removeAttribute('src');
      video.src = s.url;
      alignCues();
      seek.disabled = live || !src.duration;
      play();
      syncTime();
    },
    play,
    stop() {
      art.hidden = true;
      video.pause();
      video.removeAttribute('src');
      video.load();
      src = null;
    },
    togglePlay() {
      if (video.paused) play();
      else video.pause();
    },
    toggleMute() {
      video.muted = !video.muted;
      if (!video.muted && video.volume === 0) video.volume = 0.5;
    },
    changeVolume(delta) {
      video.muted = false;
      video.volume = clampVolume(video.volume + delta);
    },
    seekTo(sec) {
      if (live || !src) return;
      const dur = src.duration || 0;
      const t = Math.max(0, dur ? Math.min(sec, dur - 0.5) : sec);
      if (src.kind === 'transcode' && opts.sourceAt) {
        api.load({ ...opts.sourceAt(t), duration: src.duration });
      } else {
        video.currentTime = t - (src.base || 0);
      }
    },
    seekBy(delta) {
      api.seekTo(position() + delta);
    },
    async toggleFullscreen() {
      try {
        if (document.fullscreenElement) await document.exitFullscreen();
        else await stage.requestFullscreen();
      } catch { /* not allowed */ }
    },
    async togglePiP() {
      try {
        if (document.pictureInPictureElement) await document.exitPictureInPicture();
        else if (video.readyState >= 1) await video.requestPictureInPicture();
      } catch { /* unsupported */ }
    },
    /** Replace subtitle tracks: [{src, label, srclang, default}]; a default track is shown. */
    setTracks(tracks) {
      for (const t of video.querySelectorAll('track')) t.remove();
      for (const t of tracks || []) {
        const el = h('track', { kind: 'subtitles', src: t.src, label: t.label || t.srclang || 'Subtitles', srclang: t.srclang || undefined });
        if (t.default) el.default = true;
        el.addEventListener('load', alignCues);
        video.append(el);
        if (t.default && el.track) el.track.mode = 'showing';
      }
      ccBtn.hidden = !(tracks && tracks.length);
      api.syncCaptions();
    },
    /** The subtitle track being shown ({label, srclang}) or null. */
    caption() {
      const t = [...video.textTracks].find((x) => x.mode === 'showing');
      return t ? { label: t.label, srclang: t.language || undefined } : null;
    },
    syncCaptions() {
      const on = [...video.textTracks].some((t) => t.mode === 'showing');
      ccBtn.classList.toggle('is-on', on);
      ccBtn.setAttribute('aria-pressed', String(on));
    },
    /** Cycle subtitles: off → track 1 → track 2 → … → off. */
    cycleCaptions() {
      const tracks = [...video.textTracks];
      if (!tracks.length) return null;
      const i = tracks.findIndex((t) => t.mode === 'showing');
      tracks.forEach((t) => { t.mode = 'disabled'; });
      const next = i + 1 < tracks.length ? tracks[i + 1] : null;
      if (next) next.mode = 'showing';
      api.syncCaptions();
      opts.onCaptions?.(next ? { label: next.label, srclang: next.language || undefined } : null);
      return next ? next.label : null;
    },
    setLiveProgress(fn) {
      liveProgress = fn;
      syncTime();
    },
    setTimeText(text) {
      timeLabel.textContent = text;
    },
    showControls,
    destroy() {
      destroyed = true;
      clearTimeout(hideTimer);
      document.removeEventListener('fullscreenchange', onFs);
      if (document.pictureInPictureElement === video) document.exitPictureInPicture().catch(() => {});
      if (document.fullscreenElement === stage) document.exitFullscreen().catch(() => {});
      api.stop();
    },
  };
  syncVolumeUI();
  syncPlayUI();
  showControls();
  return api;
}
