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

// The column types of the operator read, in SELECT order, as the server
// sends them.
var operatorTypes = []uint32{pgtype.OIDOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.BoolOID, pgtype.TextOID,
	pgtype.TextOID, pgtype.OIDOID, pgtype.TextOID, pgtype.OIDOID, pgtype.TextOID, pgtype.OIDOID, pgtype.TextOID,
	pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.TextArrayOID,
	pgtype.BoolOID, pgtype.BoolOID, pgtype.TextOID}

// operatorMatch tells the operator read apart in a fake.
const operatorMatch = "FROM pg_catalog.pg_operator o\n"

// Two operators with every column set to something its neighbours are not:
// an infix one, and a shell prefix one.
var (
	rowOperator1 = []any{uint32(96), "pg_catalog", "=", "postgres", true, "citext",
		"b", uint32(23), "integer", uint32(70010), `app."Mood"`, uint32(16), "boolean",
		[]string{"pg_catalog", "int4eq"}, []string{"pg_catalog", "="}, []string{"app", "<>"},
		[]string{"pg_catalog", "eqsel"}, []string{"pg_catalog", "eqjoinsel"},
		true, false, "equal"}
	rowOperator2 = []any{uint32(70004), "My s", "!!", "own", false, "",
		"l", uint32(0), "", uint32(20), "bigint", uint32(0), "",
		[]string{}, []string{}, []string{}, []string{}, []string{},
		false, true, ""}
)

// The rows above as Operators, without their Database.
var wantOperators = []pgmo.Operator{
	{Origin: pgmo.Origin{IsSystem: true, Extension: "citext"}, OID: 96, Schema: "pg_catalog", Name: "=", Owner: "postgres",
		Kind: pgmo.OperatorInfix, LeftType: 23, RightType: 70010, ResultType: 16,
		Left: "integer", Right: `app."Mood"`, Result: "boolean",
		Function:   pgmo.ObjectName{Schema: "pg_catalog", Name: "int4eq"},
		Commutator: pgmo.ObjectName{Schema: "pg_catalog", Name: "="}, Negator: pgmo.ObjectName{Schema: "app", Name: "<>"},
		Restrict: pgmo.ObjectName{Schema: "pg_catalog", Name: "eqsel"}, Join: pgmo.ObjectName{Schema: "pg_catalog", Name: "eqjoinsel"},
		Merges: true, Comment: "equal"},
	{OID: 70004, Schema: "My s", Name: "!!", Owner: "own", Kind: pgmo.OperatorPrefix, RightType: 20, Right: "bigint",
		Hashes: true},
}

// checkOperators compares got with the first len(got) of wantOperators,
// each read from d.
func checkOperators(t *testing.T, got []*pgmo.Operator, d *pgmo.Database) {
	t.Helper()
	for i, o := range got {
		if o.Database() != d {
			t.Errorf("[%d].Database() = %v, want %v", i, o.Database(), d)
		}
		g := *o
		g.SetDatabase(nil)
		if !reflect.DeepEqual(g, wantOperators[i]) {
			t.Errorf("[%d] =\n%+v\nwant\n%+v", i, g, wantOperators[i])
		}
	}
	if n := reflect.TypeFor[pgmo.Operator]().NumField(); n != 21 {
		t.Errorf("Operator has %d fields; this test checks 21 — add the new one", n)
	}
}

// The fragments every operator read holds.
var operatorFragments = []string{
	"SELECT o.oid, n.nspname::pg_catalog.text, o.oprname::pg_catalog.text,\n\tpg_catalog.pg_get_userbyid(o.oprowner)::pg_catalog.text,",
	"o.oprkind::pg_catalog.text,\n\to.oprleft, COALESCE(CASE",
	"o.oprcanmerge, o.oprcanhash,",
	operatorMatch,
	"JOIN pg_catalog.pg_namespace n ON n.oid = o.oprnamespace",
	"LEFT JOIN pg_catalog.pg_type lt ON lt.oid = o.oprleft",
	"LEFT JOIN pg_catalog.pg_type rt ON rt.oid = o.oprright",
	"LEFT JOIN pg_catalog.pg_type ot ON ot.oid = o.oprresult",
	"LEFT JOIN pg_catalog.pg_proc fc ON fc.oid = o.oprcode::pg_catalog.oid",
	"LEFT JOIN pg_catalog.pg_operator oc ON oc.oid = o.oprcom",
	"LEFT JOIN pg_catalog.pg_operator og ON og.oid = o.oprnegate",
	"LEFT JOIN pg_catalog.pg_proc fr ON fr.oid = o.oprrest::pg_catalog.oid",
	"LEFT JOIN pg_catalog.pg_proc fj ON fj.oid = o.oprjoin::pg_catalog.oid",
	"d.classid = 'pg_catalog.pg_operator'::pg_catalog.regclass AND d.objid = o.oid",
	"LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = o.oid\n\tAND ds.classoid = 'pg_catalog.pg_operator'::pg_catalog.regclass AND ds.objsubid = 0",
}

