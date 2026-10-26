// Admin › Users: search, cursor-paginated table, create, edit (role, quota,
// disabled, email verified), reset password, revoke sessions, remove 2FA, delete.

import { h, debounce } from '../../core/dom.js';
import { icon } from '../../core/icons.js';
import { field, input, select, switchInput, passwordInput, checkbox, clearErrors, showFieldErrors, showFormError, diffObject } from '../../core/forms.js';
import { formatBytes, formatDate, parseBytes, formatPercent } from '../../core/format.js';
import { generatePassword } from '../../core/password.js';
import { displayName, setUser } from '../../core/store.js';
import { usageVariant } from '../../core/charts.js';

const PAGE_SIZE = 25;

/** Quota input: free text like "500 GiB" / "2 TB" / "0" (unlimited). */
function quotaInput(bytes = 0) {
  const el = input({ name: 'quota_bytes', value: bytes ? formatBytes(bytes) : '0', placeholder: 'e.g. 500 GiB, 0 = unlimited', spellcheck: 'false' });
  // Remember the exact value: the rounded display text must not cause a spurious change.
  el.dataset.initial = el.value;
  el.dataset.bytes = String(bytes);
  return el;
}

function readQuota(el) {
  if (el.value.trim() === el.dataset.initial) return Number(el.dataset.bytes);
  const v = parseBytes(el.value);
  return v === null ? NaN : v;
}

function statusBadges(ui, u) {
  return h('div', { class: 'chips' },
    u.disabled ? ui.badge('Disabled', 'danger', { dot: true }) : ui.badge('Active', 'success', { dot: true }),
    u.email_verified ? null : ui.badge('Unverified', 'warning'),
    u.totp_enabled ? ui.badge('2FA', 'accent', { icon: 'shield-check', title: 'Two-factor enabled' }) : null);
}

