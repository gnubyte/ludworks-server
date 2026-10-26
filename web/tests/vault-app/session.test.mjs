// Session logic of the Passwords app against an in-memory server, with the real vault crypto.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { createSession, ConflictError, LockedError } from '../../assets/js/apps/vault/session.js';
import { blankItem } from '../../assets/js/apps/vault/model.js';
import { decryptItem, importAesKey, unlockVault, isValidId } from '../../assets/js/lib/vault/crypto.js';
import { createFakeServer } from './fake-server.mjs';

const MASTER = 'correct horse battery staple vault';

function login(name, extra = {}) {
  const it = blankItem('login');
  it.name = name;
  it.login.username = `${name.toLowerCase()}@example.com`;
  it.login.password = `pw-${name}`;
  Object.assign(it, extra);
  return it;
}

test('initialise, unlock on a second device, CRUD, conflicts and delta sync', async () => {
  const srv = createFakeServer();
  const a = createSession({ api: srv.api });
  assert.equal(await a.load(), 'uninitialized');
  await a.initialize(MASTER);
  assert.equal(a.status, 'unlocked');
  assert.equal(srv.state.keys.kdf_iterations, 600000);
  assert.ok(srv.state.keys.wrapped_key.startsWith('v1.'));

  // Nothing secret reaches the server in plaintext.
  const work = await a.createFolder(' Work / Email ');
  assert.equal(work.path, 'Work/Email');
  const rec = await a.saveItem({ plain: login('GitHub', { custom_future_key: { keep: true } }), folderId: work.id });
  assert.ok(isValidId(rec.id));
  const stored = srv.state.items.get(rec.id);
  assert.ok(!stored.data.includes('GitHub') && !stored.data.includes('pw-GitHub'));
  assert.ok(!srv.state.folders.get(work.id).name.includes('Work'));

  // Second device unlocks with the master password and sees everything.
  const b = createSession({ api: srv.api });
  assert.equal(await b.load(), 'locked');
  await assert.rejects(b.unlock('wrong master password'), (e) => e.code === 'bad_password');
  await b.unlock(MASTER);
  const brec = b.items.get(rec.id);
  assert.equal(brec.view.name, 'GitHub');
  assert.deepEqual(brec.plain.custom_future_key, { keep: true });
  assert.equal(b.folderPath(brec.folder_id), 'Work/Email');

  // b edits; a's stale edit conflicts and gets the decrypted server copy.
  const edited = structuredClone(brec.plain);
  edited.login.password = 'new-from-b';
  await b.saveItem({ plain: edited, folderId: brec.folder_id, original: brec });
  const stale = structuredClone(rec.plain);
  stale.notes = 'from a';
  await assert.rejects(a.saveItem({ plain: stale, folderId: rec.folder_id, original: rec }), (e) => {
    assert.ok(e instanceof ConflictError);
    assert.equal(e.current.plain.login.password, 'new-from-b');
    assert.equal(e.current.revision, 2);
    return true;
  });
  // Overwrite with the server copy as base.
  const fixed = await a.saveItem({ plain: stale, folderId: rec.folder_id, original: a.items.get(rec.id) });
  assert.equal(fixed.revision, 3);

  // Delta sync on b: update + deletion + folder deletion (un-files items).
  const other = await a.saveItem({ plain: login('Other'), folderId: null });
  await b.sync();
  assert.equal(b.items.get(rec.id).plain.notes, 'from a');
  assert.ok(b.items.has(other.id));
  await a.deleteItem(other.id);
  await a.deleteFolders([work.id]);
  assert.equal(a.items.get(rec.id).folder_id, null);
  await b.sync();
  assert.ok(!b.items.has(other.id));
  assert.equal(b.folders.size, 0);
  assert.equal(b.items.get(rec.id).folder_id, null);
  assert.equal(b.items.get(rec.id).revision, 4);

  // Moving re-seals the item so its authenticated folder_id (and seq) follow the move.
  const f2 = await a.createFolder('Personal');
  const before = a.items.get(rec.id);
  const moved = await a.moveItem(before, f2.id);
  assert.equal(moved.folder_id, f2.id);
  assert.equal(srv.state.items.get(rec.id).data, a.items.get(rec.id).data);
  assert.notEqual(moved.data, before.data);
  assert.equal(moved.plain.folder_id, f2.id);
  assert.equal(moved.plain.seq, before.plain.seq + 1);
});

