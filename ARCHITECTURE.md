# pgmo architecture

The API map, feature map, errors and connection internals of pgmo, modelled on
gosmo's `ARCHITECTURE.md` (`~/go/gosmo`, at the baseline in `CLAUDE.md`).

**Status: planning.** Sections below state the target design from
`~/go/pgms/docs/PLAN.md` (P1–P6). Each is rewritten as its code lands; until
then, the plan is authoritative.

## Version gating

Floor: **PostgreSQL 15** (`MinimumServerVersion = 150000`). Columns, views and
syntax added after the floor are gated through `version_gate.go` and pinned
three ways, as gosmo: an offline test per gate, a `testdata/version_gates/*.sql`
file with the exact catalog text, and the live sweep on pg15.

`version_gate.go` (landed P1.4.1): `ServerVersion` is a `server_version_num`
(major×10000 + minor; `PostgreSQL15`…`PostgreSQL18`, `Major`/`Minor`/`String`).
`atLeast`/`before` are the only comparisons, with 0 (never read) counting as
newest; `colSince` substitutes a typed expression (`NULL::bigint`, `false`) so
the scan list never changes; `hasColumnSince` is for a term that must be
dropped rather than substituted. The `Server`/`Database` method forms arrive
with those types (P2).

`colUntil` is colSince's mirror for a removed column (P1.4.2); a rename is a
`colSince` whose zero is the old name. `extensionAtLeast` gates an extension's
columns on its installed `extversion`, which survives `pg_upgrade` unchanged,
so `pg_stat_statements` 1.10 can sit on a 17 server.

The inventory (landed P1.4.2) is `version_gate_inventory_test.go`, written by
hand from the lab catalogs, never from call sites: `catalogGates` (relation or
column, `since`, `until`), `statementGates` (a probe and the SQLSTATE an older
server raises), `extensionGates` (`pg_stat_statements` by extension version)
and `stableColumns`. `live_version_gates_test.go` asks the connected server
about every entry, both directions; it passes on pg15–pg18. The headlines:

| Since | What |
|---|---|
| 16 | `pg_stat_io`; `pg_auth_members.inherit_option`/`set_option`/`oid` and `GRANT … WITH INHERIT/SET`; `EXPLAIN (GENERIC_PLAN)`; `pg_create_subscription`, `pg_use_reserved_connections`; `daticurules`/`collicurules`; `pg_stat_all_tables.last_seq_scan`… |
| 17 | `pg_stat_checkpointer`, and `pg_stat_bgwriter` loses its checkpoint and backend counters; `daticulocale`→`datlocale`, `colliculocale`→`colllocale`; builtin collation provider; `MERGE … RETURNING`; `EXPLAIN (SERIALIZE, MEMORY)`; `pg_maintain`; replication-slot failover columns |
| 18 | virtual generated columns; `pg_stat_io.op_bytes` → `read_bytes`/`write_bytes`/`extend_bytes`; `pg_stat_wal` loses its write/sync counters; `conenforced`/`conperiod`; `relallfrozen`; `pg_signal_autovacuum_worker` |
| ext | `pg_stat_statements` 1.11 renames `blk_*_time` → `shared_blk_*_time` and adds `local_blk_*_time`, `jit_deform_*`, `stats_since`; 1.12 adds `wal_buffers_full`, `parallel_workers_*`; `jit_*` is 1.10, `*_exec_time` 1.8 |

Ungated, though the plan listed them: `pg_stat_progress_copy` (14) and
`pg_database.datlocprovider` (15) are at or below the floor;
`pg_stat_activity` and `pg_stat_replication` have identical columns on 15–18
(held by `stableColumns`) — only their values drift (`backend_type` gains
`slotsync worker` in 17, `io worker` in 18).

The guards (landed P1.4.3) read the source, so they cover gates no test
reaches. `TestVersionGatesGoThroughAtLeast` (`version_gate_test.go`) refuses
a version compared outside `version_gate.go` — an ordering, `== 0`, equality
with a release constant, an extension version compared as text — and a gate
on `PostgreSQL15` or below or on a non-constant version.
`TestEveryVersionGateIsInTheInventory` holds each call site to the inventory:
a `colSince` column to an entry with that `since`, a rename's substitute to one
with that `until`, a `colUntil` column to its `until`, an `atLeast`/`before`/
`hasColumnSince` release to something that changes in it, an
`extensionAtLeast` version to an extension gate. `version_gate_golden_test.go`
renders each builder in `goldenQueries` at 15–18 into
`testdata/version_gates/<name>.sql` (`go test -run
TestGatedQueriesMatchTheirGoldenFiles -updategolden`, then read the diff),
checks every rendering against `catalogGates`, the select-list arity across
releases, and that every gate call site has a golden query. A new gated read
adds its builder to `goldenQueries` in the same change. Each guard has a
fixture self-test, since pgmo has no gated reads yet.

