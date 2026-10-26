// Command palette (Ctrl/Cmd+K): fuzzy search over registered commands — apps,
// pages and quick actions. Apps add their own commands while mounted.

import { h, cls, uid } from './dom.js';
import { icon } from './icons.js';
import { comboLabel } from './shortcuts.js';

/**
 * Fuzzy subsequence score (higher is better); null when not all query chars match
 * in order. Rewards prefix, word-start and consecutive matches.
 * @param {string} query
 * @param {string} text
 * @returns {number|null}
 */
export function fuzzyScore(query, text) {
  const q = query.toLowerCase().trim();
  const t = text.toLowerCase();
  if (!q) return 0;
  if (t === q) return 1000;
  if (t.startsWith(q)) return 800 - t.length;
  const idx = t.indexOf(q);
  if (idx >= 0) return 600 - idx - t.length / 10 + (/[\s/:_-]/.test(t[idx - 1] || ' ') ? 50 : 0);
  let score = 0;
  let ti = 0;
  let prev = -2;
  for (const ch of q) {
    if (ch === ' ') continue;
    const found = t.indexOf(ch, ti);
    if (found < 0) return null;
    const wordStart = found === 0 || /[\s/:_-]/.test(t[found - 1]);
    score += 10 + (found === prev + 1 ? 15 : 0) + (wordStart ? 20 : 0) - Math.min(found - ti, 10);
    prev = found;
    ti = found + 1;
  }
  return score;
}

/**
 * Rank commands for a query (pure; exported for tests).
 * @param {Array<{title: string, keywords?: string[], subtitle?: string}>} commands
 * @param {string} query
 */
export function rankCommands(commands, query) {
  if (!query.trim()) return commands.slice();
  const scored = [];
  for (const c of commands) {
    const fields = [c.title, ...(c.keywords || []), c.subtitle || ''];
    let best = null;
    fields.forEach((f, i) => {
      const s = fuzzyScore(query, f);
      if (s !== null) {
        const adj = i === 0 ? s : s - 40; // title matches beat keyword matches
        if (best === null || adj > best) best = adj;
      }
    });
    if (best !== null) scored.push([best, c]);
  }
  scored.sort((a, b) => b[0] - a[0]);
  return scored.map((x) => x[1]);
}

const sources = new Set();

/**
 * Register commands. `cmds` is an array or a function returning one (evaluated
 * each time the palette opens, so it can depend on the current user/state).
 * Command: {id, title, subtitle?, icon?, group?, keywords?, shortcut?, run()}.
 * @returns {() => void} unregister
 */
export function registerCommands(cmds) {
  sources.add(cmds);
  return () => sources.delete(cmds);
}

function allCommands() {
  const out = [];
  const seen = new Set();
  // Most recently registered first so app commands appear above globals.
  for (const src of [...sources].reverse()) {
    const list = typeof src === 'function' ? src() : src;
    for (const c of list || []) {
      if (!c || seen.has(c.id)) continue;
      seen.add(c.id);
      out.push(c);
    }
  }
  return out;
}

let openState = null;

/** Is the palette open? */
export function isPaletteOpen() {
  return !!openState;
}

/** Close the palette if open. */
export function closePalette() {
  openState?.close();
}

/**
 * Open the command palette.
 * @param {string} [initialQuery]
 */
