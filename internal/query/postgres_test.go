package query

import (
	"strings"
	"testing"
)

var _ Engine = (*SQLiteEngine)(nil)
var _ Engine = (*DuckDBEngine)(nil)
var _ Engine = (*PostgreSQLEngine)(nil)

func TestPostgresStatsWhereClauseParenthesizesEmailFilter(t *testing.T) {
	sourceID := int64(42)
	opts := StatsOptions{
		SourceID:              &sourceID,
		WithAttachmentsOnly:   true,
		HideDeletedFromSource: true,
	}

	where, args := postgresStatsWhereClause(opts)

	if !strings.HasPrefix(where, "(") {
		t.Fatalf("where clause should start with parenthesized email filter, got %q", where)
	}
	if !strings.Contains(where, emailOnlyFilterM) {
		t.Fatalf("where clause should include shared email filter %q, got %q", emailOnlyFilterM, where)
	}
	if strings.Contains(where, "m.message_type = 'email' OR m.message_type IS NULL OR m.message_type = '' AND") {
		t.Fatalf("where clause has unparenthesized OR/AND precedence bug: %q", where)
	}
	if !strings.Contains(where, "m.source_id = $1") {
		t.Fatalf("where clause should include source filter with first placeholder, got %q", where)
	}
	if len(args) != 1 || args[0] != sourceID {
		t.Fatalf("args = %#v, want [%d]", args, sourceID)
	}
}
