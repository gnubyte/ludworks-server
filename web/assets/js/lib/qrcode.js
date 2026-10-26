// QR Code encoder (ISO/IEC 18004:2015) — byte mode, versions 1–40, error
// correction levels L/M/Q/H (default M), automatic mask selection by penalty
// score. Renders to SVG (DOM) or <canvas>. No dependencies; the encoding core is
// pure and unit tested under node.

/** Error correction levels: ordinal (table row) and the 2 format bits. */
const ECL = {
  L: { ord: 0, bits: 1 },
  M: { ord: 1, bits: 0 },
  Q: { ord: 2, bits: 3 },
  H: { ord: 3, bits: 2 },
};

// Error correction codewords per block, indexed [ecl.ord][version] (index 0 unused).
const ECC_CODEWORDS_PER_BLOCK = [
  [-1, 7, 10, 15, 20, 26, 18, 20, 24, 30, 18, 20, 24, 26, 30, 22, 24, 28, 30, 28, 28, 28, 28, 30, 30, 26, 28, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30],
  [-1, 10, 16, 26, 18, 24, 16, 18, 22, 22, 26, 30, 22, 22, 24, 24, 28, 28, 26, 26, 26, 26, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28, 28],
  [-1, 13, 22, 18, 26, 18, 24, 18, 22, 20, 24, 28, 26, 24, 20, 30, 24, 28, 28, 26, 30, 28, 30, 30, 30, 30, 28, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30],
  [-1, 17, 28, 22, 16, 22, 28, 26, 26, 24, 28, 24, 28, 22, 24, 24, 30, 28, 28, 26, 28, 30, 24, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30, 30],
];

// Number of error correction blocks, indexed [ecl.ord][version].
const NUM_ERROR_CORRECTION_BLOCKS = [
  [-1, 1, 1, 1, 1, 1, 2, 2, 2, 2, 4, 4, 4, 4, 4, 6, 6, 6, 6, 7, 8, 8, 9, 9, 10, 12, 12, 12, 13, 14, 15, 16, 17, 18, 19, 19, 20, 21, 22, 24, 25],
  [-1, 1, 1, 1, 2, 2, 4, 4, 4, 5, 5, 5, 8, 9, 9, 10, 10, 11, 13, 14, 16, 17, 17, 18, 20, 21, 23, 25, 26, 28, 29, 31, 33, 35, 37, 38, 40, 43, 45, 47, 49],
  [-1, 1, 1, 2, 2, 4, 4, 6, 6, 8, 8, 8, 10, 12, 16, 12, 17, 16, 18, 21, 20, 23, 23, 25, 27, 29, 34, 34, 35, 38, 40, 43, 45, 48, 51, 53, 56, 59, 62, 65, 68],
  [-1, 1, 1, 2, 4, 4, 4, 5, 6, 8, 8, 11, 11, 16, 16, 18, 16, 19, 21, 25, 25, 25, 34, 30, 32, 35, 37, 40, 42, 45, 48, 51, 54, 57, 60, 63, 66, 70, 74, 77, 81],
];

const PENALTY_N1 = 3;
const PENALTY_N2 = 3;
const PENALTY_N3 = 40;
const PENALTY_N4 = 10;

/* ----------------------------------------------------------- GF(256) / RS */

const EXP = new Uint8Array(512);
const LOG = new Uint8Array(256);
{
  let x = 1;
  for (let i = 0; i < 255; i++) {
    EXP[i] = x;
    LOG[x] = i;
    x <<= 1;
    if (x & 0x100) x ^= 0x11d; // primitive polynomial x^8 + x^4 + x^3 + x^2 + 1
  }
  for (let i = 255; i < 512; i++) EXP[i] = EXP[i - 255];
}

/** Multiply in GF(2^8) mod 0x11D. */
export function gfMul(a, b) {
  return a === 0 || b === 0 ? 0 : EXP[LOG[a] + LOG[b]];
}

/** α^i in GF(256). */
export function gfExp(i) {
  return EXP[((i % 255) + 255) % 255];
}

/**
 * Reed–Solomon generator polynomial ∏(x − α^i), i = 0..degree−1, as coefficients
 * from the highest power (leading 1 included).
 * @param {number} degree
 * @returns {number[]}
 */
