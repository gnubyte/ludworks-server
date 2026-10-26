// Email verification landing page (?token=...). Verifies once on load; the
// token is scrubbed from the URL first.

import { h } from '../core/dom.js';
import { button, spinner } from '../core/ui.js';
import { setUser } from '../core/store.js';
import { authLayout, scrubQuery } from './auth-common.js';

export async function mount(container, ctx) {
  ctx.setTitle('Verify email');
  const token = ctx.query.token || '';
  scrubQuery(['token']);
  const next = ctx.user ? { label: 'Continue to Ludworks', href: '/' } : { label: 'Sign in', href: '/login' };

  const show = (o) => container.replaceChildren(authLayout({ ...o, settings: ctx.settings }));
  if (!token) {
    show({
      title: 'Missing verification link', icon: 'alert', iconVariant: 'danger',
      subtitle: 'Open the link from your verification email again.',
      body: h('div', { class: 'auth-actions' }, button({ label: next.label, variant: 'primary', block: true, href: next.href })),
    });
    return;
  }
  show({
    title: 'Verifying your email…', icon: 'mail',
    body: h('div', { class: 'loading-block' }, spinner({ size: 'lg', label: 'Verifying' })),
  });
  try {
    await ctx.api.post('/auth/verify-email', { token }, { noRedirect: true, signal: ctx.signal });
    if (ctx.user) {
      try {
        setUser(await ctx.api.get('/me', { noRedirect: true }));
      } catch {
        /* not critical */
      }
    }
    show({
      title: 'Email verified', icon: 'check-circle', iconVariant: 'success',
      subtitle: 'Thanks — your email address is confirmed.',
      body: h('div', { class: 'auth-actions' }, button({ label: next.label, variant: 'primary', block: true, href: next.href })),
    });
  } catch (err) {
    if (err?.name === 'AbortError') return;
    show({
      title: 'Verification failed', icon: 'alert', iconVariant: 'danger',
      subtitle: err.status === 429 ? 'Too many attempts — try again shortly.' : 'This verification link is invalid or has expired. Sign in and request a new one from your profile.',
      body: h('div', { class: 'auth-actions' }, button({ label: next.label, variant: 'primary', block: true, href: next.href })),
    });
  }
}
