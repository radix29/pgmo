package pgmo

import (
	"context"
	"database/sql/driver"
	"encoding/hex"
	"errors"
	"math"
	"reflect"
	"slices"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Script mode: writes collected instead of run
// ============================================================

type scriptCtxKey struct{}

// ScriptEntry is one statement a ScriptCollector captured, with where it
// would have run.
type ScriptEntry struct {
	// Database is the database whose session the statement would have run
	// on. PostgreSQL has no USE: a script spanning databases has to be run
	// in pieces, one session per database, and this says which piece an
	// entry belongs to. A statement on a cluster-wide object (CREATE ROLE,
	// CREATE DATABASE) runs in whichever database its Server is connected
	// to, and records that one.
	Database string
	// SQL is the statement alone, with any bound parameters substituted as
	// literals (see bindScriptArgs) and no terminating semicolon of its own
	// added: String adds one.
	SQL string
}

// ScriptCollector accumulates the statements write methods would have run,
// instead of running them. See WithScript. Its entries are guarded by mu,
// since nothing stops a caller from issuing writes with one WithScript
// context from several goroutines, so they are read through Entries and
// Len, never a field.
//
// String renders the capture as one script, which is what a caller handing
// it to a person wants. Joining SQL by hand is the mistake it exists to
// prevent: an entry ending in a -- comment swallows a semicolon appended to
// it, and entries for different databases need separate sessions.
type ScriptCollector struct {
	mu      sync.Mutex
	entries []ScriptEntry
}

// append adds e under mu — the only way a write touches the entries.
func (c *ScriptCollector) append(e ScriptEntry) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries = append(c.entries, e)
}

// Add appends entries as if they had been captured, in order: statements
// gathered elsewhere, rendered by String as one script with the capture.
func (c *ScriptCollector) Add(entries ...ScriptEntry) {
	for _, e := range entries {
		c.append(e)
	}
}

// Entries returns a copy of every captured entry, in capture order.
func (c *ScriptCollector) Entries() []ScriptEntry {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.entries)
}

// Len returns how many entries have been captured.
func (c *ScriptCollector) Len() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	return len(c.entries)
}

// String renders every captured entry as one script, in capture order, each
// statement terminated by a semicolon on its own line run and followed by a
// blank line. When the entries span more than one database, a
// "-- database <name>" line opens each run of entries for the same
// database: PostgreSQL has no USE, so the person running the script runs
// each run connected to the database it names (psql's \connect, or a query
// tab of its own). It returns "" when nothing was captured.
func (c *ScriptCollector) String() string {
	entries := c.Entries()
	multi := slices.ContainsFunc(entries, func(e ScriptEntry) bool { return e.Database != entries[0].Database })
	var b strings.Builder
	for i, e := range entries {
		if i > 0 {
			b.WriteString("\n")
		}
		if multi && (i == 0 || e.Database != entries[i-1].Database) {
			b.WriteString("-- database " + pgsql.CommentSafe(e.Database) + "\n")
		}
		b.WriteString(terminated(e.SQL))
		b.WriteString("\n")
	}
	return b.String()
}

// terminated is stmt ending in a semicolon that ends it: one already there
// is kept, and one after a trailing -- comment goes on a line of its own,
// where the comment does not swallow it.
func terminated(stmt string) string {
	stmt = strings.TrimRight(stmt, " \t\r\n\f\v")
	var last pgsql.Span
	for sp := range pgsql.Spans(stmt) {
		last = sp
	}
	switch {
	case last.Kind == pgsql.SpanCode && strings.HasSuffix(stmt, ";"):
		return stmt
	case last.Kind == pgsql.SpanLineComment:
		return stmt + "\n;"
	}
	return stmt + ";"
}

// WithScript returns a derived context carrying a *ScriptCollector. Every
// pgmo write method invoked with it appends its statement to the collector
// and returns as if it had succeeded, without sending it — for previewing,
// or handing off, the exact SQL a set of pending edits would run (a "Script
// Changes" action that opens the statements in a query editor).
//
// Reads are unaffected and go to the server: only the write chokepoints
// (exec, execIn) consult the collector. A write's refusals are the same as
// when it runs, so a call that would fail fails while scripting too.
//
// InTransaction with a WithScript context sends nothing: it captures BEGIN,
// fn's writes, and COMMIT (ROLLBACK when fn fails), so the script applies
// whole or not at all, as the call would have.
func WithScript(ctx context.Context) (context.Context, *ScriptCollector) {
	c := &ScriptCollector{}
	return context.WithValue(ctx, scriptCtxKey{}, c), c
}

