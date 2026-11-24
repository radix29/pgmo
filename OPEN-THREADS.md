# Open threads

Open work and settled decisions — nothing else. Close an item by deleting it;
add one whenever something is knowingly left undone. `CLAUDE.md` §
Conventions and `ARCHITECTURE.md` describe what *is*; this file records what
is knowingly **not** done, and what must be watched. The plan (what *will* be
done) is `~/go/pgms/docs/PLAN.md`.

## Version support: the policy, and how it is held

The target is **PostgreSQL 15 and later** (`PLAN.md` D2): 15 is the oldest lab
cluster, so every gate is live-testable. The standing check is
`TestLiveVersionSweep` (`live_versionsweep_test.go`, `ARCHITECTURE.md` §
Version sweep), run on pg15 after any query change: it calls every read pgmo
sends, as the connected role and as a plain one, and reports what the server
rejects. A sweep of 0 failures is not proof on its own — the `-v` log lists
every label called, and `sweepMustCall` fails one never reached.

Last result (2026-11-21, P3.8): 17 calls per role, 0 failed, 0 refused, on
pg15.20, pg16.16, pg17.12 and pg18.7.

What the sweep does not reach yet: the P4 families' listings join it as they
land (a family type goes in `sweptTypes`; a read it sends fails
`TestSweepCoversEveryRead` until `sweepReaders` maps it). A query fragment no
read sends (`originColumns`, `ObjectFilter.clause` today) is not found by that
guard — the sweep runs those two by hand until a family read sends them.

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
replication (`pg_stat_wal_receiver`) is fake-tested only, as is
`ServerInfo.InRecovery = true`. No managed service either: `ServerInfo.Managed`
(by the services' administrator roles) is a hint checked only against the fake
until P16.2.

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

## Cancel: accepted limits

- **A pool given to `NewServer` cancels as its owner configured it** — pgx's
  default closes the session — and so do the derived pools and sessions
  pgmo opens from it (`ARCHITECTURE.md` § Cancel). pgmo does not override a
  caller's configuration; `UseCancelRequests` is the opt-in. Revisit if
  pgMS ever wraps a pool it did not build.
- **`cancelFallback` (5 s) is not configurable.** A `ConnectionOptions`
  field would have to be a libpq keyword (`TestEveryFieldReachesPgx`);
  add a `ServerOption` if a caller needs another bound.

## Name resolution: operators follow `search_path`

pgmo qualifies relations, functions and types (`ARCHITECTURE.md` § Name
resolution) but writes operators bare (`n.oid = c.relnamespace`), not
`OPERATOR(pg_catalog.=)`. `pg_catalog` is searched first unless the path
names it later, so this matters only for a `search_path` that lists another
schema ahead of `pg_catalog` and an operator there with the same argument
types — checked on pg15 and pg18: it is then the one used. Accepted: the
caller chose that path, and spelling every join `OPERATOR(pg_catalog.=)`
would make every read unreadable. Revisit if pgMS ever reads with a path it
does not control (psql qualifies only its regex operators, for the same
reason). A name inside a literal cast to `regclass`/`regproc`
(`'pg_class'::pg_catalog.regclass`) is resolved through the path too; write
it schema-qualified inside the literal — the guard cannot see into literals.

## pgmotest: not yet faked

Landed P2.2.2 with what pgmo sends today. Each arrives with the pgmo feature
that needs it, and is added to `pgmotest` in the same change:

- **`DropConn`** (gosmotest's) is not ported and not needed: a Fake has no
  sessions to invalidate, and a Responder that fails with a retryable error
  and then answers scripts a transient failure
  (`TestTransientFailureIsRetried`). `Hold` landed with cancel (P2.2.7).
- **Notices (P2.2.6).** Not faked, and not fakeable as the seam stands:
  notices belong to a session from `Server.OpenSession`, a `*pgx.Conn`
  with its `OnNotice` set at dial time, which a `Fake` cannot be. A
  caller's notice tests drive a pgproto3 backend (as `session_test.go`
  does). Revisit if a pgmo read ever has to surface notices itself.
- **pgx.Tx extras.** `SendBatch` and `Prepare` fail the test;
  `LargeObjects` returns pgx's zero value, which panics if used.

## Roles: what P4.2 left

- **No password flag.** Whether a role has a password (and its hash's
  method) is `pg_authid.rolpassword`, readable by superusers only; pg_roles
  shows `********` for every role. A superuser-only `HasPassword` read could
  be added for pgMS's Login properties page (P8) if it needs one.
- **Direct grants only.** `MemberOf`/`Members` are `pg_auth_members` rows;
  the transitive closure (what a role holds through nesting, and with which
  INHERIT/SET path) is `pg_has_role` per pair or a recursive query — add it
  with effective permissions (P4.13.4/P5).
- **`ALTER ROLE ALL SET`** (`pg_db_role_setting` with both `setrole` and
  `setdatabase` 0) is neither a role's nor a database's setting and is not
  read; neither is `ALTER ROLE ALL IN DATABASE`, which is
  `Database.Settings`.
- **`RoleMembership.Inherit`/`Set` on 15 are what 15 checks**, not stored
  options; a P4.13.2 write must not emit `WITH INHERIT`/`SET` before 16.
- **Not read yet:** objects owned (`pg_shdepend`, needed before a drop —
  `REASSIGN OWNED`/`DROP OWNED`, P4.14), `rolpassword` method, security
  labels.

## Flaky: `TestDatabasePoolsUnderConcurrency` under `-race`

Seen 2026-11-21 while landing P2.2.6, present before it (2 of 15 runs on
the P2.2.5 tree, 3 of 15 with P2.2.6): under `go test -race`, some readers
of database `e` get `closed pool` and the test ends with 2 databases open.
A statement reached a `Fake` that eviction had already closed — either the
fake's `Idle()` reports idle while a lease's statement is about to run, or
the lease window misses a step. A race between lease and eviction, which
the soft-cap design says cannot happen; not yet diagnosed.
Seen again 2026-11-21 during P3.6 **without** `-race` (1 of 7 runs of
`go test ./pgmotest/`), so it is not only a race-detector timing artefact.
Seen again 2026-11-21 during P3.7 (1 of 10 runs, without `-race`).

## Script mode: not ported from gosmo yet

- **`WithStatementObserver`** (the executing-side twin of `WithScript`: a
  callback per statement that ran, held until commit inside
  `InTransaction`) and **`WithScriptServer`** / `ScriptEntry.Server`. A
  collector shared by two Servers tells their entries apart by database
  only. Port when pgMS needs either (a multi-step dialog reporting how far
  it got; a script spanning clusters).
- **`execAtomic`**: gosmo's all-or-nothing batch. PostgreSQL's DDL is
  transactional, so a multi-statement write uses `InTransaction` (or one
  `exec` of several statements, which the simple protocol runs as one
  implicit transaction) — decide when the first such write lands (P4).
- **Secrets in another database's statement**: `execSecret` is the Server's
  own database only. A user mapping's `OPTIONS (password …)` (P4.12) runs
  in the mapping's database: add `execInSecret` (through `onDatabase`) with
  it, and its `secretWrites` entry. A secret that is not a password (a
  user mapping's, a subscription's `CONNECTION` string) is sent as is —
  only a role password has a verifier form — so it reaches the server log;
  say so in that write's doc.

## Passwords: pgx's login differs from PostgreSQL's SASLprep

pgx (v5.13) prepares a SCRAM login password with PRECIS OpaqueString (NFC);
PostgreSQL and libpq use RFC 4013 SASLprep (NFKC). pgmo's verifiers follow
the server, so a password NFKC changes (full-width letters, ligatures,
superscripts) logs in with psql but not with pgx — the same as one the
server hashed from cleartext. Worth reporting upstream to pgx; pgMS could
warn in the password dialog when `scram.Normalize(p)` differs from NFC(p)
(P4.13.2).
- **`scriptLiteral` covers plain Go values only**: no `pgtype` values that
  are not `driver.Valuer`s, no slices but `[]string`, no `time.Duration`,
  no years outside 1–9999. A write binding one fails under `WithScript`
  with `ErrUnsupported`; extend `scriptLiteral`, and its live test, then.

## Identity: what P3.3 left for the families

`RoutineIdentity` and `routineByName` (`identity.go`) have no exported caller
yet: P4.6's `FunctionByName`/`ProcedureByName`/`AggregateByName` embed and
wrap them (adding the `pgmo: ` prefix). A caller naming an overload has
argument type OIDs only; looking one up by type *names* needs those names
resolved server-side (`to_regtype`) — qualified, or the session's
`search_path` decides — and is left to P4.6. The P5 scripter must write a
zero-argument aggregate as `AGGREGATE s.ag(*)` (or use `ROUTINE`):
`Signature()` gives `s.ag()`.

## System objects: what P3.7 left for the families

`Origin`, `originColumns`/`scanOrigin` and `isExtensionMember`
(`system_objects.go`) have no listing calling them yet: each P4 family embeds
`Origin`, reads it with `originColumns`, and sets its `filterColumns`'
`system`/`extension` from `systemSchema`/`isExtensionMember` on the same
expressions, so a row's flags and the filter never disagree. `catalogTable`
holds `pg_class`, `pg_proc`, `pg_type`, `pg_namespace`; a family whose
objects live elsewhere (operators, casts, FDWs, event triggers, languages)
adds its catalog. Objects with no schema (event triggers, casts, languages,
FDWs) have no `IsSystem` by this rule — the family decides (an OID below
`FirstNormalObjectId`, 16384, is the usual sign of a built-in one). The P5
scripter writes `CREATE EXTENSION` for a member, never the member's DDL.

## Databases: what P4.1 left

- **Role-in-database settings** (`pg_db_role_setting` rows with both
  `setdatabase` and `setrole` set, `ALTER ROLE … IN DATABASE … SET`) are not
  on `Database.Settings`; P4.2 reads them with the role
  (`Role.DatabaseSettings`).
- **`ParameterSetting.Value` is the stored text, not a literal.** A list
  parameter keeps its elements' quotes (`"$user", public`); the P4.13.1
  `SET` write must turn it back into a statement the way `pg_dump` does
  (`GUC_LIST_QUOTE` parameters split and quoted per element).
- **No sizes with the listing.** `Database.Size` is one round trip per
  database; a Details pane over hundreds of databases may want one read of
  every size it may see (`CASE WHEN has_database_privilege(oid, 'CONNECT')
  OR pg_has_role('pg_read_all_stats', 'USAGE') THEN pg_database_size(oid)
  END`). Add it when pgMS's Databases folder needs it (P8).
- **Not read yet:** `pg_database_collation_actual_version(oid)` (the
  version mismatch `ALTER DATABASE … REFRESH COLLATION VERSION` fixes),
  `age(datfrozenxid)` (wraparound, P6/P14), `datacl` (privileges,
  P4.13.4/P8), `dathasloginevt` (17+; event triggers, P4.9).
- **`diagram/`** is still empty: no family through P4.1 has its class
  diagram yet (gosmo's `diagram/*.mmd`).

## Roles: what P4.2 left

- **No password flag.** Whether a role has a password (and its hash's
  method) is `pg_authid.rolpassword`, readable by superusers only; pg_roles
  shows `********` for every role. A superuser-only `HasPassword` read could
  be added for pgMS's Login properties page (P8) if it needs one.
- **Direct grants only.** `MemberOf`/`Members` are `pg_auth_members` rows;
  the transitive closure (what a role holds through nesting, and with which
  INHERIT/SET path) is `pg_has_role` per pair or a recursive query — add it
  with effective permissions (P4.13.4/P5).
- **`ALTER ROLE ALL SET`** (`pg_db_role_setting` with both `setrole` and
  `setdatabase` 0) is neither a role's nor a database's setting and is not
  read; neither is `ALTER ROLE ALL IN DATABASE`, which is
  `Database.Settings`.
- **`RoleMembership.Inherit`/`Set` on 15 are what 15 checks**, not stored
  options; a P4.13.2 write must not emit `WITH INHERIT`/`SET` before 16.
- **Not read yet:** objects owned (`pg_shdepend`, needed before a drop —
  `REASSIGN OWNED`/`DROP OWNED`, P4.14), `rolpassword` method, security
  labels.

## Schemas: what P4.3 left

- **No effective privileges.** `Privileges` is the ACL as stored (plus the
  hard-wired default); what a role can actually do through membership is
  `has_schema_privilege` — with effective permissions (P4.13.4/P5).
- **Database-wide default privileges are on the Database only.** A Schema
  carries its IN SCHEMA entries; combining them with the database-wide ones
  (and the hard-wired default) into "what a new table here will get" is
  left to the caller or to P5's scripter.
- **Not read yet:** schema size (sum of its relations, P4.4/P6), objects
  per schema, security labels.

## Flaky: `TestDatabasePoolsUnderConcurrency`

`pgmotest`'s concurrency test fails about one run in three (seen 2026-11-21,
during P3.8, with no change to pools or `pgmotest`): a reader's
`DatabaseQuerier(db).Query` fails `closed pool` (`pgmotest_test.go:537`) —
its database's Fake was evicted and closed between the lease and the
statement, or the Fake reopens late. Either the soft cap evicts an entry a
statement is about to use (a pgmo bug, `database_pools.go` `lease`/`evict`)
or the Fake's reopen races its close. Not investigated yet.
Seen again 2026-11-21 during P4.1 (2 of 8 runs under `-race`).
