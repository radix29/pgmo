//go:build livedb

package pgmo

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/radix29/pgmo/internal/scram"
	"github.com/radix29/pgmo/pgsql"
)

// liveLoginRole creates a throwaway LOGIN role and drops it when the test
// ends.
func liveLoginRole(t *testing.T, admin *pgxpool.Pool, ctx context.Context) string {
	t.Helper()
	name := fmt.Sprintf("pgmo_live_pw_%d", time.Now().UnixNano())
	if _, err := admin.Exec(ctx, "CREATE ROLE "+name+" LOGIN"); err != nil {
		t.Fatalf("create role: %v", err)
	}
	t.Cleanup(func() {
		if _, err := admin.Exec(context.Background(), "DROP ROLE "+name); err != nil {
			t.Errorf("drop role %s: %v", name, err)
		}
	})
	return name
}

// storedPassword is role's pg_authid.rolpassword.
func storedPassword(t *testing.T, admin *pgxpool.Pool, ctx context.Context, role string) string {
	t.Helper()
	var v string
	if err := admin.QueryRow(ctx, `SELECT rolpassword FROM pg_catalog.pg_authid WHERE rolname = $1`, role).Scan(&v); err != nil {
		t.Fatalf("read rolpassword: %v", err)
	}
	return v
}

// livePasswords walks each step of SASLprep (internal/scram's
// normalizeCases, less the one that is not UTF-8, which the server refuses
// in a literal).
var livePasswords = []string{
	"pencil", "a\x01b\x7f", "a\u00a0b\u3000c", "pa\u00adss\u200dword", "a\u200bb",
	"\uff21\ufb01\u00b2", "e\u0301", "p\u00e4ss", "\u00ad\ufeff", "\u00e4\ue000",
	"\u00e4\u0085", "\u00e4\u0221", "\u2474", "\u05d0\u05d1", "\u05d01\u05d1",
	"\u05d0a\u05d1", "\u05d0\u05d11", "\u00b2\u0221", "\u05d0\uff21\u05d1", "1\u05d0\u00b2\u05d1",
	"\u05d0\u00b2\u05d11", "\u05d0\u00b2\u05d1", "s3cr3t'quote", `back\slash`,
}

// TestLiveRolePasswordMatchesServer holds alterRolePassword to the server:
// for each password, the verifier the server computes from the cleartext,
// recomputed here with its salt, is the same string, so the password pgmo
// sets is the one the cleartext would have set; the one pgmo stores verifies
// against it; a login with it succeeds (where pgx prepares it as the server
// does), a wrong one fails; scram_iterations is honoured; and the cleartext
// sent through pgmo reaches no pg_stat_statements entry.
func TestLiveRolePasswordMatchesServer(t *testing.T) {
	admin, ctx, done := liveDB(t)
	t.Cleanup(done) // after liveLoginRole's drop, which needs admin
	role := liveLoginRole(t, admin, ctx)
	s, err := Connect(ctx, ConnectionOptions{ConnString: *liveConnString, ApplicationName: "pgmo - live secret"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	for _, pw := range livePasswords {
		// The server's own preparation and hashing of the cleartext.
		if _, err := admin.Exec(ctx, "ALTER ROLE "+role+" PASSWORD "+pgsql.QuoteLiteral(pw)); err != nil {
			t.Fatalf("%+q: server-side hash: %v", pw, err)
		}
		server := storedPassword(t, admin, ctx, role)
		iterations, salt, ok := scram.Parse(server)
		if !ok {
			t.Fatalf("%+q: server stored %q, not a SCRAM verifier", pw, server)
		}
		if got := scram.Verifier(pw, salt, iterations); got != server {
			t.Errorf("%+q: verifier %s; the server's is %s", pw, got, server)
		}

		// pgmo's.
		if err := s.alterRolePassword(ctx, role, pw); err != nil {
			t.Fatalf("%+q: %v", pw, err)
		}
		stored := storedPassword(t, admin, ctx, role)
		if stored == server || !scram.Verifies(stored, pw) {
			t.Errorf("%+q: stored %s, not a fresh verifier of the password", pw, stored)
		}
		if n, _, _ := scram.Parse(stored); n != iterations {
			t.Errorf("%+q: stored at %d iterations, the server's setting is %d", pw, n, iterations)
		}
	}

	t.Run("login", func(t *testing.T) {
		login := func(pw string) error {
			c, err := Connect(ctx, ConnectionOptions{ConnString: *liveConnString, User: role, Password: pw,
				ApplicationName: "pgmo - live secret login"})
			if err == nil {
				c.Close()
			}
			return err
		}
		for _, pw := range []string{"pencil", "p\u00e4ss", `s3cr3t'quote`, `back\slash`} {
			if err := s.alterRolePassword(ctx, role, pw); err != nil {
				t.Fatal(err)
			}
			if err := login(pw); err != nil {
				t.Errorf("%+q: login: %v", pw, err)
			}
			if err := login(pw + "x"); err == nil || !strings.Contains(err.Error(), "28P01") {
				t.Errorf("%+q: a wrong password: %v, want 28P01", pw, err)
			}
		}
	})

	t.Run("scram_iterations", func(t *testing.T) {
		if s.Version() < PostgreSQL16 {
			t.Skip("scram_iterations is PostgreSQL 16 and later")
		}
		err := s.InTransaction(ctx, func(ctx context.Context) error {
			if _, err := s.exec(ctx, "SET LOCAL scram_iterations = 5000"); err != nil {
				return err
			}
			return s.alterRolePassword(ctx, role, "pencil")
		})
		if err != nil {
			t.Fatal(err)
		}
		stored := storedPassword(t, admin, ctx, role)
		if n, _, _ := scram.Parse(stored); n != 5000 || !scram.Verifies(stored, "pencil") {
			t.Errorf("stored %s, want a verifier of 5000 iterations", stored)
		}
	})

	t.Run("pg_stat_statements", func(t *testing.T) {
		var installed bool
		if err := admin.QueryRow(ctx, `SELECT pg_catalog.to_regclass('public.pg_stat_statements') IS NOT NULL`).Scan(&installed); err != nil {
			t.Fatal(err)
		}
		if !installed {
			t.Skip("pg_stat_statements is not installed in this database")
		}
		marker := fmt.Sprintf("pgmo-marker-%d", time.Now().UnixNano())
		if err := s.alterRolePassword(ctx, role, marker); err != nil {
			t.Fatal(err)
		}
		var leaked, recorded int
		if err := admin.QueryRow(ctx, `SELECT pg_catalog.count(*) FILTER (WHERE query LIKE '%' || $1 || '%'),
			pg_catalog.count(*) FILTER (WHERE query LIKE '%' || $2 || '%')
			FROM public.pg_stat_statements`, marker, role).Scan(&leaked, &recorded); err != nil {
			t.Fatal(err)
		}
		if leaked != 0 {
			t.Errorf("the cleartext is in %d pg_stat_statements entries", leaked)
		}
		if recorded == 0 {
			t.Log("no pg_stat_statements entry names the role: utility statements not tracked here")
		}
	})
}
