package pgmo

import (
	"context"
	"fmt"
	"reflect"
	"sync"

	"github.com/jackc/pgx/v5/pgxpool"
)

// ============================================================
// Server
// ============================================================

// Server is a connection to one PostgreSQL server, the root every catalog
// read and write starts from. Open one with Connect, or wrap a pool (or any
// other Querier) opened elsewhere with NewServer; Close it when done.
//
// A Server opened by Connect, or over a pool, is safe for concurrent use: the
// pool hands each statement a connection of its own. One over a single
// connection (a *pgx.Conn, a *pgxpool.Conn, a pgx.Tx) is not, as that
// connection is not.
type Server struct {
	// q is what every statement goes through: Connect's pool, or what
	// NewServer was given.
	q Querier

	// version is server_version_num, read once when the Server is built and
	// never refreshed: a minor upgrade restarts the server, which ends every
	// session of the pool anyway.
	version ServerVersion

	// database is current_database(), read with version: the one database
	// q reaches. Every other is read through DatabaseQuerier.
	database string

	// opts are the per-database limits; connectOpts are Connect's options,
	// which another database's pool is resolved from (nil after NewServer).
	opts        serverOptions
	connectOpts *ConnectionOptions

	// mu guards the other databases' Queriers, by name, and closed.
	mu     sync.Mutex
	pools  map[string]*databasePool
	clock  uint64 // a lease counter, for least-recently-used
	closed bool

	// ctx is the Server's lifetime, cancelled by Close, so a caller's own
	// background work can end with the connection (see Context). Closing a
	// pgxpool.Pool waits for its connections to be released but does not
	// stop a statement in flight.
	ctx    context.Context
	cancel context.CancelFunc

	closeOnce sync.Once
}

// Connect opens a pool from opts and returns the Server on it, once a
// connection has been made and the server's version read.
//
// opts is resolved as libpq would resolve it (see ConnectionOptions) and
// checked before anything is dialled: a field pgmo refuses fails with
// ErrInvalidRequest naming it. A server older than MinimumServerVersion is
// refused with ErrUnsupportedVersion, and the pool closed.
//
// ctx bounds the first connection and the version read only; the pool's later
// connections are each bounded by the call that needs one, and by
// ConnectionOptions.ConnectTimeout per host.
func Connect(ctx context.Context, opts ConnectionOptions, sopts ...ServerOption) (*Server, error) {
	so, err := applyServerOptions(sopts)
	if err != nil {
		return nil, fmt.Errorf("pgmo: connect: %w", err)
	}
	cfg, err := opts.poolConfig()
	if err != nil {
		return nil, err
	}
	// NewWithConfig dials nothing (pool_min_conns aside, whose dials ignore
	// ctx's deadline); the version read below makes the first connection.
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pgmo: connect: %w", err)
	}
	s, err := newServer(ctx, pool, so)
	if err != nil {
		pool.Close()
		return nil, fmt.Errorf("pgmo: connect: %w", err)
	}
	s.connectOpts = &opts
	return s, nil
}

// NewServer wraps an already-open Querier as a Server, reading the same
// server version Connect reads. Use it when the connection is not pgmo's to
// open: a pool shared with the rest of an application, one configured with
// hooks (BeforeConnect for a cloud IAM token, AfterConnect, a tracer) that
// ConnectionOptions has no field for, a single *pgx.Conn, or package
// pgmotest's fake in a test.
//
// q must reach a PostgreSQL server of MinimumServerVersion or later; an older
// one is refused with ErrUnsupportedVersion and q is left open, as it is on
// any error. On success, ownership passes to the returned Server: Close ends
// q as its type is ended — closes a pool or a *pgx.Conn, releases a
// *pgxpool.Conn — except a pgx.Tx, which stays the caller's to commit or roll
// back.
//
// Another database is reached through a pool on q's configuration when q is
// a *pgxpool.Pool, and through WithDatabaseOpener otherwise (see
// DatabaseQuerier).
//
// ctx bounds the version read only.
func NewServer(ctx context.Context, q Querier, opts ...ServerOption) (*Server, error) {
	if isNilQuerier(q) {
		return nil, invalidf("pgmo: new server: querier is nil")
	}
	so, err := applyServerOptions(opts)
	if err != nil {
		return nil, fmt.Errorf("pgmo: new server: %w", err)
	}
	s, err := newServer(ctx, q, so)
	if err != nil {
		return nil, fmt.Errorf("pgmo: new server: %w", err)
	}
	return s, nil
}

