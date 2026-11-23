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
	"log"
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

// Open opens (or creates) the SQLite database at dsn and brings it to the
// canonical schema. dsn is a file path; busy_timeout keeps brief writer
// contention from erroring.
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
	// WAL is a property of the database file, and it cannot be switched on from
	// inside a transaction, so it is set here rather than in schema.sql.
	if _, err := db.Exec(`PRAGMA journal_mode = WAL`); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable WAL: %w", err)
	}
	if err := migrate(context.Background(), db); err != nil {
		db.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	// The migration committed, so the database is usable. Report any table an
	// interrupted migration left behind (rows the application never reads) before
	// handing the handle out. This runs outside every step transaction, reads only,
	// and cannot fail the open: see warnStrandedMigrationTables.
	warnStrandedMigrationTables(context.Background(), db)
	return &Store{db: db}, nil
}

func (s *Store) Close() error { return s.db.Close() }

const rfc3339 = time.RFC3339Nano

// parseStoredTime parses one of our timestamp columns. An empty value means
// "never" (e.g. remote_verified_at) and is not an error. A malformed value IS
// an error: these columns are written only by this package, in rfc3339, so
// anything else is corruption.
//
// Callers must not paper over the error with a zero time on a path that makes a
// destructive decision. time.Time{} is year 1, which is "older than every
// retention horizon", so a corrupt timestamp used to make a recent remote object
// look ancient and eligible for deletion. Where a zero time is harmless
// (display, age labels) use displayTime instead, which logs and degrades.
func parseStoredTime(field, value string) (time.Time, error) {
	if value == "" {
		return time.Time{}, nil
	}
	t, err := time.Parse(rfc3339, value)
	if err != nil {
		return time.Time{}, fmt.Errorf("corrupt %s %q: %w", field, value, err)
	}
	return t, nil
}

// displayTime is parseStoredTime for columns that only feed a label or a sort
// hint. A corrupt value degrades that one row's timestamp to zero and is logged
// loudly; it drives no deletion, so failing closed would only cost the operator
// the page.
func displayTime(field, value string) time.Time {
	t, err := parseStoredTime(field, value)
	if err != nil {
		log.Printf("store: %v — showing the zero time", err)
	}
	return t
}

// ErrEmptySlug is returned by CreateClient when the client name contains no
// character that survives slugging ("..", ".", "/", "...."). Such a name has no
// valid directory component of its own, and joining "" onto the backup base dir
// would hand the client the directory every other client lives in.
var ErrEmptySlug = errors.New(`client name contains no letters or digits, so it has no storage slug`)

// SlugConflictError reports a create that would have reused another client's
// slug — and therefore its read-write backup directory.
type SlugConflictError struct {
	Slug     string // the computed slug
	Existing string // Name of the client that already owns it
}

func (e *SlugConflictError) Error() string {
	if e.Existing == "" {
		return fmt.Sprintf("slug %q is already taken by another client", e.Slug)
	}
	return fmt.Sprintf("slug %q is already taken by client %q", e.Slug, e.Existing)
}

// warnf is the logging sink for migration warnings. It is a variable so tests
// can capture exactly what an operator sees on startup.
var warnf = log.Printf

// slugOwner identifies the client that already holds a storage slug.
type slugOwner struct {
	id   int64
	name string
}

// slugWithSuffix returns base plus a "-<n>" disambiguator, keeping the whole
// value within model.MaxSlugLen by trimming the stem (never the suffix) so the
// result still satisfies the authkeys allowlist.
func slugWithSuffix(base string, n int) string {
	suffix := fmt.Sprintf("-%d", n)
	stem := base
	if max := model.MaxSlugLen - len(suffix); len(stem) > max {
		stem = strings.TrimRight(stem[:max], "-")
	}
	return stem + suffix
}