## Architecture

```
github.com/radix29/pgmo
├── (root)      package pgmo — Server, Database, every object family, Scripter, monitoring
├── pgsql/      stdlib-only text layer: QuoteIdent, QualifiedName, QuoteLiteral, Spans (lexer), CommentSafe
├── pgmotest/   test kit for callers: a scripted fake of pgmo's Querier
├── version/    pgmo's version metadata
├── diagram/    Mermaid class diagrams (00-map.mmd + one per group)
├── examples/   runnable programs (see examples/README.md)
└── testdata/   version_gates/, configurations/, replication/ fixtures
```

### Connecting and the `Server` object

`Connect(ctx, ConnectionOptions, ...ServerOption)` builds a `pgxpool.Config` (pgx's
`ParseConfig`, which already honours libpq service files, `.pgpass` and `PG*`
variables), validates pgmo's fields first, and opens the pool for the
requested database. `NewServer(ctx, Querier, ...ServerOption)` wraps an existing pool or other
`Querier` (below); `Server.Pool()` returns a pool. The `ServerOption`s are the
per-database limits (below).

`Server` (`server.go`, landed P2.2.1) holds the pool, `Version()` (the
`server_version_num` read once, through `pg_catalog.current_setting`, by the
one constructor `newServer`) and a lifetime context (`Context()`, cancelled by
`Close`), as gosmo's. A server below `MinimumServerVersion` is refused with
`ErrUnsupportedVersion` (`checkServerVersion`, which lives in
`version_gate.go`: it is the one comparison against the floor, which the gate
guard refuses elsewhere). `Connect` validates the options before dialling
(`pgxpool.NewWithConfig` dials nothing; the version read makes the first
connection) and closes its pool on any failure; `NewServer` leaves the
caller's pool open on failure and takes ownership on success. Unlike gosmo,
`Close()` returns nothing, as `pgxpool.Pool.Close` does; it is idempotent.
Errors: `pgmo: connect: …` / `pgmo: new server: …` (options refusals keep
`pgmo: connection options: …` alone). gosmo's `bound` (every statement also
cancelled by `Close`) arrives with pgmo's first read path (P2.2.2/P2.2.5).
`server_test.go` drives `Connect`/`NewServer` offline against a pgproto3 fake
backend (simple protocol), so the refusal of an old server and the pool being
closed (its session `Terminate`d) are tested without a pre-15 cluster;
`TestLiveServerConnect`/`TestLiveNewServer` pass on pg15 and pg18.

`ConnectionOptions` (`connection.go`, landed P2.1.1) is one field per libpq
parameter — `Host` (comma list), `Port`, `Database`, `User`, `Password`,
`Service`, `SSLMode`, `SSLRootCert`, `SSLCert`/`SSLKey`, `ApplicationName`,
`ConnectTimeout`, `TargetSessionAttrs` — plus `Params` (runtime parameters;
a key a field controls is refused) and `ConnString` (keyword/value or URI).
An empty field defers along libpq's order: field → `ConnString` → service
file → `PG*` → `.pgpass`. `SSLMode` and `TargetSessionAttrs` are typed
strings whose constants are tested against what `pgconn.ParseConfig` accepts.

Resolution (`connection_dsn.go`, landed P2.1.2): `poolConfig` validates the
fields first — each refusal is `ErrInvalidRequest` naming the field
(`ConnectionOptions.Port 70000 is outside 1-65535`) — then writes every set
field but `Password` *after* `ConnString`, in its own form (quoted
keyword/value, or percent-encoded URI query parameters), and hands the result
to `pgxpool.ParseConfig`. pgx keeps the last value of a repeated keyword, and a
URI query parameter over the URI's authority, so the field wins; pgx then
applies service file → `PG*` → `.pgpass` beneath. `Password` is set on the
parsed config instead (it outranks every source anyway), so it is never in a
string pgx could quote. A parse failure keeps pgx's redacted text and cause
(`fs.ErrNotExist` for a missing CA file) but not the `*pgconn.ParseConfigError`,
whose `ConnString` field is unredacted. `application_name` defaults to `pgmo`
when nothing names one.

