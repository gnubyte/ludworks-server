// The unlocked vault, held only in this module's memory.
//
// Key handling (VAULT-CRYPTO.md §1, §7): the master password is used to unwrap the vault key,
// which is immediately imported as a NON-EXTRACTABLE AES-GCM CryptoKey; the raw bytes are
// zeroed. Nothing is ever written to localStorage/sessionStorage/IndexedDB. Operations that
// need the raw key (changing the master password) re-derive it from the current master
// password. lock() drops the key and every decrypted record; dropPlaintext() drops only the
// decrypted records (used when the app unmounts while the key is still within its idle window).
// JavaScript cannot guarantee that strings are wiped from memory: dropping references is best
// effort.
//
// Key freshness (§2.2): the key in memory belongs to one vault key generation (key_id). Every
// write names it, so the server refuses records sealed under a key that was retired meanwhile
// (vault reset + re-init, or key rotation, on another device) with 409 vault_key_changed; every
// sync compares the key_id of the records it receives, and a 410 or a write conflict re-reads
// GET /vault. Whenever the key is no longer the current one the session locks.
//
// Sync: GET /vault/sync with the previous server_time; 410 → full sync. Records are decrypted
// with their id bound in the AAD, so a server swapping ciphertexts between records is detected
// (such records are kept as "undecryptable" and never re-saved).

import {
  createVault, unlockVault, rewrapVaultKey, importAesKey, encryptItem, decryptItem, encryptFolder, decryptFolder,
  createKeyCheck, verifyKeyCheck, keepIterations, uuid, isValidId, VaultError,
} from '../../lib/vault/crypto.js';
import { chunkBatches, normalizePath, view } from './model.js';

/** Thrown when an operation is overtaken by lock(). */
export class LockedError extends Error {
  constructor() {
    super('The vault was locked');
    this.name = 'LockedError';
    this.code = 'locked';
  }
}

/** Thrown by saveItem/renameFolder on a 409 with details.current: `current` is the decrypted server copy. */
export class ConflictError extends Error {
  constructor(current) {
    super('This item was changed on another device.');
    this.name = 'ConflictError';
    this.code = 'conflict';
    this.current = current;
  }
}

const DECRYPT_CHUNK = 256;
export const KEY_CHANGED_REASON = 'Locked because the vault key was replaced on another device (vault reset or key rotation). Unlock again to continue.';

const counter = (v) => (Number.isSafeInteger(v) && v >= 0 ? v : 0);

/**
 * Item plaintext as written by this client (§3, §2.5): `seq` is the authenticated per-item edit counter
 * (one more than the version it replaces; 1 for a new item) and `folder_id` the authenticated copy of the
 * record's folder, so clients that keep state can detect a server replaying an older version or moving
 * the item. The caller's object is not modified.
 * @param {object} plain
 * @param {string|null} folderId
 * @param {object|null} base plaintext of the version being replaced (null for a new item)
 */
export function sealablePlain(plain, folderId, base) {
  const seq = base ? Math.max(counter(base.seq), counter(plain.seq)) + 1 : 1;
  return { ...plain, seq, folder_id: folderId ?? null };
}

/**
 * @param {{api: {get: Function, post: Function, put: Function, del: Function}, now?: () => Date}} deps
 */
