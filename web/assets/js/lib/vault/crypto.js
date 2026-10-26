/**
 * Ludworks vault end-to-end encryption (docs/spec/VAULT-CRYPTO.md §1–§2, §7).
 *
 * The server only ever sees KDF parameters and sealed strings ("v1.<b64 nonce>.<b64 ct||tag>").
 * Everything here uses WebCrypto (globalThis.crypto.subtle), so it runs unchanged in browsers,
 * MV3 extension service workers and Node.
 */
import { VaultError, invalidArg } from './errors.js';
import { asBytes, b64decode, b64encode, randomBytes, utf8Decode, utf8Encode } from './bytes.js';

export { VaultError } from './errors.js';
export { b64decode, b64encode, randomBytes } from './bytes.js';

const subtle = globalThis.crypto.subtle;

export const KDF_PBKDF2 = 'pbkdf2-sha256';
export const DEFAULT_ITERATIONS = 600000;
/** Server rejects lower counts; clients refuse to create or unlock with fewer too. */
export const MIN_ITERATIONS = 600000;
export const MAX_ITERATIONS = 10000000;
export const SALT_BYTES = 16;
export const KEY_BYTES = 32;
export const NONCE_BYTES = 12;

const WRAP_INFO = 'ludworks/vault/wrap/v1';
export const AAD_KEY = 'ludworks/vault/key/v1';
export const AAD_KEY_CHECK = 'ludworks/vault/check/v1';
export const AAD_ITEM_PREFIX = 'ludworks/vault/item/v1:';
export const AAD_FOLDER_PREFIX = 'ludworks/vault/folder/v1:';
const SEAL_VERSION = 'v1';

// Lowercase hyphenated UUID; ids are bound into the AAD, so odd strings are refused outright.
const ID_RE = /^[0-9a-f]{8}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{4}-[0-9a-f]{12}$/;

/** @returns {string} lowercase UUID v4 with hyphens */
export function uuid() {
  if (typeof globalThis.crypto.randomUUID === 'function') return globalThis.crypto.randomUUID();
  const b = randomBytes(16);
  b[6] = (b[6] & 0x0f) | 0x40;
  b[8] = (b[8] & 0x3f) | 0x80;
  const h = Array.from(b, (x) => x.toString(16).padStart(2, '0')).join('');
  return `${h.slice(0, 8)}-${h.slice(8, 12)}-${h.slice(12, 16)}-${h.slice(16, 20)}-${h.slice(20)}`;
}

/** @param {string} id */
export function isValidId(id) {
  return typeof id === 'string' && ID_RE.test(id);
}

function checkId(id) {
  if (!isValidId(id)) throw invalidArg('id must be a lowercase hyphenated UUID');
}

function checkIterations(iterations, min) {
  if (!Number.isInteger(iterations) || iterations < min || iterations > MAX_ITERATIONS) {
    throw invalidArg(`kdf_iterations must be an integer in [${min}, ${MAX_ITERATIONS}]`);
  }
}

/**
 * master_key = PBKDF2-HMAC-SHA256(UTF-8(NFC(password)), salt, iterations, 32).
 * Accepts any iteration count in [1, MAX_ITERATIONS] so known-answer tests can use small counts; the
 * vault-level functions enforce MIN_ITERATIONS.
 * @param {string} password
 * @param {Uint8Array} saltBytes
 * @param {number} iterations
 * @returns {Promise<Uint8Array>} 32 bytes
 */
export async function deriveMasterKey(password, saltBytes, iterations) {
  if (typeof password !== 'string') throw invalidArg('password must be a string');
  const salt = asBytes(saltBytes, 'salt');
  checkIterations(iterations, 1);
  const pw = utf8Encode(password.normalize('NFC'));
  const base = await subtle.importKey('raw', pw, 'PBKDF2', false, ['deriveBits']);
  pw.fill(0);
  const bits = await subtle.deriveBits({ name: 'PBKDF2', hash: 'SHA-256', salt, iterations }, base, 256);
  return new Uint8Array(bits);
}

function hkdfParams(info) {
  // Empty salt == RFC 5869 default salt of HashLen zero bytes (HMAC zero-pads short keys).
  return { name: 'HKDF', hash: 'SHA-256', salt: new Uint8Array(0), info: utf8Encode(info) };
}

