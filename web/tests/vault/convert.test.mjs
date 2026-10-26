// Conversions between KeePass / Bitwarden and the vault interchange shape.
import test from 'node:test';
import assert from 'node:assert/strict';
import {
  bitwardenToVault, detectFormat, exportVault, extractKeePassTotp, importVault, kdbxTreeToVault, keepassXmlToVault,
  normalizeFolderPath, vaultToBitwarden, vaultToKdbxTree, vaultToKeepassXml,
} from '../../assets/js/lib/vault/convert.js';
import { newEntry, newGroup, newTimes, newTree } from '../../assets/js/lib/vault/keepass-xml.js';
import { parseOtpauth } from '../../assets/js/lib/vault/totp.js';
import { utf8Encode } from '../../assets/js/lib/vault/bytes.js';

const FAST = { kdf: { type: 'argon2id', memory: 1024 * 1024, iterations: 1, parallelism: 1 } };

const VAULT = {
  folders: [{ id: 'f1', name: 'Work' }, { id: 'f2', name: 'Work/Email' }, { id: 'f3', name: 'Empty Folder' }],
  items: [
    {
      folderPath: 'Work/Email', type: 'login', name: 'Mail ✉', favorite: true, notes: 'n1\nn2',
      login: {
        username: 'me@example.com', password: 'current-pw',
        uris: [{ uri: 'https://mail.example.com' }, { uri: 'https://webmail.example.com' }, { uri: 'https://m.example.com' }],
        totp: 'otpauth://totp/Mail:me?secret=JBSWY3DPEHPK3PXP&issuer=Mail&digits=8',
      },
      fields: [{ name: 'PIN', value: '0000', hidden: true }, { name: 'Title', value: 'collides with KeePass Title', hidden: false }, { name: 'PIN', value: 'dup', hidden: false }],
      password_history: [
        { password: 'pw-2', changed_at: '2024-03-01T00:00:00Z' },
        { password: 'pw-1', changed_at: '2024-02-01T00:00:00Z' },
      ],
      created_at: '2024-01-01T00:00:00Z', updated_at: '2024-03-01T00:00:00Z',
    },
    {
      folderPath: '', type: 'note', name: 'Wifi', favorite: false, notes: 'SSID: home\nPSK: hunter2', fields: [], password_history: [],
      created_at: '2023-05-05T05:05:05Z', updated_at: '2023-05-05T05:05:05Z',
    },
    {
      folderPath: 'Work', type: 'card', name: 'Corp card', favorite: false, notes: '',
      card: { cardholder: 'Jane Doe', brand: 'Visa', number: '4111111111111111', exp_month: '12', exp_year: '2030', code: '321' },
      fields: [], password_history: [], created_at: '2023-01-01T00:00:00Z', updated_at: '2023-01-02T00:00:00Z',
    },
    {
      folderPath: 'Work', type: 'identity', name: 'Me', favorite: false, notes: '',
      identity: {
        title: 'Dr', first_name: 'Jane', middle_name: 'Q', last_name: 'Doe', company: 'ACME', email: 'jane@acme.test', phone: '+1 555',
        address1: '1 Main St', address2: 'Apt 2', city: 'Springfield', state: 'IL', postal_code: '62701', country: 'US',
        username: 'jdoe', ssn: '123-45-6789', passport: 'X123', license: 'D456',
      },
      fields: [], password_history: [], created_at: '2023-01-01T00:00:00Z', updated_at: '2023-01-01T00:00:00Z',
    },
  ],
};

const strip = (d) => ({
  folders: d.folders.map((f) => f.name).sort(),
  items: d.items.map(({ folderPath, ...rest }) => ({ folderPath, ...rest })).sort((a, b) => a.name.localeCompare(b.name)),
});

