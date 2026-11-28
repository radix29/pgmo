package pgmo

import (
	"strings"
	"testing"
)

// The fold is spliced into every place a query marks, three in each
// direction — the parts, the climb and the root test — and nowhere is a
// marker left for the server to read as a comment, which would fold
// nothing and still run.
func TestDependencyQueriesFoldEverywhere(t *testing.T) {
	fold, join := dependencyFold("f"), dependencyFoldJoin("f")
	for name, q := range map[string]string{"dependencies": dependenciesQuery, "dependents": dependentsQuery} {
		if strings.Contains(q, "/*") {
			t.Errorf("%s: a marker is left:\n%s", name, q)
		}
		if n := strings.Count(q, fold); n != 3 {
			t.Errorf("%s: the fold condition %d times, want 3", name, n)
		}
		if n := strings.Count(q, join); n != 3 {
			t.Errorf("%s: the fold join %d times, want 3", name, n)
		}
	}
	for _, w := range []string{
		"(f.refclassid, f.refobjid) <> (f.classid, f.objid)",
		"f.deptype = 'i' AND fr.relkind IS DISTINCT FROM 'S'",
		"f.deptype = 'a' AND (fr.relkind IN ('i', 'I') OR f.classid IN (",
		"'pg_catalog.pg_attrdef'::pg_catalog.regclass, 'pg_catalog.pg_constraint'::pg_catalog.regclass,",
		"'pg_catalog.pg_rewrite'::pg_catalog.regclass, 'pg_catalog.pg_trigger'::pg_catalog.regclass,",
		"'pg_catalog.pg_policy'::pg_catalog.regclass",
	} {
		if !strings.Contains(fold, w) {
			t.Errorf("no %q in the fold:\n%s", w, fold)
		}
	}
	if !strings.Contains(join, "LEFT JOIN pg_catalog.pg_class fr ON f.classid = 'pg_catalog.pg_class'::pg_catalog.regclass AND fr.oid = f.objid") {
		t.Errorf("fold join:\n%s", join)
	}
}
