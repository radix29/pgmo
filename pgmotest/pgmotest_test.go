package pgmotest_test

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/radix29/pgmo"
	"github.com/radix29/pgmo/pgmotest"
)

// recorder is a testing.TB whose Errorf is kept for the test to read rather
// than failing it.
type recorder struct {
	testing.TB
	mu   sync.Mutex
	errs []string
}

func (r *recorder) Errorf(format string, args ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.errs = append(r.errs, fmt.Sprintf(format, args...))
}

func (r *recorder) errors() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.errs...)
}

// TestServerReadsTheVersion: the Fake's default rule answers pgmo's own
// version read, so the Server reports Fake.Version; Server is built once and
// its Close closes the Fake.
func TestServerReadsTheVersion(t *testing.T) {
	f := pgmotest.New(t)
	f.Version = 160004
	s := f.Server()
	if s.Version() != 160004 {
		t.Errorf("Version() = %d, want 160004", s.Version())
	}
	if f.Server() != s {
		t.Error("Server built a second Server")
	}
	q := f.Queries()
	if len(q) != 1 || q[0].Kind != pgmotest.KindQuery || !strings.Contains(q[0].Text, "pg_catalog.current_setting('server_version_num')") {
		t.Errorf("queries = %+v, want pgmo's one version read", q)
	}
	if s.Querier() != pgmo.Querier(f) || s.Pool() != nil {
		t.Error("the Server is not over the Fake")
	}
	s.Close()
	if !f.Closed() {
		t.Error("Server.Close left the Fake open")
	}
	if _, err := f.Exec(context.Background(), "select 1"); err == nil || err.Error() != "closed pool" {
		t.Errorf("statement after Close: err = %v, want closed pool", err)
	}
}

func TestDefaultVersionIsSupported(t *testing.T) {
	if pgmotest.DefaultVersion < pgmo.MinimumServerVersion {
		t.Errorf("DefaultVersion %d is below pgmo's floor", pgmotest.DefaultVersion)
	}
}

// TestOldVersionIsRefused: the Fake reaches pgmo's version check like a
// server does.
func TestOldVersionIsRefused(t *testing.T) {
	f := pgmotest.New(t)
	f.Version = 140005
	if _, err := pgmo.NewServer(context.Background(), f); !errors.Is(err, pgmo.ErrUnsupportedVersion) {
		t.Errorf("err = %v, want ErrUnsupportedVersion", err)
	}
}

// TestRowsScanAsPgxDoes: values are encoded under their column type and
// scanned with pgx's plans, so what pgx refuses is refused here.
func TestRowsScanAsPgxDoes(t *testing.T) {
	f := pgmotest.New(t)
	when := time.Date(2026, 11, 21, 9, 30, 0, 0, time.UTC)
	f.On("FROM t", func(pgmotest.Query) pgmotest.Result {
		return pgmotest.Result{
			Columns: []string{"id", "name", "note", "at"},
			Rows: [][]any{
				{int64(70000), "a", nil, when},
				{int64(2), "b", "x", when},
			},
		}
	})
	ctx := context.Background()
	rows, err := f.Query(ctx, "SELECT id, name, note, at FROM t")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	fds := rows.FieldDescriptions()
	wantOIDs := []uint32{pgtype.Int8OID, pgtype.TextOID, pgtype.TextOID, pgtype.TimestamptzOID}
	for i, fd := range fds {
		if fd.DataTypeOID != wantOIDs[i] {
			t.Errorf("column %d (%s): OID %d, want %d", i, fd.Name, fd.DataTypeOID, wantOIDs[i])
		}
	}
	if fds[0].Name != "id" {
		t.Errorf("column 0 is %q, want id", fds[0].Name)
	}
	if !rows.Next() {
		t.Fatal("no first row")
	}
	var (
		id   int64
		name string
		note pgtype.Text
		at   time.Time
	)
	if err := rows.Scan(&id, &name, &note, &at); err != nil {
		t.Fatal(err)
	}
	if id != 70000 || name != "a" || note.Valid || !at.Equal(when) {
		t.Errorf("row 1 = %d %q %+v %v", id, name, note, at)
	}
	vals, err := rows.Values()
	if err != nil || len(vals) != 4 || vals[0] != int64(70000) || vals[2] != nil {
		t.Errorf("Values() = %v, %v", vals, err)
	}
	// 70000 does not fit an int16: pgx's error, and the rows are done.
	var small int16
	err = rows.Scan(&small, nil, nil, nil)
	var sae pgx.ScanArgError
	if !errors.As(err, &sae) || sae.ColumnIndex != 0 {
		t.Errorf("int8 70000 into *int16: err = %v, want a ScanArgError on column 0", err)
	}
	if rows.Next() || rows.Err() == nil {
		t.Error("rows went on after a failed Scan")
	}
}

