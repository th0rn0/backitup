// Package lifecycle is backitup's server-side maintenance worker (design doc
// D1/D8/D9). Two independent workers run on separate schedules:
//
//   - Lifecycle worker (StartWorker / RunOnce): hot-store retention pruning,
//     remote existence verification, integrity checks, stale alerts, run-log
//     trimming. Never uploads and never deletes an offsite object: cold storage
//     is Backblaze B2 and the bucket's lifecycle rule owns offsite retention, so
//     an operator asking "why is B2 not shrinking" gets the answer from the
//     startup line (LogOffsiteRetentionDelegated) rather than from a debugging
//     session. Safe to run frequently without touching rclone.
//
//   - Offsite worker (StartOffsiteWorker / RunOffsiteOnce): uploads new
//     snapshots to cold storage per client based on each client's own
//     OffsiteCron schedule (standard 5-field cron expression).
//
// Both shell out to rclone via the Offsite interface, so neither holds a
// SQLite write transaction across a long network operation.
package lifecycle

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/robfig/cron/v3"
	"github.com/th0rn0/backitup/internal/alert"
	"github.com/th0rn0/backitup/internal/authkeys"
	"github.com/th0rn0/backitup/internal/diskusage"
	"github.com/th0rn0/backitup/internal/mode"
	"github.com/th0rn0/backitup/internal/model"
	"github.com/th0rn0/backitup/internal/store"
)

// DefaultRunsKeepDays bounds the runs table (D7).
const DefaultRunsKeepDays = 90

// DefaultLogRetentionDays is how long log_tail is kept before being cleared.
const DefaultLogRetentionDays = 7

// DefaultPartOrphanAge is how old a .tar.gz.part file must be before it is
// considered an abandoned upload and removed. An active upload continuously
// updates the file's mtime, so anything older than this threshold is stale.
const DefaultPartOrphanAge = 2 * time.Hour

// DefaultIncompleteAge is how long a snapshot a failed run marked incomplete is
// left on disk before the lifecycle reclaims it.
//
// A marked directory can never become a snapshot: a retry always writes a new
// timestamp directory, and the marker is only written after rsync has exited.
// The grace is therefore about the one case that is not a retry — a second run
// that happened to start in the same second and is still writing into the same
// directory — and it costs only the disk a failed run already paid for.
const DefaultIncompleteAge = time.Hour

// OffsiteRetentionDelegatedMessage is the answer to "why is cold storage not
// shrinking?", printed once at startup instead of being rediscovered.
//
// Offsite retention is not backitup's job: cold storage is Backblaze B2 and its
// bucket lifecycle rule expires objects. Nothing in this package deletes an
// offsite object on a schedule, so a fleet whose B2 bucket keeps growing is a
// misconfigured rule — and the only useful thing the log can do about that is
// say whose setting it is.
const OffsiteRetentionDelegatedMessage = "lifecycle: offsite retention is delegated to the B2 bucket lifecycle rule; backitup will not delete offsite objects"

// offsiteRetentionDelegatedOnce keeps that line to one per process: the server
// starts two workers (lifecycle and offsite), and a line repeated per worker
// would read as a recurring event rather than a statement about the build.
var offsiteRetentionDelegatedOnce sync.Once

// LogOffsiteRetentionDelegated prints the delegation line once per process.
// Called at startup (cmd/server); the second and later calls say nothing.
func LogOffsiteRetentionDelegated() {
	offsiteRetentionDelegatedOnce.Do(func() {
		log.Print(OffsiteRetentionDelegatedMessage)
	})
}

// Offsite is the cold-storage backend (rclone crypt in production). objectName
// is the path within the remote, e.g. "client-3/20260618T....tar.gz".
type Offsite interface {
	Upload(ctx context.Context, localPath, remote, objectName string) (bytes int64, err error)
	// Move renames an object inside the remote in one step. Production is rclone
	// moveto, which is a server-side copy followed by a server-side delete, so no
	// bytes cross the wire twice. It exists for the single-generation mode, where
	// a finished upload has to replace the live object without a window in which
	// the client has no cold copy at all.
	Move(ctx context.Context, remote, from, to string) error
	Delete(ctx context.Context, remote, objectName string) error
	// Lsf returns the filenames (no path prefix) inside a remote directory.
	Lsf(ctx context.Context, remote, dir string) ([]string, error)
}

// DefaultStaleRunTimeout is the maximum duration a run can stay in "running"
// state before the lifecycle worker marks it failed. getConfig handles the
// common case (client reconnects); this is the safety net for clients that
// never reconnect.
const DefaultStaleRunTimeout = 2 * time.Hour

