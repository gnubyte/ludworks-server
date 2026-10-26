// TV library — item detail: poster, metadata, play/resume, file and codec info,
// subtitles, and for episodes the rest of the season.

import { h, cls } from '../../../../core/dom.js';
import { icon } from '../../../../core/icons.js';
import { formatBytes, formatDate, formatDuration } from '../../../../core/format.js';
import { artBox, wideCard, watchedToggle, progressBar } from '../cards.js';
import { shelf, showError } from './common.js';
import {
  displayTitle, episodeCode, runtime, resolutionLabel, isEpisode, canResume, playHref, showHref, libraryHref, sizedUrl,
  progressFraction, remainingLabel, subtitleFormat,
} from '../model.js';
import { openMetadataEditor } from '../editor.js';

function parentDir(path) {
  const i = String(path || '').lastIndexOf('/');
  return i > 0 ? path.slice(0, i) : '';
}

/**
 * @param {HTMLElement} root
 * @param {{ctx: object, api: object, signal: AbortSignal, id: string}} env
 */
export async function renderItem(root, env) {
  const { ctx, api, signal, id } = env;
  const { ui } = ctx;
  root.replaceChildren(h('div', { class: 'tvl-item' }, h('div', { class: 'tvl-hero skeleton' })));
  let item;
  try {
    item = await api.get(id, { signal });
  } catch (err) {
    if (err?.status === 404) {
      root.replaceChildren(h('div', { class: 'tvl-state' }, ui.emptyState({
        icon: 'film', title: 'Not found', message: 'This item does not exist, is not a video, or you do not have access to it.',
        action: ui.button({ label: 'Back to the library', href: libraryHref() }),
      })));
      return {};
    }
    showError(root, ui, err, () => renderItem(root, env));
    return {};
  }
  if (signal.aborted) return {};
  draw(item);

  function draw(media) {
    item = media;
    const m = media.media;
    const title = displayTitle(m);
    ctx.setTitle?.(title);
    const ep = isEpisode(m);
    const res = resolutionLabel(m.width, m.height);
    const metaLine = [
      ep ? episodeCode(m.season, m.episode) : null,
      m.year,
      runtime(m.duration_seconds),
      m.content_rating || null,
      res || null,
    ].filter(Boolean);
    const resume = canResume(m);
    const frac = progressFraction(m);

    const actions = h('div', { class: 'tvl-hero-actions' },
      resume
        ? ui.button({ label: `Resume · ${remainingLabel(m) || formatDuration(m.position_seconds)}`, icon: 'play', variant: 'primary', href: playHref(media.id, m.position_seconds) })
        : ui.button({ label: 'Play', icon: 'play', variant: 'primary', href: playHref(media.id, 0) }),
      resume ? ui.button({ label: 'Play from start', icon: 'restore', href: playHref(media.id, 0) }) : null,
      watchedToggle(m.watched, async (next) => {
        try {
          await api.setWatched(media.id, next);
          draw(await api.get(media.id, { signal }));
        } catch (err) {
          ui.toast.error(err);
          throw err;
        }
      }, { size: 'md', labelled: true }),
      ui.button({ label: 'Edit', icon: 'edit', onClick: () => edit() }),
      ui.menuButton({
        icon: 'more', title: 'More actions', variant: 'secondary',
        items: () => [
          { label: 'Identify on TMDB…', icon: 'search', onSelect: () => edit('identify') },
          { label: 'Change poster…', icon: 'image', onSelect: () => edit('poster') },
          { label: 'Re-probe file', icon: 'refresh', onSelect: () => reprobe() },
          { separator: true },
          { label: 'Show in Files', icon: 'folder', onSelect: () => ctx.router.navigate(`/files?space=${encodeURIComponent(media.space_id)}&path=${encodeURIComponent(parentDir(media.path))}`) },
          { label: 'Download', icon: 'download', onSelect: () => ctx.api.download(`/files/${encodeURIComponent(media.id)}/content`, { download: 1 }) },
        ],
      }));

    const hero = h('section', { class: cls('tvl-hero', { 'has-backdrop': !!m.backdrop_url }) },
      m.backdrop_url ? h('div', { class: 'tvl-hero-backdrop', aria: { hidden: 'true' } }, artBox({ title, kind: 'wide', src: sizedUrl(m.backdrop_url, 'lg') })) : null,
      h('div', { class: 'tvl-hero-inner' },
        h('div', { class: 'tvl-hero-poster' },
          artBox({ title: ep ? m.show_title : title, src: m.poster_url ? sizedUrl(m.poster_url, 'lg') : '', children: frac > 0 ? [progressBar(frac)] : [] })),
        h('div', { class: 'tvl-hero-text' },
          ep ? h('a', { class: 'tvl-back', href: showHref(m.show_key) }, icon('chevron-left', 14), m.show_title)
            : h('a', { class: 'tvl-back', href: libraryHref(media.category === 'movie' ? 'movies' : '') }, icon('chevron-left', 14), media.category === 'movie' ? 'Movies' : 'Library'),
          h('h1', { class: 'tvl-hero-title' }, title),
          metaLine.length ? h('div', { class: 'tvl-hero-meta' }, metaLine.join(' · '), m.watched ? h('span', { class: 'tvl-hero-watched' }, ' · Watched') : null) : null,
          m.genres?.length ? h('div', { class: 'chips' }, m.genres.map((g) => h('a', { class: 'chip', href: libraryHref('browse', { genre: g }) }, g))) : null,
          m.description ? h('p', { class: 'tvl-hero-desc' }, m.description) : h('p', { class: 'tvl-hero-desc muted' }, 'No description yet. ',
            h('button', { type: 'button', class: 'link-button', onClick: () => edit('identify') }, 'Identify on TMDB'), ' or ',
            h('button', { type: 'button', class: 'link-button', onClick: () => edit() }, 'edit the details'), '.'),
          actions)));

    const playable = m.browser_playable
      ? ui.badge('Plays in the browser', 'success', { icon: 'check' })
      : ui.badge(m.probe_status === 'pending' ? 'Analysing…' : 'Needs transcoding', m.probe_status === 'pending' ? 'neutral' : 'warning', { icon: m.probe_status === 'pending' ? 'clock' : 'zap' });
    const info = ui.card({
      title: 'File', icon: 'file',
      body: h('div', { class: 'stack-sm' },
        h('div', { class: 'tvl-tech' }, playable,
          res ? ui.badge(res) : null,
          m.video_codec ? ui.badge(m.video_codec.toUpperCase()) : null,
          m.audio_codec ? ui.badge(m.audio_codec.toUpperCase()) : null,
          m.container ? ui.badge(m.container.toUpperCase()) : null,
          m.probe_status === 'failed' ? ui.badge('Probe failed', 'danger') : null),
        ui.kv([
          ['Name', h('span', { class: 'break' }, media.name)],
          ['Folder', h('a', { href: `/files?space=${encodeURIComponent(media.space_id)}&path=${encodeURIComponent(parentDir(media.path))}`, class: 'break' }, parentDir(media.path) || '/')],
          ['Size', formatBytes(media.size)],
          ['Duration', m.duration_seconds ? formatDuration(m.duration_seconds) : '—'],
          ['Resolution', m.width && m.height ? `${m.width} × ${m.height}` : '—'],
          ['Added', formatDate(m.added_at)],
          m.imdb_id ? ['IMDb', m.imdb_id] : null,
          m.tmdb_id ? ['TMDB', String(m.tmdb_id)] : null,
        ])),
    });
    const subs = ui.card({
      title: 'Subtitles', icon: 'file-text',
      body: m.subtitles?.length
        ? h('ul', { class: 'tvl-subs' }, m.subtitles.map((s) => h('li', {},
          h('span', { class: 'tvl-sub-label' }, s.label || s.language || `Track ${s.index + 1}`),
          s.language ? ui.badge(s.language) : null,
          ui.badge(subtitleFormat(s.format), s.format === 'embedded' ? 'info' : 'neutral'),
          h('a', { class: 'btn btn-ghost btn-sm', href: api.subtitleUrl(media.id, s.index), target: '_blank', rel: 'noopener', dataset: { native: '' } }, icon('external', 13), 'Open'))))
        : h('p', { class: 'muted small' }, 'No subtitles. Put a file such as “', h('span', { class: 'mono' }, `${media.name.replace(/\.[^.]+$/, '')}.en.srt`), '” next to the video.'),
    });

    const body = h('div', { class: 'tvl-item-body' }, info, subs);
    const page = h('div', { class: 'tvl-item' }, hero, body);
    root.replaceChildren(page);
    if (ep && m.show_key) loadSeason(page, m);
  }

  async function loadSeason(page, m) {
    try {
      const q = { show: m.show_key, sort: 'episode', limit: 100 };
      if (Number.isInteger(m.season)) q.season = m.season;
      const res = await api.list(q, { signal });
      const others = res.items.filter((x) => x.id !== item.id);
      if (!others.length || signal.aborted) return;
      page.append(shelf({
        title: Number.isInteger(m.season) ? `More from season ${m.season}` : `More from ${m.show_title}`,
        href: showHref(m.show_key), items: others.map((x) => wideCard(x, { showName: false })), wide: true,
      }));
    } catch {
      /* optional */
    }
  }

  async function edit(tab) {
    const updated = await openMetadataEditor(ctx, item.id, { tab });
    if (updated) draw(updated);
  }

  async function reprobe() {
    try {
      const updated = await api.probe(item.id);
      draw(updated);
      ui.toast.success(updated.media.probe_status === 'ok' ? 'File analysed' : 'The file could not be analysed');
    } catch (err) {
      ui.toast.error(err);
    }
  }

  return {};
}
