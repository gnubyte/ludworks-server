import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  blankItem, view, subtitle, haystack, matchesQuery, sortEntries, buildFolderTree, foldersUnder, renamePath, folderPathError,
  normalizePath, inFolder, detectCardBrand, luhnValid, formatCardNumber, totpError, finalizeItem, dupKey, chunkBatches,
  securityReport, safeHref, hostOf, passwordSetAt, MAX_HISTORY, avatarName,
} from '../../assets/js/apps/vault/model.js';
import { planImport, formatFromName } from '../../assets/js/apps/vault/views/importer.js';
import { exportFileName, KDBX_EXPORT_KDF } from '../../assets/js/apps/vault/views/exporter.js';
import { unlockDelay } from '../../assets/js/apps/vault/views/unlock.js';
import { generateFrom } from '../../assets/js/apps/vault/views/generator.js';

test('blank items carry only their own sub-object', () => {
  assert.deepEqual(Object.keys(blankItem('login').login), ['username', 'password', 'uris', 'totp']);
  assert.equal(blankItem('note').login, undefined);
  assert.equal(Object.keys(blankItem('card').card).length, 6);
  assert.equal(Object.keys(blankItem('identity').identity).length, 17);
  assert.ok(!('card' in blankItem('login')));
});

test('view() tolerates malformed decrypted data', () => {
  const v = view({ type: 'weird', name: 42, login: 'x', fields: [null, { name: 'a', value: { x: 1 }, hidden: 'yes' }], password_history: [{ password: '' }, 5] });
  assert.equal(v.type, 'note');
  assert.equal(v.name, '42');
  assert.deepEqual(v.fields, [{ name: 'a', value: '', hidden: false }]);
  assert.deepEqual(v.history, []);
  assert.equal(view(null).name, '(untitled)');
  assert.deepEqual(view({ type: 'login', login: { uris: [{ uri: ' ' }, { uri: 'example.com', match: 'host' }] } }).login.uris, [{ uri: 'example.com', match: 'host' }]);
});

test('subtitles and search never expose secrets', () => {
  const l = view({ type: 'login', name: 'GitHub', login: { username: 'me', password: 'hunter2', uris: [{ uri: 'https://github.com/login' }] }, fields: [{ name: 'PIN', value: '1234', hidden: true }, { name: 'Team', value: 'core' }] });
  assert.equal(subtitle(l), 'me');
  const hay = haystack(l, 'Work');
  assert.ok(hay.includes('github') && hay.includes('work') && hay.includes('core'));
  assert.ok(!hay.includes('hunter2') && !hay.includes('1234'));
  assert.ok(matchesQuery(hay, 'GIT  core'));
  assert.ok(!matchesQuery(hay, 'git gitlab'));
  const c = view({ type: 'card', name: 'Visa', card: { number: '4111 1111 1111 1111', code: '123' } });
  assert.equal(subtitle(c), 'Visa •••• 1111');
  assert.ok(!haystack(c).includes('4111'));
  assert.equal(subtitle(view({ type: 'identity', identity: { first_name: 'Ada', last_name: 'Lovelace' } })), 'Ada Lovelace');
  assert.equal(subtitle(view({ type: 'note', notes: '\n\n  first line\nsecond' })), '  first line');
  assert.equal(subtitle(view({ type: 'login', login: { uris: [{ uri: 'example.org/x' }] } })), 'example.org');
});

test('only http(s) links are opened', () => {
  assert.equal(safeHref('javascript:alert(1)'), null);
  assert.equal(safeHref('data:text/html,x'), null);
  assert.equal(safeHref('example.com'), 'https://example.com/');
  assert.equal(safeHref('http://a.test/p?q'), 'http://a.test/p?q');
  assert.equal(hostOf('ftp://files.example.com'), '');
});

