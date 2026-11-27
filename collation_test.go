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

// The column types of the collation read, in SELECT order, as the server
// sends them.
var collationTypes = []uint32{pgtype.OIDOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.BoolOID, pgtype.TextOID,
	pgtype.TextOID, pgtype.BoolOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID,
	pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID}

// collationMatch tells the collation read apart in a fake.
const collationMatch = "FROM pg_catalog.pg_collation co"

// Two collations with every column set to something its neighbours are not.
var (
	rowCollation1 = []any{uint32(50001), "app", "De ci", "own", true, "citext",
		"i", false, "", "", "",
		"de-u-ks-level2", "&a < b", "153.121", "case-insensitive"}
	rowCollation2 = []any{uint32(50002), "Other", "c8", "B b", false, "",
		"c", true, "UTF8", "C.UTF-8", "C.utf8",
		"", "", "", ""}
)

// The rows above as Collations, without their Database.
var wantCollations = []pgmo.Collation{
	{Origin: pgmo.Origin{IsSystem: true, Extension: "citext"}, OID: 50001, Schema: "app", Name: "De ci", Owner: "own",
		Provider: pgmo.LocaleProviderICU, Locale: "de-u-ks-level2", ICURules: "&a < b", Version: "153.121",
		Comment: "case-insensitive"},
	{OID: 50002, Schema: "Other", Name: "c8", Owner: "B b", Provider: pgmo.LocaleProviderLibc, Deterministic: true,
		Encoding: "UTF8", Collate: "C.UTF-8", CType: "C.utf8"},
}

// checkCollations compares got with the first len(got) of wantCollations,
// each read from d.
func checkCollations(t *testing.T, got []*pgmo.Collation, d *pgmo.Database) {
	t.Helper()
	for i, c := range got {
		if c.Database() != d {
			t.Errorf("[%d].Database() = %v, want %v", i, c.Database(), d)
		}
		g := *c
		g.SetDatabase(nil)
		if !reflect.DeepEqual(g, wantCollations[i]) {
			t.Errorf("[%d] =\n%+v\nwant\n%+v", i, g, wantCollations[i])
		}
	}
	if n := reflect.TypeFor[pgmo.Collation]().NumField(); n != 15 {
		t.Errorf("Collation has %d fields; this test checks 15 — add the new one", n)
	}
}

// The fragments every collation read holds (the gated columns are the
// golden file's, testdata/version_gates/collation.sql).
var collationFragments = []string{
	"SELECT co.oid, n.nspname::pg_catalog.text, co.collname::pg_catalog.text,\n\tpg_catalog.pg_get_userbyid(co.collowner)::pg_catalog.text,",
	"co.collprovider::pg_catalog.text, co.collisdeterministic,\n\tpg_catalog.pg_encoding_to_char(co.collencoding)::pg_catalog.text,\n\tCOALESCE(co.collcollate, ''), COALESCE(co.collctype, ''),",
	"COALESCE(co.collversion, ''),",
	collationMatch,
	"JOIN pg_catalog.pg_namespace n ON n.oid = co.collnamespace",
	"d.classid = 'pg_catalog.pg_collation'::pg_catalog.regclass AND d.objid = co.oid",
	"LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = co.oid\n\tAND ds.classoid = 'pg_catalog.pg_collation'::pg_catalog.regclass AND ds.objsubid = 0",
}

