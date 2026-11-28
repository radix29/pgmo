package pgmo_test

import (
	"context"
	"testing"

	"github.com/radix29/pgmo"
	"github.com/radix29/pgmo/pgmotest"
)

// The script_*_write_test.go files pin the exact statement every write
// method emits, with WithScript as a serverless harness: the write
// chokepoints capture before anything reaches the Querier.
//
// They assert the whole statement, not a substring: the bug they exist to
// catch is a quoting one — a name that loses its double quotes, a literal
// whose apostrophe stops being doubled — which a substring match passes
// over. So every case feeds a quote-hostile value through each parameter
// that reaches the statement text:
//
//   - My "DB"   upper case, a space and a double quote: an identifier
//     that must be quoted, its quote doubled
//   - o'brien   an apostrophe, which must double inside a literal
//   - a\b       a backslash, which QuoteLiteral writes as E'…' so the
//     text reads the same whatever standard_conforming_strings is
//   - user      a reserved keyword, which must be quoted to be a name

// scriptCase is one write call and the statement it must produce.
type scriptCase struct {
	name string
	call func(context.Context) error
	want string
}

// runScriptCases runs each case under its own collector and compares the one
// statement it captured, and the database it records, against want and the
// Server's own database: every write here runs there.
func runScriptCases(t *testing.T, f *pgmotest.Fake, cases []scriptCase) {
	t.Helper()
	runScriptCasesIn(t, f, pgmotest.DefaultDatabase, cases)
}

// runScriptCasesIn is runScriptCases for writes to objects inside database:
// each must be captured as run there, on that database's own session.
func runScriptCasesIn(t *testing.T, f *pgmotest.Fake, database string, cases []scriptCase) {
	t.Helper()
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			before, beforeIn := len(f.Queries()), len(f.Database(database).Queries())
			ctx, script := pgmo.WithScript(t.Context())
			if err := c.call(ctx); err != nil {
				t.Fatalf("under WithScript: %v", err)
			}
			got := script.Entries()
			if len(got) != 1 {
				t.Fatalf("captured %d entries, want 1:\n%s", len(got), script)
			}
			if got[0].SQL != c.want {
				t.Errorf("statement mismatch\n got: %s\nwant: %s", got[0].SQL, c.want)
			}
			if got[0].Database != database {
				t.Errorf("captured as run in %q, want %q", got[0].Database, database)
			}
			if q := f.Queries()[before:]; len(q) != 0 {
				t.Errorf("sent under WithScript: %v", q)
			}
			if q := f.Database(database).Queries()[beforeIn:]; len(q) != 0 {
				t.Errorf("sent to %q under WithScript: %v", database, q)
			}
		})
	}
}

