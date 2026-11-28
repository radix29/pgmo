package pgmo

import (
	"context"
	"fmt"
	"slices"
	"strings"
)

// ============================================================
// Object dependencies
// ============================================================
//
// dependency.go answers SSMS's Object Dependencies questions — what does
// this object depend on, and what depends on it — from the catalogs DROP
// itself consults: pg_depend for the objects of one database and
// pg_shdepend for their edges to the shared objects (roles, tablespaces)
// and for the shared objects' own.
//
// pg_depend records an edge on the piece of an object that holds it, not on
// the object a person names: a view's references are its _RETURN rule's,
// a table's foreign key is its constraint's, a column default's is a
// pg_attrdef row's, a table's row type and its array type are objects of
// their own. Read raw, a view would depend on nothing and a table's
// dependents would be its own constraints. So each end of an edge is folded
// into the object it is part of — along an internal edge (deptype 'i': a
// rule, a row or array type, a TOAST table, a constraint's index), or an
// automatic one (deptype 'a') from a part that lives under its object in
// DDL and in the Object Explorer: a column default, a constraint, a rule, a
// trigger, a policy, an index. A sequence is never folded, whether an
// identity column's ('i') or OWNED BY ('a'): it is an object of its own, in
// the Sequences folder, that a function can name. The piece an edge is
// recorded on comes back with it (Dependency.Part, OwnPart), so the fold
// loses nothing: the trigger through which a table depends on its
// function, the columns through which a view depends on a table.
//
// What the catalogs do not hold is not reported. From PostgreSQL 15 the
// objects initdb creates are pinned by OID and given no pg_depend rows, so
// nothing is recorded as depending on int4 or pg_catalog; a role the
// bootstrap superuser owns has no owner edge; a relation stored in a
// tablespace has none either (only one without storage, a partitioned
// table, has a 't' edge); and a PL/pgSQL body is not parsed, so a function
// depends on the tables it queries only when its body is SQL-standard
// (BEGIN ATOMIC / RETURN).

// ObjectAddress names any object by where the server keeps it: the
// catalog holding its row and its OID there — pg_depend's classid and
// objid. Every family handle gives its own (Address()), and so do a
// Dependency, an ExtensionMember and a SearchResult, so the graph can be
// walked from any of them.
type ObjectAddress struct {
	// Catalog is the system catalog, unqualified: pg_class, pg_proc,
	// pg_type, pg_namespace, pg_authid…
	Catalog string

	// OID is the object's OID in Catalog.
	OID OID
}

// sharedCatalogs are the catalogs of the shared objects pg_shdepend
// records edges of and to: roles, databases, tablespaces and parameters
// with privileges. Their rows belong to no database.
var sharedCatalogs = []string{"pg_authid", "pg_database", "pg_tablespace", "pg_parameter_acl"}

// IsShared reports whether a is a shared object — a role, a database, a
// tablespace, a parameter's privileges — whose edges between one another
// Server.Dependencies and Server.Dependents read.
func (a ObjectAddress) IsShared() bool { return slices.Contains(sharedCatalogs, a.Catalog) }

// DependencyKind is how an edge binds its two ends: pg_depend's deptype for
// an edge inside a database, pg_shdepend's for one to or between shared
// objects. A letter this release does not know reads as "pg_depend deptype
// 'q'" (or pg_shdepend), never as one of the kinds below.
type DependencyKind string

// The edges of pg_depend.
const (
	// DependencyNormal ('n'): a reference; DROP of the referenced object
	// needs CASCADE, which drops the referencing one.
	DependencyNormal DependencyKind = "normal"
	// DependencyAuto ('a'): the dependent goes when the referenced object
	// does, whatever CASCADE says — a sequence OWNED BY a column, a
	// partition of its parent.
	DependencyAuto DependencyKind = "auto"
	// DependencyInternal ('i'): the dependent is part of the referenced
	// object's implementation — an identity column's sequence.
	DependencyInternal DependencyKind = "internal"
	// DependencyPartitionPrimary ('P') and DependencyPartitionSecondary
	// ('S'): a partition's index to its parent's index, and to its own
	// partition.
	DependencyPartitionPrimary   DependencyKind = "partition primary"
	DependencyPartitionSecondary DependencyKind = "partition secondary"
	// DependencyExtension ('e'): a member of an extension
	// (Extension.Members reads them all).
	DependencyExtension DependencyKind = "extension"
	// DependencyAutoExtension ('x'): ALTER … DEPENDS ON EXTENSION; dropped
	// with the extension, still a user object.
	DependencyAutoExtension DependencyKind = "auto extension"
)

