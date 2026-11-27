package pgmo_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/radix29/pgmo"
	"github.com/radix29/pgmo/pgmotest"
)

// Every refusal is decided before anything is sent, and reaches its
// sentinel: the caller's arguments ErrInvalidRequest, a release the server
// predates ErrUnsupportedVersion, a statement refused in a transaction block
// ErrUnsupported.
func TestDatabaseWriteRefusals(t *testing.T) {
	f := pgmotest.New(t)
	f.Version = pgmo.PostgreSQL15
	s := f.Server()
	d := s.DatabaseRef("app")
	own := s.DatabaseRef(pgmotest.DefaultDatabase)
	create := func(req pgmo.CreateDatabaseRequest) func(context.Context) error {
		return func(ctx context.Context) error { _, err := s.CreateDatabase(ctx, req); return err }
	}
	cases := []struct {
		name string
		call func(context.Context) error
		kind error
	}{
		{"create, no name", create(pgmo.CreateDatabaseRequest{}), pgmo.ErrInvalidRequest},
		{"create, unknown strategy", create(pgmo.CreateDatabaseRequest{Name: "x", Strategy: "wal_log"}), pgmo.ErrInvalidRequest},
		{"create, collation's default provider", create(pgmo.CreateDatabaseRequest{Name: "x", LocaleProvider: pgmo.LocaleProviderDefault}), pgmo.ErrInvalidRequest},
		{"create, unknown provider", create(pgmo.CreateDatabaseRequest{Name: "x", LocaleProvider: "z"}), pgmo.ErrInvalidRequest},
		{"create, limit below -1", create(pgmo.CreateDatabaseRequest{Name: "x", ConnectionLimit: new(-2)}), pgmo.ErrInvalidRequest},
		{"create, ICU rules on 15", create(pgmo.CreateDatabaseRequest{Name: "x", ICURules: "&a < b"}), pgmo.ErrUnsupportedVersion},
		{"create, builtin provider on 15", create(pgmo.CreateDatabaseRequest{Name: "x", LocaleProvider: pgmo.LocaleProviderBuiltin}), pgmo.ErrUnsupportedVersion},
		{"create, builtin locale on 15", create(pgmo.CreateDatabaseRequest{Name: "x", BuiltinLocale: "C.UTF-8"}), pgmo.ErrUnsupportedVersion},
		{"drop, no name", func(ctx context.Context) error { return s.DatabaseRef("").Drop(ctx, pgmo.DropDatabaseOptions{}) }, pgmo.ErrInvalidRequest},
		{"drop, own database", func(ctx context.Context) error { return own.Drop(ctx, pgmo.DropDatabaseOptions{Force: true}) }, pgmo.ErrInvalidRequest},
		{"rename, no new name", func(ctx context.Context) error { return d.Rename(ctx, "") }, pgmo.ErrInvalidRequest},
		{"rename, no name", func(ctx context.Context) error { return s.DatabaseRef("").Rename(ctx, "y") }, pgmo.ErrInvalidRequest},
		{"rename, own database", func(ctx context.Context) error { return own.Rename(ctx, "y") }, pgmo.ErrInvalidRequest},
		{"owner, no role", func(ctx context.Context) error { return d.SetOwner(ctx, "") }, pgmo.ErrInvalidRequest},
		{"owner, no name", func(ctx context.Context) error { return s.DatabaseRef("").SetOwner(ctx, "r") }, pgmo.ErrInvalidRequest},
		{"tablespace, none", func(ctx context.Context) error { return d.SetTablespace(ctx, "") }, pgmo.ErrInvalidRequest},
		{"tablespace, no name", func(ctx context.Context) error { return s.DatabaseRef("").SetTablespace(ctx, "t") }, pgmo.ErrInvalidRequest},
		{"tablespace, own database", func(ctx context.Context) error { return own.SetTablespace(ctx, "t") }, pgmo.ErrInvalidRequest},
		{"alter, limit below -1", func(ctx context.Context) error {
			return d.Alter(ctx, pgmo.DatabaseChanges{ConnectionLimit: new(-2)})
		}, pgmo.ErrInvalidRequest},
		{"alter, no name", func(ctx context.Context) error {
			return s.DatabaseRef("").Alter(ctx, pgmo.DatabaseChanges{IsTemplate: new(true)})
		}, pgmo.ErrInvalidRequest},
		{"set, no parameter", func(ctx context.Context) error { return d.SetParameter(ctx, "", "x") }, pgmo.ErrInvalidRequest},
		{"set, no value", func(ctx context.Context) error { return d.SetParameter(ctx, "work_mem") }, pgmo.ErrInvalidRequest},
		{"set, no name", func(ctx context.Context) error { return s.DatabaseRef("").SetParameter(ctx, "work_mem", "1MB") }, pgmo.ErrInvalidRequest},
		{"reset, no parameter", func(ctx context.Context) error { return d.ResetParameter(ctx, "") }, pgmo.ErrInvalidRequest},
		{"reset, no name", func(ctx context.Context) error { return s.DatabaseRef("").ResetParameter(ctx, "work_mem") }, pgmo.ErrInvalidRequest},
		{"reset all, no name", func(ctx context.Context) error { return s.DatabaseRef("").ResetAllParameters(ctx) }, pgmo.ErrInvalidRequest},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			for _, scripting := range []bool{false, true} {
				ctx := t.Context()
				var script *pgmo.ScriptCollector
				if scripting {
					ctx, script = pgmo.WithScript(ctx)
				}
				before := len(f.Queries())
				err := c.call(ctx)
				if !errors.Is(err, c.kind) {
					t.Errorf("scripting %v: %v, want %v", scripting, err, c.kind)
				}
				if err != nil && !strings.HasPrefix(err.Error(), "pgmo: ") {
					t.Errorf("error %q lacks the pgmo: prefix", err)
				}
				if q := f.Queries()[before:]; len(q) != 0 {
					t.Errorf("scripting %v: sent %v before refusing", scripting, q)
				}
				if script != nil && script.Len() != 0 {
					t.Errorf("captured %q before refusing", script)
				}
			}
		})
	}
}

