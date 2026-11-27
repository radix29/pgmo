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

// The column types of the wrapper, server and user mapping reads, in
// SELECT order, as the server sends them.
var (
	foreignDataWrapperTypes = append([]uint32{pgtype.OIDOID, pgtype.TextOID, pgtype.TextOID, pgtype.BoolOID, pgtype.TextOID,
		pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.TextArrayOID, pgtype.BoolOID}, append(aclTypes, pgtype.TextOID)...)
	foreignServerTypes = append([]uint32{pgtype.OIDOID, pgtype.TextOID, pgtype.TextOID, pgtype.BoolOID, pgtype.TextOID,
		pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextArrayOID, pgtype.BoolOID}, append(aclTypes, pgtype.TextOID)...)
	userMappingTypes = []uint32{pgtype.OIDOID, pgtype.OIDOID, pgtype.TextOID, pgtype.OIDOID, pgtype.TextOID,
		pgtype.TextArrayOID, pgtype.BoolOID}
)

// What tells each read apart in a fake.
const (
	foreignDataWrapperMatch = "FROM pg_catalog.pg_foreign_data_wrapper w"
	foreignServerMatch      = "FROM pg_catalog.pg_foreign_server s"
	userMappingMatch        = "FROM pg_catalog.pg_user_mappings um"
)

// Two rows of each with every column set to something its neighbour's is
// not.
var (
	rowWrapper1 = []any{uint32(16390), "postgres_fdw", "postgres", false, "postgres_fdw",
		[]string{"public", "postgres_fdw_handler"}, []string{"public", "postgres_fdw_validator"}, []string{}, true,
		[]string{"postgres"}, []string{"postgres"}, []string{"USAGE"}, []bool{false}, ""}
	rowWrapper2 = []any{uint32(70002), "My W", "own", true, "",
		[]string{}, []string{}, []string{"debug=a=b", "level=1"}, false,
		[]string{"own", ""}, []string{"own", "own"}, []string{"USAGE", "USAGE"}, []bool{true, false}, "wrapper"}

	rowServer1 = []any{uint32(70010), "remote", "own", false, "",
		"postgres_fdw", "pg", "18", []string{"host=db.example", "dbname=app"}, false,
		[]string{"own", "B b"}, []string{"own", "own"}, []string{"USAGE", "USAGE"}, []bool{false, true}, "srv"}
	rowServer2 = []any{uint32(70011), "Odd S", "postgres", true, "ext",
		"My W", "", "", []string{}, true,
		[]string{"postgres"}, []string{"postgres"}, []string{"USAGE"}, []bool{false}, ""}

	rowMapping1 = []any{uint32(70020), uint32(70010), "remote", uint32(0), "", []string{"user=pub", "password=x=y"}, false}
	rowMapping2 = []any{uint32(70021), uint32(70011), "Odd S", uint32(16500), "B b", []string{}, true}
)

