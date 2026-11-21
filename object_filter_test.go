package pgmo

import (
	"errors"
	"reflect"
	"strings"
	"testing"
)

// testFilterCols stands in for one family's column mapping.
var testFilterCols = filterColumns{
	name:      "c.relname",
	schema:    "n.nspname",
	system:    systemSchema("n.nspname"),
	extension: "x(c.oid)",
}

// The clause is what makes a pushed-down filter equal the caller-side one, so
// each operator's SQL and its parameters are asserted in full rather than by
// substring: a comparison that lost its fold, or a LIKE that crept back in,
// still runs and returns rows — the wrong ones.
func TestObjectFilterClause(t *testing.T) {
	yes, no := true, false
	for _, c := range []struct {
		name     string
		filter   ObjectFilter
		wantSQL  string
		wantArgs []any
	}{
		{
			name:    "an empty filter narrows nothing",
			filter:  ObjectFilter{},
			wantSQL: "",
		},
		{
			name:     "contains",
			filter:   ObjectFilter{Name: []TextCriterion{{Op: TextContains, Value: "cust"}}},
			wantSQL:  " AND pg_catalog.strpos(c.relname, $1) > 0",
			wantArgs: []any{"cust"},
		},
		{
			name:     "does not contain",
			filter:   ObjectFilter{Name: []TextCriterion{{Op: TextNotContains, Value: "tmp"}}},
			wantSQL:  " AND pg_catalog.strpos(c.relname, $1) = 0",
			wantArgs: []any{"tmp"},
		},
		{
			name:     "equals is compared as text, never truncated to a name",
			filter:   ObjectFilter{Name: []TextCriterion{{Op: TextEquals, Value: "orders"}}},
			wantSQL:  " AND c.relname = $1::pg_catalog.text",
			wantArgs: []any{"orders"},
		},
		{
			name:     "does not equal",
			filter:   ObjectFilter{Name: []TextCriterion{{Op: TextNotEquals, Value: "orders"}}},
			wantSQL:  " AND c.relname <> $1::pg_catalog.text",
			wantArgs: []any{"orders"},
		},
		{
			// Typed text is read as the server reads an identifier.
			name:     "bare text is folded",
			filter:   ObjectFilter{Name: []TextCriterion{{Op: TextEquals, Value: "  Orders "}}},
			wantSQL:  " AND c.relname = $1::pg_catalog.text",
			wantArgs: []any{"orders"},
		},
		{
			name:     "quoted text keeps its case",
			filter:   ObjectFilter{Name: []TextCriterion{{Op: TextContains, Value: `"Cust"`}}},
			wantSQL:  " AND pg_catalog.strpos(c.relname, $1) > 0",
			wantArgs: []any{"Cust"},
		},
		{
			name:     "only ASCII is folded",
			filter:   ObjectFilter{Name: []TextCriterion{{Op: TextContains, Value: "ÄB"}}},
			wantSQL:  " AND pg_catalog.strpos(c.relname, $1) > 0",
			wantArgs: []any{"Äb"},
		},
		{
			// strpos, not LIKE: nothing in the value is pattern syntax.
			name:     "wildcard characters are passed as they are",
			filter:   ObjectFilter{Name: []TextCriterion{{Op: TextContains, Value: `pct_100%\x`}}},
			wantSQL:  " AND pg_catalog.strpos(c.relname, $1) > 0",
			wantArgs: []any{`pct_100%\x`},
		},
		{
			name: "criteria are AND-ed, name before schema",
			filter: ObjectFilter{
				Schema: []TextCriterion{{Op: TextEquals, Value: "sales"}},
				Name:   []TextCriterion{{Op: TextContains, Value: "a"}, {Op: TextNotContains, Value: "b"}},
			},
			wantSQL:  " AND pg_catalog.strpos(c.relname, $1) > 0 AND pg_catalog.strpos(c.relname, $2) = 0 AND n.nspname = $3::pg_catalog.text",
			wantArgs: []any{"a", "b", "sales"},
		},
		{
			name:    "system objects only",
			filter:  ObjectFilter{System: &yes},
			wantSQL: " AND (pg_catalog.starts_with(n.nspname, 'pg_') OR n.nspname = 'information_schema')",
		},
		{
			name:    "user objects only",
			filter:  ObjectFilter{System: &no},
			wantSQL: " AND NOT (pg_catalog.starts_with(n.nspname, 'pg_') OR n.nspname = 'information_schema')",
		},
		{
			name:    "extension members only",
			filter:  ObjectFilter{ExtensionMember: &yes},
			wantSQL: " AND x(c.oid)",
		},
		{
			name:    "no extension members",
			filter:  ObjectFilter{ExtensionMember: &no},
			wantSQL: " AND NOT x(c.oid)",
		},
		{
			// The two flags are independent: a user object no extension owns.
			name:    "system and extension, system first",
			filter:  ObjectFilter{ExtensionMember: &no, System: &no},
			wantSQL: " AND NOT (pg_catalog.starts_with(n.nspname, 'pg_') OR n.nspname = 'information_schema') AND NOT x(c.oid)",
		},
		{
			name:    "an empty value is no criterion",
			filter:  ObjectFilter{Name: []TextCriterion{{Op: TextContains, Value: ""}}},
			wantSQL: "",
		},
		{
			name:    "a value folding to nothing is no criterion",
			filter:  ObjectFilter{Name: []TextCriterion{{Op: TextContains, Value: " \t"}}, Schema: []TextCriterion{{Value: `""`}}},
			wantSQL: "",
		},
	} {
		t.Run(c.name, func(t *testing.T) {
			sql, args, err := c.filter.clause(testFilterCols, nil)
			if err != nil {
				t.Fatal(err)
			}
			if sql != c.wantSQL {
				t.Errorf("sql\n got: %q\nwant: %q", sql, c.wantSQL)
			}
			if len(args) != 0 || len(c.wantArgs) != 0 {
				if !reflect.DeepEqual(args, c.wantArgs) {
					t.Errorf("args = %#v, want %#v", args, c.wantArgs)
				}
			}
		})
	}
}

