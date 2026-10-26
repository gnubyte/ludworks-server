// Files app — pure helpers (no DOM): paths, selection, file kinds, sorting, conflict
// names, drop planning and URL state. Unit tested under node (web/tests/files).

/* ------------------------------------------------------------------ paths */

/** Join a directory path and a name ("" is the space root). */
export function joinPath(dir, name) {
  if (!dir) return name || '';
  if (!name) return dir;
  return `${dir}/${name}`;
}

/** Parent directory of a path ("" for top-level entries and the root). */
export function parentPath(p) {
  const i = String(p || '').lastIndexOf('/');
  return i < 0 ? '' : p.slice(0, i);
}

/** Last path segment. */
export function baseName(p) {
  const s = String(p || '');
  const i = s.lastIndexOf('/');
  return i < 0 ? s : s.slice(i + 1);
}

/** True when p equals dir or lies below it. */
export function isWithin(p, dir) {
  if (!dir) return true;
  return p === dir || p.startsWith(`${dir}/`);
}

/** Normalise a user-typed path: trim slashes, drop empty segments. */
export function normalizePath(p) {
  return String(p || '').split('/').map((s) => s.trim()).filter((s) => s && s !== '.').join('/');
}

/** C0/C1 control characters and the Unicode line/paragraph separators. */
function isControl(c) {
  return c < 0x20 || (c >= 0x7f && c <= 0x9f) || c === 0x2028 || c === 0x2029;
}

const WIN_RESERVED =/^(con|prn|aux|nul|com[0-9¹²³]|lpt[0-9¹²³]|conin\$|conout\$)$/i;

/**
 * Validate a file or folder name like the server (ARCHITECTURE.md §2 item 10).
 * @returns {string} problem description, or '' when valid
 */
export function nameProblem(name) {
  const n = String(name ?? '');
  if (!n) return 'Enter a name.';
  if (new TextEncoder().encode(n).length > 255) return 'The name is too long (max 255 bytes).';
  if (n === '.' || n === '..') return 'This name is reserved.';
  if (/[/\\\u0000]/.test(n)) return 'Names cannot contain / or \\.';
  if ([...n].some((ch) => isControl(ch.codePointAt(0)))) return 'Names cannot contain control characters.';
  if (/[. ]$/.test(n)) return 'Names cannot end with a dot or a space.';
  const base = n.split('.')[0].replace(/ +$/, '');
  if (WIN_RESERVED.test(base)) return 'This is a reserved device name.';
  return '';
}

const DOUBLE_EXTS = ['.tar.gz', '.tar.bz2', '.tar.xz', '.tar.zst', '.tar.lz', '.tar.lzma'];

/** Split "name.ext" into [stem, ext] (compound .tar.* kept together; folders have no ext). */
export function splitExt(name, isDir = false) {
  if (isDir) return [name, ''];
  const lower = name.toLowerCase();
  for (const de of DOUBLE_EXTS) {
    if (lower.endsWith(de) && name.length > de.length) return [name.slice(0, -de.length), name.slice(-de.length)];
  }
  const i = name.lastIndexOf('.');
  if (i <= 0 || name.length - i > 16) return [name, ''];
  return [name.slice(0, i), name.slice(i)];
}

/** "name (n).ext" — same scheme as the server's on_conflict=rename. */
export function numberedName(name, isDir, n) {
  const [stem, ext] = splitExt(name, isDir);
  return `${stem} (${n})${ext}`;
}

/** First "name (n).ext" not in `taken` (a Set of names). */
export function freeName(name, isDir, taken) {
  if (!taken.has(name)) return name;
  for (let n = 1; n < 10000; n++) {
    const cand = numberedName(name, isDir, n);
    if (!taken.has(cand)) return cand;
  }
  return numberedName(name, isDir, Date.now());
}

/** Lower-case extension without the dot ("" if none). */
export function extOf(name) {
  const [, ext] = splitExt(String(name || ''), false);
  return ext.replace(/^\./, '').toLowerCase();
}

