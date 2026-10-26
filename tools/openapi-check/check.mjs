#!/usr/bin/env node
// Validates the Ludworks OpenAPI description exactly as the server assembles it:
// internal/apidocs/base.json + internal/<module>/openapi.json fragments, merged in module order.
//
//   node tools/openapi-check/check.mjs                  validate (exit 1 on errors)
//   node tools/openapi-check/check.mjs --write-reference regenerate docs/api/reference.md
//   node tools/openapi-check/check.mjs --out merged.json also write the merged document
//   options: --root <ludworks-server dir> (default: two levels above this script), --verbose
import { readFileSync, writeFileSync, existsSync } from 'node:fs';
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { MODULES, BASE_PATH, fragmentPath, mergeFragments, validateDocument, compareWithApiMd, generateReference } from './lib.mjs';

const args = process.argv.slice(2);
const opt = (name) => { const i = args.indexOf(name); return i >= 0 ? args[i + 1] : undefined; };
const root = resolve(opt('--root') ?? join(dirname(fileURLToPath(import.meta.url)), '..', '..'));
const REFERENCE = 'docs/api/reference.md';
const API_MD = 'docs/spec/API.md';

const errors = []; const warnings = [];
function readJSON(rel) {
  const p = join(root, rel);
  if (!existsSync(p)) { errors.push(`${rel}: file not found`); return null; }
  try { return JSON.parse(readFileSync(p, 'utf8')); } catch (e) { errors.push(`${rel}: invalid JSON: ${e.message}`); return null; }
}

const base = readJSON(BASE_PATH);
const fragments = [];
for (const m of MODULES) {
  const doc = readJSON(fragmentPath(m));
  if (doc) fragments.push({ name: fragmentPath(m), doc });
}
if (!base) { for (const e of errors) console.error('ERROR', e); process.exit(1); }

const merged = mergeFragments(base, fragments);
errors.push(...merged.errors);
warnings.push(...merged.warnings);
const v = validateDocument(merged.doc);
errors.push(...v.errors);
warnings.push(...v.warnings);
// Schemas defined by a module but never referenced are probably stale.
const baseSchemas = new Set(Object.keys(base.components?.schemas ?? {}));
for (const s of v.unusedSchemas) if (!baseSchemas.has(s)) warnings.push(`schema ${s} is never referenced`);

let apiCount = 0;
if (existsSync(join(root, API_MD))) {
  const cmp = compareWithApiMd(merged.doc, readFileSync(join(root, API_MD), 'utf8'));
  errors.push(...cmp.errors);
  apiCount = cmp.count;
} else errors.push(`${API_MD}: not found`);

const reference = generateReference(merged.doc);
const refPath = join(root, REFERENCE);
if (args.includes('--write-reference')) {
  writeFileSync(refPath, reference);
  console.log(`wrote ${REFERENCE}`);
} else if (!existsSync(refPath) || readFileSync(refPath, 'utf8').replace(/\r\n/g, '\n') !== reference) {
  errors.push(`${REFERENCE} is out of date: run node tools/openapi-check/check.mjs --write-reference`);
}
const out = opt('--out');
if (out) { writeFileSync(resolve(out), JSON.stringify(merged.doc, null, 2) + '\n'); console.log(`wrote ${out}`); }

for (const w of warnings) console.log('warning:', w);
for (const e of errors) console.error('ERROR', e);
console.log(`${fragments.length} fragments, ${v.stats.paths} paths, ${v.stats.operations} operations, ${v.stats.schemas} schemas; API.md lists ${apiCount} endpoints.`);
console.log(errors.length ? `FAILED: ${errors.length} error(s)` : 'OK');
process.exit(errors.length ? 1 : 0);
