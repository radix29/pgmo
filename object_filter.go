package pgmo

import (
	"fmt"
	"slices"
	"strings"

	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Object filters
// ============================================================
//
// object_filter.go narrows a catalog listing at the server, as gosmo's does.
// It is what an Object Explorer folder filter ("show me the tables whose
// name contains cust") is built on: without it a schema of ten thousand
// tables is read whole and filtered by the caller, which is exactly the
// folder where that hurts.
//
// Where gosmo compares LOWER() of both sides — SQL Server names compare
// case-insensitively — pgmo matches names exactly (names.go) and folds the
// criterion's text instead, the way psql's \d patterns do: what a person
// types is read as PostgreSQL reads an identifier (pgsql.FoldIdent), so cust
// and CUST both find customers, and "Cust" finds Customers alone. A filter
// therefore never answers with an object the same text, typed into a
// statement, would not name — and it needs no lower()/ILIKE, which would
// fold only ASCII under the catalog's "C" collation anyway.
//
// The comparisons are pg_catalog.strpos and =, not LIKE: a name holding %,
// _ or \ is then matched literally with no escaping to get wrong (LIKE's
// escape character is a backslash, and an ESCAPE clause's literal depends on
// standard_conforming_strings). The value is compared as text, never as
// name, so a value over 63 bytes is not truncated into one that matches.
//
// PostgreSQL has no creation date in its catalog, so gosmo's Created
// criterion does not carry over; nor does MemoryOptimized.

// TextOp is one comparison a text criterion makes.
type TextOp int

const (
	TextContains TextOp = iota
	TextNotContains
	TextEquals
	TextNotEquals
)

// String names the operator, for a message.
func (op TextOp) String() string {
	switch op {
	case TextContains:
		return "contains"
	case TextNotContains:
		return "does not contain"
	case TextEquals:
		return "equals"
	case TextNotEquals:
		return "does not equal"
	}
	return fmt.Sprintf("TextOp(%d)", int(op))
}

// TextCriterion is one comparison against a name or a schema name.
//
// Value is text as a person types it, folded with pgsql.FoldIdent before
// the comparison: surrounding white space is dropped, a whole "quoted" value
// keeps its case (and loses the quotes), and anything else has ASCII A–Z
// lowered. A caller holding a stored name — from the catalog or a tree
// node — passes pgsql.QuoteIdent(name), which folds back to the name
// exactly. A Value empty once folded is no criterion.
type TextCriterion struct {
	Op    TextOp
	Value string
}

// ObjectFilter narrows a catalog listing. Every criterion narrows it further
// — they are AND-ed, never OR-ed — and a zero ObjectFilter narrows nothing,
// so the unfiltered listing is the same call with an empty one.
type ObjectFilter struct {
	Name   []TextCriterion
	Schema []TextCriterion
	// System, when set, keeps only system objects (true) or only user
	// objects (false); nil keeps both. An object is a system one when its
	// schema is: information_schema, or any schema named pg_* (pg_catalog,
	// pg_toast, and the pg_temp_N / pg_toast_temp_N of every session) — the
	// server reserves that prefix, so no user schema has it. An extension
	// member is not a system object for that (system_objects.go), only for
	// being in such a schema. A family with no such notion ignores it, as it
	// ignores a criterion on a column it lacks.
	System *bool
	// ExtensionMember, when set, keeps only the objects an extension owns
	// (true) or only those none does (false); nil keeps both. It is
	// Origin.Extension being set: pg_depend deptype 'e', not 'x'.
	ExtensionMember *bool
}

// Empty reports whether f narrows anything at all. A criterion whose Value
// folds to nothing counts as none.
func (f ObjectFilter) Empty() bool {
	return !hasCriterion(f.Name) && !hasCriterion(f.Schema) && f.System == nil && f.ExtensionMember == nil
}

// hasCriterion reports whether any of cs compares against something.
func hasCriterion(cs []TextCriterion) bool {
	for _, c := range cs {
		if pgsql.FoldIdent(c.Value) != "" {
			return true
		}
	}
	return false
}

// filterColumns names the SQL expression each criterion compares against, per
// family — a relation listing's name is c.relname and its schema n.nspname;
// the schema listing's name and schema are both n.nspname. An empty
// expression means this family has no such column, and criteria against it
// are dropped. system is a boolean expression true for a system object;
// systemSchema builds the usual one. extension is one true for an extension
// member; isExtensionMember builds it.
type filterColumns struct {
	name      string
	schema    string
	system    string
	extension string
}

// systemSchema is the SQL condition that the schema named by the expression
// nspname is a system one (ObjectFilter.System): information_schema, or a
// name starting pg_, a prefix the server refuses to CREATE SCHEMA with
// (42939) — checked on pg15 and pg18.
func systemSchema(nspname string) string {
	return fmt.Sprintf("(pg_catalog.starts_with(%s, 'pg_') OR %s = 'information_schema')", nspname, nspname)
}

// clause builds the SQL fragment and the parameters for f against one
// family's columns. The fragment is a series of " AND …" terms, so it
// appends to a query that already has a WHERE. args are the query's own
// parameters: the filter's are appended to them and numbered after them
// ($len(args)+1 on), and the whole list is returned to pass to the read.
//
// An unknown TextOp is refused (ErrInvalidRequest) rather than read as one
// of the others: a filter that silently means something else returns the
// wrong rows with no sign of it.
func (f ObjectFilter) clause(cols filterColumns, args []any) (string, []any, error) {
	var b strings.Builder
	args = slices.Clip(args) // appending never writes into the caller's array
	arg := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	text := func(what, col string, criteria []TextCriterion) error {
		for _, c := range criteria {
			// Checked before anything is skipped, so a filter is refused
			// alike by every family it is handed to.
			if c.Op < TextContains || c.Op > TextNotEquals {
				return invalidf("pgmo: object filter: %s: unknown text operator %s", what, c.Op)
			}
			v := pgsql.FoldIdent(c.Value)
			if v == "" || col == "" {
				continue
			}
			switch c.Op {
			case TextContains:
				fmt.Fprintf(&b, " AND pg_catalog.strpos(%s, %s) > 0", col, arg(v))
			case TextNotContains:
				fmt.Fprintf(&b, " AND pg_catalog.strpos(%s, %s) = 0", col, arg(v))
			case TextEquals:
				fmt.Fprintf(&b, " AND %s = %s::pg_catalog.text", col, arg(v))
			case TextNotEquals:
				fmt.Fprintf(&b, " AND %s <> %s::pg_catalog.text", col, arg(v))
			}
		}
		return nil
	}
	if err := text("name", cols.name, f.Name); err != nil {
		return "", nil, err
	}
	if err := text("schema", cols.schema, f.Schema); err != nil {
		return "", nil, err
	}

	flag := func(want *bool, cond string) {
		if want == nil || cond == "" {
			return
		}
		if *want {
			fmt.Fprintf(&b, " AND %s", cond)
		} else {
			fmt.Fprintf(&b, " AND NOT %s", cond)
		}
	}
	flag(f.System, cols.system)
	flag(f.ExtensionMember, cols.extension)
	return b.String(), args, nil
}