// The rows above as values, without their Database.
var (
	wantWrappers = []pgmo.ForeignDataWrapper{
		{Origin: pgmo.Origin{Extension: "postgres_fdw"}, OID: 16390, Name: "postgres_fdw", Owner: "postgres",
			Handler:           pgmo.ObjectName{Schema: "public", Name: "postgres_fdw_handler"},
			Validator:         pgmo.ObjectName{Schema: "public", Name: "postgres_fdw_validator"},
			Privileges:        []pgmo.PrivilegeGrant{{Grantee: "postgres", Grantor: "postgres", Privilege: pgmo.PrivilegeUsage}},
			PrivilegesDefault: true},
		{Origin: pgmo.Origin{IsSystem: true}, OID: 70002, Name: "My W", Owner: "own",
			Options: []pgmo.ParameterSetting{{Name: "debug", Value: "a=b"}, {Name: "level", Value: "1"}},
			Privileges: []pgmo.PrivilegeGrant{{Grantee: "own", Grantor: "own", Privilege: pgmo.PrivilegeUsage, Grantable: true},
				{Grantee: "", Grantor: "own", Privilege: pgmo.PrivilegeUsage}},
			Comment: "wrapper"},
	}
	wantServers = []pgmo.ForeignServer{
		{OID: 70010, Name: "remote", Owner: "own", Wrapper: "postgres_fdw", Type: "pg", Version: "18",
			Options: []pgmo.ParameterSetting{{Name: "host", Value: "db.example"}, {Name: "dbname", Value: "app"}},
			Privileges: []pgmo.PrivilegeGrant{{Grantee: "own", Grantor: "own", Privilege: pgmo.PrivilegeUsage},
				{Grantee: "B b", Grantor: "own", Privilege: pgmo.PrivilegeUsage, Grantable: true}},
			Comment: "srv"},
		{Origin: pgmo.Origin{IsSystem: true, Extension: "ext"}, OID: 70011, Name: "Odd S", Owner: "postgres", Wrapper: "My W",
			Privileges:        []pgmo.PrivilegeGrant{{Grantee: "postgres", Grantor: "postgres", Privilege: pgmo.PrivilegeUsage}},
			PrivilegesDefault: true},
	}
	wantMappings = []pgmo.UserMapping{
		{OID: 70020, ServerOID: 70010, Server: "remote",
			Options: []pgmo.ParameterSetting{{Name: "user", Value: "pub"}, {Name: "password", Value: "x=y"}}},
		{OID: 70021, ServerOID: 70011, Server: "Odd S", UserOID: 16500, User: "B b", OptionsHidden: true},
	}
)

// checkForeign compares got with the first len(got) of want, each read from
// d, with the Database cleared through clear; fields is the number of
// fields T has, which the test checks.
func checkForeign[T any](t *testing.T, got []*T, want []T, d *pgmo.Database, db func(*T) *pgmo.Database, clear func(*T), fields int) {
	t.Helper()
	for i, x := range got {
		if db(x) != d {
			t.Errorf("[%d].Database() = %v, want %v", i, db(x), d)
		}
		g := *x
		clear(&g)
		if !reflect.DeepEqual(g, want[i]) {
			t.Errorf("[%d] =\n%+v\nwant\n%+v", i, g, want[i])
		}
	}
	if n := reflect.TypeFor[T]().NumField(); n != fields {
		t.Errorf("%T has %d fields; this test checks %d — add the new one", *new(T), n, fields)
	}
}

func checkWrappers(t *testing.T, got []*pgmo.ForeignDataWrapper, d *pgmo.Database) {
	t.Helper()
	checkForeign(t, got, wantWrappers, d, (*pgmo.ForeignDataWrapper).Database,
		func(w *pgmo.ForeignDataWrapper) { w.SetDatabase(nil) }, 11)
}

func checkServers(t *testing.T, got []*pgmo.ForeignServer, d *pgmo.Database) {
	t.Helper()
	checkForeign(t, got, wantServers, d, (*pgmo.ForeignServer).Database,
		func(s *pgmo.ForeignServer) { s.SetDatabase(nil) }, 12)
}

func checkMappings(t *testing.T, got []*pgmo.UserMapping, d *pgmo.Database) {
	t.Helper()
	checkForeign(t, got, wantMappings, d, (*pgmo.UserMapping).Database,
		func(m *pgmo.UserMapping) { m.SetDatabase(nil) }, 9)
}

