// KDBX 4 write/read round trips, KDBX 3.1 read, key files, error classification, plain XML.
import test from 'node:test';
import assert from 'node:assert/strict';
import { compositeKey, generateKeyFile, keyFileKey, readKdbx, requiresKdbx41, writeKdbx } from '../../assets/js/lib/vault/kdbx.js';
import { newEntry, newGroup, newKdbxUuid, newTimes, newTree, parseKeePassXml, serializeKeePassXml } from '../../assets/js/lib/vault/keepass-xml.js';
import { el, parseXml, serializeXml } from '../../assets/js/lib/vault/xml.js';
import { b64encode, concatBytes, fromHex, randomBytes, sha256, toHex, utf8Encode } from '../../assets/js/lib/vault/bytes.js';
import { writeKdbx3 } from './helpers/kdbx3-writer.mjs';

const FAST_ARGON = { type: 'argon2d', memory: 1024 * 1024, iterations: 2, parallelism: 2 };
const code = (c) => (e) => {
  assert.equal(e.name, 'VaultError', e.stack);
  assert.equal(e.code, c, `expected ${c}, got ${e.code}: ${e.message}`);
  return true;
};

function field(key, value, prot = false) {
  return { key, value, protected: prot };
}

function sampleTree() {
  const tree = newTree('Test DB ✓');
  tree.meta.databaseDescription = 'Round-trip fixture';
  tree.meta.defaultUserName = 'octo';
  tree.meta.customData = [{ key: 'plugin.setting', value: '42' }];
  tree.binaries = [
    { data: utf8Encode('attachment one'), protected: false },
    { data: Uint8Array.from({ length: 5000 }, (_, i) => (i * 7) & 255), protected: true },
  ];
  const work = newGroup('Work');
  const email = newGroup('Email · 邮件');
  email.notes = 'nested group';
  work.groups.push(email);
  const recycle = newGroup('Recycle Bin', 43);
  tree.root.groups.push(work, recycle);
  tree.meta.recycleBinUuid = recycle.uuid;

  const e = newEntry();
  e.times = newTimes('2020-01-02T03:04:05Z', '2021-06-07T08:09:10Z');
  e.fields = [
    field('Title', 'GitHub'),
    field('UserName', 'octo@example.com'),
    field('Password', 'p@ss «ü» 🔑 </Value>', true),
    field('URL', 'https://github.com/login'),
    field('Notes', 'line1\r\nline2\n\ttab <xml> & "quotes"'),
    field('API Key', 'sk_live_0123456789', true),
    field('otp', 'otpauth://totp/GitHub:octo?secret=JBSWY3DPEHPK3PXP&issuer=GitHub', true),
    field('Empty protected', '', true),
    field('Plain field', 'visible'),
  ];
  e.tags = ['dev', 'Favorite'];
  e.binaries = [{ key: 'notes.txt', ref: 0 }, { key: 'blob.bin', ref: 1 }];
  e.customData = [{ key: 'Ludworks.Type', value: 'login' }];
  e.autoType = el('AutoType', null, el('Enabled', null, 'True'), el('DataTransferObfuscation', null, '0'), el('DefaultSequence', null, '{USERNAME}{TAB}{PASSWORD}{ENTER}'), el('Association', null, el('Window', null, 'GitHub*'), el('KeystrokeSequence', null, '')));
  e.extra = [el('FutureElement', { attr: 'x' }, el('LastModificationTime', null, '2022-01-01T00:00:00Z'), el('Data', null, 'kept'))];
  for (const [pw, when] of [['old-1', '2020-01-02T03:04:05Z'], ['old-2', '2020-06-01T00:00:00Z']]) {
    const h = newEntry();
    h.uuid = e.uuid;
    h.times = newTimes('2020-01-02T03:04:05Z', when);
    h.fields = e.fields.map((f) => (f.key === 'Password' ? { ...f, value: pw } : { ...f }));
    e.history.push(h);
  }
  email.entries.push(e);

  const root = newEntry();
  root.times = newTimes('2019-12-31T23:59:59Z', '2019-12-31T23:59:59Z');
  root.fields = [field('Title', 'Root entry'), field('UserName', ''), field('Password', 'x', true), field('URL', ''), field('Notes', '')];
  tree.root.entries.push(root);

  const binned = newEntry();
  binned.fields = [field('Title', 'Deleted thing'), field('Password', 'gone', true)];
  binned.times = newTimes('2018-01-01T00:00:00Z');
  recycle.entries.push(binned);
  tree.deletedObjects = [{ uuid: newKdbxUuid(), deletionTime: '2022-02-02T02:02:02Z' }];
  return tree;
}

