package pgmo

import (
	"context"
	"fmt"
)

// ============================================================
// Schemas
// ============================================================
//
// schema.go reads a database's schemas (PLAN.md P4.3). Unlike databases and
// roles, pg_namespace is per database: a schema is read through its
// database's own pool (s.onDatabase), and a Schema points back to the
// Database it was read from.
//
// gosmo's Schema carries an ID, a name and an owner. PostgreSQL's adds what
// SQL Server keeps elsewhere: the schema's ACL (USAGE and CREATE — SQL
// Server's schema permissions), the default privileges given to objects
// created in it (pg_default_acl IN SCHEMA, which SQL Server has no notion
// of), its Origin, and its comment.

// Schema is one schema of a database, as pg_namespace describes it. Read it
// with Database.Schemas or Database.SchemaByName; Database.SchemaRef is a
// handle holding the name alone.
type Schema struct {
	db *Database

	// Origin: IsSystem for pg_catalog, information_schema, pg_toast and the
	// pg_temp_N/pg_toast_temp_N of every session; Extension for a schema an
	// extension created (CREATE EXTENSION … SCHEMA does not make the schema
	// a member; an extension script's CREATE SCHEMA does).
	Origin

	OID  OID
	Name string

	// Owner is the owning role's name (nspowner). From 15, public is owned
	// by pg_database_owner — whoever owns the database.
	Owner string

	// Privileges are the schema's ACL: USAGE (look up objects in it) and
	// CREATE (create objects in it), per grantee, in stored order. A schema
	// whose ACL was never set holds the hard-wired default — its owner's
	// USAGE and CREATE — and that is what is read (PrivilegesDefault).
	Privileges []PrivilegeGrant

	// PrivilegesDefault reports that the ACL was never set (nspacl NULL):
	// Privileges are the owner's defaults, and no GRANT or REVOKE has touched
	// the schema. Once one has, it stays false even if the ACL is back to the
	// default's value.
	PrivilegesDefault bool

	// DefaultPrivileges are the ALTER DEFAULT PRIVILEGES … IN SCHEMA entries
	// for this schema: what objects created in it later are granted on top of
	// the database-wide defaults (Database.DefaultPrivileges). By role then
	// object type code (byte order: S, T, r); nil for none.
	DefaultPrivileges []DefaultPrivileges

	// Comment is COMMENT ON SCHEMA's text, empty for none.
	Comment string
}

// Database is the Database the schema was read from.
func (s *Schema) Database() *Database { return s.db }

// schemaSelect is the pg_namespace read a *Schema is built from, ending in
// WHERE true without an ORDER BY; scanSchema reads its row.
var schemaSelect = `SELECT n.oid, n.nspname::pg_catalog.text,
	pg_catalog.pg_get_userbyid(n.nspowner)::pg_catalog.text,
	` + originColumns("n.nspname", catalogNamespace, "n.oid") + `,
	n.nspacl IS NULL,
	` + aclColumns("p") + `,
	` + defaultPrivilegeColumns("dp") + `,
	COALESCE(pg_catalog.obj_description(n.oid, 'pg_namespace'), '')
FROM pg_catalog.pg_namespace n
` + aclLateral("COALESCE(n.nspacl, pg_catalog.acldefault('n', n.nspowner))", "p") + `
` + defaultPrivilegeLateral("n.oid", "dp") + `
WHERE true`

// schemaFilterColumns is what an ObjectFilter compares against in the schema
// listing: Name and Schema both the schema's name, System the system-schema
// rule, ExtensionMember an extension's schema.
var schemaFilterColumns = filterColumns{
	name:      "n.nspname",
	schema:    "n.nspname",
	system:    systemSchema("n.nspname"),
	extension: isExtensionMember(catalogNamespace, "n.oid"),
}

// scanSchema builds a *Schema of d from one schemaSelect row.
func scanSchema(d *Database, scan func(...any) error) (*Schema, error) {
	s := &Schema{db: d}
	var acl privilegeArrays
	var dacl defaultPrivilegeArrays
	dest := []any{&s.OID, &s.Name, &s.Owner}
	dest = append(dest, scanOrigin(&s.Origin)...)
	dest = append(dest, &s.PrivilegesDefault)
	dest = append(dest, acl.dest()...)
	dest = append(dest, dacl.dest()...)
	dest = append(dest, &s.Comment)
	if err := scan(dest...); err != nil {
		return nil, err
	}
	var err error
	if s.Privileges, err = acl.all(); err != nil {
		return nil, err
	}
	if s.DefaultPrivileges, err = dacl.entries(s.Name); err != nil {
		return nil, err
	}
	return s, nil
}

// Schemas lists the database's schemas, by name, narrowed by f: its Name
// (and Schema) criteria on the schema's name, System for the system
// schemas, ExtensionMember for those an extension created. Every schema is
// listed, whatever the role's privileges on it: pg_namespace is readable by
// all. They are read through the database's own pool; inside InTransaction,
// only the transaction's database can be read.
func (d *Database) Schemas(ctx context.Context, f ObjectFilter) ([]*Schema, error) {
	where, args, err := f.clause(schemaFilterColumns, nil)
	if err != nil {
		return nil, err
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, fmt.Sprintf("list schemas of database %q", d.Name), func(scan func(...any) error) (*Schema, error) {
		return scanSchema(d, scan)
	}, schemaSelect+where+`
ORDER BY n.nspname`, args...)
}

// SchemaByName reads the schema named name — a stored name, matched exactly
// (fold typed input with pgsql.FoldIdent first). One the database does not
// hold is ErrNotFound. Use SchemaRef instead when only a handle to write
// through is needed.
func (d *Database) SchemaByName(ctx context.Context, name string) (*Schema, error) {
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return readByName(ctx, q, d, scanSchema, schemaSelect+`
	AND n.nspname = $1::pg_catalog.text`, []any{name},
		notFoundf("pgmo: schema %q not found in database %q", name, d.Name),
		fmt.Sprintf("read schema %q of database %q", name, d.Name))
}

// SchemaRef returns a handle for the schema named name without reading the
// catalog: Name is set and every other field is zero. It is what a write to
// a schema the caller knows exists goes through — one it has just created,
// or one created only under WithScript, which SchemaByName would not find.
// It is not interchangeable with SchemaByName.
func (d *Database) SchemaRef(name string) *Schema {
	return &Schema{db: d, Name: name}
}
