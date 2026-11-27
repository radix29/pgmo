package pgmo

import (
	"context"
	"fmt"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Foreign data wrappers, foreign servers, user mappings
// ============================================================
//
// foreign_data.go reads a database's SQL/MED objects (PLAN.md P4.12) from
// pg_foreign_data_wrapper, pg_foreign_server and pg_user_mappings, through
// the database's own pool: they are a database's, not the cluster's, and
// have no schema. A wrapper is the driver (postgres_fdw, file_fdw); a
// server one remote source reached through it — SQL Server's linked
// server; a user mapping the credentials a role uses on a server — its
// linked server login. Foreign tables, the relations read through a
// server, are foreign_table.go's.
//
// None is created by initdb, so IsSystem (an OID below
// FirstNormalObjectId, the rule for an object with no schema,
// OPEN-THREADS.md § System objects) is false for every one there is; a
// wrapper or server can be an extension member (postgres_fdw's wrapper
// is). A user mapping can be neither: ALTER EXTENSION … ADD takes none.
//
// pg_foreign_data_wrapper and pg_foreign_server are readable by every role,
// so those listings are the same for all. pg_user_mapping is not — its
// options hold passwords — so user mappings are read from the
// pg_user_mappings view, which lists every mapping to every role but shows
// a mapping's options only to the role it maps (with USAGE on the server,
// or membership in its owner), to a member of the server's owner for the
// PUBLIC mapping, and to a superuser. Whether they were withheld is
// UserMapping.OptionsHidden.
//
// Options are text[] "name=value" entries as stored, split on the first
// '=' as pg_options_to_table splits them (an option name holding '=' —
// only a quoted identifier can — reads wrongly there too).

// ForeignDataWrapper is one foreign data wrapper of a database. Read it
// with Database.ForeignDataWrappers or Database.ForeignDataWrapperByName;
// Database.ForeignDataWrapperRef is a handle holding the name alone.
type ForeignDataWrapper struct {
	db *Database

	// Origin: IsSystem never (initdb creates none); Extension for one an
	// extension owns (postgres_fdw's, file_fdw's).
	Origin

	OID  OID
	Name string

	// Owner is the owning role's name (fdwowner).
	Owner string

	// Handler is the function the server calls to scan and modify a
	// foreign table (fdwhandler); zero for none, a wrapper that only
	// validates options and cannot be read through.
	Handler ObjectName

	// Validator checks the options given to the wrapper, its servers,
	// user mappings and foreign tables (fdwvalidator); zero for none, when
	// any option is accepted unchecked.
	Validator ObjectName

	// Options are the wrapper's own options (fdwoptions) in stored order;
	// nil for none.
	Options []ParameterSetting

	// Privileges are the wrapper's ACL (USAGE: creating servers on it),
	// per grantee, in stored order. One never set reads as the hard-wired
	// default: the owner's alone (PrivilegesDefault).
	Privileges []PrivilegeGrant

	// PrivilegesDefault reports that the ACL was never set (fdwacl NULL).
	PrivilegesDefault bool

	// Comment is COMMENT ON FOREIGN DATA WRAPPER's text, empty for none.
	Comment string
}

// ForeignServer is one foreign server of a database. Read it with
// Database.ForeignServers or Database.ForeignServerByName;
// Database.ForeignServerRef is a handle holding the name alone.
type ForeignServer struct {
	db *Database

	// Origin: IsSystem never; Extension for one an extension owns.
	Origin

	OID  OID
	Name string

	// Owner is the owning role's name (srvowner).
	Owner string

	// Wrapper is the foreign data wrapper's name (srvfdw).
	Wrapper string

	// Type and Version are CREATE SERVER's TYPE and VERSION, free text
	// the wrapper may read; empty for none.
	Type, Version string

	// Options are the server's options (srvoptions) in stored order — for
	// postgres_fdw its host, port and dbname; nil for none. Every role
	// reads them: a wrapper keeps secrets in user mappings, not here.
	Options []ParameterSetting

	// Privileges are the server's ACL (USAGE: creating foreign tables and
	// one's own user mapping on it), per grantee, in stored order. One
	// never set reads as the owner's alone (PrivilegesDefault).
	Privileges []PrivilegeGrant

	// PrivilegesDefault reports that the ACL was never set (srvacl NULL).
	PrivilegesDefault bool

	// Comment is COMMENT ON SERVER's text, empty for none.
	Comment string
}

// UserMapping is one user mapping of a database: the options — typically
// a remote user and password — a role, or every role (PUBLIC), uses on a
// foreign server. It has neither a name nor an owner nor a comment: it is
// the pair of its server and its role, unique in the database. Read it
// with Database.UserMappings, ForeignServer.UserMappings or
// Database.UserMappingByName; Database.UserMappingRef is a handle holding
// the two names alone.
type UserMapping struct {
	db *Database

	// Origin is always zero: a user mapping is no system object and no
	// extension's member.
	Origin

	OID OID

	// ServerOID and Server are the foreign server's OID and name.
	ServerOID OID
	Server    string

	// UserOID and User are the mapped role's OID and name; 0 and "" for
	// the PUBLIC mapping, as PrivilegeGrant.Grantee names PUBLIC.
	UserOID OID
	User    string

	// Options are the mapping's options in stored order, nil for none —
	// or for options withheld from the reading role (OptionsHidden). A
	// password among them is the cleartext the wrapper sends.
	Options []ParameterSetting

	// OptionsHidden reports that the server withheld the options from the
	// reading role (pg_user_mappings' rule, in the file's comment): Options
	// is then nil whatever the mapping holds.
	OptionsHidden bool
}

// Database is the Database the wrapper was read from.
func (w *ForeignDataWrapper) Database() *Database { return w.db }

// QualifiedName is the wrapper's name quoted for a statement. It has no
// schema.
func (w *ForeignDataWrapper) QualifiedName() string { return pgsql.QuoteIdent(w.Name) }

// Database is the Database the server was read from.
func (s *ForeignServer) Database() *Database { return s.db }

// QualifiedName is the server's name quoted for a statement. It has no
// schema.
func (s *ForeignServer) QualifiedName() string { return pgsql.QuoteIdent(s.Name) }

// Database is the Database the user mapping was read from.
func (m *UserMapping) Database() *Database { return m.db }

// IsPublic reports the PUBLIC mapping: every role without one of its own.
func (m *UserMapping) IsPublic() bool { return m.User == "" }

// Signature is the mapping's identity as DROP and ALTER USER MAPPING take
// it: "FOR app_user SERVER remote", "FOR PUBLIC SERVER remote".
func (m *UserMapping) Signature() string {
	user := "PUBLIC"
	if !m.IsPublic() {
		user = pgsql.QuoteIdent(m.User)
	}
	return "FOR " + user + " SERVER " + pgsql.QuoteIdent(m.Server)
}

// ============================================================
// Foreign data wrappers
// ============================================================

// foreignDataWrapperSelect is the pg_foreign_data_wrapper read a
// *ForeignDataWrapper is built from, ending in WHERE true without an ORDER
// BY; scanForeignDataWrapper reads its row.
var foreignDataWrapperSelect = `SELECT w.oid, w.fdwname::pg_catalog.text,
	pg_catalog.pg_get_userbyid(w.fdwowner)::pg_catalog.text,
	w.oid < 16384, COALESCE(ext.extname::pg_catalog.text, ''),
	` + procedureName("fh") + `, ` + procedureName("fv") + `,
	COALESCE(w.fdwoptions, '{}'),
	w.fdwacl IS NULL,
	` + aclColumns("p") + `,
	COALESCE(ds.description, '')
FROM pg_catalog.pg_foreign_data_wrapper w
` + procedureJoin("w.fdwhandler", "fh") + `
` + procedureJoin("w.fdwvalidator", "fv") + `
` + originJoin(catalogForeignDataWrapper, "w.oid") + `
LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = w.oid
	AND ds.classoid = 'pg_catalog.pg_foreign_data_wrapper'::pg_catalog.regclass AND ds.objsubid = 0
` + aclLateral("COALESCE(w.fdwacl, pg_catalog.acldefault('F', w.fdwowner))", "p") + `
WHERE true`

// scanForeignDataWrapper builds a *ForeignDataWrapper of d from one
// foreignDataWrapperSelect row.
func scanForeignDataWrapper(d *Database, scan func(...any) error) (*ForeignDataWrapper, error) {
	w := &ForeignDataWrapper{db: d}
	var handler, validator, options []string
	var acl privilegeArrays
	dest := []any{&w.OID, &w.Name, &w.Owner}
	dest = append(dest, scanOrigin(&w.Origin)...)
	dest = append(dest, &handler, &validator, &options, &w.PrivilegesDefault)
	dest = append(dest, acl.dest()...)
	dest = append(dest, &w.Comment)
	if err := scan(dest...); err != nil {
		return nil, err
	}
	w.Handler, w.Validator = objectNameOf(handler), objectNameOf(validator)
	w.Options = parseSettings(options)
	var err error
	w.Privileges, err = acl.all()
	return w, err
}

// foreignDataWrapperFilterColumns are a wrapper listing's ObjectFilter
// columns: no schema; System on the OID.
var foreignDataWrapperFilterColumns = filterColumns{
	name:      "w.fdwname",
	system:    "w.oid < 16384",
	extension: isExtensionMember(catalogForeignDataWrapper, "w.oid"),
}

// ForeignDataWrappers lists the database's foreign data wrappers by name,
// narrowed by f: Name on the wrapper's name, System for none there is,
// ExtensionMember for those an extension owns; a Schema criterion is
// ignored, as a wrapper has none. They are read through the database's own
// pool.
func (d *Database) ForeignDataWrappers(ctx context.Context, f ObjectFilter) ([]*ForeignDataWrapper, error) {
	where, args, err := f.clause(foreignDataWrapperFilterColumns, nil)
	if err != nil {
		return nil, err
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, fmt.Sprintf("list foreign data wrappers of database %q", d.Name),
		func(scan func(...any) error) (*ForeignDataWrapper, error) {
			return scanForeignDataWrapper(d, scan)
		}, foreignDataWrapperSelect+where+`
ORDER BY w.fdwname`, args...)
}

// ForeignDataWrapperByName reads the foreign data wrapper name — a stored
// name, matched exactly (fold typed input with pgsql.FoldIdent first). One
// the database does not hold is ErrNotFound. Use ForeignDataWrapperRef
// instead when only a handle to write through is needed.
func (d *Database) ForeignDataWrapperByName(ctx context.Context, name string) (*ForeignDataWrapper, error) {
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return readByName(ctx, q, d, scanForeignDataWrapper, foreignDataWrapperSelect+`
	AND w.fdwname = $1::pg_catalog.text`, []any{name},
		notFoundf("pgmo: foreign data wrapper %s not found in database %q", pgsql.QuoteIdent(name), d.Name),
		fmt.Sprintf("read foreign data wrapper %s of database %q", pgsql.QuoteIdent(name), d.Name))
}

// ForeignDataWrapperRef returns a handle for the foreign data wrapper name
// without reading the catalog: Name is set and every other field is zero,
// OID included. A read keyed by the name (Servers) works on it. It is not
// interchangeable with ForeignDataWrapperByName.
func (d *Database) ForeignDataWrapperRef(name string) *ForeignDataWrapper {
	return &ForeignDataWrapper{db: d, Name: name}
}

// Servers reads the foreign servers of the wrapper by name, as
// Database.ForeignServers reads them. It keys on the wrapper's name, so it
// works on a ForeignDataWrapperRef; a wrapper the database does not hold
// has none.
func (w *ForeignDataWrapper) Servers(ctx context.Context) ([]*ForeignServer, error) {
	q, err := w.db.server.onDatabase(ctx, w.db.Name)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, "list foreign servers of foreign data wrapper "+w.QualifiedName(),
		func(scan func(...any) error) (*ForeignServer, error) {
			return scanForeignServer(w.db, scan)
		}, foreignServerSelect+`
	AND sw.fdwname = $1::pg_catalog.text
ORDER BY s.srvname`, w.Name)
}