`Params` takes setting names only (`[A-Za-z0-9_.]`), case-insensitively
refusing a field's keyword, a key given twice in different case, and
`connectionKeywords` — what pgx/pgxpool read themselves (`passfile`,
`pool_max_conns`, …) plus `options`, `replication` and URI `ssl`; those go in
`ConnString`. `TestConnectionKeywordsAreNotRuntimeParams` holds that list to
pgx.

Where pgx differs from libpq (accepted, not worked around): no system-wide
service file (`PGSYSCONFDIR`); one `.pgpass` lookup, for the first host, in a
multi-host connection; `sslmode` is not checked when the host is a Unix
socket; `servicefile` is accepted in the connection string; `sslrootcert` is
read while parsing in every `sslmode`, so a missing file fails even under
`disable`/`allow`/`prefer`/`require`, where libpq ignores it (checked with
psql 2026-11-21; pinned by `TestMissingRootCertFailsInEveryMode`).

The DSN tests assert what pgx parses, not the text pgmo writes (P2.1.3,
gosmo's lesson): `TestEveryFieldReachesPgx` sets each field over no
`ConnString` and over both forms naming another value — the file fields over
`ConnString` names of missing files — and must name every field, so a new
one not written into the string fails; `TestTLSReachesPgx` holds each
`sslmode`, with and without a root file, to its dial plan (primary and
fallback `tls.Config`s, `RootCAs`); `TestHostsReachPgx` covers host lists,
IPv6 literals, socket directories and `Port` across hosts.

### Per-database pools

Landed P2.2.3 (`database_pools.go`). PostgreSQL binds a session to one
database, so this replaces gosmo's `USE [db]` and `perDatabaseExec`.
`newServer` reads `current_database()` with the version (`versionQuery`, one
round trip; `CurrentDatabase()`). `DatabaseQuerier(name)` is lookup-free: the
Server's own `Querier` for `CurrentDatabase`, else a handle that **leases**
that database's `Querier` from `Server.pools` for each statement — opening it
on the first, reopening it after an eviction — so the handle stays valid until
`Close`. Later `Database` handles (P3) read through it.

How another database is opened, in order: `WithDatabaseOpener`; else, after
`Connect`, its `ConnectionOptions` resolved again with `Database` replaced (so
a `.pgpass` line for that database applies, as in libpq); else, over a
`*pgxpool.Pool`, `pool.Config()` with the database replaced (hooks kept: a
`BeforeConnect` IAM token works for every database); else `ErrUnsupported`
(a single connection or a transaction cannot be cloned). A failed open is not
cached; a missing database fails the statement with the server's `3D000`.

Limits (settled 2026-11-21):

| Limit | Default | Option | Why |
|---|---|---|---|
| other databases open at once | 8 (`DefaultMaxDatabasePools`) | `WithMaxDatabasePools(n ≥ 1)` | an Object Explorer rarely has more expanded; reopening is cheap |
| connections per derived pool | 4 (`DefaultDatabasePoolMaxConns`), never above the Server's own `MaxConns` | `WithDatabasePoolMaxConns(n ≥ 1)` | 8 × 4 = 32 sessions beyond the Server's own pool, inside the default `max_connections` of 100 |
| eager connections per derived pool | 0 (`MinConns`, `MinIdleConns` cleared) | — | a pool opened for one read must not dial ahead, nor redial after eviction |

Eviction is least-recently-used (a lease counter) and **only of idle
entries**: no lease in flight and nothing holding a connection — a pool's
`Stat().AcquiredConns() == 0` (open `Rows`, a `Row` not yet scanned, a `Tx`
all hold one), or an opener's `Querier` with `Idle() bool` (pgmotest's
`Fake`); any other opener `Querier` is never evicted. The lease covers the
window from lookup to the pool call returning, after which a held connection
shows in `AcquiredConns`, so a pool is never closed under a statement. The
cap is therefore **soft**: when every open entry is busy another database
opens anyway, and the map shrinks back at the next open or lease end. Victims
are closed outside `Server.mu`; an open runs outside it too (per-entry
`ready` channel), so one slow dial does not block other databases.
`Close` closes every entry, then the Server's own `Querier`; a handle then
fails with `ErrInvalidRequest`, and an open still in flight closes its own
result.