// On 16 ICU rules are written; the builtin provider still waits for 17.
func TestCreateDatabaseGatesFollowTheServer(t *testing.T) {
	f := pgmotest.New(t)
	f.Version = pgmo.PostgreSQL16
	s := f.Server()
	ctx, script := pgmo.WithScript(t.Context())
	if _, err := s.CreateDatabase(ctx, pgmo.CreateDatabaseRequest{Name: "x", ICURules: "&a < b"}); err != nil {
		t.Errorf("ICU rules on 16: %v", err)
	}
	if _, err := s.CreateDatabase(ctx, pgmo.CreateDatabaseRequest{Name: "x", BuiltinLocale: "C"}); !errors.Is(err, pgmo.ErrUnsupportedVersion) {
		t.Errorf("builtin locale on 16: %v, want ErrUnsupportedVersion", err)
	}
	if _, err := s.CreateDatabase(ctx, pgmo.CreateDatabaseRequest{Name: "x", LocaleProvider: pgmo.LocaleProviderBuiltin}); !errors.Is(err, pgmo.ErrUnsupportedVersion) {
		t.Errorf("builtin provider on 16: %v, want ErrUnsupportedVersion", err)
	}
	if want := "CREATE DATABASE x WITH ICU_RULES = '&a < b';\n"; script.String() != want {
		t.Errorf("captured %q, want %q", script, want)
	}
}

