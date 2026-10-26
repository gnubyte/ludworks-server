/**
 * KeePass KDBX reader (3.1, 4.0, 4.1) and writer (4.0, or 4.1 when the tree uses 4.1-only fields).
 *
 * Read support: outer cipher AES-256-CBC or ChaCha20; KDF AES-KDF, Argon2d or Argon2id (v0x10/0x13);
 * KDBX 4 HMAC-SHA256 block stream / KDBX 3 SHA-256 hashed block stream; gzip; inner random stream
 * Salsa20 (id 2) or ChaCha20 (id 3); composite key = password and/or key file.
 *
 * Errors (VaultError.code): 'bad_credentials' (wrong password / key file), 'bad_keyfile', 'corrupt',
 * 'unsupported', 'limit_exceeded', 'invalid_argument'.
 *
 * The returned / accepted tree is the KdbxTree documented in keepass-xml.js, plus on read:
 *   version: {major, minor}
 *   header:  {cipher: 'aes'|'chacha20', compression: boolean, innerStream: 'salsa20'|'chacha20'|'none',
 *             kdf: {type:'aes', rounds} | {type:'argon2d'|'argon2id', memory /* bytes *\/, iterations, parallelism, version}}
 */
import { VaultError, invalidArg } from './errors.js';
import {
  asBytes, b64decode, b64encode, bytesEqual, concatBytes, fromHex, gunzip, gzip, hmac, hmacVerify, randomBytes,
  sha256, sha512, toHex, u32le, u64le, utf8Decode, utf8Encode,
} from './bytes.js';
import { argon2 } from './argon2.js';
import { ChaCha20 } from './chacha20.js';
import { KDBX_SALSA20_NONCE, Salsa20 } from './salsa20.js';
import { firstChild, parseXml, serializeXml, textOf } from './xml.js';
import { treeToXml, xmlToTree } from './keepass-xml.js';

const subtle = globalThis.crypto.subtle;

const SIG1 = 0x9aa2d903;
const SIG2 = 0xb54bfb67;
const SIG2_KDB = 0xb54bfb65; // KeePass 1.x

export const CIPHER_AES256 = '31c1f2e6bf714350be5805216afc5aff';
export const CIPHER_CHACHA20 = 'd6038a2b8b6f4cb5a524339a31dbb59a';
const CIPHER_TWOFISH = 'ad68f29f576f4bb9a36ad47af965346c';
export const KDF_AES = 'c9d9f39a628a4460bf740d08c18a4fea';
const KDF_AES_KEEPASSXC = '7c02bb8279a74ac0927d114a00648238';
export const KDF_ARGON2D = 'ef636ddf8c29444b91f7a9a403e30a0c';
export const KDF_ARGON2ID = '9e298b1956db4773b23dfc3ec6f0a1e6';

const H = { END: 0, COMMENT: 1, CIPHER: 2, COMPRESSION: 3, MASTER_SEED: 4, TRANSFORM_SEED: 5, TRANSFORM_ROUNDS: 6, IV: 7, PROTECTED_STREAM_KEY: 8, STREAM_START: 9, INNER_STREAM_ID: 10, KDF_PARAMS: 11, PUBLIC_CUSTOM_DATA: 12 };
const STREAM_SALSA20 = 2;
const STREAM_CHACHA20 = 3;
const BLOCK_SIZE = 1024 * 1024;
const HEADER_HMAC_INDEX = new Uint8Array(8).fill(0xff);

export const DEFAULT_KDBX_LIMITS = Object.freeze({
  maxFileBytes: 1024 * 1024 * 1024,
  maxHeaderBytes: 1024 * 1024,
  maxDecompressedBytes: 512 * 1024 * 1024,
  maxXmlChars: 256 * 1024 * 1024,
  maxArgon2MemoryBytes: 1024 * 1024 * 1024,
  maxArgon2Iterations: 100,
  maxArgon2Parallelism: 64,
  maxAesRounds: 200_000_000,
});

const corrupt = (msg) => new VaultError('corrupt', msg);

// --- binary readers ------------------------------------------------------------------------------

class Reader {
  constructor(bytes) {
    this.b = bytes;
    this.dv = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
    this.pos = 0;
  }
  need(n) {
    if (n < 0 || this.pos + n > this.b.length) throw corrupt('unexpected end of data');
  }
  u8() {
    this.need(1);
    return this.b[this.pos++];
  }
  u16() {
    this.need(2);
    const v = this.dv.getUint16(this.pos, true);
    this.pos += 2;
    return v;
  }
  u32() {
    this.need(4);
    const v = this.dv.getUint32(this.pos, true);
    this.pos += 4;
    return v;
  }
  i32() {
    this.need(4);
    const v = this.dv.getInt32(this.pos, true);
    this.pos += 4;
    return v;
  }
  bytes(n) {
    this.need(n);
    const v = this.b.subarray(this.pos, this.pos + n);
    this.pos += n;
    return v;
  }
}

