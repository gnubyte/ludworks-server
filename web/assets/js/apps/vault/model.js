// Pure helpers for the Passwords app (no DOM, no network): item shapes per VAULT-CRYPTO.md §3,
// display strings, search and sort, the folder tree, duplicate detection, import batching and
// the client-side security report. Everything here is unit-tested under node.

import { estimateStrength } from '../../lib/vault/generator.js';
import { registrableDomain } from '../../lib/vault/url-match.js';
import { base32decode, parseOtpauth } from '../../lib/vault/totp.js';

export const TYPES = ['login', 'note', 'card', 'identity'];

export const TYPE_INFO = Object.freeze({
  login: { label: 'Login', plural: 'Logins', icon: 'key' },
  note: { label: 'Secure note', plural: 'Secure notes', icon: 'note' },
  card: { label: 'Card', plural: 'Cards', icon: 'card' },
  identity: { label: 'Identity', plural: 'Identities', icon: 'id' },
});

/** URI match modes (§3). '' = the default (base domain). */
export const MATCH_MODES = Object.freeze([
  { value: '', label: 'Default (base domain)' },
  { value: 'domain', label: 'Base domain' },
  { value: 'host', label: 'Host' },
  { value: 'starts_with', label: 'Starts with' },
  { value: 'exact', label: 'Exact' },
  { value: 'regex', label: 'Regular expression' },
  { value: 'never', label: 'Never' },
]);

/** [key, label, secret] */
export const CARD_FIELDS = Object.freeze([
  ['cardholder', 'Cardholder name', false], ['brand', 'Brand', false], ['number', 'Number', true],
  ['exp_month', 'Expiry month', false], ['exp_year', 'Expiry year', false], ['code', 'Security code', true],
]);

export const IDENTITY_FIELDS = Object.freeze([
  ['title', 'Title', false], ['first_name', 'First name', false], ['middle_name', 'Middle name', false],
  ['last_name', 'Last name', false], ['company', 'Company', false], ['email', 'Email', false],
  ['phone', 'Phone', false], ['address1', 'Address', false], ['address2', 'Address line 2', false],
  ['city', 'City', false], ['state', 'State / province', false], ['postal_code', 'Postal code', false],
  ['country', 'Country', false], ['username', 'Username', false], ['ssn', 'Social security number', true],
  ['passport', 'Passport number', true], ['license', 'License number', true],
]);

export const CARD_BRANDS = Object.freeze(['Visa', 'Mastercard', 'American Express', 'Discover', 'Diners Club', 'JCB', 'Maestro', 'UnionPay', 'Other']);

/** Old passwords kept per item. */
export const MAX_HISTORY = 10;

/** A string from an untrusted decrypted value ('' for anything that is not a primitive). */
export function str(v) {
  if (typeof v === 'string') return v;
  if (typeof v === 'number' || typeof v === 'boolean') return String(v);
  return '';
}

/**
 * The name an item's initials avatar is drawn from: punctuation and symbols become word breaks so
 * "GitHub (kdbx)" gives "GK", not "G(", and "[Work] Mail" gives "WM". '?' when nothing is left.
 */
export function avatarName(name) {
  const clean = str(name).replace(/[^\p{L}\p{N}]+/gu, ' ').trim();
  return clean || '?';
}

const isObj = (v) => v !== null && typeof v === 'object' && !Array.isArray(v);

/** ISO-8601 UTC timestamp. */
export function isoNow(now = new Date()) {
  return new Date(now).toISOString();
}

/**
 * A new item plaintext of the given type (only the sub-object for that type, per §3).
 * @param {'login'|'note'|'card'|'identity'} type
 */
export function blankItem(type, now = new Date()) {
  const ts = isoNow(now);
  const item = { type, name: '', favorite: false, notes: '', fields: [], password_history: [], created_at: ts, updated_at: ts };
  if (type === 'login') item.login = { username: '', password: '', uris: [], totp: '' };
  if (type === 'card') item.card = Object.fromEntries(CARD_FIELDS.map(([k]) => [k, '']));
  if (type === 'identity') item.identity = Object.fromEntries(IDENTITY_FIELDS.map(([k]) => [k, '']));
  return item;
}

/**
 * A normalised, read-only view of a decrypted item for display. Tolerates malformed data
 * (other clients, imports) without throwing; never used as the basis for saving.
 */
