package store

import (
	"net/url"
	"strings"
	"testing"
)

// TestApplyPgDefaults_URLComposition verifies that applyPgDefaults cleanly
// preserves existing URL query parameters (search_path, sslmode) and merges
// rather than clobbers an existing libpq "options" parameter.
func TestApplyPgDefaults_URLComposition(t *testing.T) {
	tests := []struct {
		name            string
		dbURL           string
		extras          map[string]string
		wantParamChecks map[string][]string // key → substrings that must appear in the param value
	}{
		{
			name:  "bare URL gets statement_timeout",
			dbURL: "postgres://user:pass@localhost:5432/db",
			wantParamChecks: map[string][]string{
				"options": {"-c", "statement_timeout=30000"},
			},
		},
		{
			name:  "URL with existing sslmode preserves it",
			dbURL: "postgres://user:pass@localhost:5432/db?sslmode=disable",
			wantParamChecks: map[string][]string{
				"sslmode": {"disable"},
				"options": {"statement_timeout=30000"},
			},
		},
		{
			name:  "URL with search_path preserves it alongside options",
			dbURL: "postgres://user:pass@localhost:5432/db?search_path=myschema",
			wantParamChecks: map[string][]string{
				"search_path": {"myschema"},
				"options":     {"statement_timeout=30000"},
			},
		},
		{
			name:  "URL with existing options merges rather than clobbers",
			dbURL: "postgres://user:pass@localhost:5432/db?options=-c%20application_name%3Dmyapp",
			wantParamChecks: map[string][]string{
				"options": {"statement_timeout=30000", "application_name=myapp"},
			},
		},
		{
			name:  "extras merge with existing options",
			dbURL: "postgres://user:pass@localhost:5432/db",
			extras: map[string]string{
				"default_transaction_read_only": "on",
			},
			wantParamChecks: map[string][]string{
				"options": {"statement_timeout=30000", "default_transaction_read_only=on"},
			},
		},
		{
			name:  "extras override existing defaults for same key",
			dbURL: "postgres://user:pass@localhost:5432/db",
			extras: map[string]string{
				"statement_timeout": "5000",
			},
			wantParamChecks: map[string][]string{
				"options": {"statement_timeout=5000"},
			},
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := applyPgDefaults(tc.dbURL, tc.extras)
			u, err := url.Parse(got)
			if err != nil {
				t.Fatalf("applyPgDefaults returned invalid URL %q: %v", got, err)
			}
			q := u.Query()
			for key, wantSubstrings := range tc.wantParamChecks {
				v := q.Get(key)
				if v == "" {
					t.Errorf("param %q missing from result URL; got %q", key, got)
					continue
				}
				for _, want := range wantSubstrings {
					if !strings.Contains(v, want) {
						t.Errorf("param %q = %q, expected to contain %q", key, v, want)
					}
				}
			}
			// Must not produce duplicate "options" entries.
			if strings.Count(u.RawQuery, "options=") > 1 {
				t.Errorf("URL has duplicate options= entries: %q", got)
			}
			// For the "override" test, the result should contain statement_timeout=5000
			// and NOT statement_timeout=30000.
			if tc.name == "extras override existing defaults for same key" {
				if strings.Contains(q.Get("options"), "statement_timeout=30000") {
					t.Errorf("default not overridden; got %q", q.Get("options"))
				}
			}
		})
	}
}

func TestBuildPgOptionsValue_StableOrder(t *testing.T) {
	// Given the same inputs, buildPgOptionsValue should produce the same output.
	a := buildPgOptionsValue("", map[string]string{"a": "1", "b": "2", "c": "3"})
	b := buildPgOptionsValue("", map[string]string{"c": "3", "b": "2", "a": "1"})
	if a != b {
		t.Errorf("non-deterministic output:\n  a=%q\n  b=%q", a, b)
	}
}
