package pgmo

import (
	"context"
	"fmt"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Extension writes
// ============================================================
//
// extension_write.go is P4.13.3's extensions: CREATE EXTENSION, ALTER
// EXTENSION … UPDATE / SET SCHEMA, DROP EXTENSION (CASCADE opt-in), COMMENT
// ON EXTENSION. An extension's name is database-wide, not in a schema; the
// schema is where it puts its objects. Every statement may run inside
// InTransaction.

// CreateExtensionRequest describes an extension to install.
type CreateExtensionRequest struct {
	// Name is the extension, as AvailableExtensions lists it.
	Name string

	// Schema is SCHEMA: where a relocatable extension's objects go; "" is
	// the first schema of the session's search_path, or the one the
	// extension's control file fixes.
	Schema string

	// Version is VERSION; "" for the default version.
	Version string

	// Cascade is CASCADE: the extensions it requires are installed first
	// when missing.
	Cascade bool
}

// CreateExtension installs the extension req describes (CREATE EXTENSION),
// and returns it read back from the catalog — or, under Scripting(ctx), the
// ExtensionRef handle. An untrusted extension needs superuser; a trusted
// one CREATE on the database.
func (d *Database) CreateExtension(ctx context.Context, req CreateExtensionRequest) (*Extension, error) {
	if req.Name == "" {
		return nil, invalidf("pgmo: create extension: no name")
	}
	stmt := "CREATE EXTENSION " + pgsql.QuoteIdent(req.Name)
	if req.Schema != "" {
		stmt += " SCHEMA " + pgsql.QuoteIdent(req.Schema)
	}
	if req.Version != "" {
		stmt += " VERSION " + pgsql.QuoteLiteral(req.Version)
	}
	if req.Cascade {
		stmt += " CASCADE"
	}
	if err := d.exec(ctx, stmt); err != nil {
		return nil, fmt.Errorf("pgmo: create extension %s in database %q: %w", pgsql.QuoteIdent(req.Name), d.Name, err)
	}
	return createdObject(ctx, d.ExtensionRef(req.Name), func() (*Extension, error) {
		return d.ExtensionByName(ctx, req.Name)
	})
}

func (e *Extension) ddl(verb string) (ddlObject, error) {
	if e.Name == "" {
		return ddlObject{}, invalidf("pgmo: %s extension: no name", verb)
	}
	return ddlObject{keyword: "EXTENSION", name: pgsql.QuoteIdent(e.Name), run: e.db.exec}, nil
}

// Update updates the extension to version (ALTER EXTENSION … UPDATE TO), ""
// for its default version, through the update scripts the server finds.
// Version is mirrored (not under WithScript) when version is given; read
// the extension again otherwise.
func (e *Extension) Update(ctx context.Context, version string) error {
	o, err := e.ddl("update")
	if err != nil {
		return err
	}
	stmt := "ALTER EXTENSION " + o.name + " UPDATE"
	if version != "" {
		stmt += " TO " + pgsql.QuoteLiteral(version)
	}
	if err := o.do(ctx, "update", stmt); err != nil {
		return err
	}
	if version != "" {
		setIfApplied(ctx, &e.Version, version)
	}
	return nil
}

// Drop drops the extension and every object it installed (DROP EXTENSION),
// with CASCADE when opts.Cascade — which drops what depends on them too (a
// column of its type, an extension requiring it).
func (e *Extension) Drop(ctx context.Context, opts DropOptions) error {
	return dropObject(ctx, e.ddl, opts)
}

// SetSchema moves a relocatable extension's objects to schema (ALTER
// EXTENSION … SET SCHEMA), mirrored onto Schema (not under WithScript). The
// server refuses one that is not relocatable (0A000).
func (e *Extension) SetSchema(ctx context.Context, schema string) error {
	return moveObject(ctx, e.ddl, schema, &e.Schema)
}

// SetComment sets the extension's comment (COMMENT ON EXTENSION), ""
// removing it, mirrored onto Comment (not under WithScript).
func (e *Extension) SetComment(ctx context.Context, text string) error {
	return commentObject(ctx, e.ddl, text, &e.Comment)
}
