// Pure logic of the Kanban app (no DOM): routes, label palette, positions and moves (the
// server's dense-ordering rules, API.md §13), drag-and-drop targets, card filters, due dates
// and their buckets, all-day conversion and merging polled boards. Unit tested under node
// (web/tests/kanban/model.test.mjs).

const ID_RE = /^[A-Za-z0-9_-]{8,64}$/;

/** True for a syntactically valid API id. */
export const isId = (v) => typeof v === 'string' && ID_RE.test(v);

/* ------------------------------------------------------------------ routes */

/**
 * Parse the path after /kanban: '' → boards, 'my' → my cards, '<board>' → a board,
 * '<board>/c/<card>' → a card over its board. Anything else → boards.
 * @param {string} rest
 * @returns {{view: 'boards'|'my'|'board', boardId?: string, cardId?: string|null}}
 */
export function parseRoute(rest) {
  const parts = String(rest || '').split('/').filter(Boolean);
  if (parts.length === 0) return { view: 'boards' };
  if (parts.length === 1 && parts[0] === 'my') return { view: 'my' };
  if (isId(parts[0])) {
    if (parts.length === 1) return { view: 'board', boardId: parts[0], cardId: null };
    if (parts.length === 3 && parts[1] === 'c' && isId(parts[2])) return { view: 'board', boardId: parts[0], cardId: parts[2] };
  }
  return { view: 'boards' };
}

/** App paths. */
export const paths = {
  boards: () => '/kanban',
  my: () => '/kanban/my',
  board: (id) => `/kanban/${encodeURIComponent(id)}`,
  card: (boardId, cardId) => `/kanban/${encodeURIComponent(boardId)}/c/${encodeURIComponent(cardId)}`,
};

/* ----------------------------------------------------------------- palette */

/** Label, board and column colors (server enum order) with display names. */
export const COLORS = ['gray', 'red', 'orange', 'amber', 'yellow', 'green', 'teal', 'blue', 'indigo', 'purple', 'pink'];
export const COLOR_NAMES = {
  gray: 'Gray', red: 'Red', orange: 'Orange', amber: 'Amber', yellow: 'Yellow', green: 'Green',
  teal: 'Teal', blue: 'Blue', indigo: 'Indigo', purple: 'Purple', pink: 'Pink',
};
export const isColor = (c) => COLORS.includes(c);

/** CSS class carrying a palette color (unknown colors fall back to gray). */
export const colorClass = (c) => `kb-c-${isColor(c) ? c : 'gray'}`;

/**
 * Color for a new label: the least used palette color (gray last), ties in palette order.
 * @param {Array<{color: string}>} labels
 */
export function pickLabelColor(labels) {
  const used = new Map(COLORS.map((c) => [c, 0]));
  for (const l of labels || []) if (used.has(l.color)) used.set(l.color, used.get(l.color) + 1);
  let best = null;
  for (const c of [...COLORS.slice(1), 'gray']) {
    if (best === null || used.get(c) < used.get(best)) best = c;
  }
  return best;
}

/** Priorities, highest first, with labels. */
export const PRIORITIES = [
  { value: 'urgent', label: 'Urgent' },
  { value: 'high', label: 'High' },
  { value: 'medium', label: 'Medium' },
  { value: 'low', label: 'Low' },
  { value: 'none', label: 'No priority' },
];
export const priorityLabel = (p) => PRIORITIES.find((x) => x.value === p)?.label || 'No priority';

/* --------------------------------------------------------------- positions */

const byPos = (a, b) => a.position - b.position || (a.id < b.id ? -1 : a.id > b.id ? 1 : 0);

/**
 * Cards of a column in position order.
 * @param {Array<object>} cards
 * @param {string} columnId
 * @param {{archived?: boolean}} [opts] include archived cards
 */
export function columnCards(cards, columnId, { archived = false } = {}) {
  return cards.filter((c) => c.column_id === columnId && (archived || !c.archived)).sort(byPos);
}

