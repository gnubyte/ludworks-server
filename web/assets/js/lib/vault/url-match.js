/**
 * URI matching for autofill (VAULT-CRYPTO.md §3 "URI match values", §3.1 registrable domains, §8.3).
 *
 * Security choices (stricter than some password managers; see spec §8.3):
 *  - `domain` compares registrable domains computed with the Public Suffix List — ICANN *and* private
 *    sections — so tenants of shared hosting / dynamic-DNS suffixes (victim.github.io vs attacker.github.io,
 *    mynas.duckdns.org vs evil.duckdns.org, a.tail1.ts.net vs b.tail2.ts.net, alice.me.uk vs bob.me.uk) are
 *    different sites. A host that is itself a public suffix only matches itself.
 *  - `domain`/`host` only ever match http(s) pages, and an https item URI never matches an http page
 *    (no autofill into a downgraded connection).
 *  - IP addresses, `localhost` and single-label hosts compare by host *including port* in `domain` mode,
 *    since different ports on one machine are usually different applications.
 *  - `starts_with` with an absolute http(s) item URI also requires the page to have the same origin, so
 *    "https://bank.com" never matches "https://bank.com.evil.net/".
 *  - Unknown modes never match; `regex` patterns with nested quantifiers (ReDoS shapes) never match.
 */
import { PSL_RULES } from './psl-data.js';

const IPV4_RE = /^(\d{1,3})\.(\d{1,3})\.(\d{1,3})\.(\d{1,3})$/;
const MAX_REGEX_LENGTH = 1000;
const MAX_URL_LENGTH = 4096;
// A quantified group that itself contains a quantifier ("(a+)+", "(\w*)*", "(x+){2,}") is the classic
// catastrophic-backtracking shape. Vault data can come from imported files, so such patterns are refused
// rather than risking a hung extension worker on every page load.
const NESTED_QUANTIFIER_RE = /\((?:[^()\\]|\\.)*[+*}](?:[^()\\]|\\.)*\)\s*(?:[+*]|\{\d*,?\d*\})/;

let psl = null;

/** Rule sets built once from the embedded snapshot: exact rules, wildcard parents ("*.ck" → "ck"), exceptions. */
function rules() {
  if (psl) return psl;
  const exact = new Set();
  const wild = new Set();
  const except = new Set();
  for (const r of PSL_RULES.split('\n')) {
    if (!r) continue;
    if (r.startsWith('!')) except.add(r.slice(1));
    else if (r.startsWith('*.')) wild.add(r.slice(2));
    else exact.add(r);
  }
  psl = { exact, wild, except };
  return psl;
}

/**
 * Number of trailing labels that form the public suffix (publicsuffix.org algorithm): the longest matching
 * rule wins, an exception rule always wins and drops its leftmost label, no match = the implicit "*" rule.
 * @param {string[]} labels lowercase ASCII labels
 */
function suffixLabelCount(labels) {
  const { exact, wild, except } = rules();
  const n = labels.length;
  let best = 1;
  for (let i = 1; i <= n; i++) {
    const cand = labels.slice(n - i).join('.');
    if (except.has(cand)) return i - 1;
    if (exact.has(cand)) best = i;
    if (i >= 2 && wild.has(labels.slice(n - i + 1).join('.'))) best = i;
  }
  return best;
}

function isIp(host) {
  return IPV4_RE.test(host) || host.includes(':') || host.startsWith('[');
}

function stripPort(host) {
  if (host.startsWith('[')) {
    const end = host.indexOf(']');
    return end > 0 ? host.slice(0, end + 1) : host;
  }
  const m = /^([^:]+):\d+$/.exec(host);
  return m ? m[1] : host;
}

/** Lowercase ASCII (punycode) form of a hostname; '' when it is not a valid hostname. */
function toAscii(h) {
  // eslint-disable-next-line no-control-regex
  if (/^[\x00-\x7f]*$/.test(h)) return h;
  try {
    return new URL(`http://${h}/`).hostname;
  } catch {
    return '';
  }
}

function normHost(host) {
  if (typeof host !== 'string') return '';
  let h = stripPort(host.trim().toLowerCase());
  if (h.endsWith('.')) h = h.slice(0, -1);
  return h;
}

/**
 * Public suffix of a hostname per the embedded Public Suffix List (ICANN + private sections), e.g.
 * "co.uk", "github.io", "duckdns.org". IPs and localhost yield ''. A ":port" suffix is ignored.
 * @param {string} host
 * @returns {string}
 */
