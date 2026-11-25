//go:build livedb

package pgmo

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/radix29/pgmo/pgsql"
)

// TestLiveViews reads the views and materialized views of a throwaway
// database the listings, the by-name reads and each part read must describe
// in full:
//
//   - app.v1: security_barrier and security_invoker written as "on" and
//     "1", WITH CASCADED CHECK OPTION, over app.t;
//   - app.v2: over app.v1 WITH LOCAL CHECK OPTION, a column DEFAULT, SELECT
//     granted to PUBLIC, a column grant, a comment on it and on a column;
//   - app."V1", differing from app.v1 only by case; app.ext_v, a citext
//     column, adopted by citext (an extension member);
//   - app.m1: WITH NO DATA, storage parameters on it and its TOAST table;
//   - app.m2: populated, ANALYZEd, a unique index, a grant and a comment.
//
// Each by-name read refuses the other kind and a table. The definition is
// read under two search_paths, to pin that it follows the session's. The
// listings are read again as a plain role, with each part: identical.
func TestLiveViews(t *testing.T) {
	admin, ctx, done := liveDBTimeout(t, 2*time.Minute)
	t.Cleanup(done)
	stamp := time.Now().UnixNano()
	name := func(s string) string { return fmt.Sprintf("pgmo_live_view_%d_%s", stamp, s) }
	b, plain := name("B b"), name("plain")
	db := name("db")
	q := pgsql.QuoteIdent

	var connected string
	if err := admin.QueryRow(ctx, "SELECT current_user::text").Scan(&connected); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+q(db)+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database: %v", err)
		}
		for _, r := range []string{b, plain} {
			if _, err := admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+q(r)); err != nil {
				t.Errorf("drop role %s: %v", r, err)
			}
		}
	})
	for _, stmt := range []string{"CREATE DATABASE " + q(db), "CREATE ROLE " + q(b), "CREATE ROLE " + q(plain)} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	cfg, err := pgx.ParseConfig(*liveConnString)
	if err != nil {
		t.Fatal(err)
	}
	cfg.Database = db
	conn, err := pgx.ConnectConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })
	for _, stmt := range []string{
		"CREATE SCHEMA app",
		"CREATE EXTENSION citext SCHEMA app",
		"CREATE TABLE app.t (id integer, name text, tag app.citext)",
		"INSERT INTO app.t SELECT g, 'n' || g FROM generate_series(1, 50) g",
		"CREATE VIEW app.v1 WITH (security_barrier = on, check_option = cascaded, security_invoker = 1) AS SELECT id, name FROM app.t WHERE id > 0",
		"CREATE VIEW app.v2 AS SELECT id FROM app.v1 WITH LOCAL CHECK OPTION",
		"ALTER VIEW app.v2 ALTER COLUMN id SET DEFAULT 7",
		"GRANT SELECT ON app.v2 TO PUBLIC",
		"GRANT INSERT (id) ON app.v2 TO " + q(b),
		"COMMENT ON VIEW app.v2 IS 'the second'",
		"COMMENT ON COLUMN app.v2.id IS 'its id'",
		`CREATE VIEW app."V1" AS SELECT 1 AS x`,
		"CREATE VIEW app.ext_v AS SELECT tag FROM app.t",
		"ALTER EXTENSION citext ADD VIEW app.ext_v",
		"CREATE MATERIALIZED VIEW app.m1 WITH (fillfactor = 70, toast.autovacuum_enabled = false) AS SELECT id, name FROM app.t WITH NO DATA",
		"CREATE MATERIALIZED VIEW app.m2 AS SELECT id, name FROM app.t",
		"CREATE UNIQUE INDEX m2_id ON app.m2 (id)",
		"ANALYZE app.m2",
		"GRANT SELECT ON app.m2 TO " + q(b),
		"COMMENT ON MATERIALIZED VIEW app.m2 IS 'the totals'",
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	s, err := Connect(ctx, ConnectionOptions{ConnString: *liveConnString, Database: db, ApplicationName: "pgmo - live views"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	d, err := s.DatabaseByName(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	// The user views no extension owns, by schema then byte order; the
	// extension's; and information_schema's, which are system views.
	no, yes := false, true
	users, err := d.Views(ctx, ObjectFilter{System: &no, ExtensionMember: &no})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, v := range users {
		names = append(names, v.Name)
	}
	if want := []string{"V1", "v1", "v2"}; !slices.Equal(names, want) {
		t.Fatalf("Views(user) = %q, want %q", names, want)
	}
	ext, err := d.Views(ctx, ObjectFilter{ExtensionMember: &yes})
	if err != nil || len(ext) != 1 || ext[0].Name != "ext_v" || ext[0].Origin != (Origin{Extension: "citext"}) {
		t.Errorf("Views(extension) = %+v, %v; want app.ext_v of citext", ext, err)
	}
	sys, err := d.Views(ctx, ObjectFilter{System: &yes, Name: []TextCriterion{{TextEquals, "tables"}}})
	if err != nil || len(sys) != 1 || sys[0].Schema != "information_schema" || !sys[0].IsSystem {
		t.Errorf("Views(system, tables) = %+v, %v; want information_schema.tables", sys, err)
	}
	view := func(n string) *View {
		t.Helper()
		v, err := d.ViewByName(ctx, "app", n)
		if err != nil {
			t.Fatal(err)
		}
		if i := slices.IndexFunc(users, func(u *View) bool { return u.Name == n }); i >= 0 && !reflect.DeepEqual(users[i], v) {
			t.Errorf("listing's %s =\n%+v\nby name\n%+v", n, users[i], v)
		}
		return v
	}

	v1 := view("v1")
	if v1.OID == 0 || v1.Owner != connected || v1.CheckOption != CheckOptionCascaded || !v1.SecurityBarrier || !v1.SecurityInvoker ||
		!reflect.DeepEqual(v1.Options, []ParameterSetting{{"security_barrier", "on"}, {"check_option", "cascaded"}, {"security_invoker", "1"}}) ||
		!v1.PrivilegesDefault || v1.Comment != "" || v1.Origin != (Origin{}) {
		t.Errorf("app.v1 = %+v", v1)
	}
	if !strings.Contains(v1.Definition, "FROM app.t") || !strings.Contains(v1.Definition, "(id > 0)") && !strings.Contains(v1.Definition, "(t.id > 0)") ||
		!strings.HasSuffix(v1.Definition, ";") {
		t.Errorf("app.v1 definition %q: want the query over app.t, qualified (app is not on the path)", v1.Definition)
	}
	v2 := view("v2")
	if v2.CheckOption != CheckOptionLocal || v2.SecurityBarrier || v2.SecurityInvoker || v2.PrivilegesDefault ||
		v2.Comment != "the second" || !strings.Contains(v2.Definition, "FROM app.v1") {
		t.Errorf("app.v2 = %+v", v2)
	}
	if !slices.ContainsFunc(v2.Privileges, func(g PrivilegeGrant) bool { return g.Grantee == "" && g.Privilege == PrivilegeSelect }) {
		t.Errorf("app.v2 privileges %+v: want SELECT to PUBLIC", v2.Privileges)
	}
	cols, err := v2.Columns(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(cols) != 1 || cols[0].Name != "id" || cols[0].Type != "integer" || cols[0].Default != "7" || cols[0].Comment != "its id" ||
		!reflect.DeepEqual(cols[0].Privileges, []PrivilegeGrant{{Grantee: b, Grantor: connected, Privilege: PrivilegeInsert}}) {
		t.Errorf("app.v2 columns = %s", derefAll(cols))
	}
	extCols, err := ext[0].Columns(ctx)
	if err != nil || len(extCols) != 1 || extCols[0].Type != "app.citext" {
		t.Errorf("app.ext_v columns = %v, %v; want tag app.citext", extCols, err)
	}
	if upper := view("V1"); upper.OID == view("v1").OID || !strings.Contains(upper.Definition, "1 AS x") {
		t.Errorf(`app."V1" = %+v: want its own view`, upper)
	}

	// The definition follows the session's search_path.
	err = s.InTransaction(ctx, func(ctx context.Context) error {
		if _, err := s.on(ctx).Exec(ctx, "SET LOCAL search_path = app, pg_catalog"); err != nil {
			return err
		}
		v, err := d.ViewByName(ctx, "app", "v1")
		if err == nil && (!strings.Contains(v.Definition, "FROM t") || strings.Contains(v.Definition, "app.t")) {
			err = fmt.Errorf("definition %q: want FROM t, app being on the path", v.Definition)
		}
		return err
	})
	if err != nil {
		t.Error(err)
	}

	mats, err := d.MaterializedViews(ctx, ObjectFilter{Schema: []TextCriterion{{TextEquals, "app"}}})
	if err != nil {
		t.Fatal(err)
	}
	if len(mats) != 2 || mats[0].Name != "m1" || mats[1].Name != "m2" {
		t.Fatalf("MaterializedViews(app) = %+v, want m1, m2", mats)
	}
	mat := func(n string) *MaterializedView {
		t.Helper()
		m, err := d.MaterializedViewByName(ctx, "app", n)
		if err != nil {
			t.Fatal(err)
		}
		if i := slices.IndexFunc(mats, func(u *MaterializedView) bool { return u.Name == n }); !reflect.DeepEqual(mats[i], m) {
			t.Errorf("listing's %s =\n%+v\nby name\n%+v", n, mats[i], m)
		}
		return m
	}
	m1 := mat("m1")
	if m1.Populated || m1.RowEstimate != -1 || m1.Tablespace != "" || !m1.PrivilegesDefault ||
		!reflect.DeepEqual(m1.Options, []ParameterSetting{{"fillfactor", "70"}, {"toast.autovacuum_enabled", "false"}}) {
		t.Errorf("app.m1 = %+v: want unpopulated, never analyzed, with its own and its TOAST table's parameters", m1)
	}
	m2 := mat("m2")
	if !m2.Populated || m2.RowEstimate != 50 || m2.Options != nil || m2.PrivilegesDefault || m2.Comment != "the totals" ||
		!strings.Contains(m2.Definition, "FROM app.t") ||
		!slices.ContainsFunc(m2.Privileges, func(g PrivilegeGrant) bool { return g.Grantee == b && g.Privilege == PrivilegeSelect }) {
		t.Errorf("app.m2 = %+v: want populated, 50 rows, SELECT granted, commented", m2)
	}
	mcols, err := m2.Columns(ctx)
	if err != nil || len(mcols) != 2 || mcols[0].Name != "id" || mcols[1].Type != "text" {
		t.Errorf("app.m2 columns = %v, %v", mcols, err)
	}
	xs, err := m2.Indexes(ctx)
	if err != nil || len(xs) != 1 || xs[0].Name != "m2_id" || !xs[0].Unique || !xs[0].Valid ||
		!strings.Contains(xs[0].Definition, "ON app.m2 USING btree (id)") {
		t.Errorf("app.m2 indexes = %s, %v; want the unique m2_id", derefAll(xs), err)
	}
	if xs, err := m1.Indexes(ctx); err != nil || xs != nil {
		t.Errorf("app.m1 indexes = %v, %v; want none", xs, err)
	}
	sz, err := m2.Size(ctx)
	if err != nil || sz.Data <= 0 || sz.Indexes <= 0 || sz.Total != sz.Data+sz.Indexes {
		t.Errorf("app.m2 size = %+v, %v", sz, err)
	}

	// Each by-name read finds its own kind alone.
	for _, c := range []struct {
		what string
		read func() error
	}{
		{"ViewByName(a materialized view)", func() error { _, err := d.ViewByName(ctx, "app", "m1"); return err }},
		{"ViewByName(a table)", func() error { _, err := d.ViewByName(ctx, "app", "t"); return err }},
		{"ViewByName(V1 folded)", func() error { _, err := d.ViewByName(ctx, "APP", "v1"); return err }},
		{"MaterializedViewByName(a view)", func() error { _, err := d.MaterializedViewByName(ctx, "app", "v1"); return err }},
		{"MaterializedViewByName(a table)", func() error { _, err := d.MaterializedViewByName(ctx, "app", "t"); return err }},
		{"TableByName(a view)", func() error { _, err := d.TableByName(ctx, "app", "v1"); return err }},
		{"TableByName(a materialized view)", func() error { _, err := d.TableByName(ctx, "app", "m2"); return err }},
	} {
		if err := c.read(); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", c.what, err)
		}
	}
	vref, mref := d.ViewRef("app", "v1"), d.MaterializedViewRef("app", "m2")
	for _, read := range []func() error{
		func() error { _, err := vref.Columns(ctx); return err },
		func() error { _, err := mref.Columns(ctx); return err },
		func() error { _, err := mref.Indexes(ctx); return err },
		func() error { _, err := mref.Size(ctx); return err },
	} {
		if err := read(); !errors.Is(err, ErrHandleNotLoaded) {
			t.Errorf("Ref read: err = %v, want ErrHandleNotLoaded", err)
		}
	}

	// As a plain role: the same listings and parts.
	dump := func(ctx context.Context) (string, error) {
		app := ObjectFilter{Schema: []TextCriterion{{TextEquals, "app"}}}
		vs, err := d.Views(ctx, app)
		if err != nil {
			return "", err
		}
		var sb strings.Builder
		for _, v := range vs {
			c := *v
			c.db = nil
			cols, err := v.Columns(ctx)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&sb, "%+v\n%s\n", c, derefAll(cols))
		}
		ms, err := d.MaterializedViews(ctx, app)
		if err != nil {
			return "", err
		}
		for _, m := range ms {
			c := *m
			c.db = nil
			cols, err := m.Columns(ctx)
			if err != nil {
				return "", err
			}
			idx, err := m.Indexes(ctx)
			if err != nil {
				return "", err
			}
			fmt.Fprintf(&sb, "%+v\n%s\n%s\n", c, derefAll(cols), derefAll(idx))
		}
		return sb.String(), nil
	}
	all, err := dump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var asPlain string
	err = s.InTransaction(ctx, func(ctx context.Context) error {
		if _, err := s.on(ctx).Exec(ctx, "SET LOCAL ROLE "+q(plain)); err != nil {
			return err
		}
		var err error
		asPlain, err = dump(ctx)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if asPlain != all {
		t.Errorf("as a plain role the views differ:\n%s\nwant\n%s", asPlain, all)
	}
}
