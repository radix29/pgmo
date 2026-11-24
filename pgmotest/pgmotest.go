// Package pgmotest runs real pgmo code over canned rows, so a caller's tests
// exercise pgmo's own queries, scanning, Ref semantics and errors without a
// server.
//
// A Fake is a pgmo.Querier that answers each statement from the first rule
// whose text it contains. Wrap it with Server and call pgmo as usual:
//
//	f := pgmotest.New(t)
//	f.On("FROM pg_catalog.pg_namespace", pgmotest.Rows(
//		[]any{int64(2200), "public", "pg_database_owner"},
//	))
//	srv := f.Server()
//
// The rows are positional: each is one row of the result exactly as the
// server would send it, in the query's SELECT order. That is the point — a
// pgmo change in a query's shape or its scanning fails the caller's test,
// where a hand-written fake of the pgmo method would not notice. Read the
// query's text in pgmo's source for the column order (a test that fails
// prints the query it could not answer, and Queries lists every one).
//
// A row's values go through pgx's own type map, as a server's would: each
// is encoded under its column's type (Result.Types, or the type pgx maps the
// first non-nil value of the column to: int64 → int8, string → text) and
// scanned back with pgx's scan plans. So a NULL scanned into a *string, or
// an int8 into an *int16 that cannot hold it, fails here as it fails on a
// connection.
//
// A query no rule answers fails the test (t.Errorf) and returns an error to
// pgmo, so an unexpected read is reported as one, naming the query. An Exec
// no rule answers succeeds with an empty command tag: writes are recorded,
// not refused. The server version pgmo reads on connect has a default rule,
// answered from Fake.Version, which a test may change before its first call
// to Server.
//
// What pgx sends itself is recorded as it sends it: Begin, Commit and
// Rollback arrive as Exec of "begin", "commit" and "rollback", a nested
// Begin as "savepoint sp_1" (then "release savepoint sp_1" or "rollback to
// savepoint sp_1"), a CopyFrom as pgx's "copy … from stdin binary;" with the
// rows it read in Query.Copy — so a rule can fail any of them. A commit whose
// rule answers the tag "ROLLBACK" is pgx.ErrTxCommitRollback, as from a
// server. Every statement records the context it was issued under (Ctx) and
// the transaction it ran in (Tx).
//
// A statement whose context has already ended is not sent, and fails with
// the context's error, as with pgx. Hold keeps a rule's answer back until a
// channel is closed or the statement's context ends — answered then with
// the server's 57014, as on a connection pgmo opened — so a test can cancel
// a statement in flight.
//
// Rows a test's code never closed, and transactions it never ended, fail the
// test when it ends: on a pool each holds a connection.
//
// A Fake is one database, CurrentDatabase. Every other database the Server
// reads (pgmo.Server.DatabaseQuerier) is answered by a Fake of its own,
// returned by Database, with its own rules and Queries. When the Server
// evicts that database's Querier the Fake is closed; when it opens it again
// the same Fake is reopened, rules and Queries kept.
//
// A Fake is safe for concurrent use.
package pgmotest

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"sync"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/radix29/pgmo"
)

// DefaultVersion is the server version a new Fake reports.
const DefaultVersion = pgmo.PostgreSQL18

// DefaultDatabase is the database a new Fake reports it is connected to.
const DefaultDatabase = "postgres"

// Kind is how a statement was sent.
type Kind int

const (
	KindQuery Kind = iota // Query or QueryRow
	KindExec              // Exec, and pgx's own transaction statements
	KindCopy              // CopyFrom
)

func (k Kind) String() string {
	switch k {
	case KindQuery:
		return "query"
	case KindExec:
		return "exec"
	case KindCopy:
		return "copy"
	}
	return "Kind(" + strconv.Itoa(int(k)) + ")"
}

