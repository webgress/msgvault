package store

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"sync"

	"github.com/pressly/goose/v3"
)

// gooseMu serializes access to goose's global SetBaseFS/SetDialect state so
// concurrent Store opens (e.g. parallel tests with different backends) can't
// race on the dialect/FS configuration during migration runs.
var gooseMu sync.Mutex

// gooseDialect returns the goose dialect name and the embedded directory of
// SQL migrations that apply to the current Store.
func (s *Store) gooseDialect() (dialect, dir string) {
	if s.IsPostgreSQL() {
		return "postgres", "migrations/postgres"
	}
	return "sqlite3", "migrations/sqlite"
}

// runMigrations applies all pending goose migrations against the Store's
// underlying database. For pre-goose databases (e.g. those originally
// initialized by the legacy schema-load path), this also seeds the
// goose_db_version table so the existing schema is treated as the
// "version 1" baseline and future migrations apply on top.
func (s *Store) runMigrations(ctx context.Context) error {
	dialect, dir := s.gooseDialect()
	db := s.db.DB

	gooseMu.Lock()
	defer gooseMu.Unlock()

	goose.SetBaseFS(migrationsFS)
	if err := goose.SetDialect(dialect); err != nil {
		return fmt.Errorf("goose set dialect %q: %w", dialect, err)
	}

	if err := s.bootstrapGooseIfNeeded(ctx, db, dir); err != nil {
		return fmt.Errorf("goose bootstrap: %w", err)
	}

	if err := goose.UpContext(ctx, db, dir); err != nil {
		return fmt.Errorf("goose up: %w", err)
	}
	return nil
}

// bootstrapGooseIfNeeded marks the legacy pre-goose schema as the goose
// baseline. The caller has already acquired gooseMu.
//
// Decision matrix:
//   - goose_db_version exists       → already managed; no-op.
//   - messages table exists         → legacy DB; create the goose version
//     table and insert one row per embedded migration so the next
//     goose.UpContext is a no-op.
//   - neither exists                → fresh DB; let goose.UpContext create
//     the version table and apply migrations normally.
func (s *Store) bootstrapGooseIfNeeded(ctx context.Context, db *sql.DB, dir string) error {
	hasGoose, err := s.tableExists(ctx, db, "goose_db_version")
	if err != nil {
		return fmt.Errorf("check goose_db_version: %w", err)
	}
	if hasGoose {
		return nil
	}

	hasMessages, err := s.tableExists(ctx, db, "messages")
	if err != nil {
		return fmt.Errorf("check messages table: %w", err)
	}
	if !hasMessages {
		return nil
	}

	// Legacy DB: pre-create the version table and mark each embedded
	// migration as applied so the schema we already have is treated as
	// the baseline.
	if _, err := goose.EnsureDBVersionContext(ctx, db); err != nil {
		return fmt.Errorf("ensure goose version table: %w", err)
	}

	versions, err := embeddedMigrationVersions(dir)
	if err != nil {
		return fmt.Errorf("scan embedded migrations: %w", err)
	}
	for _, v := range versions {
		if err := s.insertGooseVersion(ctx, db, v); err != nil {
			return fmt.Errorf("insert goose version %d: %w", v, err)
		}
	}
	return nil
}

func (s *Store) tableExists(ctx context.Context, db *sql.DB, name string) (bool, error) {
	var exists bool
	if s.IsPostgreSQL() {
		// Use current_schema() so the test harness's per-schema isolation
		// (search_path is set on connect) reports correctly.
		err := db.QueryRowContext(ctx,
			`SELECT EXISTS(
				SELECT 1 FROM information_schema.tables
				WHERE table_schema = current_schema() AND table_name = $1
			)`, name,
		).Scan(&exists)
		if err != nil {
			return false, err
		}
		return exists, nil
	}

	var count int
	err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM sqlite_master WHERE type='table' AND name=?`, name,
	).Scan(&count)
	if err != nil {
		return false, err
	}
	return count > 0, nil
}

func (s *Store) insertGooseVersion(ctx context.Context, db *sql.DB, version int64) error {
	if s.IsPostgreSQL() {
		_, err := db.ExecContext(ctx,
			`INSERT INTO goose_db_version (version_id, is_applied) VALUES ($1, $2)`,
			version, true,
		)
		return err
	}
	_, err := db.ExecContext(ctx,
		`INSERT INTO goose_db_version (version_id, is_applied) VALUES (?, ?)`,
		version, 1,
	)
	return err
}

// embeddedMigrationVersions returns the goose version IDs of every .sql
// migration under the given embedded directory, in ascending order.
func embeddedMigrationVersions(dir string) ([]int64, error) {
	entries, err := fs.ReadDir(migrationsFS, dir)
	if err != nil {
		return nil, err
	}
	versions := make([]int64, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		v, err := goose.NumericComponent(e.Name())
		if err != nil {
			// Skip files that don't follow the goose naming convention.
			continue
		}
		versions = append(versions, v)
	}
	return versions, nil
}
