// Smart playlist rules editor model (apps/music/smartrules.js): fields/operators, value kinds,
// validation like the server's, API conversion both ways and the summary line.
process.env.TZ = 'Europe/Berlin';

import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  FIELDS, FIELD_ORDER, OPS, SORTS, MAX_CONDITIONS, kindOf, opsFor, valueKind, defaultValue, newCondition, withField, withOp,
  newDraft, fromApi, toApi, validate, errorTarget, describe, dateToIso, dateInputValue,
} from '../../assets/js/apps/music/smartrules.js';

test('fields and operators match the contract', () => {
  assert.deepEqual(FIELD_ORDER, ['title', 'artist', 'album', 'album_artist', 'genre', 'composer', 'path', 'codec', 'subcategory',
    'year', 'duration_seconds', 'play_count', 'rating', 'bitrate_kbps', 'added_at', 'last_played_at', 'favorite', 'compilation']);
  assert.deepEqual(OPS.text.map(([o]) => o), ['is', 'is_not', 'contains', 'not_contains', 'starts_with']);
  assert.deepEqual(OPS.number.map(([o]) => o), ['eq', 'ne', 'gt', 'gte', 'lt', 'lte', 'between']);
  assert.deepEqual(OPS.date.map(([o]) => o), ['in_last_days', 'not_in_last_days', 'before', 'after']);
  assert.deepEqual(OPS.bool.map(([o]) => o), ['is_true', 'is_false']);
  assert.deepEqual(SORTS.map(([s]) => s), ['title', 'artist', 'album', 'added', 'year', 'duration', 'plays', 'last_played', 'random']);
  assert.equal(MAX_CONDITIONS, 20);
  assert.equal(kindOf('year'), 'number');
  assert.equal(kindOf('nope'), 'text');
  assert.equal(opsFor('favorite'), OPS.bool);
  for (const f of FIELD_ORDER) assert.ok(FIELDS[f].label);
});

test('value kinds and defaults', () => {
  assert.equal(valueKind('genre', 'contains'), 'text');
  assert.equal(valueKind('year', 'gt'), 'number');
  assert.equal(valueKind('year', 'between'), 'range');
  assert.equal(valueKind('added_at', 'in_last_days'), 'days');
  assert.equal(valueKind('added_at', 'before'), 'date');
  assert.equal(valueKind('favorite', 'is_true'), 'none');
  assert.deepEqual(defaultValue('year', 'between'), ['', '']);
  assert.equal(defaultValue('added_at', 'not_in_last_days'), '30');
  assert.equal(defaultValue('compilation', 'is_false'), null);
  assert.deepEqual(newCondition('rating'), { field: 'rating', op: 'eq', value: '' });
  assert.deepEqual(newCondition('bogus'), { field: 'genre', op: 'is', value: '' });
});

test('changing field or operator keeps compatible values', () => {
  const c = { field: 'title', op: 'contains', value: 'love' };
  assert.deepEqual(withField(c, 'album'), { field: 'album', op: 'contains', value: 'love' }, 'same kind: kept');
  assert.deepEqual(withField(c, 'year'), { field: 'year', op: 'eq', value: '' }, 'other kind: reset');
  assert.equal(withField(c, 'nope'), c);
  const n = { field: 'year', op: 'gt', value: '1990' };
  assert.deepEqual(withOp(n, 'lte'), { field: 'year', op: 'lte', value: '1990' });
  assert.deepEqual(withOp(n, 'between'), { field: 'year', op: 'between', value: ['', ''] });
  assert.equal(withOp(n, 'contains'), n, 'operator of another kind is ignored');
});

test('validation mirrors the server', () => {
  assert.deepEqual(validate(newDraft()), {}, 'is "" is allowed (empty genre)');
  const d = {
    match: 'all', sort: 'year', order: 'desc', limit: '0',
    conditions: [
      { field: 'genre', op: 'contains', value: '  ' },
      { field: 'year', op: 'between', value: ['2000', '1990'] },
      { field: 'duration_seconds', op: 'gt', value: '3:75' },
      { field: 'added_at', op: 'in_last_days', value: '0' },
      { field: 'last_played_at', op: 'before', value: '2026-02-30' },
      { field: 'title', op: 'is', value: 'x'.repeat(501) },
      { field: 'nope', op: 'is', value: '' },
      { field: 'year', op: 'contains', value: '1' },
      { field: 'favorite', op: 'is_true', value: null },
    ],
  };
  const e = validate(d);
  assert.deepEqual(Object.keys(e).sort(), [
    'rules.conditions[0].value', 'rules.conditions[1].value', 'rules.conditions[2].value', 'rules.conditions[3].value',
    'rules.conditions[4].value', 'rules.conditions[5].value', 'rules.conditions[6].field', 'rules.conditions[7].op', 'rules.limit',
  ].sort());
  assert.ok(validate({ ...newDraft(), conditions: [] })['rules.conditions']);
  assert.ok(validate({ ...newDraft(), conditions: Array.from({ length: 21 }, () => newCondition()) })['rules.conditions']);
  assert.ok(validate({ ...newDraft(), match: 'some' })['rules.match']);
  assert.ok(validate({ ...newDraft(), sort: 'rating' })['rules.sort']);
  assert.ok(validate({ ...newDraft(), limit: '5001' })['rules.limit']);
  assert.deepEqual(validate({ ...newDraft(), limit: '5000' }), {});
  assert.deepEqual(validate({ ...newDraft(), conditions: [{ field: 'title', op: 'contains', value: 'a\u0007b' }] }), { 'rules.conditions[0].value': 'Use a single line of at most 500 characters.' });
});

