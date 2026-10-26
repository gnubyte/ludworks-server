// Password strength estimation (client-side hint only — the server enforces policy)
// and a CSPRNG password generator. Pure functions, unit tested.

const COMMON = [
  'password', 'passw0rd', 'p@ssw0rd', 'letmein', 'welcome', 'admin', 'administrator', 'qwerty',
  'qwertyuiop', 'asdfgh', 'zxcvbn', 'iloveyou', 'monkey', 'dragon', 'football', 'baseball',
  'master', 'shadow', 'sunshine', 'princess', 'superman', 'batman', 'trustno1', 'starwars',
  'whatever', 'freedom', 'secret', 'changeme', 'default', 'ludworks', 'login', 'root', 'toor',
  'abc123', '123456', '1234567890', '111111', '000000', 'hello', 'summer', 'winter', 'autumn',
  'spring', 'computer', 'internet', 'server', 'nas', 'storage', 'pokemon', 'michael', 'jordan',
];

const KEYBOARD_ROWS = ['qwertyuiop', 'asdfghjkl', 'zxcvbnm', '1234567890', 'abcdefghijklmnopqrstuvwxyz'];

const LABELS = ['Very weak', 'Weak', 'Fair', 'Strong', 'Very strong'];

function charsetSize(pw) {
  let size = 0;
  if (/[a-z]/.test(pw)) size += 26;
  if (/[A-Z]/.test(pw)) size += 26;
  if (/[0-9]/.test(pw)) size += 10;
  if (/[ -/:-@[-`{-~]/.test(pw)) size += 33;
  if (/[^\x00-\x7f]/.test(pw)) size += 100;
  return Math.max(size, 1);
}

// Length after collapsing runs ("aaaa") and keyboard/alphabet sequences ("abcd", "4321"):
// each such run counts as ~1.5 characters of real entropy.
function effectiveLength(pw) {
  const lower = pw.toLowerCase();
  let eff = 0;
  let i = 0;
  while (i < lower.length) {
    let j = i + 1;
    // repeated characters
    while (j < lower.length && lower[j] === lower[i]) j++;
    if (j - i >= 3) {
      eff += 1.5;
      i = j;
      continue;
    }
    // ascending/descending sequences along a keyboard row or the alphabet
    let best = 1;
    for (const row of KEYBOARD_ROWS) {
      for (const dir of [1, -1]) {
        const seq = dir === 1 ? row : [...row].reverse().join('');
        const start = seq.indexOf(lower[i]);
        if (start < 0) continue;
        let k = 1;
        while (i + k < lower.length && seq[start + k] === lower[i + k]) k++;
        best = Math.max(best, k);
      }
    }
    if (best >= 3) {
      eff += 1.5;
      i += best;
      continue;
    }
    eff += 1;
    i += 1;
  }
  return eff;
}

/**
 * Estimate password strength.
 * @param {string} pw
 * @param {string[]} [userInputs] e.g. username/email — containing them is penalised
 * @returns {{score: 0|1|2|3|4, label: string, bits: number, warnings: string[]}}
 */
export function estimateStrength(pw, userInputs = []) {
  const warnings = [];
  const password = String(pw || '');
  if (!password) return { score: 0, label: LABELS[0], bits: 0, warnings };
  const lower = password.toLowerCase();
  const normalized = lower.replace(/[@4]/g, 'a').replace(/3/g, 'e').replace(/[1!|]/g, 'i').replace(/0/g, 'o').replace(/[$5]/g, 's');

  let bits = effectiveLength(password) * Math.log2(charsetSize(password));

  if (COMMON.includes(lower) || COMMON.includes(normalized)) {
    return { score: 0, label: LABELS[0], bits: Math.min(bits, 10), warnings: ['This is a very common password.'] };
  }
  for (const word of COMMON) {
    if (word.length >= 4 && (lower.includes(word) || normalized.includes(word))) {
      bits -= word.length * 3;
      warnings.push('Avoid common words and passwords.');
      break;
    }
  }
  for (const input of userInputs) {
    const v = String(input || '').toLowerCase().split('@')[0];
    if (v.length >= 3 && lower.includes(v)) {
      bits -= v.length * 3;
      warnings.push('Avoid using your name or email.');
      break;
    }
  }
  if (/(19|20)\d{2}/.test(password)) {
    bits -= 6;
    warnings.push('Years are easy to guess.');
  }
  if (/^[a-z]+\d{1,4}$/i.test(password) || /^\d+[a-z]+$/i.test(password)) {
    bits -= 8;
    warnings.push('A word followed by numbers is a predictable pattern.');
  }
  if (password.length < 12) warnings.push('Use at least 12 characters.');

  bits = Math.max(0, Math.round(bits));
  const score = bits < 28 ? 0 : bits < 40 ? 1 : bits < 60 ? 2 : bits < 80 ? 3 : 4;
  return { score, label: LABELS[score], bits, warnings: [...new Set(warnings)] };
}

const SETS = {
  lower: 'abcdefghijkmnopqrstuvwxyz', // no "l"
  upper: 'ABCDEFGHJKLMNPQRSTUVWXYZ',  // no "I", "O"
  digits: '23456789',                 // no "0", "1"
  symbols: '!#$%&*+-=?@^_~',
};

function randomInt(max, rng) {
  // Rejection sampling avoids modulo bias.
  const limit = Math.floor(0x100000000 / max) * max;
  const buf = new Uint32Array(1);
  for (;;) {
    rng(buf);
    if (buf[0] < limit) return buf[0] % max;
  }
}

/**
 * Generate a random password from unambiguous characters with at least one of each class.
 * @param {number} [length]
 * @param {{symbols?: boolean, rng?: (buf: Uint32Array) => void}} [opts]
 */
export function generatePassword(length = 20, { symbols = true, rng = (b) => crypto.getRandomValues(b) } = {}) {
  const classes = [SETS.lower, SETS.upper, SETS.digits, ...(symbols ? [SETS.symbols] : [])];
  const n = Math.max(length, classes.length);
  const all = classes.join('');
  const chars = classes.map((set) => set[randomInt(set.length, rng)]);
  while (chars.length < n) chars.push(all[randomInt(all.length, rng)]);
  // Fisher–Yates so the guaranteed characters aren't always first.
  for (let i = chars.length - 1; i > 0; i--) {
    const j = randomInt(i + 1, rng);
    [chars[i], chars[j]] = [chars[j], chars[i]];
  }
  return chars.join('');
}
