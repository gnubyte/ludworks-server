// Unlock screen. The master password is checked locally (PBKDF2 + AES-GCM unwrap of the
// stored key): no request is sent per attempt, so there is nothing to rate limit server-side.
// Repeated failures get a growing local cool-down to discourage guessing at the keyboard.

import { h } from '../../../core/dom.js';
import { icon } from '../../../core/icons.js';
import { field, passwordInput, clearErrors, showFieldErrors } from '../../../core/forms.js';
import * as ui from '../../../core/ui.js';
import { vicon } from '../widgets.js';
import { openResetDialog } from './settings.js';

let failures = 0;

/** Cool-down in seconds after n consecutive wrong passwords (0 for the first three). */
export function unlockDelay(n) {
  return n < 3 ? 0 : Math.min(30, 2 ** (n - 3));
}

export function renderUnlock(host, { vault, ctx }) {
  const pw = passwordInput({ name: 'master', autocomplete: 'current-password' });
  const submit = ui.button({ label: 'Unlock', icon: 'unlock', variant: 'primary', type: 'submit', block: true, size: 'lg' });
  const reason = vault.ui.lockReason ? h('div', { class: 'vault-lock-reason' }, icon('clock', 14), vault.ui.lockReason) : null;
  const form = h('form', { class: 'stack', novalidate: true },
    field({ label: 'Master password', control: pw, name: 'master' }), submit);
  const forgot = h('button', { type: 'button', class: 'link-button small' }, 'Forgot your master password?');
  forgot.addEventListener('click', () => openResetDialog({ vault, ctx }));
  const user = ctx.user?.username || '';
  const card = h('div', { class: 'vault-gate-card vault-unlock' },
    h('div', { class: 'vault-gate-icon' }, vicon('vault', 26)),
    h('h1', { class: 'vault-gate-title' }, 'Your vault is locked'),
    h('p', { class: 'muted vault-gate-sub' }, 'Enter your master password to decrypt your passwords on this device.', user ? h('br') : null,
      user ? h('span', { class: 'small' }, 'Signed in as ', h('strong', {}, user)) : null),
    reason, form, h('div', { class: 'vault-gate-foot' }, forgot));
  host.replaceChildren(h('div', { class: 'vault-gate' }, card));
  ctx.setTitle('Locked · Passwords');
  requestAnimationFrame(() => pw.inputEl.focus());

  let cooling = null;
  function coolDown(secs) {
    let left = secs;
    submit.disabled = true;
    const label = submit.querySelector('.btn-label');
    const tick = () => {
      if (left <= 0) {
        clearInterval(cooling);
        cooling = null;
        submit.disabled = false;
        label.textContent = 'Unlock';
        return;
      }
      label.textContent = `Try again in ${left}s`;
      left -= 1;
    };
    tick();
    cooling = setInterval(tick, 1000);
  }

  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    if (cooling || submit.disabled) return;
    clearErrors(form);
    const value = pw.inputEl.value;
    if (!value) {
      showFieldErrors(form, { master: 'Enter your master password.' });
      return;
    }
    ui.setLoading(submit, true);
    try {
      await vault.session.unlock(value);
      failures = 0;
      vault.autolock.touch();
    } catch (err) {
      ui.setLoading(submit, false);
      if (err?.code === 'bad_password') {
        failures += 1;
        showFieldErrors(form, { master: 'That master password is not correct.' });
        pw.inputEl.select();
        card.classList.remove('shake');
        void card.offsetWidth;
        card.classList.add('shake');
        const d = unlockDelay(failures);
        if (d) coolDown(d);
      } else if (err?.code === 'unsupported' || err?.code === 'invalid_argument' || err?.code === 'malformed') {
        showFieldErrors(form, { master: 'The vault parameters on the server are not acceptable (unsupported or too weak). Contact your administrator.' });
      } else {
        showFieldErrors(form, { master: ui.errorMessage(err) });
      }
      return;
    } finally {
      pw.inputEl.value = '';
    }
  });
  return () => {
    if (cooling) clearInterval(cooling);
  };
}
