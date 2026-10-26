#!/usr/bin/env node
// Validates the MkDocs site structure (nav, links, anchors, ReadTheDocs config) without MkDocs installed.
//   node tools/openapi-check/docs-check.mjs [--root <ludworks-server dir>]
import { dirname, join, resolve } from 'node:path';
import { fileURLToPath } from 'node:url';
import { checkDocs } from './docs-lib.mjs';

const args = process.argv.slice(2);
const i = args.indexOf('--root');
const root = resolve(i >= 0 ? args[i + 1] : join(dirname(fileURLToPath(import.meta.url)), '..', '..'));
const { errors, warnings, pages, links } = checkDocs(root);
for (const w of warnings) console.log('warning:', w);
for (const e of errors) console.error('ERROR', e);
console.log(`${pages} pages, ${links} links checked.`);
console.log(errors.length ? `FAILED: ${errors.length} error(s)` : 'OK');
process.exit(errors.length ? 1 : 0);
