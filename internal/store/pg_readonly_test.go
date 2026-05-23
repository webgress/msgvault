//go:build integration

package store_test

import (
	"context"
	"errors"
	"os"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"go.kenn.io/msgvault/internal/store"
)

// TestPostgreSQLReadOnly verifies that store.OpenReadOnly returns a connection
// where writes are rejected by the server (PG error code 25006,
// read_only_sql_transaction) and reads still succeed. This is the regression
// guard for the pgx RuntimeParams approach used in openPostgresReadOnly.
func TestPostgreSQLReadOnly(t *testing.T) {
	dsn := os.Getenv("MSGVAULT_TEST_DB")
	if dsn == "" {
		t.Skip("MSGVAULT_TEST_DB not set")
	}

	// Ensure schema exists so the SELECT below has something to read.
	rw, err := store.Open(dsn)
	if err != nil {
		t.Fatalf("Open (rw): %v", err)
	}
	if err := rw.InitSchema(); err != nil {
		_ = rw.Close()
		t.Fatalf("InitSchema: %v", err)
	}
	_ = rw.Close()

	s, err := store.OpenReadOnly(dsn)
	if err != nil {
		t.Fatalf("OpenReadOnly: %v", err)
	}
	defer func() { _ = s.Close() }()

	ctx := context.Background()

	// Write must fail with read-only error (PG error code 25006).
	_, err = s.DB().ExecContext(ctx, "INSERT INTO messages (source_id) VALUES (0)")
	if err == nil {
		t.Fatal("expected write to fail on read-only connection, got nil error")
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		if pgErr.Code != "25006" {
			t.Errorf("expected PG error code 25006 (read_only_sql_transaction), got %s: %s", pgErr.Code, pgErr.Message)
		}
	} else {
		t.Errorf("expected *pgconn.PgError, got %T: %v", err, err)
	}

	// Read must succeed.
	var n int
	if err := s.DB().QueryRowContext(ctx, "SELECT COUNT(*) FROM messages").Scan(&n); err != nil {
		t.Errorf("SELECT failed on read-only connection: %v", err)
	}
}
