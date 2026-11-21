//go:build livedb

package pgmo

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// liveSleep is a statement the server runs far longer than any test waits.
const liveSleep = `SELECT pg_catalog.pg_sleep(60)`

// liveCancelled runs liveSleep through run under ctx and checks it ended
// with the server's 57014, promptly — the server answered the cancel
// request, the session did not wait out cancelFallback — and with ctx's
// error, want.
func liveCancelled(t *testing.T, what string, ctx context.Context, want error, run func(context.Context) error) {
	t.Helper()
	start := time.Now()
	err := run(ctx)
	if !IsQueryCanceled(err) {
		t.Errorf("%s: err = %v, want 57014 query_canceled", what, err)
	}
	if want != nil && !errors.Is(err, want) {
		t.Errorf("%s: err = %v, want it to carry %v", what, err, want)
	}
	if took := time.Since(start); took > 3*time.Second {
		t.Errorf("%s: took %v to end", what, took)
	}
}

// cancelSoon is a context cancelled 300 ms from now: long enough for the
// statement to be running on the server.
func cancelSoon(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	time.AfterFunc(300*time.Millisecond, cancel)
	return ctx
}

// backendState reports the state of session pid as pg_stat_activity shows
// it, waiting briefly for it to leave "active".
func backendState(t *testing.T, admin *pgxpool.Pool, ctx context.Context, pid uint32) string {
	t.Helper()
	var state string
	for deadline := time.Now().Add(5 * time.Second); ; time.Sleep(20 * time.Millisecond) {
		if err := admin.QueryRow(ctx, `SELECT COALESCE(state, '') FROM pg_catalog.pg_stat_activity WHERE pid = $1`, int32(pid)).Scan(&state); err != nil {
			t.Fatalf("state of session %d: %v", pid, err)
		}
		if state != "active" || time.Now().After(deadline) {
			return state
		}
	}
}

// TestLiveCancel cancels pg_sleep on each kind of connection pgmo opens
// from ConnectionOptions and checks the statement stopped on the server and
// the session survived: the pool's one session is the one used afterwards;
// another database's pool likewise; a session from OpenSession keeps its
// temporary table, and a cancel inside a transaction leaves it in a failed
// transaction, not closed. A deadline cancels too.
func TestLiveCancel(t *testing.T) {
	admin, ctx, done := liveDB(t)
	defer done()
	s, err := Connect(ctx, ConnectionOptions{
		ConnString:      *liveConnString + " pool_max_conns=1",
		ApplicationName: "pgmo - live cancel",
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	const pidQuery = `SELECT pg_catalog.pg_backend_pid()`

	// The Server's pool: its one session survives, idle.
	var before, after uint32
	if err := queryRowScan(ctx, s.Querier(), pidQuery, nil, &before); err != nil {
		t.Fatal(err)
	}
	var v any
	liveCancelled(t, "pool, queryRowScan", cancelSoon(t), context.Canceled, func(ctx context.Context) error {
		return queryRowScan(ctx, s.Querier(), liveSleep, nil, &v)
	})
	if st := backendState(t, admin, ctx, before); st != "idle" {
		t.Errorf("pool session %d is %q after the cancel, want idle", before, st)
	}
	dctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	liveCancelled(t, "pool, query by deadline", dctx, context.DeadlineExceeded, func(ctx context.Context) error {
		_, err := query(ctx, s.Querier(), pgx.RowTo[any], liveSleep)
		return err
	})
	if err := queryRowScan(ctx, s.Querier(), pidQuery, nil, &after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("the pool's session was %d before the cancels, %d after: it was replaced", before, after)
	}

	// Another database's pool: postgres, or template1 (which accepts
	// connections) when postgres is the current one.
	other := "postgres"
	if s.CurrentDatabase() == other {
		other = "template1"
	}
	q := s.DatabaseQuerier(other)
	if err := queryRowScan(ctx, q, pidQuery, nil, &before); err != nil {
		t.Fatal(err)
	}
	liveCancelled(t, "other database", cancelSoon(t), context.Canceled, func(ctx context.Context) error {
		return queryRowScan(ctx, q, liveSleep, nil, &v)
	})
	if err := queryRowScan(ctx, q, pidQuery, nil, &after); err != nil {
		t.Fatal(err)
	}
	if after != before {
		t.Errorf("%s's session was %d before the cancel, %d after: it was replaced", other, before, after)
	}

	// A session: its state outlives a cancel.
	conn, err := s.OpenSession(ctx, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	if _, err := conn.Exec(ctx, `CREATE TEMPORARY TABLE kept (a integer)`); err != nil {
		t.Fatal(err)
	}
	liveCancelled(t, "session", cancelSoon(t), nil, func(ctx context.Context) error {
		_, err := conn.Exec(ctx, liveSleep)
		return err
	})
	if conn.IsClosed() {
		t.Fatal("the session was closed by the cancel")
	}
	if st := backendState(t, admin, ctx, conn.PgConn().PID()); st != "idle" {
		t.Errorf("session is %q after the cancel, want idle", st)
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	liveCancelled(t, "session in a transaction", cancelSoon(t), nil, func(ctx context.Context) error {
		_, err := tx.Exec(ctx, liveSleep)
		return err
	})
	if got := conn.PgConn().TxStatus(); got != 'E' {
		t.Errorf("transaction status after a cancel in it = %q, want 'E' (failed transaction)", got)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback after the cancel: %v", err)
	}
	if _, err := conn.Exec(ctx, `INSERT INTO pg_temp.kept VALUES (1)`); err != nil {
		t.Errorf("the session's temporary table after the cancels: %v", err)
	}
}
