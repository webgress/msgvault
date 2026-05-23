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
- [ ] H1 — UpsertAttachment is not an upsert; concurrent duplicates
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

## Reviewer log

<!-- reviewer appends entries here, newest at bottom: "HASH — finding — PASS/FAIL — evidence" -->
