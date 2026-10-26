// Spreadsheet number formats for display (pure). Covers what the toolbar offers and the common
// Excel codes: sections (pos;neg;zero;text), colours, literals/quotes/escapes, 0 # ? digits,
// thousands and scaling commas, %, scientific, currency, and date/time codes on 1900 serials.

const MONTHS = ['January', 'February', 'March', 'April', 'May', 'June', 'July', 'August', 'September', 'October', 'November', 'December'];
const DAYS = ['Sunday', 'Monday', 'Tuesday', 'Wednesday', 'Thursday', 'Friday', 'Saturday'];
const COLORS = { black: '#000000', white: '#ffffff', red: '#d93025', green: '#188038', blue: '#1a73e8', yellow: '#f9ab00', magenta: '#c026d3', cyan: '#0891b2' };

/**
 * Text for a number in "General" format: up to 10 significant digits, exponent for very large
 * or tiny magnitudes.
 * @param {number} n
 */
export function generalNumber(n) {
  if (!Number.isFinite(n)) return '#NUM!';
  if (n === 0) return '0';
  const abs = Math.abs(n);
  if (abs >= 1e11 || abs < 1e-9) {
    const [mant, exp] = n.toExponential(5).split('e');
    const m = mant.replace(/\.?0+$/, '');
    const e = Number(exp);
    return `${m}E${e < 0 ? '-' : '+'}${String(Math.abs(e)).padStart(2, '0')}`;
  }
  if (Number.isInteger(n)) return String(n);
  let s = n.toPrecision(10);
  if (s.includes('e')) s = String(Number(s));
  if (s.includes('.')) s = s.replace(/0+$/, '').replace(/\.$/, '');
  return s;
}

/** Split a format code into sections at ";" (outside quotes, brackets and escapes). */
function sections(code) {
  const out = [];
  let cur = '';
  for (let i = 0; i < code.length; i++) {
    const ch = code[i];
    if (ch === '"') {
      const j = code.indexOf('"', i + 1);
      const end = j < 0 ? code.length : j;
      cur += code.slice(i, end + 1);
      i = end;
    } else if (ch === '\\' || ch === '_' || ch === '*') {
      cur += code.slice(i, i + 2);
      i += 1;
    } else if (ch === '[') {
      const j = code.indexOf(']', i);
      const end = j < 0 ? code.length : j;
      cur += code.slice(i, end + 1);
      i = end;
    } else if (ch === ';') {
      out.push(cur);
      cur = '';
    } else cur += ch;
  }
  out.push(cur);
  return out;
}

/** Tokenize one section into {t, v} parts. */
function lex(sec) {
  const toks = [];
  let color = null;
  let cond = null;
  let i = 0;
  const lit = (v) => toks.push({ t: 'lit', v });
  while (i < sec.length) {
    const ch = sec[i];
    const rest = sec.slice(i);
    let m;
    if (ch === '"') {
      const j = sec.indexOf('"', i + 1);
      lit(sec.slice(i + 1, j < 0 ? sec.length : j));
      i = j < 0 ? sec.length : j + 1;
    } else if (ch === '\\') {
      lit(sec[i + 1] || '');
      i += 2;
    } else if (ch === '_') {
      lit(' ');
      i += 2;
    } else if (ch === '*') {
      i += 2;
    } else if (ch === '[') {
      const j = sec.indexOf(']', i);
      const inner = sec.slice(i + 1, j < 0 ? sec.length : j);
      i = j < 0 ? sec.length : j + 1;
      const lower = inner.toLowerCase();
      if (COLORS[lower]) color = COLORS[lower];
      else if (/^color\d+$/.test(lower)) color = null;
      else if ((m = /^\$([^-\]]*)/.exec(inner))) lit(m[1]);
      else if (/^[<>=]/.test(inner)) cond = inner;
      else if (/^(h+|m+|s+)$/i.test(inner)) toks.push({ t: 'date', v: inner.toLowerCase()[0], elapsed: true, n: inner.length });
    } else if ((m = /^(am\/pm|a\/p)/i.exec(rest))) {
      toks.push({ t: 'ampm', v: m[0] });
      i += m[0].length;
    } else if ((m = /^(y+|m+|d+|h+|s+)/i.exec(rest))) {
      toks.push({ t: 'date', v: m[0][0].toLowerCase(), n: m[0].length });
      i += m[0].length;
    } else if ((m = /^[eE][+-]/.exec(rest))) {
      toks.push({ t: 'exp', v: m[0] });
      i += 2;
    } else if ('0#?'.includes(ch)) {
      toks.push({ t: 'digit', v: ch });
      i += 1;
    } else if (ch === '.') {
      toks.push({ t: 'point' });
      i += 1;
    } else if (ch === ',') {
      toks.push({ t: 'comma' });
      i += 1;
    } else if (ch === '%') {
      toks.push({ t: 'pct' });
      i += 1;
    } else if (ch === '@') {
      toks.push({ t: 'text' });
      i += 1;
    } else {
      lit(ch);
      i += 1;
    }
  }
  return { toks, color, cond };
}