// Query is one statement pgmo sent, as a rule sees it and Queries records it.
type Query struct {
	Kind Kind
	// Text is the statement as pgx would send it: after a QueryRewriter
	// (pgx.NamedArgs) has rewritten it; for a COPY, pgx's own statement.
	Text string
	// Args are its parameters as passed, $1 first, without pgx's leading
	// options (QueryExecMode, QueryResultFormats, the QueryRewriter).
	Args []any
	// Copy is a COPY's rows, as read from its pgx.CopyFromSource.
	Copy [][]any
	// Tx numbers the transaction the statement ran in, from 1 in the order
	// they began; 0 outside one. A savepoint is part of its transaction.
	Tx int
	// Ctx is the context the statement was issued under: a test asserting
	// that a caller cancelled it, or a Responder holding it until it is
	// (Hold), looks here.
	Ctx context.Context
}

// Arg is the i-th argument ($i+1), or nil if there are fewer.
func (q Query) Arg(i int) any {
	if i < 0 || i >= len(q.Args) {
		return nil
	}
	return q.Args[i]
}

// Result is a rule's answer.
type Result struct {
	// Columns names the result's columns; a missing name is "?column?", as
	// the server names an unnamed expression.
	Columns []string
	// Types are the columns' type OIDs (pgtype.Int8OID, …); 0 or missing
	// infers the column's type from its first non-nil value, text if none.
	Types []uint32
	Rows  [][]any
	// Tag is the command tag ("INSERT 0 1"); a query's defaults to
	// "SELECT <rows>".
	Tag string
	// Err fails the statement. With no Rows the query fails at once;
	// after Rows, the rows are delivered first and Err ends them, reaching
	// the caller through rows.Err as a failure mid-result does.
	Err error
}

// Responder answers a statement a rule matched.
type Responder func(Query) Result

// Rows answers with rows whose types are inferred.
func Rows(rows ...[]any) Responder {
	return func(Query) Result { return Result{Rows: rows} }
}

// Fail answers with err, as the server's error: pass a *pgconn.PgError to
// exercise pgmo's SQLSTATE classification.
func Fail(err error) Responder {
	return func(Query) Result { return Result{Err: err} }
}

// Tag answers a write with its command tag ("UPDATE 3").
func Tag(tag string) Responder {
	return func(Query) Result { return Result{Tag: tag} }
}

// Hold wraps r so that it answers only once release is closed, or, when the
// statement's context ends first, fails as a statement cancelled on a pgmo
// connection fails: the server's 57014 query_canceled (Canceled), which
// pgmo's own methods report with the context's error joined. It is how a
// test acts, or cancels, while a statement is genuinely in flight: without
// it a Fake answers at once and every statement has finished before the
// test regains control. Scope the rule (OnExec, a narrow match) so only the
// statement under test is held.
func Hold(release <-chan struct{}, r Responder) Responder {
	return func(q Query) Result {
		var done <-chan struct{}
		if q.Ctx != nil {
			done = q.Ctx.Done()
		}
		select {
		case <-release:
			return r(q)
		case <-done:
			return Result{Err: Canceled()}
		}
	}
}

// Canceled is the server's error for a statement a cancel request stopped:
// SQLSTATE 57014, as pgmo.IsQueryCanceled reads it.
func Canceled() *pgconn.PgError {
	return &pgconn.PgError{
		Severity: "ERROR", SeverityUnlocalized: "ERROR", Code: "57014",
		Message: "canceling statement due to user request",
	}
}

type rule struct {
	match    string
	writes   bool // OnExec: Exec and CopyFrom only
	respond  Responder
	fallback bool // the Fake's own defaults, tried after every test rule
}

// Fake is a scripted pgmo.Querier. Build one with New.
type Fake struct {
	// Version is the server_version_num pgmo reads on connect.
	Version pgmo.ServerVersion

	// CurrentDatabase is the current_database() pgmo reads on connect.
	CurrentDatabase string

	t testing.TB

	// root is the Fake Server was built on; others is its Fake per other
	// database, under root's mu.
	root   *Fake
	others map[string]*Fake
	opens  int // how often the Server opened this database

	mu       sync.Mutex
	rules    []rule
	queries  []Query
	txs      int
	closed   bool
	openRows map[*rows]string // not yet closed → their query
	openTxs  map[*tx]struct{} // not yet ended

	serverMu sync.Mutex
	server   *pgmo.Server
}

var _ pgmo.Querier = (*Fake)(nil)

