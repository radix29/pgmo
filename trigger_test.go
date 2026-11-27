package pgmo_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/radix29/pgmo"
	"github.com/radix29/pgmo/pgmotest"
)

// The column types of the trigger read, in SELECT order, as the server sends
// them.
var triggerTypes = []uint32{pgtype.OIDOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.BoolOID, pgtype.TextOID,
	pgtype.TextOID, pgtype.TextOID, pgtype.Int4OID, pgtype.TextArrayOID,
	pgtype.TextOID, pgtype.TextOID, pgtype.TextArrayOID,
	pgtype.TextOID, pgtype.BoolOID, pgtype.BoolOID, pgtype.BoolOID, pgtype.BoolOID,
	pgtype.TextOID, pgtype.TextOID, pgtype.BoolOID, pgtype.TextOID}

// triggerMatch tells the trigger read apart in a fake.
const triggerMatch = "FROM pg_catalog.pg_trigger tg"

// Three triggers with every column set to something its neighbours are not.
var (
	// 1|2|4|16: ROW BEFORE INSERT UPDATE, on a partition of an extension's
	// table in a system schema, with arguments and a WHEN.
	rowTrigger1 = []any{uint32(60001), "pg_x", "kid", "r", true, "citext",
		"t1", "CREATE TRIGGER t1 …", int32(23), []string{"a", "b"},
		"app", "touch", []string{"", "x y"},
		"R", false, false, false, true,
		"", "", true, "t"}
	// 8|32|64: INSTEAD OF DELETE TRUNCATE, per statement, a constraint
	// trigger with both transition tables.
	rowTrigger2 = []any{uint32(60002), "app", "names", "v", false, "",
		"t2", "CREATE CONSTRAINT TRIGGER t2 …", int32(104), []string{},
		"Other", "f g", []string{},
		"D", true, true, true, false,
		"old rows", "new", false, ""}
	// 4: AFTER INSERT, per statement, on a foreign table.
	rowTrigger3 = []any{uint32(60003), "app", "remote", "f", false, "",
		"t3", "CREATE TRIGGER t3 …", int32(4), nil,
		"app", "touch", nil,
		"A", false, false, false, false,
		"", "", false, ""}
)

// The rows above as Triggers, without their Database.
var wantTriggers = []pgmo.Trigger{
	{Origin: pgmo.Origin{IsSystem: true, Extension: "citext"}, OID: 60001, Schema: "pg_x", Table: "kid", TableKind: pgmo.RelationTable,
		Name: "t1", Definition: "CREATE TRIGGER t1 …", Timing: pgmo.TriggerBefore,
		Events: []pgmo.TriggerEvent{pgmo.TriggerInsert, pgmo.TriggerUpdate}, UpdateColumns: []string{"a", "b"}, ForEachRow: true,
		Function: pgmo.ObjectName{Schema: "app", Name: "touch"}, Arguments: []string{"", "x y"},
		Enabled: pgmo.TriggerEnabledReplica, Inherited: true, HasCondition: true, Comment: "t"},
	{OID: 60002, Schema: "app", Table: "names", TableKind: pgmo.RelationView,
		Name: "t2", Definition: "CREATE CONSTRAINT TRIGGER t2 …", Timing: pgmo.TriggerInsteadOf,
		Events:   []pgmo.TriggerEvent{pgmo.TriggerDelete, pgmo.TriggerTruncate},
		Function: pgmo.ObjectName{Schema: "Other", Name: "f g"}, Enabled: pgmo.TriggerDisabled,
		IsConstraint: true, Deferrable: true, InitiallyDeferred: true, OldTable: "old rows", NewTable: "new"},
	{OID: 60003, Schema: "app", Table: "remote", TableKind: pgmo.RelationForeignTable,
		Name: "t3", Definition: "CREATE TRIGGER t3 …", Timing: pgmo.TriggerAfter,
		Events: []pgmo.TriggerEvent{pgmo.TriggerInsert}, Function: pgmo.ObjectName{Schema: "app", Name: "touch"},
		Enabled: pgmo.TriggerEnabledAlways},
}

// checkTriggers compares got with the first n of wantTriggers, each read
// from d.
func checkTriggers(t *testing.T, got []*pgmo.Trigger, n int, d *pgmo.Database) {
	t.Helper()
	if len(got) != n {
		t.Fatalf("%d triggers, want %d", len(got), n)
	}
	for i, tr := range got {
		if tr.Database() != d {
			t.Errorf("[%d].Database() = %v, want %v", i, tr.Database(), d)
		}
		g := *tr
		g.SetDatabase(nil)
		if !reflect.DeepEqual(g, wantTriggers[i]) {
			t.Errorf("[%d] =\n%+v\nwant\n%+v", i, g, wantTriggers[i])
		}
	}
	if n := reflect.TypeFor[pgmo.Trigger]().NumField(); n != 23 {
		t.Errorf("Trigger has %d fields; this test checks 23 — add the new one", n)
	}
}

