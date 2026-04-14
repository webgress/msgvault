package store

import (
	"os"
	"strings"
	"testing"

	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestPostgresPoolWideSettings verifies that statement_timeout and
// default_transaction_read_only are set for every pool connection,
// not just the first one (the regression we want to avoid is a
// single-connection SET that other pool members don't inherit).
//
// Runs only when MSGVAULT_TEST_DB is set to a PostgreSQL URL.
func TestPostgresPoolWideSettings(t *testing.T) {
	dbURL := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(dbURL, "postgres://") && !strings.HasPrefix(dbURL, "postgresql://") {
		t.Skip("MSGVAULT_TEST_DB not a PostgreSQL URL")
	}

	st, err := Open(dbURL)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	// Probe multiple connections from the pool. database/sql picks a
	// random idle connection for each query — running many queries makes
	// it likely we hit several distinct pool members.
	for i := 0; i < 10; i++ {
		var timeout string
		if err := st.DB().QueryRow("SHOW statement_timeout").Scan(&timeout); err != nil {
			t.Fatalf("SHOW statement_timeout: %v", err)
		}
		if timeout == "0" || timeout == "" {
			t.Errorf("connection %d: statement_timeout = %q, want non-zero", i, timeout)
		}
	}
}

// TestPostgresReadOnlyPoolWide verifies read-only mode propagates to all
// pool connections when Store is opened via OpenReadOnly.
func TestPostgresReadOnlyPoolWide(t *testing.T) {
	dbURL := os.Getenv("MSGVAULT_TEST_DB")
	if !strings.HasPrefix(dbURL, "postgres://") && !strings.HasPrefix(dbURL, "postgresql://") {
		t.Skip("MSGVAULT_TEST_DB not a PostgreSQL URL")
	}

	st, err := OpenReadOnly(dbURL)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })

	for i := 0; i < 10; i++ {
		var readonly string
		if err := st.DB().QueryRow("SHOW default_transaction_read_only").Scan(&readonly); err != nil {
			t.Fatalf("SHOW default_transaction_read_only: %v", err)
		}
		if readonly != "on" {
			t.Errorf("connection %d: default_transaction_read_only = %q, want on", i, readonly)
		}
	}
}