// The writer emits KeePass's default <AutoType> for entries that had none; treat both alike.
const DEFAULT_AUTOTYPE = serializeXml(el('AutoType', null, el('Enabled', null, 'True'), el('DataTransferObfuscation', null, '0')), { declaration: false });
function autoTypeOf(e) {
  const s = e.autoType ? serializeXml(e.autoType, { declaration: false }) : null;
  return s === DEFAULT_AUTOTYPE ? null : s;
}

/** Comparable projection of a tree (drops fields that legitimately differ, e.g. generator). */
function project(tree, { entryCustomData = true } = {}) {
  const entry = (e) => ({
    uuid: e.uuid,
    fields: e.fields.map((f) => [f.key, f.value, f.protected]),
    tags: e.tags,
    times: e.times,
    binaries: e.binaries.map((b) => ({ key: b.key, data: toHex(tree.binaries[b.ref].data), protected: tree.binaries[b.ref].protected })),
    customData: entryCustomData ? e.customData : [],
    autoType: autoTypeOf(e),
    extra: e.extra.map((x) => serializeXml(x, { declaration: false })),
    history: (e.history || []).map(entry),
  });
  const group = (g) => ({ uuid: g.uuid, name: g.name, notes: g.notes, iconId: g.iconId, times: g.times, entries: g.entries.map(entry), groups: g.groups.map(group) });
  const { databaseName, databaseDescription, defaultUserName, recycleBinUuid, memoryProtection, customData } = tree.meta;
  return { meta: { databaseName, databaseDescription, defaultUserName, recycleBinUuid, memoryProtection, customData }, root: group(tree.root), deletedObjects: tree.deletedObjects };
}

function headerEnd(bytes) {
  const dv = new DataView(bytes.buffer, bytes.byteOffset, bytes.byteLength);
  let pos = 12;
  for (;;) {
    const id = bytes[pos];
    const size = dv.getUint32(pos + 1, true);
    pos += 5 + size;
    if (id === 0) return pos;
  }
}

test('KDBX4 round trip: AES-256-CBC + Argon2d, password, protected fields, nested groups, history, unicode, attachments', async () => {
  const tree = sampleTree();
  const bytes = await writeKdbx(tree, { password: 'pässwörd ✓', kdf: FAST_ARGON });
  assert.deepEqual([...bytes.subarray(0, 12)], [0x03, 0xd9, 0xa2, 0x9a, 0x67, 0xfb, 0x4b, 0xb5, 0x00, 0x00, 0x04, 0x00]);
  const back = await readKdbx(bytes, { password: 'pässwörd ✓' });
  assert.deepEqual(back.version, { major: 4, minor: 0 });
  assert.deepEqual(back.header, { cipher: 'aes', compression: true, innerStream: 'chacha20', kdf: { type: 'argon2d', memory: 1048576, iterations: 2, parallelism: 2, version: 0x13 } });
  assert.deepEqual(project(back), project(tree));
  assert.equal(back.meta.generator, 'Ludworks');
  // writing what we read reproduces the same content
  const again = await readKdbx(await writeKdbx(back, { password: 'x', kdf: FAST_ARGON }), { password: 'x' });
  assert.deepEqual(project(again), project(tree));
});