Tests: `database_pools_test.go` drives real pgxpool pools against the
pgproto3 fake backend (a session per database and only on the first
statement, exact names, LRU of idle entries, busy entries kept over the cap,
reopen, caps and hooks of derived pools, Close, refusals); pgmotest's
`TestDatabasePoolsUnderConcurrency` runs 16 goroutines over 5 databases under
a cap of 2 with `-race`; `TestLiveDatabasePools` checks sessions in
`pg_stat_activity` on pg15–pg18.

### The `Querier` seam

Landed P2.2.2 (`querier.go`). pgmo's reads and writes go through `Querier`
— pgx's `Exec`, `Query`, `QueryRow`, `CopyFrom` and `Begin`, with pgx's
signatures — satisfied by `*pgxpool.Pool`, `*pgxpool.Conn`, `*pgx.Conn` and
`pgx.Tx` (compile-time asserted, so a pgx change breaks pgmo's build, not a
caller's). `Server` holds a `Querier`; `NewServer(ctx, Querier)` takes any of
them (a nil one, or a nil pointer in one, is refused with
`ErrInvalidRequest`). `Server.Querier()` returns it; `Server.Pool()` is the
same value as a `*pgxpool.Pool`, nil over anything else. `Close` ends it as
its type is ended (`releaseQuerier`): `Close()` on a pool or the fake,
`Close(ctx)` on a `*pgx.Conn`, `Release()` on a `*pgxpool.Conn`; a `pgx.Tx`
stays its caller's to end. A Server over one session is not safe for
concurrent use. `TestNewServerOverAConn`, `TestCloseLeavesATransactionToTheCaller`
offline; `TestLiveNewServerOverOneSession` on pg15 and pg18.

`pgmotest` (`pgmotest/pgmotest.go`) is a scripted `Querier`, gosmotest's
shape over pgx instead of a `database/sql` driver: `New(t)`, `On`/`OnExec`
(first rule whose text contains the match; test rules before the defaults),
`Rows`/`Fail`/`Tag` or a `Result` literal, `Queries()`, `Server()`. Fidelity
to pgx, each pinned by a test:

- rows are encoded under their column type (`Result.Types`, else pgx's type
  for the first non-nil value) and scanned with pgx's own scan plans, so a
  NULL into `*string` or an out-of-range integer fails as on a connection;
  `QueryRow` gives `pgx.ErrNoRows`; `Result.Err` with no rows fails `Query`,
  after rows ends them through `rows.Err`;
- pgx's leading options are applied as pgx applies them (`NamedArgs`
  rewritten; exec modes and, on `Query` only, result formats dropped);
- `Begin`/`Commit`/`Rollback` and savepoints are recorded as pgx's own
  statements (`begin`, `savepoint sp_N`, `release savepoint sp_N`, `rollback
  to savepoint sp_N`, `commit`, `rollback`), numbered per transaction
  (`Query.Tx`), with pgx's `ErrTxClosed` and `ErrTxCommitRollback`;
  `CopyFrom` as pgx's `copy … from stdin binary;` with its rows;
- rows never closed and transactions never ended fail the test at cleanup.

The version read (`versionQuery` in `server.go`: version and
`current_database()`) has a default rule answered from `Fake.Version` and
`Fake.CurrentDatabase` (`DefaultDatabase` `postgres`); a change to that read
keeps the rule in step. Every other database is a `Fake` of its own
(`Fake.Database(name)`: own rules and `Queries`, closed on eviction, the same
Fake reopened, `Opens()` counted); `Fake.Server(opts...)` wires it in through
`WithDatabaseOpener`.

## Feature map

To be written per family as it lands: pgmo's name for each concept, the
catalog it reads, the statements it writes. The disposition of every gosmo
family (reuse / adapt / replace / drop) is in `PLAN.md` § Feature-parity
analysis.

## Errors

Sentinels as gosmo (`errors.go`, landed P1.3.1): `ErrNotFound`, `ErrUnsupported`, `ErrInvalidRequest`,
`ErrAmbiguous`, `ErrSchemaRequired`, `ErrHandleNotLoaded`,
`ErrUnsupportedVersion`, `ErrDefinitionHidden`. A server error is a
`*pgconn.PgError`; `AsPgError` exposes it, and classification is by SQLSTATE,
never message text (landed P1.3.2; every code confirmed live on pg15–pg18 by
`TestLiveErrorPredicates`):