test('draft ↔ API conversion', () => {
  const api = {
    match: 'any', sort: 'added', order: 'desc', limit: 50,
    conditions: [
      { field: 'genre', op: 'contains', value: 'rock' },
      { field: 'year', op: 'between', value: [1990, 1999] },
      { field: 'duration_seconds', op: 'lt', value: 210 },
      { field: 'added_at', op: 'in_last_days', value: 30 },
      { field: 'last_played_at', op: 'after', value: '2026-03-01T00:00:00.000Z' },
      { field: 'favorite', op: 'is_true', value: null },
    ],
  };
  const draft = fromApi(api);
  assert.equal(draft.match, 'any');
  assert.equal(draft.limit, '50');
  assert.deepEqual(draft.conditions[1].value, ['1990', '1999']);
  assert.equal(draft.conditions[2].value, '3:30', 'durations are shown as m:ss');
  assert.equal(draft.conditions[3].value, '30');
  assert.equal(draft.conditions[4].value, '2026-03-01', 'local date');
  assert.deepEqual(validate(draft), {});
  const back = toApi(draft);
  assert.equal(back.match, 'any');
  assert.equal(back.limit, 50);
  assert.deepEqual(back.conditions.slice(0, 4), api.conditions.slice(0, 4));
  assert.equal(back.conditions[4].value, dateToIso('2026-03-01'));
  assert.equal(back.conditions[5].value, null);
  assert.equal(toApi({ ...draft, limit: '' }).limit, null, 'empty limit = no limit');
  assert.deepEqual(fromApi(null), newDraft());
  assert.equal(fromApi({ conditions: [] }).conditions.length, 1, 'never an empty editor');
  assert.equal(fromApi({ sort: 'weird', conditions: [{ field: 'year', op: 'eq', value: 'x' }] }).sort, 'title');
});

test('dates use local midnight', () => {
  const iso = dateToIso('2026-03-01');
  assert.equal(iso, new Date(2026, 2, 1).toISOString());
  assert.equal(dateInputValue(iso), '2026-03-01');
  assert.equal(dateToIso('2026-02-30'), null);
  assert.equal(dateToIso('junk'), null);
  assert.equal(dateInputValue('junk'), '');
});

test('server error keys point at conditions', () => {
  assert.deepEqual(errorTarget('rules.conditions[3].value'), { index: 3, part: 'value' });
  assert.deepEqual(errorTarget('rules.conditions[0].op'), { index: 0, part: 'op' });
  assert.deepEqual(errorTarget('rules.conditions[2]'), { index: 2, part: 'value' });
  assert.deepEqual(errorTarget('rules.limit'), { index: -1, part: 'limit' });
  assert.deepEqual(errorTarget('name'), { index: -1, part: 'rules' });
});

test('summary line', () => {
  const s = describe({
    match: 'all', sort: 'year', order: 'desc', limit: 100,
    conditions: [{ field: 'genre', op: 'contains', value: 'rock' }, { field: 'year', op: 'between', value: [1990, 1999] }, { field: 'favorite', op: 'is_true', value: null }],
  });
  assert.equal(s, 'Genre contains “rock” and Year is between 1990 and 1999 and Favorite is true · by year, descending · up to 100 songs');
  assert.equal(describe({ match: 'any', sort: 'random', order: 'desc', limit: null, conditions: [{ field: 'added_at', op: 'in_last_days', value: 7 }] }),
    'Date added in the last 7 days · in random order');
  assert.equal(describe({ match: 'all', sort: 'title', conditions: [{ field: 'duration_seconds', op: 'gt', value: 600 }] }), 'Duration > 10:00 · by title');
  assert.equal(describe(null), '');
});
