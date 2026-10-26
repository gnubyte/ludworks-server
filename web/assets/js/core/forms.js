// Form building blocks: labelled fields, inputs, password inputs with a
// strength meter, server validation error display, and change diffing.

import { h, uid, cls } from './dom.js';
import { icon } from './icons.js';
import { estimateStrength } from './password.js';

/**
 * Labelled form field. The control gets an id, aria-describedby (hint/error) and
 * the wrapper gets data-field=name so server errors can be attached later.
 * @param {{label?: string, hint?: string|Node, error?: string, control: HTMLElement, name?: string,
 *          required?: boolean, className?: string, inline?: boolean, labelExtra?: Node}} o
 */
export function field({ label, hint, error, control, name, required = false, className, inline = false, labelExtra }) {
  const target = control.matches?.('input,select,textarea') ? control : control.querySelector?.('input,select,textarea') || control;
  const id = target.id || uid('f');
  target.id = id;
  const fieldName = name || target.getAttribute?.('name') || '';
  const hintId = `${id}-hint`;
  const errId = `${id}-err`;
  const hintEl = hint ? h('div', { class: 'field-hint', id: hintId }, hint) : null;
  const errEl = h('div', { class: 'field-error', id: errId, role: 'alert', hidden: !error }, error || '');
  const describedBy = [hint ? hintId : null, errId].filter(Boolean).join(' ');
  target.setAttribute('aria-describedby', describedBy);
  if (required) target.required = true;
  const labelEl = label
    ? h('label', { class: cls('field-label', { 'has-extra': !!labelExtra }), for: id }, label,
      required ? h('span', { class: 'field-req', aria: { hidden: 'true' } }, '*') : null)
    : null;
  // Extra content (e.g. a "Forgot password?" link) sits beside the label, outside it.
  const labelRow = labelEl && labelExtra ? h('div', { class: 'field-label-row' }, labelEl, h('span', { class: 'label-extra' }, labelExtra)) : labelEl;
  const wrap = h('div', { class: cls('field', { 'field-inline': inline }, className), dataset: { field: fieldName } },
    inline ? h('div', { class: 'field-inline-text' }, labelRow, hintEl) : labelRow,
    control,
    inline ? null : hintEl,
    errEl);
  if (error) target.setAttribute('aria-invalid', 'true');
  return wrap;
}

/**
 * Server validation messages are fragments meant to follow a field name ("must be at least
 * 12 characters"); under a field they read as a sentence: "Must be at least 12 characters."
 * Text that already starts with a capital or ends with punctuation is left as is.
 * @param {string} msg
 */
export function asSentence(msg) {
  const t = String(msg ?? '').trim();
  if (!t) return '';
  const first = t[0].toLocaleUpperCase() + t.slice(1);
  return /[.!?…:)\]"'”’]$/u.test(first) ? first : `${first}.`;
}

/** Show (or clear with '') an error message on a field wrapper. */
export function setFieldError(fieldEl, message) {
  const err = fieldEl.querySelector(':scope > .field-error');
  const ctl = fieldEl.querySelector('input,select,textarea');
  if (err) {
    err.textContent = message || '';
    err.hidden = !message;
  }
  if (ctl) {
    if (message) ctl.setAttribute('aria-invalid', 'true');
    else ctl.removeAttribute('aria-invalid');
  }
}

/** Clear every field error and banner in a form. */
export function clearErrors(root) {
  for (const f of root.querySelectorAll('.field')) setFieldError(f, '');
  for (const b of root.querySelectorAll('.form-error-banner')) b.remove();
}

/**
 * Attach server validation errors (ApiError.details.fields) to fields by data-field.
 * Keys may be "name" or "namespace.name"; `prefix` is stripped when given.
 * Returns the messages that matched no field so callers can show them elsewhere.
 * @param {HTMLElement} root
 * @param {Record<string,string>|null} fields
 * @param {string} [prefix] e.g. "smtp."
 * @returns {string[]}
 */
