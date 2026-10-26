// /tv/watch/<channel id> — the old-school TV player. Tunes with /now and seeks to the live
// offset, auto-advances when a program ends (re-syncing with /now), zaps with Up/Down,
// PageUp/PageDown, on-screen buttons or swipes, takes channel numbers from the keyboard
// with an on-screen display, shows an info banner (on tune and on "i") and a mini guide
// ("g"), falls back to the transcoder when the browser cannot play a program, and
// remembers the last channel. On-demand channels save the viewer's position. Programs'
// subtitle tracks are offered (c / the CC button); the chosen language carries over to the
// next programs and channels.

import { h, cls, isEditable } from '../../core/dom.js';
import { formatTime } from '../../core/format.js';
import { createPlayer } from './playercore.js';
import { channelLogo, programText, miniProgress, iconButton, timeRange, pref } from './shared.js';
import {
  sortChannels, channelStep, pushDigit, channelByNumber, liveOffset, chooseSource, programLabel, programProgress,
  currentAndNext, swipeDirection, parseRoute, stagePoster, programTracks, pickCaption, captionKey,
} from './model.js';
import { tvIcon } from './icons.js';

const BANNER_MS = 5000;
const OSD_COMMIT_MS = 1600;
const SAVE_EVERY_MS = 10000;
const CAPTIONS_PREF = 'lw.tv.captions';

const SHORTCUTS = [
  ['↑ / PgUp', 'Next channel'], ['↓ / PgDn', 'Previous channel'], ['0–9', 'Enter a channel number'],
  ['Enter', 'Tune to the typed number'], ['i', 'Info banner'], ['g', 'Mini guide'], ['m', 'Mute'],
  ['+ / −', 'Volume'], ['k / Space', 'Pause / resume'], ['f', 'Fullscreen'], ['p', 'Picture in picture'],
  ['c', 'Subtitles'], ['?', 'This help'], ['Esc', 'Close / back'],
];