// ============================================================
// Foreign servers
// ============================================================

// foreignServerSelect is the pg_foreign_server read a *ForeignServer is
// built from, ending in WHERE true without an ORDER BY; scanForeignServer
// reads its row.
var foreignServerSelect = `SELECT s.oid, s.srvname::pg_catalog.text,
	pg_catalog.pg_get_userbyid(s.srvowner)::pg_catalog.text,
	s.oid < 16384, COALESCE(ext.extname::pg_catalog.text, ''),
	sw.fdwname::pg_catalog.text, COALESCE(s.srvtype, ''), COALESCE(s.srvversion, ''),
	COALESCE(s.srvoptions, '{}'),
	s.srvacl IS NULL,
	` + aclColumns("p") + `,
	COALESCE(ds.description, '')
FROM pg_catalog.pg_foreign_server s
JOIN pg_catalog.pg_foreign_data_wrapper sw ON sw.oid = s.srvfdw
` + originJoin(catalogForeignServer, "s.oid") + `
LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = s.oid
	AND ds.classoid = 'pg_catalog.pg_foreign_server'::pg_catalog.regclass AND ds.objsubid = 0
` + aclLateral("COALESCE(s.srvacl, pg_catalog.acldefault('S', s.srvowner))", "p") + `
WHERE true`

