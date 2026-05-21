package store_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/wesm/msgvault/internal/store"
	"github.com/wesm/msgvault/internal/testutil"
)

// TestMigration_FreshDB verifies that opening a brand-new database and
// calling InitSchema produces (a) the application schema (messages, etc.)
// and (b) the goose_db_version table populated with at least version 1.
func TestMigration_FreshDB(t *testing.T) {
	st := testutil.NewTestStore(t)

	if !tableExists(t, st, "messages") {
		t.Error("expected messages table after InitSchema")
	}
	if !tableExists(t, st, "goose_db_version") {
		t.Error("expected goose_db_version table after InitSchema")
	}

	version := maxGooseVersion(t, st)
	if version < 1 {
		t.Errorf("goose_db_version max = %d, want >= 1", version)
	}
}

// TestMigration_ExistingDB verifies the legacy-bootstrap path: a database
// that already contains the application schema but no goose_db_version
// table gets the version table created and populated with the embedded
// migration versions, and the preexisting data is preserved.
//
// We simulate a pre-goose database by initializing it normally, inserting
// a sentinel row, and then dropping goose_db_version. Reopening and
// calling InitSchema must take the bootstrap path.
func TestMigration_ExistingDB(t *testing.T) {
	testutil.SkipIfPostgres(t, "test reopens a SQLite file at a fixed path")

	dbPath := filepath.Join(t.TempDir(), "legacy.db")

	// Phase 1: build a real schema and seed it with data.
	st, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	if err := st.InitSchema(); err != nil {
		t.Fatalf("InitSchema (initial): %v", err)
	}
	if _, err := st.DB().Exec(
		`INSERT INTO sources (source_type, identifier) VALUES ('gmail', 'legacy@example.com')`,
	); err != nil {
		t.Fatalf("seed sources row: %v", err)
	}

	// Phase 2: drop goose_db_version to simulate a database created before
	// versioned migrations were introduced.
	if _, err := st.DB().Exec(`DROP TABLE goose_db_version`); err != nil {
		t.Fatalf("drop goose_db_version: %v", err)
	}
	if err := st.Close(); err != nil {
		t.Fatalf("close store before reopen: %v", err)
	}

	// Phase 3: reopen and re-run InitSchema. Bootstrap should recreate
	// goose_db_version and mark all embedded migrations as applied,
	// without touching the existing data.
	st2, err := store.Open(dbPath)
	if err != nil {
		t.Fatalf("reopen store: %v", err)
	}
	t.Cleanup(func() { _ = st2.Close() })

	if err := st2.InitSchema(); err != nil {
		t.Fatalf("InitSchema on legacy DB: %v", err)
	}

	if !tableExists(t, st2, "goose_db_version") {
		t.Fatal("expected goose_db_version after bootstrap")
	}
	if v := maxGooseVersion(t, st2); v < 1 {
		t.Errorf("goose_db_version max = %d, want >= 1", v)
	}

	// Sentinel row must survive — bootstrap must not have dropped or
	// recreated existing tables.
	var id int64
	if err := st2.DB().QueryRow(
		`SELECT id FROM sources WHERE identifier = 'legacy@example.com'`,
	).Scan(&id); err != nil {
		t.Fatalf("read legacy source row: %v", err)
	}
	if id == 0 {
		t.Error("legacy source row id is zero")
	}
}

func tableExists(t *testing.T, st *store.Store, name string) bool {
	t.Helper()
	var exists bool
	if st.IsPostgreSQL() {
		err := st.DB().QueryRow(
			`SELECT EXISTS(
				SELECT 1 FROM information_schema.tables
				WHERE table_schema = current_schema() AND table_name = $1
			)`, name,
		).Scan(&exists)
		if err != nil {
			t.Fatalf("check %s: %v", name, err)
		}
		return exists
	}
	var count int
	err := st.DB().QueryRow(
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name,
	).Scan(&count)
	if err != nil {
		t.Fatalf("check %s: %v", name, err)
	}
	return count > 0
}

func maxGooseVersion(t *testing.T, st *store.Store) int64 {
	t.Helper()
	var v sql.NullInt64
	if err := st.DB().QueryRow(
		`SELECT MAX(version_id) FROM goose_db_version`,
	).Scan(&v); err != nil {
		t.Fatalf("read goose version: %v", err)
	}
	if !v.Valid {
		return 0
	}
	return v.Int64
}
