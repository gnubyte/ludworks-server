// Music playback decisions (apps/music/playback.js): stream vs transcode, URLs, the play-report
// rule and the retrying reporter, ReplayGain, volume curve, preload and previous rules.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  mimeCandidates, canPlayOriginal, transcodeFormat, chooseSource, streamUrl, transcodeUrl, downloadUrl,
  playThreshold, listenedDelta, shouldReport, playRecord, isTransient, createPlayReporter, replayGainFactor,
  volumeToGain, clampVolume, previousRestarts, shouldPreload, NORMALIZE_MODES, PLAY_MAX_SECONDS,
} from '../../assets/js/apps/music/playback.js';

/** A fake canPlayType for a browser that supports the given MIME types. */
const browser = (types) => (m) => (types.includes(m) ? 'probably' : '');
const CHROME = browser(['audio/mpeg', 'audio/flac', 'audio/ogg; codecs="vorbis"', 'audio/ogg; codecs="opus"', 'audio/ogg; codecs="flac"',
  'audio/mp4; codecs="mp4a.40.2"', 'audio/mp4; codecs="flac"', 'audio/mp4; codecs="opus"', 'audio/wav; codecs="1"', 'audio/wav', 'audio/aac',
  'audio/webm; codecs="opus"']);
const SAFARI = browser(['audio/mpeg', 'audio/flac', 'audio/mp4; codecs="mp4a.40.2"', 'audio/mp4; codecs="alac"', 'audio/wav; codecs="1"',
  'audio/wav', 'audio/aac', 'audio/aiff', 'audio/x-aiff']);

test('MIME candidates per container and codec', () => {
  assert.deepEqual(mimeCandidates({ container: 'mp3', codec: 'mp3' }), ['audio/mpeg']);
  assert.deepEqual(mimeCandidates({ container: 'flac', codec: 'flac' }), ['audio/flac', 'audio/x-flac']);
  assert.deepEqual(mimeCandidates({ container: 'ogg', codec: 'opus' }), ['audio/ogg; codecs="opus"']);
  assert.deepEqual(mimeCandidates({ container: 'ogg', codec: 'speex' }), []);
  assert.deepEqual(mimeCandidates({ container: 'm4a', codec: 'aac' }), ['audio/mp4; codecs="mp4a.40.2"']);
  assert.deepEqual(mimeCandidates({ container: 'mp4', codec: 'alac' }), ['audio/mp4; codecs="alac"']);
  assert.deepEqual(mimeCandidates({ container: 'wav', codec: 'pcm' }), ['audio/wav; codecs="1"', 'audio/wav']);
  assert.deepEqual(mimeCandidates({ container: 'wav', codec: 'adpcm' }), [], 'ADPCM WAV needs a transcode');
  assert.deepEqual(mimeCandidates({ container: 'aiff', codec: 'pcm' }), ['audio/aiff', 'audio/x-aiff']);
  assert.deepEqual(mimeCandidates({ container: 'wma', codec: 'wmav2', mime: 'audio/x-ms-wma' }), []);
  assert.deepEqual(mimeCandidates({ container: '', codec: '', mime: 'audio/mpeg' }), ['audio/mpeg'], 'not probed yet: trust the MIME type');
  assert.deepEqual(mimeCandidates({ container: '', codec: '', mime: 'application/ogg' }), ['audio/ogg']);
  assert.deepEqual(mimeCandidates({ container: '', mime: 'audio/x-ms-wma' }), []);
  assert.deepEqual(mimeCandidates(null), []);
});

test('stream the original when the browser plays it, else transcode', () => {
  const flac = { container: 'flac', codec: 'flac' };
  const alac = { container: 'm4a', codec: 'alac' };
  const aiff = { container: 'aiff', codec: 'pcm' };
  const wma = { container: 'asf', codec: 'wmav2' };
  assert.equal(canPlayOriginal(flac, CHROME), true);
  assert.equal(canPlayOriginal(alac, CHROME), false);
  assert.equal(canPlayOriginal(alac, SAFARI), true);
  assert.equal(canPlayOriginal(aiff, SAFARI), true);
  assert.equal(canPlayOriginal(flac, null), false);
  assert.equal(canPlayOriginal(flac, () => { throw new Error('boom'); }), false);
  assert.deepEqual(chooseSource(flac, { canPlayType: CHROME }), { kind: 'stream' });
  assert.deepEqual(chooseSource(alac, { canPlayType: CHROME }), { kind: 'transcode', format: 'aac' });
  assert.deepEqual(chooseSource(wma, { canPlayType: browser(['audio/mpeg']) }), { kind: 'transcode', format: 'mp3' });
  assert.deepEqual(chooseSource(wma, { canPlayType: browser(['audio/ogg; codecs="opus"']) }), { kind: 'transcode', format: 'opus' });
  assert.deepEqual(chooseSource(wma, { canPlayType: browser([]) }), { kind: 'none', reason: 'browser' });
  assert.deepEqual(chooseSource(wma, { canPlayType: CHROME, noTranscode: true }), { kind: 'none', reason: 'ffmpeg' });
  assert.deepEqual(chooseSource(flac, { canPlayType: CHROME, forceTranscode: true }), { kind: 'transcode', format: 'aac' });
  assert.equal(transcodeFormat(SAFARI), 'aac');
  assert.equal(transcodeFormat(undefined), null);
});

