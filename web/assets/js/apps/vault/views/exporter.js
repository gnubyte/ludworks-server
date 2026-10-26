// Export: KeePass .kdbx (encrypted with a chosen password, optional key file, KeePass-default
// Argon2d settings, built in the worker), Bitwarden JSON (plain or password-protected),
// Bitwarden CSV and KeePass XML (plaintext — strong warnings). Requires re-entering the master
// password. Files are handed to the browser as Blob downloads; nothing is sent to the server.

import { h, cls, uid, downloadBlob } from '../../../core/dom.js';
import { icon } from '../../../core/icons.js';
import { field, passwordInput, checkbox, clearErrors, showFieldErrors } from '../../../core/forms.js';
import * as ui from '../../../core/ui.js';
import { runJob, ioErrorMessage } from '../workerclient.js';
import { strengthBar, vicon } from '../widgets.js';

/** KeePass 2.x defaults for new databases (VAULT-CRYPTO.md §6). */
export const KDBX_EXPORT_KDF = Object.freeze({ type: 'argon2d', memory: 64 * 1024 * 1024, iterations: 2, parallelism: 2 });

const FORMATS = [
  { id: 'kdbx', label: 'KeePass database', ext: '.kdbx', desc: 'Encrypted with a password you choose (and optionally a key file). Opens in KeePass, KeePassXC and compatible apps.', icon: 'lock', encrypted: true, badge: 'Recommended' },
  { id: 'bitwarden-json', label: 'Bitwarden JSON', ext: '.json', desc: 'For Bitwarden and many other password managers. Can be password protected.', icon: 'file-text', encrypted: 'optional' },
  { id: 'bitwarden-csv', label: 'Bitwarden CSV', ext: '.csv', desc: 'Spreadsheet-style. Unencrypted; cards and identities become notes.', icon: 'sheet', encrypted: false },
  { id: 'keepass-xml', label: 'KeePass XML', ext: '.xml', desc: 'KeePass 2 XML. Unencrypted.', icon: 'code', encrypted: false },
];

const MIME = { kdbx: 'application/octet-stream', 'bitwarden-json': 'application/json', 'bitwarden-csv': 'text/csv', 'keepass-xml': 'application/xml' };

export function exportFileName(format, now = new Date()) {
  const d = now.toISOString().slice(0, 10).replaceAll('-', '');
  return `ludworks-vault-${d}${FORMATS.find((f) => f.id === format)?.ext || ''}`;
}

