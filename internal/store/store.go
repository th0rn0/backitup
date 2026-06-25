// Package store is backitup's SQLite persistence layer (design doc D1: SQLite,
// single-writer). It owns schema migration and CRUD for clients, runs, and
// offsite objects. Driver: modernc.org/sqlite (pure Go) so the server image is
// cgo-free and trivially multi-arch.
package store

import (
	"context"
	"database/sql"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/th0rn0/backitup/internal/model"
)

//go:embed schema.sql
var schemaSQL string

// Store wraps the database handle.
type Store struct{ db *sql.DB }

// Open opens (or creates) the SQLite database at dsn and applies the schema.
// dsn is a file path; busy_timeout keeps brief writer contention from erroring.
func Open(dsn string) (*Store, error) {
	if dir := filepath.Dir(dsn); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create db dir: %w", err)
		}
	}
	db, err := sql.Open("sqlite", dsn+"?_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)")
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	// Single-writer model; serialise to avoid SQLITE_BUSY under the lifecycle timer.
	db.SetMaxOpenConns(1)
	if _, err := db.Exec(schemaSQL); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	// Additive migration: add version column to existing databases.
	// CREATE TABLE IF NOT EXISTS already adds it for new databases; this handles
	// upgrades. SQLite has no ALTER TABLE ADD COLUMN IF NOT EXISTS, so we ignore
	// the "duplicate column name" error that fires when the column already exists.
	for _, migration := range []string{
		`ALTER TABLE clients ADD COLUMN version       INTEGER NOT NULL DEFAULT 1`,
		`ALTER TABLE clients ADD COLUMN skip_symlinks INTEGER NOT NULL DEFAULT 0`,
		`ALTER TABLE clients ADD COLUMN offsite_dir            TEXT    NOT NULL DEFAULT ''`,
		`ALTER TABLE clients ADD COLUMN offsite_interval_secs  INTEGER NOT NULL DEFAULT 0`,
	} {
		if _, err := db.Exec(migration); err != nil && !strings.Contains(err.Error(), "duplicate column name") {
			db.Close()
			return nil, fmt.Errorf("migrate: %w", err)
		}
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS users (
		id            INTEGER PRIMARY KEY AUTOINCREMENT,
		username      TEXT NOT NULL UNIQUE,
		password_hash TEXT NOT NULL,
		created_at    TEXT NOT NULL
	)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate users: %w", err)
	}
	// Migrate runs.status CHECK to include 'running'. SQLite has no ALTER
	// CONSTRAINT; we recreate the table using the recommended swap pattern.
	// Check sqlite_master to detect whether the migration is still needed.
	var runsDDL string
	_ = db.QueryRow(`SELECT sql FROM sqlite_master WHERE type='table' AND name='runs'`).Scan(&runsDDL)
	if !strings.Contains(runsDDL, "'running'") {
		for _, stmt := range []string{
			`PRAGMA foreign_keys=OFF`,
			`CREATE TABLE runs_new (
				id           INTEGER PRIMARY KEY AUTOINCREMENT,
				client_id    INTEGER NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
				started_at   TEXT    NOT NULL,
				finished_at  TEXT    NOT NULL,
				status       TEXT    NOT NULL CHECK (status IN ('ok','failed','overlap','running')),
				bytes        INTEGER NOT NULL DEFAULT 0,
				files        INTEGER NOT NULL DEFAULT 0,
				snapshot_id  TEXT    NOT NULL DEFAULT '',
				log_tail     TEXT    NOT NULL DEFAULT ''
			)`,
			`INSERT INTO runs_new SELECT * FROM runs`,
			`DROP TABLE runs`,
			`ALTER TABLE runs_new RENAME TO runs`,
			`CREATE INDEX IF NOT EXISTS idx_runs_client_started ON runs (client_id, started_at DESC)`,
			`PRAGMA foreign_keys=ON`,
		} {
			if _, err := db.Exec(stmt); err != nil {
				db.Close()
				return nil, fmt.Errorf("migrate runs status: %w", err)
			}
		}
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS remotes (
		id         INTEGER PRIMARY KEY AUTOINCREMENT,
		name       TEXT NOT NULL UNIQUE,
		backend    TEXT NOT NULL,
		config     TEXT NOT NULL DEFAULT '{}',
		created_at TEXT NOT NULL
	)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate remotes: %w", err)
	}
	if _, err := db.Exec(`CREATE TABLE IF NOT EXISTS offsite_runs (
		id                  INTEGER PRIMARY KEY AUTOINCREMENT,
		client_id           INTEGER NOT NULL REFERENCES clients(id) ON DELETE CASCADE,
		triggered_by        TEXT    NOT NULL DEFAULT 'scheduled',
		started_at          TEXT    NOT NULL,
		finished_at         TEXT    NOT NULL DEFAULT '',
		status              TEXT    NOT NULL DEFAULT 'running',
		snapshots_uploaded  INTEGER NOT NULL DEFAULT 0,
		bytes_uploaded      INTEGER NOT NULL DEFAULT 0,
		error_text          TEXT    NOT NULL DEFAULT ''
	)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate offsite_runs: %w", err)
	}
	if _, err := db.Exec(`CREATE INDEX IF NOT EXISTS idx_offsite_runs_client
		ON offsite_runs (client_id, started_at DESC)`); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate offsite_runs index: %w", err)
	}
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

const rfc3339 = time.RFC3339Nano

// CreateClient inserts a client and returns its assigned ID.
func (s *Store) CreateClient(ctx context.Context, c model.Client) (int64, error) {
	excludes, err := json.Marshal(c.Excludes)
	if err != nil {
		return 0, fmt.Errorf("marshal excludes: %w", err)
	}
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO clients (name, mode, source_label, excludes, retention_days,
			offsite_retention_days, expected_interval_secs, offsite_remote, offsite_dir,
			offsite_interval_secs, ssh_pubkey, token_hash, enabled, created_at, skip_symlinks)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.Name, string(c.Mode), c.SourceLabel, string(excludes), c.RetentionDays,
		c.OffsiteRetentionDays, c.ExpectedIntervalSecs, c.OffsiteRemote, c.OffsiteDir,
		c.OffsiteIntervalSecs, c.SSHPubKey, c.TokenHash, b2i(c.Enabled), time.Now().UTC().Format(rfc3339),
		b2i(c.SkipSymlinks))
	if err != nil {
		return 0, fmt.Errorf("insert client: %w", err)
	}
	return res.LastInsertId()
}

// ListClients returns all clients ordered by name.
func (s *Store) ListClients(ctx context.Context) ([]model.Client, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, mode, source_label, excludes, retention_days,
			offsite_retention_days, expected_interval_secs, offsite_remote, offsite_dir,
			offsite_interval_secs, ssh_pubkey, token_hash, enabled, created_at, version, skip_symlinks
		FROM clients ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Client
	for rows.Next() {
		c, err := scanClient(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GetClientBySlug returns the client whose name's slug matches, or (nil, nil)
// if none found. Fleet sizes are small so a linear scan over ListClients is fine.
func (s *Store) GetClientBySlug(ctx context.Context, slug string) (*model.Client, error) {
	clients, err := s.ListClients(ctx)
	if err != nil {
		return nil, err
	}
	for _, c := range clients {
		if model.Slug(c.Name) == slug {
			return &c, nil
		}
	}
	return nil, nil
}

// GetClient returns one client by ID, or (nil, nil) if not found.
func (s *Store) GetClient(ctx context.Context, id int64) (*model.Client, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, mode, source_label, excludes, retention_days,
			offsite_retention_days, expected_interval_secs, offsite_remote, offsite_dir,
			offsite_interval_secs, ssh_pubkey, token_hash, enabled, created_at, version, skip_symlinks
		FROM clients WHERE id = ?`, id)
	c, err := scanClient(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// RecordRun inserts a run, capping the log tail (DD4) before write.
func (s *Store) RecordRun(ctx context.Context, r model.Run) (int64, error) {
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO runs (client_id, started_at, finished_at, status, bytes, files, snapshot_id, log_tail)
		VALUES (?,?,?,?,?,?,?,?)`,
		r.ClientID, r.StartedAt.UTC().Format(rfc3339), r.FinishedAt.UTC().Format(rfc3339),
		string(r.Status), r.Bytes, r.Files, r.SnapshotID, model.CapLogTail(r.LogTail))
	if err != nil {
		return 0, fmt.Errorf("insert run: %w", err)
	}
	return res.LastInsertId()
}

// UpdateRun overwrites the mutable fields of an existing run identified by id.
// client_id must match to prevent cross-client tampering.
func (s *Store) UpdateRun(ctx context.Context, id, clientID int64, r model.Run) error {
	res, err := s.db.ExecContext(ctx, `
		UPDATE runs SET finished_at=?, status=?, bytes=?, files=?, snapshot_id=?, log_tail=?
		WHERE id=? AND client_id=?`,
		r.FinishedAt.UTC().Format(rfc3339), string(r.Status), r.Bytes, r.Files,
		r.SnapshotID, model.CapLogTail(r.LogTail), id, clientID)
	if err != nil {
		return fmt.Errorf("update run: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("run %d not found for client %d", id, clientID)
	}
	return nil
}

// LatestRun returns the most recent run for a client, or (nil, nil) if none.
// Backed by idx_runs_client_started so the dashboard avoids an N+1 scan.
func (s *Store) LatestRun(ctx context.Context, clientID int64) (*model.Run, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, client_id, started_at, finished_at, status, bytes, files, snapshot_id, log_tail
		FROM runs WHERE client_id = ? ORDER BY started_at DESC LIMIT 1`, clientID)
	r, err := scanRun(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// CreateUser adds a new user. Returns an error if the username is already taken.
func (s *Store) CreateUser(ctx context.Context, username, passwordHash string) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO users (username, password_hash, created_at) VALUES (?,?,?)`,
		username, passwordHash, time.Now().UTC().Format(rfc3339))
	if err != nil {
		return 0, fmt.Errorf("create user: %w", err)
	}
	return res.LastInsertId()
}

// UpsertUser creates the user or updates their password hash if they already exist.
func (s *Store) UpsertUser(ctx context.Context, username, passwordHash string) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO users (username, password_hash, created_at) VALUES (?,?,?)
		ON CONFLICT(username) DO UPDATE SET password_hash=excluded.password_hash`,
		username, passwordHash, time.Now().UTC().Format(rfc3339))
	if err != nil {
		return fmt.Errorf("upsert user: %w", err)
	}
	return nil
}

