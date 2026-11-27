package pgmo_test

import (
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/radix29/pgmo"
	"github.com/radix29/pgmo/pgmotest"
)

// The column types of the type read, in SELECT order, as the server sends
// them.
var typeTypes = append([]uint32{pgtype.OIDOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.BoolOID, pgtype.TextOID,
	pgtype.TextOID, pgtype.TextOID, pgtype.BoolOID, pgtype.BoolOID,
	pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.TextArrayOID,
	pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.TextArrayOID,
	pgtype.Int4OID, pgtype.BoolOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID,
	pgtype.TextOID, pgtype.TextOID, pgtype.BoolOID, pgtype.BoolOID,
	pgtype.TextArrayOID,
	pgtype.TextArrayOID, pgtype.OIDArrayOID, pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.TextArrayOID,
	pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID,
	pgtype.TextArrayOID, pgtype.TextArrayOID,
	pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID,
	pgtype.BoolOID},
	append(aclTypes, pgtype.TextOID)...)

// A type with every column set to something its neighbours are not — no
// real type is every kind at once, but each column is read alone.
var rowType = []any{uint32(42001), "app", "kind", "own", true, "citext",
	"r", "U", true, true,
	[]string{"s", "in"}, []string{"s", "out"}, []string{"s", "recv"}, []string{"s", "send"},
	[]string{"s", "tmin"}, []string{"s", "tmout"}, []string{"s", "an"}, []string{"s", "sub"},
	int32(-2), true, "d", "m", ";",
	"app.elem", "dflt", true, true,
	[]string{"sad", "ok"},
	[]string{"a", "B b"}, []uint32{23, 42002}, []string{"integer", "app.mood[]"}, []string{"", "pg_catalog"}, []string{"", "C"},
	"double precision", "o", "float8_ops", "c", "coll",
	[]string{"s", "canon"}, []string{"s", "diff"},
	"m", "multi", "r", "rng",
	false,
	[]string{""}, []string{"own"}, []string{"USAGE"}, []bool{true},
	"the kind"}

// typeMatch tells the type read apart in a fake.
const typeMatch = "WHERE t.typtype <> 'd'"

