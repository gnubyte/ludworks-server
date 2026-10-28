// Shared Kanban widgets: label chips, avatars, due badges, color swatches, an anchored
// popover (works above modals), the member search and the Files picker / save dialogs.

import { h, cls, on, uid, debounce, mount as fill } from '../../core/dom.js';
import { icon } from '../../core/icons.js';
import { avatar, button, emptyState, errorState, loadingBlock, modal, toast } from '../../core/ui.js';
import { formatBytes, formatDateTime } from '../../core/format.js';
import { kicon } from './icons.js';
import { COLORS, COLOR_NAMES, colorClass, dueLabel, dueState, memberName } from './model.js';

/* ------------------------------------------------------------ small pieces */

/**
 * A label chip; compact chips are color bars without text (tooltip shows the name).
 * @param {{name: string, color: string}} label
 */
export function labelChip(label, { compact = false } = {}) {
  return h('span', {
    class: cls('kb-label', colorClass(label.color), { 'is-compact': compact }),
    dataset: compact ? { tip: label.name } : undefined,
    aria: compact ? { label: label.name } : undefined,
  }, compact ? null : h('span', { class: 'kb-label-text' }, label.name));
}

/** Member avatar (initials with a stable hue) with the name as tooltip. */
export function memberAvatar(member, size = 24) {
  const name = memberName(member);
  return h('span', { class: 'kb-avatar', dataset: { tip: name }, aria: { label: name }, role: 'img' }, avatar({ name, size }));
}

/**
 * Overlapping avatars of user ids (+N for the rest).
 * @param {string[]} ids
 * @param {Map<string, object>} members by user id
 */
export function avatarStack(ids, members, { max = 3, size = 22 } = {}) {
  const shown = ids.slice(0, max);
  const rest = ids.length - shown.length;
  return h('span', { class: 'kb-avatars' },
    shown.map((id) => memberAvatar(members.get(id) || { username: 'Unknown user' }, size)),
    rest > 0 ? h('span', { class: 'kb-avatar-more', dataset: { tip: ids.slice(max).map((id) => memberName(members.get(id))).join(', ') } }, `+${rest}`) : null);
}

/** Due-date badge (overdue red, today amber, completed green). */
export function dueBadge(card, now = Date.now()) {
  const state = dueState(card, now);
  if (!state) return null;
  const tip = { overdue: 'Overdue', today: 'Due today', done: 'Completed', soon: 'Due soon', later: 'Due' }[state];
  return h('span', {
    class: cls('kb-due', `is-${state}`),
    dataset: { tip: `${tip} · ${card.all_day ? new Date(card.due_at).toLocaleDateString() : formatDateTime(card.due_at)}` },
  }, icon(state === 'done' ? 'check-circle' : 'clock', 12), h('span', {}, dueLabel(card, now)));
}

/** Small icon + number badge (comments, attachments, subtasks). */
export function countBadge(name, text, tip) {
  return h('span', { class: 'kb-badge', dataset: { tip } }, kicon(name, 13), h('span', {}, String(text)));
}

/**
 * Color swatches (radiogroup) for labels, boards and columns.
 * @param {{value: string|null, onChange: (c: string|null) => void, allowNone?: boolean, label?: string}} o
 */
export function colorSwatches({ value, onChange, allowNone = false, label = 'Color' }) {
  const el = h('div', { class: 'kb-swatches', role: 'radiogroup', aria: { label } });
  const options = allowNone ? [null, ...COLORS] : COLORS;
  let current = value;
  const btns = options.map((c) => {
    const b = h('button', {
      type: 'button', role: 'radio', class: cls('kb-swatch', c ? colorClass(c) : 'kb-swatch-none'),
      aria: { label: c ? COLOR_NAMES[c] : 'No color' }, dataset: { tip: c ? COLOR_NAMES[c] : 'No color' },
    }, c ? null : icon('ban', 14));
    b.addEventListener('click', () => {
      set(c);
      onChange?.(c);
    });
    el.append(b);
    return b;
  });
  function set(c) {
    current = c;
    btns.forEach((b, i) => {
      const on = options[i] === current;
      b.setAttribute('aria-checked', String(on));
      b.tabIndex = on || (current === undefined && i === 0) ? 0 : -1;
    });
    if (!btns.some((b) => b.tabIndex === 0)) btns[0].tabIndex = 0;
  }
  el.addEventListener('keydown', (e) => {
    const i = btns.indexOf(document.activeElement);
    if (i < 0) return;
    const d = { ArrowRight: 1, ArrowDown: 1, ArrowLeft: -1, ArrowUp: -1 }[e.key];
    if (!d) return;
    e.preventDefault();
    const j = (i + d + btns.length) % btns.length;
    btns[j].focus();
    btns[j].click();
  });
  set(value);
  return { el, set, get value() { return current; } };
}

