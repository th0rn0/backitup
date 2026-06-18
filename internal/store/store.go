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
	"fmt"
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
			offsite_retention_days, expected_interval_secs, offsite_remote,
			ssh_pubkey, token_hash, enabled, created_at)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.Name, string(c.Mode), c.SourceLabel, string(excludes), c.RetentionDays,
		c.OffsiteRetentionDays, c.ExpectedIntervalSecs, c.OffsiteRemote,
		c.SSHPubKey, c.TokenHash, b2i(c.Enabled), time.Now().UTC().Format(rfc3339))
	if err != nil {
		return 0, fmt.Errorf("insert client: %w", err)
	}
	return res.LastInsertId()
}

// ListClients returns all clients ordered by name.
func (s *Store) ListClients(ctx context.Context) ([]model.Client, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, mode, source_label, excludes, retention_days,
			offsite_retention_days, expected_interval_secs, offsite_remote,
			ssh_pubkey, token_hash, enabled, created_at
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

// GetClient returns one client by ID, or (nil, nil) if not found.
func (s *Store) GetClient(ctx context.Context, id int64) (*model.Client, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, mode, source_label, excludes, retention_days,
			offsite_retention_days, expected_interval_secs, offsite_remote,
			ssh_pubkey, token_hash, enabled, created_at
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
	var enabled int
	if err := sc.Scan(&c.ID, &c.Name, &mode, &c.SourceLabel, &excludes, &c.RetentionDays,
		&c.OffsiteRetentionDays, &c.ExpectedIntervalSecs, &c.OffsiteRemote,
		&c.SSHPubKey, &c.TokenHash, &enabled, &createdAt); err != nil {
		return c, err
	}
	c.Mode = model.Mode(mode)
	c.Enabled = enabled != 0
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
