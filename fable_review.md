# msgvault PostgreSQL Backend — Review (branch `pr4-upstream`)

Reviewer: Claude (Fable 5), read-only code review. Scope: the entire PostgreSQL
backend as it stands on `pr4-upstream` (107 commits ahead of `main`, ~10.5k
insertions), plus the general msgvault architecture it plugs into. Findings are
tagged by severity: **High** (likely to bite in real use), **Medium** (real but
bounded), **Low** (edge case / latent), **Info** (observation, no action
implied). File references are to the branch as checked out.

## Executive summary

**The implementation quality is high.** This is unusually disciplined porting
work: genuinely atomic upserts and state machines, lock-ordering fixes with
regression tests and fault-injection seams, dual-backend test suites running
against live PostgreSQL in CI, and a status document whose claims I verified
against the code and found accurate. Correctness of the happy path and of the
concurrency-sensitive paths is in good shape.

The real risks are **operational-at-scale**, not logical:

1. **S1 (High)** — the pool-wide 30s `statement_timeout` will break
   cascade deletes, FTS rebuilds, index builds, and seeding on an archive of
   the stated target size (1M+ messages). No maintenance escape hatch exists.
2. **S3 (Medium)** — unguarded `to_tsvector` can exceed PostgreSQL's 1MB
   tsvector limit; a single pathological message wedges the FTS backfill loop
   (which clears all FTS first, then aborts).
3. **A3/V1/V2 (Medium)** — the PG path drops the Parquet acceleration layer
   and the fused hybrid-search SQL materializes the full live-message ID set
   per query; none of this has been validated with EXPLAIN or a realistic
   corpus (the code itself says so).
4. **M1 (High for adoption)** — there is no SQLite→PG data-migration tool, so
   the existing 20-year archive cannot actually move to this backend yet.

**Gmail access security is unchanged and sound** — the branch does not touch
OAuth, token storage, or the Gmail client; sync stays read-only; logging
redacts credentials and logs SQL arg *shapes*, not values.

Recommended order of work before trusting PG as the primary backend:
timeout escape hatch (S1) → tsvector guard (S3) → EXPLAIN/scale validation of
fused search and TUI aggregates (V1/V2/A3) → migration tool (M1).

---

## 1. Overall architecture

### 1.1 msgvault in general

The codebase has a clear, well-enforced layering:

- `internal/store` — system of record, all writes, schema lifecycle, the rich
  `Dialect` interface (DDL, error classification, FTS, locking primitives).
- `internal/query` — read-side engines behind an `Engine` interface: DuckDB
  over Parquet (SQLite analytics path), a dialect-parameterized SQL engine
  (`SQLiteEngine`, despite the name) for both SQLite and PG live queries.
- `internal/vector` — vector search behind `vector.Backend` /
  `vector.FusingBackend`, with `sqlitevec` and `pgvector` implementations, a
  portable embed pipeline (`embed/`), and RRF fusion (`hybrid/`).
- `cmd/msgvault/cmd` — wiring, behind build tags (`fts5`, `sqlite_vec`,
  `pgvector`) with `_stub.go` fallbacks.

Strengths worth calling out:

- **The two-level dialect design is right.** `store.Dialect` is rich
  (38 methods) because the store owns DDL/lifecycle; `query.Dialect` is
  minimal (9 methods) because the engine only needs SQL-generation deltas. The
  shared `internal/sqldialect` package holds exactly the two primitives that
  must not drift between them (`?`→`$N` rebind, tsquery escaping) — a good
  answer to the classic "two dialect layers diverge silently" failure mode.
- **`loggedDB` / `loggedTx` as the rebind chokepoint** (`internal/store/db_logger.go`)
  is an elegant move: call sites write portable `?` SQL and the wrapper applies
  `Rebind` + structured logging uniformly, including inside transactions.
- **Concurrency-sensitive upserts are genuinely atomic**: `EnsureConversation*`,
  `EnsureParticipant*`, `GetOrCreateSource`, `UpsertAttachment` all use
  `INSERT … ON CONFLICT … RETURNING` with conflict targets that exactly mirror
  the partial unique indexes. `StartSync` and `AddAccountIdentity` use
  writer-locked transactions (`BEGIN IMMEDIATE` / `SELECT … FOR UPDATE`) with
  bounded retry loops. This is markedly better than typical first-pass ports.

