package pgmo_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/radix29/pgmo"
	"github.com/radix29/pgmo/pgmotest"
)

// The column types of the view and materialized view reads, in SELECT
// order, as the server sends them.
var (
	viewTypes = append([]uint32{pgtype.OIDOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.BoolOID, pgtype.TextOID,
		pgtype.TextOID, pgtype.TextOID, pgtype.BoolOID, pgtype.BoolOID, pgtype.TextArrayOID, pgtype.BoolOID},
		append(aclTypes, pgtype.TextOID)...)
	matviewTypes = append([]uint32{pgtype.OIDOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.BoolOID, pgtype.TextOID,
		pgtype.TextOID, pgtype.BoolOID, pgtype.TextOID, pgtype.TextArrayOID, pgtype.Int8OID, pgtype.BoolOID},
		append(aclTypes, pgtype.TextOID)...)
)

// A view and a materialized view with every column set to something its
// neighbours are not.
var (
	rowView = []any{uint32(31001), "app", "names", "own", false, "citext",
		" SELECT id\n   FROM app.t;", "cascaded", true, false, []string{"security_barrier=on", "check_option=cascaded"}, false,
		[]string{"own", ""}, []string{"own", "own"}, []string{"SELECT", "SELECT"}, []bool{true, false},
		"the names"}
	rowMatview = []any{uint32(32001), "app", "totals", "own", true, "",
		" SELECT count(*) AS n\n   FROM app.t;", false, "fast", []string{"fillfactor=70", "toast.autovacuum_enabled=false"},
		int64(42), false,
		[]string{"B b"}, []string{"own"}, []string{"SELECT"}, []bool{false},
		"the totals"}
)

// viewMatch and matviewMatch tell the two reads apart in a fake.
const (
	viewMatch    = "WHERE c.relkind = 'v'"
	matviewMatch = "WHERE c.relkind = 'm'"
)

// loadedViews reads rowView and rowMatview by name from database appdb of a
// fresh fake, which answers each part read with the responders given.
func loadedViews(t *testing.T, parts map[string]pgmotest.Responder) (*pgmotest.Fake, *pgmotest.Fake, *pgmo.View, *pgmo.MaterializedView) {
	t.Helper()
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On(viewMatch, rowsOf(viewTypes, rowView))
	other.On(matviewMatch, rowsOf(matviewTypes, rowMatview))
	for match, r := range parts {
		other.On(match, r)
	}
	d := f.Server().DatabaseRef("appdb")
	v, err := d.ViewByName(t.Context(), "app", "names")
	if err != nil {
		t.Fatal(err)
	}
	m, err := d.MaterializedViewByName(t.Context(), "app", "totals")
	if err != nil {
		t.Fatal(err)
	}
	return f, other, v, m
}

