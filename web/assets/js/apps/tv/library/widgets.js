// TV library — form widgets shared by the editor, bulk editor, auto-tag and picker:
// genre chips, category/subcategory selects and a space + folder picker.

import { h, cls, uid, mount } from '../../../core/dom.js';
import { icon } from '../../../core/icons.js';
import { normalizeGenres } from './model.js';

/**
 * Chip editor for genres: type and press Enter or comma; Backspace removes the last chip.
 * @param {{value?: string[], suggestions?: string[], label?: string}} o
 * @returns {{el: HTMLElement, input: HTMLInputElement, value: string[], set(v: string[]): void, onChange?: Function}}
 */
export function genreInput({ value = [], suggestions = [], label = 'Genres' } = {}) {
  let values = normalizeGenres(value);
  const listId = uid('genres');
  const input = h('input', { class: 'tvl-chip-input', type: 'text', list: listId, placeholder: 'Add genre…', aria: { label }, autocomplete: 'off', maxlength: '64' });
  const chips = h('span', { class: 'tvl-chip-list' });
  const datalist = h('datalist', { id: listId }, suggestions.map((s) => h('option', { value: s })));
  const el = h('div', { class: 'tvl-chip-editor input' }, chips, input, datalist);
  const api = {
    el, input,
    get value() { commit(); return values.slice(); },
    set(v) { values = normalizeGenres(v); render(); },
    onChange: null,
  };
  function render() {
    chips.replaceChildren(...values.map((g, i) => h('span', { class: 'chip chip-accent' },
      h('span', { class: 'chip-label' }, g),
      h('button', { class: 'chip-x', type: 'button', aria: { label: `Remove ${g}` }, onClick: () => { values.splice(i, 1); render(); api.onChange?.(); input.focus(); } }, icon('x', 12)))));
  }
  function commit() {
    const parts = input.value.split(',');
    if (parts.length && parts.some((p) => p.trim())) {
      values = normalizeGenres([...values, ...parts]);
      input.value = '';
      render();
      api.onChange?.();
    }
  }
  input.addEventListener('keydown', (e) => {
    if (e.key === 'Enter' || e.key === ',') {
      if (input.value.trim()) {
        e.preventDefault();
        commit();
      } else if (e.key === ',') e.preventDefault();
    } else if (e.key === 'Backspace' && !input.value && values.length) {
      values.pop();
      render();
      api.onChange?.();
    }
  });
  // Typed or pasted commas split immediately (also covers IMEs that send no keydown).
  input.addEventListener('input', () => { if (input.value.includes(',')) commit(); });
  input.addEventListener('change', () => { if (suggestions.includes(input.value.trim())) commit(); });
  input.addEventListener('blur', commit);
  el.addEventListener('click', (e) => { if (e.target === el || e.target === chips) input.focus(); });
  render();
  return api;
}

/**
 * Category + subcategory selects driven by GET /categories.
 * @returns {{category: HTMLSelectElement, subcategory: HTMLSelectElement, value: {category: string, subcategory: string}}}
 */
export function categorySelects(categories, { category = '', subcategory = '', allowNone = true } = {}) {
  const cat = h('select', { class: 'select', name: 'category' },
    allowNone ? h('option', { value: '' }, 'No category') : null,
    (categories || []).map((c) => h('option', { value: c.slug }, c.name)));
  const sub = h('select', { class: 'select', name: 'subcategory' });
  const fillSubs = (keep) => {
    const c = (categories || []).find((x) => x.slug === cat.value);
    sub.replaceChildren(h('option', { value: '' }, '—'), ...(c?.subcategories || []).map((s) => h('option', { value: s.slug }, s.name)));
    sub.value = keep && [...sub.options].some((o) => o.value === keep) ? keep : '';
    sub.disabled = !c;
  };
  cat.value = category;
  if (cat.value !== category && category) {
    cat.append(h('option', { value: category }, category));
    cat.value = category;
  }
  fillSubs(subcategory);
  cat.addEventListener('change', () => fillSubs(''));
  return {
    category: cat, subcategory: sub,
    get value() { return { category: cat.value, subcategory: sub.value }; },
  };
}

