// Resumable uploads: a tus 1.0.0 client for /api/v1/uploads/ (API.md §5.1) plus a
// concurrency-limited queue. Upload URLs are remembered per file fingerprint in
// localStorage so an interrupted upload resumes (HEAD → PATCH) after a reload.
// No DOM access at import time: the module is unit tested under node.

export const TUS_VERSION = '1.0.0';
export const DEFAULT_CHUNK_SIZE = 16 * 1024 * 1024;
export const DEFAULT_RETRY_DELAYS = [0, 1000, 3000, 5000, 10000, 20000, 30000];
const RETRYABLE_STATUS = new Set([0, 408, 423, 429, 460, 500, 502, 503, 504]);

/* ------------------------------------------------------------- encoding */

/** UTF-8 safe base64. */
export function toBase64Utf8(str) {
  const bytes = new TextEncoder().encode(String(str));
  let bin = '';
  for (let i = 0; i < bytes.length; i += 0x8000) bin += String.fromCharCode(...bytes.subarray(i, i + 0x8000));
  return btoa(bin);
}

/** Inverse of toBase64Utf8. */
export function fromBase64Utf8(b64) {
  const bin = atob(b64);
  return new TextDecoder().decode(Uint8Array.from(bin, (c) => c.charCodeAt(0)));
}

/**
 * Encode tus Upload-Metadata: "key b64(value),key2 b64(value2)". null/undefined are
 * skipped; an empty string is sent as the bare key (allowed by tus).
 * @param {Record<string, string|number|boolean|null|undefined>} meta
 */
export function encodeMetadata(meta) {
  const parts = [];
  for (const [k, v] of Object.entries(meta)) {
    if (v === undefined || v === null) continue;
    if (!/^[A-Za-z0-9_.-]+$/.test(k)) throw new Error(`invalid tus metadata key: ${k}`);
    const s = String(v);
    parts.push(s === '' ? k : `${k} ${toBase64Utf8(s)}`);
  }
  return parts.join(',');
}

/** Decode an Upload-Metadata header into an object. */
export function decodeMetadata(header) {
  const out = {};
  for (const part of String(header || '').split(',')) {
    const p = part.trim();
    if (!p) continue;
    const [k, v] = p.split(' ');
    out[k] = v ? fromBase64Utf8(v) : '';
  }
  return out;
}

/**
 * Fingerprint identifying "this file to this destination" (API.md: name + size +
 * mtime + space + path), scoped per user so accounts sharing a browser don't collide.
 * @param {{name: string, size: number, lastModified?: number}} file
 * @param {{spaceId: string, path?: string}} target
 * @param {string} [scope] e.g. user id
 */
export function uploadFingerprint(file, target, scope = '') {
  return ['lw-tus-v1', scope, target.spaceId, target.path || '', file.name, file.size, file.lastModified ?? 0]
    .map((x) => encodeURIComponent(String(x))).join(':');
}

/* ------------------------------------------------------ fingerprint store */

function memoryStorage() {
  const m = new Map();
  return {
    getItem: (k) => (m.has(k) ? m.get(k) : null),
    setItem: (k, v) => m.set(k, String(v)),
    removeItem: (k) => m.delete(k),
  };
}

function defaultStorage() {
  try {
    const ls = globalThis.localStorage;
    if (ls) {
      ls.getItem('lw.probe');
      return ls;
    }
  } catch {
    /* blocked (privacy mode) */
  }
  return memoryStorage();
}

/** Persistent map fingerprint → {url, createdAt, expiresAt, name, size}. */
export class FingerprintStore {
  /**
   * @param {{getItem: Function, setItem: Function, removeItem: Function}} [storage]
   * @param {string} [key]
   * @param {number} [maxEntries]
   */
  constructor(storage = defaultStorage(), key = 'lw.tus.uploads', maxEntries = 200) {
    this.storage = storage;
    this.key = key;
    this.maxEntries = maxEntries;
  }

  _read() {
    try {
      const raw = this.storage.getItem(this.key);
      const obj = raw ? JSON.parse(raw) : {};
      return obj && typeof obj === 'object' && !Array.isArray(obj) ? obj : {};
    } catch {
      return {};
    }
  }

  _write(map) {
    try {
      this.storage.setItem(this.key, JSON.stringify(map));
    } catch {
      /* quota/blocked: resume info is best effort */
    }
  }

