// Pure logic for the Notes app (no DOM): folder trees, list ordering and date groups,
// search-highlight segments, word counts, preferences and the autosave state machine.
// Everything here is unit-tested under Node (web/tests/notes).

import { extractTitle, toPlainText } from '../../lib/markdown.js';

/** Title shown for notes whose derived title is empty (Apple Notes says "New Note"). */
export const UNTITLED = 'New Note';

/** True when Markdown has no visible content (an untouched new note). */
export function blankMarkdown(md) {
  return !String(md || '').replace(/&nbsp;| /g, '').trim();
}

/** Title derived from the first line, like the server does (images never make a title). */
export function derivedTitle(md) {
  return extractTitle(String(md || '').slice(0, 16384).replace(/!\[[^\]\n]*\]\([^)\n]*\)/g, ''));
}

/**
 * The API does not say whether a title is explicit (import front matter, mail subject); a
 * title that differs from the derived one must be explicit.
 */
export function hasExplicitTitle(note) {
  return !!note?.title && note.title.trim() !== derivedTitle(note.body).trim();
}

/** Client-side preview of the server's title/snippet rule while typing. */
export function previewSummary(note, md, explicit = false) {
  const title = explicit ? note.title : derivedTitle(md);
  let snippet = toPlainText(md, 400);
  if (!explicit && title && snippet.startsWith(title)) snippet = snippet.slice(title.length).trim();
  if ([...snippet].length > 200) snippet = `${[...snippet].slice(0, 199).join('').trimEnd()}…`;
  return { title, snippet };
}

/** Display title of a note summary. */
export function displayTitle(note) {
  const t = (note && note.title) || '';
  return t.trim() ? t : UNTITLED;
}

/* ------------------------------------------------------------------ folders */

/**
 * Build a tree from the flat folder list of GET /notes/folders.
 * @param {Array<{id: string, name: string, parent_id: string|null, position: number, note_count: number}>} folders
 * @returns {{roots: object[], byId: Map<string, object>}} nodes are {…folder, children, depth, total}
 *   (total = notes in the folder and all of its descendants). Orphans (missing parent) become roots.
 */
export function buildFolderTree(folders) {
  const byId = new Map();
  for (const f of folders || []) byId.set(f.id, { ...f, children: [], depth: 0, total: f.note_count || 0 });
  const roots = [];
  for (const node of byId.values()) {
    const parent = node.parent_id ? byId.get(node.parent_id) : null;
    if (parent && parent !== node) parent.children.push(node);
    else roots.push(node);
  }
  const cmp = (a, b) => (a.position - b.position) || a.name.localeCompare(b.name, undefined, { sensitivity: 'base' }) || (a.id < b.id ? -1 : 1);
  const seen = new Set();
  const walk = (list, depth) => {
    list.sort(cmp);
    let sum = 0;
    for (const n of list) {
      if (seen.has(n.id)) continue; // defensive: cycles never come from the server
      seen.add(n.id);
      n.depth = depth;
      n.total = (n.note_count || 0) + walk(n.children, depth + 1);
      sum += n.total;
    }
    return sum;
  };
  walk(roots, 0);
  return { roots, byId };
}

/**
 * Depth-first flattening of a tree, skipping the children of collapsed folders.
 * @param {object[]} roots
 * @param {Set<string>} [collapsed]
 */
export function flattenTree(roots, collapsed = new Set()) {
  const out = [];
  const walk = (list) => {
    for (const n of list) {
      out.push(n);
      if (n.children.length && !collapsed.has(n.id)) walk(n.children);
    }
  };
  walk(roots);
  return out;
}

/** True when `id` is `ancestorId` or one of its descendants. */
export function isWithin(byId, id, ancestorId) {
  let cur = byId.get(id);
  const seen = new Set();
  while (cur && !seen.has(cur.id)) {
    if (cur.id === ancestorId) return true;
    seen.add(cur.id);
    cur = cur.parent_id ? byId.get(cur.parent_id) : null;
  }
  return false;
}

