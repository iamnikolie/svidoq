package mysql

import (
	"context"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/iamnikolie/svidoq/internal/datasource"
)

// These tests need a real server. They are skipped unless SVIDOQ_TEST_DSN is
// set, so `go test ./...` stays hermetic in CI and on a laptop:
//
//	docker run -d --name svidoq-test -e MYSQL_ROOT_PASSWORD=testpw \
//	  -e MYSQL_DATABASE=billing -p 13399:3306 mysql:8.4
//	SVIDOQ_TEST_DSN='root:testpw@tcp(127.0.0.1:13399)/billing?parseTime=true' go test ./...
func testDB(t *testing.T) *DB {
	t.Helper()

	dsn := os.Getenv("SVIDOQ_TEST_DSN")
	if dsn == "" {
		t.Skip("set SVIDOQ_TEST_DSN to run integration tests")
	}

	db, err := Open("billing", dsn, 5*time.Second)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	return db
}

func TestIntegrationReadWorks(t *testing.T) {
	db := testDB(t)

	res, err := db.Query(context.Background(), "SELECT 1 AS n",
		datasource.QueryOpts{Limit: 10, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("query: %v", err)
	}

	if res.RowCount != 1 {
		t.Fatalf("RowCount = %d, want 1", res.RowCount)
	}
}

// The whole point of the second layer: a write that skips the validator still
// cannot reach the data. runReadOnly is called directly here precisely because
// Query would have refused this statement at layer 1.
func TestIntegrationEngineBlocksWritesThatSkipTheValidator(t *testing.T) {
	db := testDB(t)

	for _, q := range []string{
		"INSERT INTO invoices (client, total) VALUES ('smuggled', 1)",
		"UPDATE invoices SET total = 0",
		"DELETE FROM invoices",
	} {
		_, err := db.runReadOnly(context.Background(), q,
			datasource.QueryOpts{Limit: 10, Timeout: 5 * time.Second})
		if !errors.Is(err, ErrWriteBlocked) {
			t.Errorf("runReadOnly(%q) = %v, want ErrWriteBlocked", q, err)
		}
	}
}

// stillRunning reports whether a statement carrying marker is still executing
// on the server, polling for up to two seconds for it to go away.
func stillRunning(t *testing.T, db *DB, marker string) bool {
	t.Helper()

	q := "SELECT COUNT(*) AS n FROM information_schema.PROCESSLIST" +
		" WHERE ID <> CONNECTION_ID() AND INFO LIKE " + quoteLiteral("%"+marker+"%")

	for deadline := time.Now().Add(2 * time.Second); time.Now().Before(deadline); time.Sleep(200 * time.Millisecond) {
		res, err := db.Query(context.Background(), q, datasource.QueryOpts{Limit: 1, Timeout: 5 * time.Second})
		if err != nil {
			t.Fatalf("processlist: %v", err)
		}

		if fmt.Sprint(res.Rows[0]["n"]) == "0" {
			return false
		}
	}

	return true
}

// SLEEP is only part of the statement: MySQL returns no error when a
// statement that is nothing but SLEEP() gets interrupted.
func TestIntegrationTimeoutStopsARunawayQuery(t *testing.T) {
	db := testDB(t)

	start := time.Now()

	_, err := db.Query(context.Background(), "SELECT SLEEP(5) AS svidoq_server_cap FROM invoices",
		datasource.QueryOpts{Limit: 1, Timeout: time.Second})
	if !errors.Is(err, ErrQueryTimeout) {
		t.Fatalf("a 5s sleep under a 1s timeout = %v, want ErrQueryTimeout", err)
	}

	if !strings.Contains(err.Error(), "stopped by the server") {
		t.Errorf("the server cap should win the race with the client deadline: %v", err)
	}

	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("timeout did not fire promptly: %s", elapsed)
	}

	if stillRunning(t, db, "svidoq_server_cap") {
		t.Fatal("the statement is still running on the server after the timeout")
	}
}

// Without a server-side cap (an old server, or MySQL on a non-SELECT) the
// client deadline only closes the socket; KILL QUERY must stop the statement.
func TestIntegrationClientDeadlineKillsTheQuery(t *testing.T) {
	db := testDB(t)
	db.noServerCap = true

	_, err := db.Query(context.Background(), "SELECT SLEEP(5) AS svidoq_kill_probe",
		datasource.QueryOpts{Limit: 1, Timeout: time.Second})
	if !errors.Is(err, ErrQueryTimeout) {
		t.Fatalf("got %v, want ErrQueryTimeout", err)
	}

	if !strings.Contains(err.Error(), "KILL QUERY sent") {
		t.Errorf("error should say the query was killed: %v", err)
	}

	if stillRunning(t, db, "svidoq_kill_probe") {
		t.Fatal("the statement outlived the client deadline: KILL QUERY did not stop it")
	}
}

func TestIntegrationSessionCarriesTheStatementCap(t *testing.T) {
	db := testDB(t)

	res, err := db.Query(context.Background(), "SELECT VERSION() AS v",
		datasource.QueryOpts{Limit: 1, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("version: %v", err)
	}

	q := "SELECT @@max_execution_time AS cap"
	if isMariaDB(fmt.Sprint(res.Rows[0]["v"])) {
		q = "SELECT @@max_statement_time AS cap"
	}

	res, err = db.Query(context.Background(), q, datasource.QueryOpts{Limit: 1, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("%s: %v", q, err)
	}

	if got := fmt.Sprint(res.Rows[0]["cap"]); got == "0" || got == "0.000000" {
		t.Fatalf("%s = %s inside a session with --timeout, want non-zero", q, got)
	}
}

func TestIntegrationTruncationIsReported(t *testing.T) {
	db := testDB(t)

	res, err := db.Query(context.Background(), "SELECT id FROM invoices",
		datasource.QueryOpts{Limit: 1, Timeout: 5 * time.Second})
	if err != nil {
		t.Fatalf("query: %v", err)
	}

	if !res.Truncated || res.RowCount != 1 {
		t.Fatalf("Truncated=%v RowCount=%d, want true/1", res.Truncated, res.RowCount)
	}
}
