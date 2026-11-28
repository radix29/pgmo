package pgmo

import (
	"context"
	"fmt"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Schema writes
// ============================================================
//
// schema_write.go is P4.13.3's schemas: CREATE SCHEMA, DROP SCHEMA (CASCADE
// opt-in), ALTER SCHEMA … RENAME TO / OWNER TO, COMMENT ON SCHEMA — each on
// the database's own session, each possible inside InTransaction.

// CreateSchemaRequest describes a new schema.
type CreateSchemaRequest struct {
	Name string
	// Owner is AUTHORIZATION: the owning role; "" is the connected role.
	// The connected role needs to be a member of it.
	Owner string
}

// CreateSchema creates the schema req describes (CREATE SCHEMA … [AUTHORIZATION
// owner]), and returns it read back from the catalog — or, under
// Scripting(ctx), the SchemaRef handle. The role needs CREATE on the
// database.
func (d *Database) CreateSchema(ctx context.Context, req CreateSchemaRequest) (*Schema, error) {
	if req.Name == "" {
		return nil, invalidf("pgmo: create schema: no name")
	}
	stmt := "CREATE SCHEMA " + pgsql.QuoteIdent(req.Name)
	if req.Owner != "" {
		stmt += " AUTHORIZATION " + pgsql.QuoteIdent(req.Owner)
	}
	if err := d.exec(ctx, stmt); err != nil {
		return nil, fmt.Errorf("pgmo: create schema %s in database %q: %w", pgsql.QuoteIdent(req.Name), d.Name, err)
	}
	return createdObject(ctx, d.SchemaRef(req.Name), func() (*Schema, error) {
		return d.SchemaByName(ctx, req.Name)
	})
}

// ddl is the schema's DDL handle for verb, or the refusal of one with no
// name.
func (s *Schema) ddl(verb string) (ddlObject, error) {
	if s.Name == "" {
		return ddlObject{}, invalidf("pgmo: %s schema: no name", verb)
	}
	return ddlObject{keyword: "SCHEMA", name: pgsql.QuoteIdent(s.Name), run: s.db.exec}, nil
}

// Drop drops the schema: DROP SCHEMA, with CASCADE when opts.Cascade — which
// drops every object in it. Without it the server refuses a schema that is
// not empty (2BP01).
func (s *Schema) Drop(ctx context.Context, opts DropOptions) error {
	return dropObject(ctx, s.ddl, opts)
}

// Rename renames the schema (ALTER SCHEMA … RENAME TO), mirrored onto Name
// (not under WithScript). A name starting pg_ is the system's, and the
// server refuses it (42939).
func (s *Schema) Rename(ctx context.Context, newName string) error {
	return renameObject(ctx, s.ddl, newName, &s.Name)
}

// SetOwner makes role the schema's owner (ALTER SCHEMA … OWNER TO),
// mirrored onto Owner (not under WithScript). The objects in it keep their
// own owners.
func (s *Schema) SetOwner(ctx context.Context, role string) error {
	return ownObject(ctx, s.ddl, role, &s.Owner)
}

// SetComment sets the schema's comment (COMMENT ON SCHEMA), "" removing it,
// mirrored onto Comment (not under WithScript).
func (s *Schema) SetComment(ctx context.Context, text string) error {
	return commentObject(ctx, s.ddl, text, &s.Comment)
}
