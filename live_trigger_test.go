//go:build livedb

package pgmo

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/radix29/pgmo/pgsql"
)

// liveScratchConn connects to the database db of the live server, closed in
// a t.Cleanup.
func liveScratchConn(t *testing.T, ctx context.Context, db string) *pgx.Conn {
	t.Helper()
	cfg, err := pgx.ParseConfig(*liveConnString)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = db
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	return conn
}

// dumpTriggers prints ts without their Database, one per line.
func dumpTriggers(ts []*Trigger) string {
	var sb strings.Builder
	for _, tr := range ts {
		c := *tr
		c.db = nil
		fmt.Fprintf(&sb, "%+v\n", c)
	}
	return sb.String()
}

// TestLiveTriggers reads the triggers of a throwaway database the listing,
// the by-name read and the per-relation reads must describe in full:
//
//   - app.t: "Tr 1", BEFORE UPDATE OF two columns out of their table order (one needing
//     quotes) FOR
//     EACH ROW WHEN, ENABLE REPLICA, running "Fn s"."T f" with arguments
//     holding a quote, an empty string, a backslash before 000 and
//     multibyte characters; tr2 AFTER INSERT with a NEW TABLE, DISABLEd,
//     commented; "TR2", differing from tr2 only by case, with OLD and NEW
//     TABLEs and one empty argument; a foreign key to app.p, whose
//     server-made triggers on both tables are not read;
//   - a deferrable constraint trigger on app.p;
//   - app.v, a view, with an INSTEAD OF row trigger and a statement trigger;
//   - app.pt, partitioned, whose trigger app.pt1 inherits;
//   - app.ft, a foreign table, with a row trigger;
//   - app.ext_t, a table citext was made to own, with a trigger: an
//     extension member's.
//
// Every non-internal pg_trigger row is listed once, its arguments equal to
// tgargs split in Go. The listing is read again under
// standard_conforming_strings off (identical but for the definition's
// literal, its backslash doubled) and as a plain role (identical). A LATIN1
// database's argument arrives converted to the client's encoding.
func TestLiveTriggers(t *testing.T) {
	admin, ctx, done := liveDBTimeout(t, 2*time.Minute)
	t.Cleanup(done)
	stamp := time.Now().UnixNano()
	name := func(s string) string { return fmt.Sprintf("pgmo_live_trg_%d_%s", stamp, s) }
	plain, db, latin := name("plain"), name("db"), name("latin1")
	q := pgsql.QuoteIdent

	t.Cleanup(func() {
		for _, d := range []string{db, latin} {
			if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+q(d)+" WITH (FORCE)"); err != nil {
				t.Errorf("drop database: %v", err)
			}
		}
		if _, err := admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+q(plain)); err != nil {
			t.Errorf("drop role: %v", err)
		}
	})
	for _, stmt := range []string{"CREATE DATABASE " + q(db), "CREATE ROLE " + q(plain),
		"CREATE DATABASE " + q(latin) + " ENCODING 'LATIN1' LC_COLLATE 'C' LC_CTYPE 'C' TEMPLATE template0"} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	conn := liveScratchConn(t, ctx, db)
	for _, stmt := range []string{
		"CREATE SCHEMA app",
		`CREATE SCHEMA "Fn s"`,
		"CREATE EXTENSION citext SCHEMA app",
		"CREATE EXTENSION postgres_fdw",
		"CREATE SERVER remote FOREIGN DATA WRAPPER postgres_fdw OPTIONS (host 'remote.invalid')",
		"CREATE FUNCTION app.tf() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'",
		`CREATE FUNCTION "Fn s"."T f"() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'`,
		"CREATE TABLE app.p (id integer PRIMARY KEY)",
		`CREATE TABLE app.t (a integer REFERENCES app.p, b text, "C c" integer)`,
		`CREATE TRIGGER "Tr 1" BEFORE UPDATE OF "C c", b ON app.t FOR EACH ROW WHEN (OLD.b IS DISTINCT FROM NEW.b)
			EXECUTE FUNCTION "Fn s"."T f"('x', 'it''s', '', 'a\000b', 'é€')`,
		`ALTER TABLE app.t ENABLE REPLICA TRIGGER "Tr 1"`,
		"CREATE TRIGGER tr2 AFTER INSERT ON app.t REFERENCING NEW TABLE AS nt FOR EACH STATEMENT EXECUTE FUNCTION app.tf()",
		"ALTER TABLE app.t DISABLE TRIGGER tr2",
		"COMMENT ON TRIGGER tr2 ON app.t IS 'the second'",
		`CREATE TRIGGER "TR2" AFTER UPDATE ON app.t REFERENCING OLD TABLE AS "o t" NEW TABLE AS nt
			FOR EACH STATEMENT EXECUTE FUNCTION app.tf('')`,
		"CREATE CONSTRAINT TRIGGER pc AFTER DELETE ON app.p DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION app.tf()",
		"CREATE VIEW app.v AS SELECT a, b FROM app.t",
		"CREATE TRIGGER vi INSTEAD OF INSERT ON app.v FOR EACH ROW EXECUTE FUNCTION app.tf('v')",
		"CREATE TRIGGER vs BEFORE UPDATE ON app.v FOR EACH STATEMENT EXECUTE FUNCTION app.tf()",
		"CREATE TABLE app.pt (id integer) PARTITION BY RANGE (id)",
		"CREATE TABLE app.pt1 PARTITION OF app.pt FOR VALUES FROM (0) TO (10)",
		"CREATE TRIGGER pti AFTER INSERT ON app.pt FOR EACH ROW EXECUTE FUNCTION app.tf()",
		"CREATE FOREIGN TABLE app.ft (id integer) SERVER remote",
		"CREATE TRIGGER fti AFTER INSERT ON app.ft FOR EACH ROW EXECUTE FUNCTION app.tf()",
		"CREATE TABLE app.ext_t (id integer)",
		"ALTER EXTENSION citext ADD TABLE app.ext_t",
		"CREATE TRIGGER exi AFTER INSERT ON app.ext_t FOR EACH ROW EXECUTE FUNCTION app.tf()",
		"GRANT USAGE ON SCHEMA app TO " + q(plain),
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%.60s: %v", stmt, err)
		}
	}

	s, err := Connect(ctx, ConnectionOptions{ConnString: *liveConnString, Database: db, ApplicationName: "pgmo - live triggers"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	d, err := s.DatabaseByName(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	all, err := d.Triggers(ctx, ObjectFilter{})
	if err != nil {
		t.Fatal(err)
	}
	for _, tr := range all {
		if tr.Database() != d || tr.OID == 0 || !strings.HasPrefix(tr.Definition, "CREATE ") {
			t.Errorf("%s on %s: Database %v, OID %d, Definition %q", tr.Name, tr.QualifiedTable(), tr.Database(), tr.OID, tr.Definition)
		}
	}
	tf, uf := ObjectName{"app", "tf"}, ObjectName{"Fn s", "T f"}
	want := []Trigger{
		{Origin: Origin{Extension: "citext"}, Schema: "app", Table: "ext_t", TableKind: RelationTable, Name: "exi",
			Timing: TriggerAfter, Events: []TriggerEvent{TriggerInsert}, ForEachRow: true, Function: tf, Enabled: TriggerEnabledOrigin},
		{Schema: "app", Table: "ft", TableKind: RelationForeignTable, Name: "fti",
			Timing: TriggerAfter, Events: []TriggerEvent{TriggerInsert}, ForEachRow: true, Function: tf, Enabled: TriggerEnabledOrigin},
		{Schema: "app", Table: "p", TableKind: RelationTable, Name: "pc",
			Timing: TriggerAfter, Events: []TriggerEvent{TriggerDelete}, ForEachRow: true, Function: tf, Enabled: TriggerEnabledOrigin,
			IsConstraint: true, Deferrable: true, InitiallyDeferred: true},
		{Schema: "app", Table: "pt", TableKind: RelationPartitionedTable, Name: "pti",
			Timing: TriggerAfter, Events: []TriggerEvent{TriggerInsert}, ForEachRow: true, Function: tf, Enabled: TriggerEnabledOrigin},
		{Schema: "app", Table: "pt1", TableKind: RelationTable, Name: "pti",
			Timing: TriggerAfter, Events: []TriggerEvent{TriggerInsert}, ForEachRow: true, Function: tf, Enabled: TriggerEnabledOrigin,
			Inherited: true},
		{Schema: "app", Table: "t", TableKind: RelationTable, Name: "TR2",
			Timing: TriggerAfter, Events: []TriggerEvent{TriggerUpdate}, Function: tf, Arguments: []string{""},
			Enabled: TriggerEnabledOrigin, OldTable: "o t", NewTable: "nt"},
		{Schema: "app", Table: "t", TableKind: RelationTable, Name: "Tr 1",
			Timing: TriggerBefore, Events: []TriggerEvent{TriggerUpdate}, UpdateColumns: []string{"C c", "b"}, ForEachRow: true,
			Function: uf, Arguments: []string{"x", "it's", "", `a\000b`, "é€"}, Enabled: TriggerEnabledReplica, HasCondition: true},
		{Schema: "app", Table: "t", TableKind: RelationTable, Name: "tr2",
			Timing: TriggerAfter, Events: []TriggerEvent{TriggerInsert}, Function: tf, Enabled: TriggerDisabled,
			NewTable: "nt", Comment: "the second"},
		{Schema: "app", Table: "v", TableKind: RelationView, Name: "vi",
			Timing: TriggerInsteadOf, Events: []TriggerEvent{TriggerInsert}, ForEachRow: true, Function: tf, Arguments: []string{"v"},
			Enabled: TriggerEnabledOrigin},
		{Schema: "app", Table: "v", TableKind: RelationView, Name: "vs",
			Timing: TriggerBefore, Events: []TriggerEvent{TriggerUpdate}, Function: tf, Enabled: TriggerEnabledOrigin},
	}
	if len(all) != len(want) {
		t.Fatalf("Triggers = %d:\n%s\nwant %d, no foreign key trigger", len(all), dumpTriggers(all), len(want))
	}
	defs := map[string]string{
		"Tr 1": `CREATE TRIGGER "Tr 1" BEFORE UPDATE OF "C c", b ON app.t FOR EACH ROW WHEN ((old.b IS DISTINCT FROM new.b)) ` +
			`EXECUTE FUNCTION "Fn s"."T f"('x', 'it''s', '', 'a\000b', 'é€')`,
		"vi": "CREATE TRIGGER vi INSTEAD OF INSERT ON app.v FOR EACH ROW EXECUTE FUNCTION app.tf('v')",
	}
	for i := range all {
		g := *all[i]
		g.db, g.OID = nil, 0
		if def, ok := defs[g.Name]; ok && g.Definition != def {
			t.Errorf("%s: Definition %q, want %q", g.Name, g.Definition, def)
		}
		g.Definition = ""
		if !reflect.DeepEqual(g, want[i]) {
			t.Errorf("Triggers[%d] =\n%+v\nwant\n%+v", i, g, want[i])
		}
	}

	// Every trigger the server did not make, once, with tgargs split here.
	rows, err := conn.Query(ctx, "SELECT oid, tgnargs::integer, tgargs FROM pg_catalog.pg_trigger WHERE NOT tgisinternal")
	if err != nil {
		t.Fatal(err)
	}
	type rawArgs struct {
		n    int
		args []byte
	}
	raw := map[OID]rawArgs{}
	for rows.Next() {
		var oid OID
		var r rawArgs
		if err := rows.Scan(&oid, &r.n, &r.args); err != nil {
			t.Fatal(err)
		}
		raw[oid] = r
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(raw) != len(all) {
		t.Errorf("pg_trigger holds %d non-internal triggers, the listing %d", len(raw), len(all))
	}
	for _, tr := range all {
		r, ok := raw[tr.OID]
		if !ok {
			t.Errorf("%s: OID %d not a non-internal pg_trigger row", tr.Name, tr.OID)
			continue
		}
		var split []string
		for _, a := range bytes.Split(r.args, []byte{0}) {
			split = append(split, string(a))
		}
		split = split[:r.n] // the piece after the last NUL
		if r.n == 0 {
			split = nil
		}
		if !slices.Equal(tr.Arguments, split) {
			t.Errorf("%s: Arguments %q, tgargs splits into %q", tr.Name, tr.Arguments, split)
		}
	}

	// Filters: the name exactly, by case; the extension's; no system one.
	no, yes := false, true
	for _, c := range []struct {
		f    ObjectFilter
		want []string
	}{
		{ObjectFilter{Name: []TextCriterion{{TextEquals, `"TR2"`}}}, []string{"TR2"}},
		{ObjectFilter{Name: []TextCriterion{{TextEquals, "TR2"}}}, []string{"tr2"}},
		{ObjectFilter{ExtensionMember: &yes}, []string{"exi"}},
		{ObjectFilter{System: &yes}, nil},
		{ObjectFilter{Schema: []TextCriterion{{TextEquals, "app"}}, System: &no, ExtensionMember: &no},
			[]string{"fti", "pc", "pti", "pti", "TR2", "Tr 1", "tr2", "vi", "vs"}},
	} {
		ts, err := d.Triggers(ctx, c.f)
		var got []string
		for _, tr := range ts {
			got = append(got, tr.Name)
		}
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("Triggers(%+v) = %q, %v; want %q", c.f, got, err, c.want)
		}
	}

	// By name: exact, missing, the foreign key's own, another table's, no
	// schema.
	tr, err := d.TriggerByName(ctx, "app", "t", "TR2")
	if err != nil || tr.Name != "TR2" || tr.OldTable != "o t" {
		t.Errorf("TriggerByName(app.t, TR2) = %+v, %v", tr, err)
	}
	var fkName string
	if err := conn.QueryRow(ctx, "SELECT tgname::text FROM pg_catalog.pg_trigger WHERE tgrelid = 'app.t'::regclass AND tgisinternal LIMIT 1").Scan(&fkName); err != nil {
		t.Fatal(err)
	}
	for _, c := range [][3]string{{"app", "t", "Tr2"}, {"app", "t", fkName}, {"app", "p", "tr2"}, {"app", "nope", "tr2"}, {"App", "t", "tr2"}} {
		if _, err := d.TriggerByName(ctx, c[0], c[1], c[2]); !errors.Is(err, ErrNotFound) {
			t.Errorf("TriggerByName(%q) err = %v, want ErrNotFound", c, err)
		}
	}
	if _, err := d.TriggerByName(ctx, "", "t", "tr2"); !errors.Is(err, ErrSchemaRequired) {
		t.Errorf("no schema: err = %v, want ErrSchemaRequired", err)
	}

	// Per relation: the partition's inherited one, the view's two, a Ref's
	// refusal.
	pt1, err := d.TableByName(ctx, "app", "pt1")
	if err != nil {
		t.Fatal(err)
	}
	if ts, err := pt1.Triggers(ctx); err != nil || len(ts) != 1 || !ts[0].Inherited || ts[0].Table != "pt1" {
		t.Errorf("Triggers(app.pt1) = %s, %v; want pti, inherited", dumpTriggers(ts), err)
	}
	v, err := d.ViewByName(ctx, "app", "v")
	if err != nil {
		t.Fatal(err)
	}
	if ts, err := v.Triggers(ctx); err != nil || dumpTriggers(ts) != dumpTriggers(all[8:]) {
		t.Errorf("Triggers(app.v) = %s, %v; want\n%s", dumpTriggers(ts), err, dumpTriggers(all[8:]))
	}
	if _, err := d.ViewRef("app", "v").Triggers(ctx); !errors.Is(err, ErrHandleNotLoaded) {
		t.Errorf("Triggers on a ViewRef: err = %v, want ErrHandleNotLoaded", err)
	}

	// The same listing under standard_conforming_strings off, and as a role
	// with no privilege on any of it.
	for _, set := range []string{"SET LOCAL standard_conforming_strings = off", "SET LOCAL ROLE " + q(plain)} {
		var again []*Trigger
		err = s.InTransaction(ctx, func(ctx context.Context) error {
			if _, err := s.on(ctx).Exec(ctx, set); err != nil {
				return err
			}
			var err error
			again, err = d.Triggers(ctx, ObjectFilter{})
			return err
		})
		if err != nil {
			t.Fatalf("%s: %v", set, err)
		}
		wantAgain := all
		if strings.Contains(set, "standard_conforming_strings") {
			// The definition writes its literals as the session reads them:
			// a backslash doubled. The arguments are the same.
			wantAgain = nil
			for _, tr := range all {
				c := *tr
				c.Definition = strings.ReplaceAll(c.Definition, `'a\000b'`, `'a\\000b'`)
				wantAgain = append(wantAgain, &c)
			}
		}
		if dumpTriggers(again) != dumpTriggers(wantAgain) {
			t.Errorf("after %s the triggers differ:\n%s\nwant\n%s", set, dumpTriggers(again), dumpTriggers(wantAgain))
		}
	}

	// A LATIN1 database's argument, converted for the UTF-8 client.
	lconn := liveScratchConn(t, ctx, latin)
	for _, stmt := range []string{
		"CREATE FUNCTION public.tf() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'",
		"CREATE TABLE public.t (a integer)",
		"CREATE TRIGGER l AFTER INSERT ON public.t FOR EACH ROW EXECUTE FUNCTION public.tf('é', 'x\\y', '')",
	} {
		if _, err := lconn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	lt, err := s.DatabaseRef(latin).TriggerByName(ctx, "public", "t", "l")
	if err != nil || !slices.Equal(lt.Arguments, []string{"é", `x\y`, ""}) {
		t.Errorf("LATIN1 trigger = %+v, %v; want arguments é, x\\y and ''", lt, err)
	}
}

// TestLiveEventTriggers reads the event triggers of a throwaway database:
//
//   - e1 on ddl_command_start for two command tags (written in mixed case),
//     commented;
//   - e2 on sql_drop, ENABLE ALWAYS, adopted by citext (an extension
//     member);
//   - "E1", differing from e1 only by case, on table_rewrite, DISABLEd,
//     running "Fn s"."E f";
//   - on 17+, el on login.
//
// The listing matches pg_event_trigger row for row; it is read again as a
// plain role (identical) and from another database (none of them: an event
// trigger is its database's).
func TestLiveEventTriggers(t *testing.T) {
	admin, ctx, done := liveDBTimeout(t, 2*time.Minute)
	t.Cleanup(done)
	stamp := time.Now().UnixNano()
	name := func(s string) string { return fmt.Sprintf("pgmo_live_evt_%d_%s", stamp, s) }
	plain, db, other := name("plain"), name("db"), name("other")
	q := pgsql.QuoteIdent

	t.Cleanup(func() {
		for _, d := range []string{db, other} {
			if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+q(d)+" WITH (FORCE)"); err != nil {
				t.Errorf("drop database: %v", err)
			}
		}
		if _, err := admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+q(plain)); err != nil {
			t.Errorf("drop role: %v", err)
		}
	})
	for _, stmt := range []string{"CREATE DATABASE " + q(db), "CREATE DATABASE " + q(other), "CREATE ROLE " + q(plain)} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	conn := liveScratchConn(t, ctx, db)
	stmts := []string{
		"CREATE SCHEMA app",
		`CREATE SCHEMA "Fn s"`,
		"CREATE EXTENSION citext SCHEMA app",
		"CREATE FUNCTION app.ef() RETURNS event_trigger LANGUAGE plpgsql AS 'BEGIN END'",
		`CREATE FUNCTION "Fn s"."E f"() RETURNS event_trigger LANGUAGE plpgsql AS 'BEGIN END'`,
		"CREATE EVENT TRIGGER e1 ON ddl_command_start WHEN TAG IN ('Create Table', 'drop table') EXECUTE FUNCTION app.ef()",
		"COMMENT ON EVENT TRIGGER e1 IS 'the first'",
		"CREATE EVENT TRIGGER e2 ON sql_drop EXECUTE FUNCTION app.ef()",
		"ALTER EVENT TRIGGER e2 ENABLE ALWAYS",
		"ALTER EXTENSION citext ADD EVENT TRIGGER e2",
		`CREATE EVENT TRIGGER "E1" ON table_rewrite EXECUTE FUNCTION "Fn s"."E f"()`,
		`ALTER EVENT TRIGGER "E1" DISABLE`,
	}
	s, err := Connect(ctx, ConnectionOptions{ConnString: *liveConnString, Database: db, ApplicationName: "pgmo - live event triggers"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	login := s.Version() >= PostgreSQL17
	if login {
		stmts = append(stmts, "CREATE EVENT TRIGGER el ON login EXECUTE FUNCTION app.ef()")
	}
	for _, stmt := range stmts {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	var owner string
	if err := conn.QueryRow(ctx, "SELECT current_user::text").Scan(&owner); err != nil {
		t.Fatal(err)
	}
	d, err := s.DatabaseByName(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	ef := ObjectName{"app", "ef"}
	want := []EventTrigger{
		{Name: "E1", Owner: owner, Event: EventTriggerTableRewrite, Function: ObjectName{"Fn s", "E f"}, Enabled: TriggerDisabled},
		{Name: "e1", Owner: owner, Event: EventTriggerDDLCommandStart, Function: ef, Enabled: TriggerEnabledOrigin,
			Tags: []string{"CREATE TABLE", "DROP TABLE"}, Comment: "the first"},
		{Origin: Origin{Extension: "citext"}, Name: "e2", Owner: owner, Event: EventTriggerSQLDrop, Function: ef,
			Enabled: TriggerEnabledAlways},
	}
	if login {
		want = append(want, EventTrigger{Name: "el", Owner: owner, Event: EventTriggerLogin, Function: ef, Enabled: TriggerEnabledOrigin})
	}
	dump := func(es []*EventTrigger) string {
		var sb strings.Builder
		for _, e := range es {
			c := *e
			c.db = nil
			fmt.Fprintf(&sb, "%+v\n", c)
		}
		return sb.String()
	}
	all, err := d.EventTriggers(ctx, ObjectFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != len(want) {
		t.Fatalf("EventTriggers = %s, want %d", dump(all), len(want))
	}
	oids := map[OID]string{}
	rows, err := conn.Query(ctx, "SELECT oid, evtname::text FROM pg_catalog.pg_event_trigger")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var oid OID
		var n string
		if err := rows.Scan(&oid, &n); err != nil {
			t.Fatal(err)
		}
		oids[oid] = n
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	if len(oids) != len(all) {
		t.Errorf("pg_event_trigger holds %d rows, the listing %d", len(oids), len(all))
	}
	for i, e := range all {
		if e.Database() != d || oids[e.OID] != e.Name {
			t.Errorf("%s: Database %v, OID %d (pg_event_trigger's is %q's)", e.Name, e.Database(), e.OID, oids[e.OID])
		}
		g := *e
		g.db, g.OID = nil, 0
		if !reflect.DeepEqual(g, want[i]) {
			t.Errorf("EventTriggers[%d] =\n%+v\nwant\n%+v", i, g, want[i])
		}
	}

	yes := true
	for _, c := range []struct {
		f    ObjectFilter
		want []string
	}{
		{ObjectFilter{Name: []TextCriterion{{TextEquals, `"E1"`}}}, []string{"E1"}},
		{ObjectFilter{Name: []TextCriterion{{TextEquals, "E1"}}}, []string{"e1"}},
		{ObjectFilter{ExtensionMember: &yes}, []string{"e2"}},
		{ObjectFilter{System: &yes}, nil},
		{ObjectFilter{Schema: []TextCriterion{{TextEquals, "nowhere"}}, Name: []TextCriterion{{TextContains, "1"}}}, []string{"E1", "e1"}},
	} {
		es, err := d.EventTriggers(ctx, c.f)
		var got []string
		for _, e := range es {
			got = append(got, e.Name)
		}
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("EventTriggers(%+v) = %q, %v; want %q", c.f, got, err, c.want)
		}
	}

	if e, err := d.EventTriggerByName(ctx, "E1"); err != nil || e.Name != "E1" || e.Enabled != TriggerDisabled {
		t.Errorf("EventTriggerByName(E1) = %+v, %v", e, err)
	}
	if _, err := d.EventTriggerByName(ctx, "E2"); !errors.Is(err, ErrNotFound) {
		t.Errorf("EventTriggerByName(E2): err = %v, want ErrNotFound", err)
	}

	var asPlain []*EventTrigger
	err = s.InTransaction(ctx, func(ctx context.Context) error {
		if _, err := s.on(ctx).Exec(ctx, "SET LOCAL ROLE "+q(plain)); err != nil {
			return err
		}
		var err error
		asPlain, err = d.EventTriggers(ctx, ObjectFilter{})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if dump(asPlain) != dump(all) {
		t.Errorf("as a plain role the event triggers differ:\n%s\nwant\n%s", dump(asPlain), dump(all))
	}
	if es, err := s.DatabaseRef(other).EventTriggers(ctx, ObjectFilter{}); err != nil || len(es) != 0 {
		t.Errorf("EventTriggers of another database = %s, %v; want none", dump(es), err)
	}
}
