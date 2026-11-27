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

// The column types of the domain read, in SELECT order, as the server
// sends them.
var domainTypes = append([]uint32{pgtype.OIDOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.BoolOID, pgtype.TextOID,
	pgtype.OIDOID, pgtype.TextOID, pgtype.BoolOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID,
	pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.BoolArrayOID,
	pgtype.BoolOID},
	append(aclTypes, pgtype.TextOID)...)

// A domain with every column set to something its neighbours are not: two
// constraints as pg18 answers for a NOT NULL beside a CHECK added NOT VALID.
var rowDomain = []any{uint32(43001), "app", "code", "own", true, "citext",
	uint32(1043), "character varying(8)", true, "'x'::character varying", "app", "c",
	[]string{"code_not_null", "pos"}, []string{"n", "c"}, []string{"NOT NULL", "CHECK ((VALUE > 0)) NOT VALID"}, []bool{true, false},
	false,
	[]string{""}, []string{"own"}, []string{"USAGE"}, []bool{true},
	"the code"}

// domainMatch tells the domain read apart in a fake.
const domainMatch = "WHERE t.typtype = 'd'"

// Every field is filled from its own column, the names are bound as $1 and
// $2, and the read goes to the domain's own database.
func TestDomainByNameFillsEveryField(t *testing.T) {
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On(domainMatch, rowsOf(domainTypes, rowDomain))
	got, err := f.Server().DatabaseRef("appdb").DomainByName(t.Context(), "app", "code")
	if err != nil {
		t.Fatal(err)
	}
	if got.Database() == nil || got.Database().Name != "appdb" {
		t.Errorf("Database() = %v", got.Database())
	}
	want := pgmo.Domain{
		Origin: pgmo.Origin{IsSystem: true, Extension: "citext"}, OID: 43001, Schema: "app", Name: "code", Owner: "own",
		BaseTypeOID: 1043, BaseType: "character varying(8)", NotNull: true, Default: "'x'::character varying",
		Collation: pgmo.ObjectName{Schema: "app", Name: "c"},
		Constraints: []pgmo.DomainConstraint{
			{Name: "code_not_null", Kind: pgmo.ConstraintNotNull, Definition: "NOT NULL", Validated: true},
			{Name: "pos", Kind: pgmo.ConstraintCheck, Definition: "CHECK ((VALUE > 0)) NOT VALID"},
		},
		Privileges: []pgmo.PrivilegeGrant{{Grantee: "", Grantor: "own", Privilege: pgmo.PrivilegeUsage, Grantable: true}},
		Comment:    "the code",
	}
	c := *got
	c.SetDatabase(nil)
	if !reflect.DeepEqual(c, want) {
		t.Errorf("DomainByName =\n%+v\nwant\n%+v", c, want)
	}
	if n := reflect.TypeFor[pgmo.Domain]().NumField(); n != 15 {
		t.Errorf("Domain has %d fields; this test checks 15 — add the new one", n)
	}
	if n := len(f.Queries()); n != 1 {
		t.Errorf("%d statements on the Server's own database, want 1: the domain read went to appdb", n)
	}
	last := lastQuery(other)
	if last.Arg(0) != "app" || last.Arg(1) != "code" || strings.Contains(last.Text, "'code'") {
		t.Errorf("names not bound as $1, $2: args %v, text\n%s", last.Args, last.Text)
	}
	for _, w := range []string{domainMatch,
		"t.typbasetype, CASE WHEN btn.nspname = 'pg_catalog'\n\t\t\tOR pg_catalog.starts_with(ft.name, pg_catalog.quote_ident(btn.nspname) || '.') THEN ft.name\n\t\tELSE pg_catalog.quote_ident(btn.nspname) || '.' || ft.name END,",
		"ft.name END,\n\tt.typnotnull, COALESCE(pg_catalog.pg_get_expr(t.typdefaultbin, 0), t.typdefault, ''),",
		"CASE WHEN t.typcollation <> bt.typcollation THEN COALESCE(con.nspname::pg_catalog.text, '') ELSE '' END,",
		"CASE WHEN t.typcollation <> bt.typcollation THEN COALESCE(co.collname::pg_catalog.text, '') ELSE '' END,",
		"JOIN pg_catalog.pg_type bt ON bt.oid = t.typbasetype\nJOIN pg_catalog.pg_namespace btn ON btn.oid = bt.typnamespace",
		"CROSS JOIN LATERAL (SELECT pg_catalog.format_type(t.typbasetype, t.typtypmod) AS name) AS ft",
		"LEFT JOIN pg_catalog.pg_collation co ON co.oid = t.typcollation\nLEFT JOIN pg_catalog.pg_namespace con ON con.oid = co.collnamespace",
		"pg_catalog.array_agg(c.conname::pg_catalog.text ORDER BY c.conname),\n\t\tpg_catalog.array_agg(c.contype::pg_catalog.text ORDER BY c.conname),\n\t\tpg_catalog.array_agg(pg_catalog.pg_get_constraintdef(c.oid) ORDER BY c.conname),\n\t\tpg_catalog.array_agg(c.convalidated ORDER BY c.conname)",
		"WHERE c.contypid <> 0\n\tGROUP BY c.contypid) AS dc(type, names, kinds, defs, validated) ON dc.type = t.oid",
		"LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = t.oid\n\tAND ds.classoid = 'pg_catalog.pg_type'::pg_catalog.regclass AND ds.objsubid = 0",
		"COALESCE(t.typacl, pg_catalog.acldefault('T', t.typowner))", "t.typacl IS NULL",
		"d.classid = 'pg_catalog.pg_type'::pg_catalog.regclass AND d.objid = t.oid",
		"AND n.nspname = $1::pg_catalog.text AND t.typname = $2::pg_catalog.text",
	} {
		if !strings.Contains(last.Text, w) {
			t.Errorf("no %q in\n%s", w, last.Text)
		}
	}
	if got.QualifiedName() != "app.code" {
		t.Errorf("QualifiedName = %s", got.QualifiedName())
	}
}

