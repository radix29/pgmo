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

// schemaRows answers the schema read with rows in its SELECT order, the
// columns typed as the server sends them.
func schemaRows(rows ...[]any) pgmotest.Responder {
	return func(pgmotest.Query) pgmotest.Result {
		return pgmotest.Result{
			Types: []uint32{pgtype.OIDOID, pgtype.TextOID, pgtype.TextOID, pgtype.BoolOID, pgtype.TextOID,
				pgtype.BoolOID,
				pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.BoolArrayOID,
				pgtype.TextArrayOID, pgtype.TextArrayOID,
				pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.BoolArrayOID,
				pgtype.TextOID},
			Rows: rows,
		}
	}
}

// Two schemas as pg18 answered for the live test's fixture: one owned by
// app, its ACL granting USAGE WITH GRANT OPTION to "B b" and CREATE to
// PUBLIC, with three default-privilege entries by two roles ("B b" before
// app: byte order), the first two of one object type; and pg_catalog, system, with no default privileges
// (NULLs).
var (
	rowSchemaApp = []any{uint32(20511), "app", "app", false, "", false,
		[]string{"app", "app", "B b", ""}, []string{"app", "app", "app", "app"},
		[]string{"USAGE", "CREATE", "USAGE", "CREATE"}, []bool{false, false, true, false},
		[]string{"B b", "app", "app", "app"}, []string{"T", "T", "r", "r"},
		[]string{"app", "B b", "B b", ""}, []string{"B b", "app", "app", "app"},
		[]string{"USAGE", "USAGE", "SELECT", "SELECT"}, []bool{true, false, false, false},
		"the app"}
	rowSchemaCatalog = []any{uint32(11), "pg_catalog", "postgres", true, "", false,
		[]string{"postgres", "postgres", ""}, []string{"postgres", "postgres", "postgres"},
		[]string{"USAGE", "CREATE", "USAGE"}, []bool{false, false, false},
		nil, nil, nil, nil, nil, nil,
		"system catalog schema"}
)

// Every field is filled from its own column — the ACL zipped from its
// arrays, default privileges grouped by role and object type — the name is
// bound as $1, never written into the text, and the read goes to the
// schema's own database.
func TestSchemaByNameFillsEveryField(t *testing.T) {
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On("FROM pg_catalog.pg_namespace n", schemaRows(rowSchemaApp))
	s := f.Server()
	d := s.DatabaseRef("appdb")

	got, err := d.SchemaByName(t.Context(), "app")
	if err != nil {
		t.Fatal(err)
	}
	if got.Database() != d {
		t.Errorf("Database() = %p, want %p", got.Database(), d)
	}
	app := func(grantee string, p pgmo.Privilege, grantable bool) pgmo.PrivilegeGrant {
		return pgmo.PrivilegeGrant{Grantee: grantee, Grantor: "app", Privilege: p, Grantable: grantable}
	}
	want := pgmo.Schema{
		OID: 20511, Name: "app", Owner: "app",
		Privileges: []pgmo.PrivilegeGrant{app("app", pgmo.PrivilegeUsage, false), app("app", pgmo.PrivilegeCreate, false),
			app("B b", pgmo.PrivilegeUsage, true), app("", pgmo.PrivilegeCreate, false)},
		DefaultPrivileges: []pgmo.DefaultPrivileges{
			{Role: "B b", Schema: "app", ObjectType: pgmo.DefaultPrivilegeTypes, Grants: []pgmo.PrivilegeGrant{
				{Grantee: "app", Grantor: "B b", Privilege: pgmo.PrivilegeUsage, Grantable: true}}},
			{Role: "app", Schema: "app", ObjectType: pgmo.DefaultPrivilegeTypes, Grants: []pgmo.PrivilegeGrant{
				app("B b", pgmo.PrivilegeUsage, false)}},
			{Role: "app", Schema: "app", ObjectType: pgmo.DefaultPrivilegeTables, Grants: []pgmo.PrivilegeGrant{
				app("B b", pgmo.PrivilegeSelect, false), app("", pgmo.PrivilegeSelect, false)}},
		},
		Comment: "the app",
	}
	g := *got
	g.SetDatabase(nil)
	if !reflect.DeepEqual(g, want) {
		t.Errorf("SchemaByName =\n%+v\nwant\n%+v", g, want)
	}
	if n := reflect.TypeFor[pgmo.Schema]().NumField(); n != 9 {
		t.Errorf("Schema has %d fields; this test checks 9 — add the new one", n)
	}
	if !got.Privileges[3].IsPublic() || got.Privileges[2].IsPublic() {
		t.Errorf("IsPublic: %v for PUBLIC, %v for B b", got.Privileges[3].IsPublic(), got.Privileges[2].IsPublic())
	}
	if n := len(f.Queries()); n != 1 { // the connect-time version read alone
		t.Errorf("%d statements on the Server's own database, want 1: the schema read went there", n)
	}
	q := other.Queries()
	last := q[len(q)-1]
	if last.Arg(0) != "app" || strings.Contains(last.Text, "'app'") {
		t.Errorf("name not bound as $1: args %v, text\n%s", last.Args, last.Text)
	}
	if !strings.Contains(last.Text, "n.nspname = $1::pg_catalog.text") {
		t.Errorf("name not matched exactly:\n%s", last.Text)
	}
	// A never-set ACL is read as the hard-wired default, and the default
	// privileges are this schema's alone.
	for _, w := range []string{"COALESCE(n.nspacl, pg_catalog.acldefault('n', n.nspowner))", "n.nspacl IS NULL",
		"WHERE d.defaclnamespace = n.oid"} {
		if !strings.Contains(last.Text, w) {
			t.Errorf("no %q in\n%s", w, last.Text)
		}
	}
}

