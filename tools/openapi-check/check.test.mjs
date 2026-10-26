// Unit tests: node --test tools/openapi-check/
import { test } from 'node:test';
import assert from 'node:assert/strict';
import { mkdtempSync, mkdirSync, writeFileSync, readFileSync, rmSync } from 'node:fs';
import { tmpdir } from 'node:os';
import { join, dirname } from 'node:path';
import { fileURLToPath } from 'node:url';
import {
  MODULES, BASE_PATH, fragmentPath, mergeFragments, validateDocument, parseApiMd, compareWithApiMd,
  generateReference, normalizeApiPath, resolvePointer, slugify, deepEqual, operationKeys, validateValue,
} from './lib.mjs';
import { parseNav, extractLinks, headingAnchors, stripCode, checkDocs } from './docs-lib.mjs';

const ROOT = join(dirname(fileURLToPath(import.meta.url)), '..', '..');

// A minimal valid document following the Ludworks conventions.
function minimalDoc() {
  return {
    openapi: '3.1.0',
    info: { title: 'T', version: '1' },
    tags: [{ name: 'A', description: 'a' }],
    paths: {
      '/api/v1/things/{id}': {
        get: {
          operationId: 'getThing', tags: ['A'], summary: 's', description: 'd\n\n**Scope:** `files:read`', 'x-scope': 'files:read',
          parameters: [{ name: 'id', in: 'path', required: true, schema: { $ref: '#/components/schemas/Id' } }],
          responses: { 200: { description: 'ok', content: { 'application/json': { schema: { $ref: '#/components/schemas/Thing' } } } } },
        },
      },
    },
    components: {
      schemas: {
        Id: { type: 'string' },
        Thing: { type: 'object', required: ['id', 'type'], properties: { id: { $ref: '#/components/schemas/Id' }, type: { type: 'string' } } },
      },
      securitySchemes: { bearerAuth: { type: 'http', scheme: 'bearer' } },
    },
    security: [{ bearerAuth: [] }],
  };
}
const op = (d) => d.paths['/api/v1/things/{id}'].get;

test('deepEqual ignores key order', () => {
  assert.ok(deepEqual({ a: 1, b: [1, { c: 2 }] }, { b: [1, { c: 2 }], a: 1 }));
  assert.ok(!deepEqual({ a: 1 }, { a: 1, b: undefined }));
  assert.ok(!deepEqual([1, 2], [2, 1]));
});

test('mergeFragments merges paths, components and tags', () => {
  const base = { openapi: '3.1.0', tags: [{ name: 'A', description: 'a' }], paths: { '/x': { get: { operationId: 'x' } } }, components: { schemas: { S: { type: 'string' } } } };
  const { doc, errors } = mergeFragments(base, [
    { name: 'f1', doc: { tags: [{ name: 'A', description: 'a' }, { name: 'B' }], paths: { '/x': { post: { operationId: 'y' } }, '/z': { get: {} } }, components: { schemas: { S: { type: 'string' }, T: { type: 'integer' } } } } },
  ]);
  assert.deepEqual(errors, []);
  assert.deepEqual(Object.keys(doc.paths['/x']).sort(), ['get', 'post']);
  assert.ok(doc.paths['/z'].get);
  assert.deepEqual(doc.tags.map((t) => t.name), ['A', 'B']);
  assert.equal(doc.components.schemas.T.type, 'integer');
  assert.equal(base.paths['/x'].post, undefined, 'base must not be mutated');
});

