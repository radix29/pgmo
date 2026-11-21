package pgmo

import (
	"context"
	"errors"
	"slices"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// currentDatabase reads current_database() through q, which fakeServer
// answers from the session's startup database.
func currentDatabase(t *testing.T, q Querier) string {
	t.Helper()
	var v int
	var db string
	if err := q.QueryRow(testContext(t), versionQuery).Scan(&v, &db); err != nil {
		t.Fatalf("read through the database querier: %v", err)
	}
	return db
}

// TestDatabaseQuerierOpensAPoolPerDatabase: the Server's own database is its
// own pool; another is a session dialled to that database, opened on the
// first statement and not before, and reused after.
func TestDatabaseQuerierOpensAPoolPerDatabase(t *testing.T) {
	f := startFakeServer(t, 170002)
	s, err := Connect(testContext(t), f.options(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if s.CurrentDatabase() != "d" {
		t.Errorf("CurrentDatabase() = %q, want d", s.CurrentDatabase())
	}
	if q := s.DatabaseQuerier("d"); q != Querier(s.Pool()) {
		t.Errorf("DatabaseQuerier(current) = %T, want the Server's own pool", q)
	}

	other := s.DatabaseQuerier("other")
	f.seen(t, 1)
	if got := f.dbs(); !slices.Equal(got, []string{"d"}) {
		t.Fatalf("before any statement: sessions to %q, want only d's", got)
	}
	for range 2 {
		if db := currentDatabase(t, other); db != "other" {
			t.Errorf("current_database() over DatabaseQuerier(other) = %q", db)
		}
	}
	if got := f.dbs(); !slices.Equal(got, []string{"d", "other"}) {
		t.Errorf("sessions to %q, want d's then one to other", got)
	}
	if db := currentDatabase(t, s.DatabaseQuerier("Other")); db != "Other" {
		t.Errorf("names are not matched exactly: Other reached %q", db)
	}
}

func (f *fakeServer) dbs() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.databases)
}

// TestDatabasePoolsEvictTheLeastRecentlyUsedIdleOne: over the cap, opening
// a database closes the least recently used idle pool; a busy one (rows
// still open) is kept, so the cap is exceeded, until it is idle. An evicted
// database's handle reopens it.
func TestDatabasePoolsEvictTheLeastRecentlyUsedIdleOne(t *testing.T) {
	f := startFakeServer(t, 170002)
	s, err := Connect(testContext(t), f.options(t), WithMaxDatabasePools(2))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	ctx := testContext(t)
	a, b, c := s.DatabaseQuerier("a"), s.DatabaseQuerier("b"), s.DatabaseQuerier("c")

	currentDatabase(t, a)
	currentDatabase(t, b)
	currentDatabase(t, a) // b is now the least recently used
	currentDatabase(t, c)
	if got := openPools(s); !slices.Equal(got, []string{"a", "c"}) {
		t.Errorf("pools after a, b, a, c under a cap of 2 = %q, want b evicted", got)
	}
	if _, _, term := f.seen(t, 3); term != 1 {
		t.Errorf("%d sessions ended by Terminate, want b's one", term)
	}

	// a and c busy: opening b goes over the cap.
	ra, err := a.Query(ctx, versionQuery)
	if err != nil {
		t.Fatal(err)
	}
	rc, err := c.Query(ctx, versionQuery)
	if err != nil {
		t.Fatal(err)
	}
	currentDatabase(t, b)
	if got := openPools(s); !slices.Equal(got, []string{"a", "b", "c"}) {
		t.Errorf("pools with a and c busy = %q, want all three", got)
	}
	ra.Close()
	rc.Close()
	currentDatabase(t, b) // a lease ends: the least recently used idle one goes
	if got := openPools(s); !slices.Equal(got, []string{"b", "c"}) {
		t.Errorf("pools once a and c are idle = %q, want a evicted", got)
	}
	if db := currentDatabase(t, a); db != "a" {
		t.Errorf("an evicted database's handle reached %q", db)
	}
}

// openPools lists the databases with an open pool.
func openPools(s *Server) []string {
	s.mu.Lock()
	defer s.mu.Unlock()
	var names []string
	for name, p := range s.pools {
		if p.q != nil {
			names = append(names, name)
		}
	}
	slices.Sort(names)
	return names
}

// TestDatabasePoolsAreCapped: a derived pool opens at most
// WithDatabasePoolMaxConns sessions, never more than the Server's own pool
// allows, and dials nothing ahead of a statement whatever pool_min_conns
// says; over a pool, it keeps the pool's configuration.
func TestDatabasePoolsAreCapped(t *testing.T) {
	f := startFakeServer(t, 170002)
	opts := f.options(t)
	opts.ConnString += " pool_max_conns=3 pool_min_conns=2"
	s, err := Connect(testContext(t), opts, WithDatabasePoolMaxConns(5))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	cfg, err := s.databasePoolConfig("x")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxConns != 3 || cfg.MinConns != 0 || cfg.MinIdleConns != 0 {
		t.Errorf("derived over pool_max_conns=3: MaxConns %d, MinConns %d, MinIdleConns %d; want 3, 0, 0",
			cfg.MaxConns, cfg.MinConns, cfg.MinIdleConns)
	}
	if cfg.ConnConfig.Database != "x" || cfg.ConnConfig.User != "u" {
		t.Errorf("derived config: database %q, user %q", cfg.ConnConfig.Database, cfg.ConnConfig.User)
	}

	pcfg, err := opts.poolConfig()
	if err != nil {
		t.Fatal(err)
	}
	var hooked atomic.Bool // BeforeClose runs on the pool's own goroutine
	pcfg.MaxConns = 10
	pcfg.BeforeClose = func(*pgx.Conn) { hooked.Store(true) }
	pool, err := pgxpool.NewWithConfig(testContext(t), pcfg)
	if err != nil {
		t.Fatal(err)
	}
	s2, err := NewServer(testContext(t), pool)
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	t.Cleanup(s2.Close)
	cfg, err = s2.databasePoolConfig("y")
	if err != nil {
		t.Fatal(err)
	}
	if cfg.MaxConns != DefaultDatabasePoolMaxConns || cfg.ConnConfig.Database != "y" || cfg.BeforeClose == nil {
		t.Errorf("derived over a pool: MaxConns %d, database %q, hook kept %v", cfg.MaxConns, cfg.ConnConfig.Database, cfg.BeforeClose != nil)
	}
	if db := currentDatabase(t, s2.DatabaseQuerier("y")); db != "y" {
		t.Errorf("NewServer over a pool reached %q", db)
	}
	s2.Close()
	for deadline := time.Now().Add(5 * time.Second); !hooked.Load() && time.Now().Before(deadline); {
		time.Sleep(10 * time.Millisecond)
	}
	if !hooked.Load() {
		t.Error("the derived pool did not keep the wrapped pool's hooks")
	}
}

// TestCloseClosesEveryDatabasePool: Close ends every other database's
// sessions with the Server's own, and a handle fails afterwards.
func TestCloseClosesEveryDatabasePool(t *testing.T) {
	f := startFakeServer(t, 170002)
	s, err := Connect(testContext(t), f.options(t))
	if err != nil {
		t.Fatal(err)
	}
	a := s.DatabaseQuerier("a")
	currentDatabase(t, a)
	currentDatabase(t, s.DatabaseQuerier("b"))
	s.Close()
	if _, open, term := f.seen(t, 0); open != 0 || term != 3 {
		t.Errorf("after Close: %d sessions open, %d ended by Terminate; want 0 and 3", open, term)
	}
	if _, err := a.Exec(testContext(t), "SELECT 1"); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("a statement after Close: %v, want ErrInvalidRequest", err)
	}
	if got := f.dbs(); len(got) != 3 {
		t.Errorf("sessions %q: a statement after Close reopened a pool", got)
	}
}