// CREATE DATABASE, DROP DATABASE and SET TABLESPACE cannot run in a
// transaction block: refused inside InTransaction, scripting or not; the
// ALTERs that can are sent on the transaction.
func TestDatabaseWritesInTransaction(t *testing.T) {
	f := pgmotest.New(t)
	s := f.Server()
	d := s.DatabaseRef("app")
	refused := map[string]func(context.Context) error{
		"create": func(ctx context.Context) error {
			_, err := s.CreateDatabase(ctx, pgmo.CreateDatabaseRequest{Name: "x"})
			return err
		},
		"drop":       func(ctx context.Context) error { return d.Drop(ctx, pgmo.DropDatabaseOptions{Force: true}) },
		"tablespace": func(ctx context.Context) error { return d.SetTablespace(ctx, "t") },
	}
	for name, call := range refused {
		for _, scripting := range []bool{false, true} {
			ctx := t.Context()
			if scripting {
				ctx, _ = pgmo.WithScript(ctx)
			}
			err := s.InTransaction(ctx, call)
			if !errors.Is(err, pgmo.ErrUnsupported) || !strings.Contains(err.Error(), "InTransaction") {
				t.Errorf("%s in a transaction (scripting %v): %v, want ErrUnsupported", name, scripting, err)
			}
		}
	}
	wantSent(t, f, "exec@1 begin", "exec@1 rollback", "exec@2 begin", "exec@2 rollback", "exec@3 begin", "exec@3 rollback")

	f2 := pgmotest.New(t)
	s2 := f2.Server()
	d2 := s2.DatabaseRef("app")
	err := s2.InTransaction(t.Context(), func(ctx context.Context) error {
		return errors.Join(
			d2.Rename(ctx, "app2"),
			d2.SetOwner(ctx, "r"),
			d2.Alter(ctx, pgmo.DatabaseChanges{IsTemplate: new(false)}),
			d2.SetParameter(ctx, "work_mem", "1MB"),
			d2.ResetParameter(ctx, "work_mem"),
			d2.ResetAllParameters(ctx),
		)
	})
	if err != nil {
		t.Fatal(err)
	}
	wantSent(t, f2, "exec@1 begin",
		"exec@1 ALTER DATABASE app RENAME TO app2",
		"exec@1 ALTER DATABASE app2 OWNER TO r",
		"exec@1 ALTER DATABASE app2 WITH IS_TEMPLATE = false",
		"exec@1 ALTER DATABASE app2 SET work_mem = '1MB'",
		"exec@1 ALTER DATABASE app2 RESET work_mem",
		"exec@1 ALTER DATABASE app2 RESET ALL",
		"exec@1 commit")
}

// A write that ran is mirrored onto the handle; under WithScript it is not,
// since the server still holds the old state.
func TestDatabaseWritesMirrorOnlyWhenApplied(t *testing.T) {
	f := pgmotest.New(t)
	s := f.Server()
	mk := func() *pgmo.Database {
		d := s.DatabaseRef("app")
		d.Owner, d.Tablespace, d.ConnectionLimit = "old", "pg_default", -1
		d.Settings = []pgmo.ParameterSetting{{Name: "work_mem", Value: "1MB"}}
		return d
	}
	apply := func(ctx context.Context, d *pgmo.Database) {
		t.Helper()
		if err := errors.Join(
			d.Rename(ctx, "app2"),
			d.SetOwner(ctx, "new"),
			d.SetTablespace(ctx, "fast"),
			d.Alter(ctx, pgmo.DatabaseChanges{AllowConnections: new(true), ConnectionLimit: new(7), IsTemplate: new(true)}),
			d.SetParameter(ctx, "work_mem", "2MB"),
			d.ResetAllParameters(ctx),
		); err != nil {
			t.Fatal(err)
		}
	}

	scripted := mk()
	sctx, _ := pgmo.WithScript(t.Context())
	apply(sctx, scripted)
	if scripted.Name != "app" || scripted.Owner != "old" || scripted.Tablespace != "pg_default" ||
		scripted.AllowConnections || scripted.ConnectionLimit != -1 || scripted.IsTemplate || len(scripted.Settings) != 1 {
		t.Errorf("a scripted write was mirrored: %+v", scripted)
	}

	ran := mk()
	apply(t.Context(), ran)
	if ran.Name != "app2" || ran.Owner != "new" || ran.Tablespace != "fast" ||
		!ran.AllowConnections || ran.ConnectionLimit != 7 || !ran.IsTemplate || ran.Settings != nil {
		t.Errorf("a write that ran was not mirrored: %+v", ran)
	}
	// Each statement after the rename names the new name.
	for _, q := range f.Queries()[1:] {
		if !strings.Contains(q.Text, "app2") {
			t.Errorf("sent %q, not naming the renamed database", q.Text)
		}
	}
}