General-architecture observations (pre-existing, not PG-specific):

- **A1 (Info).** The read side has three parallel search implementations
  (store `api.go` search, query-engine search, vector/hybrid search) with
  deliberate lockstep comments. The lockstep is currently maintained by
  discipline + tests; `sqldialect` covers only part of the shared surface
  (e.g. the two PG sanitizers in §3.6 already disagree).
- **A2 (Info).** `MarkMessageDeletedByGmailID(Batch)` and
  `GetMessageBySourceID` match on `source_message_id` without a `source_id`
  scope. Cross-account Gmail-ID collision is acknowledged in a comment as
  astronomically unlikely, but the deletion executor writes through this path,
  so the blast radius of a collision is a wrongly soft-deleted row in another
  account. Cheap to fix by threading `source_id`.

### 1.2 The PG backend as an add-on

The PG port swaps an entire constellation: SQLite → PostgreSQL, FTS5 →
tsvector/GIN, sqlite-vec → pgvector, and — importantly — **DuckDB/Parquet →
nothing**. On PG, the TUI and aggregates run live relational SQL through the
dialect-parameterized engine. The design is coherent and the store/query/vector
seams were clearly the right cut points; CI runs the full suite against live
PG 16 plus a dedicated pgvector lane.

- **A3 (Medium, performance-architecture).** The Parquet cache exists because
  "DuckDB over Parquet is ~3000× faster than SQLite JOINs" (CLAUDE.md). The PG
  path abandons that acceleration layer entirely and runs aggregates like
  `buildAggregateSQL` (internal/query/sqlite.go:216) — which LEFT JOINs a
  full `attachments GROUP BY message_id` derived table — directly against the
  live schema. PG's planner is far better than SQLite's, but at the stated
  target (20+ years, 1M+ messages) TUI drill-downs will regress from
  "Parquet-instant" to multi-hundred-ms-to-seconds live aggregation, on every
  keystroke-level navigation. There is no materialized-view or caching story
  on PG yet, and `docs/PG_STATUS.md` does not flag this trade-off.
- **A4 (Info).** `pgEngine` deliberately hides the SQLite-only `TextEngine`
  (FTS5 `MATCH`, `strftime`) so type assertions fail cleanly on PG
  (internal/query/postgres.go:29). Right call — but it means whatever UI
  surface depends on `TextEngine` silently loses features on PG. Worth an
  explicit feature matrix in PG_STATUS (see §9).
- **A5 (Info).** `subset.go` (`CopySubset`) is explicitly not migrated to the
  Dialect layer and always creates a SQLite destination; the source path is
  `filepath.Abs`-canonicalized, which would mangle a postgres:// URL. Callers
  must gate it off PG (verified in cmd wiring — see §6).

---

## 2. Store layer — correctness & data safety

### 2.1 What is done well

- Schema init is atomic on PG: schema files are executed as one multi-statement
  simple-protocol Exec (implicit single transaction), and the GIN index is
  deliberately created *after* legacy column migrations so a legacy DB missing
  `search_fts` can't fail the whole apply (`EnsureFTSIndex`, cr2-10).
- `RemoveSourceSerialized` runs the active-sync check, FTS delete, and source
  cascade **on the same locked connection** — no pool-deadlock, race documented
  and closed (internal/store/sources.go:145).
- Timestamp scanning is properly dual-driver: `scanSource`/`scanSyncRun` use
  `sql.NullTime` with invariant errors; computed `COALESCE` timestamps go
  through `nullableTimestamp` which accepts `time.Time` (pgx), string and
  `[]byte` (SQLite) (internal/store/api.go:640).
- The label rename two-phase dance uses a `\x01`-prefixed temp name bound as a
  parameter, replacing the non-portable SQLite `X'00'` literal — exactly the
  kind of detail ports usually miss (internal/store/messages.go:646).

### 2.2 Findings

