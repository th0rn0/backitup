// Package lifecycle is backitup's server-side maintenance worker (design doc
// D1/D8/D9). Per pass, per client it: offsites new snapshots FIRST, prunes
// offsite on its own retention horizon, prunes the hot store (offsite-first,
// never the newest), trims run history, and integrity-checks the latest snapshot.
//
// It shells out to rclone for offsite (via the Offsite interface), so it never
// holds a SQLite write txn across a long upload.
package lifecycle

import (
	"context"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"sort"
	"time"

	"github.com/th0rn0/backitup/internal/alert"
	"github.com/th0rn0/backitup/internal/archiveutil"
	"github.com/th0rn0/backitup/internal/mode"
	"github.com/th0rn0/backitup/internal/model"
	"github.com/th0rn0/backitup/internal/store"
)

// DefaultRunsKeepDays bounds the runs table (D7).
const DefaultRunsKeepDays = 90

// DefaultLogRetentionDays is how long log_tail is kept before being cleared.
const DefaultLogRetentionDays = 7

// Offsite is the cold-storage backend (rclone crypt in production). objectName
// is the path within the remote, e.g. "client-3/20260618T....tar.gz".
type Offsite interface {
	Upload(ctx context.Context, localPath, remote, objectName string) (bytes int64, err error)
	Delete(ctx context.Context, remote, objectName string) error
}

// Deps are the worker's dependencies. Offsite nil disables cold tiering.
type Deps struct {
	Store            *store.Store
	Offsite          Offsite
	BackupBaseDir    string
	RunsKeepDays     int // 0 -> DefaultRunsKeepDays
	LogRetentionDays int // 0 -> DefaultLogRetentionDays; how long log_tail is kept
	Now              func() time.Time

	DiscordWebhook string              // empty disables Discord alerts
	Verbose        bool                // log offsite uploads/deletes and status changes
	staleAlerted   map[int64]time.Time // keyed by client ID; set by StartWorker
}

func (d Deps) now() time.Time {
	if d.Now != nil {
		return d.Now()
	}
	return time.Now()
}

// RunOnce executes one lifecycle pass over all enabled clients. A failure on one
// client is logged and does not stop the others; the first error is returned.
func RunOnce(ctx context.Context, d Deps) error {
	clients, err := d.Store.ListClients(ctx)
	if err != nil {
		return fmt.Errorf("list clients: %w", err)
	}
	var firstErr error
	for _, c := range clients {
		if !c.Enabled {
			continue
		}
		if err := processClient(ctx, d, c); err != nil {
			log.Printf("lifecycle: client %d (%s): %v", c.ID, c.Name, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}

	// Prune run logs globally (not per-client) on each tick.
	logDays := d.LogRetentionDays
	if logDays == 0 {
		logDays = DefaultLogRetentionDays
	}
	cutoff := d.now().AddDate(0, 0, -logDays)
	if n, err := d.Store.PruneRunLogs(ctx, cutoff); err != nil {
		log.Printf("lifecycle: prune run logs: %v", err)
		if firstErr == nil {
			firstErr = err
		}
	} else if n > 0 {
		log.Printf("lifecycle: pruned logs from %d runs older than %d days", n, logDays)
	}

	return firstErr
}

func processClient(ctx context.Context, d Deps, c model.Client) error {
	sm, ok := mode.Server(c.Mode)
	if !ok {
		return fmt.Errorf("no server mode for %q", c.Mode)
	}
	clientDir := filepath.Join(d.BackupBaseDir, model.Slug(c.Name))

	snaps, err := sm.List(ctx, clientDir)
	if err != nil {
		return fmt.Errorf("list snapshots: %w", err)
	}
	// Newest first; the newest is always protected from pruning.
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].CreatedAt.After(snaps[j].CreatedAt) })

	offsiteOn := d.Offsite != nil && c.OffsiteRemote != ""
	offsited, err := d.offsitedSet(ctx, c.ID)
	if err != nil {
		return err
	}

	offsiteThisPass := offsiteOn
	if offsiteOn && c.OffsiteIntervalSecs > 0 {
		due, err := offsiteDue(ctx, d, c)
		if err != nil {
			return err
		}
		offsiteThisPass = due
	}

	if offsiteThisPass {
		if err := offsiteNewSnapshots(ctx, d, c, sm, clientDir, snaps, offsited); err != nil {
			return err
		}
		if err := pruneOffsite(ctx, d, c); err != nil {
			return err
		}
	}

	pruneHot(ctx, d, c, sm, clientDir, snaps, offsiteOn, offsited)
	verifyLatest(ctx, c, sm, clientDir, snaps)
	checkStaleAlert(ctx, d, c)

	if keep := d.RunsKeepDays; true {
		if keep == 0 {
			keep = DefaultRunsKeepDays
		}
		if _, err := d.Store.PruneRuns(ctx, c.ID, keep); err != nil {
			return fmt.Errorf("prune runs: %w", err)
		}
	}
	return nil
}