/* ----------------------------------------------------------------- popover */

let openPop = null;

/**
 * Open an anchored, non-modal panel (label picker, member picker, dates…). It lives on
 * <body> above modals, keeps Tab inside, and closes on Escape or a click outside.
 * @param {{anchor: HTMLElement, content: Node, className?: string, align?: 'start'|'end',
 *          onClose?: () => void, label?: string, focus?: boolean}} o
 * @returns {{el: HTMLElement, close: () => void, reposition: () => void}}
 */
export function popover({ anchor, content, className = '', align = 'start', onClose = null, label = '', focus = true }) {
  openPop?.close();
  const el = h('div', { class: cls('kb-pop', className), role: 'dialog', aria: { label: label || undefined, modal: 'false' }, tabindex: '-1' }, content);
  document.body.appendChild(el);
  const offs = [];
  const api = {
    el,
    close(restore = true) {
      if (openPop !== api) return;
      openPop = null;
      offs.forEach((off) => off());
      el.remove();
      anchor?.setAttribute('aria-expanded', 'false');
      if (restore) {
        // The anchor may have been re-rendered meanwhile: fall back to its replacement.
        const label = anchor?.getAttribute('aria-label');
        const target = anchor?.isConnected ? anchor : label ? document.querySelector(`[aria-label="${CSS.escape(label)}"]`) : null;
        target?.focus({ preventScroll: true });
      }
      onClose?.();
    },
    reposition() {
      const r = anchor.getBoundingClientRect();
      const vw = document.documentElement.clientWidth;
      const vh = window.innerHeight;
      const w = el.offsetWidth;
      const ph = el.offsetHeight;
      let left = align === 'end' ? r.right - w : r.left;
      let top = r.bottom + 6;
      if (top + ph > vh - 8 && r.top - ph - 6 > 8) top = r.top - ph - 6;
      left = Math.max(8, Math.min(left, vw - w - 8));
      top = Math.max(8, Math.min(top, vh - ph - 8));
      el.style.left = `${left}px`;
      el.style.top = `${top}px`;
    },
  };
  anchor?.setAttribute('aria-expanded', 'true');
  api.reposition();
  el.addEventListener('keydown', (e) => {
    if (e.key === 'Escape') {
      e.preventDefault();
      e.stopPropagation();
      api.close(true);
    } else if (e.key === 'Tab') {
      const items = [...el.querySelectorAll('a[href],button:not([disabled]),input:not([disabled]),select:not([disabled]),textarea:not([disabled]),[tabindex]:not([tabindex="-1"])')]
        .filter((x) => x.getClientRects().length > 0);
      e.stopPropagation(); // the modal focus trap would pull focus back into the dialog
      if (!items.length) { e.preventDefault(); return; }
      const first = items[0];
      const last = items[items.length - 1];
      if (e.shiftKey && document.activeElement === first) { e.preventDefault(); last.focus(); }
      else if (!e.shiftKey && document.activeElement === last) { e.preventDefault(); first.focus(); }
    }
  });
  setTimeout(() => {
    if (openPop !== api) return;
    offs.push(on(document, 'pointerdown', (e) => {
      if (!el.contains(e.target) && !anchor?.contains(e.target) && !e.target.closest?.('.menu')) api.close(false);
    }, true));
    offs.push(on(window, 'resize', () => api.reposition()));
  }, 0);
  openPop = api;
  if (focus) {
    requestAnimationFrame(() => {
      const first = el.querySelector('input,select,textarea,button:not([disabled])');
      (first || el).focus({ preventScroll: true });
    });
  }
  return api;
}

/** Close the open popover, if any. */
export function closePopover() {
  openPop?.close(false);
}

/* ------------------------------------------------------------- user search */

/**
 * Member search over GET /kanban/users: an input with results; `onPick(user)` per choice.
 * @param {{kapi: object, exclude?: () => Set<string>, onPick: (u: object) => void, placeholder?: string}} o
 */
