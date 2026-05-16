# PostgreSQL Backend Status

This document tracks the state of PostgreSQL backend support in msgvault.

## Summary

PR1 (`pr1-dialect-extraction`) extracted SQLite-specific behavior behind a
`Dialect` interface (zero functional change).

PR2 (`pr2-postgresql-dialect`) added the foundational scaffolding:
`PostgreSQLDialect`, pgx driver wiring, `schema_pg.sql` stub,
`PostgreSQLEngine` scaffold, and the dual-backend test harness via
`MSGVAULT_TEST_DB`.

**PR3 (this branch) makes the store layer functional against PostgreSQL.**
A PostgreSQL connection can now initialize the schema, insert rows, run FTS
queries, and serve the TUI / HTTP / MCP aggregate paths. The SQLite path is
unchanged.

PR4 (future) will address remaining functional gaps in deletion execution,
attachment storage on PG, and end-to-end coverage under
`MSGVAULT_TEST_DB=postgres://...`.

## What Works

- `PostgreSQLDialect.Rebind()` correctly converts `?` → `$1, $2, ...`
  (including quoted-string safety)
- `PostgreSQLDialect.Now()`, `InsertOrIgnore()` (complete + prefix),
  `InsertOrIgnoreSuffix()`, `FTSSearchClause()`, `UpdateOrIgnore()`
- `PostgreSQLDialect.LegacyColumnMigrations()` returns the empty list — the
  PostgreSQL schema is always shipped complete via `schema_pg.sql`, so no
  legacy `ALTER TABLE` migration loop is needed
- `PostgreSQLDialect.DatabaseSize()` reports `pg_database_size(...)`
- `PostgreSQLDialect` error-code classification (23505, 42701, 42P01)
- `Open("postgres://...")` establishes a connection with pool settings
- `OpenReadOnly` for PostgreSQL enforces `default_transaction_read_only=on`
  via pgx `RuntimeParams` (set on every pooled connection at startup)
- `schema_pg.sql` is loaded by the dialect and contains PostgreSQL-native
  DDL: `BIGINT GENERATED ALWAYS AS IDENTITY`, `TIMESTAMPTZ`, `BYTEA`,
  `JSONB`, tsvector column + GIN index for FTS
- `Rebind()` is threaded through every store-layer query via the
  `loggedDB` / `loggedTx` wrapper — call sites can emit portable `?`
  placeholders and the wrapper applies the dialect-specific rewrite
- `RETURNING id` replaces `LastInsertId()` at every insert call site
  (`messages.go`, `sync.go`)
- `queryInChunks` / `insertInChunks` use `loggedTx` (auto-rebind); chunked
  `INSERT OR IGNORE` builders use `dialect.InsertOrIgnorePrefix/Suffix`
- `SearchMessages` / `SearchMessagesQuery` use uniform `?` placeholders
  through `FTSSearchClause()`, then the whole statement is rebound by
  `loggedDB` — no mixed `?` / `$N` styles
- `FTSBackfillBatchSQL` uses `LEFT JOIN message_bodies` so messages
  without a body row are still indexed (header-only FTS for that row)
- `GetStats` uses `dialect.DatabaseSize()` instead of `os.Stat` on the DSN
- `PostgreSQLEngine` (now a dialect-parameterized `SQLiteEngine`)
  implements the full `Engine` surface for aggregates, search, and
  message detail using the query-layer `Dialect` interface
- `query.NewEngine(db, isPostgres)` factory is wired in every engine
  construction site under `cmd/msgvault/cmd/`
- `Store.IsPostgreSQL()` lets callers dispatch without an
  `internal/query` dependency
- Unit tests for dialect string methods pass without a live Postgres
- SQLite regression: all existing tests pass unmodified

## Resolved in PR3

