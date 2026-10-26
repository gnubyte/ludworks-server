/**
 * Password / passphrase generator and strength estimator (VAULT-CRYPTO.md §5).
 *
 * Randomness: every draw is an unbiased integer from rejection sampling over little-endian uint32 values
 * (§8 "Generator determinism" documents the exact byte consumption so other clients can reproduce
 * the vectors with an injected byte source).
 */
import { invalidArg } from './errors.js';
import { WORDS } from './wordlist.js';

export const UPPER = 'ABCDEFGHIJKLMNOPQRSTUVWXYZ';
export const LOWER = 'abcdefghijklmnopqrstuvwxyz';
export const DIGITS = '0123456789';
export const SYMBOLS = '!@#$%^&*()-_=+[]{};:,.?/';
export const AMBIGUOUS = 'Il1O0';

const MAX_ATTEMPTS = 10000;

/** Buffered byte source over a `(n) => Uint8Array` function (default: CSPRNG). */
function byteSource(rng) {
  const fill = rng || ((n) => globalThis.crypto.getRandomValues(new Uint8Array(n)));
  let buf = new Uint8Array(0);
  let pos = 0;
  return {
    u32() {
      if (pos + 4 > buf.length) {
        const fresh = fill(256);
        if (!(fresh instanceof Uint8Array) || fresh.length < 4) throw invalidArg('rng must return a Uint8Array of the requested length');
        const rest = buf.subarray(pos);
        const merged = new Uint8Array(rest.length + fresh.length);
        merged.set(rest);
        merged.set(fresh, rest.length);
        buf = merged;
        pos = 0;
      }
      const v = (buf[pos] | (buf[pos + 1] << 8) | (buf[pos + 2] << 16) | (buf[pos + 3] << 24)) >>> 0;
      pos += 4;
      return v;
    },
  };
}

/**
 * Uniform integer in [0, n) without modulo bias: reject draws ≥ the largest multiple of n below 2^32.
 * @param {number} n 1..2^32
 * @param {{u32:()=>number}} [src]
 */
export function randomInt(n, src = byteSource()) {
  if (!Number.isInteger(n) || n < 1 || n > 0x100000000) throw invalidArg('randomInt: n out of range');
  const limit = 0x100000000 - (0x100000000 % n);
  for (;;) {
    const x = src.u32();
    if (x < limit) return x % n;
  }
}

function stripAmbiguous(set, exclude) {
  return exclude ? [...set].filter((c) => !AMBIGUOUS.includes(c)).join('') : set;
}

/**
 * Generate a random password.
 * @param {{length?:number, upper?:boolean, lower?:boolean, digits?:boolean, symbols?:boolean,
 *          excludeAmbiguous?:boolean, rng?:(n:number)=>Uint8Array}} [opts]
 *   `rng` is for deterministic tests only; production callers must omit it.
 * @returns {string}
 */
export function generatePassword(opts = {}) {
  const { length = 20, upper = true, lower = true, digits = true, symbols = true, excludeAmbiguous = false } = opts;
  if (!Number.isInteger(length) || length < 8 || length > 128) throw invalidArg('length must be an integer in [8, 128]');
  const sets = [];
  if (upper) sets.push(stripAmbiguous(UPPER, excludeAmbiguous));
  if (lower) sets.push(stripAmbiguous(LOWER, excludeAmbiguous));
  if (digits) sets.push(stripAmbiguous(DIGITS, excludeAmbiguous));
  if (symbols) sets.push(SYMBOLS);
  if (sets.length === 0) throw invalidArg('enable at least one character set');
  if (sets.length > length) throw invalidArg('length is shorter than the number of required sets');
  const pool = sets.join('');
  const src = byteSource(opts.rng);
  // Whole-password rejection keeps the result uniform over all passwords that contain every set.
  for (let attempt = 0; attempt < MAX_ATTEMPTS; attempt++) {
    let pw = '';
    for (let i = 0; i < length; i++) pw += pool[randomInt(pool.length, src)];
    if (sets.every((set) => [...pw].some((c) => set.includes(c)))) return pw;
  }
  /* c8 ignore next */
  throw new Error('password generation failed to satisfy constraints'); // probability ~0 for length ≥ 8
}

