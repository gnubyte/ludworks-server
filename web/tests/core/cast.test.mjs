// core/cast.js: the session reducer, position interpolation, request building, device and error
// helpers, the controller (long poll, controls, stop, sign-out) against a fake API, and the DOM
// paths of the device picker (incl. PIN pairing) and the control strip under a minimal DOM.
// Also the admin "Casting" card helpers (manual devices, media base URL).
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { installDom, settle } from '../files/minidom.mjs';

installDom();
globalThis.window.addEventListener ??= () => {};
const cast = await import('../../assets/js/core/cast.js');
const realUi = await import('../../assets/js/core/ui.js');
const { store } = await import('../../assets/js/core/store.js');
const admin = await import('../../assets/js/apps/admin/settings-forms.js');

const {
  reduceSession, interpolatePosition, buildStartRequest, buildControl, normalizeItems, currentItem, sessionDuration,
  canSeek, hasNext, hasPrevious, deviceLabel, deviceDetail, deviceBadges, deviceStatus, sortDevices, kindLabel,
  kindIcon, supportsItems, stateLabel, errorText, castErrorMessage, nativeCastOptions, attachSession, startSession,
  findSession, castControls, castButton, CONTROL_ACTIONS,
} = cast;

// Controllers detach when the signed-in user changes: sign in before the first one exists.
store.set({ user: { id: 'user0000001', role: 'user' } });

const FILE = 'file00000001';
const FILE2 = 'file00000002';
const CHANNEL = 'chan00000001';
const tv = { id: 'chromecast-livingroom', name: 'Living Room TV', kind: 'chromecast', model: 'Chromecast Ultra', host: '192.168.1.20', port: 8009, capabilities: ['audio', 'video'], source: 'discovered', requires_pairing: false, paired: false, available: true, busy: false, session_id: null };

function session(over = {}) {
  return {
    id: 'sess00000001', device: tv, owner_id: 'user0000001', state: 'playing', error: null,
    items: [{ kind: 'media', id: FILE, title: 'Big Buck Bunny', subtitle: '2008', art_url: `/api/v1/media/${FILE}/poster?v=1`, duration_seconds: 600 }],
    index: 0, position_seconds: 30, duration_seconds: 600, volume: 0.5, muted: false, repeat: 'off', shuffle: false,
    version: 1, created_at: '2026-10-27T10:00:00.000Z', updated_at: '2026-10-27T10:00:00.000Z', ...over,
  };
}

function apiError(status, code, message = code, details = null) {
  return Object.assign(new Error(message), { name: 'ApiError', status, code, details });
}

/** A core/api.js look-alike: routes for plain requests, a queue for long polls. */
function fakeApi() {
  const calls = [];
  const polls = [];
  const routes = [];
  const api = {
    calls, polls,
    route(method, path, fn) { routes.push({ method, path, fn }); },
    request(method, path, opts = {}) {
      calls.push({ method, path, body: opts.body, query: opts.query });
      if (method === 'GET' && opts.query && 'wait_version' in opts.query) {
        return new Promise((resolve, reject) => {
          polls.push({ resolve, reject, query: opts.query, signal: opts.signal });
          opts.signal?.addEventListener('abort', () => reject(Object.assign(new Error('aborted'), { name: 'AbortError' })));
        });
      }
      const r = routes.find((x) => x.method === method && (typeof x.path === 'string' ? x.path === path : x.path.test(path)));
      if (!r) return Promise.reject(apiError(404, 'not_found', `no route ${method} ${path}`));
      return Promise.resolve().then(() => r.fn(opts.body, path));
    },
    get: (path, opts) => api.request('GET', path, opts),
    post: (path, body, opts) => api.request('POST', path, { ...opts, body }),
    put: (path, body, opts) => api.request('PUT', path, { ...opts, body }),
    del: (path, opts) => api.request('DELETE', path, opts),
  };
  return api;
}

/* ------------------------------------------------------------- pure logic */

test('reduceSession ignores stale versions and keeps the receive time', () => {
  const a = session({ version: 3 });
  const s1 = reduceSession(null, a, 1000);
  assert.deepEqual(s1, { session: a, receivedAt: 1000 });
  assert.equal(reduceSession(s1, { ...a, version: 2, state: 'paused' }, 2000), s1, 'older version');
  const same = reduceSession(s1, { ...a, position_seconds: 42 }, 3000);
  assert.equal(same.receivedAt, 3000, 'same version: a fresher position sample');
  assert.equal(same.session.position_seconds, 42);
  assert.equal(reduceSession(s1, { ...a, position_seconds: 42 }, 3000, { strict: true }), s1, 'strict mode needs a newer version');
  const newer = reduceSession(s1, { ...a, version: 4, state: 'paused' }, 4000);
  assert.equal(newer.session.state, 'paused');
  assert.equal(newer.receivedAt, 4000);
  const other = reduceSession(newer, session({ id: 'sess00000002', version: 1 }), 5000);
  assert.equal(other.session.id, 'sess00000002', 'another session replaces the snapshot');
  const stopped = reduceSession(s1, { ...a, version: 5, state: 'stopped', error: { code: 'replaced', message: 'x' } }, 6000);
  assert.equal(reduceSession(stopped, { ...a, version: 6, state: 'playing' }, 7000), stopped, 'a stopped session never comes back');
  assert.equal(reduceSession(s1, null, 1), s1);
  assert.equal(reduceSession(s1, { version: 9 }, 1), s1, 'no id: ignored');
  assert.equal(reduceSession(null, null, 1), null);
});

