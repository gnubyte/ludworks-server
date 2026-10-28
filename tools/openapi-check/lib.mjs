// Ludworks OpenAPI tooling: fragment merge (identical to the server's internal/apidocs merge),
// structural validation, API.md endpoint extraction and the static endpoint-reference page.
// Node >= 20, standard library only.

/** Module fragments in server wiring order (ARCHITECTURE.md §3.1). Merge order matters for tag order. */
export const MODULES = ['auth', 'admin', 'mail', 'files', 'media', 'tv', 'music', 'notes', 'vault', 'browser', 'sheets', 'kanban', 'cast', 'system'];
export const BASE_PATH = 'internal/apidocs/base.json';
export const fragmentPath = (mod) => `internal/${mod}/openapi.json`;

export const HTTP_METHODS = ['get', 'put', 'post', 'delete', 'options', 'head', 'patch', 'trace'];
const FRAGMENT_KEYS = new Set(['tags', 'paths', 'components']);
// Accepted but ignored by the server merger (owned by base.json).
const FRAGMENT_IGNORED_KEYS = new Set(['openapi', 'info', 'servers', 'security']);
// securitySchemes are global policy and may only be defined in base.json.
const FRAGMENT_COMPONENT_KINDS = new Set(['schemas', 'responses', 'parameters', 'examples', 'requestBodies', 'headers', 'links', 'callbacks', 'pathItems']);
const DOC_KEYS = new Set(['openapi', 'info', 'jsonSchemaDialect', 'servers', 'paths', 'webhooks', 'components', 'security', 'tags', 'externalDocs']);
const PATH_ITEM_KEYS = new Set([...HTTP_METHODS, '$ref', 'summary', 'description', 'servers', 'parameters']);
const OP_KEYS = new Set(['tags', 'summary', 'description', 'externalDocs', 'operationId', 'parameters', 'requestBody', 'responses', 'callbacks', 'deprecated', 'security', 'servers']);
const PARAM_IN = new Set(['path', 'query', 'header', 'cookie']);
const JSON_TYPES = new Set(['string', 'number', 'integer', 'boolean', 'array', 'object', 'null']);
export const SCOPES = new Set(['files:read', 'files:write', 'media:read', 'media:write', 'tv:read', 'tv:write', 'notes:read', 'notes:write',
  'vault:read', 'vault:write', 'sheets:read', 'sheets:write', 'music:read', 'music:write', 'kanban:read', 'kanban:write', 'cast',
  'account', 'admin']);
const X_SCOPE_VALUES = new Set([...SCOPES, 'public', 'auth']);

const isObj = (v) => v !== null && typeof v === 'object' && !Array.isArray(v);

/** Structural equality of JSON values (key order ignored). */
export function deepEqual(a, b) {
  if (a === b) return true;
  if (typeof a !== typeof b || a === null || b === null || typeof a !== 'object') return false;
  if (Array.isArray(a) !== Array.isArray(b)) return false;
  if (Array.isArray(a)) return a.length === b.length && a.every((v, i) => deepEqual(v, b[i]));
  const ka = Object.keys(a); const kb = Object.keys(b);
  return ka.length === kb.length && ka.every((k) => Object.hasOwn(b, k) && deepEqual(a[k], b[k]));
}

/**
 * Merges module fragments into a copy of the base document, with the same semantics as the server's
 * internal/apidocs.Build:
 *  - fragment top-level keys: tags, paths, components; openapi/info/servers/security/x-* are ignored
 *    (reported as warnings here), anything else is an error;
 *  - tags: appended by name, the first definition wins (a differing repeat is reported as a warning);
 *  - paths: a (path, method) pair may be defined once; other path-item keys must be deep-equal if repeated;
 *  - components.<kind>.<name>: defined once, or repeated deep-equal.
 * Policy checks beyond the server's (securitySchemes only in base) are reported as errors.
 * @param {object} base parsed base.json
 * @param {{name: string, doc: any}[]} fragments in merge order
 * @returns {{doc: object, errors: string[], warnings: string[]}}
 */
