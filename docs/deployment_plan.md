# msgvault Deployment Plan (webgress fork)

This document captures the production deployment plan for msgvault on
yuriy's home infrastructure. It is **fork-specific** and is intentionally
not part of the upstream `pr3-upstream` / `pr4-upstream` branches.

## Decision summary

- **PostgreSQL** lives on the existing `pg` LXC container (CT 100 on
  conductor). pgvector 0.8.2 is installed; vector extension is created
  in target databases on demand.
- **msgvault binary, attachments, OAuth tokens, analytics cache** live
  on the fileserver (the existing ZFS-mirrored archival host that holds
  the photo archive).
- **No double-storage on the Proxmox host.** msgvault attachments do
  not transit conductor's local `tpool`; they go straight to the
  fileserver's ZFS.
- **No internet-facing services for msgvault** day-to-day. Gmail OAuth
  uses the **device flow** (`--headless`) so no public callback is
  needed. AI / SSH access is internal only.
- **botsman is not required** for msgvault. It can host an optional
  reverse proxy later if a public URL is ever wanted, but that is
  future-tense and not on the critical path.

## Why this shape

- The fileserver is the box explicitly designated as archival, mirrored,
  important. msgvault attachments fit that role exactly — they are
  long-lived, infrequently mutated, and recoverable from off-site
  snapshots.
- Storing attachments on conductor + backing them up to the fileserver
  is redundant in the unhelpful sense: two copies on home infra, no
  off-site protection added. Putting them on the fileserver in the
  first place collapses the redundancy without losing the property
  that matters (the off-site ZFS-snapshot replication that protects
  photos already covers msgvault attachments).
- PostgreSQL on the existing `pg` host avoids spinning up a second
  Postgres instance. msgvault's database joins debaiter_dev,
  debaiter_prod, werboard, botsman_sandbox as another tenant.

## Architecture

```
                              Internet
                                 │
                                 │  (one-time Gmail OAuth via device flow,
                                 │   from your laptop browser — no callback)
                                 ▼
                          ┌────────────────┐
                          │  google.com    │
                          └────────────────┘
                                 ▲
                                 │ outbound HTTPS only
                                 │ (Gmail API)
                                 │
       ┌────────────────────────────────────────────────────────┐
       │                    home LAN                            │
       │                                                        │
       │   ┌──────────────────────────┐                         │
       │   │  fileserver (Ubuntu, ZFS)│                         │
       │   │                          │                         │
       │   │  /tank/photos/           │  existing, mode 700     │
       │   │                          │                         │
       │   │  /tank/msgvault/         │  new dataset            │
       │   │  ├── attachments/        │  content-addressed      │
       │   │  ├── tokens/             │  OAuth secrets          │
       │   │  ├── analytics/          │  parquet cache          │
       │   │  └── config.toml         │  DSN points at pg       │
       │   │                          │                         │
       │   │  msgvault binary         │  systemd unit           │
       │   │  + cron sync             │  runs as 'msgvault' user│
       │   └──────────────────────────┘                         │
       │              │                                         │
       │              │  postgres:// over LAN                   │
       │              ▼                                         │
       │   ┌──────────────────────────┐                         │
       │   │  pg (LXC CT 100)         │                         │
       │   │  PostgreSQL 15           │                         │
       │   │  + pgvector 0.8.2        │                         │
       │   │  → msgvault database     │                         │
       │   └──────────────────────────┘                         │
       │                                                        │
       │   AI access: SSH to fileserver as 'msgvault' user      │
       │   Human access: SSH to fileserver as 'yuriy'           │
       │   Remote browser: Tailscale mesh (optional)            │
       └────────────────────────────────────────────────────────┘
```

## Storage isolation on fileserver

The fileserver hosts the photo archive (high value). The msgvault
deployment must not be able to read, modify, or fill space at the
expense of the photo archive. The isolation model:

### Unix user separation
- New user `msgvault` (system account, no sudo, no group membership
  that grants access to other users' data)
- Owns `/tank/msgvault/` and all contents
- Cannot read `/tank/photos/` (which is mode 700, owned by `yuriy`)
- Cannot escalate — no entry in `/etc/sudoers` for `msgvault`

### ZFS quota
- `zfs set quota=500G tank/msgvault` (adjust to taste based on Gmail
  archive size — first sync will tell us the real number)
- Prevents a runaway sync or AI mistake from filling the pool and
  impacting the photo archive

### ZFS snapshots
- Daily snapshot of `tank/msgvault` with 14-day retention
- Same snapshot replication pattern that already protects photos
- One-line recovery from any AI mistake: `zfs rollback`

### SSH access
- AI / Claude access via a key in `~msgvault/.ssh/authorized_keys`,
  scoped to the `msgvault` user only
- yuriy's personal key is on the `yuriy` account, separately
- Optionally lock the AI key with `from="<allowed-IPs>"`

### systemd hardening for the daemon
When `msgvault serve` runs as a systemd unit:
```ini
[Service]
User=msgvault
Group=msgvault
ProtectSystem=strict
ProtectHome=yes
ReadWritePaths=/tank/msgvault
PrivateTmp=yes
NoNewPrivileges=yes
LockPersonality=yes
MemoryDenyWriteExecute=yes
```

### Threat model addressed
- Read photo archive: blocked by `chmod 700 /tank/photos`
- Damage system files: blocked by no-sudo
- Install packages, change services: blocked by no-sudo
- Fill the disk: bounded by ZFS quota
- Recover from AI mistakes inside `/tank/msgvault`: ZFS snapshots
- The blast radius of any failure is exactly `/tank/msgvault`,
  recoverable from snapshot

## OAuth flow

Use device flow (`--headless`) for adding Gmail accounts. The flow:

1. On fileserver: `msgvault add-account you@gmail.com --headless`
2. msgvault prints a URL and a code
3. Open the URL on your laptop (where you're already logged into the
   Google account); enter the code; authorize
4. msgvault polls Google until authorization completes; tokens are
   written to `/tank/msgvault/tokens/`

No public callback. No proxy. No DNS. No certs.

This eliminates the original reason botsman was going to host an
OAuth proxy.

## Day-to-day operations

| Operation | How |
|---|---|
| Add Gmail account | `ssh msgvault@fileserver msgvault add-account ... --headless` |
| Full sync | cron job on fileserver, runs as `msgvault` user |
| Incremental sync | cron job on fileserver, runs as `msgvault` user |
| Embed vectors | scheduled task, picks up post-sync |
| Build cache | scheduled task or on-demand |
| Search / TUI | SSH in as `msgvault`, run `msgvault tui` |
| HTTP API access | `msgvault serve --listen 100.x.x.x:8080` over Tailscale (if remote browser access wanted) |
| MCP server | `msgvault mcp` listening on Tailscale (if Claude desktop is connecting from outside LAN) |
| Backup | ZFS snapshot of `tank/msgvault` + `pg_dump` of msgvault DB on pg host, replicated off-site |

## What this means for the existing PR series

This deployment plan is independent of the PostgreSQL port itself:
- PR3 (dialect/store layer functional on PG) → upstream as planned
- PR4 (pgvector backend, FTS parity, deletion + attachment E2E, CI lane)
  → upstream as planned
- Deployment-specific docs (this file, plus future tasks for systemd
  units, cron, etc.) stay in the webgress fork only

## What botsman is NOT doing (for msgvault)

- Not the OAuth proxy (device flow makes it unnecessary)
- Not the reverse proxy (no public service needed)
- Not the msgvault host (fileserver is)

botsman can still run other services independently — this document
just removes msgvault from its responsibilities.

## Open questions / decisions still to make

1. **Fileserver hostname for the SSH alias.** This document calls it
   "fileserver"; the actual SSH alias is `backup`. Confirm naming.
2. **ZFS dataset quota.** Start with 500G or 1T? First sync will tell
   us if we sized it right.
3. **Snapshot retention.** Daily for 14 days proposed; adjust based on
   how often `zfs rollback` actually gets used in practice.
4. **Tailscale (or alternative mesh).** Confirm Tailscale is in use on
   home network and the fileserver. If not, set up before deployment
   if remote browser access is wanted.
5. **PostgreSQL database creation.** Need to `createdb msgvault` on pg
   host (peer auth gives me autonomous access now), grant role,
   `CREATE EXTENSION vector`.

## Provisioning checklist (when ready to deploy)

The handoff between human and AI is clearly bounded:

### Human steps (require sudo on fileserver)
1. `useradd -r -m -d /tank/msgvault -s /bin/bash msgvault`
2. `zfs create -o compression=lz4 -o atime=off -o quota=500G tank/msgvault`
3. `chown msgvault:msgvault /tank/msgvault`
4. `chmod 750 /tank/msgvault`
5. Confirm `chmod 700 /tank/photos`
6. Drop AI's SSH key into `~msgvault/.ssh/authorized_keys`
7. Install Go (or accept a prebuilt binary delivered by AI)
8. Schedule daily ZFS snapshot for `tank/msgvault` (zfs-auto-snapshot
   or systemd timer)

### AI steps (everything else)
- Build / deploy msgvault binary to `/tank/msgvault/bin/`
- Write `config.toml` pointing at `postgres://msgvault@pg:5432/msgvault`
- Create PG role + database on pg host (peer auth as admin gives
  access)
- `CREATE EXTENSION vector` on the new database
- `msgvault init-db`
- `msgvault add-account ... --headless` (human authorizes in browser
  once)
- `msgvault sync-full` (initial pull)
- `msgvault embed-vector` (initial embedding build)
- Install systemd unit + cron for ongoing sync
- Validate end-to-end (the "real test" task already filed in werboard)
