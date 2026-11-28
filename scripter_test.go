package pgmo_test

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/radix29/pgmo"
	"github.com/radix29/pgmo/pgmotest"
)

// statements are the texts f was sent, in order.
func statements(f *pgmotest.Fake) []string {
	var out []string
	for _, q := range f.Queries() {
		out = append(out, q.Text)
	}
	return out
}

// A script reads its object in a transaction of its own on the object's
// database: BEGIN, the settings pinned (search_path bound as $1, never
// written into the text), the read inside the transaction, ROLLBACK. The
// schema's script restates its owner, comment, ACL from the kind's default
// and the default privileges set in it.
func TestScriptSchemaReadsUnderFixedSettings(t *testing.T) {
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On("FROM pg_catalog.pg_namespace n", schemaRows(rowSchemaApp))
	sc := pgmo.NewScripter(f.Server().DatabaseRef("appdb"), pgmo.DefaultScriptOptions())

	got, err := sc.ScriptSchema(t.Context(), "app")
	if err != nil {
		t.Fatal(err)
	}
	want := `CREATE SCHEMA app;

ALTER SCHEMA app OWNER TO app;

COMMENT ON SCHEMA app IS 'the app';

GRANT USAGE ON SCHEMA app TO "B b" WITH GRANT OPTION;

GRANT CREATE ON SCHEMA app TO PUBLIC;

ALTER DEFAULT PRIVILEGES FOR ROLE "B b" IN SCHEMA app GRANT USAGE ON TYPES TO app WITH GRANT OPTION;

ALTER DEFAULT PRIVILEGES FOR ROLE app IN SCHEMA app GRANT USAGE ON TYPES TO "B b";

ALTER DEFAULT PRIVILEGES FOR ROLE app IN SCHEMA app GRANT SELECT ON TABLES TO "B b";

ALTER DEFAULT PRIVILEGES FOR ROLE app IN SCHEMA app GRANT SELECT ON TABLES TO PUBLIC;
`
	if got != want {
		t.Errorf("script =\n%s\nwant\n%s", got, want)
	}
	qs := other.Queries()
	if len(qs) != 4 || qs[0].Text != "begin" || qs[3].Text != "rollback" {
		t.Fatalf("sent %q", statements(other))
	}
	set, read := qs[1], qs[2]
	if set.Kind != pgmotest.KindExec || set.Tx != qs[0].Tx || set.Arg(0) != "pg_catalog, pg_temp" {
		t.Errorf("settings = %+v", set)
	}
	for _, w := range []string{"pg_catalog.set_config('search_path', $1, true)", "'standard_conforming_strings', 'on'",
		"'DateStyle', 'ISO'", "'IntervalStyle', 'postgres'", "'extra_float_digits', '3'"} {
		if !strings.Contains(set.Text, w) {
			t.Errorf("no %q in %s", w, set.Text)
		}
	}
	if read.Tx != qs[0].Tx || read.Tx == 0 {
		t.Errorf("the read ran in tx %d, the script's is %d", read.Tx, qs[0].Tx)
	}
	if len(f.Queries()) != 1 { // the version read only: nothing on the Server's own database
		t.Errorf("sent on the Server's database: %q", statements(f))
	}

	// DROP alone reads the object too (the verb's statement names it as
	// read), and writes the DROP with its modifiers.
	drop := pgmo.NewScripter(f.Server().DatabaseRef("appdb"), pgmo.ScriptOptions{Verb: pgmo.ScriptDrop, IfExists: true, Cascade: true})
	if got, err := drop.ScriptSchema(t.Context(), "app"); err != nil || got != "DROP SCHEMA IF EXISTS app CASCADE;\n" {
		t.Errorf("DROP = %q, %v", got, err)
	}
}

// A failed read ends the script's transaction and returns the read's
// error, unwrapped once more by nobody: the read named the operation.
func TestScriptReadFailureRollsBack(t *testing.T) {
	f := pgmotest.New(t)
	other := f.Database("appdb")
	other.On("FROM pg_catalog.pg_namespace n", schemaRows())
	sc := pgmo.NewScripter(f.Server().DatabaseRef("appdb"), pgmo.DefaultScriptOptions())
	if _, err := sc.ScriptSchema(t.Context(), "gone"); !errors.Is(err, pgmo.ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
	if s := statements(other); len(s) != 4 || s[3] != "rollback" {
		t.Errorf("sent %q", s)
	}
}

// Inside the caller's InTransaction on the database, the script's reads
// are a savepoint of it, rolled back to after; another database is
// refused, as any read of it there.
func TestScriptInsideInTransaction(t *testing.T) {
	f := pgmotest.New(t)
	f.On("FROM pg_catalog.pg_namespace n", schemaRows(rowSchemaApp))
	s := f.Server()
	sc := pgmo.NewScripter(s.DatabaseRef(pgmotest.DefaultDatabase), pgmo.DefaultScriptOptions())
	other := pgmo.NewScripter(s.DatabaseRef("appdb"), pgmo.DefaultScriptOptions())
	err := s.InTransaction(t.Context(), func(ctx context.Context) error {
		if _, err := sc.ScriptSchema(ctx, "app"); err != nil {
			return err
		}
		if _, err := other.ScriptSchema(ctx, "app"); !errors.Is(err, pgmo.ErrUnsupported) {
			t.Errorf("another database: %v, want ErrUnsupported", err)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	got := statements(f)
	want := []string{"begin", "savepoint sp_1", got[3], got[4], "rollback to savepoint sp_1", "commit"}
	if len(got) != 7 || !slices.Equal(got[1:], want) {
		t.Errorf("sent %q", got)
	}
}
