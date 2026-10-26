// Vault key identity, stale-key locking, KDF work-factor preservation and key rotation in the
// Passwords app session (VAULT-CRYPTO.md §2.1–§2.5), against the in-memory server.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createSession, LockedError, sealablePlain } from '../../assets/js/apps/vault/session.js';
import { blankItem } from '../../assets/js/apps/vault/model.js';
import {
  createVault, decryptFolder, decryptItem, importAesKey, unlockVault, verifyKeyCheck, keepIterations,
} from '../../assets/js/lib/vault/crypto.js';
import { createFakeServer } from './fake-server.mjs';

const MASTER = 'correct horse battery staple vault';

function login(name) {
  const it = blankItem('login');
  it.name = name;
  it.login.username = `${name.toLowerCase()}@example.com`;
  it.login.password = `pw-${name}`;
  return it;
}

async function opened(srv, password = MASTER) {
  const s = createSession({ api: srv.api });
  await s.load();
  if (s.status === 'uninitialized') await s.initialize(password);
  else await s.unlock(password);
  return s;
}

/** Remote reset + re-create on "another device" (directly against the server). */
async function resetAndRecreate(srv, password = 'brand new master password') {
  await srv.api.post('/vault/reset', { password: 'account password' });
  const { params, vaultKey } = await createVault(password);
  vaultKey.fill(0);
  await srv.api.post('/vault/init', params);
}

test('init stores a key_check that opens under the vault key, and key_id is tracked', async () => {
  const srv = createFakeServer();
  const s = await opened(srv);
  assert.ok(srv.state.keys.key_id);
  assert.equal(s.keyId, srv.state.keys.key_id);
  const raw = await unlockVault(MASTER, srv.state.keys);
  assert.equal(await verifyKeyCheck(raw, srv.state.keys.key_check), true);
  assert.equal(await verifyKeyCheck(new Uint8Array(32), srv.state.keys.key_check), false);
  assert.equal(await verifyKeyCheck(raw, null), false);
});

// Regression: after a remote reset + re-init, an unlocked tab kept sealing new items under the
// retired key and the server stored them; they were unreadable everywhere else.
test('a write with a retired key is refused and locks the session', async () => {
  const srv = createFakeServer();
  const s = await opened(srv);
  await s.saveItem({ plain: login('Before') });
  await resetAndRecreate(srv);
  const newKeyId = srv.state.keys.key_id;

  await assert.rejects(s.saveItem({ plain: login('Stale') }), (e) => e.status === 409 && e.code === 'vault_key_changed');
  assert.equal(s.unlocked, false);
  assert.equal(s.items.size, 0);
  assert.match(s.lockReason, /vault key was replaced/);
  assert.equal(srv.state.items.size, 0, 'nothing sealed under the old key reached the new vault');
  assert.equal(srv.state.keys.key_id, newKeyId);
  await assert.rejects(s.saveItem({ plain: login('Again') }), (e) => e instanceof LockedError);
});

test('every kind of write names the key: a stale tab cannot create, import, move, rename or delete', async () => {
  const ops = {
    createFolder: (s) => s.createFolder('Other'),
    renameFolder: (s, f) => s.renameFolder(f, 'Renamed'),
    importEntries: (s) => s.importEntries([{ plain: login('Imported'), folderPath: '' }]),
    moveItem: (s, _f, rec) => s.moveItem(rec, null),
    saveItem: (s, _f, rec) => s.saveItem({ plain: rec.plain, folderId: null, original: rec }),
    deleteItem: (s, _f, rec) => s.deleteItem(rec.id),
    deleteFolders: (s, f) => s.deleteFolders([f.id]),
    changeMasterPassword: (s) => s.changeMasterPassword(MASTER, 'another master password 1'),
  };
  for (const [name, op] of Object.entries(ops)) {
    const srv = createFakeServer();
    const s = await opened(srv);
    const f = await s.createFolder('Work');
    const rec = await s.saveItem({ plain: login('A'), folderId: f.id });
    // Re-created elsewhere with records whose ids happen to match (worst case for id-based writes).
    await resetAndRecreate(srv);
    const cur = await opened(srv, 'brand new master password');
    await cur.saveItem({ plain: login('Current') });
    const snapshot = JSON.stringify([...srv.state.items.values(), ...srv.state.folders.values(), srv.state.keys]);
    await assert.rejects(op(s, f, rec), (e) => e.status === 409, name);
    assert.equal(s.unlocked, false, `${name} must lock the stale session`);
    assert.equal(JSON.stringify([...srv.state.items.values(), ...srv.state.folders.values(), srv.state.keys]), snapshot, name);
  }
});

