// tus client tests against an in-memory tus 1.0.0 server driven through a mocked fetch.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import {
  encodeMetadata, decodeMetadata, toBase64Utf8, fromBase64Utf8, uploadFingerprint, FingerprintStore,
  TusUpload, UploadQueue, fetchTransport, UploadError, TUS_VERSION,
} from '../../assets/js/core/upload.js';

function memStorage() {
  const m = new Map();
  return { getItem: (k) => (m.has(k) ? m.get(k) : null), setItem: (k, v) => m.set(k, String(v)), removeItem: (k) => m.delete(k), m };
}

/**
 * Minimal tus server. Hooks let tests inject failures:
 *  onPatch(ctx) → may return a Response (short-circuit) or throw (network error).
 */
function tusServer(hooks = {}) {
  const uploads = new Map();
  const log = [];
  let n = 0;
  let inflight = 0;
  let maxInflight = 0;
  const fetch = async (url, init = {}) => {
    const method = init.method || 'GET';
    const headers = new Headers(init.headers);
    log.push({ method, url, headers });
    if (method !== 'OPTIONS') assert.equal(headers.get('tus-resumable'), TUS_VERSION, 'Tus-Resumable header on every request');
    if (method === 'POST') {
      const len = Number(headers.get('upload-length'));
      const meta = decodeMetadata(headers.get('upload-metadata'));
      const r = hooks.onCreate?.({ len, meta, headers });
      if (r) return r;
      const id = `up${++n}`;
      uploads.set(id, { len, offset: 0, meta, chunks: [] });
      return new Response(null, {
        status: 201,
        headers: { Location: `/api/v1/uploads/${id}`, 'Upload-Expires': new Date(Date.now() + 3600e3).toUTCString() },
      });
    }
    const id = url.split('/').pop();
    const up = uploads.get(id);
    if (!up) return new Response(null, { status: 404 });
    if (method === 'HEAD') {
      return new Response(null, { status: 200, headers: { 'Upload-Offset': String(up.offset), 'Upload-Length': String(up.len), 'Cache-Control': 'no-store' } });
    }
    if (method === 'DELETE') {
      uploads.delete(id);
      return new Response(null, { status: 204 });
    }
    if (method === 'PATCH') {
      inflight += 1;
      maxInflight = Math.max(maxInflight, inflight);
      try {
        assert.equal(headers.get('content-type'), 'application/offset+octet-stream');
        const off = Number(headers.get('upload-offset'));
        const bytes = new Uint8Array(await new Response(init.body).arrayBuffer());
        const hooked = await hooks.onPatch?.({ id, up, off, bytes, headers, uploads });
        if (hooked) return hooked;
        if (off !== up.offset) return new Response(null, { status: 409 });
        const sum = headers.get('upload-checksum');
        if (sum) {
          const [algo, b64] = sum.split(' ');
          assert.equal(algo, 'sha256');
          if (createHash('sha256').update(bytes).digest('base64') !== b64) return new Response(null, { status: 460 });
        }
        up.chunks.push(bytes);
        up.offset += bytes.length;
        const h = { 'Upload-Offset': String(up.offset) };
        if (up.offset === up.len) {
          h['Ludworks-File-Id'] = `file-${id}`;
          h['Ludworks-File-Path'] = up.meta.filename;
        }
        if (hooks.delay) await new Promise((r) => setTimeout(r, hooks.delay));
        return new Response(null, { status: 204, headers: h });
      } finally {
        inflight -= 1;
      }
    }
    return new Response(null, { status: 405 });
  };
  const data = (id) => Buffer.concat(uploads.get(id).chunks.map((c) => Buffer.from(c)));
  return { fetch, log, uploads, data, get maxInflight() { return maxInflight; } };
}

function makeFile(size, name = 'movie.mkv', lastModified = 1_700_000_000_000) {
  const bytes = new Uint8Array(size);
  for (let i = 0; i < size; i++) bytes[i] = (i * 31 + 7) & 0xff;
  return { file: new File([bytes], name, { lastModified, type: 'video/x-matroska' }), bytes };
}

const noSleep = () => Promise.resolve();

