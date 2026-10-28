// Now playing (/music/now-playing?tab=queue|lyrics): large art, the song's links, favorite and
// rating, a full transport (seek, shuffle / previous / play / next / repeat, volume) and a panel
// with the queue (play any entry, drag or Alt+↑/↓ to reorder, remove, clear, save as playlist)
// or the lyrics (synced lyrics follow the music and seek on click; plain lyrics as text).

import { h, cls } from '../../../core/dom.js';
import { icon } from '../../../core/icons.js';
import * as ui from '../../../core/ui.js';
import { formatDuration } from '../../../core/format.js';
import { on } from '../state.js';
import { getPlayer } from '../player.js';
import {
  mapi, artBox, createTrackList, favoriteButton, ratingControl, dialogs, trackMenuItems, shuffleAll,
} from '../widgets.js';
import {
  paths, trackTitle, trackArtist, artUrl, songCount, formatLongDuration,
} from '../model.js';
import { normalizeLyrics, activeLineIndex } from '../lyrics.js';

const reduceMotion = () => globalThis.matchMedia?.('(prefers-reduced-motion: reduce)').matches;
// Phones and narrow windows: one scrolling page (stage, then the panel) instead of two columns.
const narrowQuery = globalThis.matchMedia?.('(max-width: 900px)') || null;
const narrow = () => !!narrowQuery?.matches;

function ctlButton(name, label, onClick, className = '', size = 20) {
  const b = h('button', { type: 'button', class: cls('btn btn-ghost btn-icon mu-np-btn', className), aria: { label }, dataset: { tip: label } }, icon(name, size));
  b.addEventListener('click', onClick);
  return b;
}

function setBtn(b, name, label, size = 20) {
  b.replaceChildren(icon(name, size));
  b.setAttribute('aria-label', label);
  b.dataset.tip = label;
}

