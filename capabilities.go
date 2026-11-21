package pgmo

// capabilities.go is what the connected role may do: at the cluster scope,
// its role attributes and the predefined roles whose privileges it holds
// (Capabilities); at the database scope, its CREATE, CONNECT and TEMPORARY
// privilege on each database (DatabasePrivileges).

import (
	"context"
	"fmt"
	"slices"
)

// ProbedPredefinedRoles are the predefined roles Capabilities asks about: the
// ones the application layer gates on, not every role PostgreSQL predefines.
// A role a server does not define (pg_maintain before 17) is left out of
// Capabilities.PredefinedRoles rather than reported as not held.
//
// Every name was checked against the lab servers (15 and 18); a misspelt
// name would read as undefined forever rather than fail.
var ProbedPredefinedRoles = []string{
	"pg_monitor",
	"pg_read_all_settings",
	"pg_read_all_stats",
	"pg_signal_backend",
	"pg_read_server_files",
	"pg_checkpoint",
	"pg_maintain", // 17
}

// Capabilities is what the connected role may do at the cluster scope: the
// role attributes the server checks and the predefined roles whose
// privileges it holds. It describes current_user — the role privileges are
// checked against, which SET ROLE changes — not session_user.
//
// Obtain one with Server.Capabilities. Every method is nil-safe, so a caller
// that could not probe, or chose not to, can hold a nil *Capabilities and
// still ask questions of it.
//
// A superuser bypasses every privilege check, and the server answers so:
// InRole is true for every role it defines. The attributes are reported as
// stored, so CreateDB may be false for a superuser who may create databases
// all the same — ask IsSuperuser first.
type Capabilities struct {
	// Role is current_user.
	Role string

	// Superuser, CreateRole, CreateDB, Replication and BypassRLS are the
	// role's rolsuper, rolcreaterole, rolcreatedb, rolreplication and
	// rolbypassrls. Since PostgreSQL 16 CREATEROLE manages only the roles
	// the role has ADMIN OPTION on; before, every non-superuser role.
	Superuser, CreateRole, CreateDB, Replication, BypassRLS bool

	// PredefinedRoles maps each name in ProbedPredefinedRoles the server
	// defines to whether the role holds its privileges:
	// pg_has_role(current_user, name, 'USAGE'), membership direct or nested
	// and inherited. A member that does not inherit (NOINHERIT, or a grant
	// WITH INHERIT FALSE) does not hold them until it SETs ROLE, and reads
	// false: that is what the server checks. A name the server does not
	// define has no key. Read it through InRole.
	PredefinedRoles map[string]bool
}

// Probed reports whether these capabilities came from a server that
// answered. A nil or zero Capabilities is "nothing known": InRole and every
// attribute then read false, which a caller must not take for "may not".
// The gate fails open — check Probed before withholding anything.
func (c *Capabilities) Probed() bool { return c != nil && c.PredefinedRoles != nil }

// IsSuperuser reports the rolsuper attribute.
func (c *Capabilities) IsSuperuser() bool { return c != nil && c.Superuser }

// InRole reports whether the role holds the privileges of the named
// predefined role. False for a role never probed, one the server does not
// define, and when nothing was probed (see Probed).
func (c *Capabilities) InRole(name string) bool {
	if c == nil {
		return false
	}
	return c.PredefinedRoles[name]
}

// Defines reports whether the server defines the named predefined role
// (pg_maintain on 17 and later). False for a role never probed.
func (c *Capabilities) Defines(name string) bool {
	if c == nil {
		return false
	}
	_, ok := c.PredefinedRoles[name]
	return ok
}

// capabilitiesQuery reads current_user's attributes, the probed predefined
// roles the server defines ($1) and those whose privileges current_user
// holds. One row; pg_roles, since pg_authid is readable by superusers only.
// The arrays are text[] so they scan into []string.
const capabilitiesQuery = `SELECT r.rolname::pg_catalog.text, r.rolsuper, r.rolcreaterole, r.rolcreatedb, r.rolreplication, r.rolbypassrls,
	ARRAY(SELECT p.rolname::pg_catalog.text FROM pg_catalog.pg_roles p
		WHERE p.rolname::pg_catalog.text = ANY ($1::pg_catalog.text[]) ORDER BY 1),
	ARRAY(SELECT p.rolname::pg_catalog.text FROM pg_catalog.pg_roles p
		WHERE p.rolname::pg_catalog.text = ANY ($1::pg_catalog.text[])
		AND pg_catalog.pg_has_role(r.oid, p.oid, 'USAGE') ORDER BY 1)
FROM pg_catalog.pg_roles r
WHERE r.rolname = current_user`

