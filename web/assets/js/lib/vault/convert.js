/**
 * Conversions between external password-manager formats and the Ludworks vault interchange shape:
 *
 *   VaultData = {
 *     folders: [{ id: uuid, name: 'Work/Email' }],            // full path, '/'-separated
 *     items:   [{ folderPath: 'Work/Email' | '',              // '' = no folder
 *                 ...item plaintext per VAULT-CRYPTO.md §3 (type, name, favorite, notes, login, card,
 *                 identity, fields, password_history, created_at, updated_at) }],
 *     warnings: string[]                                       // import only: things that were not carried over
 *   }
 *
 * Before encrypting an item for the server, strip `folderPath` and resolve it to a folder id.
 * Optional extra item key written by imports: `tags: string[]` (KeePass tags; preserved per §3's
 * unknown-key rule). KeePass has no favourites, so favourite ⇄ the tag "Favorite".
 */
import { invalidArg, VaultError } from './errors.js';
import { asBytes, b64decode, fromHex, utf8Decode, utf8Encode } from './bytes.js';
import { uuid } from './crypto.js';
import { base32decode, base32encode, buildOtpauth, parseOtpauth } from './totp.js';
import { newEntry, newGroup, newTimes, newTree, nowIso, parseKeePassXml, serializeKeePassXml } from './keepass-xml.js';
import { readKdbx, writeKdbx } from './kdbx.js';
import { parseBitwardenCsv, parseBitwardenJson, toBitwardenCsv, toBitwardenJson } from './bitwarden.js';

const STANDARD = ['Title', 'UserName', 'Password', 'URL', 'Notes'];
const TYPE_KEY = 'Ludworks.Type';
const ITEM_TYPES = new Set(['login', 'note', 'card', 'identity']);
const URL_FIELD_RE = /^KP2A_URL(_\d+)?$/;

// [vault key, KeePass string name, protected]
const CARD_KP = [
  ['cardholder', 'Card Holder', false], ['brand', 'Card Brand', false], ['number', 'Card Number', true],
  ['exp_month', 'Card Expiration Month', false], ['exp_year', 'Card Expiration Year', false], ['code', 'Card Security Code', true],
];
const IDENTITY_KP = [
  ['title', 'Identity Title', false], ['first_name', 'First Name', false], ['middle_name', 'Middle Name', false],
  ['last_name', 'Last Name', false], ['company', 'Company', false], ['email', 'Email', false], ['phone', 'Phone', false],
  ['address1', 'Address 1', false], ['address2', 'Address 2', false], ['city', 'City', false], ['state', 'State', false],
  ['postal_code', 'Postal Code', false], ['country', 'Country', false], ['username', 'Identity Username', false],
  ['ssn', 'SSN', true], ['passport', 'Passport', true], ['license', 'License', true],
];
// [vault key, Bitwarden key]
const CARD_BW = [['cardholder', 'cardholderName'], ['brand', 'brand'], ['number', 'number'], ['exp_month', 'expMonth'], ['exp_year', 'expYear'], ['code', 'code']];
const IDENTITY_BW = [
  ['title', 'title'], ['first_name', 'firstName'], ['middle_name', 'middleName'], ['last_name', 'lastName'], ['company', 'company'],
  ['email', 'email'], ['phone', 'phone'], ['address1', 'address1'], ['address2', 'address2'], ['city', 'city'], ['state', 'state'],
  ['postal_code', 'postalCode'], ['country', 'country'], ['username', 'username'], ['ssn', 'ssn'], ['passport', 'passportNumber'],
  ['license', 'licenseNumber'],
];
const BW_MATCH = ['domain', 'host', 'starts_with', 'exact', 'regex', 'never'];

/** Normalise a folder path: trim segments, drop empty ones. */
export function normalizeFolderPath(p) {
  return String(p ?? '')
    .split('/')
    .map((s) => s.trim())
    .filter(Boolean)
    .join('/');
}