- **S1 (High). Pool-wide `statement_timeout=30s` will break large maintenance
  operations on PG.** `postgresConnConfig` (internal/store/store.go:299) sets
  `statement_timeout=30s` on every pooled connection, with no per-operation
  override anywhere. Statements that scale with archive size and will exceed
  30s on a 20-year, 1M+ message archive:
  - `DELETE FROM sources WHERE id = ?` in `RemoveSource(Serialized)` — cascade
    deletes millions of rows across messages/recipients/labels/bodies/raw.
  - `FTSDeleteSQL` / `FTSClearSQL` — full-table `UPDATE messages SET
    search_fts = NULL` rewrites.
  - `EnsureFTSIndex` — `CREATE INDEX … USING GIN` over a populated messages
    table (legacy-upgrade and `FTSRebuildSchema` paths).
  - `dedupeAttachmentsBeforeUniqueIndex` + `CREATE UNIQUE INDEX
    idx_attachments_msg_content_hash` in `InitSchema` — runs on **every**
    schema init against the full attachments table.
  - `DeleteAllDeduped` / `DeleteDedupedBatch` — unbounded cascade DELETEs.
  - `BeginExclusive`'s 18-table `LOCK TABLE … IN EXCLUSIVE MODE` — under a
    busy serve daemon the lock wait itself burns the 30s budget.
  The failure surfaces as SQLSTATE 57014 (`query_canceled`), which
  `IsBusyError` does **not** classify (see S2), so callers get a raw error
  rather than a retry or an actionable message. Recommendation: a
  `SET LOCAL statement_timeout = 0` escape hatch (or a dedicated maintenance
  connection) for DDL/cascade/backfill operations.
- **S2 (Low). `IsBusyError` misdocuments and under-covers PG timeout
  semantics** (internal/store/dialect_pg.go:304). The comment claims 55P03
  covers "statement_timeout-triggered lock waits" — in PostgreSQL,
  `statement_timeout` cancellation raises **57014**; 55P03 (`lock_not_available`)
  only fires for `NOWAIT`/`lock_timeout`, and `lock_timeout` is never set. Net
  effect: the `StartSync` busy-retry loop and the "stop other processes and
  retry" UX never trigger on PG for the most common contention symptom.
- **S3 (Medium). tsvector size limits are unguarded.** `FTSUpsert` and
  `FTSBackfillBatchSQL` feed the full message body into `to_tsvector`
  (internal/store/dialect_pg.go:90,127). PostgreSQL hard-errors on tsvectors
  over 1MB (and silently caps lexeme positions at 16383). FTS5 on SQLite has
  no such limit, so this is a PG-only regression. The blast radius differs by
  path: at **sync time** `UpsertFTS` is warn-only (internal/sync/sync.go:683),
  so the message persists with `search_fts = NULL` — no data loss, but that
  NULL makes `FTSNeedsBackfill` permanently true. The **backfill** path is the
  dangerous one: `BackfillFTS` first runs `FTSClearSQL` (NULLing *every* row's
  tsvector), then refills in 5000-row batches — the batch containing the
  pathological message errors, the backfill aborts, and every message after
  that batch is left unindexed; the next attempt clears-and-fails identically.
  One bad message can therefore take down PG full-text search for the whole
  archive. Mitigation: truncate the body to a sane bound (e.g. `LEFT(body_text,
  n)` in the backfill SQL, equivalent cap in `FTSUpsert`), or catch the
  per-batch error and bisect/skip the offending row.
- **S4 (Low). `BeginExclusive`'s hand-maintained 18-table lock list**
  (internal/store/dialect_pg.go:329) must be updated whenever sync/import
  writes to a new table. The comment documents the invariant, but nothing
  enforces it; a missed table reopens exactly the race the lock exists to
  close. A test that diffs the list against tables touched by sync fixtures
  would pin it.
- **S5 (Low). `WithExclusiveLock` contract is implicit.** The callback runs
  against the *pool*, while the EXCLUSIVE lock is held on a dedicated
  connection (internal/store/store.go:391). That works only because callers
  restrict `fn` to reads + filesystem work (PG EXCLUSIVE permits ACCESS
  SHARE). A future `fn` that writes will self-deadlock until the 30s timeout.
  Worth an explicit "fn must not write through the store" doc line and/or a
  read-only Store handle passed to fn.
- **S6 (Info). `GENERATED ALWAYS AS IDENTITY`** means any future tool that
  inserts explicit ids (data migrator, `CopySubset`-style cloning into PG)
  needs `OVERRIDING SYSTEM VALUE`. Fine today; relevant to the missing
  migration tool (§8).

