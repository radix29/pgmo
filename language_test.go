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

// The column types of the language read, in SELECT order, as the server
// sends them.
var languageTypes = append([]uint32{pgtype.OIDOID, pgtype.TextOID, pgtype.TextOID, pgtype.BoolOID, pgtype.TextOID,
	pgtype.BoolOID, pgtype.BoolOID, pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.TextArrayOID,
	pgtype.BoolOID}, append(aclTypes, pgtype.TextOID)...)

// languageMatch tells the language read apart in a fake.
const languageMatch = "FROM pg_catalog.pg_language l"

// Two languages with every column set to something its neighbours are not.
var (
	rowLanguage1 = []any{uint32(13609), "plpgsql", "postgres", true, "plpgsql",
		true, true, []string{"pg_catalog", "plpgsql_call_handler"}, []string{"pg_catalog", "plpgsql_inline_handler"},
		[]string{"app", "check"}, true,
		[]string{"", "postgres"}, []string{"postgres", "postgres"}, []string{"USAGE", "USAGE"}, []bool{false, false},
		"PL/pgSQL"}
	rowLanguage2 = []any{uint32(70002), "My PL", "own", false, "",
		false, false, []string{}, []string{}, []string{}, false,
		[]string{"own"}, []string{"own"}, []string{"USAGE"}, []bool{true}, ""}
)

// The rows above as Languages, without their Database.
var wantLanguages = []pgmo.Language{
	{Origin: pgmo.Origin{IsSystem: true, Extension: "plpgsql"}, OID: 13609, Name: "plpgsql", Owner: "postgres",
		Procedural: true, Trusted: true,
		Handler:   pgmo.ObjectName{Schema: "pg_catalog", Name: "plpgsql_call_handler"},
		Inline:    pgmo.ObjectName{Schema: "pg_catalog", Name: "plpgsql_inline_handler"},
		Validator: pgmo.ObjectName{Schema: "app", Name: "check"},
		Privileges: []pgmo.PrivilegeGrant{{Grantee: "", Grantor: "postgres", Privilege: pgmo.PrivilegeUsage},
			{Grantee: "postgres", Grantor: "postgres", Privilege: pgmo.PrivilegeUsage}},
		PrivilegesDefault: true, Comment: "PL/pgSQL"},
	{OID: 70002, Name: "My PL", Owner: "own",
		Privileges: []pgmo.PrivilegeGrant{{Grantee: "own", Grantor: "own", Privilege: pgmo.PrivilegeUsage, Grantable: true}}},
}

// checkLanguages compares got with the first len(got) of wantLanguages, each
// read from d.
func checkLanguages(t *testing.T, got []*pgmo.Language, d *pgmo.Database) {
	t.Helper()
	for i, l := range got {
		if l.Database() != d {
			t.Errorf("[%d].Database() = %v, want %v", i, l.Database(), d)
		}
		g := *l
		g.SetDatabase(nil)
		if !reflect.DeepEqual(g, wantLanguages[i]) {
			t.Errorf("[%d] =\n%+v\nwant\n%+v", i, g, wantLanguages[i])
		}
	}
	if n := reflect.TypeFor[pgmo.Language]().NumField(); n != 13 {
		t.Errorf("Language has %d fields; this test checks 13 — add the new one", n)
	}
}

// The fragments every language read holds.
var languageFragments = []string{
	"SELECT l.oid, l.lanname::pg_catalog.text,\n\tpg_catalog.pg_get_userbyid(l.lanowner)::pg_catalog.text,\n\tl.oid < 16384, COALESCE(ext.extname::pg_catalog.text, ''),\n\tl.lanispl, l.lanpltrusted,",
	"l.lanacl IS NULL,",
	languageMatch,
	"LEFT JOIN pg_catalog.pg_proc fh ON fh.oid = l.lanplcallfoid::pg_catalog.oid",
	"LEFT JOIN pg_catalog.pg_proc fi ON fi.oid = l.laninline::pg_catalog.oid",
	"LEFT JOIN pg_catalog.pg_proc fv ON fv.oid = l.lanvalidator::pg_catalog.oid",
	"d.classid = 'pg_catalog.pg_language'::pg_catalog.regclass AND d.objid = l.oid",
	"LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = l.oid\n\tAND ds.classoid = 'pg_catalog.pg_language'::pg_catalog.regclass AND ds.objsubid = 0",
	"COALESCE(l.lanacl, pg_catalog.acldefault('l', l.lanowner))",
}

