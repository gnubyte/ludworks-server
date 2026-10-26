// Small DOM for running app view code (core/dom.js h(), core/ui.js widgets) under node --test.
// Implements only what those modules touch: elements, text nodes, attributes, classList,
// dataset, style, events and simple selectors ('.a.b', 'tag', 'tag.a', '[attr]').
// Install it before importing any module that builds DOM: `installDom()`.

class Node {
  constructor(doc) {
    this.ownerDocument = doc;
    this.parentNode = null;
    this.childNodes = [];
    this._listeners = new Map();
  }

  get parentElement() { return this.parentNode && this.parentNode.nodeType === 1 ? this.parentNode : null; }
  get firstChild() { return this.childNodes[0] || null; }
  get isConnected() { let n = this; while (n.parentNode) n = n.parentNode; return n === this.ownerDocument; }

  appendChild(n) { return this.insertBefore(n, null); }

  insertBefore(n, ref) {
    if (n.nodeType === 11) {
      for (const c of [...n.childNodes]) this.insertBefore(c, ref);
      return n;
    }
    n.remove?.();
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

  remove() { if (this.parentNode) this.parentNode.removeChild(this); }
  contains(n) { for (let x = n; x; x = x.parentNode) if (x === this) return true; return false; }

  append(...nodes) { for (const n of nodes) this.appendChild(typeof n === 'string' ? this.ownerDocument.createTextNode(n) : n); }
  prepend(...nodes) { const first = this.firstChild; for (const n of nodes) this.insertBefore(typeof n === 'string' ? this.ownerDocument.createTextNode(n) : n, first); }
  replaceChildren(...nodes) { for (const c of [...this.childNodes]) this.removeChild(c); this.append(...nodes); }
  replaceWith(n) { const p = this.parentNode; if (p) { p.insertBefore(n, this); p.removeChild(this); } }
  after(n) { const p = this.parentNode; if (p) p.insertBefore(n, p.childNodes[p.childNodes.indexOf(this) + 1] || null); }
  before(n) { if (this.parentNode) this.parentNode.insertBefore(n, this); }

  get textContent() { return this.childNodes.map((c) => c.textContent).join(''); }
  set textContent(v) { this.replaceChildren(); if (v !== '' && v != null) this.appendChild(this.ownerDocument.createTextNode(String(v))); }

  addEventListener(type, fn, opts) {
    if (!fn) return;
    const list = this._listeners.get(type) || [];
    list.push({ fn, once: !!(opts && opts.once) });
    this._listeners.set(type, list);
  }

  removeEventListener(type, fn) {
    const list = this._listeners.get(type) || [];
    this._listeners.set(type, list.filter((l) => l.fn !== fn));
  }

  dispatchEvent(ev) {
    if (!ev.target) ev.target = this;
    ev.currentTarget = this;
    for (const l of [...(this._listeners.get(ev.type) || [])]) {
      if (l.once) this.removeEventListener(ev.type, l.fn);
      l.fn.call(this, ev);
      if (ev._stopImmediate) break;
    }
    if (ev.bubbles && !ev._stop && this.parentNode) this.parentNode.dispatchEvent(ev);
    return !ev.defaultPrevented;
  }
}

class Text extends Node {
  constructor(doc, data) { super(doc); this.nodeType = 3; this.nodeName = '#text'; this.data = data; }
  get textContent() { return this.data; }
  set textContent(v) { this.data = String(v); }
}

class Fragment extends Node {
  constructor(doc) { super(doc); this.nodeType = 11; this.nodeName = '#document-fragment'; }
}

function classListOf(el) {
  const read = () => (el.getAttribute('class') || '').split(/\s+/).filter(Boolean);
  const write = (arr) => el.setAttribute('class', arr.join(' '));
  return {
    add: (...c) => { const s = read(); for (const x of c) if (!s.includes(x)) s.push(x); write(s); },
    remove: (...c) => write(read().filter((x) => !c.includes(x))),
    contains: (c) => read().includes(c),
    toggle: (c, force) => {
      const on = force === undefined ? !read().includes(c) : !!force;
      if (on) { const s = read(); if (!s.includes(c)) s.push(c); write(s); } else write(read().filter((x) => x !== c));
      return on;
    },
    get length() { return read().length; },
  };
}

class Element extends Node {
  constructor(doc, tag, ns = null) {
    super(doc);
    this.nodeType = 1;
    this.namespaceURI = ns;
    this.tagName = ns ? tag : tag.toUpperCase();
    this.nodeName = this.tagName;
    this.localName = tag.toLowerCase();
    this.attributes = new Map();
    this.classList = classListOf(this);
    this.dataset = {};
    const styleProps = new Map();
    this.style = { setProperty: (k, v) => styleProps.set(k, v), getPropertyValue: (k) => styleProps.get(k) ?? '', removeProperty: (k) => styleProps.delete(k) };
    this.hidden = false;
    this.disabled = false;
    this.checked = false;
    this.indeterminate = false;
    this.value = '';
  }

