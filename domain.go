package pgmo

import (
	"context"
	"fmt"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Domains
// ============================================================
//
// domain.go reads a database's domains (PLAN.md P4.8): pg_type rows of
// kind d with their constraints from pg_constraint, through the database's
// own pool. A domain is SQL Server's alias type with constraints: a base
// type narrowed by CHECKs and NOT NULL, with a default and a collation.
//
// NOT NULL is read twice over, as the server stores it: NotNull is the
// domain's own flag (typnotnull) on every release; from 17 the server also
// keeps it as a named constraint (contype 'n', "d_not_null" unless CREATE
// DOMAIN … CONSTRAINT name NOT NULL named it), listed in Constraints beside
// the CHECKs. Before 17 the name given is not kept anywhere.

// DomainConstraint is one constraint of a domain.
type DomainConstraint struct {
	Name string

	// Kind is ConstraintCheck, or ConstraintNotNull from 17.
	Kind ConstraintKind

	// Definition is pg_get_constraintdef's text, "CHECK ((VALUE > 0))" or
	// "NOT NULL", with NOT VALID for one not validated — names inside it
	// as the session's search_path sees them.
	Definition string

	// Validated is false for a constraint added NOT VALID and not yet
	// VALIDATEd: values stored before it was added are unchecked.
	Validated bool
}

// Domain is one domain of a database. Read it with Database.Domains or
// Database.DomainByName; Database.DomainRef is a handle holding the name
// alone.
type Domain struct {
	db *Database

	// Origin: IsSystem for a domain in a system schema
	// (information_schema's cardinal_number and its kin); Extension for one
	// an extension owns.
	Origin

	OID    OID
	Schema string
	Name   string

	// Owner is the owning role's name (typowner).
	Owner string

	// BaseTypeOID and BaseType are the type the domain is over, BaseType
	// with its modifier (varchar(10), numeric(10,2)) and qualified outside
	// pg_catalog whatever the search_path, as Column.Type. It may be
	// another domain.
	BaseTypeOID OID
	BaseType    string

	// NotNull is the domain's NOT NULL (typnotnull), on every release.
	NotNull bool

	// Default is the DEFAULT expression, empty for none, as the session's
	// search_path sees the names in it.
	Default string

	// Collation is the domain's COLLATE when it differs from its base
	// type's; zero otherwise.
	Collation ObjectName

	// Constraints are the domain's CHECK constraints — and from 17 its NOT
	// NULL — by name; nil for none.
	Constraints []DomainConstraint

	// Privileges are the domain's ACL (USAGE), per grantee, in stored
	// order. One never set reads as the hard-wired default: PUBLIC's USAGE
	// and the owner's (PrivilegesDefault).
	Privileges []PrivilegeGrant

	// PrivilegesDefault reports that the ACL was never set (typacl NULL).
	PrivilegesDefault bool

	// Comment is COMMENT ON DOMAIN's text, empty for none.
	Comment string
}

// Database is the Database the domain was read from.
func (dm *Domain) Database() *Database { return dm.db }

// QualifiedName is the domain's name quoted for a statement: schema.name.
func (dm *Domain) QualifiedName() string { return pgsql.QualifiedName(dm.Schema, dm.Name) }

// domainConstraints is the LEFT JOIN reading every domain's constraints by
// name — names, kinds, definitions, validated flags — as dc's arrays: one
// grouped read of pg_constraint, not a subquery per domain (see
// originJoin). A domain with none reads NULLs.
const domainConstraints = `LEFT JOIN (SELECT c.contypid,
		pg_catalog.array_agg(c.conname::pg_catalog.text ORDER BY c.conname),
		pg_catalog.array_agg(c.contype::pg_catalog.text ORDER BY c.conname),
		pg_catalog.array_agg(pg_catalog.pg_get_constraintdef(c.oid) ORDER BY c.conname),
		pg_catalog.array_agg(c.convalidated ORDER BY c.conname)
	FROM pg_catalog.pg_constraint c
	WHERE c.contypid <> 0
	GROUP BY c.contypid) AS dc(type, names, kinds, defs, validated) ON dc.type = t.oid`

// domainSelect is the pg_type read a *Domain is built from, ending in its
// WHERE on the kind without an ORDER BY; scanDomain reads its row. The
// default is the stored expression decompiled (typdefaultbin), as
// pg_dump reads it, so a renamed object is named as it is now.
var domainSelect = `SELECT t.oid, n.nspname::pg_catalog.text, t.typname::pg_catalog.text,
	pg_catalog.pg_get_userbyid(t.typowner)::pg_catalog.text,
	` + originJoinColumns("n.nspname") + `,
	t.typbasetype, ` + typeNameWithModifier("btn", "ft") + `,
	t.typnotnull, COALESCE(pg_catalog.pg_get_expr(t.typdefaultbin, 0), t.typdefault, ''),
	CASE WHEN t.typcollation <> bt.typcollation THEN COALESCE(con.nspname::pg_catalog.text, '') ELSE '' END,
	CASE WHEN t.typcollation <> bt.typcollation THEN COALESCE(co.collname::pg_catalog.text, '') ELSE '' END,
	dc.names, dc.kinds, dc.defs, dc.validated,
	t.typacl IS NULL,
	` + aclColumns("p") + `,
	COALESCE(ds.description, '')
FROM pg_catalog.pg_type t
JOIN pg_catalog.pg_namespace n ON n.oid = t.typnamespace
JOIN pg_catalog.pg_type bt ON bt.oid = t.typbasetype
JOIN pg_catalog.pg_namespace btn ON btn.oid = bt.typnamespace
CROSS JOIN LATERAL (SELECT pg_catalog.format_type(t.typbasetype, t.typtypmod) AS name) AS ft
LEFT JOIN pg_catalog.pg_collation co ON co.oid = t.typcollation
LEFT JOIN pg_catalog.pg_namespace con ON con.oid = co.collnamespace
` + domainConstraints + `
` + originJoin(catalogType, "t.oid") + `
LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = t.oid
	AND ds.classoid = 'pg_catalog.pg_type'::pg_catalog.regclass AND ds.objsubid = 0
` + aclLateral("COALESCE(t.typacl, pg_catalog.acldefault('T', t.typowner))", "p") + `
WHERE t.typtype = 'd'`

// scanDomain builds a *Domain of d from one domainSelect row.
func scanDomain(d *Database, scan func(...any) error) (*Domain, error) {
	dm := &Domain{db: d}
	var names, kinds, defs []string
	var validated []bool
	var acl privilegeArrays
	dest := []any{&dm.OID, &dm.Schema, &dm.Name, &dm.Owner}
	dest = append(dest, scanOrigin(&dm.Origin)...)
	dest = append(dest, &dm.BaseTypeOID, &dm.BaseType, &dm.NotNull, &dm.Default, &dm.Collation.Schema, &dm.Collation.Name,
		&names, &kinds, &defs, &validated, &dm.PrivilegesDefault)
	dest = append(dest, acl.dest()...)
	dest = append(dest, &dm.Comment)
	if err := scan(dest...); err != nil {
		return nil, err
	}
	n := len(names)
	if len(kinds) != n || len(defs) != n || len(validated) != n {
		return nil, fmt.Errorf("constraint arrays of different lengths: %d names, %d kinds, %d definitions, %d validated",
			n, len(kinds), len(defs), len(validated))
	}
	for i := range n {
		dm.Constraints = append(dm.Constraints, DomainConstraint{Name: names[i], Kind: ConstraintKind(kinds[i]),
			Definition: defs[i], Validated: validated[i]})
	}
	var err error
	dm.Privileges, err = acl.all()
	return dm, err
}

// Domains lists the database's domains by schema then name, narrowed by f:
// Name on the domain's name, Schema on its schema's, System for those in a
// system schema (information_schema's), ExtensionMember for those an
// extension owns. Every domain is listed whatever the role's privileges on
// it. They are read through the database's own pool.
func (d *Database) Domains(ctx context.Context, f ObjectFilter) ([]*Domain, error) {
	where, args, err := f.clause(typeFilterColumns, nil)
	if err != nil {
		return nil, err
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, fmt.Sprintf("list domains of database %q", d.Name), func(scan func(...any) error) (*Domain, error) {
		return scanDomain(d, scan)
	}, domainSelect+where+`
ORDER BY n.nspname, t.typname`, args...)
}

// DomainByName reads the domain schema.name — stored names, matched exactly
// (fold typed input with pgsql.FoldIdent first). schema is required
// (ErrSchemaRequired). A name the database holds no domain by — none, or a
// type of another kind — is ErrNotFound. Use DomainRef instead when only a
// handle to write through is needed.
func (d *Database) DomainByName(ctx context.Context, schema, name string) (*Domain, error) {
	qualified := pgsql.QualifiedName(schema, name)
	if schema == "" {
		return nil, fmt.Errorf("pgmo: read domain %q: %w", name, ErrSchemaRequired)
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return readByName(ctx, q, d, scanDomain, domainSelect+`
	AND n.nspname = $1::pg_catalog.text AND t.typname = $2::pg_catalog.text`, []any{schema, name},
		notFoundf("pgmo: domain %s not found in database %q", qualified, d.Name),
		fmt.Sprintf("read domain %s of database %q", qualified, d.Name))
}

// DomainRef returns a handle for the domain schema.name without reading the
// catalog: Schema and Name are set and every other field is zero, OID
// included. It is not interchangeable with DomainByName.
func (d *Database) DomainRef(schema, name string) *Domain {
	return &Domain{db: d, Schema: schema, Name: name}
}
