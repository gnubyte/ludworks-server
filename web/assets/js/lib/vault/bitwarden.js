/**
 * Bitwarden import/export: unencrypted JSON, password-protected JSON and CSV.
 *
 * Functions here work on Bitwarden-shaped data ({encrypted:false, folders:[{id,name}], items:[...]});
 * convert.js maps that to/from the Ludworks vault interchange shape.
 *
 * Password-protected JSON (Bitwarden "file password" export):
 *   salt string S (base64 text, used as UTF-8 bytes)
 *   kdfType 0: key = PBKDF2-SHA256(UTF-8 password, UTF-8 S, kdfIterations, 32)
 *   kdfType 1: key = Argon2id(UTF-8 password, SHA-256(UTF-8 S), t=kdfIterations, m=kdfMemory MiB,
 *              p=kdfParallelism, 32)
 *   encKey = HKDF-Expand(key, "enc", 32), macKey = HKDF-Expand(key, "mac", 32)  (SHA-256, no extract)
 *   EncString type 2 = "2." + b64(iv) + "|" + b64(AES-256-CBC(encKey, iv, pt)) + "|" + b64(HMAC-SHA256(macKey, iv‖ct))
 *   encKeyValidation_DO_NOT_EDIT = EncString of a random GUID (password check); data = EncString of the
 *   unencrypted JSON export.
 */
import { VaultError, invalidArg } from './errors.js';
import { argon2 } from './argon2.js';
import { b64decode, b64encode, concatBytes, hmac, hmacVerify, randomBytes, sha256, utf8Decode, utf8Encode } from './bytes.js';
import { uuid } from './crypto.js';

const subtle = globalThis.crypto.subtle;
const MAX_INPUT_CHARS = 128 * 1024 * 1024;

// --- crypto --------------------------------------------------------------------------------------

function checkKdf(kdf) {
  if (kdf.kdfType === 0) {
    if (!Number.isInteger(kdf.kdfIterations) || kdf.kdfIterations < 5000 || kdf.kdfIterations > 10_000_000) {
      throw new VaultError('unsupported', 'PBKDF2 iterations out of the supported range (5000..10000000)');
    }
  } else if (kdf.kdfType === 1) {
    const { kdfIterations: t, kdfMemory: m, kdfParallelism: p } = kdf;
    if (!Number.isInteger(t) || t < 1 || t > 10) throw new VaultError('unsupported', 'Argon2 iterations out of range (1..10)');
    if (!Number.isInteger(m) || m < 16 || m > 1024) throw new VaultError('unsupported', 'Argon2 memory out of range (16..1024 MiB)');
    if (!Number.isInteger(p) || p < 1 || p > 16) throw new VaultError('unsupported', 'Argon2 parallelism out of range (1..16)');
  } else {
    throw new VaultError('unsupported', `unsupported Bitwarden kdfType ${kdf.kdfType}`);
  }
}

async function deriveKeys(password, saltString, kdf) {
  checkKdf(kdf);
  const pw = utf8Encode(password);
  const salt = utf8Encode(saltString);
  let master;
  if (kdf.kdfType === 0) {
    const base = await subtle.importKey('raw', pw, 'PBKDF2', false, ['deriveBits']);
    master = new Uint8Array(await subtle.deriveBits({ name: 'PBKDF2', hash: 'SHA-256', salt, iterations: kdf.kdfIterations }, base, 256));
  } else {
    master = await argon2({
      password: pw, salt: await sha256(salt), iterations: kdf.kdfIterations, memory: kdf.kdfMemory * 1024,
      parallelism: kdf.kdfParallelism, hashLength: 32, type: 'argon2id', version: 0x13,
    });
  }
  pw.fill(0);
  // HKDF-Expand with a single output block: T(1) = HMAC(PRK, info ‖ 0x01).
  const encKey = await hmac('SHA-256', master, utf8Encode('enc\x01'));
  const macKey = await hmac('SHA-256', master, utf8Encode('mac\x01'));
  master.fill(0);
  return { encKey, macKey };
}

