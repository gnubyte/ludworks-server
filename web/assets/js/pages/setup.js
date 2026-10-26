// First-run setup wizard: claim the server with the one-time setup token, create
// the first administrator, name the server. All steps live in one form so server
// validation errors can be routed back to the right step.

import { h, cls, mount as fill } from '../core/dom.js';
import { icon } from '../core/icons.js';
import { field, input, passwordInput, strengthMeter, clearErrors, showFieldErrors } from '../core/forms.js';
import { button, setLoading, copyButton } from '../core/ui.js';
import { formatFingerprint } from '../core/format.js';
import { store } from '../core/store.js';
import { authLayout, scrubQuery, setBanner, authErrorText } from './auth-common.js';

const STEPS = [
  { id: 'claim', label: 'Claim' },
  { id: 'admin', label: 'Admin' }, // short: the three steps must fit one line on a 320px phone
  { id: 'server', label: 'Server' },
];
const FIELD_STEP = { setup_token: 0, username: 1, email: 1, password: 1, confirm: 1, server_name: 2 };

export function mount(container, ctx) {
  ctx.setTitle('Set up Ludworks');
  const status = ctx.settings || {};
  const prefillToken = ctx.query.token || '';
  scrubQuery(['token']);
  let step = 0;

  /* --------------------------------------------------------------- fields */
  const token = input({ name: 'setup_token', className: 'mono', autocomplete: 'off', autocapitalize: 'off', spellcheck: 'false', value: prefillToken, placeholder: 'Paste the setup token' });
  // A placeholder, not a value: a pre-filled "admin" made typing a name append to it ("adminpat").
  const username = input({ name: 'username', autocomplete: 'username', autocapitalize: 'off', spellcheck: 'false', placeholder: 'admin' });
  const email = input({ name: 'email', type: 'email', autocomplete: 'email' });
  const pw = passwordInput({ name: 'password', autocomplete: 'new-password' });
  const confirm = passwordInput({ name: 'confirm', autocomplete: 'new-password' });
  const serverName = input({ name: 'server_name', value: status.server_name || 'Ludworks', maxlength: '64' });

  const fp = status.tls_fingerprint_sha256;
  const claimPanel = h('div', { class: 'stack' },
    h('p', { class: 'muted small' }, 'This server has no administrator yet. Prove you control it with the one-time setup token printed in the server log at first start and saved to ',
      h('code', {}, 'setup-token.txt'), ' in the data directory.'),
    field({ label: 'Setup token', control: token, name: 'setup_token' }),
    fp ? h('div', { class: 'fingerprint-box' },
      h('div', { class: 'row between' }, h('span', { class: 'small bold' }, 'TLS certificate fingerprint (SHA-256)'), copyButton(formatFingerprint(fp), { title: 'Copy fingerprint' })),
      h('div', { class: 'fingerprint-value' }, formatFingerprint(fp)),
      h('div', { class: 'xs muted' }, 'Compare with the fingerprint in the server log to make sure you are talking to your own server.'))
      : h('div', { class: 'alert alert-warning' }, icon('alert', { className: 'alert-icon' }),
        h('div', { class: 'alert-body' }, h('div', { class: 'alert-text' }, 'This connection is not using the server’s built-in TLS. Make sure a trusted HTTPS proxy sits in front of it before using it over a network.'))));

  const adminPanel = h('div', { class: 'stack' },
    field({ label: 'Username', control: username, name: 'username' }),
    field({ label: 'Email', control: email, name: 'email', hint: 'Used for password resets and notifications.' }),
    h('div', { class: 'stack-sm' }, field({ label: 'Password', control: pw, name: 'password', hint: 'At least 12 characters. A passphrase is ideal.' }),
      strengthMeter(pw.inputEl, () => [username.value, email.value])),
    field({ label: 'Confirm password', control: confirm, name: 'confirm' }));

  const summary = h('dl', { class: 'kv small' });
  const serverPanel = h('div', { class: 'stack' },
    field({ label: 'Server name', control: serverName, name: 'server_name', hint: 'Shown in the sidebar, page titles and emails.' }),
    summary);
  const panels = [claimPanel, adminPanel, serverPanel];

  /* ---------------------------------------------------------- navigation */
  const stepper = h('div', { class: 'steps', aria: { label: 'Setup progress' } });
  const backBtn = button({ label: 'Back', icon: 'arrow-left', variant: 'ghost' });
  const nextBtn = button({ label: 'Continue', iconRight: 'arrow-right', variant: 'primary', type: 'submit' });
  const form = h('form', { class: 'auth-form', novalidate: true }, ...panels, h('div', { class: 'row between' }, backBtn, nextBtn));

  function render() {
    fill(stepper, ...STEPS.flatMap((s, i) => [
      i > 0 ? h('span', { class: 'step-line', aria: { hidden: 'true' } }) : null,
      h('span', { class: cls('step', { 'is-active': i === step, 'is-done': i < step }), aria: { current: i === step ? 'step' : undefined } },
        h('span', { class: 'step-num' }, i < step ? icon('check', 12) : String(i + 1)), s.label),
    ]));
    panels.forEach((p, i) => { p.hidden = i !== step; });
    backBtn.hidden = step === 0;
    nextBtn.replaceChildren(h('span', { class: 'btn-label' }, step === STEPS.length - 1 ? 'Finish setup' : 'Continue'), icon(step === STEPS.length - 1 ? 'check' : 'arrow-right', 14));
    if (step === 2) {
      summary.replaceChildren(
        h('dt', {}, 'Administrator'), h('dd', {}, username.value.trim()),
        h('dt', {}, 'Email'), h('dd', {}, email.value.trim()));
    }
    requestAnimationFrame(() => panels[step].querySelector('input')?.focus());
  }

  function validate(i) {
    const p = {};
    if (i === 0 && !token.value.trim()) p.setup_token = 'Enter the setup token from the server log.';
    if (i === 1) {
      if (!username.value.trim()) p.username = 'Choose an administrator username.';
      if (!email.value.trim() || !email.checkValidity()) p.email = 'Enter a valid email address.';
      if (!pw.inputEl.value) p.password = 'Choose a password.';
      else if (pw.inputEl.value.length < 12) p.password = 'Use at least 12 characters.';
      else if (pw.inputEl.value !== confirm.inputEl.value) p.confirm = 'Passwords don’t match.';
    }
    if (i === 2 && !serverName.value.trim()) p.server_name = 'Give your server a name.';
    return p;
  }

  backBtn.addEventListener('click', () => {
    clearErrors(form);
    step = Math.max(0, step - 1);
    render();
  });

  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    clearErrors(form);
    const problems = validate(step);
    if (Object.keys(problems).length) {
      showFieldErrors(form, problems);
      return;
    }
    if (step < STEPS.length - 1) {
      step += 1;
      render();
      return;
    }
    setLoading(nextBtn, true);
    try {
      const res = await ctx.api.post('/setup', {
        setup_token: token.value.trim(),
        username: username.value.trim(),
        email: email.value.trim(),
        password: pw.inputEl.value,
        server_name: serverName.value.trim(),
      }, { noRedirect: true });
      store.set({ status: { ...(store.get().status || {}), setup_required: false, server_name: serverName.value.trim() } });
      await ctx.completeLogin(res, '/');
      ctx.ui.toast.success('Your server is ready', { description: 'Next: invite users and configure email in Admin.' });
    } catch (err) {
      if (err.status === 409) {
        store.set({ status: { ...(store.get().status || {}), setup_required: false } });
        ctx.ui.toast.info('This server is already set up. Sign in to continue.');
        await ctx.router.navigate('/login', { replace: true });
        return;
      }
      if (err.status === 403) {
        step = 0;
        render();
        showFieldErrors(form, { setup_token: 'That setup token isn’t valid. Copy it again from the server log or setup-token.txt.' });
        return;
      }
      if (err.fields) {
        const steps = Object.keys(err.fields).map((k) => FIELD_STEP[k]).filter((x) => x !== undefined);
        if (steps.length) {
          step = Math.min(...steps);
          render();
        }
        showFieldErrors(form, err.fields);
        return;
      }
      setBanner(form, authErrorText(err));
    } finally {
      if (nextBtn.isConnected) setLoading(nextBtn, false);
    }
  });

  container.replaceChildren(authLayout({
    title: 'Welcome to Ludworks',
    subtitle: 'Let’s secure your server and create the first administrator.',
    body: h('div', {}, stepper, form),
    wide: true,
    settings: ctx.settings,
  }));
  render();
}
