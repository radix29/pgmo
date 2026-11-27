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

// The column types of the tablespace read, in SELECT order, as the server
// sends them.
var tablespaceTypes = append([]uint32{pgtype.OIDOID, pgtype.TextOID, pgtype.TextOID, pgtype.BoolOID, pgtype.TextOID,
	pgtype.TextOID, pgtype.TextArrayOID, pgtype.BoolOID}, append(aclTypes, pgtype.TextOID)...)

// tablespaceMatch tells the tablespace read apart in a fake.
const tablespaceMatch = "FROM pg_catalog.pg_tablespace t\n"

// Two tablespaces with every column set to something its neighbours are not.
var (
	rowTablespace1 = []any{uint32(1663), "pg_default", "postgres", true, "x",
		"", []string{}, true,
		[]string{"postgres"}, []string{"postgres"}, []string{"CREATE"}, []bool{false}, ""}
	rowTablespace2 = []any{uint32(70001), "Fast ts", "own", false, "",
		"/srv/fast", []string{"random_page_cost=1.1", "seq_page_cost=1"}, false,
		[]string{"", "own"}, []string{"own", "own"}, []string{"CREATE", "CREATE"}, []bool{false, true}, "the fast one"}
)

// The rows above as Tablespaces, without their Server.
var wantTablespaces = []pgmo.Tablespace{
	{Origin: pgmo.Origin{IsSystem: true, Extension: "x"}, OID: 1663, Name: "pg_default", Owner: "postgres",
		Privileges:        []pgmo.PrivilegeGrant{{Grantee: "postgres", Grantor: "postgres", Privilege: pgmo.PrivilegeCreate}},
		PrivilegesDefault: true},
	{OID: 70001, Name: "Fast ts", Owner: "own", Location: "/srv/fast",
		Options: []pgmo.ParameterSetting{{Name: "random_page_cost", Value: "1.1"}, {Name: "seq_page_cost", Value: "1"}},
		Privileges: []pgmo.PrivilegeGrant{{Grantee: "", Grantor: "own", Privilege: pgmo.PrivilegeCreate},
			{Grantee: "own", Grantor: "own", Privilege: pgmo.PrivilegeCreate, Grantable: true}},
		Comment: "the fast one"},
}

// checkTablespaces compares got with the first len(got) of wantTablespaces,
// each read from s.
func checkTablespaces(t *testing.T, got []*pgmo.Tablespace, s *pgmo.Server) {
	t.Helper()
	for i, ts := range got {
		if ts.Server() != s {
			t.Errorf("[%d].Server() = %v, want %v", i, ts.Server(), s)
		}
		g := *ts
		g.SetServer(nil)
		if !reflect.DeepEqual(g, wantTablespaces[i]) {
			t.Errorf("[%d] =\n%+v\nwant\n%+v", i, g, wantTablespaces[i])
		}
	}
	if n := reflect.TypeFor[pgmo.Tablespace]().NumField(); n != 10 {
		t.Errorf("Tablespace has %d fields; this test checks 10 — add the new one", n)
	}
}

// The fragments every tablespace read holds.
var tablespaceFragments = []string{
	"SELECT t.oid, t.spcname::pg_catalog.text,\n\tpg_catalog.pg_get_userbyid(t.spcowner)::pg_catalog.text,\n\tt.oid < 16384, '',",
	"pg_catalog.pg_tablespace_location(t.oid), t.spcoptions,\n\tt.spcacl IS NULL,",
	tablespaceMatch,
	"LEFT JOIN pg_catalog.pg_shdescription ds ON ds.objoid = t.oid\n\tAND ds.classoid = 'pg_catalog.pg_tablespace'::pg_catalog.regclass",
	"COALESCE(t.spcacl, pg_catalog.acldefault('t', t.spcowner))",
}

