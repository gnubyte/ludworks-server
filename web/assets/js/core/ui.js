// UI component library: buttons, toasts, modals/drawers, confirm/prompt, menus,
// tooltips, tabs, data tables, pagination, empty/loading states and small widgets.
// Everything is DOM-built (no innerHTML) and keyboard accessible.

import { h, cls, uid, on } from './dom.js';
import { icon } from './icons.js';
import { formatRelative, formatDateTime, initials, hueFor } from './format.js';

/* ------------------------------------------------------------------ buttons */

/**
 * Button factory.
 * @param {{label?: string|Node, icon?: string, iconRight?: string, variant?: 'primary'|'secondary'|'ghost'|'danger'|'danger-ghost',
 *          size?: 'sm'|'md'|'lg', onClick?: (e: MouseEvent) => any, type?: string, disabled?: boolean,
 *          title?: string, ariaLabel?: string, className?: string, block?: boolean, href?: string}} o
 *   With an icon and no label the button becomes square (aria-label = title).
 *   onClick returning a promise shows a loading state until it settles.
 */
export function button(o = {}) {
  const { label, icon: ic, iconRight, variant = 'secondary', size = 'md', onClick, type = 'button', disabled = false,
    title, ariaLabel, className, block = false, href, autofocus = false, form, target } = o;
  const iconOnly = ic && (label === undefined || label === null || label === '');
  const props = {
    class: cls('btn', `btn-${variant}`, size !== 'md' && `btn-${size}`, { 'btn-icon': iconOnly, 'btn-block': block }, className),
    aria: { label: ariaLabel || (iconOnly ? title : undefined) },
    dataset: title && iconOnly ? { tip: title } : undefined,
    title: title && !iconOnly ? title : undefined,
    autofocus, form,
  };
  const children = [ic ? icon(ic, { size: size === 'sm' ? 14 : 16 }) : null, iconOnly ? null : (typeof label === 'string' ? h('span', { class: 'btn-label' }, label) : label), iconRight ? icon(iconRight, { size: 14, className: 'btn-icon-right' }) : null];
  const el = href
    ? h('a', { ...props, href, target, rel: target === '_blank' ? 'noopener noreferrer' : undefined }, children)
    : h('button', { ...props, type, disabled }, children);
  if (onClick) {
    el.addEventListener('click', (e) => {
      if (el.classList.contains('is-loading')) return;
      const r = onClick(e);
      if (r && typeof r.then === 'function') {
        setLoading(el, true);
        r.catch(() => {}).finally(() => setLoading(el, false));
      }
    });
  }
  return el;
}

/** Toggle a button's loading state (keeps its width; blocks clicks). */
export function setLoading(btn, loading) {
  btn.classList.toggle('is-loading', !!loading);
  btn.setAttribute('aria-busy', loading ? 'true' : 'false');
  if (btn.tagName === 'BUTTON') {
    if (loading) {
      btn.dataset.wasDisabled = btn.disabled ? '1' : '';
      btn.disabled = true;
    } else {
      btn.disabled = btn.dataset.wasDisabled === '1';
      delete btn.dataset.wasDisabled;
    }
  }
}

/** Run fn with a button in loading state. */
export async function withLoading(btn, fn) {
  setLoading(btn, true);
  try {
    return await fn();
  } finally {
    setLoading(btn, false);
  }
}

/* ------------------------------------------------------------------- toasts */

let toastRegion = null;
function region() {
  if (!toastRegion || !toastRegion.isConnected) {
    toastRegion = h('div', { class: 'toast-region', aria: { live: 'polite', relevant: 'additions' } });
    document.body.appendChild(toastRegion);
  }
  return toastRegion;
}

/** A friendly message for any error (ApiError or otherwise). */
export function errorMessage(err) {
  if (!err) return 'Something went wrong.';
  if (typeof err === 'string') return err;
  if (err.code === 'rate_limited' && err.retryAfter) return `${err.message} Try again in ${err.retryAfter}s.`;
  if (err.fields) {
    const msgs = Object.values(err.fields).filter(Boolean);
    if (msgs.length) return msgs.join(' ');
  }
  return err.message || 'Something went wrong.';
}

/**
 * Show a toast.
 * @param {string} message
 * @param {{type?: 'info'|'success'|'error'|'warning', description?: string, timeout?: number,
 *          action?: {label: string, onClick: () => void}}} [opts]
 * @returns {() => void} dismiss
 */
export function toast(message, opts = {}) {
  const { type = 'info', description = '', action = null } = opts;
  const timeout = opts.timeout ?? (type === 'error' ? 8000 : 4500);
  const ic = { info: 'info', success: 'check-circle', error: 'alert-circle', warning: 'alert' }[type] || 'info';
  let timer = null;
  const el = h('div', { class: cls('toast', `toast-${type}`), role: type === 'error' ? 'alert' : 'status' },
    icon(ic, { className: 'toast-icon', size: 18 }),
    h('div', { class: 'toast-content' },
      h('div', { class: 'toast-title' }, message),
      description ? h('div', { class: 'toast-desc' }, description) : null),
    h('div', { class: 'toast-actions' },
      action ? h('button', { class: 'btn btn-ghost btn-sm', type: 'button', onClick: () => { action.onClick(); dismiss(); } }, action.label) : null,
      h('button', { class: 'btn btn-ghost btn-icon btn-sm toast-close', type: 'button', aria: { label: 'Dismiss' }, onClick: () => dismiss() }, icon('x', 14))));
  const dismiss = () => {
    clearTimeout(timer);
    if (!el.isConnected) return;
    el.classList.add('is-leaving');
    setTimeout(() => el.remove(), 160);
  };
  const arm = () => {
    if (timeout > 0) timer = setTimeout(dismiss, timeout);
  };
  el.addEventListener('pointerenter', () => clearTimeout(timer));
  el.addEventListener('pointerleave', arm);
  const r = region();
  r.appendChild(el);
  while (r.children.length > 5) r.firstElementChild.remove();
  arm();
  return dismiss;
}
toast.success = (m, o) => toast(m, { ...o, type: 'success' });
toast.info = (m, o) => toast(m, { ...o, type: 'info' });
toast.warning = (m, o) => toast(m, { ...o, type: 'warning' });
/** Error toast from an Error/ApiError or string; abort errors are ignored. */
toast.error = (err, o) => (err?.name === 'AbortError' ? () => {} : toast(errorMessage(err), { ...o, type: 'error' }));

/* -------------------------------------------------------- modals & drawers */

const modalStack = [];

function focusables(root) {
  return Array.from(root.querySelectorAll(
    'a[href],button:not([disabled]),input:not([disabled]):not([type="hidden"]),select:not([disabled]),textarea:not([disabled]),[tabindex]:not([tabindex="-1"]),[contenteditable="true"]',
  )).filter((el) => el.getClientRects().length > 0);
}

function syncInert() {
  const app = document.getElementById('app');
  if (app) app.inert = modalStack.length > 0;
  modalStack.forEach((m, i) => { m.backdrop.inert = i !== modalStack.length - 1; });
  document.documentElement.classList.toggle('modal-open', modalStack.length > 0);
}