export function publicSuffix(host) {
  let h = normHost(host);
  if (!h || isIp(h) || h === 'localhost') return '';
  h = toAscii(h);
  const labels = h.split('.');
  if (!h || labels.some((l) => l === '')) return '';
  return labels.slice(labels.length - Math.min(suffixLabelCount(labels), labels.length)).join('.');
}

/**
 * Registrable domain (eTLD+1) per the embedded Public Suffix List: the public suffix plus one label
 * ("www.bbc.co.uk" → "bbc.co.uk", "victim.github.io" → "victim.github.io", "a.tail1.ts.net" →
 * "tail1.ts.net"). A host that is itself a public suffix ("github.io", "co.uk"), an IP address, localhost
 * and single-label hosts are returned unchanged (lowercase, ASCII, trailing dot and ":port" removed).
 * @param {string} host hostname (a ":port" suffix is ignored)
 * @returns {string}
 */
export function registrableDomain(host) {
  let h = normHost(host);
  if (!h || isIp(h) || h === 'localhost') return h;
  h = toAscii(h);
  if (!h) return '';
  const labels = h.split('.');
  if (labels.some((l) => l === '')) return h;
  const n = labels.length;
  const ps = suffixLabelCount(labels);
  if (ps >= n) return h;
  return labels.slice(n - ps - 1).join('.');
}

function parseUrl(s, assumeScheme) {
  if (typeof s !== 'string') return null;
  let str = s.trim();
  if (!str || str.length > MAX_URL_LENGTH) return null;
  if (assumeScheme && !/^[a-z][a-z0-9+.-]*:\/\//i.test(str)) str = `https://${str}`;
  try {
    return new URL(str);
  } catch {
    return null;
  }
}

const isWeb = (u) => u.protocol === 'https:' || u.protocol === 'http:';

function hostKey(hostname) {
  let h = hostname.toLowerCase();
  if (h.endsWith('.')) h = h.slice(0, -1);
  return h;
}

/**
 * Does a saved item URI match the page URL under the given match mode?
 * @param {string} itemUri saved URI (scheme optional for domain/host modes; https:// is assumed)
 * @param {string} pageUrl full page URL
 * @param {'domain'|'host'|'starts_with'|'exact'|'regex'|'never'|null|undefined} [mode] default 'domain'
 * @returns {boolean}
 */
export function matchUri(itemUri, pageUrl, mode) {
  if (typeof itemUri !== 'string' || typeof pageUrl !== 'string' || !itemUri.trim()) return false;
  if (pageUrl.length > MAX_URL_LENGTH) return false;
  const m = mode == null || mode === '' ? 'domain' : mode;
  switch (m) {
    case 'never':
      return false;
    case 'exact':
      return itemUri === pageUrl;
    case 'starts_with': {
      if (!pageUrl.startsWith(itemUri)) return false;
      // An absolute http(s) prefix must not reach into another origin ("https://bank.com" vs
      // "https://bank.com.evil.net/", "https://bank.com:8443.evil.net").
      if (!/^https?:\/\//i.test(itemUri)) return true;
      const item = parseUrl(itemUri, false);
      const page = parseUrl(pageUrl, false);
      return !!item && !!page && item.origin === page.origin;
    }
    case 'regex': {
      if (itemUri.length > MAX_REGEX_LENGTH || NESTED_QUANTIFIER_RE.test(itemUri)) return false;
      try {
        return new RegExp(itemUri, 'i').test(pageUrl);
      } catch {
        return false;
      }
    }
    case 'host':
    case 'domain': {
      const page = parseUrl(pageUrl, false);
      const item = parseUrl(itemUri, true);
      if (!page || !item || !isWeb(page) || !isWeb(item)) return false;
      if (item.protocol === 'https:' && page.protocol !== 'https:') return false;
      const ph = hostKey(page.hostname);
      const ih = hostKey(item.hostname);
      if (!ph || !ih) return false;
      if (m === 'host' || isIp(ih) || ih === 'localhost' || !ih.includes('.')) {
        return ih === ph && item.port === page.port;
      }
      // A host that is itself a public suffix has itself as registrable domain, so it only matches itself.
      return registrableDomain(ih) === registrableDomain(ph);
    }
    default:
      return false;
  }
}