| # | Blocker | Resolution |
|---|---------|-----------|
| 1 | Schema type translation | `schema_pg.sql` with PostgreSQL-native DDL |
| 2 | Rebind threading through store layer | `loggedDB` / `loggedTx` apply `Rebind` to every statement |
| 3 | `queryInChunks` / `insertInChunks` dialect-aware | Use `loggedTx` (auto-rebind) + `InsertOrIgnorePrefix/Suffix` |
| 4 | `LastInsertId` → `RETURNING id` | Done at every insert call site |
| 5 | Mixed placeholder styles in search | All placeholders are `?`, rebound at execution |
| 6 | FTS backfill LEFT JOIN | `FTSBackfillBatchSQL` uses LEFT JOIN |
| 7 | `statement_timeout` pool-wide | Set via pgx `RuntimeParams` (PR2) |
| 8 | `GetStats` for PostgreSQL | `dialect.DatabaseSize()` |
| 9 | `PostgreSQLEngine` method implementations | Dialect-parameterized `SQLiteEngine` |
| 10 | `PostgreSQLEngine` wired to factory | `query.NewEngine(db, isPostgres)` in cmd/ |

## Remaining for PR4

- **FTS weight differences**: PostgreSQL applies `setweight('A')` to the
  subject and `'B'` to the sender; SQLite FTS5 has no weighting. Ranking
  results will still differ between backends.
- **Deletion execution path on PostgreSQL**: end-to-end testing of
  staged-deletion → Gmail delete → archive update.
- **Attachment storage paths** under PostgreSQL — content-hash dedup
  and orphan-cleanup paths haven't been exercised end-to-end yet.
- **CI coverage** under `MSGVAULT_TEST_DB=postgres://...`: the harness
  exists and tests are portable, but no upstream CI lane runs it yet.

## Running Tests Against PostgreSQL

```bash
# Start a PostgreSQL instance, then:
export MSGVAULT_TEST_DB=postgres://user:pass@localhost:5432/msgvault_test
make test
```

Each test creates and drops its own schema (`msgvault_test_<hex>`) for
isolation. The `testutil.NewTestStore()` helper detects the env var and
routes accordingly. If `MSGVAULT_TEST_DB` is unset, SQLite is used.

## Full-Text Search

FTS is implemented per-dialect behind a small surface (`Dialect.FTSUpsert`,
`FTSSearchClause`, `FTSAvailable`, `SanitizeFTSQuery`). The store layer
calls into these and is otherwise dialect-agnostic.

| Concern | SQLite | PostgreSQL |
|---|---|---|
| Index location | `messages_fts` virtual table (FTS5) | `messages.search_fts` tsvector column + GIN index |
| Search clause | `messages_fts MATCH ?` | `m.search_fts @@ to_tsquery('simple', ?)` |
| Sanitization | strip `"*:-().`, wrap as `"term"*` | strip tsquery ops; replace `@./-/,;"` with spaces; emit `term:*` joined by ` & ` |
| Rank | implicit `rank` column | `ts_rank(m.search_fts, to_tsquery('simple', ?))` |
| Backfill | `INSERT OR REPLACE INTO messages_fts ...` | `UPDATE messages SET search_fts = setweight(...)` |

### Email tokenization on PG

PostgreSQL's 'simple' tsvector config indexes `alice@example.com` as a
single token, so searching for `alice` alone would miss the row. Both
`FTSUpsert` and `FTSBackfillBatchSQL` pre-process address fields with
`REPLACE(@, ' ')` and `REPLACE(., ' ')` so the components become
individually searchable. The same transformation runs over user input
inside `SanitizeFTSQuery` so the query and document tokenization stay
aligned.

### Portable coverage

The store-layer FTS path is exercised through public-API tests that run
identically against both backends (set `MSGVAULT_TEST_DB` to a PG URL
to run them on PostgreSQL). These cover upsert, search, replace-on-update,
backfill, cascade cleanup, and metacharacter handling. SQLite-specific
tests that inspect `messages_fts` directly are kept and skipped on PG.
