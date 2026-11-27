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

// The column types of the foreign table read, in SELECT order, as the
// server sends them.
var foreignTableTypes = append([]uint32{pgtype.OIDOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.BoolOID, pgtype.TextOID,
	pgtype.TextOID, pgtype.TextArrayOID, pgtype.BoolOID, pgtype.TextOID, pgtype.TextArrayOID, pgtype.TextArrayOID,
	pgtype.BoolOID}, append(aclTypes, pgtype.TextOID)...)

// foreignTableMatch tells the foreign table read apart in a fake.
const foreignTableMatch = "JOIN pg_catalog.pg_foreign_table ft ON ft.ftrelid = c.oid"

// Two foreign tables with every column set to something its neighbour's is
// not.
var (
	rowForeignTable1 = []any{uint32(80001), "app", "remote", "own", false, "",
		"srv", []string{"schema_name=r", "table_name=t=u"}, false, "", []string{}, []string{}, false,
		[]string{"own", ""}, []string{"own", "own"}, []string{"SELECT", "SELECT"}, []bool{true, false}, "ft"}
	rowForeignTable2 = []any{uint32(80002), "pg_x", "Part 1", "postgres", true, "ext",
		"Odd S", []string{}, true, "FOR VALUES IN (1)", []string{"app"}, []string{"parted"}, true,
		[]string{"postgres"}, []string{"postgres"}, []string{"INSERT"}, []bool{false}, ""}
)

// The rows above as ForeignTables, without their Database.
var wantForeignTables = []pgmo.ForeignTable{
	{OID: 80001, Schema: "app", Name: "remote", Owner: "own", Server: "srv",
		Options: []pgmo.ParameterSetting{{Name: "schema_name", Value: "r"}, {Name: "table_name", Value: "t=u"}},
		Privileges: []pgmo.PrivilegeGrant{{Grantee: "own", Grantor: "own", Privilege: pgmo.PrivilegeSelect, Grantable: true},
			{Grantee: "", Grantor: "own", Privilege: pgmo.PrivilegeSelect}},
		Comment: "ft"},
	{Origin: pgmo.Origin{IsSystem: true, Extension: "ext"}, OID: 80002, Schema: "pg_x", Name: "Part 1", Owner: "postgres",
		Server: "Odd S", IsPartition: true, PartitionBound: "FOR VALUES IN (1)",
		Parents:           []pgmo.ObjectName{{Schema: "app", Name: "parted"}},
		Privileges:        []pgmo.PrivilegeGrant{{Grantee: "postgres", Grantor: "postgres", Privilege: pgmo.PrivilegeInsert}},
		PrivilegesDefault: true},
}

func checkForeignTables(t *testing.T, got []*pgmo.ForeignTable, d *pgmo.Database) {
	t.Helper()
	checkForeign(t, got, wantForeignTables, d, (*pgmo.ForeignTable).Database,
		func(ft *pgmo.ForeignTable) { ft.SetDatabase(nil) }, 14)
}

// The fragments every foreign table read holds.
var foreignTableFragments = []string{
	"SELECT c.oid, n.nspname::pg_catalog.text, c.relname::pg_catalog.text,\n\tpg_catalog.pg_get_userbyid(c.relowner)::pg_catalog.text,\n\t" +
		"(pg_catalog.starts_with(n.nspname, 'pg_') OR n.nspname = 'information_schema'), COALESCE(ext.extname::pg_catalog.text, ''),\n\t" +
		"fs.srvname::pg_catalog.text, COALESCE(ft.ftoptions, '{}'),\n\tc.relispartition, COALESCE(pg_catalog.pg_get_expr(c.relpartbound, c.oid), ''),",
	"FROM pg_catalog.pg_inherits i",
	"c.relacl IS NULL,",
	foreignTableMatch,
	"JOIN pg_catalog.pg_foreign_server fs ON fs.oid = ft.ftserver",
	"d.classid = 'pg_catalog.pg_class'::pg_catalog.regclass AND d.objid = c.oid",
	"LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = c.oid\n\tAND ds.classoid = 'pg_catalog.pg_class'::pg_catalog.regclass AND ds.objsubid = 0",
	"COALESCE(c.relacl, pg_catalog.acldefault('r', c.relowner))",
	"WHERE c.relkind = 'f'",
}

