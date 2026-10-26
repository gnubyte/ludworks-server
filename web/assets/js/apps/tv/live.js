// /tv — live home: a card per channel with its logo, number, what is on now (with progress)
// and what is next. One guide request feeds every card; progress ticks locally.

import { h, cls } from '../../core/dom.js';
import { formatTime, plural } from '../../core/format.js';
import { channelLogo, programText, miniProgress, liveBadge, pref } from './shared.js';
import { currentAndNext, programLabel, programProgress, sortChannels } from './model.js';
import { tvIcon } from './icons.js';

const REFRESH_MS = 30000;

export async function render(host, { ctx, api, ui, signal, navigate }) {
  const grid = h('div', { class: 'tv-cards', role: 'list', aria: { label: 'Channels' } });
  const groupBar = h('div', { class: 'tv-groups', role: 'toolbar', aria: { label: 'Channel groups' } });
  const lastId = pref('lw.tv.lastChannel');
  const header = ui.pageHeader({
    title: 'Live TV',
    subtitle: 'Your channels, on air right now.',
    actions: [
      ui.button({ label: 'Watch', icon: 'play', variant: 'primary', href: lastId ? `/tv/watch/${lastId}` : '/tv/watch' }),
      ui.button({ label: 'Guide', icon: 'calendar', href: '/tv/guide' }),
    ],
  });
  const continueCard = h('a', { class: 'tv-continue card card-hover', href: '/tv/library' },
    h('span', { class: 'tv-continue-icon' }, tvIcon('film', 20)),
    h('span', { class: 'tv-continue-text' },
      h('span', { class: 'bold' }, 'Continue watching'),
      h('span', { class: 'muted small' }, 'Pick up movies and episodes where you left off in the library.')),
    tvIcon('chevron-right', { size: 16, className: 'faint' }));
  const page = h('div', { class: 'page wide tv-live' }, header, continueCard, groupBar, grid);
  host.append(page);

  let channels = [];
  let rows = new Map(); // channel id → programs
  let group = pref('lw.tv.group') || '';
  const cards = new Map(); // channel id → {el, bar, update}
  let timer = 0;
  let ticker = 0;

  grid.replaceChildren(...ui.skeletonCards(6, '148px'));

  async function load() {
    try {
      const start = new Date(Date.now() - 1000).toISOString();
      const g = await api.guide({ start, hours: 3 }, signal);
      channels = sortChannels(g.channels.map((c) => c.channel));
      rows = new Map(g.channels.map((c) => [c.channel.id, c.programs]));
      renderAll();
    } catch (err) {
      if (signal.aborted) return;
      grid.replaceChildren(ui.errorState(err, () => load()));
    }
  }

  function renderGroups() {
    const groups = [...new Set(channels.map((c) => c.group).filter(Boolean))].sort((a, b) => a.localeCompare(b));
    groupBar.hidden = groups.length < 2;
    if (group && !groups.includes(group)) group = '';
    const chip = (value, label) => {
      const b = h('button', { type: 'button', class: cls('tv-group-chip', { 'is-active': group === value }), aria: { pressed: String(group === value) } }, label);
      b.addEventListener('click', () => {
        group = value;
        pref('lw.tv.group', value || null);
        renderAll();
      });
      return b;
    };
    groupBar.replaceChildren(chip('', 'All'), ...groups.map((g) => chip(g, g)));
  }

  function card(ch) {
    const bar = miniProgress('Program progress');
    const nowBox = h('div', { class: 'tv-card-now' });
    const nextBox = h('div', { class: 'tv-card-next small' });
    const el = h('a', {
      class: cls('tv-card', { 'is-last': ch.id === lastId }), href: `/tv/watch/${ch.id}`, role: 'listitem',
      aria: { label: `Channel ${ch.number}, ${ch.name}` },
    },
    h('div', { class: 'tv-card-head' },
      channelLogo(ch, { size: 'lg' }),
      h('div', { class: 'tv-card-chan' },
        h('div', { class: 'tv-card-num nums' }, String(ch.number)),
        h('div', { class: 'tv-card-name truncate', title: ch.name }, ch.name),
        h('div', { class: 'tv-card-meta' },
          ch.playback_mode === 'on_demand' ? ui.badge('On demand', 'info') : liveBadge(),
          ch.visibility === 'shared' ? ui.badge('Shared', 'neutral', { icon: 'users' }) : null)),
      h('span', { class: 'tv-card-play', aria: { hidden: 'true' } }, tvIcon('play', 18))),
    nowBox, bar.el, nextBox);
    const update = () => {
      const { current, next } = currentAndNext(rows.get(ch.id), Date.now());
      nowBox.replaceChildren(programText(current));
      bar.set(programProgress(current, Date.now()));
      bar.el.hidden = !current;
      nextBox.replaceChildren(next
        ? h('span', { class: 'truncate' }, h('span', { class: 'faint' }, `Next ${formatTime(next.starts_at)} · `), programLabel(next))
        : h('span', { class: 'faint' }, current ? 'Nothing scheduled after this' : 'Add playable media to its lineup'));
      if (current && Date.parse(current.ends_at) <= Date.now()) scheduleReload(500);
    };
    update();
    return { el, update };
  }

  function renderAll() {
    renderGroups();
    cards.clear();
    const visible = channels.filter((c) => !group || c.group === group);
    if (!channels.length) {
      grid.replaceChildren(ui.emptyState({
        icon: 'tv', title: 'No channels yet',
        message: 'Build a lineup from your shows and movies, then turn it into a channel — or let Ludworks create channels for you.',
        action: h('div', { class: 'row wrap' },
          ui.button({ label: 'Create channels', icon: 'sparkle', variant: 'primary', href: '/tv/channels?auto=1' }),
          ui.button({ label: 'New lineup', icon: 'plus', href: '/tv/lineups' })),
      }));
      return;
    }
    const els = visible.map((ch) => {
      const c = card(ch);
      cards.set(ch.id, c);
      return c.el;
    });
    grid.replaceChildren(...els);
    grid.setAttribute('aria-label', `Channels (${plural(visible.length, 'channel')})`);
  }

  function scheduleReload(ms = REFRESH_MS) {
    clearTimeout(timer);
    timer = setTimeout(() => { if (!signal.aborted) load(); }, ms);
  }

  await load();
  ticker = setInterval(() => {
    for (const c of cards.values()) c.update();
  }, 5000);
  const refresh = setInterval(() => load(), REFRESH_MS);
  const onVisible = () => { if (document.visibilityState === 'visible') load(); };
  document.addEventListener('visibilitychange', onVisible);
  return () => {
    clearTimeout(timer);
    clearInterval(ticker);
    clearInterval(refresh);
    document.removeEventListener('visibilitychange', onVisible);
  };
}