// TestNullIntoAStringFails is the classic scan bug a fake must not hide.
func TestNullIntoAStringFails(t *testing.T) {
	f := pgmotest.New(t)
	f.On("FROM t", pgmotest.Rows([]any{nil}))
	var s string
	if err := f.QueryRow(context.Background(), "SELECT note FROM t").Scan(&s); err == nil {
		t.Error("NULL scanned into a *string")
	}
}

// TestEmptyIsNotNull: an empty value — "" text, an empty bytea — is a value,
// as on a connection, not NULL; nil and a pgtype.Text that is not Valid are
// NULL.
func TestEmptyIsNotNull(t *testing.T) {
	f := pgmotest.New(t)
	f.On("FROM t", func(pgmotest.Query) pgmotest.Result {
		return pgmotest.Result{
			Types: []uint32{pgtype.TextOID, pgtype.ByteaOID, pgtype.TextOID, pgtype.TextOID},
			Rows:  [][]any{{"", []byte{}, nil, pgtype.Text{}}},
		}
	})
	var s string
	var b []byte
	var n1, n2 *string
	if err := f.QueryRow(context.Background(), "SELECT a, b, c, d FROM t").Scan(&s, &b, &n1, &n2); err != nil {
		t.Fatal(err)
	}
	if b == nil || n1 != nil || n2 != nil {
		t.Errorf("bytea %v (nil %v), NULLs %v %v: want an empty bytea and two NULLs", b, b == nil, n1, n2)
	}
}

// TestTypesOverrideInference: Result.Types decides the column type; a value
// pgx cannot encode under it fails the test, naming the query.
func TestTypesOverrideInference(t *testing.T) {
	f := pgmotest.New(t)
	f.On("int4", func(pgmotest.Query) pgmotest.Result {
		return pgmotest.Result{Types: []uint32{pgtype.Int4OID}, Rows: [][]any{{int64(5)}}}
	})
	rows, err := f.Query(context.Background(), "SELECT 5::int4")
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	if oid := rows.FieldDescriptions()[0].DataTypeOID; oid != pgtype.Int4OID {
		t.Errorf("OID %d, want int4", oid)
	}

	r := &recorder{TB: t}
	g := pgmotest.New(r)
	g.On("bad", func(pgmotest.Query) pgmotest.Result {
		return pgmotest.Result{Types: []uint32{pgtype.BoolOID}, Rows: [][]any{{"not a bool"}}}
	})
	if _, err := g.Query(context.Background(), "SELECT bad"); err == nil {
		t.Error("a string encoded as bool")
	}
	if e := r.errors(); len(e) != 1 || !strings.Contains(e[0], `"SELECT bad"`) {
		t.Errorf("errors = %q, want one naming the query", e)
	}
}

