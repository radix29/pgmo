package pgmo

import (
	"context"
	"fmt"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
)

// ============================================================
// Roles
// ============================================================
//
// role.go reads the cluster's roles (PLAN.md P4.2). PostgreSQL has one kind
// of principal where SQL Server has logins, server roles, database users and
// database roles: a role, cluster-wide, which may log in (LOGIN), hold
// attributes (SUPERUSER, CREATEDB…), and be a member of other roles. gosmo's
// Login and ServerRole both map onto it; a database user has no counterpart —
// what a role may do in a database is its privileges there (P4.13.4).
//
// pg_authid, pg_auth_members and pg_db_role_setting are shared catalogs, so
// every read goes through the Server's own database (s.on(ctx)). The role is
// read from pg_roles, the view any role may read: pg_authid itself is
// readable by superusers only, and the one column the view hides is the
// password hash — so whether a role has a password is not read at all.
//
// One row per role carries everything, the membership grants in both
// directions and the per-database settings included: each is one aggregate
// in a LATERAL subquery (parallel arrays sharing one ORDER BY), so a listing
// is one statement and one snapshot, and no scan queries.

// firstNormalObjectID is FirstNormalObjectId: every object initdb does not
// create — every role a CREATE ROLE made — has an OID at or above it.
const firstNormalObjectID = 16384

// bootstrapSuperuserID is BOOTSTRAP_SUPERUSERID, the OID of the superuser
// initdb creates (named after the operating system user, or initdb -U).
const bootstrapSuperuserID = 10

// predefinedRoleCondition is the SQL form of Role.IsPredefined over the role
// row r: an OID initdb assigned, other than the bootstrap superuser's.
const predefinedRoleCondition = "(r.oid < 16384 AND r.oid <> 10)"

// ValidUntilNegativeInfinity is what Role.ValidUntil holds for VALID UNTIL
// '-infinity' — a password that is always expired: PostgreSQL's earliest
// timestamp (4714-11-24 BC, 00:00 UTC), so it compares before any time.
var ValidUntilNegativeInfinity = time.Date(-4713, time.November, 24, 0, 0, 0, 0, time.UTC)

// RoleMembership is one grant of membership in a role (pg_auth_members):
// Member is a member of Role, as granted by Grantor. From 16 a role may be
// granted the same role more than once, by different grantors; each grant is
// its own RoleMembership, and the member holds the union of their options.
type RoleMembership struct {
	Role   string
	Member string

	// Grantor is the role the grant is recorded as made by. From 16 a grant
	// by a superuser is recorded as the bootstrap superuser's. Before 16 the
	// grantor may have been dropped since; its name is then empty.
	Grantor string

	// Admin is WITH ADMIN OPTION: the member may grant Role to others, and
	// revoke it.
	Admin bool

	// Inherit is whether the member holds Role's privileges without SET
	// ROLE. From 16 it is the grant's INHERIT option; before, there is no
	// such option, and it is the member's INHERIT attribute — what 15
	// checks.
	Inherit bool

	// Set is whether the member may SET ROLE to Role (and create objects
	// owned by it) — the grant's SET option, 16+. Always true before 16,
	// which has no such option.
	Set bool
}

// DatabaseSettings is the parameter defaults ALTER ROLE … IN DATABASE … SET
// stored for one role in one database.
type DatabaseSettings struct {
	Database string
	Settings []ParameterSetting
}