// The edges of pg_shdepend.
const (
	// DependencyOwner ('o'): the role owns the object.
	DependencyOwner DependencyKind = "owner"
	// DependencyPrivilege ('a'): the role is named in the object's ACL.
	DependencyPrivilege DependencyKind = "privilege"
	// DependencyPolicyRole ('r'): the role is a target of a policy.
	DependencyPolicyRole DependencyKind = "policy role"
	// DependencyInitialPrivilege ('i'): the role is named in an extension
	// object's initial ACL (pg_init_privs).
	DependencyInitialPrivilege DependencyKind = "initial privilege"
	// DependencyTablespace ('t'): a relation without storage, a
	// partitioned table, names the tablespace its partitions default to.
	DependencyTablespace DependencyKind = "tablespace"
)

// dependencyKind maps a deptype letter, of pg_shdepend when shared.
func dependencyKind(deptype string, shared bool) DependencyKind {
	if shared {
		switch deptype {
		case "o":
			return DependencyOwner
		case "a":
			return DependencyPrivilege
		case "r":
			return DependencyPolicyRole
		case "i":
			return DependencyInitialPrivilege
		case "t":
			return DependencyTablespace
		}
		return DependencyKind(fmt.Sprintf("pg_shdepend deptype %q", deptype))
	}
	switch deptype {
	case "n":
		return DependencyNormal
	case "a":
		return DependencyAuto
	case "i":
		return DependencyInternal
	case "P":
		return DependencyPartitionPrimary
	case "S":
		return DependencyPartitionSecondary
	case "e":
		return DependencyExtension
	case "x":
		return DependencyAutoExtension
	}
	return DependencyKind(fmt.Sprintf("pg_depend deptype %q", deptype))
}

// Dependency is one edge of the dependency graph seen from the object
// asked about: the object at its other end, how the edge binds them, and the
// piece of each the server records it on. The other object is a whole one —
// a table, not its constraint — named as pg_identify_object names it.
//
// One edge is one Dependency: a view reading two columns of a table is two,
// told apart by Part. Group by Address() for one row per object.
type Dependency struct {
	// Catalog and OID are the other object's (Address()).
	Catalog string
	OID     OID

	// Type is the other object's kind as pg_identify_object names it:
	// table, view, materialized view, sequence, index, function,
	// procedure, aggregate, type (a domain too), schema, role,
	// tablespace, extension, trigger, policy…
	Type string

	// Identity is pg_identify_object's identity for it: schema-qualified
	// and quoted whatever the session's search_path, a routine with its
	// argument types, a trigger or policy as "name on table".
	Identity string

	// Kind is how the edge binds the two (DependencyNormal…); Shared
	// reports a pg_shdepend edge — to a role or a tablespace, or between
	// shared objects.
	Kind   DependencyKind
	Shared bool

	// PartType and Part name the piece of the other object the edge is
	// recorded on, when it is not the whole object — a column ("table
	// column", s.t.id), a constraint, a rule, a trigger, a default — as
	// Type and Identity name an object; both empty otherwise.
	PartType string
	Part     string

	// OwnPartType and OwnPart are the same for the object asked about: the
	// column a type is used by, the constraint holding a foreign key, a
	// view's _RETURN rule.
	OwnPartType string
	OwnPart     string
}

// Address is the other object's address, to read its own dependencies.
func (d Dependency) Address() ObjectAddress { return ObjectAddress{Catalog: d.Catalog, OID: d.OID} }

// dependencyFoldJoin is the pg_class row of the dependent end of pg_depend
// f, for dependencyFold: NULL unless that end is a relation.
func dependencyFoldJoin(f string) string {
	return `LEFT JOIN pg_catalog.pg_class ` + f + `r ON ` + f + `.classid = 'pg_catalog.pg_class'::pg_catalog.regclass AND ` + f + `r.oid = ` + f + `.objid`
}

