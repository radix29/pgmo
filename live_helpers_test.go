//go:build livedb

// Connection helpers shared by the package's live tests (go test -tags
// livedb, -livedb CONNSTRING): the admin pool every test starts from. A test
// whose setup needs more (a database, a role, a function) keeps that helper
// beside it.
package pgmo

import (
	"context"
	"flag"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

// liveConnString is the server every live test runs against; without it each
// skips. It takes a libpq connection string, in practice a service name
// ('service=pg18'), so no password appears on the command line.
var liveConnString = flag.String("livedb", "", "PostgreSQL connection string for the live tests (e.g. 'service=pg18')")

// liveDB is an admin pool on -livedb, pinged, with a one-minute context and
// the func that closes both.
func liveDB(t *testing.T) (*pgxpool.Pool, context.Context, func()) {
	t.Helper()
	return liveDBTimeout(t, 60*time.Second)
}

// liveDBTimeout is liveDB with a longer deadline, for a test whose setup
// alone outlasts the minute. Taking it here, rather than shadowing liveDB's
// ctx with a fresh one, keeps a single context per test.
func liveDBTimeout(t *testing.T, timeout time.Duration) (*pgxpool.Pool, context.Context, func()) {
	t.Helper()
	if *liveConnString == "" {
		t.Skip("no -livedb connection string given")
	}
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	pool, err := pgxpool.New(ctx, *liveConnString)
	if err != nil {
		cancel()
		t.Fatalf("open: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		cancel()
		t.Fatalf("ping: %v", err)
	}
	return pool, ctx, func() { pool.Close(); cancel() }
}

// TestLiveConnect is the smoke test for the live harness: -livedb reaches a
// server and the session reports its version.
func TestLiveConnect(t *testing.T) {
	pool, ctx, done := liveDB(t)
	defer done()
	var num int
	if err := pool.QueryRow(ctx, "SELECT pg_catalog.current_setting('server_version_num')::int").Scan(&num); err != nil {
		t.Fatalf("server_version_num: %v", err)
	}
	if num < 150000 {
		t.Errorf("server_version_num = %d, below the PostgreSQL 15 floor", num)
	}
	t.Logf("server_version_num = %d", num)
}
