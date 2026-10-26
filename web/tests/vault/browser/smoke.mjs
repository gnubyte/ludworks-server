// Browser smoke test: exercises the vault library end to end in a real browser (no Node APIs).
import * as vc from '../../../assets/js/lib/vault/crypto.js';
import { generateTotp } from '../../../assets/js/lib/vault/totp.js';
import { argon2 } from '../../../assets/js/lib/vault/argon2.js';
import { readKdbx, writeKdbx, generateKeyFile } from '../../../assets/js/lib/vault/kdbx.js';
import { exportVault, importVault } from '../../../assets/js/lib/vault/convert.js';
import { generatePassphrase, estimateStrength } from '../../../assets/js/lib/vault/generator.js';
import { matchUri } from '../../../assets/js/lib/vault/url-match.js';

const out = document.getElementById('out');
const results = {};

async function step(name, fn) {
  const t0 = performance.now();
  try {
    results[name] = { ok: true, detail: await fn(), ms: Math.round(performance.now() - t0) };
  } catch (e) {
    results[name] = { ok: false, error: `${e.name}: ${e.code || ''} ${e.message}` };
  }
  out.textContent = JSON.stringify(results, null, 2);
}

const vectors = await (await fetch('../../../../docs/spec/vault-test-vectors.json').catch(() => null))?.json?.().catch(() => null);

await step('vault lifecycle (PBKDF2 600k)', async () => {
  const { params, vaultKey } = await vc.createVault('browser pw');
  const k2 = await vc.unlockVault('browser pw', params);
  const id = vc.uuid();
  const data = await vc.encryptItem(k2, id, { type: 'note', name: 'x' });
  return { same: vc.b64encode(vaultKey) === vc.b64encode(k2), item: (await vc.decryptItem(vaultKey, id, data)).name };
});
await step('TOTP RFC 6238 (SHA1 @59 = 94287082)', async () => (await generateTotp('otpauth://totp/x?secret=GEZDGNBVGY3TQOJQGEZDGNBVGY3TQOJQ&digits=8', 59000)).code);
await step('Argon2d 64 MiB t=2 p=2', async () => (await argon2({ password: 'x', salt: new Uint8Array(32), memory: 65536, iterations: 2, parallelism: 2, type: 'argon2d' })).length);
await step('KDBX4 write/read (defaults + key file)', async () => {
  const keyFile = await generateKeyFile();
  const data = { folders: [{ id: vc.uuid(), name: 'A/B' }], items: [{ folderPath: 'A/B', type: 'login', name: 'Site ✓', login: { username: 'u', password: 'p', uris: [{ uri: 'https://example.com' }] }, fields: [], password_history: [] }] };
  const bytes = await exportVault('kdbx', data, { password: 'pw', keyFile });
  const back = await importVault('kdbx', bytes, { password: 'pw', keyFile });
  return { size: bytes.length, item: back.items[0].name, folder: back.items[0].folderPath };
});
await step('KDBX read with wrong password → bad_credentials', async () => {
  const bytes = await writeKdbx({ root: { name: 'R', entries: [], groups: [] } }, { password: 'a', kdf: { type: 'argon2id', memory: 1048576, iterations: 1, parallelism: 1 } });
  try {
    await readKdbx(bytes, { password: 'b' });
    return 'UNEXPECTED SUCCESS';
  } catch (e) {
    return e.code;
  }
});
await step('Bitwarden encrypted JSON round trip', async () => {
  const text = await exportVault('bitwarden-json', { items: [{ type: 'note', name: 'n', notes: 'secret' }] }, { password: 'file pw', kdfIterations: 5000 });
  return (await importVault('bitwarden-json', text, { password: 'file pw' })).items[0].notes;
});
await step('generator / strength / url match', async () => ({
  passphrase: generatePassphrase({ words: 5 }),
  score: estimateStrength('password').score,
  match: matchUri('https://example.com', 'https://www.example.com/login'),
}));
if (vectors) {
  await step('spec vectors unlock', async () => vc.b64encode(await vc.unlockVault(vectors.kdf.password, vectors.wrapped_key.server_params)) === vc.b64encode(new Uint8Array(vectors.wrapped_key.vault_key_hex.match(/../g).map((h) => parseInt(h, 16)))));
}
out.dataset.done = Object.values(results).every((r) => r.ok) ? 'pass' : 'fail';
document.title = `smoke: ${out.dataset.done}`;
