package pgmo_test

import (
	"errors"
	"reflect"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/radix29/pgmo"
	"github.com/radix29/pgmo/pgmotest"
)

// The column types of the extension read, in SELECT order, as the server
// sends them.
var extensionTypes = []uint32{pgtype.OIDOID, pgtype.TextOID, pgtype.TextOID, pgtype.BoolOID, pgtype.TextOID,
	pgtype.TextOID, pgtype.TextOID, pgtype.BoolOID, pgtype.TextOID, pgtype.TextArrayOID,
	pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.TextOID}

// extensionMatch tells the extension read apart in a fake.
const extensionMatch = "FROM pg_catalog.pg_extension x\n"

// Two extensions with every column set to something its neighbours are not;
// the second has a NULL requirement and configuration arrays, as the LEFT
// JOINs give for none.
var (
	rowExtension1 = []any{uint32(13605), "plpgsql", "postgres", true, "",
		"pg_catalog", "1.0", false, "1.1", []string{"cube", "Other"},
		[]string{"cron", "Sch"}, []string{"job", "job seq"}, []string{"WHERE x > 0", ""}, "PL/pgSQL"}
	rowExtension2 = []any{uint32(70002), "E x", "B b", false, "",
		"E s", "1.5-1", true, "", nil,
		nil, nil, nil, ""}
)

// The rows above as Extensions, without their Database.
var wantExtensions = []pgmo.Extension{
	{Origin: pgmo.Origin{IsSystem: true}, OID: 13605, Name: "plpgsql", Owner: "postgres", Schema: "pg_catalog",
		Version: "1.0", DefaultVersion: "1.1", Requires: []string{"cube", "Other"},
		ConfigTables: []pgmo.ExtensionConfigTable{
			{Table: pgmo.ObjectName{Schema: "cron", Name: "job"}, Condition: "WHERE x > 0"},
			{Table: pgmo.ObjectName{Schema: "Sch", Name: "job seq"}},
		}, Comment: "PL/pgSQL"},
	{OID: 70002, Name: "E x", Owner: "B b", Schema: "E s", Version: "1.5-1", Relocatable: true},
}

// checkExtensions compares got with the first len(got) of wantExtensions,
// each read from d.
func checkExtensions(t *testing.T, got []*pgmo.Extension, d *pgmo.Database) {
	t.Helper()
	for i, e := range got {
		if e.Database() != d {
			t.Errorf("[%d].Database() = %v, want %v", i, e.Database(), d)
		}
		g := *e
		g.SetDatabase(nil)
		if !reflect.DeepEqual(g, wantExtensions[i]) {
			t.Errorf("[%d] =\n%+v\nwant\n%+v", i, g, wantExtensions[i])
		}
	}
	if n := reflect.TypeFor[pgmo.Extension]().NumField(); n != 12 {
		t.Errorf("Extension has %d fields; this test checks 12 — add the new one", n)
	}
}

// The fragments every extension read holds.
var extensionFragments = []string{
	"SELECT x.oid, x.extname::pg_catalog.text,\n\tpg_catalog.pg_get_userbyid(x.extowner)::pg_catalog.text,\n\tx.oid < 16384, '',",
	"n.nspname::pg_catalog.text, x.extversion, x.extrelocatable,\n\tCOALESCE(a.default_version, ''), req.names,\n\tcfg.schemas, cfg.names, cfg.conditions,",
	extensionMatch,
	"JOIN pg_catalog.pg_namespace n ON n.oid = x.extnamespace",
	"LEFT JOIN pg_catalog.pg_available_extensions() AS a(name, default_version, comment) ON a.name = x.extname",
	"WHERE d.classid = 'pg_catalog.pg_extension'::pg_catalog.regclass\n\t\tAND d.refclassid = 'pg_catalog.pg_extension'::pg_catalog.regclass AND d.deptype = 'n'\n\tGROUP BY d.objid) AS req ON req.objid = x.oid",
	"pg_catalog.array_agg(r.extname::pg_catalog.text ORDER BY r.extname) AS names",
	"ROWS FROM (pg_catalog.unnest(x2.extconfig), pg_catalog.unnest(x2.extcondition))\n\t\t\t\tWITH ORDINALITY AS t(reloid, cond, ord)) AS u",
	"pg_catalog.array_agg(cn.nspname::pg_catalog.text ORDER BY u.ord) AS schemas",
	"pg_catalog.array_agg(c.relname::pg_catalog.text ORDER BY u.ord) AS names",
	"pg_catalog.array_agg(COALESCE(u.cond, '') ORDER BY u.ord) AS conditions",
	"JOIN pg_catalog.pg_class c ON c.oid = u.reloid\n\tJOIN pg_catalog.pg_namespace cn ON cn.oid = c.relnamespace\n\tGROUP BY u.extoid) AS cfg ON cfg.extoid = x.oid",
	"LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = x.oid\n\tAND ds.classoid = 'pg_catalog.pg_extension'::pg_catalog.regclass AND ds.objsubid = 0",
}

