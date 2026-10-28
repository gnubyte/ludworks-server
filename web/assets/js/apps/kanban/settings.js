// Board settings (general, members, labels, ownership and deletion) and the activity drawer.

import { h, cls } from '../../core/dom.js';
import { plural } from '../../core/format.js';
import { activityText, can, memberName, pickLabelColor } from './model.js';
import { colorSwatches, field, iconButton, labelChip, memberAvatar, popover, closePopover, userSearch } from './widgets.js';

const ROLE_HELP = {
  admin: 'Edits the board, its members and everything on it',
  editor: 'Changes columns, cards, labels, subtasks and attachments',
  viewer: 'Reads and comments',
};

/**
 * Open the settings dialog of a board.
 * @param {object} app {ctx, kapi, me}
 * @param {{board: () => object, tab?: string, onBoard: (b: object) => void, onLeft: () => void, onDeleted: () => void}} o
 */
export function openBoardSettings(app, o) {
  const { ctx, kapi } = app;
  const { ui } = ctx;
  const panel = h('div', { class: 'kb-settings-panel' });
  let tab = o.tab || 'general';
  const b = () => o.board();
  const role = () => b().role;

  const tabs = ui.tabs({
    items: [
      { id: 'general', label: 'General', icon: 'sliders' },
      { id: 'members', label: 'Members', icon: 'users' },
      { id: 'labels', label: 'Labels', icon: 'tag' },
      { id: 'more', label: 'Ownership', icon: 'shield' },
    ],
    active: tab, variant: 'pills', ariaLabel: 'Board settings',
    onChange: (id) => { tab = id; draw(); },
  });
  const m = ui.modal({ title: 'Board settings', description: b().name, size: 'lg', className: 'kb-settings', body: h('div', { class: 'stack' }, tabs.el, panel) });

  function changed(board) {
    o.onBoard(board);
    m.el.querySelector('.modal-desc')?.replaceChildren(board.name);
    draw();
  }

  function draw() {
    closePopover();
    if (tab === 'general') panel.replaceChildren(general());
    else if (tab === 'members') panel.replaceChildren(membersPanel());
    else if (tab === 'labels') panel.replaceChildren(labelsPanel());
    else panel.replaceChildren(morePanel());
  }

  /* ---------------------------------------------------------------- general */
  function general() {
    const board = b();
    const editable = can(role(), 'admin');
    const name = h('input', { class: 'input', value: board.name, maxlength: 200, disabled: !editable, required: true });
    const desc = h('textarea', { class: 'textarea', rows: 4, maxlength: 100000, disabled: !editable, placeholder: 'What is this board for? (Markdown)' });
    desc.value = board.description;
    let color = board.color;
    const sw = colorSwatches({ value: board.color, label: 'Board color', onChange: (c) => { color = c; } });
    if (!editable) for (const x of sw.el.querySelectorAll('button')) x.disabled = true;
    const save = async () => {
      if (!name.value.trim()) { name.focus(); return; }
      try {
        changed(await kapi.updateBoard(board.id, { name: name.value.trim(), description: desc.value, color }));
        ui.toast.success('Board saved');
      } catch (err) {
        ui.toast.error(err);
      }
    };
    const archive = async () => {
      try {
        changed(await kapi.updateBoard(board.id, { archived: !board.archived }));
        ui.toast.success(board.archived ? 'Board restored' : 'Board archived', { description: board.archived ? '' : 'Find it under Archived on the boards page.' });
      } catch (err) {
        ui.toast.error(err);
      }
    };
    return h('div', { class: 'form-grid' },
      field('Name', name),
      field('Description', desc),
      h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Color'), sw.el),
      editable ? h('div', { class: 'row between wrap' },
        ui.button({ label: board.archived ? 'Restore board' : 'Archive board', icon: board.archived ? 'restore' : 'archive', variant: 'ghost', onClick: archive }),
        ui.button({ label: 'Save', variant: 'primary', onClick: save }))
        : h('p', { class: 'faint small' }, 'Only board admins can change these settings.'));
  }

  /* ---------------------------------------------------------------- members */
  function membersPanel() {
    const board = b();
    const isAdmin = can(role(), 'admin');
    const list = h('div', { class: 'kb-member-list' });
    const put = async (members) => {
      try {
        changed(await kapi.setMembers(board.id, members));
      } catch (err) {
        ui.toast.error(err);
        draw();
      }
    };
    const current = () => b().members.filter((x) => x.role !== 'owner').map((x) => ({ user_id: x.user_id, role: x.role }));
    for (const mb of board.members) {
      let roleEl;
      if (mb.role === 'owner') roleEl = h('span', { class: 'badge badge-accent' }, 'Owner');
      else if (isAdmin) {
        roleEl = h('select', { class: 'select select-sm', aria: { label: `Role of ${memberName(mb)}` } },
          ['admin', 'editor', 'viewer'].map((r) => h('option', { value: r, selected: r === mb.role }, r[0].toUpperCase() + r.slice(1))));
        roleEl.addEventListener('change', () => put(current().map((x) => (x.user_id === mb.user_id ? { ...x, role: roleEl.value } : x))));
      } else roleEl = h('span', { class: 'badge' }, mb.role[0].toUpperCase() + mb.role.slice(1));
      const remove = isAdmin && mb.role !== 'owner' && mb.user_id !== app.me
        ? iconButton('x', `Remove ${memberName(mb)}`, async () => {
          if (!(await ui.confirm({ title: `Remove ${memberName(mb)}?`, message: 'They lose access to the board and are unassigned from its cards and subtasks.', confirmLabel: 'Remove', danger: true }))) return;
          put(current().filter((x) => x.user_id !== mb.user_id));
        }) : null;
      list.append(h('div', { class: 'kb-member-row' }, memberAvatar(mb, 32),
        h('div', { class: 'kb-member-names' }, h('span', { class: 'truncate bold' }, memberName(mb), mb.user_id === app.me ? h('span', { class: 'faint' }, ' (you)') : null),
          h('span', { class: 'faint small truncate' }, `@${mb.username}`)),
        roleEl, remove));
    }
    const parts = [h('div', { class: 'section-header' }, h('h3', { class: 'section-title' }, plural(board.members.length, 'member')),
      h('span', { class: 'faint small' }, 'At most 100 members')), list];
    if (isAdmin) {
      const roleSel = h('select', { class: 'select', aria: { label: 'Role for new members' } },
        ['editor', 'viewer', 'admin'].map((r) => h('option', { value: r }, `${r[0].toUpperCase()}${r.slice(1)} — ${ROLE_HELP[r].toLowerCase()}`)));
      const search = userSearch({
        kapi, exclude: () => new Set(b().members.map((x) => x.user_id)),
        onPick: (u) => put([...current(), { user_id: u.id, role: roleSel.value }]),
      });
      parts.push(h('div', { class: 'kb-add-member stack-sm' }, h('h3', { class: 'section-title' }, 'Add people'), roleSel, search.el));
    } else {
      parts.push(h('p', { class: 'faint small' }, 'Only board admins can add or remove members.'));
    }
    parts.push(h('dl', { class: 'kb-role-help' }, Object.entries(ROLE_HELP).map(([r, t]) => [h('dt', {}, r[0].toUpperCase() + r.slice(1)), h('dd', {}, t)]),
      h('dt', {}, 'Owner'), h('dd', {}, 'Like an admin, and can delete or transfer the board')));
    return h('div', { class: 'stack' }, ...parts);
  }

  /* ----------------------------------------------------------------- labels */
  function labelsPanel() {
    const board = b();
    const editable = can(role(), 'editor');
    const list = h('div', { class: 'kb-label-list' });
    const refresh = async () => {
      try {
        const res = await kapi.board(board.id, { includeArchived: true });
        changed(res.board);
      } catch (err) {
        ui.toast.error(err);
      }
    };
    for (const l of board.labels) {
      if (!editable) { list.append(h('div', { class: 'kb-label-row' }, labelChip(l))); continue; }
      const name = h('input', { class: 'input input-sm', value: l.name, maxlength: 200, aria: { label: 'Label name' } });
      const swatchBtn = h('button', { type: 'button', class: cls('kb-swatch', `kb-c-${l.color}`), aria: { label: 'Change color', haspopup: 'true' }, dataset: { tip: 'Change color' } });
      swatchBtn.addEventListener('click', () => {
        const sw = colorSwatches({ value: l.color, label: 'Label color', onChange: async (c) => {
          closePopover();
          try { await kapi.updateLabel(l.id, { color: c }); await refresh(); } catch (err) { ui.toast.error(err); }
        } });
        popover({ anchor: swatchBtn, content: h('div', { class: 'kb-pop-body' }, sw.el), label: 'Label color' });
      });
      const rename = async () => {
        const v = name.value.trim();
        if (!v || v === l.name) { name.value = l.name; return; }
        try { await kapi.updateLabel(l.id, { name: v }); await refresh(); } catch (err) { ui.toast.error(err); name.value = l.name; }
      };
      name.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); name.blur(); } });
      name.addEventListener('change', rename);
      const used = b().cards.filter((c) => c.labels.includes(l.id)).length;
      const del = iconButton('trash', `Delete “${l.name}”`, async () => {
        if (!(await ui.confirm({ title: `Delete the label “${l.name}”?`, message: used ? `It is removed from ${plural(used, 'card')}.` : 'No card uses it.', confirmLabel: 'Delete label', danger: true }))) return;
        try { await kapi.deleteLabel(l.id); await refresh(); } catch (err) { ui.toast.error(err); }
      });
      list.append(h('div', { class: 'kb-label-row' }, swatchBtn, name, h('span', { class: 'faint small nums kb-label-used' }, plural(used, 'card')), del));
    }
    if (!board.labels.length) list.append(h('p', { class: 'faint small' }, 'No labels yet.'));
    const parts = [list];
    if (editable) {
      const name = h('input', { class: 'input', placeholder: 'New label name', maxlength: 200, aria: { label: 'New label name' } });
      let color = pickLabelColor(board.labels);
      const sw = colorSwatches({ value: color, label: 'New label color', onChange: (c) => { color = c; } });
      const add = async () => {
        const v = name.value.trim();
        if (!v) { name.focus(); return; }
        try { await kapi.createLabel(board.id, { name: v, color }); await refresh(); } catch (err) { ui.toast.error(err); }
      };
      name.addEventListener('keydown', (e) => { if (e.key === 'Enter') { e.preventDefault(); add(); } });
      parts.push(h('div', { class: 'kb-add-label stack-sm' }, h('h3', { class: 'section-title' }, 'New label'), name, sw.el,
        h('div', { class: 'row end' }, ui.button({ label: 'Add label', variant: 'primary', onClick: add }))));
    }
    return h('div', { class: 'stack' }, ...parts);
  }

  /* ---------------------------------------------------------- ownership etc. */
  function morePanel() {
    const board = b();
    const owner = board.members.find((x) => x.role === 'owner');
    const parts = [h('p', { class: 'muted' }, `Owned by ${memberName(owner)}. Your role: ${board.role}.`)];
    if (board.role === 'owner') {
      const others = board.members.filter((x) => x.role !== 'owner');
      const sel = h('select', { class: 'select', disabled: !others.length, aria: { label: 'New owner' } },
        others.length ? others.map((x) => h('option', { value: x.user_id }, memberName(x))) : h('option', {}, 'Add a member first'));
      parts.push(ui.card({
        title: 'Transfer ownership', description: 'The new owner can delete the board; you stay on it as an admin.',
        body: h('div', { class: 'row wrap' }, sel, ui.button({
          label: 'Transfer', disabled: !others.length, onClick: async () => {
            const target = others.find((x) => x.user_id === sel.value);
            if (!target || !(await ui.confirm({ title: `Make ${memberName(target)} the owner?`, message: 'You become an admin of the board.', confirmLabel: 'Transfer ownership', danger: true }))) return;
            try {
              changed(await kapi.transfer(board.id, target.user_id));
              ui.toast.success(`${memberName(target)} now owns the board`);
            } catch (err) { ui.toast.error(err); }
          },
        })),
      }));
      parts.push(ui.card({
        title: 'Delete board', description: 'Deletes every column, card, comment and attachment. This cannot be undone.', className: 'danger-zone',
        body: ui.button({
          label: 'Delete board…', variant: 'danger', icon: 'trash', onClick: async () => {
            const ok = await ui.confirm({ title: `Delete “${board.name}”?`, message: `${plural(board.cards.length, 'card')} and everything on them will be deleted permanently.`, requireText: board.name, confirmLabel: 'Delete board', danger: true });
            if (!ok) return;
            try {
              await kapi.deleteBoard(board.id);
              m.close();
              ui.toast.success('Board deleted');
              o.onDeleted();
            } catch (err) { ui.toast.error(err); }
          },
        }),
      }));
    } else {
      parts.push(ui.card({
        title: 'Leave board', description: 'You lose access and are unassigned from its cards. An admin can add you again.',
        body: ui.button({
          label: 'Leave board', variant: 'danger', icon: 'log-out', onClick: async () => {
            if (!(await ui.confirm({ title: `Leave “${board.name}”?`, confirmLabel: 'Leave board', danger: true }))) return;
            try {
              await kapi.leave(board.id);
              m.close();
              ui.toast.success(`You left “${board.name}”`);
              o.onLeft();
            } catch (err) { ui.toast.error(err); }
          },
        }),
      }));
    }
    return h('div', { class: 'stack' }, ...parts);
  }

  draw();
  return m;
}

