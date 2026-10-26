// TV library — the media-library half of the TV app (/tv/library/*). The TV app's router
// mounts it and imports exactly these functions:
//
//   mountLibrary(container, ctx, rest) → {unmount, update(ctx, rest)}
//       rest = path after /tv/library: '' | 'movies' | 'shows' | 'shows/<key>' |
//       'item/<fileId>' | 'genres' | 'years' | 'recent' | 'unwatched' | 'videos' |
//       'search' | 'browse' (filters in the query string: q, genre, year, …)
//   openMetadataEditor(ctx, fileId)  → Promise<Media|null>
//   pickMedia(ctx, {mode, multiple}) → Promise<LineupItem[]|null>
//
// Play buttons link to /tv/play/<fileId>?t=<seconds> (the TV app's player).

import { h, cls, debounce, isEditable } from '../../../core/dom.js';
import { icon } from '../../../core/icons.js';
import { ensureStyle } from '../../../core/style.js';
import { mediaApi } from './api.js';
import { VIEWS, parseRoute, libraryHref } from './model.js';
import { renderHome } from './views/home.js';
import { renderBrowse } from './views/browse.js';
import { renderShows, renderShow } from './views/shows.js';
import { renderItem } from './views/item.js';
import { renderGenres, renderYears } from './views/genres.js';
import { openAutoTag } from './autotag.js';
import { openMetadataEditor } from './editor.js';
import { pickMedia } from './picker.js';

export { openMetadataEditor, pickMedia };

const NAV_PRIMARY = ['home', 'movies', 'shows', 'genres'];

/**
 * Mount the library into container.
 * @param {HTMLElement} container
 * @param {object} ctx app context (see apps/registry.js)
 * @param {string} rest path after /tv/library
 */