// The fragments every trigger read holds: the relation's names and kind, the
// grouped UPDATE OF columns and arguments joined by (oid, tgrelid), the
// comment as a join, internal triggers left out.
var triggerFragments = []string{
	triggerMatch,
	"SELECT tg.oid, n.nspname::pg_catalog.text, c.relname::pg_catalog.text, c.relkind::pg_catalog.text,",
	"tg.tgname::pg_catalog.text, pg_catalog.pg_get_triggerdef(tg.oid),\n\ttg.tgtype::integer, uc.cols,",
	"pn.nspname::pg_catalog.text, p.proname::pg_catalog.text, ta.args,",
	"COALESCE(tg.tgoldtable::pg_catalog.text, ''), COALESCE(tg.tgnewtable::pg_catalog.text, ''),\n\ttg.tgqual IS NOT NULL,",
	"JOIN pg_catalog.pg_class c ON c.oid = tg.tgrelid",
	"JOIN pg_catalog.pg_proc p ON p.oid = tg.tgfoid",
	"pg_catalog.unnest(t1.tgattr::pg_catalog.int2[]) WITH ORDINALITY AS u(num, n)",
	"JOIN pg_catalog.pg_attribute a ON a.attrelid = k.tgrelid AND a.attnum = k.num",
	"pg_catalog.array_agg(a.attname::pg_catalog.text ORDER BY k.n) AS cols",
	"AS uc ON uc.oid = tg.oid AND uc.tgrelid = tg.tgrelid",
	"pg_catalog.convert_from(pg_catalog.decode(x.arg, 'escape'), pg_catalog.getdatabaseencoding())\n\t\t\tORDER BY x.n",
	`pg_catalog.replace(pg_catalog.encode(t2.tgargs, 'escape'),` + "\n\t\t\t" + `E'\\\\', E'\\134'), E'\\000')`,
	"WHERE x.n <= t2.tgnargs",
	"AS ta ON ta.oid = tg.oid AND ta.tgrelid = tg.tgrelid",
	"LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = tg.oid\n\tAND ds.classoid = 'pg_catalog.pg_trigger'::pg_catalog.regclass AND ds.objsubid = 0",
	"LEFT JOIN pg_catalog.pg_extension ext ON ext.oid = d.refobjid",
	"WHERE NOT tg.tgisinternal",
}

// checkTriggerFragments fails on a fragment the statement text lacks.
func checkTriggerFragments(t *testing.T, text string) {
	t.Helper()
	for _, w := range triggerFragments {
		if !strings.Contains(text, w) {
			t.Errorf("no %q in\n%s", w, text)
		}
	}
}

// Table.Triggers: every field from its own column; tgtype decoded into
// timing, events and level; empty arrays read as none; keyed by the OID.
func TestTableTriggers(t *testing.T) {
	_, other, tb := loadedTable(t, map[string]pgmotest.Responder{
		triggerMatch: rowsOf(triggerTypes, rowTrigger1, rowTrigger2, rowTrigger3)})
	ts, err := tb.Triggers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	checkTriggers(t, ts, 3, tb.Database())
	last := lastQuery(other)
	checkTriggerFragments(t, last.Text)
	if !strings.HasSuffix(last.Text, "WHERE NOT tg.tgisinternal\n\tAND tg.tgrelid = $1\nORDER BY tg.tgname") {
		t.Errorf("not narrowed to the table, then ordered by name:\n%s", last.Text)
	}
}

