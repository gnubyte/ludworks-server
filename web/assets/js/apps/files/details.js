// Files app — details side panel: metadata, category/subcategory pickers, tag editor,
// SHA-256, owner and links. Edits are saved immediately (PATCH /files/{id}).

import { h, uid } from '../../core/dom.js';
import { icon } from '../../core/icons.js';
import { formatBytes, formatDateTime, formatNumber, plural } from '../../core/format.js';
import { fileKind, KIND_ICONS, KIND_LABELS, isMedia, previewKind, selectionSummary, thumbVersion, parseTags } from './model.js';

/**
 * @param {object} ctx
 * @param {object} fapi
 * @param {{categories: () => Promise<object[]>, onChanged: (e: object) => void, onAction: (action: string, entries: object[]) => void,
 *          onClose: () => void, spaceName: (id: string) => string}} o
 */
export function createDetails(ctx, fapi, o) {
  const { ui } = ctx;
  const body = h('div', { class: 'fx-details-body' });
  const closeBtn = ui.button({ icon: 'x', title: 'Close details (I)', size: 'sm', variant: 'ghost', onClick: () => o.onClose() });
  const el = h('aside', { class: 'fx-details', aria: { label: 'Details' } },
    h('header', { class: 'fx-details-head' }, h('h2', { class: 'fx-details-title' }, 'Details'), closeBtn), body);
  let current = [];
  let writable = false;
  let token = 0;

  function show(entries, { canWrite = false, dir = null } = {}) {
    current = entries;
    writable = canWrite;
    const my = ++token;
    if (!entries.length) {
      body.replaceChildren(dir ? folderSummary(dir) : ui.emptyState({ icon: 'info', title: 'Nothing selected', message: 'Select a file or folder to see its details.', compact: true }));
      return;
    }
    if (entries.length > 1) {
      body.replaceChildren(multi(entries));
      return;
    }
    body.replaceChildren(single(entries[0], my));
  }

  function folderSummary(dir) {
    return h('div', { class: 'stack-md' },
      h('div', { class: 'fx-details-hero' }, h('div', { class: 'fx-details-icon fx-kind-folder' }, icon('folder-open', 40))),
      h('div', { class: 'fx-details-name' }, dir.name || 'Folder'),
      h('p', { class: 'small muted center' }, 'Select items to see their details. Tip: Shift-click selects a range, Ctrl/⌘-click toggles.'));
  }

  function multi(entries) {
    const s = selectionSummary(entries);
    return h('div', { class: 'stack-md' },
      h('div', { class: 'fx-details-hero' }, h('div', { class: 'fx-details-icon fx-kind-multi' }, icon('layers', 40))),
      h('div', { class: 'fx-details-name' }, plural(s.count, 'item')),
      ui.kv([
        ['Files', formatNumber(s.files)],
        ['Folders', formatNumber(s.folders)],
        ['Size', `${formatBytes(s.bytes)}${s.folders ? ' + folder contents' : ''}`],
      ]),
      h('div', { class: 'fx-details-actions' },
        writable ? ui.button({ label: 'Categorize…', icon: 'tag', size: 'sm', onClick: () => o.onAction('categorize', entries) }) : null,
        writable ? ui.button({ label: 'Move…', icon: 'arrow-right', size: 'sm', onClick: () => o.onAction('move', entries) }) : null,
        ui.button({ label: 'Copy…', icon: 'copy', size: 'sm', onClick: () => o.onAction('copy', entries) }),
        ui.button({ label: 'Download', icon: 'download', size: 'sm', onClick: () => o.onAction('download', entries) }),
        writable ? ui.button({ label: 'Delete', icon: 'trash', size: 'sm', variant: 'danger-ghost', onClick: () => o.onAction('delete', entries) }) : null));
  }

  function single(e, my) {
    const kind = fileKind(e);
    const hero = h('div', { class: 'fx-details-hero' });
    if (e.has_thumbnail) {
      const img = h('img', { class: 'fx-details-thumb', alt: '', loading: 'lazy', src: fapi.thumbUrl(e.id, 'lg', thumbVersion(e)) });
      img.addEventListener('error', () => img.replaceWith(h('div', { class: `fx-details-icon fx-kind-${kind}` }, icon(KIND_ICONS[kind], 40))));
      hero.append(img);
    } else {
      hero.append(h('div', { class: `fx-details-icon fx-kind-${kind}` }, icon(KIND_ICONS[kind], 40)));
    }
    if (previewKind(e) || e.is_dir) {
      hero.classList.add('is-clickable');
      hero.addEventListener('click', () => o.onAction('open', [e]));
    }
    const mine = e.owner_id && e.owner_id === ctx.user?.id;
    const where = [o.spaceName(e.space_id), ...String(e.path).split('/').slice(0, -1)].filter(Boolean).join(' › ');
    const shaValue = e.is_dir ? null : e.sha256
      ? h('span', { class: 'fx-sha' }, h('code', { class: 'mono break' }, e.sha256), ui.copyButton(e.sha256, { title: 'Copy SHA-256', message: 'SHA-256 copied' }))
      : h('span', { class: 'faint' }, 'Calculating…');
    const link = new URL(fapi.contentUrl(e.id), globalThis.location.origin).href;
    const info = ui.kv([
      ['Type', e.is_dir ? 'Folder' : `${KIND_LABELS[kind]} (${e.mime})`],
      e.is_dir ? null : ['Size', h('span', { dataset: { tip: `${formatNumber(e.size)} bytes` } }, formatBytes(e.size))],
      ['Modified', formatDateTime(e.mod_time)],
      ['Added', formatDateTime(e.created_at)],
      ['Location', where || '—'],
      ['Owner', mine ? 'You' : (e.owner_id ? ownerLabel(e.owner_id) : '—')],
      shaValue ? ['SHA-256', shaValue] : null,
    ]);
    const acts = h('div', { class: 'fx-details-actions' },
      ui.button({ label: e.is_dir ? 'Open' : (previewKind(e) ? 'Preview' : 'Open'), icon: e.is_dir ? 'folder-open' : 'eye', size: 'sm', variant: 'primary', onClick: () => o.onAction('open', [e]) }),
      ui.button({ label: e.is_dir ? 'ZIP' : 'Download', icon: 'download', size: 'sm', onClick: () => o.onAction('download', [e]) }),
      e.is_dir ? null : ui.button({ icon: 'link', title: 'Copy download link', size: 'sm', onClick: () => ui.copyText(link, { message: 'Link copied (works for signed-in users with access)' }) }),
      isMedia(e) ? ui.button({ label: 'TV library', icon: 'tv', size: 'sm', href: `/tv/library/item/${encodeURIComponent(e.id)}` }) : null,
      writable ? ui.button({ icon: 'edit', title: 'Rename (F2)', size: 'sm', onClick: () => o.onAction('rename', [e]) }) : null,
      writable ? ui.button({ icon: 'trash', title: 'Delete (Del)', size: 'sm', variant: 'danger-ghost', onClick: () => o.onAction('delete', [e]) }) : null);
    const metaHost = h('div', { class: 'stack-md' });
    const wrap = h('div', { class: 'stack-md' },
      hero, h('div', { class: 'fx-details-name break' }, e.name), acts, info, metaHost);
    o.categories().then((cats) => {
      if (my === token) metaHost.replaceChildren(metaEditor(e, cats));
    }).catch(() => {});
    return wrap;
  }

  /** Owner display: admins see the username (GET /admin/users/{id}), others the short id. */
  const owners = new Map();
  function ownerLabel(id) {
    const el = h('code', { class: 'mono small' }, id.slice(0, 10));
    if (!ctx.isAdmin) return el;
    if (!owners.has(id)) owners.set(id, ctx.api.get(`/admin/users/${encodeURIComponent(id)}`).then((u) => u.username).catch(() => null));
    owners.get(id).then((name) => { if (name) el.replaceWith(h('span', {}, name)); });
    return el;
  }

  function metaEditor(e, cats) {
    const catId = uid('fxdc');
    const subId = uid('fxds');
    const catSel = h('select', { class: 'select', id: catId, disabled: !writable },
      h('option', { value: '' }, 'None'), cats.map((c) => h('option', { value: c.slug }, c.name)));
    // Keep a stored slug that is no longer in the taxonomy visible.
    if (e.category && !cats.some((c) => c.slug === e.category)) catSel.append(h('option', { value: e.category }, e.category));
    catSel.value = e.category || '';
    const subSel = h('select', { class: 'select', id: subId, disabled: !writable });
    const fillSubs = () => {
      const c = cats.find((x) => x.slug === catSel.value);
      subSel.replaceChildren(h('option', { value: '' }, c ? 'None' : '—'), ...(c?.subcategories || []).map((s) => h('option', { value: s.slug }, s.name)));
      if (e.subcategory && c?.slug === e.category && !(c.subcategories || []).some((s) => s.slug === e.subcategory)) subSel.append(h('option', { value: e.subcategory }, e.subcategory));
      subSel.value = c && c.slug === e.category ? e.subcategory || '' : '';
      subSel.disabled = !writable || !c;
    };
    fillSubs();
    const save = async (patch) => {
      try {
        const upd = await fapi.patch(e.id, patch);
        Object.assign(e, upd);
        o.onChanged(upd);
      } catch (err) {
        ui.toast.error(err);
        catSel.value = e.category || '';
        fillSubs();
      }
    };
    catSel.addEventListener('change', () => {
      fillSubs();
      save({ category: catSel.value, subcategory: '' });
    });
    subSel.addEventListener('change', () => save({ category: catSel.value, subcategory: subSel.value }));

    const chips = h('div', { class: 'chips' });
    const tagInput = h('input', { class: 'input input-sm fx-tag-input', placeholder: writable ? 'Add tag…' : '', disabled: !writable, autocomplete: 'off', aria: { label: 'Add tags' }, maxlength: '200' });
    const renderTags = () => {
      chips.replaceChildren(...(e.tags || []).map((t) => ui.chip({
        label: t, icon: 'tag',
        onRemove: writable ? () => save({ tags: e.tags.filter((x) => x !== t) }).then(renderTags) : undefined,
      })), ...(e.tags?.length ? [] : [h('span', { class: 'faint small' }, 'No tags')]));
    };
    renderTags();
    tagInput.addEventListener('keydown', (ev) => {
      if (ev.key === 'Enter' || ev.key === ',') {
        ev.preventDefault();
        const add = parseTags(tagInput.value);
        if (!add.length) return;
        tagInput.value = '';
        const next = [...(e.tags || [])];
        for (const t of add) if (!next.some((x) => x.toLowerCase() === t.toLowerCase())) next.push(t);
        save({ tags: next }).then(renderTags);
      } else if (ev.key === 'Backspace' && !tagInput.value && e.tags?.length) {
        save({ tags: e.tags.slice(0, -1) }).then(renderTags);
      }
    });
    return h('div', { class: 'stack-md fx-details-meta' },
      h('div', { class: 'grid-2' },
        h('div', { class: 'field' }, h('label', { class: 'field-label', for: catId }, 'Category'), catSel),
        h('div', { class: 'field' }, h('label', { class: 'field-label', for: subId }, 'Subcategory'), subSel)),
      h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Tags'), chips, writable ? tagInput : null));
  }

  return {
    el,
    show,
    /** Re-render with fresh data for the shown entries (after a change elsewhere). */
    refresh(byId, opts) {
      if (!current.length) return;
      show(current.map((e) => byId.get(e.id) || e), opts);
    },
    get entries() { return current; },
  };
}