// The filter's parameters follow the query's own, numbered after them, and
// appending them never writes into the caller's slice.
func TestObjectFilterClauseNumbersAfterTheQuerysArgs(t *testing.T) {
	own := make([]any, 2, 8)
	own[0], own[1] = "r", "v"
	f := ObjectFilter{Name: []TextCriterion{{Op: TextEquals, Value: "x"}}, Schema: []TextCriterion{{Op: TextContains, Value: "y"}}}
	sql, args, err := f.clause(testFilterCols, own)
	if err != nil {
		t.Fatal(err)
	}
	if want := " AND c.relname = $3::pg_catalog.text AND pg_catalog.strpos(n.nspname, $4) > 0"; sql != want {
		t.Errorf("sql\n got: %q\nwant: %q", sql, want)
	}
	if want := []any{"r", "v", "x", "y"}; !reflect.DeepEqual(args, want) {
		t.Errorf("args = %#v, want %#v", args, want)
	}
	if spare := own[:3]; spare[2] != nil {
		t.Errorf("clause wrote %#v into the caller's array", spare[2])
	}
}

// A family lacking a column drops the criteria against it; the rest still
// apply.
func TestObjectFilterClauseDropsWhatTheFamilyLacks(t *testing.T) {
	yes := true
	f := ObjectFilter{
		Name:            []TextCriterion{{Op: TextContains, Value: "a"}},
		Schema:          []TextCriterion{{Op: TextContains, Value: "s"}},
		System:          &yes,
		ExtensionMember: &yes,
	}
	sql, args, err := f.clause(filterColumns{name: "d.datname"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if want := " AND pg_catalog.strpos(d.datname, $1) > 0"; sql != want {
		t.Errorf("sql = %q, want %q", sql, want)
	}
	if want := []any{"a"}; !reflect.DeepEqual(args, want) {
		t.Errorf("args = %#v, want %#v", args, want)
	}
}

// An operator outside the four is refused, naming the criterion — also on a
// column the family lacks and with an empty value, so every family refuses
// the same filter.
func TestObjectFilterClauseRefusesAnUnknownOperator(t *testing.T) {
	for _, c := range []struct {
		name   string
		filter ObjectFilter
		cols   filterColumns
		want   string
	}{
		{"name", ObjectFilter{Name: []TextCriterion{{Op: TextNotEquals + 1, Value: "x"}}}, testFilterCols, "name: unknown text operator TextOp(4)"},
		{"negative", ObjectFilter{Name: []TextCriterion{{Op: -1, Value: "x"}}}, testFilterCols, "TextOp(-1)"},
		{"schema", ObjectFilter{Schema: []TextCriterion{{Op: 9, Value: "x"}}}, testFilterCols, "schema: unknown"},
		{"a column the family lacks", ObjectFilter{Schema: []TextCriterion{{Op: 9, Value: "x"}}}, filterColumns{name: "d.datname"}, "schema: unknown"},
		{"an empty value", ObjectFilter{Name: []TextCriterion{{Op: 9}}}, testFilterCols, "name: unknown"},
	} {
		t.Run(c.name, func(t *testing.T) {
			sql, args, err := c.filter.clause(c.cols, nil)
			if !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("err = %v, want ErrInvalidRequest", err)
			}
			if !strings.HasPrefix(err.Error(), "pgmo: object filter: ") || !strings.Contains(err.Error(), c.want) {
				t.Errorf("err = %q, want it to name %q", err, c.want)
			}
			if sql != "" || args != nil {
				t.Errorf("refused, yet returned %q %#v", sql, args)
			}
		})
	}
}

func TestObjectFilterEmpty(t *testing.T) {
	yes := true
	for _, c := range []struct {
		name   string
		filter ObjectFilter
		want   bool
	}{
		{"zero", ObjectFilter{}, true},
		{"a name", ObjectFilter{Name: []TextCriterion{{Value: "x"}}}, false},
		{"a schema", ObjectFilter{Schema: []TextCriterion{{Value: "x"}}}, false},
		{"system", ObjectFilter{System: &yes}, false},
		{"extension member", ObjectFilter{ExtensionMember: &yes}, false},
		{"only blank values", ObjectFilter{Name: []TextCriterion{{Value: " "}}, Schema: []TextCriterion{{Value: `""`}}}, true},
		{"a blank value and a real one", ObjectFilter{Name: []TextCriterion{{Value: ""}, {Value: "x"}}}, false},
	} {
		if got := c.filter.Empty(); got != c.want {
			t.Errorf("%s: Empty() = %v, want %v", c.name, got, c.want)
		}
	}
}

func TestTextOpString(t *testing.T) {
	for op, want := range map[TextOp]string{
		TextContains: "contains", TextNotContains: "does not contain",
		TextEquals: "equals", TextNotEquals: "does not equal", 7: "TextOp(7)",
	} {
		if got := op.String(); got != want {
			t.Errorf("%d.String() = %q, want %q", int(op), got, want)
		}
	}
}