// New returns a Fake reporting DefaultVersion and DefaultDatabase. When t
// ends it reports rows and transactions left open.
func New(t testing.TB) *Fake {
	t.Helper()
	f := newFake(t, DefaultDatabase)
	f.root = f
	f.others = map[string]*Fake{}
	return f
}

func newFake(t testing.TB, database string) *Fake {
	f := &Fake{
		Version:         DefaultVersion,
		CurrentDatabase: database,
		t:               t,
		openRows:        map[*rows]string{},
		openTxs:         map[*tx]struct{}{},
	}
	f.rules = append(f.rules, rule{
		match:    "pg_catalog.current_setting('server_version_num')",
		fallback: true,
		respond: func(Query) Result {
			f.mu.Lock()
			v, db := f.Version, f.CurrentDatabase
			f.mu.Unlock()
			return Result{
				Columns: []string{"current_setting", "current_database"},
				Types:   []uint32{pgtype.Int4OID, pgtype.NameOID},
				Rows:    [][]any{{int32(v), db}},
			}
		},
	})
	t.Cleanup(f.checkLeaks)
	return f
}

// Database is the Fake answering database name: f's own when name is the
// one New's Fake reports, else one made on first use, reporting the same
// Version. Set its rules before the Server's first statement there.
func (f *Fake) Database(name string) *Fake {
	r := f.root
	r.mu.Lock()
	defer r.mu.Unlock()
	if name == r.CurrentDatabase {
		return r
	}
	o, ok := r.others[name]
	if !ok {
		o = newFake(r.t, name)
		o.Version = r.Version
		o.root = r
		o.closed = true // until the Server opens it
		r.others[name] = o
	}
	return o
}

// Opens is how many times the Server opened f's database: 0 for the Fake
// it was built on, which it never opens, and for a database never read.
func (f *Fake) Opens() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.opens
}

// Idle reports whether f holds no open rows or transaction, which is when a
// Server may evict it (see pgmo.DatabaseOpener).
func (f *Fake) Idle() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return len(f.openRows) == 0 && len(f.openTxs) == 0
}

// open is the Server's pgmo.DatabaseOpener over f.
func (f *Fake) open(_ context.Context, name string) (pgmo.Querier, error) {
	o := f.Database(name)
	o.mu.Lock()
	o.closed = false
	o.opens++
	o.mu.Unlock()
	return o, nil
}

// On answers every statement whose text contains match. Rules are tried in
// the order they were added, before the Fake's defaults.
func (f *Fake) On(match string, r Responder) { f.add(rule{match: match, respond: r}) }

// OnExec is On for writes only: Exec (pgx's transaction statements among
// them) and CopyFrom.
func (f *Fake) OnExec(match string, r Responder) { f.add(rule{match: match, writes: true, respond: r}) }

func (f *Fake) add(r rule) {
	f.mu.Lock()
	defer f.mu.Unlock()
	// Before the defaults, after the earlier test rules.
	i := len(f.rules)
	for i > 0 && f.rules[i-1].fallback {
		i--
	}
	f.rules = append(f.rules[:i], append([]rule{r}, f.rules[i:]...)...)
}

// Queries is every statement sent so far, in order.
func (f *Fake) Queries() []Query {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Query(nil), f.queries...)
}

// Server is the pgmo.Server over f, built on the first call (with opts)
// and closed when the test ends; it reaches other databases through
// Database's Fakes. A failure to build it (a Version below pgmo's floor, a
// version rule that fails, an option pgmo refuses) fails the test.
//
// On a Fake from Database it is the Server over the Fake New returned.
func (f *Fake) Server(opts ...pgmo.ServerOption) *pgmo.Server {
	f.t.Helper()
	f = f.root
	f.serverMu.Lock()
	defer f.serverMu.Unlock()
	if f.server == nil {
		opts = append([]pgmo.ServerOption{pgmo.WithDatabaseOpener(f.open)}, opts...)
		s, err := pgmo.NewServer(context.Background(), f, opts...)
		if err != nil {
			f.t.Fatalf("pgmotest: %v", err)
		}
		f.t.Cleanup(s.Close)
		f.server = s
	}
	return f.server
}