export function userSearch({ kapi, exclude = () => new Set(), onPick, placeholder = 'Search people by name…' }) {
  const input = h('input', { class: 'input', type: 'search', placeholder, autocomplete: 'off', spellcheck: 'false', aria: { label: 'Search people' } });
  const results = h('div', { class: 'kb-user-results', role: 'listbox', aria: { label: 'People' } });
  let seq = 0;
  const run = debounce(async () => {
    const q = input.value.trim();
    const my = ++seq;
    if ([...q].length < 2) {
      fill(results, q ? h('div', { class: 'faint small kb-hint' }, 'Type at least two letters.') : null);
      return;
    }
    try {
      const users = await kapi.users(q);
      if (my !== seq) return;
      const skip = exclude();
      const list = users.filter((u) => !skip.has(u.id));
      fill(results, ...(list.length ? list.map((u) => {
        const b = h('button', { type: 'button', class: 'kb-user-result', role: 'option' },
          avatar({ name: u.display_name || u.username, size: 24 }),
          h('span', { class: 'kb-user-names' }, h('span', { class: 'truncate' }, u.display_name || u.username),
            u.display_name ? h('span', { class: 'faint small truncate' }, `@${u.username}`) : null));
        b.addEventListener('click', () => {
          onPick(u);
          input.value = '';
          results.replaceChildren();
          input.focus();
        });
        return b;
      }) : [h('div', { class: 'faint small kb-hint' }, 'Nobody found.')]));
    } catch (err) {
      if (my === seq) fill(results, h('div', { class: 'danger-text small kb-hint' }, err.message || 'Search failed'));
    }
  }, 200);
  input.addEventListener('input', run);
  input.addEventListener('keydown', (e) => {
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      results.querySelector('button')?.focus();
    } else if (e.key === 'Enter') {
      e.preventDefault();
      results.querySelector('button')?.click();
    }
  });
  results.addEventListener('keydown', (e) => {
    const items = [...results.querySelectorAll('button')];
    const i = items.indexOf(document.activeElement);
    if (e.key === 'ArrowDown' && i < items.length - 1) { e.preventDefault(); items[i + 1].focus(); }
    if (e.key === 'ArrowUp') { e.preventDefault(); (i > 0 ? items[i - 1] : input).focus(); }
  });
  return { el: h('div', { class: 'kb-user-search stack-xs' }, input, results), input };
}

/* -------------------------------------------------------------- Files picker */

/**
 * Pick a file from Files (browse spaces and folders, or search). Resolves the Entry or null.
 * @param {object} ctx app context
 */
export function filesPicker(ctx, { title = 'Attach from Files', description = 'Choose a file (up to 50 MiB). It is copied to the card; the original stays in Files.', confirmLabel = 'Attach' } = {}) {
  const { api } = ctx;
  let spaces = [];
  let space = null;
  let path = '';
  let chosen = null;
  let seq = 0;
  const list = h('div', { class: 'kb-picker-list', role: 'listbox', aria: { label: 'Files' } });
  const crumbs = h('div', { class: 'kb-picker-crumbs' });
  const spaceSel = h('select', { class: 'select kb-picker-space', aria: { label: 'Space' } });
  const search = h('input', { class: 'input', type: 'search', placeholder: 'Search files…', aria: { label: 'Search files' } });

  const row = (entry) => {
    const b = h('button', { type: 'button', class: cls('kb-picker-item', { 'is-dir': entry.is_dir }), role: 'option', aria: { selected: 'false' } },
      icon(entry.is_dir ? 'folder' : 'file', 16),
      h('span', { class: 'kb-picker-name truncate' }, entry.name),
      entry.is_dir ? icon('chevron-right', { size: 14, className: 'faint' }) : h('span', { class: 'faint small nums' }, formatBytes(entry.size)));
    b.addEventListener('click', () => {
      if (entry.is_dir) { path = entry.path; search.value = ''; load(); return; }
      chosen = entry;
      for (const x of list.querySelectorAll('[aria-selected="true"]')) x.setAttribute('aria-selected', 'false');
      b.setAttribute('aria-selected', 'true');
      m.buttons[1].disabled = entry.size > 50 * 1024 * 1024;
      m.buttons[1].dataset.locked = m.buttons[1].disabled ? '1' : '';
      if (m.buttons[1].disabled) toast.warning('That file is larger than 50 MiB.');
    });
    b.addEventListener('dblclick', () => { if (!entry.is_dir && entry.size <= 50 * 1024 * 1024) m.close(entry); });
    return b;
  };

  async function load() {
    const my = ++seq;
    chosen = null;
    m.buttons[1].disabled = true;
    list.replaceChildren(loadingBlock('Loading…'));
    try {
      let items;
      const q = search.value.trim();
      if (q) {
        const res = await api.get('/files/search', { query: { q, type: 'file', limit: 100 }, signal: ctx.signal });
        items = res.items || [];
        crumbs.replaceChildren(h('span', { class: 'muted small' }, `Results for “${q}”`));
      } else {
        const res = await api.get('/files', { query: { space_id: space, path, sort: 'name', limit: 200 }, signal: ctx.signal });
        items = res.items || [];
        const parts = path ? path.split('/') : [];
        crumbs.replaceChildren(
          h('button', { type: 'button', class: 'link-button', onClick: () => { path = ''; load(); } }, spaces.find((s) => s.id === space)?.name || 'Root'),
          ...parts.flatMap((p, i) => [h('span', { class: 'faint' }, ' / '),
            i === parts.length - 1 ? h('span', {}, p) : h('button', { type: 'button', class: 'link-button', onClick: () => { path = parts.slice(0, i + 1).join('/'); load(); } }, p)]));
      }
      if (my !== seq) return;
      list.replaceChildren(...(items.length ? items.map(row) : [emptyState({ icon: 'folder', title: q ? 'No matching files' : 'This folder is empty', compact: true })]));
    } catch (err) {
      if (my !== seq || err?.name === 'AbortError') return;
      list.replaceChildren(errorState(err, load));
    }
  }

  const m = modal({
    title, description, size: 'lg', className: 'kb-picker',
    body: h('div', { class: 'stack-sm' }, h('div', { class: 'row kb-picker-bar' }, spaceSel, search), crumbs, list),
    footer: [{ label: 'Cancel', value: null }, { label: confirmLabel, variant: 'primary', disabled: true, onClick: (mm) => mm.close(chosen) }],
  });
  spaceSel.addEventListener('change', () => { space = spaceSel.value; path = ''; load(); });
  const typed = debounce(load, 250);
  search.addEventListener('input', typed);
  (async () => {
    try {
      const res = await api.get('/spaces', { signal: ctx.signal });
      spaces = Array.isArray(res) ? res : res?.items || [];
      spaceSel.replaceChildren(...spaces.map((s) => h('option', { value: s.id }, s.kind === 'home' ? `${s.name} (Home)` : s.name)));
      space = spaces[0]?.id || null;
      if (!space) {
        list.replaceChildren(emptyState({ icon: 'folder', title: 'No spaces available', compact: true }));
        return;
      }
      load();
    } catch (err) {
      list.replaceChildren(errorState(err, null));
    }
  })();
  return m.closed.then((v) => (v && v.id ? v : null));
}

