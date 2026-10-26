// /tv/lineups — lineup list; /tv/lineups/<id> — lineup editor: ordered items with
// drag-and-drop and keyboard reordering (Alt+↑/↓), per-item order and limit, adding items from
// the library picker or as show/season/folder/query sources, the expanded preview with total
// duration and skipped files, rename and delete.

import { h, cls, debounce, uid } from '../../core/dom.js';
import { formatDuration, formatDurationLong, plural } from '../../core/format.js';
import { allPages } from './tvapi.js';
import { describeItem, itemToInput, moveItem, normalizePicked, pickedTitles } from './model.js';
import { tvIcon } from './icons.js';
import { openMediaPicker } from './picker.js';
import { ensureStyle } from '../../core/style.js';

const REASONS = {
  unreadable: 'Missing, in the trash or not readable',
  unknown_duration: 'Duration unknown (not probed yet)',
  not_found: 'Folder not found',
};

/* ===================================================================== list */

export async function renderList(host, { ctx, api, ui, signal, navigate }) {
  const me = ctx.user?.id;
  const body = h('div', { class: 'stack' });
  const page = h('div', { class: 'page wide tv-lineups' },
    ui.pageHeader({
      title: 'Lineups',
      subtitle: 'Ordered program sources — shows, seasons, folders, smart queries — that channels play on a loop.',
      actions: ui.button({ label: 'New lineup', icon: 'plus', variant: 'primary', onClick: () => createLineupDialog() }),
    }),
    body);
  host.append(page);

  const table = ui.dataTable({
    columns: [
      { key: 'name', label: 'Name', sortable: true, render: (l) => h('div', { class: 'cell-stack' },
        h('a', { class: 'cell-title', href: `/tv/lineups/${l.id}` }, l.name),
        l.description ? h('span', { class: 'cell-sub truncate' }, l.description) : null) },
      { key: 'item_count', label: 'Programs', sortable: true, align: 'right', render: (l) => h('span', { class: 'nums' }, String(l.item_count)) },
      { key: 'total_duration_seconds', label: 'Loop length', sortable: true, align: 'right', hideBelow: 'sm',
        render: (l) => h('span', { class: 'nums' }, l.total_duration_seconds ? formatDurationLong(l.total_duration_seconds) : '—') },
      { key: 'items', label: 'Sources', hideBelow: 'md', sortValue: (l) => l.items.length, render: (l) => plural(l.items.length, 'source') },
      { key: 'owner', label: '', hideBelow: 'sm', sortable: false, render: (l) => (l.owner_id === me ? h('span') : ui.badge('Shared with you', 'neutral', { icon: 'users' })) },
      { key: 'updated_at', label: 'Updated', sortable: true, align: 'right', hideBelow: 'md', render: (l) => ui.timeAgo(l.updated_at) },
    ],
    rowKey: 'id',
    sort: { key: 'name', dir: 'asc' },
    onRowClick: (l) => navigate(`/tv/lineups/${l.id}`),
    caption: 'Lineups',
    empty: () => ui.emptyState({
      icon: 'list', title: 'No lineups yet',
      message: 'A lineup is a playlist recipe: add shows, seasons, folders or a smart query, then give it a channel.',
      action: ui.button({ label: 'Create a lineup', icon: 'plus', variant: 'primary', onClick: () => createLineupDialog() }),
    }),
  });
  body.append(table.el);
  table.setLoading(true);

  async function load() {
    try {
      table.setRows(await allPages((cursor) => api.lineups(cursor, signal)));
    } catch (err) {
      if (!signal.aborted) body.replaceChildren(ui.errorState(err, () => { body.replaceChildren(table.el); load(); }));
    }
  }

  function createLineupDialog() {
    const name = h('input', { class: 'input', name: 'name', required: true, maxlength: '200', autocomplete: 'off', placeholder: 'Saturday morning cartoons' });
    const desc = h('textarea', { class: 'textarea', name: 'description', rows: 3, maxlength: '2000', placeholder: 'Optional' });
    const err = h('div');
    ui.modal({
      title: 'New lineup',
      body: h('div', { class: 'stack-md' }, err,
        h('div', { class: 'field', dataset: { field: 'name' } }, h('label', { class: 'field-label', for: (name.id = uid('ln')) }, 'Name'), name, h('div', { class: 'field-error', hidden: true })),
        h('div', { class: 'field', dataset: { field: 'description' } }, h('label', { class: 'field-label', for: (desc.id = uid('ld')) }, 'Description'), desc)),
      footer: [{ label: 'Cancel', value: null }, { label: 'Create', variant: 'primary', type: 'submit' }],
      onSubmit: async () => {
        try {
          const l = await api.createLineup({ name: name.value, description: desc.value });
          navigate(`/tv/lineups/${l.id}`);
          return l;
        } catch (e) {
          err.replaceChildren(ui.alert({ variant: 'danger', message: ui.errorMessage(e) }));
          return false;
        }
      },
    });
  }

  await load();
  return null;
}