/**
 * The board's activity log in a drawer (newest first, loads more on scroll).
 * @param {object} app
 * @param {{board: () => object, openCard: (id: string) => void}} o
 */
export function openActivity(app, o) {
  const { ctx, kapi } = app;
  const { ui } = ctx;
  const board = o.board();
  const members = new Map(board.members.map((x) => [x.user_id, x]));
  const users = (id) => memberName(members.get(id)) || 'a former member';
  const list = h('ol', { class: 'kb-activity kb-activity-full' });
  const loader = ui.infiniteList({
    fetchPage: (cursor) => kapi.activity(board.id, { cursor: cursor || undefined, limit: 50, signal: ctx.signal }),
    onItems: (items, { reset }) => {
      if (reset) list.replaceChildren();
      for (const a of items) {
        const text = activityText(a, { users });
        const exists = a.card_id && o.board().cards.some((c) => c.id === a.card_id);
        const content = exists
          ? h('button', { type: 'button', class: 'kb-activity-link', onClick: () => { dr.close(); o.openCard(a.card_id); } }, text)
          : h('span', { class: 'kb-activity-text' }, text);
        list.append(h('li', { class: 'kb-activity-item' },
          members.get(a.actor_id) ? memberAvatar(members.get(a.actor_id), 24) : h('span', { class: 'kb-avatar' }, ui.avatar({ name: a.actor_name, size: 24 })),
          h('div', { class: 'kb-activity-body' }, content, ui.timeAgo(a.created_at))));
      }
      if (reset && !items.length) list.replaceChildren(ui.emptyState({ icon: 'activity', title: 'No activity yet', compact: true }));
    },
  });
  const dr = ui.drawer({ title: 'Activity', description: board.name, size: 'md', body: h('div', { class: 'stack-sm' }, list, loader.el), onClose: () => loader.destroy() });
  loader.loadMore();
  return dr;
}