// Every field is filled from its own column, the names are bound as $1 and
// $2, and the read goes to the type's own database.
func TestTypeByNameFillsEveryField(t *testing.T) {
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On(typeMatch, rowsOf(typeTypes, rowType))
	got, err := f.Server().DatabaseRef("appdb").TypeByName(t.Context(), "app", "kind")
	if err != nil {
		t.Fatal(err)
	}
	if got.Database() == nil || got.Database().Name != "appdb" {
		t.Errorf("Database() = %v", got.Database())
	}
	s := func(n string) pgmo.ObjectName { return pgmo.ObjectName{Schema: "s", Name: n} }
	want := pgmo.Type{
		Origin: pgmo.Origin{IsSystem: true, Extension: "citext"}, OID: 42001, Schema: "app", Name: "kind", Owner: "own",
		Kind: pgmo.TypeRange, Category: pgmo.TypeCategoryUser, Preferred: true, Shell: true,
		Input: s("in"), Output: s("out"), Receive: s("recv"), Send: s("send"),
		TypmodInput: s("tmin"), TypmodOutput: s("tmout"), Analyze: s("an"), Subscript: s("sub"),
		InternalLength: -2, PassedByValue: true, Alignment: pgmo.AlignDouble, Storage: pgmo.StorageMain, Delimiter: ";",
		Element: "app.elem", Default: "dflt", HasDefault: true, Collatable: true,
		Labels: []string{"sad", "ok"},
		Attributes: []pgmo.TypeAttribute{{Name: "a", TypeOID: 23, Type: "integer"},
			{Name: "B b", TypeOID: 42002, Type: "app.mood[]", Collation: pgmo.ObjectName{Schema: "pg_catalog", Name: "C"}}},
		Subtype: "double precision", SubtypeOperatorClass: pgmo.ObjectName{Schema: "o", Name: "float8_ops"},
		Collation: pgmo.ObjectName{Schema: "c", Name: "coll"}, Canonical: s("canon"), SubtypeDiff: s("diff"),
		Multirange: pgmo.ObjectName{Schema: "m", Name: "multi"}, Range: pgmo.ObjectName{Schema: "r", Name: "rng"},
		Privileges: []pgmo.PrivilegeGrant{{Grantee: "", Grantor: "own", Privilege: pgmo.PrivilegeUsage, Grantable: true}},
		Comment:    "the kind",
	}
	c := *got
	c.SetDatabase(nil)
	if !reflect.DeepEqual(c, want) {
		t.Errorf("TypeByName =\n%+v\nwant\n%+v", c, want)
	}
	if n := reflect.TypeFor[pgmo.Type]().NumField(); n != 39 {
		t.Errorf("Type has %d fields; this test checks 39 — add the new one", n)
	}
	if n := len(f.Queries()); n != 1 {
		t.Errorf("%d statements on the Server's own database, want 1: the type read went to appdb", n)
	}
	last := lastQuery(other)
	if last.Arg(0) != "app" || last.Arg(1) != "kind" || strings.Contains(last.Text, "'kind'") {
		t.Errorf("names not bound as $1, $2: args %v, text\n%s", last.Args, last.Text)
	}
	for _, w := range []string{
		typeMatch + " AND (t.typrelid = 0 OR r.relkind = 'c') AND (te.oid IS NULL OR te.typarray <> t.oid)",
		"LEFT JOIN pg_catalog.pg_class r ON r.oid = t.typrelid",
		"LEFT JOIN pg_catalog.pg_type te ON te.oid = t.typelem",
		"t.typtype::pg_catalog.text, t.typcategory::pg_catalog.text, t.typispreferred, NOT t.typisdefined,",
		"t.typdelim::pg_catalog.text,\n\tCOALESCE(CASE\n\t\t\tWHEN ten.nspname = 'pg_catalog' THEN pg_catalog.format_type(te.oid, NULL)",
		"pg_catalog.array_agg(a.atttypid ORDER BY a.attnum),",
		"t.typlen::integer, t.typbyval, t.typalign::pg_catalog.text, t.typstorage::pg_catalog.text, t.typdelim::pg_catalog.text,",
		"COALESCE(t.typdefault, ''), t.typdefault IS NOT NULL, t.typcollation <> 0,",
		"LEFT JOIN pg_catalog.pg_proc fin ON fin.oid = t.typinput::pg_catalog.oid",
		"LEFT JOIN pg_catalog.pg_proc fout ON fout.oid = t.typoutput::pg_catalog.oid",
		"LEFT JOIN pg_catalog.pg_proc frecv ON frecv.oid = t.typreceive::pg_catalog.oid",
		"LEFT JOIN pg_catalog.pg_proc fsend ON fsend.oid = t.typsend::pg_catalog.oid",
		"LEFT JOIN pg_catalog.pg_proc fmin ON fmin.oid = t.typmodin::pg_catalog.oid",
		"LEFT JOIN pg_catalog.pg_proc fmout ON fmout.oid = t.typmodout::pg_catalog.oid",
		"LEFT JOIN pg_catalog.pg_proc fan ON fan.oid = t.typanalyze::pg_catalog.oid",
		"LEFT JOIN pg_catalog.pg_proc fsub ON fsub.oid = t.typsubscript::pg_catalog.oid",
		"LEFT JOIN pg_catalog.pg_range rg ON rg.rngtypid = t.oid",
		"LEFT JOIN pg_catalog.pg_type rs ON rs.oid = rg.rngsubtype",
		"LEFT JOIN pg_catalog.pg_opclass ro ON ro.oid = rg.rngsubopc\nLEFT JOIN pg_catalog.pg_namespace ron ON ron.oid = ro.opcnamespace",
		"LEFT JOIN pg_catalog.pg_collation rc ON rc.oid = rg.rngcollation\nLEFT JOIN pg_catalog.pg_namespace rcn ON rcn.oid = rc.collnamespace",
		"LEFT JOIN pg_catalog.pg_proc rcan ON rcan.oid = rg.rngcanonical::pg_catalog.oid",
		"LEFT JOIN pg_catalog.pg_proc rdiff ON rdiff.oid = rg.rngsubdiff::pg_catalog.oid",
		"LEFT JOIN pg_catalog.pg_type rm ON rm.oid = rg.rngmultitypid\nLEFT JOIN pg_catalog.pg_namespace rmn ON rmn.oid = rm.typnamespace",
		"LEFT JOIN pg_catalog.pg_range mg ON mg.rngmultitypid = t.oid",
		"LEFT JOIN pg_catalog.pg_type mr ON mr.oid = mg.rngtypid\nLEFT JOIN pg_catalog.pg_namespace mrn ON mrn.oid = mr.typnamespace",
		"COALESCE(ron.nspname::pg_catalog.text, ''), COALESCE(ro.opcname::pg_catalog.text, ''),\n\tCOALESCE(rcn.nspname::pg_catalog.text, ''), COALESCE(rc.collname::pg_catalog.text, ''),",
		"COALESCE(rmn.nspname::pg_catalog.text, ''), COALESCE(rm.typname::pg_catalog.text, ''),\n\tCOALESCE(mrn.nspname::pg_catalog.text, ''), COALESCE(mr.typname::pg_catalog.text, ''),",
		"pg_catalog.array_agg(e.enumlabel::pg_catalog.text ORDER BY e.enumsortorder)\n\tFROM pg_catalog.pg_enum e\n\tGROUP BY e.enumtypid) AS el(type, labels) ON el.type = t.oid",
		"JOIN pg_catalog.pg_class ar ON ar.oid = a.attrelid AND ar.relkind = 'c'",
		"CROSS JOIN LATERAL (SELECT pg_catalog.format_type(a.atttypid, a.atttypmod) AS name) AS aft",
		"CASE WHEN a.attcollation <> aty.typcollation THEN COALESCE(acn.nspname::pg_catalog.text, '') ELSE '' END ORDER BY a.attnum",
		"CASE WHEN a.attcollation <> aty.typcollation THEN COALESCE(ac.collname::pg_catalog.text, '') ELSE '' END ORDER BY a.attnum",
		"WHERE a.attnum > 0 AND NOT a.attisdropped\n\tGROUP BY a.attrelid) AS ca(rel, names, types, typenames, collschemas, collnames) ON ca.rel = t.typrelid",
		"LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = t.oid\n\tAND ds.classoid = 'pg_catalog.pg_type'::pg_catalog.regclass AND ds.objsubid = 0",
		"COALESCE(t.typacl, pg_catalog.acldefault('T', t.typowner))", "t.typacl IS NULL",
		"d.classid = 'pg_catalog.pg_type'::pg_catalog.regclass AND d.objid = t.oid",
		"AND n.nspname = $1::pg_catalog.text AND t.typname = $2::pg_catalog.text",
	} {
		if !strings.Contains(last.Text, w) {
			t.Errorf("no %q in\n%s", w, last.Text)
		}
	}
	if got.QualifiedName() != "app.kind" {
		t.Errorf("QualifiedName = %s", got.QualifiedName())
	}
}

