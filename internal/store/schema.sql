-- backitup schema (Lane 0). SQLite, single-writer (design doc D1: one app process).
-- The lifecycle worker writes only short status rows and never holds a write txn
-- across a long rclone/tar subprocess (eng review SQLite discipline).

PRAGMA journal_mode = WAL;
PRAGMA foreign_keys = ON;

CREATE TABLE IF NOT EXISTS clients (
    id                      INTEGER PRIMARY KEY AUTOINCREMENT,
    name                    TEXT    NOT NULL UNIQUE,
    mode                    TEXT    NOT NULL CHECK (mode IN ('targz','rsync')),
    source_label            TEXT    NOT NULL DEFAULT '',
    excludes                TEXT    NOT NULL DEFAULT '[]',  -- JSON array of globs
    retention_days          INTEGER NOT NULL DEFAULT 14,    -- hot store horizon
    offsite_retention_days  INTEGER NOT NULL DEFAULT 90,    -- cold horizon, independent (D8)
    expected_interval_secs  INTEGER NOT NULL DEFAULT 0,     -- advisory only (staleness; D1)
    offsite_remote          TEXT    NOT NULL DEFAULT '',    -- rclone remote; '' = no offsite
    offsite_dir             TEXT    NOT NULL DEFAULT '',    -- subdir within remote; '' = client slug
    ssh_pubkey              TEXT    NOT NULL DEFAULT '',
    token_hash              TEXT    NOT NULL DEFAULT '',     -- argon2id of bearer token (D4)
    enabled                 INTEGER NOT NULL DEFAULT 1,
    created_at              TEXT    NOT NULL,
    version                 INTEGER NOT NULL DEFAULT 1,  -- CAS guard for credential rotation
    skip_symlinks           INTEGER NOT NULL DEFAULT 0   -- omit symlinks from backup
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
    log_tail     TEXT    NOT NULL DEFAULT ''   -- capped at 4 KB before insert
);

-- Dashboard reads "latest run per client" off this index (eng review: no N+1).
CREATE INDEX IF NOT EXISTS idx_runs_client_started
    ON runs (client_id, started_at DESC);

CREATE TABLE IF NOT EXISTS offsite_objects (
    id           INTEGER PRIMARY KEY AUTOINCREMENT,
    client_id    INTEGER NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
    snapshot_id  TEXT    NOT NULL,
    remote       TEXT    NOT NULL,
    bytes        INTEGER NOT NULL DEFAULT 0,
    uploaded_at  TEXT    NOT NULL,
    UNIQUE (client_id, snapshot_id, remote)
);

CREATE INDEX IF NOT EXISTS idx_offsite_client
    ON offsite_objects (client_id, uploaded_at DESC);

-- Single admin account (D3: built-in login, argon2id).
CREATE TABLE IF NOT EXISTS admin (
    id            INTEGER PRIMARY KEY CHECK (id = 1),
    username      TEXT NOT NULL,
    password_hash TEXT NOT NULL,
    created_at    TEXT NOT NULL
);