test('mergeFragments reports duplicates and conflicts', () => {
  const base = { tags: [{ name: 'A', description: 'a' }], paths: { '/x': { get: {} } }, components: { schemas: { S: { type: 'string' } } } };
  const { doc, errors, warnings } = mergeFragments(base, [
    { name: 'f1', doc: { paths: { '/x': { get: {} } } } },
    { name: 'f2', doc: { components: { schemas: { S: { type: 'integer' } } } } },
    { name: 'f3', doc: { tags: [{ name: 'A', description: 'other' }] } },
    { name: 'f4', doc: { info: {}, 'x-note': 1 } },
    { name: 'f5', doc: { components: { securitySchemes: { x: {} } } } },
    { name: 'f6', doc: [] },
    { name: 'f7', doc: { webhooks: {} } },
    { name: 'f8', doc: { paths: { '/x': { parameters: [1] } } } },
    { name: 'f9', doc: { paths: { '/x': { parameters: [2] } } } },
  ]);
  const has = (list, re) => assert.ok(list.some((e) => re.test(e)), `expected ${re} in ${JSON.stringify(list)}`);
  has(errors, /f1: duplicate operation GET \/x/);
  has(errors, /f2: conflicting definition of components\.schemas\.S/);
  has(warnings, /f3: tag "A" differs from its first definition/);
  assert.equal(doc.tags[0].description, 'a', 'first tag definition wins (like the server)');
  has(warnings, /f4: top-level key "info" is ignored/);
  has(warnings, /f4: top-level key "x-note" is ignored/);
  has(errors, /f5: components\.securitySchemes is not allowed/);
  has(errors, /f6: fragment must be a JSON object/);
  has(errors, /f7: unknown top-level key "webhooks"/);
  has(errors, /f9: conflicting path-item key "parameters" on \/x/);
  assert.equal(errors.length, 6, JSON.stringify(errors));
});

test('resolvePointer handles escapes and misses', () => {
  const d = { a: { 'b/c': { '~d': 1 } } };
  assert.equal(resolvePointer(d, '#/a/b~1c/~0d'), 1);
  assert.equal(resolvePointer(d, '#/a/missing'), undefined);
  assert.equal(resolvePointer(d, 'other.json#/a'), undefined);
  assert.equal(resolvePointer(d, '#/%E0%A4%A'), undefined);
});

test('validateDocument accepts a minimal valid document', () => {
  const r = validateDocument(minimalDoc());
  assert.deepEqual(r.errors, []);
  assert.deepEqual(r.stats, { paths: 1, operations: 1, schemas: 2 });
});

test('validateDocument catches structural problems', () => {
  const cases = [
    [(d) => { op(d).responses[200].content['application/json'].schema.$ref = '#/components/schemas/Nope'; }, /unresolved \$ref/],
    [(d) => { op(d).parameters = []; }, /path parameter \{id\} is not declared/],
    [(d) => { op(d).parameters.push({ name: 'x', in: 'path', required: true, schema: {} }); }, /declared path parameter "x" is not in the path/],
    [(d) => { op(d).parameters[0].required = false; }, /must be required/],
    [(d) => { delete op(d)['x-scope']; }, /missing x-scope/],
    [(d) => { op(d)['x-scope'] = 'files:everything'; }, /invalid x-scope/],
    [(d) => { op(d)['x-scope'] = 'public'; op(d).description = 'd\n\n**Scope:** `public`'; }, /x-scope public requires "security": \[\]/],
    [(d) => { op(d).security = []; }, /requires x-scope public/],
    [(d) => { op(d).description = 'd\n\n**Scope:** `files:write`'; }, /description says scope files:write/],
    [(d) => { op(d).description = 'no scope line'; }, /must state/],
    [(d) => { op(d).operationId = 'Get_thing'; }, /camelCase/],
    [(d) => { op(d).tags = ['Missing']; }, /not declared in top-level tags/],
    [(d) => { delete op(d).tags; }, /at least one tag/],
    [(d) => { op(d).responses = {}; }, /needs responses/],
    [(d) => { op(d).responses = { 400: { description: 'x' } }; }, /needs a 2xx\/3xx response/],
    [(d) => { op(d).responses[200] = { content: {} }; }, /needs a description/],
    [(d) => { op(d).responses.abc = { description: 'x' }; }, /invalid response key/],
    [(d) => { op(d).requestBody = { content: { 'application/json': {} } }; }, /GET must not have a requestBody/],
    [(d) => { op(d).security = [{ nope: [] }]; }, /unknown security scheme/],
    [(d) => { op(d).foo = 1; }, /unknown operation key "foo"/],
    [(d) => { d.bogus = 1; }, /unknown top-level key "bogus"/],
    [(d) => { d.openapi = '3.0.3'; }, /3\.1/],
    [(d) => { d.components.schemas.Thing.required.push('idd'); }, /required property "idd"/],
    [(d) => { d.components.schemas.Thing.properties.id = { type: 'str' }; }, /invalid JSON Schema type/],
    [(d) => { d.components.schemas.Thing.nullable = true; }, /"nullable" is OpenAPI 3\.0/],
    [(d) => { op(d).parameters.push({ name: 'Content-Type', in: 'header', schema: {} }); }, /ignored by OpenAPI/],
    [(d) => { op(d).parameters.push({ name: 'access_token', in: 'query', schema: {} }); }, /accessTokenQuery security scheme/],
    [(d) => { d.paths['/api/v1/things/{other}'] = structuredClone(d.paths['/api/v1/things/{id}']); }, /equivalent templates/],
    [(d) => { d.paths['/api/v1/two'] = { get: { ...structuredClone(op(d)), parameters: [] } }; }, /already used by/],
  ];
  for (const [mutate, re] of cases) {
    const d = minimalDoc();
    mutate(d);
    const { errors } = validateDocument(d);
    assert.ok(errors.some((e) => re.test(e)), `expected ${re}, got ${JSON.stringify(errors)}`);
  }
});