function compileSection(sec) {
  const { toks, color, cond } = lex(sec);
  const isDate = toks.some((t) => t.t === 'date' || t.t === 'ampm');
  if (isDate) {
    // m/mm right after an hour or before seconds means minutes
    const dateToks = toks.filter((t) => t.t === 'date');
    for (let k = 0; k < dateToks.length; k++) {
      const t = dateToks[k];
      if (t.v !== 'm' || t.n > 2) continue;
      const prev = dateToks[k - 1];
      const next = dateToks[k + 1];
      if ((prev && prev.v === 'h') || (next && next.v === 's')) t.v = 'min';
    }
    // fractional seconds: "ss.00"
    for (let k = 0; k < toks.length; k++) {
      if (toks[k].t === 'date' && toks[k].v === 's' && toks[k + 1]?.t === 'point') {
        let n = 0;
        while (toks[k + 2 + n]?.t === 'digit' && toks[k + 2 + n].v === '0') n++;
        if (n) {
          toks[k].frac = n;
          toks.splice(k + 1, n + 1);
        }
      }
    }
    return { kind: 'date', toks, color, cond, ampm: toks.some((t) => t.t === 'ampm') };
  }
  const digitIdx = toks.map((t, i) => (t.t === 'digit' ? i : -1)).filter((i) => i >= 0);
  if (!digitIdx.length) {
    return { kind: toks.some((t) => t.t === 'text') ? 'text' : 'literal', toks, color, cond, pct: toks.filter((t) => t.t === 'pct').length };
  }
  const first = digitIdx[0];
  const last = digitIdx[digitIdx.length - 1];
  const prefix = [];
  const suffix = [];
  let intMin = 0;
  let fracMin = 0;
  let fracMax = 0;
  let thousands = false;
  let scale = 0;
  let inFrac = false;
  let exp = null;
  let expDigits = 0;
  let pct = 0;
  for (let k = 0; k < toks.length; k++) {
    const t = toks[k];
    if (t.t === 'pct') { pct++; (k < first ? prefix : suffix).push({ t: 'lit', v: '%' }); continue; }
    if (k < first) { if (t.t !== 'comma') prefix.push(t); continue; }
    if (k > last && !exp) {
      if (t.t === 'comma') { scale++; continue; }
      suffix.push(t);
      continue;
    }
    if (exp) {
      if (t.t === 'digit') expDigits++;
      else suffix.push(t);
      continue;
    }
    switch (t.t) {
      case 'digit':
        if (inFrac) { fracMax++; if (t.v === '0') fracMin = fracMax; } else if (t.v === '0') intMin++;
        break;
      case 'point': inFrac = true; break;
      case 'comma':
        if (toks[k + 1]?.t === 'digit' && !inFrac) thousands = true;
        else if (!inFrac) scale++;
        break;
      case 'exp': exp = t.v; break;
      default: if (!inFrac) prefix.push(t); else suffix.push(t);
    }
  }
  // trailing commas directly after the last digit scale by 1000 each
  return { kind: 'number', prefix, suffix, intMin, fracMin, fracMax, thousands, scale, exp, expDigits, pct, color, cond };
}

const cache = new Map();

/** Compile (and cache) a format code into sections. */
export function compile(code) {
  let c = cache.get(code);
  if (!c) {
    c = sections(code).map(compileSection);
    if (cache.size > 500) cache.clear();
    cache.set(code, c);
  }
  return c;
}

function groupThousands(s) {
  return s.replace(/\B(?=(\d{3})+(?!\d))/g, ',');
}

function renderLits(parts, text) {
  let out = '';
  for (const p of parts) out += p.t === 'lit' ? p.v : p.t === 'text' ? text : '';
  return out;
}

