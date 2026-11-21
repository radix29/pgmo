package pgmo

import (
	"slices"
	"testing"
)

// ============================================================
// pg_catalog is the source of truth
// ============================================================

// TestCatalogReadsUsePgCatalog holds pgmo's reads to pg_catalog (P3.2).
// information_schema is the SQL standard's portable view, and on
// PostgreSQL it loses what a management library needs. Each of these was
// checked on pg15 and pg18:
//
//   - It shows only what the role may use. A table in a schema the role has
//     USAGE on, with no privilege on the table, is in pg_class and absent
//     from information_schema.tables and .columns. An object it cannot see
//     cannot be identified, listed or dropped.
//   - It covers the standard's objects only. Materialized views are not in
//     information_schema.tables; indexes, extensions, publications and
//     a relation's owner are nowhere in it.
//   - It has no OIDs and keys on names the standard assumes unique.
//     (constraint_schema, constraint_name) is not unique in PostgreSQL: two
//     tables' CHECK constraints named alike come back as one constraint
//     over both tables' columns. Overloaded functions are told apart only
//     by specific_name, name_oid.
//   - It renders types the standard's way (ARRAY, USER-DEFINED,
//     character varying), not as format_type does.
//
// It does not truncate names: sql_identifier is a domain over name, so it
// has the catalog's 63-byte limit.
//
// A view in portableViews is allowed, with the reason nothing is lost.
func TestCatalogReadsUsePgCatalog(t *testing.T) {
	for _, lit := range sqlLiterals(t) {
		for _, v := range informationSchemaReads(lit.sql, portableViews) {
			t.Errorf("%s: reads information_schema.%s — read pg_catalog instead, or add it to portableViews with why nothing is lost", lit.pos, v)
		}
	}
}

// portableViews are the information_schema views and functions pgmo may
// use, each with why the pg_catalog read would add nothing. None so far:
// every read pgmo makes needs what the view loses.
var portableViews = map[string]string{}

// The scan itself: what it must find and what it must leave alone.
func TestInformationSchemaScan(t *testing.T) {
	cases := []struct {
		sql  string
		want []string
	}{
		{`SELECT table_name FROM information_schema.tables WHERE table_schema = $1`, []string{"tables"}},
		{`SELECT 1 FROM pg_catalog.pg_class c JOIN INFORMATION_SCHEMA.Columns k ON true`, []string{"columns"}},
		{`SELECT 1 FROM "information_schema"."key_column_usage", information_schema . views`, []string{"key_column_usage", "views"}},
		{`SELECT (information_schema._pg_expandarray(ARRAY[1])).n`, []string{"_pg_expandarray"}},
		{`SELECT x::information_schema.sql_identifier`, []string{"sql_identifier"}},
		{`SELECT 1 FROM information_schema.%s WHERE true`, []string{"%s"}}, // built with Sprintf
		// Fine: the schema's name as a value, in a comment, another schema
		// spelled alike but quoted differently, and pg_catalog reads.
		{`SELECT 1 FROM pg_catalog.pg_namespace n WHERE n.nspname IN ('information_schema', 'pg_catalog')`, nil},
		{`SELECT 1 -- FROM information_schema.tables
		  /* information_schema.columns */ FROM pg_catalog.pg_class`, nil},
		{`SELECT 1 FROM "Information_Schema".tables`, nil},
		{`SELECT 'information_schema.tables'::pg_catalog.regclass`, nil},
		{`SELECT 1 AS information_schema, oid FROM pg_catalog.pg_class`, nil},
		{versionQuery, nil},
	}
	for _, c := range cases {
		if got := informationSchemaReads(c.sql, nil); !slices.Equal(got, c.want) {
			t.Errorf("informationSchemaReads(%q)\n got  %q\n want %q", c.sql, got, c.want)
		}
	}
	allowed := map[string]string{"character_sets": "a reason"}
	sql := `SELECT 1 FROM information_schema.character_sets, information_schema.tables`
	if got, want := informationSchemaReads(sql, allowed), []string{"tables"}; !slices.Equal(got, want) {
		t.Errorf("informationSchemaReads(%q, allowing character_sets) = %q, want %q", sql, got, want)
	}
	for v, why := range portableViews {
		if why == "" {
			t.Errorf("portableViews[%q] has no reason", v)
		}
	}
}

// informationSchemaReads lists, in text order, every name sql reads from
// information_schema (a view, a function or a type) that allowed does not
// hold, folded as the server folds it; a name built with Sprintf is its fmt
// verb. A literal or a comment naming the schema is not a read.
func informationSchemaReads(sql string, allowed map[string]string) []string {
	toks := sqlTokens(sql)
	var out []string
	for i := 0; i+2 < len(toks); i++ {
		if toks[i].text != "information_schema" || !toks[i+1].is(".") {
			continue
		}
		if name := toks[i+2].text; allowed[name] == "" {
			out = append(out, name)
		}
	}
	return out
}