function u64FromBytes(b) {
  if (b.length !== 8) throw corrupt('bad 64-bit field');
  const v = new DataView(b.buffer, b.byteOffset, 8).getBigUint64(0, true);
  if (v > BigInt(Number.MAX_SAFE_INTEGER)) throw new VaultError('limit_exceeded', '64-bit value too large');
  return Number(v);
}

// --- VariantDictionary (KDBX 4 KDF parameters / public custom data) ---------------------------------

const VD = { UINT32: 0x04, UINT64: 0x05, BOOL: 0x08, INT32: 0x0c, INT64: 0x0d, STRING: 0x18, BYTES: 0x42 };

/** @returns {Map<string, {type:number, value:any}>} */
export function readVariantDictionary(bytes) {
  const r = new Reader(bytes);
  const version = r.u16();
  if ((version & 0xff00) > 0x0100) throw new VaultError('unsupported', 'unsupported VariantDictionary version');
  const out = new Map();
  for (;;) {
    const type = r.u8();
    if (type === 0) break;
    const name = utf8Decode(r.bytes(r.i32()));
    const raw = r.bytes(r.i32());
    const dv = new DataView(raw.buffer, raw.byteOffset, raw.byteLength);
    let value;
    switch (type) {
      case VD.UINT32:
        if (raw.length !== 4) throw corrupt('bad UInt32 in VariantDictionary');
        value = dv.getUint32(0, true);
        break;
      case VD.UINT64:
        value = u64FromBytes(raw);
        break;
      case VD.BOOL:
        if (raw.length !== 1) throw corrupt('bad Bool in VariantDictionary');
        value = raw[0] !== 0;
        break;
      case VD.INT32:
        if (raw.length !== 4) throw corrupt('bad Int32 in VariantDictionary');
        value = dv.getInt32(0, true);
        break;
      case VD.INT64:
        if (raw.length !== 8) throw corrupt('bad Int64 in VariantDictionary');
        value = Number(dv.getBigInt64(0, true));
        break;
      case VD.STRING:
        value = utf8Decode(raw);
        break;
      case VD.BYTES:
        value = raw.slice();
        break;
      default:
        throw corrupt(`unknown VariantDictionary type 0x${type.toString(16)}`);
    }
    out.set(name, { type, value });
  }
  return out;
}

/** @param {Array<[string, number, any]>} entries [name, type, value] */
export function writeVariantDictionary(entries) {
  const parts = [new Uint8Array([0x00, 0x01])];
  for (const [name, type, value] of entries) {
    let raw;
    switch (type) {
      case VD.UINT32:
      case VD.INT32:
        raw = u32le(value);
        break;
      case VD.UINT64:
      case VD.INT64:
        raw = u64le(value);
        break;
      case VD.BOOL:
        raw = new Uint8Array([value ? 1 : 0]);
        break;
      case VD.STRING:
        raw = utf8Encode(value);
        break;
      case VD.BYTES:
        raw = value;
        break;
      default:
        throw invalidArg('unknown VariantDictionary type');
    }
    const nb = utf8Encode(name);
    parts.push(new Uint8Array([type]), u32le(nb.length), nb, u32le(raw.length), raw);
  }
  parts.push(new Uint8Array([0]));
  return concatBytes(...parts);
}

// --- keys ----------------------------------------------------------------------------------------

/**
 * Key-file → 32-byte key component, following KeePass: XML key file v1 (base64) / v2 (hex + hash),
 * else exactly 32 raw bytes, else 64 hex characters, else SHA-256 of the whole file.
 * @param {Uint8Array} fileBytes
 * @returns {Promise<Uint8Array>}
 */
export async function keyFileKey(fileBytes) {
  const bytes = asBytes(fileBytes, 'key file');
  if (bytes.length === 0) throw new VaultError('bad_keyfile', 'key file is empty');
  const xmlKey = await xmlKeyFile(bytes);
  if (xmlKey) return xmlKey;
  if (bytes.length === 32) return bytes.slice();
  if (bytes.length === 64) {
    const s = String.fromCharCode(...bytes);
    if (/^[0-9a-fA-F]{64}$/.test(s)) return fromHex(s);
  }
  return sha256(bytes);
}

