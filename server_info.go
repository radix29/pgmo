package pgmo

// server_info.go is what the connected server is: its version and build, its
// role in replication, its limits and start time, where it keeps its files
// (for a role allowed to see that) and which managed service runs it, if any
// (Server.Info).

import (
	"context"
	"fmt"
	"slices"
	"time"
)

// ManagedService names the managed PostgreSQL offering a server runs on, as
// far as its catalog betrays it. A managed server has no superuser to hand
// out: its administrator role is a member of the service's own role instead,
// and the server's files, logs and configuration are the service's.
type ManagedService string

// The managed services ServerInfo recognises. ManagedNone is a server that
// shows none of their marks — self-hosted, or a service pgmo does not know.
const (
	ManagedNone     ManagedService = ""
	ManagedRDS      ManagedService = "rds"      // Amazon RDS for PostgreSQL
	ManagedAurora   ManagedService = "aurora"   // Amazon Aurora PostgreSQL
	ManagedAzure    ManagedService = "azure"    // Azure Database for PostgreSQL
	ManagedCloudSQL ManagedService = "cloudsql" // Google Cloud SQL for PostgreSQL
	ManagedAlloyDB  ManagedService = "alloydb"  // Google AlloyDB
)

// managedRoles maps the administrator role each service creates to the
// service. Aurora creates RDS's role too; aurora_version() tells it apart.
var managedRoles = map[string]ManagedService{
	"rds_superuser":     ManagedRDS,
	"azure_pg_admin":    ManagedAzure,
	"cloudsqlsuperuser": ManagedCloudSQL,
	"alloydbsuperuser":  ManagedAlloyDB,
}

// ServerInfo is what the connected server is, read by Server.Info. Every
// field but the three paths is readable by any role that can connect.
type ServerInfo struct {
	// Version is server_version_num, as Server.Version.
	Version ServerVersion

	// VersionString is server_version: the release with the packager's
	// suffix, "17.2 (Ubuntu 17.2-1.pgdg24.04+1)". Fit for a label.
	VersionString string

	// Banner is version(): "PostgreSQL 17.2 … on x86_64-pc-linux-gnu,
	// compiled by …, 64-bit" — the build's platform and compiler, on one
	// line but too long for a fixed-width label/value row.
	Banner string

	// InRecovery is pg_is_in_recovery(): the server is a standby (or still
	// replaying WAL after a crash or a restore) and refuses every write. It
	// changes on promotion, so Info reads it afresh each call.
	InRecovery bool

	// MaxConnections and SuperuserReservedConnections are the settings of
	// those names: the server's connection limit, and the slots of it only
	// superusers may take.
	MaxConnections, SuperuserReservedConnections int

	// StartTime is pg_postmaster_start_time(); ConfigLoadTime is
	// pg_conf_load_time(), when the configuration files were last read (at
	// start, or the last reload).
	StartTime, ConfigLoadTime time.Time

	// ClusterName is cluster_name ("17/main" on Debian's packages), empty
	// where it is not set. Encoding is server_encoding.
	ClusterName, Encoding string

	// DataChecksums is data_checksums: whether the cluster was initialised
	// with page checksums.
	DataChecksums bool

	// DataDirectory, ConfigFile and HBAFile are the settings data_directory,
	// config_file and hba_file — paths on the server's host, not the
	// caller's. Only a superuser or a role holding pg_read_all_settings may
	// read them; for any other they are empty and PathsHidden is set.
	DataDirectory, ConfigFile, HBAFile string

	// PathsHidden reports that the role may not read the three paths. Empty
	// paths with it set mean "not shown to this role", not "none".
	PathsHidden bool

	// Managed is the managed service the server runs on, from the
	// administrator role the service creates (and, for Aurora, its
	// aurora_version() function). A hint, not a guarantee: a self-hosted
	// server could create a role of the same name.
	Managed ManagedService
}

