//go:build livedb

package pgmo

import (
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestLiveReadRetriesATerminatedSession ends the one session of a Server's
// pool from another (pg_terminate_backend, SQLSTATE 57P01 on the next
// statement): a statement sent on it directly fails with an error
// IsRetryable accepts, so the pool really does hand the dead session back;
// queryRowScan and query recover on a fresh session; execScan, the path for
// a write, is not retried and fails.
func TestLiveReadRetriesATerminatedSession(t *testing.T) {
	admin, ctx, done := liveDB(t)
	defer done()
	s, err := Connect(ctx, ConnectionOptions{
		ConnString:      *liveConnString + " pool_max_conns=1",
		ApplicationName: "pgmo - live retry",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	const pidQuery = `SELECT pg_catalog.pg_backend_pid()`

	// terminate ends the pool's session and waits until it is gone.
	terminate := func() int32 {
		t.Helper()
		var pid int32
		if err := s.Pool().QueryRow(ctx, pidQuery).Scan(&pid); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(ctx, `SELECT pg_catalog.pg_terminate_backend($1)`, pid); err != nil {
			t.Fatal(err)
		}
		for deadline := time.Now().Add(10 * time.Second); ; time.Sleep(20 * time.Millisecond) {
			var alive bool
			if err := admin.QueryRow(ctx, `SELECT EXISTS (SELECT FROM pg_catalog.pg_stat_activity WHERE pid = $1)`, pid).Scan(&alive); err != nil {
				t.Fatal(err)
			}
			if !alive {
				return pid
			}
			if time.Now().After(deadline) {
				t.Fatalf("session %d still alive after pg_terminate_backend", pid)
			}
		}
	}
	// The pool pings a session idle for over a second before lending it,
	// which would hide the dead one; each check runs well inside that.
	checkPool(t, s.Pool())

	terminate()
	var pid int32
	if err := s.Pool().QueryRow(ctx, pidQuery).Scan(&pid); err == nil || !IsRetryable(err) {
		t.Fatalf("a statement on the terminated session = %v, want a retryable error (the test proves nothing otherwise)", err)
	}

	old := terminate()
	if err := queryRowScan(ctx, s.Querier(), pidQuery, nil, &pid); err != nil {
		t.Errorf("queryRowScan after the session was terminated: %v", err)
	} else if pid == old {
		t.Errorf("queryRowScan answered from the terminated session %d", pid)
	}

	old = terminate()
	pids, err := query(ctx, s.Querier(), pgx.RowTo[int32], pidQuery)
	if err != nil {
		t.Errorf("query after the session was terminated: %v", err)
	} else if len(pids) != 1 || pids[0] == old {
		t.Errorf("query = %v, want one new session's pid (old %d)", pids, old)
	}

	terminate()
	if err := execScan(ctx, s.Querier(), pidQuery, nil, &pid); err == nil || !IsRetryable(err) {
		t.Errorf("execScan after the session was terminated = %v, want the retryable error, not a retry", err)
	}
}

// checkPool fails the test unless p lends one session at a time, which is
// what makes the terminated session the one the next statement gets.
func checkPool(t *testing.T, p *pgxpool.Pool) {
	t.Helper()
	if got := p.Config().MaxConns; got != 1 {
		t.Fatalf("pool MaxConns = %d, want 1", got)
	}
}