/* ------------------------------------------------------------- file kinds */

const CODE_EXTS = new Set(['js', 'mjs', 'cjs', 'ts', 'tsx', 'jsx', 'go', 'py', 'rb', 'rs', 'java', 'kt', 'kts', 'c', 'h', 'cc', 'cpp', 'hpp',
  'cs', 'php', 'swift', 'lua', 'pl', 'sh', 'bash', 'zsh', 'ps1', 'bat', 'sql', 'css', 'scss', 'less', 'html', 'htm', 'xml', 'json',
  'yaml', 'yml', 'toml', 'ini', 'conf', 'cfg', 'env', 'dockerfile', 'makefile', 'mk', 'gradle', 'vue', 'svelte', 'diff', 'patch',
  'graphql', 'proto', 'r', 'dart', 'scala', 'hs', 'ex', 'exs', 'erl', 'clj', 'vim', 'tf', 'nix']);
const TEXT_EXTS = new Set(['txt', 'text', 'log', 'nfo', 'srt', 'vtt', 'ass', 'ssa', 'csv', 'tsv', 'rtf', 'cue', 'm3u', 'm3u8', 'gitignore', 'editorconfig']);
const ARCHIVE_EXTS = new Set(['zip', '7z', 'rar', 'gz', 'tgz', 'bz2', 'xz', 'zst', 'tar', 'iso', 'img', 'dmg', 'cab']);
const APP_EXTS = new Set(['exe', 'msi', 'deb', 'rpm', 'apk', 'appimage', 'jar', 'pkg']);
const SHEET_EXTS = new Set(['xlsx', 'xls', 'ods', 'csv', 'tsv']);
const DOC_EXTS = new Set(['doc', 'docx', 'odt', 'pages', 'epub', 'mobi', 'azw3', 'ppt', 'pptx', 'odp', 'key']);
const GAME_EXTS = new Set(['nes', 'sfc', 'smc', 'gb', 'gbc', 'gba', 'n64', 'z64', 'nds', '3ds', 'cia', 'chd', 'gcm', 'wbfs', 'xci', 'nsp']);

/**
 * Classify an entry for icons, colours and preview.
 * @param {{is_dir?: boolean, mime?: string, name?: string}} e
 * @returns {'folder'|'image'|'video'|'audio'|'pdf'|'markdown'|'code'|'text'|'archive'|'app'|'sheet'|'doc'|'game'|'file'}
 */
export function fileKind(e) {
  if (!e) return 'file';
  if (e.is_dir) return 'folder';
  const m = String(e.mime || '');
  const ext = extOf(e.name);
  if (ext === 'svg') return 'image';
  if (m.startsWith('image/')) return 'image';
  if (m.startsWith('video/')) return 'video';
  if (m.startsWith('audio/')) return 'audio';
  if (m === 'application/pdf' || ext === 'pdf') return 'pdf';
  if (ext === 'md' || ext === 'markdown' || m === 'text/markdown') return 'markdown';
  if (SHEET_EXTS.has(ext)) return 'sheet';
  if (CODE_EXTS.has(ext)) return 'code';
  if (TEXT_EXTS.has(ext) || m.startsWith('text/')) return 'text';
  if (ARCHIVE_EXTS.has(ext)) return 'archive';
  if (APP_EXTS.has(ext)) return 'app';
  if (DOC_EXTS.has(ext)) return 'doc';
  if (GAME_EXTS.has(ext)) return 'game';
  if (m === 'application/json' || m === 'application/xml' || m === 'application/yaml') return 'code';
  return 'file';
}

/** Icon name (core/icons.js) for a kind. */
export const KIND_ICONS = {
  folder: 'folder', image: 'image', video: 'film', audio: 'music', pdf: 'file-text', markdown: 'file-text', code: 'code',
  text: 'file-text', archive: 'archive', app: 'box', sheet: 'sheet', doc: 'file-text', game: 'gamepad', file: 'file',
};

