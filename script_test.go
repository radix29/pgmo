package pgmo_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/radix29/pgmo"
	"github.com/radix29/pgmo/pgmotest"
)

// Under WithScript a write is captured, args bound, and nothing is sent; a
// read still runs; without it the same write is sent with its args.
func TestWithScriptCapturesWritesAndSendsNothing(t *testing.T) {
	f := pgmotest.New(t)
	other := f.Database("other")
	s := f.Server()
	ctx, c := pgmo.WithScript(t.Context())
	if !pgmo.Scripting(ctx) || pgmo.Scripting(t.Context()) {
		t.Fatal("Scripting does not tell a WithScript context from its parent")
	}

	tag, err := s.Exec(ctx, `UPDATE t SET a = $1 WHERE b = $2`, "it's", int64(-3))
	if err != nil || tag.RowsAffected() != 0 {
		t.Fatalf("captured exec: %v, %v", tag, err)
	}
	if _, err := s.ExecIn(ctx, "other", `CREATE SCHEMA x`); err != nil {
		t.Fatalf("captured execIn: %v", err)
	}
	if _, err := s.On(ctx).Exec(ctx, "SELECT 1"); err != nil {
		t.Fatalf("read under WithScript: %v", err)
	}
	wantSent(t, f, "exec SELECT 1")
	wantSent(t, other)

	want := []pgmo.ScriptEntry{
		{Database: "postgres", SQL: `UPDATE t SET a = 'it''s' WHERE b = (-3)`},
		{Database: "other", SQL: `CREATE SCHEMA x`},
	}
	if got := c.Entries(); !slices.Equal(got, want) || c.Len() != 2 {
		t.Errorf("entries %q (Len %d), want %q", got, c.Len(), want)
	}

	if _, err := s.Exec(t.Context(), `UPDATE t SET a = $1`, "v"); err != nil {
		t.Fatal(err)
	}
	q := f.Queries()
	if last := q[len(q)-1]; last.Text != `UPDATE t SET a = $1` || !slices.Equal(last.Args, []any{"v"}) {
		t.Errorf("outside WithScript sent %q %v, want the statement with its args", last.Text, last.Args)
	}
	if c.Len() != 2 {
		t.Error("a write outside WithScript was captured")
	}
}

// A capture that cannot be bound fails the write, and captures nothing.
func TestWithScriptBindFailureFailsTheWrite(t *testing.T) {
	f := pgmotest.New(t)
	s := f.Server()
	ctx, c := pgmo.WithScript(t.Context())
	for _, tc := range []struct {
		stmt string
		args []any
		kind error
	}{
		{`SELECT $2`, []any{1, 2}, pgmo.ErrInvalidRequest},
		{`SELECT $1`, nil, pgmo.ErrInvalidRequest},
		{`SELECT $1`, []any{struct{}{}}, pgmo.ErrUnsupported},
	} {
		if _, err := s.Exec(ctx, tc.stmt, tc.args...); !errors.Is(err, tc.kind) || !strings.HasPrefix(err.Error(), "script: ") {
			t.Errorf("%s %v: %v, want %v under \"script: \"", tc.stmt, tc.args, err, tc.kind)
		}
	}
	if c.Len() != 0 {
		t.Errorf("captured %q from failed binds", c.Entries())
	}
	wantSent(t, f)
}

// InTransaction under WithScript captures BEGIN … COMMIT around fn's writes,
// or BEGIN … ROLLBACK when fn fails, and sends nothing. fn's reads run
// outside a transaction.
func TestInTransactionUnderWithScript(t *testing.T) {
	f := pgmotest.New(t)
	s := f.Server()
	ctx, c := pgmo.WithScript(t.Context())
	err := s.InTransaction(ctx, func(ctx context.Context) error {
		if _, err := s.Exec(ctx, "CREATE SCHEMA a"); err != nil {
			return err
		}
		_, err := s.On(ctx).Exec(ctx, "SELECT read")
		return err
	})
	if err != nil {
		t.Fatalf("InTransaction: %v", err)
	}
	boom := errors.New("boom")
	if err := s.InTransaction(ctx, func(ctx context.Context) error {
		_, _ = s.Exec(ctx, "CREATE SCHEMA b")
		return boom
	}); err != boom {
		t.Errorf("failing fn: %v, want its error", err)
	}
	func() {
		defer func() {
			if recover() == nil {
				t.Error("fn's panic did not reach the caller")
			}
		}()
		_ = s.InTransaction(ctx, func(ctx context.Context) error { panic("x") })
	}()
	wantSent(t, f, "exec SELECT read")
	var got []string
	for _, e := range c.Entries() {
		got = append(got, e.SQL)
	}
	want := []string{"BEGIN", "CREATE SCHEMA a", "COMMIT", "BEGIN", "CREATE SCHEMA b", "ROLLBACK", "BEGIN", "ROLLBACK"}
	if !slices.Equal(got, want) {
		t.Errorf("captured %q, want %q", got, want)
	}
}

