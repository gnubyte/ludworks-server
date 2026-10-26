// Idle auto-lock: locks the vault after N minutes without user activity anywhere in the page
// (the key outlives the app's mount while the user works in other apps), and optionally as
// soon as the tab is hidden. Activity listeners are passive and only installed while unlocked.

const ACTIVITY = ['pointerdown', 'keydown', 'wheel', 'touchstart', 'pointermove'];

/**
 * @param {{lock: (why: 'idle'|'hidden') => void, minutes: () => number, lockOnHide: () => boolean, doc?: Document,
 *          now?: () => number, setInterval?: typeof setInterval, clearInterval?: typeof clearInterval,
 *          tickMs?: number}} o
 */
export function createAutoLock(o) {
  const doc = o.doc ?? globalThis.document;
  const now = o.now ?? (() => Date.now());
  const setIv = o.setInterval ?? ((fn, ms) => setInterval(fn, ms));
  const clearIv = o.clearInterval ?? ((t) => clearInterval(t));
  const tickMs = o.tickMs ?? 5000;
  let last = now();
  let timer = null;
  let lastMove = 0;

  const onActivity = (e) => {
    // pointermove fires constantly: sample it at most every 2 s.
    if (e?.type === 'pointermove') {
      const t = now();
      if (t - lastMove < 2000) return;
      lastMove = t;
    }
    last = now();
  };
  const onVisibility = () => {
    if (doc.visibilityState === 'hidden' && o.lockOnHide()) o.lock('hidden');
    else if (doc.visibilityState === 'visible') check();
  };
  function check() {
    const mins = o.minutes();
    if (mins > 0 && now() - last >= mins * 60_000) o.lock('idle');
  }

  return {
    /** Start watching (idempotent). */
    start() {
      last = now();
      if (timer !== null) return;
      for (const t of ACTIVITY) doc.addEventListener(t, onActivity, { capture: true, passive: true });
      doc.addEventListener('visibilitychange', onVisibility);
      timer = setIv(check, tickMs);
    },
    stop() {
      if (timer === null) return;
      for (const t of ACTIVITY) doc.removeEventListener(t, onActivity, { capture: true });
      doc.removeEventListener('visibilitychange', onVisibility);
      clearIv(timer);
      timer = null;
    },
    /** Record activity explicitly (e.g. after unlocking). */
    touch() {
      last = now();
    },
    /** Run the idle check now (tests, visibility changes). */
    check,
    get running() {
      return timer !== null;
    },
    /** Milliseconds until the idle lock (Infinity when disabled). */
    remaining() {
      const mins = o.minutes();
      return mins > 0 ? Math.max(0, last + mins * 60_000 - now()) : Infinity;
    },
  };
}