function onModalKeydown(e) {
  const top = modalStack[modalStack.length - 1];
  if (!top) return;
  if (e.key === 'Escape' && top.dismissible && !e.defaultPrevented) {
    e.preventDefault();
    top.close(undefined);
  } else if (e.key === 'Tab') {
    const items = focusables(top.dialog);
    if (items.length === 0) {
      e.preventDefault();
      top.dialog.focus();
      return;
    }
    const first = items[0];
    const last = items[items.length - 1];
    if (e.shiftKey && (document.activeElement === first || !top.dialog.contains(document.activeElement))) {
      e.preventDefault();
      last.focus();
    } else if (!e.shiftKey && (document.activeElement === last || !top.dialog.contains(document.activeElement))) {
      e.preventDefault();
      first.focus();
    }
  }
}
// Guarded so pure helpers in this module can be imported under node for tests.
if (typeof document !== 'undefined') document.addEventListener('keydown', onModalKeydown);

/**
 * Open a modal dialog (or a drawer with kind:'drawer').
 * @param {{title?: string, description?: string, body?: Node|((m) => Node), footer?: Array<Node|object>,
 *          size?: 'sm'|'md'|'lg'|'xl', dismissible?: boolean, onClose?: (result) => void,
 *          initialFocus?: string|HTMLElement, className?: string, kind?: 'modal'|'drawer',
 *          onSubmit?: (m, form: HTMLFormElement) => any}} opts
 *   footer items may be Nodes or {label, variant, type, value, onClick(m, btn), disabled, icon}:
 *   a spec without onClick closes the modal with `value`; type:'submit' submits the form.
 *   With onSubmit the body+footer are wrapped in a <form>; returning false keeps it open,
 *   anything else closes the modal with that value.
 * @returns {{el: HTMLElement, body: HTMLElement, footer: HTMLElement, buttons: HTMLElement[],
 *            closed: Promise<any>, close: (result?: any) => void, setBusy: (b: boolean) => void,
 *            form: HTMLFormElement|null}}
 */
export function modal(opts = {}) {
  const { title = '', description = '', body = null, footer = null, size = 'md', dismissible = true, onClose = null,
    initialFocus = null, className = '', kind = 'modal', onSubmit = null } = opts;
  const prevFocus = document.activeElement;
  let resolveClosed;
  const closed = new Promise((r) => { resolveClosed = r; });
  const titleId = uid('mt');
  const descId = uid('md');
  const bodyEl = h('div', { class: 'modal-body' });
  const footerEl = h('div', { class: 'modal-footer' });
  let isClosed = false;

  const m = {
    el: null, body: bodyEl, footer: footerEl, buttons: [], closed, form: null, dismissible, backdrop: null, dialog: null,
    close(result) {
      if (isClosed) return;
      isClosed = true;
      const i = modalStack.indexOf(m);
      if (i >= 0) modalStack.splice(i, 1);
      m.backdrop.classList.add('is-leaving');
      setTimeout(() => m.backdrop.remove(), 140);
      syncInert();
      if (prevFocus && prevFocus.isConnected && typeof prevFocus.focus === 'function') prevFocus.focus({ preventScroll: true });
      onClose?.(result);
      resolveClosed(result);
    },
    setBusy(busy) {
      m.dialog.setAttribute('aria-busy', busy ? 'true' : 'false');
      for (const b of m.buttons) b.disabled = !!busy || b.dataset.locked === '1';
    },
    get isOpen() { return !isClosed; },
  };

  const closeBtn = dismissible
    ? h('button', { class: 'btn btn-ghost btn-icon btn-sm modal-close', type: 'button', aria: { label: 'Close' }, onClick: () => m.close(undefined) }, icon('x'))
    : null;

  const content = typeof body === 'function' ? body(m) : body;
  if (content) bodyEl.append(content);

  for (const item of footer || []) {
    if (!item) continue;
    if (item instanceof Node) {
      footerEl.append(item);
      continue;
    }
    const b = button({
      label: item.label, icon: item.icon, variant: item.variant || 'secondary', type: item.type || 'button',
      disabled: !!item.disabled, autofocus: !!item.autofocus,
    });
    if (item.disabled) b.dataset.locked = '1';
    if (item.type !== 'submit') {
      b.addEventListener('click', async () => {
        if (item.onClick) {
          setLoading(b, true);
          try {
            await item.onClick(m, b);
          } catch (err) {
            toast.error(err);
          } finally {
            if (b.isConnected) setLoading(b, false);
          }
        } else {
          m.close(item.value);
        }
      });
    }
    m.buttons.push(b);
    footerEl.append(b);
  }
  if (!footerEl.childElementCount) footerEl.hidden = true;

  let inner;
  if (onSubmit) {
    const form = h('form', { class: 'modal-form', novalidate: true }, bodyEl, footerEl);
    m.form = form;
    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      const submitBtn = m.buttons.find((b) => b.type === 'submit');
      if (submitBtn?.classList.contains('is-loading')) return;
      if (!form.checkValidity()) {
        form.reportValidity();
        return;
      }
      if (submitBtn) setLoading(submitBtn, true);
      try {
        const r = await onSubmit(m, form);
        if (r !== false && !isClosed) m.close(r);
      } catch (err) {
        toast.error(err);
      } finally {
        if (submitBtn?.isConnected) setLoading(submitBtn, false);
      }
    });
    inner = [form];
  } else {
    inner = [bodyEl, footerEl];
  }

  const header = title || closeBtn
    ? h('div', { class: 'modal-header' },
      h('div', { class: 'modal-heading' },
        title ? h('h2', { class: 'modal-title', id: titleId }, title) : null,
        description ? h('p', { class: 'modal-desc', id: descId }, description) : null),
      closeBtn)
    : null;

  m.dialog = h('div', {
    class: cls(kind === 'drawer' ? 'drawer' : 'modal', `modal-${size}`, className),
    role: 'dialog', tabindex: '-1',
    aria: { modal: 'true', labelledby: title ? titleId : undefined, describedby: description ? descId : undefined },
  }, header, inner);
  m.el = m.dialog;
  m.backdrop = h('div', { class: kind === 'drawer' ? 'drawer-backdrop' : 'modal-backdrop' }, m.dialog);
  let downOnBackdrop = false;
  m.backdrop.addEventListener('pointerdown', (e) => { downOnBackdrop = e.target === m.backdrop; });
  m.backdrop.addEventListener('click', (e) => {
    if (e.target === m.backdrop && downOnBackdrop && dismissible) m.close(undefined);
  });

  document.body.appendChild(m.backdrop);
  modalStack.push(m);
  syncInert();

  requestAnimationFrame(() => {
    let target = null;
    if (initialFocus) target = typeof initialFocus === 'string' ? m.dialog.querySelector(initialFocus) : initialFocus;
    target ||= m.dialog.querySelector('[autofocus]') || focusables(bodyEl).find((el) => el.matches('input,select,textarea'))
      || m.buttons.find((b) => b.classList.contains('btn-primary')) || m.dialog;
    target.focus({ preventScroll: true });
  });
  return m;
}

/** Drawer: a modal anchored to the right edge. */
export function drawer(opts) {
  return modal({ ...opts, kind: 'drawer' });
}

/**
 * Confirmation dialog.
 * @param {{title?: string, message?: string|Node, confirmLabel?: string, cancelLabel?: string,
 *          danger?: boolean, body?: Node, requireText?: string}} o
 *   requireText: the user must type this exact text to enable confirm (destructive actions).
 * @returns {Promise<boolean>}
 */