export function createSession({ api, now = () => new Date() }) {
  let params = null; // GET /vault body (not secret)
  let unlockedWith = null; // wrapped_key the current key was unwrapped from
  let keyId = null; // key_id (vault key generation) of the current key
  let key = null; // non-extractable CryptoKey
  let serverTime = null;
  let status = 'idle'; // idle | uninitialized | locked | unlocked
  let lockReason = '';
  let gen = 0; // bumped by lock()/dropPlaintext(): async work from before must not repopulate state
  let synced = false;
  let syncing = null;
  let again = false;
  const items = new Map(); // id → record
  const folders = new Map(); // id → {id, revision, updated_at, name (sealed), path, plainName, error}
  const subs = new Set();

  const emit = (what) => {
    for (const fn of [...subs]) {
      try {
        fn(what);
      } catch (err) {
        console.error(err);
      }
    }
  };
  const setStatus = (s) => {
    if (status !== s) {
      status = s;
      emit('status');
    }
  };
  const guard = (g) => {
    if (g !== gen || !key) throw new LockedError();
  };
  const requireKey = () => {
    if (!key) throw new LockedError();
    return key;
  };
  /** Write body fields naming the key the record is sealed under. */
  const keyed = (body) => (keyId ? { ...body, key_id: keyId } : body);
  /** Query for writes without a body (DELETE). */
  const keyQuery = () => (keyId ? { query: { key_id: keyId } } : undefined);

  /**
   * After a failed write: a 409 without a server copy means the vault key was replaced (or the vault
   * deleted) elsewhere, or something else changed underneath; re-read GET /vault, which locks when
   * the key is no longer current. The original error is rethrown by the caller.
   */
  async function recheckAfter(err) {
    if (err?.status === 409 && !err?.details?.current) {
      if (err.code === 'vault_key_changed' && key) {
        api$.lock(KEY_CHANGED_REASON);
      }
      await api$.load().catch(() => {});
    }
  }

  async function decryptItemRecord(k, r) {
    const rec = {
      id: r.id, folder_id: r.folder_id ?? null, revision: r.revision, created_at: r.created_at,
      updated_at: r.updated_at, data: r.data, plain: null, view: null, error: null,
    };
    try {
      if (!isValidId(r.id)) throw new VaultError('malformed', 'invalid id');
      rec.plain = await decryptItem(k, r.id, r.data);
      rec.view = view(rec.plain);
    } catch (err) {
      rec.error = err?.code || 'decrypt_failed';
    }
    return rec;
  }

  async function decryptFolderRecord(k, f) {
    const rec = { id: f.id, revision: f.revision, updated_at: f.updated_at, name: f.name, path: '', plainName: null, error: null };
    try {
      if (!isValidId(f.id)) throw new VaultError('malformed', 'invalid id');
      rec.plainName = await decryptFolder(k, f.id, f.name);
      rec.path = normalizePath(rec.plainName) || '(unnamed)';
    } catch (err) {
      rec.error = err?.code || 'decrypt_failed';
    }
    return rec;
  }

  async function mapChunks(list, fn) {
    const out = [];
    for (let i = 0; i < list.length; i += DECRYPT_CHUNK) {
      out.push(...await Promise.all(list.slice(i, i + DECRYPT_CHUNK).map(fn)));
    }
    return out;
  }

  async function doSync() {
    const g = gen;
    const k = requireKey();
    let since = synced ? serverTime : null;
    let res;
    try {
      res = await api.get('/vault/sync', { query: since ? { since } : undefined });
    } catch (err) {
      if (!(since && err?.status === 410)) throw err;
      // 410 also follows a vault reset + re-init or a key rotation elsewhere: re-check the key
      // (load() locks when it changed) before trusting a full resync.
      await api$.load();
      guard(g);
      since = null;
      res = await api.get('/vault/sync');
    }
    guard(g);
    // Records sealed under another vault key are never merged: the key we hold is not current.
    if (res && 'key_id' in res && (res.key_id ?? null) !== keyId) {
      api$.lock(KEY_CHANGED_REASON);
      await api$.load().catch(() => {});
      throw new LockedError();
    }
    const fs = await mapChunks(Array.isArray(res.folders) ? res.folders : [], (f) => decryptFolderRecord(k, f));
    const its = await mapChunks(Array.isArray(res.items) ? res.items : [], (r) => decryptItemRecord(k, r));
    guard(g);
    if (!since) {
      items.clear();
      folders.clear();
    }
    for (const f of fs) folders.set(f.id, f);
    for (const r of its) items.set(r.id, r);
    for (const id of res.deleted_item_ids || []) items.delete(id);
    for (const id of res.deleted_folder_ids || []) folders.delete(id);
    serverTime = typeof res.server_time === 'string' ? res.server_time : null;
    synced = true;
    emit('data');
  }

  /**
   * Sync with the server. Calls made while a sync is running are coalesced into one more run
   * after it, so every caller sees changes made before its call.
   */
  function sync() {
    if (syncing) {
      again = true;
      return syncing;
    }
    syncing = (async () => {
      try {
        do {
          again = false;
          await doSync();
        } while (again);
      } finally {
        syncing = null;
      }
    })();
    return syncing;
  }

  function adopt(k, p) {
    key = k;
    unlockedWith = p.wrapped_key;
    keyId = p.key_id ?? null;
    lockReason = '';
    serverTime = null;
    synced = false;
    items.clear();
    folders.clear();
  }

  function itemRecordFromServer(r, plain) {
    return { id: r.id, folder_id: r.folder_id ?? null, revision: r.revision, created_at: r.created_at, updated_at: r.updated_at,
      data: r.data, plain, view: view(plain), error: null };
  }

  async function conflictFrom(err) {
    const cur = err?.details?.current;
    if (err?.status !== 409 || !cur || typeof cur !== 'object') return null;
    const k = requireKey();
    return 'data' in cur ? decryptItemRecord(k, cur) : decryptFolderRecord(k, cur);
  }

  /** Item PUT with conflict handling shared by saveItem and moveItem. */
  async function putItem(g, id, body) {
    try {
      return await api.put(`/vault/items/${id}`, keyed(body));
    } catch (err) {
      const cur = await conflictFrom(err);
      if (cur) {
        guard(g);
        items.set(cur.id, cur);
        emit('data');
        throw new ConflictError(cur);
      }
      if (err?.status === 404) {
        items.delete(id);
        emit('data');
      }
      await recheckAfter(err);
      throw err;
    }
  }

  /**
   * Store a key_check for a vault created before key checks existed (best effort; §2.2). Only called
   * right after the key was unwrapped from the current wrapped_key with the master password.
   */
  async function storeKeyCheck(check, forKeyId) {
    try {
      await api.put('/vault/key-check', { key_id: forKeyId, key_check: check });
      if (params?.key_id === forKeyId && !params.key_check) params = { ...params, key_check: check };
    } catch {
      /* another device stored one first, or the vault changed: harmless */
    }
  }

  const api$ = {
    get status() { return status; },
    get params() { return params; },
    get unlocked() { return status === 'unlocked' && !!key; },
    get synced() { return synced; },
    get serverTime() { return serverTime; },
    get keyId() { return keyId; },
    /** Why the session locked itself ('' when locked by the user or the idle timer). */
    get lockReason() { return lockReason; },
    get items() { return items; },
    get folders() { return folders; },

    /** Subscribe to 'status' / 'data' changes. @returns {() => void} */
    subscribe(fn) {
      subs.add(fn);
      return () => subs.delete(fn);
    },

    /**
     * GET /vault. Locks when the key in memory is no longer the vault's current key: the vault was
     * reset or re-created, its key rotated (key_id changed) or the master password changed elsewhere
     * (wrapped_key changed).
     */
    async load() {
      params = await api.get('/vault');
      if (!params?.initialized) {
        if (key) api$.lock(KEY_CHANGED_REASON);
        setStatus('uninitialized');
        return status;
      }
      if (key && (params.key_id ?? null) !== keyId) api$.lock(KEY_CHANGED_REASON);
      else if (key && params.wrapped_key !== unlockedWith) api$.lock('Locked because the master password was changed on another device.');
      setStatus(key ? 'unlocked' : 'locked');
      return status;
    },

    /**
     * Create the vault: new random vault key wrapped under the master password, POST /vault/init.
     * @param {string} password
     * @param {{onStage?: (stage: 'derive'|'save'|'done') => void}} [opts]
     */
    async initialize(password, { onStage } = {}) {
      onStage?.('derive');
      const { params: p, vaultKey } = await createVault(password);
      let k;
      try {
        k = await importAesKey(vaultKey);
      } finally {
        vaultKey.fill(0);
      }
      onStage?.('save');
      const st = await api.post('/vault/init', p);
      params = st;
      gen++;
      adopt(k, st);
      setStatus('unlocked');
      onStage?.('done');
      await sync();
      return status;
    },

    /** Unlock with the master password (bad password → VaultError code 'bad_password'). */
    async unlock(password) {
      if (!params?.initialized) throw new VaultError('invalid_argument', 'vault is not initialised');
      const p = params;
      const raw = await unlockVault(password, p);
      let k;
      let check = null;
      try {
        k = await importAesKey(raw);
        if (p.key_id && !p.key_check) {
          check = await createKeyCheck(raw);
        } else if (p.key_check && !(await verifyKeyCheck(raw, p.key_check))) {
          // The key unwrapped with the master password is authoritative; a key_check that does not
          // open under it was not written by a client holding this key.
          console.warn('vault: the stored key_check does not match the vault key');
        }
      } finally {
        raw.fill(0);
      }
      gen++;
      adopt(k, p);
      setStatus('unlocked');
      if (check) await storeKeyCheck(check, p.key_id);
      await sync();
      return status;
    },

    /** Drop the key and every decrypted record. */
    lock(reason = '') {
      gen++;
      key = null;
      unlockedWith = null;
      keyId = null;
      lockReason = reason;
      serverTime = null;
      synced = false;
      items.clear();
      folders.clear();
      setStatus(params?.initialized ? 'locked' : (params ? 'uninitialized' : 'idle'));
      emit('data');
    },

    /** Drop decrypted records but keep the key (app unmounted within the idle window). */
    dropPlaintext() {
      gen++;
      serverTime = null;
      synced = false;
      items.clear();
      folders.clear();
    },

    sync,

    /** Folder path of an item record ('' = none, or when the folder is unknown/undecryptable). */
    folderPath(folderId) {
      const f = folderId ? folders.get(folderId) : null;
      return f && !f.error ? f.path : '';
    },

    /**
     * Create or update an item. For updates pass the record being edited (its revision is the
     * base revision). A 409 with a server copy throws ConflictError(current record).
     * @param {{plain: object, folderId: string|null, original?: object|null}} a
     */
    async saveItem({ plain, folderId = null, original = null }) {
      const k = requireKey();
      const g = gen;
      const id = original ? original.id : uuid();
      const sealed = sealablePlain(plain, folderId, original ? (original.plain || {}) : null);
      const data = await encryptItem(k, id, sealed);
      guard(g);
      let r;
      if (original) {
        r = await putItem(g, id, { folder_id: folderId, data, base_revision: original.revision });
      } else {
        try {
          r = await api.post('/vault/items', keyed({ id, folder_id: folderId, data }));
        } catch (err) {
          await recheckAfter(err);
          throw err;
        }
      }
      guard(g);
      const rec = itemRecordFromServer(r, sealed);
      items.set(rec.id, rec);
      emit('data');
      return rec;
    },

    /**
     * Move an item to another folder. The item is re-sealed so its authenticated folder_id (and seq)
     * follow the move; undecryptable records cannot be moved.
     */
    async moveItem(rec, folderId) {
      const k = requireKey();
      const g = gen;
      if (!rec?.plain) throw new VaultError('undecryptable', 'This item cannot be decrypted, so it cannot be moved.');
      const sealed = sealablePlain(rec.plain, folderId, rec.plain);
      const data = await encryptItem(k, rec.id, sealed);
      guard(g);
      const r = await putItem(g, rec.id, { folder_id: folderId, data, base_revision: rec.revision });
      guard(g);
      const next = itemRecordFromServer(r, sealed);
      items.set(rec.id, next);
      emit('data');
      return next;
    },

    async deleteItem(id) {
      requireKey();
      const g = gen;
      try {
        await api.del(`/vault/items/${id}`, keyQuery());
      } catch (err) {
        if (err?.status !== 404) {
          await recheckAfter(err);
          throw err;
        }
      }
      guard(g);
      items.delete(id);
      emit('data');
    },

    /** Create a folder for a normalised path; returns its record. */
    async createFolder(path) {
      const k = requireKey();
      const g = gen;
      const p = normalizePath(path);
      const id = uuid();
      const name = await encryptFolder(k, id, p);
      guard(g);
      let r;
      try {
        r = await api.post('/vault/folders', keyed({ id, name }));
      } catch (err) {
        await recheckAfter(err);
        throw err;
      }
      guard(g);
      const rec = { id: r.id, revision: r.revision, updated_at: r.updated_at, name: r.name, path: p, plainName: p, error: null };
      folders.set(rec.id, rec);
      emit('data');
      return rec;
    },

    /** Rename one folder record (a 409 throws ConflictError with the server copy). */
    async renameFolder(rec, path) {
      const k = requireKey();
      const g = gen;
      const p = normalizePath(path);
      const name = await encryptFolder(k, rec.id, p);
      guard(g);
      let r;
      try {
        r = await api.put(`/vault/folders/${rec.id}`, keyed({ name, base_revision: rec.revision }));
      } catch (err) {
        const cur = await conflictFrom(err);
        if (cur) {
          guard(g);
          folders.set(cur.id, cur);
          emit('data');
          throw new ConflictError(cur);
        }
        await recheckAfter(err);
        throw err;
      }
      guard(g);
      const next = { ...rec, revision: r.revision, updated_at: r.updated_at, name: r.name, path: p, plainName: p };
      folders.set(rec.id, next);
      emit('data');
      return next;
    },

    /** Delete folder records; the server un-files their items (picked up by a delta sync). */
    async deleteFolders(ids) {
      requireKey();
      for (const id of ids) {
        try {
          await api.del(`/vault/folders/${id}`, keyQuery());
        } catch (err) {
          if (err?.status !== 404) {
            await recheckAfter(err);
            throw err;
          }
        }
      }
      await sync();
    },

    /**
     * Import interchange items: create missing folders, encrypt, POST /vault/items/batch in
     * chunks. entries: [{plain, folderPath}]. Returns {created, errors, foldersCreated}.
     * @param {Array<{plain: object, folderPath: string}>} entries
     * @param {{onProgress?: (p: {phase: string, done: number, total: number}) => void, signal?: AbortSignal}} [opts]
     */
    async importEntries(entries, { onProgress, signal } = {}) {
      const k = requireKey();
      const g = gen;
      const byPath = new Map();
      for (const f of folders.values()) if (!f.error && !byPath.has(f.path)) byPath.set(f.path, f.id);
      const needed = [...new Set(entries.map((e) => normalizePath(e.folderPath)).filter((p) => p && !byPath.has(p)))];
      let foldersCreated = 0;
      for (const p of needed) {
        signal?.throwIfAborted();
        const rec = await api$.createFolder(p);
        byPath.set(p, rec.id);
        foldersCreated++;
        onProgress?.({ phase: 'folders', done: foldersCreated, total: needed.length });
      }
      const rows = [];
      for (let i = 0; i < entries.length; i++) {
        signal?.throwIfAborted();
        const id = uuid();
        const p = normalizePath(entries[i].folderPath);
        const folderId = p ? byPath.get(p) ?? null : null;
        // Imported data never carries our own counters: a new item starts at seq 1 in its folder.
        const { seq: _seq, folder_id: _folder, ...plain } = entries[i].plain;
        rows.push({ id, folder_id: folderId, data: await encryptItem(k, id, sealablePlain(plain, folderId, null)) });
        if (i % 50 === 49 || i === entries.length - 1) onProgress?.({ phase: 'encrypt', done: i + 1, total: entries.length });
      }
      guard(g);
      let created = 0;
      const errors = [];
      let sent = 0;
      for (const batch of chunkBatches(rows)) {
        signal?.throwIfAborted();
        guard(g);
        let r;
        try {
          r = await api.post('/vault/items/batch', keyed({ items: batch }));
        } catch (err) {
          await recheckAfter(err);
          throw err;
        }
        created += r.created || 0;
        for (const e of r.errors || []) errors.push(e);
        sent += batch.length;
        onProgress?.({ phase: 'upload', done: sent, total: rows.length });
      }
      await sync();
      return { created, errors, foldersCreated };
    },

    /** Interchange data for export: {folders:[{id,name}], items:[{folderPath, ...plain}]}. */
    exportData() {
      requireKey();
      const fl = [...folders.values()].filter((f) => !f.error).map((f) => ({ id: f.id, name: f.path }));
      const its = [...items.values()].filter((r) => r.plain).map((r) => {
        // seq/folder_id are this vault's bookkeeping, not item data.
        const { seq: _seq, folder_id: _folder, ...plain } = structuredClone(r.plain);
        return { folderPath: api$.folderPath(r.folder_id), ...plain };
      });
      return { folders: fl, items: its };
    },

    /** True when the password unwraps the stored key (re-authentication before export etc.). */
    async verifyMasterPassword(password) {
      try {
        const raw = await unlockVault(password, params);
        raw.fill(0);
        return true;
      } catch (err) {
        if (err?.code === 'bad_password') return false;
        throw err;
      }
    },

    /**
     * Change the master password: re-derive the vault key from the current password, re-wrap it
     * under the new one (fresh salt, the vault's current work factor or more) and PUT /vault/key.
     * Items are untouched; use rotateVaultKey to also replace the vault key.
     */
    async changeMasterPassword(current, next) {
      requireKey();
      const raw = await unlockVault(current, params);
      let p;
      try {
        p = await rewrapVaultKey(raw, next, { iterations: keepIterations(params) });
      } finally {
        raw.fill(0);
      }
      try {
        await api.put('/vault/key', keyed({ ...p, previous_wrapped_key: params.wrapped_key }));
      } catch (err) {
        // 409: changed elsewhere (previous_wrapped_key) or key replaced; load() locks when stale.
        await recheckAfter(err);
        throw err;
      }
      params = { ...params, ...p, updated_at: now().toISOString() };
      unlockedWith = p.wrapped_key;
      emit('status');
    },

    /**
     * Rotate the vault key (§2.3): a new random vault key replaces the current one, every item and folder
     * is re-sealed under it and the key is wrapped under `next` (or the current master password) with a
     * fresh salt — all committed by the server in one transaction. Afterwards an old wrapped_key, the old
     * master password and any remembered copy of the old key open nothing that is stored now or later.
     * Fails with VaultError 'undecryptable' while some record cannot be decrypted (it could not be
     * re-sealed), and with a 409 when the vault changed meanwhile (sync and try again).
     * @param {string} current current master password
     * @param {string} [next] new master password (default: keep the current one)
     * @param {{onStage?: (stage: 'verify'|'encrypt'|'save'|'done') => void}} [opts]
     */
    async rotateVaultKey(current, next = current, { onStage } = {}) {
      requireKey();
      const g = gen;
      onStage?.('verify');
      const raw = await unlockVault(current, params); // proves the master password and the wrapping
      raw.fill(0);
      guard(g);
      await sync();
      guard(g);
      const badItems = [...items.values()].filter((r) => !r.plain).length;
      const badFolders = [...folders.values()].filter((f) => f.error || typeof f.plainName !== 'string').length;
      if (badItems + badFolders) {
        throw new VaultError('undecryptable',
          `${badItems + badFolders} record(s) cannot be decrypted and would be lost; delete them before rotating the key.`);
      }
      onStage?.('encrypt');
      const { params: p, vaultKey } = await createVault(next, { iterations: keepIterations(params) });
      let nk;
      try {
        nk = await importAesKey(vaultKey);
      } finally {
        vaultKey.fill(0);
      }
      const its = [];
      for (const r of items.values()) {
        const plain = sealablePlain(r.plain, r.folder_id, r.plain);
        its.push({ id: r.id, base_revision: r.revision, data: await encryptItem(nk, r.id, plain) });
      }
      const fls = [];
      for (const f of folders.values()) {
        fls.push({ id: f.id, base_revision: f.revision, name: await encryptFolder(nk, f.id, f.plainName) });
      }
      guard(g);
      onStage?.('save');
      let st;
      try {
        st = await api.post('/vault/rekey', { ...p, previous_wrapped_key: params.wrapped_key, key_id: keyId, items: its, folders: fls });
      } catch (err) {
        if (err?.status === 409) await recheckAfter(err);
        throw err;
      }
      guard(g);
      params = st;
      gen++;
      adopt(nk, st);
      setStatus('unlocked');
      emit('status');
      await sync();
      onStage?.('done');
      return status;
    },

    /** Delete the whole vault (account password required by the server). */
    async reset(accountPassword) {
      await api.post('/vault/reset', { password: accountPassword });
      params = { initialized: false, kdf: null, kdf_iterations: null, kdf_salt: null, wrapped_key: null, key_id: null, key_check: null, updated_at: null };
      api$.lock();
      setStatus('uninitialized');
    },
  };
  return api$;
}