export function mergeFragments(base, fragments) {
  const errors = []; const warnings = [];
  const doc = structuredClone(base);
  doc.tags ??= [];
  doc.paths ??= {};
  doc.components ??= {};
  const tagIndex = new Map(doc.tags.map((t) => [t.name, t]));
  for (const { name, doc: frag } of fragments) {
    const where = `${name}`;
    if (!isObj(frag)) { errors.push(`${where}: fragment must be a JSON object`); continue; }
    for (const k of Object.keys(frag)) {
      if (FRAGMENT_IGNORED_KEYS.has(k) || k.startsWith('x-')) warnings.push(`${where}: top-level key "${k}" is ignored by the server merge`);
      else if (!FRAGMENT_KEYS.has(k)) errors.push(`${where}: unknown top-level key "${k}" (allowed: tags, paths, components)`);
    }
    if (frag.tags !== undefined) {
      if (!Array.isArray(frag.tags)) errors.push(`${where}: "tags" must be an array`);
      else for (const t of frag.tags) {
        if (!isObj(t) || typeof t.name !== 'string' || !t.name) { errors.push(`${where}: every tag needs a "name"`); continue; }
        const prev = tagIndex.get(t.name);
        if (!prev) { doc.tags.push(structuredClone(t)); tagIndex.set(t.name, t); }
        else if (!deepEqual(prev, t)) warnings.push(`${where}: tag "${t.name}" differs from its first definition (the first one wins)`);
      }
    }
    if (frag.paths !== undefined) {
      if (!isObj(frag.paths)) errors.push(`${where}: "paths" must be an object`);
      else for (const [p, item] of Object.entries(frag.paths)) {
        if (!isObj(item)) { errors.push(`${where}: path item ${p} must be an object`); continue; }
        const target = (doc.paths[p] ??= {});
        for (const [k, v] of Object.entries(item)) {
          if (HTTP_METHODS.includes(k)) {
            if (target[k]) errors.push(`${where}: duplicate operation ${k.toUpperCase()} ${p}`);
            else target[k] = structuredClone(v);
          } else if (Object.hasOwn(target, k) && !deepEqual(target[k], v)) {
            errors.push(`${where}: conflicting path-item key "${k}" on ${p}`);
          } else target[k] = structuredClone(v);
        }
      }
    }
    if (frag.components !== undefined) {
      if (!isObj(frag.components)) { errors.push(`${where}: "components" must be an object`); continue; }
      for (const [kind, entries] of Object.entries(frag.components)) {
        if (!isObj(entries)) { errors.push(`${where}: components.${kind} must be an object`); continue; }
        if (!FRAGMENT_COMPONENT_KINDS.has(kind)) errors.push(`${where}: components.${kind} is not allowed in a fragment (owned by base.json)`);
        const target = (doc.components[kind] ??= {});
        for (const [n, v] of Object.entries(entries)) {
          if (!Object.hasOwn(target, n)) target[n] = structuredClone(v);
          else if (!deepEqual(target[n], v)) errors.push(`${where}: conflicting definition of components.${kind}.${n}`);
        }
      }
    }
  }
  return { doc, errors, warnings };
}

/** Resolves a local JSON pointer ("#/a/b"). Returns undefined when it does not resolve. */
export function resolvePointer(doc, ref) {
  if (typeof ref !== 'string' || !ref.startsWith('#')) return undefined;
  if (ref === '#') return doc;
  if (!ref.startsWith('#/')) return undefined;
  let cur = doc;
  for (const raw of ref.slice(2).split('/')) {
    let key;
    try { key = decodeURIComponent(raw).replace(/~1/g, '/').replace(/~0/g, '~'); } catch { return undefined; }
    if (cur === null || typeof cur !== 'object' || !Object.hasOwn(cur, key)) return undefined;
    cur = cur[key];
  }
  return cur;
}

const deref = (doc, v) => (isObj(v) && typeof v.$ref === 'string' ? resolvePointer(doc, v.$ref) : v);

function walk(node, path, fn) {
  fn(node, path);
  if (Array.isArray(node)) node.forEach((v, i) => walk(v, `${path}/${i}`, fn));
  else if (isObj(node)) for (const [k, v] of Object.entries(node)) walk(v, `${path}/${k.replace(/~/g, '~0').replace(/\//g, '~1')}`, fn);
}

const SUB_SCHEMA = ['items', 'additionalProperties', 'propertyNames', 'not', 'if', 'then', 'else', 'contains', 'unevaluatedProperties', 'unevaluatedItems'];
const SUB_SCHEMA_LISTS = ['allOf', 'anyOf', 'oneOf', 'prefixItems'];
const SUB_SCHEMA_MAPS = ['properties', 'patternProperties', '$defs', 'dependentSchemas'];