export function confirm(o = {}) {
  const { title = 'Are you sure?', message = '', confirmLabel = 'Confirm', cancelLabel = 'Cancel', danger = false, body = null, requireText = null } = o;
  let typed = null;
  const content = h('div', { class: 'stack-sm' },
    message ? (typeof message === 'string' ? h('p', { class: 'confirm-msg' }, message) : message) : null,
    body,
    requireText ? h('div', { class: 'field' },
      h('label', { class: 'field-label', for: (typed = h('input', { class: 'input', id: uid('cf'), autocomplete: 'off', spellcheck: 'false' })).id },
        'Type ', h('strong', { class: 'mono' }, requireText), ' to confirm'),
      typed) : null);
  const m = modal({
    title, body: content, size: 'sm', className: 'confirm-dialog',
    footer: [
      { label: cancelLabel, value: false, variant: 'secondary', autofocus: danger && !requireText },
      { label: confirmLabel, value: true, variant: danger ? 'danger' : 'primary', disabled: !!requireText, autofocus: !danger && !requireText },
    ],
  });
  if (typed) {
    const confirmBtn = m.buttons[1];
    typed.addEventListener('input', () => {
      const ok = typed.value === requireText;
      confirmBtn.disabled = !ok;
      confirmBtn.dataset.locked = ok ? '' : '1';
    });
    typed.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' && typed.value === requireText) m.close(true);
    });
  }
  return m.closed.then((v) => v === true);
}

/**
 * Text prompt dialog.
 * @param {{title?: string, message?: string, label?: string, value?: string, placeholder?: string, type?: string,
 *          confirmLabel?: string, validate?: (v: string) => string|null, required?: boolean, hint?: string,
 *          multiline?: boolean, autocomplete?: string}} o
 * @returns {Promise<string|null>} the value, or null when cancelled
 */
export function prompt(o = {}) {
  const { title = '', message = '', label = '', value = '', placeholder = '', type = 'text', confirmLabel = 'Save',
    validate = null, required = true, hint = '', multiline = false, autocomplete = 'off' } = o;
  const id = uid('pr');
  const ctl = multiline
    ? h('textarea', { class: 'textarea', id, placeholder, required, rows: 4 })
    : h('input', { class: 'input', id, type, placeholder, required, autocomplete, spellcheck: type === 'text' ? 'true' : 'false' });
  ctl.value = value;
  const err = h('div', { class: 'field-error', role: 'alert', hidden: true });
  const content = h('div', { class: 'stack-sm' },
    message ? h('p', { class: 'confirm-msg' }, message) : null,
    h('div', { class: 'field' }, label ? h('label', { class: 'field-label', for: id }, label) : null, ctl,
      hint ? h('div', { class: 'field-hint' }, hint) : null, err));
  if (!label) ctl.setAttribute('aria-label', title || 'Value');
  const m = modal({
    title, body: content, size: 'sm',
    footer: [{ label: 'Cancel', value: null }, { label: confirmLabel, variant: 'primary', type: 'submit' }],
    onSubmit: () => {
      const v = ctl.value;
      const problem = validate ? validate(v) : null;
      if (problem) {
        err.textContent = problem;
        err.hidden = false;
        ctl.setAttribute('aria-invalid', 'true');
        ctl.focus();
        return false;
      }
      return v;
    },
  });
  requestAnimationFrame(() => {
    if (!multiline && value) ctl.select();
  });
  return m.closed.then((v) => (typeof v === 'string' ? v : null));
}

/* -------------------------------------------------------------------- menus */

let activeMenu = null;

/** Close the open menu, if any. */
export function closeMenu() {
  activeMenu?.close();
}

/**
 * Open a menu.
 * @param {{items: Array<object>, anchor?: HTMLElement, x?: number, y?: number, align?: 'start'|'end',
 *          onClose?: () => void, className?: string, focusFirst?: boolean}} o
 *   items: {label, icon, onSelect, danger, disabled, shortcut, checked} | {separator:true} | {heading}
 */
export function openMenu(o) {
  const { items = [], anchor = null, x = 0, y = 0, align = 'start', onClose = null, className = '', focusFirst = true } = o;
  closeMenu();
  const menu = h('div', { class: cls('menu', className), role: 'menu', tabindex: '-1' });
  const entries = [];
  for (const it of items) {
    if (!it) continue;
    if (it.separator) {
      menu.append(h('div', { class: 'menu-sep', role: 'separator' }));
      continue;
    }
    if (it.heading) {
      menu.append(h('div', { class: 'menu-heading' }, it.heading));
      continue;
    }
    const hasCheck = it.checked !== undefined;
    const b = h('button', {
      class: cls('menu-item', { danger: it.danger }), type: 'button', tabindex: '-1',
      role: hasCheck ? 'menuitemcheckbox' : 'menuitem',
      aria: { disabled: it.disabled ? 'true' : undefined, checked: hasCheck ? String(!!it.checked) : undefined },
    },
    it.icon ? icon(it.icon, { className: 'menu-icon' }) : (hasCheck ? h('span', { class: 'menu-icon' }, it.checked ? icon('check') : null) : null),
    h('span', { class: 'menu-label' }, it.label),
    it.shortcut ? h('kbd', { class: 'menu-shortcut' }, it.shortcut) : null);
    b.addEventListener('click', (e) => {
      e.stopPropagation();
      if (it.disabled) return;
      api.close(true);
      it.onSelect?.();
    });
    b.addEventListener('pointermove', () => { if (document.activeElement !== b) b.focus({ preventScroll: true }); });
    entries.push({ el: b, item: it });
    menu.append(b);
  }
  document.body.appendChild(menu);

  // Position: below the anchor (flipping up if needed), clamped to the viewport.
  const mw = menu.offsetWidth;
  const mh = menu.offsetHeight;
  const vw = document.documentElement.clientWidth;
  const vh = window.innerHeight;
  let left;
  let top;
  if (anchor) {
    const r = anchor.getBoundingClientRect();
    left = align === 'end' ? r.right - mw : r.left;
    top = r.bottom + 4;
    if (top + mh > vh - 8 && r.top - mh - 4 > 8) top = r.top - mh - 4;
    anchor.setAttribute('aria-expanded', 'true');
  } else {
    left = x;
    top = y;
    if (left + mw > vw - 8) left = x - mw;
    if (top + mh > vh - 8) top = y - mh;
  }
  menu.style.left = `${Math.max(8, Math.min(left, vw - mw - 8))}px`;
  menu.style.top = `${Math.max(8, Math.min(top, vh - mh - 8))}px`;

  const enabled = () => entries.filter((e) => !e.item.disabled).map((e) => e.el);
  const move = (delta) => {
    const list = enabled();
    if (!list.length) return;
    const i = list.indexOf(document.activeElement);
    const next = i < 0 ? (delta > 0 ? 0 : list.length - 1) : (i + delta + list.length) % list.length;
    list[next].focus();
  };
  let typeahead = '';
  let typeTimer = null;
  menu.addEventListener('keydown', (e) => {
    if (e.key === 'ArrowDown') { e.preventDefault(); move(1); }
    else if (e.key === 'ArrowUp') { e.preventDefault(); move(-1); }
    else if (e.key === 'Home') { e.preventDefault(); enabled()[0]?.focus(); }
    else if (e.key === 'End') { e.preventDefault(); enabled().at(-1)?.focus(); }
    else if (e.key === 'Escape') { e.preventDefault(); e.stopPropagation(); api.close(true); }
    else if (e.key === 'Tab') { api.close(false); }
    else if (e.key.length === 1 && /\S/.test(e.key) && !e.ctrlKey && !e.metaKey) {
      typeahead += e.key.toLowerCase();
      clearTimeout(typeTimer);
      typeTimer = setTimeout(() => { typeahead = ''; }, 600);
      const match = entries.find((x) => !x.item.disabled && String(x.item.label).toLowerCase().startsWith(typeahead));
      match?.el.focus();
    }
  });

  const offs = [];
  const api = {
    el: menu, anchor,
    close(restoreFocus = false) {
      if (activeMenu !== api) return;
      activeMenu = null;
      offs.forEach((off) => off());
      menu.remove();
      anchor?.setAttribute('aria-expanded', 'false');
      if (restoreFocus && anchor) anchor.focus({ preventScroll: true });
      onClose?.();
    },
  };
  // Defer outside-click listeners so the opening click doesn't immediately close it.
  setTimeout(() => {
    if (activeMenu !== api) return;
    offs.push(on(document, 'pointerdown', (e) => {
      if (!menu.contains(e.target) && !(anchor && anchor.contains(e.target))) api.close(false);
    }, true));
    offs.push(on(window, 'resize', () => api.close(false)));
    offs.push(on(window, 'blur', () => api.close(false)));
    offs.push(on(document, 'scroll', (e) => { if (!menu.contains(e.target)) api.close(false); }, true));
  }, 0);
  activeMenu = api;
  if (focusFirst) requestAnimationFrame(() => enabled()[0]?.focus({ preventScroll: true }));
  else menu.focus({ preventScroll: true });
  return api;
}

