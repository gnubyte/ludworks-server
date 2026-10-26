/**
 * BLAKE2b (RFC 7693), keyed and variable output length (1..64 bytes). 64-bit words are held as
 * little-endian (lo, hi) uint32 pairs because JS has no fast 64-bit integers.
 */
import { invalidArg } from './errors.js';

const IV32 = new Uint32Array([
  0xf3bcc908, 0x6a09e667, 0x84caa73b, 0xbb67ae85, 0xfe94f82b, 0x3c6ef372, 0x5f1d36f1, 0xa54ff53a,
  0xade682d1, 0x510e527f, 0x2b3e6c1f, 0x9b05688c, 0xfb41bd6b, 0x1f83d9ab, 0x137e2179, 0x5be0cd19,
]);

const SIGMA = [
  [0, 1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15],
  [14, 10, 4, 8, 9, 15, 13, 6, 1, 12, 0, 2, 11, 7, 5, 3],
  [11, 8, 12, 0, 5, 2, 15, 13, 10, 14, 3, 6, 7, 1, 9, 4],
  [7, 9, 3, 1, 13, 12, 11, 14, 2, 6, 5, 10, 4, 0, 15, 8],
  [9, 0, 5, 7, 2, 4, 10, 15, 14, 1, 11, 12, 6, 8, 3, 13],
  [2, 12, 6, 10, 0, 11, 8, 3, 4, 13, 7, 5, 15, 14, 1, 9],
  [12, 5, 1, 15, 14, 13, 4, 10, 0, 7, 6, 3, 9, 2, 8, 11],
  [13, 11, 7, 14, 12, 1, 3, 9, 5, 0, 15, 4, 8, 6, 2, 10],
  [6, 15, 14, 9, 11, 3, 0, 8, 12, 2, 13, 7, 1, 4, 10, 5],
  [10, 2, 8, 4, 7, 6, 1, 5, 15, 11, 9, 14, 3, 12, 13, 0],
];
// 12 rounds; rounds 10 and 11 reuse permutations 0 and 1. Pre-doubled for uint32-pair indexing.
const SIGMA2 = new Uint8Array(12 * 16);
for (let r = 0; r < 12; r++) for (let i = 0; i < 16; i++) SIGMA2[r * 16 + i] = SIGMA[r % 10][i] * 2;

const v = new Uint32Array(32);
const m = new Uint32Array(32);

// v[a] += v[b] (64-bit)
function add64aa(a, b) {
  const lo = v[a] + v[b];
  v[a + 1] = v[a + 1] + v[b + 1] + (lo >= 0x100000000 ? 1 : 0);
  v[a] = lo;
}
// v[a] += (b0, b1) (64-bit)
function add64ac(a, b0, b1) {
  const lo = v[a] + b0;
  v[a + 1] = v[a + 1] + b1 + (lo >= 0x100000000 ? 1 : 0);
  v[a] = lo;
}

function g(a, b, c, d, ix, iy) {
  add64aa(a, b);
  add64ac(a, m[ix], m[ix + 1]);
  let xl = v[d] ^ v[a];
  let xh = v[d + 1] ^ v[a + 1];
  v[d] = xh; // rotr 32
  v[d + 1] = xl;
  add64aa(c, d);
  xl = v[b] ^ v[c];
  xh = v[b + 1] ^ v[c + 1];
  v[b] = (xl >>> 24) | (xh << 8); // rotr 24
  v[b + 1] = (xh >>> 24) | (xl << 8);
  add64aa(a, b);
  add64ac(a, m[iy], m[iy + 1]);
  xl = v[d] ^ v[a];
  xh = v[d + 1] ^ v[a + 1];
  v[d] = (xl >>> 16) | (xh << 16); // rotr 16
  v[d + 1] = (xh >>> 16) | (xl << 16);
  add64aa(c, d);
  xl = v[b] ^ v[c];
  xh = v[b + 1] ^ v[c + 1];
  v[b] = (xh >>> 31) | (xl << 1); // rotr 63
  v[b + 1] = (xl >>> 31) | (xh << 1);
}

