package pgmo_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/radix29/pgmo"
	"github.com/radix29/pgmo/pgmotest"
)

// The column types of every dependency read, in SELECT order.
var dependencyTypes = []uint32{pgtype.TextOID, pgtype.TextOID, pgtype.OIDOID, pgtype.TextOID, pgtype.TextOID,
	pgtype.TextOID, pgtype.BoolOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID, pgtype.TextOID}

// The reads told apart in a fake: each direction inside a database, and
// between shared objects.
const (
	dependenciesMatch       = "FROM parts p JOIN pg_catalog.pg_depend d ON d.classid = p.cls"
	dependentsMatch         = "FROM parts p JOIN pg_catalog.pg_depend d ON d.refclassid = p.cls"
	sharedDependenciesMatch = "JOIN pg_catalog.pg_shdepend s ON s.dbid = 0 AND s.classid = x.cls"
	sharedDependentsMatch   = "JOIN pg_catalog.pg_shdepend s ON s.dbid = 0 AND s.refclassid = x.cls"
	dependentDatabasesMatch = `SELECT DISTINCT db.datname::pg_catalog.text COLLATE pg_catalog."C"`
)

// Every column lands in its field, each deptype letter maps to its kind by
// the catalog it came from, a letter pgmo does not know is kept rather than
// read as another, the address is bound as $1 and $2 and never written
// into the text, and the read goes to the database's own pool.
func TestDependenciesFillEveryField(t *testing.T) {
	f := pgmotest.New(t)
	other := f.Database("appdb")
	row := func(catalog string, oid uint32, typ, identity, deptype string, shared bool, partType, part, ownType, own string) []any {
		return []any{"app.v", catalog, oid, typ, identity, deptype, shared, partType, part, ownType, own}
	}
	other.On(dependenciesMatch, rowsOf(dependencyTypes,
		row("pg_class", 501, "table", "app.t", "n", false, "table column", "app.t.id", "rule", `"_RETURN" on app.v`),
		row("pg_authid", 10, "role", `"R r"`, "o", true, "", "", "", ""),
		row("pg_authid", 11, "role", "r2", "a", true, "", "", "", ""),
		row("pg_authid", 12, "role", "r3", "r", true, "", "", "policy", "p on app.t"),
		row("pg_authid", 13, "role", "r4", "i", true, "", "", "", ""),
		row("pg_tablespace", 14, "tablespace", "ts", "t", true, "", "", "", ""),
		row("pg_namespace", 15, "schema", "app", "a", false, "", "", "", ""),
		row("pg_class", 16, "sequence", "app.s", "i", false, "", "", "", ""),
		row("pg_class", 17, "index", "app.i", "P", false, "", "", "", ""),
		row("pg_class", 18, "table", "app.p", "S", false, "", "", "", ""),
		row("pg_extension", 19, "extension", "e", "e", false, "", "", "", ""),
		row("pg_extension", 20, "extension", "e2", "x", false, "", "", "", ""),
		row("pg_proc", 21, "function", "app.f()", "q", false, "", "", "", ""),
		row("pg_authid", 22, "role", "r5", "q", true, "", "", "", "")))
	d := f.Server().DatabaseRef("appdb")
	got, err := d.Dependencies(t.Context(), pgmo.ObjectAddress{Catalog: "pg_class", OID: 500})
	if err != nil {
		t.Fatal(err)
	}
	if n := reflect.TypeFor[pgmo.Dependency]().NumField(); n != 10 {
		t.Errorf("Dependency has %d fields; this test checks 10 — add the new one", n)
	}
	want0 := pgmo.Dependency{Catalog: "pg_class", OID: 501, Type: "table", Identity: "app.t", Kind: pgmo.DependencyNormal,
		PartType: "table column", Part: "app.t.id", OwnPartType: "rule", OwnPart: `"_RETURN" on app.v`}
	if len(got) != 14 || got[0] != want0 {
		t.Fatalf("Dependencies = %+v, want 14 starting %+v", got, want0)
	}
	if got[0].Address() != (pgmo.ObjectAddress{Catalog: "pg_class", OID: 501}) {
		t.Errorf("Address() = %+v", got[0].Address())
	}
	kinds := []pgmo.DependencyKind{pgmo.DependencyNormal, pgmo.DependencyOwner, pgmo.DependencyPrivilege, pgmo.DependencyPolicyRole,
		pgmo.DependencyInitialPrivilege, pgmo.DependencyTablespace, pgmo.DependencyAuto, pgmo.DependencyInternal,
		pgmo.DependencyPartitionPrimary, pgmo.DependencyPartitionSecondary, pgmo.DependencyExtension, pgmo.DependencyAutoExtension,
		`pg_depend deptype "q"`, `pg_shdepend deptype "q"`}
	for i, k := range kinds {
		if got[i].Kind != k {
			t.Errorf("row %d: Kind = %q, want %q", i, got[i].Kind, k)
		}
		if shared := got[i].Catalog == "pg_authid" || got[i].Catalog == "pg_tablespace"; got[i].Shared != shared {
			t.Errorf("row %d: Shared = %v", i, got[i].Shared)
		}
	}
	last := lastQuery(other)
	if last.Arg(0) != "pg_class" || last.Arg(1) != pgmo.OID(500) || len(last.Args) != 2 {
		t.Errorf("args %v, want the catalog and the OID", last.Args)
	}
	if strings.Contains(last.Text, "500") {
		t.Errorf("the OID is in the text:\n%s", last.Text)
	}
	for _, q := range f.Queries() {
		if strings.Contains(q.Text, dependenciesMatch) {
			t.Errorf("read through the Server's own database: %s", q.Text)
		}
	}
}