/**
 * The `position` to send to POST /kanban/cards/{id}/move for a drop before `beforeId` (null =
 * end of the column). Positions are dense over every card of the column, archived ones too,
 * so the target card's own position is the answer — minus one when the moved card sits above
 * it in the same column (removing it shifts the target up).
 * @param {Array<object>} cards every card the client knows (with server positions)
 * @param {{id: string, toColumnId: string, beforeId: string|null}} drop
 * @returns {number|null} null = end of the column (omit `position`)
 */
export function targetPosition(cards, { id, toColumnId, beforeId }) {
  if (!beforeId) return null;
  const moving = cards.find((c) => c.id === id);
  const before = cards.find((c) => c.id === beforeId);
  if (!before || before.column_id !== toColumnId) return null;
  if (moving && moving.column_id === toColumnId && moving.position < before.position) return before.position - 1;
  return before.position;
}

/**
 * Apply a card move like the server does (clamping, shifting siblings) and return the new
 * card list (input untouched). position null/undefined = end of the column.
 * @param {Array<object>} cards
 * @param {{id: string, toColumnId: string, position?: number|null}} move
 */
export function applyMove(cards, { id, toColumnId, position = null }) {
  const moving = cards.find((c) => c.id === id);
  if (!moving) return cards;
  const from = moving.column_id;
  const target = cards.filter((c) => c.column_id === toColumnId && c.id !== id).sort(byPos);
  const max = target.length;
  const to = position === null || position === undefined ? max : Math.max(0, Math.min(max, Math.trunc(position)));
  target.splice(to, 0, moving);
  const next = new Map();
  target.forEach((c, i) => next.set(c.id, { column_id: toColumnId, position: i }));
  if (from !== toColumnId) {
    cards.filter((c) => c.column_id === from && c.id !== id).sort(byPos).forEach((c, i) => next.set(c.id, { column_id: from, position: i }));
  }
  return cards.map((c) => {
    const n = next.get(c.id);
    return n && (n.column_id !== c.column_id || n.position !== c.position) ? { ...c, ...n } : c;
  });
}

/**
 * Move an item of a dense ordered list (columns, subtasks) to a clamped position; returns a
 * new array in order with positions renumbered.
 * @param {Array<{id: string, position: number}>} items
 * @param {string} id
 * @param {number} position
 */
export function reorder(items, id, position) {
  const list = [...items].sort(byPos);
  const i = list.findIndex((x) => x.id === id);
  if (i < 0) return list;
  const [it] = list.splice(i, 1);
  const to = Math.max(0, Math.min(list.length, Math.trunc(position)));
  list.splice(to, 0, it);
  return list.map((x, n) => (x.position === n ? x : { ...x, position: n }));
}

/**
 * Where a keyboard move ("move up/down/left/right") sends a card, among the visible cards.
 * @param {Array<object>} cards all known cards
 * @param {Array<{id: string}>} columns in order
 * @param {string} id
 * @param {'up'|'down'|'left'|'right'|'top'|'bottom'} dir
 * @param {(card: object) => boolean} [visible] the cards shown (filters, archived)
 * @returns {{toColumnId: string, beforeId: string|null}|null} null when it cannot move
 */
export function keyboardMove(cards, columns, id, dir, visible = (c) => !c.archived) {
  const card = cards.find((c) => c.id === id);
  if (!card) return null;
  const list = (colId) => columnCards(cards, colId, { archived: true }).filter((c) => visible(c) || c.id === id);
  const here = list(card.column_id);
  const i = here.findIndex((c) => c.id === id);
  if (dir === 'up' || dir === 'top') {
    if (i <= 0) return null;
    return { toColumnId: card.column_id, beforeId: dir === 'top' ? here[0].id : here[i - 1].id };
  }
  if (dir === 'down' || dir === 'bottom') {
    if (i >= here.length - 1) return null;
    return { toColumnId: card.column_id, beforeId: dir === 'bottom' ? null : here[i + 2]?.id ?? null };
  }
  const ci = columns.findIndex((c) => c.id === card.column_id);
  const next = columns[dir === 'left' ? ci - 1 : ci + 1];
  if (!next) return null;
  const there = list(next.id);
  return { toColumnId: next.id, beforeId: there[Math.min(i, there.length)]?.id ?? null };
}

/* --------------------------------------------------------------- drag & drop */