test('validateDocument enforces query-token conventions', () => {
  const d = minimalDoc();
  d.components.securitySchemes.accessTokenQuery = { type: 'apiKey', in: 'query', name: 'access_token' };
  op(d).security = [{ bearerAuth: [] }, { accessTokenQuery: [] }];
  assert.ok(validateDocument(d).errors.some((e) => /mention `\?access_token=`/.test(e)));
  op(d).description += ' · also accepts `?access_token=`';
  assert.deepEqual(validateDocument(d).errors, []);
  d.paths['/api/v1/things/{id}'].post = { ...structuredClone(op(d)), operationId: 'postThing' };
  assert.ok(validateDocument(d).errors.some((e) => /only allowed on GET/.test(e)));
});

test('validateValue covers the schema keywords used in the fragments', () => {
  const d = { components: { schemas: { Id: { type: 'string', pattern: '^[a-z]{3}$' } } } };
  const S = {
    type: 'object', required: ['id', 'n'], additionalProperties: false,
    properties: {
      id: { $ref: '#/components/schemas/Id' },
      n: { type: ['integer', 'null'], minimum: 1, maximum: 5 },
      tags: { type: 'array', items: { type: 'string', maxLength: 3 }, uniqueItems: true, maxItems: 2 },
      kind: { enum: ['a', 'b'] }, v: { const: 1 },
      any: { anyOf: [{ type: 'string' }, { type: 'boolean' }] },
      one: { oneOf: [{ type: 'number' }, { type: 'integer' }] },
      map: { type: 'object', propertyNames: { pattern: '^[A-Z]+[0-9]+$' }, additionalProperties: { type: 'number' } },
      both: { allOf: [{ type: 'object', required: ['x'] }, { type: 'object', required: ['y'] }] },
    },
  };
  assert.deepEqual(validateValue(d, S, { id: 'abc', n: null, tags: ['x'], kind: 'a', v: 1, any: true, map: { A1: 2 }, both: { x: 1, y: 2 } }), []);
  const problems = validateValue(d, S, { id: 'ABCD', n: 9, tags: ['x', 'x', 'long'], kind: 'c', v: 2, any: 3, one: 2, map: { a1: 'no' }, both: { x: 1 }, extra: 1 });
  for (const re of [/\$\.id: does not match/, /above maximum 5/, /more than 2 items/, /items not unique/, /longer than 3/, /not in enum/, /must equal 1/, /none of anyOf/,
    /exactly one of oneOf/, /\$\.map\{a1\}: does not match/, /\$\.map\.a1: expected number/, /missing required "y"/, /unexpected property "extra"/]) {
    assert.ok(problems.some((p) => re.test(p)), `expected ${re} in ${JSON.stringify(problems)}`);
  }
  assert.deepEqual(validateValue(d, S, { id: 'abc' }), ['$: missing required "n"']);
  assert.deepEqual(validateValue(d, { type: 'number' }, 3), [], 'integers are numbers');
});

