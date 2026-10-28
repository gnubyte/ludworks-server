// Smart playlist rules (API.md §12 SmartRules) — the editor's model, pure and DOM-free: fields and
// operators, value kinds, client-side validation that mirrors the server (422 keys
// `rules.conditions[i].value`), conversion between the API shape and the editor draft, and a
// one-line summary. Unit tested in web/tests/music/smartrules.test.mjs.

export const MAX_CONDITIONS = 20;
export const MAX_LIMIT = 5000;
export const MAX_TEXT = 500;
export const MAX_DAYS = 36500;
/** What a smart playlist without a limit holds at most. */
export const UNLIMITED = 10000;

/** Rule fields: id → {label, kind, unit?}. */
export const FIELDS = Object.freeze({
  title: { label: 'Title', kind: 'text' },
  artist: { label: 'Artist', kind: 'text' },
  album: { label: 'Album', kind: 'text' },
  album_artist: { label: 'Album artist', kind: 'text' },
  genre: { label: 'Genre', kind: 'text' },
  composer: { label: 'Composer', kind: 'text' },
  path: { label: 'File path', kind: 'text' },
  codec: { label: 'Codec', kind: 'text' },
  subcategory: { label: 'Subcategory', kind: 'text' },
  year: { label: 'Year', kind: 'number' },
  duration_seconds: { label: 'Duration', kind: 'number', unit: 'duration' },
  play_count: { label: 'Play count', kind: 'number' },
  rating: { label: 'Rating', kind: 'number', unit: 'stars' },
  bitrate_kbps: { label: 'Bitrate (kbps)', kind: 'number' },
  added_at: { label: 'Date added', kind: 'date' },
  last_played_at: { label: 'Last played', kind: 'date' },
  favorite: { label: 'Favorite', kind: 'bool' },
  compilation: { label: 'Compilation', kind: 'bool' },
});

/** Field ids in menu order. */
export const FIELD_ORDER = Object.freeze(Object.keys(FIELDS));

/** Operators per value kind: [op, label]. */
export const OPS = Object.freeze({
  text: [['is', 'is'], ['is_not', 'is not'], ['contains', 'contains'], ['not_contains', 'does not contain'], ['starts_with', 'starts with']],
  number: [['eq', '='], ['ne', '≠'], ['gt', '>'], ['gte', '≥'], ['lt', '<'], ['lte', '≤'], ['between', 'is between']],
  date: [['in_last_days', 'in the last'], ['not_in_last_days', 'not in the last'], ['before', 'before'], ['after', 'after']],
  bool: [['is_true', 'is true'], ['is_false', 'is false']],
});

/** Sort fields of smart playlists (the GET /music/tracks sorts). */
export const SORTS = Object.freeze([
  ['title', 'Title'], ['artist', 'Artist'], ['album', 'Album'], ['added', 'Date added'], ['year', 'Year'],
  ['duration', 'Duration'], ['plays', 'Play count'], ['last_played', 'Last played'], ['random', 'Random (changes daily)'],
]);

/** Value kind of a field ('text' for unknown fields). */
export function kindOf(field) {
  return FIELDS[field]?.kind || 'text';
}

/** The operators of a field. */
export function opsFor(field) {
  return OPS[kindOf(field)];
}

/** What the value input of (field, op) looks like: 'text' | 'number' | 'range' | 'days' | 'date' | 'none'. */
export function valueKind(field, op) {
  const kind = kindOf(field);
  if (kind === 'bool') return 'none';
  if (kind === 'number') return op === 'between' ? 'range' : 'number';
  if (kind === 'date') return op === 'in_last_days' || op === 'not_in_last_days' ? 'days' : 'date';
  return 'text';
}

/**
 * Editor value for (field, op): text → '', number → '', range → ['', ''], days → '30',
 * date → '' (YYYY-MM-DD), none → null. Values in the draft are strings as typed.
 */
export function defaultValue(field, op) {
  switch (valueKind(field, op)) {
    case 'range': return ['', ''];
    case 'days': return '30';
    case 'none': return null;
    default: return '';
  }
}

/** A new condition for a field (first operator). */
export function newCondition(field = 'genre') {
  const f = FIELDS[field] ? field : 'genre';
  const op = opsFor(f)[0][0];
  return { field: f, op, value: defaultValue(f, op) };
}

/** Change a condition's field: keep the operator and value when the kind stays the same. */
export function withField(cond, field) {
  if (!FIELDS[field]) return cond;
  if (kindOf(field) === kindOf(cond.field)) return { ...cond, field };
  return newCondition(field);
}

/** Change a condition's operator: keep the value when its input looks the same. */
export function withOp(cond, op) {
  if (!opsFor(cond.field).some(([o]) => o === op)) return cond;
  const same = valueKind(cond.field, op) === valueKind(cond.field, cond.op);
  return { ...cond, op, value: same ? cond.value : defaultValue(cond.field, op) };
}

