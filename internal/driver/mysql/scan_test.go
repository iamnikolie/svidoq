package mysql

import (
	"testing"

	mysqldriver "github.com/go-sql-driver/mysql"
)

func TestNormalizeCell(t *testing.T) {
	if got := normalizeCell([]byte("hello")); got != "hello" {
		t.Errorf("[]byte should become a string, got %#v", got)
	}

	if got := normalizeCell(int64(7)); got != int64(7) {
		t.Errorf("int64 should pass through, got %#v", got)
	}

	if got := normalizeCell(nil); got != nil {
		t.Errorf("nil should stay nil, got %#v", got)
	}
}

func TestQuoteLiteral(t *testing.T) {
	cases := map[string]string{
		"invoices":   "'invoices'",
		"it's":       `'it\'s'`,
		`back\slash`: `'back\\slash'`,
		"":           "''",
	}

	for in, want := range cases {
		if got := quoteLiteral(in); got != want {
			t.Errorf("quoteLiteral(%q) = %s, want %s", in, got, want)
		}
	}
}

// A DSN carrying multiStatements=true is common in application configs that
// get copied into a profile. The driver must strip it: two statements in one
// call is exactly the shape the validator exists to refuse, and defence in
// depth means the transport cannot carry it either.
func TestOpenDisablesMultiStatements(t *testing.T) {
	db, err := Open("x", "u:p@tcp(127.0.0.1:1)/d?parseTime=true&multiStatements=true", 0)
	if err != nil {
		t.Fatalf("open: %v", err)
	}

	t.Cleanup(func() { _ = db.Close() })

	cfg, err := mysqldriver.ParseDSN("u:p@tcp(127.0.0.1:1)/d?parseTime=true&multiStatements=true")
	if err != nil {
		t.Fatalf("parse: %v", err)
	}

	if !cfg.MultiStatements {
		t.Fatal("fixture is wrong: the DSN should ask for multiStatements")
	}
	// Open must have overridden it; asserted through the connector it built.
	if got := db.multiStatements(); got {
		t.Error("Open kept multiStatements=true from the DSN")
	}
}