// Scripting reports whether ctx came from WithScript — that is, whether
// write methods invoked with it record their statement instead of running
// it.
//
// A caller mirroring a write into its own state needs to know: under
// WithScript a write returns success without the server ever seeing it, so
// state derived from "it worked" is wrong. A rename is the case it exists
// for — an editor that renames an object and then re-reads it by the new
// name finds nothing, because the server still has the old one.
func Scripting(ctx context.Context) bool {
	_, ok := scriptFrom(ctx)
	return ok
}

func scriptFrom(ctx context.Context) (*ScriptCollector, bool) {
	c, ok := ctx.Value(scriptCtxKey{}).(*ScriptCollector)
	return c, ok
}

// setIfApplied assigns v to *dst unless ctx is a WithScript context, where
// the write that just "succeeded" was only recorded and the server still
// holds the old value.
//
// A write mirroring its change back onto the receiver — a rename updating
// Name, an owner change updating Owner — goes through this rather than
// assigning: otherwise a scripted write leaves the object claiming state the
// server does not have, and the next call built from it (a second rename, a
// drop by name) targets an object that does not exist.
func setIfApplied[T any](ctx context.Context, dst *T, v T) {
	if !Scripting(ctx) {
		*dst = v
	}
}

// setPtrIfApplied is setIfApplied for an XChanges field, where nil means
// "not part of this change" and so is not mirrored at all.
func setPtrIfApplied[T any](ctx context.Context, dst *T, v *T) {
	if v != nil {
		setIfApplied(ctx, dst, *v)
	}
}

// createdObject is the tail of every CreateX: it returns what was created.
// Under Scripting(ctx) the statement was only collected, so there is nothing
// to read, and handle — the family's lookup-free Ref form — is the answer.
// Otherwise read fetches the object back from the catalog.
//
// A read-back that finds nothing returns handle too, not the not-found
// error: the create succeeded, and what failed is visibility (a catalog row
// another role's privileges hide, a row-level policy). The same holds for
// ErrHandleNotLoaded, a read-back that needs an oid handle lacks. Any other
// read error is returned as is.
func createdObject[T any](ctx context.Context, handle T, read func() (T, error)) (T, error) {
	if Scripting(ctx) {
		return handle, nil
	}
	v, err := read()
	if errors.Is(err, ErrNotFound) || errors.Is(err, ErrHandleNotLoaded) {
		return handle, nil
	}
	return v, err
}

// ============================================================
// The write chokepoints
// ============================================================

// exec is the chokepoint every write in s's own database goes through: under
// WithScript it captures stmt, with args bound into its text, and sends
// nothing; otherwise it runs stmt with args on s.on(ctx) — ctx's transaction
// inside InTransaction — once, never retried (statement.go). Its error is
// unwrapped: the caller names the operation. A captured write's CommandTag is
// the zero one, which affected no rows.
//
// DDL cannot take a parameter, so a DDL write passes no args and builds the
// statement complete through pgsql's quoting; a DML write binds its values
// ($1, $2, …).
func (s *Server) exec(ctx context.Context, stmt string, args ...any) (pgconn.CommandTag, error) {
	if captured, err := s.capture(ctx, s.database, stmt, args); captured || err != nil {
		return pgconn.CommandTag{}, err
	}
	return s.execOn(ctx, s.on(ctx), stmt, args)
}

// execIn is exec for a write in database name, through s.onDatabase(ctx,
// name): inside InTransaction a database other than the transaction's is
// refused, scripting or not.
func (s *Server) execIn(ctx context.Context, name, stmt string, args ...any) (pgconn.CommandTag, error) {
	q, err := s.onDatabase(ctx, name)
	if err != nil {
		return pgconn.CommandTag{}, err
	}
	if captured, err := s.capture(ctx, name, stmt, args); captured || err != nil {
		return pgconn.CommandTag{}, err
	}
	return s.execOn(ctx, q, stmt, args)
}

