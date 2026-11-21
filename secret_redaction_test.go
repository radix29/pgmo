package pgmo_test

import (
	"context"
	"errors"
	"fmt"
	"regexp"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/radix29/pgmo"
	"github.com/radix29/pgmo/internal/scram"
	"github.com/radix29/pgmo/pgmotest"
)

// secretWrite is one write carrying passwords, run against s.
type secretWrite struct {
	name      string
	passwords []string // every password the write sends; each contains "s3cr3t"
	write     func(ctx context.Context, s *pgmo.Server) error
}

// secretWrites is every write that sends a password. A new one goes here:
// TestSecretsNeverCapturedOrSent is what holds it to the policy.
func secretWrites() []secretWrite {
	pw := func(n string) string { return "s3cr3t'" + n }
	return []secretWrite{
		{"alter role password", []string{pw("role")}, func(ctx context.Context, s *pgmo.Server) error {
			return s.AlterRolePassword(ctx, "app", pw("role"))
		}},
	}
}

// scramLiteral matches a verifier literal in a statement.
var scramLiteral = regexp.MustCompile(`'(SCRAM-SHA-256\$[^']*)'`)

// verifiersOf is every verifier literal in text.
func verifiersOf(text string) []string {
	var out []string
	for _, m := range scramLiteral.FindAllStringSubmatch(text, -1) {
		out = append(out, m[1])
	}
	return out
}

// wantVerifiers checks that text sends each password as a verifier of it
// with iterations, and never in the clear.
func wantVerifiers(t *testing.T, what, text string, passwords []string, iterations int) {
	t.Helper()
	if strings.Contains(text, "s3cr3t") {
		t.Errorf("%s carries a password in the clear:\n%s", what, text)
	}
	vs := verifiersOf(text)
	for _, p := range passwords {
		found := false
		for _, v := range vs {
			if n, _, _ := scram.Parse(v); scram.Verifies(v, p) && n == iterations {
				found = true
			}
		}
		if !found {
			t.Errorf("%s has no verifier of %q at %d iterations:\n%s", what, p, iterations, text)
		}
	}
}

// A password reaches the server as its SCRAM verifier and nothing else: a
// WithScript capture holds the bare placeholder and reads nothing from the
// server; WithScriptSecrets captures the verifier; a run sends the verifier,
// at the server's scram_iterations (4096 where there is none), and its
// error carries neither.
func TestSecretsNeverCapturedOrSent(t *testing.T) {
	for _, w := range secretWrites() {
		t.Run(w.name, func(t *testing.T) {
			// Captured: the placeholder, bare, and nothing sent — not even
			// the scram_iterations read.
			f := pgmotest.New(t)
			s := f.Server()
			before := len(f.Queries())
			ctx, col := pgmo.WithScript(t.Context())
			if err := w.write(ctx, s); err != nil {
				t.Fatalf("under WithScript: %v", err)
			}
			script := col.String()
			if strings.Contains(script, "s3cr3t") || len(verifiersOf(script)) > 0 || strings.Contains(script, "SCRAM") {
				t.Errorf("captured script carries a secret:\n%s", script)
			}
			if !strings.Contains(script, "PASSWORD "+pgmo.PasswordPlaceholder) || strings.Contains(script, "'"+pgmo.PasswordPlaceholder) {
				t.Errorf("captured script lacks the bare placeholder:\n%s", script)
			}
			if q := f.Queries()[before:]; len(q) != 0 {
				t.Errorf("WithScript sent %v", q)
			}

			// Captured with WithScriptSecrets: verifiers, at the server's count.
			f.On("current_setting('scram_iterations'", pgmotest.Rows([]any{"7"}))
			ctx, col = pgmo.WithScript(pgmo.WithScriptSecrets(t.Context()))
			if err := w.write(ctx, s); err != nil {
				t.Fatalf("under WithScriptSecrets: %v", err)
			}
			wantVerifiers(t, "WithScriptSecrets script", col.String(), w.passwords, 7)

			// Run: verifiers sent, at the server's count; nothing captured.
			before = len(f.Queries())
			if err := w.write(t.Context(), s); err != nil {
				t.Fatalf("run: %v", err)
			}
			var sent strings.Builder
			for _, q := range f.Queries()[before:] {
				fmt.Fprintf(&sent, "%s %v\n", q.Text, q.Args)
			}
			wantVerifiers(t, "statements sent", sent.String(), w.passwords, 7)

			// PostgreSQL 15 has no scram_iterations: the fixed 4096.
			f15 := pgmotest.New(t)
			f15.Version = pgmo.PostgreSQL15
			f15.On("current_setting('scram_iterations'", pgmotest.Rows([]any{nil}))
			before = len(f15.Queries())
			if err := w.write(t.Context(), f15.Server()); err != nil {
				t.Fatalf("run on 15: %v", err)
			}
			sent.Reset()
			for _, q := range f15.Queries()[before:] {
				sent.WriteString(q.Text + "\n")
			}
			wantVerifiers(t, "statements sent to 15", sent.String(), w.passwords, scram.DefaultIterations)

			// A failed write's error names neither the password nor the
			// verifier.
			ff := pgmotest.New(t)
			ff.On("current_setting('scram_iterations'", pgmotest.Rows([]any{"1"}))
			ff.OnExec("PASSWORD", pgmotest.Fail(&pgconn.PgError{Code: "42501", Message: "permission denied to alter role"}))
			err := w.write(t.Context(), ff.Server())
			if err == nil || !pgmo.IsPermissionDenied(err) {
				t.Fatalf("failed write: %v, want the server's 42501", err)
			}
			if msg := err.Error(); strings.Contains(msg, "s3cr3t") || strings.Contains(msg, "SCRAM") {
				t.Errorf("error carries a secret: %s", msg)
			}
		})
	}
}