// scanForeignServer builds a *ForeignServer of d from one
// foreignServerSelect row.
func scanForeignServer(d *Database, scan func(...any) error) (*ForeignServer, error) {
	s := &ForeignServer{db: d}
	var options []string
	var acl privilegeArrays
	dest := []any{&s.OID, &s.Name, &s.Owner}
	dest = append(dest, scanOrigin(&s.Origin)...)
	dest = append(dest, &s.Wrapper, &s.Type, &s.Version, &options, &s.PrivilegesDefault)
	dest = append(dest, acl.dest()...)
	dest = append(dest, &s.Comment)
	if err := scan(dest...); err != nil {
		return nil, err
	}
	s.Options = parseSettings(options)
	var err error
	s.Privileges, err = acl.all()
	return s, err
}

// foreignServerFilterColumns are a server listing's ObjectFilter columns:
// no schema; System on the OID.
var foreignServerFilterColumns = filterColumns{
	name:      "s.srvname",
	system:    "s.oid < 16384",
	extension: isExtensionMember(catalogForeignServer, "s.oid"),
}

// ForeignServers lists the database's foreign servers by name, narrowed by
// f: Name on the server's name, System for none there is, ExtensionMember
// for those an extension owns; a Schema criterion is ignored, as a server
// has none. Every server is listed whatever the role's privileges on it.
// They are read through the database's own pool.
func (d *Database) ForeignServers(ctx context.Context, f ObjectFilter) ([]*ForeignServer, error) {
	where, args, err := f.clause(foreignServerFilterColumns, nil)
	if err != nil {
		return nil, err
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, fmt.Sprintf("list foreign servers of database %q", d.Name),
		func(scan func(...any) error) (*ForeignServer, error) {
			return scanForeignServer(d, scan)
		}, foreignServerSelect+where+`
ORDER BY s.srvname`, args...)
}