// Deps are the worker's dependencies. Offsite nil disables cold tiering.
type Deps struct {
	Store            *store.Store
	Offsite          Offsite
	BackupBaseDir    string
	RunsKeepDays     int           // 0 -> DefaultRunsKeepDays
	LogRetentionDays int           // 0 -> DefaultLogRetentionDays; how long log_tail is kept
	PartOrphanAge    time.Duration // 0 -> DefaultPartOrphanAge; min age before a .part file is removed
	StaleRunTimeout  time.Duration // 0 -> DefaultStaleRunTimeout; max age of a "running" run
	Now              func() time.Time

	// DiskUsageMaxEntries bounds the per-client hot-store measurement this pass
	// records for the dashboard. 0 -> diskusage.DefaultMaxEntries, negative ->
	// no cap (tests over small trees want an exact figure). The cap keeps a
	// pathological tree from stalling a pass whose other job is retention; a
	// walk that hits it stores a lower bound and says so.
	DiskUsageMaxEntries int64

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
	// Mark runs stuck in "running" state as failed BEFORE any client is
	// processed (safety net for clients that crash and never reconnect; getConfig
	// handles the common case). The order is part of the killed-run guard: a run
	// that stops reporting is exactly the one that leaves an unmarked directory
	// behind, and the hot-store guard decides what to do with a directory from
	// the row that named it. A row still saying `running` means "this may be
	// being written right now" (mode.UnbackedSnapshots leaves such a snapshot
	// alone), so it has to be settled first — otherwise this pass would leave the
	// leftovers visible for one more pass after the timeout had already expired.
	staleTimeout := d.StaleRunTimeout
	if staleTimeout == 0 {
		staleTimeout = DefaultStaleRunTimeout
	}
	var firstErr error
	if n, err := d.Store.MarkStaleRunsFailed(ctx, d.now().Add(-staleTimeout)); err != nil {
		log.Printf("lifecycle: mark stale runs failed: %v", err)
		firstErr = err
	} else if n > 0 {
		log.Printf("lifecycle: marked %d stale run(s) as failed (exceeded %s)", n, staleTimeout)
	}

	clients, err := d.Store.ListClients(ctx)
	if err != nil {
		return fmt.Errorf("list clients: %w", err)
	}
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

func (d Deps) clientDir(c model.Client) (string, error) {
	return clientDirIn(d.BackupBaseDir, c)
}

// clientDirIn returns the storage directory for c: baseDir/<slug>.
//
// Every per-client path below goes through here, so the "a directory only ever
// comes from a persisted, safe slug" rule is enforced once rather than trusted
// at each site. filepath.Join(baseDir, "") is baseDir — shared by everyone — so
// an unusable slug must fail instead of resolving to a path.
func clientDirIn(baseDir string, c model.Client) (string, error) {
	if reason := authkeys.InvalidSlug(c.Slug); reason != "" {
		return "", fmt.Errorf("client %d (%q) has an unusable slug: %s", c.ID, c.Name, reason)
	}
	return filepath.Join(baseDir, c.Slug), nil
}

func processClient(ctx context.Context, d Deps, c model.Client) error {
	sm, ok := mode.Server(c.Mode)
	if !ok {
		return fmt.Errorf("no server mode for %q", c.Mode)
	}
	clientDir, err := d.clientDir(c)
	if err != nil {
		return err
	}

	snaps, err := sm.List(ctx, clientDir)
	if err != nil {
		return fmt.Errorf("list snapshots: %w", err)
	}
	// The mode lists directories. A directory a run was killed in the middle of
	// writing is not a backup, and only the run history knows that: nothing
	// marked it, because the run never got to report. Drop those from the pass
	// (retention must not count them, verifyLatest must not vouch for them) and
	// hand them to pruneIncomplete, which reclaims them once they are old.
	snaps, unbacked, err := VisibleSnapshots(ctx, d.Store, c, sm, snaps)
	if err != nil {
		// A history that could not be read is not evidence that a snapshot is
		// not a backup, so the listing comes back whole and this pass goes on
		// with it. The store error will surface from the calls below anyway.
		log.Printf("lifecycle: client %d (%s): %v: not hiding any snapshot this pass", c.ID, c.Name, err)
	}
	// Newest first; the newest is always protected from pruning.
	mode.SortNewestFirst(snaps)

	offsiteOn := d.Offsite != nil && c.OffsiteRemote != ""
	verify := offsiteOn
	root := ""
	if c.OffsiteRemote != "" {
		remoteRoot, unsupported, tErr := offsiteTarget(ctx, d, c)
		if tErr != nil {
			return fmt.Errorf("resolve offsite remote: %w", tErr)
		}
		if unsupported {
			// The remote is one this build dropped: do not verify against it —
			// a check that ran against a provider the project no longer
			// supports would report on objects nothing is maintaining.
			//
			// offsiteOn deliberately STAYS true for the pruner: what it gates is
			// "do not delete a hot copy that has no offsite copy", and with an
			// unusable remote nothing is offsited. Treating the client as
			// offsite-less here would prune the only copies of the data while
			// the operator still believes they are tiered.
			logUnsupportedRemote("lifecycle", c)
			verify = false
		}
		root = remoteRoot
	}
	// Remove stale .part files left by interrupted uploads before any pruning.
	prunePartOrphans(d, clientDir, c.Name)
	// Reclaim the directories failed and killed runs left behind. They are
	// already invisible to the listing, so nothing else would ever remove them —
	// and a half-copied 375 GB tree that no listing, check or retention rule can
	// see is exactly how a backup volume fills up with nothing to show for it.
	pruneIncomplete(ctx, d, c, sm, clientDir, unbacked)

	// The remote check runs BEFORE the prune, not after it. pruneHot deletes a
	// hot snapshot because an offsite record says the content is elsewhere, so
	// the listing that answers "is it still?" has to be part of this pass: run
	// after the prune, a pass could delete the last copy of a snapshot whose
	// object the bucket's lifecycle rule expired minutes earlier, and only then
	// notice it had. force is set when a delete is about to be justified by a
	// dedupe claim, the one record that goes stale without anything here doing
	// it (see pruneLeansOnDedupe).
	objs, err := d.Store.ListOffsiteObjects(ctx, c.ID)
	if err != nil {
		return fmt.Errorf("list offsite objects: %w", err)
	}
	if verify {
		verifyOffsiteObjects(ctx, d, c, root, pruneLeansOnDedupe(c, snaps, objs, d.now()))
		if objs, err = d.Store.ListOffsiteObjects(ctx, c.ID); err != nil {
			return fmt.Errorf("list offsite objects: %w", err)
		}
	}
	offsited := offsitedSetFrom(objs)

	// Uploads are handled exclusively by the offsite worker (RunOffsiteOnce /
	// StartOffsiteWorker). The lifecycle worker only prunes the hot store and
	// verifies; it never deletes an offsite object, on any schedule. Offsite
	// retention belongs to the B2 bucket's lifecycle rule (see
	// LogOffsiteRetentionDelegated).
	deleted, failed, pruneErr := pruneHot(ctx, d, c, sm, clientDir, snaps, offsiteOn, offsited)
	if deleted > 0 || failed > 0 {
		log.Printf("lifecycle: client %d (%s): hot prune: deleted=%d failed=%d", c.ID, c.Name, deleted, failed)
	}
	verifyLatest(ctx, c, sm, clientDir, snaps)
	checkStaleAlert(ctx, d, c)

	// Measure the hot store for the dashboard, last so the figure covers this
	// pass's own pruning. Bounded (diskusage.Measure): a pathological tree costs
	// the cap, not the pass, and a capped walk stores a lower bound. A store that
	// cannot be measured keeps its previous figure and its age — never a silent
	// zero, and never a walk on a request path.
	if err := measureClientStorage(ctx, d, c, clientDir); err != nil && !errors.Is(err, fs.ErrNotExist) {
		log.Printf("lifecycle: client %d (%s): measure disk usage: %v", c.ID, c.Name, err)
	}

	keep := d.RunsKeepDays
	if keep == 0 {
		keep = DefaultRunsKeepDays
	}
	if _, err := d.Store.PruneRuns(ctx, c.ID, keep); err != nil {
		return fmt.Errorf("prune runs: %w", err)
	}
	// Last, so a failed prune still lets verification and the alerting pass run;
	// the caller turns this into a non-clean lifecycle pass.
	return pruneErr
}

// measureClientStorage records how much disk the client's hot store occupies, so
// the dashboard can report a number that was measured once rather than a sum of
// per-snapshot sizes (rsync reports 0 on purpose: summing its hardlinked tree is
// a full walk) or a walk on every page load.
//
// A client whose directory does not exist yet is not an error — it simply has
// nothing to measure until its first backup — and the caller treats that as "no
// measurement" rather than storing a zero.
func measureClientStorage(ctx context.Context, d Deps, c model.Client, clientDir string) error {
	res, err := diskusage.Measure(ctx, clientDir, d.DiskUsageMaxEntries)
	if err != nil {
		return err
	}
	if err := d.Store.SetClientStorage(ctx, c.ID, res.Bytes, res.Partial, d.now()); err != nil {
		return fmt.Errorf("record measurement: %w", err)
	}
	if res.Partial {
		log.Printf("lifecycle: client %d (%s): disk usage stopped at the %d-entry cap; %d bytes is a lower bound",
			c.ID, c.Name, res.Entries, res.Bytes)
	}
	return nil
}

// RunOffsiteOnce uploads new snapshots for all clients whose OffsiteCron
// schedule is due. A failure on one client is logged and does not stop the
// others; the first error is returned.
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
		root, unsupported, err := offsiteTarget(ctx, d, c)
		if err != nil {
			// A store blip is not a reason to upload somewhere else: leave this
			// client for the next pass rather than guessing its path.
			log.Printf("offsite worker: client %d (%s): resolve remote: %v", c.ID, c.Name, err)
			continue
		}
		if unsupported {
			// Never upload through a remote this build does not support. The row
			// is kept (the operator's configuration is theirs) and the UI says
			// to recreate it as B2; until they do, this client is not tiered.
			logUnsupportedRemote("offsite worker", c)
			continue
		}
		if c.OffsiteCron != "" {
			due, err := offsiteDue(ctx, d, c)
			if err != nil {
				log.Printf("offsite worker: client %d (%s): schedule check: %v", c.ID, c.Name, err)
				continue
			}
			if !due {
				continue
			}
		}
		if err := uploadClient(ctx, d, c, root); err != nil {
			log.Printf("offsite worker: client %d (%s): %v", c.ID, c.Name, err)
			if firstErr == nil {
				firstErr = err
			}
		}
	}
	return firstErr
}

