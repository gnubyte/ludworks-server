// Active sessions and token logins: list, mark the current one, revoke.

import { h } from '../../core/dom.js';
import { icon } from '../../core/icons.js';
import { formatDateTime } from '../../core/format.js';

/** Summarise a User-Agent as "Browser · OS". */
export function describeAgent(ua = '') {
  if (!ua) return '';
  const browser = /Edg\//.test(ua) ? 'Edge' : /OPR\//.test(ua) ? 'Opera' : /Firefox\//.test(ua) ? 'Firefox'
    : /Chrome\//.test(ua) ? 'Chrome' : /Safari\//.test(ua) && /Version\//.test(ua) ? 'Safari'
      : /python-requests|python/i.test(ua) ? 'Python' : /okhttp|Dalvik/i.test(ua) ? 'Android app'
        : /curl/i.test(ua) ? 'curl' : /Ludworks/i.test(ua) ? 'Ludworks app' : '';
  const os = /Windows/.test(ua) ? 'Windows' : /Android/.test(ua) ? 'Android' : /iPhone|iPad/.test(ua) ? 'iOS'
    : /Mac OS X/.test(ua) ? 'macOS' : /Linux/.test(ua) ? 'Linux' : '';
  return [browser, os].filter(Boolean).join(' · ') || ua.slice(0, 60);
}

function deviceIcon(s) {
  if (s.kind === 'token') return 'key';
  return /Android|iPhone|iPad|Mobile/.test(s.user_agent || '') ? 'smartphone' : 'laptop';
}

/**
 * Rows "Sign out others" revokes: every other sign-in session (browsers and app logins made with
 * token_mode, both kind "session"). Personal API tokens (kind "token") are listed too but are
 * managed on the API tokens page, so a bulk sign-out must never revoke them.
 */
export function otherSignIns(rows) {
  return (rows || []).filter((s) => !s.current && s.kind !== 'token');
}

export function render(host, ctx) {
  const { ui } = ctx;
  let rows = [];

  const revokeOthers = ui.button({ label: 'Sign out others', title: 'Sign out all other sessions', icon: 'log-out', size: 'sm', onClick: () => revokeAllOthers() });
  const table = ui.dataTable({
    caption: 'Your sessions',
    rowKey: 'id',
    sort: { key: 'last_seen_at', dir: 'desc' },
    columns: [
      {
        key: 'device_name', label: 'Device', sortable: true, width: '40%',
        sortValue: (s) => s.device_name || describeAgent(s.user_agent),
        render: (s) => h('div', { class: 'cell-main' },
          h('span', { class: 'session-icon' }, icon(deviceIcon(s))),
          h('div', { class: 'cell-stack' },
            h('span', { class: 'cell-title row wrap' }, h('span', {}, s.device_name || describeAgent(s.user_agent) || 'Unknown device'),
              s.current ? ui.badge('This device', 'accent') : null,
              s.kind === 'token' ? ui.badge('API token', 'neutral') : null),
            h('span', { class: 'cell-sub' }, describeAgent(s.user_agent)))),
      },
      { key: 'ip', label: 'IP address', hideBelow: 'sm', render: (s) => h('span', { class: 'mono small' }, s.ip || '—') },
      { key: 'last_seen_at', label: 'Last active', sortable: true, render: (s) => ui.timeAgo(s.last_seen_at) },
      { key: 'created_at', label: 'Signed in', sortable: true, hideBelow: 'md', render: (s) => h('span', { class: 'small muted nowrap' }, formatDateTime(s.created_at)) },
      { key: 'expires_at', label: 'Expires', hideBelow: 'md', render: (s) => ui.timeAgo(s.expires_at) },
      {
        key: 'actions', label: '', align: 'right',
        render: (s) => ui.button({
          label: s.current ? 'Sign out' : 'Revoke', size: 'sm', variant: s.current ? 'secondary' : 'danger-ghost',
          onClick: () => revoke(s),
        }),
      },
    ],
    empty: () => ui.emptyState({ icon: 'laptop', title: 'No active sessions', compact: true }),
  });

  async function load() {
    table.setLoading(true, 3);
    try {
      const res = await ctx.api.get('/me/sessions', { signal: ctx.signal });
      rows = Array.isArray(res) ? res : res?.items || [];
      table.setRows(rows);
      revokeOthers.disabled = otherSignIns(rows).length === 0;
    } catch (err) {
      if (err?.name === 'AbortError') return;
      table.setRows([]);
      ui.toast.error(err);
    }
  }

  async function revoke(s) {
    if (s.current) {
      await ctx.actions.signOut();
      return;
    }
    const isToken = s.kind === 'token';
    const ok = await ui.confirm({
      title: isToken ? `Revoke the API token “${s.device_name || 'token'}”?` : 'Revoke this session?',
      message: isToken
        ? 'Scripts and apps using this token lose access immediately. This can’t be undone.'
        : `${s.device_name || describeAgent(s.user_agent) || 'This device'} will be signed out immediately.`,
      confirmLabel: 'Revoke', danger: true,
    });
    if (!ok) return;
    try {
      await ctx.api.del(`/me/sessions/${encodeURIComponent(s.id)}`);
      ui.toast.success(isToken ? 'Token revoked' : 'Session revoked');
      rows = rows.filter((r) => r.id !== s.id);
      table.setRows(rows);
      revokeOthers.disabled = otherSignIns(rows).length === 0;
    } catch (err) {
      ui.toast.error(err);
    }
  }

  async function revokeAllOthers() {
    const others = otherSignIns(rows);
    if (!others.length) return;
    const ok = await ui.confirm({
      title: 'Sign out everywhere else?',
      message: `${others.length} other ${others.length === 1 ? 'session' : 'sessions'} will be signed out. Apps signed in with your password are included; personal API tokens keep working (revoke them on the API tokens page).`,
      confirmLabel: 'Sign out others', danger: true,
    });
    if (!ok) return;
    const results = await Promise.allSettled(others.map((s) => ctx.api.del(`/me/sessions/${encodeURIComponent(s.id)}`)));
    const failed = results.filter((r) => r.status === 'rejected').length;
    if (failed) ui.toast.warning(`${others.length - failed} signed out, ${failed} failed`);
    else ui.toast.success('Other sessions signed out');
    load();
  }

  host.append(
    h('div', { class: 'settings-section-header' },
      h('h2', { class: 'settings-section-title' }, 'Sessions'),
      h('p', { class: 'settings-section-desc' }, 'Browsers and apps currently signed in to your account.')),
    ui.card({
      title: 'Where you’re signed in', icon: 'laptop',
      actions: [ui.button({ icon: 'refresh', title: 'Refresh', size: 'sm', variant: 'ghost', onClick: load }), revokeOthers],
      body: table.el, flush: true,
    }));
  load();
}