// An empty DatabaseChanges sends nothing, even on a handle with no name.
func TestDatabaseAlterNothingSendsNothing(t *testing.T) {
	f := pgmotest.New(t)
	if err := f.Server().DatabaseRef("").Alter(t.Context(), pgmo.DatabaseChanges{}); err != nil {
		t.Fatal(err)
	}
	wantSent(t, f)
}

// CreateDatabase reads what it created back; under WithScript it returns
// the handle, reading nothing; a read-back that finds nothing returns the
// handle too, while a write the server refused is the error, wrapped once.
func TestCreateDatabaseReadsBack(t *testing.T) {
	f := pgmotest.New(t)
	f.On("FROM pg_catalog.pg_database d", databaseRows(rowDBBuiltin))
	s := f.Server()
	got, err := s.CreateDatabase(t.Context(), pgmo.CreateDatabaseRequest{Name: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if got.OID != 16390 || got.Owner != "owner_role" {
		t.Errorf("read back %+v, want the catalog's row", got)
	}
	if q := f.Queries(); len(q) < 3 || q[1].Text != "CREATE DATABASE app" || q[2].Arg(0) != "app" {
		t.Errorf("sent %v, want CREATE DATABASE then the read by its name", q)
	}

	sctx, _ := pgmo.WithScript(t.Context())
	n := len(f.Queries())
	h, err := s.CreateDatabase(sctx, pgmo.CreateDatabaseRequest{Name: "other"})
	if err != nil || h.Name != "other" || h.OID != 0 || len(f.Queries()) != n {
		t.Errorf("scripted create: %+v, %v, sent %d", h, err, len(f.Queries())-n)
	}

	f2 := pgmotest.New(t)
	f2.On("FROM pg_catalog.pg_database d", databaseRows())
	h, err = f2.Server().CreateDatabase(t.Context(), pgmo.CreateDatabaseRequest{Name: "hidden"})
	if err != nil || h.Name != "hidden" {
		t.Errorf("create read back as nothing: %+v, %v; want the handle", h, err)
	}

	f3 := pgmotest.New(t)
	f3.OnExec("CREATE DATABASE", pgmotest.Fail(&pgconn.PgError{Code: "42P04"}))
	_, err = f3.Server().CreateDatabase(t.Context(), pgmo.CreateDatabaseRequest{Name: "dup"})
	if !pgmo.IsAlreadyExists(err) || strings.Count(err.Error(), "pgmo:") != 1 ||
		!strings.HasPrefix(err.Error(), `pgmo: create database "dup": `) {
		t.Errorf("refused create: %v", err)
	}
}

// Drop, Rename and SetTablespace close the Server's own idle pool on the
// database first, so it is not the session in the way; the next statement
// there opens it again. A pool in use is left, and under WithScript nothing
// is closed. The other writes leave the pool alone.
func TestDatabaseWritesReleaseTheirPool(t *testing.T) {
	writes := map[string]func(context.Context, *pgmo.Database) error{
		"drop":       func(ctx context.Context, d *pgmo.Database) error { return d.Drop(ctx, pgmo.DropDatabaseOptions{}) },
		"rename":     func(ctx context.Context, d *pgmo.Database) error { return d.Rename(ctx, "renamed") },
		"tablespace": func(ctx context.Context, d *pgmo.Database) error { return d.SetTablespace(ctx, "t") },
	}
	for name, write := range writes {
		t.Run(name, func(t *testing.T) {
			f := pgmotest.New(t)
			app := f.Database("app")
			app.On("SELECT 1", pgmotest.Rows([]any{int32(1)}))
			s := f.Server()
			read := func() {
				t.Helper()
				if _, err := s.DatabaseQuerier("app").Exec(t.Context(), "SELECT 1"); err != nil {
					t.Fatal(err)
				}
			}
			read()

			sctx, _ := pgmo.WithScript(t.Context())
			if err := write(sctx, s.DatabaseRef("app")); err != nil {
				t.Fatal(err)
			}
			if app.Closed() {
				t.Error("a scripted write closed the pool")
			}

			if err := write(t.Context(), s.DatabaseRef("app")); err != nil {
				t.Fatal(err)
			}
			if !app.Closed() {
				t.Error("the idle pool was not closed before the write")
			}
			read()
			if app.Opens() != 2 || app.Closed() {
				t.Errorf("after the write the database opened %d times, closed %v; want reopened", app.Opens(), app.Closed())
			}

			// A pool holding open rows is in use: left as it is.
			rows, err := s.DatabaseQuerier("app").Query(t.Context(), "SELECT 1")
			if err != nil {
				t.Fatal(err)
			}
			if err := write(t.Context(), s.DatabaseRef("app")); err != nil {
				t.Fatal(err)
			}
			if app.Closed() {
				t.Error("a pool in use was closed")
			}
			rows.Close()
		})
	}

	f := pgmotest.New(t)
	app := f.Database("app")
	s := f.Server()
	if _, err := s.DatabaseQuerier("app").Exec(t.Context(), "SELECT 1"); err != nil {
		t.Fatal(err)
	}
	d := s.DatabaseRef("app")
	if err := errors.Join(d.SetOwner(t.Context(), "r"), d.Alter(t.Context(), pgmo.DatabaseChanges{IsTemplate: new(true)}),
		d.SetParameter(t.Context(), "work_mem", "1MB"), d.ResetParameter(t.Context(), "work_mem"), d.ResetAllParameters(t.Context())); err != nil {
		t.Fatal(err)
	}
	if app.Closed() {
		t.Error("a write that does not need the database free closed its pool")
	}
}

// Values splits a list-quoted parameter's stored text as pg_dump does, and
// leaves any other parameter — and a list that does not split — one value.
func TestParameterSettingValues(t *testing.T) {
	cases := []struct {
		name, value string
		want        []string
	}{
		// As the lab stored SET search_path = 'a b', '$user', '' on 15–18.
		{"search_path", `"a b", "$user", ""`, []string{"a b", "$user", ""}},
		{"search_path", `"$user", public`, []string{"$user", "public"}},
		{"search_path", `"Odd=Name", "say ""hi"""`, []string{"Odd=Name", `say "hi"`}},
		{"search_path", "  public  ,\tx\n", []string{"public", "x"}},
		{"temp_tablespaces", `"a b", c`, []string{"a b", "c"}},
		{"local_preload_libraries", `"a b", c`, []string{"a b", "c"}},
		{"session_preload_libraries", "a", []string{"a"}},
		{"shared_preload_libraries", "a,b", []string{"a", "b"}},
		{"unix_socket_directories", `"/tmp", /run`, []string{"/tmp", "/run"}},
		// Not list-quoted: one value, whatever it holds.
		{"DateStyle", "ISO, MDY", []string{"ISO, MDY"}},
		{"work_mem", "64MB", []string{"64MB"}},
		{"Search_Path", `"a", b`, []string{`"a", b`}},
		{"myext.opt", `"a", b`, []string{`"a", b`}},
		// List-quoted, but not splitting: the stored text as one value.
		{"search_path", "", []string{""}},
		{"search_path", "   ", []string{"   "}},
		{"search_path", `"unterminated`, []string{`"unterminated`}},
		{"search_path", "a,", []string{"a,"}},
		{"search_path", "a,,b", []string{"a,,b"}},
		{"search_path", `a b`, []string{"a b"}},
		{"search_path", `"a"b`, []string{`"a"b`}},
	}
	for _, c := range cases {
		got := pgmo.ParameterSetting{Name: c.name, Value: c.value}.Values()
		if !slices.Equal(got, c.want) {
			t.Errorf("%s=%q: Values %q, want %q", c.name, c.value, got, c.want)
		}
	}
}