// A scripted transaction refuses what a real one refuses: nesting, another
// database, and statements refused in a transaction block — before anything
// is captured for them.
func TestInTransactionUnderWithScriptRefusesAsForReal(t *testing.T) {
	f := pgmotest.New(t)
	s := f.Server()
	ctx, c := pgmo.WithScript(t.Context())
	err := s.InTransaction(ctx, func(ctx context.Context) error {
		if err := s.InTransaction(ctx, func(context.Context) error { return nil }); !errors.Is(err, pgmo.ErrInvalidRequest) {
			t.Errorf("nested: %v, want ErrInvalidRequest", err)
		}
		if _, err := s.ExecIn(ctx, "other", "CREATE SCHEMA x"); !errors.Is(err, pgmo.ErrUnsupported) {
			t.Errorf("another database: %v, want ErrUnsupported", err)
		}
		if _, err := s.ExecIn(ctx, "postgres", "CREATE SCHEMA y"); err != nil {
			t.Errorf("own database: %v", err)
		}
		// A read in the own database runs on the Server's own Querier.
		if q, err := s.OnDatabase(ctx, "postgres"); err != nil || q != pgmo.Querier(f) {
			t.Errorf("read in own database: %v, %v; want the Server's own Querier", q, err)
		}
		if err := pgmo.RefuseInTx(ctx, s, "vacuum"); !errors.Is(err, pgmo.ErrUnsupported) {
			t.Errorf("refuseInTx: %v, want ErrUnsupported", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := pgmo.RefuseInTx(ctx, s, "vacuum"); err != nil {
		t.Errorf("refuseInTx outside the scripted transaction: %v", err)
	}
	if got := c.String(); got != "BEGIN;\n\nCREATE SCHEMA y;\n\nCOMMIT;\n" {
		t.Errorf("script %q", got)
	}
	wantSent(t, f)
}

// String terminates each statement once — after a trailing comment on a line
// of its own — and opens each run of a multi-database script with the
// database it needs.
func TestScriptCollectorString(t *testing.T) {
	var c pgmo.ScriptCollector
	if c.String() != "" {
		t.Errorf("empty collector: %q", c.String())
	}
	c.Add(pgmo.ScriptEntry{Database: "a", SQL: "CREATE SCHEMA s"})
	if got := c.String(); got != "CREATE SCHEMA s;\n" {
		t.Errorf("one database: %q", got)
	}
	c.Add(
		pgmo.ScriptEntry{Database: "a", SQL: "DROP SCHEMA s;  \n"},
		pgmo.ScriptEntry{Database: "b\n*/x", SQL: "SELECT 1 -- note"},
		pgmo.ScriptEntry{Database: "b\n*/x", SQL: "SELECT ';'"},
		pgmo.ScriptEntry{Database: "a", SQL: "SELECT 1 /* c; */"},
	)
	want := "-- database a\nCREATE SCHEMA s;\n\nDROP SCHEMA s;\n\n" +
		"-- database b */x\nSELECT 1 -- note\n;\n\nSELECT ';';\n\n" +
		"-- database a\nSELECT 1 /* c; */;\n"
	if got := c.String(); got != want {
		t.Errorf("String:\n%s\nwant:\n%s", got, want)
	}
}

// The collector is safe for writes from several goroutines with one context.
func TestScriptCollectorConcurrentCaptures(t *testing.T) {
	f := pgmotest.New(t)
	s := f.Server()
	ctx, c := pgmo.WithScript(t.Context())
	done := make(chan struct{})
	for range 8 {
		go func() {
			defer func() { done <- struct{}{} }()
			for range 50 {
				_, _ = s.Exec(ctx, "SELECT $1", 1)
				_ = c.Entries()
			}
		}()
	}
	for range 8 {
		<-done
	}
	if c.Len() != 400 {
		t.Errorf("Len %d, want 400", c.Len())
	}
}
