import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createApi, ApiError, errorFromBody, parseRetryAfter } from '../../assets/js/core/api.js';

function mockFetch(handler) {
  const calls = [];
  const fn = async (url, init) => {
    calls.push({ url, init, headers: new Headers(init.headers) });
    return handler(url, init);
  };
  fn.calls = calls;
  return fn;
}

const json = (status, body, headers = {}) => new Response(JSON.stringify(body), { status, headers: { 'Content-Type': 'application/json', ...headers } });

test('JSON requests carry CSRF only on unsafe methods and resolve URLs/queries', async () => {
  const f = mockFetch(() => json(200, { ok: true }));
  const api = createApi({ fetch: f, getCsrf: () => 'tok123' });
  assert.deepEqual(await api.get('/me', { query: { a: 'x y', skip: '', n: null, list: ['a', 'b'] } }), { ok: true });
  await api.post('/notes', { title: 'hi' });
  await api.patch('me', { display_name: 'x' });
  await api.del('/me/tokens/1');
  await api.get('/api/openapi.json');
  const [get, post, patch, del, abs] = f.calls;
  assert.equal(get.url, '/api/v1/me?a=x+y&list=a%2Cb');
  assert.equal(get.headers.get('x-csrf-token'), null);
  assert.equal(get.init.credentials, 'same-origin');
  assert.equal(post.headers.get('x-csrf-token'), 'tok123');
  assert.equal(post.headers.get('content-type'), 'application/json');
  assert.equal(post.init.body, '{"title":"hi"}');
  assert.equal(patch.url, '/api/v1/me');
  assert.equal(patch.headers.get('x-csrf-token'), 'tok123');
  assert.equal(del.init.method, 'DELETE');
  assert.equal(del.headers.get('x-csrf-token'), 'tok123');
  assert.equal(abs.url, '/api/openapi.json');
});

test('204 resolves to null and raw bodies pass through untouched', async () => {
  const f = mockFetch(() => new Response(null, { status: 204 }));
  const api = createApi({ fetch: f });
  assert.equal(await api.post('/auth/logout', null), null);
  const blob = new Blob(['abc']);
  await api.put('/media/x/poster', blob);
  assert.equal(f.calls[1].init.body, blob);
  assert.equal(f.calls[1].headers.get('content-type'), null);
});

test('errors become ApiError with code, details and fields', async () => {
  const api = createApi({ fetch: mockFetch(() => json(422, { error: { code: 'validation_failed', message: 'Invalid', details: { fields: { email: 'bad' } } } })) });
  await assert.rejects(api.post('/x', {}), (e) => e instanceof ApiError && e.status === 422 && e.code === 'validation_failed' && e.fields.email === 'bad');
  const api2 = createApi({ fetch: mockFetch(() => new Response('oops', { status: 500 })) });
  await assert.rejects(api2.get('/x'), (e) => e.status === 500 && e.code === 'internal' && /500/.test(e.message));
  const api3 = createApi({ fetch: mockFetch(() => json(429, { error: { code: 'rate_limited', message: 'Slow down' } }, { 'Retry-After': '7' })) });
  await assert.rejects(api3.get('/x'), (e) => e.retryAfter === 7);
});

test('401 unauthorized triggers the redirect hook unless suppressed; login errors never do', async () => {
  let hits = 0;
  const onUnauthorized = () => { hits += 1; };
  const unauth = createApi({ fetch: mockFetch(() => json(401, { error: { code: 'unauthorized', message: 'no' } })), onUnauthorized });
  await assert.rejects(unauth.get('/me'));
  assert.equal(hits, 1);
  await assert.rejects(unauth.get('/me', { noRedirect: true }));
  assert.equal(hits, 1);
  const badCreds = createApi({ fetch: mockFetch(() => json(401, { error: { code: 'invalid_credentials', message: 'no' } })), onUnauthorized });
  await assert.rejects(badCreds.post('/auth/login', {}), (e) => e.code === 'invalid_credentials');
  const totp = createApi({ fetch: mockFetch(() => json(401, { error: { code: 'totp_required', message: 'code' } })), onUnauthorized });
  await assert.rejects(totp.post('/auth/login', {}), (e) => e.code === 'totp_required');
  assert.equal(hits, 1);
});

test('503 setup_required triggers the setup hook', async () => {
  let setup = 0;
  const api = createApi({ fetch: mockFetch(() => json(503, { error: { code: 'setup_required', message: 'setup' } })), onSetupRequired: () => { setup += 1; } });
  await assert.rejects(api.get('/me'));
  assert.equal(setup, 1);
});

test('network failures and aborts', async () => {
  const api = createApi({ fetch: mockFetch(() => { throw new TypeError('Failed to fetch'); }) });
  await assert.rejects(api.get('/x'), (e) => e instanceof ApiError && e.code === 'network_error' && e.status === 0);
  const ctrl = new AbortController();
  const slow = createApi({
    fetch: (url, init) => new Promise((_, reject) => init.signal.addEventListener('abort', () => reject(new DOMException('Aborted', 'AbortError')))),
  });
  const p = slow.get('/x', { signal: ctrl.signal });
  ctrl.abort();
  await assert.rejects(p, (e) => e.name === 'AbortError');
  const timed = createApi({
    fetch: (url, init) => new Promise((_, reject) => init.signal.addEventListener('abort', () => reject(init.signal.reason))),
  });
  await assert.rejects(timed.get('/x', { timeout: 10 }), (e) => e.code === 'timeout');
});

test('helpers', () => {
  assert.equal(parseRetryAfter('12'), 12);
  assert.equal(parseRetryAfter(new Date(Date.now() + 5000).toUTCString()) <= 5, true);
  assert.equal(parseRetryAfter(null), null);
  const e = errorFromBody(404, null);
  assert.equal(e.code, 'not_found');
  assert.equal(e.fields, null);
});