test('stale since (410) falls back to a full sync; lock and dropPlaintext wipe state', async () => {
  const srv = createFakeServer();
  const s = createSession({ api: srv.api });
  await s.load();
  await s.initialize(MASTER);
  await s.saveItem({ plain: login('A') });
  await s.sync();
  // Simulate a server-side purge that moved the horizon past our sync point.
  srv.state.horizon = srv.state.clock + 1;
  srv.state.clock += 5;
  await s.sync();
  assert.equal(s.items.size, 1);

  s.dropPlaintext();
  assert.equal(s.items.size, 0);
  assert.equal(s.unlocked, true);
  await s.sync();
  assert.equal(s.items.size, 1);

  const pending = s.saveItem({ plain: login('B') });
  s.lock();
  await assert.rejects(pending, (e) => e instanceof LockedError);
  assert.equal(s.status, 'locked');
  assert.equal(s.items.size, 0);
  assert.equal(s.unlocked, false);
  await assert.rejects(s.sync(), (e) => e instanceof LockedError);
  assert.throws(() => s.exportData(), LockedError);
});

test('records swapped by a malicious server are detected, not decrypted', async () => {
  const srv = createFakeServer();
  const s = createSession({ api: srv.api });
  await s.load();
  await s.initialize(MASTER);
  const x = await s.saveItem({ plain: login('X') });
  const y = await s.saveItem({ plain: login('Y') });
  const dx = srv.state.items.get(x.id).data;
  srv.state.items.get(x.id).data = srv.state.items.get(y.id).data;
  srv.state.items.get(y.id).data = dx;
  s.dropPlaintext();
  await s.sync();
  assert.equal(s.items.get(x.id).view, null);
  assert.equal(s.items.get(x.id).error, 'decrypt_failed');
  assert.equal(s.items.get(y.id).error, 'decrypt_failed');
});

test('import creates folders, encrypts and uploads in batches of at most 1000', async () => {
  const srv = createFakeServer();
  const s = createSession({ api: srv.api });
  await s.load();
  await s.initialize(MASTER);
  await s.createFolder('Existing');
  const entries = Array.from({ length: 2300 }, (_, i) => ({
    plain: login(`Site ${i}`),
    folderPath: i % 3 === 0 ? 'Existing' : i % 3 === 1 ? 'Imported/Sub' : '',
  }));
  const phases = new Set();
  const res = await s.importEntries(entries, { onProgress: (p) => phases.add(p.phase) });
  assert.equal(res.created, 2300);
  assert.equal(res.foldersCreated, 1);
  assert.deepEqual([...phases].sort(), ['encrypt', 'folders', 'upload']);
  assert.deepEqual(srv.state.calls.map((c) => c.batch), [1000, 1000, 300]);
  assert.equal(s.items.size, 2300);
  const paths = new Set([...s.items.values()].map((r) => s.folderPath(r.folder_id)));
  assert.deepEqual([...paths].sort(), ['', 'Existing', 'Imported/Sub']);

  const out = s.exportData();
  assert.equal(out.items.length, 2300);
  assert.equal(out.folders.length, 2);
  assert.ok(out.items.every((it) => typeof it.folderPath === 'string' && it.type === 'login'));
});

test('change master password re-wraps the same key; reset needs the account password', async () => {
  const srv = createFakeServer({ accountPassword: 'my login pw' });
  const s = createSession({ api: srv.api });
  await s.load();
  await s.initialize(MASTER);
  const rec = await s.saveItem({ plain: login('Keep') });
  const oldWrapped = srv.state.keys.wrapped_key;

  await assert.rejects(s.changeMasterPassword('not the master', 'new master password 123'), (e) => e.code === 'bad_password');
  assert.equal(await s.verifyMasterPassword(MASTER), true);
  assert.equal(await s.verifyMasterPassword('nope'), false);
  await s.changeMasterPassword(MASTER, 'new master password 123');
  assert.notEqual(srv.state.keys.wrapped_key, oldWrapped);

  // Old password no longer unlocks; the new one yields the same vault key (items still decrypt).
  await assert.rejects(unlockVault(MASTER, srv.state.keys), (e) => e.code === 'bad_password');
  const raw = await unlockVault('new master password 123', srv.state.keys);
  const k = await importAesKey(raw);
  assert.equal((await decryptItem(k, rec.id, srv.state.items.get(rec.id).data)).name, 'Keep');

  // Our own change does not lock us; a change elsewhere does on the next load().
  assert.equal(await s.load(), 'unlocked');
  srv.state.keys.wrapped_key = oldWrapped;
  assert.equal(await s.load(), 'locked');

  await assert.rejects(s.reset('wrong'), (e) => e.code === 'invalid_credentials');
  await s.reset('my login pw');
  assert.equal(s.status, 'uninitialized');
  assert.equal(srv.state.items.size, 0);
});