function compress(h, block, off, t, last) {
  for (let i = 0; i < 16; i++) {
    v[i] = h[i];
    v[i + 16] = IV32[i];
  }
  v[24] ^= t >>> 0;
  v[25] ^= Math.floor(t / 0x100000000);
  if (last) {
    v[28] = ~v[28];
    v[29] = ~v[29];
  }
  for (let i = 0; i < 32; i++) {
    const p = off + i * 4;
    m[i] = block[p] | (block[p + 1] << 8) | (block[p + 2] << 16) | (block[p + 3] << 24);
  }
  for (let r = 0; r < 12; r++) {
    const s = r * 16;
    g(0, 8, 16, 24, SIGMA2[s], SIGMA2[s + 1]);
    g(2, 10, 18, 26, SIGMA2[s + 2], SIGMA2[s + 3]);
    g(4, 12, 20, 28, SIGMA2[s + 4], SIGMA2[s + 5]);
    g(6, 14, 22, 30, SIGMA2[s + 6], SIGMA2[s + 7]);
    g(0, 10, 20, 30, SIGMA2[s + 8], SIGMA2[s + 9]);
    g(2, 12, 22, 24, SIGMA2[s + 10], SIGMA2[s + 11]);
    g(4, 14, 16, 26, SIGMA2[s + 12], SIGMA2[s + 13]);
    g(6, 8, 18, 28, SIGMA2[s + 14], SIGMA2[s + 15]);
  }
  for (let i = 0; i < 16; i++) h[i] ^= v[i] ^ v[i + 16];
}

/** Incremental BLAKE2b. */
export class Blake2b {
  /**
   * @param {number} [outLen=64] digest length in bytes (1..64)
   * @param {Uint8Array} [key] optional key (0..64 bytes)
   */
  constructor(outLen = 64, key) {
    if (!Number.isInteger(outLen) || outLen < 1 || outLen > 64) throw invalidArg('BLAKE2b output length must be 1..64');
    const keyLen = key ? key.length : 0;
    if (keyLen > 64) throw invalidArg('BLAKE2b key must be at most 64 bytes');
    this.outLen = outLen;
    this.h = new Uint32Array(IV32);
    this.h[0] ^= 0x01010000 ^ (keyLen << 8) ^ outLen;
    this.buf = new Uint8Array(128);
    this.bufLen = 0;
    this.t = 0;
    this.done = false;
    if (keyLen > 0) {
      this.update(key);
      this.bufLen = 128; // key block is always a full (zero-padded) block
    }
  }

  /** @param {Uint8Array} data */
  update(data) {
    if (this.done) throw new Error('BLAKE2b: update after digest');
    for (let i = 0; i < data.length; ) {
      if (this.bufLen === 128) {
        this.t += 128;
        compress(this.h, this.buf, 0, this.t, false);
        this.bufLen = 0;
      }
      // Fast path: compress directly from input while more than one full block remains.
      if (this.bufLen === 0 && data.length - i > 128) {
        this.t += 128;
        compress(this.h, data, i, this.t, false);
        i += 128;
        continue;
      }
      const n = Math.min(128 - this.bufLen, data.length - i);
      this.buf.set(data.subarray(i, i + n), this.bufLen);
      this.bufLen += n;
      i += n;
    }
    return this;
  }

  /** @returns {Uint8Array} */
  digest() {
    if (this.done) throw new Error('BLAKE2b: digest called twice');
    this.done = true;
    this.t += this.bufLen;
    this.buf.fill(0, this.bufLen);
    compress(this.h, this.buf, 0, this.t, true);
    const out = new Uint8Array(this.outLen);
    for (let i = 0; i < this.outLen; i++) out[i] = this.h[i >> 2] >>> (8 * (i & 3));
    return out;
  }
}

/**
 * One-shot BLAKE2b.
 * @param {Uint8Array} data
 * @param {number} [outLen=64]
 * @param {Uint8Array} [key]
 * @returns {Uint8Array}
 */
export function blake2b(data, outLen = 64, key) {
  return new Blake2b(outLen, key).update(data).digest();
}
