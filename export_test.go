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
