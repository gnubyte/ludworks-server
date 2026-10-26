// URI matching (VAULT-CRYPTO.md §3).
import test from 'node:test';
import assert from 'node:assert/strict';
import { matchUri, publicSuffix, registrableDomain } from '../../assets/js/lib/vault/url-match.js';

// Regression: the registrable domain was "last two labels (three for a few ccTLD second levels)", so a
// login saved for one tenant of a shared hosting / dynamic-DNS suffix matched every other tenant
// (victim.github.io → attacker.github.io, mynas.duckdns.org → attacker.duckdns.org, ...). It is now
// computed with the Public Suffix List including its private section.
test('shared-suffix tenants are different sites (PSL private section)', () => {
  const pairs = [
    ['https://victim-project.github.io/admin/login', 'https://attacker.github.io/phish'],
    ['https://mynas.duckdns.org', 'https://attacker.duckdns.org/'],
    ['https://home.synology.me:5001', 'https://evil.synology.me/'],
    ['https://nas.tail1234.ts.net', 'https://evil.tail9999.ts.net/'],
    ['https://app.herokuapp.com', 'https://evil.herokuapp.com/'],
    ['https://site.azurewebsites.net', 'https://evil.azurewebsites.net/'],
    ['https://site.pages.dev', 'https://evil.pages.dev/'],
    ['https://site.vercel.app', 'https://evil.vercel.app/'],
    ['https://site.netlify.app', 'https://evil.netlify.app/'],
    ['https://bucket.s3.amazonaws.com', 'https://evil.s3.amazonaws.com/'],
    ['https://shop.myshopify.com', 'https://phish.myshopify.com/'],
    ['https://nas.ddns.net', 'https://evil.ddns.net/'],
    ['https://nas.myqnapcloud.com', 'https://evil.myqnapcloud.com/'],
    ['https://alice.me.uk', 'https://mallory.me.uk/'],
    ['https://shop1.ne.jp', 'https://shop2.ne.jp/'],
    ['https://tienda.gob.mx', 'https://otra.gob.mx/'],
  ];
  for (const [item, page] of pairs) {
    assert.equal(matchUri(item, page), false, `${item} must not match ${page}`);
    assert.equal(matchUri(item, page, 'domain'), false, `${item} must not match ${page}`);
  }
  // Same tenant still matches across its subdomains.
  assert.ok(matchUri('https://a.tail1234.ts.net', 'https://b.tail1234.ts.net/'));
  assert.ok(matchUri('https://victim.github.io/x', 'https://victim.github.io/y'));
  assert.ok(matchUri('https://login.example.co.uk', 'https://www.example.co.uk/'));
  // A saved URI that is itself a public suffix only matches that exact host.
  assert.ok(!matchUri('https://github.io', 'https://attacker.github.io/'));
  assert.ok(matchUri('https://github.io', 'https://github.io/'));
  assert.equal(publicSuffix('a.b.github.io'), 'github.io');
  assert.equal(publicSuffix('www.bbc.co.uk'), 'co.uk');
  assert.equal(publicSuffix('x.foo.ck'), 'foo.ck');
  assert.equal(publicSuffix('www.ck'), 'ck');
  assert.equal(publicSuffix('127.0.0.1'), '');
});

test('starts_with never reaches into another origin', () => {
  assert.ok(!matchUri('https://bank.com', 'https://bank.com.evil.net/', 'starts_with'));
  assert.ok(!matchUri('https://bank.com', 'https://bank.com@evil.net/', 'starts_with'));
  assert.ok(!matchUri('https://bank.com', 'https://bank.com:8443/', 'starts_with'));
  assert.ok(matchUri('https://bank.com', 'https://bank.com/login', 'starts_with'));
  assert.ok(matchUri('https://bank.com/', 'https://bank.com/', 'starts_with'));
});