  /** Entry for a fingerprint, or null (expired entries are dropped). */
  get(fp, now = Date.now()) {
    const map = this._read();
    const e = map[fp];
    if (!e || typeof e.url !== 'string') return null;
    if (e.expiresAt && Date.parse(e.expiresAt) <= now) {
      delete map[fp];
      this._write(map);
      return null;
    }
    return e;
  }

  set(fp, entry, now = Date.now()) {
    const map = this._read();
    map[fp] = entry;
    for (const [k, v] of Object.entries(map)) {
      if (v?.expiresAt && Date.parse(v.expiresAt) <= now) delete map[k];
    }
    const keys = Object.keys(map);
    if (keys.length > this.maxEntries) {
      keys.sort((a, b) => String(map[a].createdAt).localeCompare(String(map[b].createdAt)));
      for (const k of keys.slice(0, keys.length - this.maxEntries)) delete map[k];
    }
    this._write(map);
  }

  remove(fp) {
    const map = this._read();
    if (fp in map) {
      delete map[fp];
      this._write(map);
    }
  }

  /** Forget everything (e.g. on sign-out). */
  clear() {
    try {
      this.storage.removeItem(this.key);
    } catch {
      /* ignore */
    }
  }

  entries() {
    return this._read();
  }
}

/* ------------------------------------------------------------ transports */

function abortError() {
  return typeof DOMException !== 'undefined' ? new DOMException('Aborted', 'AbortError') : Object.assign(new Error('Aborted'), { name: 'AbortError' });
}

/**
 * Transport over fetch (no intra-chunk progress). Used under node/tests and as a
 * fallback. A transport is ({method, url, headers, body, signal, onProgress}) →
 * Promise<{status, getHeader(name), text}>; network failures reject.
 */
export function fetchTransport(fetchImpl = (...a) => globalThis.fetch(...a)) {
  return async ({ method, url, headers, body, signal, onProgress }) => {
    const res = await fetchImpl(url, { method, headers, body, signal, credentials: 'same-origin', cache: 'no-store' });
    let text = '';
    try {
      text = await res.text();
    } catch {
      text = '';
    }
    if (onProgress && body && typeof body.size === 'number' && res.status < 300) onProgress(body.size);
    return { status: res.status, getHeader: (n) => res.headers.get(n), text };
  };
}

/** Transport over XMLHttpRequest, reporting upload progress within each chunk. */
export function xhrTransport() {
  return ({ method, url, headers, body, signal, onProgress }) => new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(abortError());
      return;
    }
    const xhr = new XMLHttpRequest();
    xhr.open(method, url);
    for (const [k, v] of Object.entries(headers || {})) xhr.setRequestHeader(k, v);
    if (onProgress) xhr.upload.onprogress = (e) => onProgress(e.loaded);
    const onAbort = () => xhr.abort();
    signal?.addEventListener('abort', onAbort, { once: true });
    const done = () => signal?.removeEventListener('abort', onAbort);
    xhr.onload = () => {
      done();
      resolve({ status: xhr.status, getHeader: (n) => xhr.getResponseHeader(n), text: xhr.responseText });
    };
    xhr.onerror = () => {
      done();
      reject(new TypeError('Network error'));
    };
    xhr.ontimeout = xhr.onerror;
    xhr.onabort = () => {
      done();
      reject(abortError());
    };
    xhr.send(body ?? null);
  });
}

function defaultTransport() {
  return typeof XMLHttpRequest !== 'undefined' ? xhrTransport() : fetchTransport();
}

/* ------------------------------------------------------------------ errors */

/** Error raised by the tus client. */
export class UploadError extends Error {
  constructor(message, { status = 0, code = 'upload_failed', retryable = false, retryAfter = null } = {}) {
    super(message);
    this.name = 'UploadError';
    this.status = status;
    this.code = code;
    this.retryable = retryable;
    this.retryAfter = retryAfter;
  }
}

function httpError(res, what) {
  let code = 'upload_failed';
  let message = `${what} failed (HTTP ${res.status})`;
  try {
    const body = res.text ? JSON.parse(res.text) : null;
    if (body?.error?.message) message = body.error.message;
    if (body?.error?.code) code = body.error.code;
  } catch {
    /* not JSON */
  }
  if (res.status === 401) message = 'Your session has ended. Sign in again and add the file again to upload it.';
  if (res.status === 460) {
    code = 'checksum_mismatch';
    message = 'Chunk checksum mismatch';
  }
  const ra = Number(res.getHeader?.('Retry-After'));
  return new UploadError(message, {
    status: res.status, code, retryable: RETRYABLE_STATUS.has(res.status), retryAfter: Number.isFinite(ra) && ra > 0 ? ra : null,
  });
}