/** Calls fn(schema, location) for a JSON Schema and every subschema (keyword-aware, so a property named "type" is not mistaken for the keyword). */
export function visitSchema(s, at, fn) {
  if (!isObj(s)) return;
  fn(s, at);
  for (const k of SUB_SCHEMA) if (isObj(s[k])) visitSchema(s[k], `${at}/${k}`, fn);
  for (const k of SUB_SCHEMA_LISTS) if (Array.isArray(s[k])) s[k].forEach((x, i) => visitSchema(x, `${at}/${k}/${i}`, fn));
  for (const k of SUB_SCHEMA_MAPS) if (isObj(s[k])) for (const [n, x] of Object.entries(s[k])) visitSchema(x, `${at}/${k}/${n}`, fn);
}

const typeOf = (v) => (v === null ? 'null' : Array.isArray(v) ? 'array' : Number.isInteger(v) ? 'integer' : typeof v);

/** Property names a schema documents, following $ref and allOf (used by the strict example lint). */
function documentedProps(doc, schema, depth = 0, out = new Set()) {
  if (!isObj(schema) || depth > 32) return out;
  if (typeof schema.$ref === 'string') documentedProps(doc, resolvePointer(doc, schema.$ref), depth + 1, out);
  if (isObj(schema.properties)) for (const k of Object.keys(schema.properties)) out.add(k);
  for (const s of schema.allOf || []) documentedProps(doc, s, depth + 1, out);
  return out;
}

/**
 * Validates a JSON value against the JSON Schema subset used by the Ludworks documents ($ref, allOf/anyOf/oneOf,
 * type, enum, const, properties/required/additionalProperties/propertyNames, items, string/number/array bounds,
 * pattern). Used to check that every example matches its schema. Returns a list of problems.
 * With `strict`, object properties that the schema does not document are reported even where the schema is open
 * (the server ignores unknown request fields, so typos in examples would otherwise go unnoticed).
 */
