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
(`testdata/version_gates/database.sql`, P4.1), `roleSelect`
(`role.sql`, P4.2), `constraintsQuery` (`constraint.sql`, P4.4) and
`collationSelect` (`collation.sql`, P4.11). The inventory check reads `true`/`false` as the boolean
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
path, that names a different type or none. So pgmo builds the type names
itself (`typeName`/`typeJoins`, shared with the routine families): a
`pg_catalog` type is `format_type`'s name (`double precision`, `"char"`,
`text[]`); any other is `quote_ident(schema).quote_ident(name)`, and an array
of one (`typelem` whose `typarray` is the type, as `format_type` tests) the
element's name with `[]`. `proargtypes` has no typmods, so nothing is lost.
The type is joined, never looked up by a scalar subquery per type (§
Routines: the planner's estimate of the subquery form crosses
`jit_above_cost`).

`Signature()` is accepted after `ROUTINE` (`DROP`/`COMMENT ON`/`ALTER`/`GRANT
… ON ROUTINE`) for every kind, and after the kind's own keyword for every
routine but a zero-argument aggregate, which `AGGREGATE` writes `ag(*)`
(`ag()` is a syntax error there). An ordered-set aggregate's flat list
(`os(double precision, anyelement)`) is accepted by `AGGREGATE` and `ROUTINE`
alike. Checked on pg15–pg18.

`routineByName(ctx, q, schema, name, args)` — exported as
`Database.RoutineByName`, the lookup of any kind — reads the identity alone;
the families' by-name reads share its overload rule (`pickOverload`): an empty schema is `ErrSchemaRequired` before anything is
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
  '<verifier>'`) is `Role.SetPassword`; `Server.CreateRole` builds its
  `PASSWORD` clause through the same `passwordClause` (§ Role writes).

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

### Database writes (`database_write.go`)

Landed P4.13.1, the first family of writes. Every statement goes through
`s.exec` — the Server's own database: a database is a cluster-wide object,
written from any session but its own.

| Method | Statement | Notes |
|---|---|---|
| `Server.CreateDatabase(ctx, CreateDatabaseRequest)` | `CREATE DATABASE n WITH OWNER = … TEMPLATE = … ENCODING = '…' STRATEGY = … LOCALE_PROVIDER = … LOCALE = '…' LC_COLLATE = '…' LC_CTYPE = '…' ICU_LOCALE = '…' ICU_RULES = '…' BUILTIN_LOCALE = '…' TABLESPACE = … ALLOW_CONNECTIONS = … CONNECTION LIMIT = … IS_TEMPLATE = …` | only the fields set, in that order; read back by `DatabaseByName` (`createdObject`) |
| `Database.Drop(ctx, DropDatabaseOptions)` | `DROP DATABASE n [WITH (FORCE)]` | `Force` opt-in, never a default |
| `Database.Rename(ctx, newName)` | `ALTER DATABASE n RENAME TO m` | mirrors `Name` |
| `Database.SetOwner(ctx, role)` | `ALTER DATABASE n OWNER TO r` | mirrors `Owner` |
| `Database.SetTablespace(ctx, ts)` | `ALTER DATABASE n SET TABLESPACE t` | mirrors `Tablespace` |
| `Database.Alter(ctx, DatabaseChanges)` | `ALTER DATABASE n WITH ALLOW_CONNECTIONS = … CONNECTION LIMIT = … IS_TEMPLATE = …` | the fields set; empty sends nothing; mirrors each |
| `Database.SetParameter(ctx, name, values...)` | `ALTER DATABASE n SET name = 'v1'[, 'v2'…]` | not mirrored |
| `Database.ResetParameter(ctx, name)` | `ALTER DATABASE n RESET name` | not mirrored |
| `Database.ResetAllParameters(ctx)` | `ALTER DATABASE n RESET ALL` | mirrors `Settings = nil` |

- **Names** (database, owner, template, tablespace, parameter) go through
  `QuoteIdent`; locale, encoding and parameter values through
  `QuoteLiteral`. A custom parameter's dotted name is quoted whole
  (`"myext.opt"`), as pg_dump writes it: one identifier holding the dot is
  the parameter's name. The server folds a built-in parameter's name itself,
  so `"DateStyle"` and `"WORK_MEM"` reach the right one.
- **Fields are options, not derived.** `Locale`, `Collate`, `CType`,
  `ICULocale`, `BuiltinLocale` are the five options as the grammar has them;
  pgmo does not fill one from another (`LOCALE` sets the ICU locale itself
  from 16, the builtin one from 17).
- **Gates** (checked on the lab): `ICU_RULES` 16+, the builtin provider and
  `BUILTIN_LOCALE` 17+ — `ErrUnsupportedVersion` before anything is sent.
  `CREATE DATABASE` cannot be probed in a transaction, so the inventory's
  statement gates stand on `CREATE COLLATION`'s forms of the same features
  ("ICU tailoring rules", "builtin collation provider").
- **Refusals before sending**: `CREATE DATABASE`, `DROP DATABASE` and `SET
  TABLESPACE` under `InTransaction` (`refuseInTx`, `ErrUnsupported`; the
  others run on the transaction); `DROP`, `RENAME` and `SET TABLESPACE` of
  the Server's own database (`ErrInvalidRequest` — the server's 55006/0A000
  would only come after the script was written); a connection limit below
  -1, an unknown strategy or provider, `LocaleProviderDefault` (a
  collation's alone), an empty name or value.
- **Pgmo's own sessions are released first.** `DROP`, `RENAME` and `SET
  TABLESPACE` need the database free of sessions, and a read through
  `DatabaseQuerier` leaves pgmo's own idle pool connected to it.
  `releaseDatabase` (`database_pools.go`) closes that pool when it is idle
  and drops it from the map, so the next statement there opens afresh; a
  pool in use (a lease, open rows, a transaction) is the caller's session
  and is left — the server then reports it as any other (55006, or `FORCE`
  terminates it). Nothing is closed under `WithScript`. The server waits up
  to five seconds for an exiting backend (`CountOtherDBBackends`), so the
  closed sessions ending a moment later do not fail the statement. Without
  the release, the live rename fails with 55006 (mutation-checked).
- **Parameter values.** `pg_db_role_setting` stores a `GUC_LIST_QUOTE`
  parameter (`search_path`, `temp_tablespaces`, the preload libraries,
  `unix_socket_directories`) as its elements double-quoted where needed —
  `"a b", "$user", ""` — and any other as the text it was given.
  `ParameterSetting.Values()` splits the first kind as pg_dump's
  `SplitGUCList` does and returns the second whole, so
  `SetParameter(ctx, p.Name, p.Values()...)` stores the setting identically
  (held live for each kind). The flag is not exposed in SQL
  (`pg_settings_get_flags` omits it), so the list is pg_dump's
  `variable_is_guc_list_quote`: an extension's list-quoted parameter
  (the lab has `output_plugin_libraries`) reads as one value, pg_dump's
  limit too. `Settings` is not mirrored by `SetParameter`/`ResetParameter`:
  the server stores the canonical name and flattened value; read again.

Tests: `script_database_write_test.go` — the exact statement of every write
and option, through quote-hostile names (`My "DB"`, `user`, `o'brien`,
`a\b`), captured as run in the Server's database with nothing sent (the
first `script_*_write_test.go`; its `runScriptCases` is the harness later
families reuse). `database_write_test.go` — every refusal under both send
and `WithScript`, nothing sent; the 16/17 gates; `InTransaction` refusals
and the ALTERs on the transaction; mirroring only when applied; the
read-back (found, hidden, refused write); the pool release (idle closed and
reopened, in use left, scripted untouched, other writes untouched);
`Values` over every list-quoted parameter and each malformed form. 32
mutations, all caught. `TestLiveDatabaseWrites` (pg15–pg18): an ICU
database with every option (rules on 16+) read back field by field; the
statement captured under `WithScript` run by hand makes the same database
(builtin provider on 17+); rename and set tablespace (in-place) right after a
read through `DatabaseQuerier`; owner, connection options, template refused
by drop; five parameters — a hostile `search_path`, `temp_tablespaces`,
`work_mem`, `DateStyle`, a custom one — stored and read back through
`Values`, reset all, written back identically, one reset by a differently
cased name; drop refused with a session connected (55006), forced past it,
then 3D000.

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

### Role writes (`role_write.go`)

Landed P4.13.2. A role is cluster-wide, so every statement goes through
`s.exec` (or `s.execSecret` when it carries a password) on the Server's own
database, and every one may run inside `InTransaction` — role DDL is
transactional, and a rolled-back create leaves nothing (held live).

