package pgmo

import (
	"context"
	"fmt"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Foreign tables
// ============================================================
//
// foreign_table.go reads a database's foreign tables (PLAN.md P4.12):
// pg_class rows of kind f with their pg_foreign_table row — the server they
// are read through and their options — through the database's own pool.
// SQL Server's nearest is an external table. A foreign table stores
// nothing: its rows are the wrapper's, fetched from the server on every
// scan. It has columns, each with options of its own (Column.FDWOptions),
// CHECK and NOT NULL constraints the server trusts rather than enforces,
// triggers, and a place in an inheritance tree or as a partition; no
// indexes, no storage parameters, no row-level security.
//
// Its parts are read on demand, keyed by its OID, with the queries a
// table's are (Columns, Constraints, Triggers); a ForeignTableRef is
// refused (ErrHandleNotLoaded).

// ForeignTable is one foreign table of a database. Read it with
// Database.ForeignTables or Database.ForeignTableByName;
// Database.ForeignTableRef is a handle holding the name alone.
type ForeignTable struct {
	db *Database

	// Origin: IsSystem for one in a system schema; Extension for one an
	// extension owns.
	Origin

	OID    OID
	Schema string
	Name   string

	// Owner is the owning role's name (relowner).
	Owner string

	// Server is the foreign server's name (ftserver).
	Server string

	// Options are the table's own options (ftoptions) in stored order —
	// for postgres_fdw its schema_name and table_name on the remote side;
	// nil for none. A column's are Column.FDWOptions.
	Options []ParameterSetting

	// IsPartition reports a partition of a partitioned table
	// (relispartition): its parent is Parents[0], its bound PartitionBound.
	IsPartition bool

	// PartitionBound is a partition's bound as pg_get_expr renders it,
	// empty for a foreign table that is no partition.
	PartitionBound string

	// Parents are the tables this one inherits from (pg_inherits), in
	// INHERITS order: a partition's one parent, or those CREATE FOREIGN
	// TABLE … INHERITS named. nil for none.
	Parents []ObjectName

	// Privileges are the foreign table's ACL, per grantee, in stored order
	// — a table's privileges. One never set reads as its owner's every
	// privilege (PrivilegesDefault). A column's own grants are on
	// Column.Privileges.
	Privileges []PrivilegeGrant

	// PrivilegesDefault reports that the ACL was never set (relacl NULL).
	PrivilegesDefault bool

	// Comment is COMMENT ON FOREIGN TABLE's text, empty for none.
	Comment string
}

// Database is the Database the foreign table was read from.
func (t *ForeignTable) Database() *Database { return t.db }

// QualifiedName is the foreign table's name quoted for a statement:
// schema.name.
func (t *ForeignTable) QualifiedName() string { return pgsql.QualifiedName(t.Schema, t.Name) }

// foreignTableSelect is the pg_class read a *ForeignTable is built from,
// ending in its WHERE on the kind without an ORDER BY; scanForeignTable
// reads its row.
var foreignTableSelect = `SELECT c.oid, n.nspname::pg_catalog.text, c.relname::pg_catalog.text,
	pg_catalog.pg_get_userbyid(c.relowner)::pg_catalog.text,
	` + originJoinColumns("n.nspname") + `,
	fs.srvname::pg_catalog.text, COALESCE(ft.ftoptions, '{}'),
	c.relispartition, COALESCE(pg_catalog.pg_get_expr(c.relpartbound, c.oid), ''),
	` + tableParents + `,
	c.relacl IS NULL,
	` + aclColumns("p") + `,
	COALESCE(ds.description, '')
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
JOIN pg_catalog.pg_foreign_table ft ON ft.ftrelid = c.oid
JOIN pg_catalog.pg_foreign_server fs ON fs.oid = ft.ftserver
` + originJoin(catalogClass, "c.oid") + `
LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = c.oid
	AND ds.classoid = 'pg_catalog.pg_class'::pg_catalog.regclass AND ds.objsubid = 0
` + relationACL + `
WHERE c.relkind = 'f'`

// scanForeignTable builds a *ForeignTable of d from one foreignTableSelect
// row.
func scanForeignTable(d *Database, scan func(...any) error) (*ForeignTable, error) {
	t := &ForeignTable{db: d}
	var options, parentSchemas, parentNames []string
	var acl privilegeArrays
	dest := []any{&t.OID, &t.Schema, &t.Name, &t.Owner}
	dest = append(dest, scanOrigin(&t.Origin)...)
	dest = append(dest, &t.Server, &options, &t.IsPartition, &t.PartitionBound,
		&parentSchemas, &parentNames, &t.PrivilegesDefault)
	dest = append(dest, acl.dest()...)
	dest = append(dest, &t.Comment)
	if err := scan(dest...); err != nil {
		return nil, err
	}
	t.Options = parseSettings(options)
	var err error
	if t.Parents, err = objectNames(parentSchemas, parentNames); err != nil {
		return nil, err
	}
	t.Privileges, err = acl.all()
	return t, err
}

// ForeignTables lists the database's foreign tables by schema then name,
// narrowed by f: Name on the table's name, Schema on its schema's, System
// for those in a system schema, ExtensionMember for those an extension
// owns. Every foreign table is listed whatever the role's privileges on it.
// They are read through the database's own pool.
func (d *Database) ForeignTables(ctx context.Context, f ObjectFilter) ([]*ForeignTable, error) {
	where, args, err := f.clause(relationFilterColumns, nil)
	if err != nil {
		return nil, err
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, fmt.Sprintf("list foreign tables of database %q", d.Name),
		func(scan func(...any) error) (*ForeignTable, error) {
			return scanForeignTable(d, scan)
		}, foreignTableSelect+where+`
ORDER BY n.nspname, c.relname`, args...)
}

// ForeignTableByName reads the foreign table schema.name — stored names,
// matched exactly (fold typed input with pgsql.FoldIdent first). schema is
// required (ErrSchemaRequired). A name the database holds no foreign table
// by — none, or another kind of relation — is ErrNotFound. Use
// ForeignTableRef instead when only a handle to write through is needed.
func (d *Database) ForeignTableByName(ctx context.Context, schema, name string) (*ForeignTable, error) {
	qualified := pgsql.QualifiedName(schema, name)
	if schema == "" {
		return nil, fmt.Errorf("pgmo: read foreign table %q: %w", name, ErrSchemaRequired)
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return readByName(ctx, q, d, scanForeignTable, foreignTableSelect+`
	AND n.nspname = $1::pg_catalog.text AND c.relname = $2::pg_catalog.text`, []any{schema, name},
		notFoundf("pgmo: foreign table %s not found in database %q", qualified, d.Name),
		fmt.Sprintf("read foreign table %s of database %q", qualified, d.Name))
}

// ForeignTableRef returns a handle for the foreign table schema.name
// without reading the catalog: Schema and Name are set and every other
// field is zero, OID included — so the reads of its parts refuse it
// (ErrHandleNotLoaded). It is not interchangeable with ForeignTableByName.
func (d *Database) ForeignTableRef(schema, name string) *ForeignTable {
	return &ForeignTable{db: d, Schema: schema, Name: name}
}

// requireLoaded refuses a read keyed by the OID on a foreign table that has
// none — a ForeignTableRef.
func (t *ForeignTable) requireLoaded(what string) error {
	if t.OID == 0 {
		return fmt.Errorf("pgmo: %s %s: %w", what, t.QualifiedName(), ErrHandleNotLoaded)
	}
	return nil
}

// Columns reads the foreign table's columns in attnum order, as a table's
// are read (Table.Columns), each with its own options (FDWOptions) —
// postgres_fdw's column_name. A ForeignTableRef is refused
// (ErrHandleNotLoaded); one dropped since it was read has none.
func (t *ForeignTable) Columns(ctx context.Context) ([]*Column, error) {
	if err := t.requireLoaded("list columns of foreign table"); err != nil {
		return nil, err
	}
	q, err := t.db.server.onDatabase(ctx, t.db.Name)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, "list columns of foreign table "+t.QualifiedName(), scanColumn, columnsQuery, t.OID)
}

// Constraints reads the foreign table's constraints by name, as a table's
// are read (Table.Constraints): CHECK, and NOT NULL from 18 — the only
// kinds a foreign table takes, which the server assumes the remote rows
// satisfy rather than checks. A ForeignTableRef is refused
// (ErrHandleNotLoaded).
func (t *ForeignTable) Constraints(ctx context.Context) ([]*Constraint, error) {
	if err := t.requireLoaded("list constraints of foreign table"); err != nil {
		return nil, err
	}
	q, err := t.db.server.onDatabase(ctx, t.db.Name)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, "list constraints of foreign table "+t.QualifiedName(), scanConstraint,
		constraintsQuery(int(t.db.server.version)), t.OID)
}

// Triggers reads the foreign table's triggers by name — row and statement
// triggers, which run locally around the wrapper's writes. A
// ForeignTableRef is refused (ErrHandleNotLoaded).
func (t *ForeignTable) Triggers(ctx context.Context) ([]*Trigger, error) {
	if err := t.requireLoaded("list triggers of foreign table"); err != nil {
		return nil, err
	}
	return relationTriggers(ctx, t.db, t.OID, "list triggers of foreign table "+t.QualifiedName())
}
