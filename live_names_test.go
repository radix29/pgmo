//go:build livedb

package pgmo

import (
	"fmt"
	"testing"
	"time"

	"github.com/radix29/pgmo/pgsql"
)

// foldCorpus is typed input FoldIdent and the server must read alike: bare
// and quoted, mixed case, non-ASCII letters (folded by neither), white
// space around and inside quotes, keywords, and a name over 63 bytes.
var foldCorpus = []string{
	"orders", "Orders", "ORDERS", `"Orders"`, `"ORDERS"`, `"orders"`, "ÄbC", `"ÄbC"`, "äBC", "ǅX", "日本Ab",
	"  Ab  ", "\tAb\n", `  "A b"  `, `"a""B"`, `"a.B"`, "_X1$", "Select", `"Select"`, "USER",
	"Abcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyzabcdefghijklmnopqrstuvwxyz",
}

// TestLiveFoldIdentMatchesServer pins pgsql.FoldIdent to the server's
// parse_ident on foldCorpus: each input is one identifier to the server, and
// FoldIdent names the same stored name.
func TestLiveFoldIdentMatchesServer(t *testing.T) {
	pool, ctx, done := liveDB(t)
	defer done()
	for _, in := range foldCorpus {
		var parts []string
		if err := pool.QueryRow(ctx, "SELECT pg_catalog.parse_ident($1)", in).Scan(&parts); err != nil {
			t.Errorf("parse_ident(%q): %v", in, err)
			continue
		}
		if len(parts) != 1 {
			t.Errorf("parse_ident(%q) = %q: not one identifier; fix the corpus", in, parts)
			continue
		}
		if got := pgsql.FoldIdent(in); got != parts[0] {
			t.Errorf("FoldIdent(%q) = %q, server parse_ident = %q", in, got, parts[0])
		}
	}
}

// TestLiveNamesDifferingByCase pins the premise of exact name matching
// (names.go): tables whose names differ only by case — ASCII and not — are
// distinct objects side by side in one schema; the server resolves a typed
// name to the table whose stored name is FoldIdent of it; and matchName over
// the listed names finds each table by its stored name alone.
func TestLiveNamesDifferingByCase(t *testing.T) {
	admin, ctx, done := liveDB(t)
	t.Cleanup(done) // registered first, so it runs after the drop
	schema := fmt.Sprintf("pgmo_live_names_%d", time.Now().UnixNano())
	S := pgsql.QuoteIdent(schema)
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP SCHEMA IF EXISTS "+S+" CASCADE"); err != nil {
			t.Errorf("drop schema: %v", err)
		}
	})
	stored := []string{"orders", "Orders", "ORDERS", "Äbc", "äbc", "ÄBC"}
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+S); err != nil {
		t.Fatal(err)
	}
	for _, name := range stored {
		if _, err := admin.Exec(ctx, "CREATE TABLE "+pgsql.QualifiedName(schema, name)+" ()"); err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
	}

	type table struct {
		oid  OID
		name string
	}
	tables, err := scanRows(ctx, admin, "list tables", func(scan func(...any) error) (table, error) {
		var tb table
		return tb, scan(&tb.oid, &tb.name)
	}, `SELECT c.oid, c.relname FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
		WHERE n.nspname = $1 ORDER BY c.oid`, schema)
	if err != nil {
		t.Fatal(err)
	}
	if len(tables) != len(stored) {
		t.Fatalf("%d tables in the schema, want %d distinct ones: %v", len(tables), len(stored), tables)
	}
	oidOf := map[string]OID{}
	for _, name := range stored {
		tb, err := matchName(tables, name, func(tb table) string { return tb.name })
		if err != nil {
			t.Fatalf("matchName(%q): %v", name, err)
		}
		oidOf[name] = tb.oid
	}
	if len(oidOf) != len(stored) {
		t.Fatalf("stored names resolve to %d tables, want %d", len(oidOf), len(stored))
	}
	for _, typed := range []string{"orders", "Orders", "ORDERS", `"Orders"`, `"ORDERS"`, "ÄBC", "äBC", `"ÄBC"`, " Orders "} {
		var server *uint32
		if err := admin.QueryRow(ctx, "SELECT pg_catalog.to_regclass($1)::pg_catalog.oid", S+"."+typed).Scan(&server); err != nil {
			t.Fatalf("to_regclass(%q): %v", typed, err)
		}
		if server == nil {
			t.Errorf("typed %q: the server resolves no table", typed)
			continue
		}
		if want, ok := oidOf[pgsql.FoldIdent(typed)]; !ok || OID(*server) != want {
			t.Errorf("typed %q: the server resolves oid %d, FoldIdent names %q (oid %d)", typed, *server, pgsql.FoldIdent(typed), want)
		}
	}
	if _, err := matchName(tables, "Äbc ", func(tb table) string { return tb.name }); err != ErrNotFound {
		t.Errorf(`matchName("Äbc "): %v, want ErrNotFound — matching is exact`, err)
	}
}