func (d Deps) offsitedSet(ctx context.Context, clientID int64) (map[string]bool, error) {
	objs, err := d.Store.ListOffsiteObjects(ctx, clientID)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool, len(objs))
	for _, o := range objs {
		set[o.SnapshotID] = true
	}
	return set, nil
}

func offsiteNewSnapshots(ctx context.Context, d Deps, c model.Client, sm mode.ServerMode, clientDir string, snaps []mode.Snapshot, offsited map[string]bool) error {
	for _, s := range snaps {
		if offsited[s.ID] {
			continue
		}
		obj, err := sm.PrepareOffsite(ctx, clientDir, s)
		if err != nil {
			return fmt.Errorf("prepare offsite %s: %w", s.ID, err)
		}
		// rsync builds a temp archive; remove it after upload (tar.gz returns the
		// archive in place, which must NOT be removed).
		if c.Mode == model.ModeRsync {
			defer os.Remove(obj)
		}
		bytes, err := d.Offsite.Upload(ctx, obj, c.OffsiteRemote, objectPath(offsiteDir(c), c.Mode, s.ID))
		if err != nil {
			return fmt.Errorf("offsite upload %s: %w", s.ID, err)
		}
		if d.Verbose {
			log.Printf("offsite: client=%q remote=%s snapshot=%s uploaded bytes=%d", c.Name, c.OffsiteRemote, s.ID, bytes)
			go alert.Discord(d.DiscordWebhook, fmt.Sprintf(
				"☁️ **backitup** — `%s` offsite upload complete\nRemote: %s | snapshot: %s | bytes: %d",
				c.Name, c.OffsiteRemote, s.ID, bytes,
			))
		}
		if err := d.Store.RecordOffsiteObject(ctx, model.OffsiteObject{
			ClientID: c.ID, SnapshotID: s.ID, Remote: c.OffsiteRemote, Bytes: bytes,
		}); err != nil {
			return fmt.Errorf("record offsite %s: %w", s.ID, err)
		}
		offsited[s.ID] = true
	}
	return nil
}

// pruneOffsite enforces the INDEPENDENT offsite retention horizon (D8), keeping
// the newest offsite object regardless of age.
func pruneOffsite(ctx context.Context, d Deps, c model.Client) error {
	if c.OffsiteRetentionDays <= 0 {
		return nil
	}
	objs, err := d.Store.ListOffsiteObjects(ctx, c.ID) // newest first
	if err != nil {
		return err
	}
	cutoff := d.now().Add(-time.Duration(c.OffsiteRetentionDays) * 24 * time.Hour)
	for i, o := range objs {
		if i == 0 || !o.UploadedAt.Before(cutoff) {
			continue // protect newest; keep anything within the horizon
		}
		if err := d.Offsite.Delete(ctx, c.OffsiteRemote, objectPath(offsiteDir(c), c.Mode, o.SnapshotID)); err != nil {
			return fmt.Errorf("offsite delete %s: %w", o.SnapshotID, err)
		}
		if d.Verbose {
			log.Printf("offsite: client=%q remote=%s snapshot=%s pruned (exceeded %dd retention)", c.Name, c.OffsiteRemote, o.SnapshotID, c.OffsiteRetentionDays)
			go alert.Discord(d.DiscordWebhook, fmt.Sprintf(
				"🗑️ **backitup** — `%s` offsite snapshot pruned\nRemote: %s | snapshot: %s (exceeded %dd retention)",
				c.Name, c.OffsiteRemote, o.SnapshotID, c.OffsiteRetentionDays,
			))
		}
		if err := d.Store.DeleteOffsiteObject(ctx, c.ID, o.SnapshotID, c.OffsiteRemote); err != nil {
			return err
		}
	}
	return nil
}

