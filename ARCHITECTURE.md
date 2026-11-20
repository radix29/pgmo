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

Gates known before the first line of code (`PLAN.md` P1.4.2):

| Since | What | Where it will matter |
|---|---|---|
| 16 | `pg_stat_io`; role membership `INHERIT`/`SET` options; `EXPLAIN (GENERIC_PLAN)` | activity, roles, EXPLAIN |
| 17 | `pg_stat_checkpointer` (split from `pg_stat_bgwriter`); `pg_maintain`; `MERGE … RETURNING`; `EXPLAIN (SERIALIZE, MEMORY)`; builtin collation provider | activity, capabilities, EXPLAIN, collations |
| 18 | virtual generated columns; `pg_stat_io` byte columns; `pg_stat_statements` additions | tables, activity, top queries |

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

`Connect(ctx, ConnectionOptions)` builds a `pgxpool.Config` (pgx's
`ParseConfig`, which already honours libpq service files, `.pgpass` and `PG*`
variables), validates pgmo's fields first, and opens the pool for the
requested database. `NewServer(ctx, pool)` wraps an existing pool;
`Server.Pool()` returns it.

### Per-database pools

PostgreSQL binds a session to one database. `Server` holds a map
database → pool, opened on first use from the same options with `dbname`
replaced, capped (`MaxDatabasePools`) and closed least-recently-used. This
replaces gosmo's `USE [db]` and `perDatabaseExec`. `Database` handles read
through their own pool.

### The `Querier` seam

pgmo's reads and writes go through a narrow interface (`Query`, `QueryRow`,
`Exec`, `CopyFrom`, `Begin`), satisfied by `*pgxpool.Pool`, `*pgx.Conn` and
`pgx.Tx`. `pgmotest` implements it with scripted rows; no fake SQL driver is
needed.

## Feature map

To be written per family as it lands: pgmo's name for each concept, the
catalog it reads, the statements it writes. The disposition of every gosmo
family (reuse / adapt / replace / drop) is in `PLAN.md` § Feature-parity
analysis.

## Errors

Sentinels as gosmo: `ErrNotFound`, `ErrUnsupported`, `ErrInvalidRequest`,
`ErrAmbiguous`, `ErrSchemaRequired`, `ErrHandleNotLoaded`,
`ErrUnsupportedVersion`, `ErrDefinitionHidden`. A server error is a
`*pgconn.PgError`; `AsPgError` exposes it, and classification is by SQLSTATE
class (`08` connection, `40001`/`40P01` retryable, `42501` permission, `42P01`/
`42704`/`42883` undefined, `42P07`/`42710` duplicate, `57014` cancelled).

## Authentication

v1: password, md5 and SCRAM-SHA-256 (negotiated by pgx), TLS client
certificates, `.pgpass`, service files. Post-v1 (`PLAN.md` P16.2): cloud IAM
tokens supplied as the password via pgx's `BeforeConnect`. GSSAPI is not
planned for v1.

## Security

Identifiers and literals through `pgsql`, one implementation, pinned to the
server's own `quote_ident`/`quote_literal` by live tests. Passwords are never
sent in cleartext in DDL (pre-hashed SCRAM verifiers), and are redacted from
`WithScript` captures and observers unless the caller opts in.

## Maintaining this document

As gosmo: diagrams live in `diagram/*.mmd`; only `00-map.mmd` is inlined
here. A new family adds its diagram, its feature-map section and its row in
the map in the same change.
