// Generates docs/spec/vault-test-vectors.json (deterministic) and the KDBX cross-check fixtures in
// web/tests/vault/fixtures/ (random seeds; regenerate together with crosscheck-expected.json).
//
//   node web/tests/vault/gen-vectors.mjs --write    # from ludworks-server/
//   python3 -I web/tests/vault/crosscheck.py        # independent verification
import { mkdirSync, writeFileSync } from 'node:fs';
import { dirname, join } from 'node:path';
import { fileURLToPath } from 'node:url';
import * as vc from '../../assets/js/lib/vault/crypto.js';
import { base32encode, computeTotp, parseOtpauth } from '../../assets/js/lib/vault/totp.js';
import { generatePassphrase, generatePassword, randomInt } from '../../assets/js/lib/vault/generator.js';
import { WORDS } from '../../assets/js/lib/vault/wordlist.js';
import { generateKeyFile, writeKdbx } from '../../assets/js/lib/vault/kdbx.js';
import { newEntry, newGroup, newTimes, newTree } from '../../assets/js/lib/vault/keepass-xml.js';
import { concatBytes, fromHex, sha256, toHex, u32le, utf8Encode } from '../../assets/js/lib/vault/bytes.js';
import { matchUri, registrableDomain } from '../../assets/js/lib/vault/url-match.js';
import { PSL_DATE, PSL_SOURCE_SHA256 } from '../../assets/js/lib/vault/psl-data.js';

// Normative registrable-domain and URI-matching cases (VAULT-CRYPTO.md §3.1, §8.3). The expectations are
// written by hand; buildVectors() refuses to emit a vector the reference implementation disagrees with.
const REGISTRABLE_DOMAIN_CASES = [
  ['www.bbc.co.uk', 'bbc.co.uk'],
  ['example.co.uk', 'example.co.uk'],
  ['co.uk', 'co.uk'],
  ['gist.github.com', 'github.com'],
  ['victim.github.io', 'victim.github.io'],
  ['github.io', 'github.io'],
  ['mynas.duckdns.org', 'mynas.duckdns.org'],
  ['a.tail1234.ts.net', 'tail1234.ts.net'],
  ['alice.me.uk', 'alice.me.uk'],
  ['shop.example.com.au', 'example.com.au'],
  ['x.shop.ne.jp', 'shop.ne.jp'],
  ['a.tienda.gob.mx', 'tienda.gob.mx'],
  ['bucket.s3.amazonaws.com', 'bucket.s3.amazonaws.com'],
  ['home.synology.me', 'home.synology.me'],
  ['foo.bar.ck', 'foo.bar.ck'],
  ['www.ck', 'www.ck'],
  ['a.www.ck', 'www.ck'],
  ['a.b.unknowntld', 'b.unknowntld'],
  ['Mail.Example.COM.', 'example.com'],
  ['login.example.com:8443', 'example.com'],
  ['www.example.公司.cn', 'example.xn--55qx5d.cn'],
  ['shop.bücher.de', 'xn--bcher-kva.de'],
  ['192.168.1.10', '192.168.1.10'],
  ['[::1]', '[::1]'],
  ['localhost', 'localhost'],
  ['intranet', 'intranet'],
];

