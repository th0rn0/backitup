package lifecycle

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/th0rn0/backitup/internal/model"
)

// installCopyFailingRclone puts a stub `rclone` first on PATH that records every
// invocation and fails `copyto` with the given lines on stderr (everything else
// succeeds), so a test can see both what the worker asked for and what the run
// records when the destination refuses it.
func installCopyFailingRclone(t *testing.T, stderr string) (calls string) {
	t.Helper()
	dir := t.TempDir()
	calls = filepath.Join(dir, "rclone-calls.log")
	script := "#!/bin/sh\n" +
		"printf '%s\\n' \"$*\" >> " + calls + "\n" +
		"case \" $* \" in\n" +
		"  *\" copyto \"*)\n" +
		"    cat >&2 <<'RCLONE_FAILURE'\n" + stderr + "\nRCLONE_FAILURE\n" +
		"    exit 1\n" +
		"    ;;\n" +
		"esac\n" +
		"exit 0\n"
	if err := os.WriteFile(filepath.Join(dir, "rclone"), []byte(script), 0o755); err != nil {
		t.Fatalf("install fake rclone: %v", err)
	}
	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	return calls
}

// The reported failure: a client whose slug matches its bucket, on a remote
// whose "path" was also filled with the bucket name — so the destination read
// "<bucket>/<bucket>/<snapshot>". The object path must name the bucket once.
func TestOffsiteObjectPathNamesTheBucketOnce(t *testing.T) {
	calls := installRecordingRclone(t)
	st := openStore(t)
	now := time.Date(2026, 11, 23, 22, 20, 33, 0, time.UTC)

	if err := st.CreateRemote(t.Context(), model.Remote{
		Name: "tehHulk-source", Backend: model.BackendB2,
		Config: map[string]string{
			"account": "004appkeyid", "key": "K004applicationkey",
			"bucket": "tehhulk-source", "path": "tehhulk-source",
		},
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("create remote: %v", err)
	}
	_, base, dir := newClient(t, st, model.Client{
		Name: "tehhulk-source", Mode: model.ModeTarGz, OffsiteRemote: "tehHulk-source",
		RetentionDays: 14, Enabled: true,
	})
	const snap = "backup-20261123T222033Z-000.tar.gz"
	archive := filepath.Join(dir, snap)
	mkArchive(t, archive, now.Add(-time.Hour))

	deps := Deps{
		Store:         st,
		Offsite:       NewRclone(filepath.Join(t.TempDir(), "rclone.conf")),
		BackupBaseDir: base,
		Now:           func() time.Time { return now },
	}
	if err := RunOffsiteOnce(context.Background(), deps); err != nil {
		t.Fatalf("RunOffsiteOnce: %v", err)
	}

	log := rcloneCalls(t, calls)
	want := "tehHulk-source:tehhulk-source/" + snap
	if dst := copyDestination(log); dst != want {
		t.Fatalf("the object path does not address the bucket once:\n got: %s\nwant: %s\nfull rclone log:\n%s", dst, want, log)
	}
}

// copyDestination returns the destination argument of the first copyto in an
// rclone invocation log.
func copyDestination(log string) string {
	for _, line := range strings.Split(log, "\n") {
		i := strings.Index(line, " copyto ")
		if i < 0 {
			continue
		}
		fields := strings.Fields(line[i+len(" copyto "):])
		if len(fields) == 2 {
			return fields[1]
		}
	}
	return ""
}

// `duplicate_bucket_name` is B2 saying "that name exists and it is not yours to
// create": rclone tried to create the bucket because the key cannot see it. The
// run's error must say that, name the bucket, and keep rclone's raw output out
// of it — and the upload must be attempted once, not three times.
func TestOffsiteUploadDuplicateBucketIsLegibleAndTriedOnce(t *testing.T) {
	calls := installCopyFailingRclone(t,
		"ERROR : backup-20261123T222033Z-000.tar.gz: Failed to copy: multi-thread copy: failed to open chunk writer:\n"+
			"        failed to create bucket: Bucket name is already in use (400 duplicate_bucket_name)")
	st := openStore(t)
	now := time.Date(2026, 11, 23, 22, 20, 33, 0, time.UTC)

	if err := st.CreateRemote(t.Context(), model.Remote{
		Name: "tehHulk-source", Backend: model.BackendB2,
		Config: map[string]string{
			"account": "004appkeyid", "key": "K004applicationkey", "bucket": "tehhulk-source",
		},
		CreatedAt: now,
	}); err != nil {
		t.Fatalf("create remote: %v", err)
	}
	_, base, dir := newClient(t, st, model.Client{
		Name: "tehhulk-source", Mode: model.ModeTarGz, OffsiteRemote: "tehHulk-source",
		RetentionDays: 14, Enabled: true,
	})
	archive := filepath.Join(dir, "backup-20261123T222033Z-000.tar.gz")
	mkArchive(t, archive, now.Add(-time.Hour))

	deps := Deps{
		Store:         st,
		Offsite:       NewRclone(filepath.Join(t.TempDir(), "rclone.conf")),
		BackupBaseDir: base,
		Now:           func() time.Time { return now },
	}
	err := RunOffsiteOnce(context.Background(), deps)
	if err == nil {
		t.Fatal("an upload B2 refused with duplicate_bucket_name was reported as a successful pass")
	}
	msg := err.Error()
	for _, want := range []string{
		`bucket "tehhulk-source"`,     // which bucket it used
		"could not see",               // the cause: the key cannot see it
		"tried to create it",          // what rclone did about it
		"create a B2 application key", // the fix, because keys are immutable
	} {
		if !strings.Contains(msg, want) {
			t.Errorf("the run's error does not say %q:\n%s", want, msg)
		}
	}
	// The raw rclone output stays in the log; the run's error is the explanation.
	if strings.Contains(msg, "multi-thread copy") || strings.Contains(msg, "Failed to copy") {
		t.Errorf("the run's error quotes rclone's raw output:\n%s", msg)
	}

	log := rcloneCalls(t, calls)
	if n := strings.Count(log, "copyto "); n != 1 {
		t.Fatalf("the upload was attempted %d times, want once:\n%s", n, log)
	}
	if !strings.Contains(log, "--retries 1 copyto ") {
		t.Fatalf("rclone was left free to retry a deterministic refusal; invocation:\n%s", log)
	}
}