async function sha256Base64(blob) {
  const buf = await blob.arrayBuffer();
  const digest = new Uint8Array(await globalThis.crypto.subtle.digest('SHA-256', buf));
  let bin = '';
  for (const b of digest) bin += String.fromCharCode(b);
  return btoa(bin);
}

function checksumSupported() {
  return !!globalThis.crypto?.subtle && globalThis.isSecureContext !== false;
}

let seq = 0;

/* ------------------------------------------------------------------ upload */

/**
 * One resumable upload.
 *
 *   const up = new TusUpload(file, {spaceId, path: 'Movies', headers: () => ({'X-CSRF-Token': csrf})});
 *   up.on('progress', ({bytesUploaded, bytesTotal, speed, eta}) => …);
 *   const {fileId} = await up.start();   // or await up.done (survives pause/resume)
 *
 * States: queued → uploading ⇄ retrying → completed | error | cancelled; paused.
 * Events: 'state', 'progress', 'complete', 'error'.
 */
export class TusUpload {
  /**
   * @param {File|Blob & {name: string, lastModified?: number}} file
   * @param {{spaceId: string, path?: string, overwrite?: boolean, modTime?: string|Date, category?: string,
   *          subcategory?: string, tags?: string[]|string, mime?: string, endpoint?: string, chunkSize?: number,
   *          retryDelays?: number[], checksum?: boolean|'auto', headers?: () => Record<string,string>,
   *          transport?: Function, store?: FingerprintStore, scope?: string, activeScope?: () => string,
   *          now?: () => number, sleep?: (ms: number, signal: AbortSignal) => Promise<void>}} options
   *   scope: the user the upload belongs to. activeScope (optional) returns the user signed in
   *   now; while it differs from scope the upload sends nothing, so it can never be resumed or
   *   re-created under another account's session (headers() returns whatever CSRF is current).
   */
  constructor(file, options) {
    if (!options?.spaceId) throw new Error('TusUpload: spaceId is required');
    this.file = file;
    this.opts = {
      endpoint: '/api/v1/uploads/', chunkSize: DEFAULT_CHUNK_SIZE, retryDelays: DEFAULT_RETRY_DELAYS,
      checksum: 'auto', headers: () => ({}), scope: '', now: () => Date.now(), sleep: null, path: '',
      ...options,
    };
    this.transport = this.opts.transport || defaultTransport();
    this.store = this.opts.store || new FingerprintStore();
    this.id = `up${++seq}`;
    this.name = file.name;
    this.state = 'queued';
    this.bytesTotal = file.size;
    this.bytesUploaded = 0;
    this.speed = 0;
    this.eta = null;
    this.url = null;
    this.fileId = null;
    this.filePath = null;
    this.error = null;
    this._listeners = new Map();
    this._ctrl = null;
    this._running = null;
    this._paused = false;
    this._cancelled = false;
    this._attempt = 0;
    this._restarts = 0;
    this._lastSample = null;
    this._needsResync = false; // re-read the server offset before the next PATCH
    this._sentFinal = false;   // the last byte was acknowledged
    this.done = new Promise((resolve, reject) => {
      this._resolveDone = resolve;
      this._rejectDone = reject;
    });
    this.done.catch(() => {}); // callers may ignore it; avoid unhandled rejections
  }

  get fingerprint() {
    return uploadFingerprint(this.file, { spaceId: this.opts.spaceId, path: this.opts.path }, this.opts.scope);
  }

  /** Snapshot of progress. */
  get progress() {
    return {
      bytesUploaded: this.bytesUploaded, bytesTotal: this.bytesTotal,
      fraction: this.bytesTotal ? this.bytesUploaded / this.bytesTotal : (this.state === 'completed' ? 1 : 0),
      speed: this.speed, eta: this.eta,
    };
  }

  /** Subscribe to an event. @returns {() => void} */
  on(type, fn) {
    if (!this._listeners.has(type)) this._listeners.set(type, new Set());
    this._listeners.get(type).add(fn);
    return () => this._listeners.get(type)?.delete(fn);
  }