function opts(server, extra = {}) {
  return {
    spaceId: 'space123', path: 'Movies', chunkSize: 1000, checksum: false, retryDelays: [0, 0, 0, 0],
    transport: fetchTransport(server.fetch), store: new FingerprintStore(memStorage()), sleep: noSleep,
    headers: () => ({ 'X-CSRF-Token': 'csrf-abc' }), ...extra,
  };
}

/* -------------------------------------------------------------- encoding */

test('metadata encodes UTF-8 values as base64 and round-trips', () => {
  const header = encodeMetadata({ filename: 'Ünïcødé 🎬.mkv', space_id: 'abc', path: '', overwrite: '0', skip: undefined, nil: null });
  assert.equal(header, `filename ${toBase64Utf8('Ünïcødé 🎬.mkv')},space_id YWJj,path,overwrite MA==`);
  assert.deepEqual(decodeMetadata(header), { filename: 'Ünïcødé 🎬.mkv', space_id: 'abc', path: '', overwrite: '0' });
  assert.equal(fromBase64Utf8(toBase64Utf8('日本語')), '日本語');
  assert.throws(() => encodeMetadata({ 'bad key': 'x' }));
  assert.throws(() => encodeMetadata({ 'a,b': 'x' }));
});

test('fingerprint covers name, size, mtime, space, path and user scope', () => {
  const f = { name: 'a.mp4', size: 10, lastModified: 5 };
  const base = uploadFingerprint(f, { spaceId: 's1', path: 'p' }, 'u1');
  assert.notEqual(base, uploadFingerprint({ ...f, size: 11 }, { spaceId: 's1', path: 'p' }, 'u1'));
  assert.notEqual(base, uploadFingerprint({ ...f, lastModified: 6 }, { spaceId: 's1', path: 'p' }, 'u1'));
  assert.notEqual(base, uploadFingerprint(f, { spaceId: 's2', path: 'p' }, 'u1'));
  assert.notEqual(base, uploadFingerprint(f, { spaceId: 's1', path: 'q' }, 'u1'));
  assert.notEqual(base, uploadFingerprint(f, { spaceId: 's1', path: 'p' }, 'u2'));
  assert.equal(base, uploadFingerprint({ ...f }, { spaceId: 's1', path: 'p' }, 'u1'));
  // Separators inside values can't collide with the field separator.
  assert.notEqual(uploadFingerprint({ name: 'a:b', size: 1 }, { spaceId: 's' }), uploadFingerprint({ name: 'a', size: 1 }, { spaceId: 's', path: 'b' }));
});

test('fingerprint store drops expired entries and caps its size', () => {
  const st = new FingerprintStore(memStorage(), 'k', 3);
  const now = Date.parse('2026-10-26T12:00:00Z');
  st.set('old', { url: '/u/1', createdAt: '2026-10-26T10:00:00Z', expiresAt: '2026-10-26T11:00:00Z' }, now - 7200e3);
  assert.equal(st.get('old', now), null, 'expired');
  for (let i = 0; i < 5; i++) st.set(`k${i}`, { url: `/u/${i}`, createdAt: `2026-10-26T12:00:0${i}Z`, expiresAt: null }, now);
  assert.deepEqual(Object.keys(st.entries()).sort(), ['k2', 'k3', 'k4'], 'oldest evicted first');
  st.remove('k3');
  assert.equal(st.get('k3', now), null);
  st.clear();
  assert.deepEqual(st.entries(), {});
  // Corrupt storage never throws.
  const bad = memStorage();
  bad.setItem('lw.tus.uploads', '{not json');
  assert.equal(new FingerprintStore(bad).get('x'), null);
});

/* ---------------------------------------------------------------- upload */

