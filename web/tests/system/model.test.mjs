import { test } from 'node:test';
import assert from 'node:assert/strict';

import {
  volumeUsage, worstVolumeFraction, usageLevel, volumeTitle, sortSpaces, usersTotal, quotaState, WARN_AT, CRITICAL_AT,
} from '../../assets/js/apps/system/model.js';

test('volumeUsage uses the server used value and clamps', () => {
  assert.deepEqual(volumeUsage({ total_bytes: 1000, free_bytes: 300, used_bytes: 650 }), { total: 1000, free: 300, used: 650, fraction: 0.65 });
  // No used value: derive it from total - free.
  assert.deepEqual(volumeUsage({ total_bytes: 1000, free_bytes: 400 }), { total: 1000, free: 400, used: 600, fraction: 0.6 });
  // Garbage and empty input never produce NaN.
  assert.deepEqual(volumeUsage({}), { total: 0, free: 0, used: 0, fraction: 0 });
  assert.deepEqual(volumeUsage(null), { total: 0, free: 0, used: 0, fraction: 0 });
  assert.equal(volumeUsage({ total_bytes: 10, used_bytes: 50 }).fraction, 1);
  assert.equal(volumeUsage({ total_bytes: '2000', free_bytes: '500', used_bytes: 'x' }).used, 1500);
});

test('worstVolumeFraction and usageLevel', () => {
  assert.equal(worstVolumeFraction([]), 0);
  assert.equal(worstVolumeFraction(undefined), 0);
  assert.equal(worstVolumeFraction([{ total_bytes: 100, used_bytes: 20 }, { total_bytes: 100, used_bytes: 90 }]), 0.9);
  assert.equal(usageLevel(0.5), 'ok');
  assert.equal(usageLevel(WARN_AT), 'warning');
  assert.equal(usageLevel(0.94), 'warning');
  assert.equal(usageLevel(CRITICAL_AT), 'danger');
  assert.equal(usageLevel(1), 'danger');
});

test('volumeTitle turns server labels into readable titles', () => {
  assert.equal(volumeTitle('data'), 'Data');
  assert.equal(volumeTitle('data, storage'), 'Data · Storage');
  assert.equal(volumeTitle('library: Movies'), 'Movies');
  assert.equal(volumeTitle('data, storage, library: TV Shows +2'), 'Data · Storage · TV Shows +2 more');
  assert.equal(volumeTitle('library'), 'Library');
  assert.equal(volumeTitle('library: '), 'Library');
  assert.equal(volumeTitle(''), 'Disk');
  assert.equal(volumeTitle(undefined), 'Disk');
  assert.equal(volumeTitle('scratch'), 'scratch');
});

test('sortSpaces puts home first, then by usage and name', () => {
  const out = sortSpaces([
    { id: 'b', kind: 'library', name: 'Beta', usage_bytes: 10 },
    { id: 'a', kind: 'library', name: 'Alpha', usage_bytes: 10 },
    { id: 'h', kind: 'home', name: 'Home', usage_bytes: 1 },
    { id: 'c', kind: 'library', name: 'Gamma', usage_bytes: 500 },
  ]);
  assert.deepEqual(out.map((s) => s.id), ['h', 'c', 'a', 'b']);
  assert.deepEqual(sortSpaces(undefined), []);
  const input = [{ kind: 'library', name: 'x' }];
  assert.notEqual(sortSpaces(input), input, 'must not sort in place');
});

test('usersTotal and quotaState', () => {
  assert.equal(usersTotal([{ usage_bytes: 5 }, { usage_bytes: '7' }, {}]), 12);
  assert.equal(usersTotal(null), 0);
  assert.deepEqual(quotaState({ usage_bytes: 90, quota_bytes: 100 }), { used: 90, quota: 100, left: 10, fraction: 0.9, level: 'warning' });
  assert.deepEqual(quotaState({ usage_bytes: 120, quota_bytes: 100 }), { used: 120, quota: 100, left: 0, fraction: 1.2, level: 'danger' });
  assert.deepEqual(quotaState({ usage_bytes: 5, quota_bytes: 0 }), { used: 5, quota: 0, left: 0, fraction: 0, level: 'ok' });
  assert.deepEqual(quotaState(undefined), { used: 0, quota: 0, left: 0, fraction: 0, level: 'ok' });
});