/** Context menu at the pointer position (call from a contextmenu handler). */
export function contextMenu(event, items, opts = {}) {
  event.preventDefault();
  return openMenu({ ...opts, items, x: event.clientX, y: event.clientY });
}

/** Toggle a dropdown menu attached to an anchor element. */
export function dropdown(anchor, items, opts = {}) {
  if (activeMenu && activeMenu.anchor === anchor) {
    activeMenu.close(false);
    return null;
  }
  return openMenu({ ...opts, items: typeof items === 'function' ? items() : items, anchor });
}

/**
 * A button that opens a dropdown menu.
 * @param {{label?: string, icon?: string, items: Array|(() => Array), variant?: string, size?: string,
 *          align?: 'start'|'end', title?: string, className?: string, caret?: boolean}} o
 */
export function menuButton(o) {
  const { label, icon: ic, items, variant = 'ghost', size = 'md', align = 'end', title, className, caret = false } = o;
  const b = button({ label, icon: ic, iconRight: caret ? 'chevron-down' : undefined, variant, size, title, ariaLabel: title || label, className });
  b.setAttribute('aria-haspopup', 'menu');
  b.setAttribute('aria-expanded', 'false');
  b.addEventListener('click', () => dropdown(b, items, { align }));
  b.addEventListener('keydown', (e) => {
    if (e.key === 'ArrowDown' && b.getAttribute('aria-expanded') !== 'true') {
      e.preventDefault();
      dropdown(b, items, { align });
    }
  });
  return b;
}

/* ----------------------------------------------------------------- tooltips */

let tooltipsReady = false;
/**
 * Enable tooltips for every element with data-tip="text" (hover after a short
 * delay, or keyboard focus). Call once at startup.
 */
export function initTooltips() {
  if (tooltipsReady) return;
  tooltipsReady = true;
  let tip = null;
  let timer = null;
  let current = null;
  const hide = () => {
    clearTimeout(timer);
    current = null;
    tip?.remove();
    tip = null;
  };
  const show = (target) => {
    const text = target.dataset.tip;
    if (!text || !target.isConnected) return;
    tip = h('div', { class: 'tooltip', role: 'tooltip' }, text);
    document.body.appendChild(tip);
    const r = target.getBoundingClientRect();
    const tw = tip.offsetWidth;
    const th = tip.offsetHeight;
    const vw = document.documentElement.clientWidth;
    let top = r.top - th - 6;
    let placement = 'top';
    if (target.dataset.tipPlacement === 'right') {
      tip.style.left = `${Math.min(r.right + 8, vw - tw - 8)}px`;
      tip.style.top = `${Math.max(8, r.top + r.height / 2 - th / 2)}px`;
      tip.dataset.placement = 'right';
      return;
    }
    if (top < 8) {
      top = r.bottom + 6;
      placement = 'bottom';
    }
    tip.style.left = `${Math.max(8, Math.min(r.left + r.width / 2 - tw / 2, vw - tw - 8))}px`;
    tip.style.top = `${top}px`;
    tip.dataset.placement = placement;
  };
  document.addEventListener('pointerover', (e) => {
    const t = e.target instanceof Element ? e.target.closest('[data-tip]') : null;
    if (t === current) return;
    hide();
    if (!t || e.pointerType === 'touch') return;
    current = t;
    timer = setTimeout(() => show(t), 400);
  });
  document.addEventListener('pointerout', (e) => {
    if (current && !(e.relatedTarget instanceof Node && current.contains(e.relatedTarget))) hide();
  });
  document.addEventListener('focusin', (e) => {
    const t = e.target instanceof Element ? e.target.closest('[data-tip]') : null;
    hide();
    if (t && t.matches(':focus-visible')) {
      current = t;
      show(t);
    }
  });
  document.addEventListener('focusout', hide);
  document.addEventListener('pointerdown', hide, true);
  document.addEventListener('scroll', hide, true);
  document.addEventListener('keydown', (e) => { if (e.key === 'Escape') hide(); });
}

/* --------------------------------------------------------------------- tabs */

/**
 * Tabs. Items with `href` render as a link bar (route-driven, aria-current);
 * otherwise buttons with role=tab, roving focus and automatic activation.
 * @param {{items: Array<{id: string, label: string, icon?: string, badge?: string|number, href?: string}>,
 *          active?: string, onChange?: (id: string) => void, variant?: 'underline'|'pills'|'vertical',
 *          ariaLabel?: string}} o
 * @returns {{el: HTMLElement, setActive: (id: string) => void}}
 */
/**
 * Scroll a horizontally scrolling tab strip so its active tab is visible. Strips hide their
 * scrollbar, so on narrow screens the current section was otherwise off-screen. The element
 * that scrolls may be a wrapper (e.g. TV's .tv-tabs-inner) rather than the strip itself: the
 * strip or its nearest overflowing ancestor (up to three levels) is used.
 * @param {HTMLElement} strip
 */
export function revealActiveTab(strip) {
  const run = () => {
    const tab = strip?.querySelector?.('.tab.is-active');
    if (!tab) return;
    let sc = strip;
    for (let i = 0; sc && i < 3 && sc.scrollWidth <= sc.clientWidth; i++) sc = sc.parentElement;
    if (!sc || sc.scrollWidth <= sc.clientWidth) return;
    const s = sc.getBoundingClientRect();
    const t = tab.getBoundingClientRect();
    if (t.left < s.left) sc.scrollLeft -= s.left - t.left + 16;
    else if (t.right > s.right) sc.scrollLeft += t.right - s.right + 16;
  };
  // getBoundingClientRect forces layout, so this works synchronously once the strip is in the
  // document (no requestAnimationFrame: it never fires in a background tab).
  if (strip?.isConnected) run();
  else setTimeout(run, 0);
}

/**
 * Edge fades on a strip that overflows horizontally, so hidden tabs are discoverable without
 * a scrollbar: .fade-start / .fade-end are set while there is more content on that side.
 * @param {HTMLElement} el
 */
function syncTabFades(el) {
  const max = el.scrollWidth - el.clientWidth;
  el.classList.toggle('fade-start', max > 1 && el.scrollLeft > 1);
  el.classList.toggle('fade-end', max > 1 && el.scrollLeft < max - 1);
}

