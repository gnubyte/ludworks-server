// DOM building helpers. Everything in the UI is built with these instead of
// innerHTML so user data can never be interpreted as markup (strict CSP, XSS).

const SVG_NS = 'http://www.w3.org/2000/svg';

// Keys assigned as DOM properties rather than attributes (live state).
const PROP_KEYS = new Set([
  'value', 'checked', 'selected', 'disabled', 'hidden', 'indeterminate', 'multiple',
  'readOnly', 'required', 'tabIndex', 'defaultValue', 'defaultChecked', 'open',
]);

// Attributes that take URLs: neutralise script URLs as defence in depth.
const URL_ATTRS = new Set(['href', 'src', 'action', 'formaction', 'xlink:href', 'poster']);
const UNSAFE_URL = /^\s*(javascript|vbscript|data:text\/html)/i;

/**
 * Build a class string from strings, arrays and {name: bool} objects.
 * @param {...any} args
 * @returns {string}
 */
export function cls(...args) {
  const out = [];
  const walk = (v) => {
    if (!v) return;
    if (typeof v === 'string') out.push(v);
    else if (Array.isArray(v)) v.forEach(walk);
    else if (typeof v === 'object') for (const [k, on] of Object.entries(v)) if (on) out.push(k);
  };
  args.forEach(walk);
  return out.join(' ');
}

function isPlainProps(v) {
  return v != null && typeof v === 'object' && !Array.isArray(v) && !(typeof Node !== 'undefined' && v instanceof Node);
}

/**
 * Apply props to an element. See h() for the supported keys.
 * @param {Element} el
 * @param {Record<string, any>} props
 */
export function setProps(el, props) {
  for (const [key, value] of Object.entries(props)) {
    if (key === 'innerHTML' || key === 'outerHTML' || key === 'html') {
      throw new Error('dom: raw HTML is not allowed');
    }
    if (value === undefined || value === null) continue;
    switch (key) {
      case 'class':
      case 'className': {
        const c = cls(value);
        if (c) el.setAttribute('class', c);
        break;
      }
      case 'dataset':
        for (const [k, v] of Object.entries(value)) if (v != null) el.dataset[k] = String(v);
        break;
      case 'style':
        if (typeof value !== 'object') throw new TypeError('dom: style must be an object (CSP forbids style attributes)');
        for (const [k, v] of Object.entries(value)) {
          if (v == null) continue;
          if (k.startsWith('--')) el.style.setProperty(k, String(v));
          else el.style[k] = v;
        }
        break;
      case 'aria':
        for (const [k, v] of Object.entries(value)) {
          if (v == null) continue;
          el.setAttribute(`aria-${k}`, v === true ? 'true' : v === false ? 'false' : String(v));
        }
        break;
      case 'on':
        for (const [type, fn] of Object.entries(value)) if (fn) el.addEventListener(type, fn);
        break;
      case 'ref':
        value(el);
        break;
      case 'text':
        el.textContent = String(value);
        break;
      default:
        if (key.length > 2 && key.startsWith('on') && typeof value === 'function') {
          el.addEventListener(key.slice(2).toLowerCase(), value);
        } else if (PROP_KEYS.has(key)) {
          el[key] = value;
        } else if (value === false) {
          // boolean attribute off
        } else if (value === true) {
          el.setAttribute(key, '');
        } else {
          let v = String(value);
          if (URL_ATTRS.has(key) && UNSAFE_URL.test(v.replace(/[\u0000-\u001f]/g, ''))) v = '#';
          el.setAttribute(key, v);
        }
    }
  }
}

/**
 * Append children: strings/numbers become text nodes; arrays are flattened;
 * null/undefined/booleans are skipped.
 * @param {Node} parent
 * @param {any[]} children
 */
export function append(parent, ...children) {
  for (const c of children) {
    if (c == null || c === false || c === true) continue;
    if (Array.isArray(c)) append(parent, ...c);
    else if (typeof c === 'string' || typeof c === 'number' || typeof c === 'bigint') parent.appendChild(document.createTextNode(String(c)));
    else parent.appendChild(c);
  }
  return parent;
}

/**
 * Create an element.
 *   h('button', {class: ['btn', {active}], onClick, aria: {label: 'Close'}, dataset: {id}}, 'Text', child)
 * Props: class/className (string|array|object), dataset, style (object, CSSOM only),
 * aria, on {type: fn}, onX handlers (event name lowercased), ref(el), text,
 * live properties (value, checked, disabled...), other keys → attributes.
 * Props may be omitted: h('p', 'hello').
 * @param {string} tag
 * @param {object|any} [props]
 * @param {...any} children
 * @returns {HTMLElement}
 */