// The read's own row tells the cases apart: an object with no edge is an
// empty list, an identity of NULL — or no row, for a catalog that is no
// pg_catalog table — is ErrNotFound; a server error is wrapped once.
func TestDependenciesNotFoundAndNone(t *testing.T) {
	addr := pgmo.ObjectAddress{Catalog: "pg_proc", OID: 7}
	nulls := []any{nil, nil, nil, nil, nil, nil, nil, nil, nil, nil, nil}
	for _, c := range []struct {
		name    string
		rows    [][]any
		wantErr error
	}{
		{"no edge", [][]any{append([]any{"app.f()"}, nulls[1:]...)}, nil},
		{"no object", [][]any{nulls}, pgmo.ErrNotFound},
		{"no catalog", nil, pgmo.ErrNotFound},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := pgmotest.New(t)
			f.On(dependentsMatch, rowsOf(dependencyTypes, c.rows...))
			got, err := f.Server().DatabaseRef(pgmotest.DefaultDatabase).Dependents(t.Context(), addr)
			if !errors.Is(err, c.wantErr) || (err == nil) != (c.wantErr == nil) {
				t.Fatalf("err = %v, want %v", err, c.wantErr)
			}
			if err != nil && (!strings.HasPrefix(err.Error(), `pgmo: list dependents of pg_proc 7 in database "`) ||
				strings.Count(err.Error(), "pgmo: ") != 1) {
				t.Errorf("err = %v, want it naming the read once", err)
			}
			if got != nil {
				t.Errorf("Dependents = %+v, want none", got)
			}
		})
	}

	f := pgmotest.New(t)
	f.On(dependentsMatch, pgmotest.Fail(&pgconn.PgError{Code: "57014", Message: "canceled"}))
	_, err := f.Server().DatabaseRef(pgmotest.DefaultDatabase).Dependents(t.Context(), addr)
	if err == nil || !strings.HasPrefix(err.Error(), "pgmo: list dependents of pg_proc 7 in database ") || strings.Count(err.Error(), "pgmo: ") != 1 {
		t.Errorf("server error: err = %v, want it wrapped once naming the read", err)
	}
}

