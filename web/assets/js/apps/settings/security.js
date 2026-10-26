// Password change and TOTP two-factor setup (QR via lib/qrcode.js), recovery
// codes display/download, and disabling 2FA.

import { h, downloadBlob } from '../../core/dom.js';
import { field, input, passwordInput, strengthMeter, clearErrors, showFormError, showFieldErrors } from '../../core/forms.js';
import { setUser } from '../../core/store.js';
import { encodeQR, qrToSvg } from '../../lib/qrcode.js';

/** Group a base32 secret in blocks of 4 for readability. */
function groupSecret(secret) {
  return String(secret || '').replace(/\s+/g, '').match(/.{1,4}/g)?.join(' ') ?? '';
}

function otpInput(name = 'code') {
  const el = input({ name, className: 'otp-input', inputmode: 'numeric', autocomplete: 'one-time-code', maxlength: '6', pattern: '[0-9]{6}', placeholder: '000000' });
  el.addEventListener('input', () => { el.value = el.value.replace(/\D/g, '').slice(0, 6); });
  return el;
}

export function render(host, ctx) {
  const { ui } = ctx;

  /* --------------------------------------------------------------- password */
  const current = passwordInput({ name: 'current_password', autocomplete: 'current-password' });
  const next = passwordInput({ name: 'new_password', autocomplete: 'new-password' });
  const confirm = passwordInput({ name: 'confirm', autocomplete: 'new-password' });
  const pwSave = ui.button({ label: 'Change password', variant: 'primary', type: 'submit' });
  const pwForm = h('form', { class: 'form-grid', novalidate: true },
    field({ label: 'Current password', control: current, name: 'current_password' }),
    h('div', { class: 'form-grid cols-2' },
      h('div', { class: 'stack-sm' }, field({ label: 'New password', control: next, name: 'new_password' }),
        strengthMeter(next.inputEl, () => [ctx.user?.username, ctx.user?.email])),
      field({ label: 'Confirm new password', control: confirm, name: 'confirm' })),
    h('div', { class: 'row between wrap' }, h('span', { class: 'small muted' }, 'Changing your password signs out your other sessions.'), pwSave));

  pwForm.addEventListener('submit', async (e) => {
    e.preventDefault();
    clearErrors(pwForm);
    const body = { current_password: current.inputEl.value, new_password: next.inputEl.value };
    const problems = {};
    if (!body.current_password) problems.current_password = 'Enter your current password.';
    if (!body.new_password) problems.new_password = 'Choose a new password.';
    else if (body.new_password !== confirm.inputEl.value) problems.confirm = 'Passwords don’t match.';
    else if (body.new_password === body.current_password) problems.new_password = 'Choose a password different from the current one.';
    if (Object.keys(problems).length) {
      showFieldErrors(pwForm, problems);
      return;
    }
    await ui.withLoading(pwSave, async () => {
      try {
        await ctx.api.post('/me/password', body);
        for (const el of [current, next, confirm]) el.inputEl.value = '';
        next.inputEl.dispatchEvent(new Event('input'));
        ui.toast.success('Password changed', { description: 'Your other sessions were signed out.' });
      } catch (err) {
        if (err.code === 'invalid_credentials' || (err.status === 401 && err.code !== 'unauthorized')) {
          showFieldErrors(pwForm, { current_password: 'Your current password is incorrect.' });
        } else if (err.status === 403) {
          showFieldErrors(pwForm, { current_password: err.message || 'Your current password is incorrect.' });
        } else {
          showFormError(pwForm, err);
        }
      }
    });
  });

  /* ------------------------------------------------------------ two-factor */
  const totpBody = h('div');
  const totpCard = ui.card({ title: 'Two-factor authentication', icon: 'shield-check', body: totpBody });

  function renderTotp() {
    const enabled = !!ctx.user?.totp_enabled;
    totpBody.replaceChildren(h('div', { class: 'setting-rows' },
      h('div', { class: 'setting-row' },
        h('div', { class: 'setting-row-text' },
          h('span', { class: 'setting-label row' }, 'Authenticator app', enabled ? ui.badge('On', 'success', { dot: true }) : ui.badge('Off', 'neutral', { dot: true })),
          h('span', { class: 'setting-desc' }, enabled
            ? 'Sign-ins ask for a 6-digit code from your authenticator app. If you lose your device, use one of your recovery codes.'
            : 'Protect your account with time-based one-time codes from an app like 1Password, Bitwarden, Aegis or Google Authenticator.')),
        enabled
          ? ui.button({ label: 'Disable', variant: 'danger-ghost', onClick: disableTotp })
          : ui.button({ label: 'Set up', icon: 'qr', variant: 'primary', onClick: startSetup })),
      enabled ? h('div', { class: 'setting-row' },
        h('div', { class: 'setting-row-text' },
          h('span', { class: 'setting-label' }, 'Recovery codes'),
          h('span', { class: 'setting-desc' }, 'Recovery codes are shown once when you enable two-factor. To get a fresh set, disable and re-enable it.'))) : null));
  }

  async function startSetup() {
    let setup;
    try {
      setup = await ctx.api.post('/me/totp/setup', {});
    } catch (err) {
      ui.toast.error(err);
      return;
    }
    let qrNode;
    try {
      qrNode = h('div', { class: 'qr-box' }, qrToSvg(encodeQR(setup.otpauth_url, { ecl: 'M' }), { title: 'Authenticator QR code', pixelSize: 196 }));
    } catch {
      qrNode = h('div', { class: 'alert alert-warning' }, 'Could not draw the QR code — enter the key manually.');
    }
    const code = otpInput();
    const codeField = field({ label: 'Verification code', control: code, name: 'code' });
    const m = ui.modal({
      title: 'Set up two-factor authentication',
      size: 'lg',
      body: h('div', { class: 'totp-setup' },
        qrNode,
        h('div', { class: 'totp-steps' },
          h('div', { class: 'totp-step' },
            h('div', { class: 'totp-step-title' }, 'Scan the QR code'),
            h('p', { class: 'small muted' }, 'Open your authenticator app and scan the code. Can’t scan? Enter this key instead:'),
            h('div', { class: 'secret-box' }, h('span', { class: 'secret-text' }, groupSecret(setup.secret)), ui.copyButton(setup.secret, { title: 'Copy key' }))),
          h('div', { class: 'totp-step' },
            h('div', { class: 'totp-step-title' }, 'Enter the 6-digit code'),
            codeField))),
      footer: [{ label: 'Cancel', value: null }, { label: 'Verify & enable', variant: 'primary', type: 'submit' }],
      onSubmit: async () => {
        clearErrors(m.form);
        if (!/^\d{6}$/.test(code.value)) {
          showFieldErrors(m.form, { code: 'Enter the 6-digit code from your app.' });
          return false;
        }
        try {
          const res = await ctx.api.post('/me/totp/enable', { code: code.value });
          setUser({ ...ctx.user, totp_enabled: true });
          renderTotp();
          m.close(true);
          showRecoveryCodes(res?.recovery_codes || []);
          return true;
        } catch (err) {
          if (err.status === 400 || err.status === 401 || err.status === 422) {
            showFieldErrors(m.form, { code: err.fields?.code || 'That code didn’t match. Check your device clock and try the next code.' });
            code.select();
          } else {
            showFormError(m.form, err, { bannerHost: m.body });
          }
          return false;
        }
      },
    });
    code.addEventListener('input', () => {
      if (code.value.length === 6) m.form.requestSubmit();
    });
  }

  function showRecoveryCodes(codes) {
    const text = `Ludworks recovery codes for ${ctx.user?.username} (${location.host})\nGenerated ${new Date().toISOString()}\nEach code can be used once.\n\n${codes.join('\n')}\n`;
    const m = ui.modal({
      title: 'Save your recovery codes',
      description: 'If you lose your authenticator, each of these codes lets you sign in once. They won’t be shown again.',
      dismissible: false,
      body: h('div', { class: 'stack' },
        h('ol', { class: 'recovery-codes' }, codes.map((c) => h('li', {}, c))),
        h('div', { class: 'row wrap' },
          ui.button({ label: 'Copy all', icon: 'copy', size: 'sm', onClick: () => ui.copyText(codes.join('\n'), { message: 'Recovery codes copied' }) }),
          ui.button({ label: 'Download .txt', icon: 'download', size: 'sm', onClick: () => downloadBlob(new Blob([text], { type: 'text/plain' }), 'ludworks-recovery-codes.txt') }))),
      footer: [{ label: 'I’ve saved them', variant: 'primary', value: true }],
    });
    m.closed.then(() => ui.toast.success('Two-factor authentication is on'));
  }

  async function disableTotp() {
    const pw = passwordInput({ name: 'password', autocomplete: 'current-password' });
    const code = otpInput();
    const m = ui.modal({
      title: 'Disable two-factor authentication?',
      description: 'Your account will be protected by your password only.',
      size: 'sm',
      body: h('div', { class: 'stack' },
        field({ label: 'Password', control: pw, name: 'password' }),
        field({ label: 'Authentication code', control: code, name: 'code' })),
      footer: [{ label: 'Cancel', value: null }, { label: 'Disable 2FA', variant: 'danger', type: 'submit' }],
      onSubmit: async () => {
        clearErrors(m.form);
        if (!pw.inputEl.value || !/^\d{6}$/.test(code.value)) {
          showFieldErrors(m.form, { ...(pw.inputEl.value ? {} : { password: 'Enter your password.' }), ...(/^\d{6}$/.test(code.value) ? {} : { code: 'Enter a 6-digit code.' }) });
          return false;
        }
        try {
          await ctx.api.post('/me/totp/disable', { password: pw.inputEl.value, code: code.value });
          setUser({ ...ctx.user, totp_enabled: false });
          renderTotp();
          ui.toast.success('Two-factor authentication disabled');
          return true;
        } catch (err) {
          if (err.fields) showFieldErrors(m.form, err.fields);
          else if ([400, 401, 403].includes(err.status) && err.code !== 'unauthorized') showFormError(m.form, { message: 'The password or code is incorrect.' }, { bannerHost: m.body });
          else showFormError(m.form, err, { bannerHost: m.body });
          return false;
        }
      },
    });
  }

  renderTotp();
  host.append(
    h('div', { class: 'settings-section-header' },
      h('h2', { class: 'settings-section-title' }, 'Password & two-factor'),
      h('p', { class: 'settings-section-desc' }, 'Keep your account secure.')),
    ui.card({ title: 'Change password', icon: 'lock', body: pwForm }),
    totpCard,
    ui.alert({ variant: 'info', icon: 'info', title: 'Signed in on other devices?', message: h('span', {}, 'Review and revoke them on the ', h('a', { href: '/settings/sessions' }, 'Sessions'), ' page.') }),
  );
}
