package pgmo

import (
	"context"
	"fmt"
)

// ============================================================
// Privileges and default privileges
// ============================================================
//
// privileges.go reads PostgreSQL's access control lists (PLAN.md P4.3, for
// every family after it). Where SQL Server keeps one row per permission in
// sys.database_permissions, PostgreSQL keeps an aclitem[] on the object's own
// catalog row (nspacl, relacl, proacl…): one item per (grantee, grantor), each
// a set of privilege bits with a grant-option bit apiece. pgmo never parses
// the aclitem text — role names in it are quoted by rules of its own — but
// has the server explode it (aclexplode: one row per privilege, the grantee
// and grantor as OIDs, 0 for PUBLIC), so the result is the same on every
// release and names come from the catalog.
//
// A NULL ACL is not "no privileges": it is the object kind's hard-wired
// default (acldefault) — the owner holds everything, and PUBLIC holds
// EXECUTE on a function, USAGE on a type or language, CONNECT and TEMPORARY
// on a database. A family reads COALESCE(acl, acldefault(kind, owner)), so
// its Privileges are what the server checks, and says apart whether the
// ACL was ever set.
//
// Default privileges (pg_default_acl, ALTER DEFAULT PRIVILEGES) are the
// ACLs objects a role creates later will start with. A row names the role
// (FOR ROLE), a schema or none (IN SCHEMA), and an object type; its ACL has
// a different meaning by scope, which DefaultPrivileges documents.

// Privilege is one privilege as aclexplode names it: SELECT, INSERT, USAGE,
// CREATE… It is the server's text, so a privilege a later release adds is
// still read.
type Privilege string

// The privileges PostgreSQL 15–18 know. Which an object kind accepts is the
// kind's: a schema takes USAGE and CREATE, a table SELECT through MAINTAIN.
const (
	PrivilegeSelect      Privilege = "SELECT"
	PrivilegeInsert      Privilege = "INSERT"
	PrivilegeUpdate      Privilege = "UPDATE"
	PrivilegeDelete      Privilege = "DELETE"
	PrivilegeTruncate    Privilege = "TRUNCATE"
	PrivilegeReferences  Privilege = "REFERENCES"
	PrivilegeTrigger     Privilege = "TRIGGER"
	PrivilegeMaintain    Privilege = "MAINTAIN" // tables and materialized views, 17+
	PrivilegeExecute     Privilege = "EXECUTE"
	PrivilegeUsage       Privilege = "USAGE"
	PrivilegeCreate      Privilege = "CREATE"
	PrivilegeConnect     Privilege = "CONNECT"
	PrivilegeTemporary   Privilege = "TEMPORARY"
	PrivilegeSet         Privilege = "SET"          // parameters, 15+
	PrivilegeAlterSystem Privilege = "ALTER SYSTEM" // parameters, 15+
)

// PrivilegeGrant is one privilege an ACL grants: Grantee holds Privilege on
// the object, as granted by Grantor.
type PrivilegeGrant struct {
	// Grantee is the role holding the privilege, empty for PUBLIC (every
	// role). No role can be named PUBLIC's way: the server reserves
	// "public" as a role name.
	Grantee string

	// Grantor is the role the grant is recorded as made by: the object's
	// owner, or a role holding the privilege WITH GRANT OPTION. A grant by a
	// superuser is recorded as the owner's.
	Grantor string

	Privilege Privilege

	// Grantable is WITH GRANT OPTION: Grantee may grant Privilege to others.
	Grantable bool
}

// IsPublic reports a grant to PUBLIC.
func (g PrivilegeGrant) IsPublic() bool { return g.Grantee == "" }

// aclExploded is the FROM item exploding the aclitem[] expression acl into
// one row per privilege, aliased a(grantor, grantee, privilege, grantable,
// n): n is the row's place in the ACL — items in stored order, each item's
// privileges in the server's bit order — so aggregates ordered by it keep
// the catalog's order.
func aclExploded(acl string) string {
	return `pg_catalog.aclexplode(` + acl + `) WITH ORDINALITY AS a(grantor, grantee, privilege, grantable, n)`
}

// aclGranteeName is the grantee's name of an aclExploded row, the empty
// string for PUBLIC (grantee 0).
const aclGranteeName = `CASE WHEN a.grantee = 0 THEN '' ELSE pg_catalog.pg_get_userbyid(a.grantee)::pg_catalog.text END`

// aclAggregates is the SELECT list aggregating aclExploded rows into four
// parallel arrays — grantees, grantors, privileges, grantable flags — all
// ordered by order (a list of expressions ending in a.n), so they line up.
// privilegeArrays scans them.
func aclAggregates(order string) string {
	by := " ORDER BY " + order + ")"
	return `pg_catalog.array_agg(` + aclGranteeName + by + `,
		pg_catalog.array_agg(pg_catalog.pg_get_userbyid(a.grantor)::pg_catalog.text` + by + `,
		pg_catalog.array_agg(a.privilege` + by + `,
		pg_catalog.array_agg(a.grantable` + by
}

