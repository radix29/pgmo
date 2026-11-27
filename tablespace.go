package pgmo

import (
	"context"
	"fmt"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Tablespaces
// ============================================================
//
// tablespace.go reads the cluster's tablespaces (PLAN.md P4.11) from
// pg_tablespace — a shared catalog, so through the Server's own connection,
// as databases and roles are: a tablespace is the cluster's, not a
// database's, and every database sees the same rows. SQL Server's nearest
// counterpart is a filegroup, but a tablespace is a directory, not a part of
// one database: any database may place relations in it.
//
// initdb creates two, pg_default (the cluster's base directory) and
// pg_global (the shared catalogs'); they are IsSystem by the rule for an
// object with no schema, an OID below FirstNormalObjectId (OPEN-THREADS.md
// § System objects). A shared object is never an extension member
// (ALTER EXTENSION … ADD takes no tablespace), so Origin.Extension is always
// empty.
//
// pg_tablespace is readable by every role, so the listing is the same for
// all; the size is not (Tablespace.Size).

// Tablespace is one tablespace of the cluster. Read it with
// Server.Tablespaces or Server.TablespaceByName; Server.TablespaceRef is a
// handle holding the name alone.
type Tablespace struct {
	server *Server

	// Origin: IsSystem for pg_default and pg_global; Extension is always
	// empty — a tablespace is no extension's member.
	Origin

	OID  OID
	Name string

	// Owner is the owning role's name (spcowner).
	Owner string

	// Location is the directory CREATE TABLESPACE … LOCATION named
	// (pg_tablespace_location); empty for pg_default and pg_global, which
	// live in the data directory. An in-place tablespace (the developer
	// setting allow_in_place_tablespaces, LOCATION '') reads as its path
	// relative to the data directory, pg_tblspc/<oid>.
	Location string

	// Options are the tablespace's parameters (spcoptions:
	// seq_page_cost, random_page_cost, effective_io_concurrency,
	// maintenance_io_concurrency), as ALTER TABLESPACE … SET wrote them, in
	// stored order; nil for none.
	Options []ParameterSetting

	// Privileges are the tablespace's ACL (CREATE), per grantee, in stored
	// order. One never set reads as the hard-wired default: the owner's
	// alone (PrivilegesDefault).
	Privileges []PrivilegeGrant

	// PrivilegesDefault reports that the ACL was never set (spcacl NULL).
	PrivilegesDefault bool

	// Comment is COMMENT ON TABLESPACE's text, empty for none.
	Comment string
}

// Server is the Server the tablespace was read from.
func (t *Tablespace) Server() *Server { return t.server }

// QualifiedName is the tablespace's name quoted for a statement. It has no
// schema.
func (t *Tablespace) QualifiedName() string { return pgsql.QuoteIdent(t.Name) }

// tablespaceSelect is the pg_tablespace read a *Tablespace is built from,
// ending in WHERE true without an ORDER BY; scanTablespace reads its row.
// The comment is a join on pg_shdescription, the shared catalogs'.
var tablespaceSelect = `SELECT t.oid, t.spcname::pg_catalog.text,
	pg_catalog.pg_get_userbyid(t.spcowner)::pg_catalog.text,
	t.oid < 16384, '',
	pg_catalog.pg_tablespace_location(t.oid), t.spcoptions,
	t.spcacl IS NULL,
	` + aclColumns("p") + `,
	COALESCE(ds.description, '')
FROM pg_catalog.pg_tablespace t
LEFT JOIN pg_catalog.pg_shdescription ds ON ds.objoid = t.oid
	AND ds.classoid = 'pg_catalog.pg_tablespace'::pg_catalog.regclass
` + aclLateral("COALESCE(t.spcacl, pg_catalog.acldefault('t', t.spcowner))", "p") + `
WHERE true`

// scanTablespace builds a *Tablespace of s from one tablespaceSelect row.
func scanTablespace(s *Server, scan func(...any) error) (*Tablespace, error) {
	t := &Tablespace{server: s}
	var options []string
	var acl privilegeArrays
	dest := []any{&t.OID, &t.Name, &t.Owner}
	dest = append(dest, scanOrigin(&t.Origin)...)
	dest = append(dest, &t.Location, &options, &t.PrivilegesDefault)
	dest = append(dest, acl.dest()...)
	dest = append(dest, &t.Comment)
	if err := scan(dest...); err != nil {
		return nil, err
	}
	t.Options = parseSettings(options)
	var err error
	t.Privileges, err = acl.all()
	return t, err
}

// tablespaceFilterColumns are a tablespace listing's ObjectFilter columns:
// no schema; System on the OID; no tablespace is an extension member, so
// ExtensionMember true keeps none.
var tablespaceFilterColumns = filterColumns{
	name:      "t.spcname",
	system:    "t.oid < 16384",
	extension: "false",
}

// Tablespaces lists the cluster's tablespaces by name, narrowed by f: Name
// on the tablespace's name, System for pg_default and pg_global,
// ExtensionMember true for none; a Schema criterion is ignored, as a
// tablespace has none. Every tablespace is listed, whatever the role's
// privileges on it.
func (s *Server) Tablespaces(ctx context.Context, f ObjectFilter) ([]*Tablespace, error) {
	where, args, err := f.clause(tablespaceFilterColumns, nil)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, s.on(ctx), "list tablespaces", func(scan func(...any) error) (*Tablespace, error) {
		return scanTablespace(s, scan)
	}, tablespaceSelect+where+`
ORDER BY t.spcname`, args...)
}

