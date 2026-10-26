// TV library — shows grid and the show page (header, seasons, episodes).

import { h, cls, debounce } from '../../../../core/dom.js';
import { icon } from '../../../../core/icons.js';
import { plural } from '../../../../core/format.js';
import { showCard, wideArt, progressBar, watchedToggle, skeletonGrid, artBox } from '../cards.js';
import { grid, viewHeader, showError } from './common.js';
import {
  itemHref, playHref, libraryHref, episodeCode, runtime, progressFraction, canResume, nextEpisode, seasonLabel,
  sizedUrl, remainingLabel,
} from '../model.js';
import { openBulkEditor } from '../bulk.js';
import { openIdentifyShow } from '../editor.js';

/* ------------------------------------------------------------------- list */

/**
 * @param {HTMLElement} root
 * @param {{ctx: object, api: object, signal: AbortSignal}} env
 */
export function renderShows(root, env) {
  const { ctx, api } = env;
  const { ui } = ctx;
  const search = h('input', { type: 'search', class: 'input input-sm tvl-inline-search', placeholder: 'Filter shows', aria: { label: 'Filter shows' } });
  const list = grid(skeletonGrid(12), { label: 'Shows' });
  const sentinel = h('div');
  root.replaceChildren(h('div', { class: 'tvl-browse' },
    viewHeader({ title: 'Shows', actions: [search] }), list, sentinel));
  let pager = null;
  let gen = 0;
  const load = () => {
    gen += 1;
    const my = gen;
    pager?.destroy();
    const q = search.value.trim();
    let count = 0;
    pager = ui.infiniteList({
      fetchPage: (cursor) => api.shows({ q: q || undefined, cursor: cursor || undefined, limit: 60 }, { signal: env.signal }),
      onItems: (items, { reset }) => {
        if (my !== gen) return;
        if (reset) list.replaceChildren();
        count += items.length;
        list.append(...items.map(showCard));
        if (!count) {
          list.replaceChildren(h('div', { class: 'tvl-state tvl-span-all' }, ui.emptyState({
            icon: 'tv',
            title: q ? 'No shows match' : 'No shows yet',
            message: q ? 'Try a different name.' : 'Episodes named like “Show.Name.S01E02.mkv” or stored in “Show/Season 1/” are grouped into shows. Run auto-tag on a folder to recognise them.',
          })));
        }
      },
    });
    sentinel.replaceChildren(pager.el);
    pager.loadMore();
  };
  search.addEventListener('input', debounce(load, 250));
  load();
  return { destroy: () => pager?.destroy() };
}

/* ------------------------------------------------------------------- show */

/**
 * @param {HTMLElement} root
 * @param {{ctx: object, api: object, signal: AbortSignal, key: string}} env
 */