// Close marks f closed, as a closed pool: every later statement fails.
// Server.Close calls it. Calling it again does nothing.
func (f *Fake) Close() {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.closed = true
}

// Closed reports whether Close was called.
func (f *Fake) Closed() bool {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.closed
}

// errClosed is what a statement on a closed Fake fails with: pgxpool's text.
var errClosed = errors.New("closed pool")

func (f *Fake) checkLeaks() {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, q := range f.openRows {
		f.t.Errorf("pgmotest: rows of %q were never closed", q)
	}
	if n := len(f.openTxs); n > 0 {
		f.t.Errorf("pgmotest: %d transaction(s) never committed or rolled back", n)
	}
}

// answer records q and finds its answer; ok is false when no rule matched.
// A statement whose context has already ended is not sent, as pgx sends
// none: it fails with the context's error and is not recorded.
func (f *Fake) answer(q Query) (Result, bool, error) {
	if q.Ctx != nil && q.Ctx.Err() != nil {
		return Result{}, false, q.Ctx.Err()
	}
	f.mu.Lock()
	if f.closed {
		f.mu.Unlock()
		return Result{}, false, errClosed
	}
	f.queries = append(f.queries, q)
	var respond Responder
	for _, r := range f.rules {
		if r.writes && q.Kind == KindQuery {
			continue
		}
		if strings.Contains(q.Text, r.match) {
			respond = r.respond
			break
		}
	}
	f.mu.Unlock()
	if respond == nil {
		return Result{}, false, nil
	}
	return respond(q), true, nil
}

// ------------------------------------------------------------
// Querier
// ------------------------------------------------------------

func (f *Fake) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	return f.exec(ctx, 0, sql, args)
}

func (f *Fake) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	return f.query(ctx, 0, sql, args)
}

func (f *Fake) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	r, _ := f.query(ctx, 0, sql, args)
	return row{r}
}

func (f *Fake) CopyFrom(ctx context.Context, table pgx.Identifier, columns []string, src pgx.CopyFromSource) (int64, error) {
	return f.copyFrom(ctx, 0, table, columns, src)
}

// Begin starts a transaction: an Exec of "begin".
func (f *Fake) Begin(ctx context.Context) (pgx.Tx, error) {
	f.mu.Lock()
	f.txs++
	id := f.txs
	f.mu.Unlock()
	if _, err := f.exec(ctx, id, "begin", nil); err != nil {
		return nil, err
	}
	t := &tx{f: f, id: id}
	t.root = t
	f.mu.Lock()
	f.openTxs[t] = struct{}{}
	f.mu.Unlock()
	return t, nil
}

// leadingOptions strips the options pgx takes as leading arguments, as pgx
// does, rewriting through a QueryRewriter; query is whether the statement is
// a Query (which also takes result formats) rather than an Exec.
func leadingOptions(ctx context.Context, sql string, args []any, query bool) (string, []any, error) {
	var rewriter pgx.QueryRewriter
loop:
	for len(args) > 0 {
		switch a := args[0].(type) {
		case pgx.QueryResultFormats, pgx.QueryResultFormatsByOID:
			if !query {
				break loop
			}
		case pgx.QueryExecMode:
		case pgx.QueryRewriter:
			rewriter = a
		default:
			break loop
		}
		args = args[1:]
	}
	if rewriter != nil {
		var err error
		if sql, args, err = rewriter.RewriteQuery(ctx, nil, sql, args); err != nil {
			return "", nil, fmt.Errorf("rewrite query failed: %w", err)
		}
	}
	return sql, args, nil
}

func (f *Fake) exec(ctx context.Context, txID int, sql string, args []any) (pgconn.CommandTag, error) {
	sql, args, err := leadingOptions(ctx, sql, args, false)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	res, _, err := f.answer(Query{Kind: KindExec, Text: sql, Args: args, Tx: txID, Ctx: ctx})
	if err == nil {
		err = res.Err
	}
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	return pgconn.NewCommandTag(res.Tag), nil
}