test('interpolatePosition advances only while playing and clamps to the duration', () => {
  const snap = (over, at = 10_000) => ({ session: session(over), receivedAt: at });
  assert.equal(interpolatePosition(snap({ state: 'playing', position_seconds: 30 }), 15_000), 35);
  assert.equal(interpolatePosition(snap({ state: 'paused', position_seconds: 30 }), 15_000), 30);
  assert.equal(interpolatePosition(snap({ state: 'buffering', position_seconds: 30 }), 15_000), 30);
  assert.equal(interpolatePosition(snap({ state: 'playing', position_seconds: 598 }), 20_000), 600, 'clamped to duration');
  assert.equal(interpolatePosition(snap({ state: 'playing', position_seconds: 30 }), 5_000), 30, 'clock behind the sample');
  // Unknown duration (a channel without a program length): no clamp.
  const live = { session: session({ duration_seconds: null, items: [{ kind: 'channel', id: CHANNEL, title: 'News', subtitle: '', art_url: null, duration_seconds: null }], position_seconds: 100 }), receivedAt: 0 };
  assert.equal(interpolatePosition(live, 50_000), 150);
  // The item's duration is used when the session has none.
  const itemDur = { session: session({ duration_seconds: null, position_seconds: 590 }), receivedAt: 0 };
  assert.equal(interpolatePosition(itemDur, 60_000), 600);
  assert.equal(interpolatePosition({ session: session({ position_seconds: -5, state: 'paused' }), receivedAt: 0 }, 1), 0);
  assert.equal(interpolatePosition({ session: session({ position_seconds: 'x', state: 'paused' }), receivedAt: 0 }, 1), 0);
  assert.equal(interpolatePosition(null, 1), 0);
});

test('buildStartRequest validates and omits unset options', () => {
  assert.deepEqual(buildStartRequest({ deviceId: tv.id, items: [{ kind: 'media', id: FILE, title: 'ignored' }] }),
    { device_id: tv.id, items: [{ kind: 'media', id: FILE }] });
  assert.deepEqual(buildStartRequest({
    deviceId: tv.id, items: [{ kind: 'track', id: FILE }, { kind: 'track', id: FILE2 }], index: 1, positionSeconds: 12.34567,
    repeat: 'all', shuffle: true, volume: 0.25,
  }), { device_id: tv.id, items: [{ kind: 'track', id: FILE }, { kind: 'track', id: FILE2 }], index: 1, position_seconds: 12.346, repeat: 'all', shuffle: true, volume: 0.25 });
  assert.deepEqual(buildStartRequest({ deviceId: tv.id, items: [{ kind: 'channel', id: CHANNEL }], index: null, positionSeconds: undefined, repeat: null }),
    { device_id: tv.id, items: [{ kind: 'channel', id: CHANNEL }] });
  const bad = [
    [{ items: [{ kind: 'media', id: FILE }] }, /deviceId/],
    [{ deviceId: 'a/b', items: [{ kind: 'media', id: FILE }] }, /deviceId/],
    [{ deviceId: tv.id, items: [] }, /at least one/],
    [{ deviceId: tv.id }, /at least one/],
    [{ deviceId: tv.id, items: Array.from({ length: 1001 }, () => ({ kind: 'track', id: FILE })) }, /at most 1000/],
    [{ deviceId: tv.id, items: [{ kind: 'video', id: FILE }] }, /kind/],
    [{ deviceId: tv.id, items: [{ kind: 'media', id: 'short' }] }, /id/],
    [{ deviceId: tv.id, items: [{ kind: 'media', id: FILE }], index: 1 }, /index/],
    [{ deviceId: tv.id, items: [{ kind: 'media', id: FILE }], index: -1 }, /index/],
    [{ deviceId: tv.id, items: [{ kind: 'media', id: FILE }], positionSeconds: -1 }, /positionSeconds/],
    [{ deviceId: tv.id, items: [{ kind: 'media', id: FILE }], positionSeconds: NaN }, /positionSeconds/],
    [{ deviceId: tv.id, items: [{ kind: 'media', id: FILE }], repeat: 'forever' }, /repeat/],
    [{ deviceId: tv.id, items: [{ kind: 'media', id: FILE }], shuffle: 'yes' }, /shuffle/],
    [{ deviceId: tv.id, items: [{ kind: 'media', id: FILE }], volume: 1.5 }, /volume/],
  ];
  for (const [opts, re] of bad) assert.throws(() => buildStartRequest(opts), re, JSON.stringify(opts).slice(0, 80));
  assert.equal(normalizeItems(Array.from({ length: 1000 }, () => ({ kind: 'track', id: FILE }))).length, 1000);
});

