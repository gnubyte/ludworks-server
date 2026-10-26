// TV library — metadata editor (details, poster upload/drag-drop, TMDB identify, file
// info / re-probe) and the TMDB "identify show" dialog.

import { h, pickFiles } from '../../../core/dom.js';
import { icon } from '../../../core/icons.js';
import { formatBytes, formatDuration } from '../../../core/format.js';
import { field, showFormError, clearErrors } from '../../../core/forms.js';
import { mediaApi } from './api.js';
import { artBox } from './cards.js';
import { genreInput, categorySelects, checkRow } from './widgets.js';
import {
  displayTitle, sizedUrl, validateEdit, diffMedia, intField, resolutionLabel, isEpisode, episodeCode,
} from './model.js';

const RATINGS = ['G', 'PG', 'PG-13', 'R', 'NC-17', 'NR', 'TV-Y', 'TV-Y7', 'TV-G', 'TV-PG', 'TV-14', 'TV-MA'];
const POSTER_TYPES = 'image/jpeg,image/png,image/gif';

/**
 * Open the metadata editor for a media item.
 * @param {object} ctx app context
 * @param {string} fileId
 * @param {{tab?: 'details'|'poster'|'identify'|'file'}} [opts]
 * @returns {Promise<object|null>} the updated Media, or null when nothing changed
 */
