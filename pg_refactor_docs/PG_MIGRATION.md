# Data Migration Between SQLite and PostgreSQL

msgvault ships with a `migrate-db` command that copies every row from one
database to another, in either direction:

| From → To | Typical use case |
|---|---|
| SQLite → PostgreSQL | Graduating a single-user archive to a shared server |
| PostgreSQL → SQLite | Taking an offline snapshot, laptop-friendly subset |
| SQLite → SQLite | Relocating or consolidating files |
| PostgreSQL → PostgreSQL | Copying between clusters (use `pg_dump` where practical) |

Row IDs are preserved across the copy, so external references
(attachment storage paths, deletion manifests, OAuth token files) remain
valid without rewriting.

> See [PG_SETUP.md](PG_SETUP.md) for initial PostgreSQL setup and
> [PG_STATUS.md](PG_STATUS.md) for the feature status of the PG backend.

## TL;DR

```bash
# SQLite → PostgreSQL
msgvault migrate-db \
    --from ~/.msgvault/msgvault.db \
    --to postgres://msgvault:pw@localhost:5432/msgvault?sslmode=disable

# PostgreSQL → SQLite
msgvault migrate-db \
    --from postgres://msgvault:pw@db.local:5432/msgvault?sslmode=disable \
    --to ~/.msgvault-offline/msgvault.db
```

The destination schema is initialized automatically. If you would rather
prepare it separately, run `msgvault init-db` pointed at the destination
first — the migration is idempotent on schema creation.

## What gets copied

All fifteen msgvault tables are copied in foreign-key dependency order:

```
sources → participants → participant_identifiers
conversations → conversation_participants
labels
messages
  ├── message_recipients
  ├── message_labels
  ├── message_bodies
  ├── message_raw
  ├── attachments
  └── reactions
sync_runs, sync_checkpoints
```

What is **not** copied:

- **Attachment files on disk** (`~/.msgvault/attachments/…`) — these are
  content-addressed files outside the database. Copy the directory
  yourself if you are moving hosts. The database row still points at the
  same `storage_path`, so placing the directory at the same relative
  location on the new host is all that's required.
- **OAuth tokens** (`~/.msgvault/tokens/`) — also live outside the
  database.  They are bound to the source row's `identifier`, not its
  `id`, so copying the directory is enough.
- **The Parquet analytics cache** (`~/.msgvault/analytics/`) — it's a
  derived artifact. Rebuild it on the destination with
  `msgvault build-cache` after migrating.
- **The full-text search index** — it's rebuilt automatically after the
  copy. Use `--skip-fts-rebuild` to defer it.

## Step by step: SQLite → PostgreSQL

### 1. Provision the destination

Follow [PG_SETUP.md](PG_SETUP.md) §1 and §2 to create the role and
database and add `database_url` to your config. You can keep running the
SQLite binary while you prepare the PG side — nothing locks until you
actually run the migration.

### 2. Dry-run for a row count

```bash
msgvault migrate-db --from ~/.msgvault/msgvault.db \
                    --to postgres://msgvault:pw@host:5432/msgvault \
                    --dry-run
```

The dry-run opens the source read-only and prints per-table counts. The
destination URL is parsed for validation but never contacted.

### 3. Run the migration

```bash
msgvault migrate-db --from ~/.msgvault/msgvault.db \
                    --to postgres://msgvault:pw@host:5432/msgvault
```

You'll see per-table progress followed by a summary:

```
Migrating /home/alice/.msgvault/msgvault.db → postgres://msgvault:***@host:5432/msgvault

Migration complete:
  sources                      3 rows
  participants                 4,812 rows
  conversations                6,404 rows
  messages                     48,917 rows
  message_bodies               48,917 rows
  message_recipients           132,044 rows
  labels                       22 rows
  message_labels               198,303 rows
  attachments                  1,203 rows
  message_raw                  48,917 rows
  sync_runs                    412 rows
  TOTAL                        488,954 rows
  ELAPSED                      2m14s
```

The whole copy runs inside a single destination transaction. If it is
interrupted (Ctrl-C, network blip, disk full), the destination rolls back
to its pre-migration state and the source is unchanged.

### 4. Copy attachments

If you're moving to a new host, copy the attachments directory:

```bash
rsync -a ~/.msgvault/attachments/ user@newhost:~/.msgvault/attachments/
```

Copy `tokens/` the same way if you want to keep OAuth sessions alive.

### 5. Point the config at PostgreSQL

Set `database_url` in `~/.msgvault/config.toml` on the destination host
(if not already done). From this point on, every msgvault command runs
against the PG database.

Verify with:

```bash
msgvault stats
```

### 6. (Optional) Rebuild the Parquet cache

```bash
msgvault build-cache --full-rebuild
```

### 7. Archive the SQLite file

Keep the `.db` file around until you've confirmed the PG side is healthy
(say, a week of normal use). Once you're confident, the file can be
deleted — or kept as a point-in-time snapshot.

## Step by step: PostgreSQL → SQLite