  _emit(type, detail) {
    for (const fn of [...(this._listeners.get(type) || [])]) {
      try {
        fn(detail, this);
      } catch (err) {
        console.error('upload listener failed', err);
      }
    }
  }

  _setState(s) {
    if (this.state === s) return;
    this.state = s;
    this._emit('state', s);
  }

  /**
   * Start or continue the upload. Resolves with {fileId, filePath} on completion,
   * or null if paused/cancelled first; rejects on a fatal error.
   */
  start() {
    if (this.state === 'completed') return Promise.resolve({ fileId: this.fileId, filePath: this.filePath });
    if (this._cancelled) return Promise.resolve(null);
    if (this._running) return this._running;
    this._paused = false;
    this.error = null;
    this._lastSample = null;
    this._attempt = 0;
    this._restarts = 0;
    this._running = this._run().finally(() => {
      this._running = null;
    });
    return this._running;
  }

  /** Pause: aborts the in-flight request; resume() continues from the server offset. */
  pause() {
    if (['completed', 'cancelled', 'error'].includes(this.state)) return;
    this._paused = true;
    this._ctrl?.abort();
    this.speed = 0;
    this.eta = null;
    this._setState('paused');
  }

  /** Resume a paused or failed upload. */
  resume() {
    if (this.state !== 'paused' && this.state !== 'error' && this.state !== 'queued') return this._running || Promise.resolve(null);
    return this.start();
  }

  /** Cancel and terminate the upload on the server (tus termination). */
  async cancel() {
    const url = this._stop();
    if (url) {
      try {
        await this.transport({ method: 'DELETE', url, headers: this._headers() });
      } catch {
        /* another session (no request is sent) or the network: the server purges expired uploads anyway */
      }
    }
  }

  /**
   * Cancel locally without contacting the server, for when the session that owns the upload is
   * already gone (sign-out elsewhere, expiry): forget its resume URL and fingerprint.
   */
  discard() {
    this._stop();
  }

  /** Common part of cancel/discard; returns the tus URL to terminate, if any. */
  _stop() {
    if (this.state === 'completed' || this._cancelled) return null;
    this._cancelled = true;
    this._ctrl?.abort();
    this.speed = 0;
    this.eta = null;
    this._setState('cancelled');
    this.store.remove(this.fingerprint);
    const url = this.url;
    this.url = null;
    this._rejectDone(new UploadError('Upload cancelled', { code: 'cancelled' }));
    return url;
  }

  _headers(extra = {}) {
    if (this.opts.activeScope && this.opts.activeScope() !== this.opts.scope) {
      throw new UploadError('This upload belongs to a session that has ended. Add the file again to upload it.', { code: 'session_changed' });
    }
    return { ...this.opts.headers(), 'Tus-Resumable': TUS_VERSION, ...extra };
  }

  async _request(method, url, headers, body, onProgress) {
    const hdrs = this._headers(headers); // session_changed is final: never retried, nothing sent
    this._ctrl = new AbortController();
    try {
      return await this.transport({ method, url, headers: hdrs, body, signal: this._ctrl.signal, onProgress });
    } catch (err) {
      if (err?.name === 'AbortError') throw err;
      throw new UploadError('Network error', { status: 0, code: 'network_error', retryable: true });
    }
  }

  _sleep(ms) {
    if (ms <= 0) return Promise.resolve();
    this._ctrl = new AbortController();
    const signal = this._ctrl.signal;
    if (this.opts.sleep) return this.opts.sleep(ms, signal);
    return new Promise((resolve, reject) => {
      const t = setTimeout(resolve, ms);
      signal.addEventListener('abort', () => {
        clearTimeout(t);
        reject(abortError());
      }, { once: true });
    });
  }

  _readCompletion(res) {
    const id = res.getHeader('Ludworks-File-Id');
    if (id) {
      this.fileId = id;
      this.filePath = res.getHeader('Ludworks-File-Path') || null;
    }
  }

  _resolveUrl(location) {
    const base = globalThis.location?.origin || 'http://localhost';
    const u = new URL(location, new URL(this.opts.endpoint, base));
    // Never follow a Location to another origin (it would receive our CSRF header).
    if (globalThis.location?.origin && u.origin !== globalThis.location.origin) {
      throw new UploadError('Upload server returned a foreign Location', { code: 'bad_location' });
    }
    return globalThis.location?.origin ? u.pathname + u.search : (/^https?:/i.test(location) ? location : u.pathname + u.search);
  }

