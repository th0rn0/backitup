# backitup

Self-hosted, centralized **fleet backup** for Linux + macOS. A clean control plane
on top of proven tools (rsync, OpenSSH, rclone): one server, one webgui, many dumb
cron-triggered clients, with hot backups on the server and encrypted offsite tiering
to Google Drive / S3 / 40+ providers.

Add a machine by issuing a key in the webgui, paste one cron line on the host, and
watch the whole fleet's backup health from a single dashboard.

> **Status: in active development.** Lane 0 (foundation: domain model, SQLite store,
> mode seam, server skeleton, Docker images) is complete and tested. The webgui,
> SSH ingest, client modes, and lifecycle worker are in progress — see [Roadmap](#roadmap).
> The `app` container builds and runs today (`/healthz` is live).

---

## Table of contents

- [Why](#why)
- [How it works](#how-it-works)
- [Architecture](#architecture)
- [Quick start (Docker Compose)](#quick-start-docker-compose)
- [Building the images](#building-the-images)
- [Running the server with `docker run`](#running-the-server-with-docker-run)
- [Client setup (per host)](#client-setup-per-host)
- [Configuration reference](#configuration-reference)
- [Backup modes](#backup-modes)
- [Retention & offsite](#retention--offsite)
- [Security model](#security-model)
- [Development](#development)
- [Project layout](#project-layout)
- [Roadmap](#roadmap)

---

## Why

restic / Borg / Kopia are excellent backup *engines* with no clean centralized fleet
control. UrBackup has fleet control but a clunky UI and a heavy client. backitup is
the clean **control plane**: the server owns all configuration, the client is a dumb
uploader you drop on any host, and one dashboard answers the only question that
matters — *are my backups OK?*

backitup deliberately does **not** reinvent the data engine. Transfer is rsync +
OpenSSH, offsite + encryption is rclone, storage is plain files you can browse. The
code we write is the control plane, because that is where the value is.

## How it works

- **The server owns WHAT** — backup mode, excludes, retention, offsite target, keys.
- **The host's cron owns WHEN** — the client is fire-and-forget; you set the schedule
  by editing the host crontab. Changing frequency means editing cron, not the webgui.
- **The client is a dumb, stateless uploader** — triggered by cron, it runs once and
  exits. It reads the source **read-only** and never modifies the host.
- **Each client** has a server-issued SSH key (data channel) + bearer token (control
  channel) and its own confined directory on the server.
- **Hot backups** live on the server; the **lifecycle worker** tiers them to encrypted
  offsite storage and prunes per independent hot/cold retention horizons.

## Architecture

```
        host cron (owns WHEN)
            │ docker run --rm   (source bind-mounted READ-ONLY)
            ▼
   ┌─────────────────┐   SSH (rsync / sftp, per-client key)   ┌──────────────────┐
   │  client (Go)    │ ─────────────────────────────────────▶ │  sshd container  │
   │  dumb uploader  │                                         │  per-client      │
   │                 │ ◀── HTTPS: GET config / POST status ──▶ │  confined dir    │
   └─────────────────┘            (bearer token)               └──────────────────┘
                                                                        │ shared volumes
                                              ┌──────────────────────────▼──────────────┐
                                              │  app container (Go)                      │
                                              │  HTTP API + webgui + lifecycle timer     │
                                              │  SQLite · admin login · authorized_keys  │
                                              │  lifecycle: offsite (rclone+crypt) FIRST,│
                                              │             then prune; integrity verify │
                                              └──────────────────────────────────────────┘
```

Two server containers (design decision: one app process + one sshd). The app is a
single Go binary (API + webgui + lifecycle timer); SQLite is single-writer so one
process avoids cross-process lock coordination.

## Quick start (Docker Compose)

Requirements: Docker + the Compose plugin.

```sh
git clone https://github.com/th0rn0/backitup.git
cd backitup
docker compose up -d --build      # builds the app image and starts the stack
curl http://127.0.0.1:8080/healthz   # -> ok
docker compose logs -f app
```

The compose stack defines three volumes — `app-data` (SQLite + config), `backups`
(hot store), `authkeys` (the authorized_keys file the app writes for sshd) — and
binds the app to `127.0.0.1:8080` by default. Put it behind your own TLS reverse
proxy (or wait for built-in TLS) before exposing it.

```sh
docker compose down          # stop
docker compose down -v       # stop and DELETE all volumes (destroys backups!)
```

> The `sshd` service in `docker-compose.yml` is a placeholder for the SSH ingest
> plane (Lane A). The `app` service is fully functional today.

## Building the images

```sh
# Server (control plane): Alpine + rclone, ~145 MB, cgo-free.
docker build -t ghcr.io/th0rn0/backitup-server:dev -f Dockerfile .

# Client (dumb uploader): Alpine + rsync + openssh-client, ~25 MB.
docker build -t ghcr.io/th0rn0/backitup-client:dev -f Dockerfile.client .
```

Both are pure-Go (`CGO_ENABLED=0`) and build for `linux/amd64` and `linux/arm64`:

```sh
docker buildx build --platform linux/amd64,linux/arm64 \
  -t ghcr.io/th0rn0/backitup-server:dev -f Dockerfile .
```

## Running the server with `docker run`

```sh
docker run -d --name backitup \
  -p 127.0.0.1:8080:8080 \
  -v backitup-data:/data \
  -v backitup-backups:/srv/backups \
  ghcr.io/th0rn0/backitup-server:dev

curl http://127.0.0.1:8080/healthz   # -> ok
```

## Client setup (per host)

The client is **not** a long-running service. The webgui generates the key, token,
and the exact cron line for each client. The shape of that line:

```sh
# Back up /home/me/documents every night at 02:30 (tar.gz mode).
30 2 * * *  docker run --rm \
  --mount type=bind,src=/home/me/documents,dst=/source,readonly \
  -v /etc/backitup/laptop-docs:/secrets:ro \
  -e BACKITUP_SERVER=backup.example.com:2222 \
  -e BACKITUP_TOKEN_FILE=/secrets/token \
  ghcr.io/th0rn0/backitup-client:dev
```

Key points:

- `--mount ...,readonly` makes the source physically read-only — the client cannot
  modify the host even in principle.
- Run **multiple clients on one host** by adding more cron lines, each with its own
  source directory and its own secrets.
- The schedule lives here, in cron — backitup never changes it for you.

## Configuration reference

### Server (`backitup-server`)

| Variable        | Default               | Description                              |
|-----------------|-----------------------|------------------------------------------|
| `BACKITUP_DB`   | `/data/backitup.db`   | SQLite database path                     |
| `BACKITUP_ADDR` | `:8080`               | HTTP listen address                      |

### Client (`backitup-client`)

| Variable / flag    | Default     | Description                                       |
|--------------------|-------------|---------------------------------------------------|
| `BACKITUP_SERVER`  | (required)  | sshd ingest `host:port`                           |
| `BACKITUP_TOKEN`   | (required)  | bearer token for the control channel              |
| `BACKITUP_SOURCE`  | `/source`   | read-only source mount inside the container       |
| `BACKITUP_MODE`    | `targz`     | `targz` or `rsync` (normally fetched from server) |

Every variable has a matching flag (`-server`, `-token`, `-source`, `-mode`).

## Backup modes

Chosen per client in the webgui. They are different architectures, not two settings
of one pipeline:

| Mode    | How                                              | Retention            |
|---------|--------------------------------------------------|----------------------|
| `targz` | Each run uploads a dated `.tar.gz` snapshot archive | Prune old archives |
| `rsync` | `rsync --link-dest` into dated dirs (hardlink snapshots) | Prune old snapshot dirs |

`tar.gz` is simplest and fully self-contained per snapshot. `rsync` gives cheap
incremental snapshots on the hot store via hardlinks (only changed data costs space).

## Retention & offsite

- **Hot retention** (`retention_days`) prunes snapshots on the server.
- **Offsite retention** (`offsite_retention_days`) is **independent** — usually
  longer, because cold storage is your long-horizon copy.
- Offsite uses **rclone** with an encrypted **crypt** remote, so the provider only
  ever sees ciphertext. Adding a provider (Drive, S3, B2, …) is an rclone remote,
  not new code.
- The lifecycle worker runs **offsite first, then prune** — a snapshot is never
  pruned before it is confirmed offsited. Offsite pushes **immutable per-snapshot
  objects** (no destructive sync), so corruption cannot be replicated over your only
  cold copy.

## Security model

- **Non-destructive by design.** The client mounts the source **read-only**, only ever
  reads it, runs as a non-root user with a read-only root filesystem and dropped
  capabilities, and is covered by a test asserting source files are unchanged after a
  run (both modes, including `rsync --delete`).
- **Control channel is HTTPS** with the client verifying the certificate — the login
  password and bearer token never cross plaintext.
- **Offsite is encrypted** before it leaves (rclone crypt). The hot store on your own
  server is plaintext (the server is the trusted party; the offsite provider is not).
- **Webgui is behind an admin login** (argon2id, session cookie). It serves every
  client's data for download and issues keys, so it is never unauthenticated.
- **Per-client SSH access is confined**: `internal-sftp` + chroot for tar.gz clients,
  restricted `rrsync` locked to the client's directory for rsync clients.

## Development

```sh
go test ./...              # run all tests
go test -cover ./...       # with coverage
go build ./...             # build everything
gofmt -l . && go vet ./... # format + vet (both should be silent)

# Run the server locally (no Docker):
BACKITUP_DB=./data/backitup.db BACKITUP_ADDR=:8080 go run ./cmd/server
curl localhost:8080/healthz
```

Internal packages are at 100% statement coverage (`store` at 90% — the remainder is
defensive error branches that require fault injection). `cmd/*` are thin wiring around
the tested `internal/*` packages.

## Project layout

```
cmd/server        control plane entrypoint (wiring only)
cmd/client        dumb uploader entrypoint (wiring only)
internal/model    domain types + dashboard health derivation
internal/store    SQLite persistence (pure-Go driver, cgo-free)
internal/mode     per-mode behaviour seam (rsync / tar.gz), client + server sides
internal/server   HTTP control plane (testable handler construction)
internal/client   client config + validation
Dockerfile         server image (Alpine + rclone)
Dockerfile.client  client image (Alpine + rsync + openssh-client)
docker-compose.yml app + sshd ingest topology
```

## Roadmap

backitup is built in lanes (see the design doc). Lane 0 is done.

- [x] **Lane 0** — foundation: model, store, mode seam, server skeleton, Docker, tests
- [ ] **Lane A** — sshd ingest container + authorized_keys generation (key-sync seam)
- [ ] **Lane B** — webgui: admin login, fleet dashboard, `/api/v1/config` + `/status`, TLS
- [ ] **Lane C** — client modes: tar.gz end-to-end, then rsync (hardlink snapshots)
- [ ] **Lane D** — lifecycle worker: rclone offsite + prune + integrity verify

See `TODOS.md` for deferred work (e.g. client credential rotation).