function formatNumberSection(sec, x, withMinus) {
  let v = x;
  for (let i = 0; i < sec.pct; i++) v *= 100;
  for (let i = 0; i < sec.scale; i++) v /= 1000;
  const neg = v < 0;
  v = Math.abs(v);
  let body;
  if (sec.exp) {
    let e = v === 0 ? 0 : Math.floor(Math.log10(v));
    let mant = v === 0 ? 0 : v / 10 ** e;
    mant = Number(mant.toFixed(sec.fracMax));
    if (mant >= 10) { mant /= 10; e += 1; }
    let ms = mant.toFixed(sec.fracMax);
    if (sec.fracMax > sec.fracMin) ms = ms.replace(new RegExp(`0{1,${sec.fracMax - sec.fracMin}}$`), '').replace(/\.$/, sec.fracMin ? '.' : '');
    const es = String(Math.abs(e)).padStart(Math.max(1, sec.expDigits), '0');
    body = `${ms}E${e < 0 ? '-' : sec.exp[1] === '+' ? '+' : ''}${es}`;
  } else {
    let s = v.toFixed(sec.fracMax);
    if (s.includes('e')) s = BigInt(Math.round(v)).toString() + (sec.fracMax ? `.${'0'.repeat(sec.fracMax)}` : '');
    let [ip, fp = ''] = s.split('.');
    let keep = fp.length;
    while (keep > sec.fracMin && fp[keep - 1] === '0') keep--;
    fp = fp.slice(0, keep);
    if (ip === '0' && sec.intMin === 0) ip = '';
    ip = ip.padStart(sec.intMin, '0');
    if (sec.thousands) ip = groupThousands(ip);
    // Like Excel, optional decimals that round away still leave the point ("0.##" → "5.").
    body = sec.fracMax > 0 ? `${ip}.${fp}` : ip;
  }
  const isZero = !/[1-9]/.test(body.replace(/E.*$/, ''));
  const sign = neg && withMinus && !isZero ? '-' : '';
  return sign + renderLits(sec.prefix, '') + body + renderLits(sec.suffix, '');
}

// Excel 1900 date system: serial 1 = 1900-01-01, 60 = the fictitious 1900-02-29.
function serialParts(serial) {
  let days = Math.floor(serial);
  let secs = Math.round((serial - days) * 86400 * 1000) / 1000;
  if (secs >= 86400) { days += 1; secs -= 86400; }
  let y; let mo; let d; let wd;
  if (days === 60) { y = 1900; mo = 1; d = 29; wd = 3; } else {
    const base = Date.UTC(1899, 11, days < 60 ? 31 : 30);
    const dt = new Date(base + days * 86400000);
    y = dt.getUTCFullYear(); mo = dt.getUTCMonth(); d = dt.getUTCDate(); wd = dt.getUTCDay();
  }
  return { y, mo, d, wd, secs };
}

function formatDateSection(sec, serial) {
  if (serial < 0 || serial >= 2958466) return '#'.repeat(8);
  const p = serialParts(serial);
  const whole = Math.floor(p.secs);
  const hh = Math.floor(whole / 3600);
  const mi = Math.floor((whole % 3600) / 60);
  const ss = whole % 60;
  let out = '';
  for (const t of sec.toks) {
    switch (t.t) {
      case 'lit': out += t.v; break;
      case 'ampm': {
        const pm = hh >= 12;
        const up = t.v[0] === t.v[0].toUpperCase();
        const s = t.v.length > 3 ? (pm ? 'PM' : 'AM') : (pm ? 'P' : 'A');
        out += up ? s : s.toLowerCase();
        break;
      }
      case 'date': {
        const n = t.n;
        switch (t.v) {
          case 'y': out += n <= 2 ? String(p.y % 100).padStart(2, '0') : String(p.y); break;
          case 'm':
            out += n === 1 ? String(p.mo + 1) : n === 2 ? String(p.mo + 1).padStart(2, '0') : n === 3 ? MONTHS[p.mo].slice(0, 3) : n === 5 ? MONTHS[p.mo][0] : MONTHS[p.mo];
            break;
          case 'd':
            out += n === 1 ? String(p.d) : n === 2 ? String(p.d).padStart(2, '0') : n === 3 ? DAYS[p.wd].slice(0, 3) : DAYS[p.wd];
            break;
          case 'h': {
            let hv = t.elapsed ? Math.floor(serial * 24) : hh;
            if (sec.ampm && !t.elapsed) { hv = hh % 12; if (hv === 0) hv = 12; }
            out += n >= 2 ? String(hv).padStart(2, '0') : String(hv);
            break;
          }
          case 'min': out += n >= 2 ? String(mi).padStart(2, '0') : String(mi); break;
          case 's': {
            out += n >= 2 ? String(ss).padStart(2, '0') : String(ss);
            if (t.frac) out += '.' + String(Math.round((p.secs - whole) * 10 ** t.frac)).padStart(t.frac, '0').slice(0, t.frac);
            break;
          }
          default: break;
        }
        break;
      }
      case 'digit': out += t.v === '0' ? '0' : ''; break;
      case 'point': out += '.'; break;
      case 'comma': out += ','; break;
      case 'pct': out += '%'; break;
      default: break;
    }
  }
  return out;
}