// ListUsers returns all users ordered by username.
func (s *Store) ListUsers(ctx context.Context) ([]model.User, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, username, password_hash, created_at FROM users ORDER BY username`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.User
	for rows.Next() {
		var u model.User
		var created string
		if err := rows.Scan(&u.ID, &u.Username, &u.PasswordHash, &created); err != nil {
			return nil, err
		}
		u.CreatedAt, _ = time.Parse(rfc3339, created)
		out = append(out, u)
	}
	return out, rows.Err()
}

// GetUserByUsername returns the user with the given username, or (nil, nil) if not found.
func (s *Store) GetUserByUsername(ctx context.Context, username string) (*model.User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, created_at FROM users WHERE username=?`, username)
	var u model.User
	var created string
	switch err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &created); err {
	case sql.ErrNoRows:
		return nil, nil
	case nil:
		u.CreatedAt, _ = time.Parse(rfc3339, created)
		return &u, nil
	default:
		return nil, err
	}
}

// DeleteUser removes a user by ID. Returns an error if not found.
func (s *Store) DeleteUser(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM users WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("delete user: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("user %d: not found", id)
	}
	return nil
}

// CountUsers returns the total number of users.
func (s *Store) CountUsers(ctx context.Context) (int, error) {
	row := s.db.QueryRowContext(ctx, `SELECT COUNT(*) FROM users`)
	var n int
	return n, row.Scan(&n)
}

