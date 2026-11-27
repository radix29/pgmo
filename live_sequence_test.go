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

// TestLiveSequences reads the sequences of a throwaway database the
// listing, the by-name read and the last value must describe in full:
//
//   - app.s1: every default (bigint, never called), USAGE granted WITH GRANT
//     OPTION and SELECT to PUBLIC, a comment;
//   - app.s2: smallint, descending by 2 from -1 down to -300, CACHE 5,
//     CYCLE, called once (its cache stores the fifth value), USAGE granted
//     to the plain role;
//   - app.t's serial id (OWNED BY, deptype 'a') and identity i (deptype
//     'i'); app.s3 OWNED BY app.t.x by hand;
//   - app.s4 UNLOGGED; app."S1", differing from app.s1 only by case;
//     app.ext_s, adopted by citext (an extension member);
//   - a temporary sequence of another session.
//
// Every sequence the listing returns matches pg_sequences' row of it. The
// last value is read through setval and RESTART; refused for another
// session's temporary sequence, a dropped one and a Ref. The listing is read
// again as a plain role: identical; its last values follow its privileges.
func TestLiveSequences(t *testing.T) {
	admin, ctx, done := liveDBTimeout(t, 2*time.Minute)
	t.Cleanup(done)
	stamp := time.Now().UnixNano()
	name := func(s string) string { return fmt.Sprintf("pgmo_live_seq_%d_%s", stamp, s) }
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
		"CREATE SEQUENCE app.s1",
		"GRANT USAGE ON SEQUENCE app.s1 TO " + q(b) + " WITH GRANT OPTION",
		"GRANT SELECT ON SEQUENCE app.s1 TO PUBLIC",
		"COMMENT ON SEQUENCE app.s1 IS 'the first'",
		"CREATE SEQUENCE app.s2 AS smallint INCREMENT BY -2 MINVALUE -300 MAXVALUE -1 CACHE 5 CYCLE",
		"SELECT pg_catalog.nextval('app.s2')",
		"GRANT USAGE ON SEQUENCE app.s2 TO " + q(plain),
		"CREATE TABLE app.t (id serial, i integer GENERATED ALWAYS AS IDENTITY, x bigint)",
		"CREATE SEQUENCE app.s3 OWNED BY app.t.x",
		"CREATE UNLOGGED SEQUENCE app.s4",
		`CREATE SEQUENCE app."S1" START 10`,
		"CREATE SEQUENCE app.ext_s",
		"ALTER EXTENSION citext ADD SEQUENCE app.ext_s",
		// conn's own: another session's to pgmo's pool.
		"CREATE TEMPORARY SEQUENCE tmp_s",
		"SELECT pg_catalog.nextval('tmp_s')",
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	s, err := Connect(ctx, ConnectionOptions{ConnString: *liveConnString, Database: db, ApplicationName: "pgmo - live sequences"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	d, err := s.DatabaseByName(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	// The user sequences no extension owns, by schema then byte order; the
	// extension's; and the temporary one, a system object.
	no, yes := false, true
	users, err := d.Sequences(ctx, ObjectFilter{System: &no, ExtensionMember: &no})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, u := range users {
		names = append(names, u.Name)
	}
	if want := []string{"S1", "s1", "s2", "s3", "s4", "t_i_seq", "t_id_seq"}; !slices.Equal(names, want) {
		t.Fatalf("Sequences(user) = %q, want %q", names, want)
	}
	ext, err := d.Sequences(ctx, ObjectFilter{ExtensionMember: &yes})
	if err != nil || len(ext) != 1 || ext[0].Name != "ext_s" || ext[0].Origin != (Origin{Extension: "citext"}) {
		t.Errorf("Sequences(extension) = %+v, %v; want app.ext_s of citext", ext, err)
	}
	sys, err := d.Sequences(ctx, ObjectFilter{System: &yes})
	if err != nil || len(sys) != 1 || sys[0].Name != "tmp_s" || !strings.HasPrefix(sys[0].Schema, "pg_temp_") ||
		!sys[0].IsSystem || sys[0].Persistence != PersistenceTemporary {
		t.Fatalf("Sequences(system) = %+v, %v; want the temporary tmp_s", sys, err)
	}
	seq := func(n string) *Sequence {
		t.Helper()
		got, err := d.SequenceByName(ctx, "app", n)
		if err != nil {
			t.Fatal(err)
		}
		if i := slices.IndexFunc(users, func(u *Sequence) bool { return u.Name == n }); i >= 0 && !reflect.DeepEqual(users[i], got) {
			t.Errorf("listing's %s =\n%+v\nby name\n%+v", n, users[i], got)
		}
		return got
	}
	lastValue := func(sq *Sequence, wantValue int64, wantCalled bool) {
		t.Helper()
		v, called, err := sq.LastValue(ctx)
		if err != nil || v != wantValue || called != wantCalled {
			t.Errorf("%s.LastValue = %d, %v, %v; want %d, %v", sq.QualifiedName(), v, called, err, wantValue, wantCalled)
		}
	}
	const maxBigint = 1<<63 - 1

	s1 := seq("s1")
	if s1.OID == 0 || s1.Owner != connected || s1.Persistence != PersistencePermanent || s1.DataType != "bigint" ||
		s1.Start != 1 || s1.Increment != 1 || s1.MinValue != 1 || s1.MaxValue != maxBigint || s1.Cache != 1 || s1.Cycle ||
		s1.IsOwned() || s1.OwnedByColumn != "" || s1.Identity || s1.PrivilegesDefault || s1.Comment != "the first" ||
		s1.Origin != (Origin{}) {
		t.Errorf("app.s1 = %+v", s1)
	}
	for _, g := range []PrivilegeGrant{
		{Grantee: b, Grantor: connected, Privilege: PrivilegeUsage, Grantable: true},
		{Grantee: "", Grantor: connected, Privilege: PrivilegeSelect},
	} {
		if !slices.Contains(s1.Privileges, g) {
			t.Errorf("app.s1 privileges %+v: want %+v", s1.Privileges, g)
		}
	}
	lastValue(s1, 0, false)

	s2 := seq("s2")
	if s2.DataType != "smallint" || s2.Start != -1 || s2.Increment != -2 || s2.MinValue != -300 || s2.MaxValue != -1 ||
		s2.Cache != 5 || !s2.Cycle || s2.IsOwned() || s2.Comment != "" {
		t.Errorf("app.s2 = %+v", s2)
	}
	lastValue(s2, -9, true)
	if _, err := conn.Exec(ctx, "SELECT pg_catalog.setval('app.s2', -101, false)"); err != nil {
		t.Fatal(err)
	}
	lastValue(s2, 0, false)
	if _, err := conn.Exec(ctx, "SELECT pg_catalog.setval('app.s2', -101)"); err != nil {
		t.Fatal(err)
	}
	lastValue(s2, -101, true)
	if _, err := conn.Exec(ctx, "ALTER SEQUENCE app.s2 RESTART"); err != nil {
		t.Fatal(err)
	}
	lastValue(s2, 0, false)

	// The owning columns, checked against the server's own answer.
	for _, c := range []struct {
		seq, column string
		identity    bool
	}{{"t_id_seq", "id", false}, {"t_i_seq", "i", true}, {"s3", "x", false}} {
		o := seq(c.seq)
		if o.OwnedBy != (ObjectName{"app", "t"}) || o.OwnedByColumn != c.column || o.Identity != c.identity || !o.IsOwned() {
			t.Errorf("app.%s: owned by %+v.%q, identity %v; want app.t.%s, %v", c.seq, o.OwnedBy, o.OwnedByColumn, o.Identity, c.column, c.identity)
		}
		var serial string
		if err := conn.QueryRow(ctx, "SELECT pg_catalog.pg_get_serial_sequence('app.t', $1)", c.column).Scan(&serial); err != nil ||
			serial != o.QualifiedName() {
			t.Errorf("pg_get_serial_sequence(app.t, %s) = %q, %v; want %s", c.column, serial, err, o.QualifiedName())
		}
	}
	// A never-set ACL reads as a sequence's default, not a table's: the
	// owner's SELECT, UPDATE and USAGE.
	if s3 := seq("s3"); !s3.PrivilegesDefault || !reflect.DeepEqual(s3.Privileges, []PrivilegeGrant{
		{Grantee: connected, Grantor: connected, Privilege: PrivilegeSelect},
		{Grantee: connected, Grantor: connected, Privilege: PrivilegeUpdate},
		{Grantee: connected, Grantor: connected, Privilege: PrivilegeUsage},
	}) {
		t.Errorf("app.s3 privileges %+v (default %v): want the owner's SELECT, UPDATE, USAGE", s3.Privileges, s3.PrivilegesDefault)
	}
	if t3 := seq("t_i_seq"); t3.DataType != "integer" || t3.MaxValue != 1<<31-1 {
		t.Errorf("app.t_i_seq = %+v: want the identity's integer", t3)
	}
	if s4 := seq("s4"); s4.Persistence != PersistenceUnlogged {
		t.Errorf("app.s4 = %+v: want unlogged", s4)
	}
	if upper := seq("S1"); upper.OID == s1.OID || upper.Start != 10 {
		t.Errorf(`app."S1" = %+v: want its own sequence`, upper)
	}

	// Every sequence pg_sequences shows read as the view reads it — through
	// conn, whose own the temporary one is (the view leaves out only
	// another session's).
	all, err := d.Sequences(ctx, ObjectFilter{})
	if err != nil {
		t.Fatal(err)
	}
	type viewRow struct {
		schema, name, owner, typ  string
		start, min, max, inc, csz int64
		cycle                     bool
	}
	rows, err := conn.Query(ctx, `SELECT schemaname::text, sequencename::text, sequenceowner::text, data_type::text,
		start_value, min_value, max_value, increment_by, cache_size, cycle FROM pg_catalog.pg_sequences`)
	if err != nil {
		t.Fatal(err)
	}
	view, err := pgx.CollectRows(rows, func(r pgx.CollectableRow) (viewRow, error) {
		var v viewRow
		return v, r.Scan(&v.schema, &v.name, &v.owner, &v.typ, &v.start, &v.min, &v.max, &v.inc, &v.csz, &v.cycle)
	})
	if err != nil {
		t.Fatal(err)
	}
	var listed []viewRow
	for _, a := range all {
		listed = append(listed, viewRow{a.Schema, a.Name, a.Owner, a.DataType, a.Start, a.MinValue, a.MaxValue, a.Increment, a.Cache, a.Cycle})
	}
	key := func(a, b viewRow) int { return strings.Compare(a.schema+"."+a.name, b.schema+"."+b.name) }
	slices.SortFunc(view, key)
	slices.SortFunc(listed, key)
	if len(listed) != 9 || !slices.Equal(view, listed) {
		t.Errorf("Sequences =\n%+v\npg_sequences\n%+v", listed, view)
	}

	// Each refusal.
	if _, _, err := sys[0].LastValue(ctx); !errors.Is(err, ErrUnsupported) || !strings.Contains(err.Error(), "another session's temporary") {
		t.Errorf("tmp_s.LastValue: err = %v, want ErrUnsupported", err)
	}
	for _, c := range []struct {
		what string
		read func() error
	}{
		{"SequenceByName(a table)", func() error { _, err := d.SequenceByName(ctx, "app", "t"); return err }},
		{"SequenceByName(s1 folded)", func() error { _, err := d.SequenceByName(ctx, "APP", "s1"); return err }},
		{"TableByName(a sequence)", func() error { _, err := d.TableByName(ctx, "app", "s1"); return err }},
		{"ViewByName(a sequence)", func() error { _, err := d.ViewByName(ctx, "app", "s1"); return err }},
	} {
		if err := c.read(); !errors.Is(err, ErrNotFound) {
			t.Errorf("%s: err = %v, want ErrNotFound", c.what, err)
		}
	}
	if _, _, err := d.SequenceRef("app", "s1").LastValue(ctx); !errors.Is(err, ErrHandleNotLoaded) {
		t.Errorf("Ref LastValue: err = %v, want ErrHandleNotLoaded", err)
	}
	s4 := seq("s4")
	if _, err := conn.Exec(ctx, "DROP SEQUENCE app.s4"); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s4.LastValue(ctx); !errors.Is(err, ErrNotFound) {
		t.Errorf("dropped LastValue: err = %v, want ErrNotFound", err)
	}
	if _, err := conn.Exec(ctx, "SELECT pg_catalog.nextval('app.s2')"); err != nil {
		t.Fatal(err)
	}

	// As a plain role: the same listing; the last value of s2 (USAGE), of
	// s1 (SELECT to PUBLIC), and none of s3 (no privilege).
	dump := func(ctx context.Context) (string, error) {
		qs, err := d.Sequences(ctx, ObjectFilter{Schema: []TextCriterion{{TextEquals, "app"}}})
		if err != nil {
			return "", err
		}
		var sb strings.Builder
		for _, sq := range qs {
			c := *sq
			c.db = nil
			fmt.Fprintf(&sb, "%+v\n", c)
		}
		return sb.String(), nil
	}
	want, err := dump(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var asPlain string
	err = s.InTransaction(ctx, func(ctx context.Context) error {
		if _, err := s.on(ctx).Exec(ctx, "SET LOCAL ROLE "+q(plain)); err != nil {
			return err
		}
		var err error
		if asPlain, err = dump(ctx); err != nil {
			return err
		}
		if v, called, err := s2.LastValue(ctx); err != nil || v != -9 || !called {
			return fmt.Errorf("app.s2 as the plain role: %d, %v, %v; want -9, the cache's fifth value after RESTART and nextval", v, called, err)
		}
		if v, called, err := s1.LastValue(ctx); err != nil || v != 0 || called {
			return fmt.Errorf("app.s1 as the plain role: %d, %v, %v; want none, never called", v, called, err)
		}
		_, _, err = seq("s3").LastValue(ctx)
		if !errors.Is(err, ErrDefinitionHidden) || !errors.Is(err, ErrUnsupported) {
			return fmt.Errorf("app.s3 as the plain role: err = %v, want ErrDefinitionHidden", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if asPlain != want {
		t.Errorf("as a plain role the sequences differ:\n%s\nwant\n%s", asPlain, want)
	}
}