// dependencyFold is the condition that the pg_depend row f binds a part to
// the object it is folded into (the header): an internal edge from
// anything but a sequence, or an automatic one from a default, constraint,
// rule, trigger, policy or index. dependencyFoldJoin(f) joins the relation.
// An edge from an object to itself folds nothing: a partition key column's
// internal edge to its own table (found on pg15 and pg18) would otherwise
// make a partitioned table a part of itself, and never the object an edge
// ends at.
func dependencyFold(f string) string {
	return `(` + f + `.refclassid, ` + f + `.refobjid) <> (` + f + `.classid, ` + f + `.objid)
		AND ((` + f + `.deptype = 'i' AND ` + f + `r.relkind IS DISTINCT FROM 'S')
		OR (` + f + `.deptype = 'a' AND (` + f + `r.relkind IN ('i', 'I') OR ` + f + `.classid IN (
			'pg_catalog.pg_attrdef'::pg_catalog.regclass, 'pg_catalog.pg_constraint'::pg_catalog.regclass,
			'pg_catalog.pg_rewrite'::pg_catalog.regclass, 'pg_catalog.pg_trigger'::pg_catalog.regclass,
			'pg_catalog.pg_policy'::pg_catalog.regclass))))`
}

// dependencyFolded replaces the markers /*fold f*/ and /*foldjoin f*/ in
// sql with dependencyFold("f") and dependencyFoldJoin("f"). The queries
// are whole literals around them so that what each names — its CTEs —
// is declared in the literal that reads it.
func dependencyFolded(sql string) string {
	return strings.NewReplacer(
		"/*fold f*/", dependencyFold("f"),
		"/*foldjoin f*/", dependencyFoldJoin("f"),
	).Replace(sql)
}

// dependenciesQuery reads what the object x — the catalog named $1 and the
// OID $2; a catalog that is no pg_catalog table gives no x — depends on, in
// the database read. parts is x and every piece folded into it; edges are
// the pg_depend rows leaving a part for something outside them, and the
// pg_shdepend rows of this database leaving one (to a role or a
// tablespace); up climbs from the far end of each edge to the object it is
// folded into, the one with no fold edge of its own — never x itself, as a
// far end folded into x is one of its parts, whose edge is left out. The
// first column is
// x's identity, NULL when x does not exist, and the row is there even with
// no edge (the LEFT JOIN), so "no such object" and "no dependencies" read
// apart in one statement.
var dependenciesQuery = dependencyFolded(`WITH RECURSIVE x AS (
	SELECT c.oid AS cls, $2::pg_catalog.oid AS obj FROM pg_catalog.pg_class c
	WHERE c.relname = $1::pg_catalog.text AND c.relnamespace = 'pg_catalog'::pg_catalog.regnamespace
), parts(cls, obj) AS (
	SELECT x.cls, x.obj FROM x
	UNION
	SELECT f.classid, f.objid FROM parts p
	JOIN pg_catalog.pg_depend f ON f.refclassid = p.cls AND f.refobjid = p.obj
	/*foldjoin f*/
	WHERE /*fold f*/
), edges AS (
	SELECT d.classid AS own_cls, d.objid AS own_obj, d.objsubid AS own_sub,
		d.refclassid AS cls, d.refobjid AS obj, d.refobjsubid AS sub, d.deptype::pg_catalog.text AS deptype, false AS shared
	FROM parts p JOIN pg_catalog.pg_depend d ON d.classid = p.cls AND d.objid = p.obj
	WHERE NOT EXISTS (SELECT FROM parts q WHERE q.cls = d.refclassid AND q.obj = d.refobjid)
	UNION ALL
	SELECT s.classid, s.objid, s.objsubid, s.refclassid, s.refobjid, 0, s.deptype::pg_catalog.text, true
	FROM parts p JOIN pg_catalog.pg_shdepend s ON s.classid = p.cls AND s.objid = p.obj
		AND s.dbid = (SELECT db.oid FROM pg_catalog.pg_database db WHERE db.datname = pg_catalog.current_database())
), up(cls, obj, rcls, robj) AS (
	SELECT DISTINCT e.cls, e.obj, e.cls, e.obj FROM edges e
	UNION
	SELECT u.cls, u.obj, f.refclassid, f.refobjid FROM up u
	JOIN pg_catalog.pg_depend f ON f.classid = u.rcls AND f.objid = u.robj
	/*foldjoin f*/
	WHERE /*fold f*/
)
SELECT xi.identity, r.catalog, r.oid, r.type, r.identity, r.deptype, r.shared, r.part_type, r.part, r.own_type, r.own
FROM (SELECT (pg_catalog.pg_identify_object(x.cls, x.obj, 0)).identity FROM x) AS xi
LEFT JOIN (
	SELECT DISTINCT rc.relname::pg_catalog.text AS catalog, u.robj AS oid,
		COALESCE(ro.type, '') AS type, COALESCE(ro.identity, '') AS identity, e.deptype, e.shared,
		CASE WHEN e.cls = u.rcls AND e.obj = u.robj AND e.sub = 0 THEN '' ELSE COALESCE(ep.type, '') END AS part_type,
		CASE WHEN e.cls = u.rcls AND e.obj = u.robj AND e.sub = 0 THEN '' ELSE COALESCE(ep.identity, '') END AS part,
		CASE WHEN e.own_cls = x.cls AND e.own_obj = x.obj AND e.own_sub = 0 THEN '' ELSE COALESCE(op.type, '') END AS own_type,
		CASE WHEN e.own_cls = x.cls AND e.own_obj = x.obj AND e.own_sub = 0 THEN '' ELSE COALESCE(op.identity, '') END AS own
	FROM edges e
	CROSS JOIN x
	JOIN up u ON u.cls = e.cls AND u.obj = e.obj
	JOIN pg_catalog.pg_class rc ON rc.oid = u.rcls
	CROSS JOIN LATERAL pg_catalog.pg_identify_object(u.rcls, u.robj, 0) AS ro
	CROSS JOIN LATERAL pg_catalog.pg_identify_object(e.cls, e.obj, e.sub) AS ep
	CROSS JOIN LATERAL pg_catalog.pg_identify_object(e.own_cls, e.own_obj, e.own_sub) AS op
	WHERE NOT EXISTS (SELECT FROM pg_catalog.pg_depend f
			/*foldjoin f*/
			WHERE f.classid = u.rcls AND f.objid = u.robj AND /*fold f*/)
) AS r ON true
ORDER BY r.type COLLATE pg_catalog."C", r.identity COLLATE pg_catalog."C", r.part COLLATE pg_catalog."C", r.own COLLATE pg_catalog."C", r.deptype COLLATE pg_catalog."C", r.shared`)

