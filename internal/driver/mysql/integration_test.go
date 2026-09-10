package mysql

import (
	"context"
	"errors"
	"os"
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

func TestIntegrationTimeoutStopsARunawayQuery(t *testing.T) {
	db := testDB(t)

	start := time.Now()

	_, err := db.Query(context.Background(), "SELECT SLEEP(10)",
		datasource.QueryOpts{Limit: 1, Timeout: time.Second})
	if err == nil {
		t.Fatal("a 10s sleep under a 1s timeout should fail")
	}

	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("timeout did not fire promptly: %s", elapsed)
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
