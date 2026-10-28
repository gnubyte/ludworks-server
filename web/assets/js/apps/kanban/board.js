// Board view: horizontally scrolling columns of cards with drag and drop (cards and whole
// columns), keyboard and menu moves, quick add, column management, a filter bar, and live
// updates (GET with If-None-Match every ~10 s while visible). Moves are optimistic and run one
// at a time so the local positions always mirror the server's (API.md §13 Ordering); a failed
// move rolls back. The card detail (card.js) opens over the board at /kanban/<board>/c/<card>.

import { h, cls, on, mount as fill } from '../../core/dom.js';
import { icon } from '../../core/icons.js';
import { plural } from '../../core/format.js';
import { kicon } from './icons.js';
import { NOT_MODIFIED } from './api.js';
import {
  paths, colorClass, columnCards, targetPosition, applyMove, reorder, keyboardMove, emptyFilter, filterActive, filterFromQuery,
  filterToQuery, matchesFilter, memberName, progressOf, wipState, can, mergeCards, signature, priorityLabel,
} from './model.js';
import { createDnd } from './dnd.js';
import { labelChip, avatarStack, dueBadge, countBadge, colorSwatches, popover, closePopover, iconButton } from './widgets.js';
import { openCardDetail } from './card.js';
import { openBoardSettings, openActivity } from './settings.js';

const POLL_MS = 10_000;

/**
 * Mount the board view.
 * @param {HTMLElement} container
 * @param {object} app {ctx, kapi, me}
 * @param {{boardId: string, cardId: string|null, query: object}} route
 */
