// Autosave queue: cell edits are batched (debounced) into PATCH .../cells requests, other
// mutations run as ordered calls; one request at a time per workbook so the server sees edits
// in the order they were made. Tracks pending cells (so late responses never overwrite newer
// local edits), the workbook version (to notice edits made elsewhere) and retries network
// failures with backoff.

const DEBOUNCE_MS = 120;
const MAX_BATCH = 20000;

export class Saver {
  /**
   * @param {{api: any, base: string, version: number, onResult: (res: any, op: any) => void,
   *          onFail: (err: any, op: any) => void, onState: (state: string, detail?: any) => void,
   *          onExternalChange: () => void}} o
   */
  constructor(o) {
    this.o = o;
    this.queue = [];
    this.pending = new Map(); // "sid:key" → count of queued/in-flight edits
    this.version = o.version;
    this.busy = false;
    this.timer = 0;
    this.retryDelay = 0;
    this.closed = false;
  }

  /** Queue cell updates for a sheet; keys are the client cell keys (for pending tracking). */
  cells(sheetId, updates, keys) {
    if (!updates.length) return;
    let op = this.queue[this.queue.length - 1];
    if (!op || op.kind !== 'cells' || op.sheetId !== sheetId || op.started || op.updates.length + updates.length > MAX_BATCH || op.rangeStyle) {
      op = { kind: 'cells', sheetId, updates: [], keys: [] };
      this.queue.push(op);
    }
    op.updates.push(...updates);
    op.keys.push(...keys);
    for (const k of keys) this.bump(sheetId, k, 1);
    this.schedule(DEBOUNCE_MS);
  }

  /** Queue a range style (sent as its own PATCH in order). */
  rangeStyle(sheetId, ref, style, keys = []) {
    this.queue.push({ kind: 'cells', sheetId, updates: [], keys, rangeStyle: { ref, style } });
    for (const k of keys) this.bump(sheetId, k, 1);
    this.schedule(DEBOUNCE_MS);
  }

  /**
   * Queue an arbitrary mutation (runs after everything queued before it).
   * @param {() => Promise<any>} fn resolves with the response (may include version)
   * @returns {Promise<any>}
   */
  call(fn) {
    return new Promise((resolve, reject) => {
      this.queue.push({ kind: 'call', fn, resolve, reject });
      this.schedule(0);
    });
  }

  bump(sid, key, d) {
    const k = `${sid}:${key}`;
    const n = (this.pending.get(k) || 0) + d;
    if (n <= 0) this.pending.delete(k);
    else this.pending.set(k, n);
  }

  /** True while a newer local edit of this cell has not been confirmed. */
  isPending(sid, key) {
    return this.pending.has(`${sid}:${key}`);
  }

  get idle() {
    return !this.busy && this.queue.length === 0;
  }

  schedule(ms) {
    if (this.closed) return;
    clearTimeout(this.timer);
    this.timer = setTimeout(() => this.pump(), ms);
    this.o.onState('pending');
  }

  /** Send everything now (e.g. before leaving). */
  flush() {
    clearTimeout(this.timer);
    return this.pump();
  }

  /** Observe a server version; a jump of more than one means someone else changed the workbook. */
  seeVersion(v) {
    if (typeof v !== 'number' || !Number.isFinite(v)) return;
    if (v > this.version + 1) this.o.onExternalChange();
    if (v > this.version) this.version = v;
  }

  async pump() {
    if (this.busy || this.closed) return;
    const op = this.queue[0];
    if (!op) {
      this.o.onState('saved');
      return;
    }
    this.busy = true;
    op.started = true;
    this.o.onState('saving');
    try {
      let res;
      if (op.kind === 'cells') {
        const body = {};
        if (op.updates.length) body.updates = op.updates;
        if (op.rangeStyle) body.range_style = op.rangeStyle;
        res = await this.o.api.patch(`${this.o.base}/sheets/${op.sheetId}/cells`, body);
      } else {
        res = await op.fn();
      }
      this.queue.shift();
      this.retryDelay = 0;
      if (op.kind === 'cells') for (const k of op.keys) this.bump(op.sheetId, k, -1);
      if (res && typeof res.version === 'number') this.seeVersion(res.version);
      else if (op.kind === 'call') this.version += 1; // sheet-shaped responses carry no version
      try {
        this.o.onResult(res, op);
      } finally {
        op.resolve?.(res);
      }
    } catch (err) {
      const transient = err?.status === 0 || err?.status === 429 || err?.status === 503 || (err?.status >= 500 && err?.code !== 'internal');
      if (transient && !this.closed) {
        op.started = false;
        this.busy = false;
        this.retryDelay = Math.min(30000, this.retryDelay ? this.retryDelay * 2 : 1000);
        this.o.onState('offline', { retryIn: this.retryDelay });
        clearTimeout(this.timer);
        this.timer = setTimeout(() => this.pump(), err?.retryAfter ? err.retryAfter * 1000 : this.retryDelay);
        return;
      }
      this.queue.shift();
      if (op.kind === 'cells') for (const k of op.keys) this.bump(op.sheetId, k, -1);
      op.reject?.(err);
      this.o.onFail(err, op);
    }
    this.busy = false;
    if (this.queue.length) {
      const next = this.queue[0];
      this.schedule(next.kind === 'cells' && !next.rangeStyle ? 0 : 0);
    } else this.o.onState('saved');
  }

  close() {
    this.closed = true;
    clearTimeout(this.timer);
  }
}
