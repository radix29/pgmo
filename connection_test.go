package pgmo

import (
	"os"
	"strings"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
)

// clearLibpqEnv keeps the caller's libpq setup out of a ParseConfig call:
// every PG* variable is unset, and the home directory (where pgx finds
// ~/.pgpass, ~/.pg_service.conf and ~/.postgresql) is an empty one. Unset
// rather than set empty, since pgx refuses a present-but-empty
// PGSSLCERTMODE.
func clearLibpqEnv(t *testing.T) {
	t.Helper()
	for _, kv := range os.Environ() {
		if name, _, _ := strings.Cut(kv, "="); strings.HasPrefix(name, "PG") {
			t.Setenv(name, "") // restores the value after the test
			os.Unsetenv(name)
		}
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("APPDATA", home)
}

// TestSSLModesAreWhatPgxParses holds every SSLMode constant to a value pgx
// accepts as sslmode, and the check itself to one that can fail.
func TestSSLModesAreWhatPgxParses(t *testing.T) {
	clearLibpqEnv(t)
	for _, m := range []SSLMode{
		SSLModeDisable, SSLModeAllow, SSLModePrefer,
		SSLModeRequire, SSLModeVerifyCA, SSLModeVerifyFull,
	} {
		if _, err := pgconn.ParseConfig("host=localhost sslmode=" + string(m)); err != nil {
			t.Errorf("sslmode=%s: %v", m, err)
		}
	}
	if _, err := pgconn.ParseConfig("host=localhost sslmode=verify_full"); err == nil {
		t.Error("sslmode=verify_full parsed: the check cannot fail")
	}
}

// TestTargetSessionAttrsAreWhatPgxParses holds every TargetSessionAttrs
// constant to a value pgx accepts as target_session_attrs, and the check
// itself to one that can fail.
func TestTargetSessionAttrsAreWhatPgxParses(t *testing.T) {
	clearLibpqEnv(t)
	for _, a := range []TargetSessionAttrs{
		TargetSessionAny, TargetSessionReadWrite, TargetSessionReadOnly,
		TargetSessionPrimary, TargetSessionStandby, TargetSessionPreferStandby,
	} {
		if _, err := pgconn.ParseConfig("host=localhost target_session_attrs=" + string(a)); err != nil {
			t.Errorf("target_session_attrs=%s: %v", a, err)
		}
	}
	if _, err := pgconn.ParseConfig("host=localhost target_session_attrs=readwrite"); err == nil {
		t.Error("target_session_attrs=readwrite parsed: the check cannot fail")
	}
}
