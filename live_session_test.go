//go:build livedb

package pgmo

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

// TestLiveSessionNotices opens a session over -livedb and checks its notices
// arrive in order with its results: one raised per row before that row, one
// raised before an error before the error, each severity with its SQLSTATE,
// and none of another session's (the Server's pool's).
func TestLiveSessionNotices(t *testing.T) {
	if *liveConnString == "" {
		t.Skip("no -livedb connection string given")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	s, err := Connect(ctx, ConnectionOptions{ConnString: *liveConnString, ApplicationName: "pgmo - live session"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	var l eventLog
	var notices []*pgconn.Notice
	conn, err := s.OpenSession(ctx, "", func(n *pgconn.Notice) {
		notices = append(notices, n)
		l.notice(n)
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close(context.Background()) })

	// A temporary function: it ends with the session, nothing to drop.
	if _, err := conn.Exec(ctx, `CREATE FUNCTION pg_temp.noisy(i integer) RETURNS integer
		LANGUAGE plpgsql AS $$BEGIN RAISE NOTICE 'n%', i; RETURN i; END$$`); err != nil {
		t.Fatal(err)
	}
	notices = nil
	rows, err := conn.Query(ctx, `SELECT pg_temp.noisy(i) FROM pg_catalog.generate_series(1, 3) AS g(i)`)
	if err != nil {
		t.Fatal(err)
	}
	for rows.Next() {
		var n int
		if err := rows.Scan(&n); err != nil {
			t.Fatal(err)
		}
		l.add(fmt.Sprint("row ", n))
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	l.add("end")
	want := []string{"notice n1", "row 1", "notice n2", "row 2", "notice n3", "row 3", "end"}
	if got := l.take(); !slices.Equal(got, want) {
		t.Errorf("rows and notices:\n got %q\nwant %q", got, want)
	}
	if len(notices) > 0 {
		n := notices[0]
		if n.Severity != "NOTICE" || n.SeverityUnlocalized != "NOTICE" || n.Code != "00000" || !strings.Contains(n.Where, "noisy") {
			t.Errorf("notice fields: severity %q/%q, code %q, where %q", n.Severity, n.SeverityUnlocalized, n.Code, n.Where)
		}
	}

	notices = nil
	_, err = conn.Exec(ctx, `DO $$BEGIN
		RAISE INFO 'i' USING DETAIL = 'd', HINT = 'h';
		RAISE WARNING 'w';
		RAISE EXCEPTION 'boom';
	END$$`)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "P0001" {
		t.Fatalf("DO block: %v, want P0001", err)
	}
	l.add("error")
	if got := l.take(); !slices.Equal(got, []string{"notice i", "notice w", "error"}) {
		t.Errorf("notices before an error: %q", got)
	}
	if len(notices) == 2 {
		i, w := notices[0], notices[1]
		if i.SeverityUnlocalized != "INFO" || i.Code != "00000" || i.Detail != "d" || i.Hint != "h" {
			t.Errorf("INFO: severity %q, code %q, detail %q, hint %q", i.SeverityUnlocalized, i.Code, i.Detail, i.Hint)
		}
		if w.SeverityUnlocalized != "WARNING" || w.Code != "01000" {
			t.Errorf("WARNING: severity %q, code %q", w.SeverityUnlocalized, w.Code)
		}
	}

	// The pool's sessions are not this one: their notices go nowhere.
	if _, err := s.Querier().Exec(ctx, `DO $$BEGIN RAISE NOTICE 'pool'; END$$`); err != nil {
		t.Fatal(err)
	}
	if got := l.take(); got != nil {
		t.Errorf("the session's handler saw the pool's notices: %q", got)
	}
	var app string
	if err := conn.QueryRow(ctx, `SELECT pg_catalog.current_setting('application_name')`).Scan(&app); err != nil {
		t.Fatal(err)
	}
	if app != "pgmo - live session" {
		t.Errorf("the session's application_name = %q, want Connect's", app)
	}
}
