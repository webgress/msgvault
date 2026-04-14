# PostgreSQL Backend Status

This document tracks the state of PostgreSQL backend support in msgvault.

## Summary

**PostgreSQL is now functionally supported end-to-end.** The full test suite
(all 37+ packages) passes against a real PostgreSQL 16 instance as well as
SQLite (default).

PR1 (tag `pr1-dialect-extraction`) extracted all SQLite-specific behavior
behind a `Dialect` interface. Zero functional change.

PR2 (tag `pr2-postgresql-dialect`) added the `PostgreSQLDialect` scaffold,
schema_pg.sql, pgx driver, and unit tests. PostgreSQL was not yet functional.

PR3 (tag `pr3-postgresql-functional`) made PostgreSQL actually work:

- Complete PG-native schema (BIGINT GENERATED ALWAYS AS IDENTITY,
  TIMESTAMPTZ, BYTEA, JSONB, TSVECTOR + GIN)
- Every store-layer query rebinds ? → $N via a central helper
- Every INSERT-returning-ID uses `RETURNING id` instead of LastInsertId
- Connection-scoped PG settings (statement_timeout, default_transaction_read_only)
  applied via libpq `options=-c ...` so all pool connections inherit them
- FTS placeholders are consistent (`?` everywhere; single Rebind pass)
- FTS backfill uses LEFT-JOIN semantics on both backends
- tsvector search uses `to_tsquery` with `:*` prefix matching
  (parity with SQLite FTS5's `"term"*` prefix matching)
- `pg_database_size()` for GetStats on PG
- Query engine parameterized by a small query-package Dialect; single
  SQLiteEngine struct serves both backends via constructor selection
- `NewEngine(db, isPostgres)` / `NewPostgreSQLEngine(db)` constructors
- All cmd/msgvault engine construction sites updated

## Running Tests

### SQLite (default)

```bash
make test
```

### PostgreSQL

```bash
# Start a PostgreSQL instance (14+), then:
export MSGVAULT_TEST_DB=postgres://user:pass@localhost:5432/msgvault_test
make test-pg
```

Each test uses an isolated schema (`msgvault_test_<hex>`) created and dropped
in the cleanup hook — no cross-test contamination.

A handful of tests that exercise intentionally-invalid inputs (e.g., inserting
`'not-a-number'` where an auto-incrementing ID would go, or `'garbage'` into a
timestamp column) are skipped on PostgreSQL because PG correctly rejects
invalid input while SQLite coerces it. Use `testutil.SkipIfPostgres(t, reason)`
to mark such tests.

## Architecture

### store.Dialect (full interface)

Lives in `internal/store/dialect.go`. Implementations:
- `SQLiteDialect` (`dialect_sqlite.go`)
- `PostgreSQLDialect` (`dialect_pg.go`)

Covers: Rebind, timestamp functions, conflict-ignore syntax, FTS
schema/upsert/search/backfill, connection lifecycle, schema migration,
error classification, WAL checkpoint, database-size query.

### query.Dialect (smaller interface)

Lives in `internal/query/dialect.go`. Implementations:
- `SQLiteQueryDialect`
- `PostgreSQLQueryDialect`

Covers only the SQL differences that surface in the query-engine layer:
Rebind, time-truncation expression, FTS search expression, FTS join, FTS
term-building, and existence-probe SQL.

The two interfaces are intentionally separate to avoid a circular dependency
between `internal/store` and `internal/query`.

### Engine construction

```go
// Direct:
engine := query.NewSQLiteEngine(db)       // SQLite
engine := query.NewPostgreSQLEngine(db)   // PostgreSQL

// Factory:
engine := query.NewEngine(s.DB(), s.IsPostgres())
```

`store.Store.IsPostgres()` and `store.Store.DriverName()` let callers pick
without importing the store package from within query.

## Known Limitations

- **Subset copy (`subset.go`) is SQLite-only.** It uses PRAGMA introspection
  and ATTACH DATABASE heavily. A PG equivalent would require a different
  design (e.g., `pg_dump`/`COPY`) and is out of scope.
- **Ranking differences between SQLite FTS5 and PostgreSQL tsvector.**
  PG applies `setweight('A')` to subject and `'B'` to sender, unweighting
  body/to/cc. SQLite's FTS5 `rank` uses unweighted BM25. Relative ordering
  of top results can differ for the same query.
- **Schema migrations run only on SQLite.** The incremental ADD COLUMN
  statements apply to pre-existing SQLite databases. PG databases are always
  initialized from the complete schema_pg.sql.