// TestUnansweredQueryFailsTheTest: a read no rule answers is an error to the
// caller and a test failure naming it; an unanswered write is not.
func TestUnansweredQueryFailsTheTest(t *testing.T) {
	r := &recorder{TB: t}
	f := pgmotest.New(r)
	f.OnExec("thing", pgmotest.Tag("DELETE 1"))
	ctx := context.Background()
	if _, err := f.Query(ctx, "SELECT thing"); err == nil {
		t.Error("an OnExec rule answered a query")
	}
	if e := r.errors(); len(e) != 1 || !strings.Contains(e[0], `no rule answers query "SELECT thing"`) {
		t.Errorf("errors = %q", e)
	}
	tag, err := f.Exec(ctx, "DELETE FROM thing WHERE id = $1", 7)
	if err != nil || tag.String() != "DELETE 1" || tag.RowsAffected() != 1 {
		t.Errorf("matched exec: %q, %v", tag, err)
	}
	tag, err = f.Exec(ctx, "VACUUM")
	if err != nil || tag.String() != "" {
		t.Errorf("unmatched exec: %q, %v; want success", tag, err)
	}
	q := f.Queries()
	if len(q) != 3 || q[1].Kind != pgmotest.KindExec || q[1].Arg(0) != 7 || q[1].Arg(1) != nil {
		t.Errorf("queries = %+v", q)
	}
	if len(r.errors()) != 1 {
		t.Errorf("errors = %q, want only the query's", r.errors())
	}
}

// TestRulesInOrderBeforeDefaults: the first matching rule answers, and a
// test's rule overrides the default version read.
func TestRulesInOrderBeforeDefaults(t *testing.T) {
	f := pgmotest.New(t)
	f.On("server_version_num", pgmotest.Rows([]any{int32(170003), "postgres"}))
	f.On("server_version_num", pgmotest.Rows([]any{int32(150000), "postgres"}))
	if v := f.Server().Version(); v != 170003 {
		t.Errorf("Version() = %d, want the first rule's 170003", v)
	}
}

func TestQueryRowNoRowsIsPgxsError(t *testing.T) {
	f := pgmotest.New(t)
	f.On("FROM t", pgmotest.Rows())
	var n int
	if err := f.QueryRow(context.Background(), "SELECT n FROM t").Scan(&n); !errors.Is(err, pgx.ErrNoRows) {
		t.Errorf("err = %v, want pgx.ErrNoRows", err)
	}
}

// TestFailEarlyAndLate: an error with no rows fails Query itself; after
// rows, they are delivered first and the error ends them through Err.
func TestFailEarlyAndLate(t *testing.T) {
	f := pgmotest.New(t)
	denied := &pgconn.PgError{Code: "42501", Message: "permission denied for table t"}
	f.On("early", pgmotest.Fail(denied))
	f.On("late", func(pgmotest.Query) pgmotest.Result {
		return pgmotest.Result{Rows: [][]any{{int64(1)}}, Err: denied}
	})
	ctx := context.Background()
	if _, err := f.Query(ctx, "SELECT early"); !pgmo.IsPermissionDenied(err) {
		t.Errorf("early: err = %v, want permission denied", err)
	}
	var n int
	if err := f.QueryRow(ctx, "SELECT early").Scan(&n); !pgmo.IsPermissionDenied(err) {
		t.Errorf("early QueryRow: err = %v", err)
	}
	rows, err := f.Query(ctx, "SELECT late")
	if err != nil {
		t.Fatal(err)
	}
	got := 0
	for rows.Next() {
		got++
	}
	if got != 1 || !pgmo.IsPermissionDenied(rows.Err()) {
		t.Errorf("late: %d rows, Err %v; want 1 and permission denied", got, rows.Err())
	}
	if _, err := f.Exec(ctx, "UPDATE early"); !pgmo.IsPermissionDenied(err) {
		t.Errorf("exec: err = %v", err)
	}
}

