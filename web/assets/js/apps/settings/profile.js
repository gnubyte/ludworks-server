// Profile: display name, email, mail-in notes toggle.

import { h } from '../../core/dom.js';
import { field, input, switchInput, diffObject, showFormError, clearErrors } from '../../core/forms.js';
import { setUser, displayName } from '../../core/store.js';
import { formatDate } from '../../core/format.js';

export function render(host, ctx) {
  const { ui } = ctx;
  const user = ctx.user;
  const original = { display_name: user.display_name || '', email: user.email || '' };

  const nameInput = input({ name: 'display_name', value: original.display_name, autocomplete: 'name', maxlength: '100' });
  const emailInput = input({ name: 'email', type: 'email', value: original.email, autocomplete: 'email' });
  const save = ui.button({ label: 'Save changes', variant: 'primary', type: 'submit', disabled: true });
  const reset = ui.button({ label: 'Discard', variant: 'ghost', disabled: true });
  const avatarSlot = h('span');

  const verified = user.email_verified
    ? ui.badge('Verified', 'success', { icon: 'check' })
    : ui.badge('Not verified', 'warning', { icon: 'alert', title: 'Ask an administrator to resend verification if needed' });

  const form = h('form', { class: 'form-grid', novalidate: true },
    h('div', { class: 'row gap-3' }, avatarSlot,
      h('div', { class: 'cell-stack' },
        h('span', { class: 'cell-title' }, user.username),
        h('span', { class: 'cell-sub' }, `${user.role === 'admin' ? 'Administrator' : 'Member'} · joined ${formatDate(user.created_at)}`))),
    h('div', { class: 'form-grid cols-2' },
      field({ label: 'Display name', control: nameInput, name: 'display_name', hint: 'Shown across the app instead of your username.' }),
      field({ label: 'Email', control: emailInput, name: 'email', labelExtra: verified, hint: 'Used for password resets and mail-in notes.' })),
    h('div', { class: 'row end' }, reset, save));

  const renderAvatar = () => avatarSlot.replaceChildren(ui.avatar({ name: nameInput.value.trim() || user.username, size: 48 }));
  renderAvatar();

  const current = () => ({ display_name: nameInput.value.trim(), email: emailInput.value.trim() });
  const refresh = () => {
    const dirty = Object.keys(diffObject(original, current())).length > 0;
    save.disabled = !dirty;
    reset.disabled = !dirty;
    ctx.router.setLeaveGuard(dirty ? () => ui.confirm({ title: 'Discard unsaved changes?', confirmLabel: 'Discard', danger: true }) : null);
  };
  form.addEventListener('input', () => {
    renderAvatar();
    refresh();
  });
  reset.addEventListener('click', () => {
    nameInput.value = original.display_name;
    emailInput.value = original.email;
    clearErrors(form);
    renderAvatar();
    refresh();
  });
  form.addEventListener('submit', async (e) => {
    e.preventDefault();
    const patch = diffObject(original, current());
    if (!Object.keys(patch).length) return;
    if (patch.email !== undefined && !emailInput.checkValidity()) {
      showFormError(form, { fields: { email: 'Enter a valid email address.' } });
      return;
    }
    clearErrors(form);
    await ui.withLoading(save, async () => {
      try {
        const updated = await ctx.api.patch('/me', patch, { signal: ctx.signal });
        setUser(updated);
        Object.assign(original, { display_name: updated.display_name || '', email: updated.email || '' });
        refresh();
        ui.toast.success('Profile updated', patch.email && !updated.email_verified ? { description: 'Check your inbox to verify the new address.' } : undefined);
      } catch (err) {
        if (err?.name === 'AbortError') return;
        showFormError(form, err);
      }
    });
    refresh(); // withLoading restores the pre-click disabled state; recompute it
  });

  /* ---------------------------------------------------------------- mail-in */
  const mailSwitch = switchInput({ checked: !!user.mail_in_enabled, label: 'Mail-in notes', disabled: !user.email_verified });
  mailSwitch.inputEl.addEventListener('change', async () => {
    const want = mailSwitch.inputEl.checked;
    mailSwitch.inputEl.disabled = true;
    try {
      const updated = await ctx.api.patch('/me', { mail_in_enabled: want });
      setUser(updated);
      ui.toast.success(want ? 'Mail-in notes enabled' : 'Mail-in notes disabled');
    } catch (err) {
      mailSwitch.inputEl.checked = !want;
      ui.toast.error(err);
    } finally {
      mailSwitch.inputEl.disabled = !ctx.user?.email_verified;
    }
  });

  host.append(
    h('div', { class: 'settings-section-header' },
      h('h2', { class: 'settings-section-title' }, 'Profile'),
      h('p', { class: 'settings-section-desc' }, `Signed in as ${displayName(user)}.`)),
    ui.card({ title: 'Personal details', body: form }),
    ui.card({
      title: 'Mail-in notes', icon: 'mail',
      body: h('div', { class: 'setting-rows' },
        h('div', { class: 'setting-row' },
          h('div', { class: 'setting-row-text' },
            h('span', { class: 'setting-label' }, 'Turn emails into notes'),
            h('span', { class: 'setting-desc' }, user.email_verified
              ? 'Unread messages you send from your verified address to the server’s mail-in mailbox become notes. The subject becomes the title; attachments are saved to Home › Mail Attachments.'
              : 'Verify your email address first — only mail from a verified address is accepted.')),
          mailSwitch)),
    }),
  );
  return () => ctx.router.setLeaveGuard(null);
}
