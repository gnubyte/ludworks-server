// Module Web Worker for heavy import/export work: KDBX key derivation (Argon2 / AES-KDF),
// decryption and encryption, Bitwarden password-protected exports, and parsing large files,
// so the UI stays responsive. Messages:
//   {id, op: 'import', format, input, password?, keyFile?}  → {id, type: 'result', result}
//   {id, op: 'export', format, data, opts}                   → {id, type: 'result', result}
//   {id, op: 'keyfile'}                                      → {id, type: 'result', result: Uint8Array}
// Progress: {id, type: 'progress', fraction}. Errors: {id, type: 'error', code, message}.
// Nothing is persisted; the page terminates the worker after each job.

import { importVault, exportVault, detectFormat } from '../../lib/vault/convert.js';
import { generateKeyFile } from '../../lib/vault/kdbx.js';

const FORMATS = new Set(['kdbx', 'keepass-xml', 'bitwarden-json', 'bitwarden-csv']);

function post(msg, transfer) {
  self.postMessage(msg, transfer || []);
}

self.addEventListener('message', async (ev) => {
  const m = ev.data || {};
  const id = m.id;
  let last = -1;
  const onProgress = (f) => {
    const p = Math.round(Math.max(0, Math.min(1, Number(f) || 0)) * 100);
    if (p !== last) {
      last = p;
      post({ id, type: 'progress', fraction: p / 100 });
    }
  };
  try {
    if (m.op === 'import') {
      const format = m.format === 'auto' ? detectFormat(m.input) : m.format;
      if (!FORMATS.has(format)) throw Object.assign(new Error('Unsupported or unrecognised file format.'), { code: 'unsupported' });
      const result = await importVault(format, m.input, { password: m.password || undefined, keyFile: m.keyFile || undefined, onProgress });
      post({ id, type: 'result', result: { format, ...result } });
    } else if (m.op === 'export') {
      if (!FORMATS.has(m.format)) throw Object.assign(new Error('Unsupported export format.'), { code: 'unsupported' });
      const result = await exportVault(m.format, m.data, { ...(m.opts || {}), onProgress });
      if (result instanceof Uint8Array) post({ id, type: 'result', result }, [result.buffer]);
      else post({ id, type: 'result', result });
    } else if (m.op === 'keyfile') {
      const result = await generateKeyFile();
      post({ id, type: 'result', result }, [result.buffer]);
    } else {
      throw Object.assign(new Error(`unknown op ${m.op}`), { code: 'invalid_argument' });
    }
  } catch (err) {
    post({ id, type: 'error', code: err?.code || 'internal', message: err?.message || String(err) });
  }
});