// The listing fills every field, pushes its filter into the query — name,
// schema, system by OID, membership as false — orders by name, and goes to
// the database's own pool.
func TestExtensionsFiltered(t *testing.T) {
	yes, no := true, false
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On(extensionMatch, rowsOf(extensionTypes, rowExtension1, rowExtension2))
	d := f.Server().DatabaseRef("appdb")
	es, err := d.Extensions(t.Context(), pgmo.ObjectFilter{
		Name:            []pgmo.TextCriterion{{Op: pgmo.TextNotEquals, Value: `"Plpgsql"`}},
		Schema:          []pgmo.TextCriterion{{Op: pgmo.TextContains, Value: "pg"}},
		System:          &yes,
		ExtensionMember: &no,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(es) != 2 {
		t.Fatalf("%d extensions, want 2", len(es))
	}
	checkExtensions(t, es, d)
	if n := len(f.Queries()); n != 1 {
		t.Errorf("%d statements on the Server's own database, want 1: the listing went to appdb", n)
	}
	last := lastQuery(other)
	for _, w := range extensionFragments {
		if !strings.Contains(last.Text, w) {
			t.Errorf("no %q in\n%s", w, last.Text)
		}
	}
	tail := "WHERE true AND x.extname <> $1::pg_catalog.text AND pg_catalog.strpos(n.nspname, $2) > 0 AND x.oid < 16384 AND NOT false\nORDER BY x.extname"
	if !strings.HasSuffix(last.Text, tail) || !slices.Equal(last.Args, []any{"Plpgsql", "pg"}) {
		t.Errorf("filter not pushed down, then ordered: args %v\n%s", last.Args, last.Text)
	}
	if _, err := d.Extensions(t.Context(), pgmo.ObjectFilter{ExtensionMember: &yes}); err != nil ||
		!strings.Contains(lastQuery(other).Text, "WHERE true AND false\n") {
		t.Errorf("ExtensionMember true: err %v, want AND false (no extension is a member)\n%s", err, lastQuery(other).Text)
	}
	n := len(other.Queries())
	if _, err := d.Extensions(t.Context(), pgmo.ObjectFilter{Name: []pgmo.TextCriterion{{Op: 99, Value: "x"}}}); !errors.Is(err, pgmo.ErrInvalidRequest) {
		t.Errorf("unknown operator: err = %v, want ErrInvalidRequest", err)
	}
	if len(other.Queries()) != n {
		t.Errorf("a refused filter was sent")
	}
}

// The three configuration arrays are parallel; arrays of different lengths
// are a failed read, not a misaligned table list.
func TestExtensionConfigArraysAligned(t *testing.T) {
	row := slices.Clone(rowExtension1)
	row[12] = []string{"WHERE x > 0"}
	f := pgmotest.New(t)
	f.On(extensionMatch, rowsOf(extensionTypes, row))
	d := f.Server().DatabaseRef(pgmotest.DefaultDatabase)
	_, err := d.Extensions(t.Context(), pgmo.ObjectFilter{})
	if err == nil || !strings.Contains(err.Error(), "configuration arrays of different lengths: 2 schemas, 2 names, 1 conditions") ||
		!strings.HasPrefix(err.Error(), "pgmo: list extensions of database ") {
		t.Errorf("err = %v, want the misalignment named", err)
	}
}

// ExtensionByName binds the name as $1, fills every field, and tells a
// missing extension from a failed read; the Ref reads nothing.
func TestExtensionByName(t *testing.T) {
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On(extensionMatch, rowsOf(extensionTypes, rowExtension1))
	d := f.Server().DatabaseRef("appdb")
	e, err := d.ExtensionByName(t.Context(), "plpgsql")
	if err != nil {
		t.Fatal(err)
	}
	checkExtensions(t, []*pgmo.Extension{e}, d)
	last := lastQuery(other)
	if !strings.HasSuffix(last.Text, "WHERE true\n\tAND x.extname = $1::pg_catalog.text") || last.Arg(0) != "plpgsql" ||
		strings.Contains(last.Text, "'plpgsql'") {
		t.Errorf("name not bound as $1: args %v\n%s", last.Args, last.Text)
	}
	if e.QualifiedName() != "plpgsql" {
		t.Errorf("QualifiedName() = %s", e.QualifiedName())
	}

	f = pgmotest.New(t)
	f.On(extensionMatch, rowsOf(extensionTypes))
	d = f.Server().DatabaseRef(pgmotest.DefaultDatabase)
	_, err = d.ExtensionByName(t.Context(), "Gone")
	if !errors.Is(err, pgmo.ErrNotFound) || !strings.Contains(err.Error(), `extension "Gone" not found in database "`+pgmotest.DefaultDatabase+`"`) {
		t.Errorf("missing: err = %v, want ErrNotFound naming it", err)
	}
	f = pgmotest.New(t)
	f.On(extensionMatch, pgmotest.Fail(&pgconn.PgError{Code: "57014", Message: "canceled"}))
	d = f.Server().DatabaseRef(pgmotest.DefaultDatabase)
	_, err = d.ExtensionByName(t.Context(), "citext")
	if err == nil || errors.Is(err, pgmo.ErrNotFound) || !strings.HasPrefix(err.Error(), "pgmo: read extension citext of database ") ||
		strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("server error: err = %v, want it wrapped once naming the read", err)
	}
	_, err = d.Extensions(t.Context(), pgmo.ObjectFilter{})
	if err == nil || !strings.HasPrefix(err.Error(), `pgmo: list extensions of database "`) || strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("listing, server error: err = %v, want it wrapped once naming the read", err)
	}

	n := len(f.Queries())
	r := d.ExtensionRef("E x")
	g := *r
	g.SetDatabase(nil)
	if r.Database() != d || !reflect.DeepEqual(g, pgmo.Extension{Name: "E x"}) || r.QualifiedName() != `"E x"` {
		t.Errorf("ExtensionRef = %+v", r)
	}
	if len(f.Queries()) != n {
		t.Errorf("ExtensionRef sent a statement")
	}
}

