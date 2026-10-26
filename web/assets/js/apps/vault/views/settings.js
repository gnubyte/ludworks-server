// Vault settings: auto-lock (stored non-sensitively), lock when the tab is hidden, change the
// master password (re-wrap the vault key + PUT /vault/key, optionally replacing the vault key),
// rotate the vault key (re-encrypt everything + POST /vault/rekey) and reset the vault (account
// login password + typed confirmation → POST /vault/reset).

import { h, uid } from '../../../core/dom.js';
import { field, passwordInput, input, checkbox, clearErrors, showFieldErrors } from '../../../core/forms.js';
import * as ui from '../../../core/ui.js';
import { AUTO_LOCK_CHOICES } from '../prefs.js';
import { strengthBar } from '../widgets.js';
import { MIN_MASTER_LENGTH } from './setup.js';

export const RESET_PHRASE = 'DELETE MY VAULT';

const minutesLabel = (m) => (m < 60 ? `${m} minute${m === 1 ? '' : 's'}` : `${m / 60} hour${m === 60 ? '' : 's'}`);

/**
 * Reset dialog (also reachable from the unlock screen). Resolves true when the vault was deleted.
 */
export function openResetDialog({ vault }) {
  const pw = passwordInput({ name: 'password', autocomplete: 'current-password' });
  const phrase = input({ name: 'phrase', autocomplete: 'off', spellcheck: 'false', placeholder: RESET_PHRASE });
  const body = h('div', { class: 'stack' },
    ui.alert({ variant: 'danger', title: 'This permanently deletes every item and folder',
      message: 'Use this only if you forgot your master password. Nobody — not even an administrator — can recover the contents. Other devices will see an empty vault.' }),
    field({ label: 'Your Ludworks account password', control: pw, name: 'password', hint: 'The password you sign in with, not the master password.' }),
    field({ label: `Type “${RESET_PHRASE}” to confirm`, control: phrase, name: 'phrase' }));
  const m = ui.modal({
    title: 'Reset vault', size: 'md', body,
    footer: [{ label: 'Cancel', value: false }, { label: 'Delete vault', variant: 'danger', type: 'submit', icon: 'trash' }],
    onSubmit: async () => {
      clearErrors(body);
      const errs = {};
      if (!pw.inputEl.value) errs.password = 'Enter your account password.';
      if (phrase.value.trim() !== RESET_PHRASE) errs.phrase = `Type ${RESET_PHRASE} exactly.`;
      if (Object.keys(errs).length) {
        showFieldErrors(body, errs);
        return false;
      }
      try {
        await vault.session.reset(pw.inputEl.value);
      } catch (err) {
        pw.inputEl.value = '';
        showFieldErrors(body, { password: err?.code === 'invalid_credentials' ? 'That account password is not correct.' : ui.errorMessage(err) });
        return false;
      }
      pw.inputEl.value = '';
      ui.toast.success('Your vault was deleted', { description: 'You can set up a new one now.' });
      return true;
    },
  });
  return m.closed.then((v) => v === true);
}

const ROTATE_STAGES = {
  verify: 'Checking your master password and syncing…',
  encrypt: 'Generating a new vault key and re-encrypting every item…',
  save: 'Saving the re-encrypted vault…',
};

/** User-facing message for a failed key rotation. */
function rotateError(err) {
  if (err?.code === 'undecryptable') return err.message;
  if (err?.status === 409) return 'Your vault changed on another device while the key was being replaced. Nothing was changed; try again.';
  if (err?.status === 413) return 'Your vault is too large to re-encrypt in one step. Export it, reset the vault and import the export instead.';
  return ui.errorMessage(err);
}

