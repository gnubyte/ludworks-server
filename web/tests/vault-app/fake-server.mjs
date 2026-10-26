import { randomUUID } from 'node:crypto';
// In-memory stand-in for the /api/v1/vault endpoints with the real server's semantics
// (revisions, 409 details.current, per-user change clock, tombstones, 410 for stale `since`,
// key_id / key_check, 409 vault_key_changed for writes naming a retired key, POST /vault/rekey,
// no silent KDF downgrade). Used to test the Passwords app session logic under node.

export class FakeApiError extends Error {
  constructor(status, code, message, details = null) {
    super(message || code);
    this.status = status;
    this.code = code;
    this.details = details;
  }
}

export function createFakeServer({ accountPassword = 'account password' } = {}) {
  const st = { keys: null, items: new Map(), folders: new Map(), clock: 0, horizon: 0, calls: [] };
  const stamp = () => {
    st.clock = Math.max(Date.now(), st.clock + 1);
    return new Date(st.clock).toISOString();
  };
  const ms = (s) => Date.parse(s);
  const needInit = () => {
    if (!st.keys) throw new FakeApiError(409, 'conflict', 'The vault has not been set up yet');
  };
  const pubItem = (r) => ({ id: r.id, folder_id: r.folder_id, data: r.data, revision: r.revision, created_at: r.created_at, updated_at: r.updated_at });
  const pubFolder = (f) => ({ id: f.id, name: f.name, revision: f.revision, updated_at: f.updated_at });
  const state = () => (st.keys ? { initialized: true, ...st.keys } : { initialized: false, kdf: null, kdf_iterations: null, kdf_salt: null, wrapped_key: null, key_id: null, key_check: null, updated_at: null });
  const keyChanged = () => new FakeApiError(409, 'vault_key_changed', 'The vault key was replaced on another device');
  // Writes may name the key they were sealed under (body key_id, or ?key_id= for DELETE).
  const checkKey = (b, q) => {
    const named = b?.key_id || q?.key_id || '';
    if (!st.keys) throw named ? keyChanged() : new FakeApiError(409, 'conflict', 'The vault has not been set up yet');
    if (named && named !== st.keys.key_id) throw keyChanged();
  };
  const noDowngrade = (b) => {
    if (b.kdf_iterations < st.keys.kdf_iterations && !b.allow_kdf_downgrade) {
      throw new FakeApiError(422, 'validation_failed', 'Validation failed', { fields: { kdf_iterations: 'must not be lower' } });
    }
  };
  const keyParams = (b, keyId, keyCheck, s) => ({
    kdf: b.kdf, kdf_iterations: b.kdf_iterations, kdf_salt: b.kdf_salt, wrapped_key: b.wrapped_key, key_id: keyId, key_check: keyCheck ?? null, updated_at: s,
  });

  const routes = {
    'GET /vault': () => state(),
    'POST /vault/init': (b) => {
      if (st.keys) throw new FakeApiError(409, 'conflict', 'The vault is already initialised');
      const s = stamp();
      st.horizon = ms(s);
      st.keys = keyParams(b, randomUUID(), b.key_check, s);
      return state();
    },
    'PUT /vault/key': (b) => {
      checkKey(b);
      if (b.previous_wrapped_key !== st.keys.wrapped_key) throw new FakeApiError(409, 'conflict', 'mismatch');
      noDowngrade(b);
      st.keys = keyParams(b, st.keys.key_id, st.keys.key_check, new Date().toISOString());
      return null;
    },
    'PUT /vault/key-check': (b) => {
      if (!b.key_id) throw new FakeApiError(422, 'validation_failed', 'key_id');
      checkKey(b);
      if (st.keys.key_check && st.keys.key_check !== b.key_check) throw new FakeApiError(409, 'conflict', 'already set');
      st.keys.key_check = b.key_check;
      return null;
    },
    'POST /vault/rekey': (b) => {
      if (!b.key_id || !b.key_check) throw new FakeApiError(422, 'validation_failed', 'key_id/key_check');
      checkKey(b);
      if (b.previous_wrapped_key !== st.keys.wrapped_key) throw new FakeApiError(409, 'conflict', 'mismatch');
      noDowngrade(b);
      const liveItems = [...st.items.values()].filter((r) => !r.deleted);
      const liveFolders = [...st.folders.values()].filter((f) => !f.deleted);
      const stale = new FakeApiError(409, 'conflict', 'changed while rotating', { reason: 'records_changed' });
      if (liveItems.length !== b.items.length || liveFolders.length !== b.folders.length) throw stale;
      for (const it of b.items) if (st.items.get(it.id)?.deleted !== undefined || st.items.get(it.id)?.revision !== it.base_revision) throw stale;
      for (const f of b.folders) if (st.folders.get(f.id)?.deleted !== undefined || st.folders.get(f.id)?.revision !== f.base_revision) throw stale;
      const s = stamp();
      for (const it of b.items) Object.assign(st.items.get(it.id), { data: it.data, revision: it.base_revision + 1, updated_at: s });
      for (const f of b.folders) Object.assign(st.folders.get(f.id), { name: f.name, revision: f.base_revision + 1, updated_at: s });
      st.horizon = ms(s);
      st.keys = keyParams(b, randomUUID(), b.key_check, s);
      st.calls.push({ rekey: b.items.length });
      return state();
    },
    'POST /vault/reset': (b) => {
      if (b.password !== accountPassword) throw new FakeApiError(401, 'invalid_credentials', 'The account password is wrong');
      st.keys = null;
      st.items.clear();
      st.folders.clear();
      return null;
    },
    'GET /vault/sync': (_, q) => {
      const since = q?.since ? ms(q.since) : null;
      if (st.keys && since !== null && (since < st.horizon || since > st.clock)) throw new FakeApiError(410, 'gone', 'stale');
      const ch = (r) => (since === null ? !r.deleted : ms(r.updated_at) > since);
      return {
        folders: [...st.folders.values()].filter((f) => ch(f) && !f.deleted).map(pubFolder),
        items: [...st.items.values()].filter((r) => ch(r) && !r.deleted).map(pubItem),
        deleted_item_ids: since === null ? [] : [...st.items.values()].filter((r) => r.deleted && ch(r)).map((r) => r.id),
        deleted_folder_ids: since === null ? [] : [...st.folders.values()].filter((f) => f.deleted && ch(f)).map((f) => f.id),
        key_id: st.keys ? st.keys.key_id : null,
        server_time: st.keys ? new Date(st.clock).toISOString() : new Date().toISOString(),
      };
    },
    'POST /vault/items': (b) => {
      checkKey(b);
      if (st.items.has(b.id)) throw new FakeApiError(409, 'conflict', 'exists');
      if (b.folder_id && !(st.folders.get(b.folder_id) && !st.folders.get(b.folder_id).deleted)) throw new FakeApiError(422, 'validation_failed', 'folder', { fields: { folder_id: 'folder does not exist' } });
      const s = stamp();
      const r = { id: b.id, folder_id: b.folder_id ?? null, data: b.data, revision: 1, created_at: s, updated_at: s };
      st.items.set(b.id, r);
      return pubItem(r);
    },
    'POST /vault/items/batch': (b) => {
      checkKey(b);
      if (!Array.isArray(b.items) || b.items.length === 0 || b.items.length > 1000) throw new FakeApiError(422, 'validation_failed', 'items');
      st.calls.push({ batch: b.items.length });
      const s = stamp();
      const out = { created: 0, errors: [] };
      for (const it of b.items) {
        if (st.items.has(it.id)) out.errors.push({ id: it.id, error: 'an item with this id already exists' });
        else {
          st.items.set(it.id, { id: it.id, folder_id: it.folder_id ?? null, data: it.data, revision: 1, created_at: s, updated_at: s });
          out.created++;
        }
      }
      return out;
    },
    'PUT /vault/items/:id': (b, _, id) => {
      checkKey(b);
      const r = st.items.get(id);
      if (!r || r.deleted) throw new FakeApiError(404, 'not_found', 'Item not found');
      if (r.revision !== b.base_revision) throw new FakeApiError(409, 'conflict', 'changed', { current: pubItem(r) });
      if ('folder_id' in b) r.folder_id = b.folder_id;
      r.data = b.data;
      r.revision++;
      r.updated_at = stamp();
      return pubItem(r);
    },
    'DELETE /vault/items/:id': (_, q, id) => {
      checkKey(null, q);
      const r = st.items.get(id);
      if (!r || r.deleted) throw new FakeApiError(404, 'not_found', 'Item not found');
      Object.assign(r, { deleted: true, data: null, folder_id: null, revision: r.revision + 1, updated_at: stamp() });
      return null;
    },
    'GET /vault/folders': () => [...st.folders.values()].filter((f) => !f.deleted).map(pubFolder),
    'POST /vault/folders': (b) => {
      checkKey(b);
      if (st.folders.has(b.id)) throw new FakeApiError(409, 'conflict', 'exists');
      const f = { id: b.id, name: b.name, revision: 1, updated_at: stamp() };
      st.folders.set(b.id, f);
      return pubFolder(f);
    },
    'PUT /vault/folders/:id': (b, _, id) => {
      checkKey(b);
      const f = st.folders.get(id);
      if (!f || f.deleted) throw new FakeApiError(404, 'not_found', 'Folder not found');
      if (f.revision !== b.base_revision) throw new FakeApiError(409, 'conflict', 'changed', { current: pubFolder(f) });
      f.name = b.name;
      f.revision++;
      f.updated_at = stamp();
      return pubFolder(f);
    },
    'DELETE /vault/folders/:id': (_, q, id) => {
      checkKey(null, q);
      const f = st.folders.get(id);
      if (!f || f.deleted) throw new FakeApiError(404, 'not_found', 'Folder not found');
      const s = stamp();
      for (const r of st.items.values()) {
        if (!r.deleted && r.folder_id === id) Object.assign(r, { folder_id: null, revision: r.revision + 1, updated_at: s });
      }
      Object.assign(f, { deleted: true, name: null, revision: f.revision + 1, updated_at: s });
      return null;
    },
  };

  async function call(method, path, body, query) {
    let key = `${method} ${path}`;
    let id = null;
    const m = /^(.*\/(?:items|folders))\/([^/]+)$/.exec(path);
    if (!routes[key] && m && m[2] !== 'batch') {
      key = `${method} ${m[1]}/:id`;
      id = m[2];
    }
    const fn = routes[key];
    if (!fn) throw new FakeApiError(404, 'not_found', `no route ${key}`);
    await Promise.resolve();
    const res = fn(body ? JSON.parse(JSON.stringify(body)) : body, query, id);
    return res === undefined ? null : JSON.parse(JSON.stringify(res));
  }

  return {
    state: st,
    api: {
      get: (p, o = {}) => call('GET', p, null, o.query),
      post: (p, b) => call('POST', p, b),
      put: (p, b) => call('PUT', p, b),
      del: (p, o = {}) => call('DELETE', p, null, o.query),
    },
  };
}