test('sorting by name, reverse and time', () => {
  const e = (name, updated, created) => ({ view: view({ type: 'note', name }), updated, created });
  const list = [e('b', '2026-01-02', '2026-01-01'), e('A10', '2026-01-03', '2025-01-01'), e('a2', '2026-01-01', '2026-02-01')];
  assert.deepEqual(sortEntries([...list], 'name').map((x) => x.view.name), ['a2', 'A10', 'b']);
  assert.deepEqual(sortEntries([...list], 'name-desc').map((x) => x.view.name), ['b', 'A10', 'a2']);
  assert.deepEqual(sortEntries([...list], 'updated').map((x) => x.view.name), ['A10', 'b', 'a2']);
  assert.deepEqual(sortEntries([...list], 'created').map((x) => x.view.name), ['a2', 'b', 'A10']);
});

test('folder tree with virtual parents, rename and validation', () => {
  const folders = [{ id: '1', path: 'Work/Email' }, { id: '2', path: 'Home' }, { id: '3', path: 'Work/Code/Go' }, { id: '4', path: 'Work' }];
  const t = buildFolderTree(folders);
  assert.deepEqual(t.children.map((c) => c.name), ['Home', 'Work']);
  const work = t.children[1];
  assert.equal(work.id, '4');
  assert.deepEqual(work.children.map((c) => [c.name, c.id]), [['Code', null], ['Email', '1']]);
  assert.equal(work.children[0].children[0].depth, 2);
  assert.deepEqual(foldersUnder(folders, 'Work').map((f) => f.id).sort(), ['1', '3', '4']);
  assert.equal(renamePath('Work/Email', 'Work', 'Job'), 'Job/Email');
  assert.ok(inFolder('Work/Email', 'Work') && !inFolder('Workshop', 'Work'));
  assert.equal(normalizePath(' a // b /'), 'a/b');
  assert.equal(folderPathError('', []), 'Enter a folder name.');
  assert.match(folderPathError('work', ['Work']), /already exists/);
  assert.match(folderPathError('a\u0001b', []), /control/);
  assert.equal(folderPathError('New', ['Work']), null);
});

test('cards: brand detection, Luhn and formatting', () => {
  assert.equal(detectCardBrand('4111111111111111'), 'Visa');
  assert.equal(detectCardBrand('5500 0000 0000 0004'), 'Mastercard');
  assert.equal(detectCardBrand('2223000048400011'), 'Mastercard');
  assert.equal(detectCardBrand('378282246310005'), 'American Express');
  assert.equal(detectCardBrand('6011111111111117'), 'Discover');
  assert.equal(detectCardBrand('3530111333300000'), 'JCB');
  assert.ok(luhnValid('4111 1111 1111 1111'));
  assert.ok(!luhnValid('4111 1111 1111 1112'));
  assert.equal(formatCardNumber('4111111111111111'), '4111 1111 1111 1111');
  assert.equal(formatCardNumber('378282246310005'), '3782 822463 10005');
});

test('TOTP secrets are validated', () => {
  assert.equal(totpError(''), null);
  assert.equal(totpError('JBSWY3DPEHPK3PXP'), null);
  assert.equal(totpError('otpauth://totp/GitHub:me?secret=JBSWY3DPEHPK3PXP&issuer=GitHub'), null);
  assert.ok(totpError('otpauth://hotp/x?secret=JBSWY3DPEHPK3PXP&counter=1'));
  assert.ok(totpError('not base32 !'));
  assert.ok(totpError('AB'));
});