// Each read goes where its object's edges are: Database's inside the
// database (dependents of a shared object too), Server's between shared
// objects; an address no read can find, or one for the other side, is
// refused before anything is sent.
func TestDependencyReadsRefuse(t *testing.T) {
	f := pgmotest.New(t)
	s := f.Server()
	d := s.DatabaseRef("appdb")
	table := pgmo.ObjectAddress{Catalog: "pg_class", OID: 1}
	role := pgmo.ObjectAddress{Catalog: "pg_authid", OID: 2}
	deps := func(read func(context.Context, pgmo.ObjectAddress) ([]pgmo.Dependency, error), a pgmo.ObjectAddress) func(context.Context) error {
		return func(ctx context.Context) error { _, err := read(ctx, a); return err }
	}
	dbs := func(a pgmo.ObjectAddress) func(context.Context) error {
		return func(ctx context.Context) error { _, err := s.DependentDatabases(ctx, a); return err }
	}
	inv, ref := pgmo.ErrInvalidRequest, pgmo.ErrHandleNotLoaded
	runRefusals(t, f, []refusalCase{
		{"Database.Dependencies: a Ref", deps(d.Dependencies, d.TableRef("app", "t").Address()), ref},
		{"Database.Dependencies: no catalog", deps(d.Dependencies, pgmo.ObjectAddress{OID: 1}), inv},
		{"Database.Dependencies: a shared object", deps(d.Dependencies, role), inv},
		{"Database.Dependents: a Ref", deps(d.Dependents, d.SchemaRef("app").Address()), ref},
		{"Database.Dependents: no catalog", deps(d.Dependents, pgmo.ObjectAddress{OID: 1}), inv},
		{"Server.Dependencies: a Ref", deps(s.Dependencies, s.RoleRef("r").Address()), ref},
		{"Server.Dependencies: not shared", deps(s.Dependencies, table), inv},
		{"Server.Dependents: a Ref", deps(s.Dependents, s.TablespaceRef("t").Address()), ref},
		{"Server.Dependents: not shared", deps(s.Dependents, table), inv},
		{"Server.DependentDatabases: a Ref", dbs(s.DatabaseRef("x").Address()), ref},
		{"Server.DependentDatabases: not shared", dbs(table), inv},
	})

	// The reads each side takes.
	f.Database("appdb").On(dependentsMatch, rowsOf(dependencyTypes,
		[]any{"r", "pg_class", uint32(5), "table", "app.t", "a", true, "", "", "", ""}))
	got, err := d.Dependents(t.Context(), role)
	if err != nil || len(got) != 1 || got[0].Kind != pgmo.DependencyPrivilege || !got[0].Shared {
		t.Errorf("Database.Dependents(role) = %+v, %v", got, err)
	}
	f.On(sharedDependenciesMatch, rowsOf(dependencyTypes,
		[]any{"appdb", "pg_authid", uint32(2), "role", "r", "o", true, "", "", "", ""}))
	f.On(sharedDependentsMatch, rowsOf(dependencyTypes,
		[]any{"r", "pg_database", uint32(3), "database", "appdb", "o", true, "", "", "", ""}))
	f.On(dependentDatabasesMatch, rowsOf([]uint32{pgtype.TextOID}, []any{"appdb"}, []any{`My "DB"`}))
	if got, err := s.Dependencies(t.Context(), pgmo.ObjectAddress{Catalog: "pg_database", OID: 3}); err != nil ||
		len(got) != 1 || got[0].Kind != pgmo.DependencyOwner || got[0].Identity != "r" {
		t.Errorf("Server.Dependencies = %+v, %v", got, err)
	}
	if got, err := s.Dependents(t.Context(), role); err != nil || len(got) != 1 || got[0].Catalog != "pg_database" {
		t.Errorf("Server.Dependents = %+v, %v", got, err)
	}
	if got, err := s.DependentDatabases(t.Context(), role); err != nil || !reflect.DeepEqual(got, []string{"appdb", `My "DB"`}) {
		t.Errorf("Server.DependentDatabases = %q, %v", got, err)
	}
	if last := lastQuery(f); last.Arg(0) != "pg_authid" || last.Arg(1) != pgmo.OID(2) {
		t.Errorf("args %v, want the role's address", last.Args)
	}
}

