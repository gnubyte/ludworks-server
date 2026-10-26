// Test-only KDBX 3.1 writer (the library only writes KDBX 4). Produces files the way KeePass 2.x did:
// AES-256-CBC + AES-KDF, StreamStartBytes, SHA-256 hashed block stream, gzip, Salsa20 inner stream,
// Meta/HeaderHash, Meta/Binaries.
import { __testing as kdbxInternals, compositeKey } from '../../../assets/js/lib/vault/kdbx.js';
import { treeToXml } from '../../../assets/js/lib/vault/keepass-xml.js';
import { serializeXml } from '../../../assets/js/lib/vault/xml.js';
import { b64encode, concatBytes, fromHex, gzip, randomBytes, sha256, u32le, u64le, utf8Encode } from '../../../assets/js/lib/vault/bytes.js';

const { aesKdf, protectXml, innerStream, STREAM_SALSA20, CIPHER_AES256 } = kdbxInternals;

function field(id, data) {
  const b = new Uint8Array(3 + data.length);
  b[0] = id;
  new DataView(b.buffer).setUint16(1, data.length, true);
  b.set(data, 3);
  return b;
}

/**
 * @param {object} tree KdbxTree
 * @param {{password?: string, keyFile?: Uint8Array, rounds?: number, compression?: boolean, blockSize?: number,
 *          tamper?: 'headerHash'|'block'}} opts
 */
export async function writeKdbx3(tree, { password, keyFile, rounds = 6000, compression = true, blockSize = 4096, tamper } = {}) {
  const masterSeed = randomBytes(32);
  const transformSeed = randomBytes(32);
  const iv = randomBytes(16);
  const streamKey = randomBytes(32);
  const startBytes = randomBytes(32);
  const sig = new Uint8Array(12);
  const dv = new DataView(sig.buffer);
  dv.setUint32(0, 0x9aa2d903, true);
  dv.setUint32(4, 0xb54bfb67, true);
  dv.setUint16(8, 1, true); // minor 1
  dv.setUint16(10, 3, true); // major 3
  const header = concatBytes(
    sig,
    field(2, fromHex(CIPHER_AES256)),
    field(3, u32le(compression ? 1 : 0)),
    field(4, masterSeed),
    field(5, transformSeed),
    field(6, u64le(rounds)),
    field(7, iv),
    field(8, streamKey),
    field(9, startBytes),
    field(10, u32le(STREAM_SALSA20)),
    field(0, utf8Encode('\r\n\r\n')),
  );
  let headerHash = b64encode(await sha256(header));
  if (tamper === 'headerHash') headerHash = b64encode(new Uint8Array(32));
  const doc = treeToXml(tree, { mode: 'kdbx3', headerHash });
  protectXml(doc, await innerStream(STREAM_SALSA20, streamKey));
  let payload = utf8Encode(serializeXml(doc));
  if (compression) payload = await gzip(payload);

  const blocks = [startBytes];
  let index = 0;
  for (let off = 0; off < payload.length; off += blockSize, index++) {
    const data = payload.slice(off, off + blockSize);
    const hash = await sha256(data);
    if (tamper === 'block' && index === 0) data[0] ^= 1;
    blocks.push(u32le(index), hash, u32le(data.length), data);
  }
  blocks.push(u32le(index), new Uint8Array(32), u32le(0));

  const transformed = await aesKdf(await compositeKey(password, keyFile), transformSeed, rounds);
  const masterKey = await sha256(concatBytes(masterSeed, transformed));
  const k = await globalThis.crypto.subtle.importKey('raw', masterKey, { name: 'AES-CBC' }, false, ['encrypt']);
  const ct = new Uint8Array(await globalThis.crypto.subtle.encrypt({ name: 'AES-CBC', iv }, k, concatBytes(...blocks)));
  return concatBytes(header, ct);
}
