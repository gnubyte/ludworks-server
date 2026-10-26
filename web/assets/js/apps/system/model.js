// Pure helpers for the Storage app (no DOM), shared with the node tests.

/** Usage fraction at which a disk or quota is flagged as low / critical. */
export const WARN_AT = 0.85;
export const CRITICAL_AT = 0.95;

/**
 * Numbers for one volume of GET /system/storage. `used_bytes` may be less than
 * `total_bytes - free_bytes` (blocks reserved for the superuser), so the fraction uses
 * used / total and falls back to total - free when the server sent no used value.
 * @param {{total_bytes?: number, free_bytes?: number, used_bytes?: number}} v
 * @returns {{total: number, free: number, used: number, fraction: number}}
 */
export function volumeUsage(v) {
  const total = Math.max(0, Number(v?.total_bytes) || 0);
  const free = Math.max(0, Number(v?.free_bytes) || 0);
  const rawUsed = Number(v?.used_bytes);
  const used = Number.isFinite(rawUsed) && rawUsed > 0 ? rawUsed : Math.max(0, total - free);
  const fraction = total > 0 ? Math.min(1, used / total) : 0;
  return { total, free, used, fraction };
}

/** The fullest volume's usage fraction (0 when there are none). */
export function worstVolumeFraction(volumes) {
  return (volumes || []).reduce((m, v) => Math.max(m, volumeUsage(v).fraction), 0);
}

/** 'ok' | 'warning' | 'danger' for a usage fraction. */
export function usageLevel(fraction) {
  if (fraction >= CRITICAL_AT) return 'danger';
  if (fraction >= WARN_AT) return 'warning';
  return 'ok';
}

const LABEL_NAMES = { data: 'Data', storage: 'Storage', library: 'Library' };

/**
 * Human title for a volume label from the server ("data, storage, library: Movies +2" →
 * "Data · Storage · Movies +2 more").
 * @param {string} label
 * @returns {string}
 */
export function volumeTitle(label) {
  let text = String(label || '').trim();
  if (!text) return 'Disk';
  let more = '';
  const m = /\s\+(\d+)$/.exec(text);
  if (m) {
    more = ` +${m[1]} more`;
    text = text.slice(0, m.index);
  }
  const parts = text.split(', ').map((p) => {
    const t = p.trim();
    if (/^library:/i.test(t)) return t.slice(8).trim() || 'Library';
    return LABEL_NAMES[t.toLowerCase()] || t;
  }).filter(Boolean);
  return (parts.join(' · ') || 'Disk') + more;
}

/**
 * Spaces for display: home first, then by usage (largest first), then by name.
 * @param {Array<{kind: string, name?: string, usage_bytes?: number}>} spaces
 */
export function sortSpaces(spaces) {
  return (spaces || []).slice().sort((a, b) => {
    if ((a.kind === 'home') !== (b.kind === 'home')) return a.kind === 'home' ? -1 : 1;
    const d = (Number(b.usage_bytes) || 0) - (Number(a.usage_bytes) || 0);
    if (d !== 0) return d;
    return String(a.name || '').localeCompare(String(b.name || ''));
  });
}

/** Total bytes over the per-user table. */
export function usersTotal(users) {
  return (users || []).reduce((sum, u) => sum + (Number(u.usage_bytes) || 0), 0);
}

/**
 * Quota summary for the caller.
 * @param {{usage_bytes?: number, quota_bytes?: number}} user
 * @returns {{used: number, quota: number, left: number, fraction: number, level: string}}
 */
export function quotaState(user) {
  const used = Math.max(0, Number(user?.usage_bytes) || 0);
  const quota = Math.max(0, Number(user?.quota_bytes) || 0);
  const fraction = quota > 0 ? used / quota : 0;
  return { used, quota, left: Math.max(0, quota - used), fraction, level: quota > 0 ? usageLevel(fraction) : 'ok' };
}