| Method | Statement | Notes |
|---|---|---|
| `Server.CreateRole(ctx, CreateRoleRequest)` | `CREATE ROLE n WITH [NO]SUPERUSER [NO]INHERIT [NO]CREATEROLE [NO]CREATEDB [NO]LOGIN [NO]REPLICATION [NO]BYPASSRLS CONNECTION LIMIT n PASSWORD '<verifier>' VALID UNTIL '…' IN ROLE … ROLE … ADMIN …` | only the fields set, pg_dumpall's order; `Password` via `passwordClause`/`execSecret`; read back by `RoleByName` (`createdObject`) |
| `Role.Drop(ctx)` | `DROP ROLE n` | |
| `Role.Rename(ctx, newName)` | `ALTER ROLE n RENAME TO m` | mirrors `Name` |
| `Role.Alter(ctx, RoleAttributes)` | `ALTER ROLE n WITH …` | the attributes set; empty sends nothing; mirrors each |
| `Role.SetPassword(ctx, password)` | `ALTER ROLE n PASSWORD '<verifier>'` | `alterRolePassword` (§ Secrets and passwords) |
| `Role.ClearPassword(ctx)` | `ALTER ROLE n PASSWORD NULL` | |
| `Role.SetParameter(ctx, name, values...)` | `ALTER ROLE n SET name = 'v1'[, …]` | `parameterAssignment`, shared with `Database.SetParameter`; not mirrored |
| `Role.SetDatabaseParameter(ctx, db, name, values...)` | `ALTER ROLE n IN DATABASE d SET …` | not mirrored |
| `Role.ResetParameter(ctx, name)` / `ResetDatabaseParameter(ctx, db, name)` | `ALTER ROLE n [IN DATABASE d] RESET name` | not mirrored |
| `Role.ResetAllParameters(ctx)` | `ALTER ROLE n RESET ALL` | mirrors `Settings = nil` |
| `Role.ResetAllDatabaseParameters(ctx, db)` | `ALTER ROLE n IN DATABASE d RESET ALL` | drops `d` from `DatabaseSettings` |
| `Role.AddMember(ctx, member, MembershipOptions)` | `GRANT n TO m [WITH ADMIN … , INHERIT … , SET …] [GRANTED BY g]` | not mirrored (the server picks the grantor) |
| `Role.RemoveMember(ctx, member, RevokeMembershipOptions)` | `REVOKE [ADMIN\|INHERIT\|SET OPTION FOR] n FROM m [GRANTED BY g] [CASCADE]` | not mirrored |

- **`RoleAttributes`** is shared by the create (embedded in
  `CreateRoleRequest`) and `Alter`: `*bool` per attribute, written as `X` or
  `NOX` when set, so a create leaves the server's defaults (only `INHERIT`
  on) and an alter leaves the rest unchanged. Names match `Role`'s fields.
- **`ValidUntil`** is a `*time.Time`: the zero `Time` writes `'infinity'`
  and `ValidUntilNegativeInfinity` `'-infinity'` — the two values the read
  maps them from — so a round trip is exact. Any other time is written in
  UTC with a `Z` and microseconds (finer truncated, and mirrored truncated,
  as stored); years outside 1–9999 are refused. Checked on the lab: `Z`,
  `infinity`, `-infinity` and BC dates are all taken.
- **Membership options by release** (checked on 15, 16, 18): 15's `GRANT`
  knows only `WITH ADMIN OPTION`, and a plain re-grant leaves an existing
  admin option; 16+ takes `WITH ADMIN|INHERIT|SET TRUE|FALSE`, and a re-grant
  by the same grantor updates only the options named. So on 15 `Admin`
  false, `Inherit`, `Set`, and revoking the INHERIT or SET option are
  `ErrUnsupportedVersion` before sending (the inventory's "GRANT role WITH
  INHERIT/SET" gate); `RemoveMember` with `MembershipAdmin` is 15's way to
  clear the admin option. `GRANTED BY` and `CASCADE` exist on 15 too. From 16
  the role named by `GRANTED BY` must itself hold ADMIN — a superuser does
  not (42501).
- **The server's own refusals are left to it** (each held live with its
  SQLSTATE): a predefined role altered or renamed (42939) or dropped
  (2BP01), a `pg_` name (42939), a duplicate (42710), a role holding a
  privilege dropped (2BP01; `REASSIGN OWNED`/`DROP OWNED` per database
  clear it — not written yet), a membership cycle (0LP01), `NOSUPERUSER` on
  the bootstrap superuser (0A000, 16+ only: **15 takes it**). Not tested
  live but checked on the lab: the session or current user renamed (0A000)
  or dropped (55006).
- **Rename and passwords.** An MD5 verifier is salted with the role name,
  so the server clears one on rename; SCRAM ones survive (held live).

Tests: `script_role_write_test.go` — every statement and option, through
quote-hostile names and values, and 15's membership forms.
`role_write_test.go` — every refusal under both send and `WithScript`,
nothing sent or captured; the 16/17/18 membership forms; every write on the
`InTransaction` transaction; mirroring only when applied (and `ValidUntil`
truncated); empty `Alter`; the create's read-back. `secretWrites` gained
`Role.SetPassword` and `CreateRole` with a password.
42 mutations of `role_write.go`, all caught offline.
`TestLiveRoleWrites` (pg15–pg18): a role with every attribute, a nanosecond
expiry, a password and all three membership lists read back (verifier
checked against `pg_authid`); the statement captured under
`WithScript`+`WithScriptSecrets` run by hand makes the same role and
password; three alters, each read back equal to the mirror; rename keeping
the password; set and clear password; three parameters in every database
and in one, through `Values`, reset and written back; every membership
option the release has, the others refused; the server refusals above;
drop refused while a privilege is held, then done, then 42704; a create,
alter and grant rolled back with their transaction.

### Schemas and privileges (`schema.go`, `privileges.go`)

Landed P4.3 (reads; the writes are P4.13.3 and P4.13.4). gosmo's `Schema`
(ID, name, owner) plus what SQL Server keeps elsewhere:
`Database.Schemas(ctx, ObjectFilter)`, `Database.SchemaByName(ctx, name)`
(exact, `ErrNotFound`), `Database.SchemaRef(name)` (lookup-free, `Name`
only), `Schema.Database()`. `pg_namespace` is per database, so the read goes
through **`s.onDatabase(ctx, d.Name)`** — the database's own pool, or the
transaction's when it is that database. One statement, `schemaSelect`, with
the ACL and the default privileges as parallel arrays in two `LEFT JOIN
LATERAL`s, zipped in Go.