// Every field of both types is filled from its own column, the names are
// bound as $1 and $2, and the reads go to the view's own database.
func TestViewByNameFillsEveryField(t *testing.T) {
	f, other, v, m := loadedViews(t, nil)
	if v.Database() == nil || v.Database().Name != "appdb" || m.Database() == nil || m.Database().Name != "appdb" {
		t.Errorf("Database() = %v, %v", v.Database(), m.Database())
	}
	acl := []pgmo.PrivilegeGrant{{Grantee: "own", Grantor: "own", Privilege: pgmo.PrivilegeSelect, Grantable: true},
		{Grantee: "", Grantor: "own", Privilege: pgmo.PrivilegeSelect}}
	wantView := pgmo.View{
		Origin: pgmo.Origin{Extension: "citext"}, OID: 31001, Schema: "app", Name: "names", Owner: "own",
		Definition: " SELECT id\n   FROM app.t;", CheckOption: pgmo.CheckOptionCascaded, SecurityBarrier: true,
		Options:    []pgmo.ParameterSetting{{Name: "security_barrier", Value: "on"}, {Name: "check_option", Value: "cascaded"}},
		Privileges: acl, Comment: "the names",
	}
	gv := *v
	gv.SetDatabase(nil)
	if !reflect.DeepEqual(gv, wantView) {
		t.Errorf("ViewByName =\n%+v\nwant\n%+v", gv, wantView)
	}
	wantMatview := pgmo.MaterializedView{
		Origin: pgmo.Origin{IsSystem: true}, OID: 32001, Schema: "app", Name: "totals", Owner: "own",
		Definition: " SELECT count(*) AS n\n   FROM app.t;", Tablespace: "fast",
		Options:     []pgmo.ParameterSetting{{Name: "fillfactor", Value: "70"}, {Name: "toast.autovacuum_enabled", Value: "false"}},
		RowEstimate: 42,
		Privileges:  []pgmo.PrivilegeGrant{{Grantee: "B b", Grantor: "own", Privilege: pgmo.PrivilegeSelect}},
		Comment:     "the totals",
	}
	gm := *m
	gm.SetDatabase(nil)
	if !reflect.DeepEqual(gm, wantMatview) {
		t.Errorf("MaterializedViewByName =\n%+v\nwant\n%+v", gm, wantMatview)
	}
	if n := reflect.TypeFor[pgmo.View]().NumField(); n != 14 {
		t.Errorf("View has %d fields; this test checks 14 — add the new one", n)
	}
	if n := reflect.TypeFor[pgmo.MaterializedView]().NumField(); n != 14 {
		t.Errorf("MaterializedView has %d fields; this test checks 14 — add the new one", n)
	}
	if n := len(f.Queries()); n != 1 {
		t.Errorf("%d statements on the Server's own database, want 1: the view reads went to appdb", n)
	}
	qs := other.Queries()
	if len(qs) != 2 {
		t.Fatalf("%d statements on appdb, want 2", len(qs))
	}
	for i, c := range []struct {
		name  string
		wants []string
	}{
		{"names", []string{viewMatch,
			"COALESCE((SELECT o.option_value::pg_catalog.text FROM pg_catalog.pg_options_to_table(c.reloptions) AS o\n\t\tWHERE o.option_name = 'check_option'), '')",
			"COALESCE((SELECT o.option_value::boolean FROM pg_catalog.pg_options_to_table(c.reloptions) AS o\n\t\tWHERE o.option_name = 'security_barrier'), false)",
			"COALESCE((SELECT o.option_value::boolean FROM pg_catalog.pg_options_to_table(c.reloptions) AS o\n\t\tWHERE o.option_name = 'security_invoker'), false)",
			"COALESCE(c.reloptions, '{}'),"}},
		{"totals", []string{matviewMatch, "c.relispopulated", "'toast.' || o FROM pg_catalog.unnest(tc.reloptions)",
			"LEFT JOIN pg_catalog.pg_class tc ON tc.oid = c.reltoastrelid", "c.reltuples::bigint",
			"COALESCE(ts.spcname::pg_catalog.text, ''),", "LEFT JOIN pg_catalog.pg_tablespace ts ON ts.oid = c.reltablespace"}},
	} {
		q := qs[i]
		if q.Arg(0) != "app" || q.Arg(1) != c.name || strings.Contains(q.Text, "'"+c.name+"'") {
			t.Errorf("%s: names not bound as $1, $2: args %v, text\n%s", c.name, q.Args, q.Text)
		}
		for _, w := range append(c.wants, "COALESCE(pg_catalog.pg_get_viewdef(c.oid), '')",
			"COALESCE(c.relacl, pg_catalog.acldefault('r', c.relowner))", "c.relacl IS NULL",
			"AND n.nspname = $1::pg_catalog.text AND c.relname = $2::pg_catalog.text") {
			if !strings.Contains(q.Text, w) {
				t.Errorf("%s: no %q in\n%s", c.name, w, q.Text)
			}
		}
	}
	if v.QualifiedName() != "app.names" || m.QualifiedName() != "app.totals" {
		t.Errorf("QualifiedName %s, %s", v.QualifiedName(), m.QualifiedName())
	}
}