// uploadClient runs one upload pass for a single client and records the run.
// root is the path prefix inside the client's remote (see offsiteTarget).
func uploadClient(ctx context.Context, d Deps, c model.Client, root string) error {
	sm, ok := mode.Server(c.Mode)
	if !ok {
		return fmt.Errorf("no server mode for %q", c.Mode)
	}
	clientDir, err := d.clientDir(c)
	if err != nil {
		return err
	}
	snaps, err := sm.List(ctx, clientDir)
	if err != nil {
		return fmt.Errorf("list snapshots: %w", err)
	}
	// Nothing goes offsite that the run history cannot vouch for. A directory a
	// killed run left behind is unmarked and would otherwise be tarred and
	// uploaded as this client's newest backup — the failure this whole path
	// exists to prevent. Unlike the lifecycle pass, this one treats an
	// unreadable run history as fatal: uploading an unverified directory is
	// worse than waiting for the next pass.
	snaps, _, err = VisibleSnapshots(ctx, d.Store, c, sm, snaps)
	if err != nil {
		return fmt.Errorf("verify snapshots against the run history: %w", err)
	}
	mode.SortNewestFirst(snaps)

	// Look at the remote before deciding anything is already there. The dedupe
	// below asks the database whether identical content is on the remote, and the
	// database only knows what the last check saw; this pass is already talking
	// to rclone and is about to *create* that claim, so an answer from the last
	// hour is not enough. Unforced, an object the bucket's lifecycle rule expired
	// would be deduplicated into a hole — a second false "offsited" record
	// pointing at nothing, which is the failure this whole path exists to avoid.
	verifyOffsiteObjects(ctx, d, c, root, true)

	// The rows as well as the set: the single-generation mode decides what to
	// upload from what the one row claims the fixed object holds, not merely
	// from whether a snapshot has a row.
	objs, err := d.Store.ListOffsiteObjects(ctx, c.ID)
	if err != nil {
		return err
	}

	start := d.now()
	snapsUploaded, bytesUploaded, uploadErr := offsiteNewSnapshots(ctx, d, c, sm, clientDir, snaps, objs, root)
	if snapsUploaded > 0 || uploadErr != nil {
		recordOffsiteRun(ctx, d, c, "scheduled", start, snapsUploaded, bytesUploaded, uploadErr)
	}
	return uploadErr
}

// offsitedSetFrom answers "which snapshots' content is believed to be in cold
// storage", for records a pass has already loaded.
//
// "Believed" is doing the work: a record only counts while the object it names
// is thought to exist. A row the verification pass has reported missing protects
// nothing (its content is not on the remote), and a dedup row counts only while
// the object it points at is itself intact — otherwise "offsited" would mean "was
// offsited once", and the hot copy would be pruned with no copy left anywhere.
func offsitedSetFrom(objs []model.OffsiteObject) map[string]bool {
	present := make(map[string]bool, len(objs))
	for _, o := range objs {
		if o.DedupOf == "" && !o.RemoteMissing {
			present[o.SnapshotID] = true
		}
	}
	set := make(map[string]bool, len(objs))
	for _, o := range objs {
		if o.DedupOf == "" {
			if !o.RemoteMissing {
				set[o.SnapshotID] = true
			}
			continue
		}
		if present[o.DedupOf] {
			set[o.SnapshotID] = true
		}
	}
	return set
}

// pruneLeansOnDedupe reports whether this pass would delete a hot snapshot on
// the strength of a dedupe claim.
//
// Every other offsite record names an object this build uploaded itself, so it
// only goes stale when someone else acts. A dedupe record is an inference — "my
// content lives in that object" — and the B2 bucket's lifecycle rule can make it
// false without backitup hearing about it. The hourly verification throttle
// exists so that an idle fleet does not talk to rclone on every pass; when such a
// claim is about to justify deleting a hot copy, an answer from the previous hour
// is not good enough and the caller must look now.
//
// Since the guard became a freshness rule, the claim a delete leans on is the one
// for the newest offsite snapshot: everything strictly older than it is deleted
// because that copy exists. So the question is whether THAT record is a dedupe —
// checking each candidate's own record would answer the old, per-snapshot
// question and miss exactly the case that matters (an older direct upload pruned
// on the strength of a newer, and expired, dedupe claim).
func pruneLeansOnDedupe(c model.Client, snaps []mode.Snapshot, objs []model.OffsiteObject, now time.Time) bool {
	if c.RetentionDays <= 0 || len(snaps) <= 1 {
		return false
	}
	dedupBacked := make(map[string]bool, len(objs))
	for _, o := range objs {
		if o.DedupOf != "" {
			dedupBacked[o.SnapshotID] = true
		}
	}
	if len(dedupBacked) == 0 {
		return false
	}
	newestOffsite, ok := newestOffsiteSnapshot(snaps, offsitedSetFrom(objs))
	if !ok || !dedupBacked[newestOffsite.ID] {
		return false
	}
	cutoff := now.Add(-time.Duration(c.RetentionDays) * 24 * time.Hour)
	newest, ok := newestSnapshot(snaps)
	if !ok {
		return false
	}
	for _, s := range snaps {
		if s.ID == newest.ID || !s.CreatedAt.Before(cutoff) {
			continue
		}
		if strictlyOlder(s, newestOffsite) {
			return true
		}
	}
	return false
}

// offsiteNewSnapshots gets this client's snapshots into cold storage, in the
// shape its offsite_upload_mode asks for.
//
//   - "all": every snapshot without a copy is uploaded (or recorded as dedup).
//   - "latest": at most one un-offsited snapshot per pass — the newest, since
//     snaps must be sorted newest-first. Each upload is still its own object.
//   - "single": the fixed object (latest.tar.gz) is replaced by the newest
//     snapshot, and the client keeps exactly one object and one row. See
//     offsiteSingleSnapshot.
//
// Returns the number of snapshots uploaded and total bytes transferred;
// deduplicated snapshots are recorded but not counted as uploads.
func offsiteNewSnapshots(ctx context.Context, d Deps, c model.Client, sm mode.ServerMode, clientDir string, snaps []mode.Snapshot, objs []model.OffsiteObject, root string) (int, int64, error) {
	if c.OffsiteUploadMode == model.OffsiteUploadSingle {
		bytes, uploaded, err := offsiteSingleSnapshot(ctx, d, c, sm, clientDir, snaps, objs, root)
		if !uploaded {
			return 0, 0, err
		}
		return 1, bytes, err
	}
	var totalSnaps int
	var totalBytes int64
	offsited := offsitedSetFrom(objs)
	latestOnly := c.OffsiteUploadMode == model.OffsiteUploadLatest
	for _, s := range snaps {
		if offsited[s.ID] {
			continue
		}
		if latestOnly && totalSnaps > 0 {
			break
		}
		bytes, uploaded, err := offsiteSnapshot(ctx, d, c, sm, clientDir, s, offsited, root)
		if err != nil {
			return totalSnaps, totalBytes, err
		}
		if uploaded {
			totalSnaps++
			totalBytes += bytes
		}
	}
	return totalSnaps, totalBytes, nil
}

