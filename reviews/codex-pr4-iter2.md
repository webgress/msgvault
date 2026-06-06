# PR4 Iteration 2 Review

Scope: adversarial review of `a34c50d..pr4-upstream`, with emphasis on verifying the iteration 1 fixes and finding any remaining correctness, portability, concurrency, or test coverage issues.

## Result

Most iteration 1 defects were addressed in code. I found one remaining test coverage gap around the PostgreSQL subject filter regression.

## Finding

### D1 - Minor - `internal/vector/pgvector/backend_test.go:321`

The pgvector tests still do not exercise `vector.Filter.SubjectSubstrings`, so the iteration 1 C4 fix is not protected by CI.

`internal/vector/pgvector/backend.go:717` now correctly uses `LOWER(m.subject) LIKE LOWER($n) ESCAPE '\'`, which fixes PostgreSQL's case-sensitive `LIKE` behavior for subject substring filters. However, `TestBackend_Search_RespectsFilter` only verifies the `SourceIDs` path, and the new chunk/storage tests do not cover subject filters. A future regression back to plain `LIKE` would pass the current pgvector test suite.

Suggested fix: add a pgvector regression test that stores a subject such as `Quarterly Invoice`, searches with `vector.Filter{SubjectSubstrings: []string{"invoice"}}`, and asserts that the message is returned. Include an escaped wildcard case if practical.

## Iteration 1 Fix Verification

- C1, PostgreSQL `embed` path: `cmd/msgvault/cmd/embed_vector.go:40` now rejects PostgreSQL stores before reaching the raw pgvector embedding path.
- C2, pgvector chunk handling: `internal/vector/pgvector/schema.sql` now keys embeddings by `(generation_id, message_id, chunk_index)`, and `backend.go` now deletes/reinserts all chunks per message while tracking distinct message counts. Tests cover multi-chunk insert, replacement shrink, and one search hit per message.
- C3, CI pgvector job: `.github/workflows/ci.yml:79` defines a distinct `test-pgvector` job using `pgvector/pgvector:pg16` and `-tags "fts5 sqlite_vec pgvector"`.
- C4, subject filter portability: the implementation is fixed in `internal/vector/pgvector/backend.go:717`, but see D1 for the missing regression test.
- C5, pgvector storage stats: `Stats` now uses relation size for `StorageBytes`, and `TestBackend_Upsert_MultipleChunks_StoresAllChunks` asserts non-zero storage bytes.

## Verification Performed

- `git diff --check a34c50d..pr4-upstream` passed.
- `go test -tags "fts5 sqlite_vec" ./cmd/msgvault/cmd ./internal/store ./internal/deletion` passed.
- `go test -tags "fts5 sqlite_vec pgvector" ./cmd/msgvault/cmd` passed.
- `go test -v -tags "fts5 sqlite_vec pgvector" ./internal/vector/pgvector` passed locally, but all pgvector tests skipped because `MSGVAULT_TEST_DB` was unset. I reviewed the CI job to confirm it now provisions a pgvector PostgreSQL service for those tests.

## Resolutions (Claude, iteration 2)

- **D1 (minor) — fixed.** Added `TestBackend_Search_SubjectFilter_CaseInsensitive`
  to `internal/vector/pgvector/backend_test.go`: it stores a mixed-case subject
  (`Quarterly Invoice`) and asserts a lowercase filter term (`invoice`) matches
  it — a regression to plain case-sensitive `LIKE` would return zero hits and
  fail the test. It also stores `50% discount code` and asserts the term `50%`
  matches only that row, exercising the wildcard escaping. Verified against a
  live PostgreSQL+pgvector instance: the full pgvector suite (22 tests) passes.

All iteration-1 fixes were independently re-verified as correct by this round.
No other substantive issues were found in the fresh adversarial pass.
