# PR3 Codex Review — Fixes Log

This file is the shared coordination log between two tmux sessions working on
the findings in `codex_multilevel_review.md`:

- **pr3fix-coder** — implements fixes
- **pr3fix-reviewer** — verifies each fix on real PostgreSQL + SQLite

Both append below. Keep entries short and concrete; prefer commit hashes and
test output lines over prose.

## Workflow

1. Coder picks the next unchecked finding, implements the fix, runs the
   relevant tests, commits, and appends an entry under "Coder log" with the
   commit hash and a one-line summary.
2. Reviewer pulls latest, re-runs the failing reproduction from the review on
   PostgreSQL (set `MSGVAULT_TEST_DB` in your shell/`.envrc`; placeholder form:
   `postgres://USER:PASS@HOST:5432/DBNAME?sslmode=disable` — **never paste the
   real DSN into committed files**) and SQLite, then appends a PASS/FAIL entry
   under "Reviewer log" referencing the same commit hash.
3. Either session toggles the checkbox in "Findings" when verified.

## Findings (from codex_multilevel_review.md)

Blocking:
- [x] H1 — UpsertAttachment is not an upsert; concurrent duplicates
- [x] H2 — AddAccountIdentity loses concurrent signal updates on PG
- [x] H3 — query.Engine PG search is case-sensitive (subject + metadata LIKE)
- [x] H4 — PG absent from CI; status docs disagree with Makefile

Significant:
- [x] M1 — EnsureConversation / GetOrCreateSource / StartSync read-then-insert races
- [x] M2 — FTSNeedsBackfill cannot detect stale/null vectors; PG FTSRebuildSchema unimplemented
- [x] M3 — Dialect boundary porous; store and query dialects duplicate logic
- [x] M4 — Comments assert invariants that aren't enforced; PG_STATUS drift

## Coder log

<!-- coder appends entries here, newest at bottom: "HASH — finding — summary" -->

- f534155 — H1 — partial unique index on attachments(message_id, content_hash); rewrite UpsertAttachment as INSERT ... ON CONFLICT; pre-schema dedupe
- d074f62 — H2 — serialize AddAccountIdentity via SQLite BEGIN IMMEDIATE + PG SELECT FOR UPDATE; add Dialect.BeginWriteSQL / SelectForUpdate; retry on conflict/busy
- 5bb1d56 — H3 — wrap subject+metadata LIKE in LOWER()/LOWER() in query.Engine; escape user input; add TestQueryEngine_CaseInsensitiveSearch_Subject; move unique-attachments index out of schema.sql into InitSchema after dedupe
- 199efae — H4 — add CI `test-postgres` job (postgres:16 service, MSGVAULT_TEST_DB, -count=5 concurrency); drop Makefile scaffold-only warning; reconcile PG_STATUS
- c41aeae — M1 — collapse EnsureConversation/EnsureConversationWithType/GetOrCreateSource to INSERT ... ON CONFLICT DO UPDATE RETURNING; serialize StartSync in writer-locked tx (BEGIN IMMEDIATE / SELECT FOR UPDATE on sources row) with busy retry
- d534c39 — M2 — PG FTSNeedsBackfill now COUNT(NULL search_fts) so missing intermediate rows surface; FTSRebuildSchema implemented as DROP index / clear column / re-CREATE index
- bc03783 — M3 — extract `?`-rebind and tsquery-escape into internal/sqldialect; both store and query PG dialects now delegate; shared package has its own tests
- 784c564 — M4 — reconcile PG_STATUS.md (add Codex review fixes section; drop CI-coverage from "remaining"); inaccurate H1/H2/M2 comments were rewritten in those fix commits

## Reviewer log

<!-- reviewer appends entries here, newest at bottom: "HASH — finding — PASS/FAIL — evidence" -->