// backfillClientSlugs fills in slug for clients created before the column
// existed. It runs inside the migration transaction (so the derived slugs and the
// unique index that then enforces them commit together).
//
// Rows are visited oldest-first so the earliest client keeps the plain slug and
// later collisions are disambiguated ("dup-a", "dup-a-2", ...); a name with no
// slug at all falls back to "client-<id>". The result contains no empty and no
// duplicate slugs, which is what lets the unique index be created.
//
// A collision is NOT repaired on disk, and must not be repaired silently.
// Before this column existed the directory was derived from the name, so two
// colliding clients genuinely shared one directory — and the snapshots in it
// cannot be attributed to either of them (one ssh user, timestamp-only snapshot
// ids, no owner recorded). Moving or deleting them automatically would guess.
// Giving the loser a new slug therefore only separates future writes: the
// winner keeps read-write access to the shared directory (and prunes it under
// its own retention policy), while the loser's history there becomes
// unreachable. Each such collision is reported to the operator so the shared
// directory can be quarantined by hand.
func backfillClientSlugs(ctx context.Context, db querier) error {
	taken := map[string]slugOwner{}
	rows, err := db.QueryContext(ctx, `SELECT id, name, slug FROM clients WHERE slug != ''`)
	if err != nil {
		return err
	}
	for rows.Next() {
		var o slugOwner
		var slug string
		if err := rows.Scan(&o.id, &o.name, &slug); err != nil {
			rows.Close()
			return err
		}
		taken[slug] = o
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	rows, err = db.QueryContext(ctx, `SELECT id, name FROM clients WHERE slug = '' ORDER BY id`)
	if err != nil {
		return err
	}
	var todo []slugOwner
	for rows.Next() {
		var p slugOwner
		if err := rows.Scan(&p.id, &p.name); err != nil {
			rows.Close()
			return err
		}
		todo = append(todo, p)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, p := range todo {
		base := model.Slug(p.name)
		if base == "" {
			base = fmt.Sprintf("client-%d", p.id)
		}
		slug := base
		winner, collided := taken[base]
		if collided {
			for n := 2; ; n++ {
				candidate := slugWithSuffix(base, n)
				if _, exists := taken[candidate]; !exists {
					slug = candidate
					break
				}
			}
		}
		taken[slug] = p
		if _, err := db.ExecContext(ctx, `UPDATE clients SET slug=? WHERE id=?`, slug, p.id); err != nil {
			return err
		}
		if collided {
			warnLegacySlugCollision(winner, p, base, slug)
		}
	}
	return nil
}

// warnLegacySlugCollision reports one legacy collision: both clients, the slug
// they shared and therefore the directory they still share, and what an operator
// has to do about it. It lives in one log line so it is greppable in startup
// output ("WARNING: legacy client slug collision").
func warnLegacySlugCollision(winner, loser slugOwner, sharedSlug, newSlug string) {
	// The store does not know BACKITUP_BACKUP_DIR (it is a server setting), so
	// the directory is named relative to it rather than guessed.
	dir := "<backup dir>/" + sharedSlug
	warnf("WARNING: legacy client slug collision: clients %d (%q) and %d (%q) both mapped to storage "+
		"slug %q. Client %d (%q) was migrated to slug %q, but every snapshot either client recorded is "+
		"still in %s (under BACKUP_BACKUP_DIR): one ssh user and timestamp-only snapshot ids, so the "+
		"files cannot be attributed to a client and were left untouched. This migration is "+
		"forward-only: client %d (%q) keeps read-write access to that directory and will prune it "+
		"under its own retention policy, client %d (%q)'s history there can no longer be listed, "+
		"pruned or uploaded, and its offsite prefix moved from %q to %q, so its existing remote "+
		"objects will be reported remote_missing. Quarantine it by hand: rename %s aside (e.g. to "+
		"%s.quarantine) so %q starts empty, then re-upload client %d (%q)'s data.",
		winner.id, winner.name, loser.id, loser.name, sharedSlug,
		loser.id, loser.name, newSlug,
		dir,
		winner.id, winner.name,
		loser.id, loser.name,
		sharedSlug, newSlug,
		dir, sharedSlug, sharedSlug,
		loser.id, loser.name)
}

// slugForNewClient computes the storage slug for a new client and refuses an
// empty or already-taken one. This is the single place a slug is ever derived
// from a name: callers must use the persisted value, never model.Slug(name),
// to build a directory path.
func (s *Store) slugForNewClient(ctx context.Context, name string) (string, error) {
	slug := model.Slug(name)
	if slug == "" {
		return "", ErrEmptySlug
	}
	// Pre-check so the normal path can name the client that owns the slug; the
	// UNIQUE index is the race-safe backstop.
	var existing string
	err := s.db.QueryRowContext(ctx, `SELECT name FROM clients WHERE slug=?`, slug).Scan(&existing)
	switch {
	case err == nil:
		return "", &SlugConflictError{Slug: slug, Existing: existing}
	case errors.Is(err, sql.ErrNoRows):
		return slug, nil
	default:
		return "", fmt.Errorf("check client slug: %w", err)
	}
}

// CreateClient inserts a client and returns its assigned ID. The client's slug is
// derived here, once, and persisted; ErrEmptySlug / *SlugConflictError are
// returned when the name cannot serve as a unique storage directory.
func (s *Store) CreateClient(ctx context.Context, c model.Client) (int64, error) {
	excludes, err := json.Marshal(c.Excludes)
	if err != nil {
		return 0, fmt.Errorf("marshal excludes: %w", err)
	}
	if c.OffsiteUploadMode == "" {
		c.OffsiteUploadMode = "all"
	}
	slug, err := s.slugForNewClient(ctx, c.Name)
	if err != nil {
		return 0, err
	}
	c.Slug = slug
	res, err := s.db.ExecContext(ctx, `
		INSERT INTO clients (name, slug, mode, source_label, excludes, retention_days,
			expected_interval_secs, offsite_remote, offsite_dir,
			offsite_cron, ssh_pubkey, token_hash, token_prefix, enabled, created_at, skip_symlinks,
			offsite_upload_mode)
		VALUES (?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		c.Name, c.Slug, string(c.Mode), c.SourceLabel, string(excludes), c.RetentionDays,
		c.ExpectedIntervalSecs, c.OffsiteRemote, c.OffsiteDir,
		c.OffsiteCron, c.SSHPubKey, c.TokenHash, c.TokenPrefix, b2i(c.Enabled), time.Now().UTC().Format(rfc3339),
		b2i(c.SkipSymlinks), c.OffsiteUploadMode)
	if err != nil {
		// A concurrent create can slip past the pre-check; the unique index is
		// what actually guarantees one client per slug.
		if strings.Contains(err.Error(), "clients.slug") {
			var existing string
			_ = s.db.QueryRowContext(ctx, `SELECT name FROM clients WHERE slug=?`, c.Slug).Scan(&existing)
			return 0, &SlugConflictError{Slug: c.Slug, Existing: existing}
		}
		return 0, fmt.Errorf("insert client: %w", err)
	}
	return res.LastInsertId()
}

// ListClients returns all clients ordered by name.
func (s *Store) ListClients(ctx context.Context) ([]model.Client, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, name, slug, mode, source_label, excludes, retention_days,
			expected_interval_secs, offsite_remote, offsite_dir,
			offsite_cron, ssh_pubkey, token_hash, token_prefix, enabled, created_at, version, skip_symlinks,
			offsite_upload_mode
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

// GetClientBySlug returns the client whose stored slug matches, or (nil, nil) if
// none. The slug is the persisted identity, not model.Slug(name): the two can
// differ (disambiguated migration rows) and must never be conflated.
func (s *Store) GetClientBySlug(ctx context.Context, slug string) (*model.Client, error) {
	if slug == "" {
		return nil, nil
	}
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, slug, mode, source_label, excludes, retention_days,
			expected_interval_secs, offsite_remote, offsite_dir,
			offsite_cron, ssh_pubkey, token_hash, token_prefix, enabled, created_at, version, skip_symlinks,
			offsite_upload_mode
		FROM clients WHERE slug = ?`, slug)
	c, err := scanClient(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &c, nil
}

// GetClient returns one client by ID, or (nil, nil) if not found.
func (s *Store) GetClient(ctx context.Context, id int64) (*model.Client, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, name, slug, mode, source_label, excludes, retention_days,
			expected_interval_secs, offsite_remote, offsite_dir,
			offsite_cron, ssh_pubkey, token_hash, token_prefix, enabled, created_at, version, skip_symlinks,
			offsite_upload_mode
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

// LatestRunAllClients returns the most recent run for every client in a single
// query, keyed by client ID. Clients with no runs are absent from the map.
func (s *Store) LatestRunAllClients(ctx context.Context) (map[int64]*model.Run, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.client_id, r.started_at, r.finished_at, r.status, r.bytes, r.files, r.snapshot_id, r.log_tail
		FROM runs r
		INNER JOIN (
			SELECT client_id, MAX(started_at) AS started_at FROM runs GROUP BY client_id
		) latest ON r.client_id = latest.client_id AND r.started_at = latest.started_at`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[int64]*model.Run)
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		rc := r
		m[r.ClientID] = &rc
	}
	return m, rows.Err()
}

// LastSuccessfulRun returns the most recent run that actually SUCCEEDED for a
// client, or (nil, nil) if none ever has.
//
// Distinct from LatestRun on purpose. An "overlap" row (cron fired while the
// previous backup was still running) or a "failed" row is the newest row but not
// a backup, so health is aged from this one instead (S8). Ordered by finished_at
// because that is the timestamp staleness is measured against.
func (s *Store) LastSuccessfulRun(ctx context.Context, clientID int64) (*model.Run, error) {
	row := s.db.QueryRowContext(ctx, `
		SELECT id, client_id, started_at, finished_at, status, bytes, files, snapshot_id, log_tail
		FROM runs WHERE client_id = ? AND status = ?
		ORDER BY finished_at DESC LIMIT 1`, clientID, string(model.StatusOK))
	r, err := scanRun(row)
	if err == sql.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// LastSuccessfulRunAllClients returns each client's most recent successful run
// in a single query, keyed by client ID. Clients with no successful run are
// absent from the map.
func (s *Store) LastSuccessfulRunAllClients(ctx context.Context) (map[int64]*model.Run, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT r.id, r.client_id, r.started_at, r.finished_at, r.status, r.bytes, r.files, r.snapshot_id, r.log_tail
		FROM runs r
		INNER JOIN (
			SELECT client_id, MAX(finished_at) AS finished_at FROM runs
			WHERE status = ? GROUP BY client_id
		) ok ON r.client_id = ok.client_id AND r.finished_at = ok.finished_at`,
		string(model.StatusOK))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[int64]*model.Run)
	for rows.Next() {
		r, err := scanRun(rows)
		if err != nil {
			return nil, err
		}
		rc := r
		m[r.ClientID] = &rc
	}
	return m, rows.Err()
}

// LatestOffsiteAllClients returns the most recent offsite upload time for every
// client in a single query, keyed by client ID.
func (s *Store) LatestOffsiteAllClients(ctx context.Context) (map[int64]*time.Time, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT client_id, MAX(uploaded_at) FROM offsite_objects GROUP BY client_id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	m := make(map[int64]*time.Time)
	for rows.Next() {
		var clientID int64
		var uploaded string
		if err := rows.Scan(&clientID, &uploaded); err != nil {
			return nil, err
		}
		t := displayTime("offsite_objects.uploaded_at", uploaded)
		tc := t
		m[clientID] = &tc
	}
	return m, rows.Err()
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
		u.CreatedAt = displayTime("users.created_at", created)
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
		u.CreatedAt = displayTime("users.created_at", created)
		return &u, nil
	default:
		return nil, err
	}
}

// GetUserByID returns the user with the given ID, or (nil, nil) if not found.
// requireAdmin uses it to re-validate a session against the live account table.
func (s *Store) GetUserByID(ctx context.Context, id int64) (*model.User, error) {
	row := s.db.QueryRowContext(ctx,
		`SELECT id, username, password_hash, created_at FROM users WHERE id=?`, id)
	var u model.User
	var created string
	switch err := row.Scan(&u.ID, &u.Username, &u.PasswordHash, &created); err {
	case sql.ErrNoRows:
		return nil, nil
	case nil:
		u.CreatedAt = displayTime("users.created_at", created)
		return &u, nil
	default:
		return nil, err
	}
}

// DeleteUser removes a user by ID. Returns an error if not found.
// Callers must also invalidate that user's sessions (server.postDeleteUser).
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
		a.CreatedAt = displayTime("admin.created_at", created)
		return &a, nil
	default:
		return nil, err
	}
}

