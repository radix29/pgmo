package pgmo

import (
	"context"
	"fmt"
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

// Policy is one row-level security policy of a table.
type Policy struct {
	OID  OID
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

// policiesQuery reads the policies of the table $1 by name.
const policiesQuery = `SELECT po.oid, po.polname::pg_catalog.text, po.polcmd::pg_catalog.text, po.polpermissive,
	ARRAY(SELECT CASE WHEN r.oid = 0 THEN '' ELSE pg_catalog.pg_get_userbyid(r.oid)::pg_catalog.text END
		FROM pg_catalog.unnest(po.polroles) WITH ORDINALITY AS r(oid, n) ORDER BY r.n),
	COALESCE(pg_catalog.pg_get_expr(po.polqual, po.polrelid), ''),
	COALESCE(pg_catalog.pg_get_expr(po.polwithcheck, po.polrelid), ''),
	COALESCE(pg_catalog.obj_description(po.oid, 'pg_policy'), '')
FROM pg_catalog.pg_policy po
WHERE po.polrelid = $1
ORDER BY po.polname`

// scanPolicy builds a *Policy from one policiesQuery row.
func scanPolicy(scan func(...any) error) (*Policy, error) {
	p := &Policy{}
	var cmd string
	if err := scan(&p.OID, &p.Name, &cmd, &p.Permissive, &p.Roles, &p.Using, &p.WithCheck, &p.Comment); err != nil {
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
	return scanRows(ctx, q, "list policies of table "+t.QualifiedName(), scanPolicy, policiesQuery, t.OID)
}
