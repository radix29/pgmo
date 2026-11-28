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

// The column types of the cast read, in SELECT order, as the server sends
// them.
var castTypes = []uint32{pgtype.OIDOID, pgtype.BoolOID, pgtype.TextOID,
	pgtype.OIDOID, pgtype.TextOID, pgtype.OIDOID, pgtype.TextOID,
	pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID}

// castMatch tells the cast read apart in a fake.
const castMatch = "FROM pg_catalog.pg_cast ca"

// Two casts with every column set to something its neighbours are not.
var (
	rowCast1 = []any{uint32(10001), true, "citext",
		uint32(20), "bigint", uint32(23), "integer",
		[]string{"pg_catalog", "int4"}, []string{"bigint"}, "a", "f", "narrowing"}
	rowCast2 = []any{uint32(70003), false, "",
		uint32(70010), `app."Mood"`, uint32(25), "text",
		[]string{}, nil, "i", "i", ""}
)

// The rows above as Casts, without their Database.
var wantCasts = []pgmo.Cast{
	{Origin: pgmo.Origin{IsSystem: true, Extension: "citext"}, OID: 10001, SourceType: 20, TargetType: 23,
		Source: "bigint", Target: "integer", Function: pgmo.ObjectName{Schema: "pg_catalog", Name: "int4"},
		FunctionArguments: []string{"bigint"}, Context: pgmo.CastAssignment, Method: pgmo.CastByFunction, Comment: "narrowing"},
	{OID: 70003, SourceType: 70010, TargetType: 25, Source: `app."Mood"`, Target: "text",
		Context: pgmo.CastImplicit, Method: pgmo.CastByInOut},
}

// checkCasts compares got with the first len(got) of wantCasts, each read
// from d.
func checkCasts(t *testing.T, got []*pgmo.Cast, d *pgmo.Database) {
	t.Helper()
	for i, c := range got {
		if c.Database() != d {
			t.Errorf("[%d].Database() = %v, want %v", i, c.Database(), d)
		}
		g := *c
		g.SetDatabase(nil)
		if !reflect.DeepEqual(g, wantCasts[i]) {
			t.Errorf("[%d] =\n%+v\nwant\n%+v", i, g, wantCasts[i])
		}
	}
	if n := reflect.TypeFor[pgmo.Cast]().NumField(); n != 12 {
		t.Errorf("Cast has %d fields; this test checks 12 — add the new one", n)
	}
}

// The fragments every cast read holds.
var castFragments = []string{
	"SELECT ca.oid, ca.oid < 16384, COALESCE(ext.extname::pg_catalog.text, ''),\n\tca.castsource, CASE",
	"ca.castcontext::pg_catalog.text, ca.castmethod::pg_catalog.text,",
	castMatch,
	"JOIN pg_catalog.pg_type st ON st.oid = ca.castsource",
	"JOIN pg_catalog.pg_type tt ON tt.oid = ca.casttarget",
	"LEFT JOIN pg_catalog.pg_proc cf ON cf.oid = ca.castfunc::pg_catalog.oid",
	"d.classid = 'pg_catalog.pg_cast'::pg_catalog.regclass AND d.objid = ca.oid",
	"LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = ca.oid\n\tAND ds.classoid = 'pg_catalog.pg_cast'::pg_catalog.regclass AND ds.objsubid = 0",
}

