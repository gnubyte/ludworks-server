/**
 * Salsa20/20 stream cipher (256-bit key, 64-bit nonce, 64-bit block counter). KDBX 3.x uses it as the
 * inner random stream (id 2) with key = SHA-256(ProtectedStreamKey) and the fixed nonce below.
 */
import { invalidArg } from './errors.js';

export const KDBX_SALSA20_NONCE = new Uint8Array([0xe8, 0x30, 0x09, 0x4b, 0x97, 0x20, 0x5d, 0x2a]);
const SIGMA = [0x61707865, 0x3320646e, 0x79622d32, 0x6b206574];

function rd32(b, o) {
  return (b[o] | (b[o + 1] << 8) | (b[o + 2] << 16) | (b[o + 3] << 24)) >>> 0;
}

function rotl(v, c) {
  return (v << c) | (v >>> (32 - c));
}

function block(s, x, out) {
  x.set(s);
  for (let i = 0; i < 10; i++) {
    // column round
    x[4] ^= rotl(x[0] + x[12], 7); x[8] ^= rotl(x[4] + x[0], 9);
    x[12] ^= rotl(x[8] + x[4], 13); x[0] ^= rotl(x[12] + x[8], 18);
    x[9] ^= rotl(x[5] + x[1], 7); x[13] ^= rotl(x[9] + x[5], 9);
    x[1] ^= rotl(x[13] + x[9], 13); x[5] ^= rotl(x[1] + x[13], 18);
    x[14] ^= rotl(x[10] + x[6], 7); x[2] ^= rotl(x[14] + x[10], 9);
    x[6] ^= rotl(x[2] + x[14], 13); x[10] ^= rotl(x[6] + x[2], 18);
    x[3] ^= rotl(x[15] + x[11], 7); x[7] ^= rotl(x[3] + x[15], 9);
    x[11] ^= rotl(x[7] + x[3], 13); x[15] ^= rotl(x[11] + x[7], 18);
    // row round
    x[1] ^= rotl(x[0] + x[3], 7); x[2] ^= rotl(x[1] + x[0], 9);
    x[3] ^= rotl(x[2] + x[1], 13); x[0] ^= rotl(x[3] + x[2], 18);
    x[6] ^= rotl(x[5] + x[4], 7); x[7] ^= rotl(x[6] + x[5], 9);
    x[4] ^= rotl(x[7] + x[6], 13); x[5] ^= rotl(x[4] + x[7], 18);
    x[11] ^= rotl(x[10] + x[9], 7); x[8] ^= rotl(x[11] + x[10], 9);
    x[9] ^= rotl(x[8] + x[11], 13); x[10] ^= rotl(x[9] + x[8], 18);
    x[12] ^= rotl(x[15] + x[14], 7); x[13] ^= rotl(x[12] + x[15], 9);
    x[14] ^= rotl(x[13] + x[12], 13); x[15] ^= rotl(x[14] + x[13], 18);
  }
  for (let i = 0; i < 16; i++) {
    const w = (x[i] + s[i]) >>> 0;
    out[i * 4] = w;
    out[i * 4 + 1] = w >>> 8;
    out[i * 4 + 2] = w >>> 16;
    out[i * 4 + 3] = w >>> 24;
  }
}

/** Stateful Salsa20 keystream. */
export class Salsa20 {
  /**
   * @param {Uint8Array} key 32 bytes
   * @param {Uint8Array} nonce 8 bytes
   */
  constructor(key, nonce) {
    if (!(key instanceof Uint8Array) || key.length !== 32) throw invalidArg('Salsa20 key must be 32 bytes');
    if (!(nonce instanceof Uint8Array) || nonce.length !== 8) throw invalidArg('Salsa20 nonce must be 8 bytes');
    const s = new Uint32Array(16);
    s[0] = SIGMA[0];
    s[5] = SIGMA[1];
    s[10] = SIGMA[2];
    s[15] = SIGMA[3];
    for (let i = 0; i < 4; i++) {
      s[1 + i] = rd32(key, i * 4);
      s[11 + i] = rd32(key, 16 + i * 4);
    }
    s[6] = rd32(nonce, 0);
    s[7] = rd32(nonce, 4);
    // s[8], s[9] = 64-bit block counter (little-endian), starts at 0
    this.s = s;
    this.x = new Uint32Array(16);
    this.ks = new Uint8Array(64);
    this.ksPos = 64;
  }

  _refill() {
    block(this.s, this.x, this.ks);
    this.ksPos = 0;
    this.s[8]++;
    if (this.s[8] === 0) this.s[9]++;
  }

  /** XOR data with the keystream. @param {Uint8Array} data @returns {Uint8Array} */
  process(data) {
    const out = new Uint8Array(data.length);
    let i = 0;
    while (i < data.length) {
      if (this.ksPos === 64) this._refill();
      const take = Math.min(64 - this.ksPos, data.length - i);
      for (let j = 0; j < take; j++) out[i + j] = data[i + j] ^ this.ks[this.ksPos + j];
      this.ksPos += take;
      i += take;
    }
    return out;
  }

  /** @param {number} n */
  keystream(n) {
    return this.process(new Uint8Array(n));
  }
}
