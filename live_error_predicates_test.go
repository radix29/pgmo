//go:build livedb

package pgmo

import (
	"context"
	"testing"

	"github.com/jackc/pgx/v5"
)

// TestLiveErrorPredicates pins each SQLSTATE predicate to the error the
// server actually sends for the statement that should trip it. Every
// statement fails before it changes anything, or runs inside a transaction
// that is rolled back, so nothing on the server is touched.
func TestLiveErrorPredicates(t *testing.T) {
	pool, ctx, done := liveDB(t)
	defer done()

	cases := []struct {
		name  string
		sql   []string // run in one transaction; the last must fail
		pred  func(error) bool
		pname string
		noTx  bool // a single statement that cannot run in a transaction
	}{
		{"missing table", []string{`SELECT * FROM pgmo_no_such_table`}, IsUndefinedObject, "IsUndefinedObject", false},
		{"drop missing table", []string{`DROP TABLE pgmo_no_such_table`}, IsUndefinedObject, "IsUndefinedObject", false},
		{"drop missing schema", []string{`DROP SCHEMA pgmo_no_such_schema`}, IsUndefinedObject, "IsUndefinedObject", false},
		{"drop missing function", []string{`DROP FUNCTION pgmo_no_such_fn()`}, IsUndefinedObject, "IsUndefinedObject", false},
		{"drop missing role", []string{`DROP ROLE pgmo_no_such_role`}, IsUndefinedObject, "IsUndefinedObject", false},
		{"drop missing database", []string{`DROP DATABASE pgmo_no_such_db`}, IsUndefinedObject, "IsUndefinedObject", true},
		{"duplicate table", []string{`CREATE TEMP TABLE pgmo_dup(i int)`, `CREATE TEMP TABLE pgmo_dup(i int)`}, IsAlreadyExists, "IsAlreadyExists", false},
		{"duplicate schema", []string{`CREATE SCHEMA pgmo_dup_s`, `CREATE SCHEMA pgmo_dup_s`}, IsAlreadyExists, "IsAlreadyExists", false},
		{"duplicate function", []string{
			`CREATE FUNCTION pg_temp.pgmo_dup_f() RETURNS int LANGUAGE sql AS 'SELECT 1'`,
			`CREATE FUNCTION pg_temp.pgmo_dup_f() RETURNS int LANGUAGE sql AS 'SELECT 1'`,
		}, IsAlreadyExists, "IsAlreadyExists", false},
		{"duplicate role", []string{`DO $$ BEGIN EXECUTE pg_catalog.format('CREATE ROLE %I', current_user); END $$`}, IsAlreadyExists, "IsAlreadyExists", false},
		{"duplicate database", []string{`CREATE DATABASE template1`}, IsAlreadyExists, "IsAlreadyExists", true},
		{"permission denied", []string{`SET LOCAL ROLE pg_monitor`, `CREATE TABLE pg_catalog.pgmo_denied(i int)`}, IsPermissionDenied, "IsPermissionDenied", false},
		{"statement_timeout", []string{`SET LOCAL statement_timeout = 50`, `SELECT pg_catalog.pg_sleep(5)`}, IsQueryCanceled, "IsQueryCanceled", false},
		{"serialization failure", []string{`DO $$ BEGIN RAISE EXCEPTION USING ERRCODE = 'serialization_failure'; END $$`}, IsRetryable, "IsRetryable", false},
		{"deadlock", []string{`DO $$ BEGIN RAISE EXCEPTION USING ERRCODE = 'deadlock_detected'; END $$`}, IsRetryable, "IsRetryable", false},
	}
	all := map[string]func(error) bool{
		"IsUndefinedObject": IsUndefinedObject, "IsAlreadyExists": IsAlreadyExists,
		"IsPermissionDenied": IsPermissionDenied, "IsQueryCanceled": IsQueryCanceled,
		"IsRetryable": IsRetryable,
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			var err error
			if c.noTx {
				_, err = pool.Exec(ctx, c.sql[0])
			} else {
				err = pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
					for _, q := range c.sql {
						if _, err := tx.Exec(ctx, q); err != nil {
							return err
						}
					}
					return nil
				})
			}
			pe, ok := AsPgError(err)
			if !ok {
				t.Fatalf("%v: not a server error (%T)", err, err)
			}
			for name, pred := range all {
				if got, want := pred(err), name == c.pname; got != want {
					t.Errorf("%s(SQLSTATE %s %q) = %v, want %v", name, pe.Code, pe.Message, got, want)
				}
			}
		})
	}
}

// TestLiveTerminatedSessionIsRetryable kills a session from another one and
// pins that the victim's next statement fails retryably: the server sends
// 57P01 admin_shutdown, and pgx may surface it as that or as the dropped
// connection — IsRetryable must hold either way.
func TestLiveTerminatedSessionIsRetryable(t *testing.T) {
	pool, ctx, done := liveDB(t)
	defer done()
	victim, err := pgx.Connect(ctx, *liveConnString)
	if err != nil {
		t.Fatalf("connect: %v", err)
	}
	t.Cleanup(func() { victim.Close(context.Background()) })
	pid := victim.PgConn().PID()
	if _, err := pool.Exec(ctx, `SELECT pg_catalog.pg_terminate_backend($1)`, pid); err != nil {
		t.Fatalf("pg_terminate_backend(%d): %v", pid, err)
	}
	_, err = victim.Exec(ctx, `SELECT 1`)
	if err == nil {
		t.Fatal("statement on a terminated session succeeded")
	}
	if !IsRetryable(err) {
		t.Errorf("IsRetryable(%T %v) = false, want true", err, err)
	}
	if pe, ok := AsPgError(err); ok {
		t.Logf("server error %s: %s", pe.Code, pe.Message)
	} else {
		t.Logf("client error %T: %v", err, err)
	}
}