  /** HEAD → server offset, or null when the upload is gone/unusable. */
  async _head(url) {
    const res = await this._request('HEAD', url, {});
    if (res.status === 404 || res.status === 410 || res.status === 403) return null;
    if (res.status < 200 || res.status >= 300) throw httpError(res, 'Resume check');
    const off = Number(res.getHeader('Upload-Offset'));
    const len = res.getHeader('Upload-Length');
    if (!Number.isFinite(off) || off < 0 || off > this.bytesTotal) return null;
    if (len !== null && len !== undefined && len !== '' && Number(len) !== this.bytesTotal) return null;
    this._readCompletion(res);
    return off;
  }

  async _create() {
    const f = this.file;
    let modTime = this.opts.modTime;
    if (!modTime && f.lastModified) modTime = new Date(f.lastModified);
    const tags = Array.isArray(this.opts.tags) ? this.opts.tags.join(',') : this.opts.tags;
    const meta = {
      filename: f.name,
      space_id: this.opts.spaceId,
      path: this.opts.path || undefined,
      overwrite: this.opts.overwrite === undefined ? undefined : (this.opts.overwrite ? '1' : '0'),
      mod_time: modTime ? new Date(modTime).toISOString() : undefined,
      category: this.opts.category || undefined,
      subcategory: this.opts.subcategory || undefined,
      tags: tags || undefined,
      mime: this.opts.mime || f.type || undefined,
    };
    const res = await this._request('POST', this.opts.endpoint, {
      'Upload-Length': String(this.bytesTotal),
      'Upload-Metadata': encodeMetadata(meta),
    });
    if (res.status !== 201 && res.status !== 200) throw httpError(res, 'Upload creation');
    const loc = res.getHeader('Location');
    if (!loc) throw new UploadError('Upload creation returned no Location', { code: 'bad_response' });
    this.url = this._resolveUrl(loc);
    this.bytesUploaded = 0;
    const expires = res.getHeader('Upload-Expires');
    const exp = expires ? new Date(expires) : null;
    this.store.set(this.fingerprint, {
      url: this.url, createdAt: new Date(this.opts.now()).toISOString(),
      expiresAt: exp && !Number.isNaN(exp.getTime()) ? exp.toISOString() : null,
      name: f.name, size: f.size,
    });
    this._readCompletion(res);
  }

  _progress(bytes) {
    this.bytesUploaded = Math.min(bytes, this.bytesTotal);
    const now = this.opts.now();
    if (!this._lastSample) {
      this._lastSample = { t: now, b: this.bytesUploaded };
    } else {
      const dt = (now - this._lastSample.t) / 1000;
      if (dt >= 0.25) {
        const inst = Math.max(0, (this.bytesUploaded - this._lastSample.b) / dt);
        this.speed = this.speed ? this.speed * 0.7 + inst * 0.3 : inst;
        this._lastSample = { t: now, b: this.bytesUploaded };
      }
    }
    this.eta = this.speed > 0 ? (this.bytesTotal - this.bytesUploaded) / this.speed : null;
    this._emit('progress', this.progress);
  }

  async _sendChunks() {
    const useChecksum = this.opts.checksum === true || (this.opts.checksum === 'auto' && checksumSupported());
    while (this.fileId === null) {
      if (this._paused || this._cancelled) throw abortError();
      const start = this.bytesUploaded;
      if (start >= this.bytesTotal && this.bytesTotal > 0 && this._sentFinal) return;
      const end = Math.min(start + this.opts.chunkSize, this.bytesTotal);
      const chunk = this.file.slice(start, end);
      const headers = { 'Upload-Offset': String(start), 'Content-Type': 'application/offset+octet-stream' };
      if (useChecksum && end > start) headers['Upload-Checksum'] = `sha256 ${await sha256Base64(chunk)}`;
      const res = await this._request('PATCH', this.url, headers, chunk, (loaded) => this._progress(start + loaded));
      if (res.status === 409) {
        // Offset mismatch: ask the server where it actually is.
        const off = await this._head(this.url);
        if (off === null) throw new UploadError('Upload expired on the server', { code: 'gone', retryable: true, status: 410 });
        this.bytesUploaded = off;
        continue;
      }
      if (res.status === 404 || res.status === 410) {
        throw new UploadError('Upload expired on the server', { code: 'gone', retryable: true, status: res.status });
      }
      if (res.status < 200 || res.status >= 300) throw httpError(res, 'Upload');
      const off = Number(res.getHeader('Upload-Offset'));
      if (!Number.isFinite(off) || off < start || off > this.bytesTotal) {
        throw new UploadError('Server returned an invalid offset', { code: 'bad_offset', retryable: true });
      }
      this._attempt = 0;
      this._readCompletion(res);
      this._progress(off);
      if (off >= this.bytesTotal) {
        // Final chunk accepted; the commit headers (if any) were read above.
        this._sentFinal = true;
        return;
      }
    }
  }

