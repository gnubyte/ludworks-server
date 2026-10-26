/**
 * markdown.js — safe Markdown for Ludworks notes (CommonMark block/inline algorithm + GFM tables,
 * task lists, strikethrough and autolinks). Dependency-free ES2022 module.
 *
 *   parse(md)                      → AST (plain objects, see "AST shape" below)
 *   renderToDOM(astOrMd, doc, opts)→ DocumentFragment (DOM nodes only, never innerHTML)
 *   renderToHTML(astOrMd, opts)    → escaped HTML string (exports/tests)
 *   toPlainText(md, maxLen)        → one-line snippet
 *   extractTitle(md)               → first non-empty line as plain text
 *   toggleTask(md, line, checked)  → md with that task's checkbox rewritten
 *
 * Security model: raw HTML is never interpreted (it renders as literal text). Links survive only
 * for http(s)/mailto/relative URLs, images only for relative/https/blob URLs; everything else is
 * rendered as plain text. All attribute values are set via setAttribute (DOM) or escaped (HTML).
 *
 * AST shape (0-based `line` = source line index in the \n-split input):
 *   root{children}  paragraph{line,children}  heading{line,depth,children}  thematicBreak{line}
 *   blockquote{line,children}  list{line,ordered,start,tight,marker,children:listItem[]}
 *   listItem{line,checked:null|bool,taskLine?,children}  code{line,lang,info,value,fenced}
 *   table{line,align:(null|'left'|'center'|'right')[],children:tableRow[]}
 *   tableRow{line,header,children:tableCell[]}  tableCell{children}
 *   inline: text{value} softbreak break emphasis{children} strong{children} delete{children}
 *           inlineCode{value} link{url,title,autolink?,children} image{url,title,alt}
 *
 * @module lib/markdown
 */
import { tokenize, resolveLanguage, languageLabel, MAX_HIGHLIGHT_CHARS } from './highlight.js';

const CODE_INDENT = 4;
const MAX_BLOCK_NESTING = 48; // blockquote/list containers; deeper markers become text
const MAX_INLINE_NESTING = 64; // emphasis/link depth in the AST; deeper content is flattened

// ---------------------------------------------------------------------------------------------
// Character helpers

