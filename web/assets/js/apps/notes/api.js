// Thin wrapper around /api/v1/notes (API.md §8).

/** Attachments and images are limited to 25 MiB by the server. */
export const MAX_ATTACHMENT = 25 * 1024 * 1024;
/** Imports are limited to 50 MiB per request. */
export const MAX_IMPORT = 50 * 1024 * 1024;

/**
 * @param {ReturnType<import('../../core/api.js').createApi>} api
 */
export function notesApi(api) {
  const p = (id) => `/notes/${encodeURIComponent(id)}`;
  return {
    folders: (signal) => api.get('/notes/folders', { signal }),
    createFolder: (name, parentId = null) => api.post('/notes/folders', { name, parent_id: parentId }),
    updateFolder: (id, patch) => api.patch(`/notes/folders/${encodeURIComponent(id)}`, patch),
    deleteFolder: (id) => api.del(`/notes/folders/${encodeURIComponent(id)}`),
    tags: (signal) => api.get('/notes/tags', { signal }),
    list: (params, cursor, { limit = 50, signal } = {}) => api.get('/notes', { query: { ...params, cursor, limit }, signal }),
    get: (id, signal) => api.get(p(id), { signal }),
    create: (body) => api.post('/notes', body),
    update: (id, patch) => api.patch(p(id), patch),
    trash: (id) => api.del(p(id)),
    destroy: (id) => api.del(p(id), { query: { permanent: 1 } }),
    restore: (id) => api.post(`${p(id)}/restore`),
    uploadAttachment: (id, file) => api.upload('POST', `${p(id)}/attachments`, file, {
      query: { filename: file.name || 'image.png' },
      headers: { 'Content-Type': 'application/octet-stream' },
    }),
    importFile: (file, folderId, onProgress, signal) => api.upload('POST', '/notes/import', file, {
      query: { filename: file.name, folder_id: folderId || undefined },
      headers: { 'Content-Type': 'application/octet-stream' },
      onProgress, signal,
    }),
    exportZip: (folderId) => api.download('/notes/export', { folder_id: folderId || undefined, format: 'zip' }),
    exportNote: (id) => api.download(`${p(id)}/export.md`),
  };
}

/**
 * Run fn over items with bounded concurrency; resolves with {ok: [], failed: [{item, error}]}.
 */
export async function eachLimited(items, limit, fn) {
  const ok = [];
  const failed = [];
  let i = 0;
  const worker = async () => {
    while (i < items.length) {
      const item = items[i++];
      try {
        ok.push(await fn(item));
      } catch (error) {
        failed.push({ item, error });
      }
    }
  };
  await Promise.all(Array.from({ length: Math.min(limit, items.length) }, worker));
  return { ok, failed };
}