// offsiteSingleSnapshot replaces the client's one cold copy with the newest
// snapshot, in the only order that cannot lose it:
//
//  1. upload the archive to "latest.tar.gz.part" — a name nothing reads, so a
//     failed or truncated transfer leaves the live object untouched;
//  2. move it to "latest.tar.gz" (rclone moveto: server-side copy, then a
//     server-side delete), which is what replaces the previous generation;
//  3. only then replace the database rows with the one that names the held
//     snapshot, its checksum, size and upload time.
//
// The row is written last on purpose. A row that named the new snapshot before
// the move landed would be a claim that the fixed object holds content it does
// not, and the hot pruner deletes on the strength of exactly that claim.
//
// Nothing is uploaded when the fixed object already holds the newest snapshot:
// there is nothing newer to replace it with, so a pass over an unchanged client
// touches no bytes and — like the other modes — writes no offsite_run row.
func offsiteSingleSnapshot(ctx context.Context, d Deps, c model.Client, sm mode.ServerMode, clientDir string, snaps []mode.Snapshot, objs []model.OffsiteObject, root string) (int64, bool, error) {
	newest, ok := newestSnapshot(snaps)
	if !ok {
		return 0, false, nil // no snapshot to tier
	}
	if held, ok := singleHeldSnapshot(objs, c.OffsiteRemote); ok && held == newest.ID {
		return 0, false, nil // the fixed object already holds the newest snapshot
	}
	obj, err := sm.PrepareOffsite(ctx, clientDir, newest)
	if err != nil {
		return 0, false, fmt.Errorf("prepare offsite %s: %w", newest.ID, err)
	}
	// rsync builds a temp archive; remove it as soon as this upload is done
	// (tar.gz returns the archive in place, which must NOT be removed).
	if c.Mode == model.ModeRsync {
		defer func() { _ = os.Remove(obj) }()
	}
	checksum, err := fileChecksum(obj)
	if err != nil {
		return 0, false, fmt.Errorf("checksum %s: %w", newest.ID, err)
	}

	live := objectPathFor(root, c, newest.ID)
	part := model.ObjectPath(root, offsiteDir(c), model.SingleObjectPartName)

	bytes, err := d.Offsite.Upload(ctx, obj, c.OffsiteRemote, part)
	if err != nil {
		// The live object was never touched: this write went to the staging
		// name, which nothing reads and the bucket rule will expire if rclone
		// left anything behind.
		return 0, false, fmt.Errorf("offsite upload %s: %w", newest.ID, err)
	}
	if err := d.Offsite.Move(ctx, c.OffsiteRemote, part, live); err != nil {
		// The previous generation is still at the live name — a move that did
		// not happen replaced nothing. Drop the staged copy so a failing pass
		// does not pay for the upload twice over in the bucket, but never the
		// live object: that is the client's only cold copy.
		if delErr := d.Offsite.Delete(ctx, c.OffsiteRemote, part); delErr != nil {
			log.Printf("offsite: client=%q snap=%s: could not remove the staging object %s after a failed move: %v",
				c.Name, newest.ID, part, delErr)
		}
		return 0, false, fmt.Errorf("offsite move %s -> %s: %w", part, live, err)
	}
	if err := d.Store.ReplaceSingleOffsiteObject(ctx, model.OffsiteObject{
		ClientID:   c.ID,
		SnapshotID: newest.ID,
		Remote:     c.OffsiteRemote,
		Bytes:      bytes,
		Checksum:   checksum,
		UploadedAt: d.now(),
	}); err != nil {
		return 0, false, fmt.Errorf("record offsite %s: %w", newest.ID, err)
	}
	log.Printf("offsite: client=%q remote=%s snapshot=%s replaced %s (bytes=%d sha256=%s…)",
		c.Name, c.OffsiteRemote, newest.ID, live, bytes, checksum[:12])
	if d.Verbose {
		log.Printf("offsite: client=%q single generation: the only object is %s", c.Name, live)
	}
	if d.DiscordWebhook != "" {
		go alert.Discord(d.DiscordWebhook, fmt.Sprintf(
			"☁️ **backitup** — `%s` offsite upload complete (single generation: previous copy replaced)\nRemote: %s | snapshot: %s | size: %s",
			c.Name, c.OffsiteRemote, newest.ID, alert.FormatBytes(bytes),
		))
	}
	return bytes, true, nil
}

// singleHeldSnapshot returns the snapshot id the fixed object is recorded as
// holding, and whether that record can be read as "the object holds this
// snapshot's content right now".
//
// Only one row for this remote counts, and only an owning, verified one: a
// client that has just been switched to this mode still has the per-snapshot
// rows of the mode it came from, and none of those says anything about the fixed
// name. A dedupe row names someone else's object, and a row the verification
// pass reported missing names an object the bucket no longer holds. In all three
// cases the answer is "no," and the pass uploads — which is also what makes the
// switch to this mode replace those rows rather than trust them. Rows for a
// different remote are filtered out first: an operator repointing a client leaves
// them behind, and reading them as a second generation would re-upload the same
// snapshot on every pass forever.
func singleHeldSnapshot(objs []model.OffsiteObject, remote string) (string, bool) {
	var rows []model.OffsiteObject
	for _, o := range objs {
		if o.Remote == remote {
			rows = append(rows, o)
		}
	}
	if len(rows) != 1 {
		return "", false
	}
	o := rows[0]
	if o.DedupOf != "" || o.RemoteMissing {
		return "", false
	}
	return o.SnapshotID, true
}

