package pgmo

import (
	"context"
	"fmt"
	"strings"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Row-level security writes
// ============================================================
//
// policy_write.go is P4.13.4's row-level security: ALTER TABLE … ENABLE /
// DISABLE and FORCE / NO FORCE ROW LEVEL SECURITY, CREATE POLICY from a
// request, ALTER POLICY (roles, expressions, RENAME TO), DROP POLICY and
// COMMENT ON POLICY — each on the database's own session, each possible
// inside InTransaction. Row-level security is a table's (a partitioned
// table's too); a view or a foreign table has none (42809, the server's).

// SetRowSecurity enables (ALTER TABLE … ENABLE ROW LEVEL SECURITY) or
// disables row-level security on the table, mirrored onto RowSecurity (not
// under WithScript). Enabled with no policy, the table shows no rows to a
// role other than its owner — a default deny. The policies are kept either
// way.
func (t *Table) SetRowSecurity(ctx context.Context, enabled bool) error {
	action := "DISABLE ROW LEVEL SECURITY"
	if enabled {
		action = "ENABLE ROW LEVEL SECURITY"
	}
	if err := t.alter(ctx, "set row level security of", fixed(action)); err != nil {
		return err
	}
	setIfApplied(ctx, &t.RowSecurity, enabled)
	return nil
}

// SetForceRowSecurity sets whether the policies apply to the table's owner
// too (FORCE ROW LEVEL SECURITY) or not (NO FORCE), mirrored onto
// ForceRowSecurity (not under WithScript). A superuser and a role with
// BYPASSRLS are exempt either way. It has effect only while RowSecurity is
// on.
func (t *Table) SetForceRowSecurity(ctx context.Context, force bool) error {
	action := "NO FORCE ROW LEVEL SECURITY"
	if force {
		action = "FORCE ROW LEVEL SECURITY"
	}
	if err := t.alter(ctx, "set forced row level security of", fixed(action)); err != nil {
		return err
	}
	setIfApplied(ctx, &t.ForceRowSecurity, force)
	return nil
}

// CreatePolicyRequest describes a new policy on the table Schema.Table.
type CreatePolicyRequest struct {
	Schema, Table, Name string

	// Restrictive is AS RESTRICTIVE: every restrictive policy must pass as
	// well as one permissive policy. false is AS PERMISSIVE, the default.
	Restrictive bool

	// Command is FOR: the statements the policy applies to; "" is the
	// server's default, PolicyAll.
	Command PolicyCommand

	// Roles are TO: the roles it applies to, "" for PUBLIC; nil is PUBLIC.
	Roles []string

	// Using is USING's expression as SQL — which existing rows are visible
	// or affected — and WithCheck WITH CHECK's: which new rows are allowed.
	// "" for none. The server refuses USING on an INSERT policy and WITH
	// CHECK on a SELECT or DELETE one (42601).
	Using     string
	WithCheck string
}

// createPolicyStatement is req's CREATE POLICY, or the refusal of one that
// cannot be written. No prefix: the caller names the operation.
func createPolicyStatement(req CreatePolicyRequest) (string, error) {
	var b strings.Builder
	b.WriteString("CREATE POLICY " + pgsql.QuoteIdent(req.Name) + " ON " + pgsql.QualifiedName(req.Schema, req.Table))
	if req.Restrictive {
		b.WriteString(" AS RESTRICTIVE")
	}
	if req.Command != "" {
		cmd, err := policyCommandKeyword(req.Command)
		if err != nil {
			return "", err
		}
		b.WriteString(" FOR " + cmd)
	}
	if req.Roles != nil {
		if len(req.Roles) == 0 {
			return "", invalidf("an empty role list (nil is PUBLIC)")
		}
		b.WriteString(" TO " + roleSpecs(req.Roles))
	}
	if req.Using != "" {
		b.WriteString(" USING (" + req.Using + ")")
	}
	if req.WithCheck != "" {
		b.WriteString(" WITH CHECK (" + req.WithCheck + ")")
	}
	return b.String(), nil
}

// policyCommandKeyword is c as FOR spells it, or the refusal of a code
// pgmo does not know.
func policyCommandKeyword(c PolicyCommand) (string, error) {
	switch c {
	case PolicyAll, PolicySelect, PolicyInsert, PolicyUpdate, PolicyDelete:
		return c.String(), nil
	}
	return "", invalidf("unknown command %q", string(c))
}

// requirePolicy refuses a policy without a name, a table or a schema.
func requirePolicy(op, schema, table, name string) error {
	if name == "" {
		return invalidf("pgmo: %s: no name", op)
	}
	return requireQualified(op+" "+pgsql.QuoteIdent(name)+" on table", schema, table)
}

// CreatePolicy creates the policy req describes, and returns it read back
// from the catalog — or, under Scripting(ctx), the PolicyRef handle. The
// role must own the table. A policy applies only while the table's
// RowSecurity is on (Table.SetRowSecurity).
func (d *Database) CreatePolicy(ctx context.Context, req CreatePolicyRequest) (*Policy, error) {
	if err := requirePolicy("create policy", req.Schema, req.Table, req.Name); err != nil {
		return nil, err
	}
	op := "create policy " + pgsql.QuoteIdent(req.Name) + " on " + pgsql.QualifiedName(req.Schema, req.Table)
	stmt, err := createPolicyStatement(req)
	if err != nil {
		return nil, fmt.Errorf("pgmo: %s: %w", op, err)
	}
	if err := d.exec(ctx, stmt); err != nil {
		return nil, fmt.Errorf("pgmo: %s: %w", op, err)
	}
	return createdObject(ctx, d.PolicyRef(req.Schema, req.Table, req.Name), func() (*Policy, error) {
		return d.PolicyByName(ctx, req.Schema, req.Table, req.Name)
	})
}

// ddl is the policy as DROP, ALTER and COMMENT ON name it: name ON table.
func (p *Policy) ddl(verb string) (ddlObject, error) {
	if err := requirePolicy(verb+" policy", p.Schema, p.Table, p.Name); err != nil {
		return ddlObject{}, err
	}
	return ddlObject{keyword: "POLICY", name: pgsql.QuoteIdent(p.Name) + " ON " + p.QualifiedTable(), run: p.db.exec}, nil
}

// Drop drops the policy (DROP POLICY … ON). Nothing depends on a policy,
// so there is no CASCADE.
func (p *Policy) Drop(ctx context.Context) error {
	return dropObject(ctx, p.ddl, DropOptions{})
}

// Rename renames the policy (ALTER POLICY … ON … RENAME TO), mirrored onto
// Name (not under WithScript).
func (p *Policy) Rename(ctx context.Context, newName string) error {
	return renameObject(ctx, p.ddl, newName, &p.Name)
}

// SetComment sets the policy's comment (COMMENT ON POLICY … ON), "" removing
// it, mirrored onto Comment (not under WithScript).
func (p *Policy) SetComment(ctx context.Context, text string) error {
	return commentObject(ctx, p.ddl, text, &p.Comment)
}

// PolicyChanges are the parts of a policy ALTER POLICY can change; a nil
// field is left as it is. The command and AS RESTRICTIVE cannot be changed:
// drop the policy and create it again.
type PolicyChanges struct {
	// Roles is TO: the roles the policy applies to, "" for PUBLIC.
	Roles []string

	// Using and WithCheck replace the USING and WITH CHECK expressions, as
	// SQL. ALTER POLICY cannot remove one; "" is refused.
	Using     *string
	WithCheck *string
}

// Alter applies every field set on c in one ALTER POLICY … ON. Nothing is
// mirrored: the server renders the expressions its own way, and a role list
// holding PUBLIC is stored as PUBLIC alone — read the policy again. An empty
// c sends nothing.
func (p *Policy) Alter(ctx context.Context, c PolicyChanges) error {
	if c.Roles == nil && c.Using == nil && c.WithCheck == nil {
		return nil
	}
	o, err := p.ddl("alter")
	if err != nil {
		return err
	}
	stmt := "ALTER POLICY " + o.name
	if c.Roles != nil {
		if len(c.Roles) == 0 {
			return invalidf("pgmo: %s: an empty role list", o.op("alter"))
		}
		stmt += " TO " + roleSpecs(c.Roles)
	}
	if c.Using != nil {
		if *c.Using == "" {
			return invalidf("pgmo: %s: an empty USING expression (ALTER POLICY cannot remove one)", o.op("alter"))
		}
		stmt += " USING (" + *c.Using + ")"
	}
	if c.WithCheck != nil {
		if *c.WithCheck == "" {
			return invalidf("pgmo: %s: an empty WITH CHECK expression (ALTER POLICY cannot remove one)", o.op("alter"))
		}
		stmt += " WITH CHECK (" + *c.WithCheck + ")"
	}
	return o.do(ctx, "alter", stmt)
}
