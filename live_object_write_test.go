//go:build livedb

package pgmo

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/radix29/pgmo/pgsql"
)

// liveObjectWrites is TestLiveObjectWrites' fixture: a throwaway database,
// two roles, a Server on postgres writing into the database through its
// pool, and an admin connection to the database for what the test does by
// hand.
type liveObjectWrites struct {
	t     *testing.T
	ctx   context.Context
	s     *Server
	d     *Database
	conn  *pgx.Conn
	num   int
	own   string // a NOLOGIN role objects are handed to
	own2  string
	db    string
	stamp int64
}

// ok fails the test now when err is not nil.
func (l *liveObjectWrites) ok(what string, err error) {
	l.t.Helper()
	if err != nil {
		l.t.Fatalf("%s: %v", what, err)
	}
}

// exec runs stmt by hand in the throwaway database.
func (l *liveObjectWrites) exec(stmt string) {
	l.t.Helper()
	if _, err := l.conn.Exec(l.ctx, stmt); err != nil {
		l.t.Fatalf("%s: %v", stmt, err)
	}
}

// scalar reads one value by hand in the throwaway database.
func (l *liveObjectWrites) scalar(query string, args ...any) string {
	l.t.Helper()
	var v string
	if err := l.conn.QueryRow(l.ctx, query, args...).Scan(&v); err != nil {
		l.t.Fatalf("%s: %v", query, err)
	}
	return v
}

// wantCode fails unless err is the server's sqlstate.
func (l *liveObjectWrites) wantCode(what string, err error, sqlstate string) {
	l.t.Helper()
	if pe, ok := AsPgError(err); !ok || pe.Code != sqlstate {
		l.t.Errorf("%s: %v, want SQLSTATE %s", what, err, sqlstate)
	}
}

// scripted is the one statement call captures under WithScript.
func (l *liveObjectWrites) scripted(call func(ctx context.Context) error) string {
	l.t.Helper()
	ctx, script := WithScript(l.ctx)
	l.ok("scripted call", call(ctx))
	e := script.Entries()
	if len(e) != 1 || e[0].Database != l.db {
		l.t.Fatalf("captured %+v, want one statement in %q", e, l.db)
	}
	return e[0].SQL
}

