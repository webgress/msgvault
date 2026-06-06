# PR4 Iteration 4 Review

Scope: convergence confirmation over `a34c50d..pr4-upstream` after the lint-fix batch `b21fb0e` and tag-aware lint config change `a2d6075`.

## Findings

No substantive findings.

## Lint Verification

Ran the requested command exactly:

```sh
cd /home/admin/git/msgvault.worktrees/pr4-rebase-sync-fork-rebase && /home/admin/go/bin/golangci-lint run --build-tags "fts5 sqlite_vec pgvector" ./...
```

Result: clean, `0 issues.`

`.golangci.yml` now sets `run.build-tags` to `fts5`, `sqlite_vec`, and `pgvector`, so the normal lint config also compiles the tagged vector files instead of silently skipping them.

## Lint-Fix Regression Checks

- `internal/vector/sqlitevec/backend.go:634`: the G115 fix preserves little-endian float32 packing. The new writes are `byte(bits & 0xff)`, `byte((bits >> 8) & 0xff)`, `byte((bits >> 16) & 0xff)`, `byte(bits >> 24)`, which are byte-for-byte equivalent to the previous truncating casts but make truncation explicit.
- `internal/vector/sqlitevec/migrate.go:280` and `:311`: the `defer rows.Close()` conversions are scoped inside `readLegacy` and `readMapping` closures. Both result sets are fully consumed and closed before the rebuild transaction and `DROP TABLE`, preserving the intended vec0 iterator/DDL ordering and avoiding double close.
- `internal/vector/sqlitevec/fused.go:335`: the fused-search row close is scoped inside the per-iteration closure, so each widening query closes before the next iteration. This preserves result-set consumption order and avoids accumulating open rows until function return.
- `cmd/msgvault/cmd/search_vector.go:164` and `:259`: `outputHybridResultsTable` now returns no error, and its only caller was updated to call it then return `nil`. JSON output still returns `printJSON` errors.
- `internal/vector/embed/worker_test.go` and `internal/vector/sqlitevec/migrate_test.go`: the testify `require`/`assert` swaps preserve or strengthen control flow. The one per-connection loop that intentionally continues after assertion failure keeps an explicit lint suppression.
- `internal/vector/pgvector/backend.go:703`: `fmt.Sprintf` to string-concat rewrites produce the same SQL fragments, e.g. `m.has_attachments = $N`, `m.sent_at >= $N`, `m.sent_at < $N`, `m.size_estimate > $N`, and `m.size_estimate < $N`.

## Fresh Correctness Sweep

No new issue found in the final sweep.

- SQL portability: clean for the reviewed changes. SQLite-only vector CLI paths still reject PostgreSQL before worker SQL, and pgvector filter SQL uses PostgreSQL bind placeholders with bound values.
- Transactions/concurrency: clean for the reviewed changes. pgvector upsert/delete paths remain transactional; sqlitevec migration result sets now close before DDL-sensitive operations.
- Data integrity/cascades: clean for the reviewed changes. Prior chunk-key, distinct-message count, storage-size, and cascade-related fixes remain intact.
- SQL injection: clean for the reviewed changes. Dynamic values are bound; string concatenation is limited to fixed fragments or validated/internal table names.
- Resource leaks/swallowed errors: clean under tag-aware lint. The row iteration/close issues from iteration 3 are fixed.
- Tests that do not assert what they claim: clean for the reviewed changes. The pgvector subject-filter regression test now covers both case-insensitive matching and literal `%` escaping.

## Prior Fix Regression Check

- C1: still fixed. `runEmbed` refuses PostgreSQL up front.
- C2: still fixed. pgvector stores one row per chunk and counts distinct messages.
- C3: still fixed. CI has a distinct pgvector test lane with the `pgvector` build tag.
- C4/D1: still fixed. PG subject substring filters are case-insensitive and covered by a regression test.
- C5: still fixed. pgvector `Stats.StorageBytes` is populated and tested.
- E1-E52: fixed. The tag-aware linter is clean.

## Verification

- `golangci-lint run --build-tags "fts5 sqlite_vec pgvector" ./...`: passed with `0 issues.`
- `go test -tags "fts5 sqlite_vec" ./internal/vector/sqlitevec ./internal/vector/embed ./internal/vector/hybrid ./cmd/msgvault/cmd`: passed.
- `go test -tags "fts5 sqlite_vec" ./internal/store ./internal/deletion`: passed.
- `go test -v -tags "fts5 sqlite_vec pgvector" ./internal/vector/pgvector`: passed locally, but all pgvector tests skipped because `MSGVAULT_TEST_DB` was unset.
- `git diff --check a34c50d..pr4-upstream`: passed.
