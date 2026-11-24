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
file with the exact catalog text, and the live sweep on pg15 (§ Version sweep).

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
fixture self-test as well. The gated reads are `databaseSelect`
(`testdata/version_gates/database.sql`, P4.1) and `roleSelect`
(`role.sql`, P4.2). The inventory check reads `true`/`false` as the boolean
literals they are, not as column references (`boolLiteral`).

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
- a statement under a context already done is not sent (the context's
  error, not recorded); `Hold(release, r)` keeps an answer back until
  `release` is closed, or answers 57014 (`Canceled()`) when the statement's
  context ends, as a connection pgmo opened does (§ Cancel);
- rows never closed and transactions never ended fail the test at cleanup.

The version read (`versionQuery` in `server.go`: version and
`current_database()`) has a default rule answered from `Fake.Version` and
`Fake.CurrentDatabase` (`DefaultDatabase` `postgres`); a change to that read
keeps the rule in step. Every other database is a `Fake` of its own
(`Fake.Database(name)`: own rules and `Queries`, closed on eviction, the same
Fake reopened, `Opens()` counted); `Fake.Server(opts...)` wires it in through
`WithDatabaseOpener`.

### Sessions and notices

Landed P2.2.6 (`session.go`). pgx calls a connection's `OnNotice` from the
goroutine reading it, as each `NoticeResponse` is read, so a notice is
handled before the `DataRow`, `CommandComplete` or `ErrorResponse` the
server sent after it — in order with results by construction. But the hook
is fixed in the `pgconn.Config` at dial time and receives only the
`*PgConn`, so a pool cannot route one session's notices to one consumer.
Hence **`Server.OpenSession(ctx, database, onNotice) (*pgx.Conn, error)`**:
a dedicated connection, outside the Server's pools, whose `OnNotice` calls
`onNotice` (`NoticeHandler func(*pgconn.Notice)`; nil allowed). It is what a
query window needs (pgMS P9.4: one session per tab, session state kept,
notices shown with results); the caller owns it and closes it, and
`Server.Close` does not.

- Opened from `serverConfig` (shared with `databasePoolConfig`): `Connect`'s
  options re-resolved with `Database` replaced, else the wrapped pool's
  `Config()`; over anything else `ErrUnsupported`. A `DatabaseOpener` is not
  used — it returns `Querier`s, not configurations. `""` is
  `CurrentDatabase`.
- Run as pgxpool runs a new connection: `ConnectTimeout` defaulted to two
  minutes, `BeforeConnect` on the copy, `AfterConnect` after (its failure
  closes the session). An `OnNotice` already in the configuration is kept
  and called first (`chainNotices`).
- After `Close`: `ErrInvalidRequest`. Errors are prefixed
  `pgmo: open session on "db": `.

Tests: `session_test.go` against the pgproto3 fake backend (a scripted
`respond` answers notices between rows and before an error): order with
rows, completion and error; isolation between two sessions and from the
pool; no handler; the database dialled; outside the pools and left open by
`Server.Close`; hooks and the configured `OnNotice` over `NewServer`; a
failing `AfterConnect` closes the session; `ErrUnsupported` over a
`*pgx.Conn` (8 mutations, all caught). `TestLiveSessionNotices`: a
`pg_temp` PL/pgSQL function raising per row, `INFO`/`WARNING` before
`RAISE EXCEPTION` (codes `00000`/`01000`, `DETAIL`, `HINT`, `Where`), the
pool's notices not reaching the handler — green on pg15–pg18.

### Cancel

Landed P2.2.7 (`cancel.go`). A statement's context ending while the server
runs it — a Cancel button, a deadline — is a cancel. pgx's default
(`DeadlineContextWatcherHandler`) returns at once and closes the session,
sending a cancel request on its way out (`asyncClose`): the statement stops
but the session goes, with its `SET`s, temporary tables and transaction,
and a pool dials again. Checked on pg15 and pg18 (2026-11-21).

pgmo answers it as psql does instead: **`UseCancelRequests(*pgconn.Config)`**
installs pgx's `CancelRequestContextWatcherHandler` — a cancel request on a
separate connection (TLS when the session is encrypted, as libpq 17), then
the session waits for the server's answer: the statement fails with **57014**
(`IsQueryCanceled`) and the session is left idle, or in a failed transaction
(`TxStatus` `'E'`) when the statement was in one. If the server has not
answered within `cancelFallback` (5 s) the net.Conn deadline fires and the
session is closed after all, so a dead network still ends the call.

- `poolConfig` sets it, so it covers everything built from
  `ConnectionOptions`: `Connect`'s pool, `DatabaseQuerier`'s derived pools and
  `OpenSession`'s sessions. A pool given to `NewServer` keeps its own
  configuration, and so does what pgmo opens from it (derived pools,
  sessions); its owner calls `UseCancelRequests` on its
  `ConnConfig.Config` to opt in.
- The server reports the cancel as 57014 alone, not as a context error.
  pgmo's statement helpers join `ctx.Err()` to a 57014 that `ctx` caused
  (`cancelCause`), so a caller tells "I cancelled" by
  `errors.Is(err, context.Canceled)` (or `DeadlineExceeded`) as under pgx's
  default; a 57014 with `ctx` still live (`statement_timeout`,
  `pg_cancel_backend`) is left alone. A raw session from `OpenSession` gives
  the 57014 alone: test `IsQueryCanceled`.
- 57014 is not retryable (§ Retry of reads). A cancel request is racy by
  nature (it may land after the statement ended); pgx waits 100 ms after
  sending one so it cannot hit the session's next statement.
- A context already done sends nothing: pgx returns its error before
  writing, and pgmotest's `Fake` does the same.

Tests: `cancel_test.go` against the pgproto3 fake backend (each session its
own process ID and key; `sleep` runs until a cancel request names it): the
pool's session kept through `queryRowScan`, `query` and `execScan`, by cancel
and by deadline; an `OpenSession` session and another database's pool kept;
the fallback closing the session when the server ignores the cancel;
`NewServer` keeping pgx's default unless `UseCancelRequests` was set;
`cancelCause` (9 mutations, all caught). pgmotest's `Hold` answers 57014 when
the statement's context ends (`TestHoldIsCancelledAsOnAConnection`).
`TestLiveCancel` cancels `pg_sleep(60)` on the pool (same backend PID after,
`pg_stat_activity` idle), another database's pool, a session (temporary table
kept) and inside a transaction (status `'E'`, rollback works) — green on
pg15–pg18, and fails with `UseCancelRequests` removed from `poolConfig`.

### Name resolution (`search_path`)

Landed P2.2.4. pgmo never sets or trusts the session's `search_path` — it is
the caller's (a `Params` entry, the role's or database's default, or a `SET`
on a wrapped session). Every name a read resolves is written through its
schema instead, because the alternatives do not hold (checked on pg15 and
pg18):

- a schema listed ahead of `pg_catalog` shadows a bare function, relation or
  type — including types in casts and typed literals (`::text`, `::int4`,
  `::bool`, `::regclass`, `::json`, `date '…'`); only the grammar's keyword
  types (`integer`, `bigint`, `smallint`, `boolean`, `numeric`, `decimal`,
  `real`, `double precision`, `float`, `char`/`character`/`varchar`, `bit`,
  `interval`, `time`, `timestamp`) are mapped to `pg_catalog` by the parser;
- a temporary table shadows a bare catalog relation even under
  `search_path = ''` (`pg_temp` is searched first for relations), so pinning
  `search_path` at run time would not be enough, besides costing a round trip
  per read or changing the caller's session.