async function xmlKeyFile(bytes) {
  if (bytes.length > 1024 * 1024) return null;
  let doc;
  try {
    doc = parseXml(utf8Decode(bytes), { maxLength: 1024 * 1024, maxDepth: 16, maxElements: 1000 });
  } catch {
    return null; // not XML → treat as binary key file, like KeePass
  }
  if (doc.name !== 'KeyFile') return null;
  const version = textOf(firstChild(firstChild(doc, 'Meta') || { children: [] }, 'Version')).trim();
  const data = firstChild(firstChild(doc, 'Key') || { children: [] }, 'Data');
  if (!data) throw new VaultError('bad_keyfile', 'XML key file has no Key/Data');
  if (/^1\./.test(version)) {
    try {
      return b64decode(textOf(data).replace(/\s+/g, ''));
    } catch (e) {
      throw new VaultError('bad_keyfile', 'XML key file data is not base64', { cause: e });
    }
  }
  if (/^2\./.test(version)) {
    let key;
    try {
      key = fromHex(textOf(data).replace(/\s+/g, ''));
    } catch (e) {
      throw new VaultError('bad_keyfile', 'XML key file data is not hex', { cause: e });
    }
    const want = (data.attrs.Hash || '').trim().toLowerCase();
    if (want) {
      const got = toHex((await sha256(key)).subarray(0, 4));
      if (got !== want) throw new VaultError('bad_keyfile', 'XML key file hash mismatch (file damaged?)');
    }
    return key;
  }
  throw new VaultError('bad_keyfile', `unsupported XML key file version "${version}"`);
}

/**
 * Generate a KeePass XML key file (v2.0: 32 random bytes as hex with a SHA-256 check hash).
 * @returns {Promise<Uint8Array>} UTF-8 file contents
 */
export async function generateKeyFile() {
  const key = randomBytes(32);
  const hex = toHex(key).toUpperCase();
  const hash = toHex((await sha256(key)).subarray(0, 4)).toUpperCase();
  const groups = hex.match(/.{8}/g);
  const xml =
    '<?xml version="1.0" encoding="utf-8"?>\n<KeyFile>\n\t<Meta>\n\t\t<Version>2.0</Version>\n\t</Meta>\n\t<Key>\n' +
    `\t\t<Data Hash="${hash}">\n\t\t\t${groups.slice(0, 4).join(' ')}\n\t\t\t${groups.slice(4).join(' ')}\n\t\t</Data>\n\t</Key>\n</KeyFile>\n`;
  key.fill(0);
  return utf8Encode(xml);
}

/**
 * Composite key = SHA-256(SHA-256(UTF-8 password) ‖ keyFileKey). The password is not normalised, matching
 * KeePass; `''` is a valid (empty) password, `undefined`/`null` means "no password component".
 */
export async function compositeKey(password, keyFile) {
  const parts = [];
  if (password !== undefined && password !== null) {
    if (typeof password !== 'string') throw invalidArg('password must be a string');
    parts.push(await sha256(utf8Encode(password)));
  }
  if (keyFile !== undefined && keyFile !== null) parts.push(await keyFileKey(keyFile));
  if (parts.length === 0) throw invalidArg('a password and/or key file is required');
  return sha256(concatBytes(...parts));
}

/**
 * AES-KDF: encrypt each 16-byte half of `key` `rounds` times with AES-256-ECB(seed), then SHA-256.
 * WebCrypto has no ECB, but CBC with IV = block over all-zero plaintext yields C_i = E^i(block), so the
 * last ciphertext block of an n-block encryption is n chained ECB rounds.
 */
async function aesKdf(key, seed, rounds) {
  if (seed.length !== 32) throw corrupt('AES-KDF seed must be 32 bytes');
  const k = await subtle.importKey('raw', seed, { name: 'AES-CBC' }, false, ['encrypt']);
  const chunkBlocks = 65536;
  const zeros = new Uint8Array(Math.min(chunkBlocks, Math.max(rounds, 1)) * 16);
  const chain = async (block) => {
    let iv = block;
    let left = rounds;
    while (left > 0) {
      const n = Math.min(chunkBlocks, left);
      const ct = new Uint8Array(await subtle.encrypt({ name: 'AES-CBC', iv }, k, zeros.subarray(0, n * 16)));
      iv = ct.slice((n - 1) * 16, n * 16);
      left -= n;
    }
    return iv;
  };
  const [a, b] = await Promise.all([chain(key.slice(0, 16)), chain(key.slice(16, 32))]);
  return sha256(concatBytes(a, b));
}

