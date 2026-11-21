//go:build livedb

package pgmo

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// liveTxSchema creates a throwaway schema for a transaction test and drops
// it, with whatever the test left in it, when the test ends.
func liveTxSchema(t *testing.T, admin *pgxpool.Pool, ctx context.Context) string {
	t.Helper()
	name := fmt.Sprintf("pgmo_live_tx_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+name); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP SCHEMA "+name+" CASCADE"); err != nil {
			t.Errorf("drop schema %s: %v", name, err)
		}
	})
	return name
}

// tableExists reports whether table schema.name is committed, read on the
// admin pool, another session.
func tableExists(t *testing.T, admin *pgxpool.Pool, ctx context.Context, schema, name string) bool {
	t.Helper()
	var ok bool
	if err := admin.QueryRow(ctx, `SELECT pg_catalog.to_regclass($1) IS NOT NULL`, schema+"."+name).Scan(&ok); err != nil {
		t.Fatalf("to_regclass: %v", err)
	}
	return ok
}

// TestLiveInTransaction runs InTransaction against a real server: fn's
// statements share one session and see each other's uncommitted DDL, which
// another session does not; fn's error rolls the DDL back; a failure fn
// tolerated turns COMMIT into ROLLBACK; and Close during a statement in
// fn cancels it promptly rather than waiting for it.
func TestLiveInTransaction(t *testing.T) {
	admin, ctx, done := liveDB(t)
	t.Cleanup(done) // after liveTxSchema's drop, which needs admin
	schema := liveTxSchema(t, admin, ctx)
	s, err := Connect(ctx, ConnectionOptions{ConnString: *liveConnString, ApplicationName: "pgmo - live transaction"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	t.Run("commit", func(t *testing.T) {
		err := s.InTransaction(ctx, func(ctx context.Context) error {
			var pid1, pid2 int32
			if err := queryRowScan(ctx, s.on(ctx), `SELECT pg_catalog.pg_backend_pid()`, nil, &pid1); err != nil {
				return err
			}
			if _, err := s.on(ctx).Exec(ctx, "CREATE TABLE "+schema+".committed (id integer)"); err != nil {
				return err
			}
			var n int
			if err := queryRowScan(ctx, s.on(ctx), `SELECT pg_catalog.pg_backend_pid(), count(*)::integer FROM pg_catalog.pg_class WHERE relname = 'committed' AND relnamespace = pg_catalog.to_regnamespace($1)`, []any{schema}, &pid2, &n); err != nil {
				return err
			}
			if pid1 != pid2 || n != 1 {
				t.Errorf("inside: sessions %d and %d, table seen %d times; want one session that sees its own table", pid1, pid2, n)
			}
			if tableExists(t, admin, ctx, schema, "committed") {
				t.Error("another session sees the table before COMMIT")
			}
			return nil
		})
		if err != nil {
			t.Fatalf("InTransaction: %v", err)
		}
		if !tableExists(t, admin, ctx, schema, "committed") {
			t.Error("the table is missing after COMMIT")
		}
	})

	t.Run("fn error rolls back", func(t *testing.T) {
		boom := errors.New("boom")
		err := s.InTransaction(ctx, func(ctx context.Context) error {
			if _, err := s.on(ctx).Exec(ctx, "CREATE TABLE "+schema+".rolled_back (id integer)"); err != nil {
				return err
			}
			return boom
		})
		if err != boom {
			t.Errorf("err = %v, want fn's", err)
		}
		if tableExists(t, admin, ctx, schema, "rolled_back") {
			t.Error("the table survived the rollback")
		}
	})

	t.Run("tolerated failure is not committed", func(t *testing.T) {
		err := s.InTransaction(ctx, func(ctx context.Context) error {
			if _, err := s.on(ctx).Exec(ctx, "CREATE TABLE "+schema+".aborted (id integer)"); err != nil {
				return err
			}
			_, err := s.on(ctx).Exec(ctx, "CREATE TABLE "+schema+".committed (id integer)") // 42P07
			if !IsAlreadyExists(err) {
				t.Errorf("duplicate table: %v, want 42P07", err)
			}
			return nil
		})
		if !errors.Is(err, pgx.ErrTxCommitRollback) {
			t.Errorf("err = %v, want pgx.ErrTxCommitRollback", err)
		}
		if tableExists(t, admin, ctx, schema, "aborted") {
			t.Error("a statement before the failure was committed")
		}
	})

	t.Run("close cancels", func(t *testing.T) {
		start := time.Now()
		err := s.InTransaction(ctx, func(ctx context.Context) error {
			time.AfterFunc(300*time.Millisecond, s.Close)
			_, err := s.on(ctx).Exec(ctx, liveSleep)
			return err
		})
		if !IsQueryCanceled(err) {
			t.Errorf("err = %v, want 57014", err)
		}
		if took := time.Since(start); took > 3*time.Second {
			t.Errorf("took %v to end", took)
		}
	})
}