test('KDBX4 round trip: ChaCha20 + Argon2id with password + key file', async () => {
  const tree = sampleTree();
  const keyFile = await generateKeyFile();
  const bytes = await writeKdbx(tree, { password: 'pw', keyFile, cipher: 'chacha20', kdf: { ...FAST_ARGON, type: 'argon2id' } });
  const back = await readKdbx(bytes, { password: 'pw', keyFile });
  assert.equal(back.header.cipher, 'chacha20');
  assert.equal(back.header.kdf.type, 'argon2id');
  assert.deepEqual(project(back), project(tree));
  await assert.rejects(readKdbx(bytes, { password: 'pw' }), code('bad_credentials'));
  await assert.rejects(readKdbx(bytes, { keyFile }), code('bad_credentials'));
  await assert.rejects(readKdbx(bytes, { password: 'pw', keyFile: await generateKeyFile() }), code('bad_credentials'));
});

test('KDBX4 round trip: AES-KDF, key file only, no compression, empty database', async () => {
  const keyFile = randomBytes(32);
  const tree = sampleTree();
  const bytes = await writeKdbx(tree, { keyFile, kdf: { type: 'aes', rounds: 100000 }, compression: false });
  const back = await readKdbx(bytes, { keyFile });
  assert.deepEqual(back.header.kdf, { type: 'aes', rounds: 100000 });
  assert.equal(back.header.compression, false);
  assert.deepEqual(project(back), project(tree));
  const empty = await readKdbx(await writeKdbx(newTree('Empty'), { password: '', kdf: FAST_ARGON }), { password: '' });
  assert.equal(empty.root.name, 'Empty');
  assert.equal(empty.root.entries.length, 0);
});

test('KDBX4 export defaults: AES-256-CBC, Argon2d 64 MiB / 2 / 2, gzip, ChaCha20 inner stream', async () => {
  const t0 = performance.now();
  const bytes = await writeKdbx(sampleTree(), { password: 'defaults' });
  const back = await readKdbx(bytes, { password: 'defaults' });
  console.log(`# default KDBX write+read: ${(performance.now() - t0).toFixed(0)} ms`);
  assert.deepEqual(back.header, { cipher: 'aes', compression: true, innerStream: 'chacha20', kdf: { type: 'argon2d', memory: 64 * 1024 * 1024, iterations: 2, parallelism: 2, version: 0x13 } });
  await assert.rejects(readKdbx(bytes, { password: 'defaults' , limits: { maxArgon2MemoryBytes: 32 * 1024 * 1024 } }), code('limit_exceeded'));
});