test('finalizeItem keeps unknown keys and records password history', () => {
  const t0 = new Date('2026-01-01T00:00:00Z');
  const orig = { ...blankItem('login', t0), name: 'X', future: { a: 1 }, password_history: [{ password: 'p0', changed_at: '2025-01-01T00:00:00Z' }] };
  orig.login.password = 'p1';
  const ed = structuredClone(orig);
  ed.login.password = 'p2';
  const out = finalizeItem(orig, ed, new Date('2026-02-01T00:00:00Z'));
  assert.deepEqual(out.future, { a: 1 });
  assert.equal(out.created_at, orig.created_at);
  assert.equal(out.updated_at, '2026-02-01T00:00:00.000Z');
  assert.deepEqual(out.password_history.map((h) => h.password), ['p1', 'p0']);
  // unchanged password: no history entry
  assert.equal(finalizeItem(orig, structuredClone(orig)).password_history.length, 1);
  // capped
  let cur = orig;
  for (let i = 0; i < 15; i++) {
    const next = structuredClone(cur);
    next.login.password = `x${i}`;
    cur = finalizeItem(cur, next);
  }
  assert.equal(cur.password_history.length, MAX_HISTORY);
  assert.equal(cur.password_history[0].password, 'x13');
  // new item
  const n = finalizeItem(null, blankItem('note', t0), new Date('2026-03-01T00:00:00Z'));
  assert.equal(n.created_at, '2026-01-01T00:00:00.000Z');
});

test('duplicate keys and batching', () => {
  const a = { type: 'login', name: ' GitHub ', login: { username: 'Me', uris: [{ uri: 'https://github.com/' }] } };
  const b = { type: 'login', name: 'github', login: { username: 'me', uris: [{ uri: 'https://GitHub.com' }] } };
  assert.equal(dupKey(a), dupKey(b));
  assert.notEqual(dupKey(a), dupKey({ ...b, login: { ...b.login, username: 'you' } }));
  const rows = Array.from({ length: 2500 }, (_, i) => ({ id: `${i}`.padStart(36, '0'), folder_id: null, data: 'x'.repeat(100) }));
  assert.deepEqual(chunkBatches(rows).map((c) => c.length), [1000, 1000, 500]);
  const big = Array.from({ length: 10 }, (_, i) => ({ id: `${i}`.padStart(36, '0'), folder_id: null, data: 'x'.repeat(1 << 20) }));
  const chunks = chunkBatches(big, { maxBytes: 3 << 20 });
  assert.ok(chunks.every((c) => c.length <= 2));
  assert.equal(chunks.flat().length, 10);
});

test('security report', () => {
  const now = Date.parse('2026-10-26T00:00:00Z');
  const mk = (id, extra) => ({ id, view: view({ type: 'login', name: id, created_at: '2026-10-01T00:00:00Z', ...extra }) });
  const entries = [
    mk('weak', { login: { password: 'password1', uris: [{ uri: 'https://github.com' }] } }),
    mk('r1', { login: { password: 'Tr0ub4dour&3-unique-xyz!', uris: [{ uri: 'http://shop.example.com' }], totp: 'JBSWY3DPEHPK3PXP' } }),
    mk('r2', { login: { password: 'Tr0ub4dour&3-unique-xyz!', uris: [{ uri: 'http://192.168.1.1' }] } }),
    mk('old', { created_at: '2023-01-01T00:00:00Z', login: { password: 'k8#Qz!m2$Lp9@wX4&vB7', uris: [{ uri: 'https://accounts.google.com' }] } }),
    mk('fresh', { created_at: '2020-01-01T00:00:00Z', password_history: [{ password: 'x', changed_at: '2026-09-01T00:00:00Z' }], login: { password: 'Zq3$wP8!nM5&kT2@rY6^' } }),
    { id: 'note', view: view({ type: 'note', name: 'n' }) },
  ];
  const r = securityReport(entries, { now });
  assert.deepEqual(r.weak.map((x) => x.id), ['weak']);
  assert.deepEqual(r.reused, [{ count: 2, ids: ['r1', 'r2'] }]);
  assert.deepEqual(r.old.map((x) => x.id), ['old']);
  assert.deepEqual(r.insecure.map((x) => x.id), ['r1']);
  assert.deepEqual(r.missing2fa.map((x) => [x.id, x.domain]).sort(), [['old', 'google.com'], ['weak', 'github.com']]);
  assert.equal(r.logins, 5);
  assert.equal(r.checked, 5);
  assert.ok(r.score < 100 && r.score >= 0);
  assert.equal(passwordSetAt(entries[4].view), Date.parse('2026-09-01T00:00:00Z'));
  assert.equal(securityReport([]).score, 100);
});

