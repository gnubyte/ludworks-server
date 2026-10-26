// TOTP: RFC 6238 Appendix B vectors, otpauth parsing, base32.
import test from 'node:test';
import assert from 'node:assert/strict';
import { base32decode, base32encode, buildOtpauth, computeTotp, generateTotp, parseOtpauth } from '../../assets/js/lib/vault/totp.js';
import { utf8Encode } from '../../assets/js/lib/vault/bytes.js';

const SEEDS = {
  SHA1: '12345678901234567890',
  SHA256: '12345678901234567890123456789012',
  SHA512: '1234567890123456789012345678901234567890123456789012345678901234',
};
const RFC6238 = [
  [59, '94287082', '46119246', '90693936'],
  [1111111109, '07081804', '68084774', '25091201'],
  [1111111111, '14050471', '67062674', '99943326'],
  [1234567890, '89005924', '91819424', '93441116'],
  [2000000000, '69279037', '90698825', '38618901'],
  [20000000000, '65353130', '77737706', '47863826'],
];

test('RFC 6238 Appendix B vectors (SHA1/SHA256/SHA512, 8 digits)', async () => {
  for (const [t, c1, c256, c512] of RFC6238) {
    for (const [algo, want] of [['SHA1', c1], ['SHA256', c256], ['SHA512', c512]]) {
      const r = await computeTotp(utf8Encode(SEEDS[algo]), { algorithm: algo, digits: 8, period: 30 }, t * 1000);
      assert.equal(r.code, want, `${algo} @ ${t}`);
    }
    // same through an otpauth URI
    const uri = buildOtpauth({ secret: base32encode(utf8Encode(SEEDS.SHA256)), algorithm: 'SHA256', digits: 8, issuer: 'RFC', account: 'b' });
    assert.equal((await generateTotp(uri, t * 1000)).code, c256);
  }
});

test('generateTotp defaults, remaining seconds and bare secrets', async () => {
  const secret = 'JBSWY3DPEHPK3PXP';
  const r = await generateTotp(secret, 1_700_000_005_000);
  assert.equal(r.code.length, 6);
  assert.equal(r.period, 30);
  assert.equal(r.remaining, 30 - (1_700_000_005 % 30));
  // case, spaces and padding are ignored
  assert.equal((await generateTotp('jbsw y3dp ehpk 3pxp===', 1_700_000_005_000)).code, r.code);
  const uri = await generateTotp(`otpauth://totp/Example:alice@example.com?secret=${secret}&issuer=Example`, 1_700_000_005_000);
  assert.equal(uri.code, r.code);
});

test('parseOtpauth extracts parameters and rejects bad input', () => {
  const p = parseOtpauth('otpauth://totp/ACME%20Co:john.doe%40email.com?secret=HXDMVJECJJWSRB3HWIZR4IFUGFTMXBOZ&issuer=ACME%20Co&algorithm=SHA256&digits=8&period=60');
  assert.deepEqual(p, {
    type: 'totp', secret: 'HXDMVJECJJWSRB3HWIZR4IFUGFTMXBOZ', algorithm: 'SHA256', digits: 8, period: 60,
    issuer: 'ACME Co', account: 'john.doe@email.com', encoder: 'rfc6238',
  });
  const q = parseOtpauth('otpauth://totp/alice?secret=jbswy3dpehpk3pxp');
  assert.equal(q.issuer, '');
  assert.equal(q.account, 'alice');
  assert.equal(q.secret, 'JBSWY3DPEHPK3PXP');
  assert.equal(parseOtpauth('steam://JBSWY3DPEHPK3PXP').encoder, 'steam');
  assert.throws(() => parseOtpauth('otpauth://hotp/x?secret=JBSWY3DP&counter=1'), (e) => e.code === 'unsupported');
  assert.throws(() => parseOtpauth('otpauth://totp/x?secret=JBSWY3DP&digits=12'), (e) => e.code === 'unsupported');
  assert.throws(() => parseOtpauth('otpauth://totp/x?secret=JBSWY3DP&algorithm=MD5'), (e) => e.code === 'unsupported');
  assert.throws(() => parseOtpauth('otpauth://totp/x?secret=not*base32'), (e) => e.code === 'malformed');
  assert.throws(() => parseOtpauth('otpauth://totp/x?secret=JBSWY3DP&period=0'), (e) => e.code === 'malformed');
  assert.throws(() => parseOtpauth('https://example.com'), (e) => e.code === 'malformed');
});

test('base32 round trip and steam codes', async () => {
  for (let n = 0; n < 40; n++) {
    const b = Uint8Array.from({ length: n }, (_, i) => (i * 37 + n) & 255);
    if (n) assert.deepEqual(base32decode(base32encode(b)), b);
  }
  assert.equal(base32encode(utf8Encode('foobar')), 'MZXW6YTBOI');
  assert.deepEqual(base32decode('MZXW6YTBOI======'), utf8Encode('foobar'));
  assert.throws(() => base32decode(''), (e) => e.code === 'malformed');
  assert.throws(() => base32decode('ABC1'), (e) => e.code === 'malformed');
  await assert.rejects(generateTotp('A'), (e) => e.code === 'malformed'); // decodes to zero bytes
  const s = await generateTotp('steam://JBSWY3DPEHPK3PXP', 1_700_000_000_000);
  assert.match(s.code, /^[23456789BCDFGHJKMNPQRTVWXY]{5}$/);
});
