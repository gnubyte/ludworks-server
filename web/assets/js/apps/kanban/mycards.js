// "My cards": cards assigned to the caller across boards, grouped into overdue, today, this
// week, later and no date (local calendar days).

import { h, cls, mount as fill } from '../../core/dom.js';
import { kicon } from './icons.js';
import { paths, colorClass, groupByBucket, priorityLabel, progressOf } from './model.js';
import { dueBadge } from './widgets.js';

const MAX_ITEMS = 1000;

/**
 * Mount the "My cards" view.
 * @returns {{update: (route: object) => void, destroy: () => void}}
 */
export function mountMyCards(container, app) {
  const { ctx, kapi } = app;
  const { ui } = ctx;
  const S = { cards: [], q: '', completed: false, seq: 0, truncated: false, boardColors: new Map() };
  const offs = [];

  ctx.setLayout({});
  ctx.setTitle('My cards');
  ctx.setActions(null);
  ctx.setSearch({ placeholder: 'Search my cards', value: '', onInput: (v) => { S.q = v.trim(); load(); }, onSubmit: (v) => { S.q = v.trim(); load(); } });

  const toggle = h('input', { type: 'checkbox', class: 'switch-input', aria: { label: 'Include completed cards' } });
  toggle.addEventListener('change', () => { S.completed = toggle.checked; load(); });
  const header = ui.pageHeader({
    title: 'My cards',
    subtitle: 'Cards assigned to you on all your boards, by due date.',
    actions: [
      h('label', { class: 'kb-switch-row' }, h('span', { class: 'switch' }, toggle, h('span', { class: 'switch-track' })), h('span', {}, 'Include completed')),
      ui.button({ label: 'All boards', icon: 'kanban', href: paths.boards() }),
    ],
  });
  const body = h('div', { class: 'kb-my' });
  container.replaceChildren(h('div', { class: 'page kb-my-page' }, header, body));

  async function load() {
    const my = ++S.seq;
    fill(body, h('div', { class: 'stack-sm' }, ui.skeletonText(1), ui.skeleton({ height: '56px' }), ui.skeleton({ height: '56px' }), ui.skeleton({ height: '56px' })));
    try {
      const [boards, cards] = await Promise.all([
        kapi.boards(false, ctx.signal).catch(() => []),
        fetchAll(my),
      ]);
      if (my !== S.seq) return;
      S.boardColors = new Map(boards.map((b) => [b.id, b.color]));
      S.cards = cards;
      render();
    } catch (err) {
      if (my !== S.seq || err?.name === 'AbortError') return;
      fill(body, ui.errorState(err, () => load()));
    }
  }

  async function fetchAll(my) {
    const out = [];
    let cursor;
    S.truncated = false;
    do {
      const page = await kapi.cards({ assignee: 'me', completed: S.completed ? undefined : 'false', q: S.q || undefined, cursor, limit: 200 }, ctx.signal);
      if (my !== S.seq) return out;
      out.push(...page.items);
      cursor = page.next_cursor || undefined;
      if (out.length >= MAX_ITEMS && cursor) { S.truncated = true; break; }
    } while (cursor);
    return out;
  }

  function render() {
    if (!S.cards.length) {
      fill(body, ui.emptyState({
        icon: 'check-circle', title: S.q ? 'No matching cards' : 'Nothing assigned to you',
        message: S.q ? 'Try another search.' : 'Cards you are assigned to on any board show up here, sorted by due date.',
        action: ui.button({ label: 'Go to boards', icon: 'kanban', href: paths.boards() }),
      }));
      return;
    }
    const groups = groupByBucket(S.cards);
    fill(body, ...groups.map((g) => h('section', { class: cls('kb-my-group', `is-${g.id}`) },
      h('h2', { class: 'kb-my-title' }, g.title, h('span', { class: 'kb-my-count' }, String(g.cards.length))),
      h('ul', { class: 'kb-my-list' }, g.cards.map(row)))),
    S.truncated ? h('p', { class: 'faint small' }, `Showing the first ${MAX_ITEMS} cards.`) : null);
  }

  function row(c) {
    const cb = h('input', { type: 'checkbox', class: 'checkbox', checked: c.completed, aria: { label: c.completed ? `Mark “${c.title}” as not done` : `Mark “${c.title}” as done` } });
    cb.addEventListener('change', async () => {
      cb.disabled = true;
      try {
        const updated = await kapi.updateCard(c.id, { completed: cb.checked });
        Object.assign(c, updated);
        if (!S.completed && updated.completed) {
          S.cards = S.cards.filter((x) => x.id !== c.id);
          ui.toast.success(`Completed “${c.title}”`, { action: { label: 'Undo', onClick: async () => { await kapi.updateCard(c.id, { completed: false }).catch((e) => ui.toast.error(e)); load(); } } });
        }
        render();
      } catch (err) {
        cb.checked = !cb.checked;
        ui.toast.error(err?.status === 403 ? 'You can only view that board.' : err);
      } finally {
        cb.disabled = false;
      }
    });
    const p = progressOf(c);
    return h('li', { class: cls('kb-my-row', { 'is-completed': c.completed }) }, cb,
      h('a', { class: 'kb-my-link', href: paths.card(c.board_id, c.id) },
        h('span', { class: 'kb-my-card-title' }, c.title),
        h('span', { class: 'kb-my-meta' },
          h('span', { class: cls('kb-my-board', colorClass(S.boardColors.get(c.board_id))) }, h('span', { class: 'kb-dot' }), c.board_name),
          h('span', { class: 'faint' }, '·'), h('span', { class: 'faint' }, c.column_name))),
      h('span', { class: 'kb-my-badges' },
        c.priority !== 'none' ? h('span', { class: cls('kb-prio', `is-${c.priority}`), dataset: { tip: `${priorityLabel(c.priority)} priority` } }, kicon('flag', 13)) : null,
        p.total ? h('span', { class: 'kb-badge', dataset: { tip: 'Subtasks' } }, kicon('checklist', 13), `${p.done}/${p.total}`) : null,
        dueBadge(c) || h('span', { class: 'faint small' }, 'No date')));
  }

  offs.push(ctx.commands.register([
    { id: 'kanban:boards', title: 'All boards', icon: 'kanban', group: 'Kanban', run: () => ctx.router.navigate(paths.boards()) },
  ]));
  load();
  return {
    update() {},
    destroy() {
      for (const off of offs.splice(0)) off?.();
      ctx.setSearch(null);
    },
  };
}

