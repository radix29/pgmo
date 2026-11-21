package pgmo

import (
	"context"
	"errors"
	"net"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgproto3"
	"github.com/jackc/pgx/v5/pgxpool"
)

// fakeServer is the least of a PostgreSQL server that Connect and NewServer
// need: it trusts every login and answers every simple-protocol query with
// one row holding version and the session's database as text, as the server
// answers versionQuery. It counts what it saw, so a test can tell whether
// pgmo dialled at all, to which database, and whether its pool was closed.
type fakeServer struct {
	ln      net.Listener
	version int

	mu         sync.Mutex
	queries    []string
	databases  []string // each session's startup database, in order
	open       int      // sessions not yet ended
	terminated int      // sessions the client ended with Terminate

	// respond, when set, answers a query before the default answer: it
	// sends everything up to ReadyForQuery and reports true, or reports
	// false to leave the query to the default.
	respond func(be *pgproto3.Backend, query string) bool

	// The query "sleep" runs until a cancel request names its session
	// (each session has its own process ID and key), then fails with
	// 57014 as a server's does; with ignoreCancel it never answers.
	pids           uint32
	sleeping       map[uint32]chan struct{} // the session's sleep in flight
	cancelRequests []uint32                 // the sessions cancel requests named
	ignoreCancel   bool

	done chan struct{} // closed when the test ends
}

// startFakeServer listens on a loopback port until the test ends.
func startFakeServer(t *testing.T, version int) *fakeServer {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	f := &fakeServer{ln: ln, version: version, sleeping: map[uint32]chan struct{}{}, done: make(chan struct{})}
	t.Cleanup(func() { close(f.done); ln.Close() })
	go func() {
		for {
			c, err := ln.Accept()
			if err != nil {
				return
			}
			go f.serve(c)
		}
	}()
	return f
}

// options are ConnectionOptions reaching f with nothing taken from the
// environment, and the simple protocol, which is all f speaks.
func (f *fakeServer) options(t *testing.T) ConnectionOptions {
	t.Helper()
	clearLibpqEnv(t)
	return ConnectionOptions{
		Host:       "127.0.0.1",
		Port:       f.ln.Addr().(*net.TCPAddr).Port,
		User:       "u",
		Database:   "d",
		SSLMode:    SSLModeDisable,
		ConnString: "default_query_exec_mode=simple_protocol",
	}
}

func (f *fakeServer) serve(c net.Conn) {
	defer c.Close()
	f.mu.Lock()
	f.open++
	f.mu.Unlock()
	defer func() {
		f.mu.Lock()
		f.open--
		f.mu.Unlock()
	}()
	be := pgproto3.NewBackend(c, c)
	var database string
	for {
		msg, err := be.ReceiveStartupMessage()
		if err != nil {
			return
		}
		if _, ok := msg.(*pgproto3.SSLRequest); ok {
			if _, err := c.Write([]byte{'N'}); err != nil {
				return
			}
			continue
		}
		if cr, ok := msg.(*pgproto3.CancelRequest); ok {
			f.cancel(cr)
			return
		}
		if sm, ok := msg.(*pgproto3.StartupMessage); ok {
			database = sm.Parameters["database"]
		}
		break
	}
	f.mu.Lock()
	f.databases = append(f.databases, database)
	f.pids++
	pid := f.pids
	f.mu.Unlock()
	be.Send(&pgproto3.AuthenticationOk{})
	for _, p := range [][2]string{
		{"server_version", "fake"}, {"client_encoding", "UTF8"},
		{"standard_conforming_strings", "on"}, {"DateStyle", "ISO, MDY"},
	} {
		be.Send(&pgproto3.ParameterStatus{Name: p[0], Value: p[1]})
	}
	be.Send(&pgproto3.BackendKeyData{ProcessID: pid, SecretKey: cancelKey(pid)})
	be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	if be.Flush() != nil {
		return
	}
	for {
		msg, err := be.Receive()
		if err != nil {
			return
		}
		switch m := msg.(type) {
		case *pgproto3.Query:
			f.mu.Lock()
			f.queries = append(f.queries, m.String)
			respond := f.respond
			f.mu.Unlock()
			if m.String == "sleep" {
				if !f.sleep(be, pid) {
					return
				}
				continue
			}
			if respond != nil && respond(be, m.String) {
				be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
				if be.Flush() != nil {
					return
				}
				continue
			}
			be.Send(&pgproto3.RowDescription{Fields: []pgproto3.FieldDescription{
				{Name: []byte("current_setting"), DataTypeOID: 23, DataTypeSize: 4, TypeModifier: -1},
				{Name: []byte("current_database"), DataTypeOID: 19, DataTypeSize: 64, TypeModifier: -1},
			}})
			be.Send(&pgproto3.DataRow{Values: [][]byte{[]byte(strconv.Itoa(f.version)), []byte(database)}})
			be.Send(&pgproto3.CommandComplete{CommandTag: []byte("SELECT 1")})
			be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
			if be.Flush() != nil {
				return
			}
		case *pgproto3.Terminate:
			f.mu.Lock()
			f.terminated++
			f.mu.Unlock()
			return
		default:
			return
		}
	}
}