/** "Work › Projects" style path of a folder (names from the root). */
export function folderPath(byId, id) {
  const names = [];
  let cur = byId.get(id);
  const seen = new Set();
  while (cur && !seen.has(cur.id)) {
    names.unshift(cur.name);
    seen.add(cur.id);
    cur = cur.parent_id ? byId.get(cur.parent_id) : null;
  }
  return names.join(' › ');
}

/* -------------------------------------------------------------------- views */

/**
 * A list view is what the folders pane selects. Parsed from / serialised to the URL query.
 * kinds: all | none (top-level "Notes") | folder | tag | pinned | trash
 */
export function viewFromQuery(query = {}) {
  if (query.v === 'trash') return { kind: 'trash' };
  if (query.v === 'pinned') return { kind: 'pinned' };
  if (query.tag) return { kind: 'tag', tag: String(query.tag) };
  if (query.f === 'none') return { kind: 'none' };
  if (query.f && /^[A-Za-z0-9_-]{8,64}$/.test(query.f)) return { kind: 'folder', id: query.f };
  return { kind: 'all' };
}

/** Query params for a view (null values remove keys). */
export function viewToQuery(view) {
  const q = { v: null, f: null, tag: null };
  switch (view.kind) {
    case 'trash': q.v = 'trash'; break;
    case 'pinned': q.v = 'pinned'; break;
    case 'tag': q.tag = view.tag; break;
    case 'none': q.f = 'none'; break;
    case 'folder': q.f = view.id; break;
    default: break;
  }
  return q;
}

export function sameView(a, b) {
  return !!a && !!b && a.kind === b.kind && (a.id || '') === (b.id || '') && (a.tag || '') === (b.tag || '');
}

/** API query for GET /notes given a view, search text and sort. */
export function listParams(view, { q = '', sort = 'updated', order = '' } = {}) {
  const p = {};
  const searching = q.trim() !== '';
  if (view.kind === 'trash') p.trashed = 1;
  if (!searching) {
    if (view.kind === 'folder') p.folder_id = view.id;
    if (view.kind === 'none') p.folder_id = 'none';
    if (view.kind === 'pinned') p.pinned = 1;
    if (view.kind === 'tag') p.tag = view.tag;
  }
  if (searching) p.q = q.trim();
  else {
    p.sort = sort;
    if (order) p.order = order;
  }
  return p;
}

/** Default order for a sort key (matches the server). */
export function defaultOrder(sort) {
  return sort === 'title' ? 'asc' : 'desc';
}

/* ------------------------------------------------------------- list order */

/**
 * Comparator matching the server's ordering: pinned first, then the sort key, then id.
 * @param {'updated'|'created'|'title'} sort
 * @param {'asc'|'desc'} order
 */
export function compareNotes(sort, order) {
  const dir = order === 'asc' ? 1 : -1;
  const key = sort === 'created' ? (n) => n.created_at : sort === 'title' ? (n) => (n.title || '').trim().toLowerCase() : (n) => n.updated_at;
  return (a, b) => {
    if (!!a.pinned !== !!b.pinned) return a.pinned ? -1 : 1;
    const ka = key(a);
    const kb = key(b);
    if (ka !== kb) return (ka < kb ? -1 : 1) * dir;
    if (a.id === b.id) return 0;
    return (a.id < b.id ? -1 : 1) * dir;
  };
}

/**
 * Insert or move `note` within an already ordered list (returns a new array). When the list
 * is only partially loaded and the note would land after the last loaded item, it is left out
 * (it will arrive with a later page) unless `complete` is true.
 */
export function upsertSorted(items, note, cmp, { complete = false } = {}) {
  const rest = items.filter((n) => n.id !== note.id);
  let i = rest.findIndex((n) => cmp(note, n) < 0);
  if (i < 0) {
    if (!complete) return rest;
    i = rest.length;
  }
  rest.splice(i, 0, note);
  return rest;
}

/** Remove notes by id (returns a new array). */
export function removeIds(items, ids) {
  const set = new Set(ids);
  return items.filter((n) => !set.has(n.id));
}