const reAsciiPunct = /^[!"#$%&'()*+,\-./:;<=>?@[\\\]^_`{|}~]$/;
const reUnicodePunct = /^[\p{P}\p{S}]$/u;
const reWhitespaceChar = /^\s$/;
const isSpaceOrTab = (c) => c === 32 || c === 9;
const isAsciiPunctCode = (c) => (c >= 33 && c <= 47) || (c >= 58 && c <= 64) || (c >= 91 && c <= 96) || (c >= 123 && c <= 126);
const isPunct = (ch) => reAsciiPunct.test(ch) || reUnicodePunct.test(ch);
const isUnicodeWs = (ch) => reWhitespaceChar.test(ch);

function isBlank(s) {
  for (let i = 0; i < s.length; i++) {
    const c = s.charCodeAt(i);
    if (c !== 32 && c !== 9 && c !== 10 && c !== 13) return false;
  }
  return true;
}

function codePointBefore(s, i) {
  const lo = s.charCodeAt(i - 1);
  if (lo >= 0xdc00 && lo <= 0xdfff && i >= 2) {
    const hi = s.charCodeAt(i - 2);
    if (hi >= 0xd800 && hi <= 0xdbff) return s.slice(i - 2, i);
  }
  return s.charAt(i - 1);
}
function codePointAt(s, i) {
  const cp = s.codePointAt(i);
  return cp === undefined ? '' : String.fromCodePoint(cp);
}

// ---------------------------------------------------------------------------------------------
// Entities (curated set: HTML 4 + common HTML 5 names; unknown names stay literal)

const ENTITIES = (() => {
  const map = Object.create(null);
  const latin1 = 'nbsp iexcl cent pound curren yen brvbar sect uml copy ordf laquo not shy reg macr deg plusmn sup2 sup3 acute micro para middot cedil sup1 ordm raquo frac14 frac12 frac34 iquest Agrave Aacute Acirc Atilde Auml Aring AElig Ccedil Egrave Eacute Ecirc Euml Igrave Iacute Icirc Iuml ETH Ntilde Ograve Oacute Ocirc Otilde Ouml times Oslash Ugrave Uacute Ucirc Uuml Yacute THORN szlig agrave aacute acirc atilde auml aring aelig ccedil egrave eacute ecirc euml igrave iacute icirc iuml eth ntilde ograve oacute ocirc otilde ouml divide oslash ugrave uacute ucirc uuml yacute thorn yuml'.split(' ');
  latin1.forEach((n, i) => { map[n] = String.fromCharCode(160 + i); });
  const greekU = 'Alpha Beta Gamma Delta Epsilon Zeta Eta Theta Iota Kappa Lambda Mu Nu Xi Omicron Pi Rho - Sigma Tau Upsilon Phi Chi Psi Omega'.split(' ');
  greekU.forEach((n, i) => { if (n !== '-') map[n] = String.fromCharCode(913 + i); });
  const greekL = 'alpha beta gamma delta epsilon zeta eta theta iota kappa lambda mu nu xi omicron pi rho sigmaf sigma tau upsilon phi chi psi omega'.split(' ');
  greekL.forEach((n, i) => { map[n] = String.fromCharCode(945 + i); });
  const misc = {
    quot: 34, amp: 38, apos: 39, lt: 60, gt: 62, Tab: 9, NewLine: 10, excl: 33, num: 35, dollar: 36, percnt: 37,
    lpar: 40, rpar: 41, ast: 42, plus: 43, comma: 44, period: 46, sol: 47, colon: 58, semi: 59, equals: 61,
    quest: 63, commat: 64, lsqb: 91, bsol: 92, rsqb: 93, Hat: 94, lowbar: 95, grave: 96, lcub: 123, verbar: 124,
    vert: 124, rcub: 125, OElig: 338, oelig: 339, Scaron: 352, scaron: 353, Yuml: 376, fnof: 402, circ: 710,
    tilde: 732, thetasym: 977, upsih: 978, piv: 982, ensp: 8194, emsp: 8195, thinsp: 8201, zwnj: 8204,
    zwj: 8205, lrm: 8206, rlm: 8207, ndash: 8211, mdash: 8212, lsquo: 8216, rsquo: 8217, sbquo: 8218,
    ldquo: 8220, rdquo: 8221, bdquo: 8222, dagger: 8224, Dagger: 8225, bull: 8226, hellip: 8230, permil: 8240,
    prime: 8242, Prime: 8243, lsaquo: 8249, rsaquo: 8250, oline: 8254, frasl: 8260, euro: 8364, image: 8465,
    weierp: 8472, real: 8476, trade: 8482, alefsym: 8501, larr: 8592, uarr: 8593, rarr: 8594, darr: 8595,
    harr: 8596, crarr: 8629, lArr: 8656, uArr: 8657, rArr: 8658, dArr: 8659, hArr: 8660, forall: 8704,
    part: 8706, exist: 8707, empty: 8709, nabla: 8711, isin: 8712, notin: 8713, ni: 8715, prod: 8719,
    sum: 8721, minus: 8722, lowast: 8727, radic: 8730, prop: 8733, infin: 8734, ang: 8736, and: 8743,
    or: 8744, cap: 8745, cup: 8746, int: 8747, there4: 8756, sim: 8764, cong: 8773, asymp: 8776, ne: 8800,
    equiv: 8801, le: 8804, ge: 8805, sub: 8834, sup: 8835, nsub: 8836, sube: 8838, supe: 8839, oplus: 8853,
    otimes: 8855, perp: 8869, sdot: 8901, lceil: 8968, rceil: 8969, lfloor: 8970, rfloor: 8971,
    lang: 10216, rang: 10217, loz: 9674, spades: 9824, clubs: 9827, hearts: 9829, diams: 9830,
    check: 10003, cross: 10007, star: 9734, starf: 9733,
  };
  for (const k in misc) map[k] = String.fromCodePoint(misc[k]);
  return map;
})();

function decodeCodePoint(cp) {
  if (cp === 0 || cp > 0x10ffff || (cp >= 0xd800 && cp <= 0xdfff)) return '�';
  return String.fromCodePoint(cp);
}

const reEntityHere = /&(?:#[xX]([0-9a-fA-F]{1,6})|#([0-9]{1,7})|([A-Za-z][A-Za-z0-9]{1,31}));/y;
const reEscOrEntity = /\\([!"#$%&'()*+,\-./:;<=>?@[\\\]^_`{|}~])|&(?:#[xX]([0-9a-fA-F]{1,6})|#([0-9]{1,7})|([A-Za-z][A-Za-z0-9]{1,31}));/g;

function decodeEntityParts(whole, hex, dec, name) {
  if (hex) return decodeCodePoint(parseInt(hex, 16));
  if (dec) return decodeCodePoint(parseInt(dec, 10));
  const v = ENTITIES[name];
  return v === undefined ? whole : v;
}

/** Resolves backslash escapes and entities (link destinations, titles, info strings). */
function unescapeString(s) {
  if (s.indexOf('\\') === -1 && s.indexOf('&') === -1) return s;
  return s.replace(reEscOrEntity, (m, esc, hex, dec, name) => (esc ? esc : decodeEntityParts(m, hex, dec, name)));
}

// ---------------------------------------------------------------------------------------------
// URLs

const URI_SAFE = /[A-Za-z0-9;/?:@&=+$,\-_.!~*'()#]/;
const reHex2 = /^[0-9a-fA-F]{2}$/;

/** Percent-encodes a link destination the way CommonMark reference renderers do. */
function normalizeURI(uri) {
  let out = '';
  for (let i = 0; i < uri.length; i++) {
    const ch = uri[i];
    if (ch === '%' && reHex2.test(uri.slice(i + 1, i + 3))) { out += uri.slice(i, i + 3); i += 2; continue; }
    if (URI_SAFE.test(ch)) { out += ch; continue; }
    const cc = uri.charCodeAt(i);
    if (cc >= 0xd800 && cc <= 0xdbff && i + 1 < uri.length) {
      const lo = uri.charCodeAt(i + 1);
      if (lo >= 0xdc00 && lo <= 0xdfff) { out += encodeURIComponent(uri.slice(i, i + 2)); i++; continue; }
    }
    out += cc >= 0xd800 && cc <= 0xdfff ? '%EF%BF%BD' : encodeURIComponent(ch);
  }
  return out;
}

/**
 * Classifies a URL for the allow-lists. Returns the lower-case scheme, '' for relative URLs,
 * '//' for protocol-relative (incl. backslash variants browsers normalise) and '?' for
 * anything ambiguous. Checks the NFKC form too so full-width look-alikes cannot sneak a scheme in.
 */
// C0/C1 controls, space, soft hyphen, zero-width chars, line/paragraph separators, word joiner, BOM.
// Built from code points (not regex escapes) so the source stays plain ASCII.
function uEsc(cp) { return String.fromCharCode(92) + 'u' + cp.toString(16).padStart(4, '0'); }
const URL_IGNORABLE = new RegExp(
  '[' + [[0x00, 0x20], [0x7f, 0x9f], [0xad, 0xad], [0x200b, 0x200f], [0x2028, 0x2029], [0x2060, 0x2060], [0xfeff, 0xfeff]]
    .map(([a, b]) => uEsc(a) + '-' + uEsc(b)).join('') + ']', 'g');

function kindOf(s) {
  const m = /^([A-Za-z][A-Za-z0-9+.-]*):/.exec(s);
  if (m) return m[1].toLowerCase();
  if (/^[\\/]{2}/.test(s)) return '//';
  const colon = s.indexOf(':');
  const delim = s.search(/[/?#\\]/);
  return colon !== -1 && (delim === -1 || colon < delim) ? '?' : '';
}
function urlKind(url) {
  // Browsers drop these characters while parsing URLs, so "java\tscript:" must be judged as "javascript:".
  const raw = String(url).replace(URL_IGNORABLE, '');
  const k1 = kindOf(raw);
  const nf = raw.normalize('NFKC');
  if (nf === raw) return k1;
  const k2 = kindOf(nf);
  if (k1 === k2 || k2 === '') return k1;
  return k1 === '' ? k2 : '?';
}

/**
 * @param {string} url
 * @returns {string|null} the URL if it is safe as a link target (http, https, mailto, relative), else null.
 */
export function safeLinkHref(url) {
  if (typeof url !== 'string') return null;
  const k = urlKind(url);
  return k === '' || k === '//' || k === 'http' || k === 'https' || k === 'mailto' ? url : null;
}

/**
 * @param {string} url
 * @returns {string|null} the URL if it is safe as an image source (relative, https, blob), else null.
 */
export function safeImageSrc(url) {
  if (typeof url !== 'string' || url === '') return null;
  const k = urlKind(url);
  if (k === 'https' || k === 'blob') return url;
  if (k === '' && !/^[\\/][\\/]/.test(url)) return url;
  return null;
}

/** True for URLs that leave the app (get target=_blank / rel=noopener). */
function isAbsoluteHttp(url) {
  const k = urlKind(url);
  return k === 'http' || k === 'https' || k === '//';
}

// ---------------------------------------------------------------------------------------------
// Block parser (port of the CommonMark reference algorithm, trimmed to what notes need)

function newBlock(type, line, col) {
  return { type, line, col, endLine: line, parent: null, children: [], open: true, lines: null, depth: 0 };
}

const canContain = (parent, child) =>
  parent === 'document' || parent === 'blockquote' || parent === 'item' ? child !== 'item' : parent === 'list' ? child === 'item' : false;

const reMaybeSpecial = /^[#`~*+_=>0-9|:-]/;
const reClosingFence = /^(?:`{3,}|~{3,})(?=[ \t]*$)/;
const reSetext = /^(?:=+|-+)[ \t]*$/;
const reHr = /^(?:(?:\*[ \t]*){3,}|(?:_[ \t]*){3,}|(?:-[ \t]*){3,})$/;
const reBulletMarker = /^[*+-]/;
const reOrderedMarker = /^(\d{1,9})([.)])/;
const reTableDelim = /^\|?[ \t]*:?-+:?[ \t]*(?:\|[ \t]*:?-+:?[ \t]*)*\|?[ \t]*$/;

function splitTableRow(s) {
  s = s.trim();
  let start = s.charCodeAt(0) === 124 ? 1 : 0;
  let end = s.length;
  if (end > start && s.charCodeAt(end - 1) === 124 && s.charCodeAt(end - 2) !== 92) end--;
  const cells = [];
  let cellStart = start;
  for (let i = start; i < end; i++) {
    const c = s.charCodeAt(i);
    if (c === 92) { i++; continue; } // keep escapes intact; skip the escaped char
    if (c === 124) { cells.push(s.slice(cellStart, i)); cellStart = i + 1; }
  }
  cells.push(s.slice(cellStart, end));
  // GFM: "\|" inside a cell is a literal pipe even inside code spans.
  return cells.map((x) => x.trim().replaceAll('\\|', '|'));
}

function cellAlign(c) {
  c = c.trim();
  const l = c.startsWith(':'), r = c.endsWith(':');
  return l && r ? 'center' : r ? 'right' : l ? 'left' : null;
}

function stripClosingSequence(s) {
  s = s.trim();
  if (!s.endsWith('#')) return s;
  let j = s.length;
  while (j > 0 && s.charCodeAt(j - 1) === 35) j--;
  if (j === 0) return '';
  const c = s.charCodeAt(j - 1);
  return c === 32 || c === 9 ? s.slice(0, j).trimEnd() : s;
}

class BlockParser {
  constructor() {
    this.doc = newBlock('document', 0, 0);
    this.tip = this.doc;
    this.oldtip = this.doc;
    this.refmap = new Map();
    this.currentLine = '';
    this.lineNumber = 0;
    this.offset = 0;
    this.column = 0;
    this.nextNonspace = 0;
    this.nextNonspaceColumn = 0;
    this.indent = 0;
    this.indented = false;
    this.blank = false;
    this.partiallyConsumedTab = false;
    this.allClosed = true;
    this.lastMatchedContainer = this.doc;
  }

  parse(src) {
    const lines = src.split('\n');
    if (lines.length && lines[lines.length - 1] === '') lines.pop();
    for (let i = 0; i < lines.length; i++) {
      this.lineNumber = i;
      this.incorporateLine(lines[i]);
    }
    const last = Math.max(0, lines.length - 1);
    while (this.tip) this.finalize(this.tip, last);
    return this.doc;
  }

  findNextNonspace() {
    const ln = this.currentLine;
    let i = this.offset, cols = this.column, c;
    while (i < ln.length) {
      c = ln.charCodeAt(i);
      if (c === 32) { i++; cols++; } else if (c === 9) { i++; cols += 4 - (cols % 4); } else break;
    }
    this.blank = i >= ln.length;
    this.nextNonspace = i;
    this.nextNonspaceColumn = cols;
    this.indent = cols - this.column;
    this.indented = this.indent >= CODE_INDENT;
  }

  advanceNextNonspace() {
    this.offset = this.nextNonspace;
    this.column = this.nextNonspaceColumn;
    this.partiallyConsumedTab = false;
  }

  advanceOffset(count, columns) {
    const ln = this.currentLine;
    while (count > 0 && this.offset < ln.length) {
      if (ln.charCodeAt(this.offset) === 9) {
        const charsToTab = 4 - (this.column % 4);
        if (columns) {
          this.partiallyConsumedTab = charsToTab > count;
          const adv = charsToTab > count ? count : charsToTab;
          this.column += adv;
          this.offset += this.partiallyConsumedTab ? 0 : 1;
          count -= adv;
        } else {
          this.partiallyConsumedTab = false;
          this.column += charsToTab;
          this.offset += 1;
          count -= 1;
        }
      } else {
        this.partiallyConsumedTab = false;
        this.offset += 1;
        this.column += 1;
        count -= 1;
      }
    }
  }

  addLine() {
    let s;
    if (this.partiallyConsumedTab) {
      this.offset += 1;
      s = ' '.repeat(4 - (this.column % 4)) + this.currentLine.slice(this.offset);
    } else {
      s = this.currentLine.slice(this.offset);
    }
    const tip = this.tip;
    if (tip.type === 'table') {
      if (!isBlank(s)) tip.rows.push({ line: this.lineNumber, cells: splitTableRow(s) });
      return;
    }
    (tip.lines || (tip.lines = [])).push(s);
  }

  addChild(type, col) {
    while (!canContain(this.tip.type, type)) this.finalize(this.tip, this.lineNumber - 1);
    const b = newBlock(type, this.lineNumber, col);
    b.parent = this.tip;
    b.depth = this.tip.depth + 1;
    this.tip.children.push(b);
    this.tip = b;
    return b;
  }

  closeUnmatchedBlocks() {
    if (this.allClosed) return;
    while (this.oldtip !== this.lastMatchedContainer) {
      const parent = this.oldtip.parent;
      this.finalize(this.oldtip, this.lineNumber - 1);
      this.oldtip = parent;
    }
    this.allClosed = true;
  }

  /** 0 = matched, 1 = not matched, 2 = line consumed entirely (closing fence). */
  continueBlock(c) {
    const ln = this.currentLine;
    switch (c.type) {
      case 'document':
      case 'list':
        return 0;
      case 'blockquote':
        if (!this.indented && ln.charCodeAt(this.nextNonspace) === 62) {
          this.advanceNextNonspace();
          this.advanceOffset(1, false);
          if (isSpaceOrTab(ln.charCodeAt(this.offset))) this.advanceOffset(1, true);
          return 0;
        }
        return 1;
      case 'item':
        if (this.blank) {
          if (c.children.length === 0) return 1; // an item can start with at most one blank line
          this.advanceNextNonspace();
        } else if (this.indent >= c.markerOffset + c.padding) {
          this.advanceOffset(c.markerOffset + c.padding, true);
        } else {
          return 1;
        }
        return 0;
      case 'code':
        if (c.fenced) {
          if (this.indent <= 3 && ln.charAt(this.nextNonspace) === c.fenceChar) {
            const m = reClosingFence.exec(ln.slice(this.nextNonspace));
            if (m && m[0].length >= c.fenceLength) {
              this.finalize(c, this.lineNumber);
              return 2;
            }
          }
          for (let i = c.fenceOffset; i > 0 && isSpaceOrTab(ln.charCodeAt(this.offset)); i--) this.advanceOffset(1, true);
          return 0;
        }
        if (this.indent >= CODE_INDENT) { this.advanceOffset(CODE_INDENT, true); return 0; }
        if (this.blank) { this.advanceNextNonspace(); return 0; }
        return 1;
      case 'paragraph':
      case 'table':
        return this.blank ? 1 : 0;
      default:
        return 1; // headings and thematic breaks are single-line
    }
  }

  incorporateLine(ln) {
    let container = this.doc;
    this.oldtip = this.tip;
    this.offset = 0;
    this.column = 0;
    this.blank = false;
    this.partiallyConsumedTab = false;
    this.currentLine = ln;

    // 1. Match the line against open containers.
    for (;;) {
      const last = container.children[container.children.length - 1];
      if (!last || !last.open) break;
      container = last;
      this.findNextNonspace();
      const r = this.continueBlock(container);
      if (r === 0) continue;
      if (r === 1) { container = container.parent; break; }
      return;
    }
    this.allClosed = container === this.oldtip;
    this.lastMatchedContainer = container;

    // 2. Look for new block starts.
    let matchedLeaf = container.type === 'code';
    while (!matchedLeaf) {
      this.findNextNonspace();
      if (!this.indented && !reMaybeSpecial.test(ln.charAt(this.nextNonspace))) {
        this.advanceNextNonspace();
        break;
      }
      let res = 0;
      for (const start of BLOCK_STARTS) {
        res = start(this, container);
        if (res) break;
      }
      if (res === 1) container = this.tip;
      else if (res === 2) { container = this.tip; matchedLeaf = true; }
      else { this.advanceNextNonspace(); break; }
    }

    // 3. The rest of the line is content.
    if (!this.allClosed && !this.blank && this.tip.type === 'paragraph') {
      this.addLine(); // lazy paragraph continuation
      return;
    }
    this.closeUnmatchedBlocks();
    const t = container.type;
    if (t === 'paragraph' || t === 'code' || t === 'table') {
      this.addLine();
    } else if (this.offset < ln.length && !this.blank) {
      const p = this.addChild('paragraph', this.offset);
      this.advanceNextNonspace();
      p.col = this.offset;
      this.addLine();
    }
  }

  finalize(b, endLine) {
    const above = b.parent;
    b.open = false;
    b.endLine = endLine;
    switch (b.type) {
      case 'paragraph': {
        let content = (b.lines || []).join('\n');
        let pos;
        while (content.charCodeAt(0) === 91 && (pos = parseReference(content, this.refmap))) {
          const consumed = content.slice(0, pos);
          let nl = 0;
          for (let i = 0; i < consumed.length; i++) if (consumed.charCodeAt(i) === 10) nl++;
          if (nl) { b.line += nl; b.col = 0; }
          content = content.slice(pos);
        }
        b.lines = null;
        if (isBlank(content)) above.children.splice(above.children.lastIndexOf(b), 1);
        else b.content = content;
        break;
      }
      case 'code': {
        const ls = b.lines || [];
        if (b.fenced) {
          b.info = unescapeString((ls[0] || '').trim());
          b.value = ls.slice(1).join('\n');
        } else {
          while (ls.length && isBlank(ls[ls.length - 1])) ls.pop();
          b.info = '';
          b.value = ls.join('\n');
        }
        b.lines = null;
        break;
      }
      case 'item':
        b.endLine = b.children.length ? b.children[b.children.length - 1].endLine : b.line;
        break;
      case 'list': {
        let tight = true;
        const items = b.children;
        outer: for (let i = 0; i < items.length; i++) {
          const item = items[i];
          if (i + 1 < items.length && item.endLine !== items[i + 1].line - 1) { tight = false; break; }
          const subs = item.children;
          for (let j = 0; j + 1 < subs.length; j++) {
            if (subs[j].endLine !== subs[j + 1].line - 1) { tight = false; break outer; }
          }
        }
        b.tight = tight;
        if (items.length) b.endLine = items[items.length - 1].endLine;
        break;
      }
      default:
        break;
    }
    this.tip = above;
  }
}

function parseListMarker(p, container) {
  if (p.indent >= 4) return null;
  const ln = p.currentLine;
  const rest = ln.slice(p.nextNonspace);
  const data = { type: '', bulletChar: '', start: 1, delimiter: '', padding: 0, markerOffset: p.indent };
  let m;
  if ((m = reBulletMarker.exec(rest))) {
    data.type = 'bullet';
    data.bulletChar = m[0];
  } else if ((m = reOrderedMarker.exec(rest)) && (container.type !== 'paragraph' || m[1] === '1')) {
    data.type = 'ordered';
    data.start = parseInt(m[1], 10);
    data.delimiter = m[2];
  } else {
    return null;
  }
  const nextc = ln.charCodeAt(p.nextNonspace + m[0].length);
  if (!(Number.isNaN(nextc) || nextc === 9 || nextc === 32)) return null;
  if (container.type === 'paragraph' && isBlank(ln.slice(p.nextNonspace + m[0].length))) return null;

  p.advanceNextNonspace();
  p.advanceOffset(m[0].length, true);
  const spacesStartCol = p.column, spacesStartOffset = p.offset;
  do {
    p.advanceOffset(1, true);
  } while (p.column - spacesStartCol < 5 && isSpaceOrTab(ln.charCodeAt(p.offset)));
  const blankItem = p.offset >= ln.length;
  const spacesAfterMarker = p.column - spacesStartCol;
  if (spacesAfterMarker >= 5 || spacesAfterMarker < 1 || blankItem) {
    data.padding = m[0].length + 1;
    p.column = spacesStartCol;
    p.offset = spacesStartOffset;
    if (isSpaceOrTab(ln.charCodeAt(p.offset))) p.advanceOffset(1, true);
  } else {
    data.padding = m[0].length + spacesAfterMarker;
  }
  return data;
}

const listsMatch = (a, b) => !!a && a.type === b.type && a.delimiter === b.delimiter && a.bulletChar === b.bulletChar;

/** Block starts: return 0 (no match), 1 (container started) or 2 (leaf started). */
const BLOCK_STARTS = [
  function blockquote(p, container) {
    if (p.indented || p.currentLine.charCodeAt(p.nextNonspace) !== 62 || container.depth >= MAX_BLOCK_NESTING) return 0;
    p.advanceNextNonspace();
    p.advanceOffset(1, false);
    if (isSpaceOrTab(p.currentLine.charCodeAt(p.offset))) p.advanceOffset(1, true);
    p.closeUnmatchedBlocks();
    p.addChild('blockquote', p.nextNonspace);
    return 1;
  },
  function atxHeading(p) {
    if (p.indented) return 0;
    const ln = p.currentLine, start = p.nextNonspace;
    let i = start;
    while (i < ln.length && ln.charCodeAt(i) === 35 && i - start < 7) i++;
    const level = i - start;
    if (level < 1 || level > 6) return 0;
    const after = ln.charCodeAt(i);
    if (!(Number.isNaN(after) || after === 32 || after === 9)) return 0;
    p.advanceNextNonspace();
    p.advanceOffset(level, false);
    p.closeUnmatchedBlocks();
    const h = p.addChild('heading', p.nextNonspace);
    h.level = level;
    h.content = stripClosingSequence(ln.slice(p.offset));
    p.advanceOffset(ln.length - p.offset, false);
    return 2;
  },
  function fencedCode(p) {
    if (p.indented) return 0;
    const ln = p.currentLine, ch = ln.charAt(p.nextNonspace);
    if (ch !== '`' && ch !== '~') return 0;
    let i = p.nextNonspace;
    while (ln.charAt(i) === ch) i++;
    const len = i - p.nextNonspace;
    if (len < 3 || (ch === '`' && ln.indexOf('`', i) !== -1)) return 0;
    p.closeUnmatchedBlocks();
    const c = p.addChild('code', p.nextNonspace);
    c.fenced = true;
    c.fenceLength = len;
    c.fenceChar = ch;
    c.fenceOffset = p.indent;
    p.advanceNextNonspace();
    p.advanceOffset(len, false);
    return 2;
  },
  function setextHeading(p, container) {
    if (p.indented || container.type !== 'paragraph') return 0;
    const rest = p.currentLine.slice(p.nextNonspace);
    if (!reSetext.test(rest)) return 0;
    p.closeUnmatchedBlocks();
    let content = container.lines.join('\n');
    let pos;
    while (content.charCodeAt(0) === 91 && (pos = parseReference(content, p.refmap))) content = content.slice(pos);
    if (isBlank(content)) return 0;
    const h = newBlock('heading', container.line, container.col);
    h.level = rest.charCodeAt(0) === 61 ? 1 : 2;
    h.content = content.trim();
    h.parent = container.parent;
    h.depth = container.depth;
    const siblings = container.parent.children;
    siblings[siblings.lastIndexOf(container)] = h;
    p.tip = h;
    p.advanceOffset(p.currentLine.length - p.offset, false);
    return 2;
  },
  function table(p, container) {
    if (p.indented || container.type !== 'paragraph') return 0;
    const ln = p.currentLine;
    const rest = ln.slice(p.nextNonspace);
    if (rest.indexOf('|') === -1 || !reTableDelim.test(rest)) return 0;
    const lines = container.lines;
    const header = lines[lines.length - 1];
    if (header.indexOf('|') === -1) return 0;
    const hcells = splitTableRow(header);
    const dcells = splitTableRow(rest);
    if (hcells.length !== dcells.length) return 0;
    p.closeUnmatchedBlocks();
    const parent = container.parent;
    if (lines.length > 1) {
      lines.pop();
      p.finalize(container, p.lineNumber - 2);
    } else {
      parent.children.splice(parent.children.lastIndexOf(container), 1);
      p.tip = parent;
    }
    const t = p.addChild('table', p.nextNonspace);
    t.line = p.lineNumber - 1;
    t.align = dcells.map(cellAlign);
    t.header = hcells;
    t.rows = [];
    p.advanceOffset(ln.length - p.offset, false);
    return 2;
  },
  function thematicBreak(p) {
    if (p.indented || !reHr.test(p.currentLine.slice(p.nextNonspace))) return 0;
    p.closeUnmatchedBlocks();
    p.addChild('thematicBreak', p.nextNonspace);
    p.advanceOffset(p.currentLine.length - p.offset, false);
    return 2;
  },
  function listItem(p, container) {
    if ((p.indented && container.type !== 'list') || container.depth >= MAX_BLOCK_NESTING) return 0;
    const data = parseListMarker(p, container);
    if (!data) return 0;
    p.closeUnmatchedBlocks();
    if (p.tip.type !== 'list' || !listsMatch(container.listData, data)) {
      const l = p.addChild('list', p.nextNonspace);
      l.listData = data;
    }
    const item = p.addChild('item', p.nextNonspace);
    item.listData = data;
    item.markerOffset = data.markerOffset;
    item.padding = data.padding;
    return 1;
  },
  function indentedCode(p) {
    if (!p.indented || p.blank || p.tip.type === 'paragraph' || p.tip.type === 'table') return 0;
    p.advanceOffset(CODE_INDENT, true);
    p.closeUnmatchedBlocks();
    p.addChild('code', p.offset);
    return 2;
  },
];

// ---------------------------------------------------------------------------------------------
// Inline parser (CommonMark delimiter algorithm + GFM strikethrough and extended autolinks)

function inode(type, props) {
  const n = { type, parent: null, prev: null, next: null, first: null, last: null };
  return props ? Object.assign(n, props) : n;
}
function appendChild(parent, child) {
  child.parent = parent;
  child.next = null;
  child.prev = parent.last;
  if (parent.last) parent.last.next = child;
  else parent.first = child;
  parent.last = child;
}
function unlinkNode(n) {
  if (n.prev) n.prev.next = n.next;
  else if (n.parent) n.parent.first = n.next;
  if (n.next) n.next.prev = n.prev;
  else if (n.parent) n.parent.last = n.prev;
  n.parent = n.next = n.prev = null;
}
function insertAfter(node, sib) {
  unlinkNode(sib);
  sib.next = node.next;
  if (sib.next) sib.next.prev = sib;
  sib.prev = node;
  node.next = sib;
  sib.parent = node.parent;
  if (!sib.next && sib.parent) sib.parent.last = sib;
}

function normalizeReference(label) {
  return label.slice(1, -1).trim().replace(/[ \t\r\n]+/g, ' ').toLowerCase().toUpperCase();
}

const reMainStop = /[\n\\`*_~[\]!<&]|(?<![^\s*_~(])(?:[hH][tT][tT][pP][sS]?:\/\/|[wW][wW][wW]\.)/g;
const reEmailAutolink = /<([a-zA-Z0-9.!#$%&'*+/=?^_`{|}~-]+@[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?(?:\.[a-zA-Z0-9](?:[a-zA-Z0-9-]{0,61}[a-zA-Z0-9])?)*)>/y;
const reAutolink = /<[A-Za-z][A-Za-z0-9.+-]{1,31}:[^<>\x00-\x20]*>/y;
const reExtUrl = /(?:[hH][tT][tT][pP][sS]?:\/\/|[wW][wW][wW]\.)[^\s<]*/y;
const reHost = /^[\p{L}\p{N}_-]+(?:\.[\p{L}\p{N}_-]+)*/u;

function trimAutolinkTail(t) {
  let open = 0, close = 0;
  for (let i = 0; i < t.length; i++) {
    const c = t.charCodeAt(i);
    if (c === 40) open++;
    else if (c === 41) close++;
  }
  for (;;) {
    const last = t[t.length - 1];
    if (last === undefined) return t;
    if ('?!.,:*_~\'"'.includes(last)) { t = t.slice(0, -1); continue; }
    if (last === ')' && close > open) { t = t.slice(0, -1); close--; continue; }
    if (last === ';') {
      let j = t.length - 2;
      while (j >= 0 && /[A-Za-z0-9]/.test(t[j])) j--;
      if (j >= 0 && t[j] === '&' && j < t.length - 2) { t = t.slice(0, j); continue; }
    }
    return t;
  }
}

class InlineParser {
  constructor(refmap) {
    this.refmap = refmap || new Map();
    this.subject = '';
    this.pos = 0;
  }

  // --- scanners shared with reference-definition parsing ---

  spnl() {
    const s = this.subject;
    while (isSpaceOrTab(s.charCodeAt(this.pos))) this.pos++;
    if (s.charCodeAt(this.pos) === 10) {
      this.pos++;
      while (isSpaceOrTab(s.charCodeAt(this.pos))) this.pos++;
    }
    return true;
  }

  parseLinkLabel() {
    const s = this.subject;
    if (s.charCodeAt(this.pos) !== 91) return 0;
    const limit = Math.min(s.length, this.pos + 1002);
    for (let i = this.pos + 1; i < limit; i++) {
      const c = s.charCodeAt(i);
      if (c === 92 && i + 1 < s.length) { i++; continue; }
      if (c === 91) return 0;
      if (c === 93) {
        const n = i - this.pos + 1;
        if (n - 2 > 999) return 0;
        this.pos = i + 1;
        return n;
      }
    }
    return 0;
  }

  parseLinkDestination() {
    const s = this.subject;
    if (s.charCodeAt(this.pos) === 60) {
      let i = this.pos + 1;
      while (i < s.length) {
        const c = s.charCodeAt(i);
        if (c === 92 && isAsciiPunctCode(s.charCodeAt(i + 1))) { i += 2; continue; }
        if (c === 62) break;
        if (c === 60 || c === 10) return null;
        i++;
      }
      if (s.charCodeAt(i) !== 62) return null;
      const res = s.slice(this.pos + 1, i);
      this.pos = i + 1;
      return normalizeURI(unescapeString(res));
    }
    const start = this.pos;
    let open = 0, c;
    while (this.pos < s.length) {
      c = s.charCodeAt(this.pos);
      if (c === 92 && isAsciiPunctCode(s.charCodeAt(this.pos + 1))) { this.pos += 2; continue; }
      if (c === 40) { if (++open > 32) return null; this.pos++; continue; }
      if (c === 41) { if (open < 1) break; open--; this.pos++; continue; }
      if (c <= 32 || c === 127) break;
      this.pos++;
    }
    if (this.pos === start && c !== 41) return null;
    if (open !== 0) return null;
    return normalizeURI(unescapeString(s.slice(start, this.pos)));
  }

  parseLinkTitle() {
    const s = this.subject;
    const open = s.charAt(this.pos);
    if (open !== '"' && open !== "'" && open !== '(') return null;
    const close = open === '(' ? ')' : open;
    // A failed scan to end-of-subject for this delimiter means no later scan can succeed.
    if (this.titleFail[close] !== undefined && this.pos >= this.titleFail[close]) return null;
    let i = this.pos + 1;
    for (; i < s.length; i++) {
      const ch = s[i];
      if (ch === '\\' && i + 1 < s.length && isAsciiPunctCode(s.charCodeAt(i + 1))) { i++; continue; }
      if (ch === close) break;
      if (open === '(' && ch === '(') return null;
    }
    if (i >= s.length) { this.titleFail[close] = this.pos; return null; }
    const title = s.slice(this.pos + 1, i);
    this.pos = i + 1;
    return unescapeString(title);
  }

  // --- inline parsing ---

  parse(content) {
    this.subject = content.trim();
    this.pos = 0;
    this.delimiters = null;
    this.brackets = null;
    this.backtickFail = new Map();
    this.titleFail = Object.create(null);
    const block = inode('root');
    while (this.parseInline(block));
    this.processEmphasis(null);
    return block;
  }

  text(block, s) {
    appendChild(block, inode('text', { value: s }));
  }

  parseInline(block) {
    const s = this.subject;
    const c = s.charCodeAt(this.pos);
    if (Number.isNaN(c)) return false;
    switch (c) {
      case 10: this.parseNewline(block); break;
      case 92: this.parseBackslash(block); break;
      case 96: this.parseBackticks(block); break;
      case 42: case 95: case 126: this.handleDelim(c, block); break;
      case 91: this.parseOpenBracket(block); break;
      case 33: this.parseBang(block); break;
      case 93: this.parseCloseBracket(block); break;
      case 60:
        if (!this.parseAutolink(block)) { this.text(block, '<'); this.pos++; }
        break;
      case 38: this.parseEntity(block); break;
      default:
        if ((c === 104 || c === 72 || c === 119 || c === 87) && (this.pos === 0 || /[\s*_~(]/.test(s[this.pos - 1])) && this.parseExtAutolink(block)) break;
        this.parseString(block);
    }
    return true;
  }

  parseString(block) {
    reMainStop.lastIndex = this.pos + 1;
    const m = reMainStop.exec(this.subject);
    const end = m ? m.index : this.subject.length;
    this.text(block, this.subject.slice(this.pos, end));
    this.pos = end;
  }

  parseNewline(block) {
    this.pos++;
    const last = block.last;
    if (last && last.type === 'text' && last.value.charCodeAt(last.value.length - 1) === 32) {
      const v = last.value;
      let e = v.length;
      while (e > 0 && v.charCodeAt(e - 1) === 32) e--;
      const hard = v.length - e >= 2;
      last.value = v.slice(0, e);
      appendChild(block, inode(hard ? 'break' : 'softbreak'));
    } else {
      appendChild(block, inode('softbreak'));
    }
    while (this.subject.charCodeAt(this.pos) === 32) this.pos++;
  }

  parseBackslash(block) {
    const s = this.subject;
    this.pos++;
    const c = s.charCodeAt(this.pos);
    if (c === 10) {
      this.pos++;
      appendChild(block, inode('break'));
      while (s.charCodeAt(this.pos) === 32) this.pos++;
    } else if (isAsciiPunctCode(c)) {
      this.text(block, s[this.pos]);
      this.pos++;
    } else {
      this.text(block, '\\');
    }
  }

  parseBackticks(block) {
    const s = this.subject, start = this.pos;
    let i = start;
    while (s.charCodeAt(i) === 96) i++;
    const n = i - start, after = i;
    const memo = this.backtickFail.get(n);
    if (memo === undefined || after < memo) {
      let j = after;
      while ((j = s.indexOf('`', j)) !== -1) {
        let k = j;
        while (s.charCodeAt(k) === 96) k++;
        if (k - j === n) {
          let content = s.slice(after, j).replace(/\n/g, ' ');
          if (content.length >= 2 && content.charCodeAt(0) === 32 && content.charCodeAt(content.length - 1) === 32 && /[^ ]/.test(content)) {
            content = content.slice(1, -1);
          }
          appendChild(block, inode('code', { value: content }));
          this.pos = k;
          return;
        }
        j = k;
      }
      this.backtickFail.set(n, after);
    }
    this.text(block, s.slice(start, after));
    this.pos = after;
  }

  handleDelim(cc, block) {
    const s = this.subject, start = this.pos;
    let i = start;
    while (s.charCodeAt(i) === cc) i++;
    const numdelims = i - start;
    const before = start === 0 ? '\n' : codePointBefore(s, start);
    const after = i >= s.length ? '\n' : codePointAt(s, i);
    const afterWs = isUnicodeWs(after), afterPunct = isPunct(after);
    const beforeWs = isUnicodeWs(before), beforePunct = isPunct(before);
    const left = !afterWs && (!afterPunct || beforeWs || beforePunct);
    const right = !beforeWs && (!beforePunct || afterWs || afterPunct);
    let canOpen, canClose;
    if (cc === 95) {
      canOpen = left && (!right || beforePunct);
      canClose = right && (!left || afterPunct);
    } else {
      canOpen = left;
      canClose = right;
    }
    if (cc === 126 && numdelims !== 2) canOpen = canClose = false; // only ~~strike~~
    this.pos = i;
    const node = inode('text', { value: s.slice(start, i) });
    appendChild(block, node);
    if (canOpen || canClose) {
      const d = { cc, numdelims, origdelims: numdelims, node, previous: this.delimiters, next: null, canOpen, canClose };
      if (d.previous) d.previous.next = d;
      this.delimiters = d;
    }
  }

  removeDelimiter(d) {
    if (d.previous) d.previous.next = d.next;
    if (d.next) d.next.previous = d.previous;
    else this.delimiters = d.previous;
  }

  processEmphasis(stackBottom) {
    const openersBottom = new Map();
    let closer = this.delimiters;
    while (closer !== null && closer.previous !== stackBottom) closer = closer.previous;
    while (closer !== null) {
      const cc = closer.cc;
      if (!closer.canClose) { closer = closer.next; continue; }
      const key = cc === 126 ? '~' : `${cc}:${closer.canOpen ? 1 : 0}:${closer.origdelims % 3}`;
      const bottom = openersBottom.has(key) ? openersBottom.get(key) : stackBottom;
      let opener = closer.previous;
      let found = false;
      while (opener !== null && opener !== stackBottom && opener !== bottom) {
        const oddMatch = (closer.canOpen || opener.canClose) && closer.origdelims % 3 !== 0 && (opener.origdelims + closer.origdelims) % 3 === 0;
        if (opener.cc === cc && opener.canOpen && (cc === 126 || !oddMatch)) { found = true; break; }
        opener = opener.previous;
      }
      const oldCloser = closer;
      if (!found) {
        closer = closer.next;
      } else if (cc === 126) {
        const del = inode('delete');
        let tmp = opener.node.next;
        while (tmp && tmp !== closer.node) { const nx = tmp.next; unlinkNode(tmp); appendChild(del, tmp); tmp = nx; }
        insertAfter(opener.node, del);
        if (closer.previous !== opener) { closer.previous = opener; opener.next = closer; }
        unlinkNode(opener.node);
        unlinkNode(closer.node);
        this.removeDelimiter(opener);
        const nx = closer.next;
        this.removeDelimiter(closer);
        closer = nx;
      } else {
        const use = closer.numdelims >= 2 && opener.numdelims >= 2 ? 2 : 1;
        const oi = opener.node, ci = closer.node;
        opener.numdelims -= use;
        closer.numdelims -= use;
        oi.value = oi.value.slice(0, oi.value.length - use);
        ci.value = ci.value.slice(0, ci.value.length - use);
        const emph = inode(use === 1 ? 'emphasis' : 'strong');
        let tmp = oi.next;
        while (tmp && tmp !== ci) { const nx = tmp.next; unlinkNode(tmp); appendChild(emph, tmp); tmp = nx; }
        insertAfter(oi, emph);
        if (closer.previous !== opener) { closer.previous = opener; opener.next = closer; }
        if (opener.numdelims === 0) { unlinkNode(oi); this.removeDelimiter(opener); }
        if (closer.numdelims === 0) {
          unlinkNode(ci);
          const nx = closer.next;
          this.removeDelimiter(closer);
          closer = nx;
        }
      }
      if (!found) {
        openersBottom.set(key, oldCloser.previous);
        if (!oldCloser.canOpen) this.removeDelimiter(oldCloser);
      }
    }
    while (this.delimiters !== null && this.delimiters !== stackBottom) this.removeDelimiter(this.delimiters);
  }

  addBracket(node, index, image) {
    if (this.brackets) this.brackets.bracketAfter = true;
    this.brackets = { node, previous: this.brackets, previousDelimiter: this.delimiters, index, image, active: true, bracketAfter: false };
  }

  parseOpenBracket(block) {
    const start = this.pos;
    this.pos++;
    const node = inode('text', { value: '[' });
    appendChild(block, node);
    this.addBracket(node, start, false);
  }

  parseBang(block) {
    const start = this.pos;
    this.pos++;
    if (this.subject.charCodeAt(this.pos) === 91) {
      this.pos++;
      const node = inode('text', { value: '![' });
      appendChild(block, node);
      this.addBracket(node, start + 1, true);
    } else {
      this.text(block, '!');
    }
  }

  parseCloseBracket(block) {
    const s = this.subject;
    this.pos++;
    const startpos = this.pos;
    const opener = this.brackets;
    if (!opener) { this.text(block, ']'); return; }
    if (!opener.active) { this.text(block, ']'); this.brackets = opener.previous; return; }
    const isImage = opener.image;
    const savepos = this.pos;
    let dest = null, title = '', matched = false;

    if (s.charCodeAt(this.pos) === 40) {
      this.pos++;
      this.spnl();
      const d = this.parseLinkDestination();
      if (d !== null) {
        this.spnl();
        let t = null;
        if (reWhitespaceChar.test(s.charAt(this.pos - 1))) t = this.parseLinkTitle();
        this.spnl();
        if (s.charCodeAt(this.pos) === 41) {
          this.pos++;
          matched = true;
          dest = d;
          title = t || '';
        }
      }
      if (!matched) this.pos = savepos;
    }
    if (!matched) {
      const beforeLabel = this.pos;
      const n = this.parseLinkLabel();
      let reflabel = null;
      if (n > 2) reflabel = s.slice(beforeLabel, beforeLabel + n);
      else if (!opener.bracketAfter && startpos - opener.index <= 1001) reflabel = s.slice(opener.index, startpos);
      if (n === 0) this.pos = savepos;
      if (reflabel) {
        const ref = this.refmap.get(normalizeReference(reflabel));
        if (ref) { dest = ref.destination; title = ref.title; matched = true; }
      }
    }
    if (!matched) {
      this.brackets = opener.previous;
      this.pos = startpos;
      this.text(block, ']');
      return;
    }
    const node = inode(isImage ? 'image' : 'link', { url: dest, title });
    let tmp = opener.node.next;
    while (tmp) { const nx = tmp.next; unlinkNode(tmp); appendChild(node, tmp); tmp = nx; }
    appendChild(block, node);
    this.processEmphasis(opener.previousDelimiter);
    this.brackets = opener.previous;
    unlinkNode(opener.node);
    if (!isImage) {
      for (let o = this.brackets; o; o = o.previous) if (!o.image) o.active = false; // no links in links
    }
  }

  parseAutolink(block) {
    const s = this.subject;
    reEmailAutolink.lastIndex = this.pos;
    let m = reEmailAutolink.exec(s);
    let url, kind;
    if (m) { url = 'mailto:' + m[1]; kind = 'email'; }
    else {
      reAutolink.lastIndex = this.pos;
      m = reAutolink.exec(s);
      if (!m) return false;
      url = m[0].slice(1, -1);
      kind = 'angle';
    }
    const label = m[0].slice(1, -1);
    const link = inode('link', { url: normalizeURI(url), title: '', autolink: kind });
    appendChild(link, inode('text', { value: label }));
    appendChild(block, link);
    this.pos += m[0].length;
    return true;
  }

  parseExtAutolink(block) {
    const s = this.subject, start = this.pos;
    reExtUrl.lastIndex = start;
    const m = reExtUrl.exec(s);
    if (!m) return false;
    const text = trimAutolinkTail(m[0]);
    const www = /^www\./i.test(text);
    const host = reHost.exec(text.slice(www ? 0 : text.indexOf('//') + 2));
    if (!host) return false;
    const labels = host[0].split('.');
    if (www && labels.length < 2) return false;
    if (labels.slice(-2).some((l) => l.includes('_'))) return false;
    const link = inode('link', { url: normalizeURI((www ? 'http://' : '') + text), title: '', autolink: 'bare' });
    appendChild(link, inode('text', { value: text }));
    appendChild(block, link);
    this.pos = start + text.length;
    return true;
  }

  parseEntity(block) {
    reEntityHere.lastIndex = this.pos;
    const m = reEntityHere.exec(this.subject);
    if (m) {
      const v = decodeEntityParts(m[0], m[1], m[2], m[3]);
      if (v !== m[0]) {
        this.text(block, v);
        this.pos += m[0].length;
        return;
      }
    }
    this.text(block, '&');
    this.pos++;
  }
}

/** Parses a reference definition at the start of s; returns chars consumed or 0. */
function parseReference(s, refmap) {
  const ip = new InlineParser(refmap);
  ip.subject = s;
  ip.pos = 0;
  ip.titleFail = Object.create(null);
  const n = ip.parseLinkLabel();
  if (n === 0) return 0;
  const rawlabel = s.slice(0, n);
  if (s.charCodeAt(ip.pos) !== 58) return 0;
  ip.pos++;
  ip.spnl();
  const dest = ip.parseLinkDestination();
  if (dest === null) return 0;
  const beforeTitle = ip.pos;
  ip.spnl();
  let title = null;
  if (ip.pos !== beforeTitle) title = ip.parseLinkTitle();
  if (title === null) { title = ''; ip.pos = beforeTitle; }
  const atLineEnd = () => {
    let i = ip.pos;
    while (s.charCodeAt(i) === 32 || s.charCodeAt(i) === 9) i++;
    if (i >= s.length || s.charCodeAt(i) === 10) { ip.pos = i >= s.length ? i : i + 1; return true; }
    return false;
  };
  if (!atLineEnd()) {
    if (title === '') return 0;
    title = '';
    ip.pos = beforeTitle;
    if (!atLineEnd()) return 0;
  }
  const norm = normalizeReference(rawlabel);
  if (norm === '') return 0;
  if (!refmap.has(norm)) refmap.set(norm, { destination: dest, title });
  return ip.pos;
}

// --- linked inline tree → AST arrays ---

const isEmailLocal = (c) => (c >= 48 && c <= 57) || (c >= 65 && c <= 90) || (c >= 97 && c <= 122) || c === 46 || c === 43 || c === 45 || c === 95;
const isEmailDomain = (c) => (c >= 48 && c <= 57) || (c >= 65 && c <= 90) || (c >= 97 && c <= 122) || c === 46 || c === 45 || c === 95;

/** GFM extended email autolinks inside a text value; returns null when there are none. */
function linkifyEmails(value) {
  let at = value.indexOf('@');
  if (at === -1) return null;
  const out = [];
  let last = 0;
  while (at !== -1) {
    let l = at;
    while (l > last && isEmailLocal(value.charCodeAt(l - 1))) l--;
    let r = at + 1;
    while (r < value.length && isEmailDomain(value.charCodeAt(r))) r++;
    while (r > at + 1 && value.charCodeAt(r - 1) === 46) r--;
    const local = value.slice(l, at), domain = value.slice(at + 1, r);
    const ok = local.length > 0 && domain.includes('.') && !/[-_]$/.test(domain) && !domain.startsWith('.') && !domain.includes('..');
    if (ok) {
      if (l > last) out.push({ type: 'text', value: value.slice(last, l) });
      const addr = local + '@' + domain;
      out.push({ type: 'link', url: normalizeURI('mailto:' + addr), title: '', autolink: 'email', children: [{ type: 'text', value: addr }] });
      last = r;
      at = value.indexOf('@', r);
    } else {
      at = value.indexOf('@', at + 1);
    }
  }
  if (!out.length) return null;
  if (last < value.length) out.push({ type: 'text', value: value.slice(last) });
  return out;
}

/** Plain text of a linked subtree without recursion (used when nesting is too deep). */
function flattenText(node) {
  let s = '';
  const stack = [];
  for (let n = node.first; n || stack.length; ) {
    if (!n) { n = stack.pop(); continue; }
    if (n.type === 'text' || n.type === 'code') s += n.value;
    else if (n.type === 'softbreak' || n.type === 'break') s += ' ';
    if (n.first) { if (n.next) stack.push(n.next); n = n.first; } else n = n.next;
  }
  return s;
}

function inlineToAST(parent, depth, inLink) {
  const out = [];
  const pushText = (v) => {
    if (!v) return;
    const last = out[out.length - 1];
    if (last && last.type === 'text') last.value += v;
    else out.push({ type: 'text', value: v });
  };
  for (let n = parent.first; n; n = n.next) {
    switch (n.type) {
      case 'text': pushText(n.value); break;
      case 'softbreak': out.push({ type: 'softbreak' }); break;
      case 'break': out.push({ type: 'break' }); break;
      case 'code': out.push({ type: 'inlineCode', value: n.value }); break;
      case 'emphasis': case 'strong': case 'delete': {
        if (depth >= MAX_INLINE_NESTING) { pushText(flattenText(n)); break; }
        const children = inlineToAST(n, depth + 1, inLink);
        if (children.length) out.push({ type: n.type, children });
        break;
      }
      case 'link': {
        if (inLink || depth >= MAX_INLINE_NESTING) {
          // nested links (e.g. an autolink inside link text) collapse to their text
          if (depth >= MAX_INLINE_NESTING) pushText(flattenText(n));
          else for (const c of inlineToAST(n, depth + 1, true)) c.type === 'text' ? pushText(c.value) : out.push(c);
          break;
        }
        const node = { type: 'link', url: n.url, title: n.title || '', children: inlineToAST(n, depth + 1, true) };
        if (n.autolink) node.autolink = n.autolink;
        out.push(node);
        break;
      }
      case 'image': {
        const alt = depth >= MAX_INLINE_NESTING ? flattenText(n) : plainInline(inlineToAST(n, depth + 1, true));
        out.push({ type: 'image', url: n.url, title: n.title || '', alt });
        break;
      }
      default: break;
    }
  }
  if (inLink) return out;
  // Email autolinks are found on merged text, so "a_b@x.io" is not split by delimiter runs.
  const res = [];
  for (const node of out) {
    const linked = node.type === 'text' ? linkifyEmails(node.value) : null;
    if (linked) res.push(...linked);
    else res.push(node);
  }
  return res;
}

function plainInline(nodes) {
  let s = '';
  for (const n of nodes) {
    if (n.type === 'text' || n.type === 'inlineCode') s += n.value;
    else if (n.type === 'softbreak' || n.type === 'break') s += ' ';
    else if (n.type === 'image') s += n.alt;
    else if (n.children) s += plainInline(n.children);
  }
  return s;
}

function parseInlines(ip, content) {
  return inlineToAST(ip.parse(content || ''), 0, false);
}

/**
 * Parses a single line of inline Markdown (no block structure, no reference definitions).
 * @param {string} text
 * @returns {object[]} inline AST nodes
 */
export function parseInline(text) {
  return parseInlines(new InlineParser(new Map()), String(text ?? ''));
}

const reTaskMarker = /^\[([ xX])\](?:[ \t]+|(?=\n)|$)/;

function blockToAST(b, ip) {
  switch (b.type) {
    case 'document':
      return { type: 'root', children: b.children.map((c) => blockToAST(c, ip)) };
    case 'paragraph':
      return { type: 'paragraph', line: b.line, children: parseInlines(ip, b.content) };
    case 'heading':
      return { type: 'heading', line: b.line, depth: b.level, children: parseInlines(ip, b.content) };
    case 'thematicBreak':
      return { type: 'thematicBreak', line: b.line };
    case 'blockquote':
      return { type: 'blockquote', line: b.line, children: b.children.map((c) => blockToAST(c, ip)) };
    case 'list': {
      const d = b.listData;
      return {
        type: 'list', line: b.line, ordered: d.type === 'ordered', start: d.type === 'ordered' ? d.start : null,
        tight: b.tight !== false, marker: d.bulletChar || d.delimiter,
        children: b.children.map((it) => itemToAST(it, ip)),
      };
    }
    case 'code': {
      const lang = b.info ? b.info.split(/[ \t]/, 1)[0] : '';
      return { type: 'code', line: b.line, lang: lang || null, info: b.info || '', value: b.value || '', fenced: !!b.fenced };
    }
    case 'table': {
      const n = b.header.length;
      const row = (cells, line, header) => ({
        type: 'tableRow', line, header,
        children: Array.from({ length: n }, (_, i) => ({ type: 'tableCell', children: parseInlines(ip, cells[i] || '') })),
      });
      return {
        type: 'table', line: b.line, align: b.align,
        children: [row(b.header, b.line, true), ...b.rows.map((r) => row(r.cells, r.line, false))],
      };
    }
    default:
      return { type: 'paragraph', line: b.line, children: [] };
  }
}

function itemToAST(item, ip) {
  const node = { type: 'listItem', line: item.line, checked: null, children: [] };
  const first = item.children[0];
  if (first && first.type === 'paragraph') {
    const m = reTaskMarker.exec(first.content);
    if (m) {
      node.checked = m[1] !== ' ';
      node.taskLine = first.line;
      first.content = first.content.slice(m[0].length);
    }
  }
  for (const c of item.children) {
    const a = blockToAST(c, ip);
    if (a.type === 'paragraph' && a.children.length === 0 && node.checked !== null && c === first) continue; // "- [ ]" alone
    node.children.push(a);
  }
  return node;
}

/**
 * Parses Markdown into an AST.
 * @param {string} md
 * @returns {object} root node
 */
export function parse(md) {
  let src = String(md ?? '');
  if (src.charCodeAt(0) === 0xfeff) src = src.slice(1);
  src = src.replace(/\r\n?/g, '\n').replace(/\u0000/g, '�');
  const bp = new BlockParser();
  const doc = bp.parse(src);
  return blockToAST(doc, new InlineParser(bp.refmap));
}

// ---------------------------------------------------------------------------------------------
// Rendering: AST → lightweight element tree → DOM or HTML string

const el = (t, a, c, on) => ({ t, a: a || null, c: c || null, on: on || null });

const DEFAULTS = {
  highlight: true,
  breaks: true,
  codeHeader: true,
  headingIds: false,
  sourceLines: false,
  onTaskToggle: null,
  resolveImage: null,
};

function lineAttrs(node, ctx, extra) {
  const a = extra || {};
  if (ctx.opts.sourceLines && typeof node.line === 'number') a['data-line'] = String(node.line);
  return a;
}

function slugify(text) {
  return text.toLowerCase().trim().replace(/[^\p{L}\p{N}\s-]/gu, '').replace(/\s+/g, '-').slice(0, 80) || 'section';
}

function renderBlocks(nodes, ctx, out) {
  for (const n of nodes) renderBlock(n, ctx, out);
  return out;
}

function renderBlock(n, ctx, out) {
  switch (n.type) {
    case 'paragraph':
      if (ctx.tight) renderInlines(n.children, ctx, out);
      else out.push(el('p', lineAttrs(n, ctx), renderInlines(n.children, ctx, [])));
      break;
    case 'heading': {
      const a = lineAttrs(n, ctx);
      if (ctx.opts.headingIds) {
        const base = 'md-' + slugify(plainInline(n.children));
        const count = ctx.slugs.get(base) || 0;
        ctx.slugs.set(base, count + 1);
        a.id = count ? `${base}-${count}` : base;
      }
      out.push(el('h' + n.depth, a, renderInlines(n.children, ctx, [])));
      break;
    }
    case 'thematicBreak':
      out.push(el('hr', lineAttrs(n, ctx)));
      break;
    case 'blockquote':
      out.push(el('blockquote', lineAttrs(n, ctx), renderBlocks(n.children, { ...ctx, tight: false }, [])));
      break;
    case 'list': {
      const hasTask = n.children.some((i) => i.checked !== null);
      const a = lineAttrs(n, ctx, { class: hasTask ? 'md-list md-tasklist' : 'md-list' });
      if (n.ordered && n.start !== 1) a.start = String(n.start);
      const inner = { ...ctx, tight: n.tight };
      out.push(el(n.ordered ? 'ol' : 'ul', a, n.children.map((item) => renderItem(item, inner))));
      break;
    }
    case 'code':
      out.push(renderCode(n, ctx));
      break;
    case 'table':
      out.push(renderTable(n, ctx));
      break;
    default:
      break;
  }
}

function renderItem(item, ctx) {
  const body = renderBlocks(item.children, ctx, []);
  if (item.checked === null) return el('li', lineAttrs(item, ctx), body);
  const label = plainText(item.children).slice(0, 120);
  const cb = { type: 'checkbox', class: 'md-task-checkbox', 'aria-label': label || 'Task' };
  if (item.checked) cb.checked = '';
  let on = null;
  const toggle = ctx.opts.onTaskToggle;
  if (typeof toggle === 'function') {
    const line = item.taskLine;
    on = { change: (ev) => toggle(line, !!ev.target.checked) };
  } else {
    cb.disabled = '';
  }
  const a = lineAttrs(item, ctx, { class: item.checked ? 'md-task md-task-done' : 'md-task', 'data-task-line': String(item.taskLine) });
  return el('li', a, [el('input', cb, null, on), el('div', { class: 'md-task-body' }, body)]);
}

function renderCode(n, ctx) {
  const lang = n.lang ? resolveLanguage(n.lang) : null;
  const langToken = n.lang ? n.lang.toLowerCase().replace(/[^a-z0-9_+#.-]/g, '').slice(0, 32) : '';
  let codeChildren;
  if (ctx.opts.highlight && lang && n.value.length <= ctx.hl.left) {
    ctx.hl.left -= n.value.length;
    codeChildren = tokenize(n.value, lang).map((t) => (t.type ? el('span', { class: 'hl-' + t.type }, [t.value]) : t.value));
  } else {
    codeChildren = [n.value];
  }
  const code = el('code', langToken ? { class: 'language-' + langToken } : null, codeChildren);
  if (!ctx.opts.codeHeader) return el('pre', lineAttrs(n, ctx, { class: 'md-pre' }), [code]);
  const label = n.lang ? languageLabel(n.lang) : 'Plain text';
  const copy = el('button', { type: 'button', class: 'md-code-copy', title: 'Copy code', 'aria-label': 'Copy code' }, ['Copy'], {
    click: (ev) => copyToClipboard(n.value, ev.currentTarget),
  });
  return el('div', lineAttrs(n, ctx, { class: 'md-code', 'data-lang': lang || langToken }), [
    el('div', { class: 'md-code-header' }, [el('span', { class: 'md-code-lang' }, [label]), copy]),
    el('pre', { class: 'md-pre' }, [code]),
  ]);
}

function renderTable(n, ctx) {
  const cellAttrs = (i) => (n.align[i] ? { class: 'md-align-' + n.align[i], align: n.align[i] } : null);
  const rowEl = (row, tag) => el('tr', lineAttrs(row, ctx), row.children.map((cell, i) => el(tag, cellAttrs(i), renderInlines(cell.children, ctx, []))));
  const [head, ...rows] = n.children;
  const parts = [el('thead', null, [rowEl(head, 'th')])];
  if (rows.length) parts.push(el('tbody', null, rows.map((r) => rowEl(r, 'td'))));
  return el('div', lineAttrs(n, ctx, { class: 'md-table-wrap' }), [el('table', { class: 'md-table' }, parts)]);
}

function renderInlines(nodes, ctx, out) {
  for (const n of nodes) {
    switch (n.type) {
      case 'text': out.push(n.value); break;
      case 'softbreak': out.push(ctx.opts.breaks ? el('br') : '\n'); break;
      case 'break': out.push(el('br')); break;
      case 'emphasis': out.push(el('em', null, renderInlines(n.children, ctx, []))); break;
      case 'strong': out.push(el('strong', null, renderInlines(n.children, ctx, []))); break;
      case 'delete': out.push(el('del', null, renderInlines(n.children, ctx, []))); break;
      case 'inlineCode': out.push(el('code', { class: 'md-code-inline' }, [n.value])); break;
      case 'link': {
        const kids = renderInlines(n.children, ctx, []);
        const href = safeLinkHref(n.url);
        if (href === null) { out.push(...kids); break; } // unsafe scheme: keep the text only
        const a = { href };
        if (n.title) a.title = n.title;
        if (isAbsoluteHttp(href)) { a.target = '_blank'; a.rel = 'noopener noreferrer'; }
        else if (urlKind(href) === 'mailto') a.rel = 'noopener noreferrer';
        out.push(el('a', a, kids));
        break;
      }
      case 'image': {
        let src = safeImageSrc(n.url);
        if (src !== null && typeof ctx.opts.resolveImage === 'function') {
          const r = ctx.opts.resolveImage(src);
          src = typeof r === 'string' ? safeImageSrc(r) : null;
        }
        if (src === null) { if (n.alt) out.push(n.alt); break; }
        const a = { class: 'md-img', src, alt: n.alt, loading: 'lazy', decoding: 'async', referrerpolicy: 'no-referrer' };
        if (n.title) a.title = n.title;
        out.push(el('img', a));
        break;
      }
      default: break;
    }
  }
  return out;
}

async function copyToClipboard(text, btn) {
  let ok = false;
  try {
    await globalThis.navigator.clipboard.writeText(text);
    ok = true;
  } catch {
    const doc = btn.ownerDocument;
    const ta = doc.createElement('textarea');
    ta.value = text;
    ta.setAttribute('readonly', '');
    ta.className = 'md-copy-buffer';
    doc.body.appendChild(ta);
    ta.select();
    try { ok = doc.execCommand('copy'); } catch { ok = false; }
    ta.remove();
  }
  btn.textContent = ok ? 'Copied' : 'Copy failed';
  btn.classList.add(ok ? 'is-copied' : 'is-failed');
  clearTimeout(btn._mdTimer);
  btn._mdTimer = setTimeout(() => {
    btn.textContent = 'Copy';
    btn.classList.remove('is-copied', 'is-failed');
  }, 1600);
}

function appendVNodes(children, doc, parent) {
  let pending = '';
  for (const c of children) {
    if (typeof c === 'string') { pending += c; continue; }
    if (pending) { parent.appendChild(doc.createTextNode(pending)); pending = ''; }
    const e = doc.createElement(c.t);
    if (c.a) for (const k in c.a) e.setAttribute(k, c.a[k]);
    if (c.on) for (const k in c.on) e.addEventListener(k, c.on[k]);
    if (c.c) appendVNodes(c.c, doc, e);
    parent.appendChild(e);
  }
  if (pending) parent.appendChild(doc.createTextNode(pending));
}

const VOID_TAGS = new Set(['br', 'hr', 'img', 'input']);
const BLOCK_END_NL = new Set(['p', 'li', 'h1', 'h2', 'h3', 'h4', 'h5', 'h6', 'tr', 'pre', 'ul', 'ol', 'blockquote', 'table']);
const HTML_ESC = { '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;', "'": '&#39;' };
const escapeHtml = (s) => String(s).replace(/[&<>"']/g, (ch) => HTML_ESC[ch]);

function vnodesToHTML(children) {
  let s = '';
  for (const c of children) {
    if (typeof c === 'string') { s += escapeHtml(c); continue; }
    s += '<' + c.t;
    if (c.a) for (const k in c.a) s += ` ${k}="${escapeHtml(c.a[k])}"`;
    if (VOID_TAGS.has(c.t)) { s += c.t === 'hr' ? ' />\n' : ' />'; continue; }
    s += '>';
    if (c.c) s += vnodesToHTML(c.c);
    s += `</${c.t}>`;
    if (BLOCK_END_NL.has(c.t) || (c.t === 'div' && c.a && /md-(code|table-wrap)\b/.test(c.a.class || ''))) s += '\n';
  }
  return s;
}

function buildTree(astOrMd, opts) {
  const ast = typeof astOrMd === 'string' || astOrMd == null ? parse(astOrMd ?? '') : astOrMd;
  // Highlighting runs on the main thread, so one document (a shared .md file can hold many fenced
  // blocks) gets the same character budget as one highlighted file. hl is an object so that the
  // { ...ctx } copies made for nested blocks share it.
  const ctx = { opts: { ...DEFAULTS, ...(opts || {}) }, tight: false, slugs: new Map(), hl: { left: MAX_HIGHLIGHT_CHARS } };
  return renderBlocks(ast.type === 'root' ? ast.children : [ast], ctx, []);
}

/**
 * Renders Markdown (or a parsed AST) into DOM nodes.
 * @param {string|object} astOrMd
 * @param {Document} [doc]
 * @param {{onTaskToggle?: (line:number, checked:boolean)=>void, resolveImage?: (src:string)=>string|null,
 *          highlight?: boolean, breaks?: boolean, codeHeader?: boolean, headingIds?: boolean, sourceLines?: boolean}} [opts]
 *   breaks (default true): single newlines inside a paragraph render as <br>, like most notes apps.
 * @returns {DocumentFragment} wrap it in an element with class "md" for styling.
 */
export function renderToDOM(astOrMd, doc = globalThis.document, opts = {}) {
  const frag = doc.createDocumentFragment();
  appendVNodes(buildTree(astOrMd, opts), doc, frag);
  return frag;
}

/**
 * Renders into an escaped HTML string with the same safety rules as renderToDOM. Defaults to no
 * syntax highlighting and no code-block header (exports); pass opts to change.
 * @param {string|object} astOrMd
 * @param {object} [opts]
 * @returns {string}
 */
export function renderToHTML(astOrMd, opts = {}) {
  return vnodesToHTML(buildTree(astOrMd, { highlight: false, codeHeader: false, ...opts, onTaskToggle: null }));
}

// ---------------------------------------------------------------------------------------------
// Text utilities

function collectPlain(nodes, parts) {
  for (const n of nodes) {
    switch (n.type) {
      case 'paragraph': case 'heading': parts.push(plainInline(n.children)); break;
      case 'code': parts.push(n.value); break;
      case 'blockquote': case 'listItem': case 'list': case 'root': collectPlain(n.children, parts); break;
      case 'table':
        for (const row of n.children) for (const cell of row.children) parts.push(plainInline(cell.children));
        break;
      default: break;
    }
  }
  return parts;
}

function plainText(nodes) {
  return collectPlain(nodes, []).join(' ').replace(/\s+/g, ' ').trim();
}

/** Cuts a huge document to a prefix ending at a line boundary (snippets/titles need only the start). */
function prefixForSnippet(md, budget) {
  if (md.length <= budget) return md;
  const nl = md.lastIndexOf('\n', budget);
  return md.slice(0, nl > 0 ? nl : budget);
}

/**
 * Plain-text snippet (whitespace collapsed, Markdown syntax removed).
 * @param {string} md
 * @param {number} [maxLen] truncates with an ellipsis at a word boundary when possible
 * @returns {string}
 */
export function toPlainText(md, maxLen = Infinity) {
  let src = String(md ?? '');
  if (Number.isFinite(maxLen)) src = prefixForSnippet(src, Math.max(4096, maxLen * 8));
  const text = plainText(parse(src).children);
  if (!Number.isFinite(maxLen) || text.length <= maxLen) return text;
  if (maxLen <= 1) return text.slice(0, Math.max(0, maxLen));
  let cut = text.slice(0, maxLen - 1);
  const sp = cut.lastIndexOf(' ');
  if (sp > maxLen * 0.6) cut = cut.slice(0, sp);
  return cut.trimEnd() + '…';
}

function firstLineOf(nodes) {
  for (const n of nodes) {
    switch (n.type) {
      case 'paragraph': case 'heading': {
        const lines = [''];
        const walk = (kids) => {
          for (const k of kids) {
            if (k.type === 'softbreak' || k.type === 'break') lines.push('');
            else if (k.type === 'text' || k.type === 'inlineCode') lines[lines.length - 1] += k.value;
            else if (k.type === 'image') lines[lines.length - 1] += k.alt;
            else if (k.children) walk(k.children);
          }
        };
        walk(n.children);
        const line = lines.map((l) => l.replace(/\s+/g, ' ').trim()).find(Boolean);
        if (line) return line;
        break;
      }
      case 'code': {
        const line = n.value.split('\n').map((l) => l.trim()).find(Boolean);
        if (line) return line;
        break;
      }
      case 'blockquote': case 'list': case 'listItem': {
        const r = firstLineOf(n.children);
        if (r) return r;
        break;
      }
      case 'table': {
        const r = n.children[0].children.map((c) => plainInline(c.children).trim()).filter(Boolean).join(' ');
        if (r) return r;
        break;
      }
      default: break;
    }
  }
  return '';
}

/**
 * Title of a note: the first non-empty line, as plain text (max 200 chars).
 * @param {string} md
 * @returns {string}
 */
export function extractTitle(md) {
  const src = prefixForSnippet(String(md ?? ''), 16384);
  return firstLineOf(parse(src).children).slice(0, 200);
}

const reTaskLine = /^((?:[ \t]*(?:>|(?:[*+-]|\d{1,9}[.)])(?=[ \t])))*[ \t]*\[)([ xX])\]/;

/**
 * Returns md with the task checkbox on the given (0-based) source line set to `checked`.
 * Unknown lines or lines without a task marker leave md unchanged.
 * @param {string} md
 * @param {number} line
 * @param {boolean} checked
 * @returns {string}
 */
export function toggleTask(md, line, checked) {
  const src = String(md ?? '');
  if (!Number.isInteger(line) || line < 0) return src;
  const parts = src.split(/(\r\n|\r|\n)/);
  const idx = line * 2;
  if (idx >= parts.length) return src;
  const m = reTaskLine.exec(parts[idx]);
  if (!m) return src;
  const ch = checked ? 'x' : ' ';
  if ((m[2] !== ' ') === !!checked) return src;
  parts[idx] = m[1] + ch + parts[idx].slice(m[1].length + 1);
  return parts.join('');
}

/**
 * Convenience: renders md into `container` (clearing it) and marks it with class "md".
 * @param {Element} container
 * @param {string|object} astOrMd
 * @param {object} [opts] see renderToDOM
 */
export function renderInto(container, astOrMd, opts = {}) {
  container.classList.add('md');
  container.replaceChildren(renderToDOM(astOrMd, container.ownerDocument, opts));
}

export { plainText as astToPlainText, escapeHtml };