// dependentsQuery is dependenciesQuery the other way: edges are the
// pg_depend rows entering a part from outside them, and this database's
// pg_shdepend rows entering x itself — a shared object's, since nothing
// else is the far end of one.
var dependentsQuery = dependencyFolded(`WITH RECURSIVE x AS (
	SELECT c.oid AS cls, $2::pg_catalog.oid AS obj FROM pg_catalog.pg_class c
	WHERE c.relname = $1::pg_catalog.text AND c.relnamespace = 'pg_catalog'::pg_catalog.regnamespace
), parts(cls, obj) AS (
	SELECT x.cls, x.obj FROM x
	UNION
	SELECT f.classid, f.objid FROM parts p
	JOIN pg_catalog.pg_depend f ON f.refclassid = p.cls AND f.refobjid = p.obj
	/*foldjoin f*/
	WHERE /*fold f*/
), edges AS (
	SELECT d.refclassid AS own_cls, d.refobjid AS own_obj, d.refobjsubid AS own_sub,
		d.classid AS cls, d.objid AS obj, d.objsubid AS sub, d.deptype::pg_catalog.text AS deptype, false AS shared
	FROM parts p JOIN pg_catalog.pg_depend d ON d.refclassid = p.cls AND d.refobjid = p.obj
	WHERE NOT EXISTS (SELECT FROM parts q WHERE q.cls = d.classid AND q.obj = d.objid)
	UNION ALL
	SELECT s.refclassid, s.refobjid, 0, s.classid, s.objid, s.objsubid, s.deptype::pg_catalog.text, true
	FROM x JOIN pg_catalog.pg_shdepend s ON s.refclassid = x.cls AND s.refobjid = x.obj
		AND s.dbid = (SELECT db.oid FROM pg_catalog.pg_database db WHERE db.datname = pg_catalog.current_database())
), up(cls, obj, rcls, robj) AS (
	SELECT DISTINCT e.cls, e.obj, e.cls, e.obj FROM edges e
	UNION
	SELECT u.cls, u.obj, f.refclassid, f.refobjid FROM up u
	JOIN pg_catalog.pg_depend f ON f.classid = u.rcls AND f.objid = u.robj
	/*foldjoin f*/
	WHERE /*fold f*/
)
SELECT xi.identity, r.catalog, r.oid, r.type, r.identity, r.deptype, r.shared, r.part_type, r.part, r.own_type, r.own
FROM (SELECT (pg_catalog.pg_identify_object(x.cls, x.obj, 0)).identity FROM x) AS xi
LEFT JOIN (
	SELECT DISTINCT rc.relname::pg_catalog.text AS catalog, u.robj AS oid,
		COALESCE(ro.type, '') AS type, COALESCE(ro.identity, '') AS identity, e.deptype, e.shared,
		CASE WHEN e.cls = u.rcls AND e.obj = u.robj AND e.sub = 0 THEN '' ELSE COALESCE(ep.type, '') END AS part_type,
		CASE WHEN e.cls = u.rcls AND e.obj = u.robj AND e.sub = 0 THEN '' ELSE COALESCE(ep.identity, '') END AS part,
		CASE WHEN e.own_cls = x.cls AND e.own_obj = x.obj AND e.own_sub = 0 THEN '' ELSE COALESCE(op.type, '') END AS own_type,
		CASE WHEN e.own_cls = x.cls AND e.own_obj = x.obj AND e.own_sub = 0 THEN '' ELSE COALESCE(op.identity, '') END AS own
	FROM edges e
	CROSS JOIN x
	JOIN up u ON u.cls = e.cls AND u.obj = e.obj
	JOIN pg_catalog.pg_class rc ON rc.oid = u.rcls
	CROSS JOIN LATERAL pg_catalog.pg_identify_object(u.rcls, u.robj, 0) AS ro
	CROSS JOIN LATERAL pg_catalog.pg_identify_object(e.cls, e.obj, e.sub) AS ep
	CROSS JOIN LATERAL pg_catalog.pg_identify_object(e.own_cls, e.own_obj, e.own_sub) AS op
	WHERE NOT EXISTS (SELECT FROM pg_catalog.pg_depend f
			/*foldjoin f*/
			WHERE f.classid = u.rcls AND f.objid = u.robj AND /*fold f*/)
) AS r ON true
ORDER BY r.type COLLATE pg_catalog."C", r.identity COLLATE pg_catalog."C", r.part COLLATE pg_catalog."C", r.own COLLATE pg_catalog."C", r.deptype COLLATE pg_catalog."C", r.shared`)

