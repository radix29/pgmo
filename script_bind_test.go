package pgmo

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

func TestBindScriptArgs(t *testing.T) {
	for _, tc := range []struct {
		q    string
		args []any
		want string
	}{
		{`SELECT 1`, nil, `SELECT 1`},
		{`SELECT $1, $2`, []any{"a", int32(2)}, `SELECT 'a', 2`},
		{`SELECT $2, $1, $1`, []any{"x", true}, `SELECT true, 'x', 'x'`},
		{`SELECT $10, $1, $2, $3, $4, $5, $6, $7, $8, $9`, []any{1, 2, 3, 4, 5, 6, 7, 8, 9, "ten"}, `SELECT 'ten', 1, 2, 3, 4, 5, 6, 7, 8, 9`},
		// A $1 the lexer does not read as a parameter is left alone.
		{`SELECT '$1', "$1", $1 -- $1` + "\n/* $1 */", []any{"v"}, `SELECT '$1', "$1", 'v' -- $1` + "\n/* $1 */"},
		{`CREATE FUNCTION f(int) RETURNS int AS $$ SELECT $1 $$ LANGUAGE sql; SELECT $1`, []any{7}, `CREATE FUNCTION f(int) RETURNS int AS $$ SELECT $1 $$ LANGUAGE sql; SELECT 7`},
		{`SELECT a$1, $1`, []any{"v"}, `SELECT a$1, 'v'`},
		// A negative cannot join the operator before it.
		{`SELECT x - $1, x !=$1`, []any{-1}, `SELECT x - (-1), x !=(-1)`},
		{`SELECT x-$1`, []any{-1.5}, `SELECT x-(-1.5)`},
		// A quote after U& does not become a Unicode-escape literal.
		{`SELECT U&$1`, []any{"d"}, `SELECT U& 'd'`},
		{`SELECT x&$1`, []any{5}, `SELECT x&5`},
	} {
		got, err := bindScriptArgs(tc.q, tc.args)
		if err != nil || got != tc.want {
			t.Errorf("bindScriptArgs(%q, %v) = %q, %v; want %q", tc.q, tc.args, got, err, tc.want)
		}
	}
}

func TestBindScriptArgsRefusals(t *testing.T) {
	for _, tc := range []struct {
		q    string
		args []any
		kind error
		msg  string
	}{
		{`SELECT $1`, nil, ErrInvalidRequest, "script: $1 has no argument (0 given)"},
		{`SELECT $0`, []any{1}, ErrInvalidRequest, "script: $0 has no argument (1 given)"},
		{`SELECT $1`, []any{1, 2}, ErrInvalidRequest, "script: argument 2 is bound to no $2 in the statement"},
		// Only inside a literal: not a parameter, so the argument is unused.
		{`SELECT '$1'`, []any{1}, ErrInvalidRequest, "script: argument 1 is bound to no $1"},
		{`SELECT $1`, []any{map[string]int{}}, ErrUnsupported, "script: $1: cannot script a map[string]int argument"},
		{`SELECT $1`, []any{time.Date(0, 1, 1, 0, 0, 0, 0, time.UTC)}, ErrUnsupported, "script: $1: cannot script the year 0"},
		{`SELECT $1`, []any{badValuer{}}, ErrUnsupported, "script: $1: valuer failed"},
	} {
		_, err := bindScriptArgs(tc.q, tc.args)
		if !errors.Is(err, tc.kind) || err == nil || !strings.HasPrefix(err.Error(), tc.msg) {
			t.Errorf("bindScriptArgs(%q, %v) error %v, want %v starting %q", tc.q, tc.args, err, tc.kind, tc.msg)
		}
	}
}

type badValuer struct{}

func (badValuer) Value() (driver.Value, error) { return nil, errors.New("valuer failed") }