---

## 3. Query engine (internal/query)

### 3.1 What is done well

- Single engine implementation parameterized by dialect; PG gets `to_char(…
  AT TIME ZONE 'UTC', …)` time bucketing matching SQLite's UTC-text
  `strftime` — UTC parity was explicitly fixed (commit ea69e94) and the
  rationale is documented at the binding sites.
- Every 1:N filter is an `EXISTS`/`NOT EXISTS` correlated subquery — no
  DISTINCT+JOIN anywhere, consistent with the project SQL guidelines, and it
  side-steps PG's `SELECT DISTINCT` + ORDER-BY restriction that would
  otherwise have broken `GetGmailIDsByFilter` ordering.
- Date bounds bind `time.Time` directly (cr2-9) — correct on PG TIMESTAMPTZ
  regardless of session timezone, and still sortable on SQLite.
- `hasFTSTable` is dialect-aware with a SQLite-only liveness probe; the PG
  information_schema probe is schema-scoped (cr2-8) so a sibling schema can't
  fake FTS availability.

### 3.2 Findings

- **Q1 (Low). The two PG FTS sanitizers disagree, and the comments claim they
  don't.** `sqldialect.EscapeTSQueryTerm` is allowlist-based (letters/digits
  only); `PostgreSQLQueryDialect.SanitizeFTSQuery`
  (internal/query/dialect.go:231) is blocklist-based and passes `<`, `>`, `=`,
  `#`, `$` … through into to_tsquery operands. `<` begins tsquery's
  phrase-distance operator, so a user search containing `<` (e.g. `"<3"`,
  pasted HTML) produces a **runtime tsquery syntax error** on PG via whichever
  path calls `SanitizeFTSQuery`. The doc comment on `EscapeTSQueryTerm`
  ("intentionally mirrors … SanitizeFTSQuery") overstates parity. Align both
  on the allowlist tokenizer.
- **Q2 (Info).** Engine deep search orders by `m.sent_at DESC` only (no FTS
  rank, no id tiebreaker) — relevance ordering exists only in the store API
  search path. Consistent across backends, so a parity non-issue, but
  pagination under equal timestamps is unstable on both.
- **Q3 (Info).** `pgEngine` hides `TextEngine` so PG callers fail type
  assertions cleanly rather than receiving FTS5/`strftime` SQL. Good
  mechanism; the resulting PG feature gaps need documenting (§9).

---

## 4. Vector subsystem — pgvector backend & embed pipeline

### 4.1 What is done well

This is the strongest part of the branch. Specifically verified:

- **Generation lifecycle is genuinely atomic.** `ActivateGeneration` demotes
  the previously-active row via `UPDATE … RETURNING` (so the embeddings it
  deletes provably belong to the row it retired), re-checks the
  seeded/no-pending gate *inside* the same transaction as the flip, and reaps
  the demoted generation's queue rows in the same tx
  (internal/vector/pgvector/backend.go:281). `RetireGeneration` enforces the
  refuse-retire-active guard in the `WHERE` of the state flip itself, so a
  concurrent activation cannot get its embeddings deleted without `--force`.
  Gate failures are re-read inside the tx to produce precise, actionable
  errors.
- **Lock ordering is consistent and documented.** `Upsert`, `Delete`,
  activate and retire all take the `index_generations` row lock first
  (`SELECT … FOR UPDATE`), with the ABBA asymmetry that `Delete` previously
  had explicitly fixed and explained in a comment (backend.go:1045).
- **The enqueue/retire orphan race is closed properly.**
  `Enqueuer.EnqueueMessages` re-validates each generation under
  `FOR NO KEY UPDATE` — chosen precisely because the FK insert's
  `FOR KEY SHARE` does *not* conflict with retire's state-flip lock — and the
  reasoning for both interleavings is written out at the call site
  (internal/vector/embed/enqueue.go:154). A test-only hook
  (`afterGenSnapshotHook`) exists specifically to commit a retire inside the
  race window.
- **Queue claims are correct under concurrency**: `FOR UPDATE SKIP LOCKED` on
  PG, token-scoped Complete/Release, chunked IN-clauses inside a single
  transaction (restoring cross-chunk atomicity), stale-claim reclamation with
  a threshold derived from the embed client's full retry budget.