// The listing fills every field, pushes its filter into the query, orders
// by schema, name and encoding, and goes to the database's own pool.
func TestCollationsFiltered(t *testing.T) {
	yes := true
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On(collationMatch, rowsOf(collationTypes, rowCollation1, rowCollation2))
	d := f.Server().DatabaseRef("appdb")
	cs, err := d.Collations(t.Context(), pgmo.ObjectFilter{
		Name:            []pgmo.TextCriterion{{Op: pgmo.TextNotContains, Value: "X"}},
		Schema:          []pgmo.TextCriterion{{Op: pgmo.TextEquals, Value: "app"}},
		System:          &yes,
		ExtensionMember: &yes,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(cs) != 2 {
		t.Fatalf("%d collations, want 2", len(cs))
	}
	checkCollations(t, cs, d)
	if n := len(f.Queries()); n != 1 {
		t.Errorf("%d statements on the Server's own database, want 1: the listing went to appdb", n)
	}
	last := lastQuery(other)
	for _, w := range collationFragments {
		if !strings.Contains(last.Text, w) {
			t.Errorf("no %q in\n%s", w, last.Text)
		}
	}
	tail := "WHERE true AND pg_catalog.strpos(co.collname, $1) = 0 AND n.nspname = $2::pg_catalog.text" +
		" AND (pg_catalog.starts_with(n.nspname, 'pg_') OR n.nspname = 'information_schema') AND EXISTS (SELECT FROM pg_catalog.pg_depend d WHERE " +
		"d.classid = 'pg_catalog.pg_collation'::pg_catalog.regclass AND d.objid = co.oid"
	if !strings.Contains(last.Text, tail) || !strings.HasSuffix(last.Text, "\nORDER BY n.nspname, co.collname, co.collencoding") ||
		len(last.Args) != 2 || last.Arg(0) != "x" || last.Arg(1) != "app" {
		t.Errorf("filter not pushed down, then ordered: args %v\n%s", last.Args, last.Text)
	}
}

// CollationByName binds the names as $1 and $2, keeps to the collations the
// database can use, the encoding's own first, fills every field, and tells
// a missing collation from a failed read; the Ref reads nothing.
func TestCollationByName(t *testing.T) {
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On(collationMatch, rowsOf(collationTypes, rowCollation1))
	d := f.Server().DatabaseRef("appdb")
	c, err := d.CollationByName(t.Context(), "app", "De ci")
	if err != nil {
		t.Fatal(err)
	}
	checkCollations(t, []*pgmo.Collation{c}, d)
	last := lastQuery(other)
	if !strings.HasSuffix(last.Text, "WHERE true\n\tAND n.nspname = $1::pg_catalog.text AND co.collname = $2::pg_catalog.text"+
		"\n\tAND co.collencoding IN (-1, pg_catalog.pg_char_to_encoding(pg_catalog.getdatabaseencoding()))\nORDER BY co.collencoding DESC") ||
		last.Arg(0) != "app" || last.Arg(1) != "De ci" {
		t.Errorf("names not bound, or encoding not narrowed: args %v\n%s", last.Args, last.Text)
	}
	if c.QualifiedName() != `app."De ci"` {
		t.Errorf("QualifiedName() = %s", c.QualifiedName())
	}

	n := len(other.Queries())
	if _, err := d.CollationByName(t.Context(), "", "c"); !errors.Is(err, pgmo.ErrSchemaRequired) {
		t.Errorf("no schema: err = %v, want ErrSchemaRequired", err)
	}
	if len(other.Queries()) != n {
		t.Errorf("a refused read was sent")
	}

	f = pgmotest.New(t)
	f.On(collationMatch, rowsOf(collationTypes))
	d = f.Server().DatabaseRef(pgmotest.DefaultDatabase)
	if _, err := d.CollationByName(t.Context(), "app", "Gone"); !errors.Is(err, pgmo.ErrNotFound) ||
		!strings.Contains(err.Error(), `collation app."Gone" not found in database "`+pgmotest.DefaultDatabase+`"`) {
		t.Errorf("missing: err = %v, want ErrNotFound naming it", err)
	}
	f = pgmotest.New(t)
	f.On(collationMatch, pgmotest.Fail(&pgconn.PgError{Code: "57014", Message: "canceled"}))
	d = f.Server().DatabaseRef(pgmotest.DefaultDatabase)
	_, err = d.CollationByName(t.Context(), "app", "c")
	if err == nil || errors.Is(err, pgmo.ErrNotFound) || !strings.HasPrefix(err.Error(), "pgmo: read collation app.c of database ") ||
		strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("server error: err = %v, want it wrapped once naming the read", err)
	}
	_, err = d.Collations(t.Context(), pgmo.ObjectFilter{})
	if err == nil || !strings.HasPrefix(err.Error(), `pgmo: list collations of database "`) || strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("listing, server error: err = %v, want it wrapped once naming the read", err)
	}

	n = len(f.Queries())
	r := d.CollationRef("app", "c")
	g := *r
	g.SetDatabase(nil)
	if r.Database() != d || !reflect.DeepEqual(g, pgmo.Collation{Schema: "app", Name: "c"}) {
		t.Errorf("CollationRef = %+v", r)
	}
	if len(f.Queries()) != n {
		t.Errorf("CollationRef sent a statement")
	}
}