// The listing fills every field, pushes its filter into the query — name,
// system by OID, membership as false; a schema criterion is no column —
// orders by name, and goes to the Server's own connection.
func TestTablespacesFiltered(t *testing.T) {
	yes := true
	f := pgmotest.New(t)
	f.On(tablespaceMatch, rowsOf(tablespaceTypes, rowTablespace1, rowTablespace2))
	s := f.Server()
	ts, err := s.Tablespaces(t.Context(), pgmo.ObjectFilter{
		Name:            []pgmo.TextCriterion{{Op: pgmo.TextContains, Value: `"Fast"`}},
		Schema:          []pgmo.TextCriterion{{Op: pgmo.TextEquals, Value: "app"}},
		System:          &yes,
		ExtensionMember: &yes,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ts) != 2 {
		t.Fatalf("%d tablespaces, want 2", len(ts))
	}
	checkTablespaces(t, ts, s)
	last := lastQuery(f)
	for _, w := range tablespaceFragments {
		if !strings.Contains(last.Text, w) {
			t.Errorf("no %q in\n%s", w, last.Text)
		}
	}
	if !strings.HasSuffix(last.Text, "WHERE true AND pg_catalog.strpos(t.spcname, $1) > 0 AND t.oid < 16384 AND false\nORDER BY t.spcname") ||
		len(last.Args) != 1 || last.Arg(0) != "Fast" {
		t.Errorf("filter not pushed down, then ordered: args %v\n%s", last.Args, last.Text)
	}
	n := len(f.Queries())
	if _, err := s.Tablespaces(t.Context(), pgmo.ObjectFilter{Name: []pgmo.TextCriterion{{Op: 99, Value: "x"}}}); !errors.Is(err, pgmo.ErrInvalidRequest) {
		t.Errorf("unknown operator: err = %v, want ErrInvalidRequest", err)
	}
	if len(f.Queries()) != n {
		t.Errorf("a refused filter was sent")
	}
}

// TablespaceByName binds the name as $1, fills every field, and tells a
// missing tablespace from a failed read; the Ref reads nothing; Size keys
// on the name.
func TestTablespaceByName(t *testing.T) {
	f := pgmotest.New(t)
	f.On(tablespaceMatch, rowsOf(tablespaceTypes, rowTablespace2))
	s := f.Server()
	ts, err := s.TablespaceByName(t.Context(), "Fast ts")
	if err != nil {
		t.Fatal(err)
	}
	g := *ts
	g.SetServer(nil)
	if ts.Server() != s || !reflect.DeepEqual(g, wantTablespaces[1]) {
		t.Errorf("= %+v, want %+v", g, wantTablespaces[1])
	}
	last := lastQuery(f)
	if !strings.HasSuffix(last.Text, "WHERE true\n\tAND t.spcname = $1::pg_catalog.text") || last.Arg(0) != "Fast ts" {
		t.Errorf("name not bound as $1: args %v\n%s", last.Args, last.Text)
	}
	if ts.QualifiedName() != `"Fast ts"` {
		t.Errorf("QualifiedName() = %s", ts.QualifiedName())
	}

	f = pgmotest.New(t)
	f.On(tablespaceMatch, rowsOf(tablespaceTypes))
	f.On("pg_catalog.pg_tablespace_size(t.oid)", rowsOf([]uint32{pgtype.Int8OID}))
	s = f.Server()
	if _, err := s.TablespaceByName(t.Context(), "Gone"); !errors.Is(err, pgmo.ErrNotFound) ||
		!strings.Contains(err.Error(), `tablespace "Gone" not found`) {
		t.Errorf("missing: err = %v, want ErrNotFound naming it", err)
	}
	if _, err := s.TablespaceRef("Gone").Size(t.Context()); !errors.Is(err, pgmo.ErrNotFound) {
		t.Errorf("size of a missing one: err = %v, want ErrNotFound", err)
	}

	f = pgmotest.New(t)
	f.On(tablespaceMatch, pgmotest.Fail(&pgconn.PgError{Code: "57014", Message: "canceled"}))
	f.On("pg_catalog.pg_tablespace_size(t.oid)", pgmotest.Fail(&pgconn.PgError{Code: "42501", Message: "denied"}))
	s = f.Server()
	_, err = s.TablespaceByName(t.Context(), "fast")
	if err == nil || errors.Is(err, pgmo.ErrNotFound) || !strings.HasPrefix(err.Error(), "pgmo: read tablespace fast: ") ||
		strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("server error: err = %v, want it wrapped once naming the read", err)
	}
	_, err = s.Tablespaces(t.Context(), pgmo.ObjectFilter{})
	if err == nil || !strings.HasPrefix(err.Error(), "pgmo: list tablespaces: ") || strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("listing, server error: err = %v, want it wrapped once naming the read", err)
	}
	_, err = s.TablespaceRef("fast").Size(t.Context())
	if !pgmo.IsPermissionDenied(err) || !strings.HasPrefix(err.Error(), "pgmo: read tablespace fast size: ") {
		t.Errorf("size, denied: err = %v, want 42501 wrapped naming the read", err)
	}
	last = lastQuery(f)
	if last.Arg(0) != "fast" || !strings.HasSuffix(last.Text, "WHERE t.spcname = $1::pg_catalog.text") {
		t.Errorf("size not keyed on the bound name: args %v\n%s", last.Args, last.Text)
	}

	n := len(f.Queries())
	r := s.TablespaceRef("fast")
	g = *r
	g.SetServer(nil)
	if r.Server() != s || !reflect.DeepEqual(g, pgmo.Tablespace{Name: "fast"}) {
		t.Errorf("TablespaceRef = %+v", r)
	}
	if len(f.Queries()) != n {
		t.Errorf("TablespaceRef sent a statement")
	}
}

// The size is the server's number.
func TestTablespaceSize(t *testing.T) {
	f := pgmotest.New(t)
	f.On("pg_catalog.pg_tablespace_size(t.oid)", rowsOf([]uint32{pgtype.Int8OID}, []any{int64(12345)}))
	n, err := f.Server().TablespaceRef("pg_default").Size(t.Context())
	if err != nil || n != 12345 {
		t.Errorf("Size = %d, %v; want 12345", n, err)
	}
}
