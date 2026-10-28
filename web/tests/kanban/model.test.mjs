// Unit tests for the Kanban app's pure logic (apps/kanban/model.js).
// Dates run in a zone with daylight saving time so the all-day conversion is meaningful.
process.env.TZ = 'Europe/Berlin';

import test from 'node:test';
import assert from 'node:assert/strict';
import {
  isId, parseRoute, paths, COLORS, isColor, colorClass, pickLabelColor, priorityLabel, columnCards, targetPosition, applyMove,
  reorder, keyboardMove, dropIndex, regionAt, autoScrollSpeed, localDate, localDateTime, allDayRange, fromLocalDateTime,
  datesPatch, dueState, dueBucket, groupByBucket, dueLabel, emptyFilter, filterActive, filterFromQuery, filterToQuery,
  matchesFilter, memberName, progressOf, wipState, can, mergeCards, activityText,
} from '../../assets/js/apps/kanban/model.js';

const B = 'boardaaaa1';
const C1 = 'columnaaa1';
const C2 = 'columnaaa2';
const card = (id, column, position, extra = {}) => ({
  id, column_id: column, position, title: id, description: '', number: 1, labels: [], assignees: [], archived: false,
  completed: false, due_at: null, all_day: false, ...extra,
});

test('routes', () => {
  assert.deepEqual(parseRoute(''), { view: 'boards' });
  assert.deepEqual(parseRoute('my'), { view: 'my' });
  assert.deepEqual(parseRoute(B), { view: 'board', boardId: B, cardId: null });
  assert.deepEqual(parseRoute(`${B}/c/cardaaaaa1`), { view: 'board', boardId: B, cardId: 'cardaaaaa1' });
  assert.deepEqual(parseRoute(`${B}/c/../../x`), { view: 'boards' });
  assert.deepEqual(parseRoute('short'), { view: 'boards' }, 'ids have at least 8 characters');
  assert.equal(paths.card(B, 'cardaaaaa1'), `/kanban/${B}/c/cardaaaaa1`);
  assert.ok(isId('abc_DEF-12') && !isId('a b') && !isId(null));
});

test('label palette', () => {
  assert.equal(COLORS.length, 11);
  assert.ok(isColor('teal') && !isColor('beige'));
  assert.equal(colorClass('nope'), 'kb-c-gray');
  assert.equal(pickLabelColor([]), 'red', 'first non-gray color');
  assert.equal(pickLabelColor([{ color: 'red' }, { color: 'orange' }]), 'amber');
  const all = COLORS.filter((c) => c !== 'gray').map((color) => ({ color }));
  assert.equal(pickLabelColor(all), 'gray', 'gray once every color is used');
  assert.equal(pickLabelColor([...all, { color: 'gray' }, { color: 'red' }]), 'orange');
  assert.equal(priorityLabel('urgent'), 'Urgent');
  assert.equal(priorityLabel('bogus'), 'No priority');
});

/** Reference implementation of the server's move (API.md §13 Ordering). */
function serverMove(cards, id, toColumn, position) {
  const cols = new Map();
  for (const c of [...cards].sort((a, b) => a.position - b.position)) {
    if (!cols.has(c.column_id)) cols.set(c.column_id, []);
    cols.get(c.column_id).push(c.id);
  }
  const moving = cards.find((c) => c.id === id);
  const from = cols.get(moving.column_id);
  from.splice(from.indexOf(id), 1);
  if (!cols.has(toColumn)) cols.set(toColumn, []);
  const to = cols.get(toColumn);
  const p = position === null ? to.length : Math.max(0, Math.min(to.length, position));
  to.splice(p, 0, id);
  const out = {};
  for (const [col, ids] of cols) ids.forEach((cid, i) => { out[cid] = [col, i]; });
  return out;
}

test('target positions match the server for every drop, archived cards included', () => {
  const cards = [
    card('a', C1, 0), card('b', C1, 1, { archived: true }), card('c', C1, 2), card('d', C1, 3),
    card('x', C2, 0), card('y', C2, 1, { archived: true }), card('z', C2, 2),
  ];
  for (const moving of cards.filter((c) => !c.archived)) {
    for (const toCol of [C1, C2]) {
      const visible = columnCards(cards, toCol).filter((c) => c.id !== moving.id);
      for (const before of [...visible, null]) {
        const pos = targetPosition(cards, { id: moving.id, toColumnId: toCol, beforeId: before?.id ?? null });
        const moved = applyMove(cards, { id: moving.id, toColumnId: toCol, position: pos });
        const want = serverMove(cards, moving.id, toCol, pos);
        for (const c of moved) assert.deepEqual([c.column_id, c.position], want[c.id], `${moving.id} → ${toCol} before ${before?.id}`);
        // The moved card lands right before the drop target among the visible cards.
        const order = columnCards(moved, toCol).map((c) => c.id);
        const at = order.indexOf(moving.id);
        assert.equal(order[at + 1] ?? null, before?.id ?? null, `${moving.id} before ${before?.id}`);
      }
    }
  }
});