// Both listings push their filter into the query and order by schema then
// name; empty arrays and a NULL ACL read as none.
func TestViewsFiltered(t *testing.T) {
	yes := true
	filter := pgmo.ObjectFilter{
		Name:            []pgmo.TextCriterion{{Op: pgmo.TextContains, Value: "NAM"}},
		Schema:          []pgmo.TextCriterion{{Op: pgmo.TextEquals, Value: "app"}},
		System:          &yes,
		ExtensionMember: &yes,
	}
	for _, c := range []struct {
		kind  string
		types []uint32
		row   []any
		list  func(context.Context, *pgmo.Database) (bool, error)
	}{
		{"v", viewTypes, []any{uint32(31002), "app", "Names", "own", false, "", " SELECT 1;", "", false, false, []string{}, true,
			nil, nil, nil, nil, ""},
			func(ctx context.Context, d *pgmo.Database) (bool, error) {
				vs, err := d.Views(ctx, filter)
				return err == nil && len(vs) == 1 && vs[0].Options == nil && vs[0].Privileges == nil && vs[0].PrivilegesDefault &&
					vs[0].CheckOption == pgmo.CheckOptionNone, err
			}},
		{"m", matviewTypes, []any{uint32(32002), "app", "Names", "own", false, "", " SELECT 1;", true, "", []string{}, int64(-1), true,
			nil, nil, nil, nil, ""},
			func(ctx context.Context, d *pgmo.Database) (bool, error) {
				ms, err := d.MaterializedViews(ctx, filter)
				return err == nil && len(ms) == 1 && ms[0].Options == nil && ms[0].Privileges == nil && ms[0].PrivilegesDefault &&
					ms[0].Populated && ms[0].RowEstimate == -1, err
			}},
	} {
		f := pgmotest.New(t)
		f.On("WHERE c.relkind = '"+c.kind+"'", rowsOf(c.types, c.row))
		d := f.Server().DatabaseRef(pgmotest.DefaultDatabase)
		if ok, err := c.list(t.Context(), d); !ok {
			t.Fatalf("%s: listing wrong: %v", c.kind, err)
		}
		last := lastQuery(f)
		tail := "WHERE c.relkind = '" + c.kind + "' AND pg_catalog.strpos(c.relname, $1) > 0 AND n.nspname = $2::pg_catalog.text" +
			" AND (pg_catalog.starts_with(n.nspname, 'pg_') OR n.nspname = 'information_schema') AND EXISTS ("
		if !strings.Contains(last.Text, tail) || !strings.HasSuffix(last.Text, "\nORDER BY n.nspname, c.relname") ||
			!strings.Contains(last.Text, "d.classid = 'pg_catalog.pg_class'::pg_catalog.regclass AND d.objid = c.oid") ||
			len(last.Args) != 2 || last.Arg(0) != "nam" || last.Arg(1) != "app" {
			t.Errorf("%s: filter not pushed down, then ordered: args %v\n%s", c.kind, last.Args, last.Text)
		}
		n := len(f.Queries())
		bad := pgmo.ObjectFilter{Name: []pgmo.TextCriterion{{Op: 99, Value: "x"}}}
		if _, err := d.Views(t.Context(), bad); !errors.Is(err, pgmo.ErrInvalidRequest) {
			t.Errorf("Views, unknown operator: err = %v, want ErrInvalidRequest", err)
		}
		if _, err := d.MaterializedViews(t.Context(), bad); !errors.Is(err, pgmo.ErrInvalidRequest) {
			t.Errorf("MaterializedViews, unknown operator: err = %v, want ErrInvalidRequest", err)
		}
		if len(f.Queries()) != n {
			t.Errorf("a refused filter was sent")
		}
	}
}

// No schema is refused before anything is sent; no row is ErrNotFound; a
// server error and a malformed ACL are wrapped once, naming the read.
func TestViewByNameErrors(t *testing.T) {
	type byName func(context.Context, *pgmo.Database, string, string) error
	view := func(ctx context.Context, d *pgmo.Database, s, n string) error {
		_, err := d.ViewByName(ctx, s, n)
		return err
	}
	matview := func(ctx context.Context, d *pgmo.Database, s, n string) error {
		_, err := d.MaterializedViewByName(ctx, s, n)
		return err
	}
	for _, c := range []struct {
		what, match string
		types       []uint32
		row         []any
		read        byName
		list        func(context.Context, *pgmo.Database) error
	}{
		{"view", viewMatch, viewTypes, rowView, view,
			func(ctx context.Context, d *pgmo.Database) error {
				_, err := d.Views(ctx, pgmo.ObjectFilter{})
				return err
			}},
		{"materialized view", matviewMatch, matviewTypes, rowMatview, matview,
			func(ctx context.Context, d *pgmo.Database) error {
				_, err := d.MaterializedViews(ctx, pgmo.ObjectFilter{})
				return err
			}},
	} {
		f := pgmotest.New(t)
		f.On(c.match, rowsOf(c.types))
		d := f.Server().DatabaseRef(pgmotest.DefaultDatabase)
		n := len(f.Queries())
		if err := c.read(t.Context(), d, "", "v"); !errors.Is(err, pgmo.ErrSchemaRequired) ||
			!strings.HasPrefix(err.Error(), "pgmo: read "+c.what+` "v"`) {
			t.Errorf("%s, no schema: err = %v, want ErrSchemaRequired", c.what, err)
		}
		if len(f.Queries()) != n {
			t.Errorf("%s: a schema-less read was sent", c.what)
		}
		err := c.read(t.Context(), d, "app", "gone")
		if !errors.Is(err, pgmo.ErrNotFound) || !strings.Contains(err.Error(), c.what+` app.gone not found in database "`+pgmotest.DefaultDatabase+`"`) {
			t.Errorf("%s missing: err = %v, want ErrNotFound naming it", c.what, err)
		}

		f = pgmotest.New(t)
		f.On(c.match, pgmotest.Fail(&pgconn.PgError{Code: "57014", Message: "canceled"}))
		err = c.read(t.Context(), f.Server().DatabaseRef(pgmotest.DefaultDatabase), "app", "v")
		if err == nil || errors.Is(err, pgmo.ErrNotFound) || !strings.HasPrefix(err.Error(), "pgmo: read "+c.what+" app.v of database ") ||
			strings.Count(err.Error(), "pgmo: ") != 1 {
			t.Errorf("%s, server error: err = %v, want it wrapped once naming the read", c.what, err)
		}

		// The last ACL array, before the comment, cut short.
		row := append([]any(nil), c.row...)
		row[len(row)-2] = row[len(row)-2].([]bool)[:0]
		f = pgmotest.New(t)
		f.On(c.match, rowsOf(c.types, row))
		err = c.list(t.Context(), f.Server().DatabaseRef(pgmotest.DefaultDatabase))
		if err == nil || !strings.HasPrefix(err.Error(), "pgmo: list "+c.what+"s of database ") || !strings.Contains(err.Error(), "different lengths") {
			t.Errorf("%s, ACL short: err = %v, want the mismatch wrapped naming the read", c.what, err)
		}
	}
}

