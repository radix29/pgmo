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
	`CREATE SERVER sweep_remote FOREIGN DATA WRAPPER postgres_fdw OPTIONS (host 'sweep.invalid')`,
	`CREATE FOREIGN TABLE app.remote (id integer) SERVER sweep_remote`,
	`CREATE SEQUENCE app.seq AS integer`,
	`CREATE TYPE app.mood AS ENUM ('ok', 'not ok')`,
	`CREATE TYPE app.pair AS (a integer, b text)`,
	`CREATE DOMAIN app.positive AS integer CHECK (VALUE > 0)`,
	`CREATE COLLATION app.sweep_c FROM "C"`,
	// Overloads, so routineByName meets ErrAmbiguous as well as a match.
	`CREATE FUNCTION app.f(x integer) RETURNS integer LANGUAGE sql AS 'SELECT x'`,
	`CREATE FUNCTION app.f(x app.mood) RETURNS integer LANGUAGE sql AS 'SELECT 1'`,
	`CREATE PROCEDURE app.p(INOUT a integer) LANGUAGE sql AS 'SELECT a'`,
	`CREATE AGGREGATE app.ag(*) (sfunc = pg_catalog.int8inc, stype = bigint, initcond = '0')`,
	`CREATE FUNCTION app.touch() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'`,
	`CREATE TRIGGER child_touch BEFORE INSERT ON app.child FOR EACH ROW EXECUTE FUNCTION app.touch()`,
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

	for _, role := range []string{"", plain} {
		name := "connected role"
		if role != "" {
			name = "plain role"
		}
		t.Run(name, func(t *testing.T) {
			sw := &sweep{t: t, ctx: ctx, srv: srv, role: role}
			for typeName, recv := range map[string]any{"Server": srv, "Database": db, "Role": plainRole, "Table": child,
				"View": view, "MaterializedView": matview, "Function": touch, "Procedure": proc, "Aggregate": agg} {
				sw.reflectSweep(typeName, scratch, recv)
			}
			sweepServerReads(sw, scratch)
			sweepDatabases(sw, scratch)
			sweepRoles(sw, scratch, plain, group)
			sweepSchemas(sw, db)
			sweepTables(sw, db)
			sweepViews(sw, db)
			sweepRoutineFamilies(sw, db)
			sweepRoutines(sw)
			sweepFragments(sw)
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

// sweepRoutineFamilies drives the function, procedure and aggregate reads
// with an assertion on their rows: the three listings narrowed to app, the
// three by-name reads (the overloads both ways), both bodies, and the
// any-kind identity read.
func sweepRoutineFamilies(sw *sweep, db *Database) {
	app := ObjectFilter{Schema: []TextCriterion{{TextEquals, "app"}}, ExtensionMember: new(bool)}
	sw.call("Database.Functions(app)", func(ctx context.Context) error {
		fs, err := db.Functions(ctx, app)
		if err == nil && (len(fs) != 3 || fs[0].Name != "f" || fs[1].Name != "f" || fs[2].Name != "touch") {
			err = fmt.Errorf("%d functions: want app.f twice and app.touch", len(fs))
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