// cancelKey is session pid's secret key, which a cancel request must repeat.
func cancelKey(pid uint32) []byte {
	return []byte{0xca, 0x9c, byte(pid >> 8), byte(pid)}
}

// cancel ends the sleep of the session a cancel request names, if it gives
// that session's key, as a server does; a wrong key is ignored.
func (f *fakeServer) cancel(cr *pgproto3.CancelRequest) {
	if string(cr.SecretKey) != string(cancelKey(cr.ProcessID)) {
		return
	}
	f.mu.Lock()
	defer f.mu.Unlock()
	f.cancelRequests = append(f.cancelRequests, cr.ProcessID)
	if c, ok := f.sleeping[cr.ProcessID]; ok && !f.ignoreCancel {
		close(c)
		delete(f.sleeping, cr.ProcessID)
	}
}

// sleep answers "sleep" on session pid: 57014 once a cancel request names
// it. It reports false when the test ended first.
func (f *fakeServer) sleep(be *pgproto3.Backend, pid uint32) bool {
	c := make(chan struct{})
	f.mu.Lock()
	f.sleeping[pid] = c
	f.mu.Unlock()
	select {
	case <-c:
	case <-f.done:
		return false
	}
	be.Send(&pgproto3.ErrorResponse{Severity: "ERROR", SeverityUnlocalized: "ERROR", Code: "57014", Message: "canceling statement due to user request"})
	be.Send(&pgproto3.ReadyForQuery{TxStatus: 'I'})
	return be.Flush() == nil
}

// cancelled is the sessions cancel requests have named, in order.
func (f *fakeServer) cancelled() []uint32 {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]uint32(nil), f.cancelRequests...)
}

// seen is how many queries f has answered and how many sessions it holds
// open, once those sessions have settled (a closed pool's Terminate is sent
// asynchronously to Close returning only on the client side).
func (f *fakeServer) seen(t *testing.T, wantOpen int) (queries []string, open, terminated int) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		f.mu.Lock()
		queries, open, terminated = append([]string(nil), f.queries...), f.open, f.terminated
		f.mu.Unlock()
		if open == wantOpen || time.Now().After(deadline) {
			return queries, open, terminated
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func testContext(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx
}

// TestConnectReadsTheVersion: Connect dials, asks the server for
// server_version_num through pg_catalog (never the search_path), and keeps
// the answer; Close ends the session and cancels Context.
func TestConnectReadsTheVersion(t *testing.T) {
	f := startFakeServer(t, 170002)
	s, err := Connect(testContext(t), f.options(t))
	if err != nil {
		t.Fatal(err)
	}
	if s.Version() != 170002 {
		t.Errorf("Version() = %d, want 170002", s.Version())
	}
	if s.Pool() == nil {
		t.Fatal("Pool() is nil")
	}
	if err := s.Context().Err(); err != nil {
		t.Fatalf("Context() is done before Close: %v", err)
	}
	q, _, _ := f.seen(t, 1)
	if len(q) != 1 || !strings.Contains(q[0], "pg_catalog.current_setting('server_version_num')") {
		t.Errorf("queries = %q, want the one pg_catalog.current_setting('server_version_num') read", q)
	}
	s.Close()
	if s.Context().Err() == nil {
		t.Error("Context() is not done after Close")
	}
	if _, open, term := f.seen(t, 0); open != 0 || term != 1 {
		t.Errorf("after Close: %d sessions open, %d ended by Terminate; want 0 and 1", open, term)
	}
	s.Close() // a second Close does nothing
}

// TestConnectRefusesAnOldServer: a server below MinimumServerVersion fails
// Connect with ErrUnsupportedVersion naming both versions, and the pool
// Connect opened is closed rather than leaked.
func TestConnectRefusesAnOldServer(t *testing.T) {
	f := startFakeServer(t, 140012)
	s, err := Connect(testContext(t), f.options(t))
	if err == nil {
		s.Close()
		t.Fatal("Connect to a PostgreSQL 14.12 server succeeded")
	}
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("err = %v, want ErrUnsupportedVersion", err)
	}
	const want = "pgmo: connect: server is PostgreSQL 14.12; pgmo needs PostgreSQL 15 or later"
	if err.Error() != want {
		t.Errorf("err = %q, want %q", err, want)
	}
	if _, open, term := f.seen(t, 0); open != 0 || term != 1 {
		t.Errorf("after the refusal: %d sessions open, %d ended by Terminate; want 0 and 1 (the pool closed)", open, term)
	}
}

