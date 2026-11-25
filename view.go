package pgmo

import (
	"context"
	"fmt"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Views and materialized views
// ============================================================
//
// view.go reads a database's views and materialized views (PLAN.md P4.5):
// pg_class rows of kind v and m, through the database's own pool. They are
// two types, as CREATE VIEW and CREATE MATERIALIZED VIEW are two statements:
// a view stores no rows and carries its check option and security flags; a
// materialized view stores its query's result, so it has a populated flag,
// a tablespace, storage parameters, indexes, a row estimate and a size, as a
// table does. Both carry their query as pg_get_viewdef renders it, and their
// columns are read on demand as a table's are (columnsQuery), keyed by the
// OID — a Ref, having none, is refused (ErrHandleNotLoaded).
//
// What PostgreSQL adds over SQL Server: materialized views (an indexed
// view's nearest, but refreshed only by REFRESH MATERIALIZED VIEW, never
// kept in step), security_barrier and security_invoker. What it lacks:
// SCHEMABINDING (pg_depend binds every view to what it reads) and
// encrypted definitions.
//
// The definition names objects as the session's search_path sees them,
// as every expression the server renders does (table.go): one visible on
// the path is left unqualified (FROM t rather than FROM app.t — checked on
// pg15 and pg18). pgmo does not change the caller's path.

// ViewCheckOption is a view's WITH CHECK OPTION, the check_option
// storage parameter as stored: which rows an INSERT or UPDATE through the
// view may write.
type ViewCheckOption string

const (
	CheckOptionNone     ViewCheckOption = ""         // no check: a write may produce a row the view does not show
	CheckOptionLocal    ViewCheckOption = "local"    // the row must satisfy this view's own condition
	CheckOptionCascaded ViewCheckOption = "cascaded" // …and those of every view it is defined on
)

// String names the option as CREATE VIEW spells it: "NONE", "LOCAL",
// "CASCADED"; an unknown value reads ViewCheckOption("x").
func (o ViewCheckOption) String() string {
	switch o {
	case CheckOptionNone:
		return "NONE"
	case CheckOptionLocal:
		return "LOCAL"
	case CheckOptionCascaded:
		return "CASCADED"
	}
	return fmt.Sprintf("ViewCheckOption(%q)", string(o))
}

// View is one view of a database. Read it with Database.Views or
// Database.ViewByName; Database.ViewRef is a handle holding the name alone.
type View struct {
	db *Database

	// Origin: IsSystem for a view in a system schema (pg_catalog's,
	// information_schema's); Extension for one an extension owns.
	Origin

	OID    OID
	Schema string
	Name   string

	// Owner is the owning role's name (relowner).
	Owner string

	// Definition is the view's query as pg_get_viewdef renders it — the
	// SELECT, from a leading space to its closing semicolon, as CREATE
	// VIEW … AS takes it.
	Definition string

	// CheckOption is WITH [LOCAL | CASCADED] CHECK OPTION.
	CheckOption ViewCheckOption

	// SecurityBarrier is the security_barrier parameter: the view's
	// conditions run before any the caller adds, so a leaky function
	// cannot see the rows it hides.
	SecurityBarrier bool

	// SecurityInvoker is the security_invoker parameter: the tables
	// beneath are read with the caller's privileges (and row-level
	// security), not the view owner's.
	SecurityInvoker bool

	// Options are the view's parameters (reloptions) as stored, the three
	// above among them, as WITH (…) takes them; nil for none. A boolean is
	// kept as it was written ("on", "1"); the fields above are the
	// server's reading of it.
	Options []ParameterSetting

	// Privileges are the view's ACL, per grantee, in stored order — the
	// privileges of a table, which a view takes. One never set reads as
	// the hard-wired default, its owner's every privilege
	// (PrivilegesDefault). A column's own grants are on Column.Privileges.
	Privileges []PrivilegeGrant

	// PrivilegesDefault reports that the ACL was never set (relacl NULL).
	PrivilegesDefault bool

	// Comment is COMMENT ON VIEW's text, empty for none.
	Comment string
}

// MaterializedView is one materialized view of a database. Read it with
// Database.MaterializedViews or Database.MaterializedViewByName;
// Database.MaterializedViewRef is a handle holding the name alone.
type MaterializedView struct {
	db *Database

	// Origin: IsSystem for one in a system schema; Extension for one an
	// extension owns.
	Origin

	OID    OID
	Schema string
	Name   string

	// Owner is the owning role's name (relowner).
	Owner string

	// Definition is the query as pg_get_viewdef renders it, as View's.
	Definition string

	// Populated reports that the view holds its query's result
	// (relispopulated): false after CREATE … WITH NO DATA or REFRESH … WITH
	// NO DATA, when reading it is an error until it is refreshed.
	Populated bool

	// Tablespace is its own tablespace, empty for the database's default.
	Tablespace string

	// Options are the storage parameters (reloptions) as stored, then
	// those of its TOAST table with a "toast." prefix, as WITH (…) takes
	// them. nil for none.
	Options []ParameterSetting

	// RowEstimate is the planner's row count (reltuples), as of the last
	// VACUUM, ANALYZE, CREATE INDEX or populating REFRESH; -1 when none has
	// run.
	RowEstimate int64

	// Privileges are the ACL, per grantee, in stored order; one never set
	// reads as the owner's every privilege (PrivilegesDefault).
	Privileges []PrivilegeGrant

	// PrivilegesDefault reports that the ACL was never set (relacl NULL).
	PrivilegesDefault bool

	// Comment is COMMENT ON MATERIALIZED VIEW's text, empty for none.
	Comment string
}

// Database is the Database the view was read from.
func (v *View) Database() *Database { return v.db }

// QualifiedName is the view's name quoted for a statement: schema.name.
func (v *View) QualifiedName() string { return pgsql.QualifiedName(v.Schema, v.Name) }

// Database is the Database the materialized view was read from.
func (m *MaterializedView) Database() *Database { return m.db }

// QualifiedName is the materialized view's name quoted for a statement:
// schema.name.
func (m *MaterializedView) QualifiedName() string { return pgsql.QualifiedName(m.Schema, m.Name) }

// viewOption is the parameter name of the relation c read as typ, NULL when
// it is not set: the server's own reading of what it stored, so "on", "1"
// and "true" are the same boolean.
func viewOption(name, typ string) string {
	return `(SELECT o.option_value::` + typ + ` FROM pg_catalog.pg_options_to_table(c.reloptions) AS o
		WHERE o.option_name = '` + name + `')`
}

// relationHead is the start of a view's or materialized view's select
// list: identity, owner, origin and definition.
var relationHead = `SELECT c.oid, n.nspname::pg_catalog.text, c.relname::pg_catalog.text,
	pg_catalog.pg_get_userbyid(c.relowner)::pg_catalog.text,
	` + originColumns("n.nspname", catalogClass, "c.oid") + `,
	COALESCE(pg_catalog.pg_get_viewdef(c.oid), ''),`

// relationTail is the end of a view's or materialized view's select list —
// ACL and comment — and its FROM up to the joins each adds.
var relationTail = `
	c.relacl IS NULL,
	` + aclColumns("p") + `,
	COALESCE(pg_catalog.obj_description(c.oid, 'pg_class'), '')
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace`

// relationACL is the ACL lateral of the relation c.
var relationACL = aclLateral("COALESCE(c.relacl, pg_catalog.acldefault('r', c.relowner))", "p")

// viewSelect is the pg_class read a *View is built from, ending in its
// WHERE on the kind without an ORDER BY; scanView reads its row.
var viewSelect = relationHead + `
	COALESCE(` + viewOption("check_option", "pg_catalog.text") + `, ''),
	COALESCE(` + viewOption("security_barrier", "boolean") + `, false),
	COALESCE(` + viewOption("security_invoker", "boolean") + `, false),
	COALESCE(c.reloptions, '{}'),` + relationTail + `
` + relationACL + `
WHERE c.relkind = 'v'`

// materializedViewSelect is the pg_class read a *MaterializedView is built
// from, ending in its WHERE on the kind; scanMaterializedView reads its row.
var materializedViewSelect = relationHead + `
	c.relispopulated,
	COALESCE(ts.spcname::pg_catalog.text, ''),
	` + relationOptions + `,
	c.reltuples::bigint,` + relationTail + `
LEFT JOIN pg_catalog.pg_class tc ON tc.oid = c.reltoastrelid
LEFT JOIN pg_catalog.pg_tablespace ts ON ts.oid = c.reltablespace
` + relationACL + `
WHERE c.relkind = 'm'`

// scanView builds a *View of d from one viewSelect row.
func scanView(d *Database, scan func(...any) error) (*View, error) {
	v := &View{db: d}
	var check string
	var options []string
	var acl privilegeArrays
	dest := []any{&v.OID, &v.Schema, &v.Name, &v.Owner}
	dest = append(dest, scanOrigin(&v.Origin)...)
	dest = append(dest, &v.Definition, &check, &v.SecurityBarrier, &v.SecurityInvoker, &options, &v.PrivilegesDefault)
	dest = append(dest, acl.dest()...)
	dest = append(dest, &v.Comment)
	if err := scan(dest...); err != nil {
		return nil, err
	}
	v.CheckOption = ViewCheckOption(check)
	v.Options = parseSettings(options)
	var err error
	v.Privileges, err = acl.all()
	return v, err
}

// scanMaterializedView builds a *MaterializedView of d from one
// materializedViewSelect row.
func scanMaterializedView(d *Database, scan func(...any) error) (*MaterializedView, error) {
	m := &MaterializedView{db: d}
	var options []string
	var acl privilegeArrays
	dest := []any{&m.OID, &m.Schema, &m.Name, &m.Owner}
	dest = append(dest, scanOrigin(&m.Origin)...)
	dest = append(dest, &m.Definition, &m.Populated, &m.Tablespace, &options, &m.RowEstimate, &m.PrivilegesDefault)
	dest = append(dest, acl.dest()...)
	dest = append(dest, &m.Comment)
	if err := scan(dest...); err != nil {
		return nil, err
	}
	m.Options = parseSettings(options)
	var err error
	m.Privileges, err = acl.all()
	return m, err
}

// Views lists the database's views by schema then name, narrowed by f:
// Name on the view's name, Schema on its schema's, System for those in a
// system schema (pg_catalog's and information_schema's), ExtensionMember for
// those an extension owns. Every view is listed whatever the role's
// privileges on it. They are read through the database's own pool.
func (d *Database) Views(ctx context.Context, f ObjectFilter) ([]*View, error) {
	where, args, err := f.clause(relationFilterColumns, nil)
	if err != nil {
		return nil, err
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, fmt.Sprintf("list views of database %q", d.Name), func(scan func(...any) error) (*View, error) {
		return scanView(d, scan)
	}, viewSelect+where+`
ORDER BY n.nspname, c.relname`, args...)
}

// ViewByName reads the view schema.name — stored names, matched exactly
// (fold typed input with pgsql.FoldIdent first). schema is required
// (ErrSchemaRequired). A name the database holds no view by — none, or a
// table or materialized view — is ErrNotFound. Use ViewRef instead when
// only a handle to write through is needed.
func (d *Database) ViewByName(ctx context.Context, schema, name string) (*View, error) {
	qualified := pgsql.QualifiedName(schema, name)
	if schema == "" {
		return nil, fmt.Errorf("pgmo: read view %q: %w", name, ErrSchemaRequired)
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return readByName(ctx, q, d, scanView, viewSelect+`
	AND n.nspname = $1::pg_catalog.text AND c.relname = $2::pg_catalog.text`, []any{schema, name},
		notFoundf("pgmo: view %s not found in database %q", qualified, d.Name),
		fmt.Sprintf("read view %s of database %q", qualified, d.Name))
}

// ViewRef returns a handle for the view schema.name without reading the
// catalog: Schema and Name are set and every other field is zero, OID
// included — so Columns refuses it (ErrHandleNotLoaded). It is not
// interchangeable with ViewByName.
func (d *Database) ViewRef(schema, name string) *View {
	return &View{db: d, Schema: schema, Name: name}
}

// MaterializedViews lists the database's materialized views by schema then
// name, narrowed by f as Views is. They are read through the database's own
// pool.
func (d *Database) MaterializedViews(ctx context.Context, f ObjectFilter) ([]*MaterializedView, error) {
	where, args, err := f.clause(relationFilterColumns, nil)
	if err != nil {
		return nil, err
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, fmt.Sprintf("list materialized views of database %q", d.Name),
		func(scan func(...any) error) (*MaterializedView, error) {
			return scanMaterializedView(d, scan)
		}, materializedViewSelect+where+`
ORDER BY n.nspname, c.relname`, args...)
}

// MaterializedViewByName reads the materialized view schema.name — stored
// names, matched exactly. schema is required (ErrSchemaRequired). A name
// the database holds no materialized view by — none, or a table or view —
// is ErrNotFound.
func (d *Database) MaterializedViewByName(ctx context.Context, schema, name string) (*MaterializedView, error) {
	qualified := pgsql.QualifiedName(schema, name)
	if schema == "" {
		return nil, fmt.Errorf("pgmo: read materialized view %q: %w", name, ErrSchemaRequired)
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return readByName(ctx, q, d, scanMaterializedView, materializedViewSelect+`
	AND n.nspname = $1::pg_catalog.text AND c.relname = $2::pg_catalog.text`, []any{schema, name},
		notFoundf("pgmo: materialized view %s not found in database %q", qualified, d.Name),
		fmt.Sprintf("read materialized view %s of database %q", qualified, d.Name))
}

// MaterializedViewRef returns a handle for the materialized view
// schema.name without reading the catalog: Schema and Name are set and
// every other field is zero, OID included — so the reads of its parts
// refuse it (ErrHandleNotLoaded). It is not interchangeable with
// MaterializedViewByName.
func (d *Database) MaterializedViewRef(schema, name string) *MaterializedView {
	return &MaterializedView{db: d, Schema: schema, Name: name}
}

// ============================================================
// Parts
// ============================================================

// requireLoaded refuses a read keyed by the OID on a view that has none —
// a ViewRef.
func (v *View) requireLoaded(what string) error {
	if v.OID == 0 {
		return fmt.Errorf("pgmo: %s %s: %w", what, v.QualifiedName(), ErrHandleNotLoaded)
	}
	return nil
}

// requireLoaded refuses a read keyed by the OID on a materialized view that
// has none — a MaterializedViewRef.
func (m *MaterializedView) requireLoaded(what string) error {
	if m.OID == 0 {
		return fmt.Errorf("pgmo: %s %s: %w", what, m.QualifiedName(), ErrHandleNotLoaded)
	}
	return nil
}

// Columns reads the view's columns in attnum order, as a table's are read
// (Table.Columns): a view's column has a type, may have a DEFAULT (ALTER
// VIEW … ALTER COLUMN … SET DEFAULT, used by an INSERT through the view),
// grants and a comment; its Storage is its type's and means nothing. A
// ViewRef is refused (ErrHandleNotLoaded); a view dropped since it was read
// has none.
func (v *View) Columns(ctx context.Context) ([]*Column, error) {
	if err := v.requireLoaded("list columns of view"); err != nil {
		return nil, err
	}
	q, err := v.db.server.onDatabase(ctx, v.db.Name)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, "list columns of view "+v.QualifiedName(), scanColumn, columnsQuery, v.OID)
}

// Columns reads the materialized view's columns in attnum order, as a
// table's are read (Table.Columns). A MaterializedViewRef is refused
// (ErrHandleNotLoaded).
func (m *MaterializedView) Columns(ctx context.Context) ([]*Column, error) {
	if err := m.requireLoaded("list columns of materialized view"); err != nil {
		return nil, err
	}
	q, err := m.db.server.onDatabase(ctx, m.db.Name)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, "list columns of materialized view "+m.QualifiedName(), scanColumn, columnsQuery, m.OID)
}

// Indexes reads the materialized view's indexes by name, as a table's are
// read (Table.Indexes) — REFRESH … CONCURRENTLY needs a unique one. A
// MaterializedViewRef is refused (ErrHandleNotLoaded).
func (m *MaterializedView) Indexes(ctx context.Context) ([]*Index, error) {
	if err := m.requireLoaded("list indexes of materialized view"); err != nil {
		return nil, err
	}
	q, err := m.db.server.onDatabase(ctx, m.db.Name)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, "list indexes of materialized view "+m.QualifiedName(), scanIndex, indexesQuery, m.OID)
}

// Size reads the materialized view's size on disk, as a table's is read
// (Table.Size): its data with its TOAST table, its indexes, their total.
// Any role may read it; one dropped since it was read is ErrNotFound.
func (m *MaterializedView) Size(ctx context.Context) (TableSize, error) {
	var s TableSize
	if err := m.requireLoaded("read size of materialized view"); err != nil {
		return s, err
	}
	q, err := m.db.server.onDatabase(ctx, m.db.Name)
	if err != nil {
		return s, err
	}
	err = queryRowScan(ctx, q, tableSizeQuery, []any{m.OID}, &s.Data, &s.Indexes, &s.Total)
	return foundRow(s, err, notFoundf("pgmo: materialized view %s not found in database %q", m.QualifiedName(), m.db.Name),
		"read size of materialized view "+m.QualifiedName())
}