// SetAdmin upserts the single admin row (id=1).
func (s *Store) SetAdmin(ctx context.Context, a model.Admin) error {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO admin (id, username, password_hash, created_at)
		VALUES (1, ?, ?, ?)
		ON CONFLICT(id) DO UPDATE SET username=excluded.username, password_hash=excluded.password_hash`,
		a.Username, a.PasswordHash, time.Now().UTC().Format(rfc3339))
	if err != nil {
		return fmt.Errorf("upsert admin: %w", err)
	}
	return nil
}

// GetAdmin returns the admin account, or (nil, nil) if none has been set.
func (s *Store) GetAdmin(ctx context.Context) (*model.Admin, error) {
	row := s.db.QueryRowContext(ctx, `SELECT username, password_hash, created_at FROM admin WHERE id=1`)
	var a model.Admin
	var created string
	switch err := row.Scan(&a.Username, &a.PasswordHash, &created); err {
	case sql.ErrNoRows:
		return nil, nil
	case nil:
		a.CreatedAt, _ = time.Parse(rfc3339, created)
		return &a, nil
	default:
		return nil, err
	}
}

// RecordOffsiteObject records that a snapshot was uploaded offsite (idempotent
// on client+snapshot+remote).
func (s *Store) RecordOffsiteObject(ctx context.Context, o model.OffsiteObject) error {
	uploaded := o.UploadedAt
	if uploaded.IsZero() {
		uploaded = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO offsite_objects (client_id, snapshot_id, remote, bytes, uploaded_at)
		VALUES (?,?,?,?,?)
		ON CONFLICT(client_id, snapshot_id, remote) DO UPDATE SET bytes=excluded.bytes, uploaded_at=excluded.uploaded_at`,
		o.ClientID, o.SnapshotID, o.Remote, o.Bytes, uploaded.UTC().Format(rfc3339))
	if err != nil {
		return fmt.Errorf("record offsite object: %w", err)
	}
	return nil
}