test('creates, uploads in chunks and returns the committed file id', async () => {
  const server = tusServer();
  const { file, bytes } = makeFile(3500);
  const progress = [];
  const up = new TusUpload(file, opts(server, { category: 'movie', subcategory: 'feature', tags: ['a', 'b'], overwrite: false }));
  up.on('progress', (p) => progress.push(p.bytesUploaded));
  const res = await up.start();
  assert.deepEqual(res, { fileId: 'file-up1', filePath: 'movie.mkv' });
  assert.equal(up.state, 'completed');
  assert.deepEqual(server.data('up1'), Buffer.from(bytes));
  const post = server.log.find((l) => l.method === 'POST');
  assert.equal(post.headers.get('upload-length'), '3500');
  assert.equal(post.headers.get('x-csrf-token'), 'csrf-abc');
  assert.deepEqual(decodeMetadata(post.headers.get('upload-metadata')), {
    filename: 'movie.mkv', space_id: 'space123', path: 'Movies', overwrite: '0',
    mod_time: new Date(1_700_000_000_000).toISOString(), category: 'movie', subcategory: 'feature', tags: 'a,b', mime: 'video/x-matroska',
  });
  const patches = server.log.filter((l) => l.method === 'PATCH').map((l) => l.headers.get('upload-offset'));
  assert.deepEqual(patches, ['0', '1000', '2000', '3000']);
  assert.ok(progress.includes(3500));
  assert.equal(up.store.get(up.fingerprint), null, 'fingerprint forgotten after completion');
  assert.deepEqual(await up.done, res);
});

test('sends a correct sha256 Upload-Checksum per chunk', async () => {
  const server = tusServer();
  const { file, bytes } = makeFile(2500);
  const up = new TusUpload(file, opts(server, { checksum: true }));
  await up.start();
  assert.deepEqual(server.data('up1'), Buffer.from(bytes));
  assert.ok(server.log.filter((l) => l.method === 'PATCH').every((l) => /^sha256 [A-Za-z0-9+/]+=*$/.test(l.headers.get('upload-checksum'))));
});

test('network failure mid-chunk resyncs the offset with HEAD and resumes', async () => {
  let failed = false;
  const server = tusServer({
    onPatch: ({ up, off, bytes }) => {
      if (!failed && off === 1000) {
        failed = true;
        // The server received half of the chunk before the connection dropped.
        up.chunks.push(bytes.slice(0, 400));
        up.offset += 400;
        throw new TypeError('connection reset');
      }
      return null;
    },
  });
  const { file, bytes } = makeFile(3000);
  const states = [];
  const up = new TusUpload(file, opts(server));
  up.on('state', (s) => states.push(s));
  await up.start();
  assert.deepEqual(server.data('up1'), Buffer.from(bytes));
  assert.ok(states.includes('retrying'));
  const seq = server.log.map((l) => `${l.method}:${l.headers.get('upload-offset') ?? ''}`);
  const idx = seq.indexOf('HEAD:');
  assert.ok(idx > 0, 'HEAD issued after the failure');
  assert.equal(seq[idx + 1], 'PATCH:1400', 'continues from the server offset');
});

test('a new instance resumes from the stored fingerprint without re-creating', async () => {
  const server = tusServer();
  const store = new FingerprintStore(memStorage());
  const { file, bytes } = makeFile(4000);
  const first = new TusUpload(file, opts(server, { store, scope: 'user1' }));
  first.on('progress', (p) => { if (p.bytesUploaded >= 2000) first.pause(); });
  assert.equal(await first.start(), null, 'paused before completion');
  assert.equal(first.state, 'paused');
  assert.ok(store.get(first.fingerprint), 'URL remembered');
  const offsetAtPause = server.uploads.get('up1').offset;
  assert.ok(offsetAtPause >= 2000 && offsetAtPause < 4000);

  const second = new TusUpload(file, opts(server, { store, scope: 'user1' }));
  const res = await second.start();
  assert.equal(res.fileId, 'file-up1');
  assert.equal(server.log.filter((l) => l.method === 'POST').length, 1, 'no second creation');
  assert.deepEqual(server.data('up1'), Buffer.from(bytes));

  // A different user scope must not reuse that URL.
  const third = new TusUpload(file, opts(server, { store, scope: 'user2' }));
  assert.notEqual(third.fingerprint, second.fingerprint);
});

test('pause and resume on the same instance continue the upload', async () => {
  const server = tusServer();
  const { file, bytes } = makeFile(5000);
  const up = new TusUpload(file, opts(server));
  let paused = false;
  up.on('progress', (p) => {
    if (!paused && p.bytesUploaded >= 1000) {
      paused = true;
      up.pause();
    }
  });
  await up.start();
  assert.equal(up.state, 'paused');
  const res = await up.resume();
  assert.equal(res.fileId, 'file-up1');
  assert.deepEqual(server.data('up1'), Buffer.from(bytes));
});