// The listing pushes its filter into the query and orders by schema then
// name; empty arrays and NULLs read as none.
func TestTypesFiltered(t *testing.T) {
	yes := true
	f := pgmotest.New(t)
	f.On(typeMatch, rowsOf(typeTypes, []any{uint32(42003), "app", "Mood", "own", false, "",
		"e", "E", false, false,
		[]string{"pg_catalog", "enum_in"}, []string{"pg_catalog", "enum_out"}, []string{}, []string{},
		[]string{}, []string{}, []string{}, []string{},
		int32(4), true, "i", "p", ",",
		"", "", false, false,
		nil,
		nil, nil, nil, nil, nil,
		"", "", "", "", "",
		[]string{}, []string{},
		"", "", "", "",
		true, nil, nil, nil, nil, ""}))
	d := f.Server().DatabaseRef(pgmotest.DefaultDatabase)
	ts, err := d.Types(t.Context(), pgmo.ObjectFilter{
		Name:            []pgmo.TextCriterion{{Op: pgmo.TextContains, Value: "MOOD"}},
		Schema:          []pgmo.TextCriterion{{Op: pgmo.TextEquals, Value: "app"}},
		System:          &yes,
		ExtensionMember: &yes,
	})
	if err != nil || len(ts) != 1 || ts[0].Labels != nil || ts[0].Attributes != nil || ts[0].Privileges != nil ||
		!ts[0].PrivilegesDefault || ts[0].Receive != (pgmo.ObjectName{}) || ts[0].Input != (pgmo.ObjectName{Schema: "pg_catalog", Name: "enum_in"}) ||
		ts[0].Kind != pgmo.TypeEnum {
		t.Fatalf("Types = %+v, %v", ts, err)
	}
	last := lastQuery(f)
	tail := " AND pg_catalog.strpos(t.typname, $1) > 0 AND n.nspname = $2::pg_catalog.text" +
		" AND (pg_catalog.starts_with(n.nspname, 'pg_') OR n.nspname = 'information_schema') AND EXISTS (SELECT FROM pg_catalog.pg_depend d WHERE d.classid = 'pg_catalog.pg_type'::pg_catalog.regclass AND d.objid = t.oid"
	if !strings.Contains(last.Text, "te.typarray <> t.oid)"+tail) || !strings.HasSuffix(last.Text, "\nORDER BY n.nspname, t.typname") ||
		len(last.Args) != 2 || last.Arg(0) != "mood" || last.Arg(1) != "app" {
		t.Errorf("filter not pushed down, then ordered: args %v\n%s", last.Args, last.Text)
	}
	n := len(f.Queries())
	if _, err := d.Types(t.Context(), pgmo.ObjectFilter{Name: []pgmo.TextCriterion{{Op: 99, Value: "x"}}}); !errors.Is(err, pgmo.ErrInvalidRequest) {
		t.Errorf("unknown operator: err = %v, want ErrInvalidRequest", err)
	}
	if len(f.Queries()) != n {
		t.Errorf("a refused filter was sent")
	}
}

