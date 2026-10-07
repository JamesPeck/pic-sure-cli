package sql

import (
	"errors"
	"strings"
)

// QuoteMySQL returns s as a MySQL string literal, quotes included.
//
// A single quote is doubled, which MySQL reads the same way whatever the
// server's sql_mode. Backslash, NUL, newline, carriage return and Ctrl-Z
// become backslash escapes, as mysql_real_escape_string writes them. Under
// the NO_BACKSLASH_ESCAPES sql_mode the server would keep those escapes
// verbatim, but the literal still ends where it should, so a value can
// never break out of its quotes.
//
// The escaping relies on the connection character set being utf8mb4, in
// which no multibyte character contains the byte for a quote or a
// backslash. ExecMySQL and QueryMySQL set it.
func QuoteMySQL(s string) string {
	var b strings.Builder
	b.Grow(len(s) + 2)
	b.WriteByte('\'')
	for i := 0; i < len(s); i++ {
		switch c := s[i]; c {
		case '\'':
			b.WriteString(`''`)
		case '\\':
			b.WriteString(`\\`)
		case 0:
			b.WriteString(`\0`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case 0x1a:
			b.WriteString(`\Z`)
		default:
			b.WriteByte(c)
		}
	}
	b.WriteByte('\'')
	return b.String()
}

// quoteMySQLIdent returns name as a backquoted MySQL identifier.
func quoteMySQLIdent(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}

// ErrNUL is returned for a value with a NUL byte, which Postgres text
// cannot hold.
var ErrNUL = errors.New("sql: value contains a NUL byte")

// QuotePostgres returns s as a Postgres string literal, quotes included.
// A single quote is doubled. A value with a backslash becomes an escape
// string (E'...') with each backslash doubled, so it reads the same whether
// standard_conforming_strings is on or off. It fails with ErrNUL for a
// value with a NUL byte.
func QuotePostgres(s string) (string, error) {
	if strings.IndexByte(s, 0) >= 0 {
		return "", ErrNUL
	}
	quoted := strings.ReplaceAll(s, "'", "''")
	if !strings.Contains(s, `\`) {
		return "'" + quoted + "'", nil
	}
	return "E'" + strings.ReplaceAll(quoted, `\`, `\\`) + "'", nil
}

// QuotePostgresIdent returns name as a double-quoted Postgres identifier.
// It fails with ErrNUL for a name with a NUL byte.
func QuotePostgresIdent(name string) (string, error) {
	if strings.IndexByte(name, 0) >= 0 {
		return "", ErrNUL
	}
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`, nil
}