/** The id to select after removing `ids` from `items` (next item, else previous, else null). */
export function neighbourAfterRemoval(items, ids, currentId) {
  const set = new Set(ids);
  const idx = items.findIndex((n) => n.id === currentId);
  if (idx < 0) return null;
  for (let i = idx + 1; i < items.length; i++) if (!set.has(items[i].id)) return items[i].id;
  for (let i = idx - 1; i >= 0; i--) if (!set.has(items[i].id)) return items[i].id;
  return null;
}

/* ------------------------------------------------------------- date groups */

function startOfDay(d) {
  return new Date(d.getFullYear(), d.getMonth(), d.getDate());
}

/**
 * Apple-Notes-style section for a timestamp: today, yesterday, previous 7/30 days, month
 * (this year) or year.
 * @returns {{key: string, label: string}}
 */
export function dateGroup(value, now = new Date()) {
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return { key: 'unknown', label: 'Earlier' };
  const today = startOfDay(now).getTime();
  const day = startOfDay(d).getTime();
  const days = Math.round((today - day) / 86400000);
  if (days <= 0) return { key: 'today', label: 'Today' };
  if (days === 1) return { key: 'yesterday', label: 'Yesterday' };
  if (days < 7) return { key: 'week', label: 'Previous 7 Days' };
  if (days < 30) return { key: 'month', label: 'Previous 30 Days' };
  if (d.getFullYear() === now.getFullYear()) {
    return { key: `m${d.getMonth()}`, label: new Intl.DateTimeFormat(undefined, { month: 'long' }).format(d) };
  }
  return { key: `y${d.getFullYear()}`, label: String(d.getFullYear()) };
}

/**
 * Split a list into sections for display.
 * @param {object[]} items ordered notes
 * @param {{sort: string, searching: boolean, now?: Date}} o
 * @returns {Array<{key: string, label: string, items: object[]}>}
 */
export function groupNotes(items, { sort, searching, now = new Date() }) {
  const sections = [];
  const push = (key, label, n) => {
    const last = sections[sections.length - 1];
    if (last && last.key === key) last.items.push(n);
    else sections.push({ key, label, items: [n] });
  };
  for (const n of items) {
    if (searching) push('results', 'Top Results', n);
    else if (n.pinned) push('pinned', 'Pinned', n);
    else if (sort === 'title') push('notes', 'Notes', n);
    else {
      const g = dateGroup(sort === 'created' ? n.created_at : n.updated_at, now);
      push(g.key, g.label, n);
    }
  }
  return sections;
}

/**
 * Short date for a list row: time today, "Yesterday", weekday within a week, else a date.
 */
export function listDate(value, now = new Date()) {
  const d = new Date(value);
  if (Number.isNaN(d.getTime())) return '';
  const days = Math.round((startOfDay(now).getTime() - startOfDay(d).getTime()) / 86400000);
  if (days <= 0) return new Intl.DateTimeFormat(undefined, { hour: 'numeric', minute: '2-digit' }).format(d);
  if (days === 1) return 'Yesterday';
  if (days < 7) return new Intl.DateTimeFormat(undefined, { weekday: 'long' }).format(d);
  const opts = d.getFullYear() === now.getFullYear() ? { month: 'short', day: 'numeric' } : { year: 'numeric', month: 'short', day: 'numeric' };
  return new Intl.DateTimeFormat(undefined, opts).format(d);
}

/* --------------------------------------------------------------- highlights */

/**
 * Split text into plain and highlighted segments from [start, end) code-point ranges (as the
 * server reports them). Overlapping/out-of-range ranges are clipped.
 * @param {string} text
 * @param {Array<[number, number]>} ranges
 * @returns {Array<{text: string, hit: boolean}>}
 */
