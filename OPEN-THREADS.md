# Open threads

Open work and settled decisions — nothing else. Close an item by deleting it;
add one whenever something is knowingly left undone. `CLAUDE.md` §
Conventions and `ARCHITECTURE.md` describe what *is*; this file records what
is knowingly **not** done, and what must be watched. The plan (what *will* be
done) is `~/go/pgms/docs/PLAN.md`.

## Version support: the policy, and how it is held

The target is **PostgreSQL 15 and later** (`PLAN.md` D2): 15 is the oldest lab
cluster, so every gate is live-testable. The standing check will be
`TestLiveVersionSweep` (gosmo's), run on pg15 after any query change: it calls
every read pgmo exposes and reports what the server rejects. A sweep of 0
failures is not proof on its own — confirm the reads were actually called.

The gates, recorded so the next audit does not re-derive them (filled as code
lands; known ones are in `ARCHITECTURE.md` § Version gating):

| Column or construct | Held by |
|---|---|
| — | — |

## Lab: what it can and cannot exercise

Since 2026-11-20 every lab cluster preloads `pg_stat_statements` and `pg_cron`
and runs `wal_level = logical` (`PLAN.md` § Lab gaps). The
`pg_stat_statements` extension version differs by major (1.10 on 15/16, 1.11 on
17, 1.12 on 18): gate its columns on the extension version, not only the
server version. There is still no physical standby, so the standby side of
replication (`pg_stat_wal_receiver`) is fake-tested only.

## Scripter fidelity

To be measured against `pg_dump --schema-only` (`PLAN.md` P5.4). Gaps go
here.

## Per-database pools: accepted limits

- **The cap is soft** (`ARCHITECTURE.md` § Per-database pools): only idle
  entries are evicted, so a caller holding rows open in many databases keeps
  them all open. Revisit only if pgMS shows it in practice.
- **An opener's single-connection `Querier`** (a `*pgx.Conn`) is never
  evicted, since pgmo cannot tell it is idle; an opener wanting eviction
  returns a pool, or a `Querier` with `Idle() bool`.
- **Concurrent waiters on an open share its outcome**: if the first
  statement's context is cancelled mid-dial, the others waiting on that open
  fail with it rather than retrying. Derived pools dial nothing on open, so
  this only touches a `DatabaseOpener` that dials.

## pgmotest: not yet faked

Landed P2.2.2 with what pgmo sends today. Each arrives with the pgmo feature
that needs it, and is added to `pgmotest` in the same change:

- **Retry and cancel (P2.2.5, P2.2.7).** gosmotest's `Hold` (keep an answer
  back until released or the context ends) and `DropConn` (a connection-class
  failure) are not ported.
- **Notices (P2.2.6).** No way yet to answer with notices.
- **pgx.Tx extras.** `SendBatch` and `Prepare` fail the test;
  `LargeObjects` returns pgx's zero value, which panics if used.