test('a 410 caused by a reset elsewhere re-checks the key and locks instead of resyncing', async () => {
  const srv = createFakeServer();
  const s = await opened(srv);
  await s.saveItem({ plain: login('A') });
  await s.sync();
  await resetAndRecreate(srv);
  const other = await opened(srv, 'brand new master password');
  await other.saveItem({ plain: login('New vault item') });

  await assert.rejects(s.sync(), (e) => e instanceof LockedError);
  assert.equal(s.unlocked, false);
  assert.equal(s.status, 'locked');
  assert.equal(s.items.size, 0);
  await assert.rejects(s.saveItem({ plain: login('B') }), (e) => e instanceof LockedError);
});

test('a sync answered for another key_id locks (no undecryptable records are merged)', async () => {
  const srv = createFakeServer();
  const s = await opened(srv);
  await s.saveItem({ plain: login('A') });
  // Same since stays valid, but the records now belong to another key generation.
  srv.state.keys.key_id = '00000000-0000-4000-8000-000000000000';
  await assert.rejects(s.sync(), (e) => e instanceof LockedError);
  assert.equal(s.unlocked, false);
});

test('load() locks when key_id changed even if the wrapped key looks the same', async () => {
  const srv = createFakeServer();
  const s = await opened(srv);
  assert.equal(await s.load(), 'unlocked');
  srv.state.keys.key_id = '11111111-1111-4111-8111-111111111111';
  assert.equal(await s.load(), 'locked');
});

// Regression: the web UI re-wrapped with the default 600 000 iterations, silently lowering a work
// factor chosen with another client.
test('changing the master password keeps a higher work factor', async () => {
  const srv = createFakeServer();
  const { params, vaultKey } = await createVault(MASTER, { iterations: 650000 });
  vaultKey.fill(0);
  await srv.api.post('/vault/init', params);
  const s = await opened(srv);
  await s.changeMasterPassword(MASTER, 'new master password 123');
  assert.equal(srv.state.keys.kdf_iterations, 650000);
  assert.equal(s.params.kdf_iterations, 650000);
  const again = await opened(srv, 'new master password 123');
  assert.equal(again.unlocked, true);
  assert.equal(keepIterations({ kdf_iterations: 600000 }), 600000);
  assert.equal(keepIterations({ kdf_iterations: 2_000_000 }), 2_000_000);
  assert.equal(keepIterations(null), 600000);
});

// Regression: a master-password change never rotated the vault key, so an old wrapped_key plus
// the old password opened every item stored later.
test('rotating the vault key cuts off an old wrapped key and every other holder of the old key', async () => {
  const srv = createFakeServer();
  const s = await opened(srv);
  const work = await s.createFolder('Work/Email');
  const a = await s.saveItem({ plain: login('A'), folderId: work.id });
  const b = await s.saveItem({ plain: login('B') });
  const gone = await s.saveItem({ plain: login('Gone') });
  await s.deleteItem(gone.id);
  const snapshot = structuredClone(srv.state.keys); // the attacker's copy of GET /vault
  const otherDevice = await opened(srv); // still holds the old key afterwards
  const oldKeyId = s.keyId;

  await assert.rejects(s.rotateVaultKey('wrong password', 'new master password 123'), (e) => e.code === 'bad_password');
  const stages = [];
  await s.rotateVaultKey(MASTER, 'new master password 123', { onStage: (x) => stages.push(x) });
  assert.deepEqual(stages, ['verify', 'encrypt', 'save', 'done']);
  assert.equal(s.unlocked, true);
  assert.notEqual(s.keyId, oldKeyId);
  assert.equal(s.keyId, srv.state.keys.key_id);
  assert.deepEqual([...s.items.values()].map((r) => r.view.name).sort(), ['A', 'B']);
  assert.equal(s.folderPath(s.items.get(a.id).folder_id), 'Work/Email');
  assert.equal(s.items.get(a.id).plain.seq, a.plain.seq + 1);

  // The old wrapped key + old password still yields the OLD key, which opens nothing stored now.
  const oldKey = await importAesKey(await unlockVault(MASTER, snapshot));
  for (const r of srv.state.items.values()) {
    if (r.deleted) continue;
    await assert.rejects(decryptItem(oldKey, r.id, r.data), (e) => e.code === 'decrypt_failed');
  }
  await assert.rejects(decryptFolder(oldKey, work.id, srv.state.folders.get(work.id).name), (e) => e.code === 'decrypt_failed');
  await assert.rejects(unlockVault(MASTER, srv.state.keys), (e) => e.code === 'bad_password');
  const raw = await unlockVault('new master password 123', srv.state.keys);
  assert.equal(await verifyKeyCheck(raw, srv.state.keys.key_check), true);
  assert.equal(await verifyKeyCheck(await unlockVault(MASTER, snapshot), srv.state.keys.key_check), false);

  // A later item is out of reach of the old key too, and the other device cannot add items
  // under the old key: it is refused and locks.
  const later = await s.saveItem({ plain: login('After rotation') });
  await assert.rejects(decryptItem(oldKey, later.id, srv.state.items.get(later.id).data), (e) => e.code === 'decrypt_failed');
  await assert.rejects(otherDevice.saveItem({ plain: login('Stale') }), (e) => e.code === 'vault_key_changed');
  assert.equal(otherDevice.unlocked, false);
  const fresh = await opened(srv, 'new master password 123');
  assert.equal(fresh.items.size, 3);
  assert.ok([...fresh.items.values()].every((r) => r.plain && !r.error));
  void b;
});

