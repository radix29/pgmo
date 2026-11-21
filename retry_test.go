package pgmo

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgconn"
)

func TestIsRetryableSQLStates(t *testing.T) {
	retryable := []string{
		"08000", "08003", "08006", "08001", "08004", "08007", "08P01",
		"40001", "40P01", "57P01", "57P02", "57P03", "57P05",
	}
	for _, code := range retryable {
		if !IsRetryable(pgErr(code)) {
			t.Errorf("IsRetryable(SQLSTATE %s) = false, want true", code)
		}
	}
	// The near misses: the rest of class 40 and 57, a refused login, the
	// non-transient codes the other predicates own.
	for _, code := range []string{
		"40000", "40002", "40003", "57000", "57014", "57P04", "28P01", "28000",
		"42501", "42P01", "42P07", "53300", "XX000", "0", "",
	} {
		if IsRetryable(pgErr(code)) {
			t.Errorf("IsRetryable(SQLSTATE %q) = true, want false", code)
		}
	}
}

func TestIsRetryableClientErrors(t *testing.T) {
	refused := &net.OpError{Op: "dial", Net: "tcp", Err: os.NewSyscallError("connect", syscall.ECONNREFUSED)}
	cases := []struct {
		name string
		err  error
		want bool
	}{
		{"nil", nil, false},
		{"io.EOF", fmt.Errorf("read: %w", io.EOF), true},
		{"io.ErrUnexpectedEOF", fmt.Errorf("read: %w", io.ErrUnexpectedEOF), true},
		{"net.OpError", fmt.Errorf("dial: %w", refused), true},
		{"context.Canceled", fmt.Errorf("q: %w", context.Canceled), false},
		{"context.DeadlineExceeded", fmt.Errorf("q: %w", context.DeadlineExceeded), false},
		// A deadline ends a statement as a net timeout joined with the
		// context's error; the context wins.
		{"deadline beside net error", errors.Join(refused, context.DeadlineExceeded), false},
		// A PgError decides on its own, whatever else the chain holds: a
		// refused login reached over a flaky dial is still a refused login.
		{"PgError beside net error", errors.Join(refused, pgErr("28P01")), false},
		{"pgmo refusal", fmt.Errorf("pgmo: op: %w", ErrInvalidRequest), false},
		{"plain", errors.New("connection reset"), false},
	}
	for _, c := range cases {
		if got := IsRetryable(c.err); got != c.want {
			t.Errorf("%s: IsRetryable(%v) = %v, want %v", c.name, c.err, got, c.want)
		}
	}
}

// TestIsRetryablePgconnErrors classifies the errors pgconn itself returns,
// not hand-built look-alikes: a refused connect is retryable, the same
// connect under a cancelled or expired context is not.
func TestIsRetryablePgconnErrors(t *testing.T) {
	// A port nothing listens on: bind one, note it, close it.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	addr := l.Addr().(*net.TCPAddr)
	l.Close()
	dsn := fmt.Sprintf("host=127.0.0.1 port=%d user=u dbname=d sslmode=disable connect_timeout=5", addr.Port)

	_, err = pgconn.Connect(context.Background(), dsn)
	if _, ok := errors.AsType[*pgconn.ConnectError](err); !ok {
		t.Fatalf("Connect to a closed port: %T %v, want *pgconn.ConnectError", err, err)
	}
	if !IsRetryable(err) {
		t.Errorf("IsRetryable(refused connect %v) = false, want true", err)
	}

	cancelled, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := pgconn.Connect(cancelled, dsn); err == nil || IsRetryable(err) {
		t.Errorf("IsRetryable(connect under a cancelled context %v) = true or nil error", err)
	}

	expired, cancel2 := context.WithDeadline(context.Background(), time.Now().Add(-time.Second))
	defer cancel2()
	if _, err := pgconn.Connect(expired, dsn); err == nil || IsRetryable(err) {
		t.Errorf("IsRetryable(connect under an expired deadline %v) = true or nil error", err)
	}
}