/**
 * Generate a passphrase from the bundled wordlist.
 * Draw order: one index per word, then (if includeNumber) the word position, then the digit.
 * @param {{words?:number, separator?:string, capitalize?:boolean, includeNumber?:boolean,
 *          rng?:(n:number)=>Uint8Array, wordlist?:ReadonlyArray<string>}} [opts]
 * @returns {string}
 */
export function generatePassphrase(opts = {}) {
  const { words = 6, separator = '-', capitalize = false, includeNumber = false } = opts;
  const list = opts.wordlist || WORDS;
  if (!Number.isInteger(words) || words < 3 || words > 12) throw invalidArg('words must be an integer in [3, 12]');
  if (typeof separator !== 'string' || separator.length > 8) throw invalidArg('separator must be a string of at most 8 characters');
  const src = byteSource(opts.rng);
  const picked = [];
  for (let i = 0; i < words; i++) picked.push(list[randomInt(list.length, src)]);
  const out = capitalize ? picked.map((w) => w[0].toUpperCase() + w.slice(1)) : picked;
  if (includeNumber) {
    const at = randomInt(words, src);
    out[at] += String(randomInt(10, src));
  }
  return out.join(separator);
}

// --- strength estimation -------------------------------------------------------------------------

const COMMON = [
  'password', '123456', '123456789', '12345678', '12345', 'qwerty', 'abc123', 'football', '1234567', 'monkey',
  '111111', 'letmein', '1234', '1234567890', 'dragon', 'baseball', 'sunshine', 'iloveyou', 'trustno1', 'princess',
  'adobe123', '123123', 'welcome', 'login', 'admin', 'qwerty123', 'solo', '1q2w3e4r', 'master', '666666',
  'photoshop', '1qaz2wsx', 'qwertyuiop', 'ashley', 'mustang', '121212', 'starwars', '654321', 'bailey', 'access',
  'flower', '555555', 'passw0rd', 'shadow', 'lovely', '7777777', 'michael', '!@#$%^&*', 'jesus', 'password1',
  'superman', 'hello', 'charlie', '888888', '696969', 'hottie', 'freedom', 'aa123456', 'qazwsx', 'ninja',
  'azerty', 'loveme', 'whatever', 'donald', 'batman', 'zaq1zaq1', '000000', 'secret', 'p@ssw0rd', 'changeme',
  'administrator', 'root', 'toor', 'default', 'guest', 'test', 'test123', 'pass', 'pass123', 'password123',
  'letmein1', 'welcome1', 'monkey1', 'killer', 'hunter2', 'computer', 'internet', 'samsung', 'cheese', 'summer',
  'winter', 'spring', 'autumn', 'soccer', 'hockey', 'killer1', 'jordan23', 'liverpool', 'chelsea', 'arsenal',
];
const COMMON_SET = new Set(COMMON);
const WORD_SET = new Set(WORDS);
const KEYBOARD_ROWS = ['qwertyuiop', 'asdfghjkl', 'zxcvbnm', '1234567890', 'qwertzuiop', 'azertyuiop'];
const LEET = { 0: 'o', 1: 'l', 3: 'e', 4: 'a', 5: 's', 7: 't', '@': 'a', $: 's', '!': 'i' };

function unleet(s) {
  return [...s.toLowerCase()].map((c) => LEET[c] || c).join('');
}

function charsetSize(pw) {
  let n = 0;
  if (/[a-z]/.test(pw)) n += 26;
  if (/[A-Z]/.test(pw)) n += 26;
  if (/[0-9]/.test(pw)) n += 10;
  if (/[^A-Za-z0-9]/.test(pw) && /[\x20-\x7e]/.test(pw.replace(/[A-Za-z0-9]/g, ''))) n += 33;
  if (/[^\x00-\x7f]/.test(pw)) n += 100;
  return Math.max(n, 10);
}

const KEYBOARD_LINES = KEYBOARD_ROWS.flatMap((row) => [row, [...row].reverse().join('')]);
const MAX_ANALYZED = 256; // characters beyond this are costed per character (keeps the estimator linear)

/** Length of the ascending/descending run (step ±1) starting at i. */
function sequenceLength(chars, i) {
  if (i + 1 >= chars.length) return 1;
  const d = chars[i + 1].codePointAt(0) - chars[i].codePointAt(0);
  if (d !== 1 && d !== -1) return 1;
  let n = 2;
  while (i + n < chars.length && chars[i + n].codePointAt(0) - chars[i + n - 1].codePointAt(0) === d) n++;
  return n;
}