/** Human label for a kind. */
export const KIND_LABELS = {
  folder: 'Folder', image: 'Image', video: 'Video', audio: 'Audio', pdf: 'PDF document', markdown: 'Markdown', code: 'Source code',
  text: 'Text', archive: 'Archive', app: 'Application', sheet: 'Spreadsheet', doc: 'Document', game: 'Game', file: 'File',
};

/** Maximum size of text/code shown in the preview. */
export const MAX_TEXT_PREVIEW = 2 * 1024 * 1024;

/**
 * How an entry is previewed: 'image' | 'video' | 'audio' | 'pdf' | 'markdown' | 'code' | 'text' | null.
 * SVG is never rendered (it is served as an attachment).
 */
export function previewKind(e) {
  if (!e || e.is_dir) return null;
  const k = fileKind(e);
  const m = String(e.mime || '');
  if (k === 'image') return extOf(e.name) === 'svg' || m === 'image/svg+xml' ? null : 'image';
  if (k === 'video' || k === 'audio' || k === 'pdf') return k;
  if (k === 'markdown' || k === 'code' || k === 'text' || (k === 'sheet' && ['csv', 'tsv'].includes(extOf(e.name)))) {
    return e.size > MAX_TEXT_PREVIEW ? null : (k === 'sheet' ? 'text' : k);
  }
  return null;
}

/** Language id for highlight.js from a file name ('' = plain). */
export function languageFor(name) {
  const ext = extOf(name);
  const base = baseName(name).toLowerCase();
  if (base === 'dockerfile') return 'dockerfile';
  if (base === 'makefile') return 'makefile';
  const map = {
    js: 'javascript', mjs: 'javascript', cjs: 'javascript', jsx: 'javascript', ts: 'typescript', tsx: 'typescript',
    py: 'python', rb: 'ruby', rs: 'rust', go: 'go', java: 'java', kt: 'kotlin', kts: 'kotlin', c: 'c', h: 'c', cc: 'cpp',
    cpp: 'cpp', hpp: 'cpp', cs: 'csharp', php: 'php', swift: 'swift', lua: 'lua', pl: 'perl', sh: 'bash', bash: 'bash',
    zsh: 'bash', ps1: 'powershell', sql: 'sql', css: 'css', scss: 'css', less: 'css', html: 'html', htm: 'html', xml: 'xml',
    json: 'json', yaml: 'yaml', yml: 'yaml', toml: 'toml', ini: 'ini', conf: 'ini', cfg: 'ini', diff: 'diff', patch: 'diff',
    md: 'markdown', markdown: 'markdown', vue: 'html', svelte: 'html', graphql: 'graphql', dart: 'dart', scala: 'scala',
  };
  return map[ext] || '';
}

/** Media files can be opened in the TV library (API.md §6 guarantees a media item). */
export function isMedia(e) {
  if (!e || e.is_dir) return false;
  return String(e.mime || '').startsWith('video/') || e.category === 'movie' || e.category === 'tv';
}

/* ---------------------------------------------------------------- sorting */

export const SORTS = [
  { value: 'name', label: 'Name' },
  { value: 'mod_time', label: 'Modified' },
  { value: 'size', label: 'Size' },
  { value: 'type', label: 'Type' },
];

/** Validate a stored sort preference. */
export function parseSort(raw) {
  const [sort, order] = String(raw || '').split(':');
  return {
    sort: SORTS.some((s) => s.value === sort) ? sort : 'name',
    order: order === 'desc' ? 'desc' : 'asc',
  };
}

/**
 * Compare two entries like the server listing (folders first, then key, then name).
 * Used to insert new entries locally without a reload.
 */
