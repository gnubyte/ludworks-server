// Home dashboard: greeting, quick actions, app tiles, storage summary and
// "connect your apps" pointers (API docs, SDK, TLS fingerprint).

import { h, mount as fill } from '../../core/dom.js';
import { icon } from '../../core/icons.js';
import { formatBytes, formatPercent, formatFingerprint } from '../../core/format.js';
import { displayName } from '../../core/store.js';
import { categorySegments, stackedBar, legend, usageVariant } from '../../core/charts.js';
import { openPalette } from '../../core/palette.js';
import { comboLabel } from '../../core/shortcuts.js';
import { visibleApps } from '../registry.js';

function greeting(date = new Date()) {
  const hr = date.getHours();
  if (hr < 5) return 'Good evening';
  if (hr < 12) return 'Good morning';
  if (hr < 18) return 'Good afternoon';
  return 'Good evening';
}

export function mount(container, ctx) {
  const { ui } = ctx;
  ctx.setTitle('Home');
  const user = ctx.user;
  const name = (displayName(user) || '').split(' ')[0];
  const today = new Intl.DateTimeFormat(undefined, { weekday: 'long', month: 'long', day: 'numeric' }).format(new Date());

  const quick = h('div', { class: 'quick-actions' },
    ui.button({ label: 'Upload files', icon: 'upload', variant: 'primary', onClick: () => ctx.actions.uploadToHome() }),
    ui.button({ label: 'New note', icon: 'edit', href: '/notes?new=1' }),
    ui.button({ label: 'TV guide', icon: 'tv', href: '/tv/guide' }),
    ui.button({ icon: 'command', title: `Command palette (${comboLabel('mod+k')})`, variant: 'ghost', onClick: () => openPalette() }));

  const tiles = h('div', { class: 'tiles' }, visibleApps(user).filter((a) => a.id !== 'home').map((a) => h('a', { class: `tile tint-${a.tint}`, href: a.path },
    h('div', { class: 'tile-top' }, h('span', { class: 'tile-icon' }, icon(a.icon, { size: 20 })), icon('arrow-right', { size: 16, className: 'tile-arrow' })),
    h('div', {}, h('div', { class: 'tile-title' }, a.title), h('div', { class: 'tile-desc' }, a.description)))));

  const storageBody = h('div', { class: 'usage-summary' }, ui.skeletonText(4));
  const storageCard = ui.card({
    title: 'Your storage', icon: 'storage',
    actions: ui.button({ label: 'Details', size: 'sm', variant: 'ghost', iconRight: 'arrow-right', href: '/system' }),
    body: storageBody,
  });

  const fp = ctx.settings.tls_fingerprint_sha256;
  const connect = ui.card({
    title: 'Connect your apps', icon: 'zap',
    description: 'Desktop, Android, browser extensions and scripts all use the same API.',
    body: h('div', { class: 'connect-list' },
      h('div', { class: 'connect-item' }, h('span', { class: 'connect-item-icon' }, icon('globe')),
        h('div', { class: 'cell-stack' }, h('span', { class: 'cell-title' }, 'Server address'), h('span', { class: 'cell-sub mono break' }, location.origin)),
        h('span', { class: 'spacer' }), ui.copyButton(location.origin, { title: 'Copy server address' })),
      fp ? h('div', { class: 'connect-item' }, h('span', { class: 'connect-item-icon' }, icon('fingerprint')),
        h('div', { class: 'cell-stack' }, h('span', { class: 'cell-title' }, 'TLS fingerprint'), h('span', { class: 'cell-sub mono break' }, `${formatFingerprint(fp).slice(0, 23)}…`)),
        h('span', { class: 'spacer' }), ui.copyButton(formatFingerprint(fp), { title: 'Copy SHA-256 fingerprint' })) : null,
      h('div', { class: 'connect-item' }, h('span', { class: 'connect-item-icon' }, icon('terminal')),
        h('div', { class: 'cell-stack' }, h('span', { class: 'cell-title' }, 'Python SDK'), h('span', { class: 'cell-sub mono' }, 'pip install ludworks')),
        h('span', { class: 'spacer' }), ui.copyButton('pip install ludworks', { title: 'Copy command' })),
      h('div', { class: 'row wrap mt-2' },
        ui.button({ label: 'API reference', icon: 'code', size: 'sm', href: '/api/docs', target: '_blank' }),
        ui.button({ label: 'Create API token', icon: 'key', size: 'sm', variant: 'ghost', href: '/settings/tokens' }))),
  });

  container.append(h('div', { class: 'page wide' },
    h('div', { class: 'home-hero' },
      h('div', {}, h('h1', { class: 'home-greeting' }, `${greeting()}${name ? `, ${name}` : ''}`), h('p', { class: 'home-sub' }, today)),
      quick),
    h('div', { class: 'home-grid' },
      h('section', { class: 'stack', aria: { label: 'Apps' } }, tiles),
      h('div', { class: 'stack' }, storageCard, connect))));

  let loading = null;
  let again = false;
  const loadStorage = async () => {
    // One request at a time; a completion during a load triggers one more afterwards.
    if (loading) { again = true; return loading; }
    loading = renderStorage().finally(() => {
      loading = null;
      if (again && !ctx.signal?.aborted) { again = false; loadStorage(); }
    });
    return loading;
  };
  // Uploads started here (or anywhere in the shell) change the totals: refresh once they
  // settle instead of showing the old usage until the next visit.
  let timer = 0;
  const off = ctx.uploads?.on?.('complete', () => {
    clearTimeout(timer);
    timer = setTimeout(() => { if (!ctx.signal?.aborted) loadStorage(); }, 400);
  });
  ctx.onCleanup?.(() => { clearTimeout(timer); off?.(); });
  loadStorage();

  async function renderStorage() {
    try {
      const st = await ctx.api.get('/system/storage', { signal: ctx.signal });
      const u = st?.user || {};
      const used = Number(u.usage_bytes) || 0;
      const quota = Number(u.quota_bytes) || 0;
      const segs = categorySegments(u.by_category, { trashBytes: Number(u.trash_bytes) || 0 });
      const frac = quota > 0 ? used / quota : 0;
      const bar = ui.progress({ value: used, max: quota || 1, variant: usageVariant(frac), size: 'lg', label: 'Storage used' });
      fill(storageBody,
        h('div', { class: 'usage-numbers' },
          h('span', { class: 'usage-big' }, formatBytes(used)),
          h('span', { class: 'usage-of' }, quota > 0 ? `of ${formatBytes(quota)} · ${formatPercent(used, quota)}` : 'used · no quota')),
        quota > 0 ? bar.el : stackedBar(segs, used),
        segs.length ? legend(segs.slice(0, 5)) : h('p', { class: 'muted small' }, 'Nothing stored yet — upload something to get started.'),
        quota > 0 && frac >= 0.85 ? ui.alert({ variant: frac >= 0.95 ? 'danger' : 'warning', message: 'You are close to your storage quota. Empty the trash or ask an administrator for more space.' }) : null);
    } catch (err) {
      if (err?.name === 'AbortError') return;
      storageBody.replaceChildren(h('p', { class: 'muted small' }, `Storage details are unavailable: ${ui.errorMessage(err)}`));
    }
  }
}
