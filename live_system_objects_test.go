//go:build livedb

package pgmo

import (
	"fmt"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/radix29/pgmo/pgsql"
)

// TestLiveSystemObjects reads an Origin for relations, functions, types and
// schemas in a throwaway database holding extensions: catalog objects are
// system and owned by no extension, an extension's objects are members and
// not system, a table added with ALTER EXTENSION ADD becomes a member, a
// function that only DEPENDS ON EXTENSION (deptype 'x') does not, and the
// session's own temporary table is system. Then IsSystemSchema agrees with
// the SQL rule on every schema, and ObjectFilter's System and
// ExtensionMember split the relations exactly as their Origins do.
func TestLiveSystemObjects(t *testing.T) {
	admin, ctx, done := liveDB(t)
	t.Cleanup(done) // registered first, so it runs after the drop
	db := fmt.Sprintf("pgmo_live_system_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+pgsql.QuoteIdent(db)); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP DATABASE IF EXISTS "+pgsql.QuoteIdent(db)+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database: %v", err)
		}
	})
	cfg, err := pgx.ParseConfig(*liveConnString)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = db
	// One connection: the temporary table lives in its session's pg_temp_N.
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(ctx) })

	for _, stmt := range []string{
		`CREATE SCHEMA app`,
		`CREATE SCHEMA ext`,
		`CREATE SCHEMA adopted`,
		`CREATE EXTENSION citext SCHEMA ext`,
		`CREATE EXTENSION seg SCHEMA app`,
		`CREATE TABLE app.orders (id int)`,
		`CREATE TABLE app.joined (id int)`,
		`ALTER EXTENSION seg ADD TABLE app.joined`,
		`ALTER EXTENSION seg ADD SCHEMA adopted`,
		`CREATE FUNCTION app.depends() RETURNS int LANGUAGE sql AS 'SELECT 1'`,
		`ALTER FUNCTION app.depends() DEPENDS ON EXTENSION citext`,
		`CREATE FUNCTION app.plain() RETURNS int LANGUAGE sql AS 'SELECT 1'`,
		`CREATE TEMP TABLE scratch (id int)`,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	type named struct {
		schema, name string
		Origin
	}
	read := func(t *testing.T, sql string) map[[2]string]Origin {
		t.Helper()
		rows, err := scanRows(ctx, conn, "read origins", func(scan func(...any) error) (named, error) {
			var r named
			return r, scan(append([]any{&r.schema, &r.name}, scanOrigin(&r.Origin)...)...)
		}, sql)
		if err != nil {
			t.Fatal(err)
		}
		m := make(map[[2]string]Origin, len(rows))
		for _, r := range rows {
			m[[2]string{r.schema, r.name}] = r.Origin
		}
		return m
	}
	relations := read(t, `SELECT n.nspname, c.relname, `+originColumns("n.nspname", catalogClass, "c.oid")+`
		FROM pg_catalog.pg_class c JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace`)
	routines := read(t, `SELECT n.nspname, p.proname, `+originColumns("n.nspname", catalogProc, "p.oid")+`
		FROM pg_catalog.pg_proc p JOIN pg_catalog.pg_namespace n ON n.oid = p.pronamespace
		WHERE p.proname IN ('depends', 'plain', 'citext_eq', 'seg_in', 'textlen', 'plpgsql_call_handler')`)
	types := read(t, `SELECT n.nspname, y.typname, `+originColumns("n.nspname", catalogType, "y.oid")+`
		FROM pg_catalog.pg_type y JOIN pg_catalog.pg_namespace n ON n.oid = y.typnamespace
		WHERE y.typname IN ('citext', 'seg', 'int4', 'orders')`)
	schemas := read(t, `SELECT n.nspname, n.nspname, `+originColumns("n.nspname", catalogNamespace, "n.oid")+`
		FROM pg_catalog.pg_namespace n`)

	var tempSchema string
	if err := conn.QueryRow(ctx, `SELECT pg_catalog.pg_my_temp_schema()::pg_catalog.regnamespace::pg_catalog.text`).Scan(&tempSchema); err != nil {
		t.Fatal(err)
	}

	user, sys := Origin{}, Origin{IsSystem: true}
	for _, c := range []struct {
		what string
		got  map[[2]string]Origin
		key  [2]string
		want Origin
	}{
		{"a catalog table", relations, [2]string{"pg_catalog", "pg_class"}, sys},
		{"an information_schema view", relations, [2]string{"information_schema", "tables"}, sys},
		{"a user table", relations, [2]string{"app", "orders"}, user},
		{"a table added to an extension", relations, [2]string{"app", "joined"}, Origin{Extension: "seg"}},
		{"the session's temporary table", relations, [2]string{tempSchema, "scratch"}, sys},
		{"a catalog function", routines, [2]string{"pg_catalog", "textlen"}, sys},
		{"an extension's function in pg_catalog", routines, [2]string{"pg_catalog", "plpgsql_call_handler"}, Origin{IsSystem: true, Extension: "plpgsql"}},
		{"an extension's function", routines, [2]string{"ext", "citext_eq"}, Origin{Extension: "citext"}},
		{"another extension's function", routines, [2]string{"app", "seg_in"}, Origin{Extension: "seg"}},
		{"a function that only depends on an extension", routines, [2]string{"app", "depends"}, user},
		{"a user function", routines, [2]string{"app", "plain"}, user},
		{"an extension's type", types, [2]string{"ext", "citext"}, Origin{Extension: "citext"}},
		{"a catalog type", types, [2]string{"pg_catalog", "int4"}, sys},
		{"a table's row type", types, [2]string{"app", "orders"}, user},
		{"pg_catalog", schemas, [2]string{"pg_catalog", "pg_catalog"}, sys},
		{"pg_toast", schemas, [2]string{"pg_toast", "pg_toast"}, sys},
		{"the temporary schema", schemas, [2]string{tempSchema, tempSchema}, sys},
		{"information_schema", schemas, [2]string{"information_schema", "information_schema"}, sys},
		{"public", schemas, [2]string{"public", "public"}, user},
		{"an extension's schema", schemas, [2]string{"ext", "ext"}, user},
		{"a schema added to an extension", schemas, [2]string{"adopted", "adopted"}, Origin{Extension: "seg"}},
	} {
		got, ok := c.got[c.key]
		if !ok {
			t.Errorf("%s: %s.%s not read", c.what, c.key[0], c.key[1])
			continue
		}
		if got != c.want {
			t.Errorf("%s: %s.%s = %+v, want %+v", c.what, c.key[0], c.key[1], got, c.want)
		}
	}

	for key, o := range schemas {
		if IsSystemSchema(key[0]) != o.IsSystem {
			t.Errorf("IsSystemSchema(%q) = %v; the server's rule says %v", key[0], !o.IsSystem, o.IsSystem)
		}
	}

	t.Run("filter", func(t *testing.T) {
		cols := filterColumns{system: systemSchema("n.nspname"), extension: isExtensionMember(catalogClass, "c.oid")}
		for _, sysWant := range []bool{true, false} {
			for _, extWant := range []bool{true, false} {
				where, args, err := ObjectFilter{System: &sysWant, ExtensionMember: &extWant}.clause(cols, nil)
				if err != nil {
					t.Fatal(err)
				}
				rows, err := scanRows(ctx, conn, "filter relations", func(scan func(...any) error) ([2]string, error) {
					var r [2]string
					return r, scan(&r[0], &r[1])
				}, `SELECT n.nspname, c.relname FROM pg_catalog.pg_class c
					JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace WHERE true`+where, args...)
				if err != nil {
					t.Fatalf("%s: %v", where, err)
				}
				want := 0
				for _, o := range relations {
					if o.IsSystem == sysWant && o.IsExtensionMember() == extWant {
						want++
					}
				}
				if len(rows) != want {
					t.Errorf("System %v, ExtensionMember %v: %d relations, want %d", sysWant, extWant, len(rows), want)
				}
				for _, r := range rows {
					if o := relations[r]; o.IsSystem != sysWant || o.IsExtensionMember() != extWant {
						t.Errorf("System %v, ExtensionMember %v listed %s.%s, whose Origin is %+v", sysWant, extWant, r[0], r[1], o)
					}
				}
			}
		}
	})
}
