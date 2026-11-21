package pgmo

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/radix29/pgmo/internal/scram"
	"github.com/radix29/pgmo/pgsql"
)

// ============================================================
// Secrets: sent to the server, never captured, never logged
// ============================================================

// PasswordPlaceholder stands in for a password a captured write carries, and
// for one a generated script cannot know, so a captured write and a
// generated script agree. It is written bare, not as a literal —
//
//	ALTER ROLE app PASSWORD <insert password here>
//
// — so a script run without replacing it fails to parse. A quoted
// placeholder would run, and set the role's password to the placeholder's
// text: a password anyone reading pgmo's source knows. Replace it with the
// password as a literal ('…'), or better with its verifier, as psql's
// \password sends it.
const PasswordPlaceholder = "<insert password here>"

// scriptSecretsKey marks a context from WithScriptSecrets.
type scriptSecretsKey struct{}

// WithScriptSecrets returns a derived context under which a WithScript
// collector captures the secrets a write carries as they would be sent,
// instead of PasswordPlaceholder: for a caller that hands the script to a
// machine to run, not to a person to read. Outside WithScript it has no
// effect.
//
// A password is sent, and so captured, as its SCRAM-SHA-256 verifier, never
// in the clear: a script captured with WithScriptSecrets sets the password
// the write would have set, and still does not hold it.
func WithScriptSecrets(ctx context.Context) context.Context {
	return context.WithValue(ctx, scriptSecretsKey{}, true)
}

// scriptSecrets reports whether ctx is a WithScript context capturing
// secrets (WithScriptSecrets): when it is not, a secret write's statement as
// sent is never needed, and the work of building it — a password's verifier
// — is skipped.
func scriptSecrets(ctx context.Context) bool {
	keep, _ := ctx.Value(scriptSecretsKey{}).(bool)
	return keep
}

// execSecret is exec for a statement carrying a secret: stmt, with the
// secret, is what runs; shown, the same statement with a placeholder in its
// place, is what a WithScript collector captures unless ctx carries
// WithScriptSecrets. Every password a write sends goes through here: a
// captured script is shown to a person, and must not carry one.
//
// A caller builds stmt and shown from one function of the secret's text, so
// they cannot differ but by it, and skips building stmt (passing "") when
// Scripting(ctx) && !scriptSecrets(ctx), where it is never used. A secret
// statement is DDL and binds no arguments. Its error is the server's, which
// names no statement text.
func (s *Server) execSecret(ctx context.Context, stmt, shown string) (pgconn.CommandTag, error) {
	captured := shown
	if scriptSecrets(ctx) {
		captured = stmt
	}
	if done, err := s.capture(ctx, s.database, captured, nil); done || err != nil {
		return pgconn.CommandTag{}, err
	}
	return s.execOn(ctx, s.on(ctx), stmt, nil)
}

// ------------------------------------------------------------
// Passwords: SCRAM-SHA-256 verifiers computed here
// ------------------------------------------------------------

// passwordVerifier is password's SCRAM-SHA-256 verifier, as psql's \password
// computes it (libpq's PQencryptPasswordConn): a fresh salt, the iterations
// of the session's scram_iterations (PostgreSQL 16 and later; 4096 before,
// where the count is fixed), and the password prepared by SASLprep as the
// server prepares one sent in the clear (scram.Normalize). The server stores
// a verifier as it is, so the password it sets is the one sending password
// would have set, and the cleartext never reaches the server: not its log
// (log_statement), pg_stat_statements, pg_stat_activity or an error report.
//
// The verifier is SCRAM whatever password_encryption says. That is psql's
// \password with the default setting; with password_encryption = md5 psql
// would send an md5 hash, which PostgreSQL 18 deprecates and pgmo never
// writes. A SCRAM verifier serves an md5 pg_hba.conf line too: the server
// then authenticates with SCRAM.
//
// scram_iterations is read with current_setting's missing_ok rather than a
// version gate: on PostgreSQL 15 it is NULL, and the count fixed at 4096 is
// the setting's default, so the answer is the same.
func (s *Server) passwordVerifier(ctx context.Context, password string) (string, error) {
	var setting *string
	if err := queryRowScan(ctx, s.on(ctx), `SELECT pg_catalog.current_setting('scram_iterations', true)`, nil, &setting); err != nil {
		return "", err
	}
	iterations := scram.DefaultIterations
	if setting != nil {
		n, err := strconv.Atoi(*setting)
		if err != nil || n < 1 {
			return "", invalidf("scram_iterations is %q, not a positive count", *setting)
		}
		iterations = n
	}
	return scram.NewVerifier(password, iterations), nil
}

// checkPassword refuses a password no SCRAM login can present: the empty
// one, which PostgreSQL clears rather than sets ("empty string is not a
// valid password") — clearing a password is a write of its own — and one
// holding NUL, which no PostgreSQL client can send.
func checkPassword(password string) error {
	switch {
	case password == "":
		return invalidf("the password is empty; clearing a password is PASSWORD NULL")
	case strings.ContainsRune(password, 0):
		return invalidf("the password holds a NUL character, which no client can send")
	}
	return nil
}

// passwordClause is "PASSWORD <verifier>" for password — or, when the
// statement as sent is not needed (see execSecret), the empty string — and
// its shown form "PASSWORD <insert password here>". The write it belongs to
// builds its statement around each.
func (s *Server) passwordClause(ctx context.Context, password string) (sent, shown string, err error) {
	if err := checkPassword(password); err != nil {
		return "", "", err
	}
	shown = "PASSWORD " + PasswordPlaceholder
	if Scripting(ctx) && !scriptSecrets(ctx) {
		return "", shown, nil
	}
	verifier, err := s.passwordVerifier(ctx, password)
	if err != nil {
		return "", "", err
	}
	return "PASSWORD " + pgsql.QuoteLiteral(verifier), shown, nil
}

// alterRolePassword sets role's password: ALTER ROLE … PASSWORD with the
// password's verifier (passwordClause). It is the write the role family's
// password methods (P4.13.2) are built on.
func (s *Server) alterRolePassword(ctx context.Context, role, password string) error {
	fail := func(err error) error { return fmt.Errorf("pgmo: alter role %q password: %w", role, err) }
	if role == "" {
		return invalidf("pgmo: alter role password: no role name")
	}
	sent, shown, err := s.passwordClause(ctx, password)
	if err != nil {
		return fail(err)
	}
	head := "ALTER ROLE " + pgsql.QuoteIdent(role) + " "
	if sent != "" {
		sent = head + sent
	}
	if _, err := s.execSecret(ctx, sent, head+shown); err != nil {
		return fail(err)
	}
	return nil
}
