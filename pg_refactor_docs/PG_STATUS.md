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

## Full-Text Search

Both backends support full-text search with the same public API
(`UpsertFTS`, `BackfillFTS`, `SearchMessages`). Portable integration
tests in `internal/storeext/fts_e2e_test.go` verify identical behavior
across dialects:

- `TestFTSEndToEnd` — upsert + search + no-match
- `TestFTSUpsertReplacesPriorContent` — replace semantics
- `TestBackfillFTSRepopulatesIndex` — backfill flow
- `TestRemoveSourceClearsSearchResults` — cascade cleanup
- `TestFTSSearchSQLInjection` — metacharacter safety

Backend implementation details:

**SQLite**: FTS5 virtual table `messages_fts` populated via
`INSERT OR REPLACE`. Search uses `MATCH` + FTS5 rank.

**PostgreSQL**: `tsvector` column `messages.search_fts` with GIN index.
Search uses `@@ to_tsquery('simple', ?)`. The FTS upsert/backfill
pre-processes email addresses with `REPLACE('@', ' ')` and
`REPLACE('.', ' ')` so individual tokens (user, domain) are searchable
— the 'simple' tsvector config would otherwise treat full addresses as
single tokens.

User input for both backends is normalized through
`Dialect.SanitizeFTSQuery(query)`. SQLite strips FTS5 metacharacters
(`*`, `:`, `-`, `(`, `)`, `.`, `"`) and wraps the result in quotes with
a `*` prefix suffix. PostgreSQL strips tsquery operators
(`&`, `|`, `!`, `(`, `)`, `:`, `*`, `\`, `'`), splits on word-boundary
punctuation, and joins tokens with ` & ` and a `:*` suffix for prefix
matching.

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
- **DuckDB/Parquet analytics (`cmd msgvault build-cache`) is unaffected**
  but its SQL uses SQLite-flavored `strftime` and `messages_fts MATCH`
  from DuckDB's own dialect — these paths work against the Parquet files
  the cache builder produces, not the live Store, so they're dialect-independent.

## Test Timing (local PostgreSQL 16)

Full suite against PostgreSQL completes in ~60s wall clock. The
`internal/store` package dominates (~40s) due to per-test schema
create/drop overhead. Individual tests are all well under 1s. SQLite
runs the same suite in ~12s.