test('registrableDomain', () => {
  const cases = {
    'github.com': 'github.com',
    'www.github.com': 'github.com',
    'a.b.c.example.org': 'example.org',
    'www.bbc.co.uk': 'bbc.co.uk',
    'shop.example.com.au': 'example.com.au',
    'x.ac.jp': 'x.ac.jp',
    'foo.co.com': 'foo.co.com', // co.com is a (private) public suffix in the PSL
    'localhost': 'localhost',
    '192.168.1.10': '192.168.1.10',
    '[::1]': '[::1]',
    'Example.COM.': 'example.com',
    'intranet': 'intranet',
    'login.example.com:8443': 'example.com',
  };
  for (const [host, want] of Object.entries(cases)) assert.equal(registrableDomain(host), want, host);
});

test('domain mode (default)', () => {
  assert.ok(matchUri('https://github.com/login', 'https://gist.github.com/x'));
  assert.ok(matchUri('github.com', 'https://github.com/'));
  assert.ok(matchUri('https://accounts.bbc.co.uk', 'https://www.bbc.co.uk/home', 'domain'));
  assert.ok(!matchUri('https://bbc.co.uk', 'https://evil.co.uk/'));
  assert.ok(!matchUri('https://github.com', 'https://github.com.evil.io/'));
  assert.ok(matchUri('http://example.com', 'https://example.com/')); // upgrade is fine
  assert.ok(!matchUri('https://example.com', 'http://example.com/')); // downgrade is not
  assert.ok(!matchUri('https://example.com', 'file:///etc/passwd'));
  assert.ok(!matchUri('https://example.com', 'javascript:alert(1)'));
  assert.ok(matchUri('http://192.168.1.5:8080', 'http://192.168.1.5:8080/admin'));
  assert.ok(!matchUri('http://192.168.1.5:8080', 'http://192.168.1.5:9090/'));
  assert.ok(!matchUri('http://localhost:3000', 'http://localhost:4000/'));
  assert.ok(matchUri('http://localhost:3000/x', 'http://localhost:3000/y', null));
  assert.ok(!matchUri('', 'https://example.com'));
  assert.ok(!matchUri('not a url at all %%%', 'https://example.com'));
});

test('host, starts_with, exact, regex, never and unknown modes', () => {
  assert.ok(matchUri('https://app.example.com', 'https://app.example.com/a', 'host'));
  assert.ok(!matchUri('https://app.example.com', 'https://www.example.com/a', 'host'));
  assert.ok(!matchUri('https://app.example.com:8443', 'https://app.example.com/a', 'host'));
  assert.ok(matchUri('https://app.example.com:443', 'https://app.example.com/a', 'host'));
  assert.ok(matchUri('https://example.com/account', 'https://example.com/account/settings', 'starts_with'));
  assert.ok(!matchUri('https://example.com/account', 'https://example.com/other', 'starts_with'));
  assert.ok(matchUri('https://example.com/a?b=1', 'https://example.com/a?b=1', 'exact'));
  assert.ok(!matchUri('https://example.com/a', 'https://example.com/a/', 'exact'));
  assert.ok(matchUri('^https://(www\\.)?example\\.com/', 'https://WWW.example.com/x', 'regex'));
  assert.ok(!matchUri('([', 'https://example.com', 'regex'));
  assert.ok(!matchUri('a'.repeat(2000), 'https://example.com', 'regex'));
  // catastrophic-backtracking shapes are refused instead of hanging the caller
  const t0 = Date.now();
  for (const evil of ['^(a+)+$', '(\\w*)*x', '(x+x+)+y', '(a|a+){2,}b', '(\\d+){1,}']) assert.ok(!matchUri(evil, `https://${'a'.repeat(40)}!`, 'regex'), evil);
  assert.ok(Date.now() - t0 < 1000);
  assert.ok(matchUri('^https://([a-z]+\\.)?example\\.com/', 'https://shop.example.com/', 'regex'));
  assert.ok(!matchUri('^https://([a-z]+\\.)?example\\.com/', 'https://shopXexampleYcom/', 'regex'));
  assert.ok(!matchUri('https://example.com', 'https://example.com', 'never'));
  assert.ok(!matchUri('https://example.com', 'https://example.com', 'fuzzy'));
});
