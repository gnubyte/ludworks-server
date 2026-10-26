// Choose a new password from an emailed reset link (?token=...). The token is
// removed from the address bar immediately and kept only in memory.

import { h } from '../core/dom.js';
import { field, passwordInput, strengthMeter, clearErrors, showFieldErrors } from '../core/forms.js';
import { button, setLoading } from '../core/ui.js';
import { authLayout, scrubQuery, setBanner, authErrorText } from './auth-common.js';

export function mount(container, ctx) {
  ctx.setTitle('Choose a new password');
  const token = ctx.query.token || '';
  scrubQuery(['token']);

  const invalid = () => container.replaceChildren(authLayout({
    title: 'This link isn’t valid', icon: 'alert', iconVariant: 'danger',
    subtitle: 'The reset link is invalid, already used, or expired. Request a new one to continue.',
    body: h('div', { class: 'auth-actions' }, button({ label: 'Request a new link', variant: 'primary', block: true, href: '/forgot-password' })),
    footer: [h('a', { href: '/login' }, 'Back to sign in')],
    settings: ctx.settings,
  }));

  if (!token) {
    invalid();
    return;
  }

  const pw = passwordInput({ name: 'new_password', autocomplete: 'new-password', required: true });
  const confirm = passwordInput({ name: 'confirm', autocomplete: 'new-password', required: true });
  const submit = button({ label: 'Update password', variant: 'primary', type: 'submit', block: true, size: 'lg' });
  const form = h('form', { class: 'auth-form', novalidate: true },
    h('div', { class: 'stack-sm' }, field({ label: 'New password', control: pw, name: 'new_password' }), strengthMeter(pw.inputEl)),
    field({ label: 'Confirm new password', control: confirm, name: 'confirm' }),
    submit);

  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    clearErrors(form);
    const value = pw.inputEl.value;
    if (!value) {
      showFieldErrors(form, { new_password: 'Choose a new password.' });
      return;
    }
    if (value !== confirm.inputEl.value) {
      showFieldErrors(form, { confirm: 'Passwords don’t match.' });
      return;
    }
    setLoading(submit, true);
    try {
      await ctx.api.post('/auth/password-reset/confirm', { token, new_password: value }, { noRedirect: true });
      container.replaceChildren(authLayout({
        title: 'Password updated', icon: 'check-circle', iconVariant: 'success',
        subtitle: 'Your password was changed and every existing session was signed out.',
        body: h('div', { class: 'auth-actions' }, button({ label: 'Sign in', variant: 'primary', block: true, href: '/login' })),
        settings: ctx.settings,
      }));
    } catch (err) {
      if (err.fields) showFieldErrors(form, err.fields);
      else if ([400, 404, 410].includes(err.status) || err.code === 'unauthorized') invalid();
      else setBanner(form, authErrorText(err));
    } finally {
      if (submit.isConnected) setLoading(submit, false);
    }
  });

  container.replaceChildren(authLayout({
    title: 'Choose a new password', icon: 'key',
    subtitle: 'Pick something long and unique — a passphrase works well.',
    body: form, settings: ctx.settings,
  }));
  requestAnimationFrame(() => pw.inputEl.focus());
}
