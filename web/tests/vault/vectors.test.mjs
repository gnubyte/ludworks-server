// The committed spec vectors and cross-check fixtures must match the current implementation.
// (Regenerate with: node web/tests/vault/gen-vectors.mjs --write, then run crosscheck.py.)
import test from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import { asciiJson, buildVectors } from './gen-vectors.mjs';
import * as vc from '../../assets/js/lib/vault/crypto.js';
import { generateTotp } from '../../assets/js/lib/vault/totp.js';
import { readKdbx } from '../../assets/js/lib/vault/kdbx.js';
import { fromHex, toHex } from '../../assets/js/lib/vault/bytes.js';
import { matchUri, registrableDomain } from '../../assets/js/lib/vault/url-match.js';

const here = dirname(fileURLToPath(import.meta.url));
const vectorsPath = join(here, '..', '..', '..', 'docs', 'spec', 'vault-test-vectors.json');

test('docs/spec/vault-test-vectors.json is reproduced byte-for-byte', async () => {
  const committed = readFileSync(vectorsPath, 'utf8');
  assert.equal(asciiJson(await buildVectors()), committed);
});

test('vectors decrypt through the public API', async () => {
  const v = JSON.parse(readFileSync(vectorsPath, 'utf8'));
  const vaultKey = await vc.unlockVault(v.kdf.password, v.wrapped_key.server_params);
  assert.equal(toHex(vaultKey), v.wrapped_key.vault_key_hex);
  const item = await vc.decryptItem(vaultKey, v.item.id, v.item.data);
  assert.deepEqual(item, JSON.parse(v.item.plaintext_json));
  assert.equal(await vc.decryptFolder(vaultKey, v.folder.id, v.folder.sealed), v.folder.name);
  for (const c of v.open_must_fail) {
    await assert.rejects(vc.open(fromHex(c.key_hex), c.sealed, c.aad), (e) => e.code === c.error, c.case);
  }
  assert.equal(await vc.verifyKeyCheck(vaultKey, v.key_check.sealed), true);
  assert.equal(await vc.verifyKeyCheck(new Uint8Array(32), v.key_check.sealed), false);
  for (const c of v.registrable_domain) assert.equal(registrableDomain(c.host), c.registrable_domain, c.host);
  for (const c of v.url_match) assert.equal(matchUri(c.item_uri, c.page_url, c.match), c.expected, `${c.item_uri} ${c.match} ${c.page_url}`);
  for (const t of v.totp) {
    const uri = `otpauth://totp/x?secret=${t.secret_base32}&algorithm=${t.algorithm}&digits=${t.digits}&period=${t.period}`;
    assert.equal((await generateTotp(uri, t.unix_time * 1000)).code, t.code);
  }
});

test('cross-check KDBX fixtures open with the JS reader', async () => {
  const fx = join(here, 'fixtures');
  const expected = JSON.parse(readFileSync(join(fx, 'crosscheck-expected.json'), 'utf8'));
  for (const f of expected.fixtures) {
    const keyFile = f.keyfile ? new Uint8Array(readFileSync(join(fx, f.keyfile))) : undefined;
    const tree = await readKdbx(new Uint8Array(readFileSync(join(fx, f.file))), { password: f.password, keyFile });
    assert.equal(tree.header.cipher, f.cipher);
    const entries = [];
    const walk = (g, path) => {
      for (const e of g.entries) entries.push({ path, title: e.fields.find((x) => x.key === 'Title').value, password: e.fields.find((x) => x.key === 'Password').value });
      for (const c of g.groups) walk(c, path ? `${path}/${c.name}` : c.name);
    };
    walk(tree.root, '');
    assert.deepEqual(
      entries,
      expected.entries.map((e) => ({ path: e.path, title: e.fields.Title, password: e.fields.Password })),
    );
  }
});