// A Ref reads nothing, holds the names alone, and every part read refuses
// it before sending anything.
func TestViewRefs(t *testing.T) {
	f := pgmotest.New(t)
	d := f.Server().DatabaseRef("appdb")
	n := len(f.Queries())
	v, m := d.ViewRef("app", "names"), d.MaterializedViewRef("app", "totals")
	gv, gm := *v, *m
	gv.SetDatabase(nil)
	gm.SetDatabase(nil)
	if v.Database() != d || !reflect.DeepEqual(gv, pgmo.View{Schema: "app", Name: "names"}) {
		t.Errorf("ViewRef = %+v", v)
	}
	if m.Database() != d || !reflect.DeepEqual(gm, pgmo.MaterializedView{Schema: "app", Name: "totals"}) {
		t.Errorf("MaterializedViewRef = %+v", m)
	}
	ctx := t.Context()
	for name, read := range map[string]func() error{
		"View.Columns":             func() error { _, err := v.Columns(ctx); return err },
		"MaterializedView.Columns": func() error { _, err := m.Columns(ctx); return err },
		"MaterializedView.Indexes": func() error { _, err := m.Indexes(ctx); return err },
		"MaterializedView.Size":    func() error { _, err := m.Size(ctx); return err },
	} {
		want := "app.totals"
		if strings.HasPrefix(name, "View.") {
			want = "app.names"
		}
		if err := read(); !errors.Is(err, pgmo.ErrHandleNotLoaded) || !strings.Contains(err.Error(), want) ||
			!strings.HasPrefix(err.Error(), "pgmo: ") {
			t.Errorf("%s on a Ref: err = %v, want ErrHandleNotLoaded naming %s", name, err, want)
		}
	}
	if len(f.Queries()) != n || f.Database("appdb").Opens() != 0 {
		t.Errorf("a Ref read sent a statement")
	}
}

// Every part read goes to the view's database, keyed by its OID bound as
// $1, with the query a table's part is read by; a server error is wrapped
// once, naming the read.
func TestViewPartReadsRouteAndWrap(t *testing.T) {
	for _, r := range []struct {
		name, match, what string
		oid               uint32
		call              func(context.Context, *pgmo.View, *pgmo.MaterializedView) error
	}{
		{"View.Columns", "FROM pg_catalog.pg_attribute a", "list columns of view app.names", 31001,
			func(ctx context.Context, v *pgmo.View, _ *pgmo.MaterializedView) error {
				_, err := v.Columns(ctx)
				return err
			}},
		{"MaterializedView.Columns", "FROM pg_catalog.pg_attribute a", "list columns of materialized view app.totals", 32001,
			func(ctx context.Context, _ *pgmo.View, m *pgmo.MaterializedView) error {
				_, err := m.Columns(ctx)
				return err
			}},
		{"MaterializedView.Indexes", "FROM pg_catalog.pg_index i", "list indexes of materialized view app.totals", 32001,
			func(ctx context.Context, _ *pgmo.View, m *pgmo.MaterializedView) error {
				_, err := m.Indexes(ctx)
				return err
			}},
		{"MaterializedView.Size", "pg_catalog.pg_table_size(c.oid)", "read size of materialized view app.totals", 32001,
			func(ctx context.Context, _ *pgmo.View, m *pgmo.MaterializedView) error {
				_, err := m.Size(ctx)
				return err
			}},
	} {
		t.Run(r.name, func(t *testing.T) {
			f, other, v, m := loadedViews(t, map[string]pgmotest.Responder{
				r.match: pgmotest.Fail(&pgconn.PgError{Code: "57014", Message: "canceled"})})
			err := r.call(t.Context(), v, m)
			if err == nil || !strings.HasPrefix(err.Error(), "pgmo: "+r.what+": ") || strings.Count(err.Error(), "pgmo: ") != 1 {
				t.Errorf("err = %v, want it wrapped once as %q", err, r.what)
			}
			if n := len(f.Queries()); n != 1 {
				t.Errorf("%d statements on the Server's own database, want 1", n)
			}
			last := lastQuery(other)
			if !strings.Contains(last.Text, r.match) || len(last.Args) != 1 || last.Arg(0) != pgmo.OID(r.oid) && last.Arg(0) != r.oid {
				t.Errorf("not keyed by the OID as $1: args %v (%T)\n%s", last.Args, last.Arg(0), last.Text)
			}
		})
	}
}

