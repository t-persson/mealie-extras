# Working in mealie-extras

## Scope and parity

Keep edits inside this repository. Leave publishing and pushing to the human.
Preserve the Python programs as the reference implementation.

When changing `mealie-generate`, read `python/mealie_extras/generate.py` and
the Mealie meal-plan client under `python/mealie_extras/api/mealie/`.
When changing `mealie-shoppinglist`, read `python/mealie_extras/shoppinglist.py`
and its shopping-list and recipe clients in that directory.
For HTTP behavior, consult `python/mealie_extras/api/{session,json,paging}.py`
and the ntfy client under `python/mealie_extras/api/ntfy/`.

Preserve flag names and defaults, `API_KEY` fallback, next-Monday date selection,
inclusive shopping ranges, duplicate-recipe deletion and retry, raw ingredient
fields, household filtering, and notification contents. Zero days remains a
supported input. Report intentional departures from Python behavior.

`mealie-pantry` (web companion tracking fridge/freezer/pantry stock) has no
Python counterpart. Its code lives in `internal/pantry/{app,store,sync,web}`. It
must stay read-only towards Mealie; its own state is SQLite
(`modernc.org/sqlite`, the one non-stdlib dependency). User-facing behavior is
documented in `README.md`.

## Go implementation

Use separate entry points under `cmd/mealie-generate` and
`cmd/mealie-shoppinglist`. Keep HTTP integration in `internal/api`, shared CLI
behavior in `internal/cli`, and command workflows in their own internal packages.
Use standard-library dependencies unless a concrete requirement warrants more.

Keep HTTP clients and workflow services injectable so tests use local fake
servers instead of live Mealie or ntfy instances. Propagate failures with
operation context; command failures must exit nonzero.

## Completion

Format Go changes with `gofmt`. Run `go test ./...` and `go vet ./...`, then
build both command packages as separate binaries. For behavioral changes, cover
the affected API payload or command flow with deterministic tests. Keep generated
binaries out of version control.

## Compatibility decisions

The Go client intentionally repairs Python's nested pagination parameters and
accepts an empty HTTP 204 when deleting a duplicate. It rejects cross-origin
redirects and pagination cycles. JSON decoding is typed rather than Pydantic's
coercion, and CLI errors are concise nonzero exits rather than Python tracebacks.
Command flags, successful workflows, and notification payloads retain Python's
behavior; Python-specific help formatting and dependency debug logs are not
part of that contract.