/* =================================================================== editor */

export async function renderEditor(host, vctx) {
  const { ctx, api, ui, signal, navigate, route } = vctx;
  const me = ctx.user?.id;
  const page = h('div', { class: 'page wide tv-lineup-editor' });
  host.append(page);
  page.append(ui.loadingBlock('Loading lineup…'));

  let lineup;
  try {
    lineup = await api.lineup(route.id, signal);
  } catch (err) {
    if (signal.aborted) return null;
    page.replaceChildren(ui.emptyState({
      icon: 'list', title: err?.status === 404 ? 'Lineup not found' : 'Couldn’t load the lineup', message: ui.errorMessage(err),
      action: ui.button({ label: 'All lineups', icon: 'arrow-left', href: '/tv/lineups' }),
    }));
    return null;
  }
  const owner = lineup.owner_id === me;
  let items = lineup.items.map((it) => ({ ...it }));
  let titles = {};
  let expanded = null;
  let saveSeq = 0;
  let dragFrom = -1;
  let focusAfter = null;

  /* --------------------------------------------------------------- header */
  const titleEl = h('h1', { class: 'page-title' }, lineup.name);
  const descEl = h('p', { class: 'page-subtitle' }, lineup.description || '');
  const saveState = h('span', { class: 'tv-save-state small faint', role: 'status', aria: { live: 'polite' } });
  const actions = owner ? [
    saveState,
    ui.menuButton({
      label: 'Add', icon: 'plus', variant: 'primary', caret: true, title: 'Add items', align: 'end',
      items: () => [
        { label: 'From the library…', icon: 'film', onSelect: () => addFromLibrary() },
        { separator: true },
        { label: 'Show or season…', icon: 'tv', onSelect: () => showDialog() },
        { label: 'Folder…', icon: 'folder', onSelect: () => folderDialog() },
        { label: 'Smart query…', icon: 'filter', onSelect: () => queryDialog() },
      ],
    }),
    ui.menuButton({
      icon: 'more', title: 'Lineup actions', variant: 'secondary',
      items: () => [
        { label: 'Rename…', icon: 'edit', onSelect: () => rename() },
        { label: 'Create a channel', icon: 'radio', onSelect: () => createChannel() },
        { separator: true },
        { label: 'Delete lineup…', icon: 'trash', danger: true, onSelect: () => remove() },
      ],
    }),
  ] : [ui.badge('Shared with you — read only', 'neutral', { icon: 'eye' })];
  const header = h('header', { class: 'page-header' },
    h('div', { class: 'page-heading' },
      ui.breadcrumbs([{ label: 'Lineups', href: '/tv/lineups', icon: 'list' }, { label: lineup.name }]),
      titleEl, descEl),
    h('div', { class: 'page-actions' }, actions));

  /* ---------------------------------------------------------------- items */
  const list = h('ol', { class: 'tv-items', aria: { label: 'Lineup items' } });
  const itemsCard = ui.card({
    title: 'Program sources', icon: 'layers',
    description: owner ? 'Played top to bottom, then the lineup loops. Drag to reorder, or focus a row and press Alt+↑ / Alt+↓.' : 'Played top to bottom, then the lineup loops.',
    body: list, flush: true, className: 'tv-items-card',
  });
  const previewBody = h('div', { class: 'tv-preview' });
  const previewCard = ui.card({ title: 'Expanded preview', icon: 'eye', body: previewBody, className: 'tv-preview-card' });
  page.replaceChildren(header, h('div', { class: 'tv-editor-grid' }, itemsCard, previewCard));

  function renderItems() {
    if (!items.length) {
      list.replaceChildren(h('li', { class: 'tv-items-empty' }, ui.emptyState({
        icon: 'layers', title: 'No sources yet', compact: true,
        message: owner ? 'Add shows, seasons, folders, single files or a smart query.' : 'This lineup is empty.',
        action: owner ? ui.button({ label: 'Add from the library', icon: 'plus', onClick: () => addFromLibrary() }) : null,
      })));
      return;
    }
    // Re-rendering must not steal focus from a row control the user is working with.
    const ae = document.activeElement;
    const row = ae instanceof Element ? ae.closest('.tv-item') : null;
    const restore = row && list.contains(row)
      ? { i: Number(row.dataset.index), cls: ae === row ? null : [...ae.classList].find((c) => c.startsWith('tv-item-')) }
      : null;
    list.replaceChildren(...items.map((it, i) => itemRow(it, i)));
    if (focusAfter !== null) {
      list.children[focusAfter]?.focus();
      focusAfter = null;
    } else if (restore) {
      const r = list.children[restore.i];
      (restore.cls ? r?.querySelector(`.${restore.cls}`) : r)?.focus({ preventScroll: true });
    }
  }

  function itemRow(it, i) {
    const d = describeItem(it, titles);
    const counts = expanded ? expanded.items.filter((x) => x.source_item_id === it.id) : null;
    const dur = counts ? counts.reduce((s, x) => s + x.duration_seconds, 0) : 0;
    const li = h('li', {
      class: cls('tv-item', { 'is-readonly': !owner }), tabindex: '0', dataset: { index: String(i) },
      aria: { label: `${i + 1}. ${d.title}, ${d.detail}` }, draggable: owner ? 'true' : undefined,
    });
    const grip = owner ? h('span', { class: 'tv-item-grip', aria: { hidden: 'true' }, title: 'Drag to reorder' }, tvIcon('grip', 16)) : null;
    const main = h('div', { class: 'tv-item-main' },
      h('span', { class: 'tv-item-icon' }, tvIcon(d.icon, 16)),
      h('div', { class: 'tv-item-text' },
        h('div', { class: 'tv-item-title truncate', title: d.title }, d.title),
        h('div', { class: 'tv-item-detail truncate small muted' }, d.detail,
          counts ? h('span', { class: 'faint' }, ` · ${plural(counts.length, 'program')}${dur ? ` · ${formatDurationLong(dur)}` : ''}`) : null)));
    li.append(...[h('span', { class: 'tv-item-pos nums faint' }, String(i + 1)), grip, main].filter(Boolean));
    if (owner) {
      const order = h('select', { class: 'select select-sm tv-item-order', aria: { label: 'Order' } },
        h('option', { value: 'default' }, 'In order'), h('option', { value: 'random' }, 'Shuffled'));
      order.value = it.order || 'default';
      order.addEventListener('change', () => { items[i] = { ...items[i], order: order.value }; save(); });
      const limit = h('input', { class: 'input input-sm tv-item-limit nums', type: 'number', min: '1', max: '100000', inputmode: 'numeric', placeholder: 'All', aria: { label: 'Limit (maximum programs)' } });
      limit.value = it.limit ? String(it.limit) : '';
      const saveLimit = debounce(() => {
        const n = Number(limit.value);
        const v = limit.value === '' ? undefined : (Number.isInteger(n) && n >= 1 ? n : items[i].limit);
        if (v === items[i].limit) return;
        items[i] = { ...items[i], limit: v };
        save();
      }, 600);
      limit.addEventListener('input', saveLimit);
      const menu = ui.menuButton({
        icon: 'more-vertical', title: 'Item actions', size: 'sm',
        items: () => [
          { label: 'Move up', icon: 'arrow-up', disabled: i === 0, shortcut: 'Alt+↑', onSelect: () => move(i, i - 1) },
          { label: 'Move down', icon: 'arrow-down', disabled: i === items.length - 1, shortcut: 'Alt+↓', onSelect: () => move(i, i + 1) },
          { label: 'Move to top', icon: 'chevrons-up-down', disabled: i === 0, onSelect: () => move(i, 0) },
          { label: 'Move to bottom', icon: 'chevrons-up-down', disabled: i === items.length - 1, onSelect: () => move(i, items.length - 1) },
          { label: 'Rename label…', icon: 'edit', onSelect: () => relabel(i) },
          { separator: true },
          { label: 'Remove', icon: 'trash', danger: true, shortcut: 'Del', onSelect: () => removeItem(i) },
        ],
      });
      li.append(h('div', { class: 'tv-item-controls' }, order, limit, menu));
    } else {
      li.append(h('div', { class: 'tv-item-controls small muted' },
        it.order === 'random' ? ui.badge('Shuffled', 'accent') : null, it.limit ? ui.badge(`Max ${it.limit}`) : null));
    }
    return li;
  }

  function move(from, to) {
    if (to < 0 || to >= items.length || from === to) return;
    items = moveItem(items, from, to);
    focusAfter = to;
    renderItems();
    save();
  }

  function removeItem(i) {
    const removed = items[i];
    items = items.filter((_, k) => k !== i);
    focusAfter = Math.min(i, items.length - 1);
    if (focusAfter < 0) focusAfter = null;
    renderItems();
    save();
    ui.toast(`Removed “${describeItem(removed, titles).title}”`, {
      action: { label: 'Undo', onClick: () => { items = [...items.slice(0, i), removed, ...items.slice(i)]; renderItems(); save(); } },
    });
  }

  async function relabel(i) {
    const v = await ui.prompt({ title: 'Item label', label: 'Label shown in the editor', value: items[i].label || '', required: false, confirmLabel: 'Save',
      validate: (s) => (s.length > 200 ? 'At most 200 characters' : null) });
    if (v === null) return;
    items[i] = { ...items[i], label: v.trim() || undefined };
    renderItems();
    save();
  }

  /* ------------------------------------------------- keyboard + drag/drop */
  if (owner) {
    list.addEventListener('keydown', (e) => {
      const li = e.target instanceof Element ? e.target.closest('.tv-item') : null;
      if (!li || e.target !== li) return;
      const i = Number(li.dataset.index);
      if (e.altKey && (e.key === 'ArrowUp' || e.key === 'ArrowDown')) {
        e.preventDefault();
        move(i, i + (e.key === 'ArrowUp' ? -1 : 1));
      } else if (!e.altKey && (e.key === 'ArrowUp' || e.key === 'ArrowDown')) {
        e.preventDefault();
        list.children[i + (e.key === 'ArrowUp' ? -1 : 1)]?.focus();
      } else if (e.key === 'Delete' || e.key === 'Backspace') {
        e.preventDefault();
        removeItem(i);
      }
    });
    list.addEventListener('dragstart', (e) => {
      const li = e.target instanceof Element ? e.target.closest('.tv-item') : null;
      if (!li) return;
      dragFrom = Number(li.dataset.index);
      li.classList.add('is-dragging');
      e.dataTransfer.effectAllowed = 'move';
      e.dataTransfer.setData('text/plain', String(dragFrom));
    });
    list.addEventListener('dragend', () => {
      dragFrom = -1;
      for (const el of list.querySelectorAll('.is-dragging,.drop-before,.drop-after')) el.classList.remove('is-dragging', 'drop-before', 'drop-after');
    });
    list.addEventListener('dragover', (e) => {
      if (dragFrom < 0) return;
      const li = e.target instanceof Element ? e.target.closest('.tv-item') : null;
      if (!li) return;
      e.preventDefault();
      e.dataTransfer.dropEffect = 'move';
      const r = li.getBoundingClientRect();
      const after = e.clientY > r.top + r.height / 2;
      for (const el of list.querySelectorAll('.drop-before,.drop-after')) el.classList.remove('drop-before', 'drop-after');
      li.classList.add(after ? 'drop-after' : 'drop-before');
    });
    list.addEventListener('drop', (e) => {
      if (dragFrom < 0) return;
      const li = e.target instanceof Element ? e.target.closest('.tv-item') : null;
      if (!li) return;
      e.preventDefault();
      const target = Number(li.dataset.index);
      const after = li.classList.contains('drop-after');
      let to = target + (after ? 1 : 0);
      if (dragFrom < to) to -= 1;
      const from = dragFrom;
      dragFrom = -1;
      move(from, to);
    });
  }

  /* ------------------------------------------------------------------ save */
  async function save() {
    const my = ++saveSeq;
    saveState.textContent = 'Saving…';
    try {
      const l = await api.replaceItems(lineup.id, items.map(itemToInput));
      if (my !== saveSeq) return;
      lineup = l;
      // Keep local order; adopt server ids for new items.
      items = l.items.map((it) => ({ ...it }));
      saveState.textContent = 'Saved';
      setTimeout(() => { if (my === saveSeq) saveState.textContent = ''; }, 2000);
      renderItems();
      loadPreview();
    } catch (err) {
      if (my !== saveSeq) return;
      saveState.textContent = '';
      ui.toast.error(err);
      try {
        lineup = await api.lineup(lineup.id, signal);
        items = lineup.items.map((it) => ({ ...it }));
        renderItems();
      } catch { /* keep local state */ }
    }
  }

  async function addItems(inputs, newTitles = {}) {
    if (!inputs.length) return;
    Object.assign(titles, newTitles);
    saveState.textContent = 'Saving…';
    try {
      lineup = await api.insertItems(lineup.id, inputs);
      items = lineup.items.map((it) => ({ ...it }));
      saveState.textContent = 'Saved';
      renderItems();
      loadPreview();
      ui.toast.success(`Added ${plural(inputs.length, 'source')}`);
    } catch (err) {
      saveState.textContent = '';
      ui.toast.error(err);
    }
  }

  /* --------------------------------------------------------------- preview */
  async function loadPreview() {
    previewBody.replaceChildren(ui.skeletonText(4));
    try {
      expanded = await api.expanded(lineup.id, signal);
    } catch (err) {
      if (!signal.aborted) previewBody.replaceChildren(ui.errorState(err, () => loadPreview()));
      return;
    }
    for (const x of expanded.items) titles[x.file_id] = x.title;
    for (const s of expanded.skipped) if (s.title) titles[s.file_id] = s.title;
    renderItems();
    const shown = expanded.items.slice(0, 300);
    const stats = h('div', { class: 'tv-preview-stats' },
      h('div', { class: 'stat' }, h('div', { class: 'stat-label' }, 'Programs'), h('div', { class: 'stat-value nums' }, String(expanded.items.length))),
      h('div', { class: 'stat' }, h('div', { class: 'stat-label' }, 'Loop length'), h('div', { class: 'stat-value nums' }, expanded.total_duration_seconds ? formatDurationLong(expanded.total_duration_seconds, 3) : '0s')),
      h('div', { class: 'stat' }, h('div', { class: 'stat-label' }, 'Skipped'), h('div', { class: cls('stat-value nums', { 'warning-text': expanded.skipped.length }) }, String(expanded.skipped.length))));
    const ol = h('ol', { class: 'tv-preview-list' }, shown.map((x, i) => h('li', {},
      h('span', { class: 'tv-preview-n nums faint' }, String(i + 1)),
      h('span', { class: 'truncate', title: x.title }, x.title),
      h('span', { class: 'nums faint' }, formatDuration(x.duration_seconds)))));
    const more = expanded.items.length > shown.length ? h('p', { class: 'small faint' }, `…and ${plural(expanded.items.length - shown.length, 'more program')}`) : null;
    const skipped = expanded.skipped.length ? h('details', { class: 'tv-skipped' },
      h('summary', {}, tvIcon('alert', 14), ` ${plural(expanded.skipped.length, 'file')} skipped`),
      h('ul', {}, expanded.skipped.slice(0, 200).map((s) => h('li', {},
        h('span', { class: 'truncate' }, s.title || titles[s.file_id] || s.file_id),
        h('span', { class: 'small muted' }, REASONS[s.reason] || s.reason))))) : null;
    previewBody.replaceChildren(...[stats,
      expanded.items.length ? ol : h('p', { class: 'muted small' }, 'Nothing playable yet. Files need a known duration (the media library probes them) and must be readable by the lineup owner.'),
      more, skipped,
      h('p', { class: 'small faint tv-preview-note' }, 'Shuffled sources appear in their natural order here; channels shuffle them on every loop.')].filter(Boolean));
  }

  /* --------------------------------------------------------------- dialogs */
  async function addFromLibrary() {
    let picked = null;
    try {
      const lib = await import('./library/index.js');
      if (typeof lib.pickMedia !== 'function') throw new Error('no pickMedia');
      await ensureStyle('/assets/css/apps/tv-library.css'); // the picker is styled by the library sheet
      picked = await lib.pickMedia(ctx, { mode: 'lineup', multiple: true });
    } catch (err) {
      if (err?.name === 'AbortError') return;
      picked = await openMediaPicker(vctx, { multiple: true });
    }
    if (!picked) return;
    const arr = Array.isArray(picked) ? picked : [picked];
    addItems(normalizePicked(arr), pickedTitles(arr));
  }

  function formModal({ title, fields, submitLabel = 'Add', onSubmit }) {
    const banner = h('div');
    return ui.modal({
      title, size: 'sm',
      body: h('div', { class: 'stack-md' }, banner, ...fields),
      footer: [{ label: 'Cancel', value: null }, { label: submitLabel, variant: 'primary', type: 'submit' }],
      onSubmit: async () => {
        try {
          return await onSubmit();
        } catch (err) {
          banner.replaceChildren(ui.alert({ variant: 'danger', message: ui.errorMessage(err) }));
          return false;
        }
      },
    });
  }

  function labeled(label, control, hint) {
    control.id ||= uid('tvf');
    return h('div', { class: 'field' }, h('label', { class: 'field-label', for: control.id }, label), control,
      hint ? h('div', { class: 'field-hint' }, hint) : null);
  }

  function showDialog() {
    const listId = uid('shows');
    const show = h('input', { class: 'input', required: true, maxlength: '500', autocomplete: 'off', list: listId, placeholder: 'Show title' });
    const dl = h('datalist', { id: listId });
    const season = h('input', { class: 'input', type: 'number', min: '0', max: '10000', inputmode: 'numeric', placeholder: 'All seasons' });
    const order = ui.segmented({ options: [{ value: 'default', label: 'In order' }, { value: 'random', label: 'Shuffled' }], value: 'default', size: 'sm', ariaLabel: 'Order' });
    show.addEventListener('input', debounce(async () => {
      try {
        const r = await api.shows({ q: show.value, limit: 20 }, signal);
        dl.replaceChildren(...(r.items || []).map((s) => h('option', { value: s.show_title })));
      } catch { /* suggestions are optional */ }
    }, 250));
    formModal({
      title: 'Add a show or season',
      fields: [labeled('Show', h('div', {}, show, dl)), labeled('Season', season, 'Leave empty for every episode; 0 is specials.'), labeled('Order', order.el)],
      onSubmit: async () => {
        const s = season.value === '' ? null : Number(season.value);
        if (!show.value.trim()) throw new Error('Enter a show title.');
        await addItems([s === null ? { kind: 'show', show_title: show.value.trim(), order: order.value } : { kind: 'season', show_title: show.value.trim(), season: s, order: order.value }]);
        return true;
      },
    });
  }

  async function folderDialog() {
    const space = h('select', { class: 'select', required: true }, h('option', { value: '' }, 'Loading spaces…'));
    const path = h('input', { class: 'input', maxlength: '4096', autocomplete: 'off', placeholder: 'Movies/Classics', spellcheck: 'false' });
    const rec = h('input', { type: 'checkbox', class: 'checkbox', checked: true });
    const order = ui.segmented({ options: [{ value: 'default', label: 'In order' }, { value: 'random', label: 'Shuffled' }], value: 'default', size: 'sm', ariaLabel: 'Order' });
    formModal({
      title: 'Add a folder',
      fields: [labeled('Space', space), labeled('Folder path', path, 'Relative to the space root; leave empty for the whole space.'),
        h('label', { class: 'check-row' }, rec, h('span', {}, 'Include subfolders')), labeled('Order', order.el)],
      onSubmit: async () => {
        if (!space.value) throw new Error('Choose a space.');
        await addItems([{ kind: 'folder', space_id: space.value, path: path.value.trim().replace(/^\/+|\/+$/g, ''), recursive: rec.checked, order: order.value }]);
        return true;
      },
    });
    try {
      const spaces = await api.spaces(signal);
      const arr = Array.isArray(spaces) ? spaces : spaces?.items || [];
      space.replaceChildren(...arr.map((s) => h('option', { value: s.id }, s.kind === 'home' ? 'My files' : s.name)));
    } catch (err) {
      space.replaceChildren(h('option', { value: '' }, 'Could not load spaces'));
    }
  }

  async function queryDialog() {
    const cat = h('select', { class: 'select' }, h('option', { value: 'movie,tv' }, 'Movies & TV'), h('option', { value: 'movie' }, 'Movies'), h('option', { value: 'tv' }, 'TV'));
    const gid = uid('genres');
    const genre = h('input', { class: 'input', maxlength: '200', list: gid, autocomplete: 'off', placeholder: 'Any genre' });
    const gdl = h('datalist', { id: gid });
    const tag = h('input', { class: 'input', maxlength: '200', autocomplete: 'off', placeholder: 'Any tag' });
    const year = h('input', { class: 'input', type: 'number', min: '1800', max: '3000', inputmode: 'numeric', placeholder: 'Any year' });
    const order = ui.segmented({ options: [{ value: 'default', label: 'By title' }, { value: 'random', label: 'Shuffled' }], value: 'random', size: 'sm', ariaLabel: 'Order' });
    const limit = h('input', { class: 'input', type: 'number', min: '1', max: '100000', inputmode: 'numeric', placeholder: 'All' });
    formModal({
      title: 'Add a smart query',
      fields: [labeled('Category', cat), labeled('Genre', h('div', {}, genre, gdl)),
        h('div', { class: 'grid-2' }, labeled('Tag', tag), labeled('Year', year)),
        h('div', { class: 'grid-2' }, labeled('Order', order.el), labeled('Limit', limit, 'Maximum programs'))],
      onSubmit: async () => {
        const q = { category: cat.value };
        if (genre.value.trim()) q.genre = genre.value.trim();
        if (tag.value.trim()) q.tag = tag.value.trim();
        if (year.value) q.year = Number(year.value);
        const it = { kind: 'query', query: q, order: order.value };
        if (limit.value) it.limit = Number(limit.value);
        await addItems([it]);
        return true;
      },
    });
    try {
      const f = await api.facets(signal);
      gdl.replaceChildren(...(f.genres || []).map((g) => h('option', { value: g.value })));
    } catch { /* optional */ }
  }

  async function rename() {
    const name = await ui.prompt({ title: 'Rename lineup', label: 'Name', value: lineup.name, confirmLabel: 'Rename',
      validate: (v) => (!v.trim() ? 'Enter a name' : v.length > 200 ? 'At most 200 characters' : null) });
    if (name === null) return;
    try {
      lineup = await api.updateLineup(lineup.id, { name });
      titleEl.textContent = lineup.name;
      ctx.setTitle(lineup.name);
      ui.toast.success('Lineup renamed');
    } catch (err) {
      ui.toast.error(err);
    }
  }

  async function remove(force = false) {
    if (!force && !(await ui.confirm({ title: `Delete “${lineup.name}”?`, message: 'The lineup is deleted. Your media files are not touched.', confirmLabel: 'Delete', danger: true }))) return;
    try {
      await api.deleteLineup(lineup.id, force);
      ui.toast.success('Lineup deleted');
      navigate('/tv/lineups');
    } catch (err) {
      if (err?.status === 409 && !force) {
        const n = err.details?.channels?.length || 0;
        const ok = await ui.confirm({
          title: 'Channels use this lineup',
          message: `${plural(n, 'channel')} play this lineup. Deleting it deletes ${n === 1 ? 'that channel' : 'those channels'} too.`,
          confirmLabel: `Delete lineup and ${plural(n, 'channel')}`, danger: true,
        });
        if (ok) remove(true);
      } else {
        ui.toast.error(err);
      }
    }
  }

  async function createChannel() {
    const { openChannelDialog } = await import('./channels.js');
    const ch = await openChannelDialog(vctx, { lineupId: lineup.id, name: lineup.name });
    if (ch) {
      ui.toast.success(`Channel ${ch.number} created`, { action: { label: 'Watch', onClick: () => navigate(`/tv/watch/${ch.id}`) } });
    }
  }

  ctx.setTitle(lineup.name);
  renderItems();
  await loadPreview();
  return null;
}

