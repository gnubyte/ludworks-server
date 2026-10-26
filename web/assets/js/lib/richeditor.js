/**
 * richeditor.js — iOS-Notes-like WYSIWYG editor whose content is restricted to what Markdown can
 * represent. Markdown is the source of truth: getMarkdown() serializes the editing DOM, and every
 * foreign input (paste, drop, browser-generated markup) is funnelled through Markdown again, which
 * doubles as the sanitizer.
 *
 * Exports:
 *   createRichEditor(opts)              the editor (browser only)
 *   domToMarkdown(root, opts)           DOM → Markdown serializer (needs only a tiny DOM subset:
 *                                       nodeType, nodeName, childNodes, nodeValue, getAttribute)
 *   markdownToEditorDOM(md, doc, opts)  Markdown → canonical editing DOM
 *   htmlToMarkdown(html, doc)           sanitize pasted HTML into Markdown (browser: DOMParser)
 *   plainTextToMarkdown(text)           heuristics for pasted plain text
 *   escapeMarkdown(text)                literal text → Markdown that renders exactly that text
 *
 * @module lib/richeditor
 */
import { parse, parseInline, safeLinkHref, safeImageSrc } from './markdown.js';
import { highlight, listLanguages, resolveLanguage, languageLabel, detectLanguage } from './highlight.js';

const ELEMENT_NODE = 1;
const TEXT_NODE = 3;
const ZWSP = String.fromCharCode(0x200b);
const NBSP = String.fromCharCode(0xa0);
const MAX_TABLE_COLS = 64;
const MAX_LIST_DEPTH = 12;

const tagOf = (n) => (n.nodeName || '').toUpperCase();
const attrOf = (n, name) => (n && n.getAttribute ? n.getAttribute(name) : null);
const kids = (n) => Array.from(n.childNodes || []);
const isBlankText = (s) => !s || /^[\s]*$/.test(s.split(ZWSP).join(''));

const BLOCK_TAGS = new Set([
  'P', 'DIV', 'H1', 'H2', 'H3', 'H4', 'H5', 'H6', 'BLOCKQUOTE', 'UL', 'OL', 'LI', 'PRE', 'TABLE', 'HR',
  'SECTION', 'ARTICLE', 'HEADER', 'FOOTER', 'MAIN', 'NAV', 'ASIDE', 'FIGURE', 'FIGCAPTION', 'ADDRESS',
  'DETAILS', 'SUMMARY', 'DL', 'DT', 'DD', 'CENTER', 'FIELDSET', 'LEGEND', 'BODY', 'HTML', 'THEAD',
  'TBODY', 'TFOOT', 'TR', 'TD', 'TH', 'CAPTION', 'HGROUP', 'FORM',
]);
const SKIP_TAGS = new Set([
  'SCRIPT', 'STYLE', 'TEMPLATE', 'NOSCRIPT', 'IFRAME', 'OBJECT', 'EMBED', 'SVG', 'MATH', 'CANVAS', 'VIDEO',
  'AUDIO', 'SELECT', 'TEXTAREA', 'BUTTON', 'HEAD', 'TITLE', 'META', 'LINK', 'BASE', 'OPTION', 'OPTGROUP',
  'DATALIST', 'MAP', 'AREA', 'SOURCE', 'TRACK', 'PARAM', 'FRAME', 'FRAMESET', 'DIALOG', 'INPUT', 'COLGROUP', 'COL',
]);
const HEADING_LEVEL = { H1: 1, H2: 2, H3: 3, H4: 4, H5: 5, H6: 6 };

// =============================================================================================
// DOM → Markdown

const NO_MARKS = Object.freeze({ strong: false, em: false, del: false, link: null });
const MARK_ORDER = ['link', 'strong', 'em', 'del'];
const sameLink = (a, b) => a === b || (!!a && !!b && a.href === b.href && a.title === b.title);
const sameMarks = (a, b) => a.strong === b.strong && a.em === b.em && a.del === b.del && sameLink(a.link, b.link);
const markOn = (m, k) => (k === 'link' ? !!m.link : m[k]);
const withMark = (m, k, v = true) => ({ ...m, [k]: v });

const FLAG_PARA = { lineStart: true };
const FLAG_HEADING = { heading: true };
const FLAG_TABLE = { table: true };

const isUiNode = (n) => attrOf(n, 'data-re-ui') !== null;

/**
 * Serializes a DOM subtree (the editor surface, or any HTML) to Markdown.
 * @param {Node|Node[]} root element whose children are serialized, or an array of sibling nodes
 * @param {{pasted?: boolean, styles?: boolean}} [opts] pasted: collapse HTML whitespace, drop
 *   empty paragraphs and fragment-only links; styles: honour inline font-weight/style/decoration
 * @returns {string}
 */
export function domToMarkdown(root, opts = {}) {
  const ctx = { pasted: !!opts.pasted, styles: !!(opts.styles || opts.pasted) };
  const nodes = Array.isArray(root) ? root : kids(root);
  return joinBlocks(serializeChildren(nodes, ctx));
}

function serializeChildren(nodes, ctx) {
  const blocks = [];
  let run = [];
  const flush = () => {
    if (!run.length) return;
    const md = inlineMarkdown(run, ctx, FLAG_PARA);
    run = [];
    if (md) blocks.push({ kind: 'paragraph', md });
  };
  for (const n of nodes) {
    if (n.nodeType === TEXT_NODE) { run.push(n); continue; }
    if (n.nodeType !== ELEMENT_NODE) continue;
    const tag = tagOf(n);
    if (SKIP_TAGS.has(tag) || isUiNode(n)) continue;
    if (!BLOCK_TAGS.has(tag)) { run.push(n); continue; }
    flush();
    serializeBlock(n, tag, ctx, blocks);
  }
  flush();
  return blocks;
}

function hasBlockChild(n) {
  return kids(n).some((c) => c.nodeType === ELEMENT_NODE && BLOCK_TAGS.has(tagOf(c)));
}

function serializeBlock(n, tag, ctx, blocks) {
  const level = HEADING_LEVEL[tag];
  if (level) {
    const md = inlineMarkdown(kids(n), ctx, FLAG_HEADING);
    if (!md && ctx.pasted) return;
    blocks.push({ kind: 'heading', md: '#'.repeat(level) + (md ? ' ' + escapeHeadingTail(md) : '') });
    return;
  }
  switch (tag) {
    case 'BLOCKQUOTE': {
      const inner = joinBlocks(serializeChildren(kids(n), ctx));
      if (inner) blocks.push({ kind: 'quote', md: prefixLines(inner, '> ', '>') });
      else if (!ctx.pasted) blocks.push({ kind: 'empty' });
      return;
    }
    case 'UL':
    case 'OL': {
      const b = listBlock(n, tag, ctx, blocks[blocks.length - 1]);
      if (b) blocks.push(b);
      return;
    }
    case 'LI': {
      const b = itemsToList([{ li: n, nodes: kids(n) }], 'UL', null, ctx, blocks[blocks.length - 1]);
      if (b) blocks.push(b);
      return;
    }
    case 'PRE':
      blocks.push({ kind: 'code', md: codeBlockMarkdown(n) });
      return;
    case 'TABLE': {
      const md = tableMarkdown(n, ctx);
      if (md) blocks.push({ kind: 'table', md });
      return;
    }
    case 'HR':
      blocks.push({ kind: 'hr', md: '---' });
      return;
    default: {
      if (hasBlockChild(n)) {
        for (const b of serializeChildren(kids(n), ctx)) blocks.push(b);
        return;
      }
      const md = inlineMarkdown(kids(n), ctx, FLAG_PARA);
      if (md) blocks.push({ kind: 'paragraph', md });
      else if (!ctx.pasted && (tag === 'P' || tag === 'DIV')) blocks.push({ kind: 'empty' });
    }
  }
}

/** Joins blocks with blank lines. Empty paragraphs in the middle survive as "&nbsp;". */
function joinBlocks(blocks) {
  let s = 0, e = blocks.length;
  while (s < e && blocks[s].kind === 'empty') s++;
  while (e > s && blocks[e - 1].kind === 'empty') e--;
  const out = [];
  for (let i = s; i < e; i++) out.push(blocks[i].kind === 'empty' ? '&nbsp;' : blocks[i].md);
  return out.join('\n\n');
}

function prefixLines(text, prefix, blankPrefix) {
  return text.split('\n').map((l) => (l ? prefix + l : blankPrefix)).join('\n');
}

