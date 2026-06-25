// Package lifecycle is backitup's server-side maintenance worker (design doc
// D1/D8/D9). Two independent workers run on separate schedules:
//
//   - Lifecycle worker (StartWorker / RunOnce): retention pruning, remote
//     existence verification, integrity checks, stale alerts, run-log trimming.
//     Never uploads; safe to run frequently without touching rclone.
//
//   - Offsite worker (StartOffsiteWorker / RunOffsiteOnce): uploads new
//     snapshots to cold storage per client based on each client's own
//     OffsiteIntervalSecs.
//
// Both shell out to rclone via the Offsite interface, so neither holds a
// SQLite write transaction across a long network operation.
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
	// Lsf returns the filenames (no path prefix) inside a remote directory.
	Lsf(ctx context.Context, remote, dir string) ([]string, error)
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

	// Uploads are handled exclusively by the offsite worker (RunOffsiteOnce /
	// StartOffsiteWorker). The lifecycle worker only prunes and verifies.
	if offsiteOn {
		if err := pruneOffsite(ctx, d, c); err != nil {
			return err
		}
	}
	pruneHot(ctx, d, c, sm, clientDir, snaps, offsiteOn, offsited)
	if offsiteOn {
		verifyOffsiteObjects(ctx, d, c)
	}
	verifyLatest(ctx, c, sm, clientDir, snaps)
	checkStaleAlert(ctx, d, c)

	keep := d.RunsKeepDays
	if keep == 0 {
		keep = DefaultRunsKeepDays
	}
	if _, err := d.Store.PruneRuns(ctx, c.ID, keep); err != nil {
		return fmt.Errorf("prune runs: %w", err)
	}
	return nil
}