// RecordOffsiteObject records that a snapshot was uploaded offsite (idempotent
// on client+snapshot+remote). A deduplicated upload sets DedupOf to the snapshot
// id that owns the remote object; such a row carries Bytes=0 because nothing was
// transferred and the content is already counted against the owner.
//
// The verification columns are cleared, always: this row is being written
// because the content was just put on the remote (or found there), so a
// remote_missing flag from an earlier pass describes an object that no longer
// describes this row. Leaving it set would make a re-uploaded snapshot
// permanently un-offsited — the hot copy would never be pruned and every pass
// would upload it again.
func (s *Store) RecordOffsiteObject(ctx context.Context, o model.OffsiteObject) error {
	uploaded := o.UploadedAt
	if uploaded.IsZero() {
		uploaded = time.Now().UTC()
	}
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO offsite_objects (client_id, snapshot_id, remote, bytes, uploaded_at, checksum, dedup_of)
		VALUES (?,?,?,?,?,?,?)
		ON CONFLICT(client_id, snapshot_id, remote) DO UPDATE SET bytes=excluded.bytes, uploaded_at=excluded.uploaded_at, checksum=excluded.checksum, dedup_of=excluded.dedup_of, remote_missing=0, remote_verified_at=''`,
		o.ClientID, o.SnapshotID, o.Remote, o.Bytes, uploaded.UTC().Format(rfc3339), o.Checksum, o.DedupOf)
	if err != nil {
		return fmt.Errorf("record offsite object: %w", err)
	}
	return nil
}

// ListOffsiteObjects returns the offsite objects for a client, newest first.
//
// A row whose uploaded_at cannot be parsed is skipped with a loud log rather
// than returned with a zero time: offsite pruning deletes anything older than
// the horizon, and year 1 would make a recent object look ancient. Skipping can
// only ever protect data — the row cannot be deleted, and the snapshot it names
// stays un-offsited, so the hot copy is kept too.
func (s *Store) ListOffsiteObjects(ctx context.Context, clientID int64) ([]model.OffsiteObject, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, client_id, snapshot_id, remote, bytes, uploaded_at, remote_missing, remote_verified_at, checksum, dedup_of
		FROM offsite_objects WHERE client_id = ? ORDER BY uploaded_at DESC`, clientID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.OffsiteObject
	for rows.Next() {
		var o model.OffsiteObject
		var uploaded, verifiedAt string
		var remoteMissing int
		if err := rows.Scan(&o.ID, &o.ClientID, &o.SnapshotID, &o.Remote, &o.Bytes, &uploaded, &remoteMissing, &verifiedAt, &o.Checksum, &o.DedupOf); err != nil {
			return nil, err
		}
		uploadedAt, err := parseStoredTime("offsite_objects.uploaded_at", uploaded)
		if err != nil {
			log.Printf("store: client %d: offsite object %s (%s) is unreadable, keeping it and leaving it un-prunable: %v",
				clientID, o.SnapshotID, o.Remote, err)
			continue
		}
		o.UploadedAt = uploadedAt
		o.RemoteMissing = remoteMissing != 0
		// Only feeds the hourly verification throttle: a zero value there forces
		// a re-check, which is the safe direction.
		o.RemoteVerifiedAt = displayTime("offsite_objects.remote_verified_at", verifiedAt)
		out = append(out, o)
	}
	return out, rows.Err()
}