// The listing fills every field, pushes its filter into the query, orders
// by schema, name and operand types, and goes to the database's own pool.
func TestOperatorsFiltered(t *testing.T) {
	yes := true
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On(operatorMatch, rowsOf(operatorTypes, rowOperator1, rowOperator2))
	d := f.Server().DatabaseRef("appdb")
	os, err := d.Operators(t.Context(), pgmo.ObjectFilter{
		Name:            []pgmo.TextCriterion{{Op: pgmo.TextEquals, Value: "="}},
		Schema:          []pgmo.TextCriterion{{Op: pgmo.TextNotEquals, Value: "app"}},
		System:          &yes,
		ExtensionMember: &yes,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(os) != 2 {
		t.Fatalf("%d operators, want 2", len(os))
	}
	checkOperators(t, os, d)
	if os[0].Signature() != `pg_catalog.= (integer, app."Mood")` || os[1].Signature() != `"My s".!! (NONE, bigint)` {
		t.Errorf("Signature() = %s, %s", os[0].Signature(), os[1].Signature())
	}
	if n := len(f.Queries()); n != 1 {
		t.Errorf("%d statements on the Server's own database, want 1: the listing went to appdb", n)
	}
	last := lastQuery(other)
	for _, w := range operatorFragments {
		if !strings.Contains(last.Text, w) {
			t.Errorf("no %q in\n%s", w, last.Text)
		}
	}
	tail := "WHERE true AND o.oprname = $1::pg_catalog.text AND n.nspname <> $2::pg_catalog.text" +
		" AND (pg_catalog.starts_with(n.nspname, 'pg_') OR n.nspname = 'information_schema') AND EXISTS (SELECT FROM pg_catalog.pg_depend d WHERE " +
		"d.classid = 'pg_catalog.pg_operator'::pg_catalog.regclass AND d.objid = o.oid"
	if !strings.Contains(last.Text, tail) || !strings.HasSuffix(last.Text, "\nORDER BY n.nspname, o.oprname, o.oprleft, o.oprright") ||
		len(last.Args) != 2 || last.Arg(0) != "=" || last.Arg(1) != "app" {
		t.Errorf("filter not pushed down, then ordered: args %v\n%s", last.Args, last.Text)
	}
}

// OperatorByName binds the names and operand types as $1–$4, fills every
// field, and tells a missing operator from a failed read; the Ref reads
// nothing.
func TestOperatorByName(t *testing.T) {
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On(operatorMatch, rowsOf(operatorTypes, rowOperator2))
	d := f.Server().DatabaseRef("appdb")
	o, err := d.OperatorByName(t.Context(), "My s", "!!", 0, 20)
	if err != nil {
		t.Fatal(err)
	}
	g := *o
	g.SetDatabase(nil)
	if o.Database() != d || !reflect.DeepEqual(g, wantOperators[1]) {
		t.Errorf("= %+v, want %+v", g, wantOperators[1])
	}
	last := lastQuery(other)
	if !strings.HasSuffix(last.Text, "WHERE true\n\tAND n.nspname = $1::pg_catalog.text AND o.oprname = $2::pg_catalog.text"+
		"\n\tAND o.oprleft = $3::pg_catalog.oid AND o.oprright = $4::pg_catalog.oid") ||
		last.Arg(0) != "My s" || last.Arg(1) != "!!" || last.Arg(2) != pgmo.OID(0) || last.Arg(3) != pgmo.OID(20) {
		t.Errorf("identity not bound as $1–$4: args %v\n%s", last.Args, last.Text)
	}
	if o.QualifiedName() != `"My s".!!` {
		t.Errorf("QualifiedName() = %s", o.QualifiedName())
	}

	n := len(other.Queries())
	if _, err := d.OperatorByName(t.Context(), "", "=", 23, 23); !errors.Is(err, pgmo.ErrSchemaRequired) {
		t.Errorf("no schema: err = %v, want ErrSchemaRequired", err)
	}
	if len(other.Queries()) != n {
		t.Errorf("a refused read was sent")
	}

	f = pgmotest.New(t)
	f.On(operatorMatch, rowsOf(operatorTypes))
	d = f.Server().DatabaseRef(pgmotest.DefaultDatabase)
	if _, err := d.OperatorByName(t.Context(), "app", "===", 23, 25); !errors.Is(err, pgmo.ErrNotFound) ||
		!strings.Contains(err.Error(), `operator app.=== (23, 25) not found in database "`+pgmotest.DefaultDatabase+`"`) {
		t.Errorf("missing: err = %v, want ErrNotFound naming it", err)
	}
	f = pgmotest.New(t)
	f.On(operatorMatch, pgmotest.Fail(&pgconn.PgError{Code: "57014", Message: "canceled"}))
	d = f.Server().DatabaseRef(pgmotest.DefaultDatabase)
	_, err = d.OperatorByName(t.Context(), "app", "===", 23, 25)
	if err == nil || errors.Is(err, pgmo.ErrNotFound) || !strings.HasPrefix(err.Error(), "pgmo: read operator app.=== (23, 25) of database ") ||
		strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("server error: err = %v, want it wrapped once naming the read", err)
	}
	_, err = d.Operators(t.Context(), pgmo.ObjectFilter{})
	if err == nil || !strings.HasPrefix(err.Error(), `pgmo: list operators of database "`) || strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("listing, server error: err = %v, want it wrapped once naming the read", err)
	}

	n = len(f.Queries())
	r := d.OperatorRef("app", "===", "integer", "app.mood")
	g = *r
	g.SetDatabase(nil)
	if r.Database() != d || !reflect.DeepEqual(g, pgmo.Operator{Schema: "app", Name: "===", Left: "integer", Right: "app.mood"}) ||
		r.Signature() != "app.=== (integer, app.mood)" {
		t.Errorf("OperatorRef = %+v", r)
	}
	if d.OperatorRef("app", "!!", "", "bigint").Signature() != "app.!! (NONE, bigint)" {
		t.Errorf("a prefix OperatorRef's Signature does not write NONE")
	}
	if len(f.Queries()) != n {
		t.Errorf("OperatorRef sent a statement")
	}
}
