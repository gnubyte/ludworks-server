// Import: KeePass 2 (.kdbx with password and optional key file, or XML) and Bitwarden (JSON,
// also password-protected, or CSV). Files are parsed and decrypted in a module Web Worker; the
// preview offers folder mapping and duplicate handling; items are then encrypted with the vault
// key here and uploaded with POST /vault/items/batch in chunks.

import { h, cls, uid } from '../../../core/dom.js';
import { icon } from '../../../core/icons.js';
import { field, passwordInput, select } from '../../../core/forms.js';
import * as ui from '../../../core/ui.js';
import { formatBytes } from '../../../core/format.js';
import { TYPES, TYPE_INFO, dupKey, view, normalizePath, hostOf, folderPathError } from '../model.js';
import { runJob, ioErrorMessage } from '../workerclient.js';
import { vicon } from '../widgets.js';

const MAX_FILE = 100 << 20;
const MAX_KEYFILE = 10 << 20;
const PREVIEW_ROWS = 300;

const FORMAT_OPTIONS = [
  { value: 'auto', label: 'Detect automatically' },
  { value: 'kdbx', label: 'KeePass 2 database (.kdbx)' },
  { value: 'keepass-xml', label: 'KeePass 2 XML (.xml)' },
  { value: 'bitwarden-json', label: 'Bitwarden JSON (.json)' },
  { value: 'bitwarden-csv', label: 'Bitwarden CSV (.csv)' },
];

/** Guess from the file name only (cheap; content detection happens in the worker). */
export function formatFromName(name) {
  const n = String(name || '').toLowerCase();
  if (n.endsWith('.kdbx')) return 'kdbx';
  if (n.endsWith('.xml')) return 'keepass-xml';
  if (n.endsWith('.json')) return 'bitwarden-json';
  if (n.endsWith('.csv')) return 'bitwarden-csv';
  return 'auto';
}

/**
 * Plan the import: which rows are duplicates, final folder paths, and which rows to send.
 * @param {{items: object[]}} data worker result
 * @param {{existingKeys: Set<string>, folderMode: 'keep'|'single'|'none', singleFolder: string,
 *          duplicates: 'skip'|'keep', excluded: Set<number>}} o
 */
export function planImport(data, { existingKeys, folderMode, singleFolder, duplicates, excluded = new Set() }) {
  const seen = new Set(existingKeys);
  const rows = data.items.map((it, i) => {
    const { folderPath = '', ...plain } = it;
    const key = dupKey(plain);
    const dup = seen.has(key);
    seen.add(key);
    return { i, plain, folderPath: normalizePath(folderPath), dup };
  });
  const send = rows.filter((r) => !excluded.has(r.i) && !(r.dup && duplicates === 'skip')).map((r) => ({
    plain: r.plain,
    folderPath: folderMode === 'keep' ? r.folderPath : folderMode === 'single' ? normalizePath(singleFolder) : '',
  }));
  return { rows, send, dupCount: rows.filter((r) => r.dup).length };
}