// offsiteSnapshot gets one snapshot into cold storage — by uploading it, or by
// recording that identical content is already there. The staging archive lives
// exactly as long as this call: a deferred remove in the caller's loop kept one
// copy of every snapshot on disk until the whole pass ended (N × snapshot size
// of scratch, on the system temp dir rather than the backup volume).
func offsiteSnapshot(ctx context.Context, d Deps, c model.Client, sm mode.ServerMode, clientDir string, s mode.Snapshot, offsited map[string]bool, root string) (int64, bool, error) {
	obj, err := sm.PrepareOffsite(ctx, clientDir, s)
	if err != nil {
		return 0, false, fmt.Errorf("prepare offsite %s: %w", s.ID, err)
	}
	// rsync builds a temp archive; remove it as soon as this snapshot is done
	// (tar.gz returns the archive in place, which must NOT be removed).
	if c.Mode == model.ModeRsync {
		defer func() { _ = os.Remove(obj) }()
	}

	checksum, err := fileChecksum(obj)
	if err != nil {
		return 0, false, fmt.Errorf("checksum %s: %w", s.ID, err)
	}
	owner, dup, err := d.Store.OffsiteChecksumOwner(ctx, c.ID, c.OffsiteRemote, checksum)
	if err != nil {
		return 0, false, fmt.Errorf("checksum lookup %s: %w", s.ID, err)
	}
	if dup {
		// The content is already in cold storage — but the snapshot still has to
		// be recorded. Without a record it never becomes "offsited", so the hot
		// pruner protects it forever and an unchanged tree grows the hot store by
		// one snapshot per run. The row names the owner's remote object instead of
		// a path of its own, so pruning can never delete a file that was never
		// uploaded, and verification checks the owner's object, not a phantom.
		log.Printf("offsite: client=%q snap=%s: identical content already on remote (sha256=%s…, owner=%s), recorded as dedup",
			c.Name, s.ID, checksum[:12], owner)
		if err := d.Store.RecordOffsiteObject(ctx, model.OffsiteObject{
			ClientID: c.ID, SnapshotID: s.ID, Remote: c.OffsiteRemote, Checksum: checksum, DedupOf: owner,
		}); err != nil {
			return 0, false, fmt.Errorf("record offsite %s: %w", s.ID, err)
		}
		offsited[s.ID] = true
		return 0, false, nil
	}

	bytes, err := d.Offsite.Upload(ctx, obj, c.OffsiteRemote, objectPathFor(root, c, s.ID))
	if err != nil {
		return 0, false, fmt.Errorf("offsite upload %s: %w", s.ID, err)
	}
	if d.Verbose {
		log.Printf("offsite: client=%q remote=%s snapshot=%s uploaded bytes=%d sha256=%s…", c.Name, c.OffsiteRemote, s.ID, bytes, checksum[:12])
	}
	if d.DiscordWebhook != "" {
		go alert.Discord(d.DiscordWebhook, fmt.Sprintf(
			"☁️ **backitup** — `%s` offsite upload complete\nRemote: %s | snapshot: %s | size: %s",
			c.Name, c.OffsiteRemote, s.ID, alert.FormatBytes(bytes),
		))
	}
	if err := d.Store.RecordOffsiteObject(ctx, model.OffsiteObject{
		ClientID: c.ID, SnapshotID: s.ID, Remote: c.OffsiteRemote, Bytes: bytes, Checksum: checksum,
	}); err != nil {
		return 0, false, fmt.Errorf("record offsite %s: %w", s.ID, err)
	}
	offsited[s.ID] = true
	return bytes, true, nil
}

// prunePartOrphans removes .tar.gz.part files older than PartOrphanAge from
// clientDir. These are left by interrupted tar.gz uploads (backitup-recv writes
// to a .part then renames on success). An in-progress upload continuously
// updates mtime, so any .part file whose mtime is older than the threshold is
// safe to remove.
func prunePartOrphans(d Deps, clientDir, clientName string) {
	age := d.PartOrphanAge
	if age == 0 {
		age = DefaultPartOrphanAge
	}
	cutoff := d.now().Add(-age)

	entries, err := os.ReadDir(clientDir)
	if err != nil {
		return // directory may not exist yet; not an error
	}
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".tar.gz.part") {
			continue
		}
		info, err := e.Info()
		if err != nil {
			continue
		}
		if info.ModTime().After(cutoff) {
			continue // recently touched — upload may still be in progress
		}
		path := filepath.Join(clientDir, e.Name())
		if err := os.Remove(path); err != nil {
			log.Printf("lifecycle: client=%q: remove stale part file %s: %v", clientName, e.Name(), err)
		} else {
			log.Printf("lifecycle: client=%q: removed stale part file %s (last modified %s ago)",
				clientName, e.Name(), d.now().Sub(info.ModTime()).Round(time.Second))
		}
	}
}

// pruneIncomplete removes the snapshot directories that are not backups.
//
// There are two kinds, and they need different evidence:
//
//   - Marked directories. A failed rsync run writes IncompleteMarker into its own
//     destination and the client survives to do it. These are deliberately absent
//     from List, so iterating a listing would find nothing and leave the directory
//     on the volume forever; the mode is asked directly (ListIncomplete), which is
//     also why the interface has that method at all. Age is the marker's mtime.
//   - Unbacked directories. A run that was killed before it could report never
//     marks anything, so the server works it out from the run row it opened and
//     that never reached ok (mode.UnbackedSnapshots, via VisibleSnapshots). The
//     caller has already removed them from its listing; this is the only thing
//     that will ever delete them. Age is when the run was recorded as not-ok, so
//     a run that was only just declared dead still gets the full grace.
//
// Every one of them is logged, every pass, with why it is not a backup: "a
// directory disappeared from the backup volume" is not something an operator
// should have to reconstruct from the absence of a snapshot.
func pruneIncomplete(ctx context.Context, d Deps, c model.Client, sm mode.ServerMode, clientDir string, unbacked []mode.UnbackedSnapshot) {
	cutoff := d.now().Add(-DefaultIncompleteAge)

	snaps, err := sm.ListIncomplete(ctx, clientDir)
	if err != nil {
		log.Printf("lifecycle: client %d (%s): list incomplete snapshots: %v", c.ID, c.Name, err)
	}
	for _, s := range snaps {
		if s.CreatedAt.After(cutoff) {
			// Marked a moment ago: possibly a second run writing into the same
			// second's directory. The next pass takes it.
			continue
		}
		if delErr := sm.DeleteSnapshot(ctx, clientDir, s.ID); delErr != nil {
			log.Printf("lifecycle: client %d (%s): remove incomplete snapshot %s: %v", c.ID, c.Name, s.ID, delErr)
			continue
		}
		log.Printf("lifecycle: client %d (%s): removed incomplete snapshot %s left by a failed run on %s",
			c.ID, c.Name, s.ID, s.CreatedAt.UTC().Format(time.RFC3339))
	}

	for _, u := range unbacked {
		id := u.Snapshot.ID
		if u.Since.After(cutoff) {
			// Hide first, delete later: the directory is out of every listing
			// and every offsite pass from now on, and the grace gives a run that
			// was declared dead by a safety net (rather than by its own report)
			// the same benefit of the doubt a marked directory gets.
			log.Printf("lifecycle: client %d (%s): snapshot %s is not a backup (%s) — hidden from the listing and from offsite; reclaiming after %s",
				c.ID, c.Name, id, u.Reason, DefaultIncompleteAge)
			continue
		}
		if delErr := sm.DeleteSnapshot(ctx, clientDir, id); delErr != nil {
			log.Printf("lifecycle: client %d (%s): remove incomplete snapshot %s (run %d never reached ok): %v",
				c.ID, c.Name, id, u.RunID, delErr)
			continue
		}
		log.Printf("lifecycle: client %d (%s): removed incomplete snapshot %s left by a run that never reached ok (%s)",
			c.ID, c.Name, id, u.Reason)
	}
}

