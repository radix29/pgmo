//go:build livedb

package pgmo

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/radix29/pgmo/pgsql"
)

// TestLiveServerInfo reads Info as the connected superuser and, through SET
// LOCAL ROLE inside InTransaction, as a plain role (paths hidden) and as a
// member of pg_read_all_settings (paths shown). Each field is held to the
// server's answer read independently on the admin pool; the lab servers are
// self-hosted primaries.
func TestLiveServerInfo(t *testing.T) {
	admin, ctx, done := liveDB(t)
	t.Cleanup(done)
	stamp := time.Now().UnixNano()
	plain := fmt.Sprintf("pgmo_live_info_plain_%d", stamp)
	settings := fmt.Sprintf("pgmo_live_info_settings_%d", stamp)
	for _, stmt := range []string{
		"CREATE ROLE " + pgsql.QuoteIdent(plain) + " NOLOGIN",
		"CREATE ROLE " + pgsql.QuoteIdent(settings) + " NOLOGIN",
		"GRANT pg_read_all_settings TO " + pgsql.QuoteIdent(settings),
	} {
		if _, err := admin.Exec(ctx, stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	t.Cleanup(func() {
		for _, r := range []string{plain, settings} {
			if _, err := admin.Exec(context.Background(), "DROP ROLE IF EXISTS "+pgsql.QuoteIdent(r)); err != nil {
				t.Errorf("drop role %s: %v", r, err)
			}
		}
	})

	// want is the server's own answer, read without pgmo.
	var want ServerInfo
	var num int
	err := admin.QueryRow(ctx, `SELECT current_setting('server_version_num')::int, current_setting('server_version'), version(),
		pg_is_in_recovery(), current_setting('max_connections')::int, current_setting('superuser_reserved_connections')::int,
		pg_postmaster_start_time(), pg_conf_load_time(), current_setting('cluster_name'), current_setting('server_encoding'),
		current_setting('data_checksums') = 'on',
		current_setting('data_directory'), current_setting('config_file'), current_setting('hba_file')`).Scan(
		&num, &want.VersionString, &want.Banner, &want.InRecovery, &want.MaxConnections, &want.SuperuserReservedConnections,
		&want.StartTime, &want.ConfigLoadTime, &want.ClusterName, &want.Encoding, &want.DataChecksums,
		&want.DataDirectory, &want.ConfigFile, &want.HBAFile)
	if err != nil {
		t.Fatal(err)
	}
	want.Version = ServerVersion(num)
	if want.DataDirectory == "" || want.ConfigFile == "" || want.HBAFile == "" {
		t.Fatalf("admin read empty paths: %+v", want)
	}

	s, err := Connect(ctx, ConnectionOptions{ConnString: *liveConnString, ApplicationName: "pgmo - live server info"})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)

	// as reads Info as role ("" for the connected one).
	as := func(t *testing.T, role string) *ServerInfo {
		t.Helper()
		var i *ServerInfo
		err := s.InTransaction(ctx, func(ctx context.Context) error {
			if role != "" {
				if _, err := s.on(ctx).Exec(ctx, "SET LOCAL ROLE "+pgsql.QuoteIdent(role)); err != nil {
					return err
				}
			}
			var err error
			i, err = s.Info(ctx)
			return err
		})
		if err != nil {
			t.Fatalf("Info as %q: %v", role, err)
		}
		return i
	}

	// check holds got to want, the paths to shown or hidden.
	check := func(t *testing.T, got *ServerInfo, shown bool) {
		t.Helper()
		w := want
		if !shown {
			w.DataDirectory, w.ConfigFile, w.HBAFile, w.PathsHidden = "", "", "", true
		}
		if !got.StartTime.Equal(w.StartTime) || !got.ConfigLoadTime.Equal(w.ConfigLoadTime) {
			t.Errorf("times = %v, %v; want %v, %v", got.StartTime, got.ConfigLoadTime, w.StartTime, w.ConfigLoadTime)
		}
		g := *got
		g.StartTime, g.ConfigLoadTime = w.StartTime, w.ConfigLoadTime
		if g != w {
			t.Errorf("Info =\n%+v\nwant\n%+v", g, w)
		}
		if got.Version != s.Version() {
			t.Errorf("Version = %d, Server.Version = %d", got.Version, s.Version())
		}
		if got.InRecovery || got.IsManaged() {
			t.Errorf("lab primary read InRecovery %v, Managed %q", got.InRecovery, got.Managed)
		}
	}

	t.Run("superuser", func(t *testing.T) {
		got, err := s.Info(ctx) // outside a transaction too
		if err != nil {
			t.Fatal(err)
		}
		check(t, got, true)
	})
	t.Run("plain role", func(t *testing.T) { check(t, as(t, plain), false) })
	t.Run("pg_read_all_settings", func(t *testing.T) { check(t, as(t, settings), true) })
}
