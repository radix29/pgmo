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

// TestLiveTablespaces reads the cluster's tablespaces with one of its own:
// an in-place tablespace (allow_in_place_tablespaces, a superuser's
// developer setting, so no directory is needed on the server) with two
// options, a comment, CREATE granted to one plain role, and a table in it.
//
// The listing matches pg_tablespace; the new one's location is its path
// under the data directory; the filters narrow it; the by-name read is the
// listing's. A plain role reads the same listing; the size of pg_global is
// refused it (42501), the size of the tablespace it holds CREATE on is not,
// and the size of pg_default — its session's database's — is answered to
// any role.
func TestLiveTablespaces(t *testing.T) {
	admin, ctx, done := liveDBTimeout(t, 2*time.Minute)
	t.Cleanup(done)
	stamp := time.Now().UnixNano()
	ts := fmt.Sprintf("pgmo live ts %d", stamp)
	plain, granted, db := fmt.Sprintf("pgmo_live_ts_plain_%d", stamp), fmt.Sprintf("pgmo_live_ts_granted_%d", stamp),
		fmt.Sprintf("pgmo_live_ts_db_%d", stamp)
	q := pgsql.QuoteIdent

	// Registered first, so they run last: the database (and its table)
	// before the tablespace, the tablespace before the roles holding a
	// grant on it.
	t.Cleanup(func() {
		for _, stmt := range []string{"DROP DATABASE IF EXISTS " + q(db) + " WITH (FORCE)", "DROP TABLESPACE IF EXISTS " + q(ts),
			"DROP ROLE IF EXISTS " + q(plain), "DROP ROLE IF EXISTS " + q(granted)} {
			if _, err := admin.Exec(context.Background(), stmt); err != nil {
				t.Errorf("%s: %v", stmt, err)
			}
		}
	})
	conn, err := admin.Acquire(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"CREATE ROLE " + q(plain), "CREATE ROLE " + q(granted), "CREATE DATABASE " + q(db),
		"SET allow_in_place_tablespaces = on",
		"CREATE TABLESPACE " + q(ts) + " LOCATION ''",
		"RESET allow_in_place_tablespaces",
		"ALTER TABLESPACE " + q(ts) + " SET (random_page_cost = 1.1, seq_page_cost = 1)",
		"COMMENT ON TABLESPACE " + q(ts) + " IS 'live'",
		"GRANT CREATE ON TABLESPACE " + q(ts) + " TO " + q(granted),
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			conn.Release()
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	conn.Release()
	scratch := liveScratchConn(t, ctx, db)
	if _, err := scratch.Exec(ctx, "CREATE TABLE t (id integer) TABLESPACE "+q(ts)+"; INSERT INTO t VALUES (1)"); err != nil {
		t.Fatal(err)
	}

	s, err := Connect(ctx, ConnectionOptions{ConnString: *liveConnString, ApplicationName: "pgmo - live tablespaces"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	// What the catalog says, read independently.
	var oid OID
	var owner string
	if err := admin.QueryRow(ctx, "SELECT oid, current_user::text FROM pg_catalog.pg_tablespace WHERE spcname = $1", ts).
		Scan(&oid, &owner); err != nil {
		t.Fatal(err)
	}
	rows, err := admin.Query(ctx, "SELECT spcname::text FROM pg_catalog.pg_tablespace ORDER BY spcname")
	if err != nil {
		t.Fatal(err)
	}
	names, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}

	dump := func(xs []*Tablespace) string {
		var sb strings.Builder
		for _, x := range xs {
			c := *x
			c.server = nil
			fmt.Fprintf(&sb, "%+v\n", c)
		}
		return sb.String()
	}
	all, err := s.Tablespaces(ctx, ObjectFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var got []string
	for _, x := range all {
		got = append(got, x.Name)
		if x.Server() != s {
			t.Errorf("%s: Server() = %v", x.Name, x.Server())
		}
	}
	if !slices.Equal(got, names) {
		t.Fatalf("Tablespaces = %q, want pg_tablespace's %q", got, names)
	}
	mine := all[slices.Index(got, ts)]
	want := Tablespace{OID: oid, Name: ts, Owner: owner, Location: fmt.Sprintf("pg_tblspc/%d", oid),
		Options: []ParameterSetting{{"random_page_cost", "1.1"}, {"seq_page_cost", "1"}},
		Privileges: []PrivilegeGrant{{Grantee: owner, Grantor: owner, Privilege: PrivilegeCreate},
			{Grantee: granted, Grantor: owner, Privilege: PrivilegeCreate}},
		Comment: "live"}
	if g := *mine; func() bool { g.server = nil; return !reflect.DeepEqual(g, want) }() {
		t.Errorf("%s =\n%s\nwant\n%+v", ts, dump([]*Tablespace{mine}), want)
	}
	for _, name := range []string{"pg_default", "pg_global"} {
		x := all[slices.Index(got, name)]
		if !x.IsSystem || x.Location != "" || x.Extension != "" {
			t.Errorf("%s: IsSystem %v, Location %q, Extension %q; want a system one in the data directory", name, x.IsSystem, x.Location, x.Extension)
		}
	}

	yes, no := true, false
	for _, c := range []struct {
		f    ObjectFilter
		want []string
	}{
		{ObjectFilter{Name: []TextCriterion{{TextEquals, q(ts)}}}, []string{ts}},
		{ObjectFilter{Name: []TextCriterion{{TextEquals, strings.ToUpper(q(ts))}}}, nil},
		{ObjectFilter{System: &yes}, []string{"pg_default", "pg_global"}},
		{ObjectFilter{System: &no, Name: []TextCriterion{{TextContains, "PGMO live TS"}}}, []string{ts}}, // folded
		{ObjectFilter{System: &no, Name: []TextCriterion{{TextContains, `"PGMO live ts"`}}}, nil},        // quoted: exact
		{ObjectFilter{ExtensionMember: &yes}, nil},
		{ObjectFilter{ExtensionMember: &no, System: &yes, Schema: []TextCriterion{{TextEquals, "nothing"}}}, []string{"pg_default", "pg_global"}},
	} {
		xs, err := s.Tablespaces(ctx, c.f)
		var got []string
		for _, x := range xs {
			got = append(got, x.Name)
		}
		if err != nil || !slices.Equal(got, c.want) {
			t.Errorf("Tablespaces(%+v) = %q, %v; want %q", c.f, got, err, c.want)
		}
	}

	byName, err := s.TablespaceByName(ctx, ts)
	if err != nil || dump([]*Tablespace{byName}) != dump([]*Tablespace{mine}) {
		t.Errorf("TablespaceByName = %+v, %v; want the listing's", byName, err)
	}
	if _, err := s.TablespaceByName(ctx, strings.ToUpper(ts)); !errors.Is(err, ErrNotFound) {
		t.Errorf("TablespaceByName(upper case): err = %v, want ErrNotFound (exact names)", err)
	}
	if n, err := s.TablespaceRef(ts).Size(ctx); err != nil || n <= 0 {
		t.Errorf("Size = %d, %v; want the table's bytes", n, err)
	}
	if _, err := s.TablespaceRef("pgmo no such ts").Size(ctx); !errors.Is(err, ErrNotFound) {
		t.Errorf("Size of a missing one: err = %v, want ErrNotFound", err)
	}

	// As plain roles: the same listing; sizes as the server's rule says.
	asRole := func(role string, fn func(ctx context.Context) error) error {
		return s.InTransaction(ctx, func(ctx context.Context) error {
			if _, err := s.on(ctx).Exec(ctx, "SET LOCAL ROLE "+q(role)); err != nil {
				return err
			}
			return fn(ctx)
		})
	}
	var asPlain []*Tablespace
	if err := asRole(plain, func(ctx context.Context) error {
		var err error
		asPlain, err = s.Tablespaces(ctx, ObjectFilter{})
		if err != nil {
			return err
		}
		if n, err := s.TablespaceRef("pg_default").Size(ctx); err != nil || n <= 0 {
			return fmt.Errorf("pg_default's size = %d, %v; want it answered to any role", n, err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if dump(asPlain) != dump(all) {
		t.Errorf("as a plain role the tablespaces differ:\n%s\nwant\n%s", dump(asPlain), dump(all))
	}
	for _, c := range []struct {
		role, ts string
		denied   bool
	}{{plain, "pg_global", true}, {plain, ts, true}, {granted, ts, false}} {
		var n int64
		err := asRole(c.role, func(ctx context.Context) error {
			var err error
			n, err = s.TablespaceRef(c.ts).Size(ctx)
			return err
		})
		if c.denied && !IsPermissionDenied(err) || !c.denied && (err != nil || n <= 0) {
			t.Errorf("Size(%s) as %s = %d, %v; want denied %v", c.ts, c.role, n, err, c.denied)
		}
	}
}