export function compareEntries(a, b, sort = 'name', order = 'asc') {
  if (!!a.is_dir !== !!b.is_dir) return a.is_dir ? -1 : 1;
  const dir = order === 'desc' ? -1 : 1;
  const byName = a.name.toLowerCase() < b.name.toLowerCase() ? -1 : a.name.toLowerCase() > b.name.toLowerCase() ? 1 : 0;
  let c = 0;
  if (sort === 'size') c = (a.size || 0) - (b.size || 0);
  else if (sort === 'mod_time') c = String(a.mod_time).localeCompare(String(b.mod_time));
  else if (sort === 'type') c = String(a.mime).localeCompare(String(b.mime));
  if (c === 0) c = byName;
  if (c === 0) c = a.id < b.id ? -1 : a.id > b.id ? 1 : 0;
  return c * dir;
}

/* -------------------------------------------------------------- selection */

/**
 * Apply a pointer click to a selection (file-manager semantics).
 *   click → only this; Ctrl/Cmd → toggle; Shift → range from the anchor (added to the
 *   selection when Ctrl/Cmd is held as well).
 * @param {{ids: string[], selected: Set<string>, anchor: number|null}} st
 * @param {number} index clicked index into ids
 * @param {{toggle?: boolean, range?: boolean}} mods
 * @returns {{selected: Set<string>, anchor: number|null}}
 */
export function clickSelect(st, index, { toggle = false, range = false } = {}) {
  const { ids } = st;
  const id = ids[index];
  if (id === undefined) return { selected: new Set(st.selected), anchor: st.anchor };
  if (range && st.anchor !== null && st.anchor !== undefined && ids[st.anchor] !== undefined) {
    const [a, b] = st.anchor < index ? [st.anchor, index] : [index, st.anchor];
    const next = toggle ? new Set(st.selected) : new Set();
    for (let i = a; i <= b; i++) next.add(ids[i]);
    return { selected: next, anchor: st.anchor };
  }
  if (toggle) {
    const next = new Set(st.selected);
    if (next.has(id)) next.delete(id);
    else next.add(id);
    return { selected: next, anchor: index };
  }
  return { selected: new Set([id]), anchor: index };
}

/**
 * Keyboard focus movement in a list (cols = 1) or a grid (cols > 1).
 * @returns {number} new index clamped to [0, count-1]
 */
export function moveFocus(index, key, count, cols = 1, pageRows = 10) {
  if (count <= 0) return -1;
  const cur = index < 0 ? -1 : index;
  let next = cur;
  switch (key) {
    case 'ArrowDown': next = cur < 0 ? 0 : cur + cols; break;
    case 'ArrowUp': next = cur < 0 ? 0 : cur - cols; break;
    case 'ArrowRight': next = cols > 1 ? cur + 1 : cur; break;
    case 'ArrowLeft': next = cols > 1 ? cur - 1 : cur; break;
    case 'Home': next = 0; break;
    case 'End': next = count - 1; break;
    case 'PageDown': next = cur + cols * pageRows; break;
    case 'PageUp': next = cur - cols * pageRows; break;
    default: return cur;
  }
  return Math.max(0, Math.min(count - 1, next));
}

/** Rectangles intersect (rubber-band selection). */
export function intersects(a, b) {
  return a.left < b.right && a.right > b.left && a.top < b.bottom && a.bottom > b.top;
}

/** Normalised rectangle from two points. */
export function rectFrom(x1, y1, x2, y2) {
  return { left: Math.min(x1, x2), top: Math.min(y1, y2), right: Math.max(x1, x2), bottom: Math.max(y1, y2) };
}

/* ------------------------------------------------------------ drop upload */

/**
 * Plan an upload of dropped/picked files: every file gets the directory it goes to
 * (target dir + its relative folder), and folders that would stay empty are listed so
 * they can be created explicitly.
 * @param {Array<{file: {name: string}, relPath: string}>} files relPath includes the file name
 * @param {string[]} dirs relative paths of every dropped directory (may be empty)
 * @param {string} targetDir destination directory in the space
 * @returns {{uploads: Array<{file: any, dir: string, name: string}>, emptyDirs: string[], problems: string[]}}
 */
