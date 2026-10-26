// Storage & system: everyone sees their own usage (quota, by category, spaces);
// admins also get volumes, per-user usage, pending uploads, server info and a
// database backup action.

import { h, cls } from '../../core/dom.js';
import { icon } from '../../core/icons.js';
import { formatBytes, formatPercent, formatNumber, formatDurationLong, formatDateTime, formatFingerprint, formatDate } from '../../core/format.js';
import { categorySegments, donutChart, legend, usageVariant } from '../../core/charts.js';
import { volumeUsage, worstVolumeFraction, usageLevel, volumeTitle, sortSpaces, usersTotal, quotaState, CRITICAL_AT } from './model.js';

function volumeCard(ui, v) {
  const { total, free, used, fraction } = volumeUsage(v);
  const level = usageLevel(fraction);
  const title = volumeTitle(v.label);
  const bar = ui.progress({ value: used, max: total || 1, size: 'lg', variant: usageVariant(fraction), label: `${title} usage` });
  return h('div', { class: cls('card volume', { 'is-warning': level === 'warning', 'is-danger': level === 'danger' }) },
    h('div', { class: 'volume-head' },
      h('span', { class: 'volume-icon' }, icon('storage')),
      h('div', { class: 'cell-stack' }, h('span', { class: 'volume-name', title }, title), h('span', { class: 'volume-path', title: v.path }, v.path)),
      h('span', { class: 'spacer' }),
      level === 'danger' ? ui.badge('Critical', 'danger', { icon: 'alert' }) : level === 'warning' ? ui.badge('Low space', 'warning', { icon: 'alert' }) : ui.badge(formatPercent(used, total), 'neutral')),
    bar.el,
    h('div', { class: 'volume-stats' },
      h('span', {}, `${formatBytes(used)} used`),
      h('span', {}, `${formatBytes(free)} free of ${formatBytes(total)}`)));
}