const URL_MATCH_CASES = [
  // Tenants of shared hosting / dynamic-DNS suffixes (PSL private section) are different sites.
  ['https://victim-project.github.io/admin/login', 'domain', 'https://attacker.github.io/phish', false],
  ['https://mynas.duckdns.org', null, 'https://attacker.duckdns.org/login', false],
  ['https://home.synology.me:5001', 'domain', 'https://evil.synology.me/', false],
  ['https://nas.tail1234.ts.net', 'domain', 'https://evil.tail9999.ts.net/', false],
  ['https://app.herokuapp.com', 'domain', 'https://evil.herokuapp.com/', false],
  ['https://site.pages.dev', 'domain', 'https://evil.pages.dev/', false],
  ['https://bucket.s3.amazonaws.com', 'domain', 'https://evil.s3.amazonaws.com/', false],
  ['https://alice.me.uk', 'domain', 'https://bob.me.uk/', false],
  ['https://shop1.ne.jp', 'domain', 'https://shop2.ne.jp/', false],
  ['https://tienda.gob.mx', 'domain', 'https://otra.gob.mx/', false],
  ['https://bbc.co.uk', 'domain', 'https://evil.co.uk/', false],
  ['https://github.io', 'domain', 'https://attacker.github.io/', false],
  ['https://github.com', 'domain', 'https://github.com.evil.io/', false],
  // Same registrable domain.
  ['https://a.example.co.uk', 'domain', 'https://b.example.co.uk/', true],
  ['https://github.com/login', 'domain', 'https://gist.github.com/x', true],
  ['https://a.tail1234.ts.net', 'domain', 'https://b.tail1234.ts.net/', true],
  ['https://github.io', 'domain', 'https://github.io/', true],
  ['https://bücher.de', 'domain', 'https://www.xn--bcher-kva.de/', true],
  ['https://foo.bar.ck', 'domain', 'https://x.foo.bar.ck/', true],
  ['https://a.www.ck', 'domain', 'https://www.ck/', true],
  ['example.com', 'domain', 'https://login.example.com/', true],
  // Transport and port rules.
  ['https://example.com', 'domain', 'http://example.com/', false],
  ['http://example.com', 'domain', 'https://example.com/', true],
  ['http://192.168.1.5:8080', 'domain', 'http://192.168.1.5:8080/admin', true],
  ['http://192.168.1.5:8080', 'domain', 'http://192.168.1.5:9090/', false],
  ['http://localhost:3000/x', 'domain', 'http://localhost:3000/y', true],
  ['https://app.example.com', 'host', 'https://app.example.com:443/a', true],
  ['https://app.example.com:8443', 'host', 'https://app.example.com/', false],
  ['https://app.example.com', 'host', 'https://www.example.com/', false],
  // starts_with with an absolute http(s) URI requires the same origin.
  ['https://example.com/account', 'starts_with', 'https://example.com/account/settings', true],
  ['https://bank.com', 'starts_with', 'https://bank.com.evil.net/', false],
  ['https://bank.com', 'starts_with', 'https://bank.com@evil.net/', false],
  ['https://bank.com', 'starts_with', 'https://bank.com:8443/', false],
  ['androidapp://com.example', 'starts_with', 'androidapp://com.example.app', true],
  // exact / regex / never.
  ['https://example.com/a', 'exact', 'https://example.com/a/', false],
  ['^https://(www\\.)?example\\.com/', 'regex', 'https://WWW.example.com/x', true],
  ['^(a+)+$', 'regex', 'https://aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa!', false],
  ['https://example.com', 'never', 'https://example.com', false],
];

function urlVectors() {
  const registrable = REGISTRABLE_DOMAIN_CASES.map(([host, expected]) => {
    const got = registrableDomain(host);
    if (got !== expected) throw new Error(`registrableDomain(${host}) = ${got}, want ${expected}`);
    return { host, registrable_domain: expected };
  });
  const matches = URL_MATCH_CASES.map(([itemUri, match, pageUrl, expected]) => {
    if (matchUri(itemUri, pageUrl, match) !== expected) throw new Error(`matchUri(${itemUri}, ${pageUrl}, ${match}) != ${expected}`);
    return { item_uri: itemUri, match, page_url: pageUrl, expected };
  });
  return {
    public_suffix_list: {
      snapshot: PSL_DATE,
      source_sha256: PSL_SOURCE_SHA256,
      rules: 'web/assets/js/lib/vault/psl-data.js (ICANN + PRIVATE sections, ASCII/punycode, generated by web/tests/vault/gen-psl.mjs)',
    },
    registrable_domain: registrable,
    url_match: matches,
  };
}

