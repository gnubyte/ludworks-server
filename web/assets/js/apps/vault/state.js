// Per-user vault singleton kept in module memory across mounts of the app: the session (key +
// decrypted records), the idle auto-lock, the clipboard auto-clear, preferences and transient
// UI state. Signing out or switching accounts locks and discards it.

import { store } from '../../core/store.js';
import { createSession } from './session.js';
import { createAutoLock } from './autolock.js';
import { createClipboard } from './clipboard.js';
import { loadPrefs, savePrefs } from './prefs.js';

let current = null;

function create(api, userId) {
  const session = createSession({ api });
  let prefs = loadPrefs(userId);
  const clipboard = createClipboard();
  const ui = { filter: { kind: 'all' }, selectedId: null, query: '', genHistory: [], lockReason: '' };
  const lock = (reason = '') => {
    clipboard.clearNow();
    ui.lockReason = reason;
    if (session.unlocked) session.lock();
  };
  const autolock = createAutoLock({
    lock: (why) => lock(why === 'hidden'
      ? 'Locked because the tab was hidden.'
      : `Locked after ${prefs.autoLockMinutes} minute${prefs.autoLockMinutes === 1 ? '' : 's'} of inactivity.`),
    minutes: () => prefs.autoLockMinutes,
    lockOnHide: () => prefs.lockOnHide,
  });
  const offStatus = session.subscribe((what) => {
    if (what !== 'status') return;
    if (session.unlocked) {
      ui.lockReason = '';
      autolock.start();
    } else {
      // The session locks itself when its key stops being the vault's current key.
      if (!ui.lockReason && session.lockReason) ui.lockReason = session.lockReason;
      autolock.stop();
      ui.selectedId = null;
      ui.query = '';
      ui.genHistory.length = 0;
    }
  });
  const offUser = store.select((s) => s.user?.id || '', (id) => {
    if (id !== userId) lock('');
  });
  return {
    userId,
    session,
    clipboard,
    autolock,
    ui,
    get prefs() {
      return prefs;
    },
    setPrefs(patch) {
      prefs = savePrefs(userId, { ...prefs, ...patch, gen: { ...prefs.gen, ...(patch.gen || {}) } });
      return prefs;
    },
    lock,
    dispose() {
      lock('');
      autolock.stop();
      offStatus();
      offUser();
      clipboard.dispose();
    },
  };
}

/**
 * The vault state for the signed-in user (created on first use).
 * @param {{api: object, user: {id: string}|null}} ctx
 */
export function vaultFor(ctx) {
  const uid = ctx.user?.id || '';
  if (current && current.userId !== uid) {
    current.dispose();
    current = null;
  }
  if (!current) current = create(ctx.api, uid);
  return current;
}