// The fragments each read holds.
var (
	foreignDataWrapperFragments = []string{
		"SELECT w.oid, w.fdwname::pg_catalog.text,\n\tpg_catalog.pg_get_userbyid(w.fdwowner)::pg_catalog.text,\n\tw.oid < 16384, COALESCE(ext.extname::pg_catalog.text, ''),",
		"COALESCE(w.fdwoptions, '{}'),\n\tw.fdwacl IS NULL,",
		foreignDataWrapperMatch,
		"LEFT JOIN pg_catalog.pg_proc fh ON fh.oid = w.fdwhandler::pg_catalog.oid",
		"LEFT JOIN pg_catalog.pg_proc fv ON fv.oid = w.fdwvalidator::pg_catalog.oid",
		"d.classid = 'pg_catalog.pg_foreign_data_wrapper'::pg_catalog.regclass AND d.objid = w.oid",
		"LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = w.oid\n\tAND ds.classoid = 'pg_catalog.pg_foreign_data_wrapper'::pg_catalog.regclass AND ds.objsubid = 0",
		"COALESCE(w.fdwacl, pg_catalog.acldefault('F', w.fdwowner))",
	}
	foreignServerFragments = []string{
		"SELECT s.oid, s.srvname::pg_catalog.text,\n\tpg_catalog.pg_get_userbyid(s.srvowner)::pg_catalog.text,\n\ts.oid < 16384, COALESCE(ext.extname::pg_catalog.text, ''),",
		"sw.fdwname::pg_catalog.text, COALESCE(s.srvtype, ''), COALESCE(s.srvversion, ''),\n\tCOALESCE(s.srvoptions, '{}'),\n\ts.srvacl IS NULL,",
		foreignServerMatch,
		"JOIN pg_catalog.pg_foreign_data_wrapper sw ON sw.oid = s.srvfdw",
		"d.classid = 'pg_catalog.pg_foreign_server'::pg_catalog.regclass AND d.objid = s.oid",
		"LEFT JOIN pg_catalog.pg_description ds ON ds.objoid = s.oid\n\tAND ds.classoid = 'pg_catalog.pg_foreign_server'::pg_catalog.regclass AND ds.objsubid = 0",
		"COALESCE(s.srvacl, pg_catalog.acldefault('S', s.srvowner))",
	}
	userMappingFragments = []string{
		"SELECT um.umid, um.srvid, um.srvname::pg_catalog.text, um.umuser,\n\tCASE WHEN um.umuser = 0 THEN '' ELSE um.usename::pg_catalog.text END,\n\tCOALESCE(um.umoptions, '{}'),",
		"um.umoptions IS NULL AND NOT (",
		"(um.umuser <> 0 AND um.usename = CURRENT_USER\n\t\t\tAND (pg_catalog.pg_has_role(us.srvowner, 'USAGE') OR pg_catalog.has_server_privilege(us.oid, 'USAGE')))",
		"OR (um.umuser = 0 AND pg_catalog.pg_has_role(us.srvowner, 'USAGE'))",
		"OR COALESCE((SELECT r.rolsuper FROM pg_catalog.pg_roles r WHERE r.rolname = CURRENT_USER), false))",
		userMappingMatch,
		"JOIN pg_catalog.pg_foreign_server us ON us.oid = um.srvid",
	}
)

// checkFragments fails for each of want the query does not hold.
func checkFragments(t *testing.T, q pgmotest.Query, want []string) {
	t.Helper()
	for _, w := range want {
		if !strings.Contains(q.Text, w) {
			t.Errorf("no %q in\n%s", w, q.Text)
		}
	}
}