function escapeHeadingTail(md) {
  // "# foo #" would lose its trailing hashes as a closing sequence.
  const m = /(^|[ \t])(#+)$/.exec(md);
  return m ? md.slice(0, m.index + m[1].length) + '\\' + m[2] : md;
}

// --- lists ---

function listBlock(listEl, tag, ctx, prev) {
  const entries = [];
  const addLoose = (node) => {
    const last = entries[entries.length - 1];
    if (last && last.synthetic) last.nodes.push(node);
    else entries.push({ li: null, nodes: [node], synthetic: true });
  };
  for (const c of kids(listEl)) {
    if (c.nodeType === TEXT_NODE) {
      if (!isBlankText(c.nodeValue)) addLoose(c);
      continue;
    }
    if (c.nodeType !== ELEMENT_NODE) continue;
    const t = tagOf(c);
    if (SKIP_TAGS.has(t) || isUiNode(c)) continue;
    if (t === 'LI') entries.push({ li: c, nodes: kids(c) });
    else if ((t === 'UL' || t === 'OL') && entries.length) entries[entries.length - 1].nodes.push(c);
    else addLoose(c);
  }
  return itemsToList(entries, tag, attrOf(listEl, 'start'), ctx, prev);
}

function clampStart(s) {
  const n = parseInt(s, 10);
  return Number.isFinite(n) && n >= 0 ? Math.min(n, 999999999) : 1;
}

function isCheckbox(n) {
  return !!n && n.nodeType === ELEMENT_NODE && tagOf(n) === 'INPUT' && (attrOf(n, 'type') || '').toLowerCase() === 'checkbox';
}

function extractTask(entry, ctx) {
  const { li } = entry;
  const nodes = entry.nodes;
  if (li) {
    const dc = attrOf(li, 'data-checked');
    if (dc === 'true' || dc === 'false') return { task: dc === 'true', nodes };
  }
  // Pasted HTML (GitHub & co.): <li><input type=checkbox checked> text</li>, possibly inside a <p>.
  const idx = nodes.findIndex((n) => !(n.nodeType === TEXT_NODE && isBlankText(n.nodeValue)));
  const first = nodes[idx];
  if (isCheckbox(first)) return { task: attrOf(first, 'checked') !== null, nodes: nodes.filter((_, i) => i !== idx) };
  if (ctx.pasted && first && first.nodeType === ELEMENT_NODE && tagOf(first) === 'P') {
    const inner = kids(first);
    const j = inner.findIndex((n) => !(n.nodeType === TEXT_NODE && isBlankText(n.nodeValue)));
    if (isCheckbox(inner[j])) {
      const checked = attrOf(inner[j], 'checked') !== null;
      first.removeChild(inner[j]); // pasted documents are disposable
      return { task: checked, nodes };
    }
  }
  return { task: null, nodes };
}

function itemsToList(entries, tag, startAttr, ctx, prev) {
  if (!entries.length) return null;
  const ordered = tag === 'OL';
  const prevSame = prev && prev.kind === 'list' && prev.ordered === ordered;
  // Adjacent lists of the same kind would merge on re-parse; alternating the marker keeps them apart.
  const delim = ordered ? (prevSame && prev.delim === '.' ? ')' : '.') : (prevSame && prev.delim === '-' ? '*' : '-');
  let num = ordered ? clampStart(startAttr) : 1;
  const start = num;
  const items = [];
  let loose = false, emptyFirst = false;
  entries.forEach((e, idx) => {
    const { task, nodes } = extractTask(e, ctx);
    const blocks = serializeChildren(nodes, ctx).filter((b) => b.kind !== 'empty');
    const marker = ordered ? `${Math.min(num++, 999999999)}${delim}` : delim;
    const r = itemMarkdown(marker, task, blocks);
    if (r.loose) loose = true;
    if (idx === 0 && r.emptyHead) emptyFirst = true;
    items.push(r.md);
  });
  return { kind: 'list', ordered, delim, start, emptyFirst, md: items.join(loose ? '\n\n' : '\n') };
}

function itemMarkdown(marker, task, blocks) {
  const indent = ' '.repeat(marker.length + 1);
  const parts = blocks.slice();
  let text = task === null ? '' : task ? '[x]' : '[ ]';
  if (parts.length && parts[0].kind === 'paragraph') {
    const p = parts.shift().md;
    text = text ? text + ' ' + p : p;
  }
  const emptyHead = !text;
  let loose = false;
  for (const p of parts) {
    if (!text) { text = '\n' + p.md; continue; }
    // Code and lists may follow the item's text directly; anything else needs a blank line.
    // An ordered list not starting at 1, or a list whose first item is empty, cannot interrupt
    // a paragraph, so those get a blank line too.
    const tight = p.kind === 'code' || (p.kind === 'list' && !p.emptyFirst && !(p.ordered && p.start !== 1));
    if (!tight) loose = true;
    text += (tight ? '\n' : '\n\n') + p.md;
  }
  const lines = text.split('\n');
  let md = marker + (lines[0] ? ' ' + lines[0] : '');
  for (let i = 1; i < lines.length; i++) md += '\n' + (lines[i] ? indent + lines[i] : '');
  return { md, loose, emptyHead };
}

// --- code blocks ---

function codeText(root) {
  let out = '';
  let pending = false;
  const emit = (s) => {
    if (pending) { out += '\n'; pending = false; }
    out += s;
  };
  const walk = (n) => {
    for (const c of kids(n)) {
      if (c.nodeType === TEXT_NODE) { if (c.nodeValue) emit(c.nodeValue); continue; }
      if (c.nodeType !== ELEMENT_NODE) continue;
      const t = tagOf(c);
      if (SKIP_TAGS.has(t) || isUiNode(c)) continue;
      if (t === 'BR') { if (pending) emit(''); pending = true; continue; }
      const block = BLOCK_TAGS.has(t);
      if (block && out && !out.endsWith('\n')) pending = true;
      walk(c);
      if (block) pending = true;
    }
  };
  walk(root);
  return out;
}

function sanitizeLang(lang) {
  return String(lang || '').replace(/[^\w+#.-]/g, '').slice(0, 40);
}

function codeBlockMarkdown(pre) {
  const codeEl = kids(pre).find((c) => c.nodeType === ELEMENT_NODE && tagOf(c) === 'CODE');
  let lang = attrOf(pre, 'data-lang') || '';
  if (!lang) {
    const m = /(?:^|\s)(?:language|lang)-(\S+)/.exec(`${attrOf(pre, 'class') || ''} ${codeEl ? attrOf(codeEl, 'class') || '' : ''}`);
    if (m) lang = m[1];
  }
  let text = codeText(pre).split(ZWSP).join('').split(NBSP).join(' ').replace(/\r\n?/g, '\n');
  if (text.endsWith('\n')) text = text.slice(0, -1); // editor sentinel / conventional trailing newline
  return fencedCode(text, lang);
}

// --- tables ---

function cellAlignOf(cell, ctx) {
  if (!cell) return null;
  const a = (attrOf(cell, 'data-align') || attrOf(cell, 'align') || '').toLowerCase();
  if (a === 'left' || a === 'center' || a === 'right') return a;
  const cls = /md-align-(left|center|right)/.exec(attrOf(cell, 'class') || '');
  if (cls) return cls[1];
  if (ctx.styles) {
    const st = /text-align\s*:\s*(left|center|right)/i.exec(attrOf(cell, 'style') || '');
    if (st) return st[1].toLowerCase();
  }
  return null;
}

function tableMarkdown(table, ctx) {
  const rows = [];
  const collect = (n) => {
    for (const c of kids(n)) {
      if (c.nodeType !== ELEMENT_NODE) continue;
      const t = tagOf(c);
      if (t === 'TR') rows.push(kids(c).filter((x) => x.nodeType === ELEMENT_NODE && (tagOf(x) === 'TD' || tagOf(x) === 'TH')));
      else if (t === 'THEAD' || t === 'TBODY' || t === 'TFOOT') collect(c);
    }
  };
  collect(table);
  const width = Math.min(MAX_TABLE_COLS, rows.reduce((w, r) => Math.max(w, r.length), 0));
  if (!width) return '';
  const cell = (c) => (c ? inlineMarkdown(kids(c), ctx, FLAG_TABLE) : '');
  const line = (cells) => '| ' + cells.join(' | ') + ' |';
  const head = rows[0];
  const cols = Array.from({ length: width }, (_, i) => i);
  const delim = cols.map((i) => {
    const a = cellAlignOf(head[i], ctx);
    return a === 'center' ? ':---:' : a === 'right' ? '---:' : a === 'left' ? ':---' : '---';
  });
  const out = [line(cols.map((i) => cell(head[i]))), line(delim)];
  for (const r of rows.slice(1)) out.push(line(cols.map((i) => cell(r[i]))));
  return out.join('\n');
}

// --- inline ---

function cleanText(t, ctx) {
  if (t.indexOf(ZWSP) !== -1) t = t.split(ZWSP).join('');
  if (t.indexOf(NBSP) !== -1) t = t.split(NBSP).join(' ');
  return ctx.pasted ? t.replace(/[ \t\r\n\f]+/g, ' ') : t.replace(/[\r\n]+/g, ' ');
}

function styleMarks(m, style) {
  if (/font-weight\s*:\s*(?:bold|bolder|[6-9]00)/i.test(style)) m = withMark(m, 'strong');
  else if (/font-weight\s*:\s*(?:normal|lighter|[1-5]00)/i.test(style)) m = withMark(m, 'strong', false);
  if (/font-style\s*:\s*(?:italic|oblique)/i.test(style)) m = withMark(m, 'em');
  else if (/font-style\s*:\s*normal/i.test(style)) m = withMark(m, 'em', false);
  if (/text-decoration[^;]*line-through/i.test(style)) m = withMark(m, 'del');
  return m;
}

function textOf(n) {
  if (n.nodeType === TEXT_NODE) return n.nodeValue || '';
  let s = '';
  for (const c of kids(n)) {
    if (c.nodeType === ELEMENT_NODE && tagOf(c) === 'BR') s += ' ';
    else s += textOf(c);
  }
  return s;
}

function collectInline(nodes, marks, ctx, out, depth) {
  for (const n of nodes) {
    if (n.nodeType === TEXT_NODE) {
      const t = cleanText(n.nodeValue || '', ctx);
      if (t) out.push({ k: 'text', t, m: marks });
      continue;
    }
    if (n.nodeType !== ELEMENT_NODE) continue;
    const tag = tagOf(n);
    if (SKIP_TAGS.has(tag) || isUiNode(n)) continue;
    if (tag === 'BR') { out.push({ k: 'br' }); continue; }
    if (tag === 'IMG') {
      if (attrOf(n, 'data-uploading') !== null) continue;
      let src = (attrOf(n, 'data-md-src') || '').trim();
      if (!src) {
        src = (attrOf(n, 'src') || '').trim();
        // Relative/blob sources in foreign HTML point at the source site's files: keep alt text only.
        if (ctx.pasted && !/^https:\/\//i.test(src) && !src.startsWith('/api/')) src = '';
      }
      out.push({ k: 'img', src, alt: cleanText(attrOf(n, 'alt') || '', { pasted: true }).trim(), title: attrOf(n, 'title') || '', m: marks });
      continue;
    }
    if (tag === 'CODE' || tag === 'KBD' || tag === 'SAMP' || tag === 'TT') {
      const t = cleanText(textOf(n), { pasted: ctx.pasted }).replace(/[\r\n]/g, ' ');
      if (t) out.push({ k: 'code', t, m: marks });
      continue;
    }
    if (depth > 200) { // pathological nesting: keep the text
      const t = cleanText(textOf(n), ctx);
      if (t) out.push({ k: 'text', t, m: marks });
      continue;
    }
    if (BLOCK_TAGS.has(tag)) { // a block inside inline context: treat its edges as line breaks
      if (out.length && out[out.length - 1].k !== 'br') out.push({ k: 'br' });
      collectInline(kids(n), marks, ctx, out, depth + 1);
      out.push({ k: 'br' });
      continue;
    }
    let m = marks;
    switch (tag) {
      case 'STRONG': case 'B': m = withMark(m, 'strong'); break;
      case 'EM': case 'I': case 'CITE': case 'DFN': case 'VAR': m = withMark(m, 'em'); break;
      case 'S': case 'DEL': case 'STRIKE': m = withMark(m, 'del'); break;
      case 'A': {
        const href = (attrOf(n, 'href') || '').trim();
        const usable = href && safeLinkHref(href) !== null && !(ctx.pasted && href.startsWith('#'));
        if (usable) m = withMark(m, 'link', { href, title: attrOf(n, 'title') || '', auto: attrOf(n, 'data-md-autolink') || '' });
        break;
      }
      default: break;
    }
    if (ctx.styles) {
      const st = attrOf(n, 'style');
      if (st) m = styleMarks(m, st);
    }
    collectInline(kids(n), m, ctx, out, depth + 1);
  }
  return out;
}

function mergeSegments(segs) {
  const out = [];
  for (const s of segs) {
    const last = out[out.length - 1];
    if (s.k === 'text' && last && last.k === 'text' && sameMarks(last.m, s.m)) last.t += s.t;
    else out.push(s.k === 'text' ? { ...s } : s);
  }
  // A link whose text equals its URL is written as an autolink.
  const res = [];
  for (let i = 0; i < out.length; i++) {
    const s = out[i];
    const link = s.m && s.m.link;
    if (!link || s.k !== 'text') { res.push(s); continue; }
    let j = i;
    while (j + 1 < out.length && out[j + 1].m && sameLink(out[j + 1].m.link, link)) j++;
    const group = out.slice(i, j + 1);
    const plain = group.length === 1 && !link.title;
    if (plain) {
      const text = s.t;
      const kind = link.href === text || (link.href === 'http://' + text && /^www\./i.test(text)) ? 'url'
        : link.href.toLowerCase() === 'mailto:' + text.toLowerCase() ? 'email' : '';
      if (kind) {
        res.push({ k: 'auto', kind, text, href: link.href, prefer: link.auto, m: withMark(s.m, 'link', null) });
        continue;
      }
    }
    for (const g of group) res.push(g);
    i = j;
  }
  return res;
}

const isWsChar = (c) => c === '' || c === ' ' || c === '\t' || c === '\n';
const isAlnum = (c) => !!c && /[\p{L}\p{N}]/u.test(c);
const isAsciiPunct = (c) => !!c && /[!-/:-@[-`{-~]/.test(c);
const reEntityLike = /^&(?:#\d{1,7}|#[xX][\da-fA-F]{1,6}|[A-Za-z][A-Za-z0-9]{1,31});/;

/** Escapes Markdown-significant characters in literal text (inline context). */
function escapeText(t, prev, next, flags) {
  let out = '';
  for (let i = 0; i < t.length; i++) {
    const c = t[i];
    const p = i ? t[i - 1] : prev;
    const n = i + 1 < t.length ? t[i + 1] : next;
    switch (c) {
      case '\\': out += n === '' || isAsciiPunct(n) ? '\\\\' : '\\'; break;
      case '*': out += isWsChar(p) && isWsChar(n) ? '*' : '\\*'; break;
      case '_': out += isAlnum(p) && isAlnum(n) ? '_' : '\\_'; break;
      case '~': out += p === '~' || n === '~' || p === '' || n === '' ? '\\~' : '~'; break;
      case '`': case '[': case ']': out += '\\' + c; break;
      case '<': out += /[A-Za-z]/.test(n || '') ? '\\<' : '<'; break;
      case '!': out += n === '[' ? '\\!' : '!'; break; // "!" + link would become an image
      case '&': out += reEntityLike.test(t.slice(i, i + 40)) ? '\\&' : '&'; break;
      case '|': out += flags.table ? '\\|' : '|'; break;
      default: out += c;
    }
  }
  return out;
}

/** Escapes constructs that only matter at the start of a line (block syntax). */
function escapeLineStart(e) {
  let m;
  if (/^#{1,6}(?=[ \t]|$)/.test(e) || /^>/.test(e) || /^[-+*](?=[ \t]|$)/.test(e)) return '\\' + e;
  if ((m = /^(\d{1,9})([.)])(?=[ \t]|$)/.exec(e))) return m[1] + '\\' + e.slice(m[1].length);
  if (/^(?:=+|-+)[ \t]*$/.test(e)) return '\\' + e;
  return e;
}

function codeSpan(t, inTable) {
  if (inTable) t = t.replace(/\|/g, '\\|');
  let run = 0, max = 0;
  for (let i = 0; i < t.length; i++) {
    if (t.charCodeAt(i) === 96) { run++; if (run > max) max = run; } else run = 0;
  }
  const f = '`'.repeat(max + 1);
  const pad = t.startsWith('`') || t.endsWith('`') || (t.startsWith(' ') && t.endsWith(' ') && t.trim()) ? ' ' : '';
  return f + pad + t + pad + f;
}

function formatDest(url) {
  let u = '';
  for (const ch of String(url)) {
    const c = ch.codePointAt(0);
    u += c <= 0x20 || c === 0x7f || ch === '<' || ch === '>' || ch === '\\' ? encodeURIComponent(ch) : ch;
  }
  let depth = 0, balanced = true;
  for (const ch of u) {
    if (ch === '(') depth++;
    else if (ch === ')' && --depth < 0) { balanced = false; break; }
  }
  if (!balanced || depth !== 0) u = u.replace(/[()]/g, (c) => '\\' + c);
  return u || '<>';
}

const formatTitle = (t) => (t ? ' "' + t.replace(/[\\"]/g, (c) => '\\' + c).replace(/\s+/g, ' ') + '"' : '');

function imageMarkdown(seg) {
  if (!seg.src || safeImageSrc(seg.src) === null) return seg.alt ? escapeText(seg.alt, '', '', {}) : '';
  return '![' + escapeText(seg.alt, '', '', {}) + '](' + formatDest(seg.src) + formatTitle(seg.title) + ')';
}

function isSingleAutolink(md, kind, href) {
  const nodes = parseInline(md);
  return nodes.length === 1 && nodes[0].type === 'link' && nodes[0].autolink === kind && nodes[0].url === href;
}

class InlineWriter {
  constructor(flags) {
    this.flags = flags;
    this.out = [];
    this.stack = [];
  }

  last() {
    for (let i = this.out.length - 1; i >= 0; i--) {
      const s = this.out[i];
      if (s) return s[s.length - 1];
    }
    return '';
  }

  atLineStart() {
    const c = this.last();
    return c === '' || c === '\n';
  }

  push(s) { if (s) this.out.push(s); }

  trimTrailingWs() {
    let ws = '';
    while (this.out.length) {
      const s = this.out[this.out.length - 1];
      let e = s.length;
      while (e > 0 && (s.charCodeAt(e - 1) === 32 || s.charCodeAt(e - 1) === 9)) e--;
      ws = s.slice(e) + ws;
      if (e > 0) { this.out[this.out.length - 1] = s.slice(0, e); break; }
      this.out.pop();
    }
    return ws;
  }

  open(k, m) {
    const opener = k === 'link' ? '[' : k === 'strong' ? '**' : k === 'em' ? '*' : '~~';
    this.out.push(opener);
    this.stack.push({ type: k, link: k === 'link' ? m.link : null, index: this.out.length - 1 });
  }

  closeTop() {
    const top = this.stack.pop();
    const ws = this.trimTrailingWs();
    if (this.out.length === top.index + 1) {
      this.out.pop(); // nothing inside: drop the opener instead of writing "****"
    } else if (top.type === 'link') {
      this.out.push('](' + formatDest(top.link.href) + formatTitle(top.link.title) + ')');
    } else {
      this.out.push(top.type === 'strong' ? '**' : top.type === 'em' ? '*' : '~~');
    }
    if (ws && !this.atLineStart()) this.out.push(ws);
  }

  closeAll() { while (this.stack.length) this.closeTop(); }

  /**
   * Closes marks the segment does not have and returns the marks to open. Marks that stay
   * active longer are opened first (outermost) so "***a** b*" does not split the emphasis.
   */
  sync(m, persist) {
    let keep = 0;
    while (keep < this.stack.length && markOn(m, this.stack[keep].type) && (this.stack[keep].type !== 'link' || sameLink(this.stack[keep].link, m.link))) keep++;
    while (this.stack.length > keep) this.closeTop();
    const open = MARK_ORDER.filter((k) => markOn(m, k) && !this.stack.some((s) => s.type === k));
    if (persist && open.length > 1) {
      const rank = new Map(open.map((k) => [k, persist(k)]));
      open.sort((a, b) => rank.get(b) - rank.get(a) || MARK_ORDER.indexOf(a) - MARK_ORDER.indexOf(b));
    }
    return open;
  }

  lineBreak() {
    this.closeAll();
    if (this.flags.heading || this.flags.table) {
      if (!isWsChar(this.last())) this.out.push(' ');
      return;
    }
    this.trimTrailingWs();
    this.out.push('\n');
  }

  text(seg, next, persist) {
    const toOpen = this.sync(seg.m, persist);
    let t = seg.t;
    if (this.atLineStart()) t = t.replace(/^[ \t]+/, '');
    if (!t) return;
    if (toOpen.length) {
      const lead = /^[ \t]*/.exec(t)[0];
      if (lead.length === t.length) { this.push(t); return; } // whitespace only: never wrap it in marks
      if (lead) { this.push(lead); t = t.slice(lead.length); }
      for (const k of toOpen) this.open(k, seg.m);
    }
    const lineStart = this.atLineStart();
    let e = escapeText(t, this.last(), next, this.flags);
    if (lineStart && this.flags.lineStart) e = escapeLineStart(e);
    this.push(e);
  }

  atom(seg, next, persist) {
    for (const k of this.sync(seg.m, persist)) this.open(k, seg.m);
    if (seg.k === 'code') this.push(codeSpan(seg.t, this.flags.table));
    else if (seg.k === 'img') this.push(imageMarkdown(seg));
    else if (seg.k === 'auto') this.push(this.autolink(seg, next));
  }

  autolink(seg, next) {
    const prev = this.last();
    if (seg.prefer === 'bare' || seg.prefer === 'email') {
      const leftOk = seg.kind === 'email' ? !/[A-Za-z0-9.+_-]/.test(prev) : prev === '' || /[\s*_~(]/.test(prev);
      const rightOk = seg.kind === 'email' ? !/[A-Za-z0-9_-]/.test(next) : next === '' || /[\s*_~.,:;!?'"]/.test(next);
      if (leftOk && rightOk && !this.flags.table && isSingleAutolink(seg.text, seg.kind === 'email' ? 'email' : 'bare', seg.href)) return seg.text;
    }
    const angle = '<' + (seg.kind === 'email' ? seg.text : seg.href) + '>';
    if (!this.flags.table && isSingleAutolink(angle, seg.kind === 'email' ? 'email' : 'angle', seg.href)) return angle;
    return '[' + escapeText(seg.text, '', '', this.flags) + '](' + formatDest(seg.href) + ')';
  }

  finish() {
    this.closeAll();
    this.trimTrailingWs();
    let s = this.out.join('');
    let a = 0, b = s.length;
    while (a < b && s.charCodeAt(a) === 10) a++;
    while (b > a && (s.charCodeAt(b - 1) === 10 || s.charCodeAt(b - 1) === 32)) b--;
    s = s.slice(a, b);
    return s;
  }
}

/** First character that will follow segment `s` in the output (decides flanking-sensitive escapes). */
function nextCharAfter(s, nx) {
  if (!nx || nx.k === 'br') return '';
  if (!sameMarks(nx.m, s.m)) {
    // Closing delimiters are written before opening ones.
    if (s.m.link && !sameLink(s.m.link, nx.m.link)) return ']';
    if ((s.m.strong && !nx.m.strong) || (s.m.em && !nx.m.em)) return '*';
    if (s.m.del && !nx.m.del) return '~';
    if (nx.m.link && !sameLink(s.m.link, nx.m.link)) return '[';
    return nx.m.strong || nx.m.em ? '*' : '~';
  }
  if (nx.k === 'text') return nx.t[0];
  if (nx.k === 'auto') return nx.prefer ? nx.text[0] : '<';
  if (nx.k === 'img') return '!';
  if (nx.k === 'code') return '`';
  return '';
}

function inlineMarkdown(nodes, ctx, flags) {
  const segs = mergeSegments(collectInline(nodes, NO_MARKS, ctx, [], 0));
  const w = new InlineWriter(flags);
  for (let i = 0; i < segs.length; i++) {
    const s = segs[i];
    if (s.k === 'br') { w.lineBreak(); continue; }
    const next = nextCharAfter(s, segs[i + 1]);
    // How many segments from here on keep mark k (used to nest longer-lived marks outside).
    const persist = (k) => {
      let n = 0;
      for (let j = i; j < segs.length && segs[j].m && markOn(segs[j].m, k) && (k !== 'link' || sameLink(segs[j].m.link, s.m.link)); j++) n++;
      return n;
    };
    if (s.k === 'text') w.text(s, next, persist);
    else w.atom(s, next, persist);
  }
  return w.finish();
}

/**
 * Escapes literal text so it renders exactly as written (line breaks preserved, blank lines
 * separate paragraphs).
 * @param {string} text
 * @returns {string}
 */
export function escapeMarkdown(text) {
  return String(text ?? '')
    .replace(/\r\n?/g, '\n')
    .split(/\n[ \t]*\n+/)
    .map((para) => para.split('\n').map((line) => {
      const t = line.replace(/^[ \t]+|[ \t]+$/g, '');
      return t ? escapeLineStart(escapeText(t, '', '', {})) : '';
    }).filter(Boolean).join('\n'))
    .filter(Boolean)
    .join('\n\n');
}

/**
 * Converts pasted HTML to Markdown using an inert DOMParser document (scripts never run,
 * resources never load).
 * @param {string} html
 * @param {Document} [doc]
 * @returns {string}
 */
export function htmlToMarkdown(html, doc = globalThis.document) {
  const Parser = (doc && doc.defaultView && doc.defaultView.DOMParser) || globalThis.DOMParser;
  const parsed = new Parser().parseFromString(String(html ?? ''), 'text/html');
  return domToMarkdown(parsed.body, { pasted: true });
}

const reLooksMarkdown = /^(?:#{1,6} \S|[-*+] \S|\d{1,9}[.)] \S|> |```|~~~|\|.*\|\s*$|[-*+] \[[ xX]\] )/m;
const reInlineMarkdown = /\[[^\]\n]+\]\([^)\s]+\)|\*\*[^*\n]+\*\*|`[^`\n]+`|~~[^~\n]+~~/;

/**
 * Pasted plain text: Markdown stays Markdown, code becomes a fenced block, anything else is
 * escaped literally.
 * @param {string} text
 * @returns {string}
 */
export function plainTextToMarkdown(text) {
  const t = String(text ?? '').replace(/\r\n?/g, '\n');
  if (!t.trim()) return '';
  if (reLooksMarkdown.test(t) || reInlineMarkdown.test(t)) return t;
  const lines = t.split('\n');
  const codey = lines.filter((l) => /[{};]\s*$|^\s*(?:def|class|import|from|function|const|let|var|public|private|return|#include|package|func|fn|if|for|while)\b|^\s*(?:\/\/|#!)/.test(l)).length;
  const indented = lines.filter((l) => /^(?: {2,}|\t)\S/.test(l)).length;
  if (lines.length >= 2 && (codey >= 2 || (indented >= 2 && codey >= 1))) {
    const body = t.replace(/\n+$/, '');
    return fencedCode(body, detectLanguage(body) || '');
  }
  return escapeMarkdown(t);
}

/** A fenced code block whose fence is longer than any backtick run inside the code. */
function fencedCode(text, lang) {
  let run = 0, max = 0;
  for (let i = 0; i < text.length; i++) {
    if (text.charCodeAt(i) === 96) { run++; if (run > max) max = run; } else run = 0;
  }
  const fence = '`'.repeat(Math.max(3, max + 1));
  return fence + sanitizeLang(lang) + '\n' + (text ? text + '\n' : '') + fence;
}

// =============================================================================================
// Markdown → editing DOM

class EditorRenderer {
  constructor(doc, opts) {
    this.doc = doc;
    this.opts = opts || {};
  }

  el(tag) { return this.doc.createElement(tag); }
  br() { return this.doc.createElement('br'); }

  blocks(nodes, parent) { for (const n of nodes) this.block(n, parent); }

  block(n, parent) {
    const doc = this.doc;
    switch (n.type) {
      case 'paragraph': {
        const p = this.el('p');
        this.inlines(n.children, p);
        if (!hasInlineContent(p)) { while (p.firstChild) p.removeChild(p.firstChild); p.appendChild(this.br()); }
        parent.appendChild(p);
        break;
      }
      case 'heading': {
        const h = this.el('h' + n.depth);
        this.inlines(n.children, h);
        if (!hasInlineContent(h)) h.appendChild(this.br());
        parent.appendChild(h);
        break;
      }
      case 'thematicBreak':
        parent.appendChild(this.el('hr'));
        break;
      case 'blockquote': {
        const q = this.el('blockquote');
        this.blocks(n.children, q);
        if (!q.firstChild) { const p = this.el('p'); p.appendChild(this.br()); q.appendChild(p); }
        parent.appendChild(q);
        break;
      }
      case 'list': {
        const l = this.el(n.ordered ? 'ol' : 'ul');
        if (n.ordered && n.start !== 1) l.setAttribute('start', String(n.start));
        for (const item of n.children) {
          const li = this.el('li');
          if (item.checked !== null) li.setAttribute('data-checked', String(item.checked));
          let rest = item.children;
          if (rest[0] && rest[0].type === 'paragraph') {
            this.inlines(rest[0].children, li);
            rest = rest.slice(1);
          }
          if (!hasInlineContent(li)) li.appendChild(this.br());
          this.blocks(rest, li);
          l.appendChild(li);
        }
        parent.appendChild(l);
        break;
      }
      case 'code':
        parent.appendChild(makeCodeBlock(doc, n.value, n.lang || ''));
        break;
      case 'table': {
        const table = this.el('table');
        const [head, ...rows] = n.children;
        const thead = this.el('thead');
        thead.appendChild(this.row(head, 'th', n.align));
        table.appendChild(thead);
        const tbody = this.el('tbody');
        for (const r of rows) tbody.appendChild(this.row(r, 'td', n.align));
        table.appendChild(tbody);
        parent.appendChild(table);
        break;
      }
      default:
        break;
    }
  }

  row(r, tag, align) {
    const tr = this.el('tr');
    r.children.forEach((cell, i) => {
      const c = this.el(tag);
      if (align[i]) c.setAttribute('data-align', align[i]);
      this.inlines(cell.children, c);
      if (!hasInlineContent(c)) c.appendChild(this.br());
      tr.appendChild(c);
    });
    return tr;
  }

  inlines(nodes, parent) {
    const doc = this.doc;
    for (const n of nodes) {
      switch (n.type) {
        case 'text': parent.appendChild(doc.createTextNode(n.value)); break;
        case 'softbreak': case 'break': parent.appendChild(this.br()); break;
        case 'emphasis': case 'strong': case 'delete': {
          const e = this.el(n.type === 'emphasis' ? 'em' : n.type === 'strong' ? 'strong' : 's');
          this.inlines(n.children, e);
          parent.appendChild(e);
          break;
        }
        case 'inlineCode': {
          const c = this.el('code');
          c.appendChild(doc.createTextNode(n.value));
          parent.appendChild(c);
          break;
        }
        case 'link': {
          const href = safeLinkHref(n.url);
          if (href === null) { this.inlines(n.children, parent); break; }
          const a = this.el('a');
          a.setAttribute('href', href);
          if (n.title) a.setAttribute('title', n.title);
          if (n.autolink) a.setAttribute('data-md-autolink', n.autolink === 'angle' ? 'angle' : n.autolink);
          this.inlines(n.children, a);
          parent.appendChild(a);
          break;
        }
        case 'image': {
          const src = safeImageSrc(n.url);
          if (src === null) { if (n.alt) parent.appendChild(doc.createTextNode(n.alt)); break; }
          parent.appendChild(makeImage(doc, src, n.alt, n.title, this.opts.resolveImage));
          break;
        }
        default: break;
      }
    }
  }
}

function makeImage(doc, src, alt, title, resolveImage) {
  const img = doc.createElement('img');
  let shown = src;
  if (typeof resolveImage === 'function') {
    const r = resolveImage(src);
    if (typeof r === 'string' && safeImageSrc(r) !== null) shown = r;
  }
  img.setAttribute('src', shown);
  img.setAttribute('data-md-src', src);
  img.setAttribute('alt', alt || '');
  if (title) img.setAttribute('title', title);
  return img;
}

function makeCodeBlock(doc, value, lang) {
  const pre = doc.createElement('pre');
  pre.setAttribute('class', 're-code');
  pre.setAttribute('spellcheck', 'false');
  const l = sanitizeLang(lang);
  if (l) pre.setAttribute('data-lang', l);
  const code = doc.createElement('code');
  fillCode(doc, code, value, l);
  pre.appendChild(code);
  return pre;
}

function fillCode(doc, code, value, lang) {
  while (code.firstChild) code.removeChild(code.firstChild);
  code.appendChild(highlight(value, lang, doc));
  code.appendChild(doc.createTextNode('\n')); // sentinel: lets the caret sit on a trailing empty line
}

function hasInlineContent(el) {
  for (const c of kids(el)) {
    if (c.nodeType === TEXT_NODE) {
      const v = (c.nodeValue || '').split(ZWSP).join('').split(NBSP).join(' ');
      if (v.trim()) return true;
      continue;
    }
    if (c.nodeType !== ELEMENT_NODE) continue;
    const t = tagOf(c);
    if (t === 'IMG') return true;
    if (t === 'BR' || BLOCK_TAGS.has(t)) continue;
    if (hasInlineContent(c)) return true;
  }
  return false;
}

/**
 * Renders Markdown into the canonical editing DOM.
 * @param {string|object} mdOrAst
 * @param {Document} [doc]
 * @param {{resolveImage?: (src:string)=>string|null, trailingParagraph?: boolean}} [opts]
 * @returns {DocumentFragment}
 */
export function markdownToEditorDOM(mdOrAst, doc = globalThis.document, opts = {}) {
  const ast = typeof mdOrAst === 'string' || mdOrAst == null ? parse(mdOrAst ?? '') : mdOrAst;
  const frag = doc.createDocumentFragment();
  new EditorRenderer(doc, opts).blocks(ast.children, frag);
  if (opts.trailingParagraph !== false) {
    const last = frag.lastChild;
    if (!last || tagOf(last) !== 'P') {
      const p = doc.createElement('p');
      p.appendChild(doc.createElement('br'));
      frag.appendChild(p);
    }
  }
  return frag;
}

// =============================================================================================
// Text positions (caret save/restore across DOM rebuilds)

const POS_BLOCKS = new Set(['P', 'DIV', 'H1', 'H2', 'H3', 'H4', 'H5', 'H6', 'LI', 'PRE', 'TD', 'TH', 'BLOCKQUOTE', 'UL', 'OL', 'TABLE', 'TR']);
const ATOMS = new Set(['BR', 'IMG', 'HR']);

/**
 * Walks leaves in document order. Text counts its length, BR/IMG/HR count 1 and entering any
 * block after the first counts 1, so equivalent DOMs map caret positions identically.
 */
function walkPositions(roots, visit) {
  let pos = 0, first = true;
  const rec = (n) => {
    for (let c = n.firstChild; c; c = c.nextSibling) {
      if (c.nodeType === TEXT_NODE) {
        if (visit(c, pos, 'text')) return true;
        pos += c.nodeValue.length;
      } else if (c.nodeType === ELEMENT_NODE) {
        const t = c.nodeName;
        if (POS_BLOCKS.has(t)) { if (!first) pos += 1; first = false; }
        if (ATOMS.has(t)) {
          if (visit(c, pos, 'atom')) return true;
          pos += 1;
          continue;
        }
        if (visit(c, pos, 'enter')) return true;
        if (rec(c)) return true;
        if (visit(c, pos, 'leave')) return true;
      }
    }
    return false;
  };
  for (const r of roots) {
    if (r.nodeType === TEXT_NODE) {
      if (visit(r, pos, 'text')) return pos;
      pos += r.nodeValue.length;
      continue;
    }
    if (r.nodeType !== ELEMENT_NODE) continue;
    if (POS_BLOCKS.has(r.nodeName)) { if (!first) pos += 1; first = false; }
    if (ATOMS.has(r.nodeName)) { if (visit(r, pos, 'atom')) return pos; pos += 1; continue; }
    if (visit(r, pos, 'enter')) return pos;
    if (rec(r)) return pos;
    if (visit(r, pos, 'leave')) return pos;
  }
  return pos;
}

/** Text position of a DOM boundary point (container, offset) within `roots`; -1 if outside. */
function positionOf(roots, container, offset) {
  let result = -1;
  if (container.nodeType === TEXT_NODE) {
    walkPositions(roots, (n, pos) => {
      if (n === container) { result = pos + Math.min(offset, n.nodeValue.length); return true; }
      return false;
    });
    return result;
  }
  const target = container.childNodes[offset] || null;
  if (target) {
    // Boundary before a child: the position where that child starts.
    walkPositions(roots, (n, pos, kind) => {
      if (n === target && kind !== 'leave') { result = pos; return true; }
      return false;
    });
  } else {
    // Boundary at the end of an element: where the element is left.
    walkPositions(roots, (n, pos, kind) => {
      if (n === container && kind === 'leave') { result = pos; return true; }
      return false;
    });
  }
  if (result === -1 && !target && roots.length && roots[0].parentNode === container) result = walkPositions(roots, () => false);
  return result;
}

/**
 * Inverse of positionOf: the boundary point at text position `pos`. At a boundary between two
 * text nodes the earlier one is chosen, or the later one with `preferNext` (selection starts).
 */
function pointAt(roots, pos, preferNext = false) {
  let found = null, fallback = null;
  walkPositions(roots, (n, start, kind) => {
    if (kind === 'text') {
      const len = n.nodeValue.length;
      if (pos < start + len || (pos === start + len && !preferNext)) { found = { node: n, offset: Math.max(0, pos - start) }; return true; }
      fallback = { node: n, offset: len };
    } else if (kind === 'atom') {
      if (pos <= start) { found = { node: n.parentNode, offset: indexOf(n) }; return true; }
      fallback = { node: n.parentNode, offset: indexOf(n) + 1 };
    }
    return false;
  });
  return found || fallback;
}

function indexOf(n) {
  let i = 0;
  for (let c = n.parentNode.firstChild; c && c !== n; c = c.nextSibling) i++;
  return i;
}

// =============================================================================================
// Inline marks as segments (used to toggle bold/italic/strike without execCommand quirks)

/** Flattens editing-DOM inline content into segments; positions match walkPositions. */
function editSegments(nodes, marks, out) {
  for (const n of nodes) {
    if (n.nodeType === TEXT_NODE) { if (n.nodeValue) out.push({ k: 'text', t: n.nodeValue, m: marks }); continue; }
    if (n.nodeType !== ELEMENT_NODE) continue;
    const tag = n.nodeName;
    if (tag === 'BR') { out.push({ k: 'br', m: marks }); continue; }
    if (tag === 'IMG') { out.push({ k: 'img', el: n, m: marks }); continue; }
    if (tag === 'CODE') { const t = n.textContent; if (t) out.push({ k: 'code', t, m: marks }); continue; }
    let m = marks;
    if (tag === 'STRONG' || tag === 'B') m = withMark(m, 'strong');
    else if (tag === 'EM' || tag === 'I') m = withMark(m, 'em');
    else if (tag === 'S' || tag === 'DEL' || tag === 'STRIKE') m = withMark(m, 'del');
    else if (tag === 'A') m = withMark(m, 'link', { href: n.getAttribute('href') || '', title: n.getAttribute('title') || '', auto: n.getAttribute('data-md-autolink') || '' });
    editSegments(n.childNodes, m, out);
  }
  return out;
}

const segLength = (s) => (s.k === 'text' || s.k === 'code' ? s.t.length : 1);

/** Splits segments so that `pos` falls on a segment boundary. */
function splitSegmentsAt(segs, pos) {
  let acc = 0;
  for (let i = 0; i < segs.length; i++) {
    const s = segs[i], len = segLength(s);
    if (pos > acc && pos < acc + len && (s.k === 'text' || s.k === 'code')) {
      const k = pos - acc;
      segs.splice(i, 1, { ...s, t: s.t.slice(0, k) }, { ...s, t: s.t.slice(k) });
      return;
    }
    acc += len;
    if (acc >= pos) return;
  }
}

/** Rebuilds canonical inline DOM (a > strong > em > s nesting) from segments. */
function buildInline(doc, segs) {
  const frag = doc.createDocumentFragment();
  const stack = [];
  for (const s of segs) {
    const want = MARK_ORDER.filter((k) => markOn(s.m, k));
    let keep = 0;
    while (keep < stack.length && keep < want.length && stack[keep].type === want[keep]
      && (want[keep] !== 'link' || sameLink(stack[keep].link, s.m.link))) keep++;
    stack.length = keep;
    for (let i = keep; i < want.length; i++) {
      const k = want[i];
      const el = doc.createElement(k === 'link' ? 'a' : k === 'strong' ? 'strong' : k === 'em' ? 'em' : 's');
      if (k === 'link') {
        el.setAttribute('href', s.m.link.href);
        if (s.m.link.title) el.setAttribute('title', s.m.link.title);
        if (s.m.link.auto) el.setAttribute('data-md-autolink', s.m.link.auto);
      }
      (stack.length ? stack[stack.length - 1].el : frag).appendChild(el);
      stack.push({ type: k, link: s.m.link, el });
    }
    const parent = stack.length ? stack[stack.length - 1].el : frag;
    if (s.k === 'text') parent.appendChild(doc.createTextNode(s.t));
    else if (s.k === 'code') { const c = doc.createElement('code'); c.appendChild(doc.createTextNode(s.t)); parent.appendChild(c); }
    else if (s.k === 'br') parent.appendChild(doc.createElement('br'));
    else if (s.k === 'img') parent.appendChild(s.el);
  }
  return frag;
}

// =============================================================================================
// The editor

const SVG_NS = 'http://www.w3.org/2000/svg';
const ICONS = {
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
  undo: 'M9 14l-5-5 5-5M4 9h10.5a5.5 5.5 0 0 1 0 11H11',
  redo: 'M15 14l5-5-5-5M20 9H9.5a5.5 5.5 0 0 0 0 11H13',
  copy: 'M9 9h11v11H9zM5 15V4h11',
  open: 'M14 4h6v6M20 4l-9 9M18 14v5H5V6h5',
  edit: 'M4 20h4L19 9l-4-4L4 16v4zM14 6l4 4',
  unlink: 'M9 15l6-6M8.5 11.5l-2 2a3.5 3.5 0 0 0 5 5l2-2M15.5 12.5l2-2a3.5 3.5 0 0 0-5-5l-2 2',
  check: 'M5 12.5l4.5 4.5L19 7.5',
  close: 'M6 6l12 12M18 6L6 18',
  rowAdd: 'M4 4h16v8H4zM4 8h16M12 15v6M9 18h6',
  colAdd: 'M4 4h8v16H4zM8 4v16M15 12h6M18 9v6',
  rowDel: 'M4 4h16v8H4zM4 8h16M9 18h6',
  colDel: 'M4 4h8v16H4zM8 4v16M15 12h6',
  align: 'M4 6h16M7 10h10M4 14h16M7 18h10',
  trash: 'M4 7h16M10 11v6M14 11v6M6 7l1 13h10l1-13M9 7V4h6v3',
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
    else if (k.startsWith('on') && typeof v === 'function') e.addEventListener(k.slice(2), v);
    else e.setAttribute(k, v === true ? '' : String(v));
  }
  for (const c of children.flat()) {
    if (c === null || c === undefined || c === false) continue;
    e.appendChild(typeof c === 'string' ? doc.createTextNode(c) : c);
  }
  return e;
}

const TEXT_BLOCKS = new Set(['P', 'H1', 'H2', 'H3', 'H4', 'H5', 'H6', 'LI', 'PRE', 'TD', 'TH', 'DIV']);
const TOP_BLOCKS = new Set(['P', 'H1', 'H2', 'H3', 'H4', 'H5', 'H6', 'BLOCKQUOTE', 'UL', 'OL', 'PRE', 'TABLE', 'HR']);
const INLINE_OK = new Set(['STRONG', 'B', 'EM', 'I', 'S', 'DEL', 'STRIKE', 'CODE', 'A', 'IMG', 'BR']);
const LI_BLOCKS = new Set(['UL', 'OL', 'P', 'PRE', 'BLOCKQUOTE', 'TABLE', 'HR', 'H1', 'H2', 'H3', 'H4', 'H5', 'H6']);
const QUOTE_BLOCKS = new Set(['P', 'H1', 'H2', 'H3', 'H4', 'H5', 'H6', 'BLOCKQUOTE', 'UL', 'OL', 'PRE', 'TABLE', 'HR']);
const ALLOWED_ATTRS = {
  OL: ['start'], LI: ['data-checked'], PRE: ['class', 'data-lang', 'spellcheck'], TH: ['data-align'], TD: ['data-align'],
  A: ['href', 'title', 'data-md-autolink'], IMG: ['src', 'alt', 'title', 'data-md-src', 'data-uploading', 'class'],
};

const isMac = () => /Mac|iPhone|iPad/.test((globalThis.navigator && (globalThis.navigator.platform || globalThis.navigator.userAgent)) || '');

class History {
  constructor() { this.stack = []; this.index = -1; }
  reset(state) { this.stack = [state]; this.index = 0; }
  push(state) {
    const cur = this.stack[this.index];
    if (cur && cur.md === state.md) { cur.sel = state.sel; return; }
    this.stack.length = this.index + 1;
    this.stack.push(state);
    this.index++;
    // Bound memory: at most 200 states and ~32M chars of Markdown.
    let total = this.stack.reduce((n, s) => n + s.md.length, 0);
    while (this.stack.length > 2 && (this.stack.length > 200 || total > 32e6)) {
      total -= this.stack.shift().md.length;
      this.index--;
    }
  }
  canUndo() { return this.index > 0; }
  canRedo() { return this.index < this.stack.length - 1; }
  undo() { return this.canUndo() ? this.stack[--this.index] : null; }
  redo() { return this.canRedo() ? this.stack[++this.index] : null; }
}

/**
 * The editor command for a Ctrl/Cmd (+Shift) shortcut, or null when the editor does not handle it
 * (the key then reaches the page, e.g. the app's Ctrl/Cmd+K command palette). Code blocks hold
 * plain text, so Ctrl/Cmd+K (link) is not taken there.
 * @param {string} key lower-cased KeyboardEvent.key for printable keys
 * @param {string} code KeyboardEvent.code
 * @param {{shift?: boolean, inCode?: boolean}} [o]
 */
export function shortcutCommand(key, code, { shift = false, inCode = false } = {}) {
  if (!shift) {
    const cmd = { b: 'bold', i: 'italic', e: 'code', k: 'link', z: 'undo', y: 'redo', u: 'noop' }[key] || null;
    return cmd === 'link' && inCode ? null : cmd;
  }
  return { z: 'redo', x: 'strike', c: 'codeBlock' }[key]
    || { Digit7: 'orderedList', Digit8: 'bulletList', Digit9: 'taskList', Period: 'quote' }[code] || null;
}

/**
 * Position (relative to the scroller's content box) of a floating tool element anchored to a block.
 *  - 'header': right-aligned in the block's top label strip of height `opts.strip` (a code block's
 *    top padding). When the tools are taller than the strip they are lifted over the block's top
 *    edge instead of covering the block's first line of content.
 *  - 'beside': right of the block when it fits (covers nothing), else above its right edge.
 *  - anything else ('below'): under the anchor, left-aligned.
 * The result is clamped horizontally into the scroller.
 * @param {string} where
 * @param {{top:number,right:number,bottom:number,left:number}} rect anchor (viewport coordinates)
 * @param {{top:number,right:number,left:number}} host scroller box (viewport coordinates)
 * @param {{top:number,left:number,width:number}} scroll scroller scrollTop/scrollLeft/clientWidth
 * @param {{width:number,height:number}} size the tool element
 * @param {{strip?:number}} [opts]
 * @returns {{top:number,left:number}}
 */
export function popupPosition(where, rect, host, scroll, size, opts = {}) {
  const st = scroll.top, sl = scroll.left;
  const w = size.width, hgt = size.height;
  let top, left;
  if (where === 'header') {
    const strip = Math.max(0, opts.strip || 0);
    const inset = Math.min(6, (strip - hgt) / 2);
    // Fits in the strip: centred-ish inside it; taller: bottom edge 2px above the content.
    top = rect.top - host.top + st + (hgt + 4 <= strip ? Math.max(2, inset) : strip - hgt - 2);
    left = rect.right - host.left + sl - w - 6;
  } else if (where === 'beside') {
    if (host.right - rect.right >= w + 16) { top = rect.top - host.top + st; left = rect.right - host.left + sl + 8; }
    else { top = rect.top - host.top + st - hgt - 6; left = rect.right - host.left + sl - w; if (top < st) top = rect.bottom - host.top + st + 6; }
  } else {
    top = rect.bottom - host.top + st + 6; left = rect.left - host.left + sl;
  }
  left = Math.max(sl + 4, Math.min(left, sl + scroll.width - w - 4));
  return { top, left };
}

/**
 * Creates the rich (WYSIWYG) Markdown editor.
 * @param {{container: HTMLElement, markdown?: string, onChange?: (md:string)=>void,
 *          onUploadImage?: (file: File)=>Promise<string>, placeholder?: string,
 *          resolveImage?: (src:string)=>string|null, readOnly?: boolean, toolbar?: boolean,
 *          debounceMs?: number, autofocus?: boolean}} opts
 * @returns {{getMarkdown():string, setMarkdown(md:string):void, focus():void, destroy():void,
 *           exec(command:string, arg?:any):void, on(event:string, fn:Function):()=>void,
 *           flush():void, setReadOnly(ro:boolean):void, isEmpty():boolean, element:HTMLElement}}
 *   events: "change" (md), "state" (selection formatting), "focus", "blur", "error" (Error)
 */
export function createRichEditor(opts) {
  return new RichEditor(opts).api;
}

class RichEditor {
  constructor(o) {
    if (!o || !o.container) throw new TypeError('createRichEditor: container is required');
    this.o = o;
    this.container = o.container;
    this.doc = this.container.ownerDocument;
    this.listeners = new Map();
    this.cleanups = [];
    this.history = new History();
    this.debounceMs = o.debounceMs ?? 300;
    this.readOnly = !!o.readOnly;
    this.dirty = false;
    this.timer = null;
    this.hlTimers = new Map();
    this.composing = false;
    this.plainPaste = false;
    this.lastMd = '';
    this.uploadSeq = 0;
    this.blockCache = new WeakMap();
    this.buildUI();
    const MO = this.doc.defaultView && this.doc.defaultView.MutationObserver;
    if (MO) {
      this.observer = new MO((recs) => this.invalidate(recs));
      this.observer.observe(this.surface, { subtree: true, childList: true, characterData: true, attributes: true });
    }
    this.render(o.markdown || '');
    this.lastMd = this.getMarkdown();
    this.history.reset({ md: this.lastMd, sel: null });
    this.bindEvents();
    try { this.doc.execCommand('styleWithCSS', false, false); } catch { /* not supported: fine */ }
    if (o.autofocus) this.focus();
    this.api = {
      getMarkdown: () => this.getMarkdown(),
      setMarkdown: (md) => this.setMarkdown(md),
      focus: () => this.focus(),
      destroy: () => this.destroy(),
      exec: (cmd, arg) => this.exec(cmd, arg),
      on: (ev, fn) => this.on(ev, fn),
      flush: () => this.commit(),
      setReadOnly: (ro) => this.setReadOnly(ro),
      isEmpty: () => this.isEmpty(),
      getState: () => this.computeState(),
      element: this.surface,
    };
  }

  // ------------------------------------------------------------------ UI

  buildUI() {
    const d = this.doc;
    this.root = h(d, 'div', { class: 're-editor' });
    if (this.o.toolbar !== false) this.root.appendChild(this.buildToolbar());
    this.scroller = h(d, 'div', { class: 're-scroll' });
    this.surface = h(d, 'div', {
      class: 're-surface md',
      contenteditable: this.readOnly ? 'false' : 'true',
      role: 'textbox',
      'aria-multiline': 'true',
      'aria-label': this.o.ariaLabel || 'Note',
      spellcheck: 'true',
      'data-placeholder': this.o.placeholder ?? 'Start writing' + String.fromCharCode(0x2026),
    });
    this.scroller.appendChild(this.surface);
    this.buildOverlays();
    this.root.appendChild(this.scroller);
    this.container.appendChild(this.root);
    if (this.readOnly) this.root.classList.add('is-readonly');
  }

  buildToolbar() {
    const d = this.doc;
    const btn = (cmd, label, iconOrGlyph, shortcut) => {
      const b = h(d, 'button', {
        type: 'button', class: 're-btn', 'data-cmd': cmd, 'aria-label': label,
        title: shortcut ? `${label} (${shortcut})` : label, 'aria-pressed': 'false', tabindex: '-1',
      });
      if (ICONS[iconOrGlyph]) b.appendChild(icon(d, iconOrGlyph));
      else b.appendChild(h(d, 'span', { class: 're-glyph re-glyph-' + cmd, 'aria-hidden': 'true' }, iconOrGlyph));
      return b;
    };
    const mod = isMac() ? String.fromCharCode(0x2318) : 'Ctrl+';
    const sep = () => h(d, 'span', { class: 're-sep', 'aria-hidden': 'true' });
    this.styleSelect = h(d, 'select', { class: 're-style', 'aria-label': 'Text style', title: 'Text style', tabindex: '-1' },
      h(d, 'option', { value: 'h1' }, 'Title'),
      h(d, 'option', { value: 'h2' }, 'Heading'),
      h(d, 'option', { value: 'h3' }, 'Subheading'),
      h(d, 'option', { value: 'h4' }, 'Small heading'),
      h(d, 'option', { value: 'p' }, 'Body'),
      h(d, 'option', { value: 'code' }, 'Monospaced'));
    this.styleSelect.addEventListener('change', () => {
      const v = this.styleSelect.value;
      this.restoreSavedRange();
      if (v === 'code') this.exec('codeBlock');
      else if (v === 'p') this.exec('paragraph');
      else this.exec('heading', Number(v.slice(1)));
    });
    this.styleSelect.addEventListener('mousedown', () => this.saveRange());
    this.styleSelect.addEventListener('focus', () => this.saveRange());
    const bar = h(d, 'div', { class: 're-toolbar', role: 'toolbar', 'aria-label': 'Formatting' },
      this.styleSelect, sep(),
      btn('bold', 'Bold', 'B', mod + 'B'), btn('italic', 'Italic', 'I', mod + 'I'), btn('strike', 'Strikethrough', 'S', mod + 'Shift+X'),
      btn('code', 'Inline code', 'code', mod + 'E'), btn('link', 'Link', 'link', mod + 'K'), sep(),
      btn('bulletList', 'Bulleted list', 'ul', mod + 'Shift+8'), btn('orderedList', 'Numbered list', 'ol', mod + 'Shift+7'),
      btn('taskList', 'Checklist', 'task', mod + 'Shift+9'), btn('quote', 'Quote', 'quote', mod + 'Shift+.'), sep(),
      btn('codeBlock', 'Code block', 'codeblock', mod + 'Shift+C'), btn('table', 'Table', 'table'),
      btn('image', 'Image', 'image'), btn('hr', 'Divider', 'hr'),
      h(d, 'span', { class: 're-spacer' }),
      btn('undo', 'Undo', 'undo', mod + 'Z'), btn('redo', 'Redo', 'redo', isMac() ? mod + 'Shift+Z' : mod + 'Y'));
    // Keep the editor selection when clicking toolbar buttons.
    bar.addEventListener('mousedown', (e) => { if (e.target.closest('button')) e.preventDefault(); });
    bar.addEventListener('click', (e) => {
      const b = e.target.closest('button[data-cmd]');
      if (!b || this.readOnly) return;
      const cmd = b.getAttribute('data-cmd');
      if (cmd === 'image') { this.pickImage(); return; }
      if (cmd === 'link') { this.openLinkEditor(); return; }
      this.exec(cmd);
    });
    this.toolbar = bar;
    return bar;
  }

  buildOverlays() {
    const d = this.doc;
    const tbtn = (act, label, ic) => h(d, 'button', { type: 'button', class: 're-btn re-btn-sm', 'data-act': act, 'aria-label': label, title: label }, icon(d, ic));

    // Code block tools: language picker + copy.
    this.langSelect = h(d, 'select', { class: 're-lang', 'aria-label': 'Code language' }, h(d, 'option', { value: '' }, 'Plain text'),
      ...listLanguages().map((l) => h(d, 'option', { value: l.id }, l.label)));
    this.codeTools = h(d, 'div', { class: 're-pop re-code-tools', 'data-re-ui': '', hidden: true },
      this.langSelect, tbtn('copy', 'Copy code', 'copy'));
    this.langSelect.addEventListener('change', () => {
      const pre = this.activePre;
      if (!pre) return;
      this.mutate(() => {
        const v = sanitizeLang(this.langSelect.value);
        if (v) pre.setAttribute('data-lang', v); else pre.removeAttribute('data-lang');
        this.rehighlight(pre);
      });
    });
    this.codeTools.addEventListener('click', (e) => {
      const b = e.target.closest('button[data-act="copy"]');
      if (b && this.activePre) this.copyCode(this.activePre, b);
    });

    // Table tools.
    this.tableTools = h(d, 'div', { class: 're-pop re-table-tools', 'data-re-ui': '', hidden: true },
      tbtn('rowAdd', 'Add row below', 'rowAdd'), tbtn('colAdd', 'Add column right', 'colAdd'),
      tbtn('rowDel', 'Delete row', 'rowDel'), tbtn('colDel', 'Delete column', 'colDel'),
      tbtn('align', 'Cycle column alignment', 'align'), tbtn('tableDel', 'Delete table', 'trash'));
    this.tableTools.addEventListener('mousedown', (e) => e.preventDefault());
    this.tableTools.addEventListener('click', (e) => {
      const b = e.target.closest('button[data-act]');
      if (b) this.tableAction(b.getAttribute('data-act'));
    });

    // Link bubble (view + edit).
    this.linkInput = h(d, 'input', { type: 'url', class: 're-link-input', placeholder: 'https://example.com', 'aria-label': 'Link URL', spellcheck: 'false', autocomplete: 'off' });
    this.linkView = h(d, 'a', { class: 're-link-url', target: '_blank', rel: 'noopener noreferrer' });
    this.linkBubble = h(d, 'div', { class: 're-pop re-link-bubble', 'data-re-ui': '', hidden: true },
      h(d, 'div', { class: 're-link-view' }, this.linkView, tbtn('linkEdit', 'Edit link', 'edit'), tbtn('linkRemove', 'Remove link', 'unlink')),
      h(d, 'form', { class: 're-link-form', novalidate: true }, this.linkInput, tbtn('linkApply', 'Apply link', 'check'), tbtn('linkCancel', 'Cancel', 'close')));
    this.linkBubble.querySelector('form').addEventListener('submit', (e) => { e.preventDefault(); this.applyLinkFromInput(); });
    this.linkBubble.addEventListener('mousedown', (e) => { if (e.target.closest('button')) e.preventDefault(); });
    this.linkBubble.addEventListener('click', (e) => {
      const b = e.target.closest('button[data-act]');
      if (!b) return;
      const act = b.getAttribute('data-act');
      if (act === 'linkApply') this.applyLinkFromInput();
      else if (act === 'linkCancel') { this.closeLinkEditor(); this.restoreSavedRange(); this.surface.focus({ preventScroll: true }); }
      else if (act === 'linkEdit') this.openLinkEditor();
      else if (act === 'linkRemove') this.removeLink();
    });
    this.linkInput.addEventListener('keydown', (e) => {
      if (e.key === 'Escape') { e.preventDefault(); this.closeLinkEditor(); this.restoreSavedRange(); this.surface.focus({ preventScroll: true }); }
      // Bare domains ("example.com") are valid input here, so do not rely on native form validation.
      if (e.key === 'Enter' && !e.isComposing) { e.preventDefault(); this.applyLinkFromInput(); }
    });
    this.linkView.addEventListener('click', (e) => {
      e.preventDefault();
      const href = this.linkView.getAttribute('href');
      if (href && safeLinkHref(href) !== null) this.doc.defaultView.open(href, '_blank', 'noopener,noreferrer');
    });

    this.fileInput = h(d, 'input', { type: 'file', accept: 'image/*', multiple: true, class: 're-file', hidden: true, tabindex: '-1', 'aria-hidden': 'true' });
    this.fileInput.addEventListener('change', () => {
      const files = Array.from(this.fileInput.files || []);
      this.fileInput.value = '';
      this.restoreSavedRange();
      this.insertImageFiles(files);
    });

    this.scroller.append(this.codeTools, this.tableTools, this.linkBubble, this.fileInput);
  }

  // ------------------------------------------------------------------ events

  listen(target, type, fn, opts) {
    target.addEventListener(type, fn, opts);
    this.cleanups.push(() => target.removeEventListener(type, fn, opts));
  }

  bindEvents() {
    const s = this.surface;
    this.listen(s, 'keydown', (e) => this.onKeyDown(e));
    this.listen(s, 'beforeinput', (e) => this.onBeforeInput(e));
    this.listen(s, 'input', (e) => this.onInput(e));
    this.listen(s, 'compositionstart', () => { this.composing = true; });
    this.listen(s, 'compositionend', () => { this.composing = false; this.afterInput('insertCompositionText', null); });
    this.listen(s, 'paste', (e) => this.onPaste(e));
    this.listen(s, 'drop', (e) => this.onDrop(e));
    this.listen(s, 'dragover', (e) => { if (!this.readOnly) e.preventDefault(); });
    this.listen(s, 'mousedown', (e) => this.onMouseDown(e));
    this.listen(s, 'click', (e) => this.onClick(e));
    this.listen(s, 'focus', () => { this.root.classList.add('is-focused'); this.emit('focus'); });
    this.listen(s, 'blur', () => { this.root.classList.remove('is-focused'); this.commit(); this.emit('blur'); });
    this.listen(this.doc, 'selectionchange', () => this.scheduleState());
    this.listen(this.scroller, 'scroll', () => this.positionOverlays(), { passive: true });
    const win = this.doc.defaultView;
    if (win) this.listen(win, 'resize', () => this.positionOverlays(), { passive: true });
  }

  on(ev, fn) {
    if (!this.listeners.has(ev)) this.listeners.set(ev, new Set());
    this.listeners.get(ev).add(fn);
    return () => this.listeners.get(ev).delete(fn);
  }

  emit(ev, arg) {
    const set = this.listeners.get(ev);
    if (!set) return;
    for (const fn of set) {
      try { fn(arg); } catch (err) { if (ev !== 'error') this.emit('error', err); }
    }
  }

  // ------------------------------------------------------------------ model

  render(md) {
    const frag = markdownToEditorDOM(md, this.doc, { resolveImage: this.o.resolveImage });
    this.surface.replaceChildren(frag);
    this.ensureNonEmpty();
    this.updatePlaceholder();
  }

  /** Drops cached Markdown of every top-level block touched by the given mutations. */
  invalidate(records) {
    for (const rec of records) {
      if (rec.target === this.surface) continue; // root child list: new/moved blocks are uncached anyway
      const top = this.topBlockOf(rec.target);
      if (top) this.blockCache.delete(top);
    }
  }

  /**
   * Serializes incrementally: each top-level block's Markdown is cached until a mutation inside
   * it is observed, so a pause in typing re-serializes one block, not the whole note.
   */
  getMarkdown() {
    if (this.observer) this.invalidate(this.observer.takeRecords());
    const ctx = { pasted: false, styles: false };
    const blocks = [];
    let run = [];
    const flushRun = () => {
      if (!run.length) return;
      for (const b of serializeChildren(run, ctx)) blocks.push(b);
      run = [];
    };
    for (const c of this.surface.childNodes) {
      if (c.nodeType !== ELEMENT_NODE || !TOP_BLOCKS.has(c.nodeName)) { run.push(c); continue; }
      flushRun();
      let cached = this.blockCache.get(c);
      if (!cached) {
        cached = serializeChildren([c], ctx);
        this.blockCache.set(c, cached);
      }
      for (const b of cached) {
        const prev = blocks[blocks.length - 1];
        // Adjacent lists of one kind need the alternate marker; that depends on the neighbour.
        if (b.kind === 'list' && cached.length === 1 && (c.nodeName === 'UL' || c.nodeName === 'OL')
          && prev && prev.kind === 'list' && prev.ordered === b.ordered && prev.delim === b.delim) {
          blocks.push(listBlock(c, c.nodeName, ctx, prev));
        } else {
          blocks.push(b);
        }
      }
    }
    flushRun();
    return joinBlocks(blocks);
  }

  setMarkdown(md) {
    clearTimeout(this.timer);
    this.timer = null;
    this.dirty = false;
    this.render(String(md ?? ''));
    this.lastMd = this.getMarkdown();
    this.history.reset({ md: this.lastMd, sel: null });
    this.updateState();
  }

  isEmpty() {
    const s = this.surface;
    if (s.childNodes.length > 1) return false;
    const f = s.firstChild;
    return !f || (f.nodeName === 'P' && !hasInlineContent(f));
  }

  ensureNonEmpty() {
    if (!this.surface.firstChild) {
      const p = this.doc.createElement('p');
      p.appendChild(this.doc.createElement('br'));
      this.surface.appendChild(p);
    }
  }

  updatePlaceholder() {
    this.surface.classList.toggle('is-empty', this.isEmpty());
  }

  markDirty() {
    this.dirty = true;
    clearTimeout(this.timer);
    this.timer = setTimeout(() => this.commit(), this.debounceMs);
  }

  /** Serializes now: records history and fires onChange if the Markdown changed. */
  commit() {
    clearTimeout(this.timer);
    this.timer = null;
    if (!this.dirty) return;
    this.dirty = false;
    const md = this.getMarkdown();
    this.history.push({ md, sel: this.saveSelection() });
    if (md !== this.lastMd) {
      this.lastMd = md;
      this.emit('change', md);
      if (typeof this.o.onChange === 'function') {
        try { this.o.onChange(md); } catch (err) { this.emit('error', err); }
      }
    }
  }

  /** Runs a structural change as one undoable step. */
  mutate(fn) {
    if (this.readOnly) return;
    this.commit();
    const r = fn();
    this.normalize();
    this.ensureNonEmpty();
    this.updatePlaceholder();
    this.dirty = true;
    this.commit();
    this.updateState();
    return r;
  }

  undo() {
    this.commit();
    const s = this.history.undo();
    if (s) this.restoreSnapshot(s);
  }

  redo() {
    this.commit();
    const s = this.history.redo();
    if (s) this.restoreSnapshot(s);
  }

  restoreSnapshot(s) {
    this.render(s.md);
    if (s.sel) this.restoreSelection(s.sel);
    if (s.md !== this.lastMd) {
      this.lastMd = s.md;
      this.emit('change', s.md);
      if (typeof this.o.onChange === 'function') {
        try { this.o.onChange(s.md); } catch (err) { this.emit('error', err); }
      }
    }
    this.updateState();
  }

  // ------------------------------------------------------------------ selection helpers

  getRange() {
    const sel = this.doc.getSelection();
    if (!sel || sel.rangeCount === 0) return null;
    const r = sel.getRangeAt(0);
    return this.surface.contains(r.commonAncestorContainer) ? r : null;
  }

  selectRange(r) {
    const sel = this.doc.getSelection();
    sel.removeAllRanges();
    sel.addRange(r);
  }

  setCaret(node, offset) {
    const r = this.doc.createRange();
    r.setStart(node, offset);
    r.collapse(true);
    this.selectRange(r);
  }

  saveRange() {
    const r = this.getRange();
    this.savedRange = r ? r.cloneRange() : null;
  }

  restoreSavedRange() {
    if (this.savedRange && this.surface.contains(this.savedRange.commonAncestorContainer)) this.selectRange(this.savedRange);
  }

  /**
   * Snapshot of the selection as text positions. For the whole surface the positions are
   * relative to the containing top-level block ({block, pos}), which keeps this O(block size)
   * on large notes; with explicit `roots` they are offsets across those nodes.
   */
  saveSelection(roots = null) {
    const r = this.getRange();
    if (!r) return null;
    if (roots) {
      const a = positionOf(roots, r.startContainer, r.startOffset);
      const b = r.collapsed ? a : positionOf(roots, r.endContainer, r.endOffset);
      return a < 0 ? null : { start: a, end: b < 0 ? a : b };
    }
    const kidsOf = this.surface.childNodes;
    const point = (c, o) => {
      const top = this.topBlockOf(c);
      if (!top) return { block: Math.min(o, Math.max(0, kidsOf.length - 1)), pos: o >= kidsOf.length ? Infinity : 0 };
      const pos = positionOf([top], c, o);
      return { block: indexOf(top), pos: pos < 0 ? 0 : pos };
    };
    const start = point(r.startContainer, r.startOffset);
    return { start, end: r.collapsed ? start : point(r.endContainer, r.endOffset) };
  }

  /**
   * Runs a structural change that moves (rather than recreates) text nodes and keeps the
   * selection on the same DOM boundary points. Text positions are only a fallback, because
   * nesting changes shift the block-boundary counts they include.
   */
  keepSelection(fn) {
    const r = this.getRange();
    const nodes = r ? [r.startContainer, r.startOffset, r.endContainer, r.endOffset] : null;
    const pos = r ? this.saveSelection() : null;
    const res = fn();
    const alive = (n, off) => n && this.surface.contains(n) && off <= (n.nodeType === TEXT_NODE ? n.nodeValue.length : n.childNodes.length);
    if (nodes && alive(nodes[0], nodes[1]) && alive(nodes[2], nodes[3])) {
      try {
        this.doc.getSelection().setBaseAndExtent(nodes[0], nodes[1], nodes[2], nodes[3]);
        return res;
      } catch { /* fall through to positions */ }
    }
    if (pos) this.restoreSelection(pos);
    return res;
  }

  restoreSelection(sel, roots = null) {
    if (!sel) return;
    const resolve = (p) => {
      if (roots) return pointAt(roots, p);
      const kidsOf = this.surface.childNodes;
      if (!kidsOf.length) return null;
      const blk = kidsOf[Math.min(p.block, kidsOf.length - 1)];
      return pointAt([blk], p.pos === Infinity ? Number.MAX_SAFE_INTEGER : p.pos) || { node: blk, offset: 0 };
    };
    const a = resolve(sel.start);
    if (!a) return;
    const r = this.doc.createRange();
    try {
      r.setStart(a.node, a.offset);
      const b = sel.end !== sel.start ? resolve(sel.end) : null;
      if (b) r.setEnd(b.node, b.offset); else r.collapse(true);
      this.selectRange(r);
    } catch { /* stale positions: leave the selection alone */ }
  }

  closest(node, pred) {
    for (let n = node; n && n !== this.surface; n = n.parentNode) {
      if (n.nodeType === ELEMENT_NODE && pred(n)) return n;
    }
    return null;
  }

  closestTag(node, ...tags) { return this.closest(node, (n) => tags.includes(n.nodeName)); }

  closestBlock(node) { return this.closest(node, (n) => TEXT_BLOCKS.has(n.nodeName)); }

  topBlockOf(node) {
    let n = node;
    while (n && n.parentNode !== this.surface) n = n.parentNode;
    return n;
  }

  selectedBlocks(r = this.getRange()) {
    if (!r) return [];
    const start = this.closestBlock(r.startContainer);
    const end = this.closestBlock(r.endContainer);
    if (!start) return [];
    if (r.collapsed || start === end || !end) return [start];
    const out = [start];
    const win = this.doc.defaultView;
    const walker = this.doc.createTreeWalker(this.surface, win.NodeFilter.SHOW_ELEMENT, {
      acceptNode: (n) => (TEXT_BLOCKS.has(n.nodeName) ? win.NodeFilter.FILTER_ACCEPT : win.NodeFilter.FILTER_SKIP),
    });
    walker.currentNode = start;
    let n;
    while ((n = walker.nextNode())) {
      out.push(n);
      if (n === end) break;
    }
    return out;
  }

  /** Inline children of a block (excluding nested lists/blocks inside list items). */
  inlineEnd(block) {
    if (block.nodeName !== 'LI') return null;
    for (const c of block.childNodes) if (c.nodeType === ELEMENT_NODE && LI_BLOCKS.has(c.nodeName)) return c;
    return null;
  }

  isInlineEmpty(block) {
    const stop = this.inlineEnd(block);
    for (let c = block.firstChild; c && c !== stop; c = c.nextSibling) {
      if (c.nodeType === TEXT_NODE && (c.nodeValue || '').split(ZWSP).join('').length) return false;
      if (c.nodeType === ELEMENT_NODE && c.nodeName !== 'BR' && (c.nodeName === 'IMG' || hasInlineContent(c) || c.querySelector('img'))) return false;
    }
    return true;
  }

  caretPos(block, r) {
    const stop = this.inlineEnd(block);
    const roots = [];
    for (let c = block.firstChild; c && c !== stop; c = c.nextSibling) roots.push(c);
    let pos;
    if (r.startContainer === block) {
      pos = 0;
      for (let i = 0; i < r.startOffset && i < roots.length; i++) pos += walkPositions([roots[i]], () => false);
    } else {
      pos = positionOf(roots, r.startContainer, r.startOffset);
    }
    let total = walkPositions(roots, () => false);
    const last = roots[roots.length - 1];
    if (last && last.nodeType === ELEMENT_NODE && last.nodeName === 'BR') total -= 1; // placeholder <br>
    return { pos, total };
  }

  isCaretAtStart(block, r) { return this.caretPos(block, r).pos === 0; }

  isCaretAtEnd(block, r) {
    const { pos, total } = this.caretPos(block, r);
    return pos >= total;
  }

  caretToStart(el) {
    const stop = this.inlineEnd(el);
    for (let c = el.firstChild; c && c !== stop; c = c.nextSibling) {
      const t = firstTextIn(c);
      if (t) { this.setCaret(t, 0); return; }
      if (c.nodeType === ELEMENT_NODE && (c.nodeName === 'BR' || c.nodeName === 'IMG')) break;
    }
    this.setCaret(el, 0);
  }

  caretToEnd(el) {
    if (el.nodeName === 'PRE') {
      const code = el.querySelector('code') || el;
      const len = this.codeTextOf(el).length;
      this.setPreCaret(el, Math.max(0, len - 1));
      return code;
    }
    const stop = this.inlineEnd(el);
    let last = stop ? stop.previousSibling : el.lastChild;
    while (last && last.nodeType === ELEMENT_NODE && last.nodeName === 'BR' && last.previousSibling) last = last.previousSibling;
    if (!last || (last.nodeType === ELEMENT_NODE && last.nodeName === 'BR')) { this.setCaret(el, 0); return; }
    const t = lastTextIn(last);
    if (t) this.setCaret(t, t.nodeValue.length);
    else this.setCaret(last.parentNode, indexOf(last) + 1);
  }

  // ------------------------------------------------------------------ normalization

  /**
   * Keeps the editing DOM inside the Markdown-representable subset. Violating blocks (spans and
   * fonts from the browser, stray DIVs, root-level text…) are rebuilt via a Markdown round trip,
   * which is also what makes the subset airtight; the caret is restored by text position.
   */
  normalize() {
    const root = this.surface;
    const r = this.getRange();
    const active = new Set();
    if (r) {
      for (const n of [this.topBlockOf(r.startContainer), this.topBlockOf(r.endContainer)]) {
        if (!n) continue;
        active.add(n);
        if (n.previousSibling) active.add(n.previousSibling);
        if (n.nextSibling) active.add(n.nextSibling);
      }
    }
    const groups = [];
    let run = null;
    for (const c of [...root.childNodes]) {
      const valid = c.nodeType === ELEMENT_NODE && TOP_BLOCKS.has(c.nodeName) && (!active.has(c) || this.validTop(c));
      if (valid) { run = null; continue; }
      if (c.nodeType === ELEMENT_NODE && c.querySelector && c.querySelector('img[data-uploading]')) { run = null; continue; }
      if (!run) { run = []; groups.push(run); }
      run.push(c);
    }
    for (const g of groups) this.rebuild(g);
    this.ensureNonEmpty();
    return groups.length > 0;
  }

  validTop(el) {
    if (!this.validAttrs(el)) return false;
    const t = el.nodeName;
    if (t === 'PRE') return true; // code blocks are rebuilt by rehighlight()
    if (t === 'HR') return !el.firstChild;
    if (t === 'BLOCKQUOTE') return this.validChildren(el, (c) => QUOTE_BLOCKS.has(c) ? 'block' : null);
    if (t === 'UL' || t === 'OL') return this.validList(el);
    if (t === 'TABLE') return this.validTable(el);
    return this.validInline(el);
  }

  validAttrs(el) {
    const allowed = ALLOWED_ATTRS[el.nodeName] || [];
    for (const a of el.attributes) if (!allowed.includes(a.name)) return false;
    if (el.nodeName === 'A' && safeLinkHref(el.getAttribute('href') || '') === null) return false;
    if (el.nodeName === 'IMG' && el.getAttribute('data-uploading') === null && safeImageSrc(el.getAttribute('src') || '') === null) return false;
    return true;
  }

  validChildren(el, kind) {
    for (const c of el.childNodes) {
      if (c.nodeType === TEXT_NODE) return false;
      if (c.nodeType !== ELEMENT_NODE) continue;
      if (!kind(c.nodeName) || !this.validTop(c)) return false;
    }
    return true;
  }

  validInline(el) {
    for (const c of el.childNodes) {
      if (c.nodeType === TEXT_NODE) continue;
      if (c.nodeType !== ELEMENT_NODE) continue;
      if (!INLINE_OK.has(c.nodeName) || !this.validAttrs(c)) return false;
      if (c.nodeName === 'CODE' && c.firstElementChild) return false;
      if (!this.validInline(c)) return false;
    }
    return true;
  }

  validList(el) {
    for (const li of el.childNodes) {
      if (li.nodeType !== ELEMENT_NODE || li.nodeName !== 'LI' || !this.validAttrs(li)) return false;
      for (const c of li.childNodes) {
        if (c.nodeType === TEXT_NODE) continue;
        if (c.nodeType !== ELEMENT_NODE) continue;
        if (LI_BLOCKS.has(c.nodeName)) { if (!this.validTop(c)) return false; continue; }
        if (!INLINE_OK.has(c.nodeName) || !this.validAttrs(c) || !this.validInline(c)) return false;
      }
    }
    return true;
  }

  validTable(el) {
    for (const sec of el.childNodes) {
      if (sec.nodeType !== ELEMENT_NODE || (sec.nodeName !== 'THEAD' && sec.nodeName !== 'TBODY') || sec.attributes.length) return false;
      for (const tr of sec.childNodes) {
        if (tr.nodeType !== ELEMENT_NODE || tr.nodeName !== 'TR' || tr.attributes.length) return false;
        for (const cell of tr.childNodes) {
          if (cell.nodeType !== ELEMENT_NODE || (cell.nodeName !== 'TD' && cell.nodeName !== 'TH')) return false;
          if (!this.validAttrs(cell) || !this.validInline(cell)) return false;
        }
      }
    }
    return true;
  }

  rebuild(nodes) {
    const r = this.getRange();
    const inside = r && nodes.some((n) => n === r.startContainer || n.contains(r.startContainer));
    const sel = inside ? this.saveSelection(nodes) : null;
    const md = domToMarkdown(nodes, { styles: true });
    const frag = markdownToEditorDOM(md, this.doc, { resolveImage: this.o.resolveImage, trailingParagraph: false });
    if (!frag.firstChild) {
      const p = this.doc.createElement('p');
      p.appendChild(this.doc.createElement('br'));
      frag.appendChild(p);
    }
    const newNodes = [...frag.childNodes];
    const parent = nodes[0].parentNode;
    parent.insertBefore(frag, nodes[0]);
    for (const n of nodes) n.remove();
    if (sel) this.restoreSelection(sel, newNodes);
  }

  // ------------------------------------------------------------------ keyboard

  onKeyDown(e) {
    if (this.readOnly) return;
    const mod = isMac() ? e.metaKey : e.ctrlKey;
    const key = e.key.length === 1 ? e.key.toLowerCase() : e.key;
    const code = e.code;
    const altGraph = e.getModifierState && e.getModifierState('AltGraph');

    if (e.key === 'Escape') { this.closeLinkEditor(); return; }

    if (mod && e.altKey && !e.shiftKey && !altGraph) {
      const m = /^Digit([0-6])$/.exec(code);
      if (m) {
        e.preventDefault();
        if (m[1] === '0') this.exec('paragraph'); else this.exec('heading', Number(m[1]));
        return;
      }
    }
    if (mod && !e.altKey) {
      const r0 = key === 'k' ? this.getRange() : null;
      const cmd = shortcutCommand(key, code, { shift: e.shiftKey, inCode: !!(r0 && this.closestTag(r0.startContainer, 'PRE')) });
      if (cmd) {
        e.preventDefault();
        if (cmd === 'link') this.openLinkEditor();
        else if (cmd !== 'noop') this.exec(cmd);
        return;
      }
      if (e.key === 'Enter') {
        const r = this.getRange();
        const li = r && this.closestTag(r.startContainer, 'LI');
        const pre = r && this.closestTag(r.startContainer, 'PRE');
        if (pre) { e.preventDefault(); this.mutate(() => this.exitCodeBlock(pre)); return; }
        if (li && li.hasAttribute('data-checked')) { e.preventDefault(); this.toggleCheck(li); return; }
      }
    }
    if (e.key === 'Tab' && !mod && !e.altKey) {
      const r = this.getRange();
      if (!r) return;
      const pre = this.closestTag(r.startContainer, 'PRE');
      const cell = this.closestTag(r.startContainer, 'TD', 'TH');
      const li = this.closestTag(r.startContainer, 'LI');
      if (pre) { e.preventDefault(); this.mutate(() => this.indentCode(pre, e.shiftKey)); return; }
      if (cell) { e.preventDefault(); this.moveCell(cell, e.shiftKey ? -1 : 1, true); return; }
      if (li) { e.preventDefault(); this.exec(e.shiftKey ? 'outdent' : 'indent'); return; }
    }
    if ((e.key === 'ArrowUp' || e.key === 'ArrowLeft') && !e.shiftKey && !mod) {
      // Allow escaping above a leading code block or table (there is no other way to get there).
      const r = this.getRange();
      if (r && r.collapsed) {
        const top = this.topBlockOf(r.startContainer);
        if (top && top === this.surface.firstChild && (top.nodeName === 'PRE' || top.nodeName === 'TABLE')) {
          const block = this.closestBlock(r.startContainer);
          const firstCell = top.nodeName === 'TABLE' ? top.querySelector('th,td') : null;
          const atStart = top.nodeName === 'PRE' ? this.preOffset(top, r) === 0 : block === firstCell && this.isCaretAtStart(block, r);
          if (atStart) {
            e.preventDefault();
            this.mutate(() => {
              const p = this.doc.createElement('p');
              p.appendChild(this.doc.createElement('br'));
              this.surface.insertBefore(p, top);
              this.setCaret(p, 0);
            });
          }
        }
      }
    }
  }

  onBeforeInput(e) {
    if (this.readOnly) { e.preventDefault(); return; }
    switch (e.inputType) {
      case 'insertParagraph':
        e.preventDefault();
        this.mutate(() => this.handleEnter());
        break;
      case 'insertLineBreak': {
        const r = this.getRange();
        if (!r) break;
        const pre = this.closestTag(r.startContainer, 'PRE');
        const cell = this.closestTag(r.startContainer, 'TD', 'TH');
        if (pre) { e.preventDefault(); this.mutate(() => this.codeNewline(pre, false)); }
        else if (cell) { e.preventDefault(); this.moveCell(cell, 'down', true); }
        break;
      }
      case 'historyUndo': e.preventDefault(); this.undo(); break;
      case 'historyRedo': e.preventDefault(); this.redo(); break;
      // OS/mobile formatting menus: route through our own mark toggling.
      case 'formatBold': e.preventDefault(); this.exec('bold'); break;
      case 'formatItalic': e.preventDefault(); this.exec('italic'); break;
      case 'formatStrikeThrough': e.preventDefault(); this.exec('strike'); break;
      case 'formatUnderline': e.preventDefault(); break;
      case 'formatFontColor': case 'formatBackColor': case 'formatFontName': case 'formatJustifyCenter':
      case 'formatJustifyFull': case 'formatJustifyRight': case 'formatJustifyLeft': case 'formatSuperscript':
      case 'formatSubscript': case 'formatIndent': case 'formatOutdent':
        e.preventDefault();
        break;
      case 'deleteContentBackward':
        if (this.handleBackspace()) e.preventDefault();
        break;
      case 'deleteContentForward':
        if (this.handleDelete()) e.preventDefault();
        break;
      case 'insertFromPaste': case 'insertFromDrop': case 'insertFromPasteAsQuotation': case 'insertReplacementText':
        // Desktop pastes arrive via the paste/drop listeners first; this path covers mobile
        // keyboards' clipboard suggestions. Raw HTML never reaches the DOM unsanitized.
        if (e.dataTransfer) {
          e.preventDefault();
          this.insertClipboard({ html: e.dataTransfer.getData('text/html'), plain: e.dataTransfer.getData('text/plain'), vscode: '', plainOnly: false });
        } else if (e.inputType !== 'insertReplacementText' && typeof e.data === 'string') {
          e.preventDefault();
          this.insertClipboard({ html: '', plain: e.data, vscode: '', plainOnly: false });
        }
        break;
      default:
        break;
    }
  }

  onInput(e) {
    if (this.readOnly || this.composing || e.isComposing) return;
    this.afterInput(e.inputType, e.data);
  }

  afterInput(type, data) {
    const r = this.getRange();
    const pre = r && this.closestTag(r.startContainer, 'PRE');
    if (pre) {
      this.scheduleRehighlight(pre);
    } else if (r && (type === 'insertText' || type === 'insertCompositionText') && data) {
      // Keyed off the last inserted character so autocorrect/IME multi-char inserts work too.
      const last = data[data.length - 1];
      if (last === ' ' || last === NBSP) this.autoformatBlock(r);
      else if ('*_`~'.includes(last)) this.autoformatInline(r);
    }
    this.normalize();
    this.updatePlaceholder();
    this.markDirty();
    this.scheduleState();
  }

  // ------------------------------------------------------------------ autoformat

  autoformatBlock(r) {
    if (!r.collapsed) return;
    const block = this.closestBlock(r.startContainer);
    if (!block || !/^(P|DIV|LI)$/.test(block.nodeName)) return;
    const pre = this.doc.createRange();
    pre.setStart(block, 0);
    pre.setEnd(r.startContainer, r.startOffset);
    const before = pre.toString().split(NBSP).join(' ');
    if (before.length > 12) return;
    // Each rule returns the element that should receive the caret.
    let apply = null;
    let m;
    if (block.nodeName === 'LI') {
      if (/^\[[ xX]?\] $/.test(before) && !block.hasAttribute('data-checked')) {
        apply = () => { block.setAttribute('data-checked', /x/i.test(before) ? 'true' : 'false'); this.retagItemList(block, 'UL'); return block; };
      }
    } else if ((m = /^(#{1,6}) $/.exec(before))) {
      const level = m[1].length;
      apply = () => this.retag(block, 'H' + level);
    } else if (/^[-*+] \[[ xX]?\] $/.test(before) || /^\[[ xX]?\] $/.test(before)) {
      apply = () => this.blockToList(block, 'task', /x/i.test(before));
    } else if (/^[-*+] $/.test(before)) {
      apply = () => this.blockToList(block, 'ul');
    } else if ((m = /^(\d{1,9})[.)] $/.exec(before))) {
      const start = Number(m[1]);
      apply = () => this.blockToList(block, 'ol', false, start);
    } else if (/^> $/.test(before) && block.parentNode === this.surface) {
      apply = () => {
        const q = this.doc.createElement('blockquote');
        block.parentNode.insertBefore(q, block);
        q.appendChild(block);
        this.mergeAdjacent(q, 'BLOCKQUOTE');
        return block;
      };
    }
    if (!apply) return;
    // Record the literal text first so undo restores what was typed.
    this.dirty = true;
    this.commit();
    pre.deleteContents();
    const target = apply();
    if (target && this.surface.contains(target)) {
      ensurePlaceholder(this.doc, target);
      this.caretToStart(target);
    }
    this.normalize();
    this.dirty = true;
    this.commit();
  }

  blockToList(block, kind, checked = false, start = 1) {
    const d = this.doc;
    const li = d.createElement('li');
    if (kind === 'task') li.setAttribute('data-checked', String(!!checked));
    while (block.firstChild) li.appendChild(block.firstChild);
    ensurePlaceholder(d, li);
    const tag = kind === 'ol' ? 'OL' : 'UL';
    const prev = block.previousSibling;
    let list;
    if (prev && prev.nodeName === tag && (tag !== 'OL' || start === 1)) list = prev;
    else {
      list = d.createElement(tag);
      if (tag === 'OL' && start !== 1) list.setAttribute('start', String(start));
      block.parentNode.insertBefore(list, block);
    }
    list.appendChild(li);
    block.remove();
    this.mergeAdjacent(list, tag);
    this.caretToStart(li);
    return li;
  }

  autoformatInline(r) {
    if (!r.collapsed || r.startContainer.nodeType !== TEXT_NODE) return;
    const node = r.startContainer;
    if (this.closestTag(node, 'CODE', 'PRE')) return;
    const full = node.nodeValue.slice(0, r.startOffset);
    const base = Math.max(0, full.length - 400);
    const text = full.slice(base);
    const rules = [
      [/(^|[^`])`([^`]+)`$/, 'code'],
      [/(^|[\s(["'])\*\*([^*\s](?:[^*]*[^*\s])?)\*\*$/, 'strong'],
      [/(^|[\s(["'])__([^_\s](?:[^_]*[^_\s])?)__$/, 'strong'],
      [/(^|[\s(["'])\*([^*\s](?:[^*]*[^*\s])?)\*$/, 'em'],
      [/(^|[\s(["'])_([^_\s](?:[^_]*[^_\s])?)_$/, 'em'],
      [/(^|[^~])~~([^~\s](?:[^~]*[^~\s])?)~~$/, 's'],
    ];
    for (const [re, tag] of rules) {
      const m = re.exec(text);
      if (!m) continue;
      const start = base + m.index + m[1].length;
      this.dirty = true;
      this.commit();
      const range = this.doc.createRange();
      range.setStart(node, start);
      range.setEnd(node, r.startOffset);
      range.deleteContents();
      const el = this.doc.createElement(tag);
      el.textContent = m[2];
      range.insertNode(el);
      // A zero-width text node after the element makes further typing unformatted.
      const after = this.doc.createTextNode(ZWSP);
      el.parentNode.insertBefore(after, el.nextSibling);
      this.setCaret(after, 1);
      this.dirty = true;
      this.commit();
      return;
    }
  }

  // ------------------------------------------------------------------ Enter / Backspace / Delete

  handleEnter() {
    let r = this.getRange();
    if (!r) return;
    if (!r.collapsed) {
      const pre = this.closestTag(r.startContainer, 'PRE');
      if (pre && pre === this.closestTag(r.endContainer, 'PRE')) { this.codeNewline(pre, true); return; }
      const a = this.closestBlock(r.startContainer), z = this.closestBlock(r.endContainer);
      r.deleteContents();
      if (a && z && a !== z && this.surface.contains(z)) {
        // Deleting across blocks already leaves the two halves as separate blocks.
        pruneEmptyInlines(a);
        ensurePlaceholder(this.doc, a);
        ensurePlaceholder(this.doc, z);
        this.caretToStart(z);
        return;
      }
      r = this.getRange();
      if (!r) return;
    }
    const block = this.closestBlock(r.startContainer);
    if (!block) {
      const p = this.doc.createElement('p');
      p.appendChild(this.doc.createElement('br'));
      this.surface.appendChild(p);
      this.setCaret(p, 0);
      return;
    }
    const tag = block.nodeName;
    if (tag === 'PRE') { this.codeNewline(block, true); return; }
    if (tag === 'TD' || tag === 'TH') { this.moveCell(block, 'down', true); return; }
    if (tag === 'P' || tag === 'DIV') {
      const txt = (block.textContent || '').split(ZWSP).join('').trim();
      let m;
      if ((m = /^(```|~~~)[ \t]*([\w+#.-]*)$/.exec(txt))) { this.replaceWithCodeBlock(block, m[2]); return; }
      if (/^(?:-{3,}|\*{3,}|_{3,})$/.test(txt)) {
        const hr = this.doc.createElement('hr');
        block.parentNode.insertBefore(hr, block);
        while (block.firstChild) block.removeChild(block.firstChild);
        block.appendChild(this.doc.createElement('br'));
        this.setCaret(block, 0);
        return;
      }
    }
    if (tag === 'LI') {
      if (this.isInlineEmpty(block)) {
        if (this.isNestedItem(block)) { this.outdentItem(block); this.caretToStart(block); }
        else this.liftItem(block);
        return;
      }
      this.splitBlock(block, r, 'LI');
      return;
    }
    if (/^H[1-6]$/.test(tag)) {
      this.splitBlock(block, r, this.isCaretAtEnd(block, r) ? 'P' : tag);
      return;
    }
    const parent = block.parentNode;
    if (this.isInlineEmpty(block) && parent.nodeName === 'BLOCKQUOTE' && !block.nextSibling) {
      parent.parentNode.insertBefore(block, parent.nextSibling);
      if (!parent.firstChild) parent.remove();
      this.caretToStart(block);
      return;
    }
    this.splitBlock(block, r, 'P');
  }

  splitBlock(block, r, newTag) {
    const d = this.doc;
    const tail = d.createRange();
    tail.setStart(r.startContainer, r.startOffset);
    const stop = this.inlineEnd(block);
    if (stop) tail.setEndBefore(stop); else tail.setEnd(block, block.childNodes.length);
    const frag = tail.extractContents();
    const nb = d.createElement(newTag);
    if (newTag === 'LI' && block.hasAttribute('data-checked')) nb.setAttribute('data-checked', 'false');
    nb.appendChild(frag);
    if (stop) {
      for (let n = stop; n; ) { const nx = n.nextSibling; nb.appendChild(n); n = nx; }
    }
    pruneEmptyInlines(nb);
    pruneEmptyInlines(block);
    ensurePlaceholder(d, block);
    ensurePlaceholder(d, nb);
    block.parentNode.insertBefore(nb, block.nextSibling);
    this.caretToStart(nb);
    return nb;
  }

  handleBackspace() {
    const r = this.getRange();
    if (!r || !r.collapsed) return false;
    const block = this.closestBlock(r.startContainer);
    if (!block) return false;
    const tag = block.nodeName;
    if (tag === 'PRE') {
      const off = this.preOffset(block, r);
      if (off > 0) {
        // Smart unindent: a run of spaces back to a tab stop goes in one keystroke.
        const text = this.codeTextOf(block);
        const lineStart = text.lastIndexOf('\n', off - 1) + 1;
        const before = text.slice(lineStart, off);
        if (before.length >= 2 && /^ +$/.test(before)) {
          const n = before.length % 4 || 4;
          this.mutate(() => this.replaceInPre(block, off - n, off, ''));
          return true;
        }
        return false;
      }
      if (this.codeTextOf(block).replace(/\n$/, '') === '') {
        this.mutate(() => {
          const p = this.doc.createElement('p');
          p.appendChild(this.doc.createElement('br'));
          block.parentNode.replaceChild(p, block);
          this.setCaret(p, 0);
        });
      }
      return true;
    }
    if (!this.isCaretAtStart(block, r)) return false;
    if (tag === 'LI') {
      this.mutate(() => {
        if (this.isNestedItem(block)) this.keepSelection(() => this.outdentItem(block));
        else this.liftItem(block);
      });
      return true;
    }
    if (/^H[1-6]$/.test(tag)) { this.mutate(() => this.retag(block, 'P')); return true; }
    if (tag === 'TD' || tag === 'TH') return true;
    const parent = block.parentNode;
    if (parent.nodeName === 'BLOCKQUOTE' && !block.previousSibling) {
      this.mutate(() => {
        parent.parentNode.insertBefore(block, parent);
        if (!parent.firstChild) parent.remove();
        this.caretToStart(block);
      });
      return true;
    }
    const prev = block.previousSibling;
    if (prev && prev.nodeType === ELEMENT_NODE) {
      if (prev.nodeName === 'HR') { this.mutate(() => { prev.remove(); this.caretToStart(block); }); return true; }
      if (prev.nodeName === 'PRE' || prev.nodeName === 'TABLE') {
        this.mutate(() => {
          const empty = this.isInlineEmpty(block);
          if (empty) block.remove();
          if (prev.nodeName === 'PRE') this.caretToEnd(prev);
          else { const cells = prev.querySelectorAll('th,td'); this.caretToEnd(cells[cells.length - 1]); }
        });
        return true;
      }
    }
    return false;
  }

  handleDelete() {
    const r = this.getRange();
    if (!r || !r.collapsed) return false;
    const block = this.closestBlock(r.startContainer);
    if (!block) return false;
    if (block.nodeName === 'PRE') return this.preOffset(block, r) >= this.codeTextOf(block).length - 1;
    if (!this.isCaretAtEnd(block, r)) return false;
    if (block.nodeName === 'TD' || block.nodeName === 'TH') return true;
    const next = block.nextSibling;
    if (next && next.nodeType === ELEMENT_NODE) {
      if (next.nodeName === 'HR') { this.mutate(() => next.remove()); return true; }
      if (next.nodeName === 'PRE' || next.nodeName === 'TABLE') return true;
    }
    return false;
  }

  // ------------------------------------------------------------------ block commands

  retag(block, tag) {
    if (block.nodeName === tag) return block;
    const n = this.doc.createElement(tag);
    while (block.firstChild) n.appendChild(block.firstChild);
    block.parentNode.replaceChild(n, block);
    ensurePlaceholder(this.doc, n);
    return n;
  }

  /** Turns the current blocks into paragraphs or headings (lifting list items, unwrapping code). */
  setBlockType(tag) {
    const r = this.getRange();
    if (!r) return;
    const blocks = this.selectedBlocks(r);
    this.keepSelection(() => {
      for (let b of blocks) {
        if (!this.surface.contains(b)) continue;
        if (b.nodeName === 'TD' || b.nodeName === 'TH') continue;
        if (b.nodeName === 'PRE') { for (const p of this.codeToParagraphs(b)) this.retag(p, tag); continue; }
        if (b.nodeName === 'LI') b = this.liftItem(b);
        this.retag(b, tag);
      }
    });
  }

  isNestedItem(li) {
    const list = li.parentNode;
    return !!list && !!list.parentNode && list.parentNode.nodeName === 'LI';
  }

  listKindOf(li) {
    return li.hasAttribute('data-checked') ? 'task' : li.parentNode && li.parentNode.nodeName === 'OL' ? 'ol' : 'ul';
  }

  /** Removes an item from its list, turning it into a paragraph; the list is split around it. */
  liftItem(li) {
    const d = this.doc;
    const list = li.parentNode;
    const tag = list.nodeName;
    const after = [];
    for (let n = li.nextSibling; n; n = n.nextSibling) after.push(n);
    const p = d.createElement('p');
    const blocks = [];
    for (const c of [...li.childNodes]) {
      if (c.nodeType === ELEMENT_NODE && LI_BLOCKS.has(c.nodeName)) blocks.push(c);
      else p.appendChild(c);
    }
    pruneEmptyInlines(p);
    ensurePlaceholder(d, p);
    const container = list.parentNode;
    let ref = list.nextSibling;
    container.insertBefore(p, ref);
    for (const b of blocks) container.insertBefore(b, ref);
    if (after.length) {
      const l2 = d.createElement(tag);
      if (tag === 'OL') {
        const start = clampStart(list.getAttribute('start')) + [...list.childNodes].indexOf(li) + 1;
        if (start !== 1) l2.setAttribute('start', String(start));
      }
      for (const a of after) l2.appendChild(a);
      container.insertBefore(l2, ref);
    }
    li.remove();
    if (!list.firstChild) list.remove();
    this.caretToStart(p);
    return p;
  }

  indentItem(li) {
    const prev = li.previousSibling;
    if (!prev || prev.nodeName !== 'LI') return false;
    let depth = 0;
    for (let n = li; n && n !== this.surface; n = n.parentNode) if (n.nodeName === 'LI') depth++;
    if (depth >= MAX_LIST_DEPTH) return false;
    const tag = li.parentNode.nodeName;
    let sub = prev.lastChild;
    if (!sub || sub.nodeType !== ELEMENT_NODE || (sub.nodeName !== 'UL' && sub.nodeName !== 'OL')) {
      sub = this.doc.createElement(tag);
      prev.appendChild(sub);
    }
    sub.appendChild(li);
    return true;
  }

  outdentItem(li) {
    const list = li.parentNode;
    const parentLi = list.parentNode;
    if (!parentLi || parentLi.nodeName !== 'LI') return false;
    const following = [];
    for (let n = li.nextSibling; n; n = n.nextSibling) following.push(n);
    if (following.length) {
      let sub = li.lastChild;
      if (!sub || sub.nodeType !== ELEMENT_NODE || sub.nodeName !== list.nodeName) {
        sub = this.doc.createElement(list.nodeName);
        li.appendChild(sub);
      }
      for (const f of following) sub.appendChild(f);
    }
    parentLi.parentNode.insertBefore(li, parentLi.nextSibling);
    if (!list.firstChild) list.remove();
    return true;
  }

  /** Moves a single item into a list of the given tag, splitting its current list if needed. */
  retagItemList(li, tag) {
    const list = li.parentNode;
    if (list.nodeName === tag) return;
    const d = this.doc;
    const items = [...list.childNodes];
    const i = items.indexOf(li);
    const parent = list.parentNode;
    const target = d.createElement(tag);
    parent.insertBefore(target, list.nextSibling);
    const rest = items.slice(i + 1);
    if (rest.length) {
      const tailList = d.createElement(list.nodeName);
      for (const n of rest) tailList.appendChild(n);
      parent.insertBefore(tailList, target.nextSibling);
    }
    target.appendChild(li);
    if (!list.firstChild) list.remove();
    this.mergeAdjacent(target, tag);
  }

  mergeAdjacent(el, tag) {
    const prev = el.previousSibling;
    if (prev && prev.nodeType === ELEMENT_NODE && prev.nodeName === tag) {
      while (el.firstChild) prev.appendChild(el.firstChild);
      el.remove();
      el = prev;
    }
    const next = el.nextSibling;
    if (next && next.nodeType === ELEMENT_NODE && next.nodeName === tag) {
      while (next.firstChild) el.appendChild(next.firstChild);
      next.remove();
    }
    return el;
  }

  toggleList(kind) {
    const r = this.getRange();
    if (!r) return;
    const blocks = this.selectedBlocks(r).filter((b) => !['TD', 'TH', 'PRE'].includes(b.nodeName));
    if (!blocks.length) return;
    this.keepSelection(() => {
      if (blocks.every((b) => b.nodeName === 'LI' && this.listKindOf(b) === kind)) {
        for (const li of blocks.slice().reverse()) if (this.surface.contains(li)) this.liftItem(li);
        return;
      }
      const tag = kind === 'ol' ? 'OL' : 'UL';
      for (const b of blocks) {
        if (!this.surface.contains(b)) continue;
        if (b.nodeName === 'LI') {
          if (kind === 'task') { if (!b.hasAttribute('data-checked')) b.setAttribute('data-checked', 'false'); this.retagItemList(b, 'UL'); }
          else { b.removeAttribute('data-checked'); this.retagItemList(b, tag); }
          continue;
        }
        if (b.parentNode && b.parentNode.nodeName === 'LI') continue; // paragraph inside an item
        this.blockToList(b, kind);
      }
    });
  }

  toggleQuote() {
    const r = this.getRange();
    if (!r) return;
    const blocks = this.selectedBlocks(r);
    const quotes = blocks.map((b) => this.closestTag(b, 'BLOCKQUOTE'));
    const a = this.topBlockOf(r.startContainer), z = this.topBlockOf(r.endContainer);
    this.keepSelection(() => {
      if (blocks.length && quotes.every(Boolean)) {
        for (const q of new Set(quotes)) {
          if (!q.parentNode) continue;
          while (q.firstChild) q.parentNode.insertBefore(q.firstChild, q);
          q.remove();
        }
        return;
      }
      if (!a || !z) return;
      const q = this.doc.createElement('blockquote');
      this.surface.insertBefore(q, a);
      for (let n = a; n; ) {
        const nx = n.nextSibling;
        q.appendChild(n);
        if (n === z) break;
        n = nx;
      }
      this.mergeAdjacent(q, 'BLOCKQUOTE');
    });
  }

  codeToParagraphs(pre) {
    const d = this.doc;
    const lines = this.codeTextOf(pre).replace(/\n$/, '').split('\n');
    const ps = lines.map((line) => {
      const p = d.createElement('p');
      if (line) p.appendChild(d.createTextNode(line)); else p.appendChild(d.createElement('br'));
      return p;
    });
    for (const p of ps) pre.parentNode.insertBefore(p, pre);
    pre.remove();
    return ps;
  }

  replaceWithCodeBlock(block, lang, text = '') {
    const pre = makeCodeBlock(this.doc, text, lang);
    block.parentNode.replaceChild(pre, block);
    if (!pre.nextSibling && pre.parentNode === this.surface) {
      const p = this.doc.createElement('p');
      p.appendChild(this.doc.createElement('br'));
      this.surface.appendChild(p);
    }
    this.setPreCaret(pre, text.length);
    return pre;
  }

  toggleCodeBlock(lang) {
    const r = this.getRange();
    if (!r) return;
    const blocks = this.selectedBlocks(r).filter((b) => b.nodeName !== 'TD' && b.nodeName !== 'TH');
    if (!blocks.length) return;
    if (blocks.length === 1 && blocks[0].nodeName === 'PRE') {
      const ps = this.codeToParagraphs(blocks[0]);
      this.caretToStart(ps[0]);
      return;
    }
    const usable = blocks.filter((b) => b.nodeName !== 'PRE').map((b) => (b.nodeName === 'LI' ? this.liftItem(b) : b));
    const text = usable.map((b) => blockPlainText(b)).join('\n');
    const first = usable[0];
    for (const b of usable.slice(1)) b.remove();
    this.replaceWithCodeBlock(first, lang || '', text);
  }

  exitCodeBlock(pre) {
    let p = pre.nextSibling;
    if (!p || p.nodeName !== 'P') {
      p = this.doc.createElement('p');
      p.appendChild(this.doc.createElement('br'));
      pre.parentNode.insertBefore(p, pre.nextSibling);
    }
    this.caretToStart(p);
  }

  insertBlockAfterCurrent(node, { replaceEmpty = true } = {}) {
    const r = this.getRange();
    const d = this.doc;
    let anchor = r ? this.closestBlock(r.startContainer) : null;
    if (anchor && (anchor.nodeName === 'TD' || anchor.nodeName === 'TH')) anchor = this.closestTag(anchor, 'TABLE');
    if (anchor && anchor.nodeName === 'LI') anchor = this.topBlockOf(anchor);
    if (!anchor) anchor = this.surface.lastChild;
    if (replaceEmpty && anchor && anchor.nodeName === 'P' && this.isInlineEmpty(anchor)) {
      anchor.parentNode.replaceChild(node, anchor);
    } else if (anchor) {
      anchor.parentNode.insertBefore(node, anchor.nextSibling);
    } else {
      this.surface.appendChild(node);
    }
    let after = node.nextSibling;
    if (!after || after.nodeName !== 'P') {
      after = d.createElement('p');
      after.appendChild(d.createElement('br'));
      node.parentNode.insertBefore(after, node.nextSibling);
    }
    return after;
  }

  insertTable(rows = 3, cols = 3) {
    const d = this.doc;
    const table = d.createElement('table');
    const mkRow = (tag) => {
      const tr = d.createElement('tr');
      for (let i = 0; i < cols; i++) { const c = d.createElement(tag); c.appendChild(d.createElement('br')); tr.appendChild(c); }
      return tr;
    };
    const thead = d.createElement('thead');
    thead.appendChild(mkRow('th'));
    const tbody = d.createElement('tbody');
    for (let i = 1; i < rows; i++) tbody.appendChild(mkRow('td'));
    table.append(thead, tbody);
    this.insertBlockAfterCurrent(table);
    this.setCaret(table.querySelector('th'), 0);
  }

  insertHr() {
    const after = this.insertBlockAfterCurrent(this.doc.createElement('hr'));
    this.caretToStart(after);
  }

  // ------------------------------------------------------------------ inline commands

  toggleInlineCode() {
    const r = this.getRange();
    if (!r || this.closestTag(r.startContainer, 'PRE')) return;
    const existing = this.closestTag(r.commonAncestorContainer, 'CODE');
    if (existing) {
      this.keepSelection(() => unwrap(existing));
      return;
    }
    if (this.closestBlock(r.startContainer) !== this.closestBlock(r.endContainer)) { this.toggleCodeBlock(); return; }
    const code = this.doc.createElement('code');
    if (r.collapsed) {
      code.appendChild(this.doc.createTextNode(ZWSP));
      r.insertNode(code);
      this.setCaret(code.firstChild, 1);
      return;
    }
    const text = r.toString().split(ZWSP).join('').replace(/\s*\n\s*/g, ' ');
    if (!text) return;
    r.deleteContents();
    code.appendChild(this.doc.createTextNode(text));
    r.insertNode(code);
    const nr = this.doc.createRange();
    nr.selectNodeContents(code);
    this.selectRange(nr);
  }

  /** Position of a boundary point within a block's inline roots (clamped to the inline content). */
  blockPos(block, roots, c, o) {
    const total = walkPositions(roots, () => false);
    if (c === block) {
      let pos = 0;
      for (let i = 0; i < o && i < roots.length; i++) pos += walkPositions([roots[i]], () => false);
      return Math.min(pos, total);
    }
    const p = positionOf(roots, c, o);
    return p < 0 ? total : p;
  }

  /**
   * Toggles bold/italic/strikethrough over the selection. If every selected character already
   * has the mark it is removed, otherwise it is applied; a collapsed caret gets a zero-width
   * anchor so the next typed text carries (or drops) the mark.
   */
  toggleMark(mark) {
    const r = this.getRange();
    if (!r || this.closestTag(r.startContainer, 'PRE')) return;
    const blocks = this.selectedBlocks(r).filter((b) => b.nodeName !== 'PRE');
    if (!blocks.length) return;
    const info = blocks.map((b, i) => {
      const stop = this.inlineEnd(b);
      const roots = [];
      for (let c = b.firstChild; c && c !== stop; c = c.nextSibling) roots.push(c);
      const total = walkPositions(roots, () => false);
      const a = i === 0 ? this.blockPos(b, roots, r.startContainer, r.startOffset) : 0;
      const z = i === blocks.length - 1 ? this.blockPos(b, roots, r.endContainer, r.endOffset) : total;
      return { b, stop, roots, a, z: Math.max(a, z), segs: editSegments(roots, NO_MARKS, []) };
    });
    const collapsed = r.collapsed || info.every((x) => x.a === x.z);
    let on;
    if (collapsed) {
      const strongTags = { strong: ['STRONG', 'B'], em: ['EM', 'I'], del: ['S', 'DEL', 'STRIKE'] }[mark];
      on = !this.closestTag(r.startContainer, ...strongTags);
    } else {
      let all = true, any = false;
      for (const x of info) {
        let acc = 0;
        for (const s of x.segs) {
          const len = segLength(s);
          const overlap = Math.min(acc + len, x.z) - Math.max(acc, x.a);
          if (overlap > 0 && s.k !== 'br') { any = true; if (!s.m[mark]) all = false; }
          acc += len;
        }
      }
      on = !(any && all);
    }
    for (const x of info) {
      splitSegmentsAt(x.segs, x.a);
      splitSegmentsAt(x.segs, x.z);
      if (collapsed) {
        // Insert a zero-width anchor carrying the toggled marks of the caret position.
        let acc = 0, idx = x.segs.length, base = NO_MARKS;
        for (let i = 0; i < x.segs.length; i++) {
          if (acc >= x.a) { idx = i; break; }
          base = x.segs[i].m;
          acc += segLength(x.segs[i]);
        }
        if (idx === 0 && x.segs[0]) base = x.segs[0].m;
        x.segs.splice(idx, 0, { k: 'text', t: ZWSP, m: withMark(base, mark, on) });
      } else {
        let acc = 0;
        for (const s of x.segs) {
          const len = segLength(s);
          if (acc >= x.a && acc + len <= x.z && s.k !== 'br') s.m = withMark(s.m, mark, on);
          acc += len;
        }
      }
      const frag = buildInline(this.doc, x.segs);
      for (const n of x.roots) n.remove();
      x.b.insertBefore(frag, x.stop);
      ensurePlaceholder(this.doc, x.b);
      x.newRoots = [];
      for (let c = x.b.firstChild; c && c !== x.stop; c = c.nextSibling) x.newRoots.push(c);
    }
    const first = info[0], last = info[info.length - 1];
    const a = pointAt(first.newRoots, collapsed ? first.a + 1 : first.a, !collapsed);
    const z = collapsed ? a : pointAt(last.newRoots, last.z);
    if (a && z) this.doc.getSelection().setBaseAndExtent(a.node, a.offset, z.node, z.offset);
  }

  native(command, value = null) {
    const r = this.getRange();
    if (!r || this.closestTag(r.startContainer, 'PRE')) return;
    this.surface.focus({ preventScroll: true });
    try { this.doc.execCommand(command, false, value); } catch { /* unsupported */ }
  }

  // ------------------------------------------------------------------ links

  currentLink() {
    const r = this.getRange();
    return r ? this.closestTag(r.startContainer, 'A') : null;
  }

  openLinkEditor() {
    if (this.readOnly) return;
    this.saveRange();
    const r = this.savedRange;
    if (!r) { this.surface.focus(); this.saveRange(); }
    const a = this.currentLink();
    let value = a ? a.getAttribute('href') || '' : '';
    if (!a && this.savedRange && !this.savedRange.collapsed) {
      const t = this.savedRange.toString().trim();
      if (/^(?:https?:\/\/|mailto:|www\.)\S+$/i.test(t)) value = t;
    }
    this.linkBubble.hidden = false;
    this.linkBubble.classList.add('is-editing');
    this.linkInput.value = value;
    this.linkInput.classList.remove('is-invalid');
    this.positionLinkBubble(a);
    this.linkInput.focus();
    this.linkInput.select();
  }

  closeLinkEditor() {
    this.linkBubble.classList.remove('is-editing');
    this.linkBubble.hidden = true;
  }

  normalizeUrl(raw) {
    let u = String(raw || '').trim();
    if (!u) return '';
    if (/^www\./i.test(u)) u = 'https://' + u;
    else if (/^[\w-]+(\.[\w-]+)+(?:[/:?#]|$)/.test(u) && !/^[a-z][a-z0-9+.-]*:/i.test(u)) u = 'https://' + u;
    else if (/^[^\s@/:]+@[^\s@/]+\.[^\s@/]+$/.test(u)) u = 'mailto:' + u;
    return safeLinkHref(u) === null || /\s/.test(u) ? null : u;
  }

  applyLinkFromInput() {
    const raw = this.linkInput.value;
    if (!raw.trim()) { this.closeLinkEditor(); this.restoreSavedRange(); this.removeLink(); return; }
    const url = this.normalizeUrl(raw);
    if (!url) { this.linkInput.classList.add('is-invalid'); this.linkInput.focus(); return; }
    this.closeLinkEditor();
    this.surface.focus({ preventScroll: true });
    this.restoreSavedRange();
    this.mutate(() => this.applyLink(url));
  }

  applyLink(url) {
    const r = this.getRange();
    if (!r) return;
    const existing = this.closestTag(r.startContainer, 'A');
    if (existing && r.collapsed) {
      existing.setAttribute('href', url);
      existing.removeAttribute('data-md-autolink');
      return;
    }
    if (r.collapsed) {
      const a = this.doc.createElement('a');
      a.setAttribute('href', url);
      a.textContent = url.replace(/^mailto:/i, '');
      r.insertNode(a);
      const after = this.doc.createTextNode(ZWSP);
      a.parentNode.insertBefore(after, a.nextSibling);
      this.setCaret(after, 1);
      return;
    }
    try { this.doc.execCommand('createLink', false, url); } catch { /* ignore */ }
    for (const a of this.surface.querySelectorAll('a')) if (a.getAttribute('href') === url) a.removeAttribute('data-md-autolink');
  }

  removeLink() {
    const a = this.currentLink();
    this.closeLinkEditor();
    if (!a) { this.native('unlink'); this.markDirty(); return; }
    this.mutate(() => this.keepSelection(() => unwrap(a)));
  }

  positionLinkBubble(a) {
    const target = a || (this.savedRange && !this.savedRange.collapsed ? this.savedRange : null) || (this.savedRange ? this.savedRange : null);
    if (!target) return;
    const rect = target.getBoundingClientRect ? target.getBoundingClientRect() : null;
    this.place(this.linkBubble, rect, 'below');
  }

  // ------------------------------------------------------------------ code blocks

  codeTextOf(pre) {
    return codeText(pre).split(ZWSP).join('').split(NBSP).join(' ');
  }

  preOffset(pre, r) {
    const pos = positionOf([pre], r.startContainer, r.startOffset);
    return pos < 0 ? 0 : pos;
  }

  setPreCaret(pre, offset) {
    const p = pointAt([pre], offset);
    if (p) this.setCaret(p.node, p.offset);
    else this.setCaret(pre, 0);
  }

  rehighlight(pre, keepCaret = true) {
    const text = this.codeTextOf(pre);
    const value = text.endsWith('\n') ? text.slice(0, -1) : text;
    const r = keepCaret ? this.getRange() : null;
    const inside = r && pre.contains(r.startContainer);
    const start = inside ? this.preOffset(pre, r) : -1;
    const end = inside && !r.collapsed && pre.contains(r.endContainer) ? positionOf([pre], r.endContainer, r.endOffset) : start;
    let code = pre.firstChild;
    if (!code || code.nodeName !== 'CODE' || pre.childNodes.length !== 1) {
      while (pre.firstChild) pre.removeChild(pre.firstChild);
      code = this.doc.createElement('code');
      pre.appendChild(code);
    }
    fillCode(this.doc, code, value, pre.getAttribute('data-lang') || '');
    if (inside && start >= 0) {
      const a = pointAt([pre], Math.min(start, value.length));
      const b = end !== start ? pointAt([pre], Math.min(end, value.length)) : null;
      if (a) {
        const nr = this.doc.createRange();
        nr.setStart(a.node, a.offset);
        if (b) nr.setEnd(b.node, b.offset); else nr.collapse(true);
        this.selectRange(nr);
      }
    }
  }

  scheduleRehighlight(pre) {
    clearTimeout(this.hlTimers.get(pre));
    this.hlTimers.set(pre, setTimeout(() => {
      this.hlTimers.delete(pre);
      if (this.surface.contains(pre) && !this.composing) this.rehighlight(pre);
    }, 150));
  }

  replaceInPre(pre, start, end, str) {
    const text = this.codeTextOf(pre);
    const value = text.endsWith('\n') ? text.slice(0, -1) : text;
    const next = value.slice(0, start) + str + value.slice(Math.min(end, value.length));
    fillCode(this.doc, pre.firstChild && pre.firstChild.nodeName === 'CODE' ? pre.firstChild : (() => {
      while (pre.firstChild) pre.removeChild(pre.firstChild);
      const c = this.doc.createElement('code');
      pre.appendChild(c);
      return c;
    })(), next, pre.getAttribute('data-lang') || '');
    this.setPreCaret(pre, start + str.length);
  }

  codeNewline(pre, allowExit) {
    const r = this.getRange();
    const text = this.codeTextOf(pre);
    const value = text.endsWith('\n') ? text.slice(0, -1) : text;
    const start = this.preOffset(pre, r);
    const end = r.collapsed ? start : positionOf([pre], r.endContainer, r.endOffset);
    const before = value.slice(0, start);
    const line = before.slice(before.lastIndexOf('\n') + 1);
    if (allowExit && r.collapsed && start >= value.length && line === '' && before.endsWith('\n')) {
      // Enter on an empty last line leaves the code block.
      this.replaceInPre(pre, start - 1, start, '');
      this.exitCodeBlock(pre);
      return;
    }
    const indent = /^[ \t]*/.exec(line)[0];
    const extra = /[{[(:]\s*$/.test(line) ? '    ' : '';
    this.replaceInPre(pre, start, end, '\n' + indent + extra);
  }

  indentCode(pre, outdent) {
    const r = this.getRange();
    const text = this.codeTextOf(pre);
    const value = text.endsWith('\n') ? text.slice(0, -1) : text;
    const start = this.preOffset(pre, r);
    const end = r.collapsed ? start : Math.max(start, positionOf([pre], r.endContainer, r.endOffset));
    if (!outdent && r.collapsed) { this.replaceInPre(pre, start, start, '    '); return; }
    const lineStart = value.lastIndexOf('\n', start - 1) + 1;
    const lines = value.slice(lineStart, end).split('\n');
    let delta0 = 0, total = 0;
    const out = lines.map((l, i) => {
      if (outdent) {
        const m = /^(?: {1,4}|\t)/.exec(l);
        const n = m ? m[0].length : 0;
        if (i === 0) delta0 = -n;
        total -= n;
        return l.slice(n);
      }
      if (i === 0) delta0 = 4;
      total += 4;
      return '    ' + l;
    });
    const next = value.slice(0, lineStart) + out.join('\n') + value.slice(end);
    const code = pre.firstChild && pre.firstChild.nodeName === 'CODE' ? pre.firstChild : pre;
    fillCode(this.doc, code, next, pre.getAttribute('data-lang') || '');
    const a = pointAt([pre], Math.max(lineStart, start + delta0));
    const b = pointAt([pre], Math.max(lineStart, end + total));
    if (a && b) {
      const nr = this.doc.createRange();
      nr.setStart(a.node, a.offset);
      nr.setEnd(b.node, b.offset);
      this.selectRange(nr);
    }
  }

  async copyCode(pre, btn) {
    const text = this.codeTextOf(pre).replace(/\n$/, '');
    let ok = false;
    try {
      await this.doc.defaultView.navigator.clipboard.writeText(text);
      ok = true;
    } catch {
      // Clipboard API unavailable (e.g. plain-HTTP origin): copy via a temporary selection.
      const saved = this.getRange();
      const r = this.doc.createRange();
      r.selectNodeContents(pre);
      this.selectRange(r);
      try { ok = this.doc.execCommand('copy'); } catch { ok = false; }
      if (saved) this.selectRange(saved);
    }
    btn.classList.toggle('is-copied', ok);
    btn.setAttribute('title', ok ? 'Copied' : 'Copy failed');
    setTimeout(() => { btn.classList.remove('is-copied'); btn.setAttribute('title', 'Copy code'); }, 1500);
  }

  // ------------------------------------------------------------------ tables

  moveCell(cell, dir, addIfNeeded) {
    const table = this.closestTag(cell, 'TABLE');
    if (!table) return;
    const rows = [...table.querySelectorAll('tr')];
    const row = cell.parentNode;
    const ri = rows.indexOf(row);
    const ci = [...row.children].indexOf(cell);
    let target = null;
    if (dir === 'down') {
      if (ri + 1 < rows.length) target = rows[ri + 1].children[Math.min(ci, rows[ri + 1].children.length - 1)];
      else if (addIfNeeded) { this.mutate(() => { const tr = this.addRow(table, row); this.caretToStart(tr.children[ci] || tr.firstChild); }); return; }
    } else {
      const cells = [...table.querySelectorAll('th,td')];
      const i = cells.indexOf(cell) + dir;
      if (i >= 0 && i < cells.length) target = cells[i];
      else if (dir > 0 && addIfNeeded) { this.mutate(() => { const tr = this.addRow(table, row); this.caretToStart(tr.firstChild); }); return; }
    }
    if (target) {
      const nr = this.doc.createRange();
      nr.selectNodeContents(target);
      if (hasInlineContent(target)) this.selectRange(nr);
      else this.caretToStart(target);
    }
  }

  addRow(table, afterRow) {
    const d = this.doc;
    const width = Math.max(...[...table.querySelectorAll('tr')].map((tr) => tr.children.length));
    const tr = d.createElement('tr');
    const head = table.querySelector('tr');
    for (let i = 0; i < width; i++) {
      const td = d.createElement('td');
      const a = head && head.children[i] && head.children[i].getAttribute('data-align');
      if (a) td.setAttribute('data-align', a);
      td.appendChild(d.createElement('br'));
      tr.appendChild(td);
    }
    let tbody = table.querySelector('tbody');
    if (!tbody) { tbody = d.createElement('tbody'); table.appendChild(tbody); }
    if (afterRow && afterRow.parentNode === tbody) tbody.insertBefore(tr, afterRow.nextSibling);
    else tbody.insertBefore(tr, tbody.firstChild);
    return tr;
  }

  tableAction(act) {
    const r = this.getRange() || this.savedRange;
    const cell = r && this.closestTag(r.startContainer, 'TD', 'TH');
    const table = cell && this.closestTag(cell, 'TABLE');
    if (!table) return;
    const row = cell.parentNode;
    const ci = [...row.children].indexOf(cell);
    const rows = [...table.querySelectorAll('tr')];
    const d = this.doc;
    this.mutate(() => {
      switch (act) {
        case 'rowAdd': { const tr = this.addRow(table, row); this.caretToStart(tr.children[ci] || tr.firstChild); break; }
        case 'colAdd':
          for (const tr of rows) {
            const c = d.createElement(tr.parentNode.nodeName === 'THEAD' ? 'th' : 'td');
            c.appendChild(d.createElement('br'));
            tr.insertBefore(c, tr.children[ci + 1] || null);
          }
          this.caretToStart(row.children[ci + 1]);
          break;
        case 'rowDel':
          if (row.parentNode.nodeName === 'THEAD') {
            // The header row is required: promote the first body row, or drop the table.
            const next = table.querySelector('tbody tr');
            if (!next) { this.deleteTable(table); break; }
            for (const td of [...next.children]) {
              const th = d.createElement('th');
              for (const a of td.attributes) th.setAttribute(a.name, a.value);
              while (td.firstChild) th.appendChild(td.firstChild);
              next.replaceChild(th, td);
            }
            row.parentNode.replaceChild(next, row);
            this.caretToStart(next.children[Math.min(ci, next.children.length - 1)]);
          } else {
            const target = row.nextElementSibling || row.previousElementSibling || table.querySelector('thead tr');
            row.remove();
            this.caretToStart(target.children[Math.min(ci, target.children.length - 1)]);
          }
          break;
        case 'colDel':
          if (row.children.length <= 1) { this.deleteTable(table); break; }
          for (const tr of rows) if (tr.children[ci]) tr.children[ci].remove();
          this.caretToStart(row.children[Math.min(ci, row.children.length - 1)]);
          break;
        case 'align': {
          const order = [null, 'left', 'center', 'right'];
          const cur = (rows[0].children[ci] && rows[0].children[ci].getAttribute('data-align')) || null;
          const next = order[(order.indexOf(cur) + 1) % order.length];
          for (const tr of rows) {
            const c = tr.children[ci];
            if (!c) continue;
            if (next) c.setAttribute('data-align', next); else c.removeAttribute('data-align');
          }
          break;
        }
        case 'tableDel':
          this.deleteTable(table);
          break;
        default:
          break;
      }
    });
  }

  deleteTable(table) {
    const p = this.doc.createElement('p');
    p.appendChild(this.doc.createElement('br'));
    table.parentNode.replaceChild(p, table);
    this.setCaret(p, 0);
  }

  // ------------------------------------------------------------------ checklists

  onMouseDown(e) {
    if (this.readOnly || e.button !== 0) return;
    const li = e.target && e.target.nodeType === ELEMENT_NODE && e.target.nodeName === 'LI' && e.target.hasAttribute('data-checked') ? e.target : null;
    if (!li || !this.surface.contains(li)) return;
    const rect = li.getBoundingClientRect();
    if (e.clientX < rect.left) {
      e.preventDefault();
      this.toggleCheck(li);
    }
  }

  toggleCheck(li) {
    this.mutate(() => li.setAttribute('data-checked', li.getAttribute('data-checked') === 'true' ? 'false' : 'true'));
  }

  onClick(e) {
    const a = e.target && e.target.closest ? e.target.closest('a') : null;
    if (!a || !this.surface.contains(a)) return;
    const mod = isMac() ? e.metaKey : e.ctrlKey;
    if (mod || this.readOnly) {
      e.preventDefault();
      const href = a.getAttribute('href');
      if (href && safeLinkHref(href) !== null) this.doc.defaultView.open(href, '_blank', 'noopener,noreferrer');
    }
  }

  // ------------------------------------------------------------------ clipboard & drop

  onPaste(e) {
    if (this.readOnly) return;
    const dt = e.clipboardData;
    if (!dt) return;
    e.preventDefault();
    const plainOnly = this.plainPaste;
    this.plainPaste = false;
    const files = Array.from(dt.files || []).filter((f) => /^image\//.test(f.type));
    const html = dt.getData('text/html');
    const plain = dt.getData('text/plain');
    if (files.length && (!html || !plain)) { this.insertImageFiles(files); return; }
    this.insertClipboard({ html: plainOnly ? '' : html, plain, vscode: dt.getData('vscode-editor-data'), plainOnly });
  }

  onDrop(e) {
    if (this.readOnly) return;
    const dt = e.dataTransfer;
    if (!dt) return;
    e.preventDefault();
    const d = this.doc;
    let range = null;
    if (d.caretRangeFromPoint) range = d.caretRangeFromPoint(e.clientX, e.clientY);
    else if (d.caretPositionFromPoint) {
      const p = d.caretPositionFromPoint(e.clientX, e.clientY);
      if (p) { range = d.createRange(); range.setStart(p.offsetNode, p.offset); range.collapse(true); }
    }
    if (range && this.surface.contains(range.startContainer)) this.selectRange(range);
    const files = Array.from(dt.files || []).filter((f) => /^image\//.test(f.type));
    if (files.length) { this.insertImageFiles(files); return; }
    this.insertClipboard({ html: dt.getData('text/html'), plain: dt.getData('text/plain'), vscode: '', plainOnly: false });
  }

  insertClipboard({ html, plain, vscode, plainOnly }) {
    const r = this.getRange();
    if (!r) return;
    const pre = this.closestTag(r.startContainer, 'PRE');
    const cell = this.closestTag(r.startContainer, 'TD', 'TH');
    const text = (plain || '').replace(/\r\n?/g, '\n');
    if (pre) {
      this.mutate(() => {
        const start = this.preOffset(pre, r);
        const end = r.collapsed ? start : positionOf([pre], r.endContainer, r.endOffset);
        this.replaceInPre(pre, start, Math.max(start, end), text);
      });
      return;
    }
    if (cell) { this.mutate(() => this.insertPlainInline(text.replace(/\s*\n\s*/g, ' '))); return; }
    let md;
    if (vscode && text && !plainOnly) {
      // Copied from VS Code: keep it verbatim as a code block in the editor's language.
      let lang = '';
      try { lang = sanitizeLang(JSON.parse(vscode).mode); } catch { lang = ''; }
      md = text.includes('\n') ? fencedCode(text.replace(/\n+$/, ''), resolveLanguage(lang) ? lang : '') : '`' + text.replace(/`/g, '') + '`';
    } else if (html && !plainOnly) {
      md = htmlToMarkdown(html, this.doc);
      if (!md.trim() && text) md = plainTextToMarkdown(text);
    } else {
      md = plainOnly ? escapeMarkdown(text) : plainTextToMarkdown(text);
    }
    if (!md.trim()) return;
    this.mutate(() => this.insertMarkdown(md));
  }

  insertPlainInline(text) {
    const r = this.getRange();
    if (!r) return;
    r.deleteContents();
    const t = this.doc.createTextNode(text);
    r.insertNode(t);
    this.setCaret(t, t.nodeValue.length);
  }

  /** Inserts Markdown at the selection: a single paragraph goes inline, anything else as blocks. */
  insertMarkdown(md) {
    let r = this.getRange();
    if (!r) { this.caretToEnd(this.surface.lastChild); r = this.getRange(); if (!r) return; }
    const frag = markdownToEditorDOM(md, this.doc, { resolveImage: this.o.resolveImage, trailingParagraph: false });
    const blocks = [...frag.childNodes];
    if (!blocks.length) return;
    if (!r.collapsed) { r.deleteContents(); r = this.getRange(); }
    const block = this.closestBlock(r.startContainer);
    if (blocks.length === 1 && blocks[0].nodeName === 'P') {
      const inline = this.doc.createDocumentFragment();
      const p = blocks[0];
      const last = p.lastChild;
      while (p.firstChild) inline.appendChild(p.firstChild);
      if (last && last.nodeName === 'BR' && inline.childNodes.length === 1) return;
      r.insertNode(inline);
      if (last) {
        const after = last.nodeType === TEXT_NODE ? last : null;
        if (after) this.setCaret(after, after.nodeValue.length);
        else this.setCaret(last.parentNode, indexOf(last) + 1);
      }
      return;
    }
    if (!block || block.nodeName === 'TD' || block.nodeName === 'TH') {
      this.insertPlainInline(domToMarkdown(blocks));
      return;
    }
    if (block.nodeName === 'LI' && blocks.every((b) => b.nodeName === 'UL' || b.nodeName === 'OL')) {
      // Pasting list items into a list: splice them in after the current item.
      const tail = this.isInlineEmpty(block) ? null : this.splitBlock(block, r, 'LI');
      let ref = tail || block;
      const items = blocks.flatMap((l) => [...l.childNodes]);
      for (const li of items) { ref.parentNode.insertBefore(li, tail || ref.nextSibling); if (!tail) ref = li; }
      if (tail && this.isInlineEmpty(tail)) tail.remove();
      if (this.isInlineEmpty(block)) block.remove();
      this.caretToEnd(items[items.length - 1]);
      return;
    }
    // Split the current block at the caret and put the new blocks between the halves.
    let host = block;
    if (block.nodeName === 'LI') host = this.topBlockOf(block);
    let tail = null;
    if (host === block && block.nodeName !== 'PRE') tail = this.splitBlock(block, r, block.nodeName === 'LI' ? 'LI' : (/^H[1-6]$/.test(block.nodeName) ? 'P' : block.nodeName));
    const parent = host.parentNode;
    const ref = tail || host.nextSibling;
    for (const b of blocks) parent.insertBefore(b, ref);
    if (host === block && this.isInlineEmpty(block) && block.nodeName === 'P') block.remove();
    if (tail && this.isInlineEmpty(tail) && tail.nextSibling) tail.remove();
    const lastBlock = blocks[blocks.length - 1];
    if (lastBlock.nodeName === 'PRE') this.caretToEnd(lastBlock);
    else if (/^(UL|OL)$/.test(lastBlock.nodeName)) this.caretToEnd(lastBlock.lastChild);
    else if (lastBlock.nodeName === 'TABLE' || lastBlock.nodeName === 'HR' || lastBlock.nodeName === 'BLOCKQUOTE') {
      const nx = lastBlock.nextSibling;
      if (nx && nx.nodeName === 'P') this.caretToStart(nx);
    } else this.caretToEnd(lastBlock);
  }

  // ------------------------------------------------------------------ images

  pickImage() {
    if (typeof this.o.onUploadImage !== 'function') { this.emit('error', new Error('Image upload is not available')); return; }
    this.saveRange();
    this.fileInput.click();
  }

  insertImageFiles(files) {
    if (!files.length) return;
    if (typeof this.o.onUploadImage !== 'function') { this.emit('error', new Error('Image upload is not available')); return; }
    const win = this.doc.defaultView;
    for (const file of files) {
      if (!/^image\//.test(file.type)) continue;
      const preview = win.URL.createObjectURL(file);
      const img = this.doc.createElement('img');
      img.setAttribute('src', preview);
      img.setAttribute('alt', (file.name || 'image').replace(/\.[a-z0-9]{1,5}$/i, ''));
      img.setAttribute('data-uploading', String(++this.uploadSeq));
      img.setAttribute('class', 're-uploading');
      this.insertInlineNode(img);
      Promise.resolve()
        .then(() => this.o.onUploadImage(file))
        .then((url) => {
          const src = typeof url === 'string' ? safeImageSrc(url.trim()) : null;
          if (!src) throw new Error('Upload did not return a usable image URL');
          if (!this.surface.contains(img)) return;
          let shown = src;
          if (typeof this.o.resolveImage === 'function') {
            const rr = this.o.resolveImage(src);
            if (typeof rr === 'string' && safeImageSrc(rr) !== null) shown = rr;
          }
          img.setAttribute('data-md-src', src);
          img.removeAttribute('data-uploading');
          img.removeAttribute('class');
          img.addEventListener('load', () => win.URL.revokeObjectURL(preview), { once: true });
          img.setAttribute('src', shown);
          this.markDirty();
        })
        .catch((err) => {
          win.URL.revokeObjectURL(preview);
          if (img.parentNode) { img.remove(); this.markDirty(); }
          this.emit('error', err instanceof Error ? err : new Error(String(err)));
        });
    }
  }

  insertInlineNode(node) {
    let r = this.getRange();
    if (!r) { this.caretToEnd(this.surface.lastChild); r = this.getRange(); }
    if (!r) return;
    const pre = this.closestTag(r.startContainer, 'PRE');
    if (pre) { this.exitCodeBlock(pre); r = this.getRange(); }
    r.deleteContents();
    r.insertNode(node);
    this.setCaret(node.parentNode, indexOf(node) + 1);
    this.markDirty();
  }

  // ------------------------------------------------------------------ exec

  exec(cmd, arg) {
    if (this.readOnly && cmd !== 'selectAll') return;
    switch (cmd) {
      case 'bold': this.mutate(() => this.toggleMark('strong')); break;
      case 'italic': this.mutate(() => this.toggleMark('em')); break;
      case 'strike': this.mutate(() => this.toggleMark('del')); break;
      case 'code': this.mutate(() => this.toggleInlineCode()); break;
      case 'link':
        if (typeof arg === 'string') {
          const url = this.normalizeUrl(arg);
          if (url) this.mutate(() => this.applyLink(url));
        } else this.openLinkEditor();
        break;
      case 'unlink': this.removeLink(); break;
      case 'heading': this.mutate(() => this.setBlockType('H' + Math.min(6, Math.max(1, Number(arg) || 1)))); break;
      case 'paragraph': this.mutate(() => this.setBlockType('P')); break;
      case 'bulletList': this.mutate(() => this.toggleList('ul')); break;
      case 'orderedList': this.mutate(() => this.toggleList('ol')); break;
      case 'taskList': this.mutate(() => this.toggleList('task')); break;
      case 'quote': this.mutate(() => this.toggleQuote()); break;
      case 'codeBlock': this.mutate(() => this.toggleCodeBlock(typeof arg === 'string' ? arg : '')); break;
      case 'table': this.mutate(() => this.insertTable(arg && arg.rows, arg && arg.cols)); break;
      case 'hr': this.mutate(() => this.insertHr()); break;
      case 'image':
        if (arg && typeof arg === 'object' && 'type' in arg) this.insertImageFiles([arg]);
        else if (typeof arg === 'string' && safeImageSrc(arg)) this.mutate(() => this.insertInlineNode(makeImage(this.doc, arg, '', '', this.o.resolveImage)));
        else this.pickImage();
        break;
      case 'indent': case 'outdent': {
        const r = this.getRange();
        if (!r) break;
        const items = this.selectedBlocks(r).filter((b) => b.nodeName === 'LI');
        if (!items.length) break;
        this.mutate(() => this.keepSelection(() => {
          for (const li of items) { if (cmd === 'indent') this.indentItem(li); else this.outdentItem(li); }
        }));
        break;
      }
      case 'undo': this.undo(); break;
      case 'redo': this.redo(); break;
      case 'insertText': if (typeof arg === 'string') this.mutate(() => this.insertPlainInline(arg)); break;
      case 'insertMarkdown': if (typeof arg === 'string') this.mutate(() => this.insertMarkdown(arg)); break;
      case 'pastePlain': this.plainPaste = true; break;
      case 'selectAll': {
        const r = this.doc.createRange();
        r.selectNodeContents(this.surface);
        this.selectRange(r);
        break;
      }
      default:
        this.emit('error', new Error('Unknown editor command: ' + cmd));
    }
  }

  // ------------------------------------------------------------------ state & overlays

  scheduleState() {
    if (this.stateFrame) return;
    const win = this.doc.defaultView;
    this.stateFrame = win.requestAnimationFrame(() => { this.stateFrame = 0; this.updateState(); });
  }

  computeState() {
    const r = this.getRange();
    const s = {
      bold: false, italic: false, strike: false, code: false, link: null, block: 'p', quote: false,
      table: false, canUndo: this.history.canUndo() || this.dirty, canRedo: this.history.canRedo(), inCode: false,
    };
    if (!r) return s;
    const n = r.startContainer;
    const has = (...tags) => !!this.closestTag(n, ...tags);
    s.bold = has('STRONG', 'B');
    s.italic = has('EM', 'I');
    s.strike = has('S', 'DEL', 'STRIKE');
    s.inCode = has('PRE');
    s.code = has('CODE') && !s.inCode;
    const a = this.closestTag(n, 'A');
    s.link = a ? a.getAttribute('href') : null;
    s.quote = has('BLOCKQUOTE');
    s.table = has('TABLE');
    const b = this.closestBlock(n);
    if (b) s.block = b.nodeName === 'LI' ? this.listKindOf(b) : b.nodeName === 'PRE' ? 'code' : b.nodeName === 'DIV' ? 'p' : b.nodeName.toLowerCase();
    return s;
  }

  updateState() {
    const s = this.computeState();
    if (this.toolbar) {
      const pressed = {
        bold: s.bold, italic: s.italic, strike: s.strike, code: s.code, link: !!s.link, quote: s.quote,
        bulletList: s.block === 'ul', orderedList: s.block === 'ol', taskList: s.block === 'task', codeBlock: s.inCode,
      };
      for (const b of this.toolbar.querySelectorAll('button[data-cmd]')) {
        const cmd = b.getAttribute('data-cmd');
        if (cmd in pressed) b.setAttribute('aria-pressed', String(!!pressed[cmd]));
        if (cmd === 'undo') b.disabled = !s.canUndo;
        if (cmd === 'redo') b.disabled = !s.canRedo;
      }
      const v = s.block === 'code' ? 'code' : /^h[1-4]$/.test(s.block) ? s.block : /^h[56]$/.test(s.block) ? 'h4' : 'p';
      if (this.doc.activeElement !== this.styleSelect) this.styleSelect.value = v;
    }
    this.updateOverlays(s);
    this.emit('state', s);
  }

  updateOverlays(s) {
    const r = this.getRange();
    const focused = this.doc.activeElement === this.surface;
    this.activePre = r && s.inCode ? this.closestTag(r.startContainer, 'PRE') : null;
    this.activeTable = r && s.table ? this.closestTag(r.startContainer, 'TABLE') : null;
    this.codeTools.hidden = !this.activePre || this.readOnly;
    if (this.activePre) {
      const lang = this.activePre.getAttribute('data-lang') || '';
      const id = resolveLanguage(lang) || '';
      if (lang && !id && ![...this.langSelect.options].some((o) => o.value === lang)) {
        this.langSelect.appendChild(h(this.doc, 'option', { value: lang }, lang));
      }
      this.langSelect.value = id || lang;
    }
    this.tableTools.hidden = !this.activeTable || this.readOnly;
    if (!this.linkBubble.classList.contains('is-editing')) {
      const a = focused && s.link && r && r.collapsed ? this.closestTag(r.startContainer, 'A') : null;
      this.linkBubble.hidden = !a;
      this.activeLink = a;
      if (a) {
        const href = a.getAttribute('href') || '';
        this.linkView.textContent = href.length > 60 ? href.slice(0, 57) + '...' : href;
        if (safeLinkHref(href) !== null) this.linkView.setAttribute('href', href); else this.linkView.removeAttribute('href');
      }
    }
    this.positionOverlays();
  }

  positionOverlays() {
    if (this.activePre && !this.codeTools.hidden) {
      // The tools sit in the block's label strip (its top padding) so they never cover the code
      // being typed on the first line.
      const strip = parseFloat(this.doc.defaultView?.getComputedStyle(this.activePre).paddingTop) || 0;
      this.place(this.codeTools, this.activePre.getBoundingClientRect(), 'header', { strip });
    }
    if (this.activeTable && !this.tableTools.hidden) this.place(this.tableTools, this.activeTable.getBoundingClientRect(), 'beside');
    if (!this.linkBubble.hidden) {
      const a = this.activeLink && this.surface.contains(this.activeLink) ? this.activeLink : null;
      if (a) this.place(this.linkBubble, a.getBoundingClientRect(), 'below');
      else if (this.linkBubble.classList.contains('is-editing') && this.savedRange) this.place(this.linkBubble, this.savedRange.getBoundingClientRect(), 'below');
    }
  }

  /** Positions an overlay (CSSOM only; no inline style attributes in markup). */
  place(el, rect, where, opts) {
    if (!rect) return;
    const pos = popupPosition(where, rect, this.scroller.getBoundingClientRect(),
      { top: this.scroller.scrollTop, left: this.scroller.scrollLeft, width: this.scroller.clientWidth },
      { width: el.offsetWidth || 200, height: el.offsetHeight || 36 }, opts);
    el.style.top = `${Math.round(pos.top)}px`;
    el.style.left = `${Math.round(pos.left)}px`;
  }

  // ------------------------------------------------------------------ misc

  focus() {
    this.surface.focus({ preventScroll: true });
    if (!this.getRange()) {
      const target = lastEditable(this.surface.lastChild);
      if (target) this.caretToEnd(target);
      else this.setCaret(this.surface, this.surface.childNodes.length);
    }
  }

  setReadOnly(ro) {
    this.readOnly = !!ro;
    this.surface.setAttribute('contenteditable', this.readOnly ? 'false' : 'true');
    this.root.classList.toggle('is-readonly', this.readOnly);
    this.updateState();
  }

  destroy() {
    this.commit();
    clearTimeout(this.timer);
    for (const t of this.hlTimers.values()) clearTimeout(t);
    this.hlTimers.clear();
    if (this.stateFrame) this.doc.defaultView.cancelAnimationFrame(this.stateFrame);
    for (const c of this.cleanups) c();
    this.cleanups = [];
    if (this.observer) this.observer.disconnect();
    this.listeners.clear();
    this.root.remove();
  }
}

// --- DOM utilities used by the editor ---

function firstTextIn(n) {
  if (n.nodeType === TEXT_NODE) return n;
  for (let c = n.firstChild; c; c = c.nextSibling) {
    if (c.nodeType === ELEMENT_NODE && (LI_BLOCKS.has(c.nodeName) || c.nodeName === 'BR' || c.nodeName === 'IMG')) return null;
    const t = firstTextIn(c);
    if (t) return t;
  }
  return null;
}

function lastTextIn(n) {
  if (n.nodeType === TEXT_NODE) return n;
  for (let c = n.lastChild; c; c = c.previousSibling) {
    const t = lastTextIn(c);
    if (t) return t;
  }
  return null;
}

function unwrap(el) {
  const p = el.parentNode;
  while (el.firstChild) p.insertBefore(el.firstChild, el);
  p.removeChild(el);
}

function pruneEmptyInlines(root) {
  for (const c of [...root.childNodes]) {
    if (c.nodeType !== ELEMENT_NODE) continue;
    if (c.nodeName === 'BR' || c.nodeName === 'IMG' || LI_BLOCKS.has(c.nodeName)) continue;
    pruneEmptyInlines(c);
    if (!c.firstChild || (!hasInlineContent(c) && !c.querySelector('br,img'))) c.remove();
  }
  for (const c of [...root.childNodes]) if (c.nodeType === TEXT_NODE && c.nodeValue === '') c.remove();
}

function ensurePlaceholder(doc, el) {
  let stop = null;
  if (el.nodeName === 'LI') for (const c of el.childNodes) if (c.nodeType === ELEMENT_NODE && LI_BLOCKS.has(c.nodeName)) { stop = c; break; }
  let content = false, hasBr = false;
  for (let c = el.firstChild; c && c !== stop; c = c.nextSibling) {
    if (c.nodeType === TEXT_NODE && (c.nodeValue || '').length) content = true;
    else if (c.nodeType === ELEMENT_NODE) {
      if (c.nodeName === 'BR') hasBr = true;
      else content = true;
    }
  }
  if (!content && !hasBr) el.insertBefore(doc.createElement('br'), stop);
}

function blockPlainText(b) {
  return codeText(b).split(ZWSP).join('').split(NBSP).join(' ').replace(/\n+$/, '');
}

/** The element where the caret should go to "continue at the end" of a top-level block. */
function lastEditable(el) {
  if (!el || el.nodeType !== ELEMENT_NODE) return null;
  if (TEXT_BLOCKS.has(el.nodeName) && el.nodeName !== 'LI') return el;
  if (el.nodeName === 'UL' || el.nodeName === 'OL') {
    const li = el.lastElementChild;
    if (!li) return null;
    const nested = li.lastElementChild;
    return nested && (nested.nodeName === 'UL' || nested.nodeName === 'OL') ? lastEditable(nested) || li : li;
  }
  if (el.nodeName === 'TABLE') { const cells = el.querySelectorAll('th,td'); return cells[cells.length - 1] || null; }
  if (el.nodeName === 'BLOCKQUOTE') return lastEditable(el.lastElementChild);
  return null;
}
