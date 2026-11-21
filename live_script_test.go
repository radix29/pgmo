//go:build livedb

package pgmo

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"testing"
	"time"
)

// TestLiveScriptLiteralsMatchBinding holds every literal scriptLiteral
// writes to what pgx sends for the same value: the statement run with the
// value bound and the captured statement run as text (simple protocol, no
// parameters) read back the same, typed by their position as the parameter
// is. Run with standard_conforming_strings both on and off, since a captured
// script is run in whatever session the person has.
func TestLiveScriptLiteralsMatchBinding(t *testing.T) {
	pool, ctx, done := liveDB(t)
	defer done()
	at := time.Date(2026, 11, 21, 10, 30, 1, 123456789, time.FixedZone("x", 3600))
	cases := []struct {
		expr string // the parameter's position, $1
		v    any
	}{
		{"$1::text", "plain"},
		{"$1::text", "it's \\ back"},
		{"$1::text", ""},
		{"$1::text", nil},
		{"$1::bytea", []byte{0, 1, 0xfe, '\\', '\''}},
		{"$1::bytea", []byte{}},
		{"$1::text[]", []string{"a", `b"c`, `d\e`, "NULL", "x,y", "it's", "{}", ""}},
		{"$1::text[]", []string{}},
		{"$1::boolean", true},
		{"$1::smallint", int16(-32768)},
		{"$1::integer", int32(math.MinInt32)},
		{"$1::bigint", int64(math.MinInt64)},
		{"$1::bigint", 42},
		{"$1::numeric", uint64(math.MaxInt64)},
		{"10 - $1", -3},
		{"10 - $1", 3},
		{"$1::double precision", 0.1},
		{"$1::double precision", -1e300},
		{"$1::double precision", math.Copysign(0, -1)},
		{"$1::double precision", math.NaN()},
		{"$1::double precision", math.Inf(-1)},
		{"$1::real", float32(0.1)},
		{"$1::real", float32(math.Inf(1))},
		{"$1::numeric", 2.5},
		{"$1::timestamptz", at},
		{"$1::timestamp", at},
		{"$1::timestamp", time.Date(1900, 1, 1, 0, 0, 0, 0, mustZone(t, "Europe/Amsterdam"))}, // LMT: an offset in seconds
		{"$1::timestamptz", time.Date(1900, 1, 1, 0, 0, 0, 0, mustZone(t, "Europe/Amsterdam"))},
		{"$1::date", at},
		{"$1::timestamptz", time.Date(1, 1, 1, 0, 0, 0, 0, time.UTC)},
		{"$1::date", time.Date(2026, 2, 3, 0, 0, 0, 0, time.UTC)},
		{"$1::text", sql.NullString{String: "n", Valid: true}},
		{"$1::bigint", sql.NullInt64{}},
		{"pg_catalog.length($1)", "abc"},
		{"$1 = 'x'", "x"},
	}
	for _, scs := range []string{"on", "off"} {
		conn, err := pool.Acquire(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := conn.Exec(ctx, "SET standard_conforming_strings = "+scs+"; SET TIME ZONE 'Asia/Tokyo'"); err != nil {
			conn.Release()
			t.Fatal(err)
		}
		for _, tc := range cases {
			q := "SELECT (" + tc.expr + ")::pg_catalog.text"
			var bound, scripted sql.NullString
			if err := conn.QueryRow(ctx, q, tc.v).Scan(&bound); err != nil {
				t.Errorf("scs %s: %s bound with %#v: %v", scs, tc.expr, tc.v, err)
				continue
			}
			text, err := bindScriptArgs(q, []any{tc.v})
			if err != nil {
				t.Errorf("scs %s: bind %s: %v", scs, tc.expr, err)
				continue
			}
			// No arguments: pgx sends it as a simple query, as a script is run.
			if err := conn.QueryRow(ctx, text).Scan(&scripted); err != nil {
				t.Errorf("scs %s: %s: %v", scs, text, err)
				continue
			}
			if bound != scripted {
				t.Errorf("scs %s: %s: bound %#v reads %v, scripted %s reads %v", scs, tc.expr, tc.v, bound, text, scripted)
			}
		}
		_, _ = conn.Exec(ctx, "RESET standard_conforming_strings; RESET TIME ZONE")
		conn.Release()
	}
}

func mustZone(t *testing.T, name string) *time.Location {
	t.Helper()
	loc, err := time.LoadLocation(name)
	if err != nil {
		t.Fatal(err)
	}
	return loc
}

// TestLiveScriptRunsAsCaptured captures a scripted transaction — DDL, a
// bound DML write and a failing step's absence — runs String() as one simple
// query, and finds what running the writes directly would have left. The
// capture itself sent nothing.
func TestLiveScriptRunsAsCaptured(t *testing.T) {
	admin, ctx, done := liveDB(t)
	t.Cleanup(done)
	schema := liveTxSchema(t, admin, ctx)
	s, err := Connect(ctx, ConnectionOptions{ConnString: *liveConnString, ApplicationName: "pgmo - live script"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	sctx, c := WithScript(ctx)
	table := schema + ".t"
	err = s.InTransaction(sctx, func(ctx context.Context) error {
		if _, err := s.exec(ctx, "CREATE TABLE "+table+" (a text, b bigint, c bytea) -- trailing"); err != nil {
			return err
		}
		_, err := s.exec(ctx, "INSERT INTO "+table+" VALUES ($1, $2, $3)", "it's $1", int64(-5), []byte{'\\'})
		return err
	})
	if err != nil {
		t.Fatalf("scripted InTransaction: %v", err)
	}
	if tableExists(t, admin, ctx, schema, "t") {
		t.Fatal("the capture created the table")
	}
	script := c.String()
	t.Logf("script:\n%s", script)
	if _, err := admin.Exec(ctx, script); err != nil {
		t.Fatalf("running the script: %v", err)
	}
	var a string
	var b int64
	var cb []byte
	if err := admin.QueryRow(ctx, "SELECT a, b, c FROM "+table).Scan(&a, &b, &cb); err != nil {
		t.Fatal(err)
	}
	if a != "it's $1" || b != -5 || string(cb) != `\` {
		t.Errorf("row %q %d %q, want the values bound", a, b, cb)
	}
	if got := fmt.Sprint(c.Len()); got != "4" {
		t.Errorf("captured %s entries, want 4", got)
	}
}
