// The play queue (pure, no DOM): tracks in play order, the current position, shuffle and repeat.
//
// `entries` is the play order — what the queue view shows. Each entry wraps a track with a
// queue-unique `uid` (the same track can be queued twice) and the play context it came from.
// Shuffle physically reorders the entries after putting the current one first and remembers the
// original order, so turning shuffle off restores it (items added meanwhile keep their place
// relative to the current track). Repeat "one" only affects automatic advancing; "all" wraps.
// Unit tested in web/tests/music/queue.test.mjs.

/** Repeat modes, in the order the repeat button cycles through them. */
export const REPEAT_MODES = Object.freeze(['off', 'all', 'one']);

/** Most entries stored in localStorage and sent to a cast device. */
export const MAX_STORED = 1000;

let uidSeq = 0;
function newUid() {
  uidSeq += 1;
  return `q${uidSeq.toString(36)}`;
}

/** Fisher–Yates shuffle (in place) with an injectable random source. */
export function shuffleInPlace(list, rng = Math.random) {
  for (let i = list.length - 1; i > 0; i--) {
    const j = Math.floor(rng() * (i + 1));
    [list[i], list[j]] = [list[j], list[i]];
  }
  return list;
}

/** Next repeat mode (off → all → one → off). */
export function nextRepeat(mode) {
  const i = REPEAT_MODES.indexOf(mode);
  return REPEAT_MODES[(i + 1) % REPEAT_MODES.length];
}

/**
 * Where items land when moved: the list without them, then inserted at `position` (0-based,
 * clamped) keeping their relative order — the same rule as playlist items/move (API.md §12).
 * @template T
 * @param {T[]} list
 * @param {(item: T) => boolean} isMoved
 * @param {number} position
 * @returns {T[]}
 */
export function moveWithin(list, isMoved, position) {
  const moved = list.filter(isMoved);
  if (!moved.length) return list.slice();
  const rest = list.filter((x) => !isMoved(x));
  const p = Math.max(0, Math.min(rest.length, Math.trunc(Number(position) || 0)));
  return [...rest.slice(0, p), ...moved, ...rest.slice(p)];
}

/**
 * Convert a drop between rows into a move position: an item dragged from index `from` and
 * dropped before row `insertion` (0..length, in the list that still contains it) lands at
 * this index of the list without it.
 */
export function insertionToPosition(from, insertion) {
  return insertion > from ? insertion - 1 : insertion;
}

export class PlayQueue {
  /** @param {{rng?: () => number}} [opts] */
  constructor({ rng = Math.random } = {}) {
    this.rng = rng;
    /** @type {{uid: string, id: string, track: object, context: object|null}[]} */
    this.entries = [];
    this.index = -1;
    this.shuffle = false;
    /** @type {'off'|'all'|'one'} */
    this.repeat = 'off';
    /** uids in the order before shuffling (null while not shuffled) */
    this.original = null;
    /** bumped on every change (views compare it to skip work) */
    this.version = 0;
  }

  /** A queue entry for a track. */
  static entry(track, context = null) {
    return { uid: newUid(), id: track.id, track, context: context || null };
  }

  get length() { return this.entries.length; }
  get isEmpty() { return this.entries.length === 0; }
  /** The current entry or null. */
  get current() { return this.index >= 0 ? this.entries[this.index] || null : null; }
  /** Entries after the current one. */
  upcoming() { return this.entries.slice(this.index + 1); }
  /** Entries before the current one. */
  history() { return this.index > 0 ? this.entries.slice(0, this.index) : []; }
  /** Index of an entry by uid (-1 when absent). */
  indexOf(uid) { return this.entries.findIndex((e) => e.uid === uid); }

  _changed() {
    this.version += 1;
  }

  /**
   * Replace the queue. With shuffle (or when shuffle is already on) the chosen track plays
   * first and the rest follow in random order; `index` null with shuffle picks a random start.
   * @param {object[]} tracks
   * @param {{index?: number|null, context?: object|null, shuffle?: boolean}} [opts]
   */
  set(tracks, { index = 0, context = null, shuffle } = {}) {
    const list = (tracks || []).filter((t) => t && typeof t.id === 'string');
    this.entries = list.map((t) => PlayQueue.entry(t, context));
    if (shuffle !== undefined) this.shuffle = !!shuffle;
    this.original = null;
    if (!this.entries.length) {
      this.index = -1;
    } else if (index === null || index === undefined) {
      this.index = this.shuffle ? Math.floor(this.rng() * this.entries.length) : 0;
    } else {
      this.index = Math.max(0, Math.min(this.entries.length - 1, Math.trunc(index)));
    }
    if (this.shuffle && this.entries.length) this._shuffleRest();
    this._changed();
    return this.current;
  }

  /** Shuffle with the current entry first; remember the order to restore. */
  _shuffleRest() {
    this.original = this.entries.map((e) => e.uid);
    const cur = this.current;
    const rest = this.entries.filter((e) => e !== cur);
    shuffleInPlace(rest, this.rng);
    this.entries = cur ? [cur, ...rest] : rest;
    this.index = cur ? 0 : (this.entries.length ? 0 : -1);
  }