test('vault → KeePass tree → vault is lossless for the vault model', () => {
  const tree = vaultToKdbxTree(VAULT, { databaseName: 'Mine' });
  assert.equal(tree.root.name, 'Mine');
  assert.deepEqual(tree.root.groups.map((g) => g.name).sort(), ['Empty Folder', 'Work']);
  const mail = tree.root.groups.find((g) => g.name === 'Work').groups[0].entries[0];
  assert.deepEqual(mail.fields.map((f) => f.key), ['Title', 'UserName', 'Password', 'URL', 'Notes', 'KP2A_URL', 'KP2A_URL_1', 'otp', 'PIN', 'Title (2)', 'PIN (2)']);
  assert.ok(mail.fields.find((f) => f.key === 'otp').protected);
  assert.deepEqual(mail.tags, ['Favorite']);
  assert.deepEqual(mail.history.map((h) => h.fields.find((f) => f.key === 'Password').value), ['pw-1', 'pw-2']);
  const back = kdbxTreeToVault(tree);
  // renamed custom fields keep their (deduplicated) KeePass names
  const want = structuredClone(VAULT);
  want.items[0].fields = [{ name: 'PIN', value: '0000', hidden: true }, { name: 'Title (2)', value: 'collides with KeePass Title', hidden: false }, { name: 'PIN (2)', value: 'dup', hidden: false }];
  assert.deepEqual(strip(back), strip(want));
  assert.deepEqual(back.warnings, []);
});

test('vault ⇄ KDBX and KeePass XML end to end', async () => {
  const kdbx = await exportVault('kdbx', VAULT, { password: 'pw', ...FAST });
  assert.ok(kdbx instanceof Uint8Array);
  assert.equal(detectFormat(kdbx), 'kdbx');
  const fromKdbx = await importVault('kdbx', kdbx, { password: 'pw' });
  assert.deepEqual(strip(fromKdbx).items.map((i) => i.name), ['Corp card', 'Mail ✉', 'Me', 'Wifi']);
  assert.equal(fromKdbx.items.find((i) => i.type === 'identity').identity.ssn, '123-45-6789');
  const xml = vaultToKeepassXml(VAULT);
  assert.equal(detectFormat(xml), 'keepass-xml');
  assert.deepEqual(strip(await keepassXmlToVault(xml)), strip(kdbxTreeToVault(vaultToKdbxTree(VAULT))));
  assert.deepEqual(strip(await importVault('keepass-xml', utf8Encode(xml))), strip(fromKdbx));
});

test('vault ⇄ Bitwarden JSON / CSV', async () => {
  const bw = vaultToBitwarden(VAULT);
  assert.deepEqual(bw.items.map((i) => i.type), [1, 2, 3, 4]);
  assert.equal(bw.items[0].folderId, bw.folders.find((f) => f.name === 'Work/Email').id);
  const back = bitwardenToVault(bw);
  const want = structuredClone(VAULT);
  assert.deepEqual(strip(back), strip(want));

  const json = await exportVault('bitwarden-json', VAULT);
  assert.equal(detectFormat(json), 'bitwarden-json');
  assert.deepEqual(strip(await importVault('bitwarden-json', json)), strip(want));
  const locked = await exportVault('bitwarden-json', VAULT, { password: 'file-pw', kdfIterations: 5000 });
  assert.deepEqual(strip(await importVault('bitwarden-json', locked, { password: 'file-pw' })), strip(want));

  const csv = await exportVault('bitwarden-csv', VAULT);
  assert.equal(detectFormat(csv), 'bitwarden-csv');
  const fromCsv = await importVault('bitwarden-csv', csv);
  const mail = fromCsv.items.find((i) => i.name === 'Mail ✉');
  assert.deepEqual(mail.login.uris.map((u) => u.uri), VAULT.items[0].login.uris.map((u) => u.uri));
  assert.equal(mail.login.totp, VAULT.items[0].login.totp);
  assert.equal(fromCsv.items.find((i) => i.name === 'Corp card').type, 'note'); // CSV has no card type
});

