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

// The column types of the event trigger read, in SELECT order, as the server
// sends them.
var eventTriggerTypes = []uint32{pgtype.OIDOID, pgtype.TextOID, pgtype.TextOID, pgtype.BoolOID, pgtype.TextOID,
	pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextArrayOID, pgtype.TextOID}

// eventTriggerMatch tells the event trigger read apart in a fake.
const eventTriggerMatch = "FROM pg_catalog.pg_event_trigger et"

// Two event triggers with every column set to something its neighbours are
// not.
var (
	rowEventTrigger1 = []any{uint32(70001), "Audit", "own", true, "citext",
		"ddl_command_start", "app", "on ddl", "A", []string{"CREATE TABLE", "DROP TABLE"}, "the audit"}
	rowEventTrigger2 = []any{uint32(70002), "drops", "B b", false, "",
		"login", "Other", "f", "D", []string{}, ""}
)

// The rows above as EventTriggers, without their Database.
var wantEventTriggers = []pgmo.EventTrigger{
	{Origin: pgmo.Origin{IsSystem: true, Extension: "citext"}, OID: 70001, Name: "Audit", Owner: "own",
		Event: pgmo.EventTriggerDDLCommandStart, Function: pgmo.ObjectName{Schema: "app", Name: "on ddl"},
		Enabled: pgmo.TriggerEnabledAlways, Tags: []string{"CREATE TABLE", "DROP TABLE"}, Comment: "the audit"},
	{OID: 70002, Name: "drops", Owner: "B b", Event: pgmo.EventTriggerLogin,
		Function: pgmo.ObjectName{Schema: "Other", Name: "f"}, Enabled: pgmo.TriggerDisabled},
}

// checkEventTriggers compares got with the first len(got) of
// wantEventTriggers, each read from d.
func checkEventTriggers(t *testing.T, got []*pgmo.EventTrigger, d *pgmo.Database) {
	t.Helper()
	for i, e := range got {
		if e.Database() != d {
			t.Errorf("[%d].Database() = %v, want %v", i, e.Database(), d)
		}
		g := *e
		g.SetDatabase(nil)
		if !reflect.DeepEqual(g, wantEventTriggers[i]) {
			t.Errorf("[%d] =\n%+v\nwant\n%+v", i, g, wantEventTriggers[i])
		}
	}
	if n := reflect.TypeFor[pgmo.EventTrigger]().NumField(); n != 10 {
		t.Errorf("EventTrigger has %d fields; this test checks 10 — add the new one", n)
	}
}

// The fragments every event trigger read holds.
var eventTriggerFragments = []string{
	"SELECT et.oid, et.evtname::pg_catalog.text,\n\tpg_catalog.pg_get_userbyid(et.evtowner)::pg_catalog.text,\n\tet.oid < 16384, COALESCE(ext.extname::pg_catalog.text, ''),",
	"et.evtevent::pg_catalog.text, pn.nspname::pg_catalog.text, p.proname::pg_catalog.text,\n\tet.evtenabled::pg_catalog.text, et.evttags,",
	eventTriggerMatch,
	"JOIN pg_catalog.pg_proc p ON p.oid = et.evtfoid",
	"d.classid = 'pg_catalog.pg_event_trigger'::pg_catalog.regclass AND d.objid = et.oid",
	"LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = et.oid\n\tAND ds.classoid = 'pg_catalog.pg_event_trigger'::pg_catalog.regclass AND ds.objsubid = 0",
}

