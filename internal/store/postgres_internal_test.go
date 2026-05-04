package store

import (
	"database/sql"
	"strings"
	"testing"
)

func TestPostgresColumnExistsSQLScopesToCurrentSchema(t *testing.T) {
	query := postgresColumnExistsSQL("messages", "search_fts")

	for _, want := range []string{
		"table_schema = current_schema()",
		"table_name = 'messages'",
		"column_name = 'search_fts'",
	} {
		if !strings.Contains(query, want) {
			t.Fatalf("postgres column query should contain %q, got %q", want, query)
		}
	}
}

func TestPostgresConnConfigRuntimeParams(t *testing.T) {
	cfg, err := postgresConnConfig("postgres://user:pass@example.com:5432/msgvault", true)
	if err != nil {
		t.Fatalf("postgresConnConfig: %v", err)
	}

	if got := cfg.RuntimeParams["statement_timeout"]; got != "30s" {
		t.Fatalf("statement_timeout = %q, want 30s", got)
	}
	if got := cfg.RuntimeParams["default_transaction_read_only"]; got != "on" {
		t.Fatalf("default_transaction_read_only = %q, want on", got)
	}
}

func TestStoreCloseRunsRegisteredCleanup(t *testing.T) {
	db, err := sql.Open("sqlite3", ":memory:")
	if err != nil {
		t.Fatalf("open sqlite: %v", err)
	}

	called := false
	st := &Store{
		db:      newLoggedDB(db, nil),
		dialect: &SQLiteDialect{},
		closeCleanup: func() {
			called = true
		},
	}

	if err := st.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if !called {
		t.Fatal("Close did not run registered cleanup")
	}
}