// TestLeadingOptionsAsPgx: NamedArgs rewrites the statement a rule sees;
// exec modes and result formats are not arguments.
func TestLeadingOptionsAsPgx(t *testing.T) {
	f := pgmotest.New(t)
	f.On("FROM t", pgmotest.Rows([]any{int64(1)}))
	ctx := context.Background()
	rows, err := f.Query(ctx, "SELECT n FROM t WHERE id = @id", pgx.QueryExecModeSimpleProtocol, pgx.QueryResultFormats{1}, pgx.NamedArgs{"id": 9})
	if err != nil {
		t.Fatal(err)
	}
	rows.Close()
	if _, err := f.Exec(ctx, "DELETE FROM t WHERE id = $1", pgx.QueryExecModeExec, 4); err != nil {
		t.Fatal(err)
	}
	q := f.Queries()
	if q[0].Text != "SELECT n FROM t WHERE id = $1" || len(q[0].Args) != 1 || q[0].Arg(0) != 9 {
		t.Errorf("query recorded as %q %v", q[0].Text, q[0].Args)
	}
	if len(q[1].Args) != 1 || q[1].Arg(0) != 4 {
		t.Errorf("exec args %v, want [4]", q[1].Args)
	}
	// Exec takes no result formats: pgx passes one on as an argument.
	if _, err := f.Exec(ctx, "SELECT $1", pgx.QueryResultFormats{1}); err != nil {
		t.Fatal(err)
	}
	if q := f.Queries(); len(q[2].Args) != 1 {
		t.Errorf("exec with result formats: args %v, want them kept as the argument", q[2].Args)
	}
	if q[0].Ctx != ctx {
		t.Error("the statement's context is not recorded")
	}
}