// extensionMembersMatch tells the members read apart in a fake.
const extensionMembersMatch = "pg_catalog.pg_identify_object(d.classid, d.objid, d.objsubid) AS i"

// Members keys the read by the extension's OID, bound as $1, in its own
// database; returns every column; refuses a Ref before sending anything;
// and wraps a server error once.
func TestExtensionMembers(t *testing.T) {
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On(extensionMatch, rowsOf(extensionTypes, rowExtension2))
	other.On(extensionMembersMatch, rowsOf([]uint32{pgtype.TextOID, pgtype.OIDOID, pgtype.TextOID, pgtype.TextOID},
		[]any{"pg_proc", uint32(80001), "function", `"E s".f(integer,pg_catalog.text)`},
		[]any{"pg_type", uint32(80002), "type", `"E s".earth`}))
	d := f.Server().DatabaseRef("appdb")
	e, err := d.ExtensionByName(t.Context(), "E x")
	if err != nil {
		t.Fatal(err)
	}
	ms, err := e.Members(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []pgmo.ExtensionMember{
		{Catalog: "pg_proc", OID: 80001, Type: "function", Identity: `"E s".f(integer,pg_catalog.text)`},
		{Catalog: "pg_type", OID: 80002, Type: "type", Identity: `"E s".earth`},
	}
	if !reflect.DeepEqual(ms, want) {
		t.Errorf("Members = %+v, want %+v", ms, want)
	}
	if n := reflect.TypeFor[pgmo.ExtensionMember]().NumField(); n != 4 {
		t.Errorf("ExtensionMember has %d fields; this test checks 4 — add the new one", n)
	}
	last := lastQuery(other)
	for _, w := range []string{
		"SELECT c.relname::pg_catalog.text, d.objid, i.type, i.identity\nFROM pg_catalog.pg_depend d\nJOIN pg_catalog.pg_class c ON c.oid = d.classid",
		"WHERE d.refclassid = 'pg_catalog.pg_extension'::pg_catalog.regclass AND d.refobjid = $1\n\tAND d.deptype = 'e'\nORDER BY i.type, i.identity",
	} {
		if !strings.Contains(last.Text, w) {
			t.Errorf("no %q in\n%s", w, last.Text)
		}
	}
	if last.Arg(0) != pgmo.OID(70002) {
		t.Errorf("args %v, want the OID 70002", last.Args)
	}

	n := len(other.Queries())
	if _, err := d.ExtensionRef("E x").Members(t.Context()); !errors.Is(err, pgmo.ErrHandleNotLoaded) ||
		err.Error() != `pgmo: list members of extension "E x": `+pgmo.ErrHandleNotLoaded.Error() {
		t.Errorf("Ref: err = %v, want ErrHandleNotLoaded", err)
	}
	if len(other.Queries()) != n {
		t.Errorf("a Ref's members were read")
	}

	f = pgmotest.New(t)
	f.On(extensionMatch, rowsOf(extensionTypes, rowExtension2))
	f.On(extensionMembersMatch, pgmotest.Fail(&pgconn.PgError{Code: "57014", Message: "canceled"}))
	e, err = f.Server().DatabaseRef(pgmotest.DefaultDatabase).ExtensionByName(t.Context(), "E x")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := e.Members(t.Context()); err == nil || !strings.HasPrefix(err.Error(), `pgmo: list members of extension "E x": `) ||
		strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("server error: err = %v, want it wrapped once naming the read", err)
	}
}

