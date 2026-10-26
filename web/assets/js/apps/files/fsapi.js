// Files app — thin wrappers over the /api/v1 files endpoints (API.md §5).

/**
 * @param {ReturnType<import('../../core/api.js').createApi>} api
 */
export function filesApi(api) {
  const enc = encodeURIComponent;
  return {
    spaces: (opts) => api.get('/spaces', opts),
    categories: (opts) => api.get('/categories', opts),
    /** One page of a directory listing. */
    list: (spaceId, path, { sort = 'name', order = 'asc', cursor = null, limit = 100, signal } = {}) =>
      api.get('/files', { query: { space_id: spaceId, path, sort, order, cursor, limit }, signal }),
    search: (query, cursor, signal) => api.get('/files/search', { query: { ...query, cursor }, signal }),
    get: (id, opts) => api.get(`/files/${enc(id)}`, opts),
    patch: (id, body) => api.patch(`/files/${enc(id)}`, body),
    mkdir: (spaceId, path, name) => api.post('/files/folders', { space_id: spaceId, path, name }),
    move: (ids, destSpaceId, destPath, onConflict = 'fail') =>
      api.post('/files/move', { ids, dest_space_id: destSpaceId, dest_path: destPath, on_conflict: onConflict }),
    copy: (ids, destSpaceId, destPath, onConflict = 'fail') =>
      api.post('/files/copy', { ids, dest_space_id: destSpaceId, dest_path: destPath, on_conflict: onConflict }),
    trash: (ids) => api.post('/files/delete', { ids }),
    bulkMeta: (body) => api.post('/files/bulk-meta', body),
    rescan: (spaceId, path) => api.post('/files/rescan', { space_id: spaceId, path }),
    trashList: (cursor, signal) => api.get('/trash', { query: { cursor, limit: 100 }, signal }),
    restore: (id, rename = false) => api.post(`/trash/${enc(id)}/restore`, null, { query: { rename: rename ? 1 : undefined } }),
    purge: (id) => api.del(`/trash/${enc(id)}`),
    emptyTrash: () => api.del('/trash'),
    /** Same-origin URLs (cookie auth). */
    contentUrl: (id, download = false) => api.url(`/files/${enc(id)}/content`, download ? { download: 1 } : undefined),
    thumbUrl: (id, size = 'md', v = '') => api.url(`/files/${enc(id)}/thumbnail`, { size, v }),
    zipUrl: (id) => api.url(`/files/${enc(id)}/zip`),
    download: (id) => api.download(`/files/${enc(id)}/content`, { download: 1 }),
    downloadZip: (id) => api.download(`/files/${enc(id)}/zip`),
  };
}

/**
 * Load every sub-folder of a directory (folders come first in listings, so paging
 * stops at the first page that contains a file).
 */
export async function listFolders(fapi, spaceId, path, signal) {
  const out = [];
  let cursor = null;
  for (let i = 0; i < 50; i++) {
    const page = await fapi.list(spaceId, path, { cursor, limit: 200, signal });
    const dirs = page.items.filter((e) => e.is_dir);
    out.push(...dirs);
    if (!page.next_cursor || dirs.length < page.items.length) break;
    cursor = page.next_cursor;
  }
  return out;
}