  /** Turn shuffle on or off. */
  setShuffle(on) {
    const want = !!on;
    if (want === this.shuffle) return false;
    this.shuffle = want;
    if (want) {
      if (this.entries.length) this._shuffleRest();
    } else if (this.original) {
      const byUid = new Map(this.entries.map((e) => [e.uid, e]));
      const known = new Set(this.original);
      const ordered = this.original.filter((u) => byUid.has(u)).map((u) => byUid.get(u));
      // Entries that were never in the original order (defensive): keep them after the current one.
      const extra = this.entries.filter((e) => !known.has(e.uid));
      const cur = this.current;
      let restored = ordered;
      if (extra.length) {
        const at = cur ? ordered.indexOf(cur) + 1 : ordered.length;
        restored = [...ordered.slice(0, at), ...extra, ...ordered.slice(at)];
      }
      this.entries = restored;
      this.index = cur ? this.entries.indexOf(cur) : (this.entries.length ? 0 : -1);
      this.original = null;
    }
    this._changed();
    return true;
  }

  /** Set the repeat mode. */
  setRepeat(mode) {
    if (!REPEAT_MODES.includes(mode) || mode === this.repeat) return false;
    this.repeat = mode;
    this._changed();
    return true;
  }

  /** Cycle the repeat mode; returns the new mode. */
  cycleRepeat() {
    this.setRepeat(nextRepeat(this.repeat));
    return this.repeat;
  }

  /**
   * Insert tracks right after the current one ("play next").
   * @returns {object[]} the new entries
   */
  insertNext(tracks, context = null) {
    const added = (tracks || []).filter((t) => t && typeof t.id === 'string').map((t) => PlayQueue.entry(t, context));
    if (!added.length) return [];
    const at = this.index + 1;
    this.entries.splice(at, 0, ...added);
    if (this.original) {
      const cur = this.current;
      const o = cur ? this.original.indexOf(cur.uid) + 1 : 0;
      this.original.splice(o, 0, ...added.map((e) => e.uid));
    }
    if (this.index < 0) this.index = 0;
    this._changed();
    return added;
  }

  /**
   * Append tracks to the end ("add to queue").
   * @returns {object[]} the new entries
   */
  append(tracks, context = null) {
    const added = (tracks || []).filter((t) => t && typeof t.id === 'string').map((t) => PlayQueue.entry(t, context));
    if (!added.length) return [];
    this.entries.push(...added);
    if (this.original) this.original.push(...added.map((e) => e.uid));
    if (this.index < 0) this.index = 0;
    this._changed();
    return added;
  }

  /**
   * Remove entries by uid. When the current entry goes, the next one (or the previous one at
   * the end) becomes current.
   * @param {Iterable<string>} uids
   * @returns {{removed: number, currentRemoved: boolean}}
   */
  remove(uids) {
    const set = new Set(uids);
    const cur = this.current;
    const before = this.entries.slice(0, Math.max(0, this.index)).filter((e) => set.has(e.uid)).length;
    const currentRemoved = !!cur && set.has(cur.uid);
    const kept = this.entries.filter((e) => !set.has(e.uid));
    const removed = this.entries.length - kept.length;
    if (!removed) return { removed: 0, currentRemoved: false };
    this.entries = kept;
    if (this.original) this.original = this.original.filter((u) => !set.has(u));
    if (!kept.length) this.index = -1;
    else if (currentRemoved) this.index = Math.min(this.index - before, kept.length - 1);
    else this.index = kept.indexOf(cur);
    this._changed();
    return { removed, currentRemoved };
  }

  /**
   * Move entries (by uid, keeping their relative order) so the first lands at `position` of
   * the queue without them.
   */
  move(uids, position) {
    const set = new Set(uids);
    const cur = this.current;
    const next = moveWithin(this.entries, (e) => set.has(e.uid), position);
    if (next.every((e, i) => e === this.entries[i])) return false;
    this.entries = next;
    this.index = cur ? next.indexOf(cur) : this.index;
    this._changed();
    return true;
  }

  /** Remove everything. */
  clear() {
    if (!this.entries.length && this.index === -1) return false;
    this.entries = [];
    this.index = -1;
    this.original = null;
    this._changed();
    return true;
  }

  /** Remove the entries after the current one. */
  clearUpcoming() {
    const up = this.upcoming();
    if (!up.length) return false;
    return this.remove(up.map((e) => e.uid)).removed > 0;
  }

  /** Make entry i current. */
  jump(i) {
    if (!Number.isInteger(i) || i < 0 || i >= this.entries.length) return null;
    if (i !== this.index) {
      this.index = i;
      this._changed();
    }
    return this.current;
  }