function kdfFromDict(dict, limits) {
  const uuid = dict.get('$UUID');
  if (!uuid || uuid.type !== VD.BYTES || uuid.value.length !== 16) throw corrupt('KDF parameters have no $UUID');
  const id = toHex(uuid.value);
  const get = (name, type) => {
    const v = dict.get(name);
    if (!v || (type !== undefined && v.type !== type)) throw corrupt(`KDF parameter ${name} missing or of wrong type`);
    return v.value;
  };
  if (id === KDF_AES || id === KDF_AES_KEEPASSXC) {
    const rounds = get('R', VD.UINT64);
    if (rounds > limits.maxAesRounds) throw new VaultError('limit_exceeded', `AES-KDF rounds ${rounds} exceed limit`);
    return { type: 'aes', rounds, seed: get('S', VD.BYTES) };
  }
  if (id === KDF_ARGON2D || id === KDF_ARGON2ID) {
    const memory = get('M', VD.UINT64);
    const iterations = get('I', VD.UINT64);
    const parallelism = get('P', VD.UINT32);
    const version = get('V', VD.UINT32);
    if (memory > limits.maxArgon2MemoryBytes) throw new VaultError('limit_exceeded', 'Argon2 memory exceeds limit');
    if (iterations > limits.maxArgon2Iterations) throw new VaultError('limit_exceeded', 'Argon2 iterations exceed limit');
    if (parallelism > limits.maxArgon2Parallelism) throw new VaultError('limit_exceeded', 'Argon2 parallelism exceeds limit');
    if (version !== 0x10 && version !== 0x13) throw new VaultError('unsupported', `Argon2 version 0x${version.toString(16)} not supported`);
    if (memory % 1024 !== 0 || memory / 1024 < 8 * parallelism || iterations < 1 || parallelism < 1) throw corrupt('invalid Argon2 parameters');
    const opt = (n) => (dict.has(n) ? get(n, VD.BYTES) : undefined);
    return { type: id === KDF_ARGON2D ? 'argon2d' : 'argon2id', memory, iterations, parallelism, version, salt: get('S', VD.BYTES), secret: opt('K'), ad: opt('A') };
  }
  throw new VaultError('unsupported', `unsupported KDF ${id}`);
}

async function transformKey(composite, kdf, onProgress) {
  if (kdf.type === 'aes') return aesKdf(composite, kdf.seed, kdf.rounds);
  return argon2({
    password: composite, salt: kdf.salt, secret: kdf.secret, ad: kdf.ad, memory: kdf.memory / 1024,
    iterations: kdf.iterations, parallelism: kdf.parallelism, version: kdf.version, type: kdf.type, hashLength: 32, onProgress,
  });
}

async function blockKey(hmacBase, index) {
  return sha512(concatBytes(index instanceof Uint8Array ? index : u64le(index), hmacBase));
}

// --- inner random stream ---------------------------------------------------------------------------

async function innerStream(id, key) {
  if (id === STREAM_CHACHA20) {
    const h = await sha512(key);
    return new ChaCha20(h.slice(0, 32), h.slice(32, 44));
  }
  if (id === STREAM_SALSA20) return new Salsa20(await sha256(key), KDBX_SALSA20_NONCE);
  if (id === 0) return { process: (b) => b };
  throw new VaultError('unsupported', `unsupported inner random stream ${id}`);
}

/** Visit elements in document order (iterative pre-order DFS). */
function* documentOrder(root) {
  const stack = [root];
  while (stack.length) {
    const n = stack.pop();
    yield n;
    for (let i = n.children.length - 1; i >= 0; i--) if (typeof n.children[i] === 'object') stack.push(n.children[i]);
  }
}

const isProtected = (n) => /^true$/i.test(n.attrs.Protected || '');

/** Decrypt Protected="True" values in document order; plaintext lands in node.protectedBytes. */
function unprotectXml(doc, stream) {
  for (const n of documentOrder(doc)) {
    if (!isProtected(n)) continue;
    let ct;
    try {
      ct = b64decode(textOf(n).replace(/\s+/g, ''));
    } catch (e) {
      throw new VaultError('corrupt', 'protected value is not base64', { cause: e });
    }
    n.protectedBytes = stream.process(ct);
    n.children = [];
  }
}

/** Encrypt Protected="True" values in document order (writer side). */
function protectXml(doc, stream) {
  for (const n of documentOrder(doc)) {
    if (!isProtected(n)) continue;
    const pt = n.protectedBytes || utf8Encode(textOf(n));
    n.children = [b64encode(stream.process(pt))];
    delete n.protectedBytes;
  }
}

// --- reading -------------------------------------------------------------------------------------

function parseHeader(bytes, limits) {
  const r = new Reader(bytes);
  if (bytes.length < 12) throw corrupt('file too short to be a KeePass database');
  const s1 = r.u32();
  const s2 = r.u32();
  if (s1 === SIG1 && s2 === SIG2_KDB) throw new VaultError('unsupported', 'KeePass 1.x (.kdb) databases are not supported');
  if (s1 !== SIG1 || s2 !== SIG2) throw corrupt('not a KeePass database (bad signature)');
  const minor = r.u16();
  const major = r.u16();
  if (major !== 3 && major !== 4) throw new VaultError('unsupported', `unsupported KDBX version ${major}.${minor}`);
  const fields = new Map();
  for (;;) {
    if (r.pos > limits.maxHeaderBytes) throw new VaultError('limit_exceeded', 'header too large');
    const id = r.u8();
    const size = major >= 4 ? r.i32() : r.u16();
    if (size < 0) throw corrupt('negative header field size');
    const data = r.bytes(size);
    if (id === H.END) break;
    fields.set(id, data);
  }
  return { major, minor, fields, headerEnd: r.pos };
}