// OffsiteChecksumOwner returns the snapshot id whose real remote object already
// holds content with this checksum, if any. Dedup rows are ignored: they point
// at an object owned by another snapshot, so chaining a second dedup onto them
// would record an owner that owns nothing. An object the lifecycle has reported
// missing is not an owner either — the content is not there, so the new snapshot
// must be uploaded rather than deduplicated into a hole.
func (s *Store) OffsiteChecksumOwner(ctx context.Context, clientID int64, remote, checksum string) (string, bool, error) {
	if checksum == "" {
		return "", false, nil
	}
	var owner string
	err := s.db.QueryRowContext(ctx,
		`SELECT snapshot_id FROM offsite_objects
		 WHERE client_id=? AND remote=? AND checksum=? AND dedup_of='' AND remote_missing=0
		 ORDER BY uploaded_at DESC LIMIT 1`,
		clientID, remote, checksum).Scan(&owner)
	if errors.Is(err, sql.ErrNoRows) {
		return "", false, nil
	}
	if err != nil {
		return "", false, err
	}
	return owner, true, nil
}

// TotalOffsiteBytes returns the sum of all recorded offsite object sizes across
// all clients and remotes. Dedup rows store 0 bytes — their content is already
// counted against the object they point at — so they neither double-count nor
// hide the real total.
func (s *Store) TotalOffsiteBytes(ctx context.Context) (int64, error) {
	var n int64
	err := s.db.QueryRowContext(ctx, `SELECT COALESCE(SUM(bytes), 0) FROM offsite_objects`).Scan(&n)
	return n, err
}

