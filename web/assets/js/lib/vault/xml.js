/**
 * Small, safe XML 1.0 parser + serializer for KeePass documents (no DOMParser, so it works in MV3
 * service workers).
 *
 * Safety: any <!DOCTYPE>/<!ENTITY> or other markup declaration is rejected, only the five predefined
 * entities and numeric character references are expanded (so entity-expansion bombs are impossible),
 * and nesting depth, element count, attribute count and input length are capped.
 *
 * Tree shape: element = { name, attrs: {[name]: value} (null-prototype), children: (element|string)[] }.
 * Whitespace-only text between child elements is dropped; text inside leaf elements is kept verbatim.
 */
import { VaultError } from './errors.js';

export const DEFAULT_XML_LIMITS = Object.freeze({
  maxLength: 256 * 1024 * 1024, // characters
  maxDepth: 256,
  maxElements: 5_000_000,
  maxAttributes: 64,
});

const NAME_RE = /[A-Za-z_:À-￿][-A-Za-z0-9_:.·À-￿]*/y;
const WS_RE = /[ \t\n]*/y;
const ENTITY_RE = /&(?:(amp|lt|gt|quot|apos)|#([0-9]{1,8})|#x([0-9A-Fa-f]{1,6}));|&/g;
const PREDEF = { amp: '&', lt: '<', gt: '>', quot: '"', apos: "'" };

function fail(msg, pos) {
  return new VaultError('xml_invalid', pos === undefined ? msg : `${msg} at offset ${pos}`);
}

function isXmlChar(cp) {
  return cp === 0x9 || cp === 0xa || cp === 0xd || (cp >= 0x20 && cp <= 0xd7ff) || (cp >= 0xe000 && cp <= 0xfffd) || (cp >= 0x10000 && cp <= 0x10ffff);
}

function decodeEntities(s, pos) {
  if (s.indexOf('&') < 0) return s;
  return s.replace(ENTITY_RE, (m, named, dec, hex) => {
    if (named) return PREDEF[named];
    if (dec || hex) {
      const cp = dec ? parseInt(dec, 10) : parseInt(hex, 16);
      if (!isXmlChar(cp)) throw fail(`invalid character reference ${m}`, pos);
      return String.fromCodePoint(cp);
    }
    throw fail('undefined entity or bare "&"', pos);
  });
}

/** @typedef {{name:string, attrs:Record<string,string>, children:Array<XmlElement|string>}} XmlElement */

/**
 * Parse an XML document and return its root element.
 * @param {string} input
 * @param {Partial<typeof DEFAULT_XML_LIMITS>} [limits]
 * @returns {XmlElement}
 */
export function parseXml(input, limits = {}) {
  const lim = { ...DEFAULT_XML_LIMITS, ...limits };
  if (typeof input !== 'string') throw fail('XML input must be a string');
  if (input.length > lim.maxLength) throw new VaultError('limit_exceeded', 'XML document too large');
  // XML §2.11 end-of-line normalisation; character references (&#13;) still produce CR afterwards.
  let s = input.indexOf('\r') >= 0 ? input.replace(/\r\n?/g, '\n') : input;
  if (s.charCodeAt(0) === 0xfeff) s = s.slice(1);
  const len = s.length;
  let pos = 0;
  let root = null;
  let elements = 0;
  const stack = [];

  const skipWs = () => {
    WS_RE.lastIndex = pos;
    WS_RE.exec(s);
    pos = WS_RE.lastIndex;
  };
  const readName = () => {
    NAME_RE.lastIndex = pos;
    const m = NAME_RE.exec(s);
    if (!m) throw fail('expected a name', pos);
    pos = NAME_RE.lastIndex;
    return m[0];
  };

  // XML declaration (only at the very start).
  if (s.startsWith('<?xml') && /[\s?]/.test(s[5] || '')) {
    const end = s.indexOf('?>', 5);
    if (end < 0) throw fail('unterminated XML declaration', 0);
    pos = end + 2;
  }

  while (pos < len) {
    const lt = s.indexOf('<', pos);
    const textEnd = lt < 0 ? len : lt;
    if (textEnd > pos) {
      const raw = s.slice(pos, textEnd);
      if (stack.length === 0) {
        if (!/^[ \t\n]*$/.test(raw)) throw fail('text outside the root element', pos);
      } else {
        if (raw.indexOf(']]>') >= 0) throw fail('"]]>" not allowed in text', pos);
        const text = decodeEntities(raw, pos);
        const kids = stack[stack.length - 1].children;
        if (typeof kids[kids.length - 1] === 'string') kids[kids.length - 1] += text;
        else kids.push(text);
      }
      pos = textEnd;
    }
    if (pos >= len) break;
    // s[pos] === '<'
    if (s.startsWith('<!--', pos)) {
      const end = s.indexOf('-->', pos + 4);
      if (end < 0) throw fail('unterminated comment', pos);
      pos = end + 3;
    } else if (s.startsWith('<![CDATA[', pos)) {
      if (stack.length === 0) throw fail('CDATA outside the root element', pos);
      const end = s.indexOf(']]>', pos + 9);
      if (end < 0) throw fail('unterminated CDATA section', pos);
      const text = s.slice(pos + 9, end);
      const kids = stack[stack.length - 1].children;
      if (typeof kids[kids.length - 1] === 'string') kids[kids.length - 1] += text;
      else kids.push(text);
      pos = end + 3;
    } else if (s.startsWith('<!', pos)) {
      throw fail(s.startsWith('<!DOCTYPE', pos) ? 'DOCTYPE is not allowed' : 'markup declarations are not allowed', pos);
    } else if (s.startsWith('<?', pos)) {
      const end = s.indexOf('?>', pos + 2);
      if (end < 0) throw fail('unterminated processing instruction', pos);
      if (/^<\?xml[\s?]/i.test(s.slice(pos, pos + 6))) throw fail('misplaced XML declaration', pos);
      pos = end + 2;
    } else if (s[pos + 1] === '/') {
      pos += 2;
      const name = readName();
      skipWs();
      if (s[pos] !== '>') throw fail('expected ">"', pos);
      pos++;
      const open = stack.pop();
      if (!open || open.name !== name) throw fail(`mismatched closing tag </${name}>`, pos);
      finishElement(open);
    } else {
      pos++;
      const name = readName();
      const el = { name, attrs: Object.create(null), children: [] };
      if (++elements > lim.maxElements) throw new VaultError('limit_exceeded', 'too many XML elements');
      let nattrs = 0;
      for (;;) {
        const before = pos;
        skipWs();
        const c = s[pos];
        if (c === '>' || c === '/') break;
        if (pos === before) throw fail('expected whitespace before attribute', pos);
        const an = readName();
        skipWs();
        if (s[pos] !== '=') throw fail('expected "=" after attribute name', pos);
        pos++;
        skipWs();
        const q = s[pos];
        if (q !== '"' && q !== "'") throw fail('attribute value must be quoted', pos);
        const end = s.indexOf(q, pos + 1);
        if (end < 0) throw fail('unterminated attribute value', pos);
        const rawVal = s.slice(pos + 1, end);
        if (rawVal.indexOf('<') >= 0) throw fail('"<" not allowed in attribute value', pos);
        if (an in el.attrs) throw fail(`duplicate attribute "${an}"`, pos);
        if (++nattrs > lim.maxAttributes) throw new VaultError('limit_exceeded', 'too many XML attributes');
        // Attribute-value normalisation: literal whitespace → space; references are expanded after.
        el.attrs[an] = decodeEntities(rawVal.replace(/[\t\n]/g, ' '), pos);
        pos = end + 1;
      }
      let selfClosing = false;
      if (s[pos] === '/') {
        selfClosing = true;
        pos++;
      }
      if (s[pos] !== '>') throw fail('expected ">"', pos);
      pos++;
      if (stack.length === 0) {
        if (root) throw fail('multiple root elements', pos);
        root = el;
      } else {
        stack[stack.length - 1].children.push(el);
      }
      if (!selfClosing) {
        stack.push(el);
        if (stack.length > lim.maxDepth) throw new VaultError('limit_exceeded', 'XML nesting too deep');
      }
    }
  }
  if (stack.length) throw fail(`unclosed element <${stack[stack.length - 1].name}>`);
  if (!root) throw fail('no root element');
  return root;
}

function finishElement(el) {
  // Drop indentation whitespace when the element has element children (KeePass never uses mixed content).
  const kids = el.children;
  if (kids.length > 1 && kids.some((k) => typeof k !== 'string')) {
    el.children = kids.filter((k) => typeof k !== 'string' || !/^[ \t\n]*$/.test(k));
  }
}

// --- helpers --------------------------------------------------------------------------------------

/** Create an element. Children may be elements or strings; null/undefined are skipped. */
export function el(name, attrs, ...children) {
  const a = Object.create(null);
  if (attrs) for (const [k, v] of Object.entries(attrs)) if (v !== undefined && v !== null) a[k] = String(v);
  const kids = [];
  // Empty strings are skipped so <x></x> and <x/> build identical trees.
  for (const c of children.flat()) if (c !== undefined && c !== null && c !== '') kids.push(typeof c === 'object' ? c : String(c));
  return { name, attrs: a, children: kids };
}

/** Child elements, optionally filtered by name. */
export function childElements(node, name) {
  return node.children.filter((c) => typeof c === 'object' && (name === undefined || c.name === name));
}

/** First child element with the given name, or undefined. */
export function firstChild(node, name) {
  for (const c of node.children) if (typeof c === 'object' && c.name === name) return c;
  return undefined;
}

/** Concatenated text content (recursive). */
export function textOf(node) {
  if (!node) return '';
  let out = '';
  for (const c of node.children) out += typeof c === 'string' ? c : textOf(c);
  return out;
}

// --- serializer -----------------------------------------------------------------------------------

// Characters that cannot appear in XML 1.0 at all (not even as references) are removed.
// eslint-disable-next-line no-control-regex
const INVALID_XML_RE = /[\u0000-\u0008\u000B\u000C\u000E-\u001F￾￿]|[\uD800-\uDBFF](?![\uDC00-\uDFFF])|(?<![\uD800-\uDBFF])[\uDC00-\uDFFF]/g;

/** Escape text content. CR is written as a reference so it survives end-of-line normalisation. */
export function escapeText(s) {
  return String(s)
    .replace(INVALID_XML_RE, '')
    .replace(/&/g, '&amp;')
    .replace(/</g, '&lt;')
    .replace(/>/g, '&gt;')
    .replace(/\r/g, '&#13;');
}

/** Escape an attribute value (double-quoted). */
export function escapeAttr(s) {
  return escapeText(s).replace(/"/g, '&quot;').replace(/\n/g, '&#10;').replace(/\t/g, '&#9;');
}

/**
 * Serialize an element tree.
 * @param {XmlElement} root
 * @param {{declaration?: boolean, indent?: string}} [opts] indent '' disables pretty-printing
 * @returns {string}
 */
export function serializeXml(root, { declaration = true, indent = '\t' } = {}) {
  const out = [];
  if (declaration) out.push('<?xml version="1.0" encoding="utf-8" standalone="yes"?>', indent ? '\n' : '');
  // Iterative to be safe with deep trees.
  const stack = [{ node: root, depth: 0, i: -1 }];
  while (stack.length) {
    const fr = stack[stack.length - 1];
    const { node, depth } = fr;
    const pad = indent ? indent.repeat(depth) : '';
    if (fr.i === -1) {
      let open = `<${node.name}`;
      for (const k in node.attrs) open += ` ${k}="${escapeAttr(node.attrs[k])}"`;
      const kids = node.children;
      if (kids.length === 0) {
        out.push(pad, open, ' />', indent ? '\n' : '');
        stack.pop();
        continue;
      }
      if (kids.every((k) => typeof k === 'string')) {
        out.push(pad, open, '>', escapeText(kids.join('')), `</${node.name}>`, indent ? '\n' : '');
        stack.pop();
        continue;
      }
      out.push(pad, open, '>', indent ? '\n' : '');
      fr.i = 0;
    }
    if (fr.i < node.children.length) {
      const child = node.children[fr.i++];
      if (typeof child === 'string') {
        if (!/^[ \t\n]*$/.test(child) || !indent) out.push(escapeText(child));
      } else {
        stack.push({ node: child, depth: depth + 1, i: -1 });
      }
    } else {
      out.push(pad, `</${node.name}>`, indent ? '\n' : '');
      stack.pop();
    }
  }
  return out.join('');
}
