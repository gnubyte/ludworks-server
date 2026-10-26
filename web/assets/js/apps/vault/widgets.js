// Small DOM widgets shared by the Passwords views: extra icons, the strength bar, secret rows
// with reveal/copy, the TOTP code with its countdown ring, and copy-with-auto-clear.

import { h, s, cls, uid } from '../../core/dom.js';
import { icon } from '../../core/icons.js';
import * as ui from '../../core/ui.js';
import { estimateStrength } from '../../lib/vault/generator.js';
import { generateTotp } from '../../lib/vault/totp.js';
import { avatarName } from './model.js';

const SVG = (children) => children;
const EXTRA = {
  dice: SVG([['rect', { x: 3.5, y: 3.5, width: 17, height: 17, rx: 3.5 }], ['circle', { cx: 8.5, cy: 8.5, r: 1.1, fill: 'currentColor' }],
    ['circle', { cx: 15.5, cy: 8.5, r: 1.1, fill: 'currentColor' }], ['circle', { cx: 12, cy: 12, r: 1.1, fill: 'currentColor' }],
    ['circle', { cx: 8.5, cy: 15.5, r: 1.1, fill: 'currentColor' }], ['circle', { cx: 15.5, cy: 15.5, r: 1.1, fill: 'currentColor' }]]),
  'shield-alert': SVG([['path', { d: 'M12 3l8 3v6c0 4.5-3.4 8.3-8 9-4.6-.7-8-4.5-8-9V6z' }], ['path', { d: 'M12 8.5v4' }], ['path', { d: 'M12 16h.01' }]]),
  vault: SVG([['rect', { x: 3, y: 4, width: 18, height: 16, rx: 2.5 }], ['circle', { cx: 12, cy: 12, r: 3.6 }], ['path', { d: 'M12 8.4V7' }],
    ['path', { d: 'M12 17v-1.4' }], ['path', { d: 'M15.6 12H17' }], ['path', { d: 'M7 12h1.4' }], ['path', { d: 'M6 20v1.2' }], ['path', { d: 'M18 20v1.2' }]]),
};

/** Icon from the core set, plus a few vault-specific ones. */
export function vicon(name, opts = {}) {
  const def = EXTRA[name];
  if (!def) return icon(name, opts);
  const { size = 16, className = '' } = typeof opts === 'number' ? { size: opts } : opts;
  return s('svg', {
    viewBox: '0 0 24 24', width: size, height: size, fill: 'none', stroke: 'currentColor', 'stroke-width': 1.75,
    'stroke-linecap': 'round', 'stroke-linejoin': 'round', class: ['icon', className], focusable: 'false', 'aria-hidden': 'true',
  }, def.map(([tag, attrs]) => s(tag, attrs)));
}

export const STRENGTH_LABELS = ['Very weak', 'Weak', 'Fair', 'Strong', 'Very strong'];

/** Strength bar driven by the vault library's estimator. */
export function strengthBar() {
  const segs = [0, 1, 2, 3].map(() => h('span', { class: 'strength-seg' }));
  const label = h('span', { class: 'strength-label' });
  const tip = h('span', { class: 'strength-tip' });
  const el = h('div', { class: 'strength vault-strength', dataset: { score: '-1' }, aria: { live: 'polite' } },
    h('div', { class: 'strength-bar', aria: { hidden: 'true' } }, segs), h('div', { class: 'strength-text' }, label, tip));
  return {
    el,
    /** @returns {{score: number, entropyBits: number, feedback: string[]}|null} */
    update(pw) {
      if (!pw) {
        el.dataset.score = '-1';
        label.textContent = '';
        tip.textContent = '';
        return null;
      }
      const r = estimateStrength(pw);
      el.dataset.score = String(r.score);
      label.textContent = STRENGTH_LABELS[r.score];
      tip.textContent = r.score < 3 && r.feedback[0] ? r.feedback[0] : `≈ ${Math.round(r.entropyBits)} bits of entropy`;
      return r;
    },
  };
}

/**
 * Copy text through the vault clipboard (auto-clears secrets after 30 s) with a toast.
 * @param {{clipboard: object}} vault
 */
export async function copySecret(vault, text, what = 'Value', { secret = true } = {}) {
  const ok = await vault.clipboard.copy(text, { secret });
  if (ok) ui.toast.success(`${what} copied`, { description: secret ? 'The clipboard is cleared after 30 seconds.' : '', timeout: 2200 });
  else ui.toast.error('Copy failed — select the text and copy it manually.');
  return ok;
}

/** Icon button that copies a (possibly lazy) value. */
export function copyBtn(vault, getText, what, { secret = true, label = `Copy ${what.toLowerCase()}` } = {}) {
  const b = ui.button({ icon: 'copy', title: label, size: 'sm', variant: 'ghost' });
  b.addEventListener('click', async () => {
    if (await copySecret(vault, typeof getText === 'function' ? getText() : getText, what, { secret })) {
      b.replaceChildren(icon('check', 14));
      setTimeout(() => b.replaceChildren(icon('copy', 14)), 1500);
    }
  });
  return b;
}

