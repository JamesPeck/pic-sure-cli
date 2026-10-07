package sql

import (
	"errors"
	"testing"
)

func TestQuoteMySQL(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"", `''`},
		{"admin@example.org", `'admin@example.org'`},
		{"o'brien@example.org", `'o''brien@example.org'`},
		{`back\slash`, `'back\\slash'`},
		{`trailing\`, `'trailing\\'`},
		{`\'`, `'\\'''`},
		{"nul\x00byte", `'nul\0byte'`},
		{"two\nlines\r\n", `'two\nlines\r\n'`},
		{"ctrl\x1az", `'ctrl\Zz'`},
		{`"double" quotes and tab	stay`, `'"double" quotes and tab	stay'`},
		{"; DROP DATABASE auth; --", `'; DROP DATABASE auth; --'`},
		{"ünïcødé ✓", `'ünïcødé ✓'`},
	} {
		if got := QuoteMySQL(tc.in); got != tc.want {
			t.Errorf("QuoteMySQL(%q) = %s, want %s", tc.in, got, tc.want)
		}
	}
}

func TestQuoteMySQLIdent(t *testing.T) {
	if got, want := quoteMySQLIdent("pic`sure"), "`pic``sure`"; got != want {
		t.Errorf("quoteMySQLIdent = %s, want %s", got, want)
	}
}

func TestQuotePostgres(t *testing.T) {
	for _, tc := range []struct {
		in, want string
	}{
		{"", `''`},
		{"o'brien@example.org", `'o''brien@example.org'`},
		{`back\slash`, `E'back\\slash'`},
		{`it's\`, `E'it''s\\'`},
		{"two\nlines", "'two\nlines'"},
	} {
		got, err := QuotePostgres(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("QuotePostgres(%q) = %s, %v; want %s", tc.in, got, err, tc.want)
		}
	}
	if _, err := QuotePostgres("a\x00b"); !errors.Is(err, ErrNUL) {
		t.Errorf("QuotePostgres with NUL: err = %v, want ErrNUL", err)
	}
}

func TestQuotePostgresIdent(t *testing.T) {
	got, err := QuotePostgresIdent(`pic"sure`)
	if err != nil || got != `"pic""sure"` {
		t.Errorf("QuotePostgresIdent = %s, %v", got, err)
	}
	if _, err := QuotePostgresIdent("a\x00b"); !errors.Is(err, ErrNUL) {
		t.Errorf("QuotePostgresIdent with NUL: err = %v, want ErrNUL", err)
	}
}