// execOn runs stmt once on q, bounded by the Server's lifetime, so Close
// cancels a long write (CREATE INDEX) instead of waiting for it.
func (s *Server) execOn(ctx context.Context, q Querier, stmt string, args []any) (pgconn.CommandTag, error) {
	ctx, release := s.bound(ctx)
	defer release()
	tag, err := q.Exec(ctx, stmt, args...)
	return tag, cancelCause(ctx, err)
}

// capture is the script-mode half of a write: under WithScript it appends
// stmt, bound, as run in database, and reports true; otherwise false. A write
// reading a value back (execScan) has no value to return under WithScript,
// so its method checks Scripting itself, captures through here, and returns
// its handle (createdObject).
func (s *Server) capture(ctx context.Context, database, stmt string, args []any) (bool, error) {
	c, ok := scriptFrom(ctx)
	if !ok {
		return false, nil
	}
	bound, err := bindScriptArgs(stmt, args)
	if err != nil {
		return true, err
	}
	c.append(ScriptEntry{Database: database, SQL: bound})
	return true, nil
}

// ============================================================
// Binding parameters into a captured statement
// ============================================================

// bindScriptArgs substitutes the literal form of each arg for its $N
// placeholder in q, so a statement captured under WithScript runs on its
// own: a query editor binds nothing, and a script left saying $1 fails with
// "there is no parameter $1".
//
// The placeholders are the lexer's (pgsql.Spans' SpanParam), so a $1 inside
// a literal, a quoted identifier, a comment or a dollar-quoted function body
// — where it means something else, or nothing — is left alone; text
// replacement would rewrite an object's name in a script the user is about
// to run by hand.
//
// Every $N must have an argument and every argument a $N, as the server
// requires when the statement is bound (it would infer no type for one never
// used), so a mismatch is an error here rather than a script that differs
// from the write. Errors carry no "pgmo:" prefix: they reach the caller
// through the write that captured them, which adds its own.
func bindScriptArgs(q string, args []any) (string, error) {
	var out strings.Builder
	used := make([]bool, len(args))
	for sp := range pgsql.Spans(q) {
		text := q[sp.Start:sp.End]
		if sp.Kind != pgsql.SpanParam {
			out.WriteString(text)
			continue
		}
		n, err := strconv.Atoi(text[1:])
		if err != nil || n < 1 || n > len(args) {
			return "", invalidf("script: %s has no argument (%d given)", text, len(args))
		}
		lit, err := scriptLiteral(args[n-1])
		if err != nil {
			return "", unsupportedf("script: %s: %w", text, err)
		}
		// A literal opening with a quote after "U&" would turn into a
		// Unicode-escape literal; a space keeps it a literal of its own.
		if s := out.String(); strings.HasSuffix(s, "&") && strings.HasPrefix(strings.TrimPrefix(lit, "E"), "'") {
			out.WriteByte(' ')
		}
		out.WriteString(lit)
		used[n-1] = true
	}
	if i := slices.Index(used, false); i >= 0 {
		return "", invalidf("script: argument %d is bound to no $%d in the statement", i+1, i+1)
	}
	return out.String(), nil
}

