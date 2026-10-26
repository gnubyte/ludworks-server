// Thin wrappers over the TV/media endpoints (API.md §6–7). Every function takes the ctx.api
// client and an optional AbortSignal.

/** @param {any} api core/api.js client */
export function tvApi(api) {
  const enc = encodeURIComponent;
  return {
    channels: (signal) => api.get('/tv/channels', { signal }),
    channel: (id, signal) => api.get(`/tv/channels/${enc(id)}`, { signal }),
    createChannel: (body) => api.post('/tv/channels', body),
    updateChannel: (id, body) => api.patch(`/tv/channels/${enc(id)}`, body),
    deleteChannel: (id) => api.del(`/tv/channels/${enc(id)}`),
    reshuffle: (id) => api.post(`/tv/channels/${enc(id)}/reshuffle`, {}),
    restart: (id) => api.post(`/tv/channels/${enc(id)}/restart`, {}),
    uploadLogo: (id, file, opts) => api.upload('PUT', `/tv/channels/${enc(id)}/logo`, file, { headers: { 'Content-Type': file.type || 'application/octet-stream' }, ...opts }),
    deleteLogo: (id) => api.del(`/tv/channels/${enc(id)}/logo`),
    now: (id, signal) => api.get(`/tv/channels/${enc(id)}/now`, { signal }),
    setPosition: (id, index, offset, opts = {}) => api.put(`/tv/channels/${enc(id)}/position`, { index, offset_seconds: Math.max(0, offset) }, opts),
    guide: ({ start, hours, channelIds } = {}, signal) => api.get('/tv/guide', {
      signal, query: { start, hours, channel_ids: channelIds?.length ? channelIds.join(',') : undefined },
    }),
    autoGenerate: (body) => api.post('/tv/channels/auto-generate', body),

    lineups: (cursor, signal) => api.get('/tv/lineups', { signal, query: { cursor, limit: 100 } }),
    lineup: (id, signal) => api.get(`/tv/lineups/${enc(id)}`, { signal }),
    createLineup: (body) => api.post('/tv/lineups', body),
    updateLineup: (id, body) => api.patch(`/tv/lineups/${enc(id)}`, body),
    deleteLineup: (id, force = false) => api.del(`/tv/lineups/${enc(id)}`, { query: force ? { force: 1 } : undefined }),
    replaceItems: (id, items) => api.put(`/tv/lineups/${enc(id)}/items`, { items }),
    insertItems: (id, items, position) => api.post(`/tv/lineups/${enc(id)}/items`, { items, position }),
    expanded: (id, signal) => api.get(`/tv/lineups/${enc(id)}/expanded`, { signal }),

    feeds: (signal) => api.get('/tv/feeds', { signal }),
    rotateFeeds: () => api.post('/tv/feeds/rotate', {}),

    media: (id, signal) => api.get(`/media/${enc(id)}`, { signal }),
    mediaList: (query, signal) => api.get('/media', { signal, query }),
    shows: (query, signal) => api.get('/media/shows', { signal, query }),
    facets: (signal) => api.get('/media/facets', { signal }),
    progress: (id, position, completed, opts = {}) => api.put(`/media/${enc(id)}/progress`, { position_seconds: Math.max(0, position), completed }, opts),
    spaces: (signal) => api.get('/spaces', { signal }),
    categories: (signal) => api.get('/categories', { signal }),
    me: (signal) => api.get('/me', { signal }),
  };
}

/** Collect every page of a cursor endpoint (bounded). */
export async function allPages(fetchPage, max = 20) {
  const out = [];
  let cursor = null;
  for (let i = 0; i < max; i++) {
    const page = await fetchPage(cursor);
    out.push(...(page?.items || []));
    cursor = page?.next_cursor || null;
    if (!cursor) break;
  }
  return out;
}
