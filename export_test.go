package pgmo

import (
	"context"

	"github.com/jackc/pgx/v5/pgconn"
)

// Unexported routing the external tests (package pgmo_test, which can import
// pgmotest without a cycle) drive.

// On is Server.on.
func (s *Server) On(ctx context.Context) Querier { return s.on(ctx) }

// OnDatabase is Server.onDatabase.
func (s *Server) OnDatabase(ctx context.Context, name string) (Querier, error) {
	return s.onDatabase(ctx, name)
}

// RefuseInTx is refuseInTx.
func RefuseInTx(ctx context.Context, s *Server, op string) error { return refuseInTx(ctx, s, op) }

// Exec is Server.exec.
func (s *Server) Exec(ctx context.Context, stmt string, args ...any) (pgconn.CommandTag, error) {
	return s.exec(ctx, stmt, args...)
}

// ExecIn is Server.execIn.
func (s *Server) ExecIn(ctx context.Context, name, stmt string, args ...any) (pgconn.CommandTag, error) {
	return s.execIn(ctx, name, stmt, args...)
}

// AlterRolePassword is Server.alterRolePassword.
func (s *Server) AlterRolePassword(ctx context.Context, role, password string) error {
	return s.alterRolePassword(ctx, role, password)
}

// RoutineByName is routineByName.
func RoutineByName(ctx context.Context, q Querier, schema, name string, args []OID) (*RoutineIdentity, error) {
	return routineByName(ctx, q, schema, name, args)
}

// SetServer replaces the Database's back-pointer, so a test can compare the
// catalog fields of two Databases with reflect.DeepEqual.
func (d *Database) SetServer(s *Server) { d.server = s }

// SetServer replaces the Role's back-pointer, so a test can compare the
// catalog fields of two Roles with reflect.DeepEqual.
func (r *Role) SetServer(s *Server) { r.server = s }

// SetDatabase replaces the Schema's back-pointer, so a test can compare the
// catalog fields of two Schemas with reflect.DeepEqual.
func (s *Schema) SetDatabase(d *Database) { s.db = d }

// SetDatabase replaces the Table's back-pointer, so a test can compare the
// catalog fields of two Tables with reflect.DeepEqual.
func (t *Table) SetDatabase(d *Database) { t.db = d }

// SetDatabase replaces the View's back-pointer, so a test can compare the
// catalog fields of two Views with reflect.DeepEqual.
func (v *View) SetDatabase(d *Database) { v.db = d }

// SetDatabase replaces the MaterializedView's back-pointer, so a test can
// compare the catalog fields of two MaterializedViews with reflect.DeepEqual.
func (m *MaterializedView) SetDatabase(d *Database) { m.db = d }

// SetDatabase replaces the Sequence's back-pointer, so a test can compare
// the catalog fields of two Sequences with reflect.DeepEqual.
func (seq *Sequence) SetDatabase(d *Database) { seq.db = d }

// SetDatabase replaces the Function's back-pointer, so a test can compare the
// catalog fields of two Functions with reflect.DeepEqual.
func (f *Function) SetDatabase(d *Database) { f.db = d }

// SetDatabase replaces the Procedure's back-pointer, so a test can compare
// the catalog fields of two Procedures with reflect.DeepEqual.
func (p *Procedure) SetDatabase(d *Database) { p.db = d }

// SetDatabase replaces the Aggregate's back-pointer, so a test can compare
// the catalog fields of two Aggregates with reflect.DeepEqual.
func (a *Aggregate) SetDatabase(d *Database) { a.db = d }

// SetDatabase replaces the Type's back-pointer, so a test can compare the
// catalog fields of two Types with reflect.DeepEqual.
func (ty *Type) SetDatabase(d *Database) { ty.db = d }

// SetDatabase replaces the Domain's back-pointer, so a test can compare the
// catalog fields of two Domains with reflect.DeepEqual.
func (dm *Domain) SetDatabase(d *Database) { dm.db = d }