/** "Rotate vault key" dialog: re-encrypts the vault under a new key, keeping the master password. */
function openRotateDialog({ vault }) {
  const pw = passwordInput({ name: 'current', autocomplete: 'current-password' });
  const stage = h('div', { class: 'muted small', aria: { live: 'polite' } });
  const body = h('div', { class: 'stack' },
    h('p', { class: 'muted' }, 'A new random vault key replaces the current one and every item and folder is re-encrypted with it on this device. Copies of the old key — an old backup of the server, a stolen device, someone who learned an old master password — can no longer open anything you store from now on.'),
    field({ label: 'Current master password', control: pw, name: 'current' }),
    stage);
  const m = ui.modal({
    title: 'Rotate vault key', size: 'md', body,
    footer: [{ label: 'Cancel', value: false }, { label: 'Rotate key', variant: 'primary', type: 'submit', icon: 'key' }],
    onSubmit: async () => {
      clearErrors(body);
      if (!pw.inputEl.value) {
        showFieldErrors(body, { current: 'Enter your master password.' });
        return false;
      }
      try {
        await vault.session.rotateVaultKey(pw.inputEl.value, undefined, { onStage: (s) => { stage.textContent = ROTATE_STAGES[s] || ''; } });
      } catch (err) {
        stage.textContent = '';
        showFieldErrors(body, { current: err?.code === 'bad_password' ? 'That master password is not correct.' : rotateError(err) });
        return false;
      } finally {
        pw.inputEl.value = '';
      }
      ui.toast.success('Vault key replaced', { description: 'Other devices have to unlock again.' });
      return true;
    },
  });
  return m.closed;
}