function headerCipher(fields) {
  const c = fields.get(H.CIPHER);
  if (!c || c.length !== 16) throw corrupt('missing cipher id');
  const id = toHex(c);
  if (id === CIPHER_AES256) return 'aes';
  if (id === CIPHER_CHACHA20) return 'chacha20';
  if (id === CIPHER_TWOFISH) throw new VaultError('unsupported', 'Twofish-encrypted databases are not supported');
  throw new VaultError('unsupported', `unsupported cipher ${id}`);
}

function headerCompression(fields) {
  const c = fields.get(H.COMPRESSION);
  if (!c || c.length !== 4) throw corrupt('missing compression flags');
  const v = new DataView(c.buffer, c.byteOffset, 4).getUint32(0, true);
  if (v > 1) throw new VaultError('unsupported', `unsupported compression ${v}`);
  return v === 1;
}

async function decryptPayload(cipher, key, iv, data, wrongKeyCode) {
  if (cipher === 'aes') {
    if (iv.length !== 16) throw corrupt('AES IV must be 16 bytes');
    if (data.length === 0 || data.length % 16 !== 0) throw corrupt('ciphertext length is not a multiple of 16');
    const k = await subtle.importKey('raw', key, { name: 'AES-CBC' }, false, ['decrypt']);
    try {
      return new Uint8Array(await subtle.decrypt({ name: 'AES-CBC', iv }, k, data));
    } catch (e) {
      // Padding failure: wrong key for KDBX 3 (no MAC), corruption for KDBX 4 (MAC already verified).
      throw new VaultError(wrongKeyCode, wrongKeyCode === 'bad_credentials' ? 'wrong password or key file' : 'decryption failed', { cause: e });
    }
  }
  if (iv.length !== 12) throw corrupt('ChaCha20 IV must be 12 bytes');
  return new ChaCha20(key, iv).process(data);
}

// `xmlOpts.used` = decompressed payload bytes already charged to limits.maxDecompressedBytes: the payload and every
// attachment decoded from the XML share that one cap (gzip-compressed <Meta><Binaries> could otherwise each
// expand to the full limit).
async function decodeXml(xmlBytes, stream, limits, binaries, xmlOpts = {}) {
  let text;
  try {
    text = utf8Decode(xmlBytes);
  } catch (e) {
    throw new VaultError('corrupt', 'XML payload is not valid UTF-8', { cause: e });
  }
  const doc = parseXml(text, { maxLength: limits.maxXmlChars });
  unprotectXml(doc, stream);
  return xmlToTree(doc, {
    binaries,
    maxBinaryBytes: limits.maxDecompressedBytes,
    maxDecompressedBytes: limits.maxDecompressedBytes - (xmlOpts.used || 0),
    decodeBinaries: xmlOpts.decodeBinaries,
  });
}