/**
 * Insertion index for a pointer coordinate among items along one axis: the number of items
 * whose midpoint lies before the pointer.
 * @param {Array<{start: number, size: number}>} items in order (e.g. tiles' top and height)
 * @param {number} at pointer coordinate on the same axis
 */
export function dropIndex(items, at) {
  let i = 0;
  for (const it of items) {
    if (at < it.start + it.size / 2) break;
    i += 1;
  }
  return i;
}

/**
 * Index of the region containing a coordinate (nearest region when between or outside).
 * @param {Array<{start: number, size: number}>} regions in order
 * @param {number} at
 * @returns {number} -1 when there are no regions
 */
export function regionAt(regions, at) {
  if (!regions.length) return -1;
  let best = 0;
  let bestDist = Infinity;
  regions.forEach((r, i) => {
    const d = at < r.start ? r.start - at : at > r.start + r.size ? at - (r.start + r.size) : 0;
    if (d < bestDist) { best = i; bestDist = d; }
  });
  return best;
}

/**
 * Auto-scroll speed (px per frame) for a pointer near the edges of a scroll container:
 * negative near the start, positive near the end, 0 elsewhere; grows towards the edge.
 * @param {number} at pointer coordinate
 * @param {number} start container start
 * @param {number} end container end
 * @param {{edge?: number, max?: number}} [opts]
 */
export function autoScrollSpeed(at, start, end, { edge = 56, max = 22 } = {}) {
  const size = end - start;
  const e = Math.min(edge, size / 3);
  if (e <= 0) return 0;
  if (at < start + e) return -Math.ceil(max * Math.min(1, (start + e - at) / e));
  if (at > end - e) return Math.ceil(max * Math.min(1, (at - (end - e)) / e));
  return 0;
}

/* ------------------------------------------------------------------- dates */

const pad = (n) => String(n).padStart(2, '0');

/** 'YYYY-MM-DD' of a time in the local time zone ('' for none). */
export function localDate(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  return `${d.getFullYear()}-${pad(d.getMonth() + 1)}-${pad(d.getDate())}`;
}

/** 'YYYY-MM-DDTHH:MM' (a datetime-local input value) of a time in the local zone. */
export function localDateTime(iso) {
  if (!iso) return '';
  const d = new Date(iso);
  if (Number.isNaN(d.getTime())) return '';
  return `${localDate(iso)}T${pad(d.getHours())}:${pad(d.getMinutes())}`;
}

function parseLocalDate(s) {
  const m = /^(\d{4})-(\d{2})-(\d{2})$/.exec(String(s || ''));
  if (!m) return null;
  const d = new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3]));
  return d.getMonth() === Number(m[2]) - 1 ? d : null;
}

/**
 * All-day range of a local calendar date (API.md §13 Dates): start and end of that local
 * day as UTC ISO strings, so the server can compare with "now" without time zones.
 * @param {string} date 'YYYY-MM-DD'
 * @returns {{start: string, end: string}|null}
 */
export function allDayRange(date) {
  const d = parseLocalDate(date);
  if (!d) return null;
  const end = new Date(d.getFullYear(), d.getMonth(), d.getDate(), 23, 59, 59, 999);
  return { start: d.toISOString(), end: end.toISOString() };
}

/** UTC ISO string of a datetime-local value ('YYYY-MM-DDTHH:MM'), or null. */
export function fromLocalDateTime(value) {
  const m = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})/.exec(String(value || ''));
  if (!m) return null;
  const d = new Date(Number(m[1]), Number(m[2]) - 1, Number(m[3]), Number(m[4]), Number(m[5]));
  return Number.isNaN(d.getTime()) ? null : d.toISOString();
}

/**
 * Card date fields for the date editor: with allDay the dates are local calendar days
 * ('YYYY-MM-DD'), otherwise datetime-local values. Empty strings clear a date.
 * @param {{start: string, due: string, allDay: boolean}} v
 * @returns {{start_at: string|null, due_at: string|null, all_day: boolean}}
 */
export function datesPatch({ start, due, allDay }) {
  if (allDay) {
    return { start_at: start ? allDayRange(start)?.start ?? null : null, due_at: due ? allDayRange(due)?.end ?? null : null, all_day: true };
  }
  return { start_at: fromLocalDateTime(start), due_at: fromLocalDateTime(due), all_day: false };
}