export function validateValue(doc, schema, value, at = '$', depth = 0, strict = false, shallow = false) {
  if (!isObj(schema) || depth > 64) return [];
  if (typeof schema.$ref === 'string') {
    const target = resolvePointer(doc, schema.$ref);
    const errs = target === undefined ? [] : validateValue(doc, target, value, at, depth + 1, strict, shallow);
    const { $ref, ...rest } = schema; // 3.1 allows siblings next to $ref
    return Object.keys(rest).length ? [...errs, ...validateValue(doc, rest, value, at, depth + 1, strict, true)] : errs;
  }
  const out = [];
  const t = typeOf(value);
  if (schema.type !== undefined) {
    const types = Array.isArray(schema.type) ? schema.type : [schema.type];
    if (!types.some((x) => x === t || (x === 'number' && t === 'integer'))) return [`${at}: expected ${types.join('|')}, got ${t}`];
  }
  if (Array.isArray(schema.enum) && !schema.enum.some((e) => deepEqual(e, value))) out.push(`${at}: ${JSON.stringify(value)} not in enum`);
  if (Object.hasOwn(schema, 'const') && !deepEqual(schema.const, value)) out.push(`${at}: must equal ${JSON.stringify(schema.const)}`);
  // allOf members see only part of the object, so the unknown-property lint runs once, here, on the union.
  for (const s of schema.allOf || []) out.push(...validateValue(doc, s, value, at, depth + 1, strict, true));
  if (Array.isArray(schema.anyOf) && !schema.anyOf.some((s) => validateValue(doc, s, value, at, depth + 1, strict).length === 0)) out.push(`${at}: matches none of anyOf`);
  if (Array.isArray(schema.oneOf) && schema.oneOf.filter((s) => validateValue(doc, s, value, at, depth + 1, strict).length === 0).length !== 1) out.push(`${at}: must match exactly one of oneOf`);
  if (t === 'string') {
    if (schema.minLength !== undefined && [...value].length < schema.minLength) out.push(`${at}: shorter than ${schema.minLength}`);
    if (schema.maxLength !== undefined && [...value].length > schema.maxLength) out.push(`${at}: longer than ${schema.maxLength}`);
    if (schema.pattern !== undefined && !new RegExp(schema.pattern, 'u').test(value)) out.push(`${at}: does not match ${schema.pattern}`);
  }
  if (t === 'integer' || t === 'number') {
    if (schema.minimum !== undefined && value < schema.minimum) out.push(`${at}: below minimum ${schema.minimum}`);
    if (schema.maximum !== undefined && value > schema.maximum) out.push(`${at}: above maximum ${schema.maximum}`);
  }
  if (t === 'array') {
    if (schema.minItems !== undefined && value.length < schema.minItems) out.push(`${at}: fewer than ${schema.minItems} items`);
    if (schema.maxItems !== undefined && value.length > schema.maxItems) out.push(`${at}: more than ${schema.maxItems} items`);
    if (schema.uniqueItems && value.some((v, i) => value.findIndex((w) => deepEqual(v, w)) !== i)) out.push(`${at}: items not unique`);
    if (isObj(schema.items)) value.forEach((v, i) => out.push(...validateValue(doc, schema.items, v, `${at}[${i}]`, depth + 1, strict)));
  }
  if (t === 'object') {
    const props = isObj(schema.properties) ? schema.properties : {};
    for (const r of schema.required || []) if (!Object.hasOwn(value, r)) out.push(`${at}: missing required "${r}"`);
    const documented = strict && !shallow && schema.additionalProperties === undefined ? documentedProps(doc, schema) : null;
    for (const [k, v] of Object.entries(value)) {
      if (Object.hasOwn(props, k)) out.push(...validateValue(doc, props[k], v, `${at}.${k}`, depth + 1, strict));
      else if (schema.additionalProperties === false) out.push(`${at}: unexpected property "${k}"`);
      else if (isObj(schema.additionalProperties)) out.push(...validateValue(doc, schema.additionalProperties, v, `${at}.${k}`, depth + 1, strict));
      else if (documented && documented.size && !documented.has(k)) out.push(`${at}: undocumented property "${k}"`);
      if (isObj(schema.propertyNames)) out.push(...validateValue(doc, schema.propertyNames, k, `${at}{${k}}`, depth + 1, strict));
    }
  }
  return out;
}

/** Template parameter names of an OpenAPI path ("/a/{id}/b/{x}.vtt" → ["id", "x"]). */
export const templateParams = (p) => [...p.matchAll(/\{([^{}]+)\}/g)].map((m) => m[1]);

/**
 * Validates a merged OpenAPI document against the Ludworks conventions.
 * @returns {{errors: string[], warnings: string[], stats: {paths: number, operations: number, schemas: number}}}
 */