export function render(host, { vault, ctx }) {
  ctx.setTitle('Vault settings · Passwords');
  const { session } = vault;

  /* -------------------------------------------------------------- locking */
  const lockSel = h('select', { class: 'select', id: uid('al') }, AUTO_LOCK_CHOICES.map((m) => h('option', { value: String(m) }, minutesLabel(m))));
  lockSel.value = String(vault.prefs.autoLockMinutes);
  lockSel.addEventListener('change', () => {
    vault.setPrefs({ autoLockMinutes: Number(lockSel.value) });
    vault.autolock.touch();
    ui.toast.success('Auto-lock updated');
  });
  const hide = h('input', { type: 'checkbox', role: 'switch', class: 'switch-input', checked: vault.prefs.lockOnHide, aria: { label: 'Lock when the tab is hidden' } });
  hide.addEventListener('change', () => vault.setPrefs({ lockOnHide: hide.checked }));
  const lockCard = ui.card({
    title: 'Locking', icon: 'lock',
    body: h('div', { class: 'setting-rows' },
      h('div', { class: 'setting-row' },
        h('div', { class: 'setting-row-text' }, h('label', { class: 'setting-label', for: lockSel.id }, 'Lock after inactivity'),
          h('span', { class: 'setting-desc' }, 'The decrypted vault is kept in this tab’s memory only and locks when you have been idle this long, anywhere in Ludworks.')),
        h('div', { class: 'vault-setting-control' }, lockSel)),
      h('div', { class: 'setting-row' },
        h('div', { class: 'setting-row-text' }, h('span', { class: 'setting-label' }, 'Lock when the tab is hidden'),
          h('span', { class: 'setting-desc' }, 'Lock as soon as you switch to another tab or minimise the window.')),
        h('label', { class: 'switch' }, hide, h('span', { class: 'switch-track', aria: { hidden: 'true' } }))),
      h('div', { class: 'setting-row' },
        h('div', { class: 'setting-row-text' }, h('span', { class: 'setting-label' }, 'Clipboard'),
          h('span', { class: 'setting-desc' }, 'Copied passwords and codes are cleared from the clipboard after 30 seconds while this page has focus.'))),
      h('div', { class: 'row' }, ui.button({ label: 'Lock now', icon: 'lock', onClick: () => vault.lock('') }))),
  });

  /* ------------------------------------------------------ master password */
  const cur = passwordInput({ name: 'current', autocomplete: 'current-password' });
  const next = passwordInput({ name: 'next', autocomplete: 'new-password' });
  const conf = passwordInput({ name: 'confirm', autocomplete: 'new-password' });
  const meter = strengthBar();
  next.inputEl.addEventListener('input', () => meter.update(next.inputEl.value));
  const save = ui.button({ label: 'Change master password', variant: 'primary', type: 'submit' });
  const stage = h('div', { class: 'muted small', aria: { live: 'polite' } });
  const rotate = checkbox({ label: 'Also replace the vault key (recommended if your old master password or a device may have been compromised)', name: 'rotate' });
  const pwForm = h('form', { class: 'form-grid', novalidate: true },
    field({ label: 'Current master password', control: cur, name: 'current' }),
    h('div', { class: 'form-grid cols-2' },
      h('div', { class: 'stack-sm' }, field({ label: 'New master password', control: next, name: 'next' }), meter.el),
      field({ label: 'Confirm new master password', control: conf, name: 'confirm' })),
    rotate,
    h('div', { class: 'row between wrap' }, stage, save));
  pwForm.addEventListener('submit', async (e) => {
    e.preventDefault();
    clearErrors(pwForm);
    const errs = {};
    const nv = next.inputEl.value;
    if (!cur.inputEl.value) errs.current = 'Enter your current master password.';
    if (nv.length < MIN_MASTER_LENGTH) errs.next = `Use at least ${MIN_MASTER_LENGTH} characters.`;
    else if ((meter.update(nv)?.score ?? 0) < 2) errs.next = 'This password is too easy to guess.';
    else if (nv === cur.inputEl.value) errs.next = 'Choose a password different from the current one.';
    else if (conf.inputEl.value !== nv) errs.confirm = 'The passwords don’t match.';
    if (Object.keys(errs).length) {
      showFieldErrors(pwForm, errs);
      return;
    }
    ui.setLoading(save, true);
    const rotating = rotate.inputEl.checked;
    stage.textContent = rotating ? ROTATE_STAGES.verify : 'Re-encrypting your vault key (two key derivations)…';
    try {
      if (rotating) await session.rotateVaultKey(cur.inputEl.value, nv, { onStage: (s) => { stage.textContent = ROTATE_STAGES[s] || ''; } });
      else await session.changeMasterPassword(cur.inputEl.value, nv);
      for (const el of [cur, next, conf]) el.inputEl.value = '';
      rotate.inputEl.checked = false;
      meter.update('');
      stage.textContent = '';
      ui.toast.success(rotating ? 'Master password and vault key changed' : 'Master password changed', { description: 'Use the new one to unlock on every device.' });
    } catch (err) {
      stage.textContent = '';
      if (err?.code === 'bad_password') showFieldErrors(pwForm, { current: 'That master password is not correct.' });
      else if (rotating) showFieldErrors(pwForm, { current: rotateError(err) });
      else if (err?.status === 409) showFieldErrors(pwForm, { current: 'The master password was changed on another device. Unlock again with the new one.' });
      else showFieldErrors(pwForm, { current: ui.errorMessage(err) });
    } finally {
      ui.setLoading(save, false);
    }
  });
  const p = session.params || {};
  const pwCard = ui.card({
    title: 'Master password', icon: 'key',
    description: 'Changing it re-wraps your vault key; items stay as they are and the work factor is never lowered. Changing the password alone does not lock out anyone who already has an old copy of the wrapped key and the old password: tick “Also replace the vault key” if that may be the case. Other devices need the new password the next time they unlock.',
    body: h('div', { class: 'stack' }, pwForm,
      ui.kv([['Key derivation', `PBKDF2-SHA256 · ${Number(p.kdf_iterations || 0).toLocaleString()} iterations`], ['Encryption', 'AES-256-GCM']])),
  });

  /* ------------------------------------------------------------- vault key */
  const keyCard = ui.card({
    title: 'Vault key', icon: 'shield-check',
    description: 'The random key that encrypts your items. Replace it if a device, a backup of the server or an old master password may have been compromised: everything is re-encrypted with a new key and other devices have to unlock again.',
    body: h('div', { class: 'row' }, ui.button({ label: 'Rotate vault key…', icon: 'key', onClick: () => openRotateDialog({ vault }) })),
  });

  /* ------------------------------------------------------------- danger */
  const danger = ui.card({
    title: 'Reset vault', icon: 'alert', className: 'danger-zone',
    description: 'Forgot your master password? Delete the vault and start over. This cannot be undone.',
    body: h('div', { class: 'row' }, ui.button({ label: 'Reset vault…', icon: 'trash', variant: 'danger', onClick: () => openResetDialog({ vault, ctx }) })),
  });

  host.replaceChildren(h('div', { class: 'page narrow vault-page stack-lg' },
    ui.pageHeader({ title: 'Vault settings', subtitle: 'How your vault locks, your master password and recovery.' }),
    lockCard, pwCard, keyCard, danger));
  return () => {
    for (const el of [cur, next, conf]) el.inputEl.value = '';
  };
}
