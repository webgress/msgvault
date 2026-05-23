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
- [ ] H2 — AddAccountIdentity loses concurrent signal updates on PG
- [ ] H3 — query.Engine PG search is case-sensitive (subject + metadata LIKE)
- [ ] H4 — PG absent from CI; status docs disagree with Makefile

Significant:
- [ ] M1 — EnsureConversation / GetOrCreateSource / StartSync read-then-insert races
- [ ] M2 — FTSNeedsBackfill cannot detect stale/null vectors; PG FTSRebuildSchema unimplemented
- [ ] M3 — Dialect boundary porous; store and query dialects duplicate logic
- [ ] M4 — Comments assert invariants that aren't enforced; PG_STATUS drift

## Coder log

<!-- coder appends entries here, newest at bottom: "HASH — finding — summary" -->

- f534155 — H1 — partial unique index on attachments(message_id, content_hash); rewrite UpsertAttachment as INSERT ... ON CONFLICT; pre-schema dedupe

## Reviewer log

<!-- reviewer appends entries here, newest at bottom: "HASH — finding — PASS/FAIL — evidence" -->

- baseline @ 05c0f93 — H1 SQLite reproduces: `TestUpsertAttachment_Concurrent: got 2 attachment rows, want exactly 1` (`go test -tags fts5 -count=10 ./internal/store -run TestUpsertAttachment_Concurrent`)
- baseline @ 05c0f93 — H2 PG reproduces (local PG on sandbox, 127.0.0.1, db `msgvault_test`): `merged source_signal "header,manual" missing "account-identifier"`. CT 100 (192.168.37.100) rejects connections from sandbox via pg_hba; using local PG 16 (msgvault_test/msgvault_test) for verification.
- baseline @ 05c0f93 — H1 on local PG passed 10/10 (not deterministic on PG, but the missing UNIQUE constraint is visible in schema_pg.sql; verification will rely on schema + SQLite repro).