// No schema is refused before anything is sent; no row is ErrNotFound; a
// server error, short attribute arrays and a malformed ACL are wrapped once,
// naming the read.
func TestTypeByNameErrors(t *testing.T) {
	f := pgmotest.New(t)
	f.On(typeMatch, rowsOf(typeTypes))
	d := f.Server().DatabaseRef(pgmotest.DefaultDatabase)
	n := len(f.Queries())
	if _, err := d.TypeByName(t.Context(), "", "t"); !errors.Is(err, pgmo.ErrSchemaRequired) ||
		!strings.HasPrefix(err.Error(), `pgmo: read type "t"`) {
		t.Errorf("no schema: err = %v, want ErrSchemaRequired", err)
	}
	if len(f.Queries()) != n {
		t.Errorf("a schema-less read was sent")
	}
	_, err := d.TypeByName(t.Context(), "app", "gone")
	if !errors.Is(err, pgmo.ErrNotFound) || !strings.Contains(err.Error(), `type app.gone not found in database "`+pgmotest.DefaultDatabase+`"`) {
		t.Errorf("missing: err = %v, want ErrNotFound naming it", err)
	}

	f = pgmotest.New(t)
	f.On(typeMatch, pgmotest.Fail(&pgconn.PgError{Code: "57014", Message: "canceled"}))
	_, err = f.Server().DatabaseRef(pgmotest.DefaultDatabase).TypeByName(t.Context(), "app", "t")
	if err == nil || errors.Is(err, pgmo.ErrNotFound) || !strings.HasPrefix(err.Error(), "pgmo: read type app.t of database ") ||
		strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("server error: err = %v, want it wrapped once naming the read", err)
	}

	for _, c := range []struct {
		name string
		col  int // the array cut short
		want string
	}{
		{"attribute type names", 30, "attribute arrays of different lengths"},
		{"attribute collation names", 32, "attribute arrays of different lengths"},
		{"ACL", len(rowType) - 2, "different lengths"},
	} {
		row := append([]any(nil), rowType...)
		switch a := row[c.col].(type) {
		case []string:
			row[c.col] = a[:1]
		case []bool:
			row[c.col] = a[:0]
		}
		f = pgmotest.New(t)
		f.On(typeMatch, rowsOf(typeTypes, row))
		_, err = f.Server().DatabaseRef(pgmotest.DefaultDatabase).Types(t.Context(), pgmo.ObjectFilter{})
		if err == nil || !strings.HasPrefix(err.Error(), "pgmo: list types of database ") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s short: err = %v, want the mismatch wrapped naming the read", c.name, err)
		}
	}
}

