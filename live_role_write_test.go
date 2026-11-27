//go:build livedb

package pgmo

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"time"

	"github.com/radix29/pgmo/internal/scram"
	"github.com/radix29/pgmo/pgsql"
)

// TestLiveRoleWrites creates roles with every option CREATE ROLE takes,
// exercises each ALTER ROLE form and membership grant, and drops them,
// against the catalog:
//
//   - CreateRole reads back every attribute, the password as a verifier of
//     it, and the IN ROLE, ROLE and ADMIN memberships; the statement it
//     captures under WithScript+WithScriptSecrets, run by hand, makes the
//     same role with the same password;
//   - Alter flips every attribute and writes 'infinity' and '-infinity';
//     what is read back is what was mirrored;
//   - parameters, every database's and one database's, write back through
//     Values identically, and reset;
//   - AddMember and RemoveMember write each option the release has, and
//     refuse the ones it has not before sending;
//   - the server's own refusals arrive with their SQLSTATE: a predefined
//     role, a role holding a privilege, a membership cycle;
//   - a write inside a failed InTransaction leaves nothing.
func TestLiveRoleWrites(t *testing.T) {
	admin, ctx, done := liveDBTimeout(t, 2*time.Minute)
	t.Cleanup(done)
	stamp := time.Now().UnixNano()
	grp := fmt.Sprintf("pgmo_live_rw_grp_%d", stamp)
	mem := fmt.Sprintf("pgmo_live_rw_Mem_%d", stamp)
	boss := fmt.Sprintf("pgmo_live_rw_boss_%d", stamp)
	role := fmt.Sprintf(`pgmo_live_rw_%d "Odd's"`, stamp)
	renamed := fmt.Sprintf("pgmo_live_rw_%d renamed", stamp)
	scripted := fmt.Sprintf("pgmo_live_rw_%d_script", stamp)
	rolledBack := fmt.Sprintf("pgmo_live_rw_%d_tx", stamp)
	q := pgsql.QuoteIdent

	// The roles holding grants first: DROP OWNED revokes what each holds
	// here and on shared objects.
	t.Cleanup(func() {
		for _, r := range []string{role, renamed, scripted, rolledBack, mem, boss, grp} {
			var n int
			if err := admin.QueryRow(context.Background(), `SELECT count(*) FROM pg_catalog.pg_roles WHERE rolname = $1`, r).Scan(&n); err != nil || n == 0 {
				continue
			}
			for _, stmt := range []string{"DROP OWNED BY " + q(r), "DROP ROLE " + q(r)} {
				if _, err := admin.Exec(context.Background(), stmt); err != nil {
					t.Errorf("%s: %v", stmt, err)
				}
			}
		}
	})

	s, err := Connect(ctx, ConnectionOptions{ConnString: *liveConnString, ApplicationName: "pgmo - live role writes"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	v := s.Version()
	db := s.CurrentDatabase()

	for _, name := range []string{grp, mem, boss} {
		if _, err := s.CreateRole(ctx, CreateRoleRequest{Name: name}); err != nil {
			t.Fatal(err)
		}
	}

	// Create with every option; the time to the nanosecond, stored to the
	// microsecond.
	at := time.Date(2031, 2, 3, 4, 5, 6, 789123456, time.FixedZone("X", -5*3600))
	req := CreateRoleRequest{
		Name: role,
		RoleAttributes: RoleAttributes{
			Superuser: new(false), Inherit: new(false), CreateRole: new(true), CreateDB: new(true),
			CanLogin: new(true), Replication: new(true), BypassRLS: new(true),
			ConnectionLimit: new(3), ValidUntil: &at,
		},
		Password:     "s3cr3t'pw",
		InRole:       []string{grp},
		Members:      []string{mem},
		AdminMembers: []string{boss},
	}
	got, err := s.CreateRole(ctx, req)
	if err != nil {
		t.Fatal(err)
	}
	stored := at.Truncate(time.Microsecond)
	if got.OID == 0 || got.Name != role || got.Superuser || got.Inherit || !got.CreateRole || !got.CreateDB ||
		!got.CanLogin || !got.Replication || !got.BypassRLS || got.ConnectionLimit != 3 || !got.ValidUntil.Equal(stored) {
		t.Errorf("created %+v (valid until %v, want %v)", got, got.ValidUntil, stored)
	}
	if len(got.MemberOf) != 1 || got.MemberOf[0].Role != grp || got.MemberOf[0].Admin {
		t.Errorf("IN ROLE: member of %+v", got.MemberOf)
	}
	if len(got.Members) != 2 || got.Members[0].Member != mem || got.Members[0].Admin ||
		got.Members[1].Member != boss || !got.Members[1].Admin {
		t.Errorf("ROLE and ADMIN: members %+v", got.Members)
	}
	if pw := storedPassword(t, admin, ctx, role); !scram.Verifies(pw, "s3cr3t'pw") {
		t.Errorf("stored %q, not a verifier of the password", pw)
	}

	// The captured statement — with its verifier — run as it is.
	sreq := req
	sreq.Name, sreq.Members, sreq.AdminMembers = scripted, nil, nil
	sctx, script := WithScript(WithScriptSecrets(ctx))
	h, err := s.CreateRole(sctx, sreq)
	if err != nil || h.OID != 0 {
		t.Fatalf("scripted create: %+v, %v", h, err)
	}
	if _, err := s.RoleByName(ctx, scripted); !errors.Is(err, ErrNotFound) {
		t.Fatalf("a scripted create made %q: %v", scripted, err)
	}
	if _, err := admin.Exec(ctx, script.Entries()[0].SQL); err != nil {
		t.Fatalf("the captured statement: %v", err)
	}
	sgot, err := s.RoleByName(ctx, scripted)
	if err != nil {
		t.Fatal(err)
	}
	if sgot.CanLogin != got.CanLogin || sgot.Inherit != got.Inherit || sgot.BypassRLS != got.BypassRLS ||
		sgot.ConnectionLimit != 3 || !sgot.ValidUntil.Equal(stored) || len(sgot.MemberOf) != 1 {
		t.Errorf("the captured statement made %+v", sgot)
	}
	if pw := storedPassword(t, admin, ctx, scripted); !scram.Verifies(pw, "s3cr3t'pw") {
		t.Errorf("the captured statement stored %q", pw)
	}
	if err := sgot.Drop(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RoleByName(ctx, scripted); !errors.Is(err, ErrNotFound) {
		t.Errorf("still there after the drop: %v", err)
	}

	reread := func() *Role {
		t.Helper()
		r, err := s.RoleByName(ctx, got.Name)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	attrs := func(r *Role) string {
		return fmt.Sprint(r.Name, r.Superuser, r.Inherit, r.CreateRole, r.CreateDB, r.CanLogin,
			r.Replication, r.BypassRLS, r.ConnectionLimit, r.ValidUntil.UTC())
	}

	// Alter: every attribute flipped; the expiry to both infinities.
	for _, a := range []RoleAttributes{
		{Superuser: new(true), Inherit: new(true), CreateRole: new(false), CreateDB: new(false),
			CanLogin: new(false), Replication: new(false), BypassRLS: new(false),
			ConnectionLimit: new(-1), ValidUntil: new(time.Time{})},
		{Superuser: new(false), ValidUntil: &ValidUntilNegativeInfinity},
		{ConnectionLimit: new(0)},
	} {
		if err := got.Alter(ctx, a); err != nil {
			t.Fatal(err)
		}
		if r := reread(); attrs(r) != attrs(got) {
			t.Errorf("after Alter(%+v)\n read     %s\n mirrored %s", a, attrs(r), attrs(got))
		}
	}
	if !got.ValidUntil.Equal(ValidUntilNegativeInfinity) || got.Superuser || !got.Inherit || got.ConnectionLimit != 0 {
		t.Errorf("after the alters: %s", attrs(got))
	}

	// Rename; the SCRAM password stays.
	if err := got.Rename(ctx, renamed); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RoleByName(ctx, role); !errors.Is(err, ErrNotFound) {
		t.Errorf("the old name still reads: %v", err)
	}
	if r := reread(); r.Name != renamed {
		t.Errorf("read %q after the rename", r.Name)
	}
	if pw := storedPassword(t, admin, ctx, renamed); !scram.Verifies(pw, "s3cr3t'pw") {
		t.Errorf("the rename lost the password: %q", pw)
	}

	// Passwords: set, cleared.
	if err := got.SetPassword(ctx, "n3w"); err != nil {
		t.Fatal(err)
	}
	if pw := storedPassword(t, admin, ctx, renamed); !scram.Verifies(pw, "n3w") {
		t.Errorf("SetPassword stored %q", pw)
	}
	if err := got.ClearPassword(ctx); err != nil {
		t.Fatal(err)
	}
	var hasPassword bool
	if err := admin.QueryRow(ctx, `SELECT rolpassword IS NOT NULL FROM pg_catalog.pg_authid WHERE rolname = $1`, renamed).Scan(&hasPassword); err != nil || hasPassword {
		t.Errorf("after ClearPassword: has a password %v, %v", hasPassword, err)
	}

	// Parameters, every database's and this one's; written back through
	// Values identically.
	params := []struct {
		name   string
		values []string
	}{
		{"search_path", []string{`a "b"`, "$user", "o'brien", `back\slash`}},
		{"work_mem", []string{"64MB"}},
		{"pgmo.live_opt", []string{`x, "y"`}},
	}
	for _, p := range params {
		if err := errors.Join(got.SetParameter(ctx, p.name, p.values...),
			got.SetDatabaseParameter(ctx, db, p.name, p.values...)); err != nil {
			t.Fatalf("set %s: %v", p.name, err)
		}
	}
	r := reread()
	if len(r.Settings) != len(params) || len(r.DatabaseSettings) != 1 || r.DatabaseSettings[0].Database != db ||
		!slices.Equal(r.DatabaseSettings[0].Settings, r.Settings) {
		t.Fatalf("stored %q and %+v", r.Settings, r.DatabaseSettings)
	}
	for _, p := range params {
		i := slices.IndexFunc(r.Settings, func(sp ParameterSetting) bool { return sp.Name == p.name })
		if i < 0 || !slices.Equal(r.Settings[i].Values(), p.values) {
			t.Errorf("%s stored as %q; want %q", p.name, r.Settings, p.values)
		}
	}
	got.Settings, got.DatabaseSettings = r.Settings, r.DatabaseSettings
	if err := errors.Join(got.ResetAllParameters(ctx), got.ResetAllDatabaseParameters(ctx, db)); err != nil {
		t.Fatal(err)
	}
	if r := reread(); r.Settings != nil || r.DatabaseSettings != nil || got.Settings != nil || got.DatabaseSettings != nil {
		t.Errorf("after RESET ALL: read %q %+v, mirrored %q %+v", r.Settings, r.DatabaseSettings, got.Settings, got.DatabaseSettings)
	}
	for _, p := range r.Settings {
		if err := errors.Join(got.SetParameter(ctx, p.Name, p.Values()...),
			got.SetDatabaseParameter(ctx, db, p.Name, p.Values()...)); err != nil {
			t.Fatal(err)
		}
	}
	if again := reread(); !slices.Equal(again.Settings, r.Settings) {
		t.Errorf("written back through Values:\n got %q\nwant %q", again.Settings, r.Settings)
	}
	if err := errors.Join(got.ResetParameter(ctx, "WORK_MEM"), got.ResetDatabaseParameter(ctx, db, "work_mem")); err != nil {
		t.Fatal(err)
	}
	if r := reread(); slices.ContainsFunc(r.Settings, func(p ParameterSetting) bool { return p.Name == "work_mem" }) ||
		(r.DatabaseSettings != nil && slices.ContainsFunc(r.DatabaseSettings[0].Settings, func(p ParameterSetting) bool { return p.Name == "work_mem" })) {
		t.Errorf("work_mem still stored: %q %+v", r.Settings, r.DatabaseSettings)
	}
	if err := got.SetDatabaseParameter(ctx, fmt.Sprintf("pgmo_no_db_%d", stamp), "work_mem", "1MB"); !IsUndefinedObject(err) {
		t.Errorf("a setting in a database that does not exist: %v, want 3D000", err)
	}

	// Membership: grp granted to the role, each option the release has.
	g := s.RoleRef(grp)
	grant := func() RoleMembership {
		t.Helper()
		for _, m := range reread().MemberOf {
			if m.Role == grp {
				return m
			}
		}
		t.Fatalf("%q is no member of %q", renamed, grp)
		return RoleMembership{}
	}
	if err := g.AddMember(ctx, renamed, MembershipOptions{Admin: new(true)}); err != nil {
		t.Fatal(err)
	}
	if m := grant(); !m.Admin {
		t.Errorf("WITH ADMIN: %+v", m)
	}
	if v < PostgreSQL16 {
		for _, opts := range []MembershipOptions{{Admin: new(false)}, {Inherit: new(true)}, {Set: new(false)}} {
			if err := g.AddMember(ctx, renamed, opts); !errors.Is(err, ErrUnsupportedVersion) {
				t.Errorf("%+v on %s: %v, want ErrUnsupportedVersion", opts, v, err)
			}
		}
		if err := g.RemoveMember(ctx, renamed, RevokeMembershipOptions{Option: MembershipSet}); !errors.Is(err, ErrUnsupportedVersion) {
			t.Errorf("revoke SET on %s: %v, want ErrUnsupportedVersion", v, err)
		}
	} else {
		if err := g.AddMember(ctx, renamed, MembershipOptions{Admin: new(false), Inherit: new(false), Set: new(false)}); err != nil {
			t.Fatal(err)
		}
		if m := grant(); m.Admin || m.Inherit || m.Set {
			t.Errorf("WITH ADMIN FALSE, INHERIT FALSE, SET FALSE: %+v", m)
		}
		if err := g.AddMember(ctx, renamed, MembershipOptions{Inherit: new(true), Set: new(true)}); err != nil {
			t.Fatal(err)
		}
		if m := grant(); m.Admin || !m.Inherit || !m.Set {
			t.Errorf("WITH INHERIT TRUE, SET TRUE: %+v", m)
		}
		if err := errors.Join(g.RemoveMember(ctx, renamed, RevokeMembershipOptions{Option: MembershipInherit}),
			g.RemoveMember(ctx, renamed, RevokeMembershipOptions{Option: MembershipSet})); err != nil {
			t.Fatal(err)
		}
		if m := grant(); m.Inherit || m.Set {
			t.Errorf("after revoking INHERIT and SET: %+v", m)
		}
		if err := g.AddMember(ctx, renamed, MembershipOptions{Admin: new(true)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := g.RemoveMember(ctx, renamed, RevokeMembershipOptions{Option: MembershipAdmin, Cascade: true}); err != nil {
		t.Fatal(err)
	}
	if m := grant(); m.Admin {
		t.Errorf("after revoking ADMIN: %+v", m)
	}
	if err := s.RoleRef(renamed).AddMember(ctx, grp, MembershipOptions{}); pgErrCode(err) != "0LP01" {
		t.Errorf("a membership cycle: %v, want 0LP01", err)
	}
	if err := g.RemoveMember(ctx, renamed, RevokeMembershipOptions{}); err != nil {
		t.Fatal(err)
	}
	if slices.ContainsFunc(reread().MemberOf, func(m RoleMembership) bool { return m.Role == grp }) {
		t.Error("still a member after RemoveMember")
	}
	if err := g.RemoveMember(ctx, renamed, RevokeMembershipOptions{}); err != nil {
		t.Errorf("revoking a membership that is gone: %v", err)
	}

	// The server's own refusals.
	mon := s.RoleRef("pg_monitor")
	if err := mon.Drop(ctx); pgErrCode(err) != "2BP01" {
		t.Errorf("drop pg_monitor: %v, want 2BP01", err)
	}
	if err := mon.Rename(ctx, fmt.Sprintf("pgmo_live_rw_%d_mon", stamp)); pgErrCode(err) != "42939" {
		t.Errorf("rename pg_monitor: %v, want 42939", err)
	}
	if err := mon.Alter(ctx, RoleAttributes{CanLogin: new(true)}); pgErrCode(err) != "42939" {
		t.Errorf("alter pg_monitor: %v, want 42939", err)
	}
	if v >= PostgreSQL16 { // 15 takes it: never sent there
		var bootstrap string
		if err := admin.QueryRow(ctx, `SELECT rolname FROM pg_catalog.pg_roles WHERE oid = 10`).Scan(&bootstrap); err != nil {
			t.Fatal(err)
		}
		if err := s.RoleRef(bootstrap).Alter(ctx, RoleAttributes{Superuser: new(false)}); pgErrCode(err) != "0A000" {
			t.Errorf("NOSUPERUSER on the bootstrap superuser: %v, want 0A000", err)
		}
	}
	if _, err := s.CreateRole(ctx, CreateRoleRequest{Name: "pg_pgmo_live"}); pgErrCode(err) != "42939" {
		t.Errorf("create pg_pgmo_live: %v, want 42939", err)
	}
	if _, err := s.CreateRole(ctx, CreateRoleRequest{Name: grp}); !IsAlreadyExists(err) {
		t.Errorf("create %s again: %v, want 42710", grp, err)
	}
	if _, err := admin.Exec(ctx, "GRANT CONNECT ON DATABASE "+q(db)+" TO "+q(renamed)); err != nil {
		t.Fatal(err)
	}
	if err := got.Drop(ctx); pgErrCode(err) != "2BP01" {
		t.Errorf("drop of a role holding a privilege: %v, want 2BP01", err)
	}
	if _, err := admin.Exec(ctx, "REVOKE CONNECT ON DATABASE "+q(db)+" FROM "+q(renamed)); err != nil {
		t.Fatal(err)
	}
	if err := got.Drop(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RoleByName(ctx, renamed); !errors.Is(err, ErrNotFound) {
		t.Errorf("still there after the drop: %v", err)
	}
	if err := got.Drop(ctx); !IsUndefinedObject(err) {
		t.Errorf("drop of a role gone: %v, want 42704", err)
	}

	// A role write is transactional: rolled back with the transaction.
	err = s.InTransaction(ctx, func(ctx context.Context) error {
		r, err := s.CreateRole(ctx, CreateRoleRequest{Name: rolledBack, Password: "x"})
		if err != nil {
			return err
		}
		if r.OID == 0 {
			t.Error("the create did not read back inside the transaction")
		}
		return errors.Join(r.Alter(ctx, RoleAttributes{CanLogin: new(true)}), g.AddMember(ctx, rolledBack, MembershipOptions{}),
			errors.New("roll back"))
	})
	if err == nil {
		t.Fatal("the transaction committed")
	}
	if _, err := s.RoleByName(ctx, rolledBack); !errors.Is(err, ErrNotFound) {
		t.Errorf("a rolled-back create left %q: %v", rolledBack, err)
	}
}
