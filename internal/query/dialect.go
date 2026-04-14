// Package query - database dialect abstraction for query engine.
//
// The query engine uses a small dialect interface to handle SQLite vs.
// PostgreSQL differences that surface in aggregate/search SQL:
//   - ? vs $N placeholder syntax (Rebind)
//   - strftime vs to_char for time truncation
//   - messages_fts MATCH vs tsvector @@ for full-text search
//   - sqlite_master vs information_schema for existence probes
//
// The store package has a richer Dialect interface for its own needs;
// this package maintains a minimal parallel abstraction to avoid a
// cross-package dependency.
package query

import (
	"fmt"
	"strings"
)

// Dialect abstracts the SQL differences that the query engine must handle.
type Dialect interface {
	Rebind(query string) string
	TimeTruncExpression(column string, granularity string) string
	FTSSearchExpression() string
	HasFTSTableSQL() string

	// FTSJoin returns the JOIN clause needed for full-text search, or "" if
	// none is required. SQLite needs to join messages_fts by rowid;
	// PostgreSQL has the tsvector column on messages directly.
	FTSJoin() string

	// BuildFTSTerm converts a slice of user-supplied search terms into:
	//   - expr: the SQL boolean expression to AND into WHERE (uses ? placeholder)
	//   - arg:  the single string argument that goes with it
	// Both SQLite and PostgreSQL support prefix matching via the dialect-appropriate
	// syntax (FTS5 "term*", tsquery "term:*").
	BuildFTSTerm(terms []string) (expr string, arg string)
}

// SQLiteQueryDialect implements Dialect for SQLite.
type SQLiteQueryDialect struct{}

func (SQLiteQueryDialect) Rebind(query string) string { return query }

func (SQLiteQueryDialect) TimeTruncExpression(column string, granularity string) string {
	switch granularity {
	case "year":
		return fmt.Sprintf("strftime('%%Y', %s)", column)
	case "month":
		return fmt.Sprintf("strftime('%%Y-%%m', %s)", column)
	case "day":
		return fmt.Sprintf("strftime('%%Y-%%m-%%d', %s)", column)
	default:
		return fmt.Sprintf("strftime('%%Y-%%m', %s)", column)
	}
}

func (SQLiteQueryDialect) FTSSearchExpression() string {
	return "messages_fts MATCH ?"
}

func (SQLiteQueryDialect) HasFTSTableSQL() string {
	return `SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name='messages_fts'`
}

func (SQLiteQueryDialect) FTSJoin() string {
	return "JOIN messages_fts fts ON fts.rowid = m.id"
}

// BuildFTSTerm for SQLite FTS5: quote each term and add "*" for prefix match,
// AND them together. Escaping double-quotes in the term itself prevents
// injection of FTS5 operators (-, :, (, )).
func (SQLiteQueryDialect) BuildFTSTerm(terms []string) (expr string, arg string) {
	ftsTerms := make([]string, len(terms))
	for i, term := range terms {
		term = strings.ReplaceAll(term, "\"", "\"\"")
		term = strings.ReplaceAll(term, "*", "")
		ftsTerms[i] = fmt.Sprintf("\"%s\"*", term)
	}
	return "messages_fts MATCH ?", strings.Join(ftsTerms, " ")
}

// PostgreSQLQueryDialect implements Dialect for PostgreSQL.
type PostgreSQLQueryDialect struct{}

// Rebind converts ? placeholders to $1, $2, ... for PostgreSQL.
// Correctly handles quoted strings — only converts ? outside single quotes.
func (PostgreSQLQueryDialect) Rebind(query string) string {
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

func (PostgreSQLQueryDialect) TimeTruncExpression(column string, granularity string) string {
	switch granularity {
	case "year":
		return fmt.Sprintf("to_char(%s, 'YYYY')", column)
	case "month":
		return fmt.Sprintf("to_char(%s, 'YYYY-MM')", column)
	case "day":
		return fmt.Sprintf("to_char(%s, 'YYYY-MM-DD')", column)
	default:
		return fmt.Sprintf("to_char(%s, 'YYYY-MM')", column)
	}
}

func (PostgreSQLQueryDialect) FTSSearchExpression() string {
	return "m.search_fts @@ plainto_tsquery('simple', ?)"
}

func (PostgreSQLQueryDialect) HasFTSTableSQL() string {
	return `SELECT COUNT(*) FROM information_schema.columns
		WHERE table_name = 'messages' AND column_name = 'search_fts'`
}

// FTSJoin: PostgreSQL's tsvector column lives on messages — no join needed.
func (PostgreSQLQueryDialect) FTSJoin() string { return "" }

// BuildFTSTerm for PostgreSQL to_tsquery: sanitize each term, append :* for
// prefix match, AND them with " & ". Replace tsquery operators with spaces
// to prevent syntax errors on user-supplied terms.
func (PostgreSQLQueryDialect) BuildFTSTerm(terms []string) (expr string, arg string) {
	tsTerms := make([]string, 0, len(terms))
	for _, term := range terms {
		// Strip tsquery metacharacters so user input can't produce invalid tsquery.
		clean := tsqueryEscape(term)
		if clean == "" {
			continue
		}
		tsTerms = append(tsTerms, clean+":*")
	}
	if len(tsTerms) == 0 {
		// Pathological: user supplied only whitespace / metachars. Use a
		// condition that matches nothing rather than generating invalid SQL.
		return "FALSE", ""
	}
	return "m.search_fts @@ to_tsquery('simple', ?)", strings.Join(tsTerms, " & ")
}

// tsqueryEscape removes PostgreSQL tsquery metacharacters and whitespace,
// leaving a single alphanumeric+unicode token. Returns "" if nothing remains.
func tsqueryEscape(s string) string {
	// Remove &, |, !, (, ), :, and all whitespace — these are tsquery operators.
	var b strings.Builder
	for _, r := range s {
		switch r {
		case '&', '|', '!', '(', ')', ':', '*', '\\', '\'':
			// skip
		default:
			if r == ' ' || r == '\t' || r == '\n' || r == '\r' {
				continue
			}
			b.WriteRune(r)
		}
	}
	return b.String()
}