// Role is one role of the cluster, as pg_roles describes it. Read it with
// Server.Roles or Server.RoleByName; Server.RoleRef is a handle holding the
// name alone.
type Role struct {
	server *Server

	OID  OID
	Name string

	// Superuser, Inherit, CreateRole, CreateDB, CanLogin, Replication and
	// BypassRLS are the role's attributes: rolsuper, rolinherit,
	// rolcreaterole, rolcreatedb, rolcanlogin, rolreplication,
	// rolbypassrls. From 16 Inherit is only the default of the INHERIT
	// option of the grants the role receives (RoleMembership.Inherit).
	Superuser, Inherit, CreateRole, CreateDB, CanLogin, Replication, BypassRLS bool

	// ConnectionLimit is rolconnlimit: the most concurrent connections the
	// role may hold (a LOGIN role; superusers are not limited), -1 for none.
	ConnectionLimit int

	// ValidUntil is the password's expiry (VALID UNTIL), after which the
	// password no longer logs the role in; other authentication methods are
	// not affected. The zero Time means no expiry: none was set, or it was
	// 'infinity'. '-infinity' reads as ValidUntilNegativeInfinity.
	ValidUntil time.Time

	// Settings are the parameter defaults ALTER ROLE … SET stored for the
	// role in every database (pg_db_role_setting with setdatabase 0), in
	// stored order; nil for none.
	Settings []ParameterSetting

	// DatabaseSettings are the defaults ALTER ROLE … IN DATABASE … SET
	// stored for the role in one database each, by database name; nil for
	// none. The defaults ALTER DATABASE … SET stored for every role are the
	// database's (Database.Settings).
	DatabaseSettings []DatabaseSettings

	// MemberOf are the grants making this role a member of another, by role
	// name then grantor; Members the grants making another role a member of
	// this one, by member name then grantor. Direct grants only: a member of
	// a member is not listed. nil for none.
	MemberOf []RoleMembership
	Members  []RoleMembership

	// Comment is COMMENT ON ROLE's text, empty for none.
	Comment string
}

// Server is the Server the role was read from.
func (r *Role) Server() *Server { return r.server }

// IsPredefined reports one of the roles PostgreSQL defines itself
// (pg_monitor, pg_read_all_data, pg_database_owner…): they cannot be
// dropped, renamed or altered, and no CREATE ROLE can take a pg_ name. It is
// derived from OID — every role initdb creates but the bootstrap superuser —
// so it answers false on a RoleRef.
func (r *Role) IsPredefined() bool {
	return r.OID != 0 && r.OID < firstNormalObjectID && r.OID != bootstrapSuperuserID
}

// IsBootstrapSuperuser reports the superuser initdb created: it cannot be
// dropped, from 16 cannot lose SUPERUSER (15 lets it), and from 16 a
// superuser's grants are recorded as made by it. Derived from OID, so false
// on a RoleRef.
func (r *Role) IsBootstrapSuperuser() bool { return r.OID == bootstrapSuperuserID }

// PasswordExpired reports whether ValidUntil is set and before now: the
// password no longer logs the role in.
func (r *Role) PasswordExpired(now time.Time) bool {
	return !r.ValidUntil.IsZero() && r.ValidUntil.Before(now)
}

// membershipAggregates is the SELECT list aggregating one direction's
// pg_auth_members rows m into five parallel arrays, all ordered by other
// (the other role's name) then the grantor's OID — unique per (role,
// member) — so the arrays line up. inherit and set are the options'
// expressions for the server's release.
func membershipAggregates(other, inherit, set string) string {
	order := " ORDER BY " + other + ", m.grantor)"
	return `pg_catalog.array_agg(` + other + `::pg_catalog.text` + order + `,
		pg_catalog.array_agg(COALESCE(gr.rolname::pg_catalog.text, '')` + order + `,
		pg_catalog.array_agg(m.admin_option` + order + `,
		pg_catalog.array_agg(` + inherit + order + `,
		pg_catalog.array_agg(` + set + order
}

// inheritOption is the INHERIT option of the grant m (16+). Before 16 there is
// no such option, and what decides inheritance is the member's INHERIT
// attribute: member is the alias of the member's pg_roles row. That is a
// different column, not a rename, so it is a hasColumnSince branch rather
// than a colSince substitute.
func inheritOption(num int, member string) string {
	if hasColumnSince(num, PostgreSQL16) {
		return "m.inherit_option"
	}
	return member + ".rolinherit"
}

