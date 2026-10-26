// Files app — trash view: restore (with "keep both" on conflicts), delete forever, empty.

import { h } from '../../core/dom.js';
import { icon } from '../../core/icons.js';
import { formatBytes, plural } from '../../core/format.js';
import { clickSelect, fileKind, KIND_ICONS, parentPath } from './model.js';

/**
 * Render the trash into host.
 * @returns {{destroy: () => void, reload: () => void}}
 */
export function renderTrash(host, ctx, fapi, { spaceName, onChanged }) {
  const { ui } = ctx;
  let items = [];
  let sel = new Set();
  let anchor = null;
  const rows = new Map();
  const tbody = h('tbody');
  const countEl = h('span', { class: 'fx-bulk-count' });
  const restoreBtn = ui.button({ label: 'Restore', icon: 'restore', size: 'sm', onClick: () => restore([...sel]) });
  const purgeBtn = ui.button({ label: 'Delete forever', icon: 'trash', size: 'sm', variant: 'danger-ghost', onClick: () => purge([...sel]) });
  const emptyBtn = ui.button({ label: 'Empty trash', icon: 'trash', size: 'sm', variant: 'danger', onClick: () => emptyAll() });
  const selBar = h('div', { class: 'fx-trash-sel', hidden: true }, countEl, restoreBtn, purgeBtn,
    ui.button({ icon: 'x', title: 'Clear selection', size: 'sm', variant: 'ghost', onClick: () => setSel(new Set()) }));
  const head = h('div', { class: 'fx-trash-head' },
    h('div', { class: 'fx-trash-heading' },
      h('h2', { class: 'fx-trash-title' }, icon('trash', 18), 'Trash'),
      h('p', { class: 'small muted' }, 'Deleted items stay here until you delete them forever or the retention period set by your administrator ends.')),
    h('div', { class: 'row' }, selBar, emptyBtn));
  const allBox = h('input', { type: 'checkbox', class: 'checkbox', aria: { label: 'Select all' } });
  const table = h('table', { class: 'table fx-table fx-trash-table' },
    h('thead', {}, h('tr', {},
      h('th', { class: 'td-check' }, allBox),
      h('th', {}, 'Name'), h('th', { class: 'fx-col-loc' }, 'Original location'), h('th', { class: 'align-right fx-col-size' }, 'Size'), h('th', { class: 'fx-col-mod' }, 'Deleted'))),
    tbody);
  const emptyEl = h('div', { class: 'fx-empty', hidden: true }, ui.emptyState({ icon: 'trash', title: 'Trash is empty', message: 'Items you delete appear here so you can restore them.' }));
  // Whether the last page fetched was the final one. infiniteList only updates its own `done`
  // after onItems returns, so reading list.done there left the empty state hidden after a
  // reload (e.g. "Empty trash") or a first load of an empty trash.
  let exhausted = false;
  const list = ui.infiniteList({
    fetchPage: async (cursor) => {
      const page = await fapi.trashList(cursor, ctx.signal);
      exhausted = !page?.next_cursor;
      return page;
    },
    onItems: (page, { reset }) => {
      if (reset) {
        items = [];
        rows.clear();
        tbody.replaceChildren();
      }
      for (const e of page) addRow(e);
      items.push(...page);
      sync();
    },
  });
  const scroller = h('div', { class: 'fx-main fx-trash-main', tabindex: '-1' }, h('div', { class: 'table-wrap fx-table-wrap' }, table), emptyEl, list.el);
  host.replaceChildren(h('div', { class: 'fx-trash' }, head, scroller));
  list.loadMore();

  function addRow(e) {
    const k = fileKind(e);
    const box = h('input', { type: 'checkbox', class: 'checkbox', aria: { label: `Select ${e.name}` } });
    const tr = h('tr', { class: 'clickable', dataset: { id: e.id }, aria: { selected: 'false' } },
      h('td', { class: 'td-check' }, box),
      h('td', {}, h('div', { class: 'cell-main' }, h('span', { class: `fx-icon fx-kind-${k}` }, icon(KIND_ICONS[k], 18)),
        h('div', { class: 'cell-stack' }, h('span', { class: 'cell-title truncate' }, e.name),
          h('span', { class: 'cell-sub fx-subline' }, `${formatBytes(e.size)} · ${parentLocation(e)}`)))),
      h('td', { class: 'fx-col-loc' }, h('span', { class: 'muted truncate fx-loc' }, parentLocation(e))),
      h('td', { class: 'align-right nums nowrap fx-col-size' }, e.is_dir ? '—' : formatBytes(e.size)),
      h('td', { class: 'nowrap fx-col-mod' }, ui.timeAgo(e.trashed_at)));
    tr.addEventListener('click', (ev) => {
      const ids = items.map((x) => x.id);
      const idx = ids.indexOf(e.id);
      const onBox = ev.target === box;
      const r = clickSelect({ ids, selected: sel, anchor }, idx, { toggle: onBox || ev.ctrlKey || ev.metaKey, range: ev.shiftKey });
      anchor = r.anchor;
      setSel(r.selected);
    });
    tr.addEventListener('contextmenu', (ev) => {
      if (!sel.has(e.id)) setSel(new Set([e.id]));
      ui.contextMenu(ev, [
        { label: 'Restore', icon: 'restore', onSelect: () => restore([...sel]) },
        { separator: true },
        { label: 'Delete forever', icon: 'trash', danger: true, onSelect: () => purge([...sel]) },
      ]);
    });
    rows.set(e.id, { tr, box });
    tbody.append(tr);
  }

  function parentLocation(e) {
    return [spaceName(e.space_id), ...parentPath(e.original_path).split('/')].filter(Boolean).join(' › ');
  }

  function setSel(next) {
    sel = next;
    for (const [id, r] of rows) {
      const on = sel.has(id);
      r.tr.classList.toggle('is-selected', on);
      r.tr.setAttribute('aria-selected', String(on));
      r.box.checked = on;
    }
    sync();
  }

  function sync() {
    selBar.hidden = sel.size === 0;
    countEl.textContent = `${sel.size} selected`;
    allBox.checked = items.length > 0 && sel.size === items.length;
    allBox.indeterminate = sel.size > 0 && sel.size < items.length;
    emptyEl.hidden = items.length > 0 || !exhausted;
    table.hidden = items.length === 0;
    emptyBtn.disabled = items.length === 0;
  }

  allBox.addEventListener('change', () => setSel(allBox.checked ? new Set(items.map((e) => e.id)) : new Set()));

  function drop(ids) {
    for (const id of ids) {
      rows.get(id)?.tr.remove();
      rows.delete(id);
      sel.delete(id);
    }
    items = items.filter((e) => !ids.includes(e.id));
    sync();
  }

  async function restore(ids) {
    const done = [];
    const conflicts = [];
    for (const id of ids) {
      try {
        await fapi.restore(id);
        done.push(id);
      } catch (err) {
        if (err.code === 'conflict') conflicts.push(id);
        else ui.toast.error(err);
      }
    }
    if (conflicts.length) {
      const ok = await ui.confirm({
        title: conflicts.length === 1 ? 'Name already taken' : `${conflicts.length} names already taken`,
        message: 'Something with the same name now exists at the original location. Restore under a new name like “name (1)”?',
        confirmLabel: 'Restore with new name',
      });
      if (ok) {
        for (const id of conflicts) {
          try {
            await fapi.restore(id, true);
            done.push(id);
          } catch (err) {
            ui.toast.error(err);
          }
        }
      }
    }
    if (done.length) {
      drop(done);
      ui.toast.success(`Restored ${plural(done.length, 'item')}`);
      onChanged?.();
    }
  }

  async function purge(ids) {
    const ok = await ui.confirm({
      title: ids.length === 1 ? 'Delete forever?' : `Delete ${ids.length} items forever?`,
      message: 'This permanently removes the files from the server. It cannot be undone.',
      confirmLabel: 'Delete forever', danger: true,
    });
    if (!ok) return;
    const done = [];
    for (const id of ids) {
      try {
        await fapi.purge(id);
        done.push(id);
      } catch (err) {
        ui.toast.error(err);
      }
    }
    drop(done);
    if (done.length) {
      ui.toast.success(`Deleted ${plural(done.length, 'item')} forever`);
      onChanged?.();
    }
  }

  async function emptyAll() {
    const ok = await ui.confirm({
      title: 'Empty the trash?', message: 'Everything in your trash will be permanently deleted. This cannot be undone.',
      confirmLabel: 'Empty trash', danger: true,
    });
    if (!ok) return;
    try {
      const r = await fapi.emptyTrash();
      ui.toast.success(`Freed ${formatBytes(r.freed_bytes)}`, { description: `${plural(r.deleted, 'item')} deleted forever` });
      list.reload();
      onChanged?.();
    } catch (err) {
      ui.toast.error(err);
    }
  }

  scroller.addEventListener('keydown', (e) => {
    if ((e.key === 'a' || e.key === 'A') && (e.ctrlKey || e.metaKey)) {
      e.preventDefault();
      setSel(new Set(items.map((x) => x.id)));
    } else if (e.key === 'Delete' && sel.size) {
      e.preventDefault();
      purge([...sel]);
    } else if (e.key === 'Escape' && sel.size) {
      setSel(new Set());
    }
  });

  return {
    destroy: () => list.destroy(),
    reload: () => list.reload(),
  };
}

