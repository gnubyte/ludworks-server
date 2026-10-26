// Pure formatting helpers (no DOM) — unit tested under node.

const BYTE_UNITS = ['B', 'KiB', 'MiB', 'GiB', 'TiB', 'PiB', 'EiB'];

/**
 * Human-readable byte size using binary (1024) units.
 * @param {number} bytes
 * @param {{decimals?: number}} [opts]
 * @returns {string} e.g. "1.5 GiB"
 */
export function formatBytes(bytes, { decimals } = {}) {
  const n = Number(bytes);
  if (!Number.isFinite(n)) return '—';
  const sign = n < 0 ? '-' : '';
  let v = Math.abs(n);
  if (v < 1024) return `${sign}${Math.round(v)} B`;
  let i = 0;
  while (v >= 1024 && i < BYTE_UNITS.length - 1) {
    v /= 1024;
    i += 1;
  }
  const d = decimals ?? (v >= 100 ? 0 : v >= 10 ? 1 : 2);
  // Trim trailing zeros ("2.00 GiB" → "2 GiB") for a calmer UI.
  const str = v.toFixed(d).replace(/\.0+$|(\.\d*?[1-9])0+$/, '$1');
  return `${sign}${str} ${BYTE_UNITS[i]}`;
}

const PARSE_UNITS = { b: 0, k: 1, m: 2, g: 3, t: 4, p: 5, e: 6 };

/**
 * Parse a byte size such as "500 GB", "1.5TiB", "2048", "10m".
 * Units are binary (1 GB = 1 GiB = 1024^3) to match formatBytes.
 * @param {string|number} input
 * @returns {number|null} integer bytes, or null when unparsable
 */
export function parseBytes(input) {
  if (typeof input === 'number') return Number.isFinite(input) && input >= 0 ? Math.round(input) : null;
  const m = /^\s*(\d+(?:\.\d+)?)\s*([kmgtpe]?)(i?b?)\s*$/i.exec(String(input ?? ''));
  if (!m) return null;
  const unit = (m[2] || 'b').toLowerCase();
  if (!m[2] && m[3] && m[3].toLowerCase() !== 'b') return null;
  const value = Number(m[1]) * 1024 ** PARSE_UNITS[unit];
  return Number.isFinite(value) && value <= Number.MAX_SAFE_INTEGER ? Math.round(value) : null;
}

/**
 * Clock-style duration: "1:02:03", "2:03" ("0:00" for invalid/negative).
 * @param {number} seconds
 * @param {{alwaysHours?: boolean}} [opts]
 */
export function formatDuration(seconds, { alwaysHours = false } = {}) {
  let t = Math.floor(Number(seconds));
  if (!Number.isFinite(t) || t < 0) t = 0;
  const h = Math.floor(t / 3600);
  const m = Math.floor((t % 3600) / 60);
  const sec = t % 60;
  const pad = (x) => String(x).padStart(2, '0');
  if (h > 0 || alwaysHours) return `${h}:${pad(m)}:${pad(sec)}`;
  return `${m}:${pad(sec)}`;
}

/**
 * Compact long-form duration: "3d 4h", "2h 5m", "45s".
 * @param {number} seconds
 * @param {number} [parts] max number of unit parts
 */
export function formatDurationLong(seconds, parts = 2) {
  let t = Math.floor(Number(seconds));
  if (!Number.isFinite(t) || t < 0) t = 0;
  if (t === 0) return '0s';
  const units = [['d', 86400], ['h', 3600], ['m', 60], ['s', 1]];
  const out = [];
  for (const [label, size] of units) {
    if (t >= size) {
      out.push(`${Math.floor(t / size)}${label}`);
      t %= size;
    }
    if (out.length >= parts) break;
  }
  return out.join(' ');
}

function toDate(v) {
  if (v instanceof Date) return v;
  if (v == null || v === '') return null;
  const d = new Date(v);
  return Number.isNaN(d.getTime()) ? null : d;
}

/**
 * Locale date: "Oct 26, 2026".
 * @param {string|Date} value
 * @param {Intl.DateTimeFormatOptions} [opts]
 */
export function formatDate(value, opts = { year: 'numeric', month: 'short', day: 'numeric' }) {
  const d = toDate(value);
  return d ? new Intl.DateTimeFormat(undefined, opts).format(d) : '—';
}

/** Locale date + time: "Oct 26, 2026, 14:03". */
export function formatDateTime(value) {
  return formatDate(value, { year: 'numeric', month: 'short', day: 'numeric', hour: '2-digit', minute: '2-digit' });
}

/** Locale time: "14:03". */
export function formatTime(value) {
  return formatDate(value, { hour: '2-digit', minute: '2-digit' });
}

/**
 * Relative time: "just now", "5 minutes ago", "in 3 days".
 * @param {string|Date} value
 * @param {Date|number} [now]
 */
export function formatRelative(value, now = Date.now()) {
  const d = toDate(value);
  if (!d) return '—';
  const diff = (d.getTime() - Number(now)) / 1000;
  const abs = Math.abs(diff);
  if (abs < 45) return 'just now';
  const rtf = new Intl.RelativeTimeFormat(undefined, { numeric: 'auto' });
  const table = [
    ['minute', 60, 3600],
    ['hour', 3600, 86400],
    ['day', 86400, 86400 * 7],
    ['week', 86400 * 7, 86400 * 30],
    ['month', 86400 * 30, 86400 * 365],
    ['year', 86400 * 365, Infinity],
  ];
  for (const [unit, size, limit] of table) {
    if (abs < limit) return rtf.format(Math.round(diff / size), unit);
  }
  return formatDate(d);
}

/** Locale number with grouping. */
export function formatNumber(n, opts) {
  const v = Number(n);
  return Number.isFinite(v) ? new Intl.NumberFormat(undefined, opts).format(v) : '—';
}

/** Percentage of part/total, e.g. "42%" (or "0%" when total is 0). */
export function formatPercent(part, total, decimals = 0) {
  const p = total > 0 ? (part / total) * 100 : 0;
  return `${p.toFixed(decimals)}%`;
}

/** "1 file" / "3 files". */
export function plural(n, singular, pluralForm = `${singular}s`) {
  return `${formatNumber(n)} ${n === 1 ? singular : pluralForm}`;
}

/** Bytes per second → "12.3 MiB/s". */
export function formatSpeed(bps) {
  return `${formatBytes(bps)}/s`;
}

/**
 * Initials for an avatar: "Ada Lovelace" → "AL", "root" → "RO", "GitHub (work)" → "GW".
 * Words are runs of letters and digits in any script, so punctuation never becomes an
 * initial; characters are taken whole (no split surrogate pairs).
 * @param {string} name
 */
export function initials(name) {
  const parts = String(name ?? '').split(/[^\p{L}\p{N}]+/u).filter(Boolean);
  if (parts.length === 0) return '?';
  const chars = (w) => Array.from(w);
  if (parts.length === 1) return chars(parts[0]).slice(0, 2).join('').toUpperCase();
  return (chars(parts[0])[0] + chars(parts[parts.length - 1])[0]).toUpperCase();
}

/** Stable hue (0-359) derived from a string, for avatar/tag colours. */
export function hueFor(str) {
  let h = 0;
  for (const ch of String(str || '')) h = (h * 31 + ch.codePointAt(0)) >>> 0;
  return h % 360;
}

/** Group a SHA-256 hex fingerprint as "AB:CD:..." for display. */
export function formatFingerprint(hex) {
  const clean = String(hex || '').replace(/[^0-9a-f]/gi, '').toUpperCase();
  return clean.match(/.{1,2}/g)?.join(':') ?? '';
}
