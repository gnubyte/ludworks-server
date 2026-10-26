// Bitwarden JSON / password-protected JSON / CSV.
import test from 'node:test';
import assert from 'node:assert/strict';
import { parseBitwardenCsv, parseBitwardenJson, parseCsv, toBitwardenCsv, toBitwardenJson, toCsv } from '../../assets/js/lib/vault/bitwarden.js';
import { argon2Sync } from '../../assets/js/lib/vault/argon2.js';
import { b64encode, randomBytes } from '../../assets/js/lib/vault/bytes.js';

const subtle = globalThis.crypto.subtle;
const enc = new TextEncoder();
const code = (c) => (e) => {
  assert.equal(e.name, 'VaultError', e.stack);
  assert.equal(e.code, c, `expected ${c}, got ${e.code}: ${e.message}`);
  return true;
};

const SAMPLE = {
  encrypted: false,
  folders: [{ id: '0f2b7e0a-3c58-4a8e-9d7c-1a2b3c4d5e6f', name: 'Work/Email' }],
  items: [
    {
      passwordHistory: [{ lastUsedDate: '2024-01-01T00:00:00.000Z', password: 'older' }],
      revisionDate: '2025-02-03T04:05:06.000Z',
      creationDate: '2024-01-01T00:00:00.000Z',
      deletedDate: null,
      id: '11111111-2222-4333-8444-555555555555',
      organizationId: null,
      folderId: '0f2b7e0a-3c58-4a8e-9d7c-1a2b3c4d5e6f',
      type: 1,
      reprompt: 0,
      name: 'GitHub, Inc. "main"',
      notes: 'multi\nline, with "quotes"',
      favorite: true,
      fields: [
        { name: 'PIN', value: '1234', type: 1, linkedId: null },
        { name: 'Region', value: 'eu-west', type: 0, linkedId: null },
      ],
      login: {
        fido2Credentials: [],
        uris: [{ match: null, uri: 'https://github.com/login' }, { match: 3, uri: 'https://example.com/a,b' }],
        username: 'octo',
        password: 'p,a"s\'s ✓',
        totp: 'otpauth://totp/GitHub:octo?secret=JBSWY3DPEHPK3PXP&issuer=GitHub',
      },
      collectionIds: null,
    },
    { id: 'a1', type: 2, name: 'Secure note', notes: 'secret note', favorite: false, folderId: null, secureNote: { type: 0 }, fields: [] },
    { id: 'a2', type: 3, name: 'Visa', folderId: null, card: { cardholderName: 'Jane Doe', brand: 'Visa', number: '4111111111111111', expMonth: '12', expYear: '2030', code: '123' } },
  ],
};

// Independent implementation of Bitwarden's password-protected export (PBKDF2 variant) using raw
// WebCrypto, to produce a fixture the library has never seen.
async function independentEncryptedExport(password, clearJson, iterations = 100000) {
  const salt = b64encode(randomBytes(16));
  const base = await subtle.importKey('raw', enc.encode(password), 'PBKDF2', false, ['deriveBits']);
  const master = new Uint8Array(await subtle.deriveBits({ name: 'PBKDF2', hash: 'SHA-256', salt: enc.encode(salt), iterations }, base, 256));
  const prk = await subtle.importKey('raw', master, { name: 'HMAC', hash: 'SHA-256' }, false, ['sign']);
  const encKey = new Uint8Array(await subtle.sign('HMAC', prk, enc.encode('enc\u0001')));
  const macKey = new Uint8Array(await subtle.sign('HMAC', prk, enc.encode('mac\u0001')));
  const encString = async (text) => {
    const iv = randomBytes(16);
    const k = await subtle.importKey('raw', encKey, { name: 'AES-CBC' }, false, ['encrypt']);
    const ct = new Uint8Array(await subtle.encrypt({ name: 'AES-CBC', iv }, k, enc.encode(text)));
    const mk = await subtle.importKey('raw', macKey, { name: 'HMAC', hash: 'SHA-256' }, false, ['sign']);
    const mac = new Uint8Array(await subtle.sign('HMAC', mk, new Uint8Array([...iv, ...ct])));
    return `2.${b64encode(iv)}|${b64encode(ct)}|${b64encode(mac)}`;
  };
  return JSON.stringify({
    encrypted: true, passwordProtected: true, salt, kdfType: 0, kdfIterations: iterations, kdfMemory: null, kdfParallelism: null,
    encKeyValidation_DO_NOT_EDIT: await encString('3b7f6c9e-2f1d-4b8a-9c3e-5d6f7a8b9c0d'),
    data: await encString(clearJson),
  });
}

