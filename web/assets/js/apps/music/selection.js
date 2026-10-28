// Multi-selection of list rows (pure, no DOM): toggle, shift-click ranges from an anchor, select
// all, and pruning keys that left the list. Unit tested in web/tests/music/selection.test.mjs.

export class Selection {
  constructor() {
    /** @type {Set<string>} */
    this.keys = new Set();
    /** @type {string|null} last key toggled (start of shift ranges) */
    this.anchor = null;
  }

  get size() { return this.keys.size; }
  has(key) { return this.keys.has(key); }

  /** Toggle one key; it becomes the anchor. Returns the new state of the key. */
  toggle(key) {
    const on = !this.keys.has(key);
    if (on) this.keys.add(key);
    else this.keys.delete(key);
    this.anchor = key;
    return on;
  }

  /** Select or deselect one key (anchor moves there). */
  set(key, on) {
    if (on) this.keys.add(key);
    else this.keys.delete(key);
    this.anchor = key;
  }

  /**
   * Shift-click: select every key from the anchor to `key` (inclusive) in list order; without an
   * anchor just that key. The anchor stays, so further shift-clicks pivot around it.
   * @param {string[]} ordered keys in list order
   * @param {string} key
   */
  selectRange(ordered, key) {
    const b = ordered.indexOf(key);
    const a = this.anchor === null ? -1 : ordered.indexOf(this.anchor);
    if (b < 0) return;
    if (a < 0) {
      this.set(key, true);
      return;
    }
    const [lo, hi] = a <= b ? [a, b] : [b, a];
    for (let i = lo; i <= hi; i++) this.keys.add(ordered[i]);
  }

  /** Select all the given keys. */
  selectAll(ordered) {
    for (const k of ordered) this.keys.add(k);
  }

  clear() {
    this.keys.clear();
    this.anchor = null;
  }

  /** Drop keys that are no longer in the list. */
  retain(ordered) {
    const valid = new Set(ordered);
    for (const k of [...this.keys]) if (!valid.has(k)) this.keys.delete(k);
    if (this.anchor !== null && !valid.has(this.anchor)) this.anchor = null;
  }

  /** The selected keys in list order. */
  inOrder(ordered) {
    return ordered.filter((k) => this.keys.has(k));
  }
}
