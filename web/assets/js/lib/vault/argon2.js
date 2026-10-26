/**
 * Argon2d / Argon2i / Argon2id (RFC 9106), versions 0x13 and 0x10 (the latter still appears in old KDBX
 * files). Pure JS: 64-bit words are (lo, hi) uint32 pairs in one big Uint32Array.
 *
 * argon2Sync blocks the calling thread; argon2 (async) yields to the event loop between segments so a
 * UI can stay responsive and report progress. For large memory costs prefer running it in a Worker.
 */
import { VaultError, invalidArg } from './errors.js';
import { Blake2b, blake2b } from './blake2b.js';

export const ARGON2D = 0;
export const ARGON2I = 1;
export const ARGON2ID = 2;
const TYPE_NAMES = { argon2d: ARGON2D, argon2i: ARGON2I, argon2id: ARGON2ID };

const SYNC_POINTS = 4;
const BLOCK_WORDS = 256; // 1024-byte block as uint32
const MAX_MEMORY_KIB = 4 * 1024 * 1024; // 4 GiB hard ceiling (typed-array limits)

function le32(n) {
  return new Uint8Array([n & 0xff, (n >>> 8) & 0xff, (n >>> 16) & 0xff, (n >>> 24) & 0xff]);
}

/** Variable-length hash H' (RFC 9106 §3.3). */
function hPrime(parts, outLen) {
  const first = new Blake2b(Math.min(outLen, 64)).update(le32(outLen));
  for (const p of parts) first.update(p);
  if (outLen <= 64) return first.digest();
  const out = new Uint8Array(outLen);
  let vv = first.digest();
  out.set(vv.subarray(0, 32), 0);
  let pos = 32;
  const r = Math.ceil(outLen / 32) - 2;
  for (let i = 2; i <= r; i++) {
    vv = blake2b(vv, 64);
    out.set(vv.subarray(0, 32), pos);
    pos += 32;
  }
  out.set(blake2b(vv, outLen - 32 * r), pos);
  return out;
}

// GB index table for the permutation P applied to the 8 rows then the 8 columns of a block.
const IDX = (() => {
  const t = new Int32Array(16 * 8 * 4);
  const pattern = [0, 4, 8, 12, 1, 5, 9, 13, 2, 6, 10, 14, 3, 7, 11, 15, 0, 5, 10, 15, 1, 6, 11, 12, 2, 7, 8, 13, 3, 4, 9, 14];
  let k = 0;
  for (let round = 0; round < 16; round++) {
    const w = new Array(16);
    const i = round & 7;
    for (let j = 0; j < 16; j++) {
      // rows: 64-bit words 16i..16i+15; columns: registers i, i+8, ..., i+56 (two words each)
      w[j] = round < 8 ? 32 * i + 2 * j : 4 * i + 32 * (j >> 1) + 2 * (j & 1);
    }
    for (const p of pattern) t[k++] = w[p];
  }
  return t;
})();

/**
 * Argon2 G mixing on uint32 pairs with the BlaMka multiply: a = a + b + 2*lo(a)*lo(b) (mod 2^64).
 * Written out with locals because this is the hot loop.
 */
