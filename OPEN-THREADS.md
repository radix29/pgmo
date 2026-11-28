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

Last result (2026-11-25, P4.4): 50 calls per role, 0 failed, 0 refused, on
pg15–pg18.

What the sweep does not reach yet: the P4 families' listings join it as they
land (a family type goes in `sweptTypes`; a read it sends fails
`TestSweepCoversEveryRead` until `sweepReaders` maps it). A query fragment no
read sends (`originColumns`, `ObjectFilter.clause` today) is not found by that
guard — the sweep runs those two by hand until a family read sends them.

The gates, recorded so the next audit does not re-derive them (filled as code
lands; known ones are in `ARCHITECTURE.md` § Version gating):

| Column or construct | Held by |
|---|---|
| `pg_constraint.conenforced` (18) | `constraintsQuery` (`colSince`, `true` before), `constraint.sql`, `TestTableConstraints`, `TestLiveTables` (NOT ENFORCED on 18) |
| `pg_attribute.attstattarget` (`int4` 15–16, nullable `int2` 17+) | ungated: `COALESCE(…, -1)::integer` reads both; `TestLiveTables` on pg15–pg18 |
| NOT NULL as `pg_constraint` rows (18) | ungated: listed when present; `TestLiveTables` |
| `attgenerated = 'v'` (18) | ungated value: `GeneratedVirtual`; `TestLiveTables` on 18 |

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

To be measured against `pg_dump --schema-only` (`PLAN.md` P5.4); an ad-hoc
diff of `TestLiveScripter`'s fixture showed none on pg15 and pg18. Known
gaps of P5.1 (`ARCHITECTURE.md` § Scripter):

