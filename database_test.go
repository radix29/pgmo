package pgmo_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/radix29/pgmo"
	"github.com/radix29/pgmo/pgmotest"
)

// databaseRows answers the database read with rows in its SELECT order, the
// columns typed as the server sends them.
func databaseRows(rows ...[]any) pgmotest.Responder {
	return func(pgmotest.Query) pgmotest.Result {
		return pgmotest.Result{
			Types: []uint32{pgtype.OIDOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID,
				pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID,
				pgtype.BoolOID, pgtype.BoolOID, pgtype.Int4OID, pgtype.TextOID,
				pgtype.TextArrayOID, pgtype.TextOID},
			Rows: rows,
		}
	}
}

// Two databases as pg18 answered for the live test's fixture: a builtin
// provider one with two settings (a list value holding '=') and a comment,
// and an ICU template with tailoring rules, no settings (NULL) and a limit.
var (
	rowDBBuiltin = []any{uint32(16390), "app", "owner_role", "UTF8", "b", "C.UTF-8", "C", "C.UTF-8", "", "1",
		false, true, -1, "pg_default",
		[]string{`search_path="$user", "Odd=Name", public`, "work_mem=64MB"}, "the app"}
	rowDBICU = []any{uint32(16391), "tpl", "postgres", "UTF8", "i", "C.UTF-8", "C.UTF-8", "und", "&a < g", "153.121",
		true, false, 3, "pg_default", nil, ""}
)

// Every field is filled from its own column, settings are split at their
// first '=', and the name is bound as $1, never written into the text.
func TestDatabaseByNameFillsEveryField(t *testing.T) {
	f := pgmotest.New(t)
	f.On("FROM pg_catalog.pg_database d", databaseRows(rowDBBuiltin))
	s := f.Server()

	got, err := s.DatabaseByName(t.Context(), "app")
	if err != nil {
		t.Fatal(err)
	}
	if got.Server() != s {
		t.Errorf("Server() = %p, want %p", got.Server(), s)
	}
	want := pgmo.Database{
		OID: 16390, Name: "app", Owner: "owner_role", Encoding: "UTF8",
		LocaleProvider: pgmo.LocaleProviderBuiltin, Collate: "C.UTF-8", CType: "C", Locale: "C.UTF-8",
		CollationVersion: "1", AllowConnections: true, ConnectionLimit: -1, Tablespace: "pg_default",
		Settings: []pgmo.ParameterSetting{
			{Name: "search_path", Value: `"$user", "Odd=Name", public`},
			{Name: "work_mem", Value: "64MB"},
		},
		Comment: "the app",
	}
	g := *got
	g.SetServer(nil)
	if !reflect.DeepEqual(g, want) {
		t.Errorf("DatabaseByName =\n%+v\nwant\n%+v", g, want)
	}
	if n := reflect.TypeFor[pgmo.Database]().NumField(); n != 17 {
		t.Errorf("Database has %d fields; this test checks 17 — add the new one", n)
	}
	q := f.Queries()
	last := q[len(q)-1]
	if last.Arg(0) != "app" || strings.Contains(last.Text, "'app'") {
		t.Errorf("name not bound as $1: args %v, text\n%s", last.Args, last.Text)
	}
	if !strings.Contains(last.Text, "d.datname = $1::pg_catalog.text") {
		t.Errorf("name not matched exactly:\n%s", last.Text)
	}
}

