// Admin › Audit log: filter by action and actor, cursor-paginated ("load more"),
// row details with metadata rendered as text (never as HTML).

import { h, debounce } from '../../core/dom.js';
import { icon } from '../../core/icons.js';
import { input, select } from '../../core/forms.js';
import { formatDateTime } from '../../core/format.js';

const PAGE = 50;

/** Known actions for the filter suggestions (servers may emit more). */
const KNOWN_ACTIONS = [
  'login.ok', 'login.fail', 'logout', 'password.change', 'password.reset', 'totp.enable', 'totp.disable',
  'token.create', 'token.revoke', 'session.revoke', 'user.create', 'user.update', 'user.delete', 'user.password',
  'settings.update', 'space.create', 'space.update', 'space.delete', 'categories.update', 'db.backup', 'setup.complete',
];

function actionVariant(action = '') {
  if (/fail|delete|revoke|disable|denied/.test(action)) return 'danger';
  if (/create|enable|ok|complete/.test(action)) return 'success';
  if (/update|change|reset|settings/.test(action)) return 'info';
  return 'neutral';
}

export function render(host, ctx) {
  const { ui } = ctx;
  let rows = [];
  let cursor = null;
  let done = false;
  let gen = 0;

  const actionInput = input({ type: 'search', placeholder: 'Filter by action…', list: 'lw-audit-actions', className: 'has-icon', 'aria-label': 'Filter by action', value: ctx.query.action || '' });
  const datalist = h('datalist', { id: 'lw-audit-actions' }, KNOWN_ACTIONS.map((a) => h('option', { value: a })));
  const actorSelect = select({ options: [{ value: '', label: 'All actors' }], 'aria-label': 'Filter by actor' });
  const more = ui.button({ label: 'Load more', variant: 'ghost', size: 'sm', onClick: () => loadPage() });
  const moreWrap = h('div', { class: 'infinite-sentinel' }, more);

  const table = ui.dataTable({
    caption: 'Audit events', rowKey: 'id',
    columns: [
      { key: 'time', label: 'When', render: (e) => h('span', { class: 'small', dataset: { tip: formatDateTime(e.time) } }, ui.timeAgo(e.time)) },
      {
        key: 'actor', label: 'Actor',
        render: (e) => (e.actor_username || e.actor_id
          ? h('div', { class: 'cell-main' }, ui.avatar({ name: e.actor_username || e.actor_id, size: 22 }), h('span', { class: 'truncate' }, e.actor_username || e.actor_id))
          : h('span', { class: 'faint small' }, 'system')),
      },
      { key: 'action', label: 'Action', render: (e) => ui.badge(e.action, actionVariant(e.action)) },
      { key: 'target', label: 'Target', hideBelow: 'sm', render: (e) => (e.target_type ? h('span', { class: 'small' }, h('span', { class: 'muted' }, `${e.target_type} `), h('span', { class: 'mono xs' }, e.target_id || '')) : null) },
      { key: 'ip', label: 'IP', hideBelow: 'md', render: (e) => h('span', { class: 'mono xs' }, e.ip || '—') },
    ],
    onRowClick: (e) => details(e),
    empty: () => ui.emptyState({ icon: 'history', title: 'No events', message: 'Nothing matches these filters.', compact: true }),
  });

  function details(e) {
    ui.drawer({
      title: e.action,
      description: formatDateTime(e.time),
      body: h('div', { class: 'stack' },
        ui.kv([
          ['Actor', e.actor_username ? `${e.actor_username} (${e.actor_id})` : e.actor_id || 'system'],
          ['Action', h('span', { class: 'mono' }, e.action)],
          ['Target', e.target_type ? h('span', {}, `${e.target_type} `, h('span', { class: 'mono' }, e.target_id || '')) : '—'],
          ['IP address', h('span', { class: 'mono' }, e.ip || '—')],
          ['Event ID', h('span', { class: 'mono xs' }, e.id)],
        ]),
        h('div', { class: 'stack-sm' }, h('span', { class: 'field-label' }, 'Details'),
          // JSON is rendered as text content: safe for any server-provided strings.
          h('pre', { class: 'audit-meta' }, JSON.stringify(e.meta ?? {}, null, 2)))),
    });
  }

  async function loadPage(reset = false) {
    const my = reset ? ++gen : gen;
    if (reset) {
      rows = [];
      cursor = null;
      done = false;
      table.setLoading(true, 8);
    }
    if (done) return;
    ui.setLoading(more, true);
    try {
      const res = await ctx.api.get('/admin/audit', {
        query: { limit: PAGE, cursor, action: actionInput.value.trim(), actor_id: actorSelect.value },
        signal: ctx.signal,
      });
      if (my !== gen) return;
      rows = rows.concat(res?.items || []);
      cursor = res?.next_cursor || null;
      done = !cursor;
      table.setRows(rows);
    } catch (err) {
      if (err?.name === 'AbortError' || my !== gen) return;
      if (reset) table.setRows([]);
      ui.toast.error(err);
    } finally {
      if (my === gen) {
        ui.setLoading(more, false);
        moreWrap.hidden = done;
      }
    }
  }

  const refilter = debounce(() => {
    ctx.router.setQuery({ action: actionInput.value.trim() || null, actor: actorSelect.value || null });
    loadPage(true);
  }, 300);
  actionInput.addEventListener('input', refilter);
  actorSelect.addEventListener('change', refilter);

  // Populate the actor filter with the first page of users (enough for most installs).
  ctx.api.get('/admin/users', { query: { limit: 200 }, signal: ctx.signal }).then((res) => {
    for (const u of res?.items || []) {
      const existing = [...actorSelect.options].find((o) => o.value === u.id);
      if (existing) existing.textContent = u.username;
      else actorSelect.append(h('option', { value: u.id }, u.username));
    }
  }).catch(() => {});

  host.append(
    h('div', { class: 'toolbar' },
      h('div', { class: 'input-group' }, icon('search', { className: 'input-icon' }), actionInput, datalist),
      actorSelect,
      h('span', { class: 'spacer' }),
      ui.button({ icon: 'refresh', title: 'Refresh', variant: 'ghost', onClick: () => loadPage(true) })),
    table.el,
    moreWrap);
  if (ctx.query.actor) {
    actorSelect.append(h('option', { value: ctx.query.actor }, ctx.query.actor));
    actorSelect.value = ctx.query.actor;
  }
  loadPage(true);
}