export function openPalette(initialQuery = '') {
  if (openState) {
    openState.input.focus();
    return;
  }
  const prevFocus = document.activeElement;
  const listId = uid('pl');
  const input = h('input', {
    class: 'palette-input', type: 'text', placeholder: 'Search apps, pages and actions…', autocomplete: 'off', spellcheck: 'false',
    role: 'combobox', aria: { expanded: 'true', controls: listId, autocomplete: 'list', label: 'Command palette' },
  });
  input.value = initialQuery;
  const list = h('div', { class: 'palette-list', id: listId, role: 'listbox', aria: { label: 'Commands' } });
  const dialog = h('div', { class: 'palette', role: 'dialog', aria: { modal: 'true', label: 'Command palette' } },
    h('div', { class: 'palette-input-row' }, icon('search', { size: 18, className: 'palette-search-icon' }), input,
      h('kbd', { class: 'palette-esc' }, 'Esc')),
    list,
    h('div', { class: 'palette-footer' },
      h('span', {}, h('kbd', {}, '↑'), h('kbd', {}, '↓'), ' to navigate'),
      h('span', {}, h('kbd', {}, '↵'), ' to run'),
      h('span', {}, h('kbd', {}, 'Esc'), ' to close')));
  const backdrop = h('div', { class: 'palette-backdrop' }, dialog);
  let results = [];
  let active = 0;
  const commands = allCommands();

  const render = () => {
    results = rankCommands(commands, input.value).slice(0, 50);
    active = Math.min(active, Math.max(0, results.length - 1));
    list.replaceChildren();
    if (!results.length) {
      list.append(h('div', { class: 'palette-empty' }, 'No matching commands'));
      input.removeAttribute('aria-activedescendant');
      return;
    }
    const grouped = !input.value.trim();
    let lastGroup = null;
    results.forEach((c, i) => {
      if (grouped && c.group && c.group !== lastGroup) {
        lastGroup = c.group;
        list.append(h('div', { class: 'palette-group', role: 'presentation' }, c.group));
      }
      const id = `${listId}-${i}`;
      const item = h('div', {
        class: cls('palette-item', { 'is-active': i === active }), id, role: 'option',
        aria: { selected: String(i === active) },
      },
      h('span', { class: 'palette-item-icon' }, icon(c.icon || 'arrow-right')),
      h('span', { class: 'palette-item-title' }, c.title),
      c.subtitle ? h('span', { class: 'palette-item-sub' }, c.subtitle) : null,
      c.shortcut ? h('kbd', { class: 'palette-item-kbd' }, comboLabel(c.shortcut)) : null);
      item.addEventListener('pointermove', () => {
        if (active !== i) {
          active = i;
          highlight();
        }
      });
      item.addEventListener('click', () => run(c));
      list.append(item);
    });
    highlight();
  };
  const highlight = () => {
    list.querySelectorAll('.palette-item').forEach((el, i) => {
      el.classList.toggle('is-active', i === active);
      el.setAttribute('aria-selected', String(i === active));
      if (i === active) {
        input.setAttribute('aria-activedescendant', el.id);
        el.scrollIntoView({ block: 'nearest' });
      }
    });
  };
  const close = () => {
    if (!openState) return;
    openState = null;
    backdrop.remove();
    document.documentElement.classList.remove('palette-open');
    const app = document.getElementById('app');
    if (app) app.inert = false;
    if (prevFocus?.isConnected) prevFocus.focus({ preventScroll: true });
  };
  const run = (c) => {
    close();
    try {
      c.run?.();
    } catch (err) {
      console.error('command failed', err);
    }
  };

  input.addEventListener('input', () => {
    active = 0;
    render();
  });
  input.addEventListener('keydown', (e) => {
    if (e.key === 'ArrowDown') {
      e.preventDefault();
      active = results.length ? (active + 1) % results.length : 0;
      highlight();
    } else if (e.key === 'ArrowUp') {
      e.preventDefault();
      active = results.length ? (active - 1 + results.length) % results.length : 0;
      highlight();
    } else if (e.key === 'Enter') {
      e.preventDefault();
      if (results[active]) run(results[active]);
    } else if (e.key === 'Escape') {
      e.preventDefault();
      e.stopPropagation();
      close();
    } else if (e.key === 'Tab') {
      e.preventDefault(); // keep focus in the palette
    }
  });
  backdrop.addEventListener('pointerdown', (e) => {
    if (e.target === backdrop) close();
  });

  document.body.appendChild(backdrop);
  document.documentElement.classList.add('palette-open');
  const app = document.getElementById('app');
  if (app) app.inert = true;
  openState = { close, input };
  render();
  input.focus();
  input.setSelectionRange(input.value.length, input.value.length);
}
