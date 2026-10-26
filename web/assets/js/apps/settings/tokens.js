// Personal access tokens: create with scopes + expiry (shown once), list, revoke.

import { h } from '../../core/dom.js';
import { icon } from '../../core/icons.js';
import { field, input, select, checkbox, clearErrors, showFieldErrors, showFormError } from '../../core/forms.js';
import { formatDate } from '../../core/format.js';

/** Scope catalogue (API.md §0). */
export const SCOPE_GROUPS = [
  { label: 'Files', scopes: [['files:read', 'Read files'], ['files:write', 'Upload & modify files']] },
  { label: 'Media library', scopes: [['media:read', 'Read media & stream'], ['media:write', 'Edit metadata']] },
  { label: 'TV', scopes: [['tv:read', 'Watch channels & guide'], ['tv:write', 'Manage lineups & channels'], ['tv:feed', 'Feeds & streams only (IPTV players)']] },
  { label: 'Notes', scopes: [['notes:read', 'Read notes'], ['notes:write', 'Create & edit notes']] },
  { label: 'Passwords', scopes: [['vault:read', 'Sync encrypted vault'], ['vault:write', 'Modify encrypted vault']] },
  { label: 'Sheets', scopes: [['sheets:read', 'Read workbooks'], ['sheets:write', 'Edit workbooks']] },
  { label: 'Account', scopes: [['account', 'Manage profile, sessions & tokens']] },
  { label: 'Administration', scopes: [['admin', 'Full server administration']], adminOnly: true },
];

const EXPIRY = [
  { value: '7', label: '7 days' }, { value: '30', label: '30 days' }, { value: '90', label: '90 days' },
  { value: '365', label: '1 year' }, { value: 'never', label: 'No expiry' },
];