- **The worker's failure taxonomy is unusually complete**: permanent-4xx
  downshift drain with deferred drop decisions (so a misconfigured endpoint
  cannot silently destroy work), retired-generation drops treated as benign
  with token-aware cleanup, orphan-drain failures surfaced on the empty-claim
  exit instead of reporting a false-clean run, `embed_runs` finalized on a
  cancellation-detached context.
- **Config fingerprinting** folds model, dimension, preprocess policy,
  chunk cap, and embed-layout version into the generation fingerprint, so any
  policy change forces a rebuild instead of mixing incompatible vectors.
- Injection safety: vectors and ID arrays are formatted from typed
  ints/floats and bound via `$N::vector` / `$N::bigint[]` casts — no string
  data ever reaches SQL unparameterized.

### 4.2 Findings

- **V1 (Medium, performance). FusedSearch always materializes the full
  filtered universe.** The fused CTE starts with `filtered AS (SELECT m.id
  FROM messages m WHERE <live>)` even when the structured filter is empty
  (internal/vector/pgvector/fused.go:105). Because `filtered` is referenced
  by *both* signal CTEs, PostgreSQL materializes it — i.e. every hybrid
  search on a 1M-message archive first builds a 1M-row CTE. The non-fused
  `Search` has an empty-filter fast path (EXISTS instead of JOIN,
  backend.go:789); the fused path lacks the equivalent. Compounding it, the
  inner ANN subquery JOINs the materialized CTE, and the code's own comment
  (fused.go:144) concedes the HNSW-eligibility of that plan is "not yet
  verified with EXPLAIN ANALYZE". This is the headline query of the PG vector
  feature and its plan shape is unvalidated at scale.
