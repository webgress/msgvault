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
   PostgreSQL (`MSGVAULT_TEST_DB=postgres://msgvault_test:msgvault_test@192.168.37.100:5432/msgvault_test?sslmode=disable`)
   and SQLite, then appends a PASS/FAIL entry under "Reviewer log" referencing
   the same commit hash.
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
- [ ] M3 — Dialect boundary porous; store and query dialects duplicate logic
- [ ] M4 — Comments assert invariants that aren't enforced; PG_STATUS drift

## Coder log

<!-- coder appends entries here, newest at bottom: "HASH — finding — summary" -->

- f534155 — H1 — partial unique index on attachments(message_id, content_hash); rewrite UpsertAttachment as INSERT ... ON CONFLICT; pre-schema dedupe
- d074f62 — H2 — serialize AddAccountIdentity via SQLite BEGIN IMMEDIATE + PG SELECT FOR UPDATE; add Dialect.BeginWriteSQL / SelectForUpdate; retry on conflict/busy
- 5bb1d56 — H3 — wrap subject+metadata LIKE in LOWER()/LOWER() in query.Engine; escape user input; add TestQueryEngine_CaseInsensitiveSearch_Subject; move unique-attachments index out of schema.sql into InitSchema after dedupe
- 199efae — H4 — add CI `test-postgres` job (postgres:16 service, MSGVAULT_TEST_DB, -count=5 concurrency); drop Makefile scaffold-only warning; reconcile PG_STATUS
- c41aeae — M1 — collapse EnsureConversation/EnsureConversationWithType/GetOrCreateSource to INSERT ... ON CONFLICT DO UPDATE RETURNING; serialize StartSync in writer-locked tx (BEGIN IMMEDIATE / SELECT FOR UPDATE on sources row) with busy retry
- d534c39 — M2 — PG FTSNeedsBackfill now COUNT(NULL search_fts) so missing intermediate rows surface; FTSRebuildSchema implemented as DROP index / clear column / re-CREATE index

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