test('buildControl builds every action and rejects bad arguments', () => {
  const ok = {
    play: [{}, { action: 'play' }],
    pause: [{}, { action: 'pause' }],
    toggle: [{}, { action: 'toggle' }],
    stop: [{}, { action: 'stop' }],
    next: [{}, { action: 'next' }],
    previous: [{}, { action: 'previous' }],
    seek: [{ positionSeconds: 61.5 }, { action: 'seek', position_seconds: 61.5 }],
    jump: [{ index: 3 }, { action: 'jump', index: 3 }],
    volume: [{ volume: 0.333 }, { action: 'volume', volume: 0.33 }],
    mute: [{ muted: true }, { action: 'mute', muted: true }],
    repeat: [{ repeat: 'one' }, { action: 'repeat', repeat: 'one' }],
    shuffle: [{ shuffle: false }, { action: 'shuffle', shuffle: false }],
  };
  assert.deepEqual(Object.keys(ok).sort(), [...CONTROL_ACTIONS].sort(), 'every API action is covered');
  for (const [action, [args, body]] of Object.entries(ok)) assert.deepEqual(buildControl(action, args), body, action);
  assert.deepEqual(buildControl('seek', { position_seconds: 0 }), { action: 'seek', position_seconds: 0 });
  for (const [action, args] of [
    ['seek', {}], ['seek', { positionSeconds: -1 }], ['jump', { index: 1.5 }], ['jump', {}], ['volume', { volume: 2 }],
    ['volume', { volume: '0.5' }], ['mute', { muted: 'true' }], ['repeat', { repeat: 'twice' }], ['shuffle', {}], ['rewind', {}],
  ]) assert.throws(() => buildControl(action, args), Error, `${action} ${JSON.stringify(args)}`);
});

test('queue helpers: current item, duration, seek and next/previous availability', () => {
  const two = [{ kind: 'track', id: FILE, title: 'A', duration_seconds: 200 }, { kind: 'track', id: FILE2, title: 'B', duration_seconds: 180 }];
  assert.equal(currentItem(session({ items: two, index: 1 })).title, 'B');
  assert.equal(currentItem(session({ items: two, index: 2 })), null);
  assert.equal(currentItem(null), null);
  assert.equal(sessionDuration(session({ duration_seconds: null, items: two, index: 1 })), 180);
  assert.equal(sessionDuration(session({ duration_seconds: 0, items: [{ kind: 'channel', id: CHANNEL }] })), null);

  assert.equal(canSeek(session()), true);
  assert.equal(canSeek(session({ state: 'idle' })), false);
  assert.equal(canSeek(session({ items: [{ kind: 'channel', id: CHANNEL, duration_seconds: 1800 }], duration_seconds: 1800 })), false, 'channels never seek here');
  assert.equal(canSeek(session({ duration_seconds: null, items: [{ kind: 'media', id: FILE }] })), false, 'unknown duration');

  assert.equal(hasNext(session({ items: two, index: 0 })), true);
  assert.equal(hasNext(session({ items: two, index: 1 })), false, 'last item, repeat off');
  assert.equal(hasNext(session({ items: two, index: 1, repeat: 'all' })), true);
  assert.equal(hasNext(session({ items: two, index: 1, shuffle: true })), true);
  assert.equal(hasNext(session({ index: 0 })), false, 'single item');
  assert.equal(hasNext(session({ items: two, index: 0, state: 'stopped' })), false);
  assert.equal(hasPrevious(session({ items: two, index: 1 })), true);
  assert.equal(hasPrevious(session({ items: two, index: 0 })), true, 'restarts a seekable item');
  assert.equal(hasPrevious(session({ items: [{ kind: 'channel', id: CHANNEL }], duration_seconds: null })), false);
});

test('device helpers: labels, badges, status, sorting and capabilities', () => {
  assert.equal(kindLabel('chromecast'), 'Chromecast');
  assert.equal(kindLabel('airplay'), 'AirPlay');
  assert.equal(kindIcon('airplay'), 'airplay');
  assert.equal(kindIcon('chromecast'), 'cast');
  assert.equal(deviceLabel(tv), 'Living Room TV');
  assert.equal(deviceLabel({ name: ' ', host: '10.0.0.5' }), '10.0.0.5');
  assert.equal(deviceLabel({}), 'Unnamed device');
  assert.equal(deviceDetail(tv), 'Chromecast · Chromecast Ultra · 192.168.1.20');
  assert.equal(deviceDetail({ kind: 'airplay', host: 'tv.local', name: 'tv.local', source: 'manual' }), 'AirPlay · added manually');

  const speaker = { id: 'airplay-0011223344aa', name: 'Kitchen', kind: 'airplay', capabilities: ['audio'], available: true, busy: false };
  const appleTV = { id: 'airplay-0011223344bb', name: 'Bedroom', kind: 'airplay', capabilities: ['audio', 'video'], available: true, requires_pairing: true, paired: false };
  const video = [{ kind: 'media', id: FILE }];
  const audio = [{ kind: 'track', id: FILE }];
  assert.equal(supportsItems(speaker, video), false);
  assert.equal(supportsItems(speaker, audio), true);
  assert.equal(supportsItems(speaker, [{ kind: 'channel', id: CHANNEL }]), false);
  assert.equal(supportsItems({ capabilities: null }, video), true, 'unknown capabilities: the server decides');
  assert.equal(supportsItems(tv, video), true);

  assert.deepEqual(deviceBadges(speaker, video).map((b) => b.label), ['Audio only']);
  assert.deepEqual(deviceBadges(appleTV, video).map((b) => b.label), ['Needs pairing']);
  assert.deepEqual(deviceBadges({ ...appleTV, paired: true, busy: true }, video).map((b) => b.label), ['In use', 'Paired']);
  assert.deepEqual(deviceBadges({ ...tv, busy: true, session_id: 'sess00000001' }).map((b) => b.label), ['Casting']);
  assert.deepEqual(deviceBadges({ ...tv, available: false }).map((b) => b.label), ['Unavailable']);
  assert.equal(deviceStatus(tv).key, 'ready');
  assert.equal(deviceStatus({ ...tv, available: false, busy: true }).key, 'unavailable');
  assert.equal(deviceStatus(speaker, video).label, 'Audio only');
  assert.equal(deviceStatus({ ...tv, busy: true }).label, 'In use');

  const sorted = sortDevices([{ name: 'b', available: true }, { name: 'A', available: false }, { name: 'a', available: true }, { name: 'C', available: true }]);
  assert.deepEqual(sorted.map((d) => `${d.name}${d.available ? '' : '!'}`), ['a', 'b', 'C', 'A!']);
  assert.deepEqual(sortDevices(null), []);
});