/** Length of the longest keyboard-row run starting at i (bounded by the row length). */
function keyboardRunLength(chars, i) {
  let best = 0;
  for (let len = 2; i + len <= chars.length && len <= 12; len++) {
    const seg = chars.slice(i, i + len).join('').toLowerCase();
    if (!KEYBOARD_LINES.some((line) => line.includes(seg))) break;
    best = len;
  }
  return best;
}

/**
 * Pattern-aware entropy estimate (a small zxcvbn-style matcher: common passwords, dictionary words,
 * repeats, sequences, keyboard runs, years; everything else costs log2(charset) per character).
 * @param {string} password
 * @returns {{score:0|1|2|3|4, entropyBits:number, feedback:string[]}}
 */
export function estimateStrength(password) {
  const pw = typeof password === 'string' ? password : '';
  if (!pw) return { score: 0, entropyBits: 0, feedback: ['Enter a password.'] };
  const feedback = new Set();
  const lower = pw.toLowerCase();
  const chars = [...pw];
  const perChar = Math.log2(charsetSize(pw));

  if (COMMON_SET.has(lower) || COMMON_SET.has(unleet(pw))) {
    const rank = Math.max(COMMON.indexOf(lower), COMMON.indexOf(unleet(pw)));
    return { score: 0, entropyBits: Math.round(Math.log2(rank + 2) * 10) / 10, feedback: ['This is one of the most common passwords.'] };
  }

  let bits = 0;
  let i = 0;
  while (i < chars.length) {
    let best = null; // [length, cost]
    // Dictionary words (longest first, ≥ 4 letters), case and leet tolerant.
    for (let len = Math.min(9, chars.length - i); len >= 4; len--) {
      const seg = chars.slice(i, i + len).join('');
      const plain = unleet(seg);
      if (WORD_SET.has(plain) || COMMON_SET.has(plain)) {
        let cost = Math.log2(WORDS.length);
        if (seg !== seg.toLowerCase()) cost += seg[0] === seg[0].toUpperCase() && seg.slice(1) === seg.slice(1).toLowerCase() ? 1 : 2;
        if (plain !== seg.toLowerCase()) cost += 1;
        best = [len, cost];
        feedback.add('Avoid single dictionary words; combine several unrelated words or use random characters.');
        break;
      }
    }
    if (!best) {
      // Repeated character run.
      let r = 1;
      while (i + r < chars.length && chars[i + r] === chars[i]) r++;
      if (r >= 3) {
        best = [r, perChar + Math.log2(r)];
        feedback.add('Avoid repeated characters.');
      }
    }
    if (!best && i >= MAX_ANALYZED) best = [chars.length - i, perChar * (chars.length - i)];
    if (!best && /^(19|20)\d\d$/.test(chars.slice(i, i + 4).join(''))) {
      best = [4, 7];
      feedback.add('Years and dates are easy to guess.');
    }
    if (!best) {
      const seq = sequenceLength(chars, i);
      const kb = keyboardRunLength(chars, i);
      if (seq >= 3 && seq >= kb) {
        best = [seq, Math.log2(26) + Math.log2(seq)];
        feedback.add('Avoid sequences like "abc" or "123".');
      } else if (kb >= 4) {
        best = [kb, Math.log2(12) + Math.log2(kb) + 1];
        feedback.add('Avoid keyboard patterns like "qwerty".');
      }
    }
    if (!best) best = [1, perChar];
    bits += best[1];
    i += best[0];
  }
  // Whole-password repetition such as "abcabcabc".
  const m = pw.length <= MAX_ANALYZED ? /^(.+?)\1+$/.exec(pw) : null;
  if (m) {
    bits = Math.min(bits, (bits * m[1].length) / pw.length + Math.log2(pw.length / m[1].length));
    feedback.add('Avoid repeating the same pattern.');
  }
  bits = Math.round(bits * 10) / 10;
  const score = bits < 28 ? 0 : bits < 40 ? 1 : bits < 52 ? 2 : bits < 64 ? 3 : 4;
  if (score < 3) {
    if (chars.length < 12) feedback.add('Use at least 12 characters, or a passphrase of 5+ words.');
    if (charsetSize(pw) <= 36) feedback.add('Add uppercase letters, digits or symbols.');
  }
  return { score, entropyBits: bits, feedback: [...feedback] };
}
