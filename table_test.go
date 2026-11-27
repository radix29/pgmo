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

// rowsOf answers a read with rows typed as types.
func rowsOf(types []uint32, rows ...[]any) pgmotest.Responder {
	return func(pgmotest.Query) pgmotest.Result { return pgmotest.Result{Types: types, Rows: rows} }
}

// The column types of each read, in SELECT order, as the server sends them.
var (
	aclTypes   = []uint32{pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.BoolArrayOID}
	tableTypes = append([]uint32{pgtype.OIDOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.BoolOID, pgtype.TextOID,
		pgtype.TextOID, pgtype.TextOID, pgtype.BoolOID, pgtype.TextOID, pgtype.TextOID,
		pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.TextOID, pgtype.TextArrayOID,
		pgtype.BoolOID, pgtype.BoolOID, pgtype.Int8OID, pgtype.BoolOID}, append(aclTypes, pgtype.TextOID)...)
	columnTypes = append([]uint32{pgtype.Int4OID, pgtype.TextOID, pgtype.OIDOID, pgtype.TextOID, pgtype.BoolOID, pgtype.TextOID,
		pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID,
		pgtype.TextOID, pgtype.Int4OID, pgtype.BoolOID, pgtype.Int4OID}, append(aclTypes, pgtype.TextOID)...)
	constraintTypes = []uint32{pgtype.OIDOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextArrayOID,
		pgtype.BoolOID, pgtype.BoolOID, pgtype.BoolOID, pgtype.BoolOID, pgtype.BoolOID, pgtype.Int4OID, pgtype.BoolOID,
		pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextArrayOID,
		pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID}
	indexTypes = []uint32{pgtype.OIDOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID,
		pgtype.BoolOID, pgtype.BoolOID, pgtype.BoolOID, pgtype.BoolOID, pgtype.BoolOID, pgtype.BoolOID,
		pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID,
		pgtype.TextArrayOID, pgtype.TextOID}
	policyTypes = []uint32{pgtype.OIDOID, pgtype.TextOID, pgtype.TextOID, pgtype.BoolOID, pgtype.TextArrayOID,
		pgtype.TextOID, pgtype.TextOID, pgtype.TextOID}
	childTypes = []uint32{pgtype.OIDOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.BoolOID}
	sizeTypes  = []uint32{pgtype.Int8OID, pgtype.Int8OID, pgtype.Int8OID}
)

// A partition as pg18 answered for the live test's fixture, with every
// column set to something its neighbours are not: an extension member in a
// tablespace, with storage parameters (its TOAST table's prefixed), two
// parents, both RLS flags, a row estimate, a set ACL and a comment.
var rowTable = []any{uint32(30001), "app", "kid", "own", false, "citext",
	"r", "u", true, "", "FOR VALUES IN (1)",
	[]string{"app", "Other"}, []string{"base", "B b"}, "fast", []string{"fillfactor=70", "toast.autovacuum_enabled=false"},
	true, true, int64(42), false,
	[]string{"own", ""}, []string{"own", "own"}, []string{"SELECT", "SELECT"}, []bool{true, false},
	"the kid"}

// loadedTable reads rowTable through TableByName from database appdb of a
// fresh fake, which answers each read of a table's parts with the
// responders given.
func loadedTable(t *testing.T, parts map[string]pgmotest.Responder) (*pgmotest.Fake, *pgmotest.Fake, *pgmo.Table) {
	t.Helper()
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On("FROM pg_catalog.pg_class c\nJOIN", rowsOf(tableTypes, rowTable))
	for match, r := range parts {
		other.On(match, r)
	}
	tb, err := f.Server().DatabaseRef("appdb").TableByName(t.Context(), "app", "kid")
	if err != nil {
		t.Fatal(err)
	}
	return f, other, tb
}

// lastQuery is the last statement sent to f.
func lastQuery(f *pgmotest.Fake) pgmotest.Query {
	q := f.Queries()
	return q[len(q)-1]
}

