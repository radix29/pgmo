# CLAUDE.md

Context for Claude Code sessions on **pgmo**.

## What this is

Pure-Go (no CGO) library of PostgreSQL management objects — the PostgreSQL
counterpart of **gosmo** (`~/go/gosmo`, SQL Server's SMO in Go), over
`github.com/jackc/pgx/v5` used natively (not through `database/sql`). Go 1.27.
Module `github.com/radix29/pgmo`, MIT.

- **Status: P1 in progress** (module scaffold done: `go.mod`, `doc.go`,
  `version/`, the livedb harness `live_helpers_test.go`; the `pgsql/` text
  layer done; the error sentinels, refusal builders and SQLSTATE predicates in
  `errors.go`, `IsRetryable` in `retry.go`; `ServerVersion`,
  `MinimumServerVersion` and the gate functions in `version_gate.go`; the gate
  inventory in `version_gate_inventory_test.go`; the golden-file machinery
  in `version_gate_golden_test.go`; P2 started: `ConnectionOptions` in
  `connection.go`, its resolution and validation in `connection_dsn.go`;
  `Server`, `Connect`, `NewServer` in `server.go`; the `Querier` seam in
  `querier.go` and its fake in `pgmotest/`; per-database pools in
`database_pools.go`). The plan for pgmo *and* pgMS is
  `~/go/pgms/docs/PLAN.md` (`docs/PLAN.md` here points to it); pgmo owns
  phases P1–P6 and shares P14–P16.
- `ARCHITECTURE.md`: API map, feature map, errors, connection internals —
  filled as each family lands. Class diagrams go in `diagram/*.mmd`, as gosmo.
- Supported servers: **PostgreSQL 15 and later** (`MinimumServerVersion`).

## Upstream baseline

pgmo is modelled on gosmo at **`23435669672e118c75f485d207077fa87e1337c0`**
(2026-11-19 10:21:58 +01:00, `v0.3.0-10-g2343566`). Related baselines (the
full table is `~/go/pgms/docs/PLAN.md` § Upstream baseline): gossms
`ffa8e8b6afc7302fc8e82ca8596cf911392cfe96` (2026-11-20), tuikit
`54225788557f4bb439c0f8a5e72f4888e605d0f7` (2026-11-20).

To find gosmo fixes or conventions worth porting:

```bash
git -C ~/go/gosmo fetch -q
git -C ~/go/gosmo log --oneline --no-merges 23435669672e..origin/main
git -C ~/go/gosmo show --stat <sha>
```

Triage each commit as port / n/a / already; port with the gosmo sha in the
commit message; log the review in `~/go/pgms/docs/PLAN.md` § Upstream sync
log; then move the baseline here and in the plan. **gosmo is a read-only
reference** — never edit it from a pgmo session.

## This is a library, not pgMS's back end

**pgMS** (`~/go/pgms`) is the main consumer, developed alongside via an
active `replace github.com/radix29/pgmo => ../pgmo`. It is not the definition
of pgmo's API.

- **Never remove or narrow a capability because pgMS doesn't call it.**
- Optimisation preserves the API: same signature, results, errors.
- Adding capability for pgMS is encouraged — as a library feature, not a shim.
- pgmo is a SQL library: it does not shell out (`pg_dump` lives in pgMS,
  `PLAN.md` D12) and does not read the server's filesystem except through SQL
  functions the server offers.

## Build & verify

```
go build ./...  &&  go test ./...  &&  gofmt -w .  &&  go vet ./...
staticcheck ./...           # must stay clean; deadcode is not a gate (library surface)
go test -race -count=1 ./...  # a change touching concurrency (pools, retry, pgmotest); before a tag
go vet -tags livedb ./...   # after any rename, and before a tag
go test -tags livedb . -run '^(TestLiveA|TestLiveB)$' -livedb 'service=pg15'   # the tests a change touches
go test -tags livedb . -run '^(TestLiveA|TestLiveB)$' -livedb 'service=pg18'
go test -tags livedb . -short -livedb 'service=pg18'   # broad check: skips tests over 15 s
```

The lab clusters (pg15–pg18) and how to reach them are in
`~/go/pgms/docs/testing.md` — the source of truth; `-livedb` takes a libpq
**service name**, so no password appears anywhere. Verification rules:
`~/go/pgms/docs/verification.md`.

Rules carried over from gosmo (`~/go/gosmo/CLAUDE.md` § Build & verify):

- `go test ./...` uses fakes and needs no server. `pgmotest` is exported for
  callers' tests; a change to how pgmo sends a read keeps `pgmotest` in step.
- **Every write has an offline test of its exact statement** in the matching
  `script_*_write_test.go` table, mutation-checked.
- **A connection-option test asserts what pgx parses** (`pgconn.ParseConfig`),
  not only what pgmo writes.
- **A live test closes its connection with `t.Cleanup`, never `defer`,** when
  it drops objects in a `t.Cleanup`.
- **A live test creates its own throwaway database or schema** and drops it;
  never mutates pre-existing objects.
- **After a change to quoting or `pgsql.Spans`**, fuzz (`FuzzNames`,
  `FuzzSpans`) for 60 s with `-fuzzminimizetime 0`.
- Build and test **here** before relying on a change from pgMS.

## Conventions

gosmo's conventions (`~/go/gosmo/CLAUDE.md` § Conventions) apply unless this
section overrides them. In short:

- **One form, context first.** Every database-touching method takes `ctx`
  first; no `Foo`/`FooContext` pairs, no iterators.
- **Errors** wrap with `%w`, prefixed `pgmo: ` + the attempted operation,
  once. `invalidf` (`ErrInvalidRequest`), `unsupportedf` (`ErrUnsupported`),
  `notFoundf` (`ErrNotFound`) — never a bare `fmt.Errorf`/`errors.New`;
  `invalid_request_test.go` enforces it and the one-prefix rule. The
  server's error is `*pgconn.PgError`; classify by **SQLSTATE**, never by
  message text.
- **A write never goes through the retrying read path.** A write reading a
  value back (`RETURNING`) uses `execScan`.
- **Row iteration**: `defer rows.Close()`, checked `rows.Err()`, the same
  message wrapping query, scan and `Err`; `scanRows`/`readByName`/`foundRow`.
- **Never query inside a `rows.Next()` loop.** Fetch children in one query,
  group in Go.
- **Quoting**: `pgsql/` (stdlib only) is the authority — `QuoteIdent`,
  `QualifiedName`, `QuoteLiteral`. A name inside a literal needs both.
  Prefer a bound parameter (`$1`) wherever PostgreSQL accepts one.
- **Comment safety**: names written into `--` or `/* */` comments go through
  `pgsql.CommentSafe`/`BlockCommentSafe` (PostgreSQL block comments nest).
- **Version gates only through `version_gate.go`** (`atLeast`/`before`/
  `colSince`/`colUntil` on `server_version_num`). Never gate at or below 15.
  An extension's columns gate on its installed version (`extensionAtLeast`),
  not the server's. A new gate gets an inventory entry
  (`version_gate_inventory_test.go`) checked on the lab catalogs, and its read
  a `goldenQueries` entry with a regenerated `testdata/version_gates/*.sql`
  (`-updategolden`, then read the diff).
- **Script mode** (`WithScript`, `Scripting(ctx)`), `createdObject`,
  `setIfApplied`, `bindScriptArgs`, `execSecret` — as gosmo.
- **`CreateX(ctx, CreateXRequest) (*X, error)`; `XChanges` for partial alters;
  `XSpec` for a part; writes are methods on the handle.**
- **Catalog state is exported fields.** Only derivations and back-pointers
  are methods. Every type holding a parent exposes `Database()`/`Server()`.
- **The `Ref` suffix marks a lookup-free handle**; `XByName` reads the
  catalog. Not interchangeable.
- **Every read returning a type fills every field it declares.**

PostgreSQL-specific (new, not in gosmo):

- **Catalog reads use `pg_catalog`, schema-qualified**, never the session's
  `search_path`; `information_schema` only where nothing is lost.
- **Identity is the OID plus the qualified name; for functions, procedures
  and aggregates also the identity arguments.** A by-name lookup of an
  overloaded function returns `ErrAmbiguous`.
- **Names are matched exactly.** PostgreSQL folds only *unquoted* input;
  stored names are case-sensitive, so there is no case-insensitive fallback
  (gosmo's `matchName` fallback does not carry over).
- **No `USE`.** Each database is read through `Server.DatabaseQuerier(name)`
  (a pool per database, lazy, LRU-capped; never hold its pool directly). Never read another database's catalog from the
  current one — it is impossible, and `dblink` is not a substitute.
- **Statements that cannot run in a transaction** (`CREATE DATABASE`,
  `CREATE INDEX CONCURRENTLY`, `VACUUM`, `ALTER SYSTEM`, `REINDEX …
  CONCURRENTLY`) are refused under `InTransaction` with `ErrInvalidRequest`
  before anything is sent.
- **Passwords are sent pre-hashed** (SCRAM verifier computed client-side), so
  the cleartext never reaches the server log.

## Open threads

`OPEN-THREADS.md` holds work knowingly left undone and what to watch.

## Release

`RELEASE.md` is the current release, `CHANGELOG.md` the history; don't edit
either in a feature or fix unless asked. pgMS cannot release while pgmo's
`HEAD` is untagged — tag and push here first.

## Repo hygiene

- **Never commit, amend, push or tag — the user does.**
- Check a path exists before writing to it.
- Never write credentials into any file.