test('source URLs', () => {
  assert.equal(streamUrl('abcdefgh12'), '/api/v1/music/tracks/abcdefgh12/stream');
  assert.equal(downloadUrl('abcdefgh12'), '/api/v1/music/tracks/abcdefgh12/download?quality=original');
  assert.equal(transcodeUrl('abcdefgh12'), '/api/v1/music/tracks/abcdefgh12/transcode?format=aac');
  assert.equal(transcodeUrl('abcdefgh12', { format: 'mp3', start: 61.27 }), '/api/v1/music/tracks/abcdefgh12/transcode?format=mp3&start=61.2');
  assert.equal(transcodeUrl('abcdefgh12', { bitrate: 999 }), '/api/v1/music/tracks/abcdefgh12/transcode?format=aac&bitrate=320');
  assert.equal(transcodeUrl('abcdefgh12', { bitrate: 10, start: -5 }), '/api/v1/music/tracks/abcdefgh12/transcode?format=aac&bitrate=64');
  assert.ok(!transcodeUrl('a/b?c').includes('a/b?c'), 'ids are encoded');
});

test('play threshold: min(50% of the duration, 240 s)', () => {
  assert.equal(PLAY_MAX_SECONDS, 240);
  assert.equal(playThreshold(180), 90);
  assert.equal(playThreshold(600), 240);
  assert.equal(playThreshold(null), 240);
  assert.equal(playThreshold(0), 240);
  assert.equal(playThreshold(-3), 240);
});

test('listening time ignores seeks', () => {
  assert.equal(listenedDelta(10, 10.25), 0.25);
  assert.equal(listenedDelta(10, 40), 0, 'seek forward');
  assert.equal(listenedDelta(40, 10), 0, 'seek back');
  assert.equal(listenedDelta(NaN, 1), 0);
  assert.equal(listenedDelta(1, 2.5, 2), 1.5);
});

test('when to report a play', () => {
  assert.deepEqual(shouldReport({ listened: 89, duration: 180 }), { report: false, completed: false });
  assert.deepEqual(shouldReport({ listened: 90, duration: 180 }), { report: true, completed: false });
  assert.deepEqual(shouldReport({ listened: 240, duration: 3600 }), { report: true, completed: false });
  assert.deepEqual(shouldReport({ listened: 300, duration: 3600, reported: true }), { report: false, completed: false }, 'once per play');
  assert.deepEqual(shouldReport({ listened: 10, duration: 180, ended: true }), { report: false, completed: false }, 'skipped to the end: not a play');
  assert.deepEqual(shouldReport({ listened: 100, duration: 180, ended: true }), { report: true, completed: true });
  assert.deepEqual(shouldReport({ listened: 45, duration: null, ended: true }), { report: true, completed: true }, 'unknown duration: completed counts');
  assert.deepEqual(shouldReport({ listened: 5, duration: null, ended: true }), { report: false, completed: false });
});

test('play records', () => {
  assert.deepEqual(playRecord({ trackId: 'abcdefgh12', playedAt: '2026-10-27T10:00:00.000Z', seconds: 91.234 }),
    { track_id: 'abcdefgh12', played_at: '2026-10-27T10:00:00.000Z', seconds_played: 91.2 });
  assert.deepEqual(playRecord({ trackId: 'abcdefgh12', playedAt: 'x', seconds: -4, completed: true, context: { kind: 'album', id: 'album0001' } }),
    { track_id: 'abcdefgh12', played_at: 'x', seconds_played: 0, completed: true, context: { kind: 'album', id: 'album0001' } });
  assert.deepEqual(playRecord({ trackId: 't', playedAt: 'x', seconds: 1, context: { kind: 'queue' } }).context, { kind: 'queue' });
  assert.ok(isTransient({ status: 0 }) && isTransient({ status: 503 }) && isTransient({ status: 429 }) && isTransient(new Error('x')));
  assert.ok(!isTransient({ status: 422 }) && !isTransient({ status: 401 }));
});

/** A manual scheduler for the reporter's retry timers. */
function clock() {
  const timers = [];
  return {
    schedule: (fn, ms) => { timers.push({ fn, ms }); return timers.length; },
    cancel: (id) => { if (timers[id - 1]) timers[id - 1].fn = null; },
    async fire() {
      const t = timers.find((x) => x.fn);
      if (!t) return null;
      const fn = t.fn;
      t.fn = null;
      fn();
      await new Promise((r) => setTimeout(r, 0));
      return t.ms;
    },
    get armed() { return timers.filter((x) => x.fn).map((x) => x.ms); },
  };
}

const tick = () => new Promise((r) => setTimeout(r, 0));