export function tabs(o) {
  const { items, active, onChange, variant = 'underline', ariaLabel = 'Sections' } = o;
  const isLinks = items.some((it) => it.href);
  const el = h(isLinks ? 'nav' : 'div', {
    class: cls('tabs', `tabs-${variant}`),
    role: isLinks ? undefined : 'tablist',
    aria: { label: ariaLabel, orientation: !isLinks && variant === 'vertical' ? 'vertical' : undefined },
  });
  const nodes = new Map();
  for (const it of items) {
    const kids = [it.icon ? icon(it.icon) : null, h('span', { class: 'tab-label' }, it.label),
      it.badge !== undefined && it.badge !== null && it.badge !== '' ? h('span', { class: 'tab-badge' }, String(it.badge)) : null];
    const n = isLinks
      ? h('a', { class: 'tab', href: it.href, dataset: { id: it.id } }, kids)
      : h('button', { class: 'tab', type: 'button', role: 'tab', dataset: { id: it.id }, id: it.tabId, aria: { controls: it.panelId } }, kids);
    if (!isLinks) {
      n.addEventListener('click', () => {
        api.setActive(it.id);
        onChange?.(it.id);
      });
    } else if (onChange) {
      n.addEventListener('click', () => onChange(it.id));
    }
    nodes.set(it.id, n);
    el.append(n);
  }
  if (!isLinks) {
    el.addEventListener('keydown', (e) => {
      const keysNext = variant === 'vertical' ? ['ArrowDown'] : ['ArrowRight'];
      const keysPrev = variant === 'vertical' ? ['ArrowUp'] : ['ArrowLeft'];
      const list = [...nodes.values()];
      const i = list.indexOf(document.activeElement);
      if (i < 0) return;
      let j = null;
      if (keysNext.includes(e.key)) j = (i + 1) % list.length;
      else if (keysPrev.includes(e.key)) j = (i - 1 + list.length) % list.length;
      else if (e.key === 'Home') j = 0;
      else if (e.key === 'End') j = list.length - 1;
      if (j === null) return;
      e.preventDefault();
      list[j].focus();
      list[j].click();
    });
  }
  const api = {
    el,
    setActive(id) {
      for (const [key, n] of nodes) {
        const sel = key === id;
        n.classList.toggle('is-active', sel);
        if (isLinks) {
          if (sel) n.setAttribute('aria-current', 'page');
          else n.removeAttribute('aria-current');
        } else {
          n.setAttribute('aria-selected', String(sel));
          n.tabIndex = sel ? 0 : -1;
        }
      }
      revealActiveTab(el);
      setTimeout(() => syncTabFades(el), 0);
    },
  };
  el.addEventListener('scroll', () => syncTabFades(el), { passive: true });
  if (typeof ResizeObserver === 'function') new ResizeObserver(() => syncTabFades(el)).observe(el);
  api.setActive(active ?? items[0]?.id);
  return api;
}

/* --------------------------------------------------------------- data table */

function compareValues(a, b) {
  const an = a === null || a === undefined || a === '';
  const bn = b === null || b === undefined || b === '';
  if (an && bn) return 0;
  if (an) return 1;
  if (bn) return -1;
  if (typeof a === 'number' && typeof b === 'number') return a - b;
  if (typeof a === 'boolean' && typeof b === 'boolean') return Number(a) - Number(b);
  return String(a).localeCompare(String(b), undefined, { numeric: true, sensitivity: 'base' });
}

/**
 * Sort rows by a column (pure; exported for tests).
 * @param {object[]} rows
 * @param {{key: string, sortValue?: (row) => any}} col
 * @param {'asc'|'desc'} dir
 */
export function sortRows(rows, col, dir) {
  const get = col.sortValue || ((r) => r[col.key]);
  const mul = dir === 'desc' ? -1 : 1;
  return rows.map((r, i) => [r, i]).sort((x, y) => {
    const c = compareValues(get(x[0]), get(y[0]));
    // Empty values always last; stable for ties.
    if (c !== 0) {
      const xe = get(x[0]); const ye = get(y[0]);
      const xEmpty = xe === null || xe === undefined || xe === '';
      const yEmpty = ye === null || ye === undefined || ye === '';
      if (xEmpty !== yEmpty) return xEmpty ? 1 : -1;
      return c * mul;
    }
    return x[1] - y[1];
  }).map((p) => p[0]);
}

/**
 * Data table with sortable headers, sticky header, optional row selection.
 * @param {{columns: Array<{key: string, label: string, sortable?: boolean, sortValue?: (row) => any,
 *            render?: (row) => any, align?: 'left'|'right'|'center', width?: string, className?: string,
 *            hideBelow?: 'sm'|'md'}>,
 *          rows?: object[], rowKey?: string|((row) => string), selectable?: boolean,
 *          sort?: {key: string, dir: 'asc'|'desc'}|null, onSort?: (s) => void,
 *          onRowClick?: (row, e) => void, onRowContextMenu?: (row, e) => void, empty?: Node|(() => Node),
 *          rowClass?: (row) => string, caption?: string, maxHeight?: string, className?: string,
 *          onSelectionChange?: (rows: object[]) => void}} o
 *   With onSort, sorting is server-side (only the header indicator changes).
 */