function startOfDay(t) {
  const d = new Date(t);
  d.setHours(0, 0, 0, 0);
  return d.getTime();
}

const DAY = 86_400_000;

/**
 * State of a card's due date for its badge.
 * @returns {'done'|'overdue'|'today'|'soon'|'later'|null} soon = within 7 days
 */
export function dueState(card, now = Date.now()) {
  if (!card?.due_at) return null;
  const due = new Date(card.due_at).getTime();
  if (Number.isNaN(due)) return null;
  if (card.completed) return 'done';
  if (due < now) return 'overdue';
  const today = startOfDay(now);
  if (due < today + DAY) return 'today';
  if (due < today + 8 * DAY) return 'soon';
  return 'later';
}

/**
 * "My cards" bucket by local calendar day: overdue (open and past due), today, this week (the
 * next 7 days), later, earlier (completed cards due before today) or none.
 * @returns {'overdue'|'today'|'week'|'later'|'earlier'|'none'}
 */
export function dueBucket(card, now = Date.now()) {
  if (!card?.due_at) return 'none';
  const due = new Date(card.due_at).getTime();
  if (Number.isNaN(due)) return 'none';
  if (due < now && !card.completed) return 'overdue';
  const today = startOfDay(now);
  if (due < today) return 'earlier';
  if (due < today + DAY) return 'today';
  if (due < today + 8 * DAY) return 'week';
  return 'later';
}

export const BUCKETS = [
  { id: 'overdue', title: 'Overdue' },
  { id: 'today', title: 'Today' },
  { id: 'week', title: 'This week' },
  { id: 'later', title: 'Later' },
  { id: 'earlier', title: 'Earlier' },
  { id: 'none', title: 'No due date' },
];

/**
 * Group cards into the "My cards" buckets (empty buckets dropped), each by due date.
 * @returns {Array<{id: string, title: string, cards: object[]}>}
 */
export function groupByBucket(cards, now = Date.now()) {
  const groups = new Map(BUCKETS.map((b) => [b.id, []]));
  for (const c of cards) groups.get(dueBucket(c, now)).push(c);
  const t = (c) => (c.due_at ? new Date(c.due_at).getTime() : Infinity);
  return BUCKETS.map((b) => ({ ...b, cards: groups.get(b.id).sort((x, y) => t(x) - t(y) || x.title.localeCompare(y.title)) }))
    .filter((g) => g.cards.length);
}

/**
 * Short due label: "Today", "Tomorrow", "Yesterday", weekday within a week, else a date (with
 * the year when not this year); times are added for cards that are not all-day.
 * @param {{due_at: string|null, all_day?: boolean}} card
 * @param {number} [now]
 * @param {string} [locale]
 */
export function dueLabel(card, now = Date.now(), locale = undefined) {
  if (!card?.due_at) return '';
  const d = new Date(card.due_at);
  if (Number.isNaN(d.getTime())) return '';
  const days = Math.round((startOfDay(d.getTime()) - startOfDay(now)) / DAY);
  let day;
  if (days === 0) day = 'Today';
  else if (days === 1) day = 'Tomorrow';
  else if (days === -1) day = 'Yesterday';
  else if (days > 1 && days < 7) day = new Intl.DateTimeFormat(locale, { weekday: 'short' }).format(d);
  else {
    const sameYear = d.getFullYear() === new Date(now).getFullYear();
    day = new Intl.DateTimeFormat(locale, sameYear ? { month: 'short', day: 'numeric' } : { year: 'numeric', month: 'short', day: 'numeric' }).format(d);
  }
  if (card.all_day) return day;
  return `${day}, ${new Intl.DateTimeFormat(locale, { hour: '2-digit', minute: '2-digit' }).format(d)}`;
}

/* ----------------------------------------------------------------- filters */

/** An empty board filter. */
export const emptyFilter = () => ({ q: '', labels: [], assignee: '', due: '' });

/** True when the filter hides anything. */
export const filterActive = (f) => !!(f && (f.q || f.labels.length || f.assignee || f.due));

