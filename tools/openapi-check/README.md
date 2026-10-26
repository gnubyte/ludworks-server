# openapi-check

Dependency-free Node (≥ 20) tooling for the API description and the documentation site.

| Command | Purpose |
|---|---|
| `node tools/openapi-check/check.mjs` | Merge `internal/apidocs/base.json` + every `internal/<module>/openapi.json` exactly like the server, validate the result, compare its `(method, path)` set with `docs/spec/API.md`, and verify `docs/api/reference.md` is current |
| `node tools/openapi-check/check.mjs --write-reference` | Regenerate `docs/api/reference.md` |
| `node tools/openapi-check/check.mjs --out merged.json` | Also write the merged OpenAPI document |
| `node tools/openapi-check/docs-check.mjs` | Validate `mkdocs.yml` nav, every relative link and `#anchor` in `docs/`, `.readthedocs.yaml` and `docs/requirements.txt` without installing MkDocs |
| `node --test tools/openapi-check/*.test.mjs` | Unit tests (including both checks against the real repository) |

Both CLIs accept `--root <ludworks-server dir>` and exit non-zero on errors. Fragment conventions and merge rules
are documented in `docs/development/openapi.md`.