function gb(v, a, b, c, d) {
  let al = v[a];
  let ah = v[a + 1];
  let bl = v[b];
  let bh = v[b + 1];
  let cl = v[c];
  let ch = v[c + 1];
  let dl = v[d];
  let dh = v[d + 1];
  let x16, x0, y16, y0, t0, t1, t2, ph, pl, lo, xl, xh;

  // a = fBlaMka(a, b)
  x16 = al >>> 16; x0 = al & 0xffff; y16 = bl >>> 16; y0 = bl & 0xffff;
  t0 = x0 * y0; t1 = x16 * y0 + (t0 >>> 16); t2 = x0 * y16 + (t1 & 0xffff);
  ph = x16 * y16 + (t1 >>> 16) + (t2 >>> 16); pl = ((t2 & 0xffff) << 16) | (t0 & 0xffff);
  ph = ((ph << 1) | (pl >>> 31)) >>> 0; pl = (pl << 1) >>> 0;
  lo = al + bl + pl; al = lo >>> 0; ah = (ah + bh + ph + (lo - al) / 4294967296) >>> 0;
  // d = rotr64(d ^ a, 32)
  xl = (dh ^ ah) >>> 0; dh = (dl ^ al) >>> 0; dl = xl;
  // c = fBlaMka(c, d)
  x16 = cl >>> 16; x0 = cl & 0xffff; y16 = dl >>> 16; y0 = dl & 0xffff;
  t0 = x0 * y0; t1 = x16 * y0 + (t0 >>> 16); t2 = x0 * y16 + (t1 & 0xffff);
  ph = x16 * y16 + (t1 >>> 16) + (t2 >>> 16); pl = ((t2 & 0xffff) << 16) | (t0 & 0xffff);
  ph = ((ph << 1) | (pl >>> 31)) >>> 0; pl = (pl << 1) >>> 0;
  lo = cl + dl + pl; cl = lo >>> 0; ch = (ch + dh + ph + (lo - cl) / 4294967296) >>> 0;
  // b = rotr64(b ^ c, 24)
  xl = bl ^ cl; xh = bh ^ ch;
  bl = ((xl >>> 24) | (xh << 8)) >>> 0; bh = ((xh >>> 24) | (xl << 8)) >>> 0;
  // a = fBlaMka(a, b)
  x16 = al >>> 16; x0 = al & 0xffff; y16 = bl >>> 16; y0 = bl & 0xffff;
  t0 = x0 * y0; t1 = x16 * y0 + (t0 >>> 16); t2 = x0 * y16 + (t1 & 0xffff);
  ph = x16 * y16 + (t1 >>> 16) + (t2 >>> 16); pl = ((t2 & 0xffff) << 16) | (t0 & 0xffff);
  ph = ((ph << 1) | (pl >>> 31)) >>> 0; pl = (pl << 1) >>> 0;
  lo = al + bl + pl; al = lo >>> 0; ah = (ah + bh + ph + (lo - al) / 4294967296) >>> 0;
  // d = rotr64(d ^ a, 16)
  xl = dl ^ al; xh = dh ^ ah;
  dl = ((xl >>> 16) | (xh << 16)) >>> 0; dh = ((xh >>> 16) | (xl << 16)) >>> 0;
  // c = fBlaMka(c, d)
  x16 = cl >>> 16; x0 = cl & 0xffff; y16 = dl >>> 16; y0 = dl & 0xffff;
  t0 = x0 * y0; t1 = x16 * y0 + (t0 >>> 16); t2 = x0 * y16 + (t1 & 0xffff);
  ph = x16 * y16 + (t1 >>> 16) + (t2 >>> 16); pl = ((t2 & 0xffff) << 16) | (t0 & 0xffff);
  ph = ((ph << 1) | (pl >>> 31)) >>> 0; pl = (pl << 1) >>> 0;
  lo = cl + dl + pl; cl = lo >>> 0; ch = (ch + dh + ph + (lo - cl) / 4294967296) >>> 0;
  // b = rotr64(b ^ c, 63)
  xl = bl ^ cl; xh = bh ^ ch;
  bl = ((xh >>> 31) | (xl << 1)) >>> 0; bh = ((xl >>> 31) | (xh << 1)) >>> 0;

  v[a] = al; v[a + 1] = ah; v[b] = bl; v[b + 1] = bh;
  v[c] = cl; v[c + 1] = ch; v[d] = dl; v[d + 1] = dh;
}

const R = new Uint32Array(BLOCK_WORDS);
const T = new Uint32Array(BLOCK_WORDS);
const ZERO = new Uint32Array(BLOCK_WORDS);

/** Compression G: out = P(X ^ Y) ^ X ^ Y (^ out when withXor, version 0x13 passes > 0). */
function fillBlock(xa, xo, ya, yo, outA, outO, withXor) {
  if (withXor) {
    for (let i = 0; i < BLOCK_WORDS; i++) {
      const r = xa[xo + i] ^ ya[yo + i];
      R[i] = r;
      T[i] = r ^ outA[outO + i];
    }
  } else {
    for (let i = 0; i < BLOCK_WORDS; i++) {
      const r = xa[xo + i] ^ ya[yo + i];
      R[i] = r;
      T[i] = r;
    }
  }
  for (let k = 0; k < IDX.length; k += 4) gb(R, IDX[k], IDX[k + 1], IDX[k + 2], IDX[k + 3]);
  for (let i = 0; i < BLOCK_WORDS; i++) outA[outO + i] = T[i] ^ R[i];
}