  /**
   * The entry that plays after the current one, without moving. `auto` (the track ended)
   * honours repeat "one".
   */
  peekNext({ auto = false } = {}) {
    if (!this.entries.length) return null;
    if (auto && this.repeat === 'one') return this.current;
    if (this.index + 1 < this.entries.length) return this.entries[this.index + 1];
    if (this.repeat === 'all') return this.entries[0];
    return null;
  }

  /** Advance; returns the new current entry or null at the end of the queue (index unchanged). */
  next({ auto = false } = {}) {
    const n = this.peekNext({ auto });
    if (!n) return null;
    const i = this.entries.indexOf(n);
    if (i !== this.index) {
      this.index = i;
      this._changed();
    }
    return n;
  }

  /** The entry before the current one (wrapping with repeat all), or null at the start. */
  peekPrevious() {
    if (!this.entries.length) return null;
    if (this.index > 0) return this.entries[this.index - 1];
    if (this.repeat === 'all') return this.entries[this.entries.length - 1];
    return null;
  }

  /** Step back; returns the new current entry or null at the start (index unchanged). */
  previous() {
    const p = this.peekPrevious();
    if (!p) return null;
    const i = this.entries.indexOf(p);
    if (i !== this.index) {
      this.index = i;
      this._changed();
    }
    return p;
  }

  /** Replace the track object of every entry with this id (favorite/tag changes). */
  updateTrack(track) {
    let hit = false;
    for (const e of this.entries) {
      if (e.id === track.id) {
        e.track = track;
        hit = true;
      }
    }
    if (hit) this._changed();
    return hit;
  }

  /**
   * At most `max` consecutive entries around the current one (casting sends ≤ 1000 items).
   * @returns {{start: number, entries: object[]}}
   */
  window(max = MAX_STORED) {
    if (this.entries.length <= max) return { start: 0, entries: this.entries.slice() };
    const lead = Math.min(50, Math.floor(max / 10));
    const start = Math.max(0, Math.min(this.index - lead, this.entries.length - max));
    return { start, entries: this.entries.slice(start, start + max) };
  }

  /**
   * Serialisable state for localStorage: track ids only (plus play contexts), ≤ MAX_STORED
   * entries around the current one.
   */
  toJSON() {
    const { start, entries } = this.window(MAX_STORED);
    const ctxKeys = [];
    const ctxIndex = new Map();
    const ci = entries.map((e) => {
      if (!e.context) return -1;
      const key = `${e.context.kind}\u0000${e.context.id ?? ''}`;
      if (!ctxIndex.has(key)) {
        ctxIndex.set(key, ctxKeys.length);
        ctxKeys.push(e.context.id === undefined ? { kind: e.context.kind } : { kind: e.context.kind, id: e.context.id });
      }
      return ctxIndex.get(key);
    });
    const out = {
      v: 1,
      ids: entries.map((e) => e.id),
      index: this.index - start,
      shuffle: this.shuffle,
      repeat: this.repeat,
      ctx: ctxKeys,
      ci,
    };
    // The order to restore when shuffle is turned off, as positions in `ids`.
    if (this.original && start === 0 && entries.length === this.entries.length) {
      const pos = new Map(entries.map((e, i) => [e.uid, i]));
      out.orig = this.original.filter((u) => pos.has(u)).map((u) => pos.get(u));
    }
    return out;
  }

  /**
   * Rebuild a queue from toJSON() output. Ids without a track in `tracksById` are dropped (gone
   * or no longer readable); a missing current track moves to the next available one.
   * @param {any} data
   * @param {Map<string, object>} tracksById
   * @param {{rng?: () => number}} [opts]
   */
  static fromJSON(data, tracksById, opts = {}) {
    const q = new PlayQueue(opts);
    if (!data || data.v !== 1 || !Array.isArray(data.ids)) return q;
    const ctx = Array.isArray(data.ctx) ? data.ctx : [];
    const ci = Array.isArray(data.ci) ? data.ci : [];
    const kept = [];
    let index = -1;
    const want = Number.isInteger(data.index) ? data.index : 0;
    data.ids.forEach((id, i) => {
      const t = typeof id === 'string' ? tracksById.get(id) : null;
      if (!t) return;
      if (index < 0 && i >= want) index = kept.length;
      const c = ctx[ci[i]];
      const context = c && typeof c.kind === 'string' ? (c.id === undefined ? { kind: c.kind } : { kind: c.kind, id: String(c.id) }) : null;
      kept.push({ pos: i, entry: PlayQueue.entry(t, context) });
    });
    q.entries = kept.map((k) => k.entry);
    q.index = q.entries.length ? (index < 0 ? q.entries.length - 1 : index) : -1;
    q.repeat = REPEAT_MODES.includes(data.repeat) ? data.repeat : 'off';
    q.shuffle = !!data.shuffle;
    if (q.shuffle && Array.isArray(data.orig)) {
      const byPos = new Map(kept.map((k) => [k.pos, k.entry.uid]));
      const orig = data.orig.filter((p) => byPos.has(p)).map((p) => byPos.get(p));
      if (orig.length === q.entries.length) q.original = orig;
    }
    return q;
  }
}