// TestConnectRefusesOptionsBeforeDialling: a field pgmo refuses fails
// Connect with ErrInvalidRequest under the options' own prefix, and nothing
// reaches the server.
func TestConnectRefusesOptionsBeforeDialling(t *testing.T) {
	f := startFakeServer(t, 170002)
	o := f.options(t)
	o.Params = map[string]string{"host": "elsewhere"}
	_, err := Connect(testContext(t), o)
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
	if !strings.HasPrefix(err.Error(), "pgmo: connection options: ") {
		t.Errorf("err = %q, want the connection options prefix alone", err)
	}
	if q, _, _ := f.seen(t, 0); len(q) != 0 {
		t.Errorf("queries = %q, want none: Connect dialled before refusing", q)
	}
}

// TestConnectFailureKeepsThePasswordOut: a dial that fails is reported under
// Connect's prefix, is not an argument refusal, and never quotes Password.
func TestConnectFailureKeepsThePasswordOut(t *testing.T) {
	clearLibpqEnv(t)
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := ln.Addr().(*net.TCPAddr).Port
	ln.Close() // nothing listens on port now
	const secret = "s3cret-Passw0rd"
	_, err = Connect(testContext(t), ConnectionOptions{
		Host: "127.0.0.1", Port: port, User: "u", Database: "d",
		Password: secret, SSLMode: SSLModeDisable, ConnectTimeout: 2 * time.Second,
	})
	if err == nil {
		t.Fatal("Connect to a closed port succeeded")
	}
	if !strings.HasPrefix(err.Error(), "pgmo: connect: ") || strings.Count(err.Error(), "pgmo:") != 1 {
		t.Errorf("err = %q, want one pgmo: connect: prefix", err)
	}
	if errors.Is(err, ErrInvalidRequest) || errors.Is(err, ErrUnsupportedVersion) {
		t.Errorf("err = %v wraps a pgmo sentinel; a failed dial is not a refusal", err)
	}
	if strings.Contains(err.Error(), secret) {
		t.Errorf("err = %q quotes the password", err)
	}
}

// TestNewServerWrapsAPool: NewServer reads the version over the caller's
// pool, returns that very pool from Pool(), and closes it on Close.
func TestNewServerWrapsAPool(t *testing.T) {
	f := startFakeServer(t, 180001)
	cfg, err := f.options(t).poolConfig()
	if err != nil {
		t.Fatal(err)
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
	if s.Pool() != pool {
		t.Error("Pool() is not the pool NewServer was given")
	}
	if s.Version() != 180001 {
		t.Errorf("Version() = %d, want 180001", s.Version())
	}
	s.Close()
	if _, open, _ := f.seen(t, 0); open != 0 {
		t.Errorf("after Close: %d sessions open, want the pool closed", open)
	}
}

// TestNewServerRefusesAnOldServer: the refusal is NewServer's, under its
// prefix, and the caller's pool stays open — the caller still owns it.
func TestNewServerRefusesAnOldServer(t *testing.T) {
	f := startFakeServer(t, 130000)
	cfg, err := f.options(t).poolConfig()
	if err != nil {
		t.Fatal(err)
	}
	ctx := testContext(t)
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pool.Close)
	_, err = NewServer(ctx, pool)
	if !errors.Is(err, ErrUnsupportedVersion) {
		t.Fatalf("err = %v, want ErrUnsupportedVersion", err)
	}
	const want = "pgmo: new server: server is PostgreSQL 13; pgmo needs PostgreSQL 15 or later"
	if err.Error() != want {
		t.Errorf("err = %q, want %q", err, want)
	}
	if err := pool.Ping(ctx); err != nil {
		t.Errorf("the caller's pool is unusable after the refusal: %v", err)
	}
}

