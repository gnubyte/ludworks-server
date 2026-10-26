/**
 * TOTP (RFC 6238 / RFC 4226) per VAULT-CRYPTO.md §4. HMAC runs on WebCrypto.
 */
import { VaultError, invalidArg } from './errors.js';

const B32 = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ234567';
const STEAM_CHARS = '23456789BCDFGHJKMNPQRTVWXY';
const ALGOS = { SHA1: 'SHA-1', SHA256: 'SHA-256', SHA512: 'SHA-512' };

/**
 * Decode RFC 4648 base32. Case-insensitive; spaces, hyphens and '=' padding are ignored.
 * @param {string} str
 * @returns {Uint8Array}
 */
export function base32decode(str) {
  if (typeof str !== 'string') throw invalidArg('base32 input must be a string');
  const s = str.replace(/[\s=-]/g, '').toUpperCase();
  if (s.length === 0) throw new VaultError('malformed', 'empty base32 secret');
  const out = new Uint8Array(Math.floor((s.length * 5) / 8));
  let bits = 0;
  let value = 0;
  let o = 0;
  for (let i = 0; i < s.length; i++) {
    const v = B32.indexOf(s[i]);
    if (v < 0) throw new VaultError('malformed', 'invalid base32 character');
    value = (value << 5) | v;
    bits += 5;
    if (bits >= 8) {
      out[o++] = (value >>> (bits - 8)) & 0xff;
      bits -= 8;
    }
  }
  return out;
}

/** RFC 4648 base32 encode without padding. @param {Uint8Array} bytes */
export function base32encode(bytes) {
  let out = '';
  let bits = 0;
  let value = 0;
  for (let i = 0; i < bytes.length; i++) {
    value = ((value << 8) | bytes[i]) & 0xffff;
    bits += 8;
    while (bits >= 5) {
      out += B32[(value >>> (bits - 5)) & 31];
      bits -= 5;
    }
  }
  if (bits > 0) out += B32[(value << (5 - bits)) & 31];
  return out;
}

function normalizeAlgorithm(a) {
  const k = String(a || 'SHA1').toUpperCase().replace(/[-_\s]/g, '').replace(/^HMAC/, '');
  if (!(k in ALGOS)) throw new VaultError('unsupported', `unsupported TOTP algorithm "${a}"`);
  return k;
}

function safeDecode(s) {
  try {
    return decodeURIComponent(s.replace(/\+/g, ' '));
  } catch {
    return s;
  }
}

/**
 * Parse an otpauth://totp/... URI (or steam://SECRET).
 * @param {string} uri
 * @returns {{type:'totp', secret:string, algorithm:'SHA1'|'SHA256'|'SHA512', digits:number, period:number,
 *            issuer:string, account:string, encoder:'rfc6238'|'steam'}}
 */
