package pgmo_test

import (
	"context"
	"testing"
	"time"

	"github.com/radix29/pgmo"
	"github.com/radix29/pgmo/pgmotest"
)

// The role writes' statements, whole, with quote-hostile names and values
// through every parameter that reaches the text (see
// script_database_write_test.go). The password's own statements are pinned
// in secret_redaction_test.go; here the placeholder stands where it goes.
func TestScriptRoleWrites(t *testing.T) {
	f := pgmotest.New(t)
	s := f.Server()
	r := s.RoleRef(`My "Role"`)
	at := time.Date(2027, 1, 2, 3, 4, 5, 123456789, time.FixedZone("CET", 3600))

	runScriptCases(t, f, []scriptCase{
		{"create, name only", func(ctx context.Context) error {
			_, err := s.CreateRole(ctx, pgmo.CreateRoleRequest{Name: `My "Role"`})
			return err
		}, `CREATE ROLE "My ""Role"""`},
		{"create, every option on", func(ctx context.Context) error {
			_, err := s.CreateRole(ctx, pgmo.CreateRoleRequest{
				Name: "user",
				RoleAttributes: pgmo.RoleAttributes{
					Superuser: new(true), Inherit: new(true), CreateRole: new(true), CreateDB: new(true),
					CanLogin: new(true), Replication: new(true), BypassRLS: new(true),
					ConnectionLimit: new(3), ValidUntil: &at,
				},
				Password:     "s3cr3t",
				InRole:       []string{"o'brien", `G "1"`},
				Members:      []string{"user"},
				AdminMembers: []string{`a\b`, "Boss"},
			})
			return err
		}, `CREATE ROLE "user" WITH SUPERUSER INHERIT CREATEROLE CREATEDB LOGIN REPLICATION BYPASSRLS` +
			` CONNECTION LIMIT 3 PASSWORD <insert password here> VALID UNTIL '2027-01-02 02:04:05.123456Z'` +
			` IN ROLE "o'brien", "G ""1""" ROLE "user" ADMIN "a\b", "Boss"`},
		{"create, every option off", func(ctx context.Context) error {
			_, err := s.CreateRole(ctx, pgmo.CreateRoleRequest{
				Name: "app",
				RoleAttributes: pgmo.RoleAttributes{
					Superuser: new(false), Inherit: new(false), CreateRole: new(false), CreateDB: new(false),
					CanLogin: new(false), Replication: new(false), BypassRLS: new(false),
					ConnectionLimit: new(-1), ValidUntil: new(time.Time{}),
				},
			})
			return err
		}, `CREATE ROLE app WITH NOSUPERUSER NOINHERIT NOCREATEROLE NOCREATEDB NOLOGIN NOREPLICATION NOBYPASSRLS` +
			` CONNECTION LIMIT -1 VALID UNTIL 'infinity'`},
		{"create, password alone", func(ctx context.Context) error {
			_, err := s.CreateRole(ctx, pgmo.CreateRoleRequest{Name: "app", Password: "x"})
			return err
		}, `CREATE ROLE app WITH PASSWORD <insert password here>`},
		{"create, password and login", func(ctx context.Context) error {
			_, err := s.CreateRole(ctx, pgmo.CreateRoleRequest{Name: "app", Password: "x",
				RoleAttributes: pgmo.RoleAttributes{CanLogin: new(true)}})
			return err
		}, `CREATE ROLE app WITH LOGIN PASSWORD <insert password here>`},
		{"create, members only", func(ctx context.Context) error {
			_, err := s.CreateRole(ctx, pgmo.CreateRoleRequest{Name: "app", Members: []string{"a"}})
			return err
		}, `CREATE ROLE app WITH ROLE a`},
		{"drop", func(ctx context.Context) error {
			return r.Drop(ctx)
		}, `DROP ROLE "My ""Role"""`},
		{"rename", func(ctx context.Context) error {
			return s.RoleRef(`My "Role"`).Rename(ctx, "user")
		}, `ALTER ROLE "My ""Role""" RENAME TO "user"`},
		{"alter, one attribute", func(ctx context.Context) error {
			return r.Alter(ctx, pgmo.RoleAttributes{CanLogin: new(true)})
		}, `ALTER ROLE "My ""Role""" WITH LOGIN`},
		{"alter, every attribute", func(ctx context.Context) error {
			return r.Alter(ctx, pgmo.RoleAttributes{
				Superuser: new(false), Inherit: new(true), CreateRole: new(false), CreateDB: new(true),
				CanLogin: new(false), Replication: new(true), BypassRLS: new(false),
				ConnectionLimit: new(0), ValidUntil: &pgmo.ValidUntilNegativeInfinity,
			})
		}, `ALTER ROLE "My ""Role""" WITH NOSUPERUSER INHERIT NOCREATEROLE CREATEDB NOLOGIN REPLICATION NOBYPASSRLS` +
			` CONNECTION LIMIT 0 VALID UNTIL '-infinity'`},
		{"alter, valid until in UTC", func(ctx context.Context) error {
			return r.Alter(ctx, pgmo.RoleAttributes{ValidUntil: new(time.Date(1, 1, 2, 0, 0, 0, 0, time.UTC))})
		}, `ALTER ROLE "My ""Role""" WITH VALID UNTIL '0001-01-02 00:00:00Z'`},
		{"set password", func(ctx context.Context) error {
			return r.SetPassword(ctx, "s3cr3t")
		}, `ALTER ROLE "My ""Role""" PASSWORD <insert password here>`},
		{"clear password", func(ctx context.Context) error {
			return r.ClearPassword(ctx)
		}, `ALTER ROLE "My ""Role""" PASSWORD NULL`},
		{"set a parameter", func(ctx context.Context) error {
			return r.SetParameter(ctx, "work_mem", "64MB")
		}, `ALTER ROLE "My ""Role""" SET work_mem = '64MB'`},
		{"set a list parameter", func(ctx context.Context) error {
			return r.SetParameter(ctx, "search_path", "o'brien", "$user", `a\b`)
		}, `ALTER ROLE "My ""Role""" SET search_path = 'o''brien', '$user', E'a\\b'`},
		{"set a parameter in a database", func(ctx context.Context) error {
			return r.SetDatabaseParameter(ctx, `My "DB"`, "myext.Option", "on")
		}, `ALTER ROLE "My ""Role""" IN DATABASE "My ""DB""" SET "myext.Option" = 'on'`},
		{"reset a parameter", func(ctx context.Context) error {
			return r.ResetParameter(ctx, "DateStyle")
		}, `ALTER ROLE "My ""Role""" RESET "DateStyle"`},
		{"reset a parameter in a database", func(ctx context.Context) error {
			return r.ResetDatabaseParameter(ctx, "user", "work_mem")
		}, `ALTER ROLE "My ""Role""" IN DATABASE "user" RESET work_mem`},
		{"reset every parameter", func(ctx context.Context) error {
			return r.ResetAllParameters(ctx)
		}, `ALTER ROLE "My ""Role""" RESET ALL`},
		{"reset every parameter in a database", func(ctx context.Context) error {
			return r.ResetAllDatabaseParameters(ctx, "o'brien")
		}, `ALTER ROLE "My ""Role""" IN DATABASE "o'brien" RESET ALL`},
		{"grant", func(ctx context.Context) error {
			return r.AddMember(ctx, "user", pgmo.MembershipOptions{})
		}, `GRANT "My ""Role""" TO "user"`},
		{"grant, every option", func(ctx context.Context) error {
			return r.AddMember(ctx, `M "1"`, pgmo.MembershipOptions{Admin: new(true), Inherit: new(false), Set: new(true), GrantedBy: "user"})
		}, `GRANT "My ""Role""" TO "M ""1""" WITH ADMIN TRUE, INHERIT FALSE, SET TRUE GRANTED BY "user"`},
		{"grant, admin false", func(ctx context.Context) error {
			return r.AddMember(ctx, "m", pgmo.MembershipOptions{Admin: new(false)})
		}, `GRANT "My ""Role""" TO m WITH ADMIN FALSE`},
		{"grant, set alone", func(ctx context.Context) error {
			return r.AddMember(ctx, "m", pgmo.MembershipOptions{Set: new(false)})
		}, `GRANT "My ""Role""" TO m WITH SET FALSE`},
		{"revoke", func(ctx context.Context) error {
			return r.RemoveMember(ctx, "user", pgmo.RevokeMembershipOptions{})
		}, `REVOKE "My ""Role""" FROM "user"`},
		{"revoke, every choice", func(ctx context.Context) error {
			return r.RemoveMember(ctx, "m", pgmo.RevokeMembershipOptions{GrantedBy: `G "1"`, Cascade: true})
		}, `REVOKE "My ""Role""" FROM m GRANTED BY "G ""1""" CASCADE`},
		{"revoke admin option", func(ctx context.Context) error {
			return r.RemoveMember(ctx, "m", pgmo.RevokeMembershipOptions{Option: pgmo.MembershipAdmin})
		}, `REVOKE ADMIN OPTION FOR "My ""Role""" FROM m`},
		{"revoke inherit option", func(ctx context.Context) error {
			return r.RemoveMember(ctx, "m", pgmo.RevokeMembershipOptions{Option: pgmo.MembershipInherit, Cascade: true})
		}, `REVOKE INHERIT OPTION FOR "My ""Role""" FROM m CASCADE`},
		{"revoke set option", func(ctx context.Context) error {
			return r.RemoveMember(ctx, "m", pgmo.RevokeMembershipOptions{Option: pgmo.MembershipSet})
		}, `REVOKE SET OPTION FOR "My ""Role""" FROM m`},
	})
}

