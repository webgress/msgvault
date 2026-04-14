# PostgreSQL Backend Setup

msgvault's default backend is SQLite, which is the right choice for most
users — it's a single file, requires no daemon, and handles a multi-decade
personal archive on a laptop without breaking a sweat.

PostgreSQL is a supported opt-in backend for deployments that need
true multi-client concurrency, server-mode hosting (NAS / home server with
multiple clients), or data co-location with downstream analytics (pgvector,
custom SQL, BI tools). This guide walks through setting one up.

> See [PG_STATUS.md](PG_STATUS.md) for the implementation state and known
> limitations. PostgreSQL 14 or newer is required.

## 1. Create the database and role

Connect as a PostgreSQL superuser and create a dedicated role + database.
The role does **not** need superuser privileges; ownership of the database
is sufficient.

```sql
CREATE ROLE msgvault WITH LOGIN PASSWORD 'changeme';
CREATE DATABASE msgvault OWNER msgvault;
```

If you will be hosting on a remote machine, edit `pg_hba.conf` to allow
the msgvault client host to connect (typically with `scram-sha-256` auth)
and reload the server.

## 2. Configure msgvault

Add `database_url` to your `~/.msgvault/config.toml` (or whatever
`$MSGVAULT_HOME/config.toml` resolves to):

```toml
[data]
database_url = "postgres://msgvault:changeme@localhost:5432/msgvault?sslmode=disable"
```

Connection-URL parameters follow [libpq conventions](https://www.postgresql.org/docs/current/libpq-connect.html#LIBPQ-CONNSTRING):

| Parameter | Purpose |
|---|---|
| `sslmode=disable` | No TLS. Use `require` or `verify-full` for production. |
| `search_path=myschema` | Scope msgvault tables to a specific schema. Useful for sharing a database across projects. |
| `application_name=msgvault` | Appears in `pg_stat_activity` for diagnostics. |
| `options=-c statement_timeout=60000` | Merge additional libpq `-c` options. msgvault adds its own `-c statement_timeout=30000` by default; your values merge with it. |

> When `database_url` is set, it takes precedence over the SQLite
> path (`~/.msgvault/msgvault.db`). Attachment storage, OAuth tokens, and
> the Parquet analytics cache still live in `$MSGVAULT_HOME` on disk.

## 3. Initialize the schema

```bash
msgvault init-db
```

This creates all tables, indexes, and the `messages.search_fts` tsvector
column with its GIN index. Verify:

```sql
\c msgvault
\dt
```

You should see 15 tables (`sources`, `participants`, `conversations`,
`messages`, `attachments`, `labels`, `message_labels`, `message_bodies`,
`message_raw`, `message_recipients`, `conversation_participants`,
`reactions`, `participant_identifiers`, `sync_runs`, `sync_checkpoints`).

## 4. Verify

Run a dry-run to confirm connectivity, schema, and permissions:

```bash
msgvault stats
```

Expected output (empty database):

```
Database: postgres://msgvault:***@localhost:5432/msgvault?sslmode=disable
  Messages:    0
  Threads:     0
  Attachments: 0
  Labels:      0
  Accounts:    0
  Size:        ~8 MB
```

The `Size` comes from `pg_database_size(current_database())` — a non-zero
value confirms the client is actually talking to PostgreSQL rather than
silently falling back to SQLite. (PostgreSQL's system catalogs and the
default tablespace take up a few MB even on a fresh database.)

## 5. Add an account and sync

Subsequent commands are identical regardless of backend:

```bash
msgvault add-account you@gmail.com
msgvault sync-full you@gmail.com --limit 100
msgvault tui
```

## Schema isolation (shared server)

If multiple applications share a PostgreSQL instance, put msgvault in its
own schema:

```sql
CREATE SCHEMA msgvault;
ALTER ROLE msgvault SET search_path = msgvault, public;
```

Then in `config.toml`:

```toml
[data]
database_url = "postgres://msgvault:changeme@host:5432/shared?search_path=msgvault"
```

Running `init-db` will create tables in the `msgvault` schema and leave the
`public` schema untouched.

## Read-only connections

The MCP server and read-only TUI callers open the database via
`Store.OpenReadOnly`, which sets `default_transaction_read_only = on`
pool-wide. This is a belt-and-braces check against accidental writes.

If you want a fully read-only operator role at the database level:

```sql
CREATE ROLE msgvault_reader WITH LOGIN PASSWORD 'readonly';
GRANT CONNECT ON DATABASE msgvault TO msgvault_reader;
GRANT USAGE ON SCHEMA public TO msgvault_reader;
GRANT SELECT ON ALL TABLES IN SCHEMA public TO msgvault_reader;
ALTER DEFAULT PRIVILEGES IN SCHEMA public GRANT SELECT ON TABLES TO msgvault_reader;
```

Then point the MCP/TUI-only client at that role via `database_url`.

## Backups

`pg_dump` is the supported backup mechanism:

```bash
pg_dump --format=custom --compress=9 --file=msgvault-$(date +%F).dump \
  -U msgvault -h host -d msgvault
```

msgvault's `subset` command is SQLite-only and will not work against a
PostgreSQL source (see PG_STATUS known limitations).

## Common issues

**`FATAL: no pg_hba.conf entry for host`**
Client host isn't allowed. Add the host/subnet to `pg_hba.conf` and
`SELECT pg_reload_conf()`.

**`ERROR: relation "messages" does not exist`**
You ran a command before `init-db`, or your `database_url` points to a
different database/schema than where you initialized. Verify with
`\c msgvault` and `\dt`.

**`column of type jsonb but expression is of type text`**
Stale binary pre-dating the JSONPlaceholder fix (pr3-postgresql-functional
or later). Rebuild: `make install`.

**Connection pool exhaustion**
msgvault's pool defaults to 25 open connections per process. If you
run many concurrent `msgvault serve` instances against the same PG
database, ensure `max_connections` in `postgresql.conf` is large enough
(`25 × N + headroom`).

**Tests fail with `password authentication failed`**
The `MSGVAULT_TEST_DB` URL is wrong, or the role's password has been
reset. `PGPASSWORD=... psql -h ... -U ... -c 'select 1'` to verify
credentials before running `make test-pg`.

## Running tests against your PostgreSQL

The integration test suite runs against whatever `MSGVAULT_TEST_DB`
points at. Each test creates an isolated schema (`msgvault_test_<random>`)
and drops it on cleanup, so running the suite against a shared database
is safe:

```bash
export MSGVAULT_TEST_DB="postgres://msgvault:changeme@localhost:5432/msgvault?sslmode=disable"
make test-pg
```

Expected wall-clock: ~60 seconds on PG 16, vs ~12 seconds on SQLite. The
gap is per-test schema setup overhead, not a performance gap in the
production path.