/**
 * Space + folder browser (folders only). onChange({spaceId, path}) fires on navigation.
 * @param {{api: object, ui: object, writableOnly?: boolean, spaceId?: string, path?: string, onChange?: Function}} o
 */
export function folderPicker({ api, ui, writableOnly = false, spaceId = '', path = '', onChange = null }) {
  const state = { spaces: [], spaceId, path, loading: false };
  const spaceSel = h('select', { class: 'select', aria: { label: 'Space' } });
  const crumbs = h('div', { class: 'tvl-fp-crumbs' });
  const list = h('div', { class: 'tvl-fp-list', role: 'listbox', aria: { label: 'Folders' }, tabindex: '-1' });
  const el = h('div', { class: 'tvl-fp' }, spaceSel, crumbs, list);
  let gen = 0;

  async function init() {
    try {
      const spaces = await api.spaces();
      state.spaces = spaces.filter((s) => !writableOnly || s.access !== 'read');
    } catch (err) {
      list.replaceChildren(h('div', { class: 'muted small' }, ui.errorMessage(err)));
      return;
    }
    spaceSel.replaceChildren(...state.spaces.map((s) => h('option', { value: s.id }, s.kind === 'home' ? 'Home' : s.name)));
    if (!state.spaces.some((s) => s.id === state.spaceId)) {
      state.spaceId = state.spaces[0]?.id || '';
      state.path = '';
    }
    spaceSel.value = state.spaceId;
    await open(state.path);
  }

  async function open(p) {
    const my = ++gen;
    state.path = p;
    renderCrumbs();
    onChange?.({ spaceId: state.spaceId, path: state.path });
    if (!state.spaceId) {
      list.replaceChildren(h('div', { class: 'muted small tvl-fp-empty' }, 'No space available.'));
      return;
    }
    list.replaceChildren(h('div', { class: 'tvl-fp-empty' }, ui.spinner({ size: 'sm' })));
    try {
      const res = await api.folders(state.spaceId, p);
      if (my !== gen) return;
      const dirs = (res.items || []).filter((e) => e.is_dir);
      list.replaceChildren(...(dirs.length ? dirs.map((d) => h('button', {
        type: 'button', class: 'tvl-fp-item', role: 'option', onClick: () => open(d.path),
      }, icon('folder', 16), h('span', { class: 'truncate' }, d.name), icon('chevron-right', { size: 14, className: 'tvl-fp-go' })))
        : [h('div', { class: 'muted small tvl-fp-empty' }, 'No subfolders')]));
    } catch (err) {
      if (my === gen) list.replaceChildren(h('div', { class: 'danger-text small tvl-fp-empty' }, ui.errorMessage(err)));
    }
  }

  function renderCrumbs() {
    const parts = state.path ? state.path.split('/') : [];
    const items = [{ label: 'Root', p: '' }, ...parts.map((name, i) => ({ label: name, p: parts.slice(0, i + 1).join('/') }))];
    mount(crumbs, items.map((it, i) => [
      i ? icon('chevron-right', { size: 12, className: 'crumb-sep' }) : null,
      i === items.length - 1
        ? h('span', { class: 'tvl-fp-crumb is-current', aria: { current: 'location' } }, it.label)
        : h('button', { type: 'button', class: 'tvl-fp-crumb', onClick: () => open(it.p) }, it.label),
    ]));
  }

  spaceSel.addEventListener('change', () => {
    state.spaceId = spaceSel.value;
    open('');
  });
  init();
  return {
    el,
    get value() { return { spaceId: state.spaceId, path: state.path, spaceName: state.spaces.find((s) => s.id === state.spaceId)?.name || '' }; },
  };
}

/** A simple labelled checkbox row (returns the label; .inputEl is the input). */
export function checkRow(label, checked = false, hint = '') {
  const inputEl = h('input', { type: 'checkbox', class: 'checkbox', checked });
  const el = h('label', { class: cls('check-row') }, inputEl,
    h('span', { class: 'check-text' }, h('span', { class: 'check-label' }, label), hint ? h('span', { class: 'check-hint' }, hint) : null));
  el.inputEl = inputEl;
  return el;
}