export async function renderShow(root, env) {
  const { ctx, api, signal, key } = env;
  const { ui } = ctx;
  root.replaceChildren(h('div', { class: 'tvl-show' }, h('div', { class: 'tvl-hero skeleton' }), h('div', { class: 'tvl-grid' }, skeletonGrid(4, 'wide'))));
  let show;
  try {
    show = await api.show(key, { signal });
  } catch (err) {
    if (err?.status === 404) {
      root.replaceChildren(h('div', { class: 'tvl-state' }, ui.emptyState({
        icon: 'tv', title: 'Show not found', message: 'It may have been renamed or its episodes moved.',
        action: ui.button({ label: 'All shows', href: libraryHref('shows') }),
      })));
      return {};
    }
    showError(root, ui, err, () => renderShow(root, env));
    return {};
  }
  if (signal.aborted) return {};
  ctx.setTitle?.(show.show_title);
  const seasons = show.seasons || [];
  const allEpisodes = seasons.flatMap((s) => s.episodes);
  const params = new URLSearchParams(ctx.query?.season !== undefined ? `season=${ctx.query.season}` : '');
  let active = Number(params.get('season'));
  if (!seasons.some((s) => s.season === active)) active = (seasons.find((s) => s.season > 0) || seasons[0])?.season ?? 0;

  /* hero */
  const next = nextEpisode(seasons);
  const nm = next?.media || {};
  const playLabel = next ? (canResume(nm) ? `Resume ${episodeCode(nm.season, nm.episode)}` : `Play ${episodeCode(nm.season, nm.episode)}`.trim()) : 'Play';
  const meta = [show.year, plural(seasons.filter((s) => s.season > 0).length || seasons.length, 'season'), plural(show.episodes, 'episode')].filter(Boolean);
  const watchedAll = show.episodes > 0 && show.watched >= show.episodes;
  const hero = h('section', { class: cls('tvl-hero', { 'has-backdrop': !!show.backdrop_url }) },
    show.backdrop_url ? h('div', { class: 'tvl-hero-backdrop', aria: { hidden: 'true' } }, artBox({ title: show.show_title, kind: 'wide', src: sizedUrl(show.backdrop_url, 'lg') })) : null,
    h('div', { class: 'tvl-hero-inner' },
      h('div', { class: 'tvl-hero-poster' }, artBox({ title: show.show_title, src: show.poster_url ? sizedUrl(show.poster_url, 'lg') : '' })),
      h('div', { class: 'tvl-hero-text' },
        h('a', { class: 'tvl-back', href: libraryHref('shows') }, icon('chevron-left', 14), 'Shows'),
        h('h1', { class: 'tvl-hero-title' }, show.show_title),
        h('div', { class: 'tvl-hero-meta' }, meta.join(' · '),
          show.watched ? h('span', { class: 'tvl-hero-watched' }, ` · ${show.watched} watched`) : null),
        show.genres?.length ? h('div', { class: 'chips' }, show.genres.map((g) => h('a', { class: 'chip', href: libraryHref('browse', { genre: g }) }, g))) : null,
        show.description ? h('p', { class: 'tvl-hero-desc' }, show.description) : null,
        h('div', { class: 'tvl-hero-actions' },
          next ? ui.button({ label: playLabel, icon: 'play', variant: 'primary', href: playHref(next.id, canResume(nm) ? nm.position_seconds : null) }) : null,
          ui.button({
            label: watchedAll ? 'Mark unwatched' : 'Mark all watched', icon: watchedAll ? 'circle-dot' : 'check-circle',
            onClick: async () => {
              try {
                await api.bulk(allEpisodes.map((e) => e.id), { watched: !watchedAll });
                ui.toast.success(watchedAll ? 'Marked as unwatched' : 'Marked all episodes as watched');
                renderShow(root, env);
              } catch (err) { ui.toast.error(err); }
            },
          }),
          ui.menuButton({
            icon: 'more', title: 'More actions', variant: 'secondary',
            items: [
              { label: 'Edit all episodes…', icon: 'edit', onSelect: async () => { if (await openBulkEditor(ctx, allEpisodes.map((e) => e.id), { show: show.show_title })) renderShow(root, env); } },
              { label: 'Identify on TMDB…', icon: 'search', onSelect: async () => { const r = await openIdentifyShow(ctx, show); if (r) navigateToShow(r); } },
            ],
          })))));

  function navigateToShow(media) {
    const k = media?.media?.show_key;
    if (k && k !== key) ctx.router.navigate(libraryHref(`shows/${encodeURIComponent(k)}`), { replace: true });
    else renderShow(root, env);
  }

  /* seasons + episodes */
  const episodesHost = h('div', { class: 'tvl-episodes', role: 'list', aria: { label: 'Episodes' } });
  const renderSeason = (n) => {
    active = n;
    const s = seasons.find((x) => x.season === n);
    episodesHost.replaceChildren(...(s?.episodes || []).map((e) => episodeRow(e)));
  };
  const tabs = seasons.length > 1 ? ui.tabs({
    items: seasons.map((s) => ({ id: String(s.season), label: seasonLabel(s.season), badge: s.episodes.length })),
    active: String(active), variant: 'pills', ariaLabel: 'Seasons',
    onChange: (id) => {
      renderSeason(Number(id));
      ctx.router.setQuery({ season: id });
    },
  }) : null;

  function episodeRow(e) {
    const m = e.media;
    const title = m.episode_title || m.title;
    const frac = progressFraction(m);
    const href = itemHref(e.id);
    return h('div', { class: cls('tvl-episode', { 'is-watched': m.watched }), role: 'listitem' },
      h('a', { class: 'tvl-episode-art', href, tabindex: '-1', aria: { hidden: 'true' } },
        wideArt(e, { children: frac > 0 ? [progressBar(frac)] : [] })),
      h('div', { class: 'tvl-episode-main' },
        h('div', { class: 'tvl-episode-head' },
          h('span', { class: 'tvl-episode-code nums' }, episodeCode(m.season, m.episode) || '—'),
          h('a', { class: 'tvl-episode-title', href }, title)),
        h('div', { class: 'tvl-episode-meta' },
          [runtime(m.duration_seconds), remainingLabel(m), m.browser_playable ? '' : 'Needs transcoding'].filter(Boolean).join(' · ')),
        m.description ? h('p', { class: 'tvl-episode-desc' }, m.description) : null),
      h('div', { class: 'tvl-episode-actions' },
        watchedToggle(m.watched, async (nextState) => {
          try {
            await api.setWatched(e.id, nextState);
            m.watched = nextState;
          } catch (err) {
            ui.toast.error(err);
            throw err;
          }
        }),
        h('a', { class: 'btn btn-secondary btn-icon btn-sm', href: playHref(e.id, canResume(m) ? m.position_seconds : null), aria: { label: `Play ${title}` }, dataset: { tip: 'Play' } }, icon('play', 14))));
  }

  root.replaceChildren(h('div', { class: 'tvl-show' }, hero,
    h('section', { class: 'tvl-seasons' },
      h('div', { class: 'tvl-seasons-head' }, h('h2', { class: 'tvl-shelf-title' }, seasons.length > 1 ? 'Episodes' : seasonLabel(active)), tabs?.el || null),
      episodesHost)));
  renderSeason(active);
  return {};
}