// RunOffsiteOnce uploads new snapshots for all clients whose per-client
// OffsiteIntervalSecs has elapsed since the last upload. A failure on one
// client is logged and does not stop the others; the first error is returned.
func RunOffsiteOnce(ctx context.Context, d Deps) error {
	if d.Offsite == nil {
		return nil
	}
	clients, err := d.Store.ListClients(ctx)
	if err != nil {
		return fmt.Errorf("list clients: %w", err)
	}
	var firstErr error
	for _, c := range clients {
		if !c.Enabled || c.OffsiteRemote == "" {
			continue
		}
		if c.OffsiteIntervalSecs > 0 {
			due, err := offsiteDue(ctx, d, c)
			if err != nil {
				log.Printf("offsite worker: client %d (%s): interval check: %v", c.ID, c.Name, err)
				continue
			}
			if !due {
				continue
			}
		}
		if err := uploadClient(ctx, d, c); err != nil {
			log.Printf("offsite worker: client %d (%s): %v", c.ID, c.Name, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// uploadClient runs one upload pass for a single client and records the run.
func uploadClient(ctx context.Context, d Deps, c model.Client) error {
	sm, ok := mode.Server(c.Mode)
	if !ok {
		return fmt.Errorf("no server mode for %q", c.Mode)
	}
	clientDir := filepath.Join(d.BackupBaseDir, model.Slug(c.Name))
	snaps, err := sm.List(ctx, clientDir)
	if err != nil {
		return fmt.Errorf("list snapshots: %w", err)
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].CreatedAt.After(snaps[j].CreatedAt) })

	offsited, err := d.offsitedSet(ctx, c.ID)
	if err != nil {
		return err
	}

	start := d.now()
	snapsUploaded, bytesUploaded, uploadErr := offsiteNewSnapshots(ctx, d, c, sm, clientDir, snaps, offsited)
	if snapsUploaded > 0 || uploadErr != nil {
		recordOffsiteRun(ctx, d, c, "scheduled", start, snapsUploaded, bytesUploaded, uploadErr)
	}
	return uploadErr
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

// offsiteNewSnapshots uploads snapshots not yet in cold storage.
// When c.OffsiteUploadMode is "latest", only the newest un-offsited snapshot
// is uploaded (snaps must be sorted newest-first). Otherwise all are uploaded.
// Returns the number of snapshots uploaded and total bytes transferred.
func offsiteNewSnapshots(ctx context.Context, d Deps, c model.Client, sm mode.ServerMode, clientDir string, snaps []mode.Snapshot, offsited map[string]bool) (int, int64, error) {
	var totalSnaps int
	var totalBytes int64
	latestOnly := c.OffsiteUploadMode == "latest"
	for _, s := range snaps {
		if offsited[s.ID] {
			continue
		}
		if latestOnly && totalSnaps > 0 {
			break
		}
		obj, err := sm.PrepareOffsite(ctx, clientDir, s)
		if err != nil {
			return totalSnaps, totalBytes, fmt.Errorf("prepare offsite %s: %w", s.ID, err)
		}
		// rsync builds a temp archive; remove it after upload (tar.gz returns the
		// archive in place, which must NOT be removed).
		if c.Mode == model.ModeRsync {
			defer os.Remove(obj)
		}
		bytes, err := d.Offsite.Upload(ctx, obj, c.OffsiteRemote, objectPath(offsiteDir(c), c.Mode, s.ID))
		if err != nil {
			return totalSnaps, totalBytes, fmt.Errorf("offsite upload %s: %w", s.ID, err)
		}
		if d.Verbose {
			log.Printf("offsite: client=%q remote=%s snapshot=%s uploaded bytes=%d", c.Name, c.OffsiteRemote, s.ID, bytes)
		}
		if d.DiscordWebhook != "" {
			go alert.Discord(d.DiscordWebhook, fmt.Sprintf(
				"☁️ **backitup** — `%s` offsite upload complete\nRemote: %s | snapshot: %s | bytes: %d",
				c.Name, c.OffsiteRemote, s.ID, bytes,
			))
		}
		if err := d.Store.RecordOffsiteObject(ctx, model.OffsiteObject{
			ClientID: c.ID, SnapshotID: s.ID, Remote: c.OffsiteRemote, Bytes: bytes,
		}); err != nil {
			return totalSnaps, totalBytes, fmt.Errorf("record offsite %s: %w", s.ID, err)
		}
		offsited[s.ID] = true
		totalSnaps++
		totalBytes += bytes
	}
	return totalSnaps, totalBytes, nil
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
		}
		if d.DiscordWebhook != "" {
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
		return c.OffsiteDir
	}
	return model.Slug(c.Name)
}

// verifyOffsiteObjects calls rclone lsf for the client's remote directory and
// updates the remote_missing / remote_verified_at fields on each offsite_objects
// row. Throttled to once per hour per client: if the oldest verified_at is
// within the last hour, the check is skipped.
func verifyOffsiteObjects(ctx context.Context, d Deps, c model.Client) {
	objects, err := d.Store.ListOffsiteObjects(ctx, c.ID)
	if err != nil || len(objects) == 0 {
		return
	}

	// Skip if all objects have been checked within the last hour.
	oldestVerified := time.Time{}
	for _, o := range objects {
		if o.RemoteVerifiedAt.IsZero() {
			oldestVerified = time.Time{} // force run
			break
		}
		if oldestVerified.IsZero() || o.RemoteVerifiedAt.Before(oldestVerified) {
			oldestVerified = o.RemoteVerifiedAt
		}
	}
	if !oldestVerified.IsZero() && d.now().Sub(oldestVerified) < time.Hour {
		return
	}

	dir := offsiteDir(c)
	lsfCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	files, err := d.Offsite.Lsf(lsfCtx, c.OffsiteRemote, dir)
	cancel()
	if err != nil {
		log.Printf("lifecycle: offsite verify: client=%s: lsf: %v", c.Name, err)
		return // don't update status on lsf failure; keep last known state
	}

	fileSet := make(map[string]bool, len(files))
	for _, f := range files {
		fileSet[f] = true
	}

	now := d.now().UTC()
	for _, o := range objects {
		expectedName := o.SnapshotID
		if c.Mode == model.ModeRsync {
			expectedName += ".tar.gz"
		}
		missing := !fileSet[expectedName]
		if err := d.Store.UpdateOffsiteRemoteStatus(ctx, o.ID, missing, now); err != nil {
			log.Printf("lifecycle: offsite verify: update: %v", err)
		}
		if missing {
			log.Printf("lifecycle: offsite verify: client=%s snap=%s missing from %s", c.Name, o.SnapshotID, c.OffsiteRemote)
		}
	}
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

// OffsiteClient runs an immediate offsite upload + prune pass for a single
// client, bypassing the per-client interval check. Safe to call concurrently
// with the background worker because all state mutations go through the store.
// Uses a two-step Start/Finish so the dashboard can show upload progress.
func OffsiteClient(ctx context.Context, d Deps, c model.Client) error {
	if d.Offsite == nil || c.OffsiteRemote == "" {
		return fmt.Errorf("client %q has no offsite remote configured", c.Name)
	}
	sm, ok := mode.Server(c.Mode)
	if !ok {
		return fmt.Errorf("no server mode for %q", c.Mode)
	}
	clientDir := filepath.Join(d.BackupBaseDir, model.Slug(c.Name))
	snaps, err := sm.List(ctx, clientDir)
	if err != nil {
		return fmt.Errorf("list snapshots: %w", err)
	}
	sort.Slice(snaps, func(i, j int) bool { return snaps[i].CreatedAt.After(snaps[j].CreatedAt) })

	offsited, err := d.offsitedSet(ctx, c.ID)
	if err != nil {
		return err
	}

	// Insert a "running" row immediately so the dashboard can show progress.
	// Pruning is the lifecycle worker's job; the adhoc trigger only uploads.
	runID, startErr := d.Store.StartOffsiteRun(ctx, c.ID, "adhoc", d.now())
	snapsUploaded, bytesUploaded, uploadErr := offsiteNewSnapshots(ctx, d, c, sm, clientDir, snaps, offsited)
	if startErr == nil {
		status, errText := "ok", ""
		if uploadErr != nil {
			status, errText = "failed", uploadErr.Error()
			log.Printf("offsite: client=%q remote=%s adhoc FAILED: %v", c.Name, c.OffsiteRemote, uploadErr)
			if d.DiscordWebhook != "" {
				go alert.Discord(d.DiscordWebhook, fmt.Sprintf(
					"❌ **backitup** — `%s` offsite adhoc **FAILED**\nRemote: `%s`\nError: %s",
					c.Name, c.OffsiteRemote, uploadErr.Error(),
				))
			}
		}
		if err := d.Store.FinishOffsiteRun(ctx, runID, status, snapsUploaded, bytesUploaded, errText, d.now()); err != nil {
			log.Printf("offsite run: finish failed for client=%q: %v", c.Name, err)
		}
	} else {
		// StartOffsiteRun failed — fall back to single-INSERT so the run is still recorded.
		recordOffsiteRun(ctx, d, c, "adhoc", d.now(), snapsUploaded, bytesUploaded, uploadErr)
	}
	return uploadErr
}

// recordOffsiteRun writes a completed offsite session to the store. Non-fatal:
// a logging failure never aborts the upload itself.
func recordOffsiteRun(ctx context.Context, d Deps, c model.Client, triggeredBy string, start time.Time, snaps int, bytes int64, runErr error) {
	status, errText := "ok", ""
	if runErr != nil {
		status, errText = "failed", runErr.Error()
		log.Printf("offsite: client=%q remote=%s %s FAILED: %v", c.Name, c.OffsiteRemote, triggeredBy, runErr)
		if d.DiscordWebhook != "" {
			go alert.Discord(d.DiscordWebhook, fmt.Sprintf(
				"❌ **backitup** — `%s` offsite %s **FAILED**\nRemote: `%s`\nError: %s",
				c.Name, triggeredBy, c.OffsiteRemote, runErr.Error(),
			))
		}
	}
	if err := d.Store.RecordOffsiteRun(ctx, model.OffsiteRun{
		ClientID:          c.ID,
		TriggeredBy:       triggeredBy,
		StartedAt:         start,
		FinishedAt:        d.now(),
		Status:            status,
		SnapshotsUploaded: snaps,
		BytesUploaded:     bytes,
		ErrorText:         errText,
	}); err != nil {
		log.Printf("offsite run: record failed for client=%q: %v", c.Name, err)
	}
}

func objectPath(dir string, m model.Mode, snapshotID string) string {
	name := snapshotID
	if m == model.ModeRsync {
		name = snapshotID + ".tar.gz" // rsync snapshot dir -> one offsite archive
	}
	return dir + "/" + name
}