// Every field is filled from its own column, the names are bound as $1 and
// $2, never written into the text, and the read goes to the table's own
// database.
func TestTableByNameFillsEveryField(t *testing.T) {
	f, other, got := loadedTable(t, nil)
	if got.Database() == nil || got.Database().Name != "appdb" {
		t.Errorf("Database() = %v", got.Database())
	}
	want := pgmo.Table{
		Origin: pgmo.Origin{Extension: "citext"}, OID: 30001, Schema: "app", Name: "kid", Owner: "own",
		Kind: pgmo.RelationTable, Persistence: pgmo.PersistenceUnlogged, IsPartition: true, PartitionBound: "FOR VALUES IN (1)",
		Parents:     []pgmo.ObjectName{{Schema: "app", Name: "base"}, {Schema: "Other", Name: "B b"}},
		Tablespace:  "fast",
		Options:     []pgmo.ParameterSetting{{Name: "fillfactor", Value: "70"}, {Name: "toast.autovacuum_enabled", Value: "false"}},
		RowSecurity: true, ForceRowSecurity: true, RowEstimate: 42,
		Privileges: []pgmo.PrivilegeGrant{{Grantee: "own", Grantor: "own", Privilege: pgmo.PrivilegeSelect, Grantable: true},
			{Grantee: "", Grantor: "own", Privilege: pgmo.PrivilegeSelect}},
		Comment: "the kid",
	}
	g := *got
	g.SetDatabase(nil)
	if !reflect.DeepEqual(g, want) {
		t.Errorf("TableByName =\n%+v\nwant\n%+v", g, want)
	}
	if n := reflect.TypeFor[pgmo.Table]().NumField(); n != 20 {
		t.Errorf("Table has %d fields; this test checks 20 — add the new one", n)
	}
	if n := len(f.Queries()); n != 1 {
		t.Errorf("%d statements on the Server's own database, want 1: the table read went there", n)
	}
	last := lastQuery(other)
	if last.Arg(0) != "app" || last.Arg(1) != "kid" || strings.Contains(last.Text, "'kid'") {
		t.Errorf("names not bound as $1, $2: args %v, text\n%s", last.Args, last.Text)
	}
	for _, w := range []string{"WHERE c.relkind IN ('r', 'p')",
		"AND n.nspname = $1::pg_catalog.text AND c.relname = $2::pg_catalog.text",
		"COALESCE(c.relacl, pg_catalog.acldefault('r', c.relowner))", "c.relacl IS NULL",
		"'toast.' || o FROM pg_catalog.unnest(tc.reloptions)", "ORDER BY i.inhseqno"} {
		if !strings.Contains(last.Text, w) {
			t.Errorf("no %q in\n%s", w, last.Text)
		}
	}
	if got.QualifiedName() != "app.kid" || got.IsPartitioned() || got.IsDefaultPartition() {
		t.Errorf("QualifiedName %s, IsPartitioned %v, IsDefaultPartition %v", got.QualifiedName(), got.IsPartitioned(), got.IsDefaultPartition())
	}
}

