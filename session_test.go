package pgmo

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

// respondWithNotices scripts f: "notices" answers two rows with a notice
// before each and one before the command completes; "fail" answers a notice
// then an error. Anything else gets the default answer.
func respondWithNotices(f *fakeServer) {
	notice := func(msg string) *pgproto3.NoticeResponse {
		return &pgproto3.NoticeResponse{Severity: "NOTICE", SeverityUnlocalized: "NOTICE", Code: "00000", Message: msg}
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.respond = func(be *pgproto3.Backend, query string) bool {
		switch query {
		case "notices":
			be.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{
				{Name: []byte("n"), DataTypeOID: 23, DataTypeSize: 4, TypeModifier: -1},
			}})
			be.Send(notice("n1"))
			be.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("1")}})
			be.Send(notice("n2"))
			be.Send(&pgproto3.DataRow{Values: [][]byte{[]byte("2")}})
			be.Send(notice("done"))
			be.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 2")})
			return true
		case "fail":
			be.Send(notice("before"))
			be.Send(&pgproto3.ErrorResponse{Severity: "ERROR", SeverityUnlocalized: "ERROR", Code: "P0001", Message: "boom"})
			return true
		}
		return false
	}
}

// eventLog records notices and results in the order a session saw them.
type eventLog struct {
	mu     sync.Mutex
	events []string
}

func (l *eventLog) add(e string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.events = append(l.events, e)
}

func (l *eventLog) notice(n *pgconn.Notice) { l.add("notice " + n.Message) }

func (l *eventLog) take() []string {
	l.mu.Lock()
	defer l.mu.Unlock()
	e := l.events
	l.events = nil
	return e
}

// runNotices runs "notices" then "fail" on q, logging each row, the end of
// the rows and the error into l between the notices the handler logs.
func runNotices(t *testing.T, q Querier, l *eventLog) {
	t.Helper()
	ctx := testContext(t)
	rows, err := q.Query(ctx, "notices")
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		l.add(fmt.Sprint("row ", n))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	l.add("end")
	_, err = q.Exec(ctx, "fail")
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "P0001" {
		t.Fatalf(`Exec("fail") = %v, want the server's P0001`, err)
	}
	l.add("error")
}

// TestSessionNoticesArriveInOrderWithResults: a session's handler sees each
// notice before the row, completion or error the server sent after it.
func TestSessionNoticesArriveInOrderWithResults(t *testing.T) {
	f := startFakeServer(t, 170002)
	respondWithNotices(f)
	s, err := Connect(testContext(t), f.options(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	var l eventLog
	conn, err := s.OpenSession(testContext(t), "", l.notice)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })

	runNotices(t, conn, &l)
	want := []string{"notice n1", "row 1", "notice n2", "row 2", "notice done", "end", "notice before", "error"}
	if got := l.take(); !slices.Equal(got, want) {
		t.Errorf("events:\n got %q\nwant %q", got, want)
	}
}

// TestSessionNoticesStayInTheirSession: each session's handler sees its own
// notices only, and the Server's pool reaches none of them.
func TestSessionNoticesStayInTheirSession(t *testing.T) {
	f := startFakeServer(t, 170002)
	respondWithNotices(f)
	s, err := Connect(testContext(t), f.options(t))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	var a, b eventLog
	ca, err := s.OpenSession(testContext(t), "", a.notice)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ca.Close(context.Background()) })
	cb, err := s.OpenSession(testContext(t), "", b.notice)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cb.Close(context.Background()) })

	var pool eventLog
	runNotices(t, s.Querier(), &pool)
	runNotices(t, ca, &a)
	if got := b.take(); got != nil {
		t.Errorf("session b saw %q from the pool and session a", got)
	}
	if got := a.take(); len(got) != 8 {
		t.Errorf("session a saw %q, want its own 5 notices among its results", got)
	}
	runNotices(t, cb, &b)
	if got := a.take(); got != nil {
		t.Errorf("session a saw %q from session b", got)
	}

	// No handler: the notices are dropped, the results unchanged.
	cn, err := s.OpenSession(testContext(t), "", nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { cn.Close(context.Background()) })
	var none eventLog
	runNotices(t, cn, &none)
	if got := none.take(); !slices.Equal(got, []string{"row 1", "row 2", "end", "error"}) {
		t.Errorf("without a handler: %q", got)
	}
}

