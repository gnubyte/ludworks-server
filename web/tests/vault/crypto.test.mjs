// Vault E2E crypto (VAULT-CRYPTO.md §1-§2): KDF known answers, Seal/Open, vault lifecycle.
import test from 'node:test';
import assert from 'node:assert/strict';
import * as vc from '../../assets/js/lib/vault/crypto.js';
import { fromHex, toHex, utf8Encode, hmac } from '../../assets/js/lib/vault/bytes.js';

const { __testing } = vc;

async function expectCode(promise, code) {
  await assert.rejects(promise, (e) => {
    assert.equal(e.name, 'VaultError');
    assert.equal(e.code, code, `expected ${code}, got ${e.code}: ${e.message}`);
    return true;
  });
}

// Independent HKDF-SHA256 (RFC 5869) built from HMAC, to check the WebCrypto path and the
// "empty salt == 32 zero bytes" rule.
async function hkdfManual(ikm, salt, info, len) {
  const prk = await hmac('SHA-256', salt.length ? salt : new Uint8Array(32), ikm);
  let t = new Uint8Array(0);
  const out = [];
  for (let i = 1; out.length < len; i++) {
    t = await hmac('SHA-256', prk, new Uint8Array([...t, ...info, i]));
    out.push(...t);
  }
  return new Uint8Array(out.slice(0, len));
}

test('base64 is strict standard RFC 4648 with padding', () => {
  assert.equal(vc.b64encode(new Uint8Array([])), '');
  assert.equal(vc.b64encode(utf8Encode('f')), 'Zg==');
  assert.equal(vc.b64encode(utf8Encode('foob')), 'Zm9vYg==');
  assert.equal(vc.b64encode(new Uint8Array([0xfb, 0xff])), '+/8=');
  assert.deepEqual(vc.b64decode('Zm9vYmFy'), utf8Encode('foobar'));
  assert.deepEqual(vc.b64decode('Zg'), utf8Encode('f')); // unpadded accepted
  for (const bad of ['Zg=', 'Z', '-_8=', 'Zm9v YmFy', 'Zm9v\n', 'Zm===', '====']) {
    assert.throws(() => vc.b64decode(bad), (e) => e.code === 'malformed', bad);
  }
  const big = vc.randomBytes(200000);
  assert.deepEqual(vc.b64decode(vc.b64encode(big)), big);
});

test('randomBytes and uuid', () => {
  assert.equal(vc.randomBytes(0).length, 0);
  assert.equal(vc.randomBytes(70000).length, 70000);
  assert.notDeepEqual(vc.randomBytes(16), vc.randomBytes(16));
  const ids = new Set(Array.from({ length: 100 }, () => vc.uuid()));
  assert.equal(ids.size, 100);
  for (const id of ids) assert.match(id, /^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$/);
});

test('PBKDF2-SHA256 known answers (RFC 7914 §11) and NFC normalisation', async () => {
  assert.equal(
    toHex(await vc.deriveMasterKey('passwd', utf8Encode('salt'), 1)),
    '55ac046e56e3089fec1691c22544b605f94185216dde0465e68b9d57c20dacbc',
  );
  assert.equal(
    toHex(await vc.deriveMasterKey('Password', utf8Encode('NaCl'), 80000)),
    '4ddcd8f60b98be21830cee5ef22701f9641a4418d04c0414aeff08876b34ab56',
  );
  // "é" precomposed (U+00E9) and decomposed (e + U+0301) derive the same key.
  const salt = new Uint8Array(16);
  assert.deepEqual(await vc.deriveMasterKey('café', salt, 10), await vc.deriveMasterKey('café', salt, 10));
  await expectCode(vc.deriveMasterKey('x', salt, 0), 'invalid_argument');
  await expectCode(vc.deriveMasterKey('x', salt, 10_000_001), 'invalid_argument');
});

test('HKDF wrap key: RFC 5869 case 3 sanity + spec derivation', async () => {
  // RFC 5869 test case 3: zero-length salt and info.
  const okm = await hkdfManual(new Uint8Array(22).fill(0x0b), new Uint8Array(0), new Uint8Array(0), 42);
  assert.equal(toHex(okm), '8da4e775a563c18f715f802a063c5a31b8a11f5c5ee1879ec3454e5f3c738d2d9d201395faa4b61a96c8');
  const master = fromHex('000102030405060708090a0b0c0d0e0f101112131415161718191a1b1c1d1e1f');
  const want = await hkdfManual(master, new Uint8Array(0), utf8Encode('ludworks/vault/wrap/v1'), 32);
  assert.deepEqual(await __testing.deriveWrapKeyBytes(master), want);
  // deriveWrapKey returns a non-extractable AES-GCM key that seals like the raw bytes.
  const k = await vc.deriveWrapKey(master);
  assert.equal(k.extractable, false);
  assert.equal(k.algorithm.name, 'AES-GCM');
  const nonce = new Uint8Array(12).fill(7);
  assert.equal(await __testing.sealWithNonce(k, utf8Encode('x'), 'a', nonce), await __testing.sealWithNonce(want, utf8Encode('x'), 'a', nonce));
});

