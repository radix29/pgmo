//go:build livedb

package pgmo

import (
	"context"
	"errors"
	"fmt"
	"reflect"
	"slices"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/radix29/pgmo/pgsql"
)

// TestLiveSchemas reads the schemas of a throwaway database the listing and
// the by-name read must tell apart and describe in full:
//
//   - app, owned by own: USAGE WITH GRANT OPTION granted to "B b", CREATE to
//     PUBLIC; default privileges IN SCHEMA app for own (tables, types) and
//     for "B b" (functions, WITH GRANT OPTION); citext installed in it,
//     which does not make app an extension member;
//   - App, differing from app only by case, with a comment;
//   - untouched, whose ACL was never set, and reset, granted and revoked
//     back to the same value — the one PrivilegesDefault, the other not;
//   - adopted, made a member of citext by ALTER EXTENSION … ADD SCHEMA;
//   - public, owned by pg_database_owner.
//
// own's database-wide default privileges — EXECUTE on functions revoked from
// PUBLIC, every privilege on tables revoked from own itself (an entry with
// no grants), USAGE on schemas granted to "B b" — are read by
// Database.DefaultPrivileges and appear on no Schema. The listing is read
// again as a plain role: pg_namespace and pg_default_acl are readable by
// all, so it must be identical.
func TestLiveSchemas(t *testing.T) {
	admin, ctx, done := liveDBTimeout(t, 2*time.Minute)
	t.Cleanup(done)
	stamp := time.Now().UnixNano()
	name := func(s string) string { return fmt.Sprintf("pgmo_live_schema_%d_%s", stamp, s) }
	own, b, plain := name("own"), name("B b"), name("plain")
	db := name("db")
	q := pgsql.QuoteIdent

	var connected string
	if err := admin.QueryRow(ctx, "SELECT current_user::text").Scan(&connected); err != nil {
		t.Fatal(err)
	}
	roles := []string{own, b, plain}
	t.Cleanup(func() {
		// The database first: its ACLs and default privileges name the roles.
		if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+q(db)+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database: %v", err)
		}
		for _, r := range roles {
			if _, err := admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+q(r)); err != nil {
				t.Errorf("drop role %s: %v", r, err)
			}
		}
	})
	for _, stmt := range []string{"CREATE DATABASE " + q(db), "CREATE ROLE " + q(own), "CREATE ROLE " + q(b), "CREATE ROLE " + q(plain)} {
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
		"CREATE SCHEMA app AUTHORIZATION " + q(own),
		`CREATE SCHEMA "App"`,
		`COMMENT ON SCHEMA "App" IS 'case'`,
		"CREATE SCHEMA untouched",
		"CREATE SCHEMA reset",
		"GRANT USAGE ON SCHEMA reset TO " + q(own),
		"REVOKE USAGE ON SCHEMA reset FROM " + q(own),
		"CREATE SCHEMA adopted",
		"CREATE EXTENSION citext SCHEMA app",
		"ALTER EXTENSION citext ADD SCHEMA adopted",
		"GRANT USAGE ON SCHEMA app TO " + q(b) + " WITH GRANT OPTION",
		"GRANT CREATE ON SCHEMA app TO PUBLIC",
		"ALTER DEFAULT PRIVILEGES FOR ROLE " + q(own) + " IN SCHEMA app GRANT SELECT ON TABLES TO " + q(b) + ", PUBLIC",
		"ALTER DEFAULT PRIVILEGES FOR ROLE " + q(own) + " IN SCHEMA app GRANT USAGE ON TYPES TO " + q(b),
		"ALTER DEFAULT PRIVILEGES FOR ROLE " + q(b) + " IN SCHEMA app GRANT EXECUTE ON FUNCTIONS TO " + q(own) + " WITH GRANT OPTION",
		"ALTER DEFAULT PRIVILEGES FOR ROLE " + q(own) + " REVOKE EXECUTE ON FUNCTIONS FROM PUBLIC",
		"ALTER DEFAULT PRIVILEGES FOR ROLE " + q(own) + " REVOKE ALL ON TABLES FROM " + q(own),
		"ALTER DEFAULT PRIVILEGES FOR ROLE " + q(own) + " GRANT USAGE ON SCHEMAS TO " + q(b),
	} {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}

	s, err := Connect(ctx, ConnectionOptions{ConnString: *liveConnString, Database: db, ApplicationName: "pgmo - live schemas"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close) // before the drop: the database's sessions end first
	d, err := s.DatabaseByName(ctx, db)
	if err != nil {
		t.Fatal(err)
	}

	// The user schemas no extension owns, in byte order: App before app.
	no, yes := false, true
	users, err := d.Schemas(ctx, ObjectFilter{System: &no, ExtensionMember: &no})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, sc := range users {
		names = append(names, sc.Name)
	}
	if want := []string{"App", "app", "public", "reset", "untouched"}; !slices.Equal(names, want) {
		t.Fatalf("Schemas(user) = %q, want %q", names, want)
	}

	grant := func(grantee, grantor string, p Privilege, grantable bool) PrivilegeGrant {
		return PrivilegeGrant{Grantee: grantee, Grantor: grantor, Privilege: p, Grantable: grantable}
	}
	inApp := []DefaultPrivileges{
		{Role: b, Schema: "app", ObjectType: DefaultPrivilegeFunctions, Grants: []PrivilegeGrant{grant(own, b, PrivilegeExecute, true)}},
		{Role: own, Schema: "app", ObjectType: DefaultPrivilegeTypes, Grants: []PrivilegeGrant{grant(b, own, PrivilegeUsage, false)}},
		{Role: own, Schema: "app", ObjectType: DefaultPrivilegeTables, Grants: []PrivilegeGrant{
			// The server's ACL order, not the GRANT's: PUBLIC's item first.
			grant("", own, PrivilegeSelect, false), grant(b, own, PrivilegeSelect, false)}},
	}
	app, err := d.SchemaByName(ctx, "app")
	if err != nil {
		t.Fatal(err)
	}
	got := *app
	got.db, got.OID = nil, 0
	wantApp := Schema{
		Name: "app", Owner: own,
		Privileges: []PrivilegeGrant{grant(own, own, PrivilegeUsage, false), grant(own, own, PrivilegeCreate, false),
			grant(b, own, PrivilegeUsage, true), grant("", own, PrivilegeCreate, false)},
		DefaultPrivileges: inApp,
	}
	if !reflect.DeepEqual(got, wantApp) {
		t.Errorf("SchemaByName(app) =\n%+v\nwant\n%+v", got, wantApp)
	}
	if app.OID < 16384 || app.Database() != d {
		t.Errorf("OID %d, Database %p: want a user OID, read from %p", app.OID, app.Database(), d)
	}
	if i := slices.IndexFunc(users, func(sc *Schema) bool { return sc.Name == "app" }); !reflect.DeepEqual(users[i], app) {
		t.Errorf("listing's app =\n%+v\nby name\n%+v", users[i], app)
	}

	ownerDefault := []PrivilegeGrant{grant(connected, connected, PrivilegeUsage, false), grant(connected, connected, PrivilegeCreate, false)}
	for _, c := range []struct {
		name        string
		comment     string
		aclNeverSet bool
	}{{"App", "case", true}, {"untouched", "", true}, {"reset", "", false}} {
		sc, err := d.SchemaByName(ctx, c.name)
		if err != nil {
			t.Fatal(err)
		}
		if sc.Owner != connected || sc.Comment != c.comment || sc.PrivilegesDefault != c.aclNeverSet ||
			!reflect.DeepEqual(sc.Privileges, ownerDefault) || sc.DefaultPrivileges != nil || sc.Origin != (Origin{}) {
			t.Errorf("%s = %+v; want %s's, comment %q, PrivilegesDefault %v, the owner's USAGE and CREATE",
				c.name, sc, connected, c.comment, c.aclNeverSet)
		}
	}
	// Exact: a case the catalog does not hold is not found.
	if _, err := d.SchemaByName(ctx, "APP"); !errors.Is(err, ErrNotFound) {
		t.Errorf("SchemaByName(APP): err = %v, want ErrNotFound", err)
	}
	// A quoted value matches one of the two case-only names alone.
	if ss, err := d.Schemas(ctx, ObjectFilter{Name: []TextCriterion{{TextEquals, `"App"`}}}); err != nil || len(ss) != 1 || ss[0].Name != "App" {
		t.Errorf(`Schemas(equals "App") = %v, %v; want it alone`, ss, err)
	}

	// public belongs to pg_database_owner, and PUBLIC may use it.
	pub, err := d.SchemaByName(ctx, "public")
	if err != nil {
		t.Fatal(err)
	}
	if pub.Owner != "pg_database_owner" || pub.PrivilegesDefault ||
		!slices.Contains(pub.Privileges, grant("", "pg_database_owner", PrivilegeUsage, false)) {
		t.Errorf("public = %+v; want pg_database_owner's, PUBLIC holding USAGE", pub)
	}

	// Extension membership: adopted alone, and not system.
	members, err := d.Schemas(ctx, ObjectFilter{ExtensionMember: &yes})
	if err != nil {
		t.Fatal(err)
	}
	if len(members) != 1 || members[0].Name != "adopted" || members[0].Extension != "citext" || members[0].IsSystem {
		t.Errorf("Schemas(ExtensionMember) = %+v; want adopted, a member of citext", derefSchemas(members))
	}

	// System: pg_catalog, information_schema, pg_toast; each IsSystem.
	sys, err := d.Schemas(ctx, ObjectFilter{System: &yes})
	if err != nil {
		t.Fatal(err)
	}
	var sysNames []string
	for _, sc := range sys {
		sysNames = append(sysNames, sc.Name)
		if !sc.IsSystem || sc.IsExtensionMember() {
			t.Errorf("Schemas(System) lists %s with IsSystem %v, Extension %q", sc.Name, sc.IsSystem, sc.Extension)
		}
	}
	for _, want := range []string{"information_schema", "pg_catalog", "pg_toast"} {
		if !slices.Contains(sysNames, want) {
			t.Errorf("Schemas(System) = %q, want %s among them", sysNames, want)
		}
	}

	// The database's default privileges: own's three database-wide entries
	// (the emptied one with no grants) among the IN SCHEMA ones, by role,
	// database-wide first, then type code.
	dps, err := d.DefaultPrivileges(ctx)
	if err != nil {
		t.Fatal(err)
	}
	wantDPs := []DefaultPrivileges{
		inApp[0],
		{Role: own, ObjectType: DefaultPrivilegeFunctions, Grants: []PrivilegeGrant{grant(own, own, PrivilegeExecute, false)}},
		{Role: own, ObjectType: DefaultPrivilegeSchemas, Grants: []PrivilegeGrant{
			grant(own, own, PrivilegeUsage, false), grant(own, own, PrivilegeCreate, false), grant(b, own, PrivilegeUsage, false)}},
		{Role: own, ObjectType: DefaultPrivilegeTables},
		inApp[1], inApp[2],
	}
	if !reflect.DeepEqual(dps, wantDPs) {
		t.Errorf("DefaultPrivileges =\n%+v\nwant\n%+v", dps, wantDPs)
	}
	// On a DatabaseRef: keyed on the name alone.
	if ref, err := s.DatabaseRef(db).DefaultPrivileges(ctx); err != nil || !reflect.DeepEqual(ref, dps) {
		t.Errorf("DatabaseRef(%s).DefaultPrivileges = %+v, %v; want the same", db, ref, err)
	}

	// As a plain role: the same rows.
	all, err := d.Schemas(ctx, ObjectFilter{})
	if err != nil {
		t.Fatal(err)
	}
	var asPlain []*Schema
	err = s.InTransaction(ctx, func(ctx context.Context) error {
		if _, err := s.on(ctx).Exec(ctx, "SET LOCAL ROLE "+q(plain)); err != nil {
			return err
		}
		var err error
		asPlain, err = d.Schemas(ctx, ObjectFilter{})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%+v", derefSchemas(asPlain)) != fmt.Sprintf("%+v", derefSchemas(all)) {
		t.Errorf("as a plain role the listing differs:\n%+v\nwant\n%+v", derefSchemas(asPlain), derefSchemas(all))
	}
}

// derefSchemas is ss's values, so %+v prints the fields rather than
// pointers.
func derefSchemas(ss []*Schema) []Schema {
	out := make([]Schema, len(ss))
	for i, s := range ss {
		out[i] = *s
		out[i].db = nil
	}
	return out
}