test('applyMove clamps, shifts and leaves the input alone', () => {
  const cards = [card('a', C1, 0), card('b', C1, 1), card('c', C2, 0)];
  const out = applyMove(cards, { id: 'a', toColumnId: C2, position: 99 });
  assert.deepEqual(out.map((c) => [c.id, c.column_id, c.position]), [['a', C2, 1], ['b', C1, 0], ['c', C2, 0]]);
  assert.equal(cards[0].column_id, C1, 'input untouched');
  assert.equal(applyMove(cards, { id: 'nope', toColumnId: C2 }), cards);
  const neg = applyMove(cards, { id: 'b', toColumnId: C1, position: -3 });
  assert.deepEqual(columnCards(neg, C1).map((c) => c.id), ['b', 'a']);
  const same = applyMove(cards, { id: 'c', toColumnId: C2, position: 0 });
  assert.equal(same[2], cards[2], 'unchanged cards keep their identity');
});

test('reorder lists of columns and subtasks', () => {
  const items = [{ id: 'a', position: 0 }, { id: 'b', position: 1 }, { id: 'c', position: 2 }];
  assert.deepEqual(reorder(items, 'a', 2).map((x) => `${x.id}${x.position}`), ['b0', 'c1', 'a2']);
  assert.deepEqual(reorder(items, 'c', -1).map((x) => x.id), ['c', 'a', 'b']);
  assert.deepEqual(reorder(items, 'zz', 0).map((x) => x.id), ['a', 'b', 'c']);
});

test('keyboard moves', () => {
  const cards = [card('a', C1, 0), card('h', C1, 1, { archived: true }), card('b', C1, 2), card('c', C1, 3), card('x', C2, 0)];
  const cols = [{ id: C1 }, { id: C2 }];
  assert.deepEqual(keyboardMove(cards, cols, 'b', 'up'), { toColumnId: C1, beforeId: 'a' });
  assert.equal(keyboardMove(cards, cols, 'a', 'up'), null);
  assert.deepEqual(keyboardMove(cards, cols, 'a', 'down'), { toColumnId: C1, beforeId: 'c' });
  assert.deepEqual(keyboardMove(cards, cols, 'b', 'down'), { toColumnId: C1, beforeId: null });
  assert.equal(keyboardMove(cards, cols, 'c', 'down'), null);
  assert.deepEqual(keyboardMove(cards, cols, 'c', 'top'), { toColumnId: C1, beforeId: 'a' });
  assert.deepEqual(keyboardMove(cards, cols, 'b', 'right'), { toColumnId: C2, beforeId: null }, 'index 1 → end of a 1-card column');
  assert.deepEqual(keyboardMove(cards, cols, 'a', 'right'), { toColumnId: C2, beforeId: 'x' });
  assert.equal(keyboardMove(cards, cols, 'a', 'left'), null);
  // Applying a keyboard move through targetPosition really moves the card one step.
  const mv = keyboardMove(cards, cols, 'a', 'down');
  const moved = applyMove(cards, { id: 'a', toColumnId: mv.toColumnId, position: targetPosition(cards, { id: 'a', ...mv }) });
  assert.deepEqual(columnCards(moved, C1).map((c) => c.id), ['b', 'a', 'c']);
});

test('drop index, regions and auto-scroll', () => {
  const tiles = [{ start: 0, size: 40 }, { start: 48, size: 40 }, { start: 96, size: 60 }];
  assert.equal(dropIndex(tiles, -10), 0);
  assert.equal(dropIndex(tiles, 19), 0);
  assert.equal(dropIndex(tiles, 21), 1);
  assert.equal(dropIndex(tiles, 120), 2);
  assert.equal(dropIndex(tiles, 127), 3);
  assert.equal(dropIndex([], 5), 0);
  const cols = [{ start: 0, size: 280 }, { start: 292, size: 280 }];
  assert.equal(regionAt(cols, 100), 0);
  assert.equal(regionAt(cols, 287), 1, 'nearest in the gap');
  assert.equal(regionAt(cols, 5000), 1);
  assert.equal(regionAt([], 1), -1);
  assert.equal(autoScrollSpeed(500, 0, 1000), 0);
  assert.ok(autoScrollSpeed(5, 0, 1000) < 0 && autoScrollSpeed(995, 0, 1000) > 0);
  assert.ok(Math.abs(autoScrollSpeed(1, 0, 1000)) > Math.abs(autoScrollSpeed(40, 0, 1000)), 'faster nearer the edge');
  assert.ok(autoScrollSpeed(-50, 0, 1000) >= -22, 'capped');
});