// The listing fills every field, pushes its filter into the query — name,
// system by OID, extension membership; a schema criterion is no column —
// orders by name, and goes to the database's own pool.
func TestLanguagesFiltered(t *testing.T) {
	no := false
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On(languageMatch, rowsOf(languageTypes, rowLanguage1, rowLanguage2))
	d := f.Server().DatabaseRef("appdb")
	ls, err := d.Languages(t.Context(), pgmo.ObjectFilter{
		Name:            []pgmo.TextCriterion{{Op: pgmo.TextEquals, Value: "PLPGSQL"}},
		Schema:          []pgmo.TextCriterion{{Op: pgmo.TextEquals, Value: "app"}},
		System:          &no,
		ExtensionMember: &no,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ls) != 2 {
		t.Fatalf("%d languages, want 2", len(ls))
	}
	checkLanguages(t, ls, d)
	if n := len(f.Queries()); n != 1 {
		t.Errorf("%d statements on the Server's own database, want 1: the listing went to appdb", n)
	}
	last := lastQuery(other)
	for _, w := range languageFragments {
		if !strings.Contains(last.Text, w) {
			t.Errorf("no %q in\n%s", w, last.Text)
		}
	}
	tail := "WHERE true AND l.lanname = $1::pg_catalog.text AND NOT l.oid < 16384 AND NOT EXISTS (SELECT FROM pg_catalog.pg_depend d WHERE " +
		"d.classid = 'pg_catalog.pg_language'::pg_catalog.regclass AND d.objid = l.oid"
	if !strings.Contains(last.Text, tail) || !strings.HasSuffix(last.Text, "\nORDER BY l.lanname") ||
		len(last.Args) != 1 || last.Arg(0) != "plpgsql" || strings.Contains(last.Text, "nspname = $") {
		t.Errorf("filter not pushed down, then ordered: args %v\n%s", last.Args, last.Text)
	}
}

// LanguageByName binds the name as $1, fills every field, and tells a
// missing language from a failed read; the Ref reads nothing.
func TestLanguageByName(t *testing.T) {
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On(languageMatch, rowsOf(languageTypes, rowLanguage2))
	d := f.Server().DatabaseRef("appdb")
	l, err := d.LanguageByName(t.Context(), "My PL")
	if err != nil {
		t.Fatal(err)
	}
	g := *l
	g.SetDatabase(nil)
	if l.Database() != d || !reflect.DeepEqual(g, wantLanguages[1]) {
		t.Errorf("= %+v, want %+v", g, wantLanguages[1])
	}
	last := lastQuery(other)
	if !strings.HasSuffix(last.Text, "WHERE true\n\tAND l.lanname = $1::pg_catalog.text") || last.Arg(0) != "My PL" {
		t.Errorf("name not bound as $1: args %v\n%s", last.Args, last.Text)
	}
	if l.QualifiedName() != `"My PL"` {
		t.Errorf("QualifiedName() = %s", l.QualifiedName())
	}

	f = pgmotest.New(t)
	f.On(languageMatch, rowsOf(languageTypes))
	d = f.Server().DatabaseRef(pgmotest.DefaultDatabase)
	if _, err := d.LanguageByName(t.Context(), "Gone"); !errors.Is(err, pgmo.ErrNotFound) ||
		!strings.Contains(err.Error(), `language "Gone" not found in database "`+pgmotest.DefaultDatabase+`"`) {
		t.Errorf("missing: err = %v, want ErrNotFound naming it", err)
	}
	f = pgmotest.New(t)
	f.On(languageMatch, pgmotest.Fail(&pgconn.PgError{Code: "57014", Message: "canceled"}))
	d = f.Server().DatabaseRef(pgmotest.DefaultDatabase)
	_, err = d.LanguageByName(t.Context(), "pl")
	if err == nil || errors.Is(err, pgmo.ErrNotFound) || !strings.HasPrefix(err.Error(), "pgmo: read language pl of database ") ||
		strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("server error: err = %v, want it wrapped once naming the read", err)
	}
	_, err = d.Languages(t.Context(), pgmo.ObjectFilter{})
	if err == nil || !strings.HasPrefix(err.Error(), `pgmo: list languages of database "`) || strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("listing, server error: err = %v, want it wrapped once naming the read", err)
	}

	n := len(f.Queries())
	r := d.LanguageRef("pl")
	g = *r
	g.SetDatabase(nil)
	if r.Database() != d || !reflect.DeepEqual(g, pgmo.Language{Name: "pl"}) {
		t.Errorf("LanguageRef = %+v", r)
	}
	if len(f.Queries()) != n {
		t.Errorf("LanguageRef sent a statement")
	}
}
