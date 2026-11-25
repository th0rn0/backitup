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

// MaxSlugLen is the longest storage slug the system will produce or accept. A
// slug becomes a directory name and part of an sshd forced command, so it is
// kept to a short, plain, lowercase component — the same ceiling the authkeys
// allowlist enforces, and well inside every filesystem's NAME_MAX.
const MaxSlugLen = 63

// Slug converts a client name into a URL/filesystem-safe identifier:
// lowercase, non-alphanumeric characters replaced with hyphens, consecutive
// hyphens collapsed, leading/trailing hyphens trimmed.
//
// A name made entirely of stripped characters (".", "..", "/", "....") yields
// "", which is NOT a usable directory component. Callers that turn a slug into a
// path must reject "" rather than joining it onto a base directory (that yields
// the base directory itself, shared by every other client).
//
// The result is capped at MaxSlugLen characters and never starts or ends with a
// hyphen, so it always satisfies the authkeys allowlist.
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
	slug := strings.TrimRight(b.String(), "-")
	if len(slug) > MaxSlugLen {
		// Cut, then drop the hyphen the cut may have exposed, so the value
		// still satisfies ^[a-z0-9][a-z0-9-]{0,62}$.
		slug = strings.TrimRight(slug[:MaxSlugLen], "-")
	}
	return slug
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
	ID   int64
	Name string // unique, human label, e.g. "laptop-docs"

	// Slug is the unique, server-derived storage identifier for this client and
	// the ONLY source of its directory (backups/<Slug>) and of its default
	// offsite prefix. It is computed once, on create, and persisted: deriving it
	// from Name on demand let two clients ("dup a", "dup-a", "Dup_A") share one
	// read-write SSH jail. Never recompute it from Name.
	Slug        string
	Mode        Mode
	SourceLabel string // descriptive only; the real path lives in the host's docker run

	// Behaviour (server-owned; D1). Returned to the client via GET /api/v1/config.
	Excludes     []string // glob excludes (rsync --exclude / tar --exclude)
	SkipSymlinks bool     // omit symlinks from the backup (BACKITUP_SKIP_SYMLINKS)

	// Fidelity: what the snapshot captures beyond `-a`. Opt-in per client,
	// rsync mode only, and every one of them defaults to OFF so a client that
	// was created before they existed keeps producing exactly the snapshot it
	// always did. -S and -H change what the snapshot HOLDS (holes stay holes,
	// in-tree hardlinks stay one inode); -A and -X carry metadata the receiver
	// can only apply as the file's owner, which it is (it creates the file).
	PreserveSparse    bool // rsync -S: keep holes as holes instead of writing zeros
	PreserveHardlinks bool // rsync -H: one inode for an in-tree hardlink pair
	PreserveACLs      bool // rsync -A: POSIX ACLs
	PreserveXattrs    bool // rsync -X: extended attributes

	// GroupReadable makes the snapshot tree group-readable as it is written
	// (rsync --chmod=Dg+rx,Fg+r), so an account in the server's group — gid
	// 10001 in the shipped image — can read a backup without root. rsync mode
	// only, default off.
	//
	// It is not a fidelity setting like the four above; it is the opposite of
	// one, and deliberately so: modes in the snapshot no longer mirror the
	// source (a 0600 file lands 0640-style), and because unchanged files are
	// hardlinks shared with earlier snapshots the mode lives on the inode, so
	// turning it on also changes the modes of earlier snapshots that share
	// those files. The operator is told both things on the client page.
	GroupReadable bool

	// Retention (D7/D8). Hot retention is ours: the disk the server owns. There
	// is no offsite horizon any more — cold storage is Backblaze B2 and its
	// bucket lifecycle rule owns offsite retention (see lifecycle.go), because a
	// rule living there cannot be doubled up, missed or argued with by backitup.
	RetentionDays int // hot store: prune snapshots/archives older than this

	// Advisory cadence (D-folded). The server does NOT enforce schedule (host cron
	// owns WHEN); this is only used to compute the "stale" dashboard state.
	ExpectedIntervalSecs int

	OffsiteRemote     string // rclone remote name selecting this client's cold target; "" = no offsite
	OffsiteDir        string // subdirectory within the remote; "" = use the client's slug
	OffsiteCron       string // standard 5-field cron expression for upload schedule; "" = every lifecycle pass
	OffsiteUploadMode string // model.OffsiteUploadAll (default), OffsiteUploadLatest or OffsiteUploadSingle

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
	LogTail    string // capped at MaxLogTail (64 KB) before write
}

// OffsiteObject tracks what has been pushed to cold storage, so the lifecycle
// worker can (a) never prune un-offsited data and (b) verify that what it
// believes is on the remote is really there. It is bookkeeping, not a retention
// horizon: the B2 bucket's lifecycle rule decides when an object expires.
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

	// DedupOf is the snapshot id whose remote object holds this snapshot's
	// content, set when an upload was skipped because an identical archive was
	// already on the remote. Empty means this row owns a real remote object.
	// A dedup row is never uploaded, never verified on its own and never
	// deleted on its own: the object it names belongs to DedupOf.
	DedupOf string
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