test('rotation keeps the master password by default, keeps the work factor and refuses undecryptable vaults', async () => {
  const srv = createFakeServer();
  const { params, vaultKey } = await createVault(MASTER, { iterations: 700000 });
  vaultKey.fill(0);
  await srv.api.post('/vault/init', params);
  const s = await opened(srv);
  const x = await s.saveItem({ plain: login('X') });
  await s.rotateVaultKey(MASTER);
  assert.equal(srv.state.keys.kdf_iterations, 700000);
  assert.equal((await opened(srv)).items.get(x.id).view.name, 'X');

  // A record that cannot be decrypted would be lost by a rotation: refused, nothing changed.
  const y = await s.saveItem({ plain: login('Y') });
  srv.state.items.get(y.id).data = srv.state.items.get(x.id).data; // swapped by a bad server
  s.dropPlaintext();
  const keyBefore = srv.state.keys.key_id;
  await assert.rejects(s.rotateVaultKey(MASTER), (e) => e.code === 'undecryptable');
  assert.equal(srv.state.keys.key_id, keyBefore);
});

test('a rotation racing with another device is refused atomically', async () => {
  const srv = createFakeServer();
  const s = await opened(srv);
  await s.saveItem({ plain: login('X') });
  const other = await opened(srv);
  const realPost = srv.api.post;
  srv.api.post = async (p, b) => {
    if (p === '/vault/rekey') await other.saveItem({ plain: login('Sneaked in') });
    return realPost(p, b);
  };
  const keyBefore = srv.state.keys.key_id;
  await assert.rejects(s.rotateVaultKey(MASTER), (e) => e.status === 409);
  srv.api.post = realPost;
  assert.equal(srv.state.keys.key_id, keyBefore);
  assert.equal(s.unlocked, true); // nothing was rotated: the session keeps working
  await s.sync();
  await s.rotateVaultKey(MASTER);
  assert.equal((await opened(srv)).items.size, 2);
});

test('unlocking a vault created before key checks stores one', async () => {
  const srv = createFakeServer();
  const { params, vaultKey } = await createVault(MASTER);
  vaultKey.fill(0);
  delete params.key_check; // an older client
  await srv.api.post('/vault/init', params);
  assert.equal(srv.state.keys.key_check, null);
  await opened(srv);
  const raw = await unlockVault(MASTER, srv.state.keys);
  assert.equal(await verifyKeyCheck(raw, srv.state.keys.key_check), true);
});

test('items carry an authenticated seq and folder_id; exports and imports do not leak them', async () => {
  const srv = createFakeServer();
  const s = await opened(srv);
  const f = await s.createFolder('F');
  const r1 = await s.saveItem({ plain: login('A'), folderId: f.id });
  assert.equal(r1.plain.seq, 1);
  assert.equal(r1.plain.folder_id, f.id);
  const k = await importAesKey(await unlockVault(MASTER, srv.state.keys));
  assert.equal((await decryptItem(k, r1.id, srv.state.items.get(r1.id).data)).seq, 1);
  const edited = structuredClone(r1.plain);
  edited.notes = 'changed';
  const r2 = await s.saveItem({ plain: edited, folderId: null, original: r1 });
  assert.equal(r2.plain.seq, 2);
  assert.equal(r2.plain.folder_id, null);
  const stored = await decryptItem(k, r1.id, srv.state.items.get(r1.id).data);
  assert.equal(stored.seq, 2);
  assert.equal(stored.folder_id, null);

  // Clones and imports start their own counter.
  const copy = await s.saveItem({ plain: structuredClone(r2.plain) });
  assert.equal(copy.plain.seq, 1);
  await s.importEntries([{ plain: { ...login('Imp'), seq: 99, folder_id: 'bogus' }, folderPath: 'F' }]);
  const imp = [...s.items.values()].find((r) => r.view.name === 'Imp');
  assert.equal(imp.plain.seq, 1);
  assert.equal(imp.plain.folder_id, f.id);

  const out = s.exportData();
  assert.ok(out.items.every((it) => !('seq' in it) && !('folder_id' in it)));
  assert.deepEqual(sealablePlain({ seq: 4 }, null, { seq: 7 }).seq, 8);
  assert.deepEqual(sealablePlain({ seq: -1 }, 'x', {}).seq, 1);
});