test('state labels and error texts', () => {
  assert.equal(stateLabel(session({ state: 'buffering' })), 'Buffering…');
  assert.equal(stateLabel(session({ state: 'stopped' })), 'Stopped');
  assert.equal(stateLabel(null), '');
  for (const code of ['replaced', 'idle', 'access_revoked', 'cast_disabled', 'device_unavailable', 'playback_failed', 'unsupported_media',
    'pairing_required', 'pairing_unsupported', 'interrupted', 'cast_unavailable', 'session_limit']) {
    assert.ok(errorText(code).length > 10, code);
  }
  assert.match(errorText('cast_unavailable'), /media port|base URL|public URL/);
  assert.match(errorText('pairing_unsupported'), /Allow speaker access/);
  assert.equal(errorText('weird', 'Server says no'), 'Server says no');
  assert.equal(errorText('weird'), 'Casting failed.');
  assert.match(castErrorMessage(apiError(503, 'cast_unavailable', 'nope')), /media port/);
  assert.equal(castErrorMessage(apiError(409, 'conflict', 'Live channels cannot be seeked')), 'Live channels cannot be seeked');
  assert.equal(castErrorMessage(apiError(422, 'validation_failed', 'Validation failed', { fields: { pin: 'Wrong PIN' } })), 'Wrong PIN');
  assert.match(castErrorMessage(Object.assign(apiError(429, 'rate_limited'), { retryAfter: 7 })), /7 s/);
  assert.equal(castErrorMessage(new TypeError('repeat must be off, all or one.')), 'repeat must be off, all or one.');
  assert.equal(castErrorMessage('plain'), 'plain');
});

test('nativeCastOptions offers only what the browser has', () => {
  assert.deepEqual(nativeCastOptions(null, {}), []);
  const plain = {};
  assert.deepEqual(nativeCastOptions(plain, {}), []);
  let prompted = 0;
  const chrome = { remote: { prompt: () => { prompted += 1; return Promise.resolve(); } }, disableRemotePlayback: false };
  const opts = nativeCastOptions(chrome, {});
  assert.deepEqual(opts.map((o) => o.id), ['remote-playback']);
  opts[0].run();
  assert.equal(prompted, 1);
  assert.deepEqual(nativeCastOptions({ ...chrome, disableRemotePlayback: true }, {}), []);
  let picked = 0;
  const safari = { webkitShowPlaybackTargetPicker: () => { picked += 1; }, remote: { prompt() {} } };
  const s = nativeCastOptions(safari, { WebKitPlaybackTargetAvailabilityEvent: function WebKitPlaybackTargetAvailabilityEvent() {} });
  assert.deepEqual(s.map((o) => o.id), ['webkit-airplay'], 'Safari: one AirPlay entry, not two');
  s[0].run();
  assert.equal(picked, 1);
  assert.deepEqual(nativeCastOptions(safari, {}).map((o) => o.id), ['remote-playback'], 'no WebKit event: not Safari');
});

/* ------------------------------------------------------------- controller */

test('controller: long poll with wait_version, change and ended events, detach', async () => {
  const api = fakeApi();
  let now = 1000;
  const c = attachSession(api, session({ version: 4 }), { now: () => now });
  const changes = [];
  const ended = [];
  c.on('change', (s) => changes.push(s));
  c.on('ended', (e) => ended.push(e));
  assert.equal(api.polls.length, 1);
  assert.deepEqual(api.polls[0].query, { wait_version: 4, timeout: 20 });
  assert.equal(c.position(), 30);
  now = 3000;
  assert.equal(c.position(), 32, 'interpolated while playing');

  api.polls[0].resolve(session({ version: 5, state: 'paused', position_seconds: 33 }));
  await settle();
  assert.equal(c.session.state, 'paused');
  assert.equal(changes.length, 1);
  assert.equal(api.polls.length, 2);
  assert.equal(api.polls[1].query.wait_version, 5);
  now = 9000;
  assert.equal(c.position(), 33, 'paused: no interpolation');

  // Stale answer (older version) changes nothing.
  api.polls[1].resolve(session({ version: 4, state: 'playing' }));
  await settle();
  assert.equal(c.session.state, 'paused');
  assert.equal(changes.length, 1);

  api.polls[2].resolve(session({ version: 9, state: 'stopped', error: { code: 'replaced', message: 'Replaced' } }));
  await settle();
  assert.equal(ended.length, 1);
  assert.equal(ended[0].reason, 'replaced');
  assert.equal(ended[0].byUser, false);
  assert.equal(c.ended, true);
  assert.equal(api.polls.length, 3, 'no polling after the end');

  // detach: the in-flight poll is aborted and listeners dropped.
  const d = attachSession(api, session({ id: 'sess00000002' }), { now: () => now });
  const seen = [];
  d.on('change', (s) => seen.push(s));
  const poll = api.polls.at(-1);
  d.detach();
  assert.equal(poll.signal.aborted, true);
  poll.resolve(session({ id: 'sess00000002', version: 7 }));
  await settle();
  assert.equal(seen.length, 0);
  assert.equal(d.attached, false);
});