export function rsGenerator(degree) {
  let poly = [1];
  for (let i = 0; i < degree; i++) {
    const next = new Array(poly.length + 1).fill(0);
    for (let j = 0; j < poly.length; j++) {
      next[j] ^= poly[j];
      next[j + 1] ^= gfMul(poly[j], EXP[i]);
    }
    poly = next;
  }
  return poly;
}

/**
 * Error correction codewords: remainder of data(x)·x^n divided by the generator.
 * @param {ArrayLike<number>} data
 * @param {number[]} generator from rsGenerator
 * @returns {number[]}
 */
export function rsRemainder(data, generator) {
  const degree = generator.length - 1;
  const rem = new Array(degree).fill(0);
  for (const b of data) {
    const factor = b ^ rem[0];
    rem.shift();
    rem.push(0);
    for (let i = 0; i < degree; i++) rem[i] ^= gfMul(generator[i + 1], factor);
  }
  return rem;
}

/* --------------------------------------------------------- symbol tables */

/** Number of modules available for data+ECC (after function patterns). */
export function numRawDataModules(ver) {
  let result = (16 * ver + 128) * ver + 64;
  if (ver >= 2) {
    const numAlign = Math.floor(ver / 7) + 2;
    result -= (25 * numAlign - 10) * numAlign - 55;
    if (ver >= 7) result -= 36;
  }
  return result;
}

/** Data codewords available for a version/level. */
export function numDataCodewords(ver, ecl) {
  const o = ECL[ecl].ord;
  return Math.floor(numRawDataModules(ver) / 8) - ECC_CODEWORDS_PER_BLOCK[o][ver] * NUM_ERROR_CORRECTION_BLOCKS[o][ver];
}

/** Block structure for a version/level (for tests and the interleaver). */
export function blockInfo(ver, ecl) {
  const o = ECL[ecl].ord;
  return {
    numBlocks: NUM_ERROR_CORRECTION_BLOCKS[o][ver],
    eccPerBlock: ECC_CODEWORDS_PER_BLOCK[o][ver],
    totalCodewords: Math.floor(numRawDataModules(ver) / 8),
  };
}

/** Alignment pattern centre coordinates for a version (ascending). */
export function alignmentPositions(ver) {
  if (ver === 1) return [];
  const numAlign = Math.floor(ver / 7) + 2;
  const size = ver * 4 + 17;
  const step = Math.floor((ver * 8 + numAlign * 3 + 5) / (numAlign * 4 - 4)) * 2;
  const result = [6];
  for (let pos = size - 7; result.length < numAlign; pos -= step) result.splice(1, 0, pos);
  return result;
}

/** 15-bit format information (BCH(15,5), masked with 0x5412). */
export function formatBits(ecl, mask) {
  const data = (ECL[ecl].bits << 3) | mask;
  let rem = data;
  for (let i = 0; i < 10; i++) rem = (rem << 1) ^ ((rem >>> 9) * 0x537);
  return ((data << 10) | rem) ^ 0x5412;
}

/** 18-bit version information (BCH(18,6)) for versions ≥ 7. */
export function versionBits(ver) {
  let rem = ver;
  for (let i = 0; i < 12; i++) rem = (rem << 1) ^ ((rem >>> 11) * 0x1f25);
  return (ver << 12) | rem;
}

/** Mask predicate (x = column, y = row). */
export function maskFn(mask, x, y) {
  switch (mask) {
    case 0: return (x + y) % 2 === 0;
    case 1: return y % 2 === 0;
    case 2: return x % 3 === 0;
    case 3: return (x + y) % 3 === 0;
    case 4: return (Math.floor(x / 3) + Math.floor(y / 2)) % 2 === 0;
    case 5: return ((x * y) % 2) + ((x * y) % 3) === 0;
    case 6: return (((x * y) % 2) + ((x * y) % 3)) % 2 === 0;
    case 7: return (((x + y) % 2) + ((x * y) % 3)) % 2 === 0;
    default: throw new RangeError('mask must be 0..7');
  }
}

const getBit = (v, i) => ((v >>> i) & 1) !== 0;

/* --------------------------------------------------------------- penalty */

/**
 * Mask penalty score per ISO 18004 §7.8.3 (N1 runs, N2 2×2 blocks, N3 finder-like
 * 1:1:3:1:1 patterns with 4 light modules, N4 dark balance). Outside the symbol
 * counts as light (the quiet zone).
 * @param {boolean[][]} m modules[y][x]
 */
