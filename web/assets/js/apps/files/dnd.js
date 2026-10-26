// Files app — drag and drop: reading dropped files *and folders* from the OS
// (DataTransferItem.webkitGetAsEntry recursion) and internal drags of entries.

/** MIME type used for internal drags of entries. */
export const DRAG_TYPE = 'application/x-ludworks-entries';

/** True when a drag carries files from the OS. */
export function hasOSFiles(dt) {
  return !!dt && Array.from(dt.types || []).includes('Files');
}

/** True when a drag carries entries from this app. */
export function hasEntries(dt) {
  return !!dt && Array.from(dt.types || []).includes(DRAG_TYPE);
}

const MAX_DROP_FILES = 100_000;

function readAllEntries(reader) {
  // readEntries returns batches (Chrome: 100 at a time) until an empty batch.
  return new Promise((resolve, reject) => {
    const out = [];
    const next = () => reader.readEntries((batch) => {
      if (!batch.length) resolve(out);
      else {
        out.push(...batch);
        if (out.length > MAX_DROP_FILES) reject(new Error('Too many files in the dropped folders.'));
        else next();
      }
    }, reject);
    next();
  });
}

function entryFile(entry) {
  return new Promise((resolve, reject) => entry.file(resolve, reject));
}

/**
 * Collect the files and folders of a drop. Must be called synchronously from the drop
 * handler (DataTransferItems are only valid during the event); reading is async.
 * @param {DataTransfer} dt
 * @returns {Promise<{files: Array<{file: File, relPath: string}>, dirs: string[]}>}
 */
export function readDrop(dt) {
  const roots = [];
  const plain = [];
  for (const item of Array.from(dt.items || [])) {
    if (item.kind !== 'file') continue;
    const entry = typeof item.webkitGetAsEntry === 'function' ? item.webkitGetAsEntry() : null;
    if (entry) roots.push(entry);
    else {
      const f = item.getAsFile();
      if (f) plain.push(f);
    }
  }
  if (!roots.length && !plain.length) for (const f of Array.from(dt.files || [])) plain.push(f);
  return (async () => {
    const files = plain.map((f) => ({ file: f, relPath: f.name }));
    const dirs = [];
    const walk = async (entry, prefix) => {
      const rel = prefix ? `${prefix}/${entry.name}` : entry.name;
      if (entry.isFile) {
        files.push({ file: await entryFile(entry), relPath: rel });
      } else if (entry.isDirectory) {
        dirs.push(rel);
        const children = await readAllEntries(entry.createReader());
        for (const c of children) {
          await walk(c, rel);
          if (files.length > MAX_DROP_FILES) throw new Error('Too many files in the dropped folders.');
        }
      }
    };
    for (const r of roots) await walk(r, '');
    return { files, dirs };
  })();
}

/**
 * Files picked with <input webkitdirectory>: relative paths come from webkitRelativePath.
 * @param {File[]} list
 */
export function pickedFiles(list) {
  return list.map((f) => ({ file: f, relPath: f.webkitRelativePath || f.name }));
}

/** A small drag image showing "N items" (built off-screen, removed after the drag starts). */
export function dragBadge(doc, count, label) {
  const el = doc.createElement('div');
  el.className = 'fx-drag-badge';
  el.textContent = count === 1 ? label : `${count} items`;
  doc.body.appendChild(el);
  setTimeout(() => el.remove(), 0);
  return el;
}
