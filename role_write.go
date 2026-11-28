package pgmo

import (
	"context"
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Role writes
// ============================================================
//
// role_write.go is P4.13.2: CREATE ROLE, DROP ROLE, the ALTER ROLE forms —
// WITH <attributes>, RENAME TO, PASSWORD (through secret.go), SET/RESET
// with and without IN DATABASE — and GRANT/REVOKE of membership. A role is
// a cluster-wide object, so every statement runs on the Server's own
// database (s.exec, s.execSecret), and all of them may run in a transaction
// block.
//
// What the server refuses on its own is left to it, with its own SQLSTATE:
// a predefined role altered, renamed or dropped (42939, 2BP01), a pg_ name
// created (42939), the session or current user renamed (0A000), a role
// still owning objects or holding privileges dropped (2BP01). pgmo refuses
// before sending only what no server takes, and what this server's release
// cannot write: a membership's INHERIT and SET options, and WITH ADMIN
// FALSE, are 16+ (on 15 GRANT knows only WITH ADMIN OPTION).

// RoleAttributes are the attributes CREATE ROLE and ALTER ROLE … WITH set:
// each non-nil field is written, as SUPERUSER or NOSUPERUSER and so on; a
// nil one is left out — the server's default on a create (only INHERIT is
// on by default), unchanged on an alter. They are Role's fields of the same
// names.
type RoleAttributes struct {
	Superuser   *bool // SUPERUSER: the role bypasses every check; a superuser grants it
	Inherit     *bool // INHERIT: the default of the INHERIT option on grants it receives (16+), or whether it inherits at all (15)
	CreateRole  *bool // CREATEROLE: the role may create, alter and drop roles
	CreateDB    *bool // CREATEDB: the role may create databases
	CanLogin    *bool // LOGIN: the role may connect
	Replication *bool // REPLICATION: the role may stream replication; a superuser grants it
	BypassRLS   *bool // BYPASSRLS: row-level security policies do not apply to it; a superuser grants it

	// ConnectionLimit is CONNECTION LIMIT, -1 for none.
	ConnectionLimit *int

	// ValidUntil is VALID UNTIL: when the role's password stops logging it
	// in. The zero Time writes 'infinity' (no expiry, as Role.ValidUntil
	// reads it), ValidUntilNegativeInfinity '-infinity'; any other time is
	// written in UTC to the microsecond (finer is truncated), and must lie
	// in years 1 to 9999.
	ValidUntil *time.Time
}

// options are the attributes set, as ALTER ROLE … WITH and CREATE ROLE
// write them, in pg_dumpall's order, or the refusal of a value no server
// takes. No prefix: the caller names the operation.
func (a RoleAttributes) options() ([]string, error) {
	var opts []string
	flag := func(v *bool, on string) {
		if v != nil {
			if *v {
				opts = append(opts, on)
			} else {
				opts = append(opts, "NO"+on)
			}
		}
	}
	flag(a.Superuser, "SUPERUSER")
	flag(a.Inherit, "INHERIT")
	flag(a.CreateRole, "CREATEROLE")
	flag(a.CreateDB, "CREATEDB")
	flag(a.CanLogin, "LOGIN")
	flag(a.Replication, "REPLICATION")
	flag(a.BypassRLS, "BYPASSRLS")
	if a.ConnectionLimit != nil {
		if *a.ConnectionLimit < -1 {
			return nil, invalidf("connection limit %d; it is -1 (none) or more", *a.ConnectionLimit)
		}
		opts = append(opts, "CONNECTION LIMIT "+strconv.Itoa(*a.ConnectionLimit))
	}
	if a.ValidUntil != nil {
		lit, err := validUntilLiteral(*a.ValidUntil)
		if err != nil {
			return nil, err
		}
		opts = append(opts, "VALID UNTIL "+lit)
	}
	return opts, nil
}

// validUntilLiteral is t as VALID UNTIL's literal (see
// RoleAttributes.ValidUntil).
func validUntilLiteral(t time.Time) (string, error) {
	switch {
	case t.IsZero():
		return "'infinity'", nil
	case t.Equal(ValidUntilNegativeInfinity):
		return "'-infinity'", nil
	}
	u := t.UTC()
	if y := u.Year(); y < 1 || y > 9999 {
		return "", invalidf("valid until %s: the year is outside 1 to 9999", u.Format(time.RFC3339))
	}
	return pgsql.QuoteLiteral(u.Format("2006-01-02 15:04:05.999999Z07:00")), nil
}

// mirror copies the attributes set onto r (not under WithScript).
func (a RoleAttributes) mirror(ctx context.Context, r *Role) {
	setPtrIfApplied(ctx, &r.Superuser, a.Superuser)
	setPtrIfApplied(ctx, &r.Inherit, a.Inherit)
	setPtrIfApplied(ctx, &r.CreateRole, a.CreateRole)
	setPtrIfApplied(ctx, &r.CreateDB, a.CreateDB)
	setPtrIfApplied(ctx, &r.CanLogin, a.CanLogin)
	setPtrIfApplied(ctx, &r.Replication, a.Replication)
	setPtrIfApplied(ctx, &r.BypassRLS, a.BypassRLS)
	setPtrIfApplied(ctx, &r.ConnectionLimit, a.ConnectionLimit)
	if a.ValidUntil != nil {
		// As stored: to the microsecond. The zero Time and the
		// -infinity sentinel are themselves already.
		setIfApplied(ctx, &r.ValidUntil, a.ValidUntil.Truncate(time.Microsecond))
	}
}

// CreateRoleRequest describes a new role. Only Name is required; every
// attribute left nil is the server's default (NOLOGIN, INHERIT, no limit, no
// password, nothing else).
type CreateRoleRequest struct {
	Name string
	RoleAttributes

	// Password is the role's password, sent as its SCRAM-SHA-256 verifier
	// (never in the clear, as SetPassword); "" sets none.
	Password string

	// InRole are the roles the new role becomes a member of (IN ROLE);
	// Members the roles that become members of it (ROLE), and AdminMembers
	// the ones that become members WITH ADMIN OPTION (ADMIN). The grants are
	// recorded as made by the connected role (by the bootstrap superuser
	// for a superuser, from 16).
	InRole       []string
	Members      []string
	AdminMembers []string
}

// createRoleStatement is req's CREATE ROLE with password — a PASSWORD
// clause, or "" for none — in its place, or the refusal of a field no server
// takes. No prefix: the caller names the operation.
func createRoleStatement(req CreateRoleRequest, password string) (string, error) {
	opts, err := req.options()
	if err != nil {
		return "", err
	}
	if password != "" {
		// PASSWORD before VALID UNTIL, as pg_dumpall writes them.
		at := len(opts)
		if req.ValidUntil != nil {
			at--
		}
		opts = slices.Insert(opts, at, password)
	}
	for _, list := range []struct {
		clause string
		roles  []string
	}{{"IN ROLE", req.InRole}, {"ROLE", req.Members}, {"ADMIN", req.AdminMembers}} {
		if len(list.roles) == 0 {
			continue
		}
		names, err := roleList(list.roles)
		if err != nil {
			return "", fmt.Errorf("%s: %w", list.clause, err)
		}
		opts = append(opts, list.clause+" "+names)
	}
	stmt := "CREATE ROLE " + pgsql.QuoteIdent(req.Name)
	if len(opts) > 0 {
		stmt += " WITH " + strings.Join(opts, " ")
	}
	return stmt, nil
}

// roleList is roles as a comma-separated list of names, or the refusal of
// an empty one.
func roleList(roles []string) (string, error) {
	quoted := make([]string, len(roles))
	for i, r := range roles {
		if r == "" {
			return "", invalidf("an empty role name")
		}
		quoted[i] = pgsql.QuoteIdent(r)
	}
	return strings.Join(quoted, ", "), nil
}

// CreateRole creates the role req describes, and returns it read back from
// the catalog — or, under Scripting(ctx), the RoleRef handle, since nothing
// ran. A Password is sent as its verifier, through the same path as
// SetPassword: a WithScript collector captures PasswordPlaceholder in its
// place unless ctx carries WithScriptSecrets.
//
// The role needs CREATEROLE (and, from 16, ADMIN on every role it grants
// membership in), or superuser for SUPERUSER, REPLICATION and BYPASSRLS. A
// name starting pg_ is reserved: the server refuses it (42939).
func (s *Server) CreateRole(ctx context.Context, req CreateRoleRequest) (*Role, error) {
	if req.Name == "" {
		return nil, invalidf("pgmo: create role: no name")
	}
	fail := func(err error) error { return fmt.Errorf("pgmo: create role %q: %w", req.Name, err) }
	shown, err := createRoleStatement(req, "")
	if err != nil {
		return nil, fail(err)
	}
	if req.Password == "" {
		if _, err := s.exec(ctx, shown); err != nil {
			return nil, fail(err)
		}
	} else {
		sentClause, shownClause, err := s.passwordClause(ctx, req.Password)
		if err != nil {
			return nil, fail(err)
		}
		// Both statements from the one function, so they differ only by
		// the clause; the sent one is not built when it is never used.
		shown, _ = createRoleStatement(req, shownClause)
		sent := ""
		if sentClause != "" {
			sent, _ = createRoleStatement(req, sentClause)
		}
		if _, err := s.execSecret(ctx, sent, shown); err != nil {
			return nil, fail(err)
		}
	}
	return createdObject(ctx, s.RoleRef(req.Name), func() (*Role, error) {
		return s.RoleByName(ctx, req.Name)
	})
}

// Drop drops the role: DROP ROLE. The server refuses a role that still owns
// objects or holds privileges in any database (2BP01, naming them in its
// detail) — REASSIGN OWNED and DROP OWNED in each database clear them first
// — a predefined role and the bootstrap superuser (2BP01), and the session
// or current user (55006). Memberships are dropped with the role.
func (r *Role) Drop(ctx context.Context) error {
	if r.Name == "" {
		return invalidf("pgmo: drop role: no name")
	}
	if _, err := r.server.exec(ctx, "DROP ROLE "+pgsql.QuoteIdent(r.Name)); err != nil {
		return fmt.Errorf("pgmo: drop role %q: %w", r.Name, err)
	}
	return nil
}

// Rename renames the role: ALTER ROLE … RENAME TO, mirrored onto Name (not
// under WithScript). The server refuses the session or current user
// (0A000) and a predefined role or a pg_ name (42939). An MD5 password is
// salted with the role's name, so a rename clears one (the server says so
// in a notice); a SCRAM password, the only kind pgmo writes, stays.
func (r *Role) Rename(ctx context.Context, newName string) error {
	if r.Name == "" || newName == "" {
		return invalidf("pgmo: rename role: both names are required")
	}
	if _, err := r.server.exec(ctx, "ALTER ROLE "+pgsql.QuoteIdent(r.Name)+" RENAME TO "+pgsql.QuoteIdent(newName)); err != nil {
		return fmt.Errorf("pgmo: rename role %q to %q: %w", r.Name, newName, err)
	}
	setIfApplied(ctx, &r.Name, newName)
	return nil
}

// Alter applies every attribute set on a in one ALTER ROLE … WITH, mirrored
// onto the matching fields (not under WithScript). An empty a sends nothing.
// A connection limit below -1 or a ValidUntil outside years 1 to 9999 is
// refused with ErrInvalidRequest. The password is SetPassword's and
// ClearPassword's. The server refuses a predefined role (42939), and from 16
// NOSUPERUSER on the bootstrap superuser (0A000) — 15 takes it.
func (r *Role) Alter(ctx context.Context, a RoleAttributes) error {
	opts, err := a.options()
	if err != nil {
		return fmt.Errorf("pgmo: alter role %q: %w", r.Name, err)
	}
	if len(opts) == 0 {
		return nil
	}
	if r.Name == "" {
		return invalidf("pgmo: alter role: no name")
	}
	if _, err := r.server.exec(ctx, "ALTER ROLE "+pgsql.QuoteIdent(r.Name)+" WITH "+strings.Join(opts, " ")); err != nil {
		return fmt.Errorf("pgmo: alter role %q: %w", r.Name, err)
	}
	a.mirror(ctx, r)
	return nil
}

// SetPassword sets the role's password: ALTER ROLE … PASSWORD with the
// password's SCRAM-SHA-256 verifier, computed here at the session's
// scram_iterations, so the cleartext never reaches the server (see
// passwordVerifier). Under WithScript the collector captures
// PasswordPlaceholder in its place unless ctx carries WithScriptSecrets, and
// nothing is read. An empty password, or one holding NUL, is refused with
// ErrInvalidRequest: clearing a password is ClearPassword.
//
// A role may set its own password; another's needs CREATEROLE with ADMIN on
// it (16+), or superuser for a superuser's.
func (r *Role) SetPassword(ctx context.Context, password string) error {
	return r.server.alterRolePassword(ctx, r.Name, password)
}

// ClearPassword removes the role's password (ALTER ROLE … PASSWORD NULL):
// password authentication then always fails for it.
func (r *Role) ClearPassword(ctx context.Context) error {
	if r.Name == "" {
		return invalidf("pgmo: clear role password: no name")
	}
	if _, err := r.server.exec(ctx, "ALTER ROLE "+pgsql.QuoteIdent(r.Name)+" PASSWORD NULL"); err != nil {
		return fmt.Errorf("pgmo: clear role %q password: %w", r.Name, err)
	}
	return nil
}

// ------------------------------------------------------------
// Parameter settings: ALTER ROLE … [IN DATABASE …] SET / RESET
// ------------------------------------------------------------

// roleSettingTarget is "ALTER ROLE role" or "ALTER ROLE role IN DATABASE
// database", the head every role SET/RESET shares.
func roleSettingTarget(role, database string) string {
	head := "ALTER ROLE " + pgsql.QuoteIdent(role)
	if database != "" {
		head += " IN DATABASE " + pgsql.QuoteIdent(database)
	}
	return head
}

// setRoleParameter is SetParameter and SetDatabaseParameter; database ""
// is every database.
func (r *Role) setRoleParameter(ctx context.Context, op, database, name string, values []string) error {
	if r.Name == "" || name == "" {
		return invalidf("pgmo: %s: both the role and the parameter are required", op)
	}
	if len(values) == 0 {
		return invalidf("pgmo: %s: role %q parameter %q: no value; resetting removes one", op, r.Name, name)
	}
	stmt := roleSettingTarget(r.Name, database) + " SET " + parameterAssignment(name, values)
	if _, err := r.server.exec(ctx, stmt); err != nil {
		return fmt.Errorf("pgmo: %s: role %q parameter %q: %w", op, r.Name, name, err)
	}
	return nil
}

// SetParameter stores a default for configuration parameter name, applied
// to every session of the role, in every database: ALTER ROLE … SET name =
// 'value'[, …]. The values are written as Database.SetParameter writes them
// — each a literal, several a list's elements — so a stored setting writes
// back with SetParameter(ctx, p.Name, p.Values()...). Settings is not
// mirrored (the server canonicalises). A role may set its own (but not a
// superuser-only parameter); another's needs CREATEROLE with ADMIN on it
// (16+), or superuser.
func (r *Role) SetParameter(ctx context.Context, name string, values ...string) error {
	return r.setRoleParameter(ctx, "set role parameter", "", name, values)
}

// SetDatabaseParameter is SetParameter for the role's sessions in database
// alone: ALTER ROLE … IN DATABASE … SET. It outranks both the role's own
// default and the database's. A database that does not exist is the
// server's 3D000.
func (r *Role) SetDatabaseParameter(ctx context.Context, database, name string, values ...string) error {
	if database == "" {
		return invalidf("pgmo: set role database parameter: no database")
	}
	return r.setRoleParameter(ctx, "set role parameter in database "+strconv.Quote(database), database, name, values)
}

// resetRoleParameter is ResetParameter and ResetDatabaseParameter.
func (r *Role) resetRoleParameter(ctx context.Context, op, database, name string) error {
	if r.Name == "" || name == "" {
		return invalidf("pgmo: %s: both the role and the parameter are required", op)
	}
	if _, err := r.server.exec(ctx, roleSettingTarget(r.Name, database)+" RESET "+pgsql.QuoteIdent(name)); err != nil {
		return fmt.Errorf("pgmo: %s: role %q parameter %q: %w", op, r.Name, name, err)
	}
	return nil
}

// ResetParameter removes the role's default for parameter name in every
// database (ALTER ROLE … RESET name); one not stored succeeds. Settings is
// not mirrored.
func (r *Role) ResetParameter(ctx context.Context, name string) error {
	return r.resetRoleParameter(ctx, "reset role parameter", "", name)
}

// ResetDatabaseParameter removes the role's default for parameter name in
// database (ALTER ROLE … IN DATABASE … RESET name). DatabaseSettings is not
// mirrored.
func (r *Role) ResetDatabaseParameter(ctx context.Context, database, name string) error {
	if database == "" {
		return invalidf("pgmo: reset role database parameter: no database")
	}
	return r.resetRoleParameter(ctx, "reset role parameter in database "+strconv.Quote(database), database, name)
}

// ResetAllParameters removes every default stored for the role in every
// database (ALTER ROLE … RESET ALL), mirrored onto Settings (not under
// WithScript). The ones stored for one database (DatabaseSettings) stay.
func (r *Role) ResetAllParameters(ctx context.Context) error {
	if r.Name == "" {
		return invalidf("pgmo: reset role parameters: no name")
	}
	if _, err := r.server.exec(ctx, roleSettingTarget(r.Name, "")+" RESET ALL"); err != nil {
		return fmt.Errorf("pgmo: reset role %q parameters: %w", r.Name, err)
	}
	setIfApplied(ctx, &r.Settings, nil)
	return nil
}

// ResetAllDatabaseParameters removes every default stored for the role in
// database (ALTER ROLE … IN DATABASE … RESET ALL), mirrored by dropping
// database's entry from DatabaseSettings (not under WithScript).
func (r *Role) ResetAllDatabaseParameters(ctx context.Context, database string) error {
	if r.Name == "" || database == "" {
		return invalidf("pgmo: reset role database parameters: both the role and the database are required")
	}
	if _, err := r.server.exec(ctx, roleSettingTarget(r.Name, database)+" RESET ALL"); err != nil {
		return fmt.Errorf("pgmo: reset role %q parameters in database %q: %w", r.Name, database, err)
	}
	if !Scripting(ctx) {
		r.DatabaseSettings = slices.DeleteFunc(r.DatabaseSettings, func(d DatabaseSettings) bool { return d.Database == database })
		if len(r.DatabaseSettings) == 0 {
			r.DatabaseSettings = nil
		}
	}
	return nil
}

// ------------------------------------------------------------
// Membership: GRANT role TO member / REVOKE role FROM member
// ------------------------------------------------------------

// MembershipOptions are AddMember's choices: each option set is written,
// the rest left out. A new grant's unset options are the server's defaults
// — no ADMIN, INHERIT from the member's INHERIT attribute, SET true; on an
// existing grant by the same grantor (16+) only the options set change.
type MembershipOptions struct {
	// Admin is the ADMIN option: the member may grant the role to others
	// and revoke it. On 15 only true can be written (WITH ADMIN OPTION);
	// false is refused with ErrUnsupportedVersion — remove the option with
	// RemoveMember and MembershipAdmin.
	Admin *bool
	// Inherit is the INHERIT option (16+): the member holds the role's
	// privileges without SET ROLE.
	Inherit *bool
	// Set is the SET option (16+): the member may SET ROLE to the role.
	Set *bool
	// GrantedBy records the grant as made by this role (GRANTED BY); "" is
	// the connected role. From 16 the named role must hold ADMIN on the
	// role itself — a superuser's grant is recorded as the bootstrap
	// superuser's.
	GrantedBy string
}

// MembershipOption names one option of a membership grant, for RemoveMember
// to revoke alone; "" is the membership itself.
type MembershipOption string

const (
	// MembershipAdmin is the ADMIN option (see MembershipOptions.Admin).
	MembershipAdmin MembershipOption = "ADMIN"
	// MembershipInherit is the INHERIT option, 16+.
	MembershipInherit MembershipOption = "INHERIT"
	// MembershipSet is the SET option, 16+.
	MembershipSet MembershipOption = "SET"
)

// RevokeMembershipOptions are RemoveMember's choices.
type RevokeMembershipOptions struct {
	// Option revokes that option alone (REVOKE … OPTION FOR), leaving the
	// membership; "" revokes the membership.
	Option MembershipOption
	// GrantedBy revokes the grant recorded as made by this role; "" the
	// connected role's (from 16 a superuser's is the bootstrap
	// superuser's). The server warns, and changes nothing, when there is no
	// such grant.
	GrantedBy string
	// Cascade also revokes the grants the member made through the ADMIN
	// option being revoked (CASCADE); without it the server refuses while
	// one exists.
	Cascade bool
}

// grantMembershipStatement is GRANT role TO member with opts on a server at
// num, or the refusal of an option num cannot write. No prefix.
func grantMembershipStatement(num int, role, member string, opts MembershipOptions) (string, error) {
	var with []string
	if before(num, PostgreSQL16) {
		if opts.Inherit != nil || opts.Set != nil {
			return "", refusalf(ErrUnsupportedVersion, "a grant's INHERIT and SET options need %s or later; the server is %s", PostgreSQL16, ServerVersion(num))
		}
		if opts.Admin != nil {
			if !*opts.Admin {
				return "", refusalf(ErrUnsupportedVersion, "WITH ADMIN FALSE needs %s or later; the server is %s — revoke the ADMIN option instead", PostgreSQL16, ServerVersion(num))
			}
			with = append(with, "ADMIN OPTION")
		}
	} else {
		option := func(name string, v *bool) {
			if v != nil {
				word := "FALSE"
				if *v {
					word = "TRUE"
				}
				with = append(with, name+" "+word)
			}
		}
		option("ADMIN", opts.Admin)
		option("INHERIT", opts.Inherit)
		option("SET", opts.Set)
	}
	stmt := "GRANT " + pgsql.QuoteIdent(role) + " TO " + pgsql.QuoteIdent(member)
	if len(with) > 0 {
		stmt += " WITH " + strings.Join(with, ", ")
	}
	if opts.GrantedBy != "" {
		stmt += " GRANTED BY " + pgsql.QuoteIdent(opts.GrantedBy)
	}
	return stmt, nil
}

// revokeMembershipStatement is REVOKE role FROM member with opts on a server
// at num, or the refusal of an option num cannot revoke. No prefix.
func revokeMembershipStatement(num int, role, member string, opts RevokeMembershipOptions) (string, error) {
	stmt := "REVOKE "
	switch opts.Option {
	case "":
	case MembershipAdmin:
		stmt += "ADMIN OPTION FOR "
	case MembershipInherit, MembershipSet:
		if before(num, PostgreSQL16) {
			return "", refusalf(ErrUnsupportedVersion, "a grant's %s option needs %s or later; the server is %s", opts.Option, PostgreSQL16, ServerVersion(num))
		}
		stmt += string(opts.Option) + " OPTION FOR "
	default:
		return "", invalidf("unknown membership option %q; it is ADMIN, INHERIT or SET", string(opts.Option))
	}
	stmt += pgsql.QuoteIdent(role) + " FROM " + pgsql.QuoteIdent(member)
	if opts.GrantedBy != "" {
		stmt += " GRANTED BY " + pgsql.QuoteIdent(opts.GrantedBy)
	}
	if opts.Cascade {
		stmt += " CASCADE"
	}
	return stmt, nil
}

// AddMember makes member a member of the role: GRANT role TO member, with
// the options set on opts. Re-granting an existing membership succeeds
// (with a notice); from 16 it updates the options set. Neither Members nor
// the member's MemberOf is mirrored — the server records the grantor — so
// read the role again to see the grant.
//
// The connected role needs ADMIN on the role (CREATEROLE alone sufficed
// before 16), or superuser. A grant making a role a member of itself, or
// closing a cycle, is the server's 0LP01.
func (r *Role) AddMember(ctx context.Context, member string, opts MembershipOptions) error {
	if r.Name == "" || member == "" {
		return invalidf("pgmo: add role member: both the role and the member are required")
	}
	stmt, err := grantMembershipStatement(int(r.server.version), r.Name, member, opts)
	if err == nil {
		_, err = r.server.exec(ctx, stmt)
	}
	if err != nil {
		return fmt.Errorf("pgmo: grant role %q to %q: %w", r.Name, member, err)
	}
	return nil
}

// RemoveMember revokes member's membership in the role — or, with
// opts.Option, that option of it alone: REVOKE [option OPTION FOR] role
// FROM member. Revoking a membership that does not exist succeeds with a
// warning. Not mirrored, as for AddMember.
func (r *Role) RemoveMember(ctx context.Context, member string, opts RevokeMembershipOptions) error {
	if r.Name == "" || member == "" {
		return invalidf("pgmo: remove role member: both the role and the member are required")
	}
	stmt, err := revokeMembershipStatement(int(r.server.version), r.Name, member, opts)
	if err == nil {
		_, err = r.server.exec(ctx, stmt)
	}
	if err != nil {
		return fmt.Errorf("pgmo: revoke role %q from %q: %w", r.Name, member, err)
	}
	return nil
}

// SetComment sets the role's comment (COMMENT ON ROLE), "" removing it,
// mirrored onto Comment (not under WithScript). The role needs CREATEROLE
// and ADMIN on the role, or to be it.
func (r *Role) SetComment(ctx context.Context, text string) error {
	if r.Name == "" {
		return invalidf("pgmo: comment on role: no name")
	}
	if _, err := r.server.exec(ctx, commentStatement("ROLE "+pgsql.QuoteIdent(r.Name), text)); err != nil {
		return fmt.Errorf("pgmo: comment on role %q: %w", r.Name, err)
	}
	setIfApplied(ctx, &r.Comment, text)
	return nil
}

// ------------------------------------------------------------
// Objects owned: REASSIGN OWNED, DROP OWNED
// ------------------------------------------------------------
//
// What stops a DROP ROLE is what depends on the role (2BP01): the objects
// it owns and the privileges and policies naming it, in every database.
// Database.Dependents on the role's Address() lists them per database and
// Server.DependentDatabases names the databases; these two clear them, one
// database at a time, as the server does — each runs in d.

// ownedRoles quotes roles for an OWNED BY list, refusing none or an empty
// name.
func ownedRoles(roles []string) (string, error) {
	if len(roles) == 0 {
		return "", invalidf("no role")
	}
	if err := requireNames("the roles", roles); err != nil {
		return "", err
	}
	return identList(roles), nil
}

// ReassignOwned gives every object roles own in the database to newOwner:
// REASSIGN OWNED BY … TO …, run in d. The databases and tablespaces they
// own are shared objects, and the server reassigns them too, from any
// database; their privileges are left as they are (DropOwned revokes them).
// An empty role or newOwner is refused (ErrInvalidRequest); the server
// refuses an unknown role (42704) and a role the caller is not a member of
// (42501).
func (d *Database) ReassignOwned(ctx context.Context, roles []string, newOwner string) error {
	what := fmt.Sprintf("reassign objects owned in database %q", d.Name)
	list, err := ownedRoles(roles)
	if err == nil && newOwner == "" {
		err = invalidf("no new owner")
	}
	if err != nil {
		return fmt.Errorf("pgmo: %s: %w", what, err)
	}
	if err := d.exec(ctx, "REASSIGN OWNED BY "+list+" TO "+pgsql.QuoteIdent(newOwner)); err != nil {
		return fmt.Errorf("pgmo: %s: %w", what, err)
	}
	return nil
}

// DropOwned drops every object roles own in the database and revokes every
// privilege they hold there and on the shared objects: DROP OWNED BY …,
// run in d. A policy naming one of them loses it, and goes when it names
// no other role. The databases and tablespaces they own are not dropped.
// opts.Cascade drops what depends on those objects too (CASCADE); without
// it the server refuses while anything does (2BP01). Never a default.
func (d *Database) DropOwned(ctx context.Context, roles []string, opts DropOptions) error {
	what := fmt.Sprintf("drop objects owned in database %q", d.Name)
	list, err := ownedRoles(roles)
	if err != nil {
		return fmt.Errorf("pgmo: %s: %w", what, err)
	}
	if err := d.exec(ctx, "DROP OWNED BY "+list+opts.clause()); err != nil {
		return fmt.Errorf("pgmo: %s: %w", what, err)
	}
	return nil
}
