package pgmo_test

import (
	"context"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/radix29/pgmo"
	"github.com/radix29/pgmo/pgmotest"
)

// Every request P4.13.4 cannot write is refused before anything is sent or
// captured: an incomplete securable, a privilege its kind does not take (the
// allowlist that keeps a caller's text out of the SQL), a modifier the
// action has no form for, a shared object through a Database or the other
// way round, a release that lacks the form.
func TestPrivilegeWriteRefusals(t *testing.T) {
	f := pgmotest.New(t)
	f.Version = pgmo.PostgreSQL16
	s := f.Server()
	d := s.DatabaseRef("app")
	tbl := pgmo.Securable{Kind: pgmo.SecurableTable, Schema: "app", Name: "t"}
	ok := pgmo.PrivilegeChange{Action: pgmo.ActionGrant, Privileges: []pgmo.Privilege{pgmo.PrivilegeSelect}, Grantees: []string{"r"}}
	with := func(m func(*pgmo.PrivilegeChange)) pgmo.PrivilegeChange { c := ok; m(&c); return c }
	on := func(m func(*pgmo.Securable)) pgmo.Securable { s := tbl; m(&s); return s }
	apply := func(sec pgmo.Securable, c pgmo.PrivilegeChange) func(context.Context) error {
		return func(ctx context.Context) error { return d.ApplyPrivileges(ctx, sec, c) }
	}
	cluster := func(sec pgmo.Securable, c pgmo.PrivilegeChange) func(context.Context) error {
		return func(ctx context.Context) error { return s.ApplyPrivileges(ctx, sec, c) }
	}
	privs := func(p ...pgmo.Privilege) pgmo.PrivilegeChange {
		return with(func(c *pgmo.PrivilegeChange) { c.Privileges = p })
	}
	defaults := func(scope pgmo.DefaultPrivilegeScope, c pgmo.PrivilegeChange) func(context.Context) error {
		return func(ctx context.Context) error { return d.AlterDefaultPrivileges(ctx, scope, c) }
	}
	tables := pgmo.DefaultPrivilegeScope{ObjectType: pgmo.DefaultPrivilegeTables}
	policy := func(r pgmo.CreatePolicyRequest) func(context.Context) error {
		return func(ctx context.Context) error {
			if r.Schema == "" && r.Table == "" && r.Name == "" {
				r.Schema, r.Table, r.Name = "app", "t", "p"
			}
			_, err := d.CreatePolicy(ctx, r)
			return err
		}
	}
	pol := d.PolicyRef("app", "t", "p")
	fn := pgmo.Securable{Kind: pgmo.SecurableFunction, Schema: "app", Name: "f", Arguments: []string{}}
	inv, sch, ver := pgmo.ErrInvalidRequest, pgmo.ErrSchemaRequired, pgmo.ErrUnsupportedVersion

	runRefusals(t, f, []refusalCase{
		// The change
		{"unknown action", apply(tbl, with(func(c *pgmo.PrivilegeChange) { c.Action = "DENY" })), inv},
		{"no action", apply(tbl, with(func(c *pgmo.PrivilegeChange) { c.Action = "" })), inv},
		{"no grantees", apply(tbl, with(func(c *pgmo.PrivilegeChange) { c.Grantees = nil })), inv},
		{"no privileges", apply(tbl, with(func(c *pgmo.PrivilegeChange) { c.Privileges = nil })), inv},
		{"empty privilege list", apply(tbl, privs()), inv},
		{"all and a list", apply(tbl, with(func(c *pgmo.PrivilegeChange) { c.All = true })), inv},
		{"cascade on a grant", apply(tbl, with(func(c *pgmo.PrivilegeChange) { c.Cascade = true })), inv},
		{"injected privilege", apply(tbl, privs("SELECT ON app.t TO r; DROP TABLE app.t; --")), inv},
		{"lower-case privilege", apply(tbl, privs("select")), inv},
		{"MAINTAIN before 17", apply(tbl, privs(pgmo.PrivilegeMaintain)), ver},

		// Privileges by kind
		{"usage on a table", apply(tbl, privs(pgmo.PrivilegeUsage)), inv},
		{"delete on a column", apply(on(func(s *pgmo.Securable) { s.Columns = []string{"a"} }), privs(pgmo.PrivilegeDelete)), inv},
		{"insert on a sequence", apply(pgmo.Securable{Kind: pgmo.SecurableSequence, Schema: "app", Name: "s"}, privs(pgmo.PrivilegeInsert)), inv},
		{"select on a function", apply(fn, privs(pgmo.PrivilegeSelect)), inv},
		{"create on a type", apply(pgmo.Securable{Kind: pgmo.SecurableType, Schema: "app", Name: "ty"}, privs(pgmo.PrivilegeCreate)), inv},
		{"select on a schema", apply(pgmo.Securable{Kind: pgmo.SecurableSchema, Name: "app"}, privs(pgmo.PrivilegeSelect)), inv},
		{"usage on a large object", apply(pgmo.Securable{Kind: pgmo.SecurableLargeObject, Name: "1"}, privs(pgmo.PrivilegeUsage)), inv},
		{"connect on a tablespace", cluster(pgmo.Securable{Kind: pgmo.SecurableTablespace, Name: "ts"}, privs(pgmo.PrivilegeConnect)), inv},
		{"usage on a database", cluster(pgmo.Securable{Kind: pgmo.SecurableDatabase, Name: "app"}, privs(pgmo.PrivilegeUsage)), inv},
		{"select on a parameter", cluster(pgmo.Securable{Kind: pgmo.SecurableParameter, Name: "work_mem"}, privs(pgmo.PrivilegeSelect)), inv},

		// The securable
		{"unknown kind", apply(pgmo.Securable{Kind: "INDEX", Schema: "app", Name: "i"}, ok), inv},
		{"no kind", apply(pgmo.Securable{Schema: "app", Name: "i"}, ok), inv},
		{"table without a schema", apply(on(func(s *pgmo.Securable) { s.Schema = "" }), ok), sch},
		{"table without a name", apply(on(func(s *pgmo.Securable) { s.Name = "" }), ok), inv},
		{"schema on a schema", apply(pgmo.Securable{Kind: pgmo.SecurableSchema, Schema: "x", Name: "app"}, privs(pgmo.PrivilegeUsage)), inv},
		{"language without a name", apply(pgmo.Securable{Kind: pgmo.SecurableLanguage}, privs(pgmo.PrivilegeUsage)), inv},
		{"empty column list", apply(on(func(s *pgmo.Securable) { s.Columns = []string{} }), ok), inv},
		{"empty column name", apply(on(func(s *pgmo.Securable) { s.Columns = []string{"a", ""} }), ok), inv},
		{"columns on a sequence", apply(pgmo.Securable{Kind: pgmo.SecurableSequence, Schema: "app", Name: "s", Columns: []string{"a"}},
			privs(pgmo.PrivilegeSelect)), inv},
		{"columns on all tables", apply(pgmo.Securable{Kind: pgmo.SecurableTable, Schema: "app", AllInSchema: true, Columns: []string{"a"}}, ok), inv},
		{"routine without argument types", apply(pgmo.Securable{Kind: pgmo.SecurableFunction, Schema: "app", Name: "f"},
			privs(pgmo.PrivilegeExecute)), inv},
		{"routine with an empty argument type", apply(pgmo.Securable{Kind: pgmo.SecurableProcedure, Schema: "app", Name: "p",
			Arguments: []string{""}}, privs(pgmo.PrivilegeExecute)), inv},
		{"arguments on a table", apply(on(func(s *pgmo.Securable) { s.Arguments = []string{} }), ok), inv},
		{"all in a schema-less kind", apply(pgmo.Securable{Kind: pgmo.SecurableSchema, Schema: "app", AllInSchema: true}, privs(pgmo.PrivilegeUsage)), inv},
		{"all types in schema", apply(pgmo.Securable{Kind: pgmo.SecurableType, Schema: "app", AllInSchema: true}, privs(pgmo.PrivilegeUsage)), inv},
		{"all in no schema", apply(pgmo.Securable{Kind: pgmo.SecurableTable, AllInSchema: true}, ok), sch},
		{"all with a name", apply(on(func(s *pgmo.Securable) { s.AllInSchema = true }), ok), inv},
		{"large object not a number", apply(pgmo.Securable{Kind: pgmo.SecurableLargeObject, Name: "1; DROP"}, ok), inv},
		{"large object zero", apply(pgmo.Securable{Kind: pgmo.SecurableLargeObject, Name: "0"}, ok), inv},
		{"large object leading zero", apply(pgmo.Securable{Kind: pgmo.SecurableLargeObject, Name: "01"}, ok), inv},
		{"large object too big", apply(pgmo.Securable{Kind: pgmo.SecurableLargeObject, Name: "4294967296"}, ok), inv},
		{"database through a database", apply(d.Securable(), privs(pgmo.PrivilegeConnect)), inv},
		{"parameter through a database", apply(pgmo.Securable{Kind: pgmo.SecurableParameter, Name: "work_mem"}, privs(pgmo.PrivilegeSet)), inv},
		{"table through the server", cluster(tbl, ok), inv},
		{"database without a name", cluster(pgmo.Securable{Kind: pgmo.SecurableDatabase}, privs(pgmo.PrivilegeConnect)), inv},

		// ALTER DEFAULT PRIVILEGES
		{"defaults unknown type", defaults(pgmo.DefaultPrivilegeScope{ObjectType: "x"}, ok), inv},
		{"defaults no type", defaults(pgmo.DefaultPrivilegeScope{}, ok), inv},
		{"defaults wrong privilege", defaults(tables, privs(pgmo.PrivilegeExecute)), inv},
		{"defaults MAINTAIN before 17", defaults(tables, privs(pgmo.PrivilegeMaintain)), ver},
		{"defaults large objects before 18", defaults(pgmo.DefaultPrivilegeScope{ObjectType: pgmo.DefaultPrivilegeLargeObjects}, ok), ver},
		{"defaults empty role list", defaults(pgmo.DefaultPrivilegeScope{Roles: []string{}, ObjectType: pgmo.DefaultPrivilegeTables}, ok), inv},
		{"defaults empty role", defaults(pgmo.DefaultPrivilegeScope{Roles: []string{""}, ObjectType: pgmo.DefaultPrivilegeTables}, ok), inv},
		{"defaults empty schema list", defaults(pgmo.DefaultPrivilegeScope{Schemas: []string{}, ObjectType: pgmo.DefaultPrivilegeTables}, ok), inv},
		{"defaults empty schema", defaults(pgmo.DefaultPrivilegeScope{Schemas: []string{"app", ""}, ObjectType: pgmo.DefaultPrivilegeTables}, ok), inv},
		{"defaults no grantees", defaults(tables, with(func(c *pgmo.PrivilegeChange) { c.Grantees = nil })), inv},

		// Policies
		{"policy no name", policy(pgmo.CreatePolicyRequest{Schema: "app", Table: "t"}), inv},
		{"policy no schema", policy(pgmo.CreatePolicyRequest{Table: "t", Name: "p"}), sch},
		{"policy no table", policy(pgmo.CreatePolicyRequest{Schema: "app", Name: "p"}), inv},
		{"policy unknown command", policy(pgmo.CreatePolicyRequest{Command: "x"}), inv},
		{"policy empty role list", policy(pgmo.CreatePolicyRequest{Roles: []string{}}), inv},
		{"policy alter empty role list", func(ctx context.Context) error { return pol.Alter(ctx, pgmo.PolicyChanges{Roles: []string{}}) }, inv},
		{"policy alter empty using", func(ctx context.Context) error { return pol.Alter(ctx, pgmo.PolicyChanges{Using: new("")}) }, inv},
		{"policy alter empty check", func(ctx context.Context) error { return pol.Alter(ctx, pgmo.PolicyChanges{WithCheck: new("")}) }, inv},
		{"policy rename to nothing", func(ctx context.Context) error { return pol.Rename(ctx, "") }, inv},
		{"policy handle without a table", func(ctx context.Context) error { return d.PolicyRef("app", "", "p").Drop(ctx) }, inv},
		{"policy handle without a schema", func(ctx context.Context) error { return d.PolicyRef("", "t", "p").SetComment(ctx, "x") }, sch},
		{"policy handle without a name", func(ctx context.Context) error { return d.PolicyRef("app", "t", "").Drop(ctx) }, inv},
		{"row security without a schema", func(ctx context.Context) error { return d.TableRef("", "t").SetRowSecurity(ctx, true) }, sch},
		{"forced row security without a name", func(ctx context.Context) error {
			return d.TableRef("app", "").SetForceRowSecurity(ctx, true)
		}, inv},
	})
}