export async function openMetadataEditor(ctx, fileId, opts = {}) {
  const { ui } = ctx;
  const api = mediaApi(ctx.api);
  let media;
  let categories = [];
  let genreSuggestions = [];
  try {
    [media, categories] = await Promise.all([api.get(fileId), api.categories().catch(() => [])]);
    const f = await api.facets({}).catch(() => null);
    genreSuggestions = (f?.genres || []).map((g) => g.value);
  } catch (err) {
    ui.toast.error(err);
    return null;
  }
  let changed = false;

  /* -------------------------------------------------------------- details */
  const inp = (name, props = {}) => h('input', { class: 'input', name, autocomplete: 'off', ...props });
  const f = {
    title: inp('title', { required: true, maxlength: '500' }),
    sort_title: inp('sort_title', { maxlength: '500' }),
    year: inp('year', { inputmode: 'numeric', placeholder: 'e.g. 1999', maxlength: '4' }),
    content_rating: inp('content_rating', { maxlength: '32', list: 'tvl-ratings' }),
    show_title: inp('show_title', { maxlength: '500' }),
    season: inp('season', { inputmode: 'numeric', maxlength: '5' }),
    episode: inp('episode', { inputmode: 'numeric', maxlength: '6' }),
    episode_title: inp('episode_title', { maxlength: '500' }),
    tmdb_id: inp('tmdb_id', { inputmode: 'numeric', maxlength: '13' }),
    imdb_id: inp('imdb_id', { maxlength: '16', placeholder: 'tt0063350', spellcheck: 'false' }),
    description: h('textarea', { class: 'textarea', name: 'description', rows: 5, maxlength: '20000' }),
  };
  const genres = genreInput({ value: media.media.genres, suggestions: genreSuggestions });
  const cats = categorySelects(categories, { category: media.category, subcategory: media.subcategory });
  const fill = (m) => {
    const mi = m.media;
    for (const k of ['title', 'sort_title', 'content_rating', 'show_title', 'episode_title', 'imdb_id', 'description']) f[k].value = mi[k] || '';
    for (const k of ['year', 'season', 'episode', 'tmdb_id']) f[k].value = mi[k] === null || mi[k] === undefined ? '' : String(mi[k]);
    f.sort_title.placeholder = 'Derived from the title';
    genres.set(mi.genres || []);
  };
  fill(media);
  const details = h('div', { class: 'tvl-editor-panel' },
    h('datalist', { id: 'tvl-ratings' }, RATINGS.map((r) => h('option', { value: r }))),
    h('div', { class: 'form-grid cols-2' },
      field({ label: 'Title', control: f.title, name: 'title', className: 'span-2', required: true }),
      field({ label: 'Sort title', control: f.sort_title, name: 'sort_title', hint: 'Used for ordering, e.g. without a leading “The”.' }),
      h('div', { class: 'form-grid cols-2' },
        field({ label: 'Year', control: f.year, name: 'year' }),
        field({ label: 'Rating', control: f.content_rating, name: 'content_rating' })),
      field({ label: 'Category', control: cats.category, name: 'category' }),
      field({ label: 'Type', control: cats.subcategory, name: 'subcategory' }),
      field({ label: 'Genres', control: genres.el, name: 'genres', className: 'span-2', hint: 'Press Enter or comma to add.' }),
      field({ label: 'Description', control: f.description, name: 'description', className: 'span-2' })),
    h('fieldset', { class: 'tvl-fieldset' },
      h('legend', {}, 'TV episode'),
      h('div', { class: 'form-grid cols-2' },
        field({ label: 'Show', control: f.show_title, name: 'show_title', className: 'span-2', hint: 'Episodes with the same show are grouped together.' }),
        h('div', { class: 'form-grid cols-2' },
          field({ label: 'Season', control: f.season, name: 'season' }),
          field({ label: 'Episode', control: f.episode, name: 'episode' })),
        field({ label: 'Episode title', control: f.episode_title, name: 'episode_title' }))),
    h('fieldset', { class: 'tvl-fieldset' },
      h('legend', {}, 'External IDs'),
      h('div', { class: 'form-grid cols-2' },
        field({ label: 'TMDB ID', control: f.tmdb_id, name: 'tmdb_id' }),
        field({ label: 'IMDb ID', control: f.imdb_id, name: 'imdb_id' }))));

  function readForm() {
    return {
      title: f.title.value.trim(), sort_title: f.sort_title.value.trim(), description: f.description.value.trim(),
      content_rating: f.content_rating.value.trim(), show_title: f.show_title.value.trim(), episode_title: f.episode_title.value.trim(),
      imdb_id: f.imdb_id.value.trim(), genres: genres.value,
      year: intField(f.year.value), season: intField(f.season.value), episode: intField(f.episode.value), tmdb_id: intField(f.tmdb_id.value),
    };
  }

  /* --------------------------------------------------------------- poster */
  const posterPreview = h('div', { class: 'tvl-editor-poster' });
  const posterStatus = h('div', { class: 'tvl-upload-status', aria: { live: 'polite' } });
  let bust = 0;
  const drawPoster = () => {
    const m = media.media;
    const src = m.poster_url ? sizedUrl(m.poster_url, 'lg') + (bust ? `&v=${bust}` : '') : '';
    posterPreview.replaceChildren(artBox({ title: displayTitle(m), src }));
  };
  drawPoster();
  const drop = h('div', { class: 'tvl-drop', tabindex: '0', role: 'button', aria: { label: 'Choose a poster image or drop one here' } },
    icon('image', 28),
    h('div', { class: 'tvl-drop-title' }, 'Drop an image here'),
    h('div', { class: 'muted small' }, 'or click to choose a JPEG, PNG or GIF (up to 20 MiB)'));
  const choose = async () => {
    const [file] = await pickFiles({ multiple: false, accept: POSTER_TYPES });
    if (file) upload(file);
  };
  drop.addEventListener('click', choose);
  drop.addEventListener('keydown', (e) => { if (e.key === 'Enter' || e.key === ' ') { e.preventDefault(); choose(); } });
  drop.addEventListener('dragover', (e) => {
    if (![...(e.dataTransfer?.types || [])].includes('Files')) return;
    e.preventDefault();
    e.dataTransfer.dropEffect = 'copy';
    drop.classList.add('is-over');
  });
  drop.addEventListener('dragleave', () => drop.classList.remove('is-over'));
  drop.addEventListener('drop', (e) => {
    e.preventDefault();
    drop.classList.remove('is-over');
    const file = [...(e.dataTransfer?.files || [])][0];
    if (file) upload(file);
  });
  async function upload(file) {
    if (file.size > 20 * 1024 * 1024) {
      ui.toast.error('That image is larger than 20 MiB.');
      return;
    }
    if (file.type && !POSTER_TYPES.split(',').includes(file.type)) {
      ui.toast.error(file.type === 'image/webp' ? 'WebP images are not supported; use JPEG or PNG.' : 'Choose a JPEG, PNG or GIF image.');
      return;
    }
    const bar = ui.progress({ value: 0, max: 1, size: 'sm', label: 'Uploading poster' });
    posterStatus.replaceChildren(h('span', { class: 'small muted' }, `Uploading ${file.name}…`), bar.el);
    drop.classList.add('is-busy');
    try {
      media = await api.uploadPoster(media.id, file, (p) => bar.set(p.fraction));
      changed = true;
      bust = Date.now();
      drawPoster();
      syncRemovePoster();
      posterStatus.replaceChildren(h('span', { class: 'small success-text' }, icon('check', 14), ' Poster updated'));
    } catch (err) {
      posterStatus.replaceChildren(h('span', { class: 'small danger-text' }, ui.errorMessage(err)));
    } finally {
      drop.classList.remove('is-busy');
    }
  }
  const removeBtn = ui.button({
    label: 'Remove custom poster', icon: 'trash', size: 'sm', variant: 'ghost',
    onClick: async () => {
      try {
        await api.deletePoster(media.id);
        media = await api.get(media.id);
        changed = true;
        bust = Date.now();
        drawPoster();
        syncRemovePoster();
        posterStatus.replaceChildren(h('span', { class: 'small muted' }, 'Custom poster removed — the video thumbnail is used when available.'));
      } catch (err) {
        ui.toast.error(err);
      }
    },
  });
  const removeBackdrop = ui.button({
    label: 'Remove backdrop', icon: 'trash', size: 'sm', variant: 'ghost',
    onClick: async () => {
      try {
        await api.deleteBackdrop(media.id);
        media = await api.get(media.id);
        changed = true;
        removeBackdrop.hidden = true;
      } catch (err) {
        ui.toast.error(err);
      }
    },
  });
  removeBackdrop.hidden = !media.media.backdrop_url;
  // Without any poster there is nothing to remove (it was offered for every item before).
  const syncRemovePoster = () => { removeBtn.hidden = !media.media.poster_url; };
  syncRemovePoster();
  const posterPanel = h('div', { class: 'tvl-editor-panel tvl-poster-panel' },
    posterPreview,
    h('div', { class: 'stack-sm tvl-poster-side' }, drop, posterStatus, h('div', { class: 'row' }, removeBtn, removeBackdrop),
      h('p', { class: 'muted small' }, 'Images are re-encoded as JPEG on the server. Identifying the title on TMDB also downloads a poster.')));

  /* ------------------------------------------------------------- identify */
  const identifyPanel = identifyView(ctx, api, () => media, (updated) => {
    media = updated;
    changed = true;
    fill(media);
    bust = Date.now();
    drawPoster();
    syncRemovePoster();
    removeBackdrop.hidden = !media.media.backdrop_url;
  });

  /* ----------------------------------------------------------------- file */
  const fileBody = h('div', { class: 'stack-sm' });
  const drawFile = () => {
    const m = media.media;
    fileBody.replaceChildren(
      ui.kv([
        ['File', h('span', { class: 'break' }, media.path)],
        ['Size', formatBytes(media.size)],
        ['Duration', m.duration_seconds ? formatDuration(m.duration_seconds) : '—'],
        ['Video', [m.video_codec?.toUpperCase(), m.width && m.height ? `${m.width}×${m.height}` : '', resolutionLabel(m.width, m.height)].filter(Boolean).join(' · ') || '—'],
        ['Audio', m.audio_codec?.toUpperCase() || '—'],
        ['Container', m.container?.toUpperCase() || '—'],
        ['Browser playback', m.browser_playable ? 'Direct' : 'Needs transcoding (ffmpeg)'],
        ['Subtitles', m.subtitles?.length ? m.subtitles.map((s) => s.label || s.language).join(', ') : 'None'],
        ['Analysis', { ok: 'Done', failed: 'Failed (unknown or damaged format)', pending: 'Pending' }[m.probe_status] || '—'],
      ]),
      h('div', { class: 'row' }, ui.button({
        label: 'Re-probe now', icon: 'refresh', size: 'sm',
        onClick: async () => {
          try {
            media = await api.probe(media.id);
            changed = true;
            drawFile();
            ui.toast.success('File analysed');
          } catch (err) { ui.toast.error(err); }
        },
      })));
  };
  drawFile();
  const filePanel = h('div', { class: 'tvl-editor-panel' }, fileBody);

  /* ---------------------------------------------------------------- modal */
  const panels = { details, poster: posterPanel, identify: identifyPanel, file: filePanel };
  const tabs = ui.tabs({
    items: [
      { id: 'details', label: 'Details', icon: 'edit' },
      { id: 'poster', label: 'Poster', icon: 'image' },
      { id: 'identify', label: 'Identify', icon: 'search' },
      { id: 'file', label: 'File', icon: 'file' },
    ],
    active: opts.tab && panels[opts.tab] ? opts.tab : 'details',
    ariaLabel: 'Editor sections',
    onChange: (id) => showTab(id),
  });
  const showTab = (id) => {
    for (const [k, p] of Object.entries(panels)) p.hidden = k !== id;
    if (id === 'identify') identifyPanel.focusSearch?.();
  };
  showTab(opts.tab && panels[opts.tab] ? opts.tab : 'details');

  const title = isEpisode(media.media)
    ? `${media.media.show_title} ${episodeCode(media.media.season, media.media.episode)}`.trim()
    : displayTitle(media.media);
  const m = ui.modal({
    title: 'Edit metadata',
    description: title || media.name,
    size: 'lg',
    className: 'tvl-editor',
    body: h('div', { class: 'stack' }, tabs.el, ...Object.values(panels)),
    footer: [{ label: 'Close', value: undefined }, { label: 'Save changes', variant: 'primary', type: 'submit' }],
    onSubmit: async (modal, form) => {
      clearErrors(details);
      const data = readForm();
      const errors = validateEdit(data);
      const patch = diffMedia(media.media, data);
      const cat = cats.value;
      if (cat.category !== (media.category || '')) patch.category = cat.category;
      if (cat.subcategory !== (media.subcategory || '') || 'category' in patch) patch.subcategory = cat.subcategory;
      if (Object.keys(errors).length) {
        tabs.setActive('details');
        showTab('details');
        showFormError(details, { fields: errors });
        return false;
      }
      if (!Object.keys(patch).length) return changed ? media : null;
      try {
        media = await api.patch(media.id, patch);
      } catch (err) {
        if (err.fields) {
          tabs.setActive('details');
          showTab('details');
          showFormError(details, err);
          return false;
        }
        throw err;
      }
      ui.toast.success('Saved');
      return media;
    },
  });
  const result = await m.closed;
  if (result && typeof result === 'object') return result;
  return changed ? media : null;
}

