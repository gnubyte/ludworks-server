// In-cell editor and formula bar: a textarea overlaid on the active cell, kept in sync with the
// formula bar input, with function-name autocomplete, a signature hint for the enclosing
// call, and "point mode" so clicking cells (or arrow keys) inserts references into formulas.

import { h, cls } from '../../core/dom.js';
import { completionContext, callContext, canInsertRef, refAtCaret } from './formula.js';

const IDLE_TEXT = String.fromCharCode(0xa0); // one selected no-break space

export class CellEditor {
  /**
   * @param {{host: HTMLElement, bar: HTMLInputElement, functions: () => Array<{name,syntax,description}>,
   *          onCommit: (text: string, move: {dr: number, dc: number}, opts?: {all?: boolean}) => void,
   *          onCancel: () => void, onChange: (text: string) => void, onPointKey: (dr: number, dc: number, extend: boolean) => void}} o
   */
  constructor(o) {
    this.o = o;
    this.open = false;
    this.mode = 'enter';
    // The textarea is also the grid's keyboard focus target while idle (invisible, parked on
    // the active cell): typed characters, IME composition and clipboard events all arrive here.
    this.ta = h('textarea', {
      class: 'sh-cell-editor is-idle', spellcheck: 'false', autocomplete: 'off', rows: 1, inputmode: 'none',
      aria: { label: 'Spreadsheet', autocomplete: 'list', roledescription: 'spreadsheet' },
    });
    this.list = h('div', { class: 'sh-ac', role: 'listbox', hidden: true, aria: { label: 'Functions' } });
    this.hint = h('div', { class: 'sh-sig', role: 'tooltip', hidden: true });
    o.host.append(this.ta, this.list, this.hint);
    this.items = [];
    this.active = 0;
    this.pointSpan = null; // {start, end} of a reference inserted by pointing
    this.composing = false;
    this.ta.addEventListener('compositionstart', () => {
      this.composing = true;
      if (!this.open) this.o.onIdleCompose?.();
    });
    this.ta.addEventListener('compositionend', () => {
      this.composing = false;
      if (this.open) this.input(this.ta);
    });
    for (const el of [this.ta, o.bar]) {
      el.addEventListener('keydown', (e) => {
        if (el === this.ta && !this.open) this.o.onIdleKey?.(e);
        else this.keydown(e, el);
      });
      el.addEventListener('input', () => {
        if (el === this.ta && !this.open) {
          // text that arrived without a keydown (insertText, voice input…) starts an edit
          const typed = this.ta.value.replace(IDLE_TEXT, '');
          if (!this.composing && typed) this.o.onIdleInput?.(typed);
          else this.arm();
          return;
        }
        if (this.composing) return;
        this.input(el);
      });
      el.addEventListener('click', () => { this.pointSpan = null; this.refreshPopups(el); });
      el.addEventListener('keyup', (e) => { if (['ArrowLeft', 'ArrowRight', 'Home', 'End'].includes(e.key)) this.refreshPopups(el); });
    }
    o.bar.addEventListener('focus', () => {
      if (!this.open) this.o.onBarFocus?.();
      this.mode = 'edit';
    });
    this.list.addEventListener('pointerdown', (e) => {
      const item = e.target.closest('[data-name]');
      if (!item) return;
      e.preventDefault();
      this.accept(item.dataset.name);
    });
  }

  /** Text currently being edited. */
  get value() {
    return this.ta.value;
  }

  /** The input element that has focus (textarea or formula bar). */
  get focused() {
    return document.activeElement === this.o.bar ? this.o.bar : this.ta;
  }

