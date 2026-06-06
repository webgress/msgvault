# Codex PR4 Iteration 1 Review

## Findings

### C1 - major - cmd/msgvault/cmd/embed_vector.go:96

The PostgreSQL embedding command wires the raw pgx `*sql.DB` into code paths that still emit SQLite-only SQL. On the PG branch, `vectorsDB = pgb.DB()` and `MainDB: s.DB()` are passed to `pendingCount` and `embed.NewWorker`; those are raw handles, not the store `loggedDB` wrapper that rebinding relies on. `pendingCount` immediately runs `SELECT ... WHERE generation_id = ?`, `embed.Queue` uses `?`, `INSERT OR IGNORE`, and `json_each(?)`, and `embedBatch` builds `WHERE m.id IN (?,...)` against the raw main DB. pgx does not accept those SQLite constructs, so a PG user can create/migrate the pgvector schema and then fail before claiming or embedding any rows.

This is wrong because the PR exposes a PG branch in `msgvault embeddings` while the shared queue/worker layer is not dialect-aware. The serve path refuses PG vector features, but the embed CLI does not, so this is a broken user-facing path rather than a deliberate refusal.

Suggested fix: until `embed.Queue`, `embed.Enqueuer`, `pendingCount`, and `embedBatch` are ported to a dialect-aware API, refuse PostgreSQL in `runEmbed` with the same up-front error as `setupVectorFeatures`. If PG embeddings are intended to work in this PR, introduce a queue/enqueuer abstraction or a dialect-aware DB wrapper for the vector worker, replace `json_each`/`INSERT OR IGNORE` with PostgreSQL equivalents, and add an end-to-end PG embedding test that drives the CLI/worker path instead of only the backend.

### C2 - major - internal/vector/pgvector/schema.sql:49

The pgvector `embeddings` table is keyed only by `(generation_id, message_id)` and has no `chunk_index`, `chunk_char_start`, or `chunk_char_end`, while the `vector.Backend` contract requires backends to key vectors by `(GenerationID, MessageID, ChunkIndex)`. `Backend.Upsert` then inserts every `vector.Chunk` with `ON CONFLICT (generation_id, message_id) DO UPDATE`, so a long message that produces multiple chunks overwrites earlier chunks and leaves only the last one stored. The same method increments `message_count` by `len(chunks) - preexisting`, so a new two-chunk message records `message_count += 2` even though only one row survives.

This is wrong because the shared embed worker already chunks long messages and SQLite persists each chunk independently. PostgreSQL would silently lose recall for all but one chunk of long messages and report inconsistent generation counts.

Suggested fix: make the PG schema mirror the backend contract: add `chunk_index`, `chunk_char_start`, and `chunk_char_end`, use a unique key on `(generation_id, message_id, chunk_index)`, and update `Upsert`, `Search`, `LoadVector`, `Delete`, and count logic accordingly. Upsert should replace all prior chunks for the affected distinct message IDs before inserting the new chunk set, as the SQLite backend does, and `message_count` should be based on distinct message IDs, not chunk count. Add a PG test that upserts two chunks for one message and asserts both rows are stored and searchable.

### C3 - major - .github/workflows/ci.yml:79

The workflow defines `test-postgres` twice: once at line 79 for the pgvector image and again at line 134 for the PostgreSQL backend lane. A workflow cannot have two jobs with the same stable job ID without one definition being rejected or shadowing the other. In addition, neither path actually enables the `pgvector` build tag: the first job runs `make test`, whose default tags are `fts5 sqlite_vec`, and the second runs `make test-pg`, which uses only `fts5`.

This is wrong because the new pgvector tests are guarded by `//go:build pgvector`; with the CI tag set, `go list -tags "fts5 sqlite_vec" ./internal/vector/pgvector` reports only `ext_stub.go` and no test files. The pgvector backend can therefore regress while CI still passes.

Suggested fix: give the jobs unique IDs, for example `test-postgres` and `test-pgvector`, and make the pgvector lane run at least `go test -tags "fts5 sqlite_vec pgvector" ./internal/vector/pgvector` against the `pgvector/pgvector` service. If the full suite should validate the real backend, add `pgvector` to that lane's tags explicitly rather than relying on `make test`.

### C4 - major - internal/vector/pgvector/backend.go:681

`filteredMessageIDs` applies `SubjectSubstrings` with `m.subject LIKE $N ESCAPE '\'` on PostgreSQL. PostgreSQL `LIKE` is case-sensitive, unlike SQLite's default ASCII-insensitive `LIKE` behavior and unlike the store/query PG compatibility fixes elsewhere in the repo. A vector search filter such as `subject:invoice` will miss a message whose subject is `Invoice from Acme` on PG, while the same query matches on SQLite and in the non-vector PG search path.

This is wrong because `vector.Filter.SubjectSubstrings` is part of the cross-backend filter contract used by hybrid/vector search. The PG backend is supposed to preserve the same result set for structured filters, not silently narrow matches based on case.