func (f *Fake) query(ctx context.Context, txID int, sql string, args []any) (pgx.Rows, error) {
	sql, args, err := leadingOptions(ctx, sql, args, true)
	if err != nil {
		return &rows{err: err, closed: true}, err
	}
	res, ok, err := f.answer(Query{Kind: KindQuery, Text: sql, Args: args, Tx: txID, Ctx: ctx})
	if err == nil && !ok {
		f.t.Errorf("pgmotest: no rule answers query %q", sql)
		err = fmt.Errorf("pgmotest: no rule answers query %q", sql)
	}
	if err == nil && res.Err != nil && len(res.Rows) == 0 {
		err = res.Err
	}
	if err != nil {
		return &rows{err: err, closed: true}, err
	}
	r, err := newRows(res)
	if err != nil {
		f.t.Errorf("pgmotest: answer to %q: %v", sql, err)
		return &rows{err: err, closed: true}, err
	}
	r.f = f
	f.mu.Lock()
	f.openRows[r] = sql
	f.mu.Unlock()
	return r, nil
}

func (f *Fake) copyFrom(ctx context.Context, txID int, table pgx.Identifier, columns []string, src pgx.CopyFromSource) (int64, error) {
	var data [][]any
	for src.Next() {
		vals, err := src.Values()
		if err != nil {
			return 0, err
		}
		if len(vals) != len(columns) {
			return 0, fmt.Errorf("expected %d values, got %d values", len(columns), len(vals))
		}
		data = append(data, vals)
	}
	if err := src.Err(); err != nil {
		return 0, err
	}
	quoted := make([]string, len(columns))
	for i, c := range columns {
		quoted[i] = pgx.Identifier{c}.Sanitize()
	}
	text := fmt.Sprintf("copy %s ( %s ) from stdin binary;", table.Sanitize(), strings.Join(quoted, ", "))
	res, _, err := f.answer(Query{Kind: KindCopy, Text: text, Copy: data, Tx: txID, Ctx: ctx})
	if err == nil {
		err = res.Err
	}
	if err != nil {
		return 0, err
	}
	return int64(len(data)), nil
}

// ------------------------------------------------------------
// Rows
// ------------------------------------------------------------

// rows is pgx.Rows over a Result encoded on the wire, scanned as pgx's own
// rows scan: through the type map's scan plans.
type rows struct {
	f      *Fake
	m      *pgtype.Map
	fields []pgconn.FieldDescription
	data   [][][]byte
	i      int // the current row; -1 before the first Next
	late   error
	tag    pgconn.CommandTag
	err    error
	closed bool
}

// newRows encodes res's rows under their column types. Each rows has its own
// type map, as each pgx connection has: a Map is not safe for concurrent use.
func newRows(res Result) (*rows, error) {
	m := pgtype.NewMap()
	width := len(res.Columns)
	for _, r := range res.Rows {
		width = max(width, len(r))
	}
	for i, r := range res.Rows {
		if len(r) != width {
			return nil, fmt.Errorf("row %d has %d values, want %d", i, len(r), width)
		}
	}
	fields := make([]pgconn.FieldDescription, width)
	for c := range fields {
		name := "?column?"
		if c < len(res.Columns) && res.Columns[c] != "" {
			name = res.Columns[c]
		}
		var oid uint32
		if c < len(res.Types) {
			oid = res.Types[c]
		}
		if oid == 0 {
			oid = pgtype.TextOID
			for _, r := range res.Rows {
				if r[c] == nil {
					continue
				}
				t, ok := m.TypeForValue(r[c])
				if !ok {
					return nil, fmt.Errorf("column %d: pgx maps no type to %T; set Result.Types", c, r[c])
				}
				oid = t.OID
				break
			}
		}
		fields[c] = pgconn.FieldDescription{Name: name, DataTypeOID: oid, DataTypeSize: -1, TypeModifier: -1, Format: m.FormatCodeForOID(oid)}
	}
	data := make([][][]byte, len(res.Rows))
	for i, r := range res.Rows {
		data[i] = make([][]byte, width)
		for c, v := range r {
			// An empty, non-nil buf: Encode returns nil only for NULL, and an
			// empty value (a "" text) appends nothing, so a nil buf would
			// come back nil and be read as NULL.
			buf, err := m.Encode(fields[c].DataTypeOID, fields[c].Format, v, []byte{})
			if err != nil {
				return nil, fmt.Errorf("row %d column %d: %w", i, c, err)
			}
			data[i][c] = buf
		}
	}
	tag := res.Tag
	if tag == "" {
		tag = "SELECT " + strconv.Itoa(len(res.Rows))
	}
	return &rows{m: m, fields: fields, data: data, i: -1, late: res.Err, tag: pgconn.NewCommandTag(tag)}, nil
}

