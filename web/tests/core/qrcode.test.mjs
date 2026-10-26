// QR encoder tests. Besides known constants from ISO/IEC 18004 tables, every
// generated symbol is decoded by an independent reader written here (own GF(256)
// tables, own function-pattern map, own zigzag walk) which verifies format/version
// BCH words, Reed–Solomon syndromes of every block, and the decoded payload.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import {
  encodeQR, rsGenerator, rsRemainder, formatBits, versionBits, alignmentPositions,
  numRawDataModules, numDataCodewords, blockInfo, penaltyScore, qrPathData,
} from '../../assets/js/lib/qrcode.js';

/* ---------------------------------------------- independent GF(256) arithmetic */
const gfExpT = [];
const gfLogT = [];
{
  let x = 1;
  for (let i = 0; i < 255; i++) {
    gfExpT[i] = x;
    gfLogT[x] = i;
    x = x << 1;
    if (x > 255) x ^= 0x11d;
  }
}
const mul = (a, b) => (a && b ? gfExpT[(gfLogT[a] + gfLogT[b]) % 255] : 0);

/* ----------------------------------------------- independent BCH for format/version */
function polyMod(value, valueBits, gen, genDegree) {
  let v = value;
  for (let bit = valueBits - 1; bit >= genDegree; bit--) {
    if ((v >> bit) & 1) v ^= gen << (bit - genDegree);
  }
  return v;
}
const ECL_BITS = { L: 1, M: 0, Q: 3, H: 2 };
const validFormat = new Map(); // codeword → {ecl, mask}
for (const [ecl, eb] of Object.entries(ECL_BITS)) {
  for (let mask = 0; mask < 8; mask++) {
    const data = (eb << 3) | mask;
    const word = ((data << 10) | polyMod(data << 10, 15, 0b10100110111, 10)) ^ 0b101010000010010;
    validFormat.set(word, { ecl, mask });
  }
}

/* ------------------------------------------------------- function-module map */
const SPEC_ALIGN = {
  1: [], 2: [6, 18], 5: [6, 30], 7: [6, 22, 38], 10: [6, 28, 50], 15: [6, 26, 48, 70], 20: [6, 34, 62, 90],
  25: [6, 32, 58, 84, 110], 27: [6, 34, 62, 90, 118], 32: [6, 34, 60, 86, 112, 138],
  36: [6, 24, 50, 76, 102, 128, 154], 40: [6, 30, 58, 86, 114, 142, 170],
};

function functionMap(ver) {
  const n = ver * 4 + 17;
  const f = Array.from({ length: n }, () => new Array(n).fill(false));
  const fill = (r0, r1, c0, c1) => {
    for (let r = r0; r <= r1; r++) for (let c = c0; c <= c1; c++) if (r >= 0 && c >= 0 && r < n && c < n) f[r][c] = true;
  };
  fill(0, 8, 0, 8); // finder + separator + format (top-left)
  fill(0, 8, n - 8, n - 1); // top-right
  fill(n - 8, n - 1, 0, 8); // bottom-left (+ dark module)
  fill(6, 6, 0, n - 1); // timing row
  fill(0, n - 1, 6, 6); // timing column
  const pos = SPEC_ALIGN[ver] ?? alignmentPositions(ver);
  for (const r of pos) {
    for (const c of pos) {
      if ((r === 6 && c === 6) || (r === 6 && c === n - 7) || (r === n - 7 && c === 6)) continue;
      fill(r - 2, r + 2, c - 2, c + 2);
    }
  }
  if (ver >= 7) {
    fill(0, 5, n - 11, n - 9);
    fill(n - 11, n - 9, 0, 5);
  }
  return f;
}

const maskOf = [
  (r, c) => (r + c) % 2 === 0,
  (r) => r % 2 === 0,
  (r, c) => c % 3 === 0,
  (r, c) => (r + c) % 3 === 0,
  (r, c) => (Math.floor(r / 2) + Math.floor(c / 3)) % 2 === 0,
  (r, c) => ((r * c) % 2) + ((r * c) % 3) === 0,
  (r, c) => (((r * c) % 2) + ((r * c) % 3)) % 2 === 0,
  (r, c) => (((r + c) % 2) + ((r * c) % 3)) % 2 === 0,
];