export function render(host, { vault, ctx, openItem }) {
  ctx.setTitle('Import · Passwords');
  const { session } = vault;
  const ctrl = new AbortController();
  let file = null;
  let keyFile = null;
  let data = null;

  const page = h('div', { class: 'page vault-page' },
    ui.pageHeader({ title: 'Import', subtitle: 'Bring your passwords from KeePass or Bitwarden. Files are read and decrypted on this device.' }));
  const stage = h('div', { class: 'stack-lg' });
  page.append(stage);
  host.replaceChildren(page);

  /* ------------------------------------------------------------ step 1 */
  function chooseStep(error = '', needsPassword = false) {
    const fmt = select({ options: FORMAT_OPTIONS, value: file ? formatFromName(file.name) : 'auto' });
    const pw = passwordInput({ name: 'file_password', autocomplete: 'off' });
    const kfName = h('span', { class: 'muted small truncate' }, keyFile ? keyFile.name : 'No key file');
    const kfBtn = ui.button({ label: 'Choose key file…', icon: 'key', size: 'sm' });
    const kfClear = ui.button({ icon: 'x', title: 'Remove key file', size: 'sm', variant: 'ghost' });
    kfClear.hidden = !keyFile;
    kfBtn.addEventListener('click', async () => {
      const [f] = await pickFile({ accept: '.key,.keyx,.xml,*/*' });
      if (!f) return;
      if (f.size > MAX_KEYFILE) {
        ui.toast.error('Key files larger than 10 MiB are not supported.');
        return;
      }
      keyFile = f;
      kfName.textContent = f.name;
      kfClear.hidden = false;
    });
    kfClear.addEventListener('click', () => {
      keyFile = null;
      kfName.textContent = 'No key file';
      kfClear.hidden = true;
    });
    const fileLabel = h('div', { class: 'vault-drop-file' });
    const drop = h('div', { class: cls('vault-drop', { 'has-file': !!file }), tabindex: '0', role: 'button', aria: { label: 'Choose a file to import' } },
      h('span', { class: 'vault-drop-icon' }, icon('upload', 22)),
      h('div', { class: 'vault-drop-text' }, h('strong', {}, 'Drop a file here'), h('span', { class: 'muted small' }, 'or click to choose · .kdbx, .xml, .json, .csv')),
      fileLabel);
    const paintFile = () => {
      drop.classList.toggle('has-file', !!file);
      fileLabel.replaceChildren(...(file ? [icon('file', 14), h('span', { class: 'truncate' }, file.name), h('span', { class: 'muted' }, formatBytes(file.size))] : []));
      updateFields();
    };
    const take = (f) => {
      if (!f) return;
      if (f.size > MAX_FILE) {
        ui.toast.error(`Files larger than ${formatBytes(MAX_FILE)} are not supported.`);
        return;
      }
      file = f;
      fmt.value = formatFromName(f.name);
      paintFile();
    };
    drop.addEventListener('click', async () => take((await pickFile({ accept: '.kdbx,.xml,.json,.csv' }))[0]));
    drop.addEventListener('keydown', (e) => {
      if (e.key === 'Enter' || e.key === ' ') {
        e.preventDefault();
        drop.click();
      }
    });
    drop.addEventListener('dragover', (e) => {
      e.preventDefault();
      drop.classList.add('is-over');
    });
    drop.addEventListener('dragleave', () => drop.classList.remove('is-over'));
    drop.addEventListener('drop', (e) => {
      e.preventDefault();
      drop.classList.remove('is-over');
      take(e.dataTransfer?.files?.[0]);
    });

    const pwField = field({ label: 'File password', control: pw, name: 'file_password', hint: 'The master password of the KeePass database, or the export password of a protected Bitwarden file.' });
    const kfField = h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Key file (optional)'), h('div', { class: 'row' }, kfBtn, kfName, kfClear));
    function updateFields() {
      const f = fmt.value === 'auto' && file ? formatFromName(file.name) : fmt.value;
      pwField.hidden = !(f === 'kdbx' || f === 'bitwarden-json' || f === 'auto');
      kfField.hidden = !(f === 'kdbx' || f === 'auto');
    }
    fmt.addEventListener('change', updateFields);

    const read = ui.button({ label: 'Read file', icon: 'arrow-right', variant: 'primary', type: 'submit' });
    const form = h('form', { class: 'stack', novalidate: true },
      error ? (needsPassword
        ? ui.alert({ variant: 'warning', title: 'Password needed', message: error })
        : ui.alert({ variant: 'danger', title: 'The file could not be imported', message: error })) : null,
      drop,
      h('div', { class: 'form-grid cols-2' }, field({ label: 'Format', control: fmt, name: 'format' }), h('div')),
      h('div', { class: 'form-grid cols-2' }, pwField, kfField),
      h('div', { class: 'row end' }, read));
    form.addEventListener('submit', (e) => {
      e.preventDefault();
      if (!file) {
        ui.toast.warning('Choose a file first.');
        drop.focus();
        return;
      }
      readFile(fmt.value, pw.inputEl.value);
      pw.inputEl.value = '';
    });
    stage.replaceChildren(
      h('div', { class: 'card' }, h('div', { class: 'card-body' }, form)),
      h('p', { class: 'muted small vault-page-foot' }, icon('lock', 14), ' Nothing is uploaded until you confirm the preview, and then only encrypted.'));
    paintFile();
  }

  /* ------------------------------------------------------------ step 2 */
  async function readFile(format, password) {
    const bar = ui.progress({ value: 0, label: 'Reading file' });
    const status = h('div', { class: 'muted small' }, format === 'kdbx' || formatFromName(file.name) === 'kdbx' ? 'Deriving the database key — this can take a few seconds.' : 'Reading…');
    const cancel = ui.button({ label: 'Cancel', variant: 'ghost', onClick: () => job.abort() });
    const job = new AbortController();
    const onAbort = () => job.abort();
    ctrl.signal.addEventListener('abort', onAbort);
    stage.replaceChildren(h('div', { class: 'card' }, h('div', { class: 'card-body stack' },
      h('div', { class: 'row between' }, h('strong', {}, file.name), cancel), bar.el, status)));
    try {
      const input = new Uint8Array(await file.arrayBuffer());
      const kf = keyFile ? new Uint8Array(await keyFile.arrayBuffer()) : undefined;
      const transfer = [input.buffer, ...(kf ? [kf.buffer] : [])];
      data = await runJob({ op: 'import', format, input, password, keyFile: kf }, {
        signal: job.signal, transfer,
        onProgress: (f) => {
          bar.set(f, 1);
          status.textContent = f < 1 ? `Deriving the database key… ${Math.round(f * 100)}%` : 'Decrypting…';
        },
      });
      previewStep();
    } catch (err) {
      if (err?.name === 'AbortError') {
        if (!ctrl.signal.aborted) chooseStep();
        return;
      }
      chooseStep(ioErrorMessage(err), err?.code === 'password_required');
    } finally {
      ctrl.signal.removeEventListener('abort', onAbort);
    }
  }

  /* ------------------------------------------------------------ step 3 */
  function previewStep() {
    const existingKeys = new Set([...session.items.values()].filter((r) => r.plain).map((r) => dupKey(r.plain)));
    const today = new Date().toISOString().slice(0, 10);
    const opts = { existingKeys, folderMode: 'keep', singleFolder: `Imported ${today}`, duplicates: 'skip', excluded: new Set() };
    const counts = Object.fromEntries(TYPES.map((t) => [t, 0]));
    for (const it of data.items) counts[TYPES.includes(it.type) ? it.type : 'note']++;
    const folderCount = new Set(data.items.map((it) => normalizePath(it.folderPath)).filter(Boolean)).size;

    const folderSeg = ui.segmented({
      ariaLabel: 'Folders', value: 'keep',
      options: [{ value: 'keep', label: 'Keep folders' }, { value: 'single', label: 'One folder' }, { value: 'none', label: 'No folders' }],
      onChange: (v) => { opts.folderMode = v; singleWrap.hidden = v !== 'single'; refresh(); },
    });
    const singleIn = h('input', { class: 'input', value: opts.singleFolder, maxlength: '200', aria: { label: 'Folder name' } });
    singleIn.addEventListener('input', () => { opts.singleFolder = singleIn.value; refresh(); });
    const singleWrap = h('div', { class: 'field', hidden: true }, h('span', { class: 'field-label' }, 'Folder name'), singleIn);
    const dupSeg = ui.segmented({
      ariaLabel: 'Duplicates', value: 'skip',
      options: [{ value: 'skip', label: 'Skip duplicates' }, { value: 'keep', label: 'Keep both' }],
      onChange: (v) => { opts.duplicates = v; refresh(); },
    });

    const tbody = h('tbody');
    const table = h('div', { class: 'table-wrap vault-import-table' }, h('table', { class: 'table' },
      h('thead', {}, h('tr', {}, h('th', { class: 'td-check' }, h('span', { class: 'sr-only' }, 'Include')), h('th', {}, 'Name'), h('th', {}, 'Username / details'),
        h('th', {}, 'Website'), h('th', {}, 'Folder'), h('th', {}, ''))), tbody));
    const more = h('p', { class: 'muted small' });
    const go = ui.button({ label: 'Import', icon: 'upload', variant: 'primary' });
    const summary = h('div', { class: 'muted small' });

    function refresh() {
      const plan = planImport(data, opts);
      tbody.replaceChildren(...plan.rows.slice(0, PREVIEW_ROWS).map((r) => {
        const v = view(r.plain);
        const cb = h('input', { type: 'checkbox', class: 'checkbox', checked: !opts.excluded.has(r.i) && !(r.dup && opts.duplicates === 'skip'),
          disabled: r.dup && opts.duplicates === 'skip', aria: { label: `Include ${v.name}` } });
        cb.addEventListener('change', () => {
          if (cb.checked) opts.excluded.delete(r.i);
          else opts.excluded.add(r.i);
          refresh();
        });
        const folder = opts.folderMode === 'keep' ? r.folderPath : opts.folderMode === 'single' ? normalizePath(opts.singleFolder) : '';
        return h('tr', { class: cls({ 'is-dup': r.dup }) },
          h('td', { class: 'td-check' }, cb),
          h('td', {}, h('div', { class: 'cell-main' }, vicon(TYPE_INFO[v.type].icon, 14), h('span', { class: 'cell-title truncate' }, v.name))),
          h('td', { class: 'truncate' }, v.type === 'login' ? v.login.username : TYPE_INFO[v.type].label),
          h('td', { class: 'truncate' }, v.type === 'login' ? hostOf(v.login.uris[0]?.uri) : ''),
          h('td', { class: 'truncate' }, folder || h('span', { class: 'faint' }, '—')),
          h('td', {}, r.dup ? ui.badge('Duplicate', 'warning', { title: 'Same name, username and website as an existing item' }) : null));
      }));
      more.textContent = plan.rows.length > PREVIEW_ROWS ? `Showing the first ${PREVIEW_ROWS} of ${plan.rows.length} items; the rest are imported with the same settings.` : '';
      const folderErr = opts.folderMode === 'single' ? folderPathError(normalizePath(opts.singleFolder), []) : null;
      go.disabled = plan.send.length === 0 || !!folderErr;
      go.querySelector('.btn-label').textContent = `Import ${plan.send.length} item${plan.send.length === 1 ? '' : 's'}`;
      summary.textContent = `${plan.dupCount} duplicate${plan.dupCount === 1 ? '' : 's'} found${opts.duplicates === 'skip' && plan.dupCount ? ' and skipped' : ''}.`;
      return plan;
    }
    go.addEventListener('click', () => upload(refresh().send));

    const warnings = data.warnings?.length
      ? ui.alert({ variant: 'warning', title: `${data.warnings.length} note${data.warnings.length === 1 ? '' : 's'} about this file`,
        message: h('ul', { class: 'vault-warnings' }, data.warnings.slice(0, 20).map((w) => h('li', {}, w)), data.warnings.length > 20 ? h('li', {}, `…and ${data.warnings.length - 20} more`) : null) })
      : null;

    stage.replaceChildren(
      h('div', { class: 'card' }, h('div', { class: 'card-body stack' },
        h('div', { class: 'row between wrap' },
          h('div', { class: 'stack-xs' }, h('strong', {}, file.name),
            h('span', { class: 'muted small' }, `${data.items.length} item${data.items.length === 1 ? '' : 's'} · ${folderCount} folder${folderCount === 1 ? '' : 's'} · `,
              TYPES.filter((t) => counts[t]).map((t) => `${counts[t]} ${counts[t] === 1 ? TYPE_INFO[t].label.toLowerCase() : TYPE_INFO[t].plural.toLowerCase()}`).join(', '))),
          ui.button({ label: 'Choose another file', icon: 'arrow-left', variant: 'ghost', onClick: () => { data = null; chooseStep(); } })),
        warnings,
        h('div', { class: 'form-grid cols-2' },
          h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Folders'), folderSeg.el, singleWrap),
          h('div', { class: 'field' }, h('span', { class: 'field-label' }, 'Duplicates'), dupSeg.el, summary)))),
      table, more,
      h('div', { class: 'row end' }, go));
    refresh();
  }

  /* ------------------------------------------------------------ step 4 */
  async function upload(entries) {
    const bar = ui.progress({ value: 0, label: 'Importing' });
    const status = h('div', { class: 'muted small' }, 'Preparing…');
    stage.replaceChildren(h('div', { class: 'card' }, h('div', { class: 'card-body stack' }, h('strong', {}, 'Importing…'), bar.el, status)));
    try {
      const res = await session.importEntries(entries, {
        signal: ctrl.signal,
        onProgress: ({ phase, done, total }) => {
          const w = { folders: [0, 0.05], encrypt: [0.05, 0.45], upload: [0.5, 0.5] }[phase];
          bar.set(w[0] + (w[1] * done) / Math.max(total, 1), 1);
          status.textContent = phase === 'folders' ? `Creating folders (${done}/${total})…` : phase === 'encrypt' ? `Encrypting (${done}/${total})…` : `Uploading (${done}/${total})…`;
        },
      });
      data = null;
      const skipped = res.errors.length;
      stage.replaceChildren(h('div', { class: 'card' }, h('div', { class: 'card-body stack' },
        ui.alert({ variant: skipped ? 'warning' : 'success', title: `Imported ${res.created} item${res.created === 1 ? '' : 's'}`,
          message: `${res.foldersCreated} folder${res.foldersCreated === 1 ? '' : 's'} created.${skipped ? ` ${skipped} item${skipped === 1 ? ' was' : 's were'} not stored.` : ''}` }),
        skipped ? h('ul', { class: 'vault-warnings small' }, res.errors.slice(0, 20).map((e) => h('li', {}, e.error))) : null,
        h('div', { class: 'row' }, ui.button({ label: 'View items', variant: 'primary', icon: 'arrow-right', onClick: () => openItem(null) }),
          ui.button({ label: 'Import another file', onClick: () => { file = null; keyFile = null; chooseStep(); } })))));
      ui.toast.success(`Imported ${res.created} items`);
    } catch (err) {
      if (err?.name === 'AbortError' || err?.code === 'locked') return;
      stage.replaceChildren(ui.alert({ variant: 'danger', title: 'Import failed', message: ui.errorMessage(err) }),
        ui.button({ label: 'Back', onClick: () => previewStep() }));
    }
  }

  chooseStep();
  return () => {
    ctrl.abort();
    data = null;
  };
}

/** File picker resolving with File[] (empty when cancelled). */
function pickFile({ accept = '' } = {}) {
  return new Promise((resolve) => {
    const input = h('input', { type: 'file', class: 'hidden', accept: accept || undefined, id: uid('pick') });
    let done = false;
    const finish = () => {
      if (done) return;
      done = true;
      resolve(Array.from(input.files || []));
      input.remove();
    };
    input.addEventListener('change', finish, { once: true });
    input.addEventListener('cancel', finish, { once: true });
    document.body.appendChild(input);
    input.click();
  });
}
