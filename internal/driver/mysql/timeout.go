package mysql

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
)

// ErrQueryTimeout means a statement ran past --timeout and was stopped.
var ErrQueryTimeout = errors.New("query timed out")

// Server error numbers for a statement stopped by the server-side cap.
const (
	errMariaDBStatementTimeout = 1969 // max_statement_time exceeded
	errMySQLQueryTimeout       = 3024 // MAX_EXECUTION_TIME exceeded
	errUnknownSystemVariable   = 1193
)

// killTimeout bounds the KILL QUERY sent after a client-side deadline, so a
// server that is unreachable cannot make the CLI hang on its way out.
const killTimeout = 3 * time.Second

// warnf writes a warning to stderr. The driver has no other channel to the
// user, and a silent no-op is exactly how an unbounded query slips through.
var warnf = func(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "svidoq: warning: "+format+"\n", args...)
}

// isMariaDB reports whether a VERSION() string belongs to MariaDB, e.g.
// "10.6.25-MariaDB-log". MySQL returns a bare "8.0.36" or "8.4.2".
func isMariaDB(version string) bool {
	return strings.Contains(strings.ToLower(version), "mariadb")
}

// serverCap is the statement cap to set on the server for a client deadline.
// It is a little shorter than the deadline, so the server stops the statement
// and reports why, instead of the client hanging up on a query that keeps
// running.
func serverCap(timeout time.Duration) time.Duration {
	margin := min(max(timeout/10, 50*time.Millisecond), 2*time.Second)
	if timeout <= 2*margin {
		return timeout
	}

	return timeout - margin
}

// timeoutStatement is the SET that caps statement time on the server:
// max_statement_time in (fractional) seconds on MariaDB, MAX_EXECUTION_TIME in
// milliseconds on MySQL. MySQL applies it to SELECT only; everything else
// relies on the KILL QUERY sent after the client deadline.
func timeoutStatement(version string, limit time.Duration) string {
	if isMariaDB(version) {
		return "SET SESSION max_statement_time = " +
			strconv.FormatFloat(limit.Seconds(), 'f', -1, 64)
	}

	return fmt.Sprintf("SET SESSION MAX_EXECUTION_TIME = %d", max(limit.Milliseconds(), 1))
}

// isServerTimeout reports whether err is the server stopping a statement at
// its cap.
func isServerTimeout(err error) bool {
	var me *mysqldriver.MySQLError

	return errors.As(err, &me) &&
		(me.Number == errMariaDBStatementTimeout || me.Number == errMySQLQueryTimeout)
}

func isUnknownVariable(err error) bool {
	var me *mysqldriver.MySQLError

	return errors.As(err, &me) && me.Number == errUnknownSystemVariable
}

// killQuery stops whatever connection id is executing, over a second, short
// connection. The driver only closes its socket when the context expires; the
// server keeps running a statement that has not produced output yet, so
// without this a timed-out aggregate can run for hours. Any user may kill its
// own threads, so a read-only account is enough.
func (d *DB) killQuery(id uint64) error {
	ctx, cancel := context.WithTimeout(context.Background(), killTimeout)
	defer cancel()

	_, err := d.db.ExecContext(ctx, fmt.Sprintf("KILL QUERY %d", id))

	return err
}

// timeoutError explains a statement stopped by --timeout, on whichever side
// stopped it.
func timeoutError(timeout time.Duration, cause string) error {
	return fmt.Errorf("%w after %s: %s", ErrQueryTimeout, timeout, cause)
}
