//go:build livedb

// Live version sweep: call every read pgmo sends against the connected server
// and report the ones it rejects — gosmo's TestLiveVersionSweep.
//
// A query naming a column or function the server does not have fails the
// whole read, and nothing in an offline test says so: the fake answers
// whatever pgmo asks. The sweep is the standing check: run it on pg15, the
// oldest lab cluster, after any query change (PLAN.md P3.8, P15.1.4).
//
//	go test -tags livedb . -run '^TestLiveVersionSweep$' -v -livedb 'service=pg15'
//
// It creates one throwaway database holding one of each object kind the
// families will read, and sweeps it twice: as the connected role, and as a
// plain role with no privileges (SET LOCAL ROLE, a transaction per call), the
// visibility pgMS's least-privileged user has. Then it drops both.
//
// Which reads exist, and so what must be swept, is held offline by
// versionsweep_reflect_test.go: every function that sends a read is in
// sweepReaders, every exported context-first method is swept or says why not,
// and sweepMustCall lists what checkCoverage confirms was called. A sweep of
// 0 failures is not proof on its own — the -v log lists every label called.
package pgmo

import (
	"context"
	"errors"
	"fmt"
	"maps"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/radix29/pgmo/pgsql"
)

// sweepSchema is one of each object kind the sweep's reads (and the P4
// families' to come) have a row for. A read that finds nothing can succeed on
// a version its query cannot even run on, so each statement gives some read a
// row to return.
var sweepSchema = []string{
	`CREATE SCHEMA app`,
	`CREATE EXTENSION citext SCHEMA app`,
	`CREATE EXTENSION postgres_fdw`,
	`CREATE TABLE app.parent (id integer PRIMARY KEY, name app.citext NOT NULL UNIQUE, amount numeric(18,2))`,
	`CREATE TABLE app.child (id integer PRIMARY KEY, parent_id integer NOT NULL REFERENCES app.parent,
	   note text, CONSTRAINT child_id_positive CHECK (id > 0))`,
	`CREATE INDEX child_parent ON app.child (parent_id) INCLUDE (note)`,
	`CREATE TABLE app.part (id integer, at date NOT NULL) PARTITION BY RANGE (at)`,
	`CREATE TABLE app.part_2026 PARTITION OF app.part FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')`,
	`CREATE UNLOGGED TABLE app.scratch (id integer)`,
	`CREATE VIEW app.parent_names AS SELECT id, name FROM app.parent`,
	`CREATE MATERIALIZED VIEW app.parent_totals AS SELECT pg_catalog.count(*) AS n FROM app.parent`,
	`CREATE UNIQUE INDEX parent_totals_n ON app.parent_totals (n)`,
	`ALTER VIEW app.parent_names SET (security_barrier = on)`,
	// The P4.12 families: a commented server with an option, PUBLIC's user
	// mapping on it (its options withheld from the plain role), and a
	// foreign table with a table option, a column option and a CHECK.
	`CREATE SERVER sweep_remote FOREIGN DATA WRAPPER postgres_fdw OPTIONS (host 'sweep.invalid')`,
	`COMMENT ON SERVER sweep_remote IS 'sweep'`,
	`CREATE USER MAPPING FOR PUBLIC SERVER sweep_remote OPTIONS (user 'sweep')`,
	`CREATE FOREIGN TABLE app.remote (id integer OPTIONS (column_name 'ID') CHECK (id > 0)) SERVER sweep_remote
	   OPTIONS (table_name 'r')`,
	`CREATE SEQUENCE app.seq AS integer`,
	// A sequence owned by a column, called once (its CACHE stores the fifth
	// value), with a comment, and readable by PUBLIC, so the plain role's
	// pass reads its last value where app.seq's is refused.
	`CREATE SEQUENCE app.counter AS smallint INCREMENT BY -2 CACHE 5 CYCLE OWNED BY app.child.id`,
	`SELECT pg_catalog.nextval('app.counter')`,
	`GRANT USAGE ON SEQUENCE app.counter TO PUBLIC`,
	`COMMENT ON SEQUENCE app.counter IS 'sweep'`,
	`CREATE TYPE app.mood AS ENUM ('ok', 'not ok')`,
	`CREATE TYPE app.pair AS (a integer, b text)`,
	`CREATE DOMAIN app.positive AS integer CHECK (VALUE > 0)`,
	`CREATE COLLATION app.sweep_c FROM "C"`,
	// A range with a named multirange, a commented enum, and a domain with
	// a collation, a default and NOT NULL (a constraint row of its own from
	// 17) granted to PUBLIC.
	`CREATE TYPE app.span AS RANGE (subtype = integer, multirange_type_name = app.spans)`,
	`COMMENT ON TYPE app.mood IS 'sweep'`,
	`CREATE DOMAIN app.code AS varchar(8) COLLATE app.sweep_c DEFAULT 'x' NOT NULL`,
	`GRANT USAGE ON DOMAIN app.code TO PUBLIC`,
	// Overloads, so routineByName meets ErrAmbiguous as well as a match.
	`CREATE FUNCTION app.f(x integer) RETURNS integer LANGUAGE sql AS 'SELECT x'`,
	`CREATE FUNCTION app.f(x app.mood) RETURNS integer LANGUAGE sql AS 'SELECT 1'`,
	`CREATE PROCEDURE app.p(INOUT a integer) LANGUAGE sql AS 'SELECT a'`,
	`CREATE AGGREGATE app.ag(*) (sfunc = pg_catalog.int8inc, stype = bigint, initcond = '0')`,
	// The P4.11 families: a commented collation (app.sweep_c, above); a
	// text search parser and template over pg_catalog's C functions, a
	// dictionary with options, and a commented configuration mapping two
	// token types, one to two dictionaries; a commented language over
	// plpgsql's handler; a commented cast by function; an infix operator
	// with a negator (a shell operator) and a prefix one.
	`COMMENT ON COLLATION app.sweep_c IS 'sweep'`,
	`CREATE TEXT SEARCH PARSER app.sweep_prs (START = prsd_start, GETTOKEN = prsd_nexttoken, END = prsd_end, LEXTYPES = prsd_lextype)`,
	`CREATE TEXT SEARCH TEMPLATE app.sweep_tmpl (LEXIZE = dsimple_lexize)`,
	`CREATE TEXT SEARCH DICTIONARY app.sweep_dict (TEMPLATE = pg_catalog.simple, STOPWORDS = english)`,
	`CREATE TEXT SEARCH CONFIGURATION app.sweep_cfg (PARSER = app.sweep_prs)`,
	`ALTER TEXT SEARCH CONFIGURATION app.sweep_cfg ADD MAPPING FOR asciiword WITH app.sweep_dict, pg_catalog.english_stem`,
	`ALTER TEXT SEARCH CONFIGURATION app.sweep_cfg ADD MAPPING FOR int WITH pg_catalog.simple`,
	`COMMENT ON TEXT SEARCH CONFIGURATION app.sweep_cfg IS 'sweep'`,
	`CREATE LANGUAGE sweep_pl HANDLER plpgsql_call_handler`,
	`COMMENT ON LANGUAGE sweep_pl IS 'sweep'`,
	`CREATE CAST (app.mood AS integer) WITH FUNCTION app.f(app.mood) AS ASSIGNMENT`,
	`COMMENT ON CAST (app.mood AS integer) IS 'sweep'`,
	`CREATE FUNCTION app.eqm(integer, app.mood) RETURNS boolean LANGUAGE sql AS 'SELECT true'`,
	`CREATE OPERATOR app.=== (LEFTARG = integer, RIGHTARG = app.mood, FUNCTION = app.eqm,
	   NEGATOR = OPERATOR(app.@@@), RESTRICT = eqsel, HASHES)`,
	`CREATE OPERATOR app.!! (RIGHTARG = integer, FUNCTION = app.f)`,
	`COMMENT ON OPERATOR app.=== (integer, app.mood) IS 'sweep'`,
	`CREATE FUNCTION app.touch() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'`,
	`CREATE TRIGGER child_touch BEFORE INSERT ON app.child FOR EACH ROW EXECUTE FUNCTION app.touch()`,
	// A view's INSTEAD OF trigger with arguments, and a commented event
	// trigger narrowed to one command tag.
	`CREATE TRIGGER names_update INSTEAD OF UPDATE ON app.parent_names FOR EACH ROW EXECUTE FUNCTION app.touch('a', '')`,
	`CREATE FUNCTION app.on_ddl() RETURNS event_trigger LANGUAGE plpgsql AS 'BEGIN END'`,
	`CREATE EVENT TRIGGER sweep_ddl ON ddl_command_end WHEN TAG IN ('create table') EXECUTE FUNCTION app.on_ddl()`,
	`COMMENT ON EVENT TRIGGER sweep_ddl IS 'sweep'`,
	`COMMENT ON EXTENSION citext IS 'sweep'`,
	`CREATE STATISTICS app.parent_stats ON id, amount FROM app.parent`,
	`ALTER TABLE app.child ENABLE ROW LEVEL SECURITY`,
	`CREATE POLICY child_all ON app.child USING (true)`,
	`CREATE PUBLICATION sweep_pub FOR TABLE app.parent`,
	`CREATE RULE parent_names_insert AS ON INSERT TO app.parent_names DO INSTEAD NOTHING`,
	`COMMENT ON TABLE app.parent IS 'sweep'`,
	// A schema ACL, a comment and default privileges — in app and
	// database-wide — for the schema reads, granted to a predefined role so
	// the plain role's pass stays privilege-less. Last, so the REVOKE
	// changes no function the fixture creates.
	`COMMENT ON SCHEMA app IS 'sweep'`,
	`GRANT USAGE ON SCHEMA app TO pg_monitor WITH GRANT OPTION`,
	`ALTER DEFAULT PRIVILEGES IN SCHEMA app GRANT SELECT ON TABLES TO pg_monitor`,
	`ALTER DEFAULT PRIVILEGES REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC`,
	`INSERT INTO app.parent VALUES (1, 'One', 1.00), (2, 'two', 2.00)`,
	`INSERT INTO app.child VALUES (1, 1, 'a'), (2, 2, 'b')`,
	`ANALYZE`,
}

