package pgmo

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"io/fs"
	"maps"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

// libpqFixture isolates the libpq environment (clearLibpqEnv) and points
// PGSERVICEFILE at a service file with two entries, [svc] and [svc2] (only
// a search_path), and PGPASSFILE at a
// password file matching everything.
func libpqFixture(t *testing.T) {
	t.Helper()
	clearLibpqEnv(t)
	dir := t.TempDir()
	service := filepath.Join(dir, "pg_service.conf")
	pass := filepath.Join(dir, "pgpass")
	writeFile(t, service, "[svc]\nhost=svc-host\nport=6001\ndbname=svc-db\nuser=svc-user\n"+
		"application_name=svc-app\nconnect_timeout=61\nsslmode=disable\ntarget_session_attrs=standby\n"+
		"search_path=svc-path\n[svc2]\nsearch_path=svc2-path\n")
	writeFile(t, pass, "*:*:*:*:file-pass\n")
	t.Setenv("PGSERVICEFILE", service)
	t.Setenv("PGPASSFILE", pass)
}

func writeFile(t *testing.T, path, text string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(text), 0o600); err != nil {
		t.Fatal(err)
	}
}

// setEnv sets the PG* variable of every parameter the resolution tests read.
func setEnv(t *testing.T) {
	t.Helper()
	for k, v := range map[string]string{
		"PGHOST": "env-host", "PGPORT": "6000", "PGDATABASE": "env-db",
		"PGUSER": "env-user", "PGAPPNAME": "env-app", "PGCONNECT_TIMEOUT": "60",
		"PGSSLMODE": "disable", "PGTARGETSESSIONATTRS": "primary",
	} {
		t.Setenv(k, v)
	}
}

// resolved is the part of a parsed config the resolution tests compare.
type resolved struct {
	Host, Database, User, Password, App string
	Port                                uint16
	Timeout                             time.Duration
	TargetSession                       string // which ValidateConnect is set, by name
}

func resolve(t *testing.T, o ConnectionOptions) resolved {
	t.Helper()
	cfg, err := o.poolConfig()
	if err != nil {
		t.Fatalf("poolConfig(%+v): %v", o, err)
	}
	c := cfg.ConnConfig.Config
	return resolved{
		Host: c.Host, Database: c.Database, User: c.User, Password: c.Password,
		App: c.RuntimeParams["application_name"], Port: c.Port,
		Timeout: c.ConnectTimeout, TargetSession: targetSessionOf(c.ValidateConnect),
	}
}

// targetSessionOf names the target_session_attrs a ValidateConnect
// implements, by testing it against the pgx function for each.
func targetSessionOf(f pgconn.ValidateConnectFunc) string {
	if f == nil {
		return "any"
	}
	got := funcName(f)
	for name, g := range map[string]pgconn.ValidateConnectFunc{
		"read-write":     pgconn.ValidateConnectTargetSessionAttrsReadWrite,
		"read-only":      pgconn.ValidateConnectTargetSessionAttrsReadOnly,
		"primary":        pgconn.ValidateConnectTargetSessionAttrsPrimary,
		"standby":        pgconn.ValidateConnectTargetSessionAttrsStandby,
		"prefer-standby": pgconn.ValidateConnectTargetSessionAttrsPreferStandby,
	} {
		if funcName(g) == got {
			return name
		}
	}
	return "unknown"
}