// TestNewServerRefusesANilQuerier: nil, and a nil pointer of each pgx type
// (whose first statement would panic), are refused before any statement.
func TestNewServerRefusesANilQuerier(t *testing.T) {
	for _, q := range []Querier{nil, (*pgxpool.Pool)(nil), (*pgxpool.Conn)(nil), (*pgx.Conn)(nil)} {
		_, err := NewServer(context.Background(), q)
		if !errors.Is(err, ErrInvalidRequest) || err.Error() != "pgmo: new server: querier is nil" {
			t.Errorf("NewServer(%T nil): err = %v, want the ErrInvalidRequest refusal of a nil querier", q, err)
		}
	}
}

// TestNewServerOverAConn: a single *pgx.Conn is a Querier too. The Server
// reads its version over it, has no Pool, and Close closes the connection.
func TestNewServerOverAConn(t *testing.T) {
	f := startFakeServer(t, 160004)
	cfg, err := f.options(t).poolConfig()
	if err != nil {
		t.Fatal(err)
	}
	conn, err := pgx.ConnectConfig(testContext(t), cfg.ConnConfig)
	if err != nil {
		t.Fatal(err)
	}
	s, err := NewServer(testContext(t), conn)
	if err != nil {
		conn.Close(context.Background())
		t.Fatal(err)
	}
	if s.Version() != 160004 {
		t.Errorf("Version() = %d, want 160004", s.Version())
	}
	if s.Querier() != conn {
		t.Error("Querier() is not the connection NewServer was given")
	}
	if s.Pool() != nil {
		t.Error("Pool() is not nil over a *pgx.Conn")
	}
	s.Close()
	if !conn.IsClosed() {
		t.Error("Close left the connection open")
	}
	if _, open, term := f.seen(t, 0); open != 0 || term != 1 {
		t.Errorf("after Close: %d sessions open, %d ended by Terminate; want 0 and 1", open, term)
	}
	s.Close()
}

// stubTx is a pgx.Tx that answers the version read and records how it was
// ended; every other method is the nil embedded interface's (a panic).
type stubTx struct {
	pgx.Tx
	ended []string
}

func (tx *stubTx) QueryRow(context.Context, string, ...any) pgx.Row { return stubRow{} }
func (tx *stubTx) Commit(context.Context) error {
	tx.ended = append(tx.ended, "commit")
	return nil
}
func (tx *stubTx) Rollback(context.Context) error {
	tx.ended = append(tx.ended, "rollback")
	return nil
}

type stubRow struct{}

func (stubRow) Scan(dest ...any) error {
	*dest[0].(*int) = 170000
	return nil
}

// TestCloseLeavesATransactionToTheCaller: a Server over a pgx.Tx neither
// commits nor rolls it back on Close; the caller who began it ends it.
func TestCloseLeavesATransactionToTheCaller(t *testing.T) {
	tx := &stubTx{}
	s, err := NewServer(context.Background(), tx)
	if err != nil {
		t.Fatal(err)
	}
	if s.Querier() != Querier(tx) || s.Pool() != nil {
		t.Errorf("Querier() = %v, Pool() = %v; want the tx and nil", s.Querier(), s.Pool())
	}
	s.Close()
	if len(tx.ended) != 0 {
		t.Errorf("Close ended the caller's transaction: %q", tx.ended)
	}
	if s.Context().Err() == nil {
		t.Error("Context() is not done after Close")
	}
}

// TestCheckServerVersion holds the floor exactly at MinimumServerVersion.
func TestCheckServerVersion(t *testing.T) {
	for _, tc := range []struct {
		v  ServerVersion
		ok bool
	}{
		{140099, false}, {149999, false}, {150000, true}, {150010, true}, {180001, true},
	} {
		err := checkServerVersion(tc.v)
		if (err == nil) != tc.ok {
			t.Errorf("checkServerVersion(%d) = %v, want ok %v", tc.v, err, tc.ok)
		}
		if err != nil && !errors.Is(err, ErrUnsupportedVersion) {
			t.Errorf("checkServerVersion(%d) = %v, want ErrUnsupportedVersion", tc.v, err)
		}
	}
}

// TestServerWithoutALifetime: a Server not built by Connect or NewServer (a
// test's literal), and a nil one, answer Context with Background, and Close
// on the literal does nothing.
func TestServerWithoutALifetime(t *testing.T) {
	var nilServer *Server
	if nilServer.Context() != context.Background() {
		t.Error("nil Server: Context() is not Background")
	}
	s := &Server{}
	if s.Context() != context.Background() {
		t.Error("literal Server: Context() is not Background")
	}
	s.Close()
}