// ListOffsiteObjects returns the offsite objects for a client, newest first.
func (s *Store) ListOffsiteObjects(ctx context.Context, clientID int64) ([]model.OffsiteObject, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, client_id, snapshot_id, remote, bytes, uploaded_at
		FROM offsite_objects WHERE client_id = ? ORDER BY uploaded_at DESC`, clientID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.OffsiteObject
	for rows.Next() {
		var o model.OffsiteObject
		var uploaded string
		if err := rows.Scan(&o.ID, &o.ClientID, &o.SnapshotID, &o.Remote, &o.Bytes, &uploaded); err != nil {
			return nil, err
		}
		o.UploadedAt, _ = time.Parse(rfc3339, uploaded)
		out = append(out, o)
	}
	return out, rows.Err()
}

// DeleteOffsiteObject removes an offsite record (after the remote object is gone).
func (s *Store) DeleteOffsiteObject(ctx context.Context, clientID int64, snapshotID, remote string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM offsite_objects WHERE client_id=? AND snapshot_id=? AND remote=?`,
		clientID, snapshotID, remote)
	return err
}

// LatestOffsite returns the most recent offsite upload time for a client, or
// (nil, nil) if it has never been offsited. Drives the dashboard offsite state.
func (s *Store) LatestOffsite(ctx context.Context, clientID int64) (*time.Time, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT uploaded_at FROM offsite_objects WHERE client_id=? ORDER BY uploaded_at DESC LIMIT 1`, clientID)
	var uploaded string
	switch err := row.Scan(&uploaded); err {
	case sql.ErrNoRows:
		return nil, nil
	case nil:
		t, _ := time.Parse(rfc3339, uploaded)
		return &t, nil
	default:
		return nil, err
	}
}

// StartOffsiteRun inserts an offsite_runs row with status="running" so the
// dashboard can show upload progress. Call FinishOffsiteRun when done.
// Use only for adhoc (user-triggered) runs; scheduled runs use RecordOffsiteRun.
func (s *Store) StartOffsiteRun(ctx context.Context, clientID int64, triggeredBy string, startedAt time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO offsite_runs (client_id, triggered_by, started_at, status)
		 VALUES (?, ?, ?, 'running')`,
		clientID, triggeredBy, startedAt.UTC().Format(rfc3339))
	if err != nil {
		return 0, fmt.Errorf("start offsite run: %w", err)
	}
	return res.LastInsertId()
}

// FinishOffsiteRun sets the final state of a run started by StartOffsiteRun.
func (s *Store) FinishOffsiteRun(ctx context.Context, id int64, status string, snaps int, bytes int64, errText string, finishedAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE offsite_runs
		 SET status=?, snapshots_uploaded=?, bytes_uploaded=?, error_text=?, finished_at=?
		 WHERE id=?`,
		status, snaps, bytes, errText, finishedAt.UTC().Format(rfc3339), id)
	return err
}

// MarkStaleOffsiteRunsFailed marks any rows still in "running" status as
// failed. Call once at server startup to clean up rows left by a crash or
// restart that interrupted an upload.
func (s *Store) MarkStaleOffsiteRunsFailed(ctx context.Context) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE offsite_runs SET status='failed', error_text='interrupted by server restart'
		 WHERE status='running'`)
	return err
}

