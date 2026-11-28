package pgmo

import (
	"context"
	"fmt"
	"strings"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Tablespace writes
// ============================================================
//
// tablespace_write.go is P4.13.3's tablespaces: CREATE TABLESPACE, DROP
// TABLESPACE, ALTER TABLESPACE … RENAME TO / OWNER TO / SET (…) / RESET
// (…), COMMENT ON TABLESPACE. A tablespace is cluster-wide, so every
// statement runs on the Server's own database. CREATE and DROP TABLESPACE
// cannot run in a transaction block (25001, checked on 15 and 18) and are
// refused under InTransaction; the ALTERs and the comment can.

// CreateTablespaceRequest describes a new tablespace.
type CreateTablespaceRequest struct {
	Name string

	// Owner is OWNER; "" is the connected role.
	Owner string

	// Location is LOCATION: an absolute path on the server's host, an
	// existing empty directory the server's account owns. "" makes an
	// in-place tablespace under pg_tblspc, which the server takes only with
	// allow_in_place_tablespaces on — a developer's setting.
	Location string

	// Options are WITH's parameters: seq_page_cost, random_page_cost,
	// effective_io_concurrency, maintenance_io_concurrency.
	Options []ParameterSetting
}

// CreateTablespace creates the tablespace req describes, and returns it
// read back from the catalog — or, under Scripting(ctx), the TablespaceRef
// handle. It needs superuser, and cannot run in a transaction block: inside InTransaction it is refused
// with ErrUnsupported before anything is sent.
func (s *Server) CreateTablespace(ctx context.Context, req CreateTablespaceRequest) (*Tablespace, error) {
	if req.Name == "" {
		return nil, invalidf("pgmo: create tablespace: no name")
	}
	op := "create tablespace " + pgsql.QuoteIdent(req.Name)
	if err := refuseInTx(ctx, s, op); err != nil {
		return nil, err
	}
	opts, err := storageParameters(req.Options)
	if err != nil {
		return nil, fmt.Errorf("pgmo: %s: %w", op, err)
	}
	stmt := "CREATE TABLESPACE " + pgsql.QuoteIdent(req.Name)
	if req.Owner != "" {
		stmt += " OWNER " + pgsql.QuoteIdent(req.Owner)
	}
	stmt += " LOCATION " + pgsql.QuoteLiteral(req.Location)
	if opts != "" {
		stmt += " WITH " + opts
	}
	if _, err := s.exec(ctx, stmt); err != nil {
		return nil, fmt.Errorf("pgmo: %s: %w", op, err)
	}
	return createdObject(ctx, s.TablespaceRef(req.Name), func() (*Tablespace, error) {
		return s.TablespaceByName(ctx, req.Name)
	})
}

func (t *Tablespace) ddl(verb string) (ddlObject, error) {
	if t.Name == "" {
		return ddlObject{}, invalidf("pgmo: %s tablespace: no name", verb)
	}
	return ddlObject{keyword: "TABLESPACE", name: pgsql.QuoteIdent(t.Name), run: func(ctx context.Context, stmt string) error {
		_, err := t.server.exec(ctx, stmt)
		return err
	}}, nil
}

// Drop drops the tablespace (DROP TABLESPACE). The server refuses one any
// database still keeps an object in (55000) — every database's, which a
// session in one cannot see. It cannot run in a transaction block: inside
// InTransaction it is refused with ErrUnsupported.
func (t *Tablespace) Drop(ctx context.Context) error {
	o, err := t.ddl("drop")
	if err != nil {
		return err
	}
	if err := refuseInTx(ctx, t.server, o.op("drop")); err != nil {
		return err
	}
	return o.drop(ctx, DropOptions{})
}

// Rename renames the tablespace, mirrored onto Name (not under WithScript).
func (t *Tablespace) Rename(ctx context.Context, newName string) error {
	return renameObject(ctx, t.ddl, newName, &t.Name)
}

// SetOwner makes role the tablespace's owner, mirrored onto Owner (not
// under WithScript).
func (t *Tablespace) SetOwner(ctx context.Context, role string) error {
	return ownObject(ctx, t.ddl, role, &t.Owner)
}

// SetComment sets the tablespace's comment, "" removing it, mirrored onto
// Comment (not under WithScript).
func (t *Tablespace) SetComment(ctx context.Context, text string) error {
	return commentObject(ctx, t.ddl, text, &t.Comment)
}

// SetOptions sets the tablespace's parameters (ALTER TABLESPACE … SET (…)),
// each value a literal the server parses. Options is not mirrored: read the
// tablespace again.
func (t *Tablespace) SetOptions(ctx context.Context, options ...ParameterSetting) error {
	o, err := t.ddl("set options of")
	if err != nil {
		return err
	}
	if len(options) == 0 {
		return invalidf("pgmo: %s: no options", o.op("set options of"))
	}
	opts, err := storageParameters(options)
	if err != nil {
		return fmt.Errorf("pgmo: %s: %w", o.op("set options of"), err)
	}
	return o.do(ctx, "set options of", "ALTER TABLESPACE "+o.name+" SET "+opts)
}

// ResetOptions resets the tablespace's parameters names to their defaults
// (ALTER TABLESPACE … RESET (…)). Options is not mirrored.
func (t *Tablespace) ResetOptions(ctx context.Context, names ...string) error {
	o, err := t.ddl("reset options of")
	if err != nil {
		return err
	}
	if len(names) == 0 {
		return invalidf("pgmo: %s: no options", o.op("reset options of"))
	}
	quoted := make([]string, len(names))
	for i, n := range names {
		if quoted[i], err = storageParameterName(n); err != nil {
			return fmt.Errorf("pgmo: %s: %w", o.op("reset options of"), err)
		}
	}
	return o.do(ctx, "reset options of", "ALTER TABLESPACE "+o.name+" RESET ("+strings.Join(quoted, ", ")+")")
}