export function showFieldErrors(root, fields, prefix = '') {
  const unmatched = [];
  let first = null;
  for (const [key, msg] of Object.entries(fields || {})) {
    const k = prefix && key.startsWith(prefix) ? key.slice(prefix.length) : key;
    const el = root.querySelector(`.field[data-field="${CSS.escape(k)}"]`)
      || root.querySelector(`.field[data-field="${CSS.escape(key)}"]`);
    if (el) {
      setFieldError(el, asSentence(msg));
      first ??= el;
    } else {
      unmatched.push(`${key}: ${msg}`);
    }
  }
  first?.querySelector('input,select,textarea')?.focus();
  return unmatched;
}

/**
 * Inline error/info banner for forms.
 * @param {string} message
 * @param {'danger'|'warning'|'info'|'success'} [variant]
 */
export function formBanner(message, variant = 'danger') {
  const ic = { danger: 'alert-circle', warning: 'alert', info: 'info', success: 'check-circle' }[variant];
  return h('div', { class: cls('alert', `alert-${variant}`, 'form-error-banner'), role: variant === 'danger' ? 'alert' : 'status' },
    icon(ic, { className: 'alert-icon' }), h('div', { class: 'alert-body' }, message));
}

/**
 * Show an ApiError on a form: field errors next to fields, the rest as a banner
 * inserted at `bannerHost` (default: top of root).
 */
export function showFormError(root, err, { prefix = '', bannerHost = null } = {}) {
  clearErrors(root);
  const unmatched = err?.fields ? showFieldErrors(root, err.fields, prefix) : [];
  const msg = unmatched.length ? unmatched.join(' · ') : err?.fields ? '' : (err?.message || 'Something went wrong.');
  if (msg) {
    const host = bannerHost || root;
    host.prepend(formBanner(msg));
  }
}

/** Text-like input. Extra props pass through to h(). */
export function input(props = {}) {
  const { className, ...rest } = props;
  return h('input', { type: 'text', class: cls('input', className), ...rest });
}

/** Textarea. */
export function textarea(props = {}) {
  const { className, ...rest } = props;
  return h('textarea', { class: cls('textarea', className), ...rest });
}

/**
 * Select with options [{value, label, disabled}] or ['a','b'].
 */
export function select({ options = [], value, className, ...rest } = {}) {
  const el = h('select', { class: cls('select', className), ...rest },
    options.map((o) => {
      const opt = typeof o === 'object' ? o : { value: o, label: String(o) };
      return h('option', { value: String(opt.value), disabled: !!opt.disabled }, opt.label);
    }));
  if (value !== undefined && value !== null) el.value = String(value);
  return el;
}

/** Checkbox with an inline label. Returns the <label>; the input is .input-el */
export function checkbox({ label, checked = false, name, onChange, disabled = false, hint } = {}) {
  const inputEl = h('input', { type: 'checkbox', class: 'checkbox', name, checked, disabled });
  if (onChange) inputEl.addEventListener('change', () => onChange(inputEl.checked));
  const el = h('label', { class: cls('check-row', { 'is-disabled': disabled }) }, inputEl,
    h('span', { class: 'check-text' }, h('span', { class: 'check-label' }, label), hint ? h('span', { class: 'check-hint' }, hint) : null));
  el.inputEl = inputEl;
  return el;
}

/**
 * Toggle switch (input[type=checkbox][role=switch]). Returns the <label>; .inputEl is the input.
 */
export function switchInput({ checked = false, name, onChange, disabled = false, label = '' } = {}) {
  const inputEl = h('input', { type: 'checkbox', role: 'switch', class: 'switch-input', name, checked, disabled, aria: label ? { label } : undefined });
  if (onChange) inputEl.addEventListener('change', () => onChange(inputEl.checked));
  const el = h('label', { class: cls('switch', { 'is-disabled': disabled }) }, inputEl, h('span', { class: 'switch-track', aria: { hidden: 'true' } }));
  el.inputEl = inputEl;
  return el;
}

