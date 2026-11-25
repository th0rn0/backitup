package rsync

// The invocation a client sends is the contract between the settings an operator
// ticks on the client page and what a snapshot actually contains. These tests
// pin the whole argv — every fidelity flag together with group_readable — because
// a flag that is silently dropped (or added twice) is a backup that looks fine and
// is not the one that was asked for.

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/th0rn0/backitup/internal/mode"
)

// every setting on at once: the fidelity flags in table order, then the
// group-readable chmod, then --link-dest, then the debug switch, the symlink
// exclusion, the exclude list, and finally the transport and the destination.
func TestSnapshotArgsWithEverySetting(t *testing.T) {
	opts := mode.BackupOpts{
		SourceDir:           "/src",
		Excludes:            []string{"*.tmp", "node_modules/"},
		SkipSymlinks:        true,
		PreserveSparse:      true,
		PreserveHardlinks:   true,
		PreserveACLs:        true,
		PreserveXattrs:      true,
		HasPreviousSnapshot: true,
		GroupReadable:       true,
		SSHServer:           "h:22",
		SSHUser:             "backitup",
	}
	t.Setenv("BACKITUP_RSYNC_DEBUG", "1")
	got := snapshotArgs(opts, "ssh -i /k", "backitup@h:snapshots/ts/")

	want := []string{
		"-a", "--numeric-ids", "--delete", "--stats",
		"-S", "-H", "-A", "-X", // fidelity, in table order
		"--chmod=Dg+rx,Fg+r", // group_readable
		"--link-dest=/snapshots/latest",
		"-vvv",       // BACKITUP_RSYNC_DEBUG=1
		"--no-links", // SkipSymlinks
		"--exclude=*.tmp",
		"--exclude=node_modules/",
		"-e", "ssh -i /k",
		"/src/", "backitup@h:snapshots/ts/",
	}
	if strings.Join(got, "\x00") != strings.Join(want, "\x00") {
		t.Fatalf("argv = %q,\nwant   %q", got, want)
	}

	// The destination must be the last word and the -e value must sit
	// immediately before the source: rsync reads everything after -e as the
	// remote command, so an option appended past it is a transfer that fails
	// on a host that is perfectly fine.
	if got[len(got)-1] != "backitup@h:snapshots/ts/" {
		t.Errorf("last argument = %q, want the destination", got[len(got)-1])
	}
	if got[len(got)-4] != "-e" {
		t.Errorf("-e is at position %d of %d; the source and destination must follow it", len(got)-4, len(got))
	}
	// Same shape with every setting off: the invocation a client created before
	// these settings existed sends.
	t.Setenv("BACKITUP_RSYNC_DEBUG", "")
	off := snapshotArgs(mode.BackupOpts{SourceDir: "/src"}, "ssh -i /k", "backitup@h:snapshots/ts/")
	wantOff := []string{"-a", "--numeric-ids", "--delete", "--stats", "-e", "ssh -i /k", "/src/", "backitup@h:snapshots/ts/"}
	if strings.Join(off, "\x00") != strings.Join(wantOff, "\x00") {
		t.Fatalf("with every setting off argv = %q, want %q", off, wantOff)
	}
}

// A receiver can refuse an option that is not one of our settings (an older
// rrsync, a hardened build). The error must still say the receiver refused it
// and where to look, rather than leaving rsync's "protocol data stream" shrug as
// the only clue.
func TestRefusedOptionThatIsNotASetting(t *testing.T) {
	const said = "rrsync error: option -o has been disabled on this server."
	got := refusedFidelityOption(said)
	if got == "" {
		t.Fatal("a refused option produced no explanation")
	}
	if !strings.Contains(got, "-o") {
		t.Errorf("explanation does not name the refused flag: %q", got)
	}
	if !strings.Contains(got, "fidelity setting") {
		t.Errorf("explanation does not point at the settings: %q", got)
	}
	// Text that mentions no refusal at all must not be dressed up as one.
	if got := refusedFidelityOption("rsync: connection unexpectedly closed"); got != "" {
		t.Errorf("a non-refusal was explained as a refused option: %q", got)
	}
}

// A failed run normally marks the directory it left behind. When even that
// fails, the run has to fail louder: the directory is then one List would report
// and the offsite worker would upload, so the operator must be told to look at it
// and remove it by hand — not just that "the backup failed".
func TestFailedRunSaysWhenTheSnapshotCouldNotBeMarked(t *testing.T) {
	requireRsync(t)
	if os.Geteuid() == 0 {
		t.Skip("root ignores directory permissions; the marker write this test needs to fail would succeed")
	}

	server := t.TempDir()
	fakeSSH(t, server)
	// Make the marker unwritable where it has to land: snapshots/ itself. The
	// sync fails (it cannot create its destination) and so does the marker.
	snapshots := filepath.Join(server, "snapshots")
	if err := os.Chmod(snapshots, 0o555); err != nil {
		t.Fatalf("chmod snapshots: %v", err)
	}
	t.Cleanup(func() { _ = os.Chmod(snapshots, 0o755) })

	src := t.TempDir()
	if err := os.WriteFile(filepath.Join(src, "f.txt"), []byte("x"), 0o644); err != nil {
		t.Fatalf("write source: %v", err)
	}

	opts, buf := backupOpts(t, src, false)
	_, err := Mode{}.Backup(context.Background(), opts)
	if err == nil {
		t.Fatal("a run that could not write to the server reported success")
	}
	// The error must not read as an ordinary failed run: it has to say the
	// directory is still there, unmarked, and may be treated as a backup.
	for _, want := range []string{"could NOT be marked incomplete", "may be listed and offsited as a backup"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error does not say %q: %v", want, err)
		}
	}
	if !strings.Contains(buf.String(), "INCOMPLETE SNAPSHOT NOT MARKED") {
		t.Errorf("the run log does not flag the unmarked directory:\n%s", buf.String())
	}
	if !strings.Contains(buf.String(), "run failed") {
		t.Errorf("the run log does not say the run failed:\n%s", buf.String())
	}
}

// The marker helper itself: a reason that is several lines long is flattened to
// one line and bounded, because it is logged verbatim by the lifecycle and read
// back by VerifySnapshot.
func TestMarkerReasonIsOneBoundedLine(t *testing.T) {
	long := strings.Repeat("rsync: send_files failed to open /x\n", 200)
	line := oneLine(long, markerMaxReason)
	if strings.Contains(line, "\n") {
		t.Fatal("oneLine left a newline in the reason")
	}
	if len(line) > markerMaxReason+len("…") {
		t.Fatalf("oneLine kept %d bytes, limit is %d", len(line), markerMaxReason)
	}
	if !strings.HasSuffix(line, "…") {
		t.Error("a truncated reason must be visibly truncated, or it reads as the whole story")
	}

	// And the marker's body names the run's failure, with the counts attributed
	// to rsync's file list rather than to the snapshot.
	reason := incompleteReasonText("Number of files: 4\nTotal file size: 12 bytes\n", errString("rsync exit 23"))
	if !strings.Contains(reason, "run failed: rsync exit 23") {
		t.Errorf("marker body does not carry the failure: %s", reason)
	}
	if !strings.Contains(reason, "files in rsync's file list: 4") {
		t.Errorf("marker body does not attribute the counts to rsync: %s", reason)
	}
}

type errString string

func (e errString) Error() string { return string(e) }
