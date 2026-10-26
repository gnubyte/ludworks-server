/**
 * Byte helpers shared by the vault modules. Pure ES2022; works in browsers, MV3 service workers and Node.
 */
import { VaultError, invalidArg } from './errors.js';

const encoder = new TextEncoder();
const strictDecoder = new TextDecoder('utf-8', { fatal: true });

/** UTF-8 encode a string. @param {string} s @returns {Uint8Array} */
export function utf8Encode(s) {
  return encoder.encode(s);
}

/** Strict UTF-8 decode (throws VaultError 'malformed' on invalid sequences). @param {Uint8Array} b */
export function utf8Decode(b) {
  try {
    return strictDecoder.decode(b);
  } catch (e) {
    throw new VaultError('malformed', 'invalid UTF-8', { cause: e });
  }
}

/** @param {unknown} v @returns {v is Uint8Array} */
export function isBytes(v) {
  return v instanceof Uint8Array;
}

/** Accept Uint8Array / ArrayBuffer / typed-array views and return a Uint8Array view. */
export function asBytes(v, what = 'value') {
  if (v instanceof Uint8Array) return v;
  if (v instanceof ArrayBuffer) return new Uint8Array(v);
  if (ArrayBuffer.isView(v)) return new Uint8Array(v.buffer, v.byteOffset, v.byteLength);
  throw invalidArg(`${what} must be a Uint8Array`);
}

/** @param {...Uint8Array} parts @returns {Uint8Array} */
export function concatBytes(...parts) {
  let n = 0;
  for (const p of parts) n += p.length;
  const out = new Uint8Array(n);
  let o = 0;
  for (const p of parts) {
    out.set(p, o);
    o += p.length;
  }
  return out;
}

/** Constant-time (for equal lengths) byte comparison. */
export function bytesEqual(a, b) {
  if (a.length !== b.length) return false;
  let d = 0;
  for (let i = 0; i < a.length; i++) d |= a[i] ^ b[i];
  return d === 0;
}

/** Overwrite a buffer with zeros (best effort; JS cannot guarantee no copies exist). */
export function wipe(b) {
  if (b instanceof Uint8Array) b.fill(0);
}

const HEX = '0123456789abcdef';
/** @param {Uint8Array} b @returns {string} lowercase hex */
export function toHex(b) {
  let s = '';
  for (let i = 0; i < b.length; i++) s += HEX[b[i] >> 4] + HEX[b[i] & 15];
  return s;
}

/** @param {string} s @returns {Uint8Array} */
export function fromHex(s) {
  if (typeof s !== 'string' || s.length % 2 !== 0 || !/^[0-9a-fA-F]*$/.test(s)) {
    throw new VaultError('malformed', 'invalid hex string');
  }
  const out = new Uint8Array(s.length / 2);
  for (let i = 0; i < out.length; i++) out[i] = parseInt(s.substr(i * 2, 2), 16);
  return out;
}

/**
 * Standard base64 with padding (RFC 4648 §4).
 * @param {Uint8Array} bytes
 * @returns {string}
 */
export function b64encode(bytes) {
  bytes = asBytes(bytes, 'bytes');
  let bin = '';
  // Chunked so String.fromCharCode never receives too many arguments.
  for (let i = 0; i < bytes.length; i += 0x8000) {
    bin += String.fromCharCode.apply(null, bytes.subarray(i, i + 0x8000));
  }
  return btoa(bin);
}

const B64_RE = /^[A-Za-z0-9+/]*$/;

/**
 * Strict standard base64 decode. Padding is optional but if present must be correct; whitespace,
 * URL-safe characters and stray symbols are rejected (callers strip whitespace themselves when a format
 * allows it).
 * @param {string} s
 * @returns {Uint8Array}
 */
