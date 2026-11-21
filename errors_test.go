package pgmo

import (
	"errors"
	"fmt"
	"io"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// sentinels is every exported sentinel with the text gosmo's twin carries
// (ErrDefinitionHidden says "role" where gosmo says "login").
var sentinels = []struct {
	name string
	err  error
	text string
}{
	{"ErrNotFound", ErrNotFound, "not found"},
	{"ErrAmbiguous", ErrAmbiguous, "ambiguous name"},
	{"ErrSchemaRequired", ErrSchemaRequired, "schema is required"},
	{"ErrHandleNotLoaded", ErrHandleNotLoaded, "handle not loaded: read it by name first"},
	{"ErrUnsupportedVersion", ErrUnsupportedVersion, "unsupported server version"},
	{"ErrUnsupported", ErrUnsupported, "not supported"},
	{"ErrDefinitionHidden", ErrDefinitionHidden, "definition not visible to the role"},
	{"ErrInvalidRequest", ErrInvalidRequest, "invalid request"},
}

func TestSentinelText(t *testing.T) {
	for _, s := range sentinels {
		if got := s.err.Error(); got != s.text {
			t.Errorf("%s.Error() = %q, want %q", s.name, got, s.text)
		}
	}
}

// TestSentinelsAreDistinct pins that no sentinel reaches another through
// errors.Is, wrapped or not: a caller switching on them must land in exactly
// one case.
func TestSentinelsAreDistinct(t *testing.T) {
	for _, a := range sentinels {
		wrapped := fmt.Errorf("pgmo: op: %w", a.err)
		for _, b := range sentinels {
			want := a.name == b.name
			if got := errors.Is(wrapped, b.err); got != want {
				t.Errorf("errors.Is(wrapped %s, %s) = %v, want %v", a.name, b.name, got, want)
			}
		}
	}
}

// pgErr is a server error with SQLSTATE code, wrapped the way pgmo wraps
// one, so every helper is shown to look through the chain.
func pgErr(code string) error {
	return fmt.Errorf("pgmo: op: %w", &pgconn.PgError{Severity: "ERROR", Code: code, Message: "m"})
}

func TestAsPgError(t *testing.T) {
	pe := &pgconn.PgError{Code: "42501"}
	for _, err := range []error{pe, fmt.Errorf("a: %w", pe), errors.Join(io.EOF, fmt.Errorf("b: %w", pe))} {
		got, ok := AsPgError(err)
		if !ok || got != pe {
			t.Errorf("AsPgError(%v) = %v, %v; want the PgError", err, got, ok)
		}
	}
	for _, err := range []error{nil, io.EOF, ErrNotFound, fmt.Errorf("pgmo: op: %w", ErrInvalidRequest)} {
		if got, ok := AsPgError(err); ok || got != nil {
			t.Errorf("AsPgError(%v) = %v, %v; want nil, false", err, got, ok)
		}
	}
}

// TestSQLStatePredicates pins each predicate to exactly its codes: every code
// below answers true for the one predicate named and false for the others,
// and the near misses (42703, 42701, 57P04, a class prefix alone) for all.
func TestSQLStatePredicates(t *testing.T) {
	preds := map[string]func(error) bool{
		"IsPermissionDenied": IsPermissionDenied,
		"IsUndefinedObject":  IsUndefinedObject,
		"IsAlreadyExists":    IsAlreadyExists,
		"IsQueryCanceled":    IsQueryCanceled,
	}
	cases := []struct{ code, pred string }{
		{"42501", "IsPermissionDenied"},
		{"42P01", "IsUndefinedObject"}, {"42704", "IsUndefinedObject"}, {"42883", "IsUndefinedObject"},
		{"3F000", "IsUndefinedObject"}, {"3D000", "IsUndefinedObject"},
		{"42P07", "IsAlreadyExists"}, {"42710", "IsAlreadyExists"}, {"42723", "IsAlreadyExists"},
		{"42P06", "IsAlreadyExists"}, {"42P04", "IsAlreadyExists"},
		{"57014", "IsQueryCanceled"},
		{"42703", ""}, {"42701", ""}, {"57P04", ""}, {"42", ""}, {"42P0", ""}, {"", ""}, {"XX000", ""},
	}
	for _, c := range cases {
		for name, pred := range preds {
			if got, want := pred(pgErr(c.code)), name == c.pred; got != want {
				t.Errorf("%s(SQLSTATE %q) = %v, want %v", name, c.code, got, want)
			}
		}
	}
	for name, pred := range preds {
		for _, err := range []error{nil, io.EOF, ErrNotFound, errors.New("42501")} {
			if pred(err) {
				t.Errorf("%s(%v) = true for an error that is not the server's", name, err)
			}
		}
	}
}