function emptyCard() {
  return Object.fromEntries(CARD_KP.map(([k]) => [k, '']));
}
function emptyIdentity() {
  return Object.fromEntries(IDENTITY_KP.map(([k]) => [k, '']));
}

// --- TOTP field conventions ------------------------------------------------------------------------

function isBase32(s) {
  try {
    base32decode(s);
    return true;
  } catch {
    return false;
  }
}

function otpAlgorithm(v) {
  const a = String(v || '').toUpperCase().replace(/[^A-Z0-9]/g, '');
  return a.endsWith('512') ? 'SHA512' : a.endsWith('256') ? 'SHA256' : 'SHA1';
}

/**
 * Find a TOTP configuration in KeePass string fields. Supports KeePassXC/KeePassDX `otp` (otpauth URI),
 * the KeeOtp plugin `otp` format (key=...&size=...&step=...), KeePass 2.47+ `TimeOtp-*` fields and
 * KeePassXC legacy `TOTP Seed`/`TOTP Settings`.
 * @returns {{totp: string|null, consumed: string[]}}
 */
export function extractKeePassTotp(fieldMap, issuer, account) {
  const get = (k) => (fieldMap.has(k) ? fieldMap.get(k).value.trim() : '');
  const otp = get('otp');
  if (otp) {
    if (/^(otpauth|steam):\/\//i.test(otp)) {
      try {
        parseOtpauth(otp);
        return { totp: otp, consumed: ['otp'] };
      } catch {
        /* unparseable: leave as a custom field */
      }
    } else if (/(^|&)key=/i.test(otp)) {
      const q = new Map(otp.split('&').map((kv) => {
        const i = kv.indexOf('=');
        return [kv.slice(0, i < 0 ? kv.length : i).toLowerCase(), i < 0 ? '' : decodeURIComponent(kv.slice(i + 1))];
      }));
      const type = (q.get('type') || 'totp').toLowerCase();
      if (type === 'totp' && isBase32(q.get('key') || '')) {
        const totp = buildOtpauth({
          secret: q.get('key'), issuer, account, algorithm: otpAlgorithm(q.get('otphashmode')),
          digits: Number(q.get('size') || 6), period: Number(q.get('step') || 30),
        });
        return { totp, consumed: ['otp'] };
      }
    } else if (isBase32(otp)) {
      return { totp: otp.replace(/\s+/g, '').toUpperCase(), consumed: ['otp'] };
    }
  }
  const secretKeys = ['TimeOtp-Secret-Base32', 'TimeOtp-Secret-Hex', 'TimeOtp-Secret-Base64', 'TimeOtp-Secret'];
  const present = secretKeys.find((k) => get(k));
  if (present) {
    try {
      const v = get(present);
      const bytes =
        present === 'TimeOtp-Secret-Base32' ? base32decode(v)
          : present === 'TimeOtp-Secret-Hex' ? fromHex(v.replace(/\s+/g, ''))
            : present === 'TimeOtp-Secret-Base64' ? b64decode(v.replace(/\s+/g, ''))
              : utf8Encode(v);
      const totp = buildOtpauth({
        secret: base32encode(bytes), issuer, account, algorithm: otpAlgorithm(get('TimeOtp-Algorithm')),
        digits: Number(get('TimeOtp-Length') || 6), period: Number(get('TimeOtp-Period') || 30),
      });
      return { totp, consumed: [...secretKeys, 'TimeOtp-Length', 'TimeOtp-Period', 'TimeOtp-Algorithm'].filter((k) => fieldMap.has(k)) };
    } catch {
      /* fall through */
    }
  }
  const seed = get('TOTP Seed');
  if (seed && isBase32(seed)) {
    const [period, digits] = get('TOTP Settings').split(';');
    const totp =
      digits === 'S'
        ? `steam://${seed.replace(/\s+/g, '').toUpperCase()}`
        : buildOtpauth({ secret: seed, issuer, account, digits: Number(digits) || 6, period: Number(period) || 30 });
    return { totp, consumed: ['TOTP Seed', 'TOTP Settings'].filter((k) => fieldMap.has(k)) };
  }
  return { totp: null, consumed: [] };
}