test('409 offset mismatch is reconciled via HEAD', async () => {
  let once = true;
  const server = tusServer({
    onPatch: ({ off }) => {
      if (once && off === 2000) {
        once = false;
        return new Response(null, { status: 409 });
      }
      return null;
    },
  });
  const { file, bytes } = makeFile(3000);
  await new TusUpload(file, opts(server)).start();
  assert.deepEqual(server.data('up1'), Buffer.from(bytes));
});

test('checksum mismatch (460) retries the chunk', async () => {
  let once = true;
  const server = tusServer({
    onPatch: ({ off }) => {
      if (once && off === 1000) {
        once = false;
        return new Response(null, { status: 460 });
      }
      return null;
    },
  });
  const { file, bytes } = makeFile(2200);
  await new TusUpload(file, opts(server)).start();
  assert.deepEqual(server.data('up1'), Buffer.from(bytes));
});

test('an upload that expired on the server is re-created from scratch', async () => {
  let expired = false;
  const server = tusServer({
    onPatch: ({ id, off, uploads }) => {
      if (!expired && id === 'up1' && off === 1000) {
        expired = true;
        uploads.delete('up1');
        return new Response(null, { status: 404 });
      }
      return null;
    },
  });
  const { file, bytes } = makeFile(2500);
  const res = await new TusUpload(file, opts(server)).start();
  assert.equal(res.fileId, 'file-up2');
  assert.deepEqual(server.data('up2'), Buffer.from(bytes));
});

test('non-retryable creation errors fail fast with the server message', async () => {
  const server = tusServer({
    onCreate: () => new Response(JSON.stringify({ error: { code: 'quota_exceeded', message: 'Quota exceeded' } }), { status: 507, headers: { 'Content-Type': 'application/json' } }),
  });
  const { file } = makeFile(100);
  const up = new TusUpload(file, opts(server));
  const errors = [];
  up.on('error', (e) => errors.push(e));
  await assert.rejects(up.start(), (err) => err instanceof UploadError && err.code === 'quota_exceeded' && err.message === 'Quota exceeded' && err.status === 507);
  assert.equal(up.state, 'error');
  assert.equal(errors.length, 1);
});

test('persistent network failure gives up after the retry budget', async () => {
  const server = tusServer({ onPatch: () => { throw new TypeError('offline'); } });
  const { file } = makeFile(1500);
  const up = new TusUpload(file, opts(server, { retryDelays: [0, 0] }));
  await assert.rejects(up.start(), (err) => err.code === 'network_error');
  assert.equal(up.state, 'error');
  assert.ok(up.store.get(up.fingerprint), 'resume info kept for a later retry');
});

test('cancel terminates the upload on the server and rejects done', async () => {
  const server = tusServer();
  const { file } = makeFile(5000);
  const up = new TusUpload(file, opts(server));
  up.on('progress', (p) => { if (p.bytesUploaded >= 1000 && up.state === 'uploading') up.cancel(); });
  await up.start();
  await new Promise((r) => setTimeout(r, 10));
  assert.equal(up.state, 'cancelled');
  assert.ok(server.log.some((l) => l.method === 'DELETE'));
  assert.equal(server.uploads.size, 0);
  await assert.rejects(up.done, (e) => e.code === 'cancelled');
  assert.equal(up.store.get(up.fingerprint), null);
});

test('zero-byte files commit with an empty PATCH', async () => {
  const server = tusServer();
  const { file } = makeFile(0, 'empty.txt');
  const res = await new TusUpload(file, opts(server)).start();
  assert.equal(res.fileId, 'file-up1');
});

test('speed and ETA are reported from progress samples', async () => {
  const server = tusServer();
  const { file } = makeFile(4000);
  let t = 0;
  const up = new TusUpload(file, opts(server, { now: () => (t += 500) }));
  const speeds = [];
  up.on('progress', (p) => speeds.push(p.speed));
  await up.start();
  assert.ok(speeds.some((s) => s > 0));
});