export function penaltyScore(m) {
  const n = m.length;
  let result = 0;
  const P1 = [1, 0, 1, 1, 1, 0, 1, 0, 0, 0, 0];
  const P2 = [0, 0, 0, 0, 1, 0, 1, 1, 1, 0, 1];
  const scanLine = (get) => {
    let runColor = get(0);
    let runLen = 1;
    for (let i = 1; i <= n; i++) {
      const c = i < n ? get(i) : !runColor;
      if (i < n && c === runColor) {
        runLen++;
      } else {
        if (runLen >= 5) result += PENALTY_N1 + (runLen - 5);
        runColor = c;
        runLen = 1;
      }
    }
    const line = [0, 0, 0, 0];
    for (let i = 0; i < n; i++) line.push(get(i) ? 1 : 0);
    line.push(0, 0, 0, 0);
    for (let i = 0; i + 11 <= line.length; i++) {
      let a = true;
      let b = true;
      for (let k = 0; k < 11; k++) {
        if (line[i + k] !== P1[k]) a = false;
        if (line[i + k] !== P2[k]) b = false;
        if (!a && !b) break;
      }
      if (a) result += PENALTY_N3;
      if (b) result += PENALTY_N3;
    }
  };
  for (let y = 0; y < n; y++) scanLine((x) => m[y][x]);
  for (let x = 0; x < n; x++) scanLine((y) => m[y][x]);
  for (let y = 0; y < n - 1; y++) {
    for (let x = 0; x < n - 1; x++) {
      const c = m[y][x];
      if (c === m[y][x + 1] && c === m[y + 1][x] && c === m[y + 1][x + 1]) result += PENALTY_N2;
    }
  }
  let dark = 0;
  for (const row of m) for (const c of row) if (c) dark++;
  const total = n * n;
  const k = Math.max(0, Math.ceil(Math.abs(dark * 20 - total * 10) / total) - 1);
  result += k * PENALTY_N4;
  return result;
}

/* --------------------------------------------------------------- encoder */

function addEccAndInterleave(data, ver, ecl) {
  const { numBlocks, eccPerBlock, totalCodewords } = blockInfo(ver, ecl);
  const numShortBlocks = numBlocks - (totalCodewords % numBlocks);
  const shortBlockLen = Math.floor(totalCodewords / numBlocks);
  const gen = rsGenerator(eccPerBlock);
  const blocks = [];
  for (let i = 0, k = 0; i < numBlocks; i++) {
    const len = shortBlockLen - eccPerBlock + (i < numShortBlocks ? 0 : 1);
    const dat = Array.from(data.slice(k, k + len));
    k += len;
    const ecc = rsRemainder(dat, gen);
    if (i < numShortBlocks) dat.push(0); // placeholder so all blocks align; skipped below
    blocks.push(dat.concat(ecc));
  }
  const result = [];
  for (let i = 0; i < blocks[0].length; i++) {
    for (let j = 0; j < blocks.length; j++) {
      if (i !== shortBlockLen - eccPerBlock || j >= numShortBlocks) result.push(blocks[j][i]);
    }
  }
  return result;
}

class QrSymbol {
  constructor(ver, ecl) {
    this.ver = ver;
    this.ecl = ecl;
    this.size = ver * 4 + 17;
    this.modules = Array.from({ length: this.size }, () => new Array(this.size).fill(false));
    this.isFunction = Array.from({ length: this.size }, () => new Array(this.size).fill(false));
  }

  setFn(x, y, dark) {
    this.modules[y][x] = dark;
    this.isFunction[y][x] = true;
  }

  drawFunctionPatterns() {
    const n = this.size;
    for (let i = 0; i < n; i++) {
      this.setFn(6, i, i % 2 === 0);
      this.setFn(i, 6, i % 2 === 0);
    }
    this.drawFinder(3, 3);
    this.drawFinder(n - 4, 3);
    this.drawFinder(3, n - 4);
    const pos = alignmentPositions(this.ver);
    const last = pos.length - 1;
    for (let i = 0; i < pos.length; i++) {
      for (let j = 0; j < pos.length; j++) {
        if ((i === 0 && j === 0) || (i === 0 && j === last) || (i === last && j === 0)) continue;
        this.drawAlignment(pos[i], pos[j]);
      }
    }
    this.drawFormat(0); // reserve the area; redrawn with the real mask later
    this.drawVersion();
  }