async function readKdbx4(bytes, hdr, composite, limits, onProgress, decodeBinaries) {
  const { fields, headerEnd } = hdr;
  const headerBytes = bytes.subarray(0, headerEnd);
  if (bytes.length < headerEnd + 64) throw corrupt('file truncated after header');
  const storedHash = bytes.subarray(headerEnd, headerEnd + 32);
  const storedHmac = bytes.subarray(headerEnd + 32, headerEnd + 64);
  if (!bytesEqual(await sha256(headerBytes), storedHash)) throw corrupt('header checksum mismatch');
  const cipher = headerCipher(fields);
  const compression = headerCompression(fields);
  const seed = fields.get(H.MASTER_SEED);
  const iv = fields.get(H.IV);
  const kdfBytes = fields.get(H.KDF_PARAMS);
  if (!seed || seed.length !== 32 || !iv || !kdfBytes) throw corrupt('header is missing required fields');
  const kdf = kdfFromDict(readVariantDictionary(kdfBytes), limits);
  const transformed = await transformKey(composite, kdf, onProgress);
  const masterKey = await sha256(concatBytes(seed, transformed));
  const hmacBase = await sha512(concatBytes(seed, transformed, new Uint8Array([1])));
  transformed.fill(0);
  if (!(await hmacVerify('SHA-256', await blockKey(hmacBase, HEADER_HMAC_INDEX), storedHmac, headerBytes))) {
    throw new VaultError('bad_credentials', 'wrong password or key file');
  }
  // HMAC block stream
  const r = new Reader(bytes);
  r.pos = headerEnd + 64;
  const chunks = [];
  for (let index = 0; ; index++) {
    const mac = r.bytes(32);
    const size = r.i32();
    if (size < 0) throw corrupt('negative block size');
    const data = r.bytes(size);
    const ok = await hmacVerify('SHA-256', await blockKey(hmacBase, index), mac, concatBytes(u64le(index), u32le(size), data));
    if (!ok) throw corrupt(`block ${index} failed HMAC verification`);
    if (size === 0) break;
    chunks.push(data);
  }
  let plain = await decryptPayload(cipher, masterKey, iv, concatBytes(...chunks), 'corrupt');
  if (compression) plain = await gunzip(plain, limits.maxDecompressedBytes);
  // inner header
  const ir = new Reader(plain);
  let streamId = null;
  let streamKey = null;
  const binaries = [];
  for (;;) {
    const id = ir.u8();
    const size = ir.i32();
    if (size < 0) throw corrupt('negative inner header field size');
    const data = ir.bytes(size);
    if (id === 0) break;
    if (id === 1) {
      if (size !== 4) throw corrupt('bad inner stream id');
      streamId = new DataView(data.buffer, data.byteOffset, 4).getUint32(0, true);
    } else if (id === 2) {
      streamKey = data.slice();
    } else if (id === 3) {
      if (size < 1) throw corrupt('bad inner binary');
      const prot = (data[0] & 1) === 1;
      binaries.push(decodeBinaries === false ? { data: new Uint8Array(0), protected: prot, skipped: true } : { data: data.slice(1), protected: prot });
    }
  }
  if (streamId === null || !streamKey) throw corrupt('inner header is missing the random stream');
  const { tree } = await decodeXml(plain.subarray(ir.pos), await innerStream(streamId, streamKey), limits, binaries,
    { used: compression ? plain.length : 0, decodeBinaries });
  return {
    ...tree,
    version: { major: hdr.major, minor: hdr.minor },
    header: { cipher, compression, innerStream: streamName(streamId), kdf: publicKdf(kdf) },
  };
}

async function readKdbx3(bytes, hdr, composite, limits, decodeBinaries) {
  const { fields, headerEnd } = hdr;
  const cipher = headerCipher(fields);
  const compression = headerCompression(fields);
  const need = (id, len) => {
    const v = fields.get(id);
    if (!v || (len !== undefined && v.length !== len)) throw corrupt(`header field ${id} missing or invalid`);
    return v;
  };
  const seed = need(H.MASTER_SEED, 32);
  const transformSeed = need(H.TRANSFORM_SEED, 32);
  const rounds = u64FromBytes(need(H.TRANSFORM_ROUNDS, 8));
  if (rounds > limits.maxAesRounds) throw new VaultError('limit_exceeded', `AES-KDF rounds ${rounds} exceed limit`);
  const iv = need(H.IV);
  const streamKey = need(H.PROTECTED_STREAM_KEY);
  const startBytes = need(H.STREAM_START, 32);
  const sid = need(H.INNER_STREAM_ID, 4);
  const streamId = new DataView(sid.buffer, sid.byteOffset, 4).getUint32(0, true);
  const transformed = await aesKdf(composite, transformSeed, rounds);
  const masterKey = await sha256(concatBytes(seed, transformed));
  transformed.fill(0);
  const plain = await decryptPayload(cipher, masterKey, iv, bytes.subarray(headerEnd), 'bad_credentials');
  if (plain.length < 32 || !bytesEqual(plain.subarray(0, 32), startBytes)) throw new VaultError('bad_credentials', 'wrong password or key file');
  // hashed block stream
  const r = new Reader(plain);
  r.pos = 32;
  const chunks = [];
  for (let index = 0; ; index++) {
    const blockIndex = r.u32();
    if (blockIndex !== index) throw corrupt('hashed block stream out of order');
    const hash = r.bytes(32);
    const size = r.i32();
    if (size < 0) throw corrupt('negative block size');
    if (size === 0) {
      if (hash.some((x) => x !== 0)) throw corrupt('final block hash is not zero');
      break;
    }
    const data = r.bytes(size);
    if (!bytesEqual(await sha256(data), hash)) throw corrupt(`block ${index} failed hash verification`);
    chunks.push(data);
  }
  let xmlBytes = concatBytes(...chunks);
  if (compression) xmlBytes = await gunzip(xmlBytes, limits.maxDecompressedBytes);
  const { tree, headerHash } = await decodeXml(xmlBytes, await innerStream(streamId, streamKey), limits, undefined,
    { used: compression ? xmlBytes.length : 0, decodeBinaries });
  if (headerHash) {
    const actual = b64encode(await sha256(bytes.subarray(0, headerEnd)));
    if (actual !== headerHash) throw corrupt('header hash in XML does not match the file header');
  }
  return {
    ...tree,
    version: { major: hdr.major, minor: hdr.minor },
    header: { cipher, compression, innerStream: streamName(streamId), kdf: { type: 'aes', rounds } },
  };
}