// IsManaged reports whether the server shows the marks of a managed service.
func (i *ServerInfo) IsManaged() bool { return i != nil && i.Managed != ManagedNone }

// serverInfoQuery reads ServerInfo in one row. The paths come from
// pg_settings rather than current_setting, which raises for a role that may
// not see them: the view leaves their setting NULL instead. integer and
// boolean are keyword types; data_checksums reads "on"/"off", which boolean
// takes. The managed roles are $1, so the query and managedRoles cannot
// drift apart.
const serverInfoQuery = `SELECT pg_catalog.current_setting('server_version_num')::integer,
	pg_catalog.current_setting('server_version'),
	pg_catalog.version(),
	pg_catalog.pg_is_in_recovery(),
	pg_catalog.current_setting('max_connections')::integer,
	pg_catalog.current_setting('superuser_reserved_connections')::integer,
	pg_catalog.pg_postmaster_start_time(),
	pg_catalog.pg_conf_load_time(),
	pg_catalog.current_setting('cluster_name'),
	pg_catalog.current_setting('server_encoding'),
	pg_catalog.current_setting('data_checksums')::boolean,
	(SELECT s.setting FROM pg_catalog.pg_settings s WHERE s.name = 'data_directory'),
	(SELECT s.setting FROM pg_catalog.pg_settings s WHERE s.name = 'config_file'),
	(SELECT s.setting FROM pg_catalog.pg_settings s WHERE s.name = 'hba_file'),
	ARRAY(SELECT r.rolname::pg_catalog.text FROM pg_catalog.pg_roles r
		WHERE r.rolname::pg_catalog.text = ANY ($1::pg_catalog.text[]) ORDER BY 1),
	EXISTS (SELECT FROM pg_catalog.pg_proc p WHERE p.proname = 'aurora_version')`

// managedRoleNames is managedRoles' keys, sorted: serverInfoQuery's $1.
func managedRoleNames() []string {
	names := make([]string, 0, len(managedRoles))
	for name := range managedRoles {
		names = append(names, name)
	}
	slices.Sort(names)
	return names
}

// managedService is the service the roles found (sorted) and the presence of
// aurora_version() point to. Aurora first: it creates rds_superuser too.
func managedService(roles []string, aurora bool) ManagedService {
	if aurora {
		return ManagedAurora
	}
	for _, name := range roles {
		if m, ok := managedRoles[name]; ok {
			return m
		}
	}
	return ManagedNone
}

// Info reads what the connected server is. It reads afresh on every call —
// InRecovery changes on promotion, ConfigLoadTime on a reload — so a caller
// that shows it repeatedly keeps its own copy. Inside InTransaction it reads
// the transaction's session, so a SET LOCAL ROLE there decides whether the
// paths are shown.
func (s *Server) Info(ctx context.Context) (*ServerInfo, error) {
	var i ServerInfo
	var num int
	var dataDir, configFile, hbaFile *string
	var roles []string
	var aurora bool
	err := queryRowScan(ctx, s.on(ctx), serverInfoQuery, []any{managedRoleNames()},
		&num, &i.VersionString, &i.Banner, &i.InRecovery,
		&i.MaxConnections, &i.SuperuserReservedConnections,
		&i.StartTime, &i.ConfigLoadTime, &i.ClusterName, &i.Encoding, &i.DataChecksums,
		&dataDir, &configFile, &hbaFile, &roles, &aurora)
	if err != nil {
		return nil, fmt.Errorf("pgmo: read server info: %w", err)
	}
	i.Version = ServerVersion(num)
	i.PathsHidden = dataDir == nil
	for _, p := range []struct{ dst, src *string }{{&i.DataDirectory, dataDir}, {&i.ConfigFile, configFile}, {&i.HBAFile, hbaFile}} {
		if p.src != nil {
			*p.dst = *p.src
		}
	}
	i.Managed = managedService(roles, aurora)
	return &i, nil
}
