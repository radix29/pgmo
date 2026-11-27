package pgmo

import (
	"cmp"
	"context"
	"fmt"
	"slices"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Extensions
// ============================================================
//
// extension.go reads a database's extensions (PLAN.md P4.10), through the
// database's own pool: those installed (pg_extension), what the server could
// install (pg_available_extensions and pg_available_extension_versions, read
// from the control files and scripts in the server's extension directory),
// and the objects an installed one owns (pg_depend deptype 'e'). SQL Server
// has no counterpart; gosmo has none to follow.
//
// An extension is a database's object with no schema of its own: its name
// is unique in the database, and its Schema is where it put its objects
// (CREATE EXTENSION … SCHEMA; a relocatable one moves with ALTER EXTENSION
// … SET SCHEMA). IsSystem is the rule for a schema-less object, an OID below
// FirstNormalObjectId (OPEN-THREADS.md § System objects): plpgsql, which
// initdb installs in every database, is the one such extension. An extension
// is never itself a member of another (ALTER EXTENSION … ADD takes no
// extension), so Origin.Extension is always empty.
//
// pg_extension is readable by every role, and the functions behind the two
// availability views are executable by PUBLIC (checked on pg15 and pg18), so
// every read here is the same for all roles.

// Extension is one extension installed in a database. Read it with
// Database.Extensions or Database.ExtensionByName; Database.ExtensionRef is
// a handle holding the name alone.
type Extension struct {
	db *Database

	// Origin: IsSystem for an extension initdb installed (plpgsql);
	// Extension is always empty — an extension is no other's member.
	Origin

	OID  OID
	Name string

	// Owner is the owning role's name (extowner).
	Owner string

	// Schema is the schema holding its objects (extnamespace).
	Schema string

	// Version is the installed version (extversion), as its scripts spell
	// it: an opaque string, ordered only by the update scripts there are.
	Version string

	// Relocatable reports whether ALTER EXTENSION … SET SCHEMA can move it
	// (extrelocatable).
	Relocatable bool

	// DefaultVersion is the version CREATE EXTENSION installs, and ALTER
	// EXTENSION … UPDATE updates to, by the control file the server has now
	// (pg_available_extensions); empty when the server has no control file
	// for it any longer (its package removed). It differing from Version
	// does not promise an update path.
	DefaultVersion string

	// Requires are the extensions it requires (its control file's requires,
	// pg_depend deptype 'n' from it to another extension), by name; nil for
	// none.
	Requires []string

	// ConfigTables are the tables and sequences its script marked with
	// pg_extension_config_dump (extconfig, extcondition), whose rows pg_dump
	// dumps although they are members, in the order marked; nil for none.
	ConfigTables []ExtensionConfigTable

	// Comment is COMMENT ON EXTENSION's text, empty for none; CREATE
	// EXTENSION sets it from the control file.
	Comment string
}

// ExtensionConfigTable is one configuration relation of an extension: a
// table or sequence whose data pg_dump dumps, a table's narrowed by
// Condition.
type ExtensionConfigTable struct {
	Table ObjectName

	// Condition is the WHERE clause pg_dump applies to the table's rows
	// (WHERE included), empty for all rows and for a sequence.
	Condition string
}

// Database is the Database the extension was read from.
func (e *Extension) Database() *Database { return e.db }

// QualifiedName is the extension's name quoted for a statement. It has no
// schema.
func (e *Extension) QualifiedName() string { return pgsql.QuoteIdent(e.Name) }

// extensionSelect is the pg_extension read an *Extension is built from,
// ending in WHERE true without an ORDER BY; scanExtension reads its row.
// The default version, the requirements, the configuration tables and the
// comment are joins: the first on pg_available_extensions() itself rather
// than its view, which would join pg_extension a second time.
var extensionSelect = `SELECT x.oid, x.extname::pg_catalog.text,
	pg_catalog.pg_get_userbyid(x.extowner)::pg_catalog.text,
	x.oid < 16384, '',
	n.nspname::pg_catalog.text, x.extversion, x.extrelocatable,
	COALESCE(a.default_version, ''), req.names,
	cfg.schemas, cfg.names, cfg.conditions,
	COALESCE(ds.description, '')
FROM pg_catalog.pg_extension x
JOIN pg_catalog.pg_namespace n ON n.oid = x.extnamespace
LEFT JOIN pg_catalog.pg_available_extensions() AS a(name, default_version, comment) ON a.name = x.extname
LEFT JOIN (SELECT d.objid, pg_catalog.array_agg(r.extname::pg_catalog.text ORDER BY r.extname) AS names
	FROM pg_catalog.pg_depend d
	JOIN pg_catalog.pg_extension r ON r.oid = d.refobjid
	WHERE d.classid = 'pg_catalog.pg_extension'::pg_catalog.regclass
		AND d.refclassid = 'pg_catalog.pg_extension'::pg_catalog.regclass AND d.deptype = 'n'
	GROUP BY d.objid) AS req ON req.objid = x.oid
LEFT JOIN (SELECT u.extoid,
		pg_catalog.array_agg(cn.nspname::pg_catalog.text ORDER BY u.ord) AS schemas,
		pg_catalog.array_agg(c.relname::pg_catalog.text ORDER BY u.ord) AS names,
		pg_catalog.array_agg(COALESCE(u.cond, '') ORDER BY u.ord) AS conditions
	FROM (SELECT x2.oid AS extoid, t.reloid, t.cond, t.ord
		FROM pg_catalog.pg_extension x2,
			ROWS FROM (pg_catalog.unnest(x2.extconfig), pg_catalog.unnest(x2.extcondition))
				WITH ORDINALITY AS t(reloid, cond, ord)) AS u
	JOIN pg_catalog.pg_class c ON c.oid = u.reloid
	JOIN pg_catalog.pg_namespace cn ON cn.oid = c.relnamespace
	GROUP BY u.extoid) AS cfg ON cfg.extoid = x.oid
LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = x.oid
	AND ds.classoid = 'pg_catalog.pg_extension'::pg_catalog.regclass AND ds.objsubid = 0
WHERE true`

// scanExtension builds an *Extension of d from one extensionSelect row.
func scanExtension(d *Database, scan func(...any) error) (*Extension, error) {
	e := &Extension{db: d}
	var cfgSchemas, cfgNames, cfgConditions []string
	dest := []any{&e.OID, &e.Name, &e.Owner}
	dest = append(dest, scanOrigin(&e.Origin)...)
	dest = append(dest, &e.Schema, &e.Version, &e.Relocatable, &e.DefaultVersion, &e.Requires,
		&cfgSchemas, &cfgNames, &cfgConditions, &e.Comment)
	if err := scan(dest...); err != nil {
		return nil, err
	}
	if len(cfgSchemas) != len(cfgNames) || len(cfgNames) != len(cfgConditions) {
		return nil, fmt.Errorf("configuration arrays of different lengths: %d schemas, %d names, %d conditions",
			len(cfgSchemas), len(cfgNames), len(cfgConditions))
	}
	for i := range cfgNames {
		e.ConfigTables = append(e.ConfigTables, ExtensionConfigTable{
			Table:     ObjectName{Schema: cfgSchemas[i], Name: cfgNames[i]},
			Condition: cfgConditions[i],
		})
	}
	return e, nil
}

// extensionFilterColumns are an extension listing's ObjectFilter columns:
// Name on the extension's name, Schema on the schema holding its objects,
// System on the OID; no extension is a member, so ExtensionMember true
// keeps none.
var extensionFilterColumns = filterColumns{
	name:      "x.extname",
	schema:    "n.nspname",
	system:    "x.oid < 16384",
	extension: "false",
}

// Extensions lists the extensions installed in the database by name,
// narrowed by f: Name on the extension's name, Schema on the schema holding
// its objects, System for those initdb installed (plpgsql),
// ExtensionMember true for none (an extension is never a member). They are
// read through the database's own pool.
func (d *Database) Extensions(ctx context.Context, f ObjectFilter) ([]*Extension, error) {
	where, args, err := f.clause(extensionFilterColumns, nil)
	if err != nil {
		return nil, err
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, fmt.Sprintf("list extensions of database %q", d.Name), func(scan func(...any) error) (*Extension, error) {
		return scanExtension(d, scan)
	}, extensionSelect+where+`
ORDER BY x.extname`, args...)
}

// ExtensionByName reads the installed extension name — a stored name,
// matched exactly (fold typed input with pgsql.FoldIdent first). One the
// database has not installed is ErrNotFound, whether or not the server
// could install it (Database.AvailableExtensionByName). Use ExtensionRef
// instead when only a handle to write through is needed.
func (d *Database) ExtensionByName(ctx context.Context, name string) (*Extension, error) {
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return readByName(ctx, q, d, scanExtension, extensionSelect+`
	AND x.extname = $1::pg_catalog.text`, []any{name},
		notFoundf("pgmo: extension %s not found in database %q", pgsql.QuoteIdent(name), d.Name),
		fmt.Sprintf("read extension %s of database %q", pgsql.QuoteIdent(name), d.Name))
}

// ExtensionRef returns a handle for the extension name without reading the
// catalog: Name is set and every other field is zero, OID included — so
// Members refuses it (ErrHandleNotLoaded). It is not interchangeable with
// ExtensionByName.
func (d *Database) ExtensionRef(name string) *Extension {
	return &Extension{db: d, Name: name}
}

// ExtensionMember is one object an extension owns (pg_depend deptype 'e'),
// as the server identifies it. It covers every kind of object an extension
// can hold — functions, types, operators, casts, operator classes,
// languages, schemas and more — so it names the object rather than reading
// it: OID in Catalog is the key a family's read takes, Identity the text a
// statement takes.
type ExtensionMember struct {
	// Catalog is the system catalog holding the object's row (pg_depend's
	// classid), unqualified: pg_proc, pg_class, pg_type, pg_operator…
	Catalog string

	// OID is the object's OID in Catalog.
	OID OID

	// Type is the kind of object, as pg_identify_object names it:
	// function, aggregate, table, view, sequence, type, operator, cast,
	// operator class, operator family, language, schema…
	Type string

	// Identity is pg_identify_object's identity: schema-qualified and
	// quoted whatever the session's search_path, routines and operators with
	// their argument types (public.f(integer,pg_catalog.text)), a cast as
	// (source AS target), an operator class with USING method — the form
	// ALTER EXTENSION … DROP <Type> takes.
	Identity string
}

// extensionMembersQuery reads, for the extension $1, every object pg_depend
// makes a member of it, by type and identity. pg_identify_object's schema
// and name columns are not read: they come back quoted ("E s"), so they
// are no stored names (found on pg15 and pg18).
const extensionMembersQuery = `SELECT c.relname::pg_catalog.text, d.objid, i.type, i.identity
FROM pg_catalog.pg_depend d
JOIN pg_catalog.pg_class c ON c.oid = d.classid
CROSS JOIN LATERAL pg_catalog.pg_identify_object(d.classid, d.objid, d.objsubid) AS i
WHERE d.refclassid = 'pg_catalog.pg_extension'::pg_catalog.regclass AND d.refobjid = $1
	AND d.deptype = 'e'
ORDER BY i.type, i.identity`

// Members reads the objects the extension owns, by type and then identity.
// An ExtensionRef is refused (ErrHandleNotLoaded); an extension dropped
// since it was read has none.
func (e *Extension) Members(ctx context.Context) ([]ExtensionMember, error) {
	what := "list members of extension " + e.QualifiedName()
	if e.OID == 0 {
		return nil, fmt.Errorf("pgmo: %s: %w", what, ErrHandleNotLoaded)
	}
	q, err := e.db.server.onDatabase(ctx, e.db.Name)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, what, func(scan func(...any) error) (ExtensionMember, error) {
		var m ExtensionMember
		return m, scan(&m.Catalog, &m.OID, &m.Type, &m.Identity)
	}, extensionMembersQuery, e.OID)
}