// The column types of the available extension read, in SELECT order.
var availableExtensionTypes = []uint32{pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID,
	pgtype.TextOID, pgtype.BoolOID, pgtype.BoolOID, pgtype.BoolOID, pgtype.BoolOID, pgtype.TextOID, pgtype.TextArrayOID,
	pgtype.TextOID}

// availableExtensionMatch tells the available extension read apart.
const availableExtensionMatch = "FROM pg_catalog.pg_available_extensions a\n"

// Rows of two available extensions: cube's versions out of order, as the
// server reads them from its directory, each flag set on some version
// alone; "No v", with no version at all (the LEFT JOIN's NULLs, coalesced
// but the version), and cube's comment, so only the name tells them apart.
var availableRows = [][]any{
	{"cube", "1.10", "1.9", "cubes", "1.10", false, true, false, false, "", []string{}, "v10"},
	{"cube", "1.10", "1.9", "cubes", "1.9", true, false, false, false, "", []string{}, ""},
	{"cube", "1.10", "1.9", "cubes", "1.4-1", false, false, true, false, "", []string{}, ""},
	{"cube", "1.10", "1.9", "cubes", "1.4", false, false, false, true, "pg_catalog", []string{"a", "B"}, ""},
	{"No v", "", "", "cubes", nil, false, false, false, false, "", nil, ""},
}