// scriptLiteral renders one bound argument as the literal that means what
// pgx would have sent for it, in the same position. Only plain Go values are
// rendered; anything else is an error rather than a %v guess, since a
// silently wrong literal in a script the user runs by hand is worse than a
// refusal.
//
//   - Strings, byte slices, arrays of strings and times are untyped
//     literals ('…'), which the server types from their position as it types
//     a parameter there. QuoteLiteral writes E'…' for one with a backslash,
//     so the script reads the same whatever standard_conforming_strings is.
//   - A negative number is parenthesised, so it cannot join the operator
//     before it ("x - -1" is not "x --1", a comment; "x !=-1" is not the
//     operator "!=-").
//   - A non-finite float is a typed literal ('NaN'::double precision): bare,
//     NaN is a column name. So is -0, which as a number is the integer 0
//     negated: 0.
//   - A time is written in its own zone with its offset (timeLiteral), so a
//     timestamptz reads its instant and a timestamp its wall clock, as pgx
//     binds them.
//   - nil, a nil pointer, a nil slice and a driver.Valuer returning nil are
//     NULL. A pointer renders what it points at, a driver.Valuer its Value.
func scriptLiteral(v any) (string, error) {
	switch x := v.(type) {
	case nil:
		return "NULL", nil
	case string:
		return pgsql.QuoteLiteral(x), nil
	case []byte:
		if x == nil {
			return "NULL", nil
		}
		return pgsql.QuoteLiteral(`\x` + hex.EncodeToString(x)), nil
	case []string:
		if x == nil {
			return "NULL", nil
		}
		return pgsql.QuoteLiteral(arrayText(x)), nil
	case bool:
		return strconv.FormatBool(x), nil
	case int:
		return intLiteral(int64(x)), nil
	case int8:
		return intLiteral(int64(x)), nil
	case int16:
		return intLiteral(int64(x)), nil
	case int32:
		return intLiteral(int64(x)), nil
	case int64:
		return intLiteral(x), nil
	case uint:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint8:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint16:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint32:
		return strconv.FormatUint(uint64(x), 10), nil
	case uint64:
		return strconv.FormatUint(x, 10), nil
	case float32:
		return floatLiteral(float64(x), 32), nil
	case float64:
		return floatLiteral(x, 64), nil
	case time.Time:
		return timeLiteral(x)
	case driver.Valuer:
		if rv := reflect.ValueOf(x); rv.Kind() == reflect.Pointer && rv.IsNil() {
			return "NULL", nil
		}
		dv, err := x.Value()
		if err != nil {
			return "", err
		}
		return scriptLiteral(dv)
	}
	if rv := reflect.ValueOf(v); rv.Kind() == reflect.Pointer {
		if rv.IsNil() {
			return "NULL", nil
		}
		return scriptLiteral(rv.Elem().Interface())
	}
	return "", unsupportedf("cannot script a %T argument", v)
}

func intLiteral(n int64) string {
	if n < 0 {
		return "(" + strconv.FormatInt(n, 10) + ")"
	}
	return strconv.FormatInt(n, 10)
}

// floatLiteral is f as the shortest numeric constant that reads back as f at
// its precision; the server converts it to the column's type as it would a
// bound float.
func floatLiteral(f float64, bits int) string {
	typ := "double precision"
	if bits == 32 {
		typ = "real"
	}
	switch {
	case math.IsNaN(f):
		return "'NaN'::" + typ
	case math.IsInf(f, 1):
		return "'Infinity'::" + typ
	case math.IsInf(f, -1):
		return "'-Infinity'::" + typ
	case f == 0 && math.Signbit(f):
		// -0 is the integer 0 negated, which is 0.
		return "'-0'::" + typ
	}
	s := strconv.FormatFloat(f, 'g', -1, bits)
	if f < 0 {
		return "(" + s + ")"
	}
	return s
}

// timeLiteral is t in its own zone, with its UTC offset, to the microsecond
// PostgreSQL keeps (truncated, as pgx truncates). That reads as pgx binds t
// in either position: a timestamptz takes the instant, a timestamp the wall
// clock — its input ignores the offset, as pgx drops t's location — and a
// date the day. Years before 1 AD and after 9999 have forms Go's layout
// cannot write (BC, five digits), and are refused.
func timeLiteral(t time.Time) (string, error) {
	if y := t.Year(); y < 1 || y > 9999 {
		return "", unsupportedf("cannot script the year %d", y)
	}
	return "'" + t.Format("2006-01-02 15:04:05.999999-07:00:00") + "'", nil
}

// arrayText is a text array's input form: every element double-quoted, with
// " and \ backslash-escaped, so no element is read as NULL or split.
func arrayText(elems []string) string {
	var b strings.Builder
	b.WriteByte('{')
	for i, e := range elems {
		if i > 0 {
			b.WriteByte(',')
		}
		b.WriteByte('"')
		for _, r := range e {
			if r == '"' || r == '\\' {
				b.WriteByte('\\')
			}
			b.WriteRune(r)
		}
		b.WriteByte('"')
	}
	b.WriteByte('}')
	return b.String()
}
