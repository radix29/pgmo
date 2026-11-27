package pgmo_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/radix29/pgmo"
	"github.com/radix29/pgmo/pgmotest"
)

// Every refusal is decided before anything is sent or captured, and reaches
// its sentinel: the caller's arguments ErrInvalidRequest, an option the
// server's release predates ErrUnsupportedVersion.
func TestRoleWriteRefusals(t *testing.T) {
	f := pgmotest.New(t)
	f.Version = pgmo.PostgreSQL15
	s := f.Server()
	r := s.RoleRef("app")
	none := s.RoleRef("")
	create := func(req pgmo.CreateRoleRequest) func(context.Context) error {
		return func(ctx context.Context) error { _, err := s.CreateRole(ctx, req); return err }
	}
	cases := []struct {
		name string
		call func(context.Context) error
		kind error
	}{
		{"create, no name", create(pgmo.CreateRoleRequest{}), pgmo.ErrInvalidRequest},
		{"create, limit below -1", create(pgmo.CreateRoleRequest{Name: "x", RoleAttributes: pgmo.RoleAttributes{ConnectionLimit: new(-2)}}), pgmo.ErrInvalidRequest},
		{"create, year 10000", create(pgmo.CreateRoleRequest{Name: "x", RoleAttributes: pgmo.RoleAttributes{ValidUntil: new(time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC))}}), pgmo.ErrInvalidRequest},
		{"create, year 0", create(pgmo.CreateRoleRequest{Name: "x", RoleAttributes: pgmo.RoleAttributes{ValidUntil: new(time.Date(0, 6, 1, 0, 0, 0, 0, time.UTC))}}), pgmo.ErrInvalidRequest},
		{"create, empty in-role", create(pgmo.CreateRoleRequest{Name: "x", InRole: []string{"a", ""}}), pgmo.ErrInvalidRequest},
		{"create, empty member", create(pgmo.CreateRoleRequest{Name: "x", Members: []string{""}}), pgmo.ErrInvalidRequest},
		{"create, empty admin", create(pgmo.CreateRoleRequest{Name: "x", AdminMembers: []string{""}}), pgmo.ErrInvalidRequest},
		{"create, NUL password", create(pgmo.CreateRoleRequest{Name: "x", Password: "a\x00b"}), pgmo.ErrInvalidRequest},
		{"drop, no name", none.Drop, pgmo.ErrInvalidRequest},
		{"rename, no new name", func(ctx context.Context) error { return r.Rename(ctx, "") }, pgmo.ErrInvalidRequest},
		{"rename, no name", func(ctx context.Context) error { return none.Rename(ctx, "y") }, pgmo.ErrInvalidRequest},
		{"alter, no name", func(ctx context.Context) error { return none.Alter(ctx, pgmo.RoleAttributes{CanLogin: new(true)}) }, pgmo.ErrInvalidRequest},
		{"alter, limit below -1", func(ctx context.Context) error { return r.Alter(ctx, pgmo.RoleAttributes{ConnectionLimit: new(-2)}) }, pgmo.ErrInvalidRequest},
		{"alter, year -1", func(ctx context.Context) error {
			return r.Alter(ctx, pgmo.RoleAttributes{ValidUntil: new(time.Date(-1, 1, 1, 0, 0, 0, 0, time.UTC))})
		}, pgmo.ErrInvalidRequest},
		{"password, empty", func(ctx context.Context) error { return r.SetPassword(ctx, "") }, pgmo.ErrInvalidRequest},
		{"password, no name", func(ctx context.Context) error { return none.SetPassword(ctx, "x") }, pgmo.ErrInvalidRequest},
		{"clear password, no name", none.ClearPassword, pgmo.ErrInvalidRequest},
		{"set, no parameter", func(ctx context.Context) error { return r.SetParameter(ctx, "", "x") }, pgmo.ErrInvalidRequest},
		{"set, no value", func(ctx context.Context) error { return r.SetParameter(ctx, "work_mem") }, pgmo.ErrInvalidRequest},
		{"set, no name", func(ctx context.Context) error { return none.SetParameter(ctx, "work_mem", "1MB") }, pgmo.ErrInvalidRequest},
		{"set in database, no database", func(ctx context.Context) error { return r.SetDatabaseParameter(ctx, "", "work_mem", "1MB") }, pgmo.ErrInvalidRequest},
		{"set in database, no value", func(ctx context.Context) error { return r.SetDatabaseParameter(ctx, "db", "work_mem") }, pgmo.ErrInvalidRequest},
		{"reset, no parameter", func(ctx context.Context) error { return r.ResetParameter(ctx, "") }, pgmo.ErrInvalidRequest},
		{"reset, no name", func(ctx context.Context) error { return none.ResetParameter(ctx, "work_mem") }, pgmo.ErrInvalidRequest},
		{"reset in database, no database", func(ctx context.Context) error { return r.ResetDatabaseParameter(ctx, "", "work_mem") }, pgmo.ErrInvalidRequest},
		{"reset all, no name", none.ResetAllParameters, pgmo.ErrInvalidRequest},
		{"reset all in database, no database", func(ctx context.Context) error { return r.ResetAllDatabaseParameters(ctx, "") }, pgmo.ErrInvalidRequest},
		{"reset all in database, no name", func(ctx context.Context) error { return none.ResetAllDatabaseParameters(ctx, "db") }, pgmo.ErrInvalidRequest},
		{"grant, no member", func(ctx context.Context) error { return r.AddMember(ctx, "", pgmo.MembershipOptions{}) }, pgmo.ErrInvalidRequest},
		{"grant, no name", func(ctx context.Context) error { return none.AddMember(ctx, "m", pgmo.MembershipOptions{}) }, pgmo.ErrInvalidRequest},
		{"grant, inherit on 15", func(ctx context.Context) error {
			return r.AddMember(ctx, "m", pgmo.MembershipOptions{Inherit: new(true)})
		}, pgmo.ErrUnsupportedVersion},
		{"grant, set on 15", func(ctx context.Context) error { return r.AddMember(ctx, "m", pgmo.MembershipOptions{Set: new(true)}) }, pgmo.ErrUnsupportedVersion},
		{"grant, admin false on 15", func(ctx context.Context) error {
			return r.AddMember(ctx, "m", pgmo.MembershipOptions{Admin: new(false)})
		}, pgmo.ErrUnsupportedVersion},
		{"revoke, no member", func(ctx context.Context) error { return r.RemoveMember(ctx, "", pgmo.RevokeMembershipOptions{}) }, pgmo.ErrInvalidRequest},
		{"revoke, no name", func(ctx context.Context) error { return none.RemoveMember(ctx, "m", pgmo.RevokeMembershipOptions{}) }, pgmo.ErrInvalidRequest},
		{"revoke, unknown option", func(ctx context.Context) error {
			return r.RemoveMember(ctx, "m", pgmo.RevokeMembershipOptions{Option: "admin"})
		}, pgmo.ErrInvalidRequest},
		{"revoke, inherit option on 15", func(ctx context.Context) error {
			return r.RemoveMember(ctx, "m", pgmo.RevokeMembershipOptions{Option: pgmo.MembershipInherit})
		}, pgmo.ErrUnsupportedVersion},
		{"revoke, set option on 15", func(ctx context.Context) error {
			return r.RemoveMember(ctx, "m", pgmo.RevokeMembershipOptions{Option: pgmo.MembershipSet})
		}, pgmo.ErrUnsupportedVersion},
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
				if err != nil && (!strings.HasPrefix(err.Error(), "pgmo: ") || strings.Count(err.Error(), "pgmo:") != 1) {
					t.Errorf("error %q lacks the one pgmo: prefix", err)
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

// The INHERIT and SET options, and ADMIN FALSE, are written from 16 on —
// whatever the release past it.
func TestRoleMembershipGatesFollowTheServer(t *testing.T) {
	for _, v := range []pgmo.ServerVersion{pgmo.PostgreSQL16, pgmo.PostgreSQL17, pgmo.PostgreSQL18} {
		f := pgmotest.New(t)
		f.Version = v
		r := f.Server().RoleRef("g")
		ctx, script := pgmo.WithScript(t.Context())
		if err := errors.Join(
			r.AddMember(ctx, "m", pgmo.MembershipOptions{Admin: new(false), Inherit: new(true), Set: new(false)}),
			r.RemoveMember(ctx, "m", pgmo.RevokeMembershipOptions{Option: pgmo.MembershipInherit}),
			r.RemoveMember(ctx, "m", pgmo.RevokeMembershipOptions{Option: pgmo.MembershipSet}),
		); err != nil {
			t.Errorf("%s: %v", v, err)
		}
		want := []string{"GRANT g TO m WITH ADMIN FALSE, INHERIT TRUE, SET FALSE",
			"REVOKE INHERIT OPTION FOR g FROM m", "REVOKE SET OPTION FOR g FROM m"}
		var got []string
		for _, e := range script.Entries() {
			got = append(got, e.SQL)
		}
		if !slices.Equal(got, want) {
			t.Errorf("%s: captured %q, want %q", v, got, want)
		}
	}
}

// Every role write may run in a transaction block: inside InTransaction each
// is sent on the transaction, the password's scram_iterations read too.
func TestRoleWritesInTransaction(t *testing.T) {
	f := pgmotest.New(t)
	f.On("current_setting('scram_iterations'", pgmotest.Rows([]any{"1"}))
	f.On("FROM pg_catalog.pg_roles r", roleRows())
	s := f.Server()
	r := s.RoleRef("app")
	before := len(f.Queries())
	err := s.InTransaction(t.Context(), func(ctx context.Context) error {
		_, err := s.CreateRole(ctx, pgmo.CreateRoleRequest{Name: "app", Password: "s3cr3t"})
		return errors.Join(err,
			r.Alter(ctx, pgmo.RoleAttributes{CanLogin: new(true)}),
			r.SetPassword(ctx, "s3cr3t"),
			r.ClearPassword(ctx),
			r.SetParameter(ctx, "work_mem", "1MB"),
			r.SetDatabaseParameter(ctx, "db", "work_mem", "1MB"),
			r.ResetParameter(ctx, "work_mem"),
			r.ResetDatabaseParameter(ctx, "db", "work_mem"),
			r.ResetAllParameters(ctx),
			r.ResetAllDatabaseParameters(ctx, "db"),
			r.AddMember(ctx, "m", pgmo.MembershipOptions{}),
			r.RemoveMember(ctx, "m", pgmo.RevokeMembershipOptions{}),
			r.Rename(ctx, "app2"),
			r.Drop(ctx),
		)
	})
	if err != nil {
		t.Fatal(err)
	}
	var stmts int
	for _, q := range f.Queries()[before:] {
		if q.Tx == 0 {
			t.Errorf("%q ran outside the transaction", q.Text)
		}
		if q.Kind == pgmotest.KindExec && q.Text != "begin" && q.Text != "commit" {
			stmts++
		}
	}
	if stmts != 14 {
		t.Errorf("sent %d statements on the transaction, want 14", stmts)
	}
}

// A write that ran is mirrored onto the handle — the attributes set, the
// name, the settings reset — and under WithScript it is not, since the
// server still holds the old state. Memberships and parameters set are not
// mirrored either way.
func TestRoleWritesMirrorOnlyWhenApplied(t *testing.T) {
	f := pgmotest.New(t)
	s := f.Server()
	at := time.Date(2027, 1, 2, 3, 4, 5, 123456789, time.UTC)
	mk := func() *pgmo.Role {
		r := s.RoleRef("app")
		r.Inherit, r.ConnectionLimit, r.ValidUntil = true, -1, until
		r.Settings = []pgmo.ParameterSetting{{Name: "work_mem", Value: "1MB"}}
		r.DatabaseSettings = []pgmo.DatabaseSettings{
			{Database: "db1", Settings: r.Settings}, {Database: "db2", Settings: r.Settings}}
		return r
	}
	apply := func(ctx context.Context, r *pgmo.Role, a pgmo.RoleAttributes) {
		t.Helper()
		if err := errors.Join(
			r.Alter(ctx, a),
			r.Rename(ctx, "app2"),
			r.ResetAllParameters(ctx),
			r.ResetAllDatabaseParameters(ctx, "db1"),
			r.SetParameter(ctx, "work_mem", "2MB"),
			r.AddMember(ctx, "m", pgmo.MembershipOptions{}),
		); err != nil {
			t.Fatal(err)
		}
	}
	all := pgmo.RoleAttributes{
		Superuser: new(true), Inherit: new(false), CreateRole: new(true), CreateDB: new(true),
		CanLogin: new(true), Replication: new(true), BypassRLS: new(true),
		ConnectionLimit: new(9), ValidUntil: &at,
	}

	scripted := mk()
	sctx, _ := pgmo.WithScript(t.Context())
	apply(sctx, scripted, all)
	if want := mk(); scripted.Name != "app" || scripted.Superuser || !scripted.Inherit || scripted.CanLogin ||
		scripted.ConnectionLimit != -1 || !scripted.ValidUntil.Equal(until) ||
		len(scripted.Settings) != 1 || len(scripted.DatabaseSettings) != len(want.DatabaseSettings) {
		t.Errorf("a scripted write was mirrored: %+v", scripted)
	}

	ran := mk()
	apply(t.Context(), ran, all)
	if !ran.Superuser || ran.Inherit || !ran.CreateRole || !ran.CreateDB || !ran.CanLogin || !ran.Replication || !ran.BypassRLS ||
		ran.ConnectionLimit != 9 || ran.Name != "app2" || ran.Settings != nil ||
		len(ran.DatabaseSettings) != 1 || ran.DatabaseSettings[0].Database != "db2" || ran.Members != nil {
		t.Errorf("a write that ran was not mirrored: %+v", ran)
	}
	if want := at.Truncate(time.Microsecond); !ran.ValidUntil.Equal(want) {
		t.Errorf("ValidUntil mirrored as %v, want %v as stored", ran.ValidUntil, want)
	}
	// Each statement after the rename names the new name.
	for _, q := range f.Queries()[2:] {
		if !strings.Contains(q.Text, "app2") {
			t.Errorf("sent %q, not naming the renamed role", q.Text)
		}
	}

	// Only what is set is mirrored: 'infinity' as the zero Time, the
	// last database's settings removed to nil, the rest left.
	one := mk()
	if err := errors.Join(one.Alter(t.Context(), pgmo.RoleAttributes{ValidUntil: new(time.Time{})}),
		one.ResetAllDatabaseParameters(t.Context(), "db1"), one.ResetAllDatabaseParameters(t.Context(), "db2")); err != nil {
		t.Fatal(err)
	}
	if !one.ValidUntil.IsZero() || !one.Inherit || one.ConnectionLimit != -1 || one.DatabaseSettings != nil || len(one.Settings) != 1 {
		t.Errorf("a one-field alter mirrored: %+v", one)
	}
}

// An empty RoleAttributes sends nothing, even on a handle with no name.
func TestRoleAlterNothingSendsNothing(t *testing.T) {
	f := pgmotest.New(t)
	if err := f.Server().RoleRef("").Alter(t.Context(), pgmo.RoleAttributes{}); err != nil {
		t.Fatal(err)
	}
	wantSent(t, f)
}

// CreateRole reads what it created back by its name; under WithScript it
// returns the handle, reading nothing; a read-back that finds nothing
// returns the handle too, while a create the server refused is the error,
// wrapped once.
func TestCreateRoleReadsBack(t *testing.T) {
	f := pgmotest.New(t)
	f.On("FROM pg_catalog.pg_roles r", roleRows(rowRoleApp))
	s := f.Server()
	got, err := s.CreateRole(t.Context(), pgmo.CreateRoleRequest{Name: "app"})
	if err != nil {
		t.Fatal(err)
	}
	if got.OID != 16400 || got.Comment != "the app" {
		t.Errorf("read back %+v, want the catalog's row", got)
	}
	if q := f.Queries(); len(q) < 3 || q[1].Text != "CREATE ROLE app" || q[2].Arg(0) != "app" {
		t.Errorf("sent %v, want CREATE ROLE then the read by its name", q)
	}

	sctx, _ := pgmo.WithScript(t.Context())
	n := len(f.Queries())
	h, err := s.CreateRole(sctx, pgmo.CreateRoleRequest{Name: "other"})
	if err != nil || h.Name != "other" || h.OID != 0 || len(f.Queries()) != n {
		t.Errorf("scripted create: %+v, %v, sent %d", h, err, len(f.Queries())-n)
	}

	f2 := pgmotest.New(t)
	f2.On("FROM pg_catalog.pg_roles r", roleRows())
	h, err = f2.Server().CreateRole(t.Context(), pgmo.CreateRoleRequest{Name: "hidden"})
	if err != nil || h.Name != "hidden" {
		t.Errorf("create read back as nothing: %+v, %v; want the handle", h, err)
	}

	f3 := pgmotest.New(t)
	f3.OnExec("CREATE ROLE", pgmotest.Fail(&pgconn.PgError{Code: "42710"}))
	_, err = f3.Server().CreateRole(t.Context(), pgmo.CreateRoleRequest{Name: "dup"})
	if !pgmo.IsAlreadyExists(err) || strings.Count(err.Error(), "pgmo:") != 1 ||
		!strings.HasPrefix(err.Error(), `pgmo: create role "dup": `) {
		t.Errorf("refused create: %v", err)
	}
}