// AvailableExtension is an extension the server can install, as its
// control file describes it: one row of pg_available_extensions, with its
// versions from pg_available_extension_versions. What it says is the
// server's extension directory as of the read, so it is the same in every
// database but for what is installed there.
type AvailableExtension struct {
	db *Database

	Name string

	// DefaultVersion is the version CREATE EXTENSION installs when none is
	// named (the control file's default_version); empty when the control
	// file sets none.
	DefaultVersion string

	// InstalledVersion is the version installed in the database read, empty
	// when it is not installed there.
	InstalledVersion string

	// Comment is the control file's comment, empty for none.
	Comment string

	// Versions are the versions the server has an install script for, in
	// version order (compareExtensionVersions: number runs compared as
	// numbers, so 1.10 follows 1.9); nil when it has none.
	Versions []AvailableExtensionVersion
}

// AvailableExtensionVersion is one installable version of an extension: a
// row of pg_available_extension_versions, the control file's settings as a
// version's secondary control file overrides them.
type AvailableExtensionVersion struct {
	Version string

	// Installed reports whether this is the version installed in the
	// database read.
	Installed bool

	// Superuser reports whether installing or updating to it takes a
	// superuser, Trusted whether a role with CREATE on the database may
	// install it nonetheless (PostgreSQL 13's trusted extensions).
	Superuser bool
	Trusted   bool

	// Relocatable reports whether it can be moved to another schema once
	// installed.
	Relocatable bool

	// Schema is the schema it must be installed into, empty when it can go
	// into any.
	Schema string

	// Requires are the extensions it requires, as the control file lists
	// them; nil for none.
	Requires []string

	// Comment is the version's comment, empty for none.
	Comment string
}