/**
 * wrap_key = HKDF-SHA256(IKM=master_key, salt=empty, info="ludworks/vault/wrap/v1", L=32), returned as a
 * non-extractable AES-GCM CryptoKey.
 * @param {Uint8Array} masterKeyBytes
 * @returns {Promise<CryptoKey>}
 */
export async function deriveWrapKey(masterKeyBytes) {
  const ikm = asBytes(masterKeyBytes, 'master key');
  if (ikm.length !== KEY_BYTES) throw invalidArg('master key must be 32 bytes');
  const base = await subtle.importKey('raw', ikm, 'HKDF', false, ['deriveKey']);
  return subtle.deriveKey(hkdfParams(WRAP_INFO), base, { name: 'AES-GCM', length: 256 }, false, [
    'encrypt',
    'decrypt',
  ]);
}

/** Test/vector helper: the raw wrap_key bytes (never used by the app). */
async function deriveWrapKeyBytes(masterKeyBytes) {
  const base = await subtle.importKey('raw', asBytes(masterKeyBytes), 'HKDF', false, ['deriveBits']);
  return new Uint8Array(await subtle.deriveBits(hkdfParams(WRAP_INFO), base, 256));
}

/**
 * Import 32 raw bytes as a non-extractable AES-256-GCM key. Callers that seal many values (e.g. a full
 * sync) should import the vault key once and pass the CryptoKey around.
 * @param {Uint8Array} bytes
 * @returns {Promise<CryptoKey>}
 */
export async function importAesKey(bytes) {
  const k = asBytes(bytes, 'key');
  if (k.length !== KEY_BYTES) throw invalidArg('AES key must be 32 bytes');
  return subtle.importKey('raw', k, { name: 'AES-GCM' }, false, ['encrypt', 'decrypt']);
}

async function toKey(keyOrBytes) {
  if (typeof CryptoKey !== 'undefined' && keyOrBytes instanceof CryptoKey) return keyOrBytes;
  if (keyOrBytes && typeof keyOrBytes === 'object' && keyOrBytes.type === 'secret' && keyOrBytes.algorithm) {
    return keyOrBytes; // CryptoKey from another realm
  }
  return importAesKey(keyOrBytes);
}

function toPlainBytes(p) {
  return typeof p === 'string' ? utf8Encode(p) : asBytes(p, 'plaintext');
}

async function sealWithNonce(keyOrBytes, plaintext, aad, nonce) {
  if (typeof aad !== 'string') throw invalidArg('aad must be a string');
  const iv = asBytes(nonce, 'nonce');
  if (iv.length !== NONCE_BYTES) throw invalidArg('nonce must be 12 bytes');
  const key = await toKey(keyOrBytes);
  const ct = await subtle.encrypt(
    { name: 'AES-GCM', iv, additionalData: utf8Encode(aad), tagLength: 128 },
    key,
    toPlainBytes(plaintext),
  );
  return `${SEAL_VERSION}.${b64encode(iv)}.${b64encode(new Uint8Array(ct))}`;
}

/**
 * Seal(key, plaintext, aad) = "v1." + b64(nonce) + "." + b64(ciphertext || tag), AES-256-GCM, random nonce.
 * @param {CryptoKey|Uint8Array} keyOrBytes
 * @param {Uint8Array|string} plaintextBytes (strings are UTF-8 encoded)
 * @param {string} aadString
 * @returns {Promise<string>}
 */
export async function seal(keyOrBytes, plaintextBytes, aadString) {
  return sealWithNonce(keyOrBytes, plaintextBytes, aadString, randomBytes(NONCE_BYTES));
}

/**
 * Open a sealed string. Throws VaultError: 'unsupported_version' for an unknown prefix, 'malformed' for
 * a structurally invalid string, 'decrypt_failed' for a wrong key / wrong AAD / tampering.
 * @param {CryptoKey|Uint8Array} keyOrBytes
 * @param {string} sealed
 * @param {string} aadString
 * @returns {Promise<Uint8Array>}
 */
