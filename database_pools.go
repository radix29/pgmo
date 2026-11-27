package pgmo

import (
	"context"
	"fmt"
	"maps"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// ============================================================
// Per-database pools
// ============================================================

// PostgreSQL binds a session to the database it connected to: there is no
// USE. A Server reads its own database through the Querier it was built on,
// and every other database through a pool of its own, opened on first use,
// capped at MaxDatabasePools and closed least-recently-used. This replaces
// gosmo's USE [db] and perDatabaseExec.

// The limits a Server starts with; ServerOptions changes them.
const (
	// DefaultMaxDatabasePools is how many other databases' pools a Server
	// keeps open at once.
	DefaultMaxDatabasePools = 8

	// DefaultDatabasePoolMaxConns is the most connections each of those
	// pools opens, when pgmo derives it (Connect, or NewServer over a
	// pool): with the defaults, at most 32 sessions beyond the Server's own
	// pool, well inside max_connections' default of 100.
	DefaultDatabasePoolMaxConns = 4
)

// DatabaseOpener opens the Querier for database name on the Server's
// server, for a Server whose own Querier pgmo cannot derive one from
// (NewServer over a single connection, a transaction or a fake), or to
// replace the pool pgmo would derive. ctx is the first statement's.
//
// The Server owns what it returns and ends it as NewServer describes when it
// is evicted or the Server closed. It is evicted only while idle: a
// *pgxpool.Pool with no connection acquired, or any Querier with a method
// Idle() bool that reports true. Any other Querier is never evicted, so
// MaxDatabasePools does not bound it.
type DatabaseOpener func(ctx context.Context, name string) (Querier, error)

// ServerOption configures Connect and NewServer.
type ServerOption func(*serverOptions)

// serverOptions are the per-database limits, zero meaning the default.
type serverOptions struct {
	maxPools    int
	maxConns    int
	opener      DatabaseOpener
	invalidOpts []string // refusals found while applying, reported by the constructor
}

// WithMaxDatabasePools sets how many other databases' Queriers the Server
// keeps open at once (DefaultMaxDatabasePools); at least 1. The cap is
// soft: when every open one is in use, another database opens anyway, and
// the least recently used is closed once it is idle and another opens.
func WithMaxDatabasePools(n int) ServerOption {
	return func(o *serverOptions) {
		if n < 1 {
			o.invalidOpts = append(o.invalidOpts, fmt.Sprintf("WithMaxDatabasePools(%d) is below 1", n))
			return
		}
		o.maxPools = n
	}
}

// WithDatabasePoolMaxConns sets the most connections each other database's
// pool opens (DefaultDatabasePoolMaxConns); at least 1. It never raises a
// pool's own limit (pool_max_conns in ConnString, or the wrapped pool's
// MaxConns), and does not apply to a DatabaseOpener's Queriers.
func WithDatabasePoolMaxConns(n int) ServerOption {
	return func(o *serverOptions) {
		if n < 1 {
			o.invalidOpts = append(o.invalidOpts, fmt.Sprintf("WithDatabasePoolMaxConns(%d) is below 1", n))
			return
		}
		o.maxConns = n
	}
}

// WithDatabaseOpener sets how the Server opens another database's Querier,
// in place of the pool it would derive from its own; see DatabaseOpener.
func WithDatabaseOpener(open DatabaseOpener) ServerOption {
	return func(o *serverOptions) {
		if open == nil {
			o.invalidOpts = append(o.invalidOpts, "WithDatabaseOpener(nil)")
			return
		}
		o.opener = open
	}
}

// applyServerOptions applies opts over the defaults; its error is a
// refusal without prefix.
func applyServerOptions(opts []ServerOption) (serverOptions, error) {
	o := serverOptions{maxPools: DefaultMaxDatabasePools, maxConns: DefaultDatabasePoolMaxConns}
	for _, opt := range opts {
		if opt == nil {
			o.invalidOpts = append(o.invalidOpts, "a nil ServerOption")
			continue
		}
		opt(&o)
	}
	if len(o.invalidOpts) > 0 {
		return o, invalidf("%s", o.invalidOpts[0])
	}
	return o, nil
}

// databasePool is one other database's Querier in Server.pools.
type databasePool struct {
	q     Querier
	err   error         // the open's failure, for those waiting on ready
	ready chan struct{} // closed once q or err is set

	leases   int    // statements between lookup and their pool call returning
	lastUsed uint64 // Server.clock at the last lease, for LRU
}

// idle reports whether p can be closed without breaking a statement: no
// lease, and nothing holding one of its connections (rows not yet closed,
// a transaction, a QueryRow not yet scanned). Under Server.mu.
func (p *databasePool) idle() bool {
	if p.leases > 0 {
		return false
	}
	switch q := p.q.(type) {
	case *pgxpool.Pool:
		return q.Stat().AcquiredConns() == 0
	case interface{ Idle() bool }:
		return q.Idle()
	}
	return false
}

// DatabaseQuerier returns the Querier for database name on this server,
// without reading anything: the Server's own Querier when name is
// CurrentDatabase, else a handle on that database's pool, which its first
// statement opens. The handle stays valid however often the pool behind it
// is evicted and reopened, until the Server is closed; it is safe for
// concurrent use, as a pool is.
//
// Names match exactly, as PostgreSQL stores them. A database that does not
// exist fails each statement with the server's 3D000 (IsUndefinedObject).
// When the Server was built over something pgmo cannot open another
// database from — a single connection, a transaction — and no
// DatabaseOpener was given, each statement fails with ErrUnsupported.
func (s *Server) DatabaseQuerier(name string) Querier {
	if name != "" && name == s.database {
		return s.q
	}
	return databaseQuerier{s: s, name: name}
}

// lease returns name's Querier, opening it if needed, and the func that
// ends the lease once the statement's pool call has returned. Its error
// carries the "pgmo: database …" prefix.
func (s *Server) lease(ctx context.Context, name string) (Querier, func(), error) {
	if name == "" {
		return nil, nil, invalidf("pgmo: database querier: the database name is empty")
	}
	for {
		s.mu.Lock()
		if s.closed {
			s.mu.Unlock()
			return nil, nil, invalidf("pgmo: database %q: the Server is closed", name)
		}
		p, ok := s.pools[name]
		if !ok {
			return s.open(ctx, name) // unlocks
		}
		select {
		case <-p.ready:
		default:
			s.mu.Unlock()
			select {
			case <-p.ready:
			case <-ctx.Done():
				return nil, nil, fmt.Errorf("pgmo: database %q: open: %w", name, ctx.Err())
			}
			continue // look again: it may have failed, or been evicted since
		}
		if p.err != nil {
			s.mu.Unlock()
			return nil, nil, p.err
		}
		return p.q, s.take(name, p), nil // unlocks
	}
}

// take leases p, which is ready and open. Called under s.mu; unlocks it.
func (s *Server) take(name string, p *databasePool) func() {
	p.leases++
	s.clock++
	p.lastUsed = s.clock
	s.mu.Unlock()
	return func() {
		s.mu.Lock()
		p.leases--
		victims := s.evict(name)
		s.mu.Unlock()
		closeQueriers(victims)
	}
}

// open adds name's entry, opens its Querier outside s.mu and leases it.
// Called under s.mu; unlocks it.
func (s *Server) open(ctx context.Context, name string) (Querier, func(), error) {
	p := &databasePool{ready: make(chan struct{})}
	if s.pools == nil { // a Server not built by Connect or NewServer
		s.pools = map[string]*databasePool{}
	}
	s.pools[name] = p
	victims := s.evict(name)
	s.mu.Unlock()
	closeQueriers(victims)

	q, err := s.openQuerier(ctx, name)

	s.mu.Lock()
	switch {
	case err != nil:
		p.err = err
		delete(s.pools, name) // the next statement tries again
	case s.closed:
		p.err = invalidf("pgmo: database %q: the Server is closed", name)
		delete(s.pools, name)
		releaseQuerier(q)
	default:
		p.q = q
	}
	close(p.ready)
	if p.err != nil {
		s.mu.Unlock()
		return nil, nil, p.err
	}
	return q, s.take(name, p), nil
}

// evict removes, least recently used first, open idle entries other than
// keep while the map holds more than the cap, and returns their Queriers
// for the caller to close once s.mu is released. Under s.mu.
func (s *Server) evict(keep string) []Querier {
	var victims []Querier
	for len(s.pools) > s.opts.maxPools {
		var lru string
		var lruUsed uint64
		for name, p := range s.pools {
			if name == keep || p.q == nil || !p.idle() {
				continue
			}
			if lru == "" || p.lastUsed < lruUsed {
				lru, lruUsed = name, p.lastUsed
			}
		}
		if lru == "" {
			break // every other one is busy or opening: over the cap until one is idle
		}
		victims = append(victims, s.pools[lru].q)
		delete(s.pools, lru)
	}
	return victims
}

func closeQueriers(qs []Querier) {
	for _, q := range qs {
		releaseQuerier(q)
	}
}

// openQuerier opens database name's Querier: the DatabaseOpener's, else a
// pool on the Server's own configuration with the database replaced.
func (s *Server) openQuerier(ctx context.Context, name string) (Querier, error) {
	if s.opts.opener != nil {
		q, err := s.opts.opener(ctx, name)
		if err == nil && isNilQuerier(q) {
			err = invalidf("the DatabaseOpener returned a nil Querier")
		}
		if err != nil {
			return nil, fmt.Errorf("pgmo: database %q: open: %w", name, err)
		}
		return q, nil
	}
	cfg, err := s.databasePoolConfig(name)
	if err != nil {
		return nil, err
	}
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		return nil, fmt.Errorf("pgmo: database %q: open: %w", name, err)
	}
	return pool, nil
}