function streamName(id) {
  return id === STREAM_CHACHA20 ? 'chacha20' : id === STREAM_SALSA20 ? 'salsa20' : 'none';
}

function publicKdf(kdf) {
  if (kdf.type === 'aes') return { type: 'aes', rounds: kdf.rounds };
  return { type: kdf.type, memory: kdf.memory, iterations: kdf.iterations, parallelism: kdf.parallelism, version: kdf.version };
}

/**
 * Read a KDBX 3.1 / 4.x database.
 * @param {Uint8Array} bytes
 * @param {{password?: string, keyFile?: Uint8Array, limits?: Partial<typeof DEFAULT_KDBX_LIMITS>,
 *          onProgress?: (fraction:number)=>void, decodeBinaries?: boolean}} opts
 *   `limits.maxDecompressedBytes` caps the decompressed payload and every attachment decoded from the XML
 *   together. `decodeBinaries: false` skips attachment data entirely (entries still reference them; each
 *   tree.binaries element is then `{data: empty, protected, skipped: true}`) — used by the vault import,
 *   which drops attachments.
 * @returns {Promise<object>} KdbxTree + {version, header}
 */
export async function readKdbx(bytes, opts = {}) {
  const data = asBytes(bytes, 'KDBX data');
  const limits = { ...DEFAULT_KDBX_LIMITS, ...(opts.limits || {}) };
  if (data.length > limits.maxFileBytes) throw new VaultError('limit_exceeded', 'file too large');
  const hdr = parseHeader(data, limits);
  const composite = await compositeKey(opts.password, opts.keyFile);
  try {
    return hdr.major === 4
      ? await readKdbx4(data, hdr, composite, limits, opts.onProgress, opts.decodeBinaries)
      : await readKdbx3(data, hdr, composite, limits, opts.decodeBinaries);
  } finally {
    composite.fill(0);
  }
}

// --- writing -------------------------------------------------------------------------------------

/** True when the tree uses fields that only exist in KDBX 4.1. */
export function requiresKdbx41(tree) {
  const meta = tree.meta || {};
  if ((meta.customIcons || []).some((i) => i.name !== undefined || i.lastModified)) return true;
  if ((meta.customData || []).some((i) => i.lastModified)) return true;
  const entry41 = (e) => e.qualityCheck === false || !!e.previousParentGroup || (e.customData || []).some((i) => i.lastModified);
  const walk = (g) =>
    (g.tags && g.tags.length > 0) || !!g.previousParentGroup || (g.customData || []).some((i) => i.lastModified) ||
    (g.entries || []).some((e) => entry41(e) || (e.history || []).some(entry41)) || (g.groups || []).some(walk);
  return walk(tree.root);
}

function kdfSettings(kdf = {}) {
  const type = kdf.type || 'argon2d';
  if (type === 'aes') {
    const rounds = kdf.rounds ?? 2_000_000;
    if (!Number.isSafeInteger(rounds) || rounds < 1) throw invalidArg('AES-KDF rounds must be a positive integer');
    return { type, rounds, seed: randomBytes(32) };
  }
  if (type !== 'argon2d' && type !== 'argon2id') throw invalidArg(`unknown KDF type "${type}"`);
  const memory = kdf.memory ?? 64 * 1024 * 1024;
  const iterations = kdf.iterations ?? 2;
  const parallelism = kdf.parallelism ?? 2;
  if (!Number.isSafeInteger(memory) || memory % 1024 !== 0 || memory / 1024 < 8 * parallelism) {
    throw invalidArg('Argon2 memory must be a multiple of 1024 bytes and ≥ 8 KiB × parallelism');
  }
  if (!Number.isInteger(iterations) || iterations < 1) throw invalidArg('Argon2 iterations must be ≥ 1');
  if (!Number.isInteger(parallelism) || parallelism < 1 || parallelism > 0xffffff) throw invalidArg('Argon2 parallelism out of range');
  return { type, memory, iterations, parallelism, version: 0x13, salt: randomBytes(32) };
}

function kdfDictEntries(kdf) {
  if (kdf.type === 'aes') {
    return [['$UUID', VD.BYTES, fromHex(KDF_AES)], ['R', VD.UINT64, kdf.rounds], ['S', VD.BYTES, kdf.seed]];
  }
  return [
    ['$UUID', VD.BYTES, fromHex(kdf.type === 'argon2d' ? KDF_ARGON2D : KDF_ARGON2ID)],
    ['S', VD.BYTES, kdf.salt],
    ['P', VD.UINT32, kdf.parallelism],
    ['M', VD.UINT64, kdf.memory],
    ['I', VD.UINT64, kdf.iterations],
    ['V', VD.UINT32, kdf.version],
  ];
}