// Every handle names its catalog, and a Ref's OID is the zero the reads
// refuse.
func TestObjectAddresses(t *testing.T) {
	f := pgmotest.New(t)
	s := f.Server()
	d := s.DatabaseRef("db")
	for _, c := range []struct {
		addr    pgmo.ObjectAddress
		catalog string
	}{
		{d.Address(), "pg_database"},
		{s.RoleRef("r").Address(), "pg_authid"},
		{s.TablespaceRef("t").Address(), "pg_tablespace"},
		{d.SchemaRef("s").Address(), "pg_namespace"},
		{d.TableRef("s", "t").Address(), "pg_class"},
		{d.ViewRef("s", "v").Address(), "pg_class"},
		{d.MaterializedViewRef("s", "m").Address(), "pg_class"},
		{d.ForeignTableRef("s", "f").Address(), "pg_class"},
		{d.SequenceRef("s", "q").Address(), "pg_class"},
		{(&pgmo.Index{}).Address(), "pg_class"},
		{(&pgmo.Constraint{}).Address(), "pg_constraint"},
		{d.FunctionRef("s", "f", nil).Address(), "pg_proc"},
		{d.ProcedureRef("s", "p", nil).Address(), "pg_proc"},
		{d.AggregateRef("s", "a", nil).Address(), "pg_proc"},
		{d.TypeRef("s", "t").Address(), "pg_type"},
		{d.DomainRef("s", "d").Address(), "pg_type"},
		{d.TriggerRef("s", "t", "g").Address(), "pg_trigger"},
		{d.EventTriggerRef("e").Address(), "pg_event_trigger"},
		{d.PolicyRef("s", "t", "p").Address(), "pg_policy"},
		{d.ExtensionRef("e").Address(), "pg_extension"},
		{d.CollationRef("s", "c").Address(), "pg_collation"},
		{d.TextSearchParserRef("s", "p").Address(), "pg_ts_parser"},
		{d.TextSearchTemplateRef("s", "t").Address(), "pg_ts_template"},
		{d.TextSearchDictionaryRef("s", "d").Address(), "pg_ts_dict"},
		{d.TextSearchConfigurationRef("s", "c").Address(), "pg_ts_config"},
		{d.LanguageRef("l").Address(), "pg_language"},
		{d.CastRef("a", "b").Address(), "pg_cast"},
		{d.OperatorRef("s", "+", "a", "b").Address(), "pg_operator"},
		{d.ForeignDataWrapperRef("w").Address(), "pg_foreign_data_wrapper"},
		{d.ForeignServerRef("v").Address(), "pg_foreign_server"},
		{d.UserMappingRef("v", "u").Address(), "pg_user_mapping"},
		{pgmo.ExtensionMember{Catalog: "pg_type", OID: 9}.Address(), "pg_type"},
		{pgmo.SearchResult{Catalog: "pg_trigger", OID: 9}.Address(), "pg_trigger"},
	} {
		if c.addr.Catalog != c.catalog {
			t.Errorf("%+v: want catalog %s", c.addr, c.catalog)
		}
		if c.addr.IsShared() != (c.catalog == "pg_database" || c.catalog == "pg_authid" || c.catalog == "pg_tablespace") {
			t.Errorf("%+v: IsShared() = %v", c.addr, c.addr.IsShared())
		}
	}
	if !(pgmo.ObjectAddress{Catalog: "pg_parameter_acl"}).IsShared() {
		t.Error("pg_parameter_acl is shared")
	}
}