export function planUpload(files, dirs, targetDir) {
  const uploads = [];
  const problems = [];
  const used = new Set();
  for (const { file, relPath } of files) {
    const rel = normalizePath(relPath || file.name);
    const parts = rel.split('/');
    const bad = parts.find((p) => nameProblem(p));
    if (bad !== undefined) {
      problems.push(`${rel}: ${nameProblem(bad)}`);
      continue;
    }
    const name = parts.pop();
    const dir = joinPath(targetDir, parts.join('/'));
    uploads.push({ file, dir, name });
    for (let i = 1; i <= parts.length; i++) used.add(parts.slice(0, i).join('/'));
  }
  const emptyDirs = [];
  for (const d of dirs || []) {
    const rel = normalizePath(d);
    if (!rel || used.has(rel) || rel.split('/').some((p) => nameProblem(p))) continue;
    // Skip a dir when one of its descendants is already created by a file or listed dir.
    emptyDirs.push(rel);
  }
  emptyDirs.sort();
  const leaves = emptyDirs.filter((d, i) => !emptyDirs.slice(i + 1).some((o) => o.startsWith(`${d}/`)));
  return { uploads, emptyDirs: leaves.map((d) => joinPath(targetDir, d)), problems };
}

/** Group bulk-operation errors: conflicts (resolvable by the user) vs other failures. */
export function splitErrors(errors) {
  const conflicts = [];
  const other = [];
  for (const e of errors || []) (e.code === 'conflict' ? conflicts : other).push(e);
  return { conflicts, other };
}

/* -------------------------------------------------------------- URL state */

/**
 * Derive the view from the route: /files → browse, /files/trash → trash; query
 * space, path, q (search), category, type, kind.
 */
export function routeState(rest, query) {
  const q = query || {};
  const mode = rest === 'trash' ? 'trash' : (q.q || q.category || q.tag || q.kind) ? 'search' : 'browse';
  return {
    mode,
    spaceId: q.space || '',
    path: normalizePath(q.path || ''),
    q: q.q || '',
    filters: {
      category: q.category || '',
      type: q.type === 'file' || q.type === 'dir' ? q.type : '',
      kind: ['image', 'video', 'audio', 'document'].includes(q.kind) ? q.kind : '',
      tag: q.tag || '',
      scope: q.scope === 'all' ? 'all' : 'space',
    },
  };
}

/** Search API query from the view state. */
export function searchQuery(st, limit = 100) {
  const f = st.filters || {};
  const mimePrefix = { image: 'image/', video: 'video/', audio: 'audio/', document: 'application/pdf' }[f.kind] || '';
  return {
    q: st.q || undefined,
    space_id: f.scope === 'all' ? undefined : st.spaceId || undefined,
    category: f.category || undefined,
    tag: f.tag || undefined,
    type: f.type || undefined,
    mime_prefix: mimePrefix || undefined,
    limit,
  };
}

/** Cache-busting version for thumbnails (changes when the content changes). */
export function thumbVersion(e) {
  return `${e.size}-${String(e.mod_time || '').replace(/[^0-9]/g, '')}`;
}

/** Summary of a selection: count, files, folders and total file bytes. */
export function selectionSummary(entries) {
  let files = 0;
  let folders = 0;
  let bytes = 0;
  for (const e of entries) {
    if (e.is_dir) folders += 1;
    else {
      files += 1;
      bytes += e.size || 0;
    }
  }
  return { count: entries.length, files, folders, bytes };
}

/** Parse a comma/semicolon separated tag list. */
export function parseTags(text) {
  const out = [];
  const seen = new Set();
  for (const raw of String(text || '').split(/[,;\n]/)) {
    const t = raw.trim();
    if (!t || t.length > 64) continue;
    const k = t.toLowerCase();
    if (seen.has(k)) continue;
    seen.add(k);
    out.push(t);
  }
  return out;
}
