import { test } from 'node:test';
import assert from 'node:assert/strict';
import { compilePattern, matchPattern, matchRoute, parseQuery, buildQuery, safeNext } from '../../assets/js/core/router.js';

test('static, param, optional and wildcard patterns', () => {
  const root = compilePattern('/');
  assert.deepEqual(matchPattern(root, '/'), {});
  assert.equal(matchPattern(root, '/x'), null);

  const settings = compilePattern('/settings/:tab?');
  assert.deepEqual(matchPattern(settings, '/settings'), { tab: undefined });
  assert.deepEqual(matchPattern(settings, '/settings/'), { tab: undefined });
  assert.deepEqual(matchPattern(settings, '/settings/tokens'), { tab: 'tokens' });
  assert.equal(matchPattern(settings, '/settings/a/b'), null);
  assert.equal(matchPattern(settings, '/settingsx'), null);

  const user = compilePattern('/users/:id/edit');
  assert.deepEqual(matchPattern(user, '/users/abc%20def/edit'), { id: 'abc def' });
  assert.equal(matchPattern(user, '/users/%E0%A4%A/edit'), null, 'malformed escapes never match');

  const files = compilePattern('/files/*rest');
  assert.deepEqual(matchPattern(files, '/files'), { rest: '' });
  assert.deepEqual(matchPattern(files, '/files/'), { rest: '' });
  assert.deepEqual(matchPattern(files, '/files/Movies/Sci%20Fi/'), { rest: 'Movies/Sci Fi' });
  assert.equal(matchPattern(files, '/filesystem'), null);

  const special = compilePattern('/a.b/(x)');
  assert.deepEqual(matchPattern(special, '/a.b/(x)'), {});
  assert.equal(matchPattern(special, '/aXb/(x)'), null, 'regex metacharacters are escaped');
});

test('routes are matched in order with a catch-all', () => {
  const routes = [{ path: '/login' }, { path: '/' }, { path: '/tv/*rest' }, { path: '*' }];
  assert.equal(matchRoute(routes, '/login').route, routes[0]);
  assert.equal(matchRoute(routes, '/').route, routes[1]);
  const tv = matchRoute(routes, '/tv/guide');
  assert.equal(tv.route, routes[2]);
  assert.equal(tv.params.rest, 'guide');
  assert.equal(matchRoute(routes, '/nope').route, routes[3]);
});

test('query parsing and building', () => {
  assert.deepEqual(parseQuery('?a=1&b=two%20words&a=3'), { a: '3', b: 'two words' });
  assert.deepEqual(parseQuery(''), {});
  assert.equal(buildQuery({ q: 'a b', page: 2, empty: '', nil: null, undef: undefined }), '?q=a+b&page=2');
  assert.equal(buildQuery({}), '');
});

test('safeNext only allows same-origin app paths', () => {
  assert.equal(safeNext('/files?x=1#y'), '/files?x=1#y');
  assert.equal(safeNext('/admin/users'), '/admin/users');
  for (const bad of ['//evil.example', '/\\evil.example', 'https://evil.example', 'javascript:alert(1)', 'files', '',
    null, undefined, '/api/v1/me', '/assets/js/app.js', '/login', '/setup', '/x\u0000y', '/x\\y', `/${'a'.repeat(3000)}`]) {
    assert.equal(safeNext(bad), '/', String(bad).slice(0, 30));
  }
  assert.equal(safeNext('//evil', '/home'), '/home');
});
