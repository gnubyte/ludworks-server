// State shared by the Music views and the player, which outlives them (the dock stays while the
// user moves between apps): an event bus and per-viewer preferences in localStorage.
//
// Events (emit/on):
//   'player'        the player was created or destroyed
//   'track'         the current queue entry changed
//   'queue'         the queue changed (order, items, shuffle/repeat)
//   'state'         play/pause/loading/error/volume/casting changed
//   'time'          position updates (several per second while playing)
//   'track-updated' a track's metadata changed (favorite, rating, tags) — payload: Track
//   'favorite'      an album or artist favorite changed — payload: {kind, id, favorite}
//   'playlists'     playlists changed (created, renamed, deleted, items added) — payload: {id?}

const listeners = new Map();

/** Subscribe; returns the unsubscribe function. */
export function on(type, fn) {
  if (!listeners.has(type)) listeners.set(type, new Set());
  listeners.get(type).add(fn);
  return () => listeners.get(type)?.delete(fn);
}

/** Notify subscribers (errors in one listener never stop the others). */
export function emit(type, payload) {
  for (const fn of [...(listeners.get(type) || [])]) {
    try {
      fn(payload);
    } catch (err) {
      console.error(err);
    }
  }
}

/* ----------------------------------------------------------- preferences */

const PREFIX = 'lw.music.';

function key(userId, name) {
  return `${PREFIX}${name}.${userId || 'anon'}`;
}

/** Read a JSON value from localStorage (null when missing, unreadable or storage is blocked). */
export function loadPref(userId, name) {
  try {
    const v = localStorage.getItem(key(userId, name));
    return v ? JSON.parse(v) : null;
  } catch {
    return null;
  }
}

/** Write a JSON value to localStorage (null removes it); storage failures are ignored. */
export function savePref(userId, name, value) {
  try {
    if (value === null || value === undefined) localStorage.removeItem(key(userId, name));
    else localStorage.setItem(key(userId, name), JSON.stringify(value));
  } catch { /* storage unavailable or full */ }
}
