// Row/column geometry with sparse custom sizes (pure): O(log n) position ↔ index lookups for
// grids of up to a million rows without materialising every row.

export class Axis {
  /**
   * @param {number} count number of rows/columns
   * @param {number} defaultSize size in CSS pixels of entries without a custom size
   * @param {Map<number, number>|Record<number, number>} [sizes] custom sizes by index
   */
  constructor(count, defaultSize, sizes) {
    this.count = count;
    this.def = defaultSize;
    this.sizes = new Map(sizes instanceof Map ? sizes : Object.entries(sizes || {}).map(([k, v]) => [Number(k), v]));
    this.rebuild();
  }

  /** Recompute prefix sums after changing sizes or count. */
  rebuild() {
    this.idx = [...this.sizes.keys()].filter((i) => i >= 0 && i < this.count).sort((a, b) => a - b);
    this.cum = new Float64Array(this.idx.length + 1); // cum[k] = sum of deltas of idx[0..k-1]
    for (let k = 0; k < this.idx.length; k++) this.cum[k + 1] = this.cum[k] + (this.sizes.get(this.idx[k]) - this.def);
  }

  /** Size of entry i. */
  size(i) {
    const s = this.sizes.get(i);
    return s === undefined ? this.def : s;
  }

  /** Set (or with null reset) the size of entry i. */
  setSize(i, px) {
    if (px == null) this.sizes.delete(i);
    else this.sizes.set(i, px);
    this.rebuild();
  }

  setCount(n) {
    this.count = n;
    this.rebuild();
  }

  // number of custom entries with index < i
  lowerBound(i) {
    let lo = 0;
    let hi = this.idx.length;
    while (lo < hi) {
      const mid = (lo + hi) >> 1;
      if (this.idx[mid] < i) lo = mid + 1;
      else hi = mid;
    }
    return lo;
  }

  /** Start position (px) of entry i (i may equal count for the total). */
  pos(i) {
    return i * this.def + this.cum[this.lowerBound(i)];
  }

  /** Total size of all entries. */
  total() {
    return this.pos(this.count);
  }

  /** Index of the entry containing position px (clamped to [0, count-1]). */
  indexAt(px) {
    if (px <= 0) return 0;
    let lo = 0;
    let hi = this.count - 1;
    while (lo < hi) {
      const mid = Math.floor((lo + hi + 1) / 2);
      if (this.pos(mid) <= px) lo = mid;
      else hi = mid - 1;
    }
    return lo;
  }
}
