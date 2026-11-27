package pgmo

import (
	"context"
	"fmt"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Sequences
// ============================================================
//
// sequence.go reads a database's sequences (PLAN.md P4.7): pg_class rows of
// kind S with their pg_sequence row, through the database's own pool — what
// the pg_sequences view shows, read from the catalogs beneath it, plus what
// it leaves out: the OID, the persistence, the ACL, the comment, the origin
// and the column the sequence is owned by.
//
// The listing is catalog state alone, identical for every role. The last
// value is not in it: it is read from the sequence itself
// (pg_sequence_last_value), which opens each sequence and needs SELECT or
// USAGE on it — pg15 refuses a role without either (42501) where pg18
// answers NULL, as it does for another session's temporary sequence, so
// pg_sequences' own column cannot tell "never called" from "not yours to
// read". Sequence.LastValue reads it on demand and tells them apart.
//
// What PostgreSQL adds over SQL Server: ownership by a column (OWNED BY,
// which serial and identity columns set: the sequence is dropped with the
// column), identity sequences (deptype 'i', altered through the column's
// ALTER TABLE … ALTER COLUMN, never ALTER SEQUENCE … OWNED BY), unlogged
// and temporary sequences, and a per-session cache. What it lacks: decimal
// sequences — the type is smallint, integer or bigint, so every bound fits
// an int64.

// Sequence is one sequence of a database. Read it with Database.Sequences or
// Database.SequenceByName; Database.SequenceRef is a handle holding the name
// alone.
type Sequence struct {
	db *Database

	// Origin: IsSystem for a sequence in a system schema (every session's
	// temporary sequences, in pg_temp_N); Extension for one an extension
	// owns.
	Origin

	OID    OID
	Schema string
	Name   string

	// Owner is the owning role's name (relowner).
	Owner string

	// Persistence is CREATE [UNLOGGED | TEMPORARY] SEQUENCE's.
	Persistence Persistence

	// DataType is the AS type as format_type spells it: smallint, integer
	// or bigint, the only three a sequence takes.
	DataType string

	// Start is START WITH: where the sequence begins, and where a RESTART
	// without a value goes back to.
	Start int64

	// Increment is INCREMENT BY, negative for a descending sequence.
	Increment int64

	// MinValue and MaxValue are the bounds, the type's own when NO
	// MINVALUE / NO MAXVALUE was given — the catalog stores the value
	// either way.
	MinValue int64
	MaxValue int64

	// Cache is CACHE: how many values a session takes at once, 1 for none.
	Cache int64

	// Cycle is CYCLE: past a bound the sequence wraps to the other, rather
	// than failing.
	Cycle bool

	// OwnedBy is the table whose column (OwnedByColumn) owns the sequence —
	// OWNED BY, which serial and identity columns set: the sequence is
	// dropped with the column. Zero for none.
	OwnedBy       ObjectName
	OwnedByColumn string

	// Identity reports that the sequence is an identity column's
	// (GENERATED … AS IDENTITY): it is altered and dropped through the
	// column, never on its own.
	Identity bool

	// Privileges are the sequence's ACL, per grantee, in stored order. One
	// never set reads as the hard-wired default, its owner's every privilege
	// (PrivilegesDefault).
	Privileges []PrivilegeGrant

	// PrivilegesDefault reports that the ACL was never set (relacl NULL).
	PrivilegesDefault bool

	// Comment is COMMENT ON SEQUENCE's text, empty for none.
	Comment string
}

// Database is the Database the sequence was read from.
func (seq *Sequence) Database() *Database { return seq.db }

// QualifiedName is the sequence's name quoted for a statement: schema.name.
func (seq *Sequence) QualifiedName() string { return pgsql.QualifiedName(seq.Schema, seq.Name) }

// IsOwned reports whether a column owns the sequence (OwnedBy).
func (seq *Sequence) IsOwned() bool { return seq.OwnedBy.Name != "" }

// sequenceSelect is the pg_class and pg_sequence read a *Sequence is built
// from, ending in its WHERE on the kind without an ORDER BY; scanSequence
// reads its row. The extension, the owning column and the comment are joins,
// not a subquery per row (originJoin). The owning column is the sequence's
// one pg_depend row on a pg_class column of deptype 'a' (OWNED BY) or 'i'
// (identity); a sequence has no other dependency on a relation.
var sequenceSelect = `SELECT c.oid, n.nspname::pg_catalog.text, c.relname::pg_catalog.text,
	pg_catalog.pg_get_userbyid(c.relowner)::pg_catalog.text,
	` + originJoinColumns("n.nspname") + `,
	c.relpersistence::pg_catalog.text,
	pg_catalog.format_type(s.seqtypid, NULL),
	s.seqstart, s.seqincrement, s.seqmin, s.seqmax, s.seqcache, s.seqcycle,
	COALESCE(otn.nspname::pg_catalog.text, ''), COALESCE(ot.relname::pg_catalog.text, ''),
	COALESCE(oa.attname::pg_catalog.text, ''), COALESCE(od.deptype = 'i', false),
	c.relacl IS NULL,
	` + aclColumns("p") + `,
	COALESCE(ds.description, '')
FROM pg_catalog.pg_class c
JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
JOIN pg_catalog.pg_sequence s ON s.seqrelid = c.oid
` + originJoin(catalogClass, "c.oid") + `
LEFT JOIN pg_catalog.pg_depend od ON od.classid = 'pg_catalog.pg_class'::pg_catalog.regclass AND od.objid = c.oid
	AND od.objsubid = 0 AND od.refclassid = 'pg_catalog.pg_class'::pg_catalog.regclass AND od.deptype IN ('a', 'i')
LEFT JOIN pg_catalog.pg_class ot ON ot.oid = od.refobjid
LEFT JOIN pg_catalog.pg_namespace otn ON otn.oid = ot.relnamespace
LEFT JOIN pg_catalog.pg_attribute oa ON oa.attrelid = od.refobjid AND oa.attnum = od.refobjsubid
LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = c.oid
	AND ds.classoid = 'pg_catalog.pg_class'::pg_catalog.regclass AND ds.objsubid = 0
` + aclLateral("COALESCE(c.relacl, pg_catalog.acldefault('s', c.relowner))", "p") + `
WHERE c.relkind = 'S'`

// scanSequence builds a *Sequence of d from one sequenceSelect row.
func scanSequence(d *Database, scan func(...any) error) (*Sequence, error) {
	seq := &Sequence{db: d}
	var persistence string
	var acl privilegeArrays
	dest := []any{&seq.OID, &seq.Schema, &seq.Name, &seq.Owner}
	dest = append(dest, scanOrigin(&seq.Origin)...)
	dest = append(dest, &persistence, &seq.DataType, &seq.Start, &seq.Increment, &seq.MinValue, &seq.MaxValue, &seq.Cache, &seq.Cycle,
		&seq.OwnedBy.Schema, &seq.OwnedBy.Name, &seq.OwnedByColumn, &seq.Identity, &seq.PrivilegesDefault)
	dest = append(dest, acl.dest()...)
	dest = append(dest, &seq.Comment)
	if err := scan(dest...); err != nil {
		return nil, err
	}
	seq.Persistence = Persistence(persistence)
	var err error
	seq.Privileges, err = acl.all()
	return seq, err
}

// Sequences lists the database's sequences — identity columns' included —
// by schema then name, narrowed by f: Name on the sequence's name, Schema on
// its schema's, System for those in a system schema (every session's
// temporary sequences), ExtensionMember for those an extension owns. Every
// sequence is listed whatever the role's privileges on it. They are read
// through the database's own pool.
func (d *Database) Sequences(ctx context.Context, f ObjectFilter) ([]*Sequence, error) {
	where, args, err := f.clause(relationFilterColumns, nil)
	if err != nil {
		return nil, err
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return scanRows(ctx, q, fmt.Sprintf("list sequences of database %q", d.Name), func(scan func(...any) error) (*Sequence, error) {
		return scanSequence(d, scan)
	}, sequenceSelect+where+`
ORDER BY n.nspname, c.relname`, args...)
}

// SequenceByName reads the sequence schema.name — stored names, matched
// exactly (fold typed input with pgsql.FoldIdent first). schema is required
// (ErrSchemaRequired). A name the database holds no sequence by — none, or
// a table, view or index — is ErrNotFound. Use SequenceRef instead when only
// a handle to write through is needed.
func (d *Database) SequenceByName(ctx context.Context, schema, name string) (*Sequence, error) {
	qualified := pgsql.QualifiedName(schema, name)
	if schema == "" {
		return nil, fmt.Errorf("pgmo: read sequence %q: %w", name, ErrSchemaRequired)
	}
	q, err := d.server.onDatabase(ctx, d.Name)
	if err != nil {
		return nil, err
	}
	return readByName(ctx, q, d, scanSequence, sequenceSelect+`
	AND n.nspname = $1::pg_catalog.text AND c.relname = $2::pg_catalog.text`, []any{schema, name},
		notFoundf("pgmo: sequence %s not found in database %q", qualified, d.Name),
		fmt.Sprintf("read sequence %s of database %q", qualified, d.Name))
}

// SequenceRef returns a handle for the sequence schema.name without reading
// the catalog: Schema and Name are set and every other field is zero, OID
// included — so LastValue refuses it (ErrHandleNotLoaded). It is not
// interchangeable with SequenceByName.
func (d *Database) SequenceRef(schema, name string) *Sequence {
	return &Sequence{db: d, Schema: schema, Name: name}
}

// sequenceLastValueQuery reads, for the sequence $1, whether the role may
// read its value, whether it is another session's temporary one, whether it
// is unlogged on a standby (whose unlogged relations hold nothing), and then
// — only when none stands in the way, since pg15 raises on the first —
// pg_sequence_last_value, NULL when the sequence has not been called.
const sequenceLastValueQuery = `SELECT r.readable, r.other_temp, r.unlogged_standby,
	CASE WHEN r.readable AND NOT r.other_temp AND NOT r.unlogged_standby
		THEN pg_catalog.pg_sequence_last_value(r.oid) END
FROM (SELECT c.oid,
		pg_catalog.has_sequence_privilege(c.oid, 'SELECT,USAGE') AS readable,
		pg_catalog.pg_is_other_temp_schema(c.relnamespace) AS other_temp,
		c.relpersistence = 'u' AND pg_catalog.pg_is_in_recovery() AS unlogged_standby
	FROM pg_catalog.pg_class c WHERE c.oid = $1 AND c.relkind = 'S') AS r`

// LastValue reads the last value the sequence has given out — to any
// session, as of the call — and called true; or called false when it has
// given none since it was created, RESTARTed or set by setval(…, false).
// It is the value stored in the sequence, which a session's CACHE runs
// ahead of: with CACHE 20, the first nextval stores the 20th value.
//
// It needs SELECT or USAGE on the sequence; without either it is refused
// with ErrDefinitionHidden (and ErrUnsupported). Another session's
// temporary sequence, and an unlogged one on a standby, have no value to
// read: ErrUnsupported. A SequenceRef is refused (ErrHandleNotLoaded); a
// sequence dropped since it was read is ErrNotFound.
func (seq *Sequence) LastValue(ctx context.Context) (value int64, called bool, err error) {
	what := "read last value of sequence " + seq.QualifiedName()
	if seq.OID == 0 {
		return 0, false, fmt.Errorf("pgmo: %s: %w", what, ErrHandleNotLoaded)
	}
	db, err := seq.db.server.onDatabase(ctx, seq.db.Name)
	if err != nil {
		return 0, false, err
	}
	var readable, otherTemp, unloggedStandby bool
	var last *int64
	err = queryRowScan(ctx, db, sequenceLastValueQuery, []any{seq.OID}, &readable, &otherTemp, &unloggedStandby, &last)
	if err != nil {
		return 0, false, rowErr(err, notFoundf("pgmo: sequence %s not found in database %q", seq.QualifiedName(), seq.db.Name), what)
	}
	switch {
	case !readable:
		return 0, false, unsupportedf("pgmo: %s: the role holds neither SELECT nor USAGE on it: %w", what, ErrDefinitionHidden)
	case otherTemp:
		return 0, false, unsupportedf("pgmo: %s: another session's temporary sequence", what)
	case unloggedStandby:
		return 0, false, unsupportedf("pgmo: %s: an unlogged sequence holds no value on a standby", what)
	case last == nil:
		return 0, false, nil
	}
	return *last, true, nil
}
