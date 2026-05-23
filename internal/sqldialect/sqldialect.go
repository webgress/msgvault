// Package sqldialect carries the small SQL primitives that both the
// store and query packages have to keep in lockstep so a query routed
// through one package matches a query routed through the other.
//
// Add things here only when divergence between the two packages would
// silently produce different results for the same input (e.g., a `?`
// rebind that doesn't respect quoted strings, or a tsquery escape that
// strips a different set of metacharacters). Dialect features that
// only one package needs (DDL, lifecycle, error classification) stay
// in that package's own Dialect interface.
package sqldialect

import (
	"fmt"
	"strings"
)

// RebindPostgreSQL converts `?` placeholders in query to PostgreSQL
// `$1, $2, ...` numbered placeholders. `?` inside single-quoted string
// literals is left alone so prepared SQL fragments containing literal
// question marks survive the rewrite intact.
func RebindPostgreSQL(query string) string {
	var b strings.Builder
	b.Grow(len(query) + 16)
	n := 1
	inQuote := false
	for i := 0; i < len(query); i++ {
		ch := query[i]
		if ch == '\'' {
			inQuote = !inQuote
			b.WriteByte(ch)
		} else if ch == '?' && !inQuote {
			fmt.Fprintf(&b, "$%d", n)
			n++
		} else {
			b.WriteByte(ch)
		}
	}
	return b.String()
}

// EscapeTSQueryTerm removes PostgreSQL `to_tsquery` metacharacters and
// whitespace from a single term, returning a token safe to suffix with
// `:*` for prefix matching. Returns "" when the input collapses to
// nothing usable.
func EscapeTSQueryTerm(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '&', '|', '!', '(', ')', ':', '*', '\\', '\'':
			continue
		}
		if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
			continue
		}
		b.WriteRune(r)
	}
	return b.String()
}
