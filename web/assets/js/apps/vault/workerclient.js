// Runs one job in a fresh module worker (worker.js) and resolves with its result. The worker
// is terminated afterwards (releasing e.g. Argon2's 64 MiB) or when the signal aborts.

let seq = 0;

/** Error from a worker job; `code` is a VaultError code ('bad_credentials', 'password_required', …). */
export class WorkerJobError extends Error {
  constructor(code, message) {
    super(message || code);
    this.name = 'WorkerJobError';
    this.code = code;
  }
}

/**
 * @param {object} job message for worker.js (without id)
 * @param {{onProgress?: (fraction: number) => void, signal?: AbortSignal, transfer?: Transferable[],
 *          createWorker?: () => Worker}} [opts]
 * @returns {Promise<any>}
 */
export function runJob(job, { onProgress, signal, transfer = [], createWorker } = {}) {
  return new Promise((resolve, reject) => {
    if (signal?.aborted) {
      reject(new DOMException('Aborted', 'AbortError'));
      return;
    }
    const w = createWorker ? createWorker() : new Worker(new URL('./worker.js', import.meta.url), { type: 'module', name: 'vault-io' });
    const id = ++seq;
    const done = () => {
      w.terminate();
      signal?.removeEventListener('abort', onAbort);
    };
    const onAbort = () => {
      done();
      reject(new DOMException('Aborted', 'AbortError'));
    };
    signal?.addEventListener('abort', onAbort, { once: true });
    w.addEventListener('message', (ev) => {
      const m = ev.data || {};
      if (m.id !== id) return;
      if (m.type === 'progress') onProgress?.(m.fraction);
      else if (m.type === 'result') {
        done();
        resolve(m.result);
      } else if (m.type === 'error') {
        done();
        reject(new WorkerJobError(m.code, m.message));
      }
    });
    w.addEventListener('error', (ev) => {
      done();
      reject(new WorkerJobError('worker_failed', ev?.message || 'The background worker failed to start.'));
    });
    w.postMessage({ ...job, id }, transfer);
  });
}

/** Human message for an import/export error code. */
export function ioErrorMessage(err) {
  switch (err?.code) {
    case 'bad_credentials': return 'Wrong password or key file.';
    case 'bad_keyfile': return 'The key file is not valid.';
    case 'password_required': return 'This file is password protected. Enter its password.';
    case 'bad_password': return 'Wrong password for this file.';
    case 'corrupt': return 'The file is damaged or incomplete.';
    case 'unsupported': return err.message ? `Not supported: ${err.message}` : 'This file format or version is not supported.';
    case 'limit_exceeded': return 'The file exceeds a safety limit (size or key-derivation cost).';
    case 'xml_invalid': return 'The XML is not valid.';
    case 'malformed': return err.message ? `The file could not be read: ${err.message}` : 'The file could not be read.';
    default: return err?.message || 'Something went wrong.';
  }
}