const here = dirname(fileURLToPath(import.meta.url));
const serverRoot = join(here, '..', '..', '..');
const range = (n, start = 0) => Uint8Array.from({ length: n }, (_, i) => start + i);

/** Deterministic test byte stream: SHA-256(seed ‖ LE32(0)) ‖ SHA-256(seed ‖ LE32(1)) ‖ ... */
export async function testByteStream(seed, nBytes) {
  const blocks = [];
  for (let i = 0; i * 32 < nBytes; i++) blocks.push(await sha256(concatBytes(seed, u32le(i))));
  const all = concatBytes(...blocks);
  let pos = 0;
  return (n) => {
    if (pos + n > all.length) throw new Error('test byte stream exhausted');
    const out = all.slice(pos, pos + n);
    pos += n;
    return out;
  };
}

export async function buildVectors() {
  const { sealWithNonce, deriveWrapKeyBytes } = vc.__testing;
  const password = 'correct horse battery staple';
  const salt = range(16, 0x00);
  const iterations = 600000;
  const vaultKey = range(32, 0x10);
  const masterKey = await vc.deriveMasterKey(password, salt, iterations);
  const wrapKey = await deriveWrapKeyBytes(masterKey);
  const wrapNonce = range(12, 0xa0);
  const wrapped = await sealWithNonce(wrapKey, vaultKey, vc.AAD_KEY, wrapNonce);

  const nfcPassword = 'Café Ångström'; // decomposed é, Angstrom sign → NFC "Café Ångström"
  const nfcMaster = await vc.deriveMasterKey(nfcPassword, salt, iterations);

  const itemId = '3f0c9a52-7d1e-4b6a-9c2f-8e5d4a1b0c7e';
  const item = {
    type: 'login', name: 'GitHub', favorite: false, notes: 'Ünïcödé ✓ notes\nline 2',
    login: {
      username: 'me@example.com', password: 'hunter2-💡',
      uris: [{ uri: 'https://github.com/login', match: 'domain' }],
      totp: 'otpauth://totp/GitHub:me?secret=JBSWY3DPEHPK3PXP&issuer=GitHub',
    },
    fields: [{ name: 'PIN', value: '1234', hidden: true }],
    password_history: [{ password: 'old', changed_at: '2026-01-01T00:00:00Z' }],
    created_at: '2026-10-26T12:00:00Z', updated_at: '2026-10-26T12:00:00Z',
  };
  const itemJson = JSON.stringify(item);
  const itemNonce = range(12, 0xb0);
  const itemSealed = await sealWithNonce(vaultKey, utf8Encode(itemJson), vc.AAD_ITEM_PREFIX + itemId, itemNonce);

  const folderId = 'b2d4f6a8-1c3e-4f5a-8b7c-9d0e1f2a3b4c';
  const folderName = 'Work/Email ✓';
  const folderJson = JSON.stringify({ name: folderName });
  const folderNonce = range(12, 0xc0);
  const folderSealed = await sealWithNonce(vaultKey, utf8Encode(folderJson), vc.AAD_FOLDER_PREFIX + folderId, folderNonce);

  const keyCheckNonce = range(12, 0xd0);
  const keyCheckSealed = await sealWithNonce(vaultKey, new Uint8Array(0), vc.AAD_KEY_CHECK, keyCheckNonce);

  const [, n64, ct64] = itemSealed.split('.');
  const tampered = vc.b64decode(ct64);
  tampered[tampered.length - 1] ^= 0x01;

  // TOTP: RFC 6238 Appendix B (8 digits) plus default-parameter vectors.
  const seeds = {
    SHA1: utf8Encode('12345678901234567890'),
    SHA256: utf8Encode('12345678901234567890123456789012'),
    SHA512: utf8Encode('1234567890123456789012345678901234567890123456789012345678901234'),
  };
  const totp = [];
  for (const t of [59, 1111111109, 1111111111, 1234567890, 2000000000, 20000000000]) {
    for (const algorithm of ['SHA1', 'SHA256', 'SHA512']) {
      const r = await computeTotp(seeds[algorithm], { algorithm, digits: 8, period: 30 }, t * 1000);
      totp.push({ secret_base32: base32encode(seeds[algorithm]), algorithm, digits: 8, period: 30, unix_time: t, code: r.code });
    }
  }
  for (const [t, period, digits] of [[1700000000, 30, 6], [1700000000, 60, 7], [1893456000, 30, 6]]) {
    const r = await computeTotp(fromHex('48656c6c6f21deadbeef'), { algorithm: 'SHA1', digits, period }, t * 1000);
    totp.push({ secret_base32: 'JBSWY3DPEHPK3PXP', algorithm: 'SHA1', digits, period, unix_time: t, code: r.code });
  }
  const otpUris = [
    'otpauth://totp/ACME%20Co:john.doe%40email.com?secret=HXDMVJECJJWSRB3HWIZR4IFUGFTMXBOZ&issuer=ACME%20Co&algorithm=SHA256&digits=8&period=60',
    'otpauth://totp/alice?secret=jbsw%20y3dp%20ehpk%203pxp',
  ].map((uri) => ({ uri, parsed: parseOtpauth(uri) }));

  // Generator determinism.
  const genSeed = utf8Encode('ludworks/vault/generator/v1');
  const pwCases = [
    { length: 20 },
    { length: 8, excludeAmbiguous: true },
    { length: 32, symbols: false },
    { length: 12, upper: false, lower: false, symbols: false },
  ];
  const passwords = [];
  for (const options of pwCases) passwords.push({ options, password: generatePassword({ ...options, rng: await testByteStream(genSeed, 8192) }) });
  const ppOptions = { words: 6, separator: '-', capitalize: true, includeNumber: true };
  const passphrase = generatePassphrase({ ...ppOptions, rng: await testByteStream(genSeed, 8192) });
  const src = await testByteStream(genSeed, 8192);
  const buf = { b: new Uint8Array(0), p: 0 };
  const u32 = () => {
    if (buf.p + 4 > buf.b.length) {
      buf.b = src(256);
      buf.p = 0;
    }
    const v = new DataView(buf.b.buffer).getUint32(buf.p, true);
    buf.p += 4;
    return v;
  };
  const wordIndices = Array.from({ length: ppOptions.words }, () => randomInt(WORDS.length, { u32 }));

  return {
    version: 1,
    description:
      'Ludworks vault test vectors (VAULT-CRYPTO.md §8). Raw bytes are lowercase hex (*_hex); b64 = RFC 4648 standard base64 with padding. ' +
      'Sealed strings use the fixed nonces given here; production code must always use random nonces.',
    generated_by: 'web/tests/vault/gen-vectors.mjs (verified by web/tests/vault/crosscheck.py)',
    kdf: {
      password,
      kdf: 'pbkdf2-sha256',
      kdf_iterations: iterations,
      kdf_salt_hex: toHex(salt),
      kdf_salt: vc.b64encode(salt),
      master_key_hex: toHex(masterKey),
      hkdf_salt: '',
      hkdf_info: 'ludworks/vault/wrap/v1',
      wrap_key_hex: toHex(wrapKey),
    },
    nfc: {
      password: nfcPassword,
      password_nfc_utf8_hex: toHex(utf8Encode(nfcPassword.normalize('NFC'))),
      kdf_iterations: iterations,
      kdf_salt_hex: toHex(salt),
      master_key_hex: toHex(nfcMaster),
    },
    wrapped_key: {
      vault_key_hex: toHex(vaultKey),
      aad: vc.AAD_KEY,
      nonce_hex: toHex(wrapNonce),
      sealed: wrapped,
      server_params: { kdf: 'pbkdf2-sha256', kdf_iterations: iterations, kdf_salt: vc.b64encode(salt), wrapped_key: wrapped },
    },
    item: { id: itemId, aad: vc.AAD_ITEM_PREFIX + itemId, plaintext_json: itemJson, nonce_hex: toHex(itemNonce), data: itemSealed },
    folder: { id: folderId, aad: vc.AAD_FOLDER_PREFIX + folderId, name: folderName, plaintext_json: folderJson, nonce_hex: toHex(folderNonce), sealed: folderSealed },
    open_must_fail: [
      { case: 'item data opened with the folder AAD', key_hex: toHex(vaultKey), sealed: itemSealed, aad: vc.AAD_FOLDER_PREFIX + itemId, error: 'decrypt_failed' },
      { case: 'item data opened under another id', key_hex: toHex(vaultKey), sealed: itemSealed, aad: `${vc.AAD_ITEM_PREFIX}${folderId}`, error: 'decrypt_failed' },
      { case: 'last tag byte flipped', key_hex: toHex(vaultKey), sealed: `v1.${n64}.${vc.b64encode(tampered)}`, aad: vc.AAD_ITEM_PREFIX + itemId, error: 'decrypt_failed' },
      { case: 'wrapped key opened with the vault key', key_hex: toHex(vaultKey), sealed: wrapped, aad: vc.AAD_KEY, error: 'decrypt_failed' },
      { case: 'unknown version prefix', key_hex: toHex(vaultKey), sealed: `v2.${n64}.${ct64}`, aad: vc.AAD_ITEM_PREFIX + itemId, error: 'unsupported_version' },
      { case: 'key_check opened with another key (a retired vault key)', key_hex: toHex(range(32, 0x40)), sealed: keyCheckSealed, aad: vc.AAD_KEY_CHECK, error: 'decrypt_failed' },
    ],
    key_check: {
      description: 'key_check = Seal(vault_key, "" (empty plaintext), AAD "ludworks/vault/check/v1"): exactly 16 bytes of ciphertext (the GCM tag)',
      vault_key_hex: toHex(vaultKey),
      aad: vc.AAD_KEY_CHECK,
      nonce_hex: toHex(keyCheckNonce),
      sealed: keyCheckSealed,
    },
    ...urlVectors(),
    totp,
    otpauth: otpUris,
    generator: {
      byte_stream: 'stream = SHA-256(seed || LE32(0)) || SHA-256(seed || LE32(1)) || ...; each draw consumes the next 4 bytes as a little-endian uint32',
      seed_utf8: 'ludworks/vault/generator/v1',
      random_int: 'limit = 2^32 - (2^32 mod n); draw x until x < limit; return x mod n',
      charsets: { upper: 'ABCDEFGHIJKLMNOPQRSTUVWXYZ', lower: 'abcdefghijklmnopqrstuvwxyz', digits: '0123456789', symbols: '!@#$%^&*()-_=+[]{};:,.?/', ambiguous: 'Il1O0' },
      password_algorithm:
        'pool = enabled sets concatenated in order upper, lower, digits, symbols (ambiguous chars removed from upper/lower/digits when excludeAmbiguous); ' +
        'repeat { pw = length × pool[randomInt(|pool|)] } until every enabled set occurs in pw. Defaults: length 20, all sets on, excludeAmbiguous false. ' +
        'Each case starts a fresh stream.',
      passwords,
      passphrase: {
        wordlist: 'web/assets/js/lib/vault/wordlist.js',
        wordlist_size: WORDS.length,
        algorithm: 'for each word: WORDS[randomInt(N)]; capitalize first letter; if includeNumber: pos = randomInt(words), digit = randomInt(10), append digit to word[pos]; join with separator',
        options: ppOptions,
        word_indices: wordIndices,
        passphrase,
      },
    },
  };
}