// sharedDependencyQuery reads the pg_shdepend edges between shared
// objects (dbid 0) of the shared object x: own is the condition that s
// leaves x (or enters it, for dependents) and other the far end's
// (classid, objid, objsubid). Shared objects have no parts, so nothing is
// folded. The first column is x's identity, as dependenciesQuery's.
func sharedDependencyQuery(own, other string) string {
	return `WITH x AS (
	SELECT c.oid AS cls, $2::pg_catalog.oid AS obj FROM pg_catalog.pg_class c
	WHERE c.relname = $1::pg_catalog.text AND c.relnamespace = 'pg_catalog'::pg_catalog.regnamespace
)
SELECT xi.identity, r.catalog, r.oid, r.type, r.identity, r.deptype, r.shared, r.part_type, r.part, r.own_type, r.own
FROM (SELECT (pg_catalog.pg_identify_object(x.cls, x.obj, 0)).identity FROM x) AS xi
LEFT JOIN (
	SELECT DISTINCT rc.relname::pg_catalog.text AS catalog, e.obj AS oid,
		COALESCE(ro.type, '') AS type, COALESCE(ro.identity, '') AS identity, e.deptype, true AS shared,
		'' AS part_type, '' AS part, '' AS own_type, '' AS own
	FROM x
	JOIN pg_catalog.pg_shdepend s ON s.dbid = 0 AND ` + own + `
	CROSS JOIN LATERAL (SELECT ` + other + `, s.deptype::pg_catalog.text) AS e(cls, obj, sub, deptype)
	JOIN pg_catalog.pg_class rc ON rc.oid = e.cls
	CROSS JOIN LATERAL pg_catalog.pg_identify_object(e.cls, e.obj, e.sub) AS ro
) AS r ON true
ORDER BY r.type COLLATE pg_catalog."C", r.identity COLLATE pg_catalog."C", r.deptype COLLATE pg_catalog."C"`
}

