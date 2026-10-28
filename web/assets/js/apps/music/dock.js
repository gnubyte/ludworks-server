// The player bar in the shell's persistent bottom dock: art and title/artist links, favorite,
// shuffle / previous / play-pause / next / repeat, a seek bar with times, lyrics and queue buttons
// (they open the Now playing view), the cast button, mute + volume and a menu (normalisation,
// details, add to playlist, download, close). While casting it shows core/cast.js castControls
// instead. On phones it collapses to art, titles, play/pause and next with a thin progress line.

import { h, cls } from '../../core/dom.js';
import { icon } from '../../core/icons.js';
import { formatDuration } from '../../core/format.js';
import * as ui from '../../core/ui.js';
import { api as appApi } from '../../core/api.js';
import { musicApi } from './api.js';
import { emit } from './state.js';
import { artUrl, paths, trackTitle, trackArtist, errorText } from './model.js';

const mapi = musicApi(appApi);

function iconBtn(name, label, onClick, className = '') {
  const b = h('button', { type: 'button', class: cls('btn btn-ghost btn-icon mu-dock-btn', className), aria: { label }, dataset: { tip: label } }, icon(name, 18));
  b.addEventListener('click', onClick);
  return b;
}

function setBtn(b, name, label, size = 18) {
  b.replaceChildren(icon(name, size));
  b.setAttribute('aria-label', label);
  b.dataset.tip = label;
}

/**
 * Album/track art with an icon placeholder when missing or broken.
 * @param {string|null} url art_url
 * @param {'sm'|'md'|'lg'|'xl'} size
 */
export function artBox(url, size = 'sm', { className = '', iconName = 'music', alt = '' } = {}) {
  const box = h('span', { class: cls('mu-art', className) });
  const fallback = () => {
    box.classList.add('is-empty');
    box.replaceChildren(icon(iconName, { size: 20, className: 'mu-art-icon' }));
  };
  const src = artUrl(url, size);
  if (src) {
    const img = h('img', { src, alt, loading: 'lazy', decoding: 'async', draggable: 'false' });
    img.addEventListener('error', fallback, { once: true });
    box.append(img);
  } else {
    fallback();
  }
  return box;
}

/**
 * @param {object} player the player API (player.js)
 * @param {{navigate: (path: string) => void, onRoute?: (fn: () => void) => (() => void)}} o
 *   onRoute subscribes to completed navigations (the queue/lyrics buttons show where you are).
 */