export function render(host, ctx) {
  const { ui } = ctx;
  let rows = [];

  const table = ui.dataTable({
    caption: 'API tokens',
    rowKey: 'id',
    sort: { key: 'created_at', dir: 'desc' },
    columns: [
      {
        key: 'name', label: 'Name', sortable: true,
        render: (t) => h('div', { class: 'cell-main' },
          h('span', { class: 'token-icon' }, icon('key')),
          h('div', { class: 'cell-stack' }, h('span', { class: 'cell-title' }, t.name), h('span', { class: 'cell-sub mono' }, `${t.prefix || 'lwp_'}…`))),
      },
      {
        key: 'scopes', label: 'Scopes', hideBelow: 'sm',
        render: (t) => {
          const list = t.scopes || [];
          return h('div', { class: 'chips' }, list.slice(0, 3).map((s) => h('span', { class: 'badge' }, s)),
            list.length > 3 ? h('span', { class: 'badge badge-accent', dataset: { tip: list.slice(3).join(', ') } }, `+${list.length - 3}`) : null);
        },
      },
      { key: 'last_used_at', label: 'Last used', sortable: true, hideBelow: 'md', render: (t) => (t.last_used_at ? ui.timeAgo(t.last_used_at) : h('span', { class: 'faint' }, 'Never')) },
      {
        key: 'expires_at', label: 'Expires', sortable: true,
        render: (t) => {
          if (!t.expires_at) return h('span', { class: 'muted small' }, 'Never');
          if (Date.parse(t.expires_at) < Date.now()) return ui.badge('Expired', 'danger');
          return h('span', { class: 'small', dataset: { tip: formatDate(t.expires_at) } }, formatDate(t.expires_at));
        },
      },
      { key: 'actions', label: '', align: 'right', render: (t) => ui.button({ label: 'Revoke', size: 'sm', variant: 'danger-ghost', onClick: () => revoke(t) }) },
    ],
    empty: () => ui.emptyState({
      icon: 'key', title: 'No API tokens yet', compact: true,
      message: 'Create a token to use the Python SDK, scripts or other integrations.',
      action: ui.button({ label: 'New token', icon: 'plus', variant: 'primary', size: 'sm', onClick: create }),
    }),
  });

  async function load() {
    table.setLoading(true, 3);
    try {
      const res = await ctx.api.get('/me/tokens', { signal: ctx.signal });
      rows = Array.isArray(res) ? res : res?.items || [];
      table.setRows(rows);
    } catch (err) {
      if (err?.name === 'AbortError') return;
      table.setRows([]);
      ui.toast.error(err);
    }
  }

  async function revoke(t) {
    const ok = await ui.confirm({
      title: `Revoke “${t.name}”?`,
      message: 'Anything using this token will immediately lose access. This can’t be undone.',
      confirmLabel: 'Revoke token', danger: true,
    });
    if (!ok) return;
    try {
      await ctx.api.del(`/me/tokens/${encodeURIComponent(t.id)}`);
      rows = rows.filter((r) => r.id !== t.id);
      table.setRows(rows);
      ui.toast.success('Token revoked');
    } catch (err) {
      ui.toast.error(err);
    }
  }

  function create() {
    const isAdmin = ctx.isAdmin;
    const name = input({ name: 'name', placeholder: 'e.g. Backup script on NAS', maxlength: '100', autocomplete: 'off' });
    const expiry = select({ name: 'expires_in_days', options: EXPIRY, value: '90' });
    const boxes = new Map();
    const groups = SCOPE_GROUPS.filter((g) => !g.adminOnly || isAdmin);
    const scopeGrid = h('div', { class: 'scope-grid' }, groups.flatMap((g) => g.scopes.map(([scope, label]) => {
      const cb = checkbox({ label: `${g.label}: ${label}`, hint: scope, name: scope });
      boxes.set(scope, cb.inputEl);
      return cb;
    })));
    const preset = (fn) => () => { for (const [scope, el] of boxes) el.checked = fn(scope); };
    const presets = h('div', { class: 'scope-presets' },
      ui.button({ label: 'Read-only', size: 'sm', variant: 'ghost', onClick: preset((s) => s.endsWith(':read')) }),
      ui.button({ label: 'Read & write', size: 'sm', variant: 'ghost', onClick: preset((s) => s.includes(':')) }),
      ui.button({ label: 'Clear', size: 'sm', variant: 'ghost', onClick: preset(() => false) }));
    const scopeField = h('div', { class: 'field', dataset: { field: 'scopes' } },
      h('div', { class: 'row between wrap' }, h('span', { class: 'field-label' }, 'Scopes'), presets),
      scopeGrid,
      h('div', { class: 'field-hint' }, 'Grant only what the integration needs. Tokens never get the admin scope unless you select it.'),
      h('div', { class: 'field-error', role: 'alert', hidden: true }));

    const m = ui.modal({
      title: 'New API token',
      description: 'Tokens authenticate scripts and apps as you, limited to the scopes you choose.',
      size: 'lg',
      body: h('div', { class: 'stack' },
        h('div', { class: 'form-grid cols-2' },
          field({ label: 'Name', control: name, name: 'name' }),
          field({ label: 'Expiration', control: expiry, name: 'expires_in_days' })),
        scopeField),
      footer: [{ label: 'Cancel', value: null }, { label: 'Create token', variant: 'primary', type: 'submit' }],
      onSubmit: async () => {
        clearErrors(m.form);
        const scopes = [...boxes].filter(([, el]) => el.checked).map(([s]) => s);
        const problems = {};
        if (!name.value.trim()) problems.name = 'Give the token a name you’ll recognise.';
        if (!scopes.length) problems.scopes = 'Select at least one scope.';
        if (Object.keys(problems).length) {
          showFieldErrors(m.form, problems);
          return false;
        }
        const body = { name: name.value.trim(), scopes };
        if (expiry.value !== 'never') body.expires_in_days = Number(expiry.value);
        try {
          const res = await ctx.api.post('/me/tokens', body);
          m.close(true);
          revealToken(res);
          load();
          return true;
        } catch (err) {
          if (err.fields) showFieldErrors(m.form, err.fields);
          else showFormError(m.form, err, { bannerHost: m.body });
          return false;
        }
      },
    });
    preset((s) => s.endsWith(':read'))();
  }

  function revealToken(res) {
    ui.modal({
      title: 'Copy your new token',
      description: 'This is the only time the token is shown. Store it in a password manager or secret store.',
      dismissible: false,
      body: h('div', { class: 'stack' },
        h('div', { class: 'secret-box accent' }, h('span', { class: 'secret-text' }, res.token), ui.copyButton(res.token, { title: 'Copy token', message: 'Token copied' })),
        ui.alert({ variant: 'warning', message: 'Treat this like a password. Anyone with it can act as you within its scopes.' }),
        h('div', { class: 'small muted' }, 'Use it as ', h('code', {}, 'Authorization: Bearer <token>'), ' — see the ', h('a', { href: '/api/docs', target: '_blank', rel: 'noopener' }, 'API reference'), '.')),
      footer: [{ label: 'Done', variant: 'primary', value: true }],
    });
  }

  host.append(
    h('div', { class: 'settings-section-header' },
      h('h2', { class: 'settings-section-title' }, 'API tokens'),
      h('p', { class: 'settings-section-desc' }, 'Personal access tokens for the Python SDK, scripts and integrations.')),
    ui.card({
      title: 'Your tokens', icon: 'key',
      actions: ui.button({ label: 'New token', icon: 'plus', variant: 'primary', size: 'sm', onClick: create }),
      body: table.el, flush: true,
    }));
  load();
}
