// Admin › Libraries: shared spaces (/admin/spaces) with per-user or everyone
// read/write membership.

import { h, debounce } from '../../core/dom.js';
import { icon } from '../../core/icons.js';
import { field, input, select, clearErrors, showFieldErrors, showFormError } from '../../core/forms.js';
import { formatBytes, formatNumber } from '../../core/format.js';

const ACCESS = [{ value: 'read', label: 'Can view' }, { value: 'write', label: 'Can edit' }];

function listOf(res) {
  return Array.isArray(res) ? res : res?.items || [];
}

export function render(host, ctx) {
  const { ui } = ctx;
  const userNames = new Map(); // id → username cache for member display
  let spaces = [];

  const table = ui.dataTable({
    caption: 'Libraries',
    rowKey: 'id',
    sort: { key: 'name', dir: 'asc' },
    columns: [
      {
        key: 'name', label: 'Library', sortable: true,
        render: (s) => h('div', { class: 'cell-main' }, h('span', { class: 'token-icon' }, icon('library')),
          h('div', { class: 'cell-stack' }, h('span', { class: 'cell-title' }, s.name), h('span', { class: 'cell-sub mono' }, s.id))),
      },
      { key: 'members', label: 'Access', render: (s) => membersSummary(s) },
      { key: 'usage_bytes', label: 'Size', sortable: true, align: 'right', hideBelow: 'sm', render: (s) => formatBytes(s.usage_bytes || 0) },
      { key: 'file_count', label: 'Files', sortable: true, align: 'right', hideBelow: 'md', render: (s) => (s.file_count === undefined ? '—' : formatNumber(s.file_count)) },
      {
        key: 'actions', label: '', align: 'right',
        render: (s) => h('div', { class: 'cell-actions' },
          ui.button({ icon: 'edit', title: 'Edit library', size: 'sm', variant: 'ghost', onClick: () => edit(s) }),
          ui.button({ icon: 'trash', title: 'Delete library', size: 'sm', variant: 'ghost', onClick: () => remove(s) })),
      },
    ],
    onRowClick: (s) => edit(s),
    empty: () => ui.emptyState({
      icon: 'library', title: 'No libraries yet', compact: true,
      message: 'Libraries are shared folders — for example a family movie collection everyone can watch.',
      action: ui.button({ label: 'New library', icon: 'plus', variant: 'primary', size: 'sm', onClick: () => edit(null) }),
    }),
  });

  function membersSummary(s) {
    const members = s.members || [];
    if (!members.length) return h('span', { class: 'faint small' }, 'Admins only');
    const everyone = members.find((m) => m.user_id === '*');
    const others = members.filter((m) => m.user_id !== '*');
    return h('div', { class: 'chips' },
      everyone ? ui.badge(`Everyone · ${everyone.access === 'write' ? 'edit' : 'view'}`, 'accent', { icon: 'users' }) : null,
      others.slice(0, 3).map((m) => ui.badge(`${m.username || userNames.get(m.user_id) || m.user_id.slice(0, 6)} · ${m.access === 'write' ? 'edit' : 'view'}`, 'neutral')),
      others.length > 3 ? ui.badge(`+${others.length - 3}`, 'neutral') : null);
  }

  async function resolveNames(list) {
    const missing = [...new Set(list.flatMap((s) => (s.members || []).map((m) => m.user_id)).filter((id) => id !== '*' && !userNames.has(id)))];
    await Promise.allSettled(missing.slice(0, 50).map(async (id) => {
      const u = await ctx.api.get(`/admin/users/${encodeURIComponent(id)}`, { signal: ctx.signal });
      userNames.set(id, u.username);
    }));
  }

  async function load() {
    table.setLoading(true, 3);
    try {
      spaces = listOf(await ctx.api.get('/admin/spaces', { signal: ctx.signal }));
      for (const s of spaces) for (const m of s.members || []) if (m.username) userNames.set(m.user_id, m.username);
      table.setRows(spaces);
      await resolveNames(spaces);
      table.setRows(spaces);
    } catch (err) {
      if (err?.name === 'AbortError') return;
      table.setRows([]);
      ui.toast.error(err);
    }
  }

  function memberEditor(initial) {
    let members = initial.map((m) => ({ user_id: m.user_id, access: m.access === 'write' ? 'write' : 'read' }));
    const list = h('div', { class: 'member-list' });
    const results = h('div', { class: 'picker-results', role: 'listbox', aria: { label: 'Matching users' } });
    const search = input({ type: 'search', placeholder: 'Add people — search by name or email', className: 'has-icon', autocomplete: 'off', 'aria-label': 'Search users to add' });
    const render = () => {
      list.replaceChildren(...(members.length ? members.map((m) => {
        const acc = select({ options: ACCESS, value: m.access, 'aria-label': 'Access level' });
        acc.addEventListener('change', () => { m.access = acc.value; });
        const label = m.user_id === '*' ? 'Everyone' : userNames.get(m.user_id) || m.user_id;
        return h('div', { class: 'member-row' },
          h('div', { class: 'member-name' }, m.user_id === '*' ? h('span', { class: 'session-icon' }, icon('users')) : ui.avatar({ name: label, size: 26 }),
            h('span', { class: 'truncate' }, label)),
          acc,
          ui.button({ icon: 'x', title: `Remove ${label}`, size: 'sm', variant: 'ghost', onClick: () => { members = members.filter((x) => x !== m); render(); } }));
      }) : [h('p', { class: 'small faint' }, 'No members yet: only administrators can open this library.')]));
    };
    const add = (id, name) => {
      if (name) userNames.set(id, name);
      if (!members.some((m) => m.user_id === id)) members.push({ user_id: id, access: 'read' });
      search.value = '';
      results.replaceChildren();
      render();
    };
    const lookup = debounce(async () => {
      const q = search.value.trim();
      if (!q) {
        results.replaceChildren();
        return;
      }
      try {
        const res = await ctx.api.get('/admin/users', { query: { q, limit: 8 } });
        const opts = [];
        if ('everyone'.startsWith(q.toLowerCase())) opts.push(['*', 'Everyone (all users)']);
        for (const u of listOf(res)) opts.push([u.id, u.username, u.email]);
        results.replaceChildren(...opts.filter(([id]) => !members.some((m) => m.user_id === id)).map(([id, name, mail]) => {
          const b = h('button', { type: 'button', class: 'picker-result', role: 'option' },
            id === '*' ? icon('users') : ui.avatar({ name, size: 22 }), h('span', { class: 'bold' }, name), mail ? h('span', { class: 'small faint truncate' }, mail) : null);
          b.addEventListener('click', () => add(id, id === '*' ? null : name));
          return b;
        }));
        if (!results.childElementCount) results.append(h('div', { class: 'small faint' }, 'No matching users'));
      } catch (err) {
        ui.toast.error(err);
      }
    }, 200);
    search.addEventListener('input', lookup);
    search.addEventListener('keydown', (e) => {
      if (e.key === 'Enter') {
        e.preventDefault();
        results.querySelector('.picker-result')?.click();
      }
    });
    render();
    return {
      el: h('div', { class: 'field', dataset: { field: 'members' } },
        h('div', { class: 'row between' }, h('span', { class: 'field-label' }, 'Members'),
          ui.button({ label: 'Everyone', icon: 'users', size: 'sm', variant: 'ghost', onClick: () => add('*') })),
        list,
        h('div', { class: 'input-group' }, icon('search', { className: 'input-icon' }), search),
        results,
        h('div', { class: 'field-error', role: 'alert', hidden: true })),
      get: () => members.map((m) => ({ user_id: m.user_id, access: m.access })),
    };
  }

  function edit(space) {
    const isNew = !space;
    const name = input({ name: 'name', value: space?.name || '', maxlength: '100', placeholder: 'e.g. Family Movies' });
    const editor = memberEditor(space?.members || [{ user_id: '*', access: 'read' }]);
    const m = ui.modal({
      title: isNew ? 'New library' : `Edit ${space.name}`,
      description: 'Libraries are shared spaces in Files. Members can view (and optionally edit) everything inside.',
      size: 'lg',
      body: h('div', { class: 'stack' }, field({ label: 'Name', control: name, name: 'name' }), editor.el),
      footer: [{ label: 'Cancel', value: null }, { label: isNew ? 'Create library' : 'Save', variant: 'primary', type: 'submit' }],
      onSubmit: async () => {
        clearErrors(m.form);
        const n = name.value.trim();
        if (!n) {
          showFieldErrors(m.form, { name: 'Give the library a name.' });
          return false;
        }
        const members = editor.get();
        try {
          if (isNew) {
            await ctx.api.post('/admin/spaces', { name: n, members });
            ui.toast.success(`Library “${n}” created`);
          } else {
            const body = {};
            if (n !== space.name) body.name = n;
            body.members = members;
            await ctx.api.patch(`/admin/spaces/${encodeURIComponent(space.id)}`, body);
            ui.toast.success('Library updated');
          }
          load();
          return true;
        } catch (err) {
          if (err.fields) showFieldErrors(m.form, err.fields);
          else showFormError(m.form, err, { bannerHost: m.body });
          return false;
        }
      },
    });
  }

  async function remove(space) {
    const ok = await ui.confirm({
      title: `Delete “${space.name}”?`,
      message: 'Members lose access immediately. Empty libraries are deleted right away.',
      confirmLabel: 'Delete library', danger: true,
    });
    if (!ok) return;
    try {
      await ctx.api.del(`/admin/spaces/${encodeURIComponent(space.id)}`);
      ui.toast.success('Library deleted');
      load();
    } catch (err) {
      if (err.status !== 409) {
        ui.toast.error(err);
        return;
      }
      const force = await ui.confirm({
        title: 'This library isn’t empty',
        message: `It still contains ${space.file_count ? `${formatNumber(space.file_count)} files` : 'files'} (${formatBytes(space.usage_bytes || 0)}). Deleting it permanently removes them from disk.`,
        requireText: space.name, confirmLabel: 'Delete library and files', danger: true,
      });
      if (!force) return;
      try {
        await ctx.api.del(`/admin/spaces/${encodeURIComponent(space.id)}`, { query: { force: 'true' } });
        ui.toast.success('Library and its files deleted');
        load();
      } catch (e2) {
        ui.toast.error(e2);
      }
    }
  }

  host.append(
    h('div', { class: 'toolbar' },
      h('div', { class: 'stack-xs' }, h('h2', { class: 'section-title' }, 'Libraries'), h('p', { class: 'section-desc' }, 'Shared spaces for media collections and team folders.')),
      h('span', { class: 'spacer' }),
      ui.button({ icon: 'refresh', title: 'Refresh', variant: 'ghost', onClick: () => load() }),
      ui.button({ label: 'New library', icon: 'plus', variant: 'primary', onClick: () => edit(null) })),
    table.el);
  load();
}
