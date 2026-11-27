package pgmo

import (
	"context"
	"fmt"
	"strings"
)

// ============================================================
// Databases
// ============================================================
//
// database.go reads the cluster's databases (PLAN.md P4.1). pg_database is a
// shared catalog: every database's row is visible from any one, so the
// listing, the by-name read and the size all go through the Server's own
// database (s.on(ctx)) — never the database being described, which may not
// take connections at all (template0, ALLOW_CONNECTIONS false, an invalid
// database).
//
// What gosmo's Database carries and PostgreSQL has no notion of — a recovery
// model, a compatibility level, a state (ONLINE, RESTORING…), a read-only
// flag, a creation date — is not here. What PostgreSQL has instead: the
// locale provider and its locale, the template flag, datallowconn, the
// connection limit, the default tablespace, and the database-wide parameter
// settings ALTER DATABASE … SET keeps in pg_db_role_setting.

// LocaleProvider is pg_database.datlocprovider (and, from P4.11,
// pg_collation.collprovider): the library that implements the database's
// default collation. It is the catalog's one-letter code, so a provider a
// later release adds is still read — String names the ones pgmo knows.
type LocaleProvider string

const (
	LocaleProviderLibc    LocaleProvider = "c" // the C library's locales
	LocaleProviderICU     LocaleProvider = "i" // ICU
	LocaleProviderBuiltin LocaleProvider = "b" // PostgreSQL's own, from 17

	// LocaleProviderDefault is a collation's alone: the "default" collation,
	// which stands for the database's own provider and locale.
	LocaleProviderDefault LocaleProvider = "d"
)

// String names the provider as CREATE DATABASE's LOCALE_PROVIDER spells it:
// libc, icu, builtin (and a collation's default); an unknown code reads
// LocaleProvider("x").
func (p LocaleProvider) String() string {
	switch p {
	case LocaleProviderLibc:
		return "libc"
	case LocaleProviderICU:
		return "icu"
	case LocaleProviderBuiltin:
		return "builtin"
	case LocaleProviderDefault:
		return "default"
	}
	return fmt.Sprintf("LocaleProvider(%q)", string(p))
}

// ParameterSetting is one configuration parameter default stored in
// pg_db_role_setting — what ALTER DATABASE … SET (and ALTER ROLE … SET)
// writes, applied when a session starts.
type ParameterSetting struct {
	// Name is the parameter's name as stored: lower case for a built-in
	// parameter, as typed for a custom one (myext.option).
	Name string

	// Value is the stored value, as SHOW reports it: a list parameter keeps
	// its elements' quotes (search_path reads "$user", public). It is not a
	// SQL literal: Values splits it as SetParameter takes it back.
	Value string
}

// parseSettings splits pg_db_role_setting.setconfig's "name=value" entries.
// A parameter name never holds '=' (the server refuses one), so the first
// '=' ends it; the value may hold more (search_path = "a=b").
func parseSettings(config []string) []ParameterSetting {
	if len(config) == 0 {
		return nil
	}
	out := make([]ParameterSetting, 0, len(config))
	for _, c := range config {
		name, value, _ := strings.Cut(c, "=")
		out = append(out, ParameterSetting{Name: name, Value: value})
	}
	return out
}

// Database is one database of the cluster, as pg_database describes it.
// Read it with Server.Databases or Server.DatabaseByName; Server.DatabaseRef
// is a handle holding the name alone.
type Database struct {
	server *Server

	OID  OID
	Name string

	// Owner is the owning role's name (datdba).
	Owner string

	// Encoding is the server encoding's name: UTF8, LATIN1, SQL_ASCII…
	Encoding string

	// LocaleProvider is the default collation's provider; Collate and CType
	// are LC_COLLATE and LC_CTYPE (libc locale names, used for LC_CTYPE
	// whatever the provider). Locale is the ICU or builtin provider's locale
	// (daticulocale before 17, datlocale from 17) and empty under libc.
	LocaleProvider LocaleProvider
	Collate        string
	CType          string
	Locale         string

	// ICURules is the ICU tailoring rules (CREATE DATABASE … ICU_RULES, 16+);
	// empty for none, and always on 15.
	ICURules string

	// CollationVersion is the collation version recorded when the database
	// was created or last refreshed (ALTER DATABASE … REFRESH COLLATION
	// VERSION); empty when the provider reports none.
	CollationVersion string

	// IsTemplate is datistemplate: any role with CREATEDB may clone it, not
	// only its owner. template0 and template1 are; a user database can be.
	IsTemplate bool

	// AllowConnections is datallowconn: false on template0 and on a database
	// ALTER DATABASE … ALLOW_CONNECTIONS false closed. Nobody connects to it,
	// superusers included.
	AllowConnections bool

	// ConnectionLimit is datconnlimit as stored: -1 for no limit, otherwise
	// the most concurrent connections (superusers and background workers are
	// not counted). -2 marks an invalid database (IsInvalid).
	ConnectionLimit int

	// Tablespace is the default tablespace's name: where the database's
	// catalogs live and its objects go unless they name another.
	Tablespace string

	// Settings are the parameter defaults ALTER DATABASE … SET stored for
	// every role (pg_db_role_setting with setrole 0), in stored order; nil
	// for none. The defaults stored for one role in this database (ALTER
	// ROLE … IN DATABASE … SET) are the role's (P4.2).
	Settings []ParameterSetting

	// Comment is COMMENT ON DATABASE's text, empty for none.
	Comment string
}

// connLimitInvalid is the datconnlimit a DROP DATABASE interrupted part way
// leaves (DATCONNLIMIT_INVALID_DB, since the August 2023 minor releases).
const connLimitInvalid = -2