test('KDBX4 corruption, wrong credentials and unsupported files are classified', async () => {
  const bytes = await writeKdbx(sampleTree(), { password: 'pw', kdf: FAST_ARGON });
  await assert.rejects(readKdbx(bytes, { password: 'PW' }), code('bad_credentials'));
  const he = headerEnd(bytes);
  const flipped = (at) => {
    const b = bytes.slice();
    b[at] ^= 0x01;
    return b;
  };
  await assert.rejects(readKdbx(flipped(20), { password: 'pw' }), code('corrupt')); // header byte → checksum
  await assert.rejects(readKdbx(flipped(he + 64 + 36 + 10), { password: 'pw' }), code('corrupt')); // payload → block HMAC
  await assert.rejects(readKdbx(flipped(he + 64), { password: 'pw' }), code('corrupt')); // block MAC itself
  await assert.rejects(readKdbx(bytes.subarray(0, bytes.length - 10), { password: 'pw' }), code('corrupt'));
  await assert.rejects(readKdbx(bytes.subarray(0, he + 20), { password: 'pw' }), code('corrupt'));
  await assert.rejects(readKdbx(utf8Encode('definitely not a database'), { password: 'pw' }), code('corrupt'));
  await assert.rejects(readKdbx(new Uint8Array(4), { password: 'pw' }), code('corrupt'));
  const kdb = bytes.slice(0, 64);
  kdb[4] = 0x65; // KeePass 1.x signature 0xB54BFB65
  await assert.rejects(readKdbx(kdb, { password: 'pw' }), code('unsupported'));
  const v5 = bytes.slice();
  v5[10] = 5;
  await assert.rejects(readKdbx(v5, { password: 'pw' }), code('unsupported'));
  // Twofish: swap the cipher UUID and fix up the header checksum so only the cipher is "wrong".
  const tf = bytes.slice();
  const aes = fromHex('31c1f2e6bf714350be5805216afc5aff');
  const at = tf.findIndex((_, i) => aes.every((x, j) => tf[i + j] === x));
  tf.set(fromHex('ad68f29f576f4bb9a36ad47af965346c'), at);
  tf.set(await sha256(tf.subarray(0, he)), he);
  await assert.rejects(readKdbx(tf, { password: 'pw' }), (e) => code('unsupported')(e) && /Twofish/.test(e.message));
  await assert.rejects(readKdbx(bytes, {}), code('invalid_argument'));
  await assert.rejects(writeKdbx(sampleTree(), { password: 'x', cipher: 'twofish' }), code('invalid_argument'));
  await assert.rejects(writeKdbx(sampleTree(), { password: 'x', kdf: { type: 'argon2d', memory: 1000 } }), code('invalid_argument'));
});

test('key files: XML v1/v2, raw 32 bytes, 64 hex chars, hashed arbitrary file', async () => {
  const key = Uint8Array.from({ length: 32 }, (_, i) => i + 1);
  const v1 = utf8Encode(`<?xml version="1.0" encoding="utf-8"?>\n<KeyFile><Meta><Version>1.00</Version></Meta><Key><Data>${b64encode(key)}</Data></Key></KeyFile>`);
  assert.deepEqual(await keyFileKey(v1), key);
  const hash = toHex((await sha256(key)).subarray(0, 4)).toUpperCase();
  const hex = toHex(key).toUpperCase().match(/.{8}/g).join(' ');
  const v2 = utf8Encode(`<KeyFile>\r\n<Meta><Version>2.0</Version></Meta>\r\n<Key><Data Hash="${hash}">\r\n ${hex}\r\n</Data></Key></KeyFile>`);
  assert.deepEqual(await keyFileKey(v2), key);
  const bad = utf8Encode(new TextDecoder().decode(v2).replace(hash, '00000000'));
  await assert.rejects(keyFileKey(bad), code('bad_keyfile'));
  const v3 = utf8Encode('<KeyFile><Meta><Version>3.0</Version></Meta><Key><Data>x</Data></Key></KeyFile>');
  await assert.rejects(keyFileKey(v3), code('bad_keyfile'));
  assert.deepEqual(await keyFileKey(key), key);
  assert.deepEqual(await keyFileKey(utf8Encode(toHex(key))), key);
  const other = utf8Encode('any old file contents, e.g. a photo');
  assert.deepEqual(await keyFileKey(other), await sha256(other));
  const notKeyFileXml = utf8Encode('<root>hello</root>');
  assert.deepEqual(await keyFileKey(notKeyFileXml), await sha256(notKeyFileXml));
  await assert.rejects(keyFileKey(new Uint8Array(0)), code('bad_keyfile'));
  // generated key files parse back to 32 bytes and verify their own hash
  const gen = await generateKeyFile();
  assert.match(new TextDecoder().decode(gen), /<Version>2\.0<\/Version>/);
  assert.equal((await keyFileKey(gen)).length, 32);
  // composite key = SHA-256(SHA-256(pw) || key)
  const want = await sha256(concatBytes(await sha256(utf8Encode('pw')), key));
  assert.deepEqual(await compositeKey('pw', key), want);
});

