//go:build livedb

package pgmo

import (
	"context"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// TestLiveConnectionOptionsResolve opens a pool from ConnectionOptions over
// -livedb (a service name, so the host, port and user come from the service
// file and the password from ~/.pgpass) and asks the session what it got:
// the fields and Params set on top of the service reach the server.
func TestLiveConnectionOptionsResolve(t *testing.T) {
	if *liveConnString == "" {
		t.Skip("no -livedb connection string given")
	}
	cfg, err := ConnectionOptions{
		ConnString:      *liveConnString,
		ApplicationName: "pgmo - live 'test'",
		ConnectTimeout:  1500 * time.Millisecond,
		Params:          map[string]string{"statement_timeout": "7s", "search_path": "pg_catalog"},
	}.poolConfig()
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(pool.Close)
	var app, timeout, path string
	err = pool.QueryRow(ctx, `SELECT pg_catalog.current_setting('application_name'),
		pg_catalog.current_setting('statement_timeout'), pg_catalog.current_setting('search_path')`).
		Scan(&app, &timeout, &path)
	if err != nil {
		t.Fatal(err)
	}
	if app != "pgmo - live 'test'" || timeout != "7s" || path != "pg_catalog" {
		t.Errorf("application_name %q, statement_timeout %q, search_path %q", app, timeout, path)
	}
}

// TestLiveServerConnect opens a Server with Connect over -livedb and checks
// that Version is the server's own server_version_num, that Pool reaches the
// same server, and that Close cancels Context and closes the pool.
func TestLiveServerConnect(t *testing.T) {
	if *liveConnString == "" {
		t.Skip("no -livedb connection string given")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := Connect(ctx, ConnectionOptions{ConnString: *liveConnString, ApplicationName: "pgmo - live connect"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	var num int
	var app string
	err = s.Pool().QueryRow(ctx, `SELECT pg_catalog.current_setting('server_version_num')::integer,
		pg_catalog.current_setting('application_name')`).Scan(&num, &app)
	if err != nil {
		t.Fatal(err)
	}
	if int(s.Version()) != num || s.Version() < MinimumServerVersion {
		t.Errorf("Version() = %d, server_version_num = %d", s.Version(), num)
	}
	if app != "pgmo - live connect" {
		t.Errorf("application_name = %q", app)
	}
	t.Logf("connected to %s", s.Version())
	s.Close()
	if s.Context().Err() == nil {
		t.Error("Context() is not done after Close")
	}
	if err := s.Pool().Ping(ctx); err == nil {
		t.Error("the pool still answers after Close")
	}
}

// TestLiveNewServer wraps the harness's own pool: NewServer reads the same
// version Connect would, and Pool returns the pool it was given.
func TestLiveNewServer(t *testing.T) {
	pool, ctx, done := liveDB(t)
	defer done()
	s, err := NewServer(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if s.Pool() != pool {
		t.Error("Pool() is not the pool NewServer was given")
	}
	var num int
	if err := pool.QueryRow(ctx, "SELECT pg_catalog.current_setting('server_version_num')::integer").Scan(&num); err != nil {
		t.Fatal(err)
	}
	if int(s.Version()) != num {
		t.Errorf("Version() = %d, server_version_num = %d", s.Version(), num)
	}
}

// TestLiveNewServerOverOneSession: a pool's acquired connection, a bare
// *pgx.Conn and a transaction are Queriers too. The Server reads the
// version over each; Close releases the acquired connection to the pool,
// closes the *pgx.Conn, and leaves the transaction for its caller to end.
func TestLiveNewServerOverOneSession(t *testing.T) {
	pool, ctx, done := liveDB(t)
	defer done()

	acquired, err := pool.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(ctx, acquired)
	if err != nil {
		acquired.Release()
		t.Fatal(err)
	}
	if s.Version() < MinimumServerVersion || s.Pool() != nil {
		t.Errorf("over *pgxpool.Conn: Version() = %d, Pool() = %v", s.Version(), s.Pool())
	}
	s.Close()
	if st := pool.Stat(); st.AcquiredConns() != 0 {
		t.Errorf("after Close: %d connections still acquired from the pool", st.AcquiredConns())
		acquired.Release() // or closing the pool waits for it forever
	}

	conn, err := pgx.Connect(ctx, *liveConnString)
	if err != nil {
		t.Fatal(err)
	}
	s, err = NewServer(ctx, conn)
	if err != nil {
		conn.Close(ctx)
		t.Fatal(err)
	}
	s.Close()
	if !conn.IsClosed() {
		t.Error("Close left the *pgx.Conn open")
	}

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(ctx)
	s, err = NewServer(ctx, tx)
	if err != nil {
		t.Fatal(err)
	}
	s.Close()
	var one int
	if err := tx.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Errorf("the transaction is unusable after Server.Close: %v", err)
	}
}