export function validateDocument(doc) {
  const errors = []; const warnings = [];
  const E = (m) => errors.push(m);
  if (!isObj(doc)) return { errors: ['document is not an object'], warnings, stats: { paths: 0, operations: 0, schemas: 0 } };
  for (const k of Object.keys(doc)) if (!DOC_KEYS.has(k) && !k.startsWith('x-')) E(`unknown top-level key "${k}"`);
  if (typeof doc.openapi !== 'string' || !/^3\.1\.\d+$/.test(doc.openapi)) E('openapi must be "3.1.x"');
  if (!isObj(doc.info) || !doc.info.title || !doc.info.version) E('info.title and info.version are required');

  // $refs anywhere must be local and resolve; every schema (component or inline) is checked.
  const checkSchema = (s, at) => {
    if (Object.hasOwn(s, 'type')) {
      const types = Array.isArray(s.type) ? s.type : [s.type];
      if (types.some((t) => typeof t !== 'string' || !JSON_TYPES.has(t))) E(`${at}: invalid JSON Schema type ${JSON.stringify(s.type)}`);
    }
    // Typos in "required" lists are a common source of wrong client code.
    if (Array.isArray(s.required) && isObj(s.properties)) {
      for (const r of s.required) if (!Object.hasOwn(s.properties, r)) E(`${at}: required property "${r}" is not defined in properties`);
    }
    if (Object.hasOwn(s, 'nullable')) E(`${at}: "nullable" is OpenAPI 3.0; use type: [..., "null"] in 3.1`);
  };
  walk(doc, '#', (node, at) => {
    if (isObj(node) && Object.hasOwn(node, '$ref')) {
      if (typeof node.$ref !== 'string' || !node.$ref.startsWith('#/')) E(`${at}: only local "#/..." $refs are allowed (got ${JSON.stringify(node.$ref)})`);
      else if (resolvePointer(doc, node.$ref) === undefined) E(`${at}: unresolved $ref ${node.$ref}`);
    }
    if (at.endsWith('/schema') && isObj(node)) visitSchema(node, at, checkSchema);
    // Media-type examples must match the media type's schema.
    if (isObj(node) && isObj(node.schema) && /\/content\/[^/]+$/.test(at)) {
      const samples = [];
      if (Object.hasOwn(node, 'example')) samples.push(['example', node.example]);
      if (isObj(node.examples)) for (const [n, ex] of Object.entries(node.examples)) if (isObj(ex) && Object.hasOwn(ex, 'value')) samples.push([`examples/${n}`, ex.value]);
      for (const [n, v] of samples) for (const p of validateValue(doc, node.schema, v, '$', 0, true)) E(`${at}/${n}: example invalid: ${p}`);
    }
  });
  if (isObj(doc.components?.schemas)) {
    for (const [n, s] of Object.entries(doc.components.schemas)) visitSchema(s, `#/components/schemas/${n}`, checkSchema);
  }
  // Schema-level "examples" (JSON Schema 2020-12) must validate against the schema that carries them.
  walk(doc, '#', (node, at) => {
    if (isObj(node) && Array.isArray(node.examples) && (at.includes('/schemas/') || at.includes('/schema'))) {
      node.examples.forEach((v, i) => { for (const p of validateValue(doc, node, v, '$', 0, true)) E(`${at}/examples/${i}: example invalid: ${p}`); });
    }
  });

  const tagNames = new Set();
  if (!Array.isArray(doc.tags)) E('tags must be an array');
  else for (const t of doc.tags) {
    if (tagNames.has(t.name)) E(`tag "${t.name}" defined twice`);
    tagNames.add(t.name);
    if (!t.description) warnings.push(`tag "${t.name}" has no description`);
  }
  const schemes = isObj(doc.components?.securitySchemes) ? doc.components.securitySchemes : {};
  const checkSecurity = (sec, at) => {
    if (!Array.isArray(sec)) { E(`${at}: security must be an array`); return; }
    for (const req of sec) {
      if (!isObj(req)) { E(`${at}: security requirement must be an object`); continue; }
      for (const s of Object.keys(req)) if (!Object.hasOwn(schemes, s)) E(`${at}: unknown security scheme "${s}"`);
    }
  };
  if (doc.security !== undefined) checkSecurity(doc.security, 'security');

  const opIds = new Map();
  const shapes = new Map();
  const usedTags = new Set();
  let operations = 0;
  for (const [p, item] of Object.entries(doc.paths || {})) {
    if (!p.startsWith('/')) E(`path "${p}" must start with "/"`);
    const shape = p.replace(/\{[^{}]+\}/g, '{}');
    if (shapes.has(shape)) E(`paths "${shapes.get(shape)}" and "${p}" are equivalent templates (OpenAPI forbids this)`);
    shapes.set(shape, p);
    if (!isObj(item)) { E(`${p}: path item must be an object`); continue; }
    for (const k of Object.keys(item)) if (!PATH_ITEM_KEYS.has(k) && !k.startsWith('x-')) E(`${p}: unknown path-item key "${k}"`);
    const tmpl = templateParams(p);
    if (new Set(tmpl).size !== tmpl.length) E(`${p}: repeated template parameter`);
    const pathLevel = (item.parameters || []).map((x) => deref(doc, x)).filter(Boolean);
    for (const m of HTTP_METHODS) {
      const op = item[m];
      if (op === undefined) continue;
      operations++;
      const at = `${m.toUpperCase()} ${p}`;
      if (!isObj(op)) { E(`${at}: operation must be an object`); continue; }
      for (const k of Object.keys(op)) if (!OP_KEYS.has(k) && !k.startsWith('x-')) E(`${at}: unknown operation key "${k}"`);
      if (typeof op.operationId !== 'string' || !/^[a-z][A-Za-z0-9]*$/.test(op.operationId)) E(`${at}: operationId must be camelCase`);
      else if (opIds.has(op.operationId)) E(`${at}: operationId "${op.operationId}" already used by ${opIds.get(op.operationId)}`);
      else opIds.set(op.operationId, at);
      if (!Array.isArray(op.tags) || op.tags.length === 0) E(`${at}: needs at least one tag`);
      else for (const t of op.tags) { usedTags.add(t); if (!tagNames.has(t)) E(`${at}: tag "${t}" is not declared in top-level tags`); }
      if (typeof op.summary !== 'string' || !op.summary.trim()) E(`${at}: needs a summary`);
      if (typeof op.description !== 'string' || !op.description.trim()) E(`${at}: needs a description`);

      // Access contract: x-scope always; public <=> security: [].
      const scope = op['x-scope'];
      const isPublicSec = Array.isArray(op.security) && op.security.length === 0;
      if (scope === undefined) E(`${at}: missing x-scope`);
      else if (!X_SCOPE_VALUES.has(scope)) E(`${at}: invalid x-scope "${scope}"`);
      else {
        if (scope === 'public' && !isPublicSec) E(`${at}: x-scope public requires "security": []`);
        if (scope !== 'public' && isPublicSec) E(`${at}: "security": [] requires x-scope public`);
        const m2 = typeof op.description === 'string' && op.description.match(/\*\*Scope:\*\* `([^`]+)`/);
        if (!m2) E(`${at}: description must state "**Scope:** \`${scope}\`"`);
        else if (m2[1] !== scope) E(`${at}: description says scope ${m2[1]} but x-scope is ${scope}`);
      }
      if (op.security !== undefined) checkSecurity(op.security, at);
      const usesQueryToken = Array.isArray(op.security) && op.security.some((r) => isObj(r) && Object.hasOwn(r, 'accessTokenQuery'));
      if (usesQueryToken && m !== 'get') E(`${at}: query tokens are only allowed on GET`);
      if (usesQueryToken !== /access_token=/.test(op.description || '')) E(`${at}: description must mention \`?access_token=\` exactly when accessTokenQuery is allowed`);

      // Parameters.
      const params = (op.parameters || []).map((x) => ({ raw: x, p: deref(doc, x) }));
      const seen = new Set();
      for (const { raw, p: prm } of params) {
        if (!isObj(prm)) { E(`${at}: unresolvable parameter ${JSON.stringify(raw)}`); continue; }
        if (typeof prm.name !== 'string' || !prm.name) E(`${at}: parameter without name`);
        if (!PARAM_IN.has(prm.in)) E(`${at}: parameter ${prm.name} has invalid "in" ${JSON.stringify(prm.in)}`);
        if (!prm.schema && !prm.content) E(`${at}: parameter ${prm.name} needs a schema`);
        const key = `${prm.in}:${prm.in === 'header' ? String(prm.name).toLowerCase() : prm.name}`;
        if (seen.has(key)) E(`${at}: duplicate parameter ${key}`);
        seen.add(key);
        if (prm.in === 'path' && prm.required !== true) E(`${at}: path parameter ${prm.name} must be required`);
        if (prm.in === 'header' && ['accept', 'content-type', 'authorization'].includes(String(prm.name).toLowerCase())) E(`${at}: header parameter ${prm.name} is ignored by OpenAPI; describe it elsewhere`);
        if (prm.in === 'query' && prm.name === 'access_token') E(`${at}: do not declare access_token as a parameter; use the accessTokenQuery security scheme`);
      }
      const declared = new Set([...pathLevel, ...params.map((x) => x.p)].filter((x) => isObj(x) && x.in === 'path').map((x) => x.name));
      for (const t of tmpl) if (!declared.has(t)) E(`${at}: path parameter {${t}} is not declared`);
      for (const d of declared) if (!tmpl.includes(d)) E(`${at}: declared path parameter "${d}" is not in the path`);

      if (op.requestBody !== undefined) {
        const rb = deref(doc, op.requestBody);
        if (!isObj(rb) || !isObj(rb.content) || Object.keys(rb.content).length === 0) E(`${at}: requestBody needs content`);
        if (m === 'get' || m === 'head') E(`${at}: ${m.toUpperCase()} must not have a requestBody`);
      }
      if (!isObj(op.responses) || Object.keys(op.responses).length === 0) E(`${at}: needs responses`);
      else for (const [code, r] of Object.entries(op.responses)) {
        if (!/^([1-5][0-9][0-9]|[1-5]XX|default)$/.test(code)) E(`${at}: invalid response key "${code}"`);
        const rr = deref(doc, r);
        if (!isObj(rr) || typeof (r.description ?? rr.description) !== 'string') E(`${at}: response ${code} needs a description`);
        if (m === 'head' && isObj(rr) && rr.content) E(`${at}: HEAD response ${code} must not have content`);
      }
      if (isObj(op.responses) && !Object.keys(op.responses).some((c) => /^2/.test(c) || /^3/.test(c))) E(`${at}: needs a 2xx/3xx response`);
    }
  }
  for (const t of tagNames) if (!usedTags.has(t)) warnings.push(`tag "${t}" is not used by any operation`);

  // Unused schemas (warning only; base schemas are shared vocabulary).
  const schemas = isObj(doc.components?.schemas) ? doc.components.schemas : {};
  const refd = new Set();
  walk(doc, '#', (n) => { if (isObj(n) && typeof n.$ref === 'string' && n.$ref.startsWith('#/components/schemas/')) refd.add(n.$ref.slice(21)); });
  const stats = { paths: Object.keys(doc.paths || {}).length, operations, schemas: Object.keys(schemas).length };
  return { errors, warnings, stats, unusedSchemas: Object.keys(schemas).filter((s) => !refd.has(s)) };
}

