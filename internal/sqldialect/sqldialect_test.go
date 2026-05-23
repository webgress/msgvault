package sqldialect

import "testing"

func TestRebindPostgreSQL(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"none", "SELECT 1", "SELECT 1"},
		{"one", "SELECT * FROM t WHERE a = ?", "SELECT * FROM t WHERE a = $1"},
		{"two", "INSERT INTO t (a, b) VALUES (?, ?)", "INSERT INTO t (a, b) VALUES ($1, $2)"},
		{"quoted_question_mark", "SELECT '? literal' FROM t WHERE x = ?",
			"SELECT '? literal' FROM t WHERE x = $1"},
		{"three_alternating",
			"SELECT * FROM t WHERE a = ? AND b = '?' AND c = ?",
			"SELECT * FROM t WHERE a = $1 AND b = '?' AND c = $2"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := RebindPostgreSQL(tc.in); got != tc.want {
				t.Errorf("RebindPostgreSQL(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}

func TestEscapeTSQueryTerm(t *testing.T) {
	tests := []struct {
		name, in, want string
	}{
		{"plain", "invoice", "invoice"},
		{"unicode_kept", "café", "café"},
		{"strip_meta", "in&voice|!", "invoice"},
		{"strip_whitespace", "hello world", "helloworld"},
		{"all_meta_empty", "&|!():*\\'", ""},
		{"colon_stripped", "user:foo", "userfoo"},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := EscapeTSQueryTerm(tc.in); got != tc.want {
				t.Errorf("EscapeTSQueryTerm(%q) = %q, want %q", tc.in, got, tc.want)
			}
		})
	}
}