// The listing pushes its filter into the query and orders by schema then
// name; empty arrays and a NULL ACL read as none.
func TestTablesFiltered(t *testing.T) {
	f := pgmotest.New(t)
	bare := []any{uint32(30002), "app", "Parent", "own", false, "",
		"p", "p", false, "RANGE (at)", "", []string{}, []string{}, "", []string{},
		false, false, int64(-1), true, nil, nil, nil, nil, ""}
	f.On("FROM pg_catalog.pg_class c\nJOIN", rowsOf(tableTypes, bare))
	d := f.Server().DatabaseRef(pgmotest.DefaultDatabase)
	yes := true
	ts, err := d.Tables(t.Context(), pgmo.ObjectFilter{
		Name:            []pgmo.TextCriterion{{Op: pgmo.TextContains, Value: "PAR"}},
		Schema:          []pgmo.TextCriterion{{Op: pgmo.TextEquals, Value: "app"}},
		System:          &yes,
		ExtensionMember: &yes,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 1 || ts[0].Parents != nil || ts[0].Options != nil || ts[0].Privileges != nil || !ts[0].PrivilegesDefault ||
		!ts[0].IsPartitioned() || ts[0].PartitionKey != "RANGE (at)" || ts[0].RowEstimate != -1 {
		t.Fatalf("Tables = %+v", ts)
	}
	last := lastQuery(f)
	const tail = "WHERE c.relkind IN ('r', 'p') AND pg_catalog.strpos(c.relname, $1) > 0 AND n.nspname = $2::pg_catalog.text" +
		" AND (pg_catalog.starts_with(n.nspname, 'pg_') OR n.nspname = 'information_schema') AND EXISTS ("
	if !strings.Contains(last.Text, tail) || !strings.HasSuffix(last.Text, "\nORDER BY n.nspname, c.relname") ||
		!strings.Contains(last.Text, "d.classid = 'pg_catalog.pg_class'::pg_catalog.regclass AND d.objid = c.oid") ||
		len(last.Args) != 2 || last.Arg(0) != "par" || last.Arg(1) != "app" {
		t.Errorf("filter not pushed down, then ordered: args %v\n%s", last.Args, last.Text)
	}

	n := len(f.Queries())
	if _, err := d.Tables(t.Context(), pgmo.ObjectFilter{Name: []pgmo.TextCriterion{{Op: 99, Value: "x"}}}); !errors.Is(err, pgmo.ErrInvalidRequest) {
		t.Errorf("unknown operator: err = %v, want ErrInvalidRequest", err)
	}
	if len(f.Queries()) != n {
		t.Errorf("a refused filter was sent")
	}
}

// No schema is refused before anything is sent; no row is ErrNotFound; a
// server error and a malformed row are wrapped once, naming the read.
func TestTableByNameErrors(t *testing.T) {
	f := pgmotest.New(t)
	f.On("FROM pg_catalog.pg_class c\nJOIN", rowsOf(tableTypes))
	d := f.Server().DatabaseRef(pgmotest.DefaultDatabase)
	n := len(f.Queries())
	if _, err := d.TableByName(t.Context(), "", "kid"); !errors.Is(err, pgmo.ErrSchemaRequired) || !strings.HasPrefix(err.Error(), "pgmo: ") {
		t.Errorf("no schema: err = %v, want ErrSchemaRequired", err)
	}
	if len(f.Queries()) != n {
		t.Errorf("a schema-less read was sent")
	}
	_, err := d.TableByName(t.Context(), "app", "gone")
	if !errors.Is(err, pgmo.ErrNotFound) || !strings.Contains(err.Error(), `table app.gone not found in database "`+pgmotest.DefaultDatabase+`"`) {
		t.Errorf("missing: err = %v, want ErrNotFound naming it", err)
	}

	f = pgmotest.New(t)
	f.On("FROM pg_catalog.pg_class c\nJOIN", pgmotest.Fail(&pgconn.PgError{Code: "57014", Message: "canceled"}))
	_, err = f.Server().DatabaseRef(pgmotest.DefaultDatabase).TableByName(t.Context(), "app", "kid")
	if err == nil || errors.Is(err, pgmo.ErrNotFound) || !strings.HasPrefix(err.Error(), "pgmo: read table app.kid of database ") ||
		strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("server error: err = %v, want it wrapped once naming the read", err)
	}

	// The parent arrays (11, 12) and an ACL array (20) cut short.
	for _, col := range []int{11, 12, 20} {
		row := append([]any(nil), rowTable...)
		v := row[col].([]string)
		row[col] = v[:len(v)-1]
		f = pgmotest.New(t)
		f.On("FROM pg_catalog.pg_class c\nJOIN", rowsOf(tableTypes, row))
		_, err = f.Server().DatabaseRef(pgmotest.DefaultDatabase).Tables(t.Context(), pgmo.ObjectFilter{})
		if err == nil || !strings.HasPrefix(err.Error(), "pgmo: list tables of database ") || !strings.Contains(err.Error(), "different lengths") {
			t.Errorf("column %d short: err = %v, want the mismatch wrapped naming the read", col, err)
		}
	}
}

// TableRef reads nothing, holds the names alone, and every read of a part
// refuses it before sending anything.
func TestTableRef(t *testing.T) {
	f := pgmotest.New(t)
	d := f.Server().DatabaseRef("appdb")
	n := len(f.Queries())
	tb := d.TableRef("app", "kid")
	g := *tb
	g.SetDatabase(nil)
	if tb.Database() != d || !reflect.DeepEqual(g, pgmo.Table{Schema: "app", Name: "kid"}) {
		t.Errorf("TableRef = %+v", tb)
	}
	ctx := t.Context()
	for name, read := range map[string]func() error{
		"Columns":     func() error { _, err := tb.Columns(ctx); return err },
		"Constraints": func() error { _, err := tb.Constraints(ctx); return err },
		"Indexes":     func() error { _, err := tb.Indexes(ctx); return err },
		"Triggers":    func() error { _, err := tb.Triggers(ctx); return err },
		"Policies":    func() error { _, err := tb.Policies(ctx); return err },
		"Children":    func() error { _, err := tb.Children(ctx); return err },
		"Size":        func() error { _, err := tb.Size(ctx); return err },
	} {
		if err := read(); !errors.Is(err, pgmo.ErrHandleNotLoaded) || !strings.Contains(err.Error(), "app.kid") ||
			!strings.HasPrefix(err.Error(), "pgmo: ") {
			t.Errorf("%s on a TableRef: err = %v, want ErrHandleNotLoaded naming app.kid", name, err)
		}
	}
	if len(f.Queries()) != n || f.Database("appdb").Opens() != 0 {
		t.Errorf("a TableRef read sent a statement")
	}
}

// partRead is one read of a table's parts, for the checks every one of them
// shares.
type partRead struct {
	name, match, what string
	types             []uint32
	call              func(context.Context, *pgmo.Table) error
}

var partReads = []partRead{
	{"Columns", "FROM pg_catalog.pg_attribute a", "list columns of table app.kid", columnTypes,
		func(ctx context.Context, t *pgmo.Table) error { _, err := t.Columns(ctx); return err }},
	{"Constraints", "FROM pg_catalog.pg_constraint co", "list constraints of table app.kid", constraintTypes,
		func(ctx context.Context, t *pgmo.Table) error { _, err := t.Constraints(ctx); return err }},
	{"Indexes", "FROM pg_catalog.pg_index i", "list indexes of table app.kid", indexTypes,
		func(ctx context.Context, t *pgmo.Table) error { _, err := t.Indexes(ctx); return err }},
	{"Triggers", "FROM pg_catalog.pg_trigger tg", "list triggers of table app.kid", triggerTypes,
		func(ctx context.Context, t *pgmo.Table) error { _, err := t.Triggers(ctx); return err }},
	{"Policies", "FROM pg_catalog.pg_policy po", "list policies of table app.kid", policyTypes,
		func(ctx context.Context, t *pgmo.Table) error { _, err := t.Policies(ctx); return err }},
	{"Children", "FROM pg_catalog.pg_inherits i", "list children of table app.kid", childTypes,
		func(ctx context.Context, t *pgmo.Table) error { _, err := t.Children(ctx); return err }},
	{"Size", "pg_catalog.pg_table_size(c.oid)", "read size of table app.kid", sizeTypes,
		func(ctx context.Context, t *pgmo.Table) error { _, err := t.Size(ctx); return err }},
}

// Every read of a part goes to the table's database, keyed by its OID bound
// as $1; a server error is wrapped once, naming the read.
func TestTablePartReadsRouteAndWrap(t *testing.T) {
	for _, r := range partReads {
		t.Run(r.name, func(t *testing.T) {
			f, other, tb := loadedTable(t, map[string]pgmotest.Responder{
				r.match: pgmotest.Fail(&pgconn.PgError{Code: "57014", Message: "canceled"})})
			err := r.call(t.Context(), tb)
			if err == nil || !strings.HasPrefix(err.Error(), "pgmo: "+r.what+": ") || strings.Count(err.Error(), "pgmo: ") != 1 {
				t.Errorf("err = %v, want it wrapped once as %q", err, r.what)
			}
			if n := len(f.Queries()); n != 1 {
				t.Errorf("%d statements on the Server's own database, want 1", n)
			}
			last := lastQuery(other)
			if !strings.Contains(last.Text, r.match) || len(last.Args) != 1 || last.Arg(0) != pgmo.OID(30001) && last.Arg(0) != uint32(30001) {
				t.Errorf("not keyed by the OID as $1: args %v (%T)\n%s", last.Args, last.Arg(0), last.Text)
			}
		})
	}
}

// Columns: every field from its own column; a NULL column ACL is none.
func TestTableColumns(t *testing.T) {
	_, other, tb := loadedTable(t, map[string]pgmotest.Responder{
		"FROM pg_catalog.pg_attribute a": rowsOf(columnTypes,
			[]any{int32(6), "label", uint32(25), "text", true, "'x'::text", "", "", "", "pg_catalog", "C",
				"e", int32(500), false, int32(2), []string{"B b"}, []string{"own"}, []string{"UPDATE"}, []bool{true}, "lbl"},
			[]any{int32(9), "tripled", uint32(1700), "numeric", false, "", "d", "v", "(amount * (3)::numeric)", "", "",
				"m", int32(-1), true, int32(0), nil, nil, nil, nil, ""}),
	})
	cols, err := tb.Columns(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []pgmo.Column{
		{Number: 6, Name: "label", TypeOID: 25, Type: "text", NotNull: true, Default: "'x'::text",
			Collation: pgmo.ObjectName{Schema: "pg_catalog", Name: "C"}, Storage: pgmo.StorageExternal, StatisticsTarget: 500,
			InheritCount: 2, Privileges: []pgmo.PrivilegeGrant{{Grantee: "B b", Grantor: "own", Privilege: pgmo.PrivilegeUpdate, Grantable: true}},
			Comment: "lbl"},
		{Number: 9, Name: "tripled", TypeOID: 1700, Type: "numeric", Identity: pgmo.IdentityByDefault, Generated: pgmo.GeneratedVirtual,
			GenerationExpression: "(amount * (3)::numeric)", Storage: pgmo.StorageMain, StatisticsTarget: -1, IsLocal: true},
	}
	if len(cols) != 2 || !reflect.DeepEqual(*cols[0], want[0]) || !reflect.DeepEqual(*cols[1], want[1]) {
		t.Errorf("Columns =\n%+v\n%+v\nwant\n%+v", cols[0], cols[1], want)
	}
	if n := reflect.TypeFor[pgmo.Column]().NumField(); n != 16 {
		t.Errorf("Column has %d fields; this test checks 16 — add the new one", n)
	}
	last := lastQuery(other)
	for _, w := range []string{"WHERE a.attrelid = $1 AND a.attnum > 0 AND NOT a.attisdropped\nORDER BY a.attnum",
		"COALESCE(a.attstattarget, -1)::integer", "CASE WHEN a.attcollation <> t.typcollation",
		"pg_catalog.format_type(a.atttypid, a.atttypmod)", "pg_catalog.aclexplode(a.attacl)",
		"CASE WHEN a.attgenerated = '' THEN COALESCE(pg_catalog.pg_get_expr(ad.adbin, ad.adrelid), '') ELSE '' END"} {
		if !strings.Contains(last.Text, w) {
			t.Errorf("no %q in\n%s", w, last.Text)
		}
	}
}

// Constraints: every field from its own column; empty arrays are none;
// conenforced is read from 18 and true before; constraint triggers are
// not read.
func TestTableConstraints(t *testing.T) {
	for _, c := range []struct {
		version pgmo.ServerVersion
		want    string
	}{{pgmo.PostgreSQL15, "\n\ttrue,\n"}, {pgmo.PostgreSQL17, "\n\ttrue,\n"}, {pgmo.PostgreSQL18, "\n\tco.conenforced,\n"}} {
		f := pgmotest.New(t)
		f.Version = c.version
		other := f.Database("appdb")
		other.On("FROM pg_catalog.pg_class c\nJOIN", rowsOf(tableTypes, rowTable))
		other.On("FROM pg_catalog.pg_constraint co", rowsOf(constraintTypes,
			[]any{uint32(40001), "kid_fk", "f", "FOREIGN KEY (pid) REFERENCES app.parent(id) MATCH FULL", []string{"pid", "q"},
				true, true, false, false, false, int32(1), true, "",
				"app", "parent", []string{"id", "r"}, "c", "n", "f", "fk"},
			[]any{uint32(40002), "kid_pk", "p", "PRIMARY KEY (id)", []string{"id"},
				false, false, true, true, true, int32(0), false, "kid_pk",
				"", "", []string{}, "", "", "", ""}))
		tb, err := f.Server().DatabaseRef("appdb").TableByName(t.Context(), "app", "kid")
		if err != nil {
			t.Fatal(err)
		}
		cons, err := tb.Constraints(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		want := []pgmo.Constraint{
			{OID: 40001, Name: "kid_fk", Kind: pgmo.ConstraintForeignKey, Definition: "FOREIGN KEY (pid) REFERENCES app.parent(id) MATCH FULL",
				Columns: []string{"pid", "q"}, Deferrable: true, InitiallyDeferred: true, InheritCount: 1, NoInherit: true,
				ReferencedTable: pgmo.ObjectName{Schema: "app", Name: "parent"}, ReferencedColumns: []string{"id", "r"},
				OnUpdate: pgmo.ForeignKeyCascade, OnDelete: pgmo.ForeignKeySetNull, Match: pgmo.ForeignKeyMatchFull, Comment: "fk"},
			{OID: 40002, Name: "kid_pk", Kind: pgmo.ConstraintPrimaryKey, Definition: "PRIMARY KEY (id)", Columns: []string{"id"},
				Validated: true, Enforced: true, IsLocal: true, Index: "kid_pk"},
		}
		if len(cons) != 2 || !reflect.DeepEqual(*cons[0], want[0]) || !reflect.DeepEqual(*cons[1], want[1]) {
			t.Errorf("%s: Constraints =\n%+v\n%+v\nwant\n%+v", c.version, cons[0], cons[1], want)
		}
		last := lastQuery(other)
		if !strings.Contains(last.Text, c.want) || !strings.Contains(last.Text, "WHERE co.conrelid = $1 AND co.contype <> 't'\nORDER BY co.conname") {
			t.Errorf("%s: no %q, or constraint triggers read:\n%s", c.version, c.want, last.Text)
		}
	}
	if n := reflect.TypeFor[pgmo.Constraint]().NumField(); n != 19 {
		t.Errorf("Constraint has %d fields; this test checks 19 — add the new one", n)
	}
}

// Indexes: every field from its own column; no INCLUDE and no options are
// none.
func TestTableIndexes(t *testing.T) {
	_, other, tb := loadedTable(t, map[string]pgmotest.Responder{
		"FROM pg_catalog.pg_index i": rowsOf(indexTypes,
			[]any{uint32(50001), "kid_expr", "CREATE INDEX kid_expr ON app.kid USING gist (b) INCLUDE (c) WHERE (a > 0)", "gist",
				true, true, true, true, true, false, []string{"lower(b::text)", "a"}, []string{"c"}, "a > 0", "kid_pk", "fast",
				[]string{"fillfactor=80"}, "idx"},
			[]any{uint32(50002), "plain", "CREATE INDEX plain ON app.kid USING btree (a)", "btree",
				false, false, false, false, false, true, []string{"a"}, []string{}, "", "", "", nil, ""}),
	})
	xs, err := tb.Indexes(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []pgmo.Index{
		{OID: 50001, Name: "kid_expr", Definition: "CREATE INDEX kid_expr ON app.kid USING gist (b) INCLUDE (c) WHERE (a > 0)", Method: "gist",
			Unique: true, Primary: true, Exclusion: true, NullsNotDistinct: true, Clustered: true,
			KeyColumns: []string{"lower(b::text)", "a"}, IncludedColumns: []string{"c"}, Predicate: "a > 0", Constraint: "kid_pk",
			Tablespace: "fast", Options: []pgmo.ParameterSetting{{Name: "fillfactor", Value: "80"}}, Comment: "idx"},
		{OID: 50002, Name: "plain", Definition: "CREATE INDEX plain ON app.kid USING btree (a)", Method: "btree", Valid: true,
			KeyColumns: []string{"a"}},
	}
	if len(xs) != 2 || !reflect.DeepEqual(*xs[0], want[0]) || !reflect.DeepEqual(*xs[1], want[1]) {
		t.Errorf("Indexes =\n%+v\n%+v\nwant\n%+v", xs[0], xs[1], want)
	}
	if n := reflect.TypeFor[pgmo.Index]().NumField(); n != 17 {
		t.Errorf("Index has %d fields; this test checks 17 — add the new one", n)
	}
	last := lastQuery(other)
	for _, w := range []string{"pg_catalog.generate_series(1, i.indnkeyatts)", "pg_catalog.generate_series(i.indnkeyatts + 1, i.indnatts)",
		"AND co.contype IN ('p', 'u', 'x')", "WHERE i.indrelid = $1\nORDER BY ic.relname"} {
		if !strings.Contains(last.Text, w) {
			t.Errorf("no %q in\n%s", w, last.Text)
		}
	}
}

// Policies: PUBLIC is "", in stored order.
func TestTablePolicies(t *testing.T) {
	_, other, tb := loadedTable(t, map[string]pgmotest.Responder{
		"FROM pg_catalog.pg_policy po": rowsOf(policyTypes,
			[]any{uint32(70001), "p1", "w", false, []string{"B b", ""}, "(a > 0)", "(b > 0)", "pol"}),
	})
	ps, err := tb.Policies(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := pgmo.Policy{OID: 70001, Name: "p1", Command: pgmo.PolicyUpdate, Roles: []string{"B b", ""},
		Using: "(a > 0)", WithCheck: "(b > 0)", Comment: "pol"}
	if len(ps) != 1 || !reflect.DeepEqual(*ps[0], want) {
		t.Errorf("Policies = %+v, want %+v", ps, want)
	}
	if !strings.Contains(lastQuery(other).Text, "CASE WHEN r.oid = 0 THEN ''") {
		t.Errorf("PUBLIC not read as '':\n%s", lastQuery(other).Text)
	}
}

// Children and Size: every field; a dropped table's size is ErrNotFound.
func TestTableChildrenAndSize(t *testing.T) {
	_, _, tb := loadedTable(t, map[string]pgmotest.Responder{
		"FROM pg_catalog.pg_inherits i": rowsOf(childTypes,
			[]any{uint32(80001), "app", "p1", "f", "DEFAULT", true},
			[]any{uint32(80002), "Other", "kid 2", "r", "", false}),
		"pg_catalog.pg_table_size(c.oid)": rowsOf(sizeTypes, []any{int64(8192), int64(16384), int64(24576)}),
	})
	cs, err := tb.Children(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := []pgmo.ChildTable{
		{OID: 80001, ObjectName: pgmo.ObjectName{Schema: "app", Name: "p1"}, Kind: pgmo.RelationForeignTable, PartitionBound: "DEFAULT", DetachPending: true},
		{OID: 80002, ObjectName: pgmo.ObjectName{Schema: "Other", Name: "kid 2"}, Kind: pgmo.RelationTable},
	}
	if !reflect.DeepEqual(cs, want) || !cs[0].IsDefaultPartition() || cs[1].IsDefaultPartition() {
		t.Errorf("Children = %+v, want %+v", cs, want)
	}
	if cs[1].QualifiedName() != `"Other"."kid 2"` || (pgmo.ObjectName{Name: "t"}).QualifiedName() != "t" {
		t.Errorf("QualifiedName = %s", cs[1].QualifiedName())
	}
	sz, err := tb.Size(t.Context())
	if err != nil || sz != (pgmo.TableSize{Data: 8192, Indexes: 16384, Total: 24576}) {
		t.Errorf("Size = %+v, %v", sz, err)
	}

	_, _, tb = loadedTable(t, map[string]pgmotest.Responder{"pg_catalog.pg_table_size(c.oid)": rowsOf(sizeTypes)})
	if _, err := tb.Size(t.Context()); !errors.Is(err, pgmo.ErrNotFound) || !strings.Contains(err.Error(), "table app.kid not found") {
		t.Errorf("dropped: err = %v, want ErrNotFound", err)
	}
}

// String spells each code as its statement does; an unknown code is shown
// as itself.
func TestTableCodeStrings(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{pgmo.RelationTable.String(), "table"}, {pgmo.RelationPartitionedTable.String(), "partitioned table"},
		{pgmo.RelationView.String(), "view"}, {pgmo.RelationMaterializedView.String(), "materialized view"},
		{pgmo.RelationForeignTable.String(), "foreign table"}, {pgmo.RelationSequence.String(), "sequence"},
		{pgmo.RelationIndex.String(), "index"}, {pgmo.RelationPartitionedIndex.String(), "partitioned index"},
		{pgmo.RelationCompositeType.String(), "composite type"}, {pgmo.RelationTOASTTable.String(), "TOAST table"},
		{pgmo.RelationKind("x").String(), `RelationKind("x")`},
		{pgmo.PersistencePermanent.String(), "permanent"}, {pgmo.PersistenceUnlogged.String(), "UNLOGGED"},
		{pgmo.PersistenceTemporary.String(), "TEMPORARY"}, {pgmo.Persistence("x").String(), `Persistence("x")`},
		{pgmo.StoragePlain.String(), "PLAIN"}, {pgmo.StorageExternal.String(), "EXTERNAL"},
		{pgmo.StorageMain.String(), "MAIN"}, {pgmo.StorageExtended.String(), "EXTENDED"},
		{pgmo.StorageKind("z").String(), `StorageKind("z")`},
		{pgmo.ConstraintPrimaryKey.String(), "PRIMARY KEY"}, {pgmo.ConstraintUnique.String(), "UNIQUE"},
		{pgmo.ConstraintForeignKey.String(), "FOREIGN KEY"}, {pgmo.ConstraintCheck.String(), "CHECK"},
		{pgmo.ConstraintExclusion.String(), "EXCLUDE"}, {pgmo.ConstraintNotNull.String(), "NOT NULL"},
		{pgmo.ConstraintKind("z").String(), `ConstraintKind("z")`},
		{pgmo.ForeignKeyNoAction.String(), "NO ACTION"}, {pgmo.ForeignKeyRestrict.String(), "RESTRICT"},
		{pgmo.ForeignKeyCascade.String(), "CASCADE"}, {pgmo.ForeignKeySetNull.String(), "SET NULL"},
		{pgmo.ForeignKeySetDefault.String(), "SET DEFAULT"}, {pgmo.ForeignKeyAction("x").String(), `ForeignKeyAction("x")`},
		{pgmo.ForeignKeyMatchSimple.String(), "SIMPLE"}, {pgmo.ForeignKeyMatchFull.String(), "FULL"},
		{pgmo.ForeignKeyMatchPartial.String(), "PARTIAL"}, {pgmo.ForeignKeyMatch("x").String(), `ForeignKeyMatch("x")`},
		{pgmo.TriggerEnabledOrigin.String(), "ENABLE"}, {pgmo.TriggerEnabledReplica.String(), "ENABLE REPLICA"},
		{pgmo.TriggerEnabledAlways.String(), "ENABLE ALWAYS"}, {pgmo.TriggerDisabled.String(), "DISABLE"},
		{pgmo.TriggerEnabled("x").String(), `TriggerEnabled("x")`},
		{pgmo.PolicyAll.String(), "ALL"}, {pgmo.PolicySelect.String(), "SELECT"}, {pgmo.PolicyInsert.String(), "INSERT"},
		{pgmo.PolicyUpdate.String(), "UPDATE"}, {pgmo.PolicyDelete.String(), "DELETE"},
		{pgmo.PolicyCommand("x").String(), `PolicyCommand("x")`},
	} {
		if c.got != c.want {
			t.Errorf("String() = %q, want %q", c.got, c.want)
		}
	}
}