Suggested fix: make the predicate case-insensitive on PG, for example `LOWER(m.subject) LIKE LOWER($N) ESCAPE '\'` or `m.subject ILIKE $N ESCAPE '\'`, while still using `escapeLikeSubject`. Add a pgvector filtered-search test with stored subject `Quarterly Invoice` and filter term `invoice`.

### C5 - minor - internal/vector/pgvector/backend.go:766

`Stats` documents that `StorageBytes` is reported with `pg_relation_size`, but the implementation only fills `EmbeddingCount` and `PendingCount` and returns with `StorageBytes` left at zero.

This is wrong because callers of the shared `vector.Stats` surface will see PostgreSQL vector storage as `0` bytes regardless of actual data, and the comment makes the missing query easy to miss in tests.

Suggested fix: query `pg_total_relation_size('embeddings')` or the intended relation-size expression and assign it to `s.StorageBytes`; if scoped per-generation sizing is not available, document that it is table-wide and test that a non-empty PG backend reports non-zero storage.

## Verification

- Ran `go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd ./internal/store ./internal/deletion ./internal/vector/pgvector`:
  - `cmd/msgvault/cmd`, `internal/store`, and `internal/deletion` passed.
  - `internal/vector/pgvector` reported `[no test files]` under the CI default tag set.
- Ran `go list -tags "fts5 sqlite_vec" -f '{{.GoFiles}} {{.TestGoFiles}}' ./internal/vector/pgvector` and confirmed only `ext_stub.go` is included.
- Ran `go list -tags "fts5 sqlite_vec pgvector" -f '{{.GoFiles}} {{.TestGoFiles}}' ./internal/vector/pgvector` and confirmed the real backend and pgvector tests become visible.
- Ran `go test -tags "fts5 sqlite_vec pgvector" ./internal/vector/pgvector`; it passed locally, with tests skipped because no live `MSGVAULT_TEST_DB` was configured.

## Resolutions (Claude, iteration 1)

All five findings verified against the code and fixed. The pgvector changes
were verified against a live PostgreSQL+pgvector instance (`MSGVAULT_TEST_DB`),
where all 21 backend tests pass (18 existing + 3 new chunk regression tests).

- **C1 (major) — fixed.** `runEmbed` now refuses PostgreSQL up-front with the
  same actionable message as `serve`, instead of failing deep in the worker on
  a SQLite-only `?`/`json_each`/`INSERT OR IGNORE` query. The pgvector branch
  is retained for when the embed queue/worker becomes dialect-aware.
  (`cmd/msgvault/cmd/embed_vector.go`)
- **C2 (major) — fixed.** pgvector `embeddings` is now keyed by
  `(generation_id, message_id, chunk_index)` with `chunk_char_start/end`
  columns, mirroring sqlitevec. `Upsert` deletes prior chunks per message then
  inserts the new set (idempotent replace), `message_count` and
  `Stats.EmbeddingCount` count distinct messages (not chunk rows), `Search`
  dedupes to the best chunk per message (`GROUP BY message_id, MIN(distance)`),
  and `LoadVector` returns the `chunk_index = 0` head. New tests:
  `TestBackend_Upsert_MultiChunk_StoresAllChunks`, `..._ReplaceShrinks`,
  `TestBackend_Search_MultiChunk_OneHitPerMessage`.
  (`internal/vector/pgvector/{schema.sql,backend.go,backend_test.go}`)
- **C3 (major) — fixed.** The duplicate `test-postgres` job is renamed to
  `test-pgvector` and now runs `go test -tags "fts5 sqlite_vec pgvector"
  ./internal/vector/pgvector/...` against the pgvector image, so the backend
  has real CI coverage. (`.github/workflows/ci.yml`)
- **C4 (major) — fixed.** The subject filter now uses
  `LOWER(m.subject) LIKE LOWER($N) ESCAPE '\'`, matching SQLite's
  case-insensitive default and the store/query PG search path.
  (`internal/vector/pgvector/backend.go`)
- **C5 (minor) — fixed.** `Stats.StorageBytes` is now populated via
  `pg_total_relation_size(to_regclass('embeddings'))` (table-wide; there is no
  vectors.db file to stat as on SQLite), and the doc comment is corrected.
  (`internal/vector/pgvector/backend.go`)

### Additional issues found and fixed while verifying

- **CI lint was already red** (independent of these findings): three
  golangci-lint failures in PR4-added test files —
  `internal/deletion/executor_e2e_test.go` (`perfsprint`,
  `embeddedstructfieldcheck`) and `internal/store/attachment_e2e_test.go`
  (`unparam`). Fixed; `golangci-lint run ./...` is now clean.
- **Meta (not fixed): CI lint does not pass build tags.** `make lint-ci` runs
  `golangci-lint run ./...` with no `--build-tags`, so all `//go:build sqlite_vec`
  / `pgvector` files (e.g. `search_vector.go`, the pgvector package) are never
  linted. ~18 latent lint issues live behind those tags. Tracked for a future
  pass; not CI-blocking today. The new `test-pgvector` lane runs tests, not
  lint, so it does not change this.