  async _run() {
    const fp = this.fingerprint;
    this._setState('uploading');
    for (;;) {
      if (this._paused || this._cancelled) return null;
      try {
        if (!this.url) {
          const saved = this.store.get(fp);
          if (saved) {
            const off = await this._head(saved.url);
            if (off === null) this.store.remove(fp);
            else {
              this.url = saved.url;
              this.bytesUploaded = off;
              this._progress(off);
            }
          }
          if (!this.url) await this._create();
        } else if (this._needsResync) {
          const off = await this._head(this.url);
          if (off === null) throw new UploadError('Upload expired on the server', { code: 'gone', retryable: true, status: 410 });
          this.bytesUploaded = off;
        }
        this._needsResync = false;
        if (this.fileId === null) await this._sendChunks();
        this.store.remove(fp);
        this.speed = 0;
        this.eta = 0;
        this.bytesUploaded = this.bytesTotal;
        this._setState('completed');
        const result = { fileId: this.fileId, filePath: this.filePath };
        this._emit('complete', result);
        this._resolveDone(result);
        return result;
      } catch (err) {
        if (this._paused || this._cancelled) return null;
        if (err?.name === 'AbortError') return null;
        if (err?.code === 'gone') {
          // The server forgot the upload (expired/purged): start over once.
          this.store.remove(fp);
          this.url = null;
          this.bytesUploaded = 0;
          this._sentFinal = false;
          if (this._restarts++ < 1) continue;
        }
        const retryable = err?.retryable && this._attempt < this.opts.retryDelays.length;
        if (!retryable) {
          this.error = err instanceof UploadError ? err : new UploadError(err?.message || 'Upload failed');
          this.speed = 0;
          this._setState('error');
          this._emit('error', this.error);
          throw this.error;
        }
        const delay = err.retryAfter ? err.retryAfter * 1000 : this.opts.retryDelays[this._attempt];
        this._attempt += 1;
        this._needsResync = !!this.url;
        this._setState('retrying');
        try {
          await this._sleep(delay);
        } catch {
          return null; // paused/cancelled while waiting
        }
        this._setState('uploading');
      }
    }
  }
}

/* ------------------------------------------------------------------- queue */

/**
 * Upload queue running up to `concurrency` uploads at once.
 *
 *   const q = new UploadQueue({headers: () => ({'X-CSRF-Token': csrf}), scope: userId});
 *   q.on('change', () => render(q.uploads));
 *   q.on('complete', (upload) => refreshListing(upload.fileId));
 *   q.add(file, {spaceId, path: 'Movies', category: 'movie'});
 */
export class UploadQueue {
  /**
   * @param {object} [defaults] options merged into every TusUpload
   * @param {{concurrency?: number}} [opts]
   */
  constructor(defaults = {}, { concurrency = 3 } = {}) {
    this.defaults = defaults;
    this.concurrency = concurrency;
    this.uploads = [];
    this._listeners = new Map();
    this._pumping = false;
  }

  on(type, fn) {
    if (!this._listeners.has(type)) this._listeners.set(type, new Set());
    this._listeners.get(type).add(fn);
    return () => this._listeners.get(type)?.delete(fn);
  }

  _emit(type, detail) {
    for (const fn of [...(this._listeners.get(type) || [])]) {
      try {
        fn(detail);
      } catch (err) {
        console.error('queue listener failed', err);
      }
    }
  }

  /** Merge into the defaults (e.g. a new CSRF getter or user scope). */
  configure(patch) {
    Object.assign(this.defaults, patch);
  }

