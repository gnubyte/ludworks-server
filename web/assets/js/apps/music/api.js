// Thin wrapper around /api/v1/music (API.md §12).

const enc = encodeURIComponent;

/**
 * @param {ReturnType<import('../../core/api.js').createApi>} api
 */
export function musicApi(api) {
  const raw = (method, path, file, { query, onProgress, signal } = {}) => api.upload(method, path, file, {
    query, onProgress, signal, headers: { 'Content-Type': file?.type || 'application/octet-stream' },
  });
  return {
    /* tracks */
    tracks: (query, signal) => api.get('/music/tracks', { query, signal }),
    lookup: (ids, signal) => api.post('/music/tracks/lookup', { ids }, { signal }),
    bulk: (fileIds, set) => api.post('/music/tracks/bulk', { file_ids: fileIds, set }),
    track: (id, signal) => api.get(`/music/tracks/${enc(id)}`, { signal }),
    patchTrack: (id, patch) => api.patch(`/music/tracks/${enc(id)}`, patch),
    probe: (id, reset = false) => api.post(`/music/tracks/${enc(id)}/probe`, null, { query: { reset: reset ? 1 : undefined } }),
    lyrics: (id, signal) => api.get(`/music/tracks/${enc(id)}/lyrics`, { signal }),
    rate: (id, rating) => api.put(`/music/tracks/${enc(id)}/rating`, { rating }),
    favorite: (kind, id, favorite) => api.put('/music/favorites', { kind, id, favorite: !!favorite }),
    download: (id) => api.download(`/music/tracks/${enc(id)}/download`, { quality: 'original' }),

    /* albums, artists, genres */
    albums: (query, signal) => api.get('/music/albums', { query, signal }),
    album: (id, signal) => api.get(`/music/albums/${enc(id)}`, { signal }),
    putAlbumArt: (id, file, opts) => raw('PUT', `/music/albums/${enc(id)}/art`, file, opts),
    deleteAlbumArt: (id) => api.del(`/music/albums/${enc(id)}/art`),
    artists: (query, signal) => api.get('/music/artists', { query, signal }),
    artist: (id, signal) => api.get(`/music/artists/${enc(id)}`, { signal }),
    genres: (signal) => api.get('/music/genres', { signal }),
    facets: (signal) => api.get('/music/facets', { signal }),
    search: (q, limit, signal) => api.get('/music/search', { query: { q, limit }, signal }),
    home: (signal) => api.get('/music/home', { signal }),

    /* plays */
    plays: (plays, opts = {}) => api.post('/music/plays', { plays }, opts),
    history: (cursor, signal, limit = 100) => api.get('/music/history', { query: { cursor, limit }, signal }),
    clearHistory: () => api.del('/music/history'),

    /* playlists */
    playlists: (query, signal) => api.get('/music/playlists', { query, signal }),
    createPlaylist: (body) => api.post('/music/playlists', body),
    importPlaylist: (file, { name, spaceId, basePath }, opts = {}) => raw('POST', '/music/playlists/import', file, {
      ...opts, query: { name: name || undefined, space_id: spaceId || undefined, base_path: basePath || undefined },
    }),
    playlist: (id, signal) => api.get(`/music/playlists/${enc(id)}`, { signal }),
    patchPlaylist: (id, body) => api.patch(`/music/playlists/${enc(id)}`, body),
    deletePlaylist: (id) => api.del(`/music/playlists/${enc(id)}`),
    insertItems: (id, trackIds, { position, baseVersion } = {}) => api.post(`/music/playlists/${enc(id)}/items`, {
      track_ids: trackIds, position: position ?? undefined, base_version: baseVersion ?? undefined,
    }),
    removeItems: (id, itemIds, baseVersion) => api.post(`/music/playlists/${enc(id)}/items/remove`, { item_ids: itemIds, base_version: baseVersion ?? undefined }),
    moveItems: (id, itemIds, position, baseVersion) => api.post(`/music/playlists/${enc(id)}/items/move`, { item_ids: itemIds, position, base_version: baseVersion ?? undefined }),
    putPlaylistArt: (id, file, opts) => raw('PUT', `/music/playlists/${enc(id)}/art`, file, opts),
    deletePlaylistArt: (id) => api.del(`/music/playlists/${enc(id)}/art`),
    duplicate: (id, body = {}) => api.post(`/music/playlists/${enc(id)}/duplicate`, body),
    exportPlaylist: (id) => api.download(`/music/playlists/${enc(id)}/export.m3u8`),

    /* library */
    rescan: (body = {}) => api.post('/music/rescan', body),
    spaces: (signal) => api.get('/spaces', { signal }),
    categories: (signal) => api.get('/categories', { signal }),
  };
}
