package mysql

import (
	"context"
	"errors"
	"net"
	"strings"
	"testing"
)

// fakeTransport records what the driver does with it.
type fakeTransport struct {
	closed int
	dialed []string
}

func (f *fakeTransport) DialContext(_ context.Context, network, addr string) (net.Conn, error) {
	f.dialed = append(f.dialed, network+" "+addr)

	return nil, errors.New("fake transport dials nothing")
}

func (f *fakeTransport) Close() error {
	f.closed++

	return nil
}

func TestOpenSendsEveryConnectionThroughTheTransport(t *testing.T) {
	via := &fakeTransport{}

	db, err := Open("x", "u:p@tcp(127.0.0.1:3306)/d?parseTime=true", 0, via)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	_ = db.db.PingContext(context.Background())

	if len(via.dialed) == 0 || via.dialed[0] != "tcp 127.0.0.1:3306" {
		t.Fatalf("dialed %v, want the DSN's address through the transport", via.dialed)
	}

	if err := db.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	if via.closed != 1 {
		t.Fatalf("transport closed %d times, want once with the DB", via.closed)
	}
}

func TestOpenRefusesWhatATunnelCannotCarry(t *testing.T) {
	for dsn, want := range map[string]string{
		"u:p@unix(/var/run/mysqld/mysqld.sock)/d":      "tcp only",
		"u:p@tcp(127.0.0.1:3306)/d?readTimeout=5s":     "readTimeout",
		"u:p@tcp(127.0.0.1:3306)/d?writeTimeout=5s":    "writeTimeout",
		"u:p@tcp(127.0.0.1:3306)/d?parseTime=maybe&x=": "parse dsn",
	} {
		via := &fakeTransport{}

		_, err := Open("x", dsn, 0, via)
		if err == nil || !strings.Contains(err.Error(), want) {
			t.Errorf("Open(%q) = %v, want an error naming %q", dsn, err, want)
		}

		if via.closed != 1 {
			t.Errorf("Open(%q) failed but closed the transport %d times, want once", dsn, via.closed)
		}
	}
}

// Without a transport, deadlines in the DSN are the driver's business as before.
func TestOpenWithoutTransportKeepsDSNTimeouts(t *testing.T) {
	db, err := Open("x", "u:p@tcp(127.0.0.1:1)/d?readTimeout=5s", 0, nil)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	_ = db.Close()
}