// TestResolutionOrder walks libpq's order one layer at a time, from the
// environment up to the fields, and asserts what pgx parses at each: every
// layer must win over every layer below it, for every parameter a field
// controls, and an empty field must defer.
func TestResolutionOrder(t *testing.T) {
	envOnly := resolved{
		Host: "env-host", Port: 6000, Database: "env-db", User: "env-user",
		Password: "file-pass", App: "env-app", Timeout: 60 * time.Second, TargetSession: "primary",
	}
	service := resolved{
		Host: "svc-host", Port: 6001, Database: "svc-db", User: "svc-user",
		Password: "file-pass", App: "svc-app", Timeout: 61 * time.Second, TargetSession: "standby",
	}
	connString := resolved{
		Host: "cs-host", Port: 6002, Database: "cs-db", User: "cs-user",
		Password: "cs-pass", App: "cs-app", Timeout: 62 * time.Second, TargetSession: "read-only",
	}
	fields := resolved{
		Host: "field-host", Port: 6003, Database: "field-db", User: "field-user",
		Password: "field-pass", App: "field-app", Timeout: 63 * time.Second, TargetSession: "read-write",
	}
	fieldOpts := ConnectionOptions{
		Host: "field-host", Port: 6003, Database: "field-db", User: "field-user",
		Password: "field-pass", ApplicationName: "field-app", ConnectTimeout: 63 * time.Second,
		TargetSessionAttrs: TargetSessionReadWrite, SSLMode: SSLModeDisable,
	}
	const kv = "service=svc host=cs-host port=6002 dbname=cs-db user=cs-user password=cs-pass " +
		"application_name=cs-app connect_timeout=62 target_session_attrs=read-only"
	const uri = "postgres://cs-user:cs-pass@cs-host:6002/cs-db?service=svc&application_name=cs-app" +
		"&connect_timeout=62&target_session_attrs=read-only"

	for _, tc := range []struct {
		name     string
		pgsvc    bool // PGSERVICE=svc
		opts     ConnectionOptions
		expected resolved
	}{
		{"environment", false, ConnectionOptions{}, envOnly},
		{"service from PGSERVICE", true, ConnectionOptions{}, service},
		{"service from the field", false, ConnectionOptions{Service: "svc"}, service},
		{"keyword/value ConnString", true, ConnectionOptions{ConnString: kv}, connString},
		{"URI ConnString", true, ConnectionOptions{ConnString: uri}, connString},
		{"fields over keyword/value", true, withConnString(fieldOpts, kv), fields},
		{"fields over URI", true, withConnString(fieldOpts, uri), fields},
		{"fields alone", true, fieldOpts, fields},
	} {
		t.Run(tc.name, func(t *testing.T) {
			libpqFixture(t)
			setEnv(t)
			if tc.pgsvc {
				t.Setenv("PGSERVICE", "svc")
			}
			if got := resolve(t, tc.opts); got != tc.expected {
				t.Errorf("\n got %+v\nwant %+v", got, tc.expected)
			}
		})
	}
}

func withConnString(o ConnectionOptions, s string) ConnectionOptions {
	o.ConnString = s
	return o
}

// TestPasswordResolution: the field, then ConnString, then PGPASSWORD, then
// the password file — and the field wins over a ConnString password though
// it is never written into the string.
func TestPasswordResolution(t *testing.T) {
	for _, tc := range []struct {
		name, pgpassword string
		opts             ConnectionOptions
		expected         string
	}{
		{"password file", "", ConnectionOptions{Host: "h"}, "file-pass"},
		{"PGPASSWORD", "env-pass", ConnectionOptions{Host: "h"}, "env-pass"},
		{"ConnString", "env-pass", ConnectionOptions{ConnString: "host=h password=cs-pass"}, "cs-pass"},
		{"URI", "env-pass", ConnectionOptions{ConnString: "postgres://u:cs%20pass@h/"}, "cs pass"},
		{"field", "env-pass", ConnectionOptions{ConnString: "host=h password=cs-pass", Password: "f'p\\w"}, "f'p\\w"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			libpqFixture(t)
			if tc.pgpassword != "" {
				t.Setenv("PGPASSWORD", tc.pgpassword)
			}
			tc.opts.SSLMode = SSLModeDisable
			if got := resolve(t, tc.opts).Password; got != tc.expected {
				t.Errorf("password = %q, want %q", got, tc.expected)
			}
		})
	}
}