test('foreign Location headers are rejected', async () => {
  globalThis.location = new URL('https://nas.local:8443/files');
  try {
    const server = tusServer({ onCreate: () => new Response(null, { status: 201, headers: { Location: 'https://evil.example/api/v1/uploads/x' } }) });
    const { file } = makeFile(10);
    await assert.rejects(new TusUpload(file, opts(server)).start(), (e) => e.code === 'bad_location');
  } finally {
    delete globalThis.location;
  }
});

test('queue runs at most three uploads concurrently and completes all', async () => {
  const server = tusServer({ delay: 5 });
  const store = new FingerprintStore(memStorage());
  const q = new UploadQueue({ ...opts(server, { store }) }, { concurrency: 3 });
  const done = [];
  q.on('complete', (u) => done.push(u.fileId));
  const ups = Array.from({ length: 7 }, (_, i) => q.add(makeFile(2500, `f${i}.bin`).file, { spaceId: 'sp', path: '' }));
  await Promise.all(ups.map((u) => u.done));
  assert.equal(done.length, 7);
  assert.ok(server.maxInflight <= 3, `max in flight ${server.maxInflight}`);
  assert.ok(server.maxInflight >= 2, 'actually concurrent');
  const t = q.totals;
  assert.equal(t.counts.completed, 7);
  assert.equal(t.bytesUploaded, t.bytesTotal);
  q.clearFinished();
  assert.equal(q.uploads.length, 0);
});

test('queue pause/resume keeps state transitions consistent', async () => {
  const server = tusServer({ delay: 2 });
  const q = new UploadQueue(opts(server), { concurrency: 1 });
  const a = q.add(makeFile(3000, 'a.bin').file, { spaceId: 'sp' });
  const b = q.add(makeFile(3000, 'b.bin').file, { spaceId: 'sp' });
  assert.equal(a.state, 'uploading');
  assert.equal(b.state, 'queued');
  q.pause(a);
  assert.equal(a.state, 'paused');
  await b.done; // b got the free slot
  q.resume(a);
  await a.done;
  assert.equal(a.state, 'completed');
});

/* ------------------------------------------------- sign-out / account switch */

/**
 * The tus server above, plus per-session ownership like the real one (loadUpload(id, user_id)):
 * an upload is only visible to the session (CSRF token here) that created it.
 */
function ownedTusServer() {
  const server = tusServer();
  const owner = new Map(); // upload URL → csrf of the creating session
  const posts = [];
  const fetch = async (url, init = {}) => {
    const csrf = new Headers(init.headers).get('x-csrf-token');
    if (init.method === 'POST') posts.push({ csrf, meta: decodeMetadata(new Headers(init.headers).get('upload-metadata')) });
    else if (owner.has(url) && owner.get(url) !== csrf) return new Response(null, { status: 404 });
    const res = await server.fetch(url, init);
    if (init.method === 'POST' && res.status === 201) owner.set(res.headers.get('Location'), csrf);
    return res;
  };
  return { ...server, fetch, posts, log: server.log };
}

const stateReached = (up, state) => new Promise((resolve) => {
  if (up.state === state) resolve();
  else up.on('state', (s) => s === state && resolve());
});

// Regression: signOut() only paused the queue. The tray kept the previous user's uploads (with
// their File handles and target space) on the login screen, and after someone else signed in in
// that tab, Resume replayed them under the new session: PATCH → 404 (not their upload) → the
// client re-created the upload with POST /uploads (old space_id, old file) as the new user.
test('a paused upload is never resumed or re-created under another user\'s session', async () => {
  const server = ownedTusServer();
  let csrf = 'csrf-A';
  const q = new UploadQueue({ ...opts(server, { transport: fetchTransport(server.fetch), headers: () => ({ 'X-CSRF-Token': csrf }) }), scope: 'user-A' }, { concurrency: 1 });
  const up = q.add(makeFile(5000, 'tax-return.pdf').file, { spaceId: 'space-shared-family', path: '' });
  const offPause = up.on('progress', (p) => { if (p.bytesUploaded >= 1000 && up.state === 'uploading') { offPause(); q.pause(up); } });
  await stateReached(up, 'paused');
  await new Promise((r) => setTimeout(r, 10));
  assert.equal(server.posts.length, 1);

  // A signs out (old code: pauseAll, CSRF cleared); B signs in on the same tab and presses Resume.
  csrf = 'csrf-B';
  q.configure({ scope: 'user-B' });
  const sent = server.log.length;
  q.resume(up);
  await assert.rejects(up.start(), (e) => e.code === 'session_changed');
  assert.equal(up.state, 'error');
  assert.equal(server.log.length, sent, 'no request at all under the new session');
  assert.equal(server.posts.filter((p) => p.csrf === 'csrf-B').length, 0, 'never re-created for B');

  // The same account signing back in may continue its own upload.
  csrf = 'csrf-A';
  q.configure({ scope: 'user-A' });
  q.resume(up);
  const res = await up.done;
  assert.equal(res.fileId, 'file-up1');
});

