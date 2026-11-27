package pgmo

import (
	"context"
	"fmt"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Procedural languages
// ============================================================
//
// language.go reads a database's languages (PLAN.md P4.11) from
// pg_language, through the database's own pool: what a function's
// LANGUAGE names. A language is a database's object with no schema: its
// name is unique in the database.
//
// initdb creates internal, c and sql, and installs plpgsql as an
// extension: all four have OIDs below FirstNormalObjectId, so all four are
// IsSystem by the rule for an object with no schema (OPEN-THREADS.md
// § System objects) — plpgsql an extension member as well. Every other
// procedural language (plperl, plpython3u…) comes with its extension
// today; CREATE LANGUAGE on its own survives for compatibility.
//
// pg_language is readable by every role, so the listing is the same for
// all.

// Language is one language of a database. Read it with Database.Languages
// or Database.LanguageByName; Database.LanguageRef is a handle holding the
// name alone.
type Language struct {
	db *Database

	// Origin: IsSystem for initdb's (internal, c, sql, plpgsql);
	// Extension for one an extension owns (plpgsql, plperl…).
	Origin

	OID  OID
	Name string

	// Owner is the owning role's name (lanowner).
	Owner string

	// Procedural is false for internal, c and sql, whose functions the
	// server calls directly, and true for a language with a call handler
	// (lanispl).
	Procedural bool

	// Trusted reports a trusted language (lanpltrusted): one a role
	// without superuser may write functions in, given USAGE. c and internal
	// are not.
	Trusted bool

	// Handler is the call handler (lanplcallfoid), Inline the DO block
	// handler (laninline), Validator the function body validator
	// (lanvalidator); zero for none.
	Handler, Inline, Validator ObjectName

	// Privileges are the language's ACL (USAGE), per grantee, in stored
	// order. One never set reads as the hard-wired default: PUBLIC's USAGE
	// and the owner's (PrivilegesDefault).
	Privileges []PrivilegeGrant

	// PrivilegesDefault reports that the ACL was never set (lanacl NULL).
	PrivilegesDefault bool

	// Comment is COMMENT ON LANGUAGE's text, empty for none.
	Comment string
}

// Database is the Database the language was read from.
func (l *Language) Database() *Database { return l.db }

// QualifiedName is the language's name quoted for a statement. It has no
// schema.
func (l *Language) QualifiedName() string { return pgsql.QuoteIdent(l.Name) }

// languageSelect is the pg_language read a *Language is built from, ending
// in WHERE true without an ORDER BY; scanLanguage reads its row.
var languageSelect = `SELECT l.oid, l.lanname::pg_catalog.text,
	pg_catalog.pg_get_userbyid(l.lanowner)::pg_catalog.text,
	l.oid < 16384, COALESCE(ext.extname::pg_catalog.text, ''),
	l.lanispl, l.lanpltrusted,
	` + procedureName("fh") + `, ` + procedureName("fi") + `, ` + procedureName("fv") + `,
	l.lanacl IS NULL,
	` + aclColumns("p") + `,
	COALESCE(ds.description, '')
FROM pg_catalog.pg_language l
` + procedureJoin("l.lanplcallfoid", "fh") + `
` + procedureJoin("l.laninline", "fi") + `
` + procedureJoin("l.lanvalidator", "fv") + `
` + originJoin(catalogLanguage, "l.oid") + `
LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = l.oid
	AND ds.classoid = 'pg_catalog.pg_language'::pg_catalog.regclass AND ds.objsubid = 0
` + aclLateral("COALESCE(l.lanacl, pg_catalog.acldefault('l', l.lanowner))", "p") + `
WHERE true`

// scanLanguage builds a *Language of d from one languageSelect row.
func scanLanguage(d *Database, scan func(...any) error) (*Language, error) {
	l := &Language{db: d}
	var handler, inline, validator []string
	var acl privilegeArrays
	dest := []any{&l.OID, &l.Name, &l.Owner}
	dest = append(dest, scanOrigin(&l.Origin)...)
	dest = append(dest, &l.Procedural, &l.Trusted, &handler, &inline, &validator, &l.PrivilegesDefault)
	dest = append(dest, acl.dest()...)
	dest = append(dest, &l.Comment)
	if err := scan(dest...); err != nil {
		return nil, err
	}
	l.Handler, l.Inline, l.Validator = objectNameOf(handler), objectNameOf(inline), objectNameOf(validator)
	var err error
	l.Privileges, err = acl.all()
	return l, err
}

// languageFilterColumns are a language listing's ObjectFilter columns: no
// schema; System on the OID.
var languageFilterColumns = filterColumns{
	name:      "l.lanname",
	system:    "l.oid < 16384",
	extension: isExtensionMember(catalogLanguage, "l.oid"),
}

// Languages lists the database's languages by name, narrowed by f: Name on
// the language's name, System for initdb's (internal, c, sql, plpgsql),
// ExtensionMember for those an extension owns; a Schema criterion is
// ignored, as a language has none. They are read through the database's own
// pool.
func (d *Database) Languages(ctx context.Context, f ObjectFilter) ([]*Language, error) {
	where, args, err := f.clause(languageFilterColumns, nil)
	if err != nil {
		return nil, err
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, fmt.Sprintf("list languages of database %q", d.Name), func(scan func(...any) error) (*Language, error) {
		return scanLanguage(d, scan)
	}, languageSelect+where+`
ORDER BY l.lanname`, args...)
}

// LanguageByName reads the language name — a stored name, matched exactly
// (fold typed input with pgsql.FoldIdent first). One the database does not
// hold is ErrNotFound. Use LanguageRef instead when only a handle to write
// through is needed.
func (d *Database) LanguageByName(ctx context.Context, name string) (*Language, error) {
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return readByName(ctx, q, d, scanLanguage, languageSelect+`
	AND l.lanname = $1::pg_catalog.text`, []any{name},
		notFoundf("pgmo: language %s not found in database %q", pgsql.QuoteIdent(name), d.Name),
		fmt.Sprintf("read language %s of database %q", pgsql.QuoteIdent(name), d.Name))
}

// LanguageRef returns a handle for the language name without reading the
// catalog: Name is set and every other field is zero, OID included. It is
// not interchangeable with LanguageByName.
func (d *Database) LanguageRef(name string) *Language {
	return &Language{db: d, Name: name}
}