// Server is the Server the database was read from.
func (d *Database) Server() *Server { return d.server }

// IsInvalid reports a database a DROP DATABASE was interrupted in: no one
// can connect to it, and the one thing left to do with it is DROP DATABASE.
// It is derived from ConnectionLimit, so it answers false on a DatabaseRef.
func (d *Database) IsInvalid() bool { return d.ConnectionLimit == connLimitInvalid }

// databaseSelect is the pg_database read a *Database is built from, for a
// server at num, without its WHERE or ORDER BY; scanDatabase reads its row.
//
// The locale column was daticulocale until 16 and datlocale from 17;
// daticurules is 16+. Both are nullable, as are datcollversion and the
// comment, and read as the empty string for none. Settings come from one correlated
// subquery — pg_db_role_setting holds at most one row per (database, role).
func databaseSelect(num int) string {
	return `SELECT d.oid, d.datname::pg_catalog.text,
	pg_catalog.pg_get_userbyid(d.datdba)::pg_catalog.text,
	pg_catalog.pg_encoding_to_char(d.encoding)::pg_catalog.text,
	d.datlocprovider::pg_catalog.text, d.datcollate, d.datctype,
	COALESCE(` + colSince(num, PostgreSQL17, "d.datlocale", "d.daticulocale") + `, ''),
	COALESCE(` + colSince(num, PostgreSQL16, "d.daticurules", "NULL::pg_catalog.text") + `, ''),
	COALESCE(d.datcollversion, ''),
	d.datistemplate, d.datallowconn, d.datconnlimit, t.spcname::pg_catalog.text,
	(SELECT s.setconfig FROM pg_catalog.pg_db_role_setting s WHERE s.setdatabase = d.oid AND s.setrole = 0),
	COALESCE(pg_catalog.shobj_description(d.oid, 'pg_database'), '')
FROM pg_catalog.pg_database d
JOIN pg_catalog.pg_tablespace t ON t.oid = d.dattablespace
WHERE true`
}

// databaseFilterColumns is what an ObjectFilter compares against in the
// database listing: the name only. A database has no schema, and none is a
// system object by pgmo's rule (a schema's), so Schema, System and
// ExtensionMember narrow nothing here.
var databaseFilterColumns = filterColumns{name: "d.datname"}

// scanDatabase builds a *Database of s from one databaseSelect row.
func scanDatabase(s *Server, scan func(...any) error) (*Database, error) {
	d := &Database{server: s}
	var provider string
	var config []string
	if err := scan(&d.OID, &d.Name, &d.Owner, &d.Encoding,
		&provider, &d.Collate, &d.CType, &d.Locale, &d.ICURules, &d.CollationVersion,
		&d.IsTemplate, &d.AllowConnections, &d.ConnectionLimit, &d.Tablespace,
		&config, &d.Comment); err != nil {
		return nil, err
	}
	d.LocaleProvider = LocaleProvider(provider)
	d.Settings = parseSettings(config)
	return d, nil
}

// Databases lists the cluster's databases, by name, narrowed by f (its Name
// criteria; a database has no schema and is never a system object). Every
// database is listed, whatever the role's privileges on it: pg_database is
// readable by all — Server.DatabasePrivileges says which the role may
// connect to.
func (s *Server) Databases(ctx context.Context, f ObjectFilter) ([]*Database, error) {
	where, args, err := f.clause(databaseFilterColumns, nil)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, s.on(ctx), "list databases", func(scan func(...any) error) (*Database, error) {
		return scanDatabase(s, scan)
	}, databaseSelect(int(s.version))+where+`
ORDER BY d.datname`, args...)
}

// DatabaseByName reads the database named name — a stored name, matched
// exactly (fold typed input with pgsql.FoldIdent first). One the catalog
// does not hold is ErrNotFound. Use DatabaseRef instead when only a handle to
// write through is needed.
func (s *Server) DatabaseByName(ctx context.Context, name string) (*Database, error) {
	return readByName(ctx, s.on(ctx), s, scanDatabase, databaseSelect(int(s.version))+`
	AND d.datname = $1::pg_catalog.text`, []any{name},
		notFoundf("pgmo: database %q not found", name), fmt.Sprintf("read database %q", name))
}

// DatabaseRef returns a handle for the database named name without reading
// the catalog: Name is set and every other field is zero. It is what a write
// to a database the caller knows exists goes through — one it has just
// created, or one created only under WithScript, which DatabaseByName would
// not find. A read keyed by the name (Size) works on it; IsInvalid answers
// false. It is not interchangeable with DatabaseByName.
func (s *Server) DatabaseRef(name string) *Database {
	return &Database{server: s, Name: name}
}

// databaseSizeQuery is pg_database_size of the database named $1, through
// pg_database so a missing name is no row (ErrNotFound) rather than the
// function's 3D000.
const databaseSizeQuery = `SELECT pg_catalog.pg_database_size(d.oid)
FROM pg_catalog.pg_database d WHERE d.datname = $1::pg_catalog.text`

// Size reads the database's size on disk, in bytes: every file of its
// directory in each tablespace. The server walks those directories on every
// call, so it is read on demand rather than with the listing.
//
// The server answers only a role that holds CONNECT on the database or the
// privileges of pg_read_all_stats; any other gets its 42501
// (IsPermissionDenied). It keys on the name, so it works on a DatabaseRef; a
// database the catalog does not hold is ErrNotFound.
func (d *Database) Size(ctx context.Context) (int64, error) {
	var n int64
	err := queryRowScan(ctx, d.server.on(ctx), databaseSizeQuery, []any{d.Name}, &n)
	return foundRow(n, err, notFoundf("pgmo: database %q not found", d.Name), fmt.Sprintf("read database %q size", d.Name))
}
