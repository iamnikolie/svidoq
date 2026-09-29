package mysql

import (
	"errors"
	"fmt"
	"testing"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
)

func TestTimeoutStatementPicksTheServersVariable(t *testing.T) {
	cases := []struct {
		version string
		limit   time.Duration
		want    string
	}{
		{"10.6.25-MariaDB-log", 120 * time.Second, "SET SESSION max_statement_time = 120"},
		{"10.6.25-MariaDB-log", 500 * time.Millisecond, "SET SESSION max_statement_time = 0.5"},
		{"11.4.2-MariaDB-ubu2404", 1500 * time.Millisecond, "SET SESSION max_statement_time = 1.5"},
		{"8.0.36", 120 * time.Second, "SET SESSION MAX_EXECUTION_TIME = 120000"},
		{"8.4.2", 900 * time.Millisecond, "SET SESSION MAX_EXECUTION_TIME = 900"},
		{"8.0.36", 100 * time.Microsecond, "SET SESSION MAX_EXECUTION_TIME = 1"},
	}

	for _, c := range cases {
		if got := timeoutStatement(c.version, c.limit); got != c.want {
			t.Errorf("timeoutStatement(%q, %s) = %q, want %q", c.version, c.limit, got, c.want)
		}
	}
}

// The server cap must land before the client deadline, or the client hangs up
// first and the server error that explains the timeout is never seen.
func TestServerCapIsShorterThanTheDeadline(t *testing.T) {
	cases := map[time.Duration]time.Duration{
		time.Second:            900 * time.Millisecond,
		5 * time.Second:        4500 * time.Millisecond,
		30 * time.Second:       28 * time.Second,
		10 * time.Minute:       10*time.Minute - 2*time.Second,
		100 * time.Millisecond: 100 * time.Millisecond,
	}

	for deadline, want := range cases {
		if got := serverCap(deadline); got != want {
			t.Errorf("serverCap(%s) = %s, want %s", deadline, got, want)
		}
	}
}

func TestServerTimeoutErrorsMapToErrQueryTimeout(t *testing.T) {
	db := &DB{}

	for _, n := range []uint16{errMariaDBStatementTimeout, errMySQLQueryTimeout} {
		err := db.queryError(t.Context(), &mysqldriver.MySQLError{Number: n}, 7, 5*time.Second)
		if !errors.Is(err, ErrQueryTimeout) {
			t.Errorf("error %d mapped to %v, want ErrQueryTimeout", n, err)
		}
	}

	err := db.queryError(t.Context(), &mysqldriver.MySQLError{Number: 1064}, 7, 5*time.Second)
	if errors.Is(err, ErrQueryTimeout) {
		t.Errorf("a syntax error must not read as a timeout: %v", err)
	}

	err = db.queryError(t.Context(), fmt.Errorf("wrapped: %w", &mysqldriver.MySQLError{Number: 1792}), 7, time.Second)
	if !errors.Is(err, ErrWriteBlocked) {
		t.Errorf("1792 mapped to %v, want ErrWriteBlocked", err)
	}
}

func TestIsMariaDB(t *testing.T) {
	for v, want := range map[string]bool{
		"10.6.25-MariaDB-log":           true,
		"5.5.5-10.11.8-MariaDB-0+deb12": true,
		"8.0.36":                        false,
		"8.0.28-0ubuntu0.20.04.3":       false,
	} {
		if got := isMariaDB(v); got != want {
			t.Errorf("isMariaDB(%q) = %v, want %v", v, got, want)
		}
	}
}
