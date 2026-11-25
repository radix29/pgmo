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

// SetDatabase replaces the Function's back-pointer, so a test can compare the
// catalog fields of two Functions with reflect.DeepEqual.
func (f *Function) SetDatabase(d *Database) { f.db = d }

// SetDatabase replaces the Procedure's back-pointer, so a test can compare
// the catalog fields of two Procedures with reflect.DeepEqual.
func (p *Procedure) SetDatabase(d *Database) { p.db = d }

// SetDatabase replaces the Aggregate's back-pointer, so a test can compare
// the catalog fields of two Aggregates with reflect.DeepEqual.
func (a *Aggregate) SetDatabase(d *Database) { a.db = d }
