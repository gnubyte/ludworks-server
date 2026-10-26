// Tab-separated clipboard text (pure), compatible with Excel / Google Sheets / LibreOffice:
// fields containing tabs, newlines or quotes are quoted with doubled quotes.

/**
 * Serialise rows of strings as TSV (CRLF-free; "\n" between rows).
 * @param {string[][]} rows
 */
export function toTSV(rows) {
  return rows.map((row) => row.map((v) => {
    const s = v == null ? '' : String(v);
    return /[\t\n\r"]/.test(s) ? `"${s.replace(/"/g, '""')}"` : s;
  }).join('\t')).join('\n');
}

/**
 * Parse TSV text into rows of strings. Quoted fields may contain tabs and newlines; a single
 * trailing newline is ignored. Rows are padded to the widest row.
 * @param {string} text
 * @returns {string[][]}
 */
export function parseTSV(text) {
  const s = String(text ?? '').replace(/\r\n?/g, '\n');
  const rows = [];
  let row = [];
  let i = 0;
  let field = '';
  let atStart = true;
  while (i < s.length) {
    const ch = s[i];
    if (atStart && ch === '"') {
      // quoted field: only if it closes properly before a tab/newline/end
      let j = i + 1;
      let val = '';
      let closed = false;
      while (j < s.length) {
        if (s[j] === '"') {
          if (s[j + 1] === '"') { val += '"'; j += 2; continue; }
          closed = true;
          j += 1;
          break;
        }
        val += s[j];
        j += 1;
      }
      if (closed && (j >= s.length || s[j] === '\t' || s[j] === '\n')) {
        field = val;
        i = j;
        atStart = false;
        continue;
      }
    }
    atStart = false;
    if (ch === '\t') {
      row.push(field);
      field = '';
      atStart = true;
    } else if (ch === '\n') {
      row.push(field);
      rows.push(row);
      row = [];
      field = '';
      atStart = true;
    } else field += ch;
    i += 1;
  }
  if (!atStart || field !== '' || row.length) {
    row.push(field);
    rows.push(row);
  }
  const width = rows.reduce((w, r) => Math.max(w, r.length), 0);
  for (const r of rows) while (r.length < width) r.push('');
  return rows;
}