// Database is the Database the available extension was read from: the one
// its InstalledVersion and Installed flags are of.
func (a *AvailableExtension) Database() *Database { return a.db }

// availableExtensionSelect reads pg_available_extensions joined to its
// versions, one row per version (one with no version columns for an
// extension with none), ending in WHERE true without an ORDER BY; a row is
// an availableExtensionRow, grouped by groupAvailableExtensions.
const availableExtensionSelect = `SELECT a.name::pg_catalog.text, COALESCE(a.default_version, ''),
	COALESCE(a.installed_version, ''), COALESCE(a.comment, ''),
	v.version, COALESCE(v.installed, false), COALESCE(v.superuser, false), COALESCE(v.trusted, false),
	COALESCE(v.relocatable, false), COALESCE(v.schema::pg_catalog.text, ''), v.requires::pg_catalog.text[],
	COALESCE(v.comment, '')
FROM pg_catalog.pg_available_extensions a
LEFT JOIN pg_catalog.pg_available_extension_versions v ON v.name = a.name
WHERE true`

// availableExtensionRow is one availableExtensionSelect row: the
// extension's columns and one version's, the version nil for none.
type availableExtensionRow struct {
	ext     AvailableExtension
	version *string
	v       AvailableExtensionVersion
}

// scanAvailableExtensionRow reads one availableExtensionSelect row.
func scanAvailableExtensionRow(scan func(...any) error) (availableExtensionRow, error) {
	var r availableExtensionRow
	err := scan(&r.ext.Name, &r.ext.DefaultVersion, &r.ext.InstalledVersion, &r.ext.Comment,
		&r.version, &r.v.Installed, &r.v.Superuser, &r.v.Trusted, &r.v.Relocatable, &r.v.Schema, &r.v.Requires,
		&r.v.Comment)
	if len(r.v.Requires) == 0 {
		r.v.Requires = nil
	}
	return r, err
}