// aclLateral is a LEFT JOIN LATERAL reading the ACL expression acl as
// privilegeArrays, under alias: alias.grantees, .grantors, .privileges,
// .grantables. An empty ACL reads as four NULLs.
func aclLateral(acl, alias string) string {
	return `LEFT JOIN LATERAL (SELECT ` + aclAggregates("a.n") + `
	FROM ` + aclExploded(acl) + `) AS ` + alias + `(grantees, grantors, privileges, grantables) ON true`
}

// aclColumns are aclLateral's columns under alias, in scan order.
func aclColumns(alias string) string {
	return alias + ".grantees, " + alias + ".grantors, " + alias + ".privileges, " + alias + ".grantables"
}

// privilegeArrays are one ACL's four parallel arrays as scanned.
type privilegeArrays struct {
	grantees, grantors, privileges []string
	grantables                     []bool
}

// dest is the arrays' scan destinations, in aclColumns order.
func (a *privilegeArrays) dest() []any {
	return []any{&a.grantees, &a.grantors, &a.privileges, &a.grantables}
}

// grants zips the arrays from index lo up to hi into PrivilegeGrants.
func (a privilegeArrays) grants(lo, hi int) []PrivilegeGrant {
	if lo == hi {
		return nil
	}
	out := make([]PrivilegeGrant, 0, hi-lo)
	for i := lo; i < hi; i++ {
		out = append(out, PrivilegeGrant{Grantee: a.grantees[i], Grantor: a.grantors[i],
			Privilege: Privilege(a.privileges[i]), Grantable: a.grantables[i]})
	}
	return out
}

// check refuses arrays of different lengths, and n other arrays (parallel
// to them, as a default-privilege read's roles and object types) of a
// length of their own; it returns the common length.
func (a privilegeArrays) check(others ...int) (int, error) {
	n := len(a.grantees)
	ok := len(a.grantors) == n && len(a.privileges) == n && len(a.grantables) == n
	for _, o := range others {
		ok = ok && o == n
	}
	if !ok {
		return 0, fmt.Errorf("privilege arrays of different lengths: %d grantees, %d grantors, %d privileges, %d grant options, %v others",
			n, len(a.grantors), len(a.privileges), len(a.grantables), others)
	}
	return n, nil
}

// all is every grant the arrays hold, nil for none.
func (a privilegeArrays) all() ([]PrivilegeGrant, error) {
	n, err := a.check()
	if err != nil {
		return nil, err
	}
	return a.grants(0, n), nil
}

// DefaultPrivilegeObjectType is pg_default_acl.defaclobjtype: the kind of
// object a default-privilege entry applies to. It is the catalog's one-letter
// code, so a kind a later release adds is still read — String names the
// ones pgmo knows.
type DefaultPrivilegeObjectType string

const (
	DefaultPrivilegeTables       DefaultPrivilegeObjectType = "r" // tables, views, materialized views, foreign tables
	DefaultPrivilegeSequences    DefaultPrivilegeObjectType = "S"
	DefaultPrivilegeFunctions    DefaultPrivilegeObjectType = "f" // functions, procedures, aggregates
	DefaultPrivilegeTypes        DefaultPrivilegeObjectType = "T" // types and domains
	DefaultPrivilegeSchemas      DefaultPrivilegeObjectType = "n" // never IN SCHEMA
	DefaultPrivilegeLargeObjects DefaultPrivilegeObjectType = "L" // 18+, never IN SCHEMA
)

// String names the type as ALTER DEFAULT PRIVILEGES' ON clause spells it:
// TABLES, SEQUENCES, FUNCTIONS, TYPES, SCHEMAS, LARGE OBJECTS; an unknown
// code reads DefaultPrivilegeObjectType("x").
func (t DefaultPrivilegeObjectType) String() string {
	switch t {
	case DefaultPrivilegeTables:
		return "TABLES"
	case DefaultPrivilegeSequences:
		return "SEQUENCES"
	case DefaultPrivilegeFunctions:
		return "FUNCTIONS"
	case DefaultPrivilegeTypes:
		return "TYPES"
	case DefaultPrivilegeSchemas:
		return "SCHEMAS"
	case DefaultPrivilegeLargeObjects:
		return "LARGE OBJECTS"
	}
	return fmt.Sprintf("DefaultPrivilegeObjectType(%q)", string(t))
}

// DefaultPrivileges is one pg_default_acl entry: the privileges objects of
// ObjectType that Role creates — in Schema, or anywhere — are given when
// they are created.
//
// What Grants means depends on Schema:
//
//   - Database-wide (Schema empty, ALTER DEFAULT PRIVILEGES without IN
//     SCHEMA), Grants replaces the type's hard-wired default: it is the
//     whole ACL a new object starts with, the owner's own privileges
//     included. A REVOKE … FROM PUBLIC ON FUNCTIONS shows as an entry
//     without PUBLIC.
//   - In a schema, Grants is added to the database-wide ACL (or the
//     hard-wired default); it can grant, never take away.
type DefaultPrivileges struct {
	// Role is the role whose new objects the entry applies to (FOR ROLE).
	Role string

	// Schema is the schema the entry is limited to (IN SCHEMA), empty for
	// every schema.
	Schema string

	ObjectType DefaultPrivilegeObjectType

	// Grants are the entry's ACL, in stored order. Grantor is Role
	// throughout.
	Grants []PrivilegeGrant
}