func (r *rows) Close() {
	if r.closed {
		return
	}
	r.closed = true
	if r.f != nil {
		r.f.mu.Lock()
		delete(r.f.openRows, r)
		r.f.mu.Unlock()
	}
}

func (r *rows) Err() error                                   { return r.err }
func (r *rows) CommandTag() pgconn.CommandTag                { return r.tag }
func (r *rows) FieldDescriptions() []pgconn.FieldDescription { return r.fields }
func (r *rows) Conn() *pgx.Conn                              { return nil }
func (r *rows) TypeMap() *pgtype.Map                         { return r.m }

func (r *rows) fatal(err error) {
	if r.err == nil {
		r.err = err
	}
	r.Close()
}

func (r *rows) Next() bool {
	if r.closed {
		return false
	}
	r.i++
	if r.i >= len(r.data) {
		if r.late != nil {
			r.fatal(r.late)
		}
		r.Close()
		return false
	}
	return true
}

func (r *rows) RawValues() [][]byte {
	if r.i < 0 || r.i >= len(r.data) {
		return nil
	}
	return r.data[r.i]
}

func (r *rows) Scan(dest ...any) error {
	values := r.RawValues()
	if len(dest) == 1 {
		if rs, ok := dest[0].(pgx.RowScanner); ok {
			err := rs.ScanRow(r)
			if err != nil {
				r.fatal(err)
			}
			return err
		}
	}
	if len(r.fields) != len(dest) {
		err := fmt.Errorf("number of field descriptions must equal number of destinations, got %d and %d", len(r.fields), len(dest))
		r.fatal(err)
		return err
	}
	for i, d := range dest {
		if d == nil {
			continue
		}
		fd := r.fields[i]
		if err := r.m.PlanScan(fd.DataTypeOID, fd.Format, d).Scan(values[i], d); err != nil {
			err = pgx.ScanArgError{ColumnIndex: i, FieldName: fd.Name, Err: err}
			r.fatal(err)
			return err
		}
	}
	return nil
}

func (r *rows) Values() ([]any, error) {
	if r.closed {
		return nil, errors.New("rows is closed")
	}
	values := r.RawValues()
	out := make([]any, 0, len(r.fields))
	for i, fd := range r.fields {
		if values[i] == nil {
			out = append(out, nil)
			continue
		}
		t, _ := r.m.TypeForOID(fd.DataTypeOID) // newRows encoded it, so the map knows it
		v, err := t.Codec.DecodeValue(r.m, fd.DataTypeOID, fd.Format, values[i])
		if err != nil {
			r.fatal(err)
			return nil, err
		}
		out = append(out, v)
	}
	return out, nil
}

// row is pgx.Row over rows, as pgx's QueryRow: no row is pgx.ErrNoRows, and
// the rows are closed after the first.
type row struct{ r pgx.Rows }

func (w row) Scan(dest ...any) error {
	r := w.r
	if r.Err() != nil {
		return r.Err()
	}
	for _, d := range dest {
		if _, ok := d.(*pgtype.DriverBytes); ok {
			r.Close()
			return errors.New("cannot scan into *pgtype.DriverBytes from QueryRow")
		}
	}
	if !r.Next() {
		if r.Err() == nil {
			return pgx.ErrNoRows
		}
		return r.Err()
	}
	_ = r.Scan(dest...) // its error is also r.Err, returned below
	r.Close()
	return r.Err()
}

// ------------------------------------------------------------
// Transactions
// ------------------------------------------------------------

// tx is pgx.Tx: the transaction, or one of its savepoints (savepoint > 0),
// sending what pgx's own sends.
type tx struct {
	f    *Fake
	id   int
	root *tx

	mu         sync.Mutex
	closed     bool
	savepoint  int // this savepoint's number; 0 for the transaction itself
	savepoints int // root only: the last savepoint number given out
}

