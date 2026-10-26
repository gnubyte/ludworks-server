// Files app — dialogs: destination picker (move/copy across spaces), conflict
// resolution, rename, new folder and categorize/tag.

import { h, uid } from '../../core/dom.js';
import { icon } from '../../core/icons.js';
import { plural } from '../../core/format.js';
import { createFolderTree } from './tree.js';
import { nameProblem, splitExt, parseTags, joinPath, isWithin } from './model.js';

/**
 * Pick a destination folder in any writable space.
 * @returns {Promise<{spaceId: string, path: string}|null>}
 */
export function pickDestination(ctx, fapi, { title, confirmLabel, spaces, initialSpace, initialPath = '', sources = [] }) {
  const { ui } = ctx;
  const writable = spaces.filter((s) => s.access !== 'read');
  const where = h('div', { class: 'fx-picker-where' });
  const warn = h('div', { class: 'field-error', role: 'alert', hidden: true });
  let sel = { spaceId: writable.some((s) => s.id === initialSpace) ? initialSpace : writable[0]?.id || '', path: writable.some((s) => s.id === initialSpace) ? initialPath : '' };
  const tree = createFolderTree({ fapi, label: 'Destination folder', onSelect: (spaceId, path) => choose(spaceId, path), onError: (e) => ui.toast.error(e), signal: ctx.signal });
  tree.setSpaces(writable);
  let m;
  function invalid() {
    // A folder cannot be moved into itself or below itself.
    return sources.some((s) => s.is_dir && s.space_id === sel.spaceId && isWithin(sel.path, s.path));
  }
  function choose(spaceId, path) {
    sel = { spaceId, path };
    tree.setActive(spaceId, path, { reveal: false });
    const sp = writable.find((s) => s.id === spaceId);
    where.replaceChildren(icon(sp?.kind === 'home' ? 'home' : 'library', 14),
      h('span', { class: 'truncate' }, [sp?.kind === 'home' ? 'Home' : sp?.name || '', ...path.split('/').filter(Boolean)].join(' › ')));
    const bad = invalid();
    warn.hidden = !bad;
    warn.textContent = bad ? 'A folder cannot be placed inside itself.' : '';
    if (m) m.buttons[m.buttons.length - 1].disabled = bad || !sel.spaceId;
  }
  const newFolder = ui.button({
    label: 'New folder', icon: 'plus', size: 'sm', variant: 'ghost',
    onClick: async () => {
      const name = await promptName(ctx, { title: 'New folder', label: 'Folder name', confirmLabel: 'Create' });
      if (!name) return;
      try {
        await fapi.mkdir(sel.spaceId, sel.path, name);
        await tree.refresh(sel.spaceId, sel.path);
        await tree.expand(sel.spaceId, sel.path);
        choose(sel.spaceId, joinPath(sel.path, name));
        await tree.setActive(sel.spaceId, sel.path);
      } catch (err) {
        ui.toast.error(err);
      }
    },
  });
  m = ui.modal({
    title, size: 'md', className: 'fx-picker',
    body: h('div', { class: 'stack-sm' },
      writable.length ? h('div', { class: 'fx-picker-tree' }, tree.el) : ui.emptyState({ icon: 'lock', title: 'No writable spaces', compact: true }),
      h('div', { class: 'row between' }, where, newFolder), warn),
    footer: [
      { label: 'Cancel', value: null },
      h('span', { class: 'spacer' }),
      { label: confirmLabel, variant: 'primary', onClick: (mm) => mm.close({ ...sel }) },
    ],
  });
  choose(sel.spaceId, sel.path);
  tree.setActive(sel.spaceId, sel.path);
  return m.closed.then((v) => v || null);
}

/**
 * Ask how to resolve name conflicts.
 * @returns {Promise<'overwrite'|'rename'|'skip'|null>}
 */