/** A new editor draft. */
export function newDraft() {
  return { match: 'all', conditions: [newCondition('genre')], sort: 'title', order: 'asc', limit: '' };
}

/* ------------------------------------------------------------ conversions */

/** Duration values are typed as m:ss (or seconds). */
function parseDuration(s) {
  const t = String(s ?? '').trim();
  if (/^\d+(\.\d+)?$/.test(t)) return Number(t);
  const m = /^(?:(\d+):)?(\d{1,2}):(\d{1,2})$/.exec(t);
  if (!m) return NaN;
  if (Number(m[3]) >= 60 || (m[1] !== undefined && Number(m[2]) >= 60)) return NaN;
  return Number(m[1] || 0) * 3600 + Number(m[2]) * 60 + Number(m[3]);
}

function formatDuration(sec) {
  const s = Math.max(0, Math.round(Number(sec) || 0));
  const h = Math.floor(s / 3600);
  const m = Math.floor((s % 3600) / 60);
  const r = s % 60;
  const pad = (x) => String(x).padStart(2, '0');
  return h ? `${h}:${pad(m)}:${pad(r)}` : `${m}:${pad(r)}`;
}

function numberOf(field, s) {
  if (FIELDS[field]?.unit === 'duration') return parseDuration(s);
  const t = String(s ?? '').trim();
  return t === '' ? NaN : Number(t);
}

function numberText(field, n) {
  if (n === null || n === undefined || !Number.isFinite(Number(n))) return '';
  return FIELDS[field]?.unit === 'duration' ? formatDuration(n) : String(n);
}

/** Local date (YYYY-MM-DD) of an RFC 3339 time. */
export function dateInputValue(iso) {
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  const pad = (x) => String(x).padStart(2, '0');
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

/** RFC 3339 time of local midnight of a YYYY-MM-DD date, or null. */
export function dateToIso(value) {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(String(value || ''));
  if (!m) return null;
  const d = new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3]));
  if (d.getFullYear() !== Number(m[1]) || d.getMonth() !== Number(m[2]) - 1 || d.getDate() !== Number(m[3])) return null;
  return d.toISOString();
}

/**
 * Editor draft from API rules (unknown fields/ops are kept as they are and flagged by validate).
 * @param {object|null} rules
 */
export function fromApi(rules) {
  if (!rules || typeof rules !== 'object') return newDraft();
  const conditions = (Array.isArray(rules.conditions) ? rules.conditions : []).map((c) => {
    const field = String(c?.field || '');
    const op = String(c?.op || '');
    let value;
    switch (valueKind(field, op)) {
      case 'range': value = Array.isArray(c.value) ? [numberText(field, c.value[0]), numberText(field, c.value[1])] : ['', '']; break;
      case 'number': value = numberText(field, c.value); break;
      case 'days': value = c.value === null || c.value === undefined ? '' : String(c.value); break;
      case 'date': value = typeof c.value === 'string' ? dateInputValue(c.value) : ''; break;
      case 'none': value = null; break;
      default: value = typeof c.value === 'string' ? c.value : '';
    }
    return { field, op, value };
  });
  return {
    match: rules.match === 'any' ? 'any' : 'all',
    conditions: conditions.length ? conditions : [newCondition('genre')],
    sort: SORTS.some(([s]) => s === rules.sort) ? rules.sort : 'title',
    order: rules.order === 'desc' ? 'desc' : 'asc',
    limit: Number.isInteger(rules.limit) ? String(rules.limit) : '',
  };
}

/**
 * Check a draft like the server does. Returns {} when valid, else messages keyed like the
 * server's 422 fields ("rules.conditions[0].value", "rules.limit", …).
 * @param {object} draft
 * @returns {Record<string, string>}
 */
export function validate(draft) {
  const errs = {};
  const conds = Array.isArray(draft?.conditions) ? draft.conditions : [];
  if (draft?.match !== 'all' && draft?.match !== 'any') errs['rules.match'] = 'Choose all or any.';
  if (conds.length < 1 || conds.length > MAX_CONDITIONS) errs['rules.conditions'] = `Use 1–${MAX_CONDITIONS} conditions.`;
  if (!SORTS.some(([s]) => s === draft?.sort)) errs['rules.sort'] = 'Choose a sort order.';
  const lim = String(draft?.limit ?? '').trim();
  if (lim !== '') {
    const n = Number(lim);
    if (!Number.isInteger(n) || n < 1 || n > MAX_LIMIT) errs['rules.limit'] = `Use a whole number from 1 to ${MAX_LIMIT}, or leave it empty.`;
  }
  conds.forEach((c, i) => {
    const key = `rules.conditions[${i}]`;
    if (!FIELDS[c?.field]) {
      errs[`${key}.field`] = 'Unknown field.';
      return;
    }
    if (!opsFor(c.field).some(([o]) => o === c.op)) {
      errs[`${key}.op`] = 'Choose a condition.';
      return;
    }
    const msg = valueProblem(c);
    if (msg) errs[`${key}.value`] = msg;
  });
  return errs;
}