export function view(plain) {
  const p = isObj(plain) ? plain : {};
  const type = TYPES.includes(p.type) ? p.type : 'note';
  const login = isObj(p.login) ? p.login : {};
  const card = isObj(p.card) ? p.card : {};
  const ident = isObj(p.identity) ? p.identity : {};
  return {
    type,
    name: str(p.name).trim() || '(untitled)',
    favorite: p.favorite === true,
    notes: str(p.notes),
    login: {
      username: str(login.username),
      password: str(login.password),
      totp: str(login.totp).trim(),
      uris: (Array.isArray(login.uris) ? login.uris : [])
        .filter(isObj).map((u) => ({ uri: str(u.uri).trim(), match: str(u.match) })).filter((u) => u.uri),
    },
    card: Object.fromEntries(CARD_FIELDS.map(([k]) => [k, str(card[k])])),
    identity: Object.fromEntries(IDENTITY_FIELDS.map(([k]) => [k, str(ident[k])])),
    fields: (Array.isArray(p.fields) ? p.fields : []).filter(isObj)
      .map((f) => ({ name: str(f.name), value: str(f.value), hidden: f.hidden === true })),
    history: (Array.isArray(p.password_history) ? p.password_history : []).filter(isObj)
      .map((h) => ({ password: str(h.password), changed_at: str(h.changed_at) })).filter((h) => h.password),
    created_at: str(p.created_at),
    updated_at: str(p.updated_at),
  };
}

/** Parse a saved URI (scheme optional) into a URL, or null. */
export function parseUri(uri) {
  const s = str(uri).trim();
  if (!s) return null;
  try {
    return new URL(/^[a-z][a-z0-9+.-]*:/i.test(s) ? s : `https://${s}`);
  } catch {
    return null;
  }
}

/** Host name of a saved URI ('' when it has none). */
export function hostOf(uri) {
  const u = parseUri(uri);
  return u && (u.protocol === 'http:' || u.protocol === 'https:') ? u.hostname : '';
}

/** A link target for "open in browser": only http(s) URLs, else null (never javascript: etc). */
export function safeHref(uri) {
  const u = parseUri(uri);
  return u && (u.protocol === 'http:' || u.protocol === 'https:') ? u.href : null;
}

const last4 = (n) => str(n).replace(/\D/g, '').slice(-4);

/** One-line secondary text for list rows. */
export function subtitle(v) {
  switch (v.type) {
    case 'login':
      return v.login.username || hostOf(v.login.uris[0]?.uri) || '';
    case 'card': {
      const l4 = last4(v.card.number);
      return [v.card.brand || detectCardBrand(v.card.number), l4 ? `•••• ${l4}` : ''].filter(Boolean).join(' ');
    }
    case 'identity':
      return [v.identity.first_name, v.identity.last_name].filter(Boolean).join(' ') || v.identity.email || '';
    default: {
      const line = v.notes.split('\n').find((l) => l.trim()) || '';
      return line.length > 80 ? `${line.slice(0, 79)}…` : line;
    }
  }
}

/** Lower-cased text searched by the list filter. Secrets (passwords, hidden fields, card numbers) are excluded. */
export function haystack(v, folderPath = '') {
  const parts = [v.name, folderPath, v.notes];
  if (v.type === 'login') parts.push(v.login.username, ...v.login.uris.map((u) => u.uri));
  if (v.type === 'card') parts.push(v.card.cardholder, v.card.brand, last4(v.card.number));
  if (v.type === 'identity') {
    const i = v.identity;
    parts.push(i.first_name, i.middle_name, i.last_name, i.company, i.email, i.phone, i.username, i.city, i.country);
  }
  for (const f of v.fields) parts.push(f.name, f.hidden ? '' : f.value);
  return parts.filter(Boolean).join('\n').toLowerCase();
}

/** Does the item match every whitespace-separated term of the query? */
export function matchesQuery(hay, query) {
  const terms = str(query).toLowerCase().split(/\s+/).filter(Boolean);
  return terms.every((t) => hay.includes(t));
}

const collator = typeof Intl !== 'undefined' ? new Intl.Collator(undefined, { numeric: true, sensitivity: 'base' }) : null;
const cmpText = (a, b) => (collator ? collator.compare(a, b) : a.localeCompare(b));

export const SORTS = Object.freeze([
  { value: 'name', label: 'Name (A–Z)' },
  { value: 'name-desc', label: 'Name (Z–A)' },
  { value: 'updated', label: 'Recently updated' },
  { value: 'created', label: 'Recently created' },
]);