// The parts read the rows a table's do: a view's columns, a materialized
// view's columns, indexes and size; a dropped one's size is ErrNotFound.
func TestViewParts(t *testing.T) {
	_, _, v, m := loadedViews(t, map[string]pgmotest.Responder{
		"FROM pg_catalog.pg_attribute a": rowsOf(columnTypes,
			[]any{int32(1), "id", uint32(23), "integer", false, "7", "", "", "", "", "",
				"p", int32(-1), true, int32(0), []string{"B b"}, []string{"own"}, []string{"INSERT"}, []bool{false}, "key"}),
		"FROM pg_catalog.pg_index i": rowsOf(indexTypes,
			[]any{uint32(50001), "totals_n", "CREATE UNIQUE INDEX totals_n ON app.totals USING btree (n)", "btree",
				true, false, false, false, false, true, []string{"n"}, []string{}, "", "", "", nil, ""}),
		"pg_catalog.pg_table_size(c.oid)": rowsOf(sizeTypes, []any{int64(8192), int64(16384), int64(24576)}),
	})
	wantCol := pgmo.Column{Number: 1, Name: "id", TypeOID: 23, Type: "integer", Default: "7", Storage: pgmo.StoragePlain,
		StatisticsTarget: -1, IsLocal: true,
		Privileges: []pgmo.PrivilegeGrant{{Grantee: "B b", Grantor: "own", Privilege: pgmo.PrivilegeInsert}}, Comment: "key"}
	for what, cols := range map[string]func() ([]*pgmo.Column, error){
		"View": func() ([]*pgmo.Column, error) { return v.Columns(t.Context()) },
		"MaterializedView": func() ([]*pgmo.Column, error) {
			return m.Columns(t.Context())
		},
	} {
		cs, err := cols()
		if err != nil || len(cs) != 1 || !reflect.DeepEqual(*cs[0], wantCol) {
			t.Errorf("%s.Columns = %+v, %v; want %+v", what, cs, err, wantCol)
		}
	}
	xs, err := m.Indexes(t.Context())
	if err != nil || len(xs) != 1 || xs[0].Name != "totals_n" || !xs[0].Unique || xs[0].IncludedColumns != nil {
		t.Errorf("Indexes = %+v, %v", xs, err)
	}
	sz, err := m.Size(t.Context())
	if err != nil || sz != (pgmo.TableSize{Data: 8192, Indexes: 16384, Total: 24576}) {
		t.Errorf("Size = %+v, %v", sz, err)
	}

	_, _, _, m = loadedViews(t, map[string]pgmotest.Responder{"pg_catalog.pg_table_size(c.oid)": rowsOf(sizeTypes)})
	if _, err := m.Size(t.Context()); !errors.Is(err, pgmo.ErrNotFound) || !strings.Contains(err.Error(), "materialized view app.totals not found") {
		t.Errorf("dropped: err = %v, want ErrNotFound", err)
	}
}

// String spells each check option as CREATE VIEW does; an unknown one is
// shown as itself.
func TestViewCheckOptionString(t *testing.T) {
	for o, want := range map[pgmo.ViewCheckOption]string{
		pgmo.CheckOptionNone: "NONE", pgmo.CheckOptionLocal: "LOCAL", pgmo.CheckOptionCascaded: "CASCADED",
		pgmo.ViewCheckOption("x"): `ViewCheckOption("x")`,
	} {
		if got := o.String(); got != want {
			t.Errorf("%q.String() = %q, want %q", string(o), got, want)
		}
	}
}
