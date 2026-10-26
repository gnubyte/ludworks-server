// /tv/play/<file id> — on-demand player for library items: resumes from the saved
// progress, reports progress every 10 s and on pause/leave, offers subtitles (<track> VTT),
// falls back to the transcoder, and counts down into the next episode of a show.

import { h, isEditable } from '../../core/dom.js';
import { formatDuration } from '../../core/format.js';
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

  /* ---------------------------------------------------------------- layout */
  const status = h('div', { class: 'tv-status', hidden: true, role: 'status' });
  const top = h('div', { class: 'tv-vod-top' },
    h('a', { class: 'tv-back btn btn-ghost btn-sm', href: backHref, aria: { label: 'Back to the item' } }, tvIcon('arrow-left', 16), h('span', { class: 'hide-sm' }, 'Back')),
    h('div', { class: 'tv-vod-title' }, h('div', { class: 'truncate bold' }, title), sub ? h('div', { class: 'truncate small tv-vod-sub' }, sub) : null));
  const upNext = h('div', { class: 'tv-upnext', hidden: true, role: 'dialog', aria: { label: 'Up next' } });
  const nextBtn = iconButton({ icon: 'skip-next', title: 'Next episode (n)', className: 'tv-ctl', onClick: () => goNext() });
  nextBtn.hidden = true;
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
  player.load({ ...firstSource, duration, poster: stageArt });
  if (start > 0 && !explicit) {
    ui.toast(`Resumed at ${formatDuration(start)}`, {
      timeout: 6000,
      action: { label: 'Start over', onClick: () => player.seekTo(0) },
    });
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
    const pos = player.position();
    if (!(pos >= 0) || (!force && Math.abs(pos - lastSaved) < 2)) return;
    lastSaved = pos;
    try {
      await api.progress(id, pos, isCompleted(pos, duration));
    } catch { /* best effort */ }
  }
  function saveOnHide() {
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
    await save(true);
    navigate(`/tv/play/${next.id}`);
  }

  /* --------------------------------------------------------------- keyboard */
  function onKeyCapture(e) {
    const before = e.defaultPrevented;
    onKey(e);
    if (!before && e.defaultPrevented) e.stopPropagation();
  }

  function onKey(e) {
    if (e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey || isEditable(e.target)) return;
    if (document.documentElement.classList.contains('modal-open') || document.documentElement.classList.contains('palette-open')) return;
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
    save(true);
    clearInterval(saveTimer);
    clearInterval(countdown?.timer);
    document.removeEventListener('keydown', onKeyCapture, true);
    window.removeEventListener('pagehide', saveOnHide);
    player.destroy();
  };
}