// UpdateOffsiteRemoteStatus records the result of a remote existence check.
func (s *Store) UpdateOffsiteRemoteStatus(ctx context.Context, id int64, missing bool, verifiedAt time.Time) error {
	m := 0
	if missing {
		m = 1
	}
	_, err := s.db.ExecContext(ctx,
		`UPDATE offsite_objects SET remote_missing=?, remote_verified_at=? WHERE id=?`,
		m, verifiedAt.UTC().Format(rfc3339), id)
	return err
}

// DeleteOffsiteObject removes an offsite record (after the remote object is gone).
func (s *Store) DeleteOffsiteObject(ctx context.Context, clientID int64, snapshotID, remote string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM offsite_objects WHERE client_id=? AND snapshot_id=? AND remote=?`,
		clientID, snapshotID, remote)
	return err
}

// DeleteDedupRecords removes the dedup records whose content lived in the given
// owner's remote object. They must go when that object does: left behind they
// would claim a remote copy of content that no longer exists anywhere.
func (s *Store) DeleteDedupRecords(ctx context.Context, clientID int64, ownerSnapshotID, remote string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM offsite_objects WHERE client_id=? AND remote=? AND dedup_of=?`,
		clientID, remote, ownerSnapshotID)
	return err
}

// LatestOffsite returns the most recent offsite upload time for a client, or
// (nil, nil) if it has never been offsited. Drives the dashboard offsite state
// and the per-client upload schedule.
//
// A row whose uploaded_at cannot be parsed is logged and skipped, and the
// newest readable row is returned instead — the same policy as
// ListOffsiteObjects. Returning the error here would be fail-open in the wrong
// direction: it reaches offsiteDue, which RunOffsiteOnce logs and skips, so one
// undateable row would stop that client's offsite uploads *silently*. Skipping
// moves the last-upload time backwards at worst, which can only make the client
// due sooner; the dedupe path makes the extra upload cheap.
func (s *Store) LatestOffsite(ctx context.Context, clientID int64) (*time.Time, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT uploaded_at FROM offsite_objects WHERE client_id=? ORDER BY uploaded_at DESC`, clientID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var uploaded string
		if err := rows.Scan(&uploaded); err != nil {
			return nil, err
		}
		t, err := parseStoredTime("offsite_objects.uploaded_at", uploaded)
		if err != nil {
			log.Printf("store: client %d: %v — ignoring that row when deciding whether an upload is due", clientID, err)
			continue
		}
		return &t, nil
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Nothing readable: treat as never offsited, which is "due now".
	return nil, nil
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

// MarkClientRunningRunFailed marks any run currently in "running" state for the
// given client as failed. Call when the client starts a new backup (via getConfig)
// so a prior run that never received a final status update is cleaned up.
func (s *Store) MarkClientRunningRunFailed(ctx context.Context, clientID int64) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status='failed', finished_at=?, log_tail='interrupted: client started a new backup without completing the previous run'
		 WHERE client_id=? AND status='running'`,
		time.Now().UTC().Format(rfc3339), clientID)
	return err
}