/** Board filter from the URL query (?q=&label=a,b&assignee=me|none|<id>&due=…). */
export function filterFromQuery(q = {}) {
  const f = emptyFilter();
  if (typeof q.q === 'string') f.q = q.q.slice(0, 200);
  if (typeof q.label === 'string') f.labels = q.label.split(',').filter(isId).slice(0, 50);
  if (q.assignee === 'me' || q.assignee === 'none' || isId(q.assignee)) f.assignee = q.assignee;
  if (['overdue', 'today', 'week', 'none'].includes(q.due)) f.due = q.due;
  return f;
}

/** URL query patch of a board filter (null removes a key). */
export function filterToQuery(f) {
  return { q: f.q || null, label: f.labels.length ? f.labels.join(',') : null, assignee: f.assignee || null, due: f.due || null };
}

/**
 * Does a card pass the board filter? Text matches title, description and "#number"; labels
 * match when the card has any of them; due windows use local days like the badges.
 * @param {object} card
 * @param {{q: string, labels: string[], assignee: string, due: string}} f
 * @param {{me?: string, now?: number}} [env]
 */
export function matchesFilter(card, f, { me = '', now = Date.now() } = {}) {
  if (!f) return true;
  const q = f.q.trim().toLowerCase();
  if (q) {
    const num = /^#?(\d+)$/.exec(q);
    const hit = (num && card.number === Number(num[1])) || card.title.toLowerCase().includes(q)
      || (card.description || '').toLowerCase().includes(q);
    if (!hit) return false;
  }
  if (f.labels.length && !f.labels.some((l) => card.labels.includes(l))) return false;
  if (f.assignee === 'none' && card.assignees.length) return false;
  if (f.assignee === 'me' && !card.assignees.includes(me)) return false;
  if (isId(f.assignee) && !card.assignees.includes(f.assignee)) return false;
  if (f.due) {
    const b = dueBucket(card, now);
    if (f.due === 'overdue' && b !== 'overdue') return false;
    if (f.due === 'today' && !(b === 'overdue' || b === 'today')) return false;
    if (f.due === 'week' && !(b === 'overdue' || b === 'today' || b === 'week')) return false;
    if (f.due === 'none' && card.due_at) return false;
  }
  return true;
}

/* ------------------------------------------------------------ small helpers */

/** Display name of a member (display name, else username, else "Unknown user"). */
export function memberName(m) {
  return (m && (m.display_name?.trim() || m.username)) || 'Unknown user';
}

/** Subtask progress of a card. */
export function progressOf(card) {
  const total = card?.subtasks_total || 0;
  const done = card?.subtasks_done || 0;
  return { done, total, pct: total ? Math.round((done / total) * 100) : 0 };
}

/**
 * WIP state of a column with `count` visible (non-archived) cards.
 * @returns {'none'|'ok'|'full'|'over'}
 */
export function wipState(column, count) {
  const lim = column?.wip_limit;
  if (!lim) return 'none';
  if (count > lim) return 'over';
  if (count === lim) return 'full';
  return 'ok';
}

/** Role checks. */
const RANK = { viewer: 1, editor: 2, admin: 3, owner: 4 };
export const can = (role, need) => (RANK[role] || 0) >= (RANK[need] || 99);

/**
 * Merge a polled card list into the local one: cards with an operation in flight keep their
 * local copy (the server answer of that operation brings the truth); everything else comes
 * from the server.
 * @param {Array<object>} local
 * @param {Array<object>} remote
 * @param {Set<string>} pending ids with local changes in flight
 */
export function mergeCards(local, remote, pending) {
  if (!pending || pending.size === 0) return remote;
  const mine = new Map(local.map((c) => [c.id, c]));
  return remote.map((c) => (pending.has(c.id) && mine.has(c.id) ? mine.get(c.id) : c));
}

/** Stable signature of a value for change detection (key order of plain objects kept). */
export function signature(v) {
  return JSON.stringify(v);
}

/**
 * Sentence for an activity entry (actor first).
 * @param {{action: string, actor_name: string, meta: object}} a
 * @param {{users?: (id: string) => string, inCard?: boolean}} [opts]
 */