func TestScriptDatabaseWrites(t *testing.T) {
	f := pgmotest.New(t)
	s := f.Server()
	d := s.DatabaseRef(`My "DB"`)

	runScriptCases(t, f, []scriptCase{
		{"create, name only", func(ctx context.Context) error {
			_, err := s.CreateDatabase(ctx, pgmo.CreateDatabaseRequest{Name: `My "DB"`})
			return err
		}, `CREATE DATABASE "My ""DB"""`},
		{"create, every option", func(ctx context.Context) error {
			_, err := s.CreateDatabase(ctx, pgmo.CreateDatabaseRequest{
				Name: `My "DB"`, Owner: "user", Template: "Tpl 0", Encoding: "UTF8",
				Strategy:       pgmo.DatabaseStrategyFileCopy,
				LocaleProvider: pgmo.LocaleProviderICU,
				Locale:         "o'brien", Collate: `a\b`, CType: "C.UTF-8",
				ICULocale: "en-US", ICURules: "&a < 'b'", BuiltinLocale: "C.UTF-8",
				Tablespace:       `fast "ssd"`,
				AllowConnections: new(false), ConnectionLimit: new(-1), IsTemplate: new(true),
			})
			return err
		}, `CREATE DATABASE "My ""DB""" WITH OWNER = "user" TEMPLATE = "Tpl 0" ENCODING = 'UTF8'` +
			` STRATEGY = FILE_COPY LOCALE_PROVIDER = icu LOCALE = 'o''brien' LC_COLLATE = E'a\\b'` +
			` LC_CTYPE = 'C.UTF-8' ICU_LOCALE = 'en-US' ICU_RULES = '&a < ''b''' BUILTIN_LOCALE = 'C.UTF-8'` +
			` TABLESPACE = "fast ""ssd""" ALLOW_CONNECTIONS = false CONNECTION LIMIT = -1 IS_TEMPLATE = true`},
		{"create, builtin, WAL_LOG", func(ctx context.Context) error {
			_, err := s.CreateDatabase(ctx, pgmo.CreateDatabaseRequest{
				Name: "app", Template: "template0", Strategy: pgmo.DatabaseStrategyWALLog,
				LocaleProvider: pgmo.LocaleProviderBuiltin, BuiltinLocale: "C.UTF-8",
				ConnectionLimit: new(0), AllowConnections: new(true), IsTemplate: new(false),
			})
			return err
		}, `CREATE DATABASE app WITH TEMPLATE = template0 STRATEGY = WAL_LOG LOCALE_PROVIDER = builtin` +
			` BUILTIN_LOCALE = 'C.UTF-8' ALLOW_CONNECTIONS = true CONNECTION LIMIT = 0 IS_TEMPLATE = false`},
		{"create, libc", func(ctx context.Context) error {
			_, err := s.CreateDatabase(ctx, pgmo.CreateDatabaseRequest{Name: "app", LocaleProvider: pgmo.LocaleProviderLibc})
			return err
		}, `CREATE DATABASE app WITH LOCALE_PROVIDER = libc`},
		{"drop", func(ctx context.Context) error {
			return d.Drop(ctx, pgmo.DropDatabaseOptions{})
		}, `DROP DATABASE "My ""DB"""`},
		{"drop with force", func(ctx context.Context) error {
			return d.Drop(ctx, pgmo.DropDatabaseOptions{Force: true})
		}, `DROP DATABASE "My ""DB""" WITH (FORCE)`},
		{"rename", func(ctx context.Context) error {
			return s.DatabaseRef(`My "DB"`).Rename(ctx, "user")
		}, `ALTER DATABASE "My ""DB""" RENAME TO "user"`},
		{"owner", func(ctx context.Context) error {
			return d.SetOwner(ctx, `O"wner`)
		}, `ALTER DATABASE "My ""DB""" OWNER TO "O""wner"`},
		{"tablespace", func(ctx context.Context) error {
			return d.SetTablespace(ctx, "user")
		}, `ALTER DATABASE "My ""DB""" SET TABLESPACE "user"`},
		{"alter every option", func(ctx context.Context) error {
			return d.Alter(ctx, pgmo.DatabaseChanges{AllowConnections: new(true), ConnectionLimit: new(25), IsTemplate: new(false)})
		}, `ALTER DATABASE "My ""DB""" WITH ALLOW_CONNECTIONS = true CONNECTION LIMIT = 25 IS_TEMPLATE = false`},
		{"alter one option", func(ctx context.Context) error {
			return d.Alter(ctx, pgmo.DatabaseChanges{ConnectionLimit: new(-1)})
		}, `ALTER DATABASE "My ""DB""" WITH CONNECTION LIMIT = -1`},
		{"set a parameter", func(ctx context.Context) error {
			return d.SetParameter(ctx, "work_mem", "64MB")
		}, `ALTER DATABASE "My ""DB""" SET work_mem = '64MB'`},
		{"set a list parameter", func(ctx context.Context) error {
			return d.SetParameter(ctx, "search_path", "o'brien", "$user", `a\b`, "")
		}, `ALTER DATABASE "My ""DB""" SET search_path = 'o''brien', '$user', E'a\\b', ''`},
		{"set a custom parameter", func(ctx context.Context) error {
			return d.SetParameter(ctx, "myext.Option", "on")
		}, `ALTER DATABASE "My ""DB""" SET "myext.Option" = 'on'`},
		{"reset a parameter", func(ctx context.Context) error {
			return d.ResetParameter(ctx, "DateStyle")
		}, `ALTER DATABASE "My ""DB""" RESET "DateStyle"`},
		{"reset every parameter", func(ctx context.Context) error {
			return d.ResetAllParameters(ctx)
		}, `ALTER DATABASE "My ""DB""" RESET ALL`},
	})
}