async function decryptEncString(str, keys) {
  if (typeof str !== 'string') throw new VaultError('malformed', 'EncString must be a string');
  const m = /^(\d+)\.([^|]+)\|([^|]+)(?:\|([^|]+))?$/.exec(str.trim());
  if (!m) throw new VaultError('malformed', 'invalid EncString');
  if (m[1] !== '2' || !m[4]) throw new VaultError('unsupported', `unsupported EncString type ${m[1]} (only type 2 AES-CBC-256 + HMAC-SHA256)`);
  const iv = b64decode(m[2]);
  const ct = b64decode(m[3]);
  const mac = b64decode(m[4]);
  if (iv.length !== 16 || mac.length !== 32 || ct.length === 0 || ct.length % 16) throw new VaultError('malformed', 'invalid EncString lengths');
  if (!(await hmacVerify('SHA-256', keys.macKey, mac, concatBytes(iv, ct)))) throw new VaultError('decrypt_failed', 'EncString MAC mismatch');
  const k = await subtle.importKey('raw', keys.encKey, { name: 'AES-CBC' }, false, ['decrypt']);
  try {
    return new Uint8Array(await subtle.decrypt({ name: 'AES-CBC', iv }, k, ct));
  } catch (e) {
    throw new VaultError('decrypt_failed', 'EncString decryption failed', { cause: e });
  }
}

async function encryptEncString(plaintext, keys) {
  const iv = randomBytes(16);
  const k = await subtle.importKey('raw', keys.encKey, { name: 'AES-CBC' }, false, ['encrypt']);
  const ct = new Uint8Array(await subtle.encrypt({ name: 'AES-CBC', iv }, k, plaintext));
  const mac = await hmac('SHA-256', keys.macKey, concatBytes(iv, ct));
  return `2.${b64encode(iv)}|${b64encode(ct)}|${b64encode(mac)}`;
}

// --- JSON ----------------------------------------------------------------------------------------

function parseJsonText(text) {
  if (typeof text !== 'string') throw invalidArg('Bitwarden export must be text');
  if (text.length > MAX_INPUT_CHARS) throw new VaultError('limit_exceeded', 'export file too large');
  try {
    return JSON.parse(text.charCodeAt(0) === 0xfeff ? text.slice(1) : text);
  } catch (e) {
    throw new VaultError('malformed', 'not valid JSON', { cause: e });
  }
}

function normalizeExport(doc) {
  if (!doc || typeof doc !== 'object' || !Array.isArray(doc.items)) throw new VaultError('malformed', 'not a Bitwarden JSON export (no items array)');
  const folders = Array.isArray(doc.folders) ? doc.folders.filter((f) => f && typeof f.name === 'string') : [];
  return { encrypted: false, folders, items: doc.items.filter((i) => i && typeof i === 'object') };
}

/**
 * Parse a Bitwarden JSON export. Password-protected exports need `password` (the file password);
 * a wrong password throws VaultError('bad_password'); a missing one 'password_required'.
 * Account-restricted encrypted exports cannot be decrypted outside Bitwarden ('unsupported').
 * @param {string} text
 * @param {{password?: string}} [opts]
 * @returns {Promise<{encrypted:false, folders:Array<{id:string,name:string}>, items:Array<object>}>}
 */
export async function parseBitwardenJson(text, { password } = {}) {
  const doc = parseJsonText(text);
  if (doc && doc.encrypted === true) {
    if (doc.passwordProtected !== true) {
      throw new VaultError('unsupported', 'account-restricted encrypted Bitwarden exports can only be opened by Bitwarden; export with a file password or unencrypted');
    }
    if (typeof password !== 'string' || password === '') throw new VaultError('password_required', 'this export is password protected');
    if (typeof doc.salt !== 'string' || !doc.salt) throw new VaultError('malformed', 'encrypted export has no salt');
    const keys = await deriveKeys(password, doc.salt, {
      kdfType: doc.kdfType ?? 0, kdfIterations: doc.kdfIterations, kdfMemory: doc.kdfMemory, kdfParallelism: doc.kdfParallelism,
    });
    try {
      await decryptEncString(doc.encKeyValidation_DO_NOT_EDIT, keys);
    } catch (e) {
      if (e instanceof VaultError && e.code === 'decrypt_failed') throw new VaultError('bad_password', 'wrong export password', { cause: e });
      throw e;
    }
    const inner = parseJsonText(utf8Decode(await decryptEncString(doc.data, keys)));
    if (inner && inner.encrypted === true) throw new VaultError('malformed', 'nested encrypted export');
    return normalizeExport(inner);
  }
  return normalizeExport(doc);
}

/**
 * Serialize Bitwarden-shaped data as a JSON export. With `password`, produce a password-protected export.
 * @param {{folders?:Array<object>, items:Array<object>}} data
 * @param {{password?: string, kdfType?: 0|1, kdfIterations?: number, kdfMemory?: number, kdfParallelism?: number}} [opts]
 * @returns {Promise<string>}
 */
