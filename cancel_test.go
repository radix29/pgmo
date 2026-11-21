package pgmo

import (
	"context"
	"errors"
	"fmt"
	"net"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// cancelAfter is a context cancelled d from now, as a user's Cancel is.
func cancelAfter(t *testing.T, d time.Duration) context.Context {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	time.AfterFunc(d, cancel)
	return ctx
}

// sessions is how many sessions f has had dialled.
func (f *fakeServer) sessions() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.databases)
}

// sleepIsCancelled runs "sleep" through run under a context cancelled 50 ms
// in, and checks the statement failed with the server's 57014 once the
// server answered the cancel request, well before cancelFallback.
func sleepIsCancelled(t *testing.T, run func(context.Context) error) error {
	t.Helper()
	start := time.Now()
	err := run(cancelAfter(t, 50*time.Millisecond))
	if !IsQueryCanceled(err) {
		t.Errorf("cancelled sleep: err = %v, want the server's 57014", err)
	}
	if took := time.Since(start); took >= cancelFallback {
		t.Errorf("cancelled sleep took %v: the session waited out the fallback", took)
	}
	return err
}

// TestCancelKeepsThePoolsSession: a statement whose context is cancelled while
// the server runs it sends a cancel request for the session running it,
// fails with 57014 and ctx's error, and leaves the session in the pool:
// the next statement reuses it, nothing is dialled again.
func TestCancelKeepsThePoolsSession(t *testing.T) {
	f := startFakeServer(t, 170002)
	s, err := Connect(testContext(t), f.options(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	var n int
	for _, read := range []struct {
		name string
		run  func(context.Context) error
	}{
		{"queryRowScan", func(ctx context.Context) error { return queryRowScan(ctx, s.Querier(), "sleep", nil, &n) }},
		{"query", func(ctx context.Context) error {
			_, err := query(ctx, s.Querier(), pgx.RowTo[int32], "sleep")
			return err
		}},
		{"execScan", func(ctx context.Context) error { return execScan(ctx, s.Querier(), "sleep", nil, &n) }},
	} {
		err := sleepIsCancelled(t, read.run)
		if !errors.Is(err, context.Canceled) {
			t.Errorf("%s: err = %v, want it to carry context.Canceled", read.name, err)
		}
	}
	if got := f.cancelled(); !slices.Equal(got, []uint32{1, 1, 1}) {
		t.Errorf("cancel requests named sessions %v, want [1 1 1], the pool's", got)
	}
	if err := queryRowScan(testContext(t), s.Querier(), "after", nil, &n, new(string)); err != nil {
		t.Fatalf("statement after the cancel: %v", err)
	}
	if got := f.sessions(); got != 1 {
		t.Errorf("%d sessions dialled, want the pool's one kept", got)
	}
}

// TestCancelByDeadline: a deadline ending the statement is a cancel too,
// reported with context.DeadlineExceeded; execScan (a write) is answered the
// same way.
func TestCancelByDeadline(t *testing.T) {
	f := startFakeServer(t, 170002)
	s, err := Connect(testContext(t), f.options(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	var n int
	err = execScan(ctx, s.Querier(), "sleep", nil, &n)
	if !IsQueryCanceled(err) || !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err = %v, want 57014 carrying context.DeadlineExceeded", err)
	}
	if err := queryRowScan(testContext(t), s.Querier(), "after", nil, &n, new(string)); err != nil {
		t.Fatalf("statement after the cancel: %v", err)
	}
	if got := f.sessions(); got != 1 {
		t.Errorf("%d sessions dialled, want the pool's one kept", got)
	}
}

// TestCancelKeepsAnOpenSession: a session from OpenSession survives a
// cancel with its state, as a query window needs.
func TestCancelKeepsAnOpenSession(t *testing.T) {
	f := startFakeServer(t, 170002)
	s, err := Connect(testContext(t), f.options(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	conn, err := s.OpenSession(testContext(t), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })

	sleepIsCancelled(t, func(ctx context.Context) error {
		_, err := conn.Exec(ctx, "sleep")
		return err
	})
	if conn.IsClosed() {
		t.Fatal("the session was closed by the cancel")
	}
	if got := f.cancelled(); !slices.Equal(got, []uint32{conn.PgConn().PID()}) {
		t.Errorf("cancel requests named sessions %v, want the session's %d", got, conn.PgConn().PID())
	}
	if _, err := conn.Exec(testContext(t), "after"); err != nil {
		t.Errorf("statement after the cancel: %v", err)
	}
}

// TestCancelKeepsAnotherDatabasesSession: a pool DatabaseQuerier opens is
// built from Connect's options, so it cancels the same way.
func TestCancelKeepsAnotherDatabasesSession(t *testing.T) {
	f := startFakeServer(t, 170002)
	s, err := Connect(testContext(t), f.options(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	q := s.DatabaseQuerier("other")
	var n int
	sleepIsCancelled(t, func(ctx context.Context) error {
		return queryRowScan(ctx, q, "sleep", nil, &n)
	})
	if err := queryRowScan(testContext(t), q, "after", nil, &n, new(string)); err != nil {
		t.Fatalf("statement after the cancel: %v", err)
	}
	if got := f.dbs(); !slices.Equal(got, []string{"d", "other"}) {
		t.Errorf("sessions dialled for %q, want one per database", got)
	}
}

// TestCancelFallsBackToClosingTheSession: a server that never answers the
// cancel request has the session closed after cancelFallback, so a cancel
// still ends the call; the error carries ctx's.
func TestCancelFallsBackToClosingTheSession(t *testing.T) {
	saved := cancelFallback
	cancelFallback = 300 * time.Millisecond
	t.Cleanup(func() { cancelFallback = saved })
	f := startFakeServer(t, 170002)
	f.ignoreCancel = true
	s, err := Connect(testContext(t), f.options(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	conn, err := s.OpenSession(testContext(t), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })

	start := time.Now()
	_, err = conn.Exec(cancelAfter(t, 50*time.Millisecond), "sleep")
	took := time.Since(start)
	if err == nil || !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want it to carry context.Canceled", err)
	}
	if took < cancelFallback || took > cancelFallback+2*time.Second {
		t.Errorf("the call returned after %v, want about cancelFallback (%v) past the cancel", took, cancelFallback)
	}
	if !conn.IsClosed() {
		t.Error("the session is still open after the server ignored the cancel")
	}
	if len(f.cancelled()) == 0 {
		t.Error("no cancel request was sent")
	}
}

// TestNewServerKeepsTheCallersCancel: a pool NewServer is given cancels as
// its own configuration says — pgx's default closes the session — unless
// the caller set UseCancelRequests on it.
func TestNewServerKeepsTheCallersCancel(t *testing.T) {
	for _, use := range []bool{false, true} {
		t.Run(fmt.Sprint("UseCancelRequests=", use), func(t *testing.T) {
			f := startFakeServer(t, 170002)
			f.options(t) // clears the environment
			cfg, err := pgxpool.ParseConfig(fmt.Sprintf(
				"host=127.0.0.1 port=%d user=u dbname=d sslmode=disable default_query_exec_mode=simple_protocol",
				f.ln.Addr().(*net.TCPAddr).Port))
			if err != nil {
				t.Fatal(err)
			}
			if use {
				UseCancelRequests(&cfg.ConnConfig.Config)
			}
			pool, err := pgxpool.NewWithConfig(testContext(t), cfg)
			if err != nil {
				t.Fatal(err)
			}
			s, err := NewServer(testContext(t), pool)
			if err != nil {
				pool.Close()
				t.Fatal(err)
			}
			t.Cleanup(s.Close)
			conn, err := s.OpenSession(testContext(t), "", nil)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { conn.Close(context.Background()) })
			_, err = conn.Exec(cancelAfter(t, 50*time.Millisecond), "sleep")
			if got := IsQueryCanceled(err); got != use {
				t.Errorf("IsQueryCanceled(%v) = %v, want %v", err, got, use)
			}
			if conn.IsClosed() == use {
				t.Errorf("session closed = %v, want %v", conn.IsClosed(), !use)
			}
		})
	}
}

// TestCancelCause: ctx's error is joined to a 57014 that ctx caused, and
// to nothing else.
func TestCancelCause(t *testing.T) {
	canceled := &pgconn.PgError{Code: "57014"}
	other := &pgconn.PgError{Code: "40P01"}
	done, cancel := context.WithCancel(context.Background())
	cancel()
	live := context.Background()
	already := fmt.Errorf("x: %w", context.Canceled)
	for _, tc := range []struct {
		name      string
		ctx       context.Context
		err       error
		joined    bool
		unchanged bool
	}{
		{"nil", done, nil, false, true},
		{"57014 by ctx", done, canceled, true, false},
		{"57014 by statement_timeout", live, canceled, false, true},
		{"another error", done, other, false, true},
		{"already ctx's", done, errors.Join(already, canceled), true, true},
	} {
		got := cancelCause(tc.ctx, tc.err)
		if tc.unchanged && got != tc.err {
			t.Errorf("%s: cancelCause changed %v to %v", tc.name, tc.err, got)
		}
		if errors.Is(got, context.Canceled) != tc.joined {
			t.Errorf("%s: errors.Is(%v, context.Canceled) = %v, want %v", tc.name, got, !tc.joined, tc.joined)
		}
		if tc.err != nil && !errors.Is(got, tc.err) {
			t.Errorf("%s: %v lost the cause %v", tc.name, got, tc.err)
		}
	}
}