test('Bitwarden specifics: URI match modes, field types, ssh keys, trash, passkeys', () => {
  const v = bitwardenToVault({
    folders: [{ id: 'x', name: ' A / B ' }],
    items: [
      {
        type: 1, name: 'L', folderId: 'x',
        login: { uris: [{ uri: 'a.com', match: 1 }, { uri: 'b.com', match: null }, { uri: '^x', match: 4 }], fido2Credentials: [{}], username: null, password: null, totp: null },
        fields: [{ name: 'bool', value: 'true', type: 2 }, { name: 'link', value: null, type: 3, linkedId: 100 }],
      },
      { type: 5, name: 'SSH', sshKey: { privateKey: 'PRIV', publicKey: 'PUB', keyFingerprint: 'FP' } },
      { type: 1, name: 'Trashed', deletedDate: '2024-01-01T00:00:00Z', login: {} },
      { type: 4, name: 'Id', identity: { firstName: 'A', address3: 'Line 3' } },
    ],
  });
  assert.deepEqual(v.folders.map((f) => f.name), ['A/B']);
  const [l, ssh, id] = v.items;
  assert.equal(l.folderPath, 'A/B');
  assert.deepEqual(l.login.uris, [{ uri: 'a.com', match: 'host' }, { uri: 'b.com' }, { uri: '^x', match: 'regex' }]);
  assert.deepEqual(l.fields, [{ name: 'bool', value: 'true', hidden: false }]);
  assert.equal(ssh.type, 'note');
  assert.deepEqual(ssh.fields.map((f) => [f.name, f.hidden]), [['Private key', true], ['Public key', false], ['Fingerprint', false]]);
  assert.equal(id.identity.first_name, 'A');
  assert.equal(id.fields[0].value, 'Line 3');
  assert.equal(v.items.length, 3);
  assert.equal(v.warnings.length, 1); // passkey skipped
  assert.deepEqual(vaultToBitwarden({ items: [{ type: 'login', name: 'm', login: { uris: [{ uri: 'u', match: 'starts_with' }] } }] }).items[0].login.uris, [{ match: 2, uri: 'u' }]);
});

function kpEntry(fields, extra = {}) {
  const e = newEntry();
  e.fields = Object.entries(fields).map(([key, value]) => ({ key, value, protected: false }));
  return Object.assign(e, extra);
}

test('KeePass TOTP conventions are all recognised', () => {
  const map = (f) => new Map(Object.entries(f).map(([k, v]) => [k, { key: k, value: v }]));
  const uri = extractKeePassTotp(map({ otp: 'otpauth://totp/X?secret=JBSWY3DPEHPK3PXP' }), 'T', 'u');
  assert.deepEqual(uri, { totp: 'otpauth://totp/X?secret=JBSWY3DPEHPK3PXP', consumed: ['otp'] });
  const keeotp = extractKeePassTotp(map({ otp: 'key=JBSWY3DPEHPK3PXP&size=8&step=60&otpHashMode=Sha256' }), 'Site', 'bob');
  assert.deepEqual(
    { ...parseOtpauth(keeotp.totp) },
    { type: 'totp', secret: 'JBSWY3DPEHPK3PXP', algorithm: 'SHA256', digits: 8, period: 60, issuer: 'Site', account: 'bob', encoder: 'rfc6238' },
  );
  const native = extractKeePassTotp(map({ 'TimeOtp-Secret-Base32': 'JBSWY3DPEHPK3PXP', 'TimeOtp-Length': '7', 'TimeOtp-Period': '45', 'TimeOtp-Algorithm': 'HMAC-SHA-512' }), 'S', '');
  const p = parseOtpauth(native.totp);
  assert.deepEqual([p.secret, p.digits, p.period, p.algorithm], ['JBSWY3DPEHPK3PXP', 7, 45, 'SHA512']);
  assert.deepEqual(native.consumed.sort(), ['TimeOtp-Algorithm', 'TimeOtp-Length', 'TimeOtp-Period', 'TimeOtp-Secret-Base32']);
  const hex = extractKeePassTotp(map({ 'TimeOtp-Secret-Hex': '48656c6c6f21deadbeef' }), 'S', '');
  assert.equal(parseOtpauth(hex.totp).secret, 'JBSWY3DPEHPK3PXP');
  const text = extractKeePassTotp(map({ 'TimeOtp-Secret': 'Hello!Þ' }), 'S', '');
  assert.ok(parseOtpauth(text.totp).secret);
  const legacy = extractKeePassTotp(map({ 'TOTP Seed': 'JBSWY3DPEHPK3PXP', 'TOTP Settings': '30;8' }), 'S', '');
  assert.equal(parseOtpauth(legacy.totp).digits, 8);
  const steam = extractKeePassTotp(map({ 'TOTP Seed': 'JBSWY3DPEHPK3PXP', 'TOTP Settings': '30;S' }), 'S', '');
  assert.equal(steam.totp, 'steam://JBSWY3DPEHPK3PXP');
  const bare = extractKeePassTotp(map({ otp: 'jbsw y3dp ehpk 3pxp' }), 'S', '');
  assert.equal(bare.totp, 'JBSWY3DPEHPK3PXP');
  assert.deepEqual(extractKeePassTotp(map({ otp: 'otpauth://totp/x?secret=!!' }), 'S', ''), { totp: null, consumed: [] });
});