test('seal/open: format, round trip, tamper and wrong-AAD failures', async () => {
  const key = vc.randomBytes(32);
  const sealed = await vc.seal(key, utf8Encode('hello vault'), 'aad-1');
  const parts = sealed.split('.');
  assert.equal(parts.length, 3);
  assert.equal(parts[0], 'v1');
  assert.equal(vc.b64decode(parts[1]).length, 12);
  assert.equal(vc.b64decode(parts[2]).length, 'hello vault'.length + 16);
  assert.equal(new TextDecoder().decode(await vc.open(key, sealed, 'aad-1')), 'hello vault');
  // random nonces: same input seals differently
  assert.notEqual(await vc.seal(key, utf8Encode('hello vault'), 'aad-1'), sealed);
  // CryptoKey input works too
  const ck = await vc.importAesKey(key);
  assert.equal(new TextDecoder().decode(await vc.open(ck, sealed, 'aad-1')), 'hello vault');

  await expectCode(vc.open(key, sealed, 'aad-2'), 'decrypt_failed');
  await expectCode(vc.open(vc.randomBytes(32), sealed, 'aad-1'), 'decrypt_failed');
  const ct = vc.b64decode(parts[2]);
  for (const idx of [0, ct.length - 1]) {
    const t = ct.slice();
    t[idx] ^= 1;
    await expectCode(vc.open(key, `v1.${parts[1]}.${vc.b64encode(t)}`, 'aad-1'), 'decrypt_failed');
  }
  const n = vc.b64decode(parts[1]);
  n[0] ^= 1;
  await expectCode(vc.open(key, `v1.${vc.b64encode(n)}.${parts[2]}`, 'aad-1'), 'decrypt_failed');
  await expectCode(vc.open(key, `v2.${parts[1]}.${parts[2]}`, 'aad-1'), 'unsupported_version');
  await expectCode(vc.open(key, `v1.${parts[1]}`, 'aad-1'), 'malformed');
  await expectCode(vc.open(key, `v1.AAAA.${parts[2]}`, 'aad-1'), 'malformed');
  await expectCode(vc.open(key, `v1.${parts[1]}.AAAA`, 'aad-1'), 'malformed');
  await expectCode(vc.open(key, `v1.${parts[1]}.${parts[2]}!`, 'aad-1'), 'malformed');
  await expectCode(vc.seal(new Uint8Array(16), utf8Encode('x'), 'a'), 'invalid_argument');
  await expectCode(__testing.sealWithNonce(key, utf8Encode('x'), 'a', new Uint8Array(8)), 'invalid_argument');
});

test('vault create / unlock / rewrap lifecycle', async () => {
  const { params, vaultKey } = await vc.createVault('correct horse battery staple');
  assert.equal(params.kdf, 'pbkdf2-sha256');
  assert.equal(params.kdf_iterations, 600000);
  assert.equal(vc.b64decode(params.kdf_salt).length, 16);
  assert.match(params.wrapped_key, /^v1\.[A-Za-z0-9+/=]+\.[A-Za-z0-9+/=]+$/);
  assert.equal(vaultKey.length, 32);
  assert.deepEqual(await vc.unlockVault('correct horse battery staple', params), vaultKey);
  await expectCode(vc.unlockVault('wrong password', params), 'bad_password');

  const next = await vc.rewrapVaultKey(vaultKey, 'new master password', { iterations: 650000 });
  assert.equal(next.kdf_iterations, 650000);
  assert.notEqual(next.kdf_salt, params.kdf_salt);
  assert.deepEqual(await vc.unlockVault('new master password', next), vaultKey);
  await expectCode(vc.unlockVault('correct horse battery staple', next), 'bad_password');

  // Weak or bogus parameters are refused before any key is derived.
  await expectCode(vc.createVault('pw', { iterations: 1000 }), 'invalid_argument');
  await expectCode(vc.createVault(''), 'invalid_argument');
  await expectCode(vc.unlockVault('pw', { ...params, kdf_iterations: 1000 }), 'invalid_argument');
  await expectCode(vc.unlockVault('pw', { ...params, kdf_iterations: 20_000_000 }), 'invalid_argument');
  await expectCode(vc.unlockVault('pw', { ...params, kdf: 'argon2id' }), 'unsupported');
  await expectCode(vc.unlockVault('pw', { ...params, kdf_salt: 'AAAA' }), 'malformed');
  await expectCode(vc.rewrapVaultKey(new Uint8Array(31), 'x'), 'invalid_argument');
});

test('items and folders are bound to their ids', async () => {
  const key = vc.randomBytes(32);
  const id1 = vc.uuid();
  const id2 = vc.uuid();
  const item = { type: 'login', name: 'GitHub ✓', login: { username: 'me', password: 'p@ss', uris: [{ uri: 'https://github.com', match: 'domain' }] }, x_future: { kept: true } };
  const data = await vc.encryptItem(key, id1, item);
  assert.deepEqual(await vc.decryptItem(key, id1, data), item);
  await expectCode(vc.decryptItem(key, id2, data), 'decrypt_failed'); // swapped by a malicious server
  const folder = await vc.encryptFolder(key, id1, 'Work/Email');
  assert.equal(await vc.decryptFolder(key, id1, folder), 'Work/Email');
  await expectCode(vc.decryptFolder(key, id2, folder), 'decrypt_failed');
  // an item ciphertext cannot be opened as a folder with the same id (different AAD prefix)
  await expectCode(vc.decryptFolder(key, id1, data), 'decrypt_failed');
  await expectCode(vc.encryptItem(key, 'NOT-A-UUID', item), 'invalid_argument');
  await expectCode(vc.encryptItem(key, id1.toUpperCase(), item), 'invalid_argument');
  await expectCode(vc.encryptItem(key, id1, ['array']), 'invalid_argument');
  // non-JSON plaintext under the right AAD is reported as malformed
  const junk = await vc.seal(key, utf8Encode('not json'), `ludworks/vault/item/v1:${id1}`);
  await expectCode(vc.decryptItem(key, id1, junk), 'malformed');
});