export function dataTable(o) {
  const { columns, rowKey = 'id', selectable = false, onSort = null, onRowClick = null, onRowContextMenu = null,
    empty = null, rowClass = null, caption = '', maxHeight = null, className = '', onSelectionChange = null } = o;
  let rows = o.rows || [];
  let sortState = o.sort || null;
  let loading = false;
  let skeletonCount = 5;
  let anchorIndex = null;
  const selected = new Set();
  const keyOf = typeof rowKey === 'function' ? rowKey : (r) => r[rowKey];

  const selectAll = selectable ? h('input', { type: 'checkbox', class: 'checkbox', aria: { label: 'Select all rows' } }) : null;
  const headCells = new Map();
  const thead = h('thead', {}, h('tr', {},
    selectable ? h('th', { class: 'td-check', scope: 'col' }, selectAll) : null,
    columns.map((c) => {
      const th = h('th', {
        scope: 'col',
        class: cls(c.className, c.align && `align-${c.align}`, c.hideBelow && `hide-${c.hideBelow}`),
        style: c.width ? { width: c.width } : undefined,
      });
      if (c.sortable) {
        const b = h('button', { class: 'th-sort', type: 'button' }, h('span', {}, c.label), h('span', { class: 'th-sort-icon', aria: { hidden: 'true' } }));
        b.addEventListener('click', () => {
          const dir = sortState?.key === c.key && sortState.dir === 'asc' ? 'desc' : 'asc';
          sortState = { key: c.key, dir };
          updateHeader();
          if (onSort) onSort(sortState);
          else renderBody();
        });
        th.append(b);
      } else {
        th.append(c.label || '');
      }
      headCells.set(c.key, th);
      return th;
    })));
  const tbody = h('tbody');
  const table = h('table', { class: 'table' }, caption ? h('caption', { class: 'sr-only' }, caption) : null, thead, tbody);
  const el = h('div', { class: cls('table-wrap', className) }, table);
  if (maxHeight) {
    el.style.maxHeight = maxHeight;
    el.classList.add('scroll-y');
  }

  function updateHeader() {
    for (const c of columns) {
      const th = headCells.get(c.key);
      if (!c.sortable) continue;
      const active = sortState?.key === c.key;
      th.setAttribute('aria-sort', active ? (sortState.dir === 'asc' ? 'ascending' : 'descending') : 'none');
      const ic = th.querySelector('.th-sort-icon');
      ic.replaceChildren(active ? icon(sortState.dir === 'asc' ? 'arrow-up' : 'arrow-down', 12) : icon('chevrons-up-down', 12));
      th.classList.toggle('is-sorted', active);
    }
  }

  function visibleRows() {
    if (!sortState || onSort) return rows;
    const col = columns.find((c) => c.key === sortState.key);
    return col ? sortRows(rows, col, sortState.dir) : rows;
  }

  function emitSelection() {
    if (selectAll) {
      const n = rows.filter((r) => selected.has(keyOf(r))).length;
      selectAll.checked = n > 0 && n === rows.length;
      selectAll.indeterminate = n > 0 && n < rows.length;
    }
    onSelectionChange?.(api.getSelected());
  }

  function renderBody() {
    const colCount = columns.length + (selectable ? 1 : 0);
    if (loading) {
      tbody.replaceChildren(...Array.from({ length: skeletonCount }, () => h('tr', { class: 'skeleton-row' },
        selectable ? h('td', { class: 'td-check' }) : null,
        columns.map((c) => h('td', { class: cls(c.hideBelow && `hide-${c.hideBelow}`) }, h('span', { class: 'skeleton skeleton-text' }))))));
      return;
    }
    const list = visibleRows();
    if (list.length === 0) {
      const content = typeof empty === 'function' ? empty() : empty;
      tbody.replaceChildren(h('tr', { class: 'empty-row' }, h('td', { colspan: colCount }, content || emptyState({ icon: 'search', title: 'Nothing here', compact: true }))));
      return;
    }
    const frag = document.createDocumentFragment();
    list.forEach((row, index) => {
      const key = keyOf(row);
      const isSel = selected.has(key);
      const tr = h('tr', {
        class: cls({ 'is-selected': isSel, clickable: !!onRowClick }, rowClass?.(row)),
        tabindex: onRowClick ? '0' : undefined, dataset: { key },
        aria: { selected: selectable ? String(isSel) : undefined },
      });
      if (selectable) {
        const cb = h('input', { type: 'checkbox', class: 'checkbox', checked: isSel, aria: { label: 'Select row' } });
        cb.addEventListener('click', (e) => {
          e.stopPropagation();
          if (e.shiftKey && anchorIndex !== null) {
            const [a, b] = [Math.min(anchorIndex, index), Math.max(anchorIndex, index)];
            for (let i = a; i <= b; i++) {
              const k = keyOf(list[i]);
              if (cb.checked) selected.add(k);
              else selected.delete(k);
            }
            renderBody();
          } else {
            if (cb.checked) selected.add(key);
            else selected.delete(key);
            tr.classList.toggle('is-selected', cb.checked);
            tr.setAttribute('aria-selected', String(cb.checked));
          }
          anchorIndex = index;
          emitSelection();
        });
        tr.append(h('td', { class: 'td-check' }, cb));
      }
      for (const c of columns) {
        let v = c.render ? c.render(row) : row[c.key];
        if (v === null || v === undefined || v === '') v = h('span', { class: 'faint' }, '—');
        tr.append(h('td', { class: cls(c.className, c.align && `align-${c.align}`, c.hideBelow && `hide-${c.hideBelow}`) }, typeof v === 'object' ? v : String(v)));
      }
      if (onRowClick) {
        tr.addEventListener('click', (e) => {
          if (e.target instanceof Element && e.target.closest('button,a,input,select,label,textarea')) return;
          onRowClick(row, e);
        });
        tr.addEventListener('keydown', (e) => {
          if ((e.key === 'Enter' || e.key === ' ') && e.target === tr) {
            e.preventDefault();
            onRowClick(row, e);
          }
        });
      }
      if (onRowContextMenu) tr.addEventListener('contextmenu', (e) => onRowContextMenu(row, e));
      frag.append(tr);
    });
    tbody.replaceChildren(frag);
  }

  selectAll?.addEventListener('change', () => {
    if (selectAll.checked) rows.forEach((r) => selected.add(keyOf(r)));
    else selected.clear();
    renderBody();
    emitSelection();
  });

  const api = {
    el,
    setRows(next) {
      rows = next || [];
      loading = false;
      const keys = new Set(rows.map(keyOf));
      for (const k of [...selected]) if (!keys.has(k)) selected.delete(k);
      renderBody();
      emitSelection();
    },
    getRows: () => rows,
    setLoading(on, count = 5) {
      loading = !!on;
      skeletonCount = count;
      table.setAttribute('aria-busy', String(loading));
      renderBody();
    },
    getSelected: () => rows.filter((r) => selected.has(keyOf(r))),
    clearSelection() {
      selected.clear();
      renderBody();
      emitSelection();
    },
    setSort(s) {
      sortState = s;
      updateHeader();
      renderBody();
    },
  };
  updateHeader();
  renderBody();
  return api;
}

/* --------------------------------------------------------------- pagination */

/**
 * Cursor pagination with back navigation (keeps a stack of cursors).
 * fetchPage(cursor) → Promise<{items, next_cursor}>. Stale responses are dropped.
 */
export function createCursorPager(fetchPage) {
  const cursors = [null];
  let index = 0;
  let nextCursor = null;
  let items = [];
  let loading = false;
  let token = 0;
  async function load(i) {
    const my = ++token;
    loading = true;
    try {
      const page = await fetchPage(cursors[i]);
      if (my !== token) return items;
      items = page?.items || [];
      nextCursor = page?.next_cursor || null;
      index = i;
      cursors.length = i + 1;
      if (nextCursor) cursors.push(nextCursor);
      return items;
    } finally {
      if (my === token) loading = false;
    }
  }
  return {
    get state() {
      return { index, page: index + 1, hasPrev: index > 0, hasNext: !!nextCursor, loading, items };
    },
    first() {
      cursors.length = 1;
      cursors[0] = null;
      return load(0);
    },
    next() {
      return nextCursor ? load(index + 1) : Promise.resolve(items);
    },
    prev() {
      return index > 0 ? load(index - 1) : Promise.resolve(items);
    },
    reload() {
      return load(index);
    },
  };
}

/**
 * Prev/next controls for a cursor pager. onPage(items) is called after each load.
 * @returns {{el: HTMLElement, update: () => void}}
 */
export function pagerControls(pager, onPage) {
  const label = h('span', { class: 'pager-label' });
  const run = async (fn) => {
    try {
      const items = await fn();
      onPage(items);
    } catch (err) {
      toast.error(err);
    }
    update();
  };
  const prev = button({ icon: 'chevron-left', title: 'Previous page', size: 'sm', onClick: () => run(() => pager.prev()) });
  const next = button({ icon: 'chevron-right', title: 'Next page', size: 'sm', onClick: () => run(() => pager.next()) });
  const el = h('div', { class: 'pager' }, label, prev, next);
  function update() {
    const s = pager.state;
    label.textContent = `Page ${s.page}`;
    prev.disabled = !s.hasPrev;
    next.disabled = !s.hasNext;
    el.hidden = !s.hasPrev && !s.hasNext;
  }
  update();
  return { el, update };
}

/**
 * Infinite list loader for cursor pages: auto-loads when the sentinel nears the
 * viewport, with a "Load more" button fallback.
 * @param {{fetchPage: (cursor: string|null) => Promise<{items: any[], next_cursor: string|null}>,
 *          onItems: (items: any[], info: {reset: boolean}) => void, root?: Element|null, rootMargin?: string}} o
 */