// --- KeePass tree → vault ----------------------------------------------------------------------------

function fieldMapOf(entry) {
  return new Map((entry.fields || []).map((f) => [f.key, f]));
}

function passwordHistoryFromKeePass(entry) {
  const pw = (e) => fieldMapOf(e).get('Password')?.value ?? '';
  const versions = [...(entry.history || [])]
    .map((h, i) => ({ h, i }))
    .sort((a, b) => String(a.h.times?.lastModificationTime || '').localeCompare(String(b.h.times?.lastModificationTime || '')) || a.i - b.i)
    .map((x) => x.h);
  versions.push(entry);
  const out = [];
  for (let i = 0; i < versions.length - 1; i++) {
    const p = pw(versions[i]);
    if (p && p !== pw(versions[i + 1])) {
      out.push({ password: p, changed_at: versions[i + 1].times?.lastModificationTime || versions[i].times?.lastModificationTime || nowIso() });
    }
  }
  return out.reverse(); // newest first, like Bitwarden
}

function entryToItem(entry, folderPath, warnings) {
  const fm = fieldMapOf(entry);
  const get = (k) => fm.get(k)?.value ?? '';
  const consumed = new Set(STANDARD);
  const declared = (entry.customData || []).find((c) => c.key === TYPE_KEY)?.value;
  const type = ITEM_TYPES.has(declared) ? declared : 'login';
  const name = get('Title');
  const item = {
    folderPath,
    type,
    name,
    favorite: false,
    notes: get('Notes'),
    fields: [],
    password_history: passwordHistoryFromKeePass(entry),
    created_at: entry.times?.creationTime || nowIso(),
    updated_at: entry.times?.lastModificationTime || entry.times?.creationTime || nowIso(),
  };
  const tags = (entry.tags || []).filter((t) => {
    if (t.toLowerCase() === 'favorite') {
      item.favorite = true;
      return false;
    }
    return true;
  });
  if (tags.length) item.tags = tags;

  const { totp, consumed: totpKeys } = extractKeePassTotp(fm, name, get('UserName'));
  totpKeys.forEach((k) => consumed.add(k));
  const uris = [];
  // KeePass has no match modes: every URI gets the default (domain) matching.
  if (get('URL').trim()) uris.push({ uri: get('URL').trim() });
  for (const f of entry.fields || []) {
    if (URL_FIELD_RE.test(f.key)) {
      consumed.add(f.key);
      if (f.value.trim()) uris.push({ uri: f.value.trim() });
    }
  }
  if (type === 'login' || get('UserName') || get('Password') || uris.length || totp) {
    item.login = { username: get('UserName'), password: get('Password'), uris };
    if (totp) item.login.totp = totp;
  }
  const sub = type === 'card' ? ['card', CARD_KP, emptyCard()] : type === 'identity' ? ['identity', IDENTITY_KP, emptyIdentity()] : null;
  if (sub) {
    const [prop, table, obj] = sub;
    for (const [k, kp] of table) {
      if (!fm.has(kp)) continue;
      obj[k] = get(kp);
      consumed.add(kp);
    }
    item[prop] = obj;
  }
  for (const f of entry.fields || []) {
    if (!consumed.has(f.key)) item.fields.push({ name: f.key, value: f.value, hidden: !!f.protected });
  }
  if ((entry.binaries || []).length) warnings.push(`"${name || '(untitled)'}": ${entry.binaries.length} attachment(s) were not imported`);
  return item;
}

/**
 * KeePass tree → vault interchange data. The recycle bin is skipped unless includeRecycleBin.
 * @param {object} tree KdbxTree
 * @param {{includeRecycleBin?: boolean}} [opts]
 */