export async function mountLibrary(container, ctx, rest = '') {
  await ensureStyle('/assets/css/apps/tv-library.css');
  let current = { ctx, rest: null, routeKey: '', view: null, ctrl: null };
  const api = mediaApi(ctx.api);

  /* --------------------------------------------------------------- header */
  const navLinks = new Map();
  const navItem = (v) => {
    const a = h('a', { class: 'tvl-nav-item', href: libraryHref(v.path) }, icon(v.icon, 15), h('span', {}, v.label));
    navLinks.set(v.id, a);
    return a;
  };
  const primary = VIEWS.filter((v) => NAV_PRIMARY.includes(v.id));
  const secondary = VIEWS.filter((v) => !NAV_PRIMARY.includes(v.id));
  const nav = h('nav', { class: 'tvl-nav', aria: { label: 'Library' } }, primary.map(navItem), secondary.map(navItem));
  const search = h('input', {
    type: 'search', class: 'input tvl-search-input', placeholder: 'Search library', aria: { label: 'Search the library' },
    autocomplete: 'off', spellcheck: 'false', maxlength: '200',
  });
  const searchForm = h('form', { class: 'tvl-search', role: 'search' }, icon('search', { size: 15, className: 'tvl-search-icon' }), search);
  const autoTagBtn = ctx.ui.button({ label: 'Auto-tag', icon: 'sparkle', size: 'sm', variant: 'ghost', title: 'Recognise movies and episodes in a folder', onClick: () => autoTag() });
  const head = h('div', { class: 'tvl-head' }, nav, h('div', { class: 'tvl-head-tools' }, searchForm, autoTagBtn));
  const body = h('div', { class: 'tvl-body' });
  const root = h('div', { class: 'tvl' }, head, body);
  container.replaceChildren(root);

  const goSearch = (q, replace) => {
    const text = q.trim();
    if (!text && parseRoute(current.rest).preset === 'search') {
      current.ctx.router.navigate(libraryHref(), { replace: true });
      return;
    }
    if (!text) return;
    current.ctx.router.navigate(libraryHref('search', { q: text }), { replace });
  };
  searchForm.addEventListener('submit', (e) => {
    e.preventDefault();
    goSearch(search.value, parseRoute(current.rest).preset === 'search');
  });
  search.addEventListener('input', debounce(() => {
    if (parseRoute(current.rest).preset === 'search' || search.value.trim().length >= 2) goSearch(search.value, parseRoute(current.rest).preset === 'search');
  }, 300));

  // "/" focuses the library search (unless typing somewhere).
  const onKey = (e) => {
    if (e.key === '/' && !e.ctrlKey && !e.metaKey && !e.altKey && !isEditable(document.activeElement) && !document.querySelector('.modal-backdrop')) {
      e.preventDefault();
      search.focus();
      search.select();
    }
  };
  document.addEventListener('keydown', onKey);

  async function autoTag() {
    const res = await openAutoTag(current.ctx);
    if (res?.updated) show(current.rest, true);
  }

  /* --------------------------------------------------------------- routing */
  async function show(r, force = false) {
    const route = parseRoute(r);
    const routeKey = `${route.view}:${route.preset || route.key || route.id || ''}`;
    const prevRest = current.rest;
    current.rest = r;
    // Same view: let it react to the new query in place.
    if (!force && routeKey === current.routeKey && current.view) {
      if (current.view.update) current.view.update(current.ctx);
      return;
    }
    current.ctrl?.abort();
    current.view?.destroy?.();
    current.view = null;
    current.routeKey = routeKey;
    const ctrl = new AbortController();
    current.ctrl = ctrl;
    const activeNav = route.view === 'browse' ? route.preset : route.view === 'show' ? 'shows' : route.view === 'item' ? '' : route.view;
    for (const [id, a] of navLinks) {
      const on = id === activeNav;
      a.classList.toggle('is-active', on);
      if (on) a.setAttribute('aria-current', 'page'); else a.removeAttribute('aria-current');
    }
    if (route.preset === 'search') search.value = current.ctx.query?.q || search.value;
    else if (prevRest !== null && document.activeElement !== search) search.value = '';
    root.classList.toggle('is-detail', route.view === 'item' || route.view === 'show');
    current.ctx.setTitle?.(titleFor(route));
    const viewRoot = h('div', { class: cls('tvl-view', `tvl-view-${route.view}`) });
    body.replaceChildren(viewRoot);
    const env = { ctx: current.ctx, api, signal: ctrl.signal, autoTag };
    let view = null;
    try {
      switch (route.view) {
        case 'home': await renderHome(viewRoot, env); break;
        case 'browse': view = renderBrowse(viewRoot, { ...env, preset: route.preset }); break;
        case 'shows': view = renderShows(viewRoot, env); break;
        case 'show': view = await renderShow(viewRoot, { ...env, key: route.key }); break;
        case 'item': view = await renderItem(viewRoot, { ...env, id: route.id }); break;
        case 'genres': view = await renderGenres(viewRoot, env); break;
        case 'years': view = await renderYears(viewRoot, env); break;
        default:
          viewRoot.replaceChildren(h('div', { class: 'tvl-state' }, current.ctx.ui.emptyState({
            icon: 'alert-circle', title: 'Page not found', action: current.ctx.ui.button({ label: 'Library home', href: libraryHref() }),
          })));
      }
    } catch (err) {
      if (!ctrl.signal.aborted) {
        console.error(err);
        viewRoot.replaceChildren(h('div', { class: 'tvl-state' }, current.ctx.ui.errorState(err, () => show(current.rest, true))));
      }
    }
    if (current.ctrl === ctrl) current.view = view || {};
    else view?.destroy?.();
  }

  function titleFor(route) {
    // The library's own "Home" view would read as the app's Home page in the top bar and tab.
    if (route.view === 'home') return 'Library';
    if (route.view === 'browse') return { movies: 'Movies', recent: 'Recently added', unwatched: 'Unwatched', videos: 'Other videos', search: 'Search' }[route.preset] || 'Library';
    return VIEWS.find((v) => v.id === route.view)?.label || 'Library';
  }

  await show(rest);
  // Remounted while typing a search (the host may remount per path): keep typing.
  if (parseRoute(rest).preset === 'search' && (!document.activeElement || document.activeElement === document.body)) {
    search.focus();
    search.setSelectionRange(search.value.length, search.value.length);
  }

  return {
    unmount() {
      current.ctrl?.abort();
      current.view?.destroy?.();
      document.removeEventListener('keydown', onKey);
    },
    // The TV router may call update(ctx) for query-only changes: keep the current path then.
    update(nextCtx, nextRest) {
      current.ctx = nextCtx;
      return show(typeof nextRest === 'string' ? nextRest : current.rest);
    },
  };
}