/** All (method, path) pairs of a document as "METHOD /path" strings. */
export function operationKeys(doc) {
  const out = [];
  for (const [p, item] of Object.entries(doc.paths || {})) for (const m of HTTP_METHODS) if (isObj(item) && item[m]) out.push(`${m.toUpperCase()} ${p}`);
  return out.sort();
}

const METHOD_RE = '(?:GET|HEAD|POST|PUT|PATCH|DELETE|OPTIONS)';
const SPAN_METHODS_PATH = new RegExp(`^(${METHOD_RE}(?:/${METHOD_RE})*)(?:\\s+(/\\S*))?$`);

/** Normalises an API.md path: strips the query, `<x>` → `{x}`, adds the /api/v1 prefix. */
export function normalizeApiPath(raw) {
  let p = raw.split('?')[0].replace(/<([A-Za-z0-9_]+)>/g, '{$1}');
  if (!p.startsWith('/api/')) p = '/api/v1' + p;
  return p;
}

/**
 * Extracts the endpoints listed in API.md: leading code spans of table rows, plus bullets whose first
 * code span is "METHOD /api/...". Shorthands handled: "`GET/POST /x`", "`GET /x` · `PATCH`" (method
 * inherits the path) and "`GET /a/b.json`, `/c.json`" (sibling path inherits methods).
 * @returns {{endpoints: {key: string, line: number}[], errors: string[]}}
 */
