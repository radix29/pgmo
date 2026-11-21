package pgmo

import (
	"context"
	"errors"
	"io"
	"slices"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
)

// ============================================================
// withRetry
// ============================================================

func TestWithRetrySucceedsAfterTransient(t *testing.T) {
	calls := 0
	v, err := withRetry(t.Context(), func() (int, error) {
		calls++
		if calls < 2 {
			return 0, io.ErrUnexpectedEOF
		}
		return 99, nil
	})
	if err != nil || v != 99 || calls != 2 {
		t.Errorf("withRetry = %d, %v after %d calls; want 99, nil after 2", v, err, calls)
	}
}

func TestWithRetryExhausts(t *testing.T) {
	calls := 0
	_, err := withRetry(t.Context(), func() (int, error) {
		calls++
		return 0, pgErr("57P01")
	})
	if !hasSQLState(err, "57P01") {
		t.Errorf("err = %v, want the last 57P01 back", err)
	}
	if calls != readRetryAttempts {
		t.Errorf("calls = %d, want %d", calls, readRetryAttempts)
	}
}

func TestWithRetryStopsOnNonRetryable(t *testing.T) {
	calls := 0
	_, err := withRetry(t.Context(), func() (int, error) {
		calls++
		return 0, pgErr("42P01")
	})
	if !hasSQLState(err, "42P01") || calls != 1 {
		t.Errorf("err = %v after %d calls, want the 42P01 after 1", err, calls)
	}
}

func TestWithRetryHonoursContextCancel(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel() // already cancelled before the backoff
	calls := 0
	_, err := withRetry(ctx, func() (int, error) {
		calls++
		return 0, pgErr("40P01")
	})
	if !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
	if !hasSQLState(err, "40P01") {
		t.Errorf("err = %v, want the transient error that caused the retry kept", err)
	}
	if calls != 1 {
		t.Errorf("calls = %d, want 1 (cancelled before the retry)", calls)
	}
}

// ============================================================
// The statement helpers
// ============================================================

// attempt is how flakyQuerier answers one statement: rows of one int4
// column, then late (through Rows.Err, as pgx reports a failure during
// execution); or err at once.
type attempt struct {
	rows []int32
	late error
	err  error
}

// flakyQuerier answers its n-th statement with attempts[n], the last one
// repeating, and counts them.
type flakyQuerier struct {
	attempts []attempt
	calls    int
}

func (f *flakyQuerier) next() attempt {
	f.calls++
	return f.attempts[min(f.calls, len(f.attempts))-1]
}

func (f *flakyQuerier) Query(context.Context, string, ...any) (pgx.Rows, error) {
	a := f.next()
	if a.err != nil {
		return &intRows{err: a.err}, a.err
	}
	return &intRows{vals: a.rows, i: -1, late: a.late}, nil
}

func (f *flakyQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	rows, _ := f.Query(ctx, sql, args...)
	return intRow{rows.(*intRows)}
}

func (f *flakyQuerier) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	panic("not used")
}

func (f *flakyQuerier) CopyFrom(context.Context, pgx.Identifier, []string, pgx.CopyFromSource) (int64, error) {
	panic("not used")
}

func (f *flakyQuerier) Begin(context.Context) (pgx.Tx, error) { panic("not used") }

// flakyTx is flakyQuerier as a pgx.Tx, which no read retries on.
type flakyTx struct {
	pgx.Tx
	f *flakyQuerier
}

func (x flakyTx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return x.f.Query(ctx, sql, args...)
}

func (x flakyTx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	return x.f.QueryRow(ctx, sql, args...)
}

// intRows is pgx.Rows over one int4 column.
type intRows struct {
	pgx.Rows // the methods CollectRows does not call
	vals     []int32
	i        int
	late     error
	err      error
}

func (r *intRows) Next() bool {
	if r.err != nil {
		return false
	}
	r.i++
	if r.i < len(r.vals) {
		return true
	}
	r.err = r.late
	return false
}