export function render(host, { vault, ctx }) {
  ctx.setTitle('Export · Passwords');
  const { session } = vault;
  const ctrl = new AbortController();
  let format = 'kdbx';
  let keyFile = null; // {name, bytes}

  const total = [...session.items.values()].filter((r) => r.plain).length;
  const broken = session.items.size - total;
  const folders = [...session.folders.values()].filter((f) => !f.error).length;

  const formatCards = h('div', { class: 'vault-format-grid', role: 'radiogroup', aria: { label: 'Export format' } });
  const options = h('div', { class: 'stack' });
  const master = passwordInput({ name: 'master', autocomplete: 'current-password' });
  const go = ui.button({ label: 'Export', icon: 'download', variant: 'primary', type: 'submit' });
  const status = h('div', { class: 'stack-sm', aria: { live: 'polite' } });
  const form = h('form', { class: 'stack-lg', novalidate: true },
    h('section', { class: 'card' }, h('div', { class: 'card-body stack' }, h('h3', { class: 'card-title' }, '1. Format'), formatCards)),
    h('section', { class: 'card' }, h('div', { class: 'card-body stack' }, h('h3', { class: 'card-title' }, '2. Options'), options)),
    h('section', { class: 'card' }, h('div', { class: 'card-body stack' }, h('h3', { class: 'card-title' }, '3. Confirm'),
      field({ label: 'Master password', control: master, name: 'master', hint: 'Exporting reveals your whole vault, so confirm it is you.' }),
      status, h('div', { class: 'row end' }, go))));

  host.replaceChildren(h('div', { class: 'page narrow vault-page' },
    ui.pageHeader({ title: 'Export', subtitle: `${total} item${total === 1 ? '' : 's'} and ${folders} folder${folders === 1 ? '' : 's'} from your vault, prepared on this device.` }),
    broken ? ui.alert({ variant: 'warning', message: `${broken} item${broken === 1 ? '' : 's'} that cannot be decrypted will be left out.` }) : null,
    form));

  /* ------------------------------------------------------------ options */
  let exportPw = null;
  let confirmPw = null;
  let protect = null;
  let ack = null;
  let meter = null;

  function renderFormats() {
    formatCards.replaceChildren(...FORMATS.map((f) => {
      const on = f.id === format;
      const b = h('button', { type: 'button', role: 'radio', class: cls('vault-format', { 'is-active': on }), aria: { checked: String(on) }, tabindex: on ? '0' : '-1' },
        h('span', { class: 'vault-format-icon' }, vicon(f.icon, 18)),
        h('span', { class: 'vault-format-text' },
          h('span', { class: 'vault-format-name' }, f.label, f.badge ? ui.badge(f.badge, 'success') : null, f.encrypted === false ? ui.badge('Unencrypted', 'danger') : null),
          h('span', { class: 'vault-format-desc' }, f.desc)));
      b.addEventListener('click', () => {
        format = f.id;
        renderFormats();
        renderOptions();
        formatCards.querySelector('.is-active')?.focus();
      });
      return b;
    }));
  }
  formatCards.addEventListener('keydown', (e) => {
    const i = FORMATS.findIndex((f) => f.id === format);
    const d = e.key === 'ArrowDown' || e.key === 'ArrowRight' ? 1 : e.key === 'ArrowUp' || e.key === 'ArrowLeft' ? -1 : 0;
    if (!d) return;
    e.preventDefault();
    formatCards.children[(i + d + FORMATS.length) % FORMATS.length].click();
  });

  function passwordFields(label) {
    exportPw = passwordInput({ name: 'export_password', autocomplete: 'new-password' });
    confirmPw = passwordInput({ name: 'export_confirm', autocomplete: 'new-password' });
    meter = strengthBar();
    exportPw.inputEl.addEventListener('input', () => meter.update(exportPw.inputEl.value));
    return h('div', { class: 'form-grid cols-2' },
      h('div', { class: 'stack-sm' }, field({ label, control: exportPw, name: 'export_password', hint: 'Not your master password: choose a separate one and store it safely.' }), meter.el),
      field({ label: 'Confirm password', control: confirmPw, name: 'export_confirm' }));
  }

  function renderOptions() {
    exportPw = confirmPw = protect = ack = meter = null;
    const f = FORMATS.find((x) => x.id === format);
    const parts = [];
    if (format === 'kdbx') {
      parts.push(passwordFields('Database password'));
      const kfLabel = h('span', { class: 'muted small truncate' }, keyFile ? keyFile.name : 'None');
      const choose = ui.button({ label: 'Use existing…', icon: 'key', size: 'sm' });
      const gen = ui.button({ label: 'Generate new key file', icon: 'plus', size: 'sm' });
      const clear = ui.button({ icon: 'x', title: 'Do not use a key file', size: 'sm', variant: 'ghost' });
      clear.hidden = !keyFile;
      const setKf = (kf) => {
        keyFile = kf;
        kfLabel.textContent = kf ? kf.name : 'None';
        clear.hidden = !kf;
      };
      choose.addEventListener('click', async () => {
        const [file] = await pickOne();
        if (!file) return;
        if (file.size > 10 << 20) {
          ui.toast.error('Key files larger than 10 MiB are not supported.');
          return;
        }
        setKf({ name: file.name, bytes: new Uint8Array(await file.arrayBuffer()) });
      });
      gen.addEventListener('click', async () => {
        try {
          const bytes = await runJob({ op: 'keyfile' }, { signal: ctrl.signal });
          const name = exportFileName('kdbx').replace(/\.kdbx$/, '.keyx');
          downloadBlob(new Blob([bytes], { type: 'application/xml' }), name);
          setKf({ name, bytes });
          ui.toast.success('Key file downloaded', { description: 'Keep it safe: the database cannot be opened without it.' });
        } catch (err) {
          if (err?.name !== 'AbortError') ui.toast.error(ioErrorMessage(err));
        }
      });
      parts.push(h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Key file (optional)'),
        h('div', { class: 'row wrap' }, choose, gen, kfLabel, clear),
        h('div', { class: 'field-hint' }, 'A key file is a second factor: the database then needs both the password and the file.')));
      parts.push(h('p', { class: 'muted small' }, icon('info', 13), ' KDBX 4 · AES-256 · Argon2d (64 MiB, 2 iterations, 2 lanes — the KeePass defaults) · ChaCha20 protected fields.'));
    } else if (format === 'bitwarden-json') {
      protect = checkbox({ label: 'Protect the file with a password', checked: true, hint: 'Bitwarden “password protected” export: PBKDF2-SHA256 (600 000 iterations) and AES-256 with HMAC.' });
      const pwBox = h('div', {}, passwordFields('File password'));
      const plainWarn = h('div', { hidden: true }, plaintextWarning());
      protect.inputEl.addEventListener('change', () => {
        pwBox.hidden = !protect.inputEl.checked;
        plainWarn.hidden = protect.inputEl.checked;
      });
      parts.push(protect, pwBox, plainWarn);
    } else {
      parts.push(plaintextWarning());
    }
    void f;
    options.replaceChildren(...parts);
  }

  function plaintextWarning() {
    ack = checkbox({ label: 'I understand that this file contains all my passwords in plain text.', name: 'ack' });
    return h('div', { class: 'stack-sm' },
      ui.alert({ variant: 'danger', title: 'This file will not be encrypted',
        message: 'Anyone who gets the file can read every password, card number and note in it. Store it only on a device you trust, import it right away and then delete it permanently (including from Downloads and the trash).' }),
      h('div', { class: 'field', dataset: { field: 'ack' } }, ack, h('div', { class: 'field-error', role: 'alert', hidden: true })));
  }

  /* -------------------------------------------------------------- export */
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    clearErrors(form);
    const errs = {};
    const usePw = format === 'kdbx' || (format === 'bitwarden-json' && protect?.inputEl.checked);
    let pw = '';
    if (usePw) {
      pw = exportPw.inputEl.value;
      if (!pw && !(format === 'kdbx' && keyFile)) errs.export_password = 'Choose a password for the file.';
      else if (pw && pw.length < 8) errs.export_password = 'Use at least 8 characters.';
      else if (pw !== confirmPw.inputEl.value) errs.export_confirm = 'The passwords don’t match.';
    } else if (ack && !ack.inputEl.checked) errs.ack = 'Please confirm that you understand the risk.';
    if (!master.inputEl.value) errs.master = 'Enter your master password.';
    if (Object.keys(errs).length) {
      showFieldErrors(form, errs);
      return;
    }
    ui.setLoading(go, true);
    status.replaceChildren(h('div', { class: 'row muted small' }, ui.spinner({ size: 'sm' }), 'Checking your master password…'));
    try {
      const ok = await session.verifyMasterPassword(master.inputEl.value);
      master.inputEl.value = '';
      if (!ok) {
        status.replaceChildren();
        showFieldErrors(form, { master: 'That master password is not correct.' });
        return;
      }
      const data = session.exportData();
      const bar = ui.progress({ value: 0, label: 'Exporting' });
      const line = h('div', { class: 'muted small' }, format === 'kdbx' ? 'Deriving the database key (Argon2d, 64 MiB)…' : 'Preparing the file…');
      status.replaceChildren(bar.el, line);
      const opts = format === 'kdbx'
        ? { password: pw || undefined, keyFile: keyFile?.bytes ? keyFile.bytes.slice() : undefined, kdf: KDBX_EXPORT_KDF, cipher: 'aes', databaseName: 'Ludworks Vault' }
        : format === 'bitwarden-json' && pw ? { password: pw } : {};
      const out = await runJob({ op: 'export', format, data, opts }, {
        signal: ctrl.signal,
        onProgress: (f) => {
          bar.set(f, 1);
          if (format === 'kdbx') line.textContent = f < 1 ? `Deriving the database key… ${Math.round(f * 100)}%` : 'Encrypting…';
        },
      });
      const name = exportFileName(format);
      downloadBlob(new Blob([out], { type: MIME[format] }), name);
      status.replaceChildren(ui.alert({ variant: 'success', title: 'Export ready', message: `${data.items.length} items saved to “${name}”.${usePw ? '' : ' Remember to delete this unencrypted file when you no longer need it.'}` }));
      if (exportPw) exportPw.inputEl.value = '';
      if (confirmPw) confirmPw.inputEl.value = '';
      meter?.update('');
    } catch (err) {
      if (err?.name === 'AbortError') return;
      status.replaceChildren(ui.alert({ variant: 'danger', title: 'Export failed', message: ioErrorMessage(err) }));
    } finally {
      ui.setLoading(go, false);
    }
  });

  renderFormats();
  renderOptions();
  return () => {
    ctrl.abort();
    keyFile = null;
  };
}

function pickOne() {
  return new Promise((resolve) => {
    const input = h('input', { type: 'file', class: 'hidden', id: uid('kf') });
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