// The listing fills every field, pushes its filter into the query — name,
// system by OID, extension membership; a schema criterion is no column —
// orders by name, and goes to the database's own pool.
func TestEventTriggersFiltered(t *testing.T) {
	yes := true
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On(eventTriggerMatch, rowsOf(eventTriggerTypes, rowEventTrigger1, rowEventTrigger2))
	d := f.Server().DatabaseRef("appdb")
	es, err := d.EventTriggers(t.Context(), pgmo.ObjectFilter{
		Name:            []pgmo.TextCriterion{{Op: pgmo.TextNotEquals, Value: `"Audit"`}},
		Schema:          []pgmo.TextCriterion{{Op: pgmo.TextEquals, Value: "app"}},
		System:          &yes,
		ExtensionMember: &yes,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 2 {
		t.Fatalf("%d event triggers, want 2", len(es))
	}
	checkEventTriggers(t, es, d)
	if n := len(f.Queries()); n != 1 {
		t.Errorf("%d statements on the Server's own database, want 1: the listing went to appdb", n)
	}
	last := lastQuery(other)
	for _, w := range eventTriggerFragments {
		if !strings.Contains(last.Text, w) {
			t.Errorf("no %q in\n%s", w, last.Text)
		}
	}
	tail := "WHERE true AND et.evtname <> $1::pg_catalog.text AND et.oid < 16384 AND EXISTS (SELECT FROM pg_catalog.pg_depend d WHERE " +
		"d.classid = 'pg_catalog.pg_event_trigger'::pg_catalog.regclass AND d.objid = et.oid"
	if !strings.Contains(last.Text, tail) || !strings.HasSuffix(last.Text, "\nORDER BY et.evtname") ||
		len(last.Args) != 1 || last.Arg(0) != "Audit" || strings.Contains(last.Text, "nspname = $") {
		t.Errorf("filter not pushed down, then ordered: args %v\n%s", last.Args, last.Text)
	}
	n := len(other.Queries())
	if _, err := d.EventTriggers(t.Context(), pgmo.ObjectFilter{Name: []pgmo.TextCriterion{{Op: 99, Value: "x"}}}); !errors.Is(err, pgmo.ErrInvalidRequest) {
		t.Errorf("unknown operator: err = %v, want ErrInvalidRequest", err)
	}
	if len(other.Queries()) != n {
		t.Errorf("a refused filter was sent")
	}
}

// EventTriggerByName binds the name as $1, fills every field, and tells a
// missing event trigger from a failed read; the Ref reads nothing.
func TestEventTriggerByName(t *testing.T) {
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On(eventTriggerMatch, rowsOf(eventTriggerTypes, rowEventTrigger1))
	d := f.Server().DatabaseRef("appdb")
	e, err := d.EventTriggerByName(t.Context(), "Audit")
	if err != nil {
		t.Fatal(err)
	}
	checkEventTriggers(t, []*pgmo.EventTrigger{e}, d)
	last := lastQuery(other)
	if !strings.HasSuffix(last.Text, "WHERE true\n\tAND et.evtname = $1::pg_catalog.text") || last.Arg(0) != "Audit" ||
		strings.Contains(last.Text, "'Audit'") {
		t.Errorf("name not bound as $1: args %v\n%s", last.Args, last.Text)
	}
	if e.QualifiedName() != `"Audit"` {
		t.Errorf("QualifiedName() = %s", e.QualifiedName())
	}

	f = pgmotest.New(t)
	f.On(eventTriggerMatch, rowsOf(eventTriggerTypes))
	d = f.Server().DatabaseRef(pgmotest.DefaultDatabase)
	_, err = d.EventTriggerByName(t.Context(), "Gone")
	if !errors.Is(err, pgmo.ErrNotFound) || !strings.Contains(err.Error(), `event trigger "Gone" not found in database "`+pgmotest.DefaultDatabase+`"`) {
		t.Errorf("missing: err = %v, want ErrNotFound naming it", err)
	}
	f = pgmotest.New(t)
	f.On(eventTriggerMatch, pgmotest.Fail(&pgconn.PgError{Code: "57014", Message: "canceled"}))
	d = f.Server().DatabaseRef(pgmotest.DefaultDatabase)
	_, err = d.EventTriggerByName(t.Context(), "audit")
	if err == nil || errors.Is(err, pgmo.ErrNotFound) || !strings.HasPrefix(err.Error(), "pgmo: read event trigger audit of database ") ||
		strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("server error: err = %v, want it wrapped once naming the read", err)
	}
	_, err = d.EventTriggers(t.Context(), pgmo.ObjectFilter{})
	if err == nil || !strings.HasPrefix(err.Error(), `pgmo: list event triggers of database "`) || strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("listing, server error: err = %v, want it wrapped once naming the read", err)
	}

	n := len(f.Queries())
	r := d.EventTriggerRef("audit")
	g := *r
	g.SetDatabase(nil)
	if r.Database() != d || !reflect.DeepEqual(g, pgmo.EventTrigger{Name: "audit"}) {
		t.Errorf("EventTriggerRef = %+v", r)
	}
	if len(f.Queries()) != n {
		t.Errorf("EventTriggerRef sent a statement")
	}
}