test('controller: a vanished session (404) ends it; a signed-out user detaches everything', async () => {
  const api = fakeApi();
  const c = attachSession(api, session());
  const ended = [];
  c.on('ended', (e) => ended.push(e));
  api.polls[0].reject(apiError(404, 'not_found'));
  await settle();
  assert.deepEqual(ended.map((e) => e.reason), ['stopped']);
  assert.equal(c.session.state, 'stopped');

  const a = attachSession(api, session({ id: 'sess00000003' }));
  const b = attachSession(api, session({ id: 'sess00000004' }));
  const polls = api.polls.slice(-2);
  store.set({ user: null });
  assert.equal(a.attached, false);
  assert.equal(b.attached, false);
  assert.ok(polls.every((p) => p.signal.aborted));
  store.set({ user: { id: 'user0000001', role: 'user' } });
});

test('controller: optimistic controls, shared identical calls, errors roll back', async () => {
  const api = fakeApi();
  let now = 0;
  let version = 1;
  const posts = [];
  api.route('POST', '/cast/sessions/sess00000001/control', (body) => {
    posts.push(body);
    if (body.action === 'next') throw apiError(409, 'conflict', 'There is no next item');
    if (body.action === 'mute') throw apiError(502, 'device_unavailable', 'gone');
    version += 1;
    if (body.action === 'pause') return session({ version, state: 'paused', position_seconds: 31 });
    if (body.action === 'volume') return session({ version, volume: body.volume });
    return session({ version });
  });
  api.route('GET', '/cast/sessions/sess00000001', () => session({ version }));
  const c = attachSession(api, session(), { now: () => now });
  const states = [];
  c.on('change', (s) => states.push(s.state));
  const errors = [];
  c.on('error', (e) => errors.push(e.code));

  now = 1000;
  const p1 = c.pause();
  const p2 = c.pause();
  assert.equal(p1, p2, 'identical concurrent calls share one request');
  assert.equal(c.session.state, 'paused', 'optimistic');
  assert.equal(c.session.position_seconds, 31, 'frozen at the interpolated position');
  await p1;
  assert.equal(posts.length, 1);
  assert.equal(c.session.version, 2);
  assert.deepEqual(states, ['paused', 'paused']);

  await c.setVolume(0.8);
  assert.equal(c.session.volume, 0.8);
  assert.deepEqual(posts.at(-1), { action: 'volume', volume: 0.8 });

  await assert.rejects(c.next(), /no next item/);
  assert.deepEqual(errors, ['conflict']);

  // A failed optimistic change is undone with the server's state.
  const muting = c.setMuted(true);
  assert.equal(c.session.muted, true, 'optimistic');
  await assert.rejects(muting, /gone/);
  await settle();
  assert.equal(c.session.muted, false, 'rolled back from GET /cast/sessions/{id}');
  assert.deepEqual(errors, ['conflict', 'device_unavailable']);

  await c.seek(0);
  assert.deepEqual(posts.at(-1), { action: 'seek', position_seconds: 0 });
  await assert.rejects(c.jump(-1), /index/);
  await assert.rejects(c.setRepeat('sometimes'), /repeat/);
  c.detach();
});

test('controller: replace/insert items and stop', async () => {
  const api = fakeApi();
  const bodies = [];
  api.route('PUT', '/cast/sessions/sess00000001/items', (body) => { bodies.push(['PUT', body]); return session({ version: 2, items: [{ kind: 'channel', id: CHANNEL, title: 'News' }] }); });
  api.route('POST', '/cast/sessions/sess00000001/items', (body) => { bodies.push(['POST', body]); return session({ version: 3 }); });
  let deleted = 0;
  api.route('DELETE', '/cast/sessions/sess00000001', () => { deleted += 1; return null; });
  let now = 0;
  const c = attachSession(api, session(), { now: () => now });
  await c.replaceItems([{ kind: 'channel', id: CHANNEL }], { index: 0, positionSeconds: 5 });
  await c.insertItems([{ kind: 'media', id: FILE2 }], 1);
  assert.deepEqual(bodies, [
    ['PUT', { items: [{ kind: 'channel', id: CHANNEL }], index: 0, position_seconds: 5 }],
    ['POST', { items: [{ kind: 'media', id: FILE2 }], position: 1 }],
  ]);
  await assert.rejects(c.replaceItems([]), /at least one/);
  await assert.rejects(c.insertItems([{ kind: 'media', id: FILE2 }], -2), /position/);

  const ended = [];
  c.on('ended', (e) => ended.push(e));
  now = 10_000;
  const where = await c.stop();
  assert.equal(deleted, 1);
  assert.equal(where.index, 0);
  assert.equal(where.item.id, FILE);
  assert.equal(where.positionSeconds, 40, 'position at the stop');
  assert.deepEqual(ended.map((e) => [e.reason, e.byUser]), [['stopped', true]]);
  assert.equal(c.session.state, 'stopped');
  await assert.rejects(c.play(), /stopped/i);
  assert.deepEqual(await c.stop(), { index: 0, item: c.item(), positionSeconds: 40 }, 'stop is idempotent');

  // A failing DELETE keeps the session followed and rejects.
  const api2 = fakeApi();
  api2.route('DELETE', '/cast/sessions/sess00000001', () => { throw apiError(0, 'network_error', 'offline'); });
  const c2 = attachSession(api2, session());
  await assert.rejects(c2.stop(), /offline/);
  assert.equal(c2.ended, false);
  assert.equal(api2.polls.length, 2, 'polling resumed');
  c2.detach();
});