| Predicate | File | SQLSTATEs |
|---|---|---|
| `IsPermissionDenied` | `errors.go` | `42501` |
| `IsUndefinedObject` | `errors.go` | `42P01` relation, `42704` object/role/type, `42883` function, `3F000` schema, `3D000` database |
| `IsAlreadyExists` | `errors.go` | `42P07` relation, `42710` object/role/type/constraint, `42723` function, `42P06` schema, `42P04` database |
| `IsQueryCanceled` | `errors.go` | `57014` |
| `IsRetryable` | `retry.go` | class `08`, `40001`, `40P01`, `57P01`, `57P02`, `57P03`, `57P05`; plus network errors and `io.EOF`/`io.ErrUnexpectedEOF`; never a context error |

Column codes (`42703`, `42701`) are deliberately in no predicate: from
pgmo's own read they mean a broken read or a missing version gate, not
absence. A `PgError` in the chain decides `IsRetryable` alone, so a refused
login (`28P01`) over a flaky dial is not retried.

pgmo's own refusals (landed P1.3.3, `errors.go` § Refusals) are built by
`invalidf` (`ErrInvalidRequest`), `unsupportedf` (`ErrUnsupported`) or
`notFoundf` (`ErrNotFound`), never a bare `fmt.Errorf`/`errors.New`: the
message is exactly the format, the sentinel is reached through the chain, and
a `%w` keeps its cause reachable (`unsupportedf("…: %w", ErrDefinitionHidden)`
satisfies both). The text carries `pgmo: ` + the operation once: the exported
method adds it, an unexported helper leaves it off, and a caller of an
exported method returns its error as is. `invalid_request_test.go` enforces
both by reading the source.

## Authentication

v1: password, md5 and SCRAM-SHA-256 (negotiated by pgx), TLS client
certificates, `.pgpass`, service files. Post-v1 (`PLAN.md` P16.2): cloud IAM
tokens supplied as the password via pgx's `BeforeConnect`. GSSAPI is not
planned for v1.

## Text layer (`pgsql/`)

Landed (P1.2). Stdlib only (`TestImportsOnlyStdlib`); the server's rules,
with `standard_conforming_strings` on.

| API | Rule | Pinned by |
|---|---|---|
| `QuoteIdent` | bare only for `[a-z_][a-z0-9_]*` that is not a C/T/R keyword; else `"…"`, `"` doubled — `quote_ident` (D14) | `TestLiveQuoteIdentMatchesServer`: exact on pg18; on older servers only stricter |
| `QualifiedName` | `schema.name`, each part `QuoteIdent`; just the name when schema is `""` | `TestQuote`, `FuzzNames` |
| `QuoteLiteral` | `'…'`, `'` doubled; `E'…'` with `\` doubled when it holds a backslash — `quote_literal` | `TestLiveQuoteLiteralMatchesServer`, with `standard_conforming_strings` on and off |
| `UnquoteIdent` | `"a""b"` → `a"b`; anything else unchanged, never case-folded | `TestUnquoteIdent`, `FuzzNames` |
| `Spans`, `Span.Unescape` | scan.l: `--` (ends at CR or LF), nested `/* */`, `$N`, `'…'`/`N'…'`, `E'…'`, `U&'…'`, `B''`/`X''` (no escape), `$tag$…$tag$`, `"…"`, `U&"…"`; a prefix or `$` starts a token only as a word of its own | `TestSpans`, `FuzzSpans` (every span re-scans alone as itself), `TestLiveSpanUnescapeMatchesServer` |
| `CommentSafe`, `BlockCommentSafe` | control characters → space; a space splits `*/` and `/*` | `TestCommentSafe`, `TestBlockCommentSafe`, `FuzzNames` |

`keywords.go` is the union of every supported version's non-unreserved
keywords (pg18's list: keywords are only ever added). Regenerate it from
`pg_catalog.pg_get_keywords()` on a new major release; the live test fails
on pg18 (or newer) until it is.

## Security

Identifiers and literals through `pgsql`, one implementation, pinned to the
server's own `quote_ident`/`quote_literal` by live tests. Passwords are never
sent in cleartext in DDL (pre-hashed SCRAM verifiers), and are redacted from
`WithScript` captures and observers unless the caller opts in.

## Maintaining this document

As gosmo: diagrams live in `diagram/*.mmd`; only `00-map.mmd` is inlined
here. A new family adds its diagram, its feature-map section and its row in
the map in the same change.