// MAINTAIN from 17 and default privileges on large objects from 18 are
// written; ALL PRIVILEGES is the server's to expand on any release.
func TestPrivilegeWriteGatesFollowTheServer(t *testing.T) {
	for _, c := range []struct {
		v                 pgmo.ServerVersion
		maintain, largeOb bool
	}{{pgmo.PostgreSQL15, false, false}, {pgmo.PostgreSQL16, false, false}, {pgmo.PostgreSQL17, true, false}, {pgmo.PostgreSQL18, true, true}} {
		f := pgmotest.New(t)
		f.Version = c.v
		d := f.Server().DatabaseRef("app")
		ctx, _ := pgmo.WithScript(t.Context())
		maintain := pgmo.PrivilegeChange{Action: pgmo.ActionGrant, Privileges: []pgmo.Privilege{pgmo.PrivilegeMaintain}, Grantees: []string{"r"}}
		err := d.ApplyPrivileges(ctx, pgmo.Securable{Kind: pgmo.SecurableTable, Schema: "app", Name: "t"}, maintain)
		if (err == nil) != c.maintain || err != nil && !errors.Is(err, pgmo.ErrUnsupportedVersion) {
			t.Errorf("MAINTAIN on %s: %v", c.v, err)
		}
		err = d.AlterDefaultPrivileges(ctx, pgmo.DefaultPrivilegeScope{ObjectType: pgmo.DefaultPrivilegeTables}, maintain)
		if (err == nil) != c.maintain || err != nil && !errors.Is(err, pgmo.ErrUnsupportedVersion) {
			t.Errorf("default MAINTAIN on %s: %v", c.v, err)
		}
		lo := pgmo.PrivilegeChange{Action: pgmo.ActionGrant, Privileges: []pgmo.Privilege{pgmo.PrivilegeSelect}, Grantees: []string{"r"}}
		err = d.AlterDefaultPrivileges(ctx, pgmo.DefaultPrivilegeScope{ObjectType: pgmo.DefaultPrivilegeLargeObjects}, lo)
		if (err == nil) != c.largeOb || err != nil && !errors.Is(err, pgmo.ErrUnsupportedVersion) {
			t.Errorf("default privileges on large objects on %s: %v", c.v, err)
		}
		all := pgmo.PrivilegeChange{Action: pgmo.ActionGrant, All: true, Grantees: []string{"r"}}
		if err := d.ApplyPrivileges(ctx, pgmo.Securable{Kind: pgmo.SecurableTable, Schema: "app", Name: "t"}, all); err != nil {
			t.Errorf("ALL PRIVILEGES on %s: %v", c.v, err)
		}
	}
}