export async function render(host, vctx) {
  const { ctx, api, ui, signal, navigate } = vctx;
  let channels = [];
  let channel = null;
  let nowResp = null;
  let fetchedAt = 0;
  let forceTranscode = false;
  let tuneSeq = 0;
  let digits = '';
  let osdTimer = 0;
  let bannerTimer = 0;
  let endTimer = 0;
  let retryTimer = 0;
  let saveTimer = 0;
  let clockTimer = 0;
  let pausedAt = 0;

  /* ------------------------------------------------------------- overlays */
  const statusBox = h('div', { class: 'tv-status', hidden: true, role: 'status', aria: { live: 'polite' } });
  const osd = h('div', { class: 'tv-osd nums', hidden: true, aria: { live: 'assertive' } });
  const clock = h('span', { class: 'tv-banner-clock nums' });
  const bannerProgress = miniProgress('Program progress');
  const banner = h('div', { class: 'tv-banner', hidden: true, aria: { live: 'polite' } });
  const miniGuide = h('div', { class: 'tv-miniguide', hidden: true, role: 'dialog', aria: { label: 'Mini guide', modal: 'false' } });
  const help = h('div', { class: 'tv-help', hidden: true, role: 'dialog', aria: { label: 'Keyboard shortcuts' } },
    h('div', { class: 'tv-help-card' },
      h('div', { class: 'row between' }, h('h2', { class: 'tv-help-title' }, 'Keyboard shortcuts'),
        iconButton({ icon: 'x', title: 'Close', size: 'sm', onClick: () => toggleHelp(false) })),
      h('dl', { class: 'tv-help-list' }, SHORTCUTS.map(([k, d]) => [h('dt', {}, h('kbd', {}, k)), h('dd', {}, d)]))));
  const backBtn = h('a', { class: 'tv-back btn btn-ghost btn-sm', href: '/tv', aria: { label: 'Back to live TV' } }, tvIcon('arrow-left', 16), h('span', {}, 'Live TV'));
  const chUp = iconButton({ icon: 'channel-up', title: 'Next channel (↑)', className: 'tv-zap tv-zap-up', size: 'lg', onClick: () => zap(1) });
  const chDown = iconButton({ icon: 'channel-down', title: 'Previous channel (↓)', className: 'tv-zap tv-zap-down', size: 'lg', onClick: () => zap(-1) });

  const player = createPlayer({
    live: true,
    label: 'Live TV',
    onEnded: () => resync(true),
    onError: () => onMediaError(),
    // A transcode cut after a long pause (or a network error) restarts where it stopped.
    sourceAt: (t) => chooseSource(nowResp?.program || {}, t, { forceTranscode: true }),
    onCaptions: (t) => pref(CAPTIONS_PREF, t ? captionKey(t) : null),
    extraControls: [
      iconButton({ icon: 'channel-down', title: 'Previous channel (↓)', className: 'tv-ctl', onClick: () => zap(-1) }),
      iconButton({ icon: 'channel-up', title: 'Next channel (↑)', className: 'tv-ctl', onClick: () => zap(1) }),
      iconButton({ icon: 'info', title: 'Info (i)', className: 'tv-ctl', onClick: () => showBanner(true) }),
      iconButton({ icon: 'list', title: 'Mini guide (g)', className: 'tv-ctl', onClick: () => toggleGuide() }),
      iconButton({ icon: 'help', title: 'Shortcuts (?)', className: 'tv-ctl hide-sm', onClick: () => toggleHelp() }),
    ],
  });
  player.overlays.append(backBtn, chUp, chDown, statusBox, osd, banner, miniGuide, help);
  const page = h('div', { class: 'tv-watch' }, player.el);
  host.append(page);
  player.setLiveProgress(() => {
    const p = nowResp?.program;
    if (!p || !player.source) return 0;
    return Math.min(1, player.position() / (p.duration_seconds || 1));
  });

  /* --------------------------------------------------------------- status */
  function setStatus(content) {
    if (!content) {
      statusBox.hidden = true;
      statusBox.replaceChildren();
      return;
    }
    statusBox.hidden = false;
    statusBox.replaceChildren(content);
  }

  function statusCard({ iconName, title, message = '', actions = [] }) {
    return h('div', { class: 'tv-status-card' },
      h('span', { class: 'tv-status-icon' }, tvIcon(iconName, 22)),
      h('div', { class: 'tv-status-title' }, title),
      message ? h('div', { class: 'tv-status-msg' }, message) : null,
      actions.length ? h('div', { class: 'row wrap tv-status-actions' }, actions) : null);
  }

  /* --------------------------------------------------------------- banner */
  function renderBanner() {
    if (!channel) return;
    const p = nowResp?.program || null;
    const next = nowResp?.next?.[0] || null;
    bannerProgress.set(p ? Math.min(1, player.position() / (p.duration_seconds || 1)) : 0);
    bannerProgress.el.hidden = !p;
    clock.textContent = formatTime(new Date());
    banner.replaceChildren(
      h('div', { class: 'tv-banner-chan' },
        channelLogo(channel, { size: 'lg' }),
        h('div', { class: 'tv-banner-num nums' }, String(channel.number)),
        h('div', { class: 'tv-banner-name truncate' }, channel.name)),
      h('div', { class: 'tv-banner-now' },
        h('div', { class: 'tv-banner-label' }, channel.playback_mode === 'on_demand' ? 'Playing' : 'Now', p ? h('span', { class: 'faint' }, ` · ${timeRange(p)}`) : null),
        player.el.classList.contains('is-tuning') && !p ? h('div', { class: 'tv-prog-text tv-banner-prog' }, h('div', { class: 'tv-prog-title' }, 'Tuning…')) : programText(p, { className: 'tv-banner-prog' }),
        bannerProgress.el),
      h('div', { class: 'tv-banner-next hide-sm' },
        h('div', { class: 'tv-banner-label' }, 'Next', next ? h('span', { class: 'faint' }, ` · ${formatTime(next.starts_at)}`) : null),
        h('div', { class: 'truncate' }, next ? programLabel(next) : '—')),
      clock);
  }

  function showBanner(toggle = false) {
    if (toggle && !banner.hidden) {
      banner.hidden = true;
      return;
    }
    renderBanner();
    banner.hidden = false;
    clearTimeout(bannerTimer);
    bannerTimer = setTimeout(() => { banner.hidden = true; }, BANNER_MS);
  }

  /* --------------------------------------------------------------- tuning */
  async function loadChannels() {
    channels = sortChannels(await api.channels(signal));
  }

  async function tune(id, { replaceUrl = true } = {}) {
    const my = ++tuneSeq;
    clearTimeout(endTimer);
    clearTimeout(retryTimer);
    await savePosition();
    forceTranscode = false;
    channel = channels.find((c) => c.id === id) || null;
    if (!channel) {
      try {
        channel = await api.channel(id, signal);
      } catch (err) {
        if (signal.aborted || my !== tuneSeq) return;
        player.stop();
        setStatus(statusCard({
          iconName: 'antenna', title: 'No such channel',
          message: err?.status === 404 ? 'This channel does not exist or is not shared with you.' : ui.errorMessage(err),
          actions: [ui.button({ label: 'Back to live TV', icon: 'arrow-left', href: '/tv' })],
        }));
        return;
      }
    }
    pref('lw.tv.lastChannel', channel.id);
    ctx.setTitle(`${channel.number} · ${channel.name}`);
    if (replaceUrl && location.pathname !== `/tv/watch/${channel.id}`) navigate(`/tv/watch/${channel.id}`, { replace: true });
    nowResp = { channel, program: null, next: [] };
    player.el.classList.add('is-tuning');
    showBanner();
    setStatus(statusCard({ iconName: 'antenna', title: `Tuning to ${channel.number}`, message: channel.name }));
    await resync(false, my);
  }

  /** Fetch /now and play the current program from its live offset. */
  async function resync(afterEnd = false, my = tuneSeq) {
    if (!channel) return;
    clearTimeout(endTimer);
    clearTimeout(retryTimer);
    if (afterEnd && channel.playback_mode === 'on_demand' && nowResp?.program) {
      // Finished on demand: store the end so /now continues with the next program.
      await savePosition(nowResp.program.duration_seconds);
    }
    let r;
    try {
      r = await api.now(channel.id, signal);
    } catch (err) {
      if (signal.aborted || my !== tuneSeq) return;
      player.stop();
      setStatus(statusCard({
        iconName: 'alert-circle', title: 'Could not tune in', message: ui.errorMessage(err),
        actions: [ui.button({ label: 'Retry', icon: 'refresh', onClick: () => tune(channel.id) })],
      }));
      return;
    }
    if (my !== tuneSeq) return;
    nowResp = r;
    fetchedAt = performance.now();
    channel = r.channel;
    player.el.classList.remove('is-tuning');
    if (!r.program) {
      player.stop();
      player.setTracks([]);
      setStatus(statusCard({
        iconName: 'antenna', title: 'Nothing on air',
        message: 'This channel’s lineup has no playable programs yet. Media needs a known duration to be scheduled.',
        actions: channel.owner_id === ctx.user?.id
          ? [ui.button({ label: 'Edit lineup', icon: 'edit', href: `/tv/lineups/${channel.lineup_id}` })] : [],
      }));
      renderBanner();
      retryTimer = setTimeout(() => resync(false, my), 60000);
      return;
    }
    setStatus(null);
    startProgram();
    renderBanner();
    if (!afterEnd) showBanner();
  }

  function startProgram() {
    const p = nowResp.program;
    const offset = liveOffset(nowResp, fetchedAt, performance.now());
    const src = chooseSource(p, offset, { forceTranscode });
    player.load({ ...src, duration: p.duration_seconds, poster: stagePoster(p, channel) });
    const tracks = programTracks(p);
    const show = pickCaption(tracks, pref(CAPTIONS_PREF) || '');
    player.setTracks(tracks.map((t, i) => ({ ...t, default: i === show })));
    // Safety net: re-sync at the scheduled end even if the media runs longer.
    clearTimeout(endTimer);
    if (channel.playback_mode !== 'on_demand') {
      const remaining = Math.max(1, p.duration_seconds - offset) * 1000;
      endTimer = setTimeout(() => resync(true), remaining + 1500);
    }
  }

  function onMediaError() {
    const p = nowResp?.program;
    if (!p) return;
    const src = player.source;
    if (src?.kind === 'direct' && p.transcode_url && !forceTranscode) {
      forceTranscode = true;
      fetchedAt = performance.now();
      nowResp = { ...nowResp, offset_seconds: player.position() || liveOffset(nowResp, fetchedAt, performance.now()) };
      startProgram();
      return;
    }
    player.stop();
    setStatus(statusCard({
      iconName: 'alert', title: 'This program can’t play here',
      message: p.transcode_url
        ? 'Even the converted stream failed. The next program starts automatically.'
        : 'Your browser cannot play this file, and the server has no ffmpeg to convert it. The next program starts automatically.',
      actions: [
        ui.button({ label: 'Retry', icon: 'refresh', onClick: () => { setStatus(null); resync(false); } }),
        ui.button({ label: 'Next channel', icon: 'arrow-up', onClick: () => zap(1) }),
      ],
    }));
  }

  function zap(dir) {
    const next = channelStep(channels, channel?.id, dir);
    if (next && next.id !== channel?.id) tune(next.id);
  }

  /* ----------------------------------------------------- number entry OSD */
  function typeDigit(d) {
    digits = pushDigit(digits, d);
    osd.hidden = !digits;
    osd.textContent = digits ? digits.padEnd(Math.max(digits.length, 1), '-') : '';
    osd.classList.remove('is-miss');
    clearTimeout(osdTimer);
    osdTimer = setTimeout(commitDigits, OSD_COMMIT_MS);
  }

  function commitDigits() {
    clearTimeout(osdTimer);
    if (!digits) return false;
    const target = channelByNumber(channels, digits);
    const typed = digits;
    digits = '';
    if (target) {
      osd.hidden = true;
      if (target.id !== channel?.id) tune(target.id);
      else showBanner();
    } else {
      osd.textContent = `${typed} —`;
      osd.classList.add('is-miss');
      osd.setAttribute('aria-label', `No channel ${typed}`);
      osdTimer = setTimeout(() => { osd.hidden = true; osd.classList.remove('is-miss'); }, 1400);
    }
    return true;
  }

  /* ------------------------------------------------------------ mini guide */
  let guideRows = [];
  let guideSel = 0;
  async function toggleGuide(force) {
    const open = force ?? miniGuide.hidden;
    if (!open) {
      miniGuide.hidden = true;
      player.el.classList.remove('keep-controls', 'guide-open');
      player.el.focus({ preventScroll: true });
      return;
    }
    miniGuide.hidden = false;
    toggleHelp(false);
    player.el.classList.add('keep-controls', 'guide-open');
    miniGuide.replaceChildren(h('div', { class: 'tv-miniguide-head' }, h('span', { class: 'bold' }, 'Channels'),
      iconButton({ icon: 'x', title: 'Close', size: 'sm', onClick: () => toggleGuide(false) })), ui.loadingBlock('Loading…'));
    try {
      const g = await api.guide({ start: new Date(Date.now() - 1000).toISOString(), hours: 1 }, signal);
      guideRows = g.channels;
    } catch (err) {
      if (signal.aborted) return;
      guideRows = channels.map((c) => ({ channel: c, programs: [] }));
    }
    if (miniGuide.hidden) return;
    const list = h('div', { class: 'tv-miniguide-list', role: 'listbox', aria: { label: 'Channels' } });
    guideSel = Math.max(0, guideRows.findIndex((r) => r.channel.id === channel?.id));
    guideRows.forEach((r, i) => {
      const { current } = currentAndNext(r.programs, Date.now());
      const prog = miniProgress('Progress');
      prog.set(programProgress(current, Date.now()));
      const item = h('div', {
        class: cls('tv-miniguide-item', { 'is-current': r.channel.id === channel?.id }), role: 'option', id: `tvmg-${i}`,
        aria: { selected: String(i === guideSel) },
      },
      channelLogo(r.channel, { size: 'sm' }),
      h('span', { class: 'tv-miniguide-num nums' }, String(r.channel.number)),
      h('span', { class: 'tv-miniguide-text' },
        h('span', { class: 'truncate bold' }, r.channel.name),
        h('span', { class: 'truncate small muted' }, current ? programLabel(current) : 'Nothing on air'),
        current ? prog.el : null));
      item.addEventListener('click', () => { toggleGuide(false); if (r.channel.id !== channel?.id) tune(r.channel.id); });
      list.append(item);
    });
    list.tabIndex = 0;
    miniGuide.replaceChildren(miniGuide.firstElementChild, list);
    selectGuide(guideSel);
    list.focus({ preventScroll: true });
  }

  function selectGuide(i) {
    const items = miniGuide.querySelectorAll('.tv-miniguide-item');
    if (!items.length) return;
    guideSel = (i + items.length) % items.length;
    items.forEach((el, k) => el.setAttribute('aria-selected', String(k === guideSel)));
    const list = miniGuide.querySelector('.tv-miniguide-list');
    list?.setAttribute('aria-activedescendant', `tvmg-${guideSel}`);
    items[guideSel].scrollIntoView({ block: 'nearest' });
  }

  function toggleHelp(force) {
    const open = force ?? help.hidden;
    help.hidden = !open;
    player.el.classList.toggle('keep-controls', open || !miniGuide.hidden);
    if (open) help.querySelector('button')?.focus();
  }

  /* -------------------------------------------------------------- keyboard */
  function onKeyCapture(e) {
    const before = e.defaultPrevented;
    onKey(e);
    if (!before && e.defaultPrevented) e.stopPropagation();
  }

  function onKey(e) {
    if (e.defaultPrevented || e.ctrlKey || e.metaKey || e.altKey) return;
    if (isEditable(e.target) || document.documentElement.classList.contains('modal-open') || document.documentElement.classList.contains('palette-open')) return;
    if (!page.isConnected) return;
    const k = e.key;
    if (!miniGuide.hidden) {
      if (k === 'ArrowDown' || k === 'ArrowUp') { e.preventDefault(); selectGuide(guideSel + (k === 'ArrowDown' ? 1 : -1)); return; }
      if (k === 'Enter') {
        e.preventDefault();
        const r = guideRows[guideSel];
        toggleGuide(false);
        if (r && r.channel.id !== channel?.id) tune(r.channel.id);
        return;
      }
      if (k === 'Escape' || k === 'g') { e.preventDefault(); toggleGuide(false); return; }
    }
    if (!help.hidden && (k === 'Escape' || k === '?')) { e.preventDefault(); toggleHelp(false); return; }
    if (/^[0-9]$/.test(k)) { e.preventDefault(); typeDigit(k); return; }
    switch (k) {
      case 'ArrowUp': case 'PageUp': case 'ChannelUp': e.preventDefault(); zap(1); break;
      case 'ArrowDown': case 'PageDown': case 'ChannelDown': e.preventDefault(); zap(-1); break;
      case 'Enter': if (!(e.target instanceof HTMLButtonElement || e.target instanceof HTMLAnchorElement)) { e.preventDefault(); if (!commitDigits()) showBanner(true); } break;
      case 'Backspace': if (digits) { e.preventDefault(); digits = digits.slice(0, -1); if (digits) osd.textContent = digits; else osd.hidden = true; } break;
      case 'i': case 'I': e.preventDefault(); showBanner(true); break;
      case 'g': case 'G': e.preventDefault(); toggleGuide(); break;
      case 'm': case 'M': e.preventDefault(); player.toggleMute(); break;
      case 'f': case 'F': e.preventDefault(); player.toggleFullscreen(); break;
      case 'p': case 'P': e.preventDefault(); player.togglePiP(); break;
      case 'c': case 'C': { e.preventDefault(); const label = player.cycleCaptions(); if (label !== null) ui.toast(label ? `Subtitles: ${label}` : 'Subtitles off', { timeout: 1500 }); break; }
      case '?': e.preventDefault(); toggleHelp(); break;
      case '+': case '=': e.preventDefault(); player.changeVolume(0.1); break;
      case '-': case '_': e.preventDefault(); player.changeVolume(-0.1); break;
      case 'k': case ' ': if (e.target instanceof HTMLButtonElement && k === ' ') break; e.preventDefault(); togglePause(); break;
      case 'Escape':
        if (!osd.hidden) { digits = ''; osd.hidden = true; } else if (!banner.hidden) banner.hidden = true;
        else if (!document.fullscreenElement) navigate('/tv');
        break;
      default:
    }
  }

  function togglePause() {
    const v = player.video;
    if (v.paused) {
      // Live TV never really pauses: coming back re-joins the broadcast (unless on demand).
      if (channel?.playback_mode !== 'on_demand' && pausedAt && performance.now() - pausedAt > 2000) resync(false);
      else player.play();
      pausedAt = 0;
    } else {
      pausedAt = performance.now();
      v.pause();
      savePosition();
    }
  }

  /* ---------------------------------------------------------------- swipes */
  let swipe = null;
  player.el.addEventListener('pointerdown', (e) => {
    if (e.pointerType !== 'touch' || e.target.closest('.tv-controls,.tv-miniguide,.tv-help')) return;
    swipe = { x: e.clientX, y: e.clientY, t: performance.now() };
  });
  player.el.addEventListener('pointerup', (e) => {
    if (!swipe || e.pointerType !== 'touch') return;
    const dir = performance.now() - swipe.t < 800 ? swipeDirection(e.clientX - swipe.x, e.clientY - swipe.y, 60) : null;
    swipe = null;
    if (dir === 'up') zap(1);
    else if (dir === 'down') zap(-1);
    else if (dir === 'left' || dir === 'right') showBanner();
  });
  player.el.addEventListener('pointercancel', () => { swipe = null; });

  /* ------------------------------------------------------------- positions */
  async function savePosition(offset) {
    if (channel?.playback_mode !== 'on_demand' || !nowResp?.program || !Number.isInteger(nowResp.index)) return;
    const pos = offset ?? player.position();
    if (!(pos >= 0)) return;
    try {
      await api.setPosition(channel.id, nowResp.index, pos);
    } catch { /* best effort */ }
  }

  function saveOnHide() {
    if (channel?.playback_mode !== 'on_demand' || !nowResp?.program || !Number.isInteger(nowResp.index)) return;
    try {
      fetch(`/api/v1/tv/channels/${encodeURIComponent(channel.id)}/position`, {
        method: 'PUT', keepalive: true, credentials: 'same-origin',
        headers: { 'Content-Type': 'application/json', 'X-CSRF-Token': ctx.api.csrf() || '' },
        body: JSON.stringify({ index: nowResp.index, offset_seconds: Math.max(0, player.position()) }),
      }).catch(() => {});
    } catch { /* ignore */ }
  }

  /* ---------------------------------------------------------------- start */
  // Capture phase: the player's keys win over the shell's global shortcuts ("?", "g x" sequences).
  document.addEventListener('keydown', onKeyCapture, true);
  window.addEventListener('pagehide', saveOnHide);
  saveTimer = setInterval(() => { if (!player.video.paused) savePosition(); }, SAVE_EVERY_MS);
  clockTimer = setInterval(() => {
    clock.textContent = formatTime(new Date());
    if (!banner.hidden) bannerProgress.set(nowResp?.program ? Math.min(1, player.position() / (nowResp.program.duration_seconds || 1)) : 0);
  }, 1000);

  try {
    await loadChannels();
  } catch (err) {
    if (!signal.aborted) setStatus(statusCard({ iconName: 'alert-circle', title: 'Could not load channels', message: ui.errorMessage(err) }));
    channels = [];
  }
  // The URL's channel, else the last one watched (if still visible), else the first.
  const last = pref('lw.tv.lastChannel');
  const initial = vctx.route.id || (channels.some((c) => c.id === last) ? last : channels[0]?.id);
  if (!initial) {
    setStatus(statusCard({
      iconName: 'tv', title: 'No channels yet', message: 'Create a channel to start watching.',
      actions: [ui.button({ label: 'Create channels', icon: 'plus', variant: 'primary', href: '/tv/channels' })],
    }));
  } else {
    await tune(initial);
  }
  player.el.focus({ preventScroll: true });

  return {
    update(c) {
      const id = parseRoute(c.params.rest).id;
      if (id && id !== channel?.id) tune(id, { replaceUrl: false });
    },
    unmount() {
      savePosition();
      tuneSeq++;
      document.removeEventListener('keydown', onKeyCapture, true);
      window.removeEventListener('pagehide', saveOnHide);
      clearTimeout(osdTimer);
      clearTimeout(bannerTimer);
      clearTimeout(endTimer);
      clearTimeout(retryTimer);
      clearInterval(saveTimer);
      clearInterval(clockTimer);
      player.destroy();
    },
  };
}