test('reporter sends, retries transient errors briefly and drops permanent ones', async () => {
  const sent = [];
  let fail = [{ status: 503 }, { status: 0 }];
  const c = clock();
  const dropped = [];
  const rep = createPlayReporter({
    send: async (plays) => {
      if (fail.length) throw fail.shift();
      sent.push(...plays);
    },
    schedule: c.schedule, cancel: c.cancel, delays: [10, 20, 30],
    onDrop: (plays, err) => dropped.push({ plays, err }),
  });
  rep.add({ track_id: 'a' });
  await tick();
  assert.deepEqual(c.armed, [10], 'first retry armed');
  rep.add({ track_id: 'b' });
  await tick();
  assert.equal(sent.length, 0, 'waits for the retry timer');
  assert.equal(await c.fire(), 10);
  await tick();
  assert.deepEqual(c.armed, [20]);
  assert.equal(await c.fire(), 20);
  await tick();
  assert.deepEqual(sent.map((p) => p.track_id), ['a', 'b']);
  assert.equal(rep.pending.length, 0);

  fail = [{ status: 422 }];
  rep.add({ track_id: 'c' });
  await tick();
  assert.equal(dropped.length, 1, 'validation errors are not retried');
  assert.equal(rep.pending.length, 0);

  fail = [{ status: 500 }, { status: 500 }, { status: 500 }, { status: 500 }];
  rep.add({ track_id: 'd' });
  await tick();
  while (await c.fire() !== null) await tick();
  assert.equal(dropped.length, 2, 'gives up after the last delay');
  assert.equal(rep.pending.length, 0);
  rep.destroy();
  rep.add({ track_id: 'e' });
  assert.equal(rep.pending.length, 0);
});

test('reporter hands over pending plays for a keepalive request', async () => {
  const c = clock();
  const rep = createPlayReporter({ send: async () => { throw { status: 0 }; }, schedule: c.schedule, cancel: c.cancel, delays: [10] });
  rep.add({ track_id: 'a' });
  await tick();
  assert.deepEqual(rep.takePending().map((p) => p.track_id), ['a']);
  assert.equal(rep.pending.length, 0);
  assert.deepEqual(c.armed, [], 'retry cancelled');
});

test('ReplayGain factor', () => {
  assert.deepEqual(NORMALIZE_MODES, ['off', 'track', 'album']);
  const rg = { track_gain_db: -6, track_peak: 0.9, album_gain_db: -8, album_peak: 0.95 };
  assert.ok(Math.abs(replayGainFactor(rg, 'track') - 10 ** (-6 / 20)) < 1e-9);
  assert.ok(Math.abs(replayGainFactor(rg, 'album') - 10 ** (-8 / 20)) < 1e-9);
  assert.equal(replayGainFactor(rg, 'off'), 1);
  assert.equal(replayGainFactor(null, 'track'), 1);
  // Positive gains never clip: limited by 1/peak; an unknown peak (1.0) means no boost.
  assert.equal(replayGainFactor({ track_gain_db: 6, track_peak: 1 }, 'track'), 1);
  assert.ok(Math.abs(replayGainFactor({ track_gain_db: 6, track_peak: 0.5 }, 'track') - 10 ** (6 / 20)) < 1e-9);
  assert.equal(replayGainFactor({ track_gain_db: 12, track_peak: 0.5 }, 'track'), 2);
  // Album mode without an album gain falls back to the track gain.
  assert.ok(Math.abs(replayGainFactor({ track_gain_db: -3, track_peak: 1, album_gain_db: null, album_peak: null }, 'album') - 10 ** (-3 / 20)) < 1e-9);
  assert.ok(Math.abs(replayGainFactor({ track_gain_db: -3, track_peak: 1 }, 'track', { preampDb: 3 }) - 1) < 1e-9);
  assert.equal(replayGainFactor({ track_gain_db: 'x', track_peak: 1 }, 'track'), 1);
});

test('volume, previous and preload rules', () => {
  assert.equal(volumeToGain(0), 0);
  assert.equal(volumeToGain(1), 1);
  assert.equal(volumeToGain(0.5), 0.25);
  assert.equal(volumeToGain(-1), 0);
  assert.equal(volumeToGain(7), 1);
  assert.equal(clampVolume('0.3'), 0.3);
  assert.equal(clampVolume('junk', 0.8), 0.8);
  assert.equal(clampVolume(4), 1);
  assert.equal(previousRestarts(2.9), false);
  assert.equal(previousRestarts(3.5), true);
  assert.equal(shouldPreload({ position: 170, duration: 190, sourceKind: 'stream' }), true);
  assert.equal(shouldPreload({ position: 100, duration: 190, sourceKind: 'stream' }), false);
  assert.equal(shouldPreload({ position: 170, duration: 190, sourceKind: 'transcode' }), false, 'transcodes hold a server slot');
  assert.equal(shouldPreload({ position: 5, duration: null, sourceKind: 'stream' }), false);
});