export function highlightSegments(text, ranges) {
  const cps = Array.from(text || '');
  const sorted = (ranges || [])
    .filter((r) => Array.isArray(r) && r.length === 2 && Number.isInteger(r[0]) && Number.isInteger(r[1]))
    .map(([s, e]) => [Math.max(0, s), Math.min(cps.length, e)])
    .filter(([s, e]) => e > s)
    .sort((a, b) => a[0] - b[0]);
  const out = [];
  let pos = 0;
  for (let [s, e] of sorted) {
    if (s < pos) s = pos;
    if (e <= s) continue;
    if (s > pos) out.push({ text: cps.slice(pos, s).join(''), hit: false });
    out.push({ text: cps.slice(s, e).join(''), hit: true });
    pos = e;
  }
  if (pos < cps.length || out.length === 0) out.push({ text: cps.slice(pos).join(''), hit: false });
  return out.filter((seg) => seg.text !== '' || out.length === 1);
}

/* ------------------------------------------------------------------ counts */

/** Rough word and character counts of Markdown (syntax characters are not words). */
export function countWords(md) {
  const text = String(md || '')
    .replace(/```[^\n]*\n/g, '\n')
    .replace(/!\[([^\]]*)\]\([^)]*\)/g, '$1')
    .replace(/\[([^\]]*)\]\([^)]*\)/g, '$1')
    .replace(/^\s{0,3}(#{1,6}|>|[-*+]|\d+[.)])\s+(\[[ xX]\]\s+)?/gm, '')
    .replace(/[*_~`|]/g, ' ');
  const words = text.match(/[\p{L}\p{N}][\p{L}\p{N}'’.-]*/gu) || [];
  const chars = Array.from(String(md || '').replace(/\s/g, '')).length;
  return { words: words.length, chars };
}

/* ------------------------------------------------------------- preferences */

/** localStorage key for the per-user editor mode preference (non-sensitive). */
export function prefKey(userId, name) {
  return `lw.notes.${name}.${userId || 'anon'}`;
}

/** Read a preference with a whitelist and default (storage may be unavailable). */
export function readPref(storage, key, allowed, fallback) {
  try {
    const v = storage?.getItem(key);
    return allowed.includes(v) ? v : fallback;
  } catch {
    return fallback;
  }
}

export function writePref(storage, key, value) {
  try {
    storage?.setItem(key, value);
  } catch {
    /* storage full or blocked: preference is not essential */
  }
}

/* ------------------------------------------------------------- import file */

/** Classify files dropped on the import dialog. */
export function classifyImportFile(file) {
  const name = String(file?.name || '').toLowerCase();
  if (name.endsWith('.zip')) return 'zip';
  if (/\.(md|markdown|txt)$/.test(name)) return 'markdown';
  return 'unsupported';
}

/* ---------------------------------------------------------------- autosave */

/**
 * Autosave state machine for one open note. It debounces edits, serialises PATCH requests
 * (so a note never conflicts with itself), carries base_version for optimistic concurrency,
 * merges metadata changes into the next request, retries transient failures with backoff and
 * reports conflicts.
 *
 * @param {{
 *   note: {id: string, version: number, body: string},
 *   send: (id: string, patch: object) => Promise<object>,   // resolves with the server Note
 *   delay?: number,
 *   onState?: (state: 'saved'|'dirty'|'saving'|'error'|'conflict'|'gone') => void,
 *   onSaved?: (note: object) => void,
 *   onConflict?: (info: {current: object, mine: object}) => void,
 *   onGone?: () => void,
 *   timers?: {setTimeout: Function, clearTimeout: Function},
 *   isConflict?: (err) => boolean, isGone?: (err) => boolean, isRetryable?: (err) => boolean,
 * }} o
 */
export function createSaver(o) {
  const timers = o.timers || { setTimeout: (...a) => globalThis.setTimeout(...a), clearTimeout: (id) => globalThis.clearTimeout(id) };
  const delay = o.delay ?? 800;
  const isConflict = o.isConflict || ((e) => e?.status === 409);
  const isGone = o.isGone || ((e) => e?.status === 404);
  const isRetryable = o.isRetryable || ((e) => !e?.status || e.status === 0 || e.status === 429 || e.status >= 500);
  let version = o.note.version;
  let savedBody = o.note.body ?? '';
  let pending = {}; // fields not yet sent
  let timer = null;
  let inflight = null;
  let state = 'saved';
  let retries = 0;
  let stopped = false;
  let paused = false; // conflict/gone: wait for the user

  const setState = (s) => {
    if (s === state) return;
    state = s;
    o.onState?.(s);
  };
  const hasPending = () => Object.keys(pending).length > 0;
  const arm = (ms) => {
    timers.clearTimeout(timer);
    timer = timers.setTimeout(() => { timer = null; run(); }, ms);
  };

  async function run() {
    if (stopped || paused) return;
    if (inflight) {
      await inflight.catch(() => {});
      if (hasPending()) return run();
      return;
    }
    if (!hasPending()) {
      setState('saved');
      return;
    }
    const fields = { ...pending };
    const patch = { ...fields, base_version: version };
    const sentBody = Object.prototype.hasOwnProperty.call(pending, 'body') ? pending.body : undefined;
    pending = {};
    setState('saving');
    inflight = (async () => {
      try {
        const note = await o.send(o.note.id, patch);
        retries = 0;
        version = note.version;
        if (typeof note.body === 'string') savedBody = note.body;
        else if (sentBody !== undefined) savedBody = sentBody;
        o.onSaved?.(note);
        setState(hasPending() ? 'dirty' : 'saved');
      } catch (err) {
        // Put the unsent fields back underneath newer edits.
        pending = { ...fields, ...pending };
        if (isConflict(err)) {
          const current = err?.details?.current;
          if (current && Number.isInteger(current.version) && typeof current.body === 'string' && current.body === savedBody && !('title' in fields)) {
            // The text on the server is still what we based our edit on (only metadata such
            // as pin/tags changed elsewhere): nothing can be lost, so retry on the new version.
            version = current.version;
            retries = 0;
            arm(0);
            return;
          }
          paused = true;
          setState('conflict');
          o.onConflict?.({ current, mine: { ...pending } });
        } else if (isGone(err)) {
          paused = true;
          setState('gone');
          o.onGone?.();
        } else if (isRetryable(err) && !stopped) {
          retries += 1;
          setState('error');
          arm(Math.min(30000, 1000 * 2 ** Math.min(retries, 5)));
        } else {
          setState('error');
          throw err;
        }
      } finally {
        inflight = null;
      }
    })();
    try {
      await inflight;
    } catch (err) {
      o.onError?.(err);
    }
  }

  return {
    /** Record a body edit (debounced). */
    edit(body) {
      if (stopped) return;
      if (body === savedBody && !('body' in pending)) return;
      pending.body = body;
      if (!paused) {
        setState('dirty');
        arm(delay);
      }
    },
    /** Record metadata changes and save them right away (with any pending body). */
    update(fields) {
      if (stopped) return Promise.resolve();
      Object.assign(pending, fields);
      if (paused) return Promise.resolve();
      timers.clearTimeout(timer);
      timer = null;
      return run();
    },
    /** Save everything now; resolves when nothing is pending or saving is blocked. */
    async flush() {
      timers.clearTimeout(timer);
      timer = null;
      for (let i = 0; i < 5 && !paused && !stopped && (hasPending() || inflight); i++) {
        if (inflight) await inflight.catch(() => {});
        else await run();
        if (state === 'error') break;
      }
      return !hasPending() && !inflight;
    },
    /** Resolve a conflict: continue from the given server version with `pending` as-is or replaced. */
    resolve({ version: v, body, keepPending = true }) {
      version = v;
      if (typeof body === 'string') savedBody = body;
      if (!keepPending) pending = {};
      paused = false;
      retries = 0;
      if (hasPending()) {
        setState('dirty');
        arm(0);
      } else setState('saved');
    },
    /** Stop timers (the caller flushes first if it wants to keep edits). */
    stop() {
      stopped = true;
      timers.clearTimeout(timer);
      timer = null;
    },
    get dirty() { return hasPending() || !!inflight; },
    get state() { return state; },
    get version() { return version; },
    get savedBody() { return savedBody; },
    get pending() { return { ...pending }; },
  };
}