export function mount(container, ctx) {
  const { ui } = ctx;
  ctx.setTitle('Storage');
  const admin = ctx.isAdmin;
  const body = h('div', { class: 'stack-lg' });
  const refresh = ui.button({ icon: 'refresh', title: 'Refresh', variant: 'ghost', onClick: () => load() });
  ctx.setActions([refresh]);

  container.append(h('div', { class: 'page' },
    ui.pageHeader({ title: 'Storage', subtitle: admin ? 'Usage across your server, its disks and users.' : 'How much space you’re using and where.' }),
    body));

  function renderSkeleton() {
    body.replaceChildren(h('div', { class: 'grid-2' }, ...ui.skeletonCards(2, '180px')));
  }

  function mine(st) {
    const u = st.user || {};
    const q = quotaState(u);
    const trash = Number(u.trash_bytes) || 0;
    const segs = categorySegments(u.by_category, { trashBytes: trash });
    const donut = donutChart(segs, { centerValue: formatBytes(q.used), centerLabel: q.quota > 0 ? `of ${formatBytes(q.quota)}` : 'used', ariaLabel: 'Storage by category' });
    const usage = ui.card({
      title: 'Your usage', icon: 'pie',
      body: h('div', { class: 'stack' },
        h('div', { class: 'donut-layout' }, donut, segs.length ? legend(segs) : h('p', { class: 'muted' }, 'You haven’t stored anything yet.')),
        q.quota > 0 ? h('div', { class: 'usage-summary' },
          h('div', { class: 'usage-numbers' }, h('span', { class: 'bold' }, 'Quota'), h('span', { class: 'usage-of' }, `${formatBytes(q.used)} of ${formatBytes(q.quota)} · ${formatBytes(q.left)} left`)),
          ui.progress({ value: Math.min(q.used, q.quota), max: q.quota, size: 'lg', variant: usageVariant(q.fraction), label: 'Quota used' }).el) : h('p', { class: 'small muted' }, 'No storage quota applies to your account.'),
        q.level !== 'ok' ? ui.alert({ variant: q.level, title: q.fraction >= CRITICAL_AT ? 'Almost out of space' : 'Running low on space', message: 'Delete files you no longer need, or ask an administrator to raise your quota.' }) : null,
        trash > 0 ? h('div', { class: 'row between wrap' }, h('span', { class: 'small muted' }, `${formatBytes(trash)} is in your trash. Emptying it frees the space on disk.`), ui.button({ label: 'Open trash', icon: 'trash', size: 'sm', href: '/files/trash' })) : null),
    });

    const spaces = sortSpaces(st.spaces);
    const maxSpace = Math.max(1, ...spaces.map((s) => Number(s.usage_bytes) || 0));
    const spacesCard = ui.card({
      title: 'Spaces', icon: 'folder', description: 'Your home folder and the shared libraries you can open.',
      body: spaces.length ? h('div', { class: 'stack-md' }, spaces.map((s) => {
        const name = s.kind === 'home' ? 'Home' : s.name;
        const bar = ui.progress({ value: Number(s.usage_bytes) || 0, max: maxSpace, size: 'sm', label: `${name} usage` });
        return h('div', { class: 'stack-xs' },
          h('div', { class: 'row between' },
            h('span', { class: 'row' }, icon(s.kind === 'home' ? 'home' : 'library', { size: 14, className: 'faint' }), h('a', { href: `/files?space=${encodeURIComponent(s.id)}`, class: 'bold' }, name)),
            h('span', { class: 'small muted nums' }, `${formatBytes(s.usage_bytes || 0)} · ${formatNumber(s.file_count || 0)} ${Number(s.file_count) === 1 ? 'file' : 'files'}`)),
          bar.el);
      })) : ui.emptyState({ icon: 'folder', title: 'No spaces yet', message: 'Your home folder appears here once you store files.', compact: true }),
    });
    return h('div', { class: 'grid-2' }, usage, spacesCard);
  }

  function adminStorage(st) {
    const vols = st.volumes || [];
    const worst = worstVolumeFraction(vols);
    const users = st.users || [];
    const table = ui.dataTable({
      caption: 'Usage per user', rowKey: 'id', maxHeight: '420px',
      sort: { key: 'usage_bytes', dir: 'desc' },
      columns: [
        { key: 'username', label: 'User', sortable: true, render: (u) => h('div', { class: 'cell-main' }, ui.avatar({ name: u.username, size: 24 }), h('span', { class: 'cell-title' }, u.username)) },
        { key: 'usage_bytes', label: 'Used', sortable: true, align: 'right', render: (u) => formatBytes(u.usage_bytes || 0) },
        { key: 'quota_bytes', label: 'Quota', sortable: true, align: 'right', render: (u) => (u.quota_bytes ? formatBytes(u.quota_bytes) : h('span', { class: 'faint' }, 'Unlimited')) },
        {
          key: 'pct', label: 'Of quota', hideBelow: 'sm',
          sortValue: (u) => (u.quota_bytes ? u.usage_bytes / u.quota_bytes : -1),
          render: (u) => {
            if (!u.quota_bytes) return h('span', { class: 'faint small' }, '—');
            const f = u.usage_bytes / u.quota_bytes;
            return h('div', { class: 'mini-bar' }, ui.progress({ value: Math.min(u.usage_bytes, u.quota_bytes), max: u.quota_bytes, size: 'sm', variant: usageVariant(f) }).el, h('span', {}, formatPercent(u.usage_bytes, u.quota_bytes)));
          },
        },
      ],
      rows: users,
      onRowClick: (u) => ctx.router.navigate(`/admin/users?user=${encodeURIComponent(u.id)}`),
      empty: () => ui.emptyState({ icon: 'users', title: 'No users', compact: true }),
    });
    const level = usageLevel(worst);
    return h('div', { class: 'stack-lg' },
      level !== 'ok' ? ui.alert({ variant: level, title: level === 'danger' ? 'A disk is almost full' : 'A disk is running low', message: 'Uploads fail when free space drops below the size of the upload plus 1 GiB of headroom.' }) : null,
      h('section', { class: 'section' },
        h('div', { class: 'section-header' }, h('h2', { class: 'section-title' }, 'Disks')),
        vols.length ? h('div', { class: 'grid-auto' }, vols.map((v) => volumeCard(ui, v))) : ui.emptyState({ icon: 'storage', title: 'No volume information', message: 'Disk statistics are not available on this platform.', compact: true })),
      h('div', { class: 'stats-row' },
        h('div', { class: 'card stat-card stat' }, h('span', { class: 'stat-label' }, 'Pending uploads'), h('span', { class: 'stat-value' }, formatBytes(st.uploads_pending_bytes || 0)), h('span', { class: 'stat-sub' }, 'Incomplete resumable uploads on disk')),
        h('div', { class: 'card stat-card stat' }, h('span', { class: 'stat-label' }, 'Users'), h('span', { class: 'stat-value' }, formatNumber(users.length)), h('span', { class: 'stat-sub' }, `${formatBytes(usersTotal(users))} stored in total`)),
        h('div', { class: 'card stat-card stat' }, h('span', { class: 'stat-label' }, 'Data directory'),
          h('span', { class: 'row' }, h('span', { class: 'mono small truncate', title: st.data_dir || '' }, st.data_dir || '—'), st.data_dir ? ui.copyButton(st.data_dir, { title: 'Copy path' }) : null),
          h('span', { class: 'stat-sub' }, 'Database, storage, uploads and backups'))),
      h('section', { class: 'section' },
        h('div', { class: 'section-header' }, h('h2', { class: 'section-title' }, 'Usage by user')),
        table.el));
  }

  function infoCard(info) {
    const tls = info.tls || {};
    const ff = info.ffmpeg || {};
    const backupBtn = ui.button({ label: 'Back up database', icon: 'database', size: 'sm' });
    backupBtn.addEventListener('click', () => backup(backupBtn));
    // The actions sit in the footer: in the header they never wrap, so on a phone the two
    // buttons overlapped the "Server" title.
    return ui.card({
      title: 'Server', icon: 'server',
      footer: [ui.button({ label: 'API docs', icon: 'external', size: 'sm', variant: 'ghost', href: '/api/docs', target: '_blank' }), backupBtn],
      body: ui.kv([
        ['Version', h('span', { class: 'row' }, info.version || '—', info.go_version ? h('span', { class: 'faint small' }, `(${info.go_version})`) : null)],
        ['Host', `${info.hostname || '—'} · ${info.os || '?'}/${info.arch || '?'}`],
        ['Uptime', h('span', { dataset: { tip: `Started ${formatDateTime(info.started_at)}` } }, formatDurationLong(info.uptime_seconds || 0, 3))],
        ['Database', `${formatBytes(info.db_size_bytes || 0)} · ${formatNumber(info.user_count || 0)} ${info.user_count === 1 ? 'user' : 'users'} · ${formatNumber(info.file_count || 0)} ${info.file_count === 1 ? 'file' : 'files'}`],
        ['FFmpeg', ff.available ? h('span', { class: 'row' }, ui.badge('Available', 'success', { dot: true }), h('span', { class: 'small muted truncate' }, ff.version || '')) : h('span', { class: 'row wrap' }, ui.badge('Not found', 'warning', { dot: true }), h('span', { class: 'small muted' }, 'Video thumbnails and transcoding are off'))],
        ['TLS', h('span', { class: 'row wrap' }, ui.badge(tls.mode || 'unknown', tls.mode === 'off' ? 'warning' : 'accent'), tls.not_after ? h('span', { class: 'small muted' }, `valid until ${formatDate(tls.not_after)}`) : tls.mode === 'off' ? h('span', { class: 'small muted' }, 'plain HTTP — use only behind a TLS reverse proxy') : null)],
        tls.fingerprint_sha256 ? ['Fingerprint', h('span', { class: 'row top' }, h('span', { class: 'mono xs break' }, formatFingerprint(tls.fingerprint_sha256)), ui.copyButton(formatFingerprint(tls.fingerprint_sha256), { title: 'Copy fingerprint' }))] : null,
      ]),
    });
  }

  async function backup(btn) {
    const ok = await ui.confirm({
      title: 'Back up the database?',
      message: 'A consistent snapshot (SQLite VACUUM INTO) is written to the backups folder in the data directory. File contents are not included.',
      confirmLabel: 'Create backup',
    });
    if (!ok) return;
    await ui.withLoading(btn, async () => {
      try {
        const res = await ctx.api.post('/admin/db-backup', {});
        ui.toast.success('Database backed up', { description: `${res?.path || ''} · ${formatBytes(res?.size || 0)}`, timeout: 9000 });
      } catch (err) {
        ui.toast.error(err);
      }
    });
  }

  let seq = 0;
  async function load() {
    const mine_ = ++seq;
    renderSkeleton();
    const [storage, info] = await Promise.allSettled([
      ctx.api.get('/system/storage', { signal: ctx.signal }),
      admin ? ctx.api.get('/system/info', { signal: ctx.signal }) : Promise.resolve(null),
    ]);
    if (ctx.signal.aborted || mine_ !== seq) return; // unmounted, or a newer refresh is running
    if (storage.status === 'rejected') {
      body.replaceChildren(ui.errorState(storage.reason, () => load()));
      return;
    }
    const st = storage.value || {};
    const parts = [mine(st)];
    if (admin) {
      if (info.status === 'fulfilled' && info.value) parts.push(infoCard(info.value));
      else if (info.status === 'rejected') parts.push(ui.alert({ variant: 'warning', title: 'Server info unavailable', message: ui.errorMessage(info.reason) }));
      parts.push(adminStorage(st));
    }
    body.replaceChildren(...parts);
  }

  ctx.commands?.register?.([{ id: 'system.refresh', title: 'Refresh storage', icon: 'refresh', keywords: ['disk', 'usage', 'quota'], run: () => load() }]);
  load();
  return { update: () => {} };
}
