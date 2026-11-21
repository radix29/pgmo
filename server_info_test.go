package pgmo_test

import (
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/radix29/pgmo"
	"github.com/radix29/pgmo/pgmotest"
)

// serverInfoRow is one answer to the server info read, in its SELECT order.
// A nil path is a role that may not see it.
func serverInfoRow(dataDir, configFile, hbaFile any, roles []string, aurora bool) []any {
	return []any{
		int32(170002), "17.2 (Debian 17.2-1)", "PostgreSQL 17.2 on x86_64-pc-linux-gnu, 64-bit", true,
		int32(250), int32(5),
		time.Date(2026, 11, 20, 22, 16, 9, 0, time.UTC), time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC),
		"17/main", "UTF8", true,
		dataDir, configFile, hbaFile, roles, aurora,
	}
}

// Every field is filled from its own column — each column holds a value no
// other field could take — and the managed role names are sent as $1.
func TestServerInfoFillsEveryField(t *testing.T) {
	f := pgmotest.New(t)
	f.On("pg_postmaster_start_time", pgmotest.Rows(serverInfoRow(
		"/var/lib/postgresql/17/main", "/etc/postgresql/17/main/postgresql.conf", "/etc/postgresql/17/main/pg_hba.conf",
		[]string{"azure_pg_admin"}, false)))
	s := f.Server()

	got, err := s.Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	want := pgmo.ServerInfo{
		Version:        pgmo.ServerVersion(170002),
		VersionString:  "17.2 (Debian 17.2-1)",
		Banner:         "PostgreSQL 17.2 on x86_64-pc-linux-gnu, 64-bit",
		InRecovery:     true,
		MaxConnections: 250, SuperuserReservedConnections: 5,
		StartTime:      time.Date(2026, 11, 20, 22, 16, 9, 0, time.UTC),
		ConfigLoadTime: time.Date(2026, 11, 21, 8, 0, 0, 0, time.UTC),
		ClusterName:    "17/main", Encoding: "UTF8", DataChecksums: true,
		DataDirectory: "/var/lib/postgresql/17/main",
		ConfigFile:    "/etc/postgresql/17/main/postgresql.conf",
		HBAFile:       "/etc/postgresql/17/main/pg_hba.conf",
		Managed:       pgmo.ManagedAzure,
	}
	if !got.StartTime.Equal(want.StartTime) || !got.ConfigLoadTime.Equal(want.ConfigLoadTime) {
		t.Errorf("times = %v, %v; want %v, %v", got.StartTime, got.ConfigLoadTime, want.StartTime, want.ConfigLoadTime)
	}
	got.StartTime, got.ConfigLoadTime = want.StartTime, want.ConfigLoadTime
	if *got != want {
		t.Errorf("Info =\n%+v\nwant\n%+v", *got, want)
	}
	if !got.IsManaged() {
		t.Error("IsManaged = false for an Azure server")
	}

	var sentArg any
	for _, q := range f.Queries() {
		if strings.Contains(q.Text, "pg_postmaster_start_time") {
			sentArg = q.Arg(0)
		}
	}
	wantRoles := []string{"alloydbsuperuser", "azure_pg_admin", "cloudsqlsuperuser", "rds_superuser"}
	if got, ok := sentArg.([]string); !ok || !slices.Equal(got, wantRoles) {
		t.Errorf("$1 = %#v, want %v", sentArg, wantRoles)
	}
}

// A role that may not read the paths gets them NULL: empty, and PathsHidden.
func TestServerInfoHiddenPaths(t *testing.T) {
	f := pgmotest.New(t)
	f.On("pg_postmaster_start_time", pgmotest.Rows(serverInfoRow(nil, nil, nil, []string{}, false)))
	got, err := f.Server().Info(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	if !got.PathsHidden || got.DataDirectory != "" || got.ConfigFile != "" || got.HBAFile != "" {
		t.Errorf("paths = %q %q %q hidden %v; want empty and hidden", got.DataDirectory, got.ConfigFile, got.HBAFile, got.PathsHidden)
	}
	if got.Managed != pgmo.ManagedNone || got.IsManaged() {
		t.Errorf("Managed = %q, want none", got.Managed)
	}
}

// Each service's role names it; Aurora, which also creates rds_superuser, is
// told apart by aurora_version().
func TestServerInfoManagedService(t *testing.T) {
	for _, tc := range []struct {
		roles  []string
		aurora bool
		want   pgmo.ManagedService
	}{
		{nil, false, pgmo.ManagedNone},
		{[]string{"rds_superuser"}, false, pgmo.ManagedRDS},
		{[]string{"rds_superuser"}, true, pgmo.ManagedAurora},
		{[]string{"azure_pg_admin"}, false, pgmo.ManagedAzure},
		{[]string{"cloudsqlsuperuser"}, false, pgmo.ManagedCloudSQL},
		{[]string{"alloydbsuperuser"}, false, pgmo.ManagedAlloyDB},
	} {
		f := pgmotest.New(t)
		f.On("pg_postmaster_start_time", pgmotest.Rows(serverInfoRow("/d", "/c", "/h", tc.roles, tc.aurora)))
		got, err := f.Server().Info(t.Context())
		if err != nil {
			t.Fatal(err)
		}
		if got.Managed != tc.want || got.IsManaged() != (tc.want != pgmo.ManagedNone) {
			t.Errorf("roles %v aurora %v: Managed = %q, want %q", tc.roles, tc.aurora, got.Managed, tc.want)
		}
		if got.PathsHidden {
			t.Errorf("roles %v: PathsHidden with every path read", tc.roles)
		}
	}
}

// A failed read is named once and keeps the server's error.
func TestServerInfoError(t *testing.T) {
	f := pgmotest.New(t)
	f.On("pg_postmaster_start_time", pgmotest.Fail(&pgconn.PgError{Code: "42501", Message: "permission denied"}))
	_, err := f.Server().Info(t.Context())
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
		t.Fatalf("err = %v, want the 42501 wrapped", err)
	}
	if !strings.HasPrefix(err.Error(), "pgmo: read server info: ") {
		t.Errorf("err = %q, want the pgmo: read server info prefix", err)
	}
}

// A nil ServerInfo is not managed.
func TestServerInfoNilIsNotManaged(t *testing.T) {
	var i *pgmo.ServerInfo
	if i.IsManaged() {
		t.Error("nil ServerInfo reads managed")
	}
}