// The listing fills every field, pushes its filter into the query — name,
// schema, system schema, extension membership — orders by schema then
// name, and goes to the database's own pool.
func TestForeignTablesFiltered(t *testing.T) {
	no := false
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On(foreignTableMatch, rowsOf(foreignTableTypes, rowForeignTable1, rowForeignTable2))
	d := f.Server().DatabaseRef("appdb")
	fts, err := d.ForeignTables(t.Context(), pgmo.ObjectFilter{
		Name:            []pgmo.TextCriterion{{Op: pgmo.TextContains, Value: "Rem"}},
		Schema:          []pgmo.TextCriterion{{Op: pgmo.TextEquals, Value: "APP"}},
		System:          &no,
		ExtensionMember: &no,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(fts) != 2 {
		t.Fatalf("%d foreign tables, want 2", len(fts))
	}
	checkForeignTables(t, fts, d)
	if n := len(f.Queries()); n != 1 {
		t.Errorf("%d statements on the Server's own database, want 1: the listing went to appdb", n)
	}
	last := lastQuery(other)
	checkFragments(t, last, foreignTableFragments)
	tail := "WHERE c.relkind = 'f' AND pg_catalog.strpos(c.relname, $1) > 0 AND n.nspname = $2::pg_catalog.text AND NOT (pg_catalog.starts_with(n.nspname, 'pg_')"
	if !strings.Contains(last.Text, tail) || !strings.HasSuffix(last.Text, "\nORDER BY n.nspname, c.relname") ||
		!reflect.DeepEqual(last.Args, []any{"rem", "app"}) {
		t.Errorf("filter not pushed down, then ordered: args %v\n%s", last.Args, last.Text)
	}
}

// ForeignTableByName binds both names, fills every field, requires the
// schema, and tells a missing table from a failed read; the Ref reads
// nothing.
func TestForeignTableByName(t *testing.T) {
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On(foreignTableMatch, rowsOf(foreignTableTypes, rowForeignTable2))
	d := f.Server().DatabaseRef("appdb")
	ft, err := d.ForeignTableByName(t.Context(), "pg_x", "Part 1")
	if err != nil {
		t.Fatal(err)
	}
	g := *ft
	g.SetDatabase(nil)
	if ft.Database() != d || !reflect.DeepEqual(g, wantForeignTables[1]) {
		t.Errorf("= %+v, want %+v", g, wantForeignTables[1])
	}
	last := lastQuery(other)
	if !strings.HasSuffix(last.Text, "WHERE c.relkind = 'f'\n\tAND n.nspname = $1::pg_catalog.text AND c.relname = $2::pg_catalog.text") ||
		!reflect.DeepEqual(last.Args, []any{"pg_x", "Part 1"}) {
		t.Errorf("names not bound as $1, $2: args %v\n%s", last.Args, last.Text)
	}
	if ft.QualifiedName() != `pg_x."Part 1"` {
		t.Errorf("QualifiedName() = %s", ft.QualifiedName())
	}

	if _, err := d.ForeignTableByName(t.Context(), "", "remote"); !errors.Is(err, pgmo.ErrSchemaRequired) {
		t.Errorf("no schema: err = %v, want ErrSchemaRequired", err)
	}
	f = pgmotest.New(t)
	other = f.Database("appdb")
	other.On(foreignTableMatch, rowsOf(foreignTableTypes))
	d = f.Server().DatabaseRef("appdb")
	if _, err := d.ForeignTableByName(t.Context(), "app", "Gone"); !errors.Is(err, pgmo.ErrNotFound) ||
		!strings.Contains(err.Error(), `foreign table app."Gone" not found in database "appdb"`) {
		t.Errorf("missing: err = %v, want ErrNotFound naming it", err)
	}
	f = pgmotest.New(t)
	other = f.Database("appdb")
	other.On(foreignTableMatch, pgmotest.Fail(&pgconn.PgError{Code: "57014", Message: "canceled"}))
	d = f.Server().DatabaseRef("appdb")
	_, err = d.ForeignTableByName(t.Context(), "app", "remote")
	if err == nil || errors.Is(err, pgmo.ErrNotFound) || !strings.HasPrefix(err.Error(), `pgmo: read foreign table app.remote of database "appdb": `) ||
		strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("server error: err = %v, want it wrapped once naming the read", err)
	}
	_, err = d.ForeignTables(t.Context(), pgmo.ObjectFilter{})
	if err == nil || !strings.HasPrefix(err.Error(), `pgmo: list foreign tables of database "appdb": `) || strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("listing, server error: err = %v, want it wrapped once naming the read", err)
	}

	n := len(f.Queries())
	r := d.ForeignTableRef("app", "remote")
	g = *r
	g.SetDatabase(nil)
	if r.Database() != d || !reflect.DeepEqual(g, pgmo.ForeignTable{Schema: "app", Name: "remote"}) {
		t.Errorf("ForeignTableRef = %+v", r)
	}
	if len(f.Queries()) != n {
		t.Errorf("ForeignTableRef sent a statement")
	}
}

