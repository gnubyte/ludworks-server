// Self-service registration (only when the admin enabled it).

import { h } from '../core/dom.js';
import { field, input, passwordInput, strengthMeter, clearErrors, showFieldErrors } from '../core/forms.js';
import { button, setLoading } from '../core/ui.js';
import { authLayout, setBanner, authErrorText } from './auth-common.js';

export function mount(container, ctx) {
  ctx.setTitle('Create account');

  if (!ctx.settings.registration_enabled) {
    container.replaceChildren(authLayout({
      title: 'Registration is closed', icon: 'lock',
      subtitle: 'New accounts on this server are created by an administrator. Ask them for an invitation.',
      body: h('div', { class: 'auth-actions' }, button({ label: 'Back to sign in', variant: 'primary', block: true, href: '/login' })),
      settings: ctx.settings,
    }));
    return undefined;
  }

  const username = input({ name: 'username', autocomplete: 'username', autocapitalize: 'off', spellcheck: 'false', required: true });
  const email = input({ name: 'email', type: 'email', autocomplete: 'email', required: true });
  const pw = passwordInput({ name: 'password', autocomplete: 'new-password', required: true });
  const confirm = passwordInput({ name: 'confirm', autocomplete: 'new-password', required: true });
  const meter = strengthMeter(pw.inputEl, () => [username.value, email.value]);
  const submit = button({ label: 'Create account', variant: 'primary', type: 'submit', block: true, size: 'lg' });
  const form = h('form', { class: 'auth-form', novalidate: true },
    field({ label: 'Username', control: username, name: 'username', hint: 'Letters, numbers, dots, dashes and underscores.' }),
    field({ label: 'Email', control: email, name: 'email' }),
    h('div', { class: 'stack-sm' }, field({ label: 'Password', control: pw, name: 'password' }), meter),
    field({ label: 'Confirm password', control: confirm, name: 'confirm' }),
    submit);

  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    clearErrors(form);
    const body = { username: username.value.trim(), email: email.value.trim(), password: pw.inputEl.value };
    const problems = {};
    if (!body.username) problems.username = 'Choose a username.';
    if (!body.email || !email.checkValidity()) problems.email = 'Enter a valid email address.';
    if (!body.password) problems.password = 'Choose a password.';
    else if (body.password !== confirm.inputEl.value) problems.confirm = 'Passwords don’t match.';
    if (Object.keys(problems).length) {
      showFieldErrors(form, problems);
      return;
    }
    setLoading(submit, true);
    try {
      const res = await ctx.api.post('/auth/register', body, { noRedirect: true });
      if (res?.verification_required) {
        renderVerify(body.email);
        return;
      }
      // Registration signed us in with a cookie; fetch the CSRF token via the session.
      const session = await ctx.api.get('/auth/session', { noRedirect: true });
      await ctx.completeLogin(session, '/');
      ctx.ui.toast.success('Welcome to Ludworks!');
    } catch (err) {
      if (err.fields) showFieldErrors(form, err.fields);
      else setBanner(form, authErrorText(err));
    } finally {
      if (submit.isConnected) setLoading(submit, false);
    }
  });

  function renderVerify(addr) {
    container.replaceChildren(authLayout({
      title: 'Check your inbox', icon: 'mail', iconVariant: 'success',
      subtitle: ['We sent a verification link to ', h('strong', {}, addr), '. Open it to activate your account.'],
      body: h('div', { class: 'auth-actions' }, button({ label: 'Back to sign in', variant: 'primary', block: true, href: '/login' })),
      settings: ctx.settings,
    }));
  }

  container.replaceChildren(authLayout({
    title: 'Create your account',
    subtitle: `Join ${ctx.settings.server_name || 'Ludworks'}.`,
    body: form,
    footer: ['Already have an account? ', h('a', { href: '/login' }, 'Sign in')],
    settings: ctx.settings,
  }));
  requestAnimationFrame(() => username.focus());
  return undefined;
}