/* -------------------------------------------------------------------- decoder */
function decode(qr) {
  const m = qr.modules;
  const n = m.length;
  assert.equal((n - 17) % 4, 0, 'size must be 4v+17');
  const ver = (n - 17) / 4;
  const bit = (r, c) => (m[r][c] ? 1 : 0);

  // Finder patterns (7x7 rings) at three corners, light separators.
  for (const [r0, c0] of [[0, 0], [0, n - 7], [n - 7, 0]]) {
    for (let r = 0; r < 7; r++) {
      for (let c = 0; c < 7; c++) {
        const ring = Math.max(Math.abs(r - 3), Math.abs(c - 3));
        assert.equal(bit(r0 + r, c0 + c), ring === 2 ? 0 : 1, `finder module ${r0 + r},${c0 + c}`);
      }
    }
  }
  for (let i = 8; i < n - 8; i++) {
    assert.equal(bit(6, i), i % 2 === 0 ? 1 : 0, 'timing row');
    assert.equal(bit(i, 6), i % 2 === 0 ? 1 : 0, 'timing column');
  }
  assert.equal(bit(n - 8, 8), 1, 'dark module');

  // Format information, both copies (bit 14 = MSB).
  let fa = 0;
  const a = [];
  for (let c = 0; c <= 5; c++) a.push([8, c]); // bits 14..9
  a.push([8, 7], [8, 8], [7, 8]); // bits 8, 7, 6
  for (let r = 5; r >= 0; r--) a.push([r, 8]); // bits 5..0
  for (const [r, c] of a) fa = (fa << 1) | bit(r, c);
  let fb = 0;
  const b = [];
  for (let r = n - 1; r >= n - 7; r--) b.push([r, 8]); // bits 14..8
  for (let c = n - 8; c <= n - 1; c++) b.push([8, c]); // bits 7..0
  for (const [r, c] of b) fb = (fb << 1) | bit(r, c);
  assert.equal(fa, fb, 'format copies agree');
  const fmt = validFormat.get(fa);
  assert.ok(fmt, `valid format codeword ${fa.toString(2)}`);

  // Version information (v >= 7), both copies.
  if (ver >= 7) {
    let va = 0;
    let vb = 0;
    for (let i = 17; i >= 0; i--) {
      const r = Math.floor(i / 3);
      const c = n - 11 + (i % 3);
      va = (va << 1) | bit(r, c);
      vb = (vb << 1) | bit(c, r);
    }
    assert.equal(va, vb, 'version copies agree');
    assert.equal(va >> 12, ver, 'version number');
    assert.equal(va & 0xfff, polyMod(ver << 12, 18, 0b1111100100101, 12), 'version BCH');
  }

  // Data modules: two-column strips from the right, alternating up/down, skipping column 6.
  const fn = functionMap(ver);
  const bits = [];
  let up = true;
  for (let col = n - 1; col > 0; col -= 2) {
    if (col === 6) col = 5;
    for (let k = 0; k < n; k++) {
      const r = up ? n - 1 - k : k;
      for (const c of [col, col - 1]) {
        if (fn[r][c]) continue;
        bits.push(bit(r, c) ^ (maskOf[fmt.mask](r, c) ? 1 : 0));
      }
    }
    up = !up;
  }
  assert.equal(bits.length, numRawDataModules(ver), 'raw data module count');

  const { numBlocks, eccPerBlock, totalCodewords } = blockInfo(ver, fmt.ecl);
  const cw = [];
  for (let i = 0; i < totalCodewords; i++) {
    let v = 0;
    for (let k = 0; k < 8; k++) v = (v << 1) | bits[i * 8 + k];
    cw.push(v);
  }
  // Remainder bits must be zero after unmasking.
  assert.ok(bits.slice(totalCodewords * 8).every((x) => x === 0), 'remainder bits are zero');

  // De-interleave.
  const shortLen = Math.floor(totalCodewords / numBlocks);
  const numShort = numBlocks - (totalCodewords % numBlocks);
  const dataLens = Array.from({ length: numBlocks }, (_, j) => shortLen - eccPerBlock + (j < numShort ? 0 : 1));
  const dataBlocks = dataLens.map(() => []);
  const eccBlocks = dataLens.map(() => []);
  let k = 0;
  for (let i = 0; i < Math.max(...dataLens); i++) for (let j = 0; j < numBlocks; j++) if (i < dataLens[j]) dataBlocks[j].push(cw[k++]);
  for (let i = 0; i < eccPerBlock; i++) for (let j = 0; j < numBlocks; j++) eccBlocks[j].push(cw[k++]);
  assert.equal(k, totalCodewords);

  // Every block must be a valid RS codeword: c(α^i) = 0 for i < eccPerBlock.
  for (let j = 0; j < numBlocks; j++) {
    const c = dataBlocks[j].concat(eccBlocks[j]);
    for (let i = 0; i < eccPerBlock; i++) {
      let s = 0;
      for (const coef of c) s = mul(s, gfExpT[i]) ^ coef;
      assert.equal(s, 0, `RS syndrome ${i} of block ${j}`);
    }
  }

  // Parse the byte-mode segment.
  const data = dataBlocks.flat();
  const dbits = [];
  for (const v of data) for (let i = 7; i >= 0; i--) dbits.push((v >> i) & 1);
  let p = 0;
  const read = (len) => {
    let v = 0;
    for (let i = 0; i < len; i++) v = (v << 1) | dbits[p++];
    return v;
  };
  assert.equal(read(4), 0b0100, 'byte mode indicator');
  const count = read(ver < 10 ? 8 : 16);
  const out = new Uint8Array(count);
  for (let i = 0; i < count; i++) out[i] = read(8);
  // Terminator (up to 4 zero bits), then zero padding to a byte, then 0xEC/0x11 pad bytes.
  const rest = dbits.slice(p);
  const termLen = Math.min(4, rest.length);
  assert.ok(rest.slice(0, termLen).every((x) => x === 0), 'terminator');
  const afterTerm = p + termLen;
  const aligned = Math.ceil(afterTerm / 8) * 8;
  assert.ok(dbits.slice(afterTerm, aligned).every((x) => x === 0), 'byte alignment bits');
  for (let i = aligned / 8, t = 0; i < data.length; i++, t++) assert.equal(data[i], t % 2 === 0 ? 0xec : 0x11, 'pad bytes');
  return { version: ver, ecl: fmt.ecl, mask: fmt.mask, bytes: out };
}