// MarkStaleRunsFailed marks any run still in "running" state with a started_at
// older than olderThan as failed. Used by the lifecycle worker as a safety net
// for clients that crash and never reconnect.
func (s *Store) MarkStaleRunsFailed(ctx context.Context, olderThan time.Time) (int64, error) {
	res, err := s.db.ExecContext(ctx,
		`UPDATE runs SET status='failed', finished_at=?, log_tail='interrupted: run exceeded maximum allowed duration'
		 WHERE status='running' AND started_at < ?`,
		time.Now().UTC().Format(rfc3339), olderThan.UTC().Format(rfc3339))
	if err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
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
		r.StartedAt = displayTime("offsite_runs.started_at", startedStr)
		r.FinishedAt = displayTime("offsite_runs.finished_at", finishedStr)
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
		`INSERT INTO remotes (name, backend, config, created_at, unsupported) VALUES (?, ?, ?, ?, ?)`,
		r.Name, string(r.Backend), string(cfg), r.CreatedAt.UTC().Format(rfc3339), b2i(r.Unsupported))
	return err
}

// ListRemotes returns all remotes ordered by name.
func (s *Store) ListRemotes(ctx context.Context) ([]model.Remote, error) {
	rows, err := s.db.QueryContext(ctx,
		`SELECT id, name, backend, config, created_at, unsupported FROM remotes ORDER BY name`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []model.Remote
	for rows.Next() {
		r, err := scanRemote(rows.Scan)
		if err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// scanRemote reads one remotes row. The scan function is passed in so
// ListRemotes and GetRemoteByName cannot drift apart.
func scanRemote(scan func(dest ...any) error) (model.Remote, error) {
	var r model.Remote
	var backend, cfgJSON, createdStr string
	var unsupported int
	if err := scan(&r.ID, &r.Name, &backend, &cfgJSON, &createdStr, &unsupported); err != nil {
		return model.Remote{}, err
	}
	r.Backend = model.RemoteBackend(backend)
	r.CreatedAt = displayTime("remotes.created_at", createdStr)
	r.Unsupported = unsupported != 0
	if err := json.Unmarshal([]byte(cfgJSON), &r.Config); err != nil {
		r.Config = map[string]string{}
	}
	return r, nil
}

// GetRemoteByName returns the remote with the given name, or nil if not found.
func (s *Store) GetRemoteByName(ctx context.Context, name string) (*model.Remote, error) {
	r, err := scanRemote(s.db.QueryRowContext(ctx,
		`SELECT id, name, backend, config, created_at, unsupported FROM remotes WHERE name=?`, name).Scan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &r, nil
}

// UpdateRemote replaces the config for an existing remote (name and backend are immutable).
func (s *Store) UpdateRemote(ctx context.Context, name string, cfg map[string]string) error {
	b, err := json.Marshal(cfg)
	if err != nil {
		return fmt.Errorf("marshal remote config: %w", err)
	}
	_, err = s.db.ExecContext(ctx, `UPDATE remotes SET config=? WHERE name=?`, string(b), name)
	return err
}

// DeleteRemote removes a remote by name.
func (s *Store) DeleteRemote(ctx context.Context, name string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM remotes WHERE name=?`, name)
	return err
}

// UpdateClientOffsite changes the offsite destination settings for a client.
// An empty remote disables offsite tiering for this client.
func (s *Store) UpdateClientOffsite(ctx context.Context, id int64, remote, dir, cronExpr, uploadMode string) error {
	if uploadMode == "" {
		uploadMode = "all"
	}
	res, err := s.db.ExecContext(ctx,
		`UPDATE clients SET offsite_remote=?, offsite_dir=?, offsite_cron=?, offsite_upload_mode=? WHERE id=?`,
		remote, dir, cronExpr, uploadMode, id)
	if err != nil {
		return fmt.Errorf("update offsite remote: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("client %d: not found", id)
	}
	return nil
}

// UpdateClientRetention changes the hot retention horizon and the expected
// backup interval used for health-status derivation. There is no offsite horizon
// to set: offsite retention is the B2 bucket's lifecycle rule.
func (s *Store) UpdateClientRetention(ctx context.Context, id int64, hotDays, expectedIntervalSecs int) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE clients SET retention_days=?, expected_interval_secs=? WHERE id=?`,
		hotDays, expectedIntervalSecs, id)
	if err != nil {
		return fmt.Errorf("update retention: %w", err)
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return fmt.Errorf("client %d: not found", id)
	}
	return nil
}

// ForgetOffsiteObject removes the offsite tracking record without touching the
// remote file. Use when the local copy has already been pruned and the operator
// just wants to clear the stale entry.
func (s *Store) ForgetOffsiteObject(ctx context.Context, clientID int64, snapshotID, remote string) error {
	_, err := s.db.ExecContext(ctx,
		`DELETE FROM offsite_objects WHERE client_id=? AND snapshot_id=? AND remote=?`,
		clientID, snapshotID, remote)
	return err
}

// ErrConflict is returned by RotateClientCreds when a concurrent rotation
// has already incremented the client's version since the caller read it.
var ErrConflict = fmt.Errorf("concurrent modification: version mismatch")

// RotateClientCreds replaces a client's SSH public key and token hash atomically.
// oldVersion is the version observed by the caller via GetClient; the UPDATE only
// applies if the version has not changed (compare-and-swap). Returns ErrConflict
// if a concurrent rotation already incremented the version.
func (s *Store) RotateClientCreds(ctx context.Context, id int64, pubKey, tokenHash, tokenPrefix string, oldVersion int) error {
	res, err := s.db.ExecContext(ctx,
		`UPDATE clients SET ssh_pubkey=?, token_hash=?, token_prefix=?, version=version+1 WHERE id=? AND version=?`,
		pubKey, tokenHash, tokenPrefix, id, oldVersion)
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

// ListRunsSince returns every run row for a client that started at or after
// since, oldest first.
//
// It exists for the hot-store guard (mode.UnbackedSnapshots), which asks "is
// there any run that reached ok for this snapshot directory" and must be able to
// tell a row that is absent because the run never finished from a row that is
// absent because history was pruned: the answer has to come from the rows that
// are still there, so the query returns them all rather than "the latest N".
//
// Only a bounded prefix of log_tail is loaded. The full column is capped at 64 KB
// per row and is cleared by the log-retention pass anyway; what the guard wants
// from it is the one line the server wrote when it declared a silent run dead
// ("interrupted: …"), which is quoted in the lifecycle's log line. Reading the
// whole column for every run of a client on every dashboard request is how a
// listing becomes expensive for no reason.
func (s *Store) ListRunsSince(ctx context.Context, clientID int64, since time.Time) ([]model.Run, error) {
	rows, err := s.db.QueryContext(ctx, `
		SELECT id, client_id, started_at, finished_at, status, bytes, files, snapshot_id,
		       substr(log_tail, 1, 512)
		FROM runs WHERE client_id = ? AND started_at >= ?
		ORDER BY started_at`, clientID, since.UTC().Format(rfc3339))
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
	if err := sc.Scan(&c.ID, &c.Name, &c.Slug, &mode, &c.SourceLabel, &excludes, &c.RetentionDays,
		&c.ExpectedIntervalSecs, &c.OffsiteRemote, &c.OffsiteDir,
		&c.OffsiteCron, &c.SSHPubKey, &c.TokenHash, &c.TokenPrefix, &enabled, &createdAt, &c.Version, &skipSymlinks,
		&c.OffsiteUploadMode); err != nil {
		return c, err
	}
	c.Mode = model.Mode(mode)
	c.Enabled = enabled != 0
	c.SkipSymlinks = skipSymlinks != 0
	if excludes != "" {
		_ = json.Unmarshal([]byte(excludes), &c.Excludes)
	}
	c.CreatedAt = displayTime("clients.created_at", createdAt)
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
	r.StartedAt = displayTime("runs.started_at", started)
	r.FinishedAt = displayTime("runs.finished_at", finished)
	return r, nil
}
