# claude_ci_nix_vendorhash.md — CI nix-build vendorHash fix on PR #328

## Context

PR #328 (`webgress:pr3-upstream` → `kenn-io:main`) — "PostgreSQL dialect
refactor - PR3 of 4 - Functional store layer".

CI run https://github.com/kenn-io/msgvault/actions/runs/26341744277 failed
on the `nix-build` job at commit `e1c94af` with:

```
error: hash mismatch in fixed-output derivation
'/nix/store/.../msgvault-0.14.1-go-modules.drv':
         specified: sha256-/C+svBQ4b9+l8nY8BZ5Lvd072XLKpRDIR2fvqVqLJUE=
            got:    sha256-o+MtdsSXomxymaPY/ZwsBN5PnddGpvKAvwK6ElSUHSQ=
```

Root cause: commit `62a6f7e` ("deps: bump golang.org/x/net to v0.55.0 (CVE
fix)") changed `go.sum` / vendored modules, but the corresponding Nix
fixed-output hash in `nix/package.nix:16` was not bumped. Nix re-hashes
the materialised vendor tree and rejects the build because it doesn't
match the declared hash.

The declared hash lives in `nix/package.nix:16` (the `vendorHash` attr on
the `buildGoModule`-style derivation). It is **not** the unrelated
`hash` on line 30 of `flake.nix`, which is for a different store path.

## Findings

- [x] **F1**: `nix/package.nix:16` — bump `vendorHash` from
      `sha256-/C+svBQ4b9+l8nY8BZ5Lvd072XLKpRDIR2fvqVqLJUE=` to
      `sha256-o+MtdsSXomxymaPY/ZwsBN5PnddGpvKAvwK6ElSUHSQ=` (the value Nix
      computed locally from the current `go.sum`). Verify it matches what
      `nix build` would derive at HEAD before declaring done.

## Workflow rules

- Coder: implement F1, commit + push to `pr3-upstream`, append a Coder log
  entry, toggle the checkbox. One logical fix per commit.
- Reviewer: independently re-derive the expected hash (e.g.
  `nix build 2>&1 | grep -E 'got:'` after temporarily blanking the hash,
  or `nix-prefetch` the module set, or compare against
  `gomod2nix.toml`/`go.sum` if present) and confirm the new hash matches
  what the upstream build would produce. PASS only with concrete evidence
  (the computed hash quoted back). Then watch CI on the next push and log
  PASS when the `nix-build` job is green.
- If F1 turns out to have a second source of truth (a generated lockfile,
  a duplicated hash elsewhere), the coder must fix both and the reviewer
  must verify both.

## Coder log

(coder appends one line per commit: `<hash> — F<n> — <one-line summary>`)

- e85d8a6 — F1 — bumped nix/package.nix vendorHash to the CI-reported value (nix unavailable on sandbox; CI to confirm)

## Reviewer log

(reviewer appends PASS/FAIL with concrete evidence per coder commit, and
a final all-CI-green PASS when the upstream run flips green)

- e85d8a6 — F1 — **PASS** — coder bumped `nix/package.nix:16` to
  `sha256-o+MtdsSXomxymaPY/ZwsBN5PnddGpvKAvwK6ElSUHSQ=`. Independently
  verified two ways: (a) baseline reproduction on HEAD `8b5c9e5` in CI
  run https://github.com/kenn-io/msgvault/actions/runs/26342426550
  failed identically with `specified: sha256-/C+sv…LJUE= got:
  sha256-o+Mtd…HSQ=` (same as the original
  https://github.com/kenn-io/msgvault/actions/runs/26341744277), and
  (b) Nix re-derived the same `got` hash both times — that *is* the
  authoritative value for the current `go.sum`. The committed hash
  matches byte-for-byte. CI run on `e85d8a6`
  https://github.com/kenn-io/msgvault/actions/runs/26342471505 is
  green on all jobs incl. `nix-build`. SQLite `make test` sweep green
  locally at HEAD `02f13ca` (no test inputs changed; PG suite per
  `docs/PG_STATUS.md` baseline — not exercised by this fix). Doc
  commit `02f13ca` CI run
  https://github.com/kenn-io/msgvault/actions/runs/26342477025 also
  fully green incl. `nix-build`. F1 closed.