test('KDBX 3.1 read: AES-KDF, hashed blocks, Salsa20 inner stream, binaries, header hash', async () => {
  const tree = sampleTree();
  const keyFile = randomBytes(32);
  for (const opts of [{ password: 'v3 pässword' }, { password: 'v3', keyFile }, { keyFile, compression: false }]) {
    const bytes = await writeKdbx3(tree, opts);
    assert.deepEqual([...bytes.subarray(8, 12)], [0x01, 0x00, 0x03, 0x00]);
    const back = await readKdbx(bytes, opts);
    assert.deepEqual(back.version, { major: 3, minor: 1 });
    assert.deepEqual(back.header, { cipher: 'aes', compression: opts.compression !== false, innerStream: 'salsa20', kdf: { type: 'aes', rounds: 6000 } });
    assert.deepEqual(project(back, { entryCustomData: false }), project(tree, { entryCustomData: false }));
    assert.equal(back.meta.generator, 'Ludworks');
  }
  const bytes = await writeKdbx3(tree, { password: 'right' });
  await assert.rejects(readKdbx(bytes, { password: 'wrong' }), code('bad_credentials'));
  await assert.rejects(readKdbx(await writeKdbx3(tree, { password: 'p', tamper: 'block' }), { password: 'p' }), code('corrupt'));
  await assert.rejects(readKdbx(await writeKdbx3(tree, { password: 'p', tamper: 'headerHash' }), { password: 'p' }), code('corrupt'));
  // a KDBX 3.1 file converts to KDBX 4 losslessly
  const upgraded = await readKdbx(await writeKdbx(await readKdbx(bytes, { password: 'right' }), { password: 'v4', kdf: FAST_ARGON }), { password: 'v4' });
  assert.deepEqual(project(upgraded, { entryCustomData: false }), project(tree, { entryCustomData: false }));
});

test('KDBX 4.1 is written only when 4.1-only fields are used', async () => {
  const tree = sampleTree();
  assert.equal(requiresKdbx41(tree), false);
  tree.root.groups[0].tags = ['team'];
  assert.equal(requiresKdbx41(tree), true);
  const back = await readKdbx(await writeKdbx(tree, { password: 'x', kdf: FAST_ARGON }), { password: 'x' });
  assert.deepEqual(back.version, { major: 4, minor: 1 });
  assert.deepEqual(back.root.groups[0].tags, ['team']);
});

test('KeePass plain XML export round trip and protected-value handling', async () => {
  const tree = sampleTree();
  const xml = serializeKeePassXml(tree);
  assert.match(xml, /<Value ProtectInMemory="True">p@ss «ü» 🔑 &lt;\/Value&gt;<\/Value>/);
  assert.match(xml, /<CreationTime>2020-01-02T03:04:05Z<\/CreationTime>/);
  assert.doesNotMatch(xml, /Protected="True"/);
  const back = await parseKeePassXml(xml);
  assert.deepEqual(project(back), project(tree));
  const enc = xml.replace('<Value ProtectInMemory="True">p@ss', '<Value Protected="True">p@ss');
  await assert.rejects(parseKeePassXml(enc), code('unsupported'));
  await assert.rejects(parseKeePassXml('<NotKeePass/>'), code('corrupt'));
  await assert.rejects(parseKeePassXml('<!DOCTYPE x [<!ENTITY a "b">]><KeePassFile/>'), code('xml_invalid'));
  // a minimal hand-written export (as other tools produce) is accepted
  const minimal = await parseKeePassXml(
    '<KeePassFile><Root><Group><Name>R</Name><Entry><String><Key>Title</Key><Value>T</Value></String></Entry></Group></Root></KeePassFile>',
  );
  assert.equal(minimal.root.entries[0].fields[0].value, 'T');
  assert.match(minimal.root.entries[0].uuid, /^[0-9a-f]{32}$/);
  assert.equal(parseXml(xml).name, 'KeePassFile');
});