function crosscheckTree() {
  const tree = newTree('Cross-check DB');
  const work = newGroup('Work');
  const email = newGroup('Email · 邮件');
  work.groups.push(email);
  tree.root.groups.push(work);
  const f = (key, value, prot = false) => ({ key, value, protected: prot });
  const a = newEntry();
  a.times = newTimes('2024-01-01T00:00:00Z', '2024-03-01T00:00:00Z');
  a.fields = [
    f('Title', 'Mail ✉'), f('UserName', 'me@example.com'), f('Password', 'p@ss «ü» 🔑 </Value> & more', true), f('URL', 'https://mail.example.com'),
    f('Notes', 'multi\nline\r\nnotes'), f('otp', 'otpauth://totp/Mail:me?secret=JBSWY3DPEHPK3PXP&issuer=Mail', true), f('PIN', '0000', true), f('Region', 'eu'),
  ];
  const h = newEntry();
  h.uuid = a.uuid;
  h.times = newTimes('2024-01-01T00:00:00Z', '2024-01-01T00:00:00Z');
  h.fields = a.fields.map((x) => (x.key === 'Password' ? { ...x, value: 'previous-pw' } : { ...x }));
  a.history.push(h);
  email.entries.push(a);
  const b = newEntry();
  b.fields = [f('Title', 'Root entry'), f('UserName', 'root'), f('Password', '', true), f('URL', ''), f('Notes', '')];
  tree.root.entries.push(b);
  return tree;
}