test('validateDocument checks media-type and schema examples', () => {
  const d = minimalDoc();
  op(d).responses[200].content['application/json'].example = { id: 'x' };
  d.components.schemas.Thing.examples = [{ id: 'a', type: 1 }];
  const { errors } = validateDocument(d);
  assert.ok(errors.some((e) => /content\/application~1json\/example: example invalid: \$: missing required "type"/.test(e)), JSON.stringify(errors));
  assert.ok(errors.some((e) => /schemas\/Thing\/examples\/0: example invalid: \$\.type: expected string/.test(e)), JSON.stringify(errors));
});

test('strict example lint reports undocumented properties, including across allOf', () => {
  const d = { components: { schemas: {
    Base: { type: 'object', properties: { id: { type: 'string' } } },
    Ext: { allOf: [{ $ref: '#/components/schemas/Base' }, { type: 'object', properties: { extra: { type: 'object', properties: { a: { type: 'integer' } } } } }] },
    Map: { type: 'object', additionalProperties: { type: 'integer' } },
  } } };
  const ext = { $ref: '#/components/schemas/Ext' };
  assert.deepEqual(validateValue(d, ext, { id: 'x', extra: { a: 1 } }, '$', 0, true), []);
  assert.deepEqual(validateValue(d, ext, { id: 'x', idd: 1, extra: { b: 1 } }, '$', 0, true).sort(), ['$.extra: undocumented property "b"', '$: undocumented property "idd"']);
  assert.deepEqual(validateValue(d, ext, { idd: 1 }), [], 'non-strict validation follows JSON Schema (open objects)');
  assert.deepEqual(validateValue(d, { $ref: '#/components/schemas/Map' }, { anything: 1 }, '$', 0, true), [], 'maps are not linted');
});

test('validateDocument does not treat a property named "type" as the keyword', () => {
  const d = minimalDoc();
  d.components.schemas.Thing.properties.type = { type: 'object', properties: { type: { type: 'string' } } };
  assert.deepEqual(validateDocument(d).errors, []);
});

test('normalizeApiPath', () => {
  assert.equal(normalizeApiPath('/files?space_id=&path='), '/api/v1/files');
  assert.equal(normalizeApiPath('/api/v1/uploads/<id>'), '/api/v1/uploads/{id}');
  assert.equal(normalizeApiPath('/api/openapi.json'), '/api/openapi.json');
});

test('parseApiMd understands every shorthand used in API.md', () => {
  const md = [
    '| Method & path | Scope | Notes |',
    '|---|---|---|',
    '| `GET /api/v1/health` | public | `{"status":"ok"}` |',
    '| `GET /files?sort=name|size&order=asc|desc` | files:read | x |',
    '| `GET /admin/spaces` / `POST /admin/spaces` | admin | `{"name"}` |',
    '| `PATCH /admin/spaces/{id}` / `DELETE /admin/spaces/{id}?force=` | | |',
    '| `GET /tv/channels/{id}` · `PATCH` · `DELETE` | tv:read | |',
    '| `PUT /tv/channels/{id}/logo` / `DELETE` | tv:write | raw |',
    '| `GET/POST /vault/folders`, `PUT/DELETE /vault/folders/{id}` | vault | |',
    '| `GET /hdhr/{t}/discover.json`, `/lineup.json`, `/device.xml` | public | |',
    '* `OPTIONS /api/v1/uploads/` → 204 with `Tus-Version: 1.0.0`, `GET /ignored`',
    '* `HEAD /api/v1/uploads/<id>` → 200',
    '* `general`: `server_name`',
    '```',
    '| `GET /in/code/block` | x | x |',
    '```',
  ].join('\n');
  const { endpoints, errors } = parseApiMd(md);
  assert.deepEqual(errors, []);
  assert.deepEqual(endpoints.map((e) => e.key), [
    'GET /api/v1/health', 'GET /api/v1/files',
    'GET /api/v1/admin/spaces', 'POST /api/v1/admin/spaces',
    'PATCH /api/v1/admin/spaces/{id}', 'DELETE /api/v1/admin/spaces/{id}',
    'GET /api/v1/tv/channels/{id}', 'PATCH /api/v1/tv/channels/{id}', 'DELETE /api/v1/tv/channels/{id}',
    'PUT /api/v1/tv/channels/{id}/logo', 'DELETE /api/v1/tv/channels/{id}/logo',
    'GET /api/v1/vault/folders', 'POST /api/v1/vault/folders', 'PUT /api/v1/vault/folders/{id}', 'DELETE /api/v1/vault/folders/{id}',
    'GET /api/v1/hdhr/{t}/discover.json', 'GET /api/v1/hdhr/{t}/lineup.json', 'GET /api/v1/hdhr/{t}/device.xml',
    'OPTIONS /api/v1/uploads/', 'HEAD /api/v1/uploads/{id}',
  ]);
  assert.equal(endpoints[0].line, 3);
});

