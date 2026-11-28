package pgmo_test

import (
	"context"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/radix29/pgmo"
	"github.com/radix29/pgmo/pgmotest"
)

// The column types of a search, in SELECT order.
var searchTypes = []uint32{pgtype.TextOID, pgtype.OIDOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID,
	pgtype.TextOID, pgtype.TextOID, pgtype.BoolOID, pgtype.TextOID}

// searchMatch tells either search apart in a fake; definitionMatch the
// definition search alone.
const (
	searchMatch     = ") AS o(cls, catalog, oid, type, schema, name, tbl, member)"
	definitionMatch = "AND pg_catalog.strpos(CASE"
)

// Every column lands in its field; the filter is pushed into the query
// with its text bound, never written into it, folded as every listing's
// is; the read goes to the database's own pool, ordered by kind and name.
func TestSearchFillsEveryField(t *testing.T) {
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On(searchMatch, rowsOf(searchTypes,
		[]any{"pg_trigger", uint32(40), "trigger", "app", "Tg", "T t", `"Tg" on app."T t"`, false, "ext"},
		[]any{"pg_proc", uint32(41), "function", "pg_catalog", "f", "", "pg_catalog.f(integer)", true, ""}))
	got, err := f.Server().DatabaseRef("appdb").Search(t.Context(), pgmo.ObjectFilter{
		Name: []pgmo.TextCriterion{{Op: pgmo.TextContains, Value: "TG"}}, System: new(false)})
	if err != nil {
		t.Fatal(err)
	}
	want := []pgmo.SearchResult{
		{Origin: pgmo.Origin{Extension: "ext"}, Catalog: "pg_trigger", OID: 40, Type: "trigger", Schema: "app", Name: "Tg",
			Table: "T t", Identity: `"Tg" on app."T t"`},
		{Origin: pgmo.Origin{IsSystem: true}, Catalog: "pg_proc", OID: 41, Type: "function", Schema: "pg_catalog", Name: "f",
			Identity: "pg_catalog.f(integer)"},
	}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("Search = %+v\nwant %+v", got, want)
	}
	if n := reflect.TypeFor[pgmo.SearchResult]().NumField(); n != 8 {
		t.Errorf("SearchResult has %d fields; this test checks 8 — add the new one", n)
	}
	last := lastQuery(other)
	for _, w := range []string{"pg_catalog.strpos(o.name, $1)", `ORDER BY o.type COLLATE pg_catalog."C", o.schema COLLATE pg_catalog."C", o.name COLLATE pg_catalog."C", o.tbl COLLATE pg_catalog."C", i.identity COLLATE pg_catalog."C"`,
		"WHERE NOT tg.tgisinternal", "(te.oid IS NULL OR te.typarray <> t.oid)",
		// A trigger is an extension's when its table is.
		"d.classid = CASE o.catalog WHEN 'pg_trigger' THEN 'pg_catalog.pg_class'::pg_catalog.regclass::pg_catalog.oid ELSE o.cls END",
		"AND d.objid = o.member"} {
		if !strings.Contains(last.Text, w) {
			t.Errorf("no %q in\n%s", w, last.Text)
		}
	}
	if strings.Contains(last.Text, definitionMatch) {
		t.Errorf("a name search reads definitions:\n%s", last.Text)
	}
	if last.Arg(0) != "tg" || len(last.Args) != 1 {
		t.Errorf("args %v, want the folded name alone", last.Args)
	}
	for _, q := range f.Queries() {
		if strings.Contains(q.Text, searchMatch) {
			t.Errorf("read through the Server's own database: %s", q.Text)
		}
	}
}

// The definition search binds its text as $1, unfolded, and the filter's
// after it; an empty text is refused before anything is sent; a server
// error is wrapped once.
func TestSearchDefinitions(t *testing.T) {
	f := pgmotest.New(t)
	f.On(searchMatch, rowsOf(searchTypes, []any{"pg_class", uint32(50), "view", "app", "v", "", "app.v", false, ""}))
	d := f.Server().DatabaseRef(pgmotest.DefaultDatabase)
	got, err := d.SearchDefinitions(t.Context(), "Foo%_", pgmo.ObjectFilter{Schema: []pgmo.TextCriterion{{Op: pgmo.TextEquals, Value: "APP"}}})
	if err != nil || len(got) != 1 || got[0].Identity != "app.v" {
		t.Fatalf("SearchDefinitions = %+v, %v", got, err)
	}
	last := lastQuery(f)
	if last.Arg(0) != "Foo%_" || last.Arg(1) != "app" || len(last.Args) != 2 {
		t.Errorf("args %v, want the text as given, then the folded schema", last.Args)
	}
	for _, w := range []string{definitionMatch, "pg_catalog.strpos(" + "CASE", "$1::pg_catalog.text) > 0", "o.schema = $2",
		"AND o.type IN ('view', 'materialized view', 'function', 'procedure')",
		"WHEN p.prosqlbody IS NOT NULL THEN pg_catalog.pg_get_function_sqlbody(p.oid)",
		"WHEN l.lanname NOT IN ('c', 'internal') THEN p.prosrc"} {
		if !strings.Contains(last.Text, w) {
			t.Errorf("no %q in\n%s", w, last.Text)
		}
	}

	runRefusals(t, f, []refusalCase{
		{"empty text", func(ctx context.Context) error {
			_, err := d.SearchDefinitions(ctx, "", pgmo.ObjectFilter{})
			return err
		}, pgmo.ErrInvalidRequest},
		{"unknown filter op", func(ctx context.Context) error {
			_, err := d.Search(ctx, pgmo.ObjectFilter{Name: []pgmo.TextCriterion{{Op: 99, Value: "x"}}})
			return err
		}, pgmo.ErrInvalidRequest},
	})

	f = pgmotest.New(t)
	f.On(searchMatch, pgmotest.Fail(&pgconn.PgError{Code: "57014", Message: "canceled"}))
	_, err = f.Server().DatabaseRef(pgmotest.DefaultDatabase).Search(t.Context(), pgmo.ObjectFilter{})
	if err == nil || !strings.HasPrefix(err.Error(), "pgmo: search objects of database ") || strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("server error: err = %v, want it wrapped once naming the read", err)
	}
}