export function infiniteList({ fetchPage, onItems, root = null, rootMargin = '600px' }) {
  let cursor = null;
  let done = false;
  let busy = false;
  let gen = 0;
  let started = false;
  const spin = spinner({ size: 'sm' });
  const more = button({ label: 'Load more', variant: 'ghost', size: 'sm', onClick: () => loadMore() });
  const el = h('div', { class: 'infinite-sentinel' }, spin, more);
  const render = () => {
    spin.hidden = !busy;
    more.hidden = busy || done || !started;
    el.classList.toggle('is-done', done);
  };
  async function loadMore() {
    if (busy || done) return;
    busy = true;
    started = true;
    render();
    const my = gen;
    try {
      const page = await fetchPage(cursor);
      if (my !== gen) return;
      onItems(page?.items || [], { reset: cursor === null });
      cursor = page?.next_cursor || null;
      done = !cursor;
    } catch (err) {
      if (my === gen) toast.error(err);
    } finally {
      if (my === gen) {
        busy = false;
        render();
        if (!done && io) {
          // Re-observe so a still-visible sentinel triggers the next page.
          io.unobserve(el);
          io.observe(el);
        }
      }
    }
  }
  const io = typeof IntersectionObserver !== 'undefined'
    ? new IntersectionObserver((entries) => { if (entries.some((e) => e.isIntersecting)) loadMore(); }, { root, rootMargin })
    : null;
  io?.observe(el);
  render();
  return {
    el,
    loadMore,
    reload() {
      gen += 1;
      cursor = null;
      done = false;
      busy = false;
      return loadMore();
    },
    destroy() {
      io?.disconnect();
    },
    get done() {
      return done;
    },
  };
}

/* ------------------------------------------------------ states & feedback */

/**
 * Empty state block.
 * @param {{icon?: string, title: string, message?: string|Node, action?: Node, compact?: boolean}} o
 */
export function emptyState({ icon: ic = 'layers', title, message = '', action = null, compact = false }) {
  return h('div', { class: cls('empty', { 'empty-compact': compact }) },
    h('div', { class: 'empty-icon' }, icon(ic, { size: compact ? 18 : 22 })),
    h('div', { class: 'empty-title' }, title),
    message ? h('div', { class: 'empty-msg' }, message) : null,
    action ? h('div', { class: 'empty-action' }, action) : null);
}

/** Error state with optional retry. */
export function errorState(err, onRetry) {
  return emptyState({
    icon: 'alert-circle',
    title: 'Couldn’t load this',
    message: errorMessage(err),
    action: onRetry ? button({ label: 'Try again', icon: 'refresh', onClick: onRetry }) : null,
  });
}

/** Spinner (role=status). */
export function spinner({ size = 'md', label = 'Loading' } = {}) {
  return h('span', { class: cls('spinner', `spinner-${size}`), role: 'status', aria: { label } });
}

/** Centered loading block for whole sections. */
export function loadingBlock(label = 'Loading…') {
  return h('div', { class: 'loading-block' }, spinner({ label }), h('span', { class: 'muted small' }, label));
}

/** Skeleton placeholder. */
export function skeleton({ width = '100%', height = '12px', circle = false, className = '' } = {}) {
  return h('span', { class: cls('skeleton', { 'skeleton-circle': circle }, className), style: { width, height }, aria: { hidden: 'true' } });
}

/** Several skeleton text lines (last one shorter). */
export function skeletonText(lines = 3) {
  return h('div', { class: 'skeleton-lines', aria: { hidden: 'true' } },
    Array.from({ length: lines }, (_, i) => skeleton({ width: i === lines - 1 ? '60%' : '100%' })));
}

/** Skeleton cards for grids. */
export function skeletonCards(n = 3, height = '120px') {
  return Array.from({ length: n }, () => h('div', { class: 'card skeleton-card' }, skeleton({ height })));
}

/* -------------------------------------------------------------- clipboard */

/**
 * Copy text to the clipboard (falls back to execCommand outside secure contexts).
 * @returns {Promise<boolean>}
 */
export async function copyText(text, { message = 'Copied to clipboard', silent = false } = {}) {
  let ok = false;
  try {
    if (navigator.clipboard && window.isSecureContext) {
      await navigator.clipboard.writeText(text);
      ok = true;
    }
  } catch {
    ok = false;
  }
  if (!ok) {
    const ta = h('textarea', { class: 'clipboard-proxy', readonly: true, aria: { hidden: 'true' } });
    ta.value = text;
    document.body.appendChild(ta);
    ta.select();
    try {
      ok = document.execCommand('copy');
    } catch {
      ok = false;
    }
    ta.remove();
  }
  if (!silent) {
    if (ok) toast.success(message, { timeout: 2000 });
    else toast.error('Copy failed — select the text and copy it manually.');
  }
  return ok;
}

/** Icon button that copies text (or the result of a function). */
export function copyButton(text, { title = 'Copy', size = 'sm', variant = 'ghost', message } = {}) {
  const b = button({ icon: 'copy', title, size, variant });
  b.addEventListener('click', async () => {
    const ok = await copyText(typeof text === 'function' ? text() : text, { message });
    if (ok) {
      b.replaceChildren(icon('check', 14));
      setTimeout(() => b.replaceChildren(icon('copy', 14)), 1500);
    }
  });
  return b;
}

/* -------------------------------------------------------- small components */

/**
 * Progress bar.
 * @param {{value?: number, max?: number, variant?: 'accent'|'success'|'warning'|'danger', size?: 'sm'|'md'|'lg',
 *          label?: string, indeterminate?: boolean}} o
 * @returns {{el: HTMLElement, set: (value: number, max?: number) => void, setVariant: (v: string) => void}}
 */
export function progress({ value = 0, max = 1, variant = 'accent', size = 'md', label = '', indeterminate = false } = {}) {
  const bar = h('div', { class: 'progress-bar' });
  const el = h('div', {
    class: cls('progress', `progress-${size}`, `progress-${variant}`, { indeterminate }),
    role: 'progressbar', aria: { label: label || undefined, valuemin: '0', valuemax: '100' },
  }, bar);
  let curVariant = variant;
  const api = {
    el,
    set(v, m = max) {
      const pct = m > 0 ? Math.max(0, Math.min(100, (v / m) * 100)) : 0;
      bar.style.width = `${pct}%`;
      el.setAttribute('aria-valuenow', String(Math.round(pct)));
    },
    setVariant(v) {
      el.classList.remove(`progress-${curVariant}`);
      el.classList.add(`progress-${v}`);
      curVariant = v;
    },
  };
  if (!indeterminate) api.set(value, max);
  return api;
}

/**
 * Badge.
 * @param {string} text
 * @param {'neutral'|'accent'|'success'|'warning'|'danger'|'info'} [variant]
 * @param {{icon?: string, dot?: boolean, title?: string}} [opts]
 */
export function badge(text, variant = 'neutral', { icon: ic, dot = false, title } = {}) {
  return h('span', { class: cls('badge', `badge-${variant}`), dataset: title ? { tip: title } : undefined },
    dot ? h('span', { class: 'badge-dot', aria: { hidden: 'true' } }) : null,
    ic ? icon(ic, 12) : null, text);
}

/** Chip with optional remove button. */
export function chip({ label, icon: ic, onRemove, variant = 'neutral' }) {
  return h('span', { class: cls('chip', `chip-${variant}`) }, ic ? icon(ic, 12) : null, h('span', { class: 'chip-label' }, label),
    onRemove ? h('button', { class: 'chip-x', type: 'button', aria: { label: `Remove ${label}` }, onClick: onRemove }, icon('x', 12)) : null);
}

/** Initials avatar with a stable per-name hue. */
export function avatar({ name = '', size = 28, className = '' } = {}) {
  return h('span', {
    class: cls('avatar', className), aria: { hidden: 'true' },
    style: { '--hue': String(hueFor(name)), '--size': `${size}px` },
  }, initials(name));
}

/**
 * Breadcrumbs. Items: {label, href?, onClick?, icon?}; the last item is the current page.
 */