export function render(host, ctx) {
  const { ui } = ctx;
  let q = ctx.query.q || '';

  const search = input({ type: 'search', placeholder: 'Search by name or email…', value: q, className: 'has-icon', 'aria-label': 'Search users' });
  const newBtn = ui.button({ label: 'New user', icon: 'user-plus', variant: 'primary', onClick: () => createUser() });
  const table = ui.dataTable({
    caption: 'Users',
    rowKey: 'id',
    columns: [
      {
        key: 'username', label: 'User',
        render: (u) => h('div', { class: 'cell-main' }, ui.avatar({ name: displayName(u), size: 30 }),
          h('div', { class: 'cell-stack' },
            h('span', { class: 'cell-title row' }, displayName(u), u.id === ctx.user?.id ? ui.badge('You', 'accent') : null),
            h('span', { class: 'cell-sub' }, `@${u.username} · ${u.email || 'no email'}`))),
      },
      { key: 'role', label: 'Role', render: (u) => (u.role === 'admin' ? ui.badge('Admin', 'accent', { icon: 'shield' }) : ui.badge('User', 'neutral')) },
      { key: 'status', label: 'Status', hideBelow: 'sm', render: (u) => statusBadges(ui, u) },
      { key: 'quota_bytes', label: 'Quota', align: 'right', hideBelow: 'md', render: (u) => (u.quota_bytes ? formatBytes(u.quota_bytes) : h('span', { class: 'faint' }, 'Unlimited')) },
      { key: 'last_login_at', label: 'Last sign-in', hideBelow: 'md', render: (u) => (u.last_login_at ? ui.timeAgo(u.last_login_at) : h('span', { class: 'faint small' }, 'Never')) },
      { key: 'created_at', label: 'Created', hideBelow: 'md', render: (u) => h('span', { class: 'small muted nowrap' }, formatDate(u.created_at)) },
      {
        key: 'menu', label: '', align: 'right',
        render: (u) => ui.menuButton({ icon: 'more', title: 'Actions', size: 'sm', items: () => rowMenu(u) }),
      },
    ],
    onRowClick: (u) => openUser(u.id),
    onRowContextMenu: (u, e) => ui.contextMenu(e, rowMenu(u)),
    empty: () => ui.emptyState({
      icon: 'users', title: q ? 'No matching users' : 'No users yet', compact: true,
      message: q ? 'Try a different name or email.' : undefined,
      action: q ? null : ui.button({ label: 'New user', icon: 'user-plus', size: 'sm', onClick: () => createUser() }),
    }),
  });
  const pager = ui.createCursorPager((cursor) => ctx.api.get('/admin/users', { query: { q, cursor, limit: PAGE_SIZE }, signal: ctx.signal }));
  const controls = ui.pagerControls(pager, (items) => table.setRows(items));

  async function reload() {
    table.setLoading(true, 6);
    try {
      table.setRows(await pager.first());
    } catch (err) {
      if (err?.name === 'AbortError') return;
      table.setRows([]);
      ui.toast.error(err);
    }
    controls.update();
  }

  const onSearch = debounce(() => {
    q = search.value.trim();
    ctx.router.setQuery({ q: q || null });
    reload();
  }, 250);
  search.addEventListener('input', onSearch);

  function replaceRow(u) {
    table.setRows(table.getRows().map((r) => (r.id === u.id ? { ...r, ...u } : r)));
    if (u.id === ctx.user?.id) setUser({ ...ctx.user, ...u });
  }

  function rowMenu(u) {
    const self = u.id === ctx.user?.id;
    return [
      { label: 'Edit user', icon: 'edit', onSelect: () => openUser(u.id) },
      { label: 'Reset password…', icon: 'key', onSelect: () => resetPassword(u) },
      { label: 'Sign out everywhere', icon: 'log-out', onSelect: () => revokeSessions(u) },
      u.totp_enabled ? { label: 'Remove two-factor', icon: 'shield', onSelect: () => removeTotp(u) } : null,
      { separator: true },
      { label: u.disabled ? 'Enable account' : 'Disable account', icon: u.disabled ? 'check-circle' : 'ban', disabled: self, onSelect: () => toggleDisabled(u) },
      { label: 'Delete user…', icon: 'trash', danger: true, disabled: self, onSelect: () => deleteUser(u) },
    ];
  }

  /* ------------------------------------------------------------- actions */

  async function toggleDisabled(u) {
    try {
      replaceRow(await ctx.api.patch(`/admin/users/${encodeURIComponent(u.id)}`, { disabled: !u.disabled }));
      ui.toast.success(u.disabled ? `${u.username} can sign in again` : `${u.username} is disabled`);
    } catch (err) {
      ui.toast.error(err);
    }
  }

  async function revokeSessions(u) {
    const ok = await ui.confirm({ title: `Sign ${u.username} out everywhere?`, message: 'All of their sessions and app logins end immediately. Personal API tokens keep working.', confirmLabel: 'Sign out', danger: true });
    if (!ok) return;
    try {
      await ctx.api.post(`/admin/users/${encodeURIComponent(u.id)}/revoke-sessions`, {});
      ui.toast.success('Sessions revoked');
    } catch (err) {
      ui.toast.error(err);
    }
  }

  async function removeTotp(u) {
    const ok = await ui.confirm({ title: 'Remove two-factor authentication?', message: `${u.username} will be able to sign in with just their password until they set it up again.`, confirmLabel: 'Remove 2FA', danger: true });
    if (!ok) return;
    try {
      await ctx.api.del(`/admin/users/${encodeURIComponent(u.id)}/totp`);
      replaceRow({ ...u, totp_enabled: false });
      ui.toast.success('Two-factor removed');
    } catch (err) {
      ui.toast.error(err);
    }
  }

  function resetPassword(u) {
    const pw = passwordInput({ name: 'new_password', autocomplete: 'new-password' });
    const gen = ui.button({ label: 'Generate', icon: 'refresh', size: 'sm', variant: 'ghost' });
    gen.addEventListener('click', () => {
      pw.inputEl.value = generatePassword(20);
      pw.inputEl.type = 'text';
      pw.inputEl.select();
    });
    const m = ui.modal({
      title: `Reset password for ${u.username}`,
      description: 'They will be signed out everywhere and must use the new password.',
      size: 'sm',
      body: h('div', { class: 'stack' }, field({ label: 'New password', control: pw, name: 'new_password', labelExtra: gen })),
      footer: [{ label: 'Cancel', value: null }, { label: 'Reset password', variant: 'danger', type: 'submit' }],
      onSubmit: async () => {
        clearErrors(m.form);
        if (!pw.inputEl.value) {
          showFieldErrors(m.form, { new_password: 'Enter or generate a password.' });
          return false;
        }
        try {
          await ctx.api.post(`/admin/users/${encodeURIComponent(u.id)}/password`, { new_password: pw.inputEl.value });
          ui.toast.success('Password reset', { description: 'Share the new password with them securely.' });
          return true;
        } catch (err) {
          if (err.fields) showFieldErrors(m.form, err.fields);
          else showFormError(m.form, err, { bannerHost: m.body });
          return false;
        }
      },
    });
  }

  async function deleteUser(u) {
    const alsoFiles = checkbox({ label: 'Also delete their files', hint: 'Permanently removes their home folder. Leave unchecked to keep files on disk for an administrator to recover.' });
    const ok = await ui.confirm({
      title: `Delete ${u.username}?`,
      message: 'Their account, sessions, tokens, notes, vault and settings are removed. This can’t be undone.',
      body: alsoFiles, requireText: u.username, confirmLabel: 'Delete user', danger: true,
    });
    if (!ok) return;
    try {
      await ctx.api.del(`/admin/users/${encodeURIComponent(u.id)}`, { query: { delete_files: alsoFiles.inputEl.checked ? 'true' : 'false' } });
      ui.toast.success(`${u.username} deleted`);
      table.setRows(table.getRows().filter((r) => r.id !== u.id));
    } catch (err) {
      ui.toast.error(err);
    }
  }

  function createUser() {
    const username = input({ name: 'username', autocomplete: 'off', autocapitalize: 'off', spellcheck: 'false' });
    const email = input({ name: 'email', type: 'email', autocomplete: 'off' });
    const dname = input({ name: 'display_name', autocomplete: 'off' });
    const pw = passwordInput({ name: 'password', autocomplete: 'new-password' });
    const role = select({ name: 'role', options: [{ value: 'user', label: 'User' }, { value: 'admin', label: 'Administrator' }], value: 'user' });
    const quota = quotaInput(0);
    let generated = false;
    const gen = ui.button({ label: 'Generate', icon: 'refresh', size: 'sm', variant: 'ghost' });
    gen.addEventListener('click', () => {
      pw.inputEl.value = generatePassword(20);
      pw.inputEl.type = 'text';
      generated = true;
    });
    pw.inputEl.addEventListener('input', () => { generated = false; });

    const m = ui.modal({
      title: 'New user', size: 'lg',
      body: h('div', { class: 'form-grid cols-2' },
        field({ label: 'Username', control: username, name: 'username' }),
        field({ label: 'Email', control: email, name: 'email' }),
        field({ label: 'Display name', control: dname, name: 'display_name', hint: 'Optional' }),
        field({ label: 'Role', control: role, name: 'role' }),
        field({ label: 'Password', control: pw, name: 'password', labelExtra: gen, hint: 'Share it securely; they can change it in Settings.' }),
        field({ label: 'Storage quota', control: quota, name: 'quota_bytes', hint: '0 = unlimited' })),
      footer: [{ label: 'Cancel', value: null }, { label: 'Create user', variant: 'primary', type: 'submit' }],
      onSubmit: async () => {
        clearErrors(m.form);
        const body = {
          username: username.value.trim(), email: email.value.trim(), password: pw.inputEl.value,
          role: role.value, quota_bytes: readQuota(quota),
        };
        if (dname.value.trim()) body.display_name = dname.value.trim();
        const p = {};
        if (!body.username) p.username = 'Required.';
        if (!body.email || !email.checkValidity()) p.email = 'Enter a valid email address.';
        if (!body.password) p.password = 'Enter or generate a password.';
        if (Number.isNaN(body.quota_bytes)) p.quota_bytes = 'Use a size like 500 GiB, or 0 for unlimited.';
        if (Object.keys(p).length) {
          showFieldErrors(m.form, p);
          return false;
        }
        try {
          const created = await ctx.api.post('/admin/users', body);
          ui.toast.success(`${created.username || body.username} created`);
          reload();
          if (generated) showCredentials(body.username, body.password);
          return true;
        } catch (err) {
          if (err.fields) showFieldErrors(m.form, err.fields);
          else showFormError(m.form, err, { bannerHost: m.body });
          return false;
        }
      },
    });
  }

  function showCredentials(username, password) {
    ui.modal({
      title: 'Account created',
      description: 'Send these details over a secure channel. The password isn’t stored anywhere you can see again.',
      size: 'sm',
      body: h('div', { class: 'stack-sm' },
        h('div', { class: 'secret-box' }, h('span', { class: 'secret-text' }, `${location.origin}\n${username}`)),
        h('div', { class: 'secret-box accent' }, h('span', { class: 'secret-text' }, password), ui.copyButton(password, { title: 'Copy password' }))),
      footer: [{ label: 'Done', variant: 'primary', value: true }],
    });
  }

  /* --------------------------------------------------------------- drawer */

  async function openUser(id) {
    let u;
    const d = ui.drawer({ title: 'User', size: 'lg', body: ui.loadingBlock() });
    d.closed.then(() => {
      if (new URLSearchParams(location.search).has('user')) ctx.router.setQuery({ user: null });
    });
    try {
      u = await ctx.api.get(`/admin/users/${encodeURIComponent(id)}`);
    } catch (err) {
      d.body.replaceChildren(ui.errorState(err));
      return;
    }
    const self = u.id === ctx.user?.id;
    const original = {
      display_name: u.display_name || '', email: u.email || '', role: u.role,
      quota_bytes: Number(u.quota_bytes) || 0, disabled: !!u.disabled, email_verified: !!u.email_verified,
    };
    const dname = input({ name: 'display_name', value: original.display_name });
    const email = input({ name: 'email', type: 'email', value: original.email });
    const role = select({ name: 'role', options: [{ value: 'user', label: 'User' }, { value: 'admin', label: 'Administrator' }], value: original.role, disabled: self });
    const quota = quotaInput(original.quota_bytes);
    const disabled = switchInput({ checked: original.disabled, label: 'Account disabled', disabled: self });
    const verified = switchInput({ checked: original.email_verified, label: 'Email verified' });
    const save = ui.button({ label: 'Save changes', variant: 'primary', type: 'submit', disabled: true });

    const used = Number(u.usage_bytes) || 0;
    const usage = h('div', { class: 'usage-summary' },
      h('div', { class: 'usage-numbers' }, h('span', { class: 'bold' }, formatBytes(used)),
        h('span', { class: 'usage-of' }, original.quota_bytes ? `of ${formatBytes(original.quota_bytes)} · ${formatPercent(used, original.quota_bytes)}` : 'used · no quota')),
      original.quota_bytes ? ui.progress({ value: used, max: original.quota_bytes, variant: usageVariant(used / original.quota_bytes) }).el : null);

    const form = h('form', { class: 'stack', novalidate: true },
      h('div', { class: 'row gap-3' }, ui.avatar({ name: displayName(u), size: 44 }),
        h('div', { class: 'cell-stack' }, h('span', { class: 'cell-title' }, displayName(u)), h('span', { class: 'cell-sub' }, `@${u.username} · joined ${formatDate(u.created_at)}`)),
        h('span', { class: 'spacer' }), statusBadges(ui, u)),
      usage,
      h('div', { class: 'form-grid cols-2' },
        field({ label: 'Display name', control: dname, name: 'display_name' }),
        field({ label: 'Email', control: email, name: 'email' }),
        field({ label: 'Role', control: role, name: 'role', hint: self ? 'You can’t change your own role.' : undefined }),
        field({ label: 'Storage quota', control: quota, name: 'quota_bytes', hint: '0 = unlimited' })),
      h('div', { class: 'setting-rows' },
        field({ label: 'Account disabled', hint: self ? 'You can’t disable yourself.' : 'Disabled users can’t sign in; their files stay.', control: disabled, name: 'disabled', inline: true }),
        field({ label: 'Email verified', hint: 'Mark the address as confirmed (enables mail-in notes).', control: verified, name: 'email_verified', inline: true })),
      h('div', { class: 'row end' }, save));

    const current = () => ({
      display_name: dname.value.trim(), email: email.value.trim(), role: role.value,
      quota_bytes: readQuota(quota), disabled: disabled.inputEl.checked, email_verified: verified.inputEl.checked,
    });
    const sync = () => { save.disabled = Object.keys(diffObject(original, current())).length === 0; };
    form.addEventListener('input', sync);
    form.addEventListener('change', sync);
    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      clearErrors(form);
      const patch = diffObject(original, current());
      if (Number.isNaN(patch.quota_bytes)) {
        showFieldErrors(form, { quota_bytes: 'Use a size like 500 GiB, or 0 for unlimited.' });
        return;
      }
      if (patch.email !== undefined && !email.checkValidity()) {
        showFieldErrors(form, { email: 'Enter a valid email address.' });
        return;
      }
      if (!Object.keys(patch).length) return;
      await ui.withLoading(save, async () => {
        try {
          const updated = await ctx.api.patch(`/admin/users/${encodeURIComponent(u.id)}`, patch);
          Object.assign(original, {
            display_name: updated.display_name || '', email: updated.email || '', role: updated.role,
            quota_bytes: Number(updated.quota_bytes) || 0, disabled: !!updated.disabled, email_verified: !!updated.email_verified,
          });
          replaceRow(updated);
          sync();
          ui.toast.success('User updated');
        } catch (err) {
          if (err.status === 409) showFormError(form, { message: err.message || 'At least one active administrator must remain.' });
          else showFormError(form, err);
        }
      });
      sync(); // withLoading restores the pre-click disabled state; recompute it
    });

    const actions = ui.card({
      title: 'Account actions',
      body: h('div', { class: 'setting-rows' },
        actionRow('Reset password', 'Set a new password and sign them out everywhere.', ui.button({ label: 'Reset…', size: 'sm', onClick: () => resetPassword(u) })),
        actionRow('Revoke sessions', 'Sign out every browser and app login.', ui.button({ label: 'Revoke', size: 'sm', onClick: () => revokeSessions(u) })),
        u.totp_enabled ? actionRow('Two-factor authentication', 'Remove their authenticator if they lost access to it.', ui.button({ label: 'Remove 2FA', size: 'sm', onClick: async () => { await removeTotp(u); } })) : null),
    });
    const danger = self ? null : ui.card({
      title: 'Delete user', className: 'danger-zone',
      body: h('div', { class: 'row between wrap' }, h('span', { class: 'small muted' }, 'Removes the account and its data. Optionally deletes their files.'),
        ui.button({ label: 'Delete…', variant: 'danger', size: 'sm', onClick: async () => { await deleteUser(u); if (!table.getRows().some((r) => r.id === u.id)) d.close(); } })),
    });
    d.body.replaceChildren(h('div', { class: 'stack-lg' }, form, actions, danger));
  }

  function actionRow(label, desc, control) {
    return h('div', { class: 'setting-row' },
      h('div', { class: 'setting-row-text' }, h('span', { class: 'setting-label' }, label), h('span', { class: 'setting-desc' }, desc)), control);
  }

  host.append(
    h('div', { class: 'toolbar' },
      h('div', { class: 'input-group' }, icon('search', { className: 'input-icon' }), search),
      h('span', { class: 'spacer' }),
      ui.button({ icon: 'refresh', title: 'Refresh', variant: 'ghost', onClick: () => reload() }),
      newBtn),
    table.el,
    controls.el);
  reload();
  if (ctx.query.user) openUser(ctx.query.user);

  let lastQueryUser = ctx.query.user || null;
  return {
    update(c) {
      const want = c.query.user || null;
      if (want && want !== lastQueryUser) openUser(want);
      lastQueryUser = want;
    },
  };
}
