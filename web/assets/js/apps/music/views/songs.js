// Songs: the whole library as one virtualized list, sorted by the server and loaded page by page
// (never everything at once); Play / Shuffle start from the server's order and keep loading as
// the queue runs low.

import { h } from '../../../core/dom.js';
import * as ui from '../../../core/ui.js';
import { mapi, playTracks, trackFeed } from '../widgets.js';
import { SONG_SORTS, normalizeSort, songCount } from '../model.js';
import { pageHead, sortControl, toolbar, pagedTrackList, cached, remember } from './common.js';

export function mountSongs(host, app, route) {
  app.setTitle('Songs');
  let st = normalizeSort(SONG_SORTS, route.query.sort, route.query.order);
  const countEl = h('span', { class: 'muted small mu-count', aria: { live: 'polite' } });
  const page = h('div', { class: 'mu-page mu-songs' });
  host.append(page);
  let view = null;

  mapi.facets().then((f) => {
    if (f?.totals) countEl.textContent = songCount(f.totals.tracks);
  }).catch(() => {});

  const query = () => ({ sort: st.sort, order: st.order });

  async function playAll(shuffle) {
    const q = shuffle ? { sort: 'random', seed: Math.random().toString(36).slice(2, 10) } : query();
    try {
      const first = await mapi.tracks({ ...q, limit: 200 });
      playTracks(first.items || [], { index: 0, shuffle, more: first.next_cursor ? trackFeed(q, first.next_cursor) : null });
    } catch (err) {
      ui.toast.error(err);
    }
  }

  function build() {
    view?.destroy();
    const key = `songs:${st.sort}:${st.order}`;
    view = pagedTrackList({ label: 'Songs', art: true, album: true }, {
      fetchPage: (cursor) => mapi.tracks({ ...query(), cursor, limit: 200 }),
      initial: cached(key),
      onItems: (all, cursor) => remember(key, all, cursor),
      more: (cursor) => trackFeed(query(), cursor),
    });
    view.list.setEmpty(ui.emptyState({ icon: 'music', title: 'No songs yet', message: 'Audio files in your spaces show up here.', action: ui.button({ label: 'Open Files', icon: 'folder', href: '/files' }) }));
    page.replaceChildren(
      pageHead('Songs', { subtitle: '', actions: [
        ui.button({ label: 'Play', icon: 'play', variant: 'primary', onClick: () => playAll(false) }),
        ui.button({ label: 'Shuffle', icon: 'shuffle', onClick: () => playAll(true) }),
      ] }),
      toolbar(sortControl(SONG_SORTS, st, (v) => app.setQuery({ sort: v.sort, order: v.order })), h('span', { class: 'spacer' }), countEl),
      view.el);
  }
  build();

  return {
    update(r) {
      const next = normalizeSort(SONG_SORTS, r.query.sort, r.query.order);
      if (next.sort !== st.sort || next.order !== st.order || !view) {
        st = next;
        build();
      }
    },
    destroy() {
      view?.destroy();
    },
  };
}