// The listing scans every row (NULL settings read as none) and pushes a
// name filter into its query, folded and bound after no arguments of its own.
func TestDatabasesFiltered(t *testing.T) {
	f := pgmotest.New(t)
	f.On("FROM pg_catalog.pg_database d", databaseRows(rowDBBuiltin, rowDBICU))
	s := f.Server()

	ds, err := s.Databases(t.Context(), pgmo.ObjectFilter{
		Name:   []pgmo.TextCriterion{{Op: pgmo.TextContains, Value: "APP"}},
		Schema: []pgmo.TextCriterion{{Op: pgmo.TextEquals, Value: "ignored"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ds) != 2 || ds[1].Name != "tpl" || ds[1].Settings != nil || !ds[1].IsTemplate ||
		ds[1].AllowConnections || ds[1].ConnectionLimit != 3 || ds[1].ICURules != "&a < g" ||
		ds[1].LocaleProvider != pgmo.LocaleProviderICU {
		t.Fatalf("Databases = %+v", ds)
	}
	q := f.Queries()
	last := q[len(q)-1]
	if !strings.Contains(last.Text, "AND pg_catalog.strpos(d.datname, $1) > 0") || len(last.Args) != 1 || last.Arg(0) != "app" {
		t.Errorf("filter not pushed down as strpos(d.datname, $1) with app: args %v\n%s", last.Args, last.Text)
	}
	if !strings.Contains(last.Text, "ORDER BY d.datname") {
		t.Errorf("not ordered by name:\n%s", last.Text)
	}

	// An unknown operator is refused before anything is sent.
	n := len(f.Queries())
	if _, err := s.Databases(t.Context(), pgmo.ObjectFilter{Name: []pgmo.TextCriterion{{Op: 99, Value: "x"}}}); !errors.Is(err, pgmo.ErrInvalidRequest) {
		t.Errorf("unknown operator: err = %v, want ErrInvalidRequest", err)
	}
	if len(f.Queries()) != n {
		t.Errorf("a refused filter was sent")
	}
}

// No row is ErrNotFound naming the database; a server error is wrapped once,
// naming the read.
func TestDatabaseByNameErrors(t *testing.T) {
	f := pgmotest.New(t)
	f.On("FROM pg_catalog.pg_database d", databaseRows())
	s := f.Server()
	_, err := s.DatabaseByName(t.Context(), "gone")
	if !errors.Is(err, pgmo.ErrNotFound) || !strings.Contains(err.Error(), `database "gone" not found`) {
		t.Errorf("missing: err = %v, want ErrNotFound naming it", err)
	}

	f = pgmotest.New(t)
	f.On("FROM pg_catalog.pg_database d", pgmotest.Fail(&pgconn.PgError{Code: "57014", Message: "canceled"}))
	_, err = f.Server().DatabaseByName(t.Context(), "app")
	if err == nil || errors.Is(err, pgmo.ErrNotFound) || !strings.HasPrefix(err.Error(), `pgmo: read database "app": `) ||
		strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("server error: err = %v, want it wrapped once naming the read", err)
	}
}

// DatabaseRef reads nothing and holds the name alone.
func TestDatabaseRef(t *testing.T) {
	f := pgmotest.New(t)
	s := f.Server()
	n := len(f.Queries())
	d := s.DatabaseRef("app")
	if len(f.Queries()) != n {
		t.Errorf("DatabaseRef sent a statement")
	}
	g := *d
	g.SetServer(nil)
	if d.Server() != s || !reflect.DeepEqual(g, pgmo.Database{Name: "app"}) || d.IsInvalid() {
		t.Errorf("DatabaseRef = %+v", d)
	}
}

// Size reads pg_database_size through pg_database by the bound name: no row
// is ErrNotFound, and the server's permission refusal stays recognisable.
func TestDatabaseSize(t *testing.T) {
	f := pgmotest.New(t)
	f.On("pg_database_size", pgmotest.Rows([]any{int64(7692815)}))
	s := f.Server()
	n, err := s.DatabaseRef("app").Size(t.Context())
	if err != nil || n != 7692815 {
		t.Fatalf("Size = %d, %v", n, err)
	}
	q := f.Queries()
	if last := q[len(q)-1]; last.Arg(0) != "app" || strings.Contains(last.Text, "'app'") {
		t.Errorf("name not bound: args %v\n%s", last.Args, last.Text)
	}

	f = pgmotest.New(t)
	f.On("pg_database_size", pgmotest.Rows())
	if _, err := f.Server().DatabaseRef("gone").Size(t.Context()); !errors.Is(err, pgmo.ErrNotFound) {
		t.Errorf("missing: err = %v, want ErrNotFound", err)
	}

	f = pgmotest.New(t)
	f.On("pg_database_size", pgmotest.Fail(&pgconn.PgError{Code: "42501", Message: "permission denied for database app"}))
	_, err = f.Server().DatabaseRef("app").Size(t.Context())
	if !pgmo.IsPermissionDenied(err) || errors.Is(err, pgmo.ErrNotFound) || !strings.HasPrefix(err.Error(), `pgmo: read database "app" size: `) {
		t.Errorf("no CONNECT: err = %v, want the server's 42501 wrapped naming the read", err)
	}
}

// IsInvalid is datconnlimit -2, and nothing else.
func TestDatabaseIsInvalid(t *testing.T) {
	for limit, want := range map[int]bool{-2: true, -1: false, 0: false, 3: false} {
		if got := (&pgmo.Database{ConnectionLimit: limit}).IsInvalid(); got != want {
			t.Errorf("ConnectionLimit %d: IsInvalid = %v, want %v", limit, got, want)
		}
	}
}

func TestLocaleProviderString(t *testing.T) {
	for p, want := range map[pgmo.LocaleProvider]string{
		pgmo.LocaleProviderLibc: "libc", pgmo.LocaleProviderICU: "icu", pgmo.LocaleProviderBuiltin: "builtin",
		pgmo.LocaleProviderDefault: "default", "x": `LocaleProvider("x")`,
	} {
		if got := p.String(); got != want {
			t.Errorf("%q.String() = %q, want %q", string(p), got, want)
		}
	}
}
