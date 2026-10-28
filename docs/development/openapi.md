# OpenAPI fragments

The OpenAPI 3.1 document served at `/api/openapi.json` (and rendered by Swagger UI at `/api/docs`) is assembled at
start-up from a **base document** plus one **fragment per module**, so each module documents its own endpoints
next to its code.

| File | Contents |
|---|---|
| `internal/apidocs/base.json` | `openapi`, `info`, `servers`, global `security`, security schemes, every tag, shared schemas (`Error`, `ErrorResponse`, `ValidationError`, `Page`, `NextCursor`, `Id`, `Timestamp`, `User`, `Space`, `Entry`, `UpdatedCount`, `Scope`, `Role`, `HealthStatus`), shared parameters (`Limit`, `Cursor`, `Range`, `IfRange`, `IfNoneMatch`), headers and standard error responses; plus `/api/v1/health`, `/api/openapi.json`, `/api/docs` |
| `internal/auth/openapi.json` | `/api/v1/setup*`, `/api/v1/auth/*`, `/api/v1/me*` |
| `internal/admin/openapi.json` | `/api/v1/admin/users*`, `/api/v1/admin/settings*`, `/api/v1/admin/audit`, `/api/v1/admin/db-backup` |
| `internal/mail/openapi.json` | the *Mail* tag only (the SMTP/IMAP tests live in the admin fragment) |
| `internal/files/openapi.json` | `/api/v1/spaces`, `/api/v1/files*`, `/api/v1/trash*`, `/api/v1/uploads*` (tus), `/api/v1/categories`, `/api/v1/admin/categories`, `/api/v1/admin/spaces*`, `/api/v1/backup/devices*` |
| `internal/media/openapi.json` | `/api/v1/media*` |
| `internal/tv/openapi.json` | `/api/v1/tv/*`, `/api/v1/hdhr/*` |
| `internal/music/openapi.json` | `/api/v1/music/*` |
| `internal/notes/openapi.json` | `/api/v1/notes*` |
| `internal/vault/openapi.json` | `/api/v1/vault*` |
| `internal/sheets/openapi.json` | `/api/v1/sheets*` |
| `internal/kanban/openapi.json` | `/api/v1/kanban/*` |
| `internal/cast/openapi.json` | `/api/v1/cast/*` (including the public ticket route `/api/v1/cast/media/{ticket}/…`) |
| `internal/system/openapi.json` | `/api/v1/system/*` |

Each module embeds its fragment (`//go:embed openapi.json`) and returns it from `OpenAPI()`.

## Fragment format

```json
{
  "tags": [ { "name": "Notes", "description": "…" } ],
  "paths": { "/api/v1/notes": { "get": { … } } },
  "components": { "schemas": { "NotesNote": { … } } }
}
```

Rules every fragment follows:

* Only the keys `tags`, `paths` and `components` (and inside `components` no `securitySchemes`).
* Paths are **full** paths starting with `/api/v1/` (or `/api/`).
* Every operation has a unique camelCase `operationId`, at least one declared tag, a `summary`, a `description`,
  parameters for every `{template}` in the path, and `responses` including at least one 2xx/3xx.
* Every operation has an **`x-scope`**: `public`, `auth`, `admin`, or a token scope such as `notes:write`. The
  description ends with the same value as `**Scope:** \`notes:write\``, because Swagger UI does not display
  extensions.
* Public operations set `"security": []`. Streaming/feed operations that accept `?access_token=` set
  `"security": [{"bearerAuth": []}, {"cookieAuth": []}, {"accessTokenQuery": []}]` and mention
  `` `?access_token=` `` in the description. Everything else inherits the global security
  (`bearerAuth`, or `cookieAuth` + `csrfHeader`).
* Module-specific schema names carry the module prefix (`NotesNote`, `TvChannel`, `VaultItem`, `SheetsCell`, …)
  so they cannot collide. Cross-module shared schemas live only in `base.json`. References are local
  (`#/components/schemas/…`) and resolve after merging.
* Raw bodies (uploads, posters, imports) are described with a binary schema under their media types; tus PATCH
  uses `application/offset+octet-stream`.

## Merge rules

The server (`internal/apidocs.Build`) and the checker merge identically:

1. Start from `base.json`; set `info.version` to the running server's version.
2. Apply the module fragments. A fragment may contain `tags`, `paths` and `components`; `openapi`, `info`,
   `servers`, `security` and `x-*` keys are ignored (the checker warns), anything else is an error.
3. **Tags** are appended by name; the first definition wins (the checker warns when a repeat differs, so keep
   fragment tags identical to `base.json`).
4. **Paths**: a (path, method) pair may be defined only once, including against `base.json` — a duplicate is an
   error. Other path-item keys (e.g. `parameters`) must be identical if repeated.
5. **Components**: each `components.<kind>.<name>` may be defined once; an identical repeat is tolerated, a
   different one is an error. (Policy, checked by the checker only: `securitySchemes` live in `base.json`.)

A merge error makes the server refuse to build the document, and `go test ./internal/apidocs/` merges the
checked-in fragments, so conflicts fail CI on both sides.

## Validating

```bash
node tools/openapi-check/check.mjs                    # merge + validate + compare with API.md
node tools/openapi-check/check.mjs --write-reference  # regenerate docs/api/reference.md
node tools/openapi-check/check.mjs --out merged.json  # also write the merged document
node --test tools/openapi-check/*.test.mjs            # unit tests of the tooling
```

`check.mjs` (Node ≥ 20, no dependencies) reports:

* JSON syntax errors and unknown top-level keys,
* merge conflicts (duplicate operations, conflicting schemas or tags),
* unresolved or non-local `$ref`s, invalid schema `type`s, `required` properties that are not defined,
* duplicate or non-camelCase `operationId`s, undeclared tags,
* undeclared or extra path parameters, non-required path parameters, equivalent path templates,
* missing `x-scope`, mismatched `**Scope:**` lines, `security: []` without `x-scope: public` (and vice versa),
  query tokens on non-GET operations,
* missing responses or descriptions, request bodies on GET/HEAD,
* request/response `example`s and schema `examples` that do not validate against their schema — strictly:
  properties the schema does not document are reported too, because the server silently ignores unknown
  request fields (schemas themselves stay open for forward compatibility),
* and — most importantly — any difference between the set of `(method, path)` pairs in the merged document and
  the endpoint tables in [API.md](../spec/API.md) (missing or extra endpoints), and a stale endpoint reference
  page.

It exits non-zero on any error, so CI can run it on every change.