func TestScriptLiteral(t *testing.T) {
	s := "p"
	var nilStr *string
	var nilNull *sql.NullString
	for _, tc := range []struct {
		v    any
		want string
	}{
		{nil, "NULL"},
		{"", "''"},
		{"it's", "'it''s'"},
		{`a\b`, `E'a\\b'`},
		{[]byte(nil), "NULL"},
		{[]byte{}, `E'\\x'`},
		{[]byte{0, 0xab}, `E'\\x00ab'`},
		{[]string(nil), "NULL"},
		{[]string{}, "'{}'"},
		{[]string{"a", `b"c`, `d\e`, "NULL", "x,y", "it's"}, `E'{"a","b\\"c","d\\\\e","NULL","x,y","it''s"}'`},
		{true, "true"},
		{false, "false"},
		{int(-7), "(-7)"},
		{int8(-8), "(-8)"},
		{int16(16), "16"},
		{int32(math.MinInt32), "(-2147483648)"},
		{int64(math.MaxInt64), "9223372036854775807"},
		{uint(1), "1"},
		{uint8(8), "8"},
		{uint16(16), "16"},
		{uint32(32), "32"},
		{uint64(math.MaxUint64), "18446744073709551615"},
		{float32(0.1), "0.1"},
		{0.1, "0.1"},
		{1e300, "1e+300"},
		{-2.5, "(-2.5)"},
		{math.Copysign(0, -1), "'-0'::double precision"},
		{float32(math.Copysign(0, -1)), "'-0'::real"},
		{math.NaN(), "'NaN'::double precision"},
		{math.Inf(1), "'Infinity'::double precision"},
		{float32(math.Inf(-1)), "'-Infinity'::real"},
		{time.Date(2026, 11, 21, 10, 30, 0, 123456789, time.FixedZone("x", 3600)), "'2026-11-21 10:30:00.123456+01:00:00'"},
		{time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC), "'2026-01-02 03:04:05+00:00:00'"},
		{&s, "'p'"},
		{nilStr, "NULL"},
		{sql.NullString{String: "v", Valid: true}, "'v'"},
		{sql.NullString{}, "NULL"},
		{sql.NullInt64{Int64: -1, Valid: true}, "(-1)"},
		{nilNull, "NULL"},
	} {
		got, err := scriptLiteral(tc.v)
		if err != nil || got != tc.want {
			t.Errorf("scriptLiteral(%#v) = %q, %v; want %q", tc.v, got, err, tc.want)
		}
	}
}

func TestSetIfApplied(t *testing.T) {
	scripted, _ := WithScript(t.Context())
	name := "old"
	setIfApplied(scripted, &name, "new")
	if name != "old" {
		t.Errorf("under WithScript: %q, want old", name)
	}
	setIfApplied(t.Context(), &name, "new")
	if name != "new" {
		t.Errorf("outside WithScript: %q, want new", name)
	}
	n := 1
	setPtrIfApplied(t.Context(), &n, nil)
	if n != 1 {
		t.Errorf("nil change applied: %d", n)
	}
	two := 2
	setPtrIfApplied(scripted, &n, &two)
	setPtrIfApplied(t.Context(), &n, &two)
	if n != 2 {
		t.Errorf("change: %d, want 2", n)
	}
}

func TestCreatedObject(t *testing.T) {
	scripted, _ := WithScript(t.Context())
	read := func(v string, err error) func() (string, error) {
		return func() (string, error) { return v, err }
	}
	failing := errors.New("read failed")
	for _, tc := range []struct {
		ctx  context.Context
		read func() (string, error)
		want string
		err  error
	}{
		{scripted, func() (string, error) { t.Error("read under WithScript"); return "", nil }, "handle", nil},
		{t.Context(), read("read", nil), "read", nil},
		{t.Context(), read("", notFoundf("pgmo: x: not there")), "handle", nil},
		{t.Context(), read("", invalidf("pgmo: x: %w", ErrHandleNotLoaded)), "handle", nil},
		{t.Context(), read("", failing), "", failing},
	} {
		got, err := createdObject(tc.ctx, "handle", tc.read)
		if got != tc.want || !errors.Is(err, tc.err) || (tc.err == nil) != (err == nil) {
			t.Errorf("createdObject = %q, %v; want %q, %v", got, err, tc.want, tc.err)
		}
	}
}