function headerField(id, data) {
  return concatBytes(new Uint8Array([id]), u32le(data.length), data);
}

/**
 * Write a KDBX 4 database (defaults: AES-256-CBC, Argon2d 64 MiB / 2 iterations / 2 lanes, gzip,
 * ChaCha20 inner stream).
 * @param {object} tree KdbxTree (see keepass-xml.js)
 * @param {{password?: string, keyFile?: Uint8Array, cipher?: 'aes'|'chacha20', compression?: boolean,
 *          kdf?: {type:'argon2d'|'argon2id', memory?: number /* bytes *\/, iterations?: number, parallelism?: number}
 *             | {type:'aes', rounds?: number}, onProgress?: (fraction:number)=>void}} opts
 * @returns {Promise<Uint8Array>}
 */
export async function writeKdbx(tree, opts = {}) {
  if (!tree || !tree.root) throw invalidArg('tree has no root group');
  const cipher = opts.cipher || 'aes';
  if (cipher !== 'aes' && cipher !== 'chacha20') throw invalidArg('cipher must be "aes" or "chacha20"');
  const compression = opts.compression !== false;
  const kdf = kdfSettings(opts.kdf);
  const composite = await compositeKey(opts.password, opts.keyFile);

  const seed = randomBytes(32);
  const iv = randomBytes(cipher === 'aes' ? 16 : 12);
  const minor = requiresKdbx41(tree) ? 1 : 0;
  const sig = new Uint8Array(12);
  const dv = new DataView(sig.buffer);
  dv.setUint32(0, SIG1, true);
  dv.setUint32(4, SIG2, true);
  dv.setUint16(8, minor, true);
  dv.setUint16(10, 4, true);
  const compressionFlags = u32le(compression ? 1 : 0);
  const header = concatBytes(
    sig,
    headerField(H.CIPHER, fromHex(cipher === 'aes' ? CIPHER_AES256 : CIPHER_CHACHA20)),
    headerField(H.COMPRESSION, compressionFlags),
    headerField(H.MASTER_SEED, seed),
    headerField(H.IV, iv),
    headerField(H.KDF_PARAMS, writeVariantDictionary(kdfDictEntries(kdf))),
    headerField(H.END, utf8Encode('\r\n\r\n')),
  );

  const transformed = await transformKey(composite, kdf, opts.onProgress);
  composite.fill(0);
  const masterKey = await sha256(concatBytes(seed, transformed));
  const hmacBase = await sha512(concatBytes(seed, transformed, new Uint8Array([1])));
  transformed.fill(0);
  const headerHash = await sha256(header);
  const headerHmac = await hmac('SHA-256', await blockKey(hmacBase, HEADER_HMAC_INDEX), header);

  // inner header + XML
  const streamKey = randomBytes(64);
  const binaries = tree.binaries || [];
  const inner = [headerField(1, u32le(STREAM_CHACHA20)), headerField(2, streamKey)];
  for (const b of binaries) inner.push(headerField(3, concatBytes(new Uint8Array([b.protected ? 1 : 0]), asBytes(b.data, 'binary data'))));
  inner.push(headerField(0, new Uint8Array(0)));
  const doc = treeToXml({ ...tree, meta: { ...(tree.meta || {}), generator: 'Ludworks' } }, { mode: 'kdbx4' });
  protectXml(doc, await innerStream(STREAM_CHACHA20, streamKey));
  let plain = concatBytes(...inner, utf8Encode(serializeXml(doc)));
  if (compression) plain = await gzip(plain);

  let encrypted;
  if (cipher === 'aes') {
    const k = await subtle.importKey('raw', masterKey, { name: 'AES-CBC' }, false, ['encrypt']);
    encrypted = new Uint8Array(await subtle.encrypt({ name: 'AES-CBC', iv }, k, plain));
  } else {
    encrypted = new ChaCha20(masterKey, iv).process(plain);
  }
  masterKey.fill(0);

  const out = [header, headerHash, headerHmac];
  let index = 0;
  for (let off = 0; off <= encrypted.length; off += BLOCK_SIZE, index++) {
    const data = encrypted.subarray(off, Math.min(off + BLOCK_SIZE, encrypted.length));
    if (data.length === 0) break;
    const mac = await hmac('SHA-256', await blockKey(hmacBase, index), concatBytes(u64le(index), u32le(data.length), data));
    out.push(mac, u32le(data.length), data);
  }
  const endMac = await hmac('SHA-256', await blockKey(hmacBase, index), concatBytes(u64le(index), u32le(0)));
  out.push(endMac, u32le(0));
  return concatBytes(...out);
}

/** Internals for the test-only KDBX 3.1 writer. Not part of the public API. */
export const __testing = Object.freeze({ aesKdf, protectXml, innerStream, STREAM_SALSA20, CIPHER_AES256 });