// TestTransactionsSendWhatPgxSends: begin, savepoints, commit and rollback
// are recorded as pgx's statements, numbered by transaction, and a closed
// transaction refuses as pgx's does.
func TestTransactionsSendWhatPgxSends(t *testing.T) {
	f := pgmotest.New(t)
	ctx := context.Background()
	if _, err := f.Exec(ctx, "SELECT outside"); err != nil {
		t.Fatal(err)
	}
	tx, err := f.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "INSERT 1"); err != nil {
		t.Fatal(err)
	}
	sp1, err := tx.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := sp1.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if err := sp1.Commit(ctx); !errors.Is(err, pgx.ErrTxClosed) {
		t.Errorf("commit of a rolled-back savepoint: %v", err)
	}
	sp2, err := tx.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := sp2.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := tx.Exec(ctx, "INSERT 2"); !errors.Is(err, pgx.ErrTxClosed) {
		t.Errorf("exec after commit: %v", err)
	}
	if err := tx.Rollback(ctx); !errors.Is(err, pgx.ErrTxClosed) {
		t.Errorf("rollback after commit: %v", err)
	}
	var got []string
	for _, q := range f.Queries() {
		got = append(got, fmt.Sprintf("%d %s %s", q.Tx, q.Kind, q.Text))
	}
	want := []string{
		"0 exec SELECT outside",
		"1 exec begin",
		"1 exec INSERT 1",
		"1 exec savepoint sp_1",
		"1 exec rollback to savepoint sp_1",
		"1 exec savepoint sp_2",
		"1 exec release savepoint sp_2",
		"1 exec commit",
	}
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("sent:\n%s\nwant:\n%s", strings.Join(got, "\n"), strings.Join(want, "\n"))
	}

	// A savepoint whose transaction has ended is closed with it.
	tx3, err := f.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	sp3, err := tx3.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx3.Rollback(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := sp3.Exec(ctx, "INSERT 3"); !errors.Is(err, pgx.ErrTxClosed) {
		t.Errorf("exec on a savepoint of an ended transaction: %v", err)
	}
	if err := sp3.Commit(ctx); !errors.Is(err, pgx.ErrTxClosed) {
		t.Errorf("commit of a savepoint of an ended transaction: %v", err)
	}

	// A further transaction has the next number; a commit the server turns
	// into a rollback is pgx's ErrTxCommitRollback.
	f.OnExec("commit", pgmotest.Tag("ROLLBACK"))
	tx2, err := f.Begin(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err := tx2.Commit(ctx); !errors.Is(err, pgx.ErrTxCommitRollback) {
		t.Errorf("commit answered ROLLBACK: %v", err)
	}
	if q := f.Queries(); q[len(q)-1].Tx != 3 {
		t.Errorf("third transaction numbered %d", q[len(q)-1].Tx)
	}

	// A failed begin leaves no transaction behind.
	f.OnExec("begin", pgmotest.Fail(errors.New("boom")))
	if _, err := f.Begin(ctx); err == nil {
		t.Error("begin succeeded over a failing rule")
	}
}

func TestCopyFrom(t *testing.T) {
	f := pgmotest.New(t)
	ctx := context.Background()
	n, err := f.CopyFrom(ctx, pgx.Identifier{"Sales", "t"}, []string{"a", `b"c`}, pgx.CopyFromRows([][]any{{1, "x"}, {2, nil}}))
	if err != nil || n != 2 {
		t.Fatalf("CopyFrom = %d, %v", n, err)
	}
	q := f.Queries()[0]
	if q.Kind != pgmotest.KindCopy || q.Text != `copy "Sales"."t" ( "a", "b""c" ) from stdin binary;` || len(q.Copy) != 2 || q.Copy[1][0] != 2 {
		t.Errorf("recorded %+v", q)
	}
	if _, err := f.CopyFrom(ctx, pgx.Identifier{"t"}, []string{"a", "b"}, pgx.CopyFromRows([][]any{{1}})); err == nil || err.Error() != "expected 2 values, got 1 values" {
		t.Errorf("short row: %v", err)
	}
	f.OnExec("copy", pgmotest.Fail(&pgconn.PgError{Code: "42P01"}))
	if _, err := f.CopyFrom(ctx, pgx.Identifier{"t"}, []string{"a"}, pgx.CopyFromRows([][]any{{1}})); !pgmo.IsUndefinedObject(err) {
		t.Errorf("failing rule: %v", err)
	}
}

// TestLeaksFailTheTest: rows never closed and a transaction never ended are
// reported when the test ends; closed ones are not.
func TestLeaksFailTheTest(t *testing.T) {
	var r *recorder
	t.Run("leaky", func(t *testing.T) {
		r = &recorder{TB: t}
		f := pgmotest.New(r)
		f.On("FROM", pgmotest.Rows([]any{int64(1)}, []any{int64(2)}))
		ctx := context.Background()
		if _, err := f.Query(ctx, "SELECT a FROM leaked"); err != nil {
			t.Fatal(err)
		}
		done, _ := f.Query(ctx, "SELECT a FROM drained")
		for done.Next() {
		}
		var n int
		_ = f.QueryRow(ctx, "SELECT a FROM row").Scan(&n)
		if _, err := f.Begin(ctx); err != nil {
			t.Fatal(err)
		}
		ended, _ := f.Begin(ctx)
		_ = ended.Rollback(ctx)
	})
	e := r.errors()
	if len(e) != 2 || !strings.Contains(strings.Join(e, "\n"), `rows of "SELECT a FROM leaked" were never closed`) ||
		!strings.Contains(strings.Join(e, "\n"), "1 transaction(s) never committed") {
		t.Errorf("errors = %q, want the leaked rows and the one open transaction", e)
	}
}

// TestConcurrentUse is for -race.
func TestConcurrentUse(t *testing.T) {
	f := pgmotest.New(t)
	f.On("FROM t", pgmotest.Rows([]any{int64(1), "a"}))
	s := f.Server()
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Go(func() {
			var n int64
			var name string
			if err := s.Querier().QueryRow(context.Background(), "SELECT n, name FROM t WHERE i = $1", i).Scan(&n, &name); err != nil {
				t.Error(err)
			}
			tx, err := f.Begin(context.Background())
			if err != nil {
				t.Error(err)
				return
			}
			_ = tx.Rollback(context.Background())
		})
	}
	wg.Wait()
	if got := len(f.Queries()); got != 1+8*3 {
		t.Errorf("%d statements recorded, want 25", got)
	}
}