- **V2 (Medium, performance). Filtered ANN materializes all matching IDs
  into a parameter.** `filteredMessageIDs` pulls every matching message ID
  into Go and ships it back as one `bigint[]` literal (backend.go:1000). A
  broad filter (`after:2010`, a big label) means hundreds of thousands of IDs
  serialized per query, plus a sequential ANN scan within the set. The shape
  mirrors sqlitevec (where there's no alternative), but on PG the filter
  could stay in SQL.
- **V3 (Low). Redundant index**: `idx_embeddings_gen_msg` duplicates the
  primary key's `(generation_id, message_id)` prefix
  (internal/vector/pgvector/schema.sql:65) — pure write amplification on the
  largest, hottest table.
- **V4 (Low, ops). Retire-deletes-embeddings churn and bloat.** The design
  (delete a retired generation's rows to keep the shared per-dimension HNSW
  graph clean) is correct and well-documented, but each rebuild cycle now
  deletes ~corpus-size rows and re-inserts them: heap + HNSW bloat until
  autovacuum catches up, and dead tuples linger in the HNSW graph degrading
  scans until VACUUM. Worth a documented autovacuum/maintenance expectation.
- **V5 (Low, ops). `CREATE EXTENSION IF NOT EXISTS vector` runs on every
  vector-enabled startup** (migrate.go:24) and requires elevated privileges.
  On a locked-down or managed PG, Migrate fails and takes serve startup with
  it; `SkipMigrate` is only wired to the read-only path. A
  "schema-already-managed" escape hatch would help non-superuser deployments.
- **V6 (Info, parity). Three FTS query grammars coexist.** Non-fused PG
  search uses `to_tsquery` with `:*` prefix matching; fused PG search uses
  `websearch_to_tsquery` (web syntax, **no** prefix matching); SQLite fused
  uses raw FTS5 MATCH. The same query string can match different documents
  depending on mode and backend. Partially documented on `FusedRequest`;
  deserves a user-facing note.
- **V7 (Info).** `seedPending` is a single `INSERT … SELECT` over the whole
  corpus (backend.go:253) — at 1M+ messages this joins the S1
  statement-timeout family.

---

## 5. Test coverage

### 5.1 Strengths

- **Real dual-backend execution, not mocks.** `testutil.NewTestStore` routes
  every store-level test to live PostgreSQL when `MSGVAULT_TEST_DB` is set,
  with per-test schema isolation via a `search_path`-scoped DSN and
  guaranteed schema cleanup (internal/testutil/store_helpers.go). CI runs the
  *entire* suite this way (`test-postgres` lane, postgres:16), plus
  `-count=5` reruns of concurrency tests so nondeterministic races surface.
- **A dedicated pgvector lane** (pgvector/pgvector:pg16) runs every
  `pgvector`-tagged test — backend, queue/worker/enqueuer, hybrid filter,
  scheduler embed job, and the serve/search CLI wiring. The CI comment
  correctly notes these tests would otherwise compile out and have zero
  coverage.
- **Deliberate race tests with fault injection**: the queue/enqueuer expose
  test-only seams (`afterChunkHook`, `afterGenSnapshotHook`) so the
  cross-chunk atomicity and enqueue/retire races are tested
  deterministically, not statistically.
- **End-to-end safety nets**: deletion executor pipeline (mock Gmail →
  store, trash + permanent modes, cross-source isolation) and the attachment
  lifecycle (dedup, cascade, orphan cleanup) run unchanged on both backends.
- **Cross-backend behavior pins**: FTS rank weight parity test runs on both
  backends; the known bm25-vs-ts_rank divergence is pinned by a dedicated
  test (`fts_rank_divergence_test.go`) rather than papered over.
- `SkipIfPostgres` requires a reason string — skips are auditable.

### 5.2 Gaps

- **T1 (Medium). No scale or plan validation.** Every live-PG test uses a
  small corpus. The riskiest findings in this review (S1 timeouts, A3
  aggregate latency, V1/V2 query shapes) are precisely the class of problem
  small-corpus tests cannot catch, and the fused-path comment explicitly
  defers an EXPLAIN ANALYZE that nobody has run. A one-off seeded 1M-row
  benchmark (even manual, documented in PG_STATUS) would close this.
- **T2 (Low).** No test feeds an oversized body through the PG FTS path
  (S3), nor a pathological batch through `BackfillFTS` on PG.
- **T3 (Low).** `make test-pg` runs `-tags fts5` only — vector packages
  compile out. CI compensates with the pgvector lane, but a developer running
  only `make test-pg` locally may believe vector-on-PG was exercised.
- **T4 (Info).** The TUI itself is never driven against PG (its engine is,
  via `pg_compat_test.go`). Acceptable given the TUI is engine-agnostic.

---

## 6. Security

### 6.1 Gmail access (the stated priority)

**Unchanged and sound.** The branch does not modify `internal/oauth`,
`internal/gmail`, or token storage; tokens remain in `~/.msgvault/tokens/`
regardless of DB backend, and no Gmail credential or token ever transits the
database. Sync remains read-only against Gmail; the deletion executor's Gmail
side is untouched (only covered by new e2e tests). Re-auth flows
(`getTokenSourceWithReauth`) validate before overwriting tokens. The PG
backend adds **no new path to Gmail credentials**.

### 6.2 Local/database security

- **SQL injection**: consistently parameterized across store, query, and
  vector layers. FTS inputs are sanitized per dialect;
  `websearch_to_tsquery` (fused path) is error-proof on arbitrary input by
  design. LIKE inputs are escaped with explicit `ESCAPE '\'`. The few
  identifier interpolations are hardcoded names or integers, with an explicit
  SECURITY comment where the helper could be misused
  (internal/store/identifier_match.go:86). No findings.
- **Log hygiene**: argv is sanitized before logging (credential flags
  redacted, root.go:182); the SQL logger emits statements plus arg *shapes*
  (type+length) only — addresses, subjects, bodies, and tokens never reach
  the log at WARN (db_logger.go:392). The embedding API key is resolved from
  an env var named in config (`api_key_env`), never stored in the file.
- **SEC1 (Low).** The PostgreSQL DSN — including password — lives in
  config.toml (`cfg.DatabaseDSN()`) and in `Store.dbPath`. It is not logged
  anywhere I could find (and git history shows a committed test DSN was
  found and scrubbed — commits f1550ba, 14895e6). Recommend: document file
  permissions for config.toml, and mention pgx's native `.pgpass` /
  `PGPASSWORD` support as alternatives so the password can stay out of the
  file. Also document `sslmode` guidance for the LAN deployment (CI uses
  `sslmode=disable`, which is fine for CI but shouldn't be cargo-culted).
- **SEC2 (Low).** Read-only PG enforcement is
  `default_transaction_read_only=on` via RuntimeParams — correctly applied to
  every pooled connection, but it is a session *default* that in-session SQL
  could flip back. Fine for the single-user trust model; if `serve`/MCP is
  ever exposed beyond localhost, a dedicated read-only database role is the
  real boundary.
- **SEC3 (Info).** Per-test schemas (`CREATE SCHEMA msgvault_test_<hex>`)
  interpolate only locally generated hex — safe.

---

## 7. Performance & scale (consolidated)

Ordered by expected impact on the stated target (20+ years, 1M+ messages):

1. **S1** — 30s pool-wide `statement_timeout` vs. maintenance operations
   (cascade source delete, FTS clear/rebuild, GIN/HNSW index builds,
   attachment dedupe in `InitSchema`, `seedPending`). These *fail*, not just
   slow down.
2. **A3** — TUI aggregates run live SQL with a full
   `attachments GROUP BY message_id` derived join; the Parquet layer that
   exists specifically to make this fast has no PG counterpart.
3. **V1** — fused hybrid search materializes all live message IDs per query;
   ANN-with-JOIN plan unverified (could mean sequential scans of the
   embeddings table per search).
4. **V2** — filtered ANN ships the whole matching ID set as one array
   parameter.
5. **P1** — `search_fts` lives inline on `messages`: every FTS upsert is a
   non-HOT update of a GIN-indexed column on a table with ~8 other indexes —
   double-write per synced message, MVCC bloat during bulk sync, and
   full-row rewrites for `FTSDeleteSQL`/`FTSClearSQL`. A separate FTS table
   (mirroring the SQLite design, and the project's own "keep the messages
   B-tree small" rule for `message_bodies`) would avoid most of it.
6. **P2** — `FTSNeedsBackfill` does `COUNT(*) WHERE search_fts IS NULL` — a
   sequential scan of `messages` on every startup probe (GIN cannot serve
   IS NULL; the comment's "indexable" claim is wrong). A partial index
   `WHERE search_fts IS NULL` or a watermark would fix it.
7. **V3/V4** — redundant embeddings index; retire-churn bloat.
8. Positives: connection pool sizing is sensible (25/5/5min);
   `hnsw.ef_search` is sized to the worst-case inner LIMIT and documented,
   with the recall ceiling beyond it explicitly called best-effort;
   chunked statements everywhere respect driver bind limits.

---

## 8. Cross-backend parity & migration

- **M1 (High, adoption-blocking). There is no SQLite→PostgreSQL data
  migration path.** Nothing in the tree (code, command, or docs) moves an
  existing archive into PG; `CopySubset` is SQLite-only by design. For this
  project's actual goal — moving a 20-year archive onto the pg/CT100 server —
  this is the missing piece. Note for whoever builds it: PG tables use
  `GENERATED ALWAYS AS IDENTITY`, so preserving message IDs requires
  `OVERRIDING SYSTEM VALUE` plus `setval` afterwards; FTS and embeddings can
  be rebuilt rather than copied. PG_STATUS.md does not mention this gap.
- **Documented divergences (good)**: FTS ranking (bm25 length-normalization
  vs ts_rank) is documented in docs/search-ranking.md and pinned by a test;
  the 10:4:1 weight parity is implemented and verified cross-backend; time
  bucketing is forced to UTC on both. The `'simple'` text-search config is
  the right parity choice vs FTS5 unicode61 (neither stems).
- **Undocumented divergences**:
  - V6 — fused search grammar (`websearch_to_tsquery`, no prefix match) vs
    non-fused (`to_tsquery` + `:*`) vs SQLite (FTS5 MATCH).
  - C8 — the store API's `after:`/`before:` bind RFC3339 *strings*
    (internal/store/api.go:469): typed-correct on PG, but on SQLite they
    text-compare against space-separated stored timestamps ('T' > ' '), so
    day-boundary behavior differs between backends (and between the store API
    and the query engine, which binds `time.Time` on both). Pre-existing, but
    now a parity matter.
  - A4 — `TextEngine` (conversation/text views) is SQLite-only; PG callers
    get clean type-assertion failures, but the resulting feature differences
    are not listed anywhere.
  - Q4 — `collections` schema drift between schema.sql and schema_pg.sql
    (PG adds `updated_at`, nullable `description`; SQLite has
    `NOT NULL DEFAULT ''` and no `updated_at`). Cosmetic today; the files
    claim to be "parallel".
  - Retire semantics: pgvector deletes retired embeddings, sqlitevec retains
    them — intentional, well-documented in PG_STATUS, just listing for
    completeness.

---

## 9. Ops & upstream-PR readiness

- **PG_STATUS.md is accurate about what works** — I spot-verified each "What
  Works" claim against the code and found no overstatement; rare in status
  docs and worth maintaining. What it lacks is the *costs* side: the A3
  Parquet-gap trade-off, S1's timeout hazard, V4 vacuum expectations, the
  extension-privilege requirement (V5), and the M1 migration gap.
- **Backup story is now split** and undocumented: messages move to PG
  (pg_dump / WAL archiving territory) while attachments remain
  content-addressed files under `~/.msgvault/attachments` on the app host.
  A restore needs both, consistently. Worth an ops paragraph.
- **CI is strong**: five lanes, pinned action SHAs, live PG and pgvector
  services, concurrency re-runs, tag-aware lint. The `test-pg` Makefile
  target's tag limitation (T3) is the only rough edge.
- **Commit history needs curation before upstreaming.** 107 commits, of
  which roughly a third are coordination-log/docs commits
  (`docs(R4): toggle [x]…`, `review(codex iterN)…`) referencing internal
  review IDs (cr2-N, iter13, H1–M4) that mean nothing to an upstream
  reviewer; one commit even exists to move those logs out of the repo.
  Squash into a small series (dialect groundwork → store layer → query
  engine → pgvector backend → embed portability → CI/docs) and translate
  the cr2-style references in code comments into self-contained rationale
  (most already read fine standalone).
- Comment-accuracy nits to fix in the same pass: store.go:644 ("PostgreSQL:
  empty" — it isn't), dialect_pg.go IsBusyError (wrong SQLSTATE claim, S2),
  dialect_pg.go FTSNeedsBackfill ("indexable", P2), sqldialect.go "mirrors
  SanitizeFTSQuery" (Q1).

---

## Finding index

| ID | Severity | One-liner |
|----|----------|-----------|
| S1 | High | 30s pool-wide statement_timeout breaks large maintenance ops on PG |
| M1 | High (adoption) | No SQLite→PG data migration path for existing archives |
| S3 | Medium | Unguarded tsvector 1MB limit; backfill clears-then-aborts |
| A3 | Medium | PG drops Parquet acceleration; live aggregates unvalidated at scale |
| V1 | Medium | FusedSearch materializes full live-ID CTE; ANN plan unverified |
| V2 | Medium | Filtered ANN ships entire matching ID set as one array param |
| T1 | Medium | No scale/EXPLAIN validation anywhere in the test suite |
| S2 | Low | IsBusyError misses 57014; busy-retry UX dead on PG |
| S4 | Low | Hand-maintained 18-table BeginExclusive lock list |
| S5 | Low | WithExclusiveLock fn-must-not-write contract is implicit |
| Q1 | Low | PG sanitizer lets `<`/`>` reach to_tsquery → runtime error |
| P1 | Low/Medium | Inline tsvector on messages → non-HOT write amplification |
| P2 | Low | FTSNeedsBackfill seq-scans messages each startup |
| V3 | Low | Redundant idx_embeddings_gen_msg index |
| V4 | Low | Retire-churn heap/HNSW bloat; document vacuum expectations |
| V5 | Low | CREATE EXTENSION on startup needs elevated privileges |
| SEC1 | Low | DSN+password in config.toml; document perms / .pgpass |
| SEC2 | Low | Read-only PG is session-default, not a role boundary |
| C3/C4 | Low | Rebind/InsertOrIgnore latent edge cases (comments, DEFAULT VALUES) |
| T2/T3/T4 | Low/Info | tsvector-limit untested; make test-pg tag gap; no TUI-on-PG |
| A2 | Info | Gmail-ID matching unscoped by source_id in deletion paths |
| A4 | Info | TextEngine feature gap on PG undocumented |
| V6 | Info | Three FTS grammars across modes/backends |
| C8 | Info | Store-API date filters: string vs typed timestamp divergence |
| Q4 | Info | collections schema drift between backends |
