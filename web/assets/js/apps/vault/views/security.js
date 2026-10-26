// Security report, computed entirely on this device from the decrypted vault: weak, reused and
// old passwords, websites saved with insecure http:// URLs, and logins for sites that offer
// two-factor authentication but have no TOTP key stored.

import { h, cls } from '../../../core/dom.js';
import { icon } from '../../../core/icons.js';
import * as ui from '../../../core/ui.js';
import { securityReport, subtitle } from '../model.js';
import { vicon, itemAvatar, STRENGTH_LABELS } from '../widgets.js';

export function render(host, { vault, ctx, openItem }) {
  ctx.setTitle('Security report · Passwords');
  const { session } = vault;
  let active = 'weak';

  function entries() {
    return [...session.items.values()].filter((r) => r.view).map((r) => ({ id: r.id, view: r.view }));
  }

  function draw() {
    const list = entries();
    const byId = new Map(list.map((e) => [e.id, e]));
    const rep = securityReport(list);
    const cats = [
      { id: 'weak', label: 'Weak passwords', icon: 'alert-circle', tone: 'danger', count: rep.weak.length,
        desc: 'Easy to guess or crack. Replace them with generated passwords.',
        rows: rep.weak.map((w) => ({ id: w.id, note: STRENGTH_LABELS[w.score] })) },
      { id: 'reused', label: 'Reused passwords', icon: 'copy', tone: 'danger', count: rep.reusedIds.size,
        desc: 'A breach of one site exposes every account that shares the password.',
        rows: rep.reused.flatMap((g, gi) => g.ids.map((id) => ({ id, note: `Used ${g.count}×`, group: gi }))) },
      { id: 'old', label: 'Old passwords', icon: 'clock', tone: 'warning', count: rep.old.length,
        desc: 'Not changed in over a year.',
        rows: rep.old.map((o) => ({ id: o.id, note: `${Math.floor(o.days / 365)} year${o.days >= 730 ? 's' : ''}+` })) },
      { id: 'insecure', label: 'Insecure websites', icon: 'globe', tone: 'warning', count: rep.insecure.length,
        desc: 'Saved with http://, so the password would be sent unencrypted. Switch to https:// where the site supports it.',
        rows: rep.insecure.map((x) => ({ id: x.id, note: x.uris[0] })) },
      { id: 'missing2fa', label: 'Two-factor available', icon: 'shield-check', tone: 'info', count: rep.missing2fa.length,
        desc: 'These sites support authenticator codes but no TOTP key is stored. Enabling two-factor authentication protects the account even if the password leaks.',
        rows: rep.missing2fa.map((x) => ({ id: x.id, note: x.domain })) },
    ];
    if (!cats.some((c) => c.id === active)) active = 'weak';
    const cur = cats.find((c) => c.id === active);

    const tone = rep.score >= 90 ? 'success' : rep.score >= 70 ? 'warning' : 'danger';
    const ring = h('div', { class: cls('vault-score', `tone-${tone}`), role: 'img', aria: { label: `Password health ${rep.score} out of 100` } },
      h('span', { class: 'vault-score-num' }, String(rep.score)), h('span', { class: 'vault-score-of' }, '/ 100'));
    ring.style.setProperty('--pct', `${rep.score}%`);

    const tiles = h('div', { class: 'vault-report-tiles', role: 'tablist', aria: { label: 'Report categories' } }, cats.map((c) => {
      const b = h('button', { type: 'button', role: 'tab', class: cls('vault-report-tile', `tone-${c.count ? c.tone : 'ok'}`, { 'is-active': c.id === active }),
        aria: { selected: String(c.id === active) } },
      h('span', { class: 'vault-report-tile-icon' }, icon(c.count ? c.icon : 'check-circle', 16)),
      h('span', { class: 'vault-report-tile-count' }, String(c.count)),
      h('span', { class: 'vault-report-tile-label' }, c.label));
      b.addEventListener('click', () => { active = c.id; draw(); });
      return b;
    }));

    let lastGroup = null;
    const rows = cur.rows.map((r) => {
      const e = byId.get(r.id);
      if (!e) return null;
      const sep = cur.id === 'reused' && r.group !== lastGroup && lastGroup !== null;
      lastGroup = r.group;
      const row = h('button', { type: 'button', class: cls('vault-report-row', { 'group-start': sep }) },
        itemAvatar(e.view.name, 28),
        h('span', { class: 'vault-report-row-text' }, h('span', { class: 'vault-row-name truncate' }, e.view.name), h('span', { class: 'vault-row-sub truncate' }, subtitle(e.view))),
        h('span', { class: 'vault-report-note' }, r.note), icon('chevron-right', 14));
      row.addEventListener('click', () => openItem(r.id));
      return row;
    }).filter(Boolean);

    host.replaceChildren(h('div', { class: 'page vault-page' },
      ui.pageHeader({ title: 'Security report', subtitle: 'Checked on this device — your passwords never leave it.' }),
      h('div', { class: 'card vault-report-summary' }, h('div', { class: 'card-body row gap-4 wrap' }, ring,
        h('div', { class: 'stack-xs' }, h('div', { class: 'bold' }, rep.score >= 90 ? 'Looking good' : rep.score >= 70 ? 'A few things to fix' : 'Needs attention'),
          h('div', { class: 'muted small' }, `${rep.checked} login${rep.checked === 1 ? '' : 's'} with passwords checked · ${rep.logins} login${rep.logins === 1 ? '' : 's'} in total.`)))),
      tiles,
      h('section', { class: 'card', role: 'tabpanel' },
        h('header', { class: 'card-header' }, h('div', { class: 'card-heading' }, h('h3', { class: 'card-title' }, vicon(cur.icon, 16), cur.label), h('p', { class: 'card-desc' }, cur.desc))),
        h('div', { class: 'card-body' }, rows.length ? h('div', { class: 'vault-report-list' }, rows)
          : ui.emptyState({ icon: 'check-circle', title: 'Nothing to fix here', compact: true })))));
  }

  draw();
  const off = session.subscribe((what) => { if (what === 'data') draw(); });
  return off;
}