  /**
   * Start editing.
   * @param {{text: string, mode: 'enter'|'edit', rect: {x,y,w,h}, font: string, bg?: string, color?: string,
   *          align?: string, viaBar?: boolean}} p
   */
  start(p) {
    this.open = true;
    this.mode = p.mode;
    this.pointSpan = null;
    if (p.text !== null) this.ta.value = p.text;
    this.o.bar.value = this.ta.value;
    this.ta.hidden = false;
    this.ta.classList.remove('is-idle');
    if (this.ta.getAttribute('inputmode') !== 'text') {
      // the idle sink suppresses the on-screen keyboard; editing needs it
      this.ta.setAttribute('inputmode', 'text');
      if (!p.viaBar && document.activeElement === this.ta && window.matchMedia('(pointer: coarse)').matches) this.ta.blur();
    }
    this.place(p.rect);
    this.ta.style.font = p.font;
    this.ta.style.background = p.bg || '';
    this.ta.style.color = p.color || '';
    this.ta.style.textAlign = p.align === 'right' || p.align === 'center' ? p.align : 'left';
    const target = p.viaBar ? this.o.bar : this.ta;
    if (document.activeElement !== target) target.focus({ preventScroll: true });
    if (!this.composing) {
      const n = target.value.length;
      target.setSelectionRange(n, n);
    }
    this.autosize();
    this.o.onChange(this.value);
    this.refreshPopups(target);
  }

  /** Reposition over the cell (after scrolling). */
  place(rect) {
    this.rect = rect;
    this.ta.style.left = `${rect.x - 1}px`;
    this.ta.style.top = `${rect.y - 1}px`;
    this.ta.style.minWidth = `${rect.w + 1}px`;
    this.ta.style.minHeight = `${rect.h + 1}px`;
    this.autosize();
  }

  autosize() {
    if (!this.rect) return;
    const ta = this.ta;
    const host = this.o.host;
    ta.style.width = '0px';
    ta.style.height = '0px';
    const maxW = Math.max(this.rect.w + 1, host.clientWidth - this.rect.x - 8);
    ta.style.width = `${Math.min(maxW, Math.max(this.rect.w + 1, ta.scrollWidth + 4))}px`;
    ta.style.height = `${Math.min(Math.max(this.rect.h + 1, ta.scrollHeight), Math.max(this.rect.h + 1, host.clientHeight - this.rect.y - 8))}px`;
    this.positionPopups();
  }

  close() {
    this.open = false;
    this.ta.value = IDLE_TEXT;
    this.ta.hidden = false;
    this.ta.classList.add('is-idle');
    this.ta.setAttribute('inputmode', 'none');
    this.ta.style.background = '';
    this.list.hidden = true;
    this.hint.hidden = true;
    this.pointSpan = null;
  }

  /** Park the idle focus target on the active cell (keeps IME popups near it). */
  idlePlace(rect) {
    if (this.open) return;
    this.ta.style.left = `${Math.max(0, rect.x)}px`;
    this.ta.style.top = `${Math.max(0, rect.y)}px`;
  }

  /** Give keyboard focus to the grid (the idle textarea). */
  focusIdle() {
    if (this.open) return;
    if (document.activeElement !== this.ta) this.ta.focus({ preventScroll: true });
    this.arm();
  }

  // Browsers only run Copy/Cut for a non-empty selection, so the idle sink always holds one
  // selected placeholder character (replaced by whatever the user types).
  arm() {
    if (this.open || this.composing) return;
    if (this.ta.value !== IDLE_TEXT) this.ta.value = IDLE_TEXT;
    this.ta.setSelectionRange(0, IDLE_TEXT.length);
  }

  /** Accessible name of the focus target (current cell description). */
  setLabel(text) {
    this.ta.setAttribute('aria-label', text);
  }

  input(el) {
    const other = el === this.ta ? this.o.bar : this.ta;
    if (!this.open) {
      // typing into the formula bar starts an edit of the active cell
      this.o.onBarFocus?.(true);
      if (!this.open) return;
    }
    other.value = el.value;
    this.pointSpan = null;
    this.autosize();
    this.o.onChange(el.value);
    this.refreshPopups(el);
  }

  setText(text, caret, el = this.focused) {
    this.ta.value = text;
    this.o.bar.value = text;
    el.focus({ preventScroll: true });
    el.setSelectionRange(caret, caret);
    this.autosize();
    this.o.onChange(text);
    this.refreshPopups(el);
  }