/**
 * The TMDB search + identify panel.
 * @param {object} ctx
 * @param {object} api mediaApi
 * @param {() => object} getMedia current Media
 * @param {(m: object) => void} onIdentified
 * @param {{applyToShow?: boolean}} [o]
 */
function identifyView(ctx, api, getMedia, onIdentified, o = {}) {
  const { ui } = ctx;
  const m0 = getMedia().media;
  let type = isEpisode(m0) ? 'tv' : 'movie';
  const q = h('input', { class: 'input', type: 'search', placeholder: 'Title', aria: { label: 'Search TMDB' }, maxlength: '200' });
  const year = h('input', { class: 'input tvl-year-input', inputmode: 'numeric', placeholder: 'Year', aria: { label: 'Year' }, maxlength: '4' });
  q.value = type === 'tv' ? m0.show_title : m0.title;
  year.value = type === 'movie' && m0.year ? String(m0.year) : '';
  const typeSeg = ui.segmented({
    options: [{ value: 'movie', label: 'Movie', icon: 'film' }, { value: 'tv', label: 'TV show', icon: 'tv' }],
    value: type, size: 'sm', ariaLabel: 'Type',
    onChange: (v) => { type = v; applyRow.hidden = v !== 'tv'; },
  });
  const apply = checkRow('Apply to every episode of this show', o.applyToShow ?? true, 'Copies the show title, overview, genres, rating and poster to the other episodes in the same space.');
  const applyRow = h('div', {}, apply);
  applyRow.hidden = type !== 'tv';
  const results = h('div', { class: 'tvl-tmdb-results', role: 'list', aria: { label: 'TMDB results', live: 'polite' } });
  const searchBtn = ui.button({ label: 'Search', icon: 'search', type: 'button', onClick: () => search() });
  const formRow = h('div', { class: 'tvl-tmdb-form' }, typeSeg.el, q, year, searchBtn);
  q.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); search(); } });
  year.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); search(); } });

  async function search() {
    const text = q.value.trim();
    if (!text) {
      q.focus();
      return;
    }
    results.replaceChildren(ui.loadingBlock('Searching TMDB…'));
    try {
      const y = /^\d{4}$/.test(year.value.trim()) ? year.value.trim() : undefined;
      const res = await api.tmdbSearch({ q: text, type, year: y });
      if (!res.results.length) {
        results.replaceChildren(ui.emptyState({ icon: 'search', title: 'No matches', message: 'Try a shorter title or remove the year.', compact: true }));
        return;
      }
      results.replaceChildren(...res.results.map(resultRow));
    } catch (err) {
      results.replaceChildren(errorBox(err));
    }
  }

  function errorBox(err) {
    if (err?.status === 501) {
      return ui.alert({
        variant: 'info', title: 'TMDB is not set up',
        message: ctx.isAdmin
          ? h('span', {}, 'Add a TMDB API key in ', h('a', { href: '/admin/media' }, 'Admin › Media & TV'), ' to search The Movie Database.')
          : 'Ask an administrator to add a TMDB API key in the server settings.',
      });
    }
    return ui.alert({ variant: 'danger', title: 'TMDB search failed', message: ui.errorMessage(err) });
  }

  function resultRow(r) {
    const use = ui.button({
      label: 'Use this', size: 'sm', variant: 'primary',
      onClick: async () => {
        try {
          const media = await api.identify(getMedia().id, { tmdb_id: r.tmdb_id, type: r.type, apply_to_show: r.type === 'tv' && apply.inputEl.checked });
          onIdentified(media);
          ui.toast.success(`Identified as ${r.title}${r.year ? ` (${r.year})` : ''}`);
          results.replaceChildren(ui.alert({ variant: 'success', title: 'Metadata updated', message: `${r.title} — title, description, genres and artwork were applied.` }));
        } catch (err) {
          ui.toast.error(err);
        }
      },
    });
    return h('div', { class: 'tvl-tmdb-result', role: 'listitem' },
      artBox({ title: r.title, kind: 'poster', className: 'tvl-tmdb-art' }),
      h('div', { class: 'tvl-tmdb-text' },
        h('div', { class: 'tvl-tmdb-title' }, r.title, r.year ? h('span', { class: 'muted' }, ` (${r.year})`) : null),
        r.original_title && r.original_title !== r.title ? h('div', { class: 'muted small' }, r.original_title) : null,
        r.overview ? h('p', { class: 'tvl-tmdb-overview' }, r.overview) : null,
        h('div', { class: 'faint xs' }, `TMDB ${r.type === 'tv' ? 'TV' : 'movie'} #${r.tmdb_id}`)),
      use);
  }

  const panel = h('div', { class: 'tvl-editor-panel stack-sm' }, formRow, applyRow, results,
    h('p', { class: 'faint xs' }, 'Data and images from The Movie Database (TMDB). Search requests go through the server; your API key is never sent to the browser.'));
  panel.focusSearch = () => requestAnimationFrame(() => q.focus());
  return panel;
}

/**
 * Identify a whole show: search TMDB (TV) and apply the result to every episode.
 * @returns {Promise<object|null>} the updated representative episode, or null
 */
export async function openIdentifyShow(ctx, show) {
  const { ui } = ctx;
  const api = mediaApi(ctx.api);
  const first = show.seasons?.flatMap((s) => s.episodes)?.[0];
  if (!first) return null;
  let media = first;
  let result = null;
  const panel = identifyView(ctx, api, () => media, (m) => { media = m; result = m; modal.close(m); }, { applyToShow: true });
  const modal = ui.modal({
    title: 'Identify show', description: show.show_title, size: 'lg', className: 'tvl-editor',
    body: panel, footer: [{ label: 'Close', value: null }],
  });
  panel.focusSearch();
  const closed = await modal.closed;
  return closed || result;
}