// pruneHot deletes hot snapshots older than retention, never the newest, and —
// when offsite is enabled — only snapshots strictly older than the newest
// snapshot already confirmed offsite (offsite-first, D8).
//
// The offsite guard is a property of the CLIENT's offsite state, not of each
// snapshot's own record. It used to be the latter: every snapshot without an
// offsite record of itself was kept. That cannot work for a client on
// offsite_upload_mode=latest, where only one snapshot in the whole listing ever
// has a record of its own, and it turns a run of failing uploads into "retention
// never fires again" — the hot store fills while nothing new is protected.
//
// The rule is instead: find the newest snapshot (by CreatedAt, ID as tiebreak)
// whose id is in offsited, and delete a snapshot only when it is strictly older
// than that one. Deleting an old copy is safe because a strictly newer copy
// exists somewhere; anything at or newer than the newest offsite copy is kept,
// because nothing newer proves the content survives off the hot store.
//
// Two consequences, both deliberate:
//
//   - No offsited snapshot at all means no permission to delete anything, as
//     before. Nothing is known to be off the hot store, so nothing is offered.
//   - While offsite is stale or broken the hot store keeps everything newer than
//     the last good offsite copy and therefore grows. That growth plus the log
//     line is how an offsite outage becomes visible; the alternative — treating
//     the absence of a copy elsewhere as permission to delete — quietly removes
//     the only copies.
//
// The age horizon still applies on top: this rule narrows what retention_days
// allows, it never widens it. A snapshot inside the hot horizon is kept whatever
// the offsite state says.
//
// "Never the newest" is decided from the snapshots, not from the slice order:
// the newest is the one with the greatest CreatedAt, with the ID breaking ties.
// Trusting index 0 made the guarantee depend on the caller sorting newest-first,
// and when a listing arrived oldest-first (os.ReadDir is filename-ascending, and
// snapshot IDs begin with the timestamp) it protected the OLDEST snapshot and
// offered the newest one for deletion. The order is now fixed at the source as
// well (mode.SortNewestFirst in each mode's List); this is the second lock on the
// same door, because a silent regression in either place deletes the wrong end
// of the list.
//
// It reports what happened: how many snapshots it deleted, how many deletions
// failed, and the first failure. Returning nothing made a pass that deleted
// nothing at all indistinguishable from a pass with nothing to do — which is how
// a permanently failing prune stayed invisible while the hot store filled up.
//
// A disabled hot horizon is reported because it means no pruning every pass,
// forever, with no line explaining it.
func pruneHot(ctx context.Context, d Deps, c model.Client, sm mode.ServerMode, clientDir string, snaps []mode.Snapshot, offsiteOn bool, offsited map[string]bool) (deleted, failed int, err error) {
	if c.RetentionDays <= 0 {
		log.Printf("lifecycle: hot retention disabled for client %q (retention_days=%d): no snapshots will be pruned", c.Name, c.RetentionDays)
		return 0, 0, nil
	}
	if len(snaps) <= 1 {
		return 0, 0, nil
	}
	if offsiteOn && c.OffsiteUploadMode == model.OffsiteUploadSingle {
		// A single-generation client has exactly one cold copy, and that copy is
		// current only as of its last SUCCESSFUL upload. The freshness rule below
		// ("delete what is strictly older than the newest offsite snapshot") reads
		// the row, and the row survives a failed upload unchanged — it still names
		// the snapshot the fixed object holds, which may be days older than
		// anything this pass is looking at. Deleting hot snapshots older than a
		// copy that the last run failed to update would leave one generation in
		// cold storage and no way to tell how stale it is, so a client whose last
		// upload did not succeed is pruned by nothing until one does.
		if why, stale := singleCopyStale(ctx, d, c); stale {
			log.Printf("lifecycle: client %d (%s): keeping all %d snapshots: %s", c.ID, c.Name, len(snaps), why)
			return 0, 0, nil
		}
	}
	cutoff := d.now().Add(-time.Duration(c.RetentionDays) * 24 * time.Hour)
	newest, ok := newestSnapshot(snaps)
	// The freshest evidence that this client's content is off the hot store.
	// "Where is the newest offsite copy" is asked of the snapshots on disk, not
	// of the offsite table: a record for a snapshot that is already gone from
	// here says nothing about which of the remaining ones may be dropped, and
	// using it would let a stale, newer-looking row justify deleting the only
	// copies younger than it.
	newestOffsite, haveOffsite := newestOffsiteSnapshot(snaps, offsited)
	var kept []string
	for _, s := range snaps {
		if ok && s.ID == newest.ID {
			continue // protect the newest, whatever position it is in
		}
		if !s.CreatedAt.Before(cutoff) {
			continue // within the hot horizon
		}
		if offsiteOn && (!haveOffsite || !strictlyOlder(s, newestOffsite)) {
			kept = append(kept, s.ID)
			continue
		}
		if delErr := sm.DeleteSnapshot(ctx, clientDir, s.ID); delErr != nil {
			failed++
			log.Printf("lifecycle: client %d: prune %s: %v", c.ID, s.ID, delErr)
			if err == nil {
				err = fmt.Errorf("prune hot snapshot %s: %w", s.ID, delErr)
			}
			continue
		}
		deleted++
		if offsiteOn {
			log.Printf("lifecycle: client %d (%s): pruning %s: older than the newest offsite copy %s (%s)",
				c.ID, c.Name, s.ID, newestOffsite.ID, newestOffsite.CreatedAt.UTC().Format(time.RFC3339))
			continue
		}
		log.Printf("lifecycle: client %d (%s): pruning %s: older than the %d-day hot horizon",
			c.ID, c.Name, s.ID, c.RetentionDays)
	}
	// One line per pass, with a count when several were held back: "why was
	// nothing pruned" has to be answerable from the log, and a client with dozens
	// of snapshots would otherwise print dozens of near-identical lines.
	if len(kept) > 0 {
		what := kept[0]
		if n := len(kept); n > 1 {
			what = fmt.Sprintf("%d snapshots", n)
		}
		if haveOffsite {
			log.Printf("lifecycle: client %d (%s): keeping %s: not older than the newest offsite copy %s (%s)",
				c.ID, c.Name, what, newestOffsite.ID, newestOffsite.CreatedAt.UTC().Format(time.RFC3339))
		} else {
			log.Printf("lifecycle: client %d (%s): keeping %s: nothing is offsited for this client, so there is no copy elsewhere to fall back on",
				c.ID, c.Name, what)
		}
	}
	return deleted, failed, err
}

// singleCopyStale reports whether a single-generation client's one cold copy
// must be treated as not current, and why.
//
// The evidence is the offsite run history, not the object row: a failed upload
// leaves the row — and the object behind it — exactly as the last successful
// upload left them, so the row cannot say whether the copy is up to date. Only a
// finished, successful run says "the fixed object holds the newest snapshot as
// of that moment".
//
// No run history at all falls through to the row: a client whose rows predate
// the history (or whose history was pruned) is judged by the freshness rule like
// any other mode, because refusing to prune forever over missing bookkeeping is
// a retention stall, not a safety property.
func singleCopyStale(ctx context.Context, d Deps, c model.Client) (reason string, stale bool) {
	runs, err := d.Store.ListOffsiteRuns(ctx, c.ID, 1)
	if err != nil {
		return fmt.Sprintf("the offsite run history could not be read (%v), so there is no evidence the one cold copy is current", err), true
	}
	if len(runs) == 0 {
		return "", false
	}
	switch runs[0].Status {
	case "ok":
		return "", false
	case "running":
		return "an offsite upload is still running, so the fixed object may be mid-replacement", true
	default:
		detail := strings.TrimSpace(runs[0].ErrorText)
		if i := strings.IndexByte(detail, '\n'); i >= 0 {
			detail = detail[:i]
		}
		if len(detail) > 160 {
			detail = detail[:160] + "…"
		}
		if detail != "" {
			detail = ": " + detail
		}
		return "the last offsite upload failed (" + runs[0].Status + detail + "), so the one cold copy may not hold a snapshot " +
			"newer than what is on the hot store; nothing is pruned until an upload succeeds", true
	}
}

