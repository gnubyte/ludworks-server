/**
 * mdeditor.js — Markdown source editor for power users: a textarea with toolbar, shortcuts,
 * list continuation, Tab indentation, smart fences and a synchronized live preview
 * (source / split / preview).
 *
 * The text transformations are pure functions over {value, start, end} (exported as `textOps`)
 * so they are unit-testable without a DOM. Edits are applied with execCommand('insertText') on
 * the minimal changed range, which keeps the browser's native undo stack intact.
 *
 * @module lib/mdeditor
 */
import { renderToDOM, toggleTask, safeImageSrc } from './markdown.js';
import { htmlToMarkdown } from './richeditor.js';

const ELLIPSIS = String.fromCharCode(0x2026);

// =============================================================================================
// Pure text operations

const lineStartOf = (v, i) => v.lastIndexOf('\n', i - 1) + 1;
const lineEndOf = (v, i) => {
  const e = v.indexOf('\n', i);
  return e < 0 ? v.length : e;
};

/** Splits a line into container prefixes (indent, quotes, list marker, task box, heading) and text. */
export function parseLine(line) {
  const m = /^([ \t]*)((?:>[ \t]?)*)([ \t]*)/.exec(line);
  let rest = line.slice(m[0].length);
  let list = null, task = null, heading = 0, mm;
  if ((mm = /^([-*+])(?:[ \t]+|$)/.exec(rest))) {
    list = { type: 'ul', marker: mm[1] };
    rest = rest.slice(mm[0].length);
  } else if ((mm = /^(\d{1,9})([.)])(?:[ \t]+|$)/.exec(rest))) {
    list = { type: 'ol', num: Number(mm[1]), delim: mm[2] };
    rest = rest.slice(mm[0].length);
  }
  if (list && (mm = /^\[([ xX])\](?:[ \t]+|$)/.exec(rest))) {
    task = mm[1] !== ' ';
    rest = rest.slice(mm[0].length);
  }
  if (!list && (mm = /^(#{1,6})(?:[ \t]+|$)/.exec(rest))) {
    heading = mm[1].length;
    rest = rest.slice(mm[0].length);
  }
  return { indent: m[1], quote: m[2], inner: m[3], list, task, heading, text: rest };
}

function markerOf(list) {
  return list.type === 'ul' ? list.marker || '-' : `${list.num}${list.delim || '.'}`;
}

/** Inverse of parseLine. */
export function buildLine(p) {
  let s = p.indent + p.quote + p.inner;
  if (p.list) s += markerOf(p.list) + ' ';
  if (p.list && p.task !== null) s += p.task ? '[x] ' : '[ ] ';
  if (p.heading) s += '#'.repeat(p.heading) + ' ';
  return s + p.text;
}

const prefixLength = (p) => buildLine({ ...p, text: '' }).length;

/** Returns true when line start `ls` lies inside a fenced code block. */
function inFence(value, ls) {
  let fence = null;
  const re = /^[ \t]{0,3}(`{3,}|~{3,})(.*)$/gm;
  re.lastIndex = 0;
  let m;
  while ((m = re.exec(value)) && m.index < ls) {
    if (!fence) {
      if (m[1][0] === '`' && m[2].includes('`')) continue;
      fence = m[1];
    } else if (m[1][0] === fence[0] && m[1].length >= fence.length && !m[2].trim()) {
      fence = null;
    }
    if (re.lastIndex === m.index) re.lastIndex++;
  }
  return !!fence;
}

const edit = (value, start, end, text, selStart, selEnd) => ({
  value: value.slice(0, start) + text + value.slice(end),
  start: selStart ?? start + text.length,
  end: selEnd ?? selStart ?? start + text.length,
});

function runOf(s, ch, fromEnd) {
  let n = 0;
  if (fromEnd) { for (let i = s.length - 1; i >= 0 && s[i] === ch; i--) n++; }
  else { for (let i = 0; i < s.length && s[i] === ch; i++) n++; }
  return n;
}

function maxRun(s, ch) {
  let run = 0, max = 0;
  for (const c of s) { if (c === ch) { run++; if (run > max) max = run; } else run = 0; }
  return max;
}

/**
 * Toggles an inline marker (**, *, ~~, `) around the selection. Empty selections insert a
 * placeholder that is left selected; whitespace at the selection edges stays outside.
 */
export function wrapSelection(value, start, end, marker, placeholder = 'text') {
  const before = value.slice(0, start), sel = value.slice(start, end), after = value.slice(end);
  const ml = marker.length, c = marker[0];
  const run = Math.min(runOf(before, c, true), runOf(after, c, false));
  const present = marker === '*' ? run % 2 === 1 : run >= ml;
  if (present) return { value: before.slice(0, -ml) + sel + after.slice(ml), start: start - ml, end: end - ml };
  if (sel.length >= 2 * ml && sel.startsWith(marker) && sel.endsWith(marker)) {
    const innerRun = Math.min(runOf(sel, c, false), runOf(sel, c, true));
    if (marker !== '*' || innerRun % 2 === 1) {
      const inner = sel.slice(ml, -ml);
      return { value: before + inner + after, start, end: start + inner.length };
    }
  }
  if (!sel) {
    return { value: before + marker + placeholder + marker + after, start: start + ml, end: start + ml + placeholder.length };
  }
  const lead = /^\s*/.exec(sel)[0];
  const trail = sel.slice(lead.length).match(/\s*$/)[0];
  const core = sel.slice(lead.length, sel.length - trail.length);
  if (!core) return null;
  let mk = marker;
  if (marker === '`' && core.includes('`')) mk = '`'.repeat(maxRun(core, '`') + 1);
  const pad = mk.length > 1 && marker === '`' ? ' ' : '';
  // Emphasis cannot span blank lines: wrap each non-blank line separately.
  const wrapped = core.split(/(\n[ \t]*\n)/).map((part, i) => (i % 2 ? part : mk + pad + part + pad + mk)).join('');
  const s = start + lead.length;
  return { value: before + lead + wrapped + trail + after, start: s + mk.length + pad.length, end: s + wrapped.length - mk.length - pad.length };
}

/** Wraps the selection in a link, selecting the URL placeholder. */
export function insertLink(value, start, end, url = 'https://') {
  const sel = value.slice(start, end);
  if (/^(?:https?:\/\/|mailto:)\S+$/i.test(sel)) {
    const text = '[](' + sel + ')';
    return edit(value, start, end, text, start + 1, start + 1);
  }
  const label = sel || 'link';
  const text = `[${label}](${url})`;
  const urlStart = start + label.length + 3;
  return edit(value, start, end, text, sel ? urlStart : start + 1, sel ? urlStart + url.length : start + 1 + label.length);
}

function selectedLineRange(value, start, end) {
  const ls = lineStartOf(value, start);
  const le = lineEndOf(value, end > start && value[end - 1] === '\n' ? end - 1 : end);
  return [ls, le];
}

/**
 * Toggles a line-level construct on every selected line: 'ul', 'ol', 'task', 'quote', or
 * 'heading' (with level 0..6).
 */
export function toggleLines(value, start, end, kind, level = 1) {
  const [ls, le] = selectedLineRange(value, start, end);
  const lines = value.slice(ls, le).split('\n');
  const parsed = lines.map(parseLine);
  const content = parsed.filter((p) => p.text.trim() || p.list || p.heading || p.quote);
  const targets = content.length ? content : parsed;
  let all;
  switch (kind) {
    case 'ul': all = targets.every((p) => p.list && p.list.type === 'ul' && p.task === null); break;
    case 'ol': all = targets.every((p) => p.list && p.list.type === 'ol'); break;
    case 'task': all = targets.every((p) => p.list && p.task !== null); break;
    case 'quote': all = targets.every((p) => p.quote); break;
    case 'heading': all = level === 0 || targets.every((p) => p.heading === level); break;
    default: return null;
  }
  let n = 1;
  for (const p of parsed) {
    if (!targets.includes(p)) {
      if (kind === 'quote' && !all) p.quote = '> ';
      continue;
    }
    switch (kind) {
      case 'ul':
        if (all) { p.list = null; } else { p.list = { type: 'ul', marker: p.list && p.list.type === 'ul' ? p.list.marker : '-' }; p.task = null; p.heading = 0; }
        break;
      case 'ol':
        if (all) { p.list = null; p.task = null; } else { p.list = { type: 'ol', num: n++, delim: '.' }; p.heading = 0; }
        break;
      case 'task':
        if (all) { p.list = null; p.task = null; } else { if (!p.list) p.list = { type: 'ul', marker: '-' }; if (p.task === null) p.task = false; p.heading = 0; }
        break;
      case 'quote':
        if (all) p.quote = p.quote.replace(/^>[ \t]?/, ''); else p.quote = '> ' + p.quote;
        break;
      case 'heading':
        if (all) p.heading = 0; else { p.heading = level; p.list = null; p.task = null; }
        break;
      default: break;
    }
  }
  const out = parsed.map((p) => (kind === 'quote' ? buildLine(p).replace(/^(\s*(?:>\s?)+)$/, (q) => q.trimEnd()) : buildLine(p))).join('\n');
  const res = { value: value.slice(0, ls) + out + value.slice(le) };
  if (start === end && lines.length === 1) {
    const oldPrefix = prefixLength(parseLine(lines[0]));
    const newPrefix = prefixLength(parseLine(out));
    const caret = Math.max(ls + newPrefix, start + (newPrefix - oldPrefix));
    res.start = res.end = Math.min(caret, ls + out.length);
  } else {
    res.start = ls;
    res.end = ls + out.length;
  }
  return res;
}

/**
 * Enter: continues lists/quotes (an empty item ends the list), keeps indentation, and closes
 * a freshly typed code fence. Returns null to let the browser insert a plain newline.
 */
export function continueOnEnter(value, start, end) {
  if (start !== end) return null;
  const ls = lineStartOf(value, start);
  const le = lineEndOf(value, start);
  const line = value.slice(ls, le);
  if (inFence(value, ls)) {
    const indent = /^[ \t]*/.exec(line)[0];
    return indent && start - ls >= indent.length ? edit(value, start, start, '\n' + indent) : null;
  }
  const fm = /^([ \t]*(?:>[ \t]?)*[ \t]*)(`{3,}|~{3,})([^`\n]*)$/.exec(line);
  if (fm && start === le && !(fm[2][0] === '`' && fm[3].includes('`'))) {
    // Close the fence unless a dangling fence below would already close it.
    const below = value.slice(le).match(/^[ \t]*(?:>[ \t]?)*[ \t]*(?:`{3,}|~{3,})/gm) || [];
    if (below.length % 2 === 0) {
      const prefix = fm[1];
      return edit(value, start, start, '\n' + prefix + '\n' + prefix + fm[2], start + 1 + prefix.length);
    }
  }
  const p = parseLine(line);
  const plen = prefixLength(p);
  if ((p.list || p.quote) && start - ls >= plen) {
    if (!p.text.trim() && start === le) {
      // An empty item ends the list (or one quote level).
      const np = { ...p };
      if (p.list) { np.list = null; np.task = null; np.inner = ''; } else { np.quote = np.quote.replace(/>[ \t]?$/, ''); }
      const repl = buildLine(np).replace(/[ \t]+$/, '');
      return edit(value, ls, le, repl);
    }
    const next = { ...p, text: '', heading: 0 };
    if (p.list && p.list.type === 'ol') next.list = { ...p.list, num: p.list.num + 1 };
    if (p.task !== null) next.task = false;
    return edit(value, start, start, '\n' + buildLine(next));
  }
  const indent = /^[ \t]*/.exec(line)[0];
  if (indent && start - ls >= indent.length) return edit(value, start, start, '\n' + indent);
  return null;
}

function listUnit(value, ls, p) {
  // Nesting under the previous sibling needs its content offset (2 for "- ", 3 for "1. ").
  let pos = ls;
  for (let i = 0; i < 200 && pos > 0; i++) {
    const prevStart = lineStartOf(value, pos - 1);
    const q = parseLine(value.slice(prevStart, pos - 1));
    if (q.list && q.indent.length <= p.indent.length) return Math.max(1, q.indent.length + markerOf(q.list).length + 1 - p.indent.length);
    if (!q.list && q.text.trim() && !q.indent) break;
    pos = prevStart;
  }
  return 2;
}

function listOutdentUnit(value, ls, p) {
  // Moving out one level means aligning with the parent item's marker.
  let pos = ls;
  for (let i = 0; i < 200 && pos > 0; i++) {
    const prevStart = lineStartOf(value, pos - 1);
    const q = parseLine(value.slice(prevStart, pos - 1));
    if (q.list && q.indent.length < p.indent.length) return p.indent.length - q.indent.length;
    if (!q.list && q.text.trim() && !q.indent) break;
    pos = prevStart;
  }
  return 2;
}

/** Tab / Shift+Tab: indents list items structurally, otherwise inserts or removes spaces. */
export function indentLines(value, start, end, outdent = false) {
  const [ls, le] = selectedLineRange(value, start, end);
  const lines = value.slice(ls, le).split('\n');
  const fence = inFence(value, ls);
  if (!outdent && start === end && lines.length === 1) {
    const p = parseLine(lines[0]);
    if (!(p.list && !fence)) return edit(value, start, start, fence ? '    ' : '  ');
  }
  let delta0 = 0, total = 0, pos = ls;
  const out = lines.map((line, i) => {
    const p = parseLine(line);
    const unit = p.list && !fence ? (outdent ? listOutdentUnit : listUnit)(value, pos, p) : fence ? 4 : 2;
    pos += line.length + 1;
    let res;
    if (outdent) {
      const lead = /^[ \t]*/.exec(line)[0];
      const n = lead.startsWith('\t') ? 1 : Math.min(lead.length, unit);
      res = line.slice(n);
    } else {
      res = line.trim() ? ' '.repeat(unit) + line : line;
    }
    if (i === 0) delta0 = res.length - line.length;
    total += res.length - line.length;
    return res;
  });
  const text = out.join('\n');
  const value2 = value.slice(0, ls) + text + value.slice(le);
  if (start === end) {
    const c = Math.max(ls, start + delta0);
    return { value: value2, start: c, end: c };
  }
  // A selection that began at a line start keeps covering whole lines.
  return { value: value2, start: start === ls ? ls : Math.max(ls, start + delta0), end: Math.max(ls, end + total) };
}

/** Inserts a block (table, rule, fence…) on its own lines, separated by blank lines. */
export function insertBlock(value, start, end, block, caretOffset = block.length) {
  const before = value.slice(0, start);
  const after = value.slice(end);
  const pre = !before || before.endsWith('\n\n') ? '' : before.endsWith('\n') ? '\n' : '\n\n';
  const post = !after ? '\n' : after.startsWith('\n\n') ? '' : after.startsWith('\n') ? '\n' : '\n\n';
  const text = pre + block + post;
  return edit(value, start, end, text, start + pre.length + caretOffset);
}

/** Wraps the selected lines in a code fence (or inserts an empty one). */
export function toggleCodeFence(value, start, end, lang = '') {
  const [ls, le] = selectedLineRange(value, start, end);
  const sel = value.slice(ls, le);
  if (start === end && !sel.trim()) {
    const r = insertBlock(value, ls, le, '```' + lang + '\n\n```', 4 + lang.length);
    return r;
  }
  const f = '`'.repeat(Math.max(3, maxRun(sel, '`') + 1));
  const block = f + lang + '\n' + sel + '\n' + f;
  const r = insertBlock(value, ls, le, block, f.length + lang.length + 1);
  r.end = r.start + sel.length;
  return r;
}

export const textOps = { parseLine, buildLine, wrapSelection, insertLink, toggleLines, continueOnEnter, indentLines, insertBlock, toggleCodeFence };

// =============================================================================================
// The editor

const SVG_NS = 'http://www.w3.org/2000/svg';
const ICONS = {
  heading: 'M6 4v16M18 4v16M6 12h12',
  code: 'M8.5 8l-4 4 4 4M15.5 8l4 4-4 4',
  link: 'M10 13.5a4 4 0 0 0 5.66.34l2.5-2.5a4 4 0 0 0-5.66-5.66l-1.3 1.3M14 10.5a4 4 0 0 0-5.66-.34l-2.5 2.5a4 4 0 0 0 5.66 5.66l1.3-1.3',
  ul: 'M9 6.5h11M9 12h11M9 17.5h11M4.5 6.5h.01M4.5 12h.01M4.5 17.5h.01',
  ol: 'M10 6.5h10M10 12h10M10 17.5h10M4 5l1.5-1v5M3.5 14.5c0-.8.7-1.5 1.5-1.5s1.5.6 1.5 1.3c0 1.2-3 2-3 3.7h3',
  task: 'M5.5 3.5a3 3 0 1 0 .01 0M4.5 6.5l.9.9 1.6-1.8M5.5 14a3 3 0 1 0 .01 0M11 6.5h9M11 17h9',
  quote: 'M7 17c2.2-.6 3.5-2.4 3.5-5V7h-5v5h5M16.5 17c2.2-.6 3.5-2.4 3.5-5V7h-5v5h5',
  codeblock: 'M4 5h16v14H4zM9.5 10l-2 2 2 2M14.5 10l2 2-2 2',
  table: 'M4 5h16v14H4zM4 10h16M4 14.5h16M10 10v9',
  image: 'M4 5h16v14H4zM4 16l4.5-4.5 4 4 2.5-2.5L20 18M15.5 9.5h.01',
  hr: 'M4 12h16M8 7h8M8 17h8',
};

function icon(doc, name) {
  const svg = doc.createElementNS(SVG_NS, 'svg');
  svg.setAttribute('viewBox', '0 0 24 24');
  svg.setAttribute('aria-hidden', 'true');
  svg.setAttribute('focusable', 'false');
  const path = doc.createElementNS(SVG_NS, 'path');
  path.setAttribute('d', ICONS[name]);
  svg.appendChild(path);
  return svg;
}

function h(doc, tag, attrs = {}, ...children) {
  const e = doc.createElement(tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v === null || v === undefined || v === false) continue;
    if (k === 'class') e.className = v;
    else e.setAttribute(k, v === true ? '' : String(v));
  }
  for (const c of children.flat()) if (c !== null && c !== undefined && c !== false) e.appendChild(typeof c === 'string' ? doc.createTextNode(c) : c);
  return e;
}

const isMac = () => /Mac|iPhone|iPad/.test((globalThis.navigator && (globalThis.navigator.platform || globalThis.navigator.userAgent)) || '');
const MODES = ['source', 'split', 'preview'];

/**
 * Creates the Markdown source editor.
 * @param {{container: HTMLElement, markdown?: string, onChange?: (md:string)=>void,
 *          onUploadImage?: (file: File)=>Promise<string>, placeholder?: string,
 *          mode?: 'source'|'split'|'preview', resolveImage?: (src:string)=>string|null,
 *          readOnly?: boolean, debounceMs?: number, autofocus?: boolean}} opts
 * @returns {{getMarkdown():string, setMarkdown(md:string):void, focus():void, destroy():void,
 *           exec(command:string, arg?:any):void, on(event:string, fn:Function):()=>void,
 *           flush():void, setMode(m:string):void, getMode():string, element:HTMLTextAreaElement}}
 *   events: "change" (md), "mode" (mode), "error" (Error)
 */
export function createMarkdownEditor(opts) {
  return new MarkdownEditor(opts).api;
}

class MarkdownEditor {
  constructor(o) {
    if (!o || !o.container) throw new TypeError('createMarkdownEditor: container is required');
    this.o = o;
    this.doc = o.container.ownerDocument;
    this.listeners = new Map();
    this.cleanups = [];
    this.debounceMs = o.debounceMs ?? 300;
    this.timer = null;
    this.previewTimer = null;
    this.dirty = false;
    this.syncing = false;
    this.uploadSeq = 0;
    this.build(o);
    this.ta.value = String(o.markdown ?? '');
    this.lastMd = this.ta.value;
    this.setMode(MODES.includes(o.mode) ? o.mode : 'split', true);
    this.renderPreview();
    this.bind();
    if (o.autofocus) this.focus();
    this.api = {
      getMarkdown: () => this.ta.value,
      setMarkdown: (md) => this.setMarkdown(md),
      focus: () => this.focus(),
      destroy: () => this.destroy(),
      exec: (cmd, arg) => this.exec(cmd, arg),
      on: (ev, fn) => this.on(ev, fn),
      flush: () => this.commit(),
      setMode: (m) => this.setMode(m),
      getMode: () => this.mode,
      element: this.ta,
    };
  }

  build(o) {
    const d = this.doc;
    const mod = isMac() ? String.fromCharCode(0x2318) : 'Ctrl+';
    const btn = (cmd, label, ic, shortcut) => {
      const b = h(d, 'button', { type: 'button', class: 're-btn', 'data-cmd': cmd, 'aria-label': label, title: shortcut ? `${label} (${shortcut})` : label, tabindex: '-1' });
      if (ICONS[ic]) b.appendChild(icon(d, ic));
      else b.appendChild(h(d, 'span', { class: 're-glyph re-glyph-' + cmd, 'aria-hidden': 'true' }, ic));
      return b;
    };
    const sep = () => h(d, 'span', { class: 're-sep', 'aria-hidden': 'true' });
    this.modeButtons = MODES.map((m) => h(d, 'button', { type: 'button', class: 'mde-mode', 'data-mode': m, 'aria-pressed': 'false' },
      m === 'source' ? 'Edit' : m === 'split' ? 'Split' : 'Preview'));
    this.toolbar = h(d, 'div', { class: 're-toolbar mde-toolbar', role: 'toolbar', 'aria-label': 'Markdown formatting' },
      btn('heading', 'Heading (cycle)', 'heading', mod + 'Alt+1..3'), sep(),
      btn('bold', 'Bold', 'B', mod + 'B'), btn('italic', 'Italic', 'I', mod + 'I'), btn('strike', 'Strikethrough', 'S', mod + 'Shift+X'),
      btn('code', 'Inline code', 'code', mod + 'E'), btn('link', 'Link', 'link', mod + 'K'), sep(),
      btn('bulletList', 'Bulleted list', 'ul', mod + 'Shift+8'), btn('orderedList', 'Numbered list', 'ol', mod + 'Shift+7'),
      btn('taskList', 'Checklist', 'task', mod + 'Shift+9'), btn('quote', 'Quote', 'quote', mod + 'Shift+.'), sep(),
      btn('codeBlock', 'Code block', 'codeblock', mod + 'Shift+C'), btn('table', 'Table', 'table'), btn('image', 'Image', 'image'), btn('hr', 'Divider', 'hr'),
      h(d, 'span', { class: 're-spacer' }),
      h(d, 'div', { class: 'mde-modes', role: 'group', 'aria-label': 'View' }, ...this.modeButtons));
    this.ta = h(d, 'textarea', {
      class: 'mde-source', spellcheck: 'true', autocapitalize: 'sentences', 'aria-label': o.ariaLabel || 'Markdown source',
      placeholder: o.placeholder ?? 'Write Markdown' + ELLIPSIS, readonly: o.readOnly ? true : null,
    });
    this.preview = h(d, 'div', { class: 'mde-preview md', 'aria-label': 'Preview', role: 'region', tabindex: '0' });
    this.body = h(d, 'div', { class: 'mde-body' }, this.ta, this.preview);
    this.fileInput = h(d, 'input', { type: 'file', accept: 'image/*', multiple: true, hidden: true, tabindex: '-1', 'aria-hidden': 'true' });
    this.root = h(d, 'div', { class: 'mde' }, this.toolbar, this.body, this.fileInput);
    o.container.appendChild(this.root);
  }

  listen(t, type, fn, opts) {
    t.addEventListener(type, fn, opts);
    this.cleanups.push(() => t.removeEventListener(type, fn, opts));
  }

  bind() {
    this.listen(this.toolbar, 'mousedown', (e) => { if (e.target.closest('button')) e.preventDefault(); });
    this.listen(this.toolbar, 'click', (e) => {
      const m = e.target.closest('button[data-mode]');
      if (m) { this.setMode(m.getAttribute('data-mode')); return; }
      const b = e.target.closest('button[data-cmd]');
      if (b) this.exec(b.getAttribute('data-cmd'));
    });
    this.listen(this.ta, 'keydown', (e) => this.onKeyDown(e));
    this.listen(this.ta, 'input', () => this.onInput());
    this.listen(this.ta, 'paste', (e) => this.onPaste(e));
    this.listen(this.ta, 'drop', (e) => this.onDrop(e));
    this.listen(this.ta, 'blur', () => this.commit());
    this.listen(this.ta, 'scroll', () => this.syncScroll(this.ta, this.preview), { passive: true });
    this.listen(this.preview, 'scroll', () => this.syncScroll(this.preview, this.ta), { passive: true });
    this.listen(this.fileInput, 'change', () => {
      const files = Array.from(this.fileInput.files || []);
      this.fileInput.value = '';
      this.uploadImages(files);
    });
  }

  on(ev, fn) {
    if (!this.listeners.has(ev)) this.listeners.set(ev, new Set());
    this.listeners.get(ev).add(fn);
    return () => this.listeners.get(ev).delete(fn);
  }

  emit(ev, arg) {
    for (const fn of this.listeners.get(ev) || []) {
      try { fn(arg); } catch (err) { if (ev !== 'error') this.emit('error', err); }
    }
  }

  // ------------------------------------------------------------------ text application

  /** Applies a {value,start,end} result as one native-undoable edit of the minimal range. */
  apply(res) {
    if (!res || this.o.readOnly) return false;
    const ta = this.ta;
    const old = ta.value, next = res.value;
    if (old !== next) {
      let a = 0;
      const max = Math.min(old.length, next.length);
      while (a < max && old.charCodeAt(a) === next.charCodeAt(a)) a++;
      let b = 0;
      while (b < max - a && old.charCodeAt(old.length - 1 - b) === next.charCodeAt(next.length - 1 - b)) b++;
      const insert = next.slice(a, next.length - b);
      ta.focus({ preventScroll: true });
      ta.setSelectionRange(a, old.length - b);
      let ok = false;
      try { ok = this.doc.execCommand('insertText', false, insert); } catch { ok = false; }
      if (!ok || ta.value !== next) {
        // Fallback (no execCommand support): set the text directly.
        ta.setRangeText(insert, a, old.length - b, 'end');
        if (ta.value !== next) ta.value = next;
        this.onInput();
      }
    }
    ta.setSelectionRange(res.start, res.end ?? res.start);
    return true;
  }

  op(fn, ...args) {
    const ta = this.ta;
    return this.apply(fn(ta.value, ta.selectionStart, ta.selectionEnd, ...args));
  }

  // ------------------------------------------------------------------ events

  onKeyDown(e) {
    if (this.o.readOnly) return;
    const mod = isMac() ? e.metaKey : e.ctrlKey;
    const key = e.key.length === 1 ? e.key.toLowerCase() : e.key;
    const altGraph = e.getModifierState && e.getModifierState('AltGraph');
    let cmd = null, arg;
    if (mod && e.altKey && !e.shiftKey && !altGraph) {
      const m = /^Digit([0-6])$/.exec(e.code);
      if (m) { cmd = 'heading'; arg = Number(m[1]); }
      else if (e.code === 'KeyP') cmd = 'togglePreview';
    } else if (mod && !e.altKey) {
      if (!e.shiftKey) cmd = { b: 'bold', i: 'italic', e: 'code', k: 'link' }[key] || null;
      else cmd = { x: 'strike', c: 'codeBlock' }[key] || { Digit7: 'orderedList', Digit8: 'bulletList', Digit9: 'taskList', Period: 'quote' }[e.code] || null;
    }
    if (cmd) { e.preventDefault(); this.exec(cmd, arg); return; }
    if (e.key === 'Escape') { this.tabEscapes = true; return; }
    if (e.key === 'Enter' && !mod && !e.shiftKey && !e.altKey && !e.isComposing) {
      this.tabEscapes = false;
      if (this.op(continueOnEnter)) e.preventDefault();
      return;
    }
    if (e.key === 'Tab' && !mod && !e.altKey) {
      // Esc, then Tab, moves focus out as usual so keyboard users are never trapped.
      if (this.tabEscapes) { this.tabEscapes = false; return; }
      e.preventDefault();
      this.op(indentLines, e.shiftKey);
      return;
    }
    this.tabEscapes = false;
  }

  onInput() {
    this.dirty = true;
    clearTimeout(this.timer);
    this.timer = setTimeout(() => this.commit(), this.debounceMs);
    this.schedulePreview();
  }

  commit() {
    clearTimeout(this.timer);
    this.timer = null;
    if (!this.dirty) return;
    this.dirty = false;
    const md = this.ta.value;
    if (md === this.lastMd) return;
    this.lastMd = md;
    this.emit('change', md);
    if (typeof this.o.onChange === 'function') {
      try { this.o.onChange(md); } catch (err) { this.emit('error', err); }
    }
  }

  onPaste(e) {
    if (this.o.readOnly) return;
    const dt = e.clipboardData;
    if (!dt) return;
    const files = Array.from(dt.files || []).filter((f) => /^image\//.test(f.type));
    const html = dt.getData('text/html');
    const plain = dt.getData('text/plain');
    if (files.length && (!html || !plain)) { e.preventDefault(); this.uploadImages(files); return; }
    // Rich content (web pages, docs) becomes Markdown; code editors' HTML stays plain text.
    if (html && !dt.getData('vscode-editor-data') && /<(?:h[1-6]|ul|ol|li|a\s|b>|strong|em>|i>|table|blockquote|img)\b/i.test(html)) {
      const md = htmlToMarkdown(html, this.doc);
      if (md.trim()) {
        e.preventDefault();
        const ta = this.ta;
        this.apply({ value: ta.value.slice(0, ta.selectionStart) + md + ta.value.slice(ta.selectionEnd), start: ta.selectionStart + md.length });
      }
    }
  }

  onDrop(e) {
    const files = Array.from((e.dataTransfer && e.dataTransfer.files) || []).filter((f) => /^image\//.test(f.type));
    if (!files.length || this.o.readOnly) return;
    e.preventDefault();
    this.uploadImages(files);
  }

  // ------------------------------------------------------------------ images

  uploadImages(files) {
    if (!files.length) return;
    if (typeof this.o.onUploadImage !== 'function') { this.emit('error', new Error('Image upload is not available')); return; }
    for (const file of files) {
      const name = (file.name || 'image').replace(/\.[a-z0-9]{1,5}$/i, '').replace(/[[\]\\]/g, '');
      const token = `![Uploading ${name}${ELLIPSIS} #${++this.uploadSeq}]()`;
      const ta = this.ta;
      this.apply({ value: ta.value.slice(0, ta.selectionStart) + token + ta.value.slice(ta.selectionEnd), start: ta.selectionStart + token.length });
      Promise.resolve()
        .then(() => this.o.onUploadImage(file))
        .then((url) => {
          const src = typeof url === 'string' ? safeImageSrc(url.trim()) : null;
          if (!src) throw new Error('Upload did not return a usable image URL');
          const dest = src.replace(/[()\s<>]/g, (c) => '%' + c.charCodeAt(0).toString(16).toUpperCase().padStart(2, '0'));
          this.replaceToken(token, `![${name}](${dest})`);
        })
        .catch((err) => {
          this.replaceToken(token, '');
          this.emit('error', err instanceof Error ? err : new Error(String(err)));
        });
    }
  }

  replaceToken(token, text) {
    const ta = this.ta;
    const i = ta.value.indexOf(token);
    if (i < 0) return;
    const { selectionStart: s, selectionEnd: e } = ta;
    const shift = (p) => (p > i ? p + text.length - token.length : p);
    this.apply({ value: ta.value.slice(0, i) + text + ta.value.slice(i + token.length), start: shift(s), end: shift(e) });
  }

  // ------------------------------------------------------------------ preview

  schedulePreview() {
    if (this.mode === 'source') return;
    clearTimeout(this.previewTimer);
    // Bigger documents re-render less eagerly.
    const delay = Math.min(1000, 80 + Math.floor(this.ta.value.length / 4000));
    this.previewTimer = setTimeout(() => this.renderPreview(), delay);
  }

  renderPreview() {
    clearTimeout(this.previewTimer);
    if (this.mode === 'source') return;
    const top = this.preview.scrollTop;
    const frag = renderToDOM(this.ta.value, this.doc, {
      resolveImage: this.o.resolveImage,
      onTaskToggle: this.o.readOnly ? null : (line, checked) => {
        const next = toggleTask(this.ta.value, line, checked);
        if (next !== this.ta.value) {
          const { selectionStart: s, selectionEnd: e } = this.ta;
          this.apply({ value: next, start: s, end: e });
        }
      },
    });
    this.preview.replaceChildren(frag);
    this.preview.scrollTop = top;
  }

  syncScroll(from, to) {
    if (this.mode !== 'split' || this.syncing) return;
    const max = from.scrollHeight - from.clientHeight;
    const ratio = max > 0 ? from.scrollTop / max : 0;
    this.syncing = true;
    to.scrollTop = ratio * (to.scrollHeight - to.clientHeight);
    this.doc.defaultView.requestAnimationFrame(() => { this.syncing = false; });
  }

  setMode(m, initial = false) {
    if (!MODES.includes(m)) return;
    this.mode = m;
    this.root.setAttribute('data-mode', m);
    for (const b of this.modeButtons) b.setAttribute('aria-pressed', String(b.getAttribute('data-mode') === m));
    if (m !== 'source' && !initial) this.renderPreview();
    if (!initial) this.emit('mode', m);
  }

  // ------------------------------------------------------------------ commands

  exec(cmd, arg) {
    if (this.o.readOnly && cmd !== 'togglePreview' && cmd !== 'mode') return;
    const ta = this.ta;
    switch (cmd) {
      case 'bold': this.op(wrapSelection, '**', 'bold text'); break;
      case 'italic': this.op(wrapSelection, '*', 'italic text'); break;
      case 'strike': this.op(wrapSelection, '~~', 'text'); break;
      case 'code': this.op(wrapSelection, '`', 'code'); break;
      case 'link': this.op(insertLink, typeof arg === 'string' ? arg : 'https://'); break;
      case 'heading': {
        if (typeof arg === 'number') { this.op(toggleLines, 'heading', arg); break; }
        const p = parseLine(ta.value.slice(lineStartOf(ta.value, ta.selectionStart), lineEndOf(ta.value, ta.selectionStart)));
        const next = p.heading >= 3 ? 0 : p.heading + 1;
        this.op(toggleLines, 'heading', next);
        break;
      }
      case 'paragraph': this.op(toggleLines, 'heading', 0); break;
      case 'bulletList': this.op(toggleLines, 'ul'); break;
      case 'orderedList': this.op(toggleLines, 'ol'); break;
      case 'taskList': this.op(toggleLines, 'task'); break;
      case 'quote': this.op(toggleLines, 'quote'); break;
      case 'codeBlock': this.op(toggleCodeFence, typeof arg === 'string' ? arg.replace(/[^\w+#.-]/g, '') : ''); break;
      case 'table': this.op(insertBlock, '| Column | Column |\n| --- | --- |\n|  |  |', 2); break;
      case 'hr': this.op(insertBlock, '---'); break;
      case 'image':
        if (typeof arg === 'string' && safeImageSrc(arg)) this.op(insertBlock, `![](${arg})`, 2);
        else if (typeof this.o.onUploadImage === 'function') this.fileInput.click();
        else this.emit('error', new Error('Image upload is not available'));
        break;
      case 'indent': this.op(indentLines, false); break;
      case 'outdent': this.op(indentLines, true); break;
      case 'insertText': if (typeof arg === 'string') this.apply({ value: ta.value.slice(0, ta.selectionStart) + arg + ta.value.slice(ta.selectionEnd), start: ta.selectionStart + arg.length }); break;
      case 'undo': ta.focus(); this.doc.execCommand('undo'); break;
      case 'redo': ta.focus(); this.doc.execCommand('redo'); break;
      case 'togglePreview': this.setMode(this.mode === 'preview' ? 'source' : 'preview'); break;
      case 'mode': this.setMode(arg); break;
      default: this.emit('error', new Error('Unknown editor command: ' + cmd));
    }
  }

  setMarkdown(md) {
    clearTimeout(this.timer);
    this.dirty = false;
    this.ta.value = String(md ?? '');
    this.lastMd = this.ta.value;
    this.renderPreview();
  }

  focus() {
    if (this.mode === 'preview') this.setMode('split');
    this.ta.focus({ preventScroll: true });
  }

  destroy() {
    this.commit();
    clearTimeout(this.timer);
    clearTimeout(this.previewTimer);
    for (const c of this.cleanups) c();
    this.cleanups = [];
    this.listeners.clear();
    this.root.remove();
  }
}