// The rows above as AvailableExtensions, without their Database: versions
// in version order.
var wantAvailable = []pgmo.AvailableExtension{
	{Name: "cube", DefaultVersion: "1.10", InstalledVersion: "1.9", Comment: "cubes", Versions: []pgmo.AvailableExtensionVersion{
		{Version: "1.4", Relocatable: true, Schema: "pg_catalog", Requires: []string{"a", "B"}},
		{Version: "1.4-1", Trusted: true},
		{Version: "1.9", Installed: true},
		{Version: "1.10", Superuser: true, Comment: "v10"},
	}},
	{Name: "No v", Comment: "cubes"},
}

// checkAvailable compares got with want, each read from d.
func checkAvailable(t *testing.T, got []*pgmo.AvailableExtension, want []pgmo.AvailableExtension, d *pgmo.Database) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("%d available extensions, want %d", len(got), len(want))
	}
	for i, a := range got {
		if a.Database() != d {
			t.Errorf("[%d].Database() = %v, want %v", i, a.Database(), d)
		}
		g := *a
		g.SetDatabase(nil)
		if !reflect.DeepEqual(g, want[i]) {
			t.Errorf("[%d] =\n%+v\nwant\n%+v", i, g, want[i])
		}
	}
	if n := reflect.TypeFor[pgmo.AvailableExtension]().NumField(); n != 6 {
		t.Errorf("AvailableExtension has %d fields; this test checks 6 — add the new one", n)
	}
	if n := reflect.TypeFor[pgmo.AvailableExtensionVersion]().NumField(); n != 8 {
		t.Errorf("AvailableExtensionVersion has %d fields; this test checks 8 — add the new one", n)
	}
}

// The fragments every available extension read holds.
var availableExtensionFragments = []string{
	"SELECT a.name::pg_catalog.text, COALESCE(a.default_version, ''),\n\tCOALESCE(a.installed_version, ''), COALESCE(a.comment, ''),",
	"v.version, COALESCE(v.installed, false), COALESCE(v.superuser, false), COALESCE(v.trusted, false),",
	"COALESCE(v.relocatable, false), COALESCE(v.schema::pg_catalog.text, ''), v.requires::pg_catalog.text[],\n\tCOALESCE(v.comment, '')",
	availableExtensionMatch + "LEFT JOIN pg_catalog.pg_available_extension_versions v ON v.name = a.name\nWHERE true",
}

// The listing groups the rows per extension, orders each one's versions,
// pushes its Name filter alone into the query, orders by name, and goes to
// the database's own pool.
func TestAvailableExtensions(t *testing.T) {
	yes := true
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On(availableExtensionMatch, rowsOf(availableExtensionTypes, availableRows...))
	d := f.Server().DatabaseRef("appdb")
	as, err := d.AvailableExtensions(t.Context(), pgmo.ObjectFilter{
		Name:            []pgmo.TextCriterion{{Op: pgmo.TextContains, Value: "C"}},
		Schema:          []pgmo.TextCriterion{{Op: pgmo.TextEquals, Value: "app"}},
		System:          &yes,
		ExtensionMember: &yes,
	})
	if err != nil {
		t.Fatal(err)
	}
	checkAvailable(t, as, wantAvailable, d)
	if n := len(f.Queries()); n != 1 {
		t.Errorf("%d statements on the Server's own database, want 1: the listing went to appdb", n)
	}
	last := lastQuery(other)
	for _, w := range availableExtensionFragments {
		if !strings.Contains(last.Text, w) {
			t.Errorf("no %q in\n%s", w, last.Text)
		}
	}
	if !strings.HasSuffix(last.Text, "WHERE true AND pg_catalog.strpos(a.name, $1) > 0\nORDER BY a.name") ||
		!slices.Equal(last.Args, []any{"c"}) {
		t.Errorf("filter not pushed down, then ordered: args %v\n%s", last.Args, last.Text)
	}

	f = pgmotest.New(t)
	f.On(availableExtensionMatch, pgmotest.Fail(&pgconn.PgError{Code: "57014", Message: "canceled"}))
	d = f.Server().DatabaseRef(pgmotest.DefaultDatabase)
	if _, err := d.AvailableExtensions(t.Context(), pgmo.ObjectFilter{}); err == nil ||
		!strings.HasPrefix(err.Error(), `pgmo: list available extensions of database "`) || strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("server error: err = %v, want it wrapped once naming the read", err)
	}
	n := len(f.Queries())
	if _, err := d.AvailableExtensions(t.Context(), pgmo.ObjectFilter{Name: []pgmo.TextCriterion{{Op: 99, Value: "x"}}}); !errors.Is(err, pgmo.ErrInvalidRequest) {
		t.Errorf("unknown operator: err = %v, want ErrInvalidRequest", err)
	}
	if len(f.Queries()) != n {
		t.Errorf("a refused filter was sent")
	}
}

