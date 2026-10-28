// Kanban app: boards grid (/kanban), cards assigned to me across boards (/kanban/my), a board
// (/kanban/<board>) and a card over its board (/kanban/<board>/c/<card>, deep-linkable).
// Each view is a small controller {update(route), destroy()}; navigating within a board keeps
// it mounted (polling, scroll positions, drafts) and only opens or closes the card.

import { h } from '../../core/dom.js';
import { ensureStyle } from '../../core/style.js';
import { isApple } from '../../core/shortcuts.js';
import { kanbanApi } from './api.js';
import { parseRoute } from './model.js';
import { mountBoards } from './boards.js';
import { mountMyCards } from './mycards.js';
import { mountBoard } from './board.js';

export async function mount(container, ctx) {
  await ensureStyle('/assets/css/apps/kanban.css');
  // drafts: unsaved comment and description text per card, kept while the app is open.
  const app = { ctx, kapi: kanbanApi(ctx.api), me: ctx.user?.id || '', isApple: isApple(), drafts: new Map() };
  const host = h('div', { class: 'kb-app' });
  container.append(host);
  let view = null;
  let viewKey = '';

  function route(c) {
    const r = parseRoute(c.params?.rest || '');
    const info = { ...r, query: c.query || {} };
    const key = r.view === 'board' ? `board:${r.boardId}` : r.view;
    if (view && key === viewKey) {
      view.update(info);
      return;
    }
    view?.destroy();
    host.replaceChildren();
    const el = h('div', { class: 'kb-view' });
    host.append(el);
    viewKey = key;
    if (r.view === 'board') view = mountBoard(el, app, info);
    else if (r.view === 'my') view = mountMyCards(el, app, info);
    else view = mountBoards(el, app, info);
    host.dataset.view = r.view;
  }

  route(ctx);
  return {
    update: (c) => route(c),
    unmount: () => {
      view?.destroy();
      view = null;
    },
  };
}
