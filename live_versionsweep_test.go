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
	if _, err := admin.Exec(ctx, "CREATE ROLE "+pgsql.QuoteIdent(plain)+" NOLOGIN"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP ROLE IF EXISTS "+pgsql.QuoteIdent(plain)); err != nil {
			t.Errorf("drop role: %v", err)
		}
	})

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

	for _, role := range []string{"", plain} {
		name := "connected role"
		if role != "" {
			name = "plain role"
		}
		t.Run(name, func(t *testing.T) {
			sw := &sweep{t: t, ctx: ctx, srv: srv, role: role}
			for typeName, recv := range map[string]any{"Server": srv} {
				sw.reflectSweep(typeName, scratch, recv)
			}
			sweepServerReads(sw, scratch)
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