test('KeePass tree specifics: recycle bin, history order, attachments, tags, protected → hidden', () => {
  const tree = newTree('DB');
  const bin = newGroup('Recycle Bin', 43);
  const grp = newGroup('');
  tree.root.groups.push(bin, grp);
  tree.meta.recycleBinUuid = bin.uuid;
  bin.entries.push(kpEntry({ Title: 'trashed' }));
  const e = kpEntry({ Title: 'E', Password: 'p3', URL: ' https://x.test ', KP2A_URL_1: 'https://y.test', Secret: 's' }, {
    tags: ['favorite', 'team'],
    binaries: [{ key: 'a.txt', ref: 0 }],
    times: newTimes('2020-01-01T00:00:00Z', '2020-03-01T00:00:00Z'),
  });
  e.fields.find((f) => f.key === 'Secret').protected = true;
  e.history = [
    kpEntry({ Password: 'p2' }, { times: newTimes('2020-01-01T00:00:00Z', '2020-02-01T00:00:00Z') }),
    kpEntry({ Password: 'p1' }, { times: newTimes('2020-01-01T00:00:00Z', '2020-01-01T00:00:00Z') }),
    kpEntry({ Password: 'p1' }, { times: newTimes('2020-01-01T00:00:00Z', '2020-01-15T00:00:00Z') }), // unchanged password
  ];
  tree.binaries = [{ data: utf8Encode('x'), protected: false }];
  grp.entries.push(e);
  const v = kdbxTreeToVault(tree);
  assert.deepEqual(v.folders.map((f) => f.name), ['Untitled']);
  assert.equal(v.items.length, 1);
  const it = v.items[0];
  assert.equal(it.folderPath, 'Untitled');
  assert.equal(it.favorite, true);
  assert.deepEqual(it.tags, ['team']);
  assert.deepEqual(it.login.uris, [{ uri: 'https://x.test' }, { uri: 'https://y.test' }]);
  assert.deepEqual(it.fields, [{ name: 'Secret', value: 's', hidden: true }]);
  assert.deepEqual(it.password_history, [
    { password: 'p2', changed_at: '2020-03-01T00:00:00Z' },
    { password: 'p1', changed_at: '2020-02-01T00:00:00Z' },
  ]);
  assert.equal(v.warnings.length, 1);
  assert.equal(kdbxTreeToVault(tree, { includeRecycleBin: true }).items.length, 2);
});

test('helpers: folder paths and format detection', () => {
  assert.equal(normalizeFolderPath(' a // b /c/ '), 'a/b/c');
  assert.equal(normalizeFolderPath(undefined), '');
  assert.equal(detectFormat('folder,favorite,type,name,notes\n'), 'bitwarden-csv');
  assert.equal(detectFormat('﻿{"items":[]}'), 'bitwarden-json');
  assert.equal(detectFormat('<html></html>'), null);
  assert.equal(detectFormat(new Uint8Array([1, 2, 3])), null);
});
