-- backitup schema — the ONE canonical shape of the database.
--
-- Open() applies this file in two ordered phases (see migrate.go). Phase one
-- creates whatever tables are missing; phase two rebuilds any existing table
-- whose stored definition is not this one, and only then creates the indexes.
-- Each phase is a single transaction, gated by PRAGMA user_version, so a
-- database already at this version is not touched at all.
--
-- Column order is part of the schema. Migrations do not append columns to an old
-- table — they rebuild it to the order below — so a freshly created database and
-- an upgraded one have byte-identical column order. That is what makes a row
-- copy between an old and a new table (INSERT ... SELECT) safe: add new columns
-- at the END of their table, and never rely on SELECT *.
--
-- Connection pragmas (journal_mode=WAL, foreign_keys=ON, busy_timeout) are set
-- by Open(), not here: they cannot be changed inside the migration transaction.

CREATE TABLE IF NOT EXISTS clients (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    name                    TEXT    NOT NULL UNIQUE,
    slug                    TEXT    NOT NULL DEFAULT '',  -- unique storage id; see idx_clients_slug
    mode                    TEXT    NOT NULL CHECK (mode IN ('targz','rsync')),
    source_label            TEXT    NOT NULL DEFAULT '',
    excludes                TEXT    NOT NULL DEFAULT '[]',  -- JSON array of globs
    retention_days          INTEGER NOT NULL DEFAULT 14,    -- hot store horizon; offsite is the B2 rule's job
    expected_interval_secs  INTEGER NOT NULL DEFAULT 0,     -- advisory only (staleness; D1)
    offsite_remote          TEXT    NOT NULL DEFAULT '',    -- rclone remote; '' = no offsite
    offsite_dir             TEXT    NOT NULL DEFAULT '',    -- subdir within remote; '' = client slug
    offsite_interval_secs   INTEGER NOT NULL DEFAULT 0,     -- min secs between passes; 0 = every pass
    offsite_cron            TEXT    NOT NULL DEFAULT '',    -- upload schedule; '' = use the interval
    ssh_pubkey              TEXT    NOT NULL DEFAULT '',
    token_hash              TEXT    NOT NULL DEFAULT '',     -- argon2id of bearer token (D4)
    token_prefix            TEXT    NOT NULL DEFAULT '',     -- shown to the operator to tell tokens apart
    enabled                 INTEGER NOT NULL DEFAULT 1,
    created_at              TEXT    NOT NULL,
    version                 INTEGER NOT NULL DEFAULT 1,  -- CAS guard for credential rotation
    skip_symlinks           INTEGER NOT NULL DEFAULT 0,  -- omit symlinks from backup
    offsite_upload_mode     TEXT    NOT NULL DEFAULT 'all',  -- 'all' or 'latest': which snapshots to push
    -- Fidelity (rsync -S/-H/-A/-X). 0 = the pre-fidelity snapshot shape, which
    -- is what every existing client keeps on upgrade; the create form turns
    -- sparse on for NEW clients only.
    preserve_sparse         INTEGER NOT NULL DEFAULT 0,
    preserve_hardlinks      INTEGER NOT NULL DEFAULT 0,
    preserve_acls           INTEGER NOT NULL DEFAULT 0,
    preserve_xattrs         INTEGER NOT NULL DEFAULT 0,
    -- Group-readable snapshots (rsync --chmod=Dg+rx,Fg+r). 0 = the snapshot's
    -- modes mirror the source, which is what every client that existed before
    -- the setting produces; see model.Client.GroupReadable for what turning it
    -- on costs.
    group_readable          INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS runs (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    client_id    INTEGER NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
    started_at   TEXT    NOT NULL,
    finished_at  TEXT    NOT NULL,
    status       TEXT    NOT NULL CHECK (status IN ('ok','failed','overlap','running')),
    bytes        INTEGER NOT NULL DEFAULT 0,
    files        INTEGER NOT NULL DEFAULT 0,
    snapshot_id  TEXT    NOT NULL DEFAULT '',
    log_tail     TEXT    NOT NULL DEFAULT ''   -- capped at model.MaxLogTail (64 KB) before insert
);

-- How much disk each client's hot store occupies, as measured by the lifecycle
-- pass. A measurement, not a setting: it is written once per pass and the pages
-- only read it, so no request ever walks a backup tree.
--
-- disk_bytes is ALLOCATED blocks of unique inodes (st_blocks*512, each
-- (st_dev, st_ino) counted once) — what `du` reports and what the operator pays
-- for. It is never apparent size: a sparse image and a hardlinked rsync
-- snapshot both lie under that measure. disk_partial marks a walk that stopped
-- at its entry cap, which makes disk_bytes a LOWER BOUND that must be rendered
-- as "≥". measured_at '' means never measured (a client whose first pass has not
-- run yet), which is different from a measured zero.
CREATE TABLE IF NOT EXISTS client_storage (
    client_id    INTEGER PRIMARY KEY REFERENCES clients(id) ON DELETE CASCADE,
    disk_bytes   INTEGER NOT NULL DEFAULT 0,
    disk_partial INTEGER NOT NULL DEFAULT 0,
    measured_at  TEXT    NOT NULL DEFAULT ''
);

CREATE TABLE IF NOT EXISTS offsite_objects (
    id                 INTEGER PRIMARY KEY AUTOINCREMENT,
    client_id          INTEGER NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
    snapshot_id        TEXT    NOT NULL,
    remote             TEXT    NOT NULL,
    bytes              INTEGER NOT NULL DEFAULT 0,
    uploaded_at        TEXT    NOT NULL,
    checksum           TEXT    NOT NULL DEFAULT '',  -- SHA-256 of the archive; '' for legacy rows
    -- '' = this row owns a real remote object; otherwise the snapshot id whose
    -- remote object holds this snapshot's content (a deduplicated upload).
    dedup_of           TEXT    NOT NULL DEFAULT '',
    remote_missing     INTEGER NOT NULL DEFAULT 0,   -- last rclone check confirmed the object is gone
    remote_verified_at TEXT    NOT NULL DEFAULT '',  -- '' = never verified
    UNIQUE (client_id, snapshot_id, remote)
);

CREATE TABLE IF NOT EXISTS remotes (
    id         INTEGER PRIMARY KEY AUTOINCREMENT,
    name       TEXT NOT NULL UNIQUE,
    backend    TEXT NOT NULL,
    config     TEXT NOT NULL DEFAULT '{}',
    created_at TEXT NOT NULL,
    -- 1 = a backend this build no longer supports (s3, drive, sftp, …): the row
    -- is kept so an operator's configured remote is not deleted behind their
    -- back, but the lifecycle skips it and the UI says to recreate it as B2.
    unsupported INTEGER NOT NULL DEFAULT 0
);

CREATE TABLE IF NOT EXISTS offsite_runs (
    id                  INTEGER PRIMARY KEY AUTOINCREMENT,
    client_id           INTEGER NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
    triggered_by        TEXT    NOT NULL DEFAULT 'scheduled',
    started_at          TEXT    NOT NULL,
    finished_at         TEXT    NOT NULL DEFAULT '',
    status              TEXT    NOT NULL DEFAULT 'running',
    snapshots_uploaded  INTEGER NOT NULL DEFAULT 0,
    bytes_uploaded      INTEGER NOT NULL DEFAULT 0,
    error_text          TEXT    NOT NULL DEFAULT ''
);

-- Web UI accounts (D3: built-in login, argon2id). admin (below) is the legacy
-- single-account row; new code uses this table.
CREATE TABLE IF NOT EXISTS users (
    id            INTEGER PRIMARY KEY AUTOINCREMENT,
    username      TEXT NOT NULL UNIQUE,
    password_hash TEXT NOT NULL,
    created_at    TEXT NOT NULL
);

-- Single admin account (D3: built-in login, argon2id).
CREATE TABLE IF NOT EXISTS admin (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    username      TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    created_at    TEXT NOT NULL
);

-- Indexes come last: a rebuild drops the old table together with its indexes,
-- and idx_clients_slug can only be created once every client has a unique slug.

-- Dashboard reads "latest run per client" off this index (eng review: no N+1).
CREATE INDEX IF NOT EXISTS idx_runs_client_started
    ON runs (client_id, started_at DESC);

CREATE INDEX IF NOT EXISTS idx_offsite_client
    ON offsite_objects (client_id, uploaded_at DESC);

CREATE INDEX IF NOT EXISTS idx_offsite_runs_client
    ON offsite_runs (client_id, started_at DESC);

-- The database, not the application, guarantees one client per storage
-- directory. See backfillClientSlugs for how pre-column rows get their value.
CREATE UNIQUE INDEX IF NOT EXISTS idx_clients_slug
    ON clients (slug);