// The listing scans every row (NULL arrays read as none) and pushes a name
// filter and System into its query, ordered by name; Schema narrows on the
// schema's name too, ExtensionMember on an extension's schema.
func TestSchemasFiltered(t *testing.T) {
	f := pgmotest.New(t)
	f.On("FROM pg_catalog.pg_namespace n", schemaRows(rowSchemaApp, rowSchemaCatalog))
	d := f.Server().DatabaseRef(pgmotest.DefaultDatabase)

	yes := true
	ss, err := d.Schemas(t.Context(), pgmo.ObjectFilter{
		Name:   []pgmo.TextCriterion{{Op: pgmo.TextContains, Value: "CAT"}},
		Schema: []pgmo.TextCriterion{{Op: pgmo.TextNotEquals, Value: "x"}},
		System: &yes,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 2 {
		t.Fatalf("Schemas = %d, want 2", len(ss))
	}
	c := ss[1]
	if c.Name != "pg_catalog" || !c.IsSystem || c.IsExtensionMember() || c.DefaultPrivileges != nil || len(c.Privileges) != 3 ||
		c.Privileges[2] != (pgmo.PrivilegeGrant{Grantee: "", Grantor: "postgres", Privilege: pgmo.PrivilegeUsage}) || c.Database() != d {
		t.Fatalf("pg_catalog = %+v", c)
	}
	q := f.Queries()
	last := q[len(q)-1]
	const tail = "WHERE true AND pg_catalog.strpos(n.nspname, $1) > 0 AND n.nspname <> $2::pg_catalog.text" +
		" AND (pg_catalog.starts_with(n.nspname, 'pg_') OR n.nspname = 'information_schema')\nORDER BY n.nspname"
	if !strings.HasSuffix(last.Text, tail) || strings.Count(last.Text, "WHERE true") != 1 ||
		len(last.Args) != 2 || last.Arg(0) != "cat" || last.Arg(1) != "x" {
		t.Errorf("filter not pushed down, then ordered: args %v\n%s", last.Args, last.Text)
	}

	// ExtensionMember is pg_depend membership of the schema itself.
	if _, err := d.Schemas(t.Context(), pgmo.ObjectFilter{ExtensionMember: &yes}); err != nil {
		t.Fatal(err)
	}
	q = f.Queries()
	if last := q[len(q)-1]; !strings.Contains(last.Text, "WHERE true AND EXISTS (") ||
		!strings.Contains(last.Text, "d.classid = 'pg_catalog.pg_namespace'::pg_catalog.regclass AND d.objid = n.oid") {
		t.Errorf("ExtensionMember: not the schema's membership:\n%s", last.Text)
	}

	// An unknown operator is refused before anything is sent.
	n := len(f.Queries())
	if _, err := d.Schemas(t.Context(), pgmo.ObjectFilter{Name: []pgmo.TextCriterion{{Op: 99, Value: "x"}}}); !errors.Is(err, pgmo.ErrInvalidRequest) {
		t.Errorf("unknown operator: err = %v, want ErrInvalidRequest", err)
	}
	if len(f.Queries()) != n {
		t.Errorf("a refused filter was sent")
	}
}

// No row is ErrNotFound naming the schema and its database; a server error
// and a malformed row are wrapped once, naming the read.
func TestSchemaByNameErrors(t *testing.T) {
	f := pgmotest.New(t)
	f.On("FROM pg_catalog.pg_namespace n", schemaRows())
	d := f.Server().DatabaseRef(pgmotest.DefaultDatabase)
	_, err := d.SchemaByName(t.Context(), "gone")
	if !errors.Is(err, pgmo.ErrNotFound) || !strings.Contains(err.Error(), `schema "gone" not found in database "`+pgmotest.DefaultDatabase+`"`) {
		t.Errorf("missing: err = %v, want ErrNotFound naming it", err)
	}

	f = pgmotest.New(t)
	f.On("FROM pg_catalog.pg_namespace n", pgmotest.Fail(&pgconn.PgError{Code: "57014", Message: "canceled"}))
	d = f.Server().DatabaseRef(pgmotest.DefaultDatabase)
	_, err = d.SchemaByName(t.Context(), "app")
	if err == nil || errors.Is(err, pgmo.ErrNotFound) || !strings.HasPrefix(err.Error(), `pgmo: read schema "app" of database `) ||
		strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("server error: err = %v, want it wrapped once naming the read", err)
	}

	// One array of each parallel set cut short: the ACL's grantors (7) and
	// grant options (9); the default privileges' roles (10), types (11) and
	// privileges (14).
	for _, col := range []int{7, 9, 10, 11, 14} {
		row := append([]any(nil), rowSchemaApp...)
		switch v := row[col].(type) {
		case []string:
			row[col] = v[:len(v)-1]
		case []bool:
			row[col] = v[:len(v)-1]
		}
		f = pgmotest.New(t)
		f.On("FROM pg_catalog.pg_namespace n", schemaRows(row))
		d = f.Server().DatabaseRef(pgmotest.DefaultDatabase)
		_, err = d.Schemas(t.Context(), pgmo.ObjectFilter{})
		if err == nil || !strings.HasPrefix(err.Error(), "pgmo: list schemas of database ") || !strings.Contains(err.Error(), "different lengths") {
			t.Errorf("column %d short: err = %v, want the mismatch wrapped naming the read", col, err)
		}
	}
}

// SchemaRef reads nothing and holds the name alone.
func TestSchemaRef(t *testing.T) {
	f := pgmotest.New(t)
	d := f.Server().DatabaseRef("appdb")
	n := len(f.Queries())
	s := d.SchemaRef("app")
	if len(f.Queries()) != n || f.Database("appdb").Opens() != 0 {
		t.Errorf("SchemaRef sent a statement")
	}
	g := *s
	g.SetDatabase(nil)
	if s.Database() != d || !reflect.DeepEqual(g, pgmo.Schema{Name: "app"}) {
		t.Errorf("SchemaRef = %+v", s)
	}
}

// defaultPrivilegeRows answers Database.DefaultPrivileges with rows in its
// SELECT order.
func defaultPrivilegeRows(rows ...[]any) pgmotest.Responder {
	return func(pgmotest.Query) pgmotest.Result {
		return pgmotest.Result{
			Types: []uint32{pgtype.TextOID, pgtype.TextOID, pgtype.TextOID,
				pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.BoolArrayOID},
			Rows: rows,
		}
	}
}

// One entry per row, in the read's order: an emptied database-wide entry
// (every privilege revoked, the owner's too) is kept with no grants; a code
// pgmo does not know is still read. The read goes to the database's own
// pool.
func TestDatabaseDefaultPrivileges(t *testing.T) {
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On("FROM pg_catalog.pg_default_acl d", defaultPrivilegeRows(
		[]any{"app", "", "f", []string{"app"}, []string{"app"}, []string{"EXECUTE"}, []bool{false}},
		[]any{"app", "", "r", nil, nil, nil, nil},
		[]any{"app", "s1", "x", []string{"B b", ""}, []string{"app", "app"}, []string{"SELECT", "SELECT"}, []bool{true, false}},
	))
	ds, err := f.Server().DatabaseRef("appdb").DefaultPrivileges(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []pgmo.DefaultPrivileges{
		{Role: "app", ObjectType: pgmo.DefaultPrivilegeFunctions, Grants: []pgmo.PrivilegeGrant{
			{Grantee: "app", Grantor: "app", Privilege: pgmo.PrivilegeExecute}}},
		{Role: "app", ObjectType: pgmo.DefaultPrivilegeTables},
		{Role: "app", Schema: "s1", ObjectType: "x", Grants: []pgmo.PrivilegeGrant{
			{Grantee: "B b", Grantor: "app", Privilege: pgmo.PrivilegeSelect, Grantable: true},
			{Grantee: "", Grantor: "app", Privilege: pgmo.PrivilegeSelect}}},
	}
	if !reflect.DeepEqual(ds, want) {
		t.Errorf("DefaultPrivileges =\n%+v\nwant\n%+v", ds, want)
	}
	if n := len(f.Queries()); n != 1 {
		t.Errorf("%d statements on the Server's own database, want 1: the read went there", n)
	}
	q := other.Queries()
	if last := q[len(q)-1]; !strings.HasSuffix(last.Text, "ORDER BY pg_catalog.pg_get_userbyid(d.defaclrole), n.nspname NULLS FIRST, d.defaclobjtype") {
		t.Errorf("not ordered by role, database-wide first, schema, type:\n%s", last.Text)
	}

	f = pgmotest.New(t)
	f.On("FROM pg_catalog.pg_default_acl d", defaultPrivilegeRows(
		[]any{"app", "", "f", []string{"app"}, []string{}, []string{"EXECUTE"}, []bool{false}}))
	_, err = f.Server().DatabaseRef(pgmotest.DefaultDatabase).DefaultPrivileges(t.Context())
	if err == nil || !strings.HasPrefix(err.Error(), "pgmo: read default privileges of database ") ||
		!strings.Contains(err.Error(), "different lengths") || strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("short array: err = %v, want the mismatch wrapped once naming the read", err)
	}
}

// String spells the ON clause of ALTER DEFAULT PRIVILEGES; an unknown code
// is shown as itself.
func TestDefaultPrivilegeObjectTypeString(t *testing.T) {
	for typ, want := range map[pgmo.DefaultPrivilegeObjectType]string{
		pgmo.DefaultPrivilegeTables: "TABLES", pgmo.DefaultPrivilegeSequences: "SEQUENCES",
		pgmo.DefaultPrivilegeFunctions: "FUNCTIONS", pgmo.DefaultPrivilegeTypes: "TYPES",
		pgmo.DefaultPrivilegeSchemas: "SCHEMAS", pgmo.DefaultPrivilegeLargeObjects: "LARGE OBJECTS",
		"x": `DefaultPrivilegeObjectType("x")`,
	} {
		if got := typ.String(); got != want {
			t.Errorf("%q.String() = %q, want %q", string(typ), got, want)
		}
	}
}