export function kdbxTreeToVault(tree, { includeRecycleBin = false } = {}) {
  if (!tree || !tree.root) throw invalidArg('tree has no root group');
  const folders = new Map();
  const items = [];
  const warnings = [];
  const recycle = tree.meta && tree.meta.recycleBinEnabled !== false ? tree.meta.recycleBinUuid : null;
  const walk = (group, path) => {
    for (const e of group.entries || []) items.push(entryToItem(e, path, warnings));
    for (const g of group.groups || []) {
      if (!includeRecycleBin && recycle && g.uuid === recycle) continue;
      const seg = (g.name || '').trim() || 'Untitled';
      const p = path ? `${path}/${seg}` : seg;
      if (!folders.has(p)) folders.set(p, uuid());
      walk(g, p);
    }
  };
  walk(tree.root, '');
  // Attachments in the binary pool that no entry (or history version, or recycled entry) links to.
  const linked = new Set();
  const mark = (group) => {
    for (const e of group.entries || []) {
      for (const v of [e, ...(e.history || [])]) for (const b of v.binaries || []) linked.add(b.ref);
    }
    for (const g of group.groups || []) mark(g);
  };
  mark(tree.root);
  const unlinked = (tree.binaries || []).filter((_, i) => !linked.has(i)).length;
  if (unlinked) warnings.push(`${unlinked} attachment(s) not linked to any entry were ignored`);
  return { folders: [...folders].map(([name, id]) => ({ id, name })), items, warnings };
}

// --- vault → KeePass tree ----------------------------------------------------------------------------

