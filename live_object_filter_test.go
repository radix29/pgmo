//go:build livedb

package pgmo

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/radix29/pgmo/pgsql"
)

// TestLiveObjectFilter runs ObjectFilter.clause on the server, appended to a
// relation listing that binds a parameter of its own, over tables whose names
// differ only by case or hold %, _, \ and quotes: each criterion keeps
// exactly the tables the folded text names, and System splits the catalog's
// schemas from the user's.
func TestLiveObjectFilter(t *testing.T) {
	admin, ctx, done := liveDB(t)
	t.Cleanup(done) // registered first, so it runs after the drop
	schema := fmt.Sprintf("pgmo_live_filter_%d", time.Now().UnixNano())
	S := pgsql.QuoteIdent(schema)
	t.Cleanup(func() {
		if _, err := admin.Exec(ctx, "DROP SCHEMA IF EXISTS "+S+" CASCADE"); err != nil {
			t.Errorf("drop schema: %v", err)
		}
	})
	if _, err := admin.Exec(ctx, "CREATE SCHEMA "+S); err != nil {
		t.Fatal(err)
	}
	long := strings.Repeat("l", 63) // the longest name; a longer value must not be truncated onto it
	for _, name := range []string{"orders", "Orders", "ORDERS", "customers", "Cust_100%", `a\b`, `x"y`, "Äbc", "x1y", long} {
		if _, err := admin.Exec(ctx, "CREATE TABLE "+pgsql.QualifiedName(schema, name)+" ()"); err != nil {
			t.Fatalf("create %q: %v", name, err)
		}
	}
	if _, err := admin.Exec(ctx, "CREATE VIEW "+pgsql.QualifiedName(schema, "orders_v")+" AS SELECT 1 AS one"); err != nil {
		t.Fatal(err)
	}

	list := func(t *testing.T, f ObjectFilter) [][2]string {
		t.Helper()
		where, args, err := f.clause(filterColumns{name: "c.relname", schema: "n.nspname", system: systemSchema("n.nspname")}, []any{"r"})
		if err != nil {
			t.Fatal(err)
		}
		rows, err := scanRows(ctx, admin, "list relations", func(scan func(...any) error) ([2]string, error) {
			var r [2]string
			return r, scan(&r[0], &r[1])
		}, `SELECT n.nspname, c.relname FROM pg_catalog.pg_class c
			JOIN pg_catalog.pg_namespace n ON n.oid = c.relnamespace
			WHERE c.relkind = $1`+where+` ORDER BY n.nspname, c.relname`, args...)
		if err != nil {
			t.Fatalf("%s: %v", where, err)
		}
		return rows
	}
	inSchema := TextCriterion{Op: TextEquals, Value: pgsql.QuoteIdent(schema)}

	for _, c := range []struct {
		name string
		crit []TextCriterion
		want []string // table names in the schema, in byte order (relname's collation is "C")
	}{
		{"bare text is folded", []TextCriterion{{Op: TextContains, Value: "ORD"}}, []string{"orders"}},
		{"quoted text keeps its case", []TextCriterion{{Op: TextContains, Value: `"Ord"`}}, []string{"Orders"}},
		{"equals a quoted name", []TextCriterion{{Op: TextEquals, Value: `"ORDERS"`}}, []string{"ORDERS"}},
		{"equals a bare name", []TextCriterion{{Op: TextEquals, Value: "Orders"}}, []string{"orders"}},
		{"does not equal", []TextCriterion{{Op: TextNotEquals, Value: "orders"}, {Op: TextNotContains, Value: "l"}},
			[]string{"Cust_100%", "ORDERS", "Orders", `a\b`, "customers", `x"y`, "x1y", "Äbc"}},
		{"% is literal", []TextCriterion{{Op: TextContains, Value: "%"}}, []string{"Cust_100%"}},
		{"_ is literal", []TextCriterion{{Op: TextContains, Value: "_"}}, []string{"Cust_100%"}},
		{`\ is literal`, []TextCriterion{{Op: TextContains, Value: `\`}}, []string{`a\b`}},
		{"a quote", []TextCriterion{{Op: TextContains, Value: `"x""y"`}}, []string{`x"y`}},
		{"only ASCII is folded", []TextCriterion{{Op: TextContains, Value: "äbc"}}, nil},
		{"non-ASCII kept", []TextCriterion{{Op: TextContains, Value: "Äb"}}, []string{"Äbc"}},
		{"does not contain, AND-ed", []TextCriterion{{Op: TextContains, Value: "s"}, {Op: TextNotContains, Value: "Cust"}}, []string{"Cust_100%", "Orders", "orders"}},
		{"a value over 63 bytes is not truncated", []TextCriterion{{Op: TextEquals, Value: long + "x"}}, nil},
		{"the 63-byte name itself", []TextCriterion{{Op: TextEquals, Value: long}}, []string{long}},
	} {
		t.Run(c.name, func(t *testing.T) {
			var got []string
			for _, r := range list(t, ObjectFilter{Schema: []TextCriterion{inSchema}, Name: c.crit}) {
				if r[0] != schema {
					t.Errorf("listed %s.%s outside the schema", r[0], r[1])
				}
				got = append(got, r[1])
			}
			if !slices.Equal(got, c.want) {
				t.Errorf("got %q, want %q", got, c.want)
			}
		})
	}

	t.Run("System", func(t *testing.T) {
		yes, no := true, false
		isSystem := func(nsp string) bool { return strings.HasPrefix(nsp, "pg_") || nsp == "information_schema" }
		all := list(t, ObjectFilter{})
		sys := list(t, ObjectFilter{System: &yes})
		user := list(t, ObjectFilter{System: &no})
		if len(sys) == 0 || len(user) == 0 {
			t.Fatalf("%d system and %d user tables; want some of each", len(sys), len(user))
		}
		if len(sys)+len(user) != len(all) {
			t.Errorf("%d system + %d user tables, but %d in all", len(sys), len(user), len(all))
		}
		for _, r := range sys {
			if !isSystem(r[0]) {
				t.Errorf("System: true listed %s.%s", r[0], r[1])
			}
		}
		for _, r := range user {
			if isSystem(r[0]) {
				t.Errorf("System: false listed %s.%s", r[0], r[1])
			}
		}
		if !slices.Contains(sys, [2]string{"pg_catalog", "pg_class"}) {
			t.Error("System: true does not list pg_catalog.pg_class")
		}
		if !slices.Contains(user, [2]string{schema, "orders"}) {
			t.Errorf("System: false does not list %s.orders", schema)
		}
		got := list(t, ObjectFilter{System: &no, Schema: []TextCriterion{inSchema}, Name: []TextCriterion{{Op: TextEquals, Value: "customers"}}})
		if want := [][2]string{{schema, "customers"}}; !slices.Equal(got, want) {
			t.Errorf("System with criteria: got %q, want %q", got, want)
		}
	})
}