// TablespaceByName reads the tablespace name — a stored name, matched
// exactly (fold typed input with pgsql.FoldIdent first). One the cluster
// does not hold is ErrNotFound. Use TablespaceRef instead when only a handle
// to write through is needed.
func (s *Server) TablespaceByName(ctx context.Context, name string) (*Tablespace, error) {
	return readByName(ctx, s.on(ctx), s, scanTablespace, tablespaceSelect+`
	AND t.spcname = $1::pg_catalog.text`, []any{name},
		notFoundf("pgmo: tablespace %s not found", pgsql.QuoteIdent(name)),
		fmt.Sprintf("read tablespace %s", pgsql.QuoteIdent(name)))
}

// TablespaceRef returns a handle for the tablespace name without reading the
// catalog: Name is set and every other field is zero, OID included. A read
// keyed by the name (Size) works on it. It is not interchangeable with
// TablespaceByName.
func (s *Server) TablespaceRef(name string) *Tablespace {
	return &Tablespace{server: s, Name: name}
}

// tablespaceSizeQuery is pg_tablespace_size of the tablespace named $1,
// through pg_tablespace so a missing name is no row (ErrNotFound) rather
// than the function's 42704.
const tablespaceSizeQuery = `SELECT pg_catalog.pg_tablespace_size(t.oid)
FROM pg_catalog.pg_tablespace t WHERE t.spcname = $1::pg_catalog.text`

// Size reads the tablespace's size on disk, in bytes: every file under its
// directory, of every database. The server walks the directory on every
// call, so it is read on demand rather than with the listing.
//
// The server answers only a role that holds CREATE on the tablespace or the
// privileges of pg_read_all_stats, and any role for the tablespace its
// session's database uses by default; any other gets its 42501
// (IsPermissionDenied). It keys on the name, so it works on a
// TablespaceRef; a tablespace the catalog does not hold is ErrNotFound.
func (t *Tablespace) Size(ctx context.Context) (int64, error) {
	var n int64
	err := queryRowScan(ctx, t.server.on(ctx), tablespaceSizeQuery, []any{t.Name}, &n)
	return foundRow(n, err, notFoundf("pgmo: tablespace %s not found", pgsql.QuoteIdent(t.Name)),
		fmt.Sprintf("read tablespace %s size", pgsql.QuoteIdent(t.Name)))
}