/** High 32 bits of the 64-bit product of two uint32 values. */
function mulhi(a, b) {
  const a16 = a >>> 16;
  const a0 = a & 0xffff;
  const b16 = b >>> 16;
  const b0 = b & 0xffff;
  const t0 = a0 * b0;
  const t1 = a16 * b0 + (t0 >>> 16);
  const t2 = a0 * b16 + (t1 & 0xffff);
  return a16 * b16 + (t1 >>> 16) + (t2 >>> 16);
}

function toBytes(v, what, required) {
  if (v == null) {
    if (required) throw invalidArg(`argon2: ${what} is required`);
    return new Uint8Array(0);
  }
  if (typeof v === 'string') return new TextEncoder().encode(v);
  if (v instanceof Uint8Array) return v;
  throw invalidArg(`argon2: ${what} must be a Uint8Array or string`);
}

function normalize(opts) {
  const type = typeof opts.type === 'string' ? TYPE_NAMES[opts.type.toLowerCase()] : opts.type ?? ARGON2ID;
  if (type !== ARGON2D && type !== ARGON2I && type !== ARGON2ID) throw invalidArg('argon2: unknown type');
  const version = opts.version ?? 0x13;
  if (version !== 0x13 && version !== 0x10) throw new VaultError('unsupported', 'argon2: version must be 0x10 or 0x13');
  const p = opts.parallelism ?? 1;
  const t = opts.iterations ?? 3;
  const m = opts.memory ?? 65536;
  const hashLength = opts.hashLength ?? 32;
  if (!Number.isInteger(p) || p < 1 || p > 0xffffff) throw invalidArg('argon2: parallelism must be 1..2^24-1');
  if (!Number.isInteger(t) || t < 1 || t > 0xffffffff) throw invalidArg('argon2: iterations must be ≥ 1');
  if (!Number.isInteger(m) || m < 8 * p) throw invalidArg('argon2: memory (KiB) must be ≥ 8 × parallelism');
  if (m > MAX_MEMORY_KIB) throw new VaultError('limit_exceeded', 'argon2: memory exceeds 4 GiB');
  if (!Number.isInteger(hashLength) || hashLength < 4 || hashLength > 0xffffffff) throw invalidArg('argon2: hashLength must be ≥ 4');
  const password = toBytes(opts.password, 'password', true);
  const salt = toBytes(opts.salt, 'salt', true);
  if (salt.length < 8) throw invalidArg('argon2: salt must be at least 8 bytes');
  return { type, version, p, t, m, hashLength, password, salt, secret: toBytes(opts.secret), ad: toBytes(opts.ad) };
}