test('import plan: duplicates and folder modes', () => {
  const data = {
    items: [
      { folderPath: 'A/B', type: 'login', name: 'GitHub', login: { username: 'me', uris: [{ uri: 'https://github.com' }] } },
      { folderPath: '', type: 'login', name: 'GitHub', login: { username: 'me', uris: [{ uri: 'https://github.com/' }] } },
      { folderPath: 'A', type: 'note', name: 'Note' },
    ],
  };
  const existing = new Set([dupKey({ type: 'note', name: 'note' })]);
  const p = planImport(data, { existingKeys: existing, folderMode: 'keep', singleFolder: '', duplicates: 'skip' });
  assert.equal(p.dupCount, 2);
  assert.deepEqual(p.send.map((s) => [s.plain.name, s.folderPath]), [['GitHub', 'A/B']]);
  assert.ok(!('folderPath' in p.send[0].plain));
  const k = planImport(data, { existingKeys: existing, folderMode: 'single', singleFolder: ' Imported ', duplicates: 'keep', excluded: new Set([2]) });
  assert.deepEqual(k.send.map((s) => s.folderPath), ['Imported', 'Imported']);
  const n = planImport(data, { existingKeys: new Set(), folderMode: 'none', singleFolder: '', duplicates: 'keep' });
  assert.ok(n.send.every((s) => s.folderPath === ''));
  assert.equal(formatFromName('db.KDBX'), 'kdbx');
  assert.equal(formatFromName('export.csv'), 'bitwarden-csv');
  assert.equal(formatFromName('x.bin'), 'auto');
});

test('export defaults, unlock cool-down and generator options', () => {
  assert.deepEqual({ ...KDBX_EXPORT_KDF }, { type: 'argon2d', memory: 64 * 1024 * 1024, iterations: 2, parallelism: 2 });
  assert.equal(exportFileName('kdbx', new Date('2026-10-26T10:00:00Z')), 'ludworks-vault-20261026.kdbx');
  assert.equal(exportFileName('bitwarden-csv', new Date('2026-01-02T00:00:00Z')), 'ludworks-vault-20260102.csv');
  assert.deepEqual([0, 1, 2, 3, 4, 5, 10].map(unlockDelay), [0, 0, 0, 1, 2, 4, 30]);
  const pw = generateFrom({ mode: 'password', length: 32, upper: true, lower: false, digits: true, symbols: false, excludeAmbiguous: true });
  assert.equal(pw.length, 32);
  assert.match(pw, /^[A-HJ-NP-Z2-9]+$/);
  const pp = generateFrom({ mode: 'passphrase', words: 5, separator: '.', capitalize: true, includeNumber: true });
  assert.equal(pp.split('.').length, 5);
  assert.match(pp, /\d/);
  assert.ok(pp.split('.').every((w) => /^[A-Z]/.test(w)));
});

// Regression (e2e): an imported "GitHub (kdbx)" showed the avatar initials "G(".
test('avatar names drop punctuation so initials are letters', async () => {
  const { initials } = await import('../../assets/js/core/format.js');
  assert.equal(initials(avatarName('GitHub (kdbx)')), 'GK');
  assert.equal(initials(avatarName('[Work] Mail')), 'WM');
  assert.equal(initials(avatarName('Ünïcode ✓ Bank')), 'ÜB');
  assert.equal(initials(avatarName('Visa')), 'VI');
  assert.equal(avatarName('((('), '?');
  assert.equal(avatarName(null), '?');
  assert.equal(avatarName({ evil: 1 }), '?');
});
