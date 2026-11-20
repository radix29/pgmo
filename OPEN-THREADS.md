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