export function mountBoard(container, app, route) {
  const { ctx, kapi } = app;
  const { ui } = ctx;
  const offs = [];
  const S = {
    id: route.boardId,
    board: null,
    etag: null,
    filter: filterFromQuery(route.query),
    showArchived: route.query.archived === '1',
    pending: new Set(),
    inflight: 0,
    queue: Promise.resolve(),
    destroyed: false,
    pollTimer: 0,
    localCollapsed: new Set(),
    composer: null, // column id with an open quick-add composer
  };
  const members = new Map();
  const labels = new Map();
  const colEls = new Map();
  const tileEls = new Map();
  let detail = null; // open card detail controller
  let lastFocus = null;

  ctx.setLayout({ full: true });
  ctx.setTitle('Board');

  /* --------------------------------------------------------------- skeleton */
  const titleBtn = h('button', { type: 'button', class: 'kb-board-title' });
  const titleWrap = h('div', { class: 'kb-board-name' }, h('span', { class: 'kb-board-dot' }), titleBtn);
  const filterBar = h('div', { class: 'kb-filters', role: 'toolbar', aria: { label: 'Filter cards' } });
  const membersEl = h('button', { type: 'button', class: 'kb-board-members', aria: { label: 'Members' } });
  const head = h('div', { class: 'kb-board-head' }, titleWrap, filterBar, h('div', { class: 'kb-board-tools' }, membersEl));
  const banner = h('div', { class: 'kb-banner', hidden: true });
  const columnsEl = h('div', { class: 'kb-columns' });
  const scroller = h('div', { class: 'kb-scroller' }, columnsEl);
  const addColumnEl = h('div', { class: 'kb-add-column' });
  const root = h('div', { class: 'kb-board-view' }, head, banner, scroller);
  container.replaceChildren(h('div', { class: 'kb-board-loading' }, ui.loadingBlock('Loading board…')));

  /* ---------------------------------------------------------------- helpers */
  const role = () => S.board?.role || 'viewer';
  const editor = () => can(role(), 'editor');
  const admin = () => can(role(), 'admin');
  const sortedColumns = () => [...(S.board?.columns || [])].sort((a, b) => a.position - b.position);
  const cardById = (id) => S.board?.cards.find((c) => c.id === id) || null;
  const visible = (c) => (S.showArchived || !c.archived) && matchesFilter(c, S.filter, { me: app.me });
  const isCollapsed = (col) => col.collapsed || S.localCollapsed.has(col.id);

  function indexBoard() {
    members.clear();
    labels.clear();
    for (const m of S.board.members) members.set(m.user_id, m);
    for (const l of S.board.labels) labels.set(l.id, l);
  }

  function setBoard(b, etag) {
    S.board = b;
    if (etag) S.etag = etag;
    indexBoard();
    ctx.setTitle(b.name);
  }

  /* ------------------------------------------------------------------ load */
  async function load() {
    try {
      const res = await kapi.board(S.id, { includeArchived: true, signal: ctx.signal });
      if (S.destroyed) return;
      setBoard(res.board, res.etag);
      container.replaceChildren(root);
      wireOnce();
      render();
      schedulePoll();
      if (route.cardId) openCard(route.cardId, { pushed: false });
    } catch (err) {
      if (S.destroyed || err?.name === 'AbortError') return;
      container.replaceChildren(h('div', { class: 'page' }, err?.status === 404
        ? ui.emptyState({ icon: 'kanban', title: 'Board not found', message: 'It was deleted, or you are not a member of it.', action: ui.button({ label: 'All boards', icon: 'arrow-left', href: paths.boards() }) })
        : ui.errorState(err, () => load())));
    }
  }

  /* --------------------------------------------------------------- polling */
  function schedulePoll(ms = POLL_MS) {
    clearTimeout(S.pollTimer);
    if (S.destroyed) return;
    S.pollTimer = setTimeout(poll, ms);
  }

  async function poll() {
    if (S.destroyed) return;
    if (document.visibilityState === 'hidden') return; // resumes on visibilitychange
    if (S.inflight > 0 || dnd?.active || colDnd?.active) {
      schedulePoll(1500);
      return;
    }
    try {
      const res = await kapi.board(S.id, { etag: S.etag, includeArchived: true, signal: ctx.signal });
      if (S.destroyed) return;
      if (res !== NOT_MODIFIED && S.inflight === 0 && !dnd?.active && !colDnd?.active) applyRemote(res.board, res.etag);
    } catch (err) {
      if (S.destroyed || err?.name === 'AbortError') return;
      if (err?.status === 404) {
        ui.toast.warning('This board is no longer available to you.');
        detail?.close({ silent: true });
        ctx.router.navigate(paths.boards(), { replace: true });
        return;
      }
      // Network hiccups: try again on the next tick.
    }
    schedulePoll();
  }

  function applyRemote(b, etag) {
    const cards = mergeCards(S.board.cards, b.cards, S.pending);
    setBoard({ ...b, cards }, etag);
    render();
    detail?.boardChanged();
  }

  const refreshSoon = () => schedulePoll(400);

  offs.push(on(document, 'visibilitychange', () => {
    if (document.visibilityState === 'visible') schedulePoll(50);
  }));

  /* ---------------------------------------------------------------- render */
  function render() {
    if (!S.board) return;
    const focused = document.activeElement?.closest?.('.kb-tile')?.dataset.id || null;
    S.ctxSig = sigCtx();
    renderHead();
    renderColumns();
    if (focused && (!document.activeElement || document.activeElement === document.body)) tileEls.get(focused)?.focus({ preventScroll: true });
  }

  function renderHead() {
    const b = S.board;
    titleWrap.className = cls('kb-board-name', colorClass(b.color));
    titleBtn.textContent = b.name;
    titleBtn.disabled = !admin();
    titleBtn.dataset.tip = admin() ? 'Rename board' : '';
    membersEl.replaceChildren(avatarStack(b.members.map((m) => m.user_id), members, { max: 4, size: 26 }));
    membersEl.dataset.tip = `${plural(b.members.length, 'member')} · your role: ${b.role}`;
    banner.hidden = !b.archived;
    if (b.archived) {
      fill(banner, icon('archive', 16), h('span', {}, 'This board is archived.'),
        admin() ? ui.button({ label: 'Restore board', size: 'sm', onClick: () => patchBoard({ archived: false }) }) : null);
    }
    renderFilters();
  }

  function renderFilters() {
    const f = S.filter;
    const n = S.board.cards.filter((c) => (S.showArchived || !c.archived)).length;
    const shown = S.board.cards.filter(visible).length;
    const labelBtn = filterButton('tag', f.labels.length ? `Labels · ${f.labels.length}` : 'Labels', f.labels.length > 0, (b) => labelFilter(b));
    const assigneeText = f.assignee === 'me' ? 'Assigned to me' : f.assignee === 'none' ? 'Unassigned' : f.assignee ? memberName(members.get(f.assignee)) : 'Assignee';
    const assigneeBtn = filterButton('user', assigneeText, !!f.assignee, (b) => ui.openMenu({ anchor: b, items: [
      { label: 'Anyone', checked: !f.assignee, onSelect: () => setFilter({ assignee: '' }) },
      { label: 'Assigned to me', checked: f.assignee === 'me', onSelect: () => setFilter({ assignee: 'me' }) },
      { label: 'Unassigned', checked: f.assignee === 'none', onSelect: () => setFilter({ assignee: 'none' }) },
      { separator: true },
      ...S.board.members.map((m) => ({ label: memberName(m), checked: f.assignee === m.user_id, onSelect: () => setFilter({ assignee: m.user_id }) })),
    ] }));
    const dueText = { overdue: 'Overdue', today: 'Due today', week: 'Due this week', none: 'No due date' }[f.due] || 'Due';
    const dueBtn = filterButton('calendar', dueText, !!f.due, (b) => ui.openMenu({ anchor: b, items: [
      { label: 'Any time', checked: !f.due, onSelect: () => setFilter({ due: '' }) },
      { label: 'Overdue', checked: f.due === 'overdue', onSelect: () => setFilter({ due: 'overdue' }) },
      { label: 'Due today', checked: f.due === 'today', onSelect: () => setFilter({ due: 'today' }) },
      { label: 'Due this week', checked: f.due === 'week', onSelect: () => setFilter({ due: 'week' }) },
      { label: 'No due date', checked: f.due === 'none', onSelect: () => setFilter({ due: 'none' }) },
    ] }));
    const archived = h('label', { class: 'kb-switch-row', dataset: { tip: 'Show archived cards (Shift+A)' } },
      h('span', { class: 'switch' }, h('input', { type: 'checkbox', class: 'switch-input', checked: S.showArchived, onChange: (e) => setArchived(e.target.checked), aria: { label: 'Show archived cards' } }),
        h('span', { class: 'switch-track' })), h('span', { class: 'kb-switch-text' }, 'Archived'));
    const active = filterActive(f);
    fill(filterBar, labelBtn, assigneeBtn, dueBtn, archived,
      active ? h('span', { class: 'kb-filter-count', role: 'status' }, `${shown} of ${n}`) : null,
      active ? ui.button({ label: 'Clear', icon: 'x', size: 'sm', variant: 'ghost', onClick: () => clearFilters() }) : null);
  }

  function filterButton(ic, text, activeState, open) {
    const b = h('button', { type: 'button', class: cls('kb-filter-btn', { 'is-active': activeState }), aria: { haspopup: 'true', expanded: 'false' } },
      icon(ic, 14), h('span', { class: 'kb-filter-text' }, text), icon('chevron-down', { size: 12, className: 'kb-filter-caret' }));
    b.addEventListener('click', () => open(b));
    return b;
  }

  function labelFilter(anchor) {
    const list = h('div', { class: 'kb-check-list', role: 'group', aria: { label: 'Labels' } });
    if (!S.board.labels.length) list.append(h('div', { class: 'faint small kb-hint' }, 'This board has no labels yet.'));
    for (const l of S.board.labels) {
      const cb = h('input', { type: 'checkbox', class: 'checkbox', checked: S.filter.labels.includes(l.id) });
      cb.addEventListener('change', () => {
        const set = new Set(S.filter.labels);
        if (cb.checked) set.add(l.id);
        else set.delete(l.id);
        setFilter({ labels: [...set] }, { keepPopover: true });
      });
      list.append(h('label', { class: 'kb-check-row' }, cb, labelChip(l)));
    }
    popover({ anchor, content: h('div', { class: 'kb-pop-body' }, h('div', { class: 'kb-pop-title' }, 'Show cards with any of'), list), label: 'Filter by label' });
  }

  function setFilter(patch, { keepPopover = false } = {}) {
    S.filter = { ...S.filter, ...patch };
    if (!keepPopover) closePopover();
    ctx.router.setQuery(filterToQuery(S.filter));
    render();
  }

  function clearFilters() {
    S.filter = emptyFilter();
    ctx.setSearch(searchCfg());
    ctx.router.setQuery(filterToQuery(S.filter));
    render();
  }

  function setArchived(onOff) {
    S.showArchived = onOff;
    ctx.router.setQuery({ archived: onOff ? '1' : null });
    render();
  }

  /* --------------------------------------------------------------- columns */
  function renderColumns() {
    const cols = sortedColumns();
    const wanted = new Set();
    let ref = columnsEl.firstChild;
    for (const col of cols) {
      wanted.add(col.id);
      let el = colEls.get(col.id);
      if (!el) {
        el = buildColumn(col);
        colEls.set(col.id, el);
      }
      updateColumn(el, col);
      if (ref !== el) columnsEl.insertBefore(el, ref);
      ref = el.nextSibling;
    }
    for (const [id, el] of colEls) {
      if (!wanted.has(id)) {
        el.remove();
        colEls.delete(id);
      }
    }
    // Tiles of cards that are no longer shown anywhere.
    const shownIds = new Set(S.board.cards.filter(visible).map((c) => c.id));
    for (const [id, el] of tileEls) {
      if (!shownIds.has(id) || !colEls.has(cardById(id)?.column_id)) {
        el.remove();
        tileEls.delete(id);
      }
    }
    if (editor()) {
      renderAddColumn();
      if (addColumnEl.parentElement !== columnsEl || columnsEl.lastChild !== addColumnEl) columnsEl.append(addColumnEl);
    } else addColumnEl.remove();
    if (!cols.length) {
      columnsEl.classList.add('is-empty');
    } else columnsEl.classList.remove('is-empty');
  }

  function buildColumn(col) {
    const name = h('h2', { class: 'kb-col-name' });
    const count = h('span', { class: 'kb-col-count' });
    const addBtn = iconButton('plus', 'Add a card', () => openComposer(col.id), { className: 'kb-col-add' });
    const menuBtn = iconButton('more', 'Column actions', () => columnMenu(col.id, menuBtn));
    menuBtn.setAttribute('aria-haspopup', 'menu');
    const collapsedBtn = h('button', { type: 'button', class: 'kb-col-expand', aria: { label: 'Expand column' } });
    collapsedBtn.addEventListener('click', () => toggleCollapse(col.id, false));
    const headEl = h('div', { class: 'kb-col-head' }, h('span', { class: 'kb-col-stripe' }), name, count, h('span', { class: 'kb-col-actions' }, addBtn, menuBtn));
    const list = h('div', { class: 'kb-cards', role: 'list' });
    const body = h('div', { class: 'kb-col-body' }, list);
    const foot = h('div', { class: 'kb-col-foot' });
    const el = h('section', { class: 'kb-column', dataset: { id: col.id } }, headEl, collapsedBtn, body, foot);
    el._kb = { name, count, list, body, foot, headEl, addBtn, menuBtn, collapsedBtn };
    list.addEventListener('keydown', onTileKey);
    list.addEventListener('click', onTileClick);
    list.addEventListener('contextmenu', onTileContext);
    headEl.addEventListener('dblclick', (e) => { if (!e.target.closest('button') && editor()) renameColumn(col.id); });
    return el;
  }

  function updateColumn(el, col) {
    const k = el._kb;
    const cards = columnCards(S.board.cards, col.id, { archived: true });
    const live = cards.filter((c) => !c.archived).length;
    const shown = cards.filter(visible);
    const wip = wipState(col, live);
    el.className = cls('kb-column', col.color ? colorClass(col.color) : 'kb-c-none', `is-wip-${wip}`, { 'is-collapsed': isCollapsed(col) });
    el.setAttribute('aria-label', `${col.name}, ${plural(live, 'card')}`);
    k.name.textContent = col.name;
    k.count.textContent = col.wip_limit ? `${live} / ${col.wip_limit}` : String(live);
    k.count.dataset.tip = wip === 'over' ? `Over the WIP limit of ${col.wip_limit}` : wip === 'full' ? 'At the WIP limit' : col.wip_limit ? `WIP limit ${col.wip_limit}` : plural(live, 'card');
    k.list.setAttribute('aria-label', col.name);
    k.addBtn.hidden = !editor();
    k.collapsedBtn.replaceChildren(kicon('expand', 14), h('span', { class: 'kb-col-vname' }, col.name), h('span', { class: 'kb-col-vcount' }, String(live)));
    k.collapsedBtn.dataset.tip = `Expand “${col.name}”`;
    renderCards(el, shown);
    renderComposer(el, col);
  }

  /* ------------------------------------------------------------------ cards */
  // Tiles show label and member names: a change of those rebuilds every tile.
  const sigCtx = () => signature([S.board.labels, S.board.members.map((m) => [m.user_id, m.display_name, m.username]), editor()]);

  function renderCards(colEl, cards) {
    const list = colEl._kb.list;
    const ctxSig = S.ctxSig;
    let cursor = list.firstElementChild;
    for (const card of cards) {
      const sig = signature(card) + ctxSig;
      let tile = tileEls.get(card.id);
      if (!tile || tile._sig !== sig) {
        const nt = buildTile(card);
        nt._sig = sig;
        if (tile) {
          if (cursor === tile) cursor = nt;
          tile.replaceWith(nt);
        }
        tileEls.set(card.id, nt);
        tile = nt;
      }
      while (cursor && !cursor.classList.contains('kb-tile')) cursor = cursor.nextElementSibling;
      if (cursor === tile) cursor = cursor.nextElementSibling;
      else list.insertBefore(tile, cursor);
    }
    const ids = new Set(cards.map((c) => c.id));
    for (const el of [...list.children]) {
      if (el.classList.contains('kb-tile') && !ids.has(el.dataset.id)) el.remove();
    }
    let empty = list.querySelector(':scope > .kb-col-empty');
    if (!cards.length) {
      if (!empty) {
        empty = h('div', { class: 'kb-col-empty' });
        list.append(empty);
      }
      empty.textContent = filterActive(S.filter) ? 'No matching cards' : editor() ? 'Drop cards here' : 'No cards';
    } else empty?.remove();
  }

  function buildTile(card) {
    const coverId = card.cover_attachment_id;
    const progress = progressOf(card);
    const labelEls = card.labels.map((id) => labels.get(id)).filter(Boolean).map((l) => labelChip(l));
    const badges = [
      dueBadge(card),
      card.priority && card.priority !== 'none' ? h('span', { class: cls('kb-prio', `is-${card.priority}`), dataset: { tip: `${priorityLabel(card.priority)} priority` } }, kicon('flag', 13), h('span', { class: 'kb-prio-text' }, priorityLabel(card.priority))) : null,
      card.description ? h('span', { class: 'kb-badge', dataset: { tip: 'Has a description' } }, kicon('align-left', 13)) : null,
      progress.total ? h('span', { class: cls('kb-badge', { 'is-done': progress.done === progress.total }), dataset: { tip: `${progress.done} of ${progress.total} subtasks done` } }, kicon('checklist', 13), h('span', {}, `${progress.done}/${progress.total}`)) : null,
      card.comment_count ? countBadge('comment', card.comment_count, plural(card.comment_count, 'comment')) : null,
      card.attachment_count ? countBadge('paperclip', card.attachment_count, plural(card.attachment_count, 'attachment')) : null,
    ].filter(Boolean);
    const cover = coverId ? h('div', { class: 'kb-tile-cover' }, h('img', {
      src: kapi.thumbUrl(coverId, 'md'), alt: '', loading: 'lazy', draggable: 'false',
      onError: (e) => { const img = e.target; if (!img.dataset.fallback) { img.dataset.fallback = '1'; img.src = kapi.attachmentUrl(coverId); } else img.parentElement?.remove(); },
    })) : null;
    const tile = h('article', {
      class: cls('kb-tile', { 'is-completed': card.completed, 'is-archived': card.archived, 'has-cover': !!coverId }),
      role: 'listitem', tabindex: '0', dataset: { id: card.id },
      aria: { label: tileLabel(card), roledescription: editor() ? 'Draggable card' : 'Card', keyshortcuts: editor() ? 'Enter Alt+ArrowUp Alt+ArrowDown Alt+ArrowLeft Alt+ArrowRight' : 'Enter' },
    },
    cover,
    labelEls.length ? h('div', { class: 'kb-tile-labels' }, labelEls) : null,
    h('div', { class: 'kb-tile-title' }, card.completed ? icon('check-circle', { size: 14, className: 'kb-tile-done' }) : null, h('span', {}, card.title)),
    badges.length || card.assignees.length || card.archived ? h('div', { class: 'kb-tile-foot' },
      h('div', { class: 'kb-tile-badges' }, card.archived ? h('span', { class: 'kb-badge is-archived' }, icon('archive', 12), h('span', {}, 'Archived')) : null, badges),
      card.assignees.length ? avatarStack(card.assignees, members, { max: 3, size: 22 }) : null) : null,
    h('button', { type: 'button', class: 'kb-tile-menu kb-no-drag', tabindex: '-1', aria: { label: 'Card actions' }, dataset: { action: 'menu' } }, icon('more', 14)));
    return tile;
  }

  function tileLabel(card) {
    const parts = [card.title];
    if (card.completed) parts.push('completed');
    if (card.archived) parts.push('archived');
    if (card.due_at) parts.push(`due ${new Date(card.due_at).toLocaleString()}`);
    const p = progressOf(card);
    if (p.total) parts.push(`${p.done} of ${p.total} subtasks`);
    if (card.assignees.length) parts.push(`assigned to ${card.assignees.map((id) => memberName(members.get(id))).join(', ')}`);
    return parts.join(', ');
  }

  function onTileClick(e) {
    const tile = e.target.closest('.kb-tile');
    if (!tile) return;
    if (e.target.closest('[data-action="menu"]')) {
      e.stopPropagation();
      tileMenu(tile.dataset.id, { anchor: e.target.closest('button') });
      return;
    }
    if (e.target.closest('a,button,input')) return;
    openCard(tile.dataset.id);
  }

  function onTileContext(e) {
    const tile = e.target.closest('.kb-tile');
    if (!tile || e.target.closest('input,textarea')) return;
    e.preventDefault();
    tileMenu(tile.dataset.id, { x: e.clientX, y: e.clientY });
  }

  function onTileKey(e) {
    const tile = e.target.closest('.kb-tile');
    if (!tile || e.target !== tile) return;
    const id = tile.dataset.id;
    const arrows = { ArrowUp: 'up', ArrowDown: 'down', ArrowLeft: 'left', ArrowRight: 'right' };
    if (e.key === 'Enter' || e.key === ' ') {
      e.preventDefault();
      openCard(id);
    } else if (e.key === 'ContextMenu' || (e.shiftKey && e.key === 'F10')) {
      e.preventDefault();
      tileMenu(id, { anchor: tile });
    } else if (arrows[e.key] && e.altKey && editor()) {
      e.preventDefault();
      keyMove(id, arrows[e.key]);
    } else if (arrows[e.key] && !e.altKey && !e.ctrlKey && !e.metaKey) {
      e.preventDefault();
      focusNeighbour(id, arrows[e.key]);
    } else if ((e.key === 'Home' || e.key === 'End') && e.altKey && editor()) {
      e.preventDefault();
      keyMove(id, e.key === 'Home' ? 'top' : 'bottom');
    }
  }

  function focusNeighbour(id, dir) {
    const card = cardById(id);
    if (!card) return;
    const cols = sortedColumns().filter((c) => !isCollapsed(c));
    const list = (colId) => columnCards(S.board.cards, colId, { archived: true }).filter(visible);
    const here = list(card.column_id);
    const i = here.findIndex((c) => c.id === id);
    let target = null;
    if (dir === 'up') target = here[i - 1];
    else if (dir === 'down') target = here[i + 1];
    else {
      const ci = cols.findIndex((c) => c.id === card.column_id);
      for (let j = ci + (dir === 'left' ? -1 : 1); j >= 0 && j < cols.length; j += dir === 'left' ? -1 : 1) {
        const there = list(cols[j].id);
        if (there.length) { target = there[Math.min(Math.max(i, 0), there.length - 1)]; break; }
      }
    }
    if (target) {
      const el = tileEls.get(target.id);
      el?.focus();
      el?.scrollIntoView({ block: 'nearest', inline: 'nearest' });
    }
  }

  function keyMove(id, dir) {
    const mv = keyboardMove(S.board.cards, sortedColumns(), id, dir, visible);
    if (!mv) return;
    lastFocus = id;
    moveCard(id, mv.toColumnId, mv.beforeId, { announce: true });
  }

  function tileMenu(id, { anchor = null, x = 0, y = 0 } = {}) {
    const card = cardById(id);
    if (!card) return;
    const cols = sortedColumns();
    const ci = cols.findIndex((c) => c.id === card.column_id);
    const items = [{ label: 'Open', icon: 'external', shortcut: 'Enter', onSelect: () => openCard(id) }];
    if (editor()) {
      items.push(
        { label: 'Move to…', icon: 'arrow-right', onSelect: () => moveDialog(id) },
        { label: 'Move up', icon: 'arrow-up', shortcut: 'Alt+↑', disabled: !keyboardMove(S.board.cards, cols, id, 'up', visible), onSelect: () => keyMove(id, 'up') },
        { label: 'Move down', icon: 'arrow-down', shortcut: 'Alt+↓', disabled: !keyboardMove(S.board.cards, cols, id, 'down', visible), onSelect: () => keyMove(id, 'down') },
        { label: 'Move left', icon: 'arrow-left', shortcut: 'Alt+←', disabled: ci <= 0, onSelect: () => keyMove(id, 'left') },
        { label: 'Move right', icon: 'arrow-right', shortcut: 'Alt+→', disabled: ci >= cols.length - 1, onSelect: () => keyMove(id, 'right') },
        { separator: true },
        { label: card.completed ? 'Mark as not done' : 'Mark as done', icon: 'check-circle', onSelect: () => patchCard(id, { completed: !card.completed }) },
        { label: card.archived ? 'Restore' : 'Archive', icon: card.archived ? 'restore' : 'archive', onSelect: () => patchCard(id, { archived: !card.archived }) },
      );
    }
    items.push({ label: 'Copy link', icon: 'link', onSelect: () => ui.copyText(new URL(paths.card(S.id, id), location.origin).href, { message: 'Link copied' }) });
    if (editor()) items.push({ separator: true }, { label: 'Delete…', icon: 'trash', danger: true, onSelect: () => deleteCard(id) });
    ui.openMenu({ items, anchor, x, y, align: 'end' });
  }

  async function moveDialog(id) {
    const card = cardById(id);
    if (!card) return;
    const colSel = h('select', { class: 'select', aria: { label: 'Column' } }, sortedColumns().map((c) => h('option', { value: c.id, selected: c.id === card.column_id }, c.name)));
    const posSel = h('select', { class: 'select', aria: { label: 'Position' } });
    const fillPos = () => {
      const others = columnCards(S.board.cards, colSel.value, { archived: true }).filter((c) => c.id !== id && visible(c));
      posSel.replaceChildren(...others.map((c, i) => h('option', { value: c.id }, `${i + 1}. Before “${c.title}”`)), h('option', { value: '', selected: true }, `${others.length + 1}. At the end`));
      if (colSel.value === card.column_id) {
        const here = columnCards(S.board.cards, card.column_id, { archived: true }).filter((c) => visible(c) || c.id === id);
        const next = here[here.findIndex((c) => c.id === id) + 1];
        posSel.value = next ? next.id : '';
      }
    };
    colSel.addEventListener('change', fillPos);
    fillPos();
    const m = ui.modal({
      title: 'Move card', description: card.title, size: 'sm',
      body: h('div', { class: 'form-grid' }, h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Column'), colSel), h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Position'), posSel)),
      footer: [{ label: 'Cancel', value: null }, { label: 'Move', variant: 'primary', type: 'submit' }],
      onSubmit: () => ({ col: colSel.value, before: posSel.value || null }),
    });
    const res = await m.closed;
    if (res) moveCard(id, res.col, res.before);
  }

  /* ----------------------------------------------------------------- moves */
  /**
   * Move a card (optimistically, serialized with other moves).
   * @param {string} id
   * @param {string} toColumnId
   * @param {string|null} beforeId drop before this card (null = end)
   */
  function moveCard(id, toColumnId, beforeId, { announce = false } = {}) {
    const run = async () => {
      const card = cardById(id);
      if (!card || S.destroyed) return;
      const position = targetPosition(S.board.cards, { id, toColumnId, beforeId });
      const snapshot = S.board.cards;
      S.board.cards = applyMove(S.board.cards, { id, toColumnId, position });
      if (S.board.cards === snapshot) return;
      S.pending.add(id);
      S.inflight += 1;
      render();
      if (lastFocus === id) tileEls.get(id)?.focus({ preventScroll: false });
      if (announce) announceMove(id);
      try {
        const updated = await kapi.moveCard(id, toColumnId, position);
        replaceCard(updated);
      } catch (err) {
        S.board.cards = snapshot;
        ui.toast.error(err, { description: 'The card was put back.' });
      } finally {
        S.pending.delete(id);
        S.inflight -= 1;
        render();
        if (lastFocus === id) { tileEls.get(id)?.focus({ preventScroll: false }); lastFocus = null; }
        if (S.inflight === 0) refreshSoon();
      }
    };
    S.queue = S.queue.then(run, run);
    return S.queue;
  }

  const live = h('div', { class: 'sr-only', aria: { live: 'polite' } });
  root.append(live);
  function announceMove(id) {
    const card = cardById(id);
    const col = S.board.columns.find((c) => c.id === card?.column_id);
    if (!card || !col) return;
    const pos = columnCards(S.board.cards, col.id).filter(visible).findIndex((c) => c.id === id) + 1;
    live.textContent = `Moved to ${col.name}, position ${pos}.`;
  }

  function moveColumn(id, position) {
    const run = async () => {
      const snapshot = S.board.columns;
      S.board.columns = reorder(S.board.columns, id, position);
      S.inflight += 1;
      render();
      try {
        S.board.columns = await kapi.moveColumn(id, position);
      } catch (err) {
        S.board.columns = snapshot;
        ui.toast.error(err);
      } finally {
        S.inflight -= 1;
        render();
        if (S.inflight === 0) refreshSoon();
      }
    };
    S.queue = S.queue.then(run, run);
    return S.queue;
  }

  /* -------------------------------------------------------------- card ops */
  function replaceCard(card) {
    const i = S.board.cards.findIndex((c) => c.id === card.id);
    if (i < 0) {
      S.board.cards = [...S.board.cards, card];
    } else {
      const cur = S.board.cards[i];
      // Lists carry a 200-character description; never replace it with a longer full text.
      const merged = { ...cur, ...card, description: card.description === undefined ? cur.description : card.description.slice(0, 200) };
      S.board.cards = S.board.cards.map((c, j) => (j === i ? merged : c));
    }
  }

  async function patchCard(id, patch) {
    S.inflight += 1;
    try {
      const card = await kapi.updateCard(id, patch);
      replaceCard(card);
      render();
      detail?.cardPatched(card);
    } catch (err) {
      ui.toast.error(err);
    } finally {
      S.inflight -= 1;
      refreshSoon();
    }
  }

  async function deleteCard(id) {
    const card = cardById(id);
    if (!card) return false;
    const ok = await ui.confirm({
      title: `Delete “${card.title}”?`,
      message: 'The card is deleted permanently with its subtasks, comments and attachments. Archive it instead to keep it.',
      confirmLabel: 'Delete card', danger: true,
    });
    if (!ok) return false;
    try {
      await kapi.deleteCard(id);
      removeCardLocal(id);
      ui.toast.success('Card deleted');
      return true;
    } catch (err) {
      ui.toast.error(err);
      return false;
    }
  }

  function removeCardLocal(id) {
    const card = cardById(id);
    if (!card) return;
    S.board.cards = S.board.cards.filter((c) => c.id !== id)
      .map((c) => (c.column_id === card.column_id && c.position > card.position ? { ...c, position: c.position - 1 } : c));
    render();
    refreshSoon();
  }

  /* ------------------------------------------------------------- composer */
  function renderComposer(colEl, col) {
    const foot = colEl._kb.foot;
    if (!editor() || isCollapsed(col)) { foot.replaceChildren(); return; }
    if (S.composer === col.id) {
      if (foot.querySelector('.kb-composer')) return;
      const ta = h('textarea', { class: 'textarea kb-composer-input', rows: 2, placeholder: 'Card title…', maxlength: 500, aria: { label: `New card in ${col.name}` } });
      const add = async () => {
        const title = ta.value.replace(/\s+/g, ' ').trim();
        if (!title) { ta.focus(); return; }
        ta.disabled = true;
        try {
          const card = await kapi.createCard(col.id, { title });
          S.board.cards = [...S.board.cards, card];
          ta.value = '';
          render();
          colEl._kb.body.scrollTop = colEl._kb.body.scrollHeight;
          refreshSoon();
        } catch (err) {
          ui.toast.error(err);
        } finally {
          ta.disabled = false;
          ta.focus();
        }
      };
      ta.addEventListener('keydown', (e) => {
        if (e.key === 'Enter' && !e.shiftKey && !e.isComposing) { e.preventDefault(); add(); }
        else if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); closeComposer(); }
      });
      const box = h('div', { class: 'kb-composer' }, ta, h('div', { class: 'kb-composer-actions' },
        ui.button({ label: 'Add card', variant: 'primary', size: 'sm', onClick: add }),
        iconButton('x', 'Cancel', () => closeComposer())));
      foot.replaceChildren(box);
      requestAnimationFrame(() => ta.focus());
    } else {
      if (foot.querySelector('.kb-add-card')) return;
      foot.replaceChildren(h('button', { type: 'button', class: 'kb-add-card', onClick: () => openComposer(col.id) }, icon('plus', 14), 'Add a card'));
    }
  }

  function openComposer(colId) {
    const col = S.board.columns.find((c) => c.id === colId);
    if (!col || !editor()) return;
    if (isCollapsed(col)) toggleCollapse(colId, false);
    const prev = S.composer;
    S.composer = colId;
    if (prev && prev !== colId) colEls.get(prev)?._kb.foot.replaceChildren();
    const el = colEls.get(colId);
    if (el) {
      el._kb.foot.replaceChildren();
      renderComposer(el, col);
      el.scrollIntoView({ block: 'nearest', inline: 'nearest' });
    }
  }

  function closeComposer() {
    const colId = S.composer;
    S.composer = null;
    const el = colEls.get(colId);
    const col = S.board.columns.find((c) => c.id === colId);
    if (el && col) {
      el._kb.foot.replaceChildren();
      renderComposer(el, col);
      el._kb.foot.querySelector('.kb-add-card')?.focus();
    }
  }

  /* --------------------------------------------------------- column admin */
  function renderAddColumn() {
    if (addColumnEl.querySelector('.kb-add-column-form')) return;
    const btn = h('button', { type: 'button', class: 'kb-add-column-btn' }, icon('plus', 16), 'Add column');
    btn.addEventListener('click', () => {
      const input = h('input', { class: 'input', placeholder: 'Column name', maxlength: 200, aria: { label: 'Column name' } });
      const submit = async () => {
        const name = input.value.trim();
        if (!name) { input.focus(); return; }
        input.disabled = true;
        try {
          const col = await kapi.createColumn(S.id, { name });
          S.board.columns = [...S.board.columns, col];
          input.value = '';
          render();
          scroller.scrollLeft = scroller.scrollWidth;
          refreshSoon();
        } catch (err) {
          ui.toast.error(err);
        } finally {
          input.disabled = false;
          input.focus();
        }
      };
      input.addEventListener('keydown', (e) => {
        if (e.key === 'Enter') { e.preventDefault(); submit(); }
        if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); addColumnEl.replaceChildren(); renderAddColumn(); }
      });
      addColumnEl.replaceChildren(h('div', { class: 'kb-add-column-form' }, input, h('div', { class: 'row' },
        ui.button({ label: 'Add column', variant: 'primary', size: 'sm', onClick: submit }),
        iconButton('x', 'Cancel', () => { addColumnEl.replaceChildren(); renderAddColumn(); }))));
      input.focus();
    });
    addColumnEl.replaceChildren(btn);
  }

  function columnMenu(colId, anchor) {
    const col = S.board.columns.find((c) => c.id === colId);
    if (!col) return;
    const cols = sortedColumns();
    const i = cols.findIndex((c) => c.id === colId);
    const items = [];
    if (editor()) {
      items.push(
        { label: 'Add card', icon: 'plus', onSelect: () => openComposer(colId) },
        { label: 'Rename…', icon: 'edit', onSelect: () => renameColumn(colId) },
        { label: 'Color…', icon: 'circle-dot', onSelect: () => colorColumn(colId, anchor) },
        { label: col.wip_limit ? `WIP limit (${col.wip_limit})…` : 'Set WIP limit…', icon: 'sliders', onSelect: () => wipColumn(colId) },
        { label: 'Move left', icon: 'arrow-left', disabled: i <= 0, onSelect: () => moveColumn(colId, i - 1) },
        { label: 'Move right', icon: 'arrow-right', disabled: i >= cols.length - 1, onSelect: () => moveColumn(colId, i + 1) },
      );
    }
    items.push({ label: isCollapsed(col) ? 'Expand' : 'Collapse', icon: isCollapsed(col) ? 'chevrons-right' : 'chevrons-left', onSelect: () => toggleCollapse(colId, !isCollapsed(col)) });
    if (editor()) items.push({ separator: true }, { label: 'Delete column…', icon: 'trash', danger: true, onSelect: () => deleteColumn(colId) });
    ui.openMenu({ anchor, items, align: 'end' });
  }

  async function patchColumn(colId, patch) {
    try {
      const col = await kapi.updateColumn(colId, patch);
      S.board.columns = S.board.columns.map((c) => (c.id === colId ? col : c));
      render();
      refreshSoon();
    } catch (err) {
      ui.toast.error(err);
    }
  }

  async function renameColumn(colId) {
    const col = S.board.columns.find((c) => c.id === colId);
    const name = await ui.prompt({ title: 'Rename column', label: 'Name', value: col.name, confirmLabel: 'Rename', validate: (v) => (!v.trim() ? 'Enter a name.' : [...v.trim()].length > 200 ? 'Use at most 200 characters.' : null) });
    if (name !== null && name.trim() !== col.name) patchColumn(colId, { name: name.trim() });
  }

  function colorColumn(colId, anchor) {
    const col = S.board.columns.find((c) => c.id === colId);
    const sw = colorSwatches({ value: col.color, allowNone: true, label: 'Column color', onChange: (c) => { closePopover(); patchColumn(colId, { color: c }); } });
    popover({ anchor: colEls.get(colId)?._kb.menuBtn || anchor, content: h('div', { class: 'kb-pop-body' }, h('div', { class: 'kb-pop-title' }, `Color of “${col.name}”`), sw.el), label: 'Column color', align: 'end' });
  }

  async function wipColumn(colId) {
    const col = S.board.columns.find((c) => c.id === colId);
    const v = await ui.prompt({
      title: 'WIP limit', label: 'Maximum cards in this column', type: 'number', value: col.wip_limit ? String(col.wip_limit) : '', required: false,
      hint: 'Advisory: the column is highlighted when it holds more cards. Leave empty for no limit.', confirmLabel: 'Save',
      validate: (x) => (x.trim() === '' || (/^\d+$/.test(x.trim()) && Number(x) >= 0 && Number(x) <= 10000) ? null : 'Enter a number from 0 to 10000.'),
    });
    if (v === null) return;
    const n = v.trim() === '' ? null : Number(v);
    patchColumn(colId, { wip_limit: n || null });
  }

  function toggleCollapse(colId, collapsed) {
    const col = S.board.columns.find((c) => c.id === colId);
    if (!col) return;
    if (editor()) {
      if (!collapsed) S.localCollapsed.delete(colId);
      col.collapsed = collapsed; // optimistic
      render();
      patchColumn(colId, { collapsed });
    } else {
      if (collapsed) S.localCollapsed.add(colId);
      else S.localCollapsed.delete(colId);
      render();
    }
  }

  async function deleteColumn(colId) {
    const col = S.board.columns.find((c) => c.id === colId);
    const holding = S.board.cards.filter((c) => c.column_id === colId).length;
    let moveTo = null;
    if (holding) {
      const others = sortedColumns().filter((c) => c.id !== colId);
      if (!others.length) {
        ui.toast.warning('Move or delete the cards first: this is the only column.');
        return;
      }
      const sel = h('select', { class: 'select', aria: { label: 'Move cards to' } }, others.map((c) => h('option', { value: c.id }, c.name)));
      const m = ui.modal({
        title: `Delete “${col.name}”?`, size: 'sm',
        body: h('div', { class: 'stack-sm' }, h('p', { class: 'confirm-msg' }, `The column holds ${plural(holding, 'card')} (archived ones included). Where should they go?`),
          h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Move cards to'), sel)),
        footer: [{ label: 'Cancel', value: null }, { label: 'Move cards and delete', variant: 'danger', type: 'submit' }],
        onSubmit: () => sel.value,
      });
      moveTo = await m.closed;
      if (!moveTo) return;
    } else if (!(await ui.confirm({ title: `Delete “${col.name}”?`, message: 'The column is empty.', confirmLabel: 'Delete column', danger: true }))) {
      return;
    }
    try {
      await kapi.deleteColumn(colId, moveTo);
      const res = await kapi.board(S.id, { includeArchived: true });
      setBoard(res.board, res.etag);
      render();
      ui.toast.success(moveTo ? 'Column deleted, cards moved' : 'Column deleted');
    } catch (err) {
      ui.toast.error(err);
    }
  }

  /* ------------------------------------------------------------ board ops */
  async function patchBoard(patch) {
    try {
      const b = await kapi.updateBoard(S.id, patch);
      // Write answers leave out archived cards: keep the local list and poll for the rest.
      setBoard({ ...b, cards: S.board.cards }, null);
      refreshSoon();
      render();
    } catch (err) {
      ui.toast.error(err);
    }
  }

  titleBtn.addEventListener('click', async () => {
    if (!admin()) return;
    const name = await ui.prompt({ title: 'Rename board', label: 'Name', value: S.board.name, confirmLabel: 'Rename', validate: (v) => (!v.trim() ? 'Enter a name.' : [...v.trim()].length > 200 ? 'Use at most 200 characters.' : null) });
    if (name !== null && name.trim() !== S.board.name) patchBoard({ name: name.trim() });
  });
  membersEl.addEventListener('click', () => settings('members'));

  function settings(tab = 'general') {
    openBoardSettings(app, {
      board: () => S.board,
      tab,
      onBoard: (b) => {
        if (!b) return;
        setBoard({ ...b, cards: b.cards.length > S.board.cards.length ? b.cards : S.board.cards }, null);
        render();
        refreshSoon();
      },
      onLeft: () => ctx.router.navigate(paths.boards()),
      onDeleted: () => ctx.router.navigate(paths.boards()),
    });
  }

  /* ------------------------------------------------------------------ DnD */
  let dnd = null;
  let colDnd = null;
  let wired = false;
  function wireOnce() {
    if (wired) return;
    wired = true;
    dnd = createDnd({
      root: columnsEl, itemSelector: '.kb-tile', axis: 'y', scroller,
      containers: () => sortedColumns().filter((c) => !isCollapsed(c)).map((c) => {
        const el = colEls.get(c.id);
        return el ? { id: c.id, el, list: el._kb.list, scroll: el._kb.body } : null;
      }).filter(Boolean),
      canDrag: () => editor(),
      onStart: () => closePopover(),
      onDrop: ({ id, containerId, beforeId }) => moveCard(id, containerId, beforeId),
    });
    colDnd = createDnd({
      root: columnsEl, itemSelector: '.kb-column', handleSelector: '.kb-col-head', axis: 'x', scroller,
      containers: () => [{ id: 'columns', el: columnsEl, list: columnsEl }],
      canDrag: () => editor(),
      onDrop: ({ id, beforeId }) => {
        const cols = sortedColumns();
        const from = cols.findIndex((c) => c.id === id);
        let to = beforeId ? cols.findIndex((c) => c.id === beforeId) : cols.length;
        if (from < to) to -= 1;
        if (to !== from) moveColumn(id, to);
      },
    });
    offs.push(() => dnd.destroy(), () => colDnd.destroy());
  }

  /* ---------------------------------------------------------------- detail */
  function openCard(cardId, { pushed = true } = {}) {
    if (detail?.cardId === cardId) return;
    if (detail) detail.close({ silent: true });
    if (pushed) {
      S.pushedCard = cardId;
      ctx.router.navigate(paths.card(S.id, cardId) + location.search);
      return; // the route update opens it
    }
    detail = openCardDetail(app, {
      cardId,
      board: () => S.board,
      editor, admin,
      members: () => members,
      labels: () => labels,
      moveCard: (id, col, before) => moveCard(id, col, before),
      cardChanged: (card) => { replaceCard(card); render(); refreshSoon(); },
      cardRemoved: (id) => removeCardLocal(id),
      boardChanged: () => refreshSoon(),
      labelsChanged: async () => {
        try {
          const res = await kapi.board(S.id, { includeArchived: true });
          setBoard(res.board, res.etag);
          render();
        } catch (err) { ui.toast.error(err); }
      },
      deleteCard: (id) => deleteCard(id),
      onClosed: (byUser) => {
        const id = detail?.cardId;
        detail = null;
        if (byUser) {
          const back = S.openedWithPush;
          S.openedWithPush = false;
          if (back) history.back();
          else ctx.router.navigate(paths.board(S.id) + location.search, { replace: true });
        }
        if (id) requestAnimationFrame(() => tileEls.get(id)?.focus({ preventScroll: true }));
      },
    });
  }

  /* ------------------------------------------------------------ shortcuts */
  const noOverlay = () => !document.documentElement.classList.contains('modal-open') && !document.querySelector('.palette-backdrop');
  const searchCfg = () => ({
    placeholder: 'Filter cards', value: S.filter.q,
    onInput: (q) => setFilter({ q }), onSubmit: (q) => setFilter({ q }),
  });
  ctx.setSearch(searchCfg());
  const focusSearch = () => {
    const box = document.querySelector('.topbar-search');
    if (box && getComputedStyle(box).display === 'none') document.querySelector('.topbar-search-btn')?.click();
    else document.querySelector('.topbar-search-input')?.focus();
  };
  const firstColumnId = () => {
    const focused = document.activeElement?.closest?.('.kb-column')?.dataset.id;
    return focused || sortedColumns().find((c) => !isCollapsed(c))?.id || sortedColumns()[0]?.id;
  };
  offs.push(
    ctx.shortcuts.register('n', () => { const c = firstColumnId(); if (c) openComposer(c); }, { description: 'New card', when: () => !!S.board && editor() && noOverlay() }),
    ctx.shortcuts.register('/', () => focusSearch(), { description: 'Filter cards', when: noOverlay }),
    ctx.shortcuts.register('shift+a', () => setArchived(!S.showArchived), { description: 'Show or hide archived cards', when: () => !!S.board && noOverlay() }),
    ctx.shortcuts.register('g b', () => ctx.router.navigate(paths.boards()), { description: 'Go to all boards', when: noOverlay }),
    ctx.shortcuts.register('alt+arrowup', () => {}, { description: 'Move the focused card up (also ↓ ← →)', when: () => false }),
    ctx.commands.register(() => (S.board ? [
      ...(editor() ? [
        { id: 'kanban:new-card', title: 'New card', subtitle: S.board.name, icon: 'plus', group: 'Kanban', shortcut: 'N', run: () => { const c = firstColumnId(); if (c) openComposer(c); } },
        { id: 'kanban:new-column', title: 'Add column', subtitle: S.board.name, icon: 'plus', group: 'Kanban', run: () => addColumnEl.querySelector('button')?.click() },
      ] : []),
      { id: 'kanban:settings', title: 'Board settings', subtitle: S.board.name, icon: 'settings', group: 'Kanban', keywords: ['members', 'labels'], run: () => settings('general') },
      { id: 'kanban:activity', title: 'Board activity', subtitle: S.board.name, icon: 'activity', group: 'Kanban', keywords: ['history', 'log'], run: () => activity() },
      { id: 'kanban:archived', title: S.showArchived ? 'Hide archived cards' : 'Show archived cards', icon: 'archive', group: 'Kanban', run: () => setArchived(!S.showArchived) },
      ...(filterActive(S.filter) ? [{ id: 'kanban:clear', title: 'Clear card filters', icon: 'x', group: 'Kanban', run: () => clearFilters() }] : []),
      { id: 'kanban:boards', title: 'All boards', icon: 'kanban', group: 'Kanban', run: () => ctx.router.navigate(paths.boards()) },
      { id: 'kanban:my', title: 'My cards', icon: 'user', group: 'Kanban', keywords: ['assigned', 'due'], run: () => ctx.router.navigate(paths.my()) },
    ] : [])),
  );

  function activity() {
    openActivity(app, { board: () => S.board, openCard: (id) => { if (cardById(id)) openCard(id); else ui.toast.info('That card no longer exists.'); } });
  }

  ctx.setActions([
    iconButton('activity', 'Activity', () => activity(), { size: 'md' }),
    iconButton('settings', 'Board settings', () => settings('general'), { size: 'md' }),
  ]);

  load();

  return {
    /** React to a route change within the board (card opened/closed, filters). */
    update(r) {
      const f = filterFromQuery(r.query);
      const changed = signature(f) !== signature(S.filter);
      S.filter = f;
      const arch = r.query.archived === '1';
      if (arch !== S.showArchived) S.showArchived = arch;
      if (changed) ctx.setSearch(searchCfg());
      if (S.board) render();
      if (r.cardId) {
        if (detail?.cardId !== r.cardId && S.board) {
          S.openedWithPush = S.pushedCard === r.cardId;
          S.pushedCard = null;
          openCard(r.cardId, { pushed: false });
        }
      } else if (detail) {
        detail.close({ silent: true });
        detail = null;
      }
    },
    destroy() {
      S.destroyed = true;
      clearTimeout(S.pollTimer);
      closePopover();
      detail?.close({ silent: true });
      detail = null;
      for (const off of offs.splice(0)) off?.();
      ctx.setActions(null);
      ctx.setSearch(null);
      ctx.setLayout({});
    },
  };
}