function totpToOtpField(totp, name, username) {
  const t = String(totp).trim();
  if (/^(otpauth|steam):\/\//i.test(t)) return t;
  if (isBase32(t)) return buildOtpauth({ secret: t, issuer: name || '', account: username || '' });
  return t;
}

function itemToEntry(item) {
  const e = newEntry();
  e.times = newTimes(item.created_at, item.updated_at);
  const used = new Set();
  const add = (key, value, prot) => {
    let k = key || 'Field';
    for (let n = 2; used.has(k); n++) k = `${key || 'Field'} (${n})`;
    used.add(k);
    e.fields.push({ key: k, value: value == null ? '' : String(value), protected: !!prot });
  };
  const login = item.login || {};
  const uris = (login.uris || []).map((u) => (typeof u === 'string' ? u : u && u.uri)).filter(Boolean);
  add('Title', item.name, false);
  add('UserName', login.username, false);
  add('Password', login.password, true);
  add('URL', uris[0] || '', false);
  add('Notes', item.notes, false);
  uris.slice(1).forEach((u, i) => add(i === 0 ? 'KP2A_URL' : `KP2A_URL_${i}`, u, false));
  if (login.totp) add('otp', totpToOtpField(login.totp, item.name, login.username), true);
  if (item.type === 'card' && item.card) for (const [k, kp, p] of CARD_KP) if (item.card[k]) add(kp, item.card[k], p);
  if (item.type === 'identity' && item.identity) for (const [k, kp, p] of IDENTITY_KP) if (item.identity[k]) add(kp, item.identity[k], p);
  for (const f of item.fields || []) add(f.name, f.value, f.hidden);
  e.tags = [...(item.favorite ? ['Favorite'] : []), ...(Array.isArray(item.tags) ? item.tags.filter((t) => typeof t === 'string' && t) : [])];
  if (item.type && item.type !== 'login' && ITEM_TYPES.has(item.type)) e.customData = [{ key: TYPE_KEY, value: item.type }];
  // Each history snapshot carries the old password; its modification time is the previous change
  // (or creation), so reading it back yields the same changed_at for all but possibly the newest entry.
  const hist = [...(item.password_history || [])].filter((h) => h && h.password).sort((a, b) => String(a.changed_at || '').localeCompare(String(b.changed_at || '')));
  let prevTime = item.created_at || hist[0]?.changed_at;
  for (const h of hist) {
    const snap = newEntry();
    snap.uuid = e.uuid;
    snap.tags = [...e.tags];
    snap.customData = e.customData.map((c) => ({ ...c }));
    snap.fields = e.fields.map((f) => (f.key === 'Password' ? { ...f, value: h.password } : { ...f }));
    snap.times = newTimes(item.created_at, prevTime);
    e.history.push(snap);
    prevTime = h.changed_at;
  }
  return e;
}

/**
 * Vault interchange data → KeePass tree (folders become nested groups).
 * @param {{folders?: Array<{name:string}>, items: Array<object>}} data
 * @param {{databaseName?: string}} [opts]
 */
export function vaultToKdbxTree(data, { databaseName = 'Ludworks Vault' } = {}) {
  const tree = newTree(databaseName);
  const groups = new Map([['', tree.root]]);
  const ensure = (path) => {
    const norm = normalizeFolderPath(path);
    if (groups.has(norm)) return groups.get(norm);
    const idx = norm.lastIndexOf('/');
    const parent = ensure(idx < 0 ? '' : norm.slice(0, idx));
    const g = newGroup(norm.slice(idx + 1));
    parent.groups.push(g);
    groups.set(norm, g);
    return g;
  };
  for (const f of (data && data.folders) || []) ensure(f.name);
  for (const it of (data && data.items) || []) ensure(it.folderPath).entries.push(itemToEntry(it));
  return tree;
}

// --- Bitwarden ⇄ vault -----------------------------------------------------------------------------

const str = (v) => (v === null || v === undefined ? '' : String(v));

/**
 * Bitwarden-shaped data → vault interchange data.
 * @param {{folders?: Array<{id,name}>, items: Array<object>}} bw
 */
export function bitwardenToVault(bw) {
  const warnings = [];
  const folderById = new Map();
  const folders = new Map();
  for (const f of bw.folders || []) {
    const p = normalizeFolderPath(f.name);
    if (!p) continue;
    folderById.set(f.id, p);
    if (!folders.has(p)) folders.set(p, uuid());
  }
  const items = [];
  for (const it of bw.items || []) {
    if (it.deletedDate) continue; // in Bitwarden's trash
    const name = str(it.name);
    const item = {
      folderPath: (it.folderId && folderById.get(it.folderId)) || '',
      type: 'login',
      name,
      favorite: !!it.favorite,
      notes: str(it.notes),
      fields: [],
      password_history: (it.passwordHistory || [])
        .filter((h) => h && h.password)
        .map((h) => ({ password: String(h.password), changed_at: h.lastUsedDate || nowIso() })),
      created_at: it.creationDate || it.revisionDate || nowIso(),
      updated_at: it.revisionDate || it.creationDate || nowIso(),
    };
    for (const f of it.fields || []) {
      if (!f || f.type === 3) continue; // linked fields reference another property; nothing to carry
      item.fields.push({ name: str(f.name), value: str(f.value), hidden: f.type === 1 });
    }
    switch (it.type) {
      case 1: {
        const l = it.login || {};
        item.login = {
          username: str(l.username),
          password: str(l.password),
          uris: (l.uris || []).filter((u) => u && u.uri).map((u) => {
            const m = BW_MATCH[u.match];
            return m ? { uri: String(u.uri), match: m } : { uri: String(u.uri) };
          }),
        };
        if (l.totp) item.login.totp = String(l.totp);
        if ((l.fido2Credentials || []).length) warnings.push(`"${name}": passkeys cannot be transferred and were skipped`);
        break;
      }
      case 2:
        item.type = 'note';
        break;
      case 3:
        item.type = 'card';
        item.card = Object.fromEntries(CARD_BW.map(([k, b]) => [k, str(it.card && it.card[b])]));
        break;
      case 4: {
        item.type = 'identity';
        const id = it.identity || {};
        item.identity = Object.fromEntries(IDENTITY_BW.map(([k, b]) => [k, str(id[b])]));
        if (id.address3) item.fields.unshift({ name: 'Address 3', value: str(id.address3), hidden: false });
        break;
      }
      case 5: {
        item.type = 'note';
        const k = it.sshKey || {};
        if (k.privateKey) item.fields.push({ name: 'Private key', value: str(k.privateKey), hidden: true });
        if (k.publicKey) item.fields.push({ name: 'Public key', value: str(k.publicKey), hidden: false });
        if (k.keyFingerprint) item.fields.push({ name: 'Fingerprint', value: str(k.keyFingerprint), hidden: false });
        break;
      }
      default:
        item.type = 'note';
        warnings.push(`"${name}": unknown Bitwarden item type ${it.type} imported as a note`);
    }
    items.push(item);
  }
  return { folders: [...folders].map(([name, id]) => ({ id, name })), items, warnings };
}

/**
 * Vault interchange data → Bitwarden-shaped data (feed to toBitwardenJson / toBitwardenCsv).
 * @param {{folders?: Array<{id,name}>, items: Array<object>}} data
 */
export function vaultToBitwarden(data) {
  const folderIds = new Map();
  for (const f of (data && data.folders) || []) {
    const p = normalizeFolderPath(f.name);
    if (p && !folderIds.has(p)) folderIds.set(p, uuid());
  }
  const items = [];
  for (const it of (data && data.items) || []) {
    const path = normalizeFolderPath(it.folderPath);
    if (path && !folderIds.has(path)) folderIds.set(path, uuid());
    const out = {
      passwordHistory: (it.password_history || []).length
        ? it.password_history.map((h) => ({ lastUsedDate: h.changed_at || nowIso(), password: str(h.password) }))
        : null,
      revisionDate: it.updated_at || nowIso(),
      creationDate: it.created_at || it.updated_at || nowIso(),
      deletedDate: null,
      id: uuid(),
      organizationId: null,
      folderId: path ? folderIds.get(path) : null,
      type: 1,
      reprompt: 0,
      name: str(it.name),
      notes: it.notes ? String(it.notes) : null,
      favorite: !!it.favorite,
      fields: (it.fields || []).map((f) => ({ name: str(f.name), value: str(f.value), type: f.hidden ? 1 : 0, linkedId: null })),
      collectionIds: null,
    };
    const type = ITEM_TYPES.has(it.type) ? it.type : it.login ? 'login' : 'note';
    if (type === 'login') {
      const l = it.login || {};
      out.login = {
        fido2Credentials: [],
        uris: (l.uris || []).map((u) => (typeof u === 'string' ? { uri: u } : u)).filter((u) => u && u.uri).map((u) => {
          const i = BW_MATCH.indexOf(u.match);
          return { match: i >= 0 ? i : null, uri: String(u.uri) };
        }),
        username: l.username ? String(l.username) : null,
        password: l.password ? String(l.password) : null,
        totp: l.totp ? String(l.totp) : null,
      };
    } else if (type === 'note') {
      out.type = 2;
      out.secureNote = { type: 0 };
    } else if (type === 'card') {
      out.type = 3;
      out.card = Object.fromEntries(CARD_BW.map(([k, b]) => [b, it.card && it.card[k] ? String(it.card[k]) : null]));
    } else {
      out.type = 4;
      out.identity = Object.fromEntries(IDENTITY_BW.map(([k, b]) => [b, it.identity && it.identity[k] ? String(it.identity[k]) : null]));
      out.identity.address3 = null;
    }
    items.push(out);
  }
  return { encrypted: false, folders: [...folderIds].map(([name, id]) => ({ id, name })), items };
}

// --- KeePass XML convenience -----------------------------------------------------------------------

/**
 * KeePass 2 XML text → vault data. Attachments are not imported, so they are never decoded or
 * decompressed (a file full of compressed attachments cannot exhaust memory); the decompressed total is
 * capped anyway (opts.maxDecompressedBytes, default 512 MiB).
 */
export async function keepassXmlToVault(text, opts = {}) {
  return kdbxTreeToVault(await parseKeePassXml(text, { decodeBinaries: false, maxDecompressedBytes: opts.maxDecompressedBytes }), opts);
}

/** Vault data → KeePass 2 XML text (plaintext secrets!). */
export function vaultToKeepassXml(data, opts = {}) {
  return serializeKeePassXml(vaultToKdbxTree(data, opts));
}

// --- one-stop import / export ----------------------------------------------------------------------

/** Formats understood by importVault / exportVault. `plaintext` exports must carry a warning in the UI. */
export const FORMATS = Object.freeze({
  kdbx: { label: 'KeePass 2 (.kdbx)', extension: '.kdbx', mime: 'application/octet-stream', plaintext: false },
  'keepass-xml': { label: 'KeePass 2 XML', extension: '.xml', mime: 'application/xml', plaintext: true },
  'bitwarden-json': { label: 'Bitwarden JSON', extension: '.json', mime: 'application/json', plaintext: true },
  'bitwarden-csv': { label: 'Bitwarden CSV', extension: '.csv', mime: 'text/csv', plaintext: true },
});

function asText(input) {
  return typeof input === 'string' ? input : utf8Decode(asBytes(input, 'input'));
}

/**
 * Guess the format of an import file from its content.
 * @param {Uint8Array|string} input
 * @returns {'kdbx'|'keepass-xml'|'bitwarden-json'|'bitwarden-csv'|null}
 */
export function detectFormat(input) {
  if (input instanceof Uint8Array && input.length >= 8) {
    const dv = new DataView(input.buffer, input.byteOffset, input.byteLength);
    if (dv.getUint32(0, true) === 0x9aa2d903) return 'kdbx';
  }
  let text;
  try {
    text = asText(input).slice(0, 4096).replace(/^﻿/, '').trimStart();
  } catch {
    return null;
  }
  if (text.startsWith('<')) return /<KeePassFile[\s>]/.test(text) ? 'keepass-xml' : null;
  if (text.startsWith('{')) return 'bitwarden-json';
  if (/^"?folder"?,|(^|,)"?name"?,/i.test(text.split(/\r?\n/, 1)[0])) return 'bitwarden-csv';
  return null;
}

/**
 * Import any supported format into vault interchange data.
 * @param {'kdbx'|'keepass-xml'|'bitwarden-json'|'bitwarden-csv'} format
 * @param {Uint8Array|string} input
 * @param {{password?: string, keyFile?: Uint8Array, includeRecycleBin?: boolean, limits?: object,
 *          onProgress?: (f:number)=>void}} [opts]
 */
export async function importVault(format, input, opts = {}) {
  switch (format) {
    case 'kdbx':
      // Attachments are dropped on import: never decode them (decompression bombs cost nothing).
      return kdbxTreeToVault(await readKdbx(asBytes(input, 'input'), { ...opts, decodeBinaries: false }), opts);
    case 'keepass-xml':
      return keepassXmlToVault(asText(input), opts);
    case 'bitwarden-json':
      return bitwardenToVault(await parseBitwardenJson(asText(input), opts));
    case 'bitwarden-csv':
      return bitwardenToVault(parseBitwardenCsv(asText(input)));
    default:
      throw new VaultError('unsupported', `unknown import format "${format}"`);
  }
}

/**
 * Export vault interchange data. kdbx → Uint8Array (encrypted); others → string (plaintext unless a
 * Bitwarden JSON `password` is given).
 * @param {'kdbx'|'keepass-xml'|'bitwarden-json'|'bitwarden-csv'} format
 * @param {{folders?: Array<object>, items: Array<object>}} data
 * @param {object} [opts] kdbx: writeKdbx options + databaseName; bitwarden-json: {password, kdfType, ...}
 * @returns {Promise<Uint8Array|string>}
 */
export async function exportVault(format, data, opts = {}) {
  switch (format) {
    case 'kdbx':
      return writeKdbx(vaultToKdbxTree(data, opts), opts);
    case 'keepass-xml':
      return vaultToKeepassXml(data, opts);
    case 'bitwarden-json':
      return toBitwardenJson(vaultToBitwarden(data), opts);
    case 'bitwarden-csv':
      return toBitwardenCsv(vaultToBitwarden(data));
    default:
      throw new VaultError('unsupported', `unknown export format "${format}"`);
  }
}