// View.Triggers is the same read, keyed by the view's OID.
func TestViewTriggers(t *testing.T) {
	_, other, v, _ := loadedViews(t, map[string]pgmotest.Responder{
		triggerMatch: rowsOf(triggerTypes, rowTrigger1, rowTrigger2, rowTrigger3)})
	ts, err := v.Triggers(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	checkTriggers(t, ts, 3, v.Database())
	last := lastQuery(other)
	if !strings.HasSuffix(last.Text, "\tAND tg.tgrelid = $1\nORDER BY tg.tgname") || last.Arg(0) != pgmo.OID(31001) {
		t.Errorf("not keyed by the view's OID: args %v\n%s", last.Args, last.Text)
	}
}

// The listing pushes its filter into the query — the trigger's name, its
// relation's schema, system and extension flags — orders by schema,
// relation, name, and goes to the database's own pool.
func TestTriggersFiltered(t *testing.T) {
	yes := true
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On(triggerMatch, rowsOf(triggerTypes, rowTrigger1, rowTrigger2, rowTrigger3))
	d := f.Server().DatabaseRef("appdb")
	ts, err := d.Triggers(t.Context(), pgmo.ObjectFilter{
		Name:            []pgmo.TextCriterion{{Op: pgmo.TextContains, Value: "T1"}},
		Schema:          []pgmo.TextCriterion{{Op: pgmo.TextEquals, Value: "app"}},
		System:          &yes,
		ExtensionMember: &yes,
	})
	if err != nil {
		t.Fatal(err)
	}
	checkTriggers(t, ts, 3, d)
	if n := len(f.Queries()); n != 1 {
		t.Errorf("%d statements on the Server's own database, want 1: the listing went to appdb", n)
	}
	last := lastQuery(other)
	checkTriggerFragments(t, last.Text)
	tail := "WHERE NOT tg.tgisinternal AND pg_catalog.strpos(tg.tgname, $1) > 0 AND n.nspname = $2::pg_catalog.text" +
		" AND (pg_catalog.starts_with(n.nspname, 'pg_') OR n.nspname = 'information_schema')" +
		" AND EXISTS (SELECT FROM pg_catalog.pg_depend d WHERE d.classid = 'pg_catalog.pg_class'::pg_catalog.regclass AND d.objid = c.oid"
	if !strings.Contains(last.Text, tail) || !strings.HasSuffix(last.Text, "\nORDER BY n.nspname, c.relname, tg.tgname") ||
		len(last.Args) != 2 || last.Arg(0) != "t1" || last.Arg(1) != "app" {
		t.Errorf("filter not pushed down, then ordered: args %v\n%s", last.Args, last.Text)
	}
	n := len(other.Queries())
	if _, err := d.Triggers(t.Context(), pgmo.ObjectFilter{Name: []pgmo.TextCriterion{{Op: 99, Value: "x"}}}); !errors.Is(err, pgmo.ErrInvalidRequest) {
		t.Errorf("unknown operator: err = %v, want ErrInvalidRequest", err)
	}
	if len(other.Queries()) != n {
		t.Errorf("a refused filter was sent")
	}
}

// TriggerByName binds the three names as $1–$3, fills every field, refuses
// a missing schema before sending, and tells a missing trigger from a
// failed read.
func TestTriggerByName(t *testing.T) {
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On(triggerMatch, rowsOf(triggerTypes, rowTrigger1))
	d := f.Server().DatabaseRef("appdb")
	tr, err := d.TriggerByName(t.Context(), "pg_x", "kid", "t1")
	if err != nil {
		t.Fatal(err)
	}
	checkTriggers(t, []*pgmo.Trigger{tr}, 1, d)
	last := lastQuery(other)
	checkTriggerFragments(t, last.Text)
	if !strings.HasSuffix(last.Text, "WHERE NOT tg.tgisinternal\n\tAND n.nspname = $1::pg_catalog.text AND c.relname = $2::pg_catalog.text AND tg.tgname = $3::pg_catalog.text") ||
		last.Arg(0) != "pg_x" || last.Arg(1) != "kid" || last.Arg(2) != "t1" || strings.Contains(last.Text, "'kid'") {
		t.Errorf("names not bound as $1–$3: args %v\n%s", last.Args, last.Text)
	}
	if tr.QualifiedTable() != "pg_x.kid" {
		t.Errorf("QualifiedTable() = %s", tr.QualifiedTable())
	}

	n := len(other.Queries())
	if _, err := d.TriggerByName(t.Context(), "", "kid", "t1"); !errors.Is(err, pgmo.ErrSchemaRequired) ||
		!strings.HasPrefix(err.Error(), "pgmo: read trigger t1 on kid: ") {
		t.Errorf("no schema: err = %v, want ErrSchemaRequired naming the trigger", err)
	}
	if len(other.Queries()) != n {
		t.Errorf("a schema-less read was sent")
	}

	f = pgmotest.New(t)
	f.On(triggerMatch, rowsOf(triggerTypes))
	d = f.Server().DatabaseRef(pgmotest.DefaultDatabase)
	_, err = d.TriggerByName(t.Context(), "app", "Kid", "gone")
	if !errors.Is(err, pgmo.ErrNotFound) || !strings.Contains(err.Error(), `trigger gone on app."Kid" not found in database "`+pgmotest.DefaultDatabase+`"`) {
		t.Errorf("missing: err = %v, want ErrNotFound naming it", err)
	}
	f = pgmotest.New(t)
	f.On(triggerMatch, pgmotest.Fail(&pgconn.PgError{Code: "57014", Message: "canceled"}))
	d = f.Server().DatabaseRef(pgmotest.DefaultDatabase)
	_, err = d.TriggerByName(t.Context(), "app", "kid", "t1")
	if err == nil || errors.Is(err, pgmo.ErrNotFound) || !strings.HasPrefix(err.Error(), "pgmo: read trigger t1 on app.kid of database ") ||
		strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("server error: err = %v, want it wrapped once naming the read", err)
	}
	_, err = d.Triggers(t.Context(), pgmo.ObjectFilter{})
	if err == nil || !strings.HasPrefix(err.Error(), `pgmo: list triggers of database "`) || strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("listing, server error: err = %v, want it wrapped once naming the read", err)
	}
}

// TriggerRef reads nothing and holds the three names alone.
func TestTriggerRef(t *testing.T) {
	f := pgmotest.New(t)
	d := f.Server().DatabaseRef("appdb")
	n := len(f.Queries())
	tr := d.TriggerRef("app", "kid", "t1")
	g := *tr
	g.SetDatabase(nil)
	if tr.Database() != d || !reflect.DeepEqual(g, pgmo.Trigger{Schema: "app", Table: "kid", Name: "t1"}) {
		t.Errorf("TriggerRef = %+v", tr)
	}
	if len(f.Queries()) != n || f.Database("appdb").Opens() != 0 {
		t.Errorf("TriggerRef sent a statement")
	}
}