const maxBytes = (ver, ecl) => Math.floor((numDataCodewords(ver, ecl) * 8 - 4 - (ver < 10 ? 8 : 16)) / 8);

/* ---------------------------------------------------------------------- tests */

test('Reed–Solomon matches the published 1-M "HELLO WORLD" example', () => {
  const data = [32, 91, 11, 120, 209, 114, 220, 77, 67, 64, 236, 17, 236, 17, 236, 17];
  assert.deepEqual(rsRemainder(data, rsGenerator(10)), [196, 35, 39, 119, 235, 215, 231, 226, 93, 23]);
});

test('RS generator of degree 7 matches the standard log form', () => {
  // ISO 18004 Annex A: α^0 x^7 + α^87 x^6 + α^229 x^5 + α^146 x^4 + α^149 x^3 + α^238 x^2 + α^102 x + α^21
  const expected = [0, 87, 229, 146, 149, 238, 102, 21].map((e) => gfExpT[e]);
  assert.deepEqual(rsGenerator(7), expected);
});

test('format information words match the ISO table', () => {
  const table = {
    L: ['111011111000100', '111001011110011', '111110110101010', '111100010011101', '110011000101111', '110001100011000', '110110001000001', '110100101110110'],
    M: ['101010000010010', '101000100100101', '101111001111100', '101101101001011', '100010111111001', '100000011001110', '100111110010111', '100101010100000'],
  };
  for (const [ecl, rows] of Object.entries(table)) {
    rows.forEach((bin, mask) => assert.equal(formatBits(ecl, mask), parseInt(bin, 2), `${ecl}${mask}`));
  }
  assert.equal(formatBits('Q', 0), parseInt('011010101011111', 2));
  assert.equal(formatBits('H', 0), parseInt('001011010001001', 2));
  for (const ecl of ['L', 'M', 'Q', 'H']) for (let m = 0; m < 8; m++) assert.ok(validFormat.has(formatBits(ecl, m)));
});

test('version information words match the ISO table', () => {
  assert.equal(versionBits(7), 0x07c94);
  assert.equal(versionBits(8), 0x085bc);
  assert.equal(versionBits(40), 0x28c69);
});

test('alignment pattern positions match the ISO table', () => {
  for (const [ver, pos] of Object.entries(SPEC_ALIGN)) assert.deepEqual(alignmentPositions(Number(ver)), pos, `v${ver}`);
});

test('raw module counts equal the non-function area of every version', () => {
  for (let v = 1; v <= 40; v++) {
    const f = functionMap(v);
    let free = 0;
    for (const row of f) for (const x of row) if (!x) free++;
    assert.equal(numRawDataModules(v), free, `v${v}`);
  }
});

test('byte-mode capacities match the ISO table', () => {
  const known = { '1L': 17, '1M': 14, '1Q': 11, '1H': 7, '2M': 26, '5Q': 60, '10M': 213, '20H': 382, '40L': 2953, '40M': 2331, '40Q': 1663, '40H': 1273 };
  for (const [key, cap] of Object.entries(known)) {
    const ver = parseInt(key, 10);
    const ecl = key.slice(-1);
    assert.equal(maxBytes(ver, ecl), cap, key);
  }
  for (let v = 1; v <= 40; v++) {
    for (const ecl of ['L', 'M', 'Q', 'H']) {
      const { numBlocks, eccPerBlock, totalCodewords } = blockInfo(v, ecl);
      assert.ok(numBlocks >= 1 && eccPerBlock >= 7 && numBlocks * eccPerBlock < totalCodewords, `${v}${ecl}`);
    }
  }
});

