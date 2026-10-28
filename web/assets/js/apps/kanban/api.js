// Thin wrapper around /api/v1/kanban (API.md §13).

/** Attachments are limited to 50 MiB by the server. */
export const MAX_ATTACHMENT = 50 * 1024 * 1024;

/** Returned by board() when the board did not change since `etag`. */
export const NOT_MODIFIED = Symbol('not modified');

const enc = encodeURIComponent;

/**
 * @param {ReturnType<import('../../core/api.js').createApi>} api
 */
export function kanbanApi(api) {
  return {
    boards: (archived = false, signal) => api.get('/kanban/boards', { query: { archived: archived ? 'true' : undefined }, signal }),
    createBoard: (body) => api.post('/kanban/boards', body),
    /**
     * GET a board; with `etag` an unchanged board resolves NOT_MODIFIED (304). Resolves
     * {board, etag}.
     */
    async board(id, { etag = null, includeArchived = false, signal } = {}) {
      try {
        const res = await api.request('GET', `/kanban/boards/${enc(id)}`, {
          query: { include_archived: includeArchived ? 'true' : undefined },
          headers: etag ? { 'If-None-Match': etag } : {},
          raw: true, signal,
        });
        return { board: await res.json(), etag: res.headers.get('ETag') };
      } catch (err) {
        if (err?.status === 304) return NOT_MODIFIED;
        throw err;
      }
    },
    updateBoard: (id, patch) => api.patch(`/kanban/boards/${enc(id)}`, patch),
    deleteBoard: (id) => api.del(`/kanban/boards/${enc(id)}`),
    setMembers: (id, members) => api.put(`/kanban/boards/${enc(id)}/members`, { members }),
    leave: (id) => api.post(`/kanban/boards/${enc(id)}/leave`),
    transfer: (id, userId) => api.post(`/kanban/boards/${enc(id)}/transfer`, { user_id: userId }),
    activity: (id, { cardId, cursor, limit = 50, signal } = {}) => api.get(`/kanban/boards/${enc(id)}/activity`, { query: { card_id: cardId, cursor, limit }, signal }),
    createLabel: (boardId, body) => api.post(`/kanban/boards/${enc(boardId)}/labels`, body),
    updateLabel: (id, patch) => api.patch(`/kanban/labels/${enc(id)}`, patch),
    deleteLabel: (id) => api.del(`/kanban/labels/${enc(id)}`),
    createColumn: (boardId, body) => api.post(`/kanban/boards/${enc(boardId)}/columns`, body),
    updateColumn: (id, patch) => api.patch(`/kanban/columns/${enc(id)}`, patch),
    deleteColumn: (id, moveTo) => api.del(`/kanban/columns/${enc(id)}`, { query: { move_cards_to: moveTo || undefined } }),
    moveColumn: (id, position) => api.post(`/kanban/columns/${enc(id)}/move`, { position }),
    users: (q, signal) => api.get('/kanban/users', { query: { q }, signal }),
    createCard: (columnId, body) => api.post(`/kanban/columns/${enc(columnId)}/cards`, body),
    cards: (query, signal) => api.get('/kanban/cards', { query, signal }),
    card: (id, signal) => api.get(`/kanban/cards/${enc(id)}`, { signal }),
    updateCard: (id, patch) => api.patch(`/kanban/cards/${enc(id)}`, patch),
    deleteCard: (id) => api.del(`/kanban/cards/${enc(id)}`),
    moveCard: (id, columnId, position) => api.post(`/kanban/cards/${enc(id)}/move`, position === null || position === undefined
      ? { column_id: columnId } : { column_id: columnId, position }),
    createSubtask: (cardId, body) => api.post(`/kanban/cards/${enc(cardId)}/subtasks`, body),
    updateSubtask: (id, patch) => api.patch(`/kanban/subtasks/${enc(id)}`, patch),
    deleteSubtask: (id) => api.del(`/kanban/subtasks/${enc(id)}`),
    moveSubtask: (id, position) => api.post(`/kanban/subtasks/${enc(id)}/move`, { position }),
    promoteSubtask: (id) => api.post(`/kanban/subtasks/${enc(id)}/promote`),
    comments: (cardId, { cursor, limit = 100, signal } = {}) => api.get(`/kanban/cards/${enc(cardId)}/comments`, { query: { cursor, limit }, signal }),
    createComment: (cardId, body) => api.post(`/kanban/cards/${enc(cardId)}/comments`, { body }),
    updateComment: (id, body) => api.patch(`/kanban/comments/${enc(id)}`, { body }),
    deleteComment: (id) => api.del(`/kanban/comments/${enc(id)}`),
    upload: (cardId, file, { onProgress, signal } = {}) => api.upload('POST', `/kanban/cards/${enc(cardId)}/attachments`, file, {
      query: { filename: file.name || 'file' }, headers: { 'Content-Type': 'application/octet-stream' }, onProgress, signal,
    }),
    attachFromFile: (cardId, fileId) => api.post(`/kanban/cards/${enc(cardId)}/attachments/from-file`, { file_id: fileId }),
    renameAttachment: (id, filename) => api.patch(`/kanban/attachments/${enc(id)}`, { filename }),
    deleteAttachment: (id) => api.del(`/kanban/attachments/${enc(id)}`),
    saveToFiles: (id, body) => api.post(`/kanban/attachments/${enc(id)}/save-to-files`, body),
    downloadAttachment: (id) => api.download(`/kanban/attachments/${enc(id)}`, { download: 1 }),
    attachmentUrl: (id) => `/api/v1/kanban/attachments/${enc(id)}`,
    thumbUrl: (id, size = 'md') => `/api/v1/kanban/attachments/${enc(id)}/thumbnail?size=${size}`,
  };
}
