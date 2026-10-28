// /tv/play/<file id> — on-demand player for library items: resumes from the saved
// progress, reports progress every 10 s and on pause/leave, offers subtitles (<track> VTT),
// falls back to the transcoder, and counts down into the next episode of a show.
// Casting (core/cast.js): the cast button hands the item and position to a Chromecast/AirPlay
// device; while casting the stage shows the cast controls and the keys control the device (the
// server records progress); stopping resumes here at the cast position. Leaving the page keeps
// the device playing, and coming back to the same item re-attaches to that session.

import { h, isEditable } from '../../core/dom.js';
import { formatDuration } from '../../core/format.js';
import { castButton, castControls, attachSession, findSession, errorText, castErrorMessage } from '../../core/cast.js';
import { createPlayer } from './playercore.js';
import { iconButton, poster } from './shared.js';
import { resumeFrom, isCompleted, nextEpisode, episodeCode, sizedPoster } from './model.js';
import { tvIcon } from './icons.js';

const SAVE_EVERY_MS = 10000;
const COUNTDOWN_S = 10;

function mediaTitle(item) {
  const m = item?.media || {};
  if (m.show_title) {
    const code = episodeCode(m);
    return { title: m.show_title, sub: [code, m.episode_title || (m.title !== m.show_title ? m.title : '')].filter(Boolean).join(' · ') };
  }
  return { title: m.title || item?.name || 'Untitled', sub: m.year ? String(m.year) : '' };
}

