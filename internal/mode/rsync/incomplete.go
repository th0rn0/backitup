package rsync

import (
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/th0rn0/backitup/internal/mode"
)

// IncompleteMarker is the file a failed rsync run writes into the snapshot
// directory it left behind, and the only thing that tells a partially
// transferred tree apart from a real snapshot.
//
// A failed run cannot delete its own destination: the client talks to the
// server through rrsync, which confines it to rsync transfers inside the
// client's directory and offers no way to remove a directory (rsync empties a
// destination, it never unlinks the destination itself). So a failure leaves
// snapshots/<ts>/ on the server holding whatever rsync had managed to copy —
// with exit 23 on a permission error that is four zero-byte files.
//
// Left unmarked, that directory is indistinguishable from a backup:
//
//   - List returns any directory that is not `latest`, so the offsite worker
//     tars it and uploads four empty files to B2 as this client's backup;
//   - VerifySnapshot's shape test ("present, at least one entry") passes,
//     because four empty files are entries;
//   - retention counts it as a snapshot, so the hot horizon is measured from
//     a run that never produced one.
//
// The marker fixes that at the source: the client writes it after the sync
// fails, List and VerifySnapshot refuse a marked directory, the lifecycle
// reclaims it (see lifecycle.pruneIncomplete), and nothing downstream ever
// treats it as a backup. Absence of the marker means "normal", which is why
// snapshots written by earlier versions keep working untouched.
//
// The name is deliberately dot-prefixed and outside rsync's temp-file pattern:
// it is a plain file the client copies in with one rsync of its own.
const IncompleteMarker = ".backitup-partial"

// markerReadLimit bounds how much of the marker is read back. The run's error
// text is truncated on the way in (markerMaxReason), so this is a backstop
// against a hand-edited or corrupt file, not the usual path.
const markerReadLimit = 8 << 10

// markerMaxReason is how much of the failed run's error the marker keeps. rsync
// prints one stderr line per unreadable file, so a large source can produce
// thousands of them; the marker is read by the lifecycle's log line and by
// VerifySnapshot's error, neither of which wants the whole list.
const markerMaxReason = 600

// incompleteReasonText is the marker's body: why this directory is not a
// backup, in the operator's terms.
//
// The counts are the ones rsync itself reported for the attempt (they are the
// same "Number of files" line the real log showed), stated as what rsync
// counted rather than as what was stored — a failed run must not be readable as
// "synced 4 files".
func incompleteReasonText(stdout string, runErr error) string {
	files, bytesN := parseStats(stdout)
	errText := oneLine(runErr.Error(), markerMaxReason)
	var b strings.Builder
	b.WriteString("# backitup: this snapshot directory is NOT a usable backup.\n")
	b.WriteString("# The rsync run that created it failed, so the tree holds whatever\n")
	b.WriteString("# rsync had copied when it stopped. backitup ignores a directory that\n")
	b.WriteString("# carries this file and removes it on a later lifecycle pass.\n")
	fmt.Fprintf(&b, "run failed: %s\n", errText)
	fmt.Fprintf(&b, "finished at: %s\n", time.Now().UTC().Format(time.RFC3339))
	fmt.Fprintf(&b, "files in rsync's file list: %d\n", files)
	fmt.Fprintf(&b, "bytes in rsync's file list: %d\n", bytesN)
	return b.String()
}

// oneLine flattens multi-line text to a single line and bounds its length. A
// marker line has to survive being logged verbatim.
func oneLine(s string, limit int) string {
	s = strings.TrimSpace(s)
	if len(s) > limit {
		s = s[:limit] + "…"
	}
	return strings.Join(strings.Fields(s), " ")
}

// markIncomplete writes the marker into the snapshot directory the failed run
// left on the server.
//
// It is a second rsync (a single small file) rather than a server-side command
// because the client has no shell on the ingest host — rrsync is the only thing
// its key may run. The destination is created by that rsync only if the failed
// run had not created it yet, and a failure that never reached the server
// (refused connection, bad key) therefore leaves no directory behind at all.
func markIncomplete(ctx context.Context, o mode.BackupOpts, host, sshArgs, snap, reason string) error {
	stage, err := os.MkdirTemp("", "backitup-partial-")
	if err != nil {
		return err
	}
	defer os.RemoveAll(stage)
	local := filepath.Join(stage, IncompleteMarker)
	if err := os.WriteFile(local, []byte(reason), 0o600); err != nil {
		return err
	}
	target := fmt.Sprintf("%s@%s:snapshots/%s/", o.SSHUser, host, snap)
	_, err = runRsync(ctx, o.Log(), []string{"-a", "-e", sshArgs, local, target})
	return err
}

// incompleteReason reports whether dir was left by a failed run, and what the
// run's error was. An unreadable marker still counts as a marker: the directory
// was declared incomplete, and that is a fact about the backup rather than a
// reason to go on reporting it as one.
func incompleteReason(dir string) (string, bool) {
	path := filepath.Join(dir, IncompleteMarker)
	if _, err := os.Lstat(path); err != nil {
		return "", false
	}
	f, err := os.Open(path)
	if err != nil {
		return "marked incomplete by a failed run (marker unreadable: " + err.Error() + ")", true
	}
	defer f.Close()
	raw, _ := io.ReadAll(io.LimitReader(f, markerReadLimit))
	for _, line := range strings.Split(string(raw), "\n") {
		if reason, ok := strings.CutPrefix(strings.TrimSpace(line), "run failed: "); ok {
			return oneLine(reason, markerMaxReason), true
		}
	}
	return "marked incomplete by a failed run", true
}

// incompleteSince returns when the failure was recorded: the marker's own
// mtime, which the client sets when it writes it. It is the age the lifecycle
// uses to decide that the directory is no longer being written to (a retry
// always targets a new timestamp, so nothing ever completes a marked one).
func incompleteSince(dir string) (time.Time, bool) {
	info, err := os.Stat(filepath.Join(dir, IncompleteMarker))
	if err != nil {
		return time.Time{}, false
	}
	return info.ModTime(), true
}