export function h(tag, props, ...children) {
  if (!isPlainProps(props)) {
    if (props !== undefined) children.unshift(props);
    props = {};
  }
  const el = props.ns === 'svg' || tag === 'svg' ? document.createElementNS(SVG_NS, tag) : document.createElement(tag);
  const { ns, ...rest } = props;
  setProps(el, rest);
  append(el, ...children);
  return el;
}

/** Create an SVG element in the SVG namespace. */
export function s(tag, attrs = {}, ...children) {
  const el = document.createElementNS(SVG_NS, tag);
  for (const [k, v] of Object.entries(attrs)) {
    if (v == null || v === false) continue;
    if (k === 'class') el.setAttribute('class', cls(v));
    else if (k === 'style') {
      for (const [sk, sv] of Object.entries(v)) {
        if (sk.startsWith('--')) el.style.setProperty(sk, String(sv));
        else el.style[sk] = sv;
      }
    } else if (k === 'text') el.textContent = String(v);
    else el.setAttribute(k, String(v));
  }
  append(el, ...children);
  return el;
}

/** A DocumentFragment from children. */
export function frag(...children) {
  return append(document.createDocumentFragment(), ...children);
}

/** Remove all children. */
export function clear(el) {
  el.replaceChildren();
  return el;
}

/** Replace all children of el. */
export function mount(el, ...children) {
  el.replaceChildren();
  return append(el, ...children);
}

/**
 * addEventListener that returns its own remover.
 * @returns {() => void}
 */
export function on(target, type, handler, opts) {
  target.addEventListener(type, handler, opts);
  return () => target.removeEventListener(type, handler, opts);
}

/**
 * Delegated event listener: handler(event, matchedElement).
 * @returns {() => void}
 */
export function delegate(root, type, selector, handler, opts) {
  return on(root, type, (e) => {
    const t = e.target instanceof Element ? e.target.closest(selector) : null;
    if (t && root.contains(t)) handler(e, t);
  }, opts);
}

export const qs = (sel, root = document) => root.querySelector(sel);
export const qsa = (sel, root = document) => Array.from(root.querySelectorAll(sel));

let uidCounter = 0;
/** Unique id for wiring labels/ARIA. */
export function uid(prefix = 'lw') {
  uidCounter += 1;
  return `${prefix}-${uidCounter.toString(36)}`;
}

/** True when the element is a text-editing control. */
export function isEditable(el) {
  if (!el || !(el instanceof Element)) return false;
  if (el.isContentEditable) return true;
  const tag = el.tagName;
  if (tag === 'TEXTAREA' || tag === 'SELECT') return true;
  if (tag === 'INPUT') {
    const t = (el.getAttribute('type') || 'text').toLowerCase();
    return !['checkbox', 'radio', 'button', 'submit', 'reset', 'range', 'color', 'file'].includes(t);
  }
  return false;
}

/** Debounce a function. The returned function has .cancel(). */
export function debounce(fn, ms = 200) {
  let t = null;
  const d = (...args) => {
    clearTimeout(t);
    t = setTimeout(() => fn(...args), ms);
  };
  d.cancel = () => clearTimeout(t);
  return d;
}

/** Trigger a browser download of a Blob with a filename. */
export function downloadBlob(blob, filename) {
  const url = URL.createObjectURL(blob);
  const a = h('a', { href: url, download: filename, class: 'hidden' });
  document.body.appendChild(a);
  a.click();
  a.remove();
  setTimeout(() => URL.revokeObjectURL(url), 30_000);
}

/** Open a hidden file picker; resolves with the chosen File[] (possibly empty). */
export function pickFiles({ multiple = true, accept = '', directory = false } = {}) {
  return new Promise((resolve) => {
    const input = h('input', { type: 'file', class: 'hidden', multiple, accept: accept || undefined });
    if (directory) input.setAttribute('webkitdirectory', '');
    let done = false;
    const finish = () => {
      if (done) return;
      done = true;
      resolve(Array.from(input.files || []));
      input.remove();
    };
    input.addEventListener('change', finish, { once: true });
    input.addEventListener('cancel', finish, { once: true });
    document.body.appendChild(input);
    input.click();
  });
}