  /** Whether clicking a cell should insert a reference right now. */
  canPoint() {
    if (!this.open) return false;
    const el = this.focused;
    const caret = el.selectionStart;
    if (this.pointSpan && this.pointSpan.end === caret) return true;
    return el.selectionStart === el.selectionEnd && canInsertRef(el.value, caret);
  }

  /** Insert (or replace the previously pointed) reference at the caret. */
  pointRef(text) {
    const el = this.focused;
    const v = el.value;
    let start = el.selectionStart;
    let end = el.selectionEnd;
    if (this.pointSpan && this.pointSpan.end === start) start = this.pointSpan.start;
    else {
      const tok = refAtCaret(v, start);
      if (tok && this.pointSpan) start = tok.start;
    }
    end = Math.max(end, start);
    const next = v.slice(0, start) + text + v.slice(end);
    this.setText(next, start + text.length, el);
    this.pointSpan = { start, end: start + text.length };
  }

  keydown(e, el) {
    const acOpen = !this.list.hidden && this.items.length > 0;
    if (!this.open) {
      if (el === this.o.bar && e.key === 'Escape') { e.preventDefault(); this.o.onCancel(); }
      if (el === this.o.bar && (e.key === 'Enter' || e.key === 'Tab')) { e.preventDefault(); this.o.onCancel(); }
      return;
    }
    e.stopPropagation();
    if (acOpen && (e.key === 'ArrowDown' || e.key === 'ArrowUp')) {
      e.preventDefault();
      this.active = (this.active + (e.key === 'ArrowDown' ? 1 : -1) + this.items.length) % this.items.length;
      this.renderList();
      return;
    }
    if (acOpen && (e.key === 'Tab' || (e.key === 'Enter' && !e.altKey))) {
      e.preventDefault();
      this.accept(this.items[this.active].name);
      return;
    }
    switch (e.key) {
      case 'Escape':
        e.preventDefault();
        if (acOpen) { this.list.hidden = true; return; }
        this.o.onCancel();
        return;
      case 'Enter':
        if (e.altKey) {
          e.preventDefault();
          const s = el.selectionStart;
          this.setText(`${el.value.slice(0, s)}\n${el.value.slice(el.selectionEnd)}`, s + 1, el);
          return;
        }
        e.preventDefault();
        this.o.onCommit(el.value, { dr: e.shiftKey ? -1 : 1, dc: 0 }, { all: e.ctrlKey || e.metaKey });
        return;
      case 'Tab':
        e.preventDefault();
        this.o.onCommit(el.value, { dr: 0, dc: e.shiftKey ? -1 : 1 });
        return;
      case 'F2':
        e.preventDefault();
        this.mode = this.mode === 'enter' ? 'edit' : 'enter';
        this.o.onModeChange?.(this.mode);
        return;
      case 'ArrowUp': case 'ArrowDown': case 'ArrowLeft': case 'ArrowRight': {
        if (this.mode !== 'enter' || el === this.o.bar) return;
        const d = { ArrowUp: [-1, 0], ArrowDown: [1, 0], ArrowLeft: [0, -1], ArrowRight: [0, 1] }[e.key];
        if (this.canPoint()) {
          e.preventDefault();
          this.o.onPointKey(d[0], d[1], e.shiftKey);
          return;
        }
        e.preventDefault();
        this.o.onCommit(el.value, { dr: d[0], dc: d[1] });
        return;
      }
      default:
        if (e.key.length === 1 || e.key === 'Backspace' || e.key === 'Delete') this.pointSpan = null;
    }
  }

  accept(name) {
    const el = this.focused;
    const caret = el.selectionStart;
    const ctx = completionContext(el.value, caret);
    if (!ctx) return;
    const v = el.value;
    const insert = `${name}(`;
    const next = v.slice(0, ctx.start) + insert + v.slice(caret);
    this.list.hidden = true;
    this.setText(next, ctx.start + insert.length, el);
  }

