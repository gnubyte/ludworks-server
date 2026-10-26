import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  formatBytes, parseBytes, formatDuration, formatDurationLong, formatRelative, formatPercent, initials, hueFor,
  formatFingerprint, plural, formatSpeed,
} from '../../assets/js/core/format.js';

test('formatBytes uses binary units and trims zeros', () => {
  assert.equal(formatBytes(0), '0 B');
  assert.equal(formatBytes(1023), '1023 B');
  assert.equal(formatBytes(1024), '1 KiB');
  assert.equal(formatBytes(1536), '1.5 KiB');
  assert.equal(formatBytes(10 * 1024 ** 2 + 512 * 1024), '10.5 MiB');
  assert.equal(formatBytes(150 * 1024 ** 3), '150 GiB');
  assert.equal(formatBytes(2 * 1024 ** 4), '2 TiB');
  assert.equal(formatBytes(1.234 * 1024 ** 3), '1.23 GiB');
  assert.equal(formatBytes(-2048), '-2 KiB');
  assert.equal(formatBytes(NaN), '—');
  assert.equal(formatBytes(1024 ** 3, { decimals: 1 }), '1 GiB');
  assert.equal(formatSpeed(1024 ** 2), '1 MiB/s');
});

test('parseBytes accepts common notations', () => {
  assert.equal(parseBytes('0'), 0);
  assert.equal(parseBytes('2048'), 2048);
  assert.equal(parseBytes('1 KB'), 1024);
  assert.equal(parseBytes('1.5GiB'), 1.5 * 1024 ** 3);
  assert.equal(parseBytes('500 gb'), 500 * 1024 ** 3);
  assert.equal(parseBytes('2t'), 2 * 1024 ** 4);
  assert.equal(parseBytes(' 10 MiB '), 10 * 1024 ** 2);
  assert.equal(parseBytes('5 b'), 5);
  assert.equal(parseBytes(formatBytes(150 * 1024 ** 3)), 150 * 1024 ** 3, 'round-trips formatBytes');
  for (const bad of ['', 'abc', '-1', '1..2 GB', '5 ib', '1 XB', '1e3']) assert.equal(parseBytes(bad), null, bad);
  assert.equal(parseBytes(42.4), 42);
  assert.equal(parseBytes(-1), null);
});

test('durations', () => {
  assert.equal(formatDuration(0), '0:00');
  assert.equal(formatDuration(59.9), '0:59');
  assert.equal(formatDuration(61), '1:01');
  assert.equal(formatDuration(3723), '1:02:03');
  assert.equal(formatDuration(45, { alwaysHours: true }), '0:00:45');
  assert.equal(formatDuration(-5), '0:00');
  assert.equal(formatDuration('x'), '0:00');
  assert.equal(formatDurationLong(0), '0s');
  assert.equal(formatDurationLong(45), '45s');
  assert.equal(formatDurationLong(3 * 86400 + 4 * 3600 + 5 * 60), '3d 4h');
  assert.equal(formatDurationLong(3 * 86400 + 4 * 3600 + 5 * 60, 3), '3d 4h 5m');
  assert.equal(formatDurationLong(7260), '2h 1m');
});

test('relative time', () => {
  const now = Date.parse('2026-10-26T12:00:00Z');
  assert.equal(formatRelative('2026-10-26T11:59:40Z', now), 'just now');
  assert.match(formatRelative('2026-10-26T11:55:00Z', now), /5 minutes ago/);
  assert.match(formatRelative('2026-10-26T09:00:00Z', now), /3 hours ago/);
  assert.match(formatRelative('2026-10-29T12:00:00Z', now), /in 3 days/);
  assert.match(formatRelative('2026-10-25T12:00:00Z', now), /yesterday/);
  assert.equal(formatRelative(null, now), '—');
  assert.equal(formatRelative('not a date', now), '—');
});

test('misc helpers', () => {
  assert.equal(formatPercent(1, 3), '33%');
  assert.equal(formatPercent(1, 3, 1), '33.3%');
  assert.equal(formatPercent(5, 0), '0%');
  assert.equal(initials('Ada Lovelace'), 'AL');
  assert.equal(initials('root'), 'RO');
  assert.equal(initials('jane.doe@example.com'), 'JC');
  assert.equal(initials(''), '?');
  // Regression: punctuation became an initial ("GitHub (kdbx)" → "G(").
  assert.equal(initials('GitHub (kdbx)'), 'GK');
  assert.equal(initials('(personal)'), 'PE');
  assert.equal(initials('Bank – checking #2'), 'B2');
  assert.equal(initials('— !! —'), '?');
  assert.equal(initials('Émile Zola'), 'ÉZ');
  assert.equal(initials('𝒳avier'), '𝒳A');
  assert.equal(initials(null), '?');
  assert.equal(hueFor('alice'), hueFor('alice'));
  assert.ok(hueFor('alice') >= 0 && hueFor('alice') < 360);
  assert.equal(formatFingerprint('ab12cd'), 'AB:12:CD');
  assert.equal(formatFingerprint('AB:12:cd'), 'AB:12:CD');
  assert.match(plural(1, 'file'), /^1 file$/);
  assert.match(plural(3, 'file'), /^3 files$/);
});