export function mountNowPlaying(host, app, route) {
  app.setTitle('Now playing');
  let tab = route.query.tab === 'lyrics' ? 'lyrics' : 'queue';
  const root = h('div', { class: 'mu-np' });
  host.append(root);
  const offs = [];
  let built = null; // the parts while a player exists

  function player() {
    return getPlayer();
  }

  /* ------------------------------------------------------------- empty */
  function renderEmpty() {
    built?.destroy();
    built = null;
    root.replaceChildren(h('div', { class: 'mu-np-empty' }, ui.emptyState({
      icon: 'music', title: 'Nothing is playing',
      message: 'Pick an album, a playlist or a song — or let the whole library surprise you.',
      action: h('div', { class: 'row wrap' },
        ui.button({ label: 'Shuffle all', icon: 'shuffle', variant: 'primary', onClick: () => shuffleAll() }),
        ui.button({ label: 'Browse music', icon: 'music', href: paths.home() })),
    })));
  }

  /* ------------------------------------------------------------- build */
  function build() {
    built?.destroy();
    const p = player();
    app.setLayout(!narrow());
    if (!p || !p.current) {
      renderEmpty();
      return;
    }

    // Stage: art, titles, actions, transport.
    const art = h('div', { class: 'mu-np-art' });
    const title = h('h1', { class: 'mu-np-title' });
    const artist = h('div', { class: 'mu-np-artist' });
    const album = h('div', { class: 'mu-np-album' });
    const actions = h('div', { class: 'mu-np-actions' });
    const castNote = h('div', { class: 'mu-np-cast', hidden: true });
    const errNote = h('div', { class: 'mu-np-error', role: 'status', hidden: true });

    const time = h('span', { class: 'mu-np-time nums' }, '0:00');
    const dur = h('span', { class: 'mu-np-time nums' }, '0:00');
    const seek = h('input', { type: 'range', class: 'mu-range mu-seek mu-np-seek', min: '0', max: '1000', step: '1', value: '0', aria: { label: 'Seek' } });
    let seeking = false;
    const shuffleBtn = ctlButton('shuffle', 'Shuffle', () => player()?.toggleShuffle(), 'mu-np-shuffle');
    const prevBtn = ctlButton('prev', 'Previous', () => player()?.previous());
    const playBtn = ctlButton('play', 'Play', () => player()?.toggle(), 'mu-np-play', 26);
    const nextBtn = ctlButton('next', 'Next', () => player()?.next());
    const repeatBtn = ctlButton('repeat', 'Repeat: off', () => player()?.cycleRepeat(), 'mu-np-repeat');
    const muteBtn = ctlButton('volume', 'Mute', () => player()?.toggleMute(), 'mu-np-mute', 18);
    const volume = h('input', { type: 'range', class: 'mu-range mu-volume', min: '0', max: '100', step: '5', aria: { label: 'Volume' } });
    const transport = h('div', { class: 'mu-np-transport' },
      h('div', { class: 'mu-np-seekrow' }, time, seek, dur),
      h('div', { class: 'mu-np-buttons' }, shuffleBtn, prevBtn, playBtn, nextBtn, repeatBtn),
      h('div', { class: 'mu-np-volrow' }, muteBtn, volume));
    const stage = h('section', { class: 'mu-np-stage', aria: { label: 'Now playing' } }, art,
      h('div', { class: 'mu-np-meta' }, title, artist, album, castNote, errNote), actions, transport);

    // Panel: queue or lyrics.
    const tabs = ui.tabs({
      items: [{ id: 'queue', label: 'Up next', icon: 'queue' }, { id: 'lyrics', label: 'Lyrics', icon: 'mic' }],
      active: tab, ariaLabel: 'Now playing panels',
      onChange: (id) => app.setQuery({ tab: id }),
    });
    const scroller = h('div', { class: 'mu-np-scroll', tabindex: '-1' });
    const panelHead = h('div', { class: 'mu-np-panelhead' });
    const panel = h('section', { class: 'mu-np-panel', aria: { label: 'Queue and lyrics' } }, h('div', { class: 'mu-np-tabs' }, tabs.el), panelHead, scroller);
    root.replaceChildren(stage, panel);

    /* ---------------------------------------------------------- queue */
    let queueList = null;
    let userScrollAt = 0;
    const markUserScroll = () => { userScrollAt = Date.now(); };
    scroller.addEventListener('wheel', markUserScroll, { passive: true });
    scroller.addEventListener('touchmove', markUserScroll, { passive: true });
    scroller.addEventListener('keydown', markUserScroll);
    window.addEventListener('wheel', markUserScroll, { passive: true });
    window.addEventListener('touchmove', markUserScroll, { passive: true });

    function buildQueue() {
      lyricsTeardown();
      const q = player().queue;
      queueList?.destroy();
      queueList = createTrackList({
        label: 'Queue', scroller: narrow() ? null : scroller, art: true, album: false, selectable: true,
        getTrack: (e) => e.track, keyOf: (e) => e.uid,
        isCurrent: (e) => e === player()?.queue.current,
        rowClass: (e, i) => (i < (player()?.queue.index ?? -1) ? 'is-past' : ''),
        onPlay: (ti) => player()?.jump(ti),
        reorder: { onMove: (keys, position, meta) => {
          player()?.moveInQueue(keys, position);
          const pq = player()?.queue;
          if (!pq) return;
          const first = pq.entries.find((e) => keys.includes(e.uid));
          const at = pq.indexOf(first?.uid) + 1;
          queueList.announce(keys.length === 1 ? `Moved “${trackTitle(first?.track)}” to position ${at} of ${pq.length}.` : `Moved ${keys.length} songs to position ${at} of ${pq.length}.`);
          if (meta?.focusKey) requestAnimationFrame(() => queueList.focusKey(meta.focusKey));
        } },
        menuExtra: (targets) => [
          { label: targets.length === 1 ? 'Remove from queue' : `Remove ${targets.length} songs from the queue`, icon: 'x', onSelect: () => removeEntries(targets) },
          { label: 'Move after the current song', icon: 'queue', onSelect: () => {
            const pq = player()?.queue;
            if (!pq) return;
            const keys = targets.map((t) => t.uid).filter((u) => u !== pq.current?.uid);
            const curIdx = pq.index;
            const before = pq.entries.slice(0, curIdx + 1).filter((e) => !keys.includes(e.uid)).length;
            player().moveInQueue(keys, before);
          } },
        ],
        selectionExtra: (chosen) => [ui.button({ label: 'Remove', icon: 'x', size: 'sm', variant: 'danger-ghost', onClick: () => removeEntries(chosen) })],
        onDelete: (targets) => removeEntries(targets),
        empty: () => ui.emptyState({ icon: 'queue', title: 'The queue is empty', compact: true }),
      });
      queueList.setItems(q.entries);
      scroller.replaceChildren(queueList.el);
      renderQueueHead();
      requestAnimationFrame(() => revealCurrent(true));
    }

    function removeEntries(entries) {
      const n = entries.length;
      player()?.removeFromQueue(entries.map((e) => e.uid));
      queueList?.clearSelection();
      queueList?.announce(n === 1 ? `Removed “${trackTitle(entries[0].track)}” from the queue.` : `Removed ${n} songs from the queue.`);
    }

    function renderQueueHead() {
      const pq = player()?.queue;
      if (!pq) return;
      const up = pq.upcoming();
      panelHead.replaceChildren(
        h('div', { class: 'mu-np-count muted small' }, `${songCount(pq.length)} · ${formatLongDuration(pq.entries.reduce((s, e) => s + (Number(e.track?.duration_seconds) || 0), 0))}${up.length ? ` · ${up.length} up next` : ''}`),
        h('div', { class: 'row' },
          ui.button({ label: 'Save as playlist', icon: 'playlist-add', size: 'sm', variant: 'ghost', onClick: () => saveAsPlaylist() }),
          ui.button({ label: 'Clear', icon: 'trash', size: 'sm', variant: 'ghost', title: 'Remove the songs after the current one', onClick: () => {
            if (!up.length) return;
            player()?.clearUpcoming();
            ui.toast.info('Cleared the songs after the current one');
          } })));
    }

    async function saveAsPlaylist() {
      const pq = player()?.queue;
      if (!pq?.length) return;
      const d = await dialogs();
      const created = await d.playlistDialog({ trackIds: pq.entries.map((e) => e.id).slice(0, 10000) });
      if (created) ui.toast.success(`Saved the queue as “${created.name}”`, { action: { label: 'Open', onClick: () => app.navigate(paths.playlist(created.id)) } });
    }

    function revealCurrent(force = false) {
      // On a single scrolling page the stage stays in view: the queue is not scrolled for you.
      if (!queueList || tab !== 'queue' || narrow()) return;
      if (!force && Date.now() - userScrollAt < 5000) return;
      const pq = player()?.queue;
      if (!pq || pq.index < 0) return;
      const vl = queueList;
      // Scroll the panel so the current entry is near the top (with one played song above it).
      const rowH = parseFloat(getComputedStyle(scroller).getPropertyValue('--mu-row-h')) || 56;
      const target = Math.max(0, (pq.index - 1) * rowH);
      scroller.scrollTo({ top: target, behavior: force || reduceMotion() ? 'auto' : 'smooth' });
      vl.refresh();
    }

    /* --------------------------------------------------------- lyrics */
    let lyr = null; // {trackId, lines, synced, els, active}
    let lyrToken = 0;

    function lyricsTeardown() {
      lyr = null;
      lyrToken += 1;
    }

    async function buildLyrics() {
      queueList?.destroy();
      queueList = null;
      panelHead.replaceChildren();
      const t = player()?.current?.track;
      const my = ++lyrToken;
      lyr = null;
      if (!t) {
        scroller.replaceChildren();
        return;
      }
      if (!t.has_lyrics) {
        scroller.replaceChildren(ui.emptyState({ icon: 'mic', title: 'No lyrics for this song', message: 'Lyrics come from the file’s tags or an .lrc file with the same name next to it.', compact: true }));
        return;
      }
      scroller.replaceChildren(ui.loadingBlock('Loading lyrics…'));
      let raw;
      try {
        raw = await mapi.lyrics(t.id);
      } catch (err) {
        if (my !== lyrToken) return;
        scroller.replaceChildren(err?.status === 404
          ? ui.emptyState({ icon: 'mic', title: 'No lyrics for this song', compact: true })
          : ui.errorState(err, () => buildLyrics()));
        return;
      }
      if (my !== lyrToken) return;
      const l = normalizeLyrics(raw);
      if (!l.lines.length) {
        scroller.replaceChildren(ui.emptyState({ icon: 'mic', title: 'No lyrics for this song', compact: true }));
        return;
      }
      if (!l.synced) {
        scroller.replaceChildren(h('div', { class: 'mu-lyrics is-plain' }, l.lines.map((x) => h('p', { class: 'mu-lyric' }, x.text || ' '))));
        lyr = { trackId: t.id, synced: false };
        return;
      }
      const els = l.lines.map((x) => {
        const b = h('button', { type: 'button', class: cls('mu-lyric', { 'is-untimed': x.time === null }), tabindex: '-1' }, x.text || '♪');
        if (x.time !== null) {
          b.addEventListener('click', () => {
            if (player()?.current?.id === t.id) player().seek(x.time);
          });
        }
        return b;
      });
      const box = h('div', { class: 'mu-lyrics is-synced', role: 'list', aria: { label: 'Lyrics, follow along' } }, els);
      scroller.replaceChildren(box, h('div', { class: 'mu-lyrics-pad', aria: { hidden: 'true' } }));
      lyr = { trackId: t.id, synced: true, lines: l.lines, els, active: -1 };
      syncLyrics(true);
    }

    function syncLyrics(force = false) {
      if (!lyr?.synced || tab !== 'lyrics') return;
      const p = player();
      if (!p || p.current?.id !== lyr.trackId) return;
      const i = activeLineIndex(lyr.lines, p.position(), 0.15);
      if (i === lyr.active && !force) return;
      const prev = lyr.els[lyr.active];
      prev?.classList.remove('is-active');
      prev?.removeAttribute('aria-current');
      lyr.active = i;
      const cur = lyr.els[i];
      if (!cur) return;
      cur.classList.add('is-active');
      cur.setAttribute('aria-current', 'true');
      for (let j = 0; j < lyr.els.length; j++) lyr.els[j].classList.toggle('is-past', j < i);
      if (force || Date.now() - userScrollAt > 4000) {
        const behavior = force || reduceMotion() ? 'auto' : 'smooth';
        if (narrow()) {
          // Follow along only once the lyrics are what the user is looking at.
          const box = scroller.getBoundingClientRect();
          if (!force && box.top < window.innerHeight * 0.5) window.scrollBy({ top: cur.getBoundingClientRect().top - window.innerHeight * 0.4, behavior });
        } else {
          const top = cur.getBoundingClientRect().top - scroller.getBoundingClientRect().top + scroller.scrollTop - scroller.clientHeight * 0.35;
          scroller.scrollTo({ top: Math.max(0, top), behavior });
        }
      }
    }

    function showPanel() {
      tabs.setActive(tab);
      if (tab === 'lyrics') buildLyrics();
      else buildQueue();
    }

    /* -------------------------------------------------------- stage */
    let shownTrack = null;
    function renderTrack() {
      const pl = player();
      const e = pl?.current;
      const t = e?.track;
      if (!t) return;
      if (shownTrack !== t) {
        const sameSong = shownTrack?.id === t.id;
        shownTrack = t;
        if (!sameSong || art.childElementCount === 0) {
          const box = artBox(t.art_url, 'lg', { className: 'mu-np-img' });
          const img = box.querySelector('img');
          if (img && t.art_url) {
            img.srcset = `${artUrl(t.art_url, 'lg')} 640w, ${artUrl(t.art_url, 'xl')} 1200w`;
            img.sizes = '(max-width: 900px) 70vw, 480px';
            img.loading = 'eager';
          }
          art.replaceChildren(box);
        }
        title.textContent = trackTitle(t);
        artist.replaceChildren(t.artist_id ? h('a', { href: paths.artist(t.artist_id) }, trackArtist(t)) : trackArtist(t));
        album.replaceChildren(t.album ? (t.album_id ? h('a', { href: paths.album(t.album_id) }, t.album) : t.album) : '');
        actions.replaceChildren(
          favoriteButton('track', t),
          ratingControl(t, { compact: true }),
          ui.menuButton({ icon: 'more', title: 'More options for this song', variant: 'ghost', items: () => trackMenuItems([t], { context: e.context }) }));
        if (!sameSong && tab === 'lyrics') buildLyrics();
      }
    }

    function renderState() {
      const pl = player();
      if (!pl) return;
      const playing = pl.playing;
      setBtn(playBtn, playing ? 'pause' : 'play', playing ? 'Pause' : 'Play', 26);
      playBtn.classList.toggle('is-loading-audio', !!pl.loading && !pl.casting);
      const q = pl.queue;
      shuffleBtn.setAttribute('aria-pressed', String(q.shuffle));
      shuffleBtn.classList.toggle('is-on', q.shuffle);
      setBtn(shuffleBtn, 'shuffle', q.shuffle ? 'Shuffle: on' : 'Shuffle: off');
      repeatBtn.setAttribute('aria-pressed', String(q.repeat !== 'off'));
      repeatBtn.classList.toggle('is-on', q.repeat !== 'off');
      setBtn(repeatBtn, q.repeat === 'one' ? 'repeat-one' : 'repeat', `Repeat: ${q.repeat === 'one' ? 'this song' : q.repeat === 'all' ? 'all' : 'off'}`);
      nextBtn.disabled = !pl.hasNext();
      const casting = pl.casting;
      castNote.hidden = !casting;
      castNote.replaceChildren(...(casting ? [icon('cast', 14), h('span', {}, `Playing on ${casting.name || 'a cast device'}`)] : []));
      const castVol = casting ? pl.castController?.session : null;
      const vol = castVol ? Number(castVol.volume) || 0 : pl.volume;
      const muted = castVol ? !!castVol.muted : pl.muted;
      setBtn(muteBtn, muted || vol === 0 ? 'volume-x' : vol < 0.5 ? 'volume-low' : 'volume', muted ? 'Unmute' : 'Mute', 18);
      if (document.activeElement !== volume) {
        const v = muted ? 0 : Math.round(vol * 100);
        volume.value = String(v);
        volume.style.setProperty('--fill', `${v}%`);
      }
      volume.setAttribute('aria-label', casting ? 'Device volume' : 'Volume');
      errNote.textContent = pl.error || '';
      errNote.hidden = !pl.error;
      root.classList.toggle('is-playing', !!playing);
    }

    function renderTime() {
      const pl = player();
      if (!pl) return;
      const d = pl.duration();
      const pos = Math.min(pl.position(), d || Infinity);
      const f = d > 0 ? Math.max(0, Math.min(1, pos / d)) : 0;
      if (!seeking) {
        seek.value = String(Math.round(f * 1000));
        seek.style.setProperty('--fill', `${f * 100}%`);
        time.textContent = formatDuration(pos);
      }
      dur.textContent = d > 0 ? formatDuration(d) : '–:––';
      seek.disabled = !pl.canSeek();
      seek.setAttribute('aria-valuetext', d > 0 ? `${formatDuration(pos)} of ${formatDuration(d)}` : formatDuration(pos));
      syncLyrics(false);
    }

    seek.addEventListener('input', () => {
      seeking = true;
      const d = player()?.duration() || 0;
      time.textContent = formatDuration((Number(seek.value) / 1000) * d);
      seek.style.setProperty('--fill', `${Number(seek.value) / 10}%`);
    });
    seek.addEventListener('change', () => {
      seeking = false;
      const d = player()?.duration() || 0;
      if (d > 0) player()?.seek((Number(seek.value) / 1000) * d);
    });
    seek.addEventListener('blur', () => { seeking = false; });
    seek.addEventListener('keydown', (e) => {
      const steps = { ArrowLeft: -5, ArrowDown: -5, ArrowRight: 5, ArrowUp: 5, PageDown: -30, PageUp: 30 };
      if (e.key in steps) {
        e.preventDefault();
        player()?.seekBy(steps[e.key]);
      } else if (e.key === 'Home' || e.key === 'End') {
        e.preventDefault();
        const d = player()?.duration() || 0;
        player()?.seek(e.key === 'Home' ? 0 : Math.max(0, d - 1));
      }
    });
    let volTimer = 0;
    volume.addEventListener('input', () => {
      volume.style.setProperty('--fill', `${volume.value}%`);
      clearTimeout(volTimer);
      volTimer = setTimeout(() => player()?.setVolume(Number(volume.value) / 100), 40);
    });

    const localOffs = [
      on('track', () => {
        renderTrack();
        queueList?.refreshPlaying();
        renderQueueHead();
        revealCurrent();
      }),
      on('queue', () => {
        if (tab === 'queue' && queueList) {
          queueList.setItems(player()?.queue.entries || []);
          renderQueueHead();
        }
        renderState();
      }),
      on('state', () => {
        renderState();
        queueList?.refreshPlaying();
      }),
      on('time', renderTime),
      on('track-updated', (t) => {
        if (player()?.current?.id === t.id) {
          shownTrack = null;
          renderTrack();
        }
      }),
    ];

    renderTrack();
    renderState();
    renderTime();
    showPanel();

    built = {
      setTab(next) {
        tab = next;
        showPanel();
      },
      destroy() {
        clearTimeout(volTimer);
        localOffs.forEach((off) => off());
        window.removeEventListener('wheel', markUserScroll);
        window.removeEventListener('touchmove', markUserScroll);
        queueList?.destroy();
        lyricsTeardown();
      },
    };
  }

  const onNarrow = () => build();
  narrowQuery?.addEventListener?.('change', onNarrow);
  offs.push(() => narrowQuery?.removeEventListener?.('change', onNarrow));
  offs.push(on('player', () => build()));
  offs.push(on('track', () => { if (!built && player()?.current) build(); }));
  build();

  // Escape goes back (the dock's buttons and links bring you here).
  const onKey = (e) => {
    if (e.key !== 'Escape' || e.defaultPrevented || document.documentElement.classList.contains('modal-open')) return;
    if (e.target instanceof Element && e.target.closest('input, textarea, select, .menu')) return;
    e.preventDefault();
    if (history.length > 1) history.back();
    else app.navigate(paths.home());
  };
  document.addEventListener('keydown', onKey);

  return {
    update(r) {
      const next = r.query.tab === 'lyrics' ? 'lyrics' : 'queue';
      if (next !== tab) {
        tab = next;
        if (built) built.setTab(next);
      }
    },
    destroy() {
      document.removeEventListener('keydown', onKey);
      offs.forEach((off) => off());
      built?.destroy();
    },
  };
}