test('unencrypted JSON round trip', async () => {
  const text = await toBitwardenJson(SAMPLE);
  const back = await parseBitwardenJson(text);
  assert.deepEqual(back, { encrypted: false, folders: SAMPLE.folders, items: SAMPLE.items });
  await assert.rejects(parseBitwardenJson('{"nope":1}'), code('malformed'));
  await assert.rejects(parseBitwardenJson('not json'), code('malformed'));
});

test('password-protected JSON: independently generated fixture (PBKDF2)', async () => {
  const fixture = await independentEncryptedExport('export pässword', JSON.stringify(SAMPLE));
  const back = await parseBitwardenJson(fixture, { password: 'export pässword' });
  assert.deepEqual(back.items, SAMPLE.items);
  assert.deepEqual(back.folders, SAMPLE.folders);
  await assert.rejects(parseBitwardenJson(fixture, { password: 'wrong' }), code('bad_password'));
  await assert.rejects(parseBitwardenJson(fixture), code('password_required'));
  // Tampering with the payload is caught by the MAC.
  const doc = JSON.parse(fixture);
  const [iv, ct, mac] = doc.data.slice(2).split('|');
  const bad = Buffer.from(ct, 'base64');
  bad[0] ^= 1;
  doc.data = `2.${iv}|${bad.toString('base64')}|${mac}`;
  await assert.rejects(parseBitwardenJson(JSON.stringify(doc), { password: 'export pässword' }), code('decrypt_failed'));
  // Account-restricted exports and legacy EncString types are refused.
  await assert.rejects(parseBitwardenJson(JSON.stringify({ encrypted: true, encKeyValidation_DO_NOT_EDIT: 'x', data: 'y', items: [] }), { password: 'x' }), code('unsupported'));
  const t0 = { ...JSON.parse(fixture), encKeyValidation_DO_NOT_EDIT: '0.AAAA|BBBB' };
  await assert.rejects(parseBitwardenJson(JSON.stringify(t0), { password: 'export pässword' }), code('unsupported'));
  const lowKdf = { ...JSON.parse(fixture), kdfIterations: 10 };
  await assert.rejects(parseBitwardenJson(JSON.stringify(lowKdf), { password: 'x' }), code('unsupported'));
});