// TestDatabaseFakes: each other database has a Fake of its own, with its own
// rules and Queries; the Server opens it on the first statement there,
// closes it on eviction and reopens the same Fake.
func TestDatabaseFakes(t *testing.T) {
	f := pgmotest.New(t)
	f.Database("a").On("SELECT name", pgmotest.Rows([]any{"in a"}))
	f.Database("b").On("SELECT name", pgmotest.Rows([]any{"in b"}))
	srv := f.Server(pgmo.WithMaxDatabasePools(1))
	ctx := context.Background()
	if srv.CurrentDatabase() != pgmotest.DefaultDatabase || f.Database(pgmotest.DefaultDatabase) != f {
		t.Errorf("CurrentDatabase() = %q; Database(current) is not f", srv.CurrentDatabase())
	}
	read := func(db string) string {
		t.Helper()
		var s string
		if err := srv.DatabaseQuerier(db).QueryRow(ctx, "SELECT name").Scan(&s); err != nil {
			t.Fatalf("read in %s: %v", db, err)
		}
		return s
	}
	if got := read("a"); got != "in a" {
		t.Errorf("a answered %q", got)
	}
	if got := read("b"); got != "in b" {
		t.Errorf("b answered %q", got)
	}
	a := f.Database("a")
	if !a.Closed() {
		t.Error("a is still open after b's open under a cap of 1")
	}
	if got := read("a"); got != "in a" || a.Opens() != 2 || len(a.Queries()) != 2 {
		t.Errorf("a reopened: answered %q, opened %d times, %d queries; want in a, 2, 2", got, a.Opens(), len(a.Queries()))
	}
	if len(f.Queries()) != 1 || f.Database("c").Opens() != 0 {
		t.Errorf("the current database saw %d queries, c was opened %d times; want 1 (the version read) and 0",
			len(f.Queries()), f.Database("c").Opens())
	}
	if f.Database("a").Server() != srv {
		t.Error("Server() on a database's Fake is not the Server")
	}
}

// TestDatabasePoolsUnderConcurrency drives many databases from many
// goroutines under a small cap, rows held open across others' opens: every
// read reaches its own database, and nothing races (run with -race).
func TestDatabasePoolsUnderConcurrency(t *testing.T) {
	f := pgmotest.New(t)
	dbs := []string{"a", "b", "c", "d", "e"}
	for _, db := range dbs {
		f.Database(db).On("SELECT name", pgmotest.Rows([]any{db}))
	}
	srv := f.Server(pgmo.WithMaxDatabasePools(2))
	ctx := context.Background()
	var wg sync.WaitGroup
	for g := range 16 {
		wg.Go(func() {
			for i := range 50 {
				db := dbs[(g+i)%len(dbs)]
				rows, err := srv.DatabaseQuerier(db).Query(ctx, "SELECT name")
				if err != nil {
					t.Errorf("%s: %v", db, err)
					return
				}
				for rows.Next() {
					var s string
					if err := rows.Scan(&s); err != nil || s != db {
						t.Errorf("%s answered %q, %v", db, s, err)
					}
				}
				rows.Close()
			}
		})
	}
	wg.Wait()
	open := 0
	for _, db := range dbs {
		if !f.Database(db).Closed() {
			open++
		}
	}
	srv.Close()
	for _, db := range dbs {
		if !f.Database(db).Closed() {
			t.Errorf("%s is open after Close", db)
		}
	}
	t.Logf("%d databases open when the readers finished (cap 2, soft)", open)
}