export function activityText(a, { users = () => 'someone', inCard = false } = {}) {
  const m = a.meta || {};
  const who = a.actor_name || 'Someone';
  const card = inCard ? 'this card' : m.title ? `“${m.title}”` : 'a card';
  const fields = Array.isArray(m.fields) ? m.fields.map((f) => FIELD_NAMES[f] || f) : [];
  switch (a.action) {
    case 'board.created': return `${who} created the board`;
    case 'board.updated': return `${who} changed the board’s ${list(fields) || 'settings'}`;
    case 'board.archived': return `${who} archived the board`;
    case 'board.restored': return `${who} restored the board`;
    case 'board.transferred': return `${who} transferred the board to ${users(m.to)}`;
    case 'member.added': return `${who} added ${users(m.user_id)} as ${m.role}`;
    case 'member.removed': return m.account_deleted ? 'A deleted account was removed from the board' : `${who} removed ${users(m.user_id)}`;
    case 'member.role_changed': return `${who} made ${users(m.user_id)} ${m.role === 'admin' ? 'an admin' : `a${m.role === 'editor' ? 'n' : ''} ${m.role}`}`;
    case 'member.left': return `${who} left the board`;
    case 'column.created': return `${who} added the column “${m.name}”`;
    case 'column.updated': return m.old_name ? `${who} renamed the column “${m.old_name}” to “${m.name}”` : `${who} changed the ${list(fields)} of “${m.name}”`;
    case 'column.moved': return `${who} moved the column “${m.name}”`;
    case 'column.deleted': return `${who} deleted the column “${m.name}”${m.moved_cards ? ` and moved its cards to “${m.to_column}”` : ''}`;
    case 'label.created': return `${who} created the label “${m.name}”`;
    case 'label.updated': return m.old_name ? `${who} renamed the label “${m.old_name}” to “${m.name}”` : `${who} changed the label “${m.name}”`;
    case 'label.deleted': return `${who} deleted the label “${m.name}”`;
    case 'card.created': return `${who} created ${card}${m.column && !inCard ? ` in “${m.column}”` : ''}`;
    case 'card.updated': return m.old_title && fields.length === 1 ? `${who} renamed ${inCard ? 'this card' : `“${m.old_title}”`} to “${m.title}”` : `${who} changed the ${list(fields)} of ${card}`;
    case 'card.moved': return `${who} moved ${card} from “${m.from_column}” to “${m.to_column}”`;
    case 'card.completed': return `${who} completed ${card}`;
    case 'card.reopened': return `${who} reopened ${card}`;
    case 'card.archived': return `${who} archived ${card}`;
    case 'card.restored': return `${who} restored ${card}`;
    case 'card.deleted': return `${who} deleted the card “${m.title}”`;
    case 'subtask.created': return `${who} added the subtask “${m.title}”`;
    case 'subtask.updated': return `${who} changed the subtask “${m.title}”`;
    case 'subtask.completed': return `${who} checked “${m.title}”`;
    case 'subtask.reopened': return `${who} unchecked “${m.title}”`;
    case 'subtask.deleted': return `${who} deleted the subtask “${m.title}”`;
    case 'subtask.promoted': return `${who} turned the subtask “${m.title}” into a card`;
    case 'comment.created': return `${who} commented on ${card}`;
    case 'comment.deleted': return `${who} deleted a comment`;
    case 'attachment.added': return `${who} attached “${m.filename}”${inCard ? '' : ` to ${card}`}`;
    case 'attachment.renamed': return `${who} renamed the attachment “${m.old_filename}” to “${m.filename}”`;
    case 'attachment.deleted': return `${who} deleted the attachment “${m.filename}”`;
    default: return `${who}: ${a.action}`;
  }
}

const FIELD_NAMES = {
  title: 'title', description: 'description', labels: 'labels', assignees: 'assignees', priority: 'priority',
  start_at: 'start date', due_at: 'due date', all_day: 'all-day setting', cover_attachment_id: 'cover',
  name: 'name', color: 'color', wip_limit: 'WIP limit', done: 'state', assignee_id: 'assignee',
};

function list(items) {
  if (items.length <= 1) return items[0] || '';
  return `${items.slice(0, -1).join(', ')} and ${items[items.length - 1]}`;
}