// The wrapper listing fills every field, pushes its filter into the query —
// name, system by OID, extension membership; a schema criterion is no
// column — orders by name, and goes to the database's own pool.
func TestForeignDataWrappersFiltered(t *testing.T) {
	no := false
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On(foreignDataWrapperMatch, rowsOf(foreignDataWrapperTypes, rowWrapper1, rowWrapper2))
	d := f.Server().DatabaseRef("appdb")
	ws, err := d.ForeignDataWrappers(t.Context(), pgmo.ObjectFilter{
		Name:            []pgmo.TextCriterion{{Op: pgmo.TextContains, Value: "FDW"}},
		Schema:          []pgmo.TextCriterion{{Op: pgmo.TextEquals, Value: "app"}},
		System:          &no,
		ExtensionMember: &no,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ws) != 2 {
		t.Fatalf("%d wrappers, want 2", len(ws))
	}
	checkWrappers(t, ws, d)
	if n := len(f.Queries()); n != 1 {
		t.Errorf("%d statements on the Server's own database, want 1: the listing went to appdb", n)
	}
	last := lastQuery(other)
	checkFragments(t, last, foreignDataWrapperFragments)
	tail := "WHERE true AND pg_catalog.strpos(w.fdwname, $1) > 0 AND NOT w.oid < 16384 AND NOT EXISTS (SELECT FROM pg_catalog.pg_depend d WHERE " +
		"d.classid = 'pg_catalog.pg_foreign_data_wrapper'::pg_catalog.regclass AND d.objid = w.oid"
	if !strings.Contains(last.Text, tail) || !strings.HasSuffix(last.Text, "\nORDER BY w.fdwname") ||
		len(last.Args) != 1 || last.Arg(0) != "fdw" || strings.Contains(last.Text, "nspname = $") {
		t.Errorf("filter not pushed down, then ordered: args %v\n%s", last.Args, last.Text)
	}
}

// The server listing, likewise, on the server's own columns.
func TestForeignServersFiltered(t *testing.T) {
	yes := true
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On(foreignServerMatch, rowsOf(foreignServerTypes, rowServer1, rowServer2))
	d := f.Server().DatabaseRef("appdb")
	ss, err := d.ForeignServers(t.Context(), pgmo.ObjectFilter{
		Name:            []pgmo.TextCriterion{{Op: pgmo.TextNotEquals, Value: "X"}},
		Schema:          []pgmo.TextCriterion{{Op: pgmo.TextEquals, Value: "app"}},
		System:          &yes,
		ExtensionMember: &yes,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ss) != 2 {
		t.Fatalf("%d servers, want 2", len(ss))
	}
	checkServers(t, ss, d)
	last := lastQuery(other)
	checkFragments(t, last, foreignServerFragments)
	tail := "WHERE true AND s.srvname <> $1::pg_catalog.text AND s.oid < 16384 AND EXISTS (SELECT FROM pg_catalog.pg_depend d WHERE " +
		"d.classid = 'pg_catalog.pg_foreign_server'::pg_catalog.regclass AND d.objid = s.oid"
	if !strings.Contains(last.Text, tail) || !strings.HasSuffix(last.Text, "\nORDER BY s.srvname") ||
		len(last.Args) != 1 || last.Arg(0) != "x" || strings.Contains(last.Text, "nspname = $") {
		t.Errorf("filter not pushed down, then ordered: args %v\n%s", last.Args, last.Text)
	}
}

// The user mapping listing fills every field; a name or schema criterion is
// no column, and System or ExtensionMember true keeps none.
func TestUserMappingsFiltered(t *testing.T) {
	yes, no := true, false
	for _, c := range []struct {
		flag *bool
		tail string
	}{
		{&no, "WHERE true AND NOT false AND NOT false\nORDER BY um.srvname, 5"},
		{&yes, "WHERE true AND false AND false\nORDER BY um.srvname, 5"},
	} {
		f := pgmotest.New(t)
		other := f.Database("appdb")
		other.On(userMappingMatch, rowsOf(userMappingTypes, rowMapping1, rowMapping2))
		d := f.Server().DatabaseRef("appdb")
		ms, err := d.UserMappings(t.Context(), pgmo.ObjectFilter{
			Name:            []pgmo.TextCriterion{{Op: pgmo.TextEquals, Value: "remote"}},
			Schema:          []pgmo.TextCriterion{{Op: pgmo.TextEquals, Value: "app"}},
			System:          c.flag,
			ExtensionMember: c.flag,
		})
		if err != nil {
			t.Fatal(err)
		}
		if len(ms) != 2 {
			t.Fatalf("%d mappings, want 2", len(ms))
		}
		checkMappings(t, ms, d)
		last := lastQuery(other)
		checkFragments(t, last, userMappingFragments)
		if !strings.HasSuffix(last.Text, c.tail) || len(last.Args) != 0 {
			t.Errorf("System/ExtensionMember %v: args %v\n%s", *c.flag, last.Args, last.Text)
		}
	}
	if !wantMappingsCopy(0).IsPublic() || wantMappingsCopy(1).IsPublic() {
		t.Errorf("IsPublic wrong")
	}
	if s := wantMappingsCopy(0).Signature(); s != "FOR PUBLIC SERVER remote" {
		t.Errorf("Signature() = %s", s)
	}
	if s := wantMappingsCopy(1).Signature(); s != `FOR "B b" SERVER "Odd S"` {
		t.Errorf("Signature() = %s", s)
	}
}

// wantMappingsCopy is a copy of wantMappings[i], to call its methods on.
func wantMappingsCopy(i int) *pgmo.UserMapping {
	m := wantMappings[i]
	return &m
}

// Each by-name read binds its names, fills every field, and tells a missing
// object from a failed read; each Ref reads nothing.
func TestForeignDataByName(t *testing.T) {
	cases := []struct {
		name, match string
		types       []uint32
		row         []any
		read        func(*pgmo.Database) (any, error)
		want        any
		clear       func(any) any
		tail        string
		args        []any
		missing     string
		failed      string
	}{
		{"wrapper", foreignDataWrapperMatch, foreignDataWrapperTypes, rowWrapper2,
			func(d *pgmo.Database) (any, error) { return d.ForeignDataWrapperByName(t.Context(), "My W") },
			wantWrappers[1], func(x any) any { w := *x.(*pgmo.ForeignDataWrapper); w.SetDatabase(nil); return w },
			"WHERE true\n\tAND w.fdwname = $1::pg_catalog.text", []any{"My W"},
			`foreign data wrapper "My W" not found in database "appdb"`, `pgmo: read foreign data wrapper "My W" of database "appdb"`},
		{"server", foreignServerMatch, foreignServerTypes, rowServer2,
			func(d *pgmo.Database) (any, error) { return d.ForeignServerByName(t.Context(), "Odd S") },
			wantServers[1], func(x any) any { s := *x.(*pgmo.ForeignServer); s.SetDatabase(nil); return s },
			"WHERE true\n\tAND s.srvname = $1::pg_catalog.text", []any{"Odd S"},
			`foreign server "Odd S" not found in database "appdb"`, `pgmo: read foreign server "Odd S" of database "appdb"`},
		{"user mapping", userMappingMatch, userMappingTypes, rowMapping2,
			func(d *pgmo.Database) (any, error) { return d.UserMappingByName(t.Context(), "Odd S", "B b") },
			wantMappings[1], func(x any) any { m := *x.(*pgmo.UserMapping); m.SetDatabase(nil); return m },
			"WHERE true\n\tAND um.srvname = $1::pg_catalog.text\n\tAND CASE WHEN um.umuser = 0 THEN '' ELSE um.usename::pg_catalog.text END = $2::pg_catalog.text",
			[]any{"Odd S", "B b"},
			`user mapping FOR "B b" SERVER "Odd S" not found in database "appdb"`,
			`pgmo: read user mapping FOR "B b" SERVER "Odd S" of database "appdb"`},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			f := pgmotest.New(t)
			other := f.Database("appdb")
			other.On(c.match, rowsOf(c.types, c.row))
			d := f.Server().DatabaseRef("appdb")
			got, err := c.read(d)
			if err != nil {
				t.Fatal(err)
			}
			if g := c.clear(got); !reflect.DeepEqual(g, c.want) {
				t.Errorf("= %+v, want %+v", g, c.want)
			}
			last := lastQuery(other)
			if !strings.HasSuffix(last.Text, c.tail) || !reflect.DeepEqual(last.Args, c.args) {
				t.Errorf("names not bound: args %v\n%s", last.Args, last.Text)
			}

			f = pgmotest.New(t)
			other = f.Database("appdb")
			other.On(c.match, rowsOf(c.types))
			d = f.Server().DatabaseRef("appdb")
			if _, err := c.read(d); !errors.Is(err, pgmo.ErrNotFound) || !strings.Contains(err.Error(), c.missing) {
				t.Errorf("missing: err = %v, want ErrNotFound naming it", err)
			}
			f = pgmotest.New(t)
			other = f.Database("appdb")
			other.On(c.match, pgmotest.Fail(&pgconn.PgError{Code: "57014", Message: "canceled"}))
			d = f.Server().DatabaseRef("appdb")
			_, err = c.read(d)
			if err == nil || errors.Is(err, pgmo.ErrNotFound) || !strings.HasPrefix(err.Error(), c.failed+": ") ||
				strings.Count(err.Error(), "pgmo: ") != 1 {
				t.Errorf("server error: err = %v, want it wrapped once as %q", err, c.failed)
			}
		})
	}

	f := pgmotest.New(t)
	d := f.Server().DatabaseRef("appdb")
	n := len(f.Queries())
	w, s, m := d.ForeignDataWrapperRef("w"), d.ForeignServerRef("s"), d.UserMappingRef("s", "")
	if w.Database() != d || w.Name != "w" || w.OID != 0 || s.Database() != d || s.Name != "s" || s.OID != 0 ||
		m.Database() != d || m.Server != "s" || m.User != "" || !m.IsPublic() || m.OID != 0 {
		t.Errorf("Refs = %+v, %+v, %+v", w, s, m)
	}
	if w.QualifiedName() != "w" || d.ForeignServerRef("Odd S").QualifiedName() != `"Odd S"` {
		t.Errorf("QualifiedName() = %s, %s", w.QualifiedName(), d.ForeignServerRef("Odd S").QualifiedName())
	}
	if len(f.Queries()) != n {
		t.Errorf("a Ref sent a statement")
	}
}