  drawFinder(cx, cy) {
    for (let dy = -4; dy <= 4; dy++) {
      for (let dx = -4; dx <= 4; dx++) {
        const x = cx + dx;
        const y = cy + dy;
        if (x < 0 || y < 0 || x >= this.size || y >= this.size) continue;
        const dist = Math.max(Math.abs(dx), Math.abs(dy));
        this.setFn(x, y, dist !== 2 && dist !== 4);
      }
    }
  }

  drawAlignment(cx, cy) {
    for (let dy = -2; dy <= 2; dy++) {
      for (let dx = -2; dx <= 2; dx++) this.setFn(cx + dx, cy + dy, Math.max(Math.abs(dx), Math.abs(dy)) !== 1);
    }
  }

  drawFormat(mask) {
    const bits = formatBits(this.ecl, mask);
    const n = this.size;
    for (let i = 0; i <= 5; i++) this.setFn(8, i, getBit(bits, i));
    this.setFn(8, 7, getBit(bits, 6));
    this.setFn(8, 8, getBit(bits, 7));
    this.setFn(7, 8, getBit(bits, 8));
    for (let i = 9; i < 15; i++) this.setFn(14 - i, 8, getBit(bits, i));
    for (let i = 0; i < 8; i++) this.setFn(n - 1 - i, 8, getBit(bits, i));
    for (let i = 8; i < 15; i++) this.setFn(8, n - 15 + i, getBit(bits, i));
    this.setFn(8, n - 8, true); // the always-dark module
  }

  drawVersion() {
    if (this.ver < 7) return;
    const bits = versionBits(this.ver);
    for (let i = 0; i < 18; i++) {
      const dark = getBit(bits, i);
      const a = this.size - 11 + (i % 3);
      const b = Math.floor(i / 3);
      this.setFn(a, b, dark);
      this.setFn(b, a, dark);
    }
  }

  drawCodewords(codewords) {
    const n = this.size;
    let i = 0;
    const totalBits = codewords.length * 8;
    for (let right = n - 1; right >= 1; right -= 2) {
      if (right === 6) right = 5; // skip the vertical timing column
      const upward = ((right + 1) & 2) === 0;
      for (let vert = 0; vert < n; vert++) {
        const y = upward ? n - 1 - vert : vert;
        for (let j = 0; j < 2; j++) {
          const x = right - j;
          if (!this.isFunction[y][x] && i < totalBits) {
            this.modules[y][x] = getBit(codewords[i >>> 3], 7 - (i & 7));
            i++;
          }
          // Remainder bits (if any) stay light before masking, as the spec requires.
        }
      }
    }
  }

  applyMask(mask) {
    for (let y = 0; y < this.size; y++) {
      for (let x = 0; x < this.size; x++) {
        if (!this.isFunction[y][x] && maskFn(mask, x, y)) this.modules[y][x] = !this.modules[y][x];
      }
    }
  }
}

/**
 * Encode text (UTF-8) or bytes as a QR Code symbol in byte mode.
 * @param {string|Uint8Array|number[]} data
 * @param {{ecl?: 'L'|'M'|'Q'|'H', minVersion?: number, maxVersion?: number, mask?: number}} [opts]
 *   mask −1 (default) selects the mask with the lowest penalty score.
 * @returns {{version: number, size: number, ecl: string, mask: number, modules: boolean[][]}}
 */