function* run(o) {
  const { type, version, p, t, m, hashLength } = o;
  const h0 = new Blake2b(64);
  for (const n of [p, hashLength, m, t, version, type]) h0.update(le32(n));
  for (const part of [o.password, o.salt, o.secret, o.ad]) h0.update(le32(part.length)).update(part);
  const H0 = h0.digest();

  const segLen = Math.floor(Math.max(m, 2 * SYNC_POINTS * p) / (p * SYNC_POINTS));
  const laneLen = segLen * SYNC_POINTS;
  const blocks = laneLen * p;
  let mem;
  try {
    mem = new Uint32Array(blocks * BLOCK_WORDS);
  } catch (e) {
    throw new VaultError('limit_exceeded', 'argon2: could not allocate memory', { cause: e });
  }

  const loadBlock = (bytes, blockIndex) => {
    const base = blockIndex * BLOCK_WORDS;
    for (let i = 0; i < BLOCK_WORDS; i++) {
      const q = i * 4;
      mem[base + i] = bytes[q] | (bytes[q + 1] << 8) | (bytes[q + 2] << 16) | (bytes[q + 3] << 24);
    }
  };
  for (let l = 0; l < p; l++) {
    loadBlock(hPrime([H0, le32(0), le32(l)], 1024), l * laneLen);
    loadBlock(hPrime([H0, le32(1), le32(l)], 1024), l * laneLen + 1);
  }

  const input = new Uint32Array(BLOCK_WORDS);
  const address = new Uint32Array(BLOCK_WORDS);
  const nextAddresses = () => {
    input[12]++; // 64-bit counter at word 6 (never exceeds 2^32 in practice)
    fillBlock(ZERO, 0, input, 0, address, 0, false);
    fillBlock(ZERO, 0, address, 0, address, 0, false);
  };

  const total = t * SYNC_POINTS * p;
  let doneSegments = 0;
  for (let pass = 0; pass < t; pass++) {
    for (let slice = 0; slice < SYNC_POINTS; slice++) {
      for (let lane = 0; lane < p; lane++) {
        const dataIndependent = type === ARGON2I || (type === ARGON2ID && pass === 0 && slice < 2);
        let start = 0;
        if (dataIndependent) {
          input.fill(0);
          input[0] = pass;
          input[2] = lane;
          input[4] = slice;
          input[6] = blocks;
          input[8] = t;
          input[10] = type;
        }
        if (pass === 0 && slice === 0) {
          start = 2;
          if (dataIndependent) nextAddresses();
        }
        let curr = lane * laneLen + slice * segLen + start;
        let prev = curr % laneLen === 0 ? curr + laneLen - 1 : curr - 1;
        const withXor = version === 0x13 && pass !== 0;
        for (let i = start; i < segLen; i++, curr++, prev++) {
          if (curr % laneLen === 1) prev = curr - 1;
          let j1;
          let j2;
          if (dataIndependent) {
            const k = i % 128;
            if (k === 0) nextAddresses();
            j1 = address[2 * k];
            j2 = address[2 * k + 1];
          } else {
            j1 = mem[prev * BLOCK_WORDS];
            j2 = mem[prev * BLOCK_WORDS + 1];
          }
          const refLane = pass === 0 && slice === 0 ? lane : j2 % p;
          const sameLane = refLane === lane;
          let area;
          if (pass === 0) {
            if (slice === 0) area = i - 1;
            else area = sameLane ? slice * segLen + i - 1 : slice * segLen + (i === 0 ? -1 : 0);
          } else {
            area = sameLane ? laneLen - segLen + i - 1 : laneLen - segLen + (i === 0 ? -1 : 0);
          }
          const rel = area - 1 - mulhi(area, mulhi(j1, j1));
          const startPos = pass === 0 || slice === SYNC_POINTS - 1 ? 0 : (slice + 1) * segLen;
          const ref = refLane * laneLen + ((startPos + rel) % laneLen);
          fillBlock(mem, prev * BLOCK_WORDS, mem, ref * BLOCK_WORDS, mem, curr * BLOCK_WORDS, withXor);
        }
        doneSegments++;
        yield doneSegments / total;
      }
    }
  }

  const final = new Uint32Array(BLOCK_WORDS);
  for (let l = 0; l < p; l++) {
    const base = (l * laneLen + laneLen - 1) * BLOCK_WORDS;
    for (let i = 0; i < BLOCK_WORDS; i++) final[i] ^= mem[base + i];
  }
  mem.fill(0);
  const fb = new Uint8Array(1024);
  for (let i = 0; i < BLOCK_WORDS; i++) {
    const w = final[i];
    fb[i * 4] = w;
    fb[i * 4 + 1] = w >>> 8;
    fb[i * 4 + 2] = w >>> 16;
    fb[i * 4 + 3] = w >>> 24;
  }
  return hPrime([fb], hashLength);
}

/**
 * @typedef {object} Argon2Options
 * @property {Uint8Array|string} password
 * @property {Uint8Array|string} salt          ≥ 8 bytes
 * @property {Uint8Array} [secret]             optional key K
 * @property {Uint8Array} [ad]                 optional associated data X
 * @property {number} [memory=65536]           KiB
 * @property {number} [iterations=3]
 * @property {number} [parallelism=1]
 * @property {number} [hashLength=32]
 * @property {'argon2d'|'argon2i'|'argon2id'|0|1|2} [type='argon2id']
 * @property {0x10|0x13} [version=0x13]
 */

/**
 * Synchronous Argon2.
 * @param {Argon2Options} opts
 * @returns {Uint8Array}
 */
export function argon2Sync(opts) {
  const it = run(normalize(opts));
  for (;;) {
    const r = it.next();
    if (r.done) return r.value;
  }
}

/**
 * Asynchronous Argon2: same result as argon2Sync, but yields to the event loop roughly every 40 ms.
 * @param {Argon2Options & {onProgress?: (fraction:number)=>void}} opts
 * @returns {Promise<Uint8Array>}
 */
export async function argon2(opts) {
  const it = run(normalize(opts));
  let last = Date.now();
  for (;;) {
    const r = it.next();
    if (r.done) return r.value;
    if (opts.onProgress) opts.onProgress(r.value);
    if (Date.now() - last > 40) {
      await new Promise((res) => setTimeout(res, 0));
      last = Date.now();
    }
  }
}
