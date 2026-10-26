// Passwords app: an end-to-end encrypted vault (VAULT-CRYPTO.md). Screens: first-time setup,
// unlock, and the unlocked vault (list + detail, generator, security report, import, export,
// settings). The vault key lives only in module memory (state.js / session.js); unmounting
// drops every decrypted record, and the key itself is dropped by lock() — on demand, after the
// idle timeout, optionally when the tab is hidden, on sign-out or account switch.

import { h } from '../../core/dom.js';
import * as ui from '../../core/ui.js';
import { ensureStyle } from '../../core/style.js';
import { vaultFor } from './state.js';
import { renderSetup } from './views/setup.js';
import { renderUnlock } from './views/unlock.js';
import { renderMain } from './views/main.js';
import { LockedError } from './session.js';

export async function mount(container, ctx0) {
  let ctx = ctx0;
  await ensureStyle('/assets/css/apps/vault.css');
  ctx.setLayout({ full: true });
  const vault = vaultFor(ctx);
  const { session } = vault;
  const root = h('div', { class: 'vault' });
  container.append(root);

  let screen = null; // {kind, destroy?, update?}
  let disposed = false;

  function clearScreen() {
    try {
      screen?.destroy?.();
    } catch (err) {
      console.error(err);
    }
    screen = null;
  }

  function show(kind, build) {
    clearScreen();
    root.replaceChildren();
    const r = build(root);
    screen = { kind, ...(typeof r === 'function' ? { destroy: r } : r || {}) };
  }

  async function render() {
    if (disposed) return;
    const s = session.status;
    if (s === 'uninitialized') {
      if (screen?.kind === 'setup') return;
      ctx.setSearch(null);
      show('setup', (host) => renderSetup(host, { vault, ctx }));
    } else if (s === 'locked') {
      if (screen?.kind === 'unlock') return;
      ctx.setSearch(null);
      show('unlock', (host) => renderUnlock(host, { vault, ctx }));
      ui.closeMenu();
    } else if (s === 'unlocked') {
      if (screen?.kind === 'main') return;
      if (!session.synced) {
        ctx.setTitle('Passwords');
        show('loading', (host) => host.append(ui.loadingBlock('Decrypting your vault…')));
        try {
          await session.sync();
        } catch (err) {
          if (err instanceof LockedError || disposed) return;
          show('error', (host) => host.append(h('div', { class: 'page' }, ui.errorState(err, () => { clearScreen(); render(); }))));
          return;
        }
        if (disposed || session.status !== 'unlocked') return;
      }
      show('main', (host) => renderMain(host, { vault, ctx }));
    }
  }

  const off = session.subscribe((what) => {
    if (what === 'status') render();
  });

  root.append(ui.loadingBlock('Opening your vault…'));
  try {
    await session.load();
  } catch (err) {
    root.replaceChildren(h('div', { class: 'page' }, ui.errorState(err, () => location.reload())));
    return () => off();
  }
  await render();

  // Background refresh when the tab becomes visible again (another device may have changed things):
  // re-check the key first (load() locks when the vault was reset, its key rotated or the master
  // password changed elsewhere), then pull the changes.
  const onVisible = () => {
    if (document.visibilityState !== 'visible' || !session.unlocked || !session.synced) return;
    session.load()
      .then(() => (session.unlocked ? session.sync() : null))
      .catch(() => {});
  };
  document.addEventListener('visibilitychange', onVisible);

  return {
    update(c) {
      ctx = c;
      if (screen?.kind === 'main') screen.update?.(c);
    },
    unmount() {
      disposed = true;
      off();
      document.removeEventListener('visibilitychange', onVisible);
      clearScreen();
      // Leaving the app: drop decrypted records (the key stays only until the idle lock).
      session.dropPlaintext();
      vault.clipboard.clearNow();
    },
  };
}