// RunningOffsiteClientIDs returns the set of client IDs that currently have
// an offsite run with status='running', for dashboard progress indicators.
func (s *Store) RunningOffsiteClientIDs(ctx context.Context) (map[int64]bool, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT DISTINCT client_id FROM offsite_runs WHERE status='running'`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[int64]bool{}
	for rows.Next() {
		var id int64
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out[id] = true
	}
	return out, rows.Err()
}

// RecordOffsiteRun inserts a completed offsite session record in a single
// INSERT. Use for scheduled runs where no intermediate "running" state is needed.
func (s *Store) RecordOffsiteRun(ctx context.Context, r model.OffsiteRun) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO offsite_runs
		 (client_id, triggered_by, started_at, finished_at, status, snapshots_uploaded, bytes_uploaded, error_text)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		r.ClientID, r.TriggeredBy,
		r.StartedAt.UTC().Format(rfc3339), r.FinishedAt.UTC().Format(rfc3339),
		r.Status, r.SnapshotsUploaded, r.BytesUploaded, r.ErrorText)
	return err
}

// ListOffsiteRuns returns up to limit offsite run records for a client, newest first.
func (s *Store) ListOffsiteRuns(ctx context.Context, clientID int64, limit int) ([]model.OffsiteRun, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, client_id, triggered_by, started_at, finished_at, status,
		        snapshots_uploaded, bytes_uploaded, error_text
		 FROM offsite_runs WHERE client_id=? ORDER BY started_at DESC LIMIT ?`,
		clientID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.OffsiteRun
	for rows.Next() {
		var r model.OffsiteRun
		var startedStr, finishedStr string
		if err := rows.Scan(&r.ID, &r.ClientID, &r.TriggeredBy,
			&startedStr, &finishedStr, &r.Status,
			&r.SnapshotsUploaded, &r.BytesUploaded, &r.ErrorText); err != nil {
			return nil, err
		}
		r.StartedAt, _ = time.Parse(rfc3339, startedStr)
		r.FinishedAt, _ = time.Parse(rfc3339, finishedStr)
		out = append(out, r)
	}
	return out, rows.Err()
}

// ── Remotes ──────────────────────────────────────────────────────────────────

// CreateRemote inserts a new remote into the remotes table.
func (s *Store) CreateRemote(ctx context.Context, r model.Remote) error {
	cfg, err := json.Marshal(r.Config)
	if err != nil {
		return fmt.Errorf("marshal remote config: %w", err)
	}
	_, err = s.db.ExecContext(ctx,
		`INSERT INTO remotes (name, backend, config, created_at) VALUES (?, ?, ?, ?)`,
		r.Name, string(r.Backend), string(cfg), r.CreatedAt.UTC().Format(rfc3339))
	return err
}

