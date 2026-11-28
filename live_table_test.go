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

// TestLiveTables reads the tables of a throwaway database the listing, the
// by-name read and each read of a table's parts must describe in full:
//
//   - app.parent: an identity key, a citext (an extension's type) unique
//     column, a default, a stored generated column (and a virtual one on
//     18), an array of an enum, a COLLATE "C" column with EXTERNAL storage,
//     a statistics target, a column grant and a column comment, a sequence
//     default, a domain; a NOT VALID check; SELECT granted to PUBLIC; rows,
//     ANALYZEd;
//   - app.child: a primary key with INCLUDE, a deferrable foreign key with
//     actions and MATCH FULL, an exclusion constraint, storage parameters on
//     it and on its TOAST table, a partial expression index with INCLUDE and
//     a fillfactor, an ENABLE REPLICA trigger on UPDATE OF a column with
//     WHEN, a deferrable constraint trigger, forced row-level security with
//     a restrictive and a permissive policy;
//   - app.part, partitioned by range, with a bounded and a DEFAULT
//     partition, a partitioned index and a trigger the partitions inherit;
//   - app.kid, inheriting from app.base and "Other".base2 (a check inherited);
//   - app.scratch, unlogged, adopted by citext (an extension member);
//   - app."Parent", differing from app.parent only by case;
//   - a temporary table, in the session's pg_temp_N schema.
//
// The listing is read again as a plain role, with each table's parts:
// pg_class and the catalogs under it are readable by all, so it must be
// identical.
func TestLiveTables(t *testing.T) {
	admin, ctx, done := liveDBTimeout(t, 2*time.Minute)
	t.Cleanup(done)
	stamp := time.Now().UnixNano()
	name := func(s string) string { return fmt.Sprintf("pgmo_live_table_%d_%s", stamp, s) }
	b, plain := name("B b"), name("plain")
	db := name("db")
	q := pgsql.QuoteIdent

	var connected string
	if err := admin.QueryRow(ctx, "SELECT current_user::text").Scan(&connected); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+q(db)+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database: %v", err)
		}
		for _, r := range []string{b, plain} {
			if _, err := admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+q(r)); err != nil {
				t.Errorf("drop role %s: %v", r, err)
			}
		}
	})
	for _, stmt := range []string{"CREATE DATABASE " + q(db), "CREATE ROLE " + q(b), "CREATE ROLE " + q(plain)} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	cfg, err := pgx.ParseConfig(*liveConnString)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = db
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	var num int
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::int").Scan(&num); err != nil {
		t.Fatal(err)
	}
	pg18 := num >= int(PostgreSQL18)
	fixture := []string{
		"CREATE SCHEMA app",
		"CREATE EXTENSION citext SCHEMA app",
		"CREATE TYPE app.mood AS ENUM ('ok', 'no')",
		"CREATE DOMAIN app.pos AS integer CHECK (VALUE > 0)",
		"CREATE SEQUENCE app.s",
		`CREATE TABLE app.parent (id integer GENERATED ALWAYS AS IDENTITY PRIMARY KEY, name app.citext NOT NULL UNIQUE,
			amount numeric(18,2) DEFAULT 0, doubled numeric GENERATED ALWAYS AS (amount * 2) STORED, m app.mood[],
			label text COLLATE "C", n integer DEFAULT nextval('app.s'), p app.pos)`,
		"ALTER TABLE app.parent ALTER COLUMN label SET STORAGE EXTERNAL, ALTER COLUMN label SET STATISTICS 500",
		"ALTER TABLE app.parent ADD CONSTRAINT amount_pos CHECK (amount >= 0) NOT VALID",
		"GRANT SELECT ON app.parent TO PUBLIC",
		"GRANT UPDATE (label) ON app.parent TO " + q(b),
		"COMMENT ON TABLE app.parent IS 'the parent'",
		"COMMENT ON COLUMN app.parent.name IS 'its name'",
		"INSERT INTO app.parent (name, amount) SELECT 'n' || g, g FROM generate_series(1, 50) g",
		"ANALYZE app.parent",
		`CREATE TABLE app.child (id integer, pid integer, a integer, b integer, note text,
			CONSTRAINT child_pk PRIMARY KEY (id) INCLUDE (a),
			CONSTRAINT child_fk FOREIGN KEY (pid) REFERENCES app.parent MATCH FULL ON DELETE SET NULL ON UPDATE CASCADE
				DEFERRABLE INITIALLY DEFERRED,
			CONSTRAINT ex EXCLUDE USING btree (a WITH =)) WITH (fillfactor = 70, toast.autovacuum_enabled = false)`,
		"CREATE INDEX child_expr ON app.child (lower(b::text) DESC, a) INCLUDE (b) WITH (fillfactor = 80) WHERE a > 0",
		"COMMENT ON INDEX app.child_expr IS 'partial'",
		"CREATE FUNCTION app.touch() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'",
		"CREATE TRIGGER t1 BEFORE INSERT OR UPDATE OF a, b ON app.child FOR EACH ROW WHEN (NEW.a > 0) EXECUTE FUNCTION app.touch()",
		"ALTER TABLE app.child ENABLE REPLICA TRIGGER t1",
		"COMMENT ON TRIGGER t1 ON app.child IS 'touch'",
		"CREATE CONSTRAINT TRIGGER t2 AFTER DELETE ON app.child DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION app.touch()",
		"ALTER TABLE app.child ENABLE ROW LEVEL SECURITY, FORCE ROW LEVEL SECURITY",
		"CREATE POLICY p1 ON app.child AS RESTRICTIVE FOR UPDATE TO " + q(b) + ", " + q(plain) + " USING (a > 0) WITH CHECK (b > 0)",
		"CREATE POLICY p2 ON app.child USING (true)",
		"COMMENT ON POLICY p2 ON app.child IS 'all'",
		"CREATE TABLE app.part (id integer, at date NOT NULL) PARTITION BY RANGE (at)",
		"CREATE TABLE app.part_2026 PARTITION OF app.part FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')",
		"CREATE TABLE app.part_def PARTITION OF app.part DEFAULT",
		"CREATE INDEX part_at ON app.part (at)",
		"CREATE TRIGGER pt AFTER INSERT ON app.part FOR EACH ROW EXECUTE FUNCTION app.touch()",
		"CREATE TABLE app.base (x integer CHECK (x > 0))",
		`CREATE SCHEMA "Other"`,
		`CREATE TABLE "Other".base2 (z integer)`,
		`CREATE TABLE app.kid (y integer) INHERITS (app.base, "Other".base2)`,
		"CREATE UNLOGGED TABLE app.scratch (id integer)",
		"ALTER EXTENSION citext ADD TABLE app.scratch",
		`CREATE TABLE app."Parent" (id integer)`,
		"CREATE TEMP TABLE tmp (id integer)",
	}
	if pg18 {
		fixture = append(fixture,
			"ALTER TABLE app.parent ADD COLUMN tripled numeric GENERATED ALWAYS AS (amount * 3) VIRTUAL",
			"ALTER TABLE app.base ADD CONSTRAINT loose CHECK (x < 100) NOT ENFORCED")
	}
	for _, stmt := range fixture {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	s, err := Connect(ctx, ConnectionOptions{ConnString: *liveConnString, Database: db, ApplicationName: "pgmo - live tables"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	d, err := s.DatabaseByName(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	// The user tables no extension owns, by schema then byte order.
	no, yes := false, true
	users, err := d.Tables(ctx, ObjectFilter{System: &no, ExtensionMember: &no})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, tb := range users {
		names = append(names, tb.Name)
	}
	if want := []string{"base2", "Parent", "base", "child", "kid", "parent", "part", "part_2026", "part_def"}; !slices.Equal(names, want) {
		t.Fatalf("Tables(user) = %q, want %q", names, want)
	}
	byName := func(n string) *Table {
		t.Helper()
		tb, err := d.TableByName(ctx, "app", n)
		if err != nil {
			t.Fatal(err)
		}
		if i := slices.IndexFunc(users, func(u *Table) bool { return u.Name == n }); i >= 0 && !reflect.DeepEqual(users[i], tb) {
			t.Errorf("listing's %s =\n%+v\nby name\n%+v", n, users[i], tb)
		}
		return tb
	}
	grant := func(grantee, grantor string, p Privilege) PrivilegeGrant {
		return PrivilegeGrant{Grantee: grantee, Grantor: grantor, Privilege: p}
	}

	// parent: the table itself.
	parent := byName("parent")
	ownerACL := []PrivilegeGrant{}
	for _, p := range []Privilege{PrivilegeInsert, PrivilegeSelect, PrivilegeUpdate, PrivilegeDelete,
		PrivilegeTruncate, PrivilegeReferences, PrivilegeTrigger} {
		ownerACL = append(ownerACL, grant(connected, connected, p))
	}
	if num >= int(PostgreSQL17) {
		ownerACL = append(ownerACL, grant(connected, connected, PrivilegeMaintain))
	}
	got := *parent
	got.db, got.OID = nil, 0
	want := Table{Schema: "app", Name: "parent", Owner: connected, Kind: RelationTable, Persistence: PersistencePermanent,
		RowEstimate: 50, Privileges: append(slices.Clone(ownerACL), grant("", connected, PrivilegeSelect)), Comment: "the parent"}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("TableByName(parent) =\n%+v\nwant\n%+v", got, want)
	}
	if parent.Database() != d || parent.QualifiedName() != "app.parent" || parent.IsPartitioned() || parent.IsDefaultPartition() {
		t.Errorf("parent: Database %p, QualifiedName %s, IsPartitioned %v, IsDefaultPartition %v",
			parent.Database(), parent.QualifiedName(), parent.IsPartitioned(), parent.IsDefaultPartition())
	}
	// Exact: a case the catalog does not hold is not found; the case-only
	// twin is a table of its own.
	if _, err := d.TableByName(ctx, "app", "PARENT"); !errors.Is(err, ErrNotFound) {
		t.Errorf("TableByName(PARENT): err = %v, want ErrNotFound", err)
	}
	if tw := byName("Parent"); tw.OID == parent.OID || !tw.PrivilegesDefault || !reflect.DeepEqual(tw.Privileges, ownerACL) {
		t.Errorf(`"Parent" = %+v; want a table of its own with the owner's default ACL`, tw)
	}
	if _, err := d.TableByName(ctx, "", "parent"); !errors.Is(err, ErrSchemaRequired) {
		t.Errorf("TableByName without a schema: err = %v, want ErrSchemaRequired", err)
	}
	if _, err := d.TableByName(ctx, "app", "s"); !errors.Is(err, ErrNotFound) {
		t.Errorf("TableByName(a sequence): err = %v, want ErrNotFound", err)
	}

	// parent's columns.
	cols, err := parent.Columns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, c := range cols {
		c.TypeOID = 0
	}
	col := func(num int, name, typ string) Column {
		return Column{Number: num, Name: name, Type: typ, StatisticsTarget: -1, IsLocal: true}
	}
	wantCols := []Column{
		col(1, "id", "integer"), col(2, "name", "app.citext"), col(3, "amount", "numeric(18,2)"),
		col(4, "doubled", "numeric"), col(5, "m", "app.mood[]"), col(6, "label", "text"),
		col(7, "n", "integer"), col(8, "p", "app.pos"),
	}
	wantCols[0].NotNull, wantCols[0].Identity, wantCols[0].Storage = true, IdentityAlways, StoragePlain
	wantCols[1].NotNull, wantCols[1].Storage, wantCols[1].Comment = true, StorageExtended, "its name"
	wantCols[2].Default, wantCols[2].Storage = "0", StorageMain
	wantCols[3].Generated, wantCols[3].GenerationExpression, wantCols[3].Storage = GeneratedStored, "(amount * (2)::numeric)", StorageMain
	wantCols[4].Storage = StorageExtended
	wantCols[5].Collation, wantCols[5].Storage, wantCols[5].StatisticsTarget = ObjectName{"pg_catalog", "C"}, StorageExternal, 500
	wantCols[5].Privileges = []PrivilegeGrant{grant(b, connected, PrivilegeUpdate)}
	wantCols[6].Default, wantCols[6].Storage = "nextval('app.s'::regclass)", StoragePlain
	wantCols[7].Storage = StoragePlain
	if pg18 {
		v := col(9, "tripled", "numeric")
		v.Generated, v.GenerationExpression, v.Storage = GeneratedVirtual, "(amount * (3)::numeric)", StorageMain
		wantCols = append(wantCols, v)
	}
	if len(cols) != len(wantCols) {
		t.Fatalf("Columns(parent): %d, want %d", len(cols), len(wantCols))
	}
	for i := range cols {
		if !reflect.DeepEqual(*cols[i], wantCols[i]) {
			t.Errorf("Columns(parent)[%d] =\n%+v\nwant\n%+v", i, *cols[i], wantCols[i])
		}
	}

	// parent's constraints: from 18 the NOT NULL ones too.
	cons, err := parent.Constraints(ctx)
	if err != nil {
		t.Fatal(err)
	}
	conKinds := map[string]string{}
	for _, c := range cons {
		conKinds[c.Name] = fmt.Sprintf("%v %v %s v%v e%v %q", c.Kind, c.Columns, c.Definition, c.Validated, c.Enforced, c.Index)
	}
	wantKinds := map[string]string{
		"amount_pos":      `CHECK [amount] CHECK ((amount >= (0)::numeric)) NOT VALID vfalse etrue ""`,
		"parent_name_key": `UNIQUE [name] UNIQUE (name) vtrue etrue "parent_name_key"`,
		"parent_pkey":     `PRIMARY KEY [id] PRIMARY KEY (id) vtrue etrue "parent_pkey"`,
	}
	if pg18 {
		wantKinds["parent_id_not_null"] = `NOT NULL [id] NOT NULL id vtrue etrue ""`
		wantKinds["parent_name_not_null"] = `NOT NULL [name] NOT NULL name vtrue etrue ""`
	}
	if !reflect.DeepEqual(conKinds, wantKinds) {
		t.Errorf("Constraints(parent) =\n%q\nwant\n%q", conKinds, wantKinds)
	}

	// child: storage parameters, both RLS flags; its constraints, indexes,
	// triggers and policies.
	child := byName("child")
	if !slices.Equal(child.Options, []ParameterSetting{{"fillfactor", "70"}, {"toast.autovacuum_enabled", "false"}}) ||
		!child.RowSecurity || !child.ForceRowSecurity || child.RowEstimate != -1 {
		t.Errorf("child: Options %v, RowSecurity %v, ForceRowSecurity %v, RowEstimate %d", child.Options,
			child.RowSecurity, child.ForceRowSecurity, child.RowEstimate)
	}
	cons, err = child.Constraints(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var fk *Constraint
	var conNames []string
	for _, c := range cons {
		conNames = append(conNames, c.Name)
		if c.Name == "child_fk" {
			fk = c
		}
	}
	wantNames := []string{"child_fk", "child_pk", "ex"}
	if pg18 {
		wantNames = []string{"child_fk", "child_id_not_null", "child_pk", "ex"}
	}
	if !slices.Equal(conNames, wantNames) || fk == nil {
		t.Fatalf("Constraints(child) = %q, want %q: no constraint trigger", conNames, wantNames)
	}
	gotFK := *fk
	gotFK.OID = 0
	wantFK := Constraint{Name: "child_fk", Kind: ConstraintForeignKey,
		Definition: "FOREIGN KEY (pid) REFERENCES app.parent(id) MATCH FULL ON UPDATE CASCADE ON DELETE SET NULL DEFERRABLE INITIALLY DEFERRED",
		Columns:    []string{"pid"}, Deferrable: true, InitiallyDeferred: true, Validated: true, Enforced: true, IsLocal: true, NoInherit: true,
		ReferencedTable: ObjectName{"app", "parent"}, ReferencedColumns: []string{"id"},
		OnUpdate: ForeignKeyCascade, OnDelete: ForeignKeySetNull, Match: ForeignKeyMatchFull}
	if !reflect.DeepEqual(gotFK, wantFK) {
		t.Errorf("child_fk =\n%+v\nwant\n%+v", gotFK, wantFK)
	}

	idx, err := child.Indexes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var idxNames []string
	for _, x := range idx {
		idxNames = append(idxNames, x.Name)
		x.OID = 0
	}
	if !slices.Equal(idxNames, []string{"child_expr", "child_pk", "ex"}) {
		t.Fatalf("Indexes(child) = %q", idxNames)
	}
	wantIdx := []Index{
		{Name: "child_expr", Definition: "CREATE INDEX child_expr ON app.child USING btree (lower((b)::text) DESC, a) INCLUDE (b) WITH (fillfactor='80') WHERE (a > 0)",
			Method: "btree", Valid: true, KeyColumns: []string{"lower(b::text)", "a"}, IncludedColumns: []string{"b"},
			Predicate: "a > 0", Options: []ParameterSetting{{"fillfactor", "80"}}, Comment: "partial"},
		{Name: "child_pk", Definition: "CREATE UNIQUE INDEX child_pk ON app.child USING btree (id) INCLUDE (a)",
			Method: "btree", Unique: true, Primary: true, Valid: true, KeyColumns: []string{"id"}, IncludedColumns: []string{"a"},
			Constraint: "child_pk"},
		{Name: "ex", Definition: "CREATE INDEX ex ON app.child USING btree (a)",
			Method: "btree", Exclusion: true, Valid: true, KeyColumns: []string{"a"}, Constraint: "ex"},
	}
	for i := range idx {
		if !reflect.DeepEqual(*idx[i], wantIdx[i]) {
			t.Errorf("Indexes(child)[%d] =\n%+v\nwant\n%+v", i, *idx[i], wantIdx[i])
		}
	}

	trg, err := child.Triggers(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range trg {
		if x.Database() != d {
			t.Errorf("Triggers(child) %s: Database %v", x.Name, x.Database())
		}
		x.OID, x.db = 0, nil
	}
	touch := ObjectName{"app", "touch"}
	wantTrg := []Trigger{
		{Schema: "app", Table: "child", TableKind: RelationTable, HasCondition: true, Name: "t1", Definition: "CREATE TRIGGER t1 BEFORE INSERT OR UPDATE OF a, b ON app.child FOR EACH ROW WHEN ((new.a > 0)) EXECUTE FUNCTION app.touch()",
			Timing: TriggerBefore, Events: []TriggerEvent{TriggerInsert, TriggerUpdate}, UpdateColumns: []string{"a", "b"},
			ForEachRow: true, Function: touch, Enabled: TriggerEnabledReplica, Comment: "touch"},
		{Schema: "app", Table: "child", TableKind: RelationTable, Name: "t2", Definition: "CREATE CONSTRAINT TRIGGER t2 AFTER DELETE ON app.child DEFERRABLE INITIALLY DEFERRED FOR EACH ROW EXECUTE FUNCTION app.touch()",
			Timing: TriggerAfter, Events: []TriggerEvent{TriggerDelete}, ForEachRow: true, Function: touch,
			Enabled: TriggerEnabledOrigin, IsConstraint: true, Deferrable: true, InitiallyDeferred: true},
	}
	if len(trg) != len(wantTrg) {
		t.Fatalf("Triggers(child) = %d, want t1 and t2 alone (no foreign key trigger)", len(trg))
	}
	for i := range trg {
		if !reflect.DeepEqual(*trg[i], wantTrg[i]) {
			t.Errorf("Triggers(child)[%d] =\n%+v\nwant\n%+v", i, *trg[i], wantTrg[i])
		}
	}

	pols, err := child.Policies(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, x := range pols {
		if x.Database() != child.Database() {
			t.Errorf("policy %s: database %v, want the table's", x.Name, x.Database())
		}
		x.OID, x.db = 0, nil
	}
	wantPols := []Policy{
		{Schema: child.Schema, Table: child.Name, Name: "p1", Command: PolicyUpdate, Roles: []string{b, plain}, Using: "(a > 0)", WithCheck: "(b > 0)"},
		{Schema: child.Schema, Table: child.Name, Name: "p2", Command: PolicyAll, Permissive: true, Roles: []string{""}, Using: "true", Comment: "all"},
	}
	if len(pols) != 2 || !reflect.DeepEqual(*pols[0], wantPols[0]) || !reflect.DeepEqual(*pols[1], wantPols[1]) {
		t.Errorf("Policies(child) = %+v, want %+v", pols, wantPols)
	}

	// Sizes: parent holds rows and two indexes; a partitioned table none.
	if sz, err := parent.Size(ctx); err != nil || sz.Data <= 0 || sz.Indexes <= 0 || sz.Total != sz.Data+sz.Indexes {
		t.Errorf("Size(parent) = %+v, %v; want data and indexes, Total their sum", sz, err)
	}

	// Partitioning.
	part := byName("part")
	if !part.IsPartitioned() || part.PartitionKey != "RANGE (at)" || part.IsPartition || part.Parents != nil {
		t.Errorf("part = %+v; want partitioned by RANGE (at)", part)
	}
	if sz, err := part.Size(ctx); err != nil || sz != (TableSize{}) {
		t.Errorf("Size(part) = %+v, %v; want zero", sz, err)
	}
	kids, err := part.Children(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for i := range kids {
		kids[i].OID = 0
	}
	wantKids := []ChildTable{
		{ObjectName: ObjectName{"app", "part_2026"}, Kind: RelationTable, PartitionBound: "FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')"},
		{ObjectName: ObjectName{"app", "part_def"}, Kind: RelationTable, PartitionBound: "DEFAULT"},
	}
	if !reflect.DeepEqual(kids, wantKids) || kids[0].IsDefaultPartition() || !kids[1].IsDefaultPartition() {
		t.Errorf("Children(part) = %+v, want %+v", kids, wantKids)
	}
	def := byName("part_def")
	if !def.IsPartition || !def.IsDefaultPartition() || !slices.Equal(def.Parents, []ObjectName{{"app", "part"}}) {
		t.Errorf("part_def = %+v; want the DEFAULT partition of app.part", def)
	}
	if trg, err := def.Triggers(ctx); err != nil || len(trg) != 1 || !trg[0].Inherited || trg[0].Name != "pt" {
		t.Errorf("Triggers(part_def) = %+v, %v; want pt, inherited", trg, err)
	}
	if idx, err := part.Indexes(ctx); err != nil || len(idx) != 1 || !strings.Contains(idx[0].Definition, " ON ONLY app.part ") || !idx[0].Valid {
		t.Errorf("Indexes(part) = %+v, %v; want part_at ON ONLY, valid", idx, err)
	}

	// Inheritance: two parents in INHERITS order, the check inherited.
	kid := byName("kid")
	if !slices.Equal(kid.Parents, []ObjectName{{"app", "base"}, {"Other", "base2"}}) || kid.IsPartition {
		t.Errorf("kid.Parents = %v; want app.base, Other.base2", kid.Parents)
	}
	if kc, err := kid.Columns(ctx); err != nil || len(kc) != 3 || kc[0].Name != "x" || kc[0].IsLocal || kc[0].InheritCount != 1 ||
		kc[2].Name != "y" || !kc[2].IsLocal || kc[2].InheritCount != 0 {
		t.Errorf("Columns(kid) = %+v, %v; want x and z inherited, y local", kc, err)
	}
	if kc, err := kid.Constraints(ctx); err != nil || len(kc) < 1 || kc[0].Name != "base_x_check" || kc[0].IsLocal || kc[0].InheritCount != 1 {
		t.Errorf("Constraints(kid) = %+v, %v; want base_x_check inherited", kc, err)
	}
	base := byName("base")
	if bk, err := base.Children(ctx); err != nil || len(bk) != 1 || bk[0].Name != "kid" || bk[0].PartitionBound != "" {
		t.Errorf("Children(base) = %+v, %v; want kid, no bound", bk, err)
	}
	if pg18 {
		bc, err := base.Constraints(ctx)
		if err != nil || len(bc) != 2 || bc[1].Name != "loose" || bc[1].Enforced || bc[1].Validated {
			t.Errorf("Constraints(base) = %+v, %v; want loose NOT ENFORCED", bc, err)
		}
	}

	// The extension member and the temporary table.
	members, err := d.Tables(ctx, ObjectFilter{ExtensionMember: &yes})
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0].Name != "scratch" || members[0].Extension != "citext" ||
		members[0].Persistence != PersistenceUnlogged || members[0].RowEstimate != -1 {
		t.Errorf("Tables(ExtensionMember) = %+v; want app.scratch, unlogged, a member of citext", members)
	}
	temps, err := d.Tables(ctx, ObjectFilter{System: &yes, Name: []TextCriterion{{TextEquals, "tmp"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(temps) != 1 || temps[0].Persistence != PersistenceTemporary || !temps[0].IsSystem || !strings.HasPrefix(temps[0].Schema, "pg_temp_") {
		t.Errorf("Tables(tmp) = %+v; want the temporary table, in pg_temp_N", temps)
	}

	// A TableRef has no OID: every read of a part refuses it.
	ref := d.TableRef("app", "parent")
	for _, read := range []func() error{
		func() error { _, err := ref.Columns(ctx); return err },
		func() error { _, err := ref.Constraints(ctx); return err },
		func() error { _, err := ref.Indexes(ctx); return err },
		func() error { _, err := ref.Triggers(ctx); return err },
		func() error { _, err := ref.Policies(ctx); return err },
		func() error { _, err := ref.Children(ctx); return err },
		func() error { _, err := ref.Size(ctx); return err },
	} {
		if err := read(); !errors.Is(err, ErrHandleNotLoaded) {
			t.Errorf("TableRef read: err = %v, want ErrHandleNotLoaded", err)
		}
	}

	// As a plain role: the same listing and parts.
	dump := func(ctx context.Context) (string, error) {
		ts, err := d.Tables(ctx, ObjectFilter{Schema: []TextCriterion{{TextEquals, "app"}}})
		if err != nil {
			return "", err
		}
		var sb strings.Builder
		for _, tb := range ts {
			c := *tb
			c.db = nil
			fmt.Fprintf(&sb, "%+v\n", c)
			cols, err := tb.Columns(ctx)
			if err != nil {
				return "", err
			}
			cons, err := tb.Constraints(ctx)
			if err != nil {
				return "", err
			}
			idx, err := tb.Indexes(ctx)
			if err != nil {
				return "", err
			}
			trg, err := tb.Triggers(ctx)
			if err != nil {
				return "", err
			}
			pols, err := tb.Policies(ctx)
			if err != nil {
				return "", err
			}
			kids, err := tb.Children(ctx)
			if err != nil {
				return "", err
			}
			for _, v := range []any{cols, cons, idx, trg, pols} {
				fmt.Fprintf(&sb, "%s\n", derefAll(v))
			}
			fmt.Fprintf(&sb, "%+v\n", kids)
		}
		return sb.String(), nil
	}
	all, err := dump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var asPlain string
	err = s.InTransaction(ctx, func(ctx context.Context) error {
		if _, err := s.on(ctx).Exec(ctx, "SET LOCAL ROLE "+q(plain)); err != nil {
			return err
		}
		var err error
		asPlain, err = dump(ctx)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if asPlain != all {
		t.Errorf("as a plain role the tables differ:\n%s\nwant\n%s", asPlain, all)
	}
}

// derefAll prints a slice of pointers by value, so %+v shows the fields.
func derefAll(v any) string {
	rv := reflect.ValueOf(v)
	parts := make([]string, rv.Len())
	for i := range parts {
		parts[i] = fmt.Sprintf("%+v", rv.Index(i).Elem().Interface())
	}
	return "[" + strings.Join(parts, " ") + "]"
}