  refreshPopups(el = this.focused) {
    if (!this.open) {
      this.list.hidden = true;
      this.hint.hidden = true;
      return;
    }
    const caret = el.selectionStart;
    const fns = this.o.functions() || [];
    const ctx = el.selectionStart === el.selectionEnd ? completionContext(el.value, caret) : null;
    this.items = ctx ? rankFunctions(fns, ctx.prefix).slice(0, 8) : [];
    if (this.items.length === 1 && this.items[0].name === ctx?.prefix) this.items = this.items.filter(() => false);
    this.active = 0;
    this.renderList();
    const call = callContext(el.value, caret);
    const fn = call && fns.find((f) => f.name === call.name);
    if (fn && !this.items.length) {
      this.hint.replaceChildren(signature(fn, call.arg), h('div', { class: 'sh-sig-desc' }, fn.description));
      this.hint.hidden = false;
    } else this.hint.hidden = true;
    this.anchorEl = el;
    this.positionPopups();
  }

  renderList() {
    this.list.replaceChildren(...this.items.map((f, i) => h('div', {
      class: cls('sh-ac-item', { 'is-active': i === this.active }), role: 'option', dataset: { name: f.name },
      aria: { selected: String(i === this.active) },
    }, h('span', { class: 'sh-ac-name' }, f.name), h('span', { class: 'sh-ac-desc' }, f.description))));
    this.list.hidden = this.items.length === 0;
  }

  positionPopups() {
    const host = this.o.host;
    const hr = host.getBoundingClientRect();
    const el = this.anchorEl || this.ta;
    const r = el.getBoundingClientRect();
    const left = Math.max(4, Math.min(r.left - hr.left, host.clientWidth - 320));
    const top = r.bottom - hr.top + 4;
    for (const p of [this.list, this.hint]) {
      p.style.left = `${left}px`;
      p.style.top = `${top}px`;
    }
    if (!this.list.hidden && !this.hint.hidden) this.hint.hidden = true;
  }
}

/** Signature line with the current argument emphasised. */
function signature(fn, arg) {
  const m = /^([^(]*)\((.*)\)$/.exec(fn.syntax || '');
  if (!m) return h('div', { class: 'sh-sig-line mono' }, fn.syntax || fn.name);
  const args = m[2].split(/,\s*/);
  const idx = Math.min(arg, args.length - 1);
  const repeat = args[args.length - 1].includes('...') || args[args.length - 1].includes('…');
  const cur = arg >= args.length && repeat ? args.length - 2 : idx;
  const parts = [];
  args.forEach((a, i) => {
    if (i) parts.push(', ');
    parts.push(i === cur ? h('strong', {}, a) : a);
  });
  return h('div', { class: 'sh-sig-line mono' }, m[1], '(', parts, ')');
}

// Frequently used functions are suggested first.
const POPULAR = ['SUM', 'AVERAGE', 'COUNT', 'COUNTA', 'IF', 'IFERROR', 'MAX', 'MIN', 'ROUND', 'VLOOKUP', 'XLOOKUP', 'INDEX',
  'MATCH', 'SUMIF', 'SUMIFS', 'COUNTIF', 'COUNTIFS', 'AVERAGEIF', 'TEXT', 'CONCAT', 'LEFT', 'RIGHT', 'MID', 'LEN', 'TRIM',
  'TODAY', 'NOW', 'DATE', 'AND', 'OR', 'NOT', 'ABS', 'UPPER', 'LOWER', 'SUBSTITUTE', 'TEXTJOIN', 'YEAR', 'MONTH', 'DAY'];

/** Functions starting with prefix: exact match, then popular ones, then shortest/alphabetical. */
export function rankFunctions(fns, prefix) {
  const rank = (n) => (n === prefix ? -1 : POPULAR.includes(n) ? POPULAR.indexOf(n) : 1000);
  return fns.filter((f) => f.name.startsWith(prefix))
    .sort((a, b) => rank(a.name) - rank(b.name) || a.name.length - b.name.length || a.name.localeCompare(b.name));
}