export function parseOtpauth(uri) {
  if (typeof uri !== 'string') throw invalidArg('otpauth URI must be a string');
  const trimmed = uri.trim();
  const steam = /^steam:\/\/([A-Za-z2-7=\s]+)$/i.exec(trimmed);
  if (steam) {
    const secret = steam[1].replace(/[\s=]/g, '').toUpperCase();
    base32decode(secret);
    return { type: 'totp', secret, algorithm: 'SHA1', digits: 5, period: 30, issuer: 'Steam', account: '', encoder: 'steam' };
  }
  const m = /^otpauth:\/\/([a-z]+)\/([^?#]*)(?:\?([^#]*))?/i.exec(trimmed);
  if (!m) throw new VaultError('malformed', 'not an otpauth:// URI');
  const type = m[1].toLowerCase();
  if (type === 'hotp') throw new VaultError('unsupported', 'HOTP (counter-based) codes are not supported');
  if (type !== 'totp') throw new VaultError('malformed', `unknown otpauth type "${m[1]}"`);
  const label = safeDecode(m[2]);
  const params = new Map();
  for (const pair of (m[3] || '').split('&')) {
    if (!pair) continue;
    const i = pair.indexOf('=');
    const k = safeDecode(i < 0 ? pair : pair.slice(0, i)).toLowerCase();
    if (!params.has(k)) params.set(k, safeDecode(i < 0 ? '' : pair.slice(i + 1)));
  }
  const secret = (params.get('secret') || '').replace(/[\s=-]/g, '').toUpperCase();
  base32decode(secret); // validates
  let issuer = params.get('issuer') || '';
  let account = label;
  const colon = label.indexOf(':');
  if (colon >= 0) {
    if (!issuer) issuer = label.slice(0, colon).trim();
    account = label.slice(colon + 1).trim();
  }
  const algorithm = normalizeAlgorithm(params.get('algorithm'));
  const digits = params.has('digits') ? Number(params.get('digits')) : 6;
  const period = params.has('period') ? Number(params.get('period')) : 30;
  const encoder = (params.get('encoder') || '').toLowerCase() === 'steam' ? 'steam' : 'rfc6238';
  validateShape(digits, period, encoder);
  return { type: 'totp', secret, algorithm, digits: encoder === 'steam' ? 5 : digits, period, issuer, account, encoder };
}

function validateShape(digits, period, encoder) {
  if (encoder !== 'steam' && (!Number.isInteger(digits) || digits < 6 || digits > 8)) {
    throw new VaultError('unsupported', 'TOTP digits must be 6, 7 or 8');
  }
  if (!Number.isInteger(period) || period < 1 || period > 86400) {
    throw new VaultError('malformed', 'TOTP period must be an integer between 1 and 86400');
  }
}

/**
 * Build an otpauth URI. Default parameters are omitted to keep URIs short and widely compatible.
 * @param {{secret:string, issuer?:string, account?:string, algorithm?:string, digits?:number, period?:number}} p
 */
export function buildOtpauth(p) {
  const secret = String(p.secret || '').replace(/[\s=-]/g, '').toUpperCase();
  base32decode(secret);
  const issuer = p.issuer || '';
  const account = p.account || '';
  const label = issuer && account ? `${encodeURIComponent(issuer)}:${encodeURIComponent(account)}` : encodeURIComponent(account || issuer || 'Ludworks');
  let q = `secret=${secret}`;
  if (issuer) q += `&issuer=${encodeURIComponent(issuer)}`;
  const algo = normalizeAlgorithm(p.algorithm);
  if (algo !== 'SHA1') q += `&algorithm=${algo}`;
  if (p.digits && p.digits !== 6) q += `&digits=${p.digits}`;
  if (p.period && p.period !== 30) q += `&period=${p.period}`;
  return `otpauth://totp/${label}?${q}`;
}

/**
 * Low-level TOTP over raw key bytes.
 * @param {Uint8Array} keyBytes
 * @param {{algorithm?:string, digits?:number, period?:number, encoder?:string}} cfg
 * @param {number} [nowMs]
 * @returns {Promise<{code:string, period:number, remaining:number}>}
 */
export async function computeTotp(keyBytes, cfg = {}, nowMs = Date.now()) {
  const algorithm = normalizeAlgorithm(cfg.algorithm);
  const encoder = cfg.encoder === 'steam' ? 'steam' : 'rfc6238';
  const digits = encoder === 'steam' ? 5 : cfg.digits ?? 6;
  const period = cfg.period ?? 30;
  validateShape(digits, period, encoder);
  if (!Number.isFinite(nowMs)) throw invalidArg('nowMs must be a finite number');
  if (!(keyBytes instanceof Uint8Array) || keyBytes.length === 0) throw new VaultError('malformed', 'TOTP secret decodes to no bytes');
  const seconds = Math.floor(nowMs / 1000);
  const counter = Math.floor(seconds / period);
  const msg = new Uint8Array(8);
  const dv = new DataView(msg.buffer);
  dv.setUint32(0, Math.floor(counter / 0x100000000), false);
  dv.setUint32(4, counter >>> 0, false);
  const key = await globalThis.crypto.subtle.importKey('raw', keyBytes, { name: 'HMAC', hash: ALGOS[algorithm] }, false, ['sign']);
  const mac = new Uint8Array(await globalThis.crypto.subtle.sign('HMAC', key, msg));
  const off = mac[mac.length - 1] & 0x0f;
  const bin = ((mac[off] & 0x7f) << 24) | (mac[off + 1] << 16) | (mac[off + 2] << 8) | mac[off + 3];
  let code;
  if (encoder === 'steam') {
    let v = bin;
    code = '';
    for (let i = 0; i < 5; i++) {
      code += STEAM_CHARS[v % STEAM_CHARS.length];
      v = Math.floor(v / STEAM_CHARS.length);
    }
  } else {
    code = String(bin % 10 ** digits).padStart(digits, '0');
  }
  return { code, period, remaining: period - (((seconds % period) + period) % period) };
}

/**
 * Generate the current TOTP code from an otpauth:// URI, steam:// URI or bare base32 secret.
 * @param {string} secretOrUri
 * @param {number} [nowMs]
 */
export async function generateTotp(secretOrUri, nowMs = Date.now()) {
  if (typeof secretOrUri !== 'string' || !secretOrUri.trim()) throw invalidArg('TOTP secret is empty');
  const s = secretOrUri.trim();
  const cfg = /^(otpauth|steam):\/\//i.test(s) ? parseOtpauth(s) : { secret: s, algorithm: 'SHA1', digits: 6, period: 30 };
  return computeTotp(base32decode(cfg.secret), cfg, nowMs);
}