export function breadcrumbs(items) {
  return h('nav', { class: 'breadcrumbs', aria: { label: 'Breadcrumb' } },
    h('ol', {}, items.map((it, i) => {
      const last = i === items.length - 1;
      const content = [it.icon ? icon(it.icon, 14) : null, h('span', {}, it.label)];
      let node;
      if (last) node = h('span', { class: 'crumb current', aria: { current: 'page' } }, content);
      else if (it.href) node = h('a', { class: 'crumb', href: it.href }, content);
      else if (it.onClick) node = h('button', { class: 'crumb', type: 'button', onClick: it.onClick }, content);
      else node = h('span', { class: 'crumb' }, content);
      return h('li', {}, node, last ? null : icon('chevron-right', { size: 14, className: 'crumb-sep' }));
    })));
}

/**
 * Segmented control (radiogroup).
 * @param {{options: Array<{value: string, label: string, icon?: string}>, value: string,
 *          onChange?: (v: string) => void, ariaLabel?: string, size?: 'sm'|'md'}} o
 */
export function segmented({ options, value, onChange, ariaLabel = 'Options', size = 'md' }) {
  const el = h('div', { class: cls('segmented', size === 'sm' && 'segmented-sm'), role: 'radiogroup', aria: { label: ariaLabel } });
  const btns = options.map((opt) => {
    const b = h('button', { type: 'button', role: 'radio', class: 'segment', dataset: { value: opt.value } },
      opt.icon ? icon(opt.icon, 14) : null, opt.label ? h('span', {}, opt.label) : null);
    if (!opt.label && opt.icon) {
      b.setAttribute('aria-label', opt.title || opt.value);
      b.dataset.tip = opt.title || opt.value;
    }
    b.addEventListener('click', () => {
      api.setValue(opt.value);
      onChange?.(opt.value);
    });
    el.append(b);
    return b;
  });
  el.addEventListener('keydown', (e) => {
    const i = btns.indexOf(document.activeElement);
    if (i < 0) return;
    let j = null;
    if (e.key === 'ArrowRight' || e.key === 'ArrowDown') j = (i + 1) % btns.length;
    if (e.key === 'ArrowLeft' || e.key === 'ArrowUp') j = (i - 1 + btns.length) % btns.length;
    if (j === null) return;
    e.preventDefault();
    btns[j].focus();
    btns[j].click();
  });
  const api = {
    el,
    value,
    setValue(v) {
      api.value = v;
      for (const b of btns) {
        const on = b.dataset.value === v;
        b.setAttribute('aria-checked', String(on));
        b.tabIndex = on ? 0 : -1;
        b.classList.toggle('is-active', on);
      }
    },
  };
  api.setValue(value);
  return api;
}

/**
 * Resizable split pane (pointer + keyboard), width persisted when storageKey is set.
 * @param {{start: Node, end: Node, initial?: number, min?: number, max?: number, storageKey?: string, className?: string}} o
 */
export function splitPane({ start, end, initial = 280, min = 180, max = 640, storageKey = '', className = '' }) {
  let width = initial;
  try {
    const saved = storageKey ? Number(localStorage.getItem(storageKey)) : NaN;
    if (Number.isFinite(saved) && saved > 0) width = saved;
  } catch { /* storage unavailable */ }
  const handle = h('div', {
    class: 'split-handle', role: 'separator', tabindex: '0',
    aria: { orientation: 'vertical', label: 'Resize panel', valuemin: String(min), valuemax: String(max) },
  });
  const el = h('div', { class: cls('split', className) }, h('div', { class: 'split-start' }, start), handle, h('div', { class: 'split-end' }, end));
  const apply = (w, persist = false) => {
    width = Math.max(min, Math.min(max, Math.round(w)));
    el.style.setProperty('--split', `${width}px`);
    handle.setAttribute('aria-valuenow', String(width));
    if (persist && storageKey) {
      try { localStorage.setItem(storageKey, String(width)); } catch { /* ignore */ }
    }
  };
  handle.addEventListener('pointerdown', (e) => {
    e.preventDefault();
    handle.setPointerCapture(e.pointerId);
    const startX = e.clientX;
    const startW = width;
    el.classList.add('is-resizing');
    const move = (ev) => apply(startW + ev.clientX - startX);
    const up = () => {
      handle.removeEventListener('pointermove', move);
      el.classList.remove('is-resizing');
      apply(width, true);
    };
    handle.addEventListener('pointermove', move);
    handle.addEventListener('pointerup', up, { once: true });
    handle.addEventListener('pointercancel', up, { once: true });
  });
  handle.addEventListener('keydown', (e) => {
    if (e.key === 'ArrowLeft') { e.preventDefault(); apply(width - 16, true); }
    if (e.key === 'ArrowRight') { e.preventDefault(); apply(width + 16, true); }
  });
  apply(width);
  return el;
}

/**
 * Card container.
 * @param {{title?: string|Node, description?: string, actions?: Node|Node[], body?: any, footer?: any,
 *          className?: string, flush?: boolean, icon?: string}} o
 */
export function card({ title, description, actions, body, footer, className = '', flush = false, icon: ic }) {
  return h('section', { class: cls('card', className) },
    title || actions ? h('header', { class: 'card-header' },
      h('div', { class: 'card-heading' },
        title ? h('h3', { class: 'card-title' }, ic ? icon(ic, { className: 'card-title-icon' }) : null, title) : null,
        description ? h('p', { class: 'card-desc' }, description) : null),
      actions ? h('div', { class: 'card-actions' }, actions) : null) : null,
    body !== undefined ? h('div', { class: cls('card-body', { flush }) }, body) : null,
    footer ? h('footer', { class: 'card-footer' }, footer) : null);
}

/** Definition list of [label, value] pairs. */
export function kv(pairs) {
  return h('dl', { class: 'kv' }, pairs.filter(Boolean).map(([k, v]) => [h('dt', {}, k), h('dd', {}, v ?? '—')]));
}

/**
 * Callout / alert box.
 * @param {{variant?: 'info'|'success'|'warning'|'danger', title?: string, message?: any, icon?: string, actions?: Node}} o
 */
export function alert({ variant = 'info', title = '', message = '', icon: ic, actions = null }) {
  const defIcon = { info: 'info', success: 'check-circle', warning: 'alert', danger: 'alert-circle' }[variant];
  return h('div', { class: cls('alert', `alert-${variant}`), role: variant === 'danger' ? 'alert' : 'note' },
    icon(ic || defIcon, { className: 'alert-icon' }),
    h('div', { class: 'alert-body' }, title ? h('div', { class: 'alert-title' }, title) : null, message ? h('div', { class: 'alert-text' }, message) : null),
    actions ? h('div', { class: 'alert-actions' }, actions) : null);
}

/** Page header: title, optional subtitle and actions. */
export function pageHeader({ title, subtitle = '', actions = null }) {
  return h('header', { class: 'page-header' },
    h('div', { class: 'page-heading' }, h('h1', { class: 'page-title' }, title), subtitle ? h('p', { class: 'page-subtitle' }, subtitle) : null),
    actions ? h('div', { class: 'page-actions' }, actions) : null);
}

/** <time> with relative text and the absolute date as tooltip. */
export function timeAgo(value) {
  if (!value) return h('span', { class: 'faint' }, '—');
  return h('time', { datetime: String(value), dataset: { tip: formatDateTime(value) } }, formatRelative(value));
}

/** Keyboard key hint, e.g. kbd('Ctrl', 'K'). */
export function kbd(...keys) {
  return h('span', { class: 'kbd-group' }, keys.map((k) => h('kbd', {}, k)));
}
