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

	"github.com/radix29/pgmo/pgsql"
)

// TestLiveRoles reads throwaway roles the listing and the by-name read must
// tell apart and describe in full:
//
//   - app: LOGIN NOINHERIT CREATEDB CREATEROLE REPLICATION BYPASSRLS, a
//     connection limit, VALID UNTIL, a comment, settings in every database
//     (a list holding '=') and two in a throwaway database, while that
//     database's own setting must not appear;
//   - grp, of which app is a member WITH ADMIN OPTION granted by the
//     connected superuser and — from 16 — again, by granter, WITH INHERIT
//     FALSE, SET FALSE; before 16, granter's grant to sub, after which
//     granter is dropped, leaving no grantor name;
//   - sub, a member of app;
//   - never and always: VALID UNTIL 'infinity' and '-infinity';
//   - two names differing only by case.
//
// The listing is read again as a plain role: every column is readable by
// all, so it must be identical. System narrows to the predefined roles.
func TestLiveRoles(t *testing.T) {
	admin, ctx, done := liveDBTimeout(t, 2*time.Minute)
	t.Cleanup(done)
	stamp := time.Now().UnixNano()
	name := func(s string) string { return fmt.Sprintf("pgmo_live_role_%d_%s", stamp, s) }
	app, grp, granter, sub, never, always, upper, lower, plain := name("app"), name("grp"), name("granter"),
		name("sub"), name("never"), name("always"), name("Case"), name("case"), name("plain")
	db := fmt.Sprintf("pgmo_live_role_%d_db", stamp)

	s, err := Connect(ctx, ConnectionOptions{ConnString: *liveConnString, ApplicationName: "pgmo - live roles"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	v := s.Version()
	var connected string
	if err := admin.QueryRow(ctx, "SELECT current_user::text").Scan(&connected); err != nil {
		t.Fatal(err)
	}
	var bootstrap string
	if err := admin.QueryRow(ctx, "SELECT rolname::text FROM pg_catalog.pg_roles WHERE oid = 10").Scan(&bootstrap); err != nil {
		t.Fatal(err)
	}

	q := pgsql.QuoteIdent
	// Dropped last to first: granter last, since from 16 its grant to app
	// holds it until app goes.
	roles := []string{granter, app, grp, sub, never, always, upper, lower, plain}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+q(db)+" WITH (FORCE)"); err != nil {
			t.Errorf("drop database: %v", err)
		}
		for _, r := range slices.Backward(roles) {
			if _, err := admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+q(r)); err != nil {
				t.Errorf("drop role %s: %v", r, err)
			}
		}
	})
	stmts := []string{
		"CREATE DATABASE " + q(db),
		"CREATE ROLE " + q(app) + " LOGIN NOINHERIT CREATEDB CREATEROLE REPLICATION BYPASSRLS CONNECTION LIMIT 4" +
			" VALID UNTIL '2030-01-02 03:04:05+00'",
		"CREATE ROLE " + q(grp),
		"CREATE ROLE " + q(granter) + " CREATEROLE",
		"CREATE ROLE " + q(sub),
		"CREATE ROLE " + q(never) + " VALID UNTIL 'infinity'",
		"CREATE ROLE " + q(always) + " VALID UNTIL '-infinity'",
		"CREATE ROLE " + q(upper),
		"CREATE ROLE " + q(lower),
		"CREATE ROLE " + q(plain),
		"COMMENT ON ROLE " + q(app) + " IS 'the app'",
		"ALTER ROLE " + q(app) + ` SET search_path = '$user', "Odd=Name"`,
		"ALTER ROLE " + q(app) + " SET work_mem = '2MB'",
		"ALTER ROLE " + q(app) + " IN DATABASE " + q(db) + " SET work_mem = '3MB'",
		"ALTER ROLE " + q(app) + " IN DATABASE " + q(db) + " SET statement_timeout = '5s'",
		"ALTER DATABASE " + q(db) + " SET work_mem = '9MB'",
		"GRANT " + q(grp) + " TO " + q(app) + " WITH ADMIN OPTION",
		"GRANT " + q(grp) + " TO " + q(granter) + " WITH ADMIN OPTION",
		"GRANT " + q(app) + " TO " + q(sub),
	}
	if v >= PostgreSQL16 {
		stmts = append(stmts,
			"SET ROLE "+q(granter),
			"GRANT "+q(grp)+" TO "+q(app)+" WITH INHERIT FALSE, SET FALSE",
			"RESET ROLE")
	} else {
		stmts = append(stmts,
			"SET ROLE "+q(granter),
			"GRANT "+q(grp)+" TO "+q(sub),
			"RESET ROLE",
			"REVOKE "+q(grp)+" FROM "+q(granter),
			"DROP ROLE "+q(granter))
	}
	conn, err := admin.Acquire(ctx) // SET ROLE needs one session
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range stmts {
		if _, err := conn.Exec(ctx, stmt); err != nil {
			conn.Release()
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	conn.Release()

	// The grantor of a superuser's grant: the connected role on 15, the
	// bootstrap superuser from 16.
	superGrantor := connected
	if v >= PostgreSQL16 {
		superGrantor = bootstrap
	}

	// The listing narrowed to the stamp, in name order (byte order: _Case
	// before _always before _app…).
	rs, err := s.Roles(ctx, ObjectFilter{Name: []TextCriterion{{TextContains, fmt.Sprint(stamp)}}})
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, r := range rs {
		names = append(names, r.Name)
	}
	want := slices.Clone(roles)
	if v < PostgreSQL16 {
		want = slices.DeleteFunc(want, func(r string) bool { return r == granter })
	}
	slices.Sort(want)
	if !slices.Equal(names, want) {
		t.Fatalf("Roles = %q, want %q", names, want)
	}
	// A quoted value matches one of the two case-only names alone.
	if rs, err := s.Roles(ctx, ObjectFilter{Name: []TextCriterion{{TextEquals, q(upper)}}}); err != nil || len(rs) != 1 || rs[0].Name != upper {
		t.Errorf("Roles(equals %s) = %v, %v; want it alone", q(upper), rs, err)
	}

	r, err := s.RoleByName(ctx, app)
	if err != nil {
		t.Fatal(err)
	}
	got := *r
	got.server, got.OID = nil, 0
	got.ValidUntil = got.ValidUntil.UTC()
	wantApp := Role{
		Name: app, Inherit: false, CreateRole: true, CreateDB: true, CanLogin: true, Replication: true, BypassRLS: true,
		ConnectionLimit: 4, ValidUntil: time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC),
		Settings: []ParameterSetting{{"search_path", `"$user", "Odd=Name"`}, {"work_mem", "2MB"}},
		DatabaseSettings: []DatabaseSettings{{Database: db, Settings: []ParameterSetting{
			{"work_mem", "3MB"}, {"statement_timeout", "5s"}}}},
		MemberOf: []RoleMembership{{Role: grp, Member: app, Grantor: superGrantor, Admin: true, Inherit: false, Set: true}},
		Members:  []RoleMembership{{Role: app, Member: sub, Grantor: superGrantor, Inherit: true, Set: true}},
		Comment:  "the app",
	}
	if v >= PostgreSQL16 {
		// From 16 a grant's INHERIT option defaults to the member's INHERIT
		// attribute — false for app, as 15 reads it. granter's grant orders
		// after the superuser's: its grantor's OID is the larger.
		wantApp.MemberOf = append(wantApp.MemberOf, RoleMembership{Role: grp, Member: app, Grantor: granter})
	}
	if !reflect.DeepEqual(got, wantApp) {
		t.Errorf("RoleByName(app) =\n%+v\nwant\n%+v", got, wantApp)
	}
	if r.OID < 16384 || r.IsPredefined() || r.IsBootstrapSuperuser() || r.Server() != s || r.PasswordExpired(time.Date(2029, 1, 1, 0, 0, 0, 0, time.UTC)) {
		t.Errorf("OID %d, IsPredefined %v, IsBootstrapSuperuser %v: want a user role, password not expired in 2029",
			r.OID, r.IsPredefined(), r.IsBootstrapSuperuser())
	}

	// grp's members, by member name then grantor.
	g, err := s.RoleByName(ctx, grp)
	if err != nil {
		t.Fatal(err)
	}
	wantMembers := []RoleMembership{{Role: grp, Member: app, Grantor: superGrantor, Admin: true, Inherit: false, Set: true}}
	if v >= PostgreSQL16 {
		wantMembers = append(wantMembers,
			RoleMembership{Role: grp, Member: app, Grantor: granter},
			RoleMembership{Role: grp, Member: granter, Grantor: superGrantor, Admin: true, Inherit: true, Set: true})
	} else {
		// granter was dropped after granting grp to sub: no grantor name.
		wantMembers = append(wantMembers, RoleMembership{Role: grp, Member: sub, Grantor: "", Inherit: true, Set: true})
	}
	if !reflect.DeepEqual(g.Members, wantMembers) || g.MemberOf != nil {
		t.Errorf("grp: Members\n%+v\nwant\n%+v; MemberOf %v", g.Members, wantMembers, g.MemberOf)
	}

	for _, c := range []struct {
		role    string
		zero    bool
		expired bool
	}{{never, true, false}, {always, false, true}, {upper, true, false}} {
		r, err := s.RoleByName(ctx, c.role)
		if err != nil {
			t.Fatal(err)
		}
		if r.ValidUntil.IsZero() != c.zero || r.PasswordExpired(time.Now()) != c.expired ||
			(c.role == always && !r.ValidUntil.Equal(ValidUntilNegativeInfinity)) {
			t.Errorf("%s: ValidUntil %v, expired %v", c.role, r.ValidUntil, r.PasswordExpired(time.Now()))
		}
	}
	// Exact: a case the catalog does not hold is not found.
	if _, err := s.RoleByName(ctx, name("CASE")); !errors.Is(err, ErrNotFound) {
		t.Errorf("RoleByName(_CASE): err = %v, want ErrNotFound", err)
	}

	// The bootstrap superuser and a predefined role.
	if b, err := s.RoleByName(ctx, bootstrap); err != nil || !b.IsBootstrapSuperuser() || b.IsPredefined() || !b.Superuser {
		t.Errorf("RoleByName(%s) = %+v, %v; want the bootstrap superuser", bootstrap, b, err)
	}
	m, err := s.RoleByName(ctx, "pg_monitor")
	if err != nil || !m.IsPredefined() || m.CanLogin {
		t.Fatalf("RoleByName(pg_monitor) = %+v, %v; want a predefined role", m, err)
	}
	var memberOf []string
	for _, g := range m.MemberOf {
		memberOf = append(memberOf, g.Role)
	}
	if want := []string{"pg_read_all_settings", "pg_read_all_stats", "pg_stat_scan_tables"}; !slices.Equal(memberOf, want) {
		t.Errorf("pg_monitor MemberOf %q, want %q", memberOf, want)
	}

	// System: true is the predefined roles alone, false none of them.
	yes, no := true, false
	sys, err := s.Roles(ctx, ObjectFilter{System: &yes})
	if err != nil {
		t.Fatal(err)
	}
	if len(sys) < 10 || !slices.ContainsFunc(sys, func(r *Role) bool { return r.Name == "pg_monitor" }) {
		t.Errorf("Roles(System) = %d roles, want the predefined ones with pg_monitor", len(sys))
	}
	for _, r := range sys {
		if !r.IsPredefined() || len(r.Name) < 3 || r.Name[:3] != "pg_" {
			t.Errorf("Roles(System) lists %s (OID %d)", r.Name, r.OID)
		}
	}
	users, err := s.Roles(ctx, ObjectFilter{System: &no})
	if err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(users, (*Role).IsPredefined) || !slices.ContainsFunc(users, func(r *Role) bool { return r.Name == bootstrap }) {
		t.Errorf("Roles(not System) lists a predefined role, or not the bootstrap superuser")
	}

	// As a plain role: the same rows.
	var asPlain []*Role
	err = s.InTransaction(ctx, func(ctx context.Context) error {
		if _, err := s.on(ctx).Exec(ctx, "SET LOCAL ROLE "+q(plain)); err != nil {
			return err
		}
		var err error
		asPlain, err = s.Roles(ctx, ObjectFilter{Name: []TextCriterion{{TextContains, fmt.Sprint(stamp)}}})
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%+v", deref(asPlain)) != fmt.Sprintf("%+v", deref(rs)) {
		t.Errorf("as a plain role the listing differs:\n%+v\nwant\n%+v", deref(asPlain), deref(rs))
	}
}

// deref is rs's values, so %+v prints the fields rather than pointers.
func deref(rs []*Role) []Role {
	out := make([]Role, len(rs))
	for i, r := range rs {
		out[i] = *r
		out[i].server = nil
	}
	return out
}