`TestCatalogReadsAreSchemaQualified` (`search_path_test.go`) reads every SQL
literal in the package's source through `pgsql.Spans` and flags a bare
function call, a bare relation after `FROM`/`JOIN`, any other bare `pg_`
name, and a cast or typed literal of a non-keyword type; CTE names, aliases
and grammar constructs (`COALESCE(`, `EXTRACT(… FROM …)`, `IS DISTINCT FROM`)
are exempt. `TestUnqualifiedNameScan` pins what it flags.
`TestLiveReadsIgnoreTheSessionSearchPath` connects under a `search_path`
whose look-alike `current_setting`/`current_database` would report
PostgreSQL 9.6 and checks `Version()`/`CurrentDatabase()` are the real ones.
Operators are not qualified (`OPEN-THREADS.md` § Name resolution).

### Source of truth (`pg_catalog`)

Landed P3.2. Every read pgmo makes goes to `pg_catalog`. `information_schema`
is the SQL standard's portable view, and on PostgreSQL it loses what a
management library needs (each point checked on pg15 and pg18):

- **It shows only what the role may use.** A table in a schema the role has
  `USAGE` on, with no privilege on the table itself, is in `pg_class` and
  missing from `information_schema.tables` and `.columns`. pgmo has to
  identify, list and drop objects whatever the role's privileges on them.
- **It covers only the standard's objects.** Materialized views are not in
  `information_schema.tables`. Indexes, extensions, publications and a
  relation's owner are not in it at all.
- **It has no OIDs, and it keys on names the standard assumes are unique.**
  `(constraint_schema, constraint_name)` is not unique in PostgreSQL: two
  tables' `CHECK` constraints with the same name come back from
  `constraint_column_usage` as one constraint over both tables' columns.
  Overloads are told apart only by `specific_name` (`name_oid`).
- **It names types the standard's way** (`ARRAY`, `USER-DEFINED`,
  `character varying`), not as `format_type` does.

It does not truncate names. `sql_identifier` is a domain over `name`, so it
has the same 63-byte limit as the catalog.

`TestCatalogReadsUsePgCatalog` (`catalog_source_test.go`) reads every SQL
literal in the package, using `sqlLiterals`, the walker it shares with
`TestCatalogReadsAreSchemaQualified`. It flags any name read from
`information_schema`: a view, a function or a type, or a `fmt` verb where the
name is built with `Sprintf`. The schema's name as a value
(`nspname = 'information_schema'`, needed to flag system schemas) is not a
read. A view is allowed only through `portableViews`, with the reason
nothing is lost. That map is empty: no read so far qualifies.
`TestInformationSchemaScan` pins the scan.

### Identity (`identity.go`)

Landed P3.3. Every object pgmo reads carries its `OID` (type `OID`, a
`uint32`: unique within its catalog, and within its database) and, if it
lives in a schema, `Schema` and `Name`. The OID keys every later read of the
object's parts; the qualified name is what DDL writes. Neither alone is
enough: an OID does not survive a dump and restore, and a name is unique only
per schema and catalog. A function, procedure or aggregate is unique only
with its input argument types — overloads share a name in one schema — so
the routine families embed `RoutineIdentity`:

