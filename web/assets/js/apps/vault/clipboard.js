// Clipboard with best-effort auto-clear: 30 s after copying a secret the clipboard is emptied,
// provided the page still has focus (browsers only allow clipboard writes from a focused
// document, and clearing later could wipe something the user copied elsewhere meanwhile).

export const CLEAR_AFTER_MS = 30_000;

/**
 * @param {{write?: (text: string) => Promise<void>, fallback?: (text: string) => boolean,
 *          hasFocus?: () => boolean, setTimer?: typeof setTimeout, clearTimer?: typeof clearTimeout,
 *          delayMs?: number, onCleared?: () => void}} [deps]
 */
export function createClipboard(deps = {}) {
  const write = deps.write ?? ((t) => globalThis.navigator.clipboard.writeText(t));
  const fallback = deps.fallback ?? execCommandCopy;
  const hasFocus = deps.hasFocus ?? (() => globalThis.document?.hasFocus?.() ?? false);
  const setTimer = deps.setTimer ?? ((fn, ms) => setTimeout(fn, ms));
  const clearTimer = deps.clearTimer ?? ((t) => clearTimeout(t));
  const delayMs = deps.delayMs ?? CLEAR_AFTER_MS;
  let timer = null;

  async function put(text) {
    try {
      await write(text);
      return true;
    } catch {
      try {
        return !!fallback(text);
      } catch {
        return false;
      }
    }
  }

  async function clearNow() {
    if (timer === null) return false;
    clearTimer(timer);
    timer = null;
    if (!hasFocus()) return false;
    const ok = await put('');
    if (ok) deps.onCleared?.();
    return ok;
  }

  return {
    /** Copy text; when secret, schedule the auto-clear. Resolves true on success. */
    async copy(text, { secret = true } = {}) {
      const ok = await put(String(text ?? ''));
      if (timer !== null) {
        clearTimer(timer);
        timer = null;
      }
      if (ok && secret && text) {
        timer = setTimer(() => {
          timer = -1; // keep "pending" for clearNow
          clearNow();
        }, delayMs);
      }
      return ok;
    },
    /** Clear immediately if a secret is pending (e.g. on lock). */
    clearNow,
    get pending() {
      return timer !== null;
    },
    dispose() {
      if (timer !== null && timer !== -1) clearTimer(timer);
      timer = null;
    },
  };
}

function execCommandCopy(text) {
  const doc = globalThis.document;
  if (!doc) return false;
  const ta = doc.createElement('textarea');
  ta.className = 'clipboard-proxy';
  ta.setAttribute('readonly', '');
  ta.setAttribute('aria-hidden', 'true');
  ta.value = text;
  doc.body.appendChild(ta);
  ta.select();
  let ok = false;
  try {
    ok = doc.execCommand('copy');
  } finally {
    ta.remove();
  }
  return ok;
}