function expectedEntries(tree) {
  const out = [];
  const walk = (g, path) => {
    for (const e of g.entries) {
      out.push({
        path,
        fields: Object.fromEntries(e.fields.map((x) => [x.key, x.value])),
        protected: e.fields.filter((x) => x.protected).map((x) => x.key),
        history_passwords: e.history.map((x) => x.fields.find((y) => y.key === 'Password').value),
        creation_time: e.times.creationTime,
        modification_time: e.times.lastModificationTime,
      });
    }
    for (const c of g.groups) walk(c, path ? `${path}/${c.name}` : c.name);
  };
  walk(tree.root, '');
  return out;
}

/** ASCII-only JSON (\uXXXX escapes) so editors cannot silently re-normalise the Unicode test inputs. */
export function asciiJson(v) {
  const s = JSON.stringify(v, null, 2).replace(/[\u007f-￿]/g, (c) => `\\u${c.charCodeAt(0).toString(16).padStart(4, '0')}`);
  return `${s}\n`;
}

async function main() {
  const vectors = await buildVectors();
  writeFileSync(join(serverRoot, 'docs', 'spec', 'vault-test-vectors.json'), asciiJson(vectors));
  const fx = join(here, 'fixtures');
  mkdirSync(fx, { recursive: true });
  const tree = crosscheckTree();
  const keyFile = await generateKeyFile();
  writeFileSync(join(fx, 'crosscheck.keyx'), keyFile);
  const fixtures = [
    { file: 'crosscheck-argon2id-aes.kdbx', password: 'crosscheck ✓ pässword', keyfile: 'crosscheck.keyx', cipher: 'aes', kdf: { type: 'argon2id', memory: 64 * 1024 * 1024, iterations: 2, parallelism: 2 } },
    { file: 'crosscheck-aeskdf-chacha20.kdbx', password: 'second fixture', keyfile: null, cipher: 'chacha20', kdf: { type: 'aes', rounds: 60000 } },
  ];
  for (const f of fixtures) {
    const bytes = await writeKdbx(tree, { password: f.password, keyFile: f.keyfile ? keyFile : undefined, cipher: f.cipher, kdf: f.kdf });
    writeFileSync(join(fx, f.file), bytes);
  }
  writeFileSync(join(fx, 'crosscheck-expected.json'), asciiJson({ fixtures, database_name: tree.root.name, entries: expectedEntries(tree) }));
  console.log('wrote docs/spec/vault-test-vectors.json and web/tests/vault/fixtures/*');
}

// Explicit flag so importing this module (vectors.test.mjs) or a test runner never rewrites fixtures.
if (process.argv.includes('--write')) await main();
