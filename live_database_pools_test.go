//go:build livedb

package pgmo

import (
	"fmt"
	"testing"
	"time"

	"github.com/radix29/pgmo/pgsql"
)

// TestLiveDatabasePools: a Server reaches two throwaway databases through
// DatabaseQuerier, each on a session bound to it; under a cap of 1 opening
// the second closes the first's sessions on the server, and the first's
// handle reopens it; Close ends them all.
func TestLiveDatabasePools(t *testing.T) {
	admin, ctx, done := liveDB(t)
	t.Cleanup(done) // registered first, so it runs after the drops
	app := fmt.Sprintf("pgmo - live pools %d", time.Now().UnixNano())
	names := []string{"pgmo_live_pools_a", "pgmo_live_pools_b"}
	for _, name := range names {
		if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgsql.QuoteIdent(name)+" WITH (FORCE)"); err != nil {
			t.Fatal(err)
		}
		if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgsql.QuoteIdent(name)); err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgsql.QuoteIdent(name)+" WITH (FORCE)"); err != nil {
				t.Errorf("drop %s: %v", name, err)
			}
		})
	}
	s, err := Connect(ctx, ConnectionOptions{ConnString: *liveConnString, ApplicationName: app}, WithMaxDatabasePools(1))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	sessions := func(db string) int {
		t.Helper()
		var n int
		err := admin.QueryRow(ctx, `SELECT count(*) FROM pg_catalog.pg_stat_activity
			WHERE datname = $1 AND application_name = $2`, db, app).Scan(&n)
		if err != nil {
			t.Fatal(err)
		}
		return n
	}
	settle := func(db string, want int) int {
		t.Helper()
		n := sessions(db)
		for deadline := time.Now().Add(5 * time.Second); n != want && time.Now().Before(deadline); n = sessions(db) {
			time.Sleep(50 * time.Millisecond)
		}
		return n
	}
	read := func(db string) {
		t.Helper()
		var got string
		if err := s.DatabaseQuerier(db).QueryRow(ctx, "SELECT pg_catalog.current_database()").Scan(&got); err != nil {
			t.Fatalf("read in %s: %v", db, err)
		}
		if got != db {
			t.Errorf("DatabaseQuerier(%s) reached %s", db, got)
		}
	}

	if s.DatabaseQuerier(s.CurrentDatabase()) != Querier(s.Pool()) {
		t.Errorf("DatabaseQuerier(%s) is not the Server's own pool", s.CurrentDatabase())
	}
	read(names[0])
	if n := settle(names[0], 1); n != 1 {
		t.Errorf("%d sessions in %s after a read, want 1", n, names[0])
	}
	read(names[1])
	if n := settle(names[0], 0); n != 0 {
		t.Errorf("%d sessions still in %s after %s opened under a cap of 1", n, names[0], names[1])
	}
	read(names[0])
	if n := settle(names[1], 0); n != 0 {
		t.Errorf("%d sessions still in %s after %s reopened", n, names[1], names[0])
	}
	if err := s.DatabaseQuerier("pgmo_live_pools_missing").QueryRow(ctx, "SELECT 1").Scan(new(int)); !IsUndefinedObject(err) {
		t.Errorf("a missing database: %v, want 3D000", err)
	}
	s.Close()
	for _, db := range names {
		if n := settle(db, 0); n != 0 {
			t.Errorf("%d sessions in %s after Close", n, db)
		}
	}
}