// TestDatabaseQuerierRefusals: what DatabaseQuerier and the options refuse.
func TestDatabaseQuerierRefusals(t *testing.T) {
	f := startFakeServer(t, 170002)
	ctx := testContext(t)
	for _, opt := range []ServerOption{WithMaxDatabasePools(0), WithDatabasePoolMaxConns(-1), WithDatabaseOpener(nil), nil} {
		if s, err := Connect(ctx, f.options(t), opt); !errors.Is(err, ErrInvalidRequest) {
			if s != nil {
				s.Close()
			}
			t.Errorf("Connect with a bad option: %v, want ErrInvalidRequest", err)
		}
	}
	if q, _, _ := f.seen(t, 0); len(q) != 0 {
		t.Errorf("a refused option still dialled: %q", q)
	}

	s, err := Connect(ctx, f.options(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if _, err := s.DatabaseQuerier("").Query(ctx, "SELECT 1"); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("an empty name: %v, want ErrInvalidRequest", err)
	}

	conn, err := s.Pool().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	s1, err := NewServer(ctx, conn)
	if err != nil {
		conn.Release()
		t.Fatal(err)
	}
	defer s1.Close()
	if err := s1.DatabaseQuerier("other").QueryRow(ctx, "SELECT 1").Scan(new(int)); !errors.Is(err, ErrUnsupported) {
		t.Errorf("over one connection, no opener: %v, want ErrUnsupported", err)
	}

	// An opener's failure, or its nil Querier, fails the statement and is
	// tried again by the next.
	conn2, err := s.Pool().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	openErr := errors.New("no such tenant")
	calls := 0
	s2, err := NewServer(ctx, conn2, WithDatabaseOpener(func(_ context.Context, name string) (Querier, error) {
		calls++
		if calls == 1 {
			return nil, openErr
		}
		return (*pgxpool.Pool)(nil), nil
	}))
	if err != nil {
		conn2.Release()
		t.Fatal(err)
	}
	defer s2.Close()
	if _, err := s2.DatabaseQuerier("t").Exec(ctx, "SELECT 1"); !errors.Is(err, openErr) {
		t.Errorf("an opener's error: %v, want it wrapped", err)
	}
	if _, err := s2.DatabaseQuerier("t").Exec(ctx, "SELECT 1"); !errors.Is(err, ErrInvalidRequest) || calls != 2 {
		t.Errorf("an opener's nil Querier (call %d): %v, want ErrInvalidRequest on a second open", calls, err)
	}
}

// blockingQuerier reports itself idle and holds each Exec until release is
// closed, so only the Server's lease can keep it from eviction mid-call.
type blockingQuerier struct {
	Querier // nil: only Exec is called
	entered chan struct{}
	release chan struct{}
	closed  atomic.Bool
}

func (b *blockingQuerier) Idle() bool { return true }
func (b *blockingQuerier) Close()     { b.closed.Store(true) }
func (b *blockingQuerier) Exec(context.Context, string, ...any) (pgconn.CommandTag, error) {
	b.entered <- struct{}{}
	<-b.release
	if b.closed.Load() {
		return pgconn.CommandTag{}, errors.New("closed under a statement")
	}
	return pgconn.NewCommandTag("SELECT 1"), nil
}

// TestAStatementInFlightIsNotEvicted: a Querier that reports idle is still
// kept open while a statement is inside its call, however many other
// databases open meanwhile.
func TestAStatementInFlightIsNotEvicted(t *testing.T) {
	f := startFakeServer(t, 170002)
	ctx := testContext(t)
	pool, err := Connect(ctx, f.options(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	conn, err := pool.Pool().Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	qs := map[string]*blockingQuerier{}
	var mu sync.Mutex
	s, err := NewServer(ctx, conn, WithMaxDatabasePools(1), WithDatabaseOpener(func(_ context.Context, name string) (Querier, error) {
		mu.Lock()
		defer mu.Unlock()
		b := &blockingQuerier{entered: make(chan struct{}, 1), release: make(chan struct{})}
		qs[name] = b
		return b, nil
	}))
	if err != nil {
		conn.Release()
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	errc := make(chan error, 1)
	go func() {
		_, err := s.DatabaseQuerier("a").Exec(ctx, "SELECT 1")
		errc <- err
	}()
	mu.Lock()
	for qs["a"] == nil {
		mu.Unlock()
		time.Sleep(time.Millisecond)
		mu.Lock()
	}
	a := qs["a"]
	mu.Unlock()
	<-a.entered
	b := s.DatabaseQuerier("b")
	go func() {
		mu.Lock()
		for qs["b"] == nil {
			mu.Unlock()
			time.Sleep(time.Millisecond)
			mu.Lock()
		}
		qb := qs["b"]
		mu.Unlock()
		<-qb.entered
		close(qb.release)
	}()
	if _, err := b.Exec(ctx, "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	if a.closed.Load() {
		t.Error("a was evicted while a statement was inside its call")
	}
	close(a.release)
	if err := <-errc; err != nil {
		t.Error(err)
	}
}
