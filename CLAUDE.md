# CLAUDE.md

Context for Claude Code sessions on **pgmo**.

## What this is

Pure-Go (no CGO) library of PostgreSQL management objects — the PostgreSQL
counterpart of **gosmo** (`~/go/gosmo`, SQL Server's SMO in Go), over
`github.com/jackc/pgx/v5` used natively (not through `database/sql`). Go 1.27.
Module `github.com/radix29/pgmo`, MIT.

- **Status: P4 in progress** (P1–P3 done; P3: `Server.Info` /
  `ServerInfo` in `server_info.go`; the `information_schema` guard in
  `catalog_source_test.go`; identity in `identity.go`; the row-iteration
  helpers in `rows.go`. Module scaffold done: `go.mod`, `doc.go`,
  `version/`, the livedb harness `live_helpers_test.go`; the `pgsql/` text
  layer done; the error sentinels, refusal builders and SQLSTATE predicates in
  `errors.go`, `IsRetryable` in `retry.go`; `ServerVersion`,
  `MinimumServerVersion` and the gate functions in `version_gate.go`; the gate
  inventory in `version_gate_inventory_test.go`; the golden-file machinery
  in `version_gate_golden_test.go`; P2 started: `ConnectionOptions` in
  `connection.go`, its resolution and validation in `connection_dsn.go`;
  `Server`, `Connect`, `NewServer` in `server.go`; the `Querier` seam in
  `querier.go` and its fake in `pgmotest/`; per-database pools in
`database_pools.go`; the `search_path` guard in `search_path_test.go`;
  `withRetry` in `retry.go` and the read/write helpers `query`, `queryRow`,
  `queryRowScan`, `execScan` in `statement.go`; per-session notices,
  `Server.OpenSession`, in `session.go`; cancel requests,
  `UseCancelRequests`, in `cancel.go`; `InTransaction` and the context
  routing `on`/`onDatabase`/`refuseInTx` in `transaction.go`;
  `WithScript`/`Scripting`, the write chokepoints `exec`/`execIn` and
  `bindScriptArgs` in `script.go`; `execSecret`, `WithScriptSecrets`,
  `PasswordPlaceholder` and client-side SCRAM verifiers in `secret.go` over
  `internal/scram`; `Server.Capabilities` and `Server.DatabasePrivileges`
  in `capabilities.go`; `OID`, `RoutineIdentity` and `routineByName` in
  `identity.go`; `scanRows`, `readByName`, `foundRow`/`rowErr` in
  `rows.go`; the exact `matchName` in `names.go` and `pgsql.FoldIdent`;
  `ObjectFilter` and its `clause` in `object_filter.go`; `Origin`
  (`IsSystem`, `Extension`) and `IsSystemSchema` in `system_objects.go`;
  the version sweep `TestLiveVersionSweep` in `live_versionsweep_test.go`
  and its guards in `versionsweep_reflect_test.go`; P4 started: the database
  reads `Databases`, `DatabaseByName`, `DatabaseRef`, `Database.Size` in
  `database.go`; the role reads `Roles`, `RoleByName`, `RoleRef` in
  `role.go`; the schema reads `Schemas`, `SchemaByName`, `SchemaRef` in
  `schema.go`, ACLs and `Database.DefaultPrivileges` in `privileges.go`).
  The plan for pgmo *and* pgMS is
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
go test -tags livedb . -run '^TestLiveVersionSweep$' -v -livedb 'service=pg15'   # after any query change
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
  `notFoundf` (`ErrNotFound`), `ambiguousf` (`ErrAmbiguous`) — never a bare `fmt.Errorf`/`errors.New`;
  `invalid_request_test.go` enforces it and the one-prefix rule. The
  server's error is `*pgconn.PgError`; classify by **SQLSTATE**, never by
  message text.
- **A write never goes through the retrying read path** (`query`,
  `queryRow`, `queryRowScan`; `write_through_read_test.go` reads their SQL).
  A write reading a value back (`RETURNING`) uses `execScan`. A read's scan
  callback runs again on a retry: no queries, no side effects.
- **A statement goes through `s.on(ctx)` / `s.onDatabase(ctx, name)`**,
  never `s.q` or `DatabaseQuerier` directly, so it joins the transaction
  `ctx` carries (`ARCHITECTURE.md` § Transactions). A statement PostgreSQL
  refuses in a transaction block calls `refuseInTx` first.
- **Row iteration** (`rows.go`): a list read is `scanRows`; a by-name read
  whose row the family's `scanX(owner, scan)` builds is `readByName`; another
  single-row read where no row means absent ends in `foundRow`/`rowErr`
  (`pgx.ErrNoRows` → `notFoundf`). The query's, a scan's and `rows.Err`'s
  failures share one wrap there. No `Query`/`QueryRow`/`Next()` loop/direct
  `query` outside `statement.go`/`rows.go` (`rows_test.go` enforces it).
- **Every read is in the version sweep** (`ARCHITECTURE.md` § Version
  sweep): a function sending a read gets a `sweepReaders` entry and a
  `sweepMustCall` label the live sweep calls (`TestSweepCoversEveryRead`); a
  new family type goes in `sweptTypes`; a context-only write in `sweepSkip`.
- **Never query inside a `rows.Next()` loop** — nor in a scan handed to a
  read helper (`TestNoQueryInsideARowScan`). Fetch children in one query,
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
- **A write goes through `s.exec` / `s.execIn`** (`script.go`), never
  `Querier.Exec` directly, so `WithScript` captures it; args bind `$N`
  and are scripted by `bindScriptArgs`. `createdObject`, `setIfApplied`,
  `execSecret` — as gosmo (`ARCHITECTURE.md` § Script mode).
- **`CreateX(ctx, CreateXRequest) (*X, error)`; `XChanges` for partial alters;
  `XSpec` for a part; writes are methods on the handle.**
- **Catalog state is exported fields.** Only derivations and back-pointers
  are methods. Every type holding a parent exposes `Database()`/`Server()`.
- **The `Ref` suffix marks a lookup-free handle**; `XByName` reads the
  catalog. Not interchangeable.
- **Every read returning a type fills every field it declares.**

PostgreSQL-specific (new, not in gosmo):

- **Catalog reads use `pg_catalog`, schema-qualified**, never the session's
  `search_path`: relations, functions (`pg_catalog.count(*)`), cast and
  typed-literal types (`::pg_catalog.text`; grammar keyword types such as
  `integer`, `bigint`, `boolean` stay bare). `TestCatalogReadsAreSchemaQualified`
  reads every SQL literal.
- **`pg_catalog` is the source of truth**, never `information_schema` (it
  hides what the role cannot use, has no OIDs, omits non-standard objects).
  A view is read only through `portableViews` with why nothing is lost;
  `TestCatalogReadsUsePgCatalog` enforces it (`ARCHITECTURE.md` § Source of
  truth).
- **Identity is the OID plus the qualified name; for functions, procedures
  and aggregates also the argument types** (`RoutineIdentity`,
  `identity.go`). A by-name lookup of an overloaded function returns
  `ErrAmbiguous`. DDL names a routine by `Signature()`, never by
  `pg_get_function_identity_arguments` or `regprocedure` text — both follow
  the session's `search_path` (`ARCHITECTURE.md` § Identity).
- **Names are matched exactly.** PostgreSQL folds only *unquoted* input;
  stored names are case-sensitive, so there is no case-insensitive fallback
  (gosmo's `matchName` fallback does not carry over): SQL compares with `=`,
  Go with `matchName`. Typed input is folded first, the server's way, with
  `pgsql.FoldIdent` (ASCII only). No `EqualFold`/`ToLower`/`lower()`/`ILIKE`
  on a name — `TestNoCaseFoldingOfNames`; an exception is a
  `caseFoldingAllowed` entry with why the text is not a name
  (`ARCHITECTURE.md` § Name matching).
- **A listing takes an `ObjectFilter` and pushes it into its query**
  (`clause`), never filtering rows in Go; its text is folded with
  `FoldIdent` and matched exactly, by `strpos`/`=`, never `LIKE`
  (`ARCHITECTURE.md` § Object filters).
- **System and extension membership are two flags** (`Origin`,
  `system_objects.go`): `IsSystem` is the schema's (`pg_*` or
  `information_schema`, `systemSchema`/`IsSystemSchema`); `Extension` is
  `pg_depend` deptype `'e'` only. An extension member is not a system object
  (`ARCHITECTURE.md` § System objects).
- **No `USE`.** Each database is read through `Server.DatabaseQuerier(name)`
  (a pool per database, lazy, LRU-capped; never hold its pool directly). Never read another database's catalog from the
  current one — it is impossible, and `dblink` is not a substitute.
- **Statements that cannot run in a transaction** (`CREATE DATABASE`,
  `CREATE INDEX CONCURRENTLY`, `VACUUM`, `ALTER SYSTEM`, `REINDEX …
  CONCURRENTLY`) are refused under `InTransaction` with `ErrInvalidRequest`
  before anything is sent.
- **Passwords are sent pre-hashed** (SCRAM verifier computed client-side,
  `passwordClause`), so the cleartext never reaches the server log; every
  write carrying a secret goes through `execSecret` and has a `secretWrites`
  entry (`secret_redaction_test.go`).

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
