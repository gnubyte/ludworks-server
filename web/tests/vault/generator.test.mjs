// Password generator, wordlist and strength estimator.
import test from 'node:test';
import assert from 'node:assert/strict';
import { AMBIGUOUS, DIGITS, LOWER, SYMBOLS, UPPER, estimateStrength, generatePassphrase, generatePassword, randomInt } from '../../assets/js/lib/vault/generator.js';
import { WORDS } from '../../assets/js/lib/vault/wordlist.js';

test('wordlist: >= 2048 unique lowercase words of 3-9 letters', () => {
  assert.ok(WORDS.length >= 2048, `only ${WORDS.length} words`);
  assert.equal(new Set(WORDS).size, WORDS.length);
  for (const w of WORDS) assert.match(w, /^[a-z]{3,9}$/);
  assert.deepEqual([...WORDS].sort(), [...WORDS]);
  assert.ok(Object.isFrozen(WORDS));
});

test('generatePassword honours options and always includes each enabled set', () => {
  for (let i = 0; i < 300; i++) {
    const pw = generatePassword({ length: 8 });
    assert.equal(pw.length, 8);
    for (const set of [UPPER, LOWER, DIGITS, SYMBOLS]) assert.ok([...pw].some((c) => set.includes(c)), `${pw} missing ${set}`);
  }
  assert.equal(generatePassword().length, 20);
  const noAmb = generatePassword({ length: 128, excludeAmbiguous: true });
  assert.ok(![...noAmb].some((c) => AMBIGUOUS.includes(c)));
  const digitsOnly = generatePassword({ length: 12, upper: false, lower: false, symbols: false });
  assert.match(digitsOnly, /^[0-9]{12}$/);
  assert.throws(() => generatePassword({ length: 7 }), /length/);
  assert.throws(() => generatePassword({ length: 129 }), /length/);
  assert.throws(() => generatePassword({ upper: false, lower: false, digits: false, symbols: false }), /at least one/);
});

test('randomInt is unbiased (rejection sampling)', () => {
  // A source whose first draw lands in the biased tail must be rejected: for n = 3 the limit is
  // 2^32 - (2^32 mod 3) = 4294967295, so 0xffffffff is rejected and the next value used.
  const values = [0xffffffff, 7];
  const src = { u32: () => values.shift() };
  assert.equal(randomInt(3, src), 7 % 3);
  const counts = new Array(6).fill(0);
  for (let i = 0; i < 60000; i++) counts[randomInt(6)]++;
  for (const c of counts) assert.ok(c > 9000 && c < 11000, `skewed: ${counts}`);
  assert.throws(() => randomInt(0), /range/);
});

test('generatePassphrase', () => {
  const p = generatePassphrase();
  assert.equal(p.split('-').length, 6);
  for (const w of p.split('-')) assert.ok(WORDS.includes(w));
  const q = generatePassphrase({ words: 4, separator: ' ', capitalize: true, includeNumber: true });
  const parts = q.split(' ');
  assert.equal(parts.length, 4);
  assert.equal(parts.filter((w) => /\d$/.test(w)).length, 1);
  for (const w of parts) assert.match(w, /^[A-Z][a-z]+\d?$/);
  assert.throws(() => generatePassphrase({ words: 2 }), /words/);
  assert.throws(() => generatePassphrase({ words: 13 }), /words/);
});

test('deterministic byte source gives reproducible output (vector support)', () => {
  let counter = 0;
  const rng = (n) => Uint8Array.from({ length: n }, () => counter++ & 255);
  const a = generatePassword({ length: 16, rng });
  counter = 0;
  assert.equal(generatePassword({ length: 16, rng }), a);
});

test('estimateStrength ranks obvious cases sensibly', () => {
  assert.equal(estimateStrength('').score, 0);
  assert.equal(estimateStrength('password').score, 0);
  assert.equal(estimateStrength('P@ssw0rd').score, 0);
  assert.equal(estimateStrength('123456').score, 0);
  assert.ok(estimateStrength('aaaaaaaaaaaa').score <= 1);
  assert.ok(estimateStrength('abcdefgh1234').score <= 1);
  assert.ok(estimateStrength('qwertyuiop').score <= 1);
  assert.ok(estimateStrength('sunflower2024').score <= 2);
  assert.equal(estimateStrength(generatePassword({ length: 20 })).score, 4);
  const pp = estimateStrength(generatePassphrase({ words: 6 }));
  assert.ok(pp.score >= 3, JSON.stringify(pp));
  const r = estimateStrength('password1');
  assert.ok(r.feedback.length > 0);
  assert.ok(estimateStrength('Tr0ub4dour&3').entropyBits < estimateStrength('x8#Qv!2mLp@9Zr').entropyBits);
  assert.ok(estimateStrength('abcdefghij').score === 0);
  assert.ok(estimateStrength('1990').feedback.some((f) => /year/i.test(f)));
  // linear time on long input (pasted text, huge repeats)
  const t0 = Date.now();
  estimateStrength('a'.repeat(200000));
  estimateStrength(generatePassword({ length: 128 }).repeat(40));
  estimateStrength('abcdefghijklmnopqrstuvwxyz'.repeat(200));
  assert.ok(Date.now() - t0 < 2000, `too slow: ${Date.now() - t0} ms`);
});