// isNilQuerier is q == nil, or a nil pointer in a Querier (a nil
// *pgxpool.Pool), whose first statement would panic rather than fail.
func isNilQuerier(q Querier) bool {
	if q == nil {
		return true
	}
	v := reflect.ValueOf(q)
	return v.Kind() == reflect.Pointer && v.IsNil()
}

// versionQuery is the read every Server starts with: the server's version
// and the database the session is bound to. integer is a keyword type, not
// resolved through search_path. pgmotest answers it by default: keep the two
// in step.
const versionQuery = `SELECT pg_catalog.current_setting('server_version_num')::integer, pg_catalog.current_database()`

// newServer is the one constructor behind Connect and NewServer: it reads
// q's server version and database, refuses a version below
// MinimumServerVersion, and gives the Server its lifetime. The read is
// retried on a transient failure (queryRowScan), so a refused or dropped
// first connection is tried readRetryAttempts times. It leaves q open on
// error; the caller decides whose it is to close. Its error carries no
// prefix.
func newServer(ctx context.Context, q Querier, opts serverOptions) (*Server, error) {
	var num int
	var database string
	if err := queryRowScan(ctx, q, versionQuery, nil, &num, &database); err != nil {
		return nil, fmt.Errorf("read server version: %w", err)
	}
	if err := checkServerVersion(ServerVersion(num)); err != nil {
		return nil, err
	}
	lifetime, cancel := context.WithCancel(context.Background())
	return &Server{
		q: q, version: ServerVersion(num), database: database,
		opts: opts, pools: map[string]*databasePool{},
		ctx: lifetime, cancel: cancel,
	}, nil
}

// Close cancels Context, closes every other database's Querier, then ends
// the Server's own as NewServer describes: a pool is closed, waiting for
// every connection it has lent out to be returned. A DatabaseQuerier handle
// fails from then on. Calling it again does nothing.
func (s *Server) Close() {
	s.closeOnce.Do(func() {
		if s.cancel != nil {
			s.cancel()
		}
		if s.pools != nil {
			s.closeDatabasePools()
		}
		if s.q != nil {
			releaseQuerier(s.q)
		}
	})
}

// Context is cancelled by Close. A caller's own background work that should
// end with the connection — a timer, a poll loop, a statement run on Pool()
// directly — derives its context from it. Never nil: Background for a nil
// Server or one built without Connect or NewServer, which nothing closes.
func (s *Server) Context() context.Context {
	if s == nil || s.ctx == nil {
		return context.Background()
	}
	return s.ctx
}

// bound is ctx, also cancelled when the Server closes, and the func that
// stops tying it to the Server; the caller defers it. It is for work that
// holds a connection past one statement (InTransaction): closing a pool
// waits for its acquired connections, so without it Close would wait for
// that work to end on its own.
func (s *Server) bound(ctx context.Context) (_ context.Context, release func()) {
	if s.ctx == nil {
		return ctx, func() {}
	}
	ctx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(s.ctx, cancel)
	return ctx, func() {
		stop()
		cancel()
	}
}

// Querier returns what the Server reads and writes through — Connect's pool,
// or what NewServer was given — for ad-hoc statements. A statement run on it
// is bounded only by the context it is given; derive that from Context for it
// to stop at Close. Closing it directly leaves the Server unusable: call
// Server.Close instead.
func (s *Server) Querier() Querier { return s.q }

// Pool is Querier as a *pgxpool.Pool, for what only a pool offers (Acquire a
// session of one's own, Stat, Config): the pool Connect opened, or the one
// NewServer was given. nil when NewServer was given anything else.
func (s *Server) Pool() *pgxpool.Pool {
	p, _ := s.q.(*pgxpool.Pool)
	return p
}

// CurrentDatabase is the database the Server's own Querier is bound to,
// read when the Server was built (current_database()). DatabaseQuerier
// reaches the others.
func (s *Server) CurrentDatabase() string { return s.database }

// Version is the server's server_version_num, read when the Server was
// built: 170002 for PostgreSQL 17.2. It is at least MinimumServerVersion.
func (s *Server) Version() ServerVersion { return s.version }