test('password-protected JSON: Argon2id KDF (salt = SHA-256 of the salt string, memory in MiB)', async () => {
  const text = await toBitwardenJson(SAMPLE, { password: 'pw', kdfType: 1, kdfIterations: 2, kdfMemory: 16, kdfParallelism: 2 });
  const doc = JSON.parse(text);
  assert.equal(doc.kdfType, 1);
  assert.equal(doc.kdfMemory, 16);
  assert.match(doc.data, /^2\.[^|]+\|[^|]+\|[^|]+$/);
  assert.deepEqual((await parseBitwardenJson(text, { password: 'pw' })).items, SAMPLE.items);
  // Independent key check: decrypting the validation string with keys derived from scratch works.
  const saltHash = new Uint8Array(await subtle.digest('SHA-256', enc.encode(doc.salt)));
  const master = argon2Sync({ password: enc.encode('pw'), salt: saltHash, iterations: 2, memory: 16 * 1024, parallelism: 2, type: 'argon2id', hashLength: 32 });
  const prk = await subtle.importKey('raw', master, { name: 'HMAC', hash: 'SHA-256' }, false, ['sign']);
  const encKey = await subtle.importKey('raw', await subtle.sign('HMAC', prk, enc.encode('enc\u0001')), { name: 'AES-CBC' }, false, ['decrypt']);
  const [iv, ct] = doc.encKeyValidation_DO_NOT_EDIT.slice(2).split('|');
  const pt = new TextDecoder().decode(await subtle.decrypt({ name: 'AES-CBC', iv: Buffer.from(iv, 'base64') }, encKey, Buffer.from(ct, 'base64')));
  assert.match(pt, /^[0-9a-f-]{36}$/);
  // PBKDF2 flavour from the library round-trips as well
  const p2 = await toBitwardenJson(SAMPLE, { password: 'pw2', kdfIterations: 5000 });
  assert.deepEqual((await parseBitwardenJson(p2, { password: 'pw2' })).items, SAMPLE.items);
});

test('RFC 4180 CSV parser', () => {
  assert.deepEqual(parseCsv('a,b,c\r\n1,"2,3","say ""hi"""\n"multi\nline",,x'), [
    ['a', 'b', 'c'],
    ['1', '2,3', 'say "hi"'],
    ['multi\nline', '', 'x'],
  ]);
  assert.deepEqual(parseCsv('﻿a\n\nb\n'), [['a'], ['b']]);
  assert.deepEqual(parseCsv('x,\n'), [['x', '']]);
  assert.throws(() => parseCsv('"open'), code('malformed'));
  const rows = [['a,b', 'q"q', ' lead', 'line\r\nbreak', '']];
  assert.deepEqual(parseCsv(toCsv(rows)), rows);
});

test('CSV round trip and Bitwarden importer conventions', () => {
  const csv = toBitwardenCsv(SAMPLE);
  assert.ok(csv.startsWith('folder,favorite,type,name,notes,fields,reprompt,login_uri,login_username,login_password,login_totp\r\n'));
  const back = parseBitwardenCsv(csv);
  assert.deepEqual(back.folders.map((f) => f.name), ['Work/Email']);
  const [login, note, card] = back.items;
  assert.equal(login.folderId, back.folders[0].id);
  assert.equal(login.name, SAMPLE.items[0].name);
  assert.equal(login.notes, SAMPLE.items[0].notes);
  assert.equal(login.favorite, true);
  assert.deepEqual(login.login.uris.map((u) => u.uri), ['https://github.com/login', 'https://example.com/a,b']);
  assert.equal(login.login.password, SAMPLE.items[0].login.password);
  assert.equal(login.login.totp, SAMPLE.items[0].login.totp);
  assert.deepEqual(login.fields.map((f) => [f.name, f.value]), [['PIN', '1234'], ['Region', 'eu-west']]);
  assert.equal(note.type, 2);
  assert.equal(note.notes, 'secret note');
  // cards have no CSV representation: exported as a note with the card data in fields
  assert.equal(card.type, 2);
  assert.deepEqual(card.fields.find((f) => f.name === 'Number').value, '4111111111111111');
  // A CSV written by Bitwarden (header order differs, single URI, empty optional columns)
  const bw = parseBitwardenCsv('name,login_uri,login_username,login_password,folder,type,favorite,notes,fields,reprompt,login_totp\nSite,https://a.example,me,pw,,login,,,"a: 1\nb: c: d",0,\n');
  assert.equal(bw.items[0].login.uris[0].uri, 'https://a.example');
  assert.deepEqual(bw.items[0].fields.map((f) => [f.name, f.value]), [['a', '1'], ['b: c', 'd']]);
  assert.equal(bw.items[0].folderId, null);
  assert.throws(() => parseBitwardenCsv('foo,bar\n1,2'), code('malformed'));
});
