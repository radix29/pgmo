package pgmo

import "time"

// ConnectionOptions holds every parameter needed to open a connection.
//
// Each field is a libpq connection parameter under its Go name; the zero
// value of a field leaves that parameter to the rest of libpq's resolution
// order, as psql would:
//
//	explicit field → ConnString → service file → PG* environment → .pgpass
//
// so a field set here always wins over the same parameter written in
// ConnString, a service entry or the environment, and an empty field defers
// to them. Nothing is merged silently: a Params key that a field controls is
// refused rather than overriding it.
//
// Quick guide:
//
// A libpq service (~/.pg_service.conf), password from ~/.pgpass:
//
//	Service: "pg18"
//
// Fields:
//
//	Host: "db1.example.com", Port: 5432, Database: "app", User: "alice",
//	SSLMode: SSLModeVerifyFull, SSLRootCert: "/etc/ssl/certs/ca.pem"
//
// A connection string, keyword/value or URI, with one field overridden:
//
//	ConnString: "postgres://alice@db1.example.com/app?sslmode=require",
//	ApplicationName: "pgMS - Query"
//
// Multi-host, first host accepting writes:
//
//	Host: "db1.example.com,db2.example.com",
//	TargetSessionAttrs: TargetSessionReadWrite
type ConnectionOptions struct {
	// -- Target ------------------------------------------------------------------

	// Host is the server's host name, IP address, or Unix-domain socket
	// directory (an absolute path), as libpq's host. A comma-separated list
	// names several servers, tried in order until one accepts the connection
	// and satisfies TargetSessionAttrs. Empty defers to ConnString, the
	// service, PGHOST, then libpq's default (the local socket directory, or
	// localhost where there is none).
	Host string

	// Port is the TCP port, or the socket file's number for a Unix-domain
	// socket, used for every host in Host. Zero defers to ConnString, the
	// service, PGPORT, then 5432. Hosts on different ports are written in
	// ConnString ("host=a,b port=5432,5433", or "postgres://a:5432,b:5433/").
	// Outside 1-65535 is an error.
	Port int

	// Database is the database to connect to — the only one this connection
	// can read: PostgreSQL has no USE, so another database is reached through
	// its own pool. Empty defers to ConnString, the service, PGDATABASE, then
	// libpq's default of the user name.
	Database string

	// -- Authentication ----------------------------------------------------------

	// User is the role to connect as. Empty defers to ConnString, the
	// service, PGUSER, then the operating-system user name.
	User string

	// Password is the role's password. The method (SCRAM-SHA-256, md5 or
	// cleartext) is the one the server asks for; the driver negotiates it.
	// Empty defers to ConnString, PGPASSWORD, then the password file
	// (PGPASSFILE, else ~/.pgpass, %APPDATA%\postgresql\pgpass.conf on
	// Windows). Prefer the password file to this field wherever a person
	// is not typing the password in.
	Password string

	// Service names an entry in the connection service file (PGSERVICEFILE,
	// else ~/.pg_service.conf, %APPDATA%\postgresql\.pg_service.conf on
	// Windows), whose parameters apply wherever neither a
	// field nor ConnString sets them. Empty defers to ConnString's service,
	// then PGSERVICE.
	Service string

	// -- TLS ---------------------------------------------------------------------

	// SSLMode chooses whether, and how strictly, the connection uses TLS;
	// see SSLMode. Empty defers to ConnString, the service, PGSSLMODE, then
	// SSLModePrefer.
	SSLMode SSLMode

	// SSLRootCert is the file of trusted certificate authorities that
	// SSLModeVerifyCA and SSLModeVerifyFull check the server's certificate
	// against. Empty defers to ConnString, the service, PGSSLROOTCERT, then
	// ~/.postgresql/root.crt (%APPDATA%\postgresql\root.crt on Windows)
	// if it exists.
	SSLRootCert string

	// SSLCert is the client certificate presented for certificate
	// authentication. Set with SSLKey, or neither. Empty defers to
	// ConnString, the service, PGSSLCERT, then postgresql.crt beside the
	// default root.crt, if it exists.
	SSLCert string

	// SSLKey is the private key of SSLCert. Set with SSLCert, or neither.
	// Empty defers to ConnString, the service, PGSSLKEY, then
	// postgresql.key beside the default root.crt, if it exists.
	SSLKey string

	// -- Session -----------------------------------------------------------------

	// ApplicationName is shown in pg_stat_activity.application_name and the
	// server log. Empty defers to ConnString, the service, PGAPPNAME, then
	// "pgmo".
	ApplicationName string

	// ConnectTimeout is the most time to wait for each host to accept the
	// connection. libpq takes whole seconds, so it is rounded up to a
	// second. Zero defers to ConnString, the service, PGCONNECT_TIMEOUT,
	// then no limit beyond the context's; negative is an error.
	ConnectTimeout time.Duration

	// TargetSessionAttrs chooses which of Host's servers is acceptable; see
	// TargetSessionAttrs. Empty defers to ConnString, the service,
	// PGTARGETSESSIONATTRS, then TargetSessionAny.
	TargetSessionAttrs TargetSessionAttrs

	// Params carries server runtime parameters to set on every connection
	// at startup, as libpq's options: search_path, statement_timeout,
	// DateStyle, lock_timeout and the like (pgmo's own catalog reads are
	// schema-qualified and unaffected by search_path). A key that a field of
	// ConnectionOptions controls — host, port, dbname, user, password,
	// service, sslmode, sslrootcert, sslcert, sslkey, application_name,
	// connect_timeout, target_session_attrs — is refused rather than merged,
	// so which value wins never depends on parsing order.
	Params map[string]string

	// ConnString is a libpq connection string, keyword/value
	// ("host=db1 dbname=app sslmode=require") or URI
	// ("postgres://alice@db1/app?sslmode=require"), as psql accepts. A
	// parameter it sets yields to the same parameter set by a field, and
	// wins over the service, the environment and the password file.
	ConnString string
}

