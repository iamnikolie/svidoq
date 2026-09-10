package mysql

import "testing"

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