export function encodeQR(data, { ecl = 'M', minVersion = 1, maxVersion = 40, mask = -1 } = {}) {
  if (!ECL[ecl]) throw new RangeError(`unknown error correction level: ${ecl}`);
  if (!(minVersion >= 1 && maxVersion <= 40 && minVersion <= maxVersion)) throw new RangeError('invalid version range');
  if (mask < -1 || mask > 7) throw new RangeError('mask must be -1..7');
  const bytes = typeof data === 'string' ? new TextEncoder().encode(data) : Uint8Array.from(data);

  let version = minVersion;
  for (;;) {
    const capacity = numDataCodewords(version, ecl) * 8;
    const used = 4 + (version < 10 ? 8 : 16) + bytes.length * 8;
    if (used <= capacity) break;
    if (version >= maxVersion) throw new RangeError('data too long for a QR code at this error correction level');
    version++;
  }

  // Segment: mode 0100 (byte), character count, data; then terminator, byte align, pad.
  const bits = [];
  const push = (val, len) => {
    for (let i = len - 1; i >= 0; i--) bits.push((val >>> i) & 1);
  };
  push(0b0100, 4);
  push(bytes.length, version < 10 ? 8 : 16);
  for (const b of bytes) push(b, 8);
  const capacityBits = numDataCodewords(version, ecl) * 8;
  push(0, Math.min(4, capacityBits - bits.length));
  push(0, (8 - (bits.length % 8)) % 8);
  for (let pad = 0xec; bits.length < capacityBits; pad ^= 0xec ^ 0x11) push(pad, 8);

  const dataCodewords = new Uint8Array(bits.length / 8);
  for (let i = 0; i < bits.length; i++) dataCodewords[i >>> 3] |= bits[i] << (7 - (i & 7));

  const sym = new QrSymbol(version, ecl);
  sym.drawFunctionPatterns();
  sym.drawCodewords(addEccAndInterleave(dataCodewords, version, ecl));

  let chosen = mask;
  if (chosen === -1) {
    let best = Infinity;
    for (let i = 0; i < 8; i++) {
      sym.applyMask(i);
      sym.drawFormat(i);
      const p = penaltyScore(sym.modules);
      if (p < best) {
        best = p;
        chosen = i;
      }
      sym.applyMask(i); // XOR again to undo
    }
  }
  sym.applyMask(chosen);
  sym.drawFormat(chosen);
  return { version, size: sym.size, ecl, mask: chosen, modules: sym.modules };
}

/* ------------------------------------------------------------- rendering */

/**
 * SVG path data for the dark modules (horizontal runs merged), offset by `border`.
 * @param {{modules: boolean[][]}} qr
 * @param {number} [border] quiet zone in modules (spec minimum 4)
 */
export function qrPathData(qr, border = 4) {
  const parts = [];
  qr.modules.forEach((row, y) => {
    let x = 0;
    while (x < row.length) {
      if (!row[x]) {
        x++;
        continue;
      }
      let len = 1;
      while (x + len < row.length && row[x + len]) len++;
      parts.push(`M${x + border} ${y + border}h${len}v1h-${len}z`);
      x += len;
    }
  });
  return parts.join('');
}

/**
 * Render as an <svg> element (CSP-safe: attributes only, no inline styles).
 * Colours default to black on white regardless of theme, for reliable scanning.
 * @param {{size: number, modules: boolean[][]}} qr
 * @param {{border?: number, pixelSize?: number, dark?: string, light?: string, title?: string}} [opts]
 */
export function qrToSvg(qr, { border = 4, pixelSize = 200, dark = '#000000', light = '#ffffff', title = 'QR code' } = {}) {
  const NS = 'http://www.w3.org/2000/svg';
  const dim = qr.size + border * 2;
  const svg = document.createElementNS(NS, 'svg');
  svg.setAttribute('viewBox', `0 0 ${dim} ${dim}`);
  svg.setAttribute('width', String(pixelSize));
  svg.setAttribute('height', String(pixelSize));
  svg.setAttribute('shape-rendering', 'crispEdges');
  svg.setAttribute('role', 'img');
  svg.setAttribute('aria-label', title);
  const t = document.createElementNS(NS, 'title');
  t.textContent = title;
  const bg = document.createElementNS(NS, 'rect');
  bg.setAttribute('width', String(dim));
  bg.setAttribute('height', String(dim));
  bg.setAttribute('fill', light);
  const path = document.createElementNS(NS, 'path');
  path.setAttribute('d', qrPathData(qr, border));
  path.setAttribute('fill', dark);
  svg.append(t, bg, path);
  return svg;
}

/**
 * Draw onto a canvas (resizes it). Useful for "save as PNG".
 * @param {{size: number, modules: boolean[][]}} qr
 * @param {HTMLCanvasElement} canvas
 * @param {{scale?: number, border?: number, dark?: string, light?: string}} [opts]
 */
export function qrToCanvas(qr, canvas, { scale = 8, border = 4, dark = '#000000', light = '#ffffff' } = {}) {
  const dim = (qr.size + border * 2) * scale;
  canvas.width = dim;
  canvas.height = dim;
  const ctx = canvas.getContext('2d');
  ctx.fillStyle = light;
  ctx.fillRect(0, 0, dim, dim);
  ctx.fillStyle = dark;
  qr.modules.forEach((row, y) => row.forEach((on, x) => {
    if (on) ctx.fillRect((x + border) * scale, (y + border) * scale, scale, scale);
  }));
  return canvas;
}