// pruneHot deletes hot snapshots older than retention, never the newest, and —
// when offsite is enabled — only those already offsited (offsite-first, D8).
func pruneHot(ctx context.Context, d Deps, c model.Client, sm mode.ServerMode, clientDir string, snaps []mode.Snapshot, offsiteOn bool, offsited map[string]bool) {
	if c.RetentionDays <= 0 || len(snaps) <= 1 {
		return
	}
	cutoff := d.now().Add(-time.Duration(c.RetentionDays) * 24 * time.Hour)
	for i, s := range snaps {
		if i == 0 || !s.CreatedAt.Before(cutoff) {
			continue // protect newest; keep anything within the hot horizon
		}
		if offsiteOn && !offsited[s.ID] {
			log.Printf("lifecycle: client %d: keeping un-offsited snapshot %s (offsite-first)", c.ID, s.ID)
			continue
		}
		if err := sm.DeleteSnapshot(ctx, clientDir, s.ID); err != nil {
			log.Printf("lifecycle: client %d: prune %s: %v", c.ID, s.ID, err)
		}
	}
}

// verifyLatest integrity-checks the most recent snapshot (D9): tar.gz archives
// are read fully so gzip/CRC errors surface; rsync snapshots must be non-empty.
func verifyLatest(ctx context.Context, c model.Client, sm mode.ServerMode, clientDir string, snaps []mode.Snapshot) {
	if len(snaps) == 0 {
		return
	}
	latest := snaps[0]
	switch c.Mode {
	case model.ModeTarGz:
		if err := archiveutil.VerifyGzip(filepath.Join(clientDir, latest.ID)); err != nil {
			log.Printf("lifecycle: client %d: INTEGRITY FAIL on %s: %v", c.ID, latest.ID, err)
		}
	case model.ModeRsync:
		if latest.Bytes == 0 {
			log.Printf("lifecycle: client %d: INTEGRITY WARN: latest snapshot %s is empty", c.ID, latest.ID)
		}
	}
}

// checkStaleAlert fires a Discord alert if the client is stale and no alert
// has been sent in the last 24 h. Uses an in-memory map (resets on restart)
// to avoid spamming on every lifecycle pass.
func checkStaleAlert(ctx context.Context, d Deps, c model.Client) {
	if d.DiscordWebhook == "" || d.staleAlerted == nil {
		return
	}
	if c.ExpectedIntervalSecs <= 0 {
		return // no cadence defined; can't know if stale
	}
	run, err := d.Store.LatestRun(ctx, c.ID)
	if err != nil {
		return
	}
	h := model.DeriveHealth(run, time.Duration(c.ExpectedIntervalSecs)*time.Second, d.now())
	if h != model.HealthStale && h != model.HealthNever {
		delete(d.staleAlerted, c.ID) // recovered — reset so a future stale re-alerts
		return
	}
	const minInterval = 24 * time.Hour
	if last, ok := d.staleAlerted[c.ID]; ok && d.now().Sub(last) < minInterval {
		return
	}
	d.staleAlerted[c.ID] = d.now()
	go alert.Discord(d.DiscordWebhook, fmt.Sprintf(
		"⏰ **backitup** — `%s` backup is **STALE** (no successful run in the expected window)\nSource: %s\nExpected every: %ds",
		c.Name, c.SourceLabel, c.ExpectedIntervalSecs,
	))
}

// offsiteDir returns the remote subdirectory for a client. When the operator
// has not configured an explicit OffsiteDir, the client's slug is used so
// existing deployments are unaffected.
func offsiteDir(c model.Client) string {
	if c.OffsiteDir != "" {
		if c.OffsiteRemote == "gdrive" {
			// rclone Google Drive accepts {folderID} as a path component to
			// navigate directly to a folder by its Drive ID.
			return "{" + c.OffsiteDir + "}"
		}
		return c.OffsiteDir
	}
	return model.Slug(c.Name)
}

// offsiteDue returns true when enough time has passed since the last upload to
// satisfy the client's OffsiteIntervalSecs. Always returns true if no upload
// has happened yet (first upload should always proceed).
func offsiteDue(ctx context.Context, d Deps, c model.Client) (bool, error) {
	last, err := d.Store.LatestOffsite(ctx, c.ID)
	if err != nil {
		return false, err
	}
	if last == nil {
		return true, nil
	}
	return d.now().Sub(*last) >= time.Duration(c.OffsiteIntervalSecs)*time.Second, nil
}

func objectPath(dir string, m model.Mode, snapshotID string) string {
	name := snapshotID
	if m == model.ModeRsync {
		name = snapshotID + ".tar.gz" // rsync snapshot dir -> one offsite archive
	}
	return dir + "/" + name
}