// databasePoolConfig is the configuration of database name's pool:
// serverConfig's, with MaxConns capped at the Server's limit and MinConns
// and MinIdleConns zero, since a pool opened for one read should not dial
// ahead of it.
func (s *Server) databasePoolConfig(name string) (*pgxpool.Config, error) {
	cfg, err := s.serverConfig(name, fmt.Sprintf("database %q", name))
	if err != nil {
		return nil, err
	}
	cfg.MaxConns = min(cfg.MaxConns, int32(min(s.opts.maxConns, 1<<30)))
	cfg.MinConns = 0
	cfg.MinIdleConns = 0
	return cfg, nil
}

// serverConfig is a fresh configuration reaching database name on this
// server. Over Connect's options it is those options resolved again with
// Database replaced, so a password file entry for that database applies as
// libpq would apply it; over a pool, the pool's configuration (hooks and
// all) with the database replaced. Over anything else it fails with
// ErrUnsupported, prefixed "pgmo: " + op; a connection-options refusal
// keeps its own prefix.
func (s *Server) serverConfig(name, op string) (*pgxpool.Config, error) {
	switch {
	case s.connectOpts != nil:
		o := *s.connectOpts
		o.Database = name
		// Valid when Connect took them; only the environment or a file
		// changing since can fail here, and the error says which.
		return o.poolConfig()
	case s.Pool() != nil:
		cfg := s.Pool().Config() // a copy
		cfg.ConnConfig.Database = name
		return cfg, nil
	}
	return nil, unsupportedf("pgmo: %s: a Server over a %T has no connection configuration to open from; build it with Connect, or NewServer over a *pgxpool.Pool (or, for DatabaseQuerier, give WithDatabaseOpener)", op, s.q)
}

