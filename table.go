package pgmo

import (
	"context"
	"fmt"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Tables
// ============================================================
//
// table.go reads a database's tables (PLAN.md P4.4): pg_class rows of kind
// r (ordinary) and p (partitioned), through the database's own pool. As
// gosmo's, a Table carries what one pg_class row says about it — kind,
// persistence, partitioning, inheritance parents, storage parameters,
// row-level security, the planner's row estimate, its ACL and comment — and
// its parts are read on demand, one query each, keyed by its OID: Columns
// (table_column.go), Constraints (table_constraint.go), Indexes (index.go),
// Triggers (trigger.go), Policies (policy.go), Children and Size (here).
// Those reads refuse a TableRef (ErrHandleNotLoaded): it has no OID, and
// asking for object 0 would answer with an empty list a caller could not tell
// from a table that has no columns.
//
// What PostgreSQL adds over SQL Server: unlogged and temporary tables
// (Persistence), declarative partitioning (a partitioned table is a
// relation of its own, with a key; each partition a table with a bound) and
// table inheritance (Parents, Children), storage parameters (reloptions,
// SQL Server's table options), and row-level security policies. What it
// lacks: a creation date, filegroups (a tablespace is the nearest), temporal,
// memory-optimized, graph and ledger tables.
//
// Expressions the server renders — a default, a partition bound, a check
// constraint, an index or trigger definition — name objects as the
// session's search_path sees them: one visible on the path is left
// unqualified (nextval('s'::regclass) rather than 'app.s'), as psql's \d
// shows. pgmo does not change the caller's path; the structured fields
// (Column.Type, Constraint.ReferencedTable, Trigger.Function) are qualified
// whatever it is.

// ObjectName is a schema-scoped object's schema and name, as stored.
type ObjectName struct {
	Schema string
	Name   string
}

// QualifiedName is the name quoted for a statement: schema.name, each part
// quoted when it must be. An empty Schema gives the name alone.
func (o ObjectName) QualifiedName() string {
	if o.Schema == "" {
		return pgsql.QuoteIdent(o.Name)
	}
	return pgsql.QualifiedName(o.Schema, o.Name)
}

// RelationKind is pg_class.relkind: what kind of relation a row is. It is
// the catalog's one-letter code, so a kind a later release adds is still
// read — String names the ones pgmo knows.
type RelationKind string

const (
	RelationTable            RelationKind = "r"
	RelationPartitionedTable RelationKind = "p"
	RelationView             RelationKind = "v"
	RelationMaterializedView RelationKind = "m"
	RelationForeignTable     RelationKind = "f"
	RelationSequence         RelationKind = "S"
	RelationIndex            RelationKind = "i"
	RelationPartitionedIndex RelationKind = "I"
	RelationCompositeType    RelationKind = "c"
	RelationTOASTTable       RelationKind = "t"
)

// String names the kind as psql's \d does ("table", "partitioned table",
// "foreign table"…); an unknown code reads RelationKind("x").
func (k RelationKind) String() string {
	switch k {
	case RelationTable:
		return "table"
	case RelationPartitionedTable:
		return "partitioned table"
	case RelationView:
		return "view"
	case RelationMaterializedView:
		return "materialized view"
	case RelationForeignTable:
		return "foreign table"
	case RelationSequence:
		return "sequence"
	case RelationIndex:
		return "index"
	case RelationPartitionedIndex:
		return "partitioned index"
	case RelationCompositeType:
		return "composite type"
	case RelationTOASTTable:
		return "TOAST table"
	}
	return fmt.Sprintf("RelationKind(%q)", string(k))
}

// Persistence is pg_class.relpersistence: whether a relation's data is
// WAL-logged and outlives the session.
type Persistence string

const (
	PersistencePermanent Persistence = "p"
	PersistenceUnlogged  Persistence = "u" // not WAL-logged: emptied by crash recovery, not replicated
	PersistenceTemporary Persistence = "t" // the creating session's own, in its pg_temp_N schema
)

// String names the persistence as CREATE TABLE spells it: "permanent",
// "UNLOGGED", "TEMPORARY"; an unknown code reads Persistence("x").
func (p Persistence) String() string {
	switch p {
	case PersistencePermanent:
		return "permanent"
	case PersistenceUnlogged:
		return "UNLOGGED"
	case PersistenceTemporary:
		return "TEMPORARY"
	}
	return fmt.Sprintf("Persistence(%q)", string(p))
}

// Table is one table of a database: an ordinary or a partitioned one, a
// partition included. Read it with Database.Tables or Database.TableByName;
// Database.TableRef is a handle holding the name alone.
type Table struct {
	db *Database

	// Origin: IsSystem for a table in a system schema (the catalogs, and
	// every session's temporary tables in pg_temp_N); Extension for one an
	// extension owns.
	Origin

	OID    OID
	Schema string
	Name   string

	// Owner is the owning role's name (relowner).
	Owner string

	// Kind is RelationTable or RelationPartitionedTable.
	Kind RelationKind

	Persistence Persistence

	// IsPartition reports a partition of a partitioned table
	// (relispartition): its parent is Parents[0], its bound PartitionBound.
	IsPartition bool

	// PartitionKey is a partitioned table's key as pg_get_partkeydef
	// renders it ("RANGE (at)", "HASH (id)"), empty for any other table.
	PartitionKey string

	// PartitionBound is a partition's bound as pg_get_expr renders it
	// ("FOR VALUES FROM ('2026-01-01') TO ('2027-01-01')", "DEFAULT"),
	// empty for a table that is no partition.
	PartitionBound string

	// Parents are the tables this one inherits from (pg_inherits), in
	// INHERITS order: a partition's one parent, or the parents of a table
	// using inheritance. nil for none.
	Parents []ObjectName

	// Tablespace is the table's own tablespace, empty for the database's
	// default.
	Tablespace string

	// Options are the storage parameters (reloptions: fillfactor,
	// autovacuum_*…) as stored, then those of its TOAST table with a
	// "toast." prefix, as WITH (…) takes them. nil for none.
	Options []ParameterSetting

	// RowSecurity is ENABLE ROW LEVEL SECURITY (relrowsecurity);
	// ForceRowSecurity is FORCE, which applies the policies to the owner
	// too (relforcerowsecurity).
	RowSecurity      bool
	ForceRowSecurity bool

	// RowEstimate is the planner's row count (reltuples), as of the last
	// VACUUM, ANALYZE or CREATE INDEX; -1 when none has run yet. A
	// partitioned table holds no rows of its own and reads -1 until
	// ANALYZE.
	RowEstimate int64

	// Privileges are the table's ACL, per grantee, in stored order: SELECT,
	// INSERT, UPDATE, DELETE, TRUNCATE, REFERENCES, TRIGGER, and MAINTAIN
	// from 17. One never set reads as the hard-wired default, its owner's
	// every privilege (PrivilegesDefault). A column's own grants are on
	// Column.Privileges.
	Privileges []PrivilegeGrant

	// PrivilegesDefault reports that the ACL was never set (relacl NULL).
	PrivilegesDefault bool

	// Comment is COMMENT ON TABLE's text, empty for none.
	Comment string
}

// Database is the Database the table was read from.
func (t *Table) Database() *Database { return t.db }

// QualifiedName is the table's name quoted for a statement: schema.name.
func (t *Table) QualifiedName() string { return pgsql.QualifiedName(t.Schema, t.Name) }

// IsPartitioned reports a partitioned table: one holding no rows of its
// own, routing them to its partitions.
func (t *Table) IsPartitioned() bool { return t.Kind == RelationPartitionedTable }

// IsDefaultPartition reports the DEFAULT partition of its parent, which
// takes the rows no other partition's bound does.
func (t *Table) IsDefaultPartition() bool { return t.PartitionBound == "DEFAULT" }

// requireLoaded refuses a read keyed by the OID on a table that has none —
// a TableRef.
func (t *Table) requireLoaded(what string) error {
	if t.OID == 0 {
		return fmt.Errorf("pgmo: %s %s: %w", what, t.QualifiedName(), ErrHandleNotLoaded)
	}
	return nil
}

// on is the Querier a read of the table's parts goes through: its
// database's pool, or the transaction's when it is that database.
func (t *Table) on(ctx context.Context) (Querier, error) {
	return t.db.server.onDatabase(ctx, t.db.Name)
}

// tableParents reads the names of the parents of the table c, in
// pg_inherits order, as two parallel arrays: schemas, then names.
const tableParents = `ARRAY(SELECT pn.nspname::pg_catalog.text FROM pg_catalog.pg_inherits i
		JOIN pg_catalog.pg_class pc ON pc.oid = i.inhparent
		JOIN pg_catalog.pg_namespace pn ON pn.oid = pc.relnamespace
		WHERE i.inhrelid = c.oid ORDER BY i.inhseqno),
	ARRAY(SELECT pc.relname::pg_catalog.text FROM pg_catalog.pg_inherits i
		JOIN pg_catalog.pg_class pc ON pc.oid = i.inhparent
		WHERE i.inhrelid = c.oid ORDER BY i.inhseqno)`

// relationOptions is the reloptions of the relation c and, prefixed
// "toast.", those of its TOAST table tc: never NULL.
const relationOptions = `COALESCE(c.reloptions, '{}') || ARRAY(SELECT 'toast.' || o FROM pg_catalog.unnest(tc.reloptions) AS o)`

// tableSelect is the pg_class read a *Table is built from, ending in its
// WHERE on the kind without an ORDER BY; scanTable reads its row.
var tableSelect = `SELECT c.oid, n.nspname::pg_catalog.text, c.relname::pg_catalog.text,
	pg_catalog.pg_get_userbyid(c.relowner)::pg_catalog.text,
	` + originColumns("n.nspname", catalogClass, "c.oid") + `,
	c.relkind::pg_catalog.text, c.relpersistence::pg_catalog.text, c.relispartition,
	COALESCE(pg_catalog.pg_get_partkeydef(c.oid), ''),
	COALESCE(pg_catalog.pg_get_expr(c.relpartbound, c.oid), ''),
	` + tableParents + `,
	COALESCE(ts.spcname::pg_catalog.text, ''),
	` + relationOptions + `,
	c.relrowsecurity, c.relforcerowsecurity, c.reltuples::bigint,
	c.relacl IS NULL,
	` + aclColumns("p") + `,
	COALESCE(pg_catalog.obj_description(c.oid, 'pg_class'), '')
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
LEFT JOIN pg_catalog.pg_class tc ON tc.oid = c.reltoastrelid
LEFT JOIN pg_catalog.pg_tablespace ts ON ts.oid = c.reltablespace
` + aclLateral("COALESCE(c.relacl, pg_catalog.acldefault('r', c.relowner))", "p") + `
WHERE c.relkind IN ('r', 'p')`

// relationFilterColumns is what an ObjectFilter compares against in a
// listing of pg_class rows c in schema n: tables, views, materialized views.
var relationFilterColumns = filterColumns{
	name:      "c.relname",
	schema:    "n.nspname",
	system:    systemSchema("n.nspname"),
	extension: isExtensionMember(catalogClass, "c.oid"),
}

// objectNames zips parallel schema and name arrays; nil for none.
func objectNames(schemas, names []string) ([]ObjectName, error) {
	if len(schemas) != len(names) {
		return nil, fmt.Errorf("name arrays of different lengths: %d schemas, %d names", len(schemas), len(names))
	}
	if len(names) == 0 {
		return nil, nil
	}
	out := make([]ObjectName, len(names))
	for i := range names {
		out[i] = ObjectName{Schema: schemas[i], Name: names[i]}
	}
	return out, nil
}

// scanTable builds a *Table of d from one tableSelect row.
func scanTable(d *Database, scan func(...any) error) (*Table, error) {
	t := &Table{db: d}
	var kind, persistence string
	var parentSchemas, parentNames, options []string
	var acl privilegeArrays
	dest := []any{&t.OID, &t.Schema, &t.Name, &t.Owner}
	dest = append(dest, scanOrigin(&t.Origin)...)
	dest = append(dest, &kind, &persistence, &t.IsPartition, &t.PartitionKey, &t.PartitionBound,
		&parentSchemas, &parentNames, &t.Tablespace, &options,
		&t.RowSecurity, &t.ForceRowSecurity, &t.RowEstimate, &t.PrivilegesDefault)
	dest = append(dest, acl.dest()...)
	dest = append(dest, &t.Comment)
	if err := scan(dest...); err != nil {
		return nil, err
	}
	t.Kind, t.Persistence = RelationKind(kind), Persistence(persistence)
	t.Options = parseSettings(options)
	var err error
	if t.Parents, err = objectNames(parentSchemas, parentNames); err != nil {
		return nil, err
	}
	if t.Privileges, err = acl.all(); err != nil {
		return nil, err
	}
	return t, nil
}

// Tables lists the database's tables — ordinary and partitioned, partitions
// included — by schema then name, narrowed by f: Name on the table's name,
// Schema on its schema's, System for those in a system schema (the
// catalogs, and every session's temporary tables), ExtensionMember for
// those an extension owns. Every table is listed whatever the role's
// privileges on it: pg_class is readable by all. They are read through the
// database's own pool.
func (d *Database) Tables(ctx context.Context, f ObjectFilter) ([]*Table, error) {
	where, args, err := f.clause(relationFilterColumns, nil)
	if err != nil {
		return nil, err
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, fmt.Sprintf("list tables of database %q", d.Name), func(scan func(...any) error) (*Table, error) {
		return scanTable(d, scan)
	}, tableSelect+where+`
ORDER BY n.nspname, c.relname`, args...)
}

// TableByName reads the table schema.name — stored names, matched exactly
// (fold typed input with pgsql.FoldIdent first). schema is required
// (ErrSchemaRequired): pgmo never resolves a name through the session's
// search_path. A name the database holds no table by — none, or a view —
// is ErrNotFound. Use TableRef instead when only a handle to write through
// is needed.
func (d *Database) TableByName(ctx context.Context, schema, name string) (*Table, error) {
	qualified := pgsql.QualifiedName(schema, name)
	if schema == "" {
		return nil, fmt.Errorf("pgmo: read table %q: %w", name, ErrSchemaRequired)
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return readByName(ctx, q, d, scanTable, tableSelect+`
	AND n.nspname = $1::pg_catalog.text AND c.relname = $2::pg_catalog.text`, []any{schema, name},
		notFoundf("pgmo: table %s not found in database %q", qualified, d.Name),
		fmt.Sprintf("read table %s of database %q", qualified, d.Name))
}

// TableRef returns a handle for the table schema.name without reading the
// catalog: Schema and Name are set and every other field is zero, OID
// included — so the reads of a table's parts refuse it
// (ErrHandleNotLoaded). It is what a write to a table the caller knows
// exists goes through, or one created only under WithScript. It is not
// interchangeable with TableByName.
func (d *Database) TableRef(schema, name string) *Table {
	return &Table{db: d, Schema: schema, Name: name}
}

// ============================================================
// Partitions and inheritance children
// ============================================================

// ChildTable is a table inheriting from another: a partition of a
// partitioned table, or a child by INHERITS.
type ChildTable struct {
	OID OID
	ObjectName

	// Kind is RelationTable, RelationPartitionedTable (a partition itself
	// partitioned) or RelationForeignTable (a foreign table as a partition
	// or child).
	Kind RelationKind

	// PartitionBound is a partition's bound ("FOR VALUES …", "DEFAULT"),
	// empty for an inheritance child.
	PartitionBound string

	// DetachPending reports a partition being detached CONCURRENTLY whose
	// second transaction has not finished (pg_inherits.inhdetachpending):
	// ALTER TABLE … DETACH PARTITION … FINALIZE completes it.
	DetachPending bool
}

// IsDefaultPartition reports the parent's DEFAULT partition.
func (c ChildTable) IsDefaultPartition() bool { return c.PartitionBound == "DEFAULT" }

// childTablesQuery reads the relations inheriting directly from $1.
const childTablesQuery = `SELECT c.oid, n.nspname::pg_catalog.text, c.relname::pg_catalog.text,
	c.relkind::pg_catalog.text, COALESCE(pg_catalog.pg_get_expr(c.relpartbound, c.oid), ''), i.inhdetachpending
FROM pg_catalog.pg_inherits i
JOIN pg_catalog.pg_class c ON c.oid = i.inhrelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
WHERE i.inhparent = $1
ORDER BY n.nspname, c.relname`

// Children reads the tables inheriting directly from this one, by schema
// then name: a partitioned table's partitions with their bounds, or the
// children of an INHERITS parent. Not recursive: a partition that is
// itself partitioned lists its own. nil for none.
func (t *Table) Children(ctx context.Context) ([]ChildTable, error) {
	if err := t.requireLoaded("list children of table"); err != nil {
		return nil, err
	}
	q, err := t.on(ctx)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, "list children of table "+t.QualifiedName(), func(scan func(...any) error) (ChildTable, error) {
		var c ChildTable
		var kind string
		err := scan(&c.OID, &c.Schema, &c.Name, &kind, &c.PartitionBound, &c.DetachPending)
		c.Kind = RelationKind(kind)
		return c, err
	}, childTablesQuery, t.OID)
}

// ============================================================
// Sizes
// ============================================================

// TableSize is a table's size on disk, in bytes.
type TableSize struct {
	// Data is the table's own storage: its main data, free space and
	// visibility maps, and its TOAST table (pg_table_size).
	Data int64

	// Indexes is every index on it, the TOAST table's included
	// (pg_indexes_size).
	Indexes int64

	// Total is Data plus Indexes (pg_total_relation_size).
	Total int64
}

// tableSizeQuery reads the sizes of the relation $1, through pg_class so a
// dropped one is no row rather than NULLs.
const tableSizeQuery = `SELECT pg_catalog.pg_table_size(c.oid), pg_catalog.pg_indexes_size(c.oid),
	pg_catalog.pg_total_relation_size(c.oid)
FROM pg_catalog.pg_class c WHERE c.oid = $1`

// Size reads the table's size on disk. The server stats each file on every
// call, so it is read on demand rather than with the listing. A
// partitioned table's own size is 0 — its rows are in its partitions,
// each sized on its own. Any role may read it; a table dropped since it
// was read is ErrNotFound.
func (t *Table) Size(ctx context.Context) (TableSize, error) {
	var s TableSize
	if err := t.requireLoaded("read size of table"); err != nil {
		return s, err
	}
	q, err := t.on(ctx)
	if err != nil {
		return s, err
	}
	err = queryRowScan(ctx, q, tableSizeQuery, []any{t.OID}, &s.Data, &s.Indexes, &s.Total)
	return foundRow(s, err, notFoundf("pgmo: table %s not found in database %q", t.QualifiedName(), t.db.Name),
		"read size of table "+t.QualifiedName())
}