// TestFieldValuesRoundTrip writes awkward values through both ConnString
// forms and none, and holds pgx's parse to the value as given: quoting,
// backslashes, the URI's reserved characters, a percent sign, non-ASCII.
func TestFieldValuesRoundTrip(t *testing.T) {
	values := []string{
		"plain", "with space", "it's", `back\slash`, `trailing\`, `'\'`,
		"a&b=c?d#e/f:g,h@i%20j+k", "café ü 東京", " lead and trail ",
	}
	for _, cs := range []string{"", "sslmode=disable", "postgres://h/?sslmode=disable",
		"postgres://u:p%3Fw@h/d", "postgresql://u:p?w@h", `host=h dbname=x\`} {
		for _, v := range values {
			libpqFixture(t)
			o := ConnectionOptions{
				ConnString: cs, Host: "h", SSLMode: SSLModeDisable,
				Database: v, User: v, ApplicationName: v,
				Params: map[string]string{"search_path": v},
			}
			cfg, err := o.poolConfig()
			if err != nil {
				t.Errorf("ConnString %q, value %q: %v", cs, v, err)
				continue
			}
			c := cfg.ConnConfig.Config
			for name, got := range map[string]string{
				"database": c.Database, "user": c.User,
				"application_name": c.RuntimeParams["application_name"],
				"search_path":      c.RuntimeParams["search_path"],
			} {
				if got != v {
					t.Errorf("ConnString %q: %s = %q, want %q", cs, name, got, v)
				}
			}
		}
	}
}

// TestURIUserInfoSurvives: a '?' unescaped in a URI's password is not its
// query, so the fields start one, and the password is still the URI's.
func TestURIUserInfoSurvives(t *testing.T) {
	libpqFixture(t)
	r := resolve(t, ConnectionOptions{ConnString: "postgres://u:p?w@h", Database: "d", SSLMode: SSLModeDisable})
	if r.Host != "h" || r.User != "u" || r.Password != "p?w" || r.Database != "d" {
		t.Errorf("got %+v", r)
	}
}

// TestConnectTimeoutRoundsUp: libpq takes whole seconds; a fraction rounds up
// so a short timeout never becomes none.
func TestConnectTimeoutRoundsUp(t *testing.T) {
	for in, expected := range map[time.Duration]time.Duration{
		time.Nanosecond:         time.Second,
		time.Second:             time.Second,
		1500 * time.Millisecond: 2 * time.Second,
		90 * time.Second:        90 * time.Second,
	} {
		libpqFixture(t)
		got := resolve(t, ConnectionOptions{Host: "h", SSLMode: SSLModeDisable, ConnectTimeout: in}).Timeout
		if got != expected {
			t.Errorf("ConnectTimeout %v parsed as %v, want %v", in, got, expected)
		}
	}
}

// TestApplicationNameDefault: "pgmo" only when nothing else names one.
func TestApplicationNameDefault(t *testing.T) {
	libpqFixture(t)
	if got := resolve(t, ConnectionOptions{Host: "h", SSLMode: SSLModeDisable}).App; got != defaultApplicationName {
		t.Errorf("application_name = %q, want %q", got, defaultApplicationName)
	}
	t.Setenv("PGAPPNAME", "env-app")
	if got := resolve(t, ConnectionOptions{Host: "h", SSLMode: SSLModeDisable}).App; got != "env-app" {
		t.Errorf("application_name = %q, want PGAPPNAME's", got)
	}
}

// TestParamsAreRuntimeParams: Params reach the startup packet as written,
// over the same parameter set in ConnString.
func TestParamsAreRuntimeParams(t *testing.T) {
	libpqFixture(t)
	cfg, err := ConnectionOptions{
		ConnString: "host=h sslmode=disable statement_timeout=1s",
		Params:     map[string]string{"statement_timeout": "5s", "pg_stat_statements.track": "all"},
	}.poolConfig()
	if err != nil {
		t.Fatal(err)
	}
	rp := cfg.ConnConfig.RuntimeParams
	if rp["statement_timeout"] != "5s" || rp["pg_stat_statements.track"] != "all" {
		t.Errorf("RuntimeParams = %v", rp)
	}
}

// TestFieldRefusals: each refusal is decided before pgx sees anything,
// wraps ErrInvalidRequest, and names the field the caller set.
func TestFieldRefusals(t *testing.T) {
	libpqFixture(t)
	for _, tc := range []struct {
		opts ConnectionOptions
		says string
	}{
		{ConnectionOptions{Port: -1}, "ConnectionOptions.Port -1 is outside 1-65535"},
		{ConnectionOptions{Port: 65536}, "ConnectionOptions.Port 65536"},
		{ConnectionOptions{ConnectTimeout: -time.Second}, "ConnectionOptions.ConnectTimeout -1s is negative"},
		{ConnectionOptions{SSLMode: "verify_full"}, `ConnectionOptions.SSLMode "verify_full" is not one of "disable"`},
		{ConnectionOptions{TargetSessionAttrs: "readwrite"}, `ConnectionOptions.TargetSessionAttrs "readwrite"`},
		{ConnectionOptions{SSLCert: "c.crt"}, "ConnectionOptions.SSLCert and SSLKey"},
		{ConnectionOptions{SSLKey: "c.key"}, "ConnectionOptions.SSLCert and SSLKey"},
		{ConnectionOptions{User: "a\x00b"}, "ConnectionOptions.User holds a NUL byte"},
		{ConnectionOptions{ConnString: "host=a\x00"}, "ConnectionOptions.ConnString holds a NUL byte"},
		{ConnectionOptions{Params: map[string]string{"host": "x"}}, `Params key "host" is set through ConnectionOptions.Host`},
		{ConnectionOptions{Params: map[string]string{"DBName": "x"}}, `Params key "DBName" is set through ConnectionOptions.Database`},
		{ConnectionOptions{Params: map[string]string{"database": "x"}}, `through ConnectionOptions.Database`},
		{ConnectionOptions{Params: map[string]string{"Application_Name": "x"}}, `through ConnectionOptions.ApplicationName`},
		{ConnectionOptions{Params: map[string]string{"passfile": "x"}}, `Params key "passfile" is a connection parameter`},
		{ConnectionOptions{Params: map[string]string{"pool_max_conns": "4"}}, `"pool_max_conns" is a connection parameter`},
		{ConnectionOptions{Params: map[string]string{"options": "-c x=y"}}, `"options" is a connection parameter`},
		{ConnectionOptions{Params: map[string]string{"replication": "database"}}, `"replication" is a connection parameter`},
		{ConnectionOptions{Params: map[string]string{"": "x"}}, `Params key "" is not a runtime parameter name`},
		{ConnectionOptions{Params: map[string]string{"a b": "x"}}, `Params key "a b" is not`},
		{ConnectionOptions{Params: map[string]string{"a=b": "x"}}, `Params key "a=b" is not`},
		{ConnectionOptions{Params: map[string]string{"work_mem": "1MB", "WORK_MEM": "2MB"}}, `keys "WORK_MEM" and "work_mem" name the same parameter`},
		{ConnectionOptions{Params: map[string]string{"work_mem": "1\x00"}}, `Params["work_mem"] holds a NUL byte`},
	} {
		_, err := tc.opts.poolConfig()
		switch {
		case err == nil:
			t.Errorf("%+v: no error, want %q", tc.opts, tc.says)
		case !errors.Is(err, ErrInvalidRequest):
			t.Errorf("%+v: %v does not wrap ErrInvalidRequest", tc.opts, err)
		case !strings.HasPrefix(err.Error(), "pgmo: connection options: ") || !strings.Contains(err.Error(), tc.says):
			t.Errorf("%+v: error %q, want it to say %q", tc.opts, err, tc.says)
		}
	}
}

// TestFieldsAcceptedAtTheirLimits: the edges the refusals stop short of.
func TestFieldsAcceptedAtTheirLimits(t *testing.T) {
	libpqFixture(t)
	for _, o := range []ConnectionOptions{
		{Port: 1}, {Port: 65535}, {ConnectTimeout: 0},
		{SSLCert: "c.crt", SSLKey: "c.key", SSLMode: SSLModeDisable}, // not read: no TLS
		{Params: map[string]string{"pg_stat_statements.track": "all", "TimeZone": "UTC"}},
	} {
		o.Host = "h"
		if o.SSLMode == "" {
			o.SSLMode = SSLModeDisable
		}
		if _, err := o.poolConfig(); err != nil {
			t.Errorf("%+v: %v", o, err)
		}
	}
}

// TestParseFailureKeepsThePasswordOut: a ConnString pgx cannot parse fails as
// ErrInvalidRequest with pgx's reason, the Password field appears nowhere
// in it, and the *pgconn.ParseConfigError, whose ConnString is unredacted,
// is not reachable from it.
func TestParseFailureKeepsThePasswordOut(t *testing.T) {
	libpqFixture(t)
	for _, cs := range []string{"host=h sslmode=bogus", "postgres://u:cs-secret@h:notaport/", "host='unterminated"} {
		_, err := ConnectionOptions{ConnString: cs, Password: "field-secret"}.poolConfig()
		if err == nil {
			t.Errorf("%q: no error", cs)
			continue
		}
		if !errors.Is(err, ErrInvalidRequest) || !strings.HasPrefix(err.Error(), "pgmo: connection options: ") {
			t.Errorf("%q: %v", cs, err)
		}
		if strings.Contains(err.Error(), "secret") {
			t.Errorf("%q: error quotes a password: %v", cs, err)
		}
		if _, ok := errors.AsType[*pgconn.ParseConfigError](err); ok {
			t.Errorf("%q: the ParseConfigError is reachable", cs)
		}
	}
}

// TestParseFailureKeepsItsCause: a cause under pgx's error (a missing CA
// file) stays reachable, and a missing service fails before dialling.
func TestParseFailureKeepsItsCause(t *testing.T) {
	libpqFixture(t)
	_, err := ConnectionOptions{
		Host: "h", SSLMode: SSLModeVerifyFull, SSLRootCert: filepath.Join(t.TempDir(), "none.crt"),
	}.poolConfig()
	if !errors.Is(err, fs.ErrNotExist) || !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("missing CA file: %v", err)
	}
	_, err = ConnectionOptions{Service: "nosuch"}.poolConfig()
	if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "nosuch") {
		t.Errorf("missing service: %v", err)
	}
}

// TestConnectionKeywordsAreNotRuntimeParams holds connectionKeywords to pgx:
// every keyword pgx, pgx or pgxpool reads itself is either refused with a
// bogus value or kept out of RuntimeParams. The three exceptions pgx does
// send as startup parameters are asserted to be sent, so the list's comment
// stays true.
func TestConnectionKeywordsAreNotRuntimeParams(t *testing.T) {
	libpqFixture(t)
	sent := map[string]bool{"options": true, "replication": true}
	for _, k := range connectionKeywords {
		if k == "ssl" { // URI-only alias; in keyword/value it is a plain key
			cfg, err := pgxpool.ParseConfig("postgres://h/?ssl=true")
			if err != nil || cfg.ConnConfig.RuntimeParams["ssl"] != "" {
				t.Errorf("ssl=true: %v, %v", err, cfg.ConnConfig.RuntimeParams)
			}
			continue
		}
		cfg, err := pgxpool.ParseConfig("host=h sslmode=disable " + k + "=1")
		if err != nil {
			continue // pgx read it and refused the value
		}
		_, inRuntime := cfg.ConnConfig.RuntimeParams[k]
		if inRuntime != sent[k] {
			t.Errorf("%s: in RuntimeParams %v, want %v", k, inRuntime, sent[k])
		}
	}
	// The check can fail: a setting is a runtime parameter.
	cfg, err := pgxpool.ParseConfig("host=h sslmode=disable work_mem=1")
	if err != nil || cfg.ConnConfig.RuntimeParams["work_mem"] != "1" {
		t.Error("work_mem=1 is not a runtime parameter: the check cannot fail")
	}
}

// TestFieldKeywordsAreTheFields pins fieldKeywords to the fields: each named
// field exists, and each field but ConnString and Params has a keyword.
func TestFieldKeywordsAreTheFields(t *testing.T) {
	named := map[string]bool{}
	for _, f := range fieldKeywords {
		named[f] = true
	}
	for _, f := range exportedFields(ConnectionOptions{}) {
		if f == "ConnString" || f == "Params" {
			continue
		}
		if !named[f] {
			t.Errorf("ConnectionOptions.%s has no fieldKeywords entry", f)
		}
		delete(named, f)
	}
	for f := range named {
		t.Errorf("fieldKeywords names %s, which is not a ConnectionOptions field", f)
	}
}

// funcName is f's name, to tell pgx's ValidateConnect functions apart.
func funcName(f any) string {
	return runtime.FuncForPC(reflect.ValueOf(f).Pointer()).Name()
}

// exportedFields is the names of v's struct fields.
func exportedFields(v any) []string {
	var names []string
	for f := range reflect.TypeOf(v).Fields() {
		names = append(names, f.Name)
	}
	return names
}

// The tests below are P2.1.3: each holds what pgx parses — the dial plan,
// the TLS configuration, the files it read — to the fields, rather than the
// text connString writes (gosmo's lesson: a DSN that reads right can still
// parse wrong). A field is written over no ConnString and over both forms
// naming a different value, so a field that fails to win shows.

// dsnForms are the ConnStrings a field is written over: none, and both forms
// setting the parameters the fields control to something else. The URI names
// one host: a field's single host over its port list would not parse
// (TestHostsReachPgx).
var dsnForms = []string{
	"",
	"service=svc host=cs-host,cs-host2 port=5000 dbname=cs-db user=cs-user password=cs-pass " +
		"sslmode=require application_name=cs-app connect_timeout=62 " +
		"target_session_attrs=read-only work_mem=1MB",
	"postgres://cs-user:cs-pass@cs-host:5000/cs-db?service=svc&sslmode=require&application_name=cs-app" +
		"&connect_timeout=62&target_session_attrs=read-only&work_mem=1MB",
}

// withKeyword is cs with k=v written after it, in cs's form; "" becomes
// keyword/value.
func withKeyword(cs, k, v string) string {
	switch {
	case isURI(cs):
		return cs + "&" + k + "=" + uriEscape(v)
	case cs == "":
		return k + "=" + quoteKeywordValue(v)
	default:
		return cs + " " + k + "=" + quoteKeywordValue(v)
	}
}

// dialStep is one entry of what pgx will dial, in order: the primary host,
// port and TLS configuration, then each fallback.
type dialStep struct {
	Addr string // host:port, or socket directory:port
	TLS  string // tlsShape
}

func dialPlan(c *pgconn.Config) []dialStep {
	plan := []dialStep{{net.JoinHostPort(c.Host, strconv.Itoa(int(c.Port))), tlsShape(c.TLSConfig)}}
	for _, f := range c.Fallbacks {
		plan = append(plan, dialStep{net.JoinHostPort(f.Host, strconv.Itoa(int(f.Port))), tlsShape(f.TLSConfig)})
	}
	return plan
}

// tlsShape names what a pgx TLS configuration checks: "plain" (no TLS),
// "unverified" (TLS, any certificate), "chain" (a trusted authority signed
// it, any name: verify-ca, or require with a root file) and "full:<name>"
// (chain and the name). The authority is checked separately (RootCAs).
func tlsShape(c *tls.Config) string {
	switch {
	case c == nil:
		return "plain"
	case c.VerifyPeerCertificate != nil:
		return "chain"
	case c.InsecureSkipVerify:
		return "unverified"
	default:
		return "full:" + c.ServerName
	}
}

// testCertificate writes a self-signed certificate and its key as PEM files
// in a fresh directory, for the TLS fields to name, and returns their paths
// and a pool holding the certificate.
func testCertificate(t *testing.T) (certFile, keyFile string, pool *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "pgmo test"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		BasicConstraintsValid: true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
	}
	der, err := x509.CreateCertificate(rand.Reader, tmpl, tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	keyDER, err := x509.MarshalPKCS8PrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	dir := t.TempDir()
	certFile, keyFile = filepath.Join(dir, "test.crt"), filepath.Join(dir, "test.key")
	writeFile(t, certFile, string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})))
	writeFile(t, keyFile, string(pem.EncodeToMemory(&pem.Block{Type: "PRIVATE KEY", Bytes: keyDER})))
	cert, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool = x509.NewCertPool()
	pool.AddCert(cert)
	return certFile, keyFile, pool
}

// TestEveryFieldReachesPgx sets each field alone over each ConnString form
// and checks pgx parsed the field's value, not the ConnString's or the
// service's. The table must name every field, so a field added without
// being written into the connection string fails here, not in a dial. The
// file fields are checked through what pgx reads: the ConnString names
// missing files, so the parse succeeds only if the field's file replaced
// them.
func TestEveryFieldReachesPgx(t *testing.T) {
	certFile, keyFile, pool := testCertificate(t)
	const missing = "/nonexistent/pgmo-test"
	type check struct {
		set       func(*ConnectionOptions)
		overrides map[string]string           // keywords written into ConnString for the field to replace
		test      func(*pgconn.Config) string // "" when pgx parsed the field's value
	}
	expect := func(name string, got, want any) string {
		if reflect.DeepEqual(got, want) {
			return ""
		}
		return fmt.Sprintf("%s = %v, want %v", name, got, want)
	}
	none := func(*pgconn.Config) string { return "" }
	checks := map[string]check{
		"Host": {
			func(o *ConnectionOptions) { o.Host = "field-host" }, nil,
			func(c *pgconn.Config) string { return expect("Host", c.Host, "field-host") },
		},
		"Port": {
			func(o *ConnectionOptions) { o.Port = 6003 }, nil,
			func(c *pgconn.Config) string { return expect("Port", c.Port, uint16(6003)) },
		},
		"Database": {
			func(o *ConnectionOptions) { o.Database = "field-db" }, nil,
			func(c *pgconn.Config) string { return expect("Database", c.Database, "field-db") },
		},
		"User": {
			func(o *ConnectionOptions) { o.User = "field-user" }, nil,
			func(c *pgconn.Config) string { return expect("User", c.User, "field-user") },
		},
		"Password": {
			func(o *ConnectionOptions) { o.Password = "field-pass" }, nil,
			func(c *pgconn.Config) string { return expect("Password", c.Password, "field-pass") },
		},
		"Service": { // a setting the ConnStrings leave to the service; svc's is svc-path
			func(o *ConnectionOptions) { o.Service = "svc2" }, nil,
			func(c *pgconn.Config) string {
				return expect("search_path", c.RuntimeParams["search_path"], "svc2-path")
			},
		},
		"SSLMode": {
			func(o *ConnectionOptions) { o.SSLMode = SSLModeVerifyFull }, nil,
			func(c *pgconn.Config) string { return expect("TLS", tlsShape(c.TLSConfig), "full:"+c.Host) },
		},
		"SSLRootCert": {
			func(o *ConnectionOptions) { o.SSLRootCert = certFile },
			map[string]string{"sslrootcert": missing},
			func(c *pgconn.Config) string {
				if c.TLSConfig == nil || !c.TLSConfig.RootCAs.Equal(pool) {
					return "RootCAs is not the field's file"
				}
				return ""
			},
		},
		"SSLCert": {
			func(o *ConnectionOptions) { o.SSLCert, o.SSLKey = certFile, keyFile },
			map[string]string{"sslcert": missing}, none,
		},
		"SSLKey": {
			func(o *ConnectionOptions) { o.SSLCert, o.SSLKey = certFile, keyFile },
			map[string]string{"sslkey": missing}, none,
		},
		"ApplicationName": {
			func(o *ConnectionOptions) { o.ApplicationName = "field-app" }, nil,
			func(c *pgconn.Config) string {
				return expect("application_name", c.RuntimeParams["application_name"], "field-app")
			},
		},
		"ConnectTimeout": {
			func(o *ConnectionOptions) { o.ConnectTimeout = 63 * time.Second }, nil,
			func(c *pgconn.Config) string { return expect("ConnectTimeout", c.ConnectTimeout, 63*time.Second) },
		},
		"TargetSessionAttrs": {
			func(o *ConnectionOptions) { o.TargetSessionAttrs = TargetSessionReadWrite }, nil,
			func(c *pgconn.Config) string {
				return expect("target_session_attrs", targetSessionOf(c.ValidateConnect), "read-write")
			},
		},
		"Params": {
			func(o *ConnectionOptions) { o.Params = map[string]string{"work_mem": "2MB"} }, nil,
			func(c *pgconn.Config) string { return expect("work_mem", c.RuntimeParams["work_mem"], "2MB") },
		},
	}
	for _, f := range exportedFields(ConnectionOptions{}) {
		if f == "ConnString" {
			continue
		}
		ck, ok := checks[f]
		if !ok {
			t.Errorf("ConnectionOptions.%s has no check here", f)
			continue
		}
		for _, cs := range dsnForms {
			libpqFixture(t)
			if cs == "" {
				t.Setenv("PGSERVICE", "svc") // so the service has a value to lose
			}
			for _, k := range slices.Sorted(maps.Keys(ck.overrides)) {
				cs = withKeyword(cs, k, ck.overrides[k])
			}
			// TLS on (svc says disable), so the TLS fields are read.
			o := ConnectionOptions{ConnString: cs, SSLMode: SSLModeRequire}
			ck.set(&o)
			cfg, err := o.poolConfig()
			if err != nil {
				t.Errorf("%s over %q: %v", f, cs, err)
				continue
			}
			c := cfg.ConnConfig.Config
			if msg := ck.test(&c); msg != "" {
				t.Errorf("%s over %q: %s", f, cs, msg)
			}
		}
	}
}

// TestTLSReachesPgx: every SSLMode, with and without a root file, gives the
// dial plan libpq's mode means — over a ConnString asking for another mode.
func TestTLSReachesPgx(t *testing.T) {
	certFile, _, pool := testCertificate(t)
	for _, tc := range []struct {
		mode     SSLMode
		rootCert bool
		expected []string
	}{
		{SSLModeDisable, false, []string{"plain"}},
		{SSLModeDisable, true, []string{"plain"}},
		{SSLModeAllow, false, []string{"plain", "unverified"}},
		{SSLModePrefer, false, []string{"unverified", "plain"}},
		{SSLModeRequire, false, []string{"unverified"}},
		{SSLModeRequire, true, []string{"chain"}}, // libpq: a root file makes require verify-ca
		{SSLModeVerifyCA, true, []string{"chain"}},
		{SSLModeVerifyFull, false, []string{"full:db.example.com"}},
		{SSLModeVerifyFull, true, []string{"full:db.example.com"}},
	} {
		for _, cs := range []string{"", "sslmode=verify-full", "postgres://cs-host/?sslmode=allow"} {
			libpqFixture(t)
			o := ConnectionOptions{ConnString: cs, Host: "db.example.com", SSLMode: tc.mode}
			if tc.rootCert {
				o.SSLRootCert = certFile
			}
			cfg, err := o.poolConfig()
			if err != nil {
				t.Errorf("%s over %q: %v", tc.mode, cs, err)
				continue
			}
			c := cfg.ConnConfig.Config
			var got []string
			for _, s := range dialPlan(&c) {
				got = append(got, s.TLS)
				if s.Addr != "db.example.com:5432" {
					t.Errorf("%s over %q: dials %s", tc.mode, cs, s.Addr)
				}
			}
			if !slices.Equal(got, tc.expected) {
				t.Errorf("%s (root file %v) over %q: TLS %v, want %v", tc.mode, tc.rootCert, cs, got, tc.expected)
			}
			configs := []*tls.Config{c.TLSConfig}
			for _, f := range c.Fallbacks {
				configs = append(configs, f.TLSConfig)
			}
			for _, tc2 := range configs {
				if tc2 != nil && tc.rootCert != (tc2.RootCAs != nil && tc2.RootCAs.Equal(pool)) {
					t.Errorf("%s (root file %v) over %q: RootCAs not the field's file", tc.mode, tc.rootCert, cs)
				}
			}
		}
	}
}

// TestMissingRootCertFailsInEveryMode pins a pgx-vs-libpq gap (pgmo
// ARCHITECTURE.md): libpq reads sslrootcert only to verify, so a missing
// file is ignored under disable, allow, prefer and require; pgx reads it
// while parsing, in every mode, and fails. Should pgx change, this fails and
// the gap comes off the list.
func TestMissingRootCertFailsInEveryMode(t *testing.T) {
	libpqFixture(t)
	missing := filepath.Join(t.TempDir(), "root.crt")
	for _, m := range sslModes {
		_, err := ConnectionOptions{Host: "h", SSLMode: m, SSLRootCert: missing}.poolConfig()
		if !errors.Is(err, fs.ErrNotExist) || !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("sslmode %s, missing root file: %v", m, err)
		}
	}
}

// TestClientCertificateReachesPgx: pgx reads SSLCert and SSLKey while
// parsing, so a bad pair fails there, naming what is wrong — and is not
// read at all without TLS, as libpq.
func TestClientCertificateReachesPgx(t *testing.T) {
	certFile, keyFile, _ := testCertificate(t)
	missing := filepath.Join(t.TempDir(), "missing")
	libpqFixture(t)
	for _, tc := range []struct {
		cert, key, says string
	}{
		{certFile, keyFile, ""},
		{certFile, missing, "unable to read sslkey"},
		{missing, keyFile, "unable to read cert"},
		{keyFile, keyFile, "certificate"}, // a key where the certificate belongs
	} {
		_, err := ConnectionOptions{Host: "h", SSLMode: SSLModeRequire, SSLCert: tc.cert, SSLKey: tc.key}.poolConfig()
		switch {
		case tc.says == "" && err != nil:
			t.Errorf("cert %s, key %s: %v", tc.cert, tc.key, err)
		case tc.says != "" && (err == nil || !strings.Contains(err.Error(), tc.says) || !errors.Is(err, ErrInvalidRequest)):
			t.Errorf("cert %s, key %s: error %v, want one saying %q", tc.cert, tc.key, err, tc.says)
		}
	}
	if _, err := (ConnectionOptions{Host: "h", SSLMode: SSLModeDisable, SSLCert: missing, SSLKey: missing}).poolConfig(); err != nil {
		t.Errorf("sslmode=disable read the client certificate: %v", err)
	}
}

// TestHostsReachPgx: Host's forms — a list, an IPv6 literal, a socket
// directory — give the dial plan they mean, with Port on every host; with
// Port unset, a ConnString's per-host ports pair with the field's hosts, as
// libpq pairs them, and a count that does not match fails as in libpq.
func TestHostsReachPgx(t *testing.T) {
	for _, tc := range []struct {
		host     string
		port     int
		cs       string
		expected []dialStep
	}{
		{"db1,db2", 6000, "", []dialStep{{"db1:6000", "plain"}, {"db2:6000", "plain"}}},
		{"db1,db2", 6000, dsnForms[1], []dialStep{{"db1:6000", "plain"}, {"db2:6000", "plain"}}},
		{"db1,db2", 6000, dsnForms[2], []dialStep{{"db1:6000", "plain"}, {"db2:6000", "plain"}}},
		{"db1,db2", 0, "postgres://cs1:5000,cs2:5001/", []dialStep{{"db1:5000", "plain"}, {"db2:5001", "plain"}}},
		{"db1,db2", 0, "host=cs1,cs2 port=5000,5001", []dialStep{{"db1:5000", "plain"}, {"db2:5001", "plain"}}},
		{"db1", 0, "host=cs1,cs2 port=5000", []dialStep{{"db1:5000", "plain"}}},
		{"::1", 6000, "", []dialStep{{"[::1]:6000", "plain"}}},
		{"fe80::1", 0, dsnForms[2], []dialStep{{"[fe80::1]:5000", "plain"}}},
		{"db1,::1", 6000, dsnForms[2], []dialStep{{"db1:6000", "plain"}, {"[::1]:6000", "plain"}}},
		{"/var/run/postgresql", 6000, dsnForms[1], []dialStep{{"/var/run/postgresql:6000", "plain"}}},
	} {
		libpqFixture(t)
		cfg, err := ConnectionOptions{ConnString: tc.cs, Host: tc.host, Port: tc.port, SSLMode: SSLModeDisable}.poolConfig()
		if err != nil {
			t.Errorf("Host %q Port %d over %q: %v", tc.host, tc.port, tc.cs, err)
			continue
		}
		c := cfg.ConnConfig.Config
		if got := dialPlan(&c); !slices.Equal(got, tc.expected) {
			t.Errorf("Host %q Port %d over %q: dials %v, want %v", tc.host, tc.port, tc.cs, got, tc.expected)
		}
	}

	libpqFixture(t)
	for _, cs := range []string{"postgres://cs1:5000,cs2:5001/", "host=cs1,cs2 port=5000,5001"} {
		_, err := ConnectionOptions{ConnString: cs, Host: "db1", SSLMode: SSLModeDisable}.poolConfig()
		if !errors.Is(err, ErrInvalidRequest) || !strings.Contains(err.Error(), "could not match 2 port numbers to 1 hosts") {
			t.Errorf("one host over two ports in %q: %v", cs, err)
		}
	}

	// A socket takes no TLS, whatever SSLMode asks (pgx and libpq alike).
	cfg, err := ConnectionOptions{Host: "/tmp", SSLMode: SSLModeVerifyFull}.poolConfig()
	if err != nil {
		t.Fatal(err)
	}
	c := cfg.ConnConfig.Config
	if got := dialPlan(&c); !slices.Equal(got, []dialStep{{"/tmp:5432", "plain"}}) {
		t.Errorf("socket with verify-full dials %v", got)
	}
}
