// Cross-tab authentication signals. The session cookie is shared by every tab of this origin,
// but each tab keeps the signed-in user (and whatever it loaded or decrypted, such as an
// unlocked vault) in its own memory. When one tab signs out, finds its session ended, or signs
// in to another account, the other tabs must drop that state at once rather than keep showing
// the previous user's data until one of their own API calls happens to fail.
//
// Signals go over a BroadcastChannel and, as a fallback for browsers or contexts without one,
// a localStorage 'storage' event (fired in the other tabs only). Every message carries a random
// nonce, so a tab that receives it both ways handles it once. Only same-origin script can post
// on either path. No DOM access at import time: the module is unit tested under node.

export const AUTH_CHANNEL = 'lw-auth';
export const AUTH_STORAGE_KEY = 'lw.auth.signal';
const TYPES = new Set(['signout', 'session']);

function randomId() {
  const bytes = new Uint8Array(12);
  if (globalThis.crypto?.getRandomValues) globalThis.crypto.getRandomValues(bytes);
  else for (let i = 0; i < bytes.length; i++) bytes[i] = Math.floor(Math.random() * 256);
  return Array.from(bytes, (b) => b.toString(16).padStart(2, '0')).join('');
}

function defaultStorage() {
  try {
    return globalThis.localStorage || null;
  } catch {
    return null; // blocked (privacy settings)
  }
}

/**
 * Listen for, and send, auth signals shared between the tabs of this origin.
 *
 *   const sync = createAuthSync({onSignal: ({type, user}) => …});
 *   sync.post('signout');           // this tab signed out (or its session ended)
 *   sync.post('session', user.id);  // this tab signed in as user.id
 *
 * @param {{onSignal: (signal: {type: 'signout'|'session', user: string}) => void,
 *          BroadcastChannel?: Function|null, storage?: Storage|null, target?: EventTarget|null}} opts
 *   BroadcastChannel/storage/target default to the browser's (window for 'storage' events);
 *   pass null to disable one path.
 * @returns {{post: (type: 'signout'|'session', user?: string) => void, close: () => void}}
 */
export function createAuthSync({
  onSignal,
  BroadcastChannel: BC = globalThis.BroadcastChannel,
  storage = defaultStorage(),
  target = globalThis.window ?? null,
} = {}) {
  const tabId = randomId();
  const seen = new Set(); // recent nonces (BroadcastChannel and storage deliver the same message)

  const handle = (msg) => {
    if (!msg || typeof msg !== 'object' || msg.v !== 1 || !TYPES.has(msg.type)) return;
    if (msg.from === tabId || typeof msg.nonce !== 'string' || !msg.nonce || seen.has(msg.nonce)) return;
    seen.add(msg.nonce);
    if (seen.size > 32) seen.delete(seen.values().next().value);
    try {
      onSignal?.({ type: msg.type, user: typeof msg.user === 'string' ? msg.user : '' });
    } catch (err) {
      console.error('auth signal handler failed', err);
    }
  };

  let channel = null;
  try {
    if (typeof BC === 'function') {
      channel = new BC(AUTH_CHANNEL);
      channel.onmessage = (e) => handle(e?.data);
    }
  } catch {
    channel = null;
  }

  const onStorage = (e) => {
    if (e?.key !== AUTH_STORAGE_KEY || !e.newValue) return; // our own removeItem() fires too
    try {
      handle(JSON.parse(e.newValue));
    } catch {
      /* not ours */
    }
  };
  target?.addEventListener?.('storage', onStorage);

  return {
    post(type, user = '') {
      if (!TYPES.has(type)) throw new Error(`unknown auth signal: ${type}`);
      const msg = { v: 1, type, user: String(user || ''), from: tabId, nonce: randomId() };
      try {
        channel?.postMessage(msg);
      } catch {
        /* closed or unavailable: the storage path still runs */
      }
      try {
        // Setting the key notifies the other tabs; removing it right away leaves nothing behind.
        storage?.setItem(AUTH_STORAGE_KEY, JSON.stringify(msg));
        storage?.removeItem(AUTH_STORAGE_KEY);
      } catch {
        /* quota/blocked */
      }
    },
    close() {
      try {
        channel?.close();
      } catch {
        /* ignore */
      }
      channel = null;
      target?.removeEventListener?.('storage', onStorage);
    },
  };
}