test('startSession posts the request and follows the new session; findSession matches the current item', async () => {
  const api = fakeApi();
  let body = null;
  api.route('POST', '/cast/sessions', (b) => { body = b; return session({ version: 1 }); });
  const c = await startSession(api, { deviceId: tv.id, items: [{ kind: 'media', id: FILE }], positionSeconds: 42 });
  assert.deepEqual(body, { device_id: tv.id, items: [{ kind: 'media', id: FILE }], position_seconds: 42 });
  assert.equal(c.id, 'sess00000001');
  assert.equal(api.polls.length, 1);
  c.detach();
  await assert.rejects(startSession(api, { deviceId: tv.id, items: [] }), /at least one/);

  api.route('GET', '/cast/sessions', () => [session({ id: 'sess00000009', state: 'stopped' }), session({ id: 'sess0000000a' })]);
  const found = await findSession(api, (s, it) => it?.kind === 'media' && it.id === FILE);
  assert.equal(found.id, 'sess0000000a', 'stopped sessions are skipped');
  assert.equal(await findSession(api, (s, it) => it?.kind === 'channel'), null);
  const broken = fakeApi();
  assert.equal(await findSession(broken, () => true), null, 'errors (e.g. casting off) are no session');
});

/* ------------------------------------------------------------------ DOM */

function fakeUi() {
  const toasts = [];
  const modals = [];
  const toast = (m, o = {}) => { toasts.push({ m: String(m), type: o.type || 'info' }); return () => {}; };
  toast.success = (m, o) => toast(m, { ...o, type: 'success' });
  toast.error = (m, o) => toast(m?.message || m, { ...o, type: 'error' });
  toast.info = (m, o) => toast(m, { ...o, type: 'info' });
  toast.warning = (m, o) => toast(m, { ...o, type: 'warning' });
  return {
    ...realUi, toasts, modals, toast,
    confirm: async () => true,
    modal(o) {
      let open = true;
      const m = { body: o.body, title: o.title, get isOpen() { return open; }, close() { if (open) { open = false; o.onClose?.(); } } };
      modals.push(m);
      return m;
    },
  };
}

const text = (el) => el.textContent.replace(/\s+/g, ' ').trim();
const buttonsOf = (root) => root.querySelectorAll('.cast-device');

test('castControls renders the session and drives the controller', async (t) => {
  const api = fakeApi();
  const posts = [];
  api.route('POST', '/cast/sessions/sess00000001/control', (body) => { posts.push(body); return session({ version: 2, state: body.action === 'toggle' ? 'paused' : 'playing' }); });
  api.route('DELETE', '/cast/sessions/sess00000001', () => null);
  const c = attachSession(api, session(), { now: () => 0 });
  const stops = [];
  const view = castControls(c, { ui: fakeUi(), variant: 'overlay', onStop: (w) => stops.push(w) });
  t.after(() => { view.destroy(); c.detach(); }); // also when an assertion fails (its timer would keep node alive)
  document.body.append(view.el);
  const el = view.el;
  assert.ok(el.classList.contains('cast-controls-overlay'));
  assert.match(text(el.querySelector('.cast-ctl-head')), /Casting to Living Room TV/);
  assert.equal(text(el.querySelector('.cast-ctl-title')), 'Big Buck Bunny');
  assert.equal(text(el.querySelector('.cast-ctl-sub')), '2008');
  assert.equal(text(el.querySelector('.cast-ctl-time')), '0:30 / 10:00');
  assert.equal(el.querySelector('.cast-ctl-seek').disabled, false);
  assert.equal(el.querySelector('.cast-ctl-art').firstChild.tagName, 'IMG');
  const play = el.querySelector('.cast-ctl-play');
  assert.equal(play.getAttribute('aria-label'), 'Pause');
  assert.equal(el.querySelector('.cast-ctl-state').textContent, 'Playing');
  const [prev, , next] = el.querySelectorAll('.cast-ctl-btn');
  assert.equal(next.disabled, true, 'single item, repeat off');
  assert.equal(prev.disabled, false, 'previous restarts a seekable item');

  play.click();
  assert.equal(el.querySelector('.cast-ctl-play').getAttribute('aria-label'), 'Play', 'optimistic pause');
  await settle();
  assert.deepEqual(posts, [{ action: 'toggle' }]);

  const seek = el.querySelector('.cast-ctl-seek');
  seek.value = '500';
  seek.dispatchEvent(new Event('input'));
  seek.dispatchEvent(new Event('change'));
  await settle();
  assert.deepEqual(posts.at(-1), { action: 'seek', position_seconds: 300 });

  el.querySelector('.cast-ctl-stop').click();
  await settle();
  assert.equal(stops.length, 1);
  assert.equal(stops[0].item.id, FILE);
  assert.equal(el.querySelector('.cast-ctl-stop').disabled, true);
  view.destroy();
  assert.equal(el.parentNode, null);

  // Channels: no seeking, the program as subtitle, an error banner when the device failed.
  const ch = attachSession(fakeApi(), session({
    id: 'sess00000005', state: 'error', error: { code: 'device_unavailable', message: 'gone' },
    items: [{ kind: 'channel', id: CHANNEL, title: '7 · News', subtitle: 'Evening News', art_url: null, duration_seconds: null }], duration_seconds: 1800, position_seconds: 60,
  }), { now: () => 0 });
  const v2 = castControls(ch, { ui: fakeUi() });
  t.after(() => { v2.destroy(); ch.detach(); });
  assert.equal(v2.el.querySelector('.cast-ctl-seek').disabled, true);
  assert.equal(v2.el.querySelector('.cast-ctl-banner').hidden, false);
  assert.match(text(v2.el.querySelector('.cast-ctl-banner')), /could not be reached/);
  assert.equal(v2.el.querySelector('.cast-ctl-art').firstChild.tagName, 'svg', 'fallback icon without art');
  v2.destroy();
  ch.detach();
});