func (r *intRows) Scan(dest ...any) error {
	*dest[0].(*int32) = r.vals[r.i]
	return nil
}

func (r *intRows) Close()                        {}
func (r *intRows) Err() error                    { return r.err }
func (r *intRows) CommandTag() pgconn.CommandTag { return pgconn.CommandTag{} }

// intRow is pgx's QueryRow over intRows: the first row, or pgx.ErrNoRows.
type intRow struct{ r *intRows }

func (row intRow) Scan(dest ...any) error {
	if row.r.err != nil {
		return row.r.err
	}
	if !row.r.Next() {
		if row.r.err != nil {
			return row.r.err
		}
		return pgx.ErrNoRows
	}
	return row.r.Scan(dest...)
}

func scanInt(row pgx.CollectableRow) (int32, error) {
	var v int32
	err := row.Scan(&v)
	return v, err
}

// A read that fails transiently is run again, all of it: rows the failed
// attempt delivered before the failure are discarded, not kept alongside the
// retry's. On a pgx.Tx it is never run again, and a write is never retried.
func TestStatementRetry(t *testing.T) {
	deadlock := pgErr("40P01")
	missing := pgErr("42P01")
	for _, c := range []struct {
		name     string
		inTx     bool
		attempts []attempt
		run      func(context.Context, Querier) ([]int32, error)
		want     []int32
		wantErr  error
		calls    int
	}{
		{"query: late failure after a row", false,
			[]attempt{{rows: []int32{1}, late: deadlock}, {rows: []int32{1, 2}}},
			runQuery, []int32{1, 2}, nil, 2},
		{"query: failing at once until exhausted", false,
			[]attempt{{err: io.EOF}},
			runQuery, nil, io.EOF, readRetryAttempts},
		{"query: not retryable", false,
			[]attempt{{err: missing}, {rows: []int32{1}}},
			runQuery, nil, missing, 1},
		{"query: in a transaction", true,
			[]attempt{{rows: []int32{1}, late: deadlock}, {rows: []int32{1, 2}}},
			runQuery, nil, deadlock, 1},
		{"queryRowScan: transient then a row", false,
			[]attempt{{err: deadlock}, {rows: []int32{7}}},
			runQueryRowScan, []int32{7}, nil, 2},
		{"queryRowScan: no row is not retried", false,
			[]attempt{{}, {rows: []int32{7}}},
			runQueryRowScan, nil, pgx.ErrNoRows, 1},
		{"queryRowScan: in a transaction", true,
			[]attempt{{err: deadlock}, {rows: []int32{7}}},
			runQueryRowScan, nil, deadlock, 1},
		{"execScan: never retried", false,
			[]attempt{{err: io.EOF}, {rows: []int32{7}}},
			runExecScan, nil, io.EOF, 1},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := &flakyQuerier{attempts: c.attempts}
			var q Querier = f
			if c.inTx {
				q = flakyTx{f: f}
			}
			got, err := c.run(t.Context(), q)
			if !errors.Is(err, c.wantErr) || (c.wantErr == nil) != (err == nil) {
				t.Errorf("err = %v, want %v", err, c.wantErr)
			}
			if c.wantErr == nil && !slices.Equal(got, c.want) {
				t.Errorf("result = %v, want %v", got, c.want)
			}
			if f.calls != c.calls {
				t.Errorf("statements sent = %d, want %d", f.calls, c.calls)
			}
		})
	}
}

func runQuery(ctx context.Context, q Querier) ([]int32, error) {
	return query(ctx, q, scanInt, "SELECT 1")
}

func runQueryRowScan(ctx context.Context, q Querier) ([]int32, error) {
	var v int32
	err := queryRowScan(ctx, q, "SELECT 1", nil, &v)
	return []int32{v}, err
}

func runExecScan(ctx context.Context, q Querier) ([]int32, error) {
	var v int32
	err := execScan(ctx, q, "INSERT INTO t DEFAULT VALUES RETURNING 1", nil, &v)
	return []int32{v}, err
}