func TestLiveVersionSweep(t *testing.T) {
	checkSweepKeys(t)

	admin, ctx, done := liveDBTimeout(t, 10*time.Minute)
	t.Cleanup(done) // registered first, so it runs after the drops
	stamp := time.Now().UnixNano()
	scratch := fmt.Sprintf("pgmo_live_sweep_%d", stamp)
	plain := fmt.Sprintf("pgmo_live_sweep_plain_%d", stamp)
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgsql.QuoteIdent(scratch)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgsql.QuoteIdent(scratch)+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database: %v", err)
		}
	})
	// A database-wide setting and a comment, so the database reads have one
	// of each to return.
	for _, stmt := range []string{
		"ALTER DATABASE " + pgsql.QuoteIdent(scratch) + " SET work_mem = '8MB'",
		"COMMENT ON DATABASE " + pgsql.QuoteIdent(scratch) + " IS 'sweep'",
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := admin.Exec(ctx, "CREATE ROLE "+pgsql.QuoteIdent(plain)+" NOLOGIN"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP ROLE IF EXISTS "+pgsql.QuoteIdent(plain)); err != nil {
			t.Errorf("drop role: %v", err)
		}
	})
	// A group plain is a member of, a setting of plain's in every database
	// and one in the scratch database, and a comment, so the role reads have
	// one of each to return. The group holds no privileges: plain stays the
	// unprivileged role its pass needs.
	group := fmt.Sprintf("pgmo_live_sweep_group_%d", stamp)
	if _, err := admin.Exec(ctx, "CREATE ROLE "+pgsql.QuoteIdent(group)+" NOLOGIN"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP ROLE IF EXISTS "+pgsql.QuoteIdent(group)); err != nil {
			t.Errorf("drop role: %v", err)
		}
	})
	for _, stmt := range []string{
		"GRANT " + pgsql.QuoteIdent(group) + " TO " + pgsql.QuoteIdent(plain),
		"ALTER ROLE " + pgsql.QuoteIdent(plain) + " SET work_mem = '8MB'",
		"ALTER ROLE " + pgsql.QuoteIdent(plain) + " IN DATABASE " + pgsql.QuoteIdent(scratch) + " SET work_mem = '4MB'",
		"COMMENT ON ROLE " + pgsql.QuoteIdent(plain) + " IS 'sweep'",
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}

	srv, err := Connect(ctx, ConnectionOptions{
		ConnString: *liveConnString, Database: scratch, ApplicationName: "pgmo - live version sweep",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(srv.Close) // before the drops: the database's sessions end first
	t.Logf("server %s (server_version_num %d)", srv.Version(), int(srv.Version()))

	for _, stmt := range sweepSchema {
		if _, err := srv.Pool().Exec(ctx, stmt); err != nil {
			// A fixture statement the server rejects is itself a version
			// finding, but the sweep still has to run.
			t.Errorf("scratch schema %.70q: %v", strings.Join(strings.Fields(stmt), " "), err)
		}
	}

	db, err := srv.DatabaseByName(ctx, scratch)
	if err != nil {
		t.Fatal(err)
	}
	plainRole, err := srv.RoleByName(ctx, plain)
	if err != nil {
		t.Fatal(err)
	}
	child, err := db.TableByName(ctx, "app", "child")
	if err != nil {
		t.Fatal(err)
	}
	view, err := db.ViewByName(ctx, "app", "parent_names")
	if err != nil {
		t.Fatal(err)
	}
	matview, err := db.MaterializedViewByName(ctx, "app", "parent_totals")
	if err != nil {
		t.Fatal(err)
	}
	counter, err := db.SequenceByName(ctx, "app", "counter")
	if err != nil {
		t.Fatal(err)
	}
	mood, err := db.TypeByName(ctx, "app", "mood")
	if err != nil {
		t.Fatal(err)
	}
	code, err := db.DomainByName(ctx, "app", "code")
	if err != nil {
		t.Fatal(err)
	}
	touch, err := db.FunctionByName(ctx, "app", "touch", nil)
	if err != nil {
		t.Fatal(err)
	}
	proc, err := db.ProcedureByName(ctx, "app", "p", nil)
	if err != nil {
		t.Fatal(err)
	}
	agg, err := db.AggregateByName(ctx, "app", "ag", nil)
	if err != nil {
		t.Fatal(err)
	}
	trig, err := db.TriggerByName(ctx, "app", "parent_names", "names_update")
	if err != nil {
		t.Fatal(err)
	}
	evt, err := db.EventTriggerByName(ctx, "sweep_ddl")
	if err != nil {
		t.Fatal(err)
	}
	citext, err := db.ExtensionByName(ctx, "citext")
	if err != nil {
		t.Fatal(err)
	}
	pgDefault, err := srv.TablespaceByName(ctx, "pg_default")
	if err != nil {
		t.Fatal(err)
	}
	sweepC, err := db.CollationByName(ctx, "app", "sweep_c")
	if err != nil {
		t.Fatal(err)
	}
	tsParser, err := db.TextSearchParserByName(ctx, "app", "sweep_prs")
	if err != nil {
		t.Fatal(err)
	}
	tsTemplate, err := db.TextSearchTemplateByName(ctx, "app", "sweep_tmpl")
	if err != nil {
		t.Fatal(err)
	}
	tsDict, err := db.TextSearchDictionaryByName(ctx, "app", "sweep_dict")
	if err != nil {
		t.Fatal(err)
	}
	tsConfig, err := db.TextSearchConfigurationByName(ctx, "app", "sweep_cfg")
	if err != nil {
		t.Fatal(err)
	}
	lang, err := db.LanguageByName(ctx, "sweep_pl")
	if err != nil {
		t.Fatal(err)
	}
	cast, err := db.CastByTypes(ctx, mood.OID, 23)
	if err != nil {
		t.Fatal(err)
	}
	op, err := db.OperatorByName(ctx, "app", "===", 23, mood.OID)
	if err != nil {
		t.Fatal(err)
	}
	fdw, err := db.ForeignDataWrapperByName(ctx, "postgres_fdw")
	if err != nil {
		t.Fatal(err)
	}
	remoteServer, err := db.ForeignServerByName(ctx, "sweep_remote")
	if err != nil {
		t.Fatal(err)
	}
	mapping, err := db.UserMappingByName(ctx, "sweep_remote", "")
	if err != nil {
		t.Fatal(err)
	}
	remote, err := db.ForeignTableByName(ctx, "app", "remote")
	if err != nil {
		t.Fatal(err)
	}

	for _, role := range []string{"", plain} {
		name := "connected role"
		if role != "" {
			name = "plain role"
		}
		t.Run(name, func(t *testing.T) {
			sw := &sweep{t: t, ctx: ctx, srv: srv, role: role}
			for typeName, recv := range map[string]any{"Server": srv, "Database": db, "Role": plainRole, "Table": child,
				"View": view, "MaterializedView": matview, "Sequence": counter, "Type": mood, "Domain": code, "Function": touch, "Procedure": proc, "Aggregate": agg,
				"Trigger": trig, "EventTrigger": evt, "Extension": citext,
				"Tablespace": pgDefault, "Collation": sweepC, "TextSearchParser": tsParser, "TextSearchTemplate": tsTemplate,
				"TextSearchDictionary": tsDict, "TextSearchConfiguration": tsConfig, "Language": lang, "Cast": cast, "Operator": op,
				"ForeignDataWrapper": fdw, "ForeignServer": remoteServer, "UserMapping": mapping, "ForeignTable": remote} {
				sw.reflectSweep(typeName, scratch, recv)
			}
			sweepServerReads(sw, scratch)
			sweepDatabases(sw, scratch)
			sweepRoles(sw, scratch, plain, group)
			sweepSchemas(sw, db)
			sweepTables(sw, db)
			sweepViews(sw, db)
			sweepSequences(sw, db)
			sweepTypes(sw, db)
			sweepTriggers(sw, db)
			sweepExtensions(sw, db)
			sweepTablespaces(sw)
			sweepCatalogObjects(sw, db, mood.OID)
			sweepTextSearch(sw, db)
			sweepForeignData(sw, db)
			sweepRoutineFamilies(sw, db)
			sweepRoutines(sw)
			sweepFragments(sw)
			sweepDependencies(sw, db, plainRole)
			sw.checkCoverage()

			slices.Sort(sw.failed)
			slices.Sort(sw.refused)
			t.Logf("%d calls made, %d failed, %d refused as too new for this server", sw.calls, len(sw.failed), len(sw.refused))
			t.Logf("called: %s", strings.Join(slices.Sorted(maps.Keys(sw.called)), ", "))
			for _, r := range sw.refused {
				t.Logf("refused (expected): %s", r)
			}
			for _, f := range sw.failed {
				t.Errorf("%s", f)
			}
		})
	}
}

// sweepServerReads drives the Server reads with an assertion on their rows
// (the reflective half only sees the error), the connect-time version read,
// and the SCRAM iteration read a password write opens with.
func sweepServerReads(sw *sweep, scratch string) {
	sw.call("NewServer(on the sweep's connection)", func(ctx context.Context) error {
		// Over a connection of the call's own, which Close gives back rather
		// than closing the sweep's pool: the transaction under a role (left
		// open), else one acquired from the pool (released).
		q := sw.srv.on(ctx)
		if sw.role == "" {
			conn, err := sw.srv.Pool().Acquire(ctx)
			if err != nil {
				return err
			}
			q = conn
		}
		s, err := NewServer(ctx, q)
		if err != nil {
			return err
		}
		defer s.Close()
		if s.Version() != sw.srv.Version() || s.CurrentDatabase() != scratch {
			return fmt.Errorf("read %s in %q, want %s in %q", s.Version(), s.CurrentDatabase(), sw.srv.Version(), scratch)
		}
		return nil
	})
	sw.call("Server.Info(version)", func(ctx context.Context) error {
		i, err := sw.srv.Info(ctx)
		if err == nil && (i.Version != sw.srv.Version() || i.Banner == "" || i.StartTime.IsZero()) {
			err = fmt.Errorf("Version %s, Banner %q, StartTime %v — want %s and both set", i.Version, i.Banner, i.StartTime, sw.srv.Version())
		}
		return err
	})
	sw.call("Server.Capabilities(role)", func(ctx context.Context) error {
		c, err := sw.srv.Capabilities(ctx)
		if err == nil && (c.Role == "" || !c.Probed()) {
			err = fmt.Errorf("Role %q, Probed %v — want the session's role, probed", c.Role, c.Probed())
		}
		if err == nil && sw.role != "" && (c.Role != sw.role || c.Superuser) {
			err = fmt.Errorf("Role %q, Superuser %v — want %q, not superuser", c.Role, c.Superuser, sw.role)
		}
		return err
	})
	sw.call("Server.DatabasePrivileges(scratch)", func(ctx context.Context) error {
		m, err := sw.srv.DatabasePrivileges(ctx)
		if err == nil && !m[scratch].CanConnect() {
			err = fmt.Errorf("%q: %+v, want one the role can connect to", scratch, m[scratch])
		}
		return err
	})
	sw.call("Server.passwordVerifier", func(ctx context.Context) error {
		v, err := sw.srv.passwordVerifier(ctx, "sweep")
		if err == nil && !strings.HasPrefix(v, "SCRAM-SHA-256$") {
			err = fmt.Errorf("verifier %.20q…, want SCRAM-SHA-256$…", v)
		}
		return err
	})
}

// sweepDatabases drives the database reads with an assertion on their rows:
// the listing narrowed to the scratch database, the by-name read of it and
// of template0, and the size of template0 — a database that takes no
// connections, read through the Server's own as every database read is.
func sweepDatabases(sw *sweep, scratch string) {
	check := func(d *Database) error {
		switch {
		case d.Name != scratch || d.OID == 0 || d.Owner == "" || d.Encoding == "" || d.Tablespace == "":
			return fmt.Errorf("%+v: want the scratch database with its owner, encoding and tablespace", d)
		case d.LocaleProvider.String() != "libc" && d.LocaleProvider.String() != "icu" && d.LocaleProvider.String() != "builtin":
			return fmt.Errorf("LocaleProvider %v: not one pgmo knows", d.LocaleProvider)
		case d.Comment != "sweep" || len(d.Settings) != 1 || d.Settings[0] != (ParameterSetting{"work_mem", "8MB"}):
			return fmt.Errorf("Comment %q, Settings %v: want sweep, [work_mem=8MB]", d.Comment, d.Settings)
		case d.IsTemplate || !d.AllowConnections || d.ConnectionLimit != -1:
			return fmt.Errorf("IsTemplate %v, AllowConnections %v, ConnectionLimit %d: want a plain database",
				d.IsTemplate, d.AllowConnections, d.ConnectionLimit)
		}
		return nil
	}
	sw.call("Server.Databases(scratch)", func(ctx context.Context) error {
		ds, err := sw.srv.Databases(ctx, ObjectFilter{Name: []TextCriterion{{TextEquals, pgsql.QuoteIdent(scratch)}}})
		if err != nil {
			return err
		}
		if len(ds) != 1 {
			return fmt.Errorf("%d databases, want the scratch one alone", len(ds))
		}
		return check(ds[0])
	})
	sw.call("Server.DatabaseByName(scratch)", func(ctx context.Context) error {
		d, err := sw.srv.DatabaseByName(ctx, scratch)
		if err != nil {
			return err
		}
		return check(d)
	})
	sw.call("Server.DatabaseByName(template0)", func(ctx context.Context) error {
		d, err := sw.srv.DatabaseByName(ctx, "template0")
		if err == nil && (!d.IsTemplate || d.AllowConnections) {
			err = fmt.Errorf("IsTemplate %v, AllowConnections %v: want a template taking no connections", d.IsTemplate, d.AllowConnections)
		}
		return err
	})
	sw.call("Database.Size(template0)", func(ctx context.Context) error {
		n, err := sw.srv.DatabaseRef("template0").Size(ctx)
		if err == nil && n <= 0 {
			err = fmt.Errorf("size %d, want > 0", n)
		}
		return err
	})
}

// sweepRoles drives the role reads with an assertion on their rows: the
// listing narrowed to the plain role, the by-name read of it, and of
// pg_monitor — predefined, with members of its own.
func sweepRoles(sw *sweep, scratch, plain, group string) {
	check := func(r *Role) error {
		want := RoleMembership{Role: group, Member: plain, Admin: false, Inherit: true, Set: true}
		switch {
		case r.Name != plain || r.OID == 0 || r.IsPredefined() || r.CanLogin || r.Superuser || !r.Inherit:
			return fmt.Errorf("%+v: want the plain NOLOGIN role", r)
		case len(r.Settings) != 1 || r.Settings[0] != (ParameterSetting{"work_mem", "8MB"}):
			return fmt.Errorf("Settings %v: want [work_mem=8MB]", r.Settings)
		case len(r.DatabaseSettings) != 1 || r.DatabaseSettings[0].Database != scratch ||
			fmt.Sprint(r.DatabaseSettings[0].Settings) != "[{work_mem 4MB}]":
			return fmt.Errorf("DatabaseSettings %v: want work_mem=4MB in %s", r.DatabaseSettings, scratch)
		case len(r.MemberOf) != 1 || r.Members != nil || r.MemberOf[0].Grantor == "":
			return fmt.Errorf("MemberOf %v, Members %v: want one grant of %s, with its grantor", r.MemberOf, r.Members, group)
		case func() bool { m := r.MemberOf[0]; m.Grantor = ""; return m != want }():
			return fmt.Errorf("MemberOf %+v: want %+v", r.MemberOf[0], want)
		case r.Comment != "sweep" || !r.ValidUntil.IsZero() || r.ConnectionLimit != -1:
			return fmt.Errorf("Comment %q, ValidUntil %v, ConnectionLimit %d", r.Comment, r.ValidUntil, r.ConnectionLimit)
		}
		return nil
	}
	sw.call("Server.Roles(plain)", func(ctx context.Context) error {
		rs, err := sw.srv.Roles(ctx, ObjectFilter{Name: []TextCriterion{{TextEquals, pgsql.QuoteIdent(plain)}}})
		if err != nil {
			return err
		}
		if len(rs) != 1 {
			return fmt.Errorf("%d roles, want the plain one alone", len(rs))
		}
		return check(rs[0])
	})
	sw.call("Server.RoleByName(plain)", func(ctx context.Context) error {
		r, err := sw.srv.RoleByName(ctx, plain)
		if err != nil {
			return err
		}
		return check(r)
	})
	sw.call("Server.RoleByName(pg_monitor)", func(ctx context.Context) error {
		r, err := sw.srv.RoleByName(ctx, "pg_monitor")
		if err == nil && (!r.IsPredefined() || len(r.MemberOf) < 3) {
			err = fmt.Errorf("IsPredefined %v, MemberOf %v: want predefined, a member of pg_read_all_settings, pg_read_all_stats, pg_stat_scan_tables",
				r.IsPredefined(), r.MemberOf)
		}
		return err
	})
}

// sweepSchemas drives the schema reads with an assertion on their rows: the
// listing narrowed to app, the by-name read of it and of public — owned by
// pg_database_owner, its ACL set at creation — and the database's default
// privileges, one entry database-wide and one in app.
func sweepSchemas(sw *sweep, db *Database) {
	check := func(s *Schema) error {
		o := s.Owner
		wantACL := []PrivilegeGrant{{o, o, PrivilegeUsage, false}, {o, o, PrivilegeCreate, false},
			{"pg_monitor", o, PrivilegeUsage, true}}
		wantDefault := []DefaultPrivileges{{Role: o, Schema: "app", ObjectType: DefaultPrivilegeTables,
			Grants: []PrivilegeGrant{{"pg_monitor", o, PrivilegeSelect, false}}}}
		switch {
		case s.Name != "app" || s.OID == 0 || o == "" || s.Origin != (Origin{}) || s.Database() != db:
			return fmt.Errorf("%+v: want the user schema app with its owner, read from %s", s, db.Name)
		case s.PrivilegesDefault || !reflect.DeepEqual(s.Privileges, wantACL):
			return fmt.Errorf("PrivilegesDefault %v, Privileges %+v: want %+v", s.PrivilegesDefault, s.Privileges, wantACL)
		case !reflect.DeepEqual(s.DefaultPrivileges, wantDefault) || s.Comment != "sweep":
			return fmt.Errorf("DefaultPrivileges %+v, Comment %q: want %+v, sweep", s.DefaultPrivileges, s.Comment, wantDefault)
		}
		return nil
	}
	sw.call("Database.Schemas(app)", func(ctx context.Context) error {
		ss, err := db.Schemas(ctx, ObjectFilter{Name: []TextCriterion{{TextEquals, "app"}}})
		if err != nil {
			return err
		}
		if len(ss) != 1 {
			return fmt.Errorf("%d schemas, want app alone", len(ss))
		}
		return check(ss[0])
	})
	sw.call("Database.SchemaByName(app)", func(ctx context.Context) error {
		s, err := db.SchemaByName(ctx, "app")
		if err != nil {
			return err
		}
		return check(s)
	})
	sw.call("Database.SchemaByName(public)", func(ctx context.Context) error {
		s, err := db.SchemaByName(ctx, "public")
		if err == nil && (s.Owner != "pg_database_owner" || s.PrivilegesDefault ||
			!slices.Contains(s.Privileges, PrivilegeGrant{"", "pg_database_owner", PrivilegeUsage, false})) {
			err = fmt.Errorf("Owner %q, PrivilegesDefault %v, Privileges %+v: want pg_database_owner's, PUBLIC holding USAGE",
				s.Owner, s.PrivilegesDefault, s.Privileges)
		}
		return err
	})
	sw.call("Database.DefaultPrivileges(scratch)", func(ctx context.Context) error {
		ds, err := db.DefaultPrivileges(ctx)
		if err != nil {
			return err
		}
		if len(ds) != 2 {
			return fmt.Errorf("%+v: want two entries", ds)
		}
		o := ds[0].Role
		want := []DefaultPrivileges{
			{Role: o, ObjectType: DefaultPrivilegeFunctions, Grants: []PrivilegeGrant{{o, o, PrivilegeExecute, false}}},
			{Role: o, Schema: "app", ObjectType: DefaultPrivilegeTables, Grants: []PrivilegeGrant{{"pg_monitor", o, PrivilegeSelect, false}}},
		}
		if !reflect.DeepEqual(ds, want) {
			return fmt.Errorf("%+v, want %+v", ds, want)
		}
		return nil
	})
}

// sweepTables drives the table reads with an assertion on their rows: the
// listing narrowed to app's tables, the by-name read of app.child, and each
// read of a table's parts on the fixture table that has one — app.parent's
// columns and comment, app.child's keys, index, trigger and policy,
// app.part's partition and size.
func sweepTables(sw *sweep, db *Database) {
	byName := func(ctx context.Context, name string) (*Table, error) {
		return db.TableByName(ctx, "app", name)
	}
	sw.call("Database.Tables(app)", func(ctx context.Context) error {
		ts, err := db.Tables(ctx, ObjectFilter{Schema: []TextCriterion{{TextEquals, "app"}}})
		if err != nil {
			return err
		}
		var names []string
		for _, t := range ts {
			names = append(names, t.Name)
		}
		if want := []string{"child", "parent", "part", "part_2026", "scratch"}; !slices.Equal(names, want) {
			return fmt.Errorf("tables %q, want %q", names, want)
		}
		return nil
	})
	sw.call("Database.TableByName(app.parent)", func(ctx context.Context) error {
		t, err := byName(ctx, "parent")
		if err == nil && (t.OID == 0 || t.Owner == "" || t.Kind != RelationTable || t.Comment != "sweep" || t.RowEstimate != 2 ||
			!t.PrivilegesDefault || len(t.Privileges) == 0 || t.Origin != (Origin{})) {
			err = fmt.Errorf("%+v: want the commented table, analyzed with 2 rows, its ACL never set", t)
		}
		return err
	})
	sw.call("Table.Columns(app.parent)", func(ctx context.Context) error {
		t, err := byName(ctx, "parent")
		if err != nil {
			return err
		}
		cs, err := t.Columns(ctx)
		if err == nil && (len(cs) != 3 || cs[1].Type != "app.citext" || !cs[1].NotNull || cs[2].Type != "numeric(18,2)") {
			err = fmt.Errorf("%d columns: want id, name app.citext NOT NULL, amount numeric(18,2)", len(cs))
		}
		return err
	})
	sw.call("Table.Constraints(app.child)", func(ctx context.Context) error {
		t, err := byName(ctx, "child")
		if err != nil {
			return err
		}
		cs, err := t.Constraints(ctx)
		if err != nil {
			return err
		}
		kinds := map[ConstraintKind]int{}
		for _, c := range cs {
			kinds[c.Kind]++
		}
		if kinds[ConstraintPrimaryKey] != 1 || kinds[ConstraintForeignKey] != 1 || kinds[ConstraintCheck] != 1 {
			return fmt.Errorf("%+v: want a primary key, a foreign key and a check", kinds)
		}
		return nil
	})
	sw.call("Table.Indexes(app.child)", func(ctx context.Context) error {
		t, err := byName(ctx, "child")
		if err != nil {
			return err
		}
		xs, err := t.Indexes(ctx)
		if err == nil && (len(xs) != 2 || xs[0].Name != "child_parent" || !slices.Equal(xs[0].IncludedColumns, []string{"note"})) {
			err = fmt.Errorf("%d indexes: want child_parent INCLUDE (note) and the key's", len(xs))
		}
		return err
	})
	sw.call("Table.Triggers(app.child)", func(ctx context.Context) error {
		t, err := byName(ctx, "child")
		if err != nil {
			return err
		}
		ts, err := t.Triggers(ctx)
		if err == nil && (len(ts) != 1 || ts[0].Name != "child_touch" || ts[0].Timing != TriggerBefore) {
			err = fmt.Errorf("%d triggers: want child_touch BEFORE, no foreign key trigger", len(ts))
		}
		return err
	})
	sw.call("Table.Policies(app.child)", func(ctx context.Context) error {
		t, err := byName(ctx, "child")
		if err != nil {
			return err
		}
		ps, err := t.Policies(ctx)
		if err == nil && (len(ps) != 1 || ps[0].Name != "child_all" || ps[0].Using != "true" || !t.RowSecurity) {
			err = fmt.Errorf("%d policies, RowSecurity %v: want child_all USING (true), enabled", len(ps), t.RowSecurity)
		}
		return err
	})
	sw.call("Database.PolicyByName(app.child.child_all)", func(ctx context.Context) error {
		p, err := db.PolicyByName(ctx, "app", "child", "child_all")
		if err == nil && (p.Using != "true" || p.Table != "child" || p.Schema != "app" || p.Command != PolicyAll) {
			err = fmt.Errorf("policy %+v: want app.child's child_all, FOR ALL USING (true)", p)
		}
		return err
	})
	sw.call("Table.Children(app.part)", func(ctx context.Context) error {
		t, err := byName(ctx, "part")
		if err != nil {
			return err
		}
		cs, err := t.Children(ctx)
		if err == nil && (len(cs) != 1 || cs[0].Name != "part_2026" || !strings.HasPrefix(cs[0].PartitionBound, "FOR VALUES FROM")) {
			err = fmt.Errorf("%+v: want part_2026 with its bound", cs)
		}
		return err
	})
	sw.call("Table.Size(app.parent)", func(ctx context.Context) error {
		t, err := byName(ctx, "parent")
		if err != nil {
			return err
		}
		sz, err := t.Size(ctx)
		if err == nil && (sz.Data <= 0 || sz.Indexes <= 0 || sz.Total != sz.Data+sz.Indexes) {
			err = fmt.Errorf("%+v: want data and indexes, Total their sum", sz)
		}
		return err
	})
}

// sweepViews drives the view and materialized view reads with an assertion
// on their rows: both listings narrowed to app, both by-name reads, and each
// part read on app.parent_names and app.parent_totals.
func sweepViews(sw *sweep, db *Database) {
	app := ObjectFilter{Schema: []TextCriterion{{TextEquals, "app"}}}
	sw.call("Database.Views(app)", func(ctx context.Context) error {
		vs, err := db.Views(ctx, app)
		if err == nil && (len(vs) != 1 || vs[0].Name != "parent_names") {
			err = fmt.Errorf("%d views: want app.parent_names alone", len(vs))
		}
		return err
	})
	sw.call("Database.ViewByName(app.parent_names)", func(ctx context.Context) error {
		v, err := db.ViewByName(ctx, "app", "parent_names")
		if err == nil && (v.OID == 0 || v.Owner == "" || !strings.Contains(v.Definition, "FROM app.parent") ||
			!v.SecurityBarrier || v.SecurityInvoker || v.CheckOption != CheckOptionNone || len(v.Options) != 1 ||
			!v.PrivilegesDefault || len(v.Privileges) == 0) {
			err = fmt.Errorf("%+v: want the security_barrier view over app.parent, its ACL never set", v)
		}
		return err
	})
	sw.call("View.Columns(app.parent_names)", func(ctx context.Context) error {
		v, err := db.ViewByName(ctx, "app", "parent_names")
		if err != nil {
			return err
		}
		cs, err := v.Columns(ctx)
		if err == nil && (len(cs) != 2 || cs[0].Name != "id" || cs[1].Type != "app.citext") {
			err = fmt.Errorf("%d columns: want id, name app.citext", len(cs))
		}
		return err
	})
	sw.call("Database.MaterializedViews(app)", func(ctx context.Context) error {
		ms, err := db.MaterializedViews(ctx, app)
		if err == nil && (len(ms) != 1 || ms[0].Name != "parent_totals") {
			err = fmt.Errorf("%d materialized views: want app.parent_totals alone", len(ms))
		}
		return err
	})
	sw.call("Database.MaterializedViewByName(app.parent_totals)", func(ctx context.Context) error {
		m, err := db.MaterializedViewByName(ctx, "app", "parent_totals")
		if err == nil && (m.OID == 0 || !m.Populated || !strings.Contains(m.Definition, "count(*)") || !m.PrivilegesDefault) {
			err = fmt.Errorf("%+v: want the populated count over app.parent", m)
		}
		return err
	})
	sw.call("MaterializedView.Columns(app.parent_totals)", func(ctx context.Context) error {
		m, err := db.MaterializedViewByName(ctx, "app", "parent_totals")
		if err != nil {
			return err
		}
		cs, err := m.Columns(ctx)
		if err == nil && (len(cs) != 1 || cs[0].Name != "n" || cs[0].Type != "bigint") {
			err = fmt.Errorf("%d columns: want n bigint", len(cs))
		}
		return err
	})
	sw.call("MaterializedView.Indexes(app.parent_totals)", func(ctx context.Context) error {
		m, err := db.MaterializedViewByName(ctx, "app", "parent_totals")
		if err != nil {
			return err
		}
		xs, err := m.Indexes(ctx)
		if err == nil && (len(xs) != 1 || xs[0].Name != "parent_totals_n" || !xs[0].Unique) {
			err = fmt.Errorf("%d indexes: want the unique parent_totals_n", len(xs))
		}
		return err
	})
	sw.call("MaterializedView.Size(app.parent_totals)", func(ctx context.Context) error {
		m, err := db.MaterializedViewByName(ctx, "app", "parent_totals")
		if err != nil {
			return err
		}
		sz, err := m.Size(ctx)
		if err == nil && (sz.Data <= 0 || sz.Indexes <= 0 || sz.Total != sz.Data+sz.Indexes) {
			err = fmt.Errorf("%+v: want data and an index, Total their sum", sz)
		}
		return err
	})
}

// sweepSequences drives the sequence reads with an assertion on their rows:
// the listing narrowed to app, the by-name read, and the last value of
// app.counter (PUBLIC may read it) and of app.seq (never called; refused to
// the plain role, who holds no privilege on it).
func sweepSequences(sw *sweep, db *Database) {
	sw.call("Database.Sequences(app)", func(ctx context.Context) error {
		qs, err := db.Sequences(ctx, ObjectFilter{Schema: []TextCriterion{{TextEquals, "app"}}})
		if err == nil && (len(qs) != 2 || qs[0].Name != "counter" || qs[1].Name != "seq") {
			err = fmt.Errorf("%d sequences: want app.counter, app.seq", len(qs))
		}
		return err
	})
	sw.call("Database.SequenceByName(app.counter)", func(ctx context.Context) error {
		q, err := db.SequenceByName(ctx, "app", "counter")
		if err == nil && (q.OID == 0 || q.Owner == "" || q.DataType != "smallint" || q.Start != -1 || q.Increment != -2 ||
			q.MinValue != -32768 || q.MaxValue != -1 || q.Cache != 5 || !q.Cycle || q.Persistence != PersistencePermanent ||
			q.OwnedBy != (ObjectName{"app", "child"}) || q.OwnedByColumn != "id" || q.Identity ||
			q.PrivilegesDefault || q.Comment != "sweep") {
			err = fmt.Errorf("%+v: want the descending smallint cycle owned by app.child.id, granted, commented", q)
		}
		return err
	})
	sw.call("Sequence.LastValue(app.counter)", func(ctx context.Context) error {
		q, err := db.SequenceByName(ctx, "app", "counter")
		if err != nil {
			return err
		}
		v, called, err := q.LastValue(ctx)
		if err == nil && (v != -9 || !called) {
			err = fmt.Errorf("%d, %v: want -9, the fifth value its cache stored", v, called)
		}
		return err
	})
	sw.call("Sequence.LastValue(app.seq)", func(ctx context.Context) error {
		q, err := db.SequenceByName(ctx, "app", "seq")
		if err != nil {
			return err
		}
		v, called, err := q.LastValue(ctx)
		if sw.role != "" {
			if errors.Is(err, ErrDefinitionHidden) {
				return nil
			}
			return fmt.Errorf("as a role without USAGE: %d, %v, %v; want ErrDefinitionHidden", v, called, err)
		}
		if err == nil && (v != 0 || called) {
			err = fmt.Errorf("%d, %v: want none, never called", v, called)
		}
		return err
	})
}

// sweepTriggers drives the trigger and event trigger reads with an
// assertion on their rows: the database's triggers narrowed to app — the
// table's and the view's — the by-name read of the view's, the view's own
// listing, and the event trigger listed and read by name.
func sweepTriggers(sw *sweep, db *Database) {
	checkView := func(tr *Trigger) error {
		if tr.OID == 0 || tr.Schema != "app" || tr.Table != "parent_names" || tr.TableKind != RelationView ||
			tr.Name != "names_update" || tr.Timing != TriggerInsteadOf || !tr.ForEachRow ||
			!slices.Equal(tr.Events, []TriggerEvent{TriggerUpdate}) || tr.Function != (ObjectName{"app", "touch"}) ||
			!slices.Equal(tr.Arguments, []string{"a", ""}) || tr.Enabled != TriggerEnabledOrigin || tr.HasCondition ||
			tr.Database() != db {
			return fmt.Errorf("%+v: want app.parent_names' INSTEAD OF UPDATE row trigger running app.touch('a', '')", tr)
		}
		return nil
	}
	sw.call("Database.Triggers(app)", func(ctx context.Context) error {
		ts, err := db.Triggers(ctx, ObjectFilter{Schema: []TextCriterion{{TextEquals, "app"}}})
		if err != nil {
			return err
		}
		if len(ts) != 2 || ts[0].Name != "child_touch" || ts[0].Table != "child" || ts[0].Arguments != nil {
			return fmt.Errorf("%d triggers: want app.child's child_touch, then app.parent_names' names_update", len(ts))
		}
		return checkView(ts[1])
	})
	sw.call("Database.TriggerByName(app.parent_names.names_update)", func(ctx context.Context) error {
		tr, err := db.TriggerByName(ctx, "app", "parent_names", "names_update")
		if err != nil {
			return err
		}
		return checkView(tr)
	})
	sw.call("View.Triggers(app.parent_names)", func(ctx context.Context) error {
		v, err := db.ViewByName(ctx, "app", "parent_names")
		if err != nil {
			return err
		}
		ts, err := v.Triggers(ctx)
		if err != nil {
			return err
		}
		if len(ts) != 1 {
			return fmt.Errorf("%d triggers, want names_update alone", len(ts))
		}
		return checkView(ts[0])
	})
	checkEvent := func(e *EventTrigger) error {
		if e.OID == 0 || e.Name != "sweep_ddl" || e.Owner == "" || e.Origin != (Origin{}) || e.Event != EventTriggerDDLCommandEnd ||
			e.Function != (ObjectName{"app", "on_ddl"}) || e.Enabled != TriggerEnabledOrigin ||
			!slices.Equal(e.Tags, []string{"CREATE TABLE"}) || e.Comment != "sweep" || e.Database() != db {
			return fmt.Errorf("%+v: want sweep_ddl on ddl_command_end for CREATE TABLE, commented", e)
		}
		return nil
	}
	sw.call("Database.EventTriggers(sweep_ddl)", func(ctx context.Context) error {
		es, err := db.EventTriggers(ctx, ObjectFilter{Name: []TextCriterion{{TextEquals, "sweep_ddl"}}})
		if err != nil {
			return err
		}
		if len(es) != 1 {
			return fmt.Errorf("%d event triggers, want sweep_ddl alone", len(es))
		}
		return checkEvent(es[0])
	})
	sw.call("Database.EventTriggerByName(sweep_ddl)", func(ctx context.Context) error {
		e, err := db.EventTriggerByName(ctx, "sweep_ddl")
		if err != nil {
			return err
		}
		return checkEvent(e)
	})
}

// sweepExtensions drives the extension reads with an assertion on their
// rows: the installed listing narrowed to app (citext), plpgsql by name
// (initdb's, so a system one), citext's members, and the available
// listing and by-name read of the two extensions the fixture installed.
func sweepExtensions(sw *sweep, db *Database) {
	sw.call("Database.Extensions(app)", func(ctx context.Context) error {
		es, err := db.Extensions(ctx, ObjectFilter{Schema: []TextCriterion{{TextEquals, "app"}}})
		if err != nil {
			return err
		}
		if len(es) != 1 {
			return fmt.Errorf("%d extensions, want citext alone", len(es))
		}
		e := es[0]
		if e.OID == 0 || e.Name != "citext" || e.Owner == "" || e.Origin != (Origin{}) || e.Schema != "app" ||
			e.Version == "" || e.DefaultVersion != e.Version || !e.Relocatable || e.Requires != nil ||
			e.ConfigTables != nil || e.Comment != "sweep" || e.Database() != db {
			return fmt.Errorf("%+v: want citext in app at its default version, commented", e)
		}
		return nil
	})
	sw.call("Database.ExtensionByName(plpgsql)", func(ctx context.Context) error {
		e, err := db.ExtensionByName(ctx, "plpgsql")
		if err != nil {
			return err
		}
		if !e.IsSystem || e.Schema != "pg_catalog" || e.Version != "1.0" || e.Relocatable || e.Comment == "" {
			return fmt.Errorf("%+v: want initdb's plpgsql 1.0 in pg_catalog", e)
		}
		return nil
	})
	sw.call("Extension.Members(citext)", func(ctx context.Context) error {
		e, err := db.ExtensionByName(ctx, "citext")
		if err != nil {
			return err
		}
		ms, err := e.Members(ctx)
		if err != nil {
			return err
		}
		if !slices.ContainsFunc(ms, func(m ExtensionMember) bool {
			return m.Catalog == "pg_type" && m.Type == "type" && m.Identity == "app.citext" && m.OID != 0
		}) {
			return fmt.Errorf("%d members, none the type app.citext", len(ms))
		}
		return nil
	})
	checkAvailable := func(a *AvailableExtension, name string) error {
		installed := slices.IndexFunc(a.Versions, func(v AvailableExtensionVersion) bool { return v.Installed })
		if a.Name != name || a.DefaultVersion == "" || a.InstalledVersion == "" || a.Comment == "" || installed < 0 ||
			a.Versions[installed].Version != a.InstalledVersion || a.Database() != db {
			return fmt.Errorf("%+v: want %s installed at a version it lists", a, name)
		}
		return nil
	}
	sw.call("Database.AvailableExtensions(citext)", func(ctx context.Context) error {
		as, err := db.AvailableExtensions(ctx, ObjectFilter{Name: []TextCriterion{{TextEquals, "citext"}}})
		if err != nil {
			return err
		}
		if len(as) != 1 {
			return fmt.Errorf("%d available extensions, want citext alone", len(as))
		}
		return checkAvailable(as[0], "citext")
	})
	sw.call("Database.AvailableExtensionByName(postgres_fdw)", func(ctx context.Context) error {
		a, err := db.AvailableExtensionByName(ctx, "postgres_fdw")
		if err != nil {
			return err
		}
		return checkAvailable(a, "postgres_fdw")
	})
}

// sweepTablespaces drives the tablespace reads with an assertion on their
// rows: the listing narrowed to the built-in ones, pg_global by name, and
// two sizes — pg_default's, any role's (the session's database lives
// there), and pg_global's as the connected role. The plain role's refusal
// for pg_global (42501) is TestLiveTablespaces': here it would abort the
// call's transaction.
func sweepTablespaces(sw *sweep) {
	check := func(t *Tablespace, name string) error {
		if t.OID == 0 || t.Name != name || t.Owner == "" || !t.IsSystem || t.Extension != "" || t.Location != "" ||
			t.Options != nil || !t.PrivilegesDefault || len(t.Privileges) != 1 || t.Privileges[0].Privilege != PrivilegeCreate ||
			t.Server() != sw.srv {
			return fmt.Errorf("%+v: want initdb's %s, its owner's CREATE alone", t, name)
		}
		return nil
	}
	sw.call("Server.Tablespaces(system)", func(ctx context.Context) error {
		yes := true
		ts, err := sw.srv.Tablespaces(ctx, ObjectFilter{System: &yes})
		if err != nil {
			return err
		}
		if len(ts) != 2 {
			return fmt.Errorf("%d tablespaces, want pg_default and pg_global", len(ts))
		}
		if err := check(ts[0], "pg_default"); err != nil {
			return err
		}
		return check(ts[1], "pg_global")
	})
	sw.call("Server.TablespaceByName(pg_global)", func(ctx context.Context) error {
		t, err := sw.srv.TablespaceByName(ctx, "pg_global")
		if err != nil {
			return err
		}
		return check(t, "pg_global")
	})
	sw.call("Tablespace.Size(pg_default)", func(ctx context.Context) error {
		n, err := sw.srv.TablespaceRef("pg_default").Size(ctx)
		if err == nil && n <= 0 {
			err = fmt.Errorf("size %d, want > 0", n)
		}
		return err
	})
	if sw.role != "" {
		return
	}
	sw.call("Tablespace.Size(pg_global)", func(ctx context.Context) error {
		n, err := sw.srv.TablespaceRef("pg_global").Size(ctx)
		if err == nil && n <= 0 {
			err = fmt.Errorf("size %d, want > 0", n)
		}
		return err
	})
}

// sweepCatalogObjects drives the collation, language, cast and operator
// reads with an assertion on their rows, each on the fixture's object: the
// listings narrowed to it, and the by-name reads.
func sweepCatalogObjects(sw *sweep, db *Database, mood OID) {
	// citext, in app, brings casts and operators of its own: members.
	app := ObjectFilter{Schema: []TextCriterion{{TextEquals, "app"}}, ExtensionMember: new(bool)}
	checkCollation := func(c *Collation) error {
		if c.OID == 0 || c.Schema != "app" || c.Name != "sweep_c" || c.Owner == "" || c.Origin != (Origin{}) ||
			c.Provider != LocaleProviderLibc || !c.Deterministic || c.Collate != "C" || c.CType != "C" ||
			c.Locale != "" || c.ICURules != "" || c.Comment != "sweep" || c.Database() != db {
			return fmt.Errorf("%+v: want libc's C as app.sweep_c, commented", c)
		}
		return nil
	}
	sw.call("Database.Collations(app)", func(ctx context.Context) error {
		cs, err := db.Collations(ctx, app)
		if err != nil {
			return err
		}
		if len(cs) != 1 {
			return fmt.Errorf("%d collations, want app.sweep_c alone", len(cs))
		}
		return checkCollation(cs[0])
	})
	sw.call("Database.CollationByName(app.sweep_c)", func(ctx context.Context) error {
		c, err := db.CollationByName(ctx, "app", "sweep_c")
		if err != nil {
			return err
		}
		return checkCollation(c)
	})
	checkLanguage := func(l *Language) error {
		if l.OID == 0 || l.Name != "sweep_pl" || l.Owner == "" || l.Origin != (Origin{}) || !l.Procedural || l.Trusted ||
			l.Handler != (ObjectName{"pg_catalog", "plpgsql_call_handler"}) || l.Inline != (ObjectName{}) ||
			l.Validator != (ObjectName{}) || !l.PrivilegesDefault || len(l.Privileges) != 2 || l.Comment != "sweep" ||
			l.Database() != db {
			return fmt.Errorf("%+v: want sweep_pl over plpgsql's handler, commented", l)
		}
		return nil
	}
	sw.call("Database.Languages(sweep_pl)", func(ctx context.Context) error {
		no := false
		ls, err := db.Languages(ctx, ObjectFilter{System: &no})
		if err != nil {
			return err
		}
		if len(ls) != 1 {
			return fmt.Errorf("%d user languages, want sweep_pl alone", len(ls))
		}
		return checkLanguage(ls[0])
	})
	sw.call("Database.LanguageByName(sweep_pl)", func(ctx context.Context) error {
		l, err := db.LanguageByName(ctx, "sweep_pl")
		if err != nil {
			return err
		}
		return checkLanguage(l)
	})
	checkCast := func(c *Cast) error {
		if c.OID == 0 || c.Origin != (Origin{}) || c.SourceType != mood || c.Source != "app.mood" || c.TargetType != 23 ||
			c.Target != "integer" || c.Function != (ObjectName{"app", "f"}) || c.Context != CastAssignment ||
			c.Method != CastByFunction || c.Comment != "sweep" || c.Database() != db {
			return fmt.Errorf("%+v: want (app.mood AS integer) by app.f on assignment, commented", c)
		}
		return nil
	}
	sw.call("Database.Casts(user)", func(ctx context.Context) error {
		no := false
		cs, err := db.Casts(ctx, ObjectFilter{System: &no, ExtensionMember: &no})
		if err != nil {
			return err
		}
		// CREATE TYPE … AS RANGE made a cast of its own (deptype 'i' on
		// both types): explicit, by the multirange's constructor.
		if len(cs) != 2 || cs[1].Signature() != "(app.span AS app.spans)" || cs[1].Context != CastExplicit ||
			cs[1].Method != CastByFunction || cs[1].Function != (ObjectName{"app", "spans"}) {
			return fmt.Errorf("%d user casts: want (app.mood AS integer), then the range's (app.span AS app.spans)", len(cs))
		}
		return checkCast(cs[0])
	})
	sw.call("Database.CastByTypes(app.mood, integer)", func(ctx context.Context) error {
		c, err := db.CastByTypes(ctx, mood, 23)
		if err != nil {
			return err
		}
		return checkCast(c)
	})
	checkOperator := func(o *Operator) error {
		if o.OID == 0 || o.Schema != "app" || o.Name != "===" || o.Owner == "" || o.Origin != (Origin{}) ||
			o.Kind != OperatorInfix || o.LeftType != 23 || o.Left != "integer" || o.RightType != mood ||
			o.Right != "app.mood" || o.ResultType != 16 || o.Result != "boolean" ||
			o.Function != (ObjectName{"app", "eqm"}) || o.Commutator != (ObjectName{}) ||
			o.Negator != (ObjectName{"app", "@@@"}) || o.Restrict != (ObjectName{"pg_catalog", "eqsel"}) ||
			o.Join != (ObjectName{}) || o.Merges || !o.Hashes || o.Comment != "sweep" || o.Database() != db {
			return fmt.Errorf("%+v: want app.=== (integer, app.mood) by app.eqm, negated by app.@@@, commented", o)
		}
		return nil
	}
	sw.call("Database.Operators(app)", func(ctx context.Context) error {
		os, err := db.Operators(ctx, app)
		if err != nil {
			return err
		}
		var names []string
		for _, o := range os {
			names = append(names, o.Signature())
		}
		if !slices.Equal(names, []string{"app.!! (NONE, integer)", "app.=== (integer, app.mood)", "app.@@@ (integer, app.mood)"}) {
			return fmt.Errorf("operators %q: want app's prefix !!, === and its shell negator @@@", names)
		}
		if shell := os[2]; shell.Function != (ObjectName{}) || shell.ResultType != 0 || shell.Result != "" {
			return fmt.Errorf("%+v: want a shell operator, no function or result", shell)
		}
		return checkOperator(os[1])
	})
	sw.call("Database.OperatorByName(app.===)", func(ctx context.Context) error {
		o, err := db.OperatorByName(ctx, "app", "===", 23, mood)
		if err != nil {
			return err
		}
		return checkOperator(o)
	})
}

// sweepForeignData drives the wrapper, server, user mapping and foreign
// table reads with an assertion on their rows, each on the fixture's
// object: the listings narrowed to it, the by-name reads, a wrapper's
// servers, a server's user mappings — PUBLIC's options shown to the
// connected role, withheld from the plain one — and the foreign table's
// parts.
func sweepForeignData(sw *sweep, db *Database) {
	checkWrapper := func(w *ForeignDataWrapper) error {
		if w.OID == 0 || w.Name != "postgres_fdw" || w.Owner == "" || w.Origin != (Origin{Extension: "postgres_fdw"}) ||
			w.Handler.Name != "postgres_fdw_handler" || w.Validator.Name != "postgres_fdw_validator" || w.Options != nil ||
			!w.PrivilegesDefault || len(w.Privileges) != 1 || w.Database() != db {
			return fmt.Errorf("%+v: want postgres_fdw's, its handler and validator", w)
		}
		return nil
	}
	sw.call("Database.ForeignDataWrappers(postgres_fdw)", func(ctx context.Context) error {
		ws, err := db.ForeignDataWrappers(ctx, ObjectFilter{Name: []TextCriterion{{TextEquals, "postgres_fdw"}}, ExtensionMember: new(true)})
		if err != nil {
			return err
		}
		if len(ws) != 1 {
			return fmt.Errorf("%d wrappers, want postgres_fdw alone", len(ws))
		}
		return checkWrapper(ws[0])
	})
	sw.call("Database.ForeignDataWrapperByName(postgres_fdw)", func(ctx context.Context) error {
		w, err := db.ForeignDataWrapperByName(ctx, "postgres_fdw")
		if err != nil {
			return err
		}
		return checkWrapper(w)
	})
	checkServer := func(s *ForeignServer) error {
		if s.OID == 0 || s.Name != "sweep_remote" || s.Owner == "" || s.Origin != (Origin{}) || s.Wrapper != "postgres_fdw" ||
			s.Type != "" || s.Version != "" || !reflect.DeepEqual(s.Options, []ParameterSetting{{"host", "sweep.invalid"}}) ||
			!s.PrivilegesDefault || len(s.Privileges) != 1 || s.Comment != "sweep" || s.Database() != db {
			return fmt.Errorf("%+v: want sweep_remote on postgres_fdw, its host, commented", s)
		}
		return nil
	}
	sw.call("Database.ForeignServers(sweep_remote)", func(ctx context.Context) error {
		no := false
		ss, err := db.ForeignServers(ctx, ObjectFilter{System: &no, ExtensionMember: &no})
		if err != nil {
			return err
		}
		if len(ss) != 1 {
			return fmt.Errorf("%d servers, want sweep_remote alone", len(ss))
		}
		return checkServer(ss[0])
	})
	sw.call("Database.ForeignServerByName(sweep_remote)", func(ctx context.Context) error {
		s, err := db.ForeignServerByName(ctx, "sweep_remote")
		if err != nil {
			return err
		}
		return checkServer(s)
	})
	sw.call("ForeignDataWrapper.Servers(postgres_fdw)", func(ctx context.Context) error {
		ss, err := db.ForeignDataWrapperRef("postgres_fdw").Servers(ctx)
		if err != nil {
			return err
		}
		if len(ss) != 1 {
			return fmt.Errorf("%d servers, want sweep_remote alone", len(ss))
		}
		return checkServer(ss[0])
	})
	checkMapping := func(m *UserMapping) error {
		want := []ParameterSetting{{"user", "sweep"}}
		if sw.role != "" {
			want = nil
		}
		if m.OID == 0 || m.ServerOID == 0 || m.Server != "sweep_remote" || m.UserOID != 0 || !m.IsPublic() ||
			m.Origin != (Origin{}) || !reflect.DeepEqual(m.Options, want) || m.OptionsHidden != (sw.role != "") || m.Database() != db {
			return fmt.Errorf("%+v: want PUBLIC's on sweep_remote, options %v", m, want)
		}
		return nil
	}
	sw.call("Database.UserMappings(sweep_remote)", func(ctx context.Context) error {
		ms, err := db.UserMappings(ctx, ObjectFilter{System: new(bool)})
		if err != nil {
			return err
		}
		if len(ms) != 1 {
			return fmt.Errorf("%d user mappings, want PUBLIC's alone", len(ms))
		}
		return checkMapping(ms[0])
	})
	sw.call("Database.UserMappingByName(sweep_remote, PUBLIC)", func(ctx context.Context) error {
		m, err := db.UserMappingByName(ctx, "sweep_remote", "")
		if err != nil {
			return err
		}
		return checkMapping(m)
	})
	sw.call("ForeignServer.UserMappings(sweep_remote)", func(ctx context.Context) error {
		ms, err := db.ForeignServerRef("sweep_remote").UserMappings(ctx)
		if err != nil {
			return err
		}
		if len(ms) != 1 {
			return fmt.Errorf("%d user mappings, want PUBLIC's alone", len(ms))
		}
		return checkMapping(ms[0])
	})
	checkTable := func(t *ForeignTable) error {
		if t.OID == 0 || t.Schema != "app" || t.Name != "remote" || t.Owner == "" || t.Origin != (Origin{}) ||
			t.Server != "sweep_remote" || !reflect.DeepEqual(t.Options, []ParameterSetting{{"table_name", "r"}}) ||
			t.IsPartition || t.PartitionBound != "" || t.Parents != nil || !t.PrivilegesDefault || t.Comment != "" || t.Database() != db {
			return fmt.Errorf("%+v: want app.remote on sweep_remote, its table option", t)
		}
		return nil
	}
	var remote *ForeignTable
	sw.call("Database.ForeignTables(app)", func(ctx context.Context) error {
		ts, err := db.ForeignTables(ctx, ObjectFilter{Schema: []TextCriterion{{TextEquals, "app"}}})
		if err != nil {
			return err
		}
		if len(ts) != 1 {
			return fmt.Errorf("%d foreign tables, want app.remote alone", len(ts))
		}
		return checkTable(ts[0])
	})
	sw.call("Database.ForeignTableByName(app.remote)", func(ctx context.Context) error {
		t, err := db.ForeignTableByName(ctx, "app", "remote")
		if err == nil {
			remote, err = t, checkTable(t)
		}
		return err
	})
	if remote == nil {
		return
	}
	sw.call("ForeignTable.Columns(app.remote)", func(ctx context.Context) error {
		cs, err := remote.Columns(ctx)
		if err == nil && (len(cs) != 1 || cs[0].Name != "id" || !reflect.DeepEqual(cs[0].FDWOptions, []ParameterSetting{{"column_name", "ID"}})) {
			err = fmt.Errorf("%+v: want id, its column_name option", cs)
		}
		return err
	})
	sw.call("ForeignTable.Constraints(app.remote)", func(ctx context.Context) error {
		cs, err := remote.Constraints(ctx)
		if err == nil && (len(cs) != 1 || cs[0].Name != "remote_id_check") {
			err = fmt.Errorf("%+v: want remote_id_check alone", cs)
		}
		return err
	})
	sw.call("ForeignTable.Triggers(app.remote)", func(ctx context.Context) error {
		ts, err := remote.Triggers(ctx)
		if err == nil && len(ts) != 0 {
			err = fmt.Errorf("%d triggers, want none", len(ts))
		}
		return err
	})
}

// sweepTextSearch drives the four text search families' reads with an
// assertion on their rows: each listing narrowed to app, and each by-name
// read of the fixture's object.
func sweepTextSearch(sw *sweep, db *Database) {
	app := ObjectFilter{Schema: []TextCriterion{{TextEquals, "app"}}}
	one := func(n int, what string) error {
		if n != 1 {
			return fmt.Errorf("%d %s, want the fixture's alone", n, what)
		}
		return nil
	}
	checkParser := func(p *TextSearchParser) error {
		if p.OID == 0 || p.Schema != "app" || p.Name != "sweep_prs" || p.Origin != (Origin{}) ||
			p.Start != (ObjectName{"pg_catalog", "prsd_start"}) || p.Token != (ObjectName{"pg_catalog", "prsd_nexttoken"}) ||
			p.End != (ObjectName{"pg_catalog", "prsd_end"}) || p.LexTypes != (ObjectName{"pg_catalog", "prsd_lextype"}) ||
			p.Headline != (ObjectName{}) || p.Comment != "" || p.Database() != db {
			return fmt.Errorf("%+v: want app.sweep_prs over the default parser's functions, no headline", p)
		}
		return nil
	}
	sw.call("Database.TextSearchParsers(app)", func(ctx context.Context) error {
		ps, err := db.TextSearchParsers(ctx, app)
		if err == nil {
			err = one(len(ps), "parsers")
		}
		if err != nil {
			return err
		}
		return checkParser(ps[0])
	})
	sw.call("Database.TextSearchParserByName(app.sweep_prs)", func(ctx context.Context) error {
		p, err := db.TextSearchParserByName(ctx, "app", "sweep_prs")
		if err != nil {
			return err
		}
		return checkParser(p)
	})
	checkTemplate := func(t *TextSearchTemplate) error {
		if t.OID == 0 || t.Schema != "app" || t.Name != "sweep_tmpl" || t.Origin != (Origin{}) || t.Init != (ObjectName{}) ||
			t.Lexize != (ObjectName{"pg_catalog", "dsimple_lexize"}) || t.Comment != "" || t.Database() != db {
			return fmt.Errorf("%+v: want app.sweep_tmpl over dsimple_lexize, no init", t)
		}
		return nil
	}
	sw.call("Database.TextSearchTemplates(app)", func(ctx context.Context) error {
		ts, err := db.TextSearchTemplates(ctx, app)
		if err == nil {
			err = one(len(ts), "templates")
		}
		if err != nil {
			return err
		}
		return checkTemplate(ts[0])
	})
	sw.call("Database.TextSearchTemplateByName(app.sweep_tmpl)", func(ctx context.Context) error {
		t, err := db.TextSearchTemplateByName(ctx, "app", "sweep_tmpl")
		if err != nil {
			return err
		}
		return checkTemplate(t)
	})
	checkDict := func(dc *TextSearchDictionary) error {
		if dc.OID == 0 || dc.Schema != "app" || dc.Name != "sweep_dict" || dc.Owner == "" || dc.Origin != (Origin{}) ||
			dc.Template != (ObjectName{"pg_catalog", "simple"}) || dc.Options != "stopwords = 'english'" ||
			dc.Comment != "" || dc.Database() != db {
			return fmt.Errorf("%+v: want app.sweep_dict on simple with english stop words", dc)
		}
		return nil
	}
	sw.call("Database.TextSearchDictionaries(app)", func(ctx context.Context) error {
		ds, err := db.TextSearchDictionaries(ctx, app)
		if err == nil {
			err = one(len(ds), "dictionaries")
		}
		if err != nil {
			return err
		}
		return checkDict(ds[0])
	})
	sw.call("Database.TextSearchDictionaryByName(app.sweep_dict)", func(ctx context.Context) error {
		dc, err := db.TextSearchDictionaryByName(ctx, "app", "sweep_dict")
		if err != nil {
			return err
		}
		return checkDict(dc)
	})
	wantMappings := []TextSearchMapping{
		{TokenType: "asciiword", Dictionaries: []ObjectName{{"app", "sweep_dict"}, {"pg_catalog", "english_stem"}}},
		{TokenType: "int", Dictionaries: []ObjectName{{"pg_catalog", "simple"}}},
	}
	checkConfig := func(c *TextSearchConfiguration) error {
		if c.OID == 0 || c.Schema != "app" || c.Name != "sweep_cfg" || c.Owner == "" || c.Origin != (Origin{}) ||
			c.Parser != (ObjectName{"app", "sweep_prs"}) || !reflect.DeepEqual(c.Mappings, wantMappings) ||
			c.Comment != "sweep" || c.Database() != db {
			return fmt.Errorf("%+v: want app.sweep_cfg on app.sweep_prs mapping asciiword and int, commented", c)
		}
		return nil
	}
	sw.call("Database.TextSearchConfigurations(app)", func(ctx context.Context) error {
		cs, err := db.TextSearchConfigurations(ctx, app)
		if err == nil {
			err = one(len(cs), "configurations")
		}
		if err != nil {
			return err
		}
		return checkConfig(cs[0])
	})
	sw.call("Database.TextSearchConfigurationByName(app.sweep_cfg)", func(ctx context.Context) error {
		c, err := db.TextSearchConfigurationByName(ctx, "app", "sweep_cfg")
		if err != nil {
			return err
		}
		return checkConfig(c)
	})
}

// sweepTypes drives the type and domain reads with an assertion on their
// rows: both listings narrowed to app, a by-name read of every kind — enum,
// composite, range, multirange, an extension's base type, pg_catalog's
// int4 and a pseudo-type — the refusals of names that are no Type (an
// implicit array, a domain, a table's row type), and both domains.
func sweepTypes(sw *sweep, db *Database) {
	sw.call("Database.Types(app)", func(ctx context.Context) error {
		ts, err := db.Types(ctx, ObjectFilter{Schema: []TextCriterion{{TextEquals, "app"}}})
		var names []string
		for _, ty := range ts {
			names = append(names, ty.Name)
		}
		if want := []string{"citext", "mood", "pair", "span", "spans"}; err == nil && !slices.Equal(names, want) {
			err = fmt.Errorf("%q: want %q", names, want)
		}
		return err
	})
	for _, c := range []struct {
		schema, name string
		ok           func(*Type) bool
	}{
		{"app", "mood", func(ty *Type) bool {
			return ty.Kind == TypeEnum && slices.Equal(ty.Labels, []string{"ok", "not ok"}) && ty.Comment == "sweep" &&
				ty.Input == (ObjectName{"pg_catalog", "enum_in"}) && ty.PrivilegesDefault && len(ty.Privileges) == 2
		}},
		{"app", "pair", func(ty *Type) bool {
			return ty.Kind == TypeComposite && reflect.DeepEqual(ty.Attributes, []TypeAttribute{
				{Name: "a", TypeOID: 23, Type: "integer"}, {Name: "b", TypeOID: 25, Type: "text"}})
		}},
		{"app", "span", func(ty *Type) bool {
			return ty.Kind == TypeRange && ty.Subtype == "integer" && ty.SubtypeOperatorClass == (ObjectName{"pg_catalog", "int4_ops"}) &&
				ty.Multirange == (ObjectName{"app", "spans"}) && ty.Collation == (ObjectName{}) && ty.Canonical == (ObjectName{})
		}},
		{"app", "spans", func(ty *Type) bool { return ty.Kind == TypeMultirange && ty.Range == (ObjectName{"app", "span"}) }},
		{"app", "citext", func(ty *Type) bool {
			return ty.Kind == TypeBase && ty.Extension == "citext" && ty.Input == (ObjectName{"app", "citextin"}) &&
				ty.InternalLength == -1 && ty.Collatable && ty.Category == TypeCategoryString
		}},
		{"pg_catalog", "int4", func(ty *Type) bool {
			return ty.Kind == TypeBase && ty.IsSystem && ty.InternalLength == 4 && ty.PassedByValue && ty.Alignment == AlignInt4 &&
				ty.Input == (ObjectName{"pg_catalog", "int4in"}) && ty.Category == TypeCategoryNumeric
		}},
		{"pg_catalog", "point", func(ty *Type) bool {
			return ty.Kind == TypeBase && ty.Element == "double precision" && ty.Subscript == (ObjectName{"pg_catalog", "raw_array_subscript_handler"})
		}},
		{"pg_catalog", "anyelement", func(ty *Type) bool { return ty.Kind == TypePseudo && ty.Category == TypeCategoryPseudo }},
	} {
		sw.call("Database.TypeByName("+c.schema+"."+c.name+")", func(ctx context.Context) error {
			ty, err := db.TypeByName(ctx, c.schema, c.name)
			if err == nil && (ty.OID == 0 || ty.Owner == "" || !c.ok(ty)) {
				err = fmt.Errorf("%+v: not the fixture's", ty)
			}
			return err
		})
	}
	for _, name := range []string{"_mood", "code", "child"} {
		sw.call("Database.TypeByName(app."+name+")", func(ctx context.Context) error {
			if _, err := db.TypeByName(ctx, "app", name); !errors.Is(err, ErrNotFound) {
				return fmt.Errorf("err = %v, want ErrNotFound: no Type", err)
			}
			return nil
		})
	}
	sw.call("Database.Domains(app)", func(ctx context.Context) error {
		ds, err := db.Domains(ctx, ObjectFilter{Schema: []TextCriterion{{TextEquals, "app"}}})
		if err == nil && (len(ds) != 2 || ds[0].Name != "code" || ds[1].Name != "positive") {
			err = fmt.Errorf("%d domains: want app.code, app.positive", len(ds))
		}
		return err
	})
	sw.call("Database.DomainByName(app.code)", func(ctx context.Context) error {
		dm, err := db.DomainByName(ctx, "app", "code")
		if err != nil {
			return err
		}
		var want []DomainConstraint
		if sw.srv.Version() >= 170000 {
			want = []DomainConstraint{{Name: "code_not_null", Kind: ConstraintNotNull, Definition: "NOT NULL", Validated: true}}
		}
		if dm.OID == 0 || dm.Owner == "" || dm.BaseTypeOID != 1043 || dm.BaseType != "character varying(8)" || !dm.NotNull ||
			dm.Default != "'x'::character varying" || dm.Collation != (ObjectName{"app", "sweep_c"}) ||
			!reflect.DeepEqual(dm.Constraints, want) || dm.PrivilegesDefault {
			return fmt.Errorf("%+v: want varchar(8) COLLATE app.sweep_c DEFAULT 'x' NOT NULL, granted", dm)
		}
		return nil
	})
	sw.call("Database.DomainByName(app.positive)", func(ctx context.Context) error {
		dm, err := db.DomainByName(ctx, "app", "positive")
		if err == nil && (dm.BaseType != "integer" || dm.NotNull || len(dm.Constraints) != 1 ||
			dm.Constraints[0] != (DomainConstraint{Name: "positive_check", Kind: ConstraintCheck, Definition: "CHECK ((VALUE > 0))", Validated: true})) {
			err = fmt.Errorf("%+v: want integer CHECK (VALUE > 0)", dm)
		}
		return err
	})
	sw.call("Database.DomainByName(app.mood)", func(ctx context.Context) error {
		if _, err := db.DomainByName(ctx, "app", "mood"); !errors.Is(err, ErrNotFound) {
			return fmt.Errorf("err = %v, want ErrNotFound: an enum is no domain", err)
		}
		return nil
	})
}

// sweepRoutineFamilies drives the function, procedure and aggregate reads
// with an assertion on their rows: the three listings narrowed to app, the
// three by-name reads (the overloads both ways), both bodies, and the
// any-kind identity read.
func sweepRoutineFamilies(sw *sweep, db *Database) {
	app := ObjectFilter{Schema: []TextCriterion{{TextEquals, "app"}}, ExtensionMember: new(bool)}
	sw.call("Database.Functions(app)", func(ctx context.Context) error {
		fs, err := db.Functions(ctx, app)
		var names []string
		for _, f := range fs {
			names = append(names, f.Name)
		}
		// app.span's constructors (two) and app.spans' (three) beside them,
		// the trigger functions, and the operator's.
		if want := []string{"eqm", "f", "f", "on_ddl", "span", "span", "spans", "spans", "spans", "touch"}; err == nil && !slices.Equal(names, want) {
			err = fmt.Errorf("%q: want app.eqm, app.f twice, app.on_ddl, the range's constructors and app.touch", names)
		}
		return err
	})
	sw.call("Database.FunctionByName(app.f)", func(ctx context.Context) error {
		_, err := db.FunctionByName(ctx, "app", "f", nil)
		if errors.Is(err, ErrAmbiguous) {
			return nil
		}
		return fmt.Errorf("two overloads: %v, want ErrAmbiguous", err)
	})
	sw.call("Database.FunctionByName(app.f, integer)", func(ctx context.Context) error {
		f, err := db.FunctionByName(ctx, "app", "f", []OID{23})
		if err == nil && (f.Signature() != "app.f(integer)" || f.Language != "sql" || f.ReturnType != "integer" ||
			len(f.Arguments) != 1 || f.Arguments[0].Name != "x" || f.Volatility != VolatilityVolatile || f.Cost != 100) {
			err = fmt.Errorf("%+v: want the sql function app.f(x integer) RETURNS integer", f)
		}
		return err
	})
	sw.call("Function.Body(app.f, integer)", func(ctx context.Context) error {
		f, err := db.FunctionByName(ctx, "app", "f", []OID{23})
		if err != nil {
			return err
		}
		b, err := f.Body(ctx)
		if err == nil && (b.Source != "SELECT x" || !strings.HasPrefix(b.Definition, "CREATE OR REPLACE FUNCTION app.f(x integer)")) {
			err = fmt.Errorf("%+v: want SELECT x in its CREATE OR REPLACE", b)
		}
		return err
	})
	sw.call("Database.Procedures(app)", func(ctx context.Context) error {
		ps, err := db.Procedures(ctx, app)
		if err == nil && (len(ps) != 1 || ps[0].Name != "p") {
			err = fmt.Errorf("%d procedures: want app.p alone", len(ps))
		}
		return err
	})
	sw.call("Database.ProcedureByName(app.p)", func(ctx context.Context) error {
		p, err := db.ProcedureByName(ctx, "app", "p", nil)
		if err == nil && (p.Signature() != "app.p(integer)" || len(p.Arguments) != 1 || p.Arguments[0].Mode != ArgumentInOut) {
			err = fmt.Errorf("%+v: want app.p(INOUT a integer)", p)
		}
		return err
	})
	sw.call("Procedure.Body(app.p)", func(ctx context.Context) error {
		p, err := db.ProcedureByName(ctx, "app", "p", nil)
		if err != nil {
			return err
		}
		b, err := p.Body(ctx)
		if err == nil && (b.Source != "SELECT a" || !strings.HasPrefix(b.Definition, "CREATE OR REPLACE PROCEDURE app.p(")) {
			err = fmt.Errorf("%+v: want SELECT a in its CREATE OR REPLACE", b)
		}
		return err
	})
	sw.call("Database.Aggregates(app)", func(ctx context.Context) error {
		as, err := db.Aggregates(ctx, app)
		if err == nil && (len(as) != 1 || as[0].Name != "ag") {
			err = fmt.Errorf("%d aggregates: want app.ag alone", len(as))
		}
		return err
	})
	sw.call("Database.AggregateByName(app.ag)", func(ctx context.Context) error {
		a, err := db.AggregateByName(ctx, "app", "ag", []OID{})
		if err == nil && (a.TransitionFunction != (ObjectName{"pg_catalog", "int8inc"}) || a.StateType != "bigint" ||
			a.InitialCondition != "0" || !a.HasInitialCondition || a.AggregateKind != AggregateNormal) {
			err = fmt.Errorf("%+v: want app.ag(*) over int8inc from 0", a)
		}
		return err
	})
	sw.call("Database.RoutineByName(app.touch)", func(ctx context.Context) error {
		r, err := db.RoutineByName(ctx, "app", "touch", nil)
		if err == nil && (r.Kind != RoutineFunction || r.Signature() != "app.touch()") {
			err = fmt.Errorf("%s (%v), want the function app.touch()", r.Signature(), r.Kind)
		}
		return err
	})
}

// sweepRoutines drives routineByName against the fixture's overloads, a
// procedure and an aggregate: the ambiguity and the exact match both read
// the catalog's argument types.
func sweepRoutines(sw *sweep) {
	sw.call("routineByName(app.f)", func(ctx context.Context) error {
		_, err := routineByName(ctx, sw.srv.on(ctx), "app", "f", nil)
		if errors.Is(err, ErrAmbiguous) && strings.Contains(err.Error(), "app.f(app.mood)") {
			return nil
		}
		return fmt.Errorf("two overloads: %v, want ErrAmbiguous naming app.f(app.mood)", err)
	})
	sw.call("routineByName(app.f, integer)", func(ctx context.Context) error {
		r, err := routineByName(ctx, sw.srv.on(ctx), "app", "f", []OID{23})
		if err == nil && (r.Kind != RoutineFunction || r.Signature() != "app.f(integer)") {
			err = fmt.Errorf("%s (%v), want the function app.f(integer)", r.Signature(), r.Kind)
		}
		return err
	})
	for _, c := range []struct {
		name string
		kind RoutineKind
		sig  string
	}{
		{"p", RoutineProcedure, "app.p(integer)"},
		{"ag", RoutineAggregate, "app.ag()"}, // ag(*): Signature is the ROUTINE form (identity.go)
	} {
		sw.call("routineByName(app."+c.name+")", func(ctx context.Context) error {
			r, err := routineByName(ctx, sw.srv.on(ctx), "app", c.name, nil)
			if err == nil && (r.Kind != c.kind || r.Signature() != c.sig) {
				err = fmt.Errorf("%s (%v), want %s (%v)", r.Signature(), r.Kind, c.sig, c.kind)
			}
			return err
		})
	}
}

// sweepFragments runs the query fragments no read sends yet — originColumns
// over each catalog it is written for, and an ObjectFilter clause using every
// criterion — each in a query of its own, asserting the fixture's rows.
func sweepFragments(sw *sweep) {
	type named struct {
		schema, name string
		Origin
	}
	for _, c := range []struct {
		catalog            catalogTable
		sql                string
		schema, name       string
		want               Origin
		extSchema, extName string // an extension member's row in the same read
	}{
		{catalogClass, `SELECT n.nspname, c.relname, %s FROM pg_catalog.pg_class c
			JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace`, "app", "parent", Origin{}, "pg_catalog", "pg_class"},
		{catalogProc, `SELECT n.nspname, p.proname, %s FROM pg_catalog.pg_proc p
			JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace`, "app", "touch", Origin{}, "app", "citext_eq"},
		{catalogType, `SELECT n.nspname, y.typname, %s FROM pg_catalog.pg_type y
			JOIN pg_catalog.pg_namespace n ON n.oid = y.typnamespace`, "app", "mood", Origin{}, "app", "citext"},
		{catalogNamespace, `SELECT n.nspname, n.nspname, %s FROM pg_catalog.pg_namespace n`,
			"app", "app", Origin{}, "pg_catalog", "pg_catalog"},
	} {
		oid := map[catalogTable]string{catalogClass: "c.oid", catalogProc: "p.oid", catalogType: "y.oid", catalogNamespace: "n.oid"}[c.catalog]
		sw.call(fmt.Sprintf("originColumns(%s)", c.catalog), func(ctx context.Context) error {
			rows, err := scanRows(ctx, sw.srv.on(ctx), "read origins", func(scan func(...any) error) (named, error) {
				var r named
				return r, scan(append([]any{&r.schema, &r.name}, scanOrigin(&r.Origin)...)...)
			}, fmt.Sprintf(c.sql, originColumns("n.nspname", c.catalog, oid)))
			if err != nil {
				return err
			}
			got := map[[2]string]Origin{}
			for _, r := range rows {
				got[[2]string{r.schema, r.name}] = r.Origin
			}
			if o, ok := got[[2]string{c.schema, c.name}]; !ok || o != c.want {
				return fmt.Errorf("%s.%s: %+v (read %v), want %+v", c.schema, c.name, o, ok, c.want)
			}
			if o := got[[2]string{c.extSchema, c.extName}]; o == (Origin{}) {
				return fmt.Errorf("%s.%s read as a user object owned by no extension", c.extSchema, c.extName)
			}
			return nil
		})
	}

	sw.call("ObjectFilter.clause(every criterion)", func(ctx context.Context) error {
		user, member := false, false
		f := ObjectFilter{
			Name:            []TextCriterion{{TextContains, "AREN"}, {TextNotEquals, "child"}},
			Schema:          []TextCriterion{{TextEquals, "app"}, {TextNotContains, "pg_"}},
			System:          &user,
			ExtensionMember: &member,
		}
		cols := filterColumns{name: "c.relname", schema: "n.nspname",
			system: systemSchema("n.nspname"), extension: isExtensionMember(catalogClass, "c.oid")}
		where, args, err := f.clause(cols, []any{"r"})
		if err != nil {
			return err
		}
		names, err := scanRows(ctx, sw.srv.on(ctx), "filtered relations", func(scan func(...any) error) (string, error) {
			var s string
			return s, scan(&s)
		}, `SELECT c.relname FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
			WHERE c.relkind::pg_catalog.text = $1`+where+` ORDER BY 1`, args...)
		if err == nil && !slices.Equal(names, []string{"parent"}) {
			err = fmt.Errorf("matched %q, want [parent]", names)
		}
		return err
	})
}

// sweepDependencies drives the P4.14 reads with an assertion on their rows:
// each dependency direction on the fixture's view and table, the shared
// edges of the scratch database and of the plain role, the databases the
// plain role has dependents in, and both searches.
func sweepDependencies(sw *sweep, db *Database, plain *Role) {
	has := func(deps []Dependency, typ, identity string, kind DependencyKind, part string) bool {
		return slices.ContainsFunc(deps, func(d Dependency) bool {
			return d.Type == typ && d.Identity == identity && d.Kind == kind && d.Part == part && d.OID != 0 && d.Catalog != ""
		})
	}
	sw.call("Database.Dependencies(app.parent_names)", func(ctx context.Context) error {
		v, err := db.ViewByName(ctx, "app", "parent_names")
		if err != nil {
			return err
		}
		deps, err := v.Database().Dependencies(ctx, v.Address())
		if err != nil {
			return err
		}
		if !has(deps, "table", "app.parent", DependencyNormal, "app.parent.name") || !has(deps, "schema", "app", DependencyNormal, "") ||
			!has(deps, "type", "app.citext", DependencyNormal, "") {
			return fmt.Errorf("%+v: want app.parent through its columns, app and app.citext", deps)
		}
		return nil
	})
	sw.call("Database.Dependents(app.parent)", func(ctx context.Context) error {
		t, err := db.TableByName(ctx, "app", "parent")
		if err != nil {
			return err
		}
		deps, err := db.Dependents(ctx, t.Address())
		if err != nil {
			return err
		}
		if !has(deps, "view", "app.parent_names", DependencyNormal, `"_RETURN" on app.parent_names`) ||
			!has(deps, "materialized view", "app.parent_totals", DependencyNormal, `"_RETURN" on app.parent_totals`) ||
			!has(deps, "table", "app.child", DependencyNormal, "child_parent_id_fkey on app.child") {
			return fmt.Errorf("%+v: want the view, the materialized view and app.child's foreign key", deps)
		}
		return nil
	})
	sw.call("Server.Dependencies(scratch database)", func(ctx context.Context) error {
		deps, err := sw.srv.Dependencies(ctx, db.Address())
		for _, d := range deps {
			if !d.Shared || d.Catalog != "pg_authid" {
				return fmt.Errorf("%+v: a database depends on roles alone", d)
			}
		}
		return err
	})
	sw.call("Server.Dependents(plain role)", func(ctx context.Context) error {
		deps, err := sw.srv.Dependents(ctx, plain.Address())
		if err == nil && len(deps) != 0 {
			err = fmt.Errorf("%+v: the plain role owns no shared object", deps)
		}
		return err
	})
	sw.call("Server.DependentDatabases(plain role)", func(ctx context.Context) error {
		_, err := sw.srv.DependentDatabases(ctx, plain.Address())
		return err
	})
	sw.call("Database.Search(parent)", func(ctx context.Context) error {
		rs, err := db.Search(ctx, ObjectFilter{Name: []TextCriterion{{TextContains, "PARENT"}}, Schema: []TextCriterion{{TextEquals, "app"}}})
		if err != nil {
			return err
		}
		var got []string
		for _, r := range rs {
			got = append(got, r.Type+" "+r.Identity)
		}
		want := []string{"materialized view app.parent_totals", "table app.parent", "view app.parent_names"}
		if !slices.Equal(got, want) {
			return fmt.Errorf("found %q, want %q", got, want)
		}
		return nil
	})
	sw.call("Database.SearchDefinitions(app.parent)", func(ctx context.Context) error {
		rs, err := db.SearchDefinitions(ctx, "count(*)", ObjectFilter{Schema: []TextCriterion{{TextEquals, "app"}}})
		if err == nil && (len(rs) != 1 || rs[0].Identity != "app.parent_totals") {
			err = fmt.Errorf("%+v: want app.parent_totals alone", rs)
		}
		return err
	})
}
