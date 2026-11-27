package pgmo

import (
	"context"
	"fmt"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Collations
// ============================================================
//
// collation.go reads a database's collations (PLAN.md P4.11) from
// pg_collation, through the database's own pool. A collation is a schema's
// object: initdb fills pg_catalog with the C library's and ICU's locales
// (and from 17 the builtin provider's), and CREATE COLLATION adds more.
//
// The catalog's shape moves twice across the supported releases: 16 adds
// ICU tailoring rules (collicurules), and 17 renames colliculocale to
// colllocale, which then holds the builtin provider's locale as well — the
// same rename pg_database has (version_gate_inventory_test.go).
//
// A collation has no ACL: every role may use every collation, and
// pg_collation is readable by all, so the listing is the same for every
// role.

// Collation is one collation of a database. Read it with
// Database.Collations or Database.CollationByName; Database.CollationRef is
// a handle holding the name alone.
type Collation struct {
	db *Database

	// Origin: IsSystem for a collation in a system schema (initdb's, in
	// pg_catalog); Extension for one an extension owns.
	Origin

	OID    OID
	Schema string
	Name   string

	// Owner is the owning role's name (collowner).
	Owner string

	// Provider is the library implementing it (collprovider):
	// LocaleProviderDefault for the "default" collation alone, which stands
	// for the database's own.
	Provider LocaleProvider

	// Deterministic is false for a nondeterministic collation, one that may
	// call strings of different bytes equal (collisdeterministic).
	Deterministic bool

	// Encoding is the one encoding the collation works under, as
	// pg_encoding_to_char names it ("UTF8"); empty when it works under any
	// (collencoding -1). A collation whose encoding is not the database's
	// cannot be used there.
	Encoding string

	// Collate and CType are the libc locales (collcollate, collctype);
	// empty for the other providers.
	Collate, CType string

	// Locale is the ICU or builtin locale (colliculocale; colllocale from
	// 17, where it also carries the builtin provider's); empty for libc.
	Locale string

	// ICURules are ICU tailoring rules (collicurules, 16+); empty for none
	// and before 16.
	ICURules string

	// Version is the provider's collation version recorded when the
	// collation was created or last refreshed (collversion); empty when the
	// provider reports none.
	Version string

	// Comment is COMMENT ON COLLATION's text, empty for none.
	Comment string
}

// Database is the Database the collation was read from.
func (c *Collation) Database() *Database { return c.db }

// QualifiedName is the collation's name quoted for a statement:
// schema.name.
func (c *Collation) QualifiedName() string { return pgsql.QualifiedName(c.Schema, c.Name) }

// collationSelect is the pg_collation read a *Collation is built from on a
// server of version num, ending in WHERE true without an ORDER BY;
// scanCollation reads its row.
func collationSelect(num int) string {
	return `SELECT co.oid, n.nspname::pg_catalog.text, co.collname::pg_catalog.text,
	pg_catalog.pg_get_userbyid(co.collowner)::pg_catalog.text,
	` + originJoinColumns("n.nspname") + `,
	co.collprovider::pg_catalog.text, co.collisdeterministic,
	pg_catalog.pg_encoding_to_char(co.collencoding)::pg_catalog.text,
	COALESCE(co.collcollate, ''), COALESCE(co.collctype, ''),
	COALESCE(` + colSince(num, PostgreSQL17, "co.colllocale", "co.colliculocale") + `, ''),
	COALESCE(` + colSince(num, PostgreSQL16, "co.collicurules", "NULL::pg_catalog.text") + `, ''),
	COALESCE(co.collversion, ''),
	COALESCE(ds.description, '')
FROM pg_catalog.pg_collation co
JOIN pg_catalog.pg_namespace n ON n.oid = co.collnamespace
` + originJoin(catalogCollation, "co.oid") + `
LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = co.oid
	AND ds.classoid = 'pg_catalog.pg_collation'::pg_catalog.regclass AND ds.objsubid = 0
WHERE true`
}

// scanCollation builds a *Collation of d from one collationSelect row.
func scanCollation(d *Database, scan func(...any) error) (*Collation, error) {
	c := &Collation{db: d}
	var provider string
	dest := []any{&c.OID, &c.Schema, &c.Name, &c.Owner}
	dest = append(dest, scanOrigin(&c.Origin)...)
	dest = append(dest, &provider, &c.Deterministic, &c.Encoding, &c.Collate, &c.CType,
		&c.Locale, &c.ICURules, &c.Version, &c.Comment)
	if err := scan(dest...); err != nil {
		return nil, err
	}
	c.Provider = LocaleProvider(provider)
	return c, nil
}

// collationFilterColumns are a collation listing's ObjectFilter columns.
var collationFilterColumns = filterColumns{
	name:      "co.collname",
	schema:    "n.nspname",
	system:    systemSchema("n.nspname"),
	extension: isExtensionMember(catalogCollation, "co.oid"),
}

// Collations lists the database's collations by schema then name, narrowed
// by f: Name on the collation's name, Schema on its schema's, System for
// those in a system schema (initdb's), ExtensionMember for those an
// extension owns. Collations of every encoding are listed, those the
// database cannot use included (Collation.Encoding). They are read through
// the database's own pool.
func (d *Database) Collations(ctx context.Context, f ObjectFilter) ([]*Collation, error) {
	where, args, err := f.clause(collationFilterColumns, nil)
	if err != nil {
		return nil, err
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, fmt.Sprintf("list collations of database %q", d.Name), func(scan func(...any) error) (*Collation, error) {
		return scanCollation(d, scan)
	}, collationSelect(int(d.server.version))+where+`
ORDER BY n.nspname, co.collname, co.collencoding`, args...)
}

// CollationByName reads the collation schema.name — stored names, matched
// exactly (fold typed input with pgsql.FoldIdent first). schema is required
// (ErrSchemaRequired). The catalog may hold one collation of a name per
// encoding; the one read is the one the database can use — of its encoding
// or of any — as the server resolves the name. A name the database holds
// no usable collation by is ErrNotFound. Use CollationRef instead when only
// a handle to write through is needed.
func (d *Database) CollationByName(ctx context.Context, schema, name string) (*Collation, error) {
	qualified := pgsql.QualifiedName(schema, name)
	if schema == "" {
		return nil, fmt.Errorf("pgmo: read collation %q: %w", name, ErrSchemaRequired)
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return readByName(ctx, q, d, scanCollation, collationSelect(int(d.server.version))+`
	AND n.nspname = $1::pg_catalog.text AND co.collname = $2::pg_catalog.text
	AND co.collencoding IN (-1, pg_catalog.pg_char_to_encoding(pg_catalog.getdatabaseencoding()))
ORDER BY co.collencoding DESC`, []any{schema, name},
		notFoundf("pgmo: collation %s not found in database %q", qualified, d.Name),
		fmt.Sprintf("read collation %s of database %q", qualified, d.Name))
}

// CollationRef returns a handle for the collation schema.name without
// reading the catalog: Schema and Name are set and every other field is
// zero, OID included. It is not interchangeable with CollationByName.
func (d *Database) CollationRef(schema, name string) *Collation {
	return &Collation{db: d, Schema: schema, Name: name}
}
