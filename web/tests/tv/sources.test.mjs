// Static checks over the TV app sources: every icon name referenced exists, no raw HTML
// sinks or inline styles (strict CSP), and every module parses.
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { readFileSync, readdirSync } from 'node:fs';
import { fileURLToPath } from 'node:url';
import { dirname, join } from 'node:path';
import { iconNames } from '../../assets/js/core/icons.js';
import { tvIconNames } from '../../assets/js/apps/tv/icons.js';

const dir = join(dirname(fileURLToPath(import.meta.url)), '../../assets/js/apps/tv');
const files = readdirSync(dir).filter((f) => f.endsWith('.js'));
const src = Object.fromEntries(files.map((f) => [f, readFileSync(join(dir, f), 'utf8')]));

test('every referenced icon exists', () => {
  const known = new Set([...iconNames, ...tvIconNames]);
  const patterns = [
    /tvIcon\('([a-z0-9-]+)'/g, /\bicon\('([a-z0-9-]+)'/g, /\bicon: '([a-z0-9-]+)'/g, /iconName: '([a-z0-9-]+)'/g,
    /\bic: '([a-z0-9-]+)'/g,
  ];
  const missing = [];
  for (const [f, s] of Object.entries(src)) {
    for (const re of patterns) {
      for (const m of s.matchAll(re)) if (!known.has(m[1])) missing.push(`${f}: ${m[1]}`);
    }
  }
  assert.deepEqual(missing, []);
});

test('no raw HTML sinks, eval or inline styles', () => {
  for (const [f, s] of Object.entries(src)) {
    assert.ok(!/\.innerHTML\s*=|outerHTML\s*=|insertAdjacentHTML|document\.write/.test(s), `${f} writes raw HTML`);
    assert.ok(!/\beval\(|new Function\(/.test(s), `${f} uses eval`);
    assert.ok(!/setAttribute\('style'|style="/.test(s), `${f} sets a style attribute`);
  }
});

test('TV modules expose the router contract', () => {
  assert.match(src['index.js'], /export async function mount\(container, ctx\)/);
  for (const f of ['live.js', 'guide.js', 'player.js', 'ondemand.js', 'channels.js', 'integrations.js']) {
    assert.match(src[f], /export async function render\(host, /, f);
  }
  assert.match(src['lineups.js'], /export async function renderList\(/);
  assert.match(src['lineups.js'], /export async function renderEditor\(/);
  // The library half is imported dynamically with a fallback.
  assert.match(src['index.js'], /import\('\.\/library\/index\.js'\)/);
  assert.match(src['lineups.js'], /import\('\.\/library\/index\.js'\)/);
});

test('no local variable shadows an imported name', () => {
  // Regression: ondemand.js declared `const poster = …` inside render(), shadowing the imported
  // poster() widget, so the "Up next" card threw "poster is not a function" on every tick.
  const all = { ...src };
  for (const f of readdirSync(join(dir, 'library')).filter((x) => x.endsWith('.js'))) all[`library/${f}`] = readFileSync(join(dir, 'library', f), 'utf8');
  const problems = [];
  for (const [f, s] of Object.entries(all)) {
    const imported = new Set();
    for (const m of s.matchAll(/^import\s*\{([^}]*)\}\s*from/gm)) {
      for (const part of m[1].split(',')) {
        const name = part.trim().split(/\s+as\s+/).pop();
        if (name) imported.add(name);
      }
    }
    for (const m of s.matchAll(/\b(?:const|let|var|function)\s+([A-Za-z_$][\w$]*)\b/g)) {
      if (imported.has(m[1])) problems.push(`${f}: ${m[1]}`);
    }
    for (const m of s.matchAll(/\b(?:const|let|var)\s*\{([^}]*)\}\s*=/g)) {
      for (const part of m[1].split(',')) {
        const name = part.trim().split(':').pop().split('=')[0].trim();
        if (imported.has(name)) problems.push(`${f}: ${name} (destructured)`);
      }
    }
  }
  assert.deepEqual(problems, []);
});
