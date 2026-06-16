package store

import "context"

// ParseDBTime is exported for testing unexported timestamp parsing behavior.
var ParseDBTime = parseDBTime

// SetFTS5AvailableForTest flips the cached availability flag. Tests use this
// to exercise the guarantee that RebuildFTS works even when FTS5 looks
// unavailable — the symptom that motivates a rebuild in the first place.
func SetFTS5AvailableForTest(s *Store, v bool) {
	s.fts5Available = v
}

// SetBackfillFTSBatchErrHookForTest installs (or, with nil, clears) the
// test-only hook that forces backfillFTSBatch to fail for a chosen id range.
// Tests use it to deterministically trigger backfillFTSRowByRow's
// skip-the-bad-row-and-continue fallback. Returns a restore func that clears
// the hook, so callers can defer it.
func SetBackfillFTSBatchErrHookForTest(fn func(fromID, toID int64) error) func() {
	backfillFTSBatchErrHook = fn
	return func() { backfillFTSBatchErrHook = nil }
}

// MigrateTableOrderForTest returns a copy of the cross-backend copy order so
// external tests can assert FK-graph coverage without exporting the slice.
func MigrateTableOrderForTest() []string {
	out := make([]string, len(migrateTableOrder))
	copy(out, migrateTableOrder)
	return out
}

// TableContentHashForTest exposes the unexported per-table content hash so tests
// can assert it is independent of physical column order (a legacy SQLite source
// whose columns were ALTER-appended in a different order than a fresh
// destination must still hash equal for identical data).
func TableContentHashForTest(ctx context.Context, st *Store, table string) (string, error) {
	return tableContentHash(ctx, st, table)
}

// TableContentHashOrderedForTest exposes the order-parameterized content hash so
// tests can prove the digest is invariant to the SQL row order for TEXT-keyed
// tables (the property that makes it independent of backend ORDER BY collation).
func TableContentHashOrderedForTest(ctx context.Context, st *Store, table, order string) (string, error) {
	return tableContentHashOrdered(ctx, st, table, order)
}

// PGForeignKeyEdgesForTest returns "child.col->parent.col" strings for every
// foreign key the dynamic PG orphan check enumerates, so tests can assert the
// catalog-driven coverage spans all FK edges (not a hand-curated subset).
func PGForeignKeyEdgesForTest(ctx context.Context, dst *Store) ([]string, error) {
	fks, err := pgForeignKeys(ctx, dst)
	if err != nil {
		return nil, err
	}
	out := make([]string, len(fks))
	for i, fk := range fks {
		out[i] = fk.childTable + "." + fk.childCol + "->" + fk.parentTbl + "." + fk.parentCol
	}
	return out, nil
}