// closeDatabasePools empties the map for Server.Close and closes every open
// Querier in it, waiting for each pool's lent connections to be returned.
// An entry still opening closes its own Querier when the open ends.
func (s *Server) closeDatabasePools() {
	s.mu.Lock()
	s.closed = true
	var qs []Querier
	for _, name := range slices.Sorted(maps.Keys(s.pools)) {
		if p := s.pools[name]; p.q != nil {
			qs = append(qs, p.q)
		}
	}
	s.pools = map[string]*databasePool{}
	s.mu.Unlock()
	closeQueriers(qs)
}

// databaseQuerier is DatabaseQuerier's handle: each statement leases the
// database's current Querier for as long as its pool call takes, so the
// pool cannot be evicted between lookup and use. What the call returns
// holding a connection (Rows, a Row, a Tx) keeps the pool busy through its
// acquired connection instead.
type databaseQuerier struct {
	s    *Server
	name string
}

func (d databaseQuerier) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	q, done, err := d.s.lease(ctx, d.name)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	defer done()
	return q.Exec(ctx, sql, args...)
}

func (d databaseQuerier) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	q, done, err := d.s.lease(ctx, d.name)
	if err != nil {
		return nil, err
	}
	defer done()
	return q.Query(ctx, sql, args...)
}

func (d databaseQuerier) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	q, done, err := d.s.lease(ctx, d.name)
	if err != nil {
		return errRow{err}
	}
	defer done()
	return q.QueryRow(ctx, sql, args...)
}

func (d databaseQuerier) CopyFrom(ctx context.Context, table pgx.Identifier, columns []string, rows pgx.CopyFromSource) (int64, error) {
	q, done, err := d.s.lease(ctx, d.name)
	if err != nil {
		return 0, err
	}
	defer done()
	return q.CopyFrom(ctx, table, columns, rows)
}

func (d databaseQuerier) Begin(ctx context.Context) (pgx.Tx, error) {
	q, done, err := d.s.lease(ctx, d.name)
	if err != nil {
		return nil, err
	}
	defer done()
	return q.Begin(ctx)
}

// errRow is a pgx.Row whose Scan fails with err, as pgx's own does when the
// query could not be sent.
type errRow struct{ err error }

func (r errRow) Scan(...any) error { return r.err }

// releaseDatabase closes the Server's own Querier on database name when it is
// idle, so a statement that needs the database free of sessions — DROP
// DATABASE, ALTER DATABASE … RENAME TO and SET TABLESPACE — does not find one
// of pgmo's idle pooled connections in the way. The next statement on name
// opens afresh. One in use (a lease, an open Rows, a transaction) is left
// as it is: it is a session the caller holds, and the server reports it as
// any other. A database whose pool is still opening is left too. Under
// WithScript nothing is closed, since nothing runs.
//
// The server waits up to five seconds for a backend to exit before it
// counts it as connected, so the closed pool's sessions ending a moment
// after Close returns does not fail the statement.
func (s *Server) releaseDatabase(ctx context.Context, name string) {
	if Scripting(ctx) {
		return
	}
	s.mu.Lock()
	p, ok := s.pools[name]
	if !ok || p.q == nil || !p.idle() {
		s.mu.Unlock()
		return
	}
	delete(s.pools, name)
	s.mu.Unlock()
	releaseQuerier(p.q)
}
