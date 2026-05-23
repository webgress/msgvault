# Merge upstream/main → pr3-upstream — Resolution Log

Shared coordination log between the coder (`pr3fix-coder`) and reviewer
(`pr3fix-reviewer`) tmux sessions resolving the upstream merge.

## Scope

Merging `upstream/main` (HEAD `a3e6038`) into `pr3-upstream` (HEAD `4213a6b`).
Three upstream commits ahead of our merge base (`4c70066`):

| Commit | Title |
|--------|-------|
| `d4d413c` | nix: split flake into package.nix + lean dev shell (#335) |
| `eabce62` | Switch msgvault module to go.kenn.io (#336) — **213-file rename `github.com/wesm/msgvault` → `go.kenn.io/msgvault`** |
| `a3e6038` | chore(deps): bump docker/build-push-action from 7.1.0 to 7.2.0 (#333) |

The big one is `eabce62`: it touches every Go file that imports the module,
which heavily overlaps with the 17 commits on `pr3-upstream`. Expect
import-line conflicts in most touched files, plus semantic conflicts where
PG-dialect code lives in files the rename also touched.

## Workflow

1. **Coder**:
   - Checkout `pr3-upstream`, run `git merge upstream/main`.
   - Resolve each conflict **semantically** (not just "ours" / "theirs"):
     - Import lines: take the new path `go.kenn.io/msgvault/...`.
     - PG-dialect code: keep our pr3 logic.
     - go.mod / go.sum: keep the module rename, re-resolve our deps if needed.
     - `.github/workflows/ci.yml`: keep our new `test-postgres` job AND the action bump.
   - After resolving: `go build ./...`, `go fmt ./...`, `go vet ./...`,
     `go test -tags fts5 -count=1 ./...` on SQLite, then with
     `MSGVAULT_TEST_DB=postgres://msgvault_test:msgvault_test@127.0.0.1:5432/msgvault_test?sslmode=disable`
     on PG.
   - Commit the merge with a message naming the upstream HEAD and a one-line
     conflict summary.
   - Append to "Coder log" below.

2. **Reviewer** (read-only on code; only writes to this file):
   - Pull, inspect the merge commit, list every conflicted file and verify
     resolution preserves both sides (use `git log --merges -1 -p` carefully;
     rely on `git show <merge>^1..<merge>` + diff against each parent).
   - Re-run the H1/H2/M1 concurrency reproductions on PG with
     `-count=10` to confirm no fix was reverted.
   - Re-run the H3 case-insensitive subject test through `query.Engine`.
   - Re-check the BeginExclusive lock list still includes the four tables
     added in `4213a6b`.
   - Run full SQLite + PG sweeps; report any new failures.
   - Append to "Reviewer log" below with PASS/FAIL + evidence.

## State

- [ ] Merge attempted
- [ ] Conflicts resolved (semantic, not blind)
- [ ] SQLite full sweep green post-merge
- [ ] PG full sweep green post-merge
- [ ] All 8 codex findings (H1–H4, M1–M4) verified intact
- [ ] BeginExclusive lock list (4213a6b) still complete
- [ ] Module rename consistent — no remaining `github.com/wesm/msgvault` references
- [ ] CI workflow keeps both H4 PG job AND docker action bump

## Coder log

<!-- newest at bottom: "HASH — summary" -->

## Reviewer log

<!-- newest at bottom: "HASH — PASS/FAIL — evidence" -->
