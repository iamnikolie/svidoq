package mysql

import (
	"errors"
	"testing"
)

func TestValidateAcceptsReads(t *testing.T) {
	for _, q := range []string{
		"SELECT 1",
		"select id, name from clients where id = 42",
		"SELECT a FROM t UNION SELECT b FROM u",
		"WITH x AS (SELECT 1 AS n) SELECT n FROM x",
		"SHOW TABLES",
		"SHOW CREATE TABLE invoices",
		"DESCRIBE invoices",
		"EXPLAIN SELECT * FROM invoices",
		"SELECT * FROM t /* a comment */",
		"SELECT 1;", // a single trailing semicolon is one statement, not two
	} {
		if err := Validate(q); err != nil {
			t.Errorf("Validate(%q) = %v, want nil", q, err)
		}
	}
}

func TestValidateRejectsWrites(t *testing.T) {
	for _, q := range []string{
		"INSERT INTO t VALUES (1)",
		"REPLACE INTO t VALUES (1)",
		"UPDATE t SET a = 1",
		"DELETE FROM t",
		"TRUNCATE TABLE t",
		"DROP TABLE t",
		"DROP DATABASE d",
		"CREATE TABLE t (id INT)",
		"ALTER TABLE t ADD COLUMN b INT",
		"RENAME TABLE a TO b",
		"GRANT ALL ON *.* TO 'x'@'%'",
		"CREATE USER 'x'@'%'",
		"SET GLOBAL read_only = 0",
		"USE other_db",
		"LOAD DATA INFILE '/etc/passwd' INTO TABLE t",
		"CALL some_procedure()",
		"FLUSH PRIVILEGES",
		"KILL 1",
		"LOCK TABLES t WRITE",
		"BEGIN",
		"COMMIT",
	} {
		if err := Validate(q); !errors.Is(err, ErrNotReadOnly) {
			t.Errorf("Validate(%q) = %v, want ErrNotReadOnly", q, err)
		}
	}
}

// The two cases a prefix or regexp check on "SELECT" would wave through, and
// the reason this validator parses instead.
func TestValidateRejectsSelectsThatAreNotReads(t *testing.T) {
	for _, q := range []string{
		"SELECT * FROM t INTO OUTFILE '/tmp/dump.csv'",
		"SELECT * FROM t INTO DUMPFILE '/tmp/dump.bin'",
		"SELECT 1; DROP TABLE t",
		"SELECT 1; INSERT INTO t VALUES (1)",
	} {
		if err := Validate(q); !errors.Is(err, ErrNotReadOnly) {
			t.Errorf("Validate(%q) = %v, want ErrNotReadOnly", q, err)
		}
	}
}

func TestValidateRejectsUnparseable(t *testing.T) {
	for _, q := range []string{"", "   ", "not sql at all", "SELECT FROM"} {
		if err := Validate(q); !errors.Is(err, ErrNotReadOnly) {
			t.Errorf("Validate(%q) = %v, want ErrNotReadOnly", q, err)
		}
	}
}

func TestErrorNamesTheStatement(t *testing.T) {
	err := Validate("DELETE FROM t")
	if err == nil || !contains(err.Error(), "DELETE") {
		t.Fatalf("error should name the statement kind, got %v", err)
	}
}

func contains(haystack, needle string) bool {
	return len(haystack) >= len(needle) && (haystack == needle ||
		len(haystack) > 0 && (indexOf(haystack, needle) >= 0))
}

func indexOf(h, n string) int {
	for i := 0; i+len(n) <= len(h); i++ {
		if h[i:i+len(n)] == n {
			return i
		}
	}

	return -1
}

// A locking read is a SelectStmt, so it sails past any check that only asks
// "is this a SELECT". It changes nothing and still stalls every writer on the
// table, which is the outcome this tool exists to make impossible.
func TestValidateRejectsLockingReads(t *testing.T) {
	for _, q := range []string{
		"SELECT * FROM t FOR UPDATE",
		"SELECT * FROM t FOR SHARE",
		"SELECT * FROM t LOCK IN SHARE MODE",
	} {
		if err := Validate(q); !errors.Is(err, ErrNotReadOnly) {
			t.Errorf("Validate(%q) = %v, want ErrNotReadOnly", q, err)
		}
	}
}