// AvailableExtensionByName binds the name as $1, groups its rows into one,
// and tells an extension the server lacks from a failed read.
func TestAvailableExtensionByName(t *testing.T) {
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On(availableExtensionMatch, rowsOf(availableExtensionTypes, availableRows[:4]...))
	d := f.Server().DatabaseRef("appdb")
	a, err := d.AvailableExtensionByName(t.Context(), "cube")
	if err != nil {
		t.Fatal(err)
	}
	checkAvailable(t, []*pgmo.AvailableExtension{a}, wantAvailable[:1], d)
	last := lastQuery(other)
	if !strings.HasSuffix(last.Text, "WHERE true\n\tAND a.name = $1::pg_catalog.text") || last.Arg(0) != "cube" ||
		strings.Contains(last.Text, "'cube'") {
		t.Errorf("name not bound as $1: args %v\n%s", last.Args, last.Text)
	}

	f = pgmotest.New(t)
	f.On(availableExtensionMatch, rowsOf(availableExtensionTypes))
	d = f.Server().DatabaseRef(pgmotest.DefaultDatabase)
	_, err = d.AvailableExtensionByName(t.Context(), "Gone")
	if !errors.Is(err, pgmo.ErrNotFound) || !strings.Contains(err.Error(), `extension "Gone" not available on the server of database "`) {
		t.Errorf("missing: err = %v, want ErrNotFound naming it", err)
	}
	f = pgmotest.New(t)
	f.On(availableExtensionMatch, pgmotest.Fail(&pgconn.PgError{Code: "57014", Message: "canceled"}))
	d = f.Server().DatabaseRef(pgmotest.DefaultDatabase)
	_, err = d.AvailableExtensionByName(t.Context(), "cube")
	if err == nil || errors.Is(err, pgmo.ErrNotFound) || !strings.HasPrefix(err.Error(), "pgmo: read available extension cube of database ") ||
		strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("server error: err = %v, want it wrapped once naming the read", err)
	}
}

// Versions order by their number runs, then by text; a prefix first; and
// the order is total: equal only for equal strings.
func TestCompareExtensionVersions(t *testing.T) {
	ordered := []string{"", "0.9", "1", "1.0", "1.00", "1.2", "1.4", "1.4-1", "1.4-2", "1.4-10", "1.4a", "1.4b",
		"1.9", "1.10", "1.10.1", "2", "02", "10", "alpha", "beta", "beta2", "beta10"}
	for i, a := range ordered {
		for j, b := range ordered {
			want := 0
			if i < j {
				want = -1
			} else if i > j {
				want = 1
			}
			if got := pgmo.CompareExtensionVersions(a, b); got != want {
				t.Errorf("compare(%q, %q) = %d, want %d", a, b, got, want)
			}
		}
	}
	shuffled := []string{"1.10", "1.4", "1.8", "1.4-1", "1.9", "1.5"}
	slices.SortFunc(shuffled, pgmo.CompareExtensionVersions)
	if want := []string{"1.4", "1.4-1", "1.5", "1.8", "1.9", "1.10"}; !slices.Equal(shuffled, want) {
		t.Errorf("sorted = %q, want %q", shuffled, want)
	}
}