var _ pgx.Tx = (*tx)(nil)

func (t *tx) isClosed() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.closed
}

// live is ErrTxClosed once this savepoint or its transaction has ended.
func (t *tx) live() error {
	if t.isClosed() || t.root.isClosed() {
		return pgx.ErrTxClosed
	}
	return nil
}

func (t *tx) Begin(ctx context.Context) (pgx.Tx, error) {
	if err := t.live(); err != nil {
		return nil, err
	}
	t.root.mu.Lock()
	t.root.savepoints++
	n := t.root.savepoints
	t.root.mu.Unlock()
	if _, err := t.f.exec(ctx, t.id, "savepoint sp_"+strconv.Itoa(n), nil); err != nil {
		return nil, err
	}
	return &tx{f: t.f, id: t.id, root: t.root, savepoint: n}, nil
}

// end sends the statement that ends t and marks it ended, whatever the
// outcome, as pgx does.
func (t *tx) end(ctx context.Context, sql string) (pgconn.CommandTag, error) {
	t.mu.Lock()
	if t.closed {
		t.mu.Unlock()
		return pgconn.CommandTag{}, pgx.ErrTxClosed
	}
	t.closed = true
	t.mu.Unlock()
	if t.savepoint == 0 {
		t.f.mu.Lock()
		delete(t.f.openTxs, t)
		t.f.mu.Unlock()
	} else if t.root.isClosed() {
		return pgconn.CommandTag{}, pgx.ErrTxClosed
	}
	return t.f.exec(ctx, t.id, sql, nil)
}

func (t *tx) Commit(ctx context.Context) error {
	if t.savepoint > 0 {
		_, err := t.end(ctx, "release savepoint sp_"+strconv.Itoa(t.savepoint))
		return err
	}
	tag, err := t.end(ctx, "commit")
	if err != nil {
		return err
	}
	if tag.String() == "ROLLBACK" {
		return pgx.ErrTxCommitRollback
	}
	return nil
}

func (t *tx) Rollback(ctx context.Context) error {
	sql := "rollback"
	if t.savepoint > 0 {
		sql = "rollback to savepoint sp_" + strconv.Itoa(t.savepoint)
	}
	_, err := t.end(ctx, sql)
	return err
}

func (t *tx) Exec(ctx context.Context, sql string, args ...any) (pgconn.CommandTag, error) {
	if err := t.live(); err != nil {
		return pgconn.CommandTag{}, err
	}
	return t.f.exec(ctx, t.id, sql, args)
}

func (t *tx) Query(ctx context.Context, sql string, args ...any) (pgx.Rows, error) {
	if err := t.live(); err != nil {
		return &rows{err: err, closed: true}, err
	}
	return t.f.query(ctx, t.id, sql, args)
}

func (t *tx) QueryRow(ctx context.Context, sql string, args ...any) pgx.Row {
	r, _ := t.Query(ctx, sql, args...)
	return row{r}
}

func (t *tx) CopyFrom(ctx context.Context, table pgx.Identifier, columns []string, src pgx.CopyFromSource) (int64, error) {
	if err := t.live(); err != nil {
		return 0, err
	}
	return t.f.copyFrom(ctx, t.id, table, columns, src)
}

// unsupported is the answer of the pgx.Tx methods pgmo does not use;
// each also fails the test, since a fake answer would mislead it.
func (t *tx) unsupported(what string) error {
	t.f.t.Errorf("pgmotest: %s is not supported", what)
	return fmt.Errorf("pgmotest: %s is not supported", what)
}

func (t *tx) SendBatch(_ context.Context, b *pgx.Batch) pgx.BatchResults {
	return pgx.FailedBatchResults(b, t.unsupported("SendBatch"))
}

func (t *tx) Prepare(context.Context, string, string) (*pgconn.StatementDescription, error) {
	return nil, t.unsupported("Prepare")
}

// LargeObjects is not supported: the zero value it returns has no
// connection, and using it panics.
func (t *tx) LargeObjects() pgx.LargeObjects { return pgx.LargeObjects{} }

func (t *tx) Conn() *pgx.Conn { return nil }
