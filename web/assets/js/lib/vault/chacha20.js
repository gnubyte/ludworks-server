/**
 * ChaCha20 stream cipher (RFC 8439: 256-bit key, 96-bit nonce, 32-bit block counter). Used by KDBX 4
 * as the outer cipher (12-byte IV from the header, counter starting at 0) and as inner random stream
 * id 3 (key/nonce from SHA-512 of the inner stream key).
 */
import { invalidArg } from './errors.js';

const SIGMA = [0x61707865, 0x3320646e, 0x79622d32, 0x6b206574];

function rd32(b, o) {
  return (b[o] | (b[o + 1] << 8) | (b[o + 2] << 16) | (b[o + 3] << 24)) >>> 0;
}

/**
 * Compute one 64-byte keystream block into `out`.
 * @param {Uint32Array} state 16-word input state
 * @param {Uint32Array} x scratch
 * @param {Uint8Array} out
 */
function block(state, x, out) {
  x.set(state);
  for (let i = 0; i < 10; i++) {
    qr(x, 0, 4, 8, 12);
    qr(x, 1, 5, 9, 13);
    qr(x, 2, 6, 10, 14);
    qr(x, 3, 7, 11, 15);
    qr(x, 0, 5, 10, 15);
    qr(x, 1, 6, 11, 12);
    qr(x, 2, 7, 8, 13);
    qr(x, 3, 4, 9, 14);
  }
  for (let i = 0; i < 16; i++) {
    const w = (x[i] + state[i]) >>> 0;
    out[i * 4] = w;
    out[i * 4 + 1] = w >>> 8;
    out[i * 4 + 2] = w >>> 16;
    out[i * 4 + 3] = w >>> 24;
  }
}

function qr(x, a, b, c, d) {
  let t;
  x[a] += x[b]; t = x[d] ^ x[a]; x[d] = (t << 16) | (t >>> 16);
  x[c] += x[d]; t = x[b] ^ x[c]; x[b] = (t << 12) | (t >>> 20);
  x[a] += x[b]; t = x[d] ^ x[a]; x[d] = (t << 8) | (t >>> 24);
  x[c] += x[d]; t = x[b] ^ x[c]; x[b] = (t << 7) | (t >>> 25);
}

/** Stateful ChaCha20 keystream: successive calls continue where the previous one stopped. */
export class ChaCha20 {
  /**
   * @param {Uint8Array} key 32 bytes
   * @param {Uint8Array} nonce 12 bytes
   * @param {number} [counter=0] initial 32-bit block counter
   */
  constructor(key, nonce, counter = 0) {
    if (!(key instanceof Uint8Array) || key.length !== 32) throw invalidArg('ChaCha20 key must be 32 bytes');
    if (!(nonce instanceof Uint8Array) || nonce.length !== 12) throw invalidArg('ChaCha20 nonce must be 12 bytes');
    if (!Number.isInteger(counter) || counter < 0 || counter > 0xffffffff) throw invalidArg('ChaCha20 counter out of range');
    this.state = new Uint32Array(16);
    this.state.set(SIGMA, 0);
    for (let i = 0; i < 8; i++) this.state[4 + i] = rd32(key, i * 4);
    this.state[12] = counter;
    for (let i = 0; i < 3; i++) this.state[13 + i] = rd32(nonce, i * 4);
    this.x = new Uint32Array(16);
    this.ks = new Uint8Array(64);
    this.ksPos = 64;
    this.exhausted = false;
  }

  _refill() {
    if (this.exhausted) throw new Error('ChaCha20 block counter exhausted');
    block(this.state, this.x, this.ks);
    this.ksPos = 0;
    if (this.state[12] === 0xffffffff) this.exhausted = true;
    else this.state[12]++;
  }

  /**
   * XOR data with the keystream (encrypt == decrypt). Returns a new array.
   * @param {Uint8Array} data
   * @returns {Uint8Array}
   */
  process(data) {
    const out = new Uint8Array(data.length);
    let i = 0;
    const n = data.length;
    while (i < n) {
      if (this.ksPos === 64) this._refill();
      const take = Math.min(64 - this.ksPos, n - i);
      const ks = this.ks;
      let p = this.ksPos;
      for (let j = 0; j < take; j++) out[i + j] = data[i + j] ^ ks[p++];
      this.ksPos = p;
      i += take;
    }
    return out;
  }

  /** Raw keystream bytes. @param {number} n */
  keystream(n) {
    return this.process(new Uint8Array(n));
  }
}

/**
 * One-shot ChaCha20 XOR.
 * @param {Uint8Array} key
 * @param {Uint8Array} nonce
 * @param {Uint8Array} data
 * @param {number} [counter=0]
 */
export function chacha20(key, nonce, data, counter = 0) {
  return new ChaCha20(key, nonce, counter).process(data);
}

/** The raw 64-byte block function output (RFC 8439 §2.3), for tests. */
export function chacha20Block(key, nonce, counter) {
  return new ChaCha20(key, nonce, counter).keystream(64);
}
