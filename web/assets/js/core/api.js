// HTTP client for /api/v1 (API.md). JSON in/out, same-origin cookies, CSRF
// header on unsafe methods, typed errors, auth/setup redirects, XHR uploads.

const UNSAFE = new Set(['POST', 'PUT', 'PATCH', 'DELETE']);

/** Error thrown for any non-2xx response or network failure. */
export class ApiError extends Error {
  /**
   * @param {{status?: number, code?: string, message?: string, details?: any, retryAfter?: number|null}} init
   */
  constructor({ status = 0, code = 'internal', message = 'Request failed', details = null, retryAfter = null } = {}) {
    super(message);
    this.name = 'ApiError';
    this.status = status;
    this.code = code;
    this.details = details;
    this.retryAfter = retryAfter;
  }

  /** Field validation messages from a 422 (`details.fields`), or null. */
  get fields() {
    const f = this.details && this.details.fields;
    return f && typeof f === 'object' ? f : null;
  }
}

/** True for AbortController cancellations (fetch or XHR). */
export function isAbortError(err) {
  return err?.name === 'AbortError';
}

/** Parse a Retry-After header (seconds or HTTP date) into seconds. */
export function parseRetryAfter(value, now = Date.now()) {
  if (!value) return null;
  const n = Number(value);
  if (Number.isFinite(n)) return Math.max(0, Math.ceil(n));
  const t = Date.parse(value);
  return Number.isNaN(t) ? null : Math.max(0, Math.ceil((t - now) / 1000));
}

/**
 * Build an ApiError from a status and a decoded body.
 * @param {number} status
 * @param {any} body parsed JSON (or null)
 * @param {string|null} retryAfterHeader
 */
export function errorFromBody(status, body, retryAfterHeader = null) {
  const e = body && typeof body === 'object' && body.error && typeof body.error === 'object' ? body.error : null;
  const fallback = {
    400: ['bad_request', 'The request was invalid.'],
    401: ['unauthorized', 'Please sign in.'],
    403: ['forbidden', 'You do not have permission to do that.'],
    404: ['not_found', 'Not found.'],
    409: ['conflict', 'That conflicts with the current state.'],
    413: ['payload_too_large', 'That is too large.'],
    429: ['rate_limited', 'Too many requests. Please slow down.'],
    503: ['unavailable', 'The server is unavailable.'],
  }[status] || ['internal', `Request failed (${status}).`];
  return new ApiError({
    status,
    code: typeof e?.code === 'string' ? e.code : fallback[0],
    message: typeof e?.message === 'string' && e.message ? e.message : fallback[1],
    details: e?.details ?? null,
    retryAfter: parseRetryAfter(retryAfterHeader),
  });
}

function isBodyRaw(body) {
  return (typeof Blob !== 'undefined' && body instanceof Blob)
    || (typeof FormData !== 'undefined' && body instanceof FormData)
    || (typeof URLSearchParams !== 'undefined' && body instanceof URLSearchParams)
    || body instanceof ArrayBuffer || ArrayBuffer.isView(body)
    || (typeof ReadableStream !== 'undefined' && body instanceof ReadableStream)
    || typeof body === 'string';
}

/**
 * Create an API client.
 * @param {{base?: string, getCsrf?: () => string|null, onUnauthorized?: (e: ApiError) => void,
 *          onSetupRequired?: (e: ApiError) => void, fetch?: typeof fetch}} [options]
 */