export async function open(keyOrBytes, sealed, aadString) {
  if (typeof sealed !== 'string') throw new VaultError('malformed', 'sealed value must be a string');
  if (typeof aadString !== 'string') throw invalidArg('aad must be a string');
  const parts = sealed.split('.');
  if (parts.length !== 3) throw new VaultError('malformed', 'sealed value must have 3 parts');
  if (parts[0] !== SEAL_VERSION) throw new VaultError('unsupported_version', `unknown sealed version "${parts[0]}"`);
  const iv = b64decode(parts[1]);
  const ct = b64decode(parts[2]);
  if (iv.length !== NONCE_BYTES || ct.length < 16) throw new VaultError('malformed', 'sealed value has bad lengths');
  const key = await toKey(keyOrBytes);
  try {
    const pt = await subtle.decrypt(
      { name: 'AES-GCM', iv, additionalData: utf8Encode(aadString), tagLength: 128 },
      key,
      ct,
    );
    return new Uint8Array(pt);
  } catch (e) {
    throw new VaultError('decrypt_failed', 'authentication failed', { cause: e });
  }
}

async function wrapParams(vaultKey, password, iterations, salt) {
  const master = await deriveMasterKey(password, salt, iterations);
  const wrapKey = await deriveWrapKey(master);
  master.fill(0);
  const wrapped = await seal(wrapKey, vaultKey, AAD_KEY);
  return { kdf: KDF_PBKDF2, kdf_iterations: iterations, kdf_salt: b64encode(salt), wrapped_key: wrapped };
}

function checkPassword(password) {
  if (typeof password !== 'string' || password.length === 0) throw invalidArg('master password must be a non-empty string');
}

/**
 * key_check = Seal(vault_key, "", AAD "ludworks/vault/check/v1") (§1): a commitment to the vault key that
 * lets a client holding a remembered key confirm it is the vault's current key without the master password.
 * @param {CryptoKey|Uint8Array} vaultKey
 * @returns {Promise<string>}
 */
export async function createKeyCheck(vaultKey) {
  return seal(vaultKey, new Uint8Array(0), AAD_KEY_CHECK);
}

/**
 * True when `keyCheck` opens under `vaultKey` (so the key is the vault's current key); false for a wrong
 * key, a malformed or missing value.
 * @param {CryptoKey|Uint8Array} vaultKey
 * @param {string|null|undefined} keyCheck
 * @returns {Promise<boolean>}
 */
export async function verifyKeyCheck(vaultKey, keyCheck) {
  if (typeof keyCheck !== 'string' || !keyCheck) return false;
  try {
    return (await open(vaultKey, keyCheck, AAD_KEY_CHECK)).length === 0;
  } catch {
    return false;
  }
}

/**
 * Initialise a new vault: random 32-byte vault key wrapped under the master password, plus its key_check.
 * Also used to rotate the vault key (§2.3): the result is a fresh key with fresh parameters.
 * @param {string} password
 * @param {{iterations?: number}} [opts]
 * @returns {Promise<{params:{kdf:string,kdf_iterations:number,kdf_salt:string,wrapped_key:string,key_check:string}, vaultKey:Uint8Array}>}
 */
export async function createVault(password, { iterations = DEFAULT_ITERATIONS } = {}) {
  checkPassword(password);
  checkIterations(iterations, MIN_ITERATIONS);
  const vaultKey = randomBytes(KEY_BYTES);
  const params = await wrapParams(vaultKey, password, iterations, randomBytes(SALT_BYTES));
  params.key_check = await createKeyCheck(vaultKey);
  return { params, vaultKey };
}

/**
 * Unlock: derive wrap_key and open wrapped_key. Wrong password → VaultError('bad_password').
 * Parameters below MIN_ITERATIONS are refused: a malicious server must not be able to make a client
 * derive (and later re-wrap) with a weak KDF.
 * @param {string} password
 * @param {{kdf:string,kdf_iterations:number,kdf_salt:string,wrapped_key:string}} params
 * @returns {Promise<Uint8Array>} vault key (32 bytes)
 */
