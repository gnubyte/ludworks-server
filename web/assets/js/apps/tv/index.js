// TV app (/tv/*): live channels, the program guide, the TV player, on-demand playback,
// lineups, channels, feed integrations and (from ./library/) the media library.
//   /tv                       live home (channel cards)
//   /tv/guide                 EPG grid
//   /tv/watch/<channel id>    old-school TV player (last channel when omitted)
//   /tv/play/<file id>        on-demand player for library items
//   /tv/lineups[/<id>]        lineups and the lineup editor
//   /tv/channels              channel manager
//   /tv/integrations          M3U / XMLTV / HDHomeRun
//   /tv/library/*             media library (library/index.js → mountLibrary)

import { h } from '../../core/dom.js';
import { ensureStyle } from '../../core/style.js';
import { tvApi } from './tvapi.js';
import { TABS, parseRoute } from './model.js';
import { revealActiveTab } from './shared.js';

const VIEWS = {
  live: { load: () => import('./live.js'), title: 'Live TV', layout: 'page' },
  guide: { load: () => import('./guide.js'), title: 'Guide', layout: 'full' },
  watch: { load: () => import('./player.js'), title: 'Watch', layout: 'immersive' },
  play: { load: () => import('./ondemand.js'), title: 'Play', layout: 'immersive' },
  lineups: { load: () => import('./lineups.js'), title: 'Lineups', layout: 'page', fn: 'renderList' },
  lineup: { load: () => import('./lineups.js'), title: 'Lineup', layout: 'page', fn: 'renderEditor' },
  channels: { load: () => import('./channels.js'), title: 'Channels', layout: 'page' },
  integrations: { load: () => import('./integrations.js'), title: 'Integrations', layout: 'page' },
};

export async function mount(container, ctx) {
  await ensureStyle('/assets/css/apps/tv.css');
  const { ui } = ctx;
  const api = tvApi(ctx.api);
  const nav = ui.tabs({ items: TABS.map((t) => ({ id: t.id, label: t.label, icon: t.icon, href: t.href })), ariaLabel: 'TV sections' });
  const bar = h('div', { class: 'tv-tabs-bar' }, h('div', { class: 'tv-tabs-inner' }, nav.el));
  const content = h('div', { class: 'tv-content' });
  const root = h('div', { class: 'tv-app' }, bar, content);
  container.append(root);

  ctx.commands.register([
    { id: 'tv:guide', title: 'TV: Open the guide', icon: 'calendar', keywords: ['epg', 'schedule'], run: () => ctx.router.navigate('/tv/guide') },
    { id: 'tv:watch', title: 'TV: Watch live TV', icon: 'tv', keywords: ['channel', 'player'], run: () => ctx.router.navigate('/tv/watch') },
    { id: 'tv:channels', title: 'TV: Manage channels', icon: 'radio', run: () => ctx.router.navigate('/tv/channels') },
    { id: 'tv:lineups', title: 'TV: Lineups', icon: 'list', run: () => ctx.router.navigate('/tv/lineups') },
    { id: 'tv:feeds', title: 'TV: M3U / XMLTV / HDHomeRun', icon: 'link', keywords: ['plex', 'jellyfin', 'iptv'], run: () => ctx.router.navigate('/tv/integrations') },
  ]);

  let active = null; // {key, cleanup, ctrl}
  let token = 0;

  function teardown() {
    if (!active) return;
    active.ctrl.abort();
    try {
      const c = active.cleanup;
      (typeof c === 'function' ? c : c?.unmount)?.();
    } catch (err) {
      console.error(err);
    }
    active = null;
  }

  function setLayout(kind) {
    root.dataset.layout = kind;
    bar.hidden = kind === 'immersive';
    ctx.setLayout({ full: kind !== 'page' });
  }

  async function show(c) {
    const route = parseRoute(c.params.rest);
    // The TV player stays mounted while zapping (it re-tunes in update()).
    const key = route.view === 'library' ? `library:${route.rest}` : route.view === 'watch' ? 'watch' : `${route.view}:${route.id ?? ''}`;
    if (active?.key === key) {
      active.cleanup?.update?.(c);
      return;
    }
    const my = ++token;
    teardown();
    nav.setActive(route.tab || null);
    revealActiveTab(nav.el);
    content.replaceChildren();
    const ctrl = new AbortController();
    const signal = AbortSignal.any ? AbortSignal.any([c.signal, ctrl.signal]) : ctrl.signal;
    active = { key, cleanup: null, ctrl };
    const mine = active;

    if (route.view === 'notfound') {
      setLayout('page');
      ctx.setTitle('TV');
      content.append(h('div', { class: 'page' }, ui.emptyState({
        icon: 'tv', title: 'Nothing on this channel', message: 'That TV page does not exist.',
        action: ui.button({ label: 'Back to live TV', icon: 'arrow-left', href: '/tv' }),
      })));
      return;
    }

    if (route.view === 'library') {
      setLayout('page');
      ctx.setTitle('Library');
      mine.cleanup = await mountLibraryView(content, c, route.rest, signal);
      if (active !== mine) (typeof mine.cleanup === 'function' ? mine.cleanup : mine.cleanup?.unmount)?.();
      return;
    }

    const def = VIEWS[route.view];
    setLayout(def.layout);
    ctx.setTitle(def.title);
    let mod;
    try {
      mod = await def.load();
    } catch (err) {
      console.error(err);
      if (my === token) content.replaceChildren(h('div', { class: 'page' }, ui.errorState(new Error('This part of the TV app failed to load.'), () => location.reload())));
      return;
    }
    if (my !== token) return;
    const vctx = { ctx: c, api, ui, signal, route, setTitle: ctx.setTitle, navigate: (p, o) => ctx.router.navigate(p, o) };
    try {
      mine.cleanup = (await mod[def.fn || 'render'](content, vctx)) || null;
    } catch (err) {
      console.error(err);
      if (active === mine) content.replaceChildren(h('div', { class: 'page' }, ui.errorState(err, () => ctx.router.reload())));
    }
    if (active !== mine) (typeof mine.cleanup === 'function' ? mine.cleanup : mine.cleanup?.unmount)?.();
  }

  /** The library half lives in ./library/ (separate module); degrade to a simple browser. */
  async function mountLibraryView(host, c, rest, signal) {
    const page = h('div', { class: 'tv-library-host' });
    host.append(page);
    try {
      const lib = await import('./library/index.js');
      if (typeof lib.mountLibrary !== 'function') throw new Error('library module has no mountLibrary');
      return (await lib.mountLibrary(page, c, rest)) || null;
    } catch (err) {
      console.warn('TV library module unavailable, using the built-in browser', err);
      const { renderFallbackLibrary } = await import('./picker.js');
      return renderFallbackLibrary(page, { ctx: c, api, ui, signal, rest });
    }
  }

  await show(ctx);
  return {
    update: (c) => show(c),
    unmount: () => teardown(),
  };
}