// Every P4.13.4 write runs on the transaction when it is in the
// transaction's database; one in another database is refused, scripting or
// not. A shared object's grant runs on the transaction too.
func TestPrivilegeWritesInTransaction(t *testing.T) {
	f := pgmotest.New(t)
	f.On("FROM pg_catalog.pg_policy po", pgmotest.Rows())
	s := f.Server()
	own := s.DatabaseRef(pgmotest.DefaultDatabase)
	other := s.DatabaseRef("other")
	grant := pgmo.PrivilegeChange{Action: pgmo.ActionGrant, Privileges: []pgmo.Privilege{pgmo.PrivilegeSelect}, Grantees: []string{"r"}}
	tbl := pgmo.Securable{Kind: pgmo.SecurableTable, Schema: "app", Name: "t"}

	for _, scripting := range []bool{false, true} {
		ctx := t.Context()
		if scripting {
			ctx, _ = pgmo.WithScript(ctx)
		}
		before := len(f.Queries())
		var inner error
		_ = s.InTransaction(ctx, func(ctx context.Context) error {
			inner = other.ApplyPrivileges(ctx, tbl, grant)
			return inner
		})
		if !errors.Is(inner, pgmo.ErrUnsupported) {
			t.Errorf("another database (scripting %v): %v, want ErrUnsupported", scripting, inner)
		}
		for _, q := range f.Queries()[before:] {
			if q.Kind == pgmotest.KindExec && q.Text != "begin" && q.Text != "rollback" {
				t.Errorf("sent %q", q.Text)
			}
		}
	}

	before := len(f.Queries())
	err := s.InTransaction(t.Context(), func(ctx context.Context) error {
		_, err := own.CreatePolicy(ctx, pgmo.CreatePolicyRequest{Schema: "app", Table: "t", Name: "p"})
		return errors.Join(err,
			own.ApplyPrivileges(ctx, tbl, grant),
			s.ApplyPrivileges(ctx, own.Securable(), pgmo.PrivilegeChange{Action: pgmo.ActionRevoke, All: true, Grantees: []string{""}}),
			own.AlterDefaultPrivileges(ctx, pgmo.DefaultPrivilegeScope{ObjectType: pgmo.DefaultPrivilegeTables}, grant),
			own.TableRef("app", "t").SetRowSecurity(ctx, true),
			own.TableRef("app", "t").SetForceRowSecurity(ctx, true),
			own.PolicyRef("app", "t", "p").Alter(ctx, pgmo.PolicyChanges{Using: new("true")}),
			own.PolicyRef("app", "t", "p").Drop(ctx),
		)
	})
	if err != nil {
		t.Fatal(err)
	}
	var stmts int
	for _, q := range f.Queries()[before:] {
		if q.Tx == 0 {
			t.Errorf("%q ran outside the transaction", q.Text)
		}
		if q.Kind == pgmotest.KindExec && q.Text != "begin" && q.Text != "commit" {
			stmts++
		}
	}
	if stmts != 8 {
		t.Errorf("sent %d statements on the transaction, want 8", stmts)
	}
}