/**
 * Sort list entries ({view, updated, created}) in place and return them.
 * @param {Array<{view: object, updated?: string, created?: string}>} entries
 * @param {string} sort
 */
export function sortEntries(entries, sort) {
  const byName = (a, b) => cmpText(a.view.name, b.view.name) || cmpText(subtitle(a.view), subtitle(b.view));
  const byTime = (k) => (a, b) => (str(b[k]) > str(a[k]) ? 1 : str(b[k]) < str(a[k]) ? -1 : 0) || byName(a, b);
  switch (sort) {
    case 'name-desc':
      return entries.sort((a, b) => byName(b, a));
    case 'updated':
      return entries.sort(byTime('updated'));
    case 'created':
      return entries.sort(byTime('created'));
    default:
      return entries.sort(byName);
  }
}

// ------------------------------------------------------------------------------------ folders

/** Normalise a folder path: trim segments, drop empty ones, '/'-joined (§8.4). */
export function normalizePath(p) {
  return str(p).split('/').map((s) => s.trim()).filter(Boolean).join('/');
}

export const MAX_FOLDER_PATH = 1000;

/**
 * Validate a new folder path. Returns an error message or null.
 * @param {string} path normalised path
 * @param {Iterable<string>} existing paths in use
 */
export function folderPathError(path, existing = []) {
  if (!path) return 'Enter a folder name.';
  if (path.length > MAX_FOLDER_PATH) return `Folder paths are limited to ${MAX_FOLDER_PATH} characters.`;
  if (/[\u0000-\u001f\u007f]/.test(path)) return 'Folder names cannot contain control characters.';
  for (const p of existing) if (p.toLowerCase() === path.toLowerCase()) return 'A folder with this name already exists.';
  return null;
}

/** Is itemPath the node path or inside it? */
export function inFolder(itemPath, nodePath) {
  return itemPath === nodePath || itemPath.startsWith(`${nodePath}/`);
}

/**
 * Build the sidebar tree from folders [{id, path}]. Parents that have no folder record of their
 * own (e.g. only "Work/Email" exists) appear as virtual nodes with id null.
 * @returns {{children: Array<{name: string, path: string, id: string|null, children: any[], depth: number}>}}
 */
export function buildFolderTree(folders) {
  const root = { children: [], path: '', depth: -1 };
  const byPath = new Map([['', root]]);
  const ensure = (path) => {
    let node = byPath.get(path);
    if (node) return node;
    const i = path.lastIndexOf('/');
    const parent = ensure(i < 0 ? '' : path.slice(0, i));
    node = { name: i < 0 ? path : path.slice(i + 1), path, id: null, children: [], depth: parent.depth + 1 };
    parent.children.push(node);
    byPath.set(path, node);
    return node;
  };
  for (const f of folders) {
    const path = normalizePath(f.path);
    if (!path) continue;
    const node = ensure(path);
    if (!node.id) node.id = f.id;
  }
  const sortRec = (n) => {
    n.children.sort((a, b) => cmpText(a.name, b.name));
    n.children.forEach(sortRec);
  };
  sortRec(root);
  return root;
}

/** Folder records whose path is the node path or below it (for rename/delete of a tree node). */
export function foldersUnder(folders, nodePath) {
  return folders.filter((f) => inFolder(normalizePath(f.path), nodePath));
}

/** New path of `path` when the subtree at `from` is renamed to `to`. */
export function renamePath(path, from, to) {
  return path === from ? to : `${to}${path.slice(from.length)}`;
}

// ----------------------------------------------------------------------------------- cards

/** Best-effort card brand from the number's prefix. */
export function detectCardBrand(number) {
  const n = str(number).replace(/\D/g, '');
  if (!n) return '';
  if (/^4/.test(n)) return 'Visa';
  if (/^(5[1-5]|2(2[2-9][1-9]|2[3-9]\d|[3-6]\d\d|7[01]\d|720))/.test(n)) return 'Mastercard';
  if (/^3[47]/.test(n)) return 'American Express';
  if (/^(6011|65|64[4-9])/.test(n)) return 'Discover';
  if (/^3(0[0-5]|[68])/.test(n)) return 'Diners Club';
  if (/^35(2[89]|[3-8])/.test(n)) return 'JCB';
  if (/^62/.test(n)) return 'UnionPay';
  if (/^(5[06-9]|6\d)/.test(n)) return 'Maestro';
  return '';
}