// groupAvailableExtensions builds one *AvailableExtension of d per run of
// rows of one name — the rows ordered by name — each holding its versions
// in version order.
func groupAvailableExtensions(d *Database, rows []availableExtensionRow) []*AvailableExtension {
	var out []*AvailableExtension
	for _, r := range rows {
		if len(out) == 0 || out[len(out)-1].Name != r.ext.Name {
			a := r.ext
			a.db = d
			out = append(out, &a)
		}
		if r.version != nil {
			a := out[len(out)-1]
			v := r.v
			v.Version = *r.version
			a.Versions = append(a.Versions, v)
		}
	}
	for _, a := range out {
		slices.SortStableFunc(a.Versions, func(x, y AvailableExtensionVersion) int {
			return compareExtensionVersions(x.Version, y.Version)
		})
	}
	return out
}

// availableExtensionFilterColumns are an available extension listing's
// ObjectFilter columns: the name alone. An extension not installed has no
// schema, OID or membership to filter on.
var availableExtensionFilterColumns = filterColumns{name: "a.name"}

// AvailableExtensions lists the extensions the server can install, by
// name, each with its versions, narrowed by f's Name; its Schema, System
// and ExtensionMember criteria are ignored. Installed ones are among them
// while the server still has their control file. They are read through
// the database's own pool, which is the database InstalledVersion and
// Installed speak of.
func (d *Database) AvailableExtensions(ctx context.Context, f ObjectFilter) ([]*AvailableExtension, error) {
	where, args, err := f.clause(availableExtensionFilterColumns, nil)
	if err != nil {
		return nil, err
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	rows, err := scanRows(ctx, q, fmt.Sprintf("list available extensions of database %q", d.Name),
		scanAvailableExtensionRow, availableExtensionSelect+where+`
ORDER BY a.name`, args...)
	if err != nil {
		return nil, err
	}
	return groupAvailableExtensions(d, rows), nil
}

// AvailableExtensionByName reads the extension name the server can
// install, with its versions — a stored name, matched exactly (fold typed
// input with pgsql.FoldIdent first). One the server has no control file
// for is ErrNotFound, installed or not.
func (d *Database) AvailableExtensionByName(ctx context.Context, name string) (*AvailableExtension, error) {
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	rows, err := scanRows(ctx, q, fmt.Sprintf("read available extension %s of database %q", pgsql.QuoteIdent(name), d.Name),
		scanAvailableExtensionRow, availableExtensionSelect+`
	AND a.name = $1::pg_catalog.text`, name)
	if err != nil {
		return nil, err
	}
	if len(rows) == 0 {
		return nil, notFoundf("pgmo: extension %s not available on the server of database %q", pgsql.QuoteIdent(name), d.Name)
	}
	return groupAvailableExtensions(d, rows)[0], nil
}

// compareExtensionVersions orders two extension versions for display:
// split into runs of digits and of anything else, a digit run compared as a
// number (1.9 before 1.10), any other as bytes, a version that is a prefix
// of the other first (1.4 before 1.4-1). PostgreSQL itself gives versions
// no order — they are names, related only by the update scripts there are
// — so this is a reading aid, not what ALTER EXTENSION … UPDATE follows.
func compareExtensionVersions(a, b string) int {
	for a != "" && b != "" {
		ra, restA := versionRun(a)
		rb, restB := versionRun(b)
		da, db := isDigit(ra[0]), isDigit(rb[0])
		var c int
		switch {
		case da && db:
			c = compareDigitRuns(ra, rb)
		case da != db:
			// A number sorts before anything else at the same place.
			if da {
				c = -1
			} else {
				c = 1
			}
		default:
			c = cmp.Compare(ra, rb)
		}
		if c != 0 {
			return c
		}
		a, b = restA, restB
	}
	return cmp.Compare(len(a), len(b))
}

// versionRun splits s, not empty, into its leading run of digits or of
// non-digits and the rest.
func versionRun(s string) (run, rest string) {
	digit := isDigit(s[0])
	i := 1
	for i < len(s) && isDigit(s[i]) == digit {
		i++
	}
	return s[:i], s[i:]
}

func isDigit(c byte) bool { return '0' <= c && c <= '9' }

// compareDigitRuns compares two runs of decimal digits as numbers of any
// length: without leading zeros, the longer is larger; of one length, the
// bytes decide. Equal numbers spelt differently (01, 1) compare by length,
// so the order stays total.
func compareDigitRuns(a, b string) int {
	ta, tb := trimZeros(a), trimZeros(b)
	if c := cmp.Compare(len(ta), len(tb)); c != 0 {
		return c
	}
	if c := cmp.Compare(ta, tb); c != 0 {
		return c
	}
	return cmp.Compare(len(a), len(b))
}

// trimZeros drops a digit run's leading zeros, keeping one digit.
func trimZeros(s string) string {
	for len(s) > 1 && s[0] == '0' {
		s = s[1:]
	}
	return s
}
