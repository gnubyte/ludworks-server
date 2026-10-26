// Minimal DOM for unit-testing the Markdown libraries in Node (no jsdom, no dependencies).
// Implements only what lib/markdown.js, lib/highlight.js and the richeditor serializer/renderer use.

class FakeNode {
  constructor(doc, nodeType, nodeName) {
    this.ownerDocument = doc;
    this.nodeType = nodeType;
    this.nodeName = nodeName;
    this.parentNode = null;
    this.childNodes = [];
  }

  get firstChild() { return this.childNodes[0] || null; }
  get lastChild() { return this.childNodes[this.childNodes.length - 1] || null; }

  get nextSibling() {
    if (!this.parentNode) return null;
    const s = this.parentNode.childNodes;
    return s[s.indexOf(this) + 1] || null;
  }

  get previousSibling() {
    if (!this.parentNode) return null;
    const s = this.parentNode.childNodes;
    return s[s.indexOf(this) - 1] || null;
  }

  appendChild(n) { return this.insertBefore(n, null); }

  insertBefore(n, ref) {
    if (n.nodeType === 11) {
      for (const c of [...n.childNodes]) this.insertBefore(c, ref);
      return n;
    }
    if (n.parentNode) n.parentNode.removeChild(n);
    n.parentNode = this;
    const i = ref ? this.childNodes.indexOf(ref) : -1;
    if (i < 0) this.childNodes.push(n);
    else this.childNodes.splice(i, 0, n);
    return n;
  }

  removeChild(n) {
    const i = this.childNodes.indexOf(n);
    if (i >= 0) this.childNodes.splice(i, 1);
    n.parentNode = null;
    return n;
  }

  replaceChild(n, old) {
    this.insertBefore(n, old);
    return this.removeChild(old);
  }

  remove() { if (this.parentNode) this.parentNode.removeChild(this); }

  contains(n) {
    for (let x = n; x; x = x.parentNode) if (x === this) return true;
    return false;
  }

  get textContent() {
    if (this.nodeType === 3) return this.nodeValue;
    return this.childNodes.map((c) => c.textContent).join('');
  }

  set textContent(v) {
    for (const c of [...this.childNodes]) this.removeChild(c);
    if (v) this.appendChild(this.ownerDocument.createTextNode(String(v)));
  }
}

class FakeText extends FakeNode {
  constructor(doc, value) {
    super(doc, 3, '#text');
    this.nodeValue = String(value);
  }

  get data() { return this.nodeValue; }
  set data(v) { this.nodeValue = String(v); }
}

class FakeComment extends FakeNode {
  constructor(doc, value) {
    super(doc, 8, '#comment');
    this.nodeValue = String(value);
  }

  get textContent() { return ''; }
}

class FakeElement extends FakeNode {
  constructor(doc, tag) {
    super(doc, 1, String(tag).toUpperCase());
    this.tagName = this.nodeName;
    this.attrs = new Map();
    this.listeners = {};
  }

  setAttribute(k, v) { this.attrs.set(String(k).toLowerCase(), String(v)); }
  getAttribute(k) { const key = String(k).toLowerCase(); return this.attrs.has(key) ? this.attrs.get(key) : null; }
  hasAttribute(k) { return this.attrs.has(String(k).toLowerCase()); }
  removeAttribute(k) { this.attrs.delete(String(k).toLowerCase()); }
  get attributes() { return [...this.attrs].map(([name, value]) => ({ name, value })); }

  get className() { return this.getAttribute('class') || ''; }
  set className(v) { this.setAttribute('class', v); }

  get classList() {
    const el = this;
    const list = () => el.className.split(/\s+/).filter(Boolean);
    return {
      add: (...c) => { el.className = [...new Set([...list(), ...c])].join(' '); },
      remove: (...c) => { el.className = list().filter((x) => !c.includes(x)).join(' '); },
      contains: (c) => list().includes(c),
      toggle: (c, force) => {
        const on = force === undefined ? !list().includes(c) : force;
        if (on) el.classList.add(c); else el.classList.remove(c);
        return on;
      },
    };
  }

  get children() { return this.childNodes.filter((c) => c.nodeType === 1); }
  get firstElementChild() { return this.children[0] || null; }
  get lastElementChild() { const c = this.children; return c[c.length - 1] || null; }