export function createDock(player, o) {
  let seeking = false;
  let shownTrackId = null;
  let shownArt;
  let destroyed = false;

  /* ------------------------------------------------------------------ now */
  const artLink = h('a', { class: 'mu-dock-art', href: paths.nowPlaying(), aria: { label: 'Open Now playing' } });
  const titleEl = h('a', { class: 'mu-dock-title truncate' });
  const artistEl = h('div', { class: 'mu-dock-artist truncate' });
  const errorEl = h('div', { class: 'mu-dock-error truncate', role: 'status', hidden: true });
  const metaBtn = h('a', { class: 'mu-dock-meta-link', href: paths.nowPlaying(), aria: { label: 'Open Now playing' } });
  const favBtn = iconBtn('heart', 'Add to favorites', () => toggleFavorite(), 'mu-dock-fav');
  favBtn.setAttribute('aria-pressed', 'false');
  const now = h('div', { class: 'mu-dock-now' }, artLink,
    h('div', { class: 'mu-dock-meta' }, titleEl, artistEl, errorEl, metaBtn), favBtn);

  /* --------------------------------------------------------------- center */
  const shuffleBtn = iconBtn('shuffle', 'Shuffle', () => player.toggleShuffle(), 'mu-dock-shuffle');
  shuffleBtn.setAttribute('aria-pressed', 'false');
  const prevBtn = iconBtn('prev', 'Previous', () => player.previous(), 'mu-dock-prev');
  const playBtn = iconBtn('play', 'Play', () => player.toggle(), 'mu-dock-play');
  const nextBtn = iconBtn('next', 'Next', () => player.next(), 'mu-dock-next');
  const repeatBtn = iconBtn('repeat', 'Repeat: off', () => player.cycleRepeat(), 'mu-dock-repeat');
  repeatBtn.setAttribute('aria-pressed', 'false');
  const timeEl = h('span', { class: 'mu-dock-time nums', aria: { hidden: 'true' } }, '0:00');
  const durEl = h('span', { class: 'mu-dock-time nums', aria: { hidden: 'true' } }, '0:00');
  const seek = h('input', { type: 'range', class: 'mu-range mu-seek', min: '0', max: '1000', step: '1', value: '0', aria: { label: 'Seek' } });
  const center = h('div', { class: 'mu-dock-center' },
    h('div', { class: 'mu-dock-buttons' }, shuffleBtn, prevBtn, playBtn, nextBtn, repeatBtn),
    h('div', { class: 'mu-dock-seek' }, timeEl, seek, durEl));

  /* ----------------------------------------------------------------- side */
  const lyricsBtn = iconBtn('mic', 'Lyrics', () => openNowPlaying('lyrics'), 'mu-dock-lyrics');
  lyricsBtn.setAttribute('aria-pressed', 'false');
  const queueBtn = iconBtn('queue', 'Queue', () => openNowPlaying('queue'), 'mu-dock-queue');
  queueBtn.setAttribute('aria-pressed', 'false');
  const muteBtn = iconBtn('volume', 'Mute', () => player.toggleMute(), 'mu-dock-mute');
  const volume = h('input', { type: 'range', class: 'mu-range mu-volume', min: '0', max: '100', step: '5', aria: { label: 'Volume' } });
  const moreBtn = iconBtn('more', 'More player options', () => openMore(), 'mu-dock-more');
  moreBtn.setAttribute('aria-haspopup', 'menu');
  const expandBtn = iconBtn('chevron-up', 'Open Now playing', () => openNowPlaying(''), 'mu-dock-expand');
  const castBtn = player.castButton;
  const side = h('div', { class: 'mu-dock-side' }, lyricsBtn, queueBtn, castBtn, h('div', { class: 'mu-dock-vol' }, muteBtn, volume), moreBtn, expandBtn);

  const progress = h('div', { class: 'mu-dock-progress', aria: { hidden: 'true' } }, h('span', { class: 'mu-dock-progress-bar' }));
  const inner = h('div', { class: 'mu-dock-inner' }, now, center, side);

  /* ------------------------------------------------------------ cast mode */
  const castWrap = h('div', { class: 'mu-dock-cast' });
  const castSide = h('div', { class: 'mu-dock-castside' },
    iconBtn('queue', 'Queue', () => openNowPlaying('queue')),
    iconBtn('chevron-up', 'Open Now playing', () => openNowPlaying('')));
  const castRow = h('div', { class: 'mu-dock-castrow', hidden: true }, castWrap, castSide);

  const live = h('div', { class: 'sr-only', aria: { live: 'polite', atomic: 'true' } });
  const el = h('section', { class: 'mu-dock', aria: { label: 'Music player' } }, progress, inner, castRow, live);

  /* ---------------------------------------------------------- behaviour */
  function openNowPlaying(tab) {
    const onPage = location.pathname === '/music/now-playing';
    const curTab = new URLSearchParams(location.search).get('tab') || '';
    if (onPage && tab && curTab === tab) {
      history.length > 1 ? history.back() : o.navigate(paths.nowPlaying(''));
      return;
    }
    o.navigate(paths.nowPlaying(tab));
  }

  async function toggleFavorite() {
    const t = player.current?.track;
    if (!t) return;
    const want = !t.favorite;
    const updated = { ...t, favorite: want };
    emit('track-updated', updated);
    try {
      await mapi.favorite('track', t.id, want);
    } catch (err) {
      emit('track-updated', { ...t, favorite: !want });
      ui.toast.error(errorText(err));
    }
  }

  function openMore() {
    const t = player.current?.track || null;
    const norm = player.normalize;
    const items = [
      { heading: 'Volume normalization' },
      { label: 'Off', checked: norm === 'off', onSelect: () => player.setNormalize('off') },
      { label: 'Track gain (ReplayGain)', checked: norm === 'track', onSelect: () => player.setNormalize('track') },
      { label: 'Album gain (ReplayGain)', checked: norm === 'album', onSelect: () => player.setNormalize('album') },
      { separator: true },
    ];
    if (t) {
      if (t.album_id) items.push({ label: 'Go to album', icon: 'disc', onSelect: () => o.navigate(paths.album(t.album_id)) });
      if (t.artist_id) items.push({ label: 'Go to artist', icon: 'mic', onSelect: () => o.navigate(paths.artist(t.artist_id)) });
      items.push(
        { label: 'Song details', icon: 'info', onSelect: () => import('./widgets.js').then((w) => w.openTrackDetails(t)) },
        { label: 'Add to playlist…', icon: 'playlist-add', onSelect: () => import('./widgets.js').then((w) => w.addToPlaylistDialog([t])) },
        { label: 'Download', icon: 'download', onSelect: () => mapi.download(t.id) },
        { separator: true },
      );
    }
    items.push({ label: 'Stop and close the player', icon: 'x', danger: true, onSelect: () => player.close({ forget: true }) });
    ui.dropdown(moreBtn, items, { align: 'end' });
  }

  seek.addEventListener('input', () => {
    seeking = true;
    const d = player.duration();
    const t = (Number(seek.value) / 1000) * d;
    timeEl.textContent = formatDuration(t);
    seek.style.setProperty('--fill', `${Number(seek.value) / 10}%`);
  });
  seek.addEventListener('change', () => {
    const d = player.duration();
    seeking = false;
    if (d > 0) player.seek((Number(seek.value) / 1000) * d);
  });
  seek.addEventListener('pointerup', () => { seeking = false; });
  seek.addEventListener('blur', () => { seeking = false; });
  // Arrow keys seek by 5 s (the native 1/1000 step is too small); Page keys by 30 s.
  seek.addEventListener('keydown', (e) => {
    const steps = { ArrowLeft: -5, ArrowDown: -5, ArrowRight: 5, ArrowUp: 5, PageDown: -30, PageUp: 30 };
    if (e.key in steps) {
      e.preventDefault();
      player.seekBy(steps[e.key]);
    } else if (e.key === 'Home') {
      e.preventDefault();
      player.seek(0);
    } else if (e.key === 'End') {
      e.preventDefault();
      const d = player.duration();
      if (d > 0) player.seek(Math.max(0, d - 1));
    }
  });

  let volTimer = 0;
  volume.addEventListener('input', () => {
    volume.style.setProperty('--fill', `${volume.value}%`);
    clearTimeout(volTimer);
    volTimer = setTimeout(() => player.setVolume(Number(volume.value) / 100), 30);
  });

  /* --------------------------------------------------------------- render */
  function renderTrack() {
    const e = player.current;
    const t = e?.track || null;
    el.classList.toggle('is-empty', !t);
    if (!t) {
      titleEl.textContent = 'Nothing playing';
      titleEl.removeAttribute('href');
      artistEl.replaceChildren();
      artLink.replaceChildren(artBox(null));
      shownArt = undefined;
      shownTrackId = null;
      favBtn.hidden = true;
      return;
    }
    if (shownArt !== (t.art_url || null)) {
      shownArt = t.art_url || null;
      artLink.replaceChildren(artBox(shownArt, 'sm'));
    }
    titleEl.textContent = trackTitle(t);
    titleEl.title = trackTitle(t);
    titleEl.href = t.album_id ? paths.album(t.album_id) : paths.nowPlaying();
    const artist = trackArtist(t);
    artistEl.replaceChildren(t.artist_id ? h('a', { href: paths.artist(t.artist_id), title: artist }, artist) : h('span', { title: artist }, artist));
    favBtn.hidden = false;
    favBtn.setAttribute('aria-pressed', String(!!t.favorite));
    setBtn(favBtn, t.favorite ? 'heart-filled' : 'heart', t.favorite ? 'Remove from favorites' : 'Add to favorites');
    favBtn.classList.toggle('is-on', !!t.favorite);
    lyricsBtn.disabled = !t.has_lyrics;
    setBtn(lyricsBtn, 'mic', t.has_lyrics ? 'Lyrics' : 'No lyrics for this song');
    if (shownTrackId !== t.id) {
      shownTrackId = t.id;
      metaBtn.setAttribute('aria-label', `Open Now playing: ${trackTitle(t)} by ${artist}`);
    }
  }

  function renderState() {
    const playing = player.playing;
    const loading = player.loading;
    setBtn(playBtn, playing ? 'pause' : 'play', playing ? 'Pause' : 'Play', 20);
    playBtn.classList.toggle('is-loading-audio', !!loading && !player.casting);
    el.classList.toggle('is-playing', !!playing);
    const hasEntry = !!player.current;
    playBtn.disabled = !hasEntry;
    const muted = player.muted || player.volume === 0;
    setBtn(muteBtn, muted ? 'volume-x' : player.volume < 0.5 ? 'volume-low' : 'volume', player.muted ? 'Unmute' : 'Mute');
    if (document.activeElement !== volume) {
      const v = player.muted ? 0 : Math.round(player.volume * 100);
      volume.value = String(v);
      volume.style.setProperty('--fill', `${v}%`);
    }
    volume.setAttribute('aria-valuetext', player.muted ? 'Muted' : `${Math.round(player.volume * 100)}%`);
    const err = player.error;
    errorEl.textContent = err || '';
    errorEl.title = err || '';
    errorEl.hidden = !err;
    artistEl.hidden = !!err;
    el.classList.toggle('has-error', !!err);
  }

  function renderQueue() {
    const q = player.queue;
    shuffleBtn.setAttribute('aria-pressed', String(q.shuffle));
    shuffleBtn.classList.toggle('is-on', q.shuffle);
    setBtn(shuffleBtn, 'shuffle', q.shuffle ? 'Shuffle: on' : 'Shuffle: off');
    const mode = q.repeat;
    repeatBtn.setAttribute('aria-pressed', String(mode !== 'off'));
    repeatBtn.classList.toggle('is-on', mode !== 'off');
    setBtn(repeatBtn, mode === 'one' ? 'repeat-one' : 'repeat', `Repeat: ${mode === 'one' ? 'this song' : mode === 'all' ? 'all' : 'off'}`);
    nextBtn.disabled = !player.hasNext();
    prevBtn.disabled = !player.hasPrevious();
  }

  function renderTime() {
    const d = player.duration();
    const p = Math.min(player.position(), d || Infinity);
    const f = d > 0 ? Math.max(0, Math.min(1, p / d)) : 0;
    if (!seeking) {
      seek.value = String(Math.round(f * 1000));
      seek.style.setProperty('--fill', `${f * 100}%`);
      timeEl.textContent = formatDuration(p);
    }
    durEl.textContent = d > 0 ? formatDuration(d) : '–:––';
    seek.disabled = !player.canSeek();
    seek.setAttribute('aria-valuetext', d > 0 ? `${formatDuration(p)} of ${formatDuration(d)}` : formatDuration(p));
    progress.firstChild.style.width = `${f * 100}%`;
  }

  function syncRoute() {
    const onPage = location.pathname === '/music/now-playing';
    const tab = new URLSearchParams(location.search).get('tab') || '';
    queueBtn.setAttribute('aria-pressed', String(onPage && tab === 'queue'));
    lyricsBtn.setAttribute('aria-pressed', String(onPage && tab === 'lyrics'));
    queueBtn.classList.toggle('is-on', onPage && tab === 'queue');
    lyricsBtn.classList.toggle('is-on', onPage && tab === 'lyrics');
    expandBtn.hidden = onPage;
  }
  const offRoute = o.onRoute ? o.onRoute(syncRoute) : null;
  window.addEventListener('popstate', syncRoute);

  renderTrack();
  renderState();
  renderQueue();
  renderTime();
  syncRoute();

  return {
    el,
    update(kind) {
      if (destroyed) return;
      if (kind === 'track') {
        renderTrack();
        renderQueue();
        renderTime();
      } else if (kind === 'state') {
        renderState();
        renderQueue();
      } else if (kind === 'queue') {
        renderQueue();
      } else if (kind === 'time') {
        renderTime();
      }
    },
    /** Show cast controls (casting) or the local player (null). */
    setCastControls(node, device) {
      castWrap.replaceChildren(...(node ? [node] : []));
      castRow.hidden = !node;
      inner.hidden = !!node;
      progress.hidden = !!node;
      el.classList.toggle('is-casting', !!node);
      el.setAttribute('aria-label', node ? `Music player — casting to ${device?.name || 'a device'}` : 'Music player');
    },
    /** Polite screen reader announcement. */
    announce(msg) {
      live.textContent = '';
      setTimeout(() => { live.textContent = msg; }, 30);
    },
    destroy() {
      destroyed = true;
      clearTimeout(volTimer);
      offRoute?.();
      window.removeEventListener('popstate', syncRoute);
      el.remove();
    },
  };
}