// defaultPrivilegeArrays are a set of pg_default_acl entries flattened to
// one row per privilege, in parallel arrays ordered by role name, object
// type code (byte order: S, T, r…), then ACL order: the entries' roles and object types, and their
// privileges.
type defaultPrivilegeArrays struct {
	roles, types []string
	acl          privilegeArrays
}

// dest is the arrays' scan destinations, in defaultPrivilegeColumns order.
func (a *defaultPrivilegeArrays) dest() []any {
	return append([]any{&a.roles, &a.types}, a.acl.dest()...)
}

// entries groups the arrays into one DefaultPrivileges per (role, object
// type), each in schema; nil for none.
func (a defaultPrivilegeArrays) entries(schema string) ([]DefaultPrivileges, error) {
	n, err := a.acl.check(len(a.roles), len(a.types))
	if err != nil {
		return nil, err
	}
	var out []DefaultPrivileges
	lo := 0
	for i := 1; i <= n; i++ {
		if i < n && a.roles[i] == a.roles[lo] && a.types[i] == a.types[lo] {
			continue
		}
		out = append(out, DefaultPrivileges{Role: a.roles[lo], Schema: schema,
			ObjectType: DefaultPrivilegeObjectType(a.types[lo]), Grants: a.acl.grants(lo, i)})
		lo = i
	}
	return out, nil
}

// defaultPrivilegeLateral is a LEFT JOIN LATERAL reading the
// pg_default_acl entries in the schema whose OID is the expression nspoid
// as defaultPrivilegeArrays, under alias: alias.roles, .types, then
// aclColumns(alias).
func defaultPrivilegeLateral(nspoid, alias string) string {
	order := "pg_catalog.pg_get_userbyid(d.defaclrole), d.defaclobjtype, a.n"
	by := " ORDER BY " + order + ")"
	return `LEFT JOIN LATERAL (SELECT pg_catalog.array_agg(pg_catalog.pg_get_userbyid(d.defaclrole)::pg_catalog.text` + by + `,
		pg_catalog.array_agg(d.defaclobjtype::pg_catalog.text` + by + `,
		` + aclAggregates(order) + `
	FROM pg_catalog.pg_default_acl d
	CROSS JOIN LATERAL ` + aclExploded("d.defaclacl") + `
	WHERE d.defaclnamespace = ` + nspoid + `) AS ` + alias + `(roles, types, grantees, grantors, privileges, grantables) ON true`
}

// defaultPrivilegeColumns are defaultPrivilegeLateral's columns under alias,
// in scan order.
func defaultPrivilegeColumns(alias string) string {
	return alias + ".roles, " + alias + ".types, " + aclColumns(alias)
}

// defaultPrivilegesQuery is every pg_default_acl entry of the database, one
// row each: role, schema (empty for none), object type, and its ACL. Ordered by
// role name, the database-wide entry first, then schema name and type — each
// the catalog's own column (name, "char"), so the order is byte order
// whatever the database's collation, as defaultPrivilegeLateral's.
var defaultPrivilegesQuery = `SELECT pg_catalog.pg_get_userbyid(d.defaclrole)::pg_catalog.text,
	COALESCE(n.nspname::pg_catalog.text, ''), d.defaclobjtype::pg_catalog.text,
	` + aclColumns("p") + `
FROM pg_catalog.pg_default_acl d
LEFT JOIN pg_catalog.pg_namespace n ON n.oid = d.defaclnamespace
` + aclLateral("d.defaclacl", "p") + `
ORDER BY pg_catalog.pg_get_userbyid(d.defaclrole), n.nspname NULLS FIRST, d.defaclobjtype`

// DefaultPrivileges reads every ALTER DEFAULT PRIVILEGES entry of the
// database: the database-wide ones (Schema empty) and those IN SCHEMA, by
// role, then schema — the database-wide first — then object type code; nil for
// none. A schema's own are also on Schema.DefaultPrivileges.
//
// pg_default_acl is per database, so it is read through the database's own
// pool; any role may read it. It keys on the name, so it works on a
// DatabaseRef.
func (d *Database) DefaultPrivileges(ctx context.Context) ([]DefaultPrivileges, error) {
	what := fmt.Sprintf("read default privileges of database %q", d.Name)
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, what, func(scan func(...any) error) (DefaultPrivileges, error) {
		var e DefaultPrivileges
		var objType string
		var acl privilegeArrays
		if err := scan(append([]any{&e.Role, &e.Schema, &objType}, acl.dest()...)...); err != nil {
			return e, err
		}
		e.ObjectType = DefaultPrivilegeObjectType(objType)
		var err error
		e.Grants, err = acl.all()
		return e, err
	}, defaultPrivilegesQuery)
}