export async function unlockVault(password, params) {
  if (typeof password !== 'string') throw invalidArg('password must be a string');
  if (!params || params.kdf !== KDF_PBKDF2) throw new VaultError('unsupported', 'unsupported vault kdf');
  checkIterations(params.kdf_iterations, MIN_ITERATIONS);
  const salt = b64decode(String(params.kdf_salt ?? ''));
  if (salt.length < SALT_BYTES || salt.length > 64) throw new VaultError('malformed', 'kdf_salt must be 16-64 bytes');
  const master = await deriveMasterKey(password, salt, params.kdf_iterations);
  const wrapKey = await deriveWrapKey(master);
  master.fill(0);
  let vaultKey;
  try {
    vaultKey = await open(wrapKey, params.wrapped_key, AAD_KEY);
  } catch (e) {
    if (e instanceof VaultError && e.code === 'decrypt_failed') {
      throw new VaultError('bad_password', 'wrong master password', { cause: e });
    }
    throw e;
  }
  if (vaultKey.length !== KEY_BYTES) throw new VaultError('malformed', 'unwrapped vault key has wrong length');
  return vaultKey;
}

/**
 * Change master password: fresh salt, same vault key. Send the result plus previous_wrapped_key to
 * PUT /vault/key. Callers re-wrapping an existing vault must pass the vault's current iteration count
 * (or more; see keepIterations): the default would silently lower a stronger work factor.
 * @param {Uint8Array} vaultKey
 * @param {string} newPassword
 * @param {{iterations?: number}} [opts]
 */
export async function rewrapVaultKey(vaultKey, newPassword, { iterations = DEFAULT_ITERATIONS } = {}) {
  const vk = asBytes(vaultKey, 'vault key');
  if (vk.length !== KEY_BYTES) throw invalidArg('vault key must be 32 bytes');
  checkPassword(newPassword);
  checkIterations(iterations, MIN_ITERATIONS);
  return wrapParams(vk, newPassword, iterations, randomBytes(SALT_BYTES));
}

/**
 * Iteration count to use when re-wrapping or rotating an existing vault: never below the vault's current
 * count (a work factor chosen on another device is kept) and never below the default.
 * @param {{kdf_iterations?: number}|null|undefined} params current GET /vault parameters
 * @returns {number}
 */
export function keepIterations(params) {
  const cur = params?.kdf_iterations;
  if (!Number.isInteger(cur)) return DEFAULT_ITERATIONS;
  return Math.min(MAX_ITERATIONS, Math.max(cur, DEFAULT_ITERATIONS));
}

function parseJson(bytes) {
  try {
    return JSON.parse(utf8Decode(bytes));
  } catch (e) {
    throw new VaultError('malformed', 'decrypted payload is not valid JSON', { cause: e });
  }
}

/** Encrypt an item plaintext object (§3) → sealed `data`. */
export async function encryptItem(vaultKey, id, obj) {
  checkId(id);
  if (!obj || typeof obj !== 'object' || Array.isArray(obj)) throw invalidArg('item must be an object');
  return seal(vaultKey, utf8Encode(JSON.stringify(obj)), AAD_ITEM_PREFIX + id);
}

/** Decrypt an item's sealed `data` → plaintext object. */
export async function decryptItem(vaultKey, id, data) {
  checkId(id);
  const obj = parseJson(await open(vaultKey, data, AAD_ITEM_PREFIX + id));
  if (!obj || typeof obj !== 'object' || Array.isArray(obj)) throw new VaultError('malformed', 'item payload is not an object');
  return obj;
}

/** Encrypt a folder name → sealed `name` (plaintext is JSON {"name": ...}). */
export async function encryptFolder(vaultKey, id, name) {
  checkId(id);
  if (typeof name !== 'string') throw invalidArg('folder name must be a string');
  return seal(vaultKey, utf8Encode(JSON.stringify({ name })), AAD_FOLDER_PREFIX + id);
}

/** Decrypt a folder's sealed `name` → string. */
export async function decryptFolder(vaultKey, id, data) {
  checkId(id);
  const obj = parseJson(await open(vaultKey, data, AAD_FOLDER_PREFIX + id));
  if (!obj || typeof obj.name !== 'string') throw new VaultError('malformed', 'folder payload has no name');
  return obj.name;
}

/**
 * Test-only hooks. sealWithNonce exists solely to produce deterministic test vectors; reusing a nonce
 * with the same key breaks AES-GCM, so application code must never call it.
 */
export const __testing = Object.freeze({ sealWithNonce, deriveWrapKeyBytes });
