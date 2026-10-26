// Attachment decompression limits for KeePass XML / KDBX import (VAULT-CRYPTO.md §8.4; ARCHITECTURE §2:
// untrusted KDBX parsing caps decompressed sizes). Regression: every compressed <Meta><Binaries><Binary>
// was gunzipped and kept with only a per-attachment cap (256 MiB for XML, 512 MiB for KDBX), so a small
// file full of gzip bombs made the import worker allocate gigabytes. Sizes here are kept small (MiB).
import test from 'node:test';
import assert from 'node:assert/strict';
import { parseKeePassXml, newEntry, newTree } from '../../assets/js/lib/vault/keepass-xml.js';
import { readKdbx, writeKdbx } from '../../assets/js/lib/vault/kdbx.js';
import { importVault } from '../../assets/js/lib/vault/convert.js';
import { el } from '../../assets/js/lib/vault/xml.js';
import { b64encode, gzip, utf8Encode } from '../../assets/js/lib/vault/bytes.js';
import { writeKdbx3 } from './helpers/kdbx3-writer.mjs';

const MiB = 1024 * 1024;
const FAST_AES = { type: 'aes', rounds: 1000 };

async function compressedZeros(n) {
  return b64encode(await gzip(new Uint8Array(n)));
}
// Marked Compressed="True" but not gzip: decoding it fails, so a successful import proves it was never decoded.
const NOT_GZIP = b64encode(utf8Encode('this is not gzip data at all'));

function keepassXml(binaryValues, { linked = 1 } = {}) {
  const bins = binaryValues.map((v, i) => `<Binary ID="${i}" Compressed="True">${v}</Binary>`).join('');
  const refs = Array.from({ length: linked }, (_, i) => `<Binary><Key>file${i}.bin</Key><Value Ref="${i}"/></Binary>`).join('');
  return '<?xml version="1.0" encoding="utf-8" standalone="yes"?>\n<KeePassFile><Meta><Generator>test</Generator>' +
    `<Binaries>${bins}</Binaries></Meta><Root><Group><UUID>AAAAAAAAAAAAAAAAAAAAAA==</UUID><Name>Root</Name>` +
    '<Entry><UUID>AQEBAQEBAQEBAQEBAQEBAQ==</UUID><String><Key>Title</Key><Value>Team login</Value></String>' +
    `<String><Key>Password</Key><Value>pw</Value></String>${refs}</Entry></Group></Root></KeePassFile>`;
}

function treeWithMetaBinaries(values) {
  const tree = newTree('Shared');
  const e = newEntry();
  e.fields.push({ key: 'Title', value: 'Team login', protected: false }, { key: 'Password', value: 'pw', protected: true });
  tree.root.entries.push(e);
  // Meta/Binaries inside the (encrypted) XML: KeePass itself never writes these into KDBX 4.
  tree.meta.extra.push(el('Binaries', null, values.map((v, i) => el('Binary', { ID: String(i), Compressed: 'True' }, v))));
  return tree;
}

test('KeePass XML: compressed attachments share one decompressed budget', async () => {
  const xml = keepassXml(await Promise.all([0, 1, 2].map(() => compressedZeros(512 * 1024))));
  await assert.rejects(parseKeePassXml(xml, { maxDecompressedBytes: MiB }), (e) => e.code === 'limit_exceeded');
  const tree = await parseKeePassXml(xml); // default 512 MiB total: fine
  assert.deepEqual(tree.binaries.map((b) => b.data.length), [512 * 1024, 512 * 1024, 512 * 1024]);
  // Each attachment alone fits, together they do not.
  await assert.rejects(parseKeePassXml(xml, { maxDecompressedBytes: 1.4 * MiB }), (e) => e.code === 'limit_exceeded');
  assert.equal((await parseKeePassXml(xml, { maxDecompressedBytes: 1.6 * MiB })).binaries.length, 3);
});

test('KeePass XML import never decodes attachments and still reports them', async () => {
  const xml = keepassXml([NOT_GZIP, NOT_GZIP, NOT_GZIP]);
  await assert.rejects(parseKeePassXml(xml), (e) => e.code === 'corrupt'); // decoding would fail...
  const v = await importVault('keepass-xml', xml); // ...so the import provably skipped it
  assert.equal(v.items.length, 1);
  assert.equal(v.items[0].login.password, 'pw');
  assert.ok(v.warnings.some((w) => /1 attachment\(s\) were not imported/.test(w)), v.warnings.join('; '));
  assert.ok(v.warnings.some((w) => /2 attachment\(s\) not linked to any entry were ignored/.test(w)), v.warnings.join('; '));
  const tree = await parseKeePassXml(xml, { decodeBinaries: false });
  assert.ok(tree.binaries.every((b) => b.skipped && b.data.length === 0));
});

test('KDBX 4: the payload and Meta attachments share limits.maxDecompressedBytes', async () => {
  const bin = await compressedZeros(512 * 1024);
  const bytes = await writeKdbx(treeWithMetaBinaries([bin, bin, bin]), { password: 'team', kdf: FAST_AES });
  await assert.rejects(readKdbx(bytes, { password: 'team', limits: { maxDecompressedBytes: MiB } }), (e) => e.code === 'limit_exceeded');
  const tree = await readKdbx(bytes, { password: 'team', limits: { maxDecompressedBytes: 4 * MiB } });
  assert.equal(tree.binaries.length, 3);

  const bomb = await writeKdbx(treeWithMetaBinaries([NOT_GZIP, NOT_GZIP]), { password: 'team', kdf: FAST_AES });
  await assert.rejects(readKdbx(bomb, { password: 'team' }), (e) => e.code === 'corrupt');
  const v = await importVault('kdbx', bomb, { password: 'team' });
  assert.equal(v.items.length, 1);
  assert.ok(v.warnings.some((w) => /2 attachment\(s\) not linked/.test(w)), v.warnings.join('; '));
});

test('KDBX 3: Meta attachments are capped together and skipped on import', async () => {
  const bin = await compressedZeros(512 * 1024);
  const tree = treeWithMetaBinaries([bin, bin, bin]);
  const bytes = await writeKdbx3(tree, { password: 'team', rounds: 1000 });
  await assert.rejects(readKdbx(bytes, { password: 'team', limits: { maxDecompressedBytes: MiB } }), (e) => e.code === 'limit_exceeded');
  assert.equal((await readKdbx(bytes, { password: 'team' })).binaries.length, 3);
  const v = await importVault('kdbx', await writeKdbx3(treeWithMetaBinaries([NOT_GZIP]), { password: 'team', rounds: 1000 }), { password: 'team' });
  assert.equal(v.items.length, 1);
});

test('KDBX 4 attachments in the inner header still round-trip and are reported on import', async () => {
  const tree = newTree('DB');
  const e = newEntry();
  e.fields.push({ key: 'Title', value: 'With file', protected: false });
  e.binaries.push({ key: 'doc.txt', ref: 0 });
  tree.root.entries.push(e);
  tree.binaries.push({ data: utf8Encode('hello attachment'), protected: false });
  const bytes = await writeKdbx(tree, { password: 'pw', kdf: FAST_AES });
  const back = await readKdbx(bytes, { password: 'pw' });
  assert.equal(new TextDecoder().decode(back.binaries[0].data), 'hello attachment');
  const v = await importVault('kdbx', bytes, { password: 'pw' });
  assert.deepEqual(v.warnings, ['"With file": 1 attachment(s) were not imported']);
});
