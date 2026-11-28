package pgmo

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// View and materialized view writes
// ============================================================
//
// view_write.go is P4.13.3's views and materialized views: CREATE [OR
// REPLACE] VIEW, CREATE MATERIALIZED VIEW, REFRESH MATERIALIZED VIEW, DROP
// (CASCADE opt-in), ALTER … RENAME TO / SET SCHEMA / OWNER TO, COMMENT ON.
// The query is SQL the caller writes, and is written as given.

// CreateViewRequest describes a new view, or — with OrReplace — the new
// definition of an existing one.
type CreateViewRequest struct {
	Schema, Name string

	// Query is the view's SELECT (or VALUES) as SQL. Names in it resolve
	// through the creating session's search_path once, when the view is
	// created; the view stores what they found.
	Query string

	// Columns name the view's columns, in order; nil takes the query's.
	Columns []string

	// OrReplace is OR REPLACE: an existing view of the name is redefined —
	// the new query must keep its columns, in order, of the same types,
	// and may add more at the end.
	OrReplace bool

	// CheckOption is WITH LOCAL or CASCADED CHECK OPTION: an insert or
	// update through the view must produce a row it shows.
	CheckOption ViewCheckOption

	// SecurityBarrier and SecurityInvoker are the security_barrier and
	// security_invoker options, written when set: a barrier keeps the
	// planner from pushing a caller's leaky function below the view's
	// conditions; an invoker view checks the caller's privileges on the
	// tables it reads, not its owner's.
	SecurityBarrier *bool
	SecurityInvoker *bool
}

// createViewStatement is req's CREATE VIEW, or the refusal of one that
// cannot be written. No prefix: the caller names the operation.
func createViewStatement(req CreateViewRequest) (string, error) {
	if req.Query == "" {
		return "", invalidf("no query")
	}
	if err := requireNames("the columns", req.Columns); err != nil {
		return "", err
	}
	var b strings.Builder
	b.WriteString("CREATE ")
	if req.OrReplace {
		b.WriteString("OR REPLACE ")
	}
	b.WriteString("VIEW " + pgsql.QualifiedName(req.Schema, req.Name))
	if len(req.Columns) > 0 {
		b.WriteString(" (" + identList(req.Columns) + ")")
	}
	var opts []string
	if req.SecurityBarrier != nil {
		opts = append(opts, "security_barrier = "+strconv.FormatBool(*req.SecurityBarrier))
	}
	if req.SecurityInvoker != nil {
		opts = append(opts, "security_invoker = "+strconv.FormatBool(*req.SecurityInvoker))
	}
	if len(opts) > 0 {
		b.WriteString(" WITH (" + strings.Join(opts, ", ") + ")")
	}
	b.WriteString(" AS " + req.Query)
	switch req.CheckOption {
	case CheckOptionNone:
	case CheckOptionLocal, CheckOptionCascaded:
		b.WriteString(" WITH " + req.CheckOption.String() + " CHECK OPTION")
	default:
		return "", invalidf("unknown check option %q", string(req.CheckOption))
	}
	return b.String(), nil
}

// CreateView creates the view req describes, and returns it read back from
// the catalog — or, under Scripting(ctx), the ViewRef handle. The role needs
// CREATE on the schema and the privileges the query needs.
func (d *Database) CreateView(ctx context.Context, req CreateViewRequest) (*View, error) {
	if err := requireQualified("create view", req.Schema, req.Name); err != nil {
		return nil, err
	}
	op := "create view " + pgsql.QualifiedName(req.Schema, req.Name)
	stmt, err := createViewStatement(req)
	if err != nil {
		return nil, fmt.Errorf("pgmo: %s: %w", op, err)
	}
	if err := d.exec(ctx, stmt); err != nil {
		return nil, fmt.Errorf("pgmo: %s: %w", op, err)
	}
	return createdObject(ctx, d.ViewRef(req.Schema, req.Name), func() (*View, error) {
		return d.ViewByName(ctx, req.Schema, req.Name)
	})
}

func (v *View) ddl(verb string) (ddlObject, error) {
	return v.db.qualified(verb, "VIEW", v.Schema, v.Name)
}

// Drop drops the view (DROP VIEW), with CASCADE when opts.Cascade — which
// drops the views over it.
func (v *View) Drop(ctx context.Context, opts DropOptions) error {
	return dropObject(ctx, v.ddl, opts)
}

// Rename renames the view (ALTER VIEW … RENAME TO), mirrored onto Name (not
// under WithScript).
func (v *View) Rename(ctx context.Context, newName string) error {
	return renameObject(ctx, v.ddl, newName, &v.Name)
}

// SetSchema moves the view to schema (ALTER VIEW … SET SCHEMA), mirrored
// onto Schema (not under WithScript).
func (v *View) SetSchema(ctx context.Context, schema string) error {
	return moveObject(ctx, v.ddl, schema, &v.Schema)
}

// SetOwner makes role the view's owner (ALTER VIEW … OWNER TO), mirrored
// onto Owner (not under WithScript). The view reads its tables with its
// owner's privileges unless it is a security_invoker one.
func (v *View) SetOwner(ctx context.Context, role string) error {
	return ownObject(ctx, v.ddl, role, &v.Owner)
}

// SetComment sets the view's comment (COMMENT ON VIEW), "" removing it,
// mirrored onto Comment (not under WithScript).
func (v *View) SetComment(ctx context.Context, text string) error {
	return commentObject(ctx, v.ddl, text, &v.Comment)
}

// ------------------------------------------------------------
// Materialized views
// ------------------------------------------------------------

