// Lyrics helpers (pure, no DOM): normalise GET /music/tracks/{id}/lyrics and find the line
// that is sung at a playback position (synced/LRC lyrics). Unit tested in
// web/tests/music/lyrics.test.mjs.

/**
 * Normalise a lyrics response: lines {time (seconds|null), text}; synced only when at least one
 * line has a time. Timed lines are kept in time order (ties keep their order); untimed lines of
 * synced lyrics stay where they were, attached to the timed line before them.
 * @param {any} raw {synced, lines: [{time_seconds, text}], text, source}
 * @returns {{synced: boolean, lines: {time: number|null, text: string}[], text: string, source: string}}
 */
export function normalizeLyrics(raw) {
  const lines = Array.isArray(raw?.lines) ? raw.lines : [];
  const norm = lines.map((l) => {
    const t = l?.time_seconds;
    const time = t === null || t === undefined || !Number.isFinite(Number(t)) || Number(t) < 0 ? null : Number(t);
    return { time, text: typeof l?.text === 'string' ? l.text : '' };
  });
  const synced = !!raw?.synced && norm.some((l) => l.time !== null);
  let text = typeof raw?.text === 'string' ? raw.text : '';
  if (!text && norm.length) text = norm.map((l) => l.text).join('\n');
  if (!synced) {
    const plain = norm.length ? norm.map((l) => ({ time: null, text: l.text })) : text.split(/\r?\n/).map((s) => ({ time: null, text: s }));
    return { synced: false, lines: trimBlank(plain), text, source: String(raw?.source || '') };
  }
  // Stable sort of timed groups: every untimed line travels with the timed line before it.
  const groups = [];
  for (const l of norm) {
    if (l.time !== null || !groups.length) groups.push({ time: l.time ?? 0, lines: [l] });
    else groups[groups.length - 1].lines.push(l);
  }
  const sorted = groups.map((g, i) => ({ ...g, i })).sort((a, b) => a.time - b.time || a.i - b.i);
  return { synced: true, lines: sorted.flatMap((g) => g.lines), text, source: String(raw?.source || '') };
}

function trimBlank(lines) {
  let a = 0;
  let b = lines.length;
  while (a < b && !lines[a].text.trim()) a += 1;
  while (b > a && !lines[b - 1].text.trim()) b -= 1;
  return lines.slice(a, b);
}

/**
 * Index of the line sung at `t` seconds: the last timed line starting at or before t (binary
 * search over the timed lines, which are in time order); -1 before the first line.
 * @param {{time: number|null}[]} lines
 * @param {number} t
 * @param {number} [offset] seconds added to t (show a line a little early)
 */
export function activeLineIndex(lines, t, offset = 0) {
  const pos = Number(t) + Number(offset || 0);
  if (!Array.isArray(lines) || !lines.length || !Number.isFinite(pos)) return -1;
  // Indexes of timed lines, in order.
  let lo = 0;
  let hi = lines.length - 1;
  let best = -1;
  // Untimed lines inherit the previous time, so a linear fallback keeps that rule simple when
  // they exist; the common case (every line timed) is a plain binary search.
  if (lines.every((l) => l.time !== null)) {
    while (lo <= hi) {
      const mid = (lo + hi) >> 1;
      if (lines[mid].time <= pos) {
        best = mid;
        lo = mid + 1;
      } else {
        hi = mid - 1;
      }
    }
    return best;
  }
  for (let i = 0; i < lines.length; i++) {
    const time = lines[i].time;
    if (time === null) continue;
    if (time <= pos) best = i;
    else break;
  }
  return best;
}

/** "1:23.45" (minutes:seconds.hundredths) for a time in seconds. */
export function formatLrcTime(sec) {
  const s = Math.max(0, Number(sec) || 0);
  const m = Math.floor(s / 60);
  const rest = s - m * 60;
  const whole = Math.floor(rest);
  const cs = Math.floor((rest - whole) * 100 + 1e-6);
  return `${m}:${String(whole).padStart(2, '0')}.${String(cs).padStart(2, '0')}`;
}
