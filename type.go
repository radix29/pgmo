package pgmo

import (
	"context"
	"fmt"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Types
// ============================================================
//
// type.go reads a database's types (PLAN.md P4.8) — pg_type rows of every
// kind but a domain (domain.go): base, composite, enum, pseudo, range and
// multirange — through the database's own pool. Each kind's own catalog is
// joined in: pg_enum's labels, the composite's pg_class attributes,
// pg_range for a range and for the range a multirange is of.
//
// What a listing leaves out, as psql's \dT does: the array type the server
// creates beside every type (_name: its element's typarray, read through
// the element), and the row type of a table, view, sequence or foreign
// table (a composite type whose typrelid is not a composite relation) —
// each is part of another object. A multirange is listed, though the server
// creates it with its range and drops it with it: it is a type of its own a
// column can be declared with, and CREATE TYPE … AS RANGE names it.
//
// What PostgreSQL adds over SQL Server: enums, composite types (SQL
// Server's table types are the nearest), range and multirange types, base
// types written in C (read-only: CREATE TYPE … (INPUT = …) needs a
// superuser and a C function), and pseudo-types. SQL Server's alias types
// are domains.

// TypeKind is pg_type.typtype for every kind a Type is (a domain, 'd', is
// a Domain).
type TypeKind string

const (
	TypeBase       TypeKind = "b" // CREATE TYPE name (INPUT = …, OUTPUT = …): a C type
	TypeComposite  TypeKind = "c" // CREATE TYPE name AS (…)
	TypeEnum       TypeKind = "e" // CREATE TYPE name AS ENUM (…)
	TypePseudo     TypeKind = "p" // record, anyelement, trigger …: pg_catalog's alone
	TypeRange      TypeKind = "r" // CREATE TYPE name AS RANGE (…)
	TypeMultirange TypeKind = "m" // created with its range
)

// String names the kind as CREATE TYPE's form for it: base, composite,
// enum, pseudo, range, multirange; an unknown code reads TypeKind("x").
func (k TypeKind) String() string {
	switch k {
	case TypeBase:
		return "base"
	case TypeComposite:
		return "composite"
	case TypeEnum:
		return "enum"
	case TypePseudo:
		return "pseudo"
	case TypeRange:
		return "range"
	case TypeMultirange:
		return "multirange"
	}
	return fmt.Sprintf("TypeKind(%q)", string(k))
}

// TypeCategory is pg_type.typcategory: the group the parser's implicit
// coercion rules treat a type as part of (CREATE TYPE's CATEGORY). A user
// type is TypeCategoryUser unless its creator chose another.
type TypeCategory string

const (
	TypeCategoryArray       TypeCategory = "A"
	TypeCategoryBoolean     TypeCategory = "B"
	TypeCategoryComposite   TypeCategory = "C"
	TypeCategoryDateTime    TypeCategory = "D"
	TypeCategoryEnum        TypeCategory = "E"
	TypeCategoryGeometric   TypeCategory = "G"
	TypeCategoryNetwork     TypeCategory = "I"
	TypeCategoryNumeric     TypeCategory = "N"
	TypeCategoryPseudo      TypeCategory = "P"
	TypeCategoryRange       TypeCategory = "R"
	TypeCategoryString      TypeCategory = "S"
	TypeCategoryTimespan    TypeCategory = "T"
	TypeCategoryUser        TypeCategory = "U"
	TypeCategoryBitString   TypeCategory = "V"
	TypeCategoryUnknown     TypeCategory = "X"
	TypeCategoryInternalUse TypeCategory = "Z"
)

// String names the category as the documentation's table of typcategory
// codes does (Array, Boolean, …, Internal-use); an unknown code — a
// creator may pick any ASCII letter — reads TypeCategory("x").
func (c TypeCategory) String() string {
	switch c {
	case TypeCategoryArray:
		return "Array"
	case TypeCategoryBoolean:
		return "Boolean"
	case TypeCategoryComposite:
		return "Composite"
	case TypeCategoryDateTime:
		return "Date/time"
	case TypeCategoryEnum:
		return "Enum"
	case TypeCategoryGeometric:
		return "Geometric"
	case TypeCategoryNetwork:
		return "Network address"
	case TypeCategoryNumeric:
		return "Numeric"
	case TypeCategoryPseudo:
		return "Pseudo-types"
	case TypeCategoryRange:
		return "Range"
	case TypeCategoryString:
		return "String"
	case TypeCategoryTimespan:
		return "Timespan"
	case TypeCategoryUser:
		return "User-defined"
	case TypeCategoryBitString:
		return "Bit-string"
	case TypeCategoryUnknown:
		return "unknown"
	case TypeCategoryInternalUse:
		return "Internal-use"
	}
	return fmt.Sprintf("TypeCategory(%q)", string(c))
}

// TypeAlignment is pg_type.typalign: the boundary a value of the type is
// stored on (CREATE TYPE's ALIGNMENT).
type TypeAlignment string

const (
	AlignChar   TypeAlignment = "c" // 1 byte
	AlignInt2   TypeAlignment = "s" // 2 bytes
	AlignInt4   TypeAlignment = "i" // 4 bytes
	AlignDouble TypeAlignment = "d" // 8 bytes
)

// String names the alignment as CREATE TYPE's ALIGNMENT spells it: char,
// int2, int4, double; an unknown code reads TypeAlignment("x").
func (a TypeAlignment) String() string {
	switch a {
	case AlignChar:
		return "char"
	case AlignInt2:
		return "int2"
	case AlignInt4:
		return "int4"
	case AlignDouble:
		return "double"
	}
	return fmt.Sprintf("TypeAlignment(%q)", string(a))
}

// TypeAttribute is one attribute of a composite type, in attnum order.
type TypeAttribute struct {
	Name string

	// TypeOID and Type are the attribute's type: Type with its modifier,
	// qualified outside pg_catalog whatever the search_path, as
	// Column.Type.
	TypeOID OID
	Type    string

	// Collation is the attribute's COLLATE when it differs from its type's
	// own; zero otherwise.
	Collation ObjectName
}

// Type is one type of a database, of any kind but a domain. Read it with
// Database.Types or Database.TypeByName; Database.TypeRef is a handle
// holding the name alone. The fields after Preferred are the kind's own,
// zero for the other kinds, except the I/O functions every type has.
type Type struct {
	db *Database

	// Origin: IsSystem for a type in a system schema (pg_catalog's,
	// information_schema's); Extension for one an extension owns
	// (citext, hstore).
	Origin

	OID    OID
	Schema string
	Name   string

	// Owner is the owning role's name (typowner).
	Owner string

	Kind     TypeKind
	Category TypeCategory

	// Preferred is CREATE TYPE's PREFERRED: the type implicit coercion
	// favours within its category.
	Preferred bool

	// Shell reports a shell type — CREATE TYPE name; with no definition
	// yet (typisdefined false), as a base type's I/O functions need one to
	// be declared over. The server stores it as a pseudo-type (Kind).
	Shell bool

	// Input … Subscript are the type's support functions (typinput,
	// typoutput, typreceive, typsend, typmodin, typmodout, typanalyze,
	// typsubscript), zero for none. Every type has an input and an output
	// function — enum_in, record_in, range_in for the kinds the server
	// writes them for — but CREATE TYPE names them for a base type alone.
	Input, Output, Receive, Send ObjectName
	TypmodInput, TypmodOutput    ObjectName
	Analyze, Subscript           ObjectName

	// InternalLength is typlen: the size of a fixed-length value in bytes,
	// -1 for a variable-length one (varlena), -2 for a NUL-terminated C
	// string.
	InternalLength int

	// PassedByValue, Alignment, Storage and Delimiter are CREATE TYPE's
	// PASSEDBYVALUE, ALIGNMENT, STORAGE and DELIMITER (an array's element
	// separator in text, ',' but for box's ';').
	PassedByValue bool
	Alignment     TypeAlignment
	Storage       StorageKind
	Delimiter     string

	// Element is a base type's ELEMENT — the type a subscript reads out of
	// it (point's float8) — as typeName spells it, empty for none.
	Element string

	// Default is a base type's DEFAULT literal; HasDefault tells it from
	// none, as an empty literal is a default.
	Default    string
	HasDefault bool

	// Collatable is a base type's COLLATABLE (typcollation set); a range's
	// collation is Collation.
	Collatable bool

	// Labels are an enum's values in their sort order (enumsortorder),
	// which ALTER TYPE … ADD VALUE BEFORE/AFTER can make differ from the
	// order they were added in; nil for none.
	Labels []string

	// Attributes are a composite type's, in attnum order; nil for none.
	Attributes []TypeAttribute

	// Subtype is a range's SUBTYPE as typeName spells it;
	// SubtypeOperatorClass its SUBTYPE_OPCLASS (the subtype's default
	// btree class when none was given — the catalog stores it either
	// way); Collation its COLLATION, zero for a subtype that does not
	// collate; Canonical and SubtypeDiff its CANONICAL and SUBTYPE_DIFF
	// functions, zero for none; Multirange the multirange type created
	// with it (MULTIRANGE_TYPE_NAME's, or the server's name).
	Subtype              string
	SubtypeOperatorClass ObjectName
	Collation            ObjectName
	Canonical            ObjectName
	SubtypeDiff          ObjectName
	Multirange           ObjectName

	// Range is a multirange's range type.
	Range ObjectName

	// Privileges are the type's ACL (USAGE), per grantee, in stored order.
	// One never set reads as the hard-wired default: PUBLIC's USAGE and
	// the owner's (PrivilegesDefault).
	Privileges []PrivilegeGrant

	// PrivilegesDefault reports that the ACL was never set (typacl NULL).
	PrivilegesDefault bool

	// Comment is COMMENT ON TYPE's text, empty for none.
	Comment string
}

// Database is the Database the type was read from.
func (ty *Type) Database() *Database { return ty.db }

// QualifiedName is the type's name quoted for a statement: schema.name.
func (ty *Type) QualifiedName() string { return pgsql.QualifiedName(ty.Schema, ty.Name) }

// typeNameWithModifier is the text expression naming a type as a column
// or attribute declares it, modifier included: ft.name, the
// format_type(oid, typmod) of the type whose schema is joined as tn —
// format_type's own text for a type in pg_catalog (whose arrays it spells
// elem[]), and for any other type that text qualified by the type's schema
// (an array type is always in its element type's, ALTER TYPE … SET SCHEMA
// moving both) unless format_type already qualified it, as it does when
// the session's search_path does not find the type. A type name holding a
// dot is quoted, so the text cannot start with the schema's quoted name and
// a dot unless it was qualified.
func typeNameWithModifier(tn, ft string) string {
	return `CASE WHEN ` + tn + `.nspname = 'pg_catalog'
			OR pg_catalog.starts_with(` + ft + `.name, pg_catalog.quote_ident(` + tn + `.nspname) || '.') THEN ` + ft + `.name
		ELSE pg_catalog.quote_ident(` + tn + `.nspname) || '.' || ` + ft + `.name END`
}

// objectNameJoin LEFT JOINs the row of catalog (pg_opclass, pg_collation,
// pg_type) whose OID is the expression oid as alias, and its schema —
// whose OID is alias.<prefix>namespace — as alias+"n"; none for 0.
// objectNameColumns read the pair.
func objectNameJoin(catalog, prefix, oid, alias string) string {
	return `LEFT JOIN ` + catalog + ` ` + alias + ` ON ` + alias + `.oid = ` + oid + `
LEFT JOIN pg_catalog.pg_namespace ` + alias + `n ON ` + alias + `n.oid = ` + alias + `.` + prefix + `namespace`
}

// objectNameColumns read the object objectNameJoin joined as alias, whose
// name column is alias.<prefix>name, as two texts, schema and name: empty
// for none.
func objectNameColumns(prefix, alias string) string {
	return `COALESCE(` + alias + `n.nspname::pg_catalog.text, ''), COALESCE(` + alias + `.` + prefix + `name::pg_catalog.text, '')`
}

// typeEnumLabels is the LEFT JOIN reading every enum's labels in sort
// order as el.labels — one grouped read of pg_enum, not a subquery per
// type (see originJoin). An enum with no labels reads NULL.
const typeEnumLabels = `LEFT JOIN (SELECT e.enumtypid,
		pg_catalog.array_agg(e.enumlabel::pg_catalog.text ORDER BY e.enumsortorder)
	FROM pg_catalog.pg_enum e
	GROUP BY e.enumtypid) AS el(type, labels) ON el.type = t.oid`

// typeAttributes is the LEFT JOIN reading every composite type's
// attributes in attnum order — names, type OIDs, type names, collation
// schemas and names, the collation only where it is not the type's own —
// as ca's arrays: one grouped read of the composite relations' attributes.
// A composite with no attributes reads NULLs.
var typeAttributes = `LEFT JOIN (SELECT a.attrelid,
		pg_catalog.array_agg(a.attname::pg_catalog.text ORDER BY a.attnum),
		pg_catalog.array_agg(a.atttypid ORDER BY a.attnum),
		pg_catalog.array_agg(` + typeNameWithModifier("atn", "aft") + ` ORDER BY a.attnum),
		pg_catalog.array_agg(CASE WHEN a.attcollation <> aty.typcollation THEN COALESCE(acn.nspname::pg_catalog.text, '') ELSE '' END ORDER BY a.attnum),
		pg_catalog.array_agg(CASE WHEN a.attcollation <> aty.typcollation THEN COALESCE(ac.collname::pg_catalog.text, '') ELSE '' END ORDER BY a.attnum)
	FROM pg_catalog.pg_attribute a
	JOIN pg_catalog.pg_class ar ON ar.oid = a.attrelid AND ar.relkind = 'c'
	JOIN pg_catalog.pg_type aty ON aty.oid = a.atttypid
	JOIN pg_catalog.pg_namespace atn ON atn.oid = aty.typnamespace
	CROSS JOIN LATERAL (SELECT pg_catalog.format_type(a.atttypid, a.atttypmod) AS name) AS aft
	LEFT JOIN pg_catalog.pg_collation ac ON ac.oid = a.attcollation
	LEFT JOIN pg_catalog.pg_namespace acn ON acn.oid = ac.collnamespace
	WHERE a.attnum > 0 AND NOT a.attisdropped
	GROUP BY a.attrelid) AS ca(rel, names, types, typenames, collschemas, collnames) ON ca.rel = t.typrelid`

// typeSelect is the pg_type read a *Type is built from, ending in its
// WHERE on the kinds a Type is without an ORDER BY; scanType reads its
// row. A row is left out when it is a domain, an array the server made
// for its element (the element's typarray, te joined by typeJoins), or a
// relation's row type that is not a composite type's (r).
var typeSelect = `SELECT t.oid, n.nspname::pg_catalog.text, t.typname::pg_catalog.text,
	pg_catalog.pg_get_userbyid(t.typowner)::pg_catalog.text,
	` + originJoinColumns("n.nspname") + `,
	t.typtype::pg_catalog.text, t.typcategory::pg_catalog.text, t.typispreferred, NOT t.typisdefined,
	` + procedureName("fin") + `, ` + procedureName("fout") + `, ` + procedureName("frecv") + `, ` + procedureName("fsend") + `,
	` + procedureName("fmin") + `, ` + procedureName("fmout") + `, ` + procedureName("fan") + `, ` + procedureName("fsub") + `,
	t.typlen::integer, t.typbyval, t.typalign::pg_catalog.text, t.typstorage::pg_catalog.text, t.typdelim::pg_catalog.text,
	COALESCE(` + typeName("te") + `, ''),
	COALESCE(t.typdefault, ''), t.typdefault IS NOT NULL, t.typcollation <> 0,
	el.labels,
	ca.names, ca.types, ca.typenames, ca.collschemas, ca.collnames,
	COALESCE(` + typeName("rs") + `, ''),
	` + objectNameColumns("opc", "ro") + `,
	` + objectNameColumns("coll", "rc") + `,
	` + procedureName("rcan") + `, ` + procedureName("rdiff") + `,
	` + objectNameColumns("typ", "rm") + `,
	` + objectNameColumns("typ", "mr") + `,
	t.typacl IS NULL,
	` + aclColumns("p") + `,
	COALESCE(ds.description, '')
FROM pg_catalog.pg_type t
JOIN pg_catalog.pg_namespace n ON n.oid = t.typnamespace
LEFT JOIN pg_catalog.pg_class r ON r.oid = t.typrelid
` + typeJoins("LEFT JOIN", "t.typelem", "te") + `
` + procedureJoin("t.typinput", "fin") + `
` + procedureJoin("t.typoutput", "fout") + `
` + procedureJoin("t.typreceive", "frecv") + `
` + procedureJoin("t.typsend", "fsend") + `
` + procedureJoin("t.typmodin", "fmin") + `
` + procedureJoin("t.typmodout", "fmout") + `
` + procedureJoin("t.typanalyze", "fan") + `
` + procedureJoin("t.typsubscript", "fsub") + `
LEFT JOIN pg_catalog.pg_range rg ON rg.rngtypid = t.oid
` + typeJoins("LEFT JOIN", "rg.rngsubtype", "rs") + `
` + objectNameJoin("pg_catalog.pg_opclass", "opc", "rg.rngsubopc", "ro") + `
` + objectNameJoin("pg_catalog.pg_collation", "coll", "rg.rngcollation", "rc") + `
` + procedureJoin("rg.rngcanonical", "rcan") + `
` + procedureJoin("rg.rngsubdiff", "rdiff") + `
` + objectNameJoin("pg_catalog.pg_type", "typ", "rg.rngmultitypid", "rm") + `
LEFT JOIN pg_catalog.pg_range mg ON mg.rngmultitypid = t.oid
` + objectNameJoin("pg_catalog.pg_type", "typ", "mg.rngtypid", "mr") + `
` + typeEnumLabels + `
` + typeAttributes + `
` + originJoin(catalogType, "t.oid") + `
LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = t.oid
	AND ds.classoid = 'pg_catalog.pg_type'::pg_catalog.regclass AND ds.objsubid = 0
` + aclLateral("COALESCE(t.typacl, pg_catalog.acldefault('T', t.typowner))", "p") + `
WHERE t.typtype <> 'd' AND (t.typrelid = 0 OR r.relkind = 'c') AND (te.oid IS NULL OR te.typarray <> t.oid)`

// typeFilterColumns are a type or domain listing's ObjectFilter columns.
var typeFilterColumns = filterColumns{
	name:      "t.typname",
	schema:    "n.nspname",
	system:    systemSchema("n.nspname"),
	extension: isExtensionMember(catalogType, "t.oid"),
}

// scanType builds a *Type of d from one typeSelect row.
func scanType(d *Database, scan func(...any) error) (*Type, error) {
	ty := &Type{db: d}
	var kind, category, align, storage string
	var fin, fout, frecv, fsend, fmin, fmout, fan, fsub, rcan, rdiff []string
	var attrNames, attrTypeNames, attrCollSchemas, attrCollNames []string
	var attrTypes []OID
	var acl privilegeArrays
	dest := []any{&ty.OID, &ty.Schema, &ty.Name, &ty.Owner}
	dest = append(dest, scanOrigin(&ty.Origin)...)
	dest = append(dest, &kind, &category, &ty.Preferred, &ty.Shell,
		&fin, &fout, &frecv, &fsend, &fmin, &fmout, &fan, &fsub,
		&ty.InternalLength, &ty.PassedByValue, &align, &storage, &ty.Delimiter,
		&ty.Element, &ty.Default, &ty.HasDefault, &ty.Collatable,
		&ty.Labels,
		&attrNames, &attrTypes, &attrTypeNames, &attrCollSchemas, &attrCollNames,
		&ty.Subtype, &ty.SubtypeOperatorClass.Schema, &ty.SubtypeOperatorClass.Name,
		&ty.Collation.Schema, &ty.Collation.Name, &rcan, &rdiff,
		&ty.Multirange.Schema, &ty.Multirange.Name, &ty.Range.Schema, &ty.Range.Name,
		&ty.PrivilegesDefault)
	dest = append(dest, acl.dest()...)
	dest = append(dest, &ty.Comment)
	if err := scan(dest...); err != nil {
		return nil, err
	}
	ty.Kind, ty.Category = TypeKind(kind), TypeCategory(category)
	ty.Alignment, ty.Storage = TypeAlignment(align), StorageKind(storage)
	ty.Input, ty.Output, ty.Receive, ty.Send = objectNameOf(fin), objectNameOf(fout), objectNameOf(frecv), objectNameOf(fsend)
	ty.TypmodInput, ty.TypmodOutput = objectNameOf(fmin), objectNameOf(fmout)
	ty.Analyze, ty.Subscript = objectNameOf(fan), objectNameOf(fsub)
	ty.Canonical, ty.SubtypeDiff = objectNameOf(rcan), objectNameOf(rdiff)
	n := len(attrNames)
	if len(attrTypes) != n || len(attrTypeNames) != n || len(attrCollSchemas) != n || len(attrCollNames) != n {
		return nil, fmt.Errorf("attribute arrays of different lengths: %d names, %d types, %d type names, %d collation schemas, %d collation names",
			n, len(attrTypes), len(attrTypeNames), len(attrCollSchemas), len(attrCollNames))
	}
	for i := range n {
		ty.Attributes = append(ty.Attributes, TypeAttribute{Name: attrNames[i], TypeOID: attrTypes[i], Type: attrTypeNames[i],
			Collation: ObjectName{Schema: attrCollSchemas[i], Name: attrCollNames[i]}})
	}
	var err error
	ty.Privileges, err = acl.all()
	return ty, err
}

// Types lists the database's types of every kind but a domain
// (Database.Domains) by schema then name, narrowed by f: Name on the
// type's name, Schema on its schema's, System for those in a system schema
// (pg_catalog's hundred-odd), ExtensionMember for those an extension owns.
// The arrays the server makes for each type and the row types of tables,
// views and sequences are not listed. Every type is listed whatever the
// role's privileges on it. They are read through the database's own pool.
func (d *Database) Types(ctx context.Context, f ObjectFilter) ([]*Type, error) {
	where, args, err := f.clause(typeFilterColumns, nil)
	if err != nil {
		return nil, err
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, fmt.Sprintf("list types of database %q", d.Name), func(scan func(...any) error) (*Type, error) {
		return scanType(d, scan)
	}, typeSelect+where+`
ORDER BY n.nspname, t.typname`, args...)
}

// TypeByName reads the type schema.name — stored names, matched exactly
// (fold typed input with pgsql.FoldIdent first). schema is required
// (ErrSchemaRequired). A name the database holds no Type by is ErrNotFound:
// none, a domain (Database.DomainByName), an array the server made (_name),
// or a table's row type. Use TypeRef instead when only a handle to write
// through is needed.
func (d *Database) TypeByName(ctx context.Context, schema, name string) (*Type, error) {
	qualified := pgsql.QualifiedName(schema, name)
	if schema == "" {
		return nil, fmt.Errorf("pgmo: read type %q: %w", name, ErrSchemaRequired)
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return readByName(ctx, q, d, scanType, typeSelect+`
	AND n.nspname = $1::pg_catalog.text AND t.typname = $2::pg_catalog.text`, []any{schema, name},
		notFoundf("pgmo: type %s not found in database %q", qualified, d.Name),
		fmt.Sprintf("read type %s of database %q", qualified, d.Name))
}

// TypeRef returns a handle for the type schema.name without reading the
// catalog: Schema and Name are set and every other field is zero, OID and
// Kind included. It is not interchangeable with TypeByName.
func (d *Database) TypeRef(schema, name string) *Type {
	return &Type{db: d, Schema: schema, Name: name}
}