| Field / method | From | Use |
|---|---|---|
| `OID`, `Schema`, `Name`, `Kind` | `pg_proc.oid`, `nspname`, `proname`, `prokind` | identity; `Kind.Keyword()` is `FUNCTION`/`PROCEDURE`/`AGGREGATE` (window → `FUNCTION`) |
| `ArgumentTypes []OID` | `proargtypes` (IN, INOUT, VARIADIC; an ordered-set aggregate's direct then aggregated) | exact match of one overload (`SameArguments`), the catalog's unique key with schema and name |
| `ArgumentTypeNames` | built in SQL from `proargtypes` | what `Signature()` writes |
| `IdentityArguments` | `pg_get_function_identity_arguments` | display only (modes, names, `ORDER BY`) |
| `Signature()` | | `schema.name(t1, t2)`, the DDL key |

`pg_get_function_identity_arguments` and `regprocedure` text render a type
through `format_type`, which leaves a type unqualified when the session's
`search_path` finds it (`x ty` under `idt, public`, `x idt.ty` under
`public`; checked on pg15 and pg18). Written into DDL and run under another
path, that names a different type or none. So `routineIdentityColumns` builds
the type names itself: a `pg_catalog` type is `format_type`'s name (`double
precision`, `"char"`, `text[]`); any other is `quote_ident(schema).quote_ident(name)`,
and an array of one (`typelem` whose `typarray` is the type, as `format_type`
tests) the element's name with `[]`. `proargtypes` has no typmods, so nothing
is lost.

`Signature()` is accepted after `ROUTINE` (`DROP`/`COMMENT ON`/`ALTER`/`GRANT
… ON ROUTINE`) for every kind, and after the kind's own keyword for every
routine but a zero-argument aggregate, which `AGGREGATE` writes `ag(*)`
(`ag()` is a syntax error there). An ordered-set aggregate's flat list
(`os(double precision, anyelement)`) is accepted by `AGGREGATE` and `ROUTINE`
alike. Checked on pg15–pg18.

`routineByName(ctx, q, schema, name, args)` is the lookup the routine
families build on: an empty schema is `ErrSchemaRequired` before anything is
sent; names match exactly; with `args` nil a name with several overloads is
`ErrAmbiguous` (`ambiguousf`) naming each `Signature`, and with `args`
non-nil (empty for none) only the overload whose `ArgumentTypes` equal them
is returned. `TestLiveRoutineIdentity` reads every kind through a session
whose `search_path` finds the fixture's types and replays each `Signature`
under one that finds look-alike types of the same names: each resolves to
its own OID (`to_regprocedure`) and `COMMENT ON` lands on it, while the
server's own `regprocedure` text does not resolve there.

### Name matching (`names.go`)

Landed P3.5. **Names are matched exactly, with no case-insensitive
fallback** — gosmo's `matchName` falls back to `strings.EqualFold` because
SQL Server compares names under the instance's (usually case-insensitive)
collation; PostgreSQL folds only *unquoted input*, at parse time, and stores
the result as is. `orders`, `"Orders"` and `"ORDERS"` are three tables side
by side in one schema, so a folded match is a wrong answer (or an
`ErrAmbiguous` the server would never give) waiting for the second one.

| Where | How |
|---|---|
| SQL | `=` on the name column (`p.proname = $2`); never `lower()`/`upper()`/`ILIKE`/`~*` |
| Go, over a list already read | `matchName[T](items, name, key)`: exact; none → `ErrNotFound`, more than one → `ErrAmbiguous`, both bare for the caller to wrap |
| A name a person typed | `pgsql.FoldIdent` first — the server's `parse_ident` on one part: trim, `"…"` unquoted, otherwise ASCII `A`–`Z` lowered only (`ÄbC` means `Äbc`); no truncation. The caller's step: only it knows the text was typed |

Checked on pg15–pg18: `parse_ident` lowers ASCII only in a UTF8 database,
trims white space outside quotes, does not truncate past 63 bytes, and
refuses `U&"…"`. A filter's text (P3.6, § Object filters) is folded the
same way and then matched exactly — no `ILIKE`/`lower()` there either.

Guard (`names_test.go`): `TestNoCaseFoldingOfNames` refuses, in the non-test
Go of the root, `pgsql/` and `pgmotest/`, any call of a case-folding
function (`strings`/`bytes` `EqualFold`, `ToLower`, `ToUpper`, `ToTitle`, …,
`unicode.ToLower`/`SimpleFold`, `x/text/cases`), and in every SQL literal's
code `lower(`/`upper(`/`initcap(`/`casefold(`, `ILIKE` and `~*`/`~~*` (and
their negations), skipping literals, quoted identifiers and comments.
`caseFoldingAllowed` lists the exceptions by `file:function` with why the
text is not a name — one so far, `connection_dsn.go:validateParams` (setting
names are case-insensitive); an entry matching nothing fails.
`TestSQLCaseFoldingScan` and `TestGoCaseFoldingScan` pin both scans.
`TestMatchName`, `TestFoldIdent` and `FuzzNames`
(`FoldIdent(QuoteIdent(n)) == n`) pin the helpers; 13 mutations of helpers
and guard, all caught. Live: `TestLiveFoldIdentMatchesServer` (`parse_ident`
on a corpus of bare, quoted, non-ASCII, keyword, padded and over-long input)
and `TestLiveNamesDifferingByCase` (six tables differing only by case — ASCII
and `Ä`/`ä` — are six OIDs; `to_regclass` of each typed form lands on the
table `FoldIdent` names; `matchName` finds each by stored name), green on
pg15–pg18.

### Object filters (`object_filter.go`)

Landed P3.6. `ObjectFilter` narrows a catalog listing **at the server**, as
gosmo's does — the Object Explorer folder filter is built on it. A family
maps its columns (`filterColumns`: name, schema, a system condition) and
appends `clause(cols, args)` to its `WHERE`; the filter's parameters follow
the query's own (`$len(args)+1` on). A zero filter narrows nothing.

| Field | SQL |
|---|---|
| `Name`, `Schema` `[]TextCriterion` (AND-ed) | contains `pg_catalog.strpos(col, $n) > 0`, does not contain `= 0`, equals `col = $n::pg_catalog.text`, does not equal `<>` |
| `System *bool` (nil: both) | `systemSchema(nsp)`: `starts_with(nsp, 'pg_') OR nsp = 'information_schema'`; `false` negates it |
| `ExtensionMember *bool` (nil: both) | `isExtensionMember(catalog, oid)`: `EXISTS` a `pg_depend` deptype `'e'` row to a `pg_extension` (§ System objects); `false` negates it |

Where gosmo compares `LOWER()` of both sides, pgmo **folds the criterion's
text with `pgsql.FoldIdent` and matches exactly** — psql's `\d` pattern rule:
`cust`/`CUST` find `customers`, `"Cust"` finds `Customers` alone, `ÄB` means
`Äb`. A filter so never lists an object the same text typed into a statement
would not name, and needs no case folding in SQL (which under the catalog's
`"C"` collation would fold ASCII only anyway). A caller holding a stored name
passes `pgsql.QuoteIdent(name)`. Text empty once folded is no criterion.

`strpos` and `=`, not `LIKE`: `%`, `_` and `\` in a name match literally with
no escape to get wrong (an `ESCAPE '\'` literal depends on
`standard_conforming_strings`). The value binds as `text`, never `name`, so
one over 63 bytes is not truncated onto a 63-byte name. The `pg_` prefix is
reserved for schemas (`CREATE SCHEMA pg_x` → 42939), so `systemSchema`
covers `pg_catalog`, `pg_toast` and every session's `pg_temp_N` /
`pg_toast_temp_N`. An unknown `TextOp` is refused (`ErrInvalidRequest`),
also on a column the family lacks; a criterion on a column it lacks is
dropped. gosmo's `Created` and `MemoryOptimized` do not carry over
(PostgreSQL's catalog records no creation date).

Tests: `object_filter_test.go` asserts each operator's SQL and arguments in
full, the numbering after the query's own arguments (without writing into
the caller's array), dropped columns and refusals; `TestLiveObjectFilter`
runs the clause on the server over tables differing only by case or holding
`%`, `_`, `\`, `"`, non-ASCII and a 63-byte name, and splits system from
user schemas — green on pg15–pg18. 13 mutations, all caught.

### System objects (`system_objects.go`)

Landed P3.7. Every family embeds `Origin`, two independent flags:

| Field | Rule | SQL |
|---|---|---|
| `IsSystem` | the object's schema is `information_schema` or named `pg_*` (`pg_catalog`, `pg_toast`, every session's `pg_temp_N`/`pg_toast_temp_N` — the session's own temporary objects included); a schema's own name for a schema | `systemSchema(nsp)`; Go: `IsSystemSchema(name)` |
| `Extension` | the extension the object is a member of, `""` for none (`IsExtensionMember()`) | `extensionOf(catalog, oid)`: `pg_depend` `classid = catalog`, `objid = oid`, `objsubid = 0`, `refclassid = pg_extension`, `deptype = 'e'` |

A family reads both with `originColumns(nsp, catalog, oid)` into
`scanOrigin(&x.Origin)`, and sets its `filterColumns` from `systemSchema`
and `isExtensionMember` on the same expressions, so `ObjectFilter.System` /
`ExtensionMember` select exactly the rows whose flags say so. `catalogTable`
(`pg_class`, `pg_proc`, `pg_type`, `pg_namespace` so far) is written into the
SQL as a literal, so only its constants are values.

**An extension member is not a system object** (pgMS `docs/decisions.md`).
It lives where it was installed — usually a user schema (`pg_stat_statements`
in `public`, pg_cron's `cron` schema, itself a member) — psql's `\d` lists it
without `S`, and the user installed it. What sets it apart is that its DDL is
the extension's: `pg_dump` writes `CREATE EXTENSION`, and `DROP` of a member
alone is refused (2BP01). An extension installed into `pg_catalog`
(`plpgsql`) has members with both flags set. Only membership counts:
deptype `'x'` (`ALTER … DEPENDS ON EXTENSION`) is a dependency, not
membership, and leaves the object a user one. An object is a member of one
extension at most; `objsubid` is always 0 for a member.

Tests: `system_objects_test.go` pins `IsSystemSchema` (exact case:
`PG_CATALOG` is a user schema) and the membership SQL in full;
`object_filter_test.go` the `ExtensionMember` clause. `TestLiveSystemObjects`
builds a throwaway database with `citext` and `seg`, a table and a schema
added by `ALTER EXTENSION … ADD`, a function that only `DEPENDS ON
EXTENSION`, and a temporary table, and checks each `Origin` for relations,
functions, types and schemas; that `IsSystemSchema` agrees with the SQL rule
on every schema; and that the four `System` × `ExtensionMember` filters
partition the relations as their Origins do — green on pg15–pg18. 14
mutations, all caught.

### Retry of reads

Landed P2.2.5. A read is idempotent, so a transient failure (`IsRetryable`,
§ Errors) is answered by running it again on a fresh session: `withRetry`
(`retry.go`) makes `readRetryAttempts` (3) tries, 50 ms × n apart, stopping
at once on any other error or a done `ctx` (whose error is joined with the
transient cause). The helpers in `statement.go` are the only way pgmo sends
a statement:

| Helper | Retried | For |
|---|---|---|
| `query[T](ctx, q, scan, sql, args…)` | yes | a list read's raw form, `pgx.CollectRows` with a `pgx.RowToFunc`; called only by `scanRows` (§ Row iteration) |
| `queryRow(ctx, q, scan, sql, args…)` | yes | a single-row read with its own scan; called only by `queryRowScan` and `readByName` |
| `queryRowScan(ctx, q, sql, args, dest…)` | yes | a single-row read into destinations; the version read |
| `execScan(ctx, q, stmt, args, dest…)` | never | a write reading a value back (`RETURNING`) |

- **The retried unit is the whole read**, rows scanned and `Err` checked.
  pgx returns `Rows` before the statement has run to the end, and a
  deadlock, a terminated backend or a dropped connection during execution
  arrives through `Rows.Err`; a retry around `Query` alone would see none of
  them. What a failed attempt scanned is discarded, so a scan callback must
  have no effect beyond its value. `pgx.ErrNoRows` is not retried.
- **Never on a `pgx.Tx`** (`retries`): a `40001`/`40P01` has aborted the
  transaction, a broken connection has ended it, and a fresh session is
  outside it. Only the whole transaction may be retried, by its owner.
  `InTransaction` reaches the helpers through its `pgx.Tx` (§ Transactions).
- **Never a write.** A connection lost after the server ran a write is
  exactly the failure a read retries on, so a retried write runs twice.
  `TestNoWriteGoesThroughARetryingRead` reads the SQL handed to every read
  helper (following consts and locals, literals and comments blanked
  through `pgsql.Spans`) and flags DML, DDL, utility commands and the
  built-in functions with effects (`nextval`, `set_config`,
  `pg_terminate_backend`, advisory locks, …); `TestWriteInSQLScan` pins the
  scan and `TestReadHelperGuardSeesEachCallForm` the call forms.
- A pool hands a dead idle session back only within a second of its last
  use (pgxpool pings one idle longer); the retry covers that window and a
  session lost mid-statement. `TestLiveReadRetriesATerminatedSession`
  terminates the pool's one session and checks a bare statement fails
  retryably, `queryRowScan` and `query` recover, and `execScan` does not
  retry (green on pg15 and pg18).
- The version read goes through `queryRowScan`, so `Connect` tries a refused
  or dropped first connection up to three times (gosmo's `loadInfo` does the
  same). A `ConnectTimeout` expiring is not retried — pgx reports it as
  `context.DeadlineExceeded` — so an unreachable host costs one timeout
  (checked against an unroutable address).

### Row iteration (`rows.go`)

Landed P3.4: gosmo's `scanRows`/`readByName`/`foundRow`, over pgx and the
retrying helpers above. Every catalog read ends in one of three shapes:

| Helper | For |
|---|---|
| `scanRows[T](ctx, q, what, scan, sql, args…)` | a list read; `scan(func(...any) error) (T, error)` builds one row's value |
| `readByName[O, T](ctx, q, o, scanX, sql, args, notFound, what)` | a single-row read by name, the row built by the family's `scanX(owner, scan)` |
| `foundRow(v, err, notFound, what)` / `rowErr(err, notFound, what)` | the tail of any other single-row read where no row means absent; `rowErr` when the read fills in more after the row |

- **One wrap for every failure.** The query's error, a scan's and the rows'
  `Err` come back as one error, wrapped `pgmo: <what>: %w` (`wrapRead`) — so
  a failure mid-iteration names the read instead of surfacing as a bare
  `context deadline exceeded`. An empty `what` returns it bare, for an
  unexported reader whose caller names the operation (`routineByName`).
- **`pgx.ErrNoRows` → the caller's `notFound`** (built with `notFoundf`),
  matched with `errors.Is`, and never retried. Any other error is wrapped, so
  a permission or connection failure never reads as absence.
- **Owner and Querier are separate parameters** of `readByName`, unlike
  gosmo's (where `*Server`/`*Database` is both): the statement runs on what
  `s.on(ctx)`/`s.onDatabase(ctx, name)` answers — a transaction, a pool —
  while the object points back to its owner.
- **A scan never queries.** It runs while the rows hold their connection (on
  a pool: one held per row while acquiring another — exhaustion under load)
  and again on a retry. It is handed only the row's `Scan`; a family fetches
  children in one query ordered by parent and groups them in Go.
- An empty list is a nil slice (`query` normalises `pgx.CollectRows`'
  empty non-nil one).
- A single-row read whose row always exists (`Server.Info`, `Capabilities`'
  `current_user` row) stays on `queryRowScan` with its own wrap: no row there
  is not absence of the object asked for. `TestCapabilitiesNoRow` pins its
  `pgx.ErrNoRows`.

Guards (`rows_test.go`): `TestRowIterationGoesThroughTheHelpers` refuses a
`Query`/`QueryRow` method call, a `Next()` loop, a `pgx` row collector, or a
direct `query`/`queryRow` call outside the files allowed them
(`rawRowAccess`: `statement.go`, `rows.go`, and `database_pools.go`'s
forwarding `Querier`). `TestNoQueryInsideARowScan` reads every scan handed to
`scanRows`/`readByName`/`query`/`queryRow` — a func literal or a package
function by name, following the package functions it calls — and flags a
statement helper, the routing (`on`, `onDatabase`, `DatabaseQuerier`), a
`Querier` method, or any call passed the read's `ctx`. Each has a planted
self-test; `TestNoWriteGoesThroughARetryingRead` covers the two new helpers'
SQL. 13 mutations of the helpers and guards, all caught.

### Transactions

Landed P2.3.1 (`transaction.go`). **`Server.InTransaction(ctx, fn)`** begins
a transaction on the Server's own Querier, runs `fn` with a context carrying
it, and commits when `fn` returns nil; `fn`'s error, a panic (re-raised
after), or `ctx` — or the Server, by `Close` — ending first rolls back.
PostgreSQL's DDL is transactional, so a multi-step change lands whole.

- **Routing is by context.** Every pgmo method sends through
  `s.on(ctx)` (its own database) or `s.onDatabase(ctx, name)` — never
  `s.q` or `DatabaseQuerier` directly — so its statements join the
  transaction `ctx` carries. Reads too: they must see the transaction's
  uncommitted writes. Inside it the helpers see a `pgx.Tx` and do not retry
  (§ Retry of reads).
- **One database.** A session is bound to its database (no `USE`), so
  `onDatabase` refuses any other with `ErrUnsupported`; it does not run the
  statement outside the transaction.
- **No nesting** (unlike gosmo, which joins the outer call): `InTransaction`
  with a context already inside one — on this Server or another — is
  `ErrInvalidRequest` before anything is sent. An inner "commit" that commits
  nothing hides where atomicity ends; compose the steps in one `fn`, with a
  savepoint for a step allowed to fail. `~/go/pgms/docs/decisions.md`.
- **A tolerated failure is not committed.** A failed statement aborts the
  transaction on the server (25P02 after it); `fn` returning nil anyway gets
  `COMMIT` answered with `ROLLBACK`, reported as `pgmo: commit transaction:
  …` wrapping `pgx.ErrTxCommitRollback`. The server decides, so a failure
  `fn` rolled back to its own savepoint does not count — gosmo's `fail`
  bookkeeping is not needed.
- **Ending.** `fn`'s context is `ctx` bound to the Server's lifetime
  (`Server.bound`), so `Close` cancels a statement in flight (cancel
  request, 57014) instead of waiting for the pool's acquired connection; it
  is cancelled when `InTransaction` returns. `fn` returning nil on an ended
  context rolls back (pgx would not send `COMMIT` and would close the
  session). `ROLLBACK` runs on `context.WithoutCancel`, bounded by
  `txRollbackTimeout` (5 s); a failed rollback closes the session (pgx),
  which ends the transaction on the server too.
- **Over a `pgx.Tx`** (`NewServer` on one) it is a savepoint — pgx's `Begin`
  on a `Tx` — released or rolled back to; the outer transaction stays the
  caller's.
- `refuseInTx(ctx, s, op)` is the `ErrUnsupported` refusal for statements
  PostgreSQL refuses in a transaction block (`CREATE DATABASE`,
  `CREATE INDEX CONCURRENTLY`, `VACUUM`, `ALTER SYSTEM`) and methods that need
  a session of their own; each such method calls it before sending.
- `fn`'s statements share one session: no concurrent calls with its
  context. `Querier()`, `Pool()` and `OpenSession` are outside it.

Tests: `transaction_test.go` (external, over `pgmotest`; `export_test.go`
exposes `on`, `onDatabase`, `refuseInTx`): commit, rollback on error and
panic, `ErrTxCommitRollback`, begin/commit failures, `ctx` ending, `Close`
cancelling a held statement, nesting refused both ways, database routing,
`refuseInTx`, the savepoint form; 12 mutations, 11 caught first time, the
survivor (refusal text) caught after tightening. `TestLiveInTransaction`
(throwaway schema): one session that sees its own uncommitted table while
another session does not; rollback on `fn`'s error; a tolerated 42P07 not
committed; `Close` cancelling `pg_sleep(60)` promptly — green on pg15–pg18,
and fails three ways with routing removed.

### Script mode (`WithScript`)

Landed P2.3.2 (`script.go`). **`WithScript(ctx)`** returns a context and a
`*ScriptCollector`; every write issued with it is captured instead of sent,
and returns as if it had succeeded (a zero `CommandTag`). `Scripting(ctx)`
tells a caller so. Reads are unaffected and go to the server.

- **The write chokepoints** are `s.exec(ctx, stmt, args...)` (the Server's
  own database, through `s.on`) and `s.execIn(ctx, name, stmt, args...)`
  (through `s.onDatabase`). Every write method goes through one of them,
  never `Querier.Exec` directly; both run once, never retried, bounded by
  the Server's lifetime (`bound`), errors unwrapped for the caller to name.
  A write reading a value back (`execScan`) checks `Scripting` itself,
  captures through `s.capture`, and returns its handle (`createdObject`).
- **Refusals are the same while scripting**: `execIn` asks `onDatabase`
  before capturing, so a script never holds a statement the real call
  would have refused.
- **`ScriptEntry{Database, SQL}`**: the database whose session would run it
  (no `USE` — a multi-database script runs in pieces), and the statement
  with its arguments bound. No server name: a collector shared by two
  Servers is told apart by database only (`OPEN-THREADS.md`).
- **`String()`** terminates each entry once (a `;` already there kept; one
  after a trailing `--` comment on its own line), a blank line between, and
  — when entries span databases — a `-- database <name>` line
  (`CommentSafe`) opening each run.
- **`bindScriptArgs`** replaces each `$N` the lexer reads as a parameter
  (`pgsql.Spans`' `SpanParam`) — never one inside a literal, identifier,
  comment or dollar-quoted body — with `scriptLiteral(arg)`. Every `$N`
  needs an argument and every argument a `$N` (`ErrInvalidRequest`); a value
  with no literal form is `ErrUnsupported`. Errors are prefixed `script:`,
  the write adds `pgmo: <op>:`.
- **`scriptLiteral`** renders what pgx would have bound, in the same
  position: strings, `[]byte` (`'\x…'`), `[]string` (array text) and times
  as untyped literals through `QuoteLiteral` (so `E'…'` when a backslash is
  in it: the script reads the same under either
  `standard_conforming_strings`); negative numbers parenthesised (`x - (-1)`,
  never `x --1`); NaN, ±Infinity and −0 as typed literals; a time in its own
  zone with its offset (`'… 10:30:00.123456+01:00:00'`), which a
  `timestamptz` reads as the instant and a `timestamp` as the wall clock —
  exactly as pgx binds each; pointers and `driver.Valuer` followed, nil →
  `NULL`; a space after `U&` so a literal does not become a Unicode-escape
  one. Anything else is refused, never guessed.
- **`InTransaction` under `WithScript`** sends nothing: it captures `BEGIN`,
  `fn`'s writes and `COMMIT` — `ROLLBACK` when `fn` fails or panics — so the
  script applies whole or not at all (unlike gosmo, which scripts no
  transaction; `~/go/pgms/docs/decisions.md`). `fn`'s context carries a
  `serverTx` with a nil `tx`, so nesting, another database and `refuseInTx`
  refuse as for real, while `on`/`onDatabase` send `fn`'s reads to the
  Server's own Querier.
- `setIfApplied`/`setPtrIfApplied` (a write mirroring its change onto its
  receiver) and `createdObject` (a create returning its Ref under
  `Scripting`, or on a read-back hidden from the role) as gosmo.

Tests: `script_test.go` (external, over `pgmotest`; `export_test.go`
exposes `exec`/`execIn`): capture vs. send, bind failures capture nothing,
scripted `InTransaction` (commit, error, panic) and its refusals, `String`,
8 goroutines × 50 captures under `-race`. `script_bind_test.go`:
`bindScriptArgs` positions and refusals, every `scriptLiteral` form,
`setIfApplied`, `createdObject`. 19 mutations, 18 caught first time, the
survivor (`onDatabase` handing a scripted read the nil `tx`) caught after a
test was added. `TestLiveScriptLiteralsMatchBinding` runs each value bound
and as its scripted literal (simple protocol) and compares the text read
back, with `standard_conforming_strings` on and off, under a non-UTC
`TimeZone`; it found −0 and `timestamp` (both fixed). `TestLiveScriptRunsAsCaptured`
runs a captured transaction's `String()` and finds the bound row. Green on
pg15–pg18.

### Secrets and passwords

Landed P2.3.3 (`secret.go`, `internal/scram`). A secret reaches the server
and nothing else; a password does not even reach the server.

- **`s.execSecret(ctx, stmt, shown)`** is `exec` for a statement carrying a
  secret: `stmt` runs; `shown` — the same statement built by the same code
  with the placeholder in the secret's place — is what `WithScript`
  captures, unless the context carries **`WithScriptSecrets(ctx)`**, which
  captures `stmt` (for a script a machine runs). A secret statement is DDL
  and binds no arguments. When `stmt` is not needed (scripting without
  secrets) the caller passes `""` and skips building it — for a password,
  the server read and the PBKDF2. gosmo's `redactSecrets` (rewrite a built
  statement's literals) is not ported: building both forms is simpler and
  cannot miss. No statement observer yet (`OPEN-THREADS.md`).
- **`PasswordPlaceholder`** (`<insert password here>`) is written **bare**,
  not as a literal: `ALTER ROLE app PASSWORD <insert password here>` fails
  to parse when run unreplaced, where a quoted placeholder would run and set
  a password anyone can read in pgmo's source (`~/go/pgms/docs/decisions.md`).
- **Passwords are sent as SCRAM-SHA-256 verifiers** computed here
  (`passwordClause` → `passwordVerifier`), as psql's `\password`: a fresh
  16-byte salt, the session's `scram_iterations` (read on `s.on(ctx)`, so a
  `SET LOCAL` inside `InTransaction` applies; `current_setting(…, true)` is
  NULL on 15, whose fixed count is the setting's default 4096 — no version
  gate needed). The server stores a verifier as it is, so the cleartext is in
  no log line, `pg_stat_statements` entry, `pg_stat_activity` row or error.
  Always SCRAM, whatever `password_encryption` says: md5 is deprecated in 18,
  and a SCRAM verifier serves an `md5` `pg_hba.conf` line too.
- **`scram.Normalize` is PostgreSQL's `pg_saslprep`** (`src/common/saslprep.c`),
  not pgx's login preparation: ASCII as is; C.1.2 → space, B.1 dropped
  (C.1.2 first: U+200B is in both); NFKC (`x/text/unicode/norm`); a
  prohibited (C.1.2–C.9), unassigned-in-3.2 (A.1) or bidi-violating (D.1/D.2)
  character — checked on the *mapped* password, as PostgreSQL checks it —
  or an empty result falls back to the password as is. The six tables
  (`tables.go`) are generated from Python's `stringprep` (`gen_tables.py`)
  and were checked code point for code point against PostgreSQL 15's and
  18's `saslprep.c`. pgx prepares a login password with PRECIS OpaqueString
  (NFC), so a password NFKC changes (full-width letters, ligatures,
  superscripts) logs in with psql, not pgx — whether pgmo or the server
  hashed it (`OPEN-THREADS.md`).
- **Refused** before anything is sent (`ErrInvalidRequest`): an empty
  password (PostgreSQL clears rather than sets it, and checks a verifier of
  `""` too), one holding NUL (no client can send it), a `scram_iterations`
  that is not a positive count.
- **`s.alterRolePassword(ctx, role, password)`** (`ALTER ROLE … PASSWORD
  '<verifier>'`) is the write the role family's password methods build on
  (P4.13.2); until then only tests call it.

Tests: `internal/scram` — verifier vectors computed independently (Python
`hashlib`), every SASLprep step and fallback (`normalizeCases`, including a
case per refusal where normalizing would have changed the password), table
edges, `Parse`. `secret_redaction_test.go`: `secretWrites` lists every write
sending a password — a new one goes there — and
`TestSecretsNeverCapturedOrSent` holds each to the policy (capture: the bare
placeholder, nothing sent, not even the read; `WithScriptSecrets`: a
verifier at the server's count; run: a verifier sent, 4096 on 15; a failed
write's error names neither). 18 mutations, all caught but one equivalent
(`pg_saslprep`'s UTF-8 check: U+FFFD is prohibited anyway), three of them
after cases were added. `TestLiveRolePasswordMatchesServer`: for 24
passwords across every SASLprep branch, the server's own hash of the
cleartext, recomputed here with its salt, is the same string; pgmo's stored
verifier verifies; pgx logs in with it and not with a wrong one; `SET LOCAL
scram_iterations` applies; the cleartext is in no `pg_stat_statements`
entry. Green on pg15–pg18.

### Capabilities of the connected role

Landed P2.4.1 and P2.4.2 (`capabilities.go`). gosmo's `Capabilities`
reduced to what PostgreSQL checks at the cluster scope, and its
`DatabaseCapabilities` to the three database privileges.

- **`Server.Capabilities(ctx)`** is one read (`capabilitiesQuery`, through
  `s.on(ctx)`, so inside `InTransaction` a `SET LOCAL ROLE` is honoured) of
  **`current_user`** — the role privileges are checked against, not
  `session_user`. `pg_roles`, since `pg_authid` is superuser-only.
- **Attributes** `Superuser`, `CreateRole`, `CreateDB`, `Replication`,
  `BypassRLS` as stored: a superuser's `CreateDB` may be false; ask
  `IsSuperuser` first. From 16 `CREATEROLE` manages only roles the role
  holds `ADMIN OPTION` on.
- **`PredefinedRoles`** maps each `ProbedPredefinedRoles` name *the server
  defines* to `pg_has_role(current_user, r, 'USAGE')`: membership, direct or
  nested, whose privileges are inherited — what the server checks
  (`has_privs_of_role`). A `NOINHERIT` member (or a 16+ grant `WITH INHERIT
  FALSE`) reads false; `'MEMBER'` would answer true (checked on pg15 and
  pg18). A superuser reads true for every role, as the server answers. A role
  the server does not define (`pg_maintain` before 17) has no key
  (`Defines`), found by the catalog rather than a version gate.
- **Fail open.** No three-way `CapabilityState`: every answer here is a
  boolean the server gives without NULLs. `Probed()` is false for a nil or
  zero value, where every accessor reads false — a gate checks `Probed`
  before withholding (`~/go/pgms/docs/db-rules.md` § Permission gating).

- **`Server.DatabasePrivileges(ctx)`** maps every database's name to its
  `DatabasePrivileges` for `current_user`, in one read
  (`databasePrivilegesQuery`, through `s.on(ctx)` like `Capabilities`):
  `Create`, `Connect`, `Temporary` are `has_database_privilege(oid, …)` —
  inherited, PUBLIC's included, a `NOINHERIT` member's group grant not —
  and `AllowConnections` (`datallowconn`), `Invalid` (`datconnlimit = -2`,
  an interrupted `DROP DATABASE`) the catalog's own refusals. `pg_database`
  is shared, so the Server's own database answers for all of them: no
  per-database pool is opened. This is gosmo's `DatabaseAccess`, not its
  `Database.Capabilities`: schema and object privileges are asked where
  they are gated, not probed up front.
- **`CanConnect`** = `Connect && AllowConnections && !Invalid`: what to ask
  before expanding a database or opening a pool on it. A superuser holds
  every privilege yet cannot connect to `template0` or an invalid database
  either. `pg_hba.conf` and connection limits are not visible; the server
  may still refuse.
- No `Probed`: an error is the "nothing known" answer, and a database
  missing from the map (created since the read) is the caller's to treat as
  unknown — fail open.

Tests: `capabilities_test.go` (pgmotest: every field from its own column,
`$1` is `ProbedPredefinedRoles`, nil-safety, the error prefixed once, the
read joins the transaction, no row is `pgx.ErrNoRows`); 13 mutations, all
caught. `TestLiveCapabilities`: the superuser, a role with every attribute
in `pg_monitor` (holds `pg_read_all_settings`/`pg_read_all_stats` by
nesting), a `NOINHERIT` member of `pg_signal_backend` (defined, not held),
`pg_maintain` defined from 17 — green on pg15–pg18. Database privileges:
`TestDatabasePrivileges*` (each column its own field, `CanConnect`'s three
terms, empty is an empty map, the error prefixed once, the read joins the
transaction); 10 mutations, all caught. `TestLiveDatabasePrivileges`: three
throwaway databases (CONNECT/TEMPORARY revoked from PUBLIC with CREATE to a
group; `ALLOW_CONNECTIONS false`; marked invalid), as the superuser, an
inheriting and a `NOINHERIT` member of the group; the closed and invalid
ones refuse a connection — green on pg15–pg18.

### Server info

Landed P3.1 (`server_info.go`). gosmo's `ServerInfo` adapted: gosmo loads
it once at connect and caches it (`Server.Info()`); pgmo reads it on demand,
**`Server.Info(ctx) (*ServerInfo, error)`**, afresh each call — a standby's
`InRecovery` flips on promotion and `ConfigLoadTime` on a reload. The
constructor still reads only `versionQuery`. One row (`serverInfoQuery`,
through `s.on(ctx)`, so inside `InTransaction` a `SET LOCAL ROLE` decides
what is shown):

- `Version` (`server_version_num`), `VersionString` (`server_version`, with
  the packager's suffix — the label), `Banner` (`version()`, too long for a
  label row).
- `InRecovery` (`pg_is_in_recovery()`), `MaxConnections`,
  `SuperuserReservedConnections`, `StartTime`
  (`pg_postmaster_start_time()`), `ConfigLoadTime` (`pg_conf_load_time()`),
  `ClusterName`, `Encoding` (`server_encoding`), `DataChecksums`. Every one
  readable by any role (checked as a plain role on pg15 and pg18).
- **Paths** `DataDirectory`, `ConfigFile`, `HBAFile` from **`pg_settings`**,
  not `current_setting`: for a role without superuser or
  `pg_read_all_settings`, `current_setting('data_directory')` raises 42501
  while the view returns the row with `setting` NULL — so one read survives
  every role. NULL → empty and `PathsHidden` (gosmo's
  `SysInfoUnavailable` rule: empty means "not shown", not "none").
- **`Managed`** (`ManagedService`: `rds`, `aurora`, `azure`, `cloudsql`,
  `alloydb`, `""`) from the administrator role each service creates
  (`managedRoles`: `rds_superuser`, `azure_pg_admin`, `cloudsqlsuperuser`,
  `alloydbsuperuser`, sent as `$1`), Aurora told from RDS by an
  `aurora_version` function. A hint, not proof; untested against a real
  service (none in the lab) — the P16.2 managed-PostgreSQL work confirms it.

Tests: `server_info_test.go` (pgmotest: every field from its own column,
`$1` is the sorted role names, NULL paths → hidden, each service, the error
prefixed once, nil-safe `IsManaged`); mutations (two scan destinations
swapped, Aurora-first dropped, `PathsHidden` constant) all caught.
`TestLiveServerInfo` holds every field to the server's own answer read on the
admin pool, as the superuser, a plain role (paths hidden) and a
`pg_read_all_settings` member (paths shown) — green on pg15–pg18.
`InRecovery = true` is fake-tested only: the lab has no standby.

### Databases (`database.go`)

Landed P4.1 (reads; the writes are P4.13.1). gosmo's `Database` adapted:
`Server.Databases(ctx, ObjectFilter)`, `Server.DatabaseByName(ctx, name)`
(exact, `ErrNotFound`), `Server.DatabaseRef(name)` (lookup-free, `Name`
only), `Database.Server()`. One read, `databaseSelect(num)`, over
`pg_database` joined to `pg_tablespace`, through **`s.on(ctx)`** for every
database — `pg_database` is a shared catalog, and the database described may
take no connections (template0, `ALLOW_CONNECTIONS false`, an invalid one).

| Field | Source |
|---|---|
| `OID`, `Name` | `oid`, `datname` |
| `Owner` | `pg_get_userbyid(datdba)` |
| `Encoding` | `pg_encoding_to_char(encoding)` |
| `LocaleProvider` | `datlocprovider`, the catalog letter (`c`/`i`/`b`; `String()` names it) — reused by collations (P4.11) |
| `Collate`, `CType` | `datcollate`, `datctype` |
| `Locale` | `daticulocale` (15–16) / `datlocale` (17+), `colSince`; empty under libc |
| `ICURules` | `daticurules` (16+), `colSince`; empty on 15 |
| `CollationVersion` | `datcollversion`; empty when the provider reports none (libc `C.UTF-8` on the lab) |
| `IsTemplate`, `AllowConnections` | `datistemplate`, `datallowconn` |
| `ConnectionLimit` | `datconnlimit` as stored: -1 none, -2 invalid (`IsInvalid()`) |
| `Tablespace` | `pg_tablespace.spcname` of `dattablespace` |
| `Settings` | `pg_db_role_setting.setconfig` with `setrole = 0` (`ALTER DATABASE … SET`), split at the first `=` into `ParameterSetting{Name, Value}`; nil for none |
| `Comment` | `shobj_description(oid, 'pg_database')` |

Not gosmo's: no state, recovery model, compatibility level, read-only flag
or creation date — PostgreSQL has none. Every column is readable by any role
(checked as a plain role on pg15 and pg18; `pg_db_role_setting` included),
so the listing shows every database whatever the role's privileges —
`Server.DatabasePrivileges` says which it may connect to. An `ObjectFilter`
narrows by name only (`d.datname`); a database has no schema and is not a
system object by pgmo's rule, so `Schema`/`System`/`ExtensionMember` narrow
nothing.

**`Database.Size(ctx)`** is `pg_database_size` on demand, not a listing
column: the server walks every file of the database on each call (psql's `\l`
leaves it to `\l+` for the same reason). Read through `pg_database` by the
bound name, so a missing database is `ErrNotFound` rather than `3D000`, and
it works on a `DatabaseRef`. The server answers a role holding `CONNECT` or
the privileges of `pg_read_all_stats`, and refuses any other with 42501
(`IsPermissionDenied`), which `Size` passes through.

Tests: `database_test.go` (pgmotest: every field from its own column, the
name bound as `$1` and compared exactly, the filter pushed down, NULL
settings, `ErrNotFound`, the error wrapped once, `Size`'s three outcomes,
`DatabaseRef` reading nothing); eleven mutations, ten caught, one equivalent
(`IsInvalid` as `< -1`: no `datconnlimit` is below -2). `TestLiveDatabases`
on pg15–pg18: an ICU template (rules on 16+) taking no connections, with an
owner, a limit, a comment, a list setting holding `=`, and a role-in-database
setting that must not appear; two names differing only by case (builtin
provider on 17+); `Size` as superuser, through `PUBLIC`'s `CONNECT`, refused
without it, and answered for a `pg_read_all_stats` member. Dropping the
`setrole = 0` condition is caught live.

### Roles (`role.go`)

Landed P4.2 (reads; the writes are P4.13.2). PostgreSQL's one principal
replaces gosmo's `Login` and `ServerRole` (and has no database-user
counterpart): `Server.Roles(ctx, ObjectFilter)`, `Server.RoleByName(ctx,
name)` (exact, `ErrNotFound`), `Server.RoleRef(name)` (lookup-free, `Name`
only), `Role.Server()`. One read, `roleSelect(num)`, over `pg_roles` (any
role may read it; `pg_authid` is superuser-only and adds only the password
hash, so whether a role has a password is not read), through **`s.on(ctx)`**
— `pg_authid`, `pg_auth_members` and `pg_db_role_setting` are shared.

One row per role carries everything: the per-database settings and the
membership grants in both directions are `array_agg`s in three `LEFT JOIN
LATERAL` subqueries — parallel arrays sharing one `ORDER BY` per subquery,
zipped in Go (`membershipArrays.grants`, `groupDatabaseSettings`; arrays of
different lengths are an error, never a panic). One statement, one snapshot,
no query in a scan.

| Field | Source |
|---|---|
| `OID`, `Name` | `oid`, `rolname` |
| `Superuser`, `Inherit`, `CreateRole`, `CreateDB`, `CanLogin`, `Replication`, `BypassRLS` | `rolsuper`, `rolinherit`, `rolcreaterole`, `rolcreatedb`, `rolcanlogin`, `rolreplication`, `rolbypassrls` |
| `ConnectionLimit` | `rolconnlimit`, -1 none |
| `ValidUntil` | `rolvaliduntil` scanned as `pgtype.Timestamptz`: NULL and `infinity` → zero `Time`, `-infinity` → `ValidUntilNegativeInfinity` (4714-11-24 BC); `PasswordExpired(now)` |
| `Settings` | `rolconfig` (`pg_db_role_setting` with `setdatabase = 0`), `ParameterSetting`s |
| `DatabaseSettings` | `pg_db_role_setting` with `setrole = oid` and a database, unnested `WITH ORDINALITY`, grouped by database name |
| `MemberOf`, `Members` | `pg_auth_members` with `member = oid` / `roleid = oid`, as `RoleMembership{Role, Member, Grantor, Admin, Inherit, Set}`, ordered by the other role's name then the grantor's OID |
| `Comment` | `shobj_description(oid, 'pg_authid')` |

Membership by release: 16 allows the same role granted more than once by
different grantors (each its own `RoleMembership`) and records a
superuser's grant as the bootstrap superuser's. `inherit_option` and
`set_option` are 16+: before, `Inherit` is the **member's** `rolinherit` —
`inheritOption(num, alias)`, a `hasColumnSince` branch, since it is a
different column rather than a rename — and `Set` is `true`. `Grantor` is a
`LEFT JOIN` to `pg_roles`: on 15 a grantor may have been dropped (its name is
then empty); from 16 the server refuses that drop.

`IsPredefined()` is `OID < 16384 && OID != 10` (initdb's roles but the
bootstrap superuser; no `CREATE ROLE` or `RENAME` can take a `pg_` name, and
`pg_upgrade` refuses a `pg_` user role), `IsBootstrapSuperuser()` is
`OID == 10`; both derived, false on a `RoleRef`. An `ObjectFilter` narrows by
name (`r.rolname`) and **`System` selects the predefined roles**
(`predefinedRoleCondition`); `Schema`/`ExtensionMember` narrow nothing.

Tests: `role_test.go` (pgmotest: every field, both directions and the
database grouping; the filter's exact tail and `System false`; the pre-16
substitutes and the 16+ columns; `ValidUntil` for NULL, ±infinity, past and
future; malformed arrays wrapped once; `RoleRef`; the OID boundaries);
fourteen offline mutations, thirteen caught, one equivalent (`infinity` not
mapped explicitly: pgx leaves `Time` zero for an infinite value). Live,
`TestLiveRoles` on pg15–pg18: every attribute, settings in every database
and in a throwaway one (whose own setting must not appear), a NOINHERIT
member, two grants by different grantors (16+) or a dropped grantor (15),
±infinity, case-only names, the bootstrap superuser, `pg_monitor`'s grants,
`System` both ways, and the listing read again as a plain role, identical.
`JOIN` for the grantor's `LEFT JOIN` and a reversed grantor order are caught
live.

### Version sweep (`live_versionsweep_test.go`)

gosmo's `TestLiveVersionSweep`, landed P3.8: call every read pgmo sends
against a real server and report what it rejects. A query naming a column,
view or function the server lacks fails its whole read, and no offline test
notices — the fake answers whatever is asked. Run it on **pg15** after any
query change:

    go test -tags livedb . -run '^TestLiveVersionSweep$' -v -livedb 'service=pg15'

It creates a throwaway database holding one of each object kind the families
read (`sweepSchema`: tables with keys, an index, partitions, a view, a
materialized view, a foreign table, a sequence, enum/composite/domain types, a
collation, overloaded functions, a procedure, an aggregate, a trigger, a
statistics object, a policy, a publication, a rule, an extension) — a read
finding nothing can succeed on a version its query cannot run on — and sweeps
it twice: as the connected role, and as a plain `NOLOGIN` role with no
privileges, each call then its own transaction opened by `SET LOCAL ROLE`, so
one failure does not abort the next. Each pass reports calls made, failures,
and pgmo's own `ErrUnsupportedVersion` refusals apart (the gate working, not
a defect), and logs every label called.

Two halves, as gosmo:

- **Reflective** (`versionsweep_reflect_test.go`, offline): `reflectSweep`
  calls every exported method of a `sweptTypes` receiver taking only `ctx`
  (or `ctx` and a zero `ObjectFilter`) and returning an error, so a new
  listing is swept without anyone adding it. Each call runs under
  `WithScript`: a read still reaches the server, a write is collected and
  fails the sweep as one that wrote — `sweepSkip` (empty today) names the
  context-only writes. A family type joins `sweptTypes` when it lands.
- **By hand** (`live_versionsweep_test.go`): reads taking arguments or not
  exported — `routineByName` over the fixture's overloads, procedure and
  aggregate; `passwordVerifier`; `NewServer`'s version read; and, until a
  family read sends them, the fragments `originColumns` (over `pg_class`,
  `pg_proc`, `pg_type`, `pg_namespace`) and `ObjectFilter.clause` (every
  criterion). Each asserts the fixture's row, not only no error.

Completeness is held offline, so a read cannot drop out of the sweep
silently: `TestSweepCoversEveryRead` finds every function calling a read
helper (`query`, `queryRow`, `queryRowScan`, `scanRows`, `readByName`) and
requires it in `sweepReaders`, mapped to a label in `sweepMustCall`;
`TestSweepReachesEveryContextMethod` requires every exported context-first
method of a swept type to be reflectable, driven by hand (`sweepMustCall`) or
named in `sweepNotReads` with why (`InTransaction`, `OpenSession`); stale
`sweepSkip`/`sweepNotReads` entries fail `TestSweepSkipNamesRealMethods`. Live,
`checkCoverage` fails a `sweepMustCall` entry no label matched. Self-tests:
`TestSweepReadersScan`, `TestReflectSweepRefusesWrites`,
`TestSweepCallUnderARole`, `TestCheckCoverage`. Fourteen mutations caught,
among them a planted `pg_stat_io` read in `Server.Info` (42P01 on pg15, in
both passes) and a planted unswept reader. Result: 17 calls per role, 0
failed, on pg15–pg18.

`Database` is a swept type (P4.1): the scratch database, read by name, is
swept reflectively (`Database.Size`), and `sweepDatabases` drives the
listing narrowed to it, the by-name read of it and of template0, and the
size of template0 (no connections), asserting the fixture's setting and
comment. Result: 23 calls per role, 0 failed, on pg15–pg18.

`Role` is a swept type (P4.2; no context method of its own yet). The fixture
adds a group the plain role is a member of, plain's settings in every
database and in the scratch one, and a comment; `sweepRoles` drives the
listing narrowed to plain, the by-name read of it (asserting each) and of
`pg_monitor` (predefined, three grants). Result: 27 calls per role, 0
failed, on pg15–pg18.

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
`invalidf` (`ErrInvalidRequest`), `unsupportedf` (`ErrUnsupported`),
`notFoundf` (`ErrNotFound`) or `ambiguousf` (`ErrAmbiguous`), never a bare `fmt.Errorf`/`errors.New`: the
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
| `FoldIdent` | typed input → the stored name: trimmed; a whole `"…"` unquoted; else ASCII `A`–`Z` lowered only — `parse_ident` on one part, no truncation | `TestFoldIdent`, `FuzzNames`, `TestLiveFoldIdentMatchesServer` |
| `Spans`, `Span.Unescape` | scan.l: `--` (ends at CR or LF), nested `/* */`, `$N`, `'…'`/`N'…'`, `E'…'`, `U&'…'`, `B''`/`X''` (no escape), `$tag$…$tag$`, `"…"`, `U&"…"`; a prefix or `$` starts a token only as a word of its own | `TestSpans`, `FuzzSpans` (every span re-scans alone as itself), `TestLiveSpanUnescapeMatchesServer` |
| `CommentSafe`, `BlockCommentSafe` | control characters → space; a space splits `*/` and `/*` | `TestCommentSafe`, `TestBlockCommentSafe`, `FuzzNames` |

`keywords.go` is the union of every supported version's non-unreserved
keywords (pg18's list: keywords are only ever added). Regenerate it from
`pg_catalog.pg_get_keywords()` on a new major release; the live test fails
on pg18 (or newer) until it is.

## Security

Identifiers and literals through `pgsql`, one implementation, pinned to the
server's own `quote_ident`/`quote_literal` by live tests. Passwords are never
sent in cleartext in DDL (SCRAM verifiers computed client-side), and are
replaced by a bare placeholder in `WithScript` captures unless the caller
opts in (`WithScriptSecrets`) — § Secrets and passwords.

## Maintaining this document

As gosmo: diagrams live in `diagram/*.mmd`; only `00-map.mmd` is inlined
here. A new family adds its diagram, its feature-map section and its row in
the map in the same change.