Symmetrical. The most common reason to go this direction is to take a
portable snapshot for an air-gapped machine or a laptop.

```bash
msgvault migrate-db \
    --from postgres://msgvault:pw@host:5432/msgvault?sslmode=disable \
    --to /path/to/snapshot/msgvault.db
```

The destination SQLite file is created if it doesn't exist. All indexes
and the FTS5 virtual table are populated automatically.

Attachments still live outside the database — copy the directory along
with the file.

## Flags

| Flag | Purpose |
|---|---|
| `--from` | Source: SQLite path or `postgres://` URL. Required. |
| `--to` | Destination: SQLite path or `postgres://` URL. Required. |
| `--dry-run` | Report source row counts; no writes. |
| `--batch-size N` | Rows per multi-VALUES INSERT (default 200, auto-clamped under the SQLite 999-parameter limit). Increase to ~500 on PG→PG for modest speed gains on slow networks. |
| `--skip-fts-rebuild` | Don't run `BackfillFTS` on the destination. Use when you'll re-sync afterwards (syncing repopulates FTS rows as messages arrive). |
| `--allow-non-empty` | Bypass the guard that refuses to run when the destination has existing rows. Unsafe — IDs and unique constraints will almost certainly collide. |

## Verification

After migrating, a quick sanity check:

```bash
# Both should report the same row counts
msgvault stats                                    # against destination config
MSGVAULT_HOME=/tmp/old msgvault stats             # against a config pointing at the source
```

Spot-check FTS on the destination:

```bash
msgvault search 'some distinctive phrase' | head
```

Launch the TUI against the destination to confirm messages render end to
end:

```bash
msgvault tui
```

## How the copy works (implementation notes)

The migration is a pure-Go streaming copy, not a `pg_dump` / `.dump`
wrapper. This is deliberate:

- **Schema-aware.** Each table's column list is explicit in
  [`internal/store/migrate.go`](../internal/store/migrate.go). Column
  kinds (text / int / bool / time / json / bytes) are normalized on scan
  and re-bound on insert, so SQLite's TEXT timestamps and 0/1 booleans
  map correctly to PostgreSQL's `TIMESTAMPTZ` and `BOOLEAN`, and PG's
  `JSONB` columns round-trip through strings with an explicit `?::jsonb`
  cast.
- **ID-preserving.** Each row is inserted with its original primary
  key. On PostgreSQL this uses `INSERT … OVERRIDING SYSTEM VALUE` to
  bypass the `GENERATED ALWAYS AS IDENTITY` constraint. After commit,
  every affected identity sequence is advanced past `MAX(id)` so the
  next `INSERT` issued by msgvault produces a non-colliding ID.
- **FK-safe.** Tables are copied parents-before-children, and
  `messages` is streamed in ascending `id` order so the self-FK
  `reply_to_message_id` (a reply to an earlier message) is always
  satisfied mid-copy.
- **Atomic on the destination.** The entire copy lives in one
  destination transaction. Commit happens only after every table is
  copied; a failure rolls the destination back to whatever schema state
  `init-db` produced.
- **FTS rebuilt last.** The FTS index (SQLite FTS5 virtual table, or
  PostgreSQL `tsvector` column) is rebuilt via `Store.BackfillFTS` after
  the main copy commits — faster than streaming per-row FTS updates and
  dialect-correct by definition.

## Known limitations

- **Single transaction on the destination.** For very large archives
  (tens of GB), the destination-side transaction can be long-running.
  Against PG this holds a WAL segment open; against SQLite it pins a
  journal file. Neither is a correctness issue, but it does mean you
  should give the destination host enough free space for `2×` the
  compressed source. Splitting into per-table commits is a future
  option — file an issue if you hit it.
- **No resume after interruption.** If the migration aborts partway,
  the destination rolls back and you start over from scratch. For
  multi-hour runs, consider snapshotting the destination tablespace
  (PG) or copying the in-progress `.db` file (SQLite) before retrying.
- **Attachments are not walked.** The migration command does not
  inspect the attachments directory. If a `message_raw` row references
  a content hash whose file is missing on the destination host, the
  database copy still succeeds — the file-level check is a separate
  concern (see the `verify` command).
- **Schema version mismatch.** Both sides must be on the same msgvault
  schema version. If you built the source with an older binary, run
  `init-db` on the source first to apply any pending SQLite
  `LegacyColumnMigrations`. The PG schema is always complete (no
  `ADD COLUMN` migrations on that side).

## When to use `pg_dump` or `sqlite3 .dump` instead

The native-Go migrator is the right tool for cross-dialect moves. For
**same-dialect** copies, the database's own tools are faster and
battle-tested:

- **PG → PG:** prefer `pg_dump --format=custom` + `pg_restore`.
- **SQLite → SQLite:** prefer `sqlite3 src.db '.backup dest.db'` or just
  copy the file while msgvault is idle.

`migrate-db` still works for same-dialect copies — it's convenient when
you're scripting against the msgvault CLI anyway — but the native tools
will be a few times faster on large archives.