// TestOpenSessionIsTheCallers: a session is a connection of its own to the
// database asked for (the Server's own for ""), outside the Server's pools,
// left open by Server.Close and ended by the caller's Close.
func TestOpenSessionIsTheCallers(t *testing.T) {
	f := startFakeServer(t, 170002)
	s, err := Connect(testContext(t), f.options(t))
	if err != nil {
		t.Fatal(err)
	}
	ctx := testContext(t)
	own, err := s.OpenSession(ctx, "", nil)
	if err != nil {
		t.Fatal(err)
	}
	other, err := s.OpenSession(ctx, "other", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := f.dbs(); !slices.Equal(got, []string{"d", "d", "other"}) {
		t.Errorf("sessions to %q, want the pool's, then d, then other", got)
	}
	if db := currentDatabase(t, other); db != "other" {
		t.Errorf("current_database() on the session = %q", db)
	}
	if got := openPools(s); len(got) != 0 {
		t.Errorf("a session was added to the database pools: %q", got)
	}
	if conn, _ := s.Pool().Acquire(ctx); conn != nil {
		if conn.Conn() == own || conn.Conn() == other {
			t.Error("a session is one of the pool's connections")
		}
		conn.Release()
	}

	s.Close()
	if _, open, _ := f.seen(t, 2); open != 2 {
		t.Fatalf("after Server.Close %d sessions open, want the caller's 2", open)
	}
	if db := currentDatabase(t, own); db != "d" {
		t.Errorf("current_database() on the session after Server.Close = %q", db)
	}
	if _, err := s.OpenSession(ctx, "", nil); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("OpenSession after Close: %v, want ErrInvalidRequest", err)
	}
	own.Close(ctx)
	other.Close(ctx)
	if _, open, _ := f.seen(t, 0); open != 0 {
		t.Errorf("after the caller's Close %d sessions open, want 0", open)
	}
}

// TestOpenSessionOverAPoolKeepsItsHooks: over NewServer's pool, a session
// runs the pool's BeforeConnect and AfterConnect, and the pool's own
// OnNotice still sees the notices, before the session's handler.
func TestOpenSessionOverAPoolKeepsItsHooks(t *testing.T) {
	f := startFakeServer(t, 170002)
	respondWithNotices(f)
	opts := f.options(t)
	cfg, err := opts.poolConfig()
	if err != nil {
		t.Fatal(err)
	}
	var l eventLog
	var mu sync.Mutex
	var before, after []string
	cfg.BeforeConnect = func(_ context.Context, cc *pgx.ConnConfig) error {
		mu.Lock()
		defer mu.Unlock()
		before = append(before, cc.Database)
		return nil
	}
	cfg.AfterConnect = func(_ context.Context, c *pgx.Conn) error {
		mu.Lock()
		defer mu.Unlock()
		after = append(after, c.Config().Database)
		return nil
	}
	cfg.ConnConfig.OnNotice = func(_ *pgconn.PgConn, n *pgconn.Notice) { l.add("pool " + n.Message) }
	ctx := testContext(t)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(ctx, pool)
	if err != nil {
		pool.Close()
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	conn, err := s.OpenSession(ctx, "other", l.notice)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	mu.Lock()
	gotBefore, gotAfter := slices.Clone(before), slices.Clone(after)
	mu.Unlock()
	if !slices.Equal(gotBefore, []string{"d", "other"}) || !slices.Equal(gotAfter, []string{"d", "other"}) {
		t.Errorf("BeforeConnect saw %q, AfterConnect %q; want the pool's d, then the session's other", gotBefore, gotAfter)
	}
	if _, err := conn.Exec(ctx, "fail"); err == nil {
		t.Fatal(`Exec("fail") succeeded`)
	}
	if got := l.take(); !slices.Equal(got, []string{"pool before", "notice before"}) {
		t.Errorf("notices: %q, want the pool's handler then the session's", got)
	}

	// A failing hook fails the open, and an AfterConnect failure closes
	// the session it was given.
	cfg2 := pool.Config()
	cfg2.AfterConnect = func(context.Context, *pgx.Conn) error { return errors.New("refused by hook") }
	cfg2.MinConns = 0
	pool2, err := pgxpool.NewWithConfig(ctx, cfg2)
	if err != nil {
		t.Fatal(err)
	}
	s2 := &Server{q: pool2, database: "d"}
	t.Cleanup(s2.Close)
	if _, err := s2.OpenSession(ctx, "", nil); err == nil || !strings.Contains(err.Error(), "refused by hook") ||
		!strings.HasPrefix(err.Error(), `pgmo: open session on "d": `) {
		t.Errorf("AfterConnect failing: %v", err)
	}
	if _, open, _ := f.seen(t, 2); open != 2 {
		t.Errorf("%d sessions open after a refused AfterConnect, want 2 (the pool's and the session's)", open)
	}
}

// TestOpenSessionNeedsAConfiguration: a Server over a single connection has
// nothing to open a session from.
func TestOpenSessionNeedsAConfiguration(t *testing.T) {
	f := startFakeServer(t, 170002)
	opts := f.options(t)
	cfg, err := opts.poolConfig()
	if err != nil {
		t.Fatal(err)
	}
	ctx := testContext(t)
	conn, err := pgx.ConnectConfig(ctx, cfg.ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(ctx, conn)
	if err != nil {
		conn.Close(ctx)
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	_, err = s.OpenSession(ctx, "", nil)
	if !errors.Is(err, ErrUnsupported) || !strings.HasPrefix(err.Error(), `pgmo: open session on "d": `) {
		t.Errorf("over a *pgx.Conn: %v, want ErrUnsupported naming the session", err)
	}
}