test('parseApiMd reports duplicates and orphan methods', () => {
  const { errors } = parseApiMd('| `GET /a` | x |\n| `GET /a?x=1` | x |\n| `PATCH` | x |');
  assert.ok(errors.some((e) => /GET \/api\/v1\/a already listed on line 1/.test(e)));
  assert.ok(errors.some((e) => /without a path/.test(e)));
});

test('compareWithApiMd lists missing and extra operations', () => {
  const d = minimalDoc();
  const { errors, count } = compareWithApiMd(d, '| `GET /things/{id}` | x |\n| `DELETE /things/{id}` | x |');
  assert.equal(count, 2);
  assert.deepEqual(errors, ['missing in OpenAPI: DELETE /api/v1/things/{id} (API.md line 2)']);
  const r2 = compareWithApiMd(d, '| `GET /other` | x |');
  assert.ok(r2.errors.includes('not listed in API.md: GET /api/v1/things/{id}'));
});

test('generateReference renders grouped tables', () => {
  const d = minimalDoc();
  d.components.securitySchemes.accessTokenQuery = { type: 'apiKey', in: 'query', name: 'access_token' };
  op(d).security = [{ accessTokenQuery: [] }];
  op(d).summary = 'Get | thing';
  const md = generateReference(d);
  assert.match(md, /^# Endpoint reference/);
  assert.match(md, /## A\n\na\n/);
  assert.match(md, /\| GET \| `\/api\/v1\/things\/\{id\}` \| `files:read` · qt \| Get \\\| thing \|/);
});

test('slugify matches Python-Markdown toc', () => {
  assert.equal(slugify('Resumable uploads (tus)'), 'resumable-uploads-tus');
  assert.equal(slugify('  SMTP / IMAP & mail-in '), 'smtp-imap-mail-in');
  assert.equal(slugify('files.max_upload_bytes'), 'filesmax_upload_bytes');
  assert.equal(slugify('Café — déjà vu'), 'cafe-deja-vu');
});

test('parseNav reads page entries and skips sections and URLs', () => {
  const nav = parseNav([
    'site_name: X', 'nav:', '  - Home: index.md', '  - Guide:', '      - "Files: basics": guide/files.md',
    '      - guide/tv.md', '  - GitHub: https://example.com/', 'theme:', '  name: readthedocs',
  ].join('\n'));
  assert.deepEqual(nav.map((n) => [n.title, n.file]), [['Home', 'index.md'], ['Files: basics', 'guide/files.md'], ['', 'guide/tv.md']]);
});

test('stripCode, extractLinks and headingAnchors', () => {
  const md = [
    '# Title', '', 'See [setup](setup.md#first-run) and `[not](a link)`.', '',
    '```', '[in code](nope.md)', '# not a heading', '```', '',
    '## Uploads (tus)', '## Uploads (tus)', '### Custom {#my-id}', 'Setext', '------', '',
    '[ref]: other.md', '![img](img/a.png "t")', '#### `smtp` settings',
  ].join('\n');
  assert.ok(!stripCode(md).includes('nope.md'));
  assert.deepEqual(extractLinks(md).map((l) => [l.target, l.line]), [['setup.md#first-run', 3], ['other.md', 16], ['img/a.png', 17]]);
  assert.deepEqual([...headingAnchors(md)].sort(), ['my-id', 'setext', 'smtp-settings', 'title', 'uploads-tus', 'uploads-tus_1'].sort());
});

test('checkDocs validates nav, links, anchors and RTD config on a fixture', () => {
  const dir = mkdtempSync(join(tmpdir(), 'lw-docs-'));
  try {
    mkdirSync(join(dir, 'docs', 'guide'), { recursive: true });
    mkdirSync(join(dir, 'docs', 'spec'), { recursive: true });
    writeFileSync(join(dir, 'mkdocs.yml'), 'site_name: X\ndocs_dir: docs\ntheme:\n  name: readthedocs\nnav:\n  - Home: index.md\n  - Files: guide/files.md\n  - Missing: guide/missing.md\n');
    writeFileSync(join(dir, 'docs', 'index.md'), '# Home\n\n[files](guide/files.md#upload) [bad](guide/nope.md) [abs](/api/docs) [dir](guide) [ok](guide/files.md#tagging)\n');
    writeFileSync(join(dir, 'docs', 'guide', 'files.md'), '# Files\n\n## Tagging\n');
    writeFileSync(join(dir, 'docs', 'orphan.md'), '# Orphan\n');
    writeFileSync(join(dir, 'docs', 'spec', 'X.md'), '# X\n[b](nope.md)\n');
    writeFileSync(join(dir, 'docs', 'requirements.txt'), 'mkdocs==1.6.1\n');
    writeFileSync(join(dir, '.readthedocs.yaml'), 'version: 2\nbuild:\n  os: ubuntu-24.04\n  tools:\n    python: "3.12"\nmkdocs:\n  configuration: mkdocs.yml\npython:\n  install:\n    - requirements: docs/requirements.txt\n');
    const { errors, warnings } = checkDocs(dir);
    const has = (list, re) => assert.ok(list.some((e) => re.test(e)), `expected ${re} in ${JSON.stringify(list)}`);
    has(errors, /nav entry guide\/missing\.md does not exist/);
    has(errors, /orphan\.md is not in the nav/);
    has(errors, /anchor #upload not found in guide\/files\.md/);
    has(errors, /broken link guide\/nope\.md/);
    has(errors, /absolute link \/api\/docs/);
    has(errors, /points to a directory/);
    has(warnings, /spec\/X\.md is not in the nav/);
    has(warnings, /spec\/X\.md:2: broken link nope\.md/);
    assert.equal(errors.length, 6, JSON.stringify(errors));
    writeFileSync(join(dir, 'docs', 'requirements.txt'), 'mkdocs>=1\nmarkdown\n');
    has(checkDocs(dir).errors, /must pin exactly one requirement/);
  } finally {
    rmSync(dir, { recursive: true, force: true });
  }
});

// Integration: the real fragments in this repository must merge, validate and match API.md.
test('repository OpenAPI fragments are valid and match API.md', () => {
  const read = (rel) => JSON.parse(readFileSync(join(ROOT, rel), 'utf8'));
  const { doc, errors } = mergeFragments(read(BASE_PATH), MODULES.map((m) => ({ name: m, doc: read(fragmentPath(m)) })));
  assert.deepEqual(errors, []);
  assert.deepEqual(validateDocument(doc).errors, []);
  assert.deepEqual(compareWithApiMd(doc, readFileSync(join(ROOT, 'docs/spec/API.md'), 'utf8')).errors, []);
  const keys = operationKeys(doc);
  for (const k of ['PATCH /api/v1/uploads/{id}', 'GET /api/v1/hdhr/{feed_token}/device.xml', 'POST /api/v1/setup']) assert.ok(keys.includes(k), k);
  assert.equal(readFileSync(join(ROOT, 'docs/api/reference.md'), 'utf8').replace(/\r\n/g, '\n'), generateReference(doc));
});

test('repository docs site is structurally valid', () => {
  const { errors } = checkDocs(ROOT);
  assert.deepEqual(errors, []);
});
