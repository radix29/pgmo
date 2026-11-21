//go:build livedb

package pgmo

import (
	"fmt"
	"testing"
	"time"

	"github.com/radix29/pgmo/pgsql"
)

// TestLiveReadsIgnoreTheSessionSearchPath: a session whose search_path puts
// a schema of look-alike functions ahead of pg_catalog still gets the real
// server version and database from Connect, because pgmo's reads name
// pg_catalog themselves (P2.2.4). The same session's bare calls do reach the
// look-alikes, so the trap is live: without it this test would prove
// nothing.
func TestLiveReadsIgnoreTheSessionSearchPath(t *testing.T) {
	admin, ctx, done := liveDB(t)
	t.Cleanup(done) // registered first, so it runs after the drop
	var wantVersion int
	var wantDB string
	if err := admin.QueryRow(ctx, `SELECT pg_catalog.current_setting('server_version_num')::integer,
		pg_catalog.current_database()`).Scan(&wantVersion, &wantDB); err != nil {
		t.Fatal(err)
	}

	schema := pgsql.QuoteIdent(fmt.Sprintf("pgmo_live_shadow_%d", time.Now().UnixNano()))
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP SCHEMA IF EXISTS "+schema+" CASCADE"); err != nil {
			t.Errorf("drop %s: %v", schema, err)
		}
	})
	for _, stmt := range []string{
		"CREATE SCHEMA " + schema,
		// A version below the floor: read through these, Connect would refuse.
		"CREATE FUNCTION " + schema + ".current_setting(pg_catalog.text) RETURNS pg_catalog.text " +
			"LANGUAGE sql AS $$ SELECT '90600'::pg_catalog.text $$",
		"CREATE FUNCTION " + schema + ".current_database() RETURNS pg_catalog.name " +
			"LANGUAGE sql AS $$ SELECT 'shadowed'::pg_catalog.name $$",
		// integer is a keyword type and never looked up; int4 would be.
		"CREATE DOMAIN " + schema + ".int4 AS pg_catalog.int8",
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	s, err := Connect(ctx, ConnectionOptions{
		ConnString:      *liveConnString,
		ApplicationName: "pgmo - live search_path",
		Params:          map[string]string{"search_path": schema + ", pg_catalog"},
	})
	if err != nil {
		t.Fatalf("Connect under a shadowing search_path: %v", err)
	}
	t.Cleanup(s.Close)
	if int(s.Version()) != wantVersion || s.CurrentDatabase() != wantDB {
		t.Errorf("Version %d, CurrentDatabase %q; want %d, %q", s.Version(), s.CurrentDatabase(), wantVersion, wantDB)
	}

	var bareVersion, bareDB string
	if err := s.Pool().QueryRow(ctx, "SELECT current_setting('server_version_num'), current_database()").
		Scan(&bareVersion, &bareDB); err != nil {
		t.Fatal(err)
	}
	if bareVersion != "90600" || bareDB != "shadowed" {
		t.Errorf("bare calls on the session gave %q, %q; the look-alikes are not in the way and the test proves nothing", bareVersion, bareDB)
	}
}