/**
 * Display text (and optional colour) of a cell value under a number format.
 * @param {any} v the cell's value
 * @param {string} t type code n|s|b|e|z
 * @param {string} [fmt] format code ("" / "general" = General)
 * @returns {{text: string, color?: string}}
 */
export function formatValue(v, t, fmt = '') {
  if (t === 'z' || v === null || v === undefined) return { text: '' };
  if (t === 'b') return { text: v ? 'TRUE' : 'FALSE' };
  if (t === 'e') return { text: String(v) };
  const code = fmt && fmt.toLowerCase() !== 'general' ? fmt : '';
  if (t === 's') {
    if (!code) return { text: String(v) };
    const secs = compile(code);
    const sec = secs.length >= 4 ? secs[3] : secs.find((s) => s.kind === 'text');
    if (!sec) return { text: String(v) };
    return { text: renderLits(sec.toks, String(v)), color: sec.color || undefined };
  }
  const n = Number(v);
  if (!code) return { text: generalNumber(n) };
  if (code === '@') return { text: generalNumber(n) };
  const secs = compile(code);
  let sec = secs[0];
  let withMinus = true;
  if (secs.length >= 2 && n < 0) { sec = secs[1]; withMinus = false; }
  if (secs.length >= 3 && n === 0) sec = secs[2];
  if (sec.cond && secs.length >= 2) {
    const m = /^([<>=]{1,2})(-?[\d.]+)$/.exec(sec.cond);
    if (m && !compare(n, m[1], Number(m[2]))) { sec = secs[1]; withMinus = !secs[1].cond; }
  }
  let text;
  switch (sec.kind) {
    case 'date': text = formatDateSection(sec, n); break;
    case 'number': text = formatNumberSection(sec, n, withMinus); break;
    case 'text': text = renderLits(sec.toks, generalNumber(n)); break;
    default: text = (withMinus && n < 0 ? '-' : '') + renderLits(sec.toks, '');
  }
  return { text, color: sec.color || undefined };
}

function compare(n, op, x) {
  switch (op) {
    case '<': return n < x;
    case '<=': return n <= x;
    case '>': return n > x;
    case '>=': return n >= x;
    case '=': return n === x;
    case '<>': return n !== x;
    default: return true;
  }
}

/** True when the format displays dates or times. */
export function isDateFormat(fmt) {
  if (!fmt || fmt.toLowerCase() === 'general') return false;
  return compile(fmt).some((s) => s.kind === 'date');
}

/** True for percent formats. */
export function isPercentFormat(fmt) {
  if (!fmt) return false;
  const s = compile(fmt)[0];
  return s.kind === 'number' && s.pct > 0;
}

/** ISO-like text of a serial for editing (date, or date + time when there is a time part). */
export function serialToText(serial, withTime) {
  const p = serialParts(serial);
  const date = `${p.y}-${String(p.mo + 1).padStart(2, '0')}-${String(p.d).padStart(2, '0')}`;
  const whole = Math.round(p.secs);
  const time = `${Math.floor(whole / 3600)}:${String(Math.floor((whole % 3600) / 60)).padStart(2, '0')}${whole % 60 ? `:${String(whole % 60).padStart(2, '0')}` : ''}`;
  if (Math.floor(serial) === 0 && withTime) return time;
  return withTime && p.secs > 0 ? `${date} ${time}` : date;
}

/** Number formats offered in the toolbar menu. */
export const FORMAT_PRESETS = [
  { fmt: '', label: 'Automatic', sample: '1000.5' },
  { fmt: '@', label: 'Plain text', sample: 'Text' },
  { fmt: '0', label: 'Number (no decimals)', sample: '1001' },
  { fmt: '0.00', label: 'Number', sample: '1000.50' },
  { fmt: '#,##0.00', label: 'Number with separators', sample: '1,000.50' },
  { fmt: '0%', label: 'Percent', sample: '10%' },
  { fmt: '0.00%', label: 'Percent (2 decimals)', sample: '10.25%' },
  { fmt: '$#,##0.00', label: 'Currency', sample: '$1,000.50' },
  { fmt: '$#,##0.00;[Red]-$#,##0.00', label: 'Currency (red negatives)', sample: '-$1,000.50' },
  { fmt: '0.00E+00', label: 'Scientific', sample: '1.00E+03' },
  { fmt: 'yyyy-mm-dd', label: 'Date', sample: '2026-10-26' },
  { fmt: 'mmm d, yyyy', label: 'Date (long)', sample: 'Oct 26, 2026' },
  { fmt: 'h:mm AM/PM', label: 'Time', sample: '3:59 PM' },
  { fmt: 'yyyy-mm-dd h:mm', label: 'Date and time', sample: '2026-10-26 15:59' },
];
