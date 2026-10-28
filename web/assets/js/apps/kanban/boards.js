// Boards grid: the caller's boards (active or archived) with color, counters and members;
// create from a template, rename, recolor, archive/restore, leave and delete.

import { h, cls } from '../../core/dom.js';
import { icon } from '../../core/icons.js';
import { plural, formatRelative } from '../../core/format.js';
import { kicon } from './icons.js';
import { paths, colorClass, COLORS, can } from './model.js';
import { avatarStack, colorSwatches, field, popover, closePopover } from './widgets.js';

/**
 * Mount the boards grid.
 * @returns {{update: (route: object) => void, destroy: () => void}}
 */
export function mountBoards(container, app, route) {
  const { ctx, kapi } = app;
  const { ui } = ctx;
  const offs = [];
  const S = { archived: route.query.archived === '1', boards: [], q: '', seq: 0, loaded: false };

  ctx.setLayout({});
  ctx.setTitle('Kanban');
  ctx.setActions(null);
  const searchCfg = () => ({ placeholder: 'Search boards', value: S.q, onInput: (v) => { S.q = v.trim().toLowerCase(); render(); }, onSubmit: (v) => { S.q = v.trim().toLowerCase(); render(); } });
  ctx.setSearch(searchCfg());

  const seg = ui.segmented({
    options: [{ value: 'active', label: 'Active' }, { value: 'archived', label: 'Archived' }],
    value: S.archived ? 'archived' : 'active', size: 'sm', ariaLabel: 'Which boards',
    onChange: (v) => ctx.router.setQuery({ archived: v === 'archived' ? '1' : null }, { replace: false }),
  });
  const header = ui.pageHeader({
    title: 'Boards',
    subtitle: 'Plan work with columns and cards, share boards with others, and keep track of due dates.',
    actions: [
      seg.el,
      ui.button({ label: 'My cards', icon: 'user', variant: 'secondary', href: paths.my() }),
      ui.button({ label: 'New board', icon: 'plus', variant: 'primary', onClick: () => createBoard() }),
    ],
  });
  const grid = h('div', { class: 'kb-grid' });
  container.replaceChildren(h('div', { class: 'page wide kb-boards' }, header, grid));

  async function load() {
    const my = ++S.seq;
    if (!S.loaded) grid.replaceChildren(...ui.skeletonCards(6, '132px'));
    try {
      const boards = await kapi.boards(S.archived, ctx.signal);
      if (my !== S.seq) return;
      S.boards = boards;
      S.loaded = true;
      render();
    } catch (err) {
      if (my !== S.seq || err?.name === 'AbortError') return;
      grid.replaceChildren(ui.errorState(err, () => load()));
    }
  }

  function render() {
    const list = S.boards.filter((b) => !S.q || b.name.toLowerCase().includes(S.q) || b.description.toLowerCase().includes(S.q));
    if (!S.boards.length) {
      grid.replaceChildren(S.archived
        ? ui.emptyState({ icon: 'archive', title: 'No archived boards', message: 'Boards you archive are kept here until you restore or delete them.' })
        : ui.emptyState({
          icon: 'kanban', title: 'No boards yet',
          message: 'A board holds columns of cards — for a project, a household, a release. Start from the basic template (To do, In progress, Done) or a blank board.',
          action: ui.button({ label: 'Create a board', icon: 'plus', variant: 'primary', onClick: () => createBoard() }),
        }));
      return;
    }
    if (!list.length) {
      grid.replaceChildren(ui.emptyState({ icon: 'search', title: 'No matching boards', compact: true }));
      return;
    }
    grid.replaceChildren(...list.map(tile));
  }

  function tile(b) {
    const members = new Map(b.members.map((m) => [m.user_id, m]));
    const menuBtn = ui.button({ icon: 'more', title: 'Board actions', size: 'sm', variant: 'ghost', className: 'kb-board-menu' });
    menuBtn.setAttribute('aria-haspopup', 'menu');
    menuBtn.addEventListener('click', (e) => {
      e.preventDefault();
      e.stopPropagation();
      boardMenu(b, menuBtn);
    });
    const link = h('a', { class: 'kb-board-link', href: paths.board(b.id), aria: { label: `Open ${b.name}` } });
    const stats = [
      h('span', { class: 'kb-stat', dataset: { tip: 'Open cards' } }, kicon('checklist', 13), plural(b.open_card_count, 'open card')),
      b.overdue_count ? h('span', { class: 'kb-stat is-overdue', dataset: { tip: 'Overdue cards' } }, icon('clock', 13), `${b.overdue_count} overdue`) : null,
    ];
    return h('article', { class: cls('kb-board-card', 'card', 'card-hover', colorClass(b.color)) },
      link,
      h('div', { class: 'kb-board-band' }, h('span', { class: 'kb-board-icon' }, kicon('columns', 18)), menuBtn),
      h('div', { class: 'kb-board-body' },
        h('h2', { class: 'kb-board-card-title truncate' }, b.name),
        b.description ? h('p', { class: 'kb-board-desc' }, b.description) : null,
        h('div', { class: 'kb-board-stats' }, stats)),
      h('div', { class: 'kb-board-foot' },
        avatarStack(b.members.map((m) => m.user_id), members, { max: 4, size: 24 }),
        b.role !== 'owner' ? h('span', { class: 'badge' }, b.role) : null,
        h('span', { class: 'faint small kb-board-updated', dataset: { tip: new Date(b.updated_at).toLocaleString() } }, formatRelative(b.updated_at))));
  }

  function boardMenu(b, anchor) {
    const items = [{ label: 'Open', icon: 'external', onSelect: () => ctx.router.navigate(paths.board(b.id)) }];
    if (can(b.role, 'admin')) {
      items.push(
        { label: 'Rename…', icon: 'edit', onSelect: () => rename(b) },
        { label: 'Change color…', icon: 'circle-dot', onSelect: () => recolor(b, anchor) },
        { label: b.archived ? 'Restore' : 'Archive', icon: b.archived ? 'restore' : 'archive', onSelect: () => patch(b, { archived: !b.archived }, b.archived ? 'Board restored' : 'Board archived') },
      );
    }
    if (b.role !== 'owner') items.push({ separator: true }, { label: 'Leave board…', icon: 'log-out', danger: true, onSelect: () => leave(b) });
    else items.push({ separator: true }, { label: 'Delete board…', icon: 'trash', danger: true, onSelect: () => remove(b) });
    ui.openMenu({ anchor, items, align: 'end' });
  }

  async function patch(b, body, message) {
    try {
      await kapi.updateBoard(b.id, body);
      if (message) ui.toast.success(message);
      load();
    } catch (err) {
      ui.toast.error(err);
    }
  }

  async function rename(b) {
    const name = await ui.prompt({ title: 'Rename board', label: 'Name', value: b.name, confirmLabel: 'Rename', validate: (v) => (!v.trim() ? 'Enter a name.' : [...v.trim()].length > 200 ? 'Use at most 200 characters.' : null) });
    if (name !== null && name.trim() !== b.name) patch(b, { name: name.trim() });
  }

  function recolor(b, anchor) {
    const sw = colorSwatches({ value: b.color, label: 'Board color', onChange: (c) => { closePopover(); patch(b, { color: c }); } });
    popover({ anchor, content: h('div', { class: 'kb-pop-body' }, h('div', { class: 'kb-pop-title' }, 'Board color'), sw.el), label: 'Board color', align: 'end' });
  }

  async function leave(b) {
    if (!(await ui.confirm({ title: `Leave “${b.name}”?`, message: 'You lose access and are unassigned from its cards.', confirmLabel: 'Leave board', danger: true }))) return;
    try {
      await kapi.leave(b.id);
      ui.toast.success(`You left “${b.name}”`);
      load();
    } catch (err) {
      ui.toast.error(err);
    }
  }

  async function remove(b) {
    const ok = await ui.confirm({
      title: `Delete “${b.name}”?`, message: `${plural(b.card_count, 'card')} with their comments and attachments will be deleted permanently.`,
      requireText: b.name, confirmLabel: 'Delete board', danger: true,
    });
    if (!ok) return;
    try {
      await kapi.deleteBoard(b.id);
      ui.toast.success('Board deleted');
      load();
    } catch (err) {
      ui.toast.error(err);
    }
  }

  async function createBoard() {
    const name = h('input', { class: 'input', placeholder: 'e.g. Home renovation', maxlength: 200, required: true, autocomplete: 'off' });
    const desc = h('textarea', { class: 'textarea', rows: 2, placeholder: 'Optional', maxlength: 100000 });
    let color = COLORS[1 + Math.floor(Math.random() * (COLORS.length - 1))];
    const sw = colorSwatches({ value: color, label: 'Board color', onChange: (c) => { color = c; } });
    let template = 'basic';
    const tplOption = (value, title, cols) => {
      const radio = h('input', { type: 'radio', name: 'kb-template', value, class: 'radio', checked: value === template });
      radio.addEventListener('change', () => { template = value; });
      return h('label', { class: 'kb-template' }, radio,
        h('span', { class: 'kb-template-text' }, h('span', { class: 'bold' }, title),
          h('span', { class: 'kb-template-cols' }, cols.length ? cols.map((c) => h('span', { class: 'kb-template-col' }, c)) : h('span', { class: 'faint small' }, 'Start with no columns'))));
    };
    const err = h('div', { class: 'field-error', hidden: true });
    const m = ui.modal({
      title: 'New board', size: 'md',
      body: h('div', { class: 'form-grid' }, field('Name', name), field('Description', desc),
        h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Color'), sw.el),
        h('div', { class: 'field', role: 'radiogroup', aria: { label: 'Template' } }, h('span', { class: 'field-label' }, 'Template'),
          h('div', { class: 'kb-templates' }, tplOption('basic', 'Basic', ['To do', 'In progress', 'Done']), tplOption('blank', 'Blank', []))), err),
      footer: [{ label: 'Cancel', value: null }, { label: 'Create board', variant: 'primary', type: 'submit' }],
      onSubmit: async () => {
        if (!name.value.trim()) {
          err.textContent = 'Enter a name.';
          err.hidden = false;
          name.focus();
          return false;
        }
        return kapi.createBoard({ name: name.value.trim(), description: desc.value, color, template });
      },
    });
    const b = await m.closed;
    if (b?.id) ctx.router.navigate(paths.board(b.id));
  }

  offs.push(
    ctx.shortcuts.register('n', () => createBoard(), { description: 'New board', when: () => !document.documentElement.classList.contains('modal-open') }),
    ctx.commands.register([
      { id: 'kanban:new-board', title: 'New board', icon: 'plus', group: 'Kanban', shortcut: 'N', keywords: ['create', 'kanban'], run: () => createBoard() },
      { id: 'kanban:my', title: 'My cards', icon: 'user', group: 'Kanban', keywords: ['assigned', 'due'], run: () => ctx.router.navigate(paths.my()) },
      { id: 'kanban:archived-boards', title: 'Archived boards', icon: 'archive', group: 'Kanban', run: () => ctx.router.navigate(`${paths.boards()}?archived=1`) },
    ]),
    ctx.commands.register(() => S.boards.slice(0, 200).map((b) => ({
      id: `kanban:board:${b.id}`, title: b.name, subtitle: 'Board', icon: 'kanban', group: 'Kanban boards', run: () => ctx.router.navigate(paths.board(b.id)),
    }))),
  );

  load();
  return {
    update(r) {
      const archived = r.query.archived === '1';
      if (archived !== S.archived) {
        S.archived = archived;
        S.loaded = false;
        seg.setValue(archived ? 'archived' : 'active');
        load();
      }
    },
    destroy() {
      for (const off of offs.splice(0)) off?.();
      ctx.setSearch(null);
    },
  };
}