// The statement itself: the role quoted, the verifier a literal.
func TestAlterRolePasswordStatement(t *testing.T) {
	f := pgmotest.New(t)
	s := f.Server()
	ctx, col := pgmo.WithScript(t.Context())
	if err := s.AlterRolePassword(ctx, `Ap"p`, "x"); err != nil {
		t.Fatal(err)
	}
	if got, want := col.String(), `ALTER ROLE "Ap""p" PASSWORD <insert password here>;`+"\n"; got != want {
		t.Errorf("captured %q, want %q", got, want)
	}
	f.On("current_setting('scram_iterations'", pgmotest.Rows([]any{"1"}))
	ctx, col = pgmo.WithScript(pgmo.WithScriptSecrets(t.Context()))
	if err := s.AlterRolePassword(ctx, "app", "x"); err != nil {
		t.Fatal(err)
	}
	e := col.Entries()
	if len(e) != 1 || !regexp.MustCompile(`^ALTER ROLE app PASSWORD 'SCRAM-SHA-256\$1:[^']+'$`).MatchString(e[0].SQL) || e[0].Database != "postgres" {
		t.Errorf("captured %q", e)
	}
}

// Refused before anything is built or sent: no role, an empty password
// (PostgreSQL clears rather than sets it), a NUL, a scram_iterations that is
// not a count.
func TestAlterRolePasswordRefusals(t *testing.T) {
	for _, c := range []struct {
		name, role, password, iterations string
		kind                             error
	}{
		{"no role", "", "x", "1", pgmo.ErrInvalidRequest},
		{"empty password", "app", "", "1", pgmo.ErrInvalidRequest},
		{"NUL", "app", "a\x00b", "1", pgmo.ErrInvalidRequest},
		{"iterations not a number", "app", "x", "many", pgmo.ErrInvalidRequest},
		{"iterations zero", "app", "x", "0", pgmo.ErrInvalidRequest},
	} {
		t.Run(c.name, func(t *testing.T) {
			f := pgmotest.New(t)
			f.On("current_setting('scram_iterations'", pgmotest.Rows([]any{c.iterations}))
			s := f.Server()
			before := len(f.Queries())
			err := s.AlterRolePassword(t.Context(), c.role, c.password)
			if !errors.Is(err, c.kind) || !strings.HasPrefix(err.Error(), "pgmo: alter role ") {
				t.Errorf("%v, want %v under \"pgmo: alter role \"", err, c.kind)
			}
			for _, q := range f.Queries()[before:] {
				if q.Kind == pgmotest.KindExec {
					t.Errorf("sent %q", q.Text)
				}
			}
		})
	}
}

// Inside InTransaction the write runs on the transaction, and its
// scram_iterations read too: SET LOCAL scram_iterations applies.
func TestAlterRolePasswordInTransaction(t *testing.T) {
	f := pgmotest.New(t)
	f.On("current_setting('scram_iterations'", pgmotest.Rows([]any{"3"}))
	s := f.Server()
	err := s.InTransaction(t.Context(), func(ctx context.Context) error {
		return s.AlterRolePassword(ctx, "app", "s3cr3t")
	})
	if err != nil {
		t.Fatal(err)
	}
	var tx int
	for _, q := range f.Queries() {
		if strings.Contains(q.Text, "scram_iterations") || strings.HasPrefix(q.Text, "ALTER ROLE") {
			if q.Tx == 0 {
				t.Errorf("%q ran outside the transaction", q.Text)
			}
			tx++
		}
	}
	if tx != 2 {
		t.Errorf("saw %d of the read and the write", tx)
	}
}