test('device picker: lists devices, starts a session, pairs an Apple TV on demand', async () => {
  const api = fakeApi();
  const speaker = { id: 'airplay-0011223344aa', name: 'Kitchen', kind: 'airplay', model: 'AudioAccessory5,1', host: '192.168.1.31', port: 7000, capabilities: ['audio'], source: 'discovered', requires_pairing: false, paired: false, available: true, busy: false, session_id: null };
  const appleTV = { id: 'airplay-0011223344bb', name: 'Bedroom TV', kind: 'airplay', model: 'AppleTV11,1', host: '192.168.1.32', port: 7000, capabilities: ['audio', 'video'], source: 'discovered', requires_pairing: true, paired: false, available: true, busy: false, session_id: null };
  const off = { ...tv, id: 'chromecast-office', name: 'Office', available: false };
  api.route('GET', '/cast/devices', () => ({ enabled: true, devices: [speaker, off, appleTV, tv] }));
  const starts = [];
  let pairingNeeded = true;
  api.route('POST', '/cast/sessions', (b) => {
    starts.push(b);
    if (b.device_id === appleTV.id && pairingNeeded) throw apiError(422, 'pairing_required', 'Pair first');
    return session({ device: b.device_id === appleTV.id ? appleTV : tv });
  });
  api.route('POST', `/cast/devices/${appleTV.id}/pair`, () => ({ pairing_id: 'pair00000001' }));
  const confirms = [];
  api.route('POST', `/cast/devices/${appleTV.id}/pair/confirm`, (b) => {
    confirms.push(b);
    if (b.pin !== '4321') throw apiError(422, 'validation_failed', 'Validation failed', { fields: { pin: 'Wrong PIN' } });
    pairingNeeded = false;
    return null;
  });
  const ui = fakeUi();
  const sessions = [];
  const btn = castButton({
    api, ui, getItems: () => ({ items: [{ kind: 'media', id: FILE }], positionSeconds: 42 }),
    onSession: (ctl) => sessions.push(ctl),
  });
  assert.equal(btn.getAttribute('aria-label'), 'Cast to a device');
  btn.click();
  await settle();
  assert.equal(ui.modals.length, 1);
  const body = ui.modals[0].body;
  const rows = buttonsOf(body);
  assert.deepEqual(rows.map((b) => text(b.querySelector('.cast-device-name'))), ['Bedroom TV', 'Kitchen', 'Living Room TV', 'Office'], 'available first, by name');
  const byName = (n) => buttonsOf(body).find((b) => text(b.querySelector('.cast-device-name')) === n);
  assert.equal(byName('Kitchen').disabled, true, 'a speaker cannot play video');
  assert.match(text(byName('Kitchen')), /Audio only/);
  assert.equal(byName('Office').disabled, true, 'unavailable');
  assert.match(text(byName('Bedroom TV')), /Needs pairing/);
  assert.equal(body.querySelectorAll('.cast-pair-btn').length, 1);

  // A plain start.
  byName('Living Room TV').click();
  await settle();
  assert.deepEqual(starts.at(-1), { device_id: tv.id, items: [{ kind: 'media', id: FILE }], position_seconds: 42 });
  assert.equal(sessions.length, 1);
  assert.equal(ui.modals[0].isOpen, false, 'the picker closes');
  assert.ok(ui.toasts.some((t) => t.type === 'success' && /Living Room TV/.test(t.m)));
  sessions[0].detach();

  // pairing_required → PIN flow (wrong PIN, retry) → the start is retried once.
  btn.click();
  await settle();
  const body2 = ui.modals[1].body;
  buttonsOf(body2).find((b) => /Bedroom TV/.test(text(b))).click();
  await settle();
  assert.equal(starts.at(-1).device_id, appleTV.id);
  let pin = body2.querySelector('.cast-pin');
  assert.ok(pin, 'PIN form shown');
  assert.match(text(body2), /Pair with Bedroom TV/);
  pin.value = '1234';
  body2.querySelector('.cast-pair-form').dispatchEvent(new Event('submit'));
  await settle();
  assert.deepEqual(confirms.at(-1), { pairing_id: 'pair00000001', pin: '1234' });
  assert.match(text(body2), /didn’t match/);
  body2.querySelectorAll('button').find((b) => /Try again/.test(text(b))).click();
  await settle();
  pin = body2.querySelector('.cast-pin');
  pin.value = '4321';
  body2.querySelector('.cast-pair-form').dispatchEvent(new Event('submit'));
  await settle(10);
  assert.deepEqual(confirms.at(-1), { pairing_id: 'pair00000001', pin: '4321' });
  assert.equal(starts.at(-1).device_id, appleTV.id);
  assert.equal(sessions.length, 2, 'started after pairing');
  assert.equal(ui.modals[1].isOpen, false);
  sessions[1].detach();
  btn.destroy();
});