var (
	sharedDependenciesQuery = sharedDependencyQuery("s.classid = x.cls AND s.objid = x.obj", "s.refclassid, s.refobjid, 0")
	sharedDependentsQuery   = sharedDependencyQuery("s.refclassid = x.cls AND s.refobjid = x.obj", "s.classid, s.objid, s.objsubid")
)

// dependentDatabasesQuery names the databases holding pg_shdepend edges to
// the shared object x, in name order (byte by byte, as a name sorts).
const dependentDatabasesQuery = `WITH x AS (
	SELECT c.oid AS cls, $2::pg_catalog.oid AS obj FROM pg_catalog.pg_class c
	WHERE c.relname = $1::pg_catalog.text AND c.relnamespace = 'pg_catalog'::pg_catalog.regnamespace
)
SELECT DISTINCT db.datname::pg_catalog.text COLLATE pg_catalog."C"
FROM x
JOIN pg_catalog.pg_shdepend s ON s.refclassid = x.cls AND s.refobjid = x.obj
JOIN pg_catalog.pg_database db ON db.oid = s.dbid
ORDER BY 1`

// readDependencies runs one of the dependency queries on q for addr and
// builds its edges. The first column, addr's identity, is NULL when no
// object is there; a row whose edge columns are NULL is the LEFT JOIN's
// row for an object with none.
func readDependencies(ctx context.Context, q Querier, what string, sql string, addr ObjectAddress) ([]Dependency, error) {
	type row struct {
		self *string
		dep  *Dependency
	}
	rows, err := scanRows(ctx, q, what, func(scan func(...any) error) (row, error) {
		var r row
		var catalog, typ, identity, deptype, partType, part, ownType, own *string
		var oid *OID
		var shared *bool
		if err := scan(&r.self, &catalog, &oid, &typ, &identity, &deptype, &shared, &partType, &part, &ownType, &own); err != nil {
			return r, err
		}
		if catalog != nil {
			r.dep = &Dependency{
				Catalog: *catalog, OID: *oid, Type: *typ, Identity: *identity,
				Kind: dependencyKind(*deptype, *shared), Shared: *shared,
				PartType: *partType, Part: *part, OwnPartType: *ownType, OwnPart: *own,
			}
		}
		return r, nil
	}, sql, addr.Catalog, addr.OID)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 || rows[0].self == nil {
		return nil, notFoundf("pgmo: %s: no object %d in %s", what, addr.OID, addr.Catalog)
	}
	var out []Dependency
	for _, r := range rows {
		if r.dep != nil {
			out = append(out, *r.dep)
		}
	}
	return out, nil
}

// checkAddress refuses an address no read can find: no catalog, or no OID —
// a Ref's, whose OID is never read (ErrHandleNotLoaded).
func checkAddress(what string, addr ObjectAddress) error {
	if addr.Catalog == "" {
		return invalidf("pgmo: %s: no catalog", what)
	}
	if addr.OID == 0 {
		return fmt.Errorf("pgmo: %s: %w", what, ErrHandleNotLoaded)
	}
	return nil
}

