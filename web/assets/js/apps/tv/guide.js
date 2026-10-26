// /tv/guide — the EPG grid: sticky channel column, a 30-minute time ruler, a "now" line,
// horizontal + vertical scrolling and arrow-key navigation between programs. Choosing a
// program tunes the TV player to its channel.

import { h, cls } from '../../core/dom.js';
import { formatTime, formatDate } from '../../core/format.js';
import { channelLogo } from './shared.js';
import { guideStart, guideSlots, guideBox, guideNeighbor, programLabel, programTitle, programSubtitle } from './model.js';
import { tvIcon } from './icons.js';

const HOURS = 6;
const SHIFT_H = 3;

function pxPerMin() {
  return window.matchMedia?.('(max-width: 680px)').matches ? 4 : 6;
}

export async function render(host, { api, ui, signal, navigate }) {
  let start = guideStart(Date.now());
  let ppm = pxPerMin();
  let data = null;
  let rows = []; // [{channel, programs, cells: HTMLElement[]}]
  let focus = { row: 0, index: -1 };
  let firstRender = true;

  const dateLabel = h('span', { class: 'tv-guide-date' });
  const toolbar = h('div', { class: 'tv-guide-toolbar' },
    h('div', { class: 'row' },
      ui.button({ icon: 'chevron-left', title: 'Earlier', size: 'sm', onClick: () => shift(-SHIFT_H) }),
      ui.button({ label: 'Now', icon: 'clock', size: 'sm', onClick: () => { start = guideStart(Date.now()); firstRender = true; load(); } }),
      ui.button({ icon: 'chevron-right', title: 'Later', size: 'sm', onClick: () => shift(SHIFT_H) }),
      dateLabel),
    h('div', { class: 'row tv-guide-hint small faint hide-sm' },
      ui.kbd('←', '→', '↑', '↓'), ' move', ui.kbd('Enter'), ' watch'));
  const grid = h('div', { class: 'tv-guide-grid' });
  const scroller = h('div', { class: 'tv-guide-scroller', tabindex: '-1', aria: { label: 'Program guide' } }, grid);
  const wrap = h('div', { class: 'tv-guide' }, toolbar, scroller);
  host.append(wrap);
  grid.replaceChildren(h('div', { class: 'tv-guide-loading' }, ui.loadingBlock('Loading the guide…')));

  function shift(hours) {
    start += hours * 3600000;
    load();
  }

  async function load() {
    const end = start + HOURS * 3600000;
    dateLabel.textContent = `${formatDate(new Date(start), { weekday: 'short', month: 'short', day: 'numeric' })} · ${formatTime(new Date(start))} – ${formatTime(new Date(end))}`;
    try {
      data = await api.guide({ start: new Date(start).toISOString(), hours: HOURS }, signal);
      draw();
    } catch (err) {
      if (signal.aborted) return;
      grid.replaceChildren(ui.errorState(err, () => load()));
    }
  }

  function draw() {
    const end = start + HOURS * 3600000;
    ppm = pxPerMin();
    const width = HOURS * 60 * ppm;
    grid.style.setProperty('--track-w', `${width}px`);
    grid.style.setProperty('--slot-w', `${30 * ppm}px`);
    const prevFocus = focusedProgram();
    const hadFocus = grid.contains(document.activeElement);
    if (!data.channels.length) {
      grid.replaceChildren(ui.emptyState({
        icon: 'calendar', title: 'No channels in the guide',
        message: 'Create a channel to see what is on.',
        action: ui.button({ label: 'Channels', icon: 'radio', href: '/tv/channels' }),
      }));
      rows = [];
      return;
    }
    const ruler = h('div', { class: 'tv-guide-ruler', aria: { hidden: 'true' } },
      guideSlots(start, end).map((t) => {
        const el = h('span', { class: 'tv-guide-slot' }, formatTime(new Date(t)));
        el.style.left = `${((t - start) / 60000) * ppm}px`;
        return el;
      }));
    const corner = h('div', { class: 'tv-guide-corner' }, tvIcon('radio', 14), h('span', {}, 'Channels'));
    const now = Date.now();
    rows = data.channels.map((gc, r) => {
      const ch = gc.channel;
      const track = h('div', { class: 'tv-guide-track', role: 'list', aria: { label: `${ch.number} ${ch.name}` } });
      const cells = [];
      const programs = gc.programs.filter((p) => guideBox(p, start, end, ppm));
      programs.forEach((p, i) => {
        const box = guideBox(p, start, end, ppm);
        const s = Date.parse(p.starts_at);
        const e = Date.parse(p.ends_at);
        const isNow = s <= now && now < e;
        const sub = programSubtitle(p);
        const cell = h('a', {
          class: cls('tv-guide-prog', { 'is-now': isNow, 'is-past': e <= now, 'clip-start': box.clippedStart, 'clip-end': box.clippedEnd, narrow: box.width < 64 }),
          href: `/tv/watch/${ch.id}`, tabindex: '-1', role: 'listitem',
          aria: { label: `${programLabel(p)}, ${formatTime(p.starts_at)} to ${formatTime(p.ends_at)}, channel ${ch.number} ${ch.name}${isNow ? ', on now' : ''}` },
          dataset: { row: String(r), index: String(i) },
        },
        h('span', { class: 'tv-guide-prog-title' }, programTitle(p)),
        box.width >= 110 ? h('span', { class: 'tv-guide-prog-sub' }, sub ? `${formatTime(p.starts_at)} · ${sub}` : formatTime(p.starts_at)) : null);
        cell.style.left = `${box.left}px`;
        cell.style.width = `${box.width}px`;
        cell.title = `${programLabel(p)}\n${formatTime(p.starts_at)} – ${formatTime(p.ends_at)}`;
        cells.push(cell);
        track.append(cell);
      });
      if (!programs.length) track.append(h('span', { class: 'tv-guide-empty small faint' }, 'Nothing scheduled'));
      const head = h('a', { class: 'tv-guide-chan', href: `/tv/watch/${ch.id}`, aria: { label: `Watch channel ${ch.number}, ${ch.name}` } },
        channelLogo(ch, { size: 'sm' }),
        h('span', { class: 'tv-guide-chan-num nums' }, String(ch.number)),
        h('span', { class: 'tv-guide-chan-name truncate' }, ch.name));
      return { channel: ch, programs, cells, el: h('div', { class: 'tv-guide-row' }, head, track) };
    });
    const nowLine = h('div', { class: 'tv-guide-now', aria: { hidden: 'true' } });
    ruler.append(h('span', { class: 'tv-guide-now-mark' }, 'Now'));
    grid.replaceChildren(corner, ruler, ...rows.map((r) => r.el), nowLine);
    placeNow();
    // Restore keyboard focus (or put the roving tab stop on the first current program).
    let target = null;
    if (prevFocus) target = findCell(prevFocus.channelId, prevFocus.at);
    if (!target) target = rows.flatMap((r) => r.cells).find((c) => c.classList.contains('is-now')) || rows.find((r) => r.cells.length)?.cells[0];
    if (target) setFocus(Number(target.dataset.row), Number(target.dataset.index), hadFocus);
    if (firstRender) {
      firstRender = false;
      const x = ((Date.now() - start) / 60000) * ppm;
      scroller.scrollLeft = Math.max(0, x - 90);
    }
  }

  function placeNow() {
    const line = grid.querySelector('.tv-guide-now');
    if (!line) return;
    const end = start + HOURS * 3600000;
    const t = Date.now();
    const mark = grid.querySelector('.tv-guide-now-mark');
    line.hidden = t < start || t >= end;
    if (mark) {
      mark.hidden = line.hidden;
      mark.style.setProperty('--x', `${((t - start) / 60000) * ppm}px`);
    }
    line.style.setProperty('--x', `${((t - start) / 60000) * ppm}px`);
    for (const r of rows) {
      r.cells.forEach((c, i) => {
        const p = r.programs[i];
        const s = Date.parse(p.starts_at);
        const e = Date.parse(p.ends_at);
        c.classList.toggle('is-now', s <= t && t < e);
        c.classList.toggle('is-past', e <= t);
      });
    }
  }

  function focusedProgram() {
    const r = rows[focus.row];
    const p = r?.programs[focus.index];
    return p ? { channelId: r.channel.id, at: Date.parse(p.starts_at) } : null;
  }

  function findCell(channelId, at) {
    const r = rows.findIndex((x) => x.channel.id === channelId);
    if (r < 0) return null;
    const i = rows[r].programs.findIndex((p) => Date.parse(p.starts_at) <= at && at < Date.parse(p.ends_at));
    return rows[r].cells[i >= 0 ? i : 0] || null;
  }

  function setFocus(row, index, moveFocus = true) {
    const old = rows[focus.row]?.cells[focus.index];
    if (old) old.tabIndex = -1;
    focus = { row, index };
    const cell = rows[row]?.cells[index];
    if (!cell) return;
    cell.tabIndex = 0;
    if (moveFocus) {
      cell.focus({ preventScroll: true });
      // Keep the focused cell visible under the sticky header/column.
      const sr = scroller.getBoundingClientRect();
      const cr = cell.getBoundingClientRect();
      const chanW = rows[row].el.firstElementChild.getBoundingClientRect().width;
      const rulerH = grid.querySelector('.tv-guide-ruler')?.getBoundingClientRect().height || 0;
      if (cr.left < sr.left + chanW) scroller.scrollLeft -= sr.left + chanW - cr.left + 8;
      else if (cr.left > sr.right - 120) scroller.scrollLeft += cr.left - (sr.right - Math.min(cr.width, sr.width - chanW)) + 8;
      if (cr.top < sr.top + rulerH) scroller.scrollTop -= sr.top + rulerH - cr.top + 4;
      else if (cr.bottom > sr.bottom) scroller.scrollTop += cr.bottom - sr.bottom + 4;
    }
  }

  grid.addEventListener('keydown', (e) => {
    const cell = e.target instanceof Element ? e.target.closest('.tv-guide-prog') : null;
    if (!cell) return;
    const row = Number(cell.dataset.row);
    const index = Number(cell.dataset.index);
    const p = rows[row]?.programs[index];
    const dirs = { ArrowLeft: 'left', ArrowRight: 'right', ArrowUp: 'up', ArrowDown: 'down' };
    if (dirs[e.key]) {
      e.preventDefault();
      const at = Math.max(Date.parse(p.starts_at), start);
      const next = guideNeighbor(rows.map((r) => r.programs), row, index, dirs[e.key], at);
      if (next) setFocus(next.row, next.index);
      else if (dirs[e.key] === 'right') shift(SHIFT_H);
      else if (dirs[e.key] === 'left' && start > Date.now() - 24 * 3600000) shift(-SHIFT_H);
    } else if (e.key === 'PageDown' || e.key === 'PageUp') {
      e.preventDefault();
      const dir = e.key === 'PageDown' ? 'down' : 'up';
      const at = Math.max(Date.parse(p.starts_at), start);
      const progRows = rows.map((x) => x.programs);
      let cur = { row, index };
      for (let k = 0; k < 6; k++) {
        const n = guideNeighbor(progRows, cur.row, cur.index, dir, at);
        if (!n) break;
        cur = n;
      }
      if (cur.row !== row) setFocus(cur.row, cur.index);
    } else if (e.key === 'Home') {
      e.preventDefault();
      const i = rows[row].cells.findIndex((c) => c.classList.contains('is-now'));
      setFocus(row, i >= 0 ? i : 0);
    } else if (e.key === ' ') {
      e.preventDefault();
      navigate(cell.getAttribute('href'));
    }
  });
  grid.addEventListener('focusin', (e) => {
    const cell = e.target instanceof Element ? e.target.closest('.tv-guide-prog') : null;
    if (cell) {
      const old = rows[focus.row]?.cells[focus.index];
      if (old && old !== cell) old.tabIndex = -1;
      focus = { row: Number(cell.dataset.row), index: Number(cell.dataset.index) };
      cell.tabIndex = 0;
    }
  });

  await load();
  const nowTimer = setInterval(placeNow, 30000);
  const refresh = setInterval(() => { if (document.visibilityState === 'visible') load(); }, 5 * 60000);
  const onResize = () => {
    if (pxPerMin() !== ppm && data) draw();
  };
  window.addEventListener('resize', onResize);
  return () => {
    clearInterval(nowTimer);
    clearInterval(refresh);
    window.removeEventListener('resize', onResize);
  };
}