test('device picker: casting turned off, empty network and start errors', async () => {
  const api = fakeApi();
  api.route('GET', '/cast/devices', () => ({ enabled: false, devices: [] }));
  const ui = fakeUi();
  castButton({ api, ui, getItems: () => ({ items: [{ kind: 'channel', id: CHANNEL }] }) }).click();
  await settle();
  assert.match(text(ui.modals[0].body), /Casting is turned off/);
  assert.equal(api.calls.filter((x) => x.path === '/cast/devices/refresh').length, 0, 'no browse while off');

  const api2 = fakeApi();
  let refreshed = 0;
  api2.route('GET', '/cast/devices', () => ({ enabled: true, devices: [] }));
  api2.route('POST', '/cast/devices/refresh', () => { refreshed += 1; return { enabled: true, devices: refreshed > 1 ? [tv] : [] }; });
  api2.route('POST', '/cast/sessions', () => { throw apiError(503, 'cast_unavailable', 'unavailable'); });
  const ui2 = fakeUi();
  castButton({ api: api2, ui: ui2, getItems: () => ({ items: [{ kind: 'channel', id: CHANNEL }] }) }).click();
  await settle(10);
  assert.equal(refreshed, 1, 'an empty first answer browses once at once');
  const body = ui2.modals[0].body;
  assert.match(text(body), /No devices found/);
  assert.match(text(body), /host networking/);
  body.querySelector('.cast-refresh').click();
  await settle(10);
  assert.equal(refreshed, 2);
  buttonsOf(body)[0].click();
  await settle(10);
  assert.match(text(body), /media port|public URL/, 'the reachability problem is explained');
  assert.equal(ui2.modals[0].isOpen, true, 'the picker stays open on errors');
});

/* ------------------------------------------------------- admin: Casting card */

test('admin casting helpers: manual devices and the media base URL', () => {
  const { validDeviceHost, sameDevices, manualDevicesProblem, checkBaseUrl } = admin;
  for (const h of ['192.168.1.20', 'tv.local', 'living-room', 'fe80::1', '[2001:db8::5]', 'Chromecast-1.lan.']) assert.ok(validDeviceHost(h), h);
  for (const h of ['', '256.1.1.1', 'bad host', 'a_b.local', '-x.local', 'http://tv', 'fe80::1%eth0', ':1']) assert.ok(!validDeviceHost(h), h);
  assert.ok(sameDevices([{ kind: 'chromecast', host: 'tv.local', port: 8009 }], [{ kind: 'chromecast', host: 'TV.local' }]), 'default port and case');
  assert.ok(sameDevices([{ kind: 'airplay', host: 'a', port: 7000, name: '' }], [{ kind: 'airplay', host: 'a' }]));
  assert.ok(!sameDevices([{ kind: 'airplay', host: 'a' }], [{ kind: 'chromecast', host: 'a' }]));
  assert.ok(sameDevices(null, []));
  assert.equal(manualDevicesProblem([{ kind: 'chromecast', host: 'tv.local' }, { kind: 'airplay', host: '10.0.0.2', port: 7100, name: 'Den' }]), null);
  assert.match(manualDevicesProblem([{ kind: 'chromecast', host: '' }]), /Device 1: enter a host/);
  assert.match(manualDevicesProblem([{ kind: 'chromecast', host: 'a', port: 70000 }]), /port/);
  assert.match(manualDevicesProblem([{ kind: 'chromecast', host: 'a' }, { kind: 'chromecast', host: 'A', port: 8009 }]), /Device 2: .*twice/);
  assert.match(manualDevicesProblem([{ kind: 'chromecast', host: 'a', name: 'x'.repeat(101) }]), /100 characters/);
  assert.match(manualDevicesProblem(Array.from({ length: 51 }, (_, i) => ({ kind: 'chromecast', host: `h${i}` }))), /At most 50/);
  assert.equal(checkBaseUrl(''), null);
  assert.equal(checkBaseUrl('https://nas.example.com'), null);
  assert.equal(checkBaseUrl('https://nas.example.com:8443/'), null);
  assert.match(checkBaseUrl('https://nas.example.com/ludworks'), /no path/);
  assert.match(checkBaseUrl('https://nas.example.com/?a=1'), /no path/);
  assert.match(checkBaseUrl('ftp://nas'), /http/);
  assert.match(checkBaseUrl('nas.local'), /full http/);
});
