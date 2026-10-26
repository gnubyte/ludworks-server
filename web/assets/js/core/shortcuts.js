// Global keyboard shortcuts: single combos ("mod+k", "shift+?") and two-key
// sequences ("g h"). "mod" is Cmd on Apple platforms and Ctrl elsewhere.

import { isEditable } from './dom.js';

/** True on macOS/iOS (for Cmd vs Ctrl). */
export function isApple(platform = globalThis.navigator?.userAgentData?.platform || globalThis.navigator?.platform || '') {
  return /mac|iphone|ipad|ipod/i.test(platform);
}

const KEY_ALIASES = { esc: 'escape', del: 'delete', space: ' ', plus: '+', up: 'arrowup', down: 'arrowdown', left: 'arrowleft', right: 'arrowright' };

/**
 * Parse a combo such as "mod+shift+k" into a normalised descriptor.
 * @param {string} combo
 * @param {boolean} [apple]
 */
export function parseCombo(combo, apple = isApple()) {
  const parts = combo.toLowerCase().split('+').map((p) => p.trim());
  // "shift++" style: an empty last part means the "+" key itself.
  let key = parts.pop();
  if (key === '' && parts.at(-1) === '') {
    parts.pop();
    key = '+';
  }
  const d = { key: KEY_ALIASES[key] || key, ctrl: false, meta: false, alt: false, shift: false };
  for (const p of parts) {
    if (p === 'mod') {
      if (apple) d.meta = true;
      else d.ctrl = true;
    } else if (p === 'ctrl' || p === 'control') d.ctrl = true;
    else if (p === 'cmd' || p === 'meta') d.meta = true;
    else if (p === 'alt' || p === 'option') d.alt = true;
    else if (p === 'shift') d.shift = true;
  }
  return d;
}

/**
 * Does a KeyboardEvent-like object match a parsed combo? Shift is ignored for
 * non-letter keys (so "?" matches regardless of keyboard layout).
 */
export function eventMatches(e, d) {
  const key = (e.key || '').toLowerCase();
  if (key !== d.key) return false;
  if (!!e.ctrlKey !== d.ctrl || !!e.metaKey !== d.meta || !!e.altKey !== d.alt) return false;
  const isLetter = /^[a-z]$/.test(d.key);
  if (isLetter && !!e.shiftKey !== d.shift) return false;
  return true;
}

/** Human label for a combo: "⌘K" on Apple, "Ctrl+K" elsewhere. */
export function comboLabel(combo, apple = isApple()) {
  return combo.split(' ').map((part) => {
    const d = parseCombo(part, apple);
    const key = d.key === ' ' ? 'Space' : d.key.length === 1 ? d.key.toUpperCase() : d.key[0].toUpperCase() + d.key.slice(1);
    if (apple) return `${d.ctrl ? '⌃' : ''}${d.alt ? '⌥' : ''}${d.shift ? '⇧' : ''}${d.meta ? '⌘' : ''}${key}`;
    return [d.ctrl && 'Ctrl', d.meta && 'Win', d.alt && 'Alt', d.shift && 'Shift', key].filter(Boolean).join('+');
  }).join(' then ');
}

const registry = [];
let pending = null; // first key of a sequence
let pendingTimer = null;

/**
 * Register a shortcut.
 * @param {string} combo "mod+k", "?", "g h" (sequence), "escape"
 * @param {(e: KeyboardEvent) => void} handler
 * @param {{description?: string, group?: string, allowInInputs?: boolean, when?: () => boolean}} [opts]
 * @returns {() => void} unregister
 */
export function registerShortcut(combo, handler, opts = {}) {
  const entry = { combo, steps: combo.split(' ').map((c) => parseCombo(c)), handler, ...opts };
  registry.push(entry);
  return () => {
    const i = registry.indexOf(entry);
    if (i >= 0) registry.splice(i, 1);
  };
}

/** Shortcuts with descriptions, for the help dialog. */
export function listShortcuts() {
  return registry.filter((r) => r.description).map((r) => ({ combo: r.combo, label: comboLabel(r.combo), description: r.description, group: r.group || 'General' }));
}

function onKeydown(e) {
  if (e.defaultPrevented || e.isComposing) return;
  const inInput = isEditable(e.target);
  // Newest registrations win so an app can override a global binding.
  const candidates = [...registry].reverse().filter((r) => (!inInput || r.allowInInputs) && (!r.when || r.when()));
  if (pending) {
    const first = pending;
    pending = null;
    clearTimeout(pendingTimer);
    const seq = candidates.find((r) => r.steps.length === 2 && eventMatches(first, r.steps[0]) && eventMatches(e, r.steps[1]));
    if (seq) {
      e.preventDefault();
      seq.handler(e);
      return;
    }
  }
  const single = candidates.find((r) => r.steps.length === 1 && eventMatches(e, r.steps[0]));
  if (single) {
    e.preventDefault();
    single.handler(e);
    return;
  }
  if (candidates.some((r) => r.steps.length === 2 && eventMatches(e, r.steps[0]))) {
    // Remember the first key of a possible sequence for a short while.
    pending = { key: e.key, ctrlKey: e.ctrlKey, metaKey: e.metaKey, altKey: e.altKey, shiftKey: e.shiftKey };
    pendingTimer = setTimeout(() => { pending = null; }, 1200);
  }
}

let started = false;
/** Attach the global listener (idempotent). */
export function initShortcuts() {
  if (started) return;
  started = true;
  document.addEventListener('keydown', onKeydown);
}