// Each part read goes to the foreign table's database keyed by its OID
// bound as $1, wraps a server error once naming the read, and refuses a
// Ref before sending anything.
func TestForeignTableParts(t *testing.T) {
	for _, r := range []struct {
		name, match, what string
		call              func(context.Context, *pgmo.ForeignTable) error
	}{
		{"Columns", "FROM pg_catalog.pg_attribute a", "list columns of foreign table app.remote",
			func(ctx context.Context, t *pgmo.ForeignTable) error { _, err := t.Columns(ctx); return err }},
		{"Constraints", "FROM pg_catalog.pg_constraint co", "list constraints of foreign table app.remote",
			func(ctx context.Context, t *pgmo.ForeignTable) error { _, err := t.Constraints(ctx); return err }},
		{"Triggers", "FROM pg_catalog.pg_trigger tg", "list triggers of foreign table app.remote",
			func(ctx context.Context, t *pgmo.ForeignTable) error { _, err := t.Triggers(ctx); return err }},
	} {
		t.Run(r.name, func(t *testing.T) {
			f := pgmotest.New(t)
			other := f.Database("appdb")
			other.On(foreignTableMatch, rowsOf(foreignTableTypes, rowForeignTable1))
			other.On(r.match, pgmotest.Fail(&pgconn.PgError{Code: "57014", Message: "canceled"}))
			d := f.Server().DatabaseRef("appdb")
			ft, err := d.ForeignTableByName(t.Context(), "app", "remote")
			if err != nil {
				t.Fatal(err)
			}
			err = r.call(t.Context(), ft)
			if err == nil || !strings.HasPrefix(err.Error(), "pgmo: "+r.what+": ") || strings.Count(err.Error(), "pgmo: ") != 1 {
				t.Errorf("err = %v, want it wrapped once as %q", err, r.what)
			}
			if n := len(f.Queries()); n != 1 {
				t.Errorf("%d statements on the Server's own database, want 1", n)
			}
			last := lastQuery(other)
			if !strings.Contains(last.Text, r.match) || len(last.Args) != 1 || last.Arg(0) != pgmo.OID(80001) && last.Arg(0) != uint32(80001) {
				t.Errorf("not keyed by the OID as $1: args %v (%T)\n%s", last.Args, last.Arg(0), last.Text)
			}

			n := len(other.Queries())
			if err := r.call(t.Context(), d.ForeignTableRef("app", "remote")); !errors.Is(err, pgmo.ErrHandleNotLoaded) ||
				!strings.HasPrefix(err.Error(), "pgmo: "+r.what+": ") {
				t.Errorf("Ref: err = %v, want ErrHandleNotLoaded naming the read", err)
			}
			if len(other.Queries()) != n {
				t.Errorf("a Ref's part read sent a statement")
			}
		})
	}

	// The constraints are read for the server's version: from 18,
	// conenforced (a foreign table's CHECK may be NOT ENFORCED).
	for _, c := range []struct {
		version pgmo.ServerVersion
		want    string
	}{{pgmo.PostgreSQL17, "\n\ttrue,\n"}, {pgmo.PostgreSQL18, "\n\tco.conenforced,\n"}} {
		f := pgmotest.New(t)
		f.Version = c.version
		other := f.Database("appdb")
		other.On(foreignTableMatch, rowsOf(foreignTableTypes, rowForeignTable1))
		other.On("FROM pg_catalog.pg_constraint co", rowsOf(constraintTypes))
		ft, err := f.Server().DatabaseRef("appdb").ForeignTableByName(t.Context(), "app", "remote")
		if err != nil {
			t.Fatal(err)
		}
		if _, err := ft.Constraints(t.Context()); err != nil {
			t.Fatal(err)
		}
		if last := lastQuery(other); !strings.Contains(last.Text, c.want) {
			t.Errorf("%s: no %q in\n%s", c.version, c.want, last.Text)
		}
	}

	// The columns are a table's, the foreign ones with their options.
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On(foreignTableMatch, rowsOf(foreignTableTypes, rowForeignTable1))
	other.On("FROM pg_catalog.pg_attribute a", rowsOf(columnTypes,
		[]any{int32(1), "id", uint32(23), "integer", true, "", "", "", "", "", "",
			"p", int32(-1), true, int32(0), nil, nil, nil, nil, []string{"column_name=ID"}, ""}))
	d := f.Server().DatabaseRef("appdb")
	ft, err := d.ForeignTableByName(t.Context(), "app", "remote")
	if err != nil {
		t.Fatal(err)
	}
	cols, err := ft.Columns(t.Context())
	want := pgmo.Column{Number: 1, Name: "id", TypeOID: 23, Type: "integer", NotNull: true, Storage: pgmo.StoragePlain,
		StatisticsTarget: -1, IsLocal: true, FDWOptions: []pgmo.ParameterSetting{{Name: "column_name", Value: "ID"}}}
	if err != nil || len(cols) != 1 || !reflect.DeepEqual(*cols[0], want) {
		t.Errorf("Columns = %+v, %v; want %+v", cols, err, want)
	}
}