// CreateMaterializedViewRequest describes a new materialized view.
type CreateMaterializedViewRequest struct {
	Schema, Name string

	// Query is the view's query as SQL, as CreateViewRequest.Query.
	Query string

	// Columns name its columns, in order; nil takes the query's.
	Columns []string

	// NoData is WITH NO DATA: created empty and unreadable until
	// refreshed, instead of running the query now.
	NoData bool

	// Tablespace is TABLESPACE; "" is the database's default.
	Tablespace string

	// Options are the storage parameters (WITH (fillfactor = '90', …)).
	Options []ParameterSetting
}

// createMaterializedViewStatement is req's CREATE MATERIALIZED VIEW, or the
// refusal of one that cannot be written. No prefix: the caller names the
// operation.
func createMaterializedViewStatement(req CreateMaterializedViewRequest) (string, error) {
	if req.Query == "" {
		return "", invalidf("no query")
	}
	if err := requireNames("the columns", req.Columns); err != nil {
		return "", err
	}
	opts, err := storageParameters(req.Options)
	if err != nil {
		return "", err
	}
	stmt := "CREATE MATERIALIZED VIEW " + pgsql.QualifiedName(req.Schema, req.Name)
	if len(req.Columns) > 0 {
		stmt += " (" + identList(req.Columns) + ")"
	}
	if opts != "" {
		stmt += " WITH " + opts
	}
	if req.Tablespace != "" {
		stmt += " TABLESPACE " + pgsql.QuoteIdent(req.Tablespace)
	}
	stmt += " AS " + req.Query
	if req.NoData {
		stmt += " WITH NO DATA"
	}
	return stmt, nil
}

// CreateMaterializedView creates the materialized view req describes — and,
// unless NoData, runs its query to fill it — and returns it read back from
// the catalog, or under Scripting(ctx) the MaterializedViewRef handle.
func (d *Database) CreateMaterializedView(ctx context.Context, req CreateMaterializedViewRequest) (*MaterializedView, error) {
	if err := requireQualified("create materialized view", req.Schema, req.Name); err != nil {
		return nil, err
	}
	op := "create materialized view " + pgsql.QualifiedName(req.Schema, req.Name)
	stmt, err := createMaterializedViewStatement(req)
	if err != nil {
		return nil, fmt.Errorf("pgmo: %s: %w", op, err)
	}
	if err := d.exec(ctx, stmt); err != nil {
		return nil, fmt.Errorf("pgmo: %s: %w", op, err)
	}
	return createdObject(ctx, d.MaterializedViewRef(req.Schema, req.Name), func() (*MaterializedView, error) {
		return d.MaterializedViewByName(ctx, req.Schema, req.Name)
	})
}

func (m *MaterializedView) ddl(verb string) (ddlObject, error) {
	return m.db.qualified(verb, "MATERIALIZED VIEW", m.Schema, m.Name)
}

// RefreshMaterializedViewOptions are the choices of Refresh.
type RefreshMaterializedViewOptions struct {
	// Concurrently is CONCURRENTLY: the view stays readable during the
	// refresh, which needs a unique index on plain columns covering every
	// row and a populated view.
	Concurrently bool
	// NoData is WITH NO DATA: the view is emptied and unreadable until the
	// next refresh. Not with Concurrently.
	NoData bool
}

// Refresh runs the materialized view's query again and replaces its
// contents (REFRESH MATERIALIZED VIEW), mirrored onto Populated (not under
// WithScript). It may run inside InTransaction, CONCURRENTLY included.
func (m *MaterializedView) Refresh(ctx context.Context, opts RefreshMaterializedViewOptions) error {
	o, err := m.ddl("refresh")
	if err != nil {
		return err
	}
	if opts.Concurrently && opts.NoData {
		return invalidf("pgmo: %s: CONCURRENTLY and WITH NO DATA together; the server takes only one", o.op("refresh"))
	}
	stmt := "REFRESH MATERIALIZED VIEW "
	if opts.Concurrently {
		stmt += "CONCURRENTLY "
	}
	stmt += o.name
	if opts.NoData {
		stmt += " WITH NO DATA"
	}
	if err := o.do(ctx, "refresh", stmt); err != nil {
		return err
	}
	setIfApplied(ctx, &m.Populated, !opts.NoData)
	return nil
}

// Drop drops the materialized view (DROP MATERIALIZED VIEW), with CASCADE
// when opts.Cascade.
func (m *MaterializedView) Drop(ctx context.Context, opts DropOptions) error {
	return dropObject(ctx, m.ddl, opts)
}

// Rename renames the materialized view (ALTER MATERIALIZED VIEW … RENAME
// TO), mirrored onto Name (not under WithScript).
func (m *MaterializedView) Rename(ctx context.Context, newName string) error {
	return renameObject(ctx, m.ddl, newName, &m.Name)
}

// SetSchema moves the materialized view to schema, mirrored onto Schema
// (not under WithScript).
func (m *MaterializedView) SetSchema(ctx context.Context, schema string) error {
	return moveObject(ctx, m.ddl, schema, &m.Schema)
}

// SetOwner makes role the materialized view's owner, mirrored onto Owner
// (not under WithScript).
func (m *MaterializedView) SetOwner(ctx context.Context, role string) error {
	return ownObject(ctx, m.ddl, role, &m.Owner)
}

// SetComment sets the materialized view's comment, "" removing it,
// mirrored onto Comment (not under WithScript).
func (m *MaterializedView) SetComment(ctx context.Context, text string) error {
	return commentObject(ctx, m.ddl, text, &m.Comment)
}