// ForeignServerByName reads the foreign server name — a stored name,
// matched exactly (fold typed input with pgsql.FoldIdent first). One the
// database does not hold is ErrNotFound. Use ForeignServerRef instead when
// only a handle to write through is needed.
func (d *Database) ForeignServerByName(ctx context.Context, name string) (*ForeignServer, error) {
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return readByName(ctx, q, d, scanForeignServer, foreignServerSelect+`
	AND s.srvname = $1::pg_catalog.text`, []any{name},
		notFoundf("pgmo: foreign server %s not found in database %q", pgsql.QuoteIdent(name), d.Name),
		fmt.Sprintf("read foreign server %s of database %q", pgsql.QuoteIdent(name), d.Name))
}

// ForeignServerRef returns a handle for the foreign server name without
// reading the catalog: Name is set and every other field is zero, OID
// included. A read keyed by the name (UserMappings) works on it. It is not
// interchangeable with ForeignServerByName.
func (d *Database) ForeignServerRef(name string) *ForeignServer {
	return &ForeignServer{db: d, Name: name}
}

// UserMappings reads the server's user mappings, PUBLIC's first then by
// role name, as Database.UserMappings reads them. It keys on the server's
// name, so it works on a ForeignServerRef; a server the database does not
// hold has none.
func (s *ForeignServer) UserMappings(ctx context.Context) ([]*UserMapping, error) {
	q, err := s.db.server.onDatabase(ctx, s.db.Name)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, "list user mappings of foreign server "+s.QualifiedName(),
		func(scan func(...any) error) (*UserMapping, error) {
			return scanUserMapping(s.db, scan)
		}, userMappingSelect+`
	AND um.srvname = $1::pg_catalog.text
`+userMappingOrder, s.Name)
}

// ============================================================
// User mappings
// ============================================================