// Row-level security, a policy's name and comment are mirrored onto their
// handles when the write ran, not under WithScript; a grant and a policy's
// Alter mirror nothing (the server merges and renders).
func TestPrivilegeWritesMirrorOnlyWhenApplied(t *testing.T) {
	f := pgmotest.New(t)
	d := f.Server().DatabaseRef("app")
	for _, scripting := range []bool{true, false} {
		ctx := t.Context()
		if scripting {
			ctx, _ = pgmo.WithScript(ctx)
		}
		tb, p := d.TableRef("app", "t"), d.PolicyRef("app", "t", "p")
		p.Roles, p.Using = []string{"r"}, "a > 0"
		if err := errors.Join(tb.SetRowSecurity(ctx, true), tb.SetForceRowSecurity(ctx, true),
			p.Alter(ctx, pgmo.PolicyChanges{Roles: []string{""}, Using: new("true")}),
			p.SetComment(ctx, "c"), p.Rename(ctx, "p2")); err != nil {
			t.Fatal(err)
		}
		for what, mirrored := range map[string]bool{"RowSecurity": tb.RowSecurity, "ForceRowSecurity": tb.ForceRowSecurity,
			"policy name": p.Name == "p2", "policy comment": p.Comment == "c"} {
			if mirrored == scripting {
				t.Errorf("scripting %v: %s mirrored %v", scripting, what, mirrored)
			}
		}
		if !reflect.DeepEqual(p.Roles, []string{"r"}) || p.Using != "a > 0" {
			t.Errorf("Alter mirrored: %+v", p)
		}
		// Off again: mirrored back.
		if err := errors.Join(tb.SetRowSecurity(ctx, false), tb.SetForceRowSecurity(ctx, false)); err != nil {
			t.Fatal(err)
		}
		if tb.RowSecurity || tb.ForceRowSecurity {
			t.Errorf("scripting %v: still on", scripting)
		}
	}
	// The policy statements after a rename name the new name.
	var sawRenamed bool
	for _, q := range f.Database("app").Queries() {
		if q.Text == `COMMENT ON POLICY p ON app.t IS 'c'` {
			sawRenamed = true
		}
	}
	if !sawRenamed {
		t.Errorf("no comment on the policy: %v", f.Database("app").Queries())
	}
}