  addEventListener(type, fn) { (this.listeners[type] ||= []).push(fn); }
  removeEventListener(type, fn) { this.listeners[type] = (this.listeners[type] || []).filter((f) => f !== fn); }

  /** Test helper: invokes listeners with a minimal event object. */
  dispatch(type, extra = {}) {
    const ev = { type, target: this, currentTarget: this, preventDefault() {}, ...extra };
    for (const fn of this.listeners[type] || []) fn(ev);
    return ev;
  }
}

class FakeFragment extends FakeNode {
  constructor(doc) { super(doc, 11, '#document-fragment'); }
}

export class FakeDocument {
  createElement(tag) { return new FakeElement(this, tag); }
  createElementNS(_ns, tag) { return new FakeElement(this, tag); }
  createTextNode(v) { return new FakeText(this, v); }
  createComment(v) { return new FakeComment(this, v); }
  createDocumentFragment() { return new FakeFragment(this); }
}

export const doc = new FakeDocument();

const VOID = new Set(['area', 'base', 'br', 'col', 'embed', 'hr', 'img', 'input', 'link', 'meta', 'source', 'track', 'wbr']);
const esc = (s) => String(s).replace(/[&<>"]/g, (c) => ({ '&': '&amp;', '<': '&lt;', '>': '&gt;', '"': '&quot;' })[c]);

/** Serializes a fake node (or fragment) to HTML for assertions. */
export function toHTML(node) {
  if (node.nodeType === 3) return esc(node.nodeValue);
  if (node.nodeType === 8) return '';
  if (node.nodeType === 11) return node.childNodes.map(toHTML).join('');
  const tag = node.nodeName.toLowerCase();
  let s = '<' + tag;
  for (const [k, v] of node.attrs) s += ` ${k}="${esc(v)}"`;
  if (VOID.has(tag)) return s + '>';
  return s + '>' + node.childNodes.map(toHTML).join('') + `</${tag}>`;
}

const ENT = { amp: '&', lt: '<', gt: '>', quot: '"', apos: "'", nbsp: String.fromCharCode(0xa0), '#39': "'" };
const decode = (s) => s.replace(/&(#\d+|#x[0-9a-f]+|[a-z]+);/gi, (m, e) => {
  if (e[0] === '#') return String.fromCodePoint(e[1] === 'x' || e[1] === 'X' ? parseInt(e.slice(2), 16) : parseInt(e.slice(1), 10));
  return ENT[e] ?? m;
});

/**
 * Tiny tolerant HTML parser (tests only): returns a <body>-like element. Handles comments,
 * void elements, quoted/unquoted attributes and implicit closing of unclosed tags.
 */
export function parseHTML(html) {
  const body = doc.createElement('body');
  const stack = [body];
  const re = /<!--[\s\S]*?-->|<!\[CDATA\[[\s\S]*?\]\]>|<!doctype[^>]*>|<\/([a-zA-Z][\w:-]*)\s*>|<([a-zA-Z][\w:-]*)((?:\s+[^\s=>/]+(?:\s*=\s*(?:"[^"]*"|'[^']*'|[^\s>]+))?)*)\s*\/?>|[^<]+|</g;
  let m;
  while ((m = re.exec(html))) {
    const top = stack[stack.length - 1];
    if (m[0].startsWith('<!--')) { top.appendChild(doc.createComment(m[0].slice(4, -3))); continue; }
    if (m[0].startsWith('<!')) continue;
    if (m[1]) {
      const name = m[1].toUpperCase();
      const i = stack.map((e) => e.nodeName).lastIndexOf(name);
      if (i > 0) stack.length = i;
      continue;
    }
    if (m[2]) {
      const el = doc.createElement(m[2]);
      const attrRe = /([^\s=>/]+)(?:\s*=\s*(?:"([^"]*)"|'([^']*)'|([^\s>]+)))?/g;
      let a;
      while ((a = attrRe.exec(m[3] || ''))) el.setAttribute(a[1], decode(a[2] ?? a[3] ?? a[4] ?? ''));
      top.appendChild(el);
      if (!VOID.has(m[2].toLowerCase())) stack.push(el);
      continue;
    }
    top.appendChild(doc.createTextNode(decode(m[0])));
  }
  return body;
}
