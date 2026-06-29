// Package model holds backitup's core domain types, shared by the server
// (control plane) and referenced by the client protocol.
//
// Design decisions encoded here (see the design doc, Eng Review Decisions):
//   - D1: the server owns WHAT (mode, retention, offsite); host cron owns WHEN.
//   - D2: per-mode behaviour lives behind a Mode interface, not scattered branches.
//   - D7/D8: retention is bounded; offsite retention is INDEPENDENT of hot retention.
package model

import (
	"strings"
	"time"
)

// Slug converts a client name into a URL/filesystem-safe identifier:
// lowercase, non-alphanumeric characters replaced with hyphens, consecutive
// hyphens collapsed, leading/trailing hyphens trimmed.
func Slug(name string) string {
	var b strings.Builder
	prevHyphen := true // suppress leading hyphens
	for _, r := range strings.ToLower(name) {
		if r >= 'a' && r <= 'z' || r >= '0' && r <= '9' {
			b.WriteRune(r)
			prevHyphen = false
		} else if !prevHyphen {
			b.WriteByte('-')
			prevHyphen = true
		}
	}
	return strings.TrimRight(b.String(), "-")
}

// Mode is how a client captures and ships a backup. The two modes are different
// architectures, not two flags (see design doc Premise 2):
//
//	tar.gz  each run uploads a dated snapshot archive; prune old archives.
//	rsync   client rsyncs with --link-dest into dated dirs (hardlink snapshots);
//	        prune old snapshot dirs.
type Mode string

const (
	ModeTarGz Mode = "targz"
	ModeRsync Mode = "rsync"
)

func (m Mode) Valid() bool { return m == ModeTarGz || m == ModeRsync }

// Slug returns the URL/filesystem-safe identifier for this client.
func (c Client) Slug() string { return Slug(c.Name) }

// RunStatus is the outcome of a single client backup run, as reported back over
// the control channel. "overlap" means a prior run of the same client was still
// going and this trigger did nothing (see client lockfile, design doc).
type RunStatus string

const (
	StatusOK      RunStatus = "ok"
	StatusFailed  RunStatus = "failed"
	StatusOverlap RunStatus = "overlap"
	StatusRunning RunStatus = "running"
)

// Health is the DERIVED dashboard state for a client (DD2). It is computed from
// the latest run plus expected_interval; it is not stored. Encoded in the UI by
// icon + colour + text, never colour alone.
type Health string

const (
	HealthOK      Health = "ok"      // last run ok, within expected cadence
	HealthStale   Health = "stale"   // no successful run within 2x expected interval
	HealthFailed  Health = "failed"  // last run failed
	HealthNever   Health = "never"   // no run has ever completed
	HealthRunning Health = "running" // backup currently in progress
)

// Client is a single backup job: one source directory on one host, identified by
// a server-issued SSH key (data channel) + bearer token (control channel).
type Client struct {
	ID          int64
	Name        string // unique, human label, e.g. "laptop-docs"
	Mode        Mode
	SourceLabel string // descriptive only; the real path lives in the host's docker run

	// Behaviour (server-owned; D1). Returned to the client via GET /api/v1/config.
	Excludes     []string // glob excludes (rsync --exclude / tar --exclude)
	SkipSymlinks bool     // omit symlinks from the backup (BACKITUP_SKIP_SYMLINKS)

	// Retention (D7/D8). Hot and offsite are INDEPENDENT horizons.
	RetentionDays        int // hot store: prune snapshots/archives older than this
	OffsiteRetentionDays int // offsite: prune offsite objects older than this (usually larger)

	// Advisory cadence (D-folded). The server does NOT enforce schedule (host cron
	// owns WHEN); this is only used to compute the "stale" dashboard state.
	ExpectedIntervalSecs int

	OffsiteRemote         string // rclone remote name selecting this client's cold target; "" = no offsite
	OffsiteDir            string // subdirectory within the remote; "" = use the client's slug
	OffsiteCron           string // standard 5-field cron expression for upload schedule; "" = every lifecycle pass
	OffsiteUploadMode     string // "all" (default) or "latest" — which snapshots to push offsite
	DisableOffsitePruning bool   // skip lifecycle pruning; let the storage provider's lifecycle policy manage deletion

	// Auth. The private SSH key is shown once at creation and NOT retained (D4);
	// only the public key and a hash of the token live here.
	SSHPubKey   string
	TokenHash   string // argon2id of the bearer token
	TokenPrefix string // first 8 chars of the raw token (non-secret fast discriminator)

	Enabled   bool
	CreatedAt time.Time
	Version   int // compare-and-swap guard for credential rotation
}

// Run is one execution of a client backup, recorded from POST /api/v1/status.
type Run struct {
	ID         int64
	ClientID   int64
	StartedAt  time.Time
	FinishedAt time.Time
	Status     RunStatus
	Bytes      int64
	Files      int64
	SnapshotID string // tar.gz: archive filename; rsync: timestamp+seq dir name
	LogTail    string // capped at 4 KB before write
}

// OffsiteObject tracks what has been pushed to cold storage, so the lifecycle
// worker can (a) never prune un-offsited data and (b) prune offsite independently
// by OffsiteRetentionDays (D8).
type OffsiteObject struct {
	ID         int64
	ClientID   int64
	SnapshotID string
	Remote     string
	Bytes      int64
	UploadedAt time.Time

	// Populated by the lifecycle verification pass.
	RemoteMissing    bool      // true if last rclone lsf confirmed the file is gone
	RemoteVerifiedAt time.Time // zero = never verified

	Checksum string // SHA-256 hex of the plaintext archive; empty for legacy records
}

// OffsiteRun records one upload session (scheduled or adhoc). Mirrors the runs
// table for local backups so operators can see when offsite uploads happened,
// whether they succeeded, and how much data was moved.
type OffsiteRun struct {
	ID                int64
	ClientID          int64
	TriggeredBy       string // "scheduled" | "adhoc"
	StartedAt         time.Time
	FinishedAt        time.Time
	Status            string // "running" | "ok" | "failed"
	SnapshotsUploaded int
	BytesUploaded     int64
	ErrorText         string
}

// Admin is the single webgui admin account (D3). PasswordHash is argon2id.
// Retained for backwards-compat with the admin table; new code uses User.
type Admin struct {
	Username     string
	PasswordHash string
	CreatedAt    time.Time
}

// User is a web UI user account. All users have full access; there are no roles.
type User struct {
	ID           int64
	Username     string
	PasswordHash string
	CreatedAt    time.Time
}

// MaxLogTail is the log_tail cap in bytes. The server's status endpoint body
// limit is MaxLogTail + a small overhead for the other JSON fields.
const MaxLogTail = 64 * 1024

// CapLogTail trims a log to the last MaxLogTail bytes (DD4 / eng review).
func CapLogTail(s string) string {
	if len(s) <= MaxLogTail {
		return s
	}
	return s[len(s)-MaxLogTail:]
}