export async function toBitwardenJson(data, opts = {}) {
  const clear = JSON.stringify({ encrypted: false, folders: data.folders || [], items: data.items || [] }, null, 2);
  if (opts.password === undefined || opts.password === null) return clear;
  if (typeof opts.password !== 'string' || opts.password === '') throw invalidArg('export password must be a non-empty string');
  const kdf =
    (opts.kdfType ?? 0) === 1
      ? { kdfType: 1, kdfIterations: opts.kdfIterations ?? 3, kdfMemory: opts.kdfMemory ?? 64, kdfParallelism: opts.kdfParallelism ?? 4 }
      : { kdfType: 0, kdfIterations: opts.kdfIterations ?? 600000 };
  const salt = b64encode(randomBytes(16));
  const keys = await deriveKeys(opts.password, salt, kdf);
  const out = {
    encrypted: true,
    passwordProtected: true,
    salt,
    kdfType: kdf.kdfType,
    kdfIterations: kdf.kdfIterations,
    kdfMemory: kdf.kdfMemory ?? null,
    kdfParallelism: kdf.kdfParallelism ?? null,
    encKeyValidation_DO_NOT_EDIT: await encryptEncString(utf8Encode(uuid()), keys),
    data: await encryptEncString(utf8Encode(clear), keys),
  };
  return JSON.stringify(out, null, 2);
}

// --- CSV -----------------------------------------------------------------------------------------

/**
 * RFC 4180 CSV parser (quoted fields, "" escapes, CRLF/LF/CR line breaks, optional BOM).
 * @param {string} text
 * @returns {string[][]}
 */
export function parseCsv(text) {
  if (typeof text !== 'string') throw invalidArg('CSV must be text');
  if (text.length > MAX_INPUT_CHARS) throw new VaultError('limit_exceeded', 'CSV file too large');
  const s = text.charCodeAt(0) === 0xfeff ? text.slice(1) : text;
  const rows = [];
  let row = [];
  let field = '';
  let i = 0;
  let quoted = false;
  let fieldStarted = false;
  while (i < s.length) {
    const c = s[i];
    if (quoted) {
      if (c === '"') {
        if (s[i + 1] === '"') {
          field += '"';
          i += 2;
          continue;
        }
        quoted = false;
        i++;
        continue;
      }
      field += c;
      i++;
      continue;
    }
    if (c === '"' && !fieldStarted) {
      quoted = true;
      fieldStarted = true;
      i++;
    } else if (c === ',') {
      row.push(field);
      field = '';
      fieldStarted = false;
      i++;
    } else if (c === '\r' || c === '\n') {
      row.push(field);
      rows.push(row);
      row = [];
      field = '';
      fieldStarted = false;
      i += c === '\r' && s[i + 1] === '\n' ? 2 : 1;
    } else {
      field += c;
      fieldStarted = true;
      i++;
    }
  }
  if (quoted) throw new VaultError('malformed', 'unterminated quoted CSV field');
  if (fieldStarted || field !== '' || row.length) {
    row.push(field);
    rows.push(row);
  }
  return rows.filter((r) => !(r.length === 1 && r[0] === ''));
}