// A Ref reads nothing and holds the names alone.
func TestTypeRef(t *testing.T) {
	f := pgmotest.New(t)
	d := f.Server().DatabaseRef("appdb")
	n := len(f.Queries())
	ty := d.TypeRef("app", "mood")
	got := *ty
	got.SetDatabase(nil)
	if ty.Database() != d || !reflect.DeepEqual(got, pgmo.Type{Schema: "app", Name: "mood"}) {
		t.Errorf("TypeRef = %+v", ty)
	}
	if len(f.Queries()) != n || f.Database("appdb").Opens() != 0 {
		t.Errorf("a Ref sent a statement")
	}
}

// Each code's name, and an unknown one's.
func TestTypeEnumStrings(t *testing.T) {
	for _, c := range []struct{ got, want string }{
		{pgmo.TypeBase.String(), "base"}, {pgmo.TypeComposite.String(), "composite"}, {pgmo.TypeEnum.String(), "enum"},
		{pgmo.TypePseudo.String(), "pseudo"}, {pgmo.TypeRange.String(), "range"}, {pgmo.TypeMultirange.String(), "multirange"},
		{pgmo.TypeKind("d").String(), `TypeKind("d")`},
		{pgmo.AlignChar.String(), "char"}, {pgmo.AlignInt2.String(), "int2"}, {pgmo.AlignInt4.String(), "int4"},
		{pgmo.AlignDouble.String(), "double"}, {pgmo.TypeAlignment("q").String(), `TypeAlignment("q")`},
		{pgmo.TypeCategoryArray.String(), "Array"}, {pgmo.TypeCategoryBoolean.String(), "Boolean"},
		{pgmo.TypeCategoryComposite.String(), "Composite"}, {pgmo.TypeCategoryDateTime.String(), "Date/time"},
		{pgmo.TypeCategoryEnum.String(), "Enum"}, {pgmo.TypeCategoryGeometric.String(), "Geometric"},
		{pgmo.TypeCategoryNetwork.String(), "Network address"}, {pgmo.TypeCategoryNumeric.String(), "Numeric"},
		{pgmo.TypeCategoryPseudo.String(), "Pseudo-types"}, {pgmo.TypeCategoryRange.String(), "Range"},
		{pgmo.TypeCategoryString.String(), "String"}, {pgmo.TypeCategoryTimespan.String(), "Timespan"},
		{pgmo.TypeCategoryUser.String(), "User-defined"}, {pgmo.TypeCategoryBitString.String(), "Bit-string"},
		{pgmo.TypeCategoryUnknown.String(), "unknown"}, {pgmo.TypeCategoryInternalUse.String(), "Internal-use"},
		{pgmo.TypeCategory("q").String(), `TypeCategory("q")`},
	} {
		if c.got != c.want {
			t.Errorf("String() = %q, want %q", c.got, c.want)
		}
	}
}