func newLiveObjectWrites(t *testing.T) *liveObjectWrites {
	admin, ctx, done := liveDBTimeout(t, 3*time.Minute)
	t.Cleanup(done)
	stamp := time.Now().UnixNano()
	l := &liveObjectWrites{t: t, ctx: ctx, stamp: stamp,
		own:  fmt.Sprintf("pgmo_live_objw_%d_own", stamp),
		own2: fmt.Sprintf("pgmo_live_objw_%d Own'2", stamp),
		db:   fmt.Sprintf(`pgmo_live_objw_%d "DB"`, stamp),
	}
	q := pgsql.QuoteIdent
	t.Cleanup(func() {
		for _, stmt := range []string{"DROP DATABASE IF EXISTS " + q(l.db) + " WITH (FORCE)",
			"DROP ROLE IF EXISTS " + q(l.own), "DROP ROLE IF EXISTS " + q(l.own2)} {
			if _, err := admin.Exec(context.Background(), stmt); err != nil {
				t.Errorf("%s: %v", stmt, err)
			}
		}
	})
	for _, stmt := range []string{"CREATE DATABASE " + q(l.db), "CREATE ROLE " + q(l.own), "CREATE ROLE " + q(l.own2)} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	cfg, err := pgx.ParseConfig(*liveConnString)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = l.db
	if l.conn, err = pgx.ConnectConfig(ctx, cfg); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { l.conn.Close(context.Background()) })
	if l.s, err = Connect(ctx, ConnectionOptions{ConnString: *liveConnString, Database: "postgres",
		ApplicationName: "pgmo - live object writes"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(l.s.Close) // before the drop: the database's sessions end first
	l.num = int(l.s.Version())
	l.d = l.s.DatabaseRef(l.db)
	return l
}

// TestLiveObjectWrites drives every P4.13.3 write against a throwaway
// database: each family created from a request and read back field by
// field, each ALTER read back equal to what it mirrored, each comment set
// and removed, each drop refused while something depends on the object
// and done with CASCADE. A statement captured under WithScript, run by
// hand, makes the same object. Writes go through the database's own pool
// from a Server connected to postgres.
func TestLiveObjectWrites(t *testing.T) {
	l := newLiveObjectWrites(t)
	l.schemas()
	l.types()
	l.sequences()
	l.tables()
	l.indexes()
	l.views()
	l.routines()
	l.triggers()
	l.extensions()
	l.comments()
	l.transaction()
	l.drops()
}

const liveObjSchema = `App "S"`

func (l *liveObjectWrites) schemas() {
	t, d := l.t, l.d
	sc, err := d.CreateSchema(l.ctx, CreateSchemaRequest{Name: liveObjSchema, Owner: l.own})
	l.ok("create schema", err)
	if sc.OID == 0 || sc.Owner != l.own {
		t.Errorf("schema read back %+v, want owner %s", sc, l.own)
	}
	_, err = d.CreateSchema(l.ctx, CreateSchemaRequest{Name: "other"})
	l.ok("create schema other", err)
	l.ok("rename", sc.Rename(l.ctx, "renamed"))
	l.ok("owner", sc.SetOwner(l.ctx, l.own2))
	l.ok("comment", sc.SetComment(l.ctx, "it's \\ here"))
	got, err := d.SchemaByName(l.ctx, "renamed")
	l.ok("read renamed schema", err)
	if got.Owner != sc.Owner || got.Comment != sc.Comment || got.Name != sc.Name {
		t.Errorf("schema read back %+v, mirrored %+v", got, sc)
	}
	l.ok("rename back", sc.Rename(l.ctx, liveObjSchema))
	l.ok("uncomment", sc.SetComment(l.ctx, ""))
	if got, err := d.SchemaByName(l.ctx, liveObjSchema); err != nil || got.Comment != "" {
		t.Errorf("schema after removing its comment: %+v, %v", got, err)
	}
	l.exec("GRANT CREATE, USAGE ON SCHEMA " + pgsql.QuoteIdent(liveObjSchema) + " TO PUBLIC")
}

func (l *liveObjectWrites) types() {
	t, d, ctx := l.t, l.d, l.ctx
	mood, err := d.CreateEnumType(ctx, CreateEnumTypeRequest{Schema: liveObjSchema, Name: "mood", Labels: []string{"sad", "o'k"}})
	l.ok("create enum", err)
	if !slices.Equal(mood.Labels, []string{"sad", "o'k"}) || mood.Kind != TypeEnum {
		t.Errorf("enum read back %+v", mood)
	}
	l.ok("add value", mood.AddEnumValue(ctx, "happy", EnumValuePosition{}))
	l.ok("add value before", mood.AddEnumValue(ctx, "awful", EnumValuePosition{Before: "sad"}))
	l.ok("add value after", mood.AddEnumValue(ctx, `a\b`, EnumValuePosition{After: "sad"}))
	l.ok("rename value", mood.RenameEnumValue(ctx, "o'k", "fine"))
	got, err := d.TypeByName(ctx, liveObjSchema, "mood")
	l.ok("read enum", err)
	if want := []string{"awful", "sad", `a\b`, "fine", "happy"}; !slices.Equal(got.Labels, want) || !slices.Equal(mood.Labels, want) {
		t.Errorf("enum labels %q, mirrored %q, want %q", got.Labels, mood.Labels, want)
	}

	comp, err := d.CreateCompositeType(ctx, CreateCompositeTypeRequest{Schema: liveObjSchema, Name: "pair", Attributes: []TypeAttributeSpec{
		{Name: "Key", Type: "text", Collation: ObjectName{Schema: "pg_catalog", Name: "C"}}, {Name: "n", Type: "numeric(5,1)"}}})
	l.ok("create composite", err)
	if len(comp.Attributes) != 2 || comp.Attributes[0].Name != "Key" || comp.Attributes[0].Collation != (ObjectName{Schema: "pg_catalog", Name: "C"}) ||
		comp.Attributes[1].Type != "numeric(5,1)" {
		t.Errorf("composite read back %+v", comp.Attributes)
	}
	rng, err := d.CreateRangeType(ctx, CreateRangeTypeRequest{Schema: liveObjSchema, Name: "span", Subtype: "double precision",
		SubtypeDiff: ObjectName{Schema: "pg_catalog", Name: "float8mi"}, Multirange: ObjectName{Schema: liveObjSchema, Name: "Spans"}})
	l.ok("create range", err)
	if rng.Kind != TypeRange || rng.Subtype != "double precision" || rng.Multirange != (ObjectName{Schema: liveObjSchema, Name: "Spans"}) ||
		rng.SubtypeDiff != (ObjectName{Schema: "pg_catalog", Name: "float8mi"}) {
		t.Errorf("range read back %+v", rng)
	}
	for _, ty := range []*Type{comp, rng} {
		l.ok("type comment", ty.SetComment(ctx, "c"))
		l.ok("type owner", ty.SetOwner(ctx, l.own))
	}
	l.ok("type move", rng.SetSchema(ctx, "other"))
	l.ok("type rename", rng.Rename(ctx, "Span 2"))
	if got, err := d.TypeByName(ctx, "other", "Span 2"); err != nil || got.Owner != l.own || got.Comment != "c" {
		t.Errorf("range after the alters: %+v, %v", got, err)
	}
	l.ok("drop range", rng.Drop(ctx, DropOptions{}))

	dom, err := d.CreateDomain(ctx, CreateDomainRequest{Schema: liveObjSchema, Name: "code", BaseType: "text",
		Collation: ObjectName{Schema: "pg_catalog", Name: "C"}, Default: "'x'::text", NotNull: true,
		Constraints: []DomainConstraintSpec{{Name: "short", Check: "length(VALUE) < 10"}, {Check: "VALUE <> ''"}}})
	l.ok("create domain", err)
	checks := func(cs []DomainConstraint) int {
		n := 0
		for _, c := range cs {
			if c.Kind == ConstraintCheck {
				n++
			}
		}
		return n
	}
	// From 17 a domain's NOT NULL is a constraint row of its own too.
	if dom.BaseType != "text" || !dom.NotNull || dom.Default != "'x'::text" || checks(dom.Constraints) != 2 ||
		dom.Collation != (ObjectName{Schema: "pg_catalog", Name: "C"}) {
		t.Errorf("domain read back %+v", dom)
	}
	l.ok("domain default", dom.SetDefault(ctx, "'y'::text"))
	l.ok("domain null", dom.SetNotNull(ctx, false))
	l.ok("domain constraint", dom.AddConstraint(ctx, DomainConstraintSpec{Name: "No'Space", Check: "VALUE !~ ' '", NotValid: true}))
	got2, err := d.DomainByName(ctx, liveObjSchema, "code")
	l.ok("read domain", err)
	if got2.Default != dom.Default || got2.NotNull || len(got2.Constraints) != 3 || checks(got2.Constraints) != 3 {
		t.Errorf("domain after the alters %+v, mirrored %+v", got2, dom)
	}
	for _, c := range got2.Constraints {
		if c.Name == "No'Space" && c.Validated {
			t.Errorf("NOT VALID domain constraint read validated")
		}
	}
	l.ok("domain validate", dom.ValidateConstraint(ctx, "No'Space"))
	l.ok("domain drop constraint", dom.DropConstraint(ctx, "short", DropOptions{}))
	l.ok("domain default dropped", dom.SetDefault(ctx, ""))
	if got, err := d.DomainByName(ctx, liveObjSchema, "code"); err != nil || got.Default != "" || len(got.Constraints) != 2 ||
		!slices.ContainsFunc(got.Constraints, func(c DomainConstraint) bool { return c.Name == "No'Space" && c.Validated }) {
		t.Errorf("domain after validate and drop: %+v, %v", got, err)
	}
}

func (l *liveObjectWrites) sequences() {
	t, d, ctx := l.t, l.d, l.ctx
	seq, err := d.CreateSequence(ctx, CreateSequenceRequest{Schema: liveObjSchema, Name: "Seq'1", Unlogged: true,
		SequenceOptions: SequenceOptions{DataType: "integer", Increment: new(int64(-2)), MinValue: new(int64(-1000)),
			MaxValue: new(int64(-1)), Start: new(int64(-10)), Cache: new(int64(5)), Cycle: new(true)}})
	l.ok("create sequence", err)
	if seq.DataType != "integer" || seq.Increment != -2 || seq.MinValue != -1000 || seq.MaxValue != -1 || seq.Start != -10 ||
		seq.Cache != 5 || !seq.Cycle || seq.Persistence != PersistenceUnlogged {
		t.Errorf("sequence read back %+v", seq)
	}
	l.ok("alter sequence", seq.Alter(ctx, SequenceChanges{SequenceOptions: SequenceOptions{Increment: new(int64(-3)), Cycle: new(false),
		Cache: new(int64(1))}, Restart: new(int64(-20))}))
	got, err := d.SequenceByName(ctx, liveObjSchema, "Seq'1")
	l.ok("read sequence", err)
	if got.Increment != seq.Increment || got.Cycle != seq.Cycle || got.Cache != seq.Cache || got.Start != -10 {
		t.Errorf("sequence read back %+v, mirrored %+v", got, seq)
	}
	if v := l.scalar("SELECT nextval($1::regclass)::text", pgsql.QualifiedName(liveObjSchema, "Seq'1")); v != "-20" {
		t.Errorf("nextval after RESTART WITH -20 = %s", v)
	}
	l.ok("sequence owner", seq.SetOwner(ctx, l.own))
	l.ok("sequence comment", seq.SetComment(ctx, "s"))
	l.ok("sequence rename", seq.Rename(ctx, "s2"))
	l.ok("sequence move", seq.SetSchema(ctx, "other"))
	if got, err := d.SequenceByName(ctx, "other", "s2"); err != nil || got.Owner != l.own || got.Comment != "s" {
		t.Errorf("sequence after the alters: %+v, %v", got, err)
	}
}

// liveTableRequest is the table both CreateTable and its captured script
// make, in schema.
func (l *liveObjectWrites) liveTableRequest(schema string) CreateTableRequest {
	cols := []ColumnSpec{
		{Name: "id", Type: "bigint", Identity: IdentityAlways},
		{Name: "n", Type: "integer", Identity: IdentityByDefault},
		{Name: `Na"me`, Type: "text", Collation: ObjectName{Schema: "pg_catalog", Name: "C"}, NotNull: true, Default: "'o''brien'::text"},
		{Name: "twice", Type: "integer", Generated: GeneratedStored, GenerationExpression: "n * 2"},
		{Name: "mood", Type: pgsql.QualifiedName(liveObjSchema, "mood")},
		{Name: "during", Type: "tsrange"},
		{Name: "parent", Type: "bigint"},
	}
	if !before(l.num, PostgreSQL18) {
		cols = append(cols, ColumnSpec{Name: "v", Type: "integer", Generated: GeneratedVirtual, GenerationExpression: "n + 1"})
	}
	cons := []ConstraintSpec{
		{Name: "pk", Kind: ConstraintPrimaryKey, Columns: []string{"id"}, Include: []string{`Na"me`}},
		{Kind: ConstraintUnique, Columns: []string{"n", "mood"}, NullsNotDistinct: true, Deferrable: true, InitiallyDeferred: true},
		{Name: "o'k", Kind: ConstraintCheck, Check: "n > 0", NoInherit: true},
		{Name: "fk", Kind: ConstraintForeignKey, Columns: []string{"parent"}, ReferencedTable: ObjectName{Schema: "other", Name: "parent"},
			ReferencedColumns: []string{"id"}, Match: ForeignKeyMatchFull, OnDelete: ForeignKeyCascade, OnUpdate: ForeignKeySetNull},
		{Name: "ex", Kind: ConstraintExclusion, Exclude: "USING gist (during WITH &&)"},
	}
	if !before(l.num, PostgreSQL18) {
		cons = append(cons, ConstraintSpec{Name: "lax", Kind: ConstraintCheck, Check: "twice < 100", NotEnforced: true})
	}
	return CreateTableRequest{Schema: schema, Name: "T'ab", Unlogged: true, Columns: cols, Constraints: cons,
		Options: []ParameterSetting{{Name: "fillfactor", Value: "70"}, {Name: "toast.autovacuum_enabled", Value: "off"}}}
}

// shape is what of a table's columns and constraints does not depend on
// which schema it is in or its OIDs.
func (l *liveObjectWrites) shape(tb *Table) string {
	l.t.Helper()
	cols, err := tb.Columns(l.ctx)
	l.ok("columns", err)
	cons, err := tb.Constraints(l.ctx)
	l.ok("constraints", err)
	var b strings.Builder
	for _, c := range cols {
		fmt.Fprintf(&b, "%s %s nn=%v def=%q id=%q gen=%q %q coll=%v\n", c.Name, c.Type, c.NotNull, c.Default, c.Identity,
			c.Generated, c.GenerationExpression, c.Collation)
	}
	for _, c := range cons {
		fmt.Fprintf(&b, "%s %s %q def=%v/%v valid=%v enf=%v noinh=%v\n", c.Name, c.Kind, c.Definition, c.Deferrable,
			c.InitiallyDeferred, c.Validated, c.Enforced, c.NoInherit)
	}
	return strings.ReplaceAll(b.String(), tb.Name+"_", "T_")
}

func (l *liveObjectWrites) tables() {
	t, d, ctx := l.t, l.d, l.ctx
	l.exec("CREATE EXTENSION IF NOT EXISTS btree_gist SCHEMA other")
	parent, err := d.CreateTable(ctx, CreateTableRequest{Schema: "other", Name: "parent",
		Columns: []ColumnSpec{{Name: "id", Type: "bigint", NotNull: true}}, Constraints: []ConstraintSpec{{Kind: ConstraintPrimaryKey, Columns: []string{"id"}}}})
	l.ok("create parent", err)
	if parent.OID == 0 {
		t.Fatalf("parent not read back: %+v", parent)
	}

	req := l.liveTableRequest(liveObjSchema)
	tb, err := d.CreateTable(ctx, req)
	l.ok("create table", err)
	if tb.Persistence != PersistenceUnlogged || tb.Kind != RelationTable ||
		!reflect.DeepEqual(tb.Options, []ParameterSetting{{Name: "fillfactor", Value: "70"}, {Name: "toast.autovacuum_enabled", Value: "off"}}) {
		t.Errorf("table read back %+v", tb)
	}
	cols, err := tb.Columns(ctx)
	l.ok("columns", err)
	byName := map[string]*Column{}
	for _, c := range cols {
		byName[c.Name] = c
	}
	if c := byName["id"]; c == nil || c.Identity != IdentityAlways || !c.NotNull {
		t.Errorf("id read back %+v", c)
	}
	if c := byName[`Na"me`]; c == nil || c.Default != "'o''brien'::text" || !c.NotNull || c.Collation != (ObjectName{Schema: "pg_catalog", Name: "C"}) {
		t.Errorf("Na\"me read back %+v", c)
	}
	if c := byName["twice"]; c == nil || c.Generated != GeneratedStored || c.GenerationExpression == "" {
		t.Errorf("twice read back %+v", c)
	}
	if c := byName["mood"]; c == nil || c.Type != pgsql.QualifiedName(liveObjSchema, "mood") {
		t.Errorf("mood read back %+v", c)
	}
	if c := byName["v"]; !before(l.num, PostgreSQL18) && (c == nil || c.Generated != GeneratedVirtual) {
		t.Errorf("v read back %+v", c)
	}
	cons, err := tb.Constraints(ctx)
	l.ok("constraints", err)
	kinds := map[ConstraintKind]int{}
	for _, c := range cons {
		kinds[c.Kind]++
		switch {
		case c.Name == "pk" && !strings.Contains(c.Definition, `INCLUDE ("Na""me")`):
			t.Errorf("pk read back %+v", c)
		case c.Kind == ConstraintUnique && (!c.Deferrable || !c.InitiallyDeferred || !strings.Contains(c.Definition, "NULLS NOT DISTINCT")):
			t.Errorf("unique read back %+v", c)
		case c.Name == "o'k" && !c.NoInherit:
			t.Errorf("check read back %+v", c)
		case c.Name == "fk" && (c.OnDelete != ForeignKeyCascade || c.OnUpdate != ForeignKeySetNull || c.Match != ForeignKeyMatchFull ||
			c.ReferencedTable != (ObjectName{Schema: "other", Name: "parent"})):
			t.Errorf("fk read back %+v", c)
		case c.Name == "lax" && c.Enforced:
			t.Errorf("NOT ENFORCED check read back enforced: %+v", c)
		}
	}
	if kinds[ConstraintPrimaryKey] != 1 || kinds[ConstraintUnique] != 1 || kinds[ConstraintForeignKey] != 1 || kinds[ConstraintExclusion] != 1 {
		t.Errorf("constraint kinds %v", kinds)
	}

	// The captured statement, run by hand, makes the same table.
	_, err = d.CreateSchema(ctx, CreateSchemaRequest{Name: "copy"})
	l.ok("create schema copy", err)
	stmt := l.scripted(func(ctx context.Context) error { _, err := d.CreateTable(ctx, l.liveTableRequest("copy")); return err })
	l.exec(stmt)
	copied, err := d.TableByName(ctx, "copy", "T'ab")
	l.ok("read the copy", err)
	if a, b := l.shape(tb), l.shape(copied); a != b {
		t.Errorf("the script made another table:\n%s\nvs\n%s", a, b)
	}

	// Columns
	l.ok("add column", tb.AddColumn(ctx, ColumnSpec{Name: "Extra", Type: "integer", Default: "7", NotNull: true}))
	l.ok("alter column", tb.AlterColumn(ctx, "Extra", ColumnChanges{Type: "text", Collation: ObjectName{Schema: "pg_catalog", Name: "C"},
		Using: `"Extra"::text || 'x'`, Default: new("'d'::text"), NotNull: new(false), StatisticsTarget: new(500), Storage: StorageExternal}))
	l.ok("column comment", tb.SetColumnComment(ctx, "Extra", "e'x"))
	cols, err = tb.Columns(ctx)
	l.ok("columns", err)
	extra := cols[slices.IndexFunc(cols, func(c *Column) bool { return c.Name == "Extra" })]
	if extra.Type != "text" || extra.NotNull || extra.Default != "'d'::text" || extra.StatisticsTarget != 500 ||
		extra.Storage != StorageExternal || extra.Comment != "e'x" || extra.Collation != (ObjectName{Schema: "pg_catalog", Name: "C"}) {
		t.Errorf("Extra after the alters %+v", extra)
	}
	l.ok("statistics default", tb.AlterColumn(ctx, "Extra", ColumnChanges{StatisticsTarget: new(-1), Default: new("")}))
	l.ok("rename column", tb.RenameColumn(ctx, "Extra", "Extra 2"))
	cols, err = tb.Columns(ctx)
	l.ok("columns", err)
	if i := slices.IndexFunc(cols, func(c *Column) bool { return c.Name == "Extra 2" }); i < 0 || cols[i].StatisticsTarget != -1 || cols[i].Default != "" {
		t.Errorf("Extra 2 after the reset: %+v", cols)
	}
	l.ok("drop column", tb.DropColumn(ctx, "Extra 2", DropOptions{}))

	// Constraints
	l.exec(`INSERT INTO ` + tb.QualifiedName() + ` (n, "Na""me") VALUES (1, 'a')`)
	l.ok("add check not valid", tb.AddConstraint(ctx, ConstraintSpec{Name: "pos", Kind: ConstraintCheck, Check: "n > 5", NotValid: true}))
	valid := func(name string) bool {
		cons, err := tb.Constraints(ctx)
		l.ok("constraints", err)
		i := slices.IndexFunc(cons, func(c *Constraint) bool { return c.Name == name })
		if i < 0 {
			t.Fatalf("no constraint %q", name)
		}
		return cons[i].Validated
	}
	if valid("pos") {
		t.Errorf("NOT VALID check read validated")
	}
	l.wantCode("validate against a bad row", tb.ValidateConstraint(ctx, "pos"), "23514")
	l.exec(`DELETE FROM ` + tb.QualifiedName())
	l.ok("validate", tb.ValidateConstraint(ctx, "pos"))
	if !valid("pos") {
		t.Errorf("validated check read not valid")
	}
	l.ok("rename constraint", tb.RenameConstraint(ctx, "pos", "Pos'"))
	l.ok("constraint comment", tb.SetConstraintComment(ctx, "Pos'", "p"))
	cons, err = tb.Constraints(ctx)
	l.ok("constraints", err)
	if i := slices.IndexFunc(cons, func(c *Constraint) bool { return c.Name == "Pos'" }); i < 0 || cons[i].Comment != "p" {
		t.Errorf("renamed, commented constraint: %+v", cons)
	}
	l.ok("drop constraint", tb.DropConstraint(ctx, "Pos'", DropOptions{}))
	l.wantCode("drop the parent's key", parent.DropConstraint(ctx, "parent_pkey", DropOptions{}), "2BP01")

	// Partitions and inheritance
	pt, err := d.CreateTable(ctx, CreateTableRequest{Schema: liveObjSchema, Name: "events",
		Columns: []ColumnSpec{{Name: "at", Type: "date", NotNull: true}, {Name: "region", Type: "text"}}, PartitionBy: "RANGE (at)"})
	l.ok("create partitioned", err)
	p1, err := d.CreateTable(ctx, CreateTableRequest{Schema: liveObjSchema, Name: "events 2026", PartitionOf: ObjectName{Schema: liveObjSchema, Name: "events"},
		PartitionBound: "FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')", PartitionBy: "LIST (region)",
		Constraints: []ConstraintSpec{{Kind: ConstraintCheck, Check: "at <> '2026-06-01'"}}})
	l.ok("create partition", err)
	p2, err := d.CreateTable(ctx, CreateTableRequest{Schema: liveObjSchema, Name: "events other", PartitionOf: ObjectName{Schema: liveObjSchema, Name: "events"},
		PartitionBound: "DEFAULT"})
	l.ok("create default partition", err)
	child, err := d.CreateTable(ctx, CreateTableRequest{Schema: "other", Name: "child", Inherits: []ObjectName{{Schema: "other", Name: "parent"}},
		Columns: []ColumnSpec{{Name: "x", Type: "integer"}}})
	l.ok("create inheriting", err)
	if pt.PartitionKey != "RANGE (at)" || !p1.IsPartition || p1.PartitionBound != "FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')" ||
		p1.PartitionKey != "LIST (region)" || !p2.IsDefaultPartition() ||
		!reflect.DeepEqual(child.Parents, []ObjectName{{Schema: "other", Name: "parent"}}) {
		t.Errorf("partitions and inheritance read back: %+v %+v %+v %+v", pt, p1, p2, child)
	}

	// The table's own alters, each read back equal to the mirror.
	l.ok("table comment", tb.SetComment(ctx, "a\\b 'c'"))
	l.ok("table owner", tb.SetOwner(ctx, l.own))
	l.ok("table rename", tb.Rename(ctx, "Tab 2"))
	l.ok("table move", child.SetSchema(ctx, liveObjSchema))
	got, err := d.TableByName(ctx, liveObjSchema, "Tab 2")
	l.ok("read the renamed table", err)
	if got.Owner != tb.Owner || got.Comment != tb.Comment || got.Name != tb.Name {
		t.Errorf("table read back %+v, mirrored %+v", got, tb)
	}
	if _, err := d.TableByName(ctx, liveObjSchema, "child"); err != nil {
		t.Errorf("moved table: %v", err)
	}
	l.ok("uncomment", tb.SetComment(ctx, ""))
}

func (l *liveObjectWrites) indexes() {
	t, d, ctx := l.t, l.d, l.ctx
	tb := d.TableRef(liveObjSchema, "Tab 2")
	l.ok("create index", tb.CreateIndex(ctx, CreateIndexRequest{Name: "Ix'1", Unique: true, NullsNotDistinct: true,
		Columns: []IndexColumn{
			{Column: `Na"me`, OperatorClass: ObjectName{Schema: "pg_catalog", Name: "text_pattern_ops"}, Descending: true, NullsFirst: new(false)},
			{Expression: `lower(n::text)`, Collation: ObjectName{Schema: "pg_catalog", Name: "C"}},
		},
		Include: []string{"n"}, Options: []ParameterSetting{{Name: "fillfactor", Value: "80"}}, Predicate: "n > 0"}))
	l.ok("create index concurrently", tb.CreateIndex(ctx, CreateIndexRequest{Columns: []IndexColumn{{Column: "n"}}, Concurrently: true, Method: "hash"}))
	loaded, err := d.TableByName(ctx, liveObjSchema, "Tab 2")
	l.ok("read table", err)
	idx, err := loaded.Indexes(ctx)
	l.ok("indexes", err)
	i := slices.IndexFunc(idx, func(x *Index) bool { return x.Name == "Ix'1" })
	if i < 0 {
		t.Fatalf("no index Ix'1: %+v", idx)
	}
	ix := idx[i]
	for _, part := range []string{"UNIQUE", "text_pattern_ops DESC NULLS LAST", `lower((n)::text) COLLATE "C"`, "INCLUDE (n)", "NULLS NOT DISTINCT",
		"fillfactor='80'", "WHERE (n > 0)"} {
		if !strings.Contains(ix.Definition, part) {
			t.Errorf("index definition %q lacks %q", ix.Definition, part)
		}
	}
	var hashName string
	for _, x := range idx {
		if x.Method == "hash" {
			hashName = x.Name
			if !x.Valid {
				t.Errorf("concurrent index invalid: %+v", x)
			}
		}
	}
	l.ok("rename index", tb.RenameIndex(ctx, "Ix'1", "Ix 2"))
	l.ok("index comment", tb.SetIndexComment(ctx, "Ix 2", "i"))
	idx, err = loaded.Indexes(ctx)
	l.ok("indexes", err)
	if i := slices.IndexFunc(idx, func(x *Index) bool { return x.Name == "Ix 2" }); i < 0 || idx[i].Comment != "i" {
		t.Errorf("renamed, commented index: %+v", idx)
	}
	l.wantCode("drop the key's index", tb.DropIndex(ctx, "pk", DropIndexOptions{}), "2BP01")
	l.ok("drop index concurrently", tb.DropIndex(ctx, hashName, DropIndexOptions{Concurrently: true}))
	l.ok("drop index", tb.DropIndex(ctx, "Ix 2", DropIndexOptions{}))
	idx, err = loaded.Indexes(ctx)
	l.ok("indexes", err)
	if slices.ContainsFunc(idx, func(x *Index) bool { return x.Name == "Ix 2" || x.Name == hashName }) {
		t.Errorf("dropped indexes remain: %+v", idx)
	}
}

func (l *liveObjectWrites) views() {
	t, d, ctx := l.t, l.d, l.ctx
	src := pgsql.QualifiedName(liveObjSchema, "Tab 2")
	v, err := d.CreateView(ctx, CreateViewRequest{Schema: liveObjSchema, Name: "V'1", Query: `SELECT id, n FROM ` + src + ` WHERE n > 0`,
		Columns: []string{"Id", "n"}, CheckOption: CheckOptionCascaded, SecurityBarrier: new(true), SecurityInvoker: new(true)})
	l.ok("create view", err)
	if v.CheckOption != CheckOptionCascaded || !v.SecurityBarrier || !v.SecurityInvoker {
		t.Errorf("view read back %+v", v)
	}
	vcols, err := v.Columns(ctx)
	l.ok("view columns", err)
	if len(vcols) != 2 || vcols[0].Name != "Id" {
		t.Errorf("view columns %+v", vcols)
	}
	v2, err := d.CreateView(ctx, CreateViewRequest{Schema: liveObjSchema, Name: "V'1", OrReplace: true,
		Query: `SELECT id, n, n * 2 AS dbl FROM ` + src, Columns: []string{"Id", "n"}})
	l.ok("replace view", err)
	if v2.OID != v.OID || v2.CheckOption != CheckOptionNone {
		t.Errorf("replaced view %+v (was OID %d)", v2, v.OID)
	}
	l.ok("view comment", v.SetComment(ctx, "v"))
	l.ok("view owner", v.SetOwner(ctx, l.own))
	l.ok("view rename", v.Rename(ctx, "V 2"))
	l.ok("view move", v.SetSchema(ctx, "other"))
	if got, err := d.ViewByName(ctx, "other", "V 2"); err != nil || got.Owner != l.own || got.Comment != "v" {
		t.Errorf("view after the alters %+v, %v", got, err)
	}
	l.wantCode("drop the table under a view", d.TableRef(liveObjSchema, "Tab 2").Drop(ctx, DropOptions{}), "2BP01")

	mv, err := d.CreateMaterializedView(ctx, CreateMaterializedViewRequest{Schema: liveObjSchema, Name: "M'v", Query: "SELECT g AS id FROM generate_series(1, 3) g",
		Columns: []string{"Id"}, NoData: true, Options: []ParameterSetting{{Name: "fillfactor", Value: "50"}}})
	l.ok("create matview", err)
	if mv.Populated || !reflect.DeepEqual(mv.Options, []ParameterSetting{{Name: "fillfactor", Value: "50"}}) {
		t.Errorf("matview read back %+v", mv)
	}
	l.wantCode("refresh concurrently unpopulated", mv.Refresh(ctx, RefreshMaterializedViewOptions{Concurrently: true}), "0A000")
	l.ok("refresh", mv.Refresh(ctx, RefreshMaterializedViewOptions{}))
	l.ok("matview index", mv.CreateIndex(ctx, CreateIndexRequest{Name: "mv_id", Unique: true, Columns: []IndexColumn{{Column: "Id"}}}))
	l.ok("refresh concurrently", mv.Refresh(ctx, RefreshMaterializedViewOptions{Concurrently: true}))
	if got, err := d.MaterializedViewByName(ctx, liveObjSchema, "M'v"); err != nil || !got.Populated || !mv.Populated {
		t.Errorf("refreshed matview %+v, %v", got, err)
	}
	l.ok("matview index rename", mv.RenameIndex(ctx, "mv_id", "mv id"))
	l.ok("matview index comment", mv.SetIndexComment(ctx, "mv id", "x"))
	l.ok("refresh no data", mv.Refresh(ctx, RefreshMaterializedViewOptions{NoData: true}))
	l.ok("matview comment", mv.SetComment(ctx, "m"))
	l.ok("matview owner", mv.SetOwner(ctx, l.own))
	l.ok("matview rename", mv.Rename(ctx, "Mv 2"))
	l.ok("matview move", mv.SetSchema(ctx, "other"))
	got, err := d.MaterializedViewByName(ctx, "other", "Mv 2")
	l.ok("read matview", err)
	if got.Populated || got.Owner != l.own || got.Comment != "m" || mv.Populated {
		t.Errorf("matview after the alters %+v (mirrored %+v)", got, mv)
	}
	idx, err := got.Indexes(ctx)
	l.ok("matview indexes", err)
	if len(idx) != 1 || idx[0].Name != "mv id" || idx[0].Comment != "x" {
		t.Errorf("matview indexes %+v", idx)
	}
	l.ok("matview index drop", mv.DropIndex(ctx, "mv id", DropIndexOptions{}))
	l.ok("matview drop", mv.Drop(ctx, DropOptions{}))
}

func (l *liveObjectWrites) routines() {
	t, d, ctx := l.t, l.d, l.ctx
	f, err := d.CreateFunction(ctx, CreateFunctionRequest{Schema: liveObjSchema, Name: "F'n",
		Arguments: []RoutineArgumentSpec{
			{Name: "a", Type: "integer"},
			{Name: `B"b`, Type: "text", Default: "'o''brien'"},
			{Mode: ArgumentOut, Name: "res", Type: "text"},
		},
		Language: "plpgsql", Body: "BEGIN res := a::text || $2 || '$$'; END",
		Volatility: VolatilityStable, Strict: true, SecurityDefiner: true, Parallel: ParallelSafe, Cost: new(7.5),
		Settings: []ParameterSetting{{Name: "search_path", Value: `pg_catalog, "App S"`}, {Name: "work_mem", Value: "64MB"}}})
	l.ok("create function", err)
	if f.OID == 0 || f.Volatility != VolatilityStable || !f.Strict || !f.SecurityDefiner || f.Parallel != ParallelSafe ||
		f.Cost != 7.5 || f.Language != "plpgsql" || len(f.Arguments) != 3 || f.Arguments[1].Default != "'o''brien'::text" ||
		f.Arguments[2].Mode != ArgumentOut || len(f.Settings) != 2 || f.Settings[0].Value != `pg_catalog, "App S"` {
		t.Errorf("function read back %+v", f)
	}
	body, err := f.Body(ctx)
	l.ok("function body", err)
	if body.Source != "BEGIN res := a::text || $2 || '$$'; END" {
		t.Errorf("body %q", body.Source)
	}
	if v := l.scalar(`SELECT ` + pgsql.QualifiedName(liveObjSchema, "F'n") + `(1)`); v != "1o'brien$$" {
		t.Errorf("function returned %q", v)
	}
	// An overload: the read-back cannot tell which is new, and returns the
	// handle, whose Signature names it.
	ov, err := d.CreateFunction(ctx, CreateFunctionRequest{Schema: liveObjSchema, Name: "F'n", Arguments: []RoutineArgumentSpec{{Type: "date"}},
		Returns: "date", Language: "sql", Body: "SELECT $1 + 1"})
	l.ok("create overload", err)
	if ov.OID != 0 || ov.Signature() != pgsql.QualifiedName(liveObjSchema, "F'n")+"(date)" {
		t.Errorf("overload handle %+v", ov)
	}
	l.ok("overload drop", ov.Drop(ctx, DropOptions{}))
	tf, err := d.CreateFunction(ctx, CreateFunctionRequest{Schema: liveObjSchema, Name: "rows", OrReplace: true,
		Arguments: []RoutineArgumentSpec{{Name: "n", Type: "integer"}, {Mode: ArgumentTable, Name: "i", Type: "integer"}, {Mode: ArgumentTable, Name: "Sq", Type: "bigint"}},
		Language:  "sql", Body: "SELECT g, g::bigint * g FROM generate_series(1, n) g", Rows: new(10.0)})
	l.ok("create table function", err)
	if !tf.ReturnsSet || tf.Rows != 10 || !strings.HasPrefix(tf.Result, "TABLE(") {
		t.Errorf("table function read back %+v", tf)
	}
	if v := l.scalar(`SELECT sum("Sq")::text FROM ` + pgsql.QualifiedName(liveObjSchema, "rows") + `(3)`); v != "14" {
		t.Errorf("table function sum %s", v)
	}
	l.ok("function comment", f.SetComment(ctx, "f"))
	l.ok("function owner", f.SetOwner(ctx, l.own))
	l.ok("function rename", f.Rename(ctx, "Fn 2"))
	l.ok("function move", f.SetSchema(ctx, "other"))
	got, err := d.FunctionByName(ctx, "other", "Fn 2", nil)
	l.ok("read function", err)
	if got.Owner != l.own || got.Comment != "f" || got.OID != f.OID {
		t.Errorf("function after the alters %+v", got)
	}

	p, err := d.CreateProcedure(ctx, CreateProcedureRequest{Schema: liveObjSchema, Name: "P'r",
		Arguments: []RoutineArgumentSpec{{Name: "a", Type: "integer"}, {Mode: ArgumentInOut, Name: "b", Type: "integer"}},
		Language:  "plpgsql", Body: "BEGIN b := a + b; END", SecurityDefiner: true, Settings: []ParameterSetting{{Name: "lock_timeout", Value: "1s"}}})
	l.ok("create procedure", err)
	if p.OID == 0 || !p.SecurityDefiner || len(p.Arguments) != 2 || p.Arguments[1].Mode != ArgumentInOut ||
		!reflect.DeepEqual(p.Settings, []ParameterSetting{{Name: "lock_timeout", Value: "1s"}}) {
		t.Errorf("procedure read back %+v", p)
	}
	if v := l.scalar(`CALL ` + pgsql.QualifiedName(liveObjSchema, "P'r") + `(2, 3)`); v != "5" {
		t.Errorf("CALL returned %s", v)
	}
	l.ok("procedure comment", p.SetComment(ctx, "p"))
	l.ok("procedure owner", p.SetOwner(ctx, l.own))
	l.ok("procedure rename", p.Rename(ctx, "Pr 2"))
	l.ok("procedure move", p.SetSchema(ctx, "other"))
	if got, err := d.ProcedureByName(ctx, "other", "Pr 2", nil); err != nil || got.Owner != l.own || got.Comment != "p" {
		t.Errorf("procedure after the alters %+v, %v", got, err)
	}
	l.ok("procedure drop", p.Drop(ctx, DropOptions{}))
}

func (l *liveObjectWrites) triggers() {
	t, d, ctx := l.t, l.d, l.ctx
	fn, err := d.CreateFunction(ctx, CreateFunctionRequest{Schema: liveObjSchema, Name: "trg", Returns: "trigger", Language: "plpgsql",
		Body: "BEGIN RETURN NULL; END"})
	l.ok("create trigger function", err)
	fname := ObjectName{Schema: fn.Schema, Name: fn.Name}
	tr, err := d.CreateTrigger(ctx, CreateTriggerRequest{Schema: liveObjSchema, Table: "Tab 2", Name: "T'r1", Timing: TriggerAfter,
		Events: []TriggerEvent{TriggerInsert, TriggerUpdate}, UpdateColumns: []string{"n", `Na"me`}, ForEachRow: true,
		When: "NEW.n > 0", Function: fname, Arguments: []string{"o'brien", `a\b`}})
	l.ok("create trigger", err)
	if tr.OID == 0 || tr.Timing != TriggerAfter || !slices.Equal(tr.Events, []TriggerEvent{TriggerInsert, TriggerUpdate}) ||
		!slices.Equal(tr.UpdateColumns, []string{"n", `Na"me`}) || !tr.ForEachRow || !tr.HasCondition ||
		!slices.Equal(tr.Arguments, []string{"o'brien", `a\b`}) || tr.Function != fname || tr.Enabled != TriggerEnabledOrigin {
		t.Errorf("trigger read back %+v", tr)
	}
	st, err := d.CreateTrigger(ctx, CreateTriggerRequest{Schema: liveObjSchema, Table: "Tab 2", Name: "stmt", Timing: TriggerAfter,
		Events: []TriggerEvent{TriggerUpdate}, OldTable: "old rows", NewTable: "New", Function: fname})
	l.ok("create transition trigger", err)
	if st.OldTable != "old rows" || st.NewTable != "New" || st.ForEachRow {
		t.Errorf("transition trigger read back %+v", st)
	}
	ct, err := d.CreateTrigger(ctx, CreateTriggerRequest{Schema: liveObjSchema, Table: "Tab 2", Name: "ctr", Constraint: true,
		Deferrable: true, InitiallyDeferred: true, Timing: TriggerAfter, Events: []TriggerEvent{TriggerDelete}, ForEachRow: true, Function: fname})
	l.ok("create constraint trigger", err)
	if !ct.IsConstraint || !ct.Deferrable || !ct.InitiallyDeferred {
		t.Errorf("constraint trigger read back %+v", ct)
	}
	repl, err := d.CreateTrigger(ctx, CreateTriggerRequest{Schema: liveObjSchema, Table: "Tab 2", Name: "stmt", OrReplace: true,
		Timing: TriggerBefore, Events: []TriggerEvent{TriggerTruncate}, Function: fname})
	l.ok("replace trigger", err)
	if repl.Timing != TriggerBefore || !slices.Equal(repl.Events, []TriggerEvent{TriggerTruncate}) {
		t.Errorf("replaced trigger %+v", repl)
	}
	for _, e := range []TriggerEnabled{TriggerDisabled, TriggerEnabledReplica, TriggerEnabledAlways, TriggerEnabledOrigin} {
		l.ok("set enabled", tr.SetEnabled(ctx, e))
		if got, err := d.TriggerByName(ctx, liveObjSchema, "Tab 2", "T'r1"); err != nil || got.Enabled != e || tr.Enabled != e {
			t.Errorf("trigger enabled %v: read %+v, %v", e, got, err)
		}
	}
	l.ok("trigger comment", tr.SetComment(ctx, "t"))
	l.ok("trigger rename", tr.Rename(ctx, "Tr 2"))
	if got, err := d.TriggerByName(ctx, liveObjSchema, "Tab 2", "Tr 2"); err != nil || got.Comment != "t" {
		t.Errorf("trigger after the alters %+v, %v", got, err)
	}
	// Event triggers: a superuser's, as the connected role is.
	evf, err := d.CreateFunction(ctx, CreateFunctionRequest{Schema: liveObjSchema, Name: "evt", Returns: "event_trigger", Language: "plpgsql",
		Body: "BEGIN END"})
	l.ok("create event trigger function", err)
	evname := ObjectName{Schema: evf.Schema, Name: evf.Name}
	ev, err := d.CreateEventTrigger(ctx, CreateEventTriggerRequest{Name: "E't", Event: EventTriggerDDLCommandEnd,
		Tags: []string{"CREATE TABLE", "DROP FUNCTION"}, Function: evname})
	l.ok("create event trigger", err)
	if ev.OID == 0 || ev.Event != EventTriggerDDLCommandEnd || !slices.Equal(ev.Tags, []string{"CREATE TABLE", "DROP FUNCTION"}) ||
		ev.Function != evname || ev.Enabled != TriggerEnabledOrigin {
		t.Errorf("event trigger read back %+v", ev)
	}
	for _, e := range []TriggerEnabled{TriggerDisabled, TriggerEnabledReplica, TriggerEnabledAlways, TriggerEnabledOrigin} {
		l.ok("event trigger enabled", ev.SetEnabled(ctx, e))
		if got, err := d.EventTriggerByName(ctx, "E't"); err != nil || got.Enabled != e {
			t.Errorf("event trigger enabled %v: %+v, %v", e, got, err)
		}
	}
	me := l.scalar("SELECT current_user::text")
	l.ok("event trigger owner", ev.SetOwner(ctx, me))
	l.ok("event trigger comment", ev.SetComment(ctx, "e"))
	l.ok("event trigger rename", ev.Rename(ctx, "Et 2"))
	if got, err := d.EventTriggerByName(ctx, "Et 2"); err != nil || got.Comment != "e" || got.Owner != me {
		t.Errorf("event trigger after the alters %+v, %v", got, err)
	}
	l.wantCode("event trigger owned by a plain role", ev.SetOwner(ctx, l.own), "42501")
	l.ok("event trigger drop", ev.Drop(ctx, DropOptions{}))
	if !before(l.num, PostgreSQL17) {
		login, err := d.CreateEventTrigger(ctx, CreateEventTriggerRequest{Name: "on login", Event: EventTriggerLogin, Function: evname})
		l.ok("create login event trigger", err)
		if login.Event != EventTriggerLogin {
			t.Errorf("login event trigger read back %+v", login)
		}
		l.ok("login event trigger drop", login.Drop(ctx, DropOptions{}))
	}
	l.ok("event trigger function drop", evf.Drop(ctx, DropOptions{}))

	l.wantCode("drop the function a trigger calls", fn.Drop(ctx, DropOptions{}), "2BP01")
	l.ok("trigger drop", tr.Drop(ctx, DropOptions{}))
	l.ok("function drop cascade", fn.Drop(ctx, DropOptions{Cascade: true}))
	if _, err := d.TriggerByName(ctx, liveObjSchema, "Tab 2", "stmt"); !errors.Is(err, ErrNotFound) {
		t.Errorf("trigger after its function's CASCADE: %v", err)
	}
}

func (l *liveObjectWrites) extensions() {
	t, d, ctx := l.t, l.d, l.ctx
	e, err := d.CreateExtension(ctx, CreateExtensionRequest{Name: "citext", Schema: liveObjSchema, Version: "1.5"})
	l.ok("create extension", err)
	if e.Version != "1.5" || e.Schema != liveObjSchema {
		t.Errorf("extension read back %+v", e)
	}
	l.ok("update extension", e.Update(ctx, ""))
	got, err := d.ExtensionByName(ctx, "citext")
	l.ok("read extension", err)
	if got.Version != got.DefaultVersion || got.Version == "1.5" {
		t.Errorf("updated extension %+v", got)
	}
	l.ok("extension move", e.SetSchema(ctx, "other"))
	l.ok("extension comment", e.SetComment(ctx, "x"))
	if got, err := d.ExtensionByName(ctx, "citext"); err != nil || got.Schema != "other" || got.Comment != "x" {
		t.Errorf("extension after the alters %+v, %v", got, err)
	}
	l.exec(`CREATE TABLE other.uses_citext (c other.citext)`)
	l.wantCode("drop an extension in use", e.Drop(ctx, DropOptions{}), "2BP01")
	l.ok("drop extension cascade", e.Drop(ctx, DropOptions{Cascade: true}))
	if _, err := d.ExtensionByName(ctx, "citext"); !errors.Is(err, ErrNotFound) {
		t.Errorf("dropped extension: %v", err)
	}
}

// comments covers the cluster-wide writes: an in-place tablespace (a
// Server with allow_in_place_tablespaces on), and the comments on the
// database and a role.
func (l *liveObjectWrites) comments() {
	t, ctx := l.t, l.ctx
	ts := fmt.Sprintf("pgmo_live_objw_%d T's", l.stamp)
	s, err := Connect(ctx, ConnectionOptions{ConnString: *liveConnString, Database: "postgres",
		ApplicationName: "pgmo - live object writes (in place)", Params: map[string]string{"allow_in_place_tablespaces": "on"}})
	l.ok("connect in place", err)
	t.Cleanup(func() {
		_, _ = s.exec(context.Background(), "DROP TABLESPACE IF EXISTS "+pgsql.QuoteIdent(ts))
		_, _ = s.exec(context.Background(), "DROP TABLESPACE IF EXISTS "+pgsql.QuoteIdent(ts+" 2"))
		s.Close()
	})
	tsp, err := s.CreateTablespace(ctx, CreateTablespaceRequest{Name: ts, Owner: l.own,
		Options: []ParameterSetting{{Name: "seq_page_cost", Value: "2"}, {Name: "random_page_cost", Value: "1.5"}}})
	l.ok("create tablespace", err)
	if tsp.Owner != l.own || !reflect.DeepEqual(tsp.Options, []ParameterSetting{{Name: "seq_page_cost", Value: "2"}, {Name: "random_page_cost", Value: "1.5"}}) {
		t.Errorf("tablespace read back %+v", tsp)
	}
	l.ok("tablespace options", tsp.SetOptions(ctx, ParameterSetting{Name: "effective_io_concurrency", Value: "8"}))
	l.ok("tablespace reset", tsp.ResetOptions(ctx, "seq_page_cost"))
	l.ok("tablespace comment", tsp.SetComment(ctx, "t's"))
	l.ok("tablespace owner", tsp.SetOwner(ctx, l.own2))
	l.ok("tablespace rename", tsp.Rename(ctx, ts+" 2"))
	got, err := s.TablespaceByName(ctx, ts+" 2")
	l.ok("read tablespace", err)
	if got.Owner != l.own2 || got.Comment != "t's" ||
		!reflect.DeepEqual(got.Options, []ParameterSetting{{Name: "random_page_cost", Value: "1.5"}, {Name: "effective_io_concurrency", Value: "8"}}) {
		t.Errorf("tablespace after the alters %+v", got)
	}
	l.exec(`CREATE TABLE other.in_ts (a int) TABLESPACE ` + pgsql.QuoteIdent(ts+" 2"))
	l.wantCode("drop a tablespace in use", tsp.Drop(ctx), "55000")
	l.exec(`DROP TABLE other.in_ts`)
	l.ok("drop tablespace", tsp.Drop(ctx))

	l.ok("database comment", l.d.SetComment(ctx, "d'b"))
	if got, err := l.s.DatabaseByName(ctx, l.db); err != nil || got.Comment != "d'b" || l.d.Comment != "d'b" {
		t.Errorf("database comment: %+v, %v", got, err)
	}
	l.ok("database uncomment", l.d.SetComment(ctx, ""))
	r := l.s.RoleRef(l.own2)
	l.ok("role comment", r.SetComment(ctx, `a\b`))
	if got, err := l.s.RoleByName(ctx, l.own2); err != nil || got.Comment != `a\b` {
		t.Errorf("role comment: %+v, %v", got, err)
	}
}

// transaction: writes in the throwaway database, on a Server connected to
// it, inside InTransaction — rolled back, they leave nothing; a CONCURRENTLY
// index is refused before anything is sent.
func (l *liveObjectWrites) transaction() {
	t, ctx := l.t, l.ctx
	s, err := Connect(ctx, ConnectionOptions{ConnString: *liveConnString, Database: l.db, ApplicationName: "pgmo - live object writes (tx)"})
	l.ok("connect to the database", err)
	defer s.Close()
	d := s.DatabaseRef(l.db)
	boom := errors.New("roll back")
	err = s.InTransaction(ctx, func(ctx context.Context) error {
		if _, err := d.CreateSchema(ctx, CreateSchemaRequest{Name: "tx"}); err != nil {
			return err
		}
		tb, err := d.CreateTable(ctx, CreateTableRequest{Schema: "tx", Name: "t", Columns: []ColumnSpec{{Name: "a", Type: "int"}}})
		if err != nil {
			return err
		}
		if tb.OID == 0 {
			return fmt.Errorf("read back inside the transaction found nothing")
		}
		if err := tb.CreateIndex(ctx, CreateIndexRequest{Columns: []IndexColumn{{Column: "a"}}, Concurrently: true}); !errors.Is(err, ErrUnsupported) {
			return fmt.Errorf("concurrent index in a transaction: %v", err)
		}
		if err := tb.CreateIndex(ctx, CreateIndexRequest{Columns: []IndexColumn{{Column: "a"}}}); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("InTransaction: %v", err)
	}
	if _, err := d.SchemaByName(ctx, "tx"); !errors.Is(err, ErrNotFound) {
		t.Errorf("rolled-back schema: %v", err)
	}
}

// drops: a schema with objects in it is refused without CASCADE and dropped
// with it.
func (l *liveObjectWrites) drops() {
	sc := l.d.SchemaRef(liveObjSchema)
	l.wantCode("drop a schema that is not empty", sc.Drop(l.ctx, DropOptions{}), "2BP01")
	l.ok("drop schema cascade", sc.Drop(l.ctx, DropOptions{Cascade: true}))
	if _, err := l.d.SchemaByName(l.ctx, liveObjSchema); !errors.Is(err, ErrNotFound) {
		l.t.Errorf("dropped schema: %v", err)
	}
	l.ok("drop table cascade", l.d.TableRef("other", "parent").Drop(l.ctx, DropOptions{Cascade: true}))
}