// SSLMode is libpq's sslmode: whether the connection uses TLS, and how much
// of the server's certificate it checks.
type SSLMode string

// The sslmode values, weakest first.
const (
	// SSLModeDisable never uses TLS.
	SSLModeDisable SSLMode = "disable"

	// SSLModeAllow tries without TLS first, then with it.
	SSLModeAllow SSLMode = "allow"

	// SSLModePrefer tries TLS first, then without it. libpq's default.
	SSLModePrefer SSLMode = "prefer"

	// SSLModeRequire uses TLS without checking the certificate — unless a
	// root certificate file is given (SSLRootCert, or the default file
	// exists), when it checks it as SSLModeVerifyCA does, as libpq does.
	SSLModeRequire SSLMode = "require"

	// SSLModeVerifyCA uses TLS and checks that a trusted authority signed
	// the server's certificate.
	SSLModeVerifyCA SSLMode = "verify-ca"

	// SSLModeVerifyFull is SSLModeVerifyCA plus a check that the certificate
	// names the host connected to — the only mode that stops an impostor
	// holding any certificate from the same authority.
	SSLModeVerifyFull SSLMode = "verify-full"
)

// TargetSessionAttrs is libpq's target_session_attrs: which server a
// multi-host connection accepts.
type TargetSessionAttrs string

// The target_session_attrs values.
const (
	// TargetSessionAny accepts the first server that answers. libpq's
	// default.
	TargetSessionAny TargetSessionAttrs = "any"

	// TargetSessionReadWrite accepts a server whose sessions take writes
	// by default (default_transaction_read_only off, not in recovery).
	TargetSessionReadWrite TargetSessionAttrs = "read-write"

	// TargetSessionReadOnly accepts a server whose sessions do not take
	// writes by default.
	TargetSessionReadOnly TargetSessionAttrs = "read-only"

	// TargetSessionPrimary accepts a server not in recovery.
	TargetSessionPrimary TargetSessionAttrs = "primary"

	// TargetSessionStandby accepts a server in recovery.
	TargetSessionStandby TargetSessionAttrs = "standby"

	// TargetSessionPreferStandby accepts a standby if any host is one,
	// else any server.
	TargetSessionPreferStandby TargetSessionAttrs = "prefer-standby"
)
