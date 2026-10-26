// TV library — thin client for the media API (API.md §6).

/**
 * @param {ReturnType<import('../../../core/api.js').createApi>} api
 */
export function mediaApi(api) {
  const id = (x) => encodeURIComponent(x);
  return {
    list: (query, opts) => api.get('/media', { query, ...opts }),
    facets: (query, opts) => api.get('/media/facets', { query, ...opts }),
    shows: (query, opts) => api.get('/media/shows', { query, ...opts }),
    show: (key, opts) => api.get(`/media/shows/${id(key)}`, opts),
    get: (fileId, opts) => api.get(`/media/${id(fileId)}`, opts),
    patch: (fileId, body) => api.patch(`/media/${id(fileId)}`, body),
    bulk: (fileIds, set) => api.post('/media/bulk', { file_ids: fileIds, set }),
    autoTag: (body) => api.post('/media/auto-tag', body),
    probe: (fileId) => api.post(`/media/${id(fileId)}/probe`, {}),
    progress: (fileId, body) => api.put(`/media/${id(fileId)}/progress`, body),
    setWatched: (fileId, watched) => api.put(`/media/${id(fileId)}/progress`, { position_seconds: 0, completed: !!watched }),
    deletePoster: (fileId) => api.del(`/media/${id(fileId)}/poster`),
    deleteBackdrop: (fileId) => api.del(`/media/${id(fileId)}/backdrop`),
    uploadPoster: (fileId, file, onProgress, signal) => api.upload('PUT', `/media/${id(fileId)}/poster`, file, {
      headers: { 'Content-Type': file.type || 'application/octet-stream' }, onProgress, signal,
    }),
    tmdbSearch: (query, opts) => api.get('/media/tmdb/search', { query, ...opts }),
    identify: (fileId, body) => api.post(`/media/${id(fileId)}/identify`, body),
    spaces: () => api.get('/spaces'),
    categories: () => api.get('/categories'),
    folders: (spaceId, path, opts) => api.get('/files', { query: { space_id: spaceId, path, sort: 'name', limit: 200 }, ...opts }),
    subtitleUrl: (fileId, index) => `/api/v1/media/${id(fileId)}/subtitles/${index}.vtt`,
    downloadUrl: (fileId) => `/api/v1/files/${id(fileId)}/content?download=1`,
  };
}
