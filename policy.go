package pgmo

import (
	"context"
	"fmt"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Row-level security policies
// ============================================================
//
// policy.go reads a table's row-level security policies (PLAN.md P4.4) from
// pg_policy. Whether they apply is the table's: Table.RowSecurity and
// ForceRowSecurity. SQL Server's nearest is a security policy with
// predicate functions; PostgreSQL's is a pair of expressions on the table
// itself.

// PolicyCommand is pg_policy.polcmd: the statements a policy applies to.
type PolicyCommand string

const (
	PolicyAll    PolicyCommand = "*"
	PolicySelect PolicyCommand = "r"
	PolicyInsert PolicyCommand = "a"
	PolicyUpdate PolicyCommand = "w"
	PolicyDelete PolicyCommand = "d"
)

// String names the command as CREATE POLICY's FOR clause spells it: ALL,
// SELECT, INSERT, UPDATE, DELETE; an unknown code reads PolicyCommand("x").
func (c PolicyCommand) String() string {
	switch c {
	case PolicyAll:
		return "ALL"
	case PolicySelect:
		return "SELECT"
	case PolicyInsert:
		return "INSERT"
	case PolicyUpdate:
		return "UPDATE"
	case PolicyDelete:
		return "DELETE"
	}
	return fmt.Sprintf("PolicyCommand(%q)", string(c))
}

// Policy is one row-level security policy of a table, as Table.Policies or
// Database.PolicyByName read it; Database.PolicyRef is a handle holding the
// names alone. A policy is named by its name ON its table, and lives in its
// table's schema.
type Policy struct {
	db *Database

	OID OID

	// Schema and Table name the table the policy is on.
	Schema string
	Table  string

	Name string

	Command PolicyCommand

	// Permissive is AS PERMISSIVE (the default): permissive policies are
	// OR-ed together. false is AS RESTRICTIVE: each must also pass.
	Permissive bool

	// Roles are the roles the policy applies to (TO …), in stored order;
	// "" is PUBLIC, as on PrivilegeGrant.Grantee.
	Roles []string

	// Using is the USING expression, which rows are visible or affected;
	// WithCheck the WITH CHECK one, which new rows must pass. Empty for
	// none. Both as pg_get_expr renders them.
	Using     string
	WithCheck string

	// Comment is COMMENT ON POLICY's text, empty for none.
	Comment string
}

// Database is the database holding the policy's table.
func (p *Policy) Database() *Database { return p.db }

// QualifiedTable is the policy's table quoted for a statement: schema.table.
func (p *Policy) QualifiedTable() string { return pgsql.QualifiedName(p.Schema, p.Table) }

// policySelect reads policies with their table's names; a caller adds the
// WHERE clause and the order.
const policySelect = `SELECT po.oid, n.nspname::pg_catalog.text, c.relname::pg_catalog.text,
	po.polname::pg_catalog.text, po.polcmd::pg_catalog.text, po.polpermissive,
	ARRAY(SELECT CASE WHEN r.oid = 0 THEN '' ELSE pg_catalog.pg_get_userbyid(r.oid)::pg_catalog.text END
		FROM pg_catalog.unnest(po.polroles) WITH ORDINALITY AS r(oid, n) ORDER BY r.n),
	COALESCE(pg_catalog.pg_get_expr(po.polqual, po.polrelid), ''),
	COALESCE(pg_catalog.pg_get_expr(po.polwithcheck, po.polrelid), ''),
	COALESCE(pg_catalog.obj_description(po.oid, 'pg_policy'), '')
FROM pg_catalog.pg_policy po
JOIN pg_catalog.pg_class c ON c.oid = po.polrelid
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace`

// policiesQuery reads the policies of the table $1 by name.
const policiesQuery = policySelect + `
WHERE po.polrelid = $1
ORDER BY po.polname`

// scanPolicy builds a *Policy of database d from one policySelect row.
func scanPolicy(d *Database, scan func(...any) error) (*Policy, error) {
	p := &Policy{db: d}
	var cmd string
	if err := scan(&p.OID, &p.Schema, &p.Table, &p.Name, &cmd, &p.Permissive, &p.Roles, &p.Using, &p.WithCheck, &p.Comment); err != nil {
		return nil, err
	}
	p.Command = PolicyCommand(cmd)
	return p, nil
}

// Policies reads the table's row-level security policies by name, whether
// or not row-level security is enabled on it. A TableRef is refused
// (ErrHandleNotLoaded).
func (t *Table) Policies(ctx context.Context) ([]*Policy, error) {
	if err := t.requireLoaded("list policies of table"); err != nil {
		return nil, err
	}
	q, err := t.on(ctx)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, "list policies of table "+t.QualifiedName(), func(scan func(...any) error) (*Policy, error) {
		return scanPolicy(t.db, scan)
	}, policiesQuery, t.OID)
}

// PolicyByName reads the policy name on the table schema.table — stored
// names, matched exactly (fold typed input with pgsql.FoldIdent first).
// schema is required (ErrSchemaRequired). A policy the table does not have,
// or a table the database does not hold, is ErrNotFound. Use PolicyRef
// instead when only a handle to write through is needed.
func (d *Database) PolicyByName(ctx context.Context, schema, table, name string) (*Policy, error) {
	desc := fmt.Sprintf("%s on %s", pgsql.QuoteIdent(name), pgsql.QualifiedName(schema, table))
	if schema == "" {
		return nil, fmt.Errorf("pgmo: read policy %s: %w", desc, ErrSchemaRequired)
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return readByName(ctx, q, d, scanPolicy, policySelect+`
WHERE n.nspname = $1::pg_catalog.text AND c.relname = $2::pg_catalog.text AND po.polname = $3::pg_catalog.text`,
		[]any{schema, table, name},
		notFoundf("pgmo: policy %s not found in database %q", desc, d.Name),
		fmt.Sprintf("read policy %s of database %q", desc, d.Name))
}

// PolicyRef returns a handle for the policy name on the table schema.table
// without reading the catalog: Schema, Table and Name are set and every
// other field is zero, OID included. It is not interchangeable with
// PolicyByName.
func (d *Database) PolicyRef(schema, table, name string) *Policy {
	return &Policy{db: d, Schema: schema, Table: table, Name: name}
}