// An empty PolicyChanges sends nothing.
func TestPolicyAlterNothingSendsNothing(t *testing.T) {
	f := pgmotest.New(t)
	if err := f.Server().DatabaseRef("app").PolicyRef("app", "t", "p").Alter(t.Context(), pgmo.PolicyChanges{}); err != nil {
		t.Fatal(err)
	}
	wantSent(t, f)
	if q := f.Database("app").Queries(); len(q) != 0 {
		t.Errorf("sent %v", q)
	}
}

// CreatePolicy reads the policy back by name (every field, the table's
// names, the database); under WithScript it returns the PolicyRef, reading
// nothing; a read-back that finds nothing returns the handle.
func TestCreatePolicyReadsBack(t *testing.T) {
	f := pgmotest.New(t)
	d := f.Server().DatabaseRef("app")
	f.Database("app").On("FROM pg_catalog.pg_policy po", rowsOf(policyTypes,
		[]any{uint32(70001), "app", "t", "p", "r", true, []string{""}, "true", "", "c"}))
	req := pgmo.CreatePolicyRequest{Schema: "app", Table: "t", Name: "p", Command: pgmo.PolicySelect, Using: "true"}
	p, err := d.CreatePolicy(t.Context(), req)
	if err != nil {
		t.Fatal(err)
	}
	if p.Database() != d {
		t.Errorf("read-back's database %v", p.Database())
	}
	p.SetDatabase(nil)
	want := pgmo.Policy{OID: 70001, Schema: "app", Table: "t", Name: "p", Command: pgmo.PolicySelect, Permissive: true,
		Roles: []string{""}, Using: "true", Comment: "c"}
	if !reflect.DeepEqual(*p, want) {
		t.Errorf("read back %+v, want %+v", *p, want)
	}
	last := lastQuery(f.Database("app"))
	if !reflect.DeepEqual(last.Args, []any{"app", "t", "p"}) ||
		!strings.Contains(last.Text, "n.nspname = $1::pg_catalog.text AND c.relname = $2::pg_catalog.text AND po.polname = $3::pg_catalog.text") {
		t.Errorf("read back by %v:\n%s", last.Args, last.Text)
	}

	ctx, _ := pgmo.WithScript(t.Context())
	before := len(f.Database("app").Queries())
	p, err = d.CreatePolicy(ctx, req)
	if err != nil || p.OID != 0 || p.Name != "p" || p.Table != "t" || p.Schema != "app" || len(f.Database("app").Queries()) != before {
		t.Errorf("scripted: %+v, %v, read %v", p, err, f.Database("app").Queries()[before:])
	}

	f2 := pgmotest.New(t)
	f2.Database("app").On("FROM pg_catalog.pg_policy po", pgmotest.Rows())
	p, err = f2.Server().DatabaseRef("app").CreatePolicy(t.Context(), req)
	if err != nil || p.OID != 0 || p.Name != "p" {
		t.Errorf("read-back finding nothing: %+v, %v", p, err)
	}
}

