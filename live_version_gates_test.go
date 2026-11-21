//go:build livedb

// Live check of the version-gate inventory (version_gate_inventory_test.go):
// ask the connected server whether each gated relation, column, statement and
// extension column is there, and compare with what the inventory decides for
// its server_version_num.
//
//	go test -tags livedb . -run TestLiveVersionGates -v -livedb 'service=pg15'
//
// Both directions fail. A thing the gate allows on a server that lacks it
// kills the read that names it; a thing the gate withholds from a server that
// has it reads as a zero value where the server could have answered.
//
// The statement probes run in transactions that are always rolled back; the
// rest only reads the catalog.
package pgmo

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

// errRollback ends a probe transaction that succeeded, so it is rolled back
// all the same.
var errRollback = errors.New("rollback")

// catalogHas reports whether pg_catalog has relation, or relation.column when
// column is not "".
func catalogHas(t *testing.T, ctx context.Context, pool *pgxpool.Pool, schema, relation, column string) bool {
	t.Helper()
	const q = `
SELECT EXISTS (
  SELECT FROM pg_catalog.pg_class c
  JOIN   pg_catalog.pg_namespace n ON n.oid = c.relnamespace
  WHERE  n.nspname = $1 AND c.relname = $2
    AND  ($3 = '' OR EXISTS (
           SELECT FROM pg_catalog.pg_attribute a
           WHERE  a.attrelid = c.oid AND a.attname = $3
             AND  a.attnum > 0 AND NOT a.attisdropped)))`
	var ok bool
	if err := pool.QueryRow(ctx, q, schema, relation, column).Scan(&ok); err != nil {
		t.Fatalf("catalog lookup %s.%s.%s: %v", schema, relation, column, err)
	}
	return ok
}

// gateSpan renders g's range for a failure message: "since PostgreSQL 16,
// until PostgreSQL 18", with an open end left out.
func gateSpan(g catalogGate) string {
	switch {
	case g.since != 0 && g.until != 0:
		return "since " + g.since.String() + ", until " + g.until.String()
	case g.since != 0:
		return "since " + g.since.String()
	case g.until != 0:
		return "until " + g.until.String()
	}
	return "ungated"
}

func liveServerVersionNum(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int {
	t.Helper()
	var num int
	if err := pool.QueryRow(ctx, "SELECT pg_catalog.current_setting('server_version_num')::int").Scan(&num); err != nil {
		t.Fatalf("server_version_num: %v", err)
	}
	if num == 0 {
		t.Fatal("server_version_num read as 0; every gate would answer as newest")
	}
	return num
}

func TestLiveVersionGatesCatalog(t *testing.T) {
	pool, ctx, done := liveDB(t)
	defer done()
	num := liveServerVersionNum(t, ctx, pool)
	t.Logf("server_version_num %d", num)

	for _, g := range catalogGates {
		t.Run(g.relation+"."+g.column, func(t *testing.T) {
			has := catalogHas(t, ctx, pool, "pg_catalog", g.relation, g.column)
			want := catalogGateExpected(g, num)
			switch {
			case want && !has:
				t.Errorf("the inventory allows %s.%s on %d (%s) but the server has no such thing — a read naming it dies here",
					g.relation, g.column, num, gateSpan(g))
			case !want && has:
				t.Errorf("the inventory withholds %s.%s on %d (%s) but the server has it — a read gated on it reads zero here",
					g.relation, g.column, num, gateSpan(g))
			}
		})
	}
	for rel, cols := range stableColumns {
		for _, col := range cols {
			if !catalogHas(t, ctx, pool, "pg_catalog", rel, col) {
				t.Errorf("%s.%s is listed as stable on every supported release but %d lacks it — it needs a gate", rel, col, num)
			}
		}
	}
}

func TestLiveVersionGatesStatements(t *testing.T) {
	pool, ctx, done := liveDB(t)
	defer done()
	num := liveServerVersionNum(t, ctx, pool)

	for _, g := range statementGates {
		t.Run(g.name, func(t *testing.T) {
			var probeErr error
			err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
				for _, q := range g.setup {
					if _, err := tx.Exec(ctx, q); err != nil {
						t.Fatalf("setup %q: %v", q, err)
					}
				}
				_, probeErr = tx.Exec(ctx, g.probe)
				return errRollback
			})
			if !errors.Is(err, errRollback) {
				t.Fatalf("transaction: %v", err)
			}
			switch want := atLeast(num, g.since); {
			case want && probeErr != nil:
				t.Errorf("gated on %s, so %d should accept it, but: %v", g.since, num, probeErr)
			case !want && probeErr == nil:
				t.Errorf("gated on %s, but %d accepts it — the gate is too new", g.since, num)
			case !want && pgErrCode(probeErr) != g.sqlstate:
				t.Errorf("before %s it should fail with %s; %d raised %v", g.since, g.sqlstate, num, probeErr)
			}
		})
	}
}

// pgErrCode is err's SQLSTATE, or "" when err is not a server error.
func pgErrCode(err error) string {
	if pe, ok := AsPgError(err); ok {
		return pe.Code
	}
	return ""
}

func TestLiveVersionGatesExtensions(t *testing.T) {
	pool, ctx, done := liveDB(t)
	defer done()

	installed := map[string]struct{ schema, version string }{}
	rows, err := pool.Query(ctx, `
SELECT e.extname, n.nspname, e.extversion
FROM   pg_catalog.pg_extension e
JOIN   pg_catalog.pg_namespace n ON n.oid = e.extnamespace`)
	if err != nil {
		t.Fatalf("pg_extension: %v", err)
	}
	for rows.Next() {
		var name, schema, version string
		if err := rows.Scan(&name, &schema, &version); err != nil {
			t.Fatalf("pg_extension: %v", err)
		}
		installed[name] = struct{ schema, version string }{schema, version}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("pg_extension: %v", err)
	}

	checked := 0
	for _, g := range extensionGates {
		ext, ok := installed[g.extension]
		if !ok {
			continue
		}
		checked++
		t.Run(g.extension+"."+g.column, func(t *testing.T) {
			has := catalogHas(t, ctx, pool, ext.schema, g.relation, g.column)
			want := extensionGateExpected(g, ext.version)
			switch {
			case want && !has:
				t.Errorf("the inventory allows %s.%s at %s %s (since %q, until %q) but it is not there",
					g.relation, g.column, g.extension, ext.version, g.since, g.until)
			case !want && has:
				t.Errorf("the inventory withholds %s.%s at %s %s (since %q, until %q) but it is there",
					g.relation, g.column, g.extension, ext.version, g.since, g.until)
			}
		})
	}
	if checked == 0 {
		t.Skip("no gated extension is installed in this database")
	}
}