  /**
   * Queue a file. Returns the TusUpload.
   * @param {File} file
   * @param {object} options TusUpload options (spaceId required)
   */
  add(file, options) {
    // Bind the upload to the user signed in now (defaults.scope); see TusUpload activeScope.
    const up = new TusUpload(file, { activeScope: () => this.defaults.scope ?? '', ...this.defaults, ...options });
    let lastEmit = 0;
    up._queueOffs = [
      up.on('state', () => {
        this._emit('change', up);
        this._pump();
      }),
      up.on('progress', () => {
        const now = Date.now();
        if (now - lastEmit > 200 || up.bytesUploaded >= up.bytesTotal) {
          lastEmit = now;
          this._emit('progress', up);
        }
      }),
      up.on('complete', () => this._emit('complete', up)),
      up.on('error', (err) => this._emit('error', { upload: up, error: err })),
    ];
    this.uploads.push(up);
    this._emit('change', up);
    this._pump();
    return up;
  }

  get active() {
    return this.uploads.filter((u) => u.state === 'uploading' || u.state === 'retrying');
  }

  _pump() {
    if (this._pumping) return;
    this._pumping = true;
    try {
      let slots = this.concurrency - this.active.length;
      for (const u of this.uploads) {
        if (slots <= 0) break;
        if (u.state === 'queued') {
          slots -= 1;
          u.start().catch(() => {}); // errors surface via events
        }
      }
    } finally {
      this._pumping = false;
    }
  }

  pause(u) {
    u.pause();
  }

  /** Re-queue a paused/failed upload (respects the concurrency limit). */
  resume(u) {
    if (u.state === 'paused' || u.state === 'error') {
      u._setState('queued');
      this._pump();
    }
  }

  async cancel(u) {
    await u.cancel();
    this._pump();
  }

  pauseAll() {
    for (const u of this.uploads) if (['queued', 'uploading', 'retrying'].includes(u.state)) u.pause();
  }

  resumeAll() {
    for (const u of this.uploads) if (u.state === 'paused' || u.state === 'error') u._setState('queued');
    this._pump();
  }

  /** Remove a finished (completed/cancelled/error) upload from the list. */
  remove(u) {
    if (!['completed', 'cancelled', 'error'].includes(u.state)) return;
    this.uploads = this.uploads.filter((x) => x !== u);
    this._emit('change', null);
  }

  clearFinished() {
    this.uploads = this.uploads.filter((u) => !['completed', 'cancelled'].includes(u.state));
    this._emit('change', null);
  }

  /**
   * Forget every upload: on sign-out or when the signed-in account changes, so the next person
   * at this browser sees none of the previous user's file names and cannot resume their uploads
   * (with their File handles and target spaces) under a new session. Unfinished uploads are
   * stopped; with terminate they are also deleted on the server (tus termination) using the
   * current session, so call it before that session is revoked.
   * @param {{terminate?: boolean}} [opts]
   * @returns {Promise<void>} settles once the termination requests are done (the list is
   *          emptied synchronously)
   */
  reset({ terminate = false } = {}) {
    const dropped = this.uploads;
    if (!dropped.length) return Promise.resolve();
    this.uploads = [];
    const pending = [];
    for (const u of dropped) {
      for (const off of u._queueOffs || []) off();
      u._queueOffs = null;
      if (terminate) pending.push(u.cancel().catch(() => {}));
      else u.discard();
    }
    this._emit('change', null);
    return Promise.all(pending).then(() => {});
  }

  /** True while anything is queued or transferring. */
  get busy() {
    return this.uploads.some((u) => ['queued', 'uploading', 'retrying'].includes(u.state));
  }

  /** Aggregate numbers for UI summaries. */
  get totals() {
    let up = 0;
    let total = 0;
    const counts = { queued: 0, uploading: 0, retrying: 0, paused: 0, completed: 0, error: 0, cancelled: 0 };
    for (const u of this.uploads) {
      counts[u.state] = (counts[u.state] || 0) + 1;
      if (u.state !== 'cancelled') {
        up += u.bytesUploaded;
        total += u.bytesTotal;
      }
    }
    const speed = this.active.reduce((s, u) => s + (u.speed || 0), 0);
    return { bytesUploaded: up, bytesTotal: total, speed, eta: speed > 0 ? (total - up) / speed : null, counts };
  }
}