/**
 * A labelled read-only field in the detail pane: value, optional reveal toggle and copy.
 * @param {{label: string, value: string, secret?: boolean, mono?: boolean, vault: object, copy?: boolean,
 *          display?: (v: string) => string, extra?: Node|Node[], multiline?: boolean}} o
 */
export function fieldRow({ label, value, secret = false, mono = false, vault, copy = true, display = (v) => v, extra = null, multiline = false }) {
  const valId = uid('vv');
  const val = h('div', { class: cls('vf-value', { mono: mono || secret, multiline }), id: valId });
  let shown = !secret;
  const paint = () => {
    val.textContent = shown ? display(value) : '•'.repeat(Math.min(Math.max(value.length, 8), 16));
    val.classList.toggle('is-masked', !shown);
  };
  paint();
  const actions = h('div', { class: 'vf-actions' });
  if (secret) {
    const eye = ui.button({ icon: 'eye', title: `Show ${label.toLowerCase()}`, size: 'sm', variant: 'ghost' });
    eye.setAttribute('aria-pressed', 'false');
    eye.setAttribute('aria-controls', valId);
    eye.addEventListener('click', () => {
      shown = !shown;
      paint();
      eye.replaceChildren(icon(shown ? 'eye-off' : 'eye', 14));
      eye.setAttribute('aria-pressed', String(shown));
      const t = `${shown ? 'Hide' : 'Show'} ${label.toLowerCase()}`;
      eye.setAttribute('aria-label', t);
      eye.dataset.tip = t;
    });
    actions.append(eye);
  }
  if (extra) actions.append(...[].concat(extra));
  if (copy) actions.append(copyBtn(vault, () => value, label, { secret }));
  return h('div', { class: 'vf-row' }, h('div', { class: 'vf-label' }, label), h('div', { class: 'vf-main' }, val, actions));
}

/**
 * Live TOTP code with a countdown ring. Returns {el, stop()}; stops itself when detached.
 * @param {string} secret otpauth URI or base32 secret
 */
export function totpWidget(vault, secret) {
  const R = 9;
  const C = 2 * Math.PI * R;
  const ring = s('circle', { cx: 12, cy: 12, r: R, class: 'totp-ring-fg', 'stroke-dasharray': C.toFixed(2) });
  const svg = s('svg', { viewBox: '0 0 24 24', width: 26, height: 26, class: 'totp-ring', 'aria-hidden': 'true', focusable: 'false' },
    s('circle', { cx: 12, cy: 12, r: R, class: 'totp-ring-bg' }), ring);
  const secs = h('span', { class: 'totp-secs' });
  const code = h('span', { class: 'totp-code mono', aria: { live: 'off' } }, '••• •••');
  const ringWrap = h('span', { class: 'totp-ring-wrap', title: 'Seconds until the code changes' }, svg, secs);
  let current = '';
  const copy = copyBtn(vault, () => current, 'Code', { secret: true, label: 'Copy code' });
  const el = h('div', { class: 'totp' }, code, ringWrap, copy);
  let timer = null;
  let stopped = false;
  const fmt = (c) => (c.length === 6 ? `${c.slice(0, 3)} ${c.slice(3)}` : c.length === 8 ? `${c.slice(0, 4)} ${c.slice(4)}` : c);
  async function tick() {
    if (stopped) return;
    if (!el.isConnected && timer !== null) {
      stop();
      return;
    }
    try {
      const r = await generateTotp(secret);
      if (stopped) return;
      current = r.code;
      code.textContent = fmt(r.code);
      secs.textContent = String(r.remaining);
      ring.style.strokeDashoffset = String((C * (1 - r.remaining / r.period)).toFixed(2));
      el.classList.toggle('is-expiring', r.remaining <= 5);
      el.classList.remove('is-error');
    } catch (err) {
      current = '';
      code.textContent = 'Invalid secret';
      secs.textContent = '';
      el.classList.add('is-error');
      copy.disabled = true;
      stop();
    }
  }
  function stop() {
    stopped = true;
    if (timer !== null) clearInterval(timer);
    timer = null;
  }
  tick();
  timer = setInterval(tick, 1000);
  return { el, stop };
}

/** Avatar with the item's initials and a stable hue (no remote favicons, ever). */
export function itemAvatar(name, size = 32) {
  return ui.avatar({ name: avatarName(name), size, className: 'vault-avatar' });
}

/** Small section heading used inside the detail pane and forms. */
export function sectionTitle(text, actions = null) {
  return h('div', { class: 'vault-section-title' }, h('span', {}, text), actions);
}