// A transient failure is scripted with a Responder that fails and then
// answers: pgmo's reads retry it on a Fake as on a pool (the version read
// here), and never on a transaction begun from it.
func TestTransientFailureIsRetried(t *testing.T) {
	const versionRead = "pg_catalog.current_setting('server_version_num')"
	flaky := func() pgmotest.Responder {
		n := 0
		return func(pgmotest.Query) pgmotest.Result {
			if n++; n == 1 {
				return pgmotest.Result{Err: &pgconn.PgError{Code: "57P01"}}
			}
			return pgmotest.Result{Rows: [][]any{{int32(pgmotest.DefaultVersion), pgmotest.DefaultDatabase}}}
		}
	}

	f := pgmotest.New(t)
	f.On(versionRead, flaky())
	if _, err := pgmo.NewServer(t.Context(), f); err != nil {
		t.Fatalf("NewServer over a read failing once: %v", err)
	}
	if n := len(f.Queries()); n != 2 {
		t.Errorf("statements sent = %d, want 2 (the read and its retry)", n)
	}

	g := pgmotest.New(t)
	g.On(versionRead, flaky())
	tx, err := g.Begin(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	defer tx.Rollback(t.Context())
	if _, err := pgmo.NewServer(t.Context(), tx); !pgmo.IsRetryable(err) {
		t.Errorf("NewServer over a transaction = %v, want the 57P01, not a retry", err)
	}
	if n := len(g.Queries()); n != 2 {
		t.Errorf("statements sent = %d, want 2 (begin and the one read)", n)
	}
}

// TestHoldAnswersOnRelease: a held statement is in flight until release is
// closed, then answered by the wrapped Responder.
func TestHoldAnswersOnRelease(t *testing.T) {
	f := pgmotest.New(t)
	release := make(chan struct{})
	f.On("SELECT held", pgmotest.Hold(release, pgmotest.Rows([]any{int64(7)})))
	got := make(chan error, 1)
	var n int64
	go func() { got <- f.QueryRow(context.Background(), "SELECT held").Scan(&n) }()
	select {
	case err := <-got:
		t.Fatalf("answered before release: %v", err)
	case <-time.After(50 * time.Millisecond):
	}
	close(release)
	if err := <-got; err != nil || n != 7 {
		t.Errorf("after release: n = %d, err = %v; want 7, nil", n, err)
	}
}

// TestHoldIsCancelledAsOnAConnection: a held statement whose context ends
// fails with the server's 57014, and pgmo's own reads (here the version
// read under NewServer) add the context's error to it.
func TestHoldIsCancelledAsOnAConnection(t *testing.T) {
	f := pgmotest.New(t)
	f.On("current_setting('server_version_num')", pgmotest.Hold(make(chan struct{}), pgmotest.Rows()))
	ctx, cancel := context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	_, err := f.Exec(ctx, "SELECT pg_catalog.current_setting('server_version_num')")
	if !pgmo.IsQueryCanceled(err) {
		t.Errorf("held Exec, cancelled: err = %v, want 57014", err)
	}

	ctx, cancel = context.WithCancel(context.Background())
	time.AfterFunc(50*time.Millisecond, cancel)
	_, err = pgmo.NewServer(ctx, f)
	if !pgmo.IsQueryCanceled(err) || !errors.Is(err, context.Canceled) {
		t.Errorf("NewServer, cancelled: err = %v, want 57014 carrying context.Canceled", err)
	}
}

// TestDoneContextSendsNothing: as pgx, a statement under a context already
// ended is not sent — the context's error, nothing recorded.
func TestDoneContextSendsNothing(t *testing.T) {
	f := pgmotest.New(t)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := f.Exec(ctx, "UPDATE t SET a = 1"); !errors.Is(err, context.Canceled) {
		t.Errorf("Exec: err = %v, want context.Canceled", err)
	}
	if _, err := f.Query(ctx, "SELECT 1"); !errors.Is(err, context.Canceled) {
		t.Errorf("Query: err = %v, want context.Canceled", err)
	}
	if _, err := f.Begin(ctx); !errors.Is(err, context.Canceled) {
		t.Errorf("Begin: err = %v, want context.Canceled", err)
	}
	if q := f.Queries(); len(q) != 0 {
		t.Errorf("recorded %d statements, want none sent", len(q))
	}
}