  get children() { return this.childNodes.filter((c) => c.nodeType === 1); }
  get className() { return this.getAttribute('class') || ''; }
  set className(v) { this.setAttribute('class', v); }
  get id() { return this.getAttribute('id') || ''; }
  set id(v) { this.setAttribute('id', v); }

  setAttribute(k, v) { this.attributes.set(k, String(v)); }
  getAttribute(k) { return this.attributes.has(k) ? this.attributes.get(k) : null; }
  hasAttribute(k) { return this.attributes.has(k); }
  removeAttribute(k) { this.attributes.delete(k); }
  toggleAttribute(k, force) { const on = force === undefined ? !this.hasAttribute(k) : force; if (on) this.setAttribute(k, ''); else this.removeAttribute(k); return on; }

  focus() { this.ownerDocument.activeElement = this; }
  blur() { if (this.ownerDocument.activeElement === this) this.ownerDocument.activeElement = this.ownerDocument.body; }
  click() { this.dispatchEvent(new Event('click', { bubbles: true })); }
  getBoundingClientRect() { return { x: 0, y: 0, left: 0, top: 0, right: 0, bottom: 0, width: 0, height: 0 }; }
  scrollIntoView() {}
  closest(sel) { for (let n = this; n && n.nodeType === 1; n = n.parentNode) if (matches(n, sel)) return n; return null; }
  matches(sel) { return matches(this, sel); }

  querySelectorAll(sel) {
    const out = [];
    const walk = (n) => { for (const c of n.children) { if (sel.split(',').some((s) => matches(c, s.trim()))) out.push(c); walk(c); } };
    walk(this);
    return out;
  }

  querySelector(sel) { return this.querySelectorAll(sel)[0] || null; }
}

// Simple selectors only: tag, .class, [attr], [attr="v"], combined without spaces.
function matches(el, sel) {
  const m = sel.match(/^([a-z0-9-]*)((?:\.[\w-]+)*)((?:\[[^\]]+\])*)$/i);
  if (!m) throw new Error(`minidom: unsupported selector ${sel}`);
  if (m[1] && el.localName !== m[1].toLowerCase()) return false;
  for (const c of m[2].split('.').filter(Boolean)) if (!el.classList.contains(c)) return false;
  for (const a of m[3].match(/\[[^\]]+\]/g) || []) {
    const [, k, v] = a.match(/^\[([^=\]]+)(?:="?([^"\]]*)"?)?\]$/);
    if (!el.hasAttribute(k)) return false;
    if (v !== undefined && el.getAttribute(k) !== v) return false;
  }
  return true;
}

class Event {
  constructor(type, init = {}) {
    this.type = type;
    this.bubbles = !!init.bubbles;
    this.defaultPrevented = false;
    Object.assign(this, init);
  }

  preventDefault() { this.defaultPrevented = true; }
  stopPropagation() { this._stop = true; }
  stopImmediatePropagation() { this._stop = true; this._stopImmediate = true; }
}

class Document extends Node {
  constructor() {
    super(null);
    this.ownerDocument = this;
    this.nodeType = 9;
    this.documentElement = new Element(this, 'html');
    this.body = new Element(this, 'body');
    this.head = new Element(this, 'head');
    this.appendChild(this.documentElement);
    this.documentElement.append(this.head, this.body);
    this.activeElement = this.body;
  }

  createElement(tag) { return new Element(this, tag); }
  createElementNS(ns, tag) { return new Element(this, tag, ns); }
  createTextNode(data) { return new Text(this, String(data)); }
  createDocumentFragment() { return new Fragment(this); }
  querySelectorAll(sel) { return this.documentElement.querySelectorAll(sel); }
  querySelector(sel) { return this.documentElement.querySelector(sel); }
  getElementById(id) { return this.querySelector(`[id="${id}"]`); }
}

/** Install a fresh document and the few globals the UI modules reference. */
export function installDom() {
  const doc = new Document();
  globalThis.document = doc;
  globalThis.Node = Node;
  globalThis.Element = Element;
  globalThis.HTMLElement = Element;
  globalThis.Event = Event; // node's own Event has a read-only target; tests dispatch these
  globalThis.window = globalThis.window || globalThis;
  globalThis.requestAnimationFrame = globalThis.requestAnimationFrame || ((fn) => setTimeout(() => fn(Date.now()), 0));
  return doc;
}

/** Wait for pending promise callbacks and zero-delay timers. */
export async function settle(rounds = 5) {
  for (let i = 0; i < rounds; i++) await new Promise((r) => setTimeout(r, 0));
}