// On 15 GRANT knows only WITH ADMIN OPTION, and REVOKE only ADMIN OPTION
// FOR.
func TestScriptRoleMembershipOn15(t *testing.T) {
	f := pgmotest.New(t)
	f.Version = pgmo.PostgreSQL15
	s := f.Server()
	r := s.RoleRef(`My "Role"`)
	runScriptCases(t, f, []scriptCase{
		{"grant", func(ctx context.Context) error {
			return r.AddMember(ctx, "m", pgmo.MembershipOptions{})
		}, `GRANT "My ""Role""" TO m`},
		{"grant with admin", func(ctx context.Context) error {
			return r.AddMember(ctx, "m", pgmo.MembershipOptions{Admin: new(true), GrantedBy: "user"})
		}, `GRANT "My ""Role""" TO m WITH ADMIN OPTION GRANTED BY "user"`},
		{"revoke admin option", func(ctx context.Context) error {
			return r.RemoveMember(ctx, "m", pgmo.RevokeMembershipOptions{Option: pgmo.MembershipAdmin, GrantedBy: "user", Cascade: true})
		}, `REVOKE ADMIN OPTION FOR "My ""Role""" FROM m GRANTED BY "user" CASCADE`},
	})
}

// REASSIGN OWNED and DROP OWNED, whole, with quote-hostile role names,
// captured as run in the database they act in; the refusals send nothing.
func TestScriptOwnedWrites(t *testing.T) {
	f := pgmotest.New(t)
	d := f.Server().DatabaseRef(hostileDB)
	runScriptCasesIn(t, f, hostileDB, []scriptCase{
		{"reassign, one role", func(ctx context.Context) error {
			return d.ReassignOwned(ctx, []string{`My "Role"`}, "o'brien")
		}, `REASSIGN OWNED BY "My ""Role""" TO "o'brien"`},
		{"reassign, several roles", func(ctx context.Context) error {
			return d.ReassignOwned(ctx, []string{"a", `B\c`, "user"}, "New Owner")
		}, `REASSIGN OWNED BY a, "B\c", "user" TO "New Owner"`},
		{"drop, restrict", func(ctx context.Context) error {
			return d.DropOwned(ctx, []string{`My "Role"`, "b"}, pgmo.DropOptions{})
		}, `DROP OWNED BY "My ""Role""", b`},
		{"drop, cascade", func(ctx context.Context) error {
			return d.DropOwned(ctx, []string{"a"}, pgmo.DropOptions{Cascade: true})
		}, `DROP OWNED BY a CASCADE`},
	})

	inv := pgmo.ErrInvalidRequest
	runRefusals(t, f, []refusalCase{
		{"reassign, no role", func(ctx context.Context) error { return d.ReassignOwned(ctx, nil, "o") }, inv},
		{"reassign, an empty role", func(ctx context.Context) error { return d.ReassignOwned(ctx, []string{"a", ""}, "o") }, inv},
		{"reassign, no new owner", func(ctx context.Context) error { return d.ReassignOwned(ctx, []string{"a"}, "") }, inv},
		{"drop, no role", func(ctx context.Context) error { return d.DropOwned(ctx, []string{}, pgmo.DropOptions{}) }, inv},
		{"drop, an empty role", func(ctx context.Context) error { return d.DropOwned(ctx, []string{""}, pgmo.DropOptions{}) }, inv},
	})
}
