# pgmo
PostgreSQL Management Objects Library

A Go library of PostgreSQL management objects — the PostgreSQL counterpart of
[gosmo](https://github.com/radix29/gosmo) (SQL Server Management Objects in
Go). Pure Go, no CGO, over [pgx v5](https://github.com/jackc/pgx).

```
go get github.com/radix29/pgmo
```

> **Status: planning.** No code or release yet. The plan, shared with
> [pgMS](https://github.com/radix29/pgms) (the terminal management tool built
> on pgmo), is at
> [pgms `docs/PLAN.md`](https://github.com/radix29/pgms/blob/main/docs/PLAN.md).

## Supported PostgreSQL versions

**PostgreSQL 15 and later.** Features added after 15 are version-gated, not
assumed.

## What it will cover

- **Server** — version and settings (`pg_settings`, `ALTER SYSTEM`), roles
  and their attributes and memberships, tablespaces, the connected role's
  capabilities, the server log, activity and statistics views.
- **Database** — create/alter/drop, owner, encoding and locale, connection
  limits, per-database settings, sizes, extensions, schemas, privileges and
  default privileges, comments.
- **Objects** — tables (partitioned, foreign, unlogged), columns, constraints
  (including exclusion), indexes of every access method, triggers, row-level
  security policies, extended statistics, views and materialized views,
  functions, procedures and aggregates (identified by signature), sequences,
  domains, enums, composite and range types, event triggers, foreign data
  wrappers, servers and user mappings, text search objects, collations.
- **Scripting** — a `Scripter` generating CREATE/ALTER/DROP DDL, whole-database
  scripts in dependency order, and `WithScript`, which collects the
  statements a write *would* run instead of running them.
- **Monitoring** — `pg_stat_activity`, locks and blocking, `pg_stat_*` and
  `pg_statio_*` views, `pg_stat_io`, `pg_stat_statements`, progress views,
  cancel and terminate.
- **EXPLAIN** — estimated and actual plans as parsed JSON.
- **Maintenance** — VACUUM, ANALYZE, REINDEX, CLUSTER, REFRESH MATERIALIZED
  VIEW, `amcheck` and `pgstattuple` reads.
- **Bulk** — COPY in and out.
- **Replication (read-only) and pg_cron jobs.**

Conventions follow gosmo's: every database method takes a `context.Context`
first and has one form; `XByName` reads the catalog while `XRef` is a
lookup-free handle; catalog state is exported fields; every `Create*` takes a
request value. Code that calls pgmo can be tested without a server through
`pgmotest`.

## Documentation

| Document | |
| --- | --- |
| [`ARCHITECTURE.md`](ARCHITECTURE.md) | API map, feature map, errors, connection internals |
| [`OPEN-THREADS.md`](OPEN-THREADS.md) | Work knowingly left undone |
| [`docs/PLAN.md`](docs/PLAN.md) | Pointer to the shared plan |
| [`examples/README.md`](examples/README.md) | Planned runnable examples |
| [`CONTRIBUTING.md`](CONTRIBUTING.md) | How to contribute |

## License

pgmo is MIT licensed (see [`LICENSE`](LICENSE)).