// newestOffsiteSnapshot returns the newest snapshot in snaps whose id is in
// offsited — the copy elsewhere that permits deleting anything strictly older.
// ok is false when nothing in the listing is offsited, which is the "keep
// everything" case.
//
// Only the snapshots on disk are considered: the offsite table also holds rows
// for snapshots already pruned from here, and those must not be read as a newer
// copy than the ones still present.
func newestOffsiteSnapshot(snaps []mode.Snapshot, offsited map[string]bool) (mode.Snapshot, bool) {
	var newest mode.Snapshot
	found := false
	for _, s := range snaps {
		if !offsited[s.ID] {
			continue
		}
		if !found || strictlyOlder(newest, s) {
			newest = s
			found = true
		}
	}
	return newest, found
}

// strictlyOlder reports whether a is strictly older than b, by CreatedAt with
// the ID breaking ties — the same ordering newestSnapshot applies, so "the
// newest" and "older than the newest" cannot disagree about the pair.
func strictlyOlder(a, b mode.Snapshot) bool {
	if !a.CreatedAt.Equal(b.CreatedAt) {
		return a.CreatedAt.Before(b.CreatedAt)
	}
	return a.ID < b.ID
}

// newestSnapshot returns the snapshot that must never be pruned: the one with
// the greatest CreatedAt, with the ID as tie-break so the answer is a property
// of the set rather than of its order. ok is false for an empty slice.
func newestSnapshot(snaps []mode.Snapshot) (mode.Snapshot, bool) {
	if len(snaps) == 0 {
		return mode.Snapshot{}, false
	}
	newest := snaps[0]
	for _, s := range snaps[1:] {
		if s.CreatedAt.After(newest.CreatedAt) ||
			(s.CreatedAt.Equal(newest.CreatedAt) && s.ID > newest.ID) {
			newest = s
		}
	}
	return newest, true
}