export async function render(host, { ctx, api, ui, signal, navigate, route }) {
  const id = route.id;
  const enc = encodeURIComponent(id);
  const backHref = `/tv/library/item/${id}`;
  const page = h('div', { class: 'tv-watch tv-vod' });
  host.append(page);
  page.append(h('div', { class: 'tv-vod-loading' }, ui.loadingBlock('Loading…')));

  let item;
  try {
    item = await api.media(id, signal);
  } catch (err) {
    if (signal.aborted) return null;
    page.replaceChildren(h('div', { class: 'page' }, ui.emptyState({
      icon: 'film', title: err?.status === 404 ? 'Not found' : 'Couldn’t load this',
      message: err?.status === 404 ? 'This item does not exist or you cannot access it.' : ui.errorMessage(err),
      action: ui.button({ label: 'Back to the library', icon: 'arrow-left', href: '/tv/library' }),
    })));
    return null;
  }
  const m = item.media || {};
  const { title, sub } = mediaTitle(item);
  ctx.setTitle(sub ? `${title} · ${sub}` : title);
  const duration = Number(m.duration_seconds) || 0;
  const streamUrl = `/api/v1/media/${enc}/stream`;
  const stageArt = sizedPoster(m.poster_url, 'lg');
  const transcodeAt = (t) => ({ url: `/api/v1/media/${enc}/transcode?start=${Math.floor(t * 10) / 10}`, kind: 'transcode', base: Math.floor(t * 10) / 10, seek: 0 });

  let triedTranscode = false;
  let lastSaved = -1;
  let next = null;
  let countdown = null; // {timer, left}
  let dismissedNext = false;
  let cast = null; // {controller, controls, overlay, offEnded} while a device plays this item

  /* ---------------------------------------------------------------- layout */
  const status = h('div', { class: 'tv-status', hidden: true, role: 'status' });
  const top = h('div', { class: 'tv-vod-top' },
    h('a', { class: 'tv-back btn btn-ghost btn-sm', href: backHref, aria: { label: 'Back to the item' } }, tvIcon('arrow-left', 16), h('span', { class: 'hide-sm' }, 'Back')),
    h('div', { class: 'tv-vod-title' }, h('div', { class: 'truncate bold' }, title), sub ? h('div', { class: 'truncate small tv-vod-sub' }, sub) : null));
  const upNext = h('div', { class: 'tv-upnext', hidden: true, role: 'dialog', aria: { label: 'Up next' } });
  const nextBtn = iconButton({ icon: 'skip-next', title: 'Next episode (n)', className: 'tv-ctl', onClick: () => goNext() });
  nextBtn.hidden = true;
  const castBtn = castButton({
    api: ctx.api, ui, className: 'tv-ctl',
    // Moving a cast to another device continues where the device is, else where this player is.
    getItems: () => ({ items: [{ kind: 'media', id }], positionSeconds: Math.max(0, cast ? cast.controller.position() : player.position()) }),
    beforeStart: () => save(true),
    media: () => player.video,
    onSession: (c) => beginCast(c),
  });
  const player = createPlayer({
    label: title,
    onEnded: () => onEnded(),
    onError: () => onError(),
    onTime: (pos) => onTime(pos),
    sourceAt: transcodeAt,
    extraControls: [
      iconButton({ icon: 'rewind', title: 'Back 10 s (←)', className: 'tv-ctl', onClick: () => player.seekBy(-10) }),
      iconButton({ icon: 'forward', title: 'Forward 30 s (→)', className: 'tv-ctl', onClick: () => player.seekBy(30) }),
      nextBtn,
      castBtn,
    ],
  });
  player.overlays.append(top, status, upNext);
  page.replaceChildren(player.el);

  player.setTracks((m.subtitles || []).map((s, i) => ({
    src: `/api/v1/media/${enc}/subtitles/${encodeURIComponent(s.index)}.vtt`,
    label: s.label || s.language || `Track ${i + 1}`,
    srclang: /^[a-z]{2,3}(-[A-Za-z]{2})?$/.test(s.language || '') ? s.language : undefined,
  })));

  /* --------------------------------------------------------------- playback */
  // ?t=<seconds> (library "play from …" links) wins over the saved progress.
  const tq = ctx.query?.t;
  const explicit = tq !== undefined && tq !== '' && Number.isFinite(Number(tq)) && Number(tq) >= 0;
  const start = explicit ? Math.min(Number(tq), duration ? Math.max(0, duration - 1) : Number(tq)) : resumeFrom(item);
  const firstSource = m.browser_playable === false ? transcodeAt(start) : { url: streamUrl, kind: 'direct', base: 0, seek: start };
  triedTranscode = firstSource.kind === 'transcode';
  // Still casting this item from an earlier visit? Then show the device instead of playing here.
  const casting = await findSession(ctx.api, (sess, it) => it?.kind === 'media' && it.id === id);
  if (signal.aborted) { // navigated away meanwhile
    player.destroy();
    return null;
  }
  if (casting) {
    beginCast(attachSession(ctx.api, casting));
  } else {
    player.load({ ...firstSource, duration, poster: stageArt });
    if (start > 0 && !explicit) {
      ui.toast(`Resumed at ${formatDuration(start)}`, {
        timeout: 6000,
        action: { label: 'Start over', onClick: () => player.seekTo(0) },
      });
    }
  }

  /* ---------------------------------------------------------------- casting */
  function teardownCast() {
    if (!cast) return null;
    const c = cast;
    cast = null;
    c.offEnded();
    c.controls.destroy();
    c.overlay.remove();
    player.el.classList.remove('is-casting');
    castBtn.setCasting(null);
    return c;
  }

  /** A device plays this item now: hand over from the local player. */
  function beginCast(c) {
    if (cast) {
      if (cast.controller.id === c.id) return;
      const old = teardownCast(); // moved to another device: end the previous session
      if (!old.controller.ended) old.controller.stop().catch(() => {});
    }
    const controls = castControls(c, { variant: 'overlay', onStop: (where) => endCast(where, null) });
    const backdrop = stageArt ? h('img', { class: 'tv-cast-backdrop', src: stageArt, alt: '', draggable: 'false' }) : null;
    const overlay = h('div', { class: 'tv-cast' }, backdrop, controls.el);
    // Set before pausing: the pause handler must not save the local position over the cast's.
    cast = { controller: c, controls, overlay, offEnded: c.on('ended', ({ reason, byUser }) => { if (!byUser) endCast({ positionSeconds: c.position(), item: c.item() }, reason); }) };
    player.video.pause();
    clearInterval(countdown?.timer);
    countdown = null;
    upNext.hidden = true;
    status.hidden = true;
    player.overlays.append(overlay);
    player.el.classList.add('is-casting');
    castBtn.setCasting(c.device);
  }

  /** Casting ended: back to the local player at the cast position (playing when the user stopped it). */
  function endCast(where, reason) {
    if (!teardownCast()) return;
    const it = where?.item;
    const pos = Number(where?.positionSeconds);
    const at = (!it || (it.kind === 'media' && it.id === id)) && Number.isFinite(pos) && pos >= 0 ? pos : player.position();
    if (!reason) {
      resumeLocal(at, true);
      return;
    }
    resumeLocal(at, false);
    ui.toast(errorText(reason), {
      type: reason === 'stopped' ? 'info' : 'warning', timeout: 9000,
      action: { label: 'Play here', onClick: () => resumeLocal(player.position(), true) },
    });
  }

  function resumeLocal(pos, autoplay) {
    const t = Math.max(0, duration ? Math.min(pos, Math.max(0, duration - 1)) : pos);
    if (!player.source) {
      const src = triedTranscode ? transcodeAt(t) : { url: streamUrl, kind: 'direct', base: 0, seek: t };
      player.load({ ...src, duration, poster: stageArt });
    } else {
      player.seekTo(t);
      if (autoplay) player.play();
    }
    if (!autoplay) player.video.pause();
    player.el.focus({ preventScroll: true });
  }

  /** Run a cast control from a key; failures become a toast (the controls show their own). */
  function castRun(p) {
    p?.catch?.((err) => ui.toast(castErrorMessage(err), { type: 'warning', timeout: 4000 }));
  }

  function onError() {
    if (!triedTranscode) {
      triedTranscode = true;
      player.load({ ...transcodeAt(player.position()), duration, poster: stageArt });
      return;
    }
    player.stop();
    status.hidden = false;
    status.replaceChildren(h('div', { class: 'tv-status-card' },
      h('span', { class: 'tv-status-icon' }, tvIcon('alert', 22)),
      h('div', { class: 'tv-status-title' }, 'This file can’t play here'),
      h('div', { class: 'tv-status-msg' }, 'Your browser cannot play it directly and the server could not convert it (converting needs ffmpeg on the server).'),
      h('div', { class: 'row wrap tv-status-actions' },
        ui.button({ label: 'Download instead', icon: 'download', href: `/api/v1/files/${enc}/content?download=1` }),
        ui.button({ label: 'Back', icon: 'arrow-left', href: backHref }))));
  }

  /* ----------------------------------------------------------- progress */
  async function save(force = false) {
    if (cast) return; // the server records the device's progress
    // A direct stream reports 0 until its metadata (and the pending seek) arrived: not a position.
    if (player.video.readyState < 1 && !(player.source?.base > 0)) return;
    const pos = player.position();
    if (!(pos >= 0) || (!force && Math.abs(pos - lastSaved) < 2)) return;
    lastSaved = pos;
    try {
      await api.progress(id, pos, isCompleted(pos, duration));
    } catch { /* best effort */ }
  }
  function saveOnHide() {
    if (cast) return;
    try {
      const pos = player.position();
      fetch(`/api/v1/media/${enc}/progress`, {
        method: 'PUT', keepalive: true, credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': ctx.api.csrf() || '' },
        body: JSON.stringify({ position_seconds: Math.max(0, pos), completed: isCompleted(pos, duration) }),
      }).catch(() => {});
    } catch { /* ignore */ }
  }
  player.video.addEventListener('pause', () => save(true));
  const saveTimer = setInterval(() => { if (!player.video.paused) save(); }, SAVE_EVERY_MS);

  /* ----------------------------------------------------------- next episode */
  if (m.show_title) {
    api.mediaList({ show: m.show_title, sort: 'episode', limit: 200 }, signal).then((page2) => {
      next = nextEpisode(page2?.items || [], item);
      nextBtn.hidden = !next;
    }).catch(() => {});
  }

  function onTime(pos) {
    const dur = duration || player.video.duration || 0;
    if (next && !dismissedNext && !countdown && dur >= 30 && dur - pos <= 15) showUpNext(false);
  }

  function onEnded() {
    save(true);
    if (next && !dismissedNext) showUpNext(true);
    else player.showControls();
  }

  function showUpNext(startCountdown) {
    if (!next) return;
    const nm = mediaTitle(next);
    const left = h('span', { class: 'nums' }, String(COUNTDOWN_S));
    upNext.hidden = false;
    upNext.replaceChildren(
      poster(next.media?.poster_url || null, { className: 'tv-upnext-poster', iconName: 'tv' }),
      h('div', { class: 'tv-upnext-body' },
        h('div', { class: 'tv-upnext-label small' }, startCountdown ? ['Next episode in ', left, ' s'] : 'Up next'),
        h('div', { class: 'truncate bold' }, nm.sub || nm.title),
        h('div', { class: 'row wrap' },
          ui.button({ label: 'Play now', icon: 'play', variant: 'primary', size: 'sm', onClick: () => goNext() }),
          ui.button({ label: 'Cancel', size: 'sm', variant: 'ghost', onClick: () => cancelNext() }))));
    if (startCountdown) {
      clearInterval(countdown?.timer);
      countdown = { left: COUNTDOWN_S, timer: 0 };
      countdown.timer = setInterval(() => {
        countdown.left -= 1;
        left.textContent = String(Math.max(0, countdown.left));
        if (countdown.left <= 0) goNext();
      }, 1000);
    } else {
      countdown = { left: COUNTDOWN_S, timer: 0, armed: true };
    }
  }

  function cancelNext() {
    clearInterval(countdown?.timer);
    countdown = null;
    dismissedNext = true;
    upNext.hidden = true;
  }

  async function goNext() {
    if (!next) return;
    clearInterval(countdown?.timer);
    if (cast) {
      // Keep casting: the device plays the next episode and its page re-attaches to the session.
      try {
        await cast.controller.replaceItems([{ kind: 'media', id: next.id }]);
      } catch (err) {
        ui.toast(castErrorMessage(err), { type: 'warning' });
        return;
      }
    } else {
      await save(true);
    }
    navigate(`/tv/play/${next.id}`);
  }

  /* --------------------------------------------------------------- keyboard */
  function onKeyCapture(e) {
    const before = e.defaultPrevented;
    onKey(e);
    if (!before && e.defaultPrevented) e.stopPropagation();
  }

  /** Player keys while casting: they control the device. */
  function onCastKey(e) {
    const c = cast.controller;
    const onInput = e.target instanceof HTMLInputElement;
    const onButton = e.target instanceof HTMLButtonElement || onInput;
    switch (e.key) {
      case ' ': if (onButton) return; e.preventDefault(); castRun(c.toggle()); break;
      case 'k': case 'K': e.preventDefault(); castRun(c.toggle()); break;
      case 'ArrowLeft': case 'j': case 'J': if (onInput) return; e.preventDefault(); if (c.canSeek()) castRun(c.seek(Math.max(0, c.position() - 10))); break;
      case 'ArrowRight': case 'l': case 'L': if (onInput) return; e.preventDefault(); if (c.canSeek()) castRun(c.seek(c.position() + (e.key === 'ArrowRight' ? 30 : 10))); break;
      case 'ArrowUp': if (onInput) return; e.preventDefault(); castRun(c.setVolume(Math.min(1, (Number(c.session.volume) || 0) + 0.1))); break;
      case 'ArrowDown': if (onInput) return; e.preventDefault(); castRun(c.setVolume(Math.max(0, (Number(c.session.volume) || 0) - 0.1))); break;
      case 'm': case 'M': e.preventDefault(); castRun(c.setMuted(!c.session.muted)); break;
      case 'n': case 'N': if (next) { e.preventDefault(); goNext(); } break;
      case 'Escape': if (!document.fullscreenElement) { e.preventDefault(); navigate(backHref); } break;
      default:
        if (/^[0-9]$/.test(e.key) && c.canSeek() && c.duration()) { e.preventDefault(); castRun(c.seek((Number(e.key) / 10) * c.duration())); }
    }
  }

  function onKey(e) {
    if (e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey || isEditable(e.target)) return;
    if (document.documentElement.classList.contains('modal-open') || document.documentElement.classList.contains('palette-open')) return;
    if (cast) {
      onCastKey(e);
      return;
    }
    const onButton = e.target instanceof HTMLButtonElement || e.target instanceof HTMLInputElement;
    switch (e.key) {
      case ' ': if (onButton) return; e.preventDefault(); player.togglePlay(); break;
      case 'k': case 'K': e.preventDefault(); player.togglePlay(); break;
      case 'ArrowLeft': case 'j': case 'J': if (e.target instanceof HTMLInputElement) return; e.preventDefault(); player.seekBy(-10); break;
      case 'ArrowRight': case 'l': case 'L': if (e.target instanceof HTMLInputElement) return; e.preventDefault(); player.seekBy(e.key === 'ArrowRight' ? 30 : 10); break;
      case 'ArrowUp': e.preventDefault(); player.changeVolume(0.1); break;
      case 'ArrowDown': e.preventDefault(); player.changeVolume(-0.1); break;
      case 'm': case 'M': e.preventDefault(); player.toggleMute(); break;
      case 'f': case 'F': e.preventDefault(); player.toggleFullscreen(); break;
      case 'p': case 'P': e.preventDefault(); player.togglePiP(); break;
      case 'c': case 'C': { e.preventDefault(); const l = player.cycleCaptions(); if (l !== null) ui.toast(l ? `Subtitles: ${l}` : 'Subtitles off', { timeout: 1500 }); break; }
      case 'n': case 'N': if (next) { e.preventDefault(); goNext(); } break;
      case 'Escape': if (!document.fullscreenElement) { e.preventDefault(); navigate(backHref); } break;
      default:
        if (/^[0-9]$/.test(e.key) && duration) { e.preventDefault(); player.seekTo((Number(e.key) / 10) * duration); }
    }
  }
  // Capture phase: the player's keys win over the shell's global shortcuts ("?", "g x" sequences).
  document.addEventListener('keydown', onKeyCapture, true);
  window.addEventListener('pagehide', saveOnHide);
  player.el.focus({ preventScroll: true });

  return () => {
    // Leaving while casting keeps the device playing (and the local position stays unsaved: the
    // server records the device's progress).
    const wasCasting = !!cast;
    teardownCast()?.controller.detach();
    castBtn.destroy();
    if (!wasCasting) save(true);
    clearInterval(saveTimer);
    clearInterval(countdown?.timer);
    document.removeEventListener('keydown', onKeyCapture, true);
    window.removeEventListener('pagehide', saveOnHide);
    player.destroy();
  };
}