// ListRemotes returns all remotes ordered by name.
func (s *Store) ListRemotes(ctx context.Context) ([]model.Remote, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, backend, config, created_at FROM remotes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Remote
	for rows.Next() {
		var r model.Remote
		var backend, cfgJSON, createdStr string
		if err := rows.Scan(&r.ID, &r.Name, &backend, &cfgJSON, &createdStr); err != nil {
			return nil, err
		}
		r.Backend = model.RemoteBackend(backend)
		r.CreatedAt, _ = time.Parse(rfc3339, createdStr)
		if err := json.Unmarshal([]byte(cfgJSON), &r.Config); err != nil {
			r.Config = map[string]string{}
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRemoteByName returns the remote with the given name, or nil if not found.
func (s *Store) GetRemoteByName(ctx context.Context, name string) (*model.Remote, error) {
	var r model.Remote
	var backend, cfgJSON, createdStr string
	err := s.db.QueryRowContext(ctx,
		`SELECT id, name, backend, config, created_at FROM remotes WHERE name=?`, name).
		Scan(&r.ID, &r.Name, &backend, &cfgJSON, &createdStr)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	r.Backend = model.RemoteBackend(backend)
	r.CreatedAt, _ = time.Parse(rfc3339, createdStr)
	if err := json.Unmarshal([]byte(cfgJSON), &r.Config); err != nil {
		r.Config = map[string]string{}
	}
	return &r, nil
}

// DeleteRemote removes a remote by name.
func (s *Store) DeleteRemote(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM remotes WHERE name=?`, name)
	return err
}

// UpdateClientOffsite changes the offsite_remote, offsite_dir, and offsite_interval_secs for a client.
// An empty remote disables offsite tiering for this client.
func (s *Store) UpdateClientOffsite(ctx context.Context, id int64, remote, dir string, intervalSecs int) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE clients SET offsite_remote=?, offsite_dir=?, offsite_interval_secs=? WHERE id=?`, remote, dir, intervalSecs, id)
	if err != nil {
		return fmt.Errorf("update offsite remote: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("client %d: not found", id)
	}
	return nil
}

// ErrConflict is returned by RotateClientCreds when a concurrent rotation
// has already incremented the client's version since the caller read it.
var ErrConflict = fmt.Errorf("concurrent modification: version mismatch")

// RotateClientCreds replaces a client's SSH public key and token hash atomically.
// oldVersion is the version observed by the caller via GetClient; the UPDATE only
// applies if the version has not changed (compare-and-swap). Returns ErrConflict
// if a concurrent rotation already incremented the version.
func (s *Store) RotateClientCreds(ctx context.Context, id int64, pubKey, tokenHash string, oldVersion int) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE clients SET ssh_pubkey=?, token_hash=?, version=version+1 WHERE id=? AND version=?`,
		pubKey, tokenHash, id, oldVersion)
	if err != nil {
		return fmt.Errorf("rotate client creds: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		// Either the client was deleted or a concurrent rotation already ran.
		// Disambiguate by checking existence.
		row := s.db.QueryRowContext(ctx, `SELECT version FROM clients WHERE id=?`, id)
		var v int
		switch err := row.Scan(&v); err {
		case sql.ErrNoRows:
			return fmt.Errorf("client %d: not found", id)
		case nil:
			return ErrConflict
		default:
			return fmt.Errorf("rotate client creds disambiguate: %w", err)
		}
	}
	return nil
}

// DeleteClient removes a client and all its run history (cascaded by FK).
// Returns an error if the client does not exist.
func (s *Store) DeleteClient(ctx context.Context, id int64) error {
	res, err := s.db.ExecContext(ctx, `DELETE FROM clients WHERE id=?`, id)
	if err != nil {
		return fmt.Errorf("delete client: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("client %d: not found", id)
	}
	return nil
}

// ListRuns returns up to limit runs for a client, newest first.
func (s *Store) ListRuns(ctx context.Context, clientID int64, limit int) ([]model.Run, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, client_id, started_at, finished_at, status, bytes, files, snapshot_id, log_tail
		FROM runs WHERE client_id = ? ORDER BY started_at DESC LIMIT ?`, clientID, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Run
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// GetRun returns one run by ID, or (nil, nil) if not found.
func (s *Store) GetRun(ctx context.Context, id int64) (*model.Run, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, client_id, started_at, finished_at, status, bytes, files, snapshot_id, log_tail
		FROM runs WHERE id = ?`, id)
	r, err := scanRun(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// PruneRunLogs clears log_tail from runs finished before olderThan, preserving
// all other run metadata (status, bytes, files, timestamps).
func (s *Store) PruneRunLogs(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET log_tail = '' WHERE log_tail != '' AND finished_at < ?`,
		olderThan.UTC().Format(rfc3339))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// PruneRuns deletes run records older than keepDays for a client (0 = keep all).
func (s *Store) PruneRuns(ctx context.Context, clientID int64, keepDays int) (int64, error) {
	if keepDays <= 0 {
		return 0, nil
	}
	cutoff := time.Now().UTC().Add(-time.Duration(keepDays) * 24 * time.Hour).Format(rfc3339)
	res, err := s.db.ExecContext(ctx,
		`DELETE FROM runs WHERE client_id=? AND started_at < ?`, clientID, cutoff)
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

func b2i(b bool) int {
	if b {
		return 1
	}
	return 0
}

type scanner interface{ Scan(...any) error }

func scanClient(sc scanner) (model.Client, error) {
	var c model.Client
	var mode, excludes, createdAt string
	var enabled, skipSymlinks int
	if err := sc.Scan(&c.ID, &c.Name, &mode, &c.SourceLabel, &excludes, &c.RetentionDays,
		&c.OffsiteRetentionDays, &c.ExpectedIntervalSecs, &c.OffsiteRemote, &c.OffsiteDir,
		&c.OffsiteIntervalSecs, &c.SSHPubKey, &c.TokenHash, &enabled, &createdAt, &c.Version, &skipSymlinks); err != nil {
		return c, err
	}
	c.Mode = model.Mode(mode)
	c.Enabled = enabled != 0
	c.SkipSymlinks = skipSymlinks != 0
	if excludes != "" {
		_ = json.Unmarshal([]byte(excludes), &c.Excludes)
	}
	c.CreatedAt, _ = time.Parse(rfc3339, createdAt)
	return c, nil
}

func scanRun(sc scanner) (model.Run, error) {
	var r model.Run
	var status, started, finished string
	if err := sc.Scan(&r.ID, &r.ClientID, &started, &finished, &status,
		&r.Bytes, &r.Files, &r.SnapshotID, &r.LogTail); err != nil {
		return r, err
	}
	r.Status = model.RunStatus(status)
	r.StartedAt, _ = time.Parse(rfc3339, started)
	r.FinishedAt, _ = time.Parse(rfc3339, finished)
	return r, nil
}