// roleSelect is the pg_roles read a *Role is built from, for a server at
// num, without its ORDER BY and ending in WHERE true; scanRole reads its row.
//
// The INHERIT and SET options of a grant are 16+ columns. Before 16 the
// member's rolinherit is what decides inheritance (r's in MemberOf, the
// member u's in Members), and every member may SET ROLE.
func roleSelect(num int) string {
	set := colSince(num, PostgreSQL16, "m.set_option", "true")
	return `SELECT r.oid, r.rolname::pg_catalog.text, r.rolsuper, r.rolinherit, r.rolcreaterole,
	r.rolcreatedb, r.rolcanlogin, r.rolreplication, r.rolbypassrls, r.rolconnlimit,
	r.rolvaliduntil, r.rolconfig,
	ds.dbs, ds.entries,
	mo.roles, mo.grantors, mo.admins, mo.inherits, mo.sets,
	mb.members, mb.grantors, mb.admins, mb.inherits, mb.sets,
	COALESCE(pg_catalog.shobj_description(r.oid, 'pg_authid'), '')
FROM pg_catalog.pg_roles r
LEFT JOIN LATERAL (SELECT pg_catalog.array_agg(d.datname::pg_catalog.text ORDER BY d.datname, c.n),
		pg_catalog.array_agg(c.entry ORDER BY d.datname, c.n)
	FROM pg_catalog.pg_db_role_setting s
	JOIN pg_catalog.pg_database d ON d.oid = s.setdatabase
	CROSS JOIN LATERAL pg_catalog.unnest(s.setconfig) WITH ORDINALITY AS c(entry, n)
	WHERE s.setrole = r.oid) AS ds(dbs, entries) ON true
LEFT JOIN LATERAL (SELECT ` + membershipAggregates("g.rolname", inheritOption(num, "r"), set) + `
	FROM pg_catalog.pg_auth_members m
	JOIN pg_catalog.pg_roles g ON g.oid = m.roleid
	LEFT JOIN pg_catalog.pg_roles gr ON gr.oid = m.grantor
	WHERE m.member = r.oid) AS mo(roles, grantors, admins, inherits, sets) ON true
LEFT JOIN LATERAL (SELECT ` + membershipAggregates("u.rolname", inheritOption(num, "u"), set) + `
	FROM pg_catalog.pg_auth_members m
	JOIN pg_catalog.pg_roles u ON u.oid = m.member
	LEFT JOIN pg_catalog.pg_roles gr ON gr.oid = m.grantor
	WHERE m.roleid = r.oid) AS mb(members, grantors, admins, inherits, sets) ON true
WHERE true`
}

// roleFilterColumns is what an ObjectFilter compares against in the role
// listing: the name, and for System the predefined roles. A role has no
// schema and belongs to no extension, so Schema and ExtensionMember narrow
// nothing.
var roleFilterColumns = filterColumns{name: "r.rolname", system: predefinedRoleCondition}

// membershipArrays are one direction's five parallel arrays as scanned.
type membershipArrays struct {
	others, grantors []string
	admins, inherits []bool
	sets             []bool
}

// grants zips the arrays into memberships; self is the scanned role's name,
// filled in as Member (memberOf) or Role (otherwise).
func (a membershipArrays) grants(self string, memberOf bool) ([]RoleMembership, error) {
	n := len(a.others)
	if len(a.grantors) != n || len(a.admins) != n || len(a.inherits) != n || len(a.sets) != n {
		return nil, fmt.Errorf("membership arrays of different lengths: %d roles, %d grantors, %d/%d/%d options",
			n, len(a.grantors), len(a.admins), len(a.inherits), len(a.sets))
	}
	if n == 0 {
		return nil, nil
	}
	out := make([]RoleMembership, n)
	for i := range out {
		m := RoleMembership{Grantor: a.grantors[i], Admin: a.admins[i], Inherit: a.inherits[i], Set: a.sets[i]}
		if memberOf {
			m.Role, m.Member = a.others[i], self
		} else {
			m.Role, m.Member = self, a.others[i]
		}
		out[i] = m
	}
	return out, nil
}