// PolicyByName: schema required; none is ErrNotFound; the names bound.
func TestPolicyByName(t *testing.T) {
	f := pgmotest.New(t)
	d := f.Server().DatabaseRef("app")
	if _, err := d.PolicyByName(t.Context(), "", "t", "p"); !errors.Is(err, pgmo.ErrSchemaRequired) {
		t.Errorf("no schema: %v", err)
	}
	f.Database("app").On("FROM pg_catalog.pg_policy po", pgmotest.Rows())
	if _, err := d.PolicyByName(t.Context(), "app", "t", "p"); !errors.Is(err, pgmo.ErrNotFound) {
		t.Errorf("none: %v", err)
	}
}

// Each family's handle gives its own securable.
func TestHandleSecurables(t *testing.T) {
	f := pgmotest.New(t)
	s := f.Server()
	d := s.DatabaseRef("app")
	fn := d.FunctionRef("app", "f", []string{"integer"})
	for _, c := range []struct {
		got, want pgmo.Securable
	}{
		{d.Securable(), pgmo.Securable{Kind: pgmo.SecurableDatabase, Name: "app"}},
		{s.TablespaceRef("ts").Securable(), pgmo.Securable{Kind: pgmo.SecurableTablespace, Name: "ts"}},
		{d.SchemaRef("s").Securable(), pgmo.Securable{Kind: pgmo.SecurableSchema, Name: "s"}},
		{d.TableRef("s", "t").Securable(), pgmo.Securable{Kind: pgmo.SecurableTable, Schema: "s", Name: "t"}},
		{d.ViewRef("s", "v").Securable(), pgmo.Securable{Kind: pgmo.SecurableTable, Schema: "s", Name: "v"}},
		{d.MaterializedViewRef("s", "m").Securable(), pgmo.Securable{Kind: pgmo.SecurableTable, Schema: "s", Name: "m"}},
		{d.ForeignTableRef("s", "f").Securable(), pgmo.Securable{Kind: pgmo.SecurableTable, Schema: "s", Name: "f"}},
		{d.SequenceRef("s", "q").Securable(), pgmo.Securable{Kind: pgmo.SecurableSequence, Schema: "s", Name: "q"}},
		{fn.Securable(), pgmo.Securable{Kind: pgmo.SecurableFunction, Schema: "app", Name: "f", Arguments: []string{"integer"}}},
		{d.FunctionRef("app", "g", nil).Securable(), pgmo.Securable{Kind: pgmo.SecurableFunction, Schema: "app", Name: "g", Arguments: []string{}}},
		{d.ProcedureRef("app", "p", nil).Securable(), pgmo.Securable{Kind: pgmo.SecurableProcedure, Schema: "app", Name: "p", Arguments: []string{}}},
		{d.AggregateRef("app", "a", []string{"text"}).Securable(), pgmo.Securable{Kind: pgmo.SecurableFunction, Schema: "app", Name: "a", Arguments: []string{"text"}}},
		{d.TypeRef("s", "ty").Securable(), pgmo.Securable{Kind: pgmo.SecurableType, Schema: "s", Name: "ty"}},
		{d.DomainRef("s", "dm").Securable(), pgmo.Securable{Kind: pgmo.SecurableDomain, Schema: "s", Name: "dm"}},
		{d.LanguageRef("plpgsql").Securable(), pgmo.Securable{Kind: pgmo.SecurableLanguage, Name: "plpgsql"}},
		{d.ForeignDataWrapperRef("w").Securable(), pgmo.Securable{Kind: pgmo.SecurableForeignDataWrapper, Name: "w"}},
		{d.ForeignServerRef("srv").Securable(), pgmo.Securable{Kind: pgmo.SecurableForeignServer, Name: "srv"}},
	} {
		if !reflect.DeepEqual(c.got, c.want) {
			t.Errorf("Securable() = %+v, want %+v", c.got, c.want)
		}
	}
}
