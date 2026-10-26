// Request a password-reset email. The server always answers 202 so this page
// never reveals whether an account exists.

import { h } from '../core/dom.js';
import { field, input, clearErrors, setFieldError } from '../core/forms.js';
import { button, setLoading } from '../core/ui.js';
import { authLayout, setBanner, authErrorText } from './auth-common.js';

export function mount(container, ctx) {
  ctx.setTitle('Reset password');
  const email = input({ name: 'email', type: 'email', autocomplete: 'email', required: true, value: ctx.query.email || '' });
  const submit = button({ label: 'Send reset link', variant: 'primary', type: 'submit', block: true, size: 'lg' });
  const form = h('form', { class: 'auth-form', novalidate: true },
    field({ label: 'Email address', control: email, name: 'email' }), submit);

  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    clearErrors(form);
    const addr = email.value.trim();
    if (!addr || !email.checkValidity()) {
      setFieldError(email.closest('.field'), 'Enter the email address on your account.');
      email.focus();
      return;
    }
    setLoading(submit, true);
    try {
      await ctx.api.post('/auth/password-reset/request', { email: addr }, { noRedirect: true });
      container.replaceChildren(authLayout({
        title: 'Check your email', icon: 'mail', iconVariant: 'success',
        subtitle: ['If an account exists for ', h('strong', {}, addr), ', we sent a link to reset its password. The link expires soon and works once.'],
        body: h('div', { class: 'auth-actions' },
          button({ label: 'Back to sign in', variant: 'primary', block: true, href: '/login' }),
          h('p', { class: 'small muted center' }, 'Nothing arrived? Check spam, or ask your administrator — the server needs email (SMTP) configured to send resets.')),
        settings: ctx.settings,
      }));
    } catch (err) {
      setBanner(form, authErrorText(err));
    } finally {
      if (submit.isConnected) setLoading(submit, false);
    }
  });

  container.replaceChildren(authLayout({
    title: 'Forgot your password?', icon: 'key',
    subtitle: 'Enter your email and we’ll send you a link to choose a new one.',
    body: form,
    footer: ['Remembered it? ', h('a', { href: '/login' }, 'Sign in')],
    settings: ctx.settings,
  }));
  requestAnimationFrame(() => email.focus());
}
