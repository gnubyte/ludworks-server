// First-time setup wizard: explain zero-knowledge encryption, choose the master password
// (strength meter, confirmation, unrecoverability acknowledgement), derive the key with
// PBKDF2-SHA256 (600 000 iterations) and POST /vault/init.

import { h, cls, mount as fill } from '../../../core/dom.js';
import { icon } from '../../../core/icons.js';
import { field, passwordInput, checkbox, clearErrors, showFieldErrors } from '../../../core/forms.js';
import * as ui from '../../../core/ui.js';
import { vicon, strengthBar } from '../widgets.js';

const STEPS = ['How it works', 'Master password', 'Create vault'];
export const MIN_MASTER_LENGTH = 10;

/**
 * @param {HTMLElement} host
 * @param {{vault: object, ctx: object}} o
 */
export function renderSetup(host, { vault, ctx }) {
  let step = 0;
  const stepper = h('div', { class: 'steps vault-steps', aria: { label: 'Setup progress' } });

  const intro = h('div', { class: 'stack' },
    h('p', { class: 'vault-lead' }, 'Your passwords are encrypted on this device before they are sent to the server. The server only ever stores ciphertext — it cannot read your vault, and neither can an administrator.'),
    h('ul', { class: 'vault-points' },
      point('lock', 'One master password', 'It unlocks your vault and never leaves this browser. It is separate from your Ludworks login password.'),
      point('shield-check', 'Strong encryption', 'AES-256-GCM with a key derived by PBKDF2-SHA256 (600 000 iterations).'),
      point('alert', 'No recovery', 'Nobody can reset a forgotten master password. Your only option would be deleting the vault and starting over.')));

  const pw = passwordInput({ name: 'master', autocomplete: 'new-password' });
  const confirm = passwordInput({ name: 'confirm', autocomplete: 'new-password' });
  const meter = strengthBar();
  pw.inputEl.addEventListener('input', () => meter.update(pw.inputEl.value));
  const ack = checkbox({ label: 'I understand that my master password cannot be recovered.', name: 'ack' });
  const ackField = h('div', { class: 'field', dataset: { field: 'ack' } }, ack, h('div', { class: 'field-error', role: 'alert', hidden: true }));
  const choose = h('div', { class: 'stack' },
    h('div', { class: 'stack-sm' },
      field({ label: 'Master password', control: pw, name: 'master', hint: `At least ${MIN_MASTER_LENGTH} characters. A passphrase of four or more random words is easy to remember and hard to guess.` }),
      meter.el),
    field({ label: 'Confirm master password', control: confirm, name: 'confirm' }),
    ackField);

  const stage = h('div', { class: 'vault-creating', aria: { live: 'polite' } });
  const create = h('div', { class: 'stack' }, stage);

  const panels = [intro, choose, create];
  const back = ui.button({ label: 'Back', icon: 'arrow-left', variant: 'ghost', onClick: () => go(step - 1) });
  const next = ui.button({ label: 'Continue', iconRight: 'arrow-right', variant: 'primary', type: 'submit' });
  const nav = h('div', { class: 'row between' }, back, next);
  const form = h('form', { class: 'stack-lg', novalidate: true }, ...panels, nav);

  const card = h('div', { class: 'vault-gate-card vault-setup' },
    h('div', { class: 'vault-gate-icon' }, vicon('vault', 26)),
    h('h1', { class: 'vault-gate-title' }, 'Set up your password vault'),
    stepper, form);
  host.replaceChildren(h('div', { class: 'vault-gate' }, card));

  function point(ic, title, text) {
    return h('li', {}, h('span', { class: 'vault-point-icon' }, icon(ic, 16)), h('div', {}, h('div', { class: 'bold' }, title), h('div', { class: 'muted small' }, text)));
  }

  function render() {
    fill(stepper, ...STEPS.flatMap((label, i) => [
      i > 0 ? h('span', { class: 'step-line', aria: { hidden: 'true' } }) : null,
      h('span', { class: cls('step', { 'is-active': i === step, 'is-done': i < step }), aria: { current: i === step ? 'step' : undefined } },
        h('span', { class: 'step-num' }, i < step ? icon('check', 12) : String(i + 1)), label),
    ]));
    panels.forEach((p, i) => { p.hidden = i !== step; });
    back.hidden = step === 0 || step === 2;
    nav.hidden = step === 2;
    next.replaceChildren(h('span', { class: 'btn-label' }, step === 1 ? 'Create vault' : 'Get started'), icon(step === 1 ? 'lock' : 'arrow-right', 14));
    if (step === 1) requestAnimationFrame(() => pw.inputEl.focus());
  }
  function go(n) {
    step = Math.max(0, Math.min(STEPS.length - 1, n));
    render();
  }

  function validate() {
    clearErrors(form);
    const v = pw.inputEl.value;
    const problems = {};
    if (v.length < MIN_MASTER_LENGTH) problems.master = `Use at least ${MIN_MASTER_LENGTH} characters.`;
    else if ((meter.update(v)?.score ?? 0) < 2) problems.master = 'This password is too easy to guess. Add more words or characters.';
    if (!problems.master && confirm.inputEl.value !== v) problems.confirm = 'The passwords don’t match.';
    if (!ack.inputEl.checked) problems.ack = 'Please confirm that you understand.';
    if (Object.keys(problems).length) {
      showFieldErrors(form, problems);
      return null;
    }
    return v;
  }

  async function createVault(password) {
    go(2);
    const started = performance.now();
    const bar = ui.progress({ indeterminate: true, label: 'Creating vault' });
    const line = h('div', { class: 'vault-stage-line' }, ui.spinner({ size: 'sm' }), h('span', {}, 'Deriving your encryption key…'));
    const sub = h('div', { class: 'muted small' }, 'PBKDF2-SHA256 with 600 000 iterations runs on this device. This takes a moment on purpose: it makes guessing your master password very slow.');
    stage.replaceChildren(line, bar.el, sub);
    try {
      await vault.session.initialize(password, {
        onStage: (s) => {
          if (s === 'save') line.lastChild.textContent = 'Saving the encrypted key…';
          if (s === 'done') line.lastChild.textContent = 'Opening your vault…';
        },
      });
      const secs = ((performance.now() - started) / 1000).toFixed(1);
      ui.toast.success('Your vault is ready', { description: `Key derived and vault created in ${secs} s.` });
    } catch (err) {
      stage.replaceChildren(ui.alert({ variant: 'danger', title: 'The vault could not be created', message: ui.errorMessage(err) }));
      if (err?.status === 409) {
        // Initialised meanwhile (another tab or device): show the unlock screen instead.
        await vault.session.load().catch(() => {});
        return;
      }
      go(1);
    } finally {
      pw.inputEl.value = '';
      confirm.inputEl.value = '';
      meter.update('');
    }
  }

  form.addEventListener('submit', (e) => {
    e.preventDefault();
    if (step === 0) {
      go(1);
      return;
    }
    if (step === 1) {
      const password = validate();
      if (password) createVault(password);
    }
  });

  ctx.setTitle('Set up · Passwords');
  render();
  return () => {};
}