| Field | Source |
|---|---|
| `Origin` | `originColumns` over `pg_namespace`: `IsSystem` the schema's own name; `Extension` an extension's schema (`ALTER EXTENSION … ADD SCHEMA`, or created by its script — not `CREATE EXTENSION … SCHEMA s`) |
| `OID`, `Name`, `Owner` | `oid`, `nspname`, `pg_get_userbyid(nspowner)` (`public` is `pg_database_owner`'s) |
| `Privileges` | `aclexplode(COALESCE(nspacl, acldefault('n', nspowner)))` `WITH ORDINALITY`: one `PrivilegeGrant{Grantee, Grantor, Privilege, Grantable}` per privilege, in the server's ACL order; PUBLIC is `Grantee ""` |
| `PrivilegesDefault` | `nspacl IS NULL`: never granted or revoked (a GRANT then REVOKE back to the same value leaves it false) |
| `DefaultPrivileges` | `pg_default_acl` with `defaclnamespace = oid`, by role name, type code, ACL order, grouped per (role, type) |
| `Comment` | `obj_description(oid, 'pg_namespace')` |

`privileges.go` is shared by every later family: `aclLateral(acl, alias)` /
`aclColumns` / `privilegeArrays` read any `aclitem[]` expression;
`defaultPrivilegeLateral`/`defaultPrivilegeArrays` a schema's
`pg_default_acl` entries. pgmo never parses `aclitem` text (its role-name
quoting is its own); `aclexplode` gives OIDs, named through
`pg_get_userbyid`. A family reads `COALESCE(acl, acldefault(kind, owner))`
so `Privileges` are what the server checks, and says apart a never-set ACL.

`Database.DefaultPrivileges(ctx)` reads every entry of the database, the
database-wide ones (`Schema` empty) with them; by role, database-wide first,
schema, type code. A database-wide entry *replaces* the type's hard-wired
default (so `REVOKE ALL … FROM` the owner reads as an entry with no
`Grants`); an IN SCHEMA one adds to it. Keyed on the name, it works on a
`DatabaseRef`. `DefaultPrivilegeObjectType` is the catalog code; `String()`
is the ON clause (`TABLES` … `LARGE OBJECTS`, 18+). All orders are the
catalog's own types (`name`, `"char"`), so byte order whatever the
database's collation.

An `ObjectFilter`'s `Name` and `Schema` both match the schema's name,
`System` the system-schema rule, `ExtensionMember` the schema's own
membership.

Tests: `schema_test.go` (pgmotest: every field, routed to the schema's
database, the name bound; the filter's exact tail and `ExtensionMember`;
not found, a server error and each short array wrapped once; `SchemaRef`;
`DefaultPrivileges` with an emptied entry and an unknown code; `String`).
Sixteen mutations, all caught (the `PUBLIC` mapping live only). Live,
`TestLiveSchemas` on pg15–pg18: grants with and without GRANT OPTION and to
PUBLIC, IN SCHEMA entries by two roles, database-wide entries including an
emptied one, case-only names, a never-set and a reset ACL, an adopted
extension schema, `public`, `System` and `ExtensionMember`, and the listing
read again as a plain role, identical. The sweep reads `app` (ACL, IN
SCHEMA entry, comment), `public` and the database's entries.

### Tables (`table.go`, `table_column.go`, `table_constraint.go`, `index.go`, `trigger.go`, `policy.go`)

Landed P4.4 (reads; the writes are P4.13.3). gosmo's lean `Table` plus
reads of its parts: `Database.Tables(ctx, ObjectFilter)`,
`Database.TableByName(ctx, schema, name)` (exact; an empty schema is
`ErrSchemaRequired`, never resolved through `search_path`; a view or
sequence of that name is `ErrNotFound`), `Database.TableRef(schema, name)`
(lookup-free, names only), `Table.Database()`, `QualifiedName()`. A table is
a `pg_class` row of kind `r` or `p` — partitions included — read through
**`s.onDatabase`**, one statement with the ACL as an `aclLateral`.

| Field | Source |
|---|---|
| `Origin` | `originColumns` over `pg_class`: temporary tables (`pg_temp_N`) are `IsSystem` |
| `OID`, `Schema`, `Name`, `Owner` | `oid`, `nspname`, `relname`, `pg_get_userbyid(relowner)` |
| `Kind` | `relkind` as `RelationKind` (every code, `String()` as psql names it; reused by P4.5/P4.12) |
| `Persistence` | `relpersistence`: permanent, `UNLOGGED`, `TEMPORARY` |
| `IsPartition`, `PartitionKey`, `PartitionBound` | `relispartition`, `pg_get_partkeydef(oid)`, `pg_get_expr(relpartbound, oid)` (`"DEFAULT"`: `IsDefaultPartition()`) |
| `Parents` | `pg_inherits` by `inhseqno`, as `[]ObjectName{Schema, Name}` |
| `Tablespace` | `spcname` of `reltablespace`, empty for the database's default |
| `Options` | `reloptions`, then the TOAST table's prefixed `toast.`, as `ParameterSetting`s |
| `RowSecurity`, `ForceRowSecurity` | `relrowsecurity`, `relforcerowsecurity` |
| `RowEstimate` | `reltuples::bigint`; -1 never vacuumed or analyzed |
| `Privileges`, `PrivilegesDefault` | `aclexplode(COALESCE(relacl, acldefault('r', relowner)))`, `relacl IS NULL` |
| `Comment` | `obj_description(oid, 'pg_class')` |

The parts are read on demand, one statement each keyed by the OID bound as
`$1`; a `TableRef` (OID 0) is refused with `ErrHandleNotLoaded` before
anything is sent — as gosmo's `requireLoaded`, so an empty list always means
none:

| Read | Catalog | Notes |
|---|---|---|
| `Columns` → `[]*Column` | `pg_attribute` (`attnum > 0`, not dropped) + `pg_attrdef`, `pg_type`, `pg_collation`, `attacl` | `Type` is `format_type(atttypid, atttypmod)` for a `pg_catalog` type and schema-qualified otherwise (the element's schema for an array), whatever the path; `Default` vs `GenerationExpression` split on `attgenerated` (`v`, virtual, 18+); `Collation` only when not the type's own (psql's rule); `StatisticsTarget` `COALESCE(attstattarget, -1)` — the column is `int4` on 15–16 and a nullable `int2` from 17, the same read on both; `IsLocal`/`InheritCount`; the column ACL with no default (NULL is none) |
| `Constraints` → `[]*Constraint` | `pg_constraint` (`contype <> 't'`) | `pg_get_constraintdef`; `conkey`/`confkey` named in key order; `Enforced` is `conenforced` from 18 and `true` before (`colSince`, golden `constraint.sql`); 18's NOT NULL rows (`contype 'n'`) are listed; `Index` for `p`/`u`/`x`; FK actions and match type as `ForeignKeyAction`/`ForeignKeyMatch`, empty for other kinds |
| `Indexes` → `[]*Index` | `pg_index` + `pg_class`, `pg_am`, `pg_constraint`, `pg_tablespace` | `pg_get_indexdef` (table always qualified); `KeyColumns`/`IncludedColumns` per column from `pg_get_indexdef(oid, k, true)` over `indnkeyatts`/`indnatts`; `Predicate`; `Valid`; `NullsNotDistinct` (15); `Constraint` it enforces |
| `Triggers` → `[]*Trigger` | `pg_trigger` (`NOT tgisinternal`) + `pg_proc` | the trigger family's read keyed by the table's OID (§ Triggers and event triggers) |
| `Policies` → `[]*Policy` | `pg_policy` | `Command`, `Permissive`, `Roles` in stored order (PUBLIC `""`), `Using`/`WithCheck` |
| `Children` → `[]ChildTable` | `pg_inherits` (`inhparent = oid`) | direct only; `Kind` may be a foreign table; `PartitionBound`, `DetachPending` |
| `Size` → `TableSize` | `pg_table_size`, `pg_indexes_size`, `pg_total_relation_size` | on demand (stats files); any role may call them; 0 for a partitioned table; a dropped table is `ErrNotFound` |

**Rendered text follows the session's `search_path`.** `pg_get_expr`,
`pg_get_constraintdef`, `pg_get_triggerdef` and the expressions inside
`pg_get_indexdef` leave a name visible on the path unqualified
(`nextval('s'::regclass)` vs `nextval('app.s'::regclass)` — checked on pg15
and pg18), as psql's `\d` shows. pgmo does not set the caller's path; the
structured fields (`Column.Type`, `Collation`, `Constraint.ReferencedTable`,
`Trigger.Function`, `Parents`) are qualified whatever it is. The P5
scripter must render under a fixed path (`OPEN-THREADS.md` § Tables).

An `ObjectFilter`'s `Name` matches `relname`, `Schema` `nspname`, `System`
the system-schema rule, `ExtensionMember` the table's own membership. Order
is schema then name, byte order (`name` columns).

Tests: `table_test.go` (pgmotest: every field of each type from its own
column, with a field-count guard per type; routed to the table's database,
names bound as `$1`/`$2`, parts keyed by the OID; the filter's exact tail;
`ErrSchemaRequired` and every part's `ErrHandleNotLoaded` before anything
is sent; not found, server errors and short arrays wrapped once;
`conenforced` by version; `tgtype` decoding; each code's `String`).
Thirty mutations: twenty-one caught offline, seven only live (the collation and type-qualification rules, the partial-index predicate's pretty form, `tgparentid`, the policy role order, `inhparent`, `pg_total_relation_size`), and the parents' order once the fixture's two parents were in different schemas; one equivalent — joining an array's element type for its schema — and that join was removed, since an array type is always in its element's schema. Live, `TestLiveTables` on pg15–pg18: identity, an
extension's type, defaults, stored (and on 18 virtual) generated columns,
an enum array, `COLLATE "C"` with `EXTERNAL` storage and a statistics
target, a column grant and comment, a sequence default, a domain; a
`NOT VALID` check (and on 18 a `NOT ENFORCED` one and the NOT NULL rows); a
deferrable `MATCH FULL` foreign key with actions, an exclusion constraint,
storage parameters on a table and its TOAST table, a partial expression
index with `INCLUDE` and a fillfactor, an `ENABLE REPLICA` trigger on
`UPDATE OF` with `WHEN`, a deferrable constraint trigger (and no foreign-key
trigger), forced RLS with a restrictive and a permissive policy; range
partitions with a DEFAULT, a partitioned index and an inherited trigger;
two-parent inheritance with an inherited check; an unlogged extension
member; case-only names; a temporary table; every `TableRef` read refused;
and the listing with every part read again as a plain role, identical.

### Views and materialized views (`view.go`)

Landed P4.5 (reads; the writes are P4.13.3). Two types, as CREATE VIEW and
CREATE MATERIALIZED VIEW are two statements and pgMS shows two folders:
`Database.Views` / `ViewByName` / `ViewRef` (`pg_class` kind `v`) and
`Database.MaterializedViews` / `MaterializedViewByName` /
`MaterializedViewRef` (kind `m`), each with `Database()` and
`QualifiedName()`. Through **`s.onDatabase`**, one statement each, the ACL
an `aclLateral` (`acldefault('r', …)`: a view takes a table's privileges).
Each by-name read finds its own kind alone — a table, view or materialized
view of the name is `ErrNotFound` to the others — and an empty schema is
`ErrSchemaRequired`. The listings take `relationFilterColumns`, the
tables' filter (renamed from `tableFilterColumns`).

| Field | View | MaterializedView | Source |
|---|---|---|---|
| `Origin`, `OID`, `Schema`, `Name`, `Owner` | ✓ | ✓ | as `Table`; `information_schema`'s views are `IsSystem` |
| `Definition` | ✓ | ✓ | `pg_get_viewdef(oid)`, non-pretty as `pg_dump`: leading space to closing `;` |
| `CheckOption` | ✓ | | `check_option` from `pg_options_to_table(reloptions)`: `""`, `local`, `cascaded` (`ViewCheckOption`, `String()` NONE/LOCAL/CASCADED) |
| `SecurityBarrier`, `SecurityInvoker` | ✓ | | the parameter cast `::boolean` server-side, so `on`, `1`, `true` agree; `false` when unset |
| `Options` | ✓ | ✓ | `reloptions` as stored (a view's includes the three above); a materialized view's then its TOAST table's, `toast.`-prefixed (`relationOptions`) |
| `Populated` | | ✓ | `relispopulated`: false after `WITH NO DATA` |
| `Tablespace`, `RowEstimate` | | ✓ | as `Table` |
| `Privileges`, `PrivilegesDefault`, `Comment` | ✓ | ✓ | as `Table` |

Parts, on demand and keyed by the OID with the tables' own queries — a Ref
is `ErrHandleNotLoaded` before anything is sent: `View.Columns` and
`MaterializedView.Columns` (`columnsQuery`; a view's column may carry a
`DEFAULT`, grants and a comment, its `Storage` is its type's and means
nothing), `MaterializedView.Indexes` (`indexesQuery`) and
`MaterializedView.Size` (`tableSizeQuery`, a `TableSize`).

**The definition follows the session's `search_path`** as every rendered
expression (§ Tables): `FROM app.t` reads `FROM t` once `app` is on the
path — checked on pg15 and pg18, and pinned by the live test.

Tests: `view_test.go` (pgmotest: every field of both types from its own
column with a field-count guard; names bound as `$1`/`$2`, routed to the
view's database; each option's exact fragment; both filters' exact tail;
`ErrSchemaRequired`, not found, server errors and a short ACL wrapped once;
every Ref part refused before sending; parts keyed by the OID; a dropped
materialized view's size `ErrNotFound`; `ViewCheckOption.String`). Live,
`TestLiveViews` on pg15–pg18: both security parameters written as `on`
and `1` with a cascaded check option, a local check option over a view, a
column default, grants to PUBLIC and on a column, comments, a case-only
name, an extension member with an extension's type, `information_schema`
as system; a materialized view `WITH NO DATA` with its own and its TOAST
table's parameters, and a populated, analyzed one with a unique index; the
definition under two paths; each by-name read refusing the other kinds;
every Ref refused; and both listings with their parts read again as a
plain role, identical.

### Sequences (`sequence.go`)

Landed P4.7 (reads; the writes are P4.13.3). `Database.Sequences` /
`SequenceByName` / `SequenceRef` (`pg_class` kind `S` joined to
`pg_sequence`), with `Database()`, `QualifiedName()` and the derived
`IsOwned()`. What the `pg_sequences` view shows, read from the catalogs
beneath it, plus what it leaves out. Through **`s.onDatabase`**, one
statement; the extension, the owning column and the comment are joins
(`originJoin`, `pg_description`), not a subquery per row — the estimate
over 3,000 sequences is ~1.2 k, 0.15 s on pg15 and pg18. The ACL is an
`aclLateral` over `acldefault('s', …)`. A by-name read finds a sequence
alone (a table or view of the name is `ErrNotFound`); an empty schema is
`ErrSchemaRequired`. The listing takes `relationFilterColumns`; every
session's temporary sequences are `IsSystem` (`pg_temp_N`), and another
session's are listed, as `Tables` lists its temporary tables.

| Field | Source |
|---|---|
| `Origin`, `OID`, `Schema`, `Name`, `Owner` | as `Table` |
| `Persistence` | `relpersistence` (`Persistence`, shared with `Table`) |
| `DataType` | `format_type(seqtypid, NULL)`: `smallint`, `integer`, `bigint` — always `pg_catalog`'s |
| `Start`, `Increment`, `MinValue`, `MaxValue`, `Cache`, `Cycle` | `pg_sequence`; the bounds are stored even for `NO MINVALUE`/`NO MAXVALUE`; every value fits an `int64` (no decimal sequences, unlike SQL Server) |
| `OwnedBy`, `OwnedByColumn`, `Identity` | the sequence's one `pg_depend` row on a `pg_class` column, deptype `'a'` (OWNED BY, serial) or `'i'` (identity); checked on pg15/pg18 that a sequence has no other dependency on a relation |
| `Privileges`, `PrivilegesDefault`, `Comment` | as `Table`, the default `acldefault('s')` (the owner's SELECT, UPDATE, USAGE) |

**The last value is read on demand, not listed**: `Sequence.LastValue(ctx)
(value, called, err)`, keyed by the OID (a Ref is `ErrHandleNotLoaded`, a
dropped sequence `ErrNotFound`). `pg_sequence_last_value` opens the
sequence and answers differently by version — found on the lab: without
SELECT or USAGE, pg15 raises 42501 where pg18 answers NULL; for another
session's temporary sequence both answer NULL; NULL also means "not called
since CREATE / RESTART / `setval(…, false)`". So `pg_sequences.last_value`
cannot tell the cases apart, and a listing carrying it would differ by role.
The read asks the three questions first (`has_sequence_privilege(…,
'SELECT,USAGE')`, `pg_is_other_temp_schema`, unlogged in recovery) and
calls the function only past them: no privilege →
`unsupportedf(…, ErrDefinitionHidden)`; another session's temporary, or an
unlogged sequence on a standby → `ErrUnsupported`; NULL → `called` false.
The value is the one stored in the sequence, which a session's `CACHE`
runs ahead of: one `nextval` under `CACHE 5` stores the fifth value.

Tests: `sequence_test.go` (pgmotest: every field from its own column with a
field-count guard; names bound as `$1`/`$2`, routed to the sequence's
database; each join's exact fragment; the filter's exact tail;
`ErrSchemaRequired`, not found, server errors and a short ACL wrapped once;
the Ref refused before sending; `LastValue` keyed by the OID with each
guard's fragment, and each case of its row: called, never called, no
privilege, another session's temporary, unlogged on a standby, dropped, a
server error). Live, `TestLiveSequences` on pg15–pg18: defaults, a
descending smallint `CYCLE` with bounds and `CACHE`, a serial, an identity
and an OWNED BY column (checked against `pg_get_serial_sequence`), unlogged,
a case-only name, an extension member, another session's temporary
sequence; grants WITH GRANT OPTION and to PUBLIC and a never-set ACL; every
listed sequence equal to `pg_sequences`' row; the last value through
`nextval`, `setval` both ways and `RESTART`; each refusal; and the listing
read again as a plain role, identical, its last values following its
privileges. 14 mutations, all caught offline; live, 11 — of the other
three, the unlogged-on-a-standby guard needs a standby the lab lacks, and
the SQL's other-temp guard is equivalent on 15–18 (the function answers
NULL there and the Go check refuses it).

### Types and domains (`type.go`, `domain.go`)

Landed P4.8 (reads; the writes are P4.13.3). Two types, as two statements
and two OE folders (Types, Domains), both from `pg_type` through
**`s.onDatabase`**, sharing `typeFilterColumns` (`t.typname`, `n.nspname`,
the system schema, `isExtensionMember(catalogType, …)`). A by-name read
finds its own family alone; an empty schema is `ErrSchemaRequired`; a Ref
holds the names.

**`Type`** (`Database.Types` / `TypeByName` / `TypeRef`) is every kind but
a domain: `Kind` (`TypeKind`: base, composite, enum, pseudo, range,
multirange). Left out, as psql's `\dT` does, because each is part of
another object: the array the server makes beside every type (`_name`, its
element's `typarray`, tested on the `te` join `typeJoins` already makes for
`Element`) and a relation's row type (`typrelid` of a relkind other than
`c`). A multirange is listed — a column can be declared with it and CREATE
TYPE … AS RANGE names it — though it is created and dropped with its range.
Checked on the lab: every `pg_type` row is a listed Type, a domain, an
implicit array or a row type (`TestLiveTypes` counts them).

| Field | Source |
|---|---|
| `Origin`, `OID`, `Schema`, `Name`, `Owner` | as `Table`, the origin over `catalogType` |
| `Kind`, `Category`, `Preferred`, `Shell` | `typtype`, `typcategory` (`TypeCategory`, the documentation's names), `typispreferred`, `NOT typisdefined` (a shell type, `CREATE TYPE name;`, stored as a pseudo-type) |
| `Input` … `Subscript` | the eight `regproc` columns as `ObjectName`s (`procedureJoin`); every type has an input and output function, CREATE TYPE names them for a base type alone |
| `InternalLength`, `PassedByValue`, `Alignment`, `Storage`, `Delimiter` | `typlen` (-1 varlena, -2 cstring), `typbyval`, `typalign` (`TypeAlignment`), `typstorage` (`StorageKind`, shared with `Column`), `typdelim` |
| `Element` | `typelem` by `typeName` — a base type's ELEMENT (point's `double precision`); never an implicit array's, those are not listed |
| `Default`, `HasDefault`, `Collatable` | `typdefault` (an empty literal is a default), `typcollation <> 0` |
| `Labels` | `pg_enum` by `enumsortorder` — not creation order once `ADD VALUE BEFORE/AFTER` ran |
| `Attributes` | the composite relation's live `pg_attribute` rows: name, type OID, `typeNameWithModifier`, a collation only when not the type's own |
| `Subtype`, `SubtypeOperatorClass`, `Collation`, `Canonical`, `SubtypeDiff`, `Multirange` | `pg_range` on `rngtypid`; the operator class is stored even when defaulted |
| `Range` | `pg_range` on `rngmultitypid`: a multirange's range |
| `Privileges`, `PrivilegesDefault`, `Comment` | `typacl` over `acldefault('T')` (PUBLIC's USAGE and the owner's); `pg_description` |

Each kind's children are one grouped read joined by OID — `typeEnumLabels`
over `pg_enum`, `typeAttributes` over the composite relations' attributes —
not a subquery per type (§ Routines). The listing of pg_catalog's 107 types
is estimated at ~1.3 k (far under `jit_above_cost`), ~40 ms to run and ~65
ms to plan its ~50 joined relations on pg15 and pg18.

**`Domain`** (`Database.Domains` / `DomainByName` / `DomainRef`), `typtype
'd'`: `BaseTypeOID`/`BaseType` (`format_type(typbasetype, typtypmod)`
qualified by `typeNameWithModifier` — the shared form of `columnTypeName`,
so `varchar(10)` keeps its modifier and a user type its schema whatever the
path); `NotNull` (`typnotnull`); `Default` (`pg_get_expr(typdefaultbin,
0)`, as pg_dump, so a renamed object is named as it is now — rendered under
the session's `search_path`, like every expression); `Collation` only when
it differs from the base type's (a domain over a domain inherits it);
`Constraints []DomainConstraint{Name, Kind, Definition, Validated}` from
`pg_constraint` on `contypid`, one grouped read, by name; ACL and comment as
`Type`. **Found on the lab: from 17 a domain's NOT NULL is also a named
`pg_constraint` row** (contype `'n'`, `d_not_null` unless `CONSTRAINT name
NOT NULL` named it — the name given is lost on 15 and 16), so
`Constraints` holds it there beside the CHECKs while `NotNull` reads alike on
every release. A domain over a domain has no constraints of its own; the
base's apply. No gate: the catalogs' columns are identical on 15–18.

Tests: `type_test.go`, `domain_test.go` (pgmotest: every field from its own
column with a field-count guard; names bound as `$1`/`$2`, routed to the
type's database; each join's and grouped read's exact fragment; the
filter's exact tail; `ErrSchemaRequired`, not found, server errors, short
attribute / constraint arrays and ACLs wrapped once; the Refs; every
`String()`). Live, `TestLiveTypes` and `TestLiveDomains` on pg15–pg18 —
enums in sort order, empty enum and composite, a dropped attribute,
attribute collations and modifiers, ranges with a diff function, a
collation and a named multirange, citext's base type, pg_catalog's
`int4range`/`point`/`box`/`varchar`/`text`/`anyelement`; what is no Type
refused; every `pg_type` row accounted for; domains with collation,
default, NOT NULL (the 17+ rows), a NOT VALID CHECK, over a domain, over an
enum array; information_schema's as system; a case-only name; grants; the
listings read again under a `search_path` holding `app` and as a plain
role, identical. Sweep: `Type`/`Domain` swept types, `sweepTypes`.

### Triggers and event triggers (`trigger.go`, `event_trigger.go`)

Landed P4.9 (reads; the writes are P4.13.3). Two families, as SQL Server's
DML and DDL triggers are (gosmo's `Trigger` and `DatabaseTrigger`).

**Triggers** were a table's part from P4.4; P4.9 made them a family of the
database: `Database.Triggers(ctx, ObjectFilter)` (every relation kind that
takes one — table, partitioned table, view, foreign table),
`TriggerByName(ctx, schema, table, name)`, `TriggerRef(schema, table,
name)`, and the per-relation reads `Table.Triggers` and `View.Triggers` —
one select (`triggerSelect`), narrowed by `tgrelid` for a relation. A
trigger has **no schema of its own**: its name is unique on its relation,
and DDL names it `name ON schema.relation`, so it is addressed by three
names and `Trigger.QualifiedTable()` gives the relation. Its `Origin` is
the relation's (a trigger is created, dumped and dropped with it; `ALTER
EXTENSION … ADD` takes no TRIGGER), and the filter's Schema/System/
ExtensionMember are the relation's too; Name is the trigger's. Internal
triggers (`tgisinternal`, a foreign key's) are never read — by name either.

| Field | Source |
|---|---|
| `Origin`, `Schema`, `Table`, `TableKind` | the relation's `pg_class` row (`originJoin`), `relkind` |
| `OID`, `Name`, `Definition` | `pg_get_triggerdef(oid)` (non-pretty: table qualified) |
| `Timing`, `Events`, `ForEachRow` | `tgtype` decoded (server order) |
| `UpdateColumns` | `tgattr` named in the order given (`UPDATE OF b, a` stays `b, a`) |
| `Function` | `tgfoid`; a trigger function takes no arguments, so `FunctionByName(…, []OID{})` finds it |
| `Arguments` | `tgargs`, split server-side (below) |
| `Enabled`, constraint flags, `Inherited` | `tgenabled`, `tgconstraint`/`tgdeferrable`/`tginitdeferred`, `tgparentid <> 0` |
| `OldTable`, `NewTable` | `tgoldtable`, `tgnewtable` (REFERENCING) |
| `HasCondition` | `tgqual IS NOT NULL`; the text is in `Definition` alone — `pg_get_expr(tgqual, tgrelid)` refuses it ("expression contains variables of more than one relation", pg15 and pg18) |
| `Comment` | `pg_description` join |

**Arguments.** `tgargs` is a `bytea` of each argument's bytes in the
database's encoding, each followed by a NUL (`convert_from` refuses it
whole: a NUL is no character). It is split as text: `encode(…, 'escape')`
writes a NUL as `\000`, a backslash as `\\` and a high byte as `\ooo`;
every `\\` is rewritten `\134` (the same byte), so every backslash left
starts a three-digit escape and `\000` is found only where a NUL was; each
piece is `decode`d and `convert_from` the database's encoding, so the
server converts it to the client's as any text. Checked on the lab against
an independent split in Go of the raw bytes, with a quote, an empty
argument, `a\000b`, multibyte text, a LATIN1 database, and
`standard_conforming_strings` off (the literals are `E''`). The first form
split with `generate_series` over the bytes: the planner prices each call
at 15, ~52 k over 3,000 triggers — 6,000 would cross `jit_above_cost`. As
`unnest` the estimate over 6,000 is ~5–6 k (0.5 s through psql on pg15
and pg18); a relation's read pushes `tgrelid` into both grouped
joins (keyed `(oid, tgrelid)`), cost ~100, 6 ms.

**`Definition` follows the session** twice over: its names the
`search_path` (§ Tables), and its argument literals
`standard_conforming_strings` — off, `'a\000b'` is rendered `'a\\000b'`
(found on the lab; pinned by `TestLiveTriggers`). `Arguments` do not.

**Event triggers**: `Database.EventTriggers(ctx, ObjectFilter)`,
`EventTriggerByName(ctx, name)`, `EventTriggerRef(name)`, from
`pg_event_trigger` through the database's own pool (one statement;
extension and comment are joins). An event trigger has no schema
(`QualifiedName()` is the quoted name; a Schema criterion is ignored);
`pg_event_trigger` is readable by every role, so the listing is the same
for all. Fields: `Origin` (`IsSystem` = `oid < 16384`, the rule for a
schema-less object — no built-in row exists; `Extension` by `pg_depend` on
the new `catalogEventTrigger`, set by `ALTER EXTENSION … ADD EVENT
TRIGGER`), `OID`, `Name`, `Owner`, `Event` (`EventTriggerEvent`:
`ddl_command_start`, `ddl_command_end`, `sql_drop`, `table_rewrite`,
`login` — 17+, refused by 15–16's parser, a value not a column so no
gate), `Function`, `Enabled` (`TriggerEnabled`, shared), `Tags` (`evttags`
as stored — upper case — nil for every command), `Comment`. Checked on the
lab first: `pg_trigger` and `pg_event_trigger` identical on pg15–pg18.

Tests: `trigger_test.go` (pgmotest: every field of three triggers — on a
system-schema extension member's table, a view, a foreign table — with a
field-count guard; each join's exact fragment; the per-table and per-view
reads keyed by the OID; the listing's filter tail and order; by-name names
bound as `$1`–`$3`, `ErrSchemaRequired`, not found, server errors wrapped
once; the Ref), `event_trigger_test.go` (likewise; a Schema criterion
pushes nothing). Live, `TestLiveTriggers` on pg15–pg18: UPDATE OF out of
table order with WHEN and REPLICA, arguments as above, transition tables,
DISABLE, a comment, a case-only name, a foreign key's triggers left out
(by name too), a constraint trigger, a view's INSTEAD OF and statement
triggers, a partition's inherited one, a foreign table's, an extension
member table's; every non-internal `pg_trigger` row listed once; filters;
the listing again under `standard_conforming_strings` off and as a plain
role; a LATIN1 database. `TestLiveEventTriggers`: every event (login on
17+), tags, ENABLE ALWAYS/DISABLE, an extension member, a case-only name,
matched to `pg_event_trigger` row for row, identical as a plain role, none
seen from another database. 25 mutations, all caught (4 live only; UPDATE
OF's order after the fixture put its columns out of table order).

### Extensions (`extension.go`)

Landed P4.10 (reads; the writes — CREATE, ALTER … UPDATE / SET SCHEMA,
DROP — are P4.13.3). SQL Server has no counterpart. Three reads, all
through the database's own pool and the same for every role
(`pg_extension` is readable by all; `pg_available_extensions()` and
`pg_available_extension_versions()` are executable by PUBLIC — checked on
pg15 and pg18):

- **Installed**: `Database.Extensions(ctx, ObjectFilter)`,
  `ExtensionByName(ctx, name)`, `ExtensionRef(name)`. An extension has no
  schema of its own (its name is the database's; `QualifiedName()` is the
  quoted name), but `Schema` is where its objects are, and the filter's
  Schema criterion matches it. `Origin.IsSystem` is the schema-less rule,
  `oid < 16384` — plpgsql alone, which initdb installs; `Origin.Extension`
  is always empty (`ALTER EXTENSION … ADD` takes no extension), so
  `ExtensionMember: true` keeps none (the filter column is `false`).
- **Members**: `Extension.Members(ctx)` → `[]ExtensionMember`, keyed by the
  OID (a Ref → `ErrHandleNotLoaded`), by type then identity.
- **Available**: `Database.AvailableExtensions(ctx, ObjectFilter)` (Name
  only) and `AvailableExtensionByName(ctx, name)`, one row per version of
  the two views joined, grouped in Go. They read the server's extension
  directory, so they are the same in every database but for
  `InstalledVersion` / `Installed`, which are the database read's — hence
  a Database method, not a Server one.

| Field | Source |
|---|---|
| `Origin`, `OID`, `Name`, `Owner` | `oid < 16384`; `extname`; `pg_get_userbyid(extowner)` |
| `Schema`, `Version`, `Relocatable` | `extnamespace`, `extversion`, `extrelocatable` |
| `DefaultVersion` | `pg_available_extensions()` joined by name (the function, not its view, which joins `pg_extension` again); empty when the control file is gone |
| `Requires` | `pg_depend` deptype `'n'` from the extension to another, grouped by name |
| `ConfigTables` | `extconfig` × `extcondition` by `ROWS FROM (unnest(…), unnest(…)) WITH ORDINALITY`, in marking order (pg_dump's); a sequence has no condition |
| `Comment` | `pg_description` join |
| `ExtensionMember` | `pg_depend` deptype `'e'` to the extension: `Catalog` (`classid`'s relname), `OID` (`objid`), `Type` and `Identity` from `pg_identify_object` |
| `AvailableExtension` | `pg_available_extensions`: `name`, `default_version`, `installed_version`, `comment` |
| `AvailableExtensionVersion` | `pg_available_extension_versions`: every column but the name |

**Members are named, not read.** An extension can own any kind of object
— casts, operator classes, languages, schemas, foreign data wrappers —
most of which pgmo has no family for yet, so a member is its catalog and
OID (the key a family's read takes) and `pg_identify_object`'s identity:
schema-qualified and quoted whatever the `search_path`, routines and
operators with their argument types, `(source AS target)` for a cast,
`… USING method` for an operator class — what `ALTER EXTENSION … DROP
<type>` takes. Its `schema` and `name` columns are **not** read: they come
back quoted (`"E s"`), so they are no stored names (found on pg15 and
pg18); `pg_identify_object_as_address`'s `object_names` quote a type's
name and nothing else's. **From 17 an extension type's implicit array is a
member too** (`"E s".earth[]`; 15–16 record no array — found on the lab,
pinned by `TestLiveExtensions`).

**Versions are opaque to PostgreSQL** — names related only by the update
scripts there are — and `pg_available_extension_versions()` returns them
in directory order (pg18 gave citext's as 1.8, 1.4, 1.6, 1.7, 1.5). An
`AvailableExtension`'s versions are sorted in Go by
`compareExtensionVersions`, a reading aid: runs of digits compared as
numbers (1.9 before 1.10), other runs as bytes, a number before text at
the same place, a prefix first (1.4 before 1.4-1); total, equal only for
equal strings.

`ConfigTables` cannot be set by a fixture — `pg_extension_config_dump`
runs only inside an extension's script — so `TestLiveExtensions` checks
them against the connected database's `pg_cron` when it has one (the lab's
does: `cron.job`, `cron.jobid_seq`, `cron.job_run_details`,
`cron.runid_seq`). The listing's cost is ~51 on pg15 and pg18 (an
extension list is short; the configuration grouping is the largest part).

Tests: `extension_test.go` (pgmotest: every field of two extensions — one
with every array NULL — with field-count guards; each join's exact
fragment; the filter tail, `ExtensionMember: true` as `AND false`;
misaligned configuration arrays refused; by-name bound as `$1`, not found,
server errors wrapped once; the Ref; members keyed by the OID with a Ref
refused before sending; the available rows grouped, versions sorted, an
extension with no version, the Name criterion alone pushed down;
`compareExtensionVersions` over every pair of an ordered list). Live,
`TestLiveExtensions` on pg15–pg18: plpgsql, cube and earthdistance (which
requires it) in `"E s"`, citext at 1.5 below the default, commented,
holding a table and a function `ALTER EXTENSION … ADD` gave it; the
listing matched to `pg_extension` and each default to
`pg_available_extensions`; filters; members matched to `pg_depend` +
`pg_identify_object`, identical under a `search_path` holding `"E s"`;
the available listing matched to both views; a plain role identical;
another database with plpgsql alone and citext not installed; a dropped
extension with no members; `pg_cron`'s configuration tables. 30
mutations: 29 caught (all offline; grouping by name only after the
fixture gave two neighbours one comment), 1 equivalent (an empty
`Requires` set to nil: `array_agg` never gives one; removed).

### Tablespaces, collations, text search, languages, casts, operators

Landed P4.11 (reads only: of these, P4.13.3 writes tablespaces alone; the
rest are scripted by P5 from what is read here). Six families, nine types,
each as the earlier families: a listing taking an `ObjectFilter` pushed
into its query, an exact by-name read (`ErrNotFound`), a lookup-free
`Ref`, `Origin` on every type, every row the same for every role (none of
these catalogs hides a row, and none but tablespaces and languages has an
ACL). Each listing joins rather than subquerying per row; the costliest,
pg_catalog's ~800 operators, is planned at ~890 on pg15 and pg18, far
below `jit_above_cost`.

- **Tablespaces** (`tablespace.go`): `Server.Tablespaces`,
  `TablespaceByName`, `TablespaceRef`, and `Tablespace.Size(ctx)` on
  demand (`pg_tablespace_size`, keyed on the name, so it works on a Ref).
  A shared catalog, read through the Server's own connection as databases
  and roles are. `IsSystem` is `oid < 16384` (pg_default, pg_global);
  never an extension member, so `ExtensionMember: true` is `AND false`.
  `Size` is answered to a role holding CREATE on the tablespace or
  `pg_read_all_stats`, and to any role for its session's database's
  default tablespace; otherwise 42501.
- **Collations** (`collation.go`): `Database.Collations`,
  `CollationByName(ctx, schema, name)`, `CollationRef`. The catalog may
  hold one name per encoding (`pg_import_system_collations` imports a
  locale's alias for each encoding it supports); the listing returns them
  all, ordered by encoding, and the by-name read the one the database can
  use — its encoding's before any-encoding's (`collencoding -1`), as the
  server resolves a name. Gated: `colliculocale` → `colllocale` (17),
  `collicurules` (16; golden file `testdata/version_gates/collation.sql`).
  `LocaleProviderDefault` (`'d'`) is the `default` collation's alone.
- **Text search** (`text_search.go`): four families sharing
  `listTextSearch`/`textSearchByName` — `TextSearchParsers`,
  `TextSearchTemplates` (neither has an owner: a superuser's C
  functions), `TextSearchDictionaries`, `TextSearchConfigurations`, each
  with `…ByName(ctx, schema, name)` and `…Ref`. A configuration's
  `Mappings` are read in the same statement: `pg_ts_config_map` aggregated
  per configuration by token type then position, the aliases from
  `ts_token_type` called once per *parser* (a derived table, not a lateral
  per configuration), grouped by token type in Go. A dictionary's
  `Options` is `dictinitoption` verbatim.
- **Languages** (`language.go`): `Database.Languages`, `LanguageByName`,
  `LanguageRef`. Schema-less; `IsSystem` is `oid < 16384` — internal, c,
  sql and plpgsql (the last also an extension member). ACL USAGE, default
  `acldefault('l', owner)`.
- **Casts** (`cast.go`): `Database.Casts`, `CastByTypes(ctx, source,
  target OID)`, `CastRef(source, target string)`, `Cast.Signature()` →
  `(source AS target)`. A cast has no name, schema or owner: the filter's
  Name and Schema criteria are ignored, System is `oid < 16384`.
  `Source`/`Target` are `typeName` (§ Identity): qualified outside
  pg_catalog whatever the `search_path`. **`CREATE TYPE … AS RANGE` makes
  a cast of its own** — range to multirange, explicit, by the multirange's
  constructor, `pg_depend` deptype `'i'` on both types — so every user
  range brings a user cast (found on the lab, pinned by the sweep).
- **Operators** (`operator.go`): `Database.Operators`,
  `OperatorByName(ctx, schema, name, left, right OID)` (left 0 for a
  prefix operator — the catalog's unique key), `OperatorRef(schema, name,
  left, right string)`. `QualifiedName()` quotes the schema and not the
  name (an operator name is no identifier); `Signature()` is `schema.op
  (left, right)`, `NONE` for a prefix operator's left. A shell operator (a
  COMMUTATOR/NEGATOR named before its definition) has no function and
  result type 0.

| Type | Catalog | Notes |
|---|---|---|
| `Tablespace` | `pg_tablespace` + `pg_tablespace_location`, `pg_shdescription` | `Location` empty for pg_default/pg_global, `pg_tblspc/<oid>` for an in-place one; `Options` from `spcoptions`; ACL CREATE, default `acldefault('t', owner)` |
| `Collation` | `pg_collation` | `Encoding` empty for any (`-1`); `Collate`/`CType` libc's, `Locale` ICU's and builtin's |
| `TextSearchParser`, `TextSearchTemplate` | `pg_ts_parser`, `pg_ts_template` | functions as `ObjectName`s, zero for an optional one unset |
| `TextSearchDictionary`, `TextSearchConfiguration` | `pg_ts_dict`, `pg_ts_config` + `pg_ts_config_map` | `Template`, `Parser` as `ObjectName`s |
| `Language` | `pg_language` | handler, inline, validator as `ObjectName`s |
| `Cast` | `pg_cast` | `Context` (`e`/`a`/`i`), `Method` (`f`/`i`/`b`) as the catalog's letters |
| `Operator` | `pg_operator` | operand and result types as OIDs and `typeName` text; commutator and negator as `ObjectName`s |

Tests: one offline file per source file (pgmotest: every field of two
rows with field-count guards; each join's fragment; the filter tail; the
by-name binding; not found and server errors wrapped once; the Ref; for
text search one table over the four families and misaligned mapping
arrays refused). Live on pg15–pg18: `TestLiveTablespaces` (an in-place
tablespace via `allow_in_place_tablespaces`, options, comment, a grant and
a table; the listing matched to `pg_tablespace`; a plain role's listing
identical, `pg_global`'s and the new one's size refused it, the grantee's
answered) and, over one fixture database (`live_catalog_objects_test.go`),
`TestLiveCollations`, `TestLiveTextSearch` (every configuration's mappings
matched to `pg_ts_config_map` × `ts_token_type`), `TestLiveLanguages`,
`TestLiveCasts`, `TestLiveOperators` (every operator's `Signature`
matched to `regoperator`) — each listing matched to its catalog, and
identical as a plain role and under a `search_path` holding the fixture's
schema. 32 mutations, all caught offline.

### Foreign data: wrappers, servers, user mappings, foreign tables (`foreign_data.go`, `foreign_table.go`)

Landed P4.12 (reads; writes are P4.13, scripting P5). SQL/MED objects are
a database's, read through its own pool; wrappers, servers and user
mappings have no schema. Four families, each as the earlier ones: a
listing taking an `ObjectFilter` pushed into its query, an exact by-name
read (`ErrNotFound`), a lookup-free `Ref`, `Origin` on every type.

- **Wrappers** (`Database.ForeignDataWrappers`, `ForeignDataWrapperByName`,
  `ForeignDataWrapperRef`): handler and validator as `ObjectName`s (zero
  for none), `Options`, ACL USAGE (`acldefault('F', owner)`), comment.
  `IsSystem` is `oid < 16384` — initdb creates none; postgres_fdw's is an
  extension member. `ForeignDataWrapper.Servers(ctx)` is the server
  select narrowed by the wrapper's name, so it works on a Ref.
- **Servers** (`ForeignServers`, `ForeignServerByName`,
  `ForeignServerRef`): `Wrapper` by name, `Type`/`Version` (free text,
  empty for none), `Options`, ACL USAGE (`acldefault('S', owner)`),
  comment; can be an extension member (`ALTER EXTENSION … ADD SERVER`).
  `ForeignServer.UserMappings(ctx)` keys on the name likewise.
- **User mappings** (`UserMappings`, `UserMappingByName(ctx, server,
  user)`, `UserMappingRef`; `""` is PUBLIC, as `PrivilegeGrant.Grantee`;
  `Signature()` → `FOR role SERVER srv`). No name, owner or comment, never
  system or a member: the filter's Name and Schema are ignored, System and
  ExtensionMember true keep none. **Read from the `pg_user_mappings` view,
  not `pg_user_mapping`**, which only a superuser may read (its options
  hold passwords). The view lists every mapping to every role and shows a
  mapping's options only to the mapped role holding USAGE on the server
  (or membership in its owner), to a member of the owner for PUBLIC's, and
  to a superuser; otherwise NULL — the same NULL as a mapping with none.
  `OptionsHidden` restates the view's condition over what any role can
  read (`pg_roles.rolsuper` for the view's `pg_authid`), so "withheld" and
  "none" are told apart; it is never set where options are shown. The
  view's definition is identical on 15–18; the live test holds the
  restatement to it, role by role.
- **Foreign tables** (`ForeignTables`, `ForeignTableByName(ctx, schema,
  name)`, `ForeignTableRef`): `pg_class` kind `f` joined to
  `pg_foreign_table` and the server — `Server`, `Options`, partition and
  inheritance (`IsPartition`, `PartitionBound`, `Parents` — the table
  read's `tableParents`), a table's ACL and comment; the relation filter
  columns. Parts on demand, keyed by the OID with a table's queries:
  `Columns`, `Constraints` (CHECK; NOT NULL rows from 18; a CHECK may be
  NOT ENFORCED from 18), `Triggers` (`relationTriggers`). `Database.Tables`
  still lists kinds `r` and `p` only.
- **`Column.FDWOptions`** (`attfdwoptions`) is read for every relation's
  columns — nil but on a foreign table's.

Options everywhere are `text[]` `name=value` entries split on the first
`=` (`parseSettings`), as `pg_options_to_table` splits them. A user
mapping's password is shown as stored, cleartext, to whoever the view
shows it to.

| Type | Catalog | Notes |
|---|---|---|
| `ForeignDataWrapper` | `pg_foreign_data_wrapper`, `pg_description` | ACL USAGE, default the owner's alone |
| `ForeignServer` | `pg_foreign_server` + its wrapper, `pg_description` | ACL USAGE, default the owner's alone |
| `UserMapping` | `pg_user_mappings` (view) + `pg_foreign_server` | `OptionsHidden`; user `""` / OID 0 for PUBLIC |
| `ForeignTable` | `pg_class` (`f`) + `pg_foreign_table` + `pg_foreign_server` | parts as a table's; `Column.FDWOptions` |

Each listing is planned under 460 on pg15 and pg18. Tests: offline
`foreign_data_test.go`, `foreign_table_test.go` (every field of two rows
with field-count guards; each fragment; the filter tails; the by-name
bindings; not found and server errors wrapped once; Refs; parts keyed by
name or OID, a Ref's OID-keyed part refused; `conenforced` read from 18);
live `TestLiveForeignData` on pg15–pg18 (wrappers, servers and foreign
tables identical as a plain role; user mappings read as four roles —
superuser, plain, the mapped role, a non-superuser server owner — each
option matched to `pg_user_mappings` as that role, `OptionsHidden` exactly
where it withholds options the catalog holds; case-only names; a
partition, an inheriting foreign table, NOT ENFORCED on 18). 20 mutations,
all caught (one, the version handed to the constraint read, after a test
was added for it).

### Routines: functions, procedures, aggregates (`routine.go`)

Landed P4.6 (reads; the writes are P4.13.3). Three types, as CREATE
FUNCTION, CREATE PROCEDURE and CREATE AGGREGATE are three statements with
different clauses and pgMS shows three folders: `Database.Functions` /
`FunctionByName` / `FunctionRef` (`prokind` `f` and `w`),
`Database.Procedures` / `ProcedureByName` / `ProcedureRef` (`p`) and
`Database.Aggregates` / `AggregateByName` / `AggregateRef` (`a`, with its
`pg_aggregate` row), each embedding `Origin` and `RoutineIdentity` (§
Identity) and with `Database()`. `Database.RoutineByName` reads the
identity of any kind. Through **`s.onDatabase`**, one statement each; the
ACL an `aclLateral` over `acldefault('f', …)` (EXECUTE to PUBLIC and the
owner when never set).

By-name reads take `args []OID` and pick an overload by `pickOverload`, as
`routineByName`: nil → the name must be unique in the family (`ErrNotFound`
/ `ErrAmbiguous` naming each `Signature`), non-nil (empty for none) → the
exact input types. A routine of another family of the name is
`ErrNotFound`; an empty schema `ErrSchemaRequired`. A Ref takes the argument
type *names* as DDL writes them (`FunctionRef(schema, name,
[]string{"integer"})`) — it is for writes, and `Signature` writes them as
given.

| Field | Function | Procedure | Aggregate | Source |
|---|---|---|---|---|
| `Origin`, `RoutineIdentity`, `Owner` | ✓ | ✓ | ✓ | `ArgumentTypeNames` are the input `Arguments`' `Type`s, checked against `proargtypes` |
| `Language` | ✓ | ✓ | | `pg_language.lanname` |
| `Arguments []RoutineArgument` | ✓ | ✓ | ✓ | every argument: `Mode` (`proargmodes`, NULL → IN; `IsInput()`), `Name`, `TypeOID`, `Type` (`typeName`), `Default` (`pg_get_function_arg_default`) |
| `ReturnType`, `Result` | ✓ | | ✓ | `typeName(prorettype)`; `pg_get_function_result` (display) |
| `ReturnsSet` | ✓ | | | `proretset` (SETOF, TABLE) |
| `Volatility`, `Strict`, `Leakproof`, `Cost`, `Rows`, `SupportFunction` | ✓ | | | `provolatile`, `proisstrict`, `proleakproof`, `procost`, `prorows`, `prosupport` |
| `Parallel` | ✓ | | ✓ | `proparallel` (`ParallelSafety`) |
| `SecurityDefiner`, `Settings` | ✓ | ✓ | | `prosecdef`; `proconfig` as `ParameterSetting`s (`search_path=app, pg_temp`) |
| `ObjectFile`, `SQLStandardBody` | ✓ | ✓ | | `probin` (a C routine's library); `prosqlbody IS NOT NULL` (BEGIN ATOMIC / RETURN) |
| `AggregateKind`, `DirectArguments` | | | ✓ | `aggkind` (normal, ordered-set, hypothetical-set), `aggnumdirectargs` |
| transition / final / combine / serial / deserial / moving functions, state types and spaces, final extra / modify, initial conditions | | | ✓ | `pg_aggregate`; functions as `ObjectName`, zero for none; `InitialCondition` + `HasInitialCondition` (an empty one is not none) |
| `SortOperator` | | | ✓ | `OPERATOR(schema.op)`, the form `SORTOP` takes whatever the path |
| `Privileges`, `PrivilegesDefault`, `Comment` | ✓ | ✓ | ✓ | `proacl`; `pg_description` |

**Bodies are on demand**: `Function.Body` / `Procedure.Body` → a
`RoutineBody{Source, Definition}` (`prosrc`; `pg_get_functiondef`, whose C
form ends `AS 'obj', 'sym'`), keyed by OID and the family's kinds, a Ref
`ErrHandleNotLoaded`, a dropped routine `ErrNotFound`. A listing of pg15's
own 3,106 functions carries 577 KB of definitions; a database of PL/pgSQL
many times that. An aggregate has no body (`pg_get_functiondef` refuses
one); the P5 scripter writes `CREATE AGGREGATE` from the fields.

**No subquery per row.** The first form — a `LATERAL` per routine for its
arguments, a scalar subquery per type name, `obj_description`, the
extension subquery of `originColumns` — listed pg_catalog's functions in
5 s on pg15 and pg18: the planner estimates ten rows per `unnest` and an
index scan per subquery, the total (1.3 M) crosses `jit_above_cost`
(100 000), and 4 s of it was JIT compilation (1 s with `jit = off`). So:
types are joined (`typeJoins`), the arguments are one grouped read of every
routine of the family's kinds joined by OID (`routineArguments`; a by-name
read narrows it to the name, `$2`), the comment joins `pg_description`, the
extension joins through `originJoin`/`originJoinColumns` (the same
membership condition as `originColumns`), the aggregate's functions,
types and sort operator are joined (`procedureJoin`). Estimated 7.8 k, the
full listing runs in 0.2–0.35 s, a by-name read in ~20 ms (an aggregate's
~60 ms, most of it planning its thirty-odd joins). A family whose listing
can be as long should do the same.

**Rendered text follows the session's `search_path`**, as every expression
(§ Tables): `Result`, `IdentityArguments`, a `Default` (`'{a}'::mood[]`
for `app.mood[]` with `app` on the path) and `Body`'s definition. The
structured names — `Arguments[].Type`, `ReturnType`, `ArgumentTypeNames`,
the aggregate's functions, types and operator — are qualified whatever it
is; pinned by the live test.

Tests: `routine_test.go` (pgmotest: every field of the three types from its
own column with field-count guards; the identity's names derived from the
inputs; names bound as `$1`/`$2`, the argument read narrowed to `$2`,
routed to the database; each family's join fragments; the three filters'
exact tail, the argument read's kinds; overloads ambiguous, picked and
missed; `ErrSchemaRequired`, not found, server errors wrapped once;
malformed ACL and argument arrays, inputs that are not `proargtypes`; Refs
and `Body` refused before sending; `Body` keyed by OID and kinds, dropped
`ErrNotFound`; `RoutineByName` on the database's pool; every code's
`String`). Live, `TestLiveRoutines` on pg15–pg18: overloads with
`DEFAULT`, `VARIADIC`, `OUT`, `RETURNS TABLE`, an odd-named composite and
an enum array; STABLE STRICT LEAKPROOF; IMMUTABLE SECURITY DEFINER PARALLEL
SAFE COST ROWS with `SET search_path`; a SQL-standard body; `SUPPORT`; a
grant `WITH GRANT OPTION` and a revoke from PUBLIC; a case-only name;
citext's C function and pg_catalog's `row_number` (window); a PL/pgSQL and
a BEGIN ATOMIC procedure; `ag(*)`, a moving aggregate with every function,
a `SORTOP` one, an ordered-set one and pg_catalog's hypothetical `rank`;
each by-name read refusing the other families; structured vs rendered names
under a path finding the types; a `Signature` replayed in `COMMENT ON`;
**every routine of the database** (pg_catalog's included) listed with the
identity `routineIdentityColumns` reads for it; and the listings with their
bodies read again as a plain role, identical.

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
statistics object, a policy, a publication, a rule, an extension; from
P4.11 a text search parser, template, dictionary and configuration, a
language, a cast and two operators, one with a shell negator) — a read
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

`Table` is a swept type (P4.4): `app.child`, read by name, is swept
reflectively (each part read and `Size`), and `sweepTables` drives the
listing narrowed to `app`, the by-name read of `app.parent` (comment,
`ANALYZE`d estimate, never-set ACL) and each part read on the fixture table
that has one — `app.parent`'s columns and size, `app.child`'s constraints,
index, trigger and policy, `app.part`'s partition. Result: 50 calls per
role, 0 failed, on pg15–pg18.

`View` and `MaterializedView` are swept types (P4.5): `app.parent_names`
(made `security_barrier`) and `app.parent_totals` (given a unique index),
read by name, are swept reflectively, and `sweepViews` drives both listings
narrowed to `app`, both by-name reads and each part read, asserting their
rows. Result: 64 calls per role, 0 failed, on pg15–pg18.

`Trigger` and `EventTrigger` are swept types (P4.9; no context method of
their own yet): `app.parent_names`' INSTEAD OF trigger `names_update`
(with arguments) and the commented event trigger `sweep_ddl`, read by name.
`sweepTriggers` drives the database's triggers narrowed to `app`, both
by-name reads, `View.Triggers` (the read `Table.Triggers` shares,
`relationTriggers`) and the event trigger listing, asserting their rows.
Result: 111 calls per role, 0 failed, on pg15–pg18.

`Extension` is a swept type (P4.10): the fixture's citext (commented), read
by name, has its `Members` swept reflectively, as are both database
listings. `sweepExtensions` drives the installed listing narrowed to `app`,
plpgsql by name (the system one), citext's members, and the available
listing and by-name read of citext and postgres_fdw, asserting their rows.
Result: 119 calls per role, 0 failed, on pg15–pg18.

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
