// Item detail pane: copy buttons (clipboard auto-clear), reveal toggles, live TOTP with a
// countdown ring, URIs that open in a new tab (http/https only), notes, custom fields, card and
// identity data, and the password history.

import { h, cls, append } from '../../../core/dom.js';
import { icon } from '../../../core/icons.js';
import * as ui from '../../../core/ui.js';
import { formatDateTime, formatRelative } from '../../../core/format.js';
import { TYPE_INFO, CARD_FIELDS, IDENTITY_FIELDS, MATCH_MODES, formatCardNumber, safeHref, hostOf } from '../model.js';
import { vicon, fieldRow, totpWidget, itemAvatar, sectionTitle, strengthBar, copyBtn } from '../widgets.js';

const matchLabel = (m) => MATCH_MODES.find((x) => x.value === m)?.label || '';

/**
 * @param {HTMLElement} host
 * @param {{vault: object, ctx: object, rec: object, onBack: Function, onEdit: Function, onClone: Function,
 *          onDelete: Function, onMove: Function, onToggleFavorite: Function}} o
 * @returns {() => void} cleanup
 */
export function renderDetail(host, o) {
  const { vault, rec } = o;
  const stops = [];
  const v = rec.view;
  if (!v) {
    host.replaceChildren(h('div', { class: 'vault-detail' },
      h('div', { class: 'vault-detail-top' }, backButton(o)),
      ui.alert({ variant: 'danger', title: 'This item cannot be decrypted',
        message: 'Its ciphertext does not verify with your vault key and its id. It may be damaged or may have been tampered with on the server. It is left untouched.' }),
      ui.kv([['Item id', h('span', { class: 'mono' }, rec.id)], ['Updated', formatDateTime(rec.updated_at)]]),
      h('div', { class: 'row' }, ui.button({ label: 'Delete', icon: 'trash', variant: 'danger-ghost', onClick: o.onDelete }))));
    return () => {};
  }
  const info = TYPE_INFO[v.type];
  const folder = vault.session.folderPath(rec.folder_id);

  const fav = ui.button({ icon: 'star', title: v.favorite ? 'Remove from favorites' : 'Add to favorites', variant: 'ghost', className: cls('vault-fav-btn', { 'is-on': v.favorite }), onClick: o.onToggleFavorite });
  fav.setAttribute('aria-pressed', String(v.favorite));
  const more = ui.button({ icon: 'more', title: 'More actions', variant: 'ghost' });
  more.addEventListener('click', () => ui.openMenu({
    anchor: more, align: 'end',
    items: [
      { label: 'Clone', icon: 'copy', onSelect: o.onClone },
      { label: 'Move to folder…', icon: 'folder', onSelect: o.onMove },
      { separator: true },
      { label: 'Delete', icon: 'trash', danger: true, onSelect: o.onDelete },
    ],
  }));
  const head = h('header', { class: 'vault-detail-head' },
    backButton(o),
    itemAvatar(v.name, 44),
    h('div', { class: 'vault-detail-heading' },
      h('h2', { class: 'vault-detail-title' }, v.name),
      h('div', { class: 'vault-detail-meta' },
        h('span', { class: 'row vault-type-tag' }, vicon(info.icon, 13), info.label),
        folder ? h('span', { class: 'row vault-type-tag', title: folder }, icon('folder', 13), h('span', { class: 'truncate' }, folder)) : null)),
    h('div', { class: 'vault-detail-actions' }, fav, ui.button({ label: 'Edit', icon: 'edit', onClick: () => { o.onEdit(); } }), more));

  const body = h('div', { class: 'vault-detail-body' });
  const group = (title, rows, extra) => {
    const r = rows.filter(Boolean);
    if (!r.length) return null;
    return h('section', { class: 'vault-group' }, title ? sectionTitle(title, extra) : null, h('div', { class: 'vault-group-rows' }, r));
  };

  if (v.type === 'login') {
    const l = v.login;
    let pwRow = null;
    if (l.password) {
      const meter = strengthBar();
      meter.update(l.password);
      meter.el.classList.add('vault-detail-strength');
      pwRow = fieldRow({ label: 'Password', value: l.password, secret: true, vault });
      pwRow.querySelector('.vf-main').after(meter.el);
    }
    let totpRow = null;
    if (l.totp) {
      const w = totpWidget(vault, l.totp);
      stops.push(w.stop);
      totpRow = h('div', { class: 'vf-row' }, h('div', { class: 'vf-label' }, 'Verification code (TOTP)'), w.el);
    }
    append(body, group(null, [
      l.username ? fieldRow({ label: 'Username', value: l.username, vault, secret: false }) : null,
      pwRow, totpRow,
    ]));
    if (l.uris.length) {
      append(body, group(l.uris.length === 1 ? 'Website' : 'Websites', l.uris.map((u) => {
        const href = safeHref(u.uri);
        const open = href ? ui.button({ icon: 'external', title: `Open ${hostOf(u.uri) || 'link'}`, size: 'sm', variant: 'ghost', href, target: '_blank' }) : null;
        if (open) open.setAttribute('referrerpolicy', 'no-referrer');
        const row = fieldRow({ label: u.match ? `URI · ${matchLabel(u.match)}` : 'URI', value: u.uri, vault, secret: false, extra: open });
        row.querySelector('.vf-value').classList.add('vf-uri');
        return row;
      })));
    }
  }

  if (v.type === 'card') {
    const c = v.card;
    const exp = [c.exp_month, c.exp_year].filter(Boolean).join(' / ');
    append(body, group('Card', [
      c.cardholder ? fieldRow({ label: 'Cardholder name', value: c.cardholder, vault }) : null,
      c.brand ? fieldRow({ label: 'Brand', value: c.brand, vault, copy: false }) : null,
      c.number ? fieldRow({ label: 'Number', value: c.number, secret: true, vault, display: formatCardNumber }) : null,
      exp ? fieldRow({ label: 'Expiry', value: exp, vault, copy: false }) : null,
      c.code ? fieldRow({ label: 'Security code', value: c.code, secret: true, vault }) : null,
    ]));
  }

  if (v.type === 'identity') {
    const i = v.identity;
    const name = [i.title, i.first_name, i.middle_name, i.last_name].filter(Boolean).join(' ');
    const addr = [i.address1, i.address2, [i.city, i.state, i.postal_code].filter(Boolean).join(' '), i.country].filter(Boolean).join('\n');
    const rows = (keys) => keys.map((k) => {
      const [, label, secret] = IDENTITY_FIELDS.find((f) => f[0] === k);
      return i[k] ? fieldRow({ label, value: i[k], secret, vault }) : null;
    });
    append(body, 
      group('Personal', [name ? fieldRow({ label: 'Name', value: name, vault }) : null, ...rows(['username', 'company'])]),
      group('Contact', [...rows(['email', 'phone']), addr ? fieldRow({ label: 'Address', value: addr, vault, multiline: true }) : null]),
      group('Documents', rows(['ssn', 'passport', 'license'])),
    );
  }

  if (v.notes) {
    append(body, group('Notes', [h('div', { class: 'vault-notes' }, v.notes, h('div', { class: 'vault-notes-copy' }, copyBtn(vault, () => v.notes, 'Notes', { secret: false })))]));
  }

  if (v.fields.length) {
    append(body, group('Custom fields', v.fields.map((f) => fieldRow({ label: f.name || '(unnamed)', value: f.value, secret: f.hidden, vault }))));
  }

  if (v.history.length) {
    const list = h('div', { class: 'vault-history', hidden: true }, v.history.map((p) => fieldRow({
      label: p.changed_at ? `Replaced ${formatRelative(p.changed_at)}` : 'Older password', value: p.password, secret: true, vault,
    })));
    const toggle = ui.button({ label: `Password history (${v.history.length})`, icon: 'history', variant: 'ghost', size: 'sm' });
    toggle.setAttribute('aria-expanded', 'false');
    toggle.addEventListener('click', () => {
      list.hidden = !list.hidden;
      toggle.setAttribute('aria-expanded', String(!list.hidden));
    });
    append(body, h('section', { class: 'vault-group' }, toggle, list));
  }

  append(body, h('footer', { class: 'vault-detail-foot' },
    h('span', {}, 'Created ', h('time', { datetime: v.created_at || rec.created_at, dataset: { tip: formatDateTime(v.created_at || rec.created_at) } }, formatRelative(v.created_at || rec.created_at))),
    h('span', {}, 'Updated ', h('time', { datetime: v.updated_at || rec.updated_at, dataset: { tip: formatDateTime(v.updated_at || rec.updated_at) } }, formatRelative(v.updated_at || rec.updated_at))),
    h('span', { class: 'row vault-e2e' }, icon('lock', 12), 'End-to-end encrypted')));

  host.replaceChildren(h('article', { class: 'vault-detail' }, head, body));
  host.scrollTop = 0;
  return () => stops.forEach((fn) => fn());
}

function backButton(o) {
  const b = ui.button({ icon: 'arrow-left', title: 'Back to list', variant: 'ghost', className: 'vault-back' });
  b.addEventListener('click', () => o.onBack());
  return b;
}