/**
 * Ask where to save an attachment in Files. Resolves {space_id, path, name, overwrite} or null.
 * @param {object} ctx
 * @param {{filename: string}} att
 */
export function saveToFilesDialog(ctx, att) {
  const { api } = ctx;
  const spaceSel = h('select', { class: 'select', id: uid('kbs'), aria: { label: 'Space' } });
  const pathIn = h('input', { class: 'input', id: uid('kbp'), placeholder: 'Folder (optional), e.g. Projects/Kanban', autocomplete: 'off' });
  const nameIn = h('input', { class: 'input', id: uid('kbn'), value: att.filename, autocomplete: 'off', required: true });
  const over = h('input', { type: 'checkbox', class: 'checkbox' });
  const body = h('div', { class: 'form-grid' },
    h('div', { class: 'field' }, h('label', { class: 'field-label', for: spaceSel.id }, 'Space'), spaceSel),
    h('div', { class: 'field' }, h('label', { class: 'field-label', for: pathIn.id }, 'Folder'), pathIn,
      h('div', { class: 'field-hint' }, 'Missing folders are created.')),
    h('div', { class: 'field' }, h('label', { class: 'field-label', for: nameIn.id }, 'File name'), nameIn),
    h('label', { class: 'check-row' }, over, h('span', { class: 'check-text' }, h('span', { class: 'check-label' }, 'Replace a file with the same name'),
      h('span', { class: 'check-hint' }, 'The replaced version goes to the trash.'))));
  api.get('/spaces', { signal: ctx.signal }).then((res) => {
    const spaces = (Array.isArray(res) ? res : res?.items || []).filter((s) => s.access !== 'read');
    spaceSel.replaceChildren(...spaces.map((s) => h('option', { value: s.id }, s.kind === 'home' ? `${s.name} (Home)` : s.name)));
  }).catch((err) => toast.error(err));
  const m = modal({
    title: 'Save to Files', description: `Copy “${att.filename}” into your files.`, size: 'sm', body,
    footer: [{ label: 'Cancel', value: null }, { label: 'Save', variant: 'primary', type: 'submit' }],
    onSubmit: () => {
      if (!nameIn.value.trim()) return false;
      return { space_id: spaceSel.value || undefined, path: pathIn.value.trim().replace(/^\/+|\/+$/g, ''), name: nameIn.value.trim(), overwrite: over.checked };
    },
  });
  return m.closed.then((v) => v || null);
}

/** A labelled form field. */
export function field(labelText, control, hint = '') {
  if (!control.id) control.id = uid('kbf');
  return h('div', { class: 'field' }, h('label', { class: 'field-label', for: control.id }, labelText), control,
    hint ? h('div', { class: 'field-hint' }, hint) : null);
}

/** Icon button helper (square, tooltip). */
export function iconButton(name, title, onClick, { variant = 'ghost', size = 'sm', className = '' } = {}) {
  const b = button({ variant, size, className: cls('btn-icon', className), ariaLabel: title, onClick });
  b.replaceChildren(kicon(name, size === 'sm' ? 14 : 16));
  b.dataset.tip = title;
  return b;
}