export function createApi(options = {}) {
  const cfg = {
    base: '/api/v1',
    getCsrf: () => null,
    onUnauthorized: null,
    onSetupRequired: null,
    fetch: (...args) => globalThis.fetch(...args),
    ...options,
  };

  /**
   * Resolve an API path (relative to base unless it starts with /api/) plus a query object.
   * null/undefined/'' query values are omitted; arrays are comma-joined.
   */
  function url(path, query) {
    let p = path.startsWith('/api/') || /^https?:/i.test(path) ? path : cfg.base + (path.startsWith('/') ? path : `/${path}`);
    if (query) {
      const qs = new URLSearchParams();
      for (const [k, v] of Object.entries(query)) {
        if (v === undefined || v === null || v === '') continue;
        qs.set(k, Array.isArray(v) ? v.join(',') : String(v));
      }
      const str = qs.toString();
      if (str) p += (p.includes('?') ? '&' : '?') + str;
    }
    return p;
  }

  function sideEffects(err, noRedirect) {
    if (err.status === 401 && err.code === 'unauthorized' && !noRedirect) cfg.onUnauthorized?.(err);
    if (err.status === 503 && err.code === 'setup_required') cfg.onSetupRequired?.(err);
  }

  /**
   * Perform a request.
   * @param {string} method
   * @param {string} path
   * @param {{body?: any, query?: object, headers?: object, signal?: AbortSignal, raw?: boolean,
   *          noRedirect?: boolean, timeout?: number}} [opts]
   *   raw → resolve with the Response (still throws on non-2xx);
   *   noRedirect → don't trigger the 401 → /login redirect (login forms, probes).
   * @returns {Promise<any>} parsed JSON, text, or null for 204
   */
  async function request(method, path, opts = {}) {
    const { body, query, headers = {}, signal, raw = false, noRedirect = false, timeout = 0 } = opts;
    const m = method.toUpperCase();
    const hdrs = new Headers(headers);
    let payload;
    if (body !== undefined && body !== null) {
      if (isBodyRaw(body)) payload = body;
      else {
        payload = JSON.stringify(body);
        hdrs.set('Content-Type', 'application/json');
      }
    }
    if (!hdrs.has('Accept')) hdrs.set('Accept', 'application/json');
    if (UNSAFE.has(m)) {
      const csrf = cfg.getCsrf?.();
      if (csrf) hdrs.set('X-CSRF-Token', csrf);
    }

    let sig = signal;
    let timer = null;
    if (timeout > 0) {
      const ctrl = new AbortController();
      timer = setTimeout(() => ctrl.abort(new DOMException('Request timed out', 'TimeoutError')), timeout);
      sig = signal && AbortSignal.any ? AbortSignal.any([signal, ctrl.signal]) : ctrl.signal;
    }

    let res;
    try {
      res = await cfg.fetch(url(path, query), {
        method: m, headers: hdrs, body: payload, signal: sig,
        credentials: 'same-origin', cache: 'no-store', redirect: 'follow',
      });
    } catch (err) {
      if (isAbortError(err)) throw err;
      if (err?.name === 'TimeoutError') {
        throw new ApiError({ status: 0, code: 'timeout', message: 'The server took too long to respond.' });
      }
      throw new ApiError({ status: 0, code: 'network_error', message: 'Could not reach the server. Check your connection.' });
    } finally {
      if (timer) clearTimeout(timer);
    }

    if (!res.ok) {
      let data = null;
      try {
        if ((res.headers.get('content-type') || '').includes('json')) data = await res.json();
      } catch {
        /* non-JSON error body */
      }
      const err = errorFromBody(res.status, data, res.headers.get('retry-after'));
      sideEffects(err, noRedirect);
      throw err;
    }
    if (raw) return res;
    if (res.status === 204 || res.status === 205) return null;
    const ct = res.headers.get('content-type') || '';
    if (ct.includes('application/json')) {
      const text = await res.text();
      return text ? JSON.parse(text) : null;
    }
    return res.text();
  }

  /**
   * Send a raw body with upload progress (XHR — fetch has no upload progress).
   * @param {string} method
   * @param {string} path
   * @param {Blob|ArrayBuffer|FormData} body
   * @param {{query?: object, headers?: object, signal?: AbortSignal, noRedirect?: boolean,
   *          onProgress?: (p: {loaded: number, total: number, fraction: number}) => void}} [opts]
   */
  function upload(method, path, body, opts = {}) {
    const { query, headers = {}, signal, onProgress, noRedirect = false } = opts;
    return new Promise((resolve, reject) => {
      if (signal?.aborted) {
        reject(new DOMException('Aborted', 'AbortError'));
        return;
      }
      const xhr = new XMLHttpRequest();
      xhr.open(method.toUpperCase(), url(path, query));
      xhr.withCredentials = false; // same-origin: cookies are sent anyway
      xhr.setRequestHeader('Accept', 'application/json');
      for (const [k, v] of Object.entries(headers)) xhr.setRequestHeader(k, v);
      if (UNSAFE.has(method.toUpperCase())) {
        const csrf = cfg.getCsrf?.();
        if (csrf) xhr.setRequestHeader('X-CSRF-Token', csrf);
      }
      if (onProgress) {
        xhr.upload.addEventListener('progress', (e) => {
          const total = e.lengthComputable ? e.total : (body?.size ?? 0);
          onProgress({ loaded: e.loaded, total, fraction: total ? e.loaded / total : 0 });
        });
      }
      const onAbort = () => xhr.abort();
      signal?.addEventListener('abort', onAbort, { once: true });
      xhr.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')));
      xhr.addEventListener('error', () => reject(new ApiError({ status: 0, code: 'network_error', message: 'Upload failed: network error.' })));
      xhr.addEventListener('load', () => {
        signal?.removeEventListener('abort', onAbort);
        let data = null;
        if (xhr.responseText && (xhr.getResponseHeader('content-type') || '').includes('json')) {
          try {
            data = JSON.parse(xhr.responseText);
          } catch {
            data = null;
          }
        }
        if (xhr.status >= 200 && xhr.status < 300) resolve(data);
        else {
          const err = errorFromBody(xhr.status, data, xhr.getResponseHeader('retry-after'));
          sideEffects(err, noRedirect);
          reject(err);
        }
      });
      xhr.send(body);
    });
  }

  /** Start a browser download of a GET endpoint (cookie auth, no JS buffering). */
  function download(path, query) {
    const a = document.createElement('a');
    a.href = url(path, query);
    a.download = '';
    a.rel = 'noopener';
    a.className = 'hidden';
    document.body.appendChild(a);
    a.click();
    a.remove();
  }

  return {
    request,
    upload,
    download,
    url,
    get: (path, opts) => request('GET', path, opts),
    post: (path, body, opts) => request('POST', path, { ...opts, body }),
    put: (path, body, opts) => request('PUT', path, { ...opts, body }),
    patch: (path, body, opts) => request('PATCH', path, { ...opts, body }),
    del: (path, opts) => request('DELETE', path, opts),
    /** Update configuration (handlers, csrf getter). */
    configure(patch) {
      Object.assign(cfg, patch);
    },
    /** Current CSRF token (for custom transports such as tus). */
    csrf: () => cfg.getCsrf?.() || null,
  };
}

/** The app-wide client; app.js wires CSRF and redirect handlers into it. */
export const api = createApi();