// groupDatabaseSettings turns the parallel arrays of database names and
// "name=value" entries, ordered by database, into one DatabaseSettings per
// database.
func groupDatabaseSettings(dbs, entries []string) ([]DatabaseSettings, error) {
	if len(dbs) != len(entries) {
		return nil, fmt.Errorf("database settings arrays of different lengths: %d databases, %d entries", len(dbs), len(entries))
	}
	var out []DatabaseSettings
	for i, db := range dbs {
		if len(out) == 0 || out[len(out)-1].Database != db {
			out = append(out, DatabaseSettings{Database: db})
		}
		last := &out[len(out)-1]
		last.Settings = append(last.Settings, parseSettings(entries[i:i+1])...)
	}
	return out, nil
}

// validUntil maps rolvaliduntil onto Role.ValidUntil: NULL and 'infinity'
// to the zero Time, '-infinity' to ValidUntilNegativeInfinity.
func validUntil(ts pgtype.Timestamptz) time.Time {
	switch {
	case !ts.Valid || ts.InfinityModifier == pgtype.Infinity:
		return time.Time{}
	case ts.InfinityModifier == pgtype.NegativeInfinity:
		return ValidUntilNegativeInfinity
	}
	return ts.Time
}

// scanRole builds a *Role of s from one roleSelect row.
func scanRole(s *Server, scan func(...any) error) (*Role, error) {
	r := &Role{server: s}
	var until pgtype.Timestamptz
	var config, dbs, entries []string
	var of, members membershipArrays
	if err := scan(&r.OID, &r.Name, &r.Superuser, &r.Inherit, &r.CreateRole,
		&r.CreateDB, &r.CanLogin, &r.Replication, &r.BypassRLS, &r.ConnectionLimit,
		&until, &config, &dbs, &entries,
		&of.others, &of.grantors, &of.admins, &of.inherits, &of.sets,
		&members.others, &members.grantors, &members.admins, &members.inherits, &members.sets,
		&r.Comment); err != nil {
		return nil, err
	}
	r.ValidUntil = validUntil(until)
	r.Settings = parseSettings(config)
	var err error
	if r.DatabaseSettings, err = groupDatabaseSettings(dbs, entries); err != nil {
		return nil, err
	}
	if r.MemberOf, err = of.grants(r.Name, true); err != nil {
		return nil, err
	}
	if r.Members, err = members.grants(r.Name, false); err != nil {
		return nil, err
	}
	return r, nil
}

// Roles lists the cluster's roles, by name, narrowed by f: its Name
// criteria, and System for the predefined roles (true keeps only them, false
// only the others). Every role is listed, whatever the connected role's
// privileges: pg_roles is readable by all.
func (s *Server) Roles(ctx context.Context, f ObjectFilter) ([]*Role, error) {
	where, args, err := f.clause(roleFilterColumns, nil)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, s.on(ctx), "list roles", func(scan func(...any) error) (*Role, error) {
		return scanRole(s, scan)
	}, roleSelect(int(s.version))+where+`
ORDER BY r.rolname`, args...)
}

// RoleByName reads the role named name — a stored name, matched exactly
// (fold typed input with pgsql.FoldIdent first). One the catalog does not
// hold is ErrNotFound. Use RoleRef instead when only a handle to write
// through is needed.
func (s *Server) RoleByName(ctx context.Context, name string) (*Role, error) {
	return readByName(ctx, s.on(ctx), s, scanRole, roleSelect(int(s.version))+`
	AND r.rolname = $1::pg_catalog.text`, []any{name},
		notFoundf("pgmo: role %q not found", name), fmt.Sprintf("read role %q", name))
}

// RoleRef returns a handle for the role named name without reading the
// catalog: Name is set and every other field is zero. It is what a write to
// a role the caller knows exists goes through — one it has just created, or
// one created only under WithScript, which RoleByName would not find.
// IsPredefined and IsBootstrapSuperuser answer false on it. It is not
// interchangeable with RoleByName.
func (s *Server) RoleRef(name string) *Role {
	return &Role{server: s, Name: name}
}