test('smallest fitting version is chosen at each capacity boundary', () => {
  for (const ecl of ['L', 'M', 'Q', 'H']) {
    for (const v of [1, 2, 9, 10, 26, 39]) {
      const cap = maxBytes(v, ecl);
      assert.equal(encodeQR(new Uint8Array(cap).fill(65), { ecl }).version, v, `${cap} bytes ${ecl}`);
      assert.equal(encodeQR(new Uint8Array(cap + 1).fill(65), { ecl }).version, v + 1, `${cap + 1} bytes ${ecl}`);
    }
  }
  assert.throws(() => encodeQR(new Uint8Array(2332), { ecl: 'M' }), RangeError);
});

test('symbols decode back to their payload across versions, levels and masks', () => {
  const samples = [
    '', 'A', 'hello world', 'Ludworks — zażółć gęślą jaźń 🔐',
    'otpauth://totp/Ludworks:admin?secret=JBSWY3DPEHPK3PXP&issuer=Ludworks&algorithm=SHA1&digits=6&period=30',
    'x'.repeat(300), 'y'.repeat(1000), 'z'.repeat(2331),
  ];
  for (const text of samples) {
    for (const ecl of ['L', 'M', 'Q', 'H']) {
      const bytes = new TextEncoder().encode(text);
      if (bytes.length > maxBytes(40, ecl)) continue;
      const qr = encodeQR(text, { ecl });
      const d = decode(qr);
      assert.equal(d.version, qr.version);
      assert.equal(d.ecl, ecl);
      assert.equal(d.mask, qr.mask);
      assert.deepEqual(Array.from(d.bytes), Array.from(bytes), `${ecl} ${text.slice(0, 20)}`);
    }
  }
  // Every forced mask, and binary data, on a multi-block version with version info.
  const bin = Uint8Array.from({ length: 200 }, (_, i) => (i * 37 + 11) & 0xff);
  for (let mask = 0; mask < 8; mask++) {
    const qr = encodeQR(bin, { ecl: 'Q', mask });
    assert.equal(qr.mask, mask);
    assert.deepEqual(Array.from(decode(qr).bytes), Array.from(bin));
  }
  // Each version once (forced via minVersion).
  for (let v = 1; v <= 40; v++) {
    const qr = encodeQR(`v${v}`, { ecl: 'M', minVersion: v, maxVersion: v });
    assert.equal(qr.version, v);
    assert.equal(new TextDecoder().decode(decode(qr).bytes), `v${v}`);
  }
});

test('automatic mask is the lowest-penalty mask and output is deterministic', () => {
  for (const text of ['https://ludworks.local:8443', 'JBSWY3DPEHPK3PXP', 'a'.repeat(120)]) {
    const auto = encodeQR(text);
    const penalties = Array.from({ length: 8 }, (_, m) => penaltyScore(encodeQR(text, { mask: m }).modules));
    const best = Math.min(...penalties);
    assert.equal(penalties[auto.mask], best);
    assert.equal(penalties.indexOf(best), auto.mask, 'ties resolve to the lowest mask number');
    assert.deepEqual(encodeQR(text).modules, auto.modules);
  }
});

test('version 1-M reference structure for "LUDWORKS"', () => {
  const qr = encodeQR('LUDWORKS', { ecl: 'M' });
  assert.equal(qr.version, 1);
  assert.equal(qr.size, 21);
  // Rows rendered as strings for a stable snapshot of the deterministic output.
  const rows = qr.modules.map((r) => r.map((x) => (x ? '#' : '.')).join(''));
  // Finder rows are fixed by the standard regardless of data.
  assert.equal(rows[0].slice(0, 7), '#######');
  assert.equal(rows[0].slice(14), '#######');
  assert.equal(rows[2].slice(0, 7), '#.###.#');
  assert.equal(rows[20].slice(0, 7), '#######');
  assert.equal(rows[6].slice(8, 13), '#.#.#');
  const d = decode(qr);
  assert.equal(new TextDecoder().decode(d.bytes), 'LUDWORKS');
});

test('path data merges horizontal runs inside the quiet zone', () => {
  const qr = { size: 3, modules: [[true, true, false], [false, false, false], [true, false, true]] };
  assert.equal(qrPathData(qr, 4), 'M4 4h2v1h-2zM4 6h1v1h-1zM6 6h1v1h-1z');
});