export function parseApiMd(text) {
  const endpoints = []; const errors = [];
  let fence = false;
  const lines = text.split(/\r?\n/);
  lines.forEach((line, i) => {
    const ln = i + 1;
    if (/^\s*(```|~~~)/.test(line)) { fence = !fence; return; }
    if (fence) return;
    let rest;
    const trimmed = line.trim();
    if (trimmed.startsWith('|')) rest = trimmed.slice(1);
    else if (/^[*-]\s+`/.test(trimmed)) {
      rest = trimmed.replace(/^[*-]\s+/, '');
      const first = rest.match(/^`([^`]+)`/);
      if (!first || !new RegExp(`^${METHOD_RE}(?:/${METHOD_RE})*\\s+/api/`).test(first[1])) return;
      rest = first[0]; // bullets contribute only their first code span
    } else return;
    let prevMethods = null; let prevPath = null;
    for (;;) {
      const m = rest.match(/^\s*(?:[/·,]\s*)?`([^`]+)`/);
      if (!m) break;
      rest = rest.slice(m[0].length);
      const span = m[1].trim();
      const mp = span.match(SPAN_METHODS_PATH);
      let methods; let path;
      if (mp) {
        methods = mp[1].split('/');
        if (mp[2]) path = normalizeApiPath(mp[2]);
        else if (prevPath) path = prevPath;
        else { errors.push(`API.md:${ln}: method "${span}" without a path to inherit`); break; }
      } else if (/^\/\S*$/.test(span) && prevPath && prevMethods) {
        methods = prevMethods;
        path = prevPath.replace(/\/[^/]*$/, '') + normalizeApiPath(span).replace(/^\/api\/v1/, '');
      } else break; // not an endpoint span: end of the endpoint list in this cell
      for (const meth of methods) endpoints.push({ key: `${meth} ${path}`, line: ln });
      prevMethods = methods; prevPath = path;
    }
  });
  const seen = new Map();
  for (const e of endpoints) {
    if (seen.has(e.key)) errors.push(`API.md:${e.line}: ${e.key} already listed on line ${seen.get(e.key)}`);
    else seen.set(e.key, e.line);
  }
  return { endpoints, errors };
}

/** Compares API.md endpoints with the document; returns human-readable differences. */
export function compareWithApiMd(doc, apiMdText) {
  const { endpoints, errors } = parseApiMd(apiMdText);
  const want = new Map(endpoints.map((e) => [e.key, e.line]));
  const have = new Set(operationKeys(doc));
  const missing = [...want.keys()].filter((k) => !have.has(k)).sort();
  const extra = [...have].filter((k) => !want.has(k)).sort();
  return {
    errors: [
      ...errors,
      ...missing.map((k) => `missing in OpenAPI: ${k} (API.md line ${want.get(k)})`),
      ...extra.map((k) => `not listed in API.md: ${k}`),
    ],
    count: want.size,
  };
}

const mdCell = (s) => String(s ?? '').replace(/\|/g, '\\|').replace(/\s*\n\s*/g, ' ');

/** Python-Markdown toc slug of a heading text (mirrors markdown.extensions.toc.slugify). */
export function slugify(text) {
  return text.normalize('NFKD').replace(/[^\x00-\x7f]/g, '').replace(/[^\w\s-]/g, '').trim().toLowerCase().replace(/[-\s]+/g, '-');
}

/** Renders docs/api/reference.md: every operation grouped by its first tag, in tag order. */
export function generateReference(doc) {
  const byTag = new Map((doc.tags || []).map((t) => [t.name, []]));
  for (const [p, item] of Object.entries(doc.paths || {})) {
    for (const m of HTTP_METHODS) {
      const op = item[m];
      if (!op) continue;
      const t = op.tags?.[0] ?? 'Other';
      if (!byTag.has(t)) byTag.set(t, []);
      byTag.get(t).push({ m: m.toUpperCase(), p, op });
    }
  }
  const order = (x) => HTTP_METHODS.indexOf(x.toLowerCase());
  const out = [
    '# Endpoint reference',
    '',
    '<!-- Generated by tools/openapi-check/check.mjs --write-reference from the merged OpenAPI document. Do not edit by hand. -->',
    '',
    'Every endpoint of API v1, generated from the merged OpenAPI document (which the validator keeps identical to the',
    '[API contract](../spec/API.md)). A running server serves the full interactive reference with request/response',
    'schemas and examples at `/api/docs` (Swagger UI) and the raw document at `/api/openapi.json`.',
    '',
    'Scope legend: `public` = no authentication, `auth` = any signed-in user or token, `admin` = admin role (tokens also',
    'need the `admin` scope), anything else is the personal-access-token scope required. **qt** = the endpoint also accepts',
    '`?access_token=` (streaming and feeds).',
    '',
  ];
  const count = [...byTag.values()].reduce((n, v) => n + v.length, 0);
  out.push(`${count} operations in ${[...byTag.values()].filter((v) => v.length).length} groups.`, '');
  for (const [t, ops] of byTag) {
    if (!ops.length) continue;
    ops.sort((a, b) => (a.p < b.p ? -1 : a.p > b.p ? 1 : order(a.m) - order(b.m)));
    const tagObj = (doc.tags || []).find((x) => x.name === t);
    out.push(`## ${t}`, '');
    if (tagObj?.description) out.push(tagObj.description, '');
    out.push('| Method | Path | Scope | Summary |', '|---|---|---|---|');
    for (const { m, p, op } of ops) {
      const qt = Array.isArray(op.security) && op.security.some((r) => r && Object.hasOwn(r, 'accessTokenQuery'));
      out.push(`| ${m} | \`${p}\` | \`${mdCell(op['x-scope'])}\`${qt ? ' · qt' : ''} | ${mdCell(op.summary)} |`);
    }
    out.push('');
  }
  return out.join('\n');
}
