package pgmo

import "testing"

func TestIsSystemSchema(t *testing.T) {
	for name, want := range map[string]bool{
		"pg_catalog":          true,
		"information_schema":  true,
		"pg_toast":            true,
		"pg_temp_3":           true,
		"pg_toast_temp_3":     true,
		"public":              false,
		"cron":                false, // pg_cron's schema: an extension member, not a system schema
		"PG_CATALOG":          false, // names match exactly; the server would accept this one
		"Information_Schema":  false,
		"pg":                  false,
		"xpg_catalog":         false,
		"information_schema2": false,
		"":                    false,
	} {
		if got := IsSystemSchema(name); got != want {
			t.Errorf("IsSystemSchema(%q) = %v, want %v", name, got, want)
		}
	}
}

func TestOriginIsExtensionMember(t *testing.T) {
	if (Origin{IsSystem: true}).IsExtensionMember() {
		t.Error("a system object with no extension is a member")
	}
	if !(Origin{Extension: "pg_cron"}).IsExtensionMember() {
		t.Error("an object of pg_cron is not a member")
	}
}

// The membership test is pinned in full: a dropped objsubid, deptype or
// refclassid term still runs and returns rows — 'x' dependents, a column's
// row, a dependency on something other than an extension.
func TestOriginSQL(t *testing.T) {
	const cond = `d.classid = 'pg_catalog.pg_class'::pg_catalog.regclass AND d.objid = c.oid AND d.objsubid = 0
			AND d.refclassid = 'pg_catalog.pg_extension'::pg_catalog.regclass AND d.deptype = 'e'`
	if got := extensionMembership(catalogClass, "c.oid"); got != cond {
		t.Errorf("extensionMembership\n got: %s\nwant: %s", got, cond)
	}
	wantOf := `COALESCE((SELECT e.extname::pg_catalog.text FROM pg_catalog.pg_depend d
		JOIN pg_catalog.pg_extension e ON e.oid = d.refobjid
		WHERE ` + cond + `), '')`
	if got := extensionOf(catalogClass, "c.oid"); got != wantOf {
		t.Errorf("extensionOf\n got: %s\nwant: %s", got, wantOf)
	}
	wantMember := `EXISTS (SELECT FROM pg_catalog.pg_depend d WHERE ` + cond + `)`
	if got := isExtensionMember(catalogClass, "c.oid"); got != wantMember {
		t.Errorf("isExtensionMember\n got: %s\nwant: %s", got, wantMember)
	}
	wantCols := systemSchema("n.nspname") + ", " + wantOf
	if got := originColumns("n.nspname", catalogClass, "c.oid"); got != wantCols {
		t.Errorf("originColumns\n got: %s\nwant: %s", got, wantCols)
	}
	var o Origin
	if dst := scanOrigin(&o); len(dst) != 2 || dst[0] != &o.IsSystem || dst[1] != &o.Extension {
		t.Errorf("scanOrigin = %v, want IsSystem then Extension", dst)
	}
}