// SetDatabase replaces the Trigger's back-pointer, so a test can compare the
// catalog fields of two Triggers with reflect.DeepEqual.
func (t *Trigger) SetDatabase(d *Database) { t.db = d }

// SetDatabase replaces the Policy's back-pointer, so a test can compare the
// catalog fields of two Policies with reflect.DeepEqual.
func (p *Policy) SetDatabase(d *Database) { p.db = d }

// SetDatabase replaces the EventTrigger's back-pointer, so a test can compare
// the catalog fields of two EventTriggers with reflect.DeepEqual.
func (e *EventTrigger) SetDatabase(d *Database) { e.db = d }

// SetDatabase replaces the Extension's back-pointer, so a test can compare
// the catalog fields of two Extensions with reflect.DeepEqual.
func (e *Extension) SetDatabase(d *Database) { e.db = d }

// SetDatabase replaces the AvailableExtension's back-pointer, so a test can
// compare the catalog fields of two AvailableExtensions with
// reflect.DeepEqual.
func (a *AvailableExtension) SetDatabase(d *Database) { a.db = d }

// CompareExtensionVersions is compareExtensionVersions, for the external
// tests.
var CompareExtensionVersions = compareExtensionVersions

// SetServer replaces the Tablespace's back-pointer, so a test can compare
// the catalog fields of two Tablespaces with reflect.DeepEqual.
func (t *Tablespace) SetServer(s *Server) { t.server = s }

// SetDatabase replaces the Collation's back-pointer, so a test can compare
// the catalog fields of two Collations with reflect.DeepEqual.
func (c *Collation) SetDatabase(d *Database) { c.db = d }

// SetDatabase replaces the TextSearchParser's back-pointer, so a test can
// compare the catalog fields of two parsers with reflect.DeepEqual.
func (p *TextSearchParser) SetDatabase(d *Database) { p.db = d }

// SetDatabase replaces the TextSearchTemplate's back-pointer, so a test can
// compare the catalog fields of two templates with reflect.DeepEqual.
func (t *TextSearchTemplate) SetDatabase(d *Database) { t.db = d }

// SetDatabase replaces the TextSearchDictionary's back-pointer, so a test
// can compare the catalog fields of two dictionaries with reflect.DeepEqual.
func (dc *TextSearchDictionary) SetDatabase(d *Database) { dc.db = d }

// SetDatabase replaces the TextSearchConfiguration's back-pointer, so a
// test can compare the catalog fields of two configurations with
// reflect.DeepEqual.
func (c *TextSearchConfiguration) SetDatabase(d *Database) { c.db = d }

// SetDatabase replaces the Language's back-pointer, so a test can compare
// the catalog fields of two Languages with reflect.DeepEqual.
func (l *Language) SetDatabase(d *Database) { l.db = d }

// SetDatabase replaces the Cast's back-pointer, so a test can compare the
// catalog fields of two Casts with reflect.DeepEqual.
func (c *Cast) SetDatabase(d *Database) { c.db = d }

// SetDatabase replaces the Operator's back-pointer, so a test can compare
// the catalog fields of two Operators with reflect.DeepEqual.
func (o *Operator) SetDatabase(d *Database) { o.db = d }

// SetDatabase replaces the ForeignDataWrapper's back-pointer, so a test can
// compare the catalog fields of two wrappers with reflect.DeepEqual.
func (w *ForeignDataWrapper) SetDatabase(d *Database) { w.db = d }

// SetDatabase replaces the ForeignServer's back-pointer, as above.
func (s *ForeignServer) SetDatabase(d *Database) { s.db = d }

// SetDatabase replaces the UserMapping's back-pointer, as above.
func (m *UserMapping) SetDatabase(d *Database) { m.db = d }

// SetDatabase replaces the ForeignTable's back-pointer, as above.
func (t *ForeignTable) SetDatabase(d *Database) { t.db = d }
