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

// TestLiveForeignData reads the SQL/MED objects of a throwaway database:
//
//   - postgres_fdw's wrapper (an extension member) and "W w" over its
//     handler with no validator, an option holding '=', a comment, and
//     USAGE granted to the mapped role;
//   - "S s" on postgres_fdw with a type, version, options and a comment,
//     USAGE granted to the mapped role; "s s" (a case-only twin) and s2 on
//     "W w", s2 made postgres_fdw's member and owned by a non-superuser;
//   - user mappings: PUBLIC's and the mapped role's on "S s" with
//     passwords, the plain role's with none; PUBLIC's and the plain role's
//     on s2 with options;
//   - foreign tables in "A a": "F t" with table and column options, a
//     CHECK, NOT NULL, a NOT ENFORCED CHECK on 18+, a trigger, a comment and
//     a grant; a partition of a
//     partitioned table; one inheriting from a table.
//
// User mappings are read as the connected role, the plain role, the mapped
// role and s2's owner: every option shown matches pg_user_mappings read as
// the same role, and OptionsHidden is set exactly where the view withholds
// options the catalog holds. Everything else is read again as the plain
// role (identical: neither catalog hides a row).
func TestLiveForeignData(t *testing.T) {
	admin, ctx, done := liveDBTimeout(t, 2*time.Minute)
	t.Cleanup(done)
	stamp := time.Now().UnixNano()
	name := func(s string) string { return fmt.Sprintf("pgmo_live_fdw_%d_%s", stamp, s) }
	db, plain, mapped, srvown := name("db"), name("plain"), name("Mapped"), name("srvown")
	q := pgsql.QuoteIdent

	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+q(db)+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database: %v", err)
		}
		for _, r := range []string{plain, mapped, srvown} {
			if _, err := admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+q(r)); err != nil {
				t.Errorf("drop role: %v", err)
			}
		}
	})
	for _, stmt := range []string{"CREATE DATABASE " + q(db), "CREATE ROLE " + q(plain), "CREATE ROLE " + q(mapped), "CREATE ROLE " + q(srvown)} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	conn := liveScratchConn(t, ctx, db)
	for _, stmt := range []string{
		`CREATE EXTENSION postgres_fdw`,
		`CREATE FOREIGN DATA WRAPPER "W w" HANDLER postgres_fdw_handler OPTIONS (debug 'a=b', level '1')`,
		`COMMENT ON FOREIGN DATA WRAPPER "W w" IS 'live'`,
		`GRANT USAGE ON FOREIGN DATA WRAPPER "W w" TO ` + q(mapped) + `, ` + q(srvown),
		`CREATE SERVER "S s" TYPE 'pg' VERSION '18' FOREIGN DATA WRAPPER postgres_fdw OPTIONS (host 'remote.invalid', dbname 'd')`,
		`COMMENT ON SERVER "S s" IS 'live'`,
		`GRANT USAGE ON FOREIGN SERVER "S s" TO ` + q(mapped),
		`CREATE SERVER "s s" FOREIGN DATA WRAPPER "W w"`,
		`CREATE SERVER s2 FOREIGN DATA WRAPPER "W w"`,
		`ALTER EXTENSION postgres_fdw ADD SERVER s2`,
		`ALTER SERVER s2 OWNER TO ` + q(srvown),
		`CREATE USER MAPPING FOR PUBLIC SERVER "S s" OPTIONS (user 'pub', password 'secret=1')`,
		`CREATE USER MAPPING FOR ` + q(mapped) + ` SERVER "S s" OPTIONS (user 'm', password 'secret2')`,
		`CREATE USER MAPPING FOR ` + q(plain) + ` SERVER "S s"`,
		`CREATE USER MAPPING FOR PUBLIC SERVER s2 OPTIONS (x 'pub2')`,
		`CREATE USER MAPPING FOR ` + q(plain) + ` SERVER s2 OPTIONS (x 'p')`,
		`CREATE SCHEMA "A a"`,
		`CREATE FOREIGN TABLE "A a"."F t" (id integer OPTIONS (column_name 'ID') NOT NULL, note text CHECK (note <> ''))
		   SERVER "S s" OPTIONS (schema_name 'r', table_name 't')`,
		`COMMENT ON FOREIGN TABLE "A a"."F t" IS 'live'`,
		`GRANT SELECT ON "A a"."F t" TO ` + q(plain),
		`CREATE FUNCTION "A a".touch() RETURNS trigger LANGUAGE plpgsql AS 'BEGIN RETURN NEW; END'`,
		`CREATE TRIGGER ft_touch BEFORE INSERT ON "A a"."F t" FOR EACH ROW EXECUTE FUNCTION "A a".touch()`,
		`CREATE TABLE "A a".parted (id integer, k integer) PARTITION BY LIST (k)`,
		`CREATE FOREIGN TABLE "A a".fp PARTITION OF "A a".parted FOR VALUES IN (1) SERVER "s s"`,
		`CREATE TABLE "A a".base (id integer)`,
		`CREATE FOREIGN TABLE "A a".kid () INHERITS ("A a".base) SERVER s2`,
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	var version int
	if err := conn.QueryRow(ctx, "SELECT current_setting('server_version_num')::integer").Scan(&version); err != nil {
		t.Fatal(err)
	}
	if version >= 180000 { // NOT ENFORCED, 18+
		if _, err := conn.Exec(ctx, `ALTER FOREIGN TABLE "A a"."F t" ADD CONSTRAINT loose CHECK (id > 0) NOT ENFORCED`); err != nil {
			t.Fatal(err)
		}
	}
	s, err := Connect(ctx, ConnectionOptions{ConnString: *liveConnString, Database: db, ApplicationName: "pgmo - live foreign data"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	d, err := s.DatabaseByName(ctx, db)
	if err != nil {
		t.Fatal(err)
	}
	var owner string
	if err := conn.QueryRow(ctx, "SELECT current_user::text").Scan(&owner); err != nil {
		t.Fatal(err)
	}
	// as runs fn in a transaction as role, "" for the connected one.
	as := func(role string, fn func(ctx context.Context) error) {
		t.Helper()
		if err := s.InTransaction(ctx, func(ctx context.Context) error {
			if role != "" {
				if _, err := s.on(ctx).Exec(ctx, "SET LOCAL ROLE "+q(role)); err != nil {
					return err
				}
			}
			return fn(ctx)
		}); err != nil {
			t.Fatal(err)
		}
	}
	usage := func(grantee, grantor string) PrivilegeGrant {
		return PrivilegeGrant{Grantee: grantee, Grantor: grantor, Privilege: PrivilegeUsage}
	}
	set := func(kv ...string) []ParameterSetting {
		var out []ParameterSetting
		for i := 0; i < len(kv); i += 2 {
			out = append(out, ParameterSetting{kv[i], kv[i+1]})
		}
		return out
	}

	// Wrappers.
	wantWrappers := []ForeignDataWrapper{
		{Name: "W w", Owner: owner, Handler: ObjectName{"public", "postgres_fdw_handler"},
			Options:    set("debug", "a=b", "level", "1"),
			Privileges: []PrivilegeGrant{usage(owner, owner), usage(mapped, owner), usage(srvown, owner)}, Comment: "live"},
		{Origin: Origin{Extension: "postgres_fdw"}, Name: "postgres_fdw", Owner: owner,
			Handler: ObjectName{"public", "postgres_fdw_handler"}, Validator: ObjectName{"public", "postgres_fdw_validator"},
			Privileges: []PrivilegeGrant{usage(owner, owner)}, PrivilegesDefault: true},
	}
	checkWrappers := func(what string, got []*ForeignDataWrapper, want []ForeignDataWrapper) {
		t.Helper()
		var g []ForeignDataWrapper
		for _, w := range got {
			c := *w
			if c.db != d || c.OID == 0 {
				t.Errorf("%s: %s: Database %v, OID %d", what, c.Name, c.db, c.OID)
			}
			c.db, c.OID = nil, 0
			g = append(g, c)
		}
		if !reflect.DeepEqual(g, want) {
			t.Errorf("%s =\n%+v\nwant\n%+v", what, g, want)
		}
	}
	for _, role := range []string{"", plain} {
		as(role, func(ctx context.Context) error {
			ws, err := d.ForeignDataWrappers(ctx, ObjectFilter{})
			if err != nil {
				return err
			}
			checkWrappers("ForeignDataWrappers as "+role, ws, wantWrappers)
			ws, err = d.ForeignDataWrappers(ctx, ObjectFilter{ExtensionMember: new(bool)})
			if err != nil {
				return err
			}
			checkWrappers("ForeignDataWrappers, no member", ws, wantWrappers[:1])
			w, err := d.ForeignDataWrapperByName(ctx, "postgres_fdw")
			if err != nil {
				return err
			}
			checkWrappers("ForeignDataWrapperByName", []*ForeignDataWrapper{w}, wantWrappers[1:])
			if _, err := d.ForeignDataWrapperByName(ctx, "w w"); !errors.Is(err, ErrNotFound) {
				t.Errorf(`ForeignDataWrapperByName("w w"): err = %v, want ErrNotFound — names match exactly`, err)
			}
			return nil
		})
	}
	var catalogWrappers []string
	rows, err := conn.Query(ctx, "SELECT fdwname::text FROM pg_catalog.pg_foreign_data_wrapper ORDER BY fdwname")
	if err != nil {
		t.Fatal(err)
	}
	if catalogWrappers, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(catalogWrappers, []string{"W w", "postgres_fdw"}) {
		t.Errorf("pg_foreign_data_wrapper holds %q", catalogWrappers)
	}

	// Servers, by name in the C collation of the name type: "s s" before
	// s2.
	wantServers := []ForeignServer{
		{Name: "S s", Owner: owner, Wrapper: "postgres_fdw", Type: "pg", Version: "18",
			Options:    set("host", "remote.invalid", "dbname", "d"),
			Privileges: []PrivilegeGrant{usage(owner, owner), usage(mapped, owner)}, Comment: "live"},
		{Name: "s s", Owner: owner, Wrapper: "W w", Privileges: []PrivilegeGrant{usage(owner, owner)}, PrivilegesDefault: true},
		{Origin: Origin{Extension: "postgres_fdw"}, Name: "s2", Owner: srvown, Wrapper: "W w",
			Privileges: []PrivilegeGrant{usage(srvown, srvown)}, PrivilegesDefault: true},
	}
	checkServers := func(what string, got []*ForeignServer, want ...ForeignServer) {
		t.Helper()
		var g []ForeignServer
		for _, sv := range got {
			c := *sv
			if c.db != d || c.OID == 0 {
				t.Errorf("%s: %s: Database %v, OID %d", what, c.Name, c.db, c.OID)
			}
			c.db, c.OID = nil, 0
			g = append(g, c)
		}
		if !reflect.DeepEqual(g, want) {
			t.Errorf("%s =\n%+v\nwant\n%+v", what, g, want)
		}
	}
	for _, role := range []string{"", plain} {
		as(role, func(ctx context.Context) error {
			ss, err := d.ForeignServers(ctx, ObjectFilter{})
			if err != nil {
				return err
			}
			checkServers("ForeignServers as "+role, ss, wantServers...)
			ss, err = d.ForeignServers(ctx, ObjectFilter{ExtensionMember: new(true)})
			if err != nil {
				return err
			}
			checkServers("ForeignServers, members", ss, wantServers[2])
			ss, err = d.ForeignServers(ctx, ObjectFilter{Name: []TextCriterion{{TextEquals, "S s"}}})
			if err != nil {
				return err
			}
			checkServers(`ForeignServers named "S s" (folded: "s s")`, ss, wantServers[1])
			ss, err = d.ForeignDataWrapperRef("W w").Servers(ctx)
			if err != nil {
				return err
			}
			checkServers(`"W w".Servers`, ss, wantServers[1], wantServers[2])
			for _, i := range []int{0, 1} {
				sv, err := d.ForeignServerByName(ctx, wantServers[i].Name)
				if err != nil {
					return err
				}
				checkServers("ForeignServerByName", []*ForeignServer{sv}, wantServers[i])
			}
			return nil
		})
	}

	// User mappings: what pg_user_mappings shows each role, and what the
	// catalog holds (as the connected superuser).
	held := map[string]bool{} // server/user → the catalog holds options
	rows, err = conn.Query(ctx, `SELECT s.srvname::text || '/' || COALESCE(r.rolname::text, ''), u.umoptions IS NOT NULL
		FROM pg_catalog.pg_user_mapping u JOIN pg_catalog.pg_foreign_server s ON s.oid = u.umserver
		LEFT JOIN pg_catalog.pg_roles r ON r.oid = u.umuser`)
	if err != nil {
		t.Fatal(err)
	}
	var key string
	var has bool
	if _, err := pgx.ForEachRow(rows, []any{&key, &has}, func() error { held[key] = has; return nil }); err != nil {
		t.Fatal(err)
	}
	if len(held) != 5 {
		t.Fatalf("pg_user_mapping holds %v, want the fixture's 5", held)
	}
	type shown struct {
		options []ParameterSetting
		hidden  bool
	}
	wantShown := map[string]map[string]shown{ // role → server/user → what it reads
		"": {
			"S s/": {set("user", "pub", "password", "secret=1"), false}, "S s/" + mapped: {set("user", "m", "password", "secret2"), false},
			"S s/" + plain: {nil, false}, "s2/": {set("x", "pub2"), false}, "s2/" + plain: {set("x", "p"), false},
		},
		plain: {
			"S s/": {nil, true}, "S s/" + mapped: {nil, true}, "S s/" + plain: {nil, true},
			"s2/": {nil, true}, "s2/" + plain: {nil, true},
		},
		mapped: {
			"S s/": {nil, true}, "S s/" + mapped: {set("user", "m", "password", "secret2"), false}, "S s/" + plain: {nil, true},
			"s2/": {nil, true}, "s2/" + plain: {nil, true},
		},
		srvown: {
			"S s/": {nil, true}, "S s/" + mapped: {nil, true}, "S s/" + plain: {nil, true},
			"s2/": {set("x", "pub2"), false}, "s2/" + plain: {nil, true},
		},
	}
	for _, role := range []string{"", plain, mapped, srvown} {
		as(role, func(ctx context.Context) error {
			view := map[string][]string{}
			rows, err := s.on(ctx).Query(ctx, `SELECT srvname::text || '/' || CASE WHEN umuser = 0 THEN '' ELSE usename::text END, umoptions
				FROM pg_catalog.pg_user_mappings`)
			if err != nil {
				return err
			}
			var opts []string
			if _, err := pgx.ForEachRow(rows, []any{&key, &opts}, func() error { view[key] = opts; return nil }); err != nil {
				return err
			}
			ms, err := d.UserMappings(ctx, ObjectFilter{})
			if err != nil {
				return err
			}
			var keys []string
			for _, m := range ms {
				k := m.Server + "/" + m.User
				keys = append(keys, k)
				if m.db != d || m.OID == 0 || m.ServerOID == 0 || (m.UserOID == 0) != m.IsPublic() {
					t.Errorf("as %q: %s: %+v: want its database and OIDs", role, k, m)
				}
				if !reflect.DeepEqual(m.Options, parseSettings(view[k])) {
					t.Errorf("as %q: %s: Options %v, pg_user_mappings shows %q", role, k, m.Options, view[k])
				}
				if held[k] && m.OptionsHidden != (view[k] == nil) {
					t.Errorf("as %q: %s: OptionsHidden %v, pg_user_mappings shows %q", role, k, m.OptionsHidden, view[k])
				}
				if w := wantShown[role][k]; !reflect.DeepEqual(m.Options, w.options) || m.OptionsHidden != w.hidden {
					t.Errorf("as %q: %s: Options %v, OptionsHidden %v; want %v, %v", role, k, m.Options, m.OptionsHidden, w.options, w.hidden)
				}
			}
			if want := []string{"S s/", "S s/" + mapped, "S s/" + plain, "s2/", "s2/" + plain}; !slices.Equal(keys, want) {
				t.Errorf("as %q: UserMappings in order %q, want %q", role, keys, want)
			}
			ms, err = d.ForeignServerRef("s2").UserMappings(ctx)
			if err != nil {
				return err
			}
			if len(ms) != 2 || ms[0].User != "" || ms[1].User != plain || ms[1].UserOID == 0 {
				t.Errorf("as %q: s2.UserMappings = %+v, %+v", role, ms[0], ms[len(ms)-1])
			}
			for _, u := range []string{"", mapped} {
				m, err := d.UserMappingByName(ctx, "S s", u)
				if err != nil {
					return err
				}
				if w := wantShown[role]["S s/"+u]; m.Server != "S s" || m.User != u || !reflect.DeepEqual(m.Options, w.options) || m.OptionsHidden != w.hidden {
					t.Errorf("as %q: UserMappingByName(S s, %q) = %+v", role, u, m)
				}
			}
			if _, err := d.UserMappingByName(ctx, "s s", ""); !errors.Is(err, ErrNotFound) {
				t.Errorf(`as %q: UserMappingByName("s s", PUBLIC): err = %v, want ErrNotFound`, role, err)
			}
			if _, err := d.UserMappingByName(ctx, "s2", mapped); !errors.Is(err, ErrNotFound) {
				t.Errorf(`as %q: UserMappingByName(s2, mapped): err = %v, want ErrNotFound — PUBLIC's is no match`, role, err)
			}
			return nil
		})
	}

	// Foreign tables.
	wantTables := []ForeignTable{
		{Schema: "A a", Name: "F t", Owner: owner, Server: "S s", Options: set("schema_name", "r", "table_name", "t"),
			Comment: "live"},
		{Schema: "A a", Name: "fp", Owner: owner, Server: "s s", IsPartition: true, PartitionBound: "FOR VALUES IN (1)",
			Parents: []ObjectName{{"A a", "parted"}}, PrivilegesDefault: true},
		{Schema: "A a", Name: "kid", Owner: owner, Server: "s2", Parents: []ObjectName{{"A a", "base"}}, PrivilegesDefault: true},
	}
	var ownerPrivileges []PrivilegeGrant
	rows, err = conn.Query(ctx, `SELECT a.privilege_type FROM pg_catalog.aclexplode(pg_catalog.acldefault('r', (SELECT oid FROM pg_catalog.pg_roles WHERE rolname = current_user))) a`)
	if err != nil {
		t.Fatal(err)
	}
	privs, err := pgx.CollectRows(rows, pgx.RowTo[string])
	if err != nil {
		t.Fatal(err)
	}
	for _, p := range privs {
		ownerPrivileges = append(ownerPrivileges, PrivilegeGrant{Grantee: owner, Grantor: owner, Privilege: Privilege(p)})
	}
	wantTables[0].Privileges = append(slices.Clone(ownerPrivileges), PrivilegeGrant{Grantee: plain, Grantor: owner, Privilege: PrivilegeSelect})
	wantTables[1].Privileges, wantTables[2].Privileges = ownerPrivileges, ownerPrivileges
	checkTables := func(what string, got []*ForeignTable, want ...ForeignTable) {
		t.Helper()
		var g []ForeignTable
		for _, ft := range got {
			c := *ft
			if c.db != d || c.OID == 0 {
				t.Errorf("%s: %s: Database %v, OID %d", what, c.Name, c.db, c.OID)
			}
			c.db, c.OID = nil, 0
			g = append(g, c)
		}
		if !reflect.DeepEqual(g, want) {
			t.Errorf("%s =\n%+v\nwant\n%+v", what, g, want)
		}
	}
	for _, role := range []string{"", plain} {
		as(role, func(ctx context.Context) error {
			fts, err := d.ForeignTables(ctx, ObjectFilter{})
			if err != nil {
				return err
			}
			checkTables("ForeignTables as "+role, fts, wantTables...)
			fts, err = d.ForeignTables(ctx, ObjectFilter{Name: []TextCriterion{{TextContains, "i"}}, Schema: []TextCriterion{{TextEquals, `"A a"`}}})
			if err != nil {
				return err
			}
			checkTables("ForeignTables named *i* in \"A a\"", fts, wantTables[2])
			ft, err := d.ForeignTableByName(ctx, "A a", "F t")
			if err != nil {
				return err
			}
			checkTables("ForeignTableByName", []*ForeignTable{ft}, wantTables[0])
			if _, err := d.ForeignTableByName(ctx, "A a", "base"); !errors.Is(err, ErrNotFound) {
				t.Errorf("ForeignTableByName(base): err = %v, want ErrNotFound — a table is no foreign table", err)
			}

			cols, err := ft.Columns(ctx)
			if err != nil {
				return err
			}
			if len(cols) != 2 || cols[0].Name != "id" || !cols[0].NotNull ||
				!reflect.DeepEqual(cols[0].FDWOptions, set("column_name", "ID")) || cols[1].FDWOptions != nil {
				t.Errorf("Columns = %+v, %+v", cols[0], cols[len(cols)-1])
			}
			cons, err := ft.Constraints(ctx)
			if err != nil {
				return err
			}
			var names []string
			for _, c := range cons {
				names = append(names, fmt.Sprintf("%s %v", c.Name, c.Enforced))
			}
			want := []string{"F t_note_check true"}
			if version >= 180000 {
				want = []string{"F t_id_not_null true", "F t_note_check true", "loose false"}
			}
			if !slices.Equal(names, want) {
				t.Errorf("Constraints %q, want %q", names, want)
			}
			trs, err := ft.Triggers(ctx)
			if err != nil {
				return err
			}
			if len(trs) != 1 || trs[0].Name != "ft_touch" || trs[0].TableKind != RelationForeignTable || trs[0].Table != "F t" {
				t.Errorf("Triggers = %v", dumpTriggers(trs))
			}
			base, err := d.TableByName(ctx, "A a", "base")
			if err != nil {
				return err
			}
			if cols, err := base.Columns(ctx); err != nil || len(cols) != 1 || cols[0].FDWOptions != nil {
				t.Errorf("a table's Columns = %v, %v: want no FDWOptions", cols, err)
			}
			return nil
		})
	}
	var catalogTables []string
	rows, err = conn.Query(ctx, `SELECT c.relname::text FROM pg_catalog.pg_foreign_table f JOIN pg_catalog.pg_class c ON c.oid = f.ftrelid ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	if catalogTables, err = pgx.CollectRows(rows, pgx.RowTo[string]); err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(catalogTables, []string{"F t", "fp", "kid"}) {
		t.Errorf("pg_foreign_table holds %s", strings.Join(catalogTables, ", "))
	}
}