- baseline @ 05c0f93 — H1 SQLite reproduces: `TestUpsertAttachment_Concurrent: got 2 attachment rows, want exactly 1` (`go test -tags fts5 -count=10 ./internal/store -run TestUpsertAttachment_Concurrent`)
- baseline @ 05c0f93 — H2 PG reproduces (local PG on sandbox, 127.0.0.1, db `msgvault_test`): `merged source_signal "header,manual" missing "account-identifier"`. CT 100 (192.168.37.100) rejects connections from sandbox via pg_hba; using local PG 16 (msgvault_test/msgvault_test) for verification.
- baseline @ 05c0f93 — H1 on local PG passed 10/10 (not deterministic on PG, but the missing UNIQUE constraint is visible in schema_pg.sql; verification will rely on schema + SQLite repro).
- f534155 — H1 — PASS — `TestUpsertAttachment_Concurrent -count=10` ok on SQLite (0.41s) AND local PG (5.09s); `idx_attachments_msg_content_hash` partial unique index in both schema files; `UpsertAttachment` now `INSERT ... ON CONFLICT (message_id, content_hash) WHERE ... DO NOTHING` (messages.go:1669); pre-schema `dedupeAttachmentsBeforeUniqueIndex` retains lowest-id row.
- f534155 — regression sweep (`go test -tags fts5 ./...` on SQLite) — all packages PASS except one flake: `TestAddAccountIdentity_Concurrent` failed once with "database is locked" (SQLite lock contention, not the H2 merge issue). Repeat `-count=10` runs pass cleanly, so noting as pre-existing test flake; H2 PG repro still fires at f534155 (`missing "manual"` / `missing "header"`) — coder still owns H2.
- f534155 — PG regression sweep (`MSGVAULT_TEST_DB=... go test -tags fts5 ./...`) — all non-store packages green (cmd 36s, dedup 16s, deletion 32s, fbmessenger 36s actually ran against PG and passed); store/query/testutil need a fresh PG run after H2 commit (Go cache returned SQLite results since env vars don't invalidate the cache).
- d074f62 — H2 — PASS — `TestAddAccountIdentity_Concurrent -count=10` ok on SQLite (0.43s) AND local PG (6.97s, no `missing "..."`). `account_identities.go:60-141` retains comma-rejection (`signal names cannot contain commas`) and empty-addr no-op; merge runs inside a writer-locked tx (SQLite `BEGIN IMMEDIATE` via `BeginWriteSQL`; PG `BEGIN` + `SELECT ... FOR UPDATE` via `SelectForUpdate`) with 5-attempt retry on `IsConflictError`/`IsBusyError`. PG cache-bypass sweep clean: store 90.6s, query 7.2s, testutil 1.0s, storetest 4.1s.
- 5bb1d56 — H3 — PASS — new `TestQueryEngine_CaseInsensitiveSearch_Subject` covers `invoice`/`INVOICE`/`Invoice` against stored `Quarterly Invoice`, ok on SQLite (0.09s) and PG (0.51s). `query/sqlite.go` now uses `LOWER(m.subject) LIKE LOWER(?) ESCAPE '\'` for `subject:` terms (line 1340) AND the non-FTS metadata fallback wraps both `LOWER(m.subject)` and `LOWER(m.snippet)` (line 1388), with `escapeSQLiteLike` keeping `%`/`_` literal. Full `./internal/query/` + `./internal/api/` on PG green (6.75s + 0.15s). H1/H2 not regressed: `-count=10 TestUpsertAttachment_Concurrent|TestAddAccountIdentity_Concurrent` ok on SQLite (0.89s) + PG (11.86s) — note H3 also moved the H1 partial unique index from schema.sql into Go-level `InitSchema` after dedupe; tests still pass.
- 199efae — H4 — PASS — `.github/workflows/ci.yml` `test-postgres` job spins up `postgres:16` service with `pg_isready` health check, exports `MSGVAULT_TEST_DB`, runs `go test -tags fts5 ./internal/store ./internal/query ./internal/testutil` then `-count=5 -run Concurrent ./internal/store`; YAML parses clean. `Makefile` `test-pg` target no longer prints `scaffold-only / expected to fail`. `docs/PG_STATUS.md` drops stale `UpdateOrIgnore()` mention and rewrites the CI-coverage gap line to point at the new job. Locally simulated the CI concurrency step with PG: `-count=5 -run Concurrent ./internal/store` ok (11.84s).
- c41aeae — M1 — PASS — `EnsureConversation`/`EnsureConversationWithType` (messages.go:161, :1131) and `GetOrCreateSource` (sync.go:359) now use `INSERT ... ON CONFLICT (...) DO UPDATE SET <noop> RETURNING ...` so concurrent first-inserts converge without surfacing 23505 to callers. `StartSync` (sync.go:160-222) wraps the supersede `UPDATE` + insert `RETURNING id` inside a writer-locked tx (`BeginWriteSQL` + `SELECT ... FOR UPDATE` on the sources row), with `IsBusyError` retry. Race+PG sweep of M1-touching packages clean: SQLite store 14.1s + sync 9.8s + deletion 5.5s + textimport 1.1s + fbmessenger 6.2s; PG store 228.9s + sync 8.1s + deletion 34.8s + textimport 1.1s + fbmessenger 42.0s.
- d534c39 — M2 — PASS — `dialect_pg.go:191` `FTSNeedsBackfill` switched from `MAX(id)` vs `MAX(id WHERE NOT NULL)` to `COUNT(*) WHERE search_fts IS NULL > 0` (catches arbitrary NULL gaps, not just trailing ones). `dialect_pg.go:217` `FTSRebuildSchema` replaced "not yet implemented" stub with `DROP INDEX IF EXISTS messages_search_fts_idx; UPDATE messages SET search_fts = NULL; CREATE INDEX IF NOT EXISTS ... USING GIN(search_fts)`. Validated end-to-end on a temp PG schema: seeded one NULL + two populated tsvector rows, new logic returned `NeedsBackfill=true` where old logic would have missed it; rebuild operations all succeeded (3 rows cleared, index recreated). Caveat: `TestStore_RebuildFTS_HappyPath`/`_BypassesAvailabilityFlag` still carry `SkipIfPostgres("...PG RebuildFTS is not yet implemented (PR4 scope)")` — the skip messages are now stale but unblocking those tests is out of M2's scope.
- bc03783 — M3 — PASS — new `internal/sqldialect/` package holds shared `RebindPostgreSQL` (quote-aware `?` → `$N`) and `EscapeTSQueryTerm` (`& | ! ( ) : * \ '` + whitespace stripper) with its own `sqldialect_test.go`; `store.PostgreSQLDialect.Rebind`/`BuildFTSArg` and `query.PostgreSQLQueryDialect.Rebind`/`BuildFTSTerm` now delegate so divergence is impossible. Package doc states the inclusion criterion ("things here only when divergence between the two packages would silently produce different results"). Tests green on both backends: sqldialect 0.004s/0.003s, store 9.2s/110.1s, query 6.2s/7.8s. Store-only DDL/lifecycle surface unchanged.
- 784c564 — M4 — PASS — `docs/PG_STATUS.md` adds a "Codex Review Fixes (Late PR3)" section summarising H1–M3, and removes the stale "CI coverage" entry from "Remaining for PR4". Spot-checked code comments: `UpsertAttachment` (messages.go:1626-1638) now ties idempotency to the partial unique index instead of asserting an unbacked ON CONFLICT guarantee; `AddAccountIdentity` (account_identities.go:52-59) describes the `BEGIN IMMEDIATE` / `SELECT FOR UPDATE` writer-lock rather than implying generic concurrency safety; PG `FTSRebuildSchema` doc (dialect_pg.go:217) describes the actual DROP/clear/CREATE cycle. `grep -n 'scaffold|not yet implemented|deferred to PR|UpdateOrIgnore'` on docs+code returns only the unrelated PR2-scaffolding and PR4-deferred-fused-search lines.
- a67f8f3 (HEAD) — final PG cache-bypass sweep (`MSGVAULT_TEST_DB=... go test -tags fts5 -count=1 ./...`) — all 43 packages PASS against live PG 16, no flakes, no failures. Notable timings: cmd 43s, store 94s, query 9s, dedup 21s, deletion 37s, fbmessenger 41s, sync 5.6s, sqldialect 0.003s. Total wall ≈5 min. All 8 review findings (H1–H4, M1–M4) verified end-to-end.