// verifyLatest integrity-checks the most recent snapshot (D9). What "intact"
// means is the mode's business (mode.ServerMode.VerifySnapshot): tar.gz is read
// so gzip/CRC damage surfaces, and rsync — whose snapshots report Bytes: 0 by
// design, because summing a hardlinked tree is not free — is checked by a sample
// of its directory instead.
//
// Nothing here compares sizes. A mode reporting Bytes: 0 is not a mode with an
// empty snapshot, and treating it as one logged "INTEGRITY WARN" on every pass
// for every rsync client, which made the signal that does matter unreadable.
func verifyLatest(ctx context.Context, c model.Client, sm mode.ServerMode, clientDir string, snaps []mode.Snapshot) {
	latest, ok := newestSnapshot(snaps)
	if !ok {
		return
	}
	if err := sm.VerifySnapshot(ctx, clientDir, latest); err != nil {
		log.Printf("lifecycle: client %d: INTEGRITY FAIL on %s: %v", c.ID, latest.ID, err)
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
	// Age the alert off the last SUCCESSFUL run, not the newest row: a client
	// posting nothing but overlaps for a week is stale, not healthy (S8).
	lastOK, err := d.Store.LastSuccessfulRun(ctx, c.ID)
	if err != nil {
		return
	}
	h := model.DeriveHealth(run, lastOK, time.Duration(c.ExpectedIntervalSecs)*time.Second, d.now())
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
	return c.Slug
}

// offsiteTarget resolves where a client's offsite objects live, and whether the
// remote may be used at all.
//
// root is the path prefix inside the remote — the B2 bucket plus the remote's
// optional prefix (model.Remote.RootPath). A client whose remote has no row (a
// section the operator wrote in rclone.conf by hand) keeps root "" and the path
// it had before buckets moved onto the remote.
//
// unsupported is true for a remote an older backitup configured with a backend
// this build dropped (s3, drive, sftp, …). Those rows are kept so the operator's
// configuration is not deleted behind their back, but nothing uses them: the
// lifecycle skips the client and the UI says to recreate the remote as B2.
func offsiteTarget(ctx context.Context, d Deps, c model.Client) (root string, unsupported bool, err error) {
	if c.OffsiteRemote == "" {
		return "", false, nil
	}
	r, err := d.Store.GetRemoteByName(ctx, c.OffsiteRemote)
	if err != nil {
		return "", false, err
	}
	if r == nil {
		return "", false, nil
	}
	return r.RootPath(), r.Unsupported, nil
}

// logUnsupportedRemote is the one line an operator gets when a client still
// points at a remote this build dropped. It names both the client and the
// remote, because the fix is per-remote (recreate it as B2, then repoint the
// client) and the log is where an operator looks when offsite stops happening.
func logUnsupportedRemote(worker string, c model.Client) {
	log.Printf("%s: client %d (%s): offsite remote %q has an unsupported remote type — "+
		"offsite storage is Backblaze B2 only; recreate the remote as B2 and skip this client until then",
		worker, c.ID, c.Name, c.OffsiteRemote)
}

// verifyOffsiteObjects calls rclone lsf for the client's remote directory and
// updates the remote_missing / remote_verified_at fields on each offsite_objects
// row, then reconciles the dedupe records against what that listing showed.
//
// Throttled to once per hour per client: if the oldest verified_at is within the
// last hour, the check is skipped. force bypasses the throttle for the callers
// that are about to *act* on the answer — the hot pruner (which deletes on the
// strength of "there is a copy elsewhere") and the uploader (which skips an
// upload on the strength of the same claim).
func verifyOffsiteObjects(ctx context.Context, d Deps, c model.Client, root string, force bool) {
	all, err := d.Store.ListOffsiteObjects(ctx, c.ID)
	if err != nil {
		return
	}
	// Deduplicated snapshots share an object owned by another snapshot: verifying
	// the owner covers them, and counting them here would also keep the hourly
	// throttle below permanently tripped (a dedup row is never verified itself).
	objects := make([]model.OffsiteObject, 0, len(all))
	for _, o := range all {
		if o.DedupOf == "" {
			objects = append(objects, o)
		}
	}
	if len(objects) == 0 {
		return
	}

	// Skip if all objects have been checked within the last hour.
	if !force {
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
	}

	dir := offsiteDir(c)
	lsfCtx, cancel := context.WithTimeout(ctx, 30*time.Second)
	files, err := d.Offsite.Lsf(lsfCtx, c.OffsiteRemote, remoteDir(root, dir))
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
	missingOwners := make(map[string]bool, len(objects))
	for _, o := range objects {
		missing := !fileSet[objectNameFor(c, o.SnapshotID)]
		if err := d.Store.UpdateOffsiteRemoteStatus(ctx, o.ID, missing, now); err != nil {
			log.Printf("lifecycle: offsite verify: update: %v", err)
			continue
		}
		if missing {
			missingOwners[o.SnapshotID] = true
			log.Printf("lifecycle: offsite verify: client=%s snap=%s object %s missing from %s",
				c.Name, o.SnapshotID, objectNameFor(c, o.SnapshotID), c.OffsiteRemote)
		}
	}
	flagGoneDedupOwners(ctx, d, c, all, missingOwners, now)
}

// flagGoneDedupOwners marks the dedupe rows whose owner object the listing just
// reported gone, which is what stops them claiming an offsite copy that does not
// exist.
//
// A dedupe row says "my content lives in that object". Once the bucket's
// lifecycle rule expires that object the claim is false, and it is the dangerous
// kind of false: offsitedSet reads it as "a copy exists elsewhere", so pruneHot
// deletes the hot snapshot that is now the only one. Marking the dependent row
// missing makes the claim untrue in the same place both the pruner and the
// uploader read it: the next upload pass sees an un-offsited snapshot and
// re-uploads it (a re-upload writes a real object of its own, which clears the
// flag — see store.RecordOffsiteObject).
//
// The line is emitted once per client per pass, and only on the transition: an
// unchanged tree can have dozens of rows pointing at one expired object, and a
// re-upload that cannot happen (the client was removed, the directory is gone)
// must not re-announce itself every pass. In that case the hot copy simply
// stays — the only-copy rule wins over retention.
func flagGoneDedupOwners(ctx context.Context, d Deps, c model.Client, all []model.OffsiteObject, missingOwners map[string]bool, now time.Time) {
	if len(missingOwners) == 0 {
		return
	}
	var gone []model.OffsiteObject
	for _, o := range all {
		if o.DedupOf == "" || o.RemoteMissing || !missingOwners[o.DedupOf] {
			continue
		}
		if err := d.Store.UpdateOffsiteRemoteStatus(ctx, o.ID, true, now); err != nil {
			log.Printf("lifecycle: offsite verify: flag %s as needing a re-upload: %v", o.SnapshotID, err)
			continue
		}
		gone = append(gone, o)
	}
	if len(gone) == 0 {
		return
	}
	first := gone[0]
	more := ""
	if n := len(gone) - 1; n > 0 {
		more = fmt.Sprintf(" (and %d more run(s) deduplicated against the same object)", n)
	}
	log.Printf("lifecycle: offsite object %s referenced by run %s is gone (expired by the bucket lifecycle rule): re-uploading%s",
		objectNameFor(c, first.DedupOf), objectNameFor(c, first.SnapshotID), more)
}

// offsiteDue returns true when the client's cron schedule has a tick between
// the last upload and now. Always true if no upload has happened yet.
func offsiteDue(ctx context.Context, d Deps, c model.Client) (bool, error) {
	sched, err := cron.ParseStandard(c.OffsiteCron)
	if err != nil {
		return false, fmt.Errorf("parse cron %q: %w", c.OffsiteCron, err)
	}
	last, err := d.Store.LatestOffsite(ctx, c.ID)
	if err != nil {
		return false, err
	}
	if last == nil {
		return true, nil
	}
	// Next tick after the last upload; if it's in the past, we're due.
	return !sched.Next(*last).After(d.now()), nil
}

// OffsiteClient runs an immediate offsite upload + prune pass for a single
// client, bypassing the per-client interval check. Safe to call concurrently
// with the background worker because all state mutations go through the store.
// Uses a two-step Start/Finish so the dashboard can show upload progress.
func OffsiteClient(ctx context.Context, d Deps, c model.Client) error {
	if d.Offsite == nil || c.OffsiteRemote == "" {
		return fmt.Errorf("client %q has no offsite remote configured", c.Name)
	}
	root, unsupported, err := offsiteTarget(ctx, d, c)
	if err != nil {
		return fmt.Errorf("resolve offsite remote: %w", err)
	}
	if unsupported {
		logUnsupportedRemote("offsite run", c)
		return fmt.Errorf("offsite remote %q has an unsupported remote type — "+
			"offsite storage is Backblaze B2 only; recreate it as B2 first", c.OffsiteRemote)
	}
	sm, ok := mode.Server(c.Mode)
	if !ok {
		return fmt.Errorf("no server mode for %q", c.Mode)
	}
	clientDir, err := d.clientDir(c)
	if err != nil {
		return err
	}
	snaps, err := sm.List(ctx, clientDir)
	if err != nil {
		return fmt.Errorf("list snapshots: %w", err)
	}
	// Same rule as uploadClient: an adhoc "Backup now" must not upload a
	// directory the run history says is not a backup either.
	snaps, _, err = VisibleSnapshots(ctx, d.Store, c, sm, snaps)
	if err != nil {
		return fmt.Errorf("verify snapshots against the run history: %w", err)
	}
	mode.SortNewestFirst(snaps)

	// Same reason as uploadClient: this pass is about to decide what is already
	// on the remote, so it looks now rather than trusting the last hour's answer.
	verifyOffsiteObjects(ctx, d, c, root, true)

	objs, err := d.Store.ListOffsiteObjects(ctx, c.ID)
	if err != nil {
		return err
	}

	// Insert a "running" row immediately so the dashboard can show progress.
	// Pruning is the lifecycle worker's job; the adhoc trigger only uploads.
	runID, startErr := d.Store.StartOffsiteRun(ctx, c.ID, "adhoc", d.now())
	snapsUploaded, bytesUploaded, uploadErr := offsiteNewSnapshots(ctx, d, c, sm, clientDir, snaps, objs, root)
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

// remoteDir returns the directory inside a remote that holds a client's
// objects: "<root>/<dir>", each part omitted when empty. It is what verify
// lists with rclone lsf. model.ObjectDir owns the rule, so this listing and the
// upload path (and the download/delete routes in the server) always address the
// same place — including dropping a client directory that only repeats the
// bucket.
func remoteDir(root, dir string) string {
	return model.ObjectDir(root, dir)
}

// objectNameFor is the file name one snapshot has inside the remote directory,
// for this client's upload mode: its own name in the immutable modes, the fixed
// name in single-generation mode. model.OffsiteObjectName owns the rule so the
// verification pass and the server's download route cannot drift from it.
func objectNameFor(c model.Client, snapshotID string) string {
	return model.OffsiteObjectName(c.Mode, c.OffsiteUploadMode, snapshotID)
}

// objectPathFor returns the path of one snapshot's offsite object:
// "<root>/<dir>/<file>", where root is the bucket (and optional prefix) the
// remote was configured with — "" for a remote backitup has no row for, which
// keeps the old path.
func objectPathFor(root string, c model.Client, snapshotID string) string {
	return model.ObjectPath(root, offsiteDir(c), objectNameFor(c, snapshotID))
}

// fileChecksum returns the SHA-256 hex digest of the file at path.
func fileChecksum(path string) (string, error) {
	f, err := os.Open(path)
	if err != nil {
		return "", err
	}
	defer f.Close()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return fmt.Sprintf("%x", h.Sum(nil)), nil
}
