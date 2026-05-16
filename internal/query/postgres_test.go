package query

import (
	"strings"
	"testing"
)

var _ Engine = (*SQLiteEngine)(nil)
var _ Engine = (*DuckDBEngine)(nil)

// TestPostgresEngineUsesDialect verifies that NewPostgreSQLEngine creates an engine
// with the PostgreSQL query dialect (Rebind converts ? to $N).
func TestPostgresEngineUsesDialect(t *testing.T) {
	e := NewPostgreSQLEngine(nil)
	if _, ok := e.dialect.(PostgreSQLQueryDialect); !ok {
		t.Fatalf("NewPostgreSQLEngine dialect = %T, want PostgreSQLQueryDialect", e.dialect)
	}
	reboundQuery := e.dialect.Rebind("SELECT ? WHERE id = ?")
	if !strings.Contains(reboundQuery, "$1") || !strings.Contains(reboundQuery, "$2") {
		t.Fatalf("Rebind did not convert ? to $N: %q", reboundQuery)
	}
}

// TestPostgresTimeTruncExpression verifies the PostgreSQL time truncation expressions.
func TestPostgresTimeTruncExpression(t *testing.T) {
	d := PostgreSQLQueryDialect{}
	for _, tc := range []struct {
		gran string
		want string
	}{
		{"year", "to_char(col, 'YYYY')"},
		{"month", "to_char(col, 'YYYY-MM')"},
		{"day", "to_char(col, 'YYYY-MM-DD')"},
	} {
		got := d.TimeTruncExpression("col", tc.gran)
		if got != tc.want {
			t.Errorf("TimeTruncExpression(%q, %q) = %q, want %q", "col", tc.gran, got, tc.want)
		}
	}
}