test('reset({terminate}) empties the queue and deletes partial uploads with the old session', async () => {
  const server = ownedTusServer();
  let csrf = 'csrf-A';
  const store = new FingerprintStore(memStorage());
  const q = new UploadQueue({ ...opts(server, { store, transport: fetchTransport(server.fetch), headers: () => ({ 'X-CSRF-Token': csrf }) }), scope: 'user-A' }, { concurrency: 2 });
  const changes = [];
  q.on('change', (u) => changes.push(u));
  const a = q.add(makeFile(5000, 'a.bin').file, { spaceId: 'sp' });
  const b = q.add(makeFile(5000, 'b.bin').file, { spaceId: 'sp' });
  a.on('progress', (p) => { if (p.bytesUploaded >= 1000 && a.state === 'uploading') q.pause(a); });
  await stateReached(a, 'paused');
  await new Promise((r) => setTimeout(r, 10));
  assert.ok(Object.keys(store.entries()).length >= 1, 'resume info stored');

  changes.length = 0;
  const done = q.reset({ terminate: true });
  assert.deepEqual(q.uploads, [], 'emptied synchronously (the tray hides)');
  assert.deepEqual(changes, [null], 'one change event');
  assert.equal(q.busy, false);
  await done;
  const deletes = server.log.filter((l) => l.method === 'DELETE');
  assert.ok(deletes.length >= 1, 'partial uploads terminated');
  assert.ok(deletes.every((l) => l.headers.get('x-csrf-token') === 'csrf-A'), 'with the session that owns them');
  for (const u of [a, b]) {
    if (u.state !== 'completed') {
      assert.equal(u.state, 'cancelled');
      await assert.rejects(u.done, (e) => e.code === 'cancelled');
    }
  }
  assert.deepEqual(store.entries(), {}, 'fingerprints (file names, paths) forgotten');

  // Dropped uploads are detached: nothing a stale button could trigger reaches the server.
  csrf = 'csrf-B';
  q.configure({ scope: 'user-B' });
  const sent = server.log.length;
  q.resume(a);
  assert.equal(await a.start(), null);
  await new Promise((r) => setTimeout(r, 10));
  assert.equal(server.log.length, sent);
  assert.deepEqual(q.uploads, []);
});

test('reset() without terminate stops everything locally and sends nothing', async () => {
  const server = ownedTusServer();
  const q = new UploadQueue({ ...opts(server, { transport: fetchTransport(server.fetch) }), scope: 'user-A' }, { concurrency: 1 });
  const a = q.add(makeFile(5000, 'a.bin').file, { spaceId: 'sp' });
  const b = q.add(makeFile(5000, 'b.bin').file, { spaceId: 'sp' });
  a.on('progress', (p) => { if (p.bytesUploaded >= 1000 && a.state === 'uploading') q.reset(); });
  await assert.rejects(a.done, (e) => e.code === 'cancelled');
  await new Promise((r) => setTimeout(r, 10));
  const sent = server.log.length;
  assert.equal(a.state, 'cancelled');
  assert.equal(b.state, 'cancelled', 'queued uploads never start');
  assert.deepEqual(q.uploads, []);
  assert.equal(server.log.filter((l) => l.method === 'DELETE').length, 0);
  assert.equal(server.posts.length, 1, 'b was never created on the server');
  await new Promise((r) => setTimeout(r, 10));
  assert.equal(server.log.length, sent);
  await q.reset(); // empty queue: no-op
  assert.deepEqual(q.uploads, []);
});