// userMappingSelect is the pg_user_mappings read a *UserMapping is built
// from, ending in WHERE true without an ORDER BY; scanUserMapping reads its
// row. The view returns NULL options both for a mapping with none and for
// one it withholds, so whether it withheld them is the view's own
// condition, restated over what any role may read: pg_roles' rolsuper
// where the view, running as its owner, reads pg_authid's. A NULL rolname
// (no such role: CURRENT_USER always has one) reads false.
var userMappingSelect = `SELECT um.umid, um.srvid, um.srvname::pg_catalog.text, um.umuser,
	CASE WHEN um.umuser = 0 THEN '' ELSE um.usename::pg_catalog.text END,
	COALESCE(um.umoptions, '{}'),
	um.umoptions IS NULL AND NOT (
		(um.umuser <> 0 AND um.usename = CURRENT_USER
			AND (pg_catalog.pg_has_role(us.srvowner, 'USAGE') OR pg_catalog.has_server_privilege(us.oid, 'USAGE')))
		OR (um.umuser = 0 AND pg_catalog.pg_has_role(us.srvowner, 'USAGE'))
		OR COALESCE((SELECT r.rolsuper FROM pg_catalog.pg_roles r WHERE r.rolname = CURRENT_USER), false))
FROM pg_catalog.pg_user_mappings um
JOIN pg_catalog.pg_foreign_server us ON us.oid = um.srvid
WHERE true`

// userMappingOrder orders user mappings by server, PUBLIC's ("") first.
const userMappingOrder = `ORDER BY um.srvname, 5`

// scanUserMapping builds a *UserMapping of d from one userMappingSelect
// row.
func scanUserMapping(d *Database, scan func(...any) error) (*UserMapping, error) {
	m := &UserMapping{db: d}
	var options []string
	if err := scan(&m.OID, &m.ServerOID, &m.Server, &m.UserOID, &m.User, &options, &m.OptionsHidden); err != nil {
		return nil, err
	}
	m.Options = parseSettings(options)
	return m, nil
}

// userMappingFilterColumns are a user mapping listing's ObjectFilter
// columns: a mapping has neither name nor schema, and is never a system
// object or an extension member, so System true and ExtensionMember true
// keep none.
var userMappingFilterColumns = filterColumns{
	system:    "false",
	extension: "false",
}

// UserMappings lists the database's user mappings by server, PUBLIC's
// first then by role name, narrowed by f: System true and ExtensionMember
// true keep none; Name and Schema criteria are ignored, as a mapping has
// neither. Every mapping is listed to every role; the options only to some
// (UserMapping.OptionsHidden). They are read through the database's own
// pool.
func (d *Database) UserMappings(ctx context.Context, f ObjectFilter) ([]*UserMapping, error) {
	where, args, err := f.clause(userMappingFilterColumns, nil)
	if err != nil {
		return nil, err
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, fmt.Sprintf("list user mappings of database %q", d.Name),
		func(scan func(...any) error) (*UserMapping, error) {
			return scanUserMapping(d, scan)
		}, userMappingSelect+where+`
`+userMappingOrder, args...)
}

// UserMappingByName reads the user mapping of the role user on the foreign
// server server — stored names, matched exactly; user "" is the PUBLIC
// mapping. A pair the database holds no mapping for is ErrNotFound —
// whether or not the role would fall back on PUBLIC's. Use UserMappingRef
// instead when only a handle to write through is needed.
func (d *Database) UserMappingByName(ctx context.Context, server, user string) (*UserMapping, error) {
	ref := d.UserMappingRef(server, user)
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return readByName(ctx, q, d, scanUserMapping, userMappingSelect+`
	AND um.srvname = $1::pg_catalog.text
	AND CASE WHEN um.umuser = 0 THEN '' ELSE um.usename::pg_catalog.text END = $2::pg_catalog.text`,
		[]any{server, user},
		notFoundf("pgmo: user mapping %s not found in database %q", ref.Signature(), d.Name),
		fmt.Sprintf("read user mapping %s of database %q", ref.Signature(), d.Name))
}

// UserMappingRef returns a handle for the user mapping of the role user
// ("" for PUBLIC) on the foreign server server without reading the
// catalog: Server and User are set and every other field is zero, OIDs
// included. It is not interchangeable with UserMappingByName.
func (d *Database) UserMappingRef(server, user string) *UserMapping {
	return &UserMapping{db: d, Server: server, User: user}
}