// The listing pushes its filter into the query and orders by schema then
// name; NULL arrays read as none.
func TestDomainsFiltered(t *testing.T) {
	yes := true
	f := pgmotest.New(t)
	f.On(domainMatch, rowsOf(domainTypes, []any{uint32(43002), "app", "Code", "own", false, "",
		uint32(23), "integer", false, "", "", "", nil, nil, nil, nil, true, nil, nil, nil, nil, ""}))
	d := f.Server().DatabaseRef(pgmotest.DefaultDatabase)
	ds, err := d.Domains(t.Context(), pgmo.ObjectFilter{
		Name:            []pgmo.TextCriterion{{Op: pgmo.TextContains, Value: "CODE"}},
		Schema:          []pgmo.TextCriterion{{Op: pgmo.TextEquals, Value: "app"}},
		System:          &yes,
		ExtensionMember: &yes,
	})
	if err != nil || len(ds) != 1 || ds[0].Constraints != nil || ds[0].Privileges != nil || !ds[0].PrivilegesDefault ||
		ds[0].Collation != (pgmo.ObjectName{}) || ds[0].BaseType != "integer" {
		t.Fatalf("Domains = %+v, %v", ds, err)
	}
	last := lastQuery(f)
	tail := domainMatch + " AND pg_catalog.strpos(t.typname, $1) > 0 AND n.nspname = $2::pg_catalog.text" +
		" AND (pg_catalog.starts_with(n.nspname, 'pg_') OR n.nspname = 'information_schema') AND EXISTS (SELECT FROM pg_catalog.pg_depend d WHERE d.classid = 'pg_catalog.pg_type'::pg_catalog.regclass AND d.objid = t.oid"
	if !strings.Contains(last.Text, tail) || !strings.HasSuffix(last.Text, "\nORDER BY n.nspname, t.typname") ||
		len(last.Args) != 2 || last.Arg(0) != "code" || last.Arg(1) != "app" {
		t.Errorf("filter not pushed down, then ordered: args %v\n%s", last.Args, last.Text)
	}
	n := len(f.Queries())
	if _, err := d.Domains(t.Context(), pgmo.ObjectFilter{Name: []pgmo.TextCriterion{{Op: 99, Value: "x"}}}); !errors.Is(err, pgmo.ErrInvalidRequest) {
		t.Errorf("unknown operator: err = %v, want ErrInvalidRequest", err)
	}
	if len(f.Queries()) != n {
		t.Errorf("a refused filter was sent")
	}
}