export function b64decode(s) {
  if (typeof s !== 'string') throw new VaultError('malformed', 'base64 input must be a string');
  let body = s;
  const pad = s.endsWith('==') ? 2 : s.endsWith('=') ? 1 : 0;
  if (pad) {
    if (s.length % 4 !== 0) throw new VaultError('malformed', 'invalid base64 padding');
    body = s.slice(0, s.length - pad);
  }
  if (!B64_RE.test(body) || body.length % 4 === 1) throw new VaultError('malformed', 'invalid base64');
  let bin;
  try {
    bin = atob(body);
  } catch (e) {
    throw new VaultError('malformed', 'invalid base64', { cause: e });
  }
  const out = new Uint8Array(bin.length);
  for (let i = 0; i < bin.length; i++) out[i] = bin.charCodeAt(i);
  return out;
}

/** CSPRNG bytes (getRandomValues is limited to 65536 bytes per call). @param {number} n */
export function randomBytes(n) {
  if (!Number.isInteger(n) || n < 0) throw invalidArg('randomBytes: n must be a non-negative integer');
  const out = new Uint8Array(n);
  for (let i = 0; i < n; i += 65536) globalThis.crypto.getRandomValues(out.subarray(i, Math.min(n, i + 65536)));
  return out;
}

/** SHA-256 / SHA-512 via WebCrypto. */
export async function sha256(data) {
  return new Uint8Array(await globalThis.crypto.subtle.digest('SHA-256', data));
}
export async function sha512(data) {
  return new Uint8Array(await globalThis.crypto.subtle.digest('SHA-512', data));
}

/** HMAC-SHA-256/512 sign with raw key bytes. */
export async function hmac(hash, keyBytes, data) {
  const k = await globalThis.crypto.subtle.importKey('raw', keyBytes, { name: 'HMAC', hash }, false, ['sign']);
  return new Uint8Array(await globalThis.crypto.subtle.sign('HMAC', k, data));
}

/** Constant-time HMAC verification via WebCrypto. */
export async function hmacVerify(hash, keyBytes, mac, data) {
  const k = await globalThis.crypto.subtle.importKey('raw', keyBytes, { name: 'HMAC', hash }, false, ['verify']);
  return globalThis.crypto.subtle.verify('HMAC', k, mac, data);
}

/** Little-endian integer writers used by the binary formats. */
export function u32le(n) {
  const b = new Uint8Array(4);
  new DataView(b.buffer).setUint32(0, n >>> 0, true);
  return b;
}
export function u64le(n) {
  if (!Number.isSafeInteger(n) || n < 0) throw invalidArg('u64le: value out of range');
  const b = new Uint8Array(8);
  const dv = new DataView(b.buffer);
  dv.setUint32(0, n % 0x100000000, true);
  dv.setUint32(4, Math.floor(n / 0x100000000), true);
  return b;
}

/** Collect a ReadableStream of Uint8Array chunks into one buffer, enforcing a size cap. */
export async function readAllCapped(readable, maxBytes, what) {
  const reader = readable.getReader();
  const chunks = [];
  let total = 0;
  try {
    for (;;) {
      const { done, value } = await reader.read();
      if (done) break;
      total += value.length;
      if (total > maxBytes) {
        reader.cancel().catch(() => {});
        throw new VaultError('limit_exceeded', `${what} exceeds ${maxBytes} bytes`);
      }
      chunks.push(value);
    }
  } finally {
    try {
      reader.releaseLock();
    } catch {
      /* already released or errored */
    }
  }
  return concatBytes(...chunks);
}

/** gzip-compress bytes with CompressionStream. */
export async function gzip(bytes) {
  const cs = new CompressionStream('gzip');
  const writer = cs.writable.getWriter();
  writer.write(bytes).catch(() => {});
  writer.close().catch(() => {});
  return readAllCapped(cs.readable, Number.MAX_SAFE_INTEGER, 'gzip output');
}

/** gunzip bytes with a decompressed-size cap (zip-bomb defence). */
export async function gunzip(bytes, maxBytes) {
  const ds = new DecompressionStream('gzip');
  const writer = ds.writable.getWriter();
  writer.write(bytes).catch(() => {});
  writer.close().catch(() => {});
  try {
    return await readAllCapped(ds.readable, maxBytes, 'decompressed data');
  } catch (e) {
    if (e instanceof VaultError) throw e;
    throw new VaultError('corrupt', 'gzip data is corrupt', { cause: e });
  }
}