export function askConflict(ctx, { count, names = [], verb = 'move' }) {
  const { ui } = ctx;
  const list = names.slice(0, 5);
  const m = ui.modal({
    title: count === 1 ? 'An item with this name already exists' : `${count} items already exist`,
    size: 'sm',
    body: h('div', { class: 'stack-sm' },
      h('p', { class: 'confirm-msg' }, `The destination already contains ${count === 1 ? 'an item' : 'items'} with the same name. What should happen to ${count === 1 ? 'it' : 'them'}?`),
      list.length ? h('ul', { class: 'fx-conflict-list' }, list.map((n) => h('li', { class: 'truncate' }, n)), names.length > 5 ? h('li', { class: 'faint' }, `and ${names.length - 5} more`) : null) : null),
    footer: [
      { label: 'Skip', value: 'skip' },
      { label: 'Keep both', value: 'rename', variant: 'secondary' },
      { label: 'Replace', value: 'overwrite', variant: 'danger' },
    ],
  });
  return m.closed.then((v) => v || null);
}

/**
 * Prompt for a valid file/folder name.
 * @returns {Promise<string|null>}
 */
export function promptName(ctx, { title, label = 'Name', value = '', confirmLabel = 'Save', isDir = true }) {
  const p = ctx.ui.prompt({ title, label, value, confirmLabel, validate: (v) => nameProblem(v) || null });
  if (value && !isDir) {
    // Select the stem only (like desktop file managers) once the dialog has focused the input.
    requestAnimationFrame(() => requestAnimationFrame(() => {
      const input = document.querySelector('.modal input.input');
      if (input && input.value === value) {
        const [stem] = splitExt(value, false);
        input.setSelectionRange(0, stem.length);
      }
    }));
  }
  return p;
}

/**
 * Categorize/tag dialog for one or more entries.
 * @returns {Promise<{category?: string, subcategory?: string, add_tags?: string[], remove_tags?: string[]}|null>}
 */
export function categorizeDialog(ctx, { categories, entries }) {
  const { ui } = ctx;
  const single = entries.length === 1 ? entries[0] : null;
  const keep = '__keep__';
  const catSel = h('select', { class: 'select', id: uid('fxc') },
    entries.length > 1 ? h('option', { value: keep }, 'Keep current') : null,
    h('option', { value: '' }, 'None'),
    categories.map((c) => h('option', { value: c.slug }, c.name)));
  const subSel = h('select', { class: 'select', id: uid('fxs') });
  const addInput = h('input', { class: 'input', id: uid('fxt'), placeholder: 'e.g. family, 2024', autocomplete: 'off' });
  const rmInput = h('input', { class: 'input', id: uid('fxr'), placeholder: 'Tags to remove', autocomplete: 'off' });
  catSel.value = single ? single.category || '' : keep;
  const fillSubs = () => {
    const c = categories.find((x) => x.slug === catSel.value);
    subSel.replaceChildren(h('option', { value: '' }, c ? 'None' : '—'), ...(c?.subcategories || []).map((s) => h('option', { value: s.slug }, s.name)));
    subSel.disabled = !c;
    if (single && c && single.category === c.slug) subSel.value = single.subcategory || '';
  };
  catSel.addEventListener('change', fillSubs);
  fillSubs();
  const field = (labelText, ctl, hint) => h('div', { class: 'field' }, h('label', { class: 'field-label', for: ctl.id }, labelText), ctl, hint ? h('div', { class: 'field-hint' }, hint) : null);
  const m = ui.modal({
    title: entries.length === 1 ? `Categorize “${entries[0].name}”` : `Categorize ${plural(entries.length, 'item')}`,
    size: 'sm',
    body: h('div', { class: 'stack-md' },
      h('div', { class: 'grid-2' }, field('Category', catSel), field('Subcategory', subSel)),
      field('Add tags', addInput, 'Separate tags with commas.'),
      field('Remove tags', rmInput)),
    footer: [{ label: 'Cancel', value: null }, { label: 'Apply', variant: 'primary', type: 'submit' }],
    onSubmit: () => {
      const out = {};
      if (catSel.value !== keep) {
        out.category = catSel.value;
        out.subcategory = catSel.value ? subSel.value : '';
      }
      const add = parseTags(addInput.value);
      const rm = parseTags(rmInput.value);
      if (add.length) out.add_tags = add;
      if (rm.length) out.remove_tags = rm;
      return Object.keys(out).length ? out : null;
    },
  });
  return m.closed.then((v) => v || null);
}