// No schema is refused before anything is sent; no row is ErrNotFound; a
// server error, short constraint arrays and a malformed ACL are wrapped
// once, naming the read.
func TestDomainByNameErrors(t *testing.T) {
	f := pgmotest.New(t)
	f.On(domainMatch, rowsOf(domainTypes))
	d := f.Server().DatabaseRef(pgmotest.DefaultDatabase)
	n := len(f.Queries())
	if _, err := d.DomainByName(t.Context(), "", "c"); !errors.Is(err, pgmo.ErrSchemaRequired) ||
		!strings.HasPrefix(err.Error(), `pgmo: read domain "c"`) {
		t.Errorf("no schema: err = %v, want ErrSchemaRequired", err)
	}
	if len(f.Queries()) != n {
		t.Errorf("a schema-less read was sent")
	}
	_, err := d.DomainByName(t.Context(), "app", "gone")
	if !errors.Is(err, pgmo.ErrNotFound) || !strings.Contains(err.Error(), `domain app.gone not found in database "`+pgmotest.DefaultDatabase+`"`) {
		t.Errorf("missing: err = %v, want ErrNotFound naming it", err)
	}

	f = pgmotest.New(t)
	f.On(domainMatch, pgmotest.Fail(&pgconn.PgError{Code: "57014", Message: "canceled"}))
	_, err = f.Server().DatabaseRef(pgmotest.DefaultDatabase).DomainByName(t.Context(), "app", "c")
	if err == nil || errors.Is(err, pgmo.ErrNotFound) || !strings.HasPrefix(err.Error(), "pgmo: read domain app.c of database ") ||
		strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("server error: err = %v, want it wrapped once naming the read", err)
	}

	for _, c := range []struct {
		name string
		col  int // the array cut short
		want string
	}{
		{"constraint kinds", 13, "constraint arrays of different lengths"},
		{"constraint definitions", 14, "constraint arrays of different lengths"},
		{"constraint validated", 15, "constraint arrays of different lengths"},
		{"ACL", len(rowDomain) - 2, "different lengths"},
	} {
		row := append([]any(nil), rowDomain...)
		switch a := row[c.col].(type) {
		case []string:
			row[c.col] = a[:1]
		case []bool:
			row[c.col] = a[:0]
		}
		f = pgmotest.New(t)
		f.On(domainMatch, rowsOf(domainTypes, row))
		_, err = f.Server().DatabaseRef(pgmotest.DefaultDatabase).Domains(t.Context(), pgmo.ObjectFilter{})
		if err == nil || !strings.HasPrefix(err.Error(), "pgmo: list domains of database ") || !strings.Contains(err.Error(), c.want) {
			t.Errorf("%s short: err = %v, want the mismatch wrapped naming the read", c.name, err)
		}
	}
}

// A Ref reads nothing and holds the names alone.
func TestDomainRef(t *testing.T) {
	f := pgmotest.New(t)
	d := f.Server().DatabaseRef("appdb")
	n := len(f.Queries())
	dm := d.DomainRef("app", "code")
	got := *dm
	got.SetDatabase(nil)
	if dm.Database() != d || !reflect.DeepEqual(got, pgmo.Domain{Schema: "app", Name: "code"}) {
		t.Errorf("DomainRef = %+v", dm)
	}
	if len(f.Queries()) != n || f.Database("appdb").Opens() != 0 {
		t.Errorf("a Ref sent a statement")
	}
}