test('all-day conversion uses the local day (DST aware)', () => {
  const r = allDayRange('2026-10-25'); // Berlin switches from CEST to CET that night
  assert.equal(r.start, '2026-10-24T22:00:00.000Z');
  assert.equal(r.end, '2026-10-25T22:59:59.999Z');
  assert.equal(localDate(r.start), '2026-10-25');
  assert.equal(localDate(r.end), '2026-10-25');
  assert.equal(allDayRange('2026-02-30'), null);
  assert.equal(allDayRange('tomorrow'), null);
  assert.equal(fromLocalDateTime('2026-07-01T09:30'), '2026-07-01T07:30:00.000Z');
  assert.equal(localDateTime('2026-07-01T07:30:00.000Z'), '2026-07-01T09:30');
  assert.equal(fromLocalDateTime(''), null);
  assert.deepEqual(datesPatch({ start: '2026-11-02', due: '2026-11-03', allDay: true }),
    { start_at: '2026-11-01T23:00:00.000Z', due_at: '2026-11-03T22:59:59.999Z', all_day: true });
  assert.deepEqual(datesPatch({ start: '', due: '2026-11-03T17:00', allDay: false }),
    { start_at: null, due_at: '2026-11-03T16:00:00.000Z', all_day: false });
});

test('due states, buckets and labels', () => {
  const now = new Date(2026, 9, 27, 12, 0).getTime(); // Tue 27 Oct 2026, 12:00 local
  const at = (d, h = 12, m = 0) => new Date(2026, 9, d, h, m).toISOString();
  assert.equal(dueState({ due_at: null }, now), null);
  assert.equal(dueState({ due_at: at(27, 11) }, now), 'overdue');
  assert.equal(dueState({ due_at: at(27, 11), completed: true }, now), 'done');
  assert.equal(dueState({ due_at: at(27, 23, 59) }, now), 'today');
  assert.equal(dueState({ due_at: at(30) }, now), 'soon');
  assert.equal(dueState({ due_at: at(31 + 8) }, now), 'later');
  assert.equal(dueBucket({ due_at: at(26) }, now), 'overdue');
  assert.equal(dueBucket({ due_at: at(26), completed: true }, now), 'earlier');
  assert.equal(dueBucket({ due_at: at(27, 8), completed: true }, now), 'today');
  assert.equal(dueBucket({ due_at: at(28) }, now), 'week');
  assert.equal(dueBucket({ due_at: at(31 + 10) }, now), 'later');
  const allDayToday = { due_at: allDayRange('2026-10-27').end, all_day: true };
  assert.equal(dueBucket(allDayToday, now), 'today');
  const groups = groupByBucket([
    { id: '1', title: 'b', due_at: at(28) }, { id: '2', title: 'a', due_at: null }, { id: '3', title: 'c', due_at: at(20) },
    { id: '4', title: 'd', due_at: at(28, 9) },
  ], now);
  assert.deepEqual(groups.map((g) => [g.id, g.cards.map((c) => c.id)]), [['overdue', ['3']], ['week', ['4', '1']], ['none', ['2']]]);
  assert.equal(dueLabel(allDayToday, now, 'en-US'), 'Today');
  assert.equal(dueLabel({ due_at: at(28, 15, 5) }, now, 'en-US'), 'Tomorrow, 03:05 PM');
  assert.equal(dueLabel({ due_at: at(26), all_day: true }, now, 'en-US'), 'Yesterday');
  assert.equal(dueLabel({ due_at: at(30), all_day: true }, now, 'en-US'), 'Fri');
  assert.equal(dueLabel({ due_at: new Date(2027, 0, 5).toISOString(), all_day: true }, now, 'en-US'), 'Jan 5, 2027');
  assert.equal(dueLabel({ due_at: null }, now), '');
});