// The listing fills every field, pushes its filter into the query —
// system by OID and extension membership; a cast has no name or schema
// column — orders by the type names, and goes to the database's own pool.
func TestCastsFiltered(t *testing.T) {
	yes := true
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On(castMatch, rowsOf(castTypes, rowCast1, rowCast2))
	d := f.Server().DatabaseRef("appdb")
	cs, err := d.Casts(t.Context(), pgmo.ObjectFilter{
		Name:            []pgmo.TextCriterion{{Op: pgmo.TextEquals, Value: "int4"}},
		Schema:          []pgmo.TextCriterion{{Op: pgmo.TextEquals, Value: "app"}},
		System:          &yes,
		ExtensionMember: &yes,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 {
		t.Fatalf("%d casts, want 2", len(cs))
	}
	checkCasts(t, cs, d)
	if cs[0].Signature() != "(bigint AS integer)" || cs[1].Signature() != `(app."Mood" AS text)` {
		t.Errorf("Signature() = %s, %s", cs[0].Signature(), cs[1].Signature())
	}
	if n := len(f.Queries()); n != 1 {
		t.Errorf("%d statements on the Server's own database, want 1: the listing went to appdb", n)
	}
	last := lastQuery(other)
	for _, w := range castFragments {
		if !strings.Contains(last.Text, w) {
			t.Errorf("no %q in\n%s", w, last.Text)
		}
	}
	tail := "WHERE true AND ca.oid < 16384 AND EXISTS (SELECT FROM pg_catalog.pg_depend d WHERE " +
		"d.classid = 'pg_catalog.pg_cast'::pg_catalog.regclass AND d.objid = ca.oid"
	if !strings.Contains(last.Text, tail) || !strings.HasSuffix(last.Text, "\nORDER BY 5, 7") || len(last.Args) != 0 {
		t.Errorf("filter not pushed down, then ordered: args %v\n%s", last.Args, last.Text)
	}
}

// CastByTypes binds the OIDs as $1 and $2, fills every field, and tells a
// missing cast from a failed read; the Ref reads nothing.
func TestCastByTypes(t *testing.T) {
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On(castMatch, rowsOf(castTypes, rowCast2))
	d := f.Server().DatabaseRef("appdb")
	c, err := d.CastByTypes(t.Context(), 70010, 25)
	if err != nil {
		t.Fatal(err)
	}
	g := *c
	g.SetDatabase(nil)
	if c.Database() != d || !reflect.DeepEqual(g, wantCasts[1]) {
		t.Errorf("= %+v, want %+v", g, wantCasts[1])
	}
	last := lastQuery(other)
	if !strings.HasSuffix(last.Text, "WHERE true\n\tAND ca.castsource = $1::pg_catalog.oid AND ca.casttarget = $2::pg_catalog.oid") ||
		last.Arg(0) != pgmo.OID(70010) || last.Arg(1) != pgmo.OID(25) {
		t.Errorf("types not bound as $1, $2: args %v\n%s", last.Args, last.Text)
	}

	f = pgmotest.New(t)
	f.On(castMatch, rowsOf(castTypes))
	d = f.Server().DatabaseRef(pgmotest.DefaultDatabase)
	if _, err := d.CastByTypes(t.Context(), 1, 2); !errors.Is(err, pgmo.ErrNotFound) ||
		!strings.Contains(err.Error(), `cast from type 1 to type 2 not found in database "`+pgmotest.DefaultDatabase+`"`) {
		t.Errorf("missing: err = %v, want ErrNotFound naming it", err)
	}
	f = pgmotest.New(t)
	f.On(castMatch, pgmotest.Fail(&pgconn.PgError{Code: "57014", Message: "canceled"}))
	d = f.Server().DatabaseRef(pgmotest.DefaultDatabase)
	_, err = d.CastByTypes(t.Context(), 1, 2)
	if err == nil || errors.Is(err, pgmo.ErrNotFound) || !strings.HasPrefix(err.Error(), "pgmo: read cast from type 1 to type 2 of database ") ||
		strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("server error: err = %v, want it wrapped once naming the read", err)
	}
	_, err = d.Casts(t.Context(), pgmo.ObjectFilter{})
	if err == nil || !strings.HasPrefix(err.Error(), `pgmo: list casts of database "`) || strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("listing, server error: err = %v, want it wrapped once naming the read", err)
	}

	n := len(f.Queries())
	r := d.CastRef("app.mood", "integer")
	g = *r
	g.SetDatabase(nil)
	if r.Database() != d || !reflect.DeepEqual(g, pgmo.Cast{Source: "app.mood", Target: "integer"}) ||
		r.Signature() != "(app.mood AS integer)" {
		t.Errorf("CastRef = %+v", r)
	}
	if len(f.Queries()) != n {
		t.Errorf("CastRef sent a statement")
	}
}