// Capabilities reads what the connected role may do at the cluster scope.
// Inside InTransaction it reads the transaction's session, so a SET LOCAL
// ROLE there is honoured.
func (s *Server) Capabilities(ctx context.Context) (*Capabilities, error) {
	var c Capabilities
	var defined, held []string
	err := queryRowScan(ctx, s.on(ctx), capabilitiesQuery, []any{ProbedPredefinedRoles},
		&c.Role, &c.Superuser, &c.CreateRole, &c.CreateDB, &c.Replication, &c.BypassRLS, &defined, &held)
	if err != nil {
		return nil, fmt.Errorf("pgmo: read capabilities: %w", err)
	}
	c.PredefinedRoles = make(map[string]bool, len(defined))
	for _, name := range defined {
		c.PredefinedRoles[name] = slices.Contains(held, name)
	}
	return &c, nil
}

// DatabasePrivileges is what the connected role may do with one database: its
// database privileges, and whether the database takes connections at all.
// Like Capabilities it describes current_user.
//
// A superuser holds every privilege, as the server answers. A database
// refuses connections to every role, superusers included, when
// AllowConnections is false or it is Invalid — ask CanConnect, not Connect,
// before opening one.
type DatabasePrivileges struct {
	// Create, Connect and Temporary are has_database_privilege(oid, 'CREATE'
	// / 'CONNECT' / 'TEMPORARY'): granted to the role, to PUBLIC, or to a
	// role whose privileges it inherits. CREATE is creating schemas (and
	// trusted extensions, publications) in the database; TEMPORARY is
	// creating temporary tables. PUBLIC holds CONNECT and TEMPORARY on a new
	// database by default.
	Create, Connect, Temporary bool

	// AllowConnections is datallowconn: false on template0, and on any
	// database ALTER DATABASE … ALLOW_CONNECTIONS false has closed.
	AllowConnections bool

	// Invalid is a database a DROP DATABASE was interrupted in
	// (datconnlimit = -2): no one can connect to it, and the one thing left
	// to do with it is DROP DATABASE.
	Invalid bool
}

// CanConnect reports whether the role may open a connection to the database
// as far as the catalog can tell: it holds CONNECT and the database takes
// connections. pg_hba.conf and the connection limits are not visible here;
// the server may still refuse.
func (p DatabasePrivileges) CanConnect() bool {
	return p.Connect && p.AllowConnections && !p.Invalid
}

// databasePrivilegesQuery reads every database's privileges for
// current_user. pg_database is a shared catalog, so one read from any
// database answers for all of them.
const databasePrivilegesQuery = `SELECT d.datname::pg_catalog.text,
	pg_catalog.has_database_privilege(d.oid, 'CREATE'),
	pg_catalog.has_database_privilege(d.oid, 'CONNECT'),
	pg_catalog.has_database_privilege(d.oid, 'TEMPORARY'),
	d.datallowconn, d.datconnlimit = -2
FROM pg_catalog.pg_database d`

// DatabasePrivileges reads what the connected role may do with each database
// of the cluster, keyed by name: one read, not one per database, and through
// the Server's own database, whichever database is asked about. Inside
// InTransaction it reads the transaction's session, so a SET LOCAL ROLE
// there is honoured.
func (s *Server) DatabasePrivileges(ctx context.Context) (map[string]DatabasePrivileges, error) {
	type row struct {
		name string
		p    DatabasePrivileges
	}
	rows, err := scanRows(ctx, s.on(ctx), "read database privileges", func(scan func(...any) error) (row, error) {
		var v row
		err := scan(&v.name, &v.p.Create, &v.p.Connect, &v.p.Temporary, &v.p.AllowConnections, &v.p.Invalid)
		return v, err
	}, databasePrivilegesQuery)
	if err != nil {
		return nil, err
	}
	out := make(map[string]DatabasePrivileges, len(rows))
	for _, r := range rows {
		out[r.name] = r.p
	}
	return out, nil
}