test('board filters', () => {
  const now = new Date(2026, 9, 27, 12, 0).getTime();
  const me = 'useraaaaa1';
  const L1 = 'labelaaaa1';
  const L2 = 'labelaaaa2';
  const cards = [
    card('c1', C1, 0, { title: 'Fix login', number: 12, labels: [L1], assignees: [me], due_at: new Date(2026, 9, 26).toISOString() }),
    card('c2', C1, 1, { title: 'Write docs', description: 'Mention LOGIN flow', labels: [L2], due_at: new Date(2026, 9, 29).toISOString() }),
    card('c3', C1, 2, { title: 'Party', assignees: ['otheruser1'] }),
  ];
  const ids = (f) => cards.filter((c) => matchesFilter(c, { ...emptyFilter(), ...f }, { me, now })).map((c) => c.id);
  assert.deepEqual(ids({}), ['c1', 'c2', 'c3']);
  assert.deepEqual(ids({ q: 'login' }), ['c1', 'c2']);
  assert.deepEqual(ids({ q: '#12' }), ['c1']);
  assert.deepEqual(ids({ labels: [L1, L2] }), ['c1', 'c2']);
  assert.deepEqual(ids({ assignee: 'me' }), ['c1']);
  assert.deepEqual(ids({ assignee: 'none' }), ['c2']);
  assert.deepEqual(ids({ assignee: 'otheruser1' }), ['c3']);
  assert.deepEqual(ids({ due: 'overdue' }), ['c1']);
  assert.deepEqual(ids({ due: 'today' }), ['c1']);
  assert.deepEqual(ids({ due: 'week' }), ['c1', 'c2']);
  assert.deepEqual(ids({ due: 'none' }), ['c3']);
  assert.ok(!filterActive(emptyFilter()) && filterActive({ ...emptyFilter(), due: 'week' }));
  const f = filterFromQuery({ q: 'x', label: `${L1},bad id,${L2}`, assignee: 'me', due: 'week' });
  assert.deepEqual(f, { q: 'x', labels: [L1, L2], assignee: 'me', due: 'week' });
  assert.deepEqual(filterFromQuery({ assignee: '<script>', due: 'soon' }), emptyFilter());
  assert.deepEqual(filterToQuery(emptyFilter()), { q: null, label: null, assignee: null, due: null });
  assert.deepEqual(filterFromQuery(Object.fromEntries(Object.entries(filterToQuery(f)).filter(([, v]) => v !== null))), f);
});

test('helpers: names, progress, WIP, roles, merge', () => {
  assert.equal(memberName({ display_name: ' ', username: 'ada' }), 'ada');
  assert.equal(memberName({ display_name: 'Ada L.', username: 'ada' }), 'Ada L.');
  assert.equal(memberName(null), 'Unknown user');
  assert.deepEqual(progressOf({ subtasks_total: 3, subtasks_done: 1 }), { done: 1, total: 3, pct: 33 });
  assert.deepEqual(progressOf({}), { done: 0, total: 0, pct: 0 });
  assert.equal(wipState({ wip_limit: null }, 9), 'none');
  assert.equal(wipState({ wip_limit: 3 }, 2), 'ok');
  assert.equal(wipState({ wip_limit: 3 }, 3), 'full');
  assert.equal(wipState({ wip_limit: 3 }, 4), 'over');
  assert.ok(can('owner', 'admin') && can('editor', 'editor') && !can('viewer', 'editor') && !can(undefined, 'viewer'));
  const local = [card('a', C1, 5, { title: 'mine' }), card('b', C1, 1)];
  const remote = [card('a', C1, 0, { title: 'server' }), card('b', C1, 1, { title: 'server b' }), card('n', C1, 2)];
  assert.equal(mergeCards(local, remote, new Set()), remote);
  const merged = mergeCards(local, remote, new Set(['a']));
  assert.deepEqual(merged.map((c) => c.title), ['mine', 'server b', 'n']);
});

test('activity sentences', () => {
  const users = (id) => ({ u1: 'Ada', u2: 'Bob' }[id] || 'someone');
  assert.equal(activityText({ action: 'card.moved', actor_name: 'Ada', meta: { title: 'Fix', from_column: 'To do', to_column: 'Done' } }),
    'Ada moved “Fix” from “To do” to “Done”');
  assert.equal(activityText({ action: 'card.updated', actor_name: 'Ada', meta: { title: 'B', fields: ['priority', 'due_at', 'labels'] } }, { inCard: true }),
    'Ada changed the priority, due date and labels of this card');
  assert.equal(activityText({ action: 'card.updated', actor_name: 'Ada', meta: { title: 'New', old_title: 'Old', fields: ['title'] } }),
    'Ada renamed “Old” to “New”');
  assert.equal(activityText({ action: 'member.added', actor_name: 'Ada', meta: { user_id: 'u2', role: 'editor' } }, { users }), 'Ada added Bob as editor');
  assert.equal(activityText({ action: 'member.role_changed', actor_name: 'Ada', meta: { user_id: 'u2', role: 'admin' } }, { users }), 'Ada made Bob an admin');
  assert.equal(activityText({ action: 'member.removed', actor_name: 'Deleted user', meta: { account_deleted: true } }),
    'A deleted account was removed from the board');
  assert.equal(activityText({ action: 'some.future', actor_name: 'Ada', meta: {} }), 'Ada: some.future');
});