/** Luhn checksum (true for plausible card numbers). */
export function luhnValid(number) {
  const d = str(number).replace(/\D/g, '');
  if (d.length < 12 || d.length > 19) return false;
  let sum = 0;
  for (let i = 0; i < d.length; i++) {
    let x = d.charCodeAt(d.length - 1 - i) - 48;
    if (i % 2 === 1) {
      x *= 2;
      if (x > 9) x -= 9;
    }
    sum += x;
  }
  return sum % 10 === 0;
}

/** "1234 5678 9012 3456" grouping for display. */
export function formatCardNumber(number) {
  const d = str(number).replace(/\s+/g, '');
  if (/^3[47]\d{13}$/.test(d)) return `${d.slice(0, 4)} ${d.slice(4, 10)} ${d.slice(10)}`;
  return d.replace(/(.{4})(?=.)/g, '$1 ');
}

// ------------------------------------------------------------------------------------ TOTP

/** Validate a TOTP secret (base32) or otpauth:// / steam:// URI. Returns an error message or null. */
export function totpError(value) {
  const s = str(value).trim();
  if (!s) return null;
  try {
    if (/^(otpauth|steam):\/\//i.test(s)) parseOtpauth(s);
    else if (base32decode(s).length < 5) return 'This secret is too short.';
    return null;
  } catch (e) {
    return e?.message ? `${e.message[0].toUpperCase()}${e.message.slice(1)}.` : 'Not a valid TOTP secret.';
  }
}

// ------------------------------------------------------------------------------- saving

/**
 * Final plaintext to encrypt after an edit: keeps created_at, stamps updated_at and, when a
 * login's password changed, pushes the old one onto password_history (newest first, capped).
 * Unknown keys present in `edited` are preserved untouched (§3 forward compatibility).
 * @param {object|null} original previous plaintext (null for a new item)
 * @param {object} edited
 */
export function finalizeItem(original, edited, now = new Date()) {
  const out = structuredClone(edited);
  const ts = isoNow(now);
  out.updated_at = ts;
  if (!original) {
    out.created_at = str(out.created_at) || ts;
    return out;
  }
  out.created_at = str(original.created_at) || str(out.created_at) || ts;
  const oldPw = str(original?.login?.password);
  const newPw = str(out?.login?.password);
  if (out.type === 'login' && oldPw && oldPw !== newPw) {
    const hist = (Array.isArray(original.password_history) ? original.password_history : [])
      .filter((h) => isObj(h) && typeof h.password === 'string' && h.password);
    out.password_history = [{ password: oldPw, changed_at: ts }, ...hist].slice(0, MAX_HISTORY);
  }
  return out;
}

// ------------------------------------------------------------------------------- import

const normUri = (u) => str(u).trim().toLowerCase().replace(/\/+$/, '');

/** Duplicate key: same name + username + first URI (case-insensitive). */
export function dupKey(plain) {
  const v = view(plain);
  return [v.name.trim().toLowerCase(), v.login.username.trim().toLowerCase(), normUri(v.login.uris[0]?.uri)].join('\u0000');
}

/**
 * Split encrypted rows into batches of at most maxItems entries and roughly maxBytes of JSON.
 * @param {Array<{id: string, folder_id: string|null, data: string}>} rows
 */
export function chunkBatches(rows, { maxItems = 1000, maxBytes = 8 << 20 } = {}) {
  const out = [];
  let cur = [];
  let bytes = 0;
  for (const r of rows) {
    const size = r.data.length + r.id.length + (r.folder_id ? r.folder_id.length : 4) + 40;
    if (cur.length && (cur.length >= maxItems || bytes + size > maxBytes)) {
      out.push(cur);
      cur = [];
      bytes = 0;
    }
    cur.push(r);
    bytes += size;
  }
  if (cur.length) out.push(cur);
  return out;
}

// ------------------------------------------------------------------------ security report

/** Sites known to offer TOTP two-factor authentication (registrable domains). */
export const TWO_FACTOR_DOMAINS = new Set([
  'google.com', 'gmail.com', 'youtube.com', 'microsoft.com', 'live.com', 'outlook.com', 'office.com', 'apple.com', 'icloud.com',
  'amazon.com', 'amazon.co.uk', 'amazon.de', 'github.com', 'gitlab.com', 'bitbucket.org', 'atlassian.com', 'dropbox.com',
  'box.com', 'facebook.com', 'instagram.com', 'twitter.com', 'x.com', 'linkedin.com', 'reddit.com', 'discord.com', 'slack.com',
  'twitch.tv', 'paypal.com', 'stripe.com', 'coinbase.com', 'binance.com', 'kraken.com', 'cloudflare.com', 'digitalocean.com',
  'linode.com', 'vultr.com', 'hetzner.com', 'ovh.com', 'ovhcloud.com', 'namecheap.com', 'godaddy.com', 'porkbun.com',
  'npmjs.com', 'pypi.org', 'docker.com', 'heroku.com', 'vercel.com', 'netlify.com', 'proton.me', 'protonmail.com',
  'fastmail.com', 'zoho.com', 'yahoo.com', 'aol.com', 'steampowered.com', 'epicgames.com', 'ea.com', 'ubisoft.com',
  'battle.net', 'nintendo.com', 'playstation.com', 'xbox.com', 'ebay.com', 'shopify.com', 'wordpress.com', 'tumblr.com',
  'figma.com', 'notion.so', 'zoom.us', 'adobe.com', 'evernote.com', 'mailchimp.com', 'hubspot.com', 'salesforce.com',
  'backblaze.com', 'mega.nz', 'tailscale.com', 'okta.com', 'openai.com', 'anthropic.com', 'tiktok.com', 'snapchat.com',
  'pinterest.com', 'wise.com', 'revolut.com', 'robinhood.com', 'gemini.com', 'bitwarden.com', '1password.com', 'lastpass.com',
]);

/** Hosts for which plain http:// is not flagged (loopback / LAN). */
function isLocalHost(host) {
  return host === 'localhost' || host.endsWith('.localhost') || host.endsWith('.local') || host.endsWith('.lan')
    || /^127\./.test(host) || /^10\./.test(host) || /^192\.168\./.test(host) || /^172\.(1[6-9]|2\d|3[01])\./.test(host)
    || host === '[::1]' || !host.includes('.');
}

/**
 * When the current password was set: the newest history entry's changed_at, else created_at.
 * @returns {number} epoch ms (NaN when unknown)
 */
export function passwordSetAt(v) {
  let t = NaN;
  for (const h of v.history) {
    const x = Date.parse(h.changed_at);
    if (Number.isFinite(x) && !(x <= t)) t = x;
  }
  return Number.isFinite(t) ? t : Date.parse(v.created_at || v.updated_at);
}

/**
 * Client-side security report over decrypted entries [{id, view}].
 * @param {Array<{id: string, view: object}>} entries
 * @param {{now?: number, oldDays?: number, strength?: (pw: string) => {score: number}}} [opts]
 */
export function securityReport(entries, { now = Date.now(), oldDays = 365, strength = estimateStrength } = {}) {
  const logins = entries.filter((e) => e.view.type === 'login');
  const withPw = logins.filter((e) => e.view.login.password);
  const weak = [];
  const old = [];
  const insecure = [];
  const missing2fa = [];
  const byPassword = new Map();
  for (const e of withPw) {
    const pw = e.view.login.password;
    const s = strength(pw);
    if (s.score <= 1) weak.push({ id: e.id, score: s.score });
    if (!byPassword.has(pw)) byPassword.set(pw, []);
    byPassword.get(pw).push(e.id);
    const set = passwordSetAt(e.view);
    if (Number.isFinite(set)) {
      const days = Math.floor((now - set) / 86400000);
      if (days >= oldDays) old.push({ id: e.id, days });
    }
  }
  for (const e of logins) {
    const bad = e.view.login.uris.map((u) => u.uri).filter((u) => {
      const p = parseUri(u);
      return p && p.protocol === 'http:' && !isLocalHost(p.hostname);
    });
    if (bad.length) insecure.push({ id: e.id, uris: bad });
    if (!e.view.login.totp) {
      const domain = e.view.login.uris.map((u) => registrableDomain(hostOf(u.uri))).find((d) => TWO_FACTOR_DOMAINS.has(d));
      if (domain) missing2fa.push({ id: e.id, domain });
    }
  }
  const reused = [...byPassword.values()].filter((ids) => ids.length > 1).map((ids) => ({ count: ids.length, ids }))
    .sort((a, b) => b.count - a.count);
  const reusedIds = new Set(reused.flatMap((g) => g.ids));
  old.sort((a, b) => b.days - a.days);
  const flagged = new Set([...weak.map((x) => x.id), ...reusedIds, ...insecure.map((x) => x.id)]);
  const score = withPw.length ? Math.round(100 * (1 - flagged.size / Math.max(withPw.length, flagged.size))) : 100;
  return { logins: logins.length, checked: withPw.length, weak, reused, reusedIds, old, insecure, missing2fa, score };
}
