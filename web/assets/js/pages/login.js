// Sign-in page: username/email + password, then a TOTP or recovery-code step
// when the server answers 401 totp_required.

import { h } from '../core/dom.js';
import { field, input, passwordInput, clearErrors, setFieldError } from '../core/forms.js';
import { button, setLoading } from '../core/ui.js';
import { authLayout, deviceName, setBanner, authErrorText } from './auth-common.js';

export function mount(container, ctx) {
  ctx.setTitle('Sign in');
  // Credentials live only in this closure between the two steps and are dropped afterwards.
  let creds = null;

  function renderPassword(prefill = '', message = '') {
    const userInput = input({ name: 'username', autocomplete: 'username', autocapitalize: 'off', spellcheck: 'false', required: true, autofocus: true, value: prefill });
    const pw = passwordInput({ name: 'password', autocomplete: 'current-password', required: true });
    const submit = button({ label: 'Sign in', variant: 'primary', type: 'submit', block: true, size: 'lg' });
    const form = h('form', { class: 'auth-form', novalidate: true },
      field({ label: 'Username or email', control: userInput, name: 'username' }),
      field({
        label: 'Password', control: pw, name: 'password',
        labelExtra: h('a', { href: '/forgot-password', class: 'small' }, 'Forgot password?'),
      }),
      submit);

    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      clearErrors(form);
      const username = userInput.value.trim();
      const password = pw.inputEl.value;
      if (!username) {
        setFieldError(userInput.closest('.field'), 'Enter your username or email.');
        userInput.focus();
        return;
      }
      if (!password) {
        setFieldError(pw.closest('.field'), 'Enter your password.');
        pw.inputEl.focus();
        return;
      }
      setLoading(submit, true);
      try {
        const res = await ctx.api.post('/auth/login', { username, password, device_name: deviceName() }, { noRedirect: true });
        await ctx.completeLogin(res);
      } catch (err) {
        if (err.code === 'totp_required') {
          creds = { username, password };
          renderTotp();
          return;
        }
        setBanner(form, authErrorText(err));
        if (err.code === 'invalid_credentials') {
          pw.inputEl.value = '';
          pw.inputEl.focus();
        }
      } finally {
        if (submit.isConnected) setLoading(submit, false);
      }
    });

    const reg = ctx.settings.registration_enabled
      ? ['New here? ', h('a', { href: '/register' }, 'Create an account')]
      : null;
    container.replaceChildren(authLayout({
      title: `Sign in to ${ctx.settings.server_name || 'Ludworks'}`,
      subtitle: 'Welcome back. Enter your details to continue.',
      body: form, footer: reg, settings: ctx.settings,
    }));
    if (message) setBanner(form, message);
    requestAnimationFrame(() => (prefill ? pw.inputEl : userInput).focus());
  }

  function renderTotp() {
    let mode = 'totp';
    const code = input({ name: 'totp_code', className: 'otp-input', inputmode: 'numeric', autocomplete: 'one-time-code', maxlength: '6', pattern: '[0-9]{6}', placeholder: '000000', 'aria-label': 'Authentication code' });
    const recovery = input({ name: 'recovery_code', className: 'mono', autocomplete: 'off', autocapitalize: 'off', spellcheck: 'false', placeholder: 'xxxx-xxxx-xxxx-xxxx' });
    const codeField = field({ label: 'Authentication code', control: code, name: 'totp_code' });
    const recField = field({ label: 'Recovery code', control: recovery, name: 'recovery_code', hint: 'Each recovery code works once.' });
    recField.hidden = true;
    const submit = button({ label: 'Verify', variant: 'primary', type: 'submit', block: true, size: 'lg' });
    const toggle = h('button', { type: 'button', class: 'link-button small' }, 'Use a recovery code instead');
    const back = h('button', { type: 'button', class: 'link-button small' }, 'Use a different account');
    const form = h('form', { class: 'auth-form', novalidate: true }, codeField, recField, submit,
      h('div', { class: 'auth-row' }, toggle, back));

    toggle.addEventListener('click', () => {
      mode = mode === 'totp' ? 'recovery' : 'totp';
      codeField.hidden = mode !== 'totp';
      recField.hidden = mode === 'totp';
      toggle.textContent = mode === 'totp' ? 'Use a recovery code instead' : 'Use your authenticator app';
      clearErrors(form);
      (mode === 'totp' ? code : recovery).focus();
    });
    back.addEventListener('click', () => {
      const name = creds?.username || '';
      creds = null;
      renderPassword(name);
    });
    code.addEventListener('input', () => {
      code.value = code.value.replace(/\D/g, '').slice(0, 6);
      if (code.value.length === 6) form.requestSubmit();
    });

    form.addEventListener('submit', async (e) => {
      e.preventDefault();
      if (!creds || submit.classList.contains('is-loading')) return;
      clearErrors(form);
      const body = { username: creds.username, password: creds.password, device_name: deviceName() };
      if (mode === 'totp') {
        if (!/^\d{6}$/.test(code.value)) {
          setFieldError(codeField, 'Enter the 6-digit code.');
          code.focus();
          return;
        }
        body.totp_code = code.value;
      } else {
        const v = recovery.value.trim();
        if (!v) {
          setFieldError(recField, 'Enter one of your recovery codes.');
          recovery.focus();
          return;
        }
        body.recovery_code = v;
      }
      setLoading(submit, true);
      try {
        const res = await ctx.api.post('/auth/login', body, { noRedirect: true });
        creds = null;
        await ctx.completeLogin(res);
      } catch (err) {
        if (err.code === 'totp_required' || err.code === 'invalid_credentials' || err.status === 422) {
          setFieldError(mode === 'totp' ? codeField : recField, mode === 'totp' ? 'That code didn’t work. Wait for a new code and try again.' : 'That recovery code isn’t valid.');
          (mode === 'totp' ? code : recovery).select();
        } else {
          setBanner(form, authErrorText(err));
        }
      } finally {
        if (submit.isConnected) setLoading(submit, false);
      }
    });

    container.replaceChildren(authLayout({
      title: 'Two-factor authentication', icon: 'shield-check',
      subtitle: 'Enter the 6-digit code from your authenticator app.',
      body: form, settings: ctx.settings,
    }));
    requestAnimationFrame(() => code.focus());
  }

  renderPassword(ctx.query.username || '');
  return () => { creds = null; };
}