function valueProblem(c) {
  const finite = (n) => Number.isFinite(n) && Math.abs(n) < 1e15;
  switch (valueKind(c.field, c.op)) {
    case 'text': {
      const s = String(c.value ?? '').trim();
      // eslint-disable-next-line no-control-regex
      if (/[\u0000-\u001f]/.test(s) || [...s].length > MAX_TEXT) return `Use a single line of at most ${MAX_TEXT} characters.`;
      if (!s && c.op !== 'is' && c.op !== 'is_not') return 'Enter some text.';
      return '';
    }
    case 'number': {
      const n = numberOf(c.field, c.value);
      if (!finite(n)) return FIELDS[c.field]?.unit === 'duration' ? 'Enter a duration such as 3:30.' : 'Enter a number.';
      return '';
    }
    case 'range': {
      const v = Array.isArray(c.value) ? c.value : [];
      const a = numberOf(c.field, v[0]);
      const b = numberOf(c.field, v[1]);
      if (!finite(a) || !finite(b)) return FIELDS[c.field]?.unit === 'duration' ? 'Enter two durations such as 2:00 and 5:00.' : 'Enter two numbers.';
      if (a > b) return 'The first value must not be larger than the second.';
      return '';
    }
    case 'days': {
      const n = Number(String(c.value ?? '').trim());
      if (!Number.isInteger(n) || n < 1 || n > MAX_DAYS) return `Enter a number of days from 1 to ${MAX_DAYS}.`;
      return '';
    }
    case 'date':
      return dateToIso(c.value) ? '' : 'Pick a date.';
    default:
      return '';
  }
}

/**
 * API rules from a valid draft (call validate first).
 * @param {object} draft
 */
export function toApi(draft) {
  const lim = String(draft.limit ?? '').trim();
  return {
    match: draft.match === 'any' ? 'any' : 'all',
    conditions: draft.conditions.map((c) => {
      let value;
      switch (valueKind(c.field, c.op)) {
        case 'range': value = [numberOf(c.field, c.value[0]), numberOf(c.field, c.value[1])]; break;
        case 'number': value = numberOf(c.field, c.value); break;
        case 'days': value = Number(String(c.value).trim()); break;
        case 'date': value = dateToIso(c.value); break;
        case 'none': value = null; break;
        default: value = String(c.value ?? '').trim();
      }
      return { field: c.field, op: c.op, value };
    }),
    sort: draft.sort,
    order: draft.order === 'desc' ? 'desc' : 'asc',
    limit: lim === '' ? null : Number(lim),
  };
}

/** The draft's condition index and part of a server 422 field key ("rules.conditions[2].value"). */
export function errorTarget(key) {
  const m = /^rules\.conditions\[(\d+)\](?:\.(\w+))?$/.exec(String(key || ''));
  if (m) return { index: Number(m[1]), part: m[2] || 'value' };
  const g = /^rules\.(\w+)$/.exec(String(key || ''));
  return g ? { index: -1, part: g[1] } : { index: -1, part: 'rules' };
}

function describeValue(c) {
  switch (valueKind(c.field, c.op)) {
    case 'range': return `${numberText(c.field, c.value?.[0])} and ${numberText(c.field, c.value?.[1])}`;
    case 'number': return numberText(c.field, c.value);
    case 'days': return `${c.value} days`;
    case 'date': return typeof c.value === 'string' ? (c.value.length > 10 ? dateInputValue(c.value) : c.value) : '';
    case 'none': return '';
    default: return `“${c.value ?? ''}”`;
  }
}

/**
 * One-line summary of API rules: "Genre contains “rock” and Year is between 1990 and 1999 ·
 * by year, newest first · up to 100 songs".
 * @param {object|null} rules
 */
export function describe(rules) {
  if (!rules || !Array.isArray(rules.conditions)) return '';
  const parts = rules.conditions.map((c) => {
    const f = FIELDS[c.field]?.label || c.field;
    const opLabel = (OPS[kindOf(c.field)] || []).find(([o]) => o === c.op)?.[1] || c.op;
    return [f, opLabel, describeValue(c)].filter(Boolean).join(' ');
  });
  const joined = parts.join(rules.match === 'any' ? ' or ' : ' and ');
  const sortLabel = (SORTS.find(([s]) => s === rules.sort)?.[1] || rules.sort || 'title').toLowerCase();
  const order = rules.order === 'desc' ? ', descending' : '';
  const sorted = rules.sort === 'random' ? 'in random order' : `by ${sortLabel}${order}`;
  const limit = Number.isInteger(rules.limit) ? ` · up to ${rules.limit.toLocaleString()} songs` : '';
  return `${joined} · ${sorted}${limit}`;
}