function csvField(v) {
  const s = v === null || v === undefined ? '' : String(v);
  return /[",\r\n]|^\s|\s$/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
}

/** @param {string[][]} rows */
export function toCsv(rows) {
  return rows.map((r) => r.map(csvField).join(',')).join('\r\n') + '\r\n';
}

const CSV_HEADER = ['folder', 'favorite', 'type', 'name', 'notes', 'fields', 'reprompt', 'login_uri', 'login_username', 'login_password', 'login_totp'];

/**
 * Parse a Bitwarden CSV export into Bitwarden-shaped data (logins and secure notes).
 * @param {string} text
 * @returns {{encrypted:false, folders:Array<{id:string,name:string}>, items:Array<object>}}
 */
export function parseBitwardenCsv(text) {
  const rows = parseCsv(text);
  if (rows.length === 0) return { encrypted: false, folders: [], items: [] };
  const header = rows[0].map((h) => h.trim().toLowerCase());
  if (!header.includes('name')) throw new VaultError('malformed', 'CSV header has no "name" column');
  const col = (r, name) => {
    const i = header.indexOf(name);
    return i >= 0 && i < r.length ? r[i] : '';
  };
  const folders = new Map();
  const items = [];
  for (const r of rows.slice(1)) {
    const folderName = col(r, 'folder').trim();
    let folderId = null;
    if (folderName) {
      if (!folders.has(folderName)) folders.set(folderName, uuid());
      folderId = folders.get(folderName);
    }
    const fields = [];
    for (const line of col(r, 'fields').split(/\r\n|\r|\n/)) {
      if (!line.trim()) continue;
      const d = line.lastIndexOf(': ');
      if (d < 0) continue;
      fields.push({ name: line.slice(0, d), value: line.slice(d + 2), type: 0, linkedId: null });
    }
    const isNote = col(r, 'type').trim().toLowerCase() === 'note';
    const item = {
      id: uuid(),
      folderId,
      type: isNote ? 2 : 1,
      reprompt: col(r, 'reprompt').trim() === '1' ? 1 : 0,
      name: col(r, 'name'),
      notes: col(r, 'notes') || null,
      favorite: col(r, 'favorite').trim() === '1',
      fields,
    };
    if (isNote) {
      item.secureNote = { type: 0 };
    } else {
      const uriCell = col(r, 'login_uri');
      const uris = uriCell.trim() ? (parseCsv(uriCell)[0] || []).map((u) => u.trim()).filter(Boolean) : [];
      item.login = {
        uris: uris.map((uri) => ({ match: null, uri })),
        username: col(r, 'login_username') || null,
        password: col(r, 'login_password') || null,
        totp: col(r, 'login_totp') || null,
      };
    }
    items.push(item);
  }
  return { encrypted: false, folders: [...folders].map(([name, id]) => ({ id, name })), items };
}

const CARD_LABELS = [['cardholderName', 'Cardholder name'], ['brand', 'Brand'], ['number', 'Number'], ['expMonth', 'Expiration month'], ['expYear', 'Expiration year'], ['code', 'Security code']];
const IDENTITY_LABELS = [
  ['title', 'Title'], ['firstName', 'First name'], ['middleName', 'Middle name'], ['lastName', 'Last name'], ['company', 'Company'],
  ['email', 'Email'], ['phone', 'Phone'], ['address1', 'Address 1'], ['address2', 'Address 2'], ['address3', 'Address 3'],
  ['city', 'City'], ['state', 'State'], ['postalCode', 'Postal code'], ['country', 'Country'], ['username', 'Username'],
  ['ssn', 'SSN'], ['passportNumber', 'Passport number'], ['licenseNumber', 'License number'],
];

/**
 * Serialize Bitwarden-shaped data as Bitwarden CSV. The CSV format only has logins and notes, so cards
 * and identities are written as notes whose properties become "name: value" fields (nothing is dropped).
 * @param {{folders?:Array<{id,name}>, items:Array<object>}} data
 * @returns {string}
 */
export function toBitwardenCsv(data) {
  const folderName = new Map((data.folders || []).map((f) => [f.id, f.name]));
  const rows = [CSV_HEADER];
  for (const it of data.items || []) {
    const fields = (it.fields || []).filter((f) => f.type !== 3).map((f) => ({ name: f.name ?? '', value: f.value ?? '' }));
    const extra = it.type === 3 ? [it.card, CARD_LABELS] : it.type === 4 ? [it.identity, IDENTITY_LABELS] : null;
    if (extra && extra[0]) for (const [k, label] of extra[1]) if (extra[0][k]) fields.unshift({ name: label, value: extra[0][k] });
    const isLogin = it.type === 1;
    const uris = isLogin && it.login && it.login.uris ? it.login.uris.map((u) => u.uri).filter(Boolean) : [];
    rows.push([
      it.folderId ? folderName.get(it.folderId) || '' : '',
      it.favorite ? '1' : '',
      isLogin ? 'login' : 'note',
      it.name ?? '',
      it.notes ?? '',
      fields.map((f) => `${f.name}: ${String(f.value).replace(/\r?\n/g, ' ')}`).join('\n'),
      it.reprompt ? '1' : '0',
      uris.map(csvField).join(','),
      isLogin && it.login ? (it.login.username ?? '') : '',
      isLogin && it.login ? (it.login.password ?? '') : '',
      isLogin && it.login ? (it.login.totp ?? '') : '',
    ]);
  }
  return toCsv(rows);
}

/** Internals for tests. */
export const __testing = Object.freeze({ deriveKeys, decryptEncString, encryptEncString });