/**
 * Password input with a show/hide toggle. Returns a wrapper; .inputEl is the input.
 */
export function passwordInput({ name = 'password', value = '', autocomplete = 'current-password', placeholder = '', required = false, minLength } = {}) {
  const inputEl = h('input', {
    type: 'password', class: 'input has-action', name, value, autocomplete, placeholder, required,
    minlength: minLength, spellcheck: 'false', autocapitalize: 'off', 'data-no-trim': true,
  });
  const toggle = h('button', { type: 'button', class: 'input-action btn btn-ghost btn-icon btn-sm', aria: { label: 'Show password', pressed: 'false' } }, icon('eye'));
  toggle.addEventListener('click', () => {
    const show = inputEl.type === 'password';
    inputEl.type = show ? 'text' : 'password';
    toggle.replaceChildren(icon(show ? 'eye-off' : 'eye'));
    toggle.setAttribute('aria-label', show ? 'Hide password' : 'Show password');
    toggle.setAttribute('aria-pressed', String(show));
  });
  const wrap = h('div', { class: 'input-group' }, inputEl, toggle);
  wrap.inputEl = inputEl;
  return wrap;
}

/**
 * Live strength meter bound to a password input.
 * @param {HTMLInputElement} inputEl
 * @param {() => string[]} [userInputs] values to penalise (username, email)
 */
export function strengthMeter(inputEl, userInputs = () => []) {
  const segs = [0, 1, 2, 3].map(() => h('span', { class: 'strength-seg' }));
  const label = h('span', { class: 'strength-label' });
  const tip = h('span', { class: 'strength-tip' });
  const el = h('div', { class: 'strength', dataset: { score: '-1' }, aria: { live: 'polite' } },
    h('div', { class: 'strength-bar', aria: { hidden: 'true' } }, segs), h('div', { class: 'strength-text' }, label, tip));
  const update = () => {
    const v = inputEl.value;
    if (!v) {
      el.dataset.score = '-1';
      label.textContent = '';
      tip.textContent = '';
      return;
    }
    const r = estimateStrength(v, userInputs());
    el.dataset.score = String(r.score);
    label.textContent = r.label;
    tip.textContent = r.warnings[0] || '';
  };
  inputEl.addEventListener('input', update);
  update();
  return el;
}

/** Deep equality for JSON-like values. */
export function deepEqual(a, b) {
  if (Object.is(a, b)) return true;
  if (typeof a !== 'object' || typeof b !== 'object' || a === null || b === null) return false;
  if (Array.isArray(a) !== Array.isArray(b)) return false;
  const ka = Object.keys(a);
  const kb = Object.keys(b);
  if (ka.length !== kb.length) return false;
  return ka.every((k) => Object.prototype.hasOwnProperty.call(b, k) && deepEqual(a[k], b[k]));
}

/**
 * Keys of `current` whose values differ from `original` (deep compare).
 * Used so PATCH requests carry only changed fields.
 * @param {Record<string, any>} original
 * @param {Record<string, any>} current
 * @returns {Record<string, any>}
 */
export function diffObject(original, current) {
  const out = {};
  for (const [k, v] of Object.entries(current || {})) {
    if (v === undefined) continue;
    if (!deepEqual(original?.[k], v)) out[k] = v;
  }
  return out;
}

/**
 * Read named controls of a form into an object. Checkboxes → boolean,
 * number inputs → number (or null when empty), others → trimmed string
 * (password inputs are not trimmed).
 */
export function readForm(root) {
  const out = {};
  for (const el of root.querySelectorAll('input[name],select[name],textarea[name]')) {
    if (el.type === 'checkbox') out[el.name] = el.checked;
    else if (el.type === 'number') out[el.name] = el.value === '' ? null : Number(el.value);
    else if (el.type === 'radio') {
      if (el.checked) out[el.name] = el.value;
    } else if (el.type === 'password' || el.dataset.noTrim !== undefined) out[el.name] = el.value;
    else out[el.name] = el.value.trim();
  }
  return out;
}