// Dependencies lists what the object at addr, in this database, depends
// on — SSMS's "Objects on which … depends": the objects its definition
// references (a view's tables and columns, a function's types, a table's
// schema, the function a trigger of its calls, the table a foreign key of
// its references) and, through pg_shdepend, the roles that own it or hold
// privileges on it and, for a partitioned table, its tablespace. Each part
// of the object is folded into it and each far end into its own object
// (the dependency.go header); one Dependency per edge, ordered by the
// other object's type and identity.
//
// A shared object (ObjectAddress.IsShared) depends on nothing inside a
// database: Server.Dependencies reads its edges, and this read refuses it
// (ErrInvalidRequest). An address with no OID — a Ref's — is
// ErrHandleNotLoaded; one naming no object, ErrNotFound.
func (d *Database) Dependencies(ctx context.Context, addr ObjectAddress) ([]Dependency, error) {
	what := fmt.Sprintf("list dependencies of %s %d in database %q", addr.Catalog, addr.OID, d.Name)
	if err := checkAddress(what, addr); err != nil {
		return nil, err
	}
	if addr.IsShared() {
		return nil, invalidf("pgmo: %s: a shared object; its edges are Server.Dependencies'", what)
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return readDependencies(ctx, q, what, dependenciesQuery, addr)
}

// Dependents lists what depends on the object at addr inside this database
// — SSMS's "Objects that depend on …", what DROP … CASCADE would reach
// first: the views reading a table, the tables whose foreign keys
// reference it, the sequences it owns, the functions taking a type, the
// objects in a schema, an extension's members. Folded and ordered as
// Dependencies.
//
// addr may be a shared object: the dependents of a role or a tablespace in
// this database are its pg_shdepend edges here — the objects the role owns,
// the ACLs and policies naming it — which DROP ROLE needs gone, and REASSIGN
// OWNED / DROP OWNED reach. Server.Dependents reads the shared objects'
// edges between one another, and Server.DependentDatabases names the
// databases holding more.
func (d *Database) Dependents(ctx context.Context, addr ObjectAddress) ([]Dependency, error) {
	what := fmt.Sprintf("list dependents of %s %d in database %q", addr.Catalog, addr.OID, d.Name)
	if err := checkAddress(what, addr); err != nil {
		return nil, err
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return readDependencies(ctx, q, what, dependentsQuery, addr)
}

// checkShared refuses an address that is no shared object: its edges are in
// some database's pg_depend, which Database.Dependencies reads.
func checkShared(what string, addr ObjectAddress) error {
	if err := checkAddress(what, addr); err != nil {
		return err
	}
	if !addr.IsShared() {
		return invalidf("pgmo: %s: not a shared object; its edges are Database.Dependencies'", what)
	}
	return nil
}

// Dependencies lists what the shared object at addr depends on among the
// shared objects: a database's or a tablespace's owner and the roles in
// its ACL, a parameter's grantees. Kinds and order as
// Database.Dependencies; nothing is folded, as a shared object has no
// parts. A database's own default tablespace is not a pg_shdepend edge.
func (s *Server) Dependencies(ctx context.Context, addr ObjectAddress) ([]Dependency, error) {
	what := fmt.Sprintf("list dependencies of %s %d", addr.Catalog, addr.OID)
	if err := checkShared(what, addr); err != nil {
		return nil, err
	}
	return readDependencies(ctx, s.on(ctx), what, sharedDependenciesQuery, addr)
}

// Dependents lists the shared objects that depend on the shared object at
// addr: the databases and tablespaces a role owns, those whose ACL names it,
// the parameters it holds privileges on. What depends on it inside a
// database is that database's Dependents; DependentDatabases names them.
func (s *Server) Dependents(ctx context.Context, addr ObjectAddress) ([]Dependency, error) {
	what := fmt.Sprintf("list dependents of %s %d", addr.Catalog, addr.OID)
	if err := checkShared(what, addr); err != nil {
		return nil, err
	}
	return readDependencies(ctx, s.on(ctx), what, sharedDependentsQuery, addr)
}

// DependentDatabases names, in order, the databases holding objects that
// depend on the shared object at addr — for a role, those where it owns an
// object or is named in an ACL, a policy or an initial privilege; for a
// tablespace, those with a partitioned table defaulting to it. Each one's
// Database.Dependents lists them. A database that names none is left out,
// as is a dependency only on the shared objects (Server.Dependents). An
// object not there is no error: no database names it.
func (s *Server) DependentDatabases(ctx context.Context, addr ObjectAddress) ([]string, error) {
	what := fmt.Sprintf("list databases depending on %s %d", addr.Catalog, addr.OID)
	if err := checkShared(what, addr); err != nil {
		return nil, err
	}
	return scanRows(ctx, s.on(ctx), what, func(scan func(...any) error) (string, error) {
		var name string
		return name, scan(&name)
	}, dependentDatabasesQuery, addr.Catalog, addr.OID)
}