- **Not restated, because not read:** a column's `SET STORAGE` against its
  type's default, `attcompression`, per-column `attoptions`, replica
  identity, the table access method, typed tables (`OF type`), an identity
  column's sequence options (`START`, `INCREMENT` … in `GENERATED … AS
  IDENTITY (…)`), rules, extended statistics, security labels, a database's
  ACL (`datacl`), view column options.
- **18's NOT NULL constraints** are written as the column's `NOT NULL`:
  their names (when not the default `t_c_not_null`), `NO INHERIT` and `NOT
  VALID` are lost. Likewise a domain's (17+).
- **A partition** is `CREATE TABLE … PARTITION OF … FOR VALUES` with its own
  constraints: a column default or NOT NULL of its own beyond its parent's
  is lost (`pg_dump` creates it standalone and attaches it). An
  inheritance child writes its local columns only.
- **Grantors** other than the owner are not restated (`pg_dump` uses SET
  SESSION AUTHORIZATION); nor is a membership's `GRANTED BY`.
- **Database-wide default privileges** (`ALTER DEFAULT PRIVILEGES` without
  `IN SCHEMA`) are in no single-object script: P5.3's whole-database
  script, or a security script, writes them.
- **Not DDL**: role passwords (a note), a user mapping's `password` option
  (`PasswordPlaceholder`), a sequence's current value, extension
  configuration tables' rows.
- **`IfNotExists`** guards only the CREATEs PostgreSQL has the form for; no
  `DO` block is written for the others (a view, a function, a type …).
- **`SchemaQualify` off** writes the object's own name bare and renders
  server text with its schema first on the path; names pgmo builds from
  the catalog (a column's type, a foreign key's table, a cast's types)
  stay qualified.
- **One object per read**: each `Script…` reads its object's parts one
  statement each; P5.3 wants them batched over many tables (§ Tables).

## Per-database pools: accepted limits

- **`TestDatabasePoolsUnderConcurrency` (pgmotest) is flaky**: about one
  run in five fails with "closed pool" reads and "2 databases open when the
  readers finished (cap 2, soft)" — on `HEAD` before P5.1 as well (found
  2026-11-28, 2 of 10 runs on a clean export). An eviction racing a lease,
  in the fake or in `database_pools.go`; not investigated yet.

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

## Flaky: `TestDatabasePoolsUnderConcurrency` under `-race`

Also seen without `-race` on 2026-11-28 (1 of 10 runs of `go test -count=10
./pgmotest`, `b: closed pool`), on a tree that touches neither pgmotest nor
the pools.

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

P4.6's families embed `RoutineIdentity` and share `routineByName`'s overload
rule (`pickOverload`); `Database.RoutineByName` exports the lookup. Still
open: a caller naming an overload has argument type OIDs only; looking one
up by type *names* needs those names resolved server-side (`to_regtype`) —
qualified, or the session's `search_path` decides — a helper pgmo does not
have yet (`…Ref` takes names, for writes, unresolved). The scripter (P5.1)
writes a zero-argument aggregate as `AGGREGATE s.ag(*)`
(`aggregateSignature`): `Signature()` gives `s.ag()`.

## System objects: what P3.7 left for the families

`Origin`, `originColumns`/`scanOrigin` and `isExtensionMember`
(`system_objects.go`) have no listing calling them yet: each P4 family embeds
`Origin`, reads it with `originColumns`, and sets its `filterColumns`'
`system`/`extension` from `systemSchema`/`isExtensionMember` on the same
expressions, so a row's flags and the filter never disagree. `catalogTable`
holds `pg_class`, `pg_proc`, `pg_type`, `pg_namespace`,
`pg_event_trigger` (P4.9), and P4.11's `pg_collation`, `pg_language`,
`pg_cast`, `pg_operator` and the four `pg_ts_*`; a family whose objects
live elsewhere adds its catalog (P4.12 added `pg_foreign_data_wrapper`
and `pg_foreign_server`). Objects with no schema (event triggers,
extensions, casts, languages, tablespaces, FDWs) have no `IsSystem` by
this rule — the family decides (an OID below `FirstNormalObjectId`, 16384,
is the usual sign of a built-in one; event triggers use it, P4.9,
extensions, P4.10, where it marks plpgsql, tablespaces, languages and
casts, P4.11, wrappers and servers, P4.12, where it marks none). The
scripter (P5.1) writes a note for a member, never the member's DDL;
`ScriptExtension` writes `CREATE EXTENSION`.

## Databases: what P4.1 left

- **Role-in-database settings** (`pg_db_role_setting` rows with both
  `setdatabase` and `setrole` set, `ALTER ROLE … IN DATABASE … SET`) are not
  on `Database.Settings`; P4.2 reads them with the role
  (`Role.DatabaseSettings`).
- **`ParameterSetting.Value` is the stored text, not a literal.**
  `ParameterSetting.Values()` (P4.13.1) splits a list-quoted parameter as
  pg_dump does, from pg_dump's list of the built-in ones; an extension's
  list-quoted parameter is not known (the flag is not in SQL) and reads as
  one value. Watch a release adding a `GUC_LIST_QUOTE` parameter
  (pg_dump's `variable_is_guc_list_quote`).
- **Not written by P4.13.1:** `ALTER DATABASE … REFRESH COLLATION VERSION`, `ALTER DATABASE …
  SET … FROM CURRENT`, `ALTER ROLE … IN DATABASE … SET` (the role's,
  P4.13.2), `DROP DATABASE IF EXISTS`
  (a missing database is the server's 3D000).
- **`releaseDatabase` closes only an idle pool.** Another goroutine reading
  the database between the release and the `DROP`/`RENAME` reopens one, and
  the statement then fails with 55006 (or `FORCE` terminates it). pgMS should
  not read a database it is dropping; if that race shows up, hold the pool
  closed for the statement's length.
- **No sizes with the listing.** `Database.Size` is one round trip per
  database; a Details pane over hundreds of databases may want one read of
  every size it may see (`CASE WHEN has_database_privilege(oid, 'CONNECT')
  OR pg_has_role('pg_read_all_stats', 'USAGE') THEN pg_database_size(oid)
  END`). Add it when pgMS's Databases folder needs it (P8).
- **Not read yet:** `pg_database_collation_actual_version(oid)` (the
  version mismatch `ALTER DATABASE … REFRESH COLLATION VERSION` fixes),
  `age(datfrozenxid)` (wraparound, P6/P14), `datacl` (privileges, P8 — written
  by `Server.ApplyPrivileges` since P4.13.4, read back by hand in its test), `dathasloginevt` (17+; a flag the server keeps so a login can skip
  the event trigger lookup — `Database.EventTriggers` filtered on
  `EventTriggerLogin` is the answer, so it stays unread).
- **`diagram/`** is still empty: no family through P4.1 has its class
  diagram yet (gosmo's `diagram/*.mmd`).

## Roles: what P4.2 and P4.13.2 left

- **No password flag.** Whether a role has a password (and its hash's
  method) is `pg_authid.rolpassword`, readable by superusers only; pg_roles
  shows `********` for every role. A superuser-only `HasPassword` read could
  be added for pgMS's Login properties page (P8) if it needs one.
- **Direct grants only.** `MemberOf`/`Members` are `pg_auth_members` rows;
  the transitive closure (what a role holds through nesting, and with which
  INHERIT/SET path) is `pg_has_role` per pair or a recursive query — add it
  with effective permissions (P5; § Privilege writes).
- **`ALTER ROLE ALL SET`** (`pg_db_role_setting` with both `setrole` and
  `setdatabase` 0) is neither a role's nor a database's setting and is not
  read; neither is `ALTER ROLE ALL IN DATABASE`, which is
  `Database.Settings`.
- **`RoleMembership.Inherit`/`Set` on 15 are what 15 checks**, not stored
  options; the P4.13.2 writes refuse `INHERIT`/`SET` (and `ADMIN FALSE`)
  before 16 with `ErrUnsupportedVersion`.
- **Objects owned** are read since P4.14: `Database.Dependents` on the
  role's `Address()` per database, `Server.DependentDatabases` naming the
  databases, `Server.Dependents` the shared objects.
- **Not read yet:** `rolpassword` method, security labels.
- **`REASSIGN OWNED`/`DROP OWNED` landed with P4.14** (`Database.ReassignOwned`,
  `Database.DropOwned`; § Dependencies and search).
- **Not written:** `ALTER ROLE ALL [IN DATABASE] SET`, as it is not read. `SET … FROM CURRENT`
  is not offered (the session's value, not the caller's).
- **Memberships are not mirrored** by `AddMember`/`RemoveMember`: the
  server records the grantor (from 16 a superuser's grant as the bootstrap
  superuser's) and may merge with an existing grant; read the role again.
- **pgx login vs SASLprep** (§ Passwords): pgMS's password dialog could
  warn when NFKC changes the password; pgmo exposes no helper yet
  (`internal/scram.Normalize` is internal) — add one when P8's dialog asks.

## Schemas: what P4.3 left

- **No effective privileges.** `Privileges` is the ACL as stored (plus the
  hard-wired default); what a role can actually do through membership is
  `has_schema_privilege` — with effective permissions (P5; § Privilege
  writes).
- **Database-wide default privileges are on the Database only.** A Schema
  carries its IN SCHEMA entries; combining them with the database-wide ones
  (and the hard-wired default) into "what a new table here will get" is
  left to the caller or to P5's scripter.
- **Not read yet:** schema size (sum of its relations, P4.4/P6), objects
  per schema, security labels.

## Tables: what P4.4 left

- **Rendered text follows the caller's `search_path`.** `Column.Default`,
  `GenerationExpression`, `PartitionBound`, `Constraint.Definition`,
  `Index.Definition`/`KeyColumns`/`Predicate`, `Trigger.Definition` and
  `Policy.Using`/`WithCheck` come from `pg_get_*`, which leave a name
  visible on the session's path unqualified (`ARCHITECTURE.md` § Tables).
  Fine for display, as psql; the scripter (P5.1) reads them under fixed
  settings (`renderIn`, `ARCHITECTURE.md` § Scripter), so its DDL means
  the same in any session.
- **One table at a time.** Each part is one statement per table; the P5
  scripter (and a Columns pane over a whole schema) wants each part for
  many tables in one statement, grouped in Go (`conrelid = ANY($1)`).
  Add batch forms then; the scan functions are already per row.
- **No sizes with the listing.** `Table.Size` is one round trip per table,
  as `Database.Size`; a partitioned table's own size is 0 — summing its
  partitions (`pg_partition_tree`) is left to the caller or P6.
- **Not read yet:** identity sequence options (P4.7, `pg_get_serial_sequence`
  / the owned-by link), `attcompression`, `attoptions` (per-column
  `n_distinct`), `confdelsetcols` (`ON DELETE SET
  NULL (cols)` — in `Definition`), `conperiod` (18, `WITHOUT OVERLAPS` /
  `PERIOD` — in `Definition`), replica identity (`relreplident`, P14),
  the table access method (`relam`), typed tables (`reloftype`),
  `relallfrozen` (18), rules (`pg_rewrite`, shown on their table, P8.2) and
  extended statistics (`pg_statistic_ext`, P8.2), security labels.
- **Effective privileges** on a table or column (`has_table_privilege`,
  `has_column_privilege`) are left with the other families' to
  P5, as § Schemas says.

## Views: what P4.5 left

- **`Definition` follows the caller's `search_path`**, as every rendered
  expression in § Tables: `pg_get_viewdef` writes `FROM t` for `app.t`
  when `app` is on the path (pinned by `TestLiveViews`). The scripter
  renders it under a fixed path, as § Tables says. It is the non-pretty
  form (`pg_dump`'s), with the leading space and closing semicolon the
  server gives; a properties pane may want `pg_get_viewdef(oid, true)` —
  add a field or a method then, not a second listing.
- **A view's rules.** A view's triggers are `View.Triggers` (P4.9);
  rules other than the view's own `_RETURN` (`pg_rewrite`) are P8.2's, with
  a table's.
- **Not read yet:** whether a view is automatically updatable
  (`pg_relation_is_updatable`, `pg_column_is_updatable`), the column list
  of `CREATE VIEW v (a, b)` beyond what `Columns` shows, recursive views
  (stored as a plain query over a CTE — no flag), a materialized view's
  table access method (`relam`), the query's dependencies (P4.14,
  `pg_depend` via `pg_rewrite`).
- **One view at a time**, as § Tables: each part is a statement per view.

## Sequences: what P4.7 left

- **The unlogged-on-a-standby refusal of `Sequence.LastValue`** is tested
  offline only: the lab has no standby (§ Lab). pgmo never calls
  `pg_sequence_last_value` on one, whatever a release answers there.
- **`LastValue` is the stored value**, not the last one any session handed
  out: a session's `CACHE` runs ahead of it, and nothing in SQL reads
  another session's cache. `log_cnt`/`is_called` read straight from the
  sequence relation need SELECT (USAGE is not enough), so pgmo does not
  read them.
- **Not read:** the sequence's own `ALTER SEQUENCE … OWNED BY` history (only
  the current owner), a sequence's dependants (`DEFAULT nextval(…)` columns
  are P4.14's `pg_depend` reads), logical replication of sequences (19+).
- **A listing for a column**: the column's sequence is found from the
  sequence side (`OwnedBy`); `Column` carries `Identity` but no link back.
  pgMS can match by `OwnedBy` + `OwnedByColumn`; add a field if a caller
  needs the sequence without the listing.

## Types and domains: what P4.8 left

- **A base type's `Default`/`HasDefault`** is tested offline only: no
  pg_catalog type has one, and a base type with a default needs CREATE TYPE
  with C functions (superuser). The column is read as stored.
- **No `ArrayType` field.** The array the server makes beside each type is
  not listed and its name (`_name`, or another when that was taken) is not
  read; a caller needing it reads `typarray` itself. Add a field when one
  does (P5's scripting does not: the array follows its element).
- **Range constructors are functions.** `CREATE TYPE … AS RANGE` makes
  constructor functions named after the range and the multirange (two and
  three); `Database.Functions` lists them as the user's, as psql's `\df`
  does. pgMS may want to group them under the type.
- **Rendered text follows the caller's `search_path`**: a domain's
  `Default` and each `DomainConstraint.Definition`, as every expression
  pgmo reads (§ Tables). The structured names (`BaseType`, `Subtype`,
  `Element`, attribute types, every `ObjectName`) do not.
- **Not read:** an enum's sort-order values (`enumsortorder`, floats — the
  order is in `Labels`), a type's dependants (columns, functions:
  P4.14's `pg_depend` reads), `typndims` (only `pg_dump` cares, for a
  domain over an array, and `BaseType` already spells `[]`).

## Triggers and event triggers: what P4.9 left

- **The WHEN condition is text in `Definition` only.** `pg_get_expr` on
  `tgqual` refuses an expression over OLD and NEW; `HasCondition` says
  whether there is one. Cutting it out of the definition would mean
  parsing the statement; a properties pane shows the definition.
- **`Definition` follows the session**: names by `search_path` (as every
  rendered expression, § Tables) and argument literals by
  `standard_conforming_strings` (off doubles a backslash). The scripter
  renders it under fixed settings (P5.1).
- **Writes (P4.13.3)** leave the event trigger owner's superuser rule to
  the server (42501); pgMS can offer only superusers in its owner picker.
  Not written: `ALTER TRIGGER … DEPENDS ON EXTENSION`, a constraint
  trigger's `FROM referenced_table`.
- **Not read:** a trigger's dependency on the columns its WHEN names
  (`pg_depend`, P4.14); `ALTER TRIGGER … DEPENDS ON EXTENSION` (deptype
  `'x'`; a trigger is then dropped with the extension but stays a user
  object); a constraint trigger's referenced table (`tgconstrrelid`, only
  set by the server's own foreign-key triggers, which are not read).

## Extensions: what P4.10 left

- **Update paths are not read.** `DefaultVersion` differing from `Version`
  does not promise an `ALTER EXTENSION … UPDATE` path, nor does a newer
  listed version; `pg_extension_update_paths(name)` answers that (one call
  per extension, executable by PUBLIC). Add it as an on-demand read when
  pgMS's Update dialog needs it (P8/P4.13.3).
- **Members are identities, not objects** (`ARCHITECTURE.md` §
  Extensions): pgMS maps `Catalog` + `OID` to a family's read where one
  exists. A typed form — `Functions`, `Types`… of an extension — is the
  families' `ObjectFilter.ExtensionMember` plus the name (`Origin.Extension`)
  already.
- **Not read:** an object's `ALTER … DEPENDS ON EXTENSION` (deptype `'x'`:
  dropped with the extension, still a user object — P4.14's dependents);
  the control-file settings the views do not show (`module_pathname`,
  `directory`, `encoding`, `no_relocate` 16+); which extensions require an
  installed one (the reverse of `Requires`, for DROP … CASCADE — `Requires`
  over the listing, inverted in Go).
- **Writes (P4.13.3) leave out** `ALTER EXTENSION … ADD`/`DROP` member
  (an extension author's tool). Who may create one is
  `AvailableExtensionVersion.Superuser`/`Trusted` and CREATE on the
  database — a refusal pgMS can show before sending; pgmo leaves it to the
  server (42501).

## Tablespaces, collations, text search, languages, casts, operators: what P4.11 left

- **A collation's actual version is not read.** `Collation.Version` is
  what was recorded; `pg_collation_actual_version(oid)` asks the provider
  now, and a difference means indexes may need rebuilding (`ALTER
  COLLATION … REFRESH VERSION`). One call per collation, so an on-demand
  read (`Collation.ActualVersion(ctx)`) when pgMS's properties pane wants
  the warning.
- **A cast has no name filter.** `ObjectFilter.Name` takes one column; a
  cast is two type names. pgMS's Casts folder filters in Go, or a later
  filter field takes either type's name.
- **Operator classes and families, access methods, conversions,
  statistics objects, publications and subscriptions** have no family
  yet; extension members of those kinds are named by `Extension.Members`
  alone.
- **Writes:** tablespaces only (P4.13.3); collations, text search
  objects, languages, casts and operators are scripted by P5 from these
  reads and not written.

## Foreign data: what P4.12 left

- **No writes.** CREATE/ALTER/DROP of wrappers, servers, user mappings and
  foreign tables, and `IMPORT FOREIGN SCHEMA`, are not in P4.13's list
  yet; add them there when pgMS's dialogs need them. A user mapping's
  `OPTIONS (password …)` is a secret written in the mapping's database
  (§ Script mode: `execInSecret`), and sent as cleartext — say so in that
  write's doc.
- **`OptionsHidden` restates `pg_user_mappings`' condition** (identical on
  15–18). A release changing the view must change the restatement;
  `TestLiveForeignData` fails on the difference for the four roles it
  reads as, so run it on each new major.
- **A wrapper's handler and validator are names only.** postgres_fdw's
  are in the schema its extension was created in; their signatures are
  fixed by the server (`fdw_handler`, `(text[], oid)`), so no argument
  types are read.
- **No foreign tables per server.** `ForeignServer.ForeignTables` is not
  read; filter `Database.ForeignTables` by `Server` in Go, or add the part
  when the Object Explorer nests them.
- **An option name holding `=`** (a quoted identifier) reads split at
  that `=`, as `pg_options_to_table` reads it.
- **`Database.Tables` lists kinds `r` and `p` only**; a foreign table is a
  `ForeignTable`. A table's `Children` may be one (`Kind`
  `RelationForeignTable`).

## Routines: what P4.6 left

- **Rendered text follows the caller's `search_path`**: `Result`,
  `IdentityArguments`, `RoutineArgument.Default` and `Body`'s definition
  (pinned by `TestLiveRoutines`). The scripter renders under a fixed
  path, as § Tables.
- **Not read:** `TRANSFORM FOR TYPE` (`protrftypes`), a C function's
  `AS 'obj', 'sym'` as fields beyond `ObjectFile` + `Body.Source`, a
  routine's dependencies (P4.14; a SQL-standard body's are in `pg_depend`),
  an aggregate's `HYPOTHETICAL` / `PARALLEL` beyond the kind and
  `Parallel`, the `FINALFUNC_EXTRA` dummy argument types.
- **Event triggers' and triggers' functions** are `Function`s of return
  type `trigger` / `event_trigger`; `Trigger.Function` and
  `EventTrigger.Function` name them (P4.9), but nothing on a `Function`
  flags it as one or lists the triggers calling it — a reverse lookup is
  `Database.Triggers` filtered in Go, or P4.14's dependents.
- **An aggregate by-name read is ~60 ms**, most of it planning its
  thirty-odd joins (the aggregate's eight functions, three types, sort
  operator). Fine for a properties pane; a pgMS hover over many aggregates
  should read the listing once instead.
- **The JIT finding** (ARCHITECTURE.md § Routines) applies to every listing
  that can be long: tables, types and operators of a large database. Not
  re-measured for P4.4/P4.5 (their system listings are a few hundred rows).

## Flaky: `TestDatabasePoolsUnderConcurrency`

`pgmotest`'s concurrency test fails about one run in three (seen 2026-11-21,
during P3.8, with no change to pools or `pgmotest`): a reader's
`DatabaseQuerier(db).Query` fails `closed pool` (`pgmotest_test.go:537`) —
its database's Fake was evicted and closed between the lease and the
statement, or the Fake reopens late. Either the soft cap evicts an entry a
statement is about to use (a pgmo bug, `database_pools.go` `lease`/`evict`)
or the Fake's reopen races its close. Not investigated yet.
Seen again 2026-11-21 during P4.1 (2 of 8 runs under `-race`).

## Object writes: what P4.13.3 left

- **Not written** (each a candidate when a pgMS dialog needs it):
  - tables: `SET TABLESPACE`, `SET LOGGED/UNLOGGED`, `SET (…)`/`RESET (…)`
    storage parameters after the create, `ATTACH`/`DETACH PARTITION`,
    `INHERIT`/`NO INHERIT`, `ALTER COLUMN … ADD/SET/DROP IDENTITY` and an
    identity's sequence options, `SET EXPRESSION` (17+), `SET COMPRESSION`,
    `ADD CONSTRAINT … USING INDEX`, `CREATE TABLE … LIKE`/`OF type`/`IF NOT
    EXISTS`, temporary tables (a pool's session ends at random), `USING`
    a table access method. (Row-level security: P4.13.4, `policy_write.go`.)
  - indexes: `REINDEX` (maintenance, P10), `ALTER INDEX … SET TABLESPACE`,
    `SET (…)`, `ATTACH PARTITION`, `ALTER COLUMN … SET STATISTICS`.
  - views: `ALTER VIEW … SET (…)`/`ALTER COLUMN … SET DEFAULT`; a
    materialized view's `USING` access method, `SET TABLESPACE`.
  - routines: `ALTER FUNCTION`'s partial forms (volatility, cost,
    `SET`/`RESET`) — a redefinition goes through `OrReplace`, which resets
    every attribute left unset; a SQL-standard body (`BEGIN ATOMIC`, 14+;
    `Body` is always dollar-quoted, the `AS` form); C functions (`AS 'obj',
    'symbol'`), `SUPPORT`, `TRANSFORM`, `WINDOW`; aggregates (no writes).
  - types: composite `ADD`/`DROP`/`ALTER ATTRIBUTE`, `ALTER TYPE … SET
    (…)`, base and shell types (an extension's).
  - sequences: `SET LOGGED/UNLOGGED`, `NO MINVALUE`/`NO MAXVALUE` (nil leaves
    a value as it is; there is no way to ask for the type's bound again).
  - the families with reads and no writes: collations, text search
    objects, languages, casts, operators, foreign data (P4.12).
- **An overloaded routine's create returns its handle**, not the read
  object (`unlessAmbiguous`): matching the request's type text to
  `ArgumentTypeNames` would need the server's resolution of each type name
  (`'int'::regtype`), under the creating session's `search_path`. A
  `to_regprocedure` read of the Signature would do it; not worth a read
  until a caller needs the OID at once.
- **`CreateIndex` and `AddConstraint` return only an error**: an index and
  a constraint are parts, read with their relation's `Indexes` and
  `Constraints` (no `IndexByName`). With `Name` empty the server's chosen
  name is unknown until that read.
- **A failed `CREATE INDEX CONCURRENTLY` leaves an invalid index**
  (`Index.Valid` false); pgmo does not drop it. pgMS should offer to.
- **`SetDefault`, `AlterColumn(Default)` mirror the text as given**; the
  server stores its own rendering (`'x'::text` for `'x'`). Read again to
  compare.

## Privilege writes: what P4.13.4 left

- **No effective privileges.** `Privileges` is the ACL as stored; what a
  role can do through membership is `has_*_privilege` per object and
  privilege (or `aclexplode` joined to the role's transitive memberships).
  pgMS's permission pages (P8) will want one read per object kind for a
  role; add it to P5 with the scripter's ACL rendering.
- **ACLs not read yet:** a database's (`datacl`), a parameter's
  (`pg_parameter_acl`, which has no other read), a large object's
  (`pg_largeobject_metadata`; large objects have no family). Each is written
  by `ApplyPrivileges`; `TestLivePrivilegeWrites` reads them by hand.
- **One securable per statement.** GRANT takes a list (`ON TABLE a, b`);
  `ApplyPrivileges` writes one, or ALL … IN SCHEMA. A pgMS multi-select
  grant is one call per object — inside `InTransaction` to be atomic.
- **Not offered:** `GRANTED BY` (15–18 accept only the current user,
  0A000); `REVOKE … RESTRICT` (the default, written by omission); a role
  spec keyword (`CURRENT_USER`, `SESSION_USER`, `CURRENT_ROLE`) — a
  grantee is a name, quoted, so pgMS resolves the current role itself;
  `ALTER DEFAULT PRIVILEGES … ON ROUTINES` (the same `f` entry as
  FUNCTIONS, which is what pg_dump writes).
- **A revoke back to the default leaves an explicit ACL** (`{owner=…}`)
  where the catalog held NULL: `PrivilegesDefault` turns false though the
  privileges are the same. That is the server's; pgmo cannot write NULL.
  A pgMS permissions dialog comparing "changed from default" should compare
  the privileges, not the flag.
- **Policies:** no `Database.Policies` listing over every table (pgMS's
  Object Explorer reads them per table); `Alter` mirrors nothing (the
  server renders); a policy's command or AS RESTRICTIVE is changed by drop
  and create — a pgMS edit dialog should do both in one `InTransaction`.

## Dependencies and search: what P4.14 left

- **Definition search is case-sensitive.** `SearchDefinitions` matches
  `strpos` as written: SQL `lower()` is refused by
  `TestNoCaseFoldingOfNames` (a SQL literal has no allowlist), and under
  the catalog's "C" collation it would fold ASCII alone anyway. A pgMS
  "match case" off needs either an allowlist for SQL literals whose text is
  not a name, or `ILIKE` with escaping — decide with the author.
- **Rendered definitions follow the session's `search_path`**
  (`pg_get_viewdef`, `pg_get_function_sqlbody`), so a table on the path is
  searched unqualified — the same helper P4.4's open thread wants
  (`SET LOCAL search_path` in a read-only transaction).
- **Not searched:** indexes, constraints, policies (a table's parts),
  rules, a trigger's WHEN, check and default expressions; the families
  without a schema (event triggers, extensions, languages, casts, FDWs and
  servers) and the schema-scoped ones without a database listing in the
  union (collations, text search, operators). Add a branch when pgMS's
  search needs one; the union's columns stay the same.
- **No column-level reads.** `ObjectAddress` has no sub-ID: a column's
  dependencies are its table's, told apart by `OwnPart`. Add `SubID` if
  pgMS offers Dependencies on a column node.
- **Not folded:** a publication's relation and schema rows
  (`pg_publication_rel`, `pg_publication_namespace`: automatic edges to two
  owners — they come back as dependents of the table and the
  publication), extended statistics (an object of its own with a name).
- **Dependency order for scripting** (P5.3) needs raw edges, not folded
  ones — `pg_dump`'s own sort; P5 adds its read.
- **`REASSIGN OWNED`/`DROP OWNED` take names only** — no `CURRENT_USER`
  keyword, as grantees (§ Privilege writes).