// A wrapper's servers and a server's user mappings are the listings
// narrowed by the parent's name — so a Ref works — bound as $1, and go to
// the database's own pool.
func TestForeignDataParts(t *testing.T) {
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On(foreignServerMatch, rowsOf(foreignServerTypes, rowServer1, rowServer2))
	other.On(userMappingMatch, rowsOf(userMappingTypes, rowMapping1, rowMapping2))
	d := f.Server().DatabaseRef("appdb")

	ss, err := d.ForeignDataWrapperRef("My W").Servers(t.Context())
	if err != nil || len(ss) != 2 {
		t.Fatalf("Servers = %v, %v", ss, err)
	}
	checkServers(t, ss, d)
	last := lastQuery(other)
	checkFragments(t, last, foreignServerFragments)
	if !strings.HasSuffix(last.Text, "WHERE true\n\tAND sw.fdwname = $1::pg_catalog.text\nORDER BY s.srvname") ||
		!reflect.DeepEqual(last.Args, []any{"My W"}) {
		t.Errorf("Servers not narrowed by the wrapper: args %v\n%s", last.Args, last.Text)
	}

	ms, err := d.ForeignServerRef("Odd S").UserMappings(t.Context())
	if err != nil || len(ms) != 2 {
		t.Fatalf("UserMappings = %v, %v", ms, err)
	}
	checkMappings(t, ms, d)
	last = lastQuery(other)
	checkFragments(t, last, userMappingFragments)
	if !strings.HasSuffix(last.Text, "WHERE true\n\tAND um.srvname = $1::pg_catalog.text\nORDER BY um.srvname, 5") ||
		!reflect.DeepEqual(last.Args, []any{"Odd S"}) {
		t.Errorf("UserMappings not narrowed by the server: args %v\n%s", last.Args, last.Text)
	}
	if n := len(f.Queries()); n != 1 {
		t.Errorf("%d statements on the Server's own database, want 1 (its version read)", n)
	}

	f = pgmotest.New(t)
	other = f.Database("appdb")
	other.On(foreignServerMatch, pgmotest.Fail(&pgconn.PgError{Code: "57014", Message: "canceled"}))
	other.On(userMappingMatch, pgmotest.Fail(&pgconn.PgError{Code: "57014", Message: "canceled"}))
	d = f.Server().DatabaseRef("appdb")
	for what, err := range map[string]error{
		"pgmo: list foreign servers of foreign data wrapper w: ": func() error { _, err := d.ForeignDataWrapperRef("w").Servers(t.Context()); return err }(),
		`pgmo: list user mappings of foreign server "S s": `:     func() error { _, err := d.ForeignServerRef("S s").UserMappings(t.Context()); return err }(),
		`pgmo: list foreign servers of database "appdb": `:       func() error { _, err := d.ForeignServers(t.Context(), pgmo.ObjectFilter{}); return err }(),
		`pgmo: list user mappings of database "appdb": `:         func() error { _, err := d.UserMappings(t.Context(), pgmo.ObjectFilter{}); return err }(),
	} {
		if err == nil || !strings.HasPrefix(err.Error(), what) || strings.Count(err.Error(), "pgmo: ") != 1 {
			t.Errorf("err = %v, want it wrapped once as %q", err, what)
		}
	}
}
